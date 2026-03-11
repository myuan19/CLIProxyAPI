package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	fetchCacheTTL     = 5 * time.Minute
	fetchTimeout      = 10 * time.Second
	codexFetchUA      = "codex_cli_rs/0.101.0 (Mac OS 26.0.1; arm64) Apple_Terminal/464"
	claudeAPIVersion  = "2023-06-01"
)

type cachedModels struct {
	models    []*ModelInfo
	fetchedAt time.Time
	mu        sync.Mutex
	fetching  bool
}

// DynamicModelFetcher fetches model lists from upstream provider APIs with caching.
type DynamicModelFetcher struct {
	cache sync.Map // key: "provider:base_url" -> *cachedModels
}

var (
	globalFetcher     *DynamicModelFetcher
	globalFetcherOnce sync.Once
)

// GlobalModelFetcher returns the singleton DynamicModelFetcher instance.
func GlobalModelFetcher() *DynamicModelFetcher {
	globalFetcherOnce.Do(func() {
		globalFetcher = &DynamicModelFetcher{}
	})
	return globalFetcher
}

// FetchCodexModels fetches models for Codex (ChatGPT OAuth) credentials.
// accessToken is the OAuth bearer token.
func (f *DynamicModelFetcher) FetchCodexModels(accessToken string) []*ModelInfo {
	if accessToken == "" {
		return nil
	}
	cacheKey := "codex:chatgpt"
	return f.fetchWithCache(cacheKey, func(fctx context.Context) ([]*ModelInfo, error) {
		return fetchChatGPTModels(fctx, accessToken)
	})
}

// FetchGeminiModels fetches models for Gemini API Key credentials.
func (f *DynamicModelFetcher) FetchGeminiModels(apiKey, baseURL string) []*ModelInfo {
	if apiKey == "" {
		return nil
	}
	if baseURL == "" {
		baseURL = "https://generativelanguage.googleapis.com"
	}
	baseURL = strings.TrimRight(baseURL, "/")
	cacheKey := "gemini:" + baseURL
	return f.fetchWithCache(cacheKey, func(fctx context.Context) ([]*ModelInfo, error) {
		return fetchGeminiModelList(fctx, apiKey, baseURL)
	})
}

// FetchClaudeModels fetches models for Claude API Key credentials.
func (f *DynamicModelFetcher) FetchClaudeModels(apiKey, baseURL string) []*ModelInfo {
	if apiKey == "" {
		return nil
	}
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	baseURL = strings.TrimRight(baseURL, "/")
	cacheKey := "claude:" + baseURL
	return f.fetchWithCache(cacheKey, func(fctx context.Context) ([]*ModelInfo, error) {
		return fetchClaudeModelList(fctx, apiKey, baseURL)
	})
}

// FetchOpenAICompatModels fetches models from an OpenAI-compatible endpoint.
func (f *DynamicModelFetcher) FetchOpenAICompatModels(apiKey, baseURL, providerName string) []*ModelInfo {
	if apiKey == "" || baseURL == "" {
		return nil
	}
	cleanBase := strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1")
	cacheKey := "compat:" + cleanBase
	return f.fetchWithCache(cacheKey, func(fctx context.Context) ([]*ModelInfo, error) {
		return fetchOpenAICompatModelList(fctx, apiKey, cleanBase, providerName)
	})
}

func (f *DynamicModelFetcher) fetchWithCache(cacheKey string, fetcher func(ctx context.Context) ([]*ModelInfo, error)) []*ModelInfo {
	now := time.Now()

	if val, ok := f.cache.Load(cacheKey); ok {
		cached := val.(*cachedModels)
		if now.Sub(cached.fetchedAt) < fetchCacheTTL {
			return cached.models
		}
		cached.mu.Lock()
		if !cached.fetching {
			cached.fetching = true
			cached.mu.Unlock()
			go func() {
				defer func() {
					cached.mu.Lock()
					cached.fetching = false
					cached.mu.Unlock()
				}()
				ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
				defer cancel()
				if models, err := fetcher(ctx); err == nil && len(models) > 0 {
					f.cache.Store(cacheKey, &cachedModels{
						models:    models,
						fetchedAt: time.Now(),
					})
				}
			}()
		} else {
			cached.mu.Unlock()
		}
		return cached.models
	}

	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	models, err := fetcher(ctx)
	if err != nil {
		log.Debugf("dynamic model fetch failed [%s]: %v", cacheKey, err)
		return nil
	}
	if len(models) > 0 {
		f.cache.Store(cacheKey, &cachedModels{
			models:    models,
			fetchedAt: now,
		})
	}
	return models
}

