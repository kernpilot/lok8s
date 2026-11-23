package execx

// The scan in this file is a second net beside the behavior tests of each
// package, where a fake Runner records the Env of a kubectl child. It reads
// the Go source of internal/ and cmd/, finds each kubectl child that lo
// starts and reads its subcommand. It fails when a child that writes runs
// with the user kuberc: a kuberc default such as --dry-run=client makes a
// write report success and change nothing. The bash twin of this scan is
// tests/unit/kuberc_test.bats.
//
// A child is "off" (it runs with the user kuberc off) when:
//   - the Env of its Cmd literal names KubeRCOff or KubectlEnv;
//   - the function passes the literal, or a variable that holds it, to a
//     function that sets the Env of the Cmd it takes to KubeRCOff
//     (kapply.RunPrivate), or sets <variable>.Env itself;
//   - it runs through a wrapper: a function whose Cmd takes the tool name,
//     the arguments or the Env from a parameter. The wrapper is off when its
//     Cmd is off, or, for an Env parameter, when the caller passes a value
//     that names KubeRCOff. The scan follows a wrapper to its callers, also
//     over more than one wrapper.
//
// The scan reads the arguments of a call from its string literals, and a
// variable from its assignments in the same function or in the package. A
// list that it cannot read (a parameter, the result of a call, a list that
// a loop fills) counts as global flags, so the next literal is the
// subcommand; the kapply helpers put the global flags of the caller first.
// A one-word variable with one literal value is that value. A list with
// one value per branch (a switch) has a subcommand only when all its
// values agree. The rule fails on a child whose subcommand the scan cannot
// read, unless it runs with the user kuberc off.
//
// It does not see a kubectl that runs through a method of another package
// on a field (it knows the functions of the same package and of an
// imported package name), a tool name that comes from a variable other
// than one named kubectl, or arguments that a function returns. A closure
// is not a wrapper: its Cmd is one site. It finds a method by its name in
// the package, so two types with a wrapper of the same name count as one
// wrapper.

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// kubectlWriteVerbs are the subcommands that change an object, and diff,
// which a kuberc default can make print the data of a Secret.
var kubectlWriteVerbs = map[string]bool{
	"apply": true, "create": true, "replace": true, "patch": true, "delete": true, "annotate": true,
	"label": true, "scale": true, "set": true, "taint": true, "cordon": true, "uncordon": true,
	"drain": true, "edit": true, "expose": true, "run": true, "autoscale": true, "diff": true,
}

// kubectlValueFlags are the global flags that take the next word as their
// value, so that word is not the subcommand.
var kubectlValueFlags = map[string]bool{
	"--kubeconfig": true, "--context": true, "-n": true, "--namespace": true, "--server": true, "-s": true,
	"--request-timeout": true, "--tls-server-name": true, "--cluster": true, "--user": true, "--as": true,
	"--token": true,
}

// kubectlSite is one kubectl child.
type kubectlSite struct {
	file  string // relative to the root, with slashes
	line  int
	verb  string // the subcommand; "?" when the scan cannot read it
	known bool
	write bool
	off   bool
	via   string // "Cmd" for a Cmd literal, else the wrapper
}

// key is the state of a site without its place: "off write delete (Cmd)".
func (s kubectlSite) key() string {
	state := "on"
	if s.off {
		state = "off"
	}
	kind := "read"
	switch {
	case !s.known:
		kind = "?"
	case s.write:
		kind = "write"
	}
	return fmt.Sprintf("%s %s %s (%s)", state, kind, s.verb, s.via)
}

func (s kubectlSite) String() string {
	return fmt.Sprintf("%s:%d: %s", s.file, s.line, s.key())
}

// kubectlRuleViolations returns the sites that run with the user kuberc and
// that write, or whose subcommand the scan cannot read.
func kubectlRuleViolations(sites []kubectlSite) []kubectlSite {
	var bad []kubectlSite
	for _, s := range sites {
		if !s.off && (s.write || !s.known) {
			bad = append(bad, s)
		}
	}
	return bad
}

// word is one argument: a literal (ok), a list that the scan cannot read
// (list), or a word that it cannot read (neither).
type word struct {
	lit  string
	ok   bool
	list bool
}

// classifyKubectl reads the subcommand of the arguments of a kubectl
// child: the first word that is not a flag, the value of a global flag or
// a list that the scan cannot read.
func classifyKubectl(ws []word) (verb string, known, write bool) {
	for i := 0; i < len(ws); i++ {
		w := ws[i]
		if w.list {
			continue
		}
		if !w.ok {
			return "?", false, false
		}
		if strings.HasPrefix(w.lit, "-") {
			if kubectlValueFlags[w.lit] {
				i++
			}
			continue
		}
		sub := ""
		if i+1 < len(ws) && ws[i+1].ok {
			sub = ws[i+1].lit
		}
		switch w.lit {
		case "rollout":
			return "rollout " + sub, true, sub == "restart" || sub == "undo" || sub == "pause" || sub == "resume"
		case "certificate":
			return "certificate " + sub, true, sub == "approve" || sub == "deny"
		case "create":
			// create --dry-run=client -o yaml renders a manifest on the
			// client: it writes nothing.
			for _, r := range ws[i+1:] {
				if r.ok && r.lit == "--dry-run=client" {
					return "create (render)", true, false
				}
			}
		}
		return w.lit, true, kubectlWriteVerbs[w.lit]
	}
	return "?", false, false
}

