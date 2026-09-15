package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/kernpilot/lok8s/internal/assets"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
)

// shimExec is the exec seam of the shim (syscall.Exec). Tests swap in a
// recorder that captures the binary, argv and environment.
var shimExec = syscall.Exec

// Shim replaces the current process with the argsh implementation (`lo`
// in the bash tree), passing argv through verbatim so both implementations
// parse identically. The tree is a checkout or ejected tree when the
// project has one, else the copy embedded in the binary, extracted once
// into the versioned cache (assets.BashTree). This is the `lo drivers
// <name>` fallback for a driver without a Go twin; a routed command
// (routing.go) goes through execShim with the project's tree instead and
// never reaches the cache.
func Shim(p *config.Paths, argv []string) error {
	tree, err := assets.BashTree(p)
	if err != nil {
		return err
	}
	return execShim(p, tree, argv)
}

// execShim replaces the process with `bash <tree>/lo <argv...>`. The
// environment is prepared the way direnv used to: the toolchain and
// framework directories join PATH, and KUSTOMIZE_PLUGIN_HOME gets its
// default. With a local tree beside the project root no PATH_* variable
// is exported — the bash entrypoint derives those from its own location,
// and exporting defaults on its behalf would masquerade as user-set
// values (see config.Paths.SecretsEnv). A cached tree, or a project tree
// below a subdirectory, cannot derive the project from its location, so
// PATH_BASE and PATH_LOK8S are set for it (shimEnv).
func execShim(p *config.Paths, tree assets.Tree, argv []string) error {
	bash, err := exec.LookPath("bash")
	if err != nil {
		return fmt.Errorf("bash not found in PATH: %w", err)
	}

	args := append([]string{bash, filepath.Join(tree.Dir, "lo")}, argv...)
	// #nosec G702 -- by design: argv reaches the frozen tree untouched as
	// exec arguments; no shell parses it.
	return shimExec(bash, args, shimEnv(p, tree))
}

// shimEnv returns the process environment with the project's .bin and the
// bash tree prepended to PATH (when missing) and KUSTOMIZE_PLUGIN_HOME
// defaulted. PATH_LOK8S is set when the tree is not p.Lok8s (the cache, a
// routed tree under another name, or the project's .lok8s while
// PATH_LOK8S points elsewhere) and PATH_BASE when the tree's parent is
// not the project root (cache, temp dir, a routed tree below a
// subdirectory): the entrypoint derives PATH_BASE as the parent of its
// own directory, which would be wrong there. An inherited PATH_BIN is
// replaced by <project>/.bin: the entrypoint sources `${PATH_BIN}/argsh`,
// so an ambient value would select the runtime the bash tree runs on.
func shimEnv(p *config.Paths, tree assets.Tree) []string {
	env := os.Environ()
	env = setEnv(env, "PATH", childPATH(p, tree.Dir))
	env = setEnv(env, config.KustomizePluginHomeEnv, config.KustomizePluginHome(p))
	if v := os.Getenv("PATH_BIN"); v != "" && v != projectBin(p) {
		env = setEnv(env, "PATH_BIN", projectBin(p))
	}
	if tree.Dir != "" && tree.Dir != p.Lok8s {
		env = setEnv(env, "PATH_LOK8S", tree.Dir)
	}
	if !tree.Source.Local() || filepath.Dir(tree.Dir) != p.Base {
		env = setEnv(env, "PATH_BASE", p.Base)
	}
	return env
}

// bashTreeForPATH is the tree dir the binary puts on its children's PATH
// without extracting anything: a local tree or an extracted cache
// (FindBashTree), else p.Lok8s (the legacy PATH entry; a missing dir on
// PATH is harmless).
func bashTreeForPATH(p *config.Paths) assets.Tree {
	tree := assets.FindBashTree(p)
	if tree.Source == assets.TreeNone {
		tree.Dir = p.Lok8s
	}
	return tree
}

// childPATH is the PATH the binary prepares for its children: the
// project's .bin first, then the bash tree, then the process PATH
// (execx.PrependPATH). The project's .bin, not p.Bin: an inherited
// PATH_BIN must not put another toolchain (and argsh) ahead for a bash
// child. The binary's own tool lookups keep honouring p.Bin.
func childPATH(p *config.Paths, treeDir string) string {
	return execx.PrependPATH(projectBin(p), treeDir)
}

// projectBin is <project>/.bin, derived from the project root only.
func projectBin(p *config.Paths) string {
	return filepath.Join(p.Base, ".bin")
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
