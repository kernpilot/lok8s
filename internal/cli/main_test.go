package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/render"
)

// toolCallArgvEnv turns this test binary into a probe. A real `lo mcp`
// server in a test (mcpCall) runs a tool call by spawning its own
// executable, which is this binary: with the variable set, it prints the
// argv it got after toolCallArgvMark and exits, so a test reads the exact
// argv and no test runs again in the child.
const (
	toolCallArgvEnv  = "LOK8S_CLI_TEST_TOOL_CALL"
	toolCallArgvMark = "TOOL-CALL-ARGV: "
)

func TestMain(m *testing.M) {
	if os.Getenv(toolCallArgvEnv) == "1" {
		fmt.Println(toolCallArgvMark + strings.Join(os.Args[1:], " "))
		os.Exit(0)
	}
	// lo-full: the plugin home of the in-process render points at THIS
	// binary, as it points at `lo`. When kustomize runs
	// …/chartrenderer/ChartRenderer for an addon's chart (the real render of
	// the doctor's rook-ceph-csi-kms check), the test binary must answer as
	// the plugin, and must not run the tests again. lo core: DispatchPlugin
	// does nothing, and the tests run.
	if handled, rc := render.DispatchPlugin(context.Background(), os.Args, os.Stdin, os.Stdout, os.Stderr); handled {
		os.Exit(rc)
	}
	rc := m.Run()
	render.Cleanup()
	os.Exit(rc)
}
