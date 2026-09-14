package cli

// routing_test.go — spec.implementation: which implementation a command
// runs through. Hermetic: the exec seam (shimExec) records what the shim
// would have exec'd, nothing spawns.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kernpilot/lok8s/internal/assets"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/testutil"
)

// shimRecord is one recorded exec: what the shim handed to syscall.Exec.
type shimRecord struct {
	bin  string
	argv []string
	env  []string
}

// recordShim swaps the exec seam for a recorder for the test's lifetime.
func recordShim(t *testing.T) *[]shimRecord {
	t.Helper()
	var recs []shimRecord
	prev := shimExec
	shimExec = func(bin string, argv, env []string) error {
		recs = append(recs, shimRecord{bin: bin, argv: argv, env: env})
		return nil
	}
	t.Cleanup(func() { shimExec = prev })
	return &recs
}

// setOSArgs sets os.Args the way the process would see `lo <args...>`
// (the shim passes os.Args[1:] verbatim, not cobra's parsed args).
func setOSArgs(t *testing.T, args ...string) {
	t.Helper()
	prev := os.Args
	os.Args = append([]string{"lo"}, args...)
	t.Cleanup(func() { os.Args = prev })
}

// routedProject is a synthetic project with the given spec.implementation
// block (the YAML below `spec:`; "" = no block) and, with tree, an
// executable .lok8s/lo. It ejects nothing: the file is the whole switch.
func routedProject(t *testing.T, block string, tree bool) *config.Paths {
	t.Helper()
	p := synthProject(t)
	content := "apiVersion: lok8s.dev/v1\nkind: Project\nmetadata:\n  name: t\n"
	if block != "" {
		content += "spec:\n  implementation:\n" + block
	}
	testutil.WriteFile(t, filepath.Join(p.Base, config.ProjectFile), content)
	if tree {
		testutil.WriteFile(t, filepath.Join(p.Lok8s, "lo"), "#!/usr/bin/env bash\n")
		os.Chmod(filepath.Join(p.Lok8s, "lo"), 0o755)
	}
	t.Setenv("PATH_LOK8S", "")
	os.Unsetenv("PATH_LOK8S")
	t.Setenv("PATH_BASE", "")
	os.Unsetenv("PATH_BASE")
	return p
}

// A project without the block, and one with `default: go`, run every
// command in Go: nothing is exec'd.
func TestRoutingUnsetRunsGo(t *testing.T) {
	for _, block := range []string{"", "    default: go\n", "    default: go\n    bash:\n      commands: []\n"} {
		p := routedProject(t, block, true)
		recs := recordShim(t)
		setOSArgs(t, "drivers", "--list")
		stdout, _, err := runLo(t, NewRoot(p), "drivers", "--list")
		if err != nil || !strings.Contains(stdout, "lo") {
			t.Errorf("block %q: drivers --list: %v\n%s", block, err, stdout)
		}
		if len(*recs) != 0 {
			t.Errorf("block %q: the Go path exec'd: %+v", block, *recs)
		}
		if r := newRouting(p); r.err != nil || r.active() {
			t.Errorf("block %q: routing = %+v", block, r)
		}
	}
}

