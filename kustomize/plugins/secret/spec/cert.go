package spec

// CertSpec is the spec for the `cert:` field — a development CA or a leaf
// certificate signed by one, generated with crypto/x509 (no mkcert binary).
// There is one cert per Secret (a kubernetes.io/tls Secret holds exactly
// tls.crt + tls.key), so `cert:` is a single mapping rather than a key→entry map.
//
//	# the CA Secret (Opaque) — emits ca.crt; the CA key is cached as ca.key
//	# (for signing) but never emitted into the Secret.
//	cert:
//	  ca: true
//
//	# a leaf Secret (kubernetes.io/tls) — emits tls.crt + tls.key, signed by the
//	# CA named in caRef (auto-created on first use, so build order is irrelevant).
//	cert:
//	  hosts: [kubehz.dev, "*.kubehz.dev"]
//	  caRef: mkcert-ca/kubehz-system     # <secret>[/<namespace>]
type CertSpec struct {
	// CA marks this Secret as a self-signed development root CA. Mutually
	// exclusive with Hosts.
	CA bool `yaml:"ca,omitempty"`
	// Hosts are the SANs for a LEAF certificate: DNS names, IPs, or wildcards
	// like "*.kubehz.dev". Required for a leaf; must be empty when CA is set.
	Hosts []string `yaml:"hosts,omitempty"`
	// CARef names the CA Secret that signs this leaf, as "<secret>[/<namespace>]"
	// (namespace defaults to this Secret's namespace). Its ca.crt + ca.key cache
	// files are read for signing — and created there if absent, so the CA Secret
	// need not have been built first.
	CARef string `yaml:"caRef,omitempty"`
}