// scanPkg is one Go package of the tree.
type scanPkg struct {
	funcs map[string][]*ast.FuncDecl
	vars  map[string]ast.Expr
	files map[*ast.FuncDecl]*ast.File
}

// runner is a function that starts or builds the Cmd of a tool.
type runner struct {
	nameIdx  int    // the parameter of the tool name; -1: the tool is kubectl
	argsIdx  int    // the parameter of the arguments
	prefix   []word // the words that the runner puts before the arguments
	suffix   []word // the words that the runner puts after the arguments
	envIdx   int    // the parameter that is the Env of the Cmd; -1: none
	variadic bool
	off      bool
}

type kubectlScanner struct {
	fset    *token.FileSet
	root    string
	pkgs    map[string]*scanPkg // by import path
	runners map[*ast.FuncDecl]runner
	setters map[*ast.FuncDecl]int // a function that sets <param>.Env to KubeRCOff: the parameter
	sites   []kubectlSite
}

// scanKubectlSites parses the non-test Go files under root/internal and
// root/cmd and returns each kubectl child.
func scanKubectlSites(t *testing.T, root string) []kubectlSite {
	t.Helper()
	module := ""
	if f, err := os.Open(filepath.Join(root, "go.mod")); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if m, ok := strings.CutPrefix(sc.Text(), "module "); ok {
				module = strings.TrimSpace(m)
				break
			}
		}
		_ = f.Close()
	}
	if module == "" {
		t.Fatalf("no module line in %s", filepath.Join(root, "go.mod"))
	}
	s := &kubectlScanner{fset: token.NewFileSet(), root: root, pkgs: map[string]*scanPkg{},
		runners: map[*ast.FuncDecl]runner{}, setters: map[*ast.FuncDecl]int{}}
	for _, top := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" || filepath.ToSlash(rel) == "internal/assets/lok8s" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(s.fset, p, nil, 0)
			if err != nil {
				return err
			}
			path := module + "/" + filepath.ToSlash(filepath.Dir(rel))
			pkg := s.pkgs[path]
			if pkg == nil {
				pkg = &scanPkg{funcs: map[string][]*ast.FuncDecl{}, vars: map[string]ast.Expr{},
					files: map[*ast.FuncDecl]*ast.File{}}
				s.pkgs[path] = pkg
			}
			for _, decl := range f.Decls {
				switch d := decl.(type) {
				case *ast.FuncDecl:
					pkg.funcs[d.Name.Name] = append(pkg.funcs[d.Name.Name], d)
					pkg.files[d] = f
				case *ast.GenDecl:
					for _, spec := range d.Specs {
						if vs, ok := spec.(*ast.ValueSpec); ok {
							for i, n := range vs.Names {
								if i < len(vs.Values) {
									pkg.vars[n.Name] = vs.Values[i]
								}
							}
						}
					}
				}
			}
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("scan %s: %v", top, err)
		}
	}
	s.findRunners()
	s.findSites()
	sort.Slice(s.sites, func(i, j int) bool {
		a, b := s.sites[i], s.sites[j]
		if a.file != b.file {
			return a.file < b.file
		}
		return a.line < b.line
	})
	return s.sites
}

// params returns the parameter names of a function, and whether the last
// one is variadic.
func params(fn *ast.FuncDecl) ([]string, bool) {
	var names []string
	variadic := false
	for _, f := range fn.Type.Params.List {
		_, variadic = f.Type.(*ast.Ellipsis)
		if len(f.Names) == 0 {
			names = append(names, "_")
		}
		for _, n := range f.Names {
			names = append(names, n.Name)
		}
	}
	return names, variadic
}

func paramIdx(fn *ast.FuncDecl, e ast.Expr) int {
	id, ok := e.(*ast.Ident)
	if !ok {
		return -1
	}
	names, _ := params(fn)
	for i, n := range names {
		if n == id.Name {
			return i
		}
	}
	return -1
}

// isCmdLit reports whether e is an execx.Cmd literal (Cmd in this package).
func isCmdLit(e ast.Expr) (*ast.CompositeLit, bool) {
	cl, ok := e.(*ast.CompositeLit)
	if !ok {
		return nil, false
	}
	switch ty := cl.Type.(type) {
	case *ast.SelectorExpr:
		x, ok := ty.X.(*ast.Ident)
		return cl, ok && x.Name == "execx" && ty.Sel.Name == "Cmd"
	case *ast.Ident:
		return cl, ty.Name == "Cmd"
	}
	return nil, false
}

