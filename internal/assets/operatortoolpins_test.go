package assets

// operatortoolpins_test.go ties the tools that operator/Dockerfile
// downloads to the pins in the Go code: the khelm release and the path of
// its plugin, and the two spellings of the flux release.

import (
	"fmt"
	"path"
	"regexp"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/operator"
	"github.com/kernpilot/lok8s/internal/toolchain"
)

// operatorToolPins are the values in the Go code that the image's
// downloads follow.
type operatorToolPins struct {
	// khelm is the khelm release, with no "v" (toolchain.KhelmVersion).
	khelm string
	// pluginHome is the kustomize plugin home of `lo operator` in the
	// image (operator.DefaultKustomizePluginHome).
	pluginHome string
	// chartRenderer is where `lo operator` runs the khelm plugin from in
	// the image: its plugin home, then the plugin's path under it.
	chartRenderer string
}

func goToolPins() operatorToolPins {
	return operatorToolPins{
		khelm:         toolchain.KhelmVersion,
		pluginHome:    operator.DefaultKustomizePluginHome,
		chartRenderer: path.Join(operator.DefaultKustomizePluginHome, toolchain.ChartRendererPluginRel),
	}
}

// The image's khelm is the release that lok8s pins, and it lands where
// `lo operator` looks for it:
//   - toolchain.KhelmVersion is the khelm that lo-full links and that b
//     installs for lo core. The image's lo is lo core, which runs the
//     ChartRenderer plugin. With another release a chart can render
//     differently in the operator than on a workstation.
//   - The download and its install are in the final stage (the image), and
//     the install puts it at operator.DefaultKustomizePluginHome +
//     toolchain.ChartRendererPluginRel.
//   - Nothing else names that path, and the plugin home is named only in
//     the mkdir -p of the plugin's directory and in the install: a later
//     cp, mv, ln or COPY onto it would replace the plugin.
//   - No ENV or ARG sets KUSTOMIZE_PLUGIN_HOME: `lo operator` reads it, and
//     would then look for its plugins in another place.
//
// The image's flux URL names the release twice (the tag and the file
// name), and the two are the same. The Go code pins no flux: lo does not
// run the flux CLI (`lo gitops flux` is deferred), so the image's flux
// follows only its own URL.
//
// No RUN has a # at the start of a word (runInlineComments): the shell
// reads the rest of the RUN as a comment, so an install after it would
// not run, but the text still has it.
//
// The test reads text, not the shell. It finds each tool by its release
// URL, so a download from another host (a mirror) can get past it, and so
// can a copy of a whole tree onto a parent of the plugin home (such as
// COPY --from=<stage> / /).
func TestOperatorImageToolsFollowThePins(t *testing.T) {
	t.Parallel()
	if problems := operatorToolPinProblems(readOperatorDockerfile(t), goToolPins()); len(problems) > 0 {
		t.Errorf("operator/Dockerfile:\n  %s", strings.Join(problems, "\n  "))
	}
}

