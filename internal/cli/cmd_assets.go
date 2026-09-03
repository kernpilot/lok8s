package cli

// lo assets — the eject model's own surface (Go-only; see internal/assets):
//
//	lo assets list                      every embedded asset + its origin
//	lo assets show <rel>                one asset: files, marker, state
//	lo assets eject [rel…|--all] [--check]
//	lo assets diff [rel…] [--json] [--check]
//	lo assets update <rel> [--force]
//
// The bash implementation reads .lok8s/** from disk and has no embedded copy
// to compare against, so there is no twin and no parity harness — the Go
// tests under internal/assets and cmd_assets_test.go are the gate.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/assets"
	"github.com/kernpilot/lok8s/internal/bootstrap"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/domain"
	"github.com/kernpilot/lok8s/internal/ui"
)

// ErrCheckFailed is the `--check` verdict: something would be ejected, or
// something drifted. main prints nothing for it (the command already did)
// and exits 1.
var ErrCheckFailed = ErrHandled

func newAssetsCommand(paths *config.Paths) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "assets",
		Short:        "Framework assets embedded in the binary: list, eject, diff, update",
		GroupID:      groupComponents,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		Annotations:  map[string]string{"lok8s.dev/readonly": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(
		newAssetsListCommand(paths),
		newAssetsShowCommand(paths),
		newAssetsEjectCommand(paths),
		newAssetsDiffCommand(paths),
		newAssetsUpdateCommand(paths),
	)
	return cmd
}

func newAssetsListCommand(paths *config.Paths) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:          "list",
		Short:        "List every embedded asset with its origin (builtin · local · local (modified) · local-only)",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			reports, err := assets.Report(paths, nil)
			if err != nil {
				return err
			}
			if asJSON {
				return writeAssetsJSON(cmd.OutOrStdout(), reports)
			}
			assets.WriteTable(cmd.OutOrStdout(), reports, false)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Machine-readable output")
	return cmd
}

func newAssetsShowCommand(paths *config.Paths) *cobra.Command {
	return &cobra.Command{
		Use:          "show <rel>",
		Short:        "Show one asset: path, marker, chart version, per-file state",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			reports, err := assets.Report(paths, []string{args[0]})
			if err != nil {
				return assetsErr(cmd.ErrOrStderr(), err)
			}
			assets.WriteShow(cmd.OutOrStdout(), reports[0])
			return nil
		},
	}
}

func newAssetsEjectCommand(paths *config.Paths) *cobra.Command {
	var all, check bool
	cmd := &cobra.Command{
		Use:   "eject [rel...]",
		Short: "Write embedded assets into the project (.lok8s/<rel>/ + .lo-origin); default: what the cluster specs reference",
		Long: `Materialize embedded framework assets into the project so what a cluster
applies is pinned on disk. Without arguments the set is what this project's
cluster specs reference (every builtin spec.bootstrap addon, the driver's
cluster templates, the inventory CRD); --all ejects every embedded asset.
An existing local copy is never touched. --check writes nothing and exits 1
when any of the set would be ejected — the CI gate for "this repo pins what
it applies".`,
		Args:         cobra.ArbitraryArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			out, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
			rels := args
			switch {
			case all:
				for _, u := range assets.Units() {
					rels = append(rels, u.Rel)
				}
			case len(rels) == 0:
				rels = referencedAssets(paths, stderr)
			}
			var pending []string
			for _, rel := range rels {
				if _, ok := assets.UnitFor(rel); !ok {
					return assetsErr(stderr, fmt.Errorf("%w: %s", assets.ErrNotAsset, rel))
				}
				if !assets.LocalExists(paths, rel) {
					pending = append(pending, rel)
				}
			}
			sort.Strings(pending)
			pending = dedupe(pending)
			if check {
				if len(pending) == 0 {
					fmt.Fprintln(out, "assets: nothing to eject")
					return nil
				}
				for _, rel := range pending {
					fmt.Fprintf(out, "would eject %s\n", rel)
				}
				ui.Errorf(stderr, "assets: %d asset(s) would be ejected (run: lo assets eject)", len(pending))
				return ErrCheckFailed
			}
			if len(pending) == 0 {
				fmt.Fprintln(out, "assets: nothing to eject (every referenced asset has a local copy)")
				return nil
			}
			for _, rel := range pending {
				if _, err := assets.Eject(paths, rel); err != nil {
					return assetsErr(stderr, err)
				}
			}
			fmt.Fprintf(out, "assets: ejected %d asset(s) into %s\n", len(pending), relOrAbs(paths, paths.Lok8s))
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "Eject every embedded asset, not only the referenced ones")
	cmd.Flags().BoolVar(&check, "check", false, "Write nothing; exit 1 if any asset would be ejected")
	return cmd
}

