package cli

// completion.go: dynamic shell completion (Go-only). cobra's built-in
// `lo completion bash|zsh|fish|powershell` emits the script. This file adds
// the values the script asks the binary for: domains under clusters/ for
// `lo use <Tab>` and every `--domain` / `--cluster-override` flag, addon
// names for `lo addons <Tab>`, asset rels for `lo assets eject|diff|update
// <Tab>`, and service names for `lo init service <Tab>`. Every completer
// reads the project tree only: no docker, no network, no eject, so a Tab
// can never change the project.
//
// The hooks are installed by command path after the tree is built
// (installCompletions), so the files that own those commands stay as they
// are. A command routed to bash (a shim) keeps flag parsing off, and the
// completion request never runs the command. There the shell completes
// the name, the positional values, and --domain when it precedes the
// command (`lo --domain <Tab> down`); after the name cobra skips flag
// values under DisableFlagParsing.

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/addons"
	"github.com/kernpilot/lok8s/internal/assets"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/fsutil"
)

// installCompletions wires the dynamic completers into the assembled tree.
func installCompletions(root *cobra.Command, paths *config.Paths) {
	domains := func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return completeDomains(paths), cobra.ShellCompDirectiveNoFileComp
	}
	// The global --domain flag is one pflag.Flag shared by every command,
	// so one registration on the root serves the whole tree.
	_ = root.RegisterFlagCompletionFunc("domain", domains)

	setArgs := func(path string, fn cobra.CompletionFunc) {
		if cmd := findByPath(root, path); cmd != nil {
			cmd.ValidArgsFunction = fn
		}
	}
	setArgs("use", func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return completeDomains(paths), cobra.ShellCompDirectiveNoFileComp
	})
	setArgs("audit", func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return completeDomains(paths), cobra.ShellCompDirectiveNoFileComp
	})
	setArgs("recover", func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return completeDomains(paths), cobra.ShellCompDirectiveNoFileComp
	})
	setArgs("addons", func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
		return without(addons.Names(paths), args), cobra.ShellCompDirectiveNoFileComp
	})
	setArgs("init service", func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return completeServices(paths), cobra.ShellCompDirectiveNoFileComp
	})
	rels := func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
		return without(completeAssetRels(), args), cobra.ShellCompDirectiveNoFileComp
	}
	setArgs("assets eject", rels)
	setArgs("assets diff", rels)
	setArgs("assets update", func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return completeAssetRels(), cobra.ShellCompDirectiveNoFileComp
	})
	// Every Go driver takes a positional domain (`lo drivers lo status <domain>`).
	if drivers := findByPath(root, "drivers"); drivers != nil {
		for _, drv := range drivers.Commands() {
			for _, op := range drv.Commands() {
				op.ValidArgsFunction = func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
					if len(args) > 0 {
						return nil, cobra.ShellCompDirectiveNoFileComp
					}
					return completeDomains(paths), cobra.ShellCompDirectiveNoFileComp
				}
			}
		}
	}
	// --cluster-override (build, deploy, kubeconfig) names a cluster domain.
	walkCommands(root, func(cmd *cobra.Command) {
		if cmd.Flags().Lookup("cluster-override") != nil {
			_ = cmd.RegisterFlagCompletionFunc("cluster-override", domains)
		}
	})
}

// findByPath resolves a command by its space-separated path below the
// root ("init service"). nil when the path names no command.
func findByPath(root *cobra.Command, path string) *cobra.Command {
	cmd, rest, err := root.Find(strings.Fields(path))
	if err != nil || len(rest) > 0 || cmd == root {
		return nil
	}
	return cmd
}

// walkCommands visits every command below root, depth first.
func walkCommands(root *cobra.Command, visit func(*cobra.Command)) {
	for _, c := range root.Commands() {
		visit(c)
		walkCommands(c, visit)
	}
}

// completeDomains lists the domains under clusters/: every directory that
// holds a cluster.lok8s.yaml or a deploy.lok8s.yaml, sorted.
func completeDomains(paths *config.Paths) []string {
	entries, err := os.ReadDir(paths.Clusters)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dir := filepath.Join(paths.Clusters, e.Name())
		if fsutil.FileExists(filepath.Join(dir, "cluster.lok8s.yaml")) || fsutil.FileExists(filepath.Join(dir, "deploy.lok8s.yaml")) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// completeServices lists the names `lo init service` can take: the
// services registered in services.yaml and the directories below the
// project root that hold a service file (a lok8s.yaml with no kind or
// with kind: Service; a kind: Project file is a project, not a service),
// sorted and unique.
func completeServices(paths *config.Paths) []string {
	seen := map[string]bool{}
	if raw, err := os.ReadFile(filepath.Join(paths.Base, "services.yaml")); err == nil {
		var doc struct {
			Services map[string]any `yaml:"services"`
		}
		if yaml.Unmarshal(raw, &doc) == nil {
			for name := range doc.Services {
				seen[name] = true
			}
		}
	}
	entries, _ := os.ReadDir(paths.Base)
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if isServiceFile(filepath.Join(paths.Base, e.Name(), "lok8s.yaml")) {
			seen[e.Name()] = true
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// isServiceFile reports whether path is a service lok8s.yaml: readable
// YAML with no kind, or kind: Service.
func isServiceFile(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var doc struct {
		Kind string `yaml:"kind"`
	}
	if yaml.Unmarshal(raw, &doc) != nil {
		return false
	}
	return doc.Kind == "" || doc.Kind == "Service"
}

// completeAssetRels lists the asset units `lo assets` addresses, plus the
// word `bash` for the frozen implementation.
func completeAssetRels() []string {
	units := assets.Units()
	rels := make([]string, 0, len(units)+1)
	for _, u := range units {
		rels = append(rels, u.Rel)
	}
	rels = append(rels, "bash")
	sort.Strings(rels)
	return rels
}

// without drops the values already typed on the line.
func without(values, typed []string) []string {
	if len(typed) == 0 {
		return values
	}
	skip := map[string]bool{}
	for _, t := range typed {
		skip[t] = true
	}
	out := make([]string, 0, len(values))
	for _, v := range values {
		if !skip[v] {
			out = append(out, v)
		}
	}
	return out
}