func field(cl *ast.CompositeLit, key string) ast.Expr {
	for _, e := range cl.Elts {
		if kv, ok := e.(*ast.KeyValueExpr); ok {
			if k, ok := kv.Key.(*ast.Ident); ok && k.Name == key {
				return kv.Value
			}
		}
	}
	return nil
}

// isKubectlName reports whether e is the tool name kubectl: the literal,
// or a variable named kubectl (a path that a lookup returned).
func isKubectlName(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.BasicLit:
		v, err := strconv.Unquote(x.Value)
		return err == nil && v == "kubectl"
	case *ast.Ident:
		return x.Name == "kubectl"
	}
	return false
}

// mentionsOff reports whether e names KubeRCOff or KubectlEnv, also
// through a variable of the function or of the package.
func (s *kubectlScanner) mentionsOff(pkg *scanPkg, fn *ast.FuncDecl, e ast.Expr, depth int) bool {
	if e == nil || depth > 4 {
		return false
	}
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && (id.Name == "KubeRCOff" || id.Name == "KubectlEnv") {
			found = true
		}
		return !found
	})
	if found {
		return true
	}
	if id, ok := e.(*ast.Ident); ok {
		for _, v := range s.assigned(pkg, fn, id.Name) {
			if s.mentionsOff(pkg, fn, v, depth+1) {
				return true
			}
		}
	}
	return false
}

// assigned returns the values that a function (or else the package)
// assigns to a name, in source order.
func (s *kubectlScanner) assigned(pkg *scanPkg, fn *ast.FuncDecl, name string) []ast.Expr {
	var out []ast.Expr
	if fn != nil && fn.Body != nil {
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch st := n.(type) {
			case *ast.AssignStmt:
				if len(st.Lhs) == len(st.Rhs) {
					for i, l := range st.Lhs {
						if id, ok := l.(*ast.Ident); ok && id.Name == name {
							out = append(out, st.Rhs[i])
						}
					}
				}
			case *ast.ValueSpec:
				for i, id := range st.Names {
					if id.Name == name && i < len(st.Values) {
						out = append(out, st.Values[i])
					}
				}
			}
			return true
		})
	}
	if len(out) == 0 {
		if v, ok := pkg.vars[name]; ok {
			out = append(out, v)
		}
	}
	return out
}

func strLit(e ast.Expr) (string, bool) {
	if bl, ok := e.(*ast.BasicLit); ok && bl.Kind == token.STRING {
		if v, err := strconv.Unquote(bl.Value); err == nil {
			return v, true
		}
	}
	return "", false
}

// scalar reads one word: a literal, the literal start of a + (a flag with
// a value), or a variable with one literal value.
func (s *kubectlScanner) scalar(pkg *scanPkg, fn *ast.FuncDecl, e ast.Expr) word {
	if v, ok := strLit(e); ok {
		return word{lit: v, ok: true}
	}
	switch x := e.(type) {
	case *ast.BinaryExpr:
		if v, ok := strLit(x.X); ok && x.Op == token.ADD {
			return word{lit: v, ok: true}
		}
	case *ast.Ident:
		if fn != nil && paramIdx(fn, x) >= 0 {
			return word{}
		}
		vals := s.assigned(pkg, fn, x.Name)
		if len(vals) == 0 {
			return word{}
		}
		first, ok := strLit(vals[0])
		for _, v := range vals[1:] {
			if o, isLit := strLit(v); !isLit || o != first {
				ok = false
			}
		}
		if ok {
			return word{lit: first, ok: true}
		}
	}
	return word{}
}

// isSelfAppend reports whether e is append(name, …): a value that adds to
// the value before it.
func isSelfAppend(e ast.Expr, name string) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok || len(call.Args) == 0 {
		return false
	}
	f, ok := call.Fun.(*ast.Ident)
	id, isIdent := call.Args[0].(*ast.Ident)
	return ok && f.Name == "append" && isIdent && id.Name == name
}

