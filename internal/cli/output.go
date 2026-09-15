package cli

// output.go: `--output text|json|yaml` (`-o`), Go-only, on the commands
// that report state: use (the listing), status, addons, assets list,
// registry status, version, doctor. text is the default and byte-identical
// to before; json and yaml render one document with stable lowerCamel
// field names (documented in docs/reference/cli.md). The structured
// paths gather the same facts the text paths print. None of them ejects
// or writes.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/driver"
	lodriver "github.com/kernpilot/lok8s/internal/driver/lo"
)

// Output formats.
const (
	outputText = "text"
	outputJSON = "json"
	outputYAML = "yaml"
)

// addOutputFlag registers -o/--output on cmd and returns its reader.
func addOutputFlag(cmd *cobra.Command) func() (string, error) {
	var format string
	cmd.Flags().StringVarP(&format, "output", "o", outputText, "Output format: text, json or yaml")
	_ = cmd.RegisterFlagCompletionFunc("output", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{outputText, outputJSON, outputYAML}, cobra.ShellCompDirectiveNoFileComp
	})
	return func() (string, error) {
		switch format {
		case outputText, outputJSON, outputYAML:
			return format, nil
		}
		return "", argshErrorf(cmd.ErrOrStderr(), "invalid --output %q: text, json or yaml", format)
	}
}

// outputWithJSONFlag resolves -o next to an older --json flag: --json is
// -o json, and --json with another -o value is an error naming the
// command. The flag pair stays one source of truth.
func outputWithJSONFlag(cmd *cobra.Command, format func() (string, error), asJSON bool, command string) (string, error) {
	f, err := format()
	if err != nil {
		return "", err
	}
	if !asJSON {
		return f, nil
	}
	if f != outputText && f != outputJSON {
		return "", argshErrorf(cmd.ErrOrStderr(), "%s: --json conflicts with --output %s", command, f)
	}
	return outputJSON, nil
}

// writeOutput renders v as json or yaml on w.
func writeOutput(w io.Writer, format string, v any) error {
	switch format {
	case outputJSON:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false) // a < in a value stays a <, like the other JSON writers
		return enc.Encode(v)
	case outputYAML:
		// Through JSON, so the yaml document carries the json field names
		// and order (one contract for both formats).
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		return writeJSONAsYAML(w, raw)
	}
	return fmt.Errorf("writeOutput: no renderer for %q", format)
}

// writeJSONAsYAML re-encodes a JSON document as block-style yaml with the
// same keys in the same order.
func writeJSONAsYAML(w io.Writer, raw []byte) error {
	var node yaml.Node
	if err := yaml.Unmarshal(raw, &node); err != nil {
		return err
	}
	blockStyle(&node)
	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	if err := enc.Encode(&node); err != nil {
		return err
	}
	return enc.Close()
}

// blockStyle drops the flow and quote styles the JSON text carries into
// the node tree, so the yaml prints in block style with plain scalars.
func blockStyle(n *yaml.Node) {
	n.Style = 0
	for _, c := range n.Content {
		blockStyle(c)
	}
}

// installOutputs adds -o to the commands whose file is owned elsewhere
// (registry status, cmd_registry.go): the structured path runs the driver's
// own status report into a buffer and reads its fixed line format, so the
// driver stays the one source.
func installOutputs(root *cobra.Command, paths *config.Paths) {
	cmd := findByPath(root, "registry status")
	if cmd == nil || cmd.RunE == nil {
		return
	}
	format := addOutputFlag(cmd)
	run := cmd.RunE
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		f, err := format()
		if err != nil {
			return err
		}
		if f == outputText {
			return run(cmd, args)
		}
		d := ambientMainEnv(cmd, paths)
		stderr := cmd.ErrOrStderr()
		if err := registryGate(paths, d, stderr); err != nil {
			return ErrHandled
		}
		shared, _ := cmd.Flags().GetBool("shared")
		var buf bytes.Buffer
		drv := lodriver.New(&driver.Deps{Paths: paths, Runner: newRunner(paths), Stderr: stderr})
		drv.SetOutput(&buf)
		if err := drv.RegistryStatus(cmd.Context(), d, shared, false, &buf, stderr); err != nil {
			return ErrHandled
		}
		return writeOutput(cmd.OutOrStdout(), f, registryStatusReport{Domain: d, Registries: parseRegistryStatus(buf.String())})
	}
}

// registryStatusReport is `lo registry status -o json|yaml`.
type registryStatusReport struct {
	Domain     string                `json:"domain" yaml:"domain"`
	Registries []registryStatusEntry `json:"registries" yaml:"registries"`
}

type registryStatusEntry struct {
	Name      string `json:"name" yaml:"name"`
	Scope     string `json:"scope" yaml:"scope"` // project | shared
	Container string `json:"container" yaml:"container"`
	Endpoint  string `json:"endpoint" yaml:"endpoint"`
	Running   bool   `json:"running" yaml:"running"`
	Reachable bool   `json:"reachable" yaml:"reachable"`
	State     string `json:"state" yaml:"state"`
}

// parseRegistryStatus reads the driver's status lines
// (`<marker> <name> <[scope]> <container> → <endpoint> · <state>`, no
// colour). Lines in another shape are skipped.
func parseRegistryStatus(text string) []registryStatusEntry {
	out := []registryStatusEntry{}
	for line := range strings.SplitSeq(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 || fields[4] != "→" {
			continue
		}
		rest := strings.Join(fields[5:], " ")
		endpoint, state, ok := strings.Cut(rest, " · ")
		if !ok {
			continue
		}
		e := registryStatusEntry{
			Name:      fields[1],
			Scope:     strings.Trim(fields[2], "[]"),
			Container: fields[3],
			Endpoint:  endpoint,
			State:     state,
		}
		switch fields[0] {
		case "✓":
			e.Running, e.Reachable = true, true
		case "⚠":
			e.Running = true
		}
		out = append(out, e)
	}
	return out
}
