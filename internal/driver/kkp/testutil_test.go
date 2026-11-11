package kkp

// testutil_test.go holds the fixtures the kkp tests share: the fake
// Runner that stands in for curl, the scripted responder, the driver over a
// temp project, the spec fixture and the env the bash tests export. Nothing
// here reaches a KKP api.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"flag"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/testutil"
)

// update rewrites the golden files with the current output:
// go test ./internal/driver/kkp/ -update
var update = flag.Bool("update", false, "rewrite the golden files")

type fakeRunner struct {
	calls   []execx.Cmd
	stdins  []string
	handler func(c execx.Cmd) error
}

func (r *fakeRunner) Run(ctx context.Context, c execx.Cmd) error {
	stdin := ""
	if c.Stdin != nil {
		var b bytes.Buffer
		_, _ = b.ReadFrom(c.Stdin)
		stdin = b.String()
	}
	// The handler reads stdin again (the curl config, configBody).
	c.Stdin = strings.NewReader(stdin)
	r.calls = append(r.calls, c)
	r.stdins = append(r.stdins, stdin)
	if r.handler != nil {
		return r.handler(c)
	}
	return nil
}

func argvLine(c execx.Cmd) string { return c.Name + " " + strings.Join(c.Args, " ") }

// configBody returns the request body that the curl config on stdin
// carries (its `data-raw = "…"` line), with curl's unquoting: \n, \r, \t
// and \v are the control bytes, a backslash before any other byte is that
// byte. "" when the config has no body.
func configBody(c execx.Cmd) string {
	if c.Stdin == nil {
		return ""
	}
	var b bytes.Buffer
	_, _ = b.ReadFrom(c.Stdin)
	for line := range strings.SplitSeq(b.String(), "\n") {
		v, ok := strings.CutPrefix(line, `data-raw = "`)
		if !ok || !strings.HasSuffix(v, `"`) {
			continue
		}
		v = strings.TrimSuffix(v, `"`)
		var out strings.Builder
		for i := 0; i < len(v); i++ {
			if v[i] != '\\' || i+1 == len(v) {
				out.WriteByte(v[i])
				continue
			}
			i++
			switch v[i] {
			case 'n':
				out.WriteByte('\n')
			case 'r':
				out.WriteByte('\r')
			case 't':
				out.WriteByte('\t')
			case 'v':
				out.WriteByte('\v')
			default:
				out.WriteByte(v[i])
			}
		}
		return out.String()
	}
	return ""
}

// curlRespond scripts curl's captured stream: body then the write-out line
// (`\n%{http_code}`), both into the shared 2>&1 buffer.
func curlRespond(body string, code string) func(c execx.Cmd) error {
	return func(c execx.Cmd) error {
		fmt.Fprintf(c.Stdout, "%s\n%s", body, code)
		return nil
	}
}

func testDriver(t *testing.T) (*Driver, *fakeRunner, *bytes.Buffer) {
	t.Helper()
	base := t.TempDir()
	runner := &fakeRunner{}
	var stderr bytes.Buffer
	paths := &config.Paths{
		Base:     base,
		Bin:      filepath.Join(base, ".bin"),
		Lok8s:    filepath.Join(base, ".lok8s"),
		Clusters: filepath.Join(base, "clusters"),
	}
	d := New(&driver.Deps{Paths: paths, Runner: runner, Stderr: &stderr})
	// Deterministic clock: the wall advances only through the sleep seam
	// (the bash loops measured `date +%s` in real time; the fake keeps the
	// same arithmetic without the waiting).
	clock := time.Unix(0, 0)
	d.now = func() time.Time { return clock }
	d.sleep = func(_ context.Context, dur time.Duration) error { clock = clock.Add(dur); return nil }
	return d, runner, &stderr
}

func writeSpec(t *testing.T, d *Driver, domain, yaml string) string {
	t.Helper()
	path := filepath.Join(d.deps.Paths.Clusters, domain, "cluster.lok8s.yaml")
	testutil.WriteFile(t, path, yaml)
	return path
}

func kkpFixture(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "..", "tests", "fixtures", "kkp-cluster.lok8s.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func setKKPEnv(t *testing.T) {
	t.Helper()
	t.Setenv("KKP_TOKEN", "test-kkp-token-abc123")
	t.Setenv("KKP_API_URL", "https://kkp.test.example.com")
	t.Setenv("KKP_CA_CERT", "")
	os.Unsetenv("KKP_CA_CERT")
	t.Setenv("KKP_RETRY_DELAY", "0")
	t.Setenv("KKP_WAIT_INTERVAL", "1")
}

const allUpHealth = `{"apiserver":"HealthStatusUp","etcd":"HealthStatusUp","controller":"HealthStatusUp","scheduler":"HealthStatusUp","machineController":"HealthStatusDown"}`
