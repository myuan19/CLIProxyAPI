package healthcheck

import (
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestBuildProbeRequestUsesResponsesForCodex(t *testing.T) {
	req, opts, err := BuildProbeRequest(&coreauth.Auth{Provider: "codex"}, "gpt-5")
	if err != nil {
		t.Fatalf("BuildProbeRequest returned error: %v", err)
	}

	if opts.SourceFormat != sdktranslator.FormatOpenAIResponse {
		t.Fatalf("expected source format %q, got %q", sdktranslator.FormatOpenAIResponse, opts.SourceFormat)
	}
	if req.Format != sdktranslator.FormatOpenAIResponse {
		t.Fatalf("expected request format %q, got %q", sdktranslator.FormatOpenAIResponse, req.Format)
	}
	if gjson.GetBytes(req.Payload, "input.0.content.0.text").String() != "hi" {
		t.Fatalf("expected responses probe payload to contain input text")
	}
	if gjson.GetBytes(req.Payload, "max_tokens").Exists() {
		t.Fatalf("did not expect max_tokens in responses probe payload")
	}
}

func TestBuildProbeRequestUsesResponsesForCompatProviders(t *testing.T) {
	auth := &coreauth.Auth{
		Provider: "openrouter",
		Attributes: map[string]string{
			"compat_name": "openrouter",
		},
	}

	_, opts, err := BuildProbeRequest(auth, "gpt-4.1")
	if err != nil {
		t.Fatalf("BuildProbeRequest returned error: %v", err)
	}
	if opts.SourceFormat != sdktranslator.FormatOpenAIResponse {
		t.Fatalf("expected source format %q, got %q", sdktranslator.FormatOpenAIResponse, opts.SourceFormat)
	}
}

func TestBuildProbeRequestUsesChatCompletionsForClaude(t *testing.T) {
	req, opts, err := BuildProbeRequest(&coreauth.Auth{Provider: "claude"}, "claude-sonnet")
	if err != nil {
		t.Fatalf("BuildProbeRequest returned error: %v", err)
	}

	if opts.SourceFormat != sdktranslator.FormatOpenAI {
		t.Fatalf("expected source format %q, got %q", sdktranslator.FormatOpenAI, opts.SourceFormat)
	}
	if gjson.GetBytes(req.Payload, "messages.0.content").String() != "hi" {
		t.Fatalf("expected chat probe payload to contain a user message")
	}
	if gjson.GetBytes(req.Payload, "max_tokens").Exists() {
		t.Fatalf("did not expect max_tokens in chat probe payload")
	}
}
