package api

import (
	"akapurgo/api/v1alpha1"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }

func testResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func testContext() v1alpha1.Context {
	config := &v1alpha1.ConfigSpec{}
	config.Akamai.Host = "https://ccu.example.com"
	config.PostPurgeRequest.Enabled = true
	config.PostPurgeRequest.AllowedHosts = []string{"img.example.com"}
	config.PostPurgeRequest.Headers = map[string]string{"X-OVH-Purge": "secret-token"}
	return v1alpha1.Context{Config: config, Logger: zap.NewNop().Sugar()}
}

func testPurge(t *testing.T, app *fiber.App, req v1alpha1.PurgeRequest) (int, string) {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	httpReq := httptest.NewRequest(http.MethodPost, "/api/v1/purge", strings.NewReader(string(body)))
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(httpReq)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(responseBody)
}

func testRequest() v1alpha1.PurgeRequest {
	return v1alpha1.PurgeRequest{
		PurgeType: "urls", ActionType: "delete", Environment: "production", OriginPurgeRequest: true,
		Paths: []string{"https://img.example.com/photo.jpg?token=private-query"},
	}
}

func TestOriginFailureStillSubmitsCCU(t *testing.T) {
	for _, tc := range []struct {
		name         string
		originStatus int
		originErr    error
		ccuStatus    int
		ccuBody      string
		ccuErr       error
		signErr      error
		wantCCU      string
	}{
		{name: "forbidden", originStatus: 403, ccuStatus: 201, ccuBody: `{"httpStatus":201,"detail":"Request accepted"}`, wantCCU: "accepted"},
		{name: "origin timeout", originErr: context.DeadlineExceeded, ccuStatus: 201, ccuBody: `{"httpStatus":201}`, wantCCU: "accepted"},
		{name: "origin unavailable", originStatus: 503, ccuStatus: 201, ccuBody: `{"httpStatus":201}`, wantCCU: "accepted"},
		{name: "CCU rejected", originStatus: 403, ccuStatus: 429, ccuBody: `{"httpStatus":429,"detail":"Too many requests"}`, wantCCU: "failed"},
		{name: "CCU timeout", originStatus: 403, ccuErr: context.DeadlineExceeded, wantCCU: "unknown"},
		{name: "invalid CCU response", originStatus: 403, ccuStatus: 201, ccuBody: "not JSON", wantCCU: "unknown"},
		{name: "cannot sign", originStatus: 403, signErr: errors.New("credentials unavailable"), wantCCU: "not_attempted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			origin := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls = append(calls, "origin")
				if req.Header.Get("X-OVH-Purge") != "secret-token" || req.Header.Get("User-Agent") != "akapurgo" {
					t.Error("origin request lost its purge headers")
				}
				if tc.originErr != nil {
					return nil, tc.originErr
				}
				return testResponse(tc.originStatus, ""), nil
			})}
			ccu := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls = append(calls, "ccu")
				if req.Method != http.MethodPost || req.URL.Path != "/ccu/v3/delete/url/production" {
					t.Errorf("unexpected CCU request: %s %s", req.Method, req.URL.Path)
				}
				if req.Header.Get("X-OVH-Purge") != "" {
					t.Error("origin token leaked into CCU")
				}
				var payload struct {
					Objects []string `json:"objects"`
				}
				if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(payload.Objects, testRequest().Paths) {
					t.Errorf("unexpected CCU objects: %v", payload.Objects)
				}
				if tc.ccuErr != nil {
					return nil, tc.ccuErr
				}
				return testResponse(tc.ccuStatus, tc.ccuBody), nil
			})}
			app := fiber.New()
			app.Post("/api/v1/purge", purgeHandler(testContext(), origin, ccu, func(*http.Request) error { return tc.signErr }))
			status, body := testPurge(t, app, testRequest())
			if status != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502: %s", status, body)
			}
			wantCalls := []string{"origin", "ccu"}
			if tc.signErr != nil {
				wantCalls = []string{"origin"}
			}
			if !reflect.DeepEqual(calls, wantCalls) {
				t.Errorf("calls = %v, want %v", calls, wantCalls)
			}
			var result struct {
				Error  string `json:"error"`
				Origin struct {
					Status   string               `json:"status"`
					Failures []originPurgeFailure `json:"failures"`
				} `json:"origin"`
				Akamai struct {
					Status     string `json:"status"`
					HTTPStatus int    `json:"httpStatus"`
				} `json:"akamai"`
			}
			if err := json.Unmarshal([]byte(body), &result); err != nil {
				t.Fatal(err)
			}
			if result.Error != "Failed to purge origin cache" || result.Origin.Status != "failed" || result.Akamai.Status != tc.wantCCU {
				t.Errorf("unexpected partial failure: %s", body)
			}
			if result.Akamai.HTTPStatus != tc.ccuStatus {
				t.Errorf("lost upstream CCU status: %s", body)
			}
			if len(result.Origin.Failures) != 1 || result.Origin.Failures[0].Host != "img.example.com" || result.Origin.Failures[0].HTTPStatus != tc.originStatus {
				t.Errorf("missing origin diagnostics: %s", body)
			}
			for _, secret := range []string{"secret-token", "private-query", "credentials unavailable"} {
				if strings.Contains(body, secret) {
					t.Errorf("response leaked %q", secret)
				}
			}
		})
	}
}

