# Ops Console 前端落地：需要后端改动的交接

更新时间：2026-08-31（America/Los_Angeles）

## 背景

前端已按 `docs/design/fe1.html` 完成首版 Ops Console，代码在 `web/`（Vite + React 18 + TS，无路由/图表/UI 库），配套同源 BFF 在 `cmd/bff` + `internal/bff`（`make run-bff-dev`，默认 `:8090`）。八个板块全部实现并接真实接口，已在本地全栈验证（gateway `:8080` echo、control plane `:8081`、metering `:8082`、postgres compose）。

本文档只列**需要后端改动**的事项，按优先级分组。前端集成边界与 canary 约束仍以 [`2026-08-30-frontend-integration-handoff.md`](./2026-08-30-frontend-integration-handoff.md) 为准。

## 2026-08-31 实现状态

本轮已完成并接回前端：

- Routing Catalog 草稿 LIST：`GET /control/v1/routing-catalog/drafts`，使用 `(updated_at,id)` 稳定游标。
- Provider Operation LIST：`GET /control/v1/provider-operations?connection_id=...`，使用 `(created_at,id)` 稳定游标。
- Control Audit 查询：`GET /control/v1/audit`，平台可查全局，Tenant actor 强制限制到 acting Tenant。
- quota denial 证据流：Gateway 写 content-free `quota.denied` outbox，Metering 投影并提供 `GET /metering/v1/quota-denials`；归属决策见 ADR 0011。
- 平台 Metering summary/timeseries：platform scope 省略 `tenant_id` 即查询全平台，Overview 已移除逐 Tenant fan-out。
- Metering 错误信封现在包含 `code` 和 `message`。
- BFF 的 Gateway 代理仅开放 models、responses、healthz、readyz；Playground 忽略重复或乱序 SSE delta；生产 sourcemap 已关闭。

仍未实现的条目是 P1 6、8、9、10，以及生产化待办 17–20。本状态说明是代码与测试范围，不代表生产部署完成。

## P0 — 缺失端点（前端只能绕路或降级）

1. **Routing Catalog 草稿没有 LIST 端点**。
   现状：只有 `GET /control/v1/routing-catalog/drafts/{id}`。前端只能在 session 内记住创建过的 draft id，页面刷新后只能"按 id 打开"。
   请求：`GET /control/v1/routing-catalog/drafts?status=&cursor=&limit=`，按 `updated_at` 倒序。

2. **Provider Connection 的异步操作没有 LIST 端点**。
   现状：`POST .../probes|model-discoveries|credential-rotations` 返回单个 `Operation`，只能按 id 轮询 `GET /control/v1/provider-operations/{id}`。连接详情页的"最近操作"列表是会话级的，刷新即丢失。
   请求：`GET /control/v1/provider-connections/{id}/operations?cursor=&limit=` 或 `GET /control/v1/provider-operations?connection_id=`。

3. **审计日志（Audit Trail）完全没有查询端点**。
   控制面每个写操作都落了 append-only audit（actor envelope + reason），但没有任何读取接口。设计稿和操作预期里都有审计视图；当前前端只有 policy-revisions 里内嵌的 `actor_type/actor_id/change_reason` 可用。
   请求：`GET /control/v1/audit?tenant_id=&resource=&actor=&from=&through=&cursor=&limit=`。

4. **Quota 拒绝事件没有查询端点**。
   现状：只有 `quota-snapshot` 的聚合余额。设计稿的 "Why was my request quota-denied?" 类排查需要按时间列 quota 拒绝事件（tenant/key/route/维度/时间）。
   请求：在 metering 或 control plane 暴露 quota denial 事件流。

## P1 — 聚合与一致性问题（前端已绕路，但有代价）

5. **Metering 没有平台级聚合**。`filterFromRequest` 对 platform scope 身份强制要求 `tenant_id`，否则 403。
   前端绕法：Overview 页先 `GET /control/v1/tenants` 全量分页，再对每个 tenant 并发请求 `usage/summary` 和 `usage/timeseries` 做客户端聚合，部分失败时显示 "N of M tenants (partial)"。租户多后不可扩展。
   请求：`GET /metering/v1/usage/summary|timeseries` 支持平台级聚合（platform scope 且省略 tenant_id 时返回全平台合计）。

6. **Summary 没有 breakdown**。`Totals` 只有按币种的合计，没有 by-provider / by-model / by-tenant / by-outcome 分布。设计稿的 spend-by-provider/model/tenant 表和 outcome 分布条已删除。
   请求：summary 增加可选 `group_by=provider|public_model|tenant|outcome` 维度。

7. **错误信封不一致**。Control plane 是 `{"error":{"code","message"}}`，metering 是 `{"error":{"code"}}`（无 message）。前端已兼容两种，但排障时 metering 的错误没有可读信息。
   请求：metering 错误信封补 `message` 字段。

8. **Idempotency-Key 要求不一致**。Control plane 所有 mutation 强制要求（BFF 已代为生成）；metering 的 corrections 也要求但语义文档缺失，导出 `POST /usage/exports` 只读 query 参数、不收 body 也没有 reason。
   请求：明确各 mutation 的幂等语义；导出任务如需审计理由，补 `reason` 参数。

