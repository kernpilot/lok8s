package toolchain

// doctor.go — verify what `lo toolchain install` + `b install` landed: b
// itself (not older than BRelease), and the three render tools at the
// paths the exec render resolves, each at the pinned version. Read-only;
// the fix is the same command for each render tool (Fix), and b updates
// itself (BUpdateFix).

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/fsutil"
)

// Status of one check.
type Status int

const (
	// The zero value is not a result: a Check built without a Status
	// reads as Bad in the report, never as OK.
	_ Status = iota
	// OK — present at the pinned version.
	OK
	// Warn — present but not at the pin, or absent where this build does
	// not strictly need it.
	Warn
	// Bad — absent where this build needs it (doctor exits non-zero).
	Bad
)

// Check is one doctor line.
type Check struct {
	Status Status
	Msg    string
}

// DoctorOptions locates the toolchain to verify.
type DoctorOptions struct {
	// Base is the project root (paths are printed relative to it).
	Base string
	// Bin is the toolchain dir (<Base>/.bin).
	Bin string
	// PluginHome is KUSTOMIZE_PLUGIN_HOME as the render resolves it
	// (<Base>/.kustomize by default).
	PluginHome string
	// PATH is the lookup path for kustomize when it is not under Bin (the
	// PATH lo prepares for children). "" = the process PATH.
	PATH string
	// LoVersion is the running lo's version — the Secret plugin pin.
	LoVersion string
	// Full is true for lo-full: kustomize/khelm/Secret are then optional
	// (LO_RENDER=exec only), so their absence is a warning, not a failure.
	Full bool
	// Probe runs a tool and returns its stdout (the hermetic seam). Nil =
	// Runner with a short timeout under the caller's context.
	Probe func(path string, args ...string) (string, error)
	// Runner runs the probes when Probe is nil. Nil = execx.NewRunner(nil)
	// (the tools are probed at the resolved paths, never looked up).
	Runner execx.Runner
}

// Fix is the remedy every failed check names.
const Fix = "fix: lo toolchain install"

// BUpdateFix is the remedy for a b older than BRelease. lo toolchain
// install keeps an existing .bin/b, so only b itself updates it.
const BUpdateFix = "fix: .bin/b update b"

// The time limit of one version probe of Doctor, and the grace of every
// probe after its interrupt. At the limit (or on a cancel of the caller)
// the tool gets an interrupt, the runner's cancel. A tool that still runs
// at the end of the grace is killed, and its output pipe closes
// (exec.Cmd.WaitDelay). Without the grace, a tool that ignores the
// interrupt (a broken or foreign .bin/b) holds the caller without bound.
// A version probe has no cleanup to finish, so the grace is short.
// Variables, so a test can set them.
var (
	probeTimeout = 10 * time.Second
	probeGrace   = 2 * time.Second
)

// Doctor runs the checks. ctx bounds the probes: each gets probeTimeout
// under it, then probeGrace to end after its interrupt.
func Doctor(ctx context.Context, o DoctorOptions) []Check {
	d := &doctor{o: o, probe: o.Probe}
	if d.probe == nil {
		r := o.Runner
		if r == nil {
			r = execx.NewRunner(nil)
		}
		d.probe = func(path string, args ...string) (string, error) {
			ctx, cancel := context.WithTimeout(ctx, probeTimeout)
			defer cancel()
			return probe(ctx, r, path, args...)
		}
	}

	// b itself, and not older than the b this lo bootstraps (BRelease).
	// A newer b passes: b updates itself to its latest release. A version
	// that does not parse is a warning, as a failed probe is. A b release
	// prints its version bare (1.2.3), a build from source with a v
	// (v1.2.3); the older line prints both versions bare.
	bPath := filepath.Join(o.Bin, "b")
	if fsutil.IsExecutable(bPath) {
		rel := config.RelTo(o.Base, bPath)
		v, err := d.probe(bPath, "--version")
		got := firstField(v)
		older := false
		if err == nil {
			older, err = olderRelease(got, BRelease.Version)
		}
		switch {
		case err != nil:
			d.add(Warn, "b at %s (version unknown: %v)", rel, err)
		case older:
			d.add(Warn, "b %s at %s is older than %s, the b this lo ships — %s",
				strings.TrimPrefix(got, "v"), rel, BRelease.Version, BUpdateFix)
		default:
			d.add(OK, "b %s (%s)", got, rel)
		}
	} else {
		d.add(Bad, "b missing at %s — %s", config.RelTo(o.Base, bPath), Fix)
	}

	// kustomize: .bin first, then PATH — the exec render's own lookup.
	kPath := filepath.Join(o.Bin, "kustomize")
	if !fsutil.IsExecutable(kPath) {
		if p, ok := LookPath(o.path(), "kustomize"); ok {
			kPath = p
		} else {
			kPath = ""
		}
	}
	d.checkTool(tool{
		what: "kustomize", path: kPath, missingAt: filepath.Join(o.Bin, "kustomize"),
		note: " (lo core execs it for every render)", versionArg: "version",
		want: KustomizeCLI, version: firstField,
	})

	// khelm ChartRenderer: `<plugin> version` prints "2.9.0 (helm 3.22.1)".
	crPath := filepath.Join(o.PluginHome, filepath.FromSlash(ChartRendererPluginRel))
	d.checkTool(tool{
		what: "khelm ChartRenderer", path: crPath, missingAt: crPath,
		note: " (the addons' Helm charts inflate through it)", versionArg: "version",
		want: KhelmVersion, version: func(v string) string { return strings.TrimPrefix(firstField(v), "v") },
	})

	// The Secret generator: `<plugin> --version` prints the stamped
	// version (lok8s ≥ the release that added the flag; older plugin
	// builds treat the flag as a config path and fail).
	sPath := filepath.Join(o.PluginHome, filepath.FromSlash(SecretPluginRel))
	want := vPrefixed(o.LoVersion)
	d.checkTool(tool{
		what: "secrets.lok8s.dev Secret", path: sPath, missingAt: sPath,
		note: " (the Secret generator every render runs)", versionArg: "--version",
		want: want, version: func(v string) string { return vPrefixed(firstField(v)) },
		unknown: fmt.Sprintf("secrets.lok8s.dev Secret at %s: version unknown (built before --version; expected %s) — %s", config.RelTo(o.Base, sPath), want, Fix),
	})
	return d.checks
}

