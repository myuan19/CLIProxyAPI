// Package mitm provides a MITM (Man-in-the-Middle) engine that intercepts
// Language Server traffic to Google's API, enabling request injection and
// response extraction while maintaining authentic gRPC/Protobuf communication.
package mitm

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/mitm/certmanager"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/mitm/extserver"
	mitmgrpc "github.com/router-for-me/CLIProxyAPI/v6/internal/mitm/grpc"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/mitm/lsclient"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/mitm/lsmanager"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/mitm/proxy"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/mitm/redirect"
	log "github.com/sirupsen/logrus"
)

// EngineConfig holds all configuration for the MITM engine.
type EngineConfig struct {
	// DataDir is the root directory for MITM data (certs, binaries, cache).
	DataDir string

	// LSPath overrides the Language Server binary path. If empty, auto-detected.
	LSPath string

	// TargetHost is the upstream API hostname (default: cloudcode-pa.googleapis.com).
	TargetHost string

	// MITMPort is the port for the MITM proxy. 0 = random.
	MITMPort int

	// H2Profile controls HTTP/2 fingerprint ("chromium" or "go").
	H2Profile string

	// SystemMode controls how requests are modified ("native", "stealth", "minimal").
	SystemMode string

	// RefreshToken is the OAuth refresh token for the account.
	RefreshToken string

	// AccessToken is the current OAuth access token. Refreshed automatically.
	AccessToken string

	// AccountEmail is the email associated with the refresh token.
	AccountEmail string

	// AppRoot overrides the ANTIGRAVITY_EDITOR_APP_ROOT path.
	AppRoot string

	// QuotaCap is the fraction (0.0-1.0) of quota usage that triggers
	// proactive account rotation. Default 0.2 (20%), matching ZG.
	QuotaCap float64
}

// Engine orchestrates all MITM components: certificate management, proxy,
// Language Server process, DNS redirect, Extension Server, and gRPC interception.
type Engine struct {
	cfg         EngineConfig
	certMgr     *certmanager.Manager
	proxy       *proxy.Proxy
	lsMgr       *lsmanager.Manager
	extSrv      *extserver.Server
	lsClient    *lsclient.Client
	interceptor *mitmgrpc.Interceptor

	mu      sync.Mutex
	started bool
	cancel  context.CancelFunc

	// quotaTracker tracks token usage per model for proactive rotation.
	quotaTokens     map[string]int64
	quotaRequests   int64
	quotaExceeded   bool
}

// NewEngine creates a MITM engine with the given configuration.
func NewEngine(cfg EngineConfig) (*Engine, error) {
	if cfg.DataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("mitm engine: determine home dir: %w", err)
		}
		cfg.DataDir = filepath.Join(home, ".cliproxy", "mitm")
	}
	if cfg.TargetHost == "" {
		cfg.TargetHost = "cloudcode-pa.googleapis.com"
	}
	if cfg.H2Profile == "" {
		cfg.H2Profile = "chromium"
	}
	if cfg.SystemMode == "" {
		cfg.SystemMode = "native"
	}
	if cfg.MITMPort == 0 {
		cfg.MITMPort = 443
	}
	if cfg.QuotaCap <= 0 {
		cfg.QuotaCap = 0.2
	}

	deployProductJSON(cfg.AppRoot, cfg.DataDir)

	certDir := filepath.Join(cfg.DataDir, "certs")
	certMgr, err := certmanager.New(certDir)
	if err != nil {
		return nil, fmt.Errorf("mitm engine: init cert manager: %w", err)
	}

	interceptor := mitmgrpc.NewInterceptor(mitmgrpc.InterceptorConfig{
		SystemMode:      cfg.SystemMode,
		DummyPromptText: "Say hello.",
		SensitiveWords:  []string{"Cursor", "cursor", "OpenCode", "Claude Code", "claude-code", "Windsurf", "windsurf"},
	})

	return &Engine{
		cfg:         cfg,
		certMgr:     certMgr,
		interceptor: interceptor,
	}, nil
}

