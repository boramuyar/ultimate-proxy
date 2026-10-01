import { useEffect, useState, type FormEvent } from "react";
import { Plus } from "lucide-react";
import { api, type Limit, type LimitKind, type Period } from "@/api";
import type { PageProps } from "@/App";
import { Button } from "@/components/ui/button";
import { Card, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogBody,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogTitle,
  AlertDialogTrigger,
} from "@/components/ui/alert-dialog";
import { Empty, ErrorBox, PageHeader } from "@/components/page";
import { fmtNumber, fmtTime, fmtUSD } from "@/format";
import { Badge } from "@/components/ui/badge";
import { useAsync, useDirectory } from "@/hooks";

const isBudget = (k: LimitKind) => k === "budget_usd" || k === "budget_tokens";

// amount writes a value in a rule's unit: dollars for spend budgets.
const amountOf = (kind: LimitKind, n: number) => (kind === "budget_usd" ? fmtUSD(n) : fmtNumber(n));

function unit(l: Pick<Limit, "kind" | "period">): string {
  switch (l.kind) {
    case "rpm":
      return "requests / min";
    case "tpm":
      return "tokens / min";
    case "budget_tokens":
      return `tokens / ${l.period}`;
    default:
      return `/ ${l.period}`;
  }
}

export default function Limits(_: PageProps) {
  const dir = useDirectory();
  const list = useAsync(() => api.limits(), []);
  // Current use changes by the second; refresh it while the page is open.
  useEffect(() => {
    const t = setInterval(list.reload, 10_000);
    return () => clearInterval(t);
  }, [list.reload]);

  return (
    <>
      <PageHeader
        title="Limits"
        description="Rate limits per sliding minute and budgets per day, week or month (UTC), for a tenant, an application, or its end users. Refused requests get 429 with Retry-After."
      />
      {dir.tenants.some((t) => dir.canWrite(t.id)) && <AddLimit onAdded={list.reload} />}
      <ErrorBox error={list.error} />
      <Card>
        <CardHeader>
          <CardTitle>Rules</CardTitle>
        </CardHeader>
        {!list.data?.length ? (
          <Empty>{list.loading ? "Loading…" : "No limits yet. Every request is allowed."}</Empty>
        ) : (
          <LimitTable limits={list.data} onChange={list.reload} />
        )}
      </Card>
    </>
  );
}

function appliesTo(l: Limit): string {
  if (l.user === "*") return "Each user";
  if (l.user) return l.user;
  return "Everyone together";
}

