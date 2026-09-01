import { useApi } from "../api/useApi";
import { ErrorBanner, Loading } from "../components/feedback";
import { Card, CodeBlock } from "../components/ui";

/**
 * Placeholder page: runs a smoke GET against the BFF and renders the standard
 * loading / error / data states. Follow-up agents replace these with the real
 * section UIs; keep the smoke `path` pointed at the section's primary list API.
 */
export function SmokePage({ title, path }: { title: string; path: string }) {
  const { data, error, loading, retry } = useApi<unknown>(path);
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 16, maxWidth: 960 }}>
      <h1 style={{ fontSize: 18, fontWeight: 700 }}>{title}</h1>
      {loading && <Loading />}
      {error && <ErrorBanner error={error} retry={retry} />}
      {data !== null && !error && (
        <Card title={`Smoke check: GET /api${path}`}>
          <CodeBlock code={JSON.stringify(data, null, 2).slice(0, 4000)} lang="json" />
        </Card>
      )}
    </div>
  );
}
