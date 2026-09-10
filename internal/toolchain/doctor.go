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
	// Runner with a short timeout.
	Probe func(path string, args ...string) (string, error)
	// Runner runs the probes when Probe is nil. Nil = execx.NewRunner(nil)
	// (the tools are probed at the resolved paths, never looked up).
	Runner execx.Runner
}

// Fix is the remedy every failed check names.
const Fix = "fix: lo init toolchain"

// Doctor runs the checks.
func Doctor(o DoctorOptions) []Check {
	prb := o.Probe
	if prb == nil {
		r := o.Runner
		if r == nil {
			r = execx.NewRunner(nil)
		}
		prb = func(path string, args ...string) (string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			return probe(ctx, r, path, args...)
		}
	}
	var checks []Check
	add := func(s Status, format string, a ...any) {
		checks = append(checks, Check{Status: s, Msg: fmt.Sprintf(format, a...)})
	}
	// Absence of a render tool: fatal on core (it execs them), advisory on
	// lo-full (in-process; the binaries only serve LO_RENDER=exec).
	missing := func(what, path, note string) {
		if o.Full {
			add(Warn, "%s missing at %s (optional on lo-full: in-process render; LO_RENDER=exec needs it) — %s", what, config.RelTo(o.Base, path), Fix)
			return
		}
		add(Bad, "%s missing at %s%s — %s", what, config.RelTo(o.Base, path), note, Fix)
	}
	versioned := func(what, path, got, want string) {
		if got == want {
			add(OK, "%s %s (%s)", what, got, config.RelTo(o.Base, path))
			return
		}
		add(Warn, "%s %s at %s — expected %s (%s, then .bin/b install)", what, got, config.RelTo(o.Base, path), want, Fix)
	}

	// b itself.
	bPath := filepath.Join(o.Bin, "b")
	if isExecutable(bPath) {
		v, err := prb(bPath, "--version")
		if err != nil {
			add(Warn, "b at %s (version unknown: %v)", config.RelTo(o.Base, bPath), err)
		} else {
			add(OK, "b %s (%s)", firstField(v), config.RelTo(o.Base, bPath))
		}
	} else {
		add(Bad, "b missing at %s — %s", config.RelTo(o.Base, bPath), Fix)
	}

	// kustomize: .bin first, then PATH — the exec render's own lookup.
	kPath := filepath.Join(o.Bin, "kustomize")
	if !isExecutable(kPath) {
		if p, ok := LookPath(o.path(), "kustomize"); ok {
			kPath = p
		} else {
			kPath = ""
		}
	}
	if kPath == "" {
		missing("kustomize", filepath.Join(o.Bin, "kustomize"), " (lo core execs it for every render)")
	} else if v, err := prb(kPath, "version"); err != nil {
		add(Warn, "kustomize at %s (version unknown: %v)", config.RelTo(o.Base, kPath), err)
	} else {
		versioned("kustomize", kPath, firstField(v), KustomizeCLI)
	}

	// khelm ChartRenderer: `<plugin> version` prints "2.8.0 (helm 3.21.2)".
	crPath := filepath.Join(o.PluginHome, filepath.FromSlash(ChartRendererPluginRel))
	if !isExecutable(crPath) {
		missing("khelm ChartRenderer", crPath, " (the addons' Helm charts inflate through it)")
	} else if v, err := prb(crPath, "version"); err != nil {
		add(Warn, "khelm ChartRenderer at %s (version unknown: %v)", config.RelTo(o.Base, crPath), err)
	} else {
		versioned("khelm ChartRenderer", crPath, strings.TrimPrefix(firstField(v), "v"), KhelmVersion)
	}

	// The Secret generator: `<plugin> --version` prints the stamped
	// version (lok8s ≥ the release that added the flag; older plugin
	// builds treat the flag as a config path and fail).
	sPath := filepath.Join(o.PluginHome, filepath.FromSlash(SecretPluginRel))
	want := vPrefixed(o.LoVersion)
	if !isExecutable(sPath) {
		missing("secrets.lok8s.dev Secret", sPath, " (the Secret generator every render runs)")
	} else if v, err := prb(sPath, "--version"); err != nil {
		add(Warn, "secrets.lok8s.dev Secret at %s: version unknown (built before --version; expected %s) — %s", config.RelTo(o.Base, sPath), want, Fix)
	} else {
		versioned("secrets.lok8s.dev Secret", sPath, vPrefixed(firstField(v)), want)
	}
	return checks
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
	for _, dir := range strings.Split(path, string(os.PathListSeparator)) {
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, tool)
		if isExecutable(candidate) {
			return candidate, true
		}
	}
	return "", false
}
