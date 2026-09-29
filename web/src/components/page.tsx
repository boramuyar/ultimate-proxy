import type { ReactNode } from "react";
import { AlertCircle } from "lucide-react";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { cn } from "@/lib/utils";
import { RANGES, type RangeKey } from "@/hooks";

export function PageHeader({ title, description, children }: { title: string; description?: ReactNode; children?: ReactNode }) {
  return (
    <div className="mb-8 flex flex-wrap items-end justify-between gap-4">
      <div className="min-w-0">
        <h1 className="text-[28px] leading-9 font-semibold tracking-tight">{title}</h1>
        {description && <p className="mt-1 max-w-3xl text-muted-foreground">{description}</p>}
      </div>
      {children}
    </div>
  );
}

export function RangePicker({ value, onChange }: { value: RangeKey; onChange: (v: RangeKey) => void }) {
  return (
    <ToggleGroup type="single" value={value} onValueChange={(v) => v && onChange(v as RangeKey)} aria-label="Time range">
      {(Object.keys(RANGES) as RangeKey[]).map((k) => (
        <ToggleGroupItem key={k} value={k}>
          {k}
        </ToggleGroupItem>
      ))}
    </ToggleGroup>
  );
}

// StatGrid is one card split into metric tiles by hairline dividers.
export function StatGrid({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <div className={cn("mb-6 grid grid-cols-2 overflow-hidden rounded-lg border bg-card lg:grid-cols-4 [&>*]:border-border max-lg:[&>*:nth-child(-n+2)]:border-b max-lg:[&>*:nth-child(odd)]:border-r lg:[&>*:not(:last-child)]:border-r", className)}>
      {children}
    </div>
  );
}

export function Stat({ label, value, hint, tone }: { label: string; value: ReactNode; hint?: ReactNode; tone?: "critical" }) {
  return (
    <div className="px-5 py-4">
      <div className="text-[13px] text-muted-foreground">{label}</div>
      <div className={cn("mt-1.5 text-[26px] leading-8 font-semibold tracking-tight tabular-nums", tone === "critical" && "text-critical")}>{value}</div>
      {hint && <div className="mt-1 truncate text-[13px] text-faint">{hint}</div>}
    </div>
  );
}

export function ErrorBox({ error }: { error?: string }) {
  if (!error) return null;
  return (
    <div role="alert" className="mb-4 flex items-center gap-2 rounded-md border border-[#ffd1d1] bg-critical-bg px-3 py-2 text-[13px] text-critical">
      <AlertCircle className="size-4 shrink-0" />
      {error}
    </div>
  );
}

export function Empty({ children }: { children: ReactNode }) {
  return <div className="px-5 py-12 text-center text-muted-foreground">{children}</div>;
}

// ChartTooltip renders recharts' tooltip payload in the dashboard's style.
export function ChartTooltip({
  active,
  payload,
  label,
  format,
}: {
  active?: boolean;
  payload?: { name?: string; value?: number | string; color?: string; dataKey?: string | number }[];
  label?: string;
  format: (v: number) => string;
}) {
  if (!active || !payload?.length) return null;
  return (
    <div className="min-w-44 rounded-lg border bg-card px-3 py-2.5 text-[13px] shadow-lg">
      <div className="mb-1.5 text-xs text-muted-foreground">{label}</div>
      {payload.map((p) => (
        <div key={String(p.dataKey)} className="flex items-center justify-between gap-4 py-0.5">
          <span className="flex items-center gap-2">
            <i className="inline-block size-2 rounded-full" style={{ background: p.color }} />
            {p.name}
          </span>
          <span className="font-mono text-xs tabular-nums">{format(Number(p.value))}</span>
        </div>
      ))}
    </div>
  );
}

export function Legend({ items }: { items: { label: string; color: string }[] }) {
  return (
    <div className="flex flex-wrap gap-x-4 gap-y-1 text-[13px] text-muted-foreground">
      {items.map((i) => (
        <span key={i.label} className="flex items-center gap-2">
          <i className="inline-block size-2 rounded-full" style={{ background: i.color }} />
          {i.label}
        </span>
      ))}
    </div>
  );
}
