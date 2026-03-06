package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/mitm"
	mitmgrpc "github.com/router-for-me/CLIProxyAPI/v6/internal/mitm/grpc"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

const (
	mitmAuthType      = "antigravity-mitm"
	mitmRequestTimeout = 5 * time.Minute
)

// MITMExecutor proxies requests through a real Language Server binary using
// MITM interception. This achieves near-perfect client impersonation because
// Google receives natively-generated gRPC requests from the real LS.
type MITMExecutor struct {
	cfg *config.Config

	mu     sync.Mutex
	engine *mitm.Engine
}

// NewMITMExecutor creates a new MITM executor instance.
func NewMITMExecutor(cfg *config.Config) *MITMExecutor {
	return &MITMExecutor{cfg: cfg}
}

func (e *MITMExecutor) Identifier() string { return mitmAuthType }

func (e *MITMExecutor) getOrStartEngine(ctx context.Context, auth *cliproxyauth.Auth) (*mitm.Engine, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.engine != nil && e.engine.IsRunning() {
		return e.engine, nil
	}

	engineCfg := e.buildEngineConfig(auth)
	engine, err := mitm.NewEngine(engineCfg)
	if err != nil {
		return nil, fmt.Errorf("mitm executor: create engine: %w", err)
	}

	if err := engine.Start(ctx); err != nil {
		return nil, fmt.Errorf("mitm executor: start engine: %w", err)
	}

	e.engine = engine
	return engine, nil
}

func (e *MITMExecutor) buildEngineConfig(auth *cliproxyauth.Auth) mitm.EngineConfig {
	cfg := mitm.EngineConfig{
		TargetHost: "cloudcode-pa.googleapis.com",
		H2Profile:  "chromium",
		SystemMode: "native",
	}

	if auth != nil {
		cfg.RefreshToken = metaStringValue(auth.Metadata, "refresh_token")
		cfg.AccountEmail = auth.Label

		if v := strings.TrimSpace(auth.Attributes["ls_path"]); v != "" {
			cfg.LSPath = v
		}
		if v := strings.TrimSpace(auth.Attributes["h2_profile"]); v != "" {
			cfg.H2Profile = v
		}
		if v := strings.TrimSpace(auth.Attributes["system_mode"]); v != "" {
			cfg.SystemMode = v
		}
		if v := strings.TrimSpace(auth.Attributes["data_dir"]); v != "" {
			cfg.DataDir = v
		}
	}

	if cfg.DataDir == "" {
		if e.cfg != nil && e.cfg.AuthDir != "" {
			cfg.DataDir = filepath.Join(filepath.Dir(e.cfg.AuthDir), "mitm-data")
		}
	}

	return cfg
}

// Execute performs a non-streaming request through the MITM engine.
func (e *MITMExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	engine, err := e.getOrStartEngine(ctx, auth)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	from := opts.SourceFormat
	to := sdktranslator.FromString("antigravity")
	translated := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)

	translated, err = thinking.ApplyThinking(translated, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	messages := extractMessages(translated)
	systemPrompt := extractSystemPrompt(translated)
	model := extractModel(translated, baseModel)

	pending := &mitmgrpc.PendingRequest{
		Model:        model,
		Messages:     messages,
		SystemPrompt: systemPrompt,
		Stream:       false,
		ResponseCh:   make(chan *mitmgrpc.InterceptedResponse, 1),
		DoneCh:       make(chan struct{}),
	}

	engine.Interceptor().InjectRequest(pending)

	timeoutCtx, cancel := context.WithTimeout(ctx, mitmRequestTimeout)
	defer cancel()

	select {
	case resp := <-pending.ResponseCh:
		if resp.Error != nil {
			return cliproxyexecutor.Response{}, resp.Error
		}
		payload := buildAntigravityJSONResponse(resp, model)
		responseStr := sdktranslator.TranslateNonStream(ctx, to, from, baseModel, req.Payload, translated, payload, nil)
		return cliproxyexecutor.Response{Payload: []byte(responseStr)}, nil

	case <-timeoutCtx.Done():
		return cliproxyexecutor.Response{}, fmt.Errorf("mitm executor: request timed out")
	}
}

// ExecuteStream performs a streaming request through the MITM engine.
func (e *MITMExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	engine, err := e.getOrStartEngine(ctx, auth)
	if err != nil {
		return nil, err
	}

	from := opts.SourceFormat
	to := sdktranslator.FromString("antigravity")
	translated := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, true)

	translated, err = thinking.ApplyThinking(translated, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}

	messages := extractMessages(translated)
	systemPrompt := extractSystemPrompt(translated)
	model := extractModel(translated, baseModel)

	pending := &mitmgrpc.PendingRequest{
		Model:        model,
		Messages:     messages,
		SystemPrompt: systemPrompt,
		Stream:       true,
		StreamCh:     make(chan *mitmgrpc.StreamChunk, 100),
		DoneCh:       make(chan struct{}),
	}

	engine.Interceptor().InjectRequest(pending)

	outCh := make(chan cliproxyexecutor.StreamChunk, 100)

	translatedCapture := translated
	go func() {
		defer close(outCh)

		for chunk := range pending.StreamCh {
			if chunk.Error != nil {
				outCh <- cliproxyexecutor.StreamChunk{Err: chunk.Error}
				return
			}

			chunkPayload := buildAntigravityStreamChunk(chunk, model)
			parts := sdktranslator.TranslateStream(ctx, to, from, baseModel, req.Payload, translatedCapture, chunkPayload, nil)
			for _, part := range parts {
				if part != "" {
					outCh <- cliproxyexecutor.StreamChunk{Payload: []byte(part)}
				}
			}
		}
	}()

	return &cliproxyexecutor.StreamResult{
		Headers: http.Header{
			"Content-Type": {"text/event-stream"},
		},
		Chunks: outCh,
	}, nil
}

