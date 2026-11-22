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

// WriteFailure puts the prefix on each line of the error: a refusal of more
// lines keeps it on each line, and a text of one line prints as Fail
// printed it before (one line, the prefix, a space, the text). An empty
// line (a trailing newline, a blank line, a line of spaces) prints
// nothing, and a text with no other line prints the prefix once.
func TestWriteFailurePrefixesEachLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"one line", errors.New("line 6:13: passwd.PASSWORD: cannot unmarshal"), "secret plugin: line 6:13: passwd.PASSWORD: cannot unmarshal\n"},
		{"two lines", errors.New("s/.gitignore:9: a cause\ns/.gitignore was not changed"), "secret plugin: s/.gitignore:9: a cause\nsecret plugin: s/.gitignore was not changed\n"},
		{"an empty text", errors.New(""), "secret plugin: \n"},
		{"a trailing newline", errors.New("a cause\n"), "secret plugin: a cause\n"},
		{"a blank line", errors.New("a cause\n\n  the next step\n \r\n"), "secret plugin: a cause\nsecret plugin:   the next step\n"},
		{"empty lines only", errors.New("\n\n"), "secret plugin: \n"},
	} {
		var b bytes.Buffer
		WriteFailure(&b, tc.err)
		if b.String() != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, b.String(), tc.want)
		}
	}
}
