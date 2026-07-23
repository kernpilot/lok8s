package certgen_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"

	"github.com/kernpilot/lok8s/kustomize/pkg/certgen"
)

// parseKey decodes a "PRIVATE KEY" (PKCS#8) PEM block, failing the test if the
// block type or DER is wrong.
func parseKey(t *testing.T, pemBytes []byte) any {
	t.Helper()
	b, _ := pem.Decode(pemBytes)
	if b == nil || b.Type != "PRIVATE KEY" {
		t.Fatalf("expected a PRIVATE KEY (PKCS#8) PEM block, got %q", func() string {
			if b == nil {
				return "<not PEM>"
			}
			return b.Type
		}())
	}
	key, err := x509.ParsePKCS8PrivateKey(b.Bytes)
	if err != nil {
		t.Fatalf("ParsePKCS8PrivateKey: %v", err)
	}
	return key
}

func TestNewRSAKeyPEM(t *testing.T) {
	for _, bits := range []int{2048, 3072} {
		pemBytes, err := certgen.NewRSAKeyPEM(rand.Reader, bits)
		if err != nil {
			t.Fatalf("bits=%d: %v", bits, err)
		}
		key, ok := parseKey(t, pemBytes).(*rsa.PrivateKey)
		if !ok {
			t.Fatalf("bits=%d: not an RSA key", bits)
		}
		if key.N.BitLen() != bits {
			t.Errorf("bits=%d: got %d-bit key", bits, key.N.BitLen())
		}
	}
}

func TestNewEd25519KeyPEM(t *testing.T) {
	pemBytes, err := certgen.NewEd25519KeyPEM(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key := parseKey(t, pemBytes)
	if _, ok := key.(ed25519.PrivateKey); !ok {
		t.Fatalf("key type = %T, want ed25519.PrivateKey", key)
	}
}

// The legacy CA/leaf helpers must keep producing RSA keys of their historic
// sizes (3072 CA / 2048 leaf) via the now-shared NewRSAKeyPEM.
func TestLegacyKeyHelpersUnchanged(t *testing.T) {
	caPEM, err := certgen.NewCAKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if bits := parseKey(t, caPEM).(*rsa.PrivateKey).N.BitLen(); bits != 3072 {
		t.Errorf("NewCAKey = %d bits, want 3072", bits)
	}
	leafPEM, err := certgen.NewLeafKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if bits := parseKey(t, leafPEM).(*rsa.PrivateKey).N.BitLen(); bits != 2048 {
		t.Errorf("NewLeafKey = %d bits, want 2048", bits)
	}
}
