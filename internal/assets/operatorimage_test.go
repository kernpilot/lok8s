package assets

// operatorimage_test.go covers the sources of the operator image: its apk
// packages, its base image and the tools it downloads.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/testutil"
)

// operatorFinalStage reads operator/Dockerfile and returns its final stage,
// one instruction per line, and the image of the final FROM (lower case).
// It stops the test on a form it cannot read: an escape directive, a
// heredoc, an ARG in the final FROM, or a final stage that builds on an
// earlier stage.
func operatorFinalStage(t *testing.T) (stage, image string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(testutil.RepoRoot(t), "operator/Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if regexp.MustCompile(`(?mi)^[ \t]*#[ \t]*escape[ \t]*=`).MatchString(text) {
		t.Fatal("operator/Dockerfile sets an escape directive: this test folds only backslash line continuations")
	}
	// Comment lines are removed first (a comment may name a tool), then the
	// backslash line continuations are folded, so an instruction is one
	// line. A blank line inside a continuation does not end it (BuildKit
	// skips it). Each run of blanks becomes one space, also inside quotes:
	// the apk write's quoted text has no blank, and a checksum line
	// `echo "${X}  <path>"` reads with one space.
	var code []string
	for line := range strings.SplitSeq(text, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			code = append(code, line)
		}
	}
	s := regexp.MustCompile(`[ \t]*\\[ \t]*\n(?:[ \t]*\n)*[ \t]*`).ReplaceAllString(strings.Join(code, "\n"), " ")
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
	image = strings.ToLower(s[last[2]:last[3]])
	if strings.Contains(image, "$") {
		t.Fatalf("the final FROM of operator/Dockerfile names its base with an ARG (%s): this test cannot tell if that is an earlier stage", image)
	}
	for _, f := range froms[:len(froms)-1] {
		if f[4] >= 0 && strings.ToLower(s[f[4]:f[5]]) == image {
			t.Fatalf("the final stage of operator/Dockerfile builds on the stage %s: this test reads only the final stage", image)
		}
	}
	return s[last[0]:], image
}

