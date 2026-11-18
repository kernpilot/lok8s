package provision

// destroy_guard.go: the cluster_name guard of `lo destroy` (bash:
// provision::_destroy_guard). A provider with the destroy_plan hook finds
// its resources by the cluster_name: the hetzner provider deletes the
// resources in the token's project that carry
// lok8s.dev/cluster=<cluster_name> AND the owner mark of this domain. A
// domain copied from another one that kept the cluster_name and the token
// labels its resources the same way, and only the mark keeps the two apart:
// a resource of the other domain with a wrong mark would be deleted. So
// before anything is deleted:
//
//  1. a plan with a NUL byte, or a cluster_name that a message cannot show
//     as it is (a character of ui.Unshown, a backslash, U+FFFD or a byte
//     that is not UTF-8; the operator types the name): refuse;
//  2. another domain under clusters/ with the same cluster_name: refuse;
//  3. print what the provider would delete;
//  4. the operator types the cluster_name, or passes it with
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
	"unicode/utf8"

	"github.com/kernpilot/lok8s/internal/domain"
	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/ui"
	"github.com/kernpilot/lok8s/internal/yqsem"
)

// destroyResource is one line of a destroy plan.
type destroyResource struct{ kind, id, name string }

// setenv is os.Setenv. A test replaces it with one that fails.
var setenv = os.Setenv

// parseDestroyPlan reads the provider::destroy_plan text: the
// "cluster\t<name>" line and one "<resource>\t<id>\t<name>" line per
// resource (bash: `IFS=$'\t' read -r kind id name`, readPlanLine).
func parseDestroyPlan(text string) (string, []destroyResource) {
	var name string
	var res []destroyResource
	for line := range strings.SplitSeq(text, "\n") {
		if strings.Trim(line, "\t") == "" {
			continue
		}
		kind, id, rest := readPlanLine(line)
		if kind == "cluster" && name == "" && len(res) == 0 {
			name = id
			if rest != "" {
				name += "\t" + rest
			}
			continue
		}
		res = append(res, destroyResource{kind: kind, id: id, name: rest})
	}
	return name, res
}

// readPlanLine splits a plan line as bash's `IFS=$'\t' read -r kind id
// rest` does. A tab is IFS white space: the tabs at the start and at the end
// go, and a run of tabs after the kind and after the id is one separator.
// rest keeps the tabs in it, so a name with two tabs in a row shows two '?',
// as in bash.
func readPlanLine(line string) (kind, id, rest string) {
	rest = strings.Trim(line, "\t")
	kind, rest, _ = strings.Cut(rest, "\t")
	id, rest, _ = strings.Cut(strings.TrimLeft(rest, "\t"), "\t")
	return kind, id, strings.TrimLeft(rest, "\t")
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
	// A NUL byte: no environment variable can hold it, so the confirmed
	// plan could not go to the provider's destroy, and bash's command
	// substitution drops it (the plan would change). Refuse the plan
	// before anything is shown (bash twin: the NUL check of
	// provision::_destroy_guard).
	if strings.IndexByte(text, 0) >= 0 {
		ui.ErrorTo(w, "the destroy plan of '%s' has a NUL byte, and the confirmed plan cannot carry it to the provider. Nothing was deleted. Correct the destroy_plan hook of the provider, then run lo destroy again.", domainName)
		return true, "", driver.ErrDeclined
	}
	name, res := parseDestroyPlan(text)
	if name == "" {
		ui.ErrorTo(w, "the provider gave no cluster_name for '%s'. Nothing was deleted. Set cluster_name in the provider config, then run lo destroy again.", domainName)
		return true, "", driver.ErrDeclined
	}
	// The messages below show the cluster_name, and the operator types it
	// as the answer, so a shown form must be the name itself: an escaped
	// form is not what the operator must type. Refuse a name that a message
	// cannot show as it is, before any message shows it: a character of
	// ui.Unshown (a control, separator, bidi or invisible character), a
	// backslash (bash's error() runs echo -e), a byte that is not UTF-8,
	// and U+FFFD. jq turns each sequence that is not UTF-8 into U+FFFD, so
	// the bash twin (provision::_destroy_guard) refuses U+FFFD for both and
	// gives the same verdict. Each rule is a clause of its own.
	if !utf8.ValidString(name) ||
		strings.Contains(name, "\uFFFD") ||
		strings.ContainsRune(name, '\\') ||
		strings.ContainsFunc(name, ui.Unshown) {
		ui.ErrorTo(w, "the destroy plan of '%s' names a cluster_name with a character that a message cannot show as it is (a control, separator, bidi or invisible character, a backslash, U+FFFD, or a byte that is not UTF-8). Nothing was deleted. Correct the cluster_name in the provider config or the destroy_plan hook of the provider, then run lo destroy again.", domainName)
		return true, "", driver.ErrDeclined
	}

	if users := domain.ProviderClusterNameUsers(d.Paths.Clusters, domainName, name); len(users) > 0 {
		ui.ErrorTo(w, "cluster_name '%s' of '%s' is also the cluster_name of: %s. The provider deletes only the resources with this cluster_name and the owner mark of '%s'. The resources of those domains have the same cluster label, so only the mark keeps them apart: a resource of another domain with a wrong mark would be deleted. Nothing was deleted. Give each domain its own cluster_name, then run lo destroy again.", name, domainName, strings.Join(users, ", "), domainName)
		return true, "", driver.ErrDeclined
	}

	fmt.Fprintf(w, "\n  \033[1;33m⚠ %s\033[0m \033[2m·\033[0m destroy deletes these resources of cluster_name \033[1m%s\033[0m:\n", domainName, name)
	if len(res) == 0 {
		fmt.Fprintf(w, "    (no resource found)\n")
	}
	// Each kind, name and id as ui.Shown shows it: they come from the
	// provider (a third-party provider too) and its API, and a control,
	// separator, bidi or invisible character in them would reach the
	// terminal, split or turn the line around, or hide in the line. The
	// confirmed plan below stays as the provider gave it.
	for _, r := range res {
		fmt.Fprintf(w, "    %s %s (%s)\n", ui.Shown(r.kind), ui.Shown(r.name), ui.Shown(r.id))
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
	// Without it, a provider deletes what its own list gives: stop.
	if err := setenv(driver.DestroyPlanEnv, strings.TrimRight(text, "\n")); err != nil {
		ui.ErrorTo(w, "could not give the confirmed plan of '%s' to the provider (%v). Nothing was deleted. Run lo destroy again.", domainName, err)
		return true, "", driver.ErrDeclined
	}
	return true, "", nil
}
