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

echo
echo "== current vs recommended (--format table) =="
/tmp/k8s-resource-recommender-demo \
  --prom "http://127.0.0.1:$PORT" \
  --namespace prod \
  --format table \
  --cpu-cost 20 --mem-cost 2.5 \
  | tee /tmp/k8s-resource-recommender-demo.txt

# The patch must be structurally valid. With a cluster you would run:
#   kubectl patch deployment api -n prod --patch-file <file> --dry-run=server
grep -q 'kind: Deployment' /tmp/k8s-resource-recommender-demo.yaml
grep -q 'cpu: 250m' /tmp/k8s-resource-recommender-demo.yaml
grep -q 'current: requests cpu 2 memory 64Mi' /tmp/k8s-resource-recommender-demo.yaml
grep -q '^# reclaimable: 0.96 CPU, 448Mi' /tmp/k8s-resource-recommender-demo.yaml
grep -Eq '^prod/api +envoy +500m->20m +256Mi->32Mi +over$' /tmp/k8s-resource-recommender-demo.txt

echo
echo "== rewrite a manifest checkout (--write-manifests) =="
MANIFESTS="$(mktemp -d)"
cp examples/fixtures/base-deployment.yaml "$MANIFESTS/api.yaml"
set +e
/tmp/k8s-resource-recommender-demo \
  --prom "http://127.0.0.1:$PORT" \
  --namespace prod \
  --write-manifests "$MANIFESTS"
rc=$?
set -e
# Exit 3 means files changed, which is what a scheduled job keys the PR on.
[[ $rc -eq 3 ]] || { echo "want exit 3 from --write-manifests, got $rc"; exit 1; }
diff -u examples/fixtures/base-deployment.yaml "$MANIFESTS/api.yaml" || true
grep -q '              cpu: 250m' "$MANIFESTS/api.yaml"
grep -q '^# A deliberately badly-sized Deployment' "$MANIFESTS/api.yaml"
# A second run has nothing left to change.
/tmp/k8s-resource-recommender-demo --prom "http://127.0.0.1:$PORT" --namespace prod --write-manifests "$MANIFESTS" 2>/dev/null
rm -rf "$MANIFESTS"

echo
echo "demo OK"
