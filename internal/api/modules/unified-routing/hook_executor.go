package unifiedrouting

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

// ================== Hook Configuration Types ==================

// HookConfig represents a hook binding: a route + a hook folder + trigger conditions.
type HookConfig struct {
	ID        string            `json:"id" yaml:"id"`
	RouteID   string            `json:"route_id" yaml:"route-id"`
	Name      string            `json:"name" yaml:"name"`
	Enabled   bool              `json:"enabled" yaml:"enabled"`
	Trigger   HookTrigger       `json:"trigger" yaml:"trigger"`
	HookDir   string            `json:"hook_dir" yaml:"hook-dir"`
	Params    map[string]string `json:"params,omitempty" yaml:"params,omitempty"`
	TimeoutS  int               `json:"timeout_seconds,omitempty" yaml:"timeout-seconds,omitempty"`
	CreatedAt time.Time         `json:"created_at" yaml:"-"`
	UpdatedAt time.Time         `json:"updated_at" yaml:"-"`
}

// HookTrigger defines the conditions to fire a hook.
type HookTrigger struct {
	On            string `json:"on" yaml:"on"`
	StatusCodes   []int  `json:"status_codes,omitempty" yaml:"status-codes,omitempty"`
	ErrorContains string `json:"error_contains,omitempty" yaml:"error-contains,omitempty"`
}

// HookExecutionLog records one hook execution.
type HookExecutionLog struct {
	ID            string    `json:"id"`
	HookID        string    `json:"hook_id"`
	HookName      string    `json:"hook_name"`
	RouteID       string    `json:"route_id"`
	RouteName     string    `json:"route_name"`
	TargetID      string    `json:"target_id"`
	CredentialID  string    `json:"credential_id"`
	Model         string    `json:"model"`
	TriggerReason string    `json:"trigger_reason"`
	StatusCode    int       `json:"status_code,omitempty"`
	ErrorMessage  string    `json:"error_message,omitempty"`
	HookDir       string    `json:"hook_dir"`
	Script        string    `json:"script"`
	ExitCode      int       `json:"exit_code"`
	Stdout        string    `json:"stdout"`
	Stderr        string    `json:"stderr"`
	Success       bool      `json:"success"`
	DurationMs    int64     `json:"duration_ms"`
	Timestamp     time.Time `json:"timestamp"`
}

// HookAttemptEvent carries context about the attempt that may fire hooks.
type HookAttemptEvent struct {
	RouteID      string
	RouteName    string
	TargetID     string
	CredentialID string
	Model        string
	StatusCode   int
	Err          error
	Success      bool
}

// HookParamDef describes a parameter that a hook folder declares in params.json.
type HookParamDef struct {
	Name        string   `json:"name"`
	Label       string   `json:"label,omitempty"`
	Description string   `json:"description,omitempty"`
	Type        string   `json:"type,omitempty"`  // "text", "select", "number", "password"
	Default     string   `json:"default,omitempty"`
	Options     []string `json:"options,omitempty"` // for type=select
	Required    bool     `json:"required,omitempty"`
}

// HookDirInfo describes an available hook folder discovered on disk.
type HookDirInfo struct {
	Name    string          `json:"name"`
	Path    string          `json:"path"`
	HasRun  bool            `json:"has_run"`
	Files   []string        `json:"files,omitempty"`
	Readme  string          `json:"readme,omitempty"`
	Params  []HookParamDef  `json:"params,omitempty"`
}

// ================== Hook Executor ==================

const (
	defaultHookTimeout = 30
	maxLogFiles        = 500
	maxStdoutBytes     = 64 * 1024
	maxReadmeBytes     = 4 * 1024
)

// HookExecutor manages hook configuration, folder scanning, and execution.
type HookExecutor struct {
	mu         sync.RWMutex
	store      *FileConfigStore
	scriptsDir string // base dir for hook folders (each sub-folder = one hook)
	logsDir    string // execution logs
}

