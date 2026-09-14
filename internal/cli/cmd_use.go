package cli

// lo use — set or show the active domain (clusters/.active).
// Go port of .lok8s/libs/use. Piped output is byte-identical. On a
// terminal (stdin and stdout) a bare `lo use` opens a select over the
// domains instead of the listing, and `lo use <unknown>` adds the closest
// name and the available ones under the [error] line.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/huh/v2"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/domain"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/ui"
)

func init() { registerPorted("use", newUseCommand) }

// useIO is where the select reads and writes. Accessible runs it as a
// numbered line prompt (the tests script it, like the init wizard's).
type useIO struct {
	In         io.Reader
	Out        io.Writer
	Accessible bool
}

// The seams the tests replace: whether a bare `lo use` may open the
// select (stdin AND stdout on a terminal) and the select's IO.
var (
	useInteractive = func() bool { return ui.StdinIsTerminal() && ui.Stdout().TTY }
	useFormIO      = func() useIO { return useIO{In: os.Stdin, Out: os.Stdout} }
)

func newUseCommand(paths *config.Paths, spec commandSpec) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "use [domain]",
		Aliases:      spec.aliases,
		Short:        spec.short,
		GroupID:      spec.group,
		Annotations:  spec.annotations(),
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// The target comes from a positional or an EXPLICIT --domain flag.
			// Ambient resolution (env/.active) must NOT set it: a bare
			// `lo use` under an exported DOMAIN_NAME would silently rewrite
			// .active from the environment.
			target := ""
			if len(args) > 0 {
				target = args[0]
			}
			if target == "" {
				if f := cmd.Flags().Lookup("domain"); f != nil && f.Changed && f.Value.String() != "" {
					target = f.Value.String()
				}
			}
			if target != "" {
				return useSetActive(paths, target, cmd.OutOrStdout(), cmd.ErrOrStderr())
			}
			if useInteractive() {
				return useSelect(paths, useFormIO(), cmd.OutOrStdout(), cmd.ErrOrStderr())
			}
			return useShow(paths, cmd.OutOrStdout())
		},
	}
	return cmd
}

// useSetActive validates and persists the active domain (bash:
// use::_set_active). Rejects names that fail the character allowlist
// (path-traversal guard) or that don't resolve to a real domain directory.
// On a terminal the not-found error is followed by the closest name and
// the available domains. Piped, the [error] line is all there is.
func useSetActive(paths *config.Paths, target string, out, errOut io.Writer) error {
	if !domain.NameRe.MatchString(target) {
		ui.ErrorTo(errOut, "invalid domain name: %s", target)
		return ErrHandled
	}
	base := filepath.Join(paths.Clusters, target)
	if !fsutil.FileExists(filepath.Join(base, "cluster.lok8s.yaml")) && !fsutil.FileExists(filepath.Join(base, "deploy.lok8s.yaml")) {
		ui.ErrorTo(errOut, "domain not found: clusters/%s/ (no cluster.lok8s.yaml or deploy.lok8s.yaml)", target)
		if ui.For(errOut).TTY {
			useSuggest(errOut, target, useDomains(paths))
		}
		return ErrHandled
	}
	if err := os.MkdirAll(paths.Clusters, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(paths.Clusters, ".active"), []byte(target+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "Active domain: %s\n", target)
	return nil
}

// useSuggest is the terminal tail of the not-found error: the closest
// domain by edit distance (cobra's rule) and the available ones.
func useSuggest(w io.Writer, target string, domains []useDomain) {
	names := make([]string, 0, len(domains))
	for _, d := range domains {
		names = append(names, d.name)
	}
	if best, ok := ui.Closest(target, names); ok {
		fmt.Fprintf(w, "Did you mean %s?\n", best)
	}
	if len(names) > 0 {
		fmt.Fprintf(w, "Available domains: %s\n", strings.Join(names, ", "))
	}
}

// useShow prints the active domain plus every domain under clusters/, each
// with what it is (bash: use::_show).
func useShow(paths *config.Paths, out io.Writer) error {
	if active, ok := useActive(paths); ok {
		fmt.Fprintf(out, "Active: %s\n", active)
	} else {
		fmt.Fprintln(out, "No active domain set.")
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "Available domains:")
	for _, d := range useDomains(paths) {
		fmt.Fprintf(out, "  %s (%s)\n", d.name, d.what)
	}
	return nil
}

// useSelect is the terminal form of a bare `lo use`: one select over the
// domains, the active one preselected. Enter sets it through the same
// path as `lo use <domain>`. Esc and Ctrl-C leave without a change (rc 0,
// nothing printed). With no cluster at all there is nothing to choose.
func useSelect(paths *config.Paths, tio useIO, out, errOut io.Writer) error {
	domains := useDomains(paths)
	if len(domains) == 0 {
		fmt.Fprintln(out, "no clusters yet")
		ui.Next(out, "init", "create a project or add a cluster")
		return ErrHandled
	}
	choice := domains[0].name
	if active, ok := useActive(paths); ok {
		for _, d := range domains {
			if d.name == active {
				choice = active
			}
		}
	}
	width := 0
	for _, d := range domains {
		width = max(width, len(d.name))
	}
	opts := make([]huh.Option[string], 0, len(domains))
	for _, d := range domains {
		opts = append(opts, huh.NewOption(fmt.Sprintf("%-*s  %s", width, d.name, d.what), d.name))
	}
	keys := huh.NewDefaultKeyMap()
	keys.Quit = key.NewBinding(key.WithKeys("ctrl+c", "esc"), key.WithHelp("esc", "leave"))
	form := huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().Title("Active domain").Description("Enter sets it. Esc leaves it as it is.").Options(opts...).Value(&choice),
	)).WithTheme(ui.HuhTheme()).WithKeyMap(keys).WithAccessible(tio.Accessible)
	if tio.In != nil {
		form = form.WithInput(tio.In)
	}
	if tio.Out != nil {
		form = form.WithOutput(tio.Out)
	}
	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return nil
		}
		return err
	}
	return useSetActive(paths, choice, out, errOut)
}

