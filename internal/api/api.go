package api

import (
	"akapurgo/api/v1alpha1"
	"akapurgo/internal/commons"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/akamai/AkamaiOPEN-edgegrid-golang/v9/pkg/edgegrid"
	"github.com/gofiber/fiber/v2"
)

// Akamai rejects requests carrying Go's default User-Agent
// ("Go-http-client/1.1") with a 403, so the post-purge request never reached
// the origin. Send our own unless the configuration overrides it.
const defaultPostPurgeUserAgent = "akapurgo"
const defaultOriginPurgeTimeout = 10 * time.Second
const akamaiRequestTimeout = 30 * time.Second
const maxResponseBodySize = 1 << 20
const maxOriginPurgeURLs = 100

func PurgeHandler(ctx v1alpha1.Context) func(c *fiber.Ctx) error {
	originPurgeTimeout := time.Duration(ctx.Config.PostPurgeRequest.TimeoutSeconds) * time.Second
	if originPurgeTimeout <= 0 {
		originPurgeTimeout = defaultOriginPurgeTimeout
	}
	allowedOriginHosts, allowedHostsError := buildAllowedHosts(ctx.Config.PostPurgeRequest.AllowedHosts)
	if ctx.Config.PostPurgeRequest.Enabled && allowedHostsError != nil {
		ctx.Logger.Errorf("Invalid origin purge host configuration: %v", allowedHostsError)
	}
	originPurgeClient := newOriginPurgeHTTPClient(originPurgeTimeout)
	akamaiClient := &http.Client{Timeout: akamaiRequestTimeout}

	return func(c *fiber.Ctx) error {

		// Verify the Content-Type header
		if c.Get("Content-Type") != "application/json" {
			ctx.Logger.Error("Invalid content type")
			return c.Status(fiber.StatusBadRequest).JSON(map[string]string{
				"error": "Invalid content type",
			})
		}

		// Verify body to be really a JSON
		if !json.Valid(c.Body()) {
			ctx.Logger.Error("Invalid JSON body")
			return c.Status(fiber.StatusBadRequest).JSON(map[string]string{
				"error": "Invalid JSON body",
			})
		}

		// Parse the JSON body from the request and validate the body
		var req v1alpha1.PurgeRequest
		if err := c.BodyParser(&req); err != nil {
			ctx.Logger.Errorf("Failed to parse request: %v\n", err)
			return c.Status(fiber.StatusBadRequest).JSON(map[string]string{
				"error": "Invalid request payload",
			})
		}

		originPaths := append([]string(nil), req.Paths...)

		// Duplicate URLs with imbypass=true query parameter if requested
		if req.ImBypass && req.PurgeType == "urls" {
			req.Paths = duplicatePathsWithImBypass(req.Paths)
		}

		// Determine the Akamai API URL
		var purgeURL string
		if req.PurgeType == "urls" {
			purgeURL = fmt.Sprintf("%s/ccu/v3/%s/url/%s", ctx.Config.Akamai.Host, req.ActionType, req.Environment)
		} else if req.PurgeType == "cache-tags" {
			purgeURL = fmt.Sprintf("%s/ccu/v3/%s/tag/%s", ctx.Config.Akamai.Host, req.ActionType, req.Environment)
		} else {
			ctx.Logger.Error("Invalid purge type")
			return c.Status(fiber.StatusBadRequest).JSON(map[string]string{
				"error": "Invalid purge type",
			})
		}

		// A request carrying X-OVH-Purge bypasses Akamai, lets the property
		// derive the storage headers from the public URL, selects the same
		// dd-gra node as regular traffic and rewrites the path to /purge/....
		// Do this before purging Akamai so it cannot refill from stale origin.
		originPurgeRequested := req.OriginPurgeRequest || req.PostPurgeRequest
		if originPurgeRequested && ctx.Config.PostPurgeRequest.Enabled && req.PurgeType == "urls" {
			if allowedHostsError != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(map[string]string{
					"error": "Invalid origin purge host configuration",
				})
			}
			if err := executeOriginPurgeRequest(
				c.UserContext(), originPaths, ctx, originPurgeClient, allowedOriginHosts,
			); err != nil {
				ctx.Logger.Errorf("Failed to purge origin cache through Akamai: %v", err)
				return c.Status(fiber.StatusBadGateway).JSON(map[string]string{
					"error": "Failed to purge origin cache",
				})
			}
		}

		// Create the payload for Akamai
		akamaiPayload := map[string]interface{}{
			"objects": req.Paths,
		}

		// Marshal the payload to JSON
		payloadBytes, err := json.Marshal(akamaiPayload)
		if err != nil {
			ctx.Logger.Errorf("Failed to marshal payload: %v\n", err)
			return c.Status(fiber.StatusInternalServerError).JSON(map[string]string{
				"error": "Failed to encode payload",
			})
		}

		// Create the HTTP request to Akamai
		apiRequest, err := http.NewRequestWithContext(
			c.UserContext(), http.MethodPost, purgeURL, bytes.NewReader(payloadBytes),
		)
		if err != nil {
			ctx.Logger.Errorf("Failed to create HTTP request: %v\n", err)
			return c.Status(fiber.StatusInternalServerError).JSON(map[string]string{
				"error": "Failed to create request",
			})
		}

		// Generate the Authorization header with the edgerc Akamai library and the configuration file
		// generated previously or loaded from the environment
		// https://github.com/akamai/AkamaiOPEN-edgegrid-golang
		edgerc, err := edgegrid.New(edgegrid.WithFile(commons.AkamaiConfigPath))
		if err != nil {
			ctx.Logger.Errorf("Failed to sign the request with given credentials: %v\n", err)
			return c.Status(fiber.StatusInternalServerError).JSON(map[string]string{
				"error": "Failed to sign the request with given credentials",
			})
		}
		edgerc.SignRequest(apiRequest)

		// Set required headers
		apiRequest.Header.Set("Content-Type", "application/json")

		// Send the request to Akamai
		resp, err := akamaiClient.Do(apiRequest)
		if err != nil {
			ctx.Logger.Errorf("Failed to send request to Akamai: %v\n", err)
			return c.Status(fiber.StatusInternalServerError).JSON(map[string]string{
				"error": "Failed to communicate with Akamai",
			})
		}

		defer resp.Body.Close()

		// Decode the Akamai response
		var akamaiResp v1alpha1.AkamaiResponse
		if err := json.NewDecoder(resp.Body).Decode(&akamaiResp); err != nil {
			ctx.Logger.Errorf("Failed to decode Akamai response: %v\n", err)
			return c.Status(fiber.StatusInternalServerError).JSON(map[string]string{
				"error": "Failed to decode Akamai response",
			})
		}

		// Forward the Akamai response to the client
		ctx.Logger.Infof(`akamai-response,detail='%s',status=%d`, akamaiResp.Detail, akamaiResp.HTTPStatus)
		return c.Status(resp.StatusCode).JSON(akamaiResp)
	}
}

