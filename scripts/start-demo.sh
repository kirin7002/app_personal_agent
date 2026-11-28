#!/usr/bin/env bash
# Start the local Demo stack: Go Vehicle Agent Server + Hermes Gateway (with turn_binding_adapter).
#
# Required environment variables:
#   DASHSCOPE_API_KEY   Bailian API key (used by Hermes LLM and Qwen Realtime voice gateway)
#   HERMES_PYTHON       Python binary inside the pinned Hermes venv (vgit.72c8803 / mcp==2.3.0)
#   HERMES_AGENT_DIR    Path to the pinned Hermes agent repo (commit 72c88037)
#
# Optional environment variables:
#   LISTEN_ADDR         Public HTTP/WS gateway address (default: 127.0.0.1:8080)
#   MCP_LISTEN_ADDR     Internal Go MCP address (default: 127.0.0.1:18080)
#   HERMES_PORT         Hermes Runs API port (default: 8642)
#   SQLITE_PATH         SQLite database path (default: ./data/vehicle-demo.sqlite)
#   DEMO_AUTH_TOKEN     Demo Bearer token for /voice/* and car-sim (default: demo-token)
#   SIM_MODE            Simulated vehicle mode: auto_complete|accepted_only (default: auto_complete)
#   TTS_MODE            TTS mode: auto|say|qwen|tone (default: auto)

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_ROOT"

LISTEN_ADDR="${LISTEN_ADDR:-127.0.0.1:8080}"
MCP_LISTEN_ADDR="${MCP_LISTEN_ADDR:-127.0.0.1:18080}"
HERMES_PORT="${HERMES_PORT:-8642}"
SQLITE_PATH="${SQLITE_PATH:-$REPO_ROOT/data/vehicle-demo.sqlite}"
DEMO_AUTH_TOKEN="${DEMO_AUTH_TOKEN:-demo-token}"
SIM_MODE="${SIM_MODE:-auto_complete}"
TTS_MODE="${TTS_MODE:-auto}"

VEHICLE_MCP_SERVICE_TOKEN="${VEHICLE_MCP_SERVICE_TOKEN:-demo-mcp-service-token}"
HERMES_API_KEY="${HERMES_API_KEY:-demo-hermes-api-key}"

HERMES_PYTHON="${HERMES_PYTHON:-${H07_HERMES_PYTHON:-}}"
HERMES_AGENT_DIR="${HERMES_AGENT_DIR:-${H07_HERMES_AGENT_DIR:-}}"

if [[ -z "${DASHSCOPE_API_KEY:-}" ]]; then
  echo "ERROR: DASHSCOPE_API_KEY is required." >&2
  exit 1
fi
if [[ -z "$HERMES_PYTHON" || ! -x "$HERMES_PYTHON" ]]; then
  echo "ERROR: Set HERMES_PYTHON to the pinned Hermes venv python executable." >&2
  exit 1
fi
if [[ -z "$HERMES_AGENT_DIR" || ! -d "$HERMES_AGENT_DIR" ]]; then
  echo "ERROR: Set HERMES_AGENT_DIR to the pinned Hermes repository directory." >&2
  exit 1
fi

RUNTIME_DIR="$REPO_ROOT/.runtime/demo"
HERMES_HOME="$RUNTIME_DIR/hermes-home"
mkdir -p "$HERMES_HOME" "$(dirname "$SQLITE_PATH")"

cat > "$HERMES_HOME/config.yaml" <<YAML
model:
  default: deepseek-v4.1-flash-1017
  provider: custom:vehicle-bailian
  context_length: 65536

providers:
  vehicle-bailian:
    api: https://dashscope.aliyuncs.com/apps/anthropic
    key_env: DASHSCOPE_API_KEY
    transport: anthropic_messages
    default_model: deepseek-v4.1-flash-1017
    context_length: 65536
    discover_models: false

auth:
  adopt_external_logins: false

updates:
  check: false

memory:
  memory_enabled: false
  user_profile_enabled: false

agent:
  max_turns: 8

tools:
  tool_search:
    enabled: "off"

toolsets:
  - mcp-vehicle

platform_toolsets:
  api_server:
    - mcp-vehicle

mcp_servers:
  vehicle:
    url: http://${MCP_LISTEN_ADDR}/mcp
    headers:
      Authorization: "Bearer ${VEHICLE_MCP_SERVICE_TOKEN}"
    strict_redirect_headers: true
    timeout: 15
    connect_timeout: 10
    trust: full
    sampling:
      enabled: false
    include_tools:
      - create_vehicle_task
      - list_vehicle_tasks
      - dispatch_vehicle_command
      - get_vehicle_status
      - search_vehicle_manual
      - search_vehicle_capabilities

gateway:
  api_server:
    enabled: true
    host: 127.0.0.1
    port: ${HERMES_PORT}
    max_concurrent_runs: 8
YAML

GO_PID=""
HERMES_PID=""

