import { useCallback, useEffect, useMemo, useState, type FormEvent } from "react";
import { LogIn, LogOut } from "lucide-react";
import { api, auth, setUnauthorizedHandler, type AuthConfig, type Principal } from "@/api";
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

type AuthState =
  | { status: "loading" }
  | { status: "signed-in"; me: Principal }
  | { status: "signed-out"; config?: AuthConfig; error?: string };

// Reasons the proxy gives when it sends a failed sign-in back here.
const LOGIN_ERRORS: Record<string, string> = {
  not_allowed: "That account isn't allowed to use this dashboard. Ask the proxy's owner to add you.",
  login_expired: "The sign-in took too long or was started elsewhere. Try again.",
  provider_error: "The identity provider didn't complete the sign-in. Try again.",
  provider_unavailable: "The identity provider can't be reached right now.",
};

// takeLoginError reads and clears ?login_error from the address bar.
function takeLoginError(): string | undefined {
  const url = new URL(window.location.href);
  const code = url.searchParams.get("login_error");
  if (!code) return undefined;
  url.searchParams.delete("login_error");
  window.history.replaceState(null, "", url.pathname + url.search + url.hash);
  return LOGIN_ERRORS[code] ?? "Sign-in failed. Try again.";
}

export default function App() {
  const [state, setState] = useState<AuthState>({ status: "loading" });

  const signedOut = useCallback(async (error?: string) => {
    setState({ status: "signed-out", error });
    try {
      setState({ status: "signed-out", error, config: await auth.config() });
    } catch (err) {
      setState({ status: "signed-out", error: error ?? (err as Error).message });
    }
  }, []);

  useEffect(() => {
    setUnauthorizedHandler(() => void signedOut("Your session has ended. Sign in again."));
    const error = takeLoginError();
    auth.me().then(
      (me) => setState({ status: "signed-in", me }),
      () => void signedOut(error),
    );
  }, [signedOut]);

  if (state.status === "loading") return null;
  if (state.status === "signed-out") return <Login config={state.config} initialError={state.error} onSignedIn={(me) => setState({ status: "signed-in", me })} />;
  return (
    <Dashboard
      me={state.me}
      onSignOut={async () => {
        await auth.logout().catch(() => {});
        void signedOut();
      }}
    />
  );
}

// Logo: requests pass through the proxy, drawn as a ring on a line.
function Logo({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 24 24" aria-hidden className={cn("size-6", className)}>
      <rect width="24" height="24" rx="6" fill="currentColor" />
      <path d="M4.5 12h3.8M15.7 12h3.8" stroke="#fff" strokeWidth="2" strokeLinecap="round" />
      <circle cx="12" cy="12" r="3.7" fill="none" stroke="#fff" strokeWidth="2" />
    </svg>
  );
}

function Login({ config, initialError, onSignedIn }: { config?: AuthConfig; initialError?: string; onSignedIn: (me: Principal) => void }) {
  const [value, setValue] = useState("");
  const [error, setError] = useState(initialError);
  const [busy, setBusy] = useState(false);
  const [showToken, setShowToken] = useState(false);
  useEffect(() => setError(initialError), [initialError]);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      onSignedIn(await auth.token(value.trim()));
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  }

  const oidc = config?.oidc;
  const tokenForm = config?.token && (!oidc || showToken);
  const next = window.location.pathname + window.location.hash;

  return (
    <div className="grid min-h-screen place-items-center bg-background p-4">
      <div className="w-full max-w-sm">
        <div className="mb-8 flex flex-col items-center gap-4 text-center">
          <Logo className="size-9" />
          <div>
            <h1 className="text-2xl font-semibold tracking-tight">Sign in to ultimate-proxy</h1>
            {config && !oidc && config.token && (
              <p className="mt-1.5 text-muted-foreground">
                Use the proxy's admin token, <code className="font-mono text-[13px] text-foreground">PROXY_ADMIN_TOKEN</code>.
              </p>
            )}
          </div>
        </div>
        <div className="grid gap-4 rounded-xl border bg-card p-6 shadow-[0_2px_8px_rgba(0,0,0,0.04)]">
          <ErrorBox error={error} />
          {config && !oidc && !config.token && (
            <p className="text-muted-foreground">
              Sign-in isn't set up on this proxy. Configure <code className="font-mono text-[13px] text-foreground">admin.oidc</code> or{" "}
              <code className="font-mono text-[13px] text-foreground">admin.token</code> in its config.
            </p>
          )}
          {oidc && (
            <Button asChild size="lg">
              <a href={auth.loginURL(next)}>
                <LogIn /> Continue with {oidc.name}
              </a>
            </Button>
          )}
          {oidc && config?.token && !showToken && (
            <button type="button" onClick={() => setShowToken(true)} className="text-sm text-muted-foreground underline-offset-4 hover:text-foreground hover:underline">
              Use the break-glass admin token
            </button>
          )}
          {tokenForm && (
            <form onSubmit={submit} className={cn("grid gap-4", oidc && "border-t pt-4")}>
              <div className="grid gap-2">
                <Label htmlFor="token">Admin token</Label>
                <Input id="token" type="password" autoFocus={!oidc || showToken} value={value} onChange={(e) => setValue(e.target.value)} />
              </div>
              <Button type="submit" size="lg" variant={oidc ? "outline" : "default"} disabled={!value.trim() || busy}>
                {busy ? "Checking…" : "Continue"}
              </Button>
            </form>
          )}
        </div>
      </div>
    </div>
  );
}

function Dashboard({ me, onSignOut }: { me: Principal; onSignOut: () => void }) {
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
              <span className="hidden max-w-[220px] truncate text-[13px] text-muted-foreground md:inline" title={me.email}>
                {me.method === "token" ? "Admin token" : (me.email ?? me.name)}
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
