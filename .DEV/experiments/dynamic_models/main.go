// 实验：动态获取各 Provider 的模型列表
//
// 模式 1 - 环境变量: go run ./.DEV/experiments/dynamic_models/
//   GEMINI_API_KEY, CLAUDE_API_KEY, OPENAI_API_KEY
//
// 模式 2 - 凭证配置: go run ./.DEV/experiments/dynamic_models/ -mode=creds -config=config.yaml
//   从 config.yaml + auth-dir 加载凭证，与主程序一致
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ModelInfo 实验用简化结构，与 registry.ModelInfo 兼容
type ModelInfo struct {
	ID          string `json:"id"`
	Object      string `json:"object"`
	Created     int64  `json:"created"`
	OwnedBy     string `json:"owned_by"`
	Type        string `json:"type"`
	DisplayName string `json:"display_name,omitempty"`
	Name        string `json:"name,omitempty"`
}

func main() {
	mode := flag.String("mode", "env", "env=环境变量, creds=从 config+auth-dir 凭证")
	configPath := flag.String("config", "config.yaml", "凭证模式下的配置文件路径")
	authDir := flag.String("auth-dir", "", "凭证模式下的 auth 目录，空则用 config 中的 auth-dir")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if *mode == "creds" {
		cfgPath := *configPath
		if !filepath.IsAbs(cfgPath) {
			wd, _ := os.Getwd()
			cfgPath = filepath.Join(wd, cfgPath)
		}
		runCredsMode(ctx, cfgPath, *authDir)
		return
	}

	fmt.Println("=== 动态模型拉取实验 (环境变量模式) ===\n")

	// Gemini
	if key := os.Getenv("GEMINI_API_KEY"); key != "" {
		baseURL := os.Getenv("GEMINI_BASE_URL")
		models, err := fetchGeminiModels(ctx, key, baseURL)
		if err != nil {
			fmt.Printf("Gemini: 失败 - %v\n", err)
		} else {
			fmt.Printf("Gemini: 成功拉取 %d 个模型\n", len(models))
			for i, m := range models {
				if i >= 5 {
					fmt.Printf("  ... 还有 %d 个\n", len(models)-5)
					break
				}
				fmt.Printf("  - %s (%s)\n", m.ID, m.DisplayName)
			}
		}
		fmt.Println()
	} else {
		fmt.Println("Gemini: 跳过 (未设置 GEMINI_API_KEY)")
	}

	// Claude
	if key := os.Getenv("CLAUDE_API_KEY"); key != "" {
		baseURL := os.Getenv("CLAUDE_BASE_URL")
		models, err := fetchClaudeModels(ctx, key, baseURL)
		if err != nil {
			fmt.Printf("Claude: 失败 - %v\n", err)
		} else {
			fmt.Printf("Claude: 成功拉取 %d 个模型\n", len(models))
			for i, m := range models {
				if i >= 5 {
					fmt.Printf("  ... 还有 %d 个\n", len(models)-5)
					break
				}
				fmt.Printf("  - %s (%s)\n", m.ID, m.DisplayName)
			}
		}
		fmt.Println()
	} else {
		fmt.Println("Claude: 跳过 (未设置 CLAUDE_API_KEY)")
	}

	// OpenAI
	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		baseURL := os.Getenv("OPENAI_BASE_URL")
		models, err := fetchOpenAIModels(ctx, key, baseURL)
		if err != nil {
			fmt.Printf("OpenAI: 失败 - %v\n", err)
		} else {
			fmt.Printf("OpenAI: 成功拉取 %d 个模型\n", len(models))
			for i, m := range models {
				if i >= 5 {
					fmt.Printf("  ... 还有 %d 个\n", len(models)-5)
					break
				}
				fmt.Printf("  - %s (%s)\n", m.ID, m.DisplayName)
			}
		}
		fmt.Println()
	} else {
		fmt.Println("OpenAI: 跳过 (未设置 OPENAI_API_KEY)")
	}

	fmt.Println("=== 实验完成 ===")
}

