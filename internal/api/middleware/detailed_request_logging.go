// Package middleware provides HTTP middleware components for the CLI Proxy API server.
// This file contains the detailed request logging middleware that captures comprehensive
// structured request and response data for browsing via the management panel.
package middleware

import (
	"bytes"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	"github.com/tidwall/gjson"
)

// DetailedRequestLoggingMiddleware creates a Gin middleware that captures structured request/response
// data into the DetailedRequestLogger. It runs after the request is processed and captures
// API key, upstream attempts, status codes, and full request/response bodies.
func DetailedRequestLoggingMiddleware(logger *logging.DetailedRequestLogger) gin.HandlerFunc {
	return func(c *gin.Context) {
		if logger == nil || !logger.IsEnabled() {
			c.Next()
			return
		}

		if c.Request.Method == http.MethodGet {
			c.Next()
			return
		}

		path := c.Request.URL.Path
		if !shouldLogDetailedRequest(path) {
			c.Next()
			return
		}

		startTime := time.Now()

		// Capture request body (it was already read and restored by RequestLoggingMiddleware)
		var requestBody []byte
		if c.Request.Body != nil {
			bodyBytes, err := readAndRestoreBody(c)
			if err == nil {
				requestBody = bodyBytes
			}
		}

		// Capture request headers
		requestHeaders := make(map[string][]string)
		for key, values := range c.Request.Header {
			headerValues := make([]string, len(values))
			copy(headerValues, values)
			requestHeaders[key] = headerValues
		}

		requestID := logging.GetGinRequestID(c)

		// Create a response capture wrapper if not already wrapped
		detailedCapture := &detailedResponseCapture{
			ResponseWriter: c.Writer,
			body:           &bytes.Buffer{},
		}
		c.Writer = detailedCapture

		c.Next()

		// Re-check if logger is still enabled after processing
		if !logger.IsEnabled() {
			return
		}

		// Extract data from context
		apiKeyRaw, _ := c.Get("apiKey")
		apiKey, _ := apiKeyRaw.(string)

		// Build the record
		record := &logging.DetailedRequestRecord{
			ID:        requestID,
			Timestamp: startTime,
			URL:       c.Request.URL.Path,
			Method:    c.Request.Method,
		}

		if apiKey != "" {
			record.APIKey = logging.MaskAPIKey(apiKey)
			record.APIKeyHash = logging.HashAPIKey(apiKey)
		}

		// Extract model from request body
		if len(requestBody) > 0 {
			model := gjson.GetBytes(requestBody, "model").String()
			if model != "" {
				record.Model = model
			}
			record.RequestBody = truncateString(string(requestBody), 50000)
		}

		record.RequestHeaders = requestHeaders

		// Detect streaming
		contentType := detailedCapture.Header().Get("Content-Type")
		record.IsStreaming = strings.Contains(contentType, "text/event-stream")

		// Capture response
		finalStatus := detailedCapture.statusCode
		if finalStatus == 0 {
			finalStatus = http.StatusOK
		}
		record.StatusCode = finalStatus

		responseHeaders := make(map[string][]string)
		for key, values := range detailedCapture.Header() {
			headerValues := make([]string, len(values))
			copy(headerValues, values)
			responseHeaders[key] = headerValues
		}
		record.ResponseHeaders = responseHeaders

		if detailedCapture.body.Len() > 0 {
			record.ResponseBody = truncateString(detailedCapture.body.String(), 50000)
		}

		// Extract upstream attempts from context
		record.Attempts = extractAttempts(c)

		// Extract errors
		apiResponseError, isExist := c.Get("API_RESPONSE_ERROR")
		if isExist {
			if apiErrors, ok := apiResponseError.(interface{ Error() string }); ok {
				record.Error = apiErrors.Error()
			}
		}

		// Calculate duration
		record.TotalDurationMs = time.Since(startTime).Milliseconds()

		logger.LogRecord(record)
	}
}

// detailedResponseCapture wraps gin.ResponseWriter to capture the response body.
type detailedResponseCapture struct {
	gin.ResponseWriter
	body       *bytes.Buffer
	statusCode int
}

func (w *detailedResponseCapture) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	// Limit capture to avoid memory issues
	if w.body.Len() < 100000 {
		remaining := 100000 - w.body.Len()
		if len(data) > remaining {
			w.body.Write(data[:remaining])
		} else {
			w.body.Write(data)
		}
	}
	return n, err
}

func (w *detailedResponseCapture) WriteString(data string) (int, error) {
	n, err := w.ResponseWriter.WriteString(data)
	if w.body.Len() < 100000 {
		remaining := 100000 - w.body.Len()
		if len(data) > remaining {
			w.body.WriteString(data[:remaining])
		} else {
			w.body.WriteString(data)
		}
	}
	return n, err
}

func (w *detailedResponseCapture) WriteHeader(statusCode int) {
	w.statusCode = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}

// shouldLogDetailedRequest determines whether this request should be captured for detailed logging.
func shouldLogDetailedRequest(path string) bool {
	if strings.HasPrefix(path, "/v0/management") || strings.HasPrefix(path, "/management") {
		return false
	}
	if strings.HasPrefix(path, "/api") {
		return strings.HasPrefix(path, "/api/provider")
	}
	return true
}

