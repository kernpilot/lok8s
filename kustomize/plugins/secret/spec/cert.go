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
//	# a leaf Secret (kubernetes.io/tls) — emits tls.crt + tls.key. By default it
//	# is signed by the SHARED mkcert CA at CAROOT (one CA per developer, across
//	# projects; trust it once with `mkcert -install`).
//	cert:
//	  hosts: [kubehz.dev, "*.kubehz.dev"]
//
//	# …or sign with an OWN, managed CA in the lok8s store (CI, separated
//	# instances, special CAs) via caRef — deterministic, no machine dependency.
//	cert:
//	  hosts: [kubehz.dev, "*.kubehz.dev"]
//	  caRef: my-ca/kube-system           # <secret>[/<namespace>]
type CertSpec struct {
	// CA marks this Secret as a self-signed development root CA in the lok8s store
	// (an OWN CA — the thing leaves point to with caRef). Mutually exclusive with
	// Hosts and CARef.
	CA bool `yaml:"ca,omitempty"`
	// CARoot emits the shared mkcert CAROOT CA's public cert as ca.crt (loading
	// or creating the CA at CAROOT, mkcert-free) — for distributing trust into the
	// cluster. Takes no other fields. Mutually exclusive with everything else.
	CARoot bool `yaml:"caRoot,omitempty"`
	// Hosts are the SANs for a LEAF certificate: DNS names, IPs, or wildcards
	// like "*.kubehz.dev". Required for a leaf; must be empty when CA is set.
	Hosts []string `yaml:"hosts,omitempty"`
	// CARef opts a leaf out of the default shared CAROOT CA and signs it with an
	// OWN CA in the lok8s store, named "<secret>[/<namespace>]" (namespace
	// defaults to this Secret's namespace). The store CA is auto-created on first
	// use, so the CA Secret need not have been built first. When empty, the leaf
	// is signed by the shared mkcert CA at CAROOT (the default).
	CARef string `yaml:"caRef,omitempty"`
}
