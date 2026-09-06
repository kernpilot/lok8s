package driver

import (
	"errors"
	"fmt"
	"os/exec"
	"testing"
)

func TestRegistry(t *testing.T) {
	factory := func(deps *Deps) (Driver, error) { return nil, nil }

	Register("testdrv", factory)
	if _, ok := Get("testdrv"); !ok {
		t.Fatal("registered driver not found")
	}
	if _, ok := Get("nope"); ok {
		t.Fatal("unregistered driver found")
	}

	found := false
	for _, n := range Names() {
		if n == "testdrv" {
			found = true
		}
	}
	if !found {
		t.Fatal("Names() misses testdrv")
	}
}

func TestRegisterPanics(t *testing.T) {
	factory := func(deps *Deps) (Driver, error) { return nil, nil }

	mustPanic := func(name string, fn func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Fatalf("%s: expected panic", name)
			}
		}()
		fn()
	}
	// The libs/drivers name allowlist: ^[a-z0-9][a-z0-9_-]*$.
	mustPanic("uppercase", func() { Register("BadName", factory) })
	mustPanic("path-shaped", func() { Register("../evil", factory) })
	mustPanic("nil factory", func() { Register("okname", nil) })
	Register("dupname", factory)
	mustPanic("duplicate", func() { Register("dupname", factory) })
}

func TestExitCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, 0},
		{"declined (gate rc 3)", ErrDeclined, 3},
		{"wrapped declined", fmt.Errorf("gate: %w", ErrDeclined), 3},
		{"full lifecycle (rc 100)", ErrFullLifecycle, 100},
		{"explicit exit error", &ExitError{Code: 7}, 7},
		{"remapped decline keeps code 1", &ExitError{Code: 1, Err: ErrDeclined}, 1},
		{"plain error", errors.New("boom"), 1},
	}
	for _, tc := range cases {
		if got := ExitCode(tc.err); got != tc.want {
			t.Errorf("%s: ExitCode = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestExitCodeSubprocess(t *testing.T) {
	// A subprocess exit code rides through (dispatch_destroy's remap
	// depends on telling a driver-subprocess rc 3 apart from the gate's).
	err := exec.Command("sh", "-c", "exit 3").Run()
	if err == nil {
		t.Skip("sh not available")
	}
	if got := ExitCode(err); got != 3 {
		t.Fatalf("subprocess exit 3: ExitCode = %d, want 3", got)
	}
}
