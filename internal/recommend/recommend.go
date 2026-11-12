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
	// OverFactor marks a container over-provisioned when its current request
	// is more than this many times the recommendation.
	OverFactor float64
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
		OverFactor:     1.3,
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
	case o.OverFactor < 1:
		return fmt.Errorf("over-factor must be >= 1, got %v", o.OverFactor)
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
	// Current is what the workload runs with today, filled by ApplyCurrent.
	Current Current
}

// Current holds the requests and limits a container runs with now, as
// kube-state-metrics reports them. Zero means the value is not set (or the
// metric is missing), which is how kube-state-metrics itself represents it:
// it emits no series for an unset request.
type Current struct {
	// CPURequest / CPULimit are in cores, MemRequest / MemLimit in bytes.
	CPURequest float64
	CPULimit   float64
	MemRequest float64
	MemLimit   float64
	// Pods counts the pods that report a request right now.
	Pods int
	// Replicas is the Deployment's spec.replicas, 0 when unknown.
	Replicas int
}

// Status compares the current request with observed usage and the
// recommendation.
type Status string

const (
	// StatusUnset means no current request is known, so there is nothing to
	// compare against.
	StatusUnset Status = "unset"
	// StatusUnder means observed usage is above the current request: the
	// container is throttled or at OOM risk today.
	StatusUnder Status = "under"
	// StatusOver means the current request exceeds the recommendation by more
	// than Options.OverFactor.
	StatusOver Status = "over"
	// StatusOK is everything in between.
	StatusOK Status = "ok"
)

// Recommendation is the rounded result for one container.
type Recommendation struct {
	Usage
	CPURequest resource.Quantity
	CPULimit   *resource.Quantity
	MemRequest resource.Quantity
	MemLimit   resource.Quantity
	Status     Status
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

	r.Status = classify(r, o.OverFactor)
	return r
}

// classify applies the rule ported from k8s-rightsize: under wins over over.
// A container short on one resource is the urgent signal even when the other
// resource looks slack, so it is never reported as reclaimable.
func classify(r Recommendation, overFactor float64) Status {
	c := r.Current
	if c.CPURequest <= 0 && c.MemRequest <= 0 {
		return StatusUnset
	}
	if (c.CPURequest > 0 && r.CPUCores > c.CPURequest) || (c.MemRequest > 0 && r.MemBytes > c.MemRequest) {
		return StatusUnder
	}
	recCPU, recMem := CPUCores(r.CPURequest), float64(r.MemRequest.Value())
	if (c.CPURequest > 0 && c.CPURequest > recCPU*overFactor) || (c.MemRequest > 0 && c.MemRequest > recMem*overFactor) {
		return StatusOver
	}
	return StatusOK
}

// CPUCores converts a CPU quantity to cores.
func CPUCores(q resource.Quantity) float64 {
	return float64(q.MilliValue()) / 1000
}

// CurrentSeries is the raw kube-state-metrics data ApplyCurrent folds in. The
// per-container maps are keyed by SeriesKey, Replicas by WorkloadKey.
type CurrentSeries struct {
	CPURequest map[string]float64
	MemRequest map[string]float64
	CPULimit   map[string]float64
	MemLimit   map[string]float64
	Replicas   map[string]float64
}

// WorkloadKey builds the map key for per-Deployment series such as
// kube_deployment_spec_replicas.
func WorkloadKey(namespace, deployment string) string {
	return namespace + "\x00" + deployment
}

// ApplyCurrent fills Usage.Current from kube-state-metrics series, mapping
// pods to Deployments the same way Aggregate does and taking the maximum
// across replicas. During a rollout old and new pods can disagree; the larger
// value is the one the scheduler still has to find room for.
func ApplyCurrent(usages []Usage, s CurrentSeries) {
	type acc struct {
		cur  Current
		pods map[string]struct{}
	}
	byKey := map[Key]*acc{}
	fold := func(series map[string]float64, set func(*Current, float64), isRequest bool) {
		for key, v := range series {
			ns, pod, container, ok := parseSeries(key)
			if !ok {
				continue
			}
			k := Key{Namespace: ns, Deployment: DeploymentFromPod(pod), Container: container}
			a := byKey[k]
			if a == nil {
				a = &acc{pods: map[string]struct{}{}}
				byKey[k] = a
			}
			set(&a.cur, v)
			if isRequest {
				a.pods[pod] = struct{}{}
			}
		}
	}
	fold(s.CPURequest, func(c *Current, v float64) { c.CPURequest = math.Max(c.CPURequest, v) }, true)
	fold(s.MemRequest, func(c *Current, v float64) { c.MemRequest = math.Max(c.MemRequest, v) }, true)
	fold(s.CPULimit, func(c *Current, v float64) { c.CPULimit = math.Max(c.CPULimit, v) }, false)
	fold(s.MemLimit, func(c *Current, v float64) { c.MemLimit = math.Max(c.MemLimit, v) }, false)

	for i := range usages {
		u := &usages[i]
		if a := byKey[Key{Namespace: u.Namespace, Deployment: u.Deployment, Container: u.Container}]; a != nil {
			u.Current = a.cur
			u.Current.Pods = len(a.pods)
		}
		if n, ok := s.Replicas[WorkloadKey(u.Namespace, u.Deployment)]; ok && n > 0 {
			u.Current.Replicas = int(n)
		}
	}
}

// Pricing converts reclaimable capacity into a monthly estimate. Zero values
// disable the estimate for that resource.
type Pricing struct {
	CPUCoreMonth float64 // per requested core per month
	MemGiBMonth  float64 // per requested GiB per month
}

// Summary is the reclaimable total across all over-provisioned containers.
type Summary struct {
	// Compared counts rows with a known current request; Over those of them
	// with StatusOver.
	Compared int `json:"compared"`
	Over     int `json:"over"`
	// CPUCores and MemBytes are request capacity freed by applying the
	// recommendation to every replica of the over-provisioned containers.
	CPUCores float64 `json:"reclaimableCpuCores"`
	MemBytes float64 `json:"reclaimableMemoryBytes"`
	// MonthlyCost is the estimate from Pricing, 0 when no price was given.
	MonthlyCost float64 `json:"monthlyCost,omitempty"`
}

// Summarize totals what applying the recommendations would free. Only
// over-provisioned containers count: an ok container's small surplus is
// inside the headroom, and an under one needs more, not less.
func Summarize(recs []Recommendation, p Pricing) Summary {
	var s Summary
	for _, r := range recs {
		if r.Status == StatusUnset {
			continue
		}
		s.Compared++
		if r.Status != StatusOver {
			continue
		}
		s.Over++
		n := float64(replicas(r.Usage))
		if d := r.Current.CPURequest - CPUCores(r.CPURequest); d > 0 {
			s.CPUCores += d * n
		}
		if d := r.Current.MemRequest - float64(r.MemRequest.Value()); d > 0 {
			s.MemBytes += d * n
		}
	}
	s.MonthlyCost = s.CPUCores*p.CPUCoreMonth + s.MemBytes/(1<<30)*p.MemGiBMonth
	return s
}

// replicas prefers spec.replicas, then the pods running now. The pod count
// from the usage window is not used: it includes every pod a rollout or a
// reschedule replaced during the window.
func replicas(u Usage) int {
	switch {
	case u.Current.Replicas > 0:
		return u.Current.Replicas
	case u.Current.Pods > 0:
		return u.Current.Pods
	default:
		return 1
	}
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