func TestPartialBatchCanBeRetried(t *testing.T) {
	var calls []string
	recovered := false
	origin := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls = append(calls, req.URL.Path)
		if !recovered && req.URL.Path == "/second.jpg" {
			return testResponse(http.StatusForbidden, ""), nil
		}
		return testResponse(http.StatusNoContent, ""), nil
	})}
	ccu := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls = append(calls, "ccu")
		var payload struct {
			Objects []string `json:"objects"`
		}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.Objects) != 6 {
			t.Errorf("CCU must include all URLs and variants: %v", payload.Objects)
		}
		return testResponse(201, `{"httpStatus":201,"detail":"Request accepted"}`), nil
	})}
	app := fiber.New()
	app.Post("/api/v1/purge", purgeHandler(testContext(), origin, ccu, func(*http.Request) error { return nil }))
	req := testRequest()
	req.ImBypass = true
	req.Paths = []string{"https://img.example.com/first.jpg", "https://img.example.com/second.jpg", "https://img.example.com/third.jpg"}
	status, body := testPurge(t, app, req)
	if status != 502 || !strings.Contains(body, `"index":1`) {
		t.Fatalf("missing batch failure: %d %s", status, body)
	}
	wantCalls := []string{"/first.jpg", "/second.jpg", "/third.jpg", "ccu"}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Errorf("batch calls = %v, want %v", calls, wantCalls)
	}
	recovered, calls = true, nil
	status, body = testPurge(t, app, req)
	if status != 201 || strings.Contains(body, `"origin"`) {
		t.Errorf("retry failed: %d %s", status, body)
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Errorf("retry skipped a stage: %v", calls)
	}
}

func TestOriginRedirectDoesNotForwardPurgeToken(t *testing.T) {
	client := newOriginPurgeHTTPClient(defaultOriginPurgeTimeout)
	client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "img.example.com" {
			t.Fatal("followed an origin redirect")
		}
		response := testResponse(http.StatusFound, "")
		response.Header.Set("Location", "https://elsewhere.example.com/receive-token")
		return response, nil
	})
	urls, err := validateOriginPurgeURLs(testRequest().Paths, map[string]struct{}{"img.example.com": {}})
	if err != nil {
		t.Fatal(err)
	}
	failures := executeOriginPurgeRequest(context.Background(), urls, testContext(), client)
	if len(failures) != 1 || failures[0].HTTPStatus != http.StatusFound {
		t.Errorf("redirect was not a failure: %v", failures)
	}
}

