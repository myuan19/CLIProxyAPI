package executor

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const (
	// 请求日志（RequestLog）使用的 Gin 键
	apiAttemptsKey = "API_UPSTREAM_ATTEMPTS"
	apiRequestKey  = "API_REQUEST"
	apiResponseKey = "API_RESPONSE"

	// 详细日志（DetailedRequestLog）专用 Gin 键，与请求日志完全解耦
	detailedLogAttemptsKey = "DETAILED_LOG_API_ATTEMPTS"
	detailedLogRequestKey  = "DETAILED_LOG_API_REQUEST"
	detailedLogResponseKey = "DETAILED_LOG_API_RESPONSE"
)

// attemptKeys 表示一组 Gin 键，用于某一类消费者（请求日志 or 详细日志）
type attemptKeys struct {
	attempts string
	request  string
	response string
}

// upstreamRequestLog captures the outbound upstream request details for logging.
type upstreamRequestLog struct {
	URL       string
	Method    string
	Headers   http.Header
	Body      []byte
	Provider  string
	AuthID    string
	AuthLabel string
	AuthType  string
	AuthValue string
}

type upstreamAttempt struct {
	index                int
	request              string
	response             *strings.Builder
	responseIntroWritten bool
	statusWritten        bool
	headersWritten       bool
	bodyStarted          bool
	bodyHasContent       bool
	errorWritten         bool
}

// shouldRecordAttemptsForDetailedLog returns true when detailed request log is enabled.
// Detailed log is independent of RequestLog: attempts are recorded for it when this is true.
func shouldRecordAttemptsForDetailedLog(cfg *config.Config) bool {
	return cfg != nil && cfg.DetailedRequestLog
}

// shouldRecordAttemptsForRequestLog returns true when the generic request log is enabled.
func shouldRecordAttemptsForRequestLog(cfg *config.Config) bool {
	return cfg != nil && cfg.RequestLog
}

// recordAPIRequest stores the upstream request metadata in Gin context.
// 完全解耦：详细日志与请求日志使用独立 Gin 键，各自根据开关写入。
func recordAPIRequest(ctx context.Context, cfg *config.Config, info upstreamRequestLog) {
	ginCtx := ginContextFrom(ctx)
	if ginCtx == nil {
		return
	}
	if shouldRecordAttemptsForDetailedLog(cfg) {
		recordAPIRequestForKeys(ginCtx, &attemptKeys{detailedLogAttemptsKey, detailedLogRequestKey, detailedLogResponseKey}, info)
	}
	if shouldRecordAttemptsForRequestLog(cfg) {
		recordAPIRequestForKeys(ginCtx, &attemptKeys{apiAttemptsKey, apiRequestKey, apiResponseKey}, info)
	}
}

func recordAPIRequestForKeys(ginCtx *gin.Context, keys *attemptKeys, info upstreamRequestLog) {
	attempts := getAttemptsForKey(ginCtx, keys.attempts)
	index := len(attempts) + 1

	builder := &strings.Builder{}
	builder.WriteString(fmt.Sprintf("=== API REQUEST %d ===\n", index))
	builder.WriteString(fmt.Sprintf("Timestamp: %s\n", time.Now().Format(time.RFC3339Nano)))
	if info.URL != "" {
		builder.WriteString(fmt.Sprintf("Upstream URL: %s\n", info.URL))
	} else {
		builder.WriteString("Upstream URL: <unknown>\n")
	}
	if info.Method != "" {
		builder.WriteString(fmt.Sprintf("HTTP Method: %s\n", info.Method))
	}
	if auth := formatAuthInfo(info); auth != "" {
		builder.WriteString(fmt.Sprintf("Auth: %s\n", auth))
	}
	builder.WriteString("\nHeaders:\n")
	writeHeaders(builder, info.Headers)
	builder.WriteString("\nBody:\n")
	if len(info.Body) > 0 {
		builder.WriteString(string(info.Body))
	} else {
		builder.WriteString("<empty>")
	}
	builder.WriteString("\n\n")

	attempt := &upstreamAttempt{
		index:    index,
		request:  builder.String(),
		response: &strings.Builder{},
	}
	attempts = append(attempts, attempt)
	ginCtx.Set(keys.attempts, attempts)
	updateAggregatedRequestForKey(ginCtx, attempts, keys.request)
}

