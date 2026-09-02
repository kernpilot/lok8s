package kapply

// kapply_test.go — the kapply.Run display contract: off-tty passthrough
// (what CI, Tilt logs and the bats suites observe), tty collapse via the
// KAPPLY_TTY override, error surfacing with (×N) aggregation, and the OK
// verb list.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestRunOffTTYIsPurePassthrough(t *testing.T) {
	t.Setenv("LOK8S_NONINTERACTIVE", "1")
	var out, errOut bytes.Buffer
	err := Run("registries", &out, &errOut, func(o, e io.Writer) error {
		fmt.Fprintln(o, "registry/x created")
		fmt.Fprintln(e, "error: registry/y: boom")
		return errors.New("failed")
	})
	if err == nil {
		t.Fatal("fn error swallowed")
	}
	if out.String() != "registry/x created\n" {
		t.Fatalf("stdout not passed through: %q", out.String())
	}
	if errOut.String() != "error: registry/y: boom\n" {
		t.Fatalf("stderr not passed through: %q", errOut.String())
	}
}

func TestRunTTYCollapsesToSummary(t *testing.T) {
	t.Setenv("KAPPLY_TTY", "1")
	var out, errOut bytes.Buffer
	err := Run("registries", &out, &errOut, func(o, e io.Writer) error {
		fmt.Fprintln(o, "registry/a created")
		fmt.Fprintln(o, "registry/b unchanged")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "registries") || !strings.Contains(out.String(), "2 resources") {
		t.Fatalf("summary wrong: %q", out.String())
	}
	if strings.Contains(out.String(), "registry/a created") {
		t.Fatalf("progress lines not collapsed: %q", out.String())
	}
}

func TestRunTTYSurfacesErrorsDedupedOnFailure(t *testing.T) {
	t.Setenv("KAPPLY_TTY", "1")
	var out, errOut bytes.Buffer
	err := Run("registries", &out, &errOut, func(o, e io.Writer) error {
		fmt.Fprintln(o, "registry/a created")
		fmt.Fprintln(e, "error: webhook not ready")
		fmt.Fprintln(e, "error: webhook not ready")
		return errors.New("failed")
	})
	if err == nil {
		t.Fatal("fn error swallowed")
	}
	if !strings.Contains(errOut.String(), "error: webhook not ready") ||
		!strings.Contains(errOut.String(), "(×2)") {
		t.Fatalf("errors not aggregated: %q", errOut.String())
	}
}

func TestRunTTYNoProgressLinesShownAsIs(t *testing.T) {
	t.Setenv("KAPPLY_TTY", "1")
	var out, errOut bytes.Buffer
	err := Run("phase", &out, &errOut, func(o, e io.Writer) error {
		fmt.Fprintln(o, "warning: nothing to do")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "warning: nothing to do") {
		t.Fatalf("non-progress output hidden: %q", out.String())
	}
}

func TestOKReMatchesTheVerbList(t *testing.T) {
	for _, ok := range []string{
		"configmap/x serverside-applied", "pod/y created", "svc/z configured",
		"cm/a unchanged", "x applied", "y deleted", "svc/s annotated",
		"node/n labeled", "deploy/d patched", "deployment.apps/coredns restarted",
		"deploy/d scaled", "deploy/d rolled back", "crd/c condition met",
	} {
		if !OKRe.MatchString(ok) {
			t.Errorf("OK verb not matched: %q", ok)
		}
	}
	for _, notOK := range []string{"error: x failed", "created", "registry/x creating"} {
		if OKRe.MatchString(notOK) {
			t.Errorf("non-verb matched: %q", notOK)
		}
	}
}
