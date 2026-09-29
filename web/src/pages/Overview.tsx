import { Area, AreaChart, Bar, BarChart, CartesianGrid, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import { api, type UsageRow } from "../api";
import type { PageProps } from "../App";
import { Empty, ErrorBox, PageHead, RangePicker, Stat } from "../components/ui";
import { fmtNumber, fmtPct, fmtUSD, hitRate } from "../format";
import { RANGES, rangeBounds, useAsync, useDirectory } from "../hooks";

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

  return (
    <>
      <PageHead title="Overview" sub={`Traffic through the proxy over the last ${RANGES[range].label}.`}>
        <RangePicker value={range} onChange={setRange} />
      </PageHead>
      <ErrorBox error={error} />

      {(insights.data?.length ?? 0) > 0 && (
        <div className="notice">
          <span>
            <b>{insights.data!.length}</b> open {insights.data!.length === 1 ? "insight needs" : "insights need"} attention, starting with “
            {insights.data![0].title}”.
          </span>
          <a className="btn small" href="#/insights">
            Review
          </a>
        </div>
      )}

      <div className="grid stats">
        <Stat label="Requests" value={fmtNumber(t?.requests ?? 0)} hint={t ? `${fmtPct(t.failed_requests / Math.max(t.requests, 1))} failed` : " "} />
        <Stat
          label="Tokens"
          value={fmtNumber(t?.total_tokens ?? 0)}
          hint={t ? `${fmtNumber(t.input_tokens)} in · ${fmtNumber(t.output_tokens)} out` : " "}
        />
        <Stat label="Estimated cost" value={fmtUSD(t?.cost_usd ?? 0)} hint="From the prices table" />
        <Stat
          label="Prompt cache hit rate"
          value={fmtPct(hitRate(t?.cached_input_tokens ?? 0, t?.input_tokens ?? 0))}
          hint={t ? `${fmtNumber(t.cached_input_tokens)} input tokens cached` : " "}
        />
      </div>

      <div className="grid two">
        <div className="panel">
          <h2>Tokens</h2>
          <Chart>
            <AreaChart data={points} margin={{ left: 0, right: 8, top: 4 }}>
              <CartesianGrid vertical={false} />
              <XAxis dataKey="label" tickLine={false} axisLine={false} minTickGap={24} />
              <YAxis tickFormatter={fmtNumber} tickLine={false} axisLine={false} width={48} />
              <Tooltip formatter={(v) => fmtNumber(Number(v))} />
              <Area type="monotone" dataKey="uncached" name="Input (not cached)" stackId="t" stroke="var(--chart-1)" fill="var(--chart-1)" fillOpacity={0.25} />
              <Area type="monotone" dataKey="cached" name="Input (cached)" stackId="t" stroke="var(--chart-2)" fill="var(--chart-2)" fillOpacity={0.25} />
              <Area type="monotone" dataKey="output" name="Output" stackId="t" stroke="var(--chart-3)" fill="var(--chart-3)" fillOpacity={0.25} />
            </AreaChart>
          </Chart>
        </div>
        <div className="panel">
          <h2>Cost</h2>
          <Chart>
            <BarChart data={points} margin={{ left: 0, right: 8, top: 4 }}>
              <CartesianGrid vertical={false} />
              <XAxis dataKey="label" tickLine={false} axisLine={false} minTickGap={24} />
              <YAxis tickFormatter={(v) => fmtUSD(Number(v))} tickLine={false} axisLine={false} width={56} />
              <Tooltip formatter={(v) => fmtUSD(Number(v))} cursor={{ fill: "var(--panel-2)" }} />
              <Bar dataKey="cost" name="Cost" fill="var(--chart-1)" radius={[4, 4, 0, 0]} />
            </BarChart>
          </Chart>
        </div>
      </div>

      <div className="grid two">
        <TopTable title="Top applications" rows={apps.data} kind="app" />
        <TopTable title="Top users" rows={emails.data} kind="email" />
      </div>
    </>
  );
}

function Chart({ children }: { children: React.ReactElement }) {
  return (
    <div style={{ width: "100%", height: 220 }}>
      <ResponsiveContainer>{children}</ResponsiveContainer>
    </div>
  );
}

function TopTable({ title, rows, kind }: { title: string; rows?: UsageRow[]; kind: "app" | "email" }) {
  const dir = useDirectory();
  const top = (rows ?? []).slice(0, 8);
  return (
    <div className="panel">
      <h2>{title}</h2>
      {top.length === 0 ? (
        <Empty>No traffic in this range.</Empty>
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>{kind === "app" ? "Application" : "Email"}</th>
                <th className="num">Requests</th>
                <th className="num">Tokens</th>
                <th className="num">Cost</th>
              </tr>
            </thead>
            <tbody>
              {top.map((r, i) => (
                <tr key={i}>
                  <td>
                    {kind === "app" ? (
                      <>
                        {r.group.application_name || dir.appName(r.group.application)}{" "}
                        <span className="faint">· {r.group.tenant_name || dir.tenantName(r.group.tenant)}</span>
                      </>
                    ) : (
                      r.group.email || <span className="faint">not attributed</span>
                    )}
                  </td>
                  <td className="num">{fmtNumber(r.requests)}</td>
                  <td className="num">{fmtNumber(r.total_tokens)}</td>
                  <td className="num">{fmtUSD(r.cost_usd)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
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
      label: gran === "hour" ? d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" }) : d.toLocaleDateString(undefined, { month: "short", day: "numeric" }),
      uncached: r ? r.input_tokens - r.cached_input_tokens : 0,
      cached: r?.cached_input_tokens ?? 0,
      output: r?.output_tokens ?? 0,
      cost: r?.cost_usd ?? 0,
    });
  }
  return out;
}
