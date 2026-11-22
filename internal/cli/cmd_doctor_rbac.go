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
// reports each old object with its delete command. It deletes nothing:
// lo doctor sends only GET requests; a kubeconfig exec credential plugin
// runs as kubectl runs it, and lo does not limit what it does.
//
// The check runs only when spec.bootstrap has one of the two addons (read
// with the entry parser of `lo addons --detail`) and the domain has a
// kubeconfig (the file that the bash entrypoint exports as KUBECONFIG). It
// makes two reads, each with --request-timeout=10s and a bound of 30 s
// (rbacReadBound). A read runs the credential plugin of that kubeconfig,
// which can ask for a login. A failed read is a warning with a fixed reason
// per class. kubectl's stderr can name the server and the user, so the
// check never prints it. A binding counts when it gives the role to the
// account in any form: the ServiceAccount, the User name of its token, or
// a group of the token. A delete command is printed only for a binding of
// the account alone: a binding that the account shares gets a line that
// names it, with no command. A binding name goes into a printed delete
// command only when it is a DNS-1123 subdomain (rbacDeleteCommand checks
// it again). Each delete command names the kubeconfig of the reads
// (shell-quoted) and is the rest of its line, so a copy of it deletes in
// the cluster that the check read and nothing else. The old ClusterRole
// gets a delete command only when no ClusterRoleBinding gives it to
// another subject, and the line says to check the RoleBindings first (the
// check does not read them). Of the labels of that role, the check prints
// only the three aggregation keys of the built-in roles. Every line is
// advisory: the check never changes the doctor exit code.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
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
	rbacSUCNewRole     = "lok8s-system-upgrade-controller" // #nosec G101 -- a role name, not a credential
	rbacRedisOldRole   = "redis-operator"                  // #nosec G101 -- a role name, not a credential
	rbacRedisNewRole   = "lok8s-redis-operator"            // #nosec G101 -- a role name, not a credential
	rbacRedisChartRole = "lok8s-redis-operator-chart-rbac" // #nosec G101 -- a role name, not a credential
	rbacRedisExecRole  = "lok8s-redis-operator-exec"       // #nosec G101 -- a role name, not a credential
	rbacClusterAdmin   = "cluster-admin"                   // #nosec G101 -- a role name, not a credential
)

// The aggregation labels of the built-in ClusterRoles admin, edit and view
// (the keys of their clusterRoleSelectors). The check reports only these
// keys of the old role, matched exactly: it never prints any other key.
const (
	rbacAggregateToAdmin = "rbac.authorization.k8s.io/aggregate-to-admin" // #nosec G101 -- a label key, not a credential
	rbacAggregateToEdit  = "rbac.authorization.k8s.io/aggregate-to-edit"  // #nosec G101 -- a label key, not a credential
	rbacAggregateToView  = "rbac.authorization.k8s.io/aggregate-to-view"  // #nosec G101 -- a label key, not a credential
)

// rbacReadTimeout is the --request-timeout of each read. rbacReadBound
// stops a kubectl that does not end after it (an exec credential plugin
// that waits for a login): the reads are a diagnosis, never a hang. Each
// read runs as its own process group (execx.Cmd.ProcessGroup): at the
// bound the group gets SIGTERM, and SIGKILL after rbacReadGrace, so the
// children of a credential plugin stop too. The bash twin uses the same
// 30 s and 2 s (doctor::_rbac_bound, doctor::_rbac_stop). rbacReadBound is
// a variable so that a test can make it short; rbacReasonNoAnswer names
// the 30 s as a fixed text.
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

// The fields of the lists that the check reads. rbacParseBinding and
// rbacParseRole fill them from the decoded JSON (no struct tags: the keys
// match exactly, not with the case-insensitive match of encoding/json).
type rbacSubject struct {
	Kind      string
	Name      string
	Namespace string
}

type rbacBinding struct {
	Metadata struct {
		Name string
	}
	RoleRef struct {
		Kind string
		Name string
	}
	Subjects []rbacSubject
}

type rbacRule struct {
	APIGroups     []string
	Resources     []string
	Verbs         []string
	ResourceNames []string
}

