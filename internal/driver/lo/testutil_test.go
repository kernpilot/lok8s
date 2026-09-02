package lo

// testutil_test.go — hermetic test infrastructure. ALL external tools run
// through the fake runner; no real docker/kind/kubectl/ssh is ever invoked
// (live kind clusters exist on dev machines — hermeticity here is a safety
// property, not just speed).
//
// The file-backed fake docker mirrors the bats fake in
// tests/unit/registry_lifecycle_test.bats: containers/<name> holds
// "status\nhash", networks/<net> holds "ip/prefix name" member lines,
// networks/<net>.meta holds "subnet\nrange" — enough state for the
// reconcile matrix to be exercised end-to-end without a daemon.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/execx"
)

type fakeRunner struct {
	t *testing.T
	// calls records every invocation as "name arg1 arg2 …".
	calls []string
	// handler answers an invocation; nil handler = success, no output.
	handler func(c execx.Cmd) error
}

func (r *fakeRunner) Run(ctx context.Context, c execx.Cmd) error {
	r.calls = append(r.calls, strings.Join(append([]string{c.Name}, c.Args...), " "))
	if r.handler != nil {
		return r.handler(c)
	}
	return nil
}

func (r *fakeRunner) callsMatching(sub string) []string {
	var out []string
	for _, c := range r.calls {
		if strings.Contains(c, sub) {
			out = append(out, c)
		}
	}
	return out
}

func writeOut(c execx.Cmd, s string) {
	if c.Stdout != nil {
		io.WriteString(c.Stdout, s)
	}
}

func writeErr(c execx.Cmd, s string) {
	if c.Stderr != nil {
		io.WriteString(c.Stderr, s)
	}
}

