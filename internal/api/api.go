package api

import (
	"akapurgo/api/v1alpha1"
	"akapurgo/internal/commons"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
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

// Identify origin purge requests; deployments can override the User-Agent
// to match their access policies.
const defaultPostPurgeUserAgent = "akapurgo"
const defaultOriginPurgeTimeout = 10 * time.Second
const akamaiRequestTimeout = 30 * time.Second
const maxResponseBodySize = 1 << 20
const maxOriginPurgeURLs = 100
const originPurgeRequestIDHeader = "X-Purge-Request-Id"
const originPurgeConfirmationHeader = "X-Origin-Purge"
const originPurgeConfirmationValue = "complete"

func PurgeHandler(ctx v1alpha1.Context) func(c *fiber.Ctx) error {
	originPurgeTimeout := time.Duration(ctx.Config.PostPurgeRequest.TimeoutSeconds) * time.Second
	if originPurgeTimeout <= 0 {
		originPurgeTimeout = defaultOriginPurgeTimeout
	}
	return purgeHandler(ctx, newOriginPurgeHTTPClient(originPurgeTimeout),
		&http.Client{Timeout: akamaiRequestTimeout}, signAkamaiRequest)
}

func signAkamaiRequest(request *http.Request) error {
	edgerc, err := edgegrid.New(edgegrid.WithFile(commons.AkamaiConfigPath))
	if err != nil {
		return err
	}
	edgerc.SignRequest(request)
	return nil
}

func purgeHandler(ctx v1alpha1.Context, originPurgeClient, akamaiClient *http.Client,
	signRequest func(*http.Request) error,
) func(c *fiber.Ctx) error {
	allowedOriginHosts, allowedHostsError := buildAllowedHosts(ctx.Config.PostPurgeRequest.AllowedHosts)
	confirmation, confirmationError := buildOriginPurgeConfirmation(ctx.Config.PostPurgeRequest.Confirmation)
	if ctx.Config.PostPurgeRequest.Enabled && allowedHostsError != nil {
		ctx.Logger.Errorf("Invalid origin purge host configuration: %v", allowedHostsError)
	}
	if ctx.Config.PostPurgeRequest.Enabled && confirmationError != nil {
		ctx.Logger.Errorf("Invalid origin purge confirmation configuration: %v", confirmationError)
	}

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
		if (req.ActionType != "invalidate" && req.ActionType != "delete") ||
			(req.Environment != "production" && req.Environment != "staging") || len(req.Paths) == 0 {
			return c.Status(fiber.StatusBadRequest).JSON(map[string]string{
				"error": "Invalid action, environment or empty paths",
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

		// Try the origin first to avoid refilling the edge from stale content.
		// If the attempt fails, still submit CCU as a best-effort eviction, but
		// keep the overall request failed so callers retry the origin as well.
		originPurgeRequested := req.OriginPurgeRequest || req.PostPurgeRequest
		var originErrors []originPurgeFailure
		if originPurgeRequested && ctx.Config.PostPurgeRequest.Enabled && req.PurgeType == "urls" {
			if allowedHostsError != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(map[string]string{
					"error": "Invalid origin purge host configuration",
				})
			}
			if confirmationError != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(map[string]string{
					"error": "Invalid origin purge confirmation configuration",
				})
			}
			validatedURLs, err := validateOriginPurgeURLs(originPaths, allowedOriginHosts)
			if err != nil {
				return c.Status(fiber.StatusBadRequest).JSON(map[string]string{
					"error": err.Error(),
				})
			}
			originErrors = executeOriginPurgeRequest(c.UserContext(), validatedURLs, ctx, originPurgeClient, confirmation)
			if len(originErrors) > 0 {
				ctx.Logger.Errorf("Failed to purge origin cache through Akamai: %v", originErrors)
			}
		}

		// Preserve the legacy response when the origin succeeds or is not requested.
		// Partial failures expose CCU's outcome without treating acceptance as completion.
		ccuHTTPStatus := 0
		respond := func(status int, body interface{}, ccuStatus string) error {
			if len(originErrors) > 0 {
				return c.Status(fiber.StatusBadGateway).JSON(map[string]interface{}{
					"error":  "Failed to purge origin cache",
					"origin": map[string]interface{}{"status": "failed", "failures": originErrors},
					"akamai": map[string]interface{}{"status": ccuStatus, "httpStatus": ccuHTTPStatus, "response": body},
				})
			}
			return c.Status(status).JSON(body)
		}

		// Create the payload for Akamai
		akamaiPayload := map[string]interface{}{
			"objects": req.Paths,
		}

		// Marshal the payload to JSON
		payloadBytes, err := json.Marshal(akamaiPayload)
		if err != nil {
			ctx.Logger.Errorf("Failed to marshal payload: %v\n", err)
			return respond(fiber.StatusInternalServerError, map[string]string{
				"error": "Failed to encode payload",
			}, "not_attempted")
		}

		// Create the HTTP request to Akamai
		apiRequest, err := http.NewRequestWithContext(
			c.UserContext(), http.MethodPost, purgeURL, bytes.NewReader(payloadBytes),
		)
		if err != nil {
			ctx.Logger.Errorf("Failed to create HTTP request: %v\n", err)
			return respond(fiber.StatusInternalServerError, map[string]string{
				"error": "Failed to create request",
			}, "not_attempted")
		}

		// Generate the Authorization header with the edgerc Akamai library and the configuration file
		// generated previously or loaded from the environment
		// https://github.com/akamai/AkamaiOPEN-edgegrid-golang
		if err := signRequest(apiRequest); err != nil {
			ctx.Logger.Errorf("Failed to sign the request with given credentials: %v\n", err)
			return respond(fiber.StatusInternalServerError, map[string]string{
				"error": "Failed to sign the request with given credentials",
			}, "not_attempted")
		}

		// Set required headers
		apiRequest.Header.Set("Content-Type", "application/json")

		// Send the request to Akamai
		resp, err := akamaiClient.Do(apiRequest)
		if err != nil {
			ctx.Logger.Errorf("Failed to send request to Akamai: %v\n", err)
			return respond(fiber.StatusInternalServerError, map[string]string{
				"error": "Failed to communicate with Akamai",
			}, "unknown")
		}

		defer resp.Body.Close()
		ccuHTTPStatus = resp.StatusCode

		// Decode the Akamai response
		var akamaiResp v1alpha1.AkamaiResponse
		if err := json.NewDecoder(resp.Body).Decode(&akamaiResp); err != nil {
			ctx.Logger.Errorf("Failed to decode Akamai response: %v\n", err)
			return respond(fiber.StatusInternalServerError, map[string]string{
				"error": "Failed to decode Akamai response",
			}, "unknown")
		}

		// Forward the Akamai response to the client
		ctx.Logger.Infof(`akamai-response,detail='%s',status=%d`, akamaiResp.Detail, akamaiResp.HTTPStatus)
		ccuStatus := "failed"
		if resp.StatusCode == http.StatusCreated && akamaiResp.HTTPStatus == http.StatusCreated {
			ccuStatus = "accepted"
		}
		return respond(resp.StatusCode, akamaiResp, ccuStatus)
	}
}

func validateOriginPurgeURLs(paths []string, allowedHosts map[string]struct{}) ([]*url.URL, error) {
	if len(paths) > maxOriginPurgeURLs {
		return nil, fmt.Errorf("origin purge supports at most %d URLs", maxOriginPurgeURLs)
	}

	validatedURLs := make([]*url.URL, 0, len(paths))
	for index, path := range paths {
		validatedURL, err := validateOriginPurgeURL(path, allowedHosts)
		if err != nil {
			return nil, fmt.Errorf("invalid origin purge URL at index %d: %w", index, err)
		}
		validatedURLs = append(validatedURLs, validatedURL)
	}

	return validatedURLs, nil
}

type originPurgeFailure struct {
	Index      int    `json:"index"`
	Host       string `json:"host"`
	HTTPStatus int    `json:"httpStatus,omitempty"`
	Reason     string `json:"reason"`
}

func (failure originPurgeFailure) Error() string {
	return fmt.Sprintf("origin purge request %d to %s: %s (HTTP %d)",
		failure.Index, failure.Host, failure.Reason, failure.HTTPStatus)
}

func executeOriginPurgeRequest(
	requestContext context.Context,
	validatedURLs []*url.URL,
	ctx v1alpha1.Context,
	client *http.Client,
	confirmation v1alpha1.OriginPurgeConfirmationSpec,
) []originPurgeFailure {
	var requestErrors []originPurgeFailure

	for index, validatedURL := range validatedURLs {
		getRequest, err := http.NewRequestWithContext(
			requestContext, http.MethodGet, validatedURL.String(), nil,
		)
		if err != nil {
			ctx.Logger.Error(sanitizeOriginPurgeRequestError(index, validatedURL, err))
			requestErrors = append(requestErrors, originPurgeFailure{Index: index, Host: validatedURL.Host, Reason: "Failed to create origin request"})
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

		// A fresh challenge prevents a cached purge response (or an ordinary
		// content response) from being mistaken for this operation's success.
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			requestErrors = append(requestErrors, originPurgeFailure{Index: index, Host: validatedURL.Host,
				Reason: "Failed to generate origin purge request ID"})
			continue
		}
		requestID := hex.EncodeToString(nonce[:])
		getRequest.Header.Set(confirmation.RequestIDHeader, requestID)

		// Send the GET request
		response, err := client.Do(getRequest)
		if err != nil {
			ctx.Logger.Error(sanitizeOriginPurgeRequestError(index, validatedURL, err))
			requestErrors = append(requestErrors, originPurgeFailure{Index: index, Host: validatedURL.Host, Reason: "Failed to communicate with origin"})
			continue
		}

		_, readErr := io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBodySize))
		response.Body.Close()
		if readErr != nil {
			requestErrors = append(requestErrors, originPurgeFailure{Index: index, Host: validatedURL.Host,
				HTTPStatus: response.StatusCode, Reason: "Failed to read origin response"})
			continue
		}

		if !isSuccessfulOriginPurgeStatus(response.StatusCode) {
			requestErrors = append(requestErrors, originPurgeFailure{Index: index, Host: validatedURL.Host,
				HTTPStatus: response.StatusCode, Reason: "Origin returned an unsuccessful status"})
			continue
		}
		if !hasOriginPurgeConfirmation(response.Header, requestID, confirmation) {
			requestErrors = append(requestErrors, originPurgeFailure{Index: index, Host: validatedURL.Host,
				HTTPStatus: response.StatusCode, Reason: "Origin did not confirm the purge for this request"})
			continue
		}

		ctx.Logger.Infof("Origin purge request to %s%s returned status code %d",
			validatedURL.Host, validatedURL.EscapedPath(), response.StatusCode)
	}

	return requestErrors
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
	// The origin normalizes an absent cache entry to 204. A public 404/412
	// may come from a different route and cannot confirm a purge.
	return status == http.StatusOK || status == http.StatusNoContent
}

