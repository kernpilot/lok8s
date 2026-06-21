package certgen_test

import (
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"

	"github.com/kernpilot/lok8s/kustomize/pkg/certgen"
)

func parse(t *testing.T, pemBytes []byte) *x509.Certificate {
	t.Helper()
	b, _ := pem.Decode(pemBytes)
	if b == nil || b.Type != "CERTIFICATE" {
		t.Fatalf("expected a CERTIFICATE PEM block")
	}
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	return c
}

func TestSelfSignCA(t *testing.T) {
	key, err := certgen.NewCAKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	crtPEM, err := certgen.SelfSignCA(rand.Reader, key)
	if err != nil {
		t.Fatal(err)
	}
	ca := parse(t, crtPEM)
	if !ca.IsCA {
		t.Error("CA cert IsCA=false")
	}
	if ca.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Error("CA missing KeyUsageCertSign")
	}
	if !ca.BasicConstraintsValid {
		t.Error("CA BasicConstraintsValid=false")
	}
	if got := time.Until(ca.NotAfter); got < 9*365*24*time.Hour {
		t.Errorf("CA validity too short: %v", got)
	}
}

func TestSignLeaf_ChainsAndSANs(t *testing.T) {
	caKey, _ := certgen.NewCAKey(rand.Reader)
	caCrt, _ := certgen.SelfSignCA(rand.Reader, caKey)
	leafKey, err := certgen.NewLeafKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hosts := []string{"kubehz.dev", "*.kubehz.dev", "127.0.0.1"}
	leafCrt, err := certgen.SignLeaf(rand.Reader, caCrt, caKey, leafKey, hosts)
	if err != nil {
		t.Fatal(err)
	}

	leaf := parse(t, leafCrt)
	if len(leaf.DNSNames) != 2 {
		t.Errorf("DNSNames=%v, want [kubehz.dev *.kubehz.dev]", leaf.DNSNames)
	}
	if len(leaf.IPAddresses) != 1 {
		t.Errorf("IPAddresses=%v, want 1", leaf.IPAddresses)
	}

	// The leaf must verify against the CA — including a wildcard-covered host.
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caCrt) {
		t.Fatal("AppendCertsFromPEM(caCrt) failed")
	}
	for _, name := range []string{"kubehz.dev", "app.kubehz.dev"} {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: name}); err != nil {
			t.Errorf("verify %q against CA: %v", name, err)
		}
	}
}

func TestSignLeaf_NoHosts(t *testing.T) {
	caKey, _ := certgen.NewCAKey(rand.Reader)
	caCrt, _ := certgen.SelfSignCA(rand.Reader, caKey)
	leafKey, _ := certgen.NewLeafKey(rand.Reader)
	if _, err := certgen.SignLeaf(rand.Reader, caCrt, caKey, leafKey, nil); err == nil {
		t.Error("expected an error for a leaf with no hosts")
	}
}
