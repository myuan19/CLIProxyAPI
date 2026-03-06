//go:build !windows

package lsmanager

import (
	"context"
	"fmt"
	"os/exec"
)

func needsDLLInjection() bool {
	return false
}

func (m *Manager) startWithDLLInjection(_ context.Context, _ string) (*exec.Cmd, error) {
	return nil, fmt.Errorf("DLL injection not supported on this platform")
}
