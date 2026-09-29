import type { ReactNode } from "react";
import { AlertTriangle } from "lucide-react";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { cn } from "@/lib/utils";
import { RANGES, type RangeKey } from "@/hooks";

export function PageHeader({ title, description, children }: { title: string; description?: ReactNode; children?: ReactNode }) {
  return (
    <div className="mb-5 flex flex-wrap items-end justify-between gap-4">
      <div className="min-w-0">
        <h1 className="font-mono text-xl font-bold uppercase tracking-tight">{title}</h1>
        {description && <p className="mt-1 max-w-3xl font-sans text-[13.5px] text-muted-foreground">{description}</p>}
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

// StatGrid lays out tiles with shared 1px borders, like a spreadsheet.
export function StatGrid({ children, className }: { children: ReactNode; className?: string }) {
  return <div className={cn("mb-5 grid grid-cols-2 gap-px border border-strong bg-strong lg:grid-cols-4", className)}>{children}</div>;
}

export function Stat({ label, value, hint, tone }: { label: string; value: ReactNode; hint?: ReactNode; tone?: "critical" }) {
  return (
    <div className="bg-card px-4 py-3">
      <div className="text-[10.5px] font-bold uppercase tracking-[0.12em] text-muted-foreground">{label}</div>
      <div className={cn("mt-1 text-[28px] leading-tight font-bold tracking-tight tabular-nums", tone === "critical" && "text-critical")}>{value}</div>
      {hint && <div className="mt-0.5 truncate text-[11.5px] text-muted-foreground">{hint}</div>}
    </div>
  );
}

export function ErrorBox({ error }: { error?: string }) {
  if (!error) return null;
  return (
    <div role="alert" className="mb-4 flex items-center gap-2 border border-critical bg-critical-bg px-3 py-2 text-critical">
      <AlertTriangle className="size-4 shrink-0" />
      {error}
    </div>
  );
}

export function Empty({ children }: { children: ReactNode }) {
  return <div className="px-4 py-10 text-center text-muted-foreground">{children}</div>;
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
    <div className="min-w-40 border border-strong bg-card px-3 py-2 text-[12px] shadow-[3px_3px_0_0_var(--strong)]">
      <div className="mb-1 font-bold">{label}</div>
      {payload.map((p) => (
        <div key={String(p.dataKey)} className="flex items-center justify-between gap-4">
          <span className="flex items-center gap-1.5 text-muted-foreground">
            <i className="inline-block size-2" style={{ background: p.color }} />
            {p.name}
          </span>
          <span className="tabular-nums">{format(Number(p.value))}</span>
        </div>
      ))}
    </div>
  );
}

export function Legend({ items }: { items: { label: string; color: string }[] }) {
  return (
    <div className="flex flex-wrap gap-x-4 gap-y-1 text-[11px] text-muted-foreground">
      {items.map((i) => (
        <span key={i.label} className="flex items-center gap-1.5">
          <i className="inline-block size-2.5" style={{ background: i.color }} />
          {i.label}
        </span>
      ))}
    </div>
  );
}
