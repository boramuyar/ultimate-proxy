import { useState } from "react";
import { Download } from "lucide-react";
import { api, type Dimension, type UsageParams, type UsageRow } from "@/api";
import type { PageProps } from "@/App";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardHeader, CardTitle } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { Empty, ErrorBox, PageHeader, RangePicker } from "@/components/page";
import { cacheLabel, fmtNumber, fmtPct, fmtUSD, hitRate } from "@/format";
import { RANGES, rangeBounds, useAsync, useDirectory } from "@/hooks";

const DIMS: { key: Dimension; label: string }[] = [
  { key: "tenant", label: "Tenant" },
  { key: "application", label: "App" },
  { key: "email", label: "Email" },
  { key: "model", label: "Model" },
  { key: "provider", label: "Provider" },
  { key: "cache", label: "Cache" },
];

type Filters = NonNullable<UsageParams["filters"]>;
const ALL = "__all";

export default function Usage({ range, setRange }: PageProps) {
  const dir = useDirectory();
  const [groupBy, setGroupBy] = useState<Dimension[]>(["tenant", "application", "email"]);
  const [byTime, setByTime] = useState(false);
  const [filters, setFilters] = useState<Filters>({});
  const [from, to] = rangeBounds(range);
  const gran = byTime ? RANGES[range].granularity : undefined;
  const rows = useAsync(() => api.usage({ from, to, groupBy, granularity: gran, filters }), [range, from.getTime(), groupBy.join(), gran, JSON.stringify(filters)]);

  const setFilter = (k: keyof Filters, v: string) => setFilters((f) => ({ ...f, [k]: v && v !== ALL ? v : undefined }));
  // Keep the chosen dimensions in a stable column order.
  const cols = DIMS.filter((d) => groupBy.includes(d.key));

  const label = (r: UsageRow, d: Dimension) => {
    const v = r.group[d] ?? "";
    switch (d) {
      case "tenant":
        return r.group.tenant_name || dir.tenantName(v);
      case "application":
        return r.group.application_name || dir.appName(v);
      case "cache":
        return cacheLabel(v);
      default:
        return v || <span className="text-muted-foreground">—</span>;
    }
  };

  function exportCSV() {
    const head = [...(gran ? ["bucket"] : []), ...cols.map((c) => c.key), "requests", "failed", "input_tokens", "cached_input_tokens", "output_tokens", "reasoning_tokens", "cost_usd"];
    const lines = (rows.data ?? []).map((r) =>
      [
        ...(gran ? [r.bucket ?? ""] : []),
        ...cols.map((c) => (c.key === "tenant" ? r.group.tenant_name : c.key === "application" ? r.group.application_name : r.group[c.key]) ?? ""),
        r.requests,
        r.failed_requests,
        r.input_tokens,
        r.cached_input_tokens,
        r.output_tokens,
        r.reasoning_tokens,
        r.cost_usd,
      ]
        .map((v) => `"${String(v).replaceAll('"', '""')}"`)
        .join(","),
    );
    const blob = new Blob([[head.join(","), ...lines].join("\n")], { type: "text/csv" });
    const a = document.createElement("a");
    a.href = URL.createObjectURL(blob);
    a.download = `usage-${range}.csv`;
    a.click();
    URL.revokeObjectURL(a.href);
  }

  return (
    <>
      <PageHeader title="Usage" description="Who used how many tokens, and what it cost.">
        <RangePicker value={range} onChange={setRange} />
      </PageHeader>

      <Card className="mb-6">
        <div className="grid gap-4 p-5">
          <div className="flex flex-wrap items-center gap-3">
            <Label className="w-20">Group by</Label>
            <ToggleGroup type="multiple" value={groupBy} onValueChange={(v) => setGroupBy(v as Dimension[])} aria-label="Group by">
              {DIMS.map((d) => (
                <ToggleGroupItem key={d.key} value={d.key}>
                  {d.label}
                </ToggleGroupItem>
              ))}
            </ToggleGroup>
            <Label className="ml-2 cursor-pointer text-foreground">
              <Checkbox checked={byTime} onCheckedChange={(v) => setByTime(v === true)} />
              Per {RANGES[range].granularity}
            </Label>
          </div>
          <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-[1fr_1fr_1fr_1fr_auto] lg:items-end">
            <div className="grid gap-1.5">
              <Label>Tenant</Label>
              <Select value={filters.tenant_id ?? ALL} onValueChange={(v) => setFilter("tenant_id", v)}>
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value={ALL}>All tenants</SelectItem>
                  {dir.tenants.map((t) => (
                    <SelectItem key={t.id} value={t.id}>
                      {t.name}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div className="grid gap-1.5">
              <Label>Application</Label>
              <Select value={filters.application_id ?? ALL} onValueChange={(v) => setFilter("application_id", v)}>
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value={ALL}>All applications</SelectItem>
                  {dir.apps
                    .filter((a) => !filters.tenant_id || a.tenant_id === filters.tenant_id)
                    .map((a) => (
                      <SelectItem key={a.id} value={a.id}>
                        {a.name} / {dir.tenantName(a.tenant_id)}
                      </SelectItem>
                    ))}
                </SelectContent>
              </Select>
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="f-email">Email</Label>
              <Input id="f-email" placeholder="alice@acme.com" value={filters.email ?? ""} onChange={(e) => setFilter("email", e.target.value)} />
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="f-model">Model</Label>
              <Input id="f-model" placeholder="any" value={filters.model ?? ""} onChange={(e) => setFilter("model", e.target.value)} />
            </div>
            <Button variant="outline" onClick={exportCSV} disabled={!rows.data?.length}>
              <Download /> CSV
            </Button>
          </div>
        </div>
      </Card>

      <ErrorBox error={rows.error} />
      <Card>
        <CardHeader>
          <CardTitle>Results</CardTitle>
          <span className="text-[13px] text-muted-foreground">{rows.data ? `${rows.data.length} rows` : ""}</span>
        </CardHeader>
        {!rows.data?.length ? (
          <Empty>{rows.loading ? "Loading…" : "No usage matches."}</Empty>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                {gran && <TableHead>{gran}</TableHead>}
                {cols.map((c) => (
                  <TableHead key={c.key}>{c.label}</TableHead>
                ))}
                <TableHead className="text-right">Requests</TableHead>
                <TableHead className="text-right">Failed</TableHead>
                <TableHead className="text-right">Input</TableHead>
                <TableHead className="text-right">Cached</TableHead>
                <TableHead className="text-right">Output</TableHead>
                <TableHead className="text-right">Total</TableHead>
                <TableHead className="text-right">Cost</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.data.map((r, i) => (
                <TableRow key={i}>
                  {gran && (
                    <TableCell>
                      {new Date(r.bucket!).toLocaleString(undefined, gran === "hour" ? { month: "short", day: "2-digit", hour: "2-digit", hour12: false } : { month: "short", day: "2-digit" })}
                    </TableCell>
                  )}
                  {cols.map((c) => (
                    <TableCell key={c.key}>{label(r, c.key)}</TableCell>
                  ))}
                  <TableCell className="text-right">{fmtNumber(r.requests)}</TableCell>
                  <TableCell className="text-right">{r.failed_requests ? <Badge variant="critical">{fmtNumber(r.failed_requests)}</Badge> : <span className="text-muted-foreground">0</span>}</TableCell>
                  <TableCell className="text-right">{fmtNumber(r.input_tokens)}</TableCell>
                  <TableCell className="text-right">
                    {fmtNumber(r.cached_input_tokens)} <span className="text-muted-foreground">{fmtPct(hitRate(r.cached_input_tokens, r.input_tokens)).padStart(4, " ")}</span>
                  </TableCell>
                  <TableCell className="text-right">{fmtNumber(r.output_tokens)}</TableCell>
                  <TableCell className="text-right font-medium">{fmtNumber(r.total_tokens)}</TableCell>
                  <TableCell className="text-right">{fmtUSD(r.cost_usd)}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </Card>
    </>
  );
}
