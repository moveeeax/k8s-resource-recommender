package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
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
	byMetric := map[string]string{
		"container_cpu_usage_seconds_total":    read("prom_cpu.json"),
		"container_memory_working_set_bytes":   read("prom_mem.json"),
		"kube_pod_container_resource_requests": read("prom_requests.json"),
		"kube_pod_container_resource_limits":   read("prom_limits.json"),
		"kube_deployment_spec_replicas":        read("prom_replicas.json"),
	}

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q := r.FormValue("query")
		if queries != nil {
			*queries = append(*queries, q)
		}
		w.Header().Set("Content-Type", "application/json")
		for metric, body := range byMetric {
			if strings.Contains(q, metric) {
				_, _ = io.WriteString(w, body)
				return
			}
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"status":"error","errorType":"bad_data","error":"unexpected query"}`)
	}))
}

// usageOnlyProm answers the two usage queries and returns an empty vector for
// everything else, like a Prometheus that does not scrape kube-state-metrics.
func usageOnlyProm(t *testing.T) *httptest.Server {
	t.Helper()
	full := fakeProm(t, nil)
	t.Cleanup(full.Close)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q := r.FormValue("query")
		if strings.Contains(q, "container_cpu_usage_seconds_total") || strings.Contains(q, "container_memory_working_set_bytes") {
			resp, err := http.PostForm(full.URL+"/api/v1/query", r.Form)
			if err != nil {
				t.Errorf("proxy: %v", err)
				return
			}
			defer resp.Body.Close()
			_, _ = io.Copy(w, resp.Body)
			return
		}
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
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

	// Two usage queries plus requests, limits and replicas from
	// kube-state-metrics.
	if len(queries) != 5 {
		t.Fatalf("server saw %d queries, want 5", len(queries))
	}
	// The usage queries must be namespace-scoped and carry the window.
	for _, q := range queries[:2] {
		for _, want := range []string{`namespace="prod"`, `[7d:5m]`, `container!="POD"`, "max by (namespace, pod, container)"} {
			if !strings.Contains(q, want) {
				t.Errorf("query %q missing %q", q, want)
			}
		}
	}
	// The kube-state-metrics queries are instant lookups of today's spec.
	for _, q := range queries[2:] {
		if !strings.Contains(q, `namespace="prod"`) {
			t.Errorf("query %q is not namespace-scoped", q)
		}
		if strings.Contains(q, "[7d") {
			t.Errorf("query %q carries the usage window, want an instant lookup", q)
		}
	}
	for _, want := range []string{
		`max by (namespace, pod, container, resource) (kube_pod_container_resource_requests{resource=~"cpu|memory",`,
		`max by (namespace, pod, container, resource) (kube_pod_container_resource_limits{resource=~"cpu|memory",`,
		`max by (namespace, deployment) (kube_deployment_spec_replicas{namespace="prod"})`,
	} {
		found := false
		for _, q := range queries {
			found = found || strings.Contains(q, want)
		}
		if !found {
			t.Errorf("no query contains %q", want)
		}
	}
}

// The patch header carries the current values, and the reclaimable total
// trails the last document.
func TestRunPatchShowsCurrentValues(t *testing.T) {
	srv := fakeProm(t, nil)
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"--prom", srv.URL, "--namespace", "prod", "--cpu-cost", "20", "--mem-cost", "2.5"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr:\n%s", code, stderr.String())
	}
	got := stdout.String()
	for _, want := range []string{
		"#     current: requests cpu 2 memory 64Mi, limits cpu 4 memory 64Mi (under)",
		"#     current: requests cpu 500m memory 256Mi, limits cpu 1 memory 256Mi (over)",
		// envoy: (500m - 20m) x 2 replicas = 0.96 cores, (256Mi - 32Mi) x 2 =
		// 448Mi; 0.96 x 20 + 0.4375 x 2.5 = 20.29.
		"# reclaimable: 0.96 CPU, 448Mi, ~$20.29/month (1 of 2 containers over)\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	// worker has no requests in kube-state-metrics: no current line for it.
	worker := got[strings.Index(got, "# prod/worker"):]
	if strings.Contains(worker[:strings.Index(worker, "apiVersion")], "current:") {
		t.Errorf("worker header has a current line despite having no requests:\n%s", worker)
	}

	// The trailing comment must not break the documents.
	docs := strings.Split(got, "---\n")
	var dep appsv1.Deployment
	if err := yaml.UnmarshalStrict([]byte(docs[len(docs)-1]), &dep); err != nil {
		t.Fatalf("last document invalid with the trailer: %v", err)
	}
}

func TestRunTableFormat(t *testing.T) {
	srv := fakeProm(t, nil)
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"--prom", srv.URL, "--format", "table"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr:\n%s", code, stderr.String())
	}
	got := stdout.String()
	t.Logf("output:\n%s", got)
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if f := strings.Fields(lines[0]); strings.Join(f, " ") != "WORKLOAD CONTAINER CPU cur->rec MEM cur->rec STATUS" {
		t.Errorf("header = %q", lines[0])
	}
	want := [][]string{
		{"prod/api", "app", "2->250m", "64Mi->256Mi", "under"},
		{"prod/api", "envoy", "500m->20m", "256Mi->32Mi", "over"},
		{"prod/worker", "app", "none->1750m", "none->1280Mi", "unset"},
	}
	for i, w := range want {
		if f := strings.Fields(lines[i+1]); strings.Join(f, " ") != strings.Join(w, " ") {
			t.Errorf("row %d = %q, want %q", i, f, w)
		}
	}
	if !strings.Contains(got, "\nreclaimable: 0.96 CPU, 448Mi (1 of 2 containers over)\n") {
		t.Errorf("table missing the reclaimable line:\n%s", got)
	}
}

func TestRunJSONFormat(t *testing.T) {
	srv := fakeProm(t, nil)
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"--prom", srv.URL, "--format", "json", "--cpu-cost", "20"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr:\n%s", code, stderr.String())
	}
	var doc struct {
		Recommendations []struct {
			Workload    string `json:"workload"`
			Container   string `json:"container"`
			Status      string `json:"status"`
			Current     map[string]float64
			Recommended struct {
				Requests map[string]string `json:"requests"`
				Limits   map[string]string `json:"limits"`
			} `json:"recommended"`
		} `json:"recommendations"`
		Summary struct {
			Compared    int     `json:"compared"`
			Over        int     `json:"over"`
			CPUCores    float64 `json:"reclaimableCpuCores"`
			MemBytes    float64 `json:"reclaimableMemoryBytes"`
			MonthlyCost float64 `json:"monthlyCost"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout.String())
	}
	if len(doc.Recommendations) != 3 {
		t.Fatalf("got %d rows, want 3", len(doc.Recommendations))
	}
	envoy := doc.Recommendations[1]
	if envoy.Container != "envoy" || envoy.Status != "over" {
		t.Errorf("row 1 = %s %s, want envoy over", envoy.Container, envoy.Status)
	}
	if envoy.Current["cpuRequestCores"] != 0.5 || envoy.Current["replicas"] != 2 {
		t.Errorf("envoy current = %v", envoy.Current)
	}
	if envoy.Recommended.Requests["cpu"] != "20m" || envoy.Recommended.Limits["memory"] != "32Mi" {
		t.Errorf("envoy recommended = %+v", envoy.Recommended)
	}
	if _, ok := doc.Recommendations[2].Current["cpuRequestCores"]; ok {
		t.Errorf("worker has a current cpu request, want it omitted: %v", doc.Recommendations[2].Current)
	}
	s := doc.Summary
	if s.Compared != 2 || s.Over != 1 || math.Abs(s.CPUCores-0.96) > 1e-9 || s.MemBytes != 448<<20 || math.Abs(s.MonthlyCost-19.2) > 1e-9 {
		t.Errorf("summary = %+v", s)
	}
}

// Without kube-state-metrics the run still works; it just cannot compare.
func TestRunWithoutKubeStateMetrics(t *testing.T) {
	srv := usageOnlyProm(t)
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"--prom", srv.URL, "--format", "table"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr:\n%s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "reclaimable") {
		t.Errorf("printed a reclaimable total with nothing to compare:\n%s", stdout.String())
	}
	if strings.Count(stdout.String(), "unset") != 3 {
		t.Errorf("want every row unset:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "kube-state-metrics") {
		t.Errorf("stderr = %q, want a note about kube-state-metrics", stderr.String())
	}
}

// A live run saved with --save-queries replays to the same output offline.
func TestRunSaveAndReplay(t *testing.T) {
	srv := fakeProm(t, nil)
	defer srv.Close()
	dir := t.TempDir()

	var live, replay, stderr bytes.Buffer
	if code := Run([]string{"--prom", srv.URL, "--namespace", "prod", "--save-queries", dir}, &live, &stderr); code != 0 {
		t.Fatalf("live run exit code = %d, stderr:\n%s", code, stderr.String())
	}
	for _, name := range []string{"cpu", "memory", "requests", "limits", "replicas"} {
		if _, err := os.Stat(filepath.Join(dir, name+".json")); err != nil {
			t.Errorf("--save-queries did not write %s.json: %v", name, err)
		}
	}
	srv.Close()

	if code := Run([]string{"--from-dir", dir, "--namespace", "prod"}, &replay, &stderr); code != 0 {
		t.Fatalf("replay exit code = %d, stderr:\n%s", code, stderr.String())
	}
	if live.String() != replay.String() {
		t.Errorf("replay differs from the live run:\n--- live\n%s\n--- replay\n%s", live.String(), replay.String())
	}
}

// The kube-state-metrics files are optional for a replay; the usage ones are
// not. The example fixtures double as a saved run.
func TestRunFromDirOptionalFiles(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []struct{ src, dst string }{{"prom_cpu.json", "cpu.json"}, {"prom_mem.json", "memory.json"}} {
		b, err := os.ReadFile(filepath.Join("testdata", f.src))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f.dst), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"--from-dir", dir}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr:\n%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "cpu: 250m") {
		t.Errorf("replay output missing the api/app request:\n%s", stdout.String())
	}

	if err := os.Remove(filepath.Join(dir, "memory.json")); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"--from-dir", dir}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit code = %d without memory.json, want 1", code)
	}
	if !strings.Contains(stderr.String(), "memory query") {
		t.Errorf("stderr = %q, want it to name the missing file", stderr.String())
	}
}

