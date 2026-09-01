import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useState,
  type ReactNode,
} from "react";
import { ApiError } from "../api/client";
import { Button, EmptyState, Spinner } from "./ui";

/* ------------------------------------------------------------------ */
/* Drawer (right side)                                                 */
/* ------------------------------------------------------------------ */

export function Drawer({
  open,
  onClose,
  title,
  width = 420,
  children,
}: {
  open: boolean;
  onClose: () => void;
  title: ReactNode;
  width?: number;
  children: ReactNode;
}) {
  if (!open) return null;
  return (
    <>
      <div
        onClick={onClose}
        style={{ position: "fixed", inset: 0, background: "rgba(10,14,20,.4)", zIndex: 40 }}
      />
      <aside
        role="dialog"
        aria-label={typeof title === "string" ? title : undefined}
        style={{
          position: "fixed",
          top: 0,
          right: 0,
          bottom: 0,
          width,
          maxWidth: "90vw",
          background: "var(--panel)",
          borderLeft: "1px solid var(--line)",
          boxShadow: "-8px 0 24px rgba(16,24,40,.12)",
          zIndex: 41,
          display: "flex",
          flexDirection: "column",
        }}
      >
        <header
          style={{
            display: "flex",
            alignItems: "center",
            justifyContent: "space-between",
            padding: "14px 16px",
            borderBottom: "1px solid var(--line)",
          }}
        >
          <h2 style={{ fontSize: 13, fontWeight: 600 }}>{title}</h2>
          <Button onClick={onClose} aria-label="Close drawer">
            ✕
          </Button>
        </header>
        <div style={{ flex: 1, overflowY: "auto", padding: 16 }}>{children}</div>
      </aside>
    </>
  );
}

/* ------------------------------------------------------------------ */
/* Modal / ConfirmDialog                                               */
/* ------------------------------------------------------------------ */

export function Modal({
  open,
  onClose,
  title,
  children,
  footer,
}: {
  open: boolean;
  onClose: () => void;
  title: ReactNode;
  children: ReactNode;
  footer?: ReactNode;
}) {
  if (!open) return null;
  return (
    <div
      onClick={onClose}
      style={{
        position: "fixed",
        inset: 0,
        background: "rgba(10,14,20,.4)",
        display: "grid",
        placeItems: "center",
        zIndex: 50,
      }}
    >
      <div
        role="dialog"
        aria-label={typeof title === "string" ? title : undefined}
        onClick={(e) => e.stopPropagation()}
        style={{
          width: 480,
          maxWidth: "92vw",
          maxHeight: "85vh",
          overflowY: "auto",
          background: "var(--panel)",
          border: "1px solid var(--line)",
          borderRadius: "var(--radius-lg)",
          boxShadow: "0 12px 40px rgba(16,24,40,.2)",
        }}
      >
        <header
          style={{
            display: "flex",
            alignItems: "center",
            justifyContent: "space-between",
            padding: "14px 16px",
            borderBottom: "1px solid var(--line)",
          }}
        >
          <h2 style={{ fontSize: 13, fontWeight: 600 }}>{title}</h2>
          <Button onClick={onClose} aria-label="Close dialog">
            ✕
          </Button>
        </header>
        <div style={{ padding: 16 }}>{children}</div>
        {footer && (
          <footer
            style={{
              display: "flex",
              justifyContent: "flex-end",
              gap: 8,
              padding: "12px 16px",
              borderTop: "1px solid var(--line)",
            }}
          >
            {footer}
          </footer>
        )}
      </div>
    </div>
  );
}

/**
 * Destructive-action confirmation. The confirm button stays disabled until the
 * operator types a non-empty reason; the reason is handed back via onConfirm.
 */