type rbacRole struct {
	Metadata struct {
		Name   string
		Labels map[string]string
	}
	Rules []rbacRule
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
	if rbacHasControl(kubeconfig) {
		doctorInfo(out, rbacSkipped+"the kubeconfig path has a control character (the delete commands of the check name that path)")
		return
	}
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
	if reason := rbacRead(ctx, r, kubectl, kubeconfig, "clusterrolebindings", nil, rbacParseBinding, &bindings); reason != "" {
		doctorWarn(out, rbacNotChecked+reason)
		return
	}
	var roles []rbacRole
	if reason := rbacRead(ctx, r, kubectl, kubeconfig, "clusterroles", nil, rbacParseRole, &roles); reason != "" {
		doctorWarn(out, rbacNotChecked+reason)
		return
	}
	if checkSUC {
		rbacCheckSUC(bindings, kubeconfig, out)
	}
	if checkRedis {
		rbacCheckRedis(bindings, roles, enabled[rbacRedisChartAddon], kubeconfig, out)
	}
}

// rbacRead runs one `kubectl get <resource> [extra...] -o json` into items
// and returns "" or the fixed reason of the failure. extra are fixed
// arguments of the caller (the label selector of the csi-kms check). The
// stderr goes into a
// buffer for the class and nowhere else. The output must be one JSON
// object whose items is a list of objects, each of the shape that parse
// reads (bash: doctor::_rbac_shape): else the whole list is invalid. A
// list with a continue token (a metadata.continue that is not empty) is a
// part of the list, so it is invalid too: kubectl follows the token itself
// and prints the whole list. The decode into float64 refuses a number out
// of its range, as the bash twin refuses NaN and infinite numbers. A read
// that the bound stopped gives rbacReasonNoAnswer, also when kubectl
// handled the SIGTERM and exited 0. The whole output is in memory only
// for the decode; items keeps only what parse returns.
func rbacRead[T any](ctx context.Context, r execx.Runner, kubectl, kubeconfig, resource string, extra []string, parse func(map[string]any) (T, bool), items *[]T) string {
	data, reason := rbacGet(ctx, r, kubectl, kubeconfig, append([]string{"get", resource}, extra...))
	if reason != "" {
		return reason
	}
	var doc any
	if json.Unmarshal(data, &doc) != nil {
		return rbacReasonOther
	}
	top, _ := doc.(map[string]any)
	list, ok := top["items"].([]any)
	if !ok {
		return rbacReasonOther
	}
	md, ok := rbacObject(top["metadata"])
	if !ok {
		return rbacReasonOther
	}
	if cont, ok := rbacString(md["continue"]); !ok || cont != "" {
		return rbacReasonOther
	}
	out := make([]T, 0, len(list))
	for _, v := range list {
		m, ok := v.(map[string]any)
		if !ok {
			return rbacReasonOther
		}
		item, ok := parse(m)
		if !ok {
			return rbacReasonOther
		}
		out = append(out, item)
	}
	*items = out
	return ""
}

// rbacGet runs one `kubectl --kubeconfig <kubeconfig> <args...> -o json
// --request-timeout=10s` with the bound of a read (rbacReadBound), its own
// process group and the grace (rbacReadGrace). It returns the stdout and "",
// or nil and the fixed reason of the failure (rbacReadReason). The stderr
// goes into a buffer for the class and nowhere else. rbacRead and the
// sso-gate check (cmd_doctor_ssogate.go) use it (bash: doctor::_rbac_get).
func rbacGet(ctx context.Context, r execx.Runner, kubectl, kubeconfig string, args []string) ([]byte, string) {
	ctx, cancel := context.WithTimeout(ctx, rbacReadBound)
	defer cancel()
	var stdout, stderr bytes.Buffer
	full := append([]string{"--kubeconfig", kubeconfig}, args...)
	err := r.Run(ctx, execx.Cmd{
		Name:         kubectl,
		Args:         append(full, "-o", "json", "--request-timeout="+rbacReadTimeout),
		Stdin:        strings.NewReader(""),
		Stdout:       &stdout,
		Stderr:       &stderr,
		WaitDelay:    rbacReadGrace,
		ProcessGroup: true,
	})
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, rbacReasonNoAnswer
	}
	if err != nil {
		return nil, rbacReadReason(stderr.String())
	}
	return stdout.Bytes(), ""
}

// The shape that both twins read (bash: doctor::_rbac_shape). A field may
// be absent or null: it is then empty. A field that is there has the type
// that the check reads, and so has each element of a list (no null). Any
// other value makes the whole list invalid, also in an item that the
// check does not report.

