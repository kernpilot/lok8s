// Package image is the Go port of .lok8s/libs/image — pre-pull private
// images into the local cache registry.
//
// Bridges the credential gap between the dev's docker host (which has
// upstream creds) and kind's containerd (which doesn't). Pulls
// `<endpoint>/<branch>/<svc>:<tag>` to the host, retags it to
// `lok8s.cache/<branch>/<svc>:<tag>`, pushes to the cache registry.
//
// Reads its work list from clusters/<domain>/artifacts/.cache-queue,
// written by env.Kustomization for every service with build:false +
// resolved registry.endpoint. All docker/network access goes through
// injectable seams (Runner / HTTPGet) — tests never touch a live daemon.
package image

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/domain"
	"github.com/kernpilot/lok8s/internal/env"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/ui"
)

// ErrHandled marks a failure whose message was already printed in the bash
// [error] format; callers exit non-zero without printing anything further.
var ErrHandled = ui.ErrHandled // one sentinel for every package; see internal/ui

// Context carries one image-command invocation.
type Context struct {
	Paths  *config.Paths
	Runner execx.Runner
	Out    io.Writer
	ErrOut io.Writer
	// Domain is the resolved active domain (bash:
	// `${domain:-${DOMAIN_NAME:-lok8s.dev}}`).
	Domain string
}