// testDriver builds a Driver over a temp project tree + fake runner. The
// sleep seam is a no-op (waits become instant).
func testDriver(t *testing.T) (*Driver, *fakeRunner, *bytes.Buffer, *config.Paths) {
	t.Helper()
	base := t.TempDir()
	p := &config.Paths{
		Base:     base,
		Bin:      filepath.Join(base, ".bin"),
		Lok8s:    filepath.Join(base, ".lok8s"),
		Clusters: filepath.Join(base, "clusters"),
	}
	runner := &fakeRunner{t: t}
	var errBuf bytes.Buffer
	d := New(&driver.Deps{Paths: p, Runner: runner, Stderr: &errBuf})
	d.sleep = func(time.Duration) {}
	d.SetOutput(io.Discard)

	// Env hygiene: the driver communicates through process env exactly like
	// the bash; scrub the cross-test bleed (t.Setenv restores on cleanup).
	for _, k := range []string{
		"LOK8S_REGISTRY_JSON", "LOK8S_NETWORK_BASE_IP", "LOK8S_NETWORK_SUBNET",
		"LOK8S_NETWORK_CIDR", "KIND_EXPERIMENTAL_DOCKER_NETWORK",
		"LOK8S_LB_POOL", "LOK8S_REMOTE", "LOK8S_REMOTE_IP", "LOK8S_REMOTE_USER",
		"LOK8S_REMOTE_MODE", "LOK8S_REMOTE_EXPOSE", "LOK8S_REMOTE_SYNC_PATH",
		"LOK8S_REMOTE_SYNC_DEST", "LOK8S_REMOTE_SYNC_EXCLUDE", "LOK8S_REMOTE_TILT",
		"LOK8S_CP_COUNT", "LOK8S_WORKER_COUNT", "LOK8S_HOST_PORTS",
		"LOK8S_EXTRA_MOUNTS_COUNT", "LOK8S_MAX_CONCURRENT_DOWNLOADS",
		"LOK8S_SPEC_OIDC_ISSUER", "LOK8S_SPEC_OIDC_CLIENTID", "DOMAIN_NAME",
		"CAROOT", "PATH_SECRETS", "KUSTOMIZE_PLUGIN_HOME", "DEBUG", "KAPPLY_TTY",
	} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	t.Setenv("LO_REGISTRY_STATE_DIR", filepath.Join(base, "registry-state"))
	// kapply off-tty passthrough in tests (like the bats
	// LOK8S_NONINTERACTIVE=1).
	t.Setenv("LOK8S_NONINTERACTIVE", "1")

	return d, runner, &errBuf, p
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFileT(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

// specShared is the registry_lifecycle_test.bats spec: plain-HTTP, one
// shared pull-through mirror, slot-free explicit network.
const specShared = `apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: test-lifecycle
spec:
  cluster:
    domain: test.lok8s.dev
  network:
    name: lok8s
    cidr: "10.125.50.0/24"
  registries:
    tls: false
    shared:
      enabled: true
      network:
        name: lok8s-registries
        cidr: "10.125.200.0/24"
    mirrors:
      - name: io-docker
        url: https://registry-1.docker.io
  runtime: kind
  bootstrap: []
`

// writeLifecycleFixture writes the spec + the driver's registry config
// templates into the temp tree (the bats vendored these the same way).
func writeLifecycleFixture(t *testing.T, p *config.Paths) string {
	t.Helper()
	cy := filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml")
	writeFile(t, cy, specShared)
	regDir := filepath.Join(p.Lok8s, "drivers", "lo", "cluster", "registry")
	writeFile(t, filepath.Join(regDir, "build.yaml"), "version: 0.1\n")
	writeFile(t, filepath.Join(regDir, "cache.yaml"), "version: 0.1\n")
	// The real mirror template (no per-mirror io-docker.yaml, matching the
	// shipped config dir) so the ${REMOTE_URL} substitution is exercised
	// via fallback.
	writeFile(t, filepath.Join(regDir, "mirror.yaml"), realMirrorTemplate(t))
	return cy
}

// realMirrorTemplate reads the SHIPPED mirror.yaml from the repo — the
// render tests must exercise the real http:/proxy: stanzas, not stubs.
func realMirrorTemplate(t *testing.T) string {
	t.Helper()
	root := repoRoot(t)
	return readFileT(t, filepath.Join(root, ".lok8s", "drivers", "lo", "cluster", "registry", "mirror.yaml"))
}

func realRegistryTemplate(t *testing.T, name string) string {
	t.Helper()
	root := repoRoot(t)
	return readFileT(t, filepath.Join(root, ".lok8s", "drivers", "lo", "cluster", "registry", name))
}

// repoRoot finds the lok8s repo root from the test's working directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for d := dir; ; {
		if _, err := os.Stat(filepath.Join(d, ".lok8s", "lo")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			t.Fatalf("repo root not found from %s", dir)
		}
		d = parent
	}
}

// ── file-backed fake docker ──────────────────────────────

type fakeDocker struct {
	t   *testing.T
	dir string
	// log records every docker invocation, "docker <argv…>" (like the bats
	// DOCKER_LOG).
	log []string
	// wrap intercepts a call before the fake handles it; return handled=true
	// to override (the bats `eval _real_docker` pattern).
	wrap func(c execx.Cmd) (handled bool, err error)
}

func newFakeDocker(t *testing.T) *fakeDocker {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"containers", "networks"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f := &fakeDocker{t: t, dir: dir}
	f.createNetwork("lok8s", "", "")
	f.createNetwork("lok8s-registries", "", "")
	return f
}

func (f *fakeDocker) containerPath(name string) string {
	return filepath.Join(f.dir, "containers", name)
}
func (f *fakeDocker) networkPath(name string) string {
	return filepath.Join(f.dir, "networks", name)
}

func (f *fakeDocker) setContainer(name, status, hash string) {
	os.WriteFile(f.containerPath(name), []byte(status+"\n"+hash+"\n"), 0o644)
}

func (f *fakeDocker) containerStatus(name string) (status, hash string, ok bool) {
	raw, err := os.ReadFile(f.containerPath(name))
	if err != nil {
		return "", "", false
	}
	lines := strings.SplitN(string(raw), "\n", 3)
	status = lines[0]
	if len(lines) > 1 {
		hash = lines[1]
	}
	return status, hash, true
}

