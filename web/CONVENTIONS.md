# Web Console Conventions

Operations console for the Universal LLM Gateway. Vite + React 18 + TypeScript, no router
library, no CSS framework, no chart library. Design reference: `docs/design/fe1.html`.

## Commands

```bash
npm install
npm run dev        # vite dev server on :5173, proxies /api → http://localhost:8090 (BFF)
npm run build      # tsc -b && vite build → web/dist (served by the BFF in production)
npm run extract-fonts  # re-extract Geist woff2 subsets from docs/design/fe1.html
```

## File layout

```
web/
  index.html
  vite.config.ts            # /api proxy → localhost:8090
  public/fonts/             # extracted Geist / Geist Mono woff2 (generated, committed)
  scripts/extract-fonts.mjs # font extractor (generated src/styles/fonts.css, do not edit by hand)
  src/
    main.tsx                # entry, imports styles/fonts.css, tokens.css, base.css
    App.tsx                 # ToastProvider → AuthProvider → RequireSession → Layout + page
    router.ts               # hash routing (useRoute, navigate)
    styles/tokens.css       # ALL design tokens (CSS custom properties)
    styles/base.css         # reset, scrollbars, typography, :focus-visible ring
    auth/types.ts           # SessionView, AuthState
    auth/AuthProvider.tsx   # session bootstrap, useAuth() { state, retry, signIn, signOut, can }
    auth/RequireSession.tsx # gates the console behind a valid session
    auth/SignInPage.tsx     # standalone sign-in surface
    auth/UserMenu.tsx       # identity, organization, role, sign-out menu
    auth/permissions.ts     # PERMISSIONS constants + hasPermission
    api/client.ts           # ApiError, apiGet, apiSend, streamSSE, authFetch, setSessionLostHandler
    api/useApi.ts           # useApi<T>(path) GET-on-mount hook
    lib/format.ts           # formatting helpers
    components/layout.tsx   # sidebar/header/breadcrumbs shell, NAV definitions
    components/ui.tsx       # Button, Badge, Card, StatCard, Tabs, Table/Th/Td,
                            # KeyValueList, CopyButton, CodeBlock, EmptyState, Spinner
    components/feedback.tsx # Drawer, Modal, ConfirmDialog, ToastProvider/useToast,
                            # ErrorBanner, Loading
    components/charts.tsx   # BarChart, LineChart, SegmentedBar, Sparkline (inline SVG)
    pages/<section>/index.tsx  # one directory per nav section
    pages/shared.tsx        # SmokePage placeholder (replace per section)
```

## Auth & permissions

The console sits behind a BFF session (WorkOS AuthKit; see
`docs/handsoff/2026-08-31-workos-frontend-integration-handoff.md`).

- `AuthProvider` fetches `GET /api/auth/session` once at startup and keeps the session in
  React memory only — never localStorage (localStorage stays limited to the theme).
- `RequireSession` blocks all pages until bootstrap resolves; 401 → SignInPage,
  `organization_denied` → Access denied, network/5xx → Retry surface.
- Sign in uses top-level navigation. Sign out uses `apiSend` to POST the same-origin
  logout endpoint, then performs a top-level navigation to its validated `redirect_to`.
- Every request goes through `apiGet`/`apiSend`/`streamSSE`/`authFetch` with
  `credentials: "same-origin"`; 401 `session_required`/`session_expired` flips the app to
  anonymous via the registered session-lost handler; 403 `permission_denied` stays a plain
  `ApiError` for the page to render.
- UI gating uses `useAuth().can(PERMISSIONS.*)`: sections without read permission are
  hidden from the sidebar (NAV entries carry `permission`), mutation triggers are
  `disabled` with a `title="Requires <permission>"` tooltip. Frontend permissions only
  hide/disable — the BFF and upstreams re-authorize server-side.
- Never put WorkOS tokens, Gateway API keys, or any secret into env vars, the bundle,
  storage, logs, or error reports. Never call WorkOS or Cloud Run directly.

Local development uses the BFF development auth mode (`make run-bff-dev` sets
`BFF_DEV_AUTH=true` and binds `127.0.0.1`): `/api/auth/session` returns a fixed dev session with the full
permission set. Set `BFF_DEV_AUTH_PERMISSIONS="platform:tenants:read,..."` to exercise
permission-gated UI. Without `BFF_DEV_AUTH`, the BFF uses WorkOS only when all
required WorkOS variables are configured; otherwise auth and every business
proxy fail closed.

## Routing

Hash routes only: `#/overview`, `#/tenants`, … Default route is `overview`.

```ts
import { useRoute, navigate } from "../router";
const route = useRoute();      // reactive first path segment
navigate("tenants");           // sets location.hash
```

To add a page: create `src/pages/<name>/index.tsx` with a default-exported component,
register it in `App.tsx` (`PAGES` map) and add a `NAV` entry in
`components/layout.tsx` (24x24 stroke SVG path for the icon; set `permission` when the
section requires a WorkOS read permission).

## API client

Base URL is same-origin `/api`. The BFF strips `/api` and fans out:
`/api/llm/*` → gateway, `/api/control/*` → control plane, `/api/metering/*` → metering.

