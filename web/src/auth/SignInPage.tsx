import { Button } from "../components/ui";
import { useAuth } from "./AuthProvider";
import type { SignInReason } from "./types";

/**
 * Standalone sign-in surface (no Layout/sidebar). Uses the existing design
 * tokens and components; the button triggers a top-level navigation to the
 * BFF login route, which 302s to WorkOS Hosted AuthKit.
 */
export function SignInPage({ reason }: { reason: SignInReason }) {
  const { signIn } = useAuth();
  return (
    <div
      style={{
        minHeight: "100vh",
        display: "grid",
        placeItems: "center",
        background: "var(--bg)",
        padding: 20,
      }}
    >
      <div
        style={{
          width: 360,
          maxWidth: "100%",
          background: "var(--panel)",
          border: "1px solid var(--line)",
          borderRadius: "var(--radius-lg)",
          boxShadow: "var(--shadow)",
          padding: "28px 24px",
          display: "flex",
          flexDirection: "column",
          alignItems: "stretch",
          gap: 14,
        }}
      >
        <div style={{ display: "flex", alignItems: "center", gap: 10 }}>
          <div
            style={{
              width: 28,
              height: 28,
              borderRadius: "var(--radius)",
              background: "var(--blue)",
              color: "var(--on-accent)",
              display: "grid",
              placeItems: "center",
              fontWeight: 700,
              fontSize: 13,
              flexShrink: 0,
            }}
          >
            G
          </div>
          <div>
            <div style={{ fontWeight: 600, fontSize: 13 }}>Universal LLM Gateway</div>
            <div style={{ fontSize: 11, color: "var(--ink3)" }}>Operations console</div>
          </div>
        </div>
        {reason === "session_expired" && (
          <div
            role="status"
            style={{
              fontSize: 12,
              color: "var(--amber)",
              background: "var(--amber-bg)",
              borderRadius: "var(--radius)",
              padding: "8px 10px",
            }}
          >
            Your session has expired. Sign in again to continue.
          </div>
        )}
        {reason === "login_failed" && (
          <div
            role="alert"
            style={{
              fontSize: 12,
              color: "var(--red)",
              background: "var(--red-bg)",
              borderRadius: "var(--radius)",
              padding: "8px 10px",
            }}
          >
            Sign in could not be completed. Retry when you are ready.
          </div>
        )}
        <Button variant="primary" onClick={signIn} style={{ justifyContent: "center", padding: "8px 12px" }}>
          {reason === "login_failed" ? "Retry sign in" : "Sign in with WorkOS"}
        </Button>
        <p style={{ margin: 0, fontSize: 11, color: "var(--ink3)", textAlign: "center" }}>
          You will be redirected to your organization&apos;s sign-in page.
        </p>
      </div>
    </div>
  );
}
