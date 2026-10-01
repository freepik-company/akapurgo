package api

import (
	"akapurgo/api/v1alpha1"
	"akapurgo/internal/config"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"
)

type confirmationTransport func(*http.Request) (*http.Response, error)

func (f confirmationTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPurgeRequiresCurrentConfirmation(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		scope       string
		id          string
		duplicate   bool
		duplicateID bool
		custom      bool
		success     bool
	}{
		{name: "confirmed deletion", status: 200, scope: "complete", id: "echo", success: true},
		{name: "confirmed absence", status: 204, scope: "complete", id: "echo", success: true},
		{name: "ordinary image", status: 200},
		{name: "unconfirmed absence", status: 204},
		{name: "cached confirmation", status: 200, scope: "complete", id: strings.Repeat("0", 32)},
		{name: "incomplete purge", status: 200, scope: "partial", id: "echo"},
		{name: "missing challenge", status: 200, scope: "complete"},
		{name: "duplicate confirmation", status: 200, scope: "complete", id: "echo", duplicate: true},
		{name: "legacy missing", status: 404, scope: "complete", id: "echo"},
		{name: "legacy precondition", status: 412, scope: "complete", id: "echo"},
		{name: "partial content", status: 206, scope: "complete", id: "echo"},
		{name: "rejected at edge", status: 403},
		{name: "duplicate request ID", status: 200, scope: "complete", id: "echo", duplicateID: true},
		{name: "custom protocol", status: 200, scope: "evicted", id: "echo", custom: true, success: true},
		{name: "custom protocol wrong value", status: 200, scope: "complete", id: "echo", custom: true},
		{name: "custom protocol stale ID", status: 204, scope: "evicted", id: strings.Repeat("0", 32), custom: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &v1alpha1.ConfigSpec{}
			if tc.custom {
				parsed, err := config.Unmarshal([]byte(`post_purge_request:
  confirmation:
    request_id_header: "x-example-request-id"
    response_header: "x-example-purge-status"
    response_value: "evicted"
`))
				if err != nil {
					t.Fatal(err)
				}
				cfg = &parsed
			}
			confirmation, err := buildOriginPurgeConfirmation(cfg.PostPurgeRequest.Confirmation)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Akamai.Host = "https://ccu.example.test"
			cfg.PostPurgeRequest.Enabled = true
			cfg.PostPurgeRequest.AllowedHosts = []string{"cdn.example.test"}
			// A configured fixed ID must never replace the per-attempt challenge.
			cfg.PostPurgeRequest.Headers = map[string]string{confirmation.RequestIDHeader: "configured-id"}
			ctx := v1alpha1.Context{Config: cfg, Logger: zap.NewNop().Sugar()}
			var order []string
			origin := &http.Client{Transport: confirmationTransport(func(r *http.Request) (*http.Response, error) {
				order = append(order, "origin")
				id := r.Header.Get(confirmation.RequestIDHeader)
				if !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(id) {
					t.Fatalf("invalid challenge %q", id)
				}
				h := http.Header{}
				if tc.scope != "" {
					h.Set(confirmation.ResponseHeader, tc.scope)
				}
				if tc.id == "echo" {
					h.Set(confirmation.RequestIDHeader, id)
				} else if tc.id != "" {
					h.Set(confirmation.RequestIDHeader, tc.id)
				}
				if tc.custom && r.Header.Get(originPurgeRequestIDHeader) != "" {
					t.Fatal("default request ID header was sent despite custom configuration")
				}
				if tc.duplicateID {
					h.Add(confirmation.RequestIDHeader, id)
				}
				if tc.duplicate {
					h.Add(confirmation.ResponseHeader, tc.scope)
				}
				return &http.Response{StatusCode: tc.status, Header: h, Body: io.NopCloser(strings.NewReader("image-or-purge-body"))}, nil
			})}
			ccu := &http.Client{Transport: confirmationTransport(func(r *http.Request) (*http.Response, error) {
				order = append(order, "ccu")
				if r.Method != http.MethodPost {
					t.Fatalf("CCU method: %s", r.Method)
				}
				return &http.Response{StatusCode: 201, Body: io.NopCloser(strings.NewReader(`{"httpStatus":201,"detail":"accepted"}`))}, nil
			})}
			app := fiber.New()
			app.Post("/purge", purgeHandler(ctx, origin, ccu, func(*http.Request) error { return nil }))
			req := httptest.NewRequest("POST", "/purge", strings.NewReader(`{"purgeType":"urls","actionType":"invalidate","environment":"production","originPurgeRequest":true,"paths":["https://cdn.example.test/image.jpg"]}`))
			req.Header.Set("Content-Type", "application/json")
			resp, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			wantStatus := 502
			if tc.success {
				wantStatus = 201
			}
			if resp.StatusCode != wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, wantStatus)
			}
			if strings.Join(order, ",") != "origin,ccu" {
				t.Fatalf("call order: %v", order)
			}
			var body struct {
				HTTPStatus int `json:"httpStatus"`
				Origin     struct {
					Status   string               `json:"status"`
					Failures []originPurgeFailure `json:"failures"`
				} `json:"origin"`
				Akamai struct {
					Status     string `json:"status"`
					HTTPStatus int    `json:"httpStatus"`
				} `json:"akamai"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if tc.success {
				if body.HTTPStatus != 201 {
					t.Fatalf("legacy successful CCU response changed: %+v", body)
				}
			} else if body.Origin.Status != "failed" || len(body.Origin.Failures) != 1 || body.Origin.Failures[0].HTTPStatus != tc.status || body.Akamai.Status != "accepted" || body.Akamai.HTTPStatus != 201 {
				t.Fatalf("incorrect partial failure: %+v", body)
			}
		})
	}
}

func TestOriginPurgeRejectsConfirmationFromPreviousURL(t *testing.T) {
	cfg := &v1alpha1.ConfigSpec{}
	ctx := v1alpha1.Context{Config: cfg, Logger: zap.NewNop().Sugar()}
	urls, err := validateOriginPurgeURLs([]string{"https://cdn.example.test/a", "https://cdn.example.test/b"}, map[string]struct{}{"cdn.example.test": {}})
	if err != nil {
		t.Fatal(err)
	}
	confirmation, err := buildOriginPurgeConfirmation(cfg.PostPurgeRequest.Confirmation)
	if err != nil {
		t.Fatal(err)
	}
	var firstID string
	calls := 0
	client := &http.Client{Transport: confirmationTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		id := r.Header.Get(confirmation.RequestIDHeader)
		if calls == 1 {
			firstID = id
		} else if id == firstID {
			t.Fatal("challenge reused")
		}
		h := http.Header{}
		h.Set(confirmation.RequestIDHeader, firstID)
		h.Set(confirmation.ResponseHeader, "complete")
		return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	failures := executeOriginPurgeRequest(context.Background(), urls, ctx, client, confirmation)
	if len(failures) != 1 || failures[0].Index != 1 {
		t.Fatalf("unexpected failures: %+v", failures)
	}
}

func TestInvalidConfirmationConfigSendsNoRequests(t *testing.T) {
	tests := []struct {
		name   string
		config v1alpha1.OriginPurgeConfirmationSpec
	}{
		{"invalid request header", v1alpha1.OriginPurgeConfirmationSpec{RequestIDHeader: "Bad Header"}},
		{"invalid response header", v1alpha1.OriginPurgeConfirmationSpec{ResponseHeader: "Bad:Header"}},
		{"header injection", v1alpha1.OriginPurgeConfirmationSpec{RequestIDHeader: "X-ID\r\nX-Other"}},
		{"same header", v1alpha1.OriginPurgeConfirmationSpec{RequestIDHeader: "X-Result", ResponseHeader: "x-result"}},
		{"whitespace only", v1alpha1.OriginPurgeConfirmationSpec{ResponseValue: " "}},
		{"surrounding whitespace", v1alpha1.OriginPurgeConfirmationSpec{ResponseValue: " complete"}},
		{"value injection", v1alpha1.OriginPurgeConfirmationSpec{ResponseValue: "complete\r\nX-Other: value"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &v1alpha1.ConfigSpec{}
			cfg.Akamai.Host = "https://ccu.example.test"
			cfg.PostPurgeRequest.Enabled = true
			cfg.PostPurgeRequest.AllowedHosts = []string{"cdn.example.test"}
			cfg.PostPurgeRequest.Confirmation = tc.config
			ctx := v1alpha1.Context{Config: cfg, Logger: zap.NewNop().Sugar()}
			calls := 0
			client := &http.Client{Transport: confirmationTransport(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 201, Body: io.NopCloser(strings.NewReader(`{"httpStatus":201}`))}, nil
			})}
			app := fiber.New()
			app.Post("/purge", purgeHandler(ctx, client, client, func(*http.Request) error { return nil }))
			req := httptest.NewRequest("POST", "/purge", strings.NewReader(`{"purgeType":"urls","actionType":"invalidate","environment":"production","originPurgeRequest":true,"paths":["https://cdn.example.test/image.jpg"]}`))
			req.Header.Set("Content-Type", "application/json")
			resp, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != 500 || calls != 0 {
				t.Fatalf("status = %d, outbound requests = %d; want 500 without requests", resp.StatusCode, calls)
			}
		})
	}
}
