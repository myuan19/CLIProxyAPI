//go:build darwin

package redirect

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

func buildLibrary(dataDir string) error {
	srcDir := findNativeSourceDir()
	if srcDir == "" {
		return fmt.Errorf("native source directory not found")
	}

	src := filepath.Join(srcDir, "dns_redirect.c")
	out := filepath.Join(dataDir, "dns_redirect.dylib")

	cmd := exec.Command("clang", "-dynamiclib", "-o", out, src, "-Wall", "-O2")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func findNativeSourceDir() string {
	_, thisFile, _, _ := runtime.Caller(0)
	candidate := filepath.Join(filepath.Dir(thisFile), "native")
	if _, err := os.Stat(filepath.Join(candidate, "dns_redirect.c")); err == nil {
		return candidate
	}

	for _, p := range []string{
		"internal/mitm/redirect/native",
		"redirect/native",
	} {
		if _, err := os.Stat(filepath.Join(p, "dns_redirect.c")); err == nil {
			abs, _ := filepath.Abs(p)
			return abs
		}
	}
	return ""
}
