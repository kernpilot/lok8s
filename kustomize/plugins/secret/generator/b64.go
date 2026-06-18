package generator

import (
	"encoding/base64"
	"sort"

	"github.com/kernpilot/lok8s/kustomize/pkg/errs"
	"github.com/kernpilot/lok8s/kustomize/pkg/plugin"
)

// B64 is the generator for the `b64:` field. The user provides values
// that are **already** base64-encoded; the generator validates the
// encoding (round-trip decode) and stores the **decoded** bytes so the
// final base64 in the Secret matches what the user wrote.
//
// This is the escape hatch for pasting opaque tokens from external
// systems without double-encoding.
type B64 struct {
	spec map[string]string
}

// NewB64 wraps a b64 map.
func NewB64(spec map[string]string) *B64 { return &B64{spec: spec} }

// Name returns the generator's spec field name.
func (g *B64) Name() string { return "b64" }

// Generate validates each value as base64 and emits the decoded bytes.
func (g *B64) Generate(_ *plugin.Context) ([]plugin.Entry, error) {
	if len(g.spec) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(g.spec))
	for k := range g.spec {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]plugin.Entry, 0, len(keys))
	for _, k := range keys {
		decoded, err := base64.StdEncoding.DecodeString(g.spec[k])
		if err != nil {
			return nil, errs.Wrap(k, errs.Newf("invalid base64: %v", err))
		}
		out = append(out, plugin.Entry{Key: k, Value: decoded})
	}
	return out, nil
}