// rbacParseBinding reads metadata.name, roleRef.kind, roleRef.name and the
// kind, name and namespace of each subject.
func rbacParseBinding(m map[string]any) (rbacBinding, bool) {
	var b rbacBinding
	md, ok1 := rbacObject(m["metadata"])
	ref, ok2 := rbacObject(m["roleRef"])
	subjects, ok3 := rbacObjects(m["subjects"])
	if !ok1 || !ok2 || !ok3 {
		return b, false
	}
	var ok4, ok5, ok6 bool
	b.Metadata.Name, ok4 = rbacString(md["name"])
	b.RoleRef.Kind, ok5 = rbacString(ref["kind"])
	b.RoleRef.Name, ok6 = rbacString(ref["name"])
	if !ok4 || !ok5 || !ok6 {
		return b, false
	}
	for _, sm := range subjects {
		kind, ok1 := rbacString(sm["kind"])
		name, ok2 := rbacString(sm["name"])
		namespace, ok3 := rbacString(sm["namespace"])
		if !ok1 || !ok2 || !ok3 {
			return b, false
		}
		b.Subjects = append(b.Subjects, rbacSubject{Kind: kind, Name: name, Namespace: namespace})
	}
	return b, true
}

// rbacParseRole reads metadata.name, metadata.labels (string values) and
// the apiGroups, resources, verbs and resourceNames of each rule.
func rbacParseRole(m map[string]any) (rbacRole, bool) {
	var role rbacRole
	md, ok1 := rbacObject(m["metadata"])
	rules, ok2 := rbacObjects(m["rules"])
	if !ok1 || !ok2 {
		return role, false
	}
	var ok3, ok4 bool
	role.Metadata.Name, ok3 = rbacString(md["name"])
	role.Metadata.Labels, ok4 = rbacStringMap(md["labels"])
	if !ok3 || !ok4 {
		return role, false
	}
	for _, rm := range rules {
		var rule rbacRule
		var ok [4]bool
		rule.APIGroups, ok[0] = rbacStrings(rm["apiGroups"])
		rule.Resources, ok[1] = rbacStrings(rm["resources"])
		rule.Verbs, ok[2] = rbacStrings(rm["verbs"])
		rule.ResourceNames, ok[3] = rbacStrings(rm["resourceNames"])
		if ok != [4]bool{true, true, true, true} {
			return role, false
		}
		role.Rules = append(role.Rules, rule)
	}
	return role, true
}

// rbacObject is v as an object; absent or null is an empty one.
func rbacObject(v any) (map[string]any, bool) {
	if v == nil {
		return nil, true
	}
	m, ok := v.(map[string]any)
	return m, ok
}

// rbacObjects is v as a list of objects; absent or null is an empty one.
func rbacObjects(v any) ([]map[string]any, bool) {
	if v == nil {
		return nil, true
	}
	list, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]map[string]any, 0, len(list))
	for _, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			return nil, false
		}
		out = append(out, m)
	}
	return out, true
}

// rbacString is v as a string; absent or null is "".
func rbacString(v any) (string, bool) {
	if v == nil {
		return "", true
	}
	s, ok := v.(string)
	return s, ok
}

