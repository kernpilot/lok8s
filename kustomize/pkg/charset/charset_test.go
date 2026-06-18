package charset

import (
	"strings"
	"testing"
)

func TestResolve_StandardNames(t *testing.T) {
	for _, name := range Names() {
		chars, err := Resolve(name)
		if err != nil {
			t.Errorf("Resolve(%q) error: %v", name, err)
		}
		if len(chars) == 0 {
			t.Errorf("Resolve(%q) returned empty", name)
		}
	}
}

func TestResolve_DefaultEmpty(t *testing.T) {
	chars, err := Resolve("")
	if err != nil {
		t.Fatal(err)
	}
	def, _ := Resolve(NameAlphanum)
	if chars != def {
		t.Errorf("empty charset should equal default alphanum")
	}
}

func TestResolve_AlphanumExcludesSymbols(t *testing.T) {
	chars, _ := Resolve(NameAlphanum)
	for _, c := range "!@#$%^&*-_=+" {
		if strings.ContainsRune(chars, c) {
			t.Errorf("alphanum should not contain %q", c)
		}
	}
}

func TestResolve_AlphanumSymbolsIncludesAlphanum(t *testing.T) {
	chars, _ := Resolve(NameAlphanumSymbols)
	for _, c := range "abc012XYZ" {
		if !strings.ContainsRune(chars, c) {
			t.Errorf("alphanum+symbols should contain %q", c)
		}
	}
	for _, c := range "!@#" {
		if !strings.ContainsRune(chars, c) {
			t.Errorf("alphanum+symbols should contain %q", c)
		}
	}
}

func TestResolve_Hex(t *testing.T) {
	chars, _ := Resolve(NameHex)
	if chars != "0123456789abcdef" {
		t.Errorf("hex = %q", chars)
	}
}

func TestResolve_Base64URL(t *testing.T) {
	chars, _ := Resolve(NameBase64URL)
	if !strings.Contains(chars, "-") || !strings.Contains(chars, "_") {
		t.Errorf("base64url should contain - and _")
	}
}

func TestResolve_Custom(t *testing.T) {
	chars, err := Resolve("custom:0123")
	if err != nil {
		t.Fatal(err)
	}
	if chars != "0123" {
		t.Errorf("custom = %q", chars)
	}
}

func TestResolve_CustomEmpty_Rejected(t *testing.T) {
	if _, err := Resolve("custom:"); err == nil {
		t.Error("empty custom should be rejected")
	}
}

func TestResolve_UnknownName_Rejected(t *testing.T) {
	if _, err := Resolve("nope"); err == nil {
		t.Error("unknown name should be rejected")
	}
}
