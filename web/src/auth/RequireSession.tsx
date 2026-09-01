import type { ReactNode } from "react";
import { Button, Spinner } from "../components/ui";
import { useAuth } from "./AuthProvider";
import { SignInPage } from "./SignInPage";

function FullScreen({ children }: { children: ReactNode }) {
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
      {children}
    </div>
  );
}

/**
 * Gates the console behind a valid BFF session: nothing inside renders (and no
 * business API request fires) until the bootstrap resolves. Anonymous users
 * get the sign-in surface; network/5xx bootstrap failures get a Retry — a
 * failed /api/auth/session call is never mistaken for "logged out".
 */
export function RequireSession({ children }: { children: ReactNode }) {
  const { state, retry, signIn } = useAuth();

  switch (state.status) {
    case "loading":
      return (
        <FullScreen>
          <Spinner size={22} />
        </FullScreen>
      );
    case "anonymous":
      return <SignInPage reason={state.reason} />;
    case "forbidden":
      return (
        <FullScreen>
          <div style={{ display: "flex", flexDirection: "column", alignItems: "center", gap: 12, maxWidth: 420, textAlign: "center" }}>
            <div style={{ fontSize: 13, fontWeight: 600 }}>Access denied</div>
            <div style={{ fontSize: 12, color: "var(--ink2)" }}>
              Your WorkOS organization cannot access this operations console.
            </div>
            <Button variant="primary" onClick={signIn}>
              Sign in with another account
            </Button>
          </div>
        </FullScreen>
      );
    case "error":
      return (
        <FullScreen>
          <div style={{ display: "flex", flexDirection: "column", alignItems: "center", gap: 12, maxWidth: 420, textAlign: "center" }}>
            <div style={{ fontSize: 13, fontWeight: 600 }}>Could not reach the console backend</div>
            <div style={{ fontSize: 12, color: "var(--ink2)" }}>
              Check your connection and retry. If the problem continues, contact the platform team.
            </div>
            <Button variant="primary" onClick={retry}>
              Retry
            </Button>
          </div>
        </FullScreen>
      );
    default:
      return <>{children}</>;
  }
}
