package kubehz

// hooks.go — the constructors that satisfy the dispatch/driver seams:
// provision.Hooks.{KubehzRegister,KubehzDeregister} and the kubeone/capi
// drivers' Hooks.{ReadKubehzConfig,ProvisionHosted,DestroyHosted}. The
// hook bodies are the bash tails verbatim:
//
//	register   read_config → validate_config → (access != none) → register_cluster
//	deregister read_config → (access != none) → deregister_cluster
//	hosted     read_config (guarded) → LOK8S_KUBEHZ_HOSTING; provision/destroy_hosted

import (
	"context"

	"github.com/kernpilot/lok8s/internal/driver/capi"
	"github.com/kernpilot/lok8s/internal/driver/kubeone"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/provision"
)

// RegisterHook is provision.Hooks.KubehzRegister. Any error aborts the
// dispatch (each bash step is `|| return 1`); register_cluster itself is
// non-fatal by contract.
func (c *Context) RegisterHook() func(ctx context.Context, domain, clusterYAML string) error {
	return func(ctx context.Context, domain, clusterYAML string) error {
		cfg, err := c.ReadConfig(clusterYAML)
		if err != nil {
			return err
		}
		if err := c.Validate(cfg, clusterYAML); err != nil {
			return err
		}
		if cfg.Access != "none" {
			return c.RegisterCluster(ctx, cfg, domain, clusterYAML)
		}
		return nil
	}
}

// DeregisterHook is provision.Hooks.KubehzDeregister — best-effort at the
// dispatch (it warns and continues on error).
func (c *Context) DeregisterHook() func(ctx context.Context, domain, clusterYAML string) error {
	return func(ctx context.Context, domain, clusterYAML string) error {
		cfg, err := c.ReadConfig(clusterYAML)
		if err != nil {
			// bash: an unguarded read_config leaves ACCESS unset, and the
			// `!= "none"` test then runs deregister on empty config; the
			// api URL is empty so the https guard refuses. Surface the read
			// error instead (the warn happens at the dispatch).
			return err
		}
		if cfg.Access != "none" {
			return c.DeregisterCluster(ctx, cfg, domain, clusterYAML)
		}
		return nil
	}
}

// ReadConfigHook is the drivers' Hooks.ReadKubehzConfig: kubehz::read_config
// → LOK8S_KUBEHZ_HOSTING, guarded.
func (c *Context) ReadConfigHook() func(clusterYAML string) (string, error) {
	return func(clusterYAML string) (string, error) {
		cfg, err := c.ReadConfig(clusterYAML)
		if err != nil {
			return "", err
		}
		return cfg.Hosting, nil
	}
}

// ProvisionHostedHook is the drivers' Hooks.ProvisionHosted.
func (c *Context) ProvisionHostedHook() func(ctx context.Context, domain, clusterYAML string) error {
	return func(ctx context.Context, domain, clusterYAML string) error {
		cfg, err := c.ReadConfig(clusterYAML)
		if err != nil {
			return err
		}
		return c.ProvisionHosted(ctx, cfg, domain, clusterYAML)
	}
}

// DestroyHostedHook is the drivers' Hooks.DestroyHosted.
func (c *Context) DestroyHostedHook() func(ctx context.Context, domain, clusterYAML string) error {
	return func(ctx context.Context, domain, clusterYAML string) error {
		cfg, err := c.ReadConfig(clusterYAML)
		if err != nil {
			return err
		}
		return c.DestroyHosted(ctx, cfg, domain, clusterYAML)
	}
}

// ProvisionHooks returns the dispatch-tail hooks this package provides
// (the bootstrap/inventory/gitops seams stay nil — other ports own them).
func (c *Context) ProvisionHooks() provision.Hooks {
	return provision.Hooks{
		KubehzRegister:   c.RegisterHook(),
		KubehzDeregister: c.DeregisterHook(),
	}
}

// KubeoneHooks returns the kubeone driver's kubehz seams (the inventory /
// pre-apply seams stay nil — the hetzner provider port owns them).
func (c *Context) KubeoneHooks() kubeone.Hooks {
	return kubeone.Hooks{
		ReadKubehzConfig: c.ReadConfigHook(),
		ProvisionHosted:  c.ProvisionHostedHook(),
		DestroyHosted:    c.DestroyHostedHook(),
	}
}

// CapiHooks returns the capi driver's kubehz seams.
func (c *Context) CapiHooks() capi.Hooks {
	return capi.Hooks{
		ReadKubehzConfig: c.ReadConfigHook(),
		ProvisionHosted:  c.ProvisionHostedHook(),
		DestroyHosted:    c.DestroyHostedHook(),
	}
}

// execxCmdEnv builds a Cmd with extra env entries and the context's
// streams (bash: `ETCDCTL_API=3 etcdctl …`).
func execxCmdEnv(name string, args, env []string, c *Context) execx.Cmd {
	return execx.Cmd{Name: name, Args: args, Env: env, Stdout: c.out(), Stderr: c.errOut()}
}
