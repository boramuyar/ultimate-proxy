import { useState, type FormEvent } from "react";
import { Plus } from "lucide-react";
import { api, type Price } from "@/api";
import type { PageProps } from "@/App";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { Empty, ErrorBox, PageHeader } from "@/components/page";
import { fmtTime } from "@/format";
import { useAsync, useDirectory } from "@/hooks";

type View = "current" | "history";

export default function Prices(_: PageProps) {
  const dir = useDirectory();
  const [view, setView] = useState<View>("current");
  const list = useAsync(() => api.prices(view === "current"), [view]);

  return (
    <>
      <PageHeader
        title="Prices"
        description="USD per million tokens. Prices are never edited: a new price takes over from its start time, and past requests keep the cost they were charged."
      >
        <ToggleGroup type="single" value={view} onValueChange={(v) => v && setView(v as View)} aria-label="Prices shown">
          <ToggleGroupItem value="current">In effect</ToggleGroupItem>
          <ToggleGroupItem value="history">History</ToggleGroupItem>
        </ToggleGroup>
      </PageHeader>
      {dir.isAdmin && <AddPrice onAdded={list.reload} />}
      <ErrorBox error={list.error} />
      <Card>
        <CardHeader>
          <CardTitle>{view === "current" ? "Prices in effect now" : "All price versions, newest first"}</CardTitle>
        </CardHeader>
        {!list.data?.length ? (
          <Empty>{list.loading ? "Loading…" : "No prices yet. Requests are still counted, but cost $0 until their model has a price."}</Empty>
        ) : (
          <PriceTable prices={view === "history" ? [...list.data].reverse() : list.data} />
        )}
      </Card>
    </>
  );
}

function PriceTable({ prices }: { prices: Price[] }) {
  const money = (n: number) => `$${n.toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 4 })}`;
  const now = Date.now();
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>Model</TableHead>
          <TableHead className="text-right">Input</TableHead>
          <TableHead className="text-right">Cached input</TableHead>
          <TableHead className="text-right">Cache write</TableHead>
          <TableHead className="text-right">Output</TableHead>
          <TableHead>Effective from</TableHead>
          <TableHead />
        </TableRow>
      </TableHeader>
      <TableBody>
        {prices.map((p) => (
          <TableRow key={p.id}>
            <TableCell className="font-mono text-[13px]">{p.model}</TableCell>
            <TableCell className="text-right">{money(p.input)}</TableCell>
            <TableCell className="text-right">{money(p.cached_input)}</TableCell>
            <TableCell className="text-right">{money(p.cache_write)}</TableCell>
            <TableCell className="text-right">{money(p.output)}</TableCell>
            <TableCell>{new Date(p.effective_from).getFullYear() < 1900 ? "always" : fmtTime(p.effective_from)}</TableCell>
            <TableCell className="text-right">{new Date(p.effective_from).getTime() > now && <Badge variant="info">Scheduled</Badge>}</TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
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
  const num = { type: "number", min: "0", step: "any" } as const;
  return (
    <Card className="mb-6">
      <CardHeader className="flex-col items-stretch gap-1">
        <CardTitle>Add a price</CardTitle>
        <CardDescription>
          Model is a model name (gpt-5), for every provider, or provider/model (openai/gpt-5) for one. Cached input and cache write default to the input price.
        </CardDescription>
      </CardHeader>
      <form onSubmit={submit} className="grid gap-3 px-5 pb-5">
        <ErrorBox error={error} />
        <div className="grid gap-3 sm:grid-cols-3 lg:grid-cols-[1.4fr_1fr_1fr_1fr_1fr_1.3fr_auto] lg:items-end">
          <Field id="p-model" label="Model">
            <Input id="p-model" required value={f.model} onChange={set("model")} placeholder="gpt-5" />
          </Field>
          <Field id="p-in" label="Input $/M">
            <Input id="p-in" required {...num} value={f.input} onChange={set("input")} placeholder="1.25" />
          </Field>
          <Field id="p-cin" label="Cached $/M">
            <Input id="p-cin" {...num} value={f.cached_input} onChange={set("cached_input")} placeholder="= input" />
          </Field>
          <Field id="p-cw" label="Cache write $/M">
            <Input id="p-cw" {...num} value={f.cache_write} onChange={set("cache_write")} placeholder="= input" />
          </Field>
          <Field id="p-out" label="Output $/M">
            <Input id="p-out" required {...num} value={f.output} onChange={set("output")} placeholder="10" />
          </Field>
          <Field id="p-from" label="Effective from">
            <Input id="p-from" type="datetime-local" value={f.effective_from} onChange={set("effective_from")} />
          </Field>
          <Button type="submit" disabled={!valid || busy}>
            <Plus /> {busy ? "Adding…" : "Add"}
          </Button>
        </div>
      </form>
    </Card>
  );
}

function Field({ id, label, children }: { id: string; label: string; children: React.ReactNode }) {
  return (
    <div className="grid gap-1.5">
      <Label htmlFor={id}>{label}</Label>
      {children}
    </div>
  );
}