// words reads a list of words: a slice literal, append, slices.Clone and
// variables. self is the variable being read: append(self, …) adds to what
// came before, and a word of it that the scan cannot read is a list (a loop
// can add it more than once). A variable with more than one value that does
// not add to the one before it is a list: the scan does not follow the flow.
func (s *kubectlScanner) words(pkg *scanPkg, fn *ast.FuncDecl, e ast.Expr, self string, depth int) []word {
	if depth > 6 {
		return []word{{list: true}}
	}
	switch x := e.(type) {
	case *ast.CompositeLit:
		var out []word
		for _, el := range x.Elts {
			out = append(out, s.scalar(pkg, fn, el))
		}
		return out
	case *ast.CallExpr:
		switch f := x.Fun.(type) {
		case *ast.Ident:
			if f.Name == "append" && len(x.Args) > 0 {
				acc := isSelfAppend(x, self) && self != ""
				var out []word
				if !acc {
					out = append(out, s.words(pkg, fn, x.Args[0], self, depth+1)...)
				}
				for i, a := range x.Args[1:] {
					if i == len(x.Args)-2 && x.Ellipsis.IsValid() {
						out = append(out, s.words(pkg, fn, a, self, depth+1)...)
						continue
					}
					w := s.scalar(pkg, fn, a)
					if acc && !w.ok {
						w.list = true
					}
					out = append(out, w)
				}
				return out
			}
		case *ast.SelectorExpr:
			if p, ok := f.X.(*ast.Ident); ok && p.Name == "slices" && f.Sel.Name == "Clone" && len(x.Args) == 1 {
				return s.words(pkg, fn, x.Args[0], self, depth+1)
			}
		}
	case *ast.Ident:
		if x.Name == self || (fn != nil && paramIdx(fn, x) >= 0) {
			return []word{{list: true}}
		}
		vals := s.assigned(pkg, fn, x.Name)
		var out []word
		bases, verb := 0, ""
		for _, v := range vals {
			ws := s.words(pkg, fn, v, x.Name, depth+1)
			if isSelfAppend(v, x.Name) {
				out = append(out, ws...)
				continue
			}
			// Each value that does not add to the one before it (one per
			// branch of a switch) must hold the same subcommand.
			v, known, write := classifyKubectl(ws)
			key := fmt.Sprintf("%s/%t", v, write)
			if bases++; bases > 1 && (!known || key != verb) {
				return []word{{list: true}}
			}
			if bases == 1 {
				verb = key
				if !known {
					verb = "?"
				}
				out = append(out, ws...)
			}
		}
		if bases == 0 || (bases > 1 && verb == "?") {
			return []word{{list: true}}
		}
		return out
	}
	return []word{{list: true}}
}

// scalars reads each expression as one word.
func (s *kubectlScanner) scalars(pkg *scanPkg, fn *ast.FuncDecl, es []ast.Expr) []word {
	out := []word{}
	for _, e := range es {
		out = append(out, s.scalar(pkg, fn, e))
	}
	return out
}

// argsParam finds the parameter of fn that e (the Args of a Cmd, or the
// arguments a function passes on) takes its arguments from, and the words
// before and after it: a parameter, a variable that holds one, append with
// the parameter first or spread last, and slices.Clone.
func (s *kubectlScanner) argsParam(pkg *scanPkg, fn *ast.FuncDecl, e ast.Expr, depth int) (idx int, pre, suf []word) {
	if e == nil || depth > 4 {
		return -1, nil, nil
	}
	if idx := paramIdx(fn, e); idx >= 0 {
		return idx, nil, nil
	}
	switch x := e.(type) {
	case *ast.Ident:
		for _, v := range s.assigned(pkg, fn, x.Name) {
			if idx, pre, suf := s.argsParam(pkg, fn, v, depth+1); idx >= 0 {
				return idx, pre, suf
			}
		}
	case *ast.CallExpr:
		switch f := x.Fun.(type) {
		case *ast.Ident:
			if f.Name != "append" || len(x.Args) == 0 {
				return -1, nil, nil
			}
			n := len(x.Args)
			if idx, pre, suf := s.argsParam(pkg, fn, x.Args[0], depth+1); idx >= 0 {
				rest := x.Args[1:]
				if x.Ellipsis.IsValid() && n > 1 {
					suf = append(suf, s.scalars(pkg, fn, x.Args[1:n-1])...)
					return idx, pre, append(suf, s.words(pkg, fn, x.Args[n-1], "", 0)...)
				}
				return idx, pre, append(suf, s.scalars(pkg, fn, rest)...)
			}
			if !x.Ellipsis.IsValid() || n < 2 {
				return -1, nil, nil
			}
			if idx, pre, suf := s.argsParam(pkg, fn, x.Args[n-1], depth+1); idx >= 0 {
				before := s.words(pkg, fn, x.Args[0], "", 0)
				before = append(before, s.scalars(pkg, fn, x.Args[1:n-1])...)
				return idx, append(before, pre...), suf
			}
		case *ast.SelectorExpr:
			if p, ok := f.X.(*ast.Ident); ok && p.Name == "slices" && f.Sel.Name == "Clone" && len(x.Args) == 1 {
				return s.argsParam(pkg, fn, x.Args[0], depth+1)
			}
		}
	}
	return -1, nil, nil
}

