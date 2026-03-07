package executor

import (
	"bytes"
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
	"github.com/router-for-me/CLIProxyAPI/v6/internal/mitm/queue"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const (
	mitmAuthType       = "antigravity"
	mitmRequestTimeout = 5 * time.Minute
	dummyPromptText    = "Say hello."
	lsTriggerModel     = "gemini-2.5-flash"
)

// MITMExecutor proxies requests through a real Language Server binary using
// MITM interception. The flow is:
//
//  1. User request arrives (OpenAI/Anthropic/Gemini format)
//  2. Executor queues the real content in the gRPC interceptor
//  3. Executor sends a "dummy" request to the LS via ConnectRPC
//  4. LS constructs a real gRPC request to Google
//  5. MITM proxy intercepts and replaces the dummy with real content
//  6. MITM injects valid Authorization header
//  7. Google processes the request and responds
//  8. MITM extracts the response and returns it to the user
//
type MITMExecutor struct {
	cfg *config.Config

	mu              sync.Mutex
	engine          *mitm.Engine
	cachedToken     string
	tokenExpiry     time.Time
	currentAuthLabel string
	requestQueue    *queue.Queue
}

func NewMITMExecutor(cfg *config.Config) *MITMExecutor {
	return &MITMExecutor{
		cfg:          cfg,
		requestQueue: queue.New(queue.DefaultConfig()),
	}
}

func (e *MITMExecutor) Identifier() string { return mitmAuthType }

// --- Token management ---

func (e *MITMExecutor) getAccessToken(ctx context.Context, auth *cliproxyauth.Auth) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	authChanged := auth.Label != e.currentAuthLabel
	if !authChanged && e.cachedToken != "" && time.Now().Before(e.tokenExpiry) {
		return e.cachedToken, nil
	}

	if authChanged {
		log.WithField("from", e.currentAuthLabel).WithField("to", auth.Label).Info("mitm executor: account changed, refreshing token")
	}

	token, expiry, err := e.refreshOAuthToken(ctx, auth)
	if err != nil {
		if stored := metaStringValue(auth.Metadata, "access_token"); stored != "" {
			log.WithError(err).Warn("mitm executor: token refresh failed, using stored access_token")
			e.currentAuthLabel = auth.Label
			return stored, nil
		}
		return "", err
	}
	e.cachedToken = token
	e.tokenExpiry = expiry
	e.currentAuthLabel = auth.Label
	return token, nil
}

func (e *MITMExecutor) refreshOAuthToken(ctx context.Context, auth *cliproxyauth.Auth) (string, time.Time, error) {
	refreshToken := metaStringValue(auth.Metadata, "refresh_token")
	if refreshToken == "" {
		return "", time.Time{}, fmt.Errorf("no refresh token available")
	}

	form := url.Values{}
	form.Set("client_id", antigravityClientID)
	form.Set("client_secret", antigravityClientSecret)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://oauth2.googleapis.com/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, err
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return "", time.Time{}, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("token refresh HTTP %d: %s", resp.StatusCode, truncateStr(string(body), 200))
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", time.Time{}, err
	}

	expiry := time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
	return tokenResp.AccessToken, expiry, nil
}

// --- MITM Engine management ---

func (e *MITMExecutor) getOrStartEngine(ctx context.Context, auth *cliproxyauth.Auth) (*mitm.Engine, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.engine != nil && e.engine.IsRunning() {
		token, err := e.getAccessTokenLocked(ctx, auth)
		if err == nil && token != "" {
			e.engine.SetAccessToken(token)
		}
		return e.engine, nil
	}

	engineCfg := e.buildEngineConfig(auth)

	token, err := e.getAccessTokenLocked(ctx, auth)
	if err != nil {
		log.WithError(err).Warn("mitm executor: failed to get initial access token")
	} else {
		engineCfg.AccessToken = token
	}

	engine, err := mitm.NewEngine(engineCfg)
	if err != nil {
		return nil, fmt.Errorf("mitm executor: create engine: %w", err)
	}

	if err := engine.Start(context.Background()); err != nil {
		return nil, fmt.Errorf("mitm executor: start engine: %w", err)
	}

	e.engine = engine
	return engine, nil
}

