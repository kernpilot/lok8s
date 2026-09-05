// Package certgen generates a local development CA and leaf certificates
// using only the Go standard library — no external `mkcert` binary.
//
// The certificate-template choices (RSA-3072 CA / RSA-2048 leaf, the 2-year-
// 3-month leaf validity that stays under Apple's 825-day cap, the CA key usage
// and SAN handling) follow FiloSottile/mkcert. That logic is reimplemented here
// in pure functions (PEM in, PEM out — no file or trust-store side effects); the
// caller owns caching and where the bytes land.
//
// Portions adapted from FiloSottile/mkcert (cert.go):
//
//	Copyright (c) 2018 The mkcert Authors. All rights reserved.
//	Use of this source code is governed by a BSD-3-Clause license.
//	https://github.com/FiloSottile/mkcert/blob/master/LICENSE
package certgen

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1" // #nosec G505 -- SubjectKeyId per RFC 5280 4.2.1.2 method 1; a key identifier, not a security hash
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// Organization strings stamped into the generated certificates.
const (
	caOrg   = "lok8s development CA"
	leafOrg = "lok8s development certificate"
)

// CA file names inside CAROOT — identical to mkcert's, so a CA created here is
// interchangeable with one created by `mkcert -install` (and vice versa).
const (
	RootName    = "rootCA.pem"
	RootKeyName = "rootCA-key.pem"
)

// CARoot resolves the shared local-CA directory exactly the way mkcert does, so
// both tools share one CA: $CAROOT, else the OS data dir + "/mkcert". Returns ""
// if it can't be determined (no home directory).
func CARoot() string {
	if env := os.Getenv("CAROOT"); env != "" {
		return env
	}
	var dir string
	switch {
	case runtime.GOOS == "windows":
		dir = os.Getenv("LocalAppData")
	case os.Getenv("XDG_DATA_HOME") != "":
		dir = os.Getenv("XDG_DATA_HOME")
	case runtime.GOOS == "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, "Library", "Application Support")
	default:
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".local", "share")
	}
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "mkcert")
}

// NewCAKey returns a fresh RSA-3072 CA private key as PKCS#8 PEM.
func NewCAKey(randr io.Reader) ([]byte, error) { return NewRSAKeyPEM(randr, 3072) }

// NewLeafKey returns a fresh RSA-2048 leaf private key as PKCS#8 PEM.
func NewLeafKey(randr io.Reader) ([]byte, error) { return NewRSAKeyPEM(randr, 2048) }

// NewRSAKeyPEM returns a fresh RSA private key of the given bit size as
// PKCS#8 PEM — the same format openssl's `genpkey` default emits. Exported so
// the key: generator (and template key: sub-section) can mint RSA keys with
// the identical marshalling the cert: generator uses.
func NewRSAKeyPEM(randr io.Reader, bits int) ([]byte, error) {
	key, err := rsa.GenerateKey(randr, bits)
	if err != nil {
		return nil, fmt.Errorf("generate %d-bit key: %w", bits, err)
	}
	return marshalPKCS8PEM(key)
}

// NewEd25519KeyPEM returns a fresh Ed25519 private key as PKCS#8 PEM (matching
// openssl `genpkey -algorithm ed25519`). Ed25519 has a fixed key size, so it
// takes no bit-size parameter.
func NewEd25519KeyPEM(randr io.Reader) ([]byte, error) {
	_, key, err := ed25519.GenerateKey(randr)
	if err != nil {
		return nil, fmt.Errorf("generate ed25519 key: %w", err)
	}
	return marshalPKCS8PEM(key)
}

