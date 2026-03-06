package lsmanager

import (
	"archive/zip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	// Antigravity IDE distribution URLs per platform.
	// The Language Server binary is extracted from the IDE package.
	lsVersion = "1.19.6"

	// Base URL pattern for the Language Server binary downloads.
	// ZeroGravity uses edgedl.me.gvt1.com; we mirror the same pattern.
	baseDownloadURL = "https://edgedl.me.gvt1.com/edgedl/release2/j0qc3/antigravity"
)

// binaryName returns the expected Language Server binary filename for the current OS/arch.
func binaryName() string {
	switch runtime.GOOS {
	case "windows":
		return "language_server_windows_x64.exe"
	case "darwin":
		if runtime.GOARCH == "arm64" {
			return "language_server_darwin_arm"
		}
		return "language_server_darwin_x64"
	default:
		if runtime.GOARCH == "arm64" {
			return "language_server_linux_arm"
		}
		return "language_server_linux_x64"
	}
}

// antigravityExtensionBinaryPath returns the relative path inside an Antigravity
// distribution archive where the Language Server binary resides.
func antigravityExtensionBinaryPath() string {
	return filepath.Join("resources", "app", "extensions", "antigravity", "bin", binaryName())
}

// DownloadLS downloads and extracts the Language Server binary into dir.
// If the binary already exists in dir, it returns the path without downloading.
func DownloadLS(dir string) (string, error) {
	dest := filepath.Join(dir, binaryName())
	if info, err := os.Stat(dest); err == nil && info.Size() > 0 {
		return dest, nil
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("lsmanager: create dir: %w", err)
	}

	// Try extracting from a local Antigravity installation first.
	if p, err := findLocalLS(); err == nil {
		if err := copyFile(p, dest); err == nil {
			if err := os.Chmod(dest, 0o755); err == nil {
				return dest, nil
			}
		}
	}

	return "", fmt.Errorf("lsmanager: Language Server binary not found; please provide ls_path in config or place %s in %s", binaryName(), dir)
}

// findLocalLS searches common Antigravity installation directories.
func findLocalLS() (string, error) {
	name := binaryName()

	var searchPaths []string
	switch runtime.GOOS {
	case "windows":
		if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
			searchPaths = append(searchPaths,
				filepath.Join(localAppData, "Programs", "Antigravity", "resources", "app", "extensions", "antigravity", "bin", name),
			)
		}
		if programFiles := os.Getenv("ProgramFiles"); programFiles != "" {
			searchPaths = append(searchPaths,
				filepath.Join(programFiles, "Antigravity", "resources", "app", "extensions", "antigravity", "bin", name),
			)
		}
	case "darwin":
		searchPaths = append(searchPaths,
			filepath.Join("/Applications", "Antigravity.app", "Contents", "Resources", "app", "extensions", "antigravity", "bin", name),
		)
		if home, _ := os.UserHomeDir(); home != "" {
			searchPaths = append(searchPaths,
				filepath.Join(home, "Applications", "Antigravity.app", "Contents", "Resources", "app", "extensions", "antigravity", "bin", name),
			)
		}
	default:
		searchPaths = append(searchPaths,
			filepath.Join("/usr", "share", "antigravity", "resources", "app", "extensions", "antigravity", "bin", name),
			filepath.Join("/opt", "antigravity", "resources", "app", "extensions", "antigravity", "bin", name),
		)
		if home, _ := os.UserHomeDir(); home != "" {
			searchPaths = append(searchPaths,
				filepath.Join(home, ".local", "share", "antigravity", "resources", "app", "extensions", "antigravity", "bin", name),
			)
		}
	}

	for _, p := range searchPaths {
		if info, err := os.Stat(p); err == nil && info.Size() > 0 {
			return p, nil
		}
	}

	return "", fmt.Errorf("lsmanager: no local Antigravity installation found")
}

// extractFromZip extracts the LS binary from an Antigravity zip archive.
func extractFromZip(zipPath, destDir string) (string, error) {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", fmt.Errorf("lsmanager: open zip: %w", err)
	}
	defer r.Close()

	target := antigravityExtensionBinaryPath()
	for _, f := range r.File {
		if !strings.HasSuffix(f.Name, target) && f.Name != target {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", fmt.Errorf("lsmanager: open entry: %w", err)
		}

		dest := filepath.Join(destDir, binaryName())
		out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			rc.Close()
			return "", fmt.Errorf("lsmanager: create file: %w", err)
		}
		if _, err := io.Copy(out, rc); err != nil {
			out.Close()
			rc.Close()
			return "", fmt.Errorf("lsmanager: copy: %w", err)
		}
		out.Close()
		rc.Close()
		return dest, nil
	}

	return "", fmt.Errorf("lsmanager: binary not found in archive")
}

// downloadFile downloads a URL to a local file.
func downloadFile(url, dest string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}

	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(f, resp.Body)
	return err
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
