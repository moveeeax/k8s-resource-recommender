package recommend

import (
	"math"
	"testing"
)

// TestAcceptanceSteady200m is the acceptance criterion from the spec: a
// container sitting at a steady 200m must be recommended ~250m.
func TestAcceptanceSteady200m(t *testing.T) {
	got := For(Usage{CPUCores: 0.200, MemBytes: 200 << 20, Pods: 3}, DefaultOptions())
	if s := got.CPURequest.String(); s != "250m" {
		t.Fatalf("cpu request = %q, want 250m", s)
	}
}

func TestForCPU(t *testing.T) {
	tests := []struct {
		name  string
		cores float64
		want  string
	}{
		{"steady 200m", 0.200, "250m"},
		{"steady 1 core", 1.0, "1250m"},
		{"tiny sidecar hits the floor", 0.0001, "10m"},
		{"exactly zero hits the floor", 0, "10m"},
		{"rounds up to the 10m step", 0.111, "140m"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := For(Usage{CPUCores: tt.cores}, DefaultOptions()).CPURequest
			if got.String() != tt.want {
				t.Errorf("cpu request for %v cores = %q, want %q", tt.cores, got.String(), tt.want)
			}
		})
	}
}

func TestForMemory(t *testing.T) {
	tests := []struct {
		name  string
		bytes float64
		want  string
	}{
		{"200Mi rounds to 256Mi", 200 << 20, "256Mi"},
		{"512Mi plus headroom", 512 << 20, "640Mi"},
		{"tiny hits the 16Mi floor", 1024, "16Mi"},
		{"1Gi plus headroom", 1 << 30, "1280Mi"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := For(Usage{MemBytes: tt.bytes}, DefaultOptions()).MemRequest
			if got.String() != tt.want {
				t.Errorf("memory request for %v bytes = %q, want %q", tt.bytes, got.String(), tt.want)
			}
		})
	}
}

func TestLimitsDerivedFromRoundedRequest(t *testing.T) {
	got := For(Usage{CPUCores: 0.200, MemBytes: 200 << 20}, DefaultOptions())

	if got.CPULimit == nil {
		t.Fatal("expected a CPU limit by default")
	}
	// 250m request x 2.0 = 500m, not 200m x 1.25 x 2 rounded some other way.
	if s := got.CPULimit.String(); s != "500m" {
		t.Errorf("cpu limit = %q, want 500m", s)
	}
	// Memory limit factor defaults to 1.0: request == limit.
	if got.MemLimit.String() != got.MemRequest.String() {
		t.Errorf("memory limit %q != request %q, want them equal by default",
			got.MemLimit.String(), got.MemRequest.String())
	}
}

func TestOmitCPULimit(t *testing.T) {
	o := DefaultOptions()
	o.OmitCPULimit = true
	if got := For(Usage{CPUCores: 0.2}, o); got.CPULimit != nil {
		t.Errorf("cpu limit = %v, want nil when OmitCPULimit is set", got.CPULimit)
	}
}

func TestDeploymentFromPod(t *testing.T) {
	tests := []struct {
		pod  string
		want string
	}{
		{"api-7d9f8b6c5d-x2k4p", "api"},
		{"checkout-service-5f4bcd-abc12", "checkout-service"},
		{"my-app-79dd8bc9f9-2zzzz", "my-app"},
		// Not Deployment-shaped: returned unchanged rather than mangled.
		{"postgres-0", "postgres-0"},
		{"standalone", "standalone"},
		{"node-exporter-abcd", "node-exporter-abcd"},
		// A hash-looking tail must not eat a real name segment.
		{"api", "api"},
	}
	for _, tt := range tests {
		if got := DeploymentFromPod(tt.pod); got != tt.want {
			t.Errorf("DeploymentFromPod(%q) = %q, want %q", tt.pod, got, tt.want)
		}
	}
}

func TestAggregateTakesMaxAcrossPods(t *testing.T) {
	cpu := map[string]float64{
		SeriesKey("prod", "api-7d9f8b6c5d-aaaaa", "app"): 0.10,
		SeriesKey("prod", "api-7d9f8b6c5d-bbbbb", "app"): 0.40, // the busy replica
		SeriesKey("prod", "api-7d9f8b6c5d-ccccc", "app"): 0.15,
	}
	mem := map[string]float64{
		SeriesKey("prod", "api-7d9f8b6c5d-aaaaa", "app"): 100 << 20,
		SeriesKey("prod", "api-7d9f8b6c5d-bbbbb", "app"): 300 << 20,
		SeriesKey("prod", "api-7d9f8b6c5d-ccccc", "app"): 120 << 20,
	}

	got := Aggregate(cpu, mem)
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(got), got)
	}
	u := got[0]
	if u.Deployment != "api" || u.Container != "app" || u.Namespace != "prod" {
		t.Errorf("unexpected key: %+v", u)
	}
	if math.Abs(u.CPUCores-0.40) > 1e-9 {
		t.Errorf("CPUCores = %v, want 0.40 (max across pods)", u.CPUCores)
	}
	if u.MemBytes != 300<<20 {
		t.Errorf("MemBytes = %v, want %v (max across pods)", u.MemBytes, 300<<20)
	}
	if u.Pods != 3 {
		t.Errorf("Pods = %d, want 3", u.Pods)
	}
}

