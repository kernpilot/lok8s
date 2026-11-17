package assets

// operatorclustertools_test.go ties the kubectl and kind that
// operator/Dockerfile downloads to the releases that the b.yaml template
// pins (toolchain.KubectlVersion, toolchain.KindVersion).

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/toolchain"
)

// clusterTool is one tool of the image that follows a template pin.
type clusterTool struct {
	name string
	// host is the start of every release URL of the tool: the image must
	// name it once (the one download).
	host string
	// url matches the download in the final stage; its group 1 is the
	// release, with its v.
	url *regexp.Regexp
	// form is the URL form for the messages.
	form string
	// pin names the pin in the messages.
	pin string
}

var clusterTools = []clusterTool{
	{name: "kubectl", host: "dl.k8s.io/release/",
		url:  regexp.MustCompile(`"https://dl\.k8s\.io/release/(v[^/"\s]+)/bin/linux/\$\{ARCH\}/kubectl"`),
		form: `"https://dl.k8s.io/release/<version>/bin/linux/${ARCH}/kubectl"`, pin: "toolchain.KubectlVersion"},
	{name: "kind", host: "github.com/kubernetes-sigs/kind/releases/",
		url:  regexp.MustCompile(`"https://github\.com/kubernetes-sigs/kind/releases/download/(v[^/"\s]+)/kind-linux-\$\{ARCH\}"`),
		form: `"https://github.com/kubernetes-sigs/kind/releases/download/<version>/kind-linux-${ARCH}"`, pin: "toolchain.KindVersion"},
}

// The image's kubectl and kind are the releases that the b.yaml template
// pins, so the operator and a project run the same cluster tools: the kind
// release picks the default node image (the cluster's Kubernetes), and
// kubectl must stay within one minor of it. Each tool has one release URL
// in the image, in the final stage, at the pinned release. The test reads
// text, not the shell: a download from a mirror can get past it.
func TestOperatorImageClusterToolsFollowThePins(t *testing.T) {
	t.Parallel()
	pins := map[string]string{"kubectl": toolchain.KubectlVersion, "kind": toolchain.KindVersion}
	if problems := clusterToolProblems(readOperatorDockerfile(t), pins); len(problems) > 0 {
		t.Errorf("operator/Dockerfile:\n  %s", strings.Join(problems, "\n  "))
	}
}

// Each rule of clusterToolProblems fails on an edit of the real text, or on
// a pin that moved while the image did not.
func TestOperatorImageClusterToolCheckFindsEachHole(t *testing.T) {
	t.Parallel()
	base := readOperatorDockerfile(t)
	const (
		modDownload = "RUN go mod download\n"
		lastCommand = "    && rm -rf /tmp/dl\n"
	)
	pins := map[string]string{"kubectl": toolchain.KubectlVersion, "kind": toolchain.KindVersion}
	moved := func(name string) map[string]string {
		m := map[string]string{"kubectl": toolchain.KubectlVersion, "kind": toolchain.KindVersion}
		m[name] = "v9.9.9"
		return m
	}
	cases := []struct {
		name  string
		edits []dockerfileEdit
		pins  map[string]string
		want  string
	}{
		{"kubectl from another release than the pin",
			[]dockerfileEdit{{re: `(dl\.k8s\.io/release/)v[^/"]+/`, new: "${1}v9.9.9/"}},
			pins, "downloads kubectl v9.9.9, but the b.yaml template pins kubectl " + toolchain.KubectlVersion},
		{"the kubectl pin moves and the image does not",
			nil, moved("kubectl"), "but the b.yaml template pins kubectl v9.9.9"},
		{"a second kubectl release",
			[]dockerfileEdit{{old: lastCommand, new: "    && echo https://dl.k8s.io/release/v9.9.9/bin/linux/amd64/kubectl \\\n" + lastCommand}},
			pins, "names 2 kubectl releases"},
		{"kubectl by another URL form",
			[]dockerfileEdit{{re: `(/bin/linux/)\$\{ARCH\}(/kubectl")`, new: "${1}amd64${2}"}},
			pins, "has 0 kubectl downloads of the form"},
		{"kind from another release than the pin",
			[]dockerfileEdit{{re: `(kubernetes-sigs/kind/releases/download/)v[^/"]+/`, new: "${1}v9.9.9/"}},
			pins, "downloads kind v9.9.9, but the b.yaml template pins kind " + toolchain.KindVersion},
		{"the kind pin moves and the image does not",
			nil, moved("kind"), "but the b.yaml template pins kind v9.9.9"},
		{"a second kind release",
			[]dockerfileEdit{{old: lastCommand, new: "    && echo https://github.com/kubernetes-sigs/kind/releases/download/v9.9.9/kind-linux-amd64 \\\n" + lastCommand}},
			pins, "names 2 kind releases"},
		{"kind by another URL form",
			[]dockerfileEdit{{re: `kind-linux-\$\{ARCH\}"`, new: `kind-linux-amd64"`}},
			pins, "has 0 kind downloads of the form"},
		{"kind downloaded in an earlier stage only",
			[]dockerfileEdit{
				{re: `(?m)^    && wget -qO /tmp/dl/kind \\\n(?:.*\n){3}`, new: ""},
				{old: modDownload, new: modDownload + `RUN wget -qO /tmp/dl/kind "https://github.com/kubernetes-sigs/kind/releases/download/` + toolchain.KindVersion + `/kind-linux-${ARCH}"` + "\n"},
			},
			pins, "the final stage of operator/Dockerfile has 0 kind downloads of the form"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			problems := clusterToolProblems(editOperatorDockerfile(t, base, c.edits), c.pins)
			for _, p := range problems {
				if strings.Contains(p, c.want) {
					return
				}
			}
			t.Errorf("the check lets this hole pass, want a problem with %q; it found %q", c.want, problems)
		})
	}
}

// clusterToolProblems returns how a text of operator/Dockerfile breaks the
// rules of TestOperatorImageClusterToolsFollowThePins against pins (tool
// name to release, with its v). An empty result means the text keeps the
// rules.
func clusterToolProblems(text string, pins map[string]string) []string {
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
	for _, tool := range clusterTools {
		if n := strings.Count(s, tool.host); n != 1 {
			bad("operator/Dockerfile names %d %s releases, want the one download: change this test with the image", n, tool.name)
		}
		got := tool.url.FindAllStringSubmatch(final, -1)
		if len(got) != 1 {
			bad("the final stage of operator/Dockerfile has %d %s downloads of the form %s, want one: change this test with the image", len(got), tool.name, tool.form)
			continue
		}
		if got[0][1] != pins[tool.name] {
			bad("operator/Dockerfile downloads %s %s, but the b.yaml template pins %s %s (%s): change the URL and the sums of the image with the pin", tool.name, got[0][1], tool.name, pins[tool.name], tool.pin)
		}
	}
	return problems
}
