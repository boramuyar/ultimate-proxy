import { api, type UsageRow } from "@/api";
import type { PageProps } from "@/App";
import { Badge } from "@/components/ui/badge";
import { Card, CardHeader, CardTitle } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Empty, ErrorBox, Legend, PageHeader, RangePicker } from "@/components/page";
import { CACHE_STATUS, fmtNumber, fmtPct, hitRate } from "@/format";
import { RANGES, rangeBounds, useAsync, useDirectory } from "@/hooks";
import { cn } from "@/lib/utils";

// Status groups for the bar: what worked, what the app can fix, what the
// provider missed, and what is expected. Colour follows the group; the table
// below names every individual status.
const GROUPS = [
  { key: "hit", label: "Hit", color: "var(--good)", statuses: ["hit"] },
  {
    key: "fixable",
    label: "App changes its prompt",
    color: "var(--series-1)",
    statuses: ["miss_instructions_dynamic", "miss_instructions_changed", "miss_tools_reordered", "miss_tools_changed", "miss_history_rewritten"],
  },
  { key: "provider", label: "Provider missed", color: "var(--critical)", statuses: ["miss_unexpected"] },
  { key: "expected", label: "Expected miss", color: "var(--input)", statuses: ["miss_new_prefix", "miss_too_short", "unknown"] },
];

interface AppCache {
  key: string;
  app: string;
  tenant: string;
  requests: number;
  counts: Record<string, number>;
  input: number;
  cached: number;
}

export default function Cache({ range, setRange }: PageProps) {
  const dir = useDirectory();
  const [from, to] = rangeBounds(range);
  const rows = useAsync(() => api.usage({ from, to, groupBy: ["tenant", "application", "cache"] }), [range, from.getTime()]);
  const apps = summarize(rows.data ?? [], dir);

  return (
    <>
      <PageHeader title="Prompt cache" description={`Whether each application's prompts hit the provider's cache over the last ${RANGES[range].label}, and why not.`}>
        <RangePicker value={range} onChange={setRange} />
      </PageHeader>
      <ErrorBox error={rows.error} />

      <Card className="mb-5">
        <CardHeader>
          <CardTitle>By application</CardTitle>
          <Legend items={GROUPS.map((g) => ({ label: g.label, color: g.color }))} />
        </CardHeader>
        {apps.length === 0 ? (
          <Empty>{rows.loading ? "Loading…" : "No classified requests in this range."}</Empty>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Application</TableHead>
                <TableHead className="text-right">Requests</TableHead>
                <TableHead className="text-right">Tokens cached</TableHead>
                <TableHead className="w-[45%]">Requests by cache status</TableHead>
                <TableHead>Top miss reason</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {apps.map((a) => {
                const rate = hitRate(a.cached, a.input);
                const top = topMiss(a);
                return (
                  <TableRow key={a.key}>
                    <TableCell>
                      <b>{a.app}</b>
                      <span className="text-muted-foreground"> / {a.tenant}</span>
                    </TableCell>
                    <TableCell className="text-right">{fmtNumber(a.requests)}</TableCell>
                    <TableCell className={cn("text-right font-bold", rate < 0.2 && "text-critical")}>{fmtPct(rate)}</TableCell>
                    <TableCell>
                      <div
                        className="flex h-3 gap-px bg-card"
                        role="img"
                        aria-label={GROUPS.map((g) => `${g.label}: ${groupCount(a, g.statuses)}`).join(", ")}
                      >
                        {GROUPS.map((g) => {
                          const n = groupCount(a, g.statuses);
                          return n ? <span key={g.key} title={`${g.label}: ${n}`} style={{ width: `${(100 * n) / a.requests}%`, background: g.color }} /> : null;
                        })}
                      </div>
                    </TableCell>
                    <TableCell>
                      {top ? <Badge variant={CACHE_STATUS[top].tone === "bad" ? "critical" : CACHE_STATUS[top].tone === "warn" ? "warning" : "muted"}>{CACHE_STATUS[top].label}</Badge> : <span className="text-muted-foreground">—</span>}
                    </TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
        )}
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>What the statuses mean</CardTitle>
        </CardHeader>
        <Table>
          <TableBody>
            {Object.entries(CACHE_STATUS)
              .filter(([s]) => s !== "")
              .map(([s, info]) => (
                <TableRow key={s}>
                  <TableCell className="w-56">
                    <Badge variant={info.tone === "good" ? "good" : info.tone === "bad" ? "critical" : info.tone === "warn" ? "warning" : "muted"}>{info.label}</Badge>
                  </TableCell>
                  <TableCell className="w-56 text-muted-foreground">{s}</TableCell>
                  <TableCell className="font-sans text-[13px] whitespace-normal">{info.help}</TableCell>
                </TableRow>
              ))}
          </TableBody>
        </Table>
      </Card>
    </>
  );
}

const groupCount = (a: AppCache, statuses: string[]) => statuses.reduce((n, s) => n + (a.counts[s] ?? 0), 0);

// topMiss is the most common miss the application or provider can act on.
function topMiss(a: AppCache): string | undefined {
  const actionable = [...GROUPS[1].statuses, ...GROUPS[2].statuses];
  let best: string | undefined;
  for (const s of actionable) if ((a.counts[s] ?? 0) > (best ? a.counts[best] : 0)) best = s;
  return best;
}

function summarize(rows: UsageRow[], dir: ReturnType<typeof useDirectory>): AppCache[] {
  const byApp = new Map<string, AppCache>();
  for (const r of rows) {
    const status = r.group.cache ?? "";
    if (status === "") continue; // failed or unclassified
    const key = r.group.application;
    let a = byApp.get(key);
    if (!a) {
      a = {
        key,
        app: r.group.application_name || dir.appName(key),
        tenant: r.group.tenant_name || dir.tenantName(r.group.tenant),
        requests: 0,
        counts: {},
        input: 0,
        cached: 0,
      };
      byApp.set(key, a);
    }
    a.requests += r.requests;
    a.counts[status] = (a.counts[status] ?? 0) + r.requests;
    a.input += r.input_tokens;
    a.cached += r.cached_input_tokens;
  }
  // Worst hit rate first: those need attention.
  return [...byApp.values()].sort((x, y) => hitRate(x.cached, x.input) - hitRate(y.cached, y.input));
}
