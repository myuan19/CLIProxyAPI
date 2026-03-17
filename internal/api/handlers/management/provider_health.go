package management

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/healthcheck"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

// ProviderInfo represents information about a configured provider
type ProviderInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Label    string `json:"label,omitempty"`
	Prefix   string `json:"prefix,omitempty"`
	BaseURL  string `json:"base_url,omitempty"`
	ProxyURL string `json:"proxy_url,omitempty"`
	APIKey   string `json:"api_key,omitempty"` // masked
	Status   string `json:"status"`
	Disabled bool   `json:"disabled"`
}

// ProviderHealth represents the health status of a provider
type ProviderHealth struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	Label       string `json:"label,omitempty"`
	Prefix      string `json:"prefix,omitempty"`
	BaseURL     string `json:"base_url,omitempty"`
	Status      string `json:"status"` // "healthy", "unhealthy", "timeout"
	Message     string `json:"message,omitempty"`
	Latency     int64  `json:"latency_ms,omitempty"`
	ModelTested string `json:"model_tested,omitempty"`
}

const streamHealthCheckDeadline = 30 * time.Second

// ListProviders returns all configured API key providers from configuration
func (h *Handler) ListProviders(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	// Filter by type if specified
	typeFilter := strings.ToLower(strings.TrimSpace(c.Query("type")))

	auths := h.authManager.List()
	providers := make([]ProviderInfo, 0)

	for _, auth := range auths {
		// Only include API key providers (those with api_key attribute)
		if !isAPIKeyProvider(auth) {
			continue
		}

		providerType := getProviderType(auth)
		if typeFilter != "" && !strings.EqualFold(providerType, typeFilter) {
			continue
		}

		info := ProviderInfo{
			ID:       auth.ID,
			Name:     auth.Provider,
			Type:     providerType,
			Label:    auth.Label,
			Prefix:   auth.Prefix,
			BaseURL:  authAttribute(auth, "base_url"),
			ProxyURL: auth.ProxyURL,
			APIKey:   util.HideAPIKey(authAttribute(auth, "api_key")),
			Status:   string(auth.Status),
			Disabled: auth.Disabled,
		}
		providers = append(providers, info)
	}

	c.JSON(http.StatusOK, gin.H{
		"total":     len(providers),
		"providers": providers,
	})
}