// Start initializes and starts all MITM components in order:
// 1. Extension Server (for LS callbacks)
// 2. MITM Proxy (intercepts LS → Google traffic)
// 3. DNS redirect library (compiles if needed)
// 4. Language Server binary (spawned as child process)
// 5. Wait for LS discovery file (ports, CSRF token)
// 6. Create ConnectRPC client (sends requests to LS)
func (e *Engine) Start(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.started {
		return nil
	}

	ctx, cancel := context.WithCancel(ctx)
	e.cancel = cancel

	// 1. Start the Extension Server (for LS → Extension callbacks).
	e.extSrv = extserver.New()
	if e.cfg.AccessToken != "" || e.cfg.RefreshToken != "" {
		e.extSrv.SetTokens(e.cfg.AccessToken, e.cfg.RefreshToken)
	}
	if err := e.extSrv.Start(); err != nil {
		cancel()
		return fmt.Errorf("mitm engine: start extension server: %w", err)
	}

	// 2. Start the MITM proxy.
	h2Profile := proxy.ProfileChromium
	if e.cfg.H2Profile == "go" {
		h2Profile = proxy.ProfileGo
	}

	mitmProxy := proxy.New(proxy.Config{
		CertManager:   e.certMgr,
		ListenAddr:    fmt.Sprintf("127.0.0.1:%d", e.cfg.MITMPort),
		TargetHost:    e.cfg.TargetHost + ":443",
		OnRequest:     e.onRequest,
		OnResponse:    e.onResponse,
		OnStreamChunk: e.onStreamChunk,
		H2Profile:     h2Profile,
	})

	if err := mitmProxy.Start(ctx); err != nil {
		e.extSrv.Stop()
		cancel()
		return fmt.Errorf("mitm engine: start proxy: %w", err)
	}
	e.proxy = mitmProxy

	proxyPort := mitmProxy.Port()
	log.WithField("port", proxyPort).Info("mitm engine: proxy started")

	// 3. Prepare the DNS redirect library.
	redirectDir := filepath.Join(e.cfg.DataDir, "redirect")
	redirectLibPath, err := redirect.LibraryPath(redirect.Config{
		DataDir:    redirectDir,
		MITMPort:   proxyPort,
		TargetHost: e.cfg.TargetHost,
	})
	if err != nil {
		log.WithError(err).Warn("mitm engine: DNS redirect library not available; LS must be configured manually")
		redirectLibPath = ""
	}

	// 4. Prepare the CA cert bundle.
	caCertPath, err := e.certMgr.CACertPath()
	if err != nil {
		mitmProxy.Stop()
		e.proxy = nil
		e.extSrv.Stop()
		cancel()
		return fmt.Errorf("mitm engine: prepare CA cert: %w", err)
	}

	// 5. Start the Language Server.
	lsDataDir := filepath.Join(e.cfg.DataDir, "ls-data")
	cloudCodeEndpoint := ""
	if redirectLibPath == "" {
		cloudCodeEndpoint = fmt.Sprintf("https://127.0.0.1:%d", proxyPort)
	}
	e.lsMgr = lsmanager.New(lsmanager.Config{
		LSPath:            e.cfg.LSPath,
		DataDir:           lsDataDir,
		MITMPort:          proxyPort,
		CACertPath:        caCertPath,
		RedirectLibPath:   redirectLibPath,
		ExtServerPort:     e.extSrv.Port(),
		ExtServerCSRF:     e.extSrv.CSRFToken(),
		TargetHost:        e.cfg.TargetHost,
		AppRoot:           e.cfg.AppRoot,
		CloudCodeEndpoint: cloudCodeEndpoint,
		AccessToken:       e.cfg.AccessToken,
	})

	if err := e.lsMgr.Start(ctx); err != nil {
		mitmProxy.Stop()
		e.proxy = nil
		e.extSrv.Stop()
		cancel()
		return fmt.Errorf("mitm engine: start LS: %w", err)
	}

	// 6. Wait for the LS discovery file to get ports and CSRF token.
	discoveryPath := lsclient.DiscoveryFilePath(
		e.lsMgr.AppDataDir(),
		e.lsMgr.WorkspaceID(),
	)

	// Clean up any stale discovery file first
	lsclient.CleanupDiscovery(discoveryPath)

	go e.connectToLS(ctx, discoveryPath)

	e.started = true
	log.Info("mitm engine: all components started")
	return nil
}

