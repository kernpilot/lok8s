package cli

// lo assets — the eject model's own surface (Go-only; see internal/assets):
//
//	lo assets list                      every embedded asset + its origin
//	lo assets eject [rel…|--all] [--check]   (rel `bash` = the frozen bash implementation)
//	lo assets diff [rel…] [--json] [--check]   (diff <rel> lists the per-file state)
//	lo assets update <rel> [--force]
//
// There is no `show`: `diff <rel>` prints the per-file state of one unit
// (WP9 dropped the duplicate).
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
	"slices"
	"sort"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/assets"
	"github.com/kernpilot/lok8s/internal/bootstrap"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/domain"
	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/tilt"
	"github.com/kernpilot/lok8s/internal/ui"
)

// ErrCheckFailed is the `--check` verdict: something would be ejected, or
// something drifted. main prints nothing for it (the command already did)
// and exits 1.
var ErrCheckFailed = ErrHandled

func newAssetsCommand(paths *config.Paths) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "assets",
		Short:        "Manage the framework assets embedded in the binary",
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
		newAssetsEjectCommand(paths),
		newAssetsDiffCommand(paths),
		newAssetsUpdateCommand(paths),
	)
	return cmd
}

func newAssetsListCommand(paths *config.Paths) *cobra.Command {
	var asJSON bool
	var format func() (string, error)
	cmd := &cobra.Command{
		Use:          "list",
		Short:        "List every embedded asset with its origin",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := format()
			if err != nil {
				return err
			}
			f, err = outputWithJSONFlag(cmd, func() (string, error) { return f, nil }, asJSON, "lo assets list")
			if err != nil {
				return err
			}
			reports, err := assets.Report(paths, nil)
			if err != nil {
				return err
			}
			if reports == nil {
				reports = []assets.UnitReport{}
			}
			switch f {
			case outputJSON:
				return writeAssetsJSON(cmd.OutOrStdout(), reports)
			case outputYAML:
				return writeOutput(cmd.OutOrStdout(), f, assetsJSON{Lo: assets.Version(), Assets: reports})
			}
			assets.WriteTable(cmd.OutOrStdout(), reports, false)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Machine-readable output (the same as -o json)")
	format = addOutputFlag(cmd)
	return cmd
}

func newAssetsEjectCommand(paths *config.Paths) *cobra.Command {
	var all, check bool
	cmd := &cobra.Command{
		Use:   "eject [rel...]",
		Short: "Write embedded assets into the project (.lok8s/<rel>/)",
		Long: `Materialize embedded framework assets into the project so what a cluster
applies is pinned on disk. Without arguments the set is what this project's
cluster specs reference (every builtin spec.bootstrap addon, the driver's
cluster templates, the inventory CRD); --all ejects every data asset.
An existing local copy is never touched. --check writes nothing and exits 1
when any of the set would be ejected — the CI gate for "this repo pins what
it applies".

The rel "bash" is the frozen bash implementation (lo, libs/, utils/, the
drivers' code, the provider plugins). Ejecting it writes the code half into
.lok8s/ with a .lo-origin marker at the tree root and ejects every data asset
the project lacks, so .lok8s/ is a complete tree: the provider plugins then
run from it instead of the copy the binary extracts into its cache, and
lok8s.yaml can route commands to it (spec.implementation; a routing never
runs from the cache). It is never part of --all or of the referenced set.`,
		Args:         cobra.ArbitraryArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return assetsEject(paths, args, all, check, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "Eject every embedded asset, not only the referenced ones")
	cmd.Flags().BoolVar(&check, "check", false, "Write nothing; exit 1 if any asset would be ejected")
	return cmd
}

// assetsEject is `lo assets eject [rel…|--all] [--check]` (also the init
// wizard's eject-bash step, which passes rels = [bash]).
func assetsEject(paths *config.Paths, rels []string, all, check bool, out, stderr io.Writer) error {
	switch {
	case all:
		for _, u := range assets.Units() {
			if u.Kind != assets.KindBash {
				rels = append(rels, u.Rel)
			}
		}
	case len(rels) == 0:
		rels = referencedAssets(paths, stderr)
	}
	if slices.Contains(rels, assets.BashRel) {
		rels = append(rels, assets.MissingDataUnits(paths)...)
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
		ui.ErrorTo(stderr, "assets: %d asset(s) would be ejected (run: lo assets eject)", len(pending))
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
	fmt.Fprintf(out, "assets: ejected %d asset(s) into %s\n", len(pending), config.RelTo(paths.Base, paths.Lok8s))
	return nil
}

func newAssetsDiffCommand(paths *config.Paths) *cobra.Command {
	var asJSON, check bool
	var format func() (string, error)
	cmd := &cobra.Command{
		Use:   "diff [rel...]",
		Short: "Diff an asset three ways: origin, local, embedded",
		Long: `Per file: unchanged · local modified · lo updated · both (conflict) ·
local-only · builtin-only. The headline per addon is the chart version
(local vs embedded). --check exits 1 on any drift.`,
		Args:         cobra.ArbitraryArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			out, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
			f, err := outputWithJSONFlag(cmd, format, asJSON, "lo assets diff")
			if err != nil {
				return err
			}
			reports, err := assets.Report(paths, args)
			if err != nil {
				return assetsErr(stderr, err)
			}
			switch f {
			case outputJSON:
				if err := writeAssetsJSON(out, reports); err != nil {
					return err
				}
			case outputYAML:
				if err := writeOutput(out, f, assetsJSON{Lo: assets.Version(), Assets: reports}); err != nil {
					return err
				}
			default:
				assets.WriteTable(out, reports, len(args) > 0)
			}
			if check && assets.AnyDrift(reports) {
				n := 0
				for _, r := range reports {
					if r.Drifted {
						n++
					}
				}
				ui.ErrorTo(stderr, "assets: %d asset(s) drifted from the binary's copy (lo assets diff <rel> for the files)", n)
				return ErrCheckFailed
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Machine-readable output (the same as -o json)")
	cmd.Flags().BoolVar(&check, "check", false, "Exit 1 on any drift")
	format = addOutputFlag(cmd)
	return cmd
}

func newAssetsUpdateCommand(paths *config.Paths) *cobra.Command {
	return &cobra.Command{
		Use:          "update <rel>",
		Short:        "Apply the embedded copy over an untouched local one",
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
		ui.ErrorTo(stderr, "%v", err)
		return ErrHandled
	}
	return err
}

// assetsJSON is the stable --json shape.
type assetsJSON struct {
	Lo     string              `json:"lo"`
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
// cluster templates, plus the inventory CRD every provision publishes,
// plus the Tilt extension when the project-root Tiltfile loads it.
func referencedAssets(paths *config.Paths, stderr io.Writer) []string {
	set := map[string]bool{"libs/inventory/manifests": true}
	if raw, err := os.ReadFile(filepath.Join(paths.Base, "Tiltfile")); err == nil && tilt.LoadsExtension(raw) {
		set["tilt"] = true
	}
	entries, _ := os.ReadDir(paths.Clusters)
	for _, e := range entries {
		if !e.IsDir() || !domain.NameRe.MatchString(e.Name()) {
			continue
		}
		spec := filepath.Join(paths.Clusters, e.Name(), "cluster.lok8s.yaml")
		if !fsutil.FileExists(spec) {
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
