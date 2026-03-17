# auto-reregister

当路由节点因凭证失效（如 401/403/429 等）触发失败时，自动执行：

1. **删除**触发失败的凭证 JSON 文件
2. **注册**一个新的 OpenAI 账号（使用 `openai_register3.py`）
3. **写入**新凭证 JSON 到 auth 目录
4. **添加**新凭证到路由的 layer 1

从而保持凭证数量不变、自动轮换失效节点。

## 参数

| 参数 | 说明 | 必填 |
|------|------|------|
| proxy | 注册用代理地址 | 否 |
| ss_dns | SS 代理 DNS | 否 |
| auth_dir | 凭证存放目录 | 是 |
| api_base | 管理 API 地址 | 是 |
| api_password | API 密码 | 否 |
| register_type | 账号类型 (codex/chatgpt) | 是 |

## 依赖

- Python 3.8+
- curl_cffi >= 0.5.0
- singbox2proxy >= 0.2.0（可选，ss:// 代理支持）