// commands: [registry] routes `lo registry up --x` (and its alias) to
// <project>/.lok8s/lo with the verbatim argv and the prepared PATH; every
// other command stays Go.
func TestRoutingCommandsShimTheListedNames(t *testing.T) {
	p := routedProject(t, "    bash:\n      commands: [registry]\n", true)
	t.Setenv("PATH", "/usr/bin:/bin")
	recs := recordShim(t)

	setOSArgs(t, "registry", "up", "--x")
	if _, _, err := runLo(t, NewRoot(p), "registry", "up", "--x"); err != nil {
		t.Fatalf("routed registry: %v", err)
	}
	if len(*recs) != 1 {
		t.Fatalf("exec records = %+v", *recs)
	}
	rec := (*recs)[0]
	if !strings.HasSuffix(rec.bin, "/bash") {
		t.Errorf("bin = %q", rec.bin)
	}
	wantArgv := []string{rec.bin, filepath.Join(p.Base, ".lok8s", "lo"), "registry", "up", "--x"}
	if strings.Join(rec.argv, " ") != strings.Join(wantArgv, " ") {
		t.Errorf("argv = %v\nwant %v", rec.argv, wantArgv)
	}
	if v, _ := envValue(rec.env, "PATH"); !strings.HasPrefix(v, p.Bin+":"+p.Lok8s+":") {
		t.Errorf("PATH = %q", v)
	}
	// The tree is the project's .lok8s: no PATH_* exported on its behalf.
	for _, key := range []string{"PATH_BASE", "PATH_LOK8S"} {
		if v, ok := envValue(rec.env, key); ok {
			t.Errorf("%s=%q exported for the project tree", key, v)
		}
	}

	// The alias spelling reaches bash as typed.
	setOSArgs(t, "r", "status")
	if _, _, err := runLo(t, NewRoot(p), "r", "status"); err != nil {
		t.Fatalf("routed alias: %v", err)
	}
	if got := (*recs)[1].argv[2:]; strings.Join(got, " ") != "r status" {
		t.Errorf("alias argv = %v", got)
	}

	// An unlisted command runs in Go.
	setOSArgs(t, "drivers", "--list")
	if _, _, err := runLo(t, NewRoot(p), "drivers", "--list"); err != nil {
		t.Fatalf("drivers --list: %v", err)
	}
	if len(*recs) != 2 {
		t.Errorf("an unlisted command was exec'd: %+v", *recs)
	}
}

// default: bash shims the whole usage tree with the verbatim argv, global
// flags included; the Go-only commands stay Go.
func TestRoutingDefaultBashShimsEverything(t *testing.T) {
	p := routedProject(t, "    default: bash\n", true)
	recs := recordShim(t)

	setOSArgs(t, "--domain", "x.dev", "up", "--ci")
	if _, _, err := runLo(t, NewRoot(p), "--domain", "x.dev", "up", "--ci"); err != nil {
		t.Fatalf("routed up: %v", err)
	}
	if len(*recs) != 1 || strings.Join((*recs)[0].argv[2:], " ") != "--domain x.dev up --ci" {
		t.Fatalf("exec records = %+v", *recs)
	}
	root := NewRoot(p)
	for _, spec := range commandTree {
		cmd, _, err := root.Find([]string{spec.use})
		if err != nil || !cmd.DisableFlagParsing || cmd.HasSubCommands() {
			t.Errorf("%s is not a shim under default: bash", spec.use)
		}
	}
	for _, g := range goOnlyCommands {
		cmd, _, err := root.Find([]string{g.name})
		if err != nil || cmd.DisableFlagParsing {
			t.Errorf("Go-only %s became a shim", g.name)
		}
	}
}

// A routed tree below a subdirectory gets PATH_BASE and PATH_LOK8S: the
// entrypoint derives them from its own location, which is wrong there.
func TestRoutingNestedTreeExportsProjectPaths(t *testing.T) {
	p := routedProject(t, "    bash:\n      commands: [registry]\n      tree: vendor/lok8s\n", false)
	testutil.WriteFile(t, filepath.Join(p.Base, "vendor", "lok8s", "lo"), "#!/usr/bin/env bash\n")
	recs := recordShim(t)
	setOSArgs(t, "registry", "up")
	if _, _, err := runLo(t, NewRoot(p), "registry", "up"); err != nil {
		t.Fatalf("routed registry: %v", err)
	}
	rec := (*recs)[0]
	if rec.argv[1] != filepath.Join(p.Base, "vendor", "lok8s", "lo") {
		t.Errorf("argv = %v", rec.argv)
	}
	if v, _ := envValue(rec.env, "PATH_BASE"); v != p.Base {
		t.Errorf("PATH_BASE = %q", v)
	}
	if v, _ := envValue(rec.env, "PATH_LOK8S"); v != filepath.Join(p.Base, "vendor", "lok8s") {
		t.Errorf("PATH_LOK8S = %q", v)
	}
}

