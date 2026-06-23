package main

import (
	"strings"
	"testing"
)

func TestFlatten(t *testing.T) {
	out := flatten([]Msg{
		{Role: "system", Content: "SYS"},
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "yo"},
	})
	if !strings.Contains(out, "SYS") {
		t.Error("flatten dropped the system content")
	}
	if !strings.Contains(out, "User: hi") {
		t.Errorf("flatten missing user line:\n%s", out)
	}
	if !strings.Contains(out, "Assistant: yo") {
		t.Errorf("flatten missing assistant line:\n%s", out)
	}
}

func TestMakeBackendType(t *testing.T) {
	cli := makeBackend("c", &BackendConfig{Type: "cli", Command: []string{"echo"}})
	if cli.Agentic() {
		t.Error("cli backend must not be agentic (it's a handoff)")
	}
	http := makeBackend("h", &BackendConfig{Model: "m"})
	if !http.Agentic() {
		t.Error("http backend must be agentic")
	}
}