// recordAPIResponseMetadata captures upstream response status/header information for the latest attempt.
func recordAPIResponseMetadata(ctx context.Context, cfg *config.Config, status int, headers http.Header) {
	ginCtx := ginContextFrom(ctx)
	if ginCtx == nil {
		return
	}
	if shouldRecordAttemptsForDetailedLog(cfg) {
		recordAPIResponseMetadataForKeys(ginCtx, &attemptKeys{detailedLogAttemptsKey, detailedLogRequestKey, detailedLogResponseKey}, status, headers)
	}
	if shouldRecordAttemptsForRequestLog(cfg) {
		recordAPIResponseMetadataForKeys(ginCtx, &attemptKeys{apiAttemptsKey, apiRequestKey, apiResponseKey}, status, headers)
	}
}

func recordAPIResponseMetadataForKeys(ginCtx *gin.Context, keys *attemptKeys, status int, headers http.Header) {
	attempts, attempt := ensureAttemptForKey(ginCtx, keys)
	ensureResponseIntro(attempt)

	if status > 0 && !attempt.statusWritten {
		attempt.response.WriteString(fmt.Sprintf("Status: %d\n", status))
		attempt.statusWritten = true
	}
	if !attempt.headersWritten {
		attempt.response.WriteString("Headers:\n")
		writeHeaders(attempt.response, headers)
		attempt.headersWritten = true
		attempt.response.WriteString("\n")
	}

	updateAggregatedResponseForKey(ginCtx, attempts, keys.response)
}

// recordAPIResponseError adds an error entry for the latest attempt when no HTTP response is available.
func recordAPIResponseError(ctx context.Context, cfg *config.Config, err error) {
	if err == nil {
		return
	}
	ginCtx := ginContextFrom(ctx)
	if ginCtx == nil {
		return
	}
	if shouldRecordAttemptsForDetailedLog(cfg) {
		recordAPIResponseErrorForKeys(ginCtx, &attemptKeys{detailedLogAttemptsKey, detailedLogRequestKey, detailedLogResponseKey}, err)
	}
	if shouldRecordAttemptsForRequestLog(cfg) {
		recordAPIResponseErrorForKeys(ginCtx, &attemptKeys{apiAttemptsKey, apiRequestKey, apiResponseKey}, err)
	}
}

func recordAPIResponseErrorForKeys(ginCtx *gin.Context, keys *attemptKeys, err error) {
	attempts, attempt := ensureAttemptForKey(ginCtx, keys)
	ensureResponseIntro(attempt)

	if attempt.bodyStarted && !attempt.bodyHasContent {
		attempt.bodyStarted = false
	}
	if attempt.errorWritten {
		attempt.response.WriteString("\n")
	}
	attempt.response.WriteString(fmt.Sprintf("Error: %s\n", err.Error()))
	attempt.errorWritten = true

	updateAggregatedResponseForKey(ginCtx, attempts, keys.response)
}

// appendAPIResponseChunk appends an upstream response chunk to Gin context.
func appendAPIResponseChunk(ctx context.Context, cfg *config.Config, chunk []byte) {
	data := bytes.TrimSpace(chunk)
	if len(data) == 0 {
		return
	}
	ginCtx := ginContextFrom(ctx)
	if ginCtx == nil {
		return
	}
	if shouldRecordAttemptsForDetailedLog(cfg) {
		appendAPIResponseChunkForKeys(ginCtx, &attemptKeys{detailedLogAttemptsKey, detailedLogRequestKey, detailedLogResponseKey}, data)
	}
	if shouldRecordAttemptsForRequestLog(cfg) {
		appendAPIResponseChunkForKeys(ginCtx, &attemptKeys{apiAttemptsKey, apiRequestKey, apiResponseKey}, data)
	}
}

