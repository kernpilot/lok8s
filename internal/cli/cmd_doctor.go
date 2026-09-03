package cli

// lo doctor — environment + toolchain preflight diagnostics.
// Go port of .lok8s/libs/doctor; output is byte-identical. Reports bash
// version (argsh needs >= 4.3), required/optional tool presence, the PATH_*
// environment, whether the secrets plugin is built, and the active domain.
// Exits non-zero when a REQUIRED prerequisite is missing.
//
// Tool checks are PATH-based (`command -v`) on purpose, NOT execx.Look:
// doctor's job is diagnosing the toolchain an operator's shell will actually
// resolve. The PATH it consults is the one this binary prepares for every
// child process (shimEnv: .lok8s + .bin prepended, KUSTOMIZE_PLUGIN_HOME
// defaulted) — the same environment the argsh implementation sees when run
// through `lo`, so both implementations diagnose the same world.

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/assets"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/domain"
	"github.com/kernpilot/lok8s/internal/ui"
)

func init() { registerPorted("doctor", newDoctorCommand) }

// doctorTools is the tool table, verbatim from the bash doctor: name,
// required, purpose. envsubst is handled inline (its flavor line interleaves).
var doctorToolsPre = []struct {
	name     string
	required bool
	purpose  string
}{
	{"argsh", true, "argsh runtime (b install / argsh)"},
	{"yq", true, "YAML processor"},
	{"kustomize", true, "manifest builder"},
	{"kubectl", true, "Kubernetes CLI"},
}

var doctorToolsPost = []struct {
	name     string
	required bool
	purpose  string
}{
	{"docker", true, "container runtime"},
	// jq is REQUIRED: template::envsubst's native path (any non-GNU envsubst,
	// e.g. b's renvsubst alias) substitutes via jq.
	{"jq", true, "JSON processor (restricted substitution)"},
	{"kind", false, "local clusters (Lo driver)"},
	{"tilt", false, "dev loop (Lo driver)"},
	{"mkcert", false, "local TLS (Lo driver)"},
	{"sops", false, "secret encryption"},
	{"kubeone", false, "KubeOne driver"},
	{"clusterctl", false, "Capi driver"},
}

func newDoctorCommand(paths *config.Paths, spec commandSpec) *cobra.Command {
	return &cobra.Command{
		Use:          "doctor",
		Aliases:      spec.aliases,
		Short:        spec.short,
		GroupID:      spec.group,
		Annotations:  spec.annotations(),
		Args:         cobra.ArbitraryArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			stderr := cmd.ErrOrStderr()
			if len(args) > 0 {
				return argshErrorf(stderr, "too many arguments: %s", args[0])
			}
			if v, _ := cmd.Flags().GetCount("verbose"); v > 0 {
				os.Setenv("DEBUG", "1")
			}
			domainFlag, _ := cmd.Flags().GetString("domain")
			d := domain.Resolve(domainFlag, paths.Clusters, stderr)
			return runDoctor(paths, d, cmd.OutOrStdout(), stderr)
		},
	}
}