// The shell-operator base image names its vendor's own apk mirror, over
// plain HTTP. The final stage must write exactly Alpine's CDN over HTTPS
// (main and community) to /etc/apk/repositories, before its first apk
// call. The test is an allow list:
//   - one fixed write, and no other mention of /etc/apk or repositories;
//   - no SSL_NO_VERIFY or APK_CONFIG;
//   - apk calls only as `apk upgrade --no-cache` and
//     `apk add --no-cache <packages>` (names from the index, not .apk
//     files).
//
// Any other form fails (also a quoted path, a tee, a redirect after apk, a
// heredoc, an escape directive or an ARG in the final FROM), so a change to
// the image's package setup changes this test with it.
//
// The test reads text, not the shell. These can still get past it:
//   - a call through a variable, or by a path outside a bin directory
//     (such as ./apk);
//   - a write by a glob or a relative path (cd /etc, then ap*/repo*);
//   - a file that COPY or ADD brings in, also a binary that an earlier
//     stage built with its own apk sources (COPY --from);
//   - a CA or a proxy that the build adds;
//   - another tool or a fetched script that writes the file (such as
//     setup-apkrepos);
//   - a base image that ships its own /etc/apk/repositories.d/*.list
//     files, which apk also reads. Check for them when the base changes.
func TestOperatorImageApkUsesAlpineHTTPS(t *testing.T) {
	t.Parallel()
	stage, _ := operatorFinalStage(t)

	const write = `printf 'https://dl-cdn.alpinelinux.org/alpine/v%s/main\nhttps://dl-cdn.alpinelinux.org/alpine/v%s/community\n' "${v}" "${v}" > /etc/apk/repositories`
	// The path ends at a blank or a shell operator: a longer path (such as
	// repositories.d/x.list) is another file.
	writes := regexp.MustCompile(regexp.QuoteMeta(write)+`(?:[\s;&|)]|$)`).FindAllStringIndex(stage, -1)
	if len(writes) != 1 {
		t.Fatalf("the final stage of operator/Dockerfile has %d copies of the write, want exactly one:\n  %s", len(writes), write)
	}
	w := writes[0][0]
	// The write's RUN sets v first, from the base image's Alpine release.
	head := regexp.MustCompile(`^[ \t]*(?i:RUN)(?:[ \t]+--\S+)*[ \t]+` + regexp.QuoteMeta(`v="$(cut -d. -f1,2 /etc/alpine-release)"`) + `[ \t]+&&[ \t]+$`)
	if h := stage[strings.LastIndex(stage[:w], "\n")+1 : w]; !head.MatchString(h) {
		t.Errorf("the write's instruction must start with RUN v=\"$(cut -d. -f1,2 /etc/alpine-release)\" &&, got: %s", h)
	}
	rest := stage[:w] + stage[w+len(write):]
	for _, word := range []string{"/etc/apk", "apk/", "repositories"} {
		if strings.Contains(rest, word) {
			t.Errorf("the final stage of operator/Dockerfile names %s outside the one write: this test allows no other path with that name", word)
		}
	}
	// These change how apk checks TLS, or which options file it reads.
	for _, word := range []string{"SSL_NO_VERIFY", "APK_CONFIG"} {
		if strings.Contains(stage, word) {
			t.Errorf("the final stage of operator/Dockerfile names %s: it changes how apk fetches", word)
		}
	}

	// Each apk call: the word apk (also /sbin/apk or apk.static) after a
	// separator, a quote or a backslash. Its arguments run to the next
	// command separator: a redirect is part of them, so options after it
	// count.
	call := regexp.MustCompile("(?:[\\s;&|(\"'\\[,`\\\\])(?:\\S*/s?bin/)?apk(?:\\.static)?(?:[\\s\"',\\]`]|$)")
	end := regexp.MustCompile("[;|&\n)`]")
	allowed := regexp.MustCompile(`^(upgrade --no-cache|add --no-cache( [a-z0-9][a-z0-9._+-]*([=~][a-z0-9._+-]+)?)+)$`)
	calls := call.FindAllStringIndex(stage, -1)
	if len(calls) == 0 {
		t.Fatal("the final stage of operator/Dockerfile has no apk call: change this test with the image")
	}
	for _, c := range calls {
		if strings.ContainsAny(stage[c[1]-1:c[1]], "\"',]`") {
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
			t.Errorf("the final stage of operator/Dockerfile calls or names apk before it writes /etc/apk/repositories: apk %s", args)
		case strings.Contains(args, ".apk"):
			t.Errorf("the final stage of operator/Dockerfile names an .apk file, which apk reads as a local file, not from the index: apk %s", args)
		case !allowed.MatchString(args):
			t.Errorf("the final stage of operator/Dockerfile calls apk in a form this test does not allow (only upgrade --no-cache and add --no-cache <packages>): apk %s", args)
		}
	}
}

