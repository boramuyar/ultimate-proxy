import { useEffect, useMemo, useState, type FormEvent } from "react";
import { api, getToken, setToken, setUnauthorizedHandler } from "./api";
import { DirectoryContext, useAsync, type Directory, type RangeKey } from "./hooks";
import Overview from "./pages/Overview";
import Usage from "./pages/Usage";
import Cache from "./pages/Cache";
import Insights from "./pages/Insights";
import Prices from "./pages/Prices";
import Access from "./pages/Access";

const PAGES = {
  overview: { label: "Overview", Component: Overview },
  usage: { label: "Usage", Component: Usage },
  cache: { label: "Prompt cache", Component: Cache },
  insights: { label: "Insights", Component: Insights },
  prices: { label: "Prices", Component: Prices },
  access: { label: "Tenants & keys", Component: Access },
} as const;
type PageKey = keyof typeof PAGES;

function pageFromHash(): PageKey {
  const h = window.location.hash.replace(/^#\/?/, "");
  return h in PAGES ? (h as PageKey) : "overview";
}

export default function App() {
  const [token, setTok] = useState(getToken());
  useEffect(() => {
    setUnauthorizedHandler(() => {
      setToken(null);
      setTok(null);
    });
  }, []);
  if (!token) return <Login onToken={setTok} />;
  return (
    <Dashboard
      onSignOut={() => {
        setToken(null);
        setTok(null);
      }}
    />
  );
}

function Login({ onToken }: { onToken: (t: string) => void }) {
  const [value, setValue] = useState("");
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      await api.check(value.trim());
      setToken(value.trim());
      onToken(value.trim());
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="login">
      <form className="panel" onSubmit={submit}>
        <img src="/favicon.svg" width={36} height={36} alt="" />
        <h1>Ultimate Proxy</h1>
        <p className="muted" style={{ marginTop: 0 }}>
          Sign in with the proxy's admin token (<code>PROXY_ADMIN_TOKEN</code>).
        </p>
        {error && <div className="error">{error}</div>}
        <label className="field">
          Admin token
          <input className="input" type="password" autoFocus value={value} onChange={(e) => setValue(e.target.value)} />
        </label>
        <button className="btn primary" style={{ width: "100%", marginTop: 14 }} disabled={!value.trim() || busy}>
          {busy ? "Checking…" : "Sign in"}
        </button>
      </form>
    </div>
  );
}

function Dashboard({ onSignOut }: { onSignOut: () => void }) {
  const [page, setPage] = useState<PageKey>(pageFromHash());
  const [range, setRange] = useState<RangeKey>("24h");
  useEffect(() => {
    const onHash = () => setPage(pageFromHash());
    window.addEventListener("hashchange", onHash);
    return () => window.removeEventListener("hashchange", onHash);
  }, []);

  const tenants = useAsync(api.tenants, []);
  const apps = useAsync(api.applications, []);
  const openInsights = useAsync(() => api.insights("open"), [page]);

  const directory = useMemo<Directory>(() => {
    const t = new Map((tenants.data ?? []).map((x) => [x.id, x.name]));
    const a = new Map((apps.data ?? []).map((x) => [x.id, x.name]));
    return {
      tenants: tenants.data ?? [],
      apps: apps.data ?? [],
      tenantName: (id) => t.get(id) ?? id,
      appName: (id) => a.get(id) ?? id,
      reload: () => {
        tenants.reload();
        apps.reload();
      },
    };
  }, [tenants.data, apps.data, tenants.reload, apps.reload]);

  const { Component } = PAGES[page];
  const openCount = openInsights.data?.length ?? 0;

  return (
    <DirectoryContext.Provider value={directory}>
      <div className="shell">
        <nav className="sidebar" aria-label="Sections">
          <div className="brand">
            <img src="/favicon.svg" alt="" />
            Ultimate Proxy
          </div>
          {(Object.keys(PAGES) as PageKey[]).map((k) => (
            <a key={k} href={`#/${k}`} className={`nav-item ${k === page ? "active" : ""}`} aria-current={k === page ? "page" : undefined}>
              {PAGES[k].label}
              {k === "insights" && openCount > 0 && <span className="badge bad">{openCount}</span>}
            </a>
          ))}
          <div className="spacer" />
          <button className="nav-item" onClick={onSignOut}>
            Sign out
          </button>
        </nav>
        <main className="main">
          <Component range={range} setRange={setRange} />
        </main>
      </div>
    </DirectoryContext.Provider>
  );
}

export interface PageProps {
  range: RangeKey;
  setRange: (r: RangeKey) => void;
}
