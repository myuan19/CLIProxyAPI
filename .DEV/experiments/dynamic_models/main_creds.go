// 实验：从 config + auth-dir 凭证配置动态拉取模型
// 运行: cd CLIProxyAPI && go run ./.DEV/experiments/dynamic_models/ -mode=creds -config=config.yaml
package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/watcher/synthesizer"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func runCredsMode(ctx context.Context, configPath, authDir string) {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		fmt.Printf("加载配置失败: %v\n", err)
		os.Exit(1)
	}

	resolvedAuthDir := authDir
	if resolvedAuthDir == "" {
		resolvedAuthDir, err = util.ResolveAuthDir(cfg.AuthDir)
		if err != nil {
			resolvedAuthDir = cfg.AuthDir
		}
	}

	synthCtx := &synthesizer.SynthesisContext{
		Config:      cfg,
		AuthDir:     resolvedAuthDir,
		Now:         time.Now(),
		IDGenerator: synthesizer.NewStableIDGenerator(),
	}

	var allAuths []*coreauth.Auth
	if configAuths, _ := synthesizer.NewConfigSynthesizer().Synthesize(synthCtx); len(configAuths) > 0 {
		allAuths = append(allAuths, configAuths...)
	}
	if fileAuths, _ := synthesizer.NewFileSynthesizer().Synthesize(synthCtx); len(fileAuths) > 0 {
		allAuths = append(allAuths, fileAuths...)
	}

	fmt.Printf("=== 从凭证配置动态拉取模型 (config=%s, auth-dir=%s) ===\n\n", configPath, resolvedAuthDir)
	fmt.Printf("共加载 %d 个凭证\n\n", len(allAuths))

	// 收集所有 codex 凭证
	type codexEntry struct {
		label string
		token string
	}
	var codexCreds []codexEntry
	for _, a := range allAuths {
		if a.Disabled {
			continue
		}
		provider := strings.ToLower(strings.TrimSpace(a.Provider))
		if provider != "codex" {
			continue
		}
		token := extractCodexCreds(a)
		if token == "" {
			continue
		}
		label := a.Label
		if label == "" {
			label = a.ID
		}
		codexCreds = append(codexCreds, codexEntry{label: label, token: token})
	}

	fmt.Printf("Codex 凭证共 %d 个（含 disabled 跳过后）\n\n", len(codexCreds))

	// 并发拉取，最多 20 并发
	var (
		mu          sync.Mutex
		allModels   = map[string]int{} // model slug -> 出现在多少个凭证中
		successCnt  int64
		failCnt     int64
		cfBlockCnt  int64
		authErrCnt  int64
		timeoutCnt  int64
		otherErrCnt int64
		sem         = make(chan struct{}, 20)
		wg          sync.WaitGroup
	)

	for i, entry := range codexCreds {
		wg.Add(1)
		go func(idx int, e codexEntry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()

			models, err := fetchChatGPTModels(reqCtx, e.token)
			if err != nil {
				atomic.AddInt64(&failCnt, 1)
				errStr := err.Error()
				switch {
				case strings.Contains(errStr, "Cloudflare"):
					atomic.AddInt64(&cfBlockCnt, 1)
				case strings.Contains(errStr, "401") || strings.Contains(errStr, "403"):
					atomic.AddInt64(&authErrCnt, 1)
				case strings.Contains(errStr, "deadline") || strings.Contains(errStr, "timeout"):
					atomic.AddInt64(&timeoutCnt, 1)
				default:
					atomic.AddInt64(&otherErrCnt, 1)
				}
				if idx < 3 || idx == len(codexCreds)-1 {
					fmt.Printf("  [%d/%d] %s: 失败 - %v\n", idx+1, len(codexCreds), e.label, err)
				}
				return
			}
			atomic.AddInt64(&successCnt, 1)
			mu.Lock()
			for _, m := range models {
				allModels[m.ID]++
			}
			mu.Unlock()
			if idx < 5 || idx%200 == 0 || idx == len(codexCreds)-1 {
				fmt.Printf("  [%d/%d] %s: 成功 (%d 个模型)\n", idx+1, len(codexCreds), e.label, len(models))
			}
		}(i, entry)
	}
	wg.Wait()

	// 汇总
	fmt.Printf("\n=== Codex 模型拉取汇总 ===\n")
	fmt.Printf("总凭证: %d | 成功: %d | 失败: %d\n", len(codexCreds), successCnt, failCnt)
	if failCnt > 0 {
		fmt.Printf("  失败明细: CF拦截=%d, 认证错误=%d, 超时=%d, 其他=%d\n", cfBlockCnt, authErrCnt, timeoutCnt, otherErrCnt)
	}
	fmt.Printf("\n发现的所有模型（去重, 共 %d 种）:\n", len(allModels))
	slugs := make([]string, 0, len(allModels))
	for slug := range allModels {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	for _, slug := range slugs {
		fmt.Printf("  - %-25s  (出现在 %d 个凭证中)\n", slug, allModels[slug])
	}
	fmt.Println("\n=== 实验完成 ===")
}

// extractGeminiCreds 与 executor.geminiCreds 逻辑一致
func extractGeminiCreds(a *coreauth.Auth) (apiKey, bearer string) {
	if a == nil {
		return "", ""
	}
	if a.Attributes != nil {
		if v := a.Attributes["api_key"]; v != "" {
			apiKey = v
		}
	}
	if a.Metadata != nil {
		if v, ok := a.Metadata["access_token"].(string); ok && v != "" {
			bearer = v
		}
		if token, ok := a.Metadata["token"].(map[string]any); ok && token != nil {
			if v, ok2 := token["access_token"].(string); ok2 && v != "" {
				bearer = v
			}
		}
	}
	return
}

// extractClaudeCreds 与 executor.claudeCreds 逻辑一致
func extractClaudeCreds(a *coreauth.Auth) string {
	if a == nil {
		return ""
	}
	if a.Attributes != nil {
		if v := a.Attributes["api_key"]; v != "" {
			return v
		}
	}
	if a.Metadata != nil {
		if v, ok := a.Metadata["api_key"].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// extractCodexCreds 与 executor.codexCreds 逻辑一致 (api_key 或 access_token)
func extractCodexCreds(a *coreauth.Auth) string {
	if a == nil {
		return ""
	}
	if a.Attributes != nil {
		if v := a.Attributes["api_key"]; v != "" {
			return v
		}
	}
	if a.Metadata != nil {
		if v, ok := a.Metadata["access_token"].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func summaryStr(hasCred bool, okCount int) string {
	if !hasCred {
		return "无可用凭证"
	}
	if okCount > 0 {
		return "动态拉取成功"
	}
	return "动态拉取失败（不支持）"
}

func extractBaseURL(a *coreauth.Auth, defaultBase string) string {
	if a == nil {
		return defaultBase
	}
	if a.Attributes != nil {
		if v := strings.TrimSpace(a.Attributes["base_url"]); v != "" {
			return strings.TrimRight(v, "/")
		}
	}
	if a.Metadata != nil {
		if v, ok := a.Metadata["base_url"].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimRight(strings.TrimSpace(v), "/")
		}
		if v, ok := a.Metadata["resource_url"].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimRight(strings.TrimSpace(v), "/")
		}
	}
	return defaultBase
}
