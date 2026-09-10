package testutil

// drift.go is the one drift gate. Every "two copies must agree" test in
// the tree (the embedded mirror against its .lok8s twin, the pins against
// go.mod, the rendered CRDs against the committed files) builds two Trees
// and calls Drift.Check. The gate reports each difference with the command
// that resyncs it.

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Tree is one side of a drift comparison: a name for the messages and the
// entries it holds. A key is a slash-separated relative path (or any other
// identifier, such as a module path); a value is the content to compare.
type Tree struct {
	Name  string
	Files map[string]string
}

// Drift compares two Trees and fails the test on every difference.
type Drift struct {
	// Want is the canonical side. Got is the twin that must match it.
	Want, Got Tree
	// Sync names the command that brings Got in line with Want. It is
	// printed with every difference.
	Sync string
	// SyncBack names the command that brings Want in line with Got. It is
	// printed for an entry that exists only in Got. Empty means Sync.
	SyncBack string
	// Subset makes an entry that exists only in Got a non-difference.
	Subset bool
	// Max caps the number of differences reported. Zero means 20.
	Max int
}

// Diffs lists every difference, one message per entry, in key order.
func (d Drift) Diffs() []string {
	back := d.SyncBack
	if back == "" {
		back = d.Sync
	}
	keys := map[string]bool{}
	for k := range d.Want.Files {
		keys[k] = true
	}
	for k := range d.Got.Files {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	var out []string
	for _, k := range sorted {
		want, inWant := d.Want.Files[k]
		got, inGot := d.Got.Files[k]
		switch {
		case !inGot:
			out = append(out, fmt.Sprintf("%s: in %s but missing from %s (run: %s)", k, d.Want.Name, d.Got.Name, d.Sync))
		case !inWant:
			if !d.Subset {
				out = append(out, fmt.Sprintf("%s: in %s but not in %s (run: %s)", k, d.Got.Name, d.Want.Name, back))
			}
		case want != got:
			out = append(out, fmt.Sprintf("%s: %s differs from %s (run: %s)\n%s", k, d.Got.Name, d.Want.Name, d.Sync, FirstDiff(want, got)))
		}
	}
	return out
}

// Check reports the differences through t.Errorf, at most d.Max of them,
// and returns the total number found.
func (d Drift) Check(t *testing.T) int {
	t.Helper()
	limit := d.Max
	if limit == 0 {
		limit = 20
	}
	diffs := d.Diffs()
	for i, msg := range diffs {
		if i == limit {
			t.Errorf("%d more differences not shown (run: %s)", len(diffs)-limit, d.Sync)
			break
		}
		t.Error(msg)
	}
	return len(diffs)
}

// FirstDiff renders the first line where want and got differ.
func FirstDiff(want, got string) string {
	wl, gl := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(wl) || i < len(gl); i++ {
		var w, g string
		if i < len(wl) {
			w = wl[i]
		}
		if i < len(gl) {
			g = gl[i]
		}
		if w != g {
			return "line " + strconv.Itoa(i+1) + ":\n  want: " + w + "\n  got:  " + g
		}
	}
	return "(identical?)"
}

// ReadFS reads every file under fsys into a Tree keyed by slash path. skip,
// when set, drops the paths it returns true for.
func ReadFS(t *testing.T, name string, fsys fs.FS, skip func(rel string) bool) Tree {
	t.Helper()
	files := map[string]string{}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if skip != nil && skip(p) {
			return nil
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		files[p] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return Tree{Name: name, Files: files}
}

// ReadDir reads every file under dir into a Tree keyed by slash path
// relative to dir. skip, when set, drops the paths it returns true for.
func ReadDir(t *testing.T, name, dir string, skip func(rel string) bool) Tree {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if skip != nil && skip(rel) {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files[rel] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return Tree{Name: name, Files: files}
}

// Golden compares got with the golden file at path. With update set it
// rewrites the file with got instead and reports nothing. The message
// names the command that rewrites the golden.
func Golden(t *testing.T, path, got string, update bool) {
	t.Helper()
	if update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("golden %s: %v (run: go test -update)", path, err)
	}
	if want := string(raw); got != want {
		t.Errorf("%s: output differs from the golden (run: go test ./%s/ -update, then review the diff)\n%s\n--- want ---\n%s\n--- got ---\n%s",
			path, pkgDir(t), FirstDiff(want, got), want, got)
	}
}

// pkgDir is the package directory relative to the repo root, for the
// -update hint.
func pkgDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		return "<pkg>"
	}
	rel, err := filepath.Rel(RepoRoot(t), wd)
	if err != nil {
		return "<pkg>"
	}
	return filepath.ToSlash(rel)
}
