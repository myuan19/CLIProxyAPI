// Package logging provides request logging functionality for the CLI Proxy API server.
// This file implements a structured detailed request logger that stores each request as
// an individual JSON file for easy browsing, with automatic cleanup by file count and size.
package logging

import (
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

	// detailedFileSuffix is the file extension for meta files (no bodies).
	detailedFileSuffix = ".json"

	// detailedBodiesSuffix is the suffix for body-data companion files.
	detailedBodiesSuffix = ".bodies.json"

	// legacyDetailedLogFileName is the old JSONL file name (for backward compatibility).
	legacyDetailedLogFileName = "detailed-requests.jsonl"

	// defaultDetailedMaxSizeMB is the default maximum total size of detail files in MB.
	defaultDetailedMaxSizeMB = 100

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
	IsSimulated     bool                `json:"is_simulated,omitempty"`
	Error           string              `json:"error,omitempty"`
}

// DetailedRequestSummary is a lightweight projection of DetailedRequestRecord
// returned by the list endpoint so the frontend doesn't have to download full bodies.
type DetailedRequestSummary struct {
	ID              string    `json:"id"`
	Timestamp       time.Time `json:"timestamp"`
	APIKey          string    `json:"api_key"`
	APIKeyHash      string    `json:"api_key_hash"`
	URL             string    `json:"url"`
	Method          string    `json:"method"`
	StatusCode      int       `json:"status_code"`
	Model           string    `json:"model,omitempty"`
	TotalDurationMs int64     `json:"total_duration_ms"`
	IsStreaming     bool      `json:"is_streaming"`
	IsSimulated     bool      `json:"is_simulated,omitempty"`
	Error           string    `json:"error,omitempty"`
	AttemptCount    int       `json:"attempt_count"`
}

