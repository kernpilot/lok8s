package provision

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/ui"
)

// Gate actions (bash: provision::confirm_infra's <action> argument).
const (
	ActionReconcile = "reconcile"
	ActionBootstrap = "bootstrap"
	ActionDestroy   = "destroy"
)

// interactive reports whether we may prompt on a terminal (bash:
// provision::_interactive; the Interactive field stubs it in tests exactly
// like the bats redefine the function).
//
// NOTE: LOK8S_NONINTERACTIVE means REFUSE here, while recover's confirm
// treats it as consent — deliberate: recover is an explicit DR command whose
// entry already demanded a literal yes; up/provision are ambient and fail
// closed.
func (d *Dispatcher) interactive() bool {
	if d.Interactive != nil {
		return d.Interactive()
	}
	if os.Getenv("LOK8S_NONINTERACTIVE") != "" {
		return false
	}
	if ci := os.Getenv("CI"); ci != "" && ci != "false" {
		return false
	}
	f, ok := d.stdin().(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

func (d *Dispatcher) stdin() io.Reader {
	if d.In != nil {
		return d.In
	}
	return os.Stdin
}

// readAnswer reads one prompt answer (bash: `read -r ans` — one line,
// trailing newline stripped, surrounding IFS whitespace trimmed). An EOF
// with no data is a failed read (→ abort).
func (d *Dispatcher) readAnswer() (string, error) {
	if d.promptReader == nil {
		d.promptReader = bufio.NewReader(d.stdin())
	}
	line, err := d.promptReader.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.Trim(line, " \t\r\n"), nil
}

// ConfirmInfra is the real-infrastructure gate (bash:
// provision::confirm_infra). Cloud drivers (kubeone/capi/kkp) reconcile
// REAL infrastructure — before any of that runs, show what is about to
// happen and require an interactive yes. The local kind driver (lo) is
// exempt UNLESS remote mode (it then provisions a cloud VM via
// spec.provider — real infrastructure too); --force bypasses. destroy
// demands a literal "yes"; reconcile/bootstrap accept y/yes. Returns
// driver.ErrDeclined (the rc-3 sentinel) on a decline or a non-interactive
// refusal so callers can tell "operator said no" apart from a real failure.
//
// The summary/prompt strings are byte-identical to the bash (the bats pin
// them); the prompt goes to Stderr, the answer is read from In.
func (d *Dispatcher) ConfirmInfra(domainName, clusterYAML, kind, action string) error {
	if kind == "lo" && !d.Remote {
		return nil
	}
	if d.Force {
		return nil
	}

	info, _ := readSpecInfo(clusterYAML)
	clusterName := orQuestion(info.Metadata.Name)
	k8sVersion := orQuestion(yqScalarOrEmpty(info.Spec.Kubernetes.Version))
	providerName := orQuestion(info.Spec.Provider.Name)
	addons := len(info.Spec.Bootstrap)

	w := d.errWriter()
	fmt.Fprintf(w, "\n  \033[1;33m⚠ %s\033[0m \033[2m·\033[0m cluster \033[1m%s\033[0m (%s) targets \033[1mreal infrastructure\033[0m \033[2m(%s driver · provider %s)\033[0m\n",
		domainName, clusterName, k8sVersion, kind, providerName)
	switch action {
	case ActionDestroy:
		fmt.Fprintf(w, "    \033[31mnext: driver destroy — deprovisions the cluster's cloud resources (servers, LBs, volumes)\033[0m\n")
	case ActionBootstrap:
		fmt.Fprintf(w, "    \033[2mnext: re-apply %d bootstrap addons on the LIVE cluster\033[0m\n", addons)
	default:
		if kind == "lo" {
			// Only reachable in remote mode — the local path is exempt above.
			fmt.Fprintf(w, "    \033[2mnext: provider reconcile (provisions/updates the remote VM) → kind cluster on the\033[0m\n")
			fmt.Fprintf(w, "    \033[2m      VM → kubehz registration → bootstrap DAG (%d addons)\033[0m\n", addons)
		} else {
			fmt.Fprintf(w, "    \033[2mnext: provider reconcile (creates/updates cloud resources) → %s apply (can roll the\033[0m\n", kind)
			fmt.Fprintf(w, "    \033[2m      control plane) → addons → kubehz registration → bootstrap DAG (%d addons)\033[0m\n", addons)
		}
	}

	if !d.interactive() {
		ui.Errorf(w, "refusing to %s '%s' non-interactively — re-run with --force", action, domainName)
		return driver.ErrDeclined
	}

	abort := func() error {
		ui.Errorf(w, "aborted — '%s' left untouched", domainName)
		return driver.ErrDeclined
	}
	if action == ActionDestroy {
		fmt.Fprintf(w, "\n  continue? [type yes to continue] ")
		ans, err := d.readAnswer()
		if err != nil || ans != "yes" {
			return abort()
		}
		return nil
	}
	fmt.Fprintf(w, "\n  proceed? [y/N] ")
	ans, err := d.readAnswer()
	if err != nil {
		return abort()
	}
	low := strings.ToLower(ans)
	if low != "y" && low != "yes" {
		return abort()
	}
	return nil
}

// orQuestion maps a missing scalar to the summary's "?" placeholder (bash:
// yq `// "?"`).
func orQuestion(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

// yqScalarOrEmpty stringifies a decoded YAML scalar the way `yq -r` prints
// it, with nil → "" (the `// "?"` fallback is orQuestion's job).
func yqScalarOrEmpty(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}
