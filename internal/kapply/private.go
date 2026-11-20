package kapply

// private.go — the kubectl output of an apply stays out of the log. The
// output of `kubectl apply` (stdout and stderr) can quote a value of the
// manifest, also of a Secret: a field error prints "Invalid value: <value>",
// an admission webhook can quote the object it refused. lo reads the output
// (the classification: conflict, race, count) and prints fixed lines. The
// opt-in LO_SHOW_SENSITIVE_OUTPUT=1 prints the kubectl text as before. The
// bash twin is utils/kapply.sh (kapply::show_sensitive,
// kapply::_apply_pass, kapply::counted_lines).

import (
	"fmt"
	"os"
	"strings"
)

// ShowSensitiveEnv is the opt-in: with the value 1, lo prints the kubectl
// output that it keeps private by default (the output of kubectl apply,
// and at debug the diff text of a bootstrap dry run).
const ShowSensitiveEnv = "LO_SHOW_SENSITIVE_OUTPUT"

// ShowSensitive reports whether the opt-in is on: ShowSensitiveEnv is
// exactly "1" (bash: kapply::show_sensitive).
func ShowSensitive() bool {
	return os.Getenv(ShowSensitiveEnv) == "1"
}

// ApplyFailedText is the fixed line of a failed apply pass: the label, the
// exit code of kubectl, and how to see the kubectl output. It never holds
// the kubectl output (bash: the error line of kapply::_apply_pass).
func ApplyFailedText(label string, rc int) string {
	return fmt.Sprintf("kapply: kubectl apply failed for %s (rc=%d). lo does not show the kubectl output: it can quote a value of the manifest. To see it, run again with %s=1", label, rc, ShowSensitiveEnv)
}

// ShowSensitiveText is the [warn] line before kubectl output that lo
// prints under the opt-in. what names the output.
func ShowSensitiveText(what string) string {
	return fmt.Sprintf("%s=1: lo shows %s, which can quote a value of the manifest", ShowSensitiveEnv, what)
}

// CountedLines returns the lines of a kubectl output that RenderCaptured
// counts and never prints: a line that ends in an OK verb and whose first
// word has the <type>/<name> shape. Each line ends in a newline. The
// bootstrap scheduler feeds these lines of the private output to
// RenderCaptured for the "· N resources" count; every other line of the
// private output stays out of the log (bash: kapply::counted_lines).
func CountedLines(out string) string {
	var b strings.Builder
	for _, line := range splitLines(out) {
		if !OKRe.MatchString(line) {
			continue
		}
		if key, _, _ := strings.Cut(line, " "); key != "" && strings.Contains(key, "/") {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return b.String()
}
