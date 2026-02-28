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

// linkedField associates a JSON path with candidate formats.
// linksTo stores the formats the body could belong to when this field is present.
// On mismatch, the intersection of all present fields' linksTo yields the inferred format.
type linkedField struct {
	path    string
	linksTo []sdktranslator.Format
}

// formatRule defines the validation constraints for a specific API format.
// A body matches when all have fields are present and no notHave fields exist.
// On mismatch, every present field's linksTo (from both lists) is intersected to infer
// the actual format — positive and negative constraints are unified in one pass.
type formatRule struct {
	have    []linkedField
	notHave []linkedField
}

var formatRules = map[sdktranslator.Format]formatRule{
	sdktranslator.FormatOpenAI: {
		have: []linkedField{
			{"messages", []sdktranslator.Format{sdktranslator.FormatOpenAI, sdktranslator.FormatClaude}},
		},
		notHave: []linkedField{
			{"system", []sdktranslator.Format{sdktranslator.FormatClaude}},
			{"stop_sequences", []sdktranslator.Format{sdktranslator.FormatClaude}},
			{"thinking", []sdktranslator.Format{sdktranslator.FormatClaude}},
			{"contents", []sdktranslator.Format{sdktranslator.FormatGemini}},
			{"input", []sdktranslator.Format{sdktranslator.FormatOpenAIResponse, sdktranslator.FormatCodex}},
			{"instructions", []sdktranslator.Format{sdktranslator.FormatOpenAIResponse, sdktranslator.FormatCodex}},
			{"request", []sdktranslator.Format{sdktranslator.FormatGeminiCLI, sdktranslator.FormatAntigravity}},
		},
	},
	sdktranslator.FormatClaude: {
		have: []linkedField{
			{"messages", []sdktranslator.Format{sdktranslator.FormatOpenAI, sdktranslator.FormatClaude}},
		},
		notHave: []linkedField{
			{"contents", []sdktranslator.Format{sdktranslator.FormatGemini}},
			{"input", []sdktranslator.Format{sdktranslator.FormatOpenAIResponse, sdktranslator.FormatCodex}},
			{"instructions", []sdktranslator.Format{sdktranslator.FormatOpenAIResponse, sdktranslator.FormatCodex}},
			{"request", []sdktranslator.Format{sdktranslator.FormatGeminiCLI, sdktranslator.FormatAntigravity}},
		},
	},
	sdktranslator.FormatOpenAIResponse: {
		have: []linkedField{
			{"input", []sdktranslator.Format{sdktranslator.FormatOpenAIResponse, sdktranslator.FormatCodex}},
		},
		notHave: []linkedField{
			{"messages", []sdktranslator.Format{sdktranslator.FormatOpenAI, sdktranslator.FormatClaude}},
			{"contents", []sdktranslator.Format{sdktranslator.FormatGemini}},
			{"request", []sdktranslator.Format{sdktranslator.FormatGeminiCLI, sdktranslator.FormatAntigravity}},
		},
	},
	sdktranslator.FormatGemini: {
		have: []linkedField{
			{"contents", []sdktranslator.Format{sdktranslator.FormatGemini}},
		},
		notHave: []linkedField{
			{"messages", []sdktranslator.Format{sdktranslator.FormatOpenAI, sdktranslator.FormatClaude}},
			{"input", []sdktranslator.Format{sdktranslator.FormatOpenAIResponse, sdktranslator.FormatCodex}},
			{"request", []sdktranslator.Format{sdktranslator.FormatGeminiCLI, sdktranslator.FormatAntigravity}},
		},
	},
	sdktranslator.FormatGeminiCLI: {
		have: []linkedField{
			{"request", []sdktranslator.Format{sdktranslator.FormatGeminiCLI, sdktranslator.FormatAntigravity}},
			{"request.contents", []sdktranslator.Format{sdktranslator.FormatGeminiCLI, sdktranslator.FormatAntigravity}},
		},
		notHave: []linkedField{
			{"userAgent", []sdktranslator.Format{sdktranslator.FormatAntigravity}},
			{"requestType", []sdktranslator.Format{sdktranslator.FormatAntigravity}},
			{"messages", []sdktranslator.Format{sdktranslator.FormatOpenAI, sdktranslator.FormatClaude}},
			{"contents", []sdktranslator.Format{sdktranslator.FormatGemini}},
			{"input", []sdktranslator.Format{sdktranslator.FormatOpenAIResponse, sdktranslator.FormatCodex}},
		},
	},
	sdktranslator.FormatAntigravity: {
		have: []linkedField{
			{"request", []sdktranslator.Format{sdktranslator.FormatGeminiCLI, sdktranslator.FormatAntigravity}},
		},
		notHave: []linkedField{
			{"messages", []sdktranslator.Format{sdktranslator.FormatOpenAI, sdktranslator.FormatClaude}},
			{"contents", []sdktranslator.Format{sdktranslator.FormatGemini}},
			{"input", []sdktranslator.Format{sdktranslator.FormatOpenAIResponse, sdktranslator.FormatCodex}},
		},
	},
	sdktranslator.FormatCodex: {
		have: []linkedField{
			{"input", []sdktranslator.Format{sdktranslator.FormatOpenAIResponse, sdktranslator.FormatCodex}},
		},
		notHave: []linkedField{
			{"messages", []sdktranslator.Format{sdktranslator.FormatOpenAI, sdktranslator.FormatClaude}},
			{"contents", []sdktranslator.Format{sdktranslator.FormatGemini}},
			{"request", []sdktranslator.Format{sdktranslator.FormatGeminiCLI, sdktranslator.FormatAntigravity}},
		},
	},
}

