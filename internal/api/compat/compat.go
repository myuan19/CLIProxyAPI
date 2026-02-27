// Package compat provides a request body compatibility layer as Gin middleware.
//
// Some clients send request bodies in a format that doesn't match the endpoint they hit.
// For example, Cursor may send OpenAI Responses-format payloads (with "input") to the
// Chat Completions endpoint (which expects "messages"). Rather than rejecting these
// requests, the compat middleware transparently converts the body before downstream
// handlers see it.
//
// Rules are defined declaratively and attached to specific routes, keeping handler code
// completely unaware of the compatibility logic.
package compat

import (
	"bytes"
	"io"

	"github.com/gin-gonic/gin"
	responsesconverter "github.com/router-for-me/CLIProxyAPI/v6/internal/translator/openai/openai/responses"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// ContextKey is the Gin context key where the applied rule name is stored.
const ContextKey = "COMPAT_APPLIED"

// Rule describes a single body-level compatibility conversion.
type Rule struct {
	Name      string
	Condition func(body []byte) bool
	Transform func(body []byte) []byte
}

// ChatCompletionsRules are the compatibility rules for the /v1/chat/completions endpoint.
var ChatCompletionsRules = []Rule{
	{
		Name: "input-to-messages",
		Condition: func(body []byte) bool {
			return !gjson.GetBytes(body, "messages").Exists() &&
				gjson.GetBytes(body, "input").Exists()
		},
		Transform: func(body []byte) []byte {
			model := gjson.GetBytes(body, "model").String()
			stream := gjson.GetBytes(body, "stream").Bool()
			return responsesconverter.ConvertOpenAIResponsesRequestToOpenAIChatCompletions(model, body, stream)
		},
	},
}

// Middleware returns a Gin handler that applies the first matching Rule to the
// request body and stores the rule name in the Gin context under ContextKey.
// If no rule matches, the body passes through unchanged.
func Middleware(rules []Rule) gin.HandlerFunc {
	return func(c *gin.Context) {
		if len(rules) == 0 {
			c.Next()
			return
		}

		rawBody, err := io.ReadAll(c.Request.Body)
		if err != nil || len(rawBody) == 0 {
			c.Next()
			return
		}

		for _, rule := range rules {
			if rule.Condition(rawBody) {
				rawBody = rule.Transform(rawBody)
				c.Set(ContextKey, rule.Name)
				log.Debugf("[Compat] Applied rule %q", rule.Name)
				break
			}
		}

		c.Request.Body = io.NopCloser(bytes.NewReader(rawBody))
		c.Next()
	}
}
