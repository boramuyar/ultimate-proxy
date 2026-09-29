import { createContext, useCallback, useContext, useEffect, useState } from "react";
import type { Application, Tenant } from "./api";

export interface Async<T> {
  data: T | undefined;
  error: string | undefined;
  loading: boolean;
  reload: () => void;
}

// useAsync runs fn whenever deps change and keeps the last good data while
// reloading, so views don't flash empty.
export function useAsync<T>(fn: () => Promise<T>, deps: unknown[]): Async<T> {
  const [data, setData] = useState<T>();
  const [error, setError] = useState<string>();
  const [loading, setLoading] = useState(true);
  const [tick, setTick] = useState(0);
  const reload = useCallback(() => setTick((t) => t + 1), []);

  useEffect(() => {
    let live = true;
    setLoading(true);
    fn()
      .then((d) => {
        if (!live) return;
        setData(d);
        setError(undefined);
      })
      .catch((e: Error) => live && setError(e.message))
      .finally(() => live && setLoading(false));
    return () => {
      live = false;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, tick]);

  return { data, error, loading, reload };
}

export type RangeKey = "24h" | "7d" | "30d";

export const RANGES: Record<RangeKey, { label: string; ms: number; granularity: "hour" | "day" }> = {
  "24h": { label: "24 hours", ms: 24 * 3600e3, granularity: "hour" },
  "7d": { label: "7 days", ms: 7 * 24 * 3600e3, granularity: "day" },
  "30d": { label: "30 days", ms: 30 * 24 * 3600e3, granularity: "day" },
};

// rangeBounds returns [from, to] for a range, with "to" rounded up to the next
// minute so repeated renders reuse the same query.
export function rangeBounds(key: RangeKey): [Date, Date] {
  const to = new Date(Math.ceil(Date.now() / 60_000) * 60_000);
  return [new Date(to.getTime() - RANGES[key].ms), to];
}

export interface Directory {
  tenants: Tenant[];
  apps: Application[];
  tenantName: (id: string) => string;
  appName: (id: string) => string;
  reload: () => void;
}

export const DirectoryContext = createContext<Directory>({
  tenants: [],
  apps: [],
  tenantName: (id) => id,
  appName: (id) => id,
  reload: () => {},
});

export const useDirectory = () => useContext(DirectoryContext);
