package kubehz

// config.go — kubehz::read_config + kubehz::validate_config: the spec.kubehz
// block of cluster.lok8s.yaml (the two-axis hosting × access model, the
// agent choice, the declarative upgrade policy).

import (
	"regexp"
	"strings"

	"github.com/kernpilot/lok8s/internal/domain"
)

// Config is the LOK8S_KUBEHZ_* export set of kubehz::read_config. Field
// names carry the bash variable each mirrors.
type Config struct {
	// Hosting is LOK8S_KUBEHZ_HOSTING: self | hosted | shared (default self).
	Hosting string
	// Access is LOK8S_KUBEHZ_ACCESS: none | registered | managed (default none).
	Access string
	// APIURL is LOK8S_KUBEHZ_API_URL (spec.kubehz.apiUrl, "" when unset).
	APIURL string
	// ConnectToken is LOK8S_KUBEHZ_CONNECT_TOKEN: the raw
	// spec.kubehz.connectHcloudToken scalar ("true" opts in; default "false").
	ConnectToken string
	// Agent is LOK8S_KUBEHZ_AGENT: cronjob | operator (default cronjob).
	Agent string
	// UpgradesChannel is LOK8S_KUBEHZ_UPGRADES_CHANNEL: none | patch | minor
	// (default patch).
	UpgradesChannel string
	// UpgradesDefer is LOK8S_KUBEHZ_UPGRADES_DEFER: window | immediate
	// (default window).
	UpgradesDefer string
	// MWExclusions is LOK8S_KUBEHZ_MW_EXCLUSIONS, one entry per line.
	MWExclusions []string
}

// ReadConfig ports kubehz::read_config. A missing/unreadable spec fails
// loudly ("cannot read cluster spec") — the capi/kubeone drivers guard
// this call, and a guard over a function that could never fail was a
// no-op that sent a driver down the wrong branch on a missing file.
func (c *Context) ReadConfig(clusterYAML string) (*Config, error) {
	if !fileExists(clusterYAML) {
		if clusterYAML == "" {
			clusterYAML = "<empty path>"
		}
		c.errorf("cannot read cluster spec: %s", clusterYAML)
		return nil, ErrHandled
	}
	doc := loadSpec(clusterYAML)
	if doc.err != nil {
		// bash: yq's own parse error reaches stderr, then `|| return 1`.
		c.errorf("cannot parse cluster spec: %s: %v", clusterYAML, doc.err)
		return nil, ErrHandled
	}

	cfg := &Config{
		Hosting: doc.or("self", "spec", "kubehz", "hosting"),
		APIURL:  doc.or("", "spec", "kubehz", "apiUrl"),
	}

	// yq boolean/null handling: a missing key reads "null" and an empty
	// scalar "", both meaning "not chosen".
	access := doc.raw("spec", "kubehz", "access")
	if access == "null" || access == "" {
		cfg.Access = "none"
	} else {
		cfg.Access = access
	}

	cfg.ConnectToken = doc.or("false", "spec", "kubehz", "connectHcloudToken")

	cfg.Agent = doc.or("cronjob", "spec", "kubehz", "agent")
	if cfg.Agent == "null" || cfg.Agent == "" {
		cfg.Agent = "cronjob"
	}

	cfg.UpgradesChannel = doc.or("patch", "spec", "kubehz", "upgrades", "channel")
	if cfg.UpgradesChannel == "null" || cfg.UpgradesChannel == "" {
		cfg.UpgradesChannel = "patch"
	}
	cfg.UpgradesDefer = doc.or("window", "spec", "kubehz", "upgrades", "defer")
	if cfg.UpgradesDefer == "null" || cfg.UpgradesDefer == "" {
		cfg.UpgradesDefer = "window"
	}

	// Maintenance-window freeze entries: a scalar is coerced to a
	// single-entry list so validate rejects the CONTENT with our message.
	exclusions := doc.seqOrScalar("spec", "kubehz", "maintenanceWindow", "exclusions")
	if strings.Join(exclusions, "\n") != "null" {
		cfg.MWExclusions = exclusions
	}
	return cfg, nil
}