// formatPriority breaks ties when multiple candidate formats remain after intersection.
var formatPriority = []sdktranslator.Format{
	sdktranslator.FormatOpenAI,
	sdktranslator.FormatClaude,
	sdktranslator.FormatOpenAIResponse,
	sdktranslator.FormatGemini,
	sdktranslator.FormatCodex,
	sdktranslator.FormatGeminiCLI,
	sdktranslator.FormatAntigravity,
}

// detectMismatch checks whether rawBody conforms to endpointFormat.
// Returns ("", true) if the body matches, or (inferredFormat, true) if a
// different format was inferred. Returns ("", false) when inference fails.
//
// When a mismatch is detected, both positive (present have) and negative
// (violated notHave) linksTo sets are intersected together, preventing
// the inferred format from contradicting the fields actually present in the body.
func detectMismatch(rawBody []byte, endpointFormat sdktranslator.Format) (sdktranslator.Format, bool) {
	rule, ok := formatRules[endpointFormat]
	if !ok {
		return "", true
	}

	haveOK := true
	for _, f := range rule.have {
		if !gjson.GetBytes(rawBody, f.path).Exists() {
			haveOK = false
			break
		}
	}

	var violatedLinks [][]sdktranslator.Format
	for _, f := range rule.notHave {
		if gjson.GetBytes(rawBody, f.path).Exists() {
			violatedLinks = append(violatedLinks, f.linksTo)
		}
	}

	if haveOK && len(violatedLinks) == 0 {
		return "", true
	}

	var allLinks [][]sdktranslator.Format
	for _, f := range rule.have {
		if gjson.GetBytes(rawBody, f.path).Exists() {
			allLinks = append(allLinks, f.linksTo)
		}
	}
	allLinks = append(allLinks, violatedLinks...)

	if len(allLinks) > 0 {
		candidates := intersect(allLinks)
		if len(candidates) == 1 {
			return candidates[0], true
		}
		if len(candidates) > 1 {
			return pickByPriority(candidates), true
		}
	}

	return "", false
}

func intersect(sets [][]sdktranslator.Format) []sdktranslator.Format {
	if len(sets) == 0 {
		return nil
	}
	counts := make(map[sdktranslator.Format]int)
	for _, set := range sets {
		seen := make(map[sdktranslator.Format]bool)
		for _, f := range set {
			if !seen[f] {
				counts[f]++
				seen[f] = true
			}
		}
	}
	var result []sdktranslator.Format
	for f, c := range counts {
		if c == len(sets) {
			result = append(result, f)
		}
	}
	return result
}

func pickByPriority(candidates []sdktranslator.Format) sdktranslator.Format {
	set := make(map[sdktranslator.Format]bool, len(candidates))
	for _, c := range candidates {
		set[c] = true
	}
	for _, f := range formatPriority {
		if set[f] {
			return f
		}
	}
	return candidates[0]
}

// AutoCompat returns a Gin middleware that validates the request body against
// endpointFormat. If the body doesn't match and an alternative format can be
// inferred, the body is translated to match the endpoint using the translator
// registry. The conversion name (e.g. "claude-to-openai") is stored under ContextKey.
func AutoCompat(endpointFormat sdktranslator.Format) gin.HandlerFunc {
	return func(c *gin.Context) {
		rawBody, err := io.ReadAll(c.Request.Body)
		if err != nil || len(rawBody) == 0 {
			c.Next()
			return
		}

		inferred, ok := detectMismatch(rawBody, endpointFormat)
		if !ok || inferred == "" {
			c.Request.Body = io.NopCloser(bytes.NewReader(rawBody))
			c.Next()
			return
		}

		model := gjson.GetBytes(rawBody, "model").String()
		stream := gjson.GetBytes(rawBody, "stream").Bool()
		converted := sdktranslator.TranslateRequest(inferred, endpointFormat, model, rawBody, stream)

		ruleName := fmt.Sprintf("%s-to-%s", inferred, endpointFormat)
		c.Set(ContextKey, ruleName)
		log.Debugf("[Compat] %s", ruleName)

		c.Request.Body = io.NopCloser(bytes.NewReader(converted))
		c.Next()
	}
}
