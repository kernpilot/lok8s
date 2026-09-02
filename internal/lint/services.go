package lint

// services.yaml + lok8s.yaml schema validation, plus the drift warnings.
//
// KEEP IN SYNC WITH .lok8s/tilt/Tiltfile  _validate_services_yaml / _validate_service
// -----------------------------------------------------------------------------
// The Tilt extension (.lok8s/tilt/Tiltfile, Starlark) validates the SAME two
// files at `tilt up` time via fail(). These allow-lists MUST mirror the
// Starlark ones so `lo lint` and `tilt up` agree on what is valid.
//
// Why duplicated and not a single shared file: a shared schema file only
// prevents drift if BOTH readers consume it. The Starlark extension hardcodes
// its allow-lists inline (and is owned/edited separately), so a YAML/JSON file
// read only by this side would be drift-theater — it would not keep the
// Starlark copy honest. Until the extension is changed to read the same file,
// the only truthful option is to duplicate the lists HERE, adjacent and
// clearly labelled, so a reviewer can diff the two by eye. The slices below
// are grouped to match the Starlark `_allowed`/`_*_allowed` locals
// one-for-one. (They also mirror the bash `_LINT_*` arrays in .lok8s/libs/lint
// while both implementations ship.)
//
// DELTA (intentional): `components` is a NEW per-service key (review P1.2 /
// multi-image). lint validates it now; the Starlark `_validate_service` does
// not yet iterate it. When the extension gains `components` support, this
// delta disappears — the allow-lists converge again.
// -----------------------------------------------------------------------------

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/ui"
)

// services.yaml — top-level + sub-block allow-lists.
var (
	lintServicesTop          = []string{"apiVersion", "kind", "metadata", "registry", "defaults", "services"}
	lintServicesRegistry     = []string{"endpoint", "branch", "tag", "prefix", "parallel"}
	lintServicesDefaults     = []string{"build", "dockerfile"}
	lintServiceEntry         = []string{"enabled", "build", "path", "namespace", "dockerfile", "watch", "registry", "image"}
	lintServiceEntryRegistry = []string{"endpoint", "branch", "tag", "prefix"}

	// per-service lok8s.yaml — top-level + components-entry allow-lists.
	lintLok8sTop       = []string{"build", "ports", "links", "workloads", "tilt", "components"}
	lintLok8sComponent = []string{"name", "build", "ports", "workloads", "links"}
)

// inList is a membership test (bash: _lint_in_list).
func inList(needle string, haystack []string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}

// allowedHint formats an allow-list as a comma-separated hint string
// ("a, b, c"). Bash: _lint_allowed_hint.
func allowedHint(list []string) string {
	return strings.Join(list, ", ")
}

// servicesFile resolves the repo-root services catalog, "" when absent
// (legacy filename fallback mirrors env::services).
func (l *Linter) servicesFile() string {
	svcFile := l.Paths.Base + "/services.yaml"
	if isFile(svcFile) {
		return svcFile
	}
	svcFile = l.Paths.Base + "/services.base.yaml"
	if isFile(svcFile) {
		return svcFile
	}
	return ""
}

