package kubehz

// agentidentity_test.go: the identity read of the heartbeat script. The
// read prints the resourceVersion of Secret kubehz-agent and, for a Secret
// with entries, their number and a letter for each key of the identity
// that has a value (A for agent-token, C for claim-code). The fill writes
// only a Secret with no entry at all.
//
// kubectl prints -o go-template with text/template: the go-template
// printer of k8s.io/cli-runtime parses the template and runs it on the
// object as JSON, decoded into a map[string]interface{}. This test does
// the same with the template of the script, on the Secret fixtures in
// tests/fixtures/agent-identity-secret, and compares each output with the
// .want file of the fixture. tests/unit/kubehz_agent_secret_test.bats
// answers the read in its stub from the same fixtures with jq, and checks
// the stub against the same .want files: so the stub prints what kubectl
// prints for each fixture.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
)

// agentIdentityRead is the -o value of the identity read.
const agentIdentityRead = `go-template={{.metadata.resourceVersion}} {{with .data}}{{len .}} {{if index . "agent-token"}}A{{end}}{{if index . "claim-code"}}C{{end}}{{end}}`

const agentIdentityFixtures = "../../tests/fixtures/agent-identity-secret"

func TestAgentIdentityReadOnTheSecretFixtures(t *testing.T) {
	t.Parallel()
	if n := strings.Count(heartbeatScript(t), "-o '"+agentIdentityRead+"'"); n != 1 {
		t.Fatalf("the script has %d identity reads with the template of this test, want 1", n)
	}
	tmpl, err := template.New("output").Parse(strings.TrimPrefix(agentIdentityRead, "go-template="))
	if err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(agentIdentityFixtures, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	// One fixture for each case of the read: no data, the identity, the
	// identity and another key, each half alone, empty values, another key
	// alone, with and without a value.
	if len(files) < 10 {
		t.Fatalf("%d fixtures in %s, want at least 10", len(files), agentIdentityFixtures)
	}
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var obj map[string]interface{}
		if err := json.Unmarshal(raw, &obj); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		if obj["kind"] != "Secret" {
			t.Fatalf("%s: not a Secret", file)
		}
		var out strings.Builder
		if err := tmpl.Execute(&out, obj); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		want, err := os.ReadFile(strings.TrimSuffix(file, ".json") + ".want")
		if err != nil {
			t.Fatal(err)
		}
		// The .want file holds the output in brackets: it can end in a space.
		if got := "[" + out.String() + "]\n"; got != string(want) {
			t.Errorf("%s: the read prints %q, the .want file has %q", filepath.Base(file), got, want)
		}
	}
}
