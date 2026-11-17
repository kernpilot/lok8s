package assets

// operatorimage_test.go covers the sources of the operator image: its apk
// packages, its base images and the tools it downloads.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/testutil"
)

// readOperatorDockerfile returns operator/Dockerfile with LF line ends.
func readOperatorDockerfile(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(testutil.RepoRoot(t), "operator/Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(raw), "\r\n", "\n")
}

// foldDockerfile returns the instructions of a Dockerfile, one per line.
// Comment lines are removed first (a comment may name a tool), then the
// backslash line continuations are folded. A blank line inside a
// continuation does not end it (BuildKit skips it). Each run of blanks
// becomes one space, also inside quotes: the apk write's quoted text has no
// blank, and a checksum line `echo "${X}  <path>"` reads with one space.
// It refuses the forms it cannot read this way: an escape directive, and a
// heredoc (a heredoc body line can start with "from" and read as a stage).
func foldDockerfile(text string) ([]string, error) {
	if regexp.MustCompile(`(?mi)^[ \t]*#[ \t]*escape[ \t]*=`).MatchString(text) {
		return nil, errors.New("operator/Dockerfile sets an escape directive: these tests fold only backslash line continuations")
	}
	var code []string
	for line := range strings.SplitSeq(text, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			code = append(code, line)
		}
	}
	s := regexp.MustCompile(`[ \t]*\\[ \t]*\n(?:[ \t]*\n)*[ \t]*`).ReplaceAllString(strings.Join(code, "\n"), " ")
	s = regexp.MustCompile(`[ \t]+`).ReplaceAllString(s, " ")
	if strings.Contains(s, "<<") {
		return nil, errors.New("operator/Dockerfile uses a heredoc (<<): these tests read instructions line by line")
	}
	return strings.Split(s, "\n"), nil
}

// dockerStage is one FROM of a folded Dockerfile.
type dockerStage struct {
	image string // the base, lower case
	name  string // the AS name, lower case; empty without AS
	at    int    // the index of the FROM line
}

// dockerStages returns the stages of a folded Dockerfile, in order. A FROM
// in another form than `FROM [--platform=<p>] <image> [AS <name>]` is an
// error.
func dockerStages(lines []string) ([]dockerStage, error) {
	word := regexp.MustCompile(`(?i)^[ \t]*FROM(?:[ \t]|$)`)
	form := regexp.MustCompile(`(?i)^[ \t]*FROM[ \t]+(?:--platform=\S+[ \t]+)?(\S+)(?:[ \t]+AS[ \t]+(\S+))?[ \t]*$`)
	var stages []dockerStage
	for i, line := range lines {
		if !word.MatchString(line) {
			continue
		}
		m := form.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("operator/Dockerfile has a FROM these tests cannot read: %s", line)
		}
		stages = append(stages, dockerStage{image: strings.ToLower(m[1]), name: strings.ToLower(m[2]), at: i})
	}
	if len(stages) == 0 {
		return nil, errors.New("operator/Dockerfile has no FROM line")
	}
	return stages, nil
}