// NewHookExecutor creates a new hook executor.
//   - store: config store (hook YAML configs saved alongside routes)
//   - scriptsDir: the directory that contains hook sub-folders (each with run.sh)
//   - logsDir: base logs directory; hook logs go into logsDir/hook-logs/
func NewHookExecutor(store *FileConfigStore, scriptsDir string, logsDir string) (*HookExecutor, error) {
	if err := os.MkdirAll(scriptsDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create hook-scripts dir: %w", err)
	}
	hooksLogDir := filepath.Join(logsDir, "hook-logs")
	if err := os.MkdirAll(hooksLogDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create hook logs dir: %w", err)
	}
	return &HookExecutor{
		store:      store,
		scriptsDir: scriptsDir,
		logsDir:    hooksLogDir,
	}, nil
}

// ScriptsDir returns the hook-scripts base directory.
func (e *HookExecutor) ScriptsDir() string {
	return e.scriptsDir
}

// ================== Folder scanning ==================

// ListAvailableDirs scans scriptsDir for sub-directories.
// A valid hook folder must contain a run.sh file.
func (e *HookExecutor) ListAvailableDirs() ([]*HookDirInfo, error) {
	entries, err := os.ReadDir(e.scriptsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []*HookDirInfo{}, nil
		}
		return nil, err
	}

	var dirs []*HookDirInfo
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dirPath := filepath.Join(e.scriptsDir, entry.Name())
		info := &HookDirInfo{
			Name: entry.Name(),
			Path: dirPath,
		}

		// Check run.sh
		runPath := filepath.Join(dirPath, "run.sh")
		if _, err := os.Stat(runPath); err == nil {
			info.HasRun = true
		}

		// List files (top-level only, limit to 20)
		subEntries, _ := os.ReadDir(dirPath)
		for i, se := range subEntries {
			if i >= 20 {
				info.Files = append(info.Files, fmt.Sprintf("... and %d more", len(subEntries)-20))
				break
			}
			name := se.Name()
			if se.IsDir() {
				name += "/"
			}
			info.Files = append(info.Files, name)
		}

		// Read README if exists
		for _, readmeName := range []string{"README.md", "README", "readme.md", "readme.txt"} {
			readmePath := filepath.Join(dirPath, readmeName)
			data, err := os.ReadFile(readmePath)
			if err == nil {
				if len(data) > maxReadmeBytes {
					data = data[:maxReadmeBytes]
				}
				info.Readme = string(data)
				break
			}
		}

		// Read params.json if exists
		paramsPath := filepath.Join(dirPath, "params.json")
		if paramsData, err := os.ReadFile(paramsPath); err == nil {
			var params []HookParamDef
			if err := json.Unmarshal(paramsData, &params); err == nil {
				info.Params = params
			}
		}

		dirs = append(dirs, info)
	}
	return dirs, nil
}

// ================== Hook config CRUD ==================

// configDir returns the directory for hook config YAML files (separate from script folders).
func (e *HookExecutor) configDir() string {
	return filepath.Join(e.store.baseDir, "hook-configs")
}

func (e *HookExecutor) ensureConfigDir() error {
	return os.MkdirAll(e.configDir(), 0755)
}

func (e *HookExecutor) hookFilePath(hookID string) string {
	return filepath.Join(e.configDir(), hookID+".yaml")
}

// ListHooks lists all hook configs, optionally filtered by routeID.
func (e *HookExecutor) ListHooks(routeID string) ([]*HookConfig, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	if err := e.ensureConfigDir(); err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(e.configDir())
	if err != nil {
		if os.IsNotExist(err) {
			return []*HookConfig{}, nil
		}
		return nil, err
	}

	var hooks []*HookConfig
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".yaml" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(e.configDir(), entry.Name()))
		if err != nil {
			continue
		}
		var h HookConfig
		if err := yaml.Unmarshal(data, &h); err != nil {
			continue
		}
		if routeID != "" && h.RouteID != routeID {
			continue
		}
		hooks = append(hooks, &h)
	}
	return hooks, nil
}

