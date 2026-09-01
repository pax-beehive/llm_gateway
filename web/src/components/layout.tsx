import { useEffect, useState, type ReactNode } from "react";
import { navigate, useRoute } from "../router";

export interface NavDef {
  id: string;
  label: string;
  /** 24x24 stroke icon path data (lucide-style, stroke=currentColor, fill=none). */
  icon: string;
}

export const NAV: NavDef[] = [
  { id: "overview", label: "Overview", icon: "M3 3h7v9H3zM14 3h7v5h-7zM14 12h7v9h-7zM3 16h7v5H3z" },
  { id: "playground", label: "Playground", icon: "m4 17 6-6-6-6M12 19h8" },
  {
    id: "models",
    label: "Models & Capabilities",
    icon: "M21 8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73l7 4a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16ZM3.3 7 12 12l8.7-5M12 22V12",
  },
  {
    id: "tenants",
    label: "Tenants",
    icon: "M6 22V4a2 2 0 0 1 2-2h8a2 2 0 0 1 2 2v18M6 12H4a2 2 0 0 0-2 2v8h4M18 9h2a2 2 0 0 1 2 2v11h-4M10 6h4M10 10h4M10 14h4M10 18h4",
  },
  { id: "providers", label: "Provider Connections", icon: "M12 22v-4M9 8V2M15 8V2M18 8v6a4 4 0 0 1-4 4h-4a4 4 0 0 1-4-4V8Z" },
  { id: "routing", label: "Routing Catalog", icon: "M6 3v12M18 9a3 3 0 1 0 0-6 3 3 0 0 0 0 6zM6 21a3 3 0 1 0 0-6 3 3 0 0 0 0 6zM15 6a9 9 0 0 0-9 9" },
  { id: "usage", label: "Usage & Metering", icon: "M3 3v18h18M18 17V9M13 17V5M8 17v-3" },
  { id: "operations", label: "Operations", icon: "M22 12h-4l-3 9L9 3l-3 9H2" },
];

const THEME_KEY = "ugw.theme";

function initialTheme(): "light" | "dark" {
  const stored = window.localStorage.getItem(THEME_KEY);
  return stored === "dark" ? "dark" : "light";
}

function Icon({ d, size = 16 }: { d: string; size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2} strokeLinecap="round" strokeLinejoin="round" aria-hidden>
      <path d={d} />
    </svg>
  );
}

