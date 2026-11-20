package cli

// cmd_doctor_rbac.go is the RBAC migration check of `lo doctor` (issues
// #311 and #315). Bash twin: doctor::_rbac_check in .lok8s/libs/doctor.
// The piped lines are the same in both.
//
// The system-upgrade-controller and redis-operator addons no longer bind
// the broad RBAC of their charts (PRs #309 and #314). The bootstrap applies
// without prune, so a cluster that ran an earlier version of an addon keeps
// the old objects until a person deletes them. This check reads the
// ClusterRoleBindings and ClusterRoles of the active domain's cluster and
// reports each old object with its delete command. It never changes the
// cluster.
//
// The check runs only when spec.bootstrap has one of the two addons (read
// with the entry parser of `lo addons --detail`) and the domain has a
// kubeconfig (the file that the bash entrypoint exports as KUBECONFIG). It
// makes two reads, each with --request-timeout=10s and a bound of 30 s
// (rbacReadBound). A read runs the credential plugin of that kubeconfig,
// which can ask for a login. A failed read is a warning with a fixed reason
// per class. kubectl's stderr can name the server and the user, so the
// check never prints it. A binding name goes into a printed delete command
// only when it is a DNS-1123 subdomain. Every line is advisory: the check
// never changes the doctor exit code.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kernpilot/lok8s/internal/addons"
	"github.com/kernpilot/lok8s/internal/build"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/domain"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/toolchain"
)

// The addons the check knows, and the opt-in addons that own a grant the
// check would otherwise report.
const (
	rbacSUCAddon        = "system-upgrade-controller"
	rbacSUCAdminAddon   = "system-upgrade-controller-cluster-admin"
	rbacRedisAddon      = "redis-operator"
	rbacRedisChartAddon = "redis-operator-chart-rbac" // #nosec G101 -- an addon name, not a credential
)

// The ServiceAccounts as the addons render them (kind, name, namespace).
var (
	rbacSUCAccount   = rbacSubject{Kind: "ServiceAccount", Name: "system-upgrade-controller", Namespace: "cattle-system"}
	rbacRedisAccount = rbacSubject{Kind: "ServiceAccount", Name: "redis-operator", Namespace: "redis-operator"}
)

// The role names. Old: what the chart bound before the narrowed roles
// (the SUC chart's ClusterRoleBinding system-upgrade-controller had roleRef
// cluster-admin; the redis-operator chart bound its ClusterRole
// redis-operator). New: the main role of the narrowed RBAC. The check
// reports an old binding under any name, so a binding from an older
// upstream manifest (system-upgrade, roleRef cluster-admin) counts too.
const (
	rbacSUCNewRole      = "lok8s-system-upgrade-controller"         // #nosec G101 -- a role name, not a credential
	rbacRedisOldRole    = "redis-operator"                          // #nosec G101 -- a role name, not a credential
	rbacRedisNewRole    = "lok8s-redis-operator"                    // #nosec G101 -- a role name, not a credential
	rbacRedisChartRole  = "lok8s-redis-operator-chart-rbac"         // #nosec G101 -- a role name, not a credential
	rbacRedisExecRole   = "lok8s-redis-operator-exec"               // #nosec G101 -- a role name, not a credential
	rbacClusterAdmin    = "cluster-admin"                           // #nosec G101 -- a role name, not a credential
	rbacAggregatePrefix = "rbac.authorization.k8s.io/aggregate-to-" // #nosec G101 -- a label prefix, not a credential
)

// rbacReadTimeout is the --request-timeout of each read. rbacReadBound
// stops a kubectl that does not end after it (an exec credential plugin
// that waits for a login): the reads are a diagnosis, never a hang. The
// bash twin uses the same 30 s (doctor::_rbac_bound). rbacReadBound is a
// variable so that a test can make it short; rbacReasonNoAnswer names the
// 30 s as a fixed text.
const (
	rbacReadTimeout = "10s"
	rbacReadGrace   = 2 * time.Second
)

var rbacReadBound = 30 * time.Second

// The fixed reasons of a failed read, one per class.
const (
	rbacNotChecked        = "RBAC migration leftovers not checked: "
	rbacReasonNoKubectl   = "kubectl not found"
	rbacReasonForbidden   = "the kubeconfig user cannot list ClusterRoles and ClusterRoleBindings (forbidden or unauthorized)"
	rbacReasonUnreachable = "the cluster API did not answer (unreachable or timed out)"
	rbacReasonNoAnswer    = "kubectl did not answer within 30 s (the cluster API, or a credential plugin that waits for a login)"
	rbacReasonOther       = "kubectl failed or did not return a valid list"
	rbacSkipped           = "RBAC migration check skipped: "
)