func runDoctor(paths *config.Paths, d string, out, stderr io.Writer) error {
	path := doctorPATH(paths)
	fail := false

	fmt.Fprintln(out, "=== lok8s doctor ===")
	fmt.Fprintln(out)

	fmt.Fprintln(out, "--- runtime ---")
	// The bash doctor reports ITS interpreter's BASH_VERSINFO. The Go binary
	// has none, but the argsh side of the toolchain still runs bash — report
	// the bash the prepared PATH resolves (the one the shim executes).
	if major, minor, ok := doctorBashVersion(path); ok {
		bv := fmt.Sprintf("%d.%d", major, minor)
		if major > 4 || (major == 4 && minor >= 3) {
			doctorOK(out, "bash "+bv)
		} else {
			doctorBad(out, "bash "+bv+" — argsh needs >= 4.3 (macOS ships 3.2: brew install bash)")
			fail = true
		}
	} else {
		// Unreachable through the bash implementation (it IS bash); the Go
		// binary can still diagnose the absence.
		doctorBad(out, "bash MISSING (required) — argsh needs >= 4.3 (macOS ships 3.2: brew install bash)")
		fail = true
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "--- tools ---")
	for _, t := range doctorToolsPre {
		if !doctorTool(out, path, t.name, t.required, t.purpose) {
			fail = true
		}
	}
	if doctorTool(out, path, "envsubst", true, "variable substitution") {
		// Restricted (whitelist) substitution uses GNU envsubst when that is
		// what is on PATH and a native jq pass otherwise (template::envsubst)
		// — report the flavor so a substitution surprise is diagnosable at a
		// glance.
		doctorOK(out, "envsubst flavor: "+doctorEnvsubstFlavor(path))
	} else {
		fail = true
	}
	for _, t := range doctorToolsPost {
		if !doctorTool(out, path, t.name, t.required, t.purpose) {
			fail = true
		}
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "--- environment ---")
	doctorDir(out, "PATH_BASE", paths.Base)
	doctorDir(out, "PATH_LOK8S", paths.Lok8s)
	doctorDir(out, "PATH_CLUSTERS", paths.Clusters)
	// The bash entrypoint defaults PATH_SECRETS to ${PATH_BASE}/.secrets
	// before doctor reads it — report the same effective value.
	secretsVal := paths.SecretsEnv
	if secretsVal == "" {
		secretsVal = filepath.Join(paths.Base, ".secrets")
	}
	doctorDir(out, "PATH_SECRETS", secretsVal)
	// KUSTOMIZE_PLUGIN_HOME: this binary defaults it for every child (shimEnv)
	// — doctor reports the environment as prepared, not the raw shell's.
	pluginHome := os.Getenv("KUSTOMIZE_PLUGIN_HOME")
	if pluginHome == "" {
		pluginHome = filepath.Join(paths.Base, ".kustomize")
	}
	doctorOK(out, "KUSTOMIZE_PLUGIN_HOME="+pluginHome)
	if isExecutableFile(filepath.Join(pluginHome, "secrets.lok8s.dev/v1/secret/Secret")) {
		doctorOK(out, "secrets.lok8s.dev plugin built")
	} else {
		doctorWarn(out, "secrets.lok8s.dev plugin not built (run: lo kustomize build)")
	}
	doctorAssets(out, paths)

	fmt.Fprintln(out)
	fmt.Fprintln(out, "--- dev TLS (cert: CA) ---")
	if mkcert, ok := lookPathIn(path, "mkcert"); ok {
		caroot := doctorCommandOutput(mkcert, "-CAROOT")
		if caroot != "" && fileExists(filepath.Join(caroot, "rootCA.pem")) {
			doctorOK(out, "local CA present ("+caroot+")")
			dd := d
			if dd == "" {
				dd = "<domain>"
			}
			fmt.Fprintf(out, "    if *.%s TLS is rejected by your browser/curl, run: lo trust\n", dd)
		} else {
			doctorWarn(out, "no local CA yet — created on first cert: build, then trust it: lo trust")
		}
	} else {
		doctorWarn(out, "mkcert absent — only needed to TRUST the dev CA (lo trust), never to build")
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "--- domain ---")
	if d != "" {
		spec := filepath.Join(paths.Clusters, d, "cluster.lok8s.yaml")
		deploySpec := filepath.Join(paths.Clusters, d, "deploy.lok8s.yaml")
		switch {
		case fileExists(spec):
			kind, err := domain.SpecDriver(spec, "?")
			if err != nil {
				kind = "?"
			}
			doctorOK(out, "active: "+d+" (kind "+kind+")")
		case fileExists(deploySpec):
			doctorOK(out, "active: "+d+" (Deploy -> "+deployClusterRef(deploySpec)+")")
		default:
			doctorWarn(out, "active domain '"+d+"' has no cluster.lok8s.yaml / deploy.lok8s.yaml")
		}
	} else {
		doctorWarn(out, "no active domain (run: lo use <domain>)")
	}

	// Provider / infrastructure diagnosis — advisory, never affects the exit
	// code.
	doctorProviderSection(paths, d, path, out, stderr)

	fmt.Fprintln(out)
	if fail {
		ui.Errorf(stderr, "doctor: missing required prerequisites (see ✗ above)")
		return ErrHandled
	}
	fmt.Fprintln(out, "doctor: all required checks passed.")
	return nil
}

