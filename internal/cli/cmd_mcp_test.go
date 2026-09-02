package cli

// Tests for the native MCP server (lo mcp). Everything is hermetic: the
// tool list comes from a real ophis server over an in-memory transport (no
// subprocess, no cluster), the stdio smoke test drives the server over
// os.Pipe with raw JSON-RPC, and no tool is ever CALLED — a call would spawn
// the test binary. The synthetic project has no .lok8s/lo, so even an
// accidental shim exec would fail before reaching bash.

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/config"
)

// argshMcpTools is the surface the argsh `lo mcp` builtin advertised
// (captured from `.lok8s/lo mcp` tools/list, 2026-09-02). The ophis surface
// must stay a superset of it under the documented renames (argshMcpRenames).
var argshMcpTools = []string{
	"lo_addons", "lo_ai_check", "lo_ai_link", "lo_ai_skills", "lo_ai_unlink", "lo_audit",
	"lo_bootstrap", "lo_build", "lo_chat", "lo_clean", "lo_deploy", "lo_destroy", "lo_doctor",
	"lo_down", "lo_drivers", "lo_gitops_argo", "lo_gitops_flux", "lo_handover_preseed",
	"lo_handover_receive", "lo_image_cache", "lo_image_clean", "lo_image_list", "lo_init_service",
	"lo_init_test", "lo_kubeconfig", "lo_kubehz_assess", "lo_kubehz_claim", "lo_kubehz_claim-code",
	"lo_kubehz_deploy", "lo_kubehz_deregister", "lo_kubehz_join", "lo_kubehz_re-enroll",
	"lo_kubehz_register", "lo_kubehz_status", "lo_kustomize_build", "lo_kustomize_clean",
	"lo_kustomize_list", "lo_kustomize_test", "lo_lint", "lo_provision", "lo_recover",
	"lo_registry_clean", "lo_registry_down", "lo_registry_status", "lo_registry_up",
	"lo_secrets_add-key", "lo_secrets_allow", "lo_secrets_decrypt", "lo_secrets_encrypt",
	"lo_secrets_env", "lo_secrets_init", "lo_secrets_list", "lo_secrets_path", "lo_secrets_print",
	"lo_secrets_set", "lo_status", "lo_tilt_ci", "lo_tilt_down", "lo_tilt_preflight",
	"lo_tilt_restart", "lo_tilt_status", "lo_tilt_up", "lo_trust", "lo_up", "lo_use", "lo_version",
}

// argshMcpRenames maps an argsh tool name to the ophis name(s) that replace
// it. The argsh flattener dropped the middle of a two-level dispatcher path
// (kubehz handover receive → lo_handover_receive); ophis keeps the full path.
// `lo drivers` was one tool taking "<driver> <op> <domain>" as args; the Go
// tree spells the drivers out.
var argshMcpRenames = map[string][]string{
	"lo_handover_preseed": {"lo_kubehz_handover_preseed"},
	"lo_handover_receive": {"lo_kubehz_handover_receive"},
	"lo_drivers":          {"lo_drivers_lo_status", "lo_drivers_lo_provision", "lo_drivers_lo_destroy"},
}

func mcpToolNames(t *testing.T, x mcpExposure) map[string]*mcp.Tool {
	t.Helper()
	p := synthProject(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tools, err := mcpListTools(ctx, p, x)
	if err != nil {
		t.Fatalf("mcpListTools(%+v): %v", x, err)
	}
	byName := map[string]*mcp.Tool{}
	for _, tool := range tools {
		byName[tool.Name] = tool
	}
	return byName
}

func flagNames(t *testing.T, tool *mcp.Tool) map[string]bool {
	t.Helper()
	raw, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			Flags struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"flags"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("input schema of %s: %v", tool.Name, err)
	}
	names := map[string]bool{}
	for name := range schema.Properties.Flags.Properties {
		names[name] = true
	}
	return names
}

// ── classification ─────────────────────────────────────────────────────

