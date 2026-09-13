package kapply

// regex_test.go covers the Terminating-namespace extraction.

import (
	"testing"
)

func TestTerminatingNamespacesExtraction(t *testing.T) {
	t.Parallel()
	out := `Error from server (Forbidden): secrets "s" is forbidden: unable to create new content in namespace kubehz-system because it is being terminated
unable to create new content in namespace mla because it is being terminated
unable to create new content in namespace kubehz-system because it is being terminated`
	got := TerminatingNamespaces(out)
	want := []string{"kubehz-system", "mla"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
