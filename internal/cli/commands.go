package cli

// Annotation keys carried on every command. They mirror the argsh usage
// markers (`@destructive`, `@readonly`, `@idempotent`) and drive confirmation
// gating and MCP tool filtering.
const (
	AnnotationDestructive = "lok8s.dev/destructive"
	AnnotationReadonly    = "lok8s.dev/readonly"
	AnnotationIdempotent  = "lok8s.dev/idempotent"
)

// Command groups shown in help output.
const (
	groupLifecycle    = "lifecycle"
	groupConfigure    = "configure"
	groupIntegrations = "integrations"
	groupComponents   = "components"
)

type commandSpec struct {
	use     string
	aliases []string
	short   string
	group   string
	hidden  bool

	destructive bool
	readonly    bool
	idempotent  bool
}

// commandTree is the full `lo` surface, verbatim from the argsh usage list in
// `.lok8s/lo`. Every command starts life as a shim to the bash implementation;
// ported commands override their entry in NewRoot.
var commandTree = []commandSpec{
	{use: "up", aliases: []string{"u"}, short: "Start cluster", group: groupLifecycle, destructive: true},
	{use: "down", short: "Stop cluster", group: groupLifecycle, destructive: true},
	{use: "clean", short: "Clean up local volumes", group: groupLifecycle, destructive: true},

	{use: "provision", aliases: []string{"p"}, short: "Provision a cluster (full lifecycle)", group: groupLifecycle, destructive: true},
	{use: "bootstrap", short: "Apply/reapply bootstrap addons", group: groupLifecycle, destructive: true, idempotent: true},
	{use: "build", aliases: []string{"b"}, short: "Build kustomize targets", group: groupLifecycle, idempotent: true},
	{use: "deploy", aliases: []string{"d"}, short: "Deploy platform", group: groupLifecycle, destructive: true, idempotent: true},
	{use: "destroy", short: "Destroy a cluster", group: groupLifecycle, destructive: true},
	{use: "recover", short: "Rebuild a cluster from bare metal (disaster recovery)", group: groupLifecycle, destructive: true},

	{use: "use", short: "Set/show active domain", group: groupConfigure, idempotent: true},
	{use: "kubeconfig", aliases: []string{"kc"}, short: "Print a domain kubeconfig (--oidc for kubelogin exec-plugin)", group: groupConfigure, readonly: true},
	{use: "init", short: "Scaffold service config (lok8s.yaml/services.yaml/Tiltfile)", group: groupConfigure, idempotent: true},
	{use: "lint", aliases: []string{"l"}, short: "Validate structure and specs", group: groupConfigure, readonly: true},
	{use: "audit", aliases: []string{"au"}, short: "Static security-posture audit (read-only, cluster-free; --json | --sarif)", group: groupConfigure, readonly: true},
	{use: "status", short: "Cluster health and status", group: groupConfigure, readonly: true},
	{use: "doctor", short: "Diagnose the environment + toolchain", group: groupConfigure, readonly: true},
	{use: "trust", short: "Trust the local dev CA (mkcert -install)", group: groupConfigure, idempotent: true},
	{use: "version", short: "Print lok8s + toolchain versions", group: groupConfigure, readonly: true},

	{use: "chat", short: "Chat with a local AI (transparent, streaming)", group: groupIntegrations},
	{use: "ai", short: "Manage the AI integration (lo chat + agent skills)", group: groupIntegrations},
	{use: "gitops", aliases: []string{"g"}, short: "GitOps integration", group: groupIntegrations},
	{use: "kubehz", aliases: []string{"kh"}, short: "kubehz platform integration", group: groupIntegrations},
	{use: "tilt", aliases: []string{"t"}, short: "Manage tilt cluster", group: groupIntegrations},

	{use: "hooks", short: "Dev hook actions (internal; driven by the Tilt hooks: wrapper)", hidden: true},
	{use: "env", short: "Service/env config rendering (internal; driven by the Tilt extension)", hidden: true},
	{use: "k8s", short: "K8s artifact generation (internal; legacy capigen-era paths)", hidden: true},
	{use: "crds", short: "Operator CRD generation from schema source (internal; CI drift gate)", hidden: true},

	{use: "kustomize", aliases: []string{"ku"}, short: "Manage kustomize plugins (Go)", group: groupComponents},
	{use: "registry", aliases: []string{"r"}, short: "Manage Docker registries", group: groupComponents},
	{use: "image", aliases: []string{"i"}, short: "Manage the local cache registry", group: groupComponents},
	{use: "addons", aliases: []string{"a"}, short: "List driver addons for the active cluster", group: groupComponents, readonly: true},
	{use: "secrets", aliases: []string{"s"}, short: "Manage secrets (encrypt/decrypt/set)", group: groupComponents},
	{use: "drivers", short: "Driver-specific commands", group: groupComponents},
}

func (s commandSpec) annotations() map[string]string {
	a := map[string]string{}
	if s.destructive {
		a[AnnotationDestructive] = "true"
	}
	if s.readonly {
		a[AnnotationReadonly] = "true"
	}
	if s.idempotent {
		a[AnnotationIdempotent] = "true"
	}
	return a
}