cleanup() {
  echo ""
  echo "Stopping Demo services..."
  if [[ -n "$HERMES_PID" ]] && kill -0 "$HERMES_PID" 2>/dev/null; then
    kill "$HERMES_PID" 2>/dev/null || true
    wait "$HERMES_PID" 2>/dev/null || true
  fi
  if [[ -n "$GO_PID" ]] && kill -0 "$GO_PID" 2>/dev/null; then
    kill "$GO_PID" 2>/dev/null || true
    wait "$GO_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT INT TERM

echo "[1/3] Building Go demo server..."
go build -o "$RUNTIME_DIR/vehicle-server" ./cmd/server

echo "[2/3] Starting Go demo server (public=$LISTEN_ADDR, mcp=$MCP_LISTEN_ADDR, sqlite=$SQLITE_PATH)..."
LISTEN_ADDR="$LISTEN_ADDR" \
MCP_LISTEN_ADDR="$MCP_LISTEN_ADDR" \
SQLITE_PATH="$SQLITE_PATH" \
HERMES_BASE_URL="http://127.0.0.1:${HERMES_PORT}" \
HERMES_API_KEY="$HERMES_API_KEY" \
VEHICLE_MCP_SERVICE_TOKEN="$VEHICLE_MCP_SERVICE_TOKEN" \
DEMO_AUTH_TOKEN="$DEMO_AUTH_TOKEN" \
SIM_MODE="$SIM_MODE" \
TTS_MODE="$TTS_MODE" \
"$RUNTIME_DIR/vehicle-server" > "$RUNTIME_DIR/server.log" 2>&1 &
GO_PID=$!

for _ in $(seq 1 40); do
  if curl -fsS "http://${LISTEN_ADDR}/healthz" >/dev/null 2>&1; then
    break
  fi
  if ! kill -0 "$GO_PID" 2>/dev/null; then
    echo "Go server exited early. Log:" >&2
    cat "$RUNTIME_DIR/server.log" >&2
    exit 1
  fi
  sleep 0.25
done

echo "[3/3] Starting Hermes gateway (port=$HERMES_PORT)..."
HERMES_HOME="$HERMES_HOME" \
HERMES_AGENT_DIR="$HERMES_AGENT_DIR" \
PYTHONPATH="${HERMES_AGENT_DIR}:${REPO_ROOT}" \
API_SERVER_KEY="$HERMES_API_KEY" \
API_SERVER_ENABLED=true \
PYTHONUNBUFFERED=1 \
PYTHONDONTWRITEBYTECODE=1 \
"$HERMES_PYTHON" -m hermes.gateway_launcher > "$RUNTIME_DIR/hermes.log" 2>&1 &
HERMES_PID=$!

for _ in $(seq 1 120); do
  if curl -fsS "http://127.0.0.1:${HERMES_PORT}/health" >/dev/null 2>&1; then
    break
  fi
  if ! kill -0 "$HERMES_PID" 2>/dev/null; then
    echo "Hermes gateway exited early. Log:" >&2
    cat "$RUNTIME_DIR/hermes.log" >&2
    exit 1
  fi
  sleep 0.25
done

if ! curl -fsS "http://127.0.0.1:${HERMES_PORT}/health" >/dev/null 2>&1; then
  echo "Timed out waiting for Hermes /health. Log:" >&2
  tail -n 40 "$RUNTIME_DIR/hermes.log" >&2
  exit 1
fi

echo ""
echo "Demo is READY!"
echo "  Public Gateway : http://${LISTEN_ADDR}  (WS /voice/audio, POST /voice/utterances, GET /healthz, GET /debug/state)"
echo "  Internal MCP   : http://${MCP_LISTEN_ADDR}/mcp"
echo "  Hermes API     : http://127.0.0.1:${HERMES_PORT}"
echo "  SQLite DB      : ${SQLITE_PATH}"
echo "  Logs           : ${RUNTIME_DIR}/server.log, ${RUNTIME_DIR}/hermes.log"
echo ""
echo "Try Text Demo (Turn 1 -> Turn 2):"
echo "  curl -N -X POST http://${LISTEN_ADDR}/voice/utterances \\"
echo "    -H 'Authorization: Bearer ${DEMO_AUTH_TOKEN}' -H 'Content-Type: application/json' \\"
echo "    -d '{\"session_id\":\"car-test\",\"vehicle_id\":\"sim-car\",\"request_id\":\"demo-1\",\"text\":\"把车窗关一下\"}'"
echo ""
echo "  curl -N -X POST http://${LISTEN_ADDR}/voice/utterances \\"
echo "    -H 'Authorization: Bearer ${DEMO_AUTH_TOKEN}' -H 'Content-Type: application/json' \\"
echo "    -d '{\"session_id\":\"car-test\",\"vehicle_id\":\"sim-car\",\"request_id\":\"demo-2\",\"text\":\"左后窗\"}'"
echo ""
echo "Try Voice Demo (with car-sim):"
echo "  ./scripts/car-audio-samples.sh /tmp/car-samples"
echo "  CAR_SIM_TOKEN=${DEMO_AUTH_TOKEN} go run ./cmd/car-sim -url ws://${LISTEN_ADDR}/voice/audio -wait-result -out /tmp/car-out /tmp/car-samples/close-window.pcm /tmp/car-samples/rear-left.pcm"
echo ""
echo "Press Ctrl+C to stop."

wait "$GO_PID" "$HERMES_PID"
