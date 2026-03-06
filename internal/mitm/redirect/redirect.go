// Package redirect provides cross-platform DNS/connection redirection for MITM interception.
// It manages compilation and deployment of the native shared library that hooks
// getaddrinfo/connect to redirect Language Server traffic to a local MITM proxy.
package redirect

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Config holds parameters for the redirect library.
type Config struct {
	// DataDir is where compiled/cached redirect libraries are stored.
	DataDir string

	// MITMPort is the local port of the MITM proxy.
	MITMPort int

	// TargetHost is the hostname to intercept (default: cloudcode-pa.googleapis.com).
	TargetHost string
}

// LibraryPath returns the path to the redirect shared library for the current platform.
// If the library doesn't exist, it attempts to find a precompiled copy or build one.
func LibraryPath(cfg Config) (string, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return "", fmt.Errorf("redirect: create data dir: %w", err)
	}

	libName := libraryName()
	libPath := filepath.Join(cfg.DataDir, libName)

	if info, err := os.Stat(libPath); err == nil && info.Size() > 0 {
		return libPath, nil
	}

	if err := buildLibrary(cfg.DataDir); err != nil {
		return "", fmt.Errorf("redirect: build library: %w", err)
	}

	if _, err := os.Stat(libPath); err != nil {
		return "", fmt.Errorf("redirect: library not found after build: %s", libPath)
	}

	return libPath, nil
}

// EnvVars returns the environment variables needed to activate the redirect
// for a child process.
func EnvVars(libPath string, mitmPort int, targetHost string) []string {
	if targetHost == "" {
		targetHost = "cloudcode-pa.googleapis.com"
	}

	env := []string{
		fmt.Sprintf("MITM_PROXY_PORT=%d", mitmPort),
		fmt.Sprintf("MITM_TARGET_HOST=%s", targetHost),
	}

	switch runtime.GOOS {
	case "linux":
		env = append(env, "LD_PRELOAD="+libPath)
	case "darwin":
		env = append(env,
			"DYLD_INSERT_LIBRARIES="+libPath,
			"DYLD_FORCE_FLAT_NAMESPACE=1",
		)
	}
	// Windows uses DLL injection instead of env vars; see redirect_windows.go.

	return env
}

func libraryName() string {
	switch runtime.GOOS {
	case "darwin":
		return "dns_redirect.dylib"
	case "windows":
		return "dns_redirect.dll"
	default:
		return "dns_redirect.so"
	}
}
