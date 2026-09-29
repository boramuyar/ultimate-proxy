import { useState } from "react";
import { api, type Insight } from "../api";
import type { PageProps } from "../App";
import { Empty, ErrorBox, PageHead, Segmented } from "../components/ui";
import { cacheLabel, fmtAgo, fmtNumber, fmtPct, fmtTime, fmtUSD } from "../format";
import { useAsync, useDirectory } from "../hooks";

const KIND_LABEL: Record<string, string> = {
  cache_prefix_unstable: "Unstable prompt prefix",
  cache_unexpected_miss: "Unexpected cache misses",
  error_rate: "Error rate",
  truncation: "Truncated responses",
};

type Status = "open" | "resolved" | "all";

export default function Insights(_: PageProps) {
  const [status, setStatus] = useState<Status>("open");
  const list = useAsync(() => api.insights(status), [status]);

  return (
    <>
      <PageHead title="Insights" sub="Problems the proxy noticed in live traffic. Each one opens when a rate crosses its threshold and resolves when it recovers.">
        <Segmented
          label="Status"
          value={status}
          onChange={setStatus}
          options={[
            { value: "open", label: "Open" },
            { value: "resolved", label: "Resolved" },
            { value: "all", label: "All" },
          ]}
        />
      </PageHead>
      <ErrorBox error={list.error} />
      {!list.data?.length ? (
        <div className="panel">
          <Empty>{list.loading ? "Loading…" : status === "open" ? "Nothing needs attention right now." : "No insights yet."}</Empty>
        </div>
      ) : (
        list.data.map((i) => <InsightCard key={i.id} insight={i} />)
      )}
    </>
  );
}

function InsightCard({ insight: i }: { insight: Insight }) {
  const dir = useDirectory();
  const tone = i.status === "resolved" ? "good" : i.severity === "critical" ? "bad" : "warn";
  return (
    <div className="panel insight" style={{ marginBottom: 12 }}>
      <div className="insight-head">
        <div>
          <div style={{ display: "flex", gap: 8, alignItems: "center", marginBottom: 4, flexWrap: "wrap" }}>
            <span className={`badge ${tone}`}>{i.status === "resolved" ? "Resolved" : i.severity === "critical" ? "Critical" : "Warning"}</span>
            <span className="badge">{KIND_LABEL[i.kind] ?? i.kind}</span>
          </div>
          <h3>{i.title}</h3>
        </div>
        <span className="faint" style={{ whiteSpace: "nowrap" }} title={fmtTime(i.last_seen)}>
          {i.status === "resolved" && i.resolved_at ? `resolved ${fmtAgo(i.resolved_at)}` : `seen ${fmtAgo(i.last_seen)}`}
        </span>
      </div>
      <div>{i.detail}</div>
      <div className="kv">
        <span>
          Application <b>{dir.appName(i.application_id)}</b>
        </span>
        <span>
          Tenant <b>{dir.tenantName(i.tenant_id)}</b>
        </span>
        <span>
          Model <b>{i.model}</b>
        </span>
        <span>
          Since <b>{fmtTime(i.first_seen)}</b>
        </span>
        {evidence(i.evidence).map(([k, v]) => (
          <span key={k}>
            {k} <b>{v}</b>
          </span>
        ))}
      </div>
    </div>
  );
}

// evidence turns the rule's raw numbers into readable pairs.
function evidence(e: Record<string, unknown>): [string, string][] {
  const out: [string, string][] = [];
  const num = (k: string) => (typeof e[k] === "number" ? (e[k] as number) : undefined);
  if (e.window) out.push(["Window", String(e.window)]);
  if (num("requests") !== undefined) out.push(["Requests", fmtNumber(num("requests")!)]);
  if (num("rate") !== undefined) out.push(["Rate", fmtPct(num("rate")!)]);
  if (num("missed_cached_tokens")) out.push(["Tokens that should have been cached", fmtNumber(num("missed_cached_tokens")!)]);
  if (num("missed_savings_usd")) out.push(["Missed savings", fmtUSD(num("missed_savings_usd")!)]);
  if (e.reasons && typeof e.reasons === "object") {
    const parts = Object.entries(e.reasons as Record<string, number>)
      .sort((a, b) => b[1] - a[1])
      .map(([s, n]) => `${cacheLabel(s)} ${n}`);
    if (parts.length) out.push(["Causes", parts.join(", ")]);
  }
  if (e.error_codes && typeof e.error_codes === "object") {
    const parts = Object.entries(e.error_codes as Record<string, number>)
      .sort((a, b) => b[1] - a[1])
      .map(([c, n]) => `${c || "unknown"} ${n}`);
    if (parts.length) out.push(["Errors", parts.join(", ")]);
  }
  return out;
}