var mwDateRe = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}(/[0-9]{4}-[0-9]{2}-[0-9]{2})?$`)

// Validate ports kubehz::validate_config. specFile is the cluster spec the
// per-kind hosting rules read (bash: LOK8S_SPEC_FILE); "" skips them the way
// an unreadable file did (kind = "").
//
// Reachable without ReadConfig (callers that set the fields themselves): an
// unset agent / upgrade policy means "not chosen", never "invalid".
func (c *Context) Validate(cfg *Config, specFile string) error {
	switch cfg.Hosting {
	case "self", "hosted", "shared":
	default:
		c.errorf("invalid spec.kubehz.hosting: %s", cfg.Hosting)
		return ErrHandled
	}
	switch cfg.Access {
	case "none", "registered", "managed":
	default:
		c.errorf("invalid spec.kubehz.access: %s", cfg.Access)
		return ErrHandled
	}
	// One value, so a spec can never ask for two heartbeat producers (the
	// first of three interlocks; the other two live in deploy.go and in the
	// CronJob itself).
	agent := cfg.Agent
	if agent == "" {
		agent = "cronjob"
	}
	switch agent {
	case "cronjob", "operator":
	default:
		c.errorf("invalid spec.kubehz.agent: %s (expected cronjob or operator)", agent)
		return ErrHandled
	}
	channel := cfg.UpgradesChannel
	if channel == "" {
		channel = "patch"
	}
	switch channel {
	case "none", "patch", "minor":
	default:
		c.errorf("invalid spec.kubehz.upgrades.channel: %s (expected none, patch or minor)", channel)
		return ErrHandled
	}
	deferPolicy := cfg.UpgradesDefer
	if deferPolicy == "" {
		deferPolicy = "window"
	}
	switch deferPolicy {
	case "window", "immediate":
	default:
		c.errorf("invalid spec.kubehz.upgrades.defer: %s (expected window or immediate)", deferPolicy)
		return ErrHandled
	}
	// Shape only — a date or an inclusive from/to range.
	for _, exclusion := range cfg.MWExclusions {
		if exclusion == "" {
			continue
		}
		if !mwDateRe.MatchString(exclusion) {
			c.errorf("invalid spec.kubehz.maintenanceWindow.exclusions entry: %s (expected YYYY-MM-DD or YYYY-MM-DD/YYYY-MM-DD)", exclusion)
			return ErrHandled
		}
	}
	if cfg.Access == "none" && agent != "cronjob" {
		c.errorf("spec.kubehz.agent: %s needs spec.kubehz.access: registered or managed (access: none deploys no agent)", agent)
		return ErrHandled
	}
	if cfg.Hosting == "shared" && agent != "cronjob" {
		c.errorf("spec.kubehz.agent: %s is not valid with hosting: shared (a Space has no in-cluster agent)", agent)
		return ErrHandled
	}

	if cfg.Hosting == "hosted" && cfg.APIURL == "" {
		c.errorf("spec.kubehz.apiUrl is required when hosting: hosted")
		return ErrHandled
	}
	if cfg.Hosting == "shared" && cfg.APIURL == "" {
		c.errorf("spec.kubehz.apiUrl is required when hosting: shared")
		return ErrHandled
	}
	if cfg.Hosting == "shared" && cfg.Access == "registered" {
		c.errorf("spec.kubehz.access: registered is not valid with hosting: shared (a Space has no agent; use access: none)")
		return ErrHandled
	}
	if cfg.Access != "none" && cfg.APIURL == "" {
		c.errorf("spec.kubehz.apiUrl is required when access: %s", cfg.Access)
		return ErrHandled
	}
	// Bearer tokens travel on this URL — never allow plaintext transport.
	if cfg.APIURL != "" {
		if err := c.requireHTTPS(cfg.APIURL, "spec.kubehz.apiUrl"); err != nil {
			return err
		}
	}

	// Per-kind hosting constraints.
	kind := ""
	if specFile != "" {
		kind, _ = domain.SpecDriver(specFile, "")
	}
	if cfg.Hosting == "hosted" && kind == "lo" {
		if loadSpec(specFile).or("", "spec", "runner") == "" {
			c.errorf("hosting: hosted with kind: Lo requires spec.runner configuration")
			return ErrHandled
		}
	}
	// The hosting axis is one vocabulary, but shared is its own driver.
	if cfg.Hosting == "shared" && kind != "" && kind != "kubehz" {
		c.errorf("hosting: shared requires kind: Kubehz (the space driver); got kind: %s", kind)
		return ErrHandled
	}
	if kind == "kubehz" && cfg.Hosting != "shared" {
		c.errorf("kind: Kubehz requires spec.kubehz.hosting: shared (got: %s)", cfg.Hosting)
		return ErrHandled
	}
	return nil
}