// passOn reads a call of the runner r in fn. It returns the words of the
// call with the words of the runner around them, and the parameter of fn
// that the call takes its arguments from (-1: none), with the words before
// and after that parameter.
func (s *kubectlScanner) passOn(pkg *scanPkg, fn *ast.FuncDecl, r runner, call *ast.CallExpr) (ws []word, idx int, pre, suf []word) {
	idx = -1
	var mid []word
	n := len(call.Args)
	switch {
	case r.variadic:
		for i := r.argsIdx; i < n; i++ {
			if i < n-1 || !call.Ellipsis.IsValid() {
				mid = append(mid, s.scalar(pkg, fn, call.Args[i]))
				continue
			}
			mid = append(mid, s.words(pkg, fn, call.Args[i], "", 0)...)
			if j, p, sf := s.argsParam(pkg, fn, call.Args[i], 0); j >= 0 {
				idx, pre, suf = j, append(s.scalars(pkg, fn, call.Args[r.argsIdx:i]), p...), sf
			}
		}
	case r.argsIdx < n:
		mid = s.words(pkg, fn, call.Args[r.argsIdx], "", 0)
		idx, pre, suf = s.argsParam(pkg, fn, call.Args[r.argsIdx], 0)
	}
	ws = append(append(append([]word{}, r.prefix...), mid...), r.suffix...)
	if idx >= 0 {
		pre = append(append([]word{}, r.prefix...), pre...)
		suf = append(append([]word{}, suf...), r.suffix...)
	}
	return ws, idx, pre, suf
}

// callee resolves the function of a call: a function or method of the
// same package by name, or a function of an imported package.
func (s *kubectlScanner) callee(pkg *scanPkg, fn *ast.FuncDecl, call *ast.CallExpr) []*ast.FuncDecl {
	switch f := call.Fun.(type) {
	case *ast.Ident:
		return pkg.funcs[f.Name]
	case *ast.SelectorExpr:
		if x, ok := f.X.(*ast.Ident); ok {
			for _, imp := range pkg.files[fn].Imports {
				p, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					continue
				}
				name := p[strings.LastIndex(p, "/")+1:]
				if imp.Name != nil {
					name = imp.Name.Name
				}
				if name == x.Name {
					if other := s.pkgs[p]; other != nil {
						return other.funcs[f.Sel.Name]
					}
					return nil
				}
			}
		}
		return pkg.funcs[f.Sel.Name]
	}
	return nil
}

func (s *kubectlScanner) eachFunc(do func(pkg *scanPkg, fn *ast.FuncDecl)) {
	paths := make([]string, 0, len(s.pkgs))
	for p := range s.pkgs {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		pkg := s.pkgs[p]
		names := make([]string, 0, len(pkg.funcs))
		for n := range pkg.funcs {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			for _, fn := range pkg.funcs[n] {
				if fn.Body != nil {
					do(pkg, fn)
				}
			}
		}
	}
}

// litOff reports whether the child of a Cmd literal runs with KubeRCOff:
// its Env names it, the function passes the literal (or a variable that
// holds it) to a function that sets the Env, or it sets <variable>.Env.
func (s *kubectlScanner) litOff(pkg *scanPkg, fn *ast.FuncDecl, cl *ast.CompositeLit) bool {
	if s.mentionsOff(pkg, fn, field(cl, "Env"), 0) {
		return true
	}
	holds := func(e ast.Expr) bool {
		if e == ast.Expr(cl) {
			return true
		}
		id, ok := e.(*ast.Ident)
		if !ok {
			return false
		}
		for _, v := range s.assigned(pkg, fn, id.Name) {
			if v == ast.Expr(cl) {
				return true
			}
		}
		return false
	}
	off := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch st := n.(type) {
		case *ast.CallExpr:
			for _, c := range s.callee(pkg, fn, st) {
				if idx, ok := s.setters[c]; ok && idx < len(st.Args) && holds(st.Args[idx]) {
					off = true
				}
			}
		case *ast.AssignStmt:
			if len(st.Lhs) == len(st.Rhs) {
				for i, l := range st.Lhs {
					if sel, ok := l.(*ast.SelectorExpr); ok && sel.Sel.Name == "Env" && holds(sel.X) &&
						s.mentionsOff(pkg, fn, st.Rhs[i], 0) {
						off = true
					}
				}
			}
		}
		return !off
	})
	return off
}

