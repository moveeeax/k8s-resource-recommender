# Examples

Both scripts run without a cluster and without a real Prometheus. CI runs both,
so if the documented usage drifts from the code, the build breaks.

## `demo.sh`

Starts [`fake-prometheus.py`](fake-prometheus.py), runs the CLI against it, and
prints the patch — once with the default CPU limits, once with `--no-cpu-limit`.
Then it prints the `--format table` comparison and runs `--write-manifests` on a
temporary copy of the fixture Deployment, checking for exit code `3` and that a
second run changes nothing.

```console
$ ./examples/demo.sh
== recommended patch for namespace 'prod' ==
# prod/api
#   app: p95 cpu 0.200 cores, p95 memory 200.0MiB across 2 pod(s)
...
demo OK
```

## `verify-kubectl.sh`

The acceptance check. Generates the patch, applies it to
[`fixtures/base-deployment.yaml`](fixtures/base-deployment.yaml) with
`kubectl patch --local --type strategic`, and asserts on the merged result:

- the recommendation landed (`250m` request for the container using 0.2 cores),
- the sidecar was right-sized too,
- and the merge touched **nothing else** — image, container port, replicas,
  selector and pod labels all survive, and the container list still has exactly
  two entries rather than duplicates from a wrong merge key.

Needs `kubectl` on `PATH`, or pass it explicitly:

```console
$ KUBECTL=/tmp/kubectl ./examples/verify-kubectl.sh
```

## Fixtures

| File | What it is |
| --- | --- |
| `fixtures/cpu.json` | Recorded Prometheus response for the CPU query |
| `fixtures/memory.json` | Recorded Prometheus response for the memory query |
| `fixtures/requests.json` | kube-state-metrics requests matching `base-deployment.yaml` |
| `fixtures/limits.json` | kube-state-metrics limits matching `base-deployment.yaml` |
| `fixtures/replicas.json` | `kube_deployment_spec_replicas` for `api` and `worker` |
| `fixtures/base-deployment.yaml` | A deliberately badly-sized Deployment to patch |

The CPU fixture includes two deliberate traps that the tool has to survive: two
replicas of `api` with different usage (the higher one must win) and a series with
an empty `container` label (the cAdvisor pod-level rollup, which must be dropped
rather than become a phantom container).

The fixture files use the names `--save-queries` writes, so the directory is also
a saved run:

```console
$ k8s-resource-recommender --from-dir examples/fixtures --format table
```

They are the same payloads used by the unit tests in `internal/cli/testdata/`.

## `rightsize-pr.yml`

A GitHub Actions workflow for a manifests repository: runs `--write-manifests`
weekly and opens a pull request with the Markdown table as its body when the
exit code is `3`. CI here does not run it; it needs a runner that can reach
Prometheus.
