// Package cli wires the Prometheus client, the recommender and the patch
// renderer into one runnable command.
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"regexp"
	"time"

	"github.com/moveeeax/k8s-resource-recommender/internal/patch"
	"github.com/moveeeax/k8s-resource-recommender/internal/prom"
	"github.com/moveeeax/k8s-resource-recommender/internal/recommend"
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

	if err := fs.Parse(args); err != nil {
		return 2
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
	}

	if err := validate(c, opts); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}

	out, err := run(context.Background(), c, opts)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if out == "" {
		fmt.Fprintf(stderr, "no container usage found — check --namespace, --window, and that cAdvisor metrics are present\n")
		return 1
	}
	fmt.Fprint(stdout, out)
	return 0
}

func validate(c config, opts recommend.Options) error {
	if c.promURL == "" {
		return fmt.Errorf("--prom is required")
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

func run(ctx context.Context, c config, opts recommend.Options) (string, error) {
	client := prom.New(c.promURL, c.timeout)

	cpuSamples, err := client.Query(ctx, cpuQuery(c))
	if err != nil {
		return "", fmt.Errorf("cpu query: %w", err)
	}
	memSamples, err := client.Query(ctx, memQuery(c))
	if err != nil {
		return "", fmt.Errorf("memory query: %w", err)
	}

	usage := recommend.Aggregate(index(cpuSamples), index(memSamples))
	recs := make([]recommend.Recommendation, 0, len(usage))
	for _, u := range usage {
		recs = append(recs, recommend.For(u, opts))
	}
	return patch.Render(recs, c.quantile)
}

// index keys samples by (namespace, pod, container), dropping series that are
// missing any of those labels — cAdvisor emits pod-level rollups with an empty
// container label that would otherwise show up as a phantom workload.
func index(samples []prom.Sample) map[string]float64 {
	out := make(map[string]float64, len(samples))
	for _, s := range samples {
		ns, pod, container := s.Labels["namespace"], s.Labels["pod"], s.Labels["container"]
		if pod == "" || container == "" {
			continue
		}
		out[recommend.SeriesKey(ns, pod, container)] = s.Value
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

func memQuery(c config) string {
	return fmt.Sprintf(
		`max by (namespace, pod, container) (quantile_over_time(%g, container_memory_working_set_bytes{%s}[%s:%s]))`,
		c.quantile, selector(c), c.window, c.rateWindow,
	)
}
