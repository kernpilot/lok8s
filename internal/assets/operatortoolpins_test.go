package assets

// operatortoolpins_test.go ties the tools that operator/Dockerfile
// downloads to the pins in lok8s: the khelm release and the path of its
// plugin to the Go code, and the flux release to the fluxcd addon.

import (
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/operator"
	"github.com/kernpilot/lok8s/internal/toolchain"
)

// operatorToolPins are the values in lok8s that the image's downloads
// follow.
type operatorToolPins struct {
	// khelm is the khelm release, with no "v" (toolchain.KhelmVersion).
	khelm string
	// pluginHome is the kustomize plugin home of `lo operator` in the
	// image (operator.DefaultKustomizePluginHome).
	pluginHome string
	// chartRenderer is where `lo operator` runs the khelm plugin from in
	// the image: its plugin home, then the plugin's path under it.
	chartRenderer string
	// flux is the Flux release that the fluxcd addon installs, with no "v"
	// (fluxAddonRelease).
	flux string
}

func toolPins(t *testing.T) operatorToolPins {
	t.Helper()
	// Errorf, not Fatal: the khelm cases do not need the addon and still run.
	flux, err := fluxAddonRelease(FS())
	if err != nil {
		t.Errorf("the flux pin: %v", err)
	}
	return operatorToolPins{
		khelm:         toolchain.KhelmVersion,
		pluginHome:    operator.DefaultKustomizePluginHome,
		chartRenderer: path.Join(operator.DefaultKustomizePluginHome, toolchain.ChartRendererPluginRel),
		flux:          flux,
	}
}

// The fluxcd addon is the one source of the Flux release in lok8s.
// Its chart.yaml pins the chart (flux2 of fluxcd-community), and the first
// line of its values.yaml names that chart and the chart's appVersion: the
// Flux release whose controllers and CRDs the chart installs. A khelm
// chart.yaml has no field for the appVersion, and a test does not ask the
// chart repository, so that line is the record. Its chart must be the one
// that chart.yaml pins: a chart bump then cannot leave the line behind.
// The test cannot see a line whose appVersion is not the chart's.
const (
	fluxAddonChart  = "addons/fluxcd/chart.yaml"
	fluxAddonValues = "addons/fluxcd/values.yaml"
	fluxChartRepo   = "https://fluxcd-community.github.io/helm-charts"
)

var fluxAddonLine = regexp.MustCompile(`^# Flux CD \(fluxcd-community/flux2 chart ([^,\s]+), appVersion ([^)\s]+)\)`)

// fluxAddonRelease returns the Flux release of the fluxcd addon in fsys
// (the tree of .lok8s), or why the addon does not name one.
func fluxAddonRelease(fsys fs.FS) (string, error) {
	read := func(name string) ([]byte, error) {
		raw, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("%w: if the fluxcd addon moved, point fluxAddonChart and fluxAddonValues at its new files", err)
		}
		return raw, nil
	}
	raw, err := read(fluxAddonChart)
	if err != nil {
		return "", err
	}
	var chart struct {
		Repository string `yaml:"repository"`
		Chart      string `yaml:"chart"`
		Version    string `yaml:"version"`
	}
	if err := yaml.Unmarshal(raw, &chart); err != nil {
		return "", fmt.Errorf("%s: %w", fluxAddonChart, err)
	}
	if chart.Repository != fluxChartRepo || chart.Chart != "flux2" {
		return "", fmt.Errorf("%s pins the chart %q of %q, not flux2 of %s: change this test with the addon", fluxAddonChart, chart.Chart, chart.Repository, fluxChartRepo)
	}
	if chart.Version == "" {
		return "", fmt.Errorf("%s has no version: pin the flux2 chart release there", fluxAddonChart)
	}
	raw, err = read(fluxAddonValues)
	if err != nil {
		return "", err
	}
	// The pattern is not anchored at the end of the line, so the \r of a
	// CRLF file does not change the match.
	first, _, _ := strings.Cut(string(raw), "\n")
	m := fluxAddonLine.FindStringSubmatch(first)
	switch {
	case m == nil:
		return "", fmt.Errorf("the first line of %s is %q, want \"# Flux CD (fluxcd-community/flux2 chart <chart>, appVersion <release>)\": the flux of the operator image follows that appVersion", fluxAddonValues, first)
	case strings.HasPrefix(m[2], "v"):
		return "", fmt.Errorf("the first line of %s writes the appVersion as %s: write it as the chart does, with no v (the image URL adds the v)", fluxAddonValues, m[2])
	case m[1] != chart.Version:
		return "", fmt.Errorf("the first line of %s names the chart %s, but %s pins %s: write the appVersion of chart %s on that line, and move the flux of the operator image (URL and sums) with it", fluxAddonValues, m[1], fluxAddonChart, chart.Version, chart.Version)
	}
	return m[2], nil
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
// name), and the two are the same release as the Flux that the fluxcd addon
// installs (fluxAddonRelease). lo does not run the flux CLI today (`lo
// gitops flux` is deferred), but the image ships it for a GitOps
// bootstrap: `flux bootstrap` applies the controllers and CRDs of the CLI's
// own release, so a CLI of another release would replace the addon's.
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
	if problems := operatorToolPinProblems(readOperatorDockerfile(t), toolPins(t)); len(problems) > 0 {
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
	pins := toolPins(t)
	moved := pins
	moved.khelm = "9.9.9"
	fluxMoved := pins
	fluxMoved.flux = "9.9.9"
	elsewhere := pins
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
		{"flux from another release than the addon",
			[]dockerfileEdit{{re: `(fluxcd/flux2/releases/download/v)[^/"]+/flux_[^_/"]+_`, new: "${1}9.9.9/flux_9.9.9_"}},
			pins, "downloads flux v9.9.9, but the fluxcd addon installs Flux " + pins.flux},
		{"the addon's Flux moves and the image does not",
			nil, fluxMoved, "but the fluxcd addon installs Flux 9.9.9"},
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
	case flux[0][1] != pins.flux:
		bad("operator/Dockerfile downloads flux v%s, but the fluxcd addon installs Flux %s (the appVersion on the first line of %s): change the URL and the sums of the image with the addon", flux[0][1], pins.flux, fluxAddonValues)
	}
	return problems
}