func TestInvalidRequestDoesNotPurge(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*v1alpha1.PurgeRequest)
	}{
		{"action", func(req *v1alpha1.PurgeRequest) { req.ActionType = "invalid" }},
		{"environment", func(req *v1alpha1.PurgeRequest) { req.Environment = "invalid" }},
		{"empty paths", func(req *v1alpha1.PurgeRequest) { req.Paths = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Error("invalid request reached upstream")
				return testResponse(201, `{"httpStatus":201}`), nil
			})}
			app := fiber.New()
			app.Post("/api/v1/purge", purgeHandler(testContext(), client, client, func(*http.Request) error { return nil }))
			req := testRequest()
			tc.change(&req)
			if status, body := testPurge(t, app, req); status != http.StatusBadRequest {
				t.Errorf("status = %d, body = %s", status, body)
			}
		})
	}
}

func TestInvalidOriginURLsHaveNoSideEffects(t *testing.T) {
	for _, paths := range [][]string{
		{"https://downloadscdn5.example.com/file.jpg"},
		{"https://img.example.com/valid.jpg", "https://elsewhere.example.com/file.jpg"},
		{"http://img.example.com/file.jpg"},
		{"https://user:password@img.example.com/file.jpg"},
		make([]string, maxOriginPurgeURLs+1),
	} {
		t.Run(fmt.Sprint(paths[0]), func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Error("invalid input caused an outgoing request")
				return testResponse(201, `{"httpStatus":201}`), nil
			})}
			app := fiber.New()
			app.Post("/api/v1/purge", purgeHandler(testContext(), client, client, func(*http.Request) error {
				t.Error("invalid input reached signing")
				return nil
			}))
			req := testRequest()
			req.Paths = paths
			status, body := testPurge(t, app, req)
			if status != http.StatusBadRequest {
				t.Errorf("status = %d, want 400: %s", status, body)
			}
		})
	}
}

func TestSuccessfulOriginAndLegacyRequests(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		status                     int
		legacy, bypass, skip, tags bool
	}{
		{name: "purged", status: 200}, {name: "absent", status: 204},
		{name: "legacy cache miss", status: 412}, {name: "legacy 404", status: 404},
		{name: "legacy request field", status: 204, legacy: true},
		{name: "image manager variants", status: 204, bypass: true},
		{name: "edge only", skip: true}, {name: "cache tags", tags: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			originCalls, ccuCalls := 0, 0
			origin := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				originCalls++
				if req.URL.Query().Has("imbypass") {
					t.Error("origin received a duplicate variant")
				}
				return testResponse(tc.status, ""), nil
			})}
			ccu := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				ccuCalls++
				var payload struct {
					Objects []string `json:"objects"`
				}
				if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
					t.Fatal(err)
				}
				want := 1
				if tc.bypass {
					want = 2
				}
				if len(payload.Objects) != want {
					t.Errorf("CCU object count = %d, want %d", len(payload.Objects), want)
				}
				return testResponse(201, `{"httpStatus":201,"detail":"Request accepted"}`), nil
			})}
			app := fiber.New()
			app.Post("/api/v1/purge", purgeHandler(testContext(), origin, ccu, func(*http.Request) error { return nil }))
			req := testRequest()
			req.ImBypass = tc.bypass
			if tc.legacy {
				req.OriginPurgeRequest, req.PostPurgeRequest = false, true
			}
			if tc.skip {
				req.OriginPurgeRequest = false
			}
			if tc.tags {
				req.PurgeType, req.Paths = "cache-tags", []string{"resource-123"}
			}
			status, body := testPurge(t, app, req)
			if status != 201 || body != `{"httpStatus":201,"detail":"Request accepted"}` {
				t.Errorf("changed legacy response: %d %s", status, body)
			}
			wantOrigin := 1
			if tc.skip || tc.tags {
				wantOrigin = 0
			}
			if originCalls != wantOrigin || ccuCalls != 1 {
				t.Errorf("origin calls = %d; CCU calls = %d", originCalls, ccuCalls)
			}
		})
	}
}