// CheckProvidersHealth performs health checks on configured API key providers
func (h *Handler) CheckProvidersHealth(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	// Parse query parameters
	nameFilter := strings.TrimSpace(c.Query("name"))
	typeFilter := strings.ToLower(strings.TrimSpace(c.Query("type")))
	modelFilter := strings.TrimSpace(c.Query("model"))
	modelsFilter := strings.TrimSpace(c.Query("models"))
	isConcurrent := c.DefaultQuery("concurrent", "false") == "true"
	useStream := c.DefaultQuery("stream", "false") == "true"
	timeoutSeconds := 15
	if ts := c.Query("timeout"); ts != "" {
		if parsed, err := strconv.Atoi(ts); err == nil && parsed >= 5 && parsed <= 60 {
			timeoutSeconds = parsed
		}
	}

	// Build model filter set
	var modelFilterSet map[string]struct{}
	if modelFilter != "" || modelsFilter != "" {
		modelFilterSet = make(map[string]struct{})
		if modelFilter != "" {
			modelFilterSet[strings.ToLower(modelFilter)] = struct{}{}
		}
		if modelsFilter != "" {
			for _, m := range strings.Split(modelsFilter, ",") {
				trimmed := strings.TrimSpace(m)
				if trimmed != "" {
					modelFilterSet[strings.ToLower(trimmed)] = struct{}{}
				}
			}
		}
	}

	// Get all API key providers
	auths := h.authManager.List()
	targetAuths := make([]*coreauth.Auth, 0)

	for _, auth := range auths {
		if !isAPIKeyProvider(auth) {
			continue
		}
		if auth.Disabled {
			continue
		}

		// Apply name filter
		if nameFilter != "" {
			if !strings.EqualFold(auth.ID, nameFilter) &&
				!strings.EqualFold(auth.Provider, nameFilter) &&
				!strings.EqualFold(auth.Label, nameFilter) {
				continue
			}
		}

		// Apply type filter
		providerType := getProviderType(auth)
		if typeFilter != "" && !strings.EqualFold(providerType, typeFilter) {
			continue
		}

		targetAuths = append(targetAuths, auth)
	}

	if len(targetAuths) == 0 {
		c.JSON(http.StatusOK, gin.H{
			"status":          "healthy",
			"healthy_count":   0,
			"unhealthy_count": 0,
			"total_count":     0,
			"providers":       []ProviderHealth{},
		})
		return
	}

	if useStream {
		h.checkProvidersHealthStream(c, targetAuths, modelFilterSet, timeoutSeconds)
		return
	}

	// Prepare health check results
	results := make([]ProviderHealth, 0, len(targetAuths))
	var wg sync.WaitGroup
	var mu sync.Mutex

	checkOneModel := func(auth *coreauth.Auth, testModel *registry.ModelInfo) {
		defer wg.Done()

		providerType := getProviderType(auth)
		baseURL := authAttribute(auth, "base_url")

		if testModel == nil {
			mu.Lock()
			results = append(results, ProviderHealth{
				ID:      auth.ID,
				Name:    auth.Provider,
				Type:    providerType,
				Label:   auth.Label,
				Prefix:  auth.Prefix,
				BaseURL: baseURL,
				Status:  "unhealthy",
				Message: "no models available for this provider",
			})
			mu.Unlock()
			return
		}

		startTime := time.Now()
		checkCtx, cancel := context.WithTimeout(usage.WithSkipUsage(context.Background()), time.Duration(timeoutSeconds)*time.Second)
		defer cancel()

		req, opts, err := healthcheck.BuildProbeRequest(auth, testModel.ID)
		if err != nil {
			mu.Lock()
			results = append(results, ProviderHealth{
				ID:          auth.ID,
				Name:        auth.Provider,
				Type:        providerType,
				Label:       auth.Label,
				Prefix:      auth.Prefix,
				BaseURL:     baseURL,
				Status:      "unhealthy",
				Message:     fmt.Sprintf("failed to build request: %v", err),
				ModelTested: testModel.ID,
			})
			mu.Unlock()
			return
		}

		stream, err := h.authManager.ExecuteStreamWithAuth(checkCtx, auth, req, opts)
		if err != nil {
			mu.Lock()
			results = append(results, ProviderHealth{
				ID:          auth.ID,
				Name:        auth.Provider,
				Type:        providerType,
				Label:       auth.Label,
				Prefix:      auth.Prefix,
				BaseURL:     baseURL,
				Status:      "unhealthy",
				Message:     err.Error(),
				ModelTested: testModel.ID,
			})
			mu.Unlock()
			return
		}

		select {
		case chunk, ok := <-stream:
			if ok {
				if chunk.Err != nil {
					mu.Lock()
					results = append(results, ProviderHealth{
						ID:          auth.ID,
						Name:        auth.Provider,
						Type:        providerType,
						Label:       auth.Label,
						Prefix:      auth.Prefix,
						BaseURL:     baseURL,
						Status:      "unhealthy",
						Message:     chunk.Err.Error(),
						ModelTested: testModel.ID,
					})
					mu.Unlock()
					cancel()
					go func() {
						for range stream {
						}
					}()
					return
				}

				latency := time.Since(startTime).Milliseconds()
				cancel()
				go func() {
					for range stream {
					}
				}()

				mu.Lock()
				results = append(results, ProviderHealth{
					ID:          auth.ID,
					Name:        auth.Provider,
					Type:        providerType,
					Label:       auth.Label,
					Prefix:      auth.Prefix,
					BaseURL:     baseURL,
					Status:      "healthy",
					Latency:     latency,
					ModelTested: testModel.ID,
				})
				mu.Unlock()
			} else {
				mu.Lock()
				results = append(results, ProviderHealth{
					ID:          auth.ID,
					Name:        auth.Provider,
					Type:        providerType,
					Label:       auth.Label,
					Prefix:      auth.Prefix,
					BaseURL:     baseURL,
					Status:      "unhealthy",
					Message:     "stream closed without data",
					ModelTested: testModel.ID,
				})
				mu.Unlock()
			}
		case <-checkCtx.Done():
			mu.Lock()
			results = append(results, ProviderHealth{
				ID:          auth.ID,
				Name:        auth.Provider,
				Type:        providerType,
				Label:       auth.Label,
				Prefix:      auth.Prefix,
				BaseURL:     baseURL,
				Status:      "unhealthy",
				Message:     "health check timeout",
				ModelTested: testModel.ID,
			})
			mu.Unlock()
		}
	}

	// Build per-model tasks
	reg := registry.GetGlobalRegistry()
	type modelTask struct {
		auth  *coreauth.Auth
		model *registry.ModelInfo
	}
	var tasks []modelTask
	for _, auth := range targetAuths {
		models := reg.GetModelsForClient(auth.ID)
		if len(modelFilterSet) > 0 {
			filtered := make([]*registry.ModelInfo, 0)
			for _, model := range models {
				if _, ok := modelFilterSet[strings.ToLower(model.ID)]; ok {
					filtered = append(filtered, model)
				}
			}
			models = filtered
		}
		if len(models) == 0 {
			tasks = append(tasks, modelTask{auth: auth, model: nil})
		} else {
			for _, m := range models {
				tasks = append(tasks, modelTask{auth: auth, model: m})
			}
		}
	}

	// Execute health checks (one goroutine per model)
	if isConcurrent {
		for _, t := range tasks {
			wg.Add(1)
			go checkOneModel(t.auth, t.model)
		}
	} else {
		for _, t := range tasks {
			wg.Add(1)
			checkOneModel(t.auth, t.model)
		}
	}

	wg.Wait()

	// Count results
	healthyCount := 0
	unhealthyCount := 0
	for _, result := range results {
		if result.Status == "healthy" {
			healthyCount++
		} else {
			unhealthyCount++
		}
	}

	// Determine overall status
	overallStatus := "healthy"
	if unhealthyCount > 0 && healthyCount == 0 {
		overallStatus = "unhealthy"
	} else if unhealthyCount > 0 {
		overallStatus = "partial"
	}

	c.JSON(http.StatusOK, gin.H{
		"status":          overallStatus,
		"healthy_count":   healthyCount,
		"unhealthy_count": unhealthyCount,
		"total_count":     len(results),
		"providers":       results,
	})
}

