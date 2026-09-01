import { useState, type ButtonHTMLAttributes, type CSSProperties, type ReactNode, type TableHTMLAttributes } from "react";

/* ------------------------------------------------------------------ */
/* Button                                                              */
/* ------------------------------------------------------------------ */

type ButtonVariant = "primary" | "ghost" | "danger";

const buttonStyles: Record<ButtonVariant, CSSProperties> = {
  primary: { background: "var(--blue)", color: "#fff", border: "1px solid transparent" },
  ghost: { background: "transparent", color: "var(--ink2)", border: "1px solid var(--line)" },
  danger: { background: "var(--red)", color: "#fff", border: "1px solid transparent" },
};

export interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: ButtonVariant;
}

export function Button({ variant = "ghost", style, ...rest }: ButtonProps) {
  return (
    <button
      {...rest}
      style={{
        padding: "6px 12px",
        borderRadius: "var(--radius)",
        fontSize: 12,
        fontWeight: 600,
        opacity: rest.disabled ? 0.5 : 1,
        cursor: rest.disabled ? "not-allowed" : "pointer",
        ...buttonStyles[variant],
        ...style,
      }}
    />
  );
}

/* ------------------------------------------------------------------ */
/* Badge                                                               */
/* ------------------------------------------------------------------ */

export type BadgeTone = "green" | "amber" | "red" | "blue" | "purple" | "neutral";

const badgeTones: Record<BadgeTone, CSSProperties> = {
  green: { color: "var(--green)", background: "var(--green-bg)" },
  amber: { color: "var(--amber)", background: "var(--amber-bg)" },
  red: { color: "var(--red)", background: "var(--red-bg)" },
  blue: { color: "var(--blue)", background: "var(--blue-bg)" },
  purple: { color: "var(--purple)", background: "var(--purple-bg)" },
  neutral: { color: "var(--ink2)", background: "var(--chip)" },
};

export function Badge({ tone = "neutral", children }: { tone?: BadgeTone; children: ReactNode }) {
  return (
    <span
      style={{
        display: "inline-flex",
        alignItems: "center",
        padding: "2px 8px",
        borderRadius: "var(--radius-pill)",
        fontSize: 11,
        fontWeight: 600,
        whiteSpace: "nowrap",
        ...badgeTones[tone],
      }}
    >
      {children}
    </span>
  );
}

/* ------------------------------------------------------------------ */
/* Card / StatCard                                                     */
/* ------------------------------------------------------------------ */

export function Card({
  title,
  actions,
  children,
  style,
}: {
  title?: ReactNode;
  actions?: ReactNode;
  children: ReactNode;
  style?: CSSProperties;
}) {
  return (
    <section
      style={{
        background: "var(--panel)",
        border: "1px solid var(--line)",
        borderRadius: "var(--radius-lg)",
        boxShadow: "var(--shadow)",
        padding: 16,
        ...style,
      }}
    >
      {(title || actions) && (
        <header style={{ display: "flex", alignItems: "center", justifyContent: "space-between", marginBottom: 12 }}>
          <h2 style={{ fontSize: 13, fontWeight: 600 }}>{title}</h2>
          {actions}
        </header>
      )}
      {children}
    </section>
  );
}

export function StatCard({ label, value, sub }: { label: string; value: ReactNode; sub?: ReactNode }) {
  return (
    <Card>
      <div style={{ fontSize: 11, color: "var(--ink3)", fontWeight: 600, textTransform: "uppercase", letterSpacing: ".04em" }}>
        {label}
      </div>
      <div style={{ fontSize: 22, fontWeight: 700, marginTop: 4 }}>{value}</div>
      {sub && <div style={{ fontSize: 11, color: "var(--ink2)", marginTop: 2 }}>{sub}</div>}
    </Card>
  );
}

/* ------------------------------------------------------------------ */
/* Tabs                                                                */
/* ------------------------------------------------------------------ */

export function Tabs({
  tabs,
  active,
  onChange,
}: {
  tabs: string[];
  active: string;
  onChange: (tab: string) => void;
}) {
  return (
    <div role="tablist" style={{ display: "flex", gap: 4, borderBottom: "1px solid var(--line)", marginBottom: 16 }}>
      {tabs.map((tab) => {
        const isActive = tab === active;
        return (
          <button
            key={tab}
            role="tab"
            aria-selected={isActive}
            onClick={() => onChange(tab)}
            style={{
              border: "none",
              background: "transparent",
              padding: "8px 12px",
              fontSize: 12,
              fontWeight: isActive ? 600 : 500,
              color: isActive ? "var(--blue)" : "var(--ink2)",
              borderBottom: isActive ? "2px solid var(--blue)" : "2px solid transparent",
              marginBottom: -1,
            }}
          >
            {tab}
          </button>
        );
      })}
    </div>
  );
}

