package generator

import (
	"testing"

	"github.com/kernpilot/lok8s/kustomize/pkg/charset"

	specpkg "github.com/kernpilot/lok8s/kustomize/plugins/secret/spec"
)

func TestPasswd_RequireAllClasses(t *testing.T) {
	ctx, _ := newCtx(t, nil)
	g := NewPasswd(map[string]specpkg.PasswdEntry{
		"PW": {Length: 48, Chars: "alphanum+symbols", Require: []string{"upper", "lower", "digit", "symbol"}},
	})
	got, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].Value) != 48 {
		t.Fatalf("unexpected entries: %+v", got)
	}
	if !charset.SatisfiesAll(got[0].Value, charset.Classes()) {
		t.Errorf("generated password %q lacks a required class", got[0].Value)
	}
}

func TestPasswd_RequireStableAcrossRuns(t *testing.T) {
	ctx, _ := newCtx(t, nil)
	spec := map[string]specpkg.PasswdEntry{
		"PW": {Length: 32, Chars: "alphanum+symbols", Require: []string{"upper", "lower", "digit", "symbol"}},
	}
	first, err := NewPasswd(spec).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Re-generate against the same cache: the require value must be reused
	// (cached), not re-rolled — otherwise a re-build would change the password.
	second, err := NewPasswd(spec).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(first[0].Value) != string(second[0].Value) {
		t.Errorf("require password not stable across runs: %q != %q", first[0].Value, second[0].Value)
	}
}

func TestPasswd_RequireInfeasibleCharset(t *testing.T) {
	ctx, _ := newCtx(t, nil)
	// default charset (alphanum) has no symbols → require symbol is impossible.
	g := NewPasswd(map[string]specpkg.PasswdEntry{
		"PW": {Length: 16, Require: []string{"symbol"}},
	})
	if _, err := g.Generate(ctx); err == nil {
		t.Fatal("expected feasibility error: alphanum has no symbol")
	}
}

func TestPasswd_RequireMoreClassesThanLength(t *testing.T) {
	ctx, _ := newCtx(t, nil)
	g := NewPasswd(map[string]specpkg.PasswdEntry{
		"PW": {Length: 3, Chars: "alphanum+symbols", Require: []string{"upper", "lower", "digit", "symbol"}},
	})
	if _, err := g.Generate(ctx); err == nil {
		t.Fatal("expected error: 4 classes cannot fit in length 3")
	}
}