// checkProvidersHealthStream runs health checks and streams each result via SSE.
// It tests ALL models for each provider (not just the first one).
// Overall deadline is streamHealthCheckDeadline (30s); any model not done by then is reported as "timeout".
func (h *Handler) checkProvidersHealthStream(c *gin.Context, targetAuths []*coreauth.Auth, modelFilterSet map[string]struct{}, timeoutSeconds int) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "streaming not supported"})
		return
	}

	streamCtx, streamCancel := context.WithTimeout(usage.WithSkipUsage(c.Request.Context()), streamHealthCheckDeadline)
	defer streamCancel()

	type modelTask struct {
		auth  *coreauth.Auth
		model *registry.ModelInfo
	}
	reg := registry.GetGlobalRegistry()
	var tasks []modelTask
	for _, auth := range targetAuths {
		models := reg.GetModelsForClient(auth.ID)
		if len(modelFilterSet) > 0 {
			filtered := make([]*registry.ModelInfo, 0)
			for _, model := range models {
				if _, ok := modelFilterSet[strings.ToLower(model.ID)]; ok {
					filtered = append(filtered, model)
				}
			}
			models = filtered
		}
		if len(models) == 0 {
			tasks = append(tasks, modelTask{auth: auth, model: nil})
		} else {
			for _, m := range models {
				tasks = append(tasks, modelTask{auth: auth, model: m})
			}
		}
	}

	resultCh := make(chan ProviderHealth, len(tasks))
	completed := make(map[string]bool)
	var completedMu sync.Mutex
	var writeMu sync.Mutex
	sendEvent := func(event string, data interface{}) {
		writeMu.Lock()
		defer writeMu.Unlock()
		c.SSEvent(event, data)
		flusher.Flush()
	}

	runOneModel := func(auth *coreauth.Auth, testModel *registry.ModelInfo) {
		providerType := getProviderType(auth)
		baseURL := authAttribute(auth, "base_url")

		if testModel == nil {
			resultCh <- ProviderHealth{
				ID: auth.ID, Name: auth.Provider, Type: providerType,
				Label: auth.Label, Prefix: auth.Prefix, BaseURL: baseURL,
				Status: "unhealthy", Message: "no models available for this provider",
			}
			return
		}

		startTime := time.Now()
		checkCtx, cancel := context.WithTimeout(streamCtx, time.Duration(timeoutSeconds)*time.Second)
		defer cancel()
		req, opts, err := healthcheck.BuildProbeRequest(auth, testModel.ID)
		if err != nil {
			resultCh <- ProviderHealth{
				ID: auth.ID, Name: auth.Provider, Type: providerType,
				Label: auth.Label, Prefix: auth.Prefix, BaseURL: baseURL,
				Status: "unhealthy", Message: fmt.Sprintf("failed to build request: %v", err),
				ModelTested: testModel.ID,
			}
			return
		}
		stream, err := h.authManager.ExecuteStreamWithAuth(checkCtx, auth, req, opts)
		if err != nil {
			resultCh <- ProviderHealth{
				ID: auth.ID, Name: auth.Provider, Type: providerType,
				Label: auth.Label, Prefix: auth.Prefix, BaseURL: baseURL,
				Status: "unhealthy", Message: err.Error(), ModelTested: testModel.ID,
			}
			return
		}
		select {
		case chunk, ok := <-stream:
			if ok {
				if chunk.Err != nil {
					resultCh <- ProviderHealth{
						ID: auth.ID, Name: auth.Provider, Type: providerType,
						Label: auth.Label, Prefix: auth.Prefix, BaseURL: baseURL,
						Status: "unhealthy", Message: chunk.Err.Error(), ModelTested: testModel.ID,
					}
					cancel()
					go func() {
						for range stream {
						}
					}()
					return
				}
				latency := time.Since(startTime).Milliseconds()
				cancel()
				go func() {
					for range stream {
					}
				}()
				resultCh <- ProviderHealth{
					ID: auth.ID, Name: auth.Provider, Type: providerType,
					Label: auth.Label, Prefix: auth.Prefix, BaseURL: baseURL,
					Status: "healthy", Latency: latency, ModelTested: testModel.ID,
				}
			} else {
				resultCh <- ProviderHealth{
					ID: auth.ID, Name: auth.Provider, Type: providerType,
					Label: auth.Label, Prefix: auth.Prefix, BaseURL: baseURL,
					Status: "unhealthy", Message: "stream closed without data", ModelTested: testModel.ID,
				}
			}
		case <-checkCtx.Done():
			resultCh <- ProviderHealth{
				ID: auth.ID, Name: auth.Provider, Type: providerType,
				Label: auth.Label, Prefix: auth.Prefix, BaseURL: baseURL,
				Status: "unhealthy", Message: "health check timeout", ModelTested: testModel.ID,
			}
		}
	}

	for _, t := range tasks {
		go runOneModel(t.auth, t.model)
	}

	received := 0
	for received < len(tasks) {
		select {
		case r := <-resultCh:
			taskKey := r.ID + "::" + r.ModelTested
			completedMu.Lock()
			completed[taskKey] = true
			completedMu.Unlock()
			sendEvent("result", r)
			received++
		case <-streamCtx.Done():
			goto done
		}
	}
