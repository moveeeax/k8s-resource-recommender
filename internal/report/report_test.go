package report

import (
	"testing"

	"github.com/moveeeax/k8s-resource-recommender/internal/recommend"
)

func TestCPU(t *testing.T) {
	for _, tt := range []struct {
		in   float64
		want string
	}{{0, "-"}, {0.25, "250m"}, {0.0125, "13m"}, {1, "1"}, {2, "2"}, {1.5, "1500m"}} {
		if got := CPU(tt.in); got != tt.want {
			t.Errorf("CPU(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestMem(t *testing.T) {
	for _, tt := range []struct {
		in   float64
		want string
	}{{0, "-"}, {512, "512"}, {64 << 20, "64Mi"}, {924 << 20, "924Mi"}, {1536 << 20, "1.5Gi"}, {1 << 30, "1Gi"}} {
		if got := Mem(tt.in); got != tt.want {
			t.Errorf("Mem(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestReclaimable(t *testing.T) {
	if got := Reclaimable(recommend.Summary{}); got != "" {
		t.Errorf("nothing compared = %q, want empty", got)
	}
	got := Reclaimable(recommend.Summary{Compared: 5, Over: 2, CPUCores: 1.38, MemBytes: 924 << 20, MonthlyCost: 32})
	if want := "reclaimable: 1.38 CPU, 924Mi, ~$32.00/month (2 of 5 containers over)"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	got = Reclaimable(recommend.Summary{Compared: 3})
	if want := "reclaimable: 0.00 CPU, 0Mi (0 of 3 containers over)"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
