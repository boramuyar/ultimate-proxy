import { useState, type FormEvent } from "react";
import { Copy, KeyRound, Plus } from "lucide-react";
import { api, type ApiKey, type Application, type KeyPolicy, type Tenant } from "@/api";
import type { PageProps } from "@/App";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardHeader, CardTitle } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
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
import { fmtTime } from "@/format";
import { useAsync, useDirectory } from "@/hooks";

export default function Access(_: PageProps) {
  const dir = useDirectory();
  const [name, setName] = useState("");
  const [error, setError] = useState<string>();

  async function addTenant(e: FormEvent) {
    e.preventDefault();
    setError(undefined);
    try {
      await api.createTenant(name.trim());
      setName("");
      dir.reload();
    } catch (err) {
      setError((err as Error).message);
    }
  }

  return (
    <>
      <PageHeader title="Tenants & keys" description="Every API key belongs to one application, and every application to one tenant. Usage is attributed along that chain.">
        {dir.isAdmin && (
          <form onSubmit={addTenant} className="flex items-end gap-2">
            <div className="grid gap-1.5">
              <Label htmlFor="new-tenant">New tenant</Label>
              <Input id="new-tenant" className="w-56" value={name} onChange={(e) => setName(e.target.value)} placeholder="tenant name" />
            </div>
            <Button type="submit" disabled={!name.trim()}>
              <Plus /> Add
            </Button>
          </form>
        )}
      </PageHeader>
      <ErrorBox error={error} />
      {dir.tenants.length === 0 ? (
        <Card>
          <Empty>No tenants yet.</Empty>
        </Card>
      ) : (
        <div className="grid gap-6">
          {dir.tenants.map((t) => (
            <TenantCard key={t.id} tenant={t} apps={dir.apps.filter((a) => a.tenant_id === t.id)} />
          ))}
        </div>
      )}
    </>
  );
}

function TenantCard({ tenant, apps }: { tenant: Tenant; apps: Application[] }) {
  const writable = useDirectory().canWrite(tenant.id);
  return (
    <Card>
      <CardHeader className="items-center">
        <CardTitle className="text-base font-semibold">{tenant.name}</CardTitle>
        <span className="font-mono text-xs text-faint">
          {tenant.id} · {apps.length} app{apps.length === 1 ? "" : "s"}
        </span>
      </CardHeader>
      {apps.map((a) => (
        <AppKeys key={a.id} app={a} writable={writable} />
      ))}
      {writable && <AddApp tenantId={tenant.id} />}
    </Card>
  );
}

function AddApp({ tenantId }: { tenantId: string }) {
  const dir = useDirectory();
  const [name, setName] = useState("");
  const [trusted, setTrusted] = useState(true);
  const [error, setError] = useState<string>();

  async function submit(e: FormEvent) {
    e.preventDefault();
    setError(undefined);
    try {
      await api.createApplication(tenantId, name.trim(), trusted);
      setName("");
      dir.reload();
    } catch (err) {
      setError((err as Error).message);
    }
  }

  return (
    <form onSubmit={submit} className="border-t bg-[#fafafa] px-5 py-3">
      <ErrorBox error={error} />
      <div className="flex flex-wrap items-center gap-3">
        <Input aria-label="New application name" className="w-56 bg-card" value={name} onChange={(e) => setName(e.target.value)} placeholder="new application" />
        <Label className="cursor-pointer text-foreground">
          <Checkbox checked={trusted} onCheckedChange={(v) => setTrusted(v === true)} />
          May name its end users
        </Label>
        <Button type="submit" variant="outline" size="sm" disabled={!name.trim()}>
          <Plus /> Add application
        </Button>
      </div>
    </form>
  );
}