// --- Upstream API fetchers ---

func fetchChatGPTModels(ctx context.Context, accessToken string) ([]*ModelInfo, error) {
	url := "https://chatgpt.com/backend-api/models?history_and_training_disabled=false"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("User-Agent", codexFetchUA)
	req.Header.Set("Originator", "codex_cli_rs")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		snippet := string(body)
		if len(snippet) > 200 {
			snippet = snippet[:200] + "..."
		}
		if strings.Contains(snippet, "Just a moment") || strings.Contains(snippet, "cf_chl") {
			return nil, fmt.Errorf("HTTP %d: Cloudflare challenge", resp.StatusCode)
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, snippet)
	}

	var result struct {
		Models []struct {
			Slug string `json:"slug"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("JSON parse error: %w", err)
	}

	models := make([]*ModelInfo, 0, len(result.Models))
	for _, m := range result.Models {
		if m.Slug == "" {
			continue
		}
		models = append(models, &ModelInfo{
			ID:                  m.Slug,
			Object:              "model",
			OwnedBy:             "openai",
			Type:                "openai",
			DisplayName:         m.Slug,
			ContextLength:       400000,
			MaxCompletionTokens: 128000,
			SupportedParameters: []string{"tools"},
		})
	}
	return models, nil
}

func fetchGeminiModelList(ctx context.Context, apiKey, baseURL string) ([]*ModelInfo, error) {
	baseURL = strings.TrimSuffix(baseURL, "/")
	url := baseURL + "/v1beta/models?key=" + apiKey + "&pageSize=100"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Models []struct {
			Name                       string   `json:"name"`
			DisplayName                string   `json:"displayName"`
			Description                string   `json:"description"`
			InputTokenLimit            int      `json:"inputTokenLimit"`
			OutputTokenLimit           int      `json:"outputTokenLimit"`
			SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}

	models := make([]*ModelInfo, 0, len(result.Models))
	now := time.Now().Unix()
	for _, m := range result.Models {
		id := m.Name
		if idx := strings.LastIndex(m.Name, "/"); idx >= 0 {
			id = m.Name[idx+1:]
		}
		if id == "" {
			continue
		}
		displayName := m.DisplayName
		if displayName == "" {
			displayName = id
		}
		models = append(models, &ModelInfo{
			ID:                         id,
			Object:                     "model",
			Created:                    now,
			OwnedBy:                    "google",
			Type:                       "gemini",
			DisplayName:                displayName,
			Name:                       m.Name,
			Description:                m.Description,
			InputTokenLimit:            m.InputTokenLimit,
			OutputTokenLimit:           m.OutputTokenLimit,
			SupportedGenerationMethods: m.SupportedGenerationMethods,
		})
	}
	return models, nil
}

func fetchClaudeModelList(ctx context.Context, apiKey, baseURL string) ([]*ModelInfo, error) {
	baseURL = strings.TrimSuffix(baseURL, "/")
	url := baseURL + "/v1/models?limit=100"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", claudeAPIVersion)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
			CreatedAt   string `json:"created_at"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}

	models := make([]*ModelInfo, 0, len(result.Data))
	now := time.Now().Unix()
	for _, m := range result.Data {
		if m.ID == "" {
			continue
		}
		displayName := m.DisplayName
		if displayName == "" {
			displayName = m.ID
		}
		created := now
		if t, err := time.Parse(time.RFC3339, m.CreatedAt); err == nil {
			created = t.Unix()
		}
		models = append(models, &ModelInfo{
			ID:          m.ID,
			Object:      "model",
			Created:     created,
			OwnedBy:     "anthropic",
			Type:        "claude",
			DisplayName: displayName,
		})
	}
	return models, nil
}

func fetchOpenAICompatModelList(ctx context.Context, apiKey, baseURL, providerName string) ([]*ModelInfo, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("base URL is required")
	}
	baseURL = strings.TrimSuffix(baseURL, "/")
	url := baseURL + "/v1/models"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Data []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}

	ownedBy := providerName
	if ownedBy == "" {
		ownedBy = "openai-compatibility"
	}
	models := make([]*ModelInfo, 0, len(result.Data))
	for _, m := range result.Data {
		if m.ID == "" {
			continue
		}
		models = append(models, &ModelInfo{
			ID:          m.ID,
			Object:      "model",
			Created:     m.Created,
			OwnedBy:     ownedBy,
			Type:        "openai-compatibility",
			DisplayName: m.ID,
			UserDefined: true,
		})
	}
	return models, nil
}
