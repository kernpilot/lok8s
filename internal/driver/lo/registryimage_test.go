package lo

// registryimage_test.go — the explicit registry image pull before a
// registry-set container is created (registryimage.go): the local image
// skips the pull, a failed pull is retried with the 2/5/10 s waits, the
// last failure names its cause, and a running registry is never removed
// for an image that cannot be pulled. Every docker call goes through the
// file-backed fake; the waits go through the sleep seam.

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kernpilot/lok8s/internal/execx"
)

const (
	imageInspectCall = "docker image inspect -f {{.Id}} " + RegistryImage
	pullCall         = "docker pull " + RegistryImage
	// pullCause is a docker pull stderr for a transient network error.
	pullCause = `Error response from daemon: Get "https://registry-1.docker.io/v2/": dial tcp: lookup registry-1.docker.io: i/o timeout`
)

// pullScript answers `docker image inspect` and `docker pull` for the
// fake docker: the image is local when present is true; otherwise the
// first failures pulls fail with stderr and the next one succeeds.
type pullScript struct {
	present  bool
	failures int
	stderr   string
	pulls    int
}

func (s *pullScript) install(fd *fakeDocker) {
	fd.wrap = func(c execx.Cmd) (bool, error) {
		switch {
		case len(c.Args) >= 2 && c.Args[0] == "image" && c.Args[1] == "inspect":
			if s.present {
				writeOut(c, "sha256:0123\n")
				return true, nil
			}
			writeErr(c, "Error response from daemon: No such image: "+RegistryImage+"\n")
			return true, fmt.Errorf("exit 1")
		case len(c.Args) >= 1 && c.Args[0] == "pull":
			s.pulls++
			if s.pulls <= s.failures {
				writeErr(c, s.stderr)
				return true, fmt.Errorf("exit 1")
			}
			s.present = true
			return true, nil
		}
		return false, nil
	}
}

// recordSleeps replaces the sleep seam with one that records each wait.
func recordSleeps(d *Driver) *[]time.Duration {
	var waits []time.Duration
	d.sleep = func(ctx context.Context, w time.Duration) error {
		waits = append(waits, w)
		return ctx.Err()
	}
	return &waits
}

func countCalls(log []string, want string) int {
	n := 0
	for _, l := range log {
		if l == want {
			n++
		}
	}
	return n
}

func TestRegistriesSkipThePullWhenTheImageIsLocal(t *testing.T) {
	d, _, fd, _, _, cy := lifecycleDriver(t)
	(&pullScript{present: true}).install(fd)

	out, errOut, err := runRegistries(t, d, cy)
	if err != nil {
		t.Fatalf("registries: %v\n%s", err, errOut)
	}
	if !strings.Contains(out, "registry/lok8s-registry-build created") {
		t.Fatalf("build registry not created:\n%s", out)
	}
	if n := countCalls(fd.log, pullCall); n != 0 {
		t.Fatalf("pulled a local image %d time(s):\n%s", n, strings.Join(fd.log, "\n"))
	}
	// Checked once for the whole set, before the first container.
	if n := countCalls(fd.log, imageInspectCall); n != 1 {
		t.Fatalf("image inspected %d times, want 1:\n%s", n, strings.Join(fd.log, "\n"))
	}
	if i, r := slices.Index(fd.log, imageInspectCall), slices.IndexFunc(fd.log, func(l string) bool { return strings.HasPrefix(l, "docker run ") }); i < 0 || r < i {
		t.Fatalf("image inspect (%d) does not precede the first docker run (%d):\n%s", i, r, strings.Join(fd.log, "\n"))
	}
}

