package lsmanager

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

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
	// The DNS redirect library uses this to reroute LS connections.
	MITMPort int

	// CACertPath is the path to the PEM bundle containing our MITM CA
	// plus system certificates. Set as SSL_CERT_FILE for the child process.
	CACertPath string

	// RedirectLibPath is the path to the DNS redirect shared library
	// (LD_PRELOAD on Linux, DYLD_INSERT_LIBRARIES on macOS).
	RedirectLibPath string

	// Env contains additional environment variables for the child process.
	Env []string
}

// Manager controls the lifecycle of a Language Server child process.
type Manager struct {
	cfg Config

	mu      sync.Mutex
	cmd     *exec.Cmd
	cancel  context.CancelFunc
	running bool
	lsPath  string
}

// New creates a Manager with the given configuration.
func New(cfg Config) *Manager {
	return &Manager{cfg: cfg}
}

// Start launches the Language Server as a child process with the appropriate
// environment for DNS/TLS interception.
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
		cmd = exec.CommandContext(childCtx, lsPath,
			"--stdio",
			"--health_check",
		)
		cmd.Dir = m.cfg.DataDir
		cmd.Env = m.buildEnv()
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		if err := cmd.Start(); err != nil {
			cancel()
			return fmt.Errorf("lsmanager: start LS: %w", err)
		}
	}

	m.cmd = cmd
	m.running = true

	go func() {
		err := cmd.Wait()
		m.mu.Lock()
		m.running = false
		m.mu.Unlock()
		if err != nil {
			log.WithError(err).Warn("lsmanager: Language Server exited")
		} else {
			log.Info("lsmanager: Language Server exited normally")
		}
	}()

	log.WithField("pid", cmd.Process.Pid).Info("lsmanager: Language Server started")
	return nil
}

// Stop gracefully stops the Language Server process.
func (m *Manager) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.running || m.cancel == nil {
		return nil
	}

	m.cancel()

	done := make(chan struct{})
	go func() {
		if m.cmd != nil && m.cmd.Process != nil {
			m.cmd.Wait()
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		if m.cmd != nil && m.cmd.Process != nil {
			m.cmd.Process.Kill()
		}
	}

	m.running = false
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

func (m *Manager) buildEnv() []string {
	env := os.Environ()

	env = append(env, fmt.Sprintf("MITM_PROXY_PORT=%d", m.cfg.MITMPort))

	if m.cfg.CACertPath != "" {
		env = append(env, "SSL_CERT_FILE="+m.cfg.CACertPath)
	}

	if m.cfg.RedirectLibPath != "" {
		env = appendRedirectEnv(env, m.cfg.RedirectLibPath)
	}

	env = append(env, m.cfg.Env...)

	return env
}
