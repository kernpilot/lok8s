package cli

// orientation.go: `lo` with no arguments. On a terminal the binary
// prints where you stand (the project, the active domain and its driver,
// the kubeconfig when one was written) and the six everyday commands,
// then the one next step. Off a terminal (a pipe, a script, CI) it prints
// cobra's full help exactly as before, so nothing that parsed `lo` output
// changes. `lo --help` prints the full help everywhere. Go-only: the bash
// entrypoint prints its usage in both cases.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/build"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/domain"
	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/ui"
)

// everydayCommands are the six commands the orientation lists, in the
// order of a working day. Their one-line descriptions come from the tree.
var everydayCommands = []string{"up", "status", "build", "deploy", "lint", "down"}

// runOrientation is the root's RunE for a bare `lo`. The stdout style
// decides (ui.Stdout, ForceTTY in tests): a terminal gets the block, a
// pipe gets the help. A terminal on stdin with stdout piped is a script
// that captures output, so it gets the help too.
func runOrientation(cmd *cobra.Command, paths *config.Paths) error {
	if !ui.Stdout().TTY {
		return cmd.Help()
	}
	writeOrientation(cmd.OutOrStdout(), cmd.Root(), paths)
	return nil
}

// orientation is what the block shows, gathered from the tree only (no
// docker, no kind, no network: a bare `lo` must answer at once).
type orientation struct {
	project    string // metadata.name of lok8s.yaml, else the base directory's name; "" outside a project
	domain     string // the active domain: --domain, DOMAIN_NAME, clusters/.active; "" when none is set
	driver     string // the domain's driver kind, "Deploy -> <ref>" for a deploy domain
	kubeconfig string // the domain's kubeconfig, project-relative, "" when the file is absent
	domains    int    // how many domains clusters/ holds
}

// gatherOrientation reads the project tree.
func gatherOrientation(cmd *cobra.Command, paths *config.Paths) orientation {
	o := orientation{project: orientationProject(paths), domains: len(completeDomains(paths))}
	explicit := ""
	if f := cmd.Flags().Lookup("domain"); f != nil && f.Changed {
		explicit = f.Value.String()
	}
	if explicit == "" && os.Getenv("DOMAIN_NAME") == "" && !fsutil.FileExists(filepath.Join(paths.Clusters, ".active")) {
		return o
	}
	o.domain = domain.Resolve(explicit, paths.Clusters, io.Discard)
	base := filepath.Join(paths.Clusters, o.domain)
	switch {
	case fsutil.FileExists(filepath.Join(base, "cluster.lok8s.yaml")):
		k, err := domain.SpecDriver(filepath.Join(base, "cluster.lok8s.yaml"), "?")
		if err != nil {
			k = "?"
		}
		o.driver = k
	case fsutil.FileExists(filepath.Join(base, "deploy.lok8s.yaml")):
		o.driver = "Deploy -> " + deployClusterRef(filepath.Join(base, "deploy.lok8s.yaml"))
	default:
		o.driver = "no spec"
	}
	if kc := build.AmbientKubeconfig(paths, o.domain, ""); fsutil.FileExists(kc) {
		if rel, err := filepath.Rel(paths.Base, kc); err == nil {
			o.kubeconfig = rel
		} else {
			o.kubeconfig = kc
		}
	}
	return o
}

// writeOrientation renders the block with the ui helpers: a title, the
// facts, the everyday commands as a section, then the one next step.
func writeOrientation(out io.Writer, root *cobra.Command, paths *config.Paths) {
	o := gatherOrientation(root, paths)
	if o.project == "" {
		ui.Title(out, "lok8s: no project here")
		fmt.Fprintln(out, ui.For(out).Dim("  · no lok8s.yaml with kind: Project, no clusters/"))
		fmt.Fprintln(out)
		ui.Next(out, "init", "scaffold a project here")
		fmt.Fprintln(out, "All commands: lo --help")
		return
	}
	ui.Title(out, "lok8s · "+o.project)
	switch {
	case o.domain == "" && o.domains == 0:
		fmt.Fprintln(out, "  domain      none (clusters/ holds no domain)")
	case o.domain == "":
		fmt.Fprintf(out, "  domain      none (%d available)\n", o.domains)
	default:
		fmt.Fprintf(out, "  domain      %s (%s)\n", o.domain, o.driver)
	}
	if o.kubeconfig != "" {
		fmt.Fprintf(out, "  kubeconfig  %s\n", o.kubeconfig)
	}
	fmt.Fprintln(out)
	ui.Section(out, "Everyday commands")
	for _, name := range everydayCommands {
		short := ""
		if c := findByPath(root, name); c != nil {
			short = c.Short
		}
		fmt.Fprintf(out, "  lo %-9s %s\n", name, short)
	}
	fmt.Fprintln(out)
	next, why := orientationNext(o)
	ui.Next(out, next, why)
	fmt.Fprintln(out, "All commands: lo --help")
}

// orientationNext is the one step the state calls for: the command
// (without `lo`) and the reason.
func orientationNext(o orientation) (cmd, why string) {
	switch {
	case o.domain == "" && o.domains == 0:
		return "init project --cluster <domain> --driver lo", "clusters/ holds no domain"
	case o.domain == "":
		return "use <domain>", "no domain is active"
	case o.driver == "no spec":
		return "use <domain>", o.domain + " has no cluster.lok8s.yaml or deploy.lok8s.yaml"
	case o.kubeconfig == "":
		return "up", "no cluster kubeconfig yet"
	default:
		return "status", "the cluster has a kubeconfig"
	}
}

// orientationProject is metadata.name of the project file, else the base
// directory's name when clusters/ marks a project, else "".
func orientationProject(paths *config.Paths) string {
	if raw, err := os.ReadFile(filepath.Join(paths.Base, "lok8s.yaml")); err == nil {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
		}
		if yaml.Unmarshal(raw, &doc) == nil && doc.Kind == "Project" {
			if name := strings.TrimSpace(doc.Metadata.Name); name != "" {
				return name
			}
			return filepath.Base(paths.Base)
		}
	}
	if fsutil.DirExists(paths.Clusters) {
		return filepath.Base(paths.Base)
	}
	return ""
}
