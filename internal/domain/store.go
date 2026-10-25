package domain

import (
	"io"
	"os"
	"sort"
	"strings"

	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/ui"
)

// MissingSecretsStore reports a domain that must not fall back to the flat
// .secrets/ store (bash: domain::check_secrets_store). That is the case when
// the domain directory exists, it has no clusters/<domain>/secrets
// directory, and the project keeps a store per domain: another
// clusters/*/secrets directory exists. It returns that other store (the
// first in byte order) for the message. A project with only the flat store
// (no clusters/*/secrets at all) keeps working: ok is false there.
//
// The silent fallback read and generated values in the wrong store: the
// first build of a new domain directory generated a new sops-age key and new
// tokens into the flat store.
func MissingSecretsStore(clustersDir, domain string) (other string, ok bool) {
	if domain == "" || !fsutil.DirExists(clustersDir+"/"+domain) || fsutil.DirExists(clustersDir+"/"+domain+"/secrets") {
		return "", false
	}
	entries, err := os.ReadDir(clustersDir)
	if err != nil {
		return "", false
	}
	var stores []string
	for _, e := range entries {
		// bash's * glob skips dot-directories.
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if d := clustersDir + "/" + e.Name() + "/secrets"; fsutil.DirExists(d) {
			stores = append(stores, d)
		}
	}
	if len(stores) == 0 {
		return "", false
	}
	sort.Strings(stores)
	return stores[0], true
}

// CheckSecretsStore prints the refusal to errTo and returns ui.ErrHandled
// when MissingSecretsStore reports the domain, else nil. The paths are
// string-joined like the bash message (bash interpolates them verbatim).
func CheckSecretsStore(clustersDir, domain string, errTo io.Writer) error {
	other, ok := MissingSecretsStore(clustersDir, domain)
	if !ok {
		return nil
	}
	store := clustersDir + "/" + domain + "/secrets"
	ui.ErrorTo(errTo, "domain %s has no secrets store: %s does not exist, and this project keeps a store per domain (%s). lo does not use the flat .secrets/ store for %s.", domain, store, other, domain)
	ui.ErrorTo(errTo, "  To generate new values, create the empty directory %s. To use the values of another domain, copy its .enc files into that directory, then run \"lo secrets decrypt --domain %s\".", store, domain)
	return ui.ErrHandled
}
