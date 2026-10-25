package provision

// destroy_guard.go: the cluster_name guard of `lo destroy` (bash:
// provision::_destroy_guard). A provider with the destroy_plan hook deletes
// by a name, not by a list: the hetzner provider deletes EVERY resource in
// the token's project that carries lok8s.dev/cluster=<cluster_name>. A
// domain copied from another one that kept the cluster_name and the token
// would delete the other cluster too. So before anything is deleted:
//
//  1. another domain under clusters/ with the same cluster_name: refuse;
//  2. print what the provider would delete;
//  3. the operator types the cluster_name, or passes it with
//     --confirm-cluster off a terminal. --force does not skip this step.
//
// The confirmed plan goes to the driver's provider::destroy in
// LOK8S_DESTROY_PLAN: the provider deletes exactly those resources, not a
// second list taken after `kubeone reset`. The guard replaces the
// yes-prompt of ConfirmInfra for such a provider: one question per run.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kernpilot/lok8s/internal/domain"
	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/ui"
	"github.com/kernpilot/lok8s/internal/yqsem"
)

// DestroyPlanEnv carries the confirmed destroy plan to provider::destroy.
const DestroyPlanEnv = "LOK8S_DESTROY_PLAN"

// destroyResource is one line of a destroy plan.
type destroyResource struct{ kind, id, name string }

// parseDestroyPlan reads the provider::destroy_plan text: the
// "cluster\t<name>" line and one "<resource>\t<id>\t<name>" line per
// resource (bash: `IFS=$'\t' read -r kind id name`).
func parseDestroyPlan(text string) (string, []destroyResource) {
	var name string
	var res []destroyResource
	for line := range strings.SplitSeq(text, "\n") {
		f := strings.FieldsFunc(line, func(r rune) bool { return r == '\t' })
		if len(f) == 0 {
			continue
		}
		if f[0] == "cluster" && name == "" && len(res) == 0 {
			if len(f) > 1 {
				name = strings.Join(f[1:], "\t")
			}
			continue
		}
		r := destroyResource{kind: f[0]}
		if len(f) > 1 {
			r.id = f[1]
		}
		if len(f) > 2 {
			r.name = strings.Join(f[2:], "\t")
		}
		res = append(res, r)
	}
	return name, res
}

// providerWorkDir is the work dir the driver hands provider::destroy: the
// lo driver uses clusters/<domain>/.provider, the kubeone driver
// clusters/<domain>/.kubeone.
func providerWorkDir(clusters, domainName, kind string) string {
	if kind == "kubeone" {
		return filepath.Join(clusters, domainName, ".kubeone")
	}
	return filepath.Join(clusters, domainName, ".provider")
}

// destroyNoProviderReason is "" when the driver of this kind destroys through
// provider::destroy (bash: driver::destroy of drivers/lo and drivers/kubeone),
// else why it does not. A hosted KubeOne cluster goes through the kubehz API.
func destroyNoProviderReason(kind, clusterYAML string) string {
	switch kind {
	case "lo":
		return ""
	case "kubeone":
		if yqsem.Or(yqsem.Lookup(yqsem.LoadNode(clusterYAML), "spec", "kubehz", "hosting"), "self") == "hosted" {
			return "a hosted cluster is destroyed through the kubehz API, not the provider"
		}
		return ""
	}
	return "the " + kind + " driver does not destroy through the provider"
}

// guardDestroy runs the cluster_name guard when the loaded provider deletes
// by cluster_name. guarded=false: no guard ran, and skip says why; the
// caller asks ConfirmInfra instead. A refusal or a decline is
// driver.ErrDeclined (rc 3): nothing ran.
func (d *Dispatcher) guardDestroy(ctx context.Context, domainName, kind, clusterYAML string, deps *driver.Deps) (guarded bool, skip string, err error) {
	if deps.Provider == nil {
		if !d.Remote {
			return false, "it runs without -r (--remote), so no provider is loaded", nil
		}
		return false, "the spec names no provider", nil
	}
	if reason := destroyNoProviderReason(kind, clusterYAML); reason != "" {
		return false, reason, nil
	}
	planner, ok := deps.Provider.(driver.ProviderDestroyPlanner)
	if !ok {
		return false, "provider '" + deps.ProviderName + "' has no destroy plan", nil
	}
	w := d.errWriter()
	text, has, err := planner.DestroyPlan(ctx, deps.ProviderConfigFile, providerWorkDir(d.Paths.Clusters, domainName, kind))
	if err != nil {
		// The provider printed its own error.
		return true, "", ui.Handled(fmt.Errorf("destroy plan of %s: %w", domainName, err))
	}
	if !has {
		return false, "provider '" + deps.ProviderName + "' has no destroy plan", nil
	}
	name, res := parseDestroyPlan(text)
	if name == "" {
		ui.ErrorTo(w, "the provider gave no cluster_name for '%s'. Nothing was deleted. Set cluster_name in the provider config, then run lo destroy again.", domainName)
		return true, "", driver.ErrDeclined
	}

	if users := domain.ProviderClusterNameUsers(d.Paths.Clusters, domainName, name); len(users) > 0 {
		ui.ErrorTo(w, "cluster_name '%s' of '%s' is also the cluster_name of: %s. The provider deletes every resource of a cluster_name, so this destroy would delete the resources of those domains too. Nothing was deleted. Give each domain its own cluster_name, then run lo destroy again.", name, domainName, strings.Join(users, ", "))
		return true, "", driver.ErrDeclined
	}

	fmt.Fprintf(w, "\n  \033[1;33m⚠ %s\033[0m \033[2m·\033[0m destroy deletes these resources of cluster_name \033[1m%s\033[0m:\n", domainName, name)
	if len(res) == 0 {
		fmt.Fprintf(w, "    (no resource found)\n")
	}
	for _, r := range res {
		fmt.Fprintf(w, "    %s %s (%s)\n", r.kind, r.name, r.id)
	}

	switch {
	case d.ConfirmCluster != "":
		if d.ConfirmCluster != name {
			ui.ErrorTo(w, "--confirm-cluster '%s' does not match the cluster_name '%s' of '%s'. Nothing was deleted.", d.ConfirmCluster, name, domainName)
			return true, "", driver.ErrDeclined
		}
	case !d.interactive():
		ui.ErrorTo(w, "there is no terminal to ask for the cluster_name of '%s'. Nothing was deleted. To confirm, run: lo destroy --domain %s -r --confirm-cluster %s", domainName, domainName, name)
		return true, "", driver.ErrDeclined
	default:
		fmt.Fprintf(w, "\n  type the cluster_name (%s) to delete these resources: ", name)
		ans, err := d.readAnswer()
		if err != nil || ans != name {
			ui.ErrorTo(w, "the answer is not the cluster_name '%s'. Nothing was deleted.", name)
			return true, "", driver.ErrDeclined
		}
	}
	// The driver's provider::destroy deletes exactly the confirmed plan
	// (bash: the plan from a command substitution, no trailing newline).
	os.Setenv(DestroyPlanEnv, strings.TrimRight(text, "\n"))
	return true, "", nil
}
