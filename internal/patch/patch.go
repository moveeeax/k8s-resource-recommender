// Package patch renders recommendations as Kubernetes strategic-merge patches.
package patch

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/moveeeax/k8s-resource-recommender/internal/recommend"
	"github.com/moveeeax/k8s-resource-recommender/internal/report"
)

// container is one entry of the patched container list. The `name` field is the
// strategic-merge key, which is what lets this patch target one container
// without listing the rest of the pod spec.
type container struct {
	Name      string                      `json:"name"`
	Resources corev1.ResourceRequirements `json:"resources"`
}

type podSpec struct {
	Containers []container `json:"containers"`
}

type podTemplate struct {
	Spec podSpec `json:"spec"`
}

type deploymentSpec struct {
	Template podTemplate `json:"template"`
}

type objectMeta struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

type deploymentPatch struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Metadata   objectMeta     `json:"metadata"`
	Spec       deploymentSpec `json:"spec"`
}

// Render returns one YAML document per Deployment, joined with `---`.
// Containers of the same Deployment are merged into a single document so the
// output can be piped straight into `kubectl patch --patch-file`.
//
// quantile is only used to label the comment header, e.g. 0.95 renders as "p95".
func Render(recs []recommend.Recommendation, quantile float64) (string, error) {
	if len(recs) == 0 {
		return "", nil
	}

	label := fmt.Sprintf("p%g", quantile*100)

	// Recommendations arrive sorted by namespace/deployment/container, so a
	// single pass groups them without another sort.
	var docs []string
	var current *deploymentPatch
	var header string

	flush := func() error {
		if current == nil {
			return nil
		}
		out, err := yaml.Marshal(current)
		if err != nil {
			return fmt.Errorf("marshal patch for %s/%s: %w", current.Metadata.Namespace, current.Metadata.Name, err)
		}
		docs = append(docs, header+string(out))
		current = nil
		return nil
	}

	for _, r := range recs {
		if current == nil || current.Metadata.Name != r.Deployment || current.Metadata.Namespace != r.Namespace {
			if err := flush(); err != nil {
				return "", err
			}
			header = fmt.Sprintf("# %s/%s\n", r.Namespace, r.Deployment)
			current = &deploymentPatch{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Metadata:   objectMeta{Name: r.Deployment, Namespace: r.Namespace},
			}
		}

		requests := corev1.ResourceList{
			corev1.ResourceCPU:    r.CPURequest,
			corev1.ResourceMemory: r.MemRequest,
		}
		limits := corev1.ResourceList{
			corev1.ResourceMemory: r.MemLimit,
		}
		if r.CPULimit != nil {
			limits[corev1.ResourceCPU] = *r.CPULimit
		}

		header += fmt.Sprintf("#   %s: %s cpu %.3f cores, %s memory %s across %d pod(s)\n",
			r.Container, label, r.CPUCores, label, humanBytes(r.MemBytes), r.Pods)
		if r.Current.CPURequest > 0 || r.Current.MemRequest > 0 {
			header += fmt.Sprintf("#     current: requests cpu %s memory %s, limits cpu %s memory %s (%s)\n",
				report.CPU(r.Current.CPURequest), report.Mem(r.Current.MemRequest),
				report.CPU(r.Current.CPULimit), report.Mem(r.Current.MemLimit), r.Status)
		}

		current.Spec.Template.Spec.Containers = append(current.Spec.Template.Spec.Containers, container{
			Name:      r.Container,
			Resources: corev1.ResourceRequirements{Requests: requests, Limits: limits},
		})
	}
	if err := flush(); err != nil {
		return "", err
	}

	return strings.Join(docs, "---\n"), nil
}

// humanBytes formats a byte count with a binary suffix for the comment header.
func humanBytes(b float64) string {
	const unit = 1024.0
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	i := 0
	for b >= unit && i < len(units)-1 {
		b /= unit
		i++
	}
	return fmt.Sprintf("%.1f%s", b, units[i])
}
