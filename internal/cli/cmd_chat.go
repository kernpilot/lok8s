package cli

// lo chat — talk to a local AI about your cluster: transparent, streaming,
// read-only. Go port of .lok8s/libs/chat (main::chat).
//
// Thin shim over the `lochat` Go binary (single static binary, `b`-managed).
// It resolves the lo runtime and hands off — the binary reads a JSON config
// and launches `lo mcp` itself. The dynamic bits (the lo path, the project
// dir) are passed as flags; everything else lives in the JSON config. The
// binary parses -p / --model / --posture, so they pass straight through —
// flag parsing is disabled here and the argv reaches lochat verbatim.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/assets"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/ui"
)

func init() { registerPorted("chat", newChatCommand) }

// execProcess replaces the current process (bash: exec). Tests swap it to
// capture the exact argv.
var execProcess = syscall.Exec

func newChatCommand(paths *config.Paths, spec commandSpec) *cobra.Command {
	return &cobra.Command{
		Use:                "chat",
		Aliases:            spec.aliases,
		Short:              spec.short,
		GroupID:            spec.group,
		Annotations:        spec.annotations(),
		DisableFlagParsing: true,
		SilenceUsage:       true,
		RunE: func(cmd *cobra.Command, args []string) error {
			bin, argv, tree, err := chatCommandLine(paths, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			return execProcess(bin, append(argv, args...), shimEnv(paths, tree))
		},
	}
}

// chatCommandLine resolves the lochat binary and its base argv (argv[0] =
// the binary):
//
//	lochat --config <cfg> --lo <bash tree>/lo --cwd <PATH_BASE> --base-dir <PATH_BASE>
//
// --lo is the bash entrypoint (lochat drives `lo mcp`, the argsh builtin)
// from the resolved bash tree, which the caller hands to shimEnv. The
// caller appends the user's arguments. Errors are printed in the bash
// format and returned as ErrHandled.
func chatCommandLine(paths *config.Paths, stderr interface{ Write([]byte) (int, error) }) (string, []string, assets.Tree, error) {
	bin := filepath.Join(paths.Bin, "lochat")
	if !fsutil.IsExecutable(bin) {
		bin = ""
		if p, err := exec.LookPath("lochat"); err == nil {
			bin = p
		}
	}
	if bin == "" {
		ui.ErrorTo(stderr, "lochat binary not found in %s or on PATH.", chatShownDir(paths.Bin))
		ui.ErrorTo(stderr, "To install it, do the set-up steps at https://lok8s.io/guide/lo-chat#set-up")
		return "", nil, assets.Tree{}, ErrHandled
	}

	// config: per-project override, else the shipped defaults.
	cfgProject := os.Getenv("LO_CHAT_CONFIG")
	if cfgProject == "" {
		cfgProject = filepath.Join(paths.Base, "lo-chat.json")
	}
	cfg := cfgProject
	// The project's .lok8s/chat/defaults.json wins; else the embedded
	// defaults (ejected on first use).
	defaults, _, err := assets.Resolve(paths, "chat/defaults.json")
	if err != nil {
		return "", nil, assets.Tree{}, err
	}
	if !fsutil.FileExists(cfg) {
		cfg = defaults
	}
	if !fsutil.FileExists(cfg) {
		ui.ErrorTo(stderr, "no chat config (looked for %s or %s)", cfgProject, defaults)
		return "", nil, assets.Tree{}, ErrHandled
	}

	// Preflight: `lo chat` drives `lo mcp`, and the `mcp` command is an
	// argsh builtin provided by argsh.so. The argsh that the `lo` child
	// sources reads ARGSH_BUILTIN_PATH and ${PATH_LIB}/argsh.so first, then
	// ${PATH_BIN}/argsh.so, then the dirs of LD_LIBRARY_PATH and
	// BASH_LOADABLES_PATH. It never loads argsh.so from beside the argsh
	// binary. If that shared object is missing the bridge dies with
	// "Invalid command: mcp" and the binary would just wait out its
	// timeout — fail loudly and point at the fix.
	if !argshBuiltinPresent(paths) {
		// The error names the checked dir, and the hint installs there.
		// Without --path, argsh installs into a writable exported PATH_BIN,
		// else PATH_LIB, else a BASH_LOADABLES_PATH dir, else
		// ~/.local/lib/bash, and its loader never reads ~/.local/lib/bash.
		// Without --force, argsh writes nothing when it can load an argsh.so
		// that ARGSH_BUILTIN_PATH or PATH_LIB names. The command takes the
		// latest argsh release, not the version of the argsh in the dir. The
		// path in the command is quoted for a POSIX shell. A dir with a
		// control character is printed Go-quoted, with no command line.
		// Bash keeps its old hint (deviation catalogue).
		dir := projectBin(paths)
		ui.ErrorTo(stderr, "argsh.so is missing from %s. 'lo chat' uses 'lo mcp', and 'lo mcp' is an argsh builtin in argsh.so.", chatShownDir(dir))
		if rbacHasControl(dir) {
			ui.ErrorTo(stderr, "The directory name has a control character, so lo does not show the command.")
			ui.ErrorTo(stderr, "Install the latest release of argsh.so in that directory. Then try again.")
		} else {
			ui.ErrorTo(stderr, "To install the latest release of argsh.so in that directory, run this command. Then try again:")
			ui.ErrorTo(stderr, "  argsh builtins install --force --path %s", rbacShQuote(dir))
		}
		return "", nil, assets.Tree{}, ErrHandled
	}
	tree, err := assets.BashTree(paths)
	if err != nil {
		return "", nil, assets.Tree{}, err
	}

	argv := []string{
		bin,
		"--config", cfg,
		"--lo", filepath.Join(tree.Dir, "lo"),
		"--cwd", paths.Base,
		"--base-dir", paths.Base,
	}
	return bin, argv, tree, nil
}

// argshBuiltinPresent checks for argsh.so in the PATH_BIN of the `lo`
// child: projectBin, which shimEnv puts in place of an exported PATH_BIN
// (without an export, the child derives the same dir), not paths.Bin. The
// child sources ${PATH_BIN}/argsh, and that argsh never loads argsh.so
// from beside an argsh on PATH, so the check has no PATH fallback. The
// other places of the argsh search are not checked. Bash reads
// ${PATH_BIN}/argsh.so, else beside the argsh on PATH (deviation
// catalogue: docs/reference/go-migration.md).
func argshBuiltinPresent(paths *config.Paths) bool {
	return fsutil.FileExists(filepath.Join(projectBin(paths), "argsh.so"))
}

// chatShownDir is dir as an error line shows it: as it is, or Go-quoted
// (%q) when it has a control character (rbacHasControl). Such a character
// can end the line or drive the terminal.
func chatShownDir(dir string) string {
	if rbacHasControl(dir) {
		return strconv.Quote(dir)
	}
	return dir
}
