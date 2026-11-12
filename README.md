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
- Reads the current requests and limits from kube-state-metrics, so every row
  says whether the container is over-provisioned, under-provisioned or fine,
  and how much request capacity the change frees.
- Can rewrite the Deployment manifests in a Git checkout instead, for a pull
  request you review like any other change.

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
#     current: requests cpu 2 memory 64Mi, limits cpu 4 memory 64Mi (under)
#   envoy: p95 cpu 0.012 cores, p95 memory 24.0MiB across 1 pod(s)
#     current: requests cpu 500m memory 256Mi, limits cpu 1 memory 256Mi (over)
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
# reclaimable: 0.96 CPU, 448Mi (1 of 2 containers over)
```

Review it, then apply one Deployment at a time:

```console
$ k8s-resource-recommender --prom http://localhost:9090 --namespace prod > patch.yaml
$ kubectl patch deployment api -n prod --patch-file patch.yaml --dry-run=server
```

For a review, the table is quicker to read than the patch:

```console
$ k8s-resource-recommender --prom http://localhost:9090 --namespace prod --format table \
    --cpu-cost 20 --mem-cost 2.5
WORKLOAD     CONTAINER  CPU cur->rec  MEM cur->rec  STATUS
prod/api     app        2->250m       64Mi->256Mi   under
prod/api     envoy      500m->20m     256Mi->32Mi   over
prod/worker  app        none->1750m   none->1280Mi  unset

reclaimable: 0.96 CPU, 448Mi, ~$20.29/month (1 of 2 containers over)
```

`--format json` prints the same rows plus a `summary` object for scripts.

If Prometheus is not exposed locally, forward it first:

```console
$ kubectl port-forward -n monitoring svc/prometheus-operated 9090:9090
```

### Flags

| Flag | Default | What it does |
| --- | --- | --- |
| `--prom` | *(required)* | Prometheus base URL, e.g. `http://prom:9090` |
| `--from-dir` | | Read saved query responses from this directory instead of `--prom` |
| `--save-queries` | | Write every raw Prometheus response to this directory |
| `--format` | `yaml` | `yaml` (strategic-merge patch), `table` or `json` |
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
| `--over-factor` | `1.3` | Status is `over` when the current request is above recommendation x this |
| `--cpu-cost` | `0` | Price per requested core per month, for the reclaimable estimate |
| `--mem-cost` | `0` | Price per requested GiB per month, for the reclaimable estimate |
| `--write-manifests` | | Rewrite resources in Deployment manifests under this directory |
| `--min-change` | `0.1` | With `--write-manifests`, skip containers whose requests move by less than this fraction |
| `--timeout` | `30s` | Per-query HTTP timeout |

Exit codes: `0` success, `1` query failed or nothing found, `2` bad flags,
`3` `--write-manifests` changed at least one file.

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

### Current requests and status

Three more instant queries read what kube-state-metrics reports right now:
`kube_pod_container_resource_requests` and `kube_pod_container_resource_limits`
(kept per `resource`, maximum across replicas) and
`kube_deployment_spec_replicas`. They are optional: without kube-state-metrics
the patch is the same, every status is `unset`, and stderr says why.

The status rule comes from k8s-rightsize:

| Status | When |
| --- | --- |
| `unset` | No current CPU or memory request is known |
| `under` | Observed usage at `--quantile` is above the current request, for CPU or memory |
| `over` | The current request is more than `--over-factor` x the recommendation, for CPU or memory |
| `ok` | Neither |

Under wins over over: a container short on memory is reported `under` even
when its CPU request is far too high, because that is the risk to fix first.

The reclaimable total counts only `over` containers: current request minus
recommendation, per resource, never negative, times `spec.replicas` of the
Deployment (or the number of pods reporting a request when there is no replicas
series). `--cpu-cost` and `--mem-cost` turn it into a monthly estimate; the
number is only as good as the unit prices you pass.

### Offline runs

`--save-queries DIR` writes the five raw `/api/v1/query` responses as
`cpu.json`, `memory.json`, `requests.json`, `limits.json` and `replicas.json`.
`--from-dir DIR` replays them without Prometheus, so a reviewer or an
air-gapped machine gets the same output:

```console
$ k8s-resource-recommender --prom http://localhost:9090 --namespace prod --save-queries run-2026-11-12/
$ k8s-resource-recommender --from-dir run-2026-11-12/ --namespace prod --format table
```

Only `cpu.json` and `memory.json` are required. Pass the same `--quantile` on
replay: it only labels the header there, the values come from the files.
`--namespace` filters a saved all-namespaces run.

### Rewriting manifests for a pull request

```console
$ k8s-resource-recommender --prom http://localhost:9090 --namespace prod --write-manifests deploy/
| Workload | Container | CPU request | Memory request | Status | File |
| --- | --- | --- | --- | --- | --- |
| prod/api | app | 2000m -> 250m | 64Mi -> 256Mi | under | `api.yaml` |
| prod/api | envoy | 500m -> 20m | 256Mi -> 32Mi | over | `api.yaml` |
```

It walks `.yaml` and `.yml` files (skipping hidden directories), finds
`kind: Deployment` documents by namespace and name, and edits only
`spec.template.spec.containers[].resources` of containers that have a
recommendation. The edit is textual: values are replaced in place and missing
keys inserted next to their siblings, so comments, key order, quoting and the
rest of the file stay as they were and the diff shows only resource lines.

- A manifest without `metadata.namespace` is matched only when `--namespace`
  is set, and is then assumed to be in it.
- A container whose CPU and memory requests would both move by less than
  `--min-change` is left alone. `--min-change 0` rewrites anything that differs.
- With `--no-cpu-limit`, an existing CPU limit is removed rather than kept, so
  it cannot end up below the new request.
- Helm templates and other files that are not valid YAML, flow-style
  `resources: {cpu: 1}` blocks and containers missing from the manifest
  (injected sidecars) are skipped with a warning on stderr.

The Markdown table on stdout replaces the `--format` output and is meant for
the pull request body, and exit code
`3` tells a scheduled job that there is something to commit.
[`examples/rightsize-pr.yml`](examples/rightsize-pr.yml) is a GitHub Actions
workflow that runs it weekly and opens the PR with `gh pr create`.

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
