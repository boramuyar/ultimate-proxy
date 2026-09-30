import { useState } from "react";
import { AlertOctagon, AlertTriangle, CheckCircle2 } from "lucide-react";
import { api, type Insight } from "@/api";
import type { PageProps } from "@/App";
import { Badge } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { Empty, ErrorBox, PageHeader } from "@/components/page";
import { cacheLabel, fmtAgo, fmtNumber, fmtPct, fmtTime, fmtUSD } from "@/format";
import { useAsync, useDirectory } from "@/hooks";

const KIND_LABEL: Record<string, string> = {
  cache_prefix_unstable: "Unstable prompt prefix",
  cache_unexpected_miss: "Unexpected cache misses",
  error_rate: "Error rate",
  truncation: "Truncated responses",
  budget_threshold: "Budget",
};

type Status = "open" | "resolved" | "all";

export default function Insights(_: PageProps) {
  const [status, setStatus] = useState<Status>("open");
  const list = useAsync(() => api.insights(status), [status]);

  return (
    <>
      <PageHeader title="Insights" description="Problems the proxy noticed in live traffic. Each opens when a rate crosses its threshold and resolves when it recovers.">
        <ToggleGroup type="single" value={status} onValueChange={(v) => v && setStatus(v as Status)} aria-label="Status">
          <ToggleGroupItem value="open">Open</ToggleGroupItem>
          <ToggleGroupItem value="resolved">Resolved</ToggleGroupItem>
          <ToggleGroupItem value="all">All</ToggleGroupItem>
        </ToggleGroup>
      </PageHeader>
      <ErrorBox error={list.error} />
      {!list.data?.length ? (
        <Card>
          <Empty>{list.loading ? "Loading…" : status === "open" ? "Nothing needs attention right now." : "No insights yet."}</Empty>
        </Card>
      ) : (
        <div className="grid gap-4">
          {list.data.map((i) => (
            <InsightCard key={i.id} insight={i} />
          ))}
        </div>
      )}
    </>
  );
}

function InsightCard({ insight: i }: { insight: Insight }) {
  const dir = useDirectory();
  const resolved = i.status === "resolved";
  const tone = resolved ? "good" : i.severity === "critical" ? "critical" : "warning";
  const Icon = resolved ? CheckCircle2 : i.severity === "critical" ? AlertOctagon : AlertTriangle;
  const facts: [string, string][] = [
    ["application", dir.appName(i.application_id)],
    ["tenant", dir.tenantName(i.tenant_id)],
    ["model", i.model],
    ["since", fmtTime(i.first_seen)],
    ...evidence(i.evidence),
  ].filter(([, v]) => v !== "") as [string, string][];

  return (
    <Card>
      <div className="flex flex-wrap items-center gap-2 px-5 pt-4">
        <Badge variant={tone}>
          <Icon /> {resolved ? "Resolved" : i.severity === "critical" ? "Critical" : "Warning"}
        </Badge>
        <Badge variant="muted">{KIND_LABEL[i.kind] ?? i.kind}</Badge>
        <span className="ml-auto text-[13px] text-faint" title={fmtTime(i.last_seen)}>
          {resolved && i.resolved_at ? `resolved ${fmtAgo(i.resolved_at)}` : `last seen ${fmtAgo(i.last_seen)}`}
        </span>
      </div>
      <div className="grid gap-1.5 px-5 pt-3 pb-4">
        <h3 className="text-base font-semibold tracking-tight">{i.title}</h3>
        <p className="max-w-4xl leading-relaxed text-muted-foreground">{i.detail}</p>
      </div>
      <dl className="grid grid-cols-2 gap-x-6 gap-y-3 border-t bg-[#fafafa] px-5 py-4 sm:grid-cols-3 lg:grid-cols-6">
        {facts.map(([k, v]) => (
          <div key={k} className="min-w-0">
            <dt className="text-xs text-muted-foreground">{k[0].toUpperCase() + k.slice(1)}</dt>
            <dd className="mt-0.5 text-[13px] break-words">
              {v}
            </dd>
          </div>
        ))}
      </dl>
    </Card>
  );
}

// evidence turns the rule's raw numbers into readable pairs.
function evidence(e: Record<string, unknown>): [string, string][] {
  const out: [string, string][] = [];
  const num = (k: string) => (typeof e[k] === "number" ? (e[k] as number) : undefined);
  if (e.window) out.push(["window", String(e.window)]);
  if (num("share") !== undefined) {
    const used = e.kind === "budget_usd" ? fmtUSD(num("used")!) : fmtNumber(num("used")!);
    const amount = e.kind === "budget_usd" ? fmtUSD(num("amount")!) : fmtNumber(num("amount")!);
    out.push(["used", `${used} of ${amount} (${fmtPct(num("share")!)})`]);
  }
  if (e.user) out.push(["user", String(e.user)]);
  if (e.enforcement) out.push(["enforcement", String(e.enforcement)]);
  if (typeof e.resets_at === "string") out.push(["resets", fmtTime(e.resets_at)]);
  if (num("requests") !== undefined) out.push(["requests", fmtNumber(num("requests")!)]);
  if (num("rate") !== undefined) out.push(["rate", fmtPct(num("rate")!)]);
  if (num("missed_cached_tokens")) out.push(["tokens not cached", fmtNumber(num("missed_cached_tokens")!)]);
  if (num("missed_savings_usd")) out.push(["missed savings", fmtUSD(num("missed_savings_usd")!)]);
  if (e.reasons && typeof e.reasons === "object") {
    const parts = Object.entries(e.reasons as Record<string, number>)
      .sort((a, b) => b[1] - a[1])
      .map(([s, n]) => `${cacheLabel(s)} ×${n}`);
    if (parts.length) out.push(["causes", parts.join(", ")]);
  }
  if (e.error_codes && typeof e.error_codes === "object") {
    const parts = Object.entries(e.error_codes as Record<string, number>)
      .sort((a, b) => b[1] - a[1])
      .map(([c, n]) => `${c || "unknown"} ×${n}`);
    if (parts.length) out.push(["errors", parts.join(", ")]);
  }
  return out;
}
