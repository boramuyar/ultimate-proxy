import { api, type UsageRow } from "../api";
import type { PageProps } from "../App";
import { Empty, ErrorBox, PageHead, RangePicker } from "../components/ui";
import { CACHE_STATUS, fmtNumber, fmtPct, hitRate } from "../format";
import { RANGES, rangeBounds, useAsync, useDirectory } from "../hooks";

// Display order and colour for each status.
const ORDER: { status: string; color: string }[] = [
  { status: "hit", color: "var(--good)" },
  { status: "miss_unexpected", color: "var(--bad)" },
  { status: "miss_instructions_dynamic", color: "var(--chart-4)" },
  { status: "miss_tools_reordered", color: "#a855f7" },
  { status: "miss_instructions_changed", color: "var(--warn)" },
  { status: "miss_tools_changed", color: "#fb923c" },
  { status: "miss_history_rewritten", color: "#eab308" },
  { status: "miss_new_prefix", color: "var(--chart-2)" },
  { status: "miss_too_short", color: "var(--faint)" },
  { status: "unknown", color: "var(--border)" },
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
  const present = ORDER.filter((o) => apps.some((a) => a.counts[o.status]));

  return (
    <>
      <PageHead title="Prompt cache" sub={`Whether each application's prompts hit the provider's cache over the last ${RANGES[range].label}, and why not.`}>
        <RangePicker value={range} onChange={setRange} />
      </PageHead>
      <ErrorBox error={rows.error} />

      <div className="panel" style={{ marginBottom: 16 }}>
        <h2>By application</h2>
        <p className="sub">Hit rate counts input tokens served from cache. Bars split requests by cache status.</p>
        {apps.length === 0 ? (
          <Empty>{rows.loading ? "Loading…" : "No classified requests in this range."}</Empty>
        ) : (
          <>
            {apps.map((a) => (
              <div key={a.key} style={{ padding: "10px 0", borderTop: "1px solid var(--border)" }}>
                <div style={{ display: "flex", justifyContent: "space-between", gap: 12, marginBottom: 6, flexWrap: "wrap" }}>
                  <span>
                    <b>{a.app}</b> <span className="faint">· {a.tenant}</span>
                  </span>
                  <span className="muted">
                    {fmtNumber(a.requests)} requests · <b style={{ color: "var(--text)" }}>{fmtPct(hitRate(a.cached, a.input))}</b> tokens cached
                  </span>
                </div>
                <div className="bar" role="img" aria-label={ORDER.filter((o) => a.counts[o.status]).map((o) => `${CACHE_STATUS[o.status].label}: ${a.counts[o.status]}`).join(", ")}>
                  {ORDER.map((o) =>
                    a.counts[o.status] ? (
                      <span key={o.status} title={`${CACHE_STATUS[o.status].label}: ${a.counts[o.status]}`} style={{ width: `${(100 * a.counts[o.status]) / a.requests}%`, background: o.color }} />
                    ) : null,
                  )}
                </div>
              </div>
            ))}
            <div className="legend" style={{ marginTop: 12 }}>
              {present.map((o) => (
                <span key={o.status}>
                  <i style={{ background: o.color }} />
                  {CACHE_STATUS[o.status].label}
                </span>
              ))}
            </div>
          </>
        )}
      </div>

      <div className="panel">
        <h2>What the statuses mean</h2>
        <div className="table-wrap">
          <table>
            <tbody>
              {ORDER.map((o) => (
                <tr key={o.status}>
                  <td style={{ width: 1 }}>
                    <span className={`badge ${CACHE_STATUS[o.status].tone}`}>{CACHE_STATUS[o.status].label}</span>
                  </td>
                  <td style={{ whiteSpace: "normal" }} className="muted">
                    {CACHE_STATUS[o.status].help}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </div>
    </>
  );
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