done:
	for _, t := range tasks {
		modelID := ""
		if t.model != nil {
			modelID = t.model.ID
		}
		taskKey := t.auth.ID + "::" + modelID
		completedMu.Lock()
		isDone := completed[taskKey]
		completedMu.Unlock()
		if !isDone {
			providerType := getProviderType(t.auth)
			sendEvent("result", ProviderHealth{
				ID: t.auth.ID, Name: t.auth.Provider, Type: providerType,
				Label: t.auth.Label, Prefix: t.auth.Prefix,
				BaseURL: authAttribute(t.auth, "base_url"),
				Status:  "timeout", Message: "超过30s未完成",
				ModelTested: modelID,
			})
		}
	}
	sendEvent("done", gin.H{"event": "done"})
}

// isAPIKeyProvider checks if an auth entry is an API key provider (from config)
func isAPIKeyProvider(auth *coreauth.Auth) bool {
	if auth == nil || auth.Attributes == nil {
		return false
	}
	// Check for api_key attribute or label containing "apikey"
	if _, ok := auth.Attributes["api_key"]; ok {
		return true
	}
	if strings.Contains(strings.ToLower(auth.Label), "apikey") {
		return true
	}
	// Check source attribute for config-based providers
	source := strings.ToLower(auth.Attributes["source"])
	return strings.HasPrefix(source, "config:")
}

// getProviderType returns the type of provider (gemini, claude, codex, openai-compatibility, vertex)
func getProviderType(auth *coreauth.Auth) string {
	if auth == nil {
		return "unknown"
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	switch provider {
	case "gemini":
		return "gemini-api-key"
	case "claude":
		return "claude-api-key"
	case "codex":
		return "codex-api-key"
	case "vertex":
		return "vertex-api-key"
	default:
		// Check if it's openai-compatibility
		if auth.Attributes != nil {
			if _, ok := auth.Attributes["compat_name"]; ok {
				return "openai-compatibility"
			}
		}
		return provider
	}
}