function AppKeys({ app, writable }: { app: Application; writable: boolean }) {
  const keys = useAsync(() => api.keys(app.id), [app.id]);
  const [created, setCreated] = useState<ApiKey>();
  const [error, setError] = useState<string>();
  const [copied, setCopied] = useState(false);
  const [creating, setCreating] = useState(false);
  const [expiry, setExpiry] = useState("never");
  const [models, setModels] = useState("");

  async function createKey(e: FormEvent) {
    e.preventDefault();
    setError(undefined);
    const policy: KeyPolicy = {};
    if (expiry !== "never") policy.expires_in = Number(expiry) * 86400;
    const list = models
      .split(",")
      .map((m) => m.trim())
      .filter(Boolean);
    if (list.length > 0) policy.allowed_models = list;
    try {
      setCreated(await api.createKey(app.id, policy));
      setCopied(false);
      setCreating(false);
      setExpiry("never");
      setModels("");
      keys.reload();
    } catch (err) {
      setError((err as Error).message);
    }
  }

  async function revoke(k: ApiKey) {
    setError(undefined);
    try {
      await api.revokeKey(k.id);
      keys.reload();
    } catch (err) {
      setError((err as Error).message);
    }
  }

  return (
    <div className="border-t">
      <div className="flex flex-wrap items-center justify-between gap-3 px-5 py-3">
        <div className="flex flex-wrap items-center gap-2">
          <span className="font-medium">{app.name}</span>
          {app.can_assert_users ? <Badge variant="info">Names its users</Badge> : <Badge variant="muted">Untrusted client</Badge>}
          <span className="font-mono text-xs text-faint">{app.id}</span>
        </div>
        {writable && (
          <Button variant="outline" size="sm" onClick={() => setCreating((c) => !c)}>
            <KeyRound /> New key
          </Button>
        )}
      </div>
      {creating && (
        <form onSubmit={createKey} className="mx-5 mb-3 flex flex-wrap items-end gap-3 rounded-lg border bg-secondary p-3">
          <div className="grid gap-1.5">
            <Label>Expires</Label>
            <Select value={expiry} onValueChange={setExpiry}>
              <SelectTrigger className="w-36">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="never">Never</SelectItem>
                <SelectItem value="7">In 7 days</SelectItem>
                <SelectItem value="30">In 30 days</SelectItem>
                <SelectItem value="90">In 90 days</SelectItem>
                <SelectItem value="365">In a year</SelectItem>
              </SelectContent>
            </Select>
          </div>
          <div className="grid min-w-56 flex-1 gap-1.5">
            <Label htmlFor={`models-${app.id}`}>Models</Label>
            <Input id={`models-${app.id}`} placeholder="All models, or e.g. gpt-5-mini, vllm/*" value={models} onChange={(e) => setModels(e.target.value)} />
          </div>
          <Button type="submit" size="sm">
            Create key
          </Button>
        </form>
      )}
      <div className="px-5">
        <ErrorBox error={error ?? keys.error} />
      </div>
      {created?.key && (
        <div className="mx-5 mb-3 grid gap-2 rounded-lg border border-[#b8e6c6] bg-good-bg p-3">
          <span className="text-[13px] font-medium text-good">Copy this key now. It won't be shown again.</span>
          <code className="break-all rounded-md border bg-card px-2.5 py-1.5 font-mono text-[13px]">{created.key}</code>
          <div className="flex gap-2">
            <Button
              size="sm"
              onClick={() =>
                navigator.clipboard?.writeText(created.key!).then(
                  () => setCopied(true),
                  () => {},
                )
              }
            >
              <Copy /> {copied ? "Copied" : "Copy"}
            </Button>
            <Button size="sm" variant="outline" onClick={() => setCreated(undefined)}>
              Done
            </Button>
          </div>
        </div>
      )}
      {(keys.data?.length ?? 0) > 0 && (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Key</TableHead>
              <TableHead>Created</TableHead>
              <TableHead>Models</TableHead>
              <TableHead>Status</TableHead>
              <TableHead />
            </TableRow>
          </TableHeader>
          <TableBody>
            {keys.data!.map((k) => (
              <TableRow key={k.id}>
                <TableCell className="font-mono text-[13px]">{k.prefix}…</TableCell>
                <TableCell>{fmtTime(k.created_at)}</TableCell>
                <TableCell className="text-[13px]">{k.allowed_models === null ? <span className="text-faint">All</span> : k.allowed_models.join(", ") || <span className="text-faint">None</span>}</TableCell>
                <TableCell>
                  <KeyStatus k={k} />
                </TableCell>
                <TableCell className="text-right">
                  {writable && !k.revoked_at && !expired(k) && (
                    <AlertDialog>
                      <AlertDialogTrigger asChild>
                        <Button variant="outline" size="sm" className="text-critical hover:bg-critical-bg">
                          Revoke
                        </Button>
                      </AlertDialogTrigger>
                      <AlertDialogContent>
                        <AlertDialogBody>
                          <AlertDialogTitle>Revoke {k.prefix}…?</AlertDialogTitle>
                          <AlertDialogDescription>Requests from {app.name} using this key will fail immediately. This can't be undone.</AlertDialogDescription>
                        </AlertDialogBody>
                        <AlertDialogFooter>
                          <AlertDialogCancel>Cancel</AlertDialogCancel>
                          <AlertDialogAction onClick={() => revoke(k)}>Revoke key</AlertDialogAction>
                        </AlertDialogFooter>
                      </AlertDialogContent>
                    </AlertDialog>
                  )}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </div>
  );
}

function expired(k: ApiKey): boolean {
  return k.expires_at !== null && new Date(k.expires_at).getTime() <= Date.now();
}

function KeyStatus({ k }: { k: ApiKey }) {
  if (k.revoked_at) return <Badge variant="muted">Revoked {fmtTime(k.revoked_at)}</Badge>;
  if (expired(k)) return <Badge variant="muted">Expired {fmtTime(k.expires_at!)}</Badge>;
  if (k.expires_at) {
    const days = Math.ceil((new Date(k.expires_at).getTime() - Date.now()) / 86400000);
    return <Badge variant={days <= 7 ? "warning" : "good"}>{days <= 1 ? "Expires within a day" : `Expires in ${days} days`}</Badge>;
  }
  return <Badge variant="good">Active</Badge>;
}
