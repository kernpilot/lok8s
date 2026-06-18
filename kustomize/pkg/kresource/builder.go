// Package kresource builds k8s API resources for kustomize plugins.
//
// Today it has a Secret-aware Builder + a registry of Secret types and
// their required keys. The Builder itself (Set/Add/Marshal) is generic;
// a future ConfigMap plugin would add a configmaptypes.go and reuse the
// Builder pattern.
package kresource

import (
	"encoding/base64"
	"sort"

	"github.com/kernpilot/lok8s/kustomize/pkg/kyaml"
)

// SecretBuilder assembles a Kubernetes Secret resource. All keys are
// added to a sorted data map, base64-encoded on emit.
type SecretBuilder struct {
	Name        string
	Namespace   string
	Type        string
	Annotations map[string]string
	Labels      map[string]string
	data        map[string][]byte
}

// NewSecret returns a SecretBuilder with the given metadata. Type
// defaults to Opaque when empty.
func NewSecret(name, namespace, secretType string) *SecretBuilder {
	if secretType == "" {
		secretType = SecretTypeOpaque
	}
	return &SecretBuilder{
		Name:      name,
		Namespace: namespace,
		Type:      secretType,
		data:      make(map[string][]byte),
	}
}

// Add stores raw bytes under key. The bytes are base64-encoded at
// Marshal time. Calling Add twice with the same key replaces the value.
func (b *SecretBuilder) Add(key string, value []byte) {
	if b.data == nil {
		b.data = make(map[string][]byte)
	}
	b.data[key] = value
}

// AddBase64 stores already-base64-encoded bytes. The encoding is
// validated (round-trip decode); decoded bytes are then re-stored as
// raw so the output is consistent with Add.
func (b *SecretBuilder) AddBase64(key string, encoded []byte) error {
	decoded, err := base64.StdEncoding.DecodeString(string(encoded))
	if err != nil {
		return err
	}
	b.Add(key, decoded)
	return nil
}

// Has reports whether the builder contains a value for key.
func (b *SecretBuilder) Has(key string) bool {
	_, ok := b.data[key]
	return ok
}

// Keys returns the data keys as a set, used by validation.
func (b *SecretBuilder) Keys() map[string]struct{} {
	out := make(map[string]struct{}, len(b.data))
	for k := range b.data {
		out[k] = struct{}{}
	}
	return out
}

// secretYAML is the on-disk shape of a k8s Secret. Sorted maps for
// deterministic output.
type secretYAML struct {
	APIVersion string            `yaml:"apiVersion"`
	Kind       string            `yaml:"kind"`
	Metadata   secretMeta        `yaml:"metadata"`
	Type       string            `yaml:"type"`
	Data       map[string]string `yaml:"data,omitempty"`
}

type secretMeta struct {
	Name        string            `yaml:"name"`
	Namespace   string            `yaml:"namespace,omitempty"`
	Labels      map[string]string `yaml:"labels,omitempty"`
	Annotations map[string]string `yaml:"annotations,omitempty"`
}

// Marshal renders the Secret as YAML with sorted, base64-encoded data
// and deterministic key ordering. Suitable to write directly to stdout
// for kustomize to consume.
func (b *SecretBuilder) Marshal() ([]byte, error) {
	encoded := make(map[string]string, len(b.data))
	keys := make([]string, 0, len(b.data))
	for k := range b.data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		encoded[k] = base64.StdEncoding.EncodeToString(b.data[k])
	}
	doc := secretYAML{
		APIVersion: "v1",
		Kind:       "Secret",
		Metadata: secretMeta{
			Name:        b.Name,
			Namespace:   b.Namespace,
			Labels:      b.Labels,
			Annotations: b.Annotations,
		},
		Type: b.Type,
		Data: encoded,
	}
	return kyaml.Encode(doc)
}