func newAssetsDiffCommand(paths *config.Paths) *cobra.Command {
	var asJSON, check bool
	cmd := &cobra.Command{
		Use:   "diff [rel...]",
		Short: "Three-way diff: origin (.lo-origin) vs local vs the copy embedded in this lo",
		Long: `Per file: unchanged · local modified · lo updated · both (conflict) ·
local-only · builtin-only. The headline per addon is the chart version
(local vs embedded). --check exits 1 on any drift.`,
		Args:         cobra.ArbitraryArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			out, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
			reports, err := assets.Report(paths, args)
			if err != nil {
				return assetsErr(stderr, err)
			}
			if asJSON {
				if err := writeAssetsJSON(out, reports); err != nil {
					return err
				}
			} else {
				assets.WriteTable(out, reports, len(args) > 0)
			}
			if check && assets.AnyDrift(reports) {
				n := 0
				for _, r := range reports {
					if r.Drifted {
						n++
					}
				}
				ui.Errorf(stderr, "assets: %d asset(s) drifted from the binary's copy (lo assets diff <rel> for the files)", n)
				return ErrCheckFailed
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Machine-readable output (stable shape)")
	cmd.Flags().BoolVar(&check, "check", false, "Exit 1 on any drift")
	return cmd
}

func newAssetsUpdateCommand(paths *config.Paths) *cobra.Command {
	return &cobra.Command{
		Use:          "update <rel>",
		Short:        "Apply the embedded copy over the local one — only when local == origin (else --force)",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			force, _ := cmd.Flags().GetBool("force")
			_, err := assets.Update(paths, args[0], force, cmd.OutOrStdout())
			return assetsErr(cmd.ErrOrStderr(), err)
		},
	}
}

// assetsErr prints an assets error the bash way ([error] on stderr) and
// hands back the handled sentinel; other errors pass through.
func assetsErr(stderr io.Writer, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, assets.ErrNotAsset), errors.Is(err, assets.ErrInvalidRel),
		errors.Is(err, assets.ErrExists), errors.Is(err, assets.ErrConflict):
		ui.Errorf(stderr, "%v", err)
		return ErrHandled
	}
	return err
}

// assetsJSON is the stable --json shape.
type assetsJSON struct {
	Lo     string             `json:"lo"`
	Assets []assets.UnitReport `json:"assets"`
}

func writeAssetsJSON(w io.Writer, reports []assets.UnitReport) error {
	if reports == nil {
		reports = []assets.UnitReport{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(assetsJSON{Lo: assets.Version(), Assets: reports})
}

// referencedAssets is the set a project applies: for every cluster spec
// under clusters/, its builtin spec.bootstrap addons and the driver's
// cluster templates, plus the inventory CRD every provision publishes.
func referencedAssets(paths *config.Paths, stderr io.Writer) []string {
	set := map[string]bool{"libs/inventory/manifests": true}
	entries, _ := os.ReadDir(paths.Clusters)
	for _, e := range entries {
		if !e.IsDir() || !domain.NameRe.MatchString(e.Name()) {
			continue
		}
		spec := filepath.Join(paths.Clusters, e.Name(), "cluster.lok8s.yaml")
		if !fileExists(spec) {
			continue
		}
		kind, err := domain.SpecDriver(spec, "lo")
		if err != nil {
			continue
		}
		if _, ok := assets.UnitFor("drivers/" + kind + "/cluster"); ok {
			set["drivers/"+kind+"/cluster"] = true
		}
		raw, _ := bootstrap.ResolveEntries(spec, kind)
		for _, r := range raw {
			if r == "" {
				continue
			}
			parsed, err := bootstrap.ParseEntry(paths, io.Discard, e.Name(), r)
			if err != nil || !parsed.Builtin {
				continue
			}
			rel := "addons/" + filepath.Base(parsed.Dir)
			if _, ok := assets.UnitFor(rel); ok {
				set[rel] = true
			}
		}
	}
	rels := make([]string, 0, len(set))
	for rel := range set {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	return rels
}

func dedupe(s []string) []string {
	var out []string
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

func relOrAbs(paths *config.Paths, p string) string {
	if rel, err := filepath.Rel(paths.Base, p); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return p
}