// connectToLS waits for the LS discovery file and creates a ConnectRPC client.
func (e *Engine) connectToLS(ctx context.Context, discoveryPath string) {
	discovery, err := lsclient.WaitForDiscovery(ctx, discoveryPath, 60*time.Second)
	if err != nil {
		log.WithError(err).Error("mitm engine: failed to discover LS ports")
		return
	}

	client := lsclient.NewClient(discovery.HTTPSPort, discovery.CSRFToken)

	// Inject access token so the LS can authenticate with Google.
	if token := e.GetAccessToken(); token != "" {
		client.SetAccessToken(token)
	}

	if err := client.WaitForReady(ctx, 30*time.Second); err != nil {
		log.WithError(err).Error("mitm engine: LS not responding to heartbeats")
		return
	}

	e.mu.Lock()
	e.lsClient = client
	accessToken := e.cfg.AccessToken
	refreshToken := e.cfg.RefreshToken
	e.mu.Unlock()

	log.WithField("httpsPort", discovery.HTTPSPort).Info("mitm engine: ConnectRPC client connected to LS")

	// Inject OAuth token directly into the LS so it can authenticate with Google.
	if accessToken != "" {
		if err := client.SaveOAuthTokenInfo(ctx, accessToken, refreshToken); err != nil {
			log.WithError(err).Warn("mitm engine: failed to inject OAuth token into LS")
		} else {
			log.Info("mitm engine: OAuth token injected into LS")
			// Wait for LS to process the token and fetch model list.
			time.Sleep(3 * time.Second)
		}
	}
}

// Stop shuts down all MITM components.
func (e *Engine) Stop() {
	e.mu.Lock()
	defer e.mu.Unlock()

	if !e.started {
		return
	}

	// Cancel context first to signal all goroutines.
	if e.cancel != nil {
		e.cancel()
	}

	if e.lsMgr != nil {
		e.lsMgr.Stop()
	}
	if e.proxy != nil {
		e.proxy.Stop()
	}
	if e.extSrv != nil {
		e.extSrv.Stop()
	}

	e.started = false
	log.Info("mitm engine: stopped")
}

// RestartLS restarts the Language Server process (e.g., after account rotation).
func (e *Engine) RestartLS(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.lsMgr == nil {
		return fmt.Errorf("mitm engine: LS manager not initialized")
	}

	// Reset the client — it will be recreated after the LS restarts.
	e.lsClient = nil

	if err := e.lsMgr.Restart(ctx); err != nil {
		return err
	}

	discoveryPath := lsclient.DiscoveryFilePath(
		e.lsMgr.AppDataDir(),
		e.lsMgr.WorkspaceID(),
	)
	lsclient.CleanupDiscovery(discoveryPath)
	go e.connectToLS(ctx, discoveryPath)

	return nil
}

// IsRunning returns true if the engine is fully operational.
func (e *Engine) IsRunning() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.started && e.lsMgr != nil && e.lsMgr.IsRunning()
}

// LSClient returns the ConnectRPC client for sending requests to the LS.
// May be nil if the LS is not yet ready.
func (e *Engine) LSClient() *lsclient.Client {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lsClient
}

// WaitForLSClient waits up to timeout for the LS client to become available.
func (e *Engine) WaitForLSClient(ctx context.Context, timeout time.Duration) *lsclient.Client {
	deadline := time.After(timeout)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		if c := e.LSClient(); c != nil {
			return c
		}
		select {
		case <-ctx.Done():
			return nil
		case <-deadline:
			return nil
		case <-ticker.C:
		}
	}
}

// ProxyPort returns the MITM proxy's listening port.
func (e *Engine) ProxyPort() int {
	e.mu.Lock()
	p := e.proxy
	e.mu.Unlock()
	if p == nil {
		return 0
	}
	return p.Port()
}

// Interceptor returns the gRPC interceptor for direct request injection.
func (e *Engine) Interceptor() *mitmgrpc.Interceptor {
	return e.interceptor
}

// SetAccessToken updates the access token used for MITM auth injection and LS ConnectRPC metadata.
func (e *Engine) SetAccessToken(token string) {
	e.mu.Lock()
	e.cfg.AccessToken = token
	client := e.lsClient
	e.mu.Unlock()
	if client != nil {
		client.SetAccessToken(token)
	}
}

// GetAccessToken returns the current access token.
func (e *Engine) GetAccessToken() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cfg.AccessToken
}

// RotateAccount handles 429/403 errors by switching to a different account
// and restarting the Language Server with the new credentials.
func (e *Engine) RotateAccount(ctx context.Context, newRefreshToken, newEmail, newAccessToken string) error {
	e.mu.Lock()
	e.cfg.RefreshToken = newRefreshToken
	e.cfg.AccountEmail = newEmail
	e.cfg.AccessToken = newAccessToken
	e.mu.Unlock()

	log.WithField("email", newEmail).Info("mitm engine: rotating account, restarting LS")
	return e.RestartLS(ctx)
}

// Antigravity client identity headers. The LS running standalone doesn't always
// populate these, so we inject them to match real Antigravity traffic.
const (
	mitmXGoogAPIClient = "gl-node/22.20.0 grpc-node/1.12.6 gax-node/4.8.0 gapic/1.107.0 antigravity/1.107.0"
	mitmClientMetadata = "ideType=ANTIGRAVITY,platform=PLATFORM_UNSPECIFIED,pluginType=GEMINI"
	mitmUserAgent      = "antigravity/1.107.0 windows/amd64"
)

