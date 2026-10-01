const compact = new Intl.NumberFormat("en", { notation: "compact", maximumFractionDigits: 1 });
const whole = new Intl.NumberFormat("en");

export const fmtNumber = (n: number) => (Math.abs(n) >= 10_000 ? compact.format(n) : whole.format(Math.round(n)));

export function fmtUSD(n: number): string {
  if (n === 0) return "$0";
  if (Math.abs(n) < 0.01) return `$${n.toFixed(4)}`;
  if (Math.abs(n) >= 10_000) return `$${compact.format(n)}`;
  return `$${n.toFixed(2)}`;
}

export const fmtPct = (n: number) => `${(n * 100).toFixed(n > 0 && n < 0.1 ? 1 : 0)}%`;

export function fmtTime(iso: string): string {
  return new Date(iso).toLocaleString(undefined, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
}

export function fmtAgo(iso: string): string {
  const s = (Date.now() - new Date(iso).getTime()) / 1000;
  if (s < 60) return "just now";
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  return `${Math.floor(s / 86400)}d ago`;
}

export const hitRate = (cached: number, input: number) => (input > 0 ? cached / input : 0);

// Plain-language labels for the proxy's cache statuses.
export const CACHE_STATUS: Record<string, { label: string; tone: "good" | "bad" | "warn" | "neutral"; help: string }> = {
  hit: { label: "Hit", tone: "good", help: "The provider served part of the prompt from cache." },
  miss_unexpected: {
    label: "Unexpected miss",
    tone: "bad",
    help: "The same prefix was sent recently, but the provider still missed. Set prompt_cache_key and keep traffic on one account and region.",
  },
  miss_instructions_dynamic: {
    label: "Dynamic instructions",
    tone: "bad",
    help: "The instructions change only in numbers or ids, such as a timestamp. Move that value to the end of the input.",
  },
  miss_instructions_changed: {
    label: "Instructions changed",
    tone: "warn",
    help: "The instructions differ between requests. Put the stable part first.",
  },
  miss_tools_reordered: {
    label: "Tools reordered",
    tone: "bad",
    help: "The same tools arrive in a different order or key order. Serialize them deterministically.",
  },
  miss_tools_changed: { label: "Tools changed", tone: "warn", help: "The tool list changes between requests." },
  miss_history_rewritten: {
    label: "History rewritten",
    tone: "warn",
    help: "Earlier turns change between requests. Append turns instead of rewriting them.",
  },
  miss_new_prefix: { label: "New prompt", tone: "neutral", help: "Nothing similar was sent recently. Normal for first requests." },
  miss_too_short: { label: "Too short", tone: "neutral", help: "Below the provider's minimum cacheable size." },
  unknown: { label: "Unknown", tone: "neutral", help: "Uses previous_response_id, so the prompt isn't visible." },
  "": { label: "Not classified", tone: "neutral", help: "Failed requests, or requests from before cache tracking." },
};

export const cacheLabel = (s: string) => CACHE_STATUS[s]?.label ?? s;