// findRunners finds the functions that set <param>.Env to KubeRCOff, the
// functions whose Cmd takes the tool name, the arguments or the Env from a
// parameter, and then the functions that pass their parameters on to one.
func (s *kubectlScanner) findRunners() {
	s.eachFunc(func(pkg *scanPkg, fn *ast.FuncDecl) {
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if st, ok := n.(*ast.AssignStmt); ok && len(st.Lhs) == len(st.Rhs) {
				for i, l := range st.Lhs {
					if sel, ok := l.(*ast.SelectorExpr); ok && sel.Sel.Name == "Env" {
						if idx := paramIdx(fn, sel.X); idx >= 0 && s.mentionsOff(pkg, fn, st.Rhs[i], 0) {
							s.setters[fn] = idx
						}
					}
				}
			}
			return true
		})
	})
	s.eachFunc(func(pkg *scanPkg, fn *ast.FuncDecl) {
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			e, isExpr := n.(ast.Expr)
			if !isExpr {
				return true
			}
			cl, ok := isCmdLit(e)
			if !ok {
				return true
			}
			r := runner{nameIdx: -2, envIdx: paramIdx(fn, field(cl, "Env"))}
			r.argsIdx, r.prefix, r.suffix = s.argsParam(pkg, fn, field(cl, "Args"), 0)
			if r.argsIdx < 0 {
				return true
			}
			if idx := paramIdx(fn, field(cl, "Name")); idx >= 0 {
				r.nameIdx = idx
			} else if isKubectlName(field(cl, "Name")) {
				if s.litIsSite(pkg, fn, cl) {
					return true // the parameter holds global flags only
				}
				r.nameIdx = -1
			} else {
				return true
			}
			names, variadic := params(fn)
			r.variadic = variadic && r.argsIdx == len(names)-1
			r.off = s.litOff(pkg, fn, cl)
			s.runners[fn] = r
			return true
		})
	})
	for changed := true; changed; {
		changed = false
		s.eachFunc(func(pkg *scanPkg, fn *ast.FuncDecl) {
			if _, done := s.runners[fn]; done {
				return
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				for _, c := range s.callee(pkg, fn, call) {
					r, ok := s.runners[c]
					if !ok || r.argsIdx >= len(call.Args) {
						continue
					}
					nr := runner{nameIdx: -2, envIdx: -1, off: r.off}
					var ws []word
					ws, nr.argsIdx, nr.prefix, nr.suffix = s.passOn(pkg, fn, r, call)
					if nr.argsIdx < 0 {
						continue
					}
					if r.nameIdx == -1 {
						nr.nameIdx = -1
					} else if r.nameIdx < len(call.Args) {
						if idx := paramIdx(fn, call.Args[r.nameIdx]); idx >= 0 {
							nr.nameIdx = idx
						} else if isKubectlName(call.Args[r.nameIdx]) {
							nr.nameIdx = -1
						}
					}
					if nr.nameIdx == -2 {
						continue
					}
					if _, known, _ := classifyKubectl(ws); nr.nameIdx == -1 && known {
						continue // a site: the parameter holds global flags only
					}
					if r.envIdx >= 0 && r.envIdx < len(call.Args) {
						nr.envIdx = paramIdx(fn, call.Args[r.envIdx])
						nr.off = s.mentionsOff(pkg, fn, call.Args[r.envIdx], 0)
					}
					names, variadic := params(fn)
					nr.variadic = variadic && nr.argsIdx == len(names)-1
					s.runners[fn] = nr
					changed = true
					return false
				}
				return true
			})
		})
	}
}

// litIsSite reports whether a Cmd literal with the name kubectl is a site:
// its arguments do not come from a parameter, or the scan reads the
// subcommand from its own words (the parameter holds global flags only).
func (s *kubectlScanner) litIsSite(pkg *scanPkg, fn *ast.FuncDecl, cl *ast.CompositeLit) bool {
	if idx, _, _ := s.argsParam(pkg, fn, field(cl, "Args"), 0); idx < 0 {
		return true
	}
	_, known, _ := classifyKubectl(s.words(pkg, fn, field(cl, "Args"), "", 0))
	return known
}

// findSites lists each kubectl child: a Cmd literal with the name kubectl
// that is not a wrapper (litIsSite), and each call of a wrapper with the
// name kubectl (or of a wrapper of kubectl), unless the calling function is
// a wrapper itself: it passes its own tool name on, or its own arguments
// with no subcommand that the scan can read.
func (s *kubectlScanner) findSites() {
	s.eachFunc(func(pkg *scanPkg, fn *ast.FuncDecl) {
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if e, ok := n.(ast.Expr); ok {
				if cl, ok := isCmdLit(e); ok && isKubectlName(field(cl, "Name")) && paramIdx(fn, field(cl, "Name")) < 0 &&
					s.litIsSite(pkg, fn, cl) {
					var ws []word
					if a := field(cl, "Args"); a != nil {
						ws = s.words(pkg, fn, a, "", 0)
					}
					s.add(cl.Pos(), ws, s.litOff(pkg, fn, cl), "Cmd")
				}
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			for _, c := range s.callee(pkg, fn, call) {
				r, ok := s.runners[c]
				if !ok {
					continue
				}
				if r.nameIdx >= 0 && (r.nameIdx >= len(call.Args) || !isKubectlName(call.Args[r.nameIdx])) {
					continue
				}
				if r.nameIdx >= 0 && paramIdx(fn, call.Args[r.nameIdx]) >= 0 {
					continue // fn passes its own tool name on: fn is the wrapper
				}
				ws, idx, _, _ := s.passOn(pkg, fn, r, call)
				if _, known, _ := classifyKubectl(ws); idx >= 0 && !known {
					continue // fn passes its own arguments on: fn is the wrapper
				}
				off := r.off
				if r.envIdx >= 0 && r.envIdx < len(call.Args) {
					off = s.mentionsOff(pkg, fn, call.Args[r.envIdx], 0)
				}
				s.add(call.Pos(), ws, off, c.Name.Name)
				break
			}
			return true
		})
	})
}

