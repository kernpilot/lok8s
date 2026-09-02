package lo

// registrycli.go — the library surface behind the future `lo registry
// {up,down,status,clean}` command port (.lok8s/drivers/lo/libs/registry).
// LIBRARY-ONLY: no cobra command registers these yet; the bash CLI stays
// authoritative until the flip. Domain resolution (flag > env > .active)
// remains with the command layer — these take the resolved domain.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/kernpilot/lok8s/internal/kapply"
	"github.com/kernpilot/lok8s/internal/ui"
)

// registryInit ensures the registry JSON is loaded for the domain (bash:
// _registry_init — idempotent, FATAL when the domain has no Lo cluster spec
// or the network config cannot be resolved: every caller feeds
// docker/network commands that would otherwise run against unset variables).
func (d *Driver) registryInit(domain string, errOut io.Writer) error {
	if path := getenv("LOK8S_REGISTRY_JSON"); path != "" && fileExists(path) {
		return nil
	}
	return readNetworkConfig(d.clusterYAML(domain), errOut)
}

// RegistryUp spins up the registries (bash: registry::up).
func (d *Driver) RegistryUp(ctx context.Context, domain string, out, errOut io.Writer) error {
	if err := d.registryInit(domain, errOut); err != nil {
		return err
	}
	cy := d.clusterYAML(domain)

	if err := d.network(ctx, errOut); err != nil {
		return err
	}
	if d.isShared() {
		if err := d.registryNetwork(ctx, errOut); err != nil {
			return err
		}
	}

	return kapply.Run("registries", out, errOut, func(o, e io.Writer) error {
		return d.registries(ctx, o, e, domain, cy)
	})
}

// RegistryDown removes the registry containers, volumes untouched (bash:
// registry::down).
func (d *Driver) RegistryDown(ctx context.Context, domain string, errOut io.Writer) error {
	if err := d.registryInit(domain, errOut); err != nil {
		return err
	}
	rf, err := regFile()
	if err != nil {
		return err
	}
	for _, r := range rf.Registries {
		regName, _ := rf.containerFor(r.Name)
		_ = d.runQuiet(ctx, "docker", "rm", "-f", regName)
	}
	return nil
}

// RegistryClean removes project registries + configs; with shared=true also
// the shared mirrors AND the shared network — detaching whatever remains
// attached first (bash: registry::clean). An explicitly-invoked clean must
// actually clear the network: it is the remediation the squatted-registry
// error recommends, which fires exactly when a holder is attached. Detach
// is NAMED (the operator sees whose containers were cut); a detached kind
// node re-attaches on its cluster's next `lo up`.
func (d *Driver) RegistryClean(ctx context.Context, domain string, shared bool, errOut io.Writer) error {
	if err := d.registryInit(domain, errOut); err != nil {
		return err
	}

	d.cleanupRegistries(ctx, envOr("LOK8S_SPEC_CLUSTER_NAME", "local"))

	if !shared {
		return nil
	}
	rf, err := regFile()
	if err != nil {
		return err
	}
	for _, r := range rf.Registries {
		if r.Type != "mirror" {
			continue
		}
		regName := SharedRegistryPrefix + r.Name
		_ = d.runQuiet(ctx, "docker", "rm", "-f", regName)
		_ = d.runQuiet(ctx, "docker", "volume", "rm", "-f", regName)
		removeStateFiles(regName)
	}

	net := rf.Network.Name
	members, _ := d.output(ctx, "docker", "network", "inspect", net,
		"-f", `{{range .Containers}}{{.Name}}{{"\n"}}{{end}}`)
	for _, member := range strings.Fields(members) {
		ui.Warnf(errOut, "registry clean: detaching '%s' from %s (re-attaches on its cluster's next 'lo up')", member, net)
		_ = d.runQuiet(ctx, "docker", "network", "disconnect", "-f", net, member)
	}
	if d.networkExists(ctx, net) {
		if err := d.runQuiet(ctx, "docker", "network", "rm", net); err != nil {
			ui.Warnf(errOut, "registry clean: could not remove %s — it still has attached containers (docker network inspect %s)", net, net)
			return fmt.Errorf("could not remove registry network %s", net)
		}
		ui.Debugf(errOut, "Removed registry network %s", net)
	}
	return nil
}

// RegistryStatus prints one aligned line per registry, kapply-style markers
// (bash: registry::status):
//
//	✓ build      [project] kubehz-registry-build → https://10.125.125.101 · 3 repos
//	⚠ cache      [project] kubehz-registry-cache → … · running, unreachable
//	✗ io-docker  [shared]  lok8s-registry-io-docker → … · not running
//
// Repo names print dimmed under the line in verbose mode (DEBUG set). tty
// gates the ANSI colors (bash: [[ -t 1 ]]).
func (d *Driver) RegistryStatus(ctx context.Context, domain string, shared, tty bool, out, errOut io.Writer) error {
	if err := d.registryInit(domain, errOut); err != nil {
		return err
	}
	rf, err := regFile()
	if err != nil {
		return err
	}

	cOK, cWarn, cErr, cDim, cOff := "", "", "", "", ""
	if tty {
		cOK, cWarn, cErr, cDim, cOff = "\033[32m", "\033[33m", "\033[31m", "\033[2m", "\033[0m"
	}

	for _, r := range rf.Registries {
		regName, regNetwork := rf.containerFor(r.Name)

		label := "[project]"
		if r.Type == "mirror" && rf.Shared {
			label = "[shared]"
			if !shared {
				continue
			}
		}

		// Look up container by network — avoids name collisions across
		// clusters.
		psNames, _ := d.output(ctx, "docker", "ps", "--filter", "network="+regNetwork, "--format", "{{.Names}}")
		onNetwork := false
		for _, n := range strings.Split(psNames, "\n") {
			if n == regName {
				onNetwork = true
				break
			}
		}

		endpoint := rf.url(r.IP)

		marker, state, repos := cErr+"✗"+cOff, "not running", ""
		if onNetwork {
			catalog, err := d.output(ctx, "curl", "-sf", "--connect-timeout", "1", "--max-time", "2", endpoint+"/v2/_catalog")
			if err == nil {
				marker = cOK + "✓" + cOff
				var doc struct {
					Repositories []string `json:"repositories"`
				}
				if json.Unmarshal([]byte(catalog), &doc) == nil {
					n := len(doc.Repositories)
					noun := "repos"
					if n == 1 {
						noun = "repo"
					}
					state = fmt.Sprintf("%d %s", n, noun)
					repos = strings.Join(doc.Repositories, "\n")
				} else {
					state = "up"
				}
			} else {
				marker = cWarn + "⚠" + cOff
				state = "running, unreachable"
			}
		}

		fmt.Fprintf(out, "%s %-10s %-9s %s → %s · %s\n", marker, r.Name, label, regName, endpoint, state)
		if getenv("DEBUG") != "" && repos != "" {
			for _, repo := range strings.Split(repos, "\n") {
				fmt.Fprintf(out, "      %s%s%s\n", cDim, repo, cOff)
			}
		}
	}
	return nil
}
