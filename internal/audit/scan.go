package audit

// Manifest-file scanning: the file listers and the grep-shaped counters. The
// regexes carry the same PORTABLE word-boundary `([^[:alnum:]_]|$)` the bash
// uses (POSIX ERE — no `\b`), so pattern semantics match grep -E exactly.

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	reNodePort     = regexp.MustCompile(`^[[:space:]]*type:[[:space:]]*NodePort([^[:alnum:]_]|$)`)
	reLoadBalancer = regexp.MustCompile(`^[[:space:]]*type:[[:space:]]*LoadBalancer([^[:alnum:]_]|$)`)
	reHTTPRoute    = regexp.MustCompile(`^[[:space:]]*kind:[[:space:]]*HTTPRoute([^[:alnum:]_]|$)`)
	rePrivileged   = regexp.MustCompile(`^[[:space:]]*privileged:[[:space:]]*true([^[:alnum:]_]|$)`)
	reHostNetwork  = regexp.MustCompile(`^[[:space:]]*hostNetwork:[[:space:]]*true([^[:alnum:]_]|$)`)
	reHostPath     = regexp.MustCompile(`^[[:space:]]*hostPath:`)
	reHTTPURL      = regexp.MustCompile(`http://[a-zA-Z0-9._-]+`)
	reHTTPExclude  = regexp.MustCompile(`http://(localhost|127\.0\.0\.1|www\.w3\.org|schemas?\.|.*\.svc([.:]|$))`)
)

// targetFiles lists the cluster's own hand-written target manifests
// (bash: audit::_target_files — find targets/ -type f -name '*.yaml' -o '*.yml').
func targetFiles(domainDir string) []string {
	return findFiles(filepath.Join(domainDir, "targets"), ".yaml", ".yml")
}

// artifactFiles lists what `lo build` rendered: clusters/<domain>/artifacts.yaml
// plus every *.yaml under clusters/<domain>/artifacts/ (bash: audit::_artifact_files).
func artifactFiles(domainDir string) []string {
	var files []string
	if isFile(filepath.Join(domainDir, "artifacts.yaml")) {
		files = append(files, filepath.Join(domainDir, "artifacts.yaml"))
	}
	files = append(files, findFiles(filepath.Join(domainDir, "artifacts"), ".yaml")...)
	return files
}

// findFiles walks root for REGULAR files with one of the extensions (find
// -type f does not follow symlinks; neither does this). Sorted for
// determinism — every consumer counts or scans, so order never changes a
// verdict.
func findFiles(root string, exts ...string) []string {
	var files []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return nil //nolint:nilerr // fail-soft: unreadable subtrees scan as absent
		}
		for _, ext := range exts {
			if strings.HasSuffix(path, ext) {
				files = append(files, path)
				break
			}
		}
		return nil
	})
	sort.Strings(files)
	return files
}

// grepCountFiles counts the FILES with at least one line matching re
// (bash: audit::_grep_count — grep -lsE | wc -l; unreadable files count 0).
func grepCountFiles(re *regexp.Regexp, files []string) int {
	n := 0
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if re.MatchString(line) {
				n++
				break
			}
		}
	}
	return n
}

// artifactsContain reports whether any of the files contains the fixed
// string (bash: audit::_artifacts_contain — grep -qsF).
func artifactsContain(files []string, needle string) bool {
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if strings.Contains(string(raw), needle) {
			return true
		}
	}
	return false
}

// plaintextHits counts the UNIQUE http:// endpoints referenced across the
// files, excluding localhost / schema URLs / in-cluster .svc (bash:
// grep -rhosE 'http://…' | grep -vE '…' | sort -u | wc -l).
func plaintextHits(files []string) int {
	seen := map[string]bool{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n") {
			for _, m := range reHTTPURL.FindAllString(line, -1) {
				if reHTTPExclude.MatchString(m) {
					continue
				}
				seen[m] = true
			}
		}
	}
	return len(seen)
}
