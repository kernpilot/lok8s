package generator

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kernpilot/lok8s/kustomize/pkg/errs"
	"github.com/kernpilot/lok8s/kustomize/pkg/plugin"

	specpkg "github.com/kernpilot/lok8s/kustomize/plugins/secret/spec"
)

// Bash is the generator for the `bash:` field. It runs shell commands
// or scripts and caches the output like passwd: — the command runs
// once, the result is stored in $PATH_SECRETS, and subsequent builds
// reuse the cached value.
type Bash struct {
	spec map[string]specpkg.BashEntry
}

// NewBash wraps a bash map.
func NewBash(spec map[string]specpkg.BashEntry) *Bash { return &Bash{spec: spec} }

// Name returns the generator's spec field name.
func (g *Bash) Name() string { return "bash" }

// Generate runs each bash entry, caching the output.
//
// Security: each bash: entry is SHA256-hashed and compared against a
// committed .sha file. If no .sha file exists (first run), it's
// created automatically. If the hash mismatches (command changed),
// the generator fails with a clear error asking the user to review
// and re-approve via `lo secrets allow`.
//
// Additionally, a local .bash-allow file (not committed) must list the
// approved hash of every bash: entry in the build (one per line) — the
// "direnv allow" moment. Without it, bash: entries refuse to execute.
// Written by `lo secrets allow`.
func (g *Bash) Generate(ctx *plugin.Context) ([]plugin.Entry, error) {
	if len(g.spec) == 0 {
		return nil, nil
	}
	if ctx.Cache == nil {
		return nil, errs.New("bash generator requires PATH_SECRETS to be set")
	}

	keys := make([]string, 0, len(g.spec))
	for k := range g.spec {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Layer 1: verify per-entry .sha files.
	// Collect all entry hashes for the layer-2 check.
	var allHashes []string
	for _, k := range keys {
		entry := g.spec[k]
		entryHash := hashEntry(entry)
		shaFile := ctx.Cache.Dir() + "/" + shaFileName(ctx.Name, ctx.Namespace, k)

		existing, err := os.ReadFile(shaFile)
		if err == nil {
			// .sha exists — verify it matches.
			if strings.TrimSpace(string(existing)) != entryHash {
				return nil, errs.Newf("bash entry %q has changed (hash mismatch)\n"+
					"  expected: %s\n"+
					"  got:      %s\n"+
					"Review the change and run: lo secrets allow",
					k, strings.TrimSpace(string(existing)), entryHash)
			}
		} else if os.IsNotExist(err) {
			// First run — create the .sha file.
			if err := os.WriteFile(shaFile, []byte(entryHash+"\n"), 0644); err != nil {
				return nil, errs.Newf("bash %s: failed to write .sha file: %v", k, err)
			}
		} else {
			return nil, errs.Newf("bash %s: failed to read .sha file: %v", k, err)
		}
		allHashes = append(allHashes, entryHash)
	}

	// Layer 2: verify local .bash-allow (hash-of-hashes).
	if err := verifyBashAllow(ctx.Cache.Dir(), allHashes); err != nil {
		return nil, err
	}

	// All checks passed — execute and cache.
	out := make([]plugin.Entry, 0, len(keys))
	for _, k := range keys {
		entry := g.spec[k]
		var val []byte
		var err error
		if entry.Update {
			// Bypass cache: re-run every build and overwrite the cached value, so
			// cluster-bound secrets (e.g. an in-cluster kubeconfig embedding the
			// current cluster CA) track the live cluster instead of authenticating
			// against a recreated one. Cache.Put keeps secretRef consumers current.
			if val, err = runBash(entry, ctx); err == nil {
				err = ctx.Cache.Put(k, val)
			}
		} else {
			val, err = ctx.Cache.GetOrCreate(k, func() ([]byte, error) {
				return runBash(entry, ctx)
			})
		}
		if err != nil {
			return nil, errs.Wrap(k, err)
		}
		out = append(out, plugin.Entry{Key: k, Value: val})
	}
	return out, nil
}

// hashEntry computes the SHA256 of the bash entry's command content.
// For exec: entries, hashes the command string.
// For file: entries, hashes the file path (not contents — the file
// may not exist yet or may change independently).
func hashEntry(entry specpkg.BashEntry) string {
	var content string
	if entry.Exec != "" {
		content = "exec:" + entry.Exec
	} else {
		content = "file:" + entry.File
	}
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}

// shaFileName returns the .sha filename for a bash entry.
func shaFileName(secret, namespace, key string) string {
	return fmt.Sprintf("Secret.%s.%s.%s.sha", secret, namespace, key)
}

// verifyBashAllow checks that the local .bash-allow file approves every bash
// entry in THIS build. The file is the approved SET of per-entry hashes (one
// hex SHA256 per line) — the "direnv allow" equivalent, (re)written by
// `lo secrets allow`. Each developer must run it after cloning, or after a
// bash: entry is added or changed.
//
// Why a SET and not a single hash-of-all-hashes: builds run per-target, so any
// one build sees only that target's bash entries. A hash of the whole set could
// never match a single target's subset — every per-target build of a
// bash-carrying target would fail. Membership of each entry's hash in the
// approved set works for both per-target and full builds, while still forcing
// re-approval whenever a NEW or CHANGED command appears (its hash is absent from
// the set until approved).
func verifyBashAllow(cacheDir string, entryHashes []string) error {
	allowFile := filepath.Join(cacheDir, ".bash-allow")

	data, err := os.ReadFile(allowFile)
	if os.IsNotExist(err) {
		// First run — auto-create with this build's hashes (same
		// trust-on-first-use as the per-entry .sha files). `lo secrets allow`
		// later writes the union across all targets.
		return writeBashAllow(allowFile, entryHashes)
	} else if err != nil {
		return fmt.Errorf("failed to read .bash-allow: %w", err)
	}

	approved := make(map[string]struct{})
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			approved[line] = struct{}{}
		}
	}

	for _, h := range entryHashes {
		if _, ok := approved[h]; !ok {
			return fmt.Errorf("bash: entries have changed since last approval\n\n" +
				"  One or more bash: generators are not in your local approval set.\n" +
				"  Review the changes and run: lo secrets allow\n\n" +
				"  This is a security measure — bash: entries execute shell commands\n" +
				"  at build time. See docs/guide/addons.md for details.")
		}
	}

	return nil
}