func (s *kubectlScanner) add(pos token.Pos, ws []word, off bool, via string) {
	p := s.fset.Position(pos)
	rel, err := filepath.Rel(s.root, p.Filename)
	if err != nil {
		rel = p.Filename
	}
	verb, known, write := classifyKubectl(ws)
	s.sites = append(s.sites, kubectlSite{file: filepath.ToSlash(rel), line: p.Line,
		verb: verb, known: known, write: write, off: off, via: via})
}

// Every kubectl child of lo that writes runs with the user kuberc off, and
// so does every child whose subcommand the scan cannot read. A read keeps
// the kuberc of the user. Run with -v to see each site.
func TestEveryKubectlWriteRunsWithKubeRCOff(t *testing.T) {
	sites := scanKubectlSites(t, filepath.Join("..", ".."))
	for _, s := range sites {
		t.Log(s.String())
	}
	// Anti-vacuity: the scan reads the sites of each kind of call (a Cmd
	// literal, a wrapper with KubectlEnv, a wrapper with an Env parameter,
	// a Cmd that goes to kapply.RunPrivate, and a wrapper of kapply that
	// gets the global flags of its caller), and it sees a read as a read.
	found := map[string]bool{}
	for _, s := range sites {
		found[s.file+" "+s.key()] = true
	}
	for _, want := range []string{
		"internal/hooks/hooks.go off write delete (Cmd)",
		"internal/hooks/hooks.go off write rollout restart (Cmd)",
		"internal/hooks/hooks.go on read get (Cmd)",
		"internal/driver/capi/capi.go off write delete (Cmd)",
		"internal/driver/capi/capi.go on read create (render) (Cmd)",
		"internal/driver/lo/heal.go off write delete (cmdWith)",
		"internal/driver/lo/heal.go on read get (runQuiet)",
		"internal/driver/lo/services.go off write annotate (runOut)",
		"internal/kubehz/deploy.go off write delete (Cmd)",
		"internal/kubehz/deploy.go off write patch (run)",
		"internal/kubehz/deploy.go off write apply (applyPrivate)",
		"internal/kubehz/handover.go off write taint (Cmd)",
		"internal/kapply/apply.go off write apply (kubectl)",
		"internal/kapply/apply.go off write replace (kubectlQuiet)",
		"internal/kapply/preflight.go off write patch (kubectlQuiet)",
		"internal/kapply/preflight.go on read get (Cmd)",
		"internal/driver/capi/generate.go off write apply (Cmd)",
	} {
		if !found[want] {
			t.Errorf("the scan does not find %q", want)
		}
	}
	writes := 0
	for _, s := range sites {
		if s.write {
			writes++
		}
	}
	if len(sites) < 90 || writes < 45 {
		t.Errorf("the scan finds %d sites and %d writes, want at least 90 and 45", len(sites), writes)
	}
	for _, s := range kubectlRuleViolations(sites) {
		if s.write {
			t.Errorf("a kubectl write runs with the user kuberc: %s", s)
		} else {
			t.Errorf("a kubectl child whose subcommand the scan cannot read runs with the user kuberc: %s", s)
		}
	}
}

