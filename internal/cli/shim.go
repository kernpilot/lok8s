package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/kernpilot/lok8s/internal/config"
)

// Shim replaces the current process with the argsh implementation
// (`.lok8s/lo`), passing argv through verbatim so both implementations parse
// identically. The environment is prepared the way direnv used to: the
// toolchain and framework directories join PATH, and KUSTOMIZE_PLUGIN_HOME
// gets its default. No PATH_* variable is exported — the bash entrypoint
// derives those from its own location, and exporting defaults on its behalf
// would masquerade as user-set values (see config.Paths.SecretsEnv).
func Shim(p *config.Paths, argv []string) error {
	script := filepath.Join(p.Lok8s, "lo")
	if _, err := os.Stat(script); err != nil {
		return fmt.Errorf("no lok8s project found: %s does not exist (run inside a lok8s project or set PATH_BASE)", script)
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		return fmt.Errorf("bash not found in PATH: %w", err)
	}

	args := append([]string{bash, script}, argv...)
	// #nosec G702 -- LO_IMPL=bash by design: argv reaches the frozen tree
	// untouched as exec arguments; no shell parses it.
	return syscall.Exec(bash, args, shimEnv(p))
}

// shimEnv returns the process environment with p.Bin and p.Lok8s prepended to
// PATH (when missing) and KUSTOMIZE_PLUGIN_HOME defaulted.
func shimEnv(p *config.Paths) []string {
	env := os.Environ()
	path := os.Getenv("PATH")
	for _, dir := range []string{p.Lok8s, p.Bin} {
		if !containsPathEntry(path, dir) {
			path = dir + string(os.PathListSeparator) + path
		}
	}
	env = setEnv(env, "PATH", path)
	if os.Getenv("KUSTOMIZE_PLUGIN_HOME") == "" {
		env = setEnv(env, "KUSTOMIZE_PLUGIN_HOME", filepath.Join(p.Base, ".kustomize"))
	}
	return env
}

func containsPathEntry(path, dir string) bool {
	for _, entry := range strings.Split(path, string(os.PathListSeparator)) {
		if entry == dir {
			return true
		}
	}
	return false
}

func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}
