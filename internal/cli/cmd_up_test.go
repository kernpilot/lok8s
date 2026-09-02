package cli

// cmd_up_test.go — the run header (_lo_header) byte-exact, and main::up's
// routing after the header: dispatch failure/decline, `--ci` rc
// passthrough, interactive `tilt up` + `--open-tilt`. The dispatch is a
// recorder and tilt runs over a fake runner + fake detached spawn — no
// Tilt, kind or docker is ever reached.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/tilt"
)

func headerOf(t *testing.T, p *config.Paths, d string) string {
	t.Helper()
	var out bytes.Buffer
	writeRunHeader(&out, p, d, filepath.Join(p.Base, ".kubeconfig", "clu.yaml"))
	return out.String()
}

func TestRunHeaderShapes(t *testing.T) {
	p := synthProject(t)
	writeFile(t, filepath.Join(p.Clusters, "lo.dev", "cluster.lok8s.yaml"),
		"kind: Lo\nmetadata:\n  name: clu\nspec:\n  kubernetes:\n    version: v1.31.12@sha256:0f5cc49c\n")
	writeFile(t, filepath.Join(p.Clusters, "lo.dev", ".registries.json"), `{"registries":[{"name":"build"},{"name":"cache"},{"name":"io-docker"}]}`)
	want := "\n  \033[1;36mlo.dev\033[0m  \033[2mlo · kind · v1.31.12\033[0m\n" +
		"  \033[2mkubeconfig  .kubeconfig/clu.yaml\033[0m\n" +
		"  \033[2mregistries  local · tls · 3\033[0m\n\n"
	if got := headerOf(t, p, "lo.dev"); got != want {
		t.Errorf("lo header:\n%q\nwant:\n%q", got, want)
	}

	// shared + explicit tls:false — the yq `//` quirk reads `false // true`
	// as true, so the bash header says "tls" for a plain-HTTP setup and so
	// does this one; no .registries.json → "—"; a float version prints as
	// written.
	writeFile(t, filepath.Join(p.Clusters, "sh.dev", "cluster.lok8s.yaml"),
		"kind: Lo\nmetadata:\n  name: clu\nspec:\n  kubernetes:\n    version: 1.30\n  registries:\n    tls: false\n    shared:\n      enabled: true\n")
	if got := headerOf(t, p, "sh.dev"); !strings.Contains(got, "lo · kind · 1.30\033") || !strings.Contains(got, "registries  shared · tls · —") {
		t.Errorf("shared header: %q", got)
	}

	// Cloud driver: no registries line; version without an @ suffix.
	writeFile(t, filepath.Join(p.Clusters, "k1.cloud", "cluster.lok8s.yaml"), "kind: KubeOne\nspec:\n  kubernetes:\n    version: \"1.31.0\"\n")
	if got := headerOf(t, p, "k1.cloud"); !strings.Contains(got, "\033[2mkubeone · 1.31.0\033[0m") || strings.Contains(got, "registries") {
		t.Errorf("kubeone header: %q", got)
	}

	// Deploy-only domain → "deploy → <ref>"; missing ref → "?".
	writeFile(t, filepath.Join(p.Clusters, "dep.app", "deploy.lok8s.yaml"), "kind: Deploy\nspec:\n  clusterRef:\n    domain: k1.cloud\n")
	if got := headerOf(t, p, "dep.app"); !strings.Contains(got, "\033[2mdeploy → k1.cloud\033[0m") {
		t.Errorf("deploy header: %q", got)
	}
	writeFile(t, filepath.Join(p.Clusters, "noref.app", "deploy.lok8s.yaml"), "kind: Deploy\n")
	if got := headerOf(t, p, "noref.app"); !strings.Contains(got, "deploy → ?") {
		t.Errorf("deploy header without ref: %q", got)
	}

	// No spec at all → empty meta; a spec without a readable kind → empty
	// meta too (never "null").
	if got := headerOf(t, p, "nope.dev"); !strings.Contains(got, "\033[1;36mnope.dev\033[0m  \033[2m\033[0m\n") {
		t.Errorf("no-spec header: %q", got)
	}
	writeFile(t, filepath.Join(p.Clusters, "nokind.dev", "cluster.lok8s.yaml"), "metadata:\n  name: x\nspec:\n  kubernetes:\n    version: \"1.31.0\"\n")
	if got := headerOf(t, p, "nokind.dev"); !strings.Contains(got, "\033[2m · 1.31.0\033[0m") {
		t.Errorf("no-kind header: %q", got)
	}
}

// rcErr is a subprocess failure with its own exit status (what a real
// *exec.ExitError carries; tilt's exitCode reads it through ExitCode()).
type rcErr int

func (e rcErr) Error() string { return "exit status " + strconv.Itoa(int(e)) }
func (e rcErr) ExitCode() int { return int(e) }

type upHarness struct {
	p       *config.Paths
	out     *bytes.Buffer
	errOut  *bytes.Buffer
	runner  *scriptRunner
	deps    upDeps
	exits   []int
	started []string
}