func TestAggregateSortsAndSeparatesContainers(t *testing.T) {
	cpu := map[string]float64{
		SeriesKey("prod", "web-aaaaaaaaa-11111", "sidecar"): 0.01,
		SeriesKey("prod", "web-aaaaaaaaa-11111", "app"):     0.20,
		SeriesKey("dev", "web-aaaaaaaaa-22222", "app"):      0.05,
	}
	got := Aggregate(cpu, nil)
	if len(got) != 3 {
		t.Fatalf("got %d rows, want 3", len(got))
	}
	want := []Key{
		{"dev", "web", "app"},
		{"prod", "web", "app"},
		{"prod", "web", "sidecar"},
	}
	for i, w := range want {
		if got[i].Namespace != w.Namespace || got[i].Deployment != w.Deployment || got[i].Container != w.Container {
			t.Errorf("row %d = %v/%v/%v, want %v", i, got[i].Namespace, got[i].Deployment, got[i].Container, w)
		}
	}
}

func TestAggregateHandlesCPUOnlyAndMemOnlySeries(t *testing.T) {
	// A container can legitimately appear in one query and not the other, e.g.
	// a brand-new container with no CPU samples yet. It must still show up.
	cpu := map[string]float64{SeriesKey("prod", "a-aaaaaaaaa-11111", "app"): 0.5}
	mem := map[string]float64{SeriesKey("prod", "b-bbbbbbbbb-22222", "app"): 50 << 20}

	got := Aggregate(cpu, mem)
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2", len(got))
	}
	if got[0].MemBytes != 0 {
		t.Errorf("deployment a MemBytes = %v, want 0", got[0].MemBytes)
	}
	if got[1].CPUCores != 0 {
		t.Errorf("deployment b CPUCores = %v, want 0", got[1].CPUCores)
	}
}

func TestAggregateSkipsMalformedKeys(t *testing.T) {
	cpu := map[string]float64{
		"no-separators-at-all":                           1,
		SeriesKey("prod", "", "app"):                     1,
		SeriesKey("prod", "api-7d9f8b6c5d-aaaaa", ""):    1,
		SeriesKey("prod", "api-7d9f8b6c5d-aaaaa", "app"): 1,
	}
	if got := Aggregate(cpu, nil); len(got) != 1 {
		t.Errorf("got %d rows, want 1: %+v", len(got), got)
	}
}

func TestOptionsValidate(t *testing.T) {
	bad := []struct {
		name string
		mut  func(*Options)
	}{
		{"negative headroom", func(o *Options) { o.Headroom = -0.1 }},
		{"cpu limit factor below 1", func(o *Options) { o.CPULimitFactor = 0.5 }},
		{"mem limit factor below 1", func(o *Options) { o.MemLimitFactor = 0 }},
		{"zero cpu step", func(o *Options) { o.CPUStepMilli = 0 }},
		{"zero mem step", func(o *Options) { o.MemStepMi = 0 }},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			o := DefaultOptions()
			tt.mut(&o)
			if err := o.Validate(); err == nil {
				t.Error("expected a validation error, got nil")
			}
		})
	}
	if err := DefaultOptions().Validate(); err != nil {
		t.Errorf("DefaultOptions() must validate, got %v", err)
	}
}

func TestZeroHeadroomIsRespected(t *testing.T) {
	o := DefaultOptions()
	o.Headroom = 0
	got := For(Usage{CPUCores: 0.2}, o).CPURequest
	if s := got.String(); s != "200m" {
		t.Errorf("cpu request with zero headroom = %q, want 200m", s)
	}
}

