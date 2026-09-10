package testutil

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestDriftDiffsClassifyEveryEntry(t *testing.T) {
	t.Parallel()
	d := Drift{
		Want:     Tree{Name: "embed", Files: map[string]string{"a": "1\n2\n", "b": "same", "c": "x"}},
		Got:      Tree{Name: "disk", Files: map[string]string{"a": "1\n3\n", "b": "same", "d": "y"}},
		Sync:     "sync",
		SyncBack: "sync --back",
	}
	got := d.Diffs()
	want := []string{
		"a: disk differs from embed (run: sync)\nline 2:\n  want: 2\n  got:  3",
		"c: in embed but missing from disk (run: sync)",
		"d: in disk but not in embed (run: sync --back)",
	}
	if strings.Join(got, "\n---\n") != strings.Join(want, "\n---\n") {
		t.Errorf("diffs:\n%s\nwant:\n%s", strings.Join(got, "\n---\n"), strings.Join(want, "\n---\n"))
	}
	d.Subset = true
	if got := d.Diffs(); len(got) != 2 || strings.HasPrefix(got[len(got)-1], "d:") {
		t.Errorf("Subset must drop the entry only in Got: %v", got)
	}
	d.SyncBack = ""
	d.Subset = false
	if got := d.Diffs(); !strings.HasSuffix(got[2], "(run: sync)") {
		t.Errorf("empty SyncBack must fall back to Sync: %q", got[2])
	}
}

func TestDriftCheckPassesOnIdenticalTrees(t *testing.T) {
	t.Parallel()
	d := Drift{
		Want: Tree{Name: "a", Files: map[string]string{"x": "1", "y": "2"}},
		Got:  Tree{Name: "b", Files: map[string]string{"x": "1", "y": "2"}},
	}
	if n := d.Check(t); n != 0 {
		t.Errorf("Check = %d, want 0", n)
	}
}

func TestFirstDiffNamesTheLine(t *testing.T) {
	t.Parallel()
	if got := FirstDiff("a\nb\nc", "a\nb\nd"); got != "line 3:\n  want: c\n  got:  d" {
		t.Errorf("FirstDiff = %q", got)
	}
	if got := FirstDiff("a", "a\nb"); got != "line 2:\n  want: \n  got:  b" {
		t.Errorf("FirstDiff (extra line) = %q", got)
	}
	if got := FirstDiff("same", "same"); got != "(identical?)" {
		t.Errorf("FirstDiff (equal) = %q", got)
	}
}

func TestReadDirAndReadFSKeyBySlashPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	WriteFile(t, dir+"/sub/one.txt", "1")
	WriteFile(t, dir+"/two.txt", "2")
	WriteFile(t, dir+"/sub/.lo-origin", "marker")
	tree := ReadDir(t, "disk", dir, func(rel string) bool { return strings.HasSuffix(rel, ".lo-origin") })
	if len(tree.Files) != 2 || tree.Files["sub/one.txt"] != "1" || tree.Files["two.txt"] != "2" {
		t.Errorf("ReadDir = %v", tree.Files)
	}
	embedded := ReadFS(t, "embed", fstest.MapFS{"sub/one.txt": {Data: []byte("1")}, "two.txt": {Data: []byte("2")}}, nil)
	if n := (Drift{Want: embedded, Got: tree}).Check(t); n != 0 {
		t.Errorf("ReadFS and ReadDir disagree on the same layout: %d differences", n)
	}
}

func TestGoldenUpdateRewritesThenMatches(t *testing.T) {
	t.Parallel()
	path := t.TempDir() + "/g/out.golden"
	Golden(t, path, "fresh\n", true)
	Golden(t, path, "fresh\n", false)
	if t.Failed() {
		t.Fatal("a rewritten golden must match its own output")
	}
}
