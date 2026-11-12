// Package cli wires the Prometheus client, the recommender and the patch
// renderer into one runnable command.
package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/moveeeax/k8s-resource-recommender/internal/manifest"
	"github.com/moveeeax/k8s-resource-recommender/internal/patch"
	"github.com/moveeeax/k8s-resource-recommender/internal/prom"
	"github.com/moveeeax/k8s-resource-recommender/internal/recommend"
	"github.com/moveeeax/k8s-resource-recommender/internal/report"
)

// Exit codes. exitChanged lets a scheduled job tell "manifests were edited,
// open a pull request" apart from "nothing to do".
const (
	exitOK      = 0
	exitFailed  = 1
	exitUsage   = 2
	exitChanged = 3
)

// durationRe guards the Prometheus duration literals that get interpolated
// straight into a PromQL string. Rejecting anything else keeps a stray
// `--window '7d]) or vector(1)'` from rewriting the query.
var durationRe = regexp.MustCompile(`^[0-9]+(ms|s|m|h|d|w|y)$`)

// nameRe covers Kubernetes namespace names (RFC 1123 labels), for the same
// reason: the namespace ends up inside a PromQL label matcher.
var nameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

type config struct {
	promURL    string
	namespace  string
	window     string
	rateWindow string
	quantile   float64
	timeout    time.Duration

	headroom     float64
	cpuLimitFac  float64
	memLimitFac  float64
	omitCPULimit bool
	minCPUMilli  int64
	minMemMi     int64
	cpuStepMilli int64
	memStepMi    int64
	overFactor   float64

	format    string
	cpuCost   float64
	memCost   float64
	fromDir   string
	saveDir   string
	writeDir  string
	minChange float64
}

// Run executes the command and returns a process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("k8s-resource-recommender", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "k8s-resource-recommender — right-size Kubernetes requests/limits from real Prometheus usage.\n\n")
		fmt.Fprintf(stderr, "Usage:\n  k8s-resource-recommender --prom http://prom:9090 --namespace prod --window 7d\n\nFlags:\n")
		fs.PrintDefaults()
	}

	var c config
	fs.StringVar(&c.promURL, "prom", "", "Prometheus base URL (required), e.g. http://prom:9090")
	fs.StringVar(&c.namespace, "namespace", "", "namespace to analyze (default: all namespaces)")
	fs.StringVar(&c.window, "window", "7d", "lookback window for the usage quantile")
	fs.StringVar(&c.rateWindow, "rate-window", "5m", "inner rate() window and subquery step for CPU")
	fs.Float64Var(&c.quantile, "quantile", 0.95, "usage quantile to size against (0 < q <= 1)")
	fs.DurationVar(&c.timeout, "timeout", 30*time.Second, "per-query HTTP timeout")

	d := recommend.DefaultOptions()
	fs.Float64Var(&c.headroom, "headroom", d.Headroom, "fraction added on top of observed usage")
	fs.Float64Var(&c.cpuLimitFac, "cpu-limit-factor", d.CPULimitFactor, "CPU limit = request x this")
	fs.Float64Var(&c.memLimitFac, "mem-limit-factor", d.MemLimitFactor, "memory limit = request x this")
	fs.BoolVar(&c.omitCPULimit, "no-cpu-limit", false, "omit CPU limits entirely to avoid CFS throttling")
	fs.Int64Var(&c.minCPUMilli, "min-cpu", d.MinCPUMilli, "floor for the CPU request, in millicores")
	fs.Int64Var(&c.minMemMi, "min-memory", d.MinMemMi, "floor for the memory request, in MiB")
	fs.Int64Var(&c.cpuStepMilli, "cpu-step", d.CPUStepMilli, "round CPU up to a multiple of this, in millicores")
	fs.Int64Var(&c.memStepMi, "mem-step", d.MemStepMi, "round memory up to a multiple of this, in MiB")
	fs.Float64Var(&c.overFactor, "over-factor", d.OverFactor, "status is over when the current request exceeds the recommendation x this")

	fs.StringVar(&c.format, "format", "yaml", "output format: yaml (strategic-merge patch), table or json")
	fs.Float64Var(&c.cpuCost, "cpu-cost", 0, "price per requested core per month, for the reclaimable estimate")
	fs.Float64Var(&c.memCost, "mem-cost", 0, "price per requested GiB per month, for the reclaimable estimate")
	fs.StringVar(&c.fromDir, "from-dir", "", "read saved query responses from this directory instead of --prom")
	fs.StringVar(&c.saveDir, "save-queries", "", "write every raw Prometheus response to this directory for --from-dir")
	fs.StringVar(&c.writeDir, "write-manifests", "", "rewrite resources in Deployment manifests under this directory")
	fs.Float64Var(&c.minChange, "min-change", 0.1, "with --write-manifests, skip containers whose requests move by less than this fraction")

	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	opts := recommend.Options{
		Headroom:       c.headroom,
		CPULimitFactor: c.cpuLimitFac,
		MemLimitFactor: c.memLimitFac,
		OmitCPULimit:   c.omitCPULimit,
		MinCPUMilli:    c.minCPUMilli,
		MinMemMi:       c.minMemMi,
		CPUStepMilli:   c.cpuStepMilli,
		MemStepMi:      c.memStepMi,
		OverFactor:     c.overFactor,
	}

	if err := validate(c, opts); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return exitUsage
	}

	recs, err := collect(context.Background(), c, opts)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return exitFailed
	}
	if len(recs) == 0 {
		fmt.Fprintf(stderr, "no container usage found — check --namespace, --window, and that cAdvisor metrics are present\n")
		return exitFailed
	}
	if !anyCurrent(recs) {
		fmt.Fprintf(stderr, "note: no kube_pod_container_resource_requests series found, status is unset; is kube-state-metrics scraped?\n")
	}

	if c.writeDir != "" {
		return writeManifests(c, recs, stdout, stderr)
	}

	out, err := render(c, recs)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return exitFailed
	}
	fmt.Fprint(stdout, out)
	return exitOK
}

