package origincache

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"akapurgo/api/v1alpha1"
)

const (
	defaultTimeout      = 10 * time.Second
	defaultTotalTimeout = 30 * time.Second
	maxBodySize         = 1 << 20
)

// MaxEntries bounds the number of outbound requests a single API call can fan out.
const MaxEntries = 100

type Config struct {
	Endpoints             []string
	Token                 string
	Timeout               time.Duration
	TotalTimeout          time.Duration
	InsecureSkipTLSVerify bool
}

type Client struct {
	config Config
	client *http.Client
}

func NewClient(config Config) (*Client, error) {
	if len(config.Endpoints) == 0 {
		return nil, errors.New("origin cache purge requires at least one endpoint")
	}
	if config.Token == "" {
		return nil, errors.New("origin cache purge token is empty")
	}

	for _, endpoint := range config.Endpoints {
		parsed, err := url.ParseRequestURI(endpoint)
		if err != nil {
			return nil, fmt.Errorf("invalid origin cache purge endpoint %q: %w", endpoint, err)
		}
		if parsed.Scheme != "https" {
			return nil, fmt.Errorf("origin cache purge endpoint %q must use HTTPS", endpoint)
		}
		if parsed.Path != "/_cache_purge" {
			return nil, fmt.Errorf("origin cache purge endpoint %q must end in /_cache_purge", endpoint)
		}
	}

	timeout := config.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	if config.TotalTimeout <= 0 {
		config.TotalTimeout = defaultTotalTimeout
	}

	baseTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		baseTransport = &http.Transport{}
	}
	transport := baseTransport.Clone()
	transport.TLSClientConfig = &tls.Config{ //nolint:gosec // Explicit opt-in is needed for direct-IP endpoints with hostname certificates.
		InsecureSkipVerify: config.InsecureSkipTLSVerify,
	}

	return &Client{
		config: config,
		client: &http.Client{
			Timeout:   timeout,
			Transport: transport,
		},
	}, nil
}

func (c *Client) Purge(ctx context.Context, entries []v1alpha1.OriginCachePurgeRequest) error {
	if len(entries) > MaxEntries {
		return fmt.Errorf("origin cache purge has %d entries, maximum is %d", len(entries), MaxEntries)
	}

	ctx, cancel := context.WithTimeout(ctx, c.config.TotalTimeout)
	defer cancel()

	var purgeErrors []error

	for entryIndex, entry := range entries {
		if err := ctx.Err(); err != nil {
			purgeErrors = append(purgeErrors, err)
			break
		}
		if err := validateEntry(entry); err != nil {
			purgeErrors = append(purgeErrors, fmt.Errorf("invalid origin cache purge entry %d: %w", entryIndex, err))
			continue
		}

		for _, endpoint := range c.config.Endpoints {
			if err := c.purgeOne(ctx, endpoint, entry); err != nil {
				purgeErrors = append(purgeErrors, err)
			}
		}
	}

	return errors.Join(purgeErrors...)
}

func (c *Client) purgeOne(ctx context.Context, endpoint string, entry v1alpha1.OriginCachePurgeRequest) error {
	// The nginx cache-purge module is exposed as GET because the existing
	// server guard only permits GET, HEAD and OPTIONS. The endpoint is direct,
	// authenticated and never traverses a caching proxy.
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("create origin cache purge request for %s: %w", endpoint, err)
	}

	request.Header.Set("X-FC-Token-Auth", c.config.Token)
	request.Header.Set("X-Bucket-OVH", entry.BucketOVH)
	request.Header.Set("X-Path-OVH", entry.PathOVH)
	request.Header.Set("X-Bucket-GCS", entry.BucketGCS)
	request.Header.Set("X-Path-GCS", entry.PathGCS)

	response, err := c.client.Do(request)
	if err != nil {
		return fmt.Errorf("purge origin cache at %s: %w", endpoint, err)
	}
	defer response.Body.Close()

	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxBodySize))

	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("purge origin cache at %s returned status %d", endpoint, response.StatusCode)
	}

	return nil
}

func validateEntry(entry v1alpha1.OriginCachePurgeRequest) error {
	switch {
	case strings.TrimSpace(entry.BucketOVH) == "":
		return errors.New("bucketOvh is empty")
	case strings.TrimSpace(entry.PathOVH) == "":
		return errors.New("pathOvh is empty")
	case strings.TrimSpace(entry.BucketGCS) == "":
		return errors.New("bucketGcs is empty")
	case strings.TrimSpace(entry.PathGCS) == "":
		return errors.New("pathGcs is empty")
	case containsNewline(entry.BucketOVH) || containsNewline(entry.PathOVH) ||
		containsNewline(entry.BucketGCS) || containsNewline(entry.PathGCS):
		return errors.New("bucket and path values cannot contain newlines")
	case !strings.HasPrefix(entry.PathOVH, "/"):
		return errors.New("pathOvh must start with /")
	case !strings.HasPrefix(entry.PathGCS, "/"):
		return errors.New("pathGcs must start with /")
	default:
		return nil
	}
}

func containsNewline(value string) bool {
	return strings.ContainsAny(value, "\r\n")
}
