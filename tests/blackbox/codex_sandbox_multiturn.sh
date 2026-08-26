#!/usr/bin/env bash
set -euo pipefail

gateway_port="${GATEWAY_CODEX_PORT:-18080}"
mock_port="${MOCK_OPENAI_PORT:-18082}"
gateway_url="http://127.0.0.1:${gateway_port}/v1"
workspace="$(mktemp -d /tmp/llm-gateway-codex.XXXXXX)"
service_log="$(mktemp /tmp/llm-gateway-codex-services.XXXXXX)"

cleanup() {
  kill "${gateway_pid:-}" "${mock_pid:-}" 2>/dev/null || true
}
trap cleanup EXIT

command -v codex >/dev/null
command -v jq >/dev/null
git -C "$workspace" init -q

MOCK_OPENAI_PORT="$mock_port" python3 tests/blackbox/mock_openai_responses.py >>"$service_log" 2>&1 &
mock_pid=$!

export GATEWAY_ADDR="127.0.0.1:${gateway_port}"
export GATEWAY_DEV_MEMORY_STORE=true
export GATEWAY_API_KEYS_JSON='{"codex-token":"tenant-codex"}'
export MOCK_OPENAI_API_KEY=mock
export GATEWAY_ROUTES_JSON="[{\"id\":\"openai-mock\",\"provider\":\"openai\",\"public_model\":\"codex-mock\",\"provider_model\":\"codex-mock\",\"base_url\":\"http://127.0.0.1:${mock_port}/v1\",\"api_key_env\":\"MOCK_OPENAI_API_KEY\",\"region\":\"local\",\"home_region\":\"local\",\"credential_scope\":\"mock\",\"healthy\":true,\"capability_revision\":1,\"capabilities\":{\"text\":\"native\",\"streaming\":\"native\",\"tools\":\"native\",\"sampling\":\"native\",\"multimodal\":\"native\",\"reasoning\":\"native\",\"responses_native\":\"native\",\"end_user_id\":\"native\"},\"price_snapshot_id\":\"mock-price\",\"price_effective_at\":\"2026-08-25T00:00:00Z\",\"price_source\":\"mock\",\"currency\":\"USD\",\"input_cost_per_million\":0,\"cached_input_cost_per_million\":0,\"cache_write_cost_per_million\":0,\"cache_usage_reliable\":true,\"output_cost_per_million\":0}]"
GOCACHE="${GOCACHE:-/tmp/llm_gateway-go-cache}" go run ./cmd/llm-gateway >>"$service_log" 2>&1 &
gateway_pid=$!

for _ in $(seq 1 100); do
  if curl -fsS "http://127.0.0.1:${gateway_port}/healthz" >/dev/null 2>&1; then
    break
  fi
  sleep 0.05
done
curl -fsS "http://127.0.0.1:${gateway_port}/healthz" >/dev/null || {
  sed -n '1,160p' "$service_log"
  exit 1
}

provider_config="model_provider=\"llm_gateway\""
provider_definition="model_providers.llm_gateway={name=\"Local LLM Gateway\",base_url=\"${gateway_url}\",env_key=\"LLM_GATEWAY_API_KEY\",wire_api=\"responses\",request_max_retries=0,stream_max_retries=0}"
export LLM_GATEWAY_API_KEY=codex-token

turn1="$(codex -a never -s workspace-write exec --ignore-user-config -C "$workspace" -m codex-mock \
  -c "$provider_config" -c "$provider_definition" --json -o "$workspace/turn1.txt" \
  'Create codex-state.txt with one line turn1, then reply TURN_1_DONE.')"
thread_id="$(printf '%s\n' "$turn1" | jq -r 'select(.type == "thread.started") | .thread_id' | head -1)"
test -n "$thread_id"

turn2="$(cd "$workspace" && codex -a never -s workspace-write exec resume --ignore-user-config -m codex-mock \
  -c "$provider_config" -c "$provider_definition" --json -o "$workspace/turn2.txt" "$thread_id" \
  'Append one line turn2 to codex-state.txt, then reply TURN_2_DONE.')"
turn3="$(cd "$workspace" && codex -a never -s workspace-write exec resume --ignore-user-config -m codex-mock \
  -c "$provider_config" -c "$provider_definition" --json -o "$workspace/turn3.txt" "$thread_id" \
  'Read codex-state.txt, verify it contains exactly turn1 and turn2, then reply TURN_3_DONE.')"

for events in "$turn1" "$turn2" "$turn3"; do
  if printf '%s\n' "$events" | jq -e 'select(.type == "item.completed" and .item.type == "error")' >/dev/null; then
    printf '%s\n' "$events"
    exit 1
  fi
done

test "$(sed -n '1p' "$workspace/turn1.txt")" = "TURN_1_DONE"
test "$(sed -n '1p' "$workspace/turn2.txt")" = "TURN_2_DONE"
test "$(sed -n '1p' "$workspace/turn3.txt")" = "TURN_3_DONE"
test "$(sed -n '1p' "$workspace/codex-state.txt")" = "turn1"
test "$(sed -n '2p' "$workspace/codex-state.txt")" = "turn2"
test "$(wc -l < "$workspace/codex-state.txt" | tr -d ' ')" = "2"

echo "PASS codex-sandbox-multiturn thread_id=${thread_id} workspace=${workspace}"