func anyCurrent(recs []recommend.Recommendation) bool {
	for _, r := range recs {
		if r.Status != recommend.StatusUnset {
			return true
		}
	}
	return false
}

func render(c config, recs []recommend.Recommendation) (string, error) {
	sum := recommend.Summarize(recs, recommend.Pricing{CPUCoreMonth: c.cpuCost, MemGiBMonth: c.memCost})
	var b bytes.Buffer
	switch c.format {
	case "table":
		if err := report.Table(&b, recs, sum); err != nil {
			return "", err
		}
	case "json":
		if err := report.JSON(&b, recs, sum); err != nil {
			return "", err
		}
	default:
		out, err := patch.Render(recs, c.quantile)
		if err != nil {
			return "", err
		}
		b.WriteString(out)
		// A trailing comment is still a valid patch file for kubectl.
		if line := report.Reclaimable(sum); line != "" {
			fmt.Fprintf(&b, "# %s\n", line)
		}
	}
	return b.String(), nil
}

// writeManifests edits the checkout and prints a Markdown table of the
// changes, ready to paste into a pull request body.
func writeManifests(c config, recs []recommend.Recommendation, stdout, stderr io.Writer) int {
	res, err := manifest.Rewrite(c.writeDir, recs, manifest.Options{
		Namespace:    c.namespace,
		MinChange:    c.minChange,
		OmitCPULimit: c.omitCPULimit,
	})
	for _, w := range res.Warnings {
		fmt.Fprintf(stderr, "warning: %s\n", w)
	}
	for _, u := range res.Unmatched {
		fmt.Fprintf(stderr, "warning: no Deployment manifest found for %s\n", u)
	}
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return exitFailed
	}
	if len(res.Changes) == 0 {
		fmt.Fprintf(stderr, "no manifest changes (threshold --min-change %g)\n", c.minChange)
		return exitOK
	}

	byKey := map[recommend.Key]recommend.Recommendation{}
	for _, r := range recs {
		byKey[recommend.Key{Namespace: r.Namespace, Deployment: r.Deployment, Container: r.Container}] = r
	}
	fmt.Fprintln(stdout, "| Workload | Container | CPU request | Memory request | Status | File |")
	fmt.Fprintln(stdout, "| --- | --- | --- | --- | --- | --- |")
	for _, ch := range res.Changes {
		status := byKey[recommend.Key{Namespace: ch.Namespace, Deployment: ch.Workload, Container: ch.Container}].Status
		fmt.Fprintf(stdout, "| %s/%s | %s | %s -> %s | %s -> %s | %s | `%s` |\n",
			ch.Namespace, ch.Workload, ch.Container, ch.OldCPU, ch.NewCPU, ch.OldMem, ch.NewMem, status, filepath.ToSlash(ch.File))
	}
	sum := recommend.Summarize(recs, recommend.Pricing{CPUCoreMonth: c.cpuCost, MemGiBMonth: c.memCost})
	if line := report.Reclaimable(sum); line != "" {
		fmt.Fprintf(stdout, "\n%s\n", line)
	}
	fmt.Fprintf(stderr, "%d container(s) changed\n", len(res.Changes))
	return exitChanged
}