func (e *MITMExecutor) getAccessTokenLocked(ctx context.Context, auth *cliproxyauth.Auth) (string, error) {
	authChanged := auth.Label != e.currentAuthLabel
	if !authChanged && e.cachedToken != "" && time.Now().Before(e.tokenExpiry) {
		return e.cachedToken, nil
	}
	token, expiry, err := e.refreshOAuthToken(ctx, auth)
	if err != nil {
		if stored := metaStringValue(auth.Metadata, "access_token"); stored != "" {
			e.currentAuthLabel = auth.Label
			return stored, nil
		}
		return "", err
	}
	e.cachedToken = token
	e.tokenExpiry = expiry
	e.currentAuthLabel = auth.Label
	return token, nil
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

		if v := mitmAttr(auth, "ls_path"); v != "" {
			cfg.LSPath = v
		}
		if v := mitmAttr(auth, "h2_profile"); v != "" {
			cfg.H2Profile = v
		}
		if v := mitmAttr(auth, "system_mode"); v != "" {
			cfg.SystemMode = v
		}
		if v := mitmAttr(auth, "data_dir"); v != "" {
			cfg.DataDir = v
		}
		if v := mitmAttr(auth, "app_root"); v != "" {
			cfg.AppRoot = v
		}
	}

	if cfg.DataDir == "" {
		if e.cfg != nil && e.cfg.AuthDir != "" {
			cfg.DataDir = filepath.Join(filepath.Dir(e.cfg.AuthDir), "mitm-data")
		}
	}

	return cfg
}

// --- Execute ---

func (e *MITMExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	var result cliproxyexecutor.Response
	var execErr error

	err := e.requestQueue.Submit(ctx, func(qCtx context.Context) error {
		result, execErr = e.executeInner(qCtx, auth, req, opts)
		return execErr
	})
	if err != nil && execErr == nil {
		return cliproxyexecutor.Response{}, err
	}
	return result, execErr
}

