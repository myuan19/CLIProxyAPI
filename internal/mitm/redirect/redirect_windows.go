//go:build windows

package redirect

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"
)

func buildLibrary(dataDir string) error {
	srcDir := findNativeSourceDir()
	if srcDir == "" {
		return fmt.Errorf("native source directory not found; please provide a precompiled dns_redirect.dll")
	}

	src := filepath.Join(srcDir, "dns_redirect.c")
	out := filepath.Join(dataDir, "dns_redirect.dll")

	cmd := exec.Command("gcc", "-shared", "-o", out, src, "-lws2_32", "-Wall", "-O2")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gcc compilation failed: %w; ensure MinGW is installed", err)
	}
	return nil
}

func findNativeSourceDir() string {
	_, thisFile, _, _ := runtime.Caller(0)
	candidate := filepath.Join(filepath.Dir(thisFile), "native")
	if _, err := os.Stat(filepath.Join(candidate, "dns_redirect.c")); err == nil {
		return candidate
	}
	for _, p := range []string{
		"internal\\mitm\\redirect\\native",
		"redirect\\native",
	} {
		if _, err := os.Stat(filepath.Join(p, "dns_redirect.c")); err == nil {
			abs, _ := filepath.Abs(p)
			return abs
		}
	}
	return ""
}

// InjectDLL injects the DNS redirect DLL into a suspended process.
// The process should be created with CREATE_SUSPENDED flag.
func InjectDLL(pid uint32, dllPath string) error {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	openProcess := kernel32.NewProc("OpenProcess")
	virtualAllocEx := kernel32.NewProc("VirtualAllocEx")
	writeProcessMemory := kernel32.NewProc("WriteProcessMemory")
	createRemoteThread := kernel32.NewProc("CreateRemoteThread")
	closeHandle := kernel32.NewProc("CloseHandle")

	const (
		PROCESS_ALL_ACCESS = 0x1F0FFF
		MEM_COMMIT         = 0x1000
		MEM_RESERVE        = 0x2000
		PAGE_READWRITE     = 0x04
	)

	hProcess, _, err := openProcess.Call(
		uintptr(PROCESS_ALL_ACCESS),
		0,
		uintptr(pid),
	)
	if hProcess == 0 {
		return fmt.Errorf("OpenProcess failed: %w", err)
	}
	defer closeHandle.Call(hProcess)

	dllPathBytes, err := syscall.UTF16FromString(dllPath)
	if err != nil {
		return fmt.Errorf("invalid dll path: %w", err)
	}
	dllPathSize := len(dllPathBytes) * 2

	remoteMem, _, err := virtualAllocEx.Call(
		hProcess,
		0,
		uintptr(dllPathSize),
		MEM_COMMIT|MEM_RESERVE,
		PAGE_READWRITE,
	)
	if remoteMem == 0 {
		return fmt.Errorf("VirtualAllocEx failed: %w", err)
	}

	written := uintptr(0)
	ret, _, err := writeProcessMemory.Call(
		hProcess,
		remoteMem,
		uintptr(unsafe.Pointer(&dllPathBytes[0])),
		uintptr(dllPathSize),
		uintptr(unsafe.Pointer(&written)),
	)
	if ret == 0 {
		return fmt.Errorf("WriteProcessMemory failed: %w", err)
	}

	loadLibraryAddr := kernel32.NewProc("LoadLibraryW").Addr()

	hThread, _, err := createRemoteThread.Call(
		hProcess,
		0,
		0,
		loadLibraryAddr,
		remoteMem,
		0,
		0,
	)
	if hThread == 0 {
		return fmt.Errorf("CreateRemoteThread failed: %w", err)
	}

	// Wait for the remote thread to finish loading the DLL.
	syscall.WaitForSingleObject(syscall.Handle(hThread), syscall.INFINITE)
	closeHandle.Call(hThread)

	return nil
}
