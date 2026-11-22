package clidoc

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/testutil"
)

// oldDocsSite is the address of the docs site before it moved to
// https://lok8s.io/ (docs/public/CNAME), in lower case. The constant has
// two parts, so this file does not hold the text that the test looks for.
const oldDocsSite = "kernpilot.github.io" + "/lok8s"

// movedLinkFiles are the files whose links moved from the old docs site
// to lok8s.io. The test fails when it does not read one of them: then the
// file list or the read is wrong, and the test checks less than it seems.
var movedLinkFiles = []string{
	".github/ISSUE_TEMPLATE/config.yml",
	".lok8s/drivers/README.md",
	".lok8s/drivers/lo/utils/registries.sh",
	".lok8s/providers/README.md",
	"clusters/lok8s.dev/targets/README.md",
	"internal/assets/lok8s/drivers/README.md",
	"internal/assets/lok8s/drivers/lo/utils/registries.sh",
	"internal/assets/lok8s/providers/README.md",
	"internal/driver/lo/registries.go",
}

// minDocsSiteFiles is a floor for the number of text files that the test
// reads. The repository has more than 2000. A wrong root or a read that
// drops too much reads fewer, and an empty read proves nothing.
const minDocsSiteFiles = 1000

// TestNoLinkToTheOldDocsSite: no tracked file links to the old docs site,
// in any case. The test reads the files that `git ls-files` lists, so an
// untracked copy (for example the bash tree that an e2e leg leaves below
// tests/e2e/) does not count. It does not read binary files (a NUL byte in
// the first 8000 bytes), symbolic links or submodules.
func TestNoLinkToTheOldDocsSite(t *testing.T) {
	t.Parallel()
	root := testutil.RepoRoot(t)
	// Outside a repository the test cannot run: skip locally, FAIL under
	// CI=true (a silent skip there would pass an old link).
	skip := func(format string, a ...any) {
		if os.Getenv("CI") != "" {
			t.Fatalf("CI=true: "+format, a...)
		}
		t.Skipf(format, a...)
	}
	git, err := exec.LookPath("git")
	if err != nil {
		skip("git not on PATH")
	}
	out, err := exec.Command(git, "-C", root, "ls-files", "-z").Output()
	if err != nil || len(out) == 0 {
		skip("git ls-files: %v (empty output)", err)
	}
	read := map[string]bool{}
	for rel := range strings.SplitSeq(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		text, ok, err := readText(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("%s: cannot read the file: %v", rel, err)
			continue
		}
		if !ok {
			continue
		}
		read[rel] = true
		for i, line := range strings.Split(text, "\n") {
			if strings.Contains(strings.ToLower(line), oldDocsSite) {
				t.Errorf("%s:%d links to the old docs site %s: link the same page on https://lok8s.io/ (no /lok8s prefix)", rel, i+1, oldDocsSite)
			}
		}
	}
	for _, rel := range movedLinkFiles {
		if !read[rel] {
			t.Errorf("%s: the test did not read this file, but movedLinkFiles names it: check the read, or remove the entry if the file is gone", rel)
		}
	}
	if len(read) < minDocsSiteFiles {
		t.Errorf("the test read %d text files, want at least %d: check the root and the read", len(read), minDocsSiteFiles)
	}
}

// readText reads a regular file. It gives ok=false for a binary file (a
// NUL byte in the first 8000 bytes), a symbolic link and a directory (a
// submodule).
func readText(path string) (text string, ok bool, err error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", false, err
	}
	if !info.Mode().IsRegular() {
		return "", false, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false, err
	}
	if bytes.IndexByte(raw[:min(len(raw), 8000)], 0) >= 0 {
		return "", false, nil
	}
	return string(raw), true, nil
}
