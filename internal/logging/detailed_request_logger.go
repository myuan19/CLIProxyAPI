// Package logging provides request logging functionality for the CLI Proxy API server.
// This file implements a structured detailed request logger that stores each request as
// an individual JSON file for easy browsing, with automatic cleanup by file count and size.
package logging

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	// detailedFilePrefix is the prefix for individual detail log files.
	detailedFilePrefix = "detail-"

	// detailedFileSuffix is the file extension for detail log files.
	detailedFileSuffix = ".json"

	// legacyDetailedLogFileName is the old JSONL file name (for backward compatibility).
	legacyDetailedLogFileName = "detailed-requests.jsonl"

	// defaultDetailedMaxSizeMB is the default maximum total size of detail files in MB.
	defaultDetailedMaxSizeMB = 20

	// defaultDetailedMaxFiles is the default maximum number of detail files to keep.
	defaultDetailedMaxFiles = 500

	// detailedWriteBufferSize is the buffer size for the async write channel.
	detailedWriteBufferSize = 256

	// cleanupInterval controls how often cleanup runs (every N writes).
	cleanupInterval = 20
)

// DetailedRequestRecord represents a single proxied request with all retry attempts.
type DetailedRequestRecord struct {
	ID              string              `json:"id"`
	Timestamp       time.Time           `json:"timestamp"`
	APIKey          string              `json:"api_key"`
	APIKeyHash      string              `json:"api_key_hash"`
	URL             string              `json:"url"`
	Method          string              `json:"method"`
	StatusCode      int                 `json:"status_code"`
	Model           string              `json:"model,omitempty"`
	RequestHeaders  map[string][]string `json:"request_headers,omitempty"`
	RequestBody     string              `json:"request_body,omitempty"`
	ResponseHeaders map[string][]string `json:"response_headers,omitempty"`
	ResponseBody    string              `json:"response_body,omitempty"`
	Attempts        []DetailedAttempt   `json:"attempts,omitempty"`
	TotalDurationMs int64               `json:"total_duration_ms"`
	IsStreaming     bool                `json:"is_streaming"`
	Error           string              `json:"error,omitempty"`
}

// DetailedAttempt represents a single upstream attempt (initial or retry).
type DetailedAttempt struct {
	Index           int                 `json:"index"`
	Timestamp       time.Time           `json:"timestamp,omitempty"`
	UpstreamURL     string              `json:"upstream_url,omitempty"`
	Method          string              `json:"method,omitempty"`
	Auth            string              `json:"auth,omitempty"`
	RequestHeaders  map[string][]string `json:"request_headers,omitempty"`
	RequestBody     string              `json:"request_body,omitempty"`
	StatusCode      int                 `json:"status_code,omitempty"`
	ResponseHeaders map[string][]string `json:"response_headers,omitempty"`
	ResponseBody    string              `json:"response_body,omitempty"`
	Error           string              `json:"error,omitempty"`
	DurationMs      int64               `json:"duration_ms,omitempty"`
}

// DetailedRequestLogger handles structured logging of detailed request records
// as individual JSON files in the logs directory.
type DetailedRequestLogger struct {
	mu           sync.Mutex
	enabled      bool
	logsDir      string
	maxSizeMB    int
	maxFiles     int
	writeCh      chan *DetailedRequestRecord
	stopCh       chan struct{}
	stopped      bool
	writeCount   int64 // counts writes for periodic cleanup
	migrated     bool  // whether legacy JSONL has been checked
}

// NewDetailedRequestLogger creates a new detailed request logger.
func NewDetailedRequestLogger(enabled bool, logsDir string, maxSizeMB int) *DetailedRequestLogger {
	if maxSizeMB <= 0 {
		maxSizeMB = defaultDetailedMaxSizeMB
	}
	dl := &DetailedRequestLogger{
		enabled:   enabled,
		logsDir:   logsDir,
		maxSizeMB: maxSizeMB,
		maxFiles:  defaultDetailedMaxFiles,
		writeCh:   make(chan *DetailedRequestRecord, detailedWriteBufferSize),
		stopCh:    make(chan struct{}),
	}
	go dl.writeLoop()
	return dl
}

