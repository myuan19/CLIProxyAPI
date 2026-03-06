//go:build linux

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

	buildScript := filepath.Join(srcDir, "build.sh")
	if _, err := os.Stat(buildScript); err != nil {
		return buildFromSource(srcDir, dataDir)
	}

	cmd := exec.Command("bash", buildScript, dataDir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func buildFromSource(srcDir, outputDir string) error {
	src := filepath.Join(srcDir, "dns_redirect.c")
	out := filepath.Join(outputDir, "dns_redirect.so")

	cmd := exec.Command("gcc", "-shared", "-fPIC", "-o", out, src, "-ldl", "-Wall", "-O2")
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

	// Fallback: search relative to the working directory.
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
