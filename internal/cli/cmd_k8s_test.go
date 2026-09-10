package cli

import (
	"context"
	"errors"
	"io"
	"path/filepath"
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

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := k8sKustomizeArtifact(ctx, p, "alpha.dev", filepath.Join(p.Clusters, "alpha.dev", "targets"), t.TempDir(), "infrastructure.yaml", io.Discard)
	if !errors.Is(err, ErrHandled) {
		t.Fatalf("err = %v, want ErrHandled (render failed)", err)
	}
	if len(r.seen) != 1 || !errors.Is(r.seen[0], context.Canceled) {
		t.Fatalf("the render child did not run under the command context: %v", r.seen)
	}
}
