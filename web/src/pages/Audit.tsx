import { useEffect, useState } from "react";
import { api, type AuditEntry } from "@/api";
import type { PageProps } from "@/App";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardHeader, CardTitle } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Empty, ErrorBox, PageHeader } from "@/components/page";
import { fmtTime } from "@/format";
import { useAsync } from "@/hooks";

const ACTIONS: Record<string, string> = {
  "tenant.create": "Created tenant",
  "application.create": "Created application",
  "key.create": "Created key",
  "key.update": "Changed key",
  "key.revoke": "Revoked key",
  "token.revoke": "Revoked token",
  "limit.create": "Created limit",
  "limit.update": "Changed limit",
  "limit.delete": "Deleted limit",
  "price.add": "Added price",
  sign_in: "Signed in",
  "sign_in.refused": "Sign-in refused",
};

export default function Audit(_: PageProps) {
  const first = useAsync(() => api.audit(), []);
  const [older, setOlder] = useState<AuditEntry[]>([]);
  const [more, setMore] = useState<{ loading: boolean; done: boolean; error?: string }>({ loading: false, done: false });
  useEffect(() => setOlder([]), [first.data]);

  const entries = [...(first.data ?? []), ...older];
  async function loadMore() {
    setMore({ loading: true, done: false });
    try {
      const page = await api.audit(entries[entries.length - 1].ts);
      setOlder((o) => [...o, ...page]);
      setMore({ loading: false, done: page.length < 100 });
    } catch (e) {
      setMore({ loading: false, done: false, error: (e as Error).message });
    }
  }

  return (
    <>
      <PageHeader title="Audit log" description="Every change made through the admin API and the dashboard, and every sign-in, newest first." />
      <ErrorBox error={first.error ?? more.error} />
      <Card>
        <CardHeader>
          <CardTitle>Changes</CardTitle>
        </CardHeader>
        {!entries.length ? (
          <Empty>{first.loading ? "Loading…" : "Nothing has been changed yet."}</Empty>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>When</TableHead>
                <TableHead>Who</TableHead>
                <TableHead>What</TableHead>
                <TableHead>Object</TableHead>
                <TableHead>Details</TableHead>
                <TableHead className="text-right">Result</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {entries.map((e) => (
                <TableRow key={e.id}>
                  <TableCell className="whitespace-nowrap">{fmtTime(e.ts)}</TableCell>
                  <TableCell>
                    {e.actor_method === "token" ? (
                      <span className="text-muted-foreground">Admin token</span>
                    ) : (
                      <span title={e.actor_name}>{e.actor_email || e.actor_name || "unknown"}</span>
                    )}
                  </TableCell>
                  <TableCell>{ACTIONS[e.action] ?? `${e.method} ${e.path}`}</TableCell>
                  <TableCell className="font-mono text-[12px]">{e.target_id ?? ""}</TableCell>
                  <TableCell className="max-w-[360px] truncate font-mono text-[12px] text-muted-foreground" title={e.request ? JSON.stringify(e.request) : undefined}>
                    {e.request ? JSON.stringify(e.request) : ""}
                  </TableCell>
                  <TableCell className="text-right">
                    {e.status < 400 ? <Badge variant="good">OK</Badge> : <Badge variant="critical">{e.status}</Badge>}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </Card>
      {entries.length >= 100 && !more.done && (
        <div className="mt-4 flex justify-center">
          <Button variant="outline" onClick={loadMore} disabled={more.loading}>
            {more.loading ? "Loading…" : "Older"}
          </Button>
        </div>
      )}
    </>
  );
}