func TestRegistriesRetryAFailedPullThenCreate(t *testing.T) {
	d, _, fd, _, _, cy := lifecycleDriver(t)
	waits := recordSleeps(d)
	(&pullScript{failures: 1, stderr: pullCause + "\n"}).install(fd)

	out, errOut, err := runRegistries(t, d, cy)
	if err != nil {
		t.Fatalf("registries: %v\n%s", err, errOut)
	}
	for _, want := range []string{
		"registry/lok8s-registry-build created",
		"registry/lok8s-registry-cache created",
		"registry/lok8s-registry-io-docker created",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if n := countCalls(fd.log, pullCall); n != 2 {
		t.Fatalf("pulled %d times, want 2:\n%s", n, strings.Join(fd.log, "\n"))
	}
	if !slices.Equal(*waits, []time.Duration{2 * time.Second}) {
		t.Fatalf("waits = %v, want [2s]", *waits)
	}
	wantWarn := "[warn] docker pull " + RegistryImage + " failed (attempt 1 of 4): " + pullCause + ". Retrying in 2s.\n"
	if errOut != wantWarn {
		t.Fatalf("stderr = %q\nwant     %q", errOut, wantWarn)
	}
}

func TestRegistriesReportTheCauseWhenEveryPullFails(t *testing.T) {
	d, _, fd, _, _, cy := lifecycleDriver(t)
	waits := recordSleeps(d)
	(&pullScript{failures: 99, stderr: pullCause + "\n\n"}).install(fd)

	out, errOut, err := runRegistries(t, d, cy)
	if err == nil {
		t.Fatalf("registries succeeded without the image:\n%s", out)
	}
	wantErr := "error: docker pull " + RegistryImage + " failed after 4 attempts: " + pullCause + "\n"
	if !strings.HasSuffix(errOut, wantErr) {
		t.Fatalf("stderr does not end with the cause:\n%s\nwant suffix %q", errOut, wantErr)
	}
	if strings.Count(errOut, "error: docker pull") != 1 {
		t.Fatalf("the pull error is not printed exactly once:\n%s", errOut)
	}
	if !slices.Equal(*waits, []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second}) {
		t.Fatalf("waits = %v, want [2s 5s 10s]", *waits)
	}
	// One pull series for the set: the result is kept, the other
	// registries do not pull again.
	if n := countCalls(fd.log, pullCall); n != 4 {
		t.Fatalf("pulled %d times, want 4:\n%s", n, strings.Join(fd.log, "\n"))
	}
	for _, l := range fd.log {
		if strings.HasPrefix(l, "docker run ") {
			t.Fatalf("a container was started without the image: %s", l)
		}
	}
}

// An image change recreates a running registry. When the new image cannot
// be pulled, the running container stays.
func TestRegistriesKeepARunningContainerWhenThePullFails(t *testing.T) {
	d, _, fd, _, _, cy := lifecycleDriver(t)
	if _, errOut, err := runRegistries(t, d, cy); err != nil {
		t.Fatalf("first run: %v\n%s", err, errOut)
	}
	fd.setContainer("lok8s-registry-build", "running", "stale-hash")
	d.registryImage.done = false // a new process
	(&pullScript{failures: 99, stderr: pullCause + "\n"}).install(fd)
	fd.log = nil

	if _, _, err := runRegistries(t, d, cy); err == nil {
		t.Fatal("registries succeeded without the image")
	}
	if status, _, ok := fd.containerStatus("lok8s-registry-build"); !ok || status != "running" {
		t.Fatalf("the running registry was removed (status %q, present %v):\n%s", status, ok, strings.Join(fd.log, "\n"))
	}
	if slices.Contains(fd.log, "docker rm -f lok8s-registry-build") {
		t.Fatalf("docker rm ran before the image was there:\n%s", strings.Join(fd.log, "\n"))
	}
}

// The registry TLS volume is written through a throwaway container of the
// registry image: the pull runs before that create, with the retry.
func TestRegistriesTLSCertPullsBeforeTheIOContainer(t *testing.T) {
	d, runner, fd, _, p, _ := tlsDriver(t, "true")
	stubSecretPlugin(t, runner, p.Base)
	waits := recordSleeps(d)
	(&pullScript{failures: 2, stderr: pullCause + "\n"}).install(fd)

	var errOut strings.Builder
	if err := d.registriesTLSCert(t.Context(), tlsDomain, &errOut); err != nil {
		t.Fatalf("mint: %v\n%s", err, errOut.String())
	}
	create := "docker container create --name " + tlsVolIO + " --volume " + tlsVol + ":" + RegistryTLSMount + " " + RegistryImage
	lastPull := slices.Index(fd.log, create) - 1
	if lastPull < 0 || fd.log[lastPull] != pullCall {
		t.Fatalf("the io container is not created right after the pull:\n%s", strings.Join(fd.log, "\n"))
	}
	if n := countCalls(fd.log, pullCall); n != 3 {
		t.Fatalf("pulled %d times, want 3:\n%s", n, strings.Join(fd.log, "\n"))
	}
	if !slices.Equal(*waits, []time.Duration{2 * time.Second, 5 * time.Second}) {
		t.Fatalf("waits = %v, want [2s 5s]", *waits)
	}
	if _, ok := fd.volumeFile(tlsVol, "tls.crt"); !ok {
		t.Fatal("the cert did not reach the volume")
	}
}