// services validates the repo-root services.yaml against the top-level
// allow-list, each services.<name> entry, then resolves each enabled
// service's path: and validates the per-service lok8s.yaml there (bash:
// lint::services). Returns false if any errors were found (true also when no
// services.yaml).
func (l *Linter) services() bool {
	svcFile := l.servicesFile()
	if svcFile == "" {
		return true
	}

	fmt.Fprintln(l.Out, "Validating: services.yaml")
	errs := 0
	root := firstDoc(svcFile)

	// -- top-level keys --
	topKeys, _ := mapKeys(root)
	for _, k := range topKeys {
		if k == "" {
			continue
		}
		if !inList(k, lintServicesTop) {
			ui.Errorf(l.ErrOut, "  services.yaml: unknown top-level key '%s' — allowed: %s", k, allowedHint(lintServicesTop))
			errs++
		}
	}

	// -- registry block --
	if inList("registry", topKeys) {
		regKeys, _ := mapKeys(nodeAt(root, "registry"))
		for _, k := range regKeys {
			if k == "" {
				continue
			}
			if !inList(k, lintServicesRegistry) {
				ui.Errorf(l.ErrOut, "  services.yaml: unknown key 'registry.%s' — allowed: %s", k, allowedHint(lintServicesRegistry))
				errs++
			}
		}
	}

	// -- defaults block --
	if inList("defaults", topKeys) {
		defKeys, _ := mapKeys(nodeAt(root, "defaults"))
		for _, k := range defKeys {
			if k == "" {
				continue
			}
			if !inList(k, lintServicesDefaults) {
				ui.Errorf(l.ErrOut, "  services.yaml: unknown key 'defaults.%s' — allowed: %s", k, allowedHint(lintServicesDefaults))
				errs++
			}
		}
		df := valueOr(nodeAt(root, "defaults", "dockerfile"), "")
		if df != "" && df != "service" && df != "production" {
			ui.Errorf(l.ErrOut, "  services.yaml: defaults.dockerfile must be 'service' or 'production', got '%s'", df)
			errs++
		}
	}

	// -- per-service entries --
	svcNames, _ := mapKeys(nodeAt(root, "services"))
	for _, name := range svcNames {
		if name == "" {
			continue
		}
		entry := nodeAt(root, "services", name)
		entryKeys, _ := mapKeys(entry)
		for _, k := range entryKeys {
			if k == "" {
				continue
			}
			if !inList(k, lintServiceEntry) {
				ui.Errorf(l.ErrOut, "  services.yaml: unknown key 'services.%s.%s' — allowed: %s", name, k, allowedHint(lintServiceEntry))
				errs++
			}
		}

		// image XOR registry (a full pin bypasses registry config).
		hasImage := inList("image", entryKeys)
		hasRegistry := inList("registry", entryKeys)
		if hasImage && hasRegistry {
			ui.Errorf(l.ErrOut, "  services.yaml: services.%s: 'image' and 'registry' are mutually exclusive", name)
			errs++
		}

		// per-service registry sub-keys.
		if hasRegistry {
			sregKeys, _ := mapKeys(nodeAt(entry, "registry"))
			for _, k := range sregKeys {
				if k == "" {
					continue
				}
				if !inList(k, lintServiceEntryRegistry) {
					ui.Errorf(l.ErrOut, "  services.yaml: unknown key 'services.%s.registry.%s' — allowed: %s", name, k, allowedHint(lintServiceEntryRegistry))
					errs++
				}
			}
		}

		// Resolve path: and validate the per-service lok8s.yaml.
		spath := servicePath(root, name)
		lok8sFile := l.Paths.Base + "/" + strings.TrimPrefix(spath, "./") + "/lok8s.yaml"
		if isFile(lok8sFile) {
			errs += l.lok8sYAML(name, lok8sFile)
		}
	}

	if errs == 0 {
		fmt.Fprintln(l.Out, "  OK")
		return true
	}
	ui.Errorf(l.ErrOut, "  %d services.yaml/lok8s.yaml validation error(s)", errs)
	return false
}

// servicePath resolves a service's path: (bash: yq -r '.services."<n>".path
// // "./<n>"', with the extra empty/"null" belt-and-braces re-default).
func servicePath(root *yaml.Node, name string) string {
	spath := valueOr(nodeAt(root, "services", name, "path"), "./"+name)
	if spath == "" || spath == "null" {
		spath = "./" + name
	}
	return spath
}

// lok8sYAML validates one lok8s.yaml against the bare schema (bash:
// lint::lok8s_yaml). `build` is required UNLESS `components` is present (the
// two are mutually exclusive). When `components` is present, each entry needs
// `name` + `build`. Returns the number of errors found.
func (l *Linter) lok8sYAML(name, file string) int {
	errs := 0
	root := firstDoc(file)

	keys, _ := mapKeys(root)
	for _, k := range keys {
		if k == "" {
			continue
		}
		if !inList(k, lintLok8sTop) {
			ui.Errorf(l.ErrOut, "  lok8s.yaml (%s): unknown key '%s' — allowed: %s", name, k, allowedHint(lintLok8sTop))
			errs++
		}
	}

	hasBuild := inList("build", keys)
	hasComponents := inList("components", keys)

	// build and components are mutually exclusive; exactly one must be present.
	if hasBuild && hasComponents {
		ui.Errorf(l.ErrOut, "  lok8s.yaml (%s): 'build' and 'components' are mutually exclusive", name)
		errs++
	} else if !hasBuild && !hasComponents {
		ui.Errorf(l.ErrOut, "  lok8s.yaml (%s): 'build' is required (or use 'components')", name)
		errs++
	}

	// Validate each components[] entry: name + build required.
	if hasComponents {
		comps := nodeAt(root, "components")
		// mikefarah yq emits '!!seq' for lists; the bash tolerates a bare
		// 'seq' too — moot here, the tag reader always yields the !! form.
		if normTag(comps) != "!!seq" {
			ui.Errorf(l.ErrOut, "  lok8s.yaml (%s): 'components' must be a list", name)
			errs++
		} else {
			for i, comp := range seqItems(comps) {
				cname := valueOr(nodeAt(comp, "name"), "")
				if cname == "" || cname == "null" {
					ui.Errorf(l.ErrOut, "  lok8s.yaml (%s): components[%d] is missing required 'name'", name, i)
					errs++
					cname = fmt.Sprintf("[%d]", i)
				}
				// build required on each component.
				cbuild := valueOr(nodeAt(comp, "build"), "")
				if cbuild == "" || cbuild == "null" {
					ui.Errorf(l.ErrOut, "  lok8s.yaml (%s): components[%s] is missing required 'build'", name, cname)
					errs++
				}
				// component entry keys.
				ckeys, _ := mapKeys(comp)
				for _, k := range ckeys {
					if k == "" {
						continue
					}
					if !inList(k, lintLok8sComponent) {
						ui.Errorf(l.ErrOut, "  lok8s.yaml (%s): components[%s].%s unknown — allowed: %s", name, cname, k, allowedHint(lintLok8sComponent))
						errs++
					}
				}
			}
		}
	}

	return errs
}

