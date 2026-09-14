package cli

// examples.go — the `Examples:` block of every visible command, in one
// shape: one to three lines, each `  lo <command> <real-looking values>`,
// an aligned `# note` only where a line needs one. The block is help text
// only (`lo <cmd> --help`, the generated reference): it never reaches a
// command's own output, so the piped contract of a ported command is
// untouched. The examples are keyed by command path and applied after the
// tree is assembled (applyExamples), so the files that own the commands,
// and the shims of a command routed to bash, need no edit; a command that
// sets its own Example keeps it. TestEveryVisibleCommandHasExample keeps
// the map complete.

import (
	"strings"

	"github.com/spf13/cobra"
)

// commandExamples maps a command path below the root to its example block.
var commandExamples = map[string]string{
	// Cluster lifecycle.
	"up": `  lo up
  lo up --open-tilt
  lo up --ci --timeout 300s`,
	"down": `  lo down
  lo down --domain kubehz.dev`,
	"clean": `  lo clean
  lo clean --all`,
	"provision": `  lo provision
  lo provision --domain kubehz.in.net --force
  lo provision --bootstrap`,
	"bootstrap": `  lo bootstrap
  lo bootstrap --domain kubehz.dev`,
	"build": `  lo build
  lo build --domain kubehz.cloud`,
	"deploy": `  lo deploy
  lo deploy -l lok8s.dev/name=zitadel
  lo deploy --domain kubehz.cloud --cluster-override kubehz.in.net`,
	"destroy": `  lo destroy
  lo destroy --domain kubehz.in.net`,
	"recover": `  lo recover kubehz.in.net --dry-run
  lo recover kubehz.in.net
  lo recover kubehz.in.net --skip-rebuild`,

	// Configure & inspect.
	"use": `  lo use
  lo use kubehz.dev`,
	"kubeconfig": `  lo kubeconfig > .kubeconfig/kubehz-dev.yaml
  lo kubeconfig --oidc --domain kubehz.cloud
  lo kc --cluster-override kubehz.in.net`,
	"init": `  lo init
  lo init --plan
  lo init service api`,
	"init service": `  lo init service api
  lo init service worker --path services/worker`,
	"init test": `  lo init test
  lo init test --path e2e --force`,
	"init project": `  lo init project
  lo init project acme --env direnv
  lo init project --cluster demo.dev --driver lo`,
	"lint": `  lo lint
  lo lint --domain kubehz.dev --notes`,
	"audit": `  lo audit
  lo audit kubehz.cloud --json
  lo audit --sarif > audit.sarif`,
	"status": `  lo status
  lo status --domain kubehz.in.net`,
	"doctor": `  lo doctor
  lo doctor --toolchain`,
	"trust": `  lo trust`,
	"version": `  lo version
  lo --version`,

	// Integrations.
	"chat": `  lo chat`,
	"ai": `  lo ai check
  lo ai skills`,
	"ai check":  `  lo ai check`,
	"ai skills": `  lo ai skills`,
	"ai link": `  lo ai link claude
  lo ai link cursor --copy`,
	"ai unlink": `  lo ai unlink claude`,
	"gitops": `  lo gitops flux
  lo gitops argo --domain kubehz.cloud`,
	"gitops flux": `  lo gitops flux
  lo gitops flux --domain kubehz.cloud`,
	"gitops argo": `  lo gitops argo
  lo gitops argo --domain kubehz.cloud`,
	"kubehz": `  lo kubehz register
  lo kubehz status
  lo kubehz deploy --dry-run`,
	"kubehz register":   `  lo kubehz register`,
	"kubehz deregister": `  lo kubehz deregister`,
	"kubehz status":     `  lo kubehz status`,
	"kubehz re-enroll":  `  lo kubehz re-enroll`,
	"kubehz assess":     `  lo kubehz assess`,
	"kubehz deploy": `  lo kubehz deploy
  lo kubehz deploy --dry-run`,
	"kubehz join": `  lo kubehz join worker-1
  lo kubehz join worker-1 --print-token`,
	"kubehz claim-code": `  lo kubehz claim-code`,
	"kubehz claim": `  lo kubehz claim --nonce khzn_2f9c1e
  lo kubehz claim --nonce - < nonce.txt`,
	"kubehz node": `  lo kubehz node join
  lo kubehz node status`,
	"kubehz node join": `  sudo lo kubehz node join
  lo kubehz node join --print-only`,
	"kubehz node remove": `  lo kubehz node remove --name worker-1`,
	"kubehz node status": `  lo kubehz node status`,
	"kubehz handover": `  lo kubehz handover receive --bundle handover.tar.gz
  lo kubehz handover preseed --bundle handover.tar.gz --node 10.0.0.12`,
	"kubehz handover receive": `  lo kubehz handover receive --bundle handover.tar.gz
  lo kubehz handover receive --bundle handover.tar.gz --single-node`,
	"kubehz handover preseed": `  lo kubehz handover preseed --bundle handover.tar.gz --node 10.0.0.12`,
	"tilt": `  lo tilt up
  lo tilt ci --timeout 300s
  lo tilt down`,
	"tilt up": `  lo tilt up`,
	"tilt ci": `  lo tilt ci
  lo tilt ci --timeout 10m`,
	"tilt down":    `  lo tilt down`,
	"tilt status":  `  lo tilt status`,
	"tilt restart": `  lo tilt restart`,
	"tilt preflight": `  lo build && lo tilt preflight < clusters/kubehz.dev/artifacts.yaml
  lo tilt preflight --crds skip < artifacts.yaml`,

	// Components.
	"kustomize": `  lo kustomize build
  lo kustomize list`,
	"kustomize build": `  lo kustomize build`,
	"kustomize test":  `  lo kustomize test`,
	"kustomize clean": `  lo kustomize clean`,
	"kustomize list":  `  lo kustomize list`,
	"registry": `  lo registry up
  lo registry status --shared`,
	"registry up":   `  lo registry up`,
	"registry down": `  lo registry down`,
	"registry status": `  lo registry status
  lo registry status --shared`,
	"registry clean": `  lo registry clean
  lo registry clean --shared`,
	"image": `  lo image cache api
  lo image list`,
	"image cache": `  lo image cache api
  lo image cache --all --force`,
	"image list":  `  lo image list`,
	"image clean": `  lo image clean`,
	"addons": `  lo addons
  lo addons cilium
  lo addons --detail --origin`,
	"secrets": `  lo secrets init
  lo secrets set --name db --namespace app password
  lo secrets encrypt`,
	"secrets init": `  lo secrets init`,
	"secrets add-key": `  lo secrets add-key ~/.ssh/id_ed25519.pub
  lo secrets add-key age1qyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqs3290gq --all`,
	"secrets encrypt": `  lo secrets encrypt`,
	"secrets decrypt": `  lo secrets decrypt`,
	"secrets allow":   `  lo secrets allow`,
	"secrets list":    `  lo secrets list`,
	"secrets print": `  lo secrets print
  lo secrets print 'Secret.db.*'`,
	"secrets path": `  lo secrets path`,
	"secrets set": `  lo secrets set --name db --namespace app password
  lo secrets set --name db password - < password.txt
  lo secrets set --name db password s3cret --encrypt`,
	"secrets env": `  eval "$(lo secrets env --name db --namespace app)"`,
	"drivers": `  lo drivers --list
  lo drivers lo status kubehz.dev`,

	// Go-only.
	"mcp": `  lo mcp start
  lo mcp tools --allow-mutating
  lo mcp claude enable`,
	"mcp start": `  lo mcp start
  lo mcp start --allow-destructive`,
	"mcp serve": `  lo mcp serve
  lo mcp serve --port 8090 --allow-mutating`,
	"mcp tools": `  lo mcp tools
  lo mcp tools --allow-destructive`,
	"assets": `  lo assets list
  lo assets diff --check`,
	"assets list": `  lo assets list
  lo assets list --json`,
	"assets eject": `  lo assets eject
  lo assets eject addons/cilium
  lo assets eject bash`,
	"assets diff": `  lo assets diff
  lo assets diff addons/cilium
  lo assets diff --check`,
	"assets update": `  lo assets update addons/cilium
  lo assets update addons/cilium --force`,
	"toolchain": `  lo toolchain install
  lo toolchain doctor`,
	"toolchain install": `  lo toolchain install
  lo toolchain install --groups core,local,cloud
  lo toolchain install --dry-run`,
	"toolchain doctor": `  lo toolchain doctor`,
}

