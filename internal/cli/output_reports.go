package cli

// output_reports.go: the structured forms of `lo doctor` and `lo status`
// (`-o json|yaml`). The doctor report is read off the text report's own
// lines (every check prints through doctorOK/Warn/Bad, every section
// opens with `--- name ---`), so the two forms can never disagree. The
// status report gathers the same facts runStatus prints, as data.

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/domain"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/fsutil"
)

// ── doctor ──

// doctorReport is `lo doctor -o json|yaml`.
type doctorReport struct {
	Domain   string          `json:"domain"`
	OK       bool            `json:"ok"`
	Sections []doctorSection `json:"sections"`
}

type doctorSection struct {
	Name   string        `json:"name"`
	Checks []doctorCheck `json:"checks"`
}

type doctorCheck struct {
	Status  string `json:"status"` // ok | warn | bad | info
	Message string `json:"message"`
}

var (
	ansiRe          = regexp.MustCompile(`\x1b\[[0-9;]*m`)
	doctorSectionRe = regexp.MustCompile(`^--- (.*) ---$`)
)

// doctorCollector is the io.Writer runDoctor reports into for -o: it
// turns the report's lines into sections and checks.
type doctorCollector struct {
	buf      strings.Builder
	sections []doctorSection
}

func (c *doctorCollector) Write(p []byte) (int, error) {
	c.buf.Write(p)
	for {
		line, rest, found := strings.Cut(c.buf.String(), "\n")
		if !found {
			return len(p), nil
		}
		c.line(line)
		c.buf.Reset()
		c.buf.WriteString(rest)
	}
}

func (c *doctorCollector) line(raw string) {
	line := ansiRe.ReplaceAllString(raw, "")
	if m := doctorSectionRe.FindStringSubmatch(line); m != nil {
		c.sections = append(c.sections, doctorSection{Name: m[1], Checks: []doctorCheck{}})
		return
	}
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "===") || strings.HasPrefix(trimmed, "doctor:") {
		return
	}
	if len(c.sections) == 0 {
		c.sections = append(c.sections, doctorSection{Name: "report", Checks: []doctorCheck{}})
	}
	status := "info"
	switch {
	case strings.HasPrefix(trimmed, "✓ "):
		status, trimmed = "ok", strings.TrimPrefix(trimmed, "✓ ")
	case strings.HasPrefix(trimmed, "! "):
		status, trimmed = "warn", strings.TrimPrefix(trimmed, "! ")
	case strings.HasPrefix(trimmed, "✗ "):
		status, trimmed = "bad", strings.TrimPrefix(trimmed, "✗ ")
	}
	s := &c.sections[len(c.sections)-1]
	s.Checks = append(s.Checks, doctorCheck{Status: status, Message: trimmed})
}

// doctorStructured runs the doctor into a collector and returns the
// report. The error is the one the text form returns (ErrHandled when a required check
// failed), so the exit code is the same in every format.
func doctorStructured(ctx context.Context, paths *config.Paths, d string, toolchainFlag bool) (doctorReport, error) {
	c := &doctorCollector{}
	err := runDoctor(ctx, paths, d, toolchainFlag, c, io.Discard)
	if c.buf.Len() > 0 {
		c.line(c.buf.String())
	}
	sections := c.sections
	if sections == nil {
		sections = []doctorSection{}
	}
	return doctorReport{Domain: d, OK: err == nil, Sections: sections}, err
}

// ── status ──

// statusReport is `lo status -o json|yaml`.
type statusReport struct {
	Domain         string         `json:"domain"`
	Driver         string         `json:"driver"`
	Cluster        []string       `json:"cluster"` // the driver's status lines
	Nodes          []statusNode   `json:"nodes"`
	Inventory      map[string]any `json:"inventory"`
	Targets        []string       `json:"targets"`
	ArtifactsBuilt bool           `json:"artifactsBuilt"`
	Tilt           *statusTilt    `json:"tilt"`
}

type statusNode struct {
	Name           string `json:"name"`
	Ready          bool   `json:"ready"`
	Roles          string `json:"roles"`
	KubeletVersion string `json:"kubeletVersion"`
	InternalIP     string `json:"internalIP"`
}