func TestMcpTierClassification(t *testing.T) {
	ann := func(spec commandSpec) map[string]string { return spec.annotations() }
	root := &cobra.Command{Use: "lo"}
	ro := &cobra.Command{Use: "ro", Annotations: ann(commandSpec{readonly: true})}
	idem := &cobra.Command{Use: "idem", Annotations: ann(commandSpec{idempotent: true})}
	destr := &cobra.Command{Use: "destr", Annotations: ann(commandSpec{destructive: true, idempotent: true})}
	bare := &cobra.Command{Use: "bare"}
	dispatcher := &cobra.Command{Use: "disp"}
	inheritsRO := &cobra.Command{Use: "child"}
	dispatcherRO := &cobra.Command{Use: "dispro", Annotations: ann(commandSpec{readonly: true})}
	overridesRO := &cobra.Command{Use: "child", Annotations: ann(commandSpec{destructive: true})}
	inheritsFromRO := &cobra.Command{Use: "plain"}
	dispatcher.AddCommand(inheritsRO)
	dispatcherRO.AddCommand(overridesRO, inheritsFromRO)
	root.AddCommand(ro, idem, destr, bare, dispatcher, dispatcherRO)

	cases := map[*cobra.Command]int{
		ro: mcpTierReadonly, idem: mcpTierMutating, destr: mcpTierDestructive, bare: mcpTierMutating,
		inheritsRO: mcpTierMutating, overridesRO: mcpTierDestructive, inheritsFromRO: mcpTierReadonly,
	}
	for cmd, want := range cases {
		if got := mcpTier(cmd); got != want {
			t.Errorf("tier(%s) = %d, want %d", cmd.CommandPath(), got, want)
		}
	}

	mcpAnnotate(root)
	if inheritsFromRO.Annotations["readOnlyHint"] != "true" || inheritsFromRO.Annotations["destructiveHint"] != "false" {
		t.Errorf("inherited readonly hints not stamped: %v", inheritsFromRO.Annotations)
	}
	if destr.Annotations["destructiveHint"] != "true" || destr.Annotations["idempotentHint"] != "true" {
		t.Errorf("destructive hints not stamped: %v", destr.Annotations)
	}
	if _, set := bare.Annotations["destructiveHint"]; set {
		t.Errorf("unmarked command must not claim a destructiveHint: %v", bare.Annotations)
	}
	if idem.Annotations["destructiveHint"] != "false" {
		t.Errorf("idempotent-only command should be marked non-destructive: %v", idem.Annotations)
	}
}

func TestMcpExposureGates(t *testing.T) {
	var none mcpExposure
	if !none.allows(mcpTierReadonly) || none.allows(mcpTierMutating) || none.allows(mcpTierDestructive) {
		t.Error("default exposure must be readonly only")
	}
	mut := mcpExposure{mutating: true}
	if !mut.allows(mcpTierMutating) || mut.allows(mcpTierDestructive) {
		t.Error("--allow-mutating must not expose destructive")
	}
	des := mcpExposure{destructive: true}
	if !des.allows(mcpTierMutating) || !des.allows(mcpTierDestructive) {
		t.Error("--allow-destructive must imply mutating")
	}

	for _, name := range []string{"token", "api-token", "secret", "ssh-key", "nonce", "password", "verbose", "help"} {
		if des.allowsFlag(name) {
			t.Errorf("flag %q must never be exposed", name)
		}
	}
	for _, name := range []string{"domain", "cluster-id", "dry-run", "oidc", "name"} {
		if !none.allowsFlag(name) {
			t.Errorf("flag %q must be exposed", name)
		}
	}
	if none.allowsFlag("force") || mut.allowsFlag("force-recreate") || !des.allowsFlag("force") {
		t.Error("--force flags are destructive-only")
	}

	t.Setenv(mcpAllowEnv, "destructive")
	var fromEnv mcpExposure
	if err := fromEnv.resolve(); err != nil || !fromEnv.destructive || !fromEnv.mutating {
		t.Errorf("LO_MCP_ALLOW=destructive not honored: %+v err=%v", fromEnv, err)
	}
	t.Setenv(mcpAllowEnv, "everything")
	if err := fromEnv.resolve(); err == nil {
		t.Error("unknown LO_MCP_ALLOW value must be rejected")
	}
}

// ── tool list from the real tree ───────────────────────────────────────