func validate(c config, opts recommend.Options) error {
	switch {
	case c.promURL == "" && c.fromDir == "":
		return fmt.Errorf("--prom is required (or --from-dir for a saved run)")
	case c.promURL != "" && c.fromDir != "":
		return fmt.Errorf("--prom and --from-dir are mutually exclusive")
	case c.saveDir != "" && c.fromDir != "":
		return fmt.Errorf("--save-queries needs a live --prom run")
	}
	switch c.format {
	case "yaml", "table", "json":
	default:
		return fmt.Errorf("--format must be yaml, table or json, got %q", c.format)
	}
	if c.cpuCost < 0 || c.memCost < 0 {
		return fmt.Errorf("--cpu-cost and --mem-cost must be >= 0")
	}
	if c.minChange < 0 {
		return fmt.Errorf("--min-change must be >= 0, got %v", c.minChange)
	}
	if c.writeDir != "" {
		if fi, err := os.Stat(c.writeDir); err != nil || !fi.IsDir() {
			return fmt.Errorf("--write-manifests %q is not a directory", c.writeDir)
		}
	}
	if c.quantile <= 0 || c.quantile > 1 {
		return fmt.Errorf("--quantile must be in (0, 1], got %v", c.quantile)
	}
	if !durationRe.MatchString(c.window) {
		return fmt.Errorf("--window %q is not a Prometheus duration like 7d or 24h", c.window)
	}
	if !durationRe.MatchString(c.rateWindow) {
		return fmt.Errorf("--rate-window %q is not a Prometheus duration like 5m", c.rateWindow)
	}
	if c.namespace != "" && !nameRe.MatchString(c.namespace) {
		return fmt.Errorf("--namespace %q is not a valid Kubernetes name", c.namespace)
	}
	return opts.Validate()
}

// query names double as file names under --save-queries and --from-dir.
// Only the two usage responses are required for a replay; without the
// kube-state-metrics ones there is just no comparison.
type query struct {
	name     string
	expr     string
	required bool
}

func queries(c config) []query {
	return []query{
		{"cpu", cpuQuery(c), true},
		{"memory", memQuery(c), true},
		{"requests", ksmQuery("kube_pod_container_resource_requests", c), false},
		{"limits", ksmQuery("kube_pod_container_resource_limits", c), false},
		{"replicas", replicasQuery(c), false},
	}
}

// fetcher returns the samples for one query, live or from a saved run.
type fetcher func(ctx context.Context, q query) ([]prom.Sample, error)

