package lsmanager

import "runtime"

// appendRedirectEnv adds the platform-specific environment variable to load
// the DNS redirect shared library into the Language Server child process.
func appendRedirectEnv(env []string, libPath string) []string {
	switch runtime.GOOS {
	case "linux":
		return append(env, "LD_PRELOAD="+libPath)
	case "darwin":
		return append(env,
			"DYLD_INSERT_LIBRARIES="+libPath,
			"DYLD_FORCE_FLAT_NAMESPACE=1",
		)
	default:
		// Windows uses DLL injection via CreateProcess, not env vars.
		// The injection is handled in redirect_windows.go instead.
		return env
	}
}