func appendAPIResponseChunkForKeys(ginCtx *gin.Context, keys *attemptKeys, data []byte) {
	attempts, attempt := ensureAttemptForKey(ginCtx, keys)
	ensureResponseIntro(attempt)

	if !attempt.headersWritten {
		attempt.response.WriteString("Headers:\n")
		writeHeaders(attempt.response, nil)
		attempt.headersWritten = true
		attempt.response.WriteString("\n")
	}
	if !attempt.bodyStarted {
		attempt.response.WriteString("Body:\n")
		attempt.bodyStarted = true
	}
	if attempt.bodyHasContent {
		attempt.response.WriteString("\n\n")
	}
	attempt.response.WriteString(string(data))
	attempt.bodyHasContent = true

	updateAggregatedResponseForKey(ginCtx, attempts, keys.response)
}

func ginContextFrom(ctx context.Context) *gin.Context {
	ginCtx, _ := ctx.Value("gin").(*gin.Context)
	return ginCtx
}

func getAttemptsForKey(ginCtx *gin.Context, attemptsKey string) []*upstreamAttempt {
	if ginCtx == nil {
		return nil
	}
	if value, exists := ginCtx.Get(attemptsKey); exists {
		if attempts, ok := value.([]*upstreamAttempt); ok {
			return attempts
		}
	}
	return nil
}

func ensureAttemptForKey(ginCtx *gin.Context, keys *attemptKeys) ([]*upstreamAttempt, *upstreamAttempt) {
	attempts := getAttemptsForKey(ginCtx, keys.attempts)
	if len(attempts) == 0 {
		attempt := &upstreamAttempt{
			index:    1,
			request:  "=== API REQUEST 1 ===\n<missing>\n\n",
			response: &strings.Builder{},
		}
		attempts = []*upstreamAttempt{attempt}
		ginCtx.Set(keys.attempts, attempts)
		updateAggregatedRequestForKey(ginCtx, attempts, keys.request)
	}
	return attempts, attempts[len(attempts)-1]
}

func ensureResponseIntro(attempt *upstreamAttempt) {
	if attempt == nil || attempt.response == nil || attempt.responseIntroWritten {
		return
	}
	attempt.response.WriteString(fmt.Sprintf("=== API RESPONSE %d ===\n", attempt.index))
	attempt.response.WriteString(fmt.Sprintf("Timestamp: %s\n", time.Now().Format(time.RFC3339Nano)))
	attempt.response.WriteString("\n")
	attempt.responseIntroWritten = true
}

func updateAggregatedRequestForKey(ginCtx *gin.Context, attempts []*upstreamAttempt, requestKey string) {
	if ginCtx == nil {
		return
	}
	var builder strings.Builder
	for _, attempt := range attempts {
		builder.WriteString(attempt.request)
	}
	ginCtx.Set(requestKey, []byte(builder.String()))
}

func updateAggregatedResponseForKey(ginCtx *gin.Context, attempts []*upstreamAttempt, responseKey string) {
	if ginCtx == nil {
		return
	}
	var builder strings.Builder
	for idx, attempt := range attempts {
		if attempt == nil || attempt.response == nil {
			continue
		}
		responseText := attempt.response.String()
		if responseText == "" {
			continue
		}
		builder.WriteString(responseText)
		if !strings.HasSuffix(responseText, "\n") {
			builder.WriteString("\n")
		}
		if idx < len(attempts)-1 {
			builder.WriteString("\n")
		}
	}
	ginCtx.Set(responseKey, []byte(builder.String()))
}