// GetHook gets a single hook by ID.
func (e *HookExecutor) GetHook(hookID string) (*HookConfig, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	data, err := os.ReadFile(e.hookFilePath(hookID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("hook not found: %s", hookID)
		}
		return nil, err
	}
	var h HookConfig
	if err := yaml.Unmarshal(data, &h); err != nil {
		return nil, err
	}
	return &h, nil
}

// CreateHook creates a new hook config.
func (e *HookExecutor) CreateHook(h *HookConfig) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if err := e.ensureConfigDir(); err != nil {
		return err
	}
	if h.ID == "" {
		h.ID = "hook-" + generateShortID()
	}
	if h.TimeoutS <= 0 {
		h.TimeoutS = defaultHookTimeout
	}

	// Validate hook_dir exists and has run.sh
	if h.HookDir == "" {
		return fmt.Errorf("hook_dir is required")
	}
	runPath := filepath.Join(e.scriptsDir, h.HookDir, "run.sh")
	if _, err := os.Stat(runPath); os.IsNotExist(err) {
		return fmt.Errorf("hook folder %q does not contain run.sh (expected: %s)", h.HookDir, runPath)
	}

	h.CreatedAt = time.Now()
	h.UpdatedAt = h.CreatedAt

	data, err := yaml.Marshal(h)
	if err != nil {
		return err
	}
	return os.WriteFile(e.hookFilePath(h.ID), data, 0644)
}

// UpdateHook updates an existing hook config.
func (e *HookExecutor) UpdateHook(h *HookConfig) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	existing, err := os.ReadFile(e.hookFilePath(h.ID))
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("hook not found: %s", h.ID)
		}
		return err
	}

	// Preserve created_at
	var old HookConfig
	if err := yaml.Unmarshal(existing, &old); err == nil {
		h.CreatedAt = old.CreatedAt
	}

	if h.TimeoutS <= 0 {
		h.TimeoutS = defaultHookTimeout
	}
	h.UpdatedAt = time.Now()

	data, err := yaml.Marshal(h)
	if err != nil {
		return err
	}
	return os.WriteFile(e.hookFilePath(h.ID), data, 0644)
}