func liveFetcher(c config) fetcher {
	client := prom.New(c.promURL, c.timeout)
	return func(ctx context.Context, q query) ([]prom.Sample, error) {
		body, err := client.QueryRaw(ctx, q.expr)
		if err != nil {
			return nil, err
		}
		if c.saveDir != "" {
			if err := os.MkdirAll(c.saveDir, 0o755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(filepath.Join(c.saveDir, q.name+".json"), body, 0o644); err != nil {
				return nil, err
			}
		}
		return prom.Parse(body)
	}
}

func fileFetcher(dir string) fetcher {
	return func(_ context.Context, q query) ([]prom.Sample, error) {
		body, err := os.ReadFile(filepath.Join(dir, q.name+".json"))
		if errors.Is(err, fs.ErrNotExist) && !q.required {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return prom.Parse(body)
	}
}

func collect(ctx context.Context, c config, opts recommend.Options) ([]recommend.Recommendation, error) {
	fetch := liveFetcher(c)
	if c.fromDir != "" {
		fetch = fileFetcher(c.fromDir)
	}

	got := map[string][]prom.Sample{}
	for _, q := range queries(c) {
		samples, err := fetch(ctx, q)
		if err != nil {
			return nil, fmt.Errorf("%s query: %w", q.name, err)
		}
		got[q.name] = samples
	}

	usage := recommend.Aggregate(index(c, got["cpu"]), index(c, got["memory"]))
	reqCPU, reqMem := indexByResource(c, got["requests"])
	limCPU, limMem := indexByResource(c, got["limits"])
	recommend.ApplyCurrent(usage, recommend.CurrentSeries{
		CPURequest: reqCPU,
		MemRequest: reqMem,
		CPULimit:   limCPU,
		MemLimit:   limMem,
		Replicas:   indexReplicas(c, got["replicas"]),
	})

	recs := make([]recommend.Recommendation, 0, len(usage))
	for _, u := range usage {
		recs = append(recs, recommend.For(u, opts))
	}
	return recs, nil
}

// index keys samples by (namespace, pod, container), dropping series that are
// missing any of those labels — cAdvisor emits pod-level rollups with an empty
// container label that would otherwise show up as a phantom workload.
//
// The namespace filter is already in the query for a live run; applying it
// again here makes --namespace work on a saved all-namespaces run too.
func index(c config, samples []prom.Sample) map[string]float64 {
	out := make(map[string]float64, len(samples))
	for _, s := range samples {
		ns, pod, container := s.Labels["namespace"], s.Labels["pod"], s.Labels["container"]
		if pod == "" || container == "" || (c.namespace != "" && ns != c.namespace) {
			continue
		}
		out[recommend.SeriesKey(ns, pod, container)] = s.Value
	}
	return out
}

// indexByResource splits a kube-state-metrics requests or limits result by
// its resource label. Values are already in cores and bytes.
func indexByResource(c config, samples []prom.Sample) (cpu, mem map[string]float64) {
	var cpuS, memS []prom.Sample
	for _, s := range samples {
		switch s.Labels["resource"] {
		case "cpu":
			cpuS = append(cpuS, s)
		case "memory":
			memS = append(memS, s)
		}
	}
	return index(c, cpuS), index(c, memS)
}

func indexReplicas(c config, samples []prom.Sample) map[string]float64 {
	out := make(map[string]float64, len(samples))
	for _, s := range samples {
		ns, dep := s.Labels["namespace"], s.Labels["deployment"]
		if dep == "" || (c.namespace != "" && ns != c.namespace) {
			continue
		}
		out[recommend.WorkloadKey(ns, dep)] = s.Value
	}
	return out
}

// selector builds the shared label matcher. POD is the old pause-container
// name; excluding it and the empty container keeps rollup series out.
func selector(c config) string {
	s := `container!="",container!="POD"`
	if c.namespace != "" {
		s = fmt.Sprintf(`namespace=%q,%s`, c.namespace, s)
	}
	return s
}

func cpuQuery(c config) string {
	return fmt.Sprintf(
		`max by (namespace, pod, container) (quantile_over_time(%g, rate(container_cpu_usage_seconds_total{%s}[%s])[%s:%s]))`,
		c.quantile, selector(c), c.rateWindow, c.window, c.rateWindow,
	)
}

// ksmQuery reads what kube-state-metrics reports as set on each container
// right now. It is an instant lookup, not a window: the comparison is against
// today's spec. The resource label survives the aggregation so CPU and memory
// can be told apart.
func ksmQuery(metric string, c config) string {
	return fmt.Sprintf(`max by (namespace, pod, container, resource) (%s{resource=~"cpu|memory",%s})`, metric, selector(c))
}

func replicasQuery(c config) string {
	if c.namespace == "" {
		return `max by (namespace, deployment) (kube_deployment_spec_replicas)`
	}
	return fmt.Sprintf(`max by (namespace, deployment) (kube_deployment_spec_replicas{namespace=%q})`, c.namespace)
}

func memQuery(c config) string {
	return fmt.Sprintf(
		`max by (namespace, pod, container) (quantile_over_time(%g, container_memory_working_set_bytes{%s}[%s:%s]))`,
		c.quantile, selector(c), c.window, c.rateWindow,
	)
}