/* ------------------------------------------------------------------ */
/* Table                                                               */
/* ------------------------------------------------------------------ */

export function Table({ children, ...rest }: TableHTMLAttributes<HTMLTableElement>) {
  return (
    <div style={{ overflowX: "auto" }}>
      <table
        {...rest}
        style={{ width: "100%", borderCollapse: "collapse", fontSize: 12, ...rest.style }}
      >
        {children}
      </table>
    </div>
  );
}

export function Th({ children }: { children?: ReactNode }) {
  return (
    <th
      style={{
        textAlign: "left",
        padding: "8px 10px",
        fontSize: 11,
        fontWeight: 600,
        color: "var(--ink3)",
        borderBottom: "1px solid var(--line)",
        whiteSpace: "nowrap",
      }}
    >
      {children}
    </th>
  );
}

export function Td({ children, mono }: { children?: ReactNode; mono?: boolean }) {
  return (
    <td
      style={{
        padding: "8px 10px",
        borderBottom: "1px solid var(--line)",
        fontFamily: mono ? "var(--font-mono)" : undefined,
        fontSize: mono ? 11 : undefined,
      }}
    >
      {children}
    </td>
  );
}

/* ------------------------------------------------------------------ */
/* KeyValueList                                                        */
/* ------------------------------------------------------------------ */

export function KeyValueList({ items }: { items: Array<{ key: string; value: ReactNode; mono?: boolean }> }) {
  return (
    <dl style={{ margin: 0, display: "grid", gridTemplateColumns: "minmax(140px, auto) 1fr", rowGap: 8, columnGap: 16 }}>
      {items.map(({ key, value, mono }) => (
        <div key={key} style={{ display: "contents" }}>
          <dt style={{ fontSize: 12, color: "var(--ink3)" }}>{key}</dt>
          <dd style={{ margin: 0, fontSize: 12, fontFamily: mono ? "var(--font-mono)" : undefined }}>{value}</dd>
        </div>
      ))}
    </dl>
  );
}

/* ------------------------------------------------------------------ */
/* CopyButton / CodeBlock                                              */
/* ------------------------------------------------------------------ */

export function CopyButton({ text, label = "Copy" }: { text: string; label?: string }) {
  const [copied, setCopied] = useState(false);
  return (
    <Button
      onClick={() => {
        void navigator.clipboard.writeText(text).then(() => {
          setCopied(true);
          window.setTimeout(() => setCopied(false), 1500);
        });
      }}
    >
      {copied ? "Copied" : label}
    </Button>
  );
}

export function CodeBlock({ code, lang }: { code: string; lang?: string }) {
  return (
    <div style={{ position: "relative" }}>
      <pre
        style={{
          margin: 0,
          padding: 12,
          background: "var(--chip)",
          borderRadius: "var(--radius)",
          fontSize: 11,
          lineHeight: 1.5,
          overflowX: "auto",
          fontFamily: "var(--font-mono)",
        }}
      >
        {lang && (
          <span style={{ float: "right", color: "var(--ink3)", fontSize: 10, textTransform: "uppercase" }}>{lang}</span>
        )}
        <code>{code}</code>
      </pre>
      <div style={{ position: "absolute", top: 8, right: 8 }}>
        <CopyButton text={code} />
      </div>
    </div>
  );
}

/* ------------------------------------------------------------------ */
/* EmptyState / Spinner                                                */
/* ------------------------------------------------------------------ */

export function EmptyState({ title, hint, action }: { title: string; hint?: string; action?: ReactNode }) {
  return (
    <div style={{ textAlign: "center", padding: "48px 16px", color: "var(--ink2)" }}>
      <div style={{ fontSize: 13, fontWeight: 600, color: "var(--ink)" }}>{title}</div>
      {hint && <div style={{ fontSize: 12, marginTop: 4 }}>{hint}</div>}
      {action && <div style={{ marginTop: 12 }}>{action}</div>}
    </div>
  );
}

export function Spinner({ size = 18 }: { size?: number }) {
  return (
    <span
      role="status"
      aria-label="Loading"
      style={{
        display: "inline-block",
        width: size,
        height: size,
        border: "2px solid var(--line)",
        borderTopColor: "var(--blue)",
        borderRadius: "50%",
        animation: "ugw-spin .7s linear infinite",
      }}
    >
      <style>{"@keyframes ugw-spin{to{transform:rotate(360deg)}}"}</style>
    </span>
  );
}