// operatorFinalStage reads operator/Dockerfile and returns its final stage,
// one instruction per line. It stops the test on a form it cannot read: an
// escape directive, a heredoc, a FROM in another form, an ARG in the final
// FROM, or a final stage that builds on an earlier stage.
func operatorFinalStage(t *testing.T) string {
	t.Helper()
	lines, err := foldDockerfile(readOperatorDockerfile(t))
	if err != nil {
		t.Fatal(err)
	}
	stages, err := dockerStages(lines)
	if err != nil {
		t.Fatal(err)
	}
	final := stages[len(stages)-1]
	if strings.Contains(final.image, "$") {
		t.Fatalf("the final FROM of operator/Dockerfile names its base with an ARG (%s): this test cannot tell if that is an earlier stage", final.image)
	}
	for _, s := range stages[:len(stages)-1] {
		if s.name != "" && s.name == final.image {
			t.Fatalf("the final stage of operator/Dockerfile builds on the stage %s: this test reads only the final stage", final.image)
		}
	}
	return strings.Join(lines[final.at:], "\n")
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
	stage := operatorFinalStage(t)

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

// The operator image builds on two images and downloads the tools that are
// not Alpine packages. All of it comes from outside, so all of it is pinned
// to its bytes, in every stage (the build stage makes the lo binary that
// the final image runs):
//   - each FROM names an earlier stage, or an image by its tag and the
//     digest of its index (<image>:<tag>@sha256:<64 hex>); the final FROM
//     names ghcr.io/flant/shell-operator. A COPY --from and a RUN --mount
//     with from= follow the same rule;
//   - each download has the exact form
//     wget -qO <path> "https://<url>" && echo "${<NAME>}  <path>" | sha256sum -c -
//     and starts with the plain word wget, at the start of a RUN or right
//     after &&. So the check is the next command, before any use of the
//     file. Any other mention of wget fails, also by a path (/usr/bin/wget)
//     or in a URL;
//   - after the check comes && or the end of the RUN, and the rest of that
//     RUN has no ||, ;, |, &, ( ) or backtick outside the other checks. So
//     a failed check stops the RUN: nothing after it can run anyway, hide
//     its status, or wrap it in a command whose own status counts;
//   - each <NAME> is set to 64 hex digits in the amd64 and the arm64 branch
//     (the platforms docker.yml builds) of the one `case "${ARCH}" in`, and
//     nowhere else; the case ends with a `*)` branch that exits 1. No two
//     sums in the case are the same (a copy slip would else show only in
//     the arm64 build);
//   - each download has its own path and its own <NAME>;
//   - no curl and no ADD (each fetches with no check).
//
// The test reads text, not the shell. It checks that each download has a
// check, not that a sum is right: a wrong sum fails the build. These can
// still get past it:
//   - a download by another tool (such as git or go install), through a
//     variable, or by a script;
//   - a <NAME> that the shell sets in another form (export, read, a loop);
//   - a change to the file between its check and its use (cp, mv);
//   - a shell option or a trap that changes how a failed command ends the
//     RUN;
//   - another sha256sum: a shell function of that name, a symlink, or a
//     PATH directory in front of it;
//   - a tag change without the digest change: the test cannot tell which
//     digest a tag has, and the digest alone picks the image.
//
// The Go modules of the build stage are checked by go.sum, not by this test.
func TestOperatorImageDownloadsAreChecked(t *testing.T) {
	t.Parallel()
	for _, p := range operatorDownloadProblems(readOperatorDockerfile(t)) {
		t.Error(p)
	}
}

// Each case is operator/Dockerfile with one hole put in. The check must find
// it, with the named problem. An edit names the text it changes and must
// match it once, so a change of the image that moves that text fails here:
// then change the case with the image.
func TestOperatorImageDownloadCheckFindsEachHole(t *testing.T) {
	t.Parallel()
	base := readOperatorDockerfile(t)
	type edit = dockerfileEdit
	const (
		modDownload    = "RUN go mod download\n"
		copyLo         = "COPY --from=lo-build /out/lo /usr/local/bin/lo\n"
		kubectlGet     = "    && wget -qO /tmp/dl/kubectl \\\n"
		kubectlCheck   = `    && echo "${KUBECTL_SHA256}  /tmp/dl/kubectl" | sha256sum -c - \` + "\n"
		kubectlInstall = "    && install -m 0755 /tmp/dl/kubectl /usr/local/bin/kubectl \\\n"
		kindCheck      = `    && echo "${KIND_SHA256}  /tmp/dl/kind" | sha256sum -c - \` + "\n"
		fluxCheck      = `    && echo "${FLUX_SHA256}  /tmp/dl/flux.tar.gz" | sha256sum -c - \` + "\n"
		khelmGet       = "    && wget -qO /tmp/dl/khelm \\\n"
		mkdirDL        = "    && mkdir -p /tmp/dl \\\n"
		lastCommand    = "    && rm -rf /tmp/dl\n"
	)
	// kubectl downloaded, installed and run before its check.
	checkAfterUse := []edit{
		{old: kubectlCheck},
		{old: kubectlInstall, new: kubectlInstall + "    && /usr/local/bin/kubectl version --client \\\n" + kubectlCheck},
	}
	cases := []struct {
		name  string
		edits []edit
		want  string
	}{
		{"the base of the build stage has no digest",
			[]edit{{re: `(?m)^(FROM golang:[^@\s]+)@sha256:[0-9a-f]{64}`, new: "$1"}},
			"builds a stage on golang:"},
		{"the final base has no digest",
			[]edit{{re: `(?m)^(FROM ghcr\.io/flant/shell-operator:[^@\s]+)@sha256:[0-9a-f]{64}`, new: "$1"}},
			"the final FROM of operator/Dockerfile must name shell-operator"},
		{"an unchecked wget in the build stage",
			[]edit{{old: modDownload, new: modDownload + `RUN wget -qO /tmp/helper "https://example.org/helper" && chmod +x /tmp/helper` + "\n"}},
			"is not `wget -qO <path>"},
		{"an ADD in the build stage",
			[]edit{{old: modDownload, new: modDownload + "ADD https://example.org/helper /tmp/helper\n"}},
			"has an ADD"},
		{"a curl in the build stage",
			[]edit{{old: modDownload, new: modDownload + "RUN curl -fsSLo /tmp/helper https://example.org/helper\n"}},
			"calls curl"},
		{"a COPY from an image that is no stage",
			[]edit{{old: copyLo, new: "COPY --from=alpine:3.24 /bin/busybox /usr/local/bin/busybox\n" + copyLo}},
			"copies from alpine:3.24"},
		{"a RUN mount from an image that is no stage",
			[]edit{{old: copyLo, new: "RUN --mount=type=bind,from=alpine:3.24,target=/mnt true\n" + copyLo}},
			"mounts from alpine:3.24"},
		{"wget by its path, with no check",
			[]edit{{old: copyLo, new: "RUN /usr/bin/wget -qO /usr/local/bin/helper https://example.org/helper && chmod +x /usr/local/bin/helper\n" + copyLo}},
			"not the plain word wget"},
		{"wget by its path, into sh",
			[]edit{{old: copyLo, new: "RUN /usr/bin/wget -qO- https://example.org/install.sh | sh\n" + copyLo}},
			"not the plain word wget"},
		{"kubectl by /usr/bin/wget, run before its check",
			append([]edit{{old: kubectlGet, new: "    && /usr/bin/wget -qO /tmp/dl/kubectl \\\n"}}, checkAfterUse...),
			"not the plain word wget"},
		{"kubectl run before its check",
			checkAfterUse,
			"is not `wget -qO <path>"},
		{"a check that cannot fail (&& true || true after it)",
			[]edit{{old: kubectlCheck, new: kubectlCheck + "    && true || true \\\n"}},
			"after the check of /tmp/dl/kubectl"},
		{"a ; right after the check",
			[]edit{{old: kindCheck, new: strings.Replace(kindCheck, "-c - \\", "-c - ; true \\", 1)}},
			"after the check of /tmp/dl/kind"},
		{"a pipe right after the check",
			[]edit{{old: fluxCheck, new: strings.Replace(fluxCheck, "-c - \\", "-c - | cat \\", 1)}},
			"after the check of /tmp/dl/flux.tar.gz"},
		{"an option after the check",
			[]edit{{old: kubectlCheck, new: strings.Replace(kubectlCheck, "-c - \\", "-c - --ignore-missing \\", 1)}},
			"after the check of /tmp/dl/kubectl"},
		{"a $( ) around the last download and the rest of the RUN",
			[]edit{{old: khelmGet, new: strings.Replace(khelmGet, "&& wget", "&& echo $(true && wget", 1)},
				{old: lastCommand, new: "    && rm -rf /tmp/dl)\n"}},
			"after the check of /tmp/dl/khelm"},
		{"backticks around the last download and the rest of the RUN",
			[]edit{{old: khelmGet, new: strings.Replace(khelmGet, "&& wget", "&& echo `true && wget", 1)},
				{old: lastCommand, new: "    && rm -rf /tmp/dl`\n"}},
			"after the check of /tmp/dl/khelm"},
		{"a || at the end of the RUN",
			[]edit{{old: lastCommand, new: "    && rm -rf /tmp/dl || true\n"}},
			"after the check of /tmp/dl/khelm"},
		{"a ; at the end of the RUN",
			[]edit{{old: lastCommand, new: "    && rm -rf /tmp/dl; true\n"}},
			"after the check of /tmp/dl/khelm"},
		{"a & at the end of the RUN",
			[]edit{{old: lastCommand, new: "    && rm -rf /tmp/dl &\n"}},
			"after the check of /tmp/dl/khelm"},
		// The table of sums and the names of the checks.
		{"no download at all",
			[]edit{{re: `(?s)    && mkdir -p /tmp/dl \\\n.*?    && rm -rf /tmp/dl\n`, new: "    && true\n"}},
			"has no wget download"},
		{"the check names another path than the download",
			[]edit{{old: kindCheck, new: strings.Replace(kindCheck, "/tmp/dl/kind", "/tmp/dl/khelm", 1)}},
			"the download to /tmp/dl/kind is checked as /tmp/dl/khelm"},
		{"two tools share a sum name",
			[]edit{{old: kindCheck, new: strings.Replace(kindCheck, "KIND_SHA256", "KHELM_SHA256", 1)}},
			"shares its path or its sum"},
		{"the arm64 sum of a tool is missing",
			[]edit{{re: `(arm64\) \\\n(?:\s+[A-Z0-9_]+=[0-9a-f]{64} \\\n)*?)\s+KIND_SHA256=[0-9a-f]{64} \\\n`, new: "$1"}},
			"is checked against ${KIND_SHA256}, which the ARCH case does not set for both amd64 and arm64"},
		{"a sum set a third time",
			[]edit{{old: mkdirDL, new: "    && KUBECTL_SHA256=" + strings.Repeat("0", 64) + " \\\n" + mkdirDL}},
			"${KUBECTL_SHA256} is set 3 times"},
		{"two sums with the same value",
			[]edit{{re: `(KIND_SHA256=)[0-9a-f]{64}( \\\n\s+KHELM_SHA256=)[0-9a-f]{64}( ;; \\\n\s+arm64\))`,
				new: "${1}" + strings.Repeat("1", 64) + "${2}" + strings.Repeat("1", 64) + "${3}"}},
			"sets the same sum for KIND_SHA256 (amd64) and KHELM_SHA256 (amd64)"},
		{"a *) branch that exits 0",
			[]edit{{old: "exit 1 ;; \\\n", new: "exit 0 ;; \\\n"}},
			"must end with exit 1"},
		{"a second ARCH case",
			[]edit{{old: mkdirDL, new: "    && case \"${ARCH}\" in amd64) ;; esac \\\n" + mkdirDL}},
			"has 2 `case"},
		{"a branch body that is not NAME=<64 hex>",
			[]edit{{old: "      amd64) \\\n", new: "      amd64) \\\n        export GOARCH=amd64 \\\n"}},
			"branch of the ARCH case may only set NAME=<64 hex>, got: export"},
		{"a branch for another architecture",
			[]edit{{old: "      *) echo", new: "      riscv64) ;; \\\n      *) echo"}},
			"has the branch riscv64)"},
		{"a branch with no label",
			[]edit{{old: "      *) echo", new: "      oops ;; \\\n      *) echo"}},
			"has a branch this test cannot read: oops"},
		{"no arm64 branch",
			[]edit{{re: `(?s)      arm64\) \\\n.*?;; \\\n`, new: ""}},
			"has no arm64) branch"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			problems := operatorDownloadProblems(editOperatorDockerfile(t, base, c.edits))
			for _, p := range problems {
				if strings.Contains(p, c.want) {
					return
				}
			}
			t.Errorf("the check lets this hole pass, want a problem with %q; it found %q", c.want, problems)
		})
	}
}

