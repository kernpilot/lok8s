// wait.go — kapply::wait_ready: wait for the Deployments / DaemonSets /
// StatefulSets IN THE MANIFEST to be ready — scoped to exactly what we just
// applied, NOT the whole cluster (so it never blocks on app workloads or
// addons applied later). Best-effort: a timeout is a ⚠, never fatal — the
// caller decides whether to care.
package kapply

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/ui"
)

type waitTarget struct {
	kind string // lowercased
	ns   string
	name string
}

// workloadItem is the readiness-relevant subset of a deploy/ds/sts object.
type workloadItem struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Replicas *int `json:"replicas"`
	} `json:"spec"`
	Status struct {
		AvailableReplicas      int `json:"availableReplicas"`
		ReadyReplicas          int `json:"readyReplicas"`
		DesiredNumberScheduled int `json:"desiredNumberScheduled"`
		NumberReady            int `json:"numberReady"`
	} `json:"status"`
}

// WaitReady polls until every Deployment/DaemonSet/StatefulSet in the
// manifest reports ready, or timeoutSec elapses (bash: kapply::wait_ready).
// A manifest with no workloads is a no-op. Always returns nil — best-effort
// by contract. Off a terminal it stays quiet (debug each poll, warn on
// timeout); the live spinner is the same documented simplification as Run.
func (a *Applier) WaitReady(ctx context.Context, label string, timeoutSec int, manifest string, kubectlFlags ...string) error {
	var targets []waitTarget
	seen := map[waitTarget]bool{}
	for _, d := range parseManifestDocs(manifest) {
		switch d.kind {
		case "Deployment", "DaemonSet", "StatefulSet":
			ns := d.namespace
			if ns == "" {
				ns = "default"
			}
			t := waitTarget{kind: strings.ToLower(d.kind), ns: ns, name: d.name}
			if !seen[t] {
				seen[t] = true
				targets = append(targets, t)
			}
		}
	}
	if len(targets) == 0 {
		return nil
	}
	// bash sorts the target list (sort -u).
	sort.Slice(targets, func(i, j int) bool {
		a, b := targets[i], targets[j]
		return a.kind+"|"+a.ns+"|"+a.name < b.kind+"|"+b.ns+"|"+b.name
	})

	start := time.Now()
	for {
		args := append(append([]string{}, kubectlFlags...),
			"get", "deploy,ds,sts", "--all-namespaces", "-o", "json")
		var buf strings.Builder
		snap := "{}"
		if err := a.Runner.Run(ctx, execx.Cmd{Name: "kubectl", Args: args, Stdout: &buf, Stderr: io.Discard}); err == nil {
			snap = buf.String()
		}
		var list struct {
			Items []workloadItem `json:"items"`
		}
		_ = json.Unmarshal([]byte(snap), &list)

		var pending []string
		for _, t := range targets {
			if !workloadReady(list.Items, t) {
				pending = append(pending, t.name)
			}
		}
		if len(pending) == 0 {
			if ttyUI() {
				fmt.Fprintf(a.Stdout, "\033[32m✓\033[0m %s · ready\n", label)
			}
			return nil
		}
		elapsed := int(time.Since(start).Seconds())
		if elapsed >= timeoutSec {
			if ttyUI() {
				fmt.Fprintf(a.Stdout, "\033[33m⚠\033[0m %s · timed out after %ds, %d not ready: \033[2m%.50s\033[0m\n",
					label, timeoutSec, len(pending), strings.Join(pending, " "))
			} else {
				ui.Warnf(a.Stderr, "%s: timed out after %ds; not ready: %s", label, timeoutSec, strings.Join(pending, " "))
			}
			return nil
		}
		ui.Debugf(a.Stderr, "%s: waiting on %d: %s", label, len(pending), strings.Join(pending, " "))
		a.sleep(a.pollInterval())
	}
}

func workloadReady(items []workloadItem, t waitTarget) bool {
	for _, it := range items {
		if strings.ToLower(it.Kind) != t.kind || it.Metadata.Namespace != t.ns || it.Metadata.Name != t.name {
			continue
		}
		replicas := 1
		if it.Spec.Replicas != nil {
			replicas = *it.Spec.Replicas
		}
		switch t.kind {
		case "daemonset":
			return it.Status.DesiredNumberScheduled > 0 &&
				it.Status.NumberReady >= it.Status.DesiredNumberScheduled
		case "statefulset":
			return it.Status.ReadyReplicas >= replicas
		default:
			return it.Status.AvailableReplicas >= replicas
		}
	}
	return false
}
