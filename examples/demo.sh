#!/usr/bin/env bash
# End-to-end demo with no cluster and no real Prometheus.
#
# Starts the fixture-backed fake Prometheus, runs k8s-resource-recommender
# against it, and prints the patch it produces. Doubles as a smoke test in CI.
set -euo pipefail

cd "$(dirname "$0")/.."

PORT="${PORT:-19090}"

cleanup() {
  if [[ -n "${PROM_PID:-}" ]]; then
    kill "$PROM_PID" 2>/dev/null || true
    wait "$PROM_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

python3 examples/fake-prometheus.py "$PORT" &
PROM_PID=$!

# Wait for the port to accept connections instead of sleeping a fixed amount.
for _ in $(seq 50); do
  if python3 -c "
import socket,sys
s=socket.socket()
s.settimeout(0.2)
sys.exit(0 if s.connect_ex(('127.0.0.1', $PORT)) == 0 else 1)
" 2>/dev/null; then
    break
  fi
  sleep 0.1
done

go build -o /tmp/k8s-resource-recommender-demo ./cmd/k8s-resource-recommender

echo "== recommended patch for namespace 'prod' =="
/tmp/k8s-resource-recommender-demo \
  --prom "http://127.0.0.1:$PORT" \
  --namespace prod \
  --window 7d \
  | tee /tmp/k8s-resource-recommender-demo.yaml

echo
echo "== same run with CPU limits omitted (--no-cpu-limit) =="
/tmp/k8s-resource-recommender-demo \
  --prom "http://127.0.0.1:$PORT" \
  --namespace prod \
  --no-cpu-limit

# The patch must be structurally valid. With a cluster you would run:
#   kubectl patch deployment api -n prod --patch-file <file> --dry-run=server
grep -q 'kind: Deployment' /tmp/k8s-resource-recommender-demo.yaml
grep -q 'cpu: 250m' /tmp/k8s-resource-recommender-demo.yaml
echo
echo "demo OK"