func TestMcpToolsDefaultReadonlyOnly(t *testing.T) {
	tools := mcpToolNames(t, mcpExposure{})
	if len(tools) == 0 {
		t.Fatal("no tools")
	}
	for name, tool := range tools {
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s exposed by default without readOnlyHint", name)
		}
		if strings.Count(name, "_") == 0 {
			t.Errorf("tool %q has no lo_ prefix", name)
		}
	}
	// A ported readonly leaf, a shim readonly leaf, readonly leaves under
	// unmarked dispatchers.
	for _, want := range []string{"lo_version", "lo_status", "lo_secrets_list", "lo_kubehz_status", "lo_tilt_status", "lo_image_list"} {
		if tools[want] == nil {
			t.Errorf("readonly tool %s missing by default", want)
		}
	}
	// Hidden subtrees (hooks, env, k8s, crds) stay internal even where a leaf
	// is readonly.
	for _, absent := range []string{"lo_up", "lo_build", "lo_secrets_encrypt", "lo_deploy", "lo_registry_up", "lo_secrets", "lo_kubehz", "lo_mcp", "lo_mcp_start", "lo_hooks", "lo_env_services", "lo_crds_check", "lo_k8s_capi_platform"} {
		if tools[absent] != nil {
			t.Errorf("%s must not be exposed by default", absent)
		}
	}
	for _, tool := range tools {
		flags := flagNames(t, tool)
		for _, gated := range []string{"force", "force-recreate", "verbose", "help"} {
			if flags[gated] {
				t.Errorf("%s exposes gated flag --%s by default", tool.Name, gated)
			}
		}
		if !flags["domain"] {
			t.Errorf("%s lost the inherited --domain flag", tool.Name)
		}
	}
}

func TestMcpToolsMutatingOptIn(t *testing.T) {
	tools := mcpToolNames(t, mcpExposure{mutating: true})
	for _, want := range []string{"lo_build", "lo_use", "lo_secrets_encrypt", "lo_registry_up", "lo_registry_status", "lo_init_service", "lo_gitops_flux", "lo_version"} {
		if tools[want] == nil {
			t.Errorf("mutating tool %s missing with --allow-mutating", want)
		}
	}
	for _, absent := range []string{"lo_up", "lo_down", "lo_deploy", "lo_secrets_set", "lo_tilt_down", "lo_kubehz_deregister", "lo_image_clean"} {
		if tools[absent] != nil {
			t.Errorf("destructive %s exposed with only --allow-mutating", absent)
		}
	}
	if d := tools["lo_build"].Annotations.DestructiveHint; d == nil || *d {
		t.Error("lo_build (idempotent) should carry destructiveHint=false")
	}
	if tools["lo_registry_up"].Annotations != nil && tools["lo_registry_up"].Annotations.DestructiveHint != nil {
		t.Error("lo_registry_up (unmarked in bash) must not claim a destructiveHint")
	}
	if flagNames(t, tools["lo_build"])["force"] {
		t.Error("--force exposed without --allow-destructive")
	}
}

func TestMcpToolsDestructiveOptInIsSupersetOfArgsh(t *testing.T) {
	tools := mcpToolNames(t, mcpExposure{destructive: true})
	for _, want := range []string{"lo_up", "lo_down", "lo_deploy", "lo_destroy", "lo_tilt_down", "lo_kubehz_deregister", "lo_image_clean", "lo_secrets_set", "lo_drivers_lo_provision"} {
		if tools[want] == nil {
			t.Errorf("destructive tool %s missing with --allow-destructive", want)
		}
	}
	if tools["lo_up"].Annotations == nil || tools["lo_up"].Annotations.DestructiveHint == nil || !*tools["lo_up"].Annotations.DestructiveHint {
		t.Error("lo_up must carry destructiveHint=true")
	}
	if !flagNames(t, tools["lo_deploy"])["force"] {
		t.Error("--force missing on lo_deploy with --allow-destructive")
	}
	// Dispatchers are traversed, never exposed.
	for _, parent := range []string{"lo_secrets", "lo_tilt", "lo_kubehz", "lo_kubehz_handover", "lo_kubehz_node", "lo_drivers", "lo_drivers_lo", "lo_image", "lo_ai", "lo_init", "lo_gitops", "lo_kustomize", "lo_registry"} {
		if tools[parent] != nil {
			t.Errorf("dispatcher %s exposed as a tool", parent)
		}
	}
	// Superset of the argsh surface under the documented renames.
	for _, name := range argshMcpTools {
		wanted := []string{name}
		if renamed, ok := argshMcpRenames[name]; ok {
			wanted = renamed
		}
		for _, w := range wanted {
			if tools[w] == nil {
				t.Errorf("argsh tool %s (as %s) missing from the ophis surface", name, w)
			}
		}
	}
}

