# 钩子（Hooks）功能文档

## 概述

钩子系统允许在智能路由的请求转发过程中，根据特定条件（状态码、错误内容等）自动触发外部脚本。
典型场景：凭证失效时自动删除旧凭证并注册新账号。

---

## 核心概念

### 钩子 = 文件夹

每个钩子以**文件夹**为单位，放在 `hook-scripts/` 目录下。文件夹名即为钩子标识。

```
<数据目录>/
├── hook-scripts/          ← 钩子文件夹根目录（由系统自动创建）
│   └── auto-reregister/   ← 一个钩子
│       ├── run.sh         ← 统一入口（必须）
│       ├── params.json    ← 参数声明（可选，前端会渲染为表单）
│       ├── README.md      ← 说明文档（可选，前端会展示）
│       └── ...            ← 其他脚本 / 依赖
├── hook-configs/          ← 钩子绑定配置（YAML，系统管理）
└── logs/
    └── hook-logs/         ← 执行日志（JSON）
```

### 钩子绑定（HookConfig）

一个 HookConfig 把**钩子文件夹**绑定到**某个路由**上，并定义触发条件和参数值。
一个文件夹可以被多个 HookConfig 引用（比如给不同路由配置不同的参数）。

---

## 如何添加一个新钩子

### 第 1 步：创建钩子文件夹

在 `hook-scripts/` 下新建一个文件夹，名字随意（英文、短横线）：

```bash
mkdir -p hook-scripts/my-hook
```

### 第 2 步：编写 run.sh

`run.sh` 是钩子的**统一入口**，系统用 `bash run.sh` 执行。

```bash
#!/bin/bash
set -euo pipefail

echo "Hello from my-hook!"
echo "Route: ${ROUTE_NAME} (${ROUTE_ID})"
echo "Credential: ${CREDENTIAL_ID}"
echo "Status Code: ${STATUS_CODE}"
```

#### 系统注入的环境变量

每次执行时，系统会自动注入以下环境变量：

| 环境变量 | 说明 | 示例 |
|---|---|---|
| `HOOK_ID` | 钩子配置 ID | `hook-a1b2c3` |
| `HOOK_NAME` | 钩子名称 | `自动重注册` |
| `HOOK_DIR` | 钩子文件夹名 | `auto-reregister` |
| `ROUTE_ID` | 触发路由 ID | `route-xxx` |
| `ROUTE_NAME` | 触发路由名称 | `GPT-4o 路由` |
| `TARGET_ID` | 目标节点 ID | `target-yyy` |
| `CREDENTIAL_ID` | 触发凭证文件名 | `token_abc123.json` |
| `MODEL` | 请求使用的模型 | `gpt-4o` |
| `STATUS_CODE` | HTTP 状态码 | `401` |
| `ERROR_MESSAGE` | 错误信息 | `Unauthorized` |
| `TRIGGER_REASON` | 触发原因描述 | `status_code=401 matched [401 403]` |

#### 自定义参数环境变量

如果配置了自定义参数（见下方 params.json），以 `PARAM_` 前缀 + 大写参数名注入：

```bash
# params.json 中定义了 name=proxy → 注入为 PARAM_PROXY
echo "Proxy: ${PARAM_PROXY:-无}"
```

### 第 3 步（可选）：创建 params.json

让前端面板自动渲染参数输入表单。`params.json` 是一个数组：

```json
[
  {
    "name": "proxy",
    "label": "代理地址",
    "description": "用于注册的代理地址",
    "type": "text",
    "default": "",
    "required": false
  },
  {
    "name": "register_type",
    "label": "账号类型",
    "type": "select",
    "default": "codex",
    "options": ["codex", "chatgpt"],
    "required": true
  },
  {
    "name": "api_password",
    "label": "API 密码",
    "type": "password",
    "default": "",
    "required": false
  }
]
```

支持的 `type`：

| type | 前端渲染 |
|---|---|
| `text` | 文本输入框 |
| `password` | 密码输入框（隐藏内容） |
| `number` | 数字输入框 |
| `select` | 下拉选择（需提供 `options` 数组） |

### 第 4 步（可选）：创建 README.md

前端面板会展示 README 内容，帮助使用者理解钩子的作用。

---

## 如何在面板上绑定钩子

