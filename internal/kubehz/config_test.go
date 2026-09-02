package kubehz

// config_test.go ports tests/unit/kubehz_config_test.bats, the spec-knob
// half of kubehz_live_agent_test.bats, and the validate_config cases of
// kubehz_shared_test.bats — over REAL YAML (the bats stubbed yq; the Go
// reader has no yq to stub, so every case is fixture-driven).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readCfg(t *testing.T, h *harness, yaml string) *Config {
	t.Helper()
	p := h.writeSpec("test.kubehz.dev", yaml)
	cfg, err := h.ctx.ReadConfig(p)
	mustOK(t, err, h.output())
	return cfg
}

func TestReadConfigDefaultsWhenBlockAbsent(t *testing.T) {
	h := newHarness(t)
	cfg := readCfg(t, h, "kind: KubeOne\nspec: {}\n")
	if cfg.Hosting != "self" || cfg.Access != "none" || cfg.APIURL != "" {
		t.Fatalf("defaults: %+v", cfg)
	}
	if cfg.UpgradesChannel != "patch" || cfg.UpgradesDefer != "window" || len(cfg.MWExclusions) != 0 {
		t.Fatalf("upgrade defaults: %+v", cfg)
	}
	if cfg.Agent != "cronjob" || cfg.ConnectToken != "false" {
		t.Fatalf("agent/connect defaults: %+v", cfg)
	}
}

func TestReadConfigHostedManaged(t *testing.T) {
	h := newHarness(t)
	cfg := readCfg(t, h, specYAML("KubeOne", "    hosting: hosted\n    access: managed\n    apiUrl: https://api.kubehz.dev\n"))
	if cfg.Hosting != "hosted" || cfg.Access != "managed" || cfg.APIURL != "https://api.kubehz.dev" {
		t.Fatalf("%+v", cfg)
	}
}

func TestReadConfigSelfRegistered(t *testing.T) {
	h := newHarness(t)
	cfg := readCfg(t, h, specYAML("KubeOne", "    access: registered\n    apiUrl: https://api.kubehz.dev\n"))
	if cfg.Hosting != "self" || cfg.Access != "registered" {
		t.Fatalf("%+v", cfg)
	}
}

func TestReadConfigEmptyAccessIsNone(t *testing.T) {
	h := newHarness(t)
	if cfg := readCfg(t, h, specYAML("KubeOne", "    access: \"\"\n")); cfg.Access != "none" {
		t.Fatalf("access = %q", cfg.Access)
	}
	if cfg := readCfg(t, h, specYAML("KubeOne", "    access:\n")); cfg.Access != "none" {
		t.Fatalf("null access = %q", cfg.Access)
	}
}

func TestReadConfigFailsOnMissingSpec(t *testing.T) {
	h := newHarness(t)
	_, err := h.ctx.ReadConfig(filepath.Join(h.base, "does-not-exist.yaml"))
	mustErr(t, err)
	mustContain(t, h.output(), "cannot read cluster spec")
}

func TestReadConfigPropagatesParseFailure(t *testing.T) {
	h := newHarness(t)
	p := h.writeSpec("test.kubehz.dev", "{{ not yaml")
	_, err := h.ctx.ReadConfig(p)
	mustErr(t, err)
	mustNotContain(t, h.output(), "invalid spec.kubehz.hosting")
}

func validate(t *testing.T, cfg *Config, specFile string) (string, error) {
	t.Helper()
	h := newHarness(t)
	err := h.ctx.Validate(cfg, specFile)
	return h.output(), err
}

func TestValidateSelfNonePasses(t *testing.T) {
	out, err := validate(t, &Config{Hosting: "self", Access: "none"}, "")
	mustOK(t, err, out)
}

