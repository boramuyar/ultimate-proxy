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

function Logo({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 24 24" aria-hidden className={cn("size-5", className)}>
      <path d="M12 2 22.5 20.5h-21Z" fill="currentColor" />
    </svg>
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
    <div className="grid min-h-screen place-items-center bg-background p-4">
      <div className="w-full max-w-sm">
        <div className="mb-8 flex flex-col items-center gap-4 text-center">
          <Logo className="size-9" />
          <div>
            <h1 className="text-2xl font-semibold tracking-tight">Sign in to ultimate-proxy</h1>
            <p className="mt-1.5 text-muted-foreground">
              Use the proxy's admin token, <code className="font-mono text-[13px] text-foreground">PROXY_ADMIN_TOKEN</code>.
            </p>
          </div>
        </div>
        <form onSubmit={submit} className="grid gap-4 rounded-xl border bg-card p-6 shadow-[0_2px_8px_rgba(0,0,0,0.04)]">
          <ErrorBox error={error} />
          <div className="grid gap-2">
            <Label htmlFor="token">Admin token</Label>
            <Input id="token" type="password" autoFocus value={value} onChange={(e) => setValue(e.target.value)} />
          </div>
          <Button type="submit" size="lg" disabled={!value.trim() || busy}>
            {busy ? "Checking…" : "Continue"}
          </Button>
        </form>
      </div>
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
      <div className="min-h-screen">
        <header className="bg-card">
          <div className="mx-auto flex h-16 max-w-[1200px] items-center gap-3 px-4 md:px-6">
            <a href="#/overview" aria-label="Overview" className="text-foreground">
              <Logo />
            </a>
            <Slash />
            <span className="font-medium">ultimate-proxy</span>
            <span className="hidden rounded-full border px-2 text-xs leading-5 text-muted-foreground sm:inline">admin</span>
            <div className="ml-auto flex items-center gap-4">
              <span className="hidden items-center gap-2 text-[13px] text-muted-foreground sm:flex" title="GET /healthz">
                <span className={cn("size-2 rounded-full", health.data ? "bg-[#0cce6b]" : health.loading ? "bg-faint" : "bg-critical")} />
                {health.data ? "Healthy" : health.loading ? "Checking" : "Unreachable"}
              </span>
              <Button variant="outline" size="sm" onClick={onSignOut}>
                <LogOut /> Sign out
              </Button>
            </div>
          </div>
        </header>
        <nav aria-label="Sections" className="sticky top-0 z-40 border-b bg-card/90 backdrop-blur">
          <div className="mx-auto flex max-w-[1200px] gap-1 overflow-x-auto px-2 md:px-4">
            {keys.map((k) => (
              <a
                key={k}
                href={`#/${k}`}
                aria-current={k === page ? "page" : undefined}
                className={cn(
                  "relative flex h-12 shrink-0 items-center gap-2 px-3 text-sm text-muted-foreground outline-none transition-colors hover:text-foreground focus-visible:text-foreground",
                  "after:absolute after:inset-x-3 after:bottom-0 after:h-0.5 after:rounded-full",
                  k === page && "text-foreground after:bg-foreground",
                )}
              >
                {PAGES[k].label}
                {k === "insights" && openCount > 0 && (
                  <span className="rounded-full bg-critical px-1.5 text-[11px] leading-[18px] font-medium text-white tabular-nums">{openCount}</span>
                )}
              </a>
            ))}
          </div>
        </nav>
        <main className="mx-auto max-w-[1200px] px-4 py-8 md:px-6">
          <Component range={range} setRange={setRange} />
        </main>
      </div>
    </DirectoryContext.Provider>
  );
}

function Slash() {
  return (
    <svg viewBox="0 0 24 24" aria-hidden className="size-6 text-[#d4d4d4]">
      <path d="M16.88 3.55 7.12 20.45" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" />
    </svg>
  );
}
