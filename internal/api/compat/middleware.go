// Package compat provides a request-body compatibility layer as Gin middleware.
//
// When a client sends a request body whose format doesn't match the endpoint it
// hits (e.g. Claude-format body to /v1/chat/completions), the middleware
// transparently converts the body to the endpoint's expected format using the
// translator registry.
//
// The endpoint is always treated as the source of truth: the body adapts, not
// the route.
//
// Detection uses a single-pass JSON scanner (scanner.go) that builds a bitmask
// of present fields, then validates and infers formats via bitwise operations
// (detect.go) — zero allocations on the fast path.
package compat

import (
	"bytes"
	"fmt"
	"io"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// FormatInfoKey is the single Gin context key that carries [logging.FormatInfo].
//
// Both the compat middleware and routing wrappers write to this key;
// the detailed-request-logging middleware reads it.
const FormatInfoKey = "FORMAT_INFO"

// AutoCompat returns a Gin middleware that detects body/endpoint mismatches and
// translates the body when needed.
//
// Body translation happens before c.Next(). After the downstream handler
// returns, the middleware augments the [logging.FormatInfo] already stored at
// [FormatInfoKey] (set by the routing wrapper) with the compat result.
func AutoCompat(endpointFormat sdktranslator.Format) gin.HandlerFunc {
	return func(c *gin.Context) {
		rawBody, err := io.ReadAll(c.Request.Body)
		if err != nil || len(rawBody) == 0 {
			c.Next()
			return
		}

		present := scanTopKeys(rawBody)
		inferred, ok := detectMismatch(present, endpointFormat)

		if ok && inferred == "" {
			c.Request.Body = io.NopCloser(bytes.NewReader(rawBody))
			c.Next()
			return
		}

		if !ok {
			log.Debugf("[Compat] unrecognized body format for endpoint %s", endpointFormat)
			c.Request.Body = io.NopCloser(bytes.NewReader(rawBody))
			c.Next()
			augmentFormatInfo(c, endpointFormat, logging.FormatInfo{
				CompatError: "unrecognized-body-format",
			})
			return
		}

		model := gjson.GetBytes(rawBody, "model").String()
		stream := gjson.GetBytes(rawBody, "stream").Bool()
		converted := sdktranslator.TranslateRequest(inferred, endpointFormat, model, rawBody, stream)

		ruleName := fmt.Sprintf("%s-to-%s", inferred, endpointFormat)
		log.Debugf("[Compat] %s", ruleName)

		c.Request.Body = io.NopCloser(bytes.NewReader(converted))
		c.Next()

		augmentFormatInfo(c, endpointFormat, logging.FormatInfo{
			CompatApplied: true,
			CompatRule:    ruleName,
		})
	}
}

// augmentFormatInfo merges compat fields into the FormatInfo on the Gin context.
// The routing wrapper sets EndpointFormat before this runs; this adds the compat layer's result.
func augmentFormatInfo(c *gin.Context, endpointFormat sdktranslator.Format, patch logging.FormatInfo) {
	if raw, exists := c.Get(FormatInfoKey); exists {
		if fi, ok := raw.(logging.FormatInfo); ok {
			fi.CompatApplied = patch.CompatApplied
			fi.CompatRule = patch.CompatRule
			fi.CompatError = patch.CompatError
			c.Set(FormatInfoKey, fi)
			return
		}
	}
	patch.EndpointFormat = string(endpointFormat)
	c.Set(FormatInfoKey, patch)
}