// kubectlScanFixture is a package that holds each form of a kubectl child
// that the scan must read. A line with a site ends in "// want: <key>".
const kubectlScanFixture = `package a

import (
	"context"

	"example.com/m/internal/execx"
	"example.com/m/internal/k"
)

type ctxT struct{ r execx.Runner }

func (c *ctxT) runQuiet(ctx context.Context, name string, args ...string) error {
	return c.r.Run(ctx, execx.Cmd{Name: name, Args: args})
}

func (c *ctxT) run(ctx context.Context, name string, args ...string) error {
	return c.r.Run(ctx, execx.Cmd{Name: name, Args: args, Env: execx.KubectlEnv(name)})
}

func (c *ctxT) kubectl(ctx context.Context, args ...string) error {
	return c.runQuiet(ctx, "kubectl", args...)
}

func (c *ctxT) quiet(ctx context.Context, env []string, args ...string) error {
	return c.r.Run(ctx, execx.Cmd{Name: "kubectl", Args: args, Env: env})
}

func (c *ctxT) sites(ctx context.Context, x bool, in []string) {
	_ = c.r.Run(ctx, execx.Cmd{Name: "kubectl", Args: []string{"-n", "x", "delete", "pod", "p"}}) // want: on write delete (Cmd)
	_ = c.r.Run(ctx, execx.Cmd{Name: "kubectl", Args: []string{"patch", "cm", "x", "-p", "{}"}, Env: []string{execx.KubeRCOff}}) // want: off write patch (Cmd)
	_ = c.runQuiet(ctx, "kubectl", "annotate", "svc", "x", "a=b") // want: on write annotate (runQuiet)
	_ = c.run(ctx, "kubectl", "scale", "deploy/x", "--replicas=0") // want: off write scale (run)
	_ = c.runQuiet(ctx, "docker", "rm", "x")
	_ = k.RunPrivate(ctx, c.r, execx.Cmd{Name: "kubectl", Args: []string{"apply", "-f", "-"}}) // want: off write apply (Cmd)
	_ = c.runQuiet(ctx, "kubectl", "--kubeconfig", "/k", "get", "pods") // want: on read get (runQuiet)
	_ = c.run(ctx, "kubectl", "create", "secret", "generic", "s", "--dry-run=client", "-o", "yaml") // want: off read create (render) (run)
	_ = c.runQuiet(ctx, "kubectl", "--request-timeout="+"5s", "create", "job", "j", "--from=cronjob/c") // want: on write create (runQuiet)
	ns := "default"
	if x {
		ns = "kube-system"
	}
	_ = c.r.Run(ctx, execx.Cmd{Name: "kubectl", Args: []string{"-n", ns, "rollout", "restart", "deploy/x"}}) // want: on write rollout restart (Cmd)
	_ = c.r.Run(ctx, execx.Cmd{Name: "kubectl", Args: []string{"-n", ns, "rollout", "status", "deploy/x"}}) // want: on read rollout status (Cmd)
	var flags []string
	for _, f := range in {
		flags = append(flags, f)
	}
	label := append(append([]string{}, flags...), "label", "ns", "x", "a=b")
	_ = c.quiet(ctx, nil, label...) // want: on write label (quiet)
	_ = c.quiet(ctx, []string{execx.KubeRCOff}, label...) // want: off write label (quiet)
	_ = c.kubectl(ctx, "cordon", "n1") // want: on write cordon (kubectl)
	_ = c.r.Run(ctx, execx.Cmd{Name: "kubectl", Args: mk()}) // want: on ? ? (Cmd)
	cmd := execx.Cmd{Name: "kubectl", Args: []string{"taint", "nodes", "--all", "x-"}} // want: off write taint (Cmd)
	cmd.Env = []string{execx.KubeRCOff}
	_ = c.r.Run(ctx, cmd)
}

func mk() []string { return nil }
`

// kubectlScanFixtureK is a package with a function that sets the Env of
// the Cmd it takes, as kapply.RunPrivate does.
const kubectlScanFixtureK = `package k

import (
	"context"

	"example.com/m/internal/execx"
)

func RunPrivate(ctx context.Context, r execx.Runner, c execx.Cmd) error {
	c.Env = append(c.Env, execx.KubeRCOff)
	return r.Run(ctx, c)
}
`

// The scan on a tree of its own: it reads each planted site with its
// state, and the rule fails on exactly the planted sites that write (or
// that it cannot read) with the user kuberc.
func TestKubectlScanFindsPlantedSites(t *testing.T) {
	root := t.TempDir()
	for path, text := range map[string]string{
		"go.mod":               "module example.com/m\n\ngo 1.26\n",
		"internal/a/a.go":      kubectlScanFixture,
		"internal/k/k.go":      kubectlScanFixtureK,
		"internal/a/a_test.go": "package a\n\nfunc x() { _ = execx.Cmd{Name: \"kubectl\", Args: []string{\"delete\", \"x\"}} }\n",
	} {
		p := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want := map[int]string{}
	for i, l := range strings.Split(kubectlScanFixture, "\n") {
		if _, key, ok := strings.Cut(l, "// want: "); ok {
			want[i+1] = key
		}
	}
	if len(want) < 15 {
		t.Fatalf("the fixture has %d planted sites, want at least 15", len(want))
	}
	sites := scanKubectlSites(t, root)
	got := map[int]string{}
	for _, s := range sites {
		if s.file != "internal/a/a.go" {
			t.Errorf("a site outside the planted file (a test file or a setter): %s", s)
			continue
		}
		got[s.line] = s.key()
	}
	for line, key := range want {
		if got[line] != key {
			t.Errorf("internal/a/a.go:%d: the scan reads %q, want %q", line, got[line], key)
		}
	}
	for line, key := range got {
		if _, ok := want[line]; !ok {
			t.Errorf("internal/a/a.go:%d: the scan reads a site that is not planted: %q", line, key)
		}
	}
	wantBad := 0
	for _, key := range want {
		if strings.HasPrefix(key, "on write ") || strings.HasPrefix(key, "on ? ") {
			wantBad++
		}
	}
	if bad := kubectlRuleViolations(sites); len(bad) != wantBad {
		t.Errorf("the rule fails on %d sites, want %d: %v", len(bad), wantBad, bad)
	}
}
