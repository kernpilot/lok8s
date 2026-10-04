package cli

import (
	"fmt"
	"os"
	"strings"
	"testing"
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
	os.Exit(m.Run())
}
