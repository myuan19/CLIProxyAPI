package compat

import (
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

// ── Field bits ──────────────────────────────────────────────────────────
// Each distinguishing top-level JSON field is assigned a unique bit.
// Fields that don't help distinguish formats (model, stream, temperature …)
// are intentionally omitted — they are invisible to matching.

const (
	bMessages        uint16 = 1 << iota // OpenAI, Claude
	bInput                              // OpenAI-Response, Codex
	bContents                           // Gemini
	bRequest                            // GeminiCLI, Antigravity
	bSystem                             // Claude
	bStopSequences                      // Claude
	bThinking                           // Claude
	bInstructions                       // OpenAI-Response, Codex
	bUserAgent                          // Antigravity
	bRequestType                        // Antigravity
	bRequestContents                    // GeminiCLI, Antigravity (nested: request.contents)
)

// ── Per-format masks ────────────────────────────────────────────────────
// have:    bits that MUST be set      (field must exist)
// notHave: bits that MUST NOT be set  (field must not exist)
// Bits in neither mask are "don't care".

type formatMask struct {
	have    uint16
	notHave uint16
}

var formatMasks = map[sdktranslator.Format]formatMask{
	sdktranslator.FormatOpenAI: {
		have:    bMessages,
		notHave: bSystem | bStopSequences | bThinking | bContents | bInput | bInstructions | bRequest,
	},
	sdktranslator.FormatClaude: {
		have:    bMessages,
		notHave: bContents | bInput | bInstructions | bRequest,
	},
	sdktranslator.FormatOpenAIResponse: {
		have:    bInput,
		notHave: bMessages | bContents | bRequest,
	},
	sdktranslator.FormatGemini: {
		have:    bContents,
		notHave: bMessages | bInput | bRequest,
	},
	sdktranslator.FormatGeminiCLI: {
		have:    bRequest | bRequestContents,
		notHave: bUserAgent | bRequestType | bMessages | bContents | bInput,
	},
	sdktranslator.FormatAntigravity: {
		have:    bRequest,
		notHave: bMessages | bContents | bInput,
	},
	sdktranslator.FormatCodex: {
		have:    bInput,
		notHave: bMessages | bContents | bRequest,
	},
}

// fmtPriority defines the order in which formats are tried during inference.
// When multiple formats match the same body, the first match wins.
var fmtPriority = [...]sdktranslator.Format{
	sdktranslator.FormatOpenAI,
	sdktranslator.FormatClaude,
	sdktranslator.FormatOpenAIResponse,
	sdktranslator.FormatGemini,
	sdktranslator.FormatCodex,
	sdktranslator.FormatGeminiCLI,
	sdktranslator.FormatAntigravity,
}

// matchesMask reports whether present satisfies the given format mask.
func matchesMask(present uint16, mask formatMask) bool {
	return (present&mask.have) == mask.have && (present&mask.notHave) == 0
}

// detectMismatch checks whether the scanned field bitmask matches endpointFormat.
//
//	("", true)       — body matches the endpoint, no conversion needed
//	(inferred, true) — body matches a different format, conversion needed
//	("", false)      — no format matched, pass through unchanged
func detectMismatch(present uint16, endpointFormat sdktranslator.Format) (sdktranslator.Format, bool) {
	epMask, ok := formatMasks[endpointFormat]
	if !ok {
		return "", true
	}

	if matchesMask(present, epMask) {
		return "", true
	}

	for _, f := range fmtPriority {
		if f == endpointFormat {
			continue
		}
		if m, ok := formatMasks[f]; ok && matchesMask(present, m) {
			return f, true
		}
	}

	return "", false
}