// Every invalid block: the exact message, every command but lint refuses
// to start with it, lint reports it as a finding, nothing is exec'd.
func TestRoutingInvalidBlockRefuses(t *testing.T) {
	cases := []struct{ name, block, want string }{
		{"unknown", "    bash:\n      commands: [registry, foo]\n", `lok8s.yaml: spec.implementation.bash.commands: unknown command "foo". Use top-level command names from "lo --help".`},
		{"go-only", "    bash:\n      commands: [assets]\n", `lok8s.yaml: "assets" has no bash implementation.`},
		{"go-only-mcp", "    bash:\n      commands: [mcp]\n", `lok8s.yaml: "mcp" has no bash implementation.`},
		{"go-only-operator", "    bash:\n      commands: [operator]\n", `lok8s.yaml: "operator" has no bash implementation.`},
		{"go-only-subcommands", "    bash:\n      commands: [init]\n", `lok8s.yaml: "init" has no bash implementation for "init project|toolchain".`},
		{"alias", "    bash:\n      commands: [r]\n", `lok8s.yaml: spec.implementation.bash.commands: "r" is an alias. Use the command name "registry".`},
		{"all", "    bash:\n      commands: [all]\n", `lok8s.yaml: spec.implementation.bash.commands: unknown command "all". Use top-level command names from "lo --help".`},
		{"twice", "    bash:\n      commands: [registry, registry]\n", `lok8s.yaml: spec.implementation.bash.commands: "registry" is listed twice.`},
		{"default", "    default: python\n", `lok8s.yaml: spec.implementation.default "python" is not "go" or "bash".`},
		{"dotdot", "    default: bash\n    bash:\n      tree: ..\n", `lok8s.yaml: spec.implementation.bash.tree ".." leaves the project.`},
		{"absolute", "    default: bash\n    bash:\n      tree: /opt/lok8s\n", `lok8s.yaml: spec.implementation.bash.tree "/opt/lok8s" is absolute. Use a path relative to the project.`},
	}
	for _, c := range cases {
		p := routedProject(t, c.block, true)
		recs := recordShim(t)
		if r := newRouting(p); r.err == nil || r.err.Error() != c.want {
			t.Errorf("%s: routing err = %v\nwant %s", c.name, r.err, c.want)
		}
		setOSArgs(t, "registry", "up")
		_, stderr, err := runLo(t, NewRoot(p), "registry", "up")
		if !errors.Is(err, ErrHandled) || stderr != "lo: "+c.want+"\n" {
			t.Errorf("%s: registry up: err=%v stderr=%q", c.name, err, stderr)
		}
		setOSArgs(t, "drivers", "--list")
		_, stderr, err = runLo(t, NewRoot(p), "drivers", "--list")
		if !errors.Is(err, ErrHandled) || stderr != "lo: "+c.want+"\n" {
			t.Errorf("%s: drivers --list: err=%v stderr=%q", c.name, err, stderr)
		}
		// lint runs and reports the block as a finding.
		setOSArgs(t, "lint", "--domain", "none.dev")
		_, stderr, err = runLo(t, NewRoot(p), "lint", "--domain", "none.dev")
		if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "[error] "+c.want+"\n") || strings.Contains(stderr, "lo: ") {
			t.Errorf("%s: lint stderr = %q", c.name, stderr)
		}
		if len(*recs) != 0 {
			t.Errorf("%s: exec'd on an invalid block: %+v", c.name, *recs)
		}
	}
}

// A symlinked tree that resolves outside the project is refused.
func TestRoutingTreeSymlinkEscapeRefuses(t *testing.T) {
	outside := t.TempDir()
	testutil.WriteFile(t, filepath.Join(outside, "lo"), "#!/usr/bin/env bash\n")
	p := routedProject(t, "    default: bash\n    bash:\n      tree: vendor\n", false)
	if err := os.Symlink(outside, filepath.Join(p.Base, "vendor")); err != nil {
		t.Fatal(err)
	}
	want := `lok8s.yaml: spec.implementation.bash.tree "vendor" resolves to ` + mustEvalSymlinks(t, outside) + `, outside the project.`
	if r := newRouting(p); r.err == nil || r.err.Error() != want {
		t.Errorf("routing err = %v\nwant %s", r.err, want)
	}
	// The default .lok8s as a symlink out of the project: the same rule.
	q := routedProject(t, "    default: bash\n", false)
	os.Remove(q.Lok8s)
	if err := os.Symlink(outside, q.Lok8s); err != nil {
		t.Fatal(err)
	}
	if r := newRouting(q); r.err == nil || !strings.HasSuffix(r.err.Error(), ", outside the project.") {
		t.Errorf("symlinked .lok8s: %v", r.err)
	}
}

