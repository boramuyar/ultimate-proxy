import { useEffect, useMemo, useState, type FormEvent } from "react";
import { LogOut } from "lucide-react";
import { api, getToken, setToken, setUnauthorizedHandler } from "@/api";
import { DirectoryContext, useAsync, type Directory, type RangeKey } from "@/hooks";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { ErrorBox } from "@/components/page";
import { cn } from "@/lib/utils";
import Overview from "@/pages/Overview";
import Usage from "@/pages/Usage";
import Cache from "@/pages/Cache";
import Insights from "@/pages/Insights";
import Prices from "@/pages/Prices";
import Access from "@/pages/Access";

const PAGES = {
  overview: { label: "Overview", Component: Overview },
  usage: { label: "Usage", Component: Usage },
  cache: { label: "Prompt cache", Component: Cache },
  insights: { label: "Insights", Component: Insights },
  prices: { label: "Prices", Component: Prices },
  access: { label: "Tenants & keys", Component: Access },
} as const;
type PageKey = keyof typeof PAGES;

export interface PageProps {
  range: RangeKey;
  setRange: (r: RangeKey) => void;
}

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

function Brand() {
  return (
    <div className="flex items-center gap-2.5">
      <img src="/favicon.svg" alt="" className="size-7" />
      <div className="leading-tight">
        <div className="text-[13px] font-bold uppercase tracking-wider">Ultimate_Proxy</div>
        <div className="text-[10.5px] text-muted-foreground">open responses gateway</div>
      </div>
    </div>
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
    <div className="grid min-h-screen place-items-center p-4">
      <form onSubmit={submit} className="w-full max-w-sm border border-strong bg-card shadow-[6px_6px_0_0_var(--strong)]">
        <div className="border-b border-strong px-5 py-4">
          <Brand />
        </div>
        <div className="grid gap-4 px-5 py-5">
          <p className="font-sans text-[13.5px] text-muted-foreground">
            Sign in with the proxy's admin token, <code className="font-mono text-foreground">PROXY_ADMIN_TOKEN</code>.
          </p>
          <ErrorBox error={error} />
          <div className="grid gap-1.5">
            <Label htmlFor="token">Admin token</Label>
            <Input id="token" type="password" autoFocus value={value} onChange={(e) => setValue(e.target.value)} />
          </div>
          <Button type="submit" disabled={!value.trim() || busy}>
            {busy ? "Checking…" : "Sign in →"}
          </Button>
        </div>
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
  const health = useAsync(() => fetch("/healthz").then((r) => r.ok), [page]);

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
  const keys = Object.keys(PAGES) as PageKey[];

  return (
    <DirectoryContext.Provider value={directory}>
      <div className="min-h-screen md:grid md:grid-cols-[232px_1fr]">
        <aside className="flex flex-col border-b border-strong bg-card md:sticky md:top-0 md:h-screen md:border-r md:border-b-0">
          <div className="border-b border-strong px-4 py-4">
            <Brand />
          </div>
          <nav aria-label="Sections" className="flex flex-wrap md:flex-col">
            {keys.map((k, i) => (
              <a
                key={k}
                href={`#/${k}`}
                aria-current={k === page ? "page" : undefined}
                className={cn(
                  "flex items-center gap-3 border-b border-border px-4 py-2.5 text-[12px] font-medium uppercase tracking-wider outline-none hover:bg-muted focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-inset",
                  k === page && "bg-foreground text-background hover:bg-foreground",
                )}
              >
                <span className={cn("tabular-nums text-muted-foreground", k === page && "text-background/60")}>{String(i + 1).padStart(2, "0")}</span>
                <span className="flex-1">{PAGES[k].label}</span>
                {k === "insights" && openCount > 0 && (
                  <span className="bg-critical px-1.5 text-[10.5px] font-bold text-white tabular-nums">{openCount}</span>
                )}
              </a>
            ))}
          </nav>
          <div className="mt-auto hidden border-t border-strong md:block">
            <div className="flex items-center gap-2 px-4 py-2.5 text-[11px] text-muted-foreground">
              <span className={cn("inline-block size-2", health.data ? "bg-good" : health.loading ? "bg-muted-foreground" : "bg-critical")} />
              {health.data ? "proxy online" : health.loading ? "checking proxy" : "proxy unreachable"}
            </div>
            <button
              onClick={onSignOut}
              className="flex w-full cursor-pointer items-center gap-2 border-t border-border px-4 py-2.5 text-left text-[12px] uppercase tracking-wider hover:bg-muted"
            >
              <LogOut className="size-3.5" /> Sign out
            </button>
          </div>
        </aside>
        <div className="min-w-0">
          <div className="flex h-11 items-center justify-between border-b border-strong bg-card px-5 text-[12px] md:px-8">
            <span className="text-muted-foreground">
              ~/admin/<span className="text-foreground">{page}</span>
            </span>
            <button onClick={onSignOut} className="cursor-pointer text-[11px] uppercase tracking-wider underline md:hidden">
              Sign out
            </button>
          </div>
          <main className="mx-auto max-w-[1320px] px-4 py-6 md:px-8">
            <Component range={range} setRange={setRange} />
          </main>
        </div>
      </div>
    </DirectoryContext.Provider>
  );
}
