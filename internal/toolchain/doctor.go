package toolchain

// doctor.go — verify what `lo init toolchain` + `b install` landed: b
// itself, and the three render tools at the paths the exec render
// resolves, each at the pinned version. Read-only; the fix is always the
// same command.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/fsutil"
)

// Status of one check.
type Status int

const (
	// OK — present at the pinned version.
	OK Status = iota
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
const Fix = "fix: lo init toolchain"

// Doctor runs the checks. ctx bounds the probes (each gets ten seconds
// under it).
func Doctor(ctx context.Context, o DoctorOptions) []Check {
	d := &doctor{o: o, probe: o.Probe}
	if d.probe == nil {
		r := o.Runner
		if r == nil {
			r = execx.NewRunner(nil)
		}
		d.probe = func(path string, args ...string) (string, error) {
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			return probe(ctx, r, path, args...)
		}
	}

	// b itself.
	bPath := filepath.Join(o.Bin, "b")
	if fsutil.IsExecutable(bPath) {
		v, err := d.probe(bPath, "--version")
		if err != nil {
			d.add(Warn, "b at %s (version unknown: %v)", config.RelTo(o.Base, bPath), err)
		} else {
			d.add(OK, "b %s (%s)", firstField(v), config.RelTo(o.Base, bPath))
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

	// khelm ChartRenderer: `<plugin> version` prints "2.8.0 (helm 3.21.2)".
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

// probe runs path args… and returns its trimmed stdout.
func probe(ctx context.Context, r execx.Runner, path string, args ...string) (string, error) {
	out, err := execx.Output(ctx, r, execx.Cmd{Name: path, Args: args})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
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