func TestValidateHostedManagedPasses(t *testing.T) {
	out, err := validate(t, &Config{Hosting: "hosted", Access: "managed", APIURL: "https://api.kubehz.dev"}, "")
	mustOK(t, err, out)
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"hosting", Config{Hosting: "invalid", Access: "none"}, "invalid spec.kubehz.hosting: invalid"},
		{"access", Config{Hosting: "self", Access: "badvalue"}, "invalid spec.kubehz.access: badvalue"},
		{"hosted needs apiUrl", Config{Hosting: "hosted", Access: "none"}, "spec.kubehz.apiUrl is required when hosting: hosted"},
		{"plain http", Config{Hosting: "hosted", Access: "managed", APIURL: "http://api.kubehz.dev"}, "must use HTTPS"},
		{"registered needs apiUrl", Config{Hosting: "self", Access: "registered"}, "spec.kubehz.apiUrl is required when access: registered"},
		{"managed needs apiUrl", Config{Hosting: "self", Access: "managed"}, "spec.kubehz.apiUrl is required when access: managed"},
		{"channel", Config{Hosting: "self", Access: "none", UpgradesChannel: "major"}, "invalid spec.kubehz.upgrades.channel: major (expected none, patch or minor)"},
		{"defer", Config{Hosting: "self", Access: "none", UpgradesDefer: "later"}, "invalid spec.kubehz.upgrades.defer: later"},
		{"exclusion", Config{Hosting: "self", Access: "none", MWExclusions: []string{"2026-12-20/2027-01-06", "christmas week"}}, "invalid spec.kubehz.maintenanceWindow.exclusions entry: christmas week"},
		{"agent enum", Config{Hosting: "self", Access: "registered", APIURL: "https://api.kubehz.dev", Agent: "sidecar"}, "invalid spec.kubehz.agent: sidecar"},
		{"operator needs access", Config{Hosting: "self", Access: "none", Agent: "operator"}, "access: registered or managed"},
		{"operator not shared", Config{Hosting: "shared", Access: "registered", APIURL: "https://x", Agent: "operator"}, "not valid with hosting: shared"},
		{"shared needs apiUrl", Config{Hosting: "shared", Access: "none"}, "apiUrl is required when hosting: shared"},
		{"shared no registered", Config{Hosting: "shared", Access: "registered", APIURL: "https://api.example.test"}, "access: none"},
		{"unknown hosting", Config{Hosting: "communal", Access: "none"}, "invalid spec.kubehz.hosting"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := validate(t, &tc.cfg, "")
			mustErr(t, err)
			mustContain(t, out, tc.want)
		})
	}
}

func TestValidateAccepts(t *testing.T) {
	cases := []Config{
		{Hosting: "self", Access: "none", APIURL: "https://api.kubehz.dev"},
		{Hosting: "self", Access: "none", UpgradesChannel: "patch", UpgradesDefer: "immediate", MWExclusions: []string{"2026-12-20/2027-01-06", "2027-04-03"}},
		{Hosting: "self", Access: "none", UpgradesChannel: "none"},
		{Hosting: "self", Access: "none"}, // unset policy → defaults
		{Hosting: "self", Access: "managed", APIURL: "https://api.kubehz.dev", Agent: "operator"},
	}
	for i, cfg := range cases {
		out, err := validate(t, &cfg, "")
		if err != nil {
			t.Fatalf("case %d: %v\n%s", i, err, out)
		}
	}
}

func TestValidatePerKindRules(t *testing.T) {
	h := newHarness(t)
	lo := h.writeSpec("lo.dev", "kind: Lo\nspec: {}\n")
	err := h.ctx.Validate(&Config{Hosting: "hosted", Access: "none", APIURL: "https://api.kubehz.dev"}, lo)
	mustErr(t, err)
	mustContain(t, h.output(), "hosting: hosted with kind: Lo requires spec.runner configuration")

	h.reset()
	loRunner := h.writeSpec("lo2.dev", "kind: Lo\nspec:\n  runner: hetzner\n")
	mustOK(t, h.ctx.Validate(&Config{Hosting: "hosted", Access: "managed", APIURL: "https://api.kubehz.dev"}, loRunner), h.output())

	kubeone := h.writeSpec("k1.dev", "kind: KubeOne\nspec: {}\n")
	mustOK(t, h.ctx.Validate(&Config{Hosting: "hosted", Access: "managed", APIURL: "https://api.kubehz.dev"}, kubeone), h.output())

	kubehz := h.writeSpec("sp.dev", "kind: Kubehz\nspec: {}\n")
	mustOK(t, h.ctx.Validate(&Config{Hosting: "shared", Access: "none", APIURL: "https://api.example.test"}, kubehz), h.output())

	h.reset()
	mustErr(t, h.ctx.Validate(&Config{Hosting: "shared", Access: "none", APIURL: "https://api.example.test"}, kubeone))
	mustContain(t, h.output(), "requires kind: Kubehz")

	h.reset()
	mustErr(t, h.ctx.Validate(&Config{Hosting: "self", Access: "none"}, kubehz))
	mustContain(t, h.output(), "kind: Kubehz requires spec.kubehz.hosting: shared")
}

