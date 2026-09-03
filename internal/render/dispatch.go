package render

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/kernpilot/lok8s/kustomize/pkg/plugin"
	"github.com/kernpilot/lok8s/kustomize/plugins/secret"
)

// DispatchPlugin runs this process as a kustomize exec plugin when it was
// started under one of the self-exec plugin names (see pluginhome.go), and
// reports whether it did. main calls it FIRST — before any path
// resolution, before cobra — with os.Args/os.Stdin/os.Stdout/os.Stderr;
// the returned rc is the process exit code. Test binaries call it from
// TestMain the same way, which is what lets the in-process render tests
// exercise the real plugin protocol.
//
// Exec generator protocol (kustomize api/internal/plugins/execplugin): the
// generator config is written to a temp file passed as argv[1] and, when
// short enough, also exported as KUSTOMIZE_PLUGIN_CONFIG_STRING; the cwd
// is the kustomization root; resources go to stdout; a non-zero exit fails
// the build with stderr in the message.
//
//   - Secret → the secrets.lok8s.dev generator (kustomize/plugins/secret),
//     imported as a package: the same Run the standalone kustomize-secret
//     binary's main calls, argv[1] = config path, PATH_SECRETS/
//     LOK8S_SECRETS_* from the environment. Failure prints "secret plugin:
//     <err>" like plugin.Fail.
//   - ChartRenderer → khelm as a library (khelm.go), reproducing the khelm
//     v2.8.0 kustomize-plugin mode. Failure prints "khelm: <err>" like its
//     log.Fatalf.
//
// The match is on the LAST TWO path elements (…/secret/Secret,
// …/chartrenderer/ChartRenderer) so a stray binary merely named Secret
// is not mistaken for a plugin invocation.
func DispatchPlugin(args []string, stdin io.Reader, stdout, stderr io.Writer) (handled bool, rc int) {
	if len(args) == 0 {
		return false, 0
	}
	argv0 := filepath.ToSlash(args[0])
	switch {
	case strings.HasSuffix(argv0, "/secret/Secret"):
		if err := secret.Run(args, stdin, stdout, plugin.DefaultEnv); err != nil {
			fmt.Fprintln(stderr, "secret plugin:", err)
			return true, 1
		}
		return true, 0
	case strings.HasSuffix(argv0, "/chartrenderer/ChartRenderer"):
		if err := runChartRenderer(args, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "khelm: %s\n", err)
			return true, 1
		}
		return true, 0
	}
	return false, 0
}