// marshalPKCS8PEM DER-encodes a private key as PKCS#8 and wraps it in a
// "PRIVATE KEY" PEM block.
func marshalPKCS8PEM(key crypto.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// SelfSignCA self-signs a development root CA certificate for the given PKCS#8
// CA key (PEM). Valid for 10 years; IsCA with a single-level path constraint.
func SelfSignCA(randr io.Reader, caKeyPEM []byte) ([]byte, error) {
	key, err := parsePKCS8(caKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("CA key: %w", err)
	}
	pub := key.(crypto.Signer).Public()

	// Subject Key Identifier = SHA-1 of the DER public key (mkcert does this so
	// iOS shows the cert under "Certificate Trust Settings").
	spkiASN1, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, fmt.Errorf("marshal public key: %w", err)
	}
	var spki struct {
		Algorithm        pkix.AlgorithmIdentifier
		SubjectPublicKey asn1.BitString
	}
	if _, err := asn1.Unmarshal(spkiASN1, &spki); err != nil {
		return nil, fmt.Errorf("decode public key: %w", err)
	}
	skid := sha1.Sum(spki.SubjectPublicKey.Bytes) // #nosec G401 -- see the import note: SKID, not a signature

	serial, err := randomSerial(randr)
	if err != nil {
		return nil, err
	}
	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{caOrg},
			CommonName:   caOrg,
		},
		SubjectKeyId: skid[:],

		NotBefore: time.Now(),
		NotAfter:  time.Now().AddDate(10, 0, 0),

		KeyUsage: x509.KeyUsageCertSign,

		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(randr, tpl, tpl, pub, key)
	if err != nil {
		return nil, fmt.Errorf("create CA certificate: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

// SignLeaf issues a leaf certificate for hosts, signed by the CA (cert + key
// PEM), using leafKeyPEM as the leaf's key. hosts entries are classified as IP /
// email / URI / DNS (wildcards land in DNSNames). Valid for 2 years 3 months.
func SignLeaf(randr io.Reader, caCertPEM, caKeyPEM, leafKeyPEM []byte, hosts []string) ([]byte, error) {
	if len(hosts) == 0 {
		return nil, fmt.Errorf("no hosts given for leaf certificate")
	}
	caCert, err := parseCert(caCertPEM)
	if err != nil {
		return nil, fmt.Errorf("CA cert: %w", err)
	}
	caKey, err := parsePKCS8(caKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("CA key: %w", err)
	}
	leafKey, err := parsePKCS8(leafKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("leaf key: %w", err)
	}

	serial, err := randomSerial(randr)
	if err != nil {
		return nil, err
	}
	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{Organization: []string{leafOrg}},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().AddDate(2, 3, 0),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
	}
	for _, h := range hosts {
		switch {
		case net.ParseIP(h) != nil:
			tpl.IPAddresses = append(tpl.IPAddresses, net.ParseIP(h))
		case isEmail(h):
			tpl.EmailAddresses = append(tpl.EmailAddresses, h)
		case isURI(h):
			u, _ := url.Parse(h)
			tpl.URIs = append(tpl.URIs, u)
		default:
			tpl.DNSNames = append(tpl.DNSNames, h)
		}
	}
	if len(tpl.IPAddresses) > 0 || len(tpl.DNSNames) > 0 || len(tpl.URIs) > 0 {
		tpl.ExtKeyUsage = append(tpl.ExtKeyUsage, x509.ExtKeyUsageServerAuth)
	}
	if len(tpl.EmailAddresses) > 0 {
		tpl.ExtKeyUsage = append(tpl.ExtKeyUsage, x509.ExtKeyUsageEmailProtection)
	}

	pub := leafKey.(crypto.Signer).Public()
	der, err := x509.CreateCertificate(randr, tpl, caCert, pub, caKey)
	if err != nil {
		return nil, fmt.Errorf("create leaf certificate: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

// LoadOrCreateCARoot loads the shared CA (cert + key PEM) from CAROOT, creating
// it there — exactly as mkcert would — if absent. This is the one place that
// touches CAROOT files; the cert: generator calls it for default (CAROOT) leaves.
func LoadOrCreateCARoot(randr io.Reader) (cert, key []byte, err error) {
	dir := CARoot()
	if dir == "" {
		return nil, nil, fmt.Errorf("cannot resolve CAROOT (set $CAROOT)")
	}
	certPath := filepath.Join(dir, RootName)
	keyPath := filepath.Join(dir, RootKeyName)

	if c, e := os.ReadFile(certPath); e == nil {
		k, e2 := os.ReadFile(keyPath)
		if e2 != nil {
			return nil, nil, fmt.Errorf("CAROOT %s has %s but no %s (keyless CA cannot sign) — run `mkcert -install` / `lo trust`, or remove it", dir, RootName, RootKeyName)
		}
		return c, k, nil
	}
	// CA absent → create both (mkcert's newCA).
	if key, err = NewCAKey(randr); err != nil {
		return nil, nil, err
	}
	if cert, err = SelfSignCA(randr, key); err != nil {
		return nil, nil, err
	}
	if err = os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("create CAROOT %s: %w", dir, err)
	}
	if err = os.WriteFile(keyPath, key, 0o400); err != nil {
		return nil, nil, fmt.Errorf("write CA key: %w", err)
	}
	if err = os.WriteFile(certPath, cert, 0o644); err != nil {
		return nil, nil, fmt.Errorf("write CA cert: %w", err)
	}
	return cert, key, nil
}

func randomSerial(randr io.Reader) (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(randr, limit)
	if err != nil {
		return nil, fmt.Errorf("generate serial number: %w", err)
	}
	return serial, nil
}

func parsePKCS8(pemBytes []byte) (crypto.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("expected a PRIVATE KEY PEM block")
	}
	return x509.ParsePKCS8PrivateKey(block.Bytes)
}

func parseCert(pemBytes []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("expected a CERTIFICATE PEM block")
	}
	return x509.ParseCertificate(block.Bytes)
}

func isEmail(h string) bool {
	e, err := mail.ParseAddress(h)
	return err == nil && e.Address == h
}

func isURI(h string) bool {
	u, err := url.Parse(h)
	return err == nil && u.Scheme != "" && u.Host != ""
}

// ValidateCAPair confirms certPEM/keyPEM parse, belong together (matching
// public keys), and that the cert is a CA — the checks a consumer of the pair
// (e.g. a cert-manager CA issuer) would otherwise fail at issue time, moved to
// build time. Loud beats late: a hand-edited or mismatched CAROOT surfaces
// here, not as a cluster-side issuance error.
func ValidateCAPair(certPEM, keyPEM []byte) error {
	cert, err := parseCert(certPEM)
	if err != nil {
		return fmt.Errorf("CA cert: %w", err)
	}
	key, err := parsePKCS8(keyPEM)
	if err != nil {
		return fmt.Errorf("CA key: %w", err)
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return fmt.Errorf("CA key: %T cannot sign", key)
	}
	pub, ok := signer.Public().(interface{ Equal(x crypto.PublicKey) bool })
	if !ok {
		return fmt.Errorf("CA key: unsupported public key type %T", signer.Public())
	}
	if !pub.Equal(cert.PublicKey) {
		return fmt.Errorf("CA cert and key do not match (public keys differ)")
	}
	if !cert.IsCA {
		return fmt.Errorf("certificate is not a CA")
	}
	return nil
}
