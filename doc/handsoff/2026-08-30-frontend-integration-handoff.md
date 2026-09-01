# LLM Gateway 前端集成交接

更新时间：2026-08-30（America/Los_Angeles）

## 目的

这份文档供推理产品前端和 BFF 开发使用。它说明当前生产 Gateway 已经验证的接口、首版 UI 应采用的集成边界、已知限制，以及公开上线前仍需完成的基础设施。

管理控制台不属于本次首版集成范围。Control Plane 当前仍以 `CONTROL_IAM_DENY_ALL=true` 运行，必须先完成 Cloudflare Access 身份适配才能接入管理前端。

## 当前生产状态

- Git SHA：`eb91d513491e57db3426b023a34e9cb387ecdb75`
- Gateway revision：`llm-gateway-prod-gateway-00003-j8n`
- Gateway image：`sha256:9b7290bbb8f4257eb5f019118d4a98941087ae51b47a4b1914c1d0026f10f16e`
- Cloud Run：Ready，100% 流量
- 当前公开模型 ID：`gpt-5.6-luna`
- CI：[run 33357569909](https://github.com/pax-beehive/llm_gateway/actions/runs/33357569909)
- Production deployment：[run 33357741585](https://github.com/pax-beehive/llm_gateway/actions/runs/33357741585)

生产环境已从同一 GCP VPC 完成以下真实验证：

```text
GET  /readyz             -> 200
GET  /v1/models          -> 200, includes gpt-5.6-luna
POST /v1/responses       -> 200, completed
POST /v1/responses SSE   -> 200, completed
```

这些结果证明 Gateway、API key、Routing Catalog、quota、OpenAI Provider credential 和 Responses adapter 已能形成完整调用链。它们不代表浏览器公开入口已经创建。

## 集成结论

前端可以开始开发和 BFF 联调，但不要从浏览器直接访问 Cloud Run，也不要把 Gateway API key 放进浏览器、JavaScript bundle、localStorage、日志或错误上报。

首版采用以下边界：

```text
Browser
  -> same-origin application BFF
  -> future llm-api.paxtech.net
  -> GCP External Application Load Balancer
  -> private Cloud Run Gateway
```

浏览器的 `Authorization` 只用于应用自身的用户 session。BFF 在服务端读取 Gateway API key，并在发往 Gateway 时设置：

```http
Authorization: Bearer <gateway-api-key>
```

不要使用当前 `run.app` 地址作为前端配置。Gateway ingress 是 `internal-and-cloud-load-balancing`，正式 origin 应由 ADR 0010 中的负载均衡和 Cloudflare 路径提供。

## 建议的前端/BFF 接口

首版前端只依赖两个同源 BFF 接口：

```text
GET  /api/llm/models
POST /api/llm/responses
```

BFF 将它们分别转发到 Gateway：

```text
GET  /v1/models
POST /v1/responses
```

BFF 必须：

- 注入 Gateway API key；
- 不向浏览器返回或记录该 key；
- 保留 Gateway HTTP 状态码和结构化错误；
- 对 SSE 原样透传 `Content-Type`、事件名、data 和 flush；
- 设置 `Cache-Control: no-cache, no-transform`；
- 传播客户端断开和 `AbortSignal`，停止继续读取上游；
- 禁止代理、CDN 或应用框架缓冲流式响应；
- 对请求体设置合理上限；
- 不记录 prompt、输出正文或完整 Provider 错误体。

当前 Gateway HTTP server 没有 CORS/OPTIONS 实现。同源 BFF 可以避免在 Gateway 中开放浏览器 CORS。

## 模型列表

请求：

```http
GET /v1/models
Authorization: Bearer <gateway-api-key>
```

响应形状：

```json
{
  "object": "list",
  "data": [
    {
      "id": "gpt-5.6-luna",
      "object": "model",
      "created": 1788067855,
      "owned_by": "gateway"
    }
  ]
}
```

`created` 示例值仅用于说明类型，不要写死。前端必须以 `data[].id` 作为请求里的 public model ID，不应知道 Provider model、Route ID 或 Provider credential。

模型列表为空时，显示“当前没有可用模型”，不要回退到硬编码模型。

## 首版 Responses 请求

首版推荐无状态、流式请求：

```http
POST /v1/responses
Authorization: Bearer <gateway-api-key>
Content-Type: application/json
Accept: text/event-stream
```

```json
{
  "model": "gpt-5.6-luna",
  "input": "Explain this clearly.",
  "store": false,
  "stream": true
}
```

当前 canary 的必要约束：

- 必须设置 `store:false`；当前 Tenant 不允许 stored Responses。
- 暂时不要发送 `temperature`、`top_p`、`max_output_tokens` 或 `stop`。这些字段要求 route 发布 `sampling` capability，而当前 canary route 只发布 `text` 和 `streaming`。
- 服务端 quota policy 会把缺失的 `max_output_tokens` 收紧为 256。
- 暂不使用 `conversation`、`previous_response_id`、background mode、tools、reasoning 或 Cache Protection。
- 首版每次请求可发送一段字符串。多轮 UI 可先在客户端展示历史，但请求按单轮处理；若要重发上下文，必须控制在输入 token 上限内。

当前 canary 限额只用于集成验证，不是正式容量承诺：

```text
max input tokens       4096
max output tokens      256
max request cost       USD 0.005
requests per minute    10
tokens per minute      10000
daily spend            USD 0.10
monthly spend          USD 1.00
concurrent responses   1
```

## SSE 处理

Responses streaming 使用具名 SSE 事件：

```text
event: response.created
data: {...}

event: response.output_text.delta
data: {"sequence_number":2,"type":"response.output_text.delta","delta":"hello",...}

event: response.completed
data: {...}
```

还可能出现：

- `response.output_item.done`
- `response.failed`
- `: keepalive` 注释行

前端规则：

1. 按 SSE 空行分帧，不要按网络 chunk 分帧。
2. 忽略以 `:` 开头的 keepalive 注释。
3. 只把 `response.output_text.delta` 事件 JSON 中的 `delta` 字段追加到正在生成的 assistant message。
4. 使用 `sequence_number` 检测重复或倒序事件。
5. 收到 `response.completed` 后结束 loading，并读取其中的最终 Response/usage。
6. 收到 `response.failed` 后保留已经展示的文本并显示失败状态，不要自动重发。
7. Responses SSE 不使用 Chat Completions 的 `data: [DONE]` 作为结束条件。

浏览器应使用 `fetch()` 和 `ReadableStream` 解析 POST SSE；原生 `EventSource` 不支持所需的 POST body。

## 非流式响应形状

非流式请求将 `stream` 设为 `false` 或省略：

```json
{
  "model": "gpt-5.6-luna",
  "input": "Reply briefly.",
  "store": false
}
```

成功响应的核心字段：

```ts
type GatewayError = {
  code: string;
  message: string;
  type?: string;
  param?: string;
};

type GatewayResponse = {
  id: string;
  object: "response";
  created_at: number;
  completed_at?: number;
  status: "queued" | "in_progress" | "completed" | "failed" | "cancelled" | "deleted";
  model: string;
  output: Array<{
    id?: string;
    type: string;
    role?: string;
    content?: Array<{ type: string; text?: string }>;
  }>;
  usage: {
    input_tokens: number;
    output_tokens: number;
    total_tokens: number;
    cached_input_tokens?: number;
  };
  error?: GatewayError;
  revision: number;
};
```

展示文本时，拼接 `output` 中 `type === "message"` 的 item，再拼接其 `content` 中 `type === "output_text"` 的 `text`。不要依赖某个固定数组位置。

## 错误契约和重试

JSON 错误统一为：

```ts
type GatewayErrorEnvelope = {
  error: {
    code: string;
    message: string;
    type?: string;
    param?: string;
  };
};
```

前端/BFF 至少处理：

| HTTP | 常见 code | UI 行为 | 自动重试 |
| --- | --- | --- | --- |
| 400 | `invalid_request_error`, `route_not_found` | 显示请求或模型暂不可用 | 否 |
| 401 | `authentication_error` | 结束当前请求；检查应用 session/BFF 配置 | 否 |
| 403 | `policy_denied`, `cache_protection_not_allowed` | 显示权限或策略限制 | 否 |
| 409 | `idempotency_conflict`, `invalid_state` | 显示请求状态冲突 | 否 |
| 429 | `rate_limit_exceeded`, `policy_denied` | 显示限额；允许用户稍后手动重试 | 默认否 |
| 502 | `provider_unavailable`, `gateway_error` | 显示上游暂不可用 | 仅在没有任何可见输出且产品明确允许时手动重试 |
| 503 | `policy_coordination_unavailable` 等 | 显示服务暂不可用 | 可做有上限退避 |

流已经产生可见输出后，不得自动重发。Provider 可能已经产生费用或副作用，Gateway 对不确定结果会保守记账。

`Idempotency-Key` 仅适用于 `store:true` 的非流式 Response。当前 canary 使用 `store:false`，所以 BFF 不应添加该 header。

## 首版 UI 范围

建议首版包括：

- 启动时读取 `/api/llm/models`；
- 模型选择器；当前只有一个模型时仍保留该组件边界；
- 单轮 prompt 输入；
- 流式 assistant 输出；
- Stop 按钮，通过 `AbortController` 中断浏览器和 BFF 流；
- loading、completed、failed、cancelled 状态；
- 401、403、429、502、503 的明确错误文案；
- usage 的开发调试视图，默认不作为费用账单；
- 前端埋点只记录 request correlation、耗时、状态和 error code，不记录内容。

暂不实现：

- 浏览器直连 Gateway；
- 管理控制台；
- stored Responses 和 Conversations；
- tool calling/reasoning controls；
- temperature 和 token slider；
- Provider 或 Route 选择；
- 自动重试流式生成；
- 从 Secret Manager 读取任何 secret 的前端代码。

## 本地开发

仓库内置 deterministic echo Provider：

```sh
make run-dev
```

本地 Gateway：

```text
http://localhost:8080
Authorization: Bearer dev-token
model: echo-v1
```

生产 key 不得用于本地开发。推荐让本地 BFF 通过服务端环境变量连接本地 Gateway，浏览器仍只访问同源 `/api/llm/*`。

本地 echo 能验证 UI 状态机和 SSE parser，但不能替代通过最终 Cloudflare/LB 域名进行的生产前验收。

## 公开联调前的阻塞项

以下工作尚未完成，前端不能把它们当成现有能力：

1. GCP External Application Load Balancer、serverless NEG、origin certificate 和 Cloud Armor。
2. `llm-api.paxtech.net` Cloudflare DNS/proxy、Full strict TLS、WAF 和 `/v1/*` cache bypass。
3. 负载均衡到 private Cloud Run 的受控 invocation 路径。
4. 从最终域名验证 SSE flush、超时、请求体大小和 origin bypass 防护。
5. 前端 BFF 的应用 session、secret custody、限流和日志脱敏。
6. 若要建设管理 UI，先实现 Cloudflare Access identity adapter，并移除 Control Plane 的 deny-all 配置。

完整目标架构和上线顺序见 [`docs/adr/0010-deploy-cloudflare-edge-gcp-core.md`](../../docs/adr/0010-deploy-cloudflare-edge-gcp-core.md)。不要把 ADR 当成已经部署的证据。

## 前端验收清单

- [ ] 浏览器网络和构建产物中不存在 Gateway API key。
- [ ] 模型来自 `/api/llm/models`，没有硬编码 Provider model。
- [ ] BFF 能原样透传具名 SSE 并逐 token/片段刷新。
- [ ] keepalive 不会产生空消息。
- [ ] `response.completed` 正确关闭 loading。
- [ ] `response.failed` 保留已显示内容且不自动重试。
- [ ] Stop 会关闭浏览器和 BFF 上游连接。
- [ ] 429、502、503 有不同提示。
- [ ] 页面刷新不会把 prompt/response 内容发送到分析平台。
- [ ] Cloudflare、LB、BFF 均禁用 `/v1/*` 缓存和响应转换。
- [ ] 最终域名通过 models、non-streaming、streaming 和 direct-origin-bypass 测试。

## 代码和设计参考

- Gateway 使用说明：[`README.md`](../../README.md)
- Responses HTTP contract：[`internal/httpapi/server.go`](../../internal/httpapi/server.go)
- Response/Event 类型：[`internal/core/types.go`](../../internal/core/types.go)
- 部署架构与公开入口：[`docs/adr/0010-deploy-cloudflare-edge-gcp-core.md`](../../docs/adr/0010-deploy-cloudflare-edge-gcp-core.md)
- Gateway API key 边界：[`docs/adr/0004-build-gateway-api-key-administration-and-access-projection.md`](../../docs/adr/0004-build-gateway-api-key-administration-and-access-projection.md)
- 当前生产部署修复：commit `eb91d51`

## Suggested skills

下一位实现或审查者建议使用：

- `llm-gateway-architecture`：确认 BFF、Cloudflare、GCP LB 和 Gateway 的责任边界。
- `llm-gateway-operations`：验证生产 revision、流量、日志、最终域名和 origin bypass。
- `llm-gateway-control-plane`：仅在实现管理控制台身份和 Tenant/API key 管理时使用。
- `sites:sites-building`：创建或迭代实际前端界面时使用。
