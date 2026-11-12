package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/moveeeax/k8s-resource-recommender/internal/recommend"
)

func rec(ns, dep, container, cpuReq, cpuLim, memReq, memLim string) recommend.Recommendation {
	r := recommend.Recommendation{
		Usage:      recommend.Usage{Namespace: ns, Deployment: dep, Container: container},
		CPURequest: resource.MustParse(cpuReq),
		MemRequest: resource.MustParse(memReq),
		MemLimit:   resource.MustParse(memLim),
	}
	if cpuLim != "" {
		q := resource.MustParse(cpuLim)
		r.CPULimit = &q
	}
	return r
}

var apiApp = rec("prod", "api", "app", "250m", "500m", "256Mi", "256Mi")

// rewrite runs Rewrite over a single file and returns its new content.
func rewrite(t *testing.T, src string, recs []recommend.Recommendation, o Options) (string, Result) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "deploy.yaml")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Rewrite(dir, recs, o)
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(got), res
}

func check(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// Only the four values change; comments, quoting, the unrelated
// ephemeral-storage entry and the kubectl-style unindented list all survive.
func TestRewriteInPlace(t *testing.T) {
	src := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
  namespace: prod
spec:
  template:
    spec:
      containers:
      - name: app # main container
        image: example/api:1
        resources:
          requests:
            cpu: "2"   # guessed at launch
            memory: 64Mi
            ephemeral-storage: 1Gi
          limits:
            cpu: '4'
            memory: 64Mi
`
	want := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
  namespace: prod
spec:
  template:
    spec:
      containers:
      - name: app # main container
        image: example/api:1
        resources:
          requests:
            cpu: "250m"   # guessed at launch
            memory: 256Mi
            ephemeral-storage: 1Gi
          limits:
            cpu: '500m'
            memory: 256Mi
`
	got, res := rewrite(t, src, []recommend.Recommendation{apiApp}, Options{})
	check(t, got, want)
	if len(res.Changes) != 1 {
		t.Fatalf("changes = %+v, want 1", res.Changes)
	}
	c := res.Changes[0]
	if c.OldCPU != "2" || c.NewCPU != "250m" || c.OldMem != "64Mi" || c.NewMem != "256Mi" || c.File != "deploy.yaml" {
		t.Errorf("change = %+v", c)
	}
}

func TestRewriteAddsMissingResources(t *testing.T) {
	src := `kind: Deployment
metadata:
  name: api
  namespace: prod
spec:
  template:
    spec:
      containers:
        - name: app
          image: example/api:1
`
	want := `kind: Deployment
metadata:
  name: api
  namespace: prod
spec:
  template:
    spec:
      containers:
        - name: app
          resources:
            requests:
              cpu: 250m
              memory: 256Mi
            limits:
              cpu: 500m
              memory: 256Mi
          image: example/api:1
`
	got, _ := rewrite(t, src, []recommend.Recommendation{apiApp}, Options{})
	check(t, got, want)
}

func TestRewriteFillsEmptyResources(t *testing.T) {
	for _, empty := range []string{"resources: {}", "resources:", "resources: null"} {
		t.Run(empty, func(t *testing.T) {
			src := "kind: Deployment\nmetadata: {name: api, namespace: prod}\nspec:\n  template:\n    spec:\n      containers:\n      - name: app\n        " + empty + "\n        image: x\n"
			want := "kind: Deployment\nmetadata: {name: api, namespace: prod}\nspec:\n  template:\n    spec:\n      containers:\n      - name: app\n        resources:\n" +
				"          requests:\n            cpu: 250m\n            memory: 256Mi\n" +
				"          limits:\n            cpu: 500m\n            memory: 256Mi\n" +
				"        image: x\n"
			got, _ := rewrite(t, src, []recommend.Recommendation{apiApp}, Options{})
			check(t, got, want)
		})
	}
}

// requests exists with only memory, limits is missing: the cpu entry and the
// whole limits section are inserted, the existing line is edited.
func TestRewriteAddsMissingKeys(t *testing.T) {
	src := `kind: Deployment
metadata:
  name: api
  namespace: prod
spec:
  template:
    spec:
      containers:
      - name: app
        resources:
          requests:
            memory: 64Mi
`
	want := `kind: Deployment
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
`
	got, _ := rewrite(t, src, []recommend.Recommendation{apiApp}, Options{})
	check(t, got, want)
}

func TestRewriteOmitCPULimitDeletesIt(t *testing.T) {
	src := `kind: Deployment
metadata:
  name: api
  namespace: prod
spec:
  template:
    spec:
      containers:
      - name: app
        resources:
          requests:
            cpu: 2
            memory: 64Mi
          limits:
            cpu: 4
            memory: 64Mi
`
	want := `kind: Deployment
metadata:
  name: api
  namespace: prod
spec:
  template:
    spec:
      containers:
      - name: app
        resources:
          requests:
            cpu: 250m
            memory: 256Mi
          limits:
            memory: 256Mi
`
	r := rec("prod", "api", "app", "250m", "", "256Mi", "256Mi")
	got, _ := rewrite(t, src, []recommend.Recommendation{r}, Options{OmitCPULimit: true})
	check(t, got, want)
}

// yaml.v3 reports line numbers across the whole stream, so a Deployment in the
// second document of a file must be edited at the right line.
func TestRewriteMultiDocument(t *testing.T) {
	src := `apiVersion: v1
kind: Service
metadata:
  name: api
  namespace: prod
---
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
          requests: {cpu: 1, memory: 1Gi}
      - name: envoy
        resources:
          requests:
            cpu: 500m
            memory: 256Mi
          limits:
            memory: 256Mi
`
	envoy := rec("prod", "api", "envoy", "20m", "", "32Mi", "32Mi")
	got, res := rewrite(t, src, []recommend.Recommendation{apiApp, envoy}, Options{})
	if !strings.Contains(got, "requests: {cpu: 1, memory: 1Gi}") {
		t.Errorf("flow-style requests were edited, want them left alone:\n%s", got)
	}
	if !strings.Contains(got, "            cpu: 20m\n            memory: 32Mi\n          limits:\n            memory: 32Mi\n") {
		t.Errorf("envoy not rewritten:\n%s", got)
	}
	if !strings.HasPrefix(got, "apiVersion: v1\nkind: Service\n") {
		t.Errorf("first document changed:\n%s", got)
	}
	if len(res.Changes) != 1 || res.Changes[0].Container != "envoy" {
		t.Errorf("changes = %+v, want envoy only", res.Changes)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "container app skipped") {
		t.Errorf("warnings = %q, want one for the flow-style app", res.Warnings)
	}
}

func TestRewriteMinChange(t *testing.T) {
	src := `kind: Deployment
metadata:
  name: api
  namespace: prod
spec:
  template:
    spec:
      containers:
      - name: app
        resources:
          requests:
            cpu: 240m
            memory: 250Mi
`
	// 240m -> 250m is 4%, 250Mi -> 256Mi is 2.4%: both under 10%.
	got, res := rewrite(t, src, []recommend.Recommendation{apiApp}, Options{MinChange: 0.1})
	check(t, got, src)
	if len(res.Changes) != 0 {
		t.Errorf("changes = %+v, want none below --min-change", res.Changes)
	}

	// With the threshold off the same file is rewritten.
	got, _ = rewrite(t, src, []recommend.Recommendation{apiApp}, Options{})
	if !strings.Contains(got, "cpu: 250m") {
		t.Errorf("not rewritten with MinChange 0:\n%s", got)
	}
}

// A file that already matches is not touched and reports no change.
func TestRewriteEqualQuantities(t *testing.T) {
	src := `kind: Deployment
metadata:
  name: api
  namespace: prod
spec:
  template:
    spec:
      containers:
      - name: app
        resources:
          requests:
            cpu: 0.25
            memory: 256Mi
          limits:
            cpu: 500m
            memory: 256Mi
`
	got, res := rewrite(t, src, []recommend.Recommendation{apiApp}, Options{})
	check(t, got, src)
	if len(res.Changes) != 0 {
		t.Errorf("changes = %+v, want none", res.Changes)
	}
}

func TestRewriteNamespaceFallback(t *testing.T) {
	src := `kind: Deployment
metadata:
  name: api
spec:
  template:
    spec:
      containers:
      - name: app
        resources:
          requests:
            cpu: 2
            memory: 64Mi
`
	got, res := rewrite(t, src, []recommend.Recommendation{apiApp}, Options{})
	check(t, got, src)
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "no metadata.namespace") {
		t.Errorf("warnings = %q", res.Warnings)
	}
	if len(res.Unmatched) != 1 || res.Unmatched[0] != "prod/api" {
		t.Errorf("unmatched = %q, want prod/api", res.Unmatched)
	}

	got, _ = rewrite(t, src, []recommend.Recommendation{apiApp}, Options{Namespace: "prod"})
	if !strings.Contains(got, "cpu: 250m") {
		t.Errorf("not rewritten with Options.Namespace:\n%s", got)
	}

	// The fallback never overrides a namespace the manifest does state.
	_, res = rewrite(t, strings.Replace(src, "  name: api\n", "  name: api\n  namespace: staging\n", 1),
		[]recommend.Recommendation{apiApp}, Options{Namespace: "prod"})
	if len(res.Changes) != 0 {
		t.Errorf("rewrote a staging manifest for a prod recommendation: %+v", res.Changes)
	}
}