// IsEnabled returns whether detailed request logging is enabled.
func (dl *DetailedRequestLogger) IsEnabled() bool {
	dl.mu.Lock()
	defer dl.mu.Unlock()
	return dl.enabled
}

// SetEnabled toggles detailed request logging on or off.
func (dl *DetailedRequestLogger) SetEnabled(enabled bool) {
	dl.mu.Lock()
	defer dl.mu.Unlock()
	dl.enabled = enabled
}

// SetMaxSizeMB updates the maximum total log size in MB.
func (dl *DetailedRequestLogger) SetMaxSizeMB(maxSizeMB int) {
	dl.mu.Lock()
	defer dl.mu.Unlock()
	if maxSizeMB <= 0 {
		maxSizeMB = defaultDetailedMaxSizeMB
	}
	dl.maxSizeMB = maxSizeMB
}

// LogRecord writes a detailed request record as an individual JSON file asynchronously.
func (dl *DetailedRequestLogger) LogRecord(record *DetailedRequestRecord) {
	if record == nil {
		return
	}
	dl.mu.Lock()
	if !dl.enabled || dl.stopped {
		dl.mu.Unlock()
		return
	}
	dl.mu.Unlock()

	select {
	case dl.writeCh <- record:
	default:
		log.Warn("detailed request log write channel full, dropping record")
	}
}

// Close stops the background writer and flushes remaining records.
func (dl *DetailedRequestLogger) Close() {
	dl.mu.Lock()
	if dl.stopped {
		dl.mu.Unlock()
		return
	}
	dl.stopped = true
	dl.mu.Unlock()
	close(dl.writeCh)
	<-dl.stopCh
}

// writeLoop is the background goroutine that writes records to disk.
func (dl *DetailedRequestLogger) writeLoop() {
	defer close(dl.stopCh)
	for record := range dl.writeCh {
		if err := dl.writeRecordFile(record); err != nil {
			log.WithError(err).Warn("failed to write detailed request record")
		}
	}
}

// writeRecordFile writes a single record as an individual JSON file.
func (dl *DetailedRequestLogger) writeRecordFile(record *DetailedRequestRecord) error {
	if err := os.MkdirAll(dl.logsDir, 0755); err != nil {
		return fmt.Errorf("failed to create logs directory: %w", err)
	}

	filename := dl.generateDetailFilename(record)
	filePath := filepath.Join(dl.logsDir, filename)

	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal record: %w", err)
	}
	data = append(data, '\n')

	if err := os.WriteFile(filePath, data, 0644); err != nil {
		return fmt.Errorf("failed to write record file: %w", err)
	}

	// Periodic cleanup
	dl.mu.Lock()
	dl.writeCount++
	shouldCleanup := dl.writeCount%cleanupInterval == 0
	dl.mu.Unlock()

	if shouldCleanup {
		dl.cleanupOldFiles()
	}

	return nil
}

// generateDetailFilename creates a filename for a detail log file.
// Format: detail-v1-chat-completions-2026-02-08T130145-42cf8292.json
func (dl *DetailedRequestLogger) generateDetailFilename(record *DetailedRequestRecord) string {
	path := record.URL
	if strings.Contains(path, "?") {
		path = strings.Split(path, "?")[0]
	}
	if strings.HasPrefix(path, "/") {
		path = path[1:]
	}
	sanitized := sanitizePathForFilename(path)

	timestamp := record.Timestamp.Format("2006-01-02T150405")
	id := record.ID
	if id == "" {
		id = fmt.Sprintf("%d", time.Now().UnixNano())
	}

	return fmt.Sprintf("%s%s-%s-%s%s", detailedFilePrefix, sanitized, timestamp, id, detailedFileSuffix)
}

