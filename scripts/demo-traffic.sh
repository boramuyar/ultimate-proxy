#!/usr/bin/env bash
# Sends demo traffic to the fake provider's gpt-fake model so the dashboard has something to
# show: a few tenants and applications, some with prompt-cache problems.
#
#   scripts/demo-traffic.sh [proxy url] [admin token]
#
# Defaults match docker-compose.yml without a .env.
set -euo pipefail

URL=${1:-http://localhost:8080}
ADMIN=${2:-${PROXY_ADMIN_TOKEN:-dev-admin-token}}
H=(-H "Authorization: Bearer $ADMIN" -H "Content-Type: application/json")

json() { sed -n "s/.*\"$1\":\"\([^\"]*\)\".*/\1/p"; }

tenant() { curl -sf "${H[@]}" "$URL/admin/tenants" -d "{\"name\":\"$1\"}" | json id; }
app() { curl -sf "${H[@]}" "$URL/admin/tenants/$1/applications" -d "{\"name\":\"$2\"}" | json id; }
key() { curl -sf "${H[@]}" -X POST "$URL/admin/applications/$1/keys" | json key; }

echo "== creating tenants, applications and keys"
acme=$(tenant acme)
globex=$(tenant globex)
support=$(key "$(app "$acme" support-bot)")
search=$(key "$(app "$acme" search)")
writer=$(key "$(app "$globex" writer)")
agent=$(key "$(app "$globex" agent)")

echo "== adding a price for gpt-fake"
curl -sf "${H[@]}" "$URL/admin/prices" \
  -d '{"model":"gpt-fake","input":1.25,"cached_input":0.125,"output":10}' >/dev/null

SYSTEM="You are a helpful assistant for a large company. Answer briefly and cite the knowledge base. $(printf 'Follow the style guide carefully. %.0s' {1..150})"

send() { # key email instructions input [stream]
  local body
  body=$(printf '{"model":"gpt-fake","instructions":"%s","input":"%s","stream":%s}' "$3" "$4" "${5:-false}")
  curl -s -o /dev/null "$URL/fake/v1/responses" -H "Authorization: Bearer $1" \
    -H "X-Proxy-User-Email: $2" -H "Content-Type: application/json" -d "$body"
}

echo "== sending traffic"
for i in $(seq 1 12); do
  # support-bot puts a timestamp at the top of its instructions: every request misses the cache.
  send "$support" "user$((i % 4))@acme.com" "Current time: $(date -u +%Y-%m-%dT%H:%M:%S.%NZ). $SYSTEM" "Where is my order $i?"
  # search sends the same prompt, but the provider keeps missing it.
  send "$search" "user$((i % 3))@acme.com" "NOCACHE $SYSTEM" "Find docs about topic $i"
  # writer is healthy apart from answers that get cut off.
  if ((i % 3 == 0)); then q="Write a long essay HIT_MAX_TOKENS"; else q="Write a haiku about $i"; fi
  send "$writer" "editor$((i % 2))@globex.com" "$SYSTEM" "$q"
  # agent caches well but some streams fail.
  if ((i % 4 == 0)); then q="Run the task FAIL_MIDSTREAM"; else q="Plan step $i"; fi
  send "$agent" "ops@globex.com" "$SYSTEM" "$q" true
done

echo "done. Insights appear within a minute of the next evaluation."