// A routing without the tree in the project is refused with the fix, and
// the cache extract is never used in its place.
func TestRoutingMissingTreeRefusesAndNeverUsesTheCache(t *testing.T) {
	cacheRoot := t.TempDir()
	t.Setenv(assets.EnvCacheHome, cacheRoot)

	p := routedProject(t, "    default: bash\n", false)
	// The cache holds a complete, valid tree (what the drivers fallback
	// would run); routing must not take it.
	if tree, err := assets.BashTree(p); err != nil || tree.Source != assets.TreeCache {
		t.Fatalf("cache extract: %+v %v", tree, err)
	}
	recs := recordShim(t)
	want := "implementation bash: the tree " + filepath.Join(p.Base, ".lok8s", "lo") + ` is missing. Run "lo assets eject bash", or set spec.implementation.default: go.`
	if r := newRouting(p); r.err != nil || r.treeErr == nil || r.treeErr.Error() != want {
		t.Errorf("routing = err %v treeErr %v\nwant %s", r.err, r.treeErr, want)
	}
	setOSArgs(t, "up")
	_, stderr, err := runLo(t, NewRoot(p), "up")
	if !errors.Is(err, ErrHandled) || stderr != "lo: "+want+"\n" {
		t.Errorf("up: err=%v stderr=%q", err, stderr)
	}
	if len(*recs) != 0 {
		t.Errorf("exec'd without a project tree: %+v", *recs)
	}

	// The commands form names its own fix.
	q := routedProject(t, "    bash:\n      commands: [registry]\n", false)
	want = "implementation bash: the tree " + filepath.Join(q.Base, ".lok8s", "lo") + ` is missing. Run "lo assets eject bash", or remove spec.implementation.bash.commands.`
	if r := newRouting(q); r.treeErr == nil || r.treeErr.Error() != want {
		t.Errorf("commands form: treeErr = %v\nwant %s", r.treeErr, want)
	}

	// A PATH_LOK8S checkout elsewhere is not the project's tree either.
	checkout := t.TempDir()
	testutil.WriteFile(t, filepath.Join(checkout, "lo"), "#!/usr/bin/env bash\n")
	t.Setenv("PATH_LOK8S", checkout)
	if r := newRouting(q); r.treeErr == nil || !strings.HasSuffix(r.treeErr.Error(), "remove spec.implementation.bash.commands.") {
		t.Errorf("checkout elsewhere accepted: %v", r.treeErr)
	}
}

// The MCP projection ignores the routing: the tool list is the same with
// `default: bash` as without a block.
func TestRoutingDoesNotChangeTheMcpToolList(t *testing.T) {
	list := func(p *config.Paths) string {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		tools, err := mcpListTools(ctx, p, mcpExposure{destructive: true})
		if err != nil {
			t.Fatal(err)
		}
		names := make([]string, 0, len(tools))
		for _, tool := range tools {
			names = append(names, tool.Name)
		}
		return strings.Join(names, "\n")
	}
	plain := list(routedProject(t, "", true))
	routed := list(routedProject(t, "    default: bash\n", true))
	listed := list(routedProject(t, "    bash:\n      commands: [registry, up]\n", true))
	if plain == "" || plain != routed || plain != listed {
		t.Errorf("tool list changed with routing:\n--- plain\n%s\n--- default bash\n%s\n--- commands\n%s", plain, routed, listed)
	}
	if !strings.Contains(plain, "lo_registry_up") {
		t.Errorf("projection lost the registry leaves:\n%s", plain)
	}
}