// writeBashAllow writes the approved set (sorted, de-duplicated, one hex hash
// per line) to the .bash-allow file with owner-only permissions.
func writeBashAllow(allowFile string, entryHashes []string) error {
	set := make(map[string]struct{}, len(entryHashes))
	for _, h := range entryHashes {
		set[h] = struct{}{}
	}
	uniq := make([]string, 0, len(set))
	for h := range set {
		uniq = append(uniq, h)
	}
	sort.Strings(uniq)
	if err := os.WriteFile(allowFile, []byte(strings.Join(uniq, "\n")+"\n"), 0600); err != nil {
		return fmt.Errorf("failed to write .bash-allow: %w", err)
	}
	return nil
}

// runBash executes a bash entry and returns the captured output.
func runBash(entry specpkg.BashEntry, ctx *plugin.Context) ([]byte, error) {
	var cmd *exec.Cmd

	if entry.Exec != "" {
		cmd = exec.Command("bash", "-c", entry.Exec)
	} else if entry.File != "" {
		// Resolve env vars in the file path (e.g. ${PATH_BASE}).
		path := expandEnv(entry.File, ctx.Env)
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("script file not found: %s", path)
		}
		cmd = exec.Command("bash", path)
	} else {
		return nil, fmt.Errorf("bash entry must have exec or file")
	}

	// Set working directory to fileRoot (kustomization dir) if available.
	if ctx.FileRoot != "" {
		cmd.Dir = ctx.FileRoot
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// Include stderr in the error for debugging.
		msg := fmt.Sprintf("bash command failed: %v", err)
		if stderr.Len() > 0 {
			msg += "\nstderr: " + strings.TrimSpace(stderr.String())
		}
		return nil, fmt.Errorf("%s", msg)
	}

	var result []byte
	switch entry.EffectiveOutput() {
	case "stdout":
		result = stdout.Bytes()
	case "stderr":
		result = stderr.Bytes()
	case "combined":
		result = append(stdout.Bytes(), stderr.Bytes()...)
	}

	// Encode BEFORE newline handling so binary output is preserved exactly.
	// Newline `strip`/`ensure` trim trailing whitespace-VALUED bytes; doing that
	// to raw bytes before encoding silently corrupts binary keys whose last byte
	// happens to be 0x09/0x0a/0x0d/0x20 (e.g. `openssl rand`). After encoding,
	// the value is text (base64/hex) and newline handling is safe.
	switch entry.EffectiveEncode() {
	case "base64":
		encoded := make([]byte, base64.StdEncoding.EncodedLen(len(result)))
		base64.StdEncoding.Encode(encoded, result)
		result = encoded
	case "hex":
		result = []byte(hex.EncodeToString(result))
	case "":
		// no-op
	}

	// Apply newline handling to the final (encoded or raw-text) value. strip and
	// ensure act on the trailing LINE TERMINATOR only (\r/\n) — not spaces or
	// tabs — so "strip the newline" never silently eats meaningful trailing
	// bytes of the value.
	switch entry.EffectiveNewline() {
	case "strip":
		result = bytes.TrimRight(result, "\r\n")
	case "ensure":
		result = bytes.TrimRight(result, "\r\n")
		result = append(result, '\n')
	case "keep":
		// no-op
	}

	if len(result) == 0 {
		return nil, fmt.Errorf("bash command produced empty output")
	}

	return result, nil
}

// expandEnv resolves ${VAR} references using the plugin's env lookup.
func expandEnv(s string, env func(string) (string, bool)) string {
	return os.Expand(s, func(key string) string {
		if v, ok := env(key); ok {
			return v
		}
		return ""
	})
}
