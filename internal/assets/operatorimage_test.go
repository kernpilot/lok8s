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
// plain HTTP. The final stage must write exactly Alpine's CDN over HTTPS
// (main and community) to /etc/apk/repositories, before its first apk
// call. The test allows, not denies: one fixed write, no other mention of
// /etc/apk or repositories, and apk calls only as `apk upgrade --no-cache`
// and `apk add --no-cache <packages>`. Any other form fails (also a quoted
// path, a tee, a heredoc or an escape directive), so a change to the
// image's package setup changes this test with it. It reads text, not the
// shell: a call through a variable, or a relative path from another
// directory, can still get past it.
func TestOperatorImageApkUsesAlpineHTTPS(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join(testutil.RepoRoot(t), "operator/Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if regexp.MustCompile(`(?mi)^[ \t]*#[ \t]*escape[ \t]*=`).MatchString(text) {
		t.Fatal("operator/Dockerfile sets an escape directive: this test folds only backslash line continuations")
	}
	// Comment lines go first (a comment may name apk), then the backslash
	// line continuations are folded, so an instruction is one line. Each run
	// of blanks becomes one space (the write's quoted text has no blank).
	var code []string
	for line := range strings.SplitSeq(text, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			code = append(code, line)
		}
	}
	s := regexp.MustCompile(`[ \t]*\\[ \t]*\n[ \t]*`).ReplaceAllString(strings.Join(code, "\n"), " ")
	s = regexp.MustCompile(`[ \t]+`).ReplaceAllString(s, " ")
	// A heredoc body line can start with "from" and read as a stage.
	if strings.Contains(s, "<<") {
		t.Fatal("operator/Dockerfile uses a heredoc (<<): this test reads instructions line by line")
	}
	froms := regexp.MustCompile(`(?mi)^[ \t]*FROM[ \t]+(?:--platform=\S+[ \t]+)?(\S+)(?:[ \t]+AS[ \t]+(\S+))?`).FindAllStringSubmatchIndex(s, -1)
	if len(froms) == 0 {
		t.Fatal("operator/Dockerfile has no FROM line")
	}
	last := froms[len(froms)-1]
	image := strings.ToLower(s[last[2]:last[3]])
	for _, f := range froms[:len(froms)-1] {
		if f[4] >= 0 && strings.ToLower(s[f[4]:f[5]]) == image {
			t.Fatalf("the final stage of operator/Dockerfile builds on the stage %s: this test reads only the final stage", image)
		}
	}
	stage := s[last[0]:]

	const write = `printf 'https://dl-cdn.alpinelinux.org/alpine/v%s/main\nhttps://dl-cdn.alpinelinux.org/alpine/v%s/community\n' "${v}" "${v}" > /etc/apk/repositories`
	if n := strings.Count(stage, write); n != 1 {
		t.Fatalf("the final stage of operator/Dockerfile has %d copies of the write, want exactly one:\n  %s", n, write)
	}
	w := strings.Index(stage, write)
	// The write's RUN sets v first, from the base image's Alpine release.
	head := regexp.MustCompile(`^[ \t]*(?i:RUN)(?:[ \t]+--\S+)*[ \t]+` + regexp.QuoteMeta(`v="$(cut -d. -f1,2 /etc/alpine-release)"`) + `[ \t]+&&[ \t]+$`)
	if h := stage[strings.LastIndex(stage[:w], "\n")+1 : w]; !head.MatchString(h) {
		t.Errorf("the write's instruction must start with RUN v=\"$(cut -d. -f1,2 /etc/alpine-release)\" &&, got: %s", h)
	}
	rest := stage[:w] + stage[w+len(write):]
	for _, word := range []string{"/etc/apk", "apk/", "repositories"} {
		if strings.Contains(rest, word) {
			t.Errorf("the final stage of operator/Dockerfile names %s outside the one write", word)
		}
	}

	// Each apk call: the word apk (also /sbin/apk or apk.static) after a
	// separator or a quote. Its arguments run to the next shell operator.
	call := regexp.MustCompile(`(?:^|[\s;&|("'\[,])(?:\S*/s?bin/)?apk(?:\.static)?(?:[\s"',\]]|$)`)
	end := regexp.MustCompile("[;|&\n)<>`]")
	allowed := regexp.MustCompile(`^(upgrade --no-cache|add --no-cache( [a-z0-9][a-z0-9._+-]*)+)$`)
	calls := call.FindAllStringIndex(stage, -1)
	if len(calls) == 0 {
		t.Fatal("the final stage of operator/Dockerfile has no apk call: change this test with the image")
	}
	for _, c := range calls {
		if strings.ContainsAny(stage[c[1]-1:c[1]], `"',]`) {
			t.Errorf("the final stage of operator/Dockerfile calls apk in a quoted or exec form: %q", stage[c[0]:min(len(stage), c[1]+30)])
			continue
		}
		args := stage[c[1]:]
		if e := end.FindStringIndex(args); e != nil {
			args = args[:e[0]]
		}
		args = strings.Join(strings.Fields(args), " ")
		switch {
		case c[0] < w:
			t.Errorf("the final stage of operator/Dockerfile calls apk before it writes /etc/apk/repositories: apk %s", args)
		case !allowed.MatchString(args):
			t.Errorf("the final stage of operator/Dockerfile calls apk in a form this test does not allow: apk %s", args)
		}
	}
}
