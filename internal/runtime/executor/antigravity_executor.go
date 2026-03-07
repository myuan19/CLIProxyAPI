// Package executor provides runtime execution capabilities for various AI service providers.
// This file provides Antigravity OAuth token management, model fetching, and shared
// utility functions used by the MITM executor.
package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	goruntime "runtime"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v6/sdk/auth"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const (
	antigravityBaseURLDaily        = "https://daily-cloudcode-pa.googleapis.com"
	antigravitySandboxBaseURLDaily = "https://daily-cloudcode-pa.sandbox.googleapis.com"
	antigravityBaseURLProd         = "https://cloudcode-pa.googleapis.com"
	antigravityCountTokensPath     = "/v1internal:countTokens"
	antigravityModelsPath          = "/v1internal:fetchAvailableModels"
	antigravityClientID            = "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com"
	antigravityClientSecret        = "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"
	defaultAntigravityAgent        = "antigravity/1.107.0"
	antigravityXGoogAPIClient      = "gl-node/22.20.0 grpc-node/1.12.6 gax-node/4.8.0 gapic/1.107.0 antigravity/1.107.0"
	antigravityClientMetadata      = `ideType=ANTIGRAVITY,platform=PLATFORM_UNSPECIFIED,pluginType=GEMINI`
	antigravityAuthType            = "antigravity"
	refreshSkew                    = 3000 * time.Second
)

// AntigravityExecutor holds Antigravity configuration and provides OAuth
// token management used by FetchAntigravityModels and the MITM executor.
type AntigravityExecutor struct {
	cfg *config.Config
}

// FetchAntigravityModels retrieves available models using the supplied auth.
func FetchAntigravityModels(ctx context.Context, auth *cliproxyauth.Auth, cfg *config.Config) []*registry.ModelInfo {
	exec := &AntigravityExecutor{cfg: cfg}
	token, updatedAuth, errToken := exec.ensureAccessToken(ctx, auth)
	if errToken != nil || token == "" {
		if stored := metaStringValue(auth.Metadata, "access_token"); stored != "" {
			log.WithError(errToken).Warn("antigravity: token refresh failed, falling back to stored access_token for model fetch")
			token = stored
		} else {
			return nil
		}
	}
	if updatedAuth != nil {
		auth = updatedAuth
	}

	baseURLs := antigravityBaseURLFallbackOrder(auth)
	httpClient := newProxyAwareHTTPClient(ctx, cfg, auth, 0)

	for idx, baseURL := range baseURLs {
		modelsURL := baseURL + antigravityModelsPath
		httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, modelsURL, bytes.NewReader([]byte(`{}`)))
		if errReq != nil {
			return nil
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+token)
		httpReq.Header.Set("User-Agent", resolveUserAgent(auth))
		httpReq.Header.Set("X-Goog-Api-Client", antigravityXGoogAPIClient)
		httpReq.Header.Set("Client-Metadata", antigravityClientMetadata)
		if host := resolveHost(baseURL); host != "" {
			httpReq.Host = host
		}

		httpResp, errDo := httpClient.Do(httpReq)
		if errDo != nil {
			if errors.Is(errDo, context.Canceled) || errors.Is(errDo, context.DeadlineExceeded) {
				return nil
			}
			if idx+1 < len(baseURLs) {
				log.Debugf("antigravity executor: models request error on base url %s, retrying with fallback base url: %s", baseURL, baseURLs[idx+1])
				continue
			}
			return nil
		}

		bodyBytes, errRead := io.ReadAll(httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("antigravity executor: close response body error: %v", errClose)
		}
		if errRead != nil {
			if idx+1 < len(baseURLs) {
				log.Debugf("antigravity executor: models read error on base url %s, retrying with fallback base url: %s", baseURL, baseURLs[idx+1])
				continue
			}
			return nil
		}
		if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
			if httpResp.StatusCode == http.StatusTooManyRequests && idx+1 < len(baseURLs) {
				log.Debugf("antigravity executor: models request rate limited on base url %s, retrying with fallback base url: %s", baseURL, baseURLs[idx+1])
				continue
			}
			return nil
		}

		result := gjson.GetBytes(bodyBytes, "models")
		if !result.Exists() {
			return nil
		}

		now := time.Now().Unix()
		modelConfig := registry.GetAntigravityModelConfig()
		models := make([]*registry.ModelInfo, 0, len(result.Map()))
		for originalName, modelData := range result.Map() {
			modelID := strings.TrimSpace(originalName)
			if modelID == "" {
				continue
			}
			switch modelID {
			case "chat_20706", "chat_23310", "gemini-2.5-flash-thinking", "gemini-3-pro-low", "gemini-2.5-pro":
				continue
			}
			modelCfg := modelConfig[modelID]

			displayName := modelData.Get("displayName").String()
			if displayName == "" {
				displayName = modelID
			}

			modelInfo := &registry.ModelInfo{
				ID:          modelID,
				Name:        modelID,
				Description: displayName,
				DisplayName: displayName,
				Version:     modelID,
				Object:      "model",
				Created:     now,
				OwnedBy:     antigravityAuthType,
				Type:        antigravityAuthType,
			}
			if modelCfg != nil {
				if modelCfg.Thinking != nil {
					modelInfo.Thinking = modelCfg.Thinking
				}
				if modelCfg.MaxCompletionTokens > 0 {
					modelInfo.MaxCompletionTokens = modelCfg.MaxCompletionTokens
				}
			}
			models = append(models, modelInfo)
		}
		return models
	}
	return nil
}

