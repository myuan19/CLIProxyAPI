//go:build windows

package lsmanager

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/mitm/redirect"
	log "github.com/sirupsen/logrus"
)

func needsDLLInjection() bool {
	return true
}

// startWithDLLInjection launches the Language Server with a suspended process,
// injects the DNS redirect DLL, then resumes the process.
func (m *Manager) startWithDLLInjection(ctx context.Context, lsPath string) (*exec.Cmd, error) {
	if m.cfg.RedirectLibPath == "" {
		return nil, fmt.Errorf("lsmanager: redirect lib path required for Windows DLL injection")
	}

	cmd := exec.CommandContext(ctx, lsPath, m.buildArgs()...)
	cmd.Dir = m.cfg.DataDir
	cmd.Env = m.buildEnv()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	const createSuspended = 0x00000004
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: createSuspended,
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("lsmanager: start suspended LS: %w", err)
	}

	pid := uint32(cmd.Process.Pid)
	log.WithField("pid", pid).Info("lsmanager: LS started suspended, injecting DLL")

	if err := redirect.InjectDLL(pid, m.cfg.RedirectLibPath); err != nil {
		cmd.Process.Kill()
		return nil, fmt.Errorf("lsmanager: DLL injection failed: %w", err)
	}

	// Resume the main thread after injection.
	// We need to use kernel32.ResumeThread, but since we don't have the thread handle
	// from exec.Cmd, we use NtResumeProcess as a workaround.
	if err := resumeProcess(pid); err != nil {
		cmd.Process.Kill()
		return nil, fmt.Errorf("lsmanager: resume process failed: %w", err)
	}

	log.WithField("pid", pid).Info("lsmanager: DLL injected, LS resumed")
	return cmd, nil
}

func resumeProcess(pid uint32) error {
	ntdll := syscall.NewLazyDLL("ntdll.dll")
	ntResumeProcess := ntdll.NewProc("NtResumeProcess")

	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	openProcess := kernel32.NewProc("OpenProcess")
	closeHandle := kernel32.NewProc("CloseHandle")

	const PROCESS_SUSPEND_RESUME = 0x0800

	hProcess, _, err := openProcess.Call(
		uintptr(PROCESS_SUSPEND_RESUME),
		0,
		uintptr(pid),
	)
	if hProcess == 0 {
		return fmt.Errorf("OpenProcess for resume: %w", err)
	}
	defer closeHandle.Call(hProcess)

	ret, _, err := ntResumeProcess.Call(hProcess)
	if ret != 0 {
		return fmt.Errorf("NtResumeProcess: %w (status: 0x%x)", err, ret)
	}

	return nil
}
