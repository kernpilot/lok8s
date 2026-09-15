package cli

// confirm.go: a confirmation on a terminal before the destructive
// commands remove anything: `lo down`, `lo clean`, `lo destroy` (the local
// kind driver), `lo registry clean` and `lo image clean`. The prompt lists
// the concrete objects first, then asks. The names come from the code
// that removes them (lodriver.Removal, lodriver.ProxyContainer,
// image.CacheRegistry, the cluster name of the run), never from a second
// copy of a naming formula.
//
// The gate is stdin AND stderr on a terminal: the prompt writes to
// stderr and reads the answer from stdin. `lo down | cat` still asks;
// `lo down < /dev/null` and a script do not (the parity harnesses redirect
// both, so they see no prompt). No environment variable silences the
// prompt; `--yes` is the only skip. `--force` overrides a precondition
// and leaves the prompt alone.
//
// A command that refuses (a malformed `.kind`, a non-Lo domain, a cloud
// driver with its own gate) refuses before any prompt: one question per
// run at most. The guards are installed by command path after the tree
// is assembled, so the command files stay as they are.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/build"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/domain"
	lodriver "github.com/kernpilot/lok8s/internal/driver/lo"
	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/image"
	"github.com/kernpilot/lok8s/internal/provision"
	"github.com/kernpilot/lok8s/internal/ui"
)

// confirmTerminals reports whether stdin and stderr are terminals (tests
// swap it). No environment variable takes part.
var confirmTerminals = func() (stdin, stderr bool) {
	return ui.StdinIsTerminal(), ui.Stderr().TTY
}

// confirmIn is where the answer is read from (tests swap it).
var confirmIn io.Reader = os.Stdin

// removal is one line of the plan: what kind of object, and which.
type removal struct{ what, value string }

// removalPlan names what a command removes; nil when the command has its
// own gate or refuses on its own for this run, so no prompt here.
type removalPlan func(cmd *cobra.Command) []removal

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
	guard("down", func(cmd *cobra.Command) []removal { return downPlan(cmd, paths, true) })
	guard("clean", func(cmd *cobra.Command) []removal {
		// runClean: `--all` prunes docker after the teardown and stops;
		// without it the cluster's volumes and the whole registry set go
		// (containers, volumes, the certificate volume): one registry line.
		all, _ := cmd.Flags().GetBool("all")
		items := downPlan(cmd, paths, all)
		if items == nil {
			return nil
		}
		if all {
			return append(items, removal{"docker", "system prune -f (every unused image, container, network)"})
		}
		d, cluster := ambientMain(cmd, paths)
		items = append(items, removal{"docker volumes", "every volume named " + cluster + "-*"})
		return append(items, registrySetRemovals(paths, d)...)
	})
	guard("destroy", func(cmd *cobra.Command) []removal {
		d, cluster := ambientMain(cmd, paths)
		if remote, _ := cmd.Flags().GetBool("remote"); remote || specKind(paths, d) != "lo" {
			return nil // the driver's own gate, or a dispatch that refuses
		}
		items := []removal{{"kind cluster", cluster}}
		items = append(items, kubeconfigRemoval(paths, d)...)
		items = append(items, registrySetRemovals(paths, d)...)
		return append(items, removal{"proxy container", lodriver.ProxyContainer(cluster)})
	})
	guard("registry clean", func(cmd *cobra.Command) []removal {
		d := ambientMainEnv(cmd, paths)
		if domain.RequireDriver("lo", paths.Clusters, d, "", io.Discard) != nil {
			return nil // the command's driver gate refuses
		}
		items := registrySetRemovals(paths, d)
		// RegistryClean --shared removes the shared-network name of every
		// mirror and the file's network, whatever the set's sharing mode.
		if shared, _ := cmd.Flags().GetBool("shared"); shared {
			if rem, err := lodriver.RegistryRemoval(paths, d); err == nil {
				if len(rem.Mirrors) > 0 {
					items = append(items, removal{"shared mirror containers and volumes", strings.Join(rem.Mirrors, ", ")})
				}
				if rem.Network != "" {
					items = append(items, removal{"docker network", rem.Network})
				}
			}
		}
		return items
	})
	guard("image clean", func(cmd *cobra.Command) []removal {
		d := ambientMainEnv(cmd, paths)
		if domain.RequireDriver("lo", paths.Clusters, d, "", io.Discard) != nil {
			return nil
		}
		// ambientMainEnv exported the network the command reads.
		name := image.CacheRegistry(os.Getenv("KIND_EXPERIMENTAL_DOCKER_NETWORK"))
		return []removal{{"cache registry container and volume", name}}
	})
}

