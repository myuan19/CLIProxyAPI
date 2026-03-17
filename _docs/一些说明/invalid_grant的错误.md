Error: { "error": "invalid_grant", "error_description": "Bad Request" }

---

这是一个 **OAuth 2.0** 标准错误。`invalid_grant` 表示令牌授权失败，具体含义取决于上下文：

**最常见的原因**（在你这个项目中大概率是这个）：

- **Refresh Token 已过期或被撤销** — OAuth 的 refresh token 有有效期，或者用户在 Google 账户安全设置中撤销了应用授权。对于 Google OAuth（Gemini/Vertex），如果应用处于"测试"发布状态，refresh token **7 天**后自动失效。

**其他可能原因**：

- Authorization code 已被使用过（每个 code 只能换一次 token）
- Authorization code 已过期（通常几分钟内必须使用）
- Refresh token 使用次数超过 Google 的限额（每个用户每个应用最多 50 个有效 refresh token）
- 账户密码被修改，导致之前的 refresh token 全部失效

**在你的智能路由场景中**，这意味着某个凭证的 OAuth token 刷新失败了。对应到截图中的那些 `antigravity/xxx@gmail.com` 账号，很可能是某个 Google 账号的授权已经失效，需要重新授权。

---

一定是这个原因吗

---

不一定。invalid_grant 是 OAuth 2.0 规范中的一个通用错误码，它只表示"授权凭据无效"，但不告诉你具体哪里无效。
看了你的代码后，这个错误发生在 AntigravityExecutor.refreshToken 向 https://oauth2.googleapis.com/token 发送 grant_type=refresh_token 请求时。

Google OAuth 返回 invalid_grant + Bad Request 的所有可能原因：

1. Refresh token 已被撤销 — 用户在 Google 账户设置中取消了授权
2. Refresh token 已过期 — Google 应用在"测试"发布状态下，refresh token 7 天后失效
3. Refresh token 数量超限 — 同一个用户同一个 client_id 超过 50 个 refresh token，最早的会被自动失效
4. 用户修改了密码 — 所有已发放的 refresh token 全部作废
5. Google 账号被停用/删除
6. 凭证文件中的 refresh_token 字段损坏或为空 — 虽然代码检查了空值（line 1142），但如果值被截断或格式异常也会触发
7. client_id / client_secret 与发放 refresh token 时使用的不一致 — 比如代码更新后换了 client 凭证但没重新授权
8. 时钟偏差严重 — 服务器时间与真实时间差距过大

从你的截图看，出问题的是 antigravity 类型的凭证（Google OAuth），最常见的还是原因 1-3。但不能 100% 断定，需要看具体是哪个账号报的错。

---

我是请求了一个不存在的模型，报的这个错误

---

这就很有意思了。`invalid_grant` 是一个**纯 OAuth2 令牌端点的错误**，来自 `https://oauth2.googleapis.com/token`，跟模型是否存在没有直接关系。

但它确实可以在"请求不存在的模型"时出现，原因是这样的执行顺序：

1. 你发请求，指定了一个不存在的模型
2. 路由引擎选中了 antigravity target
3. **在实际调用模型 API 之前**，executor 先尝试刷新 access token（因为可能已过期）
4. 刷新 token 时 Google 返回了 `invalid_grant`
5. 这个错误直接返回给你了，**根本没走到调用模型那一步**

所以本质上是**两个问题碰到了一起**：

- **问题 A**：你请求的模型不存在（但这个错误还没来得及暴露）
- **问题 B**：被选中的那个凭证的 refresh token 已经失效（这是先暴露出来的错误）

你看到的 `invalid_grant` 实际上是**问题 B**，跟模型是否存在无关。只不过恰好是在请求那个不存在的模型时触发了 token 刷新流程，才暴露了这个凭证问题。

如果那个凭证是正常的，token 刷新成功后，你应该会收到类似 "model not found" 之类的错误，而不是 `invalid_grant`。

简单来说：**不是"请求不存在的模型"导致了 `invalid_grant`，而是凭证本身就有问题，只是在这个时机暴露出来了。**