// doctor collects the checks of one run.
type doctor struct {
	o      DoctorOptions
	probe  func(path string, args ...string) (string, error)
	checks []Check
}

func (d *doctor) add(s Status, format string, a ...any) {
	d.checks = append(d.checks, Check{Status: s, Msg: fmt.Sprintf(format, a...)})
}

// tool is one render tool to verify against its pin.
type tool struct {
	what string
	// path is where the tool resolved ("" = not found); missingAt names
	// the path the "missing" line reports.
	path, missingAt string
	// note is the core-only reason it is needed.
	note string
	// versionArg prints the version; version extracts the comparable
	// version from that output.
	versionArg string
	version    func(out string) string
	want       string
	// unknown is the line for a failed version probe ("" = the generic
	// "version unknown" line).
	unknown string
}

// checkTool is the one check every render tool gets: absent → the missing
// line (fatal on core, which execs them; advisory on lo-full, where the
// binaries only serve LO_RENDER=exec); a failed probe → version unknown;
// else the version against its pin.
func (d *doctor) checkTool(t tool) {
	rel := config.RelTo(d.o.Base, t.path)
	if t.path == "" || !fsutil.IsExecutable(t.path) {
		if d.o.Full {
			d.add(Warn, "%s missing at %s (optional on lo-full: in-process render; LO_RENDER=exec needs it) — %s", t.what, config.RelTo(d.o.Base, t.missingAt), Fix)
			return
		}
		d.add(Bad, "%s missing at %s%s — %s", t.what, config.RelTo(d.o.Base, t.missingAt), t.note, Fix)
		return
	}
	v, err := d.probe(t.path, t.versionArg)
	if err != nil {
		if t.unknown != "" {
			d.add(Warn, "%s", t.unknown)
			return
		}
		d.add(Warn, "%s at %s (version unknown: %v)", t.what, rel, err)
		return
	}
	got := t.version(v)
	if got == t.want {
		d.add(OK, "%s %s (%s)", t.what, got, rel)
		return
	}
	d.add(Warn, "%s %s at %s — expected %s (%s, then .bin/b install)", t.what, got, rel, t.want, Fix)
}

func (o *DoctorOptions) path() string {
	if o.PATH != "" {
		return o.PATH
	}
	return os.Getenv("PATH")
}

// probe runs path args… and returns its trimmed stdout. A tool that does
// not end within probeGrace after a cancel of ctx is killed.
func probe(ctx context.Context, r execx.Runner, path string, args ...string) (string, error) {
	out, err := execx.Output(ctx, r, execx.Cmd{Name: path, Args: args, WaitDelay: probeGrace})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// olderRelease reports whether the release got (as `b --version` prints
// it: 4.18.7, or v4.18.7) is below want. Only major.minor.patch counts: a
// pre-release of the same release is not older. A version that does not
// parse is an error, because it cannot be compared.
func olderRelease(got, want string) (bool, error) {
	g, err := semver.NewVersion(got)
	if err != nil {
		return false, fmt.Errorf("--version printed %q, not a version", got)
	}
	w, err := semver.NewVersion(want)
	if err != nil {
		return false, fmt.Errorf("the pin %q is not a version", want)
	}
	gr := [3]uint64{g.Major(), g.Minor(), g.Patch()}
	wr := [3]uint64{w.Major(), w.Minor(), w.Patch()}
	return slices.Compare(gr[:], wr[:]) < 0, nil
}

func firstField(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// LookPath resolves a tool on an explicit PATH value (`command -v`
// semantics: the first executable file wins; an empty entry is "."). The
// doctor commands share it: they diagnose the PATH an operator's shell
// resolves, not execx.Look's .bin-first lookup.
func LookPath(path, tool string) (string, bool) {
	for dir := range strings.SplitSeq(path, string(os.PathListSeparator)) {
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, tool)
		if fsutil.IsExecutable(candidate) {
			return candidate, true
		}
	}
	return "", false
}
