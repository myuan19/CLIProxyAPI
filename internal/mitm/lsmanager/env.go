package lsmanager

import (
	"path/filepath"
	"runtime"
)

// appendRedirectEnv adds the platform-specific environment variable to load
// the DNS redirect shared library into the Language Server child process.
// On Linux, uses LD_LIBRARY_PATH with the cache directory (ZG-style
// libgthread-2.0.so.0 masquerade) instead of LD_PRELOAD.
func appendRedirectEnv(env []string, libPath string) []string {
	switch runtime.GOOS {
	case "linux":
		cacheDir := filepath.Dir(libPath)
		return append(env, "LD_LIBRARY_PATH="+cacheDir)
	case "darwin":
		return append(env,
			"DYLD_INSERT_LIBRARIES="+libPath,
			"DYLD_FORCE_FLAT_NAMESPACE=1",
		)
	default:
		return env
	}
}
