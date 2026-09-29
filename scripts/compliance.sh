#!/usr/bin/env bash
# Runs the Open Responses compliance suite against the proxy, with the fake
# upstream standing in for the provider.
#
#   scripts/compliance.sh <path to an openresponses/openresponses checkout>
#
# WebSocket transport and /responses/compact are not built yet, so only the
# HTTP tests run.
set -euo pipefail

SPEC_DIR=${1:?usage: scripts/compliance.sh <openresponses checkout>}
TESTS=basic-response,assistant-phase,response-output-phase-schema,streaming-response,system-prompt,tool-calling,image-input,multi-turn
BIN=$(mktemp -d)

go build -o "$BIN/ultimate-proxy" ./cmd/ultimate-proxy
go build -o "$BIN/fake-upstream" ./cmd/fake-upstream

"$BIN/fake-upstream" -listen :9090 &
FAKE=$!
"$BIN/ultimate-proxy" -config deploy/ci.config.yaml &
PROXY=$!
trap 'kill $FAKE $PROXY 2>/dev/null || true' EXIT

for _ in $(seq 50); do
  curl -sf localhost:8080/healthz >/dev/null && break
  sleep 0.2
done

(cd "$SPEC_DIR" && bun install --frozen-lockfile >/dev/null)
for model in fake-gpt; do
  echo "== compliance: $model"
  (cd "$SPEC_DIR" && bun run bin/compliance-test.ts \
    --base-url http://localhost:8080/v1 --api-key up_ci_test_key --model "$model" --filter "$TESTS")
done
