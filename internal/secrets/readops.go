package secrets

// The read-side operations: allow, list, print, env (bash:
// secrets::allow/list/print/env).

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"

	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/ui"
)

const (
	green = "\033[0;32m"
	reset = "\033[0m"
)

// Allow approves bash: generators: it collects the per-entry hashes from
// existing .sha files and writes the approved SET to .bash-allow (bash:
// secrets::allow).
//
// The set is one hash per line, sorted + unique. The Go plugin treats
// .bash-allow as a set and requires every bash entry in a (per-target) build
// to be a member — so a subset build still matches. (Was a single
// hash-of-all-hashes, which a per-target build's subset could never match —
// see kustomize/plugins/secret/generator/bash.go verifyBashAllow.)
func (c *Context) Allow() error {
	secretsDir := c.StorePath()
	if !isDir(secretsDir) {
		ui.Warnf(c.ErrOut, "No secrets directory: %s", secretsDir)
		return nil
	}

	// Collect the per-entry hashes from existing .sha files (*.sha — not just
	// Secret.*.sha).
	var shaFiles, hashes []string
	for _, base := range storeEntries(secretsDir, "") {
		if !strings.HasSuffix(base, ".sha") || !isFile(secretsDir+"/"+base) {
			continue
		}
		raw, err := os.ReadFile(secretsDir + "/" + base)
		if err != nil {
			return err
		}
		shaFiles = append(shaFiles, base)
		hashes = append(hashes, stripSpace(string(raw)))
	}

	if len(hashes) == 0 {
		fmt.Fprintln(c.Out, "No bash: entries found (no .sha files)")
		return nil
	}

	sort.Strings(hashes)
	uniq := hashes[:0]
	for i, h := range hashes {
		if i == 0 || h != hashes[i-1] {
			uniq = append(uniq, h)
		}
	}
	allowFile := secretsDir + "/.bash-allow"
	content := strings.Join(uniq, "\n") + "\n"
	if err := os.WriteFile(allowFile, []byte(content), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(allowFile, 0o600); err != nil {
		return err
	}

	fmt.Fprintf(c.Out, "Approved %d bash: entry/entries\n", len(shaFiles))
	for _, sf := range shaFiles {
		fmt.Fprintf(c.Out, "  %s\n", sf)
	}
	return nil
}

// stripSpace mirrors `tr -d '[:space:]'`.
func stripSpace(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\v', '\f', '\r':
			return -1
		}
		return r
	}, s)
}

// List lists the store's entries with their encryption state (bash:
// secrets::list).
func (c *Context) List() error {
	secretsDir := c.StorePath()
	if !isDir(secretsDir) {
		ui.Warnf(c.ErrOut, "Secrets directory not found: %s", secretsDir)
		return nil
	}
	for _, base := range storeEntries(secretsDir, "Secret.") {
		secret := secretsDir + "/" + base
		if !isFile(secret) || strings.HasSuffix(base, ".enc") {
			continue
		}
		if isFile(secret + ".enc") {
			fmt.Fprintf(c.Out, "%s (encrypted)\n", base)
		} else {
			fmt.Fprintf(c.Out, "%s (plaintext)\n", base)
		}
	}
	// Also list .enc files without a plaintext counterpart (need decrypt)
	for _, base := range storeEntries(secretsDir, "Secret.") {
		if !strings.HasSuffix(base[len("Secret."):], ".enc") {
			continue
		}
		enc := secretsDir + "/" + base
		if !isFile(enc) {
			continue
		}
		if !isFile(strings.TrimSuffix(enc, ".enc")) {
			fmt.Fprintf(c.Out, "%s (needs decrypt)\n", base)
		}
	}
	return nil
}