var driftTiltRe = regexp.MustCompile(`(docker_build|k8s_yaml)\(`)

// drift emits the drift warnings — never hard-fails (bash: lint::drift):
//
//	(a) project-root Tiltfile hand-rolls docker_build/k8s_yaml while
//	    services.yaml declares services → the catalog is dead/diverged.
//	    Suggest the 2-line form.
//	(b) a per-service path: contains its own Tiltfile (redundant — lok8s()
//	    reads each lok8s.yaml from its path).
//	(c) a service's deploy manifests carry no lok8s.dev/name=<service> label
//	    (lok8s() routes on that label and silently drops unmatched resources).
func (l *Linter) drift() {
	svcFile := l.servicesFile()
	if svcFile == "" {
		return
	}
	root := firstDoc(svcFile)

	// Does services.yaml actually declare any services?
	svcNames, _ := mapKeys(nodeAt(root, "services"))
	svcCount := len(svcNames)

	// (a) root Tiltfile hardcodes builds/manifests alongside a populated catalog.
	tiltfile := l.Paths.Base + "/Tiltfile"
	if svcCount > 0 && isFile(tiltfile) {
		if raw, err := os.ReadFile(tiltfile); err == nil && driftTiltRe.Match(raw) {
			ui.Warnf(l.ErrOut, "  Tiltfile: contains docker_build()/k8s_yaml() while services.yaml declares %d service(s) — prefer the 2-line form: load('./.lok8s/tilt/Tiltfile','lok8s'); lok8s()", svcCount)
		}
	}

	// Per-service drift: redundant Tiltfile + missing routing label.
	for _, name := range svcNames {
		if name == "" {
			continue
		}
		spath := servicePath(root, name)
		sdir := l.Paths.Base + "/" + strings.TrimPrefix(spath, "./")

		// (b) redundant per-submodule Tiltfile.
		if isFile(sdir + "/Tiltfile") {
			ui.Warnf(l.ErrOut, "  %s: %s/Tiltfile is redundant — lok8s() reads %s/lok8s.yaml directly (remove the per-service Tiltfile)", name, spath, spath)
		}

		// (c) deploy manifests missing the lok8s.dev/name routing label.
		// Best-effort: scan the service's deploy/ dir (if present). For a
		// `components` service each image is routed by its COMPONENT name
		// (one docker_build each), NOT the service name — so check every
		// component's label.
		ddir := sdir + "/deploy"
		if !isDir(ddir) {
			continue
		}
		routeNames := []string{name}
		lokfile := sdir + "/lok8s.yaml"
		if isFile(lokfile) {
			comps := seqItems(nodeAt(firstDoc(lokfile), "components"))
			if len(comps) > 0 {
				routeNames = routeNames[:0]
				for _, comp := range comps {
					// yq -r '.components[].name' prints "null" for a missing
					// name — that string then drives the (futile) label grep,
					// exactly like bash.
					routeNames = append(routeNames, scalarText(nodeAt(comp, "name")))
				}
			}
		}
		for _, rname := range routeNames {
			if rname == "" {
				continue
			}
			if !grepDirMatches(ddir, `lok8s\.dev/name:[[:space:]]*`+rname+`([[:space:]]|$|")`) {
				ui.Warnf(l.ErrOut, "  %s: no 'lok8s.dev/name: %s' label found in %s/deploy — lok8s() will silently drop these manifests", name, rname, spath)
			}
		}
	}
}

// grepDirMatches mirrors `grep -rqsE <pattern> <dir>`: any line of any
// regular file under dir (recursively) matching the pattern. Errors are
// silent (-s); the service name is interpolated into the pattern verbatim,
// exactly like the bash (a dot in the name matches any char in both).
func grepDirMatches(dir, pattern string) bool {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return false
	}
	found := false
	walk(dir, func(path string) bool {
		raw, err := os.ReadFile(path)
		if err != nil {
			return true
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if re.MatchString(line) {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

// walk visits every regular file under dir; fn returns false to stop.
func walk(dir string, fn func(path string) bool) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return true
	}
	for _, e := range entries {
		path := dir + "/" + e.Name()
		if e.IsDir() {
			if !walk(path, fn) {
				return false
			}
			continue
		}
		if !fn(path) {
			return false
		}
	}
	return true
}
