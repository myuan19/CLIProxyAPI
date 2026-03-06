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
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/mitm/certmanager"
	mitmgrpc "github.com/router-for-me/CLIProxyAPI/v6/internal/mitm/grpc"
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

	// AccountEmail is the email associated with the refresh token.
	AccountEmail string
}

// Engine orchestrates all MITM components: certificate management, proxy,
// Language Server process, DNS redirect, and gRPC interception.
type Engine struct {
	cfg         EngineConfig
	certMgr     *certmanager.Manager
	proxy       *proxy.Proxy
	lsMgr       *lsmanager.Manager
	interceptor *mitmgrpc.Interceptor

	mu      sync.Mutex
	started bool
	cancel  context.CancelFunc
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

	certDir := filepath.Join(cfg.DataDir, "certs")
	certMgr, err := certmanager.New(certDir)
	if err != nil {
		return nil, fmt.Errorf("mitm engine: init cert manager: %w", err)
	}

	interceptor := mitmgrpc.NewInterceptor(mitmgrpc.InterceptorConfig{
		SystemMode: cfg.SystemMode,
	})

	return &Engine{
		cfg:         cfg,
		certMgr:     certMgr,
		interceptor: interceptor,
	}, nil
}

// Start initializes and starts all MITM components.
func (e *Engine) Start(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.started {
		return nil
	}

	ctx, cancel := context.WithCancel(ctx)
	e.cancel = cancel

	// 1. Start the MITM proxy.
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
		cancel()
		return fmt.Errorf("mitm engine: start proxy: %w", err)
	}
	e.proxy = mitmProxy

	proxyPort := mitmProxy.Port()
	log.WithField("port", proxyPort).Info("mitm engine: proxy started")

	// 2. Prepare the DNS redirect library.
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

	// 3. Prepare the CA cert bundle.
	caCertPath, err := e.certMgr.CACertPath()
	if err != nil {
		cancel()
		return fmt.Errorf("mitm engine: prepare CA cert: %w", err)
	}

	// 4. Start the Language Server.
	lsDataDir := filepath.Join(e.cfg.DataDir, "ls-data")
	e.lsMgr = lsmanager.New(lsmanager.Config{
		LSPath:          e.cfg.LSPath,
		DataDir:         lsDataDir,
		MITMPort:        proxyPort,
		CACertPath:      caCertPath,
		RedirectLibPath: redirectLibPath,
	})

	if err := e.lsMgr.Start(ctx); err != nil {
		cancel()
		return fmt.Errorf("mitm engine: start LS: %w", err)
	}

	e.started = true
	log.Info("mitm engine: all components started")
	return nil
}

// Stop shuts down all MITM components.
func (e *Engine) Stop() {
	e.mu.Lock()
	defer e.mu.Unlock()

	if !e.started {
		return
	}

	if e.lsMgr != nil {
		e.lsMgr.Stop()
	}
	if e.proxy != nil {
		e.proxy.Stop()
	}
	if e.cancel != nil {
		e.cancel()
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
	return e.lsMgr.Restart(ctx)
}

// IsRunning returns true if the engine is fully operational.
func (e *Engine) IsRunning() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.started && e.lsMgr != nil && e.lsMgr.IsRunning()
}

// ProxyPort returns the MITM proxy's listening port.
func (e *Engine) ProxyPort() int {
	if e.proxy == nil {
		return 0
	}
	return e.proxy.Port()
}

// Interceptor returns the gRPC interceptor for direct request injection.
func (e *Engine) Interceptor() *mitmgrpc.Interceptor {
	return e.interceptor
}

// RotateAccount handles 429/403 errors by switching to a different account
// and restarting the Language Server with the new credentials.
func (e *Engine) RotateAccount(ctx context.Context, newRefreshToken, newEmail string) error {
	e.mu.Lock()
	e.cfg.RefreshToken = newRefreshToken
	e.cfg.AccountEmail = newEmail
	e.mu.Unlock()

	log.WithField("email", newEmail).Info("mitm engine: rotating account, restarting LS")
	return e.RestartLS(ctx)
}

// onRequest is the proxy callback for incoming requests from the Language Server.
func (e *Engine) onRequest(req *http.Request, body []byte) (*http.Response, []byte, error) {
	modifiedBody, err := e.interceptor.InterceptRequest(req, body)
	if err != nil {
		return nil, nil, err
	}
	return nil, modifiedBody, nil
}

// onResponse is the proxy callback for non-streaming responses.
func (e *Engine) onResponse(req *http.Request, resp *http.Response, body []byte) ([]byte, error) {
	return e.interceptor.InterceptResponse(req, resp, body)
}

// onStreamChunk is the proxy callback for streaming response chunks.
func (e *Engine) onStreamChunk(req *http.Request, chunk []byte) ([]byte, error) {
	return e.interceptor.InterceptStreamChunk(req, chunk)
}