func buildOriginPurgeConfirmation(config v1alpha1.OriginPurgeConfirmationSpec) (v1alpha1.OriginPurgeConfirmationSpec, error) {
	if config.RequestIDHeader == "" {
		config.RequestIDHeader = originPurgeRequestIDHeader
	}
	if config.ResponseHeader == "" {
		config.ResponseHeader = originPurgeConfirmationHeader
	}
	if config.ResponseValue == "" {
		config.ResponseValue = originPurgeConfirmationValue
	}
	for _, name := range []string{config.RequestIDHeader, config.ResponseHeader} {
		for _, c := range name {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
				strings.ContainsRune("!#$%&'*+-.^_`|~", c)) {
				return config, errors.New("confirmation headers must be valid HTTP field names")
			}
		}
	}
	if strings.EqualFold(config.RequestIDHeader, config.ResponseHeader) {
		return config, errors.New("request ID and confirmation headers must be distinct")
	}
	if strings.TrimSpace(config.ResponseValue) != config.ResponseValue {
		return config, errors.New("confirmation value must not have surrounding whitespace")
	}
	for _, c := range config.ResponseValue {
		if c < 0x20 || c > 0x7e {
			return config, errors.New("confirmation value must contain printable ASCII characters")
		}
	}
	return config, nil
}

func hasOriginPurgeConfirmation(headers http.Header, requestID string, config v1alpha1.OriginPurgeConfirmationSpec) bool {
	confirmations := headers.Values(config.ResponseHeader)
	requestIDs := headers.Values(config.RequestIDHeader)
	return len(confirmations) == 1 && confirmations[0] == config.ResponseValue &&
		len(requestIDs) == 1 && requestIDs[0] == requestID
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
