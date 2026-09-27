// Package recommend turns raw Prometheus usage samples into per-container
// resource recommendations.
package recommend

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
)

// Options controls how observed usage is converted into requests and limits.
type Options struct {
	// Headroom is the fraction added on top of observed usage, e.g. 0.25 for
	// a 25% cushion above the measured quantile.
	Headroom float64
	// CPULimitFactor multiplies the CPU request to get the CPU limit.
	CPULimitFactor float64
	// MemLimitFactor multiplies the memory request to get the memory limit.
	MemLimitFactor float64
	// OmitCPULimit drops the CPU limit entirely to avoid CFS throttling.
	OmitCPULimit bool
	// MinCPUMilli / MinMemMi are floors so idle sidecars do not get
	// unschedulably tiny values.
	MinCPUMilli int64
	MinMemMi    int64
	// CPUStepMilli / MemStepMi round requests up to a readable multiple.
	CPUStepMilli int64
	MemStepMi    int64
}

// DefaultOptions are tuned so a container sitting at a steady 200m lands on a
// 250m request: 200m x (1 + 0.25) = 250m.
func DefaultOptions() Options {
	return Options{
		Headroom:       0.25,
		CPULimitFactor: 2.0,
		MemLimitFactor: 1.0,
		MinCPUMilli:    10,
		MinMemMi:       16,
		CPUStepMilli:   10,
		MemStepMi:      16,
	}
}

// Validate rejects option combinations that would produce nonsense output.
func (o Options) Validate() error {
	switch {
	case o.Headroom < 0:
		return fmt.Errorf("headroom must be >= 0, got %v", o.Headroom)
	case o.CPULimitFactor < 1:
		return fmt.Errorf("cpu-limit-factor must be >= 1, got %v", o.CPULimitFactor)
	case o.MemLimitFactor < 1:
		return fmt.Errorf("mem-limit-factor must be >= 1, got %v", o.MemLimitFactor)
	case o.CPUStepMilli < 1:
		return fmt.Errorf("cpu-step must be >= 1m, got %v", o.CPUStepMilli)
	case o.MemStepMi < 1:
		return fmt.Errorf("mem-step must be >= 1Mi, got %v", o.MemStepMi)
	}
	return nil
}

// Usage is the observed quantile usage for one container of one workload.
type Usage struct {
	Namespace  string
	Deployment string
	Container  string
	// CPUCores is in cores (1.0 == one full core), MemBytes in bytes.
	CPUCores float64
	MemBytes float64
	// Pods counts how many pods contributed to this row.
	Pods int
}

// Recommendation is the rounded result for one container.
type Recommendation struct {
	Usage
	CPURequest resource.Quantity
	CPULimit   *resource.Quantity
	MemRequest resource.Quantity
	MemLimit   resource.Quantity
}

// podSuffix matches the "-<replicaset-hash>-<pod-hash>" tail that a Deployment
// appends to its pods. The ReplicaSet hash is a base-36-ish token and the pod
// suffix is always 5 characters from the same alphabet.
var podSuffix = regexp.MustCompile(`-[a-z0-9]{5,10}-[a-z0-9]{5}$`)

// DeploymentFromPod derives the owning Deployment name from a pod name.
//
// This is a heuristic: the recommender is read-only against Prometheus and has
// no API-server access, so there is no ownerReference to walk. Pods that do not
// look Deployment-owned (bare pods, StatefulSet ordinals, DaemonSet pods) are
// returned unchanged and reported as-is, which is honest rather than wrong.
func DeploymentFromPod(pod string) string {
	if m := podSuffix.FindStringIndex(pod); m != nil {
		return pod[:m[0]]
	}
	return pod
}

// Key identifies one container of one workload.
type Key struct {
	Namespace  string
	Deployment string
	Container  string
}

// Aggregate folds per-pod samples into per-(namespace, deployment, container)
// rows, taking the maximum across pods. Max, not mean: replicas are rarely
// balanced, and sizing off the average guarantees the busiest pod is starved.
func Aggregate(cpu, mem map[string]float64) []Usage {
	type acc struct {
		cpu, mem float64
		pods     map[string]struct{}
	}
	byKey := map[Key]*acc{}

	add := func(series string, value float64, isCPU bool) {
		ns, pod, container, ok := parseSeries(series)
		if !ok {
			return
		}
		k := Key{Namespace: ns, Deployment: DeploymentFromPod(pod), Container: container}
		a := byKey[k]
		if a == nil {
			a = &acc{pods: map[string]struct{}{}}
			byKey[k] = a
		}
		a.pods[pod] = struct{}{}
		if isCPU {
			a.cpu = math.Max(a.cpu, value)
		} else {
			a.mem = math.Max(a.mem, value)
		}
	}

	for s, v := range cpu {
		add(s, v, true)
	}
	for s, v := range mem {
		add(s, v, false)
	}

	out := make([]Usage, 0, len(byKey))
	for k, a := range byKey {
		out = append(out, Usage{
			Namespace:  k.Namespace,
			Deployment: k.Deployment,
			Container:  k.Container,
			CPUCores:   a.cpu,
			MemBytes:   a.mem,
			Pods:       len(a.pods),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		if out[i].Deployment != out[j].Deployment {
			return out[i].Deployment < out[j].Deployment
		}
		return out[i].Container < out[j].Container
	})
	return out
}

// SeriesKey builds the opaque map key used by Aggregate. Keeping the encoding
// in one place means callers never have to agree on a separator.
func SeriesKey(namespace, pod, container string) string {
	return namespace + "\x00" + pod + "\x00" + container
}

func parseSeries(s string) (namespace, pod, container string, ok bool) {
	parts := strings.Split(s, "\x00")
	if len(parts) != 3 || parts[1] == "" || parts[2] == "" {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

// For builds the rounded recommendation for one usage row.
func For(u Usage, o Options) Recommendation {
	cpuMilli := roundUp(int64(math.Ceil(u.CPUCores*1000*(1+o.Headroom))), o.CPUStepMilli)
	if cpuMilli < o.MinCPUMilli {
		cpuMilli = roundUp(o.MinCPUMilli, o.CPUStepMilli)
	}

	memMi := roundUp(int64(math.Ceil(u.MemBytes*(1+o.Headroom)/(1<<20))), o.MemStepMi)
	if memMi < o.MinMemMi {
		memMi = roundUp(o.MinMemMi, o.MemStepMi)
	}

	r := Recommendation{
		Usage:      u,
		CPURequest: *resource.NewMilliQuantity(cpuMilli, resource.DecimalSI),
		MemRequest: *resource.NewQuantity(memMi*(1<<20), resource.BinarySI),
	}

	// Limits are derived from the already-rounded request, then re-rounded, so
	// the output never shows a limit like 437m next to a 250m request.
	if !o.OmitCPULimit {
		lim := roundUp(int64(math.Ceil(float64(cpuMilli)*o.CPULimitFactor)), o.CPUStepMilli)
		r.CPULimit = resource.NewMilliQuantity(lim, resource.DecimalSI)
	}
	memLim := roundUp(int64(math.Ceil(float64(memMi)*o.MemLimitFactor)), o.MemStepMi)
	r.MemLimit = *resource.NewQuantity(memLim*(1<<20), resource.BinarySI)

	return r
}

// roundUp rounds v up to the next multiple of step.
func roundUp(v, step int64) int64 {
	if step <= 1 {
		return v
	}
	if rem := v % step; rem != 0 {
		v += step - rem
	}
	return v
}