// readAndRestoreBody reads the request body and restores it for subsequent handlers.
func readAndRestoreBody(c *gin.Context) ([]byte, error) {
	if c.Request.Body == nil {
		return nil, nil
	}
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(c.Request.Body); err != nil {
		return nil, err
	}
	bodyBytes := buf.Bytes()
	c.Request.Body = nopCloser{bytes.NewReader(bodyBytes)}
	return bodyBytes, nil
}

type nopCloser struct {
	*bytes.Reader
}

func (nopCloser) Close() error { return nil }

// extractAttempts extracts upstream attempt data from the Gin context.
// It parses the API_REQUEST and API_RESPONSE aggregated data to build attempt records.
func extractAttempts(c *gin.Context) []logging.DetailedAttempt {
	apiRequestRaw, hasReq := c.Get("API_REQUEST")
	apiResponseRaw, hasResp := c.Get("API_RESPONSE")

	if !hasReq && !hasResp {
		return nil
	}

	var attempts []DetailedAttemptFromContext

	if hasReq {
		if reqData, ok := apiRequestRaw.([]byte); ok && len(reqData) > 0 {
			attempts = parseAttemptRequests(string(reqData))
		}
	}

	if hasResp {
		if respData, ok := apiResponseRaw.([]byte); ok && len(respData) > 0 {
			mergeAttemptResponses(attempts, string(respData))
		}
	}

	// Convert to logging.DetailedAttempt
	result := make([]logging.DetailedAttempt, 0, len(attempts))
	for _, a := range attempts {
		result = append(result, logging.DetailedAttempt{
			Index:       a.Index,
			UpstreamURL: a.UpstreamURL,
			Method:      a.Method,
			Auth:        a.Auth,
			RequestBody: truncateString(a.RequestBody, 30000),
			StatusCode:  a.StatusCode,
			ResponseBody: truncateString(a.ResponseBody, 30000),
			Error:       a.Error,
		})
	}

	return result
}

// DetailedAttemptFromContext is a temporary struct for parsing attempt data from context.
type DetailedAttemptFromContext struct {
	Index        int
	UpstreamURL  string
	Method       string
	Auth         string
	RequestBody  string
	StatusCode   int
	ResponseBody string
	Error        string
}

// parseAttemptRequests parses the aggregated API_REQUEST data into attempt records.
func parseAttemptRequests(data string) []DetailedAttemptFromContext {
	sections := strings.Split(data, "=== API REQUEST ")
	var attempts []DetailedAttemptFromContext

	for _, section := range sections {
		section = strings.TrimSpace(section)
		if section == "" {
			continue
		}

		attempt := DetailedAttemptFromContext{}

		// Extract index
		if idx := strings.Index(section, " ==="); idx > 0 {
			numStr := section[:idx]
			var n int
			if _, err := parseIntSafe(numStr); err == nil {
				n = parseIntValue(numStr)
			}
			attempt.Index = n
			section = section[idx+4:]
		}

		lines := strings.Split(section, "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "Upstream URL: ") {
				attempt.UpstreamURL = strings.TrimPrefix(line, "Upstream URL: ")
			} else if strings.HasPrefix(line, "HTTP Method: ") {
				attempt.Method = strings.TrimPrefix(line, "HTTP Method: ")
			} else if strings.HasPrefix(line, "Auth: ") {
				attempt.Auth = strings.TrimPrefix(line, "Auth: ")
			}
		}

		// Extract body section
		if bodyIdx := strings.Index(section, "Body:\n"); bodyIdx >= 0 {
			body := section[bodyIdx+6:]
			// Trim trailing empty lines
			body = strings.TrimRight(body, "\n ")
			attempt.RequestBody = body
		}

		attempts = append(attempts, attempt)
	}

	return attempts
}

// mergeAttemptResponses merges response data into existing attempt records.
func mergeAttemptResponses(attempts []DetailedAttemptFromContext, data string) {
	sections := strings.Split(data, "=== API RESPONSE ")

	for _, section := range sections {
		section = strings.TrimSpace(section)
		if section == "" {
			continue
		}

		var index int
		if idx := strings.Index(section, " ==="); idx > 0 {
			numStr := section[:idx]
			if _, err := parseIntSafe(numStr); err == nil {
				index = parseIntValue(numStr)
			}
			section = section[idx+4:]
		}

		// Find matching attempt
		var target *DetailedAttemptFromContext
		for i := range attempts {
			if attempts[i].Index == index {
				target = &attempts[i]
				break
			}
		}
		if target == nil {
			continue
		}

		lines := strings.Split(section, "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "Status: ") {
				statusStr := strings.TrimPrefix(line, "Status: ")
				if n := parseIntValue(statusStr); n > 0 {
					target.StatusCode = n
				}
			} else if strings.HasPrefix(line, "Error: ") {
				target.Error = strings.TrimPrefix(line, "Error: ")
			}
		}

		// Extract body
		if bodyIdx := strings.Index(section, "Body:\n"); bodyIdx >= 0 {
			body := section[bodyIdx+6:]
			body = strings.TrimRight(body, "\n ")
			target.ResponseBody = body
		}
	}
}

func parseIntSafe(s string) (int, error) {
	s = strings.TrimSpace(s)
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, nil
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

func parseIntValue(s string) int {
	s = strings.TrimSpace(s)
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// truncateString truncates a string to the given maximum length.
func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "...[truncated]"
}