// registryTLS reports whether the domain's cache registry serves TLS (bash:
// image::_registry_tls). Reads the generated .registries.json (the source of
// truth, also used by the driver) — LOK8S_REGISTRY_JSON when set, else the
// domain's file. Falls back to plain HTTP (false) when the file is absent —
// matching the back-compat default. jq -e '.tls' semantics: unreadable or
// invalid JSON, and a false/null value, all read as "not TLS".
//
// It used to read DOMAIN_NAME only, while List and Cache honoured
// `--domain X`, so `lo image list --domain X` could pair X's cache IP with
// the OTHER domain's TLS scheme and fail every request (issue #89).
func (c *Context) registryTLS(target string) bool {
	path := os.Getenv("LOK8S_REGISTRY_JSON")
	if path == "" || !fileExists(path) {
		path = c.Paths.Clusters + "/" + target + "/.registries.json"
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var doc struct {
		TLS *bool `json:"tls"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return false
	}
	return doc.TLS != nil && *doc.TLS
}

// cacheNet is the resolved cache-registry coordinates for one invocation.
type cacheNet struct {
	ip string
	// tls is non-nil when the IP was resolved from the cluster spec: the
	// bash path regenerated .registries.json from the spec right before
	// reading it, so the effective TLS answer WAS the spec's. nil → the
	// env-override path, which reads the existing file (registryTLS).
	tls *bool
}

func (n *cacheNet) tlsFor(c *Context, target string) bool {
	if n.tls != nil {
		return *n.tls
	}
	return c.registryTLS(target)
}

// resolveCacheNet resolves the cache registry IP (bash: the lazy
// `lo::read_network_config` sourcing in image::cache / image::list).
//
// DOCUMENTED SIMPLIFICATION: the bash sourced the whole Lo driver, which
// also REGENERATED clusters/<domain>/.registries.json as a side effect. This
// port computes the same values from the spec (spec.network with the
// *.lok8s.dev slot defaults, cache IP = base + 102, TLS =
// spec.registries.tls default true) without rewriting the file — the same
// validations gate the result, so the success/failure behavior and the
// resolved values match; only the file refresh is skipped.
func (c *Context) resolveCacheNet() cacheNet {
	if ip := os.Getenv("LOK8S_REGISTRY_IP_CACHE"); ip != "" {
		return cacheNet{ip: ip}
	}
	spec := c.Paths.Clusters + "/" + c.Domain + "/cluster.lok8s.yaml"
	raw, err := os.ReadFile(spec)
	if err != nil {
		return cacheNet{}
	}
	var root yaml.Node
	if yaml.Unmarshal(raw, &root) != nil {
		return cacheNet{}
	}

	netName := env.ScalarOr(&root, "", "spec", "network", "name")
	netCIDR := env.ScalarOr(&root, "", "spec", "network", "cidr")
	if netName == "" || netCIDR == "" {
		if slot := slotFromSpec(&root); slot != "" {
			if netName == "" {
				netName = env.ScalarOr(&root, "", "metadata", "name")
			}
			if netCIDR == "" {
				netCIDR = "10.125." + slot + ".0/24"
			}
		}
	}
	if netName == "" || netCIDR == "" {
		return cacheNet{}
	}

	// spec.registries.tls must be true/false (default true) — an invalid
	// value failed the bash config generation, and with it the whole IP
	// resolution.
	tlsRaw := env.ToString(&root, "spec", "registries", "tls")
	tls := true
	switch tlsRaw {
	case "false":
		tls = false
	case "true", "null":
	default:
		return cacheNet{}
	}
	// Invalid mirror declarations also failed the bash config generation.
	if !mirrorsValid(&root) {
		return cacheNet{}
	}

	base, _, _ := strings.Cut(netCIDR, "/")
	ip, ok := ipAdd(base, 102) // RegistryOffsetCache
	if !ok {
		return cacheNet{}
	}
	return cacheNet{ip: ip, tls: &tls}
}

var slotRe = regexp.MustCompile(`^([0-9]+)\.lok8s\.dev$`)
var mirrorNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// slotFromSpec mirrors lo::slot_from_domain: spec.cluster.domain
// "lok8s.dev" → 125, "<n>.lok8s.dev" (2..199) → n, else "".
func slotFromSpec(root *yaml.Node) string {
	d := env.ScalarOr(root, "", "spec", "cluster", "domain")
	if d == "" {
		return ""
	}
	if d == "lok8s.dev" {
		return "125"
	}
	if m := slotRe.FindStringSubmatch(d); m != nil {
		if slot, err := strconv.Atoi(m[1]); err == nil && slot >= 2 && slot <= 199 {
			return m[1]
		}
	}
	return ""
}

// mirrorsValid re-runs the mirror validations registry::config_generate
// applied (name shape, reserved names, url presence) — a spec they reject
// left the bash cache-IP resolution empty.
func mirrorsValid(root *yaml.Node) bool {
	mirrors := env.Path(root, "spec", "registries", "mirrors")
	if mirrors == nil || mirrors.Kind != yaml.SequenceNode {
		return true
	}
	for _, m := range mirrors.Content {
		name := env.ToString(m, "name")
		url := env.ScalarOr(m, "", "url")
		if !mirrorNameRe.MatchString(name) {
			return false
		}
		if name == "build" || name == "cache" {
			return false
		}
		if url == "" {
			return false
		}
	}
	return true
}

func ipAdd(ip string, offset int) (string, bool) {
	parts := strings.Split(ip, ".")
	if len(parts) != 4 {
		return "", false
	}
	var n uint32
	for _, p := range parts {
		v, err := strconv.Atoi(p)
		if err != nil || v < 0 || v > 255 {
			return "", false
		}
		n = n<<8 | uint32(v)
	}
	n += uint32(offset) // #nosec G115 -- negative offsets wrap to the intended modular add; masked to four octets below
	return fmt.Sprintf("%d.%d.%d.%d", n>>24, n>>16&0xff, n>>8&0xff, n&0xff), true
}

// mergedServices reads the merged services env once (bash:
// `env::services 2>/dev/null`).
func (c *Context) mergedServices(ctx context.Context) (*yaml.Node, error) {
	e := &env.Context{Paths: c.Paths, Runner: c.Runner, ErrOut: io.Discard, Domain: c.Domain}
	raw, err := e.ServicesRaw(ctx)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	_ = yaml.Unmarshal(raw, &doc)
	return &doc, nil
}

// Cache pre-pulls image(s) into the local cache registry (bash:
// image::cache). Without all, requires a service name and resolves it via
// the committed services.yaml + active config layer. With all, walks the
// .cache-queue file written by env.Kustomization.
//
// Idempotent: skips if the cache registry already has a manifest matching
// the requested ref. force overrides the skip.
func (c *Context) Cache(ctx context.Context, service string, force, all bool) error {
	ui.Debugf(c.ErrOut, "Pre-pull images into the local cache registry")

	// The cache registry is a Lo-driver (local kind cluster) feature — fail
	// at the door with the driver named, not three layers down on a spec
	// field other drivers never carry. An explicit LOK8S_REGISTRY_IP_CACHE
	// skips the gate — see List for why.
	if os.Getenv("LOK8S_REGISTRY_IP_CACHE") == "" {
		if err := domain.RequireDriver("lo", c.Paths.Clusters, c.Domain, "the image cache", c.ErrOut); err != nil {
			return ErrHandled
		}
	}

	net := c.resolveCacheNet()
	// No guessed fallback: a wrong IP burns a connect-timeout per layer and
	// reads as a network problem; an error names itself.
	if net.ip == "" {
		ui.Errorf(c.ErrOut, "cannot resolve the cache registry IP for domain '%s' (spec.network unreadable?) — export LOK8S_REGISTRY_IP_CACHE=<ip> to override", c.Domain)
		return ErrHandled
	}

	queueFile := c.Paths.Clusters + "/" + c.Domain + "/artifacts/.cache-queue"

	// Resolve parallelism (registry.parallel; default 1).
	merged, err := c.mergedServices(ctx)
	if err != nil {
		return err
	}
	parallel := env.ToString(merged, "registry", "parallel")
	if parallel == "null" {
		parallel = "1"
	}
	if !regexp.MustCompile(`^[0-9]+$`).MatchString(parallel) {
		ui.Errorf(c.ErrOut, "registry.parallel must be a non-negative integer, got: %s", parallel)
		return ErrHandled
	}

	if all {
		info, err := os.Stat(queueFile)
		if err != nil || info.Size() == 0 {
			ui.Debugf(c.ErrOut, "no cache queue at %s; nothing to pre-pull", queueFile)
			return nil
		}
		return c.cacheQueue(ctx, queueFile, net, parallel, force)
	}

	// Single-service mode: look the service up in the merged services env to
	// resolve its remote ref.
	if service == "" {
		ui.Errorf(c.ErrOut, "image cache: provide a service name or use --all")
		return ErrHandled
	}

	pinned := env.ScalarOr(merged, "", "services", service, "image")
	if pinned != "" {
		ui.Errorf(c.ErrOut, "service '%s' has an explicit 'image:' pin — there is nothing to cache, kind pulls it directly", service)
		return ErrHandled
	}

	// Resolve effective registry coordinates for this service.
	gEndpoint := subst(env.ScalarOr(merged, "${DOCKER_REGISTRY}", "registry", "endpoint"))
	gBranch := subst(env.ScalarOr(merged, "${DOCKER_PROJECT}", "registry", "branch"))
	gTag := subst(env.ScalarOr(merged, "${DOCKER_TAG}", "registry", "tag"))

	sEndpoint := subst(env.ScalarOr(merged, gEndpoint, "services", service, "registry", "endpoint"))
	sBranch := subst(env.ScalarOr(merged, gBranch, "services", service, "registry", "branch"))
	sTag := subst(env.ScalarOr(merged, gTag, "services", service, "registry", "tag"))

	if sEndpoint == "" {
		ui.Errorf(c.ErrOut, "service '%s' has no registry.endpoint configured (set spec.registries.endpoint or services.%s.registry.endpoint)", service, service)
		return ErrHandled
	}

	remoteRef := sEndpoint + "/" + sBranch + "/" + service + ":" + sTag
	if !c.cacheOne(ctx, service, remoteRef, sBranch, sTag, net, force) {
		return ErrHandled
	}
	return nil
}

// cacheQueue processes a TSV cache queue with bounded parallelism (bash:
// image::_cache_queue). TSV format: svc \t remote_ref \t branch \t tag.
func (c *Context) cacheQueue(ctx context.Context, queue string, net cacheNet, parallelStr string, force bool) error {
	raw, err := os.ReadFile(queue)
	if err != nil {
		return err
	}
	lines := strings.Split(string(raw), "\n")
	// `grep -cv '^$'` — every non-empty line counts, INCLUDING a final
	// unterminated one …
	total := 0
	for _, l := range lines {
		if l != "" {
			total++
		}
	}
	// … while the bash `while read` loop silently DROPS a final line without
	// a trailing newline. Preserve both.
	work := lines[:len(lines)-1]

	parallel, _ := strconv.Atoi(parallelStr)
	ui.Debugf(c.ErrOut, "image cache: processing %d entries (parallel=%s)", total, parallelStr)

	// Failures are recorded per entry index, not exit codes (the bash used
	// marker files because `wait -n` made job rcs unreliably collectable);
	// the final report walks them in the marker-dir glob's LEXICAL index
	// order, preserved here.
	var mu sync.Mutex
	failures := map[string]string{}
	var wg sync.WaitGroup
	var sem chan struct{}
	if parallel > 1 {
		sem = make(chan struct{}, parallel)
	}

	idx := 0
	for _, line := range work {
		fields := strings.Split(line, "\t")
		svc := fields[0]
		var remote, branch, tag string
		if len(fields) > 1 {
			remote = fields[1]
		}
		if len(fields) > 2 {
			branch = fields[2]
		}
		if len(fields) > 3 {
			tag = strings.Join(fields[3:], "\t")
		}
		if svc == "" {
			continue
		}
		idx++
		key := strconv.Itoa(idx)
		one := func() {
			if !c.cacheOne(ctx, svc, remote, branch, tag, net, force) {
				mu.Lock()
				failures[key] = svc + ":" + tag
				mu.Unlock()
			}
		}
		if parallel == 1 {
			one()
			continue
		}
		wg.Add(1)
		if sem != nil {
			sem <- struct{}{}
		}
		go func() {
			defer wg.Done()
			if sem != nil {
				defer func() { <-sem }()
			}
			one()
		}()
	}
	wg.Wait()

	if len(failures) > 0 {
		keys := make([]string, 0, len(failures))
		for k := range failures {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, failures[k])
		}
		ui.Errorf(c.ErrOut, "image cache: %d/%d images failed: %s", len(failures), total, strings.Join(parts, " "))
		return ErrHandled
	}
	return nil
}

// cacheOne is the single-image cache operation (bash: image::_cache_one).
// Idempotent: checks the cache registry's manifest before pulling.
func (c *Context) cacheOne(ctx context.Context, svc, remote, branch, tag string, net cacheNet, force bool) bool {
	cachePath := branch + "/" + svc + ":" + tag
	cacheRefLocal := net.ip + "/" + cachePath

	// Strip any scheme prefix from the remote ref. The queue carries the
	// full ${endpoint}/… form where endpoint usually carries a scheme
	// (https://...). docker pull/tag/push refuse scheme-qualified refs.
	remoteRef := strings.TrimPrefix(remote, "https://")
	remoteRef = strings.TrimPrefix(remoteRef, "http://")

	// In TLS mode the cache registry serves HTTPS with a mkcert cert the
	// host trusts — `docker manifest inspect` validates without --insecure.
	// In plain mode the registry is raw-IP HTTP, which the docker client
	// only reaches via --insecure (or insecure-registries).
	var inspectFlags []string
	if !net.tlsFor(c, c.Domain) {
		inspectFlags = []string{"--insecure"}
	}

	// Skip if already cached (unless --force).
	if !force {
		args := append([]string{"manifest", "inspect"}, inspectFlags...)
		args = append(args, cacheRefLocal)
		if c.docker(ctx, io.Discard, io.Discard, args...) == nil {
			ui.Debugf(c.ErrOut, "[ %s ] already in cache (%s)", svc, cacheRefLocal)
			return true
		}
	}

	fmt.Fprintf(c.Out, ":: [ %s ] caching %s -> %s\n", svc, remoteRef, cacheRefLocal)

	if c.docker(ctx, c.Out, c.ErrOut, "pull", remoteRef) != nil {
		ui.Errorf(c.ErrOut, "[ %s ] failed to pull %s (check upstream credentials)", svc, remoteRef)
		return false
	}
	if c.docker(ctx, c.Out, c.ErrOut, "tag", remoteRef, cacheRefLocal) != nil {
		ui.Errorf(c.ErrOut, "[ %s ] failed to tag %s as %s", svc, remoteRef, cacheRefLocal)
		return false
	}
	if c.docker(ctx, c.Out, c.ErrOut, "push", cacheRefLocal) != nil {
		ui.Errorf(c.ErrOut, "[ %s ] failed to push to cache registry at %s", svc, net.ip)
		return false
	}
	return true
}

func (c *Context) docker(ctx context.Context, stdout, stderr io.Writer, args ...string) error {
	return c.Runner.Run(ctx, execx.Cmd{Name: "docker", Args: args, Stdout: stdout, Stderr: stderr})
}

// exitCode maps a Runner error to the subprocess exit code (nil → 0, an
// *exec.ExitError or anything with ExitCode() → its code, else 1).
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var xe *exec.ExitError
	if errors.As(err, &xe) {
		return xe.ExitCode()
	}
	var ce interface{ ExitCode() int }
	if errors.As(err, &ce) {
		return ce.ExitCode()
	}
	return 1
}

// List shows what's currently in the cache registry (bash: image::list).
// The returned rc mirrors the bash function's exit status — notably curl's
// own code (7 on connection-refused) when the endpoint is unreachable, which
// pipefail propagated through `curl -s | jq .` and the raw re-fetch alike.
func (c *Context) List(ctx context.Context) (int, error) {
	ui.Debugf(c.ErrOut, "List images in the local cache registry")
	// Same gate as Cache: the cache registry only exists on Lo (kind)
	// clusters — fail with the driver named, not an IP-resolution error. An
	// explicit LOK8S_REGISTRY_IP_CACHE skips it: the operator has named the
	// registry, which is the documented escape for a shared cache, and
	// putting the gate first made that override unreachable (issue #89).
	if os.Getenv("LOK8S_REGISTRY_IP_CACHE") == "" {
		if err := domain.RequireDriver("lo", c.Paths.Clusters, c.Domain, "the image cache", c.ErrOut); err != nil {
			return 1, ErrHandled
		}
	}
	net := c.resolveCacheNet()
	if net.ip == "" {
		ui.Errorf(c.ErrOut, "cannot resolve the cache registry IP for domain '%s' — export LOK8S_REGISTRY_IP_CACHE=<ip> to override", c.Domain)
		return 1, ErrHandled
	}
	scheme := "http"
	if net.tlsFor(c, c.Domain) {
		scheme = "https"
	}
	catalogURL := scheme + "://" + net.ip + "/v2/_catalog"
	fmt.Fprintf(c.Out, ":: cache registry @ %s\n", catalogURL)

	// bash: `curl -s "$url" | jq . 2>/dev/null || curl -s "$url"` — run the
	// SAME pipeline through the Runner (only the same binaries guarantee
	// byte-identical pretty-printing AND curl's own exit codes). Under
	// pipefail a failed curl fails the pipeline even though jq succeeded on
	// the empty stream, so a dead endpoint re-fetches (nothing again) and
	// the function returns curl's code; a non-JSON body fails jq and the
	// re-fetch prints it raw (yes, a second request — observable via the
	// registry's access log, so preserved).
	var body strings.Builder
	curlErr := c.Runner.Run(ctx, execx.Cmd{Name: "curl", Args: []string{"-s", catalogURL},
		Stdout: &body, Stderr: c.ErrOut})
	jqErr := c.Runner.Run(ctx, execx.Cmd{Name: "jq", Args: []string{"."},
		Stdin: strings.NewReader(body.String()), Stdout: c.Out, Stderr: io.Discard})
	if curlErr != nil || jqErr != nil {
		refetchErr := c.Runner.Run(ctx, execx.Cmd{Name: "curl", Args: []string{"-s", catalogURL},
			Stdout: c.Out, Stderr: c.ErrOut})
		return exitCode(refetchErr), nil
	}
	return 0, nil
}

// Clean drops everything from the cache registry by recreating its volume
// (bash: image::clean). network is the resolved
// KIND_EXPERIMENTAL_DOCKER_NETWORK (spec.network.name > env > "lok8s" — the
// entrypoint's ambient export).
func (c *Context) Clean(ctx context.Context, network string) error {
	ui.Debugf(c.ErrOut, "Clean the local cache registry")
	if network == "" {
		network = "lok8s"
	}
	regName := network + "-registry-cache"
	fmt.Fprintf(c.Out, ":: dropping cache registry volume (%s)\n", regName)
	_ = c.Runner.Run(ctx, execx.Cmd{Name: "docker", Args: []string{"rm", "-f", regName},
		Stdout: c.Out, Stderr: io.Discard})
	_ = c.Runner.Run(ctx, execx.Cmd{Name: "docker", Args: []string{"volume", "rm", "-f", regName},
		Stdout: c.Out, Stderr: io.Discard})
	fmt.Fprintf(c.Out, ":: run 'lo registry up --cache' or 'lo provision' to recreate\n")
	return nil
}

func subst(s string) string {
	return string(env.BareEnvsubst([]byte(s)))
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