// doctorAssets is the eject-model summary line (Go-only): drift count,
// "all in sync", or "none ejected". It is OMITTED for the one layout the
// frozen implementation can also run in — a complete vendored .lok8s tree
// with no ejected units and no drift (what `b env sync` produces) — so the
// doctor output stays byte-identical to bash there (parity-configure diffs
// it strictly). Any ejected unit, any drift, or a project without a local
// tree prints the line.
func doctorAssets(w io.Writer, paths *config.Paths) {
	reports, err := assets.Report(paths, nil)
	if err != nil {
		doctorWarn(w, "assets: "+err.Error())
		return
	}
	ejected := false
	for _, r := range reports {
		if r.Marker != nil {
			ejected = true
		}
	}
	line, warn := assets.DoctorLine(paths)
	if !warn && !ejected && strings.HasSuffix(line, "all in sync with the binary") {
		return
	}
	if warn {
		doctorWarn(w, line)
		return
	}
	doctorOK(w, line)
}

func doctorOK(w io.Writer, msg string)   { fmt.Fprintf(w, "  \033[32m✓\033[0m %s\n", msg) }
func doctorWarn(w io.Writer, msg string) { fmt.Fprintf(w, "  \033[33m!\033[0m %s\n", msg) }
func doctorBad(w io.Writer, msg string)  { fmt.Fprintf(w, "  \033[31m✗\033[0m %s\n", msg) }

// doctorTool checks one tool on PATH (bash: doctor::_tool via `command -v`).
// Returns false when required and missing.
func doctorTool(w io.Writer, path, name string, required bool, purpose string) bool {
	if _, ok := lookPathIn(path, name); ok {
		doctorOK(w, name+" — "+purpose)
		return true
	}
	if required {
		doctorBad(w, name+" MISSING (required) — "+purpose)
		return false
	}
	doctorWarn(w, name+" not found (optional) — "+purpose)
	return true
}

func doctorDir(w io.Writer, name, val string) {
	if val != "" && dirExists(val) {
		doctorOK(w, name+"="+val)
	} else {
		if val == "" {
			val = "unset"
		}
		doctorWarn(w, name+" unset or not a directory ("+val+")")
	}
}

// doctorPATH is the PATH the binary prepares for children (shimEnv): .lok8s +
// .bin prepended when missing.
func doctorPATH(p *config.Paths) string {
	path := os.Getenv("PATH")
	for _, dir := range []string{p.Lok8s, p.Bin} {
		if !containsPathEntry(path, dir) {
			path = dir + string(os.PathListSeparator) + path
		}
	}
	return path
}

// lookPathIn resolves a tool on an explicit PATH value (`command -v`
// semantics: first executable file wins).
func lookPathIn(path, tool string) (string, bool) {
	for _, dir := range strings.Split(path, string(os.PathListSeparator)) {
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, tool)
		if isExecutableFile(candidate) {
			return candidate, true
		}
	}
	return "", false
}

func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}

var bashVersionRe = regexp.MustCompile(`^([0-9]+)\.([0-9]+)`)

