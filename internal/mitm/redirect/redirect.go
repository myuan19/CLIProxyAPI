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

// DeployLibrary copies/renames the built redirect library to match ZG's
// naming convention: libgthread-2.0.so.0 in a cache directory. The LS
// loads this via LD_LIBRARY_PATH, which is less detectable than LD_PRELOAD.
func DeployLibrary(cfg Config) (libPath string, cacheDir string, err error) {
	srcPath, err := LibraryPath(cfg)
	if err != nil {
		return "", "", err
	}

	cacheDir = filepath.Join(cfg.DataDir, ".agcache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", "", fmt.Errorf("redirect: create cache dir: %w", err)
	}

	deployName := deployedLibraryName()
	deployPath := filepath.Join(cacheDir, deployName)

	if info, err := os.Stat(deployPath); err == nil && info.Size() > 0 {
		return deployPath, cacheDir, nil
	}

	src, err := os.ReadFile(srcPath)
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(deployPath, src, 0o755); err != nil {
		return "", "", err
	}
	return deployPath, cacheDir, nil
}

// EnvVars returns the environment variables needed to activate the redirect
// for a child process. On Linux, uses LD_LIBRARY_PATH with the cache dir
// (matching ZG's libgthread-2.0.so.0 injection) instead of LD_PRELOAD.
func EnvVars(libPath string, mitmPort int, targetHost string) []string {
	if targetHost == "" {
		targetHost = "cloudcode-pa.googleapis.com"
	}

	env := []string{
		fmt.Sprintf("MITM_PROXY_PORT=%d", mitmPort),
		fmt.Sprintf("MITM_TARGET_HOST=%s", targetHost),
	}

	cacheDir := filepath.Dir(libPath)

	switch runtime.GOOS {
	case "linux":
		env = append(env, "LD_LIBRARY_PATH="+cacheDir)
	case "darwin":
		env = append(env,
			"DYLD_INSERT_LIBRARIES="+libPath,
			"DYLD_FORCE_FLAT_NAMESPACE=1",
		)
	}

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

// deployedLibraryName returns the name used in the cache directory.
// On Linux, masquerades as libgthread-2.0.so.0 so the LS loads it
// via LD_LIBRARY_PATH without needing LD_PRELOAD.
func deployedLibraryName() string {
	switch runtime.GOOS {
	case "linux":
		return "libgthread-2.0.so.0"
	case "darwin":
		return "dns_redirect.dylib"
	default:
		return "dns_redirect.dll"
	}
}
