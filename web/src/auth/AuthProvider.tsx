import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from "react";
import { apiGet, apiSend, ApiError, setSessionLostHandler } from "../api/client";
import { useToast } from "../components/feedback";
import { hasPermission } from "./permissions";
import type { AuthState, SessionView } from "./types";

const AUTH_QUERY_PARAMS = ["auth_error", "error", "error_description", "code", "state"];
let sessionRequestAttempt = -1;
let sessionRequest: Promise<SessionView> | null = null;

function bootstrapSession(attempt: number): Promise<SessionView> {
  if (sessionRequest === null || sessionRequestAttempt !== attempt) {
    sessionRequestAttempt = attempt;
    sessionRequest = apiGet<SessionView>("/auth/session");
  }
  return sessionRequest;
}

function hasAuthFailure(): boolean {
  const params = new URL(window.location.href).searchParams;
  return params.has("auth_error") || params.has("error");
}

/** Remove callback material from the address bar after the first render. */
function clearAuthQueryParams(): void {
  const url = new URL(window.location.href);
  let changed = false;
  for (const key of AUTH_QUERY_PARAMS) {
    if (url.searchParams.has(key)) {
      url.searchParams.delete(key);
      changed = true;
    }
  }
  if (changed) {
    const query = url.searchParams.toString();
    window.history.replaceState(null, "", `${url.pathname}${query ? `?${query}` : ""}${url.hash}`);
  }
}

interface AuthContextValue {
  state: AuthState;
  /** Re-run the session bootstrap after a network/5xx failure. */
  retry: () => void;
  /** Top-level navigation into the BFF login redirect (never fetch()). */
  signIn: () => void;
  signOut: () => void;
  /** Permission check for UI gating only; the server re-authorizes. */
  can: (permission: string) => boolean;
}

const AuthContext = createContext<AuthContextValue | null>(null);

/**
 * Bootstraps the session once on app start via GET /api/auth/session and keeps
 * it in React memory only (no localStorage). Any 401 session_required /
 * session_expired surfaced by the API client flips an authenticated session
 * back to anonymous — exactly once, regardless of how many parallel requests
 * failed.
 */
export function AuthProvider({ children }: { children: ReactNode }) {
  const toast = useToast();
  const [state, setState] = useState<AuthState>({ status: "loading" });
  const [attempt, setAttempt] = useState(0);
  const [loginFailed] = useState(hasAuthFailure);

  useEffect(() => clearAuthQueryParams(), []);

  useEffect(() => {
    setSessionLostHandler((reason) => {
      setState((s) => (s.status === "authenticated" ? { status: "anonymous", reason } : s));
    });
    return () => setSessionLostHandler(null);
  }, []);

  useEffect(() => {
    let cancelled = false;
    bootstrapSession(attempt).then(
      (session) => {
        if (!cancelled) setState({ status: "authenticated", session });
      },
      (err: unknown) => {
        if (cancelled) return;
        if (err instanceof ApiError && err.status === 401 && (err.code === "session_required" || err.code === "session_expired")) {
          setState({ status: "anonymous", reason: loginFailed ? "login_failed" : err.code });
        } else if (err instanceof ApiError && err.status === 403 && err.code === "organization_denied") {
          setState({ status: "forbidden", reason: "organization_denied" });
        } else {
          setState({ status: "error", error: err instanceof ApiError ? err : new ApiError(0, "network", String(err)) });
        }
      },
    );
    return () => {
      cancelled = true;
    };
  }, [attempt, loginFailed]);

  const retry = useCallback(() => {
    setState({ status: "loading" });
    setAttempt((a) => a + 1);
  }, []);

  const signIn = useCallback(() => {
    const returnTo = window.location.pathname + window.location.hash;
    window.location.assign(`/api/auth/login?return_to=${encodeURIComponent(returnTo)}`);
  }, []);

  const signOut = useCallback(() => {
    apiSend<{ redirect_to: string }>("POST", "/auth/logout").then(
      (result) => window.location.assign(result.redirect_to),
      () => toast("Sign out failed. Please retry.", "error"),
    );
  }, [toast]);

  const can = useCallback(
    (permission: string) => (state.status === "authenticated" ? hasPermission(state.session, permission) : false),
    [state],
  );

  const value = useMemo(() => ({ state, retry, signIn, signOut, can }), [state, retry, signIn, signOut, can]);
  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error("useAuth must be used inside AuthProvider");
  return ctx;
}
