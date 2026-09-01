import type { ApiError } from "../api/client";

/** Session view returned by `GET /api/auth/session`. Never carries tokens. */
export interface SessionView {
  authenticated: true;
  user: {
    id: string;
    email: string;
    first_name: string;
    last_name: string;
    profile_picture_url: string | null;
  };
  organization: {
    id: string;
    name: string;
  };
  role: string;
  permissions: string[];
}

export type SessionLostReason = "session_required" | "session_expired";
export type SignInReason = SessionLostReason | "login_failed";

export type AuthState =
  | { status: "loading" }
  | { status: "anonymous"; reason: SignInReason }
  | { status: "forbidden"; reason: "organization_denied" }
  | { status: "authenticated"; session: SessionView }
  | { status: "error"; error: ApiError };
