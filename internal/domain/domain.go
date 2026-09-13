// Package domain is the ONE place domain resolution + driver identity live —
// the Go port of .lok8s/utils/domain.sh, preserving its precedence chain,
// warnings, and error contract exactly.
package domain

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultDomain is the framework's terminal default slot. It lives HERE, at
// the end of the resolution chain — a process-level default would be
// indistinguishable from user-set env and permanently outrank `.active`.
const DefaultDomain = "lok8s.dev"

// NameRe is the domain-name allowlist (also the path-traversal guard).
var NameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// driverRe constrains `.kind` to a bare driver name: the value is used to
// locate `drivers/<kind>/` code, so it is never defaulted when malformed.
var driverRe = regexp.MustCompile(`^[a-z][a-z0-9]*$`)

// Resolution errors for SpecDriver, mirroring the bash rc contract.
var (
	// ErrNoDriver — nothing to read: no file, unreadable file, key absent or
	// empty (bash rc 1).
	ErrNoDriver = errors.New("no driver in spec")
	// ErrMalformedDriver — `.kind` is present but not a bare driver name
	// (bash rc 2). NEVER defaulted: quietly replacing a crafted value with
	// "lo" would hide the crafting.
	ErrMalformedDriver = errors.New("malformed driver in spec")
)

// Resolve resolves the active domain with the canonical precedence:
//
//	explicit value (--domain flag) > DOMAIN_NAME env > clusters/.active
//	  > lok8s.dev
//
// The env var outranks `.active` deliberately (exporting DOMAIN_NAME is a
// more explicit act than persisted state); when both are set and disagree, a
// one-line notice goes to warnTo. Never fails: an unreadable or invalid
// `.active` warns and is ignored.
func Resolve(explicit, clustersDir string, warnTo io.Writer) string {
	if explicit != "" {
		return explicit
	}

	active := ""
	if raw, err := os.ReadFile(filepath.Join(clustersDir, ".active")); err == nil {
		// Trailing newlines only — matching bash's $(cat …) substitution; a
		// leading-whitespace value must reach the validator intact.
		active = strings.TrimRight(string(raw), "\n")
		if active != "" && !NameRe.MatchString(active) {
			fmt.Fprintln(warnTo, "warning: invalid domain in clusters/.active, ignoring")
			active = ""
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(warnTo, "warning: clusters/.active exists but is unreadable, ignoring")
	}

	if envDomain := os.Getenv("DOMAIN_NAME"); envDomain != "" {
		if active != "" && active != envDomain {
			fmt.Fprintf(warnTo, "notice: using DOMAIN_NAME=%s (env); the active domain is '%s' — pass --domain or unset DOMAIN_NAME to switch\n", envDomain, active)
		}
		return envDomain
	}

	if active != "" {
		return active
	}
	return DefaultDomain
}

// SpecDriver is THE reader for "which driver does this cluster spec declare".
// Returns the lowercased `.kind`. A missing/unreadable file or an absent or
// empty key returns fallback when given, else ErrNoDriver. A present but
// non-bare value returns ErrMalformedDriver and is never defaulted.
func SpecDriver(specPath, fallback string) (string, error) {
	var doc struct {
		Kind string `yaml:"kind"`
	}
	raw, err := os.ReadFile(specPath)
	if err == nil {
		err = yaml.Unmarshal(raw, &doc)
	}
	kind := strings.ToLower(doc.Kind)
	if err != nil || kind == "" {
		if fallback != "" {
			return fallback, nil
		}
		return "", ErrNoDriver
	}
	if !driverRe.MatchString(kind) {
		return "", ErrMalformedDriver
	}
	return kind, nil
}

// Driver reports the driver a domain's cluster spec declares (`kind: Lo` →
// "lo"). Deploy-only domains (deploy.lok8s.yaml, no cluster spec) report
// "deploy"; a missing domain dir or unreadable spec returns an error.
func Driver(clustersDir, domain string) (string, error) {
	clusterYAML := filepath.Join(clustersDir, domain, "cluster.lok8s.yaml")
	if _, err := os.Stat(clusterYAML); err != nil {
		if _, err := os.Stat(filepath.Join(clustersDir, domain, "deploy.lok8s.yaml")); err == nil {
			return "deploy", nil
		}
		return "", ErrNoDriver
	}
	return SpecDriver(clusterYAML, "")
}

// RequireDriver gates a driver-specific operation on the resolved domain
// actually using that driver, failing with an actionable message naming the
// mismatch. what names the operation (e.g. "registry management").
func RequireDriver(want, clustersDir, domain, what string, errTo io.Writer) error {
	if what == "" {
		what = "this operation"
	}
	got, err := Driver(clustersDir, domain)
	if err != nil {
		fmt.Fprintf(errTo, "error: domain '%s' has no readable cluster/deploy spec under clusters/ — cannot run %s\n", domain, what)
		return fmt.Errorf("domain %s: no readable spec", domain)
	}
	if got != want {
		fmt.Fprintf(errTo, "error: domain '%s' uses the '%s' driver — %s is a '%s'-driver (local cluster) feature.\n", domain, got, what, want)
		fmt.Fprintf(errTo, "       Pass --domain <a-%s-domain> or switch with 'lo use <domain>'.\n", want)
		return fmt.Errorf("domain %s: driver %s, want %s", domain, got, want)
	}
	return nil
}