// onRequest is the proxy callback for incoming requests from the Language Server.
// It handles three critical functions:
// 1. Injects/replaces the user's content in the gRPC payload (via interceptor)
// 2. Injects a valid Authorization header (since LS may not have valid auth)
// 3. Ensures Antigravity client identity headers are present
func (e *Engine) onRequest(req *http.Request, body []byte) (*http.Response, []byte, error) {
	ct := req.Header.Get("Content-Type")
	bodySnippet := string(body)
	if len(bodySnippet) > 500 {
		bodySnippet = bodySnippet[:500] + "..."
	}
	ua := req.Header.Get("User-Agent")
	xGoog := req.Header.Get("X-Goog-Api-Client")
	clientMeta := req.Header.Get("Client-Metadata")
	log.Infof("mitm proxy: %s %s [ct=%s len=%d ua=%s x-goog=%s client-meta=%s] body=%s",
		req.Method, req.URL.Path, ct, len(body), ua, xGoog, clientMeta, bodySnippet)

	modifiedBody, err := e.interceptor.InterceptRequest(req, body)
	if err != nil {
		return nil, nil, err
	}

	// Inject Authorization header if we have an access token.
	accessToken := e.GetAccessToken()
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}

	// Ensure Antigravity client identity headers are present.
	if xGoog == "" {
		req.Header.Set("X-Goog-Api-Client", mitmXGoogAPIClient)
	}
	if clientMeta == "" {
		req.Header.Set("Client-Metadata", mitmClientMetadata)
	}
	if ua == "" || ua == "antigravity/ windows/amd64" {
		req.Header.Set("User-Agent", mitmUserAgent)
	}

	return nil, modifiedBody, nil
}

// onResponse is the proxy callback for non-streaming responses.
func (e *Engine) onResponse(req *http.Request, resp *http.Response, body []byte) ([]byte, error) {
	bodySnippet := string(body)
	if len(bodySnippet) > 500 {
		bodySnippet = bodySnippet[:500] + "..."
	}
	log.Infof("mitm proxy response: %s %d [len=%d] body=%s", req.URL.Path, resp.StatusCode, len(body), bodySnippet)
	return e.interceptor.InterceptResponse(req, resp, body)
}

// onStreamChunk is the proxy callback for streaming response chunks.
func (e *Engine) onStreamChunk(req *http.Request, chunk []byte) ([]byte, error) {
	chunkSnippet := string(chunk)
	if len(chunkSnippet) > 300 {
		chunkSnippet = chunkSnippet[:300] + "..."
	}
	log.Infof("mitm proxy stream chunk: %s [len=%d] data=%s", req.URL.Path, len(chunk), chunkSnippet)

	e.trackTokenUsage(chunk)

	return e.interceptor.InterceptStreamChunk(req, chunk)
}

// trackTokenUsage extracts token counts from streaming response metadata
// for proactive quota rotation.
func (e *Engine) trackTokenUsage(chunk []byte) {
	s := string(chunk)
	if !strings.Contains(s, "totalTokenCount") {
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if e.quotaTokens == nil {
		e.quotaTokens = make(map[string]int64)
	}
	e.quotaRequests++
}

// IsQuotaExceeded returns true if the current account's usage exceeds the
// configured quota cap, signaling that proactive rotation is recommended.
func (e *Engine) IsQuotaExceeded() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.quotaExceeded
}

// ResetQuota clears usage counters after account rotation.
func (e *Engine) ResetQuota() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.quotaTokens = make(map[string]int64)
	e.quotaRequests = 0
	e.quotaExceeded = false
}

// deployProductJSON copies product.json from the Antigravity installation
// to the expected path so the LS version detection works correctly.
func deployProductJSON(appRoot, dataDir string) {
	if appRoot == "" {
		return
	}

	src := filepath.Join(appRoot, "resources", "app", "product.json")
	if _, err := os.Stat(src); err != nil {
		return
	}

	destDir := filepath.Join(dataDir, "resources", "app")
	os.MkdirAll(destDir, 0o755)
	dest := filepath.Join(destDir, "product.json")

	if _, err := os.Stat(dest); err == nil {
		return
	}

	data, err := os.ReadFile(src)
	if err != nil {
		return
	}
	if err := os.WriteFile(dest, data, 0o644); err != nil {
		log.WithError(err).Warn("mitm engine: failed to deploy product.json")
	} else {
		log.Info("mitm engine: deployed product.json for version detection")
	}
}
