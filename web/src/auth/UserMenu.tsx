import { useEffect, useRef, useState } from "react";
import { Badge } from "../components/ui";
import { useAuth } from "./AuthProvider";

function initials(text: string): string {
  const parts = text.trim().split(/[\s@.]+/).filter(Boolean);
  const letters = parts.slice(0, 2).map((part) => part[0]?.toUpperCase() ?? "");
  return letters.join("") || "?";
}

/** Session identity menu for the phase-one single-organization console. */
export function UserMenu() {
  const { state, signOut } = useAuth();
  const [open, setOpen] = useState(false);
  const rootRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const signOutRef = useRef<HTMLButtonElement>(null);

  useEffect(() => {
    if (!open) return;
    signOutRef.current?.focus();
    const onKey = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        setOpen(false);
        triggerRef.current?.focus();
      }
    };
    const onPointerDown = (event: MouseEvent) => {
      if (rootRef.current && !rootRef.current.contains(event.target as Node)) setOpen(false);
    };
    document.addEventListener("keydown", onKey);
    document.addEventListener("mousedown", onPointerDown);
    return () => {
      document.removeEventListener("keydown", onKey);
      document.removeEventListener("mousedown", onPointerDown);
    };
  }, [open]);

  if (state.status !== "authenticated") return null;
  const { user, organization, role } = state.session;
  const displayName = `${user.first_name} ${user.last_name}`.trim() || user.email || user.id;

  return (
    <div ref={rootRef} style={{ position: "relative" }}>
      <button
        ref={triggerRef}
        onClick={() => setOpen((current) => !current)}
        aria-haspopup="menu"
        aria-expanded={open}
        title={user.email}
        style={{
          display: "flex",
          alignItems: "center",
          gap: 8,
          padding: "4px 10px 4px 4px",
          border: "1px solid var(--line)",
          borderRadius: "var(--radius-pill)",
          fontSize: 12,
          color: "var(--ink2)",
          background: "transparent",
        }}
      >
        {user.profile_picture_url ? (
          <img src={user.profile_picture_url} alt="" style={{ width: 22, height: 22, borderRadius: "50%", objectFit: "cover" }} />
        ) : (
          <span
            aria-hidden
            style={{
              width: 22,
              height: 22,
              borderRadius: "50%",
              background: "var(--purple-bg)",
              color: "var(--purple)",
              display: "grid",
              placeItems: "center",
              fontWeight: 700,
              fontSize: 10,
            }}
          >
            {initials(displayName)}
          </span>
        )}
        {displayName}
      </button>
      {open && (
        <div
          role="menu"
          style={{
            position: "absolute",
            right: 0,
            top: "calc(100% + 6px)",
            minWidth: 230,
            background: "var(--panel)",
            border: "1px solid var(--line)",
            borderRadius: "var(--radius-lg)",
            boxShadow: "var(--shadow)",
            padding: 10,
            zIndex: 40,
            display: "flex",
            flexDirection: "column",
            gap: 8,
          }}
        >
          <div style={{ fontSize: 12, fontWeight: 600, color: "var(--ink)" }}>{displayName}</div>
          <div style={{ fontSize: 11, color: "var(--ink3)", wordBreak: "break-all" }}>{user.email}</div>
          <div style={{ display: "flex", alignItems: "center", gap: 6, fontSize: 11, color: "var(--ink2)" }}>
            <span style={{ overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{organization.name}</span>
            <Badge tone="blue">{role}</Badge>
          </div>
          <div style={{ borderTop: "1px solid var(--line)", paddingTop: 8 }}>
            <button
              ref={signOutRef}
              role="menuitem"
              onClick={() => {
                setOpen(false);
                signOut();
              }}
              style={{
                width: "100%",
                textAlign: "left",
                border: "none",
                background: "transparent",
                color: "var(--red)",
                fontSize: 12,
                fontWeight: 600,
                padding: "6px 8px",
                borderRadius: "var(--radius)",
              }}
            >
              Sign out
            </button>
          </div>
        </div>
      )}
    </div>
  );
}