// doctor prints the implementation lines only when something is routed.
func TestDoctorImplementationLines(t *testing.T) {
	doctor := func(p *config.Paths) string {
		var b strings.Builder
		doctorImplementation(&b, p)
		return b.String()
	}
	for _, block := range []string{"", "    default: go\n"} {
		if out := doctor(routedProject(t, block, true)); out != "" {
			t.Errorf("block %q printed:\n%s", block, out)
		}
	}
	out := doctor(routedProject(t, "    bash:\n      commands: [registry, use, lint]\n", true))
	for _, want := range []string{
		"  ✓ implementation: go; bash for registry, use, lint; tree .lok8s (project, lok8s.yaml spec.implementation)\n",
		"  ! registry is routed to bash; lo up manages the same registries with Go code. Keep the lib stock or expect drift.\n",
		"  ! use is routed to bash; every Go command reads the same clusters/.active. Keep the lib stock or expect drift.\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "lint is routed") {
		t.Errorf("lint shares no state:\n%s", out)
	}
	out = doctor(routedProject(t, "    default: bash\n    bash:\n      tree: vendor/lok8s\n", false))
	if !strings.Contains(out, "  ! implementation: implementation bash: the tree") {
		// The tree is missing: a warning, not an ok line.
		t.Errorf("missing tree:\n%s", out)
	}
	p := routedProject(t, "    default: bash\n", true)
	out = doctor(p)
	if want := "  ✓ implementation: bash for every command; tree .lok8s (project, lok8s.yaml spec.implementation)\n"; !strings.Contains(out, want) || strings.Contains(out, "routed to bash;") {
		t.Errorf("default bash:\n%s", out)
	}
}

func mustEvalSymlinks(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// ── review round 1 ─────────────────────────────────────────────────────

// F1: the symlink escape rule applies only when something routes. A
// project that runs Go may link its .lok8s to a checkout elsewhere (what
// hack/e2e-go-roundtrip.sh builds); the same link under `default: bash`
// is refused.
func TestRoutingSymlinkedTreeIsFineWithoutRouting(t *testing.T) {
	outside := t.TempDir()
	testutil.WriteFile(t, filepath.Join(outside, "lo"), "#!/usr/bin/env bash\n")
	for _, block := range []string{"", "    default: go\n"} {
		p := routedProject(t, block, false)
		os.Remove(p.Lok8s)
		if err := os.Symlink(outside, p.Lok8s); err != nil {
			t.Fatal(err)
		}
		recs := recordShim(t)
		if r := newRouting(p); r.err != nil || r.treeErr != nil {
			t.Errorf("block %q: %v %v", block, r.err, r.treeErr)
		}
		setOSArgs(t, "drivers", "--list")
		if _, stderr, err := runLo(t, NewRoot(p), "drivers", "--list"); err != nil || strings.Contains(stderr, "lo: ") {
			t.Errorf("block %q: Go did not run: %v %q", block, err, stderr)
		}
		if len(*recs) != 0 {
			t.Errorf("block %q: exec'd", block)
		}
	}
	p := routedProject(t, "    default: bash\n", false)
	os.Remove(p.Lok8s)
	if err := os.Symlink(outside, p.Lok8s); err != nil {
		t.Fatal(err)
	}
	want := `lok8s.yaml: spec.implementation.bash.tree ".lok8s" resolves to ` + mustEvalSymlinks(t, outside) + `, outside the project.`
	if r := newRouting(p); r.err == nil || r.err.Error() != want {
		t.Errorf("default bash: %v\nwant %s", r.err, want)
	}
	setOSArgs(t, "up")
	if _, stderr, err := runLo(t, NewRoot(p), "up"); !errors.Is(err, ErrHandled) || stderr != "lo: "+want+"\n" {
		t.Errorf("up: %v %q", err, stderr)
	}
}

// F2: the missing tree is a routing precondition, not a block error. The
// Go-only commands and lint/doctor/init run, so `lo assets eject bash`
// can create the tree the message asks for; a routed command refuses
// until then, then execs.
func TestRoutingMissingTreeStillAllowsEject(t *testing.T) {
	quietAssets(t)
	p := routedProject(t, "    default: bash\n", false)
	recs := recordShim(t)
	want := "implementation bash: the tree " + filepath.Join(p.Base, ".lok8s", "lo") + ` is missing. Run "lo assets eject bash", or set spec.implementation.default: go.`
	r := newRouting(p)
	if r.err != nil || r.treeErr == nil || r.treeErr.Error() != want {
		t.Fatalf("routing = err %v treeErr %v", r.err, r.treeErr)
	}
	setOSArgs(t, "up")
	if _, stderr, err := runLo(t, NewRoot(p), "up"); !errors.Is(err, ErrHandled) || stderr != "lo: "+want+"\n" {
		t.Errorf("up before eject: %v %q", err, stderr)
	}
	setOSArgs(t, "lint", "--domain", "none.dev")
	if _, stderr, err := runLo(t, NewRoot(p), "lint", "--domain", "none.dev"); !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "[error] "+want+"\n") {
		t.Errorf("lint reports the missing tree: %v %q", err, stderr)
	}
	setOSArgs(t, "assets", "eject", "bash")
	if _, stderr, err := runLo(t, NewRoot(p), "assets", "eject", "bash"); err != nil || strings.Contains(stderr, "lo: ") {
		t.Fatalf("assets eject bash: %v %q", err, stderr)
	}
	if info, err := os.Stat(filepath.Join(p.Lok8s, "lo")); err != nil || info.Mode()&0o100 == 0 {
		t.Fatalf("eject did not create the tree: %v", err)
	}
	if len(*recs) != 0 {
		t.Fatalf("exec'd before the tree existed: %+v", *recs)
	}
	setOSArgs(t, "up")
	if _, _, err := runLo(t, NewRoot(p), "up"); err != nil {
		t.Fatalf("up after eject: %v", err)
	}
	if len(*recs) != 1 || strings.Join((*recs)[0].argv[1:], " ") != filepath.Join(p.Lok8s, "lo")+" up" {
		t.Errorf("exec records = %+v", *recs)
	}
}