func TestRegistriesTLSCertStopsOnAFailedPull(t *testing.T) {
	d, runner, fd, _, p, _ := tlsDriver(t, "true")
	stubSecretPlugin(t, runner, p.Base)
	recordSleeps(d)
	(&pullScript{failures: 99, stderr: pullCause + "\n"}).install(fd)

	var errOut strings.Builder
	if err := d.registriesTLSCert(t.Context(), tlsDomain, &errOut); err == nil {
		t.Fatal("the mint succeeded without the image")
	}
	if !strings.Contains(errOut.String(), "error: docker pull "+RegistryImage+" failed after 4 attempts: "+pullCause+"\n") {
		t.Fatalf("stderr lacks the cause:\n%s", errOut.String())
	}
	if slices.ContainsFunc(fd.log, func(l string) bool { return strings.HasPrefix(l, "docker container create ") }) {
		t.Fatalf("the io container was created without the image:\n%s", strings.Join(fd.log, "\n"))
	}
}

// The defect: docker prints the implicit-pull notice first and the cause
// after it. The report names the cause.
func TestRegistriesTLSCertReportsTheCauseOfAFailedCreate(t *testing.T) {
	d, runner, fd, _, p, _ := tlsDriver(t, "true")
	stubSecretPlugin(t, runner, p.Base)
	fd.wrap = func(c execx.Cmd) (bool, error) {
		if len(c.Args) >= 2 && c.Args[0] == "container" && c.Args[1] == "create" {
			writeErr(c, "Unable to find image '"+RegistryImage+"' locally\n"+pullCause+"\n")
			return true, fmt.Errorf("exit 125")
		}
		return false, nil
	}

	var errOut strings.Builder
	if err := d.registriesTLSCert(t.Context(), tlsDomain, &errOut); err == nil {
		t.Fatal("the mint treated a docker failure as success")
	}
	want := "error: docker container create " + tlsVolIO + " (volume " + tlsVol + ") failed: " + pullCause + "\n"
	if !strings.Contains(errOut.String(), want) {
		t.Fatalf("stderr:\n%s\nwant line %q", errOut.String(), want)
	}
}

// A cancelled context ends the retries without a [warn] or an error line,
// and it is not kept as the result: the next call checks and pulls again.
func TestEnsureRegistryImageCancelledIsNotKept(t *testing.T) {
	d, _, fd, _, _, _ := lifecycleDriver(t)
	ps := &pullScript{failures: 1, stderr: pullCause + "\n"}
	ps.install(fd)
	ctx, cancel := context.WithCancel(t.Context())
	d.sleep = func(context.Context, time.Duration) error {
		cancel()
		return context.Canceled
	}

	var errOut strings.Builder
	if err := d.ensureRegistryImage(ctx, &errOut); err == nil {
		t.Fatal("a cancelled pull reported success")
	}
	if d.registryImage.done {
		t.Fatal("a cancelled pull was kept as the result")
	}
	if strings.Contains(errOut.String(), "error: docker pull") {
		t.Fatalf("a cancelled pull printed the failure line:\n%s", errOut.String())
	}

	errOut.Reset()
	if err := d.ensureRegistryImage(t.Context(), &errOut); err != nil {
		t.Fatalf("the next call did not pull again: %v\n%s", err, errOut.String())
	}
	if ps.pulls != 2 {
		t.Fatalf("pulls = %d, want 2 (one cancelled, one on the next call)", ps.pulls)
	}
}

func TestDockerErrSummary(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"empty", "", ""},
		{"one line", pullCause + "\n", pullCause},
		{"implicit pull notice and help hint dropped",
			"Unable to find image 'registry:2.8.3' locally\n" + "docker: " + pullCause + "\n\nRun 'docker run --help' for more information\n",
			"docker: " + pullCause},
		{"older help hint dropped",
			"docker: Error response from daemon: Conflict.\nSee 'docker run --help'.\n",
			"docker: Error response from daemon: Conflict."},
		{"last three lines kept",
			"one\ntwo  \r\n\t\nthree\nfour\n",
			"two | three | four"},
		{"URL credentials masked",
			`Error response from daemon: Get "https://robot:s3cret@registry.example/v2/": proxyconnect tcp: http://u:p@proxy:3128 refused`,
			`Error response from daemon: Get "https://***@registry.example/v2/": proxyconnect tcp: http://***@proxy:3128 refused`},
		{"a raw @ in the password is masked too",
			`Get "https://u:p@ss@proxy.example:3128/v2/": EOF`,
			`Get "https://***@proxy.example:3128/v2/": EOF`},
		{"a URL without credentials is kept",
			`Get "https://registry-1.docker.io/v2/": EOF and user@host`,
			`Get "https://registry-1.docker.io/v2/": EOF and user@host`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := dockerErrSummary(tc.in); got != tc.want {
				t.Fatalf("dockerErrSummary(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}
