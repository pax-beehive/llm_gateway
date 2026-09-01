# WorkOS 前端集成交接

更新时间：2026-08-31（America/Los_Angeles）

## 目标

这份文档供负责 `web/` 的前端 agent 使用。目标是在现有 Ops Console 上接入 WorkOS AuthKit 的登录、组织和 RBAC 体验，同时保持浏览器、BFF、Gateway API Key 和 Human IAM 的边界清晰。

本交接只描述 WorkOS 相关的前端工作和前后端契约。现有页面/API/SSE 约束继续参考：

- [`doc/handsoff/2026-08-30-frontend-integration-handoff.md`](../../doc/handsoff/2026-08-30-frontend-integration-handoff.md)
- [`doc/handsoff/2026-08-31-ops-console-backend-changes.md`](../../doc/handsoff/2026-08-31-ops-console-backend-changes.md)
- [`web/CONVENTIONS.md`](../../web/CONVENTIONS.md)

## 当前事实

### 已完成

- 已发布 Git SHA：`a041d526351e83348a4f21d37fac2b43e8f65b2f`。
- CI 已通过：[run 33458458146](https://github.com/pax-beehive/llm_gateway/actions/runs/33458458146)。其中包括 Go、PostgreSQL integration、Terraform 和 `web` production build。
- Production deployment 已通过：[run 33458627036](https://github.com/pax-beehive/llm_gateway/actions/runs/33458627036)。
- Gateway、Control Plane、Metering 的新 Cloud Run revision 均 Ready 并承接 100% 流量。
- 数据库 migration 和 runtime role configuration 均已完成。
- `web/` 已有八个 Ops Console 板块；同源 BFF 已在本地全栈验证现有 Gateway、Control Plane 和 Metering 接口。

### 尚未完成

- 已发布的生产版本尚未包含 WorkOS BFF/前端改动，也尚未启用浏览器 session 验证。
- BFF/SPA 尚未作为生产 Cloud Run 服务部署。
- `llm-api.paxtech.net` 和 `llm-console.paxtech.net` 当前没有 DNS 记录。
- Control Plane 生产环境仍为 `CONTROL_IAM_DENY_ALL=true`。
- 当前通用 Human IAM verifier 不能直接消费标准 WorkOS session JWT；后端仍需增加 WorkOS claim adapter。

### 本地实现状态（尚未部署）

当前工作树已经补齐 React session/permission UI 和 Go BFF 的 WorkOS AuthKit
实现，包括 PKCE、加密 HttpOnly session cookie、过期 session refresh、单一
operator organization 限制、同源 mutation 检查，以及 BFF 业务路由权限守卫。
这些改动已通过本地 fake WorkOS 测试。真实 WorkOS project、AuthKit URL、
organization 和 RBAC 已配置，但 API key、cookie password、operator 用户和生产
BFF 部署仍未完成；上面的“当前事实”仍描述已发布 SHA 的生产状态。

### WorkOS 控制面状态（已配置，尚未部署）

独立项目：`LLM Gateway`（`project_01M1DGMA7D6KG6FQH4D14W58MT`）。

| 配置 | Staging | Production |
| --- | --- | --- |
| Environment ID | `environment_01M1DGMA7P0D0YE2E7C91YZBG2` | `environment_01M1DGMAX9ST1GKYDGMFEXAHJE` |
| Client ID | `client_01M1DGMAP4NTQBTJKSM82D7M73` | `client_01M1DGMB23EB15CDTND3GG84NA` |
| Operator organization ID | `org_01M1DGRHCZ4TPH3KSJE7V7QNA3` | `org_01M1DGSFPJREACZ25VMYSRWK36` |
| Redirect URI | `http://localhost:5173/api/auth/callback` | `https://llm-console.paxtech.net/api/auth/callback` |
| Logout URI | `http://localhost:5173/` | `https://llm-console.paxtech.net/` |
| Open sign-up | disabled | disabled |
| MFA | optional | required |

两个环境都已创建代码所需的 11 个自定义 permission，以及：

- `operator-admin`：完整 Ops Console 权限，包括 Playground 和所有 mutation。
- `operator-viewer`：只读权限，不包括 Playground。

两个 Operator organization 当前都没有已接受邀请的用户。已向
`toddzheng@paxtech.net` 发送 7 天有效的 `operator-admin` 邀请，当前状态均为
`Pending`：Staging `invitation_01M1DH10GPTSV269BQ2TS4XNAS`，Production
`invitation_01M1DH12X29PAKYWJSTXARA67Y`。WorkOS MCP 不返回或生成可供 BFF
使用的 secret；仍需从 WorkOS Dashboard 创建/取得环境 API key，并直接写入本地
secret store 或 GCP Secret Manager，不要写入仓库或交接文档。

生产 BFF 需要同时配置：

```text
BFF_PUBLIC_URL=https://<console-domain>
BFF_WORKOS_API_KEY=<secret>
BFF_WORKOS_CLIENT_ID=<client-id>
BFF_WORKOS_COOKIE_PASSWORD=<at-least-32-random-characters>
BFF_WORKOS_OPERATOR_ORGANIZATION_ID=<org-id>
BFF_SESSION_COOKIE_SECURE=true
```

缺少全部 WorkOS 变量时认证保持关闭；部分配置、非 HTTPS 生产 origin、
HTTPS 下的非 Secure cookie，以及非 loopback 的 dev auth 都会启动失败。

因此：前端实现和本地联调可以开始，但不能把“登录页面完成”描述为生产集成完成。

## 决定的认证边界

```text
Browser
  -> same-origin Go BFF
     -> WorkOS AuthKit Hosted UI / Session API
     -> private Cloud Run Gateway
     -> private Cloud Run Control Plane
     -> private Cloud Run Metering
```

职责分配：

| 层 | 责任 |
| --- | --- |
| WorkOS AuthKit | 用户登录、session、organization membership、role、permissions |
| React Console | 展示登录/session/权限状态；发起同源请求；不接触任何服务端 secret |
| Go BFF | OAuth/PKCE callback、sealed session cookie、session 验证/刷新、CSRF/Origin 检查、权限守卫、上游身份注入 |
| Control Plane / Metering | 独立验证 Human IAM assertion，并从已验证 claim 派生 Actor Envelope |
| Gateway API Key | 认证推理 workload，并绑定 Tenant、policy、quota 和 usage attribution |
| Cloudflare | DNS、TLS、WAF、cache bypass 和边缘防护；不再承担 Human IAM |

不要把 WorkOS API key、WorkOS access token 和 Gateway API Key 当成同一种凭证。

## 前端不可违反的规则

1. 不要把 WorkOS API key、client secret、refresh token、Gateway API Key 或上游管理 token 写入 React 环境变量、bundle、localStorage、sessionStorage、日志或错误上报。
2. 不要从浏览器直接调用 WorkOS token endpoint、Cloud Run、Control Plane、Metering 或 Gateway。
3. 不要自行解析 JWT 后把解析结果当作授权依据。前端可以根据 BFF 返回的 permissions 控制 UI，但 BFF 和上游服务必须再次执行服务端授权。
4. 不要给现有 API 请求添加 Gateway bearer token。所有业务请求保持 same-origin `/api/...`。
5. 不要用 WorkOS API Keys 替换现有 Gateway API Keys。后者仍由 Gateway control plane 管理。
6. 不要依赖 email 作为稳定身份或 Tenant 主键。用户身份使用 WorkOS `user_id`；组织绑定由后端解析。
7. 401 与 403 分开处理：401 表示 session 不存在/过期；403 表示当前已登录用户没有权限，不能用“重新登录”掩盖权限不足。

## 前端消费的 BFF 契约

以下接口是目标契约，当前尚未在 BFF 实现。前端 agent 可以据此定义类型和 UI，但端到端验收必须等后端接口落地。

### 开始登录

```http
GET /api/auth/login?return_to=%2F%23%2Foverview
```

BFF 生成 state/PKCE verifier，保存受保护的短期登录状态，然后 `302` 到 WorkOS Hosted AuthKit。`return_to` 只能接受同源 hash/path；BFF 必须拒绝外部 URL。

前端行为：

```ts
window.location.assign(`/api/auth/login?return_to=${encodeURIComponent(window.location.pathname + window.location.hash)}`);
```

不要用 `fetch()` 发起登录，因为登录需要浏览器跟随顶层重定向。

### OAuth callback

```http
GET /api/auth/callback?code=...&state=...
```

该路由只由 BFF 和 WorkOS 使用。BFF 交换 code、创建 sealed session cookie，并重定向回登录前页面。React 不读取 callback query、code、access token 或 refresh token。

### 读取当前 session

```http
GET /api/auth/session
Accept: application/json
```

已登录响应：

```json
{
  "authenticated": true,
  "user": {
    "id": "user_...",
    "email": "operator@example.com",
    "first_name": "Dev",
    "last_name": "Operator",
    "profile_picture_url": null
  },
  "organization": {
    "id": "org_...",
    "name": "Platform Operators"
  },
  "role": "platform-admin",
  "permissions": [
    "platform:tenants:read",
    "platform:tenants:write",
    "platform:metering:read",
    "platform:metering:write",
    "gateway:models:read",
    "gateway:playground:use"
  ]
}
```

未登录响应：

```http
HTTP/1.1 401 Unauthorized
Content-Type: application/json
```

```json
{
  "error": {
    "code": "session_required",
    "message": "Sign in is required"
  }
}
```

过期且无法刷新使用 `session_expired`。BFF 应在返回前清理无效 cookie。

前端不得要求响应携带 `access_token`、`refresh_token`、`session_id` 或 Gateway Tenant/API Key。

### 登出

```http
POST /api/auth/logout
Accept: application/json
```

```json
{
  "redirect_to": "https://<workos-auth-domain>/..."
}
```

BFF 先清除应用 session cookie，再返回经过校验的 WorkOS logout URL。前端收到成功响应后执行：

```ts
window.location.assign(response.redirect_to);
```

不要把 `redirect_to` 保存到 storage，也不要接受用户提供的 logout URL。

### 切换 Organization（第二阶段）

第一阶段只允许一个平台 operator organization，不需要 organization switcher。

后续客户控制台需要时使用：

```http
POST /api/auth/organization
Content-Type: application/json

{"organization_id":"org_..."}
```

BFF 验证 membership，通过 WorkOS refresh 流程切换组织并轮换 sealed session，然后返回与 `GET /api/auth/session` 相同的 session 视图。前端不能通过 query/header 自行声明 Gateway Tenant。

## 前端实现切片

### 1. Session bootstrap

建议新增：

```text
web/src/auth/types.ts
web/src/auth/AuthProvider.tsx
web/src/auth/RequireSession.tsx
web/src/auth/permissions.ts
web/src/auth/SignInPage.tsx
```

`AuthProvider` 在应用启动时只请求一次 `/api/auth/session`，提供：

```ts
type AuthState =
  | { status: "loading" }
  | { status: "anonymous"; reason: "session_required" | "session_expired" }
  | { status: "authenticated"; session: SessionView }
  | { status: "error"; error: ApiError };
```

要求：

- bootstrap 完成前不渲染任何业务页面，避免先发出一批必然失败的 API 请求；
- anonymous 显示独立的登录 surface；
- 网络/5xx 错误显示 Retry，不能误判成未登录；
- session 状态只存在 React memory，不写 localStorage；
- localStorage 仍只保留现有 theme 设置。

### 2. API client 的 session 行为

修改 `web/src/api/client.ts`：

- 所有 `fetch` 明确设置 `credentials: "same-origin"`；浏览器默认如此，但显式声明能固定契约；
- 保留现有 JSON/SSE 解析和 AbortSignal 行为；
- 识别 `session_required`、`session_expired`，通知 AuthProvider 进入 anonymous；
- 不要在每个并行请求收到 401 时分别跳转，避免 redirect storm；
- 403 `permission_denied` 保持普通 `ApiError`，由页面显示；
- SSE 在开始前收到 401 时进入 anonymous；流已经开始后断开时保留现有 partial-output 行为。

仓库还有三处直接 `fetch`，改动 auth client 时一并检查，避免绕过统一 session 行为：

- `web/src/pages/overview/lib.ts`
- `web/src/pages/operations/hooks.ts`
- `web/src/pages/usage/exports.tsx`

可以将它们迁入统一 client，也可以抽出共享的 `authFetch`；不要复制 401 处理。

### 3. App 和 Layout

修改 `web/src/App.tsx`：

```text
ToastProvider
  -> AuthProvider
     -> RequireSession
        -> Layout + active page
```

修改 `web/src/components/layout.tsx`：

- 删除硬编码的 `dev-operator` 和 `DO`；
- 显示 session 的姓名，缺失时回退到 email，再回退到 user id；
- 头像优先使用 `profile_picture_url`，否则显示根据姓名/email 生成的首字母；
- 用户菜单包含 email、organization、role、Sign out；
- 第一阶段不要显示无效的 organization switcher；
- 用户菜单和登录按钮必须支持键盘操作、focus ring 和 Escape 关闭。

### 4. Permission-aware UI

前端权限仅用于隐藏/禁用无权操作并解释原因，不能替代服务端授权。

建议权限矩阵：

| 页面/动作 | WorkOS permission |
| --- | --- |
| Overview 的 Tenant 数据 | `platform:tenants:read` |
| Overview 的 Usage 数据 | `platform:metering:read` |
| Playground | `gateway:playground:use` |
| Models & Capabilities | `gateway:models:read` |
| Tenants 查询 | `platform:tenants:read` |
| Tenant/key/policy mutation | `platform:tenants:write` |
| Provider Connections 查询 | `platform:providers:read` |
| Provider mutation/probe/discovery/rotation | `platform:providers:write` |
| Routing Catalog 查询 | `platform:routing:read` |
| Routing draft/publish/restore | `platform:routing:write` |
| Usage & Metering 查询 | `platform:metering:read` |
| correction/export mutation | `platform:metering:write` |
| Operations | `platform:operations:read` |

说明：WorkOS 可以使用细粒度 permission；后端 adapter 可把多个 WorkOS permission 映射到当前较粗的 Gateway scope。前端必须只使用 session 响应里的 permission，不自行推导 role 含义。

UI 规则：

- 完全无页面读取权限时从侧边栏隐藏该项；
- 有读取权限但无写权限时保留页面，隐藏或禁用 mutation action；
- Overview 可以部分展示，并明确标注哪些 panel 因权限不可用；
- route 在权限变更后失效时导航到第一个可访问页面；
- 服务端仍返回 403 时展示明确错误，不刷新 session 或自动重试 mutation。

### 5. 登录和错误体验

登录页使用现有设计 token 和组件，不引入第二套 UI 框架。至少包含：

- 产品名和 “Operations console”；
- “Sign in with WorkOS” 主按钮；
- session expired 的明确提示；
- 登录失败时 Retry；
- 不展示 WorkOS/API 内部错误体或 token；
- 不在 URL 中长期保留 auth error、code 或 state。

## WorkOS 与 Gateway Tenant 的关系

第一阶段目标是内部 Ops Console：

- 只允许一个受控 WorkOS operator organization；
- WorkOS role/permissions 决定 platform read/write 能力；
- Playground 使用服务器端受限 canary Gateway API Key；
- 前端不展示或选择 Gateway API Key。

后续客户控制台：

- 一个 WorkOS Organization 需要通过后端受控绑定映射到一个 Gateway Tenant；
- 每个组织的 Playground 调用必须使用该 Tenant 对应的 Gateway API Key；
- BFF 从 Secret Manager/Secret Custody 解析 key；
- 前端只能看到组织显示信息和权限，不能收到绑定记录或 secret。

不要假设 `org_id === tenant_id`。即使早期测试数据相同，也必须通过后端绑定解析。

## 本地开发契约

前端不实现自己的 auth bypass。后端应提供显式的 BFF development auth mode，让 `/api/auth/session` 返回固定 dev session；生产配置必须 fail closed。

本地运行仍使用：

```sh
docker compose up -d postgres
make run-dev
make run-control-plane-dev
make run-metering-dev
make run-bff-dev
cd web && npm install && npm run dev
```

Vite 继续把 `/api` 代理到 `http://localhost:8090`。前端不需要 WorkOS secret 或 GCP credential。

如需测试真实 WorkOS callback，redirect URI 必须指向可被浏览器访问的 BFF callback；不要把 WorkOS callback 路由实现到 Vite SPA。

## 前端完成标准

- [ ] 未登录时不渲染 Layout 或触发业务 API 请求。
- [ ] 登录按钮通过顶层 navigation 进入 `/api/auth/login`，并保留安全的同源 return path。
- [ ] callback 完成后回到原 hash route。
- [ ] session cookie 不可被 JavaScript 读取；前端源码和构建产物中没有 WorkOS/Gateway secret。
- [ ] header 显示真实用户、organization 和 role；可以登出。
- [ ] API client、直接 fetch 和 SSE 都携带 same-origin cookie，并统一处理 session 失效。
- [ ] 401 进入 anonymous；403 留在当前页面显示权限错误。
- [ ] NAV、页面和 mutation action 根据 permissions 正确隐藏/禁用。
- [ ] viewer 角色不能从 UI 发起任何 mutation；服务端 403 仍能正确显示。
- [ ] 刷新页面后通过 BFF 恢复 session，不使用 localStorage。
- [ ] `npm run build` 通过，production sourcemap 继续关闭。
- [ ] 本地完整回归现有八个板块、Playground SSE、Stop/Abort 和 error envelope。

## 不属于前端 agent 的工作

- WorkOS project/application、redirect URI、custom auth domain 和 RBAC dashboard 配置。
- WorkOS API key、cookie password 和 Secret Manager 管理。
- Go BFF OAuth/PKCE、sealed session、refresh、CSRF 和 logout 实现。
- WorkOS JWT/JWKS verifier、permission-to-scope adapter 和 organization-to-Tenant binding。
- BFF 调用私有 Cloud Run 时的 service identity token。
- `CONTROL_IAM_DENY_ALL` 移除、Cloud Run BFF 服务、LB、DNS、TLS、WAF 和 cache bypass。
- 生产最终域名的 SSE flush、超时、body limit 和 origin bypass 验证。

如果这些后端能力尚未合并，前端 agent 应完成类型、组件和权限逻辑，但不得加入可在生产绕过认证的 fallback。

## 外部参考

- [WorkOS AuthKit Sessions](https://workos.com/docs/authkit/sessions)
- [WorkOS Session Helpers](https://workos.com/docs/reference/authkit/session-helpers)
- [WorkOS RBAC Integration](https://workos.com/docs/rbac/integration)
- [WorkOS Go SDK](https://github.com/workos/workos-go)
- [Cloud Run service-to-service authentication](https://docs.cloud.google.com/run/docs/authenticating/service-to-service)

## Suggested skills

- `llm-gateway-control-plane`：核对 Human IAM claim、Actor Envelope、scope 和 Tenant isolation。
- `llm-gateway-operations`：核对 BFF/Cloud Run/Secret Manager/最终域名部署与生产验证。
- `llm-gateway-architecture`：当 WorkOS Organization、Gateway Tenant 或平台/客户控制台边界需要调整时使用。
- `review`：前端完成后审查 session bootstrap、权限守卫、401/403、SSE 和 secret exposure。