func writeHeaders(builder *strings.Builder, headers http.Header) {
	if builder == nil {
		return
	}
	if len(headers) == 0 {
		builder.WriteString("<none>\n")
		return
	}
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		values := headers[key]
		if len(values) == 0 {
			builder.WriteString(fmt.Sprintf("%s:\n", key))
			continue
		}
		for _, value := range values {
			masked := util.MaskSensitiveHeaderValue(key, value)
			builder.WriteString(fmt.Sprintf("%s: %s\n", key, masked))
		}
	}
}

func formatAuthInfo(info upstreamRequestLog) string {
	var parts []string
	if trimmed := strings.TrimSpace(info.Provider); trimmed != "" {
		parts = append(parts, fmt.Sprintf("provider=%s", trimmed))
	}
	if trimmed := strings.TrimSpace(info.AuthID); trimmed != "" {
		parts = append(parts, fmt.Sprintf("auth_id=%s", trimmed))
	}
	if trimmed := strings.TrimSpace(info.AuthLabel); trimmed != "" {
		parts = append(parts, fmt.Sprintf("label=%s", trimmed))
	}

	authType := strings.ToLower(strings.TrimSpace(info.AuthType))
	authValue := strings.TrimSpace(info.AuthValue)
	switch authType {
	case "api_key":
		if authValue != "" {
			parts = append(parts, fmt.Sprintf("type=api_key value=%s", util.HideAPIKey(authValue)))
		} else {
			parts = append(parts, "type=api_key")
		}
	case "oauth":
		parts = append(parts, "type=oauth")
	default:
		if authType != "" {
			if authValue != "" {
				parts = append(parts, fmt.Sprintf("type=%s value=%s", authType, authValue))
			} else {
				parts = append(parts, fmt.Sprintf("type=%s", authType))
			}
		}
	}

	return strings.Join(parts, ", ")
}

func summarizeErrorBody(contentType string, body []byte) string {
	isHTML := strings.Contains(strings.ToLower(contentType), "text/html")
	if !isHTML {
		trimmed := bytes.TrimSpace(bytes.ToLower(body))
		if bytes.HasPrefix(trimmed, []byte("<!doctype html")) || bytes.HasPrefix(trimmed, []byte("<html")) {
			isHTML = true
		}
	}
	if isHTML {
		if title := extractHTMLTitle(body); title != "" {
			return title
		}
		return "[html body omitted]"
	}

	// Try to extract error message from JSON response
	if message := extractJSONErrorMessage(body); message != "" {
		return message
	}

	return string(body)
}

func extractHTMLTitle(body []byte) string {
	lower := bytes.ToLower(body)
	start := bytes.Index(lower, []byte("<title"))
	if start == -1 {
		return ""
	}
	gt := bytes.IndexByte(lower[start:], '>')
	if gt == -1 {
		return ""
	}
	start += gt + 1
	end := bytes.Index(lower[start:], []byte("</title>"))
	if end == -1 {
		return ""
	}
	title := string(body[start : start+end])
	title = html.UnescapeString(title)
	title = strings.TrimSpace(title)
	if title == "" {
		return ""
	}
	return strings.Join(strings.Fields(title), " ")
}

// extractJSONErrorMessage attempts to extract error.message from JSON error responses
func extractJSONErrorMessage(body []byte) string {
	result := gjson.GetBytes(body, "error.message")
	if result.Exists() && result.String() != "" {
		return result.String()
	}
	return ""
}

// logWithRequestID returns a logrus Entry with request_id field populated from context.
// If no request ID is found in context, it returns the standard logger.
func logWithRequestID(ctx context.Context) *log.Entry {
	if ctx == nil {
		return log.NewEntry(log.StandardLogger())
	}
	requestID := logging.GetRequestID(ctx)
	if requestID == "" {
		return log.NewEntry(log.StandardLogger())
	}
	return log.WithField("request_id", requestID)
}