// Print prints secret(s) whose basenames match every pattern
// (case-insensitive, regex) (bash: secrets::print). copy implies onlyOne.
func (c *Context) Print(patterns []string, onlyOne, copy bool) error {
	if copy {
		onlyOne = true
	}

	secretsDir := c.StorePath()

	var matches []string
	for _, base := range storeEntries(secretsDir, "Secret.") {
		secret := secretsDir + "/" + base
		if !isFile(secret) || strings.HasSuffix(base, ".enc") {
			continue
		}
		matched := true
		for _, p := range patterns {
			// bash: `grep -iqE ".*${p}.*"` — an invalid pattern fails the
			// match, it does not abort.
			re, err := regexp.Compile("(?i).*" + p + ".*")
			if err != nil || !re.MatchString(base) {
				matched = false
				break
			}
		}
		if matched {
			matches = append(matches, secret)
		}
	}

	// Quirk preserved from bash: with onlyOne, ZERO matches also takes this
	// branch and prints an empty "Multiple matches found:" list.
	if onlyOne && len(matches) != 1 {
		ui.Errorf(c.ErrOut, "Multiple matches found:")
		for _, match := range matches {
			fmt.Fprintf(c.Out, "%s%s%s\n", green, match, reset)
		}
		return ErrPrinted
	}

	if len(matches) == 0 {
		ui.Errorf(c.ErrOut, "No matches found")
		return ErrPrinted
	}

	if copy {
		for _, tool := range [][]string{
			{"pbcopy"},
			{"xclip", "-selection", "clipboard"},
			{"xsel", "--clipboard", "--input"},
			{"clip.exe"},
		} {
			path, ok := execx.Look(c.Paths, tool[0])
			if !ok {
				continue
			}
			raw, err := os.ReadFile(matches[0])
			if err != nil {
				return err
			}
			cmd := exec.Command(path, tool[1:]...)
			cmd.Stdin = strings.NewReader(string(raw))
			cmd.Stdout = c.Out
			cmd.Stderr = c.ErrOut
			return cmd.Run()
		}
		ui.Errorf(c.ErrOut, "No clipboard tool found")
		return ErrPrinted
	}

	if len(matches) == 1 {
		raw, err := os.ReadFile(matches[0])
		if err != nil {
			return err
		}
		_, err = c.Out.Write(raw)
		return err
	}

	for _, match := range matches {
		fmt.Fprintf(c.Out, "%s%s%s\n", green, strings.TrimPrefix(match, secretsDir+"/"), reset)
		raw, err := os.ReadFile(match)
		if err != nil {
			return err
		}
		c.Out.Write(raw)
		fmt.Fprint(c.Out, "\n\n")
	}
	return nil
}

// Env emits `export KEY=value` lines for every key of a cached secret, so
// provisioning-time credentials live in the managed (SOPS-encrypted,
// per-domain) store instead of a loose plaintext env file (bash:
// secrets::env). Each cache KEY becomes the exported variable name — name the
// keys after the vars you want (e.g. HCLOUD_TOKEN). Load them with:
// eval "$(lo secrets --domain <domain> env --name hetzner)"
// Read-only; values are shell-quoted with %q so the eval is injection-safe.
func (c *Context) Env(name, namespace string) error {
	if name == "" {
		ui.Errorf(c.ErrOut, "Secret --name is required")
		return ErrPrinted
	}

	secretsDir := c.StorePath()
	prefix := "Secret." + name + "." + namespace + "."
	found := false
	for _, base := range storeEntries(secretsDir, prefix) {
		f := secretsDir + "/" + base
		if !isFile(f) || strings.HasSuffix(base, ".enc") || strings.HasSuffix(base, ".sha") {
			continue
		}
		key := base[len(prefix):]
		raw, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		fmt.Fprintf(c.Out, "export %s=%s\n", key, bashQuote(trimTrailingNewlines(string(raw))))
		found = true
	}
	if !found {
		ui.Errorf(c.ErrOut, "No cached keys for %s/%s in %s", name, namespace, secretsDir)
		return ErrPrinted
	}
	return nil
}
