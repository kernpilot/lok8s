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
	"syscall"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/assets"
	"github.com/kernpilot/lok8s/internal/config"
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
			bin, argv, err := chatCommandLine(paths, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			return execProcess(bin, append(argv, args...), shimEnv(paths))
		},
	}
}

// chatCommandLine resolves the lochat binary and its base argv (argv[0] =
// the binary):
//
//	lochat --config <cfg> --lo <PATH_LOK8S>/lo --cwd <PATH_BASE> --base-dir <PATH_BASE>
//
// The caller appends the user's arguments. Errors are printed in the bash
// format and returned as ErrHandled.
func chatCommandLine(paths *config.Paths, stderr interface{ Write([]byte) (int, error) }) (string, []string, error) {
	bin := filepath.Join(paths.Bin, "lochat")
	if !isExecutable(bin) {
		bin = ""
		if p, err := exec.LookPath("lochat"); err == nil {
			bin = p
		}
	}
	if bin == "" {
		ui.Errorf(stderr, "lochat binary not found. Build it once:")
		ui.Errorf(stderr, `  go build -C ai/lochat -o "${PATH_BIN}/lochat" .`)
		return "", nil, ErrHandled
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
		return "", nil, err
	}
	if !fileExists(cfg) {
		cfg = defaults
	}
	if !fileExists(cfg) {
		ui.Errorf(stderr, "no chat config (looked for %s or %s)", cfgProject, defaults)
		return "", nil, ErrHandled
	}

	// Preflight: `lo chat` drives `lo mcp`, and the `mcp` command is an
	// argsh builtin provided by argsh.so (loaded from beside the argsh
	// binary). If that shared object is missing the bridge dies with
	// "Invalid command: mcp" and the binary would just wait out its
	// timeout — fail loudly and point at the fix.
	if !argshBuiltinPresent(paths) {
		ui.Errorf(stderr, "argsh.so is missing — 'lo mcp' (which 'lo chat' drives) is an argsh builtin from it.")
		ui.Errorf(stderr, "Install the matching builtin next to argsh, then retry:")
		ui.Errorf(stderr, "  argsh builtins install")
		return "", nil, ErrHandled
	}

	argv := []string{
		bin,
		"--config", cfg,
		"--lo", filepath.Join(paths.Lok8s, "lo"),
		"--cwd", paths.Base,
		"--base-dir", paths.Base,
	}
	return bin, argv, nil
}

// argshBuiltinPresent checks for argsh.so next to the toolchain argsh (bash:
// ${PATH_BIN}/argsh.so, else beside the argsh on PATH).
func argshBuiltinPresent(paths *config.Paths) bool {
	if fileExists(filepath.Join(paths.Bin, "argsh.so")) {
		return true
	}
	if p, err := exec.LookPath("argsh"); err == nil {
		return fileExists(filepath.Join(filepath.Dir(p), "argsh.so"))
	}
	return false
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}
