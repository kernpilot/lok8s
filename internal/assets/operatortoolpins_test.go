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

	"github.com/kernpilot/lok8s/internal/operator"
	"github.com/kernpilot/lok8s/internal/toolchain"
)

// operatorToolPins are the values in the Go code that the image's
// downloads follow.
type operatorToolPins struct {
	// khelm is the khelm release, with no "v" (toolchain.KhelmVersion).
	khelm string
	// chartRenderer is where `lo operator` runs the khelm plugin from in
	// the image: its plugin home, then the plugin's path under it.
	chartRenderer string
}

func goToolPins() operatorToolPins {
	return operatorToolPins{
		khelm:         toolchain.KhelmVersion,
		chartRenderer: path.Join(operator.DefaultKustomizePluginHome, toolchain.ChartRendererPluginRel),
	}
}

// The image's khelm is the release that lok8s pins, and it lands where
// `lo operator` looks for it:
//   - toolchain.KhelmVersion is the khelm that lo-full links and that b
//     installs for lo core. The image's lo is lo core, which runs the
//     ChartRenderer plugin. With another release a chart can render
//     differently in the operator than on a workstation.
//   - The download is installed at operator.DefaultKustomizePluginHome +
//     toolchain.ChartRendererPluginRel.
//
// The image's flux URL names the release twice (the tag and the file
// name), and the two are the same. The Go code pins no flux: lo does not
// run the flux CLI (`lo gitops flux` is deferred), so the image's flux
// follows only its own URL.
//
// The test reads text, not the shell. It finds each tool by its release
// URL, so a download from another host (a mirror) can get past it.
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
	const lastCommand = "    && rm -rf /tmp/dl\n"
	moved := goToolPins()
	moved.khelm = "9.9.9"
	elsewhere := goToolPins()
	elsewhere.chartRenderer = path.Join("/opt/plugins", toolchain.ChartRendererPluginRel)
	cases := []struct {
		name  string
		edits []dockerfileEdit
		pins  operatorToolPins
		want  string
	}{
		{"khelm from another release than the pin",
			[]dockerfileEdit{{re: `(mgoltzsche/khelm/releases/download/v)[^/"]+/`, new: "${1}9.9.9/"}},
			goToolPins(), "downloads khelm v9.9.9, but lok8s pins khelm " + toolchain.KhelmVersion},
		{"the khelm pin moves and the image does not",
			nil, moved, "but lok8s pins khelm 9.9.9"},
		{"a second khelm release",
			[]dockerfileEdit{{old: lastCommand, new: "    && echo https://github.com/mgoltzsche/khelm/releases/download/v9.9.9/khelm-linux-amd64 \\\n" + lastCommand}},
			goToolPins(), "names 2 khelm releases"},
		{"khelm by another URL form",
			[]dockerfileEdit{{re: `khelm-linux-\$\{ARCH\}"`, new: `khelm-linux-amd64"`}},
			goToolPins(), "has 0 khelm downloads of the form"},
		{"khelm installed in another place",
			[]dockerfileEdit{{re: `(install -m 0755 /tmp/dl/khelm )\S+`, new: "${1}/usr/local/bin/ChartRenderer"}},
			goToolPins(), "installs khelm at /usr/local/bin/ChartRenderer, but lo operator runs"},
		{"the plugin path moves and the image does not",
			nil, elsewhere, "but lo operator runs the ChartRenderer plugin from /opt/plugins/"},
		{"the khelm download is not installed",
			[]dockerfileEdit{{re: `    && install -m 0755 /tmp/dl/khelm \S+ \\\n`, new: ""}},
			goToolPins(), "installs the khelm download /tmp/dl/khelm 0 times"},
		{"the flux tag names another release than the file",
			[]dockerfileEdit{{re: `(fluxcd/flux2/releases/download/v)[^/"]+/`, new: "${1}9.9.9/"}},
			goToolPins(), "names two flux releases: the tag v9.9.9 and the file flux_"},
		{"the flux file names another release than the tag",
			[]dockerfileEdit{{re: `(/flux_)[^_/"]+(_linux_)`, new: "${1}9.9.9${2}"}},
			goToolPins(), "and the file flux_9.9.9_linux_"},
		{"a second flux release",
			[]dockerfileEdit{{old: lastCommand, new: "    && echo https://github.com/fluxcd/flux2/releases/download/v9.9.9/flux_9.9.9_linux_amd64.tar.gz \\\n" + lastCommand}},
			goToolPins(), "names 2 flux releases"},
		{"flux by another URL form",
			[]dockerfileEdit{{re: `(flux_[^_/"]+_linux_)\$\{ARCH\}`, new: "${1}amd64"}},
			goToolPins(), "has 0 flux downloads of the form"},
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
	s := strings.Join(lines, "\n")

	// khelm: one release, the pinned one, installed as the plugin.
	if n := strings.Count(s, "github.com/mgoltzsche/khelm/releases/"); n != 1 {
		bad("operator/Dockerfile names %d khelm releases, want the one download: change this test with the image", n)
	}
	khelm := regexp.MustCompile(`wget -qO (/\S+) "https://github\.com/mgoltzsche/khelm/releases/download/v([^/"\s]+)/khelm-linux-\$\{ARCH\}"`).FindAllStringSubmatch(s, -1)
	if len(khelm) != 1 {
		bad(`operator/Dockerfile has %d khelm downloads of the form wget -qO <path> "https://github.com/mgoltzsche/khelm/releases/download/v<version>/khelm-linux-${ARCH}", want one`, len(khelm))
	} else {
		dl, version := khelm[0][1], khelm[0][2]
		if version != pins.khelm {
			bad("operator/Dockerfile downloads khelm v%s, but lok8s pins khelm %s (toolchain.KhelmVersion): change the URL and the sums of the image with the pin", version, pins.khelm)
		}
		dest := regexp.MustCompile(`(?:^|\s)install -m 0755 `+regexp.QuoteMeta(dl)+` (\S+)(?:\s|$)`).FindAllStringSubmatch(s, -1)
		switch {
		case len(dest) != 1:
			bad("operator/Dockerfile installs the khelm download %s %d times (install -m 0755 %s <path>), want once", dl, len(dest), dl)
		case dest[0][1] != pins.chartRenderer:
			bad("operator/Dockerfile installs khelm at %s, but lo operator runs the ChartRenderer plugin from %s (operator.DefaultKustomizePluginHome and toolchain.ChartRendererPluginRel)", dest[0][1], pins.chartRenderer)
		}
	}

	// flux: one release, the same in the tag and in the file name.
	if n := strings.Count(s, "github.com/fluxcd/flux2/releases/"); n != 1 {
		bad("operator/Dockerfile names %d flux releases, want the one download: change this test with the image", n)
	}
	flux := regexp.MustCompile(`"https://github\.com/fluxcd/flux2/releases/download/v([^/"\s]+)/flux_([^_/"\s]+)_linux_\$\{ARCH\}\.tar\.gz"`).FindAllStringSubmatch(s, -1)
	switch {
	case len(flux) != 1:
		bad(`operator/Dockerfile has %d flux downloads of the form "https://github.com/fluxcd/flux2/releases/download/v<version>/flux_<version>_linux_${ARCH}.tar.gz", want one`, len(flux))
	case flux[0][1] != flux[0][2]:
		bad("the URL in operator/Dockerfile names two flux releases: the tag v%s and the file flux_%s_linux_${ARCH}.tar.gz; make them the same", flux[0][1], flux[0][2])
	}
	return problems
}