export function Layout({ children }: { children: ReactNode }) {
  const route = useRoute();
  const [collapsed, setCollapsed] = useState(false);
  const [theme, setTheme] = useState<"light" | "dark">(initialTheme);

  useEffect(() => {
    document.body.dataset.theme = theme;
    window.localStorage.setItem(THEME_KEY, theme);
  }, [theme]);

  const active = NAV.find((n) => n.id === route) ?? NAV[0];

  return (
    <div style={{ display: "flex", height: "100%", overflow: "hidden" }}>
      <aside
        style={{
          width: collapsed ? "var(--sidebar-w-collapsed)" : "var(--sidebar-w)",
          flexShrink: 0,
          background: "var(--panel)",
          borderRight: "1px solid var(--line)",
          display: "flex",
          flexDirection: "column",
          transition: "width .15s ease",
          overflow: "hidden",
        }}
      >
        <div
          style={{
            display: "flex",
            alignItems: "center",
            gap: 10,
            padding: collapsed ? "14px 0" : "14px 14px",
            justifyContent: collapsed ? "center" : "flex-start",
            borderBottom: "1px solid var(--line)",
            minHeight: "var(--header-h)",
          }}
        >
          <div
            style={{
              width: 28,
              height: 28,
              borderRadius: "var(--radius)",
              background: "var(--blue)",
              color: "#fff",
              display: "grid",
              placeItems: "center",
              fontWeight: 700,
              fontSize: 13,
              flexShrink: 0,
            }}
          >
            G
          </div>
          {!collapsed && (
            <div style={{ minWidth: 0 }}>
              <div style={{ fontWeight: 600, fontSize: 12, whiteSpace: "nowrap" }}>Universal LLM Gateway</div>
              <div style={{ fontSize: 10, color: "var(--ink3)", whiteSpace: "nowrap" }}>Operations console</div>
            </div>
          )}
        </div>
        <nav style={{ flex: 1, padding: 8, display: "flex", flexDirection: "column", gap: 2, overflowY: "auto" }}>
          {NAV.map((item) => {
            const isActive = item.id === active.id;
            return (
              <button
                key={item.id}
                onClick={() => navigate(item.id)}
                title={collapsed ? item.label : undefined}
                style={{
                  display: "flex",
                  alignItems: "center",
                  gap: 10,
                  padding: collapsed ? "8px 0" : "8px 10px",
                  justifyContent: collapsed ? "center" : "flex-start",
                  border: "none",
                  borderRadius: "var(--radius)",
                  background: isActive ? "var(--blue-bg)" : "transparent",
                  color: isActive ? "var(--blue)" : "var(--ink2)",
                  fontSize: 12,
                  fontWeight: isActive ? 600 : 500,
                  whiteSpace: "nowrap",
                }}
              >
                <Icon d={item.icon} />
                {!collapsed && item.label}
              </button>
            );
          })}
        </nav>
        <button
          onClick={() => setCollapsed((c) => !c)}
          title={collapsed ? "Expand sidebar" : "Collapse sidebar"}
          style={{
            margin: 8,
            padding: 6,
            border: "1px solid var(--line)",
            borderRadius: "var(--radius)",
            background: "transparent",
            color: "var(--ink3)",
            fontSize: 11,
          }}
        >
          {collapsed ? "»" : "«"}
        </button>
      </aside>

      <div style={{ flex: 1, display: "flex", flexDirection: "column", minWidth: 0 }}>
        <header
          style={{
            height: "var(--header-h)",
            flexShrink: 0,
            display: "flex",
            alignItems: "center",
            gap: 12,
            padding: "0 16px",
            background: "var(--panel)",
            borderBottom: "1px solid var(--line)",
          }}
        >
          <nav style={{ fontSize: 12, color: "var(--ink3)", whiteSpace: "nowrap" }}>
            Console <span style={{ margin: "0 4px" }}>/</span>{" "}
            <span style={{ color: "var(--ink)", fontWeight: 600 }}>{active.label}</span>
          </nav>
          <div style={{ flex: 1 }} />
          <div
            style={{
              display: "flex",
              alignItems: "center",
              gap: 6,
              padding: "5px 10px",
              border: "1px solid var(--line)",
              borderRadius: "var(--radius)",
              color: "var(--ink3)",
              fontSize: 12,
              background: "var(--bg)",
              minWidth: 220,
            }}
          >
            <Icon d="M11 19a8 8 0 1 0 0-16 8 8 0 0 0 0 16zM21 21l-4.35-4.35" size={13} />
            <span style={{ flex: 1 }}>Search resources…</span>
            <kbd
              style={{
                fontFamily: "var(--font-mono)",
                fontSize: 10,
                background: "var(--chip)",
                borderRadius: 4,
                padding: "1px 5px",
              }}
            >
              ⌘K
            </kbd>
          </div>
          <button
            onClick={() => setTheme((t) => (t === "dark" ? "light" : "dark"))}
            title={theme === "dark" ? "Switch to light theme" : "Switch to dark theme"}
            style={{
              border: "1px solid var(--line)",
              background: "transparent",
              color: "var(--ink2)",
              borderRadius: "var(--radius)",
              padding: 5,
              display: "grid",
              placeItems: "center",
            }}
          >
            <Icon
              d={
                theme === "dark"
                  ? "M12 3v2M12 19v2M4.2 4.2l1.4 1.4M18.4 18.4l1.4 1.4M3 12h2M19 12h2M4.2 19.8l1.4-1.4M18.4 5.6l1.4-1.4M16 12a4 4 0 1 1-8 0 4 4 0 0 1 8 0z"
                  : "M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z"
              }
            />
          </button>
          <div
            style={{
              display: "flex",
              alignItems: "center",
              gap: 8,
              padding: "4px 10px 4px 4px",
              border: "1px solid var(--line)",
              borderRadius: "var(--radius-pill)",
              fontSize: 12,
              color: "var(--ink2)",
            }}
          >
            <span
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
              DO
            </span>
            dev-operator
          </div>
        </header>
        <main style={{ flex: 1, overflowY: "auto", padding: 20 }}>{children}</main>
      </div>
    </div>
  );
}
