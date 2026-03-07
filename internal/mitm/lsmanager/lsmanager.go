package lsmanager

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

// Config controls Language Server process management.
type Config struct {
	// LSPath is the explicit path to the Language Server binary.
	// If empty, the manager will attempt auto-detection or download.
	LSPath string

	// DataDir is the working directory for the LS process and cached files.
	DataDir string

	// MITMPort is the local port where the MITM proxy listens.
	MITMPort int

	// CACertPath is the path to the PEM bundle containing our MITM CA
	// plus system certificates. Set as SSL_CERT_FILE for the child process.
	CACertPath string

	// RedirectLibPath is the path to the DNS redirect shared library
	// (LD_PRELOAD on Linux, DYLD_INSERT_LIBRARIES on macOS).
	RedirectLibPath string

	// ExtServerPort is the Extension Server port for LS callbacks.
	ExtServerPort int

	// ExtServerCSRF is the CSRF token for the Extension Server.
	ExtServerCSRF string

	// WorkspaceID identifies the workspace. Used for discovery file naming.
	WorkspaceID string

	// AppDataDir is the --app_data_dir name passed to the LS.
	// Default: "Antigravity"
	AppDataDir string

	// CloudCodeEndpoint overrides the Google API endpoint.
	CloudCodeEndpoint string

	// TargetHost is the hostname to intercept (for the MITM_TARGET_HOST env var).
	// Default: "cloudcode-pa.googleapis.com"
	TargetHost string

	// AppRoot is the ANTIGRAVITY_EDITOR_APP_ROOT environment variable value.
	// Points to the Antigravity IDE installation root.
	AppRoot string

	// Env contains additional environment variables for the child process.
	Env []string

	// AccessToken is the OAuth access token injected into stdin metadata
	// so the LS can authenticate without its own OAuth flow.
	AccessToken string
}

// Manager controls the lifecycle of a Language Server child process.
type Manager struct {
	cfg Config

	mu      sync.Mutex
	cmd     *exec.Cmd
	cancel  context.CancelFunc
	running bool
	lsPath  string
	waitDone chan struct{} // closed when cmd.Wait() returns
}

// New creates a Manager with the given configuration.
func New(cfg Config) *Manager {
	if cfg.AppDataDir == "" {
		cfg.AppDataDir = "antigravity"
	}
	if cfg.WorkspaceID == "" {
		cfg.WorkspaceID = uuid.New().String()
	}
	return &Manager{cfg: cfg}
}

// Start launches the Language Server as a child process with the correct
// command-line arguments and environment for MITM interception.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.running {
		return nil
	}

	lsPath, err := m.resolveLS()
	if err != nil {
		return err
	}
	m.lsPath = lsPath

	if err := os.MkdirAll(m.cfg.DataDir, 0o755); err != nil {
		return fmt.Errorf("lsmanager: create data dir: %w", err)
	}

	childCtx, cancel := context.WithCancel(ctx)
	m.cancel = cancel

	var cmd *exec.Cmd

	if needsDLLInjection() && m.cfg.RedirectLibPath != "" {
		var err error
		cmd, err = m.startWithDLLInjection(childCtx, lsPath)
		if err != nil {
			cancel()
			return err
		}
	} else {
		args := m.buildArgs()
		env := m.buildEnv()
		cmd = exec.CommandContext(childCtx, lsPath, args...)
		cmd.Dir = m.cfg.DataDir
		cmd.Env = env
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		stdinPipe, pipeErr := cmd.StdinPipe()
		if pipeErr != nil {
			cancel()
			return fmt.Errorf("lsmanager: create stdin pipe: %w", pipeErr)
		}

		if err := cmd.Start(); err != nil {
			cancel()
			return fmt.Errorf("lsmanager: start LS: %w", err)
		}

		// LS reads a protobuf Metadata message from stdin using io.ReadAll.
		// We provide the access token as field 1 (api_key) then close stdin.
		_, _ = stdinPipe.Write(buildStdinMetadata(m.cfg.AccessToken))
		stdinPipe.Close()
	}

	m.cmd = cmd
	m.running = true
	m.waitDone = make(chan struct{})

	go func() {
		err := cmd.Wait()
		m.mu.Lock()
		m.running = false
		m.mu.Unlock()
		close(m.waitDone)
		if err != nil {
			log.WithError(err).Warn("lsmanager: Language Server exited")
		} else {
			log.Info("lsmanager: Language Server exited normally")
		}
	}()

	log.WithFields(log.Fields{
		"pid":  cmd.Process.Pid,
		"path": lsPath,
		"args": m.buildArgs(),
		"dir":  m.cfg.DataDir,
	}).Info("lsmanager: Language Server started")
	return nil
}

// Stop gracefully stops the Language Server process.
func (m *Manager) Stop() error {
	m.mu.Lock()
	if !m.running || m.cancel == nil {
		m.mu.Unlock()
		return nil
	}
	cancelFn := m.cancel
	waitCh := m.waitDone
	cmd := m.cmd
	m.mu.Unlock()

	cancelFn()

	if waitCh != nil {
		select {
		case <-waitCh:
		case <-time.After(10 * time.Second):
			if cmd != nil && cmd.Process != nil {
				cmd.Process.Kill()
			}
			<-waitCh
		}
	}

	m.mu.Lock()
	m.running = false
	m.mu.Unlock()

	log.Info("lsmanager: Language Server stopped")
	return nil
}