```ts
import { apiGet, apiSend, ApiError, streamSSE } from "../api/client";
import { useApi } from "../api/useApi";

// GET on mount (loading/error/data/retry built in):
const { data, error, loading, retry } = useApi<TenantList>("/control/v1/tenants");

// Mutations — never set Idempotency-Key yourself on control mutations unless the
// caller already has one; the BFF generates one when absent:
await apiSend<Tenant>("POST", "/control/v1/tenants", { name, region });
await apiSend("DELETE", `/control/v1/api-keys/${id}`);

// SSE (Responses API streaming). No [DONE] sentinel — frames arrive until the
// stream ends. Named events: frame.event, JSON payload: frame.data.
const controller = new AbortController();
for await (const frame of streamSSE("/llm/responses", { model, input, stream: true }, controller.signal)) {
  if (frame.event === "response.output_text.delta") { /* JSON.parse(frame.data) */ }
}
```

## Error-handling pattern (required in every page)

```tsx
try {
  await apiSend(...);
  toast("Tenant suspended", "success");
} catch (err) {
  if (err instanceof ApiError) toast(`${err.code}: ${err.message}`, "error");
  else throw err;
}
```

For page-level loads use `useApi` + the standard surfaces:

```tsx
if (loading) return <Loading />;
if (error) return <ErrorBanner error={error} retry={retry} />;
if (!data?.items?.length) return <EmptyState title="No tenants" hint="Create one to get started" />;
```

`ErrorBanner` automatically renders an amber configuration banner for
`503 upstream_not_configured` (BFF token unset) — do not special-case it.

## Component props (signatures)

```tsx
<Button variant="primary|ghost|danger" disabled onClick>…</Button>
<Badge tone="green|amber|red|blue|purple|neutral">Healthy</Badge>
<Card title="…" actions={<Button/>}>…</Card>
<StatCard label="Requests" value="48,203" sub="+12% vs prior" />
<Tabs tabs={["Overview","Keys"]} active={tab} onChange={setTab} />
<Table><thead><tr><Th>Name</Th></tr></thead><tbody><tr><Td mono>tn_9f3k…</Td></tr></tbody></Table>
<KeyValueList items={[{ key: "Region", value: "us-west", mono: true }]} />
<CopyButton text={secret} label="Copy" />
<CodeBlock code={json} lang="json" />
<EmptyState title="…" hint="…" action={<Button/>} />
<Spinner size={18} />
<Drawer open={open} onClose={…} title="…" width={420}>…</Drawer>
<Modal open={open} onClose={…} title="…" footer={<>…</>}>…</Modal>
<ConfirmDialog open onClose onConfirm={(reason) => …} title="…" description={…} confirmLabel="Suspend" />
  // confirm button disabled until reason (required textarea) is non-empty
const toast = useToast(); toast("Saved", "success" | "error" | "info");
<BarChart data={[{ label, value }]} height={120} color="var(--blue)" />
<LineChart data={[1,2,3]} height={120} fill />        // area fill default on
<SegmentedBar segments={[{ value, color: "var(--green)", label }]} />
<Sparkline data={[…]} />
```

## Design tokens (use var(--*) only — never hardcode colors)

Surfaces/text: `--bg`, `--panel`, `--ink`, `--ink2` (muted), `--ink3` (faint), `--line`, `--chip`, `--on-accent`.
Semantic (each has a matching `--<c>-bg`): `--blue` (primary), `--green`, `--amber`, `--red`, `--purple`.
`--shadow`, `--radius-sm|–|–lg|--radius-pill`, `--font-ui`, `--font-mono`,
`--sidebar-w`, `--sidebar-w-collapsed`, `--header-h`.
Dark theme = `body[data-theme="dark"]`; the header toggle persists to `localStorage("ugw.theme")`.
Fonts: Geist (UI) / Geist Mono (code), extracted subsets in `public/fonts/`.

Status → tone mapping follows the mockup: healthy/active/enabled/succeeded → green,
degraded/pending/grace/propagating → amber, notready/suspended/revoked/failed → red,
running/draft → blue, translated → purple, unknown/stale/disabled → neutral.

## Formatting helpers (`lib/format.ts`)

```ts
formatMicrosUSD(412180000)   // "$412.18"
formatNumber(48203)          // "48,203"
timeAgo(iso)                 // "41s ago" / "3m ago" / "2d ago"
formatDateTime(iso)          // "Aug 30, 2026 14:05"
truncateId("tn_9f3kQm2W8Lp4") // "tn_9f3k…8Lp4"
```

## Styling rules

- Inline `style={{}}` with token vars is the house style (matches the mockup); no CSS
  modules, no Tailwind. Reuse the components above instead of hand-rolling.
- Compact density: base font 14px, labels 11–12px, headings 13–18px; radii 6–10px.
  Use integer px font sizes only — fractional px blurs on non-retina displays.
- Keep components small and typed; add new shared ones to `ui.tsx`, `feedback.tsx`,
  or `charts.tsx` by concern.
