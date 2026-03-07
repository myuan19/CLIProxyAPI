package lsclient

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	log "github.com/sirupsen/logrus"
)

// DiscoveryData mirrors the Language Server's discovery file that it writes
// after startup at ~/.gemini/{appDataDir}/daemon/ls_{hash}.json.
type DiscoveryData struct {
	HTTPSPort int    `json:"httpsPort"`
	HTTPPort  int    `json:"httpPort"`
	LSPPort   int    `json:"lspPort"`
	PID       int    `json:"pid"`
	LSVersion string `json:"lsVersion"`
	CSRFToken string `json:"csrfToken"`
}

// DiscoveryFilePath computes the discovery file path for a given workspace.
// The LS writes this file at ~/.gemini/{appDataDir}/daemon/ls_{sha256(workspaceId)[:16]}.json
func DiscoveryFilePath(appDataDir, workspaceID string) string {
	hash := sha256.Sum256([]byte(workspaceID))
	hashHex := fmt.Sprintf("%x", hash[:])[:16]

	home, err := os.UserHomeDir()
	if err != nil {
		log.WithError(err).Error("lsclient: failed to determine home dir for discovery path")
		home = os.TempDir()
	}
	return filepath.Join(home, ".gemini", appDataDir, "daemon", fmt.Sprintf("ls_%s.json", hashHex))
}

// WaitForDiscovery polls the discovery file until it becomes available,
// the timeout is reached, or the context is cancelled.
func WaitForDiscovery(ctx context.Context, path string, timeout time.Duration) (*DiscoveryData, error) {
	deadline := time.Now().Add(timeout)
	pollInterval := 200 * time.Millisecond

	log.WithField("path", path).Info("lsclient: waiting for LS discovery file")

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		data, err := os.ReadFile(path)
		if err == nil && len(data) > 2 {
			var d DiscoveryData
			if err := json.Unmarshal(data, &d); err == nil && d.HTTPSPort > 0 {
				log.WithFields(log.Fields{
					"httpsPort": d.HTTPSPort,
					"httpPort":  d.HTTPPort,
					"lspPort":   d.LSPPort,
					"pid":       d.PID,
				}).Info("lsclient: LS discovery file found")
				return &d, nil
			}
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
	return nil, fmt.Errorf("lsclient: discovery file not found after %v at %s", timeout, path)
}

// CleanupDiscovery removes a stale discovery file to force the LS to create a new one.
func CleanupDiscovery(path string) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		log.WithError(err).WithField("path", path).Debug("lsclient: failed to cleanup discovery file")
	}
}
