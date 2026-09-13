package execx

import (
	"os"
	"slices"
	"strings"
)

// PrependPATH returns the process PATH with dirs prepended, in the given
// order, each only when it is not on PATH already; an empty dir is
// skipped. It is the one spelling of "the PATH lo prepares for a child"
// (the bash shim, the provider bridge, recover, the render children).
func PrependPATH(dirs ...string) string {
	path := os.Getenv("PATH")
	entries := strings.Split(path, string(os.PathListSeparator))
	for _, dir := range slices.Backward(dirs) {
		if dir == "" || slices.Contains(entries, dir) {
			continue
		}
		path = dir + string(os.PathListSeparator) + path
		entries = append(entries, dir)
	}
	return path
}
