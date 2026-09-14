package cli

// confirm.go: a confirmation on a terminal before the destructive
// commands remove anything: `lo down`, `lo clean`, `lo destroy` (the local
// kind driver), `lo registry clean` and `lo image clean`. The prompt lists
// the concrete objects first (the cluster name, the kubeconfig, the
// registry containers and volumes, the docker volumes), then asks. Off a
// terminal (a pipe, a script, CI, LOK8S_NONINTERACTIVE) nothing changes:
// no prompt, no new requirement, the command runs as before. `--yes`
// answers the prompt. It is not `--force`, which overrides a precondition
// (the cloud drivers' infrastructure gate, an immutable recreate) and
// leaves this prompt alone.
//
// The cloud drivers (kubeone, capi, kkp) already demand a literal yes in
// their own infrastructure gate (provision.ConfirmInfra), so `lo down` and
// `lo destroy` prompt here only for the local driver: one question per
// run, never two.
//
// The guards are installed by command path after the tree is assembled,
// so the command files (and cmd_registry.go, owned elsewhere) stay as
// they are.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/kernpilot/lok8s/internal/build"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/domain"
	"github.com/kernpilot/lok8s/internal/fsutil"
)

// confirmIsTerminal is the interactivity seam (tests swap it): stdin and
// stdout on a terminal, and neither LOK8S_NONINTERACTIVE nor CI set (the
// rule provision.ConfirmInfra applies).
var confirmIsTerminal = func() bool {
	if os.Getenv("LOK8S_NONINTERACTIVE") != "" {
		return false
	}
	if ci := os.Getenv("CI"); ci != "" && ci != "false" {
		return false
	}
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

// confirmIn is where the answer is read from (tests swap it).
var confirmIn io.Reader = os.Stdin

// removal is one line of the plan: what kind of object, and which.
type removal struct{ what, value string }

// removalPlan names what a command removes. skip is true when the command
// has its own gate for this run (a cloud driver), so no prompt here.
type removalPlan func(cmd *cobra.Command) (items []removal, skip bool)

// installConfirmations wraps the destructive commands' RunE.
func installConfirmations(root *cobra.Command, paths *config.Paths) {
	guard := func(path string, plan removalPlan) {
		cmd := findByPath(root, path)
		if cmd == nil || cmd.RunE == nil {
			return
		}
		var yes bool
		cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Answer the confirmation on a terminal (off a terminal there is none)")
		run := cmd.RunE
		cmd.RunE = func(cmd *cobra.Command, args []string) error {
			if err := confirmRemoval(cmd, yes, plan); err != nil {
				return err
			}
			return run(cmd, args)
		}
	}
	guard("down", func(cmd *cobra.Command) ([]removal, bool) { return localTeardownPlan(cmd, paths, false) })
	guard("clean", func(cmd *cobra.Command) ([]removal, bool) {
		items, skip := localTeardownPlan(cmd, paths, true)
		if all, _ := cmd.Flags().GetBool("all"); all {
			items = append(items, removal{"docker", "system prune -f (every unused image, container, network)"})
		}
		return items, skip
	})
	guard("destroy", func(cmd *cobra.Command) ([]removal, bool) {
		d, cluster := ambientMain(cmd, paths)
		if k := specKind(paths, d); k != "lo" {
			return nil, true // the driver's own gate, or a deploy domain the dispatch refuses
		}
		return append([]removal{{"kind cluster", cluster}}, kubeconfigRemoval(paths, d)...), false
	})
	guard("registry clean", func(cmd *cobra.Command) ([]removal, bool) {
		d := ambientMainEnv(cmd, paths)
		shared, _ := cmd.Flags().GetBool("shared")
		rf, ok := readRegistriesFile(paths, d)
		if !ok {
			return []removal{{"registry containers and volumes", "the set of " + d}}, false
		}
		items := []removal{{"registry containers and volumes", strings.Join(rf.containers(shared), ", ")}}
		if shared && rf.Shared {
			items = append(items, removal{"docker network", rf.Network.Name})
		}
		return items, false
	})
	guard("image clean", func(cmd *cobra.Command) ([]removal, bool) {
		d := ambientMainEnv(cmd, paths)
		volume := "the cache registry volume of " + d
		if rf, ok := readRegistriesFile(paths, d); ok {
			volume = rf.ProjectNetwork + "-registry-cache"
		}
		return []removal{{"docker volume", volume}}, false
	})
}

// confirmRemoval prints the plan and asks. Nil means go ahead.
func confirmRemoval(cmd *cobra.Command, yes bool, plan removalPlan) error {
	if yes || !confirmIsTerminal() {
		return nil
	}
	items, skip := plan(cmd)
	if skip || len(items) == 0 {
		return nil
	}
	errOut := cmd.ErrOrStderr()
	fmt.Fprintf(errOut, "%s removes:\n", cmd.CommandPath())
	width := 0
	for _, it := range items {
		width = max(width, len(it.what))
	}
	for _, it := range items {
		fmt.Fprintf(errOut, "  %-*s  %s\n", width, it.what, it.value)
	}
	fmt.Fprint(errOut, "Continue? [y/N] ")
	answer, _ := bufio.NewReader(confirmIn).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return nil
	}
	fmt.Fprintf(errOut, "aborted: nothing removed (answer y, or pass --yes)\n")
	return ErrHandled
}

