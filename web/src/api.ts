// Typed client for the proxy's admin API.

const TOKEN_KEY = "up_admin_token";

export function getToken(): string | null {
  try {
    return localStorage.getItem(TOKEN_KEY);
  } catch {
    return null;
  }
}

export function setToken(token: string | null) {
  try {
    if (token) localStorage.setItem(TOKEN_KEY, token);
    else localStorage.removeItem(TOKEN_KEY);
  } catch {
    /* storage unavailable: the token lives for this page only */
  }
}

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
  ) {
    super(message);
  }
}

let onUnauthorized: () => void = () => {};
export function setUnauthorizedHandler(fn: () => void) {
  onUnauthorized = fn;
}

async function request<T>(method: string, path: string, body?: unknown, token = getToken()): Promise<T> {
  const res = await fetch(path, {
    method,
    headers: {
      Authorization: `Bearer ${token ?? ""}`,
      ...(body !== undefined ? { "Content-Type": "application/json" } : {}),
    },
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  if (res.status === 401) {
    onUnauthorized();
    throw new ApiError(401, "Your admin token was rejected.");
  }
  if (!res.ok) {
    let message = `${res.status} ${res.statusText}`;
    try {
      const err = await res.json();
      message = err?.error?.message ?? message;
    } catch {
      /* not JSON */
    }
    throw new ApiError(res.status, message);
  }
  if (res.status === 204) return undefined as T;
  return res.json();
}

export interface Tenant {
  id: string;
  name: string;
  created_at: string;
}

export interface Application {
  id: string;
  tenant_id: string;
  name: string;
  can_assert_users: boolean;
  created_at: string;
}

export interface ApiKey {
  id: string;
  application_id: string;
  prefix: string;
  created_at: string;
  revoked_at?: string;
  key?: string; // only when just created
}

export type Dimension = "tenant" | "application" | "email" | "model" | "provider" | "cache";

export interface UsageRow {
  bucket?: string;
  group: Record<string, string>;
  requests: number;
  failed_requests: number;
  input_tokens: number;
  cached_input_tokens: number;
  cache_write_tokens: number;
  output_tokens: number;
  reasoning_tokens: number;
  total_tokens: number;
  cost_usd: number;
}

export interface UsageParams {
  from: Date;
  to: Date;
  groupBy: Dimension[];
  granularity?: "hour" | "day";
  filters?: Partial<Record<"tenant_id" | "application_id" | "email" | "model" | "provider" | "cache_status", string>>;
}

export interface Insight {
  id: string;
  kind: string;
  severity: "warning" | "critical";
  status: "open" | "resolved";
  tenant_id: string;
  application_id: string;
  model: string;
  title: string;
  detail: string;
  evidence: Record<string, unknown>;
  first_seen: string;
  last_seen: string;
  resolved_at?: string;
}

export interface Price {
  id: string;
  model: string;
  input: number;
  cached_input: number;
  cache_write: number;
  output: number;
  effective_from: string;
  created_at: string;
}

export interface NewPrice {
  model: string;
  input: number;
  cached_input?: number;
  cache_write?: number;
  output: number;
  effective_from?: string;
}

type List<T> = { data: T[] };

export const api = {
  // Checks a token without storing it.
  check: (token: string) => request<List<Tenant>>("GET", "/admin/tenants", undefined, token),

  tenants: () => request<List<Tenant>>("GET", "/admin/tenants").then((r) => r.data),
  createTenant: (name: string) => request<Tenant>("POST", "/admin/tenants", { name }),
  applications: () => request<List<Application>>("GET", "/admin/applications").then((r) => r.data),
  createApplication: (tenantId: string, name: string, canAssertUsers: boolean) =>
    request<Application>("POST", `/admin/tenants/${tenantId}/applications`, { name, can_assert_users: canAssertUsers }),
  keys: (appId: string) => request<List<ApiKey>>("GET", `/admin/applications/${appId}/keys`).then((r) => r.data),
  createKey: (appId: string) => request<ApiKey>("POST", `/admin/applications/${appId}/keys`),
  revokeKey: (keyId: string) => request<void>("DELETE", `/admin/keys/${keyId}`),

  usage: (p: UsageParams) => {
    const qs = new URLSearchParams({
      from: p.from.toISOString(),
      to: p.to.toISOString(),
      group_by: p.groupBy.length ? p.groupBy.join(",") : "none",
    });
    if (p.granularity) qs.set("granularity", p.granularity);
    for (const [k, v] of Object.entries(p.filters ?? {})) if (v) qs.set(k, v);
    return request<List<UsageRow>>("GET", `/admin/usage?${qs}`).then((r) => r.data);
  },

  insights: (status: "open" | "resolved" | "all") =>
    request<List<Insight>>("GET", `/admin/insights?status=${status}`).then((r) => r.data),

  prices: (current: boolean) =>
    request<List<Price>>("GET", `/admin/prices${current ? "?current=true" : ""}`).then((r) => r.data),
  addPrice: (p: NewPrice) => request<Price>("POST", "/admin/prices", p),
};
