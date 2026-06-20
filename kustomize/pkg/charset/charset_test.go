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

func TestClass_Contains(t *testing.T) {
	cases := []struct {
		c    Class
		b    byte
		want bool
	}{
		{ClassUpper, 'A', true}, {ClassUpper, 'a', false}, {ClassUpper, '1', false}, {ClassUpper, '!', false},
		{ClassLower, 'a', true}, {ClassLower, 'A', false},
		{ClassDigit, '5', true}, {ClassDigit, 'a', false},
		{ClassSymbol, '!', true}, {ClassSymbol, '-', true}, {ClassSymbol, 'a', false}, {ClassSymbol, '0', false}, {ClassSymbol, 'Z', false},
	}
	for _, tc := range cases {
		if got := tc.c.Contains(tc.b); got != tc.want {
			t.Errorf("%s.Contains(%q) = %v, want %v", tc.c, tc.b, got, tc.want)
		}
	}
}

func TestParseClass(t *testing.T) {
	for _, c := range Classes() {
		if got, err := ParseClass(string(c)); err != nil || got != c {
			t.Errorf("ParseClass(%q) = %v, %v", c, got, err)
		}
	}
	if _, err := ParseClass("special"); err == nil {
		t.Error("ParseClass(special) should error")
	}
}

func TestPoolContains(t *testing.T) {
	alnum, _ := Resolve(NameAlphanum)
	if PoolContains(alnum, ClassSymbol) {
		t.Error("alphanum pool must not contain a symbol")
	}
	if !PoolContains(alnum, ClassDigit) || !PoolContains(alnum, ClassUpper) {
		t.Error("alphanum pool must contain digit + upper")
	}
	sym, _ := Resolve(NameAlphanumSymbols)
	if !PoolContains(sym, ClassSymbol) {
		t.Error("alphanum+symbols pool must contain a symbol")
	}
}

func TestSatisfiesAll(t *testing.T) {
	all := Classes()
	if !SatisfiesAll([]byte("aA1!"), all) {
		t.Error("aA1! should satisfy all four classes")
	}
	if SatisfiesAll([]byte("aaaaaa"), all) {
		t.Error("all-lowercase must not satisfy all classes")
	}
	if !SatisfiesAll([]byte("anything"), nil) {
		t.Error("an empty class list is trivially satisfied")
	}
}
