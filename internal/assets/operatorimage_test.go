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
// community) to /etc/apk/repositories before its first apk call.
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
	apk := regexp.MustCompile(`\bapk[ \t]+(add|upgrade|update|fix)\b`).FindStringIndex(stage)
	if apk == nil {
		t.Fatal("the final stage of operator/Dockerfile has no apk call: change this test with the image")
	}
	w := strings.Index(stage, "> /etc/apk/repositories")
	if w < 0 || w > apk[0] {
		t.Fatal("the final stage of operator/Dockerfile calls apk before it writes /etc/apk/repositories")
	}
	start := strings.LastIndex(stage[:w], "\n") + 1
	// The instruction up to the write: the part that names the sources.
	head := stage[start : w+len("> /etc/apk/repositories")]
	for _, repo := range []string{"main", "community"} {
		// The release as printf's %s, or written out (v3.22).
		if !regexp.MustCompile(`https://dl-cdn\.alpinelinux\.org/alpine/v(%s|[0-9]+\.[0-9]+)/` + repo + `\b`).MatchString(head) {
			t.Errorf("the write of /etc/apk/repositories does not name Alpine's %s repository over HTTPS: %s", repo, head)
		}
	}
	if strings.Contains(head, "http://") {
		t.Errorf("the write of /etc/apk/repositories names a plain HTTP source: %s", head)
	}
}
