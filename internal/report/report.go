// Package report renders recommendations next to the current requests, as a
// table for people or JSON for scripts, plus the reclaimable total.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/moveeeax/k8s-resource-recommender/internal/recommend"
)

// Table writes one row per container:
//
//	WORKLOAD  CONTAINER  CPU cur->rec  MEM cur->rec  STATUS
//
// followed by the reclaimable line when any current request was known.
func Table(w io.Writer, recs []recommend.Recommendation, sum recommend.Summary) error {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "WORKLOAD\tCONTAINER\tCPU cur->rec\tMEM cur->rec\tSTATUS")
	for _, r := range recs {
		fmt.Fprintf(tw, "%s/%s\t%s\t%s->%s\t%s->%s\t%s\n",
			r.Namespace, r.Deployment, r.Container,
			orNone(CPU(r.Current.CPURequest)), r.CPURequest.String(),
			orNone(Mem(r.Current.MemRequest)), r.MemRequest.String(),
			r.Status,
		)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if line := Reclaimable(sum); line != "" {
		_, err := fmt.Fprintf(w, "\n%s\n", line)
		return err
	}
	return nil
}

// Reclaimable formats the summary as one line, e.g.
// "reclaimable: 1.38 CPU, 924Mi, ~$32.00/month (2 of 5 containers over)".
// It returns "" when no container had a current request to compare with,
// because a zero there would read as "nothing to save" rather than "unknown".
func Reclaimable(s recommend.Summary) string {
	if s.Compared == 0 {
		return ""
	}
	line := fmt.Sprintf("reclaimable: %.2f CPU, %s", s.CPUCores, memOrZero(s.MemBytes))
	if s.MonthlyCost > 0 {
		line += fmt.Sprintf(", ~$%.2f/month", s.MonthlyCost)
	}
	return line + fmt.Sprintf(" (%d of %d containers over)", s.Over, s.Compared)
}

// jsonRow is the per-container JSON shape. Usage and current values are plain
// numbers (cores, bytes) for arithmetic; recommended values are Kubernetes
// quantities, because those are what end up in a manifest.
type jsonRow struct {
	Namespace   string     `json:"namespace"`
	Workload    string     `json:"workload"`
	Container   string     `json:"container"`
	Pods        int        `json:"pods"`
	Usage       jsonUsage  `json:"usage"`
	Current     jsonCur    `json:"current"`
	Recommended jsonRecomm `json:"recommended"`
	Status      string     `json:"status"`
}

type jsonUsage struct {
	CPUCores float64 `json:"cpuCores"`
	MemBytes float64 `json:"memoryBytes"`
}

type jsonCur struct {
	CPURequest *float64 `json:"cpuRequestCores,omitempty"`
	MemRequest *float64 `json:"memoryRequestBytes,omitempty"`
	CPULimit   *float64 `json:"cpuLimitCores,omitempty"`
	MemLimit   *float64 `json:"memoryLimitBytes,omitempty"`
	Replicas   int      `json:"replicas,omitempty"`
}

type jsonRecomm struct {
	Requests map[string]string `json:"requests"`
	Limits   map[string]string `json:"limits"`
}

type jsonDoc struct {
	Recommendations []jsonRow         `json:"recommendations"`
	Summary         recommend.Summary `json:"summary"`
}

// JSON writes the recommendations and the summary as indented JSON.
func JSON(w io.Writer, recs []recommend.Recommendation, sum recommend.Summary) error {
	doc := jsonDoc{Recommendations: make([]jsonRow, 0, len(recs)), Summary: sum}
	for _, r := range recs {
		row := jsonRow{
			Namespace: r.Namespace,
			Workload:  r.Deployment,
			Container: r.Container,
			Pods:      r.Pods,
			Usage:     jsonUsage{CPUCores: r.CPUCores, MemBytes: r.MemBytes},
			Current: jsonCur{
				CPURequest: nonZero(r.Current.CPURequest),
				MemRequest: nonZero(r.Current.MemRequest),
				CPULimit:   nonZero(r.Current.CPULimit),
				MemLimit:   nonZero(r.Current.MemLimit),
				Replicas:   r.Current.Replicas,
			},
			Recommended: jsonRecomm{
				Requests: map[string]string{"cpu": r.CPURequest.String(), "memory": r.MemRequest.String()},
				Limits:   map[string]string{"memory": r.MemLimit.String()},
			},
			Status: string(r.Status),
		}
		if r.CPULimit != nil {
			row.Recommended.Limits["cpu"] = r.CPULimit.String()
		}
		doc.Recommendations = append(doc.Recommendations, row)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

// orNone spells an unset current value as "none" in the table, where "-"
// would run into the arrow.
func orNone(s string) string {
	if s == "-" {
		return "none"
	}
	return s
}

func nonZero(v float64) *float64 {
	if v <= 0 {
		return nil
	}
	return &v
}

// CPU formats cores the way manifests usually spell them: millicores below
// one core, plain cores above. Zero, meaning unset, renders as "-".
func CPU(cores float64) string {
	if cores <= 0 {
		return "-"
	}
	m := int64(cores*1000 + 0.5)
	if m%1000 == 0 {
		return fmt.Sprintf("%d", m/1000)
	}
	return fmt.Sprintf("%dm", m)
}

// Mem formats bytes with a Kubernetes binary suffix and at most one decimal,
// e.g. 128Mi or 1.5Gi. Zero, meaning unset, renders as "-".
func Mem(bytes float64) string {
	if bytes <= 0 {
		return "-"
	}
	units := []string{"", "Ki", "Mi", "Gi", "Ti"}
	i := 0
	for bytes >= 1024 && i < len(units)-1 {
		bytes /= 1024
		i++
	}
	s := strings.TrimSuffix(fmt.Sprintf("%.1f", bytes), ".0")
	return s + units[i]
}

func memOrZero(bytes float64) string {
	if bytes <= 0 {
		return "0Mi"
	}
	return Mem(bytes)
}
