package api

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

	"go.uber.org/zap"
)

func TestExecuteOriginPurgeRequestUsesConfiguredHeaders(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if actual := request.Header.Get("User-Agent"); actual != "akapurgo-test" {
			t.Errorf("User-Agent = %q, want akapurgo-test", actual)
		}
		if actual := request.Header.Get("X-OVH-Purge"); actual != "purge-token" {
			t.Errorf("X-OVH-Purge = %q, want purge-token", actual)
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	ctx := testContext()
	ctx.Config.PostPurgeRequest.UserAgent = "akapurgo-test"
	ctx.Config.PostPurgeRequest.Headers = map[string]string{
		"X-OVH-Purge": "purge-token",
	}

	err := executeOriginPurgeRequest(
		context.Background(),
		[]string{server.URL + "/image.jpg"},
		ctx,
		server.Client(),
		allowedTestHosts(server.URL),
	)
	if err != nil {
		t.Fatalf("executeOriginPurgeRequest() error = %v", err)
	}
}

func TestExecuteOriginPurgeRequestAttemptsEveryURLAndReturnsErrors(t *testing.T) {
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

	err := executeOriginPurgeRequest(
		context.Background(),
		[]string{server.URL + "/first.jpg", server.URL + "/second.jpg"},
		testContext(),
		server.Client(),
		allowedTestHosts(server.URL),
	)
	if err == nil || !strings.Contains(err.Error(), "returned status 500") {
		t.Fatalf("executeOriginPurgeRequest() error = %v, want status 500", err)
	}
	if actual := calls.Load(); actual != 2 {
		t.Fatalf("calls = %d, want 2", actual)
	}
}

func TestExecuteOriginPurgeRequestHonorsClientTimeout(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := server.Client()
	client.Timeout = 10 * time.Millisecond
	err := executeOriginPurgeRequest(
		context.Background(),
		[]string{server.URL + "/image.jpg"},
		testContext(),
		client,
		allowedTestHosts(server.URL),
	)
	var urlError *url.Error
	if err == nil || !errors.As(err, &urlError) || !urlError.Timeout() {
		t.Fatalf("executeOriginPurgeRequest() error = %v, want timeout", err)
	}
}

func TestExecuteOriginPurgeRequestRejectsUnsafeURLs(t *testing.T) {
	t.Parallel()

	tests := []string{
		"http://img.example.com/image.jpg",
		"https://169.254.169.254/latest/meta-data",
		"https://user:password@img.example.com/image.jpg",
		"https://img.example.com:8443/image.jpg",
	}
	allowedHosts := map[string]struct{}{"img.example.com": {}}

	for _, rawURL := range tests {
		err := executeOriginPurgeRequest(
			context.Background(),
			[]string{rawURL},
			testContext(),
			newOriginPurgeHTTPClient(time.Second),
			allowedHosts,
		)
		if err == nil {
			t.Errorf("executeOriginPurgeRequest(%q) error = nil, want validation error", rawURL)
		}
	}
}

func TestExecuteOriginPurgeRequestDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()

	var destinationCalls atomic.Int32
	destination := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		destinationCalls.Add(1)
		writer.WriteHeader(http.StatusOK)
	}))
	defer destination.Close()

	source := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, destination.URL, http.StatusFound)
	}))
	defer source.Close()

	client := source.Client()
	client.CheckRedirect = newOriginPurgeHTTPClient(time.Second).CheckRedirect
	err := executeOriginPurgeRequest(
		context.Background(),
		[]string{source.URL + "/image.jpg"},
		testContext(),
		client,
		allowedTestHosts(source.URL),
	)
	if err == nil || !strings.Contains(err.Error(), "returned status 302") {
		t.Fatalf("executeOriginPurgeRequest() error = %v, want status 302", err)
	}
	if actual := destinationCalls.Load(); actual != 0 {
		t.Fatalf("destination calls = %d, want 0", actual)
	}
}

func testContext() v1alpha1.Context {
	return v1alpha1.Context{
		Config: &v1alpha1.ConfigSpec{},
		Logger: zap.NewNop().Sugar(),
	}
}

func allowedTestHosts(rawURL string) map[string]struct{} {
	parsedURL, _ := url.Parse(rawURL)
	return map[string]struct{}{parsedURL.Host: {}}
}