// Restart stops and restarts the Language Server.
func (m *Manager) Restart(ctx context.Context) error {
	if err := m.Stop(); err != nil {
		log.WithError(err).Warn("lsmanager: error stopping LS during restart")
	}
	time.Sleep(500 * time.Millisecond)
	return m.Start(ctx)
}

// IsRunning returns true if the Language Server process is alive.
func (m *Manager) IsRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

// PID returns the process ID of the running Language Server, or 0 if not running.
func (m *Manager) PID() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cmd != nil && m.cmd.Process != nil {
		return m.cmd.Process.Pid
	}
	return 0
}

// WorkspaceID returns the workspace identifier used for discovery.
func (m *Manager) WorkspaceID() string {
	return m.cfg.WorkspaceID
}

// AppDataDir returns the app data directory name.
func (m *Manager) AppDataDir() string {
	return m.cfg.AppDataDir
}

func (m *Manager) resolveLS() (string, error) {
	if m.cfg.LSPath != "" {
		if _, err := os.Stat(m.cfg.LSPath); err == nil {
			return m.cfg.LSPath, nil
		}
		return "", fmt.Errorf("lsmanager: configured ls_path not found: %s", m.cfg.LSPath)
	}

	binDir := filepath.Join(m.cfg.DataDir, "bin")
	return DownloadLS(binDir)
}

// buildArgs constructs the LS command-line arguments matching how
// the real Antigravity Extension starts the LS binary.
func (m *Manager) buildArgs() []string {
	args := []string{
		"--persistent_mode",
		"--workspace_id", m.cfg.WorkspaceID,
		"--app_data_dir", m.cfg.AppDataDir,
		"--random_port",
	}

	if m.cfg.ExtServerPort > 0 {
		args = append(args,
			"--extension_server_port", fmt.Sprintf("%d", m.cfg.ExtServerPort),
		)
	}
	if m.cfg.ExtServerCSRF != "" {
		args = append(args,
			"--extension_server_csrf_token", m.cfg.ExtServerCSRF,
			"--csrf_token", m.cfg.ExtServerCSRF,
		)
	}

	if m.cfg.CloudCodeEndpoint != "" {
		args = append(args, "--cloud_code_endpoint", m.cfg.CloudCodeEndpoint)
	}

	return args
}

func (m *Manager) buildEnv() []string {
	env := os.Environ()

	env = append(env, fmt.Sprintf("MITM_PROXY_PORT=%d", m.cfg.MITMPort))

	if m.cfg.TargetHost != "" {
		env = append(env, "MITM_TARGET_HOST="+m.cfg.TargetHost)
	}

	if m.cfg.CACertPath != "" {
		env = append(env, "SSL_CERT_FILE="+m.cfg.CACertPath)
	}

	if m.cfg.RedirectLibPath != "" {
		env = appendRedirectEnv(env, m.cfg.RedirectLibPath)
	}

	if m.cfg.AppRoot != "" {
		env = append(env, "ANTIGRAVITY_EDITOR_APP_ROOT="+m.cfg.AppRoot)
	} else {
		appRoot := findAppRoot()
		if appRoot != "" {
			env = append(env, "ANTIGRAVITY_EDITOR_APP_ROOT="+appRoot)
		}
	}

	env = append(env, m.cfg.Env...)

	return env
}

// buildStdinMetadata encodes a minimal protobuf Metadata message for LS stdin.
// Field 1 (api_key/string) carries the OAuth access token.
func buildStdinMetadata(accessToken string) []byte {
	if accessToken == "" {
		return []byte{0x0a, 0x00}
	}
	tag := byte(0x0a) // field 1, wire type 2 (length-delimited)
	tokenBytes := []byte(accessToken)
	n := len(tokenBytes)
	// Encode varint length
	var lenBuf []byte
	for n > 0x7f {
		lenBuf = append(lenBuf, byte(n&0x7f)|0x80)
		n >>= 7
	}
	lenBuf = append(lenBuf, byte(n))
	var buf []byte
	buf = append(buf, tag)
	buf = append(buf, lenBuf...)
	buf = append(buf, tokenBytes...)
	return buf
}

// findAppRoot tries to find the Antigravity installation root directory.
func findAppRoot() string {
	var candidates []string

	switch runtime.GOOS {
	case "windows":
		if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
			candidates = append(candidates, filepath.Join(localAppData, "Programs", "Antigravity"))
		}
		if pf := os.Getenv("ProgramFiles"); pf != "" {
			candidates = append(candidates, filepath.Join(pf, "Antigravity"))
		}
	case "darwin":
		candidates = append(candidates,
			"/Applications/Antigravity.app/Contents/Resources/app",
		)
		if home, _ := os.UserHomeDir(); home != "" {
			candidates = append(candidates,
				filepath.Join(home, "Applications", "Antigravity.app", "Contents", "Resources", "app"),
			)
		}
	default:
		candidates = append(candidates,
			"/usr/share/antigravity",
			"/opt/antigravity",
		)
		if home, _ := os.UserHomeDir(); home != "" {
			candidates = append(candidates,
				filepath.Join(home, ".local", "share", "antigravity"),
			)
		}
	}

	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			return c
		}
	}
	return ""
}
