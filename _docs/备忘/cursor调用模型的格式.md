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

---

cursor限制模型的小技巧：

Cursor 无论调用什么模型（GPT、Claude Opus、Gemini 等），它走的都是 `/v1/chat/completions` 端点，使用的是 **OpenAI 格式**的请求和响应。即使模型名字是 `claude-sonnet-4-20250514`，Cursor 也期望收到 OpenAI 格式的 SSE 流。

但是，请求体却没有转换！openai的用的 openai-response 的请求体，claude 模型的请求格式则会用Anthropic Claude Messages API 格式。我们的项目按照 openai格式转换了其他格式的请求体！要不然就是直接发给了提供商以不匹配的请求体。最终造成了失败。

下面是cursor对请求体的分析（发的是openai的端点）：

这个请求体的格式是 **Anthropic Claude Messages API 格式**（不是 OpenAI 格式）。让我逐项分析：

---

## 符合 Anthropic 格式的部分

| 字段 | 说明 |
|---|---|
| `system` (顶层数组) | Anthropic 格式。OpenAI 用 `messages` 里 `role: "system"` |
| `messages[].content` 是对象数组 | Anthropic 的 content block 格式，包含 `type: "text"` |
| `cache_control: {type: "ephemeral"}` | Anthropic 的 prompt caching 特性 |
| `tools[].input_schema` | Anthropic 格式。OpenAI 用 `tools[].function.parameters` |
| `tool_choice: {type: "auto"}` | Anthropic 格式 |
| `metadata: {user_id: "..."}` | Anthropic 格式 |
| `max_tokens: 4096` | 两者通用 |

## 非标准 / 混合的部分

| 字段 | 问题 |
|---|---|
| `model: "anthropic/claude-opus-4.6"` | 带了 `anthropic/` 前缀，原生 Anthropic API 期望的是纯模型名如 `claude-opus-4-20250514` |
| `stream_options: {include_usage: true}` | 这是 **OpenAI 独有** 的字段，Anthropic API 不认识这个字段 |

---

## 结论

这是 **Cursor 平台发给自己后端** 的请求格式，本质上是 Anthropic Messages API 格式，带了两个 Cursor 自己的扩展（`anthropic/` 前缀和 `stream_options`）。Cursor 的后端网关会在转发给 Anthropic 之前剥掉这些非标准字段。

如果这个请求是通过你的 CLIProxyAPI 走 `/v1/messages` 端点的话，它会被识别为 `FormatClaude`，是正确的路径。但你的代理需要注意：

1. **模型名前缀** — `anthropic/claude-opus-4.6` 需要剥掉 `anthropic/` 前缀再发给 Anthropic
2. **`stream_options`** — Anthropic 不认这个字段，转发前应该过滤掉（不过 Anthropic 通常会忽略未知字段，不一定报错）

这个请求体本身和之前那个 `"Tool '' not found in provided tools"` 错误没有直接关系——这个请求里所有 tools 都有正确的 `name` 字段。那个错误更可能发生在**后续的多轮对话**中，当 assistant 返回了 `tool_use` 并且 Cursor 把它拼回 messages 时，某个 tool_use 的 name 被丢失了。