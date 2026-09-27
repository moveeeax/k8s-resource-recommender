package patch

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"

	"github.com/moveeeax/k8s-resource-recommender/internal/recommend"
)

func rec(ns, dep, container, cpuReq, cpuLim, memReq, memLim string) recommend.Recommendation {
	r := recommend.Recommendation{
		Usage:      recommend.Usage{Namespace: ns, Deployment: dep, Container: container, Pods: 1},
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

func TestRenderEmpty(t *testing.T) {
	got, err := Render(nil, 0.95)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got != "" {
		t.Errorf("Render(nil) = %q, want empty", got)
	}
}

func TestRenderSingleDeployment(t *testing.T) {
	got, err := Render([]recommend.Recommendation{
		rec("prod", "api", "app", "250m", "500m", "256Mi", "256Mi"),
	}, 0.95)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	for _, want := range []string{
		"# prod/api",
		"apiVersion: apps/v1",
		"kind: Deployment",
		"name: api",
		"namespace: prod",
		"cpu: 250m",
		"memory: 256Mi",
		"cpu: 500m",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

// The acceptance criterion says the output must apply cleanly with kubectl.
// Strict-unmarshalling into the real appsv1.Deployment type is the closest
// check that does not need a cluster: it fails on any field kubectl would
// reject as unknown, and on any malformed quantity.
func TestRenderIsAValidDeploymentPatch(t *testing.T) {
	got, err := Render([]recommend.Recommendation{
		rec("prod", "api", "app", "250m", "500m", "256Mi", "256Mi"),
		rec("prod", "api", "sidecar", "10m", "20m", "16Mi", "16Mi"),
	}, 0.95)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	var dep appsv1.Deployment
	if err := yaml.UnmarshalStrict([]byte(got), &dep); err != nil {
		t.Fatalf("output is not a valid Deployment patch: %v\n%s", err, got)
	}

	if dep.APIVersion != "apps/v1" || dep.Kind != "Deployment" {
		t.Errorf("TypeMeta = %s/%s", dep.APIVersion, dep.Kind)
	}
	if dep.Name != "api" || dep.Namespace != "prod" {
		t.Errorf("ObjectMeta = %s/%s", dep.Namespace, dep.Name)
	}

	// Both containers of the same Deployment belong in one document.
	cs := dep.Spec.Template.Spec.Containers
	if len(cs) != 2 {
		t.Fatalf("got %d containers, want 2", len(cs))
	}
	if cs[0].Name != "app" || cs[1].Name != "sidecar" {
		t.Errorf("container names = %q, %q", cs[0].Name, cs[1].Name)
	}

	cpu := cs[0].Resources.Requests.Cpu()
	if cpu.String() != "250m" {
		t.Errorf("app cpu request = %q, want 250m", cpu.String())
	}
	mem := cs[0].Resources.Limits.Memory()
	if mem.String() != "256Mi" {
		t.Errorf("app memory limit = %q, want 256Mi", mem.String())
	}
}

// A patch must not carry an empty `creationTimestamp: null` or a `status`
// block: rendering from the real Deployment struct would emit both, which is
// why this package uses its own minimal types.
func TestRenderHasNoEmptyScaffolding(t *testing.T) {
	got, err := Render([]recommend.Recommendation{rec("prod", "api", "app", "250m", "500m", "256Mi", "256Mi")}, 0.95)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, unwanted := range []string{"creationTimestamp", "status:", "strategy:", "selector:", "replicas:"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("output contains %q, which does not belong in a patch:\n%s", unwanted, got)
		}
	}
}

func TestRenderSeparatesDeploymentsWithDocumentMarker(t *testing.T) {
	got, err := Render([]recommend.Recommendation{
		rec("prod", "api", "app", "250m", "500m", "256Mi", "256Mi"),
		rec("prod", "worker", "app", "100m", "200m", "128Mi", "128Mi"),
		rec("staging", "api", "app", "50m", "100m", "64Mi", "64Mi"),
	}, 0.95)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	docs := strings.Split(got, "---\n")
	if len(docs) != 3 {
		t.Fatalf("got %d documents, want 3:\n%s", len(docs), got)
	}
	for _, d := range docs {
		var dep appsv1.Deployment
		if err := yaml.UnmarshalStrict([]byte(d), &dep); err != nil {
			t.Errorf("document is not a valid Deployment: %v\n%s", err, d)
		}
		if len(dep.Spec.Template.Spec.Containers) != 1 {
			t.Errorf("document %s/%s has %d containers, want 1", dep.Namespace, dep.Name, len(dep.Spec.Template.Spec.Containers))
		}
	}
}

// Same deployment name in two namespaces must not be merged into one document.
func TestRenderKeepsNamespacesApart(t *testing.T) {
	got, err := Render([]recommend.Recommendation{
		rec("dev", "api", "app", "50m", "100m", "64Mi", "64Mi"),
		rec("prod", "api", "app", "250m", "500m", "256Mi", "256Mi"),
	}, 0.95)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if n := strings.Count(got, "kind: Deployment"); n != 2 {
		t.Errorf("got %d documents, want 2:\n%s", n, got)
	}
}

func TestRenderOmitsCPULimitWhenNil(t *testing.T) {
	got, err := Render([]recommend.Recommendation{
		rec("prod", "api", "app", "250m", "", "256Mi", "256Mi"),
	}, 0.95)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	var dep appsv1.Deployment
	if err := yaml.UnmarshalStrict([]byte(got), &dep); err != nil {
		t.Fatalf("invalid patch: %v\n%s", err, got)
	}
	lim := dep.Spec.Template.Spec.Containers[0].Resources.Limits
	if _, ok := lim["cpu"]; ok {
		t.Errorf("cpu limit present, want it omitted:\n%s", got)
	}
	if lim.Memory().String() != "256Mi" {
		t.Errorf("memory limit = %q, want 256Mi", lim.Memory().String())
	}
}

func TestRenderLabelsTheQuantile(t *testing.T) {
	for _, tt := range []struct {
		q    float64
		want string
	}{{0.95, "p95"}, {0.99, "p99"}, {0.5, "p50"}} {
		got, err := Render([]recommend.Recommendation{
			rec("prod", "api", "app", "250m", "500m", "256Mi", "256Mi"),
		}, tt.q)
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		if !strings.Contains(got, tt.want+" cpu") {
			t.Errorf("quantile %v: output missing %q label:\n%s", tt.q, tt.want, got)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	tests := []struct {
		in   float64
		want string
	}{
		{512, "512.0B"},
		{2048, "2.0KiB"},
		{200 << 20, "200.0MiB"},
		{3 << 30, "3.0GiB"},
	}
	for _, tt := range tests {
		if got := humanBytes(tt.in); got != tt.want {
			t.Errorf("humanBytes(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
