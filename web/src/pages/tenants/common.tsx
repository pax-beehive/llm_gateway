/**
 * Small primitives shared by the Tenants and Provider Connections consoles.
 * Both directories are owned by the same feature work, so providers imports
 * from here rather than duplicating form/pagination plumbing.
 */
import {
  useCallback,
  useEffect,
  useState,
  useSyncExternalStore,
  type CSSProperties,
  type ReactNode,
} from "react";
import { apiGet, ApiError } from "../../api/client";
import { Modal, useToast } from "../../components/feedback";
import { Button, CopyButton, type BadgeTone } from "../../components/ui";
import type { Paged } from "./types";

/* ------------------------------------------------------------------ */
/* Form styling                                                        */
/* ------------------------------------------------------------------ */

export const inputStyle: CSSProperties = {
  width: "100%",
  padding: "6px 10px",
  borderRadius: "var(--radius)",
  border: "1px solid var(--line)",
  background: "var(--bg)",
  color: "var(--ink)",
  fontSize: 12,
};

export const monoInputStyle: CSSProperties = { ...inputStyle, fontFamily: "var(--font-mono)", fontSize: 11.5 };

export const labelStyle: CSSProperties = {
  display: "block",
  fontSize: 11,
  fontWeight: 600,
  color: "var(--ink3)",
  marginBottom: 4,
};

export const hintStyle: CSSProperties = { fontSize: 11, color: "var(--ink3)", marginTop: 4 };

export function Field({ label, hint, children }: { label: string; hint?: ReactNode; children: ReactNode }) {
  return (
    <div style={{ marginBottom: 12 }}>
      <label style={labelStyle}>{label}</label>
      {children}
      {hint && <div style={hintStyle}>{hint}</div>}
    </div>
  );
}

/** Parse an optional JSON-object textarea. Empty input → {} (field omitted). */
export function parseJsonObject(text: string): { value?: Record<string, unknown>; error?: string } {
  const trimmed = text.trim();
  if (!trimmed) return {};
  try {
    const parsed: unknown = JSON.parse(trimmed);
    if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
      return { error: "must be a JSON object" };
    }
    return { value: parsed as Record<string, unknown> };
  } catch (err) {
    return { error: err instanceof Error ? err.message : "invalid JSON" };
  }
}

/** Comma/newline-separated textarea → string array; empty → undefined (omit). */
export function parseStringList(text: string): string[] | undefined {
  const items = text
    .split(/[\n,]/)
    .map((s) => s.trim())
    .filter((s) => s.length > 0);
  return items.length > 0 ? items : undefined;
}

/* ------------------------------------------------------------------ */
/* Routing helpers                                                     */
/* ------------------------------------------------------------------ */

function subscribeHash(callback: () => void): () => void {
  window.addEventListener("hashchange", callback);
  return () => window.removeEventListener("hashchange", callback);
}

/** Path segments after the first (route) segment, URL-decoded. "#/tenants/tn_1" → ["tn_1"]. */
export function useHashTail(): string[] {
  const hash = useSyncExternalStore(subscribeHash, () => window.location.hash, () => "");
  const clean = hash.replace(/^#\/?/, "").split("?")[0] ?? "";
  return clean
    .split("/")
    .slice(1)
    .filter((s) => s.length > 0)
    .map((s) => {
      try {
        return decodeURIComponent(s);
      } catch {
        return s;
      }
    });
}

/* ------------------------------------------------------------------ */
/* Status tones / error toasts                                         */
/* ------------------------------------------------------------------ */

export function statusTone(status: string | undefined): BadgeTone {
  switch (status) {
    case "active":
    case "enabled":
    case "succeeded":
    case "healthy":
      return "green";
    case "degraded":
    case "pending":
    case "grace":
    case "propagating":
    case "queued":
    case "uncertain":
      return "amber";
    case "notready":
    case "suspended":
    case "revoked":
    case "failed":
    case "closed":
      return "red";
    case "running":
    case "draft":
      return "blue";
    case "translated":
      return "purple";
    default:
      return "neutral";
  }
}

type ToastFn = (message: string, tone?: "success" | "error" | "info") => void;

/**
 * Standard mutation error surface. On 409 revision_conflict the affected
 * resource is refetched (via onConflict) and the user is told to retry.
 */
export function toastMutationError(toast: ToastFn, err: unknown, onConflict?: () => void): void {
  if (err instanceof ApiError) {
    if (err.code === "revision_conflict") {
      toast("revision_conflict: another writer saved first — latest data reloaded, review and retry", "error");
      onConflict?.();
    } else {
      toast(`${err.code}: ${err.message}`, "error");
    }
    return;
  }
  throw err;
}

/* ------------------------------------------------------------------ */
/* Cursor-paginated list hook (limit 25, "Load more")                  */
/* ------------------------------------------------------------------ */

export interface PagedList<T> {
  items: T[];
  nextCursor: string | null;
  loading: boolean;
  loadingMore: boolean;
  error: ApiError | null;
  loadMore: () => void;
  reload: () => void;
}

export function usePagedList<T>(path: string): PagedList<T> {
  const [items, setItems] = useState<T[]>([]);
  const [nextCursor, setNextCursor] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [loadingMore, setLoadingMore] = useState(false);
  const [error, setError] = useState<ApiError | null>(null);
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    const controller = new AbortController();
    setLoading(true);
    setError(null);
    apiGet<Paged<T>>(path, { signal: controller.signal })
      .then((page) => {
        if (controller.signal.aborted) return;
        setItems(page.data ?? []);
        setNextCursor(page.next_cursor || null);
        setLoading(false);
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setError(err instanceof ApiError ? err : new ApiError(0, "network", String(err)));
        setLoading(false);
      });
    return () => controller.abort();
  }, [path, attempt]);

  const loadMore = useCallback(() => {
    if (!nextCursor) return;
    setLoadingMore(true);
    const sep = path.includes("?") ? "&" : "?";
    apiGet<Paged<T>>(`${path}${sep}cursor=${encodeURIComponent(nextCursor)}`)
      .then((page) => {
        setItems((current) => [...current, ...(page.data ?? [])]);
        setNextCursor(page.next_cursor || null);
        setLoadingMore(false);
      })
      .catch((err: unknown) => {
        setError(err instanceof ApiError ? err : new ApiError(0, "network", String(err)));
        setLoadingMore(false);
      });
  }, [path, nextCursor]);

  const reload = useCallback(() => setAttempt((a) => a + 1), []);
  return { items, nextCursor, loading, loadingMore, error, loadMore, reload };
}