// confirmRemoval prints the plan and asks; nil means go ahead. Ctrl-C
// while the prompt waits cancels the run with nothing removed.
func confirmRemoval(cmd *cobra.Command, yes bool, plan removalPlan) error {
	if yes {
		return nil
	}
	if stdin, stderr := confirmTerminals(); !stdin || !stderr {
		return nil
	}
	items := plan(cmd)
	if len(items) == 0 {
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
	answer, err := readAnswer(cmd.Context(), confirmIn)
	if err != nil {
		return ui.Handled(err) // the interrupt's exit code, nothing printed
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return nil
	}
	fmt.Fprintf(errOut, "aborted: nothing removed (answer y, or pass --yes)\n")
	return ErrHandled
}

// readAnswer reads one line from in, or returns ctx.Err() when the
// context ends first. The read runs on its own goroutine: a read that
// still blocks on a terminal ends with the process.
func readAnswer(ctx context.Context, in io.Reader) (string, error) {
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := bufio.NewReader(in).ReadString('\n')
		if err != nil && line == "" {
			ch <- result{"", err}
			return
		}
		ch <- result{line, nil}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case r := <-ch:
		if r.err != nil {
			return "", nil // EOF: no answer, read as no
		}
		return r.line, nil
	}
}

// downPlan is what `lo down` removes for the local driver: the tilt
// process, the kind cluster, the kubeconfig context, and (registries)
// the project registry containers, their volumes kept. nil when the run
// refuses (a malformed .kind) or a cloud driver's dispatch gates it.
func downPlan(cmd *cobra.Command, paths *config.Paths, registries bool) []removal {
	d, cluster := ambientMain(cmd, paths)
	spec := filepath.Join(paths.Clusters, d, "cluster.lok8s.yaml")
	if fsutil.FileExists(spec) {
		k, err := provision.ReadKind(spec, io.Discard)
		if err != nil || k != "lo" {
			return nil
		}
	}
	var items []removal
	if pid := tiltPID(paths); pid != "" {
		items = append(items, removal{"tilt", "running (pid " + pid + "), stopped"})
	}
	items = append(items, removal{"kind cluster", cluster})
	items = append(items, kubeconfigRemoval(paths, d)...)
	if !registries {
		return items
	}
	rem, err := lodriver.RegistryRemoval(paths, d)
	switch {
	case err != nil:
		items = append(items, removal{"registries", registryFileNote(paths, d)})
	case rem.IsShared:
		items = append(items, removal{"registries", "shared mirrors stay up (lo registry down removes them)"})
	case len(rem.Registries) > 0:
		items = append(items, removal{"registry containers", strings.Join(rem.Registries, ", ") + " (volumes stay)"})
	}
	return items
}

// registrySetRemovals lists a full registry teardown of the domain's set,
// read from its registry file: the containers and data volumes, and the
// certificate volume when the set has one. Without a file the run names
// them itself; the plan says so.
func registrySetRemovals(paths *config.Paths, d string) []removal {
	rem, err := lodriver.RegistryRemoval(paths, d)
	if err != nil {
		return []removal{{"registry set", registryFileNote(paths, d)}}
	}
	var items []removal
	if len(rem.Registries) > 0 {
		items = append(items, removal{"registry containers and volumes", strings.Join(rem.Registries, ", ")})
	}
	if rem.TLSVolume != "" {
		items = append(items, removal{"TLS certificate volume", rem.TLSVolume})
	}
	return items
}

// registryFileNote is the line for a domain whose registry file is not
// there: the run generates it from the spec, the plan does not write.
func registryFileNote(paths *config.Paths, d string) string {
	return "the set named by the spec (no " + config.RelTo(paths.Base, filepath.Join(paths.Clusters, d, ".registries.json")) + " yet)"
}

// specKind is the domain's driver kind, "" without a cluster spec.
func specKind(paths *config.Paths, d string) string {
	k, err := domain.SpecDriver(filepath.Join(paths.Clusters, d, "cluster.lok8s.yaml"), "")
	if err != nil {
		return ""
	}
	return k
}

// kubeconfigRemoval names the written kubeconfig: the file stays, kind
// drops the cluster's context from it.
func kubeconfigRemoval(paths *config.Paths, d string) []removal {
	kc := build.AmbientKubeconfig(paths, d, "")
	if !fsutil.FileExists(kc) {
		return nil
	}
	return []removal{{"kubeconfig", config.RelTo(paths.Base, kc) + " (the file stays, kind drops its context)"}}
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