// localTeardownPlan is what `lo down` (and `lo clean`, withVolumes) removes
// for the local driver. Skip for a cloud driver, whose dispatch prompts.
func localTeardownPlan(cmd *cobra.Command, paths *config.Paths, withVolumes bool) ([]removal, bool) {
	d, cluster := ambientMain(cmd, paths)
	if k := specKind(paths, d); k != "" && k != "lo" {
		return nil, true
	}
	items := []removal{{"kind cluster", cluster}}
	items = append(items, kubeconfigRemoval(paths, d)...)
	if pid := tiltPID(paths); pid != "" {
		items = append(items, removal{"tilt", "running (pid " + pid + "), stopped"})
	}
	if withVolumes {
		items = append(items, removal{"docker volumes", "every volume named " + cluster + "-*"})
	}
	if rf, ok := readRegistriesFile(paths, d); ok {
		switch {
		case rf.Shared:
			items = append(items, removal{"registries", "shared mirrors stay up (lo registry down removes them)"})
		case withVolumes:
			items = append(items, removal{"registry containers and volumes", strings.Join(rf.containers(false), ", ")})
		default:
			items = append(items, removal{"registry containers", strings.Join(rf.containers(false), ", ") + " (volumes stay)"})
		}
	}
	return items, false
}

// specKind is the domain's driver kind, "" without a cluster spec.
func specKind(paths *config.Paths, d string) string {
	k, err := domain.SpecDriver(filepath.Join(paths.Clusters, d, "cluster.lok8s.yaml"), "")
	if err != nil {
		return ""
	}
	return k
}

// kubeconfigRemoval names the written kubeconfig, when there is one.
func kubeconfigRemoval(paths *config.Paths, d string) []removal {
	kc := build.AmbientKubeconfig(paths, d, "")
	if !fsutil.FileExists(kc) {
		return nil
	}
	return []removal{{"kubeconfig", config.RelTo(paths.Base, kc)}}
}

// tiltPID is the pid in .tilt.pid when that process is alive.
func tiltPID(paths *config.Paths) string {
	raw, err := os.ReadFile(filepath.Join(paths.Base, ".tilt.pid"))
	if err != nil {
		return ""
	}
	pid := strings.TrimSpace(string(raw))
	if pid == "" || !pidAlive(pid) {
		return ""
	}
	return pid
}

// registriesFile is the part of clusters/<domain>/.registries.json the
// plan needs (internal/driver/lo owns the full shape).
type registriesFile struct {
	Shared         bool   `json:"shared"`
	ProjectNetwork string `json:"project_network"`
	Network        struct {
		Name string `json:"name"`
	} `json:"network"`
	Registries []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"registries"`
}

// containers names the containers of the set the way the driver does:
// <project network>-registry-<name>, and the shared mirrors as
// lok8s-registry-<name> when shared is asked for.
func (rf *registriesFile) containers(shared bool) []string {
	var names []string
	for _, r := range rf.Registries {
		if rf.Shared && r.Type == "mirror" {
			if shared {
				names = append(names, "lok8s-registry-"+r.Name)
			}
			continue
		}
		names = append(names, rf.ProjectNetwork+"-registry-"+r.Name)
	}
	return names
}

// readRegistriesFile reads the domain's registry file, ok=false without one.
func readRegistriesFile(paths *config.Paths, d string) (*registriesFile, bool) {
	raw, err := os.ReadFile(filepath.Join(paths.Clusters, d, ".registries.json"))
	if err != nil {
		return nil, false
	}
	var rf registriesFile
	if json.Unmarshal(raw, &rf) != nil || len(rf.Registries) == 0 {
		return nil, false
	}
	return &rf, true
}