// F3/F7: an invalid block does not stop doctor (it warns), help or
// completion; the doctor line is asserted through the command.
func TestRoutingInvalidBlockStillRunsDoctorHelpCompletion(t *testing.T) {
	p := routedProject(t, "    bash:\n      commands: [bogus]\n", true)
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("HOME", t.TempDir())
	want := `lok8s.yaml: spec.implementation.bash.commands: unknown command "bogus". Use top-level command names from "lo --help".`
	setOSArgs(t, "doctor")
	stdout, stderr, _ := runLo(t, NewRoot(p), "doctor")
	if strings.Contains(stderr, "lo: ") || !strings.Contains(stdout, "  ! implementation: "+want+"\n") {
		t.Errorf("doctor: stdout=%q stderr=%q", stdout, stderr)
	}
	for _, args := range [][]string{{"help"}, {"help", "up"}, {"completion", "bash"}} {
		setOSArgs(t, args...)
		if _, stderr, err := runLo(t, NewRoot(p), args...); err != nil || strings.Contains(stderr, "lo: ") {
			t.Errorf("%v: %v %q", args, err, stderr)
		}
	}
	// A missing tree: doctor runs in Go and warns.
	q := routedProject(t, "    default: bash\n", false)
	setOSArgs(t, "doctor")
	stdout, _, _ = runLo(t, NewRoot(q), "doctor")
	if !strings.Contains(stdout, "  ! implementation: implementation bash: the tree ") {
		t.Errorf("doctor without the tree:\n%s", stdout)
	}
}

