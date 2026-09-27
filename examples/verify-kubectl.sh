#!/usr/bin/env bash
# Proves the acceptance criterion: the generated patch applies cleanly with
# kubectl. `--local` runs the real strategic-merge decoder against a file, so
# this needs no cluster and no kubeconfig.
set -euo pipefail

cd "$(dirname "$0")/.."

PORT="${PORT:-19091}"
KUBECTL="${KUBECTL:-kubectl}"
WORK="$(mktemp -d)"

cleanup() {
  if [[ -n "${PROM_PID:-}" ]]; then
    kill "$PROM_PID" 2>/dev/null || true
    wait "$PROM_PID" 2>/dev/null || true
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT

python3 examples/fake-prometheus.py "$PORT" &
PROM_PID=$!

for _ in $(seq 50); do
  if python3 -c "
import socket,sys
s=socket.socket(); s.settimeout(0.2)
sys.exit(0 if s.connect_ex(('127.0.0.1', $PORT)) == 0 else 1)
" 2>/dev/null; then
    break
  fi
  sleep 0.1
done

go build -o "$WORK/k8s-resource-recommender" ./cmd/k8s-resource-recommender

# Keep only the prod/api document; the patch file must hold one object.
"$WORK/k8s-resource-recommender" \
  --prom "http://127.0.0.1:$PORT" \
  --namespace prod \
  --window 7d \
  | awk '/^---$/{exit} {print}' > "$WORK/patch.yaml"

echo "== generated patch =="
cat "$WORK/patch.yaml"

echo
echo "== kubectl patch --local --type strategic =="
"$KUBECTL" patch \
  --local \
  --type strategic \
  --filename examples/fixtures/base-deployment.yaml \
  --patch-file "$WORK/patch.yaml" \
  --output yaml

# Same merge again as JSON, so the assertions below can read it with the Python
# standard library instead of a YAML dependency.
"$KUBECTL" patch \
  --local \
  --type strategic \
  --filename examples/fixtures/base-deployment.yaml \
  --patch-file "$WORK/patch.yaml" \
  --output json > "$WORK/patched.json"

echo
echo "== assertions =="
python3 - "$WORK/patched.json" <<'PY'
import json
import sys

d = json.load(open(sys.argv[1]))
containers = {c["name"]: c for c in d["spec"]["template"]["spec"]["containers"]}
app, envoy = containers["app"], containers["envoy"]

checks = [
    # The recommendation landed.
    ("app cpu request", app["resources"]["requests"]["cpu"], "250m"),
    ("app memory request", app["resources"]["requests"]["memory"], "256Mi"),
    ("app cpu limit", app["resources"]["limits"]["cpu"], "500m"),
    # The sidecar was right-sized too, not left alone.
    ("envoy cpu request", envoy["resources"]["requests"]["cpu"], "20m"),
    ("envoy memory request", envoy["resources"]["requests"]["memory"], "32Mi"),
    # A resources patch must not disturb anything it never mentioned.
    ("app image preserved", app["image"], "ghcr.io/example/api:1.4.2"),
    ("app port preserved", app["ports"][0]["containerPort"], 8080),
    ("envoy image preserved", envoy["image"], "envoyproxy/envoy:v1.31-latest"),
    ("replicas preserved", d["spec"]["replicas"], 2),
    ("selector preserved", d["spec"]["selector"]["matchLabels"]["app"], "api"),
    ("pod label preserved", d["spec"]["template"]["metadata"]["labels"]["app"], "api"),
]

failed = False
for desc, got, want in checks:
    if got != want:
        print(f"FAIL: {desc} — got {got!r}, want {want!r}")
        failed = True
    else:
        print(f"ok: {desc} = {got!r}")

# The container list must still have exactly the two original containers: a
# wrong merge key would have appended duplicates instead of merging.
if len(d["spec"]["template"]["spec"]["containers"]) != 2:
    print("FAIL: container list was not merged by name")
    failed = True

sys.exit(1 if failed else 0)
PY

echo
echo "kubectl verification OK"