func TestReadConfigUpgradesExplicit(t *testing.T) {
	h := newHarness(t)
	cfg := readCfg(t, h, specYAML("KubeOne", "    upgrades:\n      channel: minor\n      defer: immediate\n    maintenanceWindow:\n      exclusions: [\"2026-12-20/2027-01-06\", \"2027-04-03\"]\n"))
	if cfg.UpgradesChannel != "minor" || cfg.UpgradesDefer != "immediate" {
		t.Fatalf("%+v", cfg)
	}
	if strings.Join(cfg.MWExclusions, "\n") != "2026-12-20/2027-01-06\n2027-04-03" {
		t.Fatalf("exclusions %q", cfg.MWExclusions)
	}
}

func TestReadConfigExclusionShapes(t *testing.T) {
	h := newHarness(t)
	f := filepath.Join(h.base, "clusters", "test.kubehz.dev", "cluster.lok8s.yaml")
	cfg := readCfg(t, h, "spec:\n  kubehz:\n    maintenanceWindow:\n      exclusions: [\"2026-01-01\", \"2026-02-01/2026-02-03\"]\n")
	if len(cfg.MWExclusions) != 2 {
		t.Fatalf("list: %v", cfg.MWExclusions)
	}
	cfg = readCfg(t, h, "spec:\n  kubehz:\n    maintenanceWindow:\n      exclusions: \"2026-01-01\"\n")
	if strings.Join(cfg.MWExclusions, "\n") != "2026-01-01" {
		t.Fatalf("scalar: %v", cfg.MWExclusions)
	}
	cfg = readCfg(t, h, "spec: {}\n")
	if len(cfg.MWExclusions) != 0 || cfg.UpgradesChannel != "patch" {
		t.Fatalf("absent: %+v", cfg)
	}
	// Scalar-coerced INVALID content fails with OUR message.
	cfg = readCfg(t, h, "spec:\n  kubehz:\n    maintenanceWindow:\n      exclusions: \"not-a-date\"\n")
	h.reset()
	mustErr(t, h.ctx.Validate(cfg, f))
	mustContain(t, h.output(), "maintenanceWindow.exclusions entry: not-a-date")
	_ = os.Remove(f)
}

func TestReadConfigAgentKnob(t *testing.T) {
	h := newHarness(t)
	if cfg := readCfg(t, h, specYAML("KubeOne", "    access: registered\n    apiUrl: https://api.kubehz.dev\n")); cfg.Agent != "cronjob" {
		t.Fatalf("default agent %q", cfg.Agent)
	}
	cfg := readCfg(t, h, specYAML("KubeOne", "    access: managed\n    apiUrl: https://api.kubehz.dev\n    agent: operator\n"))
	if cfg.Agent != "operator" {
		t.Fatalf("agent %q", cfg.Agent)
	}
	mustOK(t, h.ctx.Validate(cfg, ""), h.output())
	for _, blank := range []string{"    agent:\n", "    agent: \"\"\n"} {
		if cfg := readCfg(t, h, specYAML("KubeOne", "    access: registered\n    apiUrl: https://api.kubehz.dev\n"+blank)); cfg.Agent != "cronjob" {
			t.Fatalf("blank agent → %q", cfg.Agent)
		}
	}
}