func (e *MITMExecutor) executeInner(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	engine, err := e.getOrStartEngine(ctx, auth)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("mitm executor: engine unavailable: %w", err)
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
	go e.triggerLSRequest(ctx, engine, model)

	timeoutCtx, cancel := context.WithTimeout(ctx, mitmRequestTimeout)
	defer cancel()

	select {
	case resp := <-pending.ResponseCh:
		if resp.Error != nil {
			return cliproxyexecutor.Response{}, resp.Error
		}
		log.WithField("text_len", len(resp.Text)).WithField("finish", resp.FinishReason).Info("mitm executor: got intercepted response")

		payload := buildAntigravityJSONResponse(resp, model)
		responseStr := sdktranslator.TranslateNonStream(ctx, to, from, baseModel, req.Payload, translated, payload, nil)
		if responseStr == "" {
			return cliproxyexecutor.Response{Payload: payload}, nil
		}
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
		return nil, fmt.Errorf("mitm executor: engine unavailable: %w", err)
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
	go e.triggerLSRequest(ctx, engine, model)

	outCh := make(chan cliproxyexecutor.StreamChunk, 100)
	translatedCapture := translated

	go func() {
		defer close(outCh)

		for {
			select {
			case chunk, ok := <-pending.StreamCh:
				if !ok {
					return
				}
				if chunk.Error != nil {
					outCh <- cliproxyexecutor.StreamChunk{Err: chunk.Error}
					return
				}

				chunkPayload := buildAntigravityStreamChunk(chunk, model)
				var param any
				parts := sdktranslator.TranslateStream(ctx, to, from, baseModel, req.Payload, translatedCapture, chunkPayload, &param)
				for _, part := range parts {
					if part != "" {
						outCh <- cliproxyexecutor.StreamChunk{Payload: []byte(part)}
					}
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	return &cliproxyexecutor.StreamResult{
		Headers: http.Header{"Content-Type": {"text/event-stream"}},
		Chunks:  outCh,
	}, nil
}

func (e *MITMExecutor) triggerLSRequest(ctx context.Context, engine *mitm.Engine, model string) {
	interceptor := engine.Interceptor()

	client := engine.WaitForLSClient(ctx, 90*time.Second)
	if client == nil {
		log.Error("mitm executor: LS client not ready after waiting")
		interceptor.CancelPending(fmt.Errorf("LS client not ready"))
		return
	}

	log.WithField("model", model).Info("mitm executor: triggering LS request")

	// Query available models from LS for diagnostics.
	if modelConfigs, err := client.GetCascadeModelConfigs(ctx); err == nil {
		snippet := string(modelConfigs)
		if len(snippet) > 1000 {
			snippet = snippet[:1000] + "..."
		}
		log.WithField("models", snippet).Info("mitm executor: LS model configs")
	} else {
		log.WithError(err).Warn("mitm executor: failed to get LS model configs")
	}

	cascadeID, err := client.StartCascade(ctx)
	if err != nil {
		log.WithError(err).Error("mitm executor: failed to start cascade")
		interceptor.CancelPending(fmt.Errorf("failed to start cascade: %w", err))
		return
	}
	log.WithField("cascadeId", cascadeID).Info("mitm executor: cascade started")

	// Always use a well-known model to trigger the LS; the interceptor
	// will replace the model in the outgoing gRPC request with the real one.
	triggerModel := lsTriggerModel
	_, err = client.SendCascadeMessage(ctx, cascadeID, dummyPromptText, triggerModel)
	if err != nil {
		log.WithError(err).Error("mitm executor: failed to send cascade message")
		interceptor.CancelPending(fmt.Errorf("failed to send cascade message: %w", err))
	}
}

// --- Refresh / CountTokens / HttpRequest / Close ---

func (e *MITMExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if auth == nil {
		return auth, nil
	}
	token, expiry, err := e.refreshOAuthToken(ctx, auth)
	if err != nil {
		return auth, err
	}
	e.mu.Lock()
	e.cachedToken = token
	e.tokenExpiry = expiry
	if e.engine != nil {
		e.engine.SetAccessToken(token)
	}
	e.mu.Unlock()

	updated := auth.Clone()
	if updated.Metadata == nil {
		updated.Metadata = make(map[string]interface{})
	}
	updated.Metadata["access_token"] = token
	updated.Metadata["refresh_token"] = metaStringValue(auth.Metadata, "refresh_token")
	updated.Metadata["expires_in"] = int(time.Until(expiry).Seconds())
	updated.Metadata["timestamp"] = time.Now().UnixMilli()
	updated.Metadata["expired"] = expiry.Format(time.RFC3339)
	return updated, nil
}

func (e *MITMExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	token, err := e.getAccessToken(ctx, auth)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	from := opts.SourceFormat
	to := sdktranslator.FromString("antigravity")
	payload := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)
	payload = deleteJSONField(payload, "model")
	reqURL := antigravityBaseURLProd + antigravityCountTokensPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(payload))
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("User-Agent", resolveUserAgent(auth))
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

func (e *MITMExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("mitm executor: request is nil")
	}
	token, err := e.getAccessToken(ctx, auth)
	if err != nil {
		return nil, err
	}
	httpReq := req.WithContext(ctx)
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

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
	contents := gjson.GetBytes(translated, "request.contents")
	if !contents.Exists() {
		contents = gjson.GetBytes(translated, "contents")
	}
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
			messages = append(messages, mitmgrpc.Message{Role: role, Content: text})
		}
	}
	return messages
}

func extractSystemPrompt(translated []byte) string {
	si := gjson.GetBytes(translated, "request.systemInstruction")
	if !si.Exists() {
		si = gjson.GetBytes(translated, "systemInstruction")
	}
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
					"parts": []map[string]interface{}{{"text": resp.Text}},
					"role":  "model",
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
					"parts": []map[string]interface{}{{"text": chunk.Text}},
					"role":  "model",
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

func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func mitmAttr(auth *cliproxyauth.Auth, key string) string {
	if auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes[key]); v != "" {
			return v
		}
	}
	return metaStringValue(auth.Metadata, key)
}
