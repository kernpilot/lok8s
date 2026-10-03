package cli

// cmd_chat_test.go covers the `lo chat` exec argv and its preflight errors.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/assets"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/testutil"
)

func chatProject(t *testing.T) *config.Paths {
	t.Helper()
	p := synthProject(t)
	testutil.WriteFile(t, filepath.Join(p.Bin, "lochat"), "#!/bin/sh\nexit 0\n")
	os.Chmod(filepath.Join(p.Bin, "lochat"), 0o755)
	testutil.WriteFile(t, filepath.Join(p.Bin, "argsh.so"), "")
	testutil.WriteFile(t, filepath.Join(p.Lok8s, "chat", "defaults.json"), "{}\n")
	// The project holds the bash tree: --lo is its entrypoint (without one
	// it would be the cache copy the binary extracts).
	testutil.WriteFile(t, filepath.Join(p.Lok8s, "lo"), "#!/usr/bin/env bash\n")
	return p
}

func TestChatExecArgv(t *testing.T) {
	p := chatProject(t)
	t.Setenv("LO_CHAT_CONFIG", "")
	os.Unsetenv("LO_CHAT_CONFIG")

	var gotBin string
	var gotArgv []string
	saved := execProcess
	execProcess = func(bin string, argv, env []string) error {
		gotBin, gotArgv = bin, argv
		return nil
	}
	defer func() { execProcess = saved }()

	if _, _, err := runLo(t, NewRoot(p), "chat", "-p", "hello", "--model", "m", "--help"); err != nil {
		t.Fatal(err)
	}
	if gotBin != filepath.Join(p.Bin, "lochat") {
		t.Errorf("bin = %q", gotBin)
	}
	want := []string{
		filepath.Join(p.Bin, "lochat"),
		"--config", filepath.Join(p.Lok8s, "chat", "defaults.json"),
		"--lo", filepath.Join(p.Lok8s, "lo"),
		"--cwd", p.Base,
		"--base-dir", p.Base,
		"-p", "hello", "--model", "m", "--help",
	}
	if strings.Join(gotArgv, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("argv = %q\nwant  %q", gotArgv, want)
	}

	// A per-project lo-chat.json wins over the shipped defaults; LO_CHAT_CONFIG
	// overrides the project path.
	testutil.WriteFile(t, filepath.Join(p.Base, "lo-chat.json"), "{}\n")
	runLo(t, NewRoot(p), "chat")
	if gotArgv[2] != filepath.Join(p.Base, "lo-chat.json") {
		t.Errorf("project config not preferred: %q", gotArgv[2])
	}
	custom := filepath.Join(p.Base, "custom.json")
	testutil.WriteFile(t, custom, "{}\n")
	t.Setenv("LO_CHAT_CONFIG", custom)
	runLo(t, NewRoot(p), "chat")
	if gotArgv[2] != custom {
		t.Errorf("LO_CHAT_CONFIG not honoured: %q", gotArgv[2])
	}
}

func TestChatPreflightErrors(t *testing.T) {
	saved := execProcess
	execProcess = func(string, []string, []string) error { t.Fatal("exec reached"); return nil }
	defer func() { execProcess = saved }()
	t.Setenv("PATH", t.TempDir()) // no lochat/argsh on PATH

	p := synthProject(t)
	_, stderr, err := runLo(t, NewRoot(p), "chat")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "lochat binary not found. Build it once:") ||
		!strings.Contains(stderr, `  go build -C ai/lochat -o "${PATH_BIN}/lochat" .`) {
		t.Errorf("missing lochat: err=%v stderr=%q", err, stderr)
	}

	testutil.WriteFile(t, filepath.Join(p.Bin, "lochat"), "#!/bin/sh\n")
	os.Chmod(filepath.Join(p.Bin, "lochat"), 0o755)
	// No project config and no local .lok8s/chat: the embedded defaults are
	// ejected on first use (the "no chat config" error is unreachable now),
	// and the preflight moves on to the argsh.so check.
	_, stderr, err = runLo(t, NewRoot(p), "chat")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "argsh.so is missing") || !strings.Contains(stderr, "  argsh builtins install") {
		t.Errorf("missing argsh.so: err=%v stderr=%q", err, stderr)
	}
	ejected, err := os.ReadFile(filepath.Join(p.Lok8s, "chat", "defaults.json"))
	if err != nil {
		t.Fatalf("chat defaults not ejected: %v", err)
	}
	embedded, _ := fs.ReadFile(assets.FS(), "chat/defaults.json")
	if !bytes.Equal(ejected, embedded) {
		t.Error("ejected defaults.json differs from the embedded copy")
	}
	if _, err := os.Stat(filepath.Join(p.Lok8s, "chat", assets.MarkerFile)); err != nil {
		t.Error("no .lo-origin marker next to the ejected defaults")
	}

	// LO_ASSETS_EJECT=never: a fresh project stays untouched. (chat passes
	// its argv through unparsed, so the env form is the one that reaches
	// it; the --no-eject flag is covered on a flag-parsing command in
	// cmd_assets_test.go.)
	t.Setenv(assets.EnvEject, "never")
	p2 := synthProject(t)
	testutil.WriteFile(t, filepath.Join(p2.Bin, "lochat"), "#!/bin/sh\n")
	os.Chmod(filepath.Join(p2.Bin, "lochat"), 0o755)
	_, _, _ = runLo(t, NewRoot(p2), "chat")
	if _, err := os.Stat(filepath.Join(p2.Lok8s, "chat")); err == nil {
		t.Error("LO_ASSETS_EJECT=never still wrote .lok8s/chat")
	}
}

// TestChatDeniesEveryCredentialOutputCommand: `lo chat` drives the bash tree's
// argsh `lo mcp`, which knows nothing of AnnotationCredentialOutput. The chat
// defaults' deny list is the only gate there, so every such command must be
// on it (in the project tree and in the embedded copy).
func TestChatDeniesEveryCredentialOutputCommand(t *testing.T) {
	var tools []string
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if c.Annotations[AnnotationCredentialOutput] == "true" {
			tools = append(tools, strings.ReplaceAll(c.CommandPath(), " ", "_"))
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(newUsageTree(synthProject(t), routing{}))
	if !slices.Contains(tools, "lo_kubehz_token") {
		t.Fatalf("credential-output commands = %v: lo_kubehz_token lost its annotation, the check proves nothing", tools)
	}
	for _, file := range []string{"../../.lok8s/chat/defaults.json", "../assets/lok8s/chat/defaults.json"} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var defaults struct {
			Injection struct {
				Deny []string `json:"deny"`
			} `json:"injection"`
		}
		if err := json.Unmarshal(raw, &defaults); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		for _, tool := range tools {
			if !slices.Contains(defaults.Injection.Deny, tool) {
				t.Errorf("%s: injection.deny lacks %s, a command whose output is a credential", file, tool)
			}
		}
		// And back: a denied tool that is not annotated would still be an
		// MCP tool of the Go server.
		for _, denied := range defaults.Injection.Deny {
			if !slices.Contains(tools, denied) {
				t.Errorf("%s: %s is denied to chat but not annotated %s", file, denied, AnnotationCredentialOutput)
			}
		}
	}
}
