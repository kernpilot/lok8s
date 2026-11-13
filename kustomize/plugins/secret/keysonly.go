package secret

import (
	"io"

	"github.com/kernpilot/lok8s/kustomize/pkg/plugin"

	"github.com/kernpilot/lok8s/kustomize/plugins/secret/generator"
)

// runKeysOnly is the keys-only run (KeysOnlyEnv and DisableEnv both on). It
// decodes the spec with the same strict check as a real run and emits the
// Secret with every key that the spec names, an empty value for each, and
// the KeysOnlyAnnotation. It reads nothing but the spec: Build does not run,
// so no store is opened, read or minted, and no generator runs (no file,
// environment variable, script, CA or random source is read).
//
// The key set is that of a real run, with one difference: an `env:` entry
// with `optional: true` is always emitted (a real run omits it when its
// variable is unset and not cached). Like a real run, it refuses a key that
// two sections produce, a cert spec with conflicting fields, and a Secret
// type whose required keys are missing. It does not run the checks that a
// generator makes on its own values.
func runKeysOnly(argv []string, stdin io.Reader, stdout io.Writer) error {
	src, closer, err := plugin.OpenSpec(argv, stdin)
	if err != nil {
		return err
	}
	if closer != nil {
		defer closer()
	}
	p := New()
	if err := p.Decode(src); err != nil {
		return err
	}
	entries, err := p.keyRegistry().Run(&plugin.Context{
		Name:      p.spec.Metadata.Name,
		Namespace: p.spec.Metadata.Namespace,
	})
	if err != nil {
		return err
	}
	p.keysOnly = true
	return p.Emit(entries, stdout)
}

// keyRegistry holds one key generator for each section of Build, with the
// same names in the same order, so the registry reports a key collision
// and a cert error exactly as in a real run.
func (p *Plugin) keyRegistry() *plugin.Registry {
	s := p.spec
	r := plugin.NewRegistry()
	r.Add(keyNames{"literals", fixed(sortedKeys(s.Literals))})
	r.Add(keyNames{"env", fixed(sortedKeys(s.Env))})
	r.Add(keyNames{"b64", fixed(sortedKeys(s.B64))})
	r.Add(keyNames{"file", fixed(sortedKeys(s.File))})
	r.Add(keyNames{"passwd", fixed(sortedKeys(s.Passwd))})
	r.Add(keyNames{"key", fixed(sortedKeys(s.Key))})
	r.Add(keyNames{"template", fixed(sortedKeys(s.Template))})
	r.Add(keyNames{"bash", fixed(sortedKeys(s.Bash))})
	r.Add(keyNames{"secretRef", fixed(sortedKeys(s.SecretRef))})
	r.Add(keyNames{"htpasswd", fixed(sortedKeys(s.Htpasswd))})
	r.Add(keyNames{"cert", func() ([]string, error) { return generator.CertKeys(s.Cert) }})
	return r
}

// fixed returns a key source with no error.
func fixed(keys []string) func() ([]string, error) {
	return func() ([]string, error) { return keys, nil }
}

// keyNames is a generator that emits its keys with an empty value. It
// ignores the Context: it reads nothing.
type keyNames struct {
	name string
	keys func() ([]string, error)
}

// Name returns the name of the section that the generator stands for.
func (g keyNames) Name() string { return g.name }

// Generate returns one entry with an empty value for each key.
func (g keyNames) Generate(*plugin.Context) ([]plugin.Entry, error) {
	keys, err := g.keys()
	if err != nil {
		return nil, err
	}
	out := make([]plugin.Entry, 0, len(keys))
	for _, k := range keys {
		out = append(out, plugin.Entry{Key: k, Value: []byte{}})
	}
	return out, nil
}
