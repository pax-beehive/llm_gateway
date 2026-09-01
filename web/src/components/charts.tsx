import type { CSSProperties } from "react";

/**
 * Inline-SVG charts — no chart library. All colors come from design tokens.
 * Data values are plain numbers; components handle scaling.
 */

export function BarChart({
  data,
  height = 120,
  color = "var(--blue)",
}: {
  data: Array<{ label: string; value: number }>;
  height?: number;
  color?: string;
}) {
  const max = Math.max(1, ...data.map((d) => d.value));
  const barWidth = data.length > 0 ? 100 / data.length : 100;
  return (
    <svg viewBox={`0 0 100 ${height}`} preserveAspectRatio="none" style={{ width: "100%", height }} role="img">
      {data.map((d, i) => {
        const h = (d.value / max) * (height - 4);
        return (
          <rect
            key={d.label}
            x={i * barWidth + barWidth * 0.15}
            y={height - h}
            width={barWidth * 0.7}
            height={h}
            rx={1.5}
            fill={color}
            opacity={0.9}
          >
            <title>{`${d.label}: ${d.value}`}</title>
          </rect>
        );
      })}
    </svg>
  );
}

function linePath(points: Array<{ x: number; y: number }>): string {
  return points.map((p, i) => `${i === 0 ? "M" : "L"}${p.x.toFixed(2)} ${p.y.toFixed(2)}`).join(" ");
}

export function LineChart({
  data,
  height = 120,
  color = "var(--blue)",
  fill = true,
}: {
  data: number[];
  height?: number;
  color?: string;
  /** Fill the area under the line with a translucent tint. */
  fill?: boolean;
}) {
  const width = 100;
  const max = Math.max(1, ...data);
  const min = Math.min(0, ...data);
  const span = max - min || 1;
  const points = data.map((v, i) => ({
    x: data.length > 1 ? (i / (data.length - 1)) * width : width / 2,
    y: height - 4 - ((v - min) / span) * (height - 8),
  }));
  const stroke: CSSProperties = { fill: "none", stroke: color, strokeWidth: 1.5, vectorEffect: "non-scaling-stroke" };
  return (
    <svg viewBox={`0 0 ${width} ${height}`} preserveAspectRatio="none" style={{ width: "100%", height }} role="img">
      {fill && points.length > 0 && (
        <path d={`${linePath(points)} L${width} ${height} L0 ${height} Z`} fill={color} opacity={0.12} stroke="none" />
      )}
      <path d={linePath(points)} style={stroke} strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

export function SegmentedBar({
  segments,
  height = 10,
}: {
  segments: Array<{ value: number; color: string; label?: string }>;
  height?: number;
}) {
  const total = segments.reduce((sum, s) => sum + s.value, 0) || 1;
  let offset = 0;
  return (
    <svg viewBox={`0 0 100 ${height}`} preserveAspectRatio="none" style={{ width: "100%", height }} role="img">
      {segments.map((s, i) => {
        const w = (s.value / total) * 100;
        const x = offset;
        offset += w;
        if (w <= 0) return null;
        return (
          <rect key={s.label ?? i} x={x} y={0} width={w} height={height} fill={s.color} rx={i === 0 || i === segments.length - 1 ? height / 4 : 0}>
            {s.label && <title>{`${s.label}: ${s.value}`}</title>}
          </rect>
        );
      })}
    </svg>
  );
}

export function Sparkline({ data, color = "var(--blue)", height = 28 }: { data: number[]; color?: string; height?: number }) {
  return <LineChart data={data} height={height} color={color} fill={false} />;
}