export function ConfirmDialog({
  open,
  onClose,
  onConfirm,
  title,
  description,
  confirmLabel = "Confirm",
}: {
  open: boolean;
  onClose: () => void;
  onConfirm: (reason: string) => void;
  title: string;
  description?: ReactNode;
  confirmLabel?: string;
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
            variant="danger"
            disabled={reason.trim().length === 0}
            onClick={() => onConfirm(reason.trim())}
          >
            {confirmLabel}
          </Button>
        </>
      }
    >
      {description && <p style={{ fontSize: 12, color: "var(--ink2)", marginBottom: 12 }}>{description}</p>}
      <label style={{ display: "block", fontSize: 11, fontWeight: 600, color: "var(--ink3)", marginBottom: 4 }}>
        Reason (required)
      </label>
      <textarea
        value={reason}
        onChange={(e) => setReason(e.target.value)}
        rows={3}
        placeholder="Why is this change needed?"
        style={{
          width: "100%",
          resize: "vertical",
          padding: 8,
          borderRadius: "var(--radius)",
          border: "1px solid var(--line)",
          background: "var(--bg)",
          fontSize: 12,
        }}
      />
    </Modal>
  );
}

/* ------------------------------------------------------------------ */
/* Toasts                                                              */
/* ------------------------------------------------------------------ */

export interface Toast {
  id: number;
  message: string;
  tone: "success" | "error" | "info";
}

const ToastContext = createContext<(message: string, tone?: Toast["tone"]) => void>(() => undefined);

let nextToastId = 1;

export function ToastProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([]);

  const push = useCallback((message: string, tone: Toast["tone"] = "info") => {
    const id = nextToastId++;
    setToasts((current) => [...current, { id, message, tone }]);
    window.setTimeout(() => setToasts((current) => current.filter((t) => t.id !== id)), 4000);
  }, []);

  const toneColor = { success: "var(--green)", error: "var(--red)", info: "var(--blue)" } as const;
  const toneBg = { success: "var(--green-bg)", error: "var(--red-bg)", info: "var(--blue-bg)" } as const;

  return (
    <ToastContext.Provider value={push}>
      {children}
      <div style={{ position: "fixed", bottom: 16, right: 16, zIndex: 60, display: "flex", flexDirection: "column", gap: 8 }}>
        {toasts.map((toast) => (
          <div
            key={toast.id}
            role="status"
            style={{
              padding: "10px 14px",
              borderRadius: "var(--radius)",
              fontSize: 12,
              fontWeight: 600,
              color: toneColor[toast.tone],
              background: toneBg[toast.tone],
              border: "1px solid var(--line)",
              boxShadow: "var(--shadow)",
              maxWidth: 360,
            }}
          >
            {toast.message}
          </div>
        ))}
      </div>
    </ToastContext.Provider>
  );
}

export function useToast(): (message: string, tone?: Toast["tone"]) => void {
  return useContext(ToastContext);
}

/* ------------------------------------------------------------------ */
/* ErrorBanner / Loading                                               */
/* ------------------------------------------------------------------ */

/**
 * Standard error surface for failed API calls. Shows a configuration hint for
 * 503 upstream_not_configured; otherwise code/message plus a retry button.
 */
export function ErrorBanner({ error, retry }: { error: ApiError; retry?: () => void }) {
  if (error.isUpstreamNotConfigured) {
    return (
      <div
        role="alert"
        style={{
          padding: "12px 14px",
          borderRadius: "var(--radius)",
          background: "var(--amber-bg)",
          color: "var(--amber)",
          fontSize: 12,
          fontWeight: 600,
        }}
      >
        This upstream is not configured on the BFF ({error.message}). Set the corresponding BFF_*_TOKEN and reload.
      </div>
    );
  }
  return (
    <div
      role="alert"
      style={{
        display: "flex",
        alignItems: "center",
        gap: 12,
        padding: "12px 14px",
        borderRadius: "var(--radius)",
        background: "var(--red-bg)",
        color: "var(--red)",
        fontSize: 12,
      }}
    >
      <span style={{ fontWeight: 700 }}>{error.code}</span>
      <span style={{ flex: 1 }}>{error.message}</span>
      {retry && <Button onClick={retry}>Retry</Button>}
    </div>
  );
}

/** Standard loading surface. */
export function Loading() {
  return (
    <div style={{ display: "flex", justifyContent: "center", padding: 48 }}>
      <Spinner />
    </div>
  );
}

export { EmptyState };
