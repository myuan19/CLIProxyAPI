# CLIProxyAPI 各 API 格式完整规范

本文档描述 CLIProxyAPI 支持的 7 种 API 格式的完整请求/响应结构，可用于格式校验和转换实现参考。

---

## 目录

1. [openai — OpenAI Chat Completions](#1-openai--openai-chat-completions)
2. [openai-response — OpenAI Responses API](#2-openai-response--openai-responses-api)
3. [claude — Anthropic Claude API](#3-claude--anthropic-claude-api)
4. [gemini — Google Gemini API](#4-gemini--google-gemini-api)
5. [gemini-cli — Gemini CLI 格式](#5-gemini-cli--gemini-cli-格式)
6. [codex — Codex 格式](#6-codex--codex-格式)
7. [antigravity — Antigravity 格式](#7-antigravity--antigravity-格式)
8. [格式对照总表](#8-格式对照总表)

---

## 1. openai — OpenAI Chat Completions

**端点**: `POST /v1/chat/completions`  
**内容类型**: `application/json`  
**核心特征**: 以 `messages` 数组承载对话历史，角色为 `system`/`user`/`assistant`/`tool`

### 1.1 请求结构

```json
{
  "model": "gpt-4o",
  "messages": [
    {
      "role": "system",
      "content": "You are a helpful assistant."
    },
    {
      "role": "user",
      "content": "Hello"
    },
    {
      "role": "user",
      "content": [
        {"type": "text", "text": "What's in this image?"},
        {
          "type": "image_url",
          "image_url": {
            "url": "data:image/png;base64,iVBOR...",
            "detail": "high"
          }
        }
      ]
    },
    {
      "role": "assistant",
      "content": "I'll look that up for you.",
      "tool_calls": [
        {
          "id": "call_abc123",
          "type": "function",
          "function": {
            "name": "get_weather",
            "arguments": "{\"location\":\"Tokyo\"}"
          }
        }
      ]
    },
    {
      "role": "tool",
      "tool_call_id": "call_abc123",
      "content": "{\"temperature\":\"22°C\",\"condition\":\"sunny\"}"
    }
  ],
  "tools": [
    {
      "type": "function",
      "function": {
        "name": "get_weather",
        "description": "Get current weather for a location",
        "parameters": {
          "type": "object",
          "properties": {
            "location": {"type": "string", "description": "City name"}
          },
          "required": ["location"]
        },
        "strict": false
      }
    }
  ],
  "tool_choice": "auto",
  "response_format": {"type": "json_object"},
  "stream": true,
  "stream_options": {"include_usage": true},
  "temperature": 0.7,
  "top_p": 1.0,
  "max_tokens": 4096,
  "n": 1,
  "stop": ["\n\n"],
  "presence_penalty": 0.0,
  "frequency_penalty": 0.0,
  "reasoning_effort": "medium",
  "seed": 42,
  "user": "user-123"
}
```

### 1.2 字段说明

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `model` | string | ✅ | 模型标识 |
| `messages` | array | ✅ | 消息数组 |
| `messages[].role` | string | ✅ | `system` / `user` / `assistant` / `tool` |
| `messages[].content` | string \| array | ✅ | 文本字符串或内容块数组 |
| `messages[].tool_calls` | array | — | 仅 `assistant` 角色，工具调用列表 |
| `messages[].tool_call_id` | string | — | 仅 `tool` 角色，关联的调用 ID |
| `tools` | array | — | 可用工具定义列表 |
| `tools[].type` | string | ✅ | 固定 `"function"` |
| `tools[].function.name` | string | ✅ | 函数名 |
| `tools[].function.description` | string | — | 函数描述 |
| `tools[].function.parameters` | object | — | JSON Schema 格式的参数定义 |
| `tools[].function.strict` | boolean | — | 是否严格模式 |
| `tool_choice` | string \| object | — | `"auto"` / `"none"` / `"required"` / `{"type":"function","function":{"name":"..."}}` |
| `response_format` | object | — | `{"type":"text"}` / `{"type":"json_object"}` / `{"type":"json_schema",...}` |
| `stream` | boolean | — | 是否流式响应 |
| `stream_options` | object | — | `{"include_usage": true}` 在流式最后一个 chunk 包含用量 |
| `temperature` | number | — | 采样温度 (0-2) |
| `top_p` | number | — | 核采样 (0-1) |
| `max_tokens` | integer | — | 最大生成 token 数 |
| `n` | integer | — | 生成候选数量 |
| `stop` | string \| array | — | 停止序列 |
| `presence_penalty` | number | — | 存在惩罚 (-2.0 ~ 2.0) |
| `frequency_penalty` | number | — | 频率惩罚 (-2.0 ~ 2.0) |
| `reasoning_effort` | string | — | `"low"` / `"medium"` / `"high"` |
| `seed` | integer | — | 确定性采样种子 |
| `user` | string | — | 终端用户标识 |

### 1.3 Content 块类型 (多模态)

```json
// 文本
{"type": "text", "text": "Hello"}

// 图片 (URL 或 base64)
{
  "type": "image_url",
  "image_url": {
    "url": "https://example.com/image.png",
    "detail": "auto"
  }
}

// 文件 (扩展)
{
  "type": "file",
  "file": {
    "filename": "document.pdf",
    "file_data": "<base64>"
  }
}
```

### 1.4 非流式响应

```json
{
  "id": "chatcmpl-abc123",
  "object": "chat.completion",
  "created": 1709000000,
  "model": "gpt-4o",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "Hello! How can I help?",
        "tool_calls": null
      },
      "finish_reason": "stop",
      "logprobs": null
    }
  ],
  "usage": {
    "prompt_tokens": 10,
    "completion_tokens": 20,
    "total_tokens": 30,
    "prompt_tokens_details": {
      "cached_tokens": 0
    },
    "completion_tokens_details": {
      "reasoning_tokens": 0
    }
  },
  "system_fingerprint": "fp_abc123"
}
```

### 1.5 流式响应 (SSE)

```
data: {"id":"chatcmpl-abc123","object":"chat.completion.chunk","created":1709000000,"model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"id":"chatcmpl-abc123","object":"chat.completion.chunk","created":1709000000,"model":"gpt-4o","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}

data: {"id":"chatcmpl-abc123","object":"chat.completion.chunk","created":1709000000,"model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}

data: [DONE]
```

**流式 delta 中的工具调用**:
```json
{"delta": {"tool_calls": [{"index": 0, "id": "call_abc", "type": "function", "function": {"name": "get_weather", "arguments": ""}}]}}
{"delta": {"tool_calls": [{"index": 0, "function": {"arguments": "{\"lo"}}]}}
{"delta": {"tool_calls": [{"index": 0, "function": {"arguments": "cation\":\"Tokyo\"}"}}]}}
```

**流式 delta 中的推理内容**:
```json
{"delta": {"reasoning_content": "Let me think about this..."}}
```

### 1.6 finish_reason 枚举

| 值 | 说明 |
|---|---|
| `stop` | 正常结束或遇到停止序列 |
| `length` | 达到 `max_tokens` |
| `tool_calls` | 模型发起工具调用 |
| `content_filter` | 内容被过滤 |

---

## 2. openai-response — OpenAI Responses API

**端点**: `POST /v1/responses`  
**内容类型**: `application/json`  
**核心特征**: 以 `input` 数组承载输入项，支持 `instructions` 顶层系统提示，原生支持状态管理

### 2.1 请求结构

```json
{
  "model": "gpt-4o",
  "instructions": "You are a helpful assistant.",
  "input": [
    {
      "type": "message",
      "role": "user",
      "content": [
        {"type": "input_text", "text": "Hello"},
        {
          "type": "input_image",
          "image_url": "data:image/png;base64,iVBOR...",
          "detail": "auto"
        },
        {
          "type": "input_file",
          "file_id": "file-abc123",
          "filename": "data.csv"
        }
      ]
    },
    {
      "type": "message",
      "role": "assistant",
      "content": [
        {"type": "output_text", "text": "I'll help you with that."}
      ]
    },
    {
      "type": "function_call",
      "call_id": "call_abc123",
      "name": "get_weather",
      "arguments": "{\"location\":\"Tokyo\"}"
    },
    {
      "type": "function_call_output",
      "call_id": "call_abc123",
      "output": "{\"temperature\":\"22°C\"}"
    }
  ],
  "tools": [
    {
      "type": "function",
      "name": "get_weather",
      "description": "Get current weather",
      "parameters": {
        "type": "object",
        "properties": {
          "location": {"type": "string"}
        },
        "required": ["location"]
      },
      "strict": true
    }
  ],
  "tool_choice": "auto",
  "text": {
    "format": {
      "type": "json_schema",
      "name": "response_schema",
      "strict": true,
      "schema": {}
    }
  },
  "reasoning": {
    "effort": "medium",
    "summary": "auto"
  },
  "max_output_tokens": 4096,
  "temperature": 0.7,
  "top_p": 1.0,
  "stream": true,
  "store": false,
  "parallel_tool_calls": true,
  "truncation": "auto",
  "include": ["reasoning.encrypted_content"],
  "user": "user-123"
}
```

### 2.2 字段说明

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `model` | string | ✅ | 模型标识 |
| `input` | string \| array | — | 输入文本或 InputItem 数组 |
| `instructions` | string | — | 系统提示 (等同于 system message) |
| `tools` | array | — | 工具定义列表 |
| `tool_choice` | string \| object | — | 工具选择策略 |
| `text` | object | — | 文本输出配置 |
| `text.format` | object | — | `{"type":"text"}` / `{"type":"json_schema",...}` |
| `reasoning` | object | — | 推理配置 |
| `reasoning.effort` | string | — | `"low"` / `"medium"` / `"high"` / `"auto"` |
| `reasoning.summary` | string | — | `"auto"` / `"concise"` / `"detailed"` |
| `max_output_tokens` | integer | — | 最大输出 token 数 (对应 Chat Completions 的 `max_tokens`) |
| `temperature` | number | — | 采样温度 |
| `top_p` | number | — | 核采样 |
| `stream` | boolean | — | 是否流式 |
| `store` | boolean | — | 是否存储交互 |
| `parallel_tool_calls` | boolean | — | 是否允许并行工具调用 |
| `truncation` | string | — | `"auto"` (自动截断) / `"disabled"` (超出则报错) |
| `include` | array | — | 附加输出数据，如 `"reasoning.encrypted_content"` |
| `user` | string | — | 终端用户标识 |
| `conversation` | string | — | 会话 ID，支持多轮对话 |
| `previous_response_id` | string | — | 前一个响应 ID，继续对话 |

### 2.3 InputItem 类型

```json
// 消息项
{
  "type": "message",
  "role": "user",
  "content": [
    {"type": "input_text", "text": "..."},
    {"type": "input_image", "image_url": "...", "detail": "auto"},
    {"type": "input_file", "file_id": "...", "filename": "..."}
  ]
}

// 函数调用 (模型历史)
{
  "type": "function_call",
  "call_id": "call_abc123",
  "name": "get_weather",
  "arguments": "{\"location\":\"Tokyo\"}"
}

// 函数调用结果
{
  "type": "function_call_output",
  "call_id": "call_abc123",
  "output": "{\"temperature\":\"22°C\"}"
}
```

**Message 角色**: `user` / `assistant` / `developer` / `system`

### 2.4 非流式响应

```json
{
  "id": "resp_abc123",
  "object": "response",
  "created_at": 1709000000,
  "status": "completed",
  "model": "gpt-4o",
  "output": [
    {
      "id": "msg_abc123",
      "type": "message",
      "role": "assistant",
      "status": "completed",
      "content": [
        {
          "type": "output_text",
          "text": "Hello! How can I help?"
        }
      ]
    }
  ],
  "usage": {
    "input_tokens": 10,
    "output_tokens": 20,
    "total_tokens": 30
  }
}
```

### 2.5 流式响应 (SSE)

```
data: {"type":"response.created","response":{"id":"resp_abc","status":"in_progress"}}

data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_abc","type":"message","role":"assistant","status":"in_progress"}}

data: {"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}

data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"Hello"}

data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"!"}

data: {"type":"response.output_text.done","output_index":0,"content_index":0,"text":"Hello!"}

data: {"type":"response.output_item.done","output_index":0,"item":{"id":"msg_abc","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello!"}]}}

data: {"type":"response.completed","response":{"id":"resp_abc","status":"completed","output":[...],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}
```

**工具调用流式事件**:
```
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_abc","type":"function_call","name":"get_weather","status":"in_progress"}}

data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"lo"}

data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"cation\":\"Tokyo\"}"}

data: {"type":"response.function_call_arguments.done","output_index":0,"arguments":"{\"location\":\"Tokyo\"}"}
```

**推理摘要流式事件**:
```
data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_abc","delta":"Thinking about..."}
```

### 2.6 与 Chat Completions 的关键差异

| 维度 | Chat Completions (`openai`) | Responses (`openai-response`) |
|---|---|---|
| 消息字段 | `messages: [...]` | `input: [...]` |
| 系统提示 | `{"role":"system","content":"..."}` | `instructions: "..."` |
| 用户内容类型 | `{"type":"text","text":"..."}` | `{"type":"input_text","text":"..."}` |
| 助手内容类型 | `content: "..."` | `{"type":"output_text","text":"..."}` |
| 工具调用 | `assistant.tool_calls[]` 嵌在消息中 | 顶层独立的 `function_call` 对象 |
| 工具结果 | `{"role":"tool","tool_call_id":"..."}` | `{"type":"function_call_output","call_id":"..."}` |
| 最大 token | `max_tokens` | `max_output_tokens` |
| 推理控制 | `reasoning_effort` | `reasoning.effort` + `reasoning.summary` |
| 响应格式 | `response_format` | `text.format` |

---

## 3. claude — Anthropic Claude API

**端点**: `POST /v1/messages`  
**内容类型**: `application/json`  
**核心特征**: `system` 为顶层字段而非消息角色，content 为结构化内容块数组，`max_tokens` 必填

### 3.1 请求结构

```json
{
  "model": "claude-opus-4-6",
  "max_tokens": 8192,
  "system": "You are a helpful assistant.",
  "messages": [
    {
      "role": "user",
      "content": "Hello"
    },
    {
      "role": "user",
      "content": [
        {"type": "text", "text": "What's in this image?"},
        {
          "type": "image",
          "source": {
            "type": "base64",
            "media_type": "image/png",
            "data": "iVBOR..."
          }
        }
      ]
    },
    {
      "role": "assistant",
      "content": [
        {"type": "text", "text": "I'll look that up."},
        {
          "type": "tool_use",
          "id": "toolu_abc123",
          "name": "get_weather",
          "input": {"location": "Tokyo"}
        }
      ]
    },
    {
      "role": "user",
      "content": [
        {
          "type": "tool_result",
          "tool_use_id": "toolu_abc123",
          "content": "{\"temperature\":\"22°C\"}"
        }
      ]
    }
  ],
  "tools": [
    {
      "name": "get_weather",
      "description": "Get current weather for a location",
      "input_schema": {
        "type": "object",
        "properties": {
          "location": {"type": "string", "description": "City name"}
        },
        "required": ["location"]
      }
    }
  ],
  "tool_choice": {"type": "auto"},
  "thinking": {
    "type": "enabled",
    "budget_tokens": 10000
  },
  "temperature": 0.7,
  "top_p": 1.0,
  "top_k": 40,
  "stop_sequences": ["\n\nHuman:"],
  "stream": true,
  "metadata": {
    "user_id": "user-123"
  }
}
```

### 3.2 字段说明

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `model` | string | ✅ | 模型标识 (如 `claude-opus-4-6`, `claude-sonnet-4-20250514`) |
| `max_tokens` | integer | ✅ | 最大输出 token 数 (Claude 必填) |
| `messages` | array | ✅ | 消息数组 (仅 `user`/`assistant` 角色) |
| `system` | string \| array | — | 顶层系统提示 (注意: 不是消息角色) |
| `tools` | array | — | 工具定义列表 |
| `tools[].name` | string | ✅ | 工具名 |
| `tools[].description` | string | — | 工具描述 (强烈推荐) |
| `tools[].input_schema` | object | ✅ | JSON Schema 格式参数 (注意命名: `input_schema` 非 `parameters`) |
| `tool_choice` | object | — | `{"type":"auto"}` / `{"type":"any"}` / `{"type":"tool","name":"..."}` |
| `thinking` | object | — | 扩展推理配置 |
| `thinking.type` | string | — | `"enabled"` / `"disabled"` |
| `thinking.budget_tokens` | integer | — | 推理 token 预算 |
| `temperature` | number | — | 采样温度 (0-1) |
| `top_p` | number | — | 核采样 |
| `top_k` | integer | — | Top-K 采样 |
| `stop_sequences` | array | — | 停止序列 |
| `stream` | boolean | — | 是否流式 |
| `metadata` | object | — | 元数据，如 `user_id` |

### 3.3 Content 块类型

```json
// 文本
{"type": "text", "text": "Hello", "cache_control": {"type": "ephemeral"}}

// 图片
{
  "type": "image",
  "source": {
    "type": "base64",
    "media_type": "image/png",
    "data": "iVBOR..."
  }
}

// 工具调用 (assistant 消息中)
{
  "type": "tool_use",
  "id": "toolu_abc123",
  "name": "get_weather",
  "input": {"location": "Tokyo"}
}

// 工具结果 (user 消息中)
{
  "type": "tool_result",
  "tool_use_id": "toolu_abc123",
  "content": "22°C, sunny",
  "is_error": false
}

// 推理内容
{
  "type": "thinking",
  "thinking": "Let me analyze this...",
  "signature": "..."
}
```

### 3.4 非流式响应

```json
{
  "id": "msg_abc123",
  "type": "message",
  "role": "assistant",
  "model": "claude-opus-4-6",
  "content": [
    {"type": "text", "text": "Hello! How can I help?"}
  ],
  "stop_reason": "end_turn",
  "stop_sequence": null,
  "usage": {
    "input_tokens": 10,
    "output_tokens": 20,
    "cache_creation_input_tokens": 0,
    "cache_read_input_tokens": 0
  }
}
```

### 3.5 流式响应 (SSE)

```
event: message_start
data: {"type":"message_start","message":{"id":"msg_abc123","type":"message","role":"assistant","model":"claude-opus-4-6","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"!"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}
```

**推理 (thinking) 流式事件**:
```
event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me think..."}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}
```

**工具调用流式事件**:
```
event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_abc","name":"get_weather"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"lo"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"cation\":\"Tokyo\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}
```

### 3.6 stop_reason 枚举

| 值 | 说明 | 对应 OpenAI |
|---|---|---|
| `end_turn` | 正常结束 | `stop` |
| `tool_use` | 发起工具调用 | `tool_calls` |
| `max_tokens` | 达到 token 限制 | `length` |
| `stop_sequence` | 遇到停止序列 | `stop` |

---

## 4. gemini — Google Gemini API

**端点**: `POST /v1beta/models/{model}:generateContent` (非流式) / `:streamGenerateContent?alt=sse` (流式)  
**内容类型**: `application/json`  
**核心特征**: 消息为 `contents` 数组，角色是 `user`/`model`，内容以 `parts` 承载，系统提示独立于 `systemInstruction`

### 4.1 请求结构

```json
{
  "systemInstruction": {
    "role": "user",
    "parts": [
      {"text": "You are a helpful assistant."}
    ]
  },
  "contents": [
    {
      "role": "user",
      "parts": [
        {"text": "Hello"},
        {
          "inlineData": {
            "mimeType": "image/png",
            "data": "iVBOR..."
          }
        }
      ]
    },
    {
      "role": "model",
      "parts": [
        {"text": "I'll look that up."},
        {
          "functionCall": {
            "name": "get_weather",
            "args": {"location": "Tokyo"}
          }
        }
      ]
    },
    {
      "role": "user",
      "parts": [
        {
          "functionResponse": {
            "name": "get_weather",
            "response": {
              "result": {"temperature": "22°C", "condition": "sunny"}
            }
          }
        }
      ]
    }
  ],
  "tools": [
    {
      "functionDeclarations": [
        {
          "name": "get_weather",
          "description": "Get current weather",
          "parameters": {
            "type": "object",
            "properties": {
              "location": {"type": "string"}
            },
            "required": ["location"]
          }
        }
      ]
    },
    {"googleSearch": {}},
    {"codeExecution": {}},
    {"urlContext": {}}
  ],
  "generationConfig": {
    "temperature": 0.7,
    "topP": 1.0,
    "topK": 40,
    "maxOutputTokens": 8192,
    "candidateCount": 1,
    "stopSequences": ["###"],
    "responseModalities": ["TEXT", "IMAGE"],
    "responseMimeType": "application/json",
    "responseSchema": {},
    "thinkingConfig": {
      "thinkingLevel": "medium",
      "thinkingBudget": -1,
      "includeThoughts": true
    },
    "imageConfig": {
      "aspectRatio": "16:9",
      "imageSize": "1024x1024"
    }
  },
  "safetySettings": [
    {
      "category": "HARM_CATEGORY_HARASSMENT",
      "threshold": "BLOCK_NONE"
    },
    {
      "category": "HARM_CATEGORY_HATE_SPEECH",
      "threshold": "BLOCK_NONE"
    },
    {
      "category": "HARM_CATEGORY_SEXUALLY_EXPLICIT",
      "threshold": "BLOCK_NONE"
    },
    {
      "category": "HARM_CATEGORY_DANGEROUS_CONTENT",
      "threshold": "BLOCK_NONE"
    },
    {
      "category": "HARM_CATEGORY_CIVIC_INTEGRITY",
      "threshold": "BLOCK_NONE"
    }
  ]
}
```

### 4.2 字段说明

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `contents` | array | ✅ | 对话内容数组 |
| `contents[].role` | string | ✅ | `user` / `model` (注意: 不是 `assistant`) |
| `contents[].parts` | array | ✅ | 内容部分数组 |
| `systemInstruction` | object | — | 系统指令 (独立于 contents) |
| `systemInstruction.role` | string | — | 固定 `"user"` |
| `systemInstruction.parts` | array | — | 同 contents 的 parts 结构 |
| `tools` | array | — | 工具定义列表 |
| `tools[].functionDeclarations` | array | — | 函数声明列表 (注意是嵌套数组) |
| `tools[].googleSearch` | object | — | Google 搜索工具 |
| `tools[].codeExecution` | object | — | 代码执行工具 |
| `tools[].urlContext` | object | — | URL 上下文工具 |
| `generationConfig` | object | — | 生成配置 |
| `generationConfig.temperature` | number | — | 采样温度 |
| `generationConfig.topP` | number | — | 核采样 (注意驼峰命名) |
| `generationConfig.topK` | integer | — | Top-K 采样 |
| `generationConfig.maxOutputTokens` | integer | — | 最大输出 token 数 |
| `generationConfig.candidateCount` | integer | — | 候选数量 |
| `generationConfig.stopSequences` | array | — | 停止序列 |
| `generationConfig.responseModalities` | array | — | 响应模态: `["TEXT"]`, `["IMAGE"]`, `["TEXT","IMAGE"]` |
| `generationConfig.responseMimeType` | string | — | 响应 MIME 类型 (如 `application/json`) |
| `generationConfig.responseSchema` | object | — | JSON Schema 约束输出格式 |
| `generationConfig.thinkingConfig` | object | — | 推理配置 |
| `generationConfig.thinkingConfig.thinkingLevel` | string | — | `"none"` / `"low"` / `"medium"` / `"high"` |
| `generationConfig.thinkingConfig.thinkingBudget` | integer | — | 推理 token 预算 (-1 = 自动) |
| `generationConfig.thinkingConfig.includeThoughts` | boolean | — | 是否包含推理过程 |
| `safetySettings` | array | — | 安全设置数组 |

### 4.3 Parts 类型

```json
// 文本
{"text": "Hello"}

// 推理文本
{"text": "Let me think...", "thought": true}

// 带签名的推理
{"text": "...", "thought": true, "thoughtSignature": "base64..."}

// 内联数据 (图片/文件)
{
  "inlineData": {
    "mimeType": "image/png",
    "data": "iVBOR..."
  }
}

// 函数调用
{
  "functionCall": {
    "name": "get_weather",
    "args": {"location": "Tokyo"}
  }
}

// 函数响应
{
  "functionResponse": {
    "name": "get_weather",
    "response": {
      "result": {"temperature": "22°C"}
    }
  }
}
```

### 4.4 非流式响应

```json
{
  "candidates": [
    {
      "index": 0,
      "content": {
        "role": "model",
        "parts": [
          {"text": "Hello! How can I help?"}
        ]
      },
      "finishReason": "STOP",
      "safetyRatings": [...]
    }
  ],
  "usageMetadata": {
    "promptTokenCount": 10,
    "candidatesTokenCount": 20,
    "totalTokenCount": 30,
    "cachedContentTokenCount": 0,
    "thoughtsTokenCount": 0
  },
  "modelVersion": "gemini-2.5-pro-preview-05-06",
  "responseId": "resp_abc123"
}
```

### 4.5 流式响应 (SSE)

每个 chunk 是独立的 JSON，包含增量 parts:
```
data: {"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"Hello"}]},"finishReason":null}],"modelVersion":"gemini-2.5-pro"}

data: {"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"!"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15},"modelVersion":"gemini-2.5-pro","responseId":"resp_abc"}
```

**推理 (thought) 流式**:
```
data: {"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"Let me think...","thought":true}]}}]}
```

**函数调用流式**:
```
data: {"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{"location":"Tokyo"}}}]}}]}
```

**图片输出流式**:
```
data: {"candidates":[{"index":0,"content":{"role":"model","parts":[{"inlineData":{"mimeType":"image/png","data":"iVBOR..."}}]}}]}
```

### 4.6 finishReason 枚举

| 值 | 说明 | 对应 OpenAI |
|---|---|---|
| `STOP` | 正常结束 | `stop` |
| `MAX_TOKENS` | 达到 token 限制 | `length` |
| `SAFETY` | 安全过滤 | `content_filter` |
| `RECITATION` | 引用检测 | `content_filter` |
| `OTHER` | 其他原因 | `stop` |

---

## 5. gemini-cli — Gemini CLI 格式

**端点**: `POST /v1internal:streamGenerateContent?alt=sse` (流式) / `/v1internal:generateContent` (非流式)  
**内容类型**: `application/json`  
**核心特征**: 与 Gemini 格式相同的内部结构，但外层包裹 `request` 信封和 `project` 字段

### 5.1 请求结构

```json
{
  "project": "",
  "model": "gemini-2.5-pro",
  "request": {
    "systemInstruction": {
      "role": "user",
      "parts": [
        {"text": "You are a helpful assistant."}
      ]
    },
    "contents": [
      {
        "role": "user",
        "parts": [
          {"text": "Hello"},
          {
            "inlineData": {
              "mimeType": "image/png",
              "data": "iVBOR..."
            },
            "thoughtSignature": "skip_thought_signature_validator"
          }
        ]
      },
      {
        "role": "model",
        "parts": [
          {"text": "I need to check something."},
          {
            "functionCall": {
              "name": "get_weather",
              "args": {"location": "Tokyo"}
            },
            "thoughtSignature": "skip_thought_signature_validator"
          }
        ]
      },
      {
        "role": "user",
        "parts": [
          {
            "functionResponse": {
              "name": "get_weather",
              "response": {
                "result": {"temperature": "22°C"}
              }
            }
          }
        ]
      }
    ],
    "tools": [
      {
        "functionDeclarations": [
          {
            "name": "get_weather",
            "description": "Get current weather",
            "parametersJsonSchema": {
              "type": "object",
              "properties": {
                "location": {"type": "string"}
              },
              "required": ["location"]
            }
          }
        ]
      },
      {"googleSearch": {}},
      {"codeExecution": {}},
      {"urlContext": {}}
    ],
    "generationConfig": {
      "temperature": 0.7,
      "topP": 1.0,
      "topK": 40,
      "maxOutputTokens": 8192,
      "candidateCount": 1,
      "responseModalities": ["TEXT"],
      "thinkingConfig": {
        "thinkingLevel": "medium",
        "thinkingBudget": -1,
        "includeThoughts": true
      }
    },
    "safetySettings": [
      {"category": "HARM_CATEGORY_HARASSMENT", "threshold": "BLOCK_NONE"},
      {"category": "HARM_CATEGORY_HATE_SPEECH", "threshold": "BLOCK_NONE"},
      {"category": "HARM_CATEGORY_SEXUALLY_EXPLICIT", "threshold": "BLOCK_NONE"},
      {"category": "HARM_CATEGORY_DANGEROUS_CONTENT", "threshold": "BLOCK_NONE"},
      {"category": "HARM_CATEGORY_CIVIC_INTEGRITY", "threshold": "BLOCK_NONE"}
    ]
  }
}
```

### 5.2 与 Gemini 标准格式的差异

| 维度 | Gemini | Gemini CLI |
|---|---|---|
| 外层结构 | 直接包含 `contents`/`tools`/... | 嵌套在 `request` 下 |
| 顶层字段 | 无 | `project`, `model` |
| 工具参数命名 | `parameters` | `parametersJsonSchema` |
| 函数调用 ID | 无 `id` | 带 `id` 字段 |
| 响应结构 | 直接返回 | 嵌套在 `response` 下 |
| `thoughtSignature` | 可选 | 函数调用和内联数据自动添加 `"skip_thought_signature_validator"` |

### 5.3 非流式响应

```json
{
  "response": {
    "candidates": [
      {
        "index": 0,
        "content": {
          "role": "model",
          "parts": [
            {"text": "Hello! How can I help?"}
          ]
        },
        "finishReason": "STOP"
      }
    ],
    "usageMetadata": {
      "promptTokenCount": 10,
      "candidatesTokenCount": 20,
      "totalTokenCount": 30,
      "thoughtsTokenCount": 0
    },
    "modelVersion": "gemini-2.5-pro",
    "responseId": "resp_abc123",
    "createTime": "2025-01-15T10:30:00.000Z"
  }
}
```

### 5.4 流式响应 (SSE)

```
data: {"response":{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"Hello"}]}}],"modelVersion":"gemini-2.5-pro"}}

data: {"response":{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"!"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15},"responseId":"resp_abc"}}
```

---

## 6. codex — Codex 格式

**端点**: `POST /v1/responses` (使用 ChatGPT / Codex CLI 的 Responses API 变体)  
**内容类型**: `application/json`  
**核心特征**: 基于 OpenAI Responses API，但有 Codex 专属扩展。`system` 角色映射为 `developer`，工具名可能被截短

### 6.1 请求结构

```json
{
  "model": "codex-mini",
  "instructions": "You are a coding assistant.",
  "input": [
    {
      "type": "message",
      "role": "developer",
      "content": [
        {"type": "input_text", "text": "Additional system context"}
      ]
    },
    {
      "type": "message",
      "role": "user",
      "content": [
        {"type": "input_text", "text": "Fix the bug in my code"},
        {
          "type": "input_image",
          "image_url": "data:image/png;base64,iVBOR..."
        }
      ]
    },
    {
      "type": "message",
      "role": "assistant",
      "content": [
        {"type": "output_text", "text": "Let me check the code."}
      ]
    },
    {
      "type": "function_call",
      "call_id": "call_abc123",
      "name": "mcp__editor__read_file",
      "arguments": "{\"path\":\"/src/main.py\"}"
    },
    {
      "type": "function_call_output",
      "call_id": "call_abc123",
      "output": "def main():\n    print('hello')"
    }
  ],
  "tools": [
    {
      "type": "function",
      "name": "mcp__editor__read_file",
      "description": "Read a file from disk",
      "parameters": {
        "type": "object",
        "properties": {
          "path": {"type": "string"}
        },
        "required": ["path"]
      },
      "strict": true
    }
  ],
  "tool_choice": "auto",
  "text": {
    "format": {
      "type": "text"
    }
  },
  "reasoning": {
    "effort": "medium",
    "summary": "auto"
  },
  "parallel_tool_calls": true,
  "include": ["reasoning.encrypted_content"],
  "stream": true,
  "store": false,
  "max_output_tokens": 16384,
  "temperature": 0.7,
  "top_p": 1.0
}
```

### 6.2 与 openai-response 的差异

| 维度 | openai-response | codex |
|---|---|---|
| `system` 角色 | `system` 或 `developer` | 一律 `developer` |
| `instructions` | 直接从 system 提取 | 首个 system message 提取为 `instructions` |
| 工具名长度 | 无限制 | 最长 64 字符 (保留 `mcp__` 前缀) |
| `reasoning.effort` | 可选 | 默认 `"medium"` |
| `include` | 可选 | 通常包含 `"reasoning.encrypted_content"` |
| `store` | 可选 | 通常为 `false` |

### 6.3 工具名截短规则

Codex 对工具名有 64 字符限制:
- 保留 `mcp__` 前缀
- 超长部分从末尾截断
- 例: `mcp__very_long_server_name__extremely_long_function_name_that_exceeds` → 截断到 64 字符

### 6.4 非流式响应

```json
{
  "id": "resp_abc123",
  "object": "response",
  "created_at": 1709000000,
  "status": "completed",
  "model": "codex-mini",
  "output": [
    {
      "id": "msg_abc123",
      "type": "message",
      "role": "assistant",
      "status": "completed",
      "content": [
        {"type": "output_text", "text": "Here's the fix..."}
      ]
    }
  ],
  "usage": {
    "input_tokens": 100,
    "output_tokens": 50,
    "total_tokens": 150
  }
}
```

### 6.5 流式响应 (SSE)

事件类型与 openai-response 相同:

```
data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_abc","created_at":1709000000,"status":"in_progress"}}

data: {"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"id":"msg_abc","type":"message","role":"assistant","status":"in_progress"}}

data: {"type":"response.output_text.delta","sequence_number":2,"output_index":0,"content_index":0,"delta":"Here's"}

data: {"type":"response.output_text.delta","sequence_number":3,"output_index":0,"content_index":0,"delta":" the fix"}

data: {"type":"response.output_text.done","sequence_number":4,"output_index":0,"content_index":0,"text":"Here's the fix"}

data: {"type":"response.output_item.done","sequence_number":5,"output_index":0,"item":{"id":"msg_abc","type":"message","role":"assistant","status":"completed"}}

data: {"type":"response.completed","sequence_number":6,"response":{"id":"resp_abc","status":"completed","output":[...],"usage":{...}}}
```

**推理摘要**:
```
data: {"type":"response.reasoning_summary_text.delta","sequence_number":2,"item_id":"rs_abc","delta":"Analyzing the code..."}
```

**工具调用**:
```
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_abc","type":"function_call","name":"read_file","call_id":"call_abc","status":"in_progress"}}

data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"path\":\""}

data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"/src/main.py\"}"}

data: {"type":"response.function_call_arguments.done","output_index":0,"call_id":"call_abc","arguments":"{\"path\":\"/src/main.py\"}"}
```

---

## 7. antigravity — Antigravity 格式

**端点**: `POST /v1internal:streamGenerateContent?alt=sse` (流式) / `/v1internal:generateContent` (非流式)  
**内容类型**: `application/json`  
**核心特征**: 基于 Gemini CLI 格式，额外添加 `requestId`、`userAgent`、`requestType`、`sessionId` 等 Antigravity 特有字段。使用 OAuth2 认证。

### 7.1 请求结构

```json
{
  "project": "useful-wave-a1b2c",
  "model": "gemini-2.5-pro",
  "userAgent": "antigravity",
  "requestType": "agent",
  "requestId": "agent-550e8400-e29b-41d4-a716-446655440000",
  "request": {
    "sessionId": "-1234567890123456789",
    "systemInstruction": {
      "role": "user",
      "parts": [
        {"text": "You are Antigravity, a powerful agentic AI coding assistant..."},
        {"text": "Please ignore following [ignore]...[/ignore]"},
        {"text": "User's actual system instruction here"}
      ]
    },
    "contents": [
      {
        "role": "user",
        "parts": [
          {"text": "Hello"},
          {
            "inlineData": {
              "mime_type": "image/png",
              "data": "iVBOR..."
            },
            "thoughtSignature": "skip_thought_signature_validator"
          }
        ]
      },
      {
        "role": "model",
        "parts": [
          {"text": "Let me check."},
          {
            "functionCall": {
              "id": "call_abc123",
              "name": "get_weather",
              "args": {"location": "Tokyo"}
            },
            "thoughtSignature": "skip_thought_signature_validator"
          }
        ]
      },
      {
        "role": "user",
        "parts": [
          {
            "functionResponse": {
              "id": "call_abc123",
              "name": "get_weather",
              "response": {
                "result": {"temperature": "22°C"}
              }
            }
          }
        ]
      }
    ],
    "tools": [
      {
        "functionDeclarations": [
          {
            "name": "get_weather",
            "description": "Get current weather",
            "parameters": {
              "type": "object",
              "properties": {
                "location": {"type": "string"}
              },
              "required": ["location"]
            }
          }
        ]
      },
      {"googleSearch": {}},
      {"codeExecution": {}},
      {"urlContext": {}}
    ],
    "toolConfig": {
      "functionCallingConfig": {
        "mode": "VALIDATED"
      }
    },
    "generationConfig": {
      "temperature": 0.7,
      "topP": 1.0,
      "topK": 40,
      "maxOutputTokens": 8192,
      "thinkingConfig": {
        "thinkingLevel": "medium",
        "thinkingBudget": -1,
        "includeThoughts": true
      },
      "responseModalities": ["TEXT"]
    }
  }
}
```

### 7.2 与 Gemini CLI 的差异

| 维度 | Gemini CLI | Antigravity |
|---|---|---|
| `project` | 空字符串 | 真实的 GCP 项目 ID 或随机生成 |
| `userAgent` | 无 | `"antigravity"` |
| `requestType` | 无 | `"agent"` |
| `requestId` | 无 | `"agent-<uuid>"` |
| `request.sessionId` | 无 | 基于内容哈希的稳定 ID |
| `systemInstruction` | 用户原始内容 | 注入 Antigravity 系统提示后追加用户内容 |
| `tools[].parameters` | `parametersJsonSchema` | 重命名回 `parameters` (Claude 模型用特殊 Schema 清理) |
| `toolConfig` | 无 | Claude 模型添加 `{"functionCallingConfig":{"mode":"VALIDATED"}}` |
| `safetySettings` | 包含 | 被删除 |
| `functionCall.id` | 无 | 带 `id` 字段 |
| `functionResponse.id` | 无 | 带 `id` 字段 |
| `maxOutputTokens` | 始终保留 | 非 Claude 模型时删除 |
| 认证方式 | API Key | OAuth2 Bearer Token (通过 refresh_token 刷新) |

### 7.3 认证流程

```
1. 使用 refresh_token 向 https://oauth2.googleapis.com/token 请求 access_token
2. 请求头: Authorization: Bearer <access_token>
3. 请求头: User-Agent: antigravity/1.104.0 darwin/arm64
```

### 7.4 非流式响应

```json
{
  "response": {
    "candidates": [
      {
        "index": 0,
        "content": {
          "role": "model",
          "parts": [
            {"text": "Hello! How can I help?"}
          ]
        },
        "finishReason": "STOP"
      }
    ],
    "usageMetadata": {
      "promptTokenCount": 10,
      "candidatesTokenCount": 20,
      "totalTokenCount": 30,
      "thoughtsTokenCount": 0
    },
    "modelVersion": "gemini-2.5-pro",
    "responseId": "resp_abc123",
    "createTime": "2025-01-15T10:30:00.000000000Z"
  },
  "traceId": "trace_abc123"
}
```

### 7.5 流式响应 (SSE)

与 Gemini CLI 相同，但额外包含 `traceId`:

```
data: {"response":{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"Hello","thought":true}]}}]},"traceId":"trace_abc"}

data: {"response":{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"!"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15,"thoughtsTokenCount":3},"responseId":"resp_abc"},"traceId":"trace_abc"}
```

### 7.6 特殊处理

**Claude 模型通过 Antigravity 调用时**:
- 使用流式请求 (`streamGenerateContent`)，在客户端聚合为非流式响应
- `toolConfig.functionCallingConfig.mode` 设为 `"VALIDATED"`
- 使用 Antigravity 专用的 JSON Schema 清理规则
- `systemInstruction` 首部注入 Antigravity 系统提示

**Gemini 3 Pro 模型**:
- 同样走 Claude 的非流式聚合路径
- `systemInstruction` 首部注入 Antigravity 系统提示

---

## 8. 格式对照总表

### 8.1 核心结构映射

| 维度 | openai | openai-response | claude | gemini | gemini-cli | codex | antigravity |
|---|---|---|---|---|---|---|---|
| **消息容器** | `messages` | `input` | `messages` | `contents` | `request.contents` | `input` | `request.contents` |
| **系统提示** | `role:"system"` | `instructions` | `system` (顶层) | `systemInstruction` | `request.systemInstruction` | `instructions` + `role:"developer"` | `request.systemInstruction` (注入) |
| **用户角色** | `user` | `user` | `user` | `user` | `user` | `user` | `user` |
| **助手角色** | `assistant` | `assistant` | `assistant` | `model` | `model` | `assistant` | `model` |
| **工具角色** | `tool` | (独立项) | `user` + `tool_result` | `user` + `functionResponse` | `user` + `functionResponse` | (独立项) | `user` + `functionResponse` |
| **最大 token** | `max_tokens` | `max_output_tokens` | `max_tokens` (必填) | `generationConfig.maxOutputTokens` | `request.generationConfig.maxOutputTokens` | `max_output_tokens` | `request.generationConfig.maxOutputTokens` |

### 8.2 工具调用映射

| 维度 | openai | openai-response | claude | gemini | gemini-cli | codex | antigravity |
|---|---|---|---|---|---|---|---|
| **定义位置** | `tools[].function` | `tools[]` | `tools[]` | `tools[].functionDeclarations[]` | `request.tools[].functionDeclarations[]` | `tools[]` | `request.tools[].functionDeclarations[]` |
| **参数字段** | `parameters` | `parameters` | `input_schema` | `parameters` | `parametersJsonSchema` | `parameters` | `parameters` |
| **调用位置** | `assistant.tool_calls[]` | 独立 `function_call` | `assistant.content[].tool_use` | `model.parts[].functionCall` | `model.parts[].functionCall` | 独立 `function_call` | `model.parts[].functionCall` |
| **结果位置** | `role:"tool"` msg | `function_call_output` | `user.content[].tool_result` | `user.parts[].functionResponse` | `user.parts[].functionResponse` | `function_call_output` | `user.parts[].functionResponse` |
| **调用 ID** | `tool_calls[].id` | `call_id` | `tool_use.id` | 无 | 无 | `call_id` | `functionCall.id` |

### 8.3 多模态内容映射

| 维度 | openai | openai-response | claude | gemini | gemini-cli | codex | antigravity |
|---|---|---|---|---|---|---|---|
| **图片格式** | `image_url.url` | `input_image.image_url` | `image.source.data` | `inlineData.data` | `inlineData.data` | `input_image.image_url` | `inlineData.data` |
| **MIME 类型** | 从 data URL 推断 | 从 data URL 推断 | `source.media_type` | `inlineData.mimeType` | `inlineData.mime_type` | 从 data URL 推断 | `inlineData.mime_type` |

### 8.4 推理 (Thinking) 映射

| 维度 | openai | openai-response | claude | gemini | gemini-cli | codex | antigravity |
|---|---|---|---|---|---|---|---|
| **配置方式** | `reasoning_effort` | `reasoning.effort` | `thinking.type` + `budget_tokens` | `thinkingConfig.thinkingLevel` | `thinkingConfig.thinkingLevel` | `reasoning.effort` | `thinkingConfig.thinkingLevel` |
| **响应输出** | `delta.reasoning_content` | `reasoning_summary_text.delta` | `thinking_delta` | `parts[].thought=true` | `parts[].thought=true` | `reasoning_summary_text.delta` | `parts[].thought=true` |
| **Token 统计** | `reasoning_tokens` | `reasoning_tokens` | (无独立字段) | `thoughtsTokenCount` | `thoughtsTokenCount` | `reasoning_tokens` | `thoughtsTokenCount` |

### 8.5 流式传输协议

| 格式 | 协议 | 终止标记 | 每 chunk 格式 |
|---|---|---|---|
| openai | SSE (`data: ...`) | `data: [DONE]` | `{"choices":[{"delta":{...}}]}` |
| openai-response | SSE (`data: ...`) | `response.completed` 事件 | `{"type":"response.xxx.delta","delta":"..."}` |
| claude | SSE (`event: ... data: ...`) | `message_stop` 事件 | `{"type":"content_block_delta","delta":{...}}` |
| gemini | SSE (`data: ...`) | 最后一个含 `finishReason` 的 chunk | `{"candidates":[{"content":{"parts":[...]}}]}` |
| gemini-cli | SSE (`data: ...`) | 最后一个含 `finishReason` 的 chunk | `{"response":{"candidates":[...]}}` |
| codex | SSE (`data: ...`) | `response.completed` 事件 | 同 openai-response |
| antigravity | SSE (`data: ...`) | 最后一个含 `finishReason` 的 chunk | `{"response":{"candidates":[...]}}` |