// fetchGeminiModels 从 Gemini API 动态拉取模型列表
// GET https://generativelanguage.googleapis.com/v1beta/models?key=API_KEY
func fetchGeminiModels(ctx context.Context, apiKey, baseURL string) ([]*ModelInfo, error) {
	if baseURL == "" {
		baseURL = "https://generativelanguage.googleapis.com"
	}
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

	// Gemini 返回格式: { "models": [ { "name": "models/gemini-2.0-flash", "displayName": "...", ... } ] }
	var result struct {
		Models []struct {
			Name        string `json:"name"`
			DisplayName string `json:"displayName"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}

	models := make([]*ModelInfo, 0, len(result.Models))
	now := time.Now().Unix()
	for _, m := range result.Models {
		// name 格式为 "models/gemini-2.0-flash"，取最后一段作为 ID
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
			ID:          id,
			Object:      "model",
			Created:     now,
			OwnedBy:     "google",
			Type:        "gemini",
			DisplayName: displayName,
			Name:        id,
		})
	}
	return models, nil
}

// fetchClaudeModels 从 Claude API 动态拉取模型列表
// GET https://api.anthropic.com/v1/models
func fetchClaudeModels(ctx context.Context, apiKey, baseURL string) ([]*ModelInfo, error) {
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	baseURL = strings.TrimSuffix(baseURL, "/")
	url := baseURL + "/v1/models?limit=100"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

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

	// Claude 返回格式: { "data": [ { "id": "claude-3-5-sonnet-20241022", "display_name": "...", "created_at": "..." } ] }
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

// fetchChatGPTModels 尝试从 ChatGPT backend API 获取模型列表
// GET https://chatgpt.com/backend-api/models
func fetchChatGPTModels(ctx context.Context, accessToken string) ([]*ModelInfo, error) {
	url := "https://chatgpt.com/backend-api/models?history_and_training_disabled=false"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("User-Agent", "codex_cli_rs/0.101.0 (Mac OS 26.0.1; arm64) Apple_Terminal/464")
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
			return nil, fmt.Errorf("HTTP %d: Cloudflare 拦截 (需要浏览器 JS challenge)", resp.StatusCode)
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, snippet)
	}

	var result struct {
		Models []struct {
			Slug string `json:"slug"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("JSON 解析失败: %w", err)
	}

	models := make([]*ModelInfo, 0, len(result.Models))
	for _, m := range result.Models {
		if m.Slug == "" {
			continue
		}
		models = append(models, &ModelInfo{
			ID:          m.Slug,
			Object:      "model",
			OwnedBy:     "openai",
			Type:        "codex",
			DisplayName: m.Slug,
		})
	}
	return models, nil
}

// fetchCodexModels 从 Cursor API 动态拉取模型列表
// GET https://api.cursor.com/v0/models (Cursor Background Agents API)
func fetchCodexModels(ctx context.Context, bearerToken string) ([]*ModelInfo, error) {
	url := "https://api.cursor.com/v0/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearerToken)

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
		Models []string `json:"models"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}

	models := make([]*ModelInfo, 0, len(result.Models))
	for _, id := range result.Models {
		if id == "" {
			continue
		}
		models = append(models, &ModelInfo{
			ID:          id,
			Object:      "model",
			Created:     0,
			OwnedBy:     "cursor",
			Type:        "codex",
			DisplayName: id,
		})
	}
	return models, nil
}

// fetchOpenAIModels 从 OpenAI API 动态拉取模型列表
// GET https://api.openai.com/v1/models
func fetchOpenAIModels(ctx context.Context, apiKey, baseURL string) ([]*ModelInfo, error) {
	if baseURL == "" {
		baseURL = "https://api.openai.com"
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

	// OpenAI 返回格式: { "data": [ { "id": "gpt-4", "object": "model", "created": 123, "owned_by": "openai" } ] }
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

	models := make([]*ModelInfo, 0, len(result.Data))
	for _, m := range result.Data {
		if m.ID == "" {
			continue
		}
		ownedBy := m.OwnedBy
		if ownedBy == "" {
			ownedBy = "openai"
		}
		models = append(models, &ModelInfo{
			ID:          m.ID,
			Object:      "model",
			Created:     m.Created,
			OwnedBy:     ownedBy,
			Type:        "openai",
			DisplayName: m.ID,
		})
	}
	return models, nil
}
