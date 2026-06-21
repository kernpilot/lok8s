package generator

import (
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"testing"

	"github.com/kernpilot/lok8s/kustomize/pkg/cache"
	"github.com/kernpilot/lok8s/kustomize/pkg/plugin"

	specpkg "github.com/kernpilot/lok8s/kustomize/plugins/secret/spec"
)

// certCtx builds a Context for a Secret (ns/name) sharing the PATH_SECRETS dir,
// with crypto/rand wired in (the cert generator needs ctx.Rand).
func certCtx(t *testing.T, dir, ns, name string) *plugin.Context {
	t.Helper()
	c, err := cache.New(dir, ns, name)
	if err != nil {
		t.Fatal(err)
	}
	return &plugin.Context{
		Name:      name,
		Namespace: ns,
		Cache:     c,
		Env:       func(k string) (string, bool) { return map[string]string{"PATH_SECRETS": dir}[k], k == "PATH_SECRETS" },
		Rand:      rand.Reader,
	}
}

func entryMap(es []plugin.Entry) map[string][]byte {
	m := map[string][]byte{}
	for _, e := range es {
		m[e.Key] = e.Value
	}
	return m
}

func TestCert_CA_EmitsCertNotKey(t *testing.T) {
	dir := t.TempDir()
	out, err := NewCert(&specpkg.CertSpec{CA: true}).Generate(certCtx(t, dir, "kube-system", "myca"))
	if err != nil {
		t.Fatal(err)
	}
	m := entryMap(out)
	if _, ok := m["ca.crt"]; !ok {
		t.Fatal("CA did not emit ca.crt")
	}
	if _, ok := m["ca.key"]; ok {
		t.Error("CA emitted ca.key — the private key must never leave the cache")
	}
	// ca.key must still be CACHED (for signing).
	c, _ := cache.New(dir, "kube-system", "myca")
	if _, err := c.GetOrCreate("ca.key", func() ([]byte, error) { return nil, fmt.Errorf("not cached") }); err != nil {
		t.Errorf("ca.key was not cached: %v", err)
	}
}

func TestCert_Leaf_AutoCreatesCAAndChains(t *testing.T) {
	dir := t.TempDir()
	// The CA Secret is NOT generated first — the leaf must create it via caRef.
	leaf := &specpkg.CertSpec{Hosts: []string{"kubehz.dev", "*.kubehz.dev"}, CARef: "myca/kube-system"}
	out, err := NewCert(leaf).Generate(certCtx(t, dir, "default", "mytls"))
	if err != nil {
		t.Fatal(err)
	}
	m := entryMap(out)
	if m["tls.crt"] == nil || m["tls.key"] == nil {
		t.Fatalf("leaf did not emit tls.crt + tls.key: %v", entryKeys(out))
	}

	// Read the auto-created CA cert and verify the leaf chains to it.
	caStore, _ := cache.New(dir, "kube-system", "myca")
	caCrt, err := caStore.GetOrCreate("ca.crt", func() ([]byte, error) { return nil, fmt.Errorf("CA was not auto-created") })
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caCrt) {
		t.Fatal("append CA failed")
	}
	blk, _ := pem.Decode(m["tls.crt"])
	leafCert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leafCert.Verify(x509.VerifyOptions{Roots: roots, DNSName: "app.kubehz.dev"}); err != nil {
		t.Errorf("leaf does not verify against the auto-created CA: %v", err)
	}
}

func TestCert_Validation(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]*specpkg.CertSpec{
		"ca+hosts":      {CA: true, Hosts: []string{"x"}},
		"leaf no caRef": {Hosts: []string{"x"}},
		"empty":         {},
	}
	for name, spec := range cases {
		if _, err := NewCert(spec).Generate(certCtx(t, dir, "default", "s")); err == nil {
			t.Errorf("%s: expected an error, got nil", name)
		}
	}
}

func entryKeys(es []plugin.Entry) []string {
	var k []string
	for _, e := range es {
		k = append(k, e.Key)
	}
	return k
}