// rbacNameRe is a DNS-1123 subdomain (with a length of at most 253). An
// RBAC name can also hold `;`, `$(`, an ESC, a TAB or a newline: a delete
// command is printed only for a name that matches.
var rbacNameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

type rbacSubject struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

type rbacBinding struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	RoleRef struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"roleRef"`
	Subjects []rbacSubject `json:"subjects"`
}

type rbacRule struct {
	APIGroups     []string `json:"apiGroups"`
	Resources     []string `json:"resources"`
	Verbs         []string `json:"verbs"`
	ResourceNames []string `json:"resourceNames"`
}

type rbacRole struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Rules []rbacRule `json:"rules"`
}

// doctorRBACLeftovers prints the lines of the check under the domain
// section. resolve reads spec.bootstrap (bootstrapEntries in production).
func doctorRBACLeftovers(ctx context.Context, r execx.Runner, paths *config.Paths, d, path string, resolve addons.EntryResolver, out io.Writer) {
	if d == "" {
		// The domain section above says it (no active domain).
		return
	}
	spec := filepath.Join(paths.Clusters, d, "cluster.lok8s.yaml")
	if domain.NameRe.MatchString(d) && !fsutil.FileExists(spec) && fsutil.FileExists(filepath.Join(paths.Clusters, d, "deploy.lok8s.yaml")) {
		doctorInfo(out, rbacSkipped+d+" is a deploy domain (run the check on its cluster domain: lo doctor --domain <cluster domain>)")
		return
	}
	if !domain.NameRe.MatchString(d) || !fsutil.FileExists(spec) {
		doctorInfo(out, rbacSkipped+d+" has no cluster.lok8s.yaml")
		return
	}
	kind, err := domain.SpecDriver(spec, "lo")
	if err != nil {
		doctorInfo(out, rbacSkipped+"the cluster spec of "+d+" cannot be read (run: lo lint)")
		return
	}
	// An addon counts only as a bare entry name (Builtin), never as a path
	// entry (bash: the same rule on the entry key).
	enabled := map[string]bool{}
	for _, e := range resolve(spec, kind, d) {
		if e.Builtin {
			enabled[filepath.Base(e.Dir)] = true
		}
	}
	if !enabled[rbacSUCAddon] && !enabled[rbacRedisAddon] {
		doctorInfo(out, rbacSkipped+"spec.bootstrap has no "+rbacSUCAddon+" and no "+rbacRedisAddon)
		return
	}
	checkSUC := enabled[rbacSUCAddon]
	if checkSUC && enabled[rbacSUCAdminAddon] {
		doctorInfo(out, "RBAC migration check of "+rbacSUCAddon+" skipped: spec.bootstrap has "+rbacSUCAdminAddon+", which owns the cluster-admin binding")
		checkSUC = false
	}
	checkRedis := enabled[rbacRedisAddon]
	if !checkSUC && !checkRedis {
		return
	}
	kubeconfig := build.AmbientKubeconfig(paths, d, "")
	if !fsutil.FileExists(kubeconfig) {
		doctorInfo(out, rbacSkipped+"no kubeconfig at "+config.RelTo(paths.Base, kubeconfig))
		return
	}
	kubectl, ok := toolchain.LookPath(path, "kubectl")
	if !ok {
		doctorWarn(out, rbacNotChecked+rbacReasonNoKubectl)
		return
	}
	var bindings []rbacBinding
	if reason := rbacRead(ctx, r, kubectl, kubeconfig, "clusterrolebindings", &bindings); reason != "" {
		doctorWarn(out, rbacNotChecked+reason)
		return
	}
	var roles []rbacRole
	if reason := rbacRead(ctx, r, kubectl, kubeconfig, "clusterroles", &roles); reason != "" {
		doctorWarn(out, rbacNotChecked+reason)
		return
	}
	if checkSUC {
		rbacCheckSUC(bindings, out)
	}
	if checkRedis {
		rbacCheckRedis(bindings, roles, enabled[rbacRedisChartAddon], out)
	}
}

// rbacRead runs one `kubectl get <resource> -o json` into items and
// returns "" or the fixed reason of the failure. The stderr goes into a
// buffer for the class and nowhere else.
func rbacRead[T any](ctx context.Context, r execx.Runner, kubectl, kubeconfig, resource string, items *[]T) string {
	ctx, cancel := context.WithTimeout(ctx, rbacReadBound)
	defer cancel()
	var stdout, stderr bytes.Buffer
	err := r.Run(ctx, execx.Cmd{
		Name:      kubectl,
		Args:      []string{"--kubeconfig", kubeconfig, "get", resource, "-o", "json", "--request-timeout=" + rbacReadTimeout},
		Stdin:     strings.NewReader(""),
		Stdout:    &stdout,
		Stderr:    &stderr,
		WaitDelay: rbacReadGrace,
	})
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return rbacReasonNoAnswer
		}
		return rbacReadReason(stderr.String())
	}
	var list struct {
		Items *[]T `json:"items"`
	}
	if json.Unmarshal(stdout.Bytes(), &list) != nil || list.Items == nil {
		return rbacReasonOther
	}
	*items = *list.Items
	return ""
}

// rbacReadReason maps kubectl's stderr to the class of the failure (bash:
// doctor::_rbac_reason, the same words in the same order).
func rbacReadReason(stderr string) string {
	s := strings.ToLower(stderr)
	for _, w := range []string{"forbidden", "unauthorized", "must be logged in"} {
		if strings.Contains(s, w) {
			return rbacReasonForbidden
		}
	}
	// "was refused": kubectl's own text for a refused connection ("The
	// connection to the server <host> was refused").
	for _, w := range []string{"unable to connect", "connection refused", "was refused", "no such host", "timeout", "deadline exceeded", "network is unreachable", "no route to host"} {
		if strings.Contains(s, w) {
			return rbacReasonUnreachable
		}
	}
	return rbacReasonOther
}

// rbacCheckSUC reports each ClusterRoleBinding that binds the controller's
// ServiceAccount to cluster-admin.
func rbacCheckSUC(bindings []rbacBinding, out io.Writer) {
	left := rbacBindingsOf(bindings, rbacSUCAccount, []string{rbacClusterAdmin})
	if len(left) == 0 {
		doctorOK(out, rbacSUCAddon+": no RBAC migration leftovers")
		return
	}
	rbacNewMissing(bindings, rbacSUCAccount, rbacSUCNewRole, out)
	for _, b := range left {
		rbacBindingLine(b, rbacSUCAccount, out)
	}
}

// rbacCheckRedis reports the old bindings of the operator's ServiceAccount,
// the old ClusterRole and its aggregation labels, and (as a fact) a missing
// pods/exec grant: a RedisCluster needs it (issue #315). The check cannot
// see RedisCluster objects (it reads RBAC only), so that line is a note,
// not a warning.
func rbacCheckRedis(bindings []rbacBinding, roles []rbacRole, chartRBAC bool, out io.Writer) {
	old := []string{rbacClusterAdmin, rbacRedisOldRole}
	if !chartRBAC {
		// The opt-in addon was removed from spec.bootstrap: its binding
		// stays in the cluster (no prune) and keeps the chart's rules.
		old = append(old, rbacRedisChartRole)
	}
	left := rbacBindingsOf(bindings, rbacRedisAccount, old)
	var oldRole *rbacRole
	for i := range roles {
		if roles[i].Metadata.Name == rbacRedisOldRole {
			oldRole = &roles[i]
			break
		}
	}
	if len(left) == 0 && oldRole == nil {
		doctorOK(out, rbacRedisAddon+": no RBAC migration leftovers")
	} else {
		rbacNewMissing(bindings, rbacRedisAccount, rbacRedisNewRole, out)
		for _, b := range left {
			rbacBindingLine(b, rbacRedisAccount, out)
		}
		if oldRole != nil {
			doctorWarn(out, "migration incomplete: ClusterRole "+rbacRedisOldRole+", the old role of the chart, is still in the cluster (run: kubectl delete clusterrole "+rbacRedisOldRole+")")
			if labels := rbacAggregateLabels(oldRole); len(labels) > 0 {
				doctorWarn(out, "migration incomplete: ClusterRole "+rbacRedisOldRole+" copies its rules into built-in roles ("+strings.Join(labels, ", ")+"). The aggregation controller removes the copies after the delete (run: kubectl delete clusterrole "+rbacRedisOldRole+")")
			}
		}
	}
	if !rbacGrantsExec(bindings, roles, rbacRedisAccount) {
		doctorInfo(out, rbacRedisAddon+": no ClusterRoleBinding gives the "+rbacRedisAccount.Name+" ServiceAccount create on pods/exec. A RedisCluster cannot form or scale without it (restore ClusterRoleBinding "+rbacRedisExecRole+" before you add one)")
	}
}

// rbacBindingsOf returns the ClusterRoleBindings whose subjects include sa
// and whose roleRef is one of the ClusterRoles in roles, in list order.
// The match reads the content, never the binding's name.
func rbacBindingsOf(bindings []rbacBinding, sa rbacSubject, roles []string) []rbacBinding {
	var out []rbacBinding
	for _, b := range bindings {
		if b.RoleRef.Kind == "ClusterRole" && slices.Contains(roles, b.RoleRef.Name) && slices.Contains(b.Subjects, sa) {
			out = append(out, b)
		}
	}
	return out
}

// rbacNewMissing warns when no binding gives sa the new role yet: a delete
// of the old binding then leaves the workload without access.
func rbacNewMissing(bindings []rbacBinding, sa rbacSubject, newRole string, out io.Writer) {
	if len(rbacBindingsOf(bindings, sa, []string{newRole})) == 0 {
		doctorWarn(out, "migration incomplete: no ClusterRoleBinding binds the "+sa.Name+" ServiceAccount to "+newRole+" yet (apply the bootstrap before the deletes below: lo bootstrap)")
	}
}

// rbacBindingLine reports one old binding. A name that is not a DNS-1123
// subdomain is never printed: the line then has no name and no command.
func rbacBindingLine(b rbacBinding, sa rbacSubject, out io.Writer) {
	role := b.RoleRef.Name
	name := b.Metadata.Name
	if len(name) > 253 || !rbacNameRe.MatchString(name) {
		doctorWarn(out, "migration incomplete: a ClusterRoleBinding with an unusual name (not a DNS-1123 subdomain) binds the "+sa.Name+" ServiceAccount to "+role+". Doctor does not print that name or a delete command: find the binding and delete it")
		return
	}
	keep := ""
	if rbacOptInRole(sa, role) {
		keep = ", unless you keep it on purpose"
	}
	doctorWarn(out, "migration incomplete: ClusterRoleBinding "+name+" binds the "+sa.Name+" ServiceAccount to "+role+" (run: kubectl delete clusterrolebinding "+name+keep+")")
}

// rbacOptInRole reports a role that an opt-in addon binds the account to
// (system-upgrade-controller-cluster-admin: cluster-admin;
// redis-operator-chart-rbac: its chart role). The check finds an opt-in
// addon only as a bare entry name, so such a binding can be one that the
// person keeps on purpose.
func rbacOptInRole(sa rbacSubject, role string) bool {
	return (sa == rbacSUCAccount && role == rbacClusterAdmin) || (sa == rbacRedisAccount && role == rbacRedisChartRole)
}

// rbacAggregateLabels are the aggregate-to-* labels with the value "true"
// (the selectors of the built-in roles match that value), sorted.
func rbacAggregateLabels(role *rbacRole) []string {
	var labels []string
	for k, v := range role.Metadata.Labels {
		if strings.HasPrefix(k, rbacAggregatePrefix) && v == "true" {
			labels = append(labels, k)
		}
	}
	sort.Strings(labels)
	return labels
}

// rbacGrantsExec reports whether a ClusterRoleBinding gives sa a
// ClusterRole with create on pods/exec in every pod: the core group (or
// *), the resource pods/exec (or *, or */exec), the verb create (or *),
// and no resourceNames.
func rbacGrantsExec(bindings []rbacBinding, roles []rbacRole, sa rbacSubject) bool {
	for _, b := range bindings {
		if b.RoleRef.Kind != "ClusterRole" || !slices.Contains(b.Subjects, sa) {
			continue
		}
		for _, role := range roles {
			if role.Metadata.Name != b.RoleRef.Name {
				continue
			}
			for _, rule := range role.Rules {
				if len(rule.ResourceNames) == 0 &&
					rbacHasAny(rule.APIGroups, "", "*") &&
					rbacHasAny(rule.Resources, "pods/exec", "*", "*/exec") &&
					rbacHasAny(rule.Verbs, "create", "*") {
					return true
				}
			}
		}
	}
	return false
}

func rbacHasAny(list []string, want ...string) bool {
	for _, w := range want {
		if slices.Contains(list, w) {
			return true
		}
	}
	return false
}