// driverOpExamples are the examples of `lo drivers <name> <op>`, one
// template per contract op; %s is the driver name.
var driverOpExamples = map[string]string{
	"":           "  lo drivers %s status kubehz.dev\n  lo drivers %s provision kubehz.dev",
	"provision":  "  lo drivers %s provision kubehz.dev",
	"destroy":    "  lo drivers %s destroy kubehz.dev",
	"status":     "  lo drivers %s status kubehz.dev",
	"kubeconfig": "  lo drivers %s kubeconfig kubehz.dev",
}

// mcpEditorExamples are the examples of `lo mcp <editor> <op>` (the
// editor subcommands ophis adds); %s is the editor name.
var mcpEditorExamples = map[string]string{
	"":        "  lo mcp %s enable\n  lo mcp %s list",
	"enable":  "  lo mcp %s enable\n  lo mcp %s enable --env LO_MCP_ALLOW=destructive",
	"disable": "  lo mcp %s disable",
	"list":    "  lo mcp %s list",
}

// applyExamples sets Example on every visible command that has one in the
// map and none of its own. Keyed by the path below the root; the `lo
// drivers <name>` and `lo mcp <editor>` subtrees use templates.
func applyExamples(root *cobra.Command) {
	walkVisibleCommands(root, func(cmd *cobra.Command) {
		if cmd.Example != "" {
			return
		}
		path := commandPathBelowRoot(cmd)
		if ex, ok := commandExamples[path]; ok {
			cmd.Example = ex
			return
		}
		if ex, ok := driverExample(path); ok {
			cmd.Example = ex
			return
		}
		if ex, ok := templateExample(path, "mcp", mcpEditorExamples); ok {
			cmd.Example = ex
		}
	})
}