func TestMcpToolsNeverExposeSensitiveFlags(t *testing.T) {
	tools := mcpToolNames(t, mcpExposure{destructive: true})
	for name, tool := range tools {
		for flag := range flagNames(t, tool) {
			if mcpSensitiveFlag(flag) || mcpNeverFlags[flag] {
				t.Errorf("%s exposes flag --%s", name, flag)
			}
		}
	}
	// Concrete flags that exist on the tree today.
	if flagNames(t, tools["lo_kubehz_claim"])["nonce"] {
		t.Error("lo_kubehz_claim exposes --nonce")
	}
	if flagNames(t, tools["lo_secrets_decrypt"])["ssh-key"] || flagNames(t, tools["lo_kubehz_handover_preseed"])["ssh-key"] {
		t.Error("--ssh-key exposed")
	}
	if !flagNames(t, tools["lo_kubehz_deploy"])["dry-run"] {
		t.Error("lo_kubehz_deploy lost its --dry-run flag")
	}
}

// ── shim leaves ────────────────────────────────────────────────────────

// shimLeafSources maps each shim dispatcher in shimLeaves to the bash file
// and function holding its usage array.
var shimLeafSources = map[string]struct{ file, fn string }{
	"registry": {file: filepath.Join(".lok8s", "drivers", "lo", "libs", "registry"), fn: "main::registry() {"},
}

// parseShimUsage parses the `local -a usage=(` array of one bash function
// (the first array after the anchor text) with the same line grammar as
// parseArgshUsage, markers included.
func parseShimUsage(t *testing.T, file, anchor string) map[string]commandSpec {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("reading %s: %v", file, err)
	}
	text := string(raw)
	at := strings.Index(text, anchor)
	if at < 0 {
		t.Fatalf("anchor %q not found in %s", anchor, file)
	}
	text = text[at:]
	start := strings.Index(text, "local -a usage=(")
	if start < 0 {
		t.Fatalf("usage array not found after %q in %s", anchor, file)
	}
	specs := map[string]commandSpec{}
	for _, line := range strings.Split(text[start:], "\n") {
		if strings.TrimSpace(line) == ")" {
			break
		}
		m := usageEntry.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		spec := commandSpec{
			use:         m[2],
			hidden:      m[1] == "#",
			short:       m[6],
			destructive: strings.Contains(m[5], "@destructive"),
			readonly:    strings.Contains(m[5], "@readonly"),
			idempotent:  strings.Contains(m[5], "@idempotent"),
		}
		if m[4] != "" {
			spec.aliases = []string{m[4]}
		}
		specs[spec.use] = spec
	}
	if len(specs) == 0 {
		t.Fatalf("no usage entries after %q in %s", anchor, file)
	}
	return specs
}

func TestMcpShimLeavesMatchArgshUsage(t *testing.T) {
	root := repoRootDir(t)
	for name, leaves := range shimLeaves {
		src, ok := shimLeafSources[name]
		if !ok {
			t.Errorf("shimLeaves[%q] has no source in shimLeafSources", name)
			continue
		}
		want := parseShimUsage(t, filepath.Join(root, src.file), src.fn)
		got := map[string]commandSpec{}
		for _, l := range leaves {
			got[l.use] = l
		}
		for use, w := range want {
			g, ok := got[use]
			if !ok {
				t.Errorf("%s: bash subcommand %q missing from shimLeaves", name, use)
				continue
			}
			if g.short != w.short || g.destructive != w.destructive || g.readonly != w.readonly || g.idempotent != w.idempotent {
				t.Errorf("%s %s: drift: bash %+v, go %+v", name, use, w, g)
			}
			if len(w.aliases) > 0 && (len(g.aliases) == 0 || g.aliases[0] != w.aliases[0]) {
				t.Errorf("%s %s: alias drift: bash %v, go %v", name, use, w.aliases, g.aliases)
			}
		}
		for use := range got {
			if _, ok := want[use]; !ok {
				t.Errorf("%s: shimLeaves has %q, bash does not", name, use)
			}
		}
	}
}