// dockerfileEdit is one edit of a hole case: re (a regexp, with $1 in new)
// or old (a literal) names the text, which must occur once.
type dockerfileEdit struct{ re, old, new string }

// editOperatorDockerfile applies the edits to text in order. A text that
// an edit does not match exactly once stops the test: the case then has to
// change with the image.
func editOperatorDockerfile(t *testing.T, text string, edits []dockerfileEdit) string {
	t.Helper()
	for _, e := range edits {
		if e.re != "" {
			rx := regexp.MustCompile(e.re)
			if n := len(rx.FindAllStringIndex(text, -1)); n != 1 {
				t.Fatalf("the edit %s matches operator/Dockerfile %d times, want once: change this case with the image", e.re, n)
			}
			text = rx.ReplaceAllString(text, e.new)
			continue
		}
		if n := strings.Count(text, e.old); n != 1 {
			t.Fatalf("operator/Dockerfile has %d copies of %q, want one: change this case with the image", n, e.old)
		}
		text = strings.Replace(text, e.old, e.new, 1)
	}
	return text
}

// operatorDownloadProblems returns what breaks the rules of
// TestOperatorImageDownloadsAreChecked in a text of operator/Dockerfile, in
// every stage. An empty result means the text keeps the rules.
func operatorDownloadProblems(text string) []string {
	lines, err := foldDockerfile(text)
	if err != nil {
		return []string{err.Error()}
	}
	stages, err := dockerStages(lines)
	if err != nil {
		return []string{err.Error()}
	}
	var problems []string
	bad := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }
	s := strings.Join(lines, "\n")

	// The images: each base and each --from source is an earlier stage, or
	// an image by its tag and the digest of its index.
	pinned := regexp.MustCompile(`^[a-z0-9][a-z0-9._/:-]*:[a-z0-9_][a-z0-9_.-]*@sha256:[0-9a-f]{64}$`)
	final := regexp.MustCompile(`^ghcr\.io/flant/shell-operator:v[0-9][0-9a-z.+-]*@sha256:[0-9a-f]{64}$`)
	flags := regexp.MustCompile(`(?i)^[ \t]*(COPY|RUN)((?:[ \t]+--\S+)*)`)
	known := map[string]bool{} // the names and the indexes of the stages so far
	source := func(what, ref string) {
		if ref = strings.ToLower(ref); !known[ref] && !pinned.MatchString(ref) {
			bad("operator/Dockerfile %s %s, which is no earlier stage and not an image by its tag and digest (<image>:<tag>@sha256:<64 hex>)", what, ref)
		}
	}
	next := 0
	for i, line := range lines {
		if next < len(stages) && stages[next].at == i {
			st := stages[next]
			switch {
			case next == len(stages)-1:
				if !final.MatchString(st.image) {
					bad("the final FROM of operator/Dockerfile must name shell-operator by its tag and the digest of its multi-arch index (<image>:<tag>@sha256:<64 hex>), got %s", st.image)
				}
			case st.image != "scratch":
				source("builds a stage on", st.image)
			}
			known[strconv.Itoa(next)] = true
			if st.name != "" {
				known[st.name] = true
			}
			next++
			continue
		}
		m := flags.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		for f := range strings.FieldsSeq(m[2]) {
			key, val, _ := strings.Cut(f, "=")
			switch {
			case strings.EqualFold(m[1], "COPY") && strings.EqualFold(key, "--from"):
				source("copies from", val)
			case strings.EqualFold(m[1], "RUN") && strings.EqualFold(key, "--mount"):
				for opt := range strings.SplitSeq(val, ",") {
					if k, v, ok := strings.Cut(opt, "="); ok && strings.EqualFold(k, "from") {
						source("mounts from", v)
					}
				}
			}
		}
	}

	// ADD can fetch a URL or unpack an archive, and curl downloads: neither
	// has a check here.
	add := regexp.MustCompile(`(?i)^[ \t]*ADD(?:[ \t]|$)`)
	for _, line := range lines {
		if add.MatchString(line) {
			bad("operator/Dockerfile has an ADD, which can fetch a URL or unpack an archive with no check: use COPY, or wget and sha256sum -c: %s", line)
		}
	}
	if regexp.MustCompile(`(?i)\bcurl\b`).MatchString(s) {
		bad("operator/Dockerfile calls curl: download with wget and check the file with sha256sum -c")
	}

	// The sums: one case on ARCH, a branch for each built platform.
	sums := map[string]map[string]bool{} // NAME -> the arches that set it
	cases := regexp.MustCompile(`case "\$\{ARCH\}" in (.*?) esac`).FindAllStringSubmatch(s, -1)
	if len(cases) != 1 {
		bad("operator/Dockerfile has %d `case \"${ARCH}\" in ... esac`, want exactly one with the download sums", len(cases))
		cases = [][]string{{"", ""}}
	}
	seen := map[string]bool{}
	values := map[string]string{} // a sum -> "NAME (arch)" that set it first
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
			bad("the ARCH case of operator/Dockerfile has a branch this test cannot read: %s", part)
		case label == "amd64" || label == "arm64":
			seen[label] = true
			for f := range strings.FieldsSeq(body) {
				m := assign.FindStringSubmatch(f)
				if m == nil {
					bad("the %s branch of the ARCH case may only set NAME=<64 hex>, got: %s", label, f)
					continue
				}
				use := m[1] + " (" + label + ")"
				if first, dup := values[m[2]]; dup {
					bad("the ARCH case sets the same sum for %s and %s: each tool and architecture has its own sum (a copy slip?)", first, use)
				}
				values[m[2]] = use
				if sums[m[1]] == nil {
					sums[m[1]] = map[string]bool{}
				}
				sums[m[1]][label] = true
			}
		case label == "*":
			seen[label] = true
			if !regexp.MustCompile(`(?:^|[;\s])exit 1$`).MatchString(body) {
				bad("the *) branch of the ARCH case must end with exit 1 (an architecture with no sums fails the build), got: %s", body)
			}
		default:
			bad("the ARCH case of operator/Dockerfile has the branch %s): this test knows amd64, arm64 and *", label)
		}
	}
	for _, label := range []string{"amd64", "arm64", "*"} {
		if !seen[label] {
			bad("the ARCH case of operator/Dockerfile has no %s) branch", label)
		}
	}

	// Each download, its check right after it, and the rest of its RUN.
	word := regexp.MustCompile(`(?i)\bwget\b`)
	runStart := regexp.MustCompile(`(?i)^[ \t]*RUN(?:[ \t]+--\S+)*[ \t]+$`)
	form := regexp.MustCompile(`^wget -qO (/[^\s"';&|]+) "https://[^"\s]+" && echo "\$\{([A-Z][A-Z0-9_]*)\} (/[^\s"';&|]+)" \| sha256sum -c -`)
	check := regexp.MustCompile(`echo "\$\{[A-Z][A-Z0-9_]*\} /[^\s"';&|]+" \| sha256sum -c -`)
	calls := word.FindAllStringIndex(s, -1)
	if len(calls) == 0 {
		bad("operator/Dockerfile has no wget download: change this test with the image")
	}
	paths, names := map[string]bool{}, map[string]bool{}
	for _, c := range calls {
		lineStart := strings.LastIndexByte(s[:c[0]], '\n') + 1
		lineEnd := len(s)
		if nl := strings.IndexByte(s[c[0]:], '\n'); nl >= 0 {
			lineEnd = c[0] + nl
		}
		pre, run := s[lineStart:c[0]], s[c[0]:lineEnd]
		show := run[:min(len(run), 160)]
		atStart := runStart.MatchString(pre) || strings.HasSuffix(pre, " && ")
		if s[c[0]:c[1]] != "wget" || !atStart {
			bad("a download in operator/Dockerfile is not the plain word wget at the start of a RUN or right after &&: ...%s%s", pre[max(0, len(pre)-40):], show)
			continue
		}
		m := form.FindStringSubmatch(run)
		if m == nil {
			bad("a download in operator/Dockerfile is not `wget -qO <path> \"https://...\" && echo \"${NAME}  <path>\" | sha256sum -c -`: %s", show)
			continue
		}
		path, name := m[1], m[2]
		if m[3] != path {
			bad("the download to %s is checked as %s: the check must name the downloaded path", path, m[3])
		}
		if paths[path] || names[name] {
			bad("the download to %s (sum ${%s}) shares its path or its sum with another download", path, name)
		}
		paths[path], names[name] = true, true
		if !sums[name]["amd64"] || !sums[name]["arm64"] {
			bad("the download to %s is checked against ${%s}, which the ARCH case does not set for both amd64 and arm64", path, name)
		}
		if n := len(regexp.MustCompile(`(?:^|[\s;&|(])`+name+`=`).FindAllStringIndex(s, -1)); n != 2 {
			bad("${%s} is set %d times in operator/Dockerfile, want 2 (the amd64 and the arm64 branch only)", name, n)
		}
		after := run[len(m[0]):]
		rest := strings.ReplaceAll(check.ReplaceAllString(after, ""), "&&", "")
		if (after != "" && !strings.HasPrefix(after, " && ")) || strings.ContainsAny(rest, "|;&()`") {
			bad("after the check of %s, operator/Dockerfile must go on with && or end the RUN, and the rest of that RUN must have no ||, ;, |, &, ( ) or backtick: a failed check must stop the RUN: %s", path, after[:min(len(after), 160)])
		}
	}
	return problems
}