// DeleteHook deletes a hook config by ID.
func (e *HookExecutor) DeleteHook(hookID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if err := os.Remove(e.hookFilePath(hookID)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ================== Trigger & Execute ==================

// ManualTriggerRequest carries user-supplied simulated event data for manual hook execution.
type ManualTriggerRequest struct {
	RouteID      string `json:"route_id"`
	RouteName    string `json:"route_name"`
	TargetID     string `json:"target_id"`
	CredentialID string `json:"credential_id"`
	Model        string `json:"model"`
	StatusCode   int    `json:"status_code"`
	ErrorMessage string `json:"error_message"`
}

// ManualTrigger synchronously executes a specific hook with user-supplied simulated data.
// Unlike EvaluateAndRun, it bypasses trigger condition matching and runs the hook directly.
func (e *HookExecutor) ManualTrigger(hookID string, req ManualTriggerRequest) (*HookExecutionLog, error) {
	h, err := e.GetHook(hookID)
	if err != nil {
		return nil, err
	}

	evt := HookAttemptEvent{
		RouteID:      req.RouteID,
		RouteName:    req.RouteName,
		TargetID:     req.TargetID,
		CredentialID: req.CredentialID,
		Model:        req.Model,
		StatusCode:   req.StatusCode,
		Success:      false,
	}
	if req.ErrorMessage != "" {
		evt.Err = fmt.Errorf("%s", req.ErrorMessage)
	}

	// If route_name not provided, try to fill it from route_id
	if evt.RouteName == "" && evt.RouteID != "" {
		evt.RouteName = evt.RouteID
	}

	reason := fmt.Sprintf("manual_trigger (hook_id=%s)", hookID)
	return e.executeSync(h, evt, reason)
}

// executeSync runs a hook synchronously and returns the log entry.
func (e *HookExecutor) executeSync(h *HookConfig, evt HookAttemptEvent, reason string) (*HookExecutionLog, error) {
	start := time.Now()
	scriptPath := filepath.Join(e.scriptsDir, h.HookDir, "run.sh")

	logEntry := HookExecutionLog{
		ID:            fmt.Sprintf("hlog-%s", generateShortID()),
		HookID:        h.ID,
		HookName:      h.Name,
		RouteID:       evt.RouteID,
		RouteName:     evt.RouteName,
		TargetID:      evt.TargetID,
		CredentialID:  evt.CredentialID,
		Model:         evt.Model,
		TriggerReason: reason,
		StatusCode:    evt.StatusCode,
		HookDir:       h.HookDir,
		Script:        scriptPath,
		Timestamp:     start,
	}
	if evt.Err != nil {
		logEntry.ErrorMessage = evt.Err.Error()
	}

	timeout := time.Duration(h.TimeoutS) * time.Second
	if timeout <= 0 {
		timeout = time.Duration(defaultHookTimeout) * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", scriptPath)
	cmd.Dir = filepath.Join(e.scriptsDir, h.HookDir)

	cmd.Env = append(os.Environ(),
		"HOOK_ID="+h.ID,
		"HOOK_NAME="+h.Name,
		"HOOK_DIR="+h.HookDir,
		"ROUTE_ID="+evt.RouteID,
		"ROUTE_NAME="+evt.RouteName,
		"TARGET_ID="+evt.TargetID,
		"CREDENTIAL_ID="+evt.CredentialID,
		"MODEL="+evt.Model,
		fmt.Sprintf("STATUS_CODE=%d", evt.StatusCode),
		"TRIGGER_REASON="+reason,
		"MANUAL_TRIGGER=true",
	)
	if evt.Err != nil {
		cmd.Env = append(cmd.Env, "ERROR_MESSAGE="+evt.Err.Error())
	}
	for k, v := range h.Params {
		cmd.Env = append(cmd.Env, fmt.Sprintf("PARAM_%s=%s", strings.ToUpper(k), v))
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	cmdErr := cmd.Run()
	logEntry.DurationMs = time.Since(start).Milliseconds()

	if stdout.Len() > maxStdoutBytes {
		logEntry.Stdout = string(stdout.Bytes()[:maxStdoutBytes]) + "\n... (truncated)"
	} else {
		logEntry.Stdout = stdout.String()
	}
	if stderr.Len() > maxStdoutBytes {
		logEntry.Stderr = string(stderr.Bytes()[:maxStdoutBytes]) + "\n... (truncated)"
	} else {
		logEntry.Stderr = stderr.String()
	}

	if cmdErr != nil {
		logEntry.Success = false
		if exitErr, ok := cmdErr.(*exec.ExitError); ok {
			logEntry.ExitCode = exitErr.ExitCode()
		} else {
			logEntry.ExitCode = -1
		}
		log.Warnf("[Hook] Manual trigger %q (%s/run.sh) failed: %v", h.Name, h.HookDir, cmdErr)
	} else {
		logEntry.Success = true
		logEntry.ExitCode = 0
		log.Infof("[Hook] Manual trigger %q (%s/run.sh) completed (%dms)", h.Name, h.HookDir, logEntry.DurationMs)
	}

	e.saveLog(&logEntry)
	return &logEntry, nil
}

// StreamCallback is called for each line of output during streaming execution.
// stream is "stdout" or "stderr"; line is the text content.
type StreamCallback func(stream string, line string)

// ManualTriggerStream executes a hook synchronously, calling onLine for each stdout/stderr line in real-time.
// Returns the final HookExecutionLog when the script finishes.
func (e *HookExecutor) ManualTriggerStream(hookID string, req ManualTriggerRequest, onLine StreamCallback) (*HookExecutionLog, error) {
	h, err := e.GetHook(hookID)
	if err != nil {
		return nil, err
	}

	evt := HookAttemptEvent{
		RouteID:      req.RouteID,
		RouteName:    req.RouteName,
		TargetID:     req.TargetID,
		CredentialID: req.CredentialID,
		Model:        req.Model,
		StatusCode:   req.StatusCode,
		Success:      false,
	}
	if req.ErrorMessage != "" {
		evt.Err = fmt.Errorf("%s", req.ErrorMessage)
	}
	if evt.RouteName == "" && evt.RouteID != "" {
		evt.RouteName = evt.RouteID
	}

	reason := fmt.Sprintf("manual_trigger (hook_id=%s)", hookID)
	return e.executeStream(h, evt, reason, onLine)
}

// executeStream runs a hook and streams stdout/stderr line-by-line via onLine.
func (e *HookExecutor) executeStream(h *HookConfig, evt HookAttemptEvent, reason string, onLine StreamCallback) (*HookExecutionLog, error) {
	start := time.Now()
	scriptPath := filepath.Join(e.scriptsDir, h.HookDir, "run.sh")

	logEntry := HookExecutionLog{
		ID:            fmt.Sprintf("hlog-%s", generateShortID()),
		HookID:        h.ID,
		HookName:      h.Name,
		RouteID:       evt.RouteID,
		RouteName:     evt.RouteName,
		TargetID:      evt.TargetID,
		CredentialID:  evt.CredentialID,
		Model:         evt.Model,
		TriggerReason: reason,
		StatusCode:    evt.StatusCode,
		HookDir:       h.HookDir,
		Script:        scriptPath,
		Timestamp:     start,
	}
	if evt.Err != nil {
		logEntry.ErrorMessage = evt.Err.Error()
	}

	timeout := time.Duration(h.TimeoutS) * time.Second
	if timeout <= 0 {
		timeout = time.Duration(defaultHookTimeout) * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", scriptPath)
	cmd.Dir = filepath.Join(e.scriptsDir, h.HookDir)
	cmd.Env = append(os.Environ(),
		"HOOK_ID="+h.ID,
		"HOOK_NAME="+h.Name,
		"HOOK_DIR="+h.HookDir,
		"ROUTE_ID="+evt.RouteID,
		"ROUTE_NAME="+evt.RouteName,
		"TARGET_ID="+evt.TargetID,
		"CREDENTIAL_ID="+evt.CredentialID,
		"MODEL="+evt.Model,
		fmt.Sprintf("STATUS_CODE=%d", evt.StatusCode),
		"TRIGGER_REASON="+reason,
		"MANUAL_TRIGGER=true",
	)
	if evt.Err != nil {
		cmd.Env = append(cmd.Env, "ERROR_MESSAGE="+evt.Err.Error())
	}
	for k, v := range h.Params {
		cmd.Env = append(cmd.Env, fmt.Sprintf("PARAM_%s=%s", strings.ToUpper(k), v))
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		logEntry.ExitCode = -1
		logEntry.DurationMs = time.Since(start).Milliseconds()
		logEntry.Stderr = err.Error()
		e.saveLog(&logEntry)
		return &logEntry, nil
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	var wg sync.WaitGroup

	scanPipe := func(pipe io.ReadCloser, stream string, buf *bytes.Buffer) {
		defer wg.Done()
		scanner := bufio.NewScanner(pipe)
		scanner.Buffer(make([]byte, 0, 64*1024), 256*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if buf.Len() < maxStdoutBytes {
				buf.WriteString(line + "\n")
			}
			if onLine != nil {
				onLine(stream, line)
			}
		}
	}

	wg.Add(2)
	go scanPipe(stdoutPipe, "stdout", &stdoutBuf)
	go scanPipe(stderrPipe, "stderr", &stderrBuf)
	wg.Wait()

	cmdErr := cmd.Wait()
	logEntry.DurationMs = time.Since(start).Milliseconds()
	logEntry.Stdout = stdoutBuf.String()
	logEntry.Stderr = stderrBuf.String()

	if cmdErr != nil {
		logEntry.Success = false
		if exitErr, ok := cmdErr.(*exec.ExitError); ok {
			logEntry.ExitCode = exitErr.ExitCode()
		} else {
			logEntry.ExitCode = -1
		}
		log.Warnf("[Hook] Stream trigger %q (%s/run.sh) failed: %v", h.Name, h.HookDir, cmdErr)
	} else {
		logEntry.Success = true
		logEntry.ExitCode = 0
		log.Infof("[Hook] Stream trigger %q (%s/run.sh) completed (%dms)", h.Name, h.HookDir, logEntry.DurationMs)
	}

	e.saveLog(&logEntry)
	return &logEntry, nil
}

// EvaluateAndRun checks all hooks for the given route and fires any matching ones asynchronously.
func (e *HookExecutor) EvaluateAndRun(evt HookAttemptEvent) {
	hooks, err := e.ListHooks(evt.RouteID)
	if err != nil || len(hooks) == 0 {
		return
	}

	for _, h := range hooks {
		if !h.Enabled {
			continue
		}
		if reason, ok := e.matches(h, evt); ok {
			go e.execute(h, evt, reason)
		}
	}
}

func (e *HookExecutor) matches(h *HookConfig, evt HookAttemptEvent) (string, bool) {
	t := h.Trigger

	switch t.On {
	case "failure":
		if evt.Success {
			return "", false
		}
	case "success":
		if !evt.Success {
			return "", false
		}
	case "any":
		// always proceed
	default:
		if evt.Success {
			return "", false
		}
	}

	if len(t.StatusCodes) > 0 {
		found := false
		for _, code := range t.StatusCodes {
			if code == evt.StatusCode {
				found = true
				break
			}
		}
		if !found {
			return "", false
		}
		return fmt.Sprintf("status_code=%d matched %v", evt.StatusCode, t.StatusCodes), true
	}

	if t.ErrorContains != "" && evt.Err != nil {
		if strings.Contains(strings.ToLower(evt.Err.Error()), strings.ToLower(t.ErrorContains)) {
			return fmt.Sprintf("error contains '%s'", t.ErrorContains), true
		}
		return "", false
	}

	if evt.Success {
		return "request succeeded", true
	}
	errMsg := ""
	if evt.Err != nil {
		errMsg = evt.Err.Error()
	}
	return fmt.Sprintf("request failed: %s", errMsg), true
}

// execute runs run.sh inside the hook's folder and records the log.
func (e *HookExecutor) execute(h *HookConfig, evt HookAttemptEvent, reason string) {
	start := time.Now()
	scriptPath := filepath.Join(e.scriptsDir, h.HookDir, "run.sh")

	logEntry := HookExecutionLog{
		ID:            fmt.Sprintf("hlog-%s", generateShortID()),
		HookID:        h.ID,
		HookName:      h.Name,
		RouteID:       evt.RouteID,
		RouteName:     evt.RouteName,
		TargetID:      evt.TargetID,
		CredentialID:  evt.CredentialID,
		Model:         evt.Model,
		TriggerReason: reason,
		StatusCode:    evt.StatusCode,
		HookDir:       h.HookDir,
		Script:        scriptPath,
		Timestamp:     start,
	}
	if evt.Err != nil {
		logEntry.ErrorMessage = evt.Err.Error()
	}

	timeout := time.Duration(h.TimeoutS) * time.Second
	if timeout <= 0 {
		timeout = time.Duration(defaultHookTimeout) * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", scriptPath)
	cmd.Dir = filepath.Join(e.scriptsDir, h.HookDir)

	cmd.Env = append(os.Environ(),
		"HOOK_ID="+h.ID,
		"HOOK_NAME="+h.Name,
		"HOOK_DIR="+h.HookDir,
		"ROUTE_ID="+evt.RouteID,
		"ROUTE_NAME="+evt.RouteName,
		"TARGET_ID="+evt.TargetID,
		"CREDENTIAL_ID="+evt.CredentialID,
		"MODEL="+evt.Model,
		fmt.Sprintf("STATUS_CODE=%d", evt.StatusCode),
		"TRIGGER_REASON="+reason,
	)
	if evt.Err != nil {
		cmd.Env = append(cmd.Env, "ERROR_MESSAGE="+evt.Err.Error())
	}
	// Inject custom params as PARAM_<NAME> env vars
	for k, v := range h.Params {
		cmd.Env = append(cmd.Env, fmt.Sprintf("PARAM_%s=%s", strings.ToUpper(k), v))
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	logEntry.DurationMs = time.Since(start).Milliseconds()

	if stdout.Len() > maxStdoutBytes {
		logEntry.Stdout = string(stdout.Bytes()[:maxStdoutBytes]) + "\n... (truncated)"
	} else {
		logEntry.Stdout = stdout.String()
	}
	if stderr.Len() > maxStdoutBytes {
		logEntry.Stderr = string(stderr.Bytes()[:maxStdoutBytes]) + "\n... (truncated)"
	} else {
		logEntry.Stderr = stderr.String()
	}

	if err != nil {
		logEntry.Success = false
		if exitErr, ok := err.(*exec.ExitError); ok {
			logEntry.ExitCode = exitErr.ExitCode()
		} else {
			logEntry.ExitCode = -1
		}
		log.Warnf("[Hook] %q (%s/run.sh) failed for route %s: %v", h.Name, h.HookDir, evt.RouteID, err)
	} else {
		logEntry.Success = true
		logEntry.ExitCode = 0
		log.Infof("[Hook] %q (%s/run.sh) executed for route %s (reason: %s, %dms)", h.Name, h.HookDir, evt.RouteID, reason, logEntry.DurationMs)
	}

	e.saveLog(&logEntry)
}

// ================== Log Persistence ==================

func (e *HookExecutor) saveLog(entry *HookExecutionLog) {
	data, err := json.MarshalIndent(entry, "", "    ")
	if err != nil {
		log.Errorf("[Hook] failed to marshal log: %v", err)
		return
	}
	fname := fmt.Sprintf("%s-%s.json", entry.Timestamp.Format("2006-01-02T150405"), entry.ID)
	fpath := filepath.Join(e.logsDir, fname)
	if err := os.WriteFile(fpath, data, 0644); err != nil {
		log.Errorf("[Hook] failed to write log file: %v", err)
	}

	go e.cleanupLogs()
}

func (e *HookExecutor) cleanupLogs() {
	entries, err := os.ReadDir(e.logsDir)
	if err != nil {
		return
	}
	var jsonFiles []os.DirEntry
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			jsonFiles = append(jsonFiles, entry)
		}
	}
	if len(jsonFiles) <= maxLogFiles {
		return
	}
	sort.Slice(jsonFiles, func(i, j int) bool {
		return jsonFiles[i].Name() < jsonFiles[j].Name()
	})
	toDelete := len(jsonFiles) - maxLogFiles
	for i := 0; i < toDelete; i++ {
		_ = os.Remove(filepath.Join(e.logsDir, jsonFiles[i].Name()))
	}
}

// ListLogs returns hook execution logs, newest first.
func (e *HookExecutor) ListLogs(routeID, hookID string, limit int) ([]*HookExecutionLog, error) {
	entries, err := os.ReadDir(e.logsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []*HookExecutionLog{}, nil
		}
		return nil, err
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() > entries[j].Name()
	})

	if limit <= 0 {
		limit = 50
	}

	var logs []*HookExecutionLog
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(e.logsDir, entry.Name()))
		if err != nil {
			continue
		}
		var logEntry HookExecutionLog
		if err := json.Unmarshal(data, &logEntry); err != nil {
			continue
		}
		if routeID != "" && logEntry.RouteID != routeID {
			continue
		}
		if hookID != "" && logEntry.HookID != hookID {
			continue
		}
		logs = append(logs, &logEntry)
		if len(logs) >= limit {
			break
		}
	}
	return logs, nil
}

// ClearLogs clears all hook execution logs.
func (e *HookExecutor) ClearLogs() error {
	entries, err := os.ReadDir(e.logsDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			_ = os.Remove(filepath.Join(e.logsDir, entry.Name()))
		}
	}
	return nil
}