// sanitizePathForFilename replaces characters that are not safe for filenames.
func sanitizePathForFilename(path string) string {
	sanitized := strings.ReplaceAll(path, "/", "-")
	sanitized = strings.ReplaceAll(sanitized, ":", "-")

	reg := regexp.MustCompile(`[<>:"|?*\s]`)
	sanitized = reg.ReplaceAllString(sanitized, "-")
	reg = regexp.MustCompile(`-+`)
	sanitized = reg.ReplaceAllString(sanitized, "-")
	sanitized = strings.Trim(sanitized, "-")

	if sanitized == "" {
		sanitized = "root"
	}
	return sanitized
}

// cleanupOldFiles removes the oldest detail files when limits are exceeded.
func (dl *DetailedRequestLogger) cleanupOldFiles() {
	entries, err := os.ReadDir(dl.logsDir)
	if err != nil {
		return
	}

	type fileInfo struct {
		name    string
		size    int64
		modTime time.Time
	}

	var files []fileInfo
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, detailedFilePrefix) || !strings.HasSuffix(name, detailedFileSuffix) {
			continue
		}
		info, errInfo := entry.Info()
		if errInfo != nil {
			continue
		}
		files = append(files, fileInfo{name: name, size: info.Size(), modTime: info.ModTime()})
	}

	if len(files) == 0 {
		return
	}

	// Sort by mod time, oldest first
	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime.Before(files[j].modTime)
	})

	dl.mu.Lock()
	maxFiles := dl.maxFiles
	maxBytes := int64(dl.maxSizeMB) * 1024 * 1024
	dl.mu.Unlock()

	// Calculate total size
	var totalSize int64
	for _, f := range files {
		totalSize += f.size
	}

	// Delete oldest files until within limits
	for len(files) > maxFiles || (totalSize > maxBytes && len(files) > 0) {
		oldest := files[0]
		if err := os.Remove(filepath.Join(dl.logsDir, oldest.name)); err == nil {
			totalSize -= oldest.size
		}
		files = files[1:]
	}
}

// listDetailFiles returns all detail-*.json files sorted by mod time (newest first).
func (dl *DetailedRequestLogger) listDetailFiles() ([]os.DirEntry, error) {
	entries, err := os.ReadDir(dl.logsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var detailFiles []os.DirEntry
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, detailedFilePrefix) && strings.HasSuffix(name, detailedFileSuffix) {
			detailFiles = append(detailFiles, entry)
		}
	}

	// Sort newest first by filename (timestamps in filenames ensure correct ordering)
	sort.Slice(detailFiles, func(i, j int) bool {
		// Use Info().ModTime for reliable sorting
		infoI, errI := detailFiles[i].Info()
		infoJ, errJ := detailFiles[j].Info()
		if errI != nil || errJ != nil {
			return detailFiles[i].Name() > detailFiles[j].Name()
		}
		return infoI.ModTime().After(infoJ.ModTime())
	})

	return detailFiles, nil
}

// readRecordFromFile reads and parses a single detail JSON file.
func (dl *DetailedRequestLogger) readRecordFromFile(filename string) (*DetailedRequestRecord, error) {
	data, err := os.ReadFile(filepath.Join(dl.logsDir, filename))
	if err != nil {
		return nil, err
	}
	var record DetailedRequestRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, err
	}
	return &record, nil
}

