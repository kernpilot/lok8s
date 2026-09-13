package secrets

// secrets_test.go covers the store resolution and the two plaintext
// checks.

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kernpilot/lok8s/internal/fsutil"
)

func TestStorePathResolution(t *testing.T) {
	c, _, _ := testEnv(t)
	flat := c.Paths.Base + "/.secrets"

	// No domain → flat store.
	if got := c.StorePath(); got != flat {
		t.Errorf("flat: got %s want %s", got, flat)
	}
	// Domain without its own store dir → still the flat store (no fallback
	// TIER, but the flat store IS the resolution when no per-domain dir
	// exists).
	c.Domain = "a.dev"
	if got := c.StorePath(); got != flat {
		t.Errorf("domain w/o store: got %s want %s", got, flat)
	}
	// Domain with its own store → that store exclusively.
	own := c.Paths.Clusters + "/a.dev/secrets"
	os.MkdirAll(own, 0o755)
	if got := c.StorePath(); got != own {
		t.Errorf("domain store: got %s want %s", got, own)
	}
	// PATH_SECRETS overrides the flat default.
	c.Domain = ""
	c.Paths.SecretsEnv = c.Paths.Base + "/custom"
	if got := c.StorePath(); got != c.Paths.Base+"/custom" {
		t.Errorf("PATH_SECRETS: got %s", got)
	}
}

func TestEnsureStoreScaffoldsOnlyExistingDomains(t *testing.T) {
	c, _, _ := testEnv(t)
	c.Domain = "a.dev"
	// Domain dir absent → no scaffold.
	if err := c.ensureStore(); err != nil {
		t.Fatal(err)
	}
	if fsutil.DirExists(c.Paths.Clusters + "/a.dev/secrets") {
		t.Error("scaffolded a store for a nonexistent domain")
	}
	// Domain dir present → store created (so the first write lands there,
	// not in the flat fallback).
	os.MkdirAll(c.Paths.Clusters+"/a.dev", 0o755)
	if err := c.ensureStore(); err != nil {
		t.Fatal(err)
	}
	if !fsutil.DirExists(c.Paths.Clusters + "/a.dev/secrets") {
		t.Error("store not scaffolded")
	}
}

func TestCheckUnencrypted(t *testing.T) {
	c, _, _ := testEnv(t)
	store := c.Paths.Base + "/.secrets"
	var warn bytes.Buffer

	if !CheckUnencrypted(store, &warn) {
		t.Error("missing dir must pass")
	}
	write(t, store+"/Secret.a.default.K", "v")
	if CheckUnencrypted(store, &warn) {
		t.Error("unencrypted secret must fail")
	}
	if !strings.Contains(warn.String(), "Unencrypted secret: Secret.a.default.K — run: lo secrets encrypt") {
		t.Errorf("warn: %s", warn.String())
	}

	write(t, store+"/Secret.a.default.K.enc", "e")
	future := time.Now().Add(2 * time.Second)
	os.Chtimes(store+"/Secret.a.default.K.enc", future, future)
	warn.Reset()
	if !CheckUnencrypted(store, &warn) {
		t.Errorf("fresh .enc must pass: %s", warn.String())
	}

	os.Chtimes(store+"/Secret.a.default.K", future.Add(2*time.Second), future.Add(2*time.Second))
	warn.Reset()
	if CheckUnencrypted(store, &warn) {
		t.Error("stale .enc must fail")
	}
	if !strings.Contains(warn.String(), "Secret changed since last encrypt: Secret.a.default.K — run: lo secrets encrypt") {
		t.Errorf("warn: %s", warn.String())
	}
}

func TestCheckFlatShadows(t *testing.T) {
	c, _, _ := testEnv(t)
	flat := c.Paths.Base + "/.secrets"
	domDir := c.Paths.Clusters + "/app.example.com"
	os.MkdirAll(flat, 0o755)
	os.MkdirAll(domDir+"/secrets", 0o755)
	var out bytes.Buffer

	// Clean: a global-only flat secret is not a shadow.
	write(t, domDir+"/secrets/Secret.app.default.TOKEN", "v")
	write(t, flat+"/Secret.registries-tls.lok8s-system.tls.crt", "global")
	if !CheckFlatShadows(flat, domDir, &out) || out.Len() != 0 {
		t.Errorf("clean case: %s", out.String())
	}

	// Identical duplicate → shadow.
	write(t, flat+"/Secret.app.default.TOKEN", "v")
	out.Reset()
	if CheckFlatShadows(flat, domDir, &out) {
		t.Error("shadow must fail")
	}
	if !strings.Contains(out.String(), "Flat-store shadow: Secret.app.default.TOKEN") {
		t.Errorf("out: %s", out.String())
	}

	// Divergent duplicate → DRIFT.
	write(t, flat+"/Secret.app.default.TOKEN", "STALE")
	out.Reset()
	if CheckFlatShadows(flat, domDir, &out) {
		t.Error("drift must fail")
	}
	if !strings.Contains(out.String(), "Flat-store DRIFT: Secret.app.default.TOKEN") {
		t.Errorf("out: %s", out.String())
	}

	// .enc/.sha siblings ignored.
	os.Remove(flat + "/Secret.app.default.TOKEN")
	write(t, domDir+"/secrets/Secret.app.default.TOKEN.enc", "e")
	write(t, domDir+"/secrets/Secret.app.default.TOKEN.sha", "h")
	write(t, flat+"/Secret.app.default.TOKEN.enc", "e")
	write(t, flat+"/Secret.app.default.TOKEN.sha", "h")
	out.Reset()
	if !CheckFlatShadows(flat, domDir, &out) || out.Len() != 0 {
		t.Errorf("enc/sha siblings: %s", out.String())
	}
}
