package toolchain

import (
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/fsutil"
)

// Argsh reports the argsh runtime of a project: the file every bash child
// sources (`source "${PATH_BIN}/argsh"`: the bash tree, a routed command,
// the provider plugins), present or not, and the command that installs it
// there. bin is the project's .bin. The path is the project's own on
// purpose: an argsh elsewhere on PATH (the main checkout of a git
// worktree, for example) does not count, because no child looks there.
func Argsh(bin string) (path, fix string, ok bool) {
	path = filepath.Join(bin, "argsh")
	ok = fsutil.FileExists(path)
	b := "b install"
	if fsutil.IsExecutable(filepath.Join(bin, "b")) {
		b = ".bin/b install"
	}
	byaml := filepath.Join(bin, "b.yaml")
	switch {
	case listsArgsh(byaml):
		fix = b
	case HasMarker(byaml):
		// The `lo toolchain install` template keeps argsh in the bash
		// group, commented out unless the group was selected.
		fix = "uncomment the bash group in .bin/b.yaml, then " + b
	default:
		fix = "add github.com/arg-sh/argsh to .bin/b.yaml, then " + b
	}
	return path, fix, ok
}

// listsArgsh reports whether a b.yaml installs argsh: a binaries key whose
// last path element is argsh (`argsh`, `github.com/arg-sh/argsh`), or an
// entry with `alias: argsh`.
func listsArgsh(byaml string) bool {
	raw, err := os.ReadFile(byaml)
	if err != nil {
		return false
	}
	var doc struct {
		Binaries map[string]struct {
			Alias string `yaml:"alias"`
		} `yaml:"binaries"`
	}
	if yaml.Unmarshal(raw, &doc) != nil {
		return false
	}
	for key, entry := range doc.Binaries {
		if key == "argsh" || strings.HasSuffix(key, "/argsh") || entry.Alias == "argsh" {
			return true
		}
	}
	return false
}
