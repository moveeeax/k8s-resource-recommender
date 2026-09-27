package cli

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"
)

// fakeProm answers /api/v1/query from the recorded fixtures, choosing by which
// metric the expression names. It also records every query it was asked.
func fakeProm(t *testing.T, queries *[]string) *httptest.Server {
	t.Helper()
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatalf("read fixture %s: %v", name, err)
		}
		return string(b)
	}
	cpu, mem := read("prom_cpu.json"), read("prom_mem.json")

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q := r.FormValue("query")
		if queries != nil {
			*queries = append(*queries, q)
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(q, "container_cpu_usage_seconds_total") {
			_, _ = io.WriteString(w, cpu)
			return
		}
		_, _ = io.WriteString(w, mem)
	}))
}

func TestRunEndToEnd(t *testing.T) {
	var queries []string
	srv := fakeProm(t, &queries)
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"--prom", srv.URL, "--namespace", "prod", "--window", "7d"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr:\n%s", code, stderr.String())
	}

	got := stdout.String()
	t.Logf("output:\n%s", got)

	// Two Deployments (api, worker) => two documents.
	docs := strings.Split(got, "---\n")
	if len(docs) != 2 {
		t.Fatalf("got %d documents, want 2:\n%s", len(docs), got)
	}

	var api appsv1.Deployment
	if err := yaml.UnmarshalStrict([]byte(docs[0]), &api); err != nil {
		t.Fatalf("api document invalid: %v\n%s", err, docs[0])
	}
	if api.Name != "api" || api.Namespace != "prod" {
		t.Fatalf("first document = %s/%s, want prod/api", api.Namespace, api.Name)
	}

	// api has two real containers; the empty-container rollup series must be
	// dropped rather than becoming a third phantom container.
	cs := api.Spec.Template.Spec.Containers
	if len(cs) != 2 {
		t.Fatalf("api has %d containers, want 2 (app, envoy): %+v", len(cs), cs)
	}

	// The acceptance criterion end to end: the busiest api/app replica sits at
	// 0.2 cores, so the request must come out at 250m.
	if got := cs[0].Resources.Requests.Cpu().String(); got != "250m" {
		t.Errorf("api/app cpu request = %q, want 250m", got)
	}
	// Max across pods: 209715200 B (200Mi) beats the other replica's 180Mi,
	// +25% headroom = 250Mi, rounded up to the 16Mi step = 256Mi.
	if got := cs[0].Resources.Requests.Memory().String(); got != "256Mi" {
		t.Errorf("api/app memory request = %q, want 256Mi", got)
	}
	if got := cs[0].Resources.Limits.Cpu().String(); got != "500m" {
		t.Errorf("api/app cpu limit = %q, want 500m", got)
	}

	if cs[1].Name != "envoy" {
		t.Errorf("second container = %q, want envoy", cs[1].Name)
	}

	// Both queries must be namespace-scoped and carry the window.
	if len(queries) != 2 {
		t.Fatalf("server saw %d queries, want 2", len(queries))
	}
	for _, q := range queries {
		for _, want := range []string{`namespace="prod"`, `[7d:5m]`, `container!="POD"`, "max by (namespace, pod, container)"} {
			if !strings.Contains(q, want) {
				t.Errorf("query %q missing %q", q, want)
			}
		}
	}
}

func TestRunNoCPULimitFlag(t *testing.T) {
	srv := fakeProm(t, nil)
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"--prom", srv.URL, "--no-cpu-limit"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr:\n%s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "cpu: 500m") {
		t.Errorf("cpu limit present despite --no-cpu-limit:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "cpu: 250m") {
		t.Errorf("cpu request missing:\n%s", stdout.String())
	}
}

func TestRunAllNamespacesOmitsMatcher(t *testing.T) {
	var queries []string
	srv := fakeProm(t, &queries)
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"--prom", srv.URL}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr:\n%s", code, stderr.String())
	}
	for _, q := range queries {
		if strings.Contains(q, "namespace=") {
			t.Errorf("query %q has a namespace matcher, want none when --namespace is empty", q)
		}
	}
}

func TestRunRejectsBadFlags(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"missing --prom", []string{"--namespace", "prod"}},
		{"bad window", []string{"--prom", "http://x", "--window", "7dd"}},
		{"promql injection via window", []string{"--prom", "http://x", "--window", `7d]) or vector(1`}},
		{"bad rate window", []string{"--prom", "http://x", "--rate-window", "5minutes"}},
		{"quantile above 1", []string{"--prom", "http://x", "--quantile", "1.5"}},
		{"quantile zero", []string{"--prom", "http://x", "--quantile", "0"}},
		{"bad namespace", []string{"--prom", "http://x", "--namespace", `prod" or up{`}},
		{"negative headroom", []string{"--prom", "http://x", "--headroom", "-1"}},
		{"cpu limit factor below 1", []string{"--prom", "http://x", "--cpu-limit-factor", "0.5"}},
		{"unknown flag", []string{"--prom", "http://x", "--nope"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := Run(tt.args, &stdout, &stderr); code != 2 {
				t.Errorf("exit code = %d, want 2 (stderr: %s)", code, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Errorf("wrote to stdout on a usage error: %q", stdout.String())
			}
		})
	}
}

func TestRunPrometheusErrorExitsOne(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"status":"error","errorType":"bad_data","error":"boom"}`)
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"--prom", srv.URL}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "boom") {
		t.Errorf("stderr = %q, want the Prometheus message", stderr.String())
	}
}

// An empty result set is a real operator problem (wrong namespace, no
// cAdvisor), so it must be a loud non-zero exit, not a silent success.
func TestRunEmptyResultExitsOne(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"--prom", srv.URL, "--namespace", "nope"}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "no container usage found") {
		t.Errorf("stderr = %q, want an explanatory message", stderr.String())
	}
}

func TestQuantileIsInterpolatedWithoutExponent(t *testing.T) {
	// %g would render 0.999 fine but 0.0000001 as 1e-07, which PromQL accepts;
	// the guard here is that the common quantiles stay literal.
	for _, q := range []struct {
		in   float64
		want string
	}{{0.95, "0.95"}, {0.99, "0.99"}, {0.5, "0.5"}, {1, "1"}} {
		got := cpuQuery(config{quantile: q.in, window: "7d", rateWindow: "5m"})
		if !strings.Contains(got, "quantile_over_time("+q.want+",") {
			t.Errorf("quantile %v rendered wrong: %s", q.in, got)
		}
	}
}
