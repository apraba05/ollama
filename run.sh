#!/usr/bin/env bash
# One-command demo: Compose (Ollama+Redis) → Go /api/generate proxy →
# local hit, Bedrock fallback (if creds), Redis offline cache.
set -euo pipefail
cd "$(dirname "$0")"
ROOT="$(pwd)"
BIN="${ROOT}/.bin"
mkdir -p "$BIN"
export PATH="$BIN:$PATH"

PROXY_PORT=11435
PROXY_PID=""
COMPOSE=()
OLLAMA_MODEL="${OLLAMA_MODEL:-llama3.2:1b}"
PROMPT="${DEMO_PROMPT:-Say hello in one short sentence.}"

need_docker() {
  if docker info >/dev/null 2>&1; then
    return 0
  fi
  if sg docker -c 'docker info' >/dev/null 2>&1; then
    exec sg docker -c "\"$0\" $*"
  fi
  echo "Docker is required (and your user must reach the docker socket)." >&2
  exit 1
}

ensure_compose() {
  if docker compose version >/dev/null 2>&1; then
    COMPOSE=(docker compose)
    return
  fi
  if command -v docker-compose >/dev/null 2>&1; then
    COMPOSE=(docker-compose)
    return
  fi
  local dest="${BIN}/docker-compose"
  if [[ ! -x "$dest" ]]; then
    echo "==> installing docker-compose into .bin/"
    curl -fsSL \
      "https://github.com/docker/compose/releases/download/v2.29.7/docker-compose-linux-x86_64" \
      -o "$dest"
    chmod +x "$dest"
  fi
  COMPOSE=("$dest")
}

