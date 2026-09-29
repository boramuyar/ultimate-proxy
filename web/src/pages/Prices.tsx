import { useState, type FormEvent } from "react";
import { api, type Price } from "../api";
import type { PageProps } from "../App";
import { Empty, ErrorBox, PageHead, Segmented } from "../components/ui";
import { fmtTime } from "../format";
import { useAsync } from "../hooks";

export default function Prices(_: PageProps) {
  const [view, setView] = useState<"current" | "history">("current");
  const list = useAsync(() => api.prices(view === "current"), [view]);

  return (
    <>
      <PageHead title="Prices" sub="USD per million tokens. Prices are never edited: a new price takes over from its start time, and past requests keep the cost they were charged.">
        <Segmented
          label="Prices shown"
          value={view}
          onChange={setView}
          options={[
            { value: "current", label: "In effect now" },
            { value: "history", label: "History" },
          ]}
        />
      </PageHead>
      <AddPrice onAdded={list.reload} />
      <ErrorBox error={list.error} />
      <div className="panel">
        {!list.data?.length ? (
          <Empty>{list.loading ? "Loading…" : "No prices yet. Requests are counted, but their cost is $0 until their model has a price."}</Empty>
        ) : (
          <PriceTable prices={view === "history" ? [...list.data].reverse() : list.data} history={view === "history"} />
        )}
      </div>
    </>
  );
}

function PriceTable({ prices, history }: { prices: Price[]; history: boolean }) {
  const money = (n: number) => `$${n.toLocaleString(undefined, { maximumFractionDigits: 4 })}`;
  const now = Date.now();
  return (
    <div className="table-wrap">
      <table>
        <thead>
          <tr>
            <th>Model</th>
            <th className="num">Input</th>
            <th className="num">Cached input</th>
            <th className="num">Cache write</th>
            <th className="num">Output</th>
            <th>Effective from</th>
            {history && <th />}
          </tr>
        </thead>
        <tbody>
          {prices.map((p) => (
            <tr key={p.id}>
              <td className="mono">{p.model}</td>
              <td className="num">{money(p.input)}</td>
              <td className="num">{money(p.cached_input)}</td>
              <td className="num">{money(p.cache_write)}</td>
              <td className="num">{money(p.output)}</td>
              <td>{new Date(p.effective_from).getFullYear() < 1900 ? "always" : fmtTime(p.effective_from)}</td>
              {history && <td>{new Date(p.effective_from).getTime() > now && <span className="badge accent">scheduled</span>}</td>}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function AddPrice({ onAdded }: { onAdded: () => void }) {
  const empty = { model: "", input: "", cached_input: "", cache_write: "", output: "", effective_from: "" };
  const [f, setF] = useState(empty);
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);
  const set = (k: keyof typeof empty) => (e: React.ChangeEvent<HTMLInputElement>) => setF({ ...f, [k]: e.target.value });
  const opt = (v: string) => (v.trim() === "" ? undefined : Number(v));

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      await api.addPrice({
        model: f.model.trim(),
        input: Number(f.input),
        output: Number(f.output),
        cached_input: opt(f.cached_input),
        cache_write: opt(f.cache_write),
        effective_from: f.effective_from ? new Date(f.effective_from).toISOString() : undefined,
      });
      setF(empty);
      onAdded();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  }

  const valid = f.model.trim() && f.input.trim() !== "" && f.output.trim() !== "";
  return (
    <form className="panel" style={{ marginBottom: 16 }} onSubmit={submit}>
      <h2>Add a price</h2>
      <p className="sub">Model can be an alias (smart), an upstream model (gpt-5) or provider/model (openai/gpt-5). Cached input and cache write default to the input price.</p>
      <ErrorBox error={error} />
      <div className="form-row">
        <label className="field">
          Model
          <input className="input" required value={f.model} onChange={set("model")} placeholder="gpt-5" />
        </label>
        <label className="field">
          Input
          <input className="input" required type="number" min="0" step="any" style={{ width: 100 }} value={f.input} onChange={set("input")} placeholder="1.25" />
        </label>
        <label className="field">
          Cached input
          <input className="input" type="number" min="0" step="any" style={{ width: 110 }} value={f.cached_input} onChange={set("cached_input")} placeholder="0.125" />
        </label>
        <label className="field">
          Cache write
          <input className="input" type="number" min="0" step="any" style={{ width: 110 }} value={f.cache_write} onChange={set("cache_write")} placeholder="same as input" />
        </label>
        <label className="field">
          Output
          <input className="input" required type="number" min="0" step="any" style={{ width: 100 }} value={f.output} onChange={set("output")} placeholder="10" />
        </label>
        <label className="field">
          Effective from
          <input className="input" type="datetime-local" value={f.effective_from} onChange={set("effective_from")} />
        </label>
        <button className="btn primary" disabled={!valid || busy}>
          {busy ? "Adding…" : "Add price"}
        </button>
      </div>
    </form>
  );
}
