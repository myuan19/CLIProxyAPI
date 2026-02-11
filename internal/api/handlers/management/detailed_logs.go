package management

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
)

// GetDetailedRequestLog returns the current detailed request logging status and stats.
func (h *Handler) GetDetailedRequestLog(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}

	enabled := h.cfg.DetailedRequestLog
	maxSizeMB := h.cfg.DetailedRequestLogMaxSizeMB
	if maxSizeMB <= 0 {
		maxSizeMB = 100
	}

	result := gin.H{
		"detailed-request-log":              enabled,
		"detailed-request-log-max-size-mb":  maxSizeMB,
		"detailed-request-log-show-retries": h.cfg.DetailedRequestLogShowRetries,
	}

	// Include stats if logger is available
	if h.detailedLogger != nil {
		sizeBytes, recordCount, err := h.detailedLogger.GetStats()
		if err == nil {
			result["size_bytes"] = sizeBytes
			result["size_mb"] = fmt.Sprintf("%.2f", float64(sizeBytes)/1024/1024)
			result["record_count"] = recordCount
		}
	}

	c.JSON(http.StatusOK, result)
}

// PutDetailedRequestLog enables or disables detailed request logging, and/or updates show-retries UI preference.
// Body may include "value" (bool) for detailed log enabled, "show_retries" (bool) for UI preference; at least one required.
func (h *Handler) PutDetailedRequestLog(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}

	var body struct {
		Value       *bool `json:"value"`
		ShowRetries *bool `json:"show_retries"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	if body.Value == nil && body.ShowRetries == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body, expected {\"value\": true/false} and/or {\"show_retries\": true/false}"})
		return
	}

	if body.Value != nil {
		h.cfg.DetailedRequestLog = *body.Value
		if h.detailedLogger != nil {
			h.detailedLogger.SetEnabled(*body.Value)
		}
	}
	if body.ShowRetries != nil {
		h.cfg.DetailedRequestLogShowRetries = *body.ShowRetries
	}

	h.persist(c)
}

// ListDetailedRequests returns a paginated, filtered list of detailed request records.
func (h *Handler) ListDetailedRequests(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}
	if h.detailedLogger == nil {
		c.JSON(http.StatusOK, gin.H{
			"records":  []any{},
			"total":    0,
			"api_keys": []string{},
		})
		return
	}

	// Support filtering by api_key_hash (SHA hash) or api_key (masked key)
	apiKeyFilter := strings.TrimSpace(c.Query("api_key_hash"))
	if apiKeyFilter == "" {
		apiKeyFilter = strings.TrimSpace(c.Query("api_key"))
	}
	filter := logging.RecordFilter{
		APIKeyHash: apiKeyFilter,
		StatusCode: strings.TrimSpace(c.Query("status_code")),
	}

	// Parse pagination
	if limitStr := c.Query("limit"); limitStr != "" {
		if n, err := strconv.Atoi(limitStr); err == nil && n > 0 {
			filter.Limit = n
		}
	}
	if filter.Limit <= 0 {
		filter.Limit = 50
	}
	if filter.Limit > 200 {
		filter.Limit = 200
	}

	if offsetStr := c.Query("offset"); offsetStr != "" {
		if n, err := strconv.Atoi(offsetStr); err == nil && n >= 0 {
			filter.Offset = n
		}
	}

	// Parse time filters
	if afterStr := c.Query("after"); afterStr != "" {
		if ts, err := strconv.ParseInt(afterStr, 10, 64); err == nil && ts > 0 {
			filter.After = time.Unix(ts, 0)
		}
	}
	if beforeStr := c.Query("before"); beforeStr != "" {
		if ts, err := strconv.ParseInt(beforeStr, 10, 64); err == nil && ts > 0 {
			filter.Before = time.Unix(ts, 0)
		}
	}

	records, total, apiKeys, err := h.detailedLogger.ReadRecords(filter)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to read records: %v", err)})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"records":  records,
		"total":    total,
		"offset":   filter.Offset,
		"limit":    filter.Limit,
		"api_keys": apiKeys,
	})
}

// GetDetailedRequest returns a single detailed request record by ID.
func (h *Handler) GetDetailedRequest(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}
	if h.detailedLogger == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "detailed logging not available"})
		return
	}

	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing request ID"})
		return
	}

	record, err := h.detailedLogger.ReadRecordByID(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to read record: %v", err)})
		return
	}
	if record == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "record not found"})
		return
	}

	// Generate curl command
	curlCmd := generateCurlCommand(record)

	c.JSON(http.StatusOK, gin.H{
		"record": record,
		"curl":   curlCmd,
	})
}

// DeleteDetailedRequests removes all detailed request log records.
func (h *Handler) DeleteDetailedRequests(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}
	if h.detailedLogger == nil {
		c.JSON(http.StatusOK, gin.H{"success": true, "message": "no records to delete"})
		return
	}

	if err := h.detailedLogger.DeleteAll(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to delete records: %v", err)})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "message": "all detailed request records deleted"})
}

// generateCurlCommand builds a curl command string from a request record.
func generateCurlCommand(record *logging.DetailedRequestRecord) string {
	if record == nil {
		return ""
	}

	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("curl -X %s '%s'", record.Method, record.URL))

	if record.RequestHeaders != nil {
		for key, values := range record.RequestHeaders {
			// Skip some headers that are auto-generated
			lowerKey := strings.ToLower(key)
			if lowerKey == "content-length" || lowerKey == "host" || lowerKey == "accept-encoding" {
				continue
			}
			for _, value := range values {
				// Mask sensitive headers
				if isSensitiveHeader(lowerKey) {
					value = "***"
				}
				builder.WriteString(fmt.Sprintf(" \\\n  -H '%s: %s'", key, escapeShellSingle(value)))
			}
		}
	}

	if record.RequestBody != "" {
		body := record.RequestBody
		if len(body) > 10000 {
			body = body[:10000] + "...[truncated]"
		}
		builder.WriteString(fmt.Sprintf(" \\\n  -d '%s'", escapeShellSingle(body)))
	}

	return builder.String()
}

// isSensitiveHeader checks if a header name contains sensitive information.
// Note: Authorization is NOT masked because the user needs it for cURL replay.
func isSensitiveHeader(name string) bool {
	sensitiveHeaders := []string{"cookie", "x-management-key"}
	for _, h := range sensitiveHeaders {
		if name == h {
			return true
		}
	}
	return false
}

// escapeShellSingle escapes single quotes for use in shell single-quoted strings.
func escapeShellSingle(s string) string {
	return strings.ReplaceAll(s, "'", "'\\''")
}
