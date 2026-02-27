**Cursor IDE 调用 Opus（Claude）时，返回的 SSE 是 OpenAI Chat Completions 格式。**

Cursor 无论调用什么模型（GPT、Claude Opus、Gemini 等），它走的都是 `/v1/chat/completions` 端点，使用的是 **OpenAI 格式**的请求和响应。即使模型名字是 `claude-sonnet-4-20250514`，Cursor 也期望收到 OpenAI 格式的 SSE 流。

SSE 格式如你文档中所述：

```198:207:CLIProxyAPI/.DEV/docs/备忘/各个API的格式.md
### 1.5 流式响应 (SSE)

data: {"id":"chatcmpl-abc123","object":"chat.completion.chunk","created":1709000000,"model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"id":"chatcmpl-abc123","object":"chat.completion.chunk","created":1709000000,"model":"gpt-4o","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}

data: {"id":"chatcmpl-abc123","object":"chat.completion.chunk","created":1709000000,"model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}

data: [DONE]
```

关键特征：
- 每个 chunk 以 `data: ` 前缀 + JSON 对象
- JSON 里用 `choices[0].delta` 承载增量内容
- 流结束用 `data: [DONE]` 作为终止信号

---

**这正好和你之前提的问题对上了：**

当 Cursor（OpenAI 格式）通过你的代理发请求到 Claude 端点时，你的格式检测会发现请求体是 OpenAI 格式。后端向 Anthropic API 发的是原生 Claude SSE（`event: content_block_delta`、`event: message_stop` 等），然后 translator 负责把 Claude SSE 转换回 OpenAI SSE（`data: {...choices[0].delta...}`），最后由 `executeWithUnifiedRoutingFailoverFormat` 追加 `data: [DONE]`。

对于 Cursor 这个场景，`sourceFormat = FormatOpenAI`，所以 `data: [DONE]` 是**需要的、正确的**。我上面说的 bug 是针对反过来的情况——当 `sourceFormat` 是 Claude/Gemini 时不应该追加 `[DONE]`。