/* ------------------------------------------------------------------ */
/* ReasonModal — ConfirmDialog pattern with non-destructive variant    */
/* ------------------------------------------------------------------ */

export function ReasonModal({
  open,
  onClose,
  onConfirm,
  title,
  description,
  confirmLabel = "Confirm",
  danger,
  busy,
  children,
}: {
  open: boolean;
  onClose: () => void;
  onConfirm: (reason: string) => void;
  title: string;
  description?: ReactNode;
  confirmLabel?: string;
  danger?: boolean;
  busy?: boolean;
  children?: ReactNode;
}) {
  const [reason, setReason] = useState("");
  useEffect(() => {
    if (open) setReason("");
  }, [open]);

  return (
    <Modal
      open={open}
      onClose={onClose}
      title={title}
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button
            variant={danger ? "danger" : "primary"}
            disabled={reason.trim().length === 0 || busy}
            onClick={() => onConfirm(reason.trim())}
          >
            {confirmLabel}
          </Button>
        </>
      }
    >
      {description && <p style={{ fontSize: 12, color: "var(--ink2)", marginBottom: 12 }}>{description}</p>}
      {children}
      <label style={labelStyle}>Reason (required)</label>
      <textarea
        value={reason}
        onChange={(e) => setReason(e.target.value)}
        rows={3}
        placeholder="Why is this change needed?"
        style={{ ...inputStyle, resize: "vertical" }}
      />
    </Modal>
  );
}

/* ------------------------------------------------------------------ */
/* SecretPanel — show-once credential reveal                           */
/* ------------------------------------------------------------------ */

export function SecretPanel({
  secret,
  heading,
  onClose,
}: {
  secret: string;
  heading: string;
  onClose: () => void;
}) {
  const toast = useToast();
  const [acknowledged, setAcknowledged] = useState(false);

  const download = () => {
    const blob = new Blob([secret], { type: "text/plain" });
    const url = URL.createObjectURL(blob);
    const anchor = document.createElement("a");
    anchor.href = url;
    anchor.download = "gateway-api-key-secret.txt";
    anchor.click();
    URL.revokeObjectURL(url);
    toast("Secret downloaded", "info");
  };

  return (
    <div
      role="alert"
      style={{
        border: "1px solid var(--amber)",
        background: "var(--amber-bg)",
        borderRadius: "var(--radius-lg)",
        padding: 14,
        marginBottom: 14,
      }}
    >
      <div style={{ fontSize: 13, fontWeight: 700, color: "var(--amber)" }}>{heading}</div>
      <p style={{ fontSize: 12, color: "var(--ink2)", margin: "6px 0 10px" }}>
        The raw secret is shown <b>exactly once</b>. It is never stored by the console, logged, or returned by the
        API again — idempotent replays return metadata only. Copy it now and store it in a secret manager.
      </p>
      <code
        style={{
          display: "block",
          padding: 10,
          borderRadius: "var(--radius)",
          background: "var(--panel)",
          border: "1px solid var(--line)",
          fontFamily: "var(--font-mono)",
          fontSize: 12,
          wordBreak: "break-all",
          marginBottom: 10,
        }}
      >
        {secret}
      </code>
      <div style={{ display: "flex", gap: 8, flexWrap: "wrap", alignItems: "center" }}>
        <CopyButton text={secret} label="Copy secret" />
        <Button onClick={download}>Download .txt</Button>
      </div>
      <label
        style={{ display: "flex", alignItems: "center", gap: 8, fontSize: 12, color: "var(--ink2)", marginTop: 12 }}
      >
        <input type="checkbox" checked={acknowledged} onChange={(e) => setAcknowledged(e.target.checked)} />I have
        copied and securely stored this secret
      </label>
      <div style={{ marginTop: 10 }}>
        <Button variant="primary" disabled={!acknowledged} onClick={onClose}>
          Done — discard secret
        </Button>
      </div>
    </div>
  );
}
