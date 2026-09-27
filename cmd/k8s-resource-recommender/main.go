// Command k8s-resource-recommender prints right-sized Kubernetes resource
// requests and limits, derived from real Prometheus usage, as a
// strategic-merge patch.
package main

import (
	"os"

	"github.com/moveeeax/k8s-resource-recommender/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
