// Command secret is the kustomize exec generator plugin for
// secrets.lok8s.dev/v1/Secret. It reads a Secret CRD from stdin, runs
// the configured generators (literals, passwd, env, secretRef,
// htpasswd, file, b64), and writes a Kubernetes Secret resource to
// stdout.
//
// Cache directory is read from $PATH_SECRETS. The cache is the source
// of truth for stable output across runs.
//
// Two env knobs shape the run (see plugins/secret/env.go):
//
//   - LOK8S_SECRETS_DISABLE=1|true — store-free OFF switch: emit nothing,
//     never read $PATH_SECRETS or run a generator (makes a render
//     store-free; wired by `lo build --no-secrets`).
//   - LOK8S_SECRETS_OUTPUT=none — run the full pipeline (store reads/mints
//     and validation intact) but suppress the emit write.
//
// See ../../plugins/secret for the plugin assembly and
// ../../pkg/plugin for the runtime contract.
package main

import (
	"fmt"
	"os"

	"github.com/kernpilot/lok8s/kustomize/internal/version"
	"github.com/kernpilot/lok8s/kustomize/pkg/plugin"
	"github.com/kernpilot/lok8s/kustomize/plugins/secret"
)

func main() {
	// `Secret --version` prints the stamped version (ldflags; "dev" for a
	// plain go build) so `lo doctor` can verify the installed plugin against
	// the pin in .bin/b.yaml. Only the exact flag is special: kustomize
	// passes its generator config as argv[1] (a temp file path), never a
	// flag, and the pre-flag builds that fail this call are reported by
	// doctor as "version unknown".
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println(version.Version)
		return
	}
	if err := secret.Run(os.Args, os.Stdin, os.Stdout, plugin.DefaultEnv); err != nil {
		plugin.Fail(err)
	}
}