// walkVisibleCommands visits every command below root that `--help`
// shows: not hidden, and under no hidden ancestor.
func walkVisibleCommands(root *cobra.Command, visit func(*cobra.Command)) {
	for _, c := range root.Commands() {
		if c.Hidden || !c.IsAvailableCommand() {
			continue
		}
		visit(c)
		walkVisibleCommands(c, visit)
	}
}

// templateExample answers `<group> <name>` and `<group> <name> <op>` from
// a template map keyed by op ("" for the name itself); %s is the name.
func templateExample(path, group string, templates map[string]string) (string, bool) {
	parts := strings.Fields(path)
	if len(parts) < 2 || len(parts) > 3 || parts[0] != group {
		return "", false
	}
	op := ""
	if len(parts) == 3 {
		op = parts[2]
	}
	tpl, ok := templates[op]
	if !ok {
		return "", false
	}
	return strings.ReplaceAll(tpl, "%s", parts[1]), true
}

// commandPathBelowRoot is cmd.CommandPath() without the root name.
func commandPathBelowRoot(cmd *cobra.Command) string {
	path := cmd.CommandPath()
	rootName := cmd.Root().Name()
	return strings.TrimPrefix(strings.TrimPrefix(path, rootName), " ")
}

// driverExample answers `drivers <name>` and `drivers <name> <op>`.
func driverExample(path string) (string, bool) {
	return templateExample(path, "drivers", driverOpExamples)
}