9. **Per-Gateway readiness 不是一等资源**。只有进程级 `/readyz`；单 gateway 的"就绪"只能由 `operations/gateways` 的 heartbeat/revision lag 推断。前端已在 UI 文案注明 liveness ≠ readiness。
   请求（可选）：`GatewaySummary` 增加派生的 readiness 判定字段，或文档明确推断规则。

10. **Operations 页全部只读**。Jobs 没有 retry/cancel，publications 没有 republish。设计稿也只做展示，但运维动作（重试 poison event、重发失败 publication）目前没有 API 承载。

## P2 — 契约陷阱（前端已适配，需写进 API 文档防下一个人踩坑）

11. **PUT policy 要求 `policy.revision = 当前 + 1`**（"policy revision must advance by one"），且 `expected_revision` 用当前 policy revision；发布成功后 Tenant `revision` 与 `policy.revision` 同步 +1。首版前端漏了 +1 导致 400，已修（`web/src/pages/tenants/policy.tsx`、`keys.tsx`）。

12. **Provider Connection 注册时 `capability_declaration.revision` 必须为 1**；`base_url` 按 provider 域名白名单校验（openai → `api.openai.com` 等），本地 echo provider 无法注册为 connection。

13. **ManagedRoute 校验规则**（draft validate 的全部条件）：`capability_profile_revision` 必须等于连接当前 profile revision；`execution_region` 必须与连接 region 一致；`provider_cost_snapshot` 必须携带与 provider/model/region 匹配的不可变身份；`selection_policy.weight > 0`；`tenant_visibility_policy` 必须声明 `all_tenants` 或显式 tenant_ids。前端路由表单已按此约束，但建议在 API 文档固化。

14. **capability 键名**是 `text` / `streaming` / `embeddings` / `moderation` / `rerank`（不是 `responses` / `moderations`）。

15. **`/readyz` 未就绪时返回 503 + `ReadinessResult` body**（不是错误信封）。前端按"body 即数据"处理；请在 API 文档中明确该端点不遵循错误信封约定。

16. **`GET /usage/exports/{id}/content` 无认证**（签名 URL，TTL 5 分钟）。前端直接以链接形式打开。请确认这在生产模型下可接受；若不可接受，需要改为 BFF 代理下载。

## 生产化待办（继承自前一份 handoff，前端侧已全部就绪）

17. Control Plane 仍是 `CONTROL_IAM_DENY_ALL=true`：先实现 Cloudflare Access identity adapter（`configureIdentityVerifier` 的 JWKS 路径已存在），才能暴露管理前端。前端/BFF 对 401 `invalid_identity_assertion` 已有明确处理。
18. `llm-api.paxtech.net` 的 LB/Cloudflare/WAF/cache-bypass 未建；BFF 的 SSE 行为（`Cache-Control: no-cache, no-transform`、`X-Accel-Buffering: no`、即时 flush、客户端断连传播）已按该路径的要求实现，但最终域名下需复验 flush/超时/请求体上限。
19. BFF 目前没有浏览器侧 session 认证（dev 全开）。产品 BFF 需要：应用 session、secret custody（当前为环境变量）、浏览器侧限流、日志脱敏。Gateway/Control/Metering 的 token 全部只在 BFF 服务端注入，前端包内无任何 key——已验收。
20. 若要让 Playground 暴露 temperature/top_p/max_output_tokens：需要 canary route 发布 `sampling` capability，前端再解锁对应控件（当前按 handoff 约束锁定）。

## 前端已验证的接口行为（后端改动时不要破坏）

- `GET /v1/models`、`POST /v1/responses`（流式/非流式）经 BFF 全链路通过，具名 SSE 事件、`sequence_number`、keepalive 注释行均符合交接文档。
- Control plane 全部 tenant/key/policy/quota-snapshot/provider-connection/routing-catalog/operations 端点经 BFF 验证，包括 CAS（`expected_revision` / 409 `revision_conflict`）、幂等重放、异步 Operation 轮询、publication receipts。
- Metering `operations/status`、`usage/summary|timeseries|events`（tenant 维度）经 BFF 验证。

## 本地复现

```sh
docker compose up -d postgres
make run-dev                 # gateway :8080, echo-v1, dev-token
make run-control-plane-dev   # :8081, local-control-admin-token
make run-metering-dev        # :8082, local-metering-admin-token
cd web && npm install && npm run build
make run-bff-dev             # :8090, 托管 web/dist + /api 代理
```

浏览器打开 `http://localhost:8090`。前端开发热更新：`cd web && npm run dev`（vite `:5173`，已配置 `/api` → `:8090` 代理）。

## Suggested skills

- `llm-gateway-control-plane`：实现 P0/P1 的 control-plane 端点（drafts list、audit、quota denials、operations 变更）。
- `llm-gateway-control-plane`（metering 部分）：平台级聚合、breakdown、错误信封与幂等语义对齐。
- `llm-gateway-architecture`：审计视图、quota denial 事件流的归属决策（control plane vs metering）。
- `llm-gateway-operations`：生产化待办 17–18 的落地与复验。