func (f *fakeDocker) createNetwork(name, subnet, ipRange string) {
	os.WriteFile(f.networkPath(name), nil, 0o644)
	os.WriteFile(f.networkPath(name)+".meta", []byte(subnet+"\n"+ipRange+"\n"), 0o644)
}

func (f *fakeDocker) setNetworkMeta(name, subnet, ipRange string) {
	os.WriteFile(f.networkPath(name)+".meta", []byte(subnet+"\n"+ipRange+"\n"), 0o644)
}

func (f *fakeDocker) networkMeta(name string) (subnet, ipRange string) {
	raw, err := os.ReadFile(f.networkPath(name) + ".meta")
	if err != nil {
		return "", ""
	}
	lines := strings.SplitN(string(raw), "\n", 3)
	subnet = lines[0]
	if len(lines) > 1 {
		ipRange = lines[1]
	}
	return subnet, ipRange
}

func (f *fakeDocker) addMember(network, ipWithPrefix, name string) {
	fp, _ := os.OpenFile(f.networkPath(network), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	fmt.Fprintf(fp, "%s %s\n", ipWithPrefix, name)
	fp.Close()
}

func (f *fakeDocker) removeMember(network, name string) {
	raw, err := os.ReadFile(f.networkPath(network))
	if err != nil {
		return
	}
	var kept []string
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if line == "" || strings.HasSuffix(line, " "+name) {
			continue
		}
		kept = append(kept, line)
	}
	content := strings.Join(kept, "\n")
	if content != "" {
		content += "\n"
	}
	os.WriteFile(f.networkPath(network), []byte(content), 0o644)
}