function LimitTable({ limits, onChange }: { limits: Limit[]; onChange: () => void }) {
  const dir = useDirectory();
  const [error, setError] = useState<string>();
  async function remove(l: Limit) {
    try {
      await api.deleteLimit(l.id);
      onChange();
    } catch (e) {
      setError((e as Error).message);
    }
  }
  return (
    <>
      <div className="px-5">
        <ErrorBox error={error} />
      </div>
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Tenant</TableHead>
            <TableHead>Application</TableHead>
            <TableHead>Applies to</TableHead>
            <TableHead className="text-right">Limit</TableHead>
            <TableHead className="w-64">Current use</TableHead>
            <TableHead />
          </TableRow>
        </TableHeader>
        <TableBody>
          {limits.map((l) => (
            <TableRow key={l.id}>
              <TableCell className="font-medium">{dir.tenantName(l.tenant_id)}</TableCell>
              <TableCell>{l.application_id ? dir.appName(l.application_id) : <span className="text-faint">All</span>}</TableCell>
              <TableCell className="text-[13px]">{appliesTo(l)}</TableCell>
              <TableCell className="text-right tabular-nums">
                {l.enforcement === "soft" && (
                  <Badge variant="muted" className="mr-2" title="Warns at 80% and 100%, never refuses">
                    Soft
                  </Badge>
                )}
                {amountOf(l.kind, l.amount)} <span className="text-faint">{unit(l)}</span>
              </TableCell>
              <TableCell>
                <UseBar limit={l} />
                {l.resets_at && <div className="mt-1 text-xs text-faint">Resets {fmtTime(l.resets_at)}</div>}
              </TableCell>
              <TableCell className="text-right">
                {dir.canWrite(l.tenant_id) && (
                  <AlertDialog>
                    <AlertDialogTrigger asChild>
                      <Button variant="outline" size="sm" className="text-critical hover:bg-critical-bg">
                        Delete
                      </Button>
                    </AlertDialogTrigger>
                    <AlertDialogContent>
                      <AlertDialogBody>
                        <AlertDialogTitle>Delete this limit?</AlertDialogTitle>
                        <AlertDialogDescription>
                          {appliesTo(l)} in {l.application_id ? dir.appName(l.application_id) : dir.tenantName(l.tenant_id)} will no longer be held to {amountOf(l.kind, l.amount)}{" "}
                          {unit(l)}.
                        </AlertDialogDescription>
                      </AlertDialogBody>
                      <AlertDialogFooter>
                        <AlertDialogCancel>Cancel</AlertDialogCancel>
                        <AlertDialogAction onClick={() => remove(l)}>Delete limit</AlertDialogAction>
                      </AlertDialogFooter>
                    </AlertDialogContent>
                  </AlertDialog>
                )}
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </>
  );
}

function UseBar({ limit: { used, amount, kind } }: { limit: Limit }) {
  if (used === null || used === undefined) return <span className="text-[13px] text-faint">Counted per user</span>;
  const share = Math.min(used / amount, 1);
  const color = share >= 1 ? "bg-critical" : share >= 0.8 ? "bg-warning" : "bg-foreground";
  return (
    <div className="flex items-center gap-2">
      <div className="h-1.5 flex-1 overflow-hidden rounded-full bg-secondary">
        <div className={`h-full rounded-full ${color}`} style={{ width: `${share * 100}%` }} />
      </div>
      <span className="w-28 text-right text-[13px] tabular-nums text-muted-foreground">
        {amountOf(kind, used)} / {amountOf(kind, amount)}
      </span>
    </div>
  );
}

type Who = "all" | "each" | "one";

function AddLimit({ onAdded }: { onAdded: () => void }) {
  const dir = useDirectory();
  const [tenant, setTenant] = useState("");
  const [app, setApp] = useState("all");
  const [who, setWho] = useState<Who>("all");
  const [email, setEmail] = useState("");
  const [kind, setKind] = useState<LimitKind>("rpm");
  const [amount, setAmount] = useState("");
  const [period, setPeriod] = useState<Period>("month");
  const [enforcement, setEnforcement] = useState<"hard" | "soft">("hard");
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);
  const apps = dir.apps.filter((a) => a.tenant_id === tenant);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      await api.createLimit({
        tenant_id: tenant,
        application_id: app === "all" ? undefined : app,
        user: who === "each" ? "*" : who === "one" ? email.trim() : undefined,
        kind,
        amount: Number(amount),
        ...(isBudget(kind) ? { period, enforcement } : {}),
      });
      setAmount("");
      setEmail("");
      onAdded();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  }

  const valid = tenant && Number(amount) > 0 && (who !== "one" || email.includes("@"));
  return (
    <Card className="mb-6">
      <CardHeader className="flex-col items-stretch gap-1">
        <CardTitle>Add a limit</CardTitle>
        <CardDescription>
          "Each user" gives every end user their own allowance. Tokens and spend are charged after each response, so one request in flight can go
          over. Budgets alert at 80% and 100% on the Insights tab; soft budgets only alert.
        </CardDescription>
      </CardHeader>
      <form onSubmit={submit} className="grid gap-3 px-5 pb-5">
        <ErrorBox error={error} />
        <div className="flex flex-wrap items-end gap-3">
          <Field label="Tenant" className="w-44">
            <Select
              value={tenant}
              onValueChange={(v) => {
                setTenant(v);
                setApp("all");
              }}
            >
              <SelectTrigger aria-label="Tenant">
                <SelectValue placeholder="Choose a tenant" />
              </SelectTrigger>
              <SelectContent>
                {dir.tenants
                  .filter((t) => dir.canWrite(t.id))
                  .map((t) => (
                    <SelectItem key={t.id} value={t.id}>
                      {t.name}
                    </SelectItem>
                  ))}
              </SelectContent>
            </Select>
          </Field>
          <Field label="Application" className="w-44">
            <Select value={app} onValueChange={setApp} disabled={!tenant}>
              <SelectTrigger aria-label="Application">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="all">All applications</SelectItem>
                {apps.map((a) => (
                  <SelectItem key={a.id} value={a.id}>
                    {a.name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </Field>
          <Field label="Applies to" className="w-44">
            <Select value={who} onValueChange={(v) => setWho(v as Who)}>
              <SelectTrigger aria-label="Applies to">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="all">Everyone together</SelectItem>
                <SelectItem value="each">Each user</SelectItem>
                <SelectItem value="one">One user</SelectItem>
              </SelectContent>
            </Select>
          </Field>
          {who === "one" && (
            <Field label="User email" htmlFor="l-email" className="w-56">
              <Input id="l-email" type="email" value={email} onChange={(e) => setEmail(e.target.value)} placeholder="ann@example.com" />
            </Field>
          )}
          <Field label="Counts" className="w-48">
            <Select value={kind} onValueChange={(v) => setKind(v as LimitKind)}>
              <SelectTrigger aria-label="Counts">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="rpm">Requests per minute</SelectItem>
                <SelectItem value="tpm">Tokens per minute</SelectItem>
                <SelectItem value="budget_usd">Spend budget (USD)</SelectItem>
                <SelectItem value="budget_tokens">Token budget</SelectItem>
              </SelectContent>
            </Select>
          </Field>
          {isBudget(kind) && (
            <>
              <Field label="Per" className="w-28">
                <Select value={period} onValueChange={(v) => setPeriod(v as Period)}>
                  <SelectTrigger aria-label="Period">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="day">Day</SelectItem>
                    <SelectItem value="week">Week</SelectItem>
                    <SelectItem value="month">Month</SelectItem>
                  </SelectContent>
                </Select>
              </Field>
              <Field label="When reached" className="w-36">
                <Select value={enforcement} onValueChange={(v) => setEnforcement(v as "hard" | "soft")}>
                  <SelectTrigger aria-label="When reached">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="hard">Refuse</SelectItem>
                    <SelectItem value="soft">Only alert</SelectItem>
                  </SelectContent>
                </Select>
              </Field>
            </>
          )}
          <Field label={kind === "budget_usd" ? "Amount (USD)" : "Amount"} htmlFor="l-amount" className="w-32">
            <Input
              id="l-amount"
              type="number"
              min={kind === "budget_usd" ? "0.01" : "1"}
              step={kind === "budget_usd" ? "0.01" : "1"}
              value={amount}
              onChange={(e) => setAmount(e.target.value)}
              placeholder={kind === "budget_usd" ? "50" : "600"}
            />
          </Field>
          <Button type="submit" disabled={!valid || busy}>
            <Plus /> {busy ? "Adding…" : "Add"}
          </Button>
        </div>
      </form>
    </Card>
  );
}

function Field({ label, htmlFor, className, children }: { label: string; htmlFor?: string; className?: string; children: React.ReactNode }) {
  return (
    <div className={`grid gap-1.5 ${className ?? ""}`}>
      <Label htmlFor={htmlFor}>{label}</Label>
      {children}
    </div>
  );
}
