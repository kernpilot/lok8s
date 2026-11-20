package clidoc

import (
	"bytes"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/testutil"
)

// oldDocsSite is the address of the docs site before it moved to
// https://lok8s.io/ (docs/public/CNAME). The constant has two parts, so
// this file does not hold the text that the test looks for.
const oldDocsSite = "kernpilot.github.io" + "/lok8s"

// oldDocsSiteSkips are the paths that the walk does not read. A key with a
// slash is a path from the repository root. A key without a slash is a
// file or directory name at any depth. Git does not track these paths, with
// one exception: docs/releases.
var oldDocsSiteSkips = map[string]string{
	".git":                  "the git store",
	"node_modules":          "installed packages",
	".claude":               "local agent notes",
	".secrets":              "a credential store: the test does not open it",
	".kubeconfig":           "credentials: the test does not open them",
	".env":                  "credentials: the test does not open them",
	"docs/releases":         "published release notes keep the links of their release",
	"docs/.vitepress/dist":  "the docs build output, which renders docs/releases",
	"docs/.vitepress/cache": "the docs build cache",
}

// oldDocsSiteExceptions are the files that can still hold the old address,
// each with its reason. An entry whose file does not hold the address fails
// the test, so that the list cannot go stale in silence.
var oldDocsSiteExceptions = map[string]string{
	".lok8s/drivers/lo/utils/registries.sh":                "changes in an open PR; remove this entry when it merges",
	"internal/assets/lok8s/drivers/lo/utils/registries.sh": "changes in an open PR; remove this entry when it merges",
	"internal/driver/lo/registries.go":                     "changes in an open PR; remove this entry when it merges",
}

// minDocsSiteFiles is a floor for the number of text files that the walk
// reads. The repository has more than 2000. A wrong root or a skip that
// drops too much reads fewer, and an empty read proves nothing.
const minDocsSiteFiles = 1000

// TestNoLinkToTheOldDocsSite: no file in the repository links to the old
// docs site, except the release notes and the files in
// oldDocsSiteExceptions. The test walks the working tree, because git is
// not available in a test. Binary files (a NUL byte in the first 8000
// bytes) and symbolic links are not read.
func TestNoLinkToTheOldDocsSite(t *testing.T) {
	t.Parallel()
	root := testutil.RepoRoot(t)
	held := map[string]bool{}
	read := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if _, skip := oldDocsSiteSkips[rel]; skip {
			return skipEntry(d)
		}
		if _, skip := oldDocsSiteSkips[d.Name()]; skip && rel != "." {
			return skipEntry(d)
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		text, ok, err := readText(path)
		if err != nil || !ok {
			return err
		}
		read++
		if !strings.Contains(text, oldDocsSite) {
			return nil
		}
		held[rel] = true
		if _, ok := oldDocsSiteExceptions[rel]; ok {
			return nil
		}
		for i, line := range strings.Split(text, "\n") {
			if strings.Contains(line, oldDocsSite) {
				t.Errorf("%s:%d links to the old docs site %s: link the same page on https://lok8s.io/ (no /lok8s prefix)", rel, i+1, oldDocsSite)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for rel, reason := range oldDocsSiteExceptions {
		if !held[rel] {
			t.Errorf("%s does not hold %s, or the walk does not read it, but oldDocsSiteExceptions names it (%s): remove its entry", rel, oldDocsSite, reason)
		}
	}
	if read < minDocsSiteFiles {
		t.Errorf("the walk read %d text files, want at least %d: check the root and oldDocsSiteSkips", read, minDocsSiteFiles)
	}
}

// skipEntry makes the walk step over d: its whole tree for a directory,
// only d for a file.
func skipEntry(d fs.DirEntry) error {
	if d.IsDir() {
		return filepath.SkipDir
	}
	return nil
}

// readText reads a file. It gives ok=false for a binary file, which has a
// NUL byte in the first 8000 bytes, and then does not read the remaining
// bytes.
func readText(path string) (text string, ok bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	head := make([]byte, 8000)
	n, err := io.ReadFull(f, head)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return "", false, err
	}
	head = head[:n]
	if bytes.IndexByte(head, 0) >= 0 {
		return "", false, nil
	}
	rest, err := io.ReadAll(f)
	if err != nil {
		return "", false, err
	}
	return string(head) + string(rest), true, nil
}
