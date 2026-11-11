package toolchain

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kernpilot/lok8s/internal/fsutil"
)

// The fix for a missing argsh, in Go (lo doctor, lo recover) and in the
// bash entrypoint (.lok8s/lo) alike: the same pattern decides, the same
// text says it. TestArgshFixMatchesTheBashEntrypoint pins the entrypoint to
// these values.
const (
	// ArgshListedPattern matches a b.yaml line that installs argsh: a key
	// whose last path element is argsh (`argsh:`,
	// `github.com/arg-sh/argsh:`) or `alias: argsh`. A commented line does
	// not match. grep -E reads it the same way.
	ArgshListedPattern = `^[[:space:]]*(alias:[[:space:]]*argsh[[:space:]]*$|([^#[:space:]][^[:space:]]*/)?argsh:)`
	// ArgshFixInstall is the fix when the b.yaml lists argsh.
	ArgshFixInstall = "b install"
	// ArgshFixAdd is the fix when it does not.
	ArgshFixAdd = "add github.com/arg-sh/argsh to the b.yaml in the same directory, then b install"
)

var argshListedRe = regexp.MustCompile(ArgshListedPattern)

// Argsh reports the argsh runtime of a project: the file every bash child
// sources (`source "${PATH_BIN}/argsh"`: the bash tree, a routed command,
// the provider plugins), present or not, and the fix when it is not. bin is
// the project's .bin. The path is the project's own on purpose: an argsh
// elsewhere on PATH (the main checkout of a git worktree, for example) does
// not count, because no child looks there.
func Argsh(bin string) (path, fix string, ok bool) {
	path = filepath.Join(bin, "argsh")
	ok = fsutil.FileExists(path)
	fix = ArgshFixAdd
	if raw, err := os.ReadFile(filepath.Join(bin, "b.yaml")); err == nil {
		for line := range strings.SplitSeq(string(raw), "\n") {
			if argshListedRe.MatchString(line) {
				fix = ArgshFixInstall
				break
			}
		}
	}
	return path, fix, ok
}
