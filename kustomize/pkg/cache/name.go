package cache

import (
	"strings"

	"github.com/kernpilot/lok8s/kustomize/pkg/storeguard"
)

// FormatName builds the cache filename for a given (secret, namespace,
// key) triple. Format: "Secret.<name>.<namespace>.<key>".
//
// This naming convention is **load-bearing**: it must match the bash
// plugin so existing $PATH_SECRETS directories work without
// regeneration. Cross-secret references in the spec ("secret/key" or
// "secret/ns/key") use the same string convention.
//
// Empty namespace is normalized to the literal "default", matching the
// bash plugin's behavior. The name comes from storeguard.EntryName, the
// builder of storeguard.CheckName too: Put writes the entry that a reader
// of this name reads.
func FormatName(secret, namespace, key string) string {
	return storeguard.EntryName(secret, defaultNamespace(namespace), key)
}

// defaultNamespace is namespace, or "default" when it is empty.
func defaultNamespace(namespace string) string {
	if namespace == "" {
		return "default"
	}
	return namespace
}

// ParseRef parses a cross-secret reference into (secret, namespace, key).
//
// Accepted forms:
//
//	"secret/key"             → namespace=defaultNS
//	"secret/namespace/key"
//	"Secret.name.ns.key"     → legacy bash form (passthrough split)
//
// The legacy form is accepted for backward compatibility with manual
// secretRef paths in existing manifests.
func ParseRef(ref, defaultNS string) (secret, namespace, key string, ok bool) {
	if ref == "" {
		return "", "", "", false
	}
	// Legacy "Secret.name.ns.key..."
	if strings.HasPrefix(ref, "Secret.") {
		parts := strings.SplitN(strings.TrimPrefix(ref, "Secret."), ".", 3)
		if len(parts) != 3 {
			return "", "", "", false
		}
		return parts[0], parts[1], parts[2], true
	}
	// "secret/key" or "secret/namespace/key"
	parts := strings.Split(ref, "/")
	switch len(parts) {
	case 2:
		return parts[0], defaultNS, parts[1], true
	case 3:
		return parts[0], parts[1], parts[2], true
	default:
		return "", "", "", false
	}
}
