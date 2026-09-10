package kubehz

// manifests.go — the vendored in-cluster agent manifests, embedded so `lo`
// renders and applies without the .lok8s tree at hand. The bytes are a
// verbatim copy of .lok8s/libs/kubehz/manifests/** (manifests_test.go pins
// the two trees byte-for-byte, including the UPSTREAM.sha256 digest file
// that proves the RBAC is the public kubehz-agent repo's).

import (
	"embed"
	"io/fs"
)

//go:embed all:manifests
var manifestsFS embed.FS

// Manifests returns the embedded manifest tree rooted at "manifests"
// (agent/, live-agent/base, live-agent/managed, live-agent/UPSTREAM.sha256).
func Manifests() fs.FS {
	sub, err := fs.Sub(manifestsFS, "manifests")
	if err != nil {
		panic(err)
	}
	return sub
}