// ReadRecords reads records from individual detail files, applying optional filters.
// Returns records in reverse chronological order (newest first).
// Also reads from legacy JSONL file if it exists (for backward compatibility).
func (dl *DetailedRequestLogger) ReadRecords(filter RecordFilter) ([]DetailedRequestRecord, int, []string, error) {
	detailFiles, err := dl.listDetailFiles()
	if err != nil {
		return nil, 0, nil, fmt.Errorf("failed to list detail files: %w", err)
	}

	var allRecords []DetailedRequestRecord
	apiKeySet := make(map[string]struct{})

	// Read from individual files (already sorted newest first)
	for _, entry := range detailFiles {
		record, errRead := dl.readRecordFromFile(entry.Name())
		if errRead != nil {
			continue
		}
		allRecords = append(allRecords, *record)
		if record.APIKey != "" {
			apiKeySet[record.APIKey] = struct{}{}
		}
	}

	// Fallback: read legacy JSONL if it exists and no individual files
	legacyRecords := dl.readLegacyJSONL()
	if len(legacyRecords) > 0 {
		// Reverse for newest first
		for i, j := 0, len(legacyRecords)-1; i < j; i, j = i+1, j-1 {
			legacyRecords[i], legacyRecords[j] = legacyRecords[j], legacyRecords[i]
		}
		for _, r := range legacyRecords {
			if r.APIKey != "" {
				apiKeySet[r.APIKey] = struct{}{}
			}
		}
		allRecords = append(allRecords, legacyRecords...)
	}

	// Collect distinct API keys
	apiKeys := make([]string, 0, len(apiKeySet))
	for k := range apiKeySet {
		apiKeys = append(apiKeys, k)
	}

	// Apply filters
	filtered := dl.applyFilters(allRecords, filter)
	total := len(filtered)

	// Apply pagination
	if filter.Offset > 0 {
		if filter.Offset >= len(filtered) {
			filtered = []DetailedRequestRecord{}
		} else {
			filtered = filtered[filter.Offset:]
		}
	}
	if filter.Limit > 0 && len(filtered) > filter.Limit {
		filtered = filtered[:filter.Limit]
	}

	return filtered, total, apiKeys, nil
}

// ReadRecordByID reads a single record by its ID, checking individual files first,
// then falling back to the legacy JSONL file.
func (dl *DetailedRequestLogger) ReadRecordByID(id string) (*DetailedRequestRecord, error) {
	// First, try to find in individual files (ID is in filename)
	entries, err := os.ReadDir(dl.logsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read logs directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, detailedFilePrefix) || !strings.HasSuffix(name, detailedFileSuffix) {
			continue
		}
		// Quick check: ID should be in the filename
		if !strings.Contains(name, id) {
			continue
		}
		record, errRead := dl.readRecordFromFile(name)
		if errRead != nil {
			continue
		}
		if record.ID == id {
			return record, nil
		}
	}

	// Fallback: check legacy JSONL
	filePath := filepath.Join(dl.logsDir, legacyDetailedLogFileName)
	f, err := os.Open(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to open legacy detailed request log: %w", err)
	}
	defer func() {
		_ = f.Close()
	}()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.Contains(line, id) {
			continue
		}
		var record DetailedRequestRecord
		if errUnmarshal := json.Unmarshal([]byte(line), &record); errUnmarshal != nil {
			continue
		}
		if record.ID == id {
			return &record, nil
		}
	}

	return nil, nil
}

// DeleteAll removes all detail log files and the legacy JSONL file.
func (dl *DetailedRequestLogger) DeleteAll() error {
	entries, err := os.ReadDir(dl.logsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var lastErr error
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		// Remove individual detail files
		if strings.HasPrefix(name, detailedFilePrefix) && strings.HasSuffix(name, detailedFileSuffix) {
			if errRm := os.Remove(filepath.Join(dl.logsDir, name)); errRm != nil {
				lastErr = errRm
			}
		}
		// Remove legacy JSONL
		if name == legacyDetailedLogFileName {
			if errRm := os.Remove(filepath.Join(dl.logsDir, name)); errRm != nil {
				lastErr = errRm
			}
		}
	}

	return lastErr
}