// ToSummary converts a full record to a lightweight summary.
func (r *DetailedRequestRecord) ToSummary() DetailedRequestSummary {
	return DetailedRequestSummary{
		ID:              r.ID,
		Timestamp:       r.Timestamp,
		APIKey:          r.APIKey,
		APIKeyHash:      r.APIKeyHash,
		URL:             r.URL,
		Method:          r.Method,
		StatusCode:      r.StatusCode,
		Model:           r.Model,
		TotalDurationMs: r.TotalDurationMs,
		IsStreaming:     r.IsStreaming,
		IsSimulated:     r.IsSimulated,
		Error:           r.Error,
		AttemptCount:    len(r.Attempts),
	}
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

// DetailedAttemptBodies holds body data for a single attempt.
type DetailedAttemptBodies struct {
	Index        int    `json:"index"`
	RequestBody  string `json:"request_body,omitempty"`
	ResponseBody string `json:"response_body,omitempty"`
}

// DetailedRecordBodies holds all body data for a record, stored separately
// from the metadata to allow fast listing without parsing large bodies.
type DetailedRecordBodies struct {
	ID           string                 `json:"id"`
	RequestBody  string                 `json:"request_body,omitempty"`
	ResponseBody string                 `json:"response_body,omitempty"`
	Attempts     []DetailedAttemptBodies `json:"attempts,omitempty"`
}

// prettyFormatBody formats a body string: if it's valid JSON, pretty-prints it;
// otherwise returns as-is.
func prettyFormatBody(body string) string {
	if body == "" {
		return body
	}
	trimmed := strings.TrimSpace(body)
	if len(trimmed) == 0 {
		return body
	}
	if trimmed[0] == '{' || trimmed[0] == '[' {
		var obj interface{}
		if err := json.Unmarshal([]byte(trimmed), &obj); err == nil {
			formatted, err2 := json.MarshalIndent(obj, "", "  ")
			if err2 == nil {
				return string(formatted)
			}
		}
	}
	return body
}

// stripBodies returns a copy of the record with all body fields cleared,
// and a DetailedRecordBodies containing the extracted (pre-formatted) body data.
func stripBodies(record *DetailedRequestRecord) (*DetailedRequestRecord, *DetailedRecordBodies) {
	bodies := &DetailedRecordBodies{
		ID:           record.ID,
		RequestBody:  prettyFormatBody(record.RequestBody),
		ResponseBody: prettyFormatBody(record.ResponseBody),
	}

	meta := *record
	meta.RequestBody = ""
	meta.ResponseBody = ""

	if len(record.Attempts) > 0 {
		metaAttempts := make([]DetailedAttempt, len(record.Attempts))
		for i, a := range record.Attempts {
			bodies.Attempts = append(bodies.Attempts, DetailedAttemptBodies{
				Index:        a.Index,
				RequestBody:  prettyFormatBody(a.RequestBody),
				ResponseBody: prettyFormatBody(a.ResponseBody),
			})
			metaAttempts[i] = a
			metaAttempts[i].RequestBody = ""
			metaAttempts[i].ResponseBody = ""
		}
		meta.Attempts = metaAttempts
	}

	return &meta, bodies
}

// mergeBodies restores body content from a bodies file back into a meta record.
func mergeBodies(meta *DetailedRequestRecord, bodies *DetailedRecordBodies) {
	if bodies == nil {
		return
	}
	meta.RequestBody = bodies.RequestBody
	meta.ResponseBody = bodies.ResponseBody

	bodyMap := make(map[int]DetailedAttemptBodies, len(bodies.Attempts))
	for _, ab := range bodies.Attempts {
		bodyMap[ab.Index] = ab
	}
	for i := range meta.Attempts {
		if ab, ok := bodyMap[meta.Attempts[i].Index]; ok {
			meta.Attempts[i].RequestBody = ab.RequestBody
			meta.Attempts[i].ResponseBody = ab.ResponseBody
		}
	}
}

// IndexEntry is a lightweight record stored in the index file for fast filtering
// without reading individual meta files.
type IndexEntry struct {
	ID           string `json:"id"`
	Filename     string `json:"file"`
	APIKey       string `json:"api_key"`
	APIKeyHash   string `json:"api_key_hash"`
	StatusCode   int    `json:"status"`
	IsSimulated  bool   `json:"sim,omitempty"`
	Timestamp    int64  `json:"ts"`
	Model        string `json:"model,omitempty"`
}

const indexFileName = "index.json"

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

// writeRecordFile writes a single record as two files: meta (no bodies) and bodies.
func (dl *DetailedRequestLogger) writeRecordFile(record *DetailedRequestRecord) error {
	if err := os.MkdirAll(dl.logsDir, 0755); err != nil {
		return fmt.Errorf("failed to create logs directory: %w", err)
	}

	baseFilename := dl.generateDetailFilename(record)
	metaPath := filepath.Join(dl.logsDir, baseFilename)
	bodiesFilename := strings.TrimSuffix(baseFilename, detailedFileSuffix) + detailedBodiesSuffix
	bodiesPath := filepath.Join(dl.logsDir, bodiesFilename)

	meta, bodies := stripBodies(record)

	metaData, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal meta: %w", err)
	}
	metaData = append(metaData, '\n')
	if err := os.WriteFile(metaPath, metaData, 0644); err != nil {
		return fmt.Errorf("failed to write meta file: %w", err)
	}

	bodiesData, err := json.MarshalIndent(bodies, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal bodies: %w", err)
	}
	bodiesData = append(bodiesData, '\n')
	if err := os.WriteFile(bodiesPath, bodiesData, 0644); err != nil {
		return fmt.Errorf("failed to write bodies file: %w", err)
	}

	dl.appendToIndex(record, baseFilename)

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

// cleanupOldFiles removes the oldest detail file pairs when limits are exceeded.
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

	// Build a set of all file sizes for companion lookup
	allSizes := make(map[string]int64)
	var metaFiles []fileInfo
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
		allSizes[name] = info.Size()

		if isMetaFile(name) {
			metaFiles = append(metaFiles, fileInfo{name: name, size: info.Size(), modTime: info.ModTime()})
		}
	}

	if len(metaFiles) == 0 {
		return
	}

	sort.Slice(metaFiles, func(i, j int) bool {
		return metaFiles[i].modTime.Before(metaFiles[j].modTime)
	})

	dl.mu.Lock()
	maxFiles := dl.maxFiles
	maxBytes := int64(dl.maxSizeMB) * 1024 * 1024
	dl.mu.Unlock()

	var totalSize int64
	for _, sz := range allSizes {
		totalSize += sz
	}

	for len(metaFiles) > maxFiles || (totalSize > maxBytes && len(metaFiles) > 0) {
		oldest := metaFiles[0]
		if err := os.Remove(filepath.Join(dl.logsDir, oldest.name)); err == nil {
			totalSize -= oldest.size
		}
		// Also remove companion bodies file
		bodiesName := strings.TrimSuffix(oldest.name, detailedFileSuffix) + detailedBodiesSuffix
		if sz, ok := allSizes[bodiesName]; ok {
			if err := os.Remove(filepath.Join(dl.logsDir, bodiesName)); err == nil {
				totalSize -= sz
			}
		}
		metaFiles = metaFiles[1:]
	}

	dl.RebuildIndex()
}

