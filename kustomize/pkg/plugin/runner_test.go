package plugin

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// stubPlugin is a minimal Plugin used to exercise Run.
type stubPlugin struct {
	decodeErr  error
	buildErr   error
	emitErr    error
	registry   *Registry
	context    *Context
	emitOutput string
}

func (s *stubPlugin) Decode(_ io.Reader) error { return s.decodeErr }

func (s *stubPlugin) Build(env func(string) (string, bool), fileRoot string) (*Registry, *Context, error) {
	if s.buildErr != nil {
		return nil, nil, s.buildErr
	}
	if s.context == nil {
		s.context = &Context{Env: env, FileRoot: fileRoot}
	}
	if s.registry == nil {
		s.registry = NewRegistry()
	}
	return s.registry, s.context, nil
}

func (s *stubPlugin) Emit(_ []Entry, w io.Writer) error {
	if s.emitErr != nil {
		return s.emitErr
	}
	_, err := io.WriteString(w, s.emitOutput)
	return err
}

func TestRun_HappyPath(t *testing.T) {
	r := NewRegistry()
	r.Add(&fakeGen{name: "literal", entries: []Entry{{Key: "K", Value: []byte("v")}}})
	p := &stubPlugin{
		registry:   r,
		emitOutput: "kind: Secret\n",
	}
	var out bytes.Buffer
	if err := Run([]string{"plugin"}, strings.NewReader(""), &out, p); err != nil {
		t.Fatal(err)
	}
	if out.String() != "kind: Secret\n" {
		t.Errorf("output = %q", out.String())
	}
}

func TestRun_DecodeError(t *testing.T) {
	wantErr := errors.New("bad input")
	p := &stubPlugin{decodeErr: wantErr}
	err := Run([]string{"plugin"}, strings.NewReader(""), io.Discard, p)
	if !errors.Is(err, wantErr) {
		t.Errorf("expected %v, got %v", wantErr, err)
	}
}

func TestRun_BuildError(t *testing.T) {
	wantErr := errors.New("build failed")
	p := &stubPlugin{buildErr: wantErr}
	err := Run([]string{"plugin"}, strings.NewReader(""), io.Discard, p)
	if !errors.Is(err, wantErr) {
		t.Errorf("expected %v, got %v", wantErr, err)
	}
}

func TestRun_RegistryError(t *testing.T) {
	r := NewRegistry()
	r.Add(&fakeGen{name: "boom", err: errors.New("gen failed")})
	p := &stubPlugin{registry: r}
	err := Run([]string{"plugin"}, strings.NewReader(""), io.Discard, p)
	if err == nil {
		t.Fatal("expected error from registry.Run")
	}
}

func TestRun_EmitError(t *testing.T) {
	wantErr := errors.New("emit failed")
	p := &stubPlugin{emitErr: wantErr}
	err := Run([]string{"plugin"}, strings.NewReader(""), io.Discard, p)
	if !errors.Is(err, wantErr) {
		t.Errorf("expected %v, got %v", wantErr, err)
	}
}

func TestRun_PassesFileRootAndEnv(t *testing.T) {
	t.Setenv("RUN_TEST_VAR", "hello")
	p := &stubPlugin{}
	if err := Run([]string{"plugin"}, strings.NewReader(""), io.Discard, p); err != nil {
		t.Fatal(err)
	}
	if p.context.FileRoot == "" {
		t.Error("FileRoot should be set")
	}
	if got, ok := p.context.Env("RUN_TEST_VAR"); !ok || got != "hello" {
		t.Errorf("Env lookup failed: (%q, %v)", got, ok)
	}
}