cleanup() {
  [[ -n "${PROXY_PID}" ]] && kill "${PROXY_PID}" 2>/dev/null || true
  if [[ ${#COMPOSE[@]} -gt 0 ]]; then
    "${COMPOSE[@]}" down >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

need_docker "$@"
ensure_compose

export AWS_REGION="${AWS_REGION:-${AWS_DEFAULT_REGION:-us-east-1}}"
export AWS_DEFAULT_REGION="$AWS_REGION"
export BEDROCK_MODEL="${BEDROCK_MODEL:-anthropic.claude-3-haiku-20240307-v1:0}"
export OLLAMA_MODEL

HAS_AWS=0
if command -v aws >/dev/null 2>&1 && aws sts get-caller-identity >/dev/null 2>&1; then
  HAS_AWS=1
  echo "==> AWS credentials OK (Bedrock fallback enabled)"
else
  echo "==> WARNING: no AWS credentials — Bedrock tier will miss; offline cache still demos after local hit"
fi

echo "==> docker compose up (ollama + redis)"
"${COMPOSE[@]}" down >/dev/null 2>&1 || true
"${COMPOSE[@]}" up -d

echo "==> wait for Redis"
for _ in $(seq 1 60); do
  if "${COMPOSE[@]}" exec -T redis redis-cli PING 2>/dev/null | grep -q PONG; then
    break
  fi
  sleep 0.5
done
"${COMPOSE[@]}" exec -T redis redis-cli PING | grep -q PONG

echo "==> wait for Ollama API"
for _ in $(seq 1 60); do
  if curl -sf http://127.0.0.1:11434/api/tags >/dev/null; then
    break
  fi
  sleep 1
done
curl -sf http://127.0.0.1:11434/api/tags >/dev/null

echo "==> pull model ${OLLAMA_MODEL} (first run downloads weights)"
"${COMPOSE[@]}" exec -T ollama ollama pull "${OLLAMA_MODEL}"

echo "==> go build proxy"
go mod tidy
CGO_ENABLED=0 go build -o fallback-proxy .
go test ./...

echo "==> start proxy :${PROXY_PORT}"
LISTEN=":${PROXY_PORT}" \
  REDIS_ADDR=127.0.0.1:26379 \
  OLLAMA_URL=http://127.0.0.1:11434 \
  OLLAMA_MODEL="${OLLAMA_MODEL}" \
  BEDROCK_MODEL="${BEDROCK_MODEL}" \
  AWS_REGION="${AWS_REGION}" \
  ./fallback-proxy > /tmp/ollama-fallback-proxy.log 2>&1 &
PROXY_PID=$!

for _ in $(seq 1 40); do
  if curl -sf "http://127.0.0.1:${PROXY_PORT}/health" >/dev/null; then
    break
  fi
  sleep 0.25
done
if ! curl -sf "http://127.0.0.1:${PROXY_PORT}/health" >/dev/null; then
  echo "proxy failed to start:" >&2
  cat /tmp/ollama-fallback-proxy.log >&2 || true
  exit 1
fi

post_generate() {
  curl -sS -X POST "http://127.0.0.1:${PROXY_PORT}/api/generate" \
    -H 'Content-Type: application/json' \
    -d "$(python3 -c "import json,sys; print(json.dumps({'model':sys.argv[1],'prompt':sys.argv[2],'stream':False}))" "$OLLAMA_MODEL" "$PROMPT")"
}

echo
echo "=== 1) Local Ollama (source=ollama) ==="
RESP1="$(post_generate)"
echo "$RESP1" | python3 -m json.tool
echo "$RESP1" | python3 -c "import json,sys; d=json.load(sys.stdin); assert d.get('source')=='ollama' and d.get('done') and d.get('response'), d"

echo
echo "=== 2) Stop Ollama → Bedrock fallback (source=bedrock) ==="
"${COMPOSE[@]}" stop ollama >/dev/null
sleep 1
RESP2="$(post_generate || true)"
echo "$RESP2" | python3 -m json.tool 2>/dev/null || echo "$RESP2"
SRC2="$(echo "$RESP2" | python3 -c "import json,sys; print(json.load(sys.stdin).get('source',''))" 2>/dev/null || true)"
if [[ "$SRC2" == "bedrock" ]]; then
  echo "(Bedrock fallback OK)"
elif [[ "$SRC2" == "offline" && "$HAS_AWS" -eq 0 ]]; then
  echo "(no AWS creds — ladder skipped Bedrock and served Redis offline cache)"
  echo
  echo "=== Demo complete (local + offline). Configure AWS for the Bedrock mid-tier. ==="
  echo "Proxy log: /tmp/ollama-fallback-proxy.log"
  exit 0
elif [[ "$HAS_AWS" -eq 0 ]]; then
  echo "expected offline cache after local hit without AWS; got source=${SRC2:-none}" >&2
  exit 1
else
  echo "Bedrock fallback failed despite AWS credentials. See /tmp/ollama-fallback-proxy.log" >&2
  exit 1
fi

echo
echo "=== 3) Block Bedrock path → Redis offline (source=offline) ==="
# Point the running process at a dead Bedrock region by restarting with bad endpoint
# is heavy; instead unset reachability by using a bogus model id via restart.
kill "${PROXY_PID}" 2>/dev/null || true
wait "${PROXY_PID}" 2>/dev/null || true
PROXY_PID=""
LISTEN=":${PROXY_PORT}" \
  REDIS_ADDR=127.0.0.1:26379 \
  OLLAMA_URL=http://127.0.0.1:11434 \
  OLLAMA_MODEL="${OLLAMA_MODEL}" \
  BEDROCK_MODEL="anthropic.claude-3-haiku-20240307-v1:0-offline-demo-invalid" \
  AWS_REGION="${AWS_REGION}" \
  ./fallback-proxy > /tmp/ollama-fallback-proxy.log 2>&1 &
PROXY_PID=$!
for _ in $(seq 1 40); do
  if curl -sf "http://127.0.0.1:${PROXY_PORT}/health" >/dev/null; then
    break
  fi
  sleep 0.25
done

RESP3="$(post_generate)"
echo "$RESP3" | python3 -m json.tool
echo "$RESP3" | python3 -c "import json,sys; d=json.load(sys.stdin); assert d.get('source')=='offline' and d.get('response'), d"

echo
echo "=== Demo complete: ollama → bedrock → offline ==="
echo "Proxy log: /tmp/ollama-fallback-proxy.log"
