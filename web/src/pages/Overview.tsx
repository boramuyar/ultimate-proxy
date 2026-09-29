import type { ReactElement } from "react";
import { ArrowRight } from "lucide-react";
import { Area, AreaChart, Bar, BarChart, CartesianGrid, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import { api, type UsageRow } from "@/api";
import type { PageProps } from "@/App";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { ChartTooltip, Empty, ErrorBox, Legend, PageHeader, RangePicker, Stat, StatGrid } from "@/components/page";
import { fmtNumber, fmtPct, fmtUSD, hitRate } from "@/format";
import { RANGES, rangeBounds, useAsync, useDirectory } from "@/hooks";

const TOKEN_SERIES = [
  { key: "uncached", label: "Input, not cached", color: "var(--series-1)" },
  { key: "cached", label: "Input, cached", color: "var(--series-3)" },
  { key: "output", label: "Output", color: "var(--series-2)" },
];

export default function Overview({ range, setRange }: PageProps) {
  const [from, to] = rangeBounds(range);
  const key = [range, from.getTime()];
  const gran = RANGES[range].granularity;
  const totals = useAsync(() => api.usage({ from, to, groupBy: [] }), key);
  const series = useAsync(() => api.usage({ from, to, groupBy: [], granularity: gran }), key);
  const apps = useAsync(() => api.usage({ from, to, groupBy: ["tenant", "application"] }), key);
  const emails = useAsync(() => api.usage({ from, to, groupBy: ["email"] }), key);
  const insights = useAsync(() => api.insights("open"), []);

  const t = totals.data?.[0];
  const points = fillSeries(series.data ?? [], from, to, gran);
  const error = totals.error ?? series.error ?? apps.error ?? emails.error;
  const open = insights.data ?? [];
  const failRate = t ? t.failed_requests / Math.max(t.requests, 1) : 0;

  return (
    <>
      <PageHeader title="Overview" description={`Traffic through the proxy over the last ${RANGES[range].label}.`}>
        <RangePicker value={range} onChange={setRange} />
      </PageHeader>
      <ErrorBox error={error} />

      {open.length > 0 && (
        <div className="mb-5 flex flex-wrap items-center gap-3 border border-strong bg-card px-4 py-3">
          <Badge variant="critical">{open.length} open</Badge>
          <span className="min-w-0 flex-1 truncate font-sans text-[13.5px]">{open[0].title}</span>
          <Button asChild variant="outline" size="sm">
            <a href="#/insights">
              Review insights <ArrowRight />
            </a>
          </Button>
        </div>
      )}

      <StatGrid>
        <Stat label="Requests" value={fmtNumber(t?.requests ?? 0)} hint={`${fmtPct(failRate)} failed`} />
        <Stat label="Tokens" value={fmtNumber(t?.total_tokens ?? 0)} hint={t ? `${fmtNumber(t.input_tokens)} in / ${fmtNumber(t.output_tokens)} out` : "—"} />
        <Stat label="Est. cost" value={fmtUSD(t?.cost_usd ?? 0)} hint="from the prices table" />
        <Stat
          label="Cache hit rate"
          value={fmtPct(hitRate(t?.cached_input_tokens ?? 0, t?.input_tokens ?? 0))}
          hint={`${fmtNumber(t?.cached_input_tokens ?? 0)} input tokens cached`}
        />
      </StatGrid>

      <div className="mb-5 grid gap-5 lg:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle>Tokens / {gran}</CardTitle>
            <Legend items={TOKEN_SERIES.map((s) => ({ label: s.label, color: s.color }))} />
          </CardHeader>
          <CardContent>
            <Chart>
              <AreaChart data={points} margin={{ left: 0, right: 8, top: 8 }}>
                <CartesianGrid vertical={false} />
                <XAxis dataKey="label" tickLine={false} axisLine={{ stroke: "var(--strong)" }} minTickGap={28} />
                <YAxis tickFormatter={fmtNumber} tickLine={false} axisLine={false} width={44} />
                <Tooltip content={<ChartTooltip format={fmtNumber} />} cursor={{ stroke: "var(--strong)", strokeDasharray: "3 3" }} />
                {TOKEN_SERIES.map((s) => (
                  <Area
                    key={s.key}
                    type="linear"
                    dataKey={s.key}
                    name={s.label}
                    stackId="t"
                    stroke={s.color}
                    strokeWidth={2}
                    fill={s.color}
                    fillOpacity={0.14}
                    isAnimationActive={false}
                    activeDot={{ r: 4, stroke: "var(--card)", strokeWidth: 2 }}
                  />
                ))}
              </AreaChart>
            </Chart>
          </CardContent>
        </Card>
        <Card>
          <CardHeader>
            <CardTitle>Cost / {gran}</CardTitle>
            <span className="text-[11px] text-muted-foreground">USD</span>
          </CardHeader>
          <CardContent>
            <Chart>
              <BarChart data={points} margin={{ left: 0, right: 8, top: 8 }}>
                <CartesianGrid vertical={false} />
                <XAxis dataKey="label" tickLine={false} axisLine={{ stroke: "var(--strong)" }} minTickGap={28} />
                <YAxis tickFormatter={(v) => fmtUSD(Number(v))} tickLine={false} axisLine={false} width={56} />
                <Tooltip content={<ChartTooltip format={fmtUSD} />} cursor={{ fill: "var(--muted)" }} />
                <Bar dataKey="cost" name="Cost" fill="var(--foreground)" maxBarSize={28} isAnimationActive={false} />
              </BarChart>
            </Chart>
          </CardContent>
        </Card>
      </div>

      <div className="grid gap-5 lg:grid-cols-2">
        <TopTable title="Top applications" rows={apps.data} kind="app" loading={apps.loading} />
        <TopTable title="Top users" rows={emails.data} kind="email" loading={emails.loading} />
      </div>
    </>
  );
}

function Chart({ children }: { children: ReactElement }) {
  return (
    <div className="h-56 w-full">
      <ResponsiveContainer>{children}</ResponsiveContainer>
    </div>
  );
}

function TopTable({ title, rows, kind, loading }: { title: string; rows?: UsageRow[]; kind: "app" | "email"; loading: boolean }) {
  const dir = useDirectory();
  const top = (rows ?? []).slice(0, 8);
  const max = Math.max(...top.map((r) => r.total_tokens), 1);
  return (
    <Card>
      <CardHeader>
        <CardTitle>{title}</CardTitle>
        <a href="#/usage" className="text-[11px] uppercase tracking-wider text-muted-foreground underline hover:text-foreground">
          All usage
        </a>
      </CardHeader>
      {top.length === 0 ? (
        <Empty>{loading ? "Loading…" : "No traffic in this range."}</Empty>
      ) : (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>{kind === "app" ? "Application" : "Email"}</TableHead>
              <TableHead className="text-right">Req</TableHead>
              <TableHead className="w-[30%]">Tokens</TableHead>
              <TableHead className="text-right">Cost</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {top.map((r, i) => (
              <TableRow key={i}>
                <TableCell className="max-w-56 truncate">
                  {kind === "app" ? (
                    <>
                      {r.group.application_name || dir.appName(r.group.application)}
                      <span className="text-muted-foreground"> / {r.group.tenant_name || dir.tenantName(r.group.tenant)}</span>
                    </>
                  ) : (
                    r.group.email || <span className="text-muted-foreground">(not attributed)</span>
                  )}
                </TableCell>
                <TableCell className="text-right">{fmtNumber(r.requests)}</TableCell>
                <TableCell>
                  <div className="flex items-center gap-2">
                    <div className="h-2 flex-1 bg-muted">
                      <div className="h-full bg-foreground" style={{ width: `${(100 * r.total_tokens) / max}%` }} />
                    </div>
                    <span className="w-12 text-right">{fmtNumber(r.total_tokens)}</span>
                  </div>
                </TableCell>
                <TableCell className="text-right">{fmtUSD(r.cost_usd)}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </Card>
  );
}

// fillSeries turns sparse buckets into one point per hour or day, so quiet
// periods show as zero instead of being skipped.
function fillSeries(rows: UsageRow[], from: Date, to: Date, gran: "hour" | "day") {
  const step = gran === "hour" ? 3600e3 : 86400e3;
  const byBucket = new Map(rows.map((r) => [new Date(r.bucket!).getTime(), r]));
  const start = Math.floor(from.getTime() / step) * step;
  const out = [];
  for (let ts = start; ts < to.getTime(); ts += step) {
    const r = byBucket.get(ts);
    const d = new Date(ts);
    out.push({
      label: gran === "hour" ? d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit", hour12: false }) : d.toLocaleDateString(undefined, { month: "short", day: "2-digit" }),
      uncached: r ? r.input_tokens - r.cached_input_tokens : 0,
      cached: r?.cached_input_tokens ?? 0,
      output: r?.output_tokens ?? 0,
      cost: r?.cost_usd ?? 0,
    });
  }
  return out;
}
