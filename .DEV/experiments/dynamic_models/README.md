# 动态模型拉取实验

验证从各 Provider API 动态获取模型列表的方案可行性。

## 运行方式

### 模式 1：环境变量（直接 Key）

```bash
cd /root/workspace/CLIProxyAPI

export GEMINI_API_KEY="your-gemini-key"   # 可选
export CLAUDE_API_KEY="your-claude-key"   # 可选
export OPENAI_API_KEY="your-openai-key"   # 可选

go run ./.DEV/experiments/dynamic_models/
```

### 模式 2：凭证配置（config + auth-dir）

从 config.yaml 和 auth-dir 加载凭证，与主程序一致：

```bash
cd /root/workspace/CLIProxyAPI

go run ./.DEV/experiments/dynamic_models/ -mode=creds -config=config.yaml
# 可选: -auth-dir=/path/to/auths  覆盖 config 中的 auth-dir
```

## API 端点

| Provider | 端点 | 认证方式 |
|----------|------|----------|
| Gemini | `GET {base}/v1beta/models?key=API_KEY` | Query 参数 |
| Claude | `GET {base}/v1/models` | Header `x-api-key` |
| OpenAI | `GET {base}/v1/models` | Header `Authorization: Bearer TOKEN` |

## 验证结果

- **成功**：输出 "成功拉取 N 个模型" 及前 5 个模型 ID
- **失败**：输出 HTTP 状态码和错误信息
- **跳过**：未设置对应环境变量时跳过

## 后续集成

验证通过后，可将 `fetchGeminiModels`、`fetchClaudeModels`、`fetchOpenAIModels` 迁移到：

- `internal/runtime/executor/` 下各 executor
- `sdk/cliproxy/service.go` 的 `registerModelsForAuth` 中，在 config 未指定 `models` 时调用动态拉取
