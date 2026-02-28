// Package compat provides a request-body compatibility layer as Gin middleware.
//
// When a client sends a request body whose format doesn't match the endpoint it hits
// (e.g. Claude-format body to /v1/chat/completions), the middleware transparently
// converts the body to the endpoint's expected format using the translator registry.
//
// The endpoint is always treated as the source of truth: the body adapts, not the route.
package compat

import (
	"bytes"
	"fmt"
	"io"

	"github.com/gin-gonic/gin"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// ContextKey is the Gin context key where the applied conversion name is stored.
const ContextKey = "COMPAT_APPLIED"

// DetectBodyFormat inspects a raw JSON body and returns the most likely API format.
// Returns empty string if the format cannot be determined.
func DetectBodyFormat(body []byte) sdktranslator.Format {
	hasMessages := gjson.GetBytes(body, "messages").Exists()
	hasInput := gjson.GetBytes(body, "input").Exists()
	hasContents := gjson.GetBytes(body, "contents").Exists()

	// Gemini: has "contents", no "messages"
	if hasContents && !hasMessages {
		return sdktranslator.FormatGemini
	}

	// OpenAI Responses: has "input", no "messages"
	if hasInput && !hasMessages {
		return sdktranslator.FormatOpenAIResponse
	}

	// Both Claude and OpenAI use "messages"; distinguish by Claude-only fields.
	if hasMessages {
		if isClaudeBody(body) {
			return sdktranslator.FormatClaude
		}
		return sdktranslator.FormatOpenAI
	}

	return ""
}

// isClaudeBody returns true if the body contains fields specific to the Anthropic
// Messages API that are absent from OpenAI Chat Completions.
func isClaudeBody(body []byte) bool {
	// "system" as a top-level JSON array is Claude-only; OpenAI puts system in messages.
	if s := gjson.GetBytes(body, "system"); s.Exists() && s.IsArray() {
		return true
	}

	// "stop_sequences" is Claude; OpenAI uses "stop".
	if gjson.GetBytes(body, "stop_sequences").Exists() {
		return true
	}

	// Tools with "input_schema" are Claude; OpenAI nests under "function.parameters".
	if tools := gjson.GetBytes(body, "tools"); tools.Exists() && tools.IsArray() {
		first := tools.Array()
		if len(first) > 0 && first[0].Get("input_schema").Exists() {
			return true
		}
	}

	return false
}

// AutoCompat returns a Gin middleware that detects the request body format and, if
// it doesn't match endpointFormat, translates the body using the translator registry.
// The conversion name (e.g. "claude-to-openai") is stored under ContextKey.
func AutoCompat(endpointFormat sdktranslator.Format) gin.HandlerFunc {
	return func(c *gin.Context) {
		rawBody, err := io.ReadAll(c.Request.Body)
		if err != nil || len(rawBody) == 0 {
			c.Next()
			return
		}

		detected := DetectBodyFormat(rawBody)

		if detected == "" || detected == endpointFormat {
			c.Request.Body = io.NopCloser(bytes.NewReader(rawBody))
			c.Next()
			return
		}

		model := gjson.GetBytes(rawBody, "model").String()
		stream := gjson.GetBytes(rawBody, "stream").Bool()
		converted := sdktranslator.TranslateRequest(detected, endpointFormat, model, rawBody, stream)

		ruleName := fmt.Sprintf("%s-to-%s", detected, endpointFormat)
		c.Set(ContextKey, ruleName)
		log.Debugf("[Compat] %s", ruleName)

		c.Request.Body = io.NopCloser(bytes.NewReader(converted))
		c.Next()
	}
}
