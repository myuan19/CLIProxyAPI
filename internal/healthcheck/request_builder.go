package healthcheck

import (
	"encoding/json"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

// BuildProbeRequest creates a provider-aware health-check request that follows
// the same translator entry format as real traffic as closely as possible.
func BuildProbeRequest(auth *coreauth.Auth, model string) (cliproxyexecutor.Request, cliproxyexecutor.Options, error) {
	sourceFormat := preferredSourceFormat(auth)

	payload, err := buildProbePayload(sourceFormat, model)
	if err != nil {
		return cliproxyexecutor.Request{}, cliproxyexecutor.Options{}, err
	}

	req := cliproxyexecutor.Request{
		Model:   model,
		Payload: payload,
		Format:  sourceFormat,
	}
	opts := cliproxyexecutor.Options{
		Stream:          true,
		SourceFormat:    sourceFormat,
		OriginalRequest: payload,
	}
	return req, opts, nil
}

func preferredSourceFormat(auth *coreauth.Auth) sdktranslator.Format {
	if auth != nil && auth.Attributes != nil {
		if compatName := strings.TrimSpace(auth.Attributes["compat_name"]); compatName != "" {
			return sdktranslator.FormatOpenAIResponse
		}
	}

	switch strings.ToLower(strings.TrimSpace(authProvider(auth))) {
	case "codex", "openai", "antigravity":
		return sdktranslator.FormatOpenAIResponse
	default:
		return sdktranslator.FormatOpenAI
	}
}

func authProvider(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	return auth.Provider
}

func buildProbePayload(sourceFormat sdktranslator.Format, model string) ([]byte, error) {
	switch sourceFormat {
	case sdktranslator.FormatOpenAIResponse:
		return json.Marshal(map[string]any{
			"model": model,
			"input": []map[string]any{
				{
					"type": "message",
					"role": "user",
					"content": []map[string]any{
						{
							"type": "input_text",
							"text": "hi",
						},
					},
				},
			},
			"stream": true,
		})
	default:
		return json.Marshal(map[string]any{
			"model": model,
			"messages": []map[string]any{
				{
					"role":    "user",
					"content": "hi",
				},
			},
			"stream": true,
		})
	}
}
