package errs

import (
	"errors"
	"strings"
	"testing"
)

func TestError_Format(t *testing.T) {
	cases := []struct {
		name string
		err  *Error
		want string
	}{
		{
			name: "msg only",
			err:  &Error{Msg: "boom"},
			want: "boom",
		},
		{
			name: "with path",
			err:  &Error{Path: "passwd.FOO", Msg: "length must be > 0"},
			want: "passwd.FOO: length must be > 0",
		},
		{
			name: "with line",
			err:  &Error{Line: 14, Msg: "unknown field"},
			want: "line 14: unknown field",
		},
		{
			name: "with line and col",
			err:  &Error{Line: 14, Col: 3, Msg: "unknown field"},
			want: "line 14:3: unknown field",
		},
		{
			name: "with line, path, and msg",
			err:  &Error{Line: 14, Path: "passwd.FOO", Msg: "length must be > 0"},
			want: "line 14: passwd.FOO: length must be > 0",
		},
		{
			name: "with wrapped error appended",
			err:  &Error{Path: "file.tls.crt", Msg: "open failed", Err: errors.New("file not found")},
			want: "file.tls.crt: open failed: file not found",
		},
		{
			name: "wrapped error equal to msg is not duplicated",
			err:  &Error{Msg: "boom", Err: errors.New("boom")},
			want: "boom",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Errorf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNew_AndNewf(t *testing.T) {
	if New("hello").Error() != "hello" {
		t.Error("New failed")
	}
	if Newf("hello %d", 42).Error() != "hello 42" {
		t.Error("Newf failed")
	}
}

func TestAt_AndAtf(t *testing.T) {
	if At(10, 0, "msg").Error() != "line 10: msg" {
		t.Error("At failed")
	}
	if At(10, 5, "msg").Error() != "line 10:5: msg" {
		t.Error("At with col failed")
	}
	if Atf(10, 0, "value=%d", 7).Error() != "line 10: value=7" {
		t.Error("Atf failed")
	}
}

func TestWrap_NilReturnsNil(t *testing.T) {
	if Wrap("path", nil) != nil {
		t.Error("Wrap(nil) should return nil")
	}
}

func TestWrap_PlainError(t *testing.T) {
	plain := errors.New("boom")
	wrapped := Wrap("passwd.FOO", plain)
	got := wrapped.Error()
	if got != "passwd.FOO: boom" {
		t.Errorf("Wrap = %q", got)
	}
	// errors.Is/As traversal must still find the original.
	if !errors.Is(wrapped, plain) {
		t.Error("errors.Is should reach the wrapped error")
	}
}

func TestWrap_OurErrorPrependsPath(t *testing.T) {
	inner := &Error{Path: "length", Msg: "must be > 0"}
	wrapped := Wrap("passwd.FOO", inner)
	if wrapped.Error() != "passwd.FOO.length: must be > 0" {
		t.Errorf("Wrap nested = %q", wrapped.Error())
	}
}

func TestWrap_OurErrorWithoutPath(t *testing.T) {
	inner := &Error{Msg: "boom"}
	wrapped := Wrap("passwd.FOO", inner)
	if wrapped.Error() != "passwd.FOO: boom" {
		t.Errorf("Wrap nested no inner path = %q", wrapped.Error())
	}
}

func TestWithPath_NilReturnsNil(t *testing.T) {
	if WithPath(nil, "x") != nil {
		t.Error("WithPath(nil) should return nil")
	}
}

func TestWithPath_PlainError(t *testing.T) {
	plain := errors.New("boom")
	got := WithPath(plain, "passwd.FOO")
	if got.Error() != "passwd.FOO: boom" {
		t.Errorf("WithPath = %q", got.Error())
	}
}

func TestWithPath_OurError(t *testing.T) {
	inner := &Error{Path: "wrong", Msg: "boom"}
	got := WithPath(inner, "right")
	if got.Error() != "right: boom" {
		t.Errorf("WithPath replace = %q", got.Error())
	}
	// Ensure we copied (didn't mutate inner).
	if inner.Path != "wrong" {
		t.Errorf("inner.Path mutated to %q", inner.Path)
	}
}

func TestMulti_Empty(t *testing.T) {
	var m Multi
	if m.Err() != nil {
		t.Error("empty Multi.Err should be nil")
	}
	if m.Error() != "" {
		t.Error("empty Multi.Error should be empty")
	}
}

func TestMulti_AddNilIgnored(t *testing.T) {
	var m Multi
	m.Add(nil)
	m.Add(nil)
	if m.Err() != nil {
		t.Error("Multi with only nils should be nil")
	}
}

func TestMulti_Single(t *testing.T) {
	var m Multi
	m.Add(errors.New("one"))
	if m.Error() != "one" {
		t.Errorf("Multi single = %q", m.Error())
	}
	if m.Err() == nil {
		t.Error("Multi.Err with one error should not be nil")
	}
}

func TestMulti_MultipleSorted(t *testing.T) {
	var m Multi
	m.Add(errors.New("c"))
	m.Add(errors.New("a"))
	m.Add(errors.New("b"))
	got := m.Error()
	if !strings.Contains(got, "a") || !strings.Contains(got, "b") || !strings.Contains(got, "c") {
		t.Errorf("Multi missing entries: %q", got)
	}
	// Sorted order: a < b < c
	if got != "a\nb\nc" {
		t.Errorf("Multi order = %q, want sorted", got)
	}
}