func (f *fakeDocker) members(network string) []string {
	raw, err := os.ReadFile(f.networkPath(network))
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func (f *fakeDocker) hasMemberIP(network, ipWithPrefix, name string) bool {
	for _, m := range f.members(network) {
		if m == ipWithPrefix+" "+name {
			return true
		}
	}
	return false
}

// handle answers a docker execx.Cmd against the file-backed state.
func (f *fakeDocker) handle(c execx.Cmd) error {
	if c.Name != "docker" {
		return nil
	}
	f.log = append(f.log, strings.Join(append([]string{"docker"}, c.Args...), " "))
	if f.wrap != nil {
		if handled, err := f.wrap(c); handled {
			return err
		}
	}
	args := c.Args
	if len(args) == 0 {
		return nil
	}
	switch args[0] {
	case "inspect":
		format := ""
		rest := args[1:]
		if len(rest) >= 2 && rest[0] == "-f" {
			format = rest[1]
			rest = rest[2:]
		}
		if len(rest) == 0 {
			return fmt.Errorf("no target")
		}
		target := rest[0]
		status, hash, ok := f.containerStatus(target)
		if !ok {
			return fmt.Errorf("no such container")
		}
		if strings.Contains(format, "State.Status") {
			writeOut(c, status+"\n")
		} else if strings.Contains(format, "config-hash") {
			writeOut(c, hash+"\n")
		}
		return nil
	case "volume":
		return nil
	case "rm":
		target := args[len(args)-1]
		os.Remove(f.containerPath(target))
		// docker rm -f releases the container's endpoints — mirror that.
		nets, _ := os.ReadDir(filepath.Join(f.dir, "networks"))
		for _, n := range nets {
			if strings.HasSuffix(n.Name(), ".meta") {
				continue
			}
			f.removeMember(n.Name(), target)
		}
		return nil
	case "run":
		var name, ip, net, hash string
		for i := 0; i < len(args); i++ {
			switch {
			case args[i] == "--name" && i+1 < len(args):
				name = args[i+1]
			case args[i] == "--ip" && i+1 < len(args):
				ip = args[i+1]
			case strings.HasPrefix(args[i], "--net="):
				net = strings.TrimPrefix(args[i], "--net=")
			case args[i] == "--label" && i+1 < len(args):
				hash = strings.TrimPrefix(args[i+1], "lok8s.dev/config-hash=")
			}
		}
		for _, m := range f.members(net) {
			if strings.HasPrefix(m, ip+"/") {
				// Real docker leaves the container behind in Created state.
				f.setContainer(name, "created", hash)
				writeErr(c, "docker: failed to set up container networking: Address already in use\n")
				return fmt.Errorf("exit 125")
			}
		}
		f.setContainer(name, "running", hash)
		f.addMember(net, ip+"/24", name)
		writeOut(c, "cid-"+name+"\n")
		return nil
	case "network":
		sub := args[1]
		rest := args[2:]
		switch sub {
		case "inspect":
			var format, netName string
			for i := 0; i < len(rest); i++ {
				switch {
				case rest[i] == "--format" || rest[i] == "-f":
					if i+1 < len(rest) {
						format = rest[i+1]
						i++
					}
				case netName == "":
					netName = rest[i]
				}
			}
			if _, err := os.Stat(f.networkPath(netName)); err != nil {
				return fmt.Errorf("no such network")
			}
			subnet, ipRange := f.networkMeta(netName)
			switch {
			case strings.Contains(format, "Subnet"):
				writeOut(c, subnet+"\n")
			case strings.Contains(format, "IPRange"):
				writeOut(c, ipRange+"\n")
			case strings.Contains(format, "IPv4Address"):
				// ORDER MATTERS (mirrors the bats fake): the holder lookup's
				// template contains IPv4Address AND Containers/Name — this
				// branch must win.
				for _, m := range f.members(netName) {
					writeOut(c, m+"\n")
				}
			case strings.Contains(format, "Containers") && strings.Contains(format, "Name"):
				for _, m := range f.members(netName) {
					fields := strings.Fields(m)
					if len(fields) >= 2 {
						writeOut(c, fields[1]+"\n")
					}
				}
			default:
				for _, m := range f.members(netName) {
					writeOut(c, m+"\n")
				}
			}
			return nil
		case "disconnect":
			r := rest
			if len(r) > 0 && r[0] == "-f" {
				r = r[1:]
			}
			if len(r) >= 2 {
				f.removeMember(r[0], r[1])
			}
			return nil
		case "connect":
			if len(rest) >= 2 {
				f.addMember(rest[0], "10.125.200.240/24", rest[1])
			}
			return nil
		case "create":
			var subnet, ipRange string
			name := rest[len(rest)-1]
			for i := 0; i < len(rest); i++ {
				switch rest[i] {
				case "--subnet":
					subnet = rest[i+1]
				case "--ip-range":
					ipRange = rest[i+1]
				}
			}
			f.createNetwork(name, subnet, ipRange)
			return nil
		case "rm":
			r := rest
			// Real docker: rm REFUSES a network with active endpoints; -f
			// only suppresses the not-found error (verified live
			// 2026-08-18).
			if len(r) > 0 && r[0] == "-f" {
				r = r[1:]
			}
			if len(r) == 0 {
				return nil
			}
			if len(f.members(r[0])) > 0 {
				writeErr(c, fmt.Sprintf("Error response from daemon: error while removing network: network %s has active endpoints\n", r[0]))
				return fmt.Errorf("exit 1")
			}
			os.Remove(f.networkPath(r[0]))
			os.Remove(f.networkPath(r[0]) + ".meta")
			return nil
		}
	}
	return nil
}

// lifecycleDriver wires a test driver whose runner routes docker to the
// file-backed fake and reads the lifecycle network config.
func lifecycleDriver(t *testing.T) (*Driver, *fakeRunner, *fakeDocker, *bytes.Buffer, *config.Paths, string) {
	t.Helper()
	d, runner, errBuf, p := testDriver(t)
	fd := newFakeDocker(t)
	runner.handler = fd.handle
	cy := writeLifecycleFixture(t, p)
	if err := readNetworkConfig(cy, errBuf); err != nil {
		t.Fatalf("readNetworkConfig: %v\nstderr: %s", err, errBuf.String())
	}
	return d, runner, fd, errBuf, p, cy
}
