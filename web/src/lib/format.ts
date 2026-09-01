/** Formatting helpers shared by all pages. Keep them pure and small. */

/** USD micros (1e-6 dollars) → "$1,234.56". */
export function formatMicrosUSD(micros: number): string {
  const dollars = micros / 1_000_000;
  return dollars.toLocaleString("en-US", {
    style: "currency",
    currency: "USD",
    minimumFractionDigits: 2,
    maximumFractionDigits: dollars !== 0 && Math.abs(dollars) < 0.01 ? 6 : 2,
  });
}

/** 48203 → "48,203". */
export function formatNumber(n: number): string {
  return n.toLocaleString("en-US");
}

/** ISO timestamp → "41s ago" / "3m ago" / "2h ago" / "5d ago" / absolute date. */
export function timeAgo(iso: string): string {
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return iso;
  const seconds = Math.max(0, Math.floor((Date.now() - then) / 1000));
  if (seconds < 60) return `${seconds}s ago`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.floor(hours / 24);
  if (days < 30) return `${days}d ago`;
  return formatDateTime(iso);
}

/** ISO timestamp → "Aug 30, 2026 14:05". */
export function formatDateTime(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleString("en-US", {
    month: "short",
    day: "numeric",
    year: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
  });
}

/** "tn_9f3kQm2W8Lp4" → "tn_9f3k…8Lp4". */
export function truncateId(id: string, head = 8, tail = 4): string {
  if (id.length <= head + tail + 1) return id;
  return `${id.slice(0, head)}…${id.slice(-tail)}`;
}
