package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/render"
)

// ctxRunner records the context state each Run saw.
type ctxRunner struct{ seen []error }

func (r *ctxRunner) Run(ctx context.Context, _ execx.Cmd) error {
	r.seen = append(r.seen, ctx.Err())
	return ctx.Err()
}

// The k8s artifact render runs under the command's context (a SIGINT
// reaches the kustomize child), not a fresh Background one.
func TestK8sArtifactRendersUnderTheCommandContext(t *testing.T) {
	t.Setenv(render.ModeEnv, string(render.ModeExec))
	p := synthProject(t)
	r := &ctxRunner{}
	prev := newRunner
	newRunner = func(*config.Paths) execx.Runner { return r }
	t.Cleanup(func() { newRunner = prev })

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := k8sKustomizeArtifact(ctx, p, "alpha.dev", filepath.Join(p.Clusters, "alpha.dev", "targets"), t.TempDir(), "infrastructure.yaml", io.Discard)
	if !errors.Is(err, ErrHandled) {
		t.Fatalf("err = %v, want ErrHandled (render failed)", err)
	}
	if len(r.seen) != 1 || !errors.Is(r.seen[0], context.Canceled) {
		t.Fatalf("the render child did not run under the command context: %v", r.seen)
	}
}

// envRunner records the env of each Run and answers an empty render.
type envRunner struct{ envs [][]string }

func (r *envRunner) Run(_ context.Context, c execx.Cmd) error {
	r.envs = append(r.envs, c.Env)
	return nil
}

// `lo k8s infrastructure/platform` renders with the domain's own store and
// has no silent flat fallback (review round 1 of lok8s#300): a domain
// without a store, in a project that keeps one per domain, stops before the
// render and writes no artifact.
func TestK8sArtifactUsesTheDomainStore(t *testing.T) {
	t.Setenv(render.ModeEnv, string(render.ModeExec))
	p := synthProject(t)
	r := &envRunner{}
	prev := newRunner
	newRunner = func(*config.Paths) execx.Runner { return r }
	t.Cleanup(func() { newRunner = prev })
	for _, d := range []string{"other.dev/secrets", "alpha.dev/targets"} {
		if err := os.MkdirAll(filepath.Join(p.Clusters, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	out := t.TempDir()
	var stderr bytes.Buffer
	err := k8sKustomizeArtifact(t.Context(), p, "alpha.dev", filepath.Join(p.Clusters, "alpha.dev", "targets"), out, "infrastructure.yaml", &stderr)
	if !errors.Is(err, ErrHandled) {
		t.Fatalf("err = %v, want ErrHandled", err)
	}
	if !strings.HasPrefix(stderr.String(), "[error] domain alpha.dev has no secrets store: "+p.Clusters+"/alpha.dev/secrets does not exist") {
		t.Errorf("stderr = %q", stderr.String())
	}
	if len(r.envs) != 0 {
		t.Error("the render ran")
	}
	if _, err := os.Stat(filepath.Join(out, "infrastructure.yaml")); err == nil {
		t.Error("an artifact was written")
	}

	store := filepath.Join(p.Clusters, "alpha.dev", "secrets")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := k8sKustomizeArtifact(t.Context(), p, "alpha.dev", filepath.Join(p.Clusters, "alpha.dev", "targets"), out, "infrastructure.yaml", io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(r.envs) != 1 || !slices.Contains(r.envs[0], "PATH_SECRETS="+store) {
		t.Errorf("render env = %v, want PATH_SECRETS=%s", r.envs, store)
	}
}