1. 打开管理面板 → 智能路由 → **钩子管理**区域
2. 点击 **添加钩子**
3. 填写：
   - **名称**：给这个绑定起个名字（如"GPT-4o 自动重注册"）
   - **路由**：选择要绑定的路由
   - **钩子文件夹**：从下拉菜单选择（系统自动扫描 `hook-scripts/` 下含 `run.sh` 的文件夹）
   - **触发条件**：
     - `on`：`failure`（仅失败时）/ `success`（仅成功时）/ `any`（任何时候）
     - `status_codes`：状态码列表，如 `[401, 403, 429]`
     - `error_contains`：错误信息包含指定字符串时触发
   - **超时**：脚本最大执行时间（秒），默认 30
   - **参数**：如果钩子文件夹有 `params.json`，会自动渲染参数输入表单
4. 保存后即生效

---

## 手动触发

每个已绑定的钩子都可以手动触发，用于测试脚本、验证参数或在不等待真实失败的情况下主动执行。

### 面板操作

钩子列表中每个条目右侧有 **手动触发** 按钮。点击后弹出模拟输入表单：

| 字段 | 说明 | 默认值 |
|---|---|---|
| Route ID | 路由 ID | 自动填充绑定的路由 |
| Route Name | 路由名称 | 自动填充 |
| Credential ID | 模拟的凭证文件名 | `token_simulated.json` |
| Model | 模型名 | `gpt-4o` |
| Status Code | 模拟的状态码 | 触发条件中的第一个状态码 |
| Error Message | 模拟的错误信息 | `Simulated error for manual trigger` |
| Target ID | 目标节点 ID | `target-manual` |

点击**执行**后，系统同步运行 `run.sh` 并直接弹出执行结果详情（stdout/stderr/exit code）。

### API 调用

```
POST /v0/management/unified-routing/hooks/:hook_id/trigger

{
  "route_id": "route-xxx",
  "route_name": "测试路由",
  "credential_id": "token_simulated.json",
  "model": "gpt-4o",
  "status_code": 401,
  "error_message": "Simulated error"
}
```

返回完整的 `HookExecutionLog`。

手动触发时会注入 `MANUAL_TRIGGER=true` 环境变量，脚本可据此判断是否为手动触发并调整行为。

---

## 触发逻辑

路由引擎在每次请求尝试后（无论成功或失败）调用 `fireHook()`：

```
请求 → 选择节点 → 转发 → 结果
                            ├── 成功 → fireHook(success=true)
                            └── 失败 → 分类错误
                                  ├── 不可重试 → fireHook(success=false) → 标记节点
                                  └── 可重试   → fireHook(success=false) → 冷却 → 重试
```

`EvaluateAndRun()` 遍历该路由下所有已启用的 HookConfig：

1. 检查 `on` 条件（failure / success / any）
2. 检查 `status_codes`（如果配置了，必须命中其中之一）
3. 检查 `error_contains`（如果配置了，错误信息必须包含指定字符串）
4. 条件匹配 → **异步**执行 `run.sh`（不阻塞请求链路）

---

## API 端点

所有端点前缀：`/v0/management/unified-routing`

### 钩子配置 CRUD

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/hooks?route_id=xxx` | 列出钩子配置（可选按路由过滤） |
| `POST` | `/hooks` | 创建钩子绑定 |
| `GET` | `/hooks/:hook_id` | 获取单个钩子详情 |
| `PUT` | `/hooks/:hook_id` | 更新钩子配置 |
| `DELETE` | `/hooks/:hook_id` | 删除钩子配置 |
| `POST` | `/hooks/:hook_id/trigger` | 手动触发钩子（模拟输入） |

### 钩子文件夹发现

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/hooks/dirs` | 列出所有可用钩子文件夹（含 params.json、README） |

返回示例：
```json
{
  "dirs": [
    {
      "name": "auto-reregister",
      "path": "/data/hook-scripts/auto-reregister",
      "has_run": true,
      "files": ["README.md", "openai_register3.py", "params.json", "requirements.txt", "run.sh"],
      "readme": "# auto-reregister\n...",
      "params": [
        {"name": "proxy", "label": "注册代理", "type": "text", "required": false},
        ...
      ]
    }
  ],
  "total": 1,
  "scripts_dir": "/data/hook-scripts"
}
```