func TestStatus(t *testing.T) {
	// Usage 0.2 cores / 200Mi recommends 250m / 256Mi with the defaults.
	u := Usage{CPUCores: 0.2, MemBytes: 200 << 20}
	tests := []struct {
		name string
		cur  Current
		want Status
	}{
		{"nothing set", Current{}, StatusUnset},
		{"only limits set", Current{CPULimit: 1, MemLimit: 1 << 30}, StatusUnset},
		{"close to the recommendation", Current{CPURequest: 0.3, MemRequest: 256 << 20}, StatusOK},
		{"cpu far above", Current{CPURequest: 2, MemRequest: 256 << 20}, StatusOver},
		{"memory far above", Current{CPURequest: 0.25, MemRequest: 1 << 30}, StatusOver},
		{"only cpu set and far above", Current{CPURequest: 2}, StatusOver},
		{"usage above the cpu request", Current{CPURequest: 0.1, MemRequest: 1 << 30}, StatusUnder},
		// Under wins over over: memory is short even though CPU is slack.
		{"under wins over over", Current{CPURequest: 4, MemRequest: 64 << 20}, StatusUnder},
		// Between usage and usage+headroom is not under: the container fits.
		{"inside the headroom", Current{CPURequest: 0.21, MemRequest: 210 << 20}, StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := u
			u.Current = tt.cur
			if got := For(u, DefaultOptions()).Status; got != tt.want {
				t.Errorf("status = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStatusOverFactor(t *testing.T) {
	u := Usage{CPUCores: 0.2, MemBytes: 200 << 20, Current: Current{CPURequest: 0.5, MemRequest: 256 << 20}}
	o := DefaultOptions()
	if got := For(u, o).Status; got != StatusOver {
		t.Fatalf("500m vs 250m at factor 1.3 = %q, want over", got)
	}
	o.OverFactor = 2.5
	if got := For(u, o).Status; got != StatusOK {
		t.Errorf("500m vs 250m at factor 2.5 = %q, want ok", got)
	}
}

func TestApplyCurrent(t *testing.T) {
	usage := Aggregate(map[string]float64{
		SeriesKey("prod", "api-7d9f8b6c5d-aaaaa", "app"): 0.2,
		SeriesKey("prod", "db-0", "postgres"):            0.5,
	}, nil)
	ApplyCurrent(usage, CurrentSeries{
		CPURequest: map[string]float64{
			// A rollout in flight: the new ReplicaSet asks for less. Max wins.
			SeriesKey("prod", "api-7d9f8b6c5d-bbbbb", "app"): 1,
			SeriesKey("prod", "api-5f6b7c8d9e-ccccc", "app"): 0.5,
			SeriesKey("prod", "db-0", "postgres"):            2,
			// Same container name in another workload must not leak in.
			SeriesKey("prod", "web-7d9f8b6c5d-ddddd", "app"): 8,
		},
		MemRequest: map[string]float64{SeriesKey("prod", "api-7d9f8b6c5d-bbbbb", "app"): 128 << 20},
		CPULimit:   map[string]float64{SeriesKey("prod", "api-7d9f8b6c5d-bbbbb", "app"): 2},
		MemLimit:   map[string]float64{SeriesKey("prod", "api-7d9f8b6c5d-bbbbb", "app"): 256 << 20},
		Replicas:   map[string]float64{WorkloadKey("prod", "api"): 3},
	})

	api, db := usage[0].Current, usage[1].Current
	want := Current{CPURequest: 1, MemRequest: 128 << 20, CPULimit: 2, MemLimit: 256 << 20, Pods: 2, Replicas: 3}
	if api != want {
		t.Errorf("api current = %+v, want %+v", api, want)
	}
	// No Deployment replicas series for a StatefulSet pod: Replicas stays 0.
	if db != (Current{CPURequest: 2, Pods: 1}) {
		t.Errorf("db current = %+v", db)
	}
}

func TestSummarize(t *testing.T) {
	o := DefaultOptions()
	over := func(cpu, mem float64, c Current) Recommendation {
		return For(Usage{CPUCores: cpu, MemBytes: mem, Pods: 9, Current: c}, o)
	}
	recs := []Recommendation{
		// 2 cores -> 250m, 1Gi -> 256Mi, 3 replicas: 5.25 cores, 2304Mi.
		over(0.2, 200<<20, Current{CPURequest: 2, MemRequest: 1 << 30, Replicas: 3}),
		// Over on CPU only; memory request is below the recommendation and
		// must not subtract. No replicas series: falls back to 2 current pods.
		over(0.2, 200<<20, Current{CPURequest: 1, MemRequest: 210 << 20, Pods: 2}),
		// ok and under rows are compared but never reclaimable.
		over(0.2, 200<<20, Current{CPURequest: 0.3, MemRequest: 256 << 20}),
		over(0.2, 200<<20, Current{CPURequest: 0.1}),
		over(0.2, 200<<20, Current{}),
	}
	s := Summarize(recs, Pricing{CPUCoreMonth: 10, MemGiBMonth: 4})
	if s.Compared != 4 || s.Over != 2 {
		t.Errorf("compared/over = %d/%d, want 4/2", s.Compared, s.Over)
	}
	if want := 5.25 + 1.5; math.Abs(s.CPUCores-want) > 1e-9 {
		t.Errorf("cpu = %v, want %v", s.CPUCores, want)
	}
	if want := float64(2304 << 20); s.MemBytes != want {
		t.Errorf("mem = %v, want %v", s.MemBytes, want)
	}
	if want := 6.75*10 + 2.25*4; math.Abs(s.MonthlyCost-want) > 1e-9 {
		t.Errorf("cost = %v, want %v", s.MonthlyCost, want)
	}
	if got := Summarize(recs, Pricing{}).MonthlyCost; got != 0 {
		t.Errorf("cost without pricing = %v, want 0", got)
	}
}

func TestValidateOverFactor(t *testing.T) {
	o := DefaultOptions()
	o.OverFactor = 0.5
	if err := o.Validate(); err == nil {
		t.Error("over-factor 0.5 accepted")
	}
}