func (e *AntigravityExecutor) ensureAccessToken(ctx context.Context, auth *cliproxyauth.Auth) (string, *cliproxyauth.Auth, error) {
	if auth == nil {
		return "", nil, statusErr{code: http.StatusUnauthorized, msg: "missing auth"}
	}
	accessToken := metaStringValue(auth.Metadata, "access_token")
	expiry := tokenExpiry(auth.Metadata)
	if accessToken != "" && expiry.After(time.Now().Add(refreshSkew)) {
		return accessToken, nil, nil
	}
	refreshCtx := context.Background()
	if ctx != nil {
		if rt, ok := ctx.Value("cliproxy.roundtripper").(http.RoundTripper); ok && rt != nil {
			refreshCtx = context.WithValue(refreshCtx, "cliproxy.roundtripper", rt)
		}
	}
	updated, errRefresh := e.refreshToken(refreshCtx, auth.Clone())
	if errRefresh != nil {
		return "", nil, errRefresh
	}
	return metaStringValue(updated.Metadata, "access_token"), updated, nil
}

func (e *AntigravityExecutor) refreshToken(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if auth == nil {
		return nil, statusErr{code: http.StatusUnauthorized, msg: "missing auth"}
	}
	refreshToken := metaStringValue(auth.Metadata, "refresh_token")
	if refreshToken == "" {
		return auth, statusErr{code: http.StatusUnauthorized, msg: "missing refresh token"}
	}

	form := url.Values{}
	form.Set("client_id", antigravityClientID)
	form.Set("client_secret", antigravityClientSecret)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)

	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, "https://oauth2.googleapis.com/token", strings.NewReader(form.Encode()))
	if errReq != nil {
		return auth, errReq
	}
	httpReq.Header.Set("Host", "oauth2.googleapis.com")
	httpReq.Header.Set("User-Agent", defaultAntigravityAgent+" "+goRuntimePlatform())
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		return auth, errDo
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("antigravity executor: close response body error: %v", errClose)
		}
	}()

	bodyBytes, errRead := io.ReadAll(httpResp.Body)
	if errRead != nil {
		return auth, errRead
	}

	if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
		sErr := statusErr{code: httpResp.StatusCode, msg: string(bodyBytes)}
		if httpResp.StatusCode == http.StatusTooManyRequests {
			if retryAfter, parseErr := parseRetryDelay(bodyBytes); parseErr == nil && retryAfter != nil {
				sErr.retryAfter = retryAfter
			}
		}
		return auth, sErr
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		TokenType    string `json:"token_type"`
	}
	if errUnmarshal := json.Unmarshal(bodyBytes, &tokenResp); errUnmarshal != nil {
		return auth, errUnmarshal
	}

	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["access_token"] = tokenResp.AccessToken
	if tokenResp.RefreshToken != "" {
		auth.Metadata["refresh_token"] = tokenResp.RefreshToken
	}
	auth.Metadata["expires_in"] = tokenResp.ExpiresIn
	now := time.Now()
	auth.Metadata["timestamp"] = now.UnixMilli()
	auth.Metadata["expired"] = now.Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339)
	auth.Metadata["type"] = antigravityAuthType
	if errProject := e.ensureAntigravityProjectID(ctx, auth, tokenResp.AccessToken); errProject != nil {
		log.Warnf("antigravity executor: ensure project id failed: %v", errProject)
	}
	return auth, nil
}