### 执行日志

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/hooks/logs?route_id=&hook_id=&limit=50` | 查看执行日志 |
| `DELETE` | `/hooks/logs` | 清空所有执行日志 |

### 创建钩子请求体示例

```json
{
  "name": "GPT-4o 自动重注册",
  "route_id": "route-abc123",
  "enabled": true,
  "hook_dir": "auto-reregister",
  "trigger": {
    "on": "failure",
    "status_codes": [401, 403]
  },
  "timeout_seconds": 120,
  "params": {
    "proxy": "socks5://127.0.0.1:1080",
    "auth_dir": "~/.cli-proxy-api",
    "api_base": "http://127.0.0.1:10101/v0/management",
    "register_type": "codex"
  }
}
```

---

## 执行日志格式

每条执行日志是一个 JSON 文件，保存在 `logs/hook-logs/` 下：

```json
{
  "id": "hlog-abc123",
  "hook_id": "hook-xxx",
  "hook_name": "GPT-4o 自动重注册",
  "route_id": "route-abc",
  "route_name": "GPT-4o 路由",
  "target_id": "target-yyy",
  "credential_id": "token_old.json",
  "model": "gpt-4o",
  "trigger_reason": "status_code=401 matched [401 403]",
  "status_code": 401,
  "error_message": "Unauthorized",
  "hook_dir": "auto-reregister",
  "script": "/data/hook-scripts/auto-reregister/run.sh",
  "exit_code": 0,
  "stdout": "...(脚本输出)...",
  "stderr": "",
  "success": true,
  "duration_ms": 8523,
  "timestamp": "2026-03-16T12:00:00Z"
}
```

日志上限 500 条，自动清理最旧的。stdout/stderr 超过 64KB 会截断。

---

## 内置钩子：auto-reregister

位于 `hook-scripts/auto-reregister/`。

### 功能

当凭证失效（401/403/429 等）时自动：
1. 删除旧凭证文件 + 通知 API 移除
2. 运行 `openai_register3.py --once` 注册新 OpenAI 账号
3. 将新凭证写入 auth 目录 + 通过 API 上传
4. 将新凭证添加到触发路由的 Layer 1 pipeline

### 参数

| 参数 | 环境变量 | 说明 | 默认值 |
|---|---|---|---|
| `proxy` | `PARAM_PROXY` | 注册用代理地址 | （空） |
| `ss_dns` | `PARAM_SS_DNS` | SS 代理 DNS | （空） |
| `auth_dir` | `PARAM_AUTH_DIR` | 凭证存放目录 | `~/.cli-proxy-api` |
| `api_base` | `PARAM_API_BASE` | 管理 API 地址 | `http://127.0.0.1:10101/v0/management` |
| `api_password` | `PARAM_API_PASSWORD` | 管理 API 密码 | （空） |
| `register_type` | `PARAM_REGISTER_TYPE` | 账号类型 | `codex` |

### 依赖

- Python 3.8+
- pip 包：`curl_cffi`、`singbox2proxy`（可选）
- 首次运行时自动创建 `.venv` 并安装依赖

---

## 模拟测试

无需真实 API 和真实注册即可验证钩子逻辑：

```bash
cd _experiments/02_hook-auto-reregister-test
bash run_test.sh
```

测试会启动一个 mock API 服务器，用 mock 注册脚本替代真实注册，然后执行 `run.sh` 并验证 11 个检查点（旧凭证删除、新凭证生成、API 调用、pipeline 更新等）。

---

## 编写自己的钩子

最简示例——收到 429 时发送通知：

```
hook-scripts/
└── notify-rate-limit/
    ├── run.sh
    └── params.json
```

**run.sh**:
```bash
#!/bin/bash
set -euo pipefail

WEBHOOK_URL="${PARAM_WEBHOOK_URL:-}"
if [ -z "$WEBHOOK_URL" ]; then
    echo "No webhook URL configured, skipping."
    exit 0
fi

curl -sf -X POST "$WEBHOOK_URL" \
    -H "Content-Type: application/json" \
    -d "{
        \"text\": \"Rate limited on route ${ROUTE_NAME}, credential ${CREDENTIAL_ID}, model ${MODEL}, status ${STATUS_CODE}\"
    }"

echo "Notification sent."
```

**params.json**:
```json
[
  {
    "name": "webhook_url",
    "label": "Webhook URL",
    "description": "通知地址（如 Slack/飞书 Webhook）",
    "type": "text",
    "required": true
  }
]
```

然后在面板上创建钩子绑定，选择 `notify-rate-limit` 文件夹，设置 `on=failure, status_codes=[429]`，填写 webhook URL 即可。
