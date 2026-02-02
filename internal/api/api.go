package api

import (
	"akapurgo/api/v1alpha1"
	"akapurgo/internal/commons"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/akamai/AkamaiOPEN-edgegrid-golang/v9/pkg/edgegrid"
	"github.com/gofiber/fiber/v2"
)

var (
	akamaiResp v1alpha1.AkamaiResponse
	req        v1alpha1.PurgeRequest
	purgeURL   string
	resp       *http.Response
)

func PurgeHandler(ctx v1alpha1.Context) func(c *fiber.Ctx) error {
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
		if err := c.BodyParser(&req); err != nil {
			ctx.Logger.Errorf("Failed to parse request: %v\n", err)
			return c.Status(fiber.StatusBadRequest).JSON(map[string]string{
				"error": "Invalid request payload",
			})
		}

		// Duplicate URLs with imbypass=true query parameter if requested
		if req.ImBypass && req.PurgeType == "urls" {
			req.Paths = duplicatePathsWithImBypass(req.Paths)
		}

		// Determine the Akamai API URL
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
		client := &http.Client{}
		apiRequest, err := http.NewRequest("POST", purgeURL, bytes.NewReader(payloadBytes))
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
		resp, err = client.Do(apiRequest)
		if err != nil {
			ctx.Logger.Errorf("Failed to send request to Akamai: %v\n", err)
			return c.Status(fiber.StatusInternalServerError).JSON(map[string]string{
				"error": "Failed to communicate with Akamai",
			})
		}

		defer resp.Body.Close()

		// Decode the Akamai response
		if err := json.NewDecoder(resp.Body).Decode(&akamaiResp); err != nil {
			ctx.Logger.Errorf("Failed to decode Akamai response: %v\n", err)
			return c.Status(fiber.StatusInternalServerError).JSON(map[string]string{
				"error": "Failed to decode Akamai response",
			})
		}

		// Send a GET requests to purged URLs
		if is2xx(akamaiResp.HTTPStatus) && req.PostPurgeRequest && ctx.Config.PostPurgeRequest.Enabled {
			time.Sleep(5 * time.Second) // Wait for 5 seconds before sending GET requests
			executePurgeRequest(req.Paths, ctx)
		}

		// Forward the Akamai response to the client
		ctx.Logger.Infof(`akamai-response,detail='%s',status=%d`, akamaiResp.Detail, akamaiResp.HTTPStatus)
		return c.Status(resp.StatusCode).JSON(akamaiResp)
	}
}

func executePurgeRequest(paths []string, ctx v1alpha1.Context) {
	client := &http.Client{}

	for _, path := range paths {
		// Create the HTTP GET request
		getRequest, err := http.NewRequest("GET", path, nil)
		if err != nil {
			ctx.Logger.Errorf("Failed to create GET request for %s: %v\n", path, err)
			continue
		}

		// Add custom headers from configuration
		for key, value := range ctx.Config.PostPurgeRequest.Headers {
			getRequest.Header.Set(key, value)
		}

		// Send the GET request
		response, err := client.Do(getRequest)
		if err != nil {
			ctx.Logger.Errorf("Failed to send GET request to %s: %v\n", path, err)
			continue
		}

		// Read and discard the body to complete the request properly
		_, err = io.ReadAll(response.Body)
		if err != nil {
			ctx.Logger.Warnf("Failed to read response body from %s: %v\n", path, err)
		}
		response.Body.Close()

		// Log the response status
		ctx.Logger.Infof("GET request to %s returned status code %d\n", path, response.StatusCode)
	}
}

func is2xx(status int) bool {
	return status >= 200 && status < 300
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