// The final stage builds on the shell-operator image and downloads the tools
// that are not Alpine packages. Both come from outside, so both are pinned
// to their bytes:
//   - the final FROM names shell-operator by its tag and by the digest of
//     its multi-arch index (ghcr.io/flant/shell-operator:<tag>@sha256:<hex>);
//   - each download has the exact form
//     wget -qO <path> "https://<url>" && echo "${<NAME>}  <path>" | sha256sum -c - &&
//     so the check is the next command, before any use of the file, and a
//     failed check stops the RUN;
//   - each <NAME> is set to 64 hex digits in the amd64 and the arm64 branch
//     (the platforms docker.yml builds) of the one `case "${ARCH}" in`, and
//     nowhere else; the case ends with a `*)` branch that exits 1;
//   - each download has its own path and its own <NAME>;
//   - the final stage has no curl and no ADD (each fetches with no check).
//
// The test reads text, not the shell. It checks that each download has a
// check, not that a sum is right: a wrong sum fails the build. These can
// still get past it:
//   - a download by another tool, through a variable, or by a script;
//   - a <NAME> that the shell sets in another form (export, read, a loop);
//   - a file that an earlier stage fetched (COPY --from).
func TestOperatorImageDownloadsAreChecked(t *testing.T) {
	t.Parallel()
	stage, image := operatorFinalStage(t)

	base := regexp.MustCompile(`^ghcr\.io/flant/shell-operator:v[0-9][0-9a-z.+-]*@sha256:[0-9a-f]{64}$`)
	if !base.MatchString(image) {
		t.Errorf("the final FROM of operator/Dockerfile must name shell-operator by its tag and the digest of its multi-arch index (<image>:<tag>@sha256:<64 hex>), got %s", image)
	}
	if regexp.MustCompile(`(?:^|[\s;&|(])curl(?:\s|$)`).MatchString(stage) {
		t.Error("the final stage of operator/Dockerfile calls curl: download with wget and check the file with sha256sum -c")
	}
	if regexp.MustCompile(`(?mi)^[ \t]*ADD[ \t]`).MatchString(stage) {
		t.Error("the final stage of operator/Dockerfile has an ADD, which can fetch a URL or unpack an archive with no check: use COPY, or wget and sha256sum -c")
	}

	// The sums: one case on ARCH, a branch for each built platform.
	cases := regexp.MustCompile(`case "\$\{ARCH\}" in (.*?) esac`).FindAllStringSubmatch(stage, -1)
	if len(cases) != 1 {
		t.Fatalf("the final stage of operator/Dockerfile has %d `case \"${ARCH}\" in ... esac`, want exactly one with the download sums", len(cases))
	}
	sums := map[string]map[string]bool{} // NAME -> the arches that set it
	seen := map[string]bool{}
	assign := regexp.MustCompile(`^([A-Z][A-Z0-9_]*)=([0-9a-f]{64})$`)
	for part := range strings.SplitSeq(cases[0][1], ";;") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		label, body, ok := strings.Cut(part, ")")
		label, body = strings.TrimSpace(label), strings.TrimSpace(body)
		switch {
		case !ok:
			t.Errorf("the ARCH case of operator/Dockerfile has a branch this test cannot read: %s", part)
		case label == "amd64" || label == "arm64":
			seen[label] = true
			for f := range strings.FieldsSeq(body) {
				m := assign.FindStringSubmatch(f)
				if m == nil {
					t.Errorf("the %s branch of the ARCH case may only set NAME=<64 hex>, got: %s", label, f)
					continue
				}
				if sums[m[1]] == nil {
					sums[m[1]] = map[string]bool{}
				}
				sums[m[1]][label] = true
			}
		case label == "*":
			seen[label] = true
			if !regexp.MustCompile(`(?:^|[;\s])exit 1$`).MatchString(body) {
				t.Errorf("the *) branch of the ARCH case must end with exit 1 (an architecture with no sums fails the build), got: %s", body)
			}
		default:
			t.Errorf("the ARCH case of operator/Dockerfile has the branch %s): this test knows amd64, arm64 and *", label)
		}
	}
	for _, label := range []string{"amd64", "arm64", "*"} {
		if !seen[label] {
			t.Errorf("the ARCH case of operator/Dockerfile has no %s) branch", label)
		}
	}

	// Each download, and its check right after it.
	calls := regexp.MustCompile(`(?:^|[\s;&|(])wget(?:\s|$)`).FindAllStringIndex(stage, -1)
	if len(calls) == 0 {
		t.Fatal("the final stage of operator/Dockerfile has no wget download: change this test with the image")
	}
	form := regexp.MustCompile(`^wget -qO (/[^\s"';&|]+) "https://[^"\s]+" && echo "\$\{([A-Z][A-Z0-9_]*)\} (/[^\s"';&|]+)" \| sha256sum -c -(?: &&|\n|$)`)
	paths, names := map[string]bool{}, map[string]bool{}
	for _, c := range calls {
		start := strings.Index(stage[c[0]:c[1]], "wget") + c[0]
		m := form.FindStringSubmatch(stage[start:])
		if m == nil {
			end := min(len(stage), start+160)
			if nl := strings.IndexByte(stage[start:end], '\n'); nl >= 0 {
				end = start + nl
			}
			t.Errorf("a download in operator/Dockerfile is not `wget -qO <path> \"https://...\" && echo \"${NAME}  <path>\" | sha256sum -c - &&`: %s", stage[start:end])
			continue
		}
		path, name := m[1], m[2]
		if m[3] != path {
			t.Errorf("the download to %s is checked as %s: the check must name the downloaded path", path, m[3])
		}
		if paths[path] || names[name] {
			t.Errorf("the download to %s (sum ${%s}) shares its path or its sum with another download", path, name)
		}
		paths[path], names[name] = true, true
		if !sums[name]["amd64"] || !sums[name]["arm64"] {
			t.Errorf("the download to %s is checked against ${%s}, which the ARCH case does not set for both amd64 and arm64", path, name)
		}
		if n := len(regexp.MustCompile(`(?:^|[\s;&|(])`+name+`=`).FindAllStringIndex(stage, -1)); n != 2 {
			t.Errorf("${%s} is set %d times in the final stage, want 2 (the amd64 and the arm64 branch only)", name, n)
		}
	}
}
