package ui

// NonInteractive reports whether the environment says that no person reads
// the terminal: LOK8S_NONINTERACTIVE is nonempty (any value, "false" too),
// or CI is nonempty and not exactly "false" (so CI=0 and CI=FALSE count
// too).
// getenv reads the environment (os.Getenv, or the env seam of a caller).
// The provision gate does not prompt then (provision.Dispatcher), and the
// kubehz registration does not print the one-time claim ticket on a
// terminal (kubehz claimTicketShown). The bash twin is
// verbose::noninteractive (utils/verbose.sh).
func NonInteractive(getenv func(string) string) bool {
	if getenv("LOK8S_NONINTERACTIVE") != "" {
		return true
	}
	ci := getenv("CI")
	return ci != "" && ci != "false"
}