// doctorBashVersion reports the major.minor of the bash the prepared PATH
// resolves (bash: BASH_VERSINFO of the running interpreter — same binary).
func doctorBashVersion(path string) (int, int, bool) {
	bash, ok := lookPathIn(path, "bash")
	if !ok {
		return 0, 0, false
	}
	out, err := exec.Command(bash, "-c", `echo "${BASH_VERSINFO[0]}.${BASH_VERSINFO[1]}"`).Output()
	if err != nil {
		return 0, 0, false
	}
	m := bashVersionRe.FindStringSubmatch(strings.TrimSpace(string(out)))
	if m == nil {
		return 0, 0, false
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	return major, minor, true
}

// doctorEnvsubstFlavor mirrors template::envsubst_flavor: "gnu" when
// `envsubst --version` mentions GNU gettext, else "other".
func doctorEnvsubstFlavor(path string) string {
	envsubst, ok := lookPathIn(path, "envsubst")
	if !ok {
		return "other"
	}
	out, _ := exec.Command(envsubst, "--version").Output()
	if strings.Contains(string(out), "GNU gettext") {
		return "gnu"
	}
	return "other"
}

// doctorCommandOutput runs a command discarding stderr and trims trailing
// newlines like a bash command substitution.
func doctorCommandOutput(cmd string, args ...string) string {
	out, err := exec.Command(cmd, args...).Output()
	if err != nil {
		return ""
	}
	return trimTrailingNewline(string(out))
}

var providerNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// doctorProviderSection renders the provider's infrastructure diagnosis
// (bash: doctor::_provider_section). Purely advisory: it never changes the
// doctor exit code and never mutates anything. Silent (prints nothing) when
// there is no provider-backed cluster spec.
//
// A provider is a BASH plugin (.lok8s/providers/<name>/main sourcing the
// argsh runtime), so the section itself is delegated to a bash child running
// the ORIGINAL doctor::_provider_section — the one extension point of these
// three commands that stays on the bash side by design. The Go side only
// mirrors the bash short-circuits (spec present, spec.provider.name valid) so
// the common no-provider case never spawns a shell.
func doctorProviderSection(paths *config.Paths, d, path string, out, stderr io.Writer) {
	if d == "" {
		return
	}
	spec := filepath.Join(paths.Clusters, d, "cluster.lok8s.yaml")
	if !fileExists(spec) {
		return
	}
	// provider::read_name, errors suppressed (bash: 2>/dev/null || return 0):
	// absent or invalid name → silent skip.
	pname := specProviderName(spec)
	if pname == "" || !providerNameRe.MatchString(pname) {
		return
	}

	bash, ok := lookPathIn(path, "bash")
	if !ok {
		return // no bash → the bash-side diagnosis cannot run at all
	}
	script := `set -euo pipefail
source "${PATH_BIN}/argsh"
import ^libs/doctor
doctor::_provider_section "${1}"`
	cmd := exec.Command(bash, "-c", script, "lo-doctor-provider", d)
	cmd.Dir = paths.Base
	secretsVal := paths.SecretsEnv
	if secretsVal == "" {
		secretsVal = filepath.Join(paths.Base, ".secrets")
	}
	cmd.Env = append(os.Environ(),
		"PATH="+path,
		"PATH_BASE="+paths.Base,
		"PATH_BIN="+paths.Bin,
		"PATH_LOK8S="+paths.Lok8s,
		"PATH_CLUSTERS="+paths.Clusters,
		"PATH_SECRETS="+secretsVal,
		"PATH_SCRIPTS="+paths.Lok8s,
	)
	cmd.Stdout = out
	cmd.Stderr = stderr
	_ = cmd.Run() // advisory: the bash section always returns 0 itself
}

// specProviderName reads .spec.provider.name from a cluster spec, "" when
// missing or unreadable.
func specProviderName(specPath string) string {
	var doc struct {
		Spec struct {
			Provider struct {
				Name string `yaml:"name"`
			} `yaml:"provider"`
		} `yaml:"spec"`
	}
	raw, err := os.ReadFile(specPath)
	if err != nil || yaml.Unmarshal(raw, &doc) != nil {
		return ""
	}
	return doc.Spec.Provider.Name
}