type statusTilt struct {
	Running bool   `json:"running"`
	PID     string `json:"pid,omitempty"`
}

// gatherStatus collects what runStatus prints, section by section, with
// the same fail-soft rules (an unreachable cluster leaves nodes empty).
func gatherStatus(ctx context.Context, deps statusDeps, domainName string) statusReport {
	r := statusReport{Domain: domainName, Cluster: []string{}, Nodes: []statusNode{}, Targets: []string{}}
	if k, err := domain.SpecDriver(filepath.Join(deps.paths.Clusters, domainName, "cluster.lok8s.yaml"), ""); err == nil {
		r.Driver = k
	}

	var cluster strings.Builder
	_ = deps.dispatchStatus(ctx, &cluster, domainName)
	for line := range strings.SplitSeq(strings.TrimRight(cluster.String(), "\n"), "\n") {
		if line != "" {
			r.Cluster = append(r.Cluster, ansiRe.ReplaceAllString(line, ""))
		}
	}

	hasKubectl := deps.hasKubectl
	if hasKubectl == nil {
		hasKubectl = func() bool { _, ok := execx.Look(deps.paths, "kubectl"); return ok }
	}
	kubeconfig := os.Getenv("KUBECONFIG")
	if hasKubectl() && fsutil.FileExists(kubeconfig) {
		var nodes strings.Builder
		if err := deps.runner.Run(ctx, execx.Cmd{Name: "kubectl", Args: []string{"get", "nodes", "-o", "json"}, Stdout: &nodes, Stderr: io.Discard}); err == nil {
			r.Nodes = parseNodes(nodes.String())
		}
		var inv strings.Builder
		if err := deps.runner.Run(ctx, execx.Cmd{Name: "kubectl", Args: []string{"get", "clusterinventories.lok8s.dev", "cluster", "-o", "json"}, Stdout: &inv, Stderr: io.Discard}); err == nil {
			var doc map[string]any
			if json.Unmarshal([]byte(inv.String()), &doc) == nil {
				r.Inventory = doc
			}
		}
	}

	domainDir := filepath.Join(deps.paths.Clusters, domainName)
	if targets := discoverTargets(domainDir); targets != nil {
		r.Targets = targets
	}
	r.ArtifactsBuilt = fsutil.FileExists(filepath.Join(domainDir, "artifacts.yaml"))

	if r.Driver == "lo" {
		t := &statusTilt{}
		if raw, err := os.ReadFile(filepath.Join(deps.paths.Base, ".tilt.pid")); err == nil {
			pid := strings.TrimRight(string(raw), "\n")
			alive := deps.pidAlive
			if alive == nil {
				alive = pidAlive
			}
			if pid != "" && alive(pid) {
				t.Running, t.PID = true, pid
			}
		}
		r.Tilt = t
	}
	return r
}

// parseNodes reads `kubectl get nodes -o json` into the node rows.
func parseNodes(raw string) []statusNode {
	var list struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Status struct {
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
				Addresses []struct {
					Type    string `json:"type"`
					Address string `json:"address"`
				} `json:"addresses"`
				NodeInfo struct {
					KubeletVersion string `json:"kubeletVersion"`
				} `json:"nodeInfo"`
			} `json:"status"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(raw), &list) != nil {
		return []statusNode{}
	}
	nodes := []statusNode{}
	for _, it := range list.Items {
		n := statusNode{Name: it.Metadata.Name, KubeletVersion: it.Status.NodeInfo.KubeletVersion}
		for _, c := range it.Status.Conditions {
			if c.Type == "Ready" {
				n.Ready = c.Status == "True"
			}
		}
		for _, a := range it.Status.Addresses {
			if a.Type == "InternalIP" {
				n.InternalIP = a.Address
			}
		}
		var roles []string
		for k := range it.Metadata.Labels {
			if role, ok := strings.CutPrefix(k, "node-role.kubernetes.io/"); ok && role != "" {
				roles = append(roles, role)
			}
		}
		sort.Strings(roles)
		n.Roles = strings.Join(roles, ",")
		if n.Roles == "" {
			n.Roles = "<none>"
		}
		nodes = append(nodes, n)
	}
	return nodes
}