// GetStats returns size information about all detail log files.
func (dl *DetailedRequestLogger) GetStats() (int64, int, error) {
	entries, err := os.ReadDir(dl.logsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, err
	}

	var totalSize int64
	var count int

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		isDetail := strings.HasPrefix(name, detailedFilePrefix) && strings.HasSuffix(name, detailedFileSuffix)
		isLegacy := name == legacyDetailedLogFileName
		if !isDetail && !isLegacy {
			continue
		}
		info, errInfo := entry.Info()
		if errInfo != nil {
			continue
		}
		totalSize += info.Size()
		if isDetail {
			count++
		} else if isLegacy {
			// Count lines in legacy file
			count += dl.countLegacyRecords()
		}
	}

	return totalSize, count, nil
}

// countLegacyRecords counts lines in the legacy JSONL file.
func (dl *DetailedRequestLogger) countLegacyRecords() int {
	filePath := filepath.Join(dl.logsDir, legacyDetailedLogFileName)
	f, err := os.Open(filePath)
	if err != nil {
		return 0
	}
	defer func() {
		_ = f.Close()
	}()

	count := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) != "" {
			count++
		}
	}
	return count
}

// readLegacyJSONL reads records from the legacy JSONL file.
func (dl *DetailedRequestLogger) readLegacyJSONL() []DetailedRequestRecord {
	filePath := filepath.Join(dl.logsDir, legacyDetailedLogFileName)
	f, err := os.Open(filePath)
	if err != nil {
		return nil
	}
	defer func() {
		_ = f.Close()
	}()

	var records []DetailedRequestRecord
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record DetailedRequestRecord
		if errUnmarshal := json.Unmarshal([]byte(line), &record); errUnmarshal != nil {
			continue
		}
		records = append(records, record)
	}

	return records
}

// RecordFilter defines the criteria for filtering detailed request records.
type RecordFilter struct {
	APIKeyHash string
	StatusCode string // e.g. "200", "4xx", "5xx"
	After      time.Time
	Before     time.Time
	Offset     int
	Limit      int
}

// applyFilters filters records based on the given criteria.
func (dl *DetailedRequestLogger) applyFilters(records []DetailedRequestRecord, filter RecordFilter) []DetailedRequestRecord {
	if filter.APIKeyHash == "" && filter.StatusCode == "" && filter.After.IsZero() && filter.Before.IsZero() {
		return records
	}

	filtered := make([]DetailedRequestRecord, 0, len(records))
	for _, r := range records {
		if filter.APIKeyHash != "" && r.APIKeyHash != filter.APIKeyHash && r.APIKey != filter.APIKeyHash {
			continue
		}
		if !matchStatusCode(r.StatusCode, filter.StatusCode) {
			continue
		}
		if !filter.After.IsZero() && r.Timestamp.Before(filter.After) {
			continue
		}
		if !filter.Before.IsZero() && r.Timestamp.After(filter.Before) {
			continue
		}
		filtered = append(filtered, r)
	}
	return filtered
}

// matchStatusCode checks if a status code matches the filter pattern.
// Supports exact match (e.g. "200") and class match (e.g. "2xx", "4xx", "5xx").
func matchStatusCode(code int, pattern string) bool {
	if pattern == "" {
		return true
	}
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	if pattern == "" {
		return true
	}

	// Class match: "2xx", "4xx", "5xx"
	if len(pattern) == 3 && pattern[1] == 'x' && pattern[2] == 'x' {
		classDigit := pattern[0]
		codeClass := byte('0' + byte(code/100))
		return classDigit == codeClass
	}

	// Exact match
	return fmt.Sprintf("%d", code) == pattern
}

// MaskAPIKey returns a masked version of the API key for display.
// Shows first 4 and last 4 characters with dots in between.
func MaskAPIKey(key string) string {
	if len(key) <= 8 {
		return strings.Repeat("*", len(key))
	}
	return key[:4] + "..." + key[len(key)-4:]
}

// HashAPIKey returns a SHA-256 hash of the API key for exact-match filtering.
func HashAPIKey(key string) string {
	h := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%x", h[:16]) // first 16 bytes (32 hex chars)
}
