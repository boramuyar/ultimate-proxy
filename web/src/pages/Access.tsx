import { useState, type FormEvent } from "react";
import { Copy, KeyRound, Plus } from "lucide-react";
import { api, type ApiKey, type Application, type Tenant } from "@/api";
import type { PageProps } from "@/App";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardHeader, CardTitle } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import {
  AlertDialog,
  AlertDialogAction,
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
        <form onSubmit={addTenant} className="flex items-end gap-2">
          <div className="grid gap-1.5">
            <Label htmlFor="new-tenant">New tenant</Label>
            <Input id="new-tenant" className="w-56" value={name} onChange={(e) => setName(e.target.value)} placeholder="tenant name" />
          </div>
          <Button type="submit" disabled={!name.trim()}>
            <Plus /> Add
          </Button>
        </form>
      </PageHeader>
      <ErrorBox error={error} />
      {dir.tenants.length === 0 ? (
        <Card>
          <Empty>No tenants yet.</Empty>
        </Card>
      ) : (
        <div className="grid gap-5">
          {dir.tenants.map((t) => (
            <TenantCard key={t.id} tenant={t} apps={dir.apps.filter((a) => a.tenant_id === t.id)} />
          ))}
        </div>
      )}
    </>
  );
}

function TenantCard({ tenant, apps }: { tenant: Tenant; apps: Application[] }) {
  return (
    <Card>
      <CardHeader className="items-center">
        <CardTitle className="text-[13px]">{tenant.name}</CardTitle>
        <span className="text-[11px] text-muted-foreground">
          {tenant.id} · {apps.length} app{apps.length === 1 ? "" : "s"}
        </span>
      </CardHeader>
      {apps.map((a) => (
        <AppKeys key={a.id} app={a} />
      ))}
      <AddApp tenantId={tenant.id} />
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
    <form onSubmit={submit} className="bg-muted/50 px-4 py-3">
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

function AppKeys({ app }: { app: Application }) {
  const keys = useAsync(() => api.keys(app.id), [app.id]);
  const [created, setCreated] = useState<ApiKey>();
  const [error, setError] = useState<string>();
  const [copied, setCopied] = useState(false);

  async function createKey() {
    setError(undefined);
    try {
      setCreated(await api.createKey(app.id));
      setCopied(false);
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
    <div className="border-b border-strong">
      <div className="flex flex-wrap items-center justify-between gap-3 px-4 py-3">
        <div className="flex flex-wrap items-center gap-2">
          <span className="text-[13px] font-bold">{app.name}</span>
          {app.can_assert_users ? <Badge variant="info">names its users</Badge> : <Badge variant="muted">untrusted client</Badge>}
          <span className="text-[11px] text-muted-foreground">{app.id}</span>
        </div>
        <Button variant="outline" size="sm" onClick={createKey}>
          <KeyRound /> New key
        </Button>
      </div>
      <div className="px-4">
        <ErrorBox error={error ?? keys.error} />
      </div>
      {created?.key && (
        <div className="mx-4 mb-3 grid gap-2 border border-good bg-good-bg p-3">
          <span className="text-[11px] font-bold uppercase tracking-wider text-good">Copy this key now. It won't be shown again.</span>
          <code className="break-all bg-card px-2 py-1.5 text-[13px]">{created.key}</code>
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
              <TableHead className="pl-4">Key</TableHead>
              <TableHead>Created</TableHead>
              <TableHead>Status</TableHead>
              <TableHead />
            </TableRow>
          </TableHeader>
          <TableBody>
            {keys.data!.map((k) => (
              <TableRow key={k.id}>
                <TableCell className="pl-4">{k.prefix}…</TableCell>
                <TableCell>{fmtTime(k.created_at)}</TableCell>
                <TableCell>{k.revoked_at ? <Badge variant="muted">revoked {fmtTime(k.revoked_at)}</Badge> : <Badge variant="good">active</Badge>}</TableCell>
                <TableCell className="text-right">
                  {!k.revoked_at && (
                    <AlertDialog>
                      <AlertDialogTrigger asChild>
                        <Button variant="destructive" size="sm">
                          Revoke
                        </Button>
                      </AlertDialogTrigger>
                      <AlertDialogContent>
                        <AlertDialogTitle>Revoke {k.prefix}…?</AlertDialogTitle>
                        <AlertDialogDescription>Requests from {app.name} using this key will fail immediately. This can't be undone.</AlertDialogDescription>
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