func (e *AntigravityExecutor) ensureAntigravityProjectID(ctx context.Context, auth *cliproxyauth.Auth, accessToken string) error {
	if auth == nil {
		return nil
	}

	if auth.Metadata["project_id"] != nil {
		return nil
	}

	token := strings.TrimSpace(accessToken)
	if token == "" {
		token = metaStringValue(auth.Metadata, "access_token")
	}
	if token == "" {
		return nil
	}

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	projectID, errFetch := sdkAuth.FetchAntigravityProjectID(ctx, token, httpClient)
	if errFetch != nil {
		return errFetch
	}
	if strings.TrimSpace(projectID) == "" {
		return nil
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["project_id"] = strings.TrimSpace(projectID)

	return nil
}

// --- shared utility functions ---

func tokenExpiry(metadata map[string]any) time.Time {
	if metadata == nil {
		return time.Time{}
	}
	if expStr, ok := metadata["expired"].(string); ok {
		expStr = strings.TrimSpace(expStr)
		if expStr != "" {
			if parsed, errParse := time.Parse(time.RFC3339, expStr); errParse == nil {
				return parsed
			}
		}
	}
	expiresIn, hasExpires := int64Value(metadata["expires_in"])
	tsMs, hasTimestamp := int64Value(metadata["timestamp"])
	if hasExpires && hasTimestamp {
		return time.Unix(0, tsMs*int64(time.Millisecond)).Add(time.Duration(expiresIn) * time.Second)
	}
	return time.Time{}
}

func metaStringValue(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	if v, ok := metadata[key]; ok {
		switch typed := v.(type) {
		case string:
			return strings.TrimSpace(typed)
		case []byte:
			return strings.TrimSpace(string(typed))
		}
	}
	return ""
}

func int64Value(value any) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	case float64:
		return int64(typed), true
	case json.Number:
		if i, errParse := typed.Int64(); errParse == nil {
			return i, true
		}
	case string:
		if strings.TrimSpace(typed) == "" {
			return 0, false
		}
		if i, errParse := strconv.ParseInt(strings.TrimSpace(typed), 10, 64); errParse == nil {
			return i, true
		}
	}
	return 0, false
}

func resolveHost(base string) string {
	parsed, errParse := url.Parse(base)
	if errParse != nil {
		return ""
	}
	if parsed.Host != "" {
		return parsed.Host
	}
	return strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
}

func resolveUserAgent(auth *cliproxyauth.Auth) string {
	if auth != nil {
		if auth.Attributes != nil {
			if ua := strings.TrimSpace(auth.Attributes["user_agent"]); ua != "" {
				return ua
			}
		}
		if auth.Metadata != nil {
			if ua, ok := auth.Metadata["user_agent"].(string); ok && strings.TrimSpace(ua) != "" {
				return strings.TrimSpace(ua)
			}
		}
	}
	return defaultAntigravityAgent + " " + goRuntimePlatform()
}

func goRuntimePlatform() string {
	os := goruntime.GOOS
	arch := goruntime.GOARCH
	switch os {
	case "windows":
		os = "win32"
	case "darwin":
		os = "darwin"
	}
	switch arch {
	case "amd64":
		arch = "x64"
	case "arm64":
		arch = "arm64"
	}
	return os + "/" + arch
}

func antigravityBaseURLFallbackOrder(auth *cliproxyauth.Auth) []string {
	if base := resolveCustomAntigravityBaseURL(auth); base != "" {
		return []string{base}
	}
	return []string{
		antigravityBaseURLProd,
		antigravityBaseURLDaily,
		antigravitySandboxBaseURLDaily,
	}
}

func resolveCustomAntigravityBaseURL(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["base_url"]); v != "" {
			return strings.TrimSuffix(v, "/")
		}
	}
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["base_url"].(string); ok {
			v = strings.TrimSpace(v)
			if v != "" {
				return strings.TrimSuffix(v, "/")
			}
		}
	}
	return ""
}