func TestMcpShimLeavesAreNotPorted(t *testing.T) {
	for name := range shimLeaves {
		if _, ported := portedCommands[name]; ported {
			t.Errorf("%q is ported to Go: drop its shimLeaves entry, the Go subcommands are the tools now", name)
		}
		found := false
		for _, spec := range commandTree {
			found = found || spec.use == name
		}
		if !found {
			t.Errorf("shimLeaves[%q] is not a command in commandTree", name)
		}
	}
}

// ── editor / env ───────────────────────────────────────────────────────

func TestMcpDefaultEnvCarriesToolchainPathAndBase(t *testing.T) {
	p := &config.Paths{Base: "/proj", Bin: "/proj/.bin", Lok8s: "/proj/.lok8s"}
	t.Setenv("PATH", "/usr/bin")
	env := mcpDefaultEnv(p)
	if env["PATH_BASE"] != "/proj" {
		t.Errorf("PATH_BASE = %q", env["PATH_BASE"])
	}
	if env["PATH"] != "/proj/.bin:/proj/.lok8s:/usr/bin" {
		t.Errorf("PATH = %q", env["PATH"])
	}
}

func TestMcpCommandTree(t *testing.T) {
	root := NewRoot(synthProject(t))
	for _, path := range [][]string{{"mcp"}, {"mcp", "start"}, {"mcp", "serve"}, {"mcp", "tools"}, {"mcp", "claude", "enable"}, {"mcp", "vscode", "enable"}, {"mcp", "cursor", "enable"}} {
		cmd, _, err := root.Find(path)
		if err != nil || cmd.Name() != path[len(path)-1] {
			t.Errorf("%v not resolvable: %v", path, err)
		}
	}
	// `lo mcp tools` end to end through the real root (readonly, hermetic).
	stdout, _, err := runLo(t, root, "mcp", "tools")
	if err != nil {
		t.Fatalf("lo mcp tools: %v", err)
	}
	if !strings.Contains(stdout, "lo_version") || strings.Contains(stdout, "lo_up ") {
		t.Errorf("lo mcp tools output unexpected:\n%s", stdout)
	}
}

// ── stdio smoke test ───────────────────────────────────────────────────

// TestMcpStdioSmoke drives the server the way an editor does: newline-
// delimited JSON-RPC over pipes — initialize, initialized, tools/list.
func TestMcpStdioSmoke(t *testing.T) {
	p := synthProject(t)
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	served := make(chan error, 1)
	go func() {
		served <- mcpRun(ctx, p, mcpExposure{}, &mcp.IOTransport{Reader: inR, Writer: outW}, true, os.Stdout, os.Stderr, "start")
	}()

	send := func(msg string) {
		t.Helper()
		if _, err := inW.Write([]byte(msg + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	reader := bufio.NewReader(outR)
	recv := func() map[string]any {
		t.Helper()
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("reading response: %v", err)
		}
		var msg map[string]any
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("bad JSON-RPC line %q: %v", line, err)
		}
		return msg
	}

	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}`)
	init := recv()
	result, _ := init["result"].(map[string]any)
	info, _ := result["serverInfo"].(map[string]any)
	if info["name"] != "lo" {
		t.Fatalf("initialize: serverInfo.name = %v (full: %v)", info["name"], init)
	}
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	send(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	list := recv()
	result, _ = list["result"].(map[string]any)
	rawTools, _ := result["tools"].([]any)
	names := map[string]bool{}
	for _, rt := range rawTools {
		tool, _ := rt.(map[string]any)
		names[tool["name"].(string)] = true
	}
	if !names["lo_version"] || !names["lo_status"] || names["lo_up"] {
		t.Errorf("tools/list over stdio: got %v", names)
	}

	inW.Close()
	cancel()
	select {
	case <-served:
	case <-time.After(10 * time.Second):
		t.Error("server did not stop after stdin closed + cancel")
	}
	outW.Close()
}
