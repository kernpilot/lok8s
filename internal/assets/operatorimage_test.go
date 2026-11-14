package assets

// operatorimage_test.go covers the package sources of the operator image.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/testutil"
)

// The shell-operator base image names its vendor's own apk mirror, over
// plain HTTP. The final stage must write Alpine's CDN over HTTPS (main and
// community) to /etc/apk/repositories, once and with a single `>`, before
// its first apk call. Nothing else in the stage may add a package source or
// a signing key. The test reads text, not the shell: a write that a shell
// does not run (in a quoted string, say) can still fool it.
func TestOperatorImageApkUsesAlpineHTTPS(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join(testutil.RepoRoot(t), "operator/Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	// Comment lines go first (a comment may name apk), then the backslash
	// line continuations are folded, so an instruction is one line.
	var code []string
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			code = append(code, line)
		}
	}
	s := regexp.MustCompile(`\\\n[ \t]*`).ReplaceAllString(strings.Join(code, "\n"), " ")
	froms := regexp.MustCompile(`(?mi)^[ \t]*FROM[ \t]`).FindAllStringIndex(s, -1)
	if len(froms) == 0 {
		t.Fatal("operator/Dockerfile has no FROM line")
	}
	stage := s[froms[len(froms)-1][0]:]

	// Each apk call, with or without options before its subcommand.
	calls := regexp.MustCompile(`(^|[\s;&|(/])apk[ \t]`).FindAllStringIndex(stage, -1)
	if len(calls) == 0 {
		t.Fatal("the final stage of operator/Dockerfile has no apk call: change this test with the image")
	}
	const file = "/etc/apk/repositories"
	names := regexp.MustCompile(regexp.QuoteMeta(file)).FindAllStringIndex(stage, -1)
	if len(names) != 1 {
		t.Fatalf("the final stage of operator/Dockerfile names %s %d times, want one write", file, len(names))
	}
	w := names[0][0]
	if after := stage[w+len(file):]; after != "" && !regexp.MustCompile(`^[\s;&|)]`).MatchString(after) {
		t.Fatalf("the final stage of operator/Dockerfile writes another file than %s: %s%s", file, file, after[:min(len(after), 20)])
	}
	if !regexp.MustCompile(`(^|[^>])>[ \t]*$`).MatchString(stage[:w]) {
		t.Fatalf("the final stage of operator/Dockerfile does not write %s with a single >", file)
	}
	if calls[0][0] < w {
		t.Fatalf("the final stage of operator/Dockerfile calls apk before it writes %s", file)
	}
	// The instruction up to the write: the part that names the sources.
	start := strings.LastIndex(stage[:w], "\n") + 1
	head := stage[start : w+len(file)]
	if strings.Contains(head, "#") {
		t.Errorf("the write of %s has a # before it (a shell comment does not run): %s", file, head)
	}
	for _, repo := range []string{"main", "community"} {
		// The release as printf's %s, or written out (v3.22); then the end
		// of the line (\n), a quote or a space.
		if !regexp.MustCompile(`https://dl-cdn\.alpinelinux\.org/alpine/v(%s|[0-9]+\.[0-9]+)/` + repo + `(\\n|['"\s])`).MatchString(head) {
			t.Errorf("the write of %s does not name Alpine's %s repository over HTTPS: %s", file, repo, head)
		}
	}
	// Nothing in the stage may add a source or a key, or skip the signature.
	for _, bad := range []struct{ re, what string }{
		{`http://`, "a plain HTTP URL"},
		{`--repository\b|--repositories-file\b`, "an apk repository option"},
		{`(^|\s)-X[ \t=]`, "apk's -X (a repository)"},
		{`--allow-untrusted\b`, "apk's --allow-untrusted"},
		{`/etc/apk/keys`, "a change to apk's signing keys"},
	} {
		if m := regexp.MustCompile(bad.re).FindString(stage); m != "" {
			t.Errorf("the final stage of operator/Dockerfile has %s (%q)", bad.what, m)
		}
	}
}