// Each rule of fluxAddonRelease fails on an edit of the real addon.
func TestFluxAddonReleaseFindsEachHole(t *testing.T) {
	t.Parallel()
	read := func(name string) string {
		raw, err := fs.ReadFile(FS(), name)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	chart, values := read(fluxAddonChart), read(fluxAddonValues)
	// edit replaces the one match of re in text; a text that re does not
	// match once stops the test (the case has to change with the addon).
	edit := func(text, re, new string) string {
		t.Helper()
		rx := regexp.MustCompile(re)
		if n := len(rx.FindAllStringIndex(text, -1)); n != 1 {
			t.Fatalf("the edit %s matches %d times, want once: change this case with the addon", re, n)
		}
		return rx.ReplaceAllString(text, new)
	}
	release := func(chart, values string) (string, error) {
		return fluxAddonRelease(fstest.MapFS{
			fluxAddonChart:  {Data: []byte(chart)},
			fluxAddonValues: {Data: []byte(values)},
		})
	}
	if got, err := release(chart, values); err != nil || got == "" {
		t.Fatalf("the unedited addon: release %q, error %v", got, err)
	}

	// The release is the one on the line, and the image rule then follows
	// it: a check that returned a fixed release would pass every hole below.
	nine := edit(values, `^(# Flux CD \(fluxcd-community/flux2 chart [^,]+, appVersion )[^)]+\)`, "${1}9.9.9)")
	t.Run("the release is read from the line", func(t *testing.T) {
		t.Parallel()
		got, err := release(chart, nine)
		if err != nil || got != "9.9.9" {
			t.Fatalf("an appVersion of 9.9.9: release %q, error %v; want 9.9.9", got, err)
		}
		pins := operatorToolPins{khelm: toolchain.KhelmVersion, pluginHome: operator.DefaultKustomizePluginHome,
			chartRenderer: path.Join(operator.DefaultKustomizePluginHome, toolchain.ChartRendererPluginRel), flux: got}
		want := "but the fluxcd addon installs Flux 9.9.9"
		problems := operatorToolPinProblems(readOperatorDockerfile(t), pins)
		for _, p := range problems {
			if strings.Contains(p, want) {
				return
			}
		}
		t.Errorf("the image rule does not follow the release on the line, want a problem with %q; it found %q", want, problems)
	})
	t.Run("a CRLF file gives the same release", func(t *testing.T) {
		t.Parallel()
		want, _ := release(chart, values)
		if got, err := release(chart, strings.ReplaceAll(values, "\n", "\r\n")); err != nil || got != want {
			t.Errorf("CRLF values.yaml: release %q, error %v; want %q", got, err, want)
		}
	})
	for _, missing := range []string{fluxAddonChart, fluxAddonValues} {
		t.Run("no "+missing, func(t *testing.T) {
			t.Parallel()
			fsys := fstest.MapFS{fluxAddonChart: {Data: []byte(chart)}, fluxAddonValues: {Data: []byte(values)}}
			delete(fsys, missing)
			_, err := fluxAddonRelease(fsys)
			if err == nil || !strings.Contains(err.Error(), "point fluxAddonChart and fluxAddonValues at its new files") {
				t.Errorf("a missing %s: error %v, want the next action", missing, err)
			}
		})
	}
	cases := []struct {
		name, chart, values, want string
	}{
		{"a chart bump that leaves the appVersion line behind",
			edit(chart, `(?m)^version: \S+$`, "version: 9.9.9"), values,
			"but " + fluxAddonChart + " pins 9.9.9"},
		{"the line names another chart",
			chart, edit(values, `^(# Flux CD \(fluxcd-community/flux2 chart )[^,]+,`, "${1}9.9.9,"),
			"names the chart 9.9.9, but"},
		{"the line is not the first",
			chart, "# values of the fluxcd addon\n" + values,
			"the first line of " + fluxAddonValues + " is"},
		{"the line names no appVersion",
			chart, edit(values, `^(# Flux CD \(fluxcd-community/flux2 chart [^,]+), appVersion [^)]+\)`, "${1})"),
			"the first line of " + fluxAddonValues + " is"},
		{"another chart",
			edit(chart, `(?m)^chart: flux2$`, "chart: flux-operator"), values,
			`pins the chart "flux-operator"`},
		{"another chart repository",
			edit(chart, `(?m)^repository: \S+$`, "repository: https://charts.example.com"), values,
			`of "https://charts.example.com", not flux2`},
		{"no chart version",
			edit(chart, `(?m)^version: \S+\n`, ""), values,
			fluxAddonChart + " has no version"},
		{"an appVersion with a v",
			chart, edit(values, `^(# Flux CD \(fluxcd-community/flux2 chart [^,]+, appVersion )([^)]+)\)`, "${1}v${2})"),
			"writes the appVersion as v"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := release(c.chart, c.values)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("the check lets this hole pass, want an error with %q; it returned %q, %v", c.want, got, err)
			}
		})
	}
}