func TestRewriteCRLF(t *testing.T) {
	src := "kind: Deployment\r\nmetadata:\r\n  name: api\r\n  namespace: prod\r\nspec:\r\n  template:\r\n    spec:\r\n      containers:\r\n      - name: app\r\n        image: x\r\n"
	got, _ := rewrite(t, src, []recommend.Recommendation{apiApp}, Options{})
	if strings.Count(got, "\n") != strings.Count(got, "\r\n") {
		t.Errorf("mixed line endings:\n%q", got)
	}
	if !strings.Contains(got, "\r\n        resources:\r\n          requests:\r\n            cpu: 250m\r\n") {
		t.Errorf("resources not inserted:\n%q", got)
	}
}

func TestRewriteSkipsTemplatesAndHiddenDirs(t *testing.T) {
	dir := t.TempDir()
	deploy := "kind: Deployment\nmetadata:\n  name: api\n  namespace: prod\nspec:\n  template:\n    spec:\n      containers:\n      - name: app\n        image: x\n"
	files := map[string]string{
		"chart/templates/deploy.yaml": "{{- if .Values.enabled }}\nkind: Deployment\n{{- end }}\n: :\n",
		".git/deploy.yaml":            deploy,
		"notes.txt":                   deploy,
		"apps/api.yml":                deploy,
	}
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	envoy := rec("prod", "api", "envoy", "20m", "40m", "32Mi", "32Mi")
	res, err := Rewrite(dir, []recommend.Recommendation{apiApp, envoy}, Options{})
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if len(res.Changes) != 1 || res.Changes[0].File != filepath.Join("apps", "api.yml") {
		t.Errorf("changes = %+v, want only apps/api.yml", res.Changes)
	}
	for _, name := range []string{".git/deploy.yaml", "notes.txt"} {
		b, _ := os.ReadFile(filepath.Join(dir, name))
		if string(b) != deploy {
			t.Errorf("%s was modified", name)
		}
	}
	var sawTemplate, sawSidecar bool
	for _, w := range res.Warnings {
		sawTemplate = sawTemplate || strings.Contains(w, "not valid YAML")
		sawSidecar = sawSidecar || strings.Contains(w, "no container envoy")
	}
	if !sawTemplate || !sawSidecar {
		t.Errorf("warnings = %q, want the template and the missing sidecar", res.Warnings)
	}
}