// isMetaFile checks if a filename is a meta file (not a bodies companion file).
func isMetaFile(name string) bool {
	return strings.HasPrefix(name, detailedFilePrefix) &&
		strings.HasSuffix(name, detailedFileSuffix) &&
		!strings.HasSuffix(name, detailedBodiesSuffix)
}

// listDetailFiles returns all meta detail-*.json files sorted by mod time (newest first).
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
		if isMetaFile(entry.Name()) {
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

// ReadRecords reads full records (meta + bodies) from individual detail files,
// applying optional filters. Returns records in reverse chronological order.
func (dl *DetailedRequestLogger) ReadRecords(filter RecordFilter) ([]DetailedRequestRecord, int, []string, error) {
	detailFiles, err := dl.listDetailFiles()
	if err != nil {
		return nil, 0, nil, fmt.Errorf("failed to list detail files: %w", err)
	}

	var allRecords []DetailedRequestRecord
	apiKeySet := make(map[string]struct{})

	for _, entry := range detailFiles {
		record, errRead := dl.readRecordFromFile(entry.Name())
		if errRead != nil {
			continue
		}
		// Try loading companion bodies file
		bodiesName := strings.TrimSuffix(entry.Name(), detailedFileSuffix) + detailedBodiesSuffix
		if bodies, errBodies := dl.readBodiesFromFile(bodiesName); errBodies == nil {
			mergeBodies(record, bodies)
		}
		allRecords = append(allRecords, *record)
		if record.APIKey != "" {
			apiKeySet[record.APIKey] = struct{}{}
		}
	}

	apiKeys := make([]string, 0, len(apiKeySet))
	for k := range apiKeySet {
		apiKeys = append(apiKeys, k)
	}

	filtered := dl.applyFilters(allRecords, filter)
	total := len(filtered)

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

// loadIndex reads the index file and returns all entries (newest first).
func (dl *DetailedRequestLogger) loadIndex() ([]IndexEntry, error) {
	data, err := os.ReadFile(filepath.Join(dl.logsDir, indexFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	var entries []IndexEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// saveIndex writes the full index to disk.
func (dl *DetailedRequestLogger) saveIndex(entries []IndexEntry) error {
	if err := os.MkdirAll(dl.logsDir, 0755); err != nil {
		return err
	}
	data, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dl.logsDir, indexFileName), data, 0644)
}

// appendToIndex adds a new record entry to the front of the index (newest first).
func (dl *DetailedRequestLogger) appendToIndex(record *DetailedRequestRecord, filename string) {
	entries, _ := dl.loadIndex()
	entry := IndexEntry{
		ID:          record.ID,
		Filename:    filename,
		APIKey:      record.APIKey,
		APIKeyHash:  record.APIKeyHash,
		StatusCode:  record.StatusCode,
		IsSimulated: record.IsSimulated,
		Timestamp:   record.Timestamp.Unix(),
		Model:       record.Model,
	}
	entries = append([]IndexEntry{entry}, entries...)
	if err := dl.saveIndex(entries); err != nil {
		log.WithError(err).Warn("failed to update detailed request index")
	}
}

// RebuildIndex rebuilds the index from meta files on disk.
func (dl *DetailedRequestLogger) RebuildIndex() error {
	detailFiles, err := dl.listDetailFiles()
	if err != nil {
		return err
	}
	entries := make([]IndexEntry, 0, len(detailFiles))
	for _, f := range detailFiles {
		record, errRead := dl.readRecordFromFile(f.Name())
		if errRead != nil {
			continue
		}
		entries = append(entries, IndexEntry{
			ID:          record.ID,
			Filename:    f.Name(),
			APIKey:      record.APIKey,
			APIKeyHash:  record.APIKeyHash,
			StatusCode:  record.StatusCode,
			IsSimulated: record.IsSimulated,
			Timestamp:   record.Timestamp.Unix(),
			Model:       record.Model,
		})
	}
	return dl.saveIndex(entries)
}

// applyIndexFilters filters index entries based on the given criteria.
func applyIndexFilters(entries []IndexEntry, filter RecordFilter) []IndexEntry {
	filtered := make([]IndexEntry, 0, len(entries))
	for _, e := range entries {
		if e.IsSimulated && !filter.IncludeSimulated {
			continue
		}
		if filter.APIKeyHash != "" && e.APIKeyHash != filter.APIKeyHash && e.APIKey != filter.APIKeyHash {
			continue
		}
		if !matchStatusCode(e.StatusCode, filter.StatusCode) {
			continue
		}
		ts := time.Unix(e.Timestamp, 0)
		if !filter.After.IsZero() && ts.Before(filter.After) {
			continue
		}
		if !filter.Before.IsZero() && ts.After(filter.Before) {
			continue
		}
		filtered = append(filtered, e)
	}
	return filtered
}

// ReadRecordSummaries returns paginated summaries using the index file.
// Records with IDs in knownIDs are returned as cached stubs ({id, cached: true})
// instead of reading the meta file from disk.
func (dl *DetailedRequestLogger) ReadRecordSummaries(filter RecordFilter, knownIDs map[string]bool) ([]any, int, error) {
	index, err := dl.loadIndex()
	if err != nil || index == nil {
		if rebuildErr := dl.RebuildIndex(); rebuildErr != nil {
			return nil, 0, fmt.Errorf("index rebuild failed: %w", rebuildErr)
		}
		index, _ = dl.loadIndex()
		if index == nil {
			return []any{}, 0, nil
		}
	}

	filtered := applyIndexFilters(index, filter)
	total := len(filtered)

	if filter.Offset > 0 {
		if filter.Offset >= len(filtered) {
			filtered = nil
		} else {
			filtered = filtered[filter.Offset:]
		}
	}
	if filter.Limit > 0 && len(filtered) > filter.Limit {
		filtered = filtered[:filter.Limit]
	}

	results := make([]any, 0, len(filtered))
	for _, entry := range filtered {
		if len(knownIDs) > 0 && knownIDs[entry.ID] {
			results = append(results, map[string]any{"id": entry.ID, "cached": true})
		} else {
			record, errRead := dl.readRecordFromFile(entry.Filename)
			if errRead != nil {
				continue
			}
			results = append(results, record.ToSummary())
		}
	}
	return results, total, nil
}

// readBodiesFromFile reads and parses a bodies companion file.
func (dl *DetailedRequestLogger) readBodiesFromFile(filename string) (*DetailedRecordBodies, error) {
	data, err := os.ReadFile(filepath.Join(dl.logsDir, filename))
	if err != nil {
		return nil, err
	}
	var bodies DetailedRecordBodies
	if err := json.Unmarshal(data, &bodies); err != nil {
		return nil, err
	}
	return &bodies, nil
}

// ReadRecordByID reads a single full record (meta + bodies) by its ID.
func (dl *DetailedRequestLogger) ReadRecordByID(id string) (*DetailedRequestRecord, error) {
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
		if !isMetaFile(name) {
			continue
		}
		if !strings.Contains(name, id) {
			continue
		}
		record, errRead := dl.readRecordFromFile(name)
		if errRead != nil {
			continue
		}
		if record.ID == id {
			bodiesName := strings.TrimSuffix(name, detailedFileSuffix) + detailedBodiesSuffix
			if bodies, errBodies := dl.readBodiesFromFile(bodiesName); errBodies == nil {
				mergeBodies(record, bodies)
			}
			return record, nil
		}
	}

	return nil, nil
}

// DeleteAll removes all detail log files (meta + bodies) and the legacy JSONL file.
func (dl *DetailedRequestLogger) DeleteAll() error {
	os.Remove(filepath.Join(dl.logsDir, indexFileName))

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
		isDetailFile := strings.HasPrefix(name, detailedFilePrefix) && strings.HasSuffix(name, detailedFileSuffix)
		isLegacy := name == legacyDetailedLogFileName
		if isDetailFile || isLegacy {
			if errRm := os.Remove(filepath.Join(dl.logsDir, name)); errRm != nil {
				lastErr = errRm
			}
		}
	}

	return lastErr
}

// GetStats returns size information about all detail log files (meta + bodies).
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
		if !strings.HasPrefix(name, detailedFilePrefix) || !strings.HasSuffix(name, detailedFileSuffix) {
			continue
		}
		info, errInfo := entry.Info()
		if errInfo != nil {
			continue
		}
		totalSize += info.Size()
		if isMetaFile(name) {
			count++
		}
	}

	return totalSize, count, nil
}


// RecordFilter defines the criteria for filtering detailed request records.
type RecordFilter struct {
	APIKeyHash       string
	StatusCode       string // e.g. "200", "4xx", "5xx"
	After            time.Time
	Before           time.Time
	Offset           int
	Limit            int
	IncludeSimulated bool // when false (default), simulated records are excluded
}

// applyFilters filters records based on the given criteria.
func (dl *DetailedRequestLogger) applyFilters(records []DetailedRequestRecord, filter RecordFilter) []DetailedRequestRecord {
	filtered := make([]DetailedRequestRecord, 0, len(records))
	for _, r := range records {
		if r.IsSimulated && !filter.IncludeSimulated {
			continue
		}
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
