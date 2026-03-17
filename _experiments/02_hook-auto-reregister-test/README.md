# 02_hook-auto-reregister-test

auto-reregister 钩子的本地模拟测试套件。

在不依赖真实 CLIProxyAPI 服务和真实 OpenAI 注册的情况下，验证 run.sh 的全流程：

1. **mock_api_server.py** — 一个轻量级 Flask 模拟 API 服务器，模拟：
   - `DELETE /auth-files` — 删除凭证
   - `POST /auth-files` — 上传凭证
   - `GET /unified-routing/config/routes/:id/pipeline` — 获取 pipeline
   - `PUT /unified-routing/config/routes/:id/pipeline` — 更新 pipeline

2. **mock_register.py** — 替代 `openai_register3.py` 的模拟注册脚本，直接生成 token JSON

3. **run_test.sh** — 一键运行测试：
   - 启动 mock API server
   - 准备模拟环境（auth 目录 + 旧凭证）
   - 以模拟模式执行 run.sh
   - 验证每个步骤的结果
   - 清理

## 使用

```bash
cd _experiments/02_hook-auto-reregister-test
bash run_test.sh
```