// rbacStrings is v as a list of strings; absent or null is an empty one.
func rbacStrings(v any) ([]string, bool) {
	if v == nil {
		return nil, true
	}
	list, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		s, ok := e.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// rbacStringMap is v as an object of strings; absent or null is an empty
// one.
func rbacStringMap(v any) (map[string]string, bool) {
	m, ok := rbacObject(v)
	if !ok {
		return nil, false
	}
	out := make(map[string]string, len(m))
	for k, e := range m {
		s, ok := e.(string)
		if !ok {
			return nil, false
		}
		out[k] = s
	}
	return out, true
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

// rbacCheckSUC reports each ClusterRoleBinding that gives the controller's
// ServiceAccount cluster-admin.
func rbacCheckSUC(bindings []rbacBinding, kubeconfig string, out io.Writer) {
	left := rbacGrantsTo(bindings, rbacSUCAccount, []string{rbacClusterAdmin})
	if len(left) == 0 {
		doctorOK(out, rbacSUCAddon+": no RBAC migration leftovers")
		return
	}
	rbacNewMissing(bindings, rbacSUCAccount, rbacSUCNewRole, out)
	for _, g := range left {
		rbacGrantLine(g, rbacSUCAccount, kubeconfig, out)
	}
}

// rbacCheckRedis reports the old bindings of the operator's ServiceAccount,
// the old ClusterRole and its aggregation labels, and (as a fact) a missing
// pods/exec grant: a RedisCluster needs it (issue #315). The check cannot
// see RedisCluster objects (it reads RBAC only), so that line is a note,
// not a warning.
func rbacCheckRedis(bindings []rbacBinding, roles []rbacRole, chartRBAC bool, kubeconfig string, out io.Writer) {
	old := []string{rbacClusterAdmin, rbacRedisOldRole}
	if !chartRBAC {
		// The opt-in addon was removed from spec.bootstrap: its binding
		// stays in the cluster (no prune) and keeps the chart's rules.
		old = append(old, rbacRedisChartRole)
	}
	left := rbacGrantsTo(bindings, rbacRedisAccount, old)
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
		for _, g := range left {
			rbacGrantLine(g, rbacRedisAccount, kubeconfig, out)
		}
		if oldRole != nil {
			line, command := rbacOldRoleLine(rbacOtherRefs(bindings, rbacRedisAccount, rbacRedisOldRole), kubeconfig)
			doctorWarn(out, "migration incomplete: "+line)
			if labels := rbacAggregateLabels(oldRole); len(labels) > 0 {
				line = "migration incomplete: ClusterRole " + rbacRedisOldRole + " copies its rules into built-in roles (" + strings.Join(labels, ", ") + "). The aggregation controller removes the copies when the role is deleted"
				if command {
					line += " (the command above)"
				}
				doctorWarn(out, line)
			}
		}
	}
	if !rbacGrantsExec(bindings, roles, rbacRedisAccount) {
		doctorInfo(out, rbacRedisAddon+": no ClusterRoleBinding gives the "+rbacRedisAccount.Name+" ServiceAccount create on pods/exec. A RedisCluster cannot form or scale without it (restore ClusterRoleBinding "+rbacRedisExecRole+" before you add one)")
	}
}

// rbacOtherRefs returns the ClusterRoleBindings to the ClusterRole role
// that give it to a subject that is not sa in any form (rbacGrantOf
// counts them): a group, a user or another ServiceAccount. A delete of
// the role ends their grant too. A binding of sa alone has a line and a
// delete command of its own.
func rbacOtherRefs(bindings []rbacBinding, sa rbacSubject, role string) []rbacBinding {
	var out []rbacBinding
	for _, b := range bindings {
		if b.RoleRef.Kind != "ClusterRole" || b.RoleRef.Name != role {
			continue
		}
		if g, _ := rbacGrantOf(b, sa); g.others > 0 {
			out = append(out, b)
		}
	}
	return out
}

// rbacOldRoleLine is the line of the old ClusterRole (bash: the redis-ref
// records in doctor::_rbac_report). Without refs (rbacOtherRefs) it ends
// with the delete command, after the advice to check the RoleBindings,
// which the check does not read. With refs it names them (a name that is
// not a DNS-1123 subdomain only in a count) and has no command: a delete
// would end their grants too. command reports a printed command.
func rbacOldRoleLine(refs []rbacBinding, kubeconfig string) (line string, command bool) {
	line = "ClusterRole " + rbacRedisOldRole + ", the old role of the chart, is still in the cluster"
	cmd, ok := rbacDeleteCommand(kubeconfig, "clusterrole", rbacRedisOldRole)
	switch {
	case len(refs) == 0 && ok:
		return line + ". Doctor does not read RoleBindings: check that none refers to it, and then delete it: " + cmd, true
	case len(refs) == 0:
		return line + ". Doctor prints no delete command for the role: check that no RoleBinding refers to it (doctor does not read RoleBindings), and then delete it", false
	}
	var names []string
	odd := 0
	for _, b := range refs {
		if rbacNameOK(b.Metadata.Name) {
			names = append(names, b.Metadata.Name)
		} else {
			odd++
		}
	}
	if odd > 0 {
		names = append(names, strconv.Itoa(odd)+" with an unusual name")
	}
	those := "those bindings"
	if len(refs) == 1 {
		line += ", and 1 ClusterRoleBinding gives it to subjects other than the " + rbacRedisAccount.Name + " ServiceAccount"
		those = "that binding"
	} else {
		line += ", and " + strconv.Itoa(len(refs)) + " ClusterRoleBindings give it to subjects other than the " + rbacRedisAccount.Name + " ServiceAccount"
	}
	line += " (" + strings.Join(names, ", ") + "). Doctor prints no delete command for the role: give the subjects of " + those + " another role first, check that no RoleBinding refers to it (doctor does not read RoleBindings), and then delete it"
	return line, false
}

// rbacGrant is a ClusterRoleBinding that gives its role to an account:
// the account is a subject (direct), or a subject is a group of the
// account's token (group). Others counts the subjects that are not the
// account.
type rbacGrant struct {
	binding rbacBinding
	direct  bool
	group   string
	others  int
}

// rbacIsAccount reports a subject that is the account: the ServiceAccount
// (in its namespace), or the User name of its token,
// system:serviceaccount:<namespace>:<name>. The authorizer reads neither
// the apiGroup of a subject nor the namespace of a User.
func rbacIsAccount(s, sa rbacSubject) bool {
	if s.Kind == "User" {
		return s.Name == "system:serviceaccount:"+sa.Namespace+":"+sa.Name
	}
	return s == sa
}

// rbacAccountGroups are the groups of a ServiceAccount token.
func rbacAccountGroups(sa rbacSubject) []string {
	return []string{"system:serviceaccounts:" + sa.Namespace, "system:serviceaccounts", "system:authenticated"}
}

// rbacGrantOf reads the subjects of b for sa: ok when b gives its role to
// sa in any form. group is the first subject that is a group of sa.
func rbacGrantOf(b rbacBinding, sa rbacSubject) (rbacGrant, bool) {
	g := rbacGrant{binding: b}
	groups := rbacAccountGroups(sa)
	for _, s := range b.Subjects {
		if rbacIsAccount(s, sa) {
			g.direct = true
			continue
		}
		g.others++
		if g.group == "" && s.Kind == "Group" && slices.Contains(groups, s.Name) {
			g.group = s.Name
		}
	}
	return g, g.direct || g.group != ""
}

// rbacGrantsTo returns the ClusterRoleBindings that give sa one of the
// ClusterRoles in roles, in list order. The match reads the content,
// never the binding's name.
func rbacGrantsTo(bindings []rbacBinding, sa rbacSubject, roles []string) []rbacGrant {
	var out []rbacGrant
	for _, b := range bindings {
		if b.RoleRef.Kind != "ClusterRole" || !slices.Contains(roles, b.RoleRef.Name) {
			continue
		}
		if g, ok := rbacGrantOf(b, sa); ok {
			out = append(out, g)
		}
	}
	return out
}

// rbacNewMissing warns when no binding gives sa the new role yet: a delete
// of the old binding then leaves the workload without access.
func rbacNewMissing(bindings []rbacBinding, sa rbacSubject, newRole string, out io.Writer) {
	if len(rbacGrantsTo(bindings, sa, []string{newRole})) == 0 {
		doctorWarn(out, "migration incomplete: no ClusterRoleBinding binds the "+sa.Name+" ServiceAccount to "+newRole+" yet (apply the bootstrap before the deletes below: lo bootstrap)")
	}
}

// rbacGrantLine reports one old grant. Only a binding of the account
// alone gets a delete command, as the rest of its line (the words before
// it say when to keep the binding). A grant through a group (it also
// gives the role to other accounts) and a binding with other subjects
// get no command: a delete would also remove the grant of the others. A
// name that is not a DNS-1123 subdomain is never printed: the line then
// has no name and no command. For a role that an opt-in addon binds the
// account to (rbacOptInRole), each advice to remove the grant says to
// keep it only on purpose.
func rbacGrantLine(g rbacGrant, sa rbacSubject, kubeconfig string, out io.Writer) {
	role := g.binding.RoleRef.Name
	name := g.binding.Metadata.Name
	shown := rbacNameOK(name)
	what := "ClusterRoleBinding " + name
	if !shown {
		what = "a ClusterRoleBinding with an unusual name (not a DNS-1123 subdomain)"
	}
	account := "the " + sa.Name + " ServiceAccount"
	keep := ""
	if rbacOptInRole(sa, role) {
		keep = "keep it only on purpose, else "
	}
	const unusual = "Doctor does not print that name or a delete command: "
	var line string
	switch {
	case g.group != "":
		line = what + " gives " + role + " to the group " + g.group + ", which includes " + account + ". "
		if shown {
			line += "Doctor prints no delete command for a shared grant: review the binding"
		} else {
			line += unusual + "find the binding and review it"
		}
	case g.others > 0:
		line = what + " binds " + account + " and " + rbacOtherSubjects(g.others) + " to " + role + ". "
		if shown {
			line += "Doctor prints no delete command for a binding with other subjects: " + keep + "remove " + account + " from its subjects (for example with kubectl edit)"
		} else {
			line += unusual + keep + "find the binding and remove " + account + " from its subjects"
		}
	default:
		line = what + " binds " + account + " to " + role + ". "
		// rbacDeleteCommand checks the name again: a name that fails gets
		// the line without the command.
		if cmd, ok := rbacDeleteCommand(kubeconfig, "clusterrolebinding", name); shown && ok {
			del := "Delete it: "
			if keep != "" {
				del = "Keep it only on purpose, else delete it: "
			}
			line += del + cmd
		} else {
			line += unusual + keep + "find the binding and delete it"
		}
	}
	doctorWarn(out, "migration incomplete: "+line)
}

// rbacOtherSubjects is "1 other subject" or "<n> other subjects".
func rbacOtherSubjects(n int) string {
	if n == 1 {
		return "1 other subject"
	}
	return strconv.Itoa(n) + " other subjects"
}

// rbacDeleteCommand is the delete command of one object in the cluster of
// the kubeconfig that the reads used (bash: doctor::_rbac_delete). It
// checks the name again where it builds the command: ok is false, and
// there is no command, for a name that is not a DNS-1123 subdomain
// (rbacNameOK). The kubeconfig path is shell-quoted (rbacShQuote).
func rbacDeleteCommand(kubeconfig, resource, name string) (string, bool) {
	return rbacDeleteCommandIn(kubeconfig, resource, "", name)
}

// rbacDeleteCommandIn is rbacDeleteCommand for an object in a namespace
// ("" for a cluster-scoped object): the command ends with --namespace
// <namespace>. It checks the namespace again too: ok is false for a
// namespace that is not a DNS-1123 label (rbacLabelOK). Bash:
// doctor::_rbac_delete with its fourth argument.
func rbacDeleteCommandIn(kubeconfig, resource, namespace, name string) (string, bool) {
	if !rbacNameOK(name) || (namespace != "" && !rbacLabelOK(namespace)) {
		return "", false
	}
	cmd := "kubectl --kubeconfig " + rbacShQuote(kubeconfig) + " delete " + resource + " " + name
	if namespace != "" {
		cmd += " --namespace " + namespace
	}
	return cmd, true
}

// rbacNameOK reports a DNS-1123 subdomain of at most 253 characters: the
// only names that the check prints.
func rbacNameOK(name string) bool {
	return len(name) <= 253 && rbacNameRe.MatchString(name)
}

// rbacLabelRe is a DNS-1123 label: the form of a namespace name, and of the
// option label value that the csi-kms check prints.
var rbacLabelRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// rbacLabelOK reports a DNS-1123 label of at most 63 characters.
func rbacLabelOK(s string) bool {
	return len(s) <= 63 && rbacLabelRe.MatchString(s)
}

// rbacShQuote quotes s for a POSIX shell: in single quotes, and each
// single quote of s ends the quotes, adds a backslash-escaped quote and
// opens them again (bash: doctor::_rbac_quote).
func rbacShQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// rbacHasControl reports an ASCII control character (a byte below 0x20,
// or DEL) in s. The check prints the kubeconfig path in its commands, and
// such a character can end the line or drive the terminal.
func rbacHasControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// rbacOptInRole reports a role that an opt-in addon binds the account to
// (system-upgrade-controller-cluster-admin: cluster-admin;
// redis-operator-chart-rbac: its chart role). The check finds an opt-in
// addon only as a bare entry name, so such a binding can be one that the
// person keeps on purpose.
func rbacOptInRole(sa rbacSubject, role string) bool {
	return (sa == rbacSUCAccount && role == rbacClusterAdmin) || (sa == rbacRedisAccount && role == rbacRedisChartRole)
}

// rbacAggregateLabels are the aggregation keys of the built-in roles
// (admin, edit, view) that role has with the value "true" (the selectors
// of the built-in roles match that value), in that order. A key matches
// only exactly: any other key of the role is never printed.
func rbacAggregateLabels(role *rbacRole) []string {
	var labels []string
	for _, k := range []string{rbacAggregateToAdmin, rbacAggregateToEdit, rbacAggregateToView} {
		if role.Metadata.Labels[k] == "true" {
			labels = append(labels, k)
		}
	}
	return labels
}

// rbacGrantsExec reports whether a ClusterRoleBinding gives sa (in any
// form, rbacGrantOf) a ClusterRole with create on pods/exec in every pod:
// the core group (or *), the resource pods/exec (or *, or */exec), the
// verb create (or *), and no resourceNames.
func rbacGrantsExec(bindings []rbacBinding, roles []rbacRole, sa rbacSubject) bool {
	for _, b := range bindings {
		if _, ok := rbacGrantOf(b, sa); b.RoleRef.Kind != "ClusterRole" || !ok {
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
