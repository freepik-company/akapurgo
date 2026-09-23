package origincache

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"akapurgo/api/v1alpha1"
)

func TestPurgeCallsEveryEndpointWithRequiredHeaders(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)

		expectedHeaders := map[string]string{
			"X-FC-Token-Auth": "secret-token",
			"X-Bucket-OVH":    "fc-gra-fp-2000",
			"X-Path-OVH":      "/52683/180/179253.jpg",
			"X-Bucket-GCS":    "fc-europe-west1-fp",
			"X-Path-GCS":      "/2000/52683/180/179253.jpg",
		}
		for header, expected := range expectedHeaders {
			if actual := request.Header.Get(header); actual != expected {
				t.Errorf("%s = %q, want %q", header, actual, expected)
			}
		}

		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := newTestClient(t, []string{
		server.URL + "/_cache_purge",
		server.URL + "/_cache_purge",
		server.URL + "/_cache_purge",
	})

	err := client.Purge(context.Background(), []v1alpha1.OriginCachePurgeRequest{validEntry()})
	if err != nil {
		t.Fatalf("Purge() error = %v", err)
	}
	if actual := calls.Load(); actual != 3 {
		t.Fatalf("calls = %d, want 3", actual)
	}
}

func TestPurgeAttemptsEveryEndpointAndReturnsFailure(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(writer, "failed", http.StatusInternalServerError)
			return
		}
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := newTestClient(t, []string{
		server.URL + "/_cache_purge",
		server.URL + "/_cache_purge",
	})

	err := client.Purge(context.Background(), []v1alpha1.OriginCachePurgeRequest{validEntry()})
	if err == nil || !strings.Contains(err.Error(), "returned status 500") {
		t.Fatalf("Purge() error = %v, want status 500", err)
	}
	if actual := calls.Load(); actual != 2 {
		t.Fatalf("calls = %d, want 2", actual)
	}
}

func TestPurgeRejectsInvalidEntryWithoutSendingRequest(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := newTestClient(t, []string{server.URL + "/_cache_purge"})
	entry := validEntry()
	entry.PathOVH = "missing-leading-slash"

	err := client.Purge(context.Background(), []v1alpha1.OriginCachePurgeRequest{entry})
	if err == nil || !strings.Contains(err.Error(), "pathOvh must start with /") {
		t.Fatalf("Purge() error = %v, want path validation error", err)
	}
	if actual := calls.Load(); actual != 0 {
		t.Fatalf("calls = %d, want 0", actual)
	}
}

func TestPurgeRejectsHeaderNewlinesWithoutSendingRequest(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := newTestClient(t, []string{server.URL + "/_cache_purge"})
	entry := validEntry()
	entry.PathOVH = "/image.jpg\r\nX-Injected: true"

	err := client.Purge(context.Background(), []v1alpha1.OriginCachePurgeRequest{entry})
	if err == nil || !strings.Contains(err.Error(), "cannot contain newlines") {
		t.Fatalf("Purge() error = %v, want newline validation error", err)
	}
	if actual := calls.Load(); actual != 0 {
		t.Fatalf("calls = %d, want 0", actual)
	}
}

func TestPurgeHonorsTimeout(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client, err := NewClient(Config{
		Endpoints:             []string{server.URL + "/_cache_purge"},
		Token:                 "secret-token",
		Timeout:               10 * time.Millisecond,
		InsecureSkipTLSVerify: true,
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	err = client.Purge(context.Background(), []v1alpha1.OriginCachePurgeRequest{validEntry()})
	var urlError *url.Error
	if err == nil || !errors.As(err, &urlError) || !urlError.Timeout() {
		t.Fatalf("Purge() error = %v, want timeout", err)
	}
}

func TestPurgeHonorsTotalTimeoutAcrossEndpoints(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client, err := NewClient(Config{
		Endpoints: []string{
			server.URL + "/_cache_purge",
			server.URL + "/_cache_purge",
			server.URL + "/_cache_purge",
		},
		Token:                 "secret-token",
		Timeout:               time.Second,
		TotalTimeout:          25 * time.Millisecond,
		InsecureSkipTLSVerify: true,
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	err = client.Purge(context.Background(), []v1alpha1.OriginCachePurgeRequest{validEntry()})
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Purge() error = %v, want context deadline exceeded", err)
	}
	if actual := calls.Load(); actual >= 3 {
		t.Fatalf("calls = %d, want fewer than 3 after total timeout", actual)
	}
}

func TestPurgeRejectsTooManyEntries(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := newTestClient(t, []string{server.URL + "/_cache_purge"})
	entries := make([]v1alpha1.OriginCachePurgeRequest, MaxEntries+1)
	for index := range entries {
		entries[index] = validEntry()
	}

	err := client.Purge(context.Background(), entries)
	if err == nil || !strings.Contains(err.Error(), "maximum is") {
		t.Fatalf("Purge() error = %v, want maximum entries error", err)
	}
	if actual := calls.Load(); actual != 0 {
		t.Fatalf("calls = %d, want 0", actual)
	}
}

func TestNewClientValidatesConfiguration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		config Config
		want   string
	}{
		{
			name:   "missing endpoints",
			config: Config{Token: "secret-token"},
			want:   "at least one endpoint",
		},
		{
			name:   "missing token",
			config: Config{Endpoints: []string{"https://127.0.0.1/_cache_purge"}},
			want:   "token is empty",
		},
		{
			name: "plaintext endpoint",
			config: Config{
				Endpoints: []string{"http://127.0.0.1/_cache_purge"},
				Token:     "secret-token",
			},
			want: "must use HTTPS",
		},
		{
			name: "wrong path",
			config: Config{
				Endpoints: []string{"https://127.0.0.1/purge"},
				Token:     "secret-token",
			},
			want: "must end in /_cache_purge",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := NewClient(test.config)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("NewClient() error = %v, want %q", err, test.want)
			}
		})
	}
}

func newTestClient(t *testing.T, endpoints []string) *Client {
	t.Helper()

	client, err := NewClient(Config{
		Endpoints:             endpoints,
		Token:                 "secret-token",
		InsecureSkipTLSVerify: true,
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	return client
}

func validEntry() v1alpha1.OriginCachePurgeRequest {
	return v1alpha1.OriginCachePurgeRequest{
		BucketOVH: "fc-gra-fp-2000",
		PathOVH:   "/52683/180/179253.jpg",
		BucketGCS: "fc-europe-west1-fp",
		PathGCS:   "/2000/52683/180/179253.jpg",
	}
}