// Refresh refreshes the OAuth token for the MITM account.
// The MITM engine itself handles token management for the LS,
// but we still refresh the CLIProxyAPI-level auth state.
func (e *MITMExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if auth == nil {
		return auth, nil
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

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://oauth2.googleapis.com/token", strings.NewReader(form.Encode()))
	if err != nil {
		return auth, err
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpReq.Header.Set("User-Agent", defaultAntigravityAgent)

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		return auth, err
	}
	defer httpResp.Body.Close()

	bodyBytes, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return auth, err
	}

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return auth, statusErr{code: httpResp.StatusCode, msg: string(bodyBytes)}
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(bodyBytes, &tokenResp); err != nil {
		return auth, fmt.Errorf("mitm executor: parse token response: %w", err)
	}

	updated := auth.Clone()
	updated.Token = tokenResp.AccessToken
	if updated.Metadata == nil {
		updated.Metadata = make(map[string]interface{})
	}
	updated.Metadata["refresh_token"] = refreshToken
	updated.Metadata["expires_in"] = tokenResp.ExpiresIn
	updated.ExpiresAt = time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)

	return updated, nil
}

// CountTokens performs a token count request.
// In MITM mode, this falls through to the standard Antigravity REST API
// since token counting doesn't benefit from MITM.
func (e *MITMExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	token := auth.Token
	if token == "" {
		updated, err := e.Refresh(ctx, auth)
		if err != nil {
			return cliproxyexecutor.Response{}, err
		}
		token = updated.Token
	}

	from := opts.SourceFormat
	to := sdktranslator.FromString("antigravity")
	payload := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)
	payload = deleteJSONField(payload, "model")

	reqURL := antigravityBaseURLProd + antigravityCountTokensPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(string(payload)))
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("User-Agent", defaultAntigravityAgent)
	httpReq.Header.Set("X-Goog-Api-Client", antigravityXGoogAPIClient)

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	defer httpResp.Body.Close()

	bodyBytes, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return cliproxyexecutor.Response{}, statusErr{code: httpResp.StatusCode, msg: string(bodyBytes)}
	}

	return cliproxyexecutor.Response{Payload: bodyBytes}, nil
}

// HttpRequest injects credentials and executes an arbitrary HTTP request.
func (e *MITMExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("mitm executor: request is nil")
	}

	token := auth.Token
	if token == "" {
		updated, err := e.Refresh(ctx, auth)
		if err != nil {
			return nil, err
		}
		token = updated.Token
	}

	httpReq := req.WithContext(ctx)
	httpReq.Header.Set("Authorization", "Bearer "+token)

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

// CloseExecutionSession stops the MITM engine if no more sessions reference it.
func (e *MITMExecutor) CloseExecutionSession(sessionID string) {
	if sessionID == cliproxyauth.CloseAllExecutionSessionsID {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.engine != nil {
			e.engine.Stop()
			e.engine = nil
		}
	}
}

// --- helpers ---

func extractMessages(translated []byte) []mitmgrpc.Message {
	var messages []mitmgrpc.Message

	contents := gjson.GetBytes(translated, "contents")
	if !contents.Exists() {
		return messages
	}

	for _, c := range contents.Array() {
		role := c.Get("role").String()
		if role == "" {
			role = "user"
		}

		var text string
		parts := c.Get("parts")
		if parts.Exists() {
			for _, p := range parts.Array() {
				if t := p.Get("text"); t.Exists() {
					text += t.String()
				}
			}
		}

		if text != "" {
			messages = append(messages, mitmgrpc.Message{
				Role:    role,
				Content: text,
			})
		}
	}

	return messages
}

func extractSystemPrompt(translated []byte) string {
	si := gjson.GetBytes(translated, "systemInstruction")
	if !si.Exists() {
		return ""
	}
	parts := si.Get("parts")
	if !parts.Exists() {
		return ""
	}
	var text string
	for _, p := range parts.Array() {
		if t := p.Get("text"); t.Exists() {
			text += t.String()
		}
	}
	return text
}

func extractModel(translated []byte, fallback string) string {
	m := gjson.GetBytes(translated, "model").String()
	if m != "" {
		return m
	}
	return fallback
}

func buildAntigravityJSONResponse(resp *mitmgrpc.InterceptedResponse, model string) []byte {
	result := map[string]interface{}{
		"candidates": []map[string]interface{}{
			{
				"content": map[string]interface{}{
					"parts": []map[string]interface{}{
						{"text": resp.Text},
					},
					"role": "model",
				},
				"finishReason": resp.FinishReason,
				"index":        0,
			},
		},
		"modelVersion": model,
	}

	if resp.Usage != nil {
		result["usageMetadata"] = map[string]interface{}{
			"promptTokenCount":     resp.Usage.PromptTokens,
			"candidatesTokenCount": resp.Usage.CompletionTokens,
			"totalTokenCount":      resp.Usage.TotalTokens,
		}
	}

	data, _ := json.Marshal(result)
	return data
}

func buildAntigravityStreamChunk(chunk *mitmgrpc.StreamChunk, model string) []byte {
	result := map[string]interface{}{
		"candidates": []map[string]interface{}{
			{
				"content": map[string]interface{}{
					"parts": []map[string]interface{}{
						{"text": chunk.Text},
					},
					"role": "model",
				},
				"index": 0,
			},
		},
		"modelVersion": model,
	}

	if chunk.FinishReason != "" {
		candidates := result["candidates"].([]map[string]interface{})
		candidates[0]["finishReason"] = chunk.FinishReason
	}

	data, _ := json.Marshal(result)
	return data
}
