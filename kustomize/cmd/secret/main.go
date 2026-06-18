// Command secret is the kustomize exec generator plugin for
// secrets.lok8s.dev/v1/Secret. It reads a Secret CRD from stdin, runs
// the configured generators (literals, passwd, env, secretRef,
// htpasswd, file, b64), and writes a Kubernetes Secret resource to
// stdout.
//
// Cache directory is read from $PATH_SECRETS. The cache is the source
// of truth for stable output across runs.
//
// See ../../plugins/secret for the plugin assembly and
// ../../pkg/plugin for the runtime contract.
package main

import (
	"os"

	"github.com/kernpilot/lok8s/kustomize/pkg/plugin"
	"github.com/kernpilot/lok8s/kustomize/plugins/secret"
)

func main() {
	if err := plugin.Run(os.Args, os.Stdin, os.Stdout, secret.New()); err != nil {
		plugin.Fail(err)
	}
}
