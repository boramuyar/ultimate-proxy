import { useState } from "react";
import { api, type Dimension, type UsageParams, type UsageRow } from "../api";
import type { PageProps } from "../App";
import { Empty, ErrorBox, PageHead, RangePicker } from "../components/ui";
import { cacheLabel, fmtNumber, fmtPct, fmtUSD, hitRate } from "../format";
import { RANGES, rangeBounds, useAsync, useDirectory } from "../hooks";

const DIMS: { key: Dimension; label: string }[] = [
  { key: "tenant", label: "Tenant" },
  { key: "application", label: "Application" },
  { key: "email", label: "Email" },
  { key: "model", label: "Model" },
  { key: "provider", label: "Provider" },
  { key: "cache", label: "Cache status" },
];

type Filters = NonNullable<UsageParams["filters"]>;

export default function Usage({ range, setRange }: PageProps) {
  const dir = useDirectory();
  const [groupBy, setGroupBy] = useState<Dimension[]>(["tenant", "application", "email"]);
  const [byTime, setByTime] = useState(false);
  const [filters, setFilters] = useState<Filters>({});
  const [from, to] = rangeBounds(range);
  const gran = byTime ? RANGES[range].granularity : undefined;
  const rows = useAsync(() => api.usage({ from, to, groupBy, granularity: gran, filters }), [range, from.getTime(), groupBy.join(), gran, JSON.stringify(filters)]);

  const toggle = (d: Dimension) => setGroupBy((g) => (g.includes(d) ? g.filter((x) => x !== d) : [...g, d]));
  const setFilter = (k: keyof Filters, v: string) => setFilters((f) => ({ ...f, [k]: v || undefined }));

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
        return v || <span className="faint">none</span>;
    }
  };

  function exportCSV() {
    const head = [...(gran ? ["bucket"] : []), ...groupBy, "requests", "failed", "input_tokens", "cached_input_tokens", "output_tokens", "reasoning_tokens", "cost_usd"];
    const lines = (rows.data ?? []).map((r) =>
      [
        ...(gran ? [r.bucket ?? ""] : []),
        ...groupBy.map((d) => (d === "tenant" ? r.group.tenant_name : d === "application" ? r.group.application_name : r.group[d]) ?? ""),
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
      <PageHead title="Usage" sub="Who used how many tokens, and what it cost.">
        <RangePicker value={range} onChange={setRange} />
      </PageHead>

      <div className="panel" style={{ marginBottom: 16, display: "grid", gap: 14 }}>
        <div className="field">
          Group by
          <div className="chips">
            {DIMS.map((d) => (
              <button key={d.key} className={`chip ${groupBy.includes(d.key) ? "on" : ""}`} aria-pressed={groupBy.includes(d.key)} onClick={() => toggle(d.key)}>
                {d.label}
              </button>
            ))}
            <button className={`chip ${byTime ? "on" : ""}`} aria-pressed={byTime} onClick={() => setByTime(!byTime)}>
              Per {RANGES[range].granularity}
            </button>
          </div>
        </div>
        <div className="form-row">
          <label className="field">
            Tenant
            <select className="input" value={filters.tenant_id ?? ""} onChange={(e) => setFilter("tenant_id", e.target.value)}>
              <option value="">All</option>
              {dir.tenants.map((t) => (
                <option key={t.id} value={t.id}>
                  {t.name}
                </option>
              ))}
            </select>
          </label>
          <label className="field">
            Application
            <select className="input" value={filters.application_id ?? ""} onChange={(e) => setFilter("application_id", e.target.value)}>
              <option value="">All</option>
              {dir.apps
                .filter((a) => !filters.tenant_id || a.tenant_id === filters.tenant_id)
                .map((a) => (
                  <option key={a.id} value={a.id}>
                    {a.name} ({dir.tenantName(a.tenant_id)})
                  </option>
                ))}
            </select>
          </label>
          <label className="field">
            Email
            <input className="input" placeholder="alice@acme.com" value={filters.email ?? ""} onChange={(e) => setFilter("email", e.target.value)} />
          </label>
          <label className="field">
            Model
            <input className="input" placeholder="any" value={filters.model ?? ""} onChange={(e) => setFilter("model", e.target.value)} />
          </label>
          <span style={{ flex: 1 }} />
          <button className="btn" onClick={exportCSV} disabled={!rows.data?.length}>
            Export CSV
          </button>
        </div>
      </div>

      <ErrorBox error={rows.error} />
      <div className="panel">
        {!rows.data?.length ? (
          <Empty>{rows.loading ? "Loading…" : "No usage matches."}</Empty>
        ) : (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  {gran && <th>{gran === "hour" ? "Hour" : "Day"}</th>}
                  {groupBy.map((d) => (
                    <th key={d}>{DIMS.find((x) => x.key === d)!.label}</th>
                  ))}
                  <th className="num">Requests</th>
                  <th className="num">Failed</th>
                  <th className="num">Input</th>
                  <th className="num">Cached</th>
                  <th className="num">Output</th>
                  <th className="num">Total tokens</th>
                  <th className="num">Cost</th>
                </tr>
              </thead>
              <tbody>
                {rows.data.map((r, i) => (
                  <tr key={i}>
                    {gran && <td>{new Date(r.bucket!).toLocaleString(undefined, gran === "hour" ? { month: "short", day: "numeric", hour: "2-digit" } : { month: "short", day: "numeric" })}</td>}
                    {groupBy.map((d) => (
                      <td key={d}>{label(r, d)}</td>
                    ))}
                    <td className="num">{fmtNumber(r.requests)}</td>
                    <td className="num">{r.failed_requests ? <span className="badge bad">{fmtNumber(r.failed_requests)}</span> : "0"}</td>
                    <td className="num">{fmtNumber(r.input_tokens)}</td>
                    <td className="num">
                      {fmtNumber(r.cached_input_tokens)} <span className="faint">({fmtPct(hitRate(r.cached_input_tokens, r.input_tokens))})</span>
                    </td>
                    <td className="num">{fmtNumber(r.output_tokens)}</td>
                    <td className="num">{fmtNumber(r.total_tokens)}</td>
                    <td className="num">{fmtUSD(r.cost_usd)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </>
  );
}