// A saved all-namespaces run can still be narrowed with --namespace.
func TestRunFromDirFiltersNamespace(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"--from-dir", "../../examples/fixtures", "--namespace", "staging"}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit code = %d, want 1 (no usage in staging); stdout:\n%s", code, stdout.String())
	}
}

func TestRunWriteManifests(t *testing.T) {
	srv := fakeProm(t, nil)
	defer srv.Close()

	dir := t.TempDir()
	base, err := os.ReadFile("../../examples/fixtures/base-deployment.yaml")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "api.yaml")
	if err := os.WriteFile(path, base, 0o644); err != nil {
		t.Fatal(err)
	}

	args := []string{"--prom", srv.URL, "--namespace", "prod", "--write-manifests", dir}
	var stdout, stderr bytes.Buffer
	if code := Run(args, &stdout, &stderr); code != 3 {
		t.Fatalf("exit code = %d, want 3 (files changed); stderr:\n%s", code, stderr.String())
	}
	t.Logf("stdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())

	for _, want := range []string{
		"| Workload | Container | CPU request | Memory request | Status | File |",
		"| prod/api | app | 2000m -> 250m | 64Mi -> 256Mi | under | `api.yaml` |",
		"| prod/api | envoy | 500m -> 20m | 256Mi -> 32Mi | over | `api.yaml` |",
		"reclaimable: 0.96 CPU, 448Mi",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout missing %q", want)
		}
	}
	// worker has usage but no manifest in the directory.
	if !strings.Contains(stderr.String(), "no Deployment manifest found for prod/worker") {
		t.Errorf("stderr = %q, want a warning for prod/worker", stderr.String())
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var dep appsv1.Deployment
	if err := yaml.UnmarshalStrict(got, &dep); err != nil {
		t.Fatalf("rewritten manifest invalid: %v\n%s", err, got)
	}
	app := dep.Spec.Template.Spec.Containers[0].Resources
	if app.Requests.Cpu().String() != "250m" || app.Limits.Cpu().String() != "500m" || app.Requests.Memory().String() != "256Mi" {
		t.Errorf("app resources = %+v", app)
	}
	if !strings.HasPrefix(string(got), "# A deliberately badly-sized Deployment") {
		t.Errorf("leading comment lost:\n%s", got)
	}

	// Second run: everything already matches, nothing to do.
	stdout.Reset()
	stderr.Reset()
	if code := Run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("second run exit code = %d, want 0; stderr:\n%s", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("second run printed a table: %q", stdout.String())
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
		{"--prom and --from-dir", []string{"--prom", "http://x", "--from-dir", "."}},
		{"--save-queries on a replay", []string{"--from-dir", ".", "--save-queries", "out"}},
		{"unknown format", []string{"--prom", "http://x", "--format", "csv"}},
		{"negative cpu cost", []string{"--prom", "http://x", "--cpu-cost", "-1"}},
		{"negative min change", []string{"--prom", "http://x", "--min-change", "-0.1"}},
		{"over factor below 1", []string{"--prom", "http://x", "--over-factor", "0.9"}},
		{"write-manifests not a dir", []string{"--prom", "http://x", "--write-manifests", "does-not-exist"}},
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