// useDomain is one entry of the listing: the name and what it is (the
// driver kind, or `Deploy -> <ref>`).
type useDomain struct {
	name, what string
}

// useDomains lists the domains under clusters/ the way use::_show does:
// the cluster domains in glob order, then the deploy domains.
func useDomains(paths *config.Paths) []useDomain {
	var out []useDomain
	for _, specPath := range sortedGlob(filepath.Join(paths.Clusters, "*", "cluster.lok8s.yaml")) {
		k, err := domain.SpecDriver(specPath, "?")
		if err != nil {
			k = "?"
		}
		out = append(out, useDomain{name: filepath.Base(filepath.Dir(specPath)), what: k})
	}
	for _, specPath := range sortedGlob(filepath.Join(paths.Clusters, "*", "deploy.lok8s.yaml")) {
		out = append(out, useDomain{name: filepath.Base(filepath.Dir(specPath)), what: "Deploy -> " + deployClusterRef(specPath)})
	}
	return out
}

// useActive reads clusters/.active (trailing newlines dropped, like the
// bash $(cat …)). ok is false without the file.
func useActive(paths *config.Paths) (string, bool) {
	raw, err := os.ReadFile(filepath.Join(paths.Clusters, ".active"))
	if err != nil {
		return "", false
	}
	return execx.TrimNewlines(string(raw)), true
}

// deployClusterRef reads .spec.clusterRef.domain from a deploy spec, "?" when
// missing or unreadable (bash: yq -r '.spec.clusterRef.domain // "?"').
func deployClusterRef(specPath string) string {
	var doc struct {
		Spec struct {
			ClusterRef struct {
				Domain string `yaml:"domain"`
			} `yaml:"clusterRef"`
		} `yaml:"spec"`
	}
	raw, err := os.ReadFile(specPath)
	if err != nil || yaml.Unmarshal(raw, &doc) != nil || doc.Spec.ClusterRef.Domain == "" {
		return "?"
	}
	return doc.Spec.ClusterRef.Domain
}

// sortedGlob matches bash's alphabetically-sorted glob expansion.
func sortedGlob(pattern string) []string {
	matches, _ := filepath.Glob(pattern)
	sort.Strings(matches)
	return matches
}