func executeOriginPurgeRequest(
	requestContext context.Context,
	paths []string,
	ctx v1alpha1.Context,
	client *http.Client,
	allowedHosts map[string]struct{},
) error {
	if len(paths) > maxOriginPurgeURLs {
		return fmt.Errorf("origin purge supports at most %d URLs", maxOriginPurgeURLs)
	}

	validatedURLs := make([]*url.URL, 0, len(paths))
	for index, path := range paths {
		validatedURL, err := validateOriginPurgeURL(path, allowedHosts)
		if err != nil {
			return fmt.Errorf("invalid origin purge URL at index %d: %w", index, err)
		}
		validatedURLs = append(validatedURLs, validatedURL)
	}

	var requestErrors []error

	for index, validatedURL := range validatedURLs {
		getRequest, err := http.NewRequestWithContext(
			requestContext, http.MethodGet, validatedURL.String(), nil,
		)
		if err != nil {
			requestErrors = append(requestErrors,
				sanitizeOriginPurgeRequestError(index, validatedURL, err))
			continue
		}

		// Identify ourselves before applying the configured headers, so those
		// can still override the User-Agent when needed.
		userAgent := ctx.Config.PostPurgeRequest.UserAgent
		if userAgent == "" {
			userAgent = defaultPostPurgeUserAgent
		}
		getRequest.Header.Set("User-Agent", userAgent)

		// Add custom headers from configuration
		for key, value := range ctx.Config.PostPurgeRequest.Headers {
			getRequest.Header.Set(key, value)
		}

		// Send the GET request
		response, err := client.Do(getRequest)
		if err != nil {
			requestErrors = append(requestErrors,
				sanitizeOriginPurgeRequestError(index, validatedURL, err))
			continue
		}

		_, readErr := io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBodySize))
		response.Body.Close()
		if readErr != nil {
			requestErrors = append(requestErrors, fmt.Errorf("read origin purge response %d: %w", index, readErr))
			continue
		}

		if !isSuccessfulOriginPurgeStatus(response.StatusCode) {
			requestErrors = append(requestErrors,
				fmt.Errorf("origin purge request %d returned status %d", index, response.StatusCode))
			continue
		}

		ctx.Logger.Infof("Origin purge request to %s%s returned status code %d",
			validatedURL.Host, validatedURL.EscapedPath(), response.StatusCode)
	}

	return errors.Join(requestErrors...)
}

