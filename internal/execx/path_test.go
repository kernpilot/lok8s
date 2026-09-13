package execx

import (
	"os"
	"strings"
	"testing"
)

func TestPrependPATH(t *testing.T) {
	sep := string(os.PathListSeparator)
	t.Setenv("PATH", "/usr/bin"+sep+"/p/.bin")
	// Order kept, present entries not duplicated, empty dirs skipped.
	got := PrependPATH("/p/.lok8s", "", "/p/.bin")
	if got != "/p/.lok8s"+sep+"/usr/bin"+sep+"/p/.bin" {
		t.Fatalf("PATH = %q", got)
	}
	got = PrependPATH("/a", "/b")
	if !strings.HasPrefix(got, "/a"+sep+"/b"+sep) || strings.Count(got, "/p/.bin") != 1 {
		t.Fatalf("PATH = %q", got)
	}
	if PrependPATH() != os.Getenv("PATH") {
		t.Fatal("no dirs must leave PATH as is")
	}
}
