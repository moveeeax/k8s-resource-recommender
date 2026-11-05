# k8s-resource-recommender

[![ci](https://github.com/moveeeax/k8s-resource-recommender/actions/workflows/ci.yml/badge.svg)](https://github.com/moveeeax/k8s-resource-recommender/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/go-1.26%2B-00ADD8?logo=go&logoColor=white)](https://go.dev/dl/)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Point it at Prometheus, get a Kubernetes strategic-merge patch with requests and
limits derived from what your containers actually use.

Most clusters run on requests somebody guessed during the first deploy and never
revisited: over-provisioned pods burn money, under-provisioned ones get
OOMKilled. VPA can fix that, but it wants a controller, CRDs and mutating
webhooks in your cluster. This is a read-only CLI instead — nothing to install
cluster-side, no admission path, and the output is a plain YAML file you can read
in a pull request before anything changes.

## What it does

- Queries Prometheus for per-container CPU and memory over a lookback window.
- Takes a quantile (p95 by default) of each series, then the **maximum across
  replicas** — sizing off the average starves the busiest pod.
- Adds headroom, rounds to readable values, and prints a strategic-merge patch
  per Deployment.

## Install

```console
go install github.com/moveeeax/k8s-resource-recommender/cmd/k8s-resource-recommender@latest
```

Or build from source:

```console
git clone https://github.com/moveeeax/k8s-resource-recommender
cd k8s-resource-recommender
go build -o k8s-resource-recommender ./cmd/k8s-resource-recommender
```

## Usage

```console
$ k8s-resource-recommender --prom http://localhost:9090 --namespace prod --window 7d
# prod/api
#   app: p95 cpu 0.200 cores, p95 memory 200.0MiB across 2 pod(s)
#   envoy: p95 cpu 0.012 cores, p95 memory 24.0MiB across 1 pod(s)
apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
  namespace: prod
spec:
  template:
    spec:
      containers:
      - name: app
        resources:
          limits:
            cpu: 500m
            memory: 256Mi
          requests:
            cpu: 250m
            memory: 256Mi
      - name: envoy
        resources:
          limits:
            cpu: 40m
            memory: 32Mi
          requests:
            cpu: 20m
            memory: 32Mi
```

Review it, then apply one Deployment at a time:

```console
$ k8s-resource-recommender --prom http://localhost:9090 --namespace prod > patch.yaml
$ kubectl patch deployment api -n prod --patch-file patch.yaml --dry-run=server
```

If Prometheus is not exposed locally, forward it first:

```console
$ kubectl port-forward -n monitoring svc/prometheus-operated 9090:9090
```

### Flags

| Flag | Default | What it does |
| --- | --- | --- |
| `--prom` | *(required)* | Prometheus base URL, e.g. `http://prom:9090` |
| `--namespace` | *(all)* | Restrict to one namespace |
| `--window` | `7d` | Lookback window for the quantile |
| `--rate-window` | `5m` | Inner `rate()` window and subquery step |
| `--quantile` | `0.95` | Usage quantile to size against |
| `--headroom` | `0.25` | Fraction added on top of observed usage |
| `--cpu-limit-factor` | `2.0` | CPU limit = request x this |
| `--mem-limit-factor` | `1.0` | Memory limit = request x this |
| `--no-cpu-limit` | `false` | Omit CPU limits entirely |
| `--min-cpu` | `10` | Floor for the CPU request, in millicores |
| `--min-memory` | `16` | Floor for the memory request, in MiB |
| `--cpu-step` | `10` | Round CPU up to a multiple of this, in millicores |
| `--mem-step` | `16` | Round memory up to a multiple of this, in MiB |
| `--timeout` | `30s` | Per-query HTTP timeout |

Exit codes: `0` success, `1` query failed or nothing found, `2` bad flags.

## How it works

Two instant queries, one per resource. CPU, with the defaults filled in:

```promql
max by (namespace, pod, container) (
  quantile_over_time(0.95,
    rate(container_cpu_usage_seconds_total{namespace="prod",container!="",container!="POD"}[5m])[7d:5m]
  )
)
```

Memory is the same shape over `container_memory_working_set_bytes` — working set
rather than RSS, because that is the number the kubelet compares against the
limit when it decides to OOMKill.

The `max by (namespace, pod, container)` wrapper matters: cAdvisor attaches
`id`, `image` and `name` labels that would otherwise split one container into
several series. The `container!=""` matcher drops the pod-level rollup series,
which would otherwise show up as a phantom container using the whole pod's CPU.

Requests are then `quantile x (1 + headroom)`, rounded **up** to the step, and
limits are derived from the already-rounded request — so you never see a `437m`
limit next to a `250m` request.

### Mapping pods back to Deployments

The tool never talks to the API server, so there is no `ownerReference` to walk.
It strips the `-<replicaset-hash>-<pod-hash>` tail that a Deployment appends to
its pods:

```
api-7d9f8b6c5d-x2k4p  ->  api
```

Names that do not match that shape — `postgres-0` from a StatefulSet, a bare pod
— are passed through unchanged rather than guessed at. You get a patch addressed
to a name that does not exist as a Deployment, which is visible and fixable,
instead of a patch silently aimed at the wrong workload.

## One note

**Memory limits default to equal the request; CPU limits do not.** Memory is
incompressible: a container over its memory limit is killed, so a limit above the
request only buys a slower, less predictable death under node pressure. Setting
them equal is the honest choice. CPU is compressible — going over just means
throttling — so the CPU limit defaults to 2x the request to leave room for
startup spikes.

If your workloads are latency-sensitive, drop CPU limits altogether with
`--no-cpu-limit`. The CFS quota is enforced per 100ms period, so a service that
sits well under its limit on average still gets throttled during bursts, and that
shows up as p99 latency nobody can explain from a dashboard.

## Development

```console
go test ./...                   # unit tests
./examples/demo.sh              # end-to-end against a fixture-backed fake Prometheus
./examples/verify-kubectl.sh    # proves the patch applies with kubectl --local
```

See [`examples/`](examples/) for the fixtures and what each script checks.

## License

MIT — see [LICENSE](LICENSE).

https://github.com/moveeeax/k8s-resource-recommender