func buildAllowedHosts(hosts []string) (map[string]struct{}, error) {
	if len(hosts) == 0 {
		return nil, errors.New("post_purge_request.allowed_hosts must not be empty")
	}

	allowedHosts := make(map[string]struct{}, len(hosts))
	for _, host := range hosts {
		normalizedHost := strings.ToLower(strings.TrimSpace(host))
		if normalizedHost == "" || strings.ContainsAny(normalizedHost, "/@\r\n") {
			return nil, fmt.Errorf("invalid allowed host")
		}
		allowedHosts[normalizedHost] = struct{}{}
	}
	return allowedHosts, nil
}

func newOriginPurgeHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func validateOriginPurgeURL(rawURL string, allowedHosts map[string]struct{}) (*url.URL, error) {
	parsedURL, err := url.ParseRequestURI(rawURL)
	if err != nil {
		return nil, errors.New("invalid URL")
	}
	if parsedURL.Scheme != "https" || parsedURL.Host == "" {
		return nil, errors.New("URL must be absolute and use HTTPS")
	}
	if parsedURL.User != nil {
		return nil, errors.New("URL credentials are not allowed")
	}
	if _, allowed := allowedHosts[strings.ToLower(parsedURL.Host)]; !allowed {
		return nil, errors.New("URL host is not allowed")
	}
	return parsedURL, nil
}

func isSuccessfulOriginPurgeStatus(status int) bool {
	// ngx_cache_purge returns 404 or 412, depending on its version, when the
	// entry is already absent. Purging is idempotent, so both are successful.
	return status >= 200 && status < 300 ||
		status == http.StatusNotFound ||
		status == http.StatusPreconditionFailed
}

func sanitizeOriginPurgeRequestError(index int, requestURL *url.URL, err error) error {
	var urlError *url.Error
	if errors.As(err, &urlError) {
		err = urlError.Err
	}

	return fmt.Errorf("send origin purge request %d to %s%s: %v",
		index, requestURL.Host, requestURL.EscapedPath(), err)
}

func duplicatePathsWithImBypass(paths []string) []string {
	duplicatedPaths := make([]string, 0, len(paths)*2)

	for _, path := range paths {
		// Add original URL
		duplicatedPaths = append(duplicatedPaths, path)

		// Add URL with imbypass=true query parameter
		duplicatedPaths = append(duplicatedPaths, addQueryParam(path, "imbypass", "true"))
	}

	return duplicatedPaths
}

func addQueryParam(urlStr, key, value string) string {
	separator := "?"
	if bytes.Contains([]byte(urlStr), []byte("?")) {
		separator = "&"
	}
	return fmt.Sprintf("%s%s%s=%s", urlStr, separator, key, value)
}