func newUpHarness(t *testing.T, dispatch error) *upHarness {
	t.Helper()
	h := &upHarness{p: synthProject(t), out: &bytes.Buffer{}, errOut: &bytes.Buffer{}, runner: &scriptRunner{}}
	writeFile(t, filepath.Join(h.p.Clusters, "lo.dev", "cluster.lok8s.yaml"), "kind: Lo\nmetadata:\n  name: clu\n")
	t.Setenv("DOMAIN_NAME", "lo.dev")
	t.Setenv("TILT_PORT", "")
	os.Unsetenv("TILT_PORT")
	t.Setenv("KUBECONFIG", filepath.Join(h.p.Base, ".kubeconfig", "clu.yaml"))
	h.runner.handler = func(c execx.Cmd) error {
		switch {
		case c.Name == "tilt" && c.Args[0] == "doctor":
			io.WriteString(c.Stdout, "Env: kind\n")
			return nil
		case c.Name == "tilt" && c.Args[0] == "get":
			return errors.New("no session")
		case c.Name == "tilt" && c.Args[0] == "ci":
			return rcErr(7)
		}
		return nil
	}
	h.deps = upDeps{
		dispatch: func(context.Context, string) error { return dispatch },
		tilt: &tilt.Context{
			Paths: h.p, Runner: h.runner, Out: h.out, ErrOut: h.errOut,
			StartDetached: func(port string) (int, error) { h.started = append(h.started, port); return 4242, nil },
		},
		lookPath: func(string) bool { return false },
	}
	prev := osExit
	osExit = func(code int) { h.exits = append(h.exits, code) }
	t.Cleanup(func() { osExit = prev })
	return h
}

func TestUpDispatchFailureStopsBeforeTilt(t *testing.T) {
	h := newUpHarness(t, errors.New("provision failed"))
	err := runUp(context.Background(), h.p, h.out, h.deps, "lo.dev", false, "", false)
	if !errors.Is(err, ErrHandled) || len(h.exits) != 0 {
		t.Fatalf("err=%v exits=%v", err, h.exits)
	}
	if !strings.HasPrefix(h.out.String(), "\n  \033[1;36mlo.dev\033[0m") {
		t.Errorf("header missing: %q", h.out.String())
	}
	if len(h.runner.calls) != 0 || len(h.started) != 0 {
		t.Errorf("tilt must not run after a failed dispatch: %v %v", h.runner.calls, h.started)
	}
}

func TestUpGateDeclinePassesRc3(t *testing.T) {
	h := newUpHarness(t, driver.ErrDeclined)
	_ = runUp(context.Background(), h.p, h.out, h.deps, "lo.dev", false, "", false)
	if len(h.exits) != 1 || h.exits[0] != 3 {
		t.Errorf("exits = %v", h.exits)
	}
}

func TestUpCIPassesTiltStatusThrough(t *testing.T) {
	h := newUpHarness(t, nil)
	_ = runUp(context.Background(), h.p, h.out, h.deps, "lo.dev", true, "10m", false)
	if len(h.exits) != 1 || h.exits[0] != 7 {
		t.Errorf("exits = %v (tilt ci's own status is the contract)", h.exits)
	}
	if !h.runner.has("tilt ci --port=") || !h.runner.has("--timeout 10m") || len(h.started) != 0 {
		t.Errorf("calls=%v started=%v", h.runner.calls, h.started)
	}
	if !strings.Contains(h.out.String(), "Tilt CI (headless) on port") {
		t.Errorf("out = %q", h.out.String())
	}
}

func TestUpInteractiveStartsTiltAndOpensBrowserFallback(t *testing.T) {
	h := newUpHarness(t, nil)
	if err := runUp(context.Background(), h.p, h.out, h.deps, "lo.dev", false, "", true); err != nil {
		t.Fatal(err)
	}
	if len(h.started) != 1 || len(h.exits) != 0 {
		t.Fatalf("started=%v exits=%v", h.started, h.exits)
	}
	port := h.started[0]
	if !strings.Contains(h.out.String(), "Tilt UI: http://localhost:"+port+"\n") {
		t.Errorf("out = %q", h.out.String())
	}
	// No opener on PATH → the URL is printed instead (twice: tilt::up's
	// own line, then the --open-tilt fallback).
	if strings.Count(h.out.String(), "Tilt UI: http://localhost:"+port) != 2 {
		t.Errorf("open-tilt fallback line missing: %q", h.out.String())
	}
	if raw, err := os.ReadFile(filepath.Join(h.p.Base, ".tilt.pid")); err != nil || string(raw) != "4242\n" {
		t.Errorf("pid file = %q err=%v", raw, err)
	}
}

func TestUpOpenTiltUsesOpener(t *testing.T) {
	h := newUpHarness(t, nil)
	var opened []string
	h.deps.lookPath = func(tool string) bool { return tool == "open" }
	h.deps.open = func(tool, url string) error { opened = append(opened, tool+" "+url); return nil }
	if err := runUp(context.Background(), h.p, h.out, h.deps, "lo.dev", false, "", true); err != nil {
		t.Fatal(err)
	}
	if len(opened) != 1 || !strings.HasPrefix(opened[0], "open http://localhost:") {
		t.Errorf("opened = %v", opened)
	}
}