// Each rule of operatorToolPinProblems fails on an edit of the real text,
// or on a pin that moved while the image did not.
func TestOperatorImageToolPinCheckFindsEachHole(t *testing.T) {
	t.Parallel()
	base := readOperatorDockerfile(t)
	const (
		modDownload = "RUN go mod download\n"
		copyLo      = "COPY --from=lo-build /out/lo /usr/local/bin/lo\n"
		lastCommand = "    && rm -rf /tmp/dl\n"
	)
	pins := goToolPins()
	moved := goToolPins()
	moved.khelm = "9.9.9"
	elsewhere := goToolPins()
	elsewhere.pluginHome = "/opt/plugins"
	elsewhere.chartRenderer = path.Join(elsewhere.pluginHome, toolchain.ChartRendererPluginRel)
	cases := []struct {
		name  string
		edits []dockerfileEdit
		pins  operatorToolPins
		want  string
	}{
		{"khelm from another release than the pin",
			[]dockerfileEdit{{re: `(mgoltzsche/khelm/releases/download/v)[^/"]+/`, new: "${1}9.9.9/"}},
			pins, "downloads khelm v9.9.9, but lok8s pins khelm " + toolchain.KhelmVersion},
		{"the khelm pin moves and the image does not",
			nil, moved, "but lok8s pins khelm 9.9.9"},
		{"a second khelm release",
			[]dockerfileEdit{{old: lastCommand, new: "    && echo https://github.com/mgoltzsche/khelm/releases/download/v9.9.9/khelm-linux-amd64 \\\n" + lastCommand}},
			pins, "names 2 khelm releases"},
		{"khelm by another URL form",
			[]dockerfileEdit{{re: `khelm-linux-\$\{ARCH\}"`, new: `khelm-linux-amd64"`}},
			pins, "has 0 khelm downloads of the form"},
		{"khelm installed in another place",
			[]dockerfileEdit{{re: `(install -m 0755 /tmp/dl/khelm )\S+`, new: "${1}/usr/local/bin/ChartRenderer"}},
			pins, "installs khelm at /usr/local/bin/ChartRenderer, but lo operator runs"},
		{"the plugin path moves and the image does not",
			nil, elsewhere, "but lo operator runs the ChartRenderer plugin from /opt/plugins/"},
		{"the khelm download is not installed",
			[]dockerfileEdit{{re: `    && install -m 0755 /tmp/dl/khelm \S+ \\\n`, new: ""}},
			pins, "installs the khelm download /tmp/dl/khelm 0 times"},
		{"khelm downloaded and installed in an earlier stage",
			[]dockerfileEdit{
				{re: `(?m)^    && wget -qO /tmp/dl/khelm \\\n(?:.*\n){4}`, new: ""},
				{old: modDownload, new: modDownload + `RUN wget -qO /tmp/dl/khelm "https://github.com/mgoltzsche/khelm/releases/download/v` + toolchain.KhelmVersion + `/khelm-linux-${ARCH}" \` + "\n" +
					"    && install -m 0755 /tmp/dl/khelm " + pins.chartRenderer + "\n"},
			},
			pins, "the final stage of operator/Dockerfile has 0 khelm downloads of the form"},
		{"an inline comment hides the install",
			[]dockerfileEdit{{re: `(mkdir -p \S+/chartrenderer) \\\n`, new: "${1} # the plugin's directory \\\n"}},
			pins, "has a comment after a command (# the plugin's directory"},
		{"a later copy onto the plugin",
			[]dockerfileEdit{{old: copyLo, new: copyLo + "COPY --from=lo-build /out/lo " + pins.chartRenderer + "\n"}},
			pins, "names the ChartRenderer plugin path " + pins.chartRenderer + " 2 times"},
		{"a copy into the plugin home",
			[]dockerfileEdit{{old: copyLo, new: copyLo + "COPY --from=lo-build /out/ " + pins.pluginHome + "/\n"}},
			pins, "names the kustomize plugin home " + pins.pluginHome + " 1 more times"},
		{"ENV moves the plugin home",
			[]dockerfileEdit{{old: copyLo, new: copyLo + "ENV " + config.KustomizePluginHomeEnv + "=/opt/plugins\n"}},
			pins, "sets " + config.KustomizePluginHomeEnv},
		{"ARG moves the plugin home",
			[]dockerfileEdit{{old: copyLo, new: copyLo + "ARG " + config.KustomizePluginHomeEnv + "=/opt/plugins\n"}},
			pins, "sets " + config.KustomizePluginHomeEnv},
		{"the flux tag names another release than the file",
			[]dockerfileEdit{{re: `(fluxcd/flux2/releases/download/v)[^/"]+/`, new: "${1}9.9.9/"}},
			pins, "names two flux releases: the tag v9.9.9 and the file flux_"},
		{"the flux file names another release than the tag",
			[]dockerfileEdit{{re: `(/flux_)[^_/"]+(_linux_)`, new: "${1}9.9.9${2}"}},
			pins, "and the file flux_9.9.9_linux_"},
		{"a second flux release",
			[]dockerfileEdit{{old: lastCommand, new: "    && echo https://github.com/fluxcd/flux2/releases/download/v9.9.9/flux_9.9.9_linux_amd64.tar.gz \\\n" + lastCommand}},
			pins, "names 2 flux releases"},
		{"flux by another URL form",
			[]dockerfileEdit{{re: `(flux_[^_/"]+_linux_)\$\{ARCH\}`, new: "${1}amd64"}},
			pins, "has 0 flux downloads of the form"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			problems := operatorToolPinProblems(editOperatorDockerfile(t, base, c.edits), c.pins)
			for _, p := range problems {
				if strings.Contains(p, c.want) {
					return
				}
			}
			t.Errorf("the check lets this hole pass, want a problem with %q; it found %q", c.want, problems)
		})
	}
}

// operatorToolPinProblems returns how a text of operator/Dockerfile breaks
// the rules of TestOperatorImageToolsFollowThePins against pins. An empty
// result means the text keeps the rules.
func operatorToolPinProblems(text string, pins operatorToolPins) []string {
	var problems []string
	bad := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }
	lines, err := foldDockerfile(text)
	if err != nil {
		return []string{err.Error()}
	}
	stages, err := dockerStages(lines)
	if err != nil {
		return []string{err.Error()}
	}
	s := strings.Join(lines, "\n")
	final := strings.Join(lines[stages[len(stages)-1].at:], "\n")
	for _, c := range runInlineComments(lines) {
		bad("a RUN of operator/Dockerfile has a comment after a command (%s): the shell does not run the rest of that RUN", c)
	}

	// khelm: one release, the pinned one, downloaded and installed as the
	// plugin in the final stage.
	if n := strings.Count(s, "github.com/mgoltzsche/khelm/releases/"); n != 1 {
		bad("operator/Dockerfile names %d khelm releases, want the one download: change this test with the image", n)
	}
	khelm := regexp.MustCompile(`wget -qO (/\S+) "https://github\.com/mgoltzsche/khelm/releases/download/v([^/"\s]+)/khelm-linux-\$\{ARCH\}"`).FindAllStringSubmatch(final, -1)
	if len(khelm) != 1 {
		bad(`the final stage of operator/Dockerfile has %d khelm downloads of the form wget -qO <path> "https://github.com/mgoltzsche/khelm/releases/download/v<version>/khelm-linux-${ARCH}", want one: change this test with the image`, len(khelm))
	} else {
		dl, version := khelm[0][1], khelm[0][2]
		if version != pins.khelm {
			bad("operator/Dockerfile downloads khelm v%s, but lok8s pins khelm %s (toolchain.KhelmVersion): change the URL and the sums of the image with the pin", version, pins.khelm)
		}
		dest := regexp.MustCompile(`(?:^|\s)install -m 0755 `+regexp.QuoteMeta(dl)+` (\S+)(?:\s|$)`).FindAllStringSubmatch(final, -1)
		switch {
		case len(dest) != 1:
			bad("the final stage of operator/Dockerfile installs the khelm download %s %d times (install -m 0755 %s <path>), want once", dl, len(dest), dl)
		case dest[0][1] != pins.chartRenderer:
			bad("operator/Dockerfile installs khelm at %s, but lo operator runs the ChartRenderer plugin from %s (operator.DefaultKustomizePluginHome and toolchain.ChartRendererPluginRel)", dest[0][1], pins.chartRenderer)
		}
	}

	// The plugin stays where the install put it, and lo operator looks for
	// it there.
	if n := strings.Count(s, pins.chartRenderer); n != 1 {
		bad("operator/Dockerfile names the ChartRenderer plugin path %s %d times, want once (the install): a later copy onto it would replace the plugin", pins.chartRenderer, n)
	}
	mkdir := "mkdir -p " + path.Dir(pins.chartRenderer)
	if n := strings.Count(s, pins.pluginHome) - strings.Count(s, pins.chartRenderer) - strings.Count(s, mkdir); n != 0 {
		bad("operator/Dockerfile names the kustomize plugin home %s %d more times than in the install and in `%s`: a copy into it can replace the plugin; change this test with the image", pins.pluginHome, n, mkdir)
	}
	setsHome := regexp.MustCompile(`(?i)^[ \t]*(?:ENV|ARG)[ \t].*\b` + config.KustomizePluginHomeEnv + `\b`)
	for _, line := range lines {
		if setsHome.MatchString(line) {
			bad("operator/Dockerfile sets %s (%s): lo operator would look for its plugins there, not in %s", config.KustomizePluginHomeEnv, line, pins.pluginHome)
		}
	}

	// flux: one release, the same in the tag and in the file name.
	if n := strings.Count(s, "github.com/fluxcd/flux2/releases/"); n != 1 {
		bad("operator/Dockerfile names %d flux releases, want the one download: change this test with the image", n)
	}
	flux := regexp.MustCompile(`"https://github\.com/fluxcd/flux2/releases/download/v([^/"\s]+)/flux_([^_/"\s]+)_linux_\$\{ARCH\}\.tar\.gz"`).FindAllStringSubmatch(s, -1)
	switch {
	case len(flux) != 1:
		bad(`operator/Dockerfile has %d flux downloads of the form "https://github.com/fluxcd/flux2/releases/download/v<version>/flux_<version>_linux_${ARCH}.tar.gz", want one: change this test with the image`, len(flux))
	case flux[0][1] != flux[0][2]:
		bad("the URL in operator/Dockerfile names two flux releases: the tag v%s and the file flux_%s_linux_${ARCH}.tar.gz; make them the same", flux[0][1], flux[0][2])
	}
	return problems
}
