import { useState, type FormEvent } from "react";
import { api, type ApiKey, type Application } from "../api";
import type { PageProps } from "../App";
import { Empty, ErrorBox, PageHead } from "../components/ui";
import { fmtTime } from "../format";
import { useAsync, useDirectory } from "../hooks";

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
      <PageHead title="Tenants & keys" sub="Every API key belongs to one application, and every application to one tenant. Usage is billed along that chain." />
      <form className="panel" style={{ marginBottom: 16 }} onSubmit={addTenant}>
        <ErrorBox error={error} />
        <div className="form-row">
          <label className="field" style={{ flex: 1, maxWidth: 360 }}>
            New tenant
            <input className="input" value={name} onChange={(e) => setName(e.target.value)} placeholder="tenant name" />
          </label>
          <button className="btn primary" disabled={!name.trim()}>
            Add tenant
          </button>
        </div>
      </form>
      {dir.tenants.length === 0 ? (
        <div className="panel">
          <Empty>No tenants yet.</Empty>
        </div>
      ) : (
        dir.tenants.map((t) => (
          <div className="panel" key={t.id} style={{ marginBottom: 16 }}>
            <h2>
              {t.name} <span className="faint mono">{t.id}</span>
            </h2>
            {dir.apps
              .filter((a) => a.tenant_id === t.id)
              .map((a) => (
                <AppKeys key={a.id} app={a} />
              ))}
            <AddApp tenantId={t.id} />
          </div>
        ))
      )}
    </>
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
    <form onSubmit={submit} style={{ borderTop: "1px solid var(--border)", paddingTop: 12 }}>
      <ErrorBox error={error} />
      <div className="form-row">
        <label className="field">
          New application
          <input className="input" value={name} onChange={(e) => setName(e.target.value)} placeholder="application name" />
        </label>
        <label className="field" style={{ flexDirection: "row", alignItems: "center", gap: 6, paddingBottom: 8 }}>
          <input type="checkbox" checked={trusted} onChange={(e) => setTrusted(e.target.checked)} />
          May name its end users
        </label>
        <button className="btn" disabled={!name.trim()}>
          Add application
        </button>
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
    if (!window.confirm(`Revoke ${k.prefix}…? Requests using it will fail immediately.`)) return;
    setError(undefined);
    try {
      await api.revokeKey(k.id);
      keys.reload();
    } catch (err) {
      setError((err as Error).message);
    }
  }

  return (
    <div className="tree-app">
      <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", gap: 12, flexWrap: "wrap" }}>
        <div>
          <b>{app.name}</b>{" "}
          {app.can_assert_users ? <span className="badge accent">names its users</span> : <span className="badge">untrusted client</span>}
          <div className="faint mono">{app.id}</div>
        </div>
        <button className="btn small" onClick={createKey}>
          New key
        </button>
      </div>
      <ErrorBox error={error ?? keys.error} />
      {created?.key && (
        <div className="key-reveal">
          <span>Copy this key now. It won't be shown again.</span>
          <code>{created.key}</code>
          <div>
            <button
              className="btn small"
              onClick={() =>
                navigator.clipboard?.writeText(created.key!).then(
                  () => setCopied(true),
                  () => {},
                )
              }
            >
              {copied ? "Copied" : "Copy"}
            </button>{" "}
            <button className="btn small" onClick={() => setCreated(undefined)}>
              Done
            </button>
          </div>
        </div>
      )}
      {(keys.data?.length ?? 0) > 0 && (
        <div className="table-wrap" style={{ marginTop: 8 }}>
          <table>
            <thead>
              <tr>
                <th>Key</th>
                <th>Created</th>
                <th>Status</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {keys.data!.map((k) => (
                <tr key={k.id}>
                  <td className="mono">{k.prefix}…</td>
                  <td>{fmtTime(k.created_at)}</td>
                  <td>{k.revoked_at ? <span className="badge">revoked {fmtTime(k.revoked_at)}</span> : <span className="badge good">active</span>}</td>
                  <td className="num">
                    {!k.revoked_at && (
                      <button className="btn small danger" onClick={() => revoke(k)}>
                        Revoke
                      </button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