// F4: an inherited PATH_BIN never reaches the routed exec; the child gets
// <project>/.bin on PATH and in PATH_BIN.
func TestRoutingIgnoresInheritedPathBin(t *testing.T) {
	p := routedProject(t, "    bash:\n      commands: [registry]\n", true)
	p.Bin = "/evil"
	t.Setenv("PATH_BIN", "/evil")
	t.Setenv("PATH", "/usr/bin:/bin")
	recs := recordShim(t)
	setOSArgs(t, "registry", "up")
	if _, _, err := runLo(t, NewRoot(p), "registry", "up"); err != nil {
		t.Fatal(err)
	}
	env := (*recs)[0].env
	projBin := filepath.Join(p.Base, ".bin")
	if v, _ := envValue(env, "PATH"); !strings.HasPrefix(v, projBin+":"+p.Lok8s+":") || strings.Contains(v, "/evil") {
		t.Errorf("PATH = %q", v)
	}
	if v, _ := envValue(env, "PATH_BIN"); v != projBin {
		t.Errorf("PATH_BIN = %q", v)
	}
	// Unset ambient: nothing exported on the entrypoint's behalf.
	os.Unsetenv("PATH_BIN")
	setOSArgs(t, "registry", "up")
	runLo(t, NewRoot(p), "registry", "up")
	if v, ok := envValue((*recs)[1].env, "PATH_BIN"); ok {
		t.Errorf("PATH_BIN=%q exported without an ambient value", v)
	}
}

// F6: unknown keys are lint warnings, never a silent `go`.
func TestRoutingUnknownKeysWarnInLint(t *testing.T) {
	p := routedProject(t, "    defaults: bash\n    bash:\n      command: [registry]\n", true)
	testutil.WriteFile(t, filepath.Join(p.Base, config.ProjectFile),
		"kind: Project\nspec:\n  implementations: {default: bash}\n  implementation:\n    defaults: bash\n    bash:\n      command: [registry]\n")
	r := newRouting(p)
	if r.err != nil || r.active() {
		t.Fatalf("routing = %+v", r)
	}
	setOSArgs(t, "lint", "--domain", "none.dev")
	_, stderr, _ := runLo(t, NewRoot(p), "lint", "--domain", "none.dev")
	for _, want := range []string{
		"[warn] lok8s.yaml: spec.implementations: unknown key. Use spec.implementation.\n",
		"[warn] lok8s.yaml: spec.implementation: unknown key \"defaults\".\n",
		"[warn] lok8s.yaml: spec.implementation.bash: unknown key \"command\".\n",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("missing %q in:\n%s", want, stderr)
		}
	}
}

// F8: the entrypoint itself must resolve inside the project.
func TestRoutingEntrypointSymlinkEscapeRefuses(t *testing.T) {
	outside := t.TempDir()
	testutil.WriteFile(t, filepath.Join(outside, "lo"), "#!/usr/bin/env bash\n")
	p := routedProject(t, "    bash:\n      commands: [registry]\n", false)
	if err := os.Symlink(filepath.Join(outside, "lo"), filepath.Join(p.Lok8s, "lo")); err != nil {
		t.Fatal(err)
	}
	want := "lok8s.yaml: spec.implementation.bash.tree: the entrypoint " + filepath.Join(".lok8s", "lo") + " resolves to " + mustEvalSymlinks(t, filepath.Join(outside, "lo")) + ", outside the project."
	if r := newRouting(p); r.err == nil || r.err.Error() != want {
		t.Errorf("routing err = %v\nwant %s", r.err, want)
	}
	recs := recordShim(t)
	setOSArgs(t, "registry", "up")
	if _, stderr, err := runLo(t, NewRoot(p), "registry", "up"); !errors.Is(err, ErrHandled) || stderr != "lo: "+want+"\n" || len(*recs) != 0 {
		t.Errorf("registry up: %v %q %+v", err, stderr, *recs)
	}
	// A link that stays inside the project runs.
	q := routedProject(t, "    bash:\n      commands: [registry]\n", false)
	testutil.WriteFile(t, filepath.Join(q.Base, "real", "lo"), "#!/usr/bin/env bash\n")
	if err := os.Symlink(filepath.Join(q.Base, "real", "lo"), filepath.Join(q.Lok8s, "lo")); err != nil {
		t.Fatal(err)
	}
	setOSArgs(t, "registry", "up")
	if _, _, err := runLo(t, NewRoot(q), "registry", "up"); err != nil || len(*recs) != 1 {
		t.Errorf("inside link: %v %+v", err, *recs)
	}
}
