package plugin

import (
	"errors"
	"strings"
	"testing"
)

// fakeGen is a test-only Generator that returns a fixed set of entries
// or an error.
type fakeGen struct {
	name    string
	entries []Entry
	err     error
}

func (f *fakeGen) Name() string                       { return f.name }
func (f *fakeGen) Generate(_ *Context) ([]Entry, error) { return f.entries, f.err }

func TestRegistry_Empty(t *testing.T) {
	r := NewRegistry()
	got, err := r.Run(&Context{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("empty registry returned %d entries", len(got))
	}
}

func TestRegistry_AddNilIgnored(t *testing.T) {
	r := NewRegistry()
	r.Add(nil)
	if len(r.generators) != 0 {
		t.Error("nil generator should not be added")
	}
}

func TestRegistry_RunsInOrder(t *testing.T) {
	r := NewRegistry()
	r.Add(&fakeGen{
		name: "literal",
		entries: []Entry{
			{Key: "FOO", Value: []byte("foo")},
			{Key: "BAR", Value: []byte("bar")},
		},
	})
	r.Add(&fakeGen{
		name: "passwd",
		entries: []Entry{
			{Key: "PASS", Value: []byte("pass")},
		},
	})
	got, err := r.Run(&Context{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3", len(got))
	}
	// Sorted alphabetically by key.
	wantKeys := []string{"BAR", "FOO", "PASS"}
	for i, k := range wantKeys {
		if got[i].Key != k {
			t.Errorf("entries[%d].Key = %q, want %q", i, got[i].Key, k)
		}
	}
}

func TestRegistry_KeyCollisionDetected(t *testing.T) {
	r := NewRegistry()
	r.Add(&fakeGen{
		name:    "literal",
		entries: []Entry{{Key: "FOO", Value: []byte("a")}},
	})
	r.Add(&fakeGen{
		name:    "passwd",
		entries: []Entry{{Key: "FOO", Value: []byte("b")}},
	})
	_, err := r.Run(&Context{})
	if err == nil {
		t.Fatal("expected collision error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "FOO") || !strings.Contains(msg, "literal") || !strings.Contains(msg, "passwd") {
		t.Errorf("error should mention both generators and the key: %q", msg)
	}
}

func TestRegistry_GeneratorErrorWrapped(t *testing.T) {
	r := NewRegistry()
	wantErr := errors.New("boom")
	r.Add(&fakeGen{name: "passwd", err: wantErr})
	_, err := r.Run(&Context{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("expected wrapped %v, got %v", wantErr, err)
	}
	if !strings.Contains(err.Error(), "passwd") {
		t.Errorf("error should mention generator name: %q", err.Error())
	}
}

func TestDefaultEnv_LookupExists(t *testing.T) {
	t.Setenv("TEST_VAR_FOR_PLUGIN", "yes")
	got, ok := DefaultEnv("TEST_VAR_FOR_PLUGIN")
	if !ok || got != "yes" {
		t.Errorf("DefaultEnv = (%q, %v)", got, ok)
	}
}

func TestDefaultEnv_LookupMissing(t *testing.T) {
	if _, ok := DefaultEnv("NONEXISTENT_VAR_PLUGIN_TEST_XYZ"); ok {
		t.Error("missing var should return ok=false")
	}
}
