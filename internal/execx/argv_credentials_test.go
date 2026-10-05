package execx

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/testutil"
)

// Every local user can read the argv of a process (ps, /proc/<pid>/cmdline),
// and audit tools store it. Thus a credential goes to a child process on
// stdin, in a file or in the environment, never as an argument.
//
// TestNoCredentialOnArgv reads the Go sources and finds each place that
// starts a process: an execx.Cmd literal (its Args), exec.Command or
// exec.CommandContext (the arguments after the name), and a call of a
// wrapper. A wrapper is an unexported function that hands some of its
// parameters to the Args of a process start, such as
// (*kubehz.Context).capture: the call's arguments for those parameters
// count. For each one it reads
// the argument expression. For a local variable in it, it also reads each
// assignment to that variable in the same function, and so on. A finding
// is one of these:
//   - a string that starts with --from-literal (kubectl takes the value on
//     argv; use --from-file or --from-env-file=/dev/stdin);
//   - a string that holds "Authorization" or "Bearer " (a header goes in a
//     curl config on stdin);
//   - a read of an environment variable whose name holds TOKEN, SECRET,
//     PASSWORD, PASSWD or a KEY word (os.Getenv, getenv);
//   - a variable or field named like a credential (token, secret, password,
//     passwd, bearer, credential, nonce, claim code), unless the name ends
//     in Name, Path, File, Dir, URL, Hash or Ref.
//
// The test reads code, not data. A credential that comes in through a
// value without such a name stays hidden from it. One such case is known:
// `lo kubehz node join` runs the platform's `kubeadm join … --token <t>`
// line (internal/kubehz/node.go), so the short-lived bootstrap token is on
// kubeadm's argv in both implementations.
func TestNoCredentialOnArgv(t *testing.T) {
	root := testutil.RepoRoot(t)
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	var names []string
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			files[rel] = f
			names = append(names, rel)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	wrappers := findWrappers(files)
	var findings []string
	sinks := 0
	for _, rel := range names {
		found, n := argvFindings(fset, rel, files[rel], wrappers)
		findings = append(findings, found...)
		sinks += n
	}
	for _, w := range []string{"capture", "runQuiet", "liveDrift"} {
		if _, ok := wrappers[w]; !ok {
			t.Errorf("the scan does not see the wrapper %s", w)
		}
	}
	for _, f := range findings {
		t.Errorf("a credential can reach argv: %s", f)
	}
	t.Logf("read %d process starts", sinks)
	// A scan that finds no process starts proves nothing.
	if sinks < minArgvSinks {
		t.Errorf("the scan found %d process starts, want at least %d", sinks, minArgvSinks)
	}
}

// minArgvSinks is below the count when the test was written (385, see the
// log line of -v), so a refactor that moves the starts out of sight fails.
const minArgvSinks = 300

// notCredentials are names that the rules flag, but that hold no
// credential. Each entry gives the reason.
var notCredentials = map[string]string{
	"agentSecret":      "the name of the agent Secret object, kubehz-agent (internal/kubehz)",
	"kubeconfigSecret": "the name of a kubeconfig Secret object (internal/operator)",
}

// The test must see the sinks it guards: a scan that finds none proves
// nothing.
func TestNoCredentialOnArgvSeesTheSinks(t *testing.T) {
	src := `package p
func f(c Context, token string) {
	args := []string{"create", "secret", "--from-literal=k=" + v}
	args = append(args, "-H", "Authorization: Bearer "+token)
	_ = c.Runner.Run(ctx, execx.Cmd{Name: "kubectl", Args: args})
	_ = exec.Command("curl", "-u", os.Getenv("HROBOT_PASSWORD"))
	_ = exec.CommandContext(ctx, "x", secretName, tokenPath, password)
}
func (c *Context) capture(ctx context.Context, quiet bool, name string, args ...string) (string, error) {
	return "", c.Runner.Run(ctx, execx.Cmd{Name: name, Args: args})
}
func (c *Context) claim(ctx context.Context, nonce string) {
	_, _ = c.capture(ctx, false, "kubectl", "annotate", "cm", "kubehz.cloud/claim-nonce="+nonce)
	_ = c.runPatch(ctx, nonce)
}
func (c *Context) runPatch(ctx context.Context, args ...string) error {
	_, err := c.capture(ctx, true, "kubectl", args...)
	return err
}`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]*ast.File{"p.go": f}
	wrappers := findWrappers(files)
	if w := wrappers["capture"]; !w.variadic[3] || len(w.variadic) != 1 || len(w.params) != 0 {
		t.Errorf("capture = %+v, want argument 3 on (variadic)", w)
	}
	if w := wrappers["runPatch"]; !w.variadic[1] || len(w.params) != 0 {
		t.Errorf("runPatch = %+v, want argument 1 on (variadic)", w)
	}
	found, sinks := argvFindings(fset, "p.go", f, wrappers)
	// The three direct starts, the start in capture, the two calls of
	// capture, the call of runPatch.
	if sinks != 7 {
		t.Errorf("sinks = %d, want 7", sinks)
	}
	got := strings.Join(found, "\n")
	for _, want := range []string{
		`"--from-literal=k="`,
		`"Authorization: Bearer "`,
		`identifier token`,
		`os.Getenv("HROBOT_PASSWORD")`,
		`identifier password`,
		`p.go:13: identifier nonce`,
		`p.go:14: identifier nonce`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("finding %q missing in:\n%s", want, got)
		}
	}
	for _, clean := range []string{"secretName", "tokenPath"} {
		if strings.Contains(got, "identifier "+clean) {
			t.Errorf("%s is no credential, but it is a finding:\n%s", clean, got)
		}
	}
}

var (
	credentialName  = regexp.MustCompile(`(?i)(token|secret|password|passwd|bearer|credential|nonce|claim_?code)`)
	harmlessSuffix  = regexp.MustCompile(`(Name|Path|File|Dir|URL|Url|Hash|Ref)s?$`)
	credentialEnv   = regexp.MustCompile(`(TOKEN|SECRET|PASSWORD|PASSWD|_KEY$|^KEY_|_KEY_)`)
	headerLiteral   = regexp.MustCompile(`Authorization|Bearer `)
	envReaderSuffix = regexp.MustCompile(`(?i)getenv$`)
)

// wrapper names the parameters of a function that reach argv, by index:
// params for single ones, variadic for a variadic one, whose arguments from
// that index on all reach argv. Functions of the same name join their
// sets, so the sets only grow and the search ends.
type wrapper struct {
	params   map[int]bool
	variadic map[int]bool
}

// findWrappers returns the unexported functions that hand some of their
// parameters to a process start, by name. A function that hands a
// parameter to a wrapper is a wrapper too. Functions and methods of the
// same name count as one: their parameter sets are joined.
func findWrappers(files map[string]*ast.File) map[string]wrapper {
	wrappers := map[string]wrapper{}
	for changed := true; changed; {
		changed = false
		for _, f := range files {
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				params := map[string]int{}
				variadic := -1
				i := 0
				for _, field := range fn.Type.Params.List {
					if _, ok := field.Type.(*ast.Ellipsis); ok {
						variadic = i
					}
					for _, name := range field.Names {
						params[name.Name] = i
						i++
					}
					if len(field.Names) == 0 {
						i++
					}
				}
				reach := map[int]bool{}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					for _, args := range sinkArgs(n, wrappers) {
						ast.Inspect(args, func(m ast.Node) bool {
							switch x := m.(type) {
							case *ast.SelectorExpr:
								// A field of a parameter (c.Args of a whole
								// Cmd) does not make a wrapper.
								return false
							case *ast.CompositeLit:
								// A Cmd literal handed on is a start of its
								// own; its other fields are no argv.
								return !isCmdType(x.Type)
							case *ast.Ident:
								if idx, ok := params[x.Name]; ok {
									reach[idx] = true
								}
							}
							return true
						})
					}
					return true
				})
				// Only unexported names: an exported name such as Run or
				// Set also names methods of the standard library.
				if len(reach) == 0 || ast.IsExported(fn.Name.Name) {
					continue
				}
				w, ok := wrappers[fn.Name.Name]
				if !ok {
					w = wrapper{params: map[int]bool{}, variadic: map[int]bool{}}
				}
				for idx := range reach {
					set := w.params
					if idx == variadic {
						set = w.variadic
					}
					if !set[idx] {
						set[idx] = true
						changed = true
					}
				}
				wrappers[fn.Name.Name] = w
			}
		}
	}
	return wrappers
}

// argvFindings returns one line for each finding in the file, and the
// number of process starts it read.
func argvFindings(fset *token.FileSet, rel string, f *ast.File, wrappers map[string]wrapper) ([]string, int) {
	var out []string
	sinks := 0
	walkFunc := func(body *ast.BlockStmt) {
		if body == nil {
			return
		}
		assigns := localAssignments(body)
		ast.Inspect(body, func(n ast.Node) bool {
			args := sinkArgs(n, wrappers)
			if len(args) > 0 {
				sinks++
			}
			for _, args := range args {
				for _, finding := range inspectArgs(args, assigns) {
					out = append(out, rel+":"+strconv.Itoa(fset.Position(n.Pos()).Line)+": "+finding)
				}
			}
			return true
		})
	}
	for _, decl := range f.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			walkFunc(fn.Body)
		}
	}
	return dedupe(out), sinks
}

// sinkArgs returns the argument expressions of a process start: the Args of
// an execx.Cmd or Cmd literal, the arguments of exec.Command after the
// name (exec.CommandContext: after the context and the name), and the
// arguments of a wrapper call for the parameters that reach argv.
func sinkArgs(n ast.Node, wrappers map[string]wrapper) []ast.Expr {
	switch x := n.(type) {
	case *ast.CompositeLit:
		if !isCmdType(x.Type) {
			return nil
		}
		for _, e := range x.Elts {
			kv, ok := e.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Args" {
				return []ast.Expr{kv.Value}
			}
		}
	case *ast.CallExpr:
		if w, ok := wrappers[calleeName(x.Fun)]; ok {
			var out []ast.Expr
			for i, a := range x.Args {
				reaches := w.params[i]
				for v := range w.variadic {
					reaches = reaches || i >= v
				}
				if reaches {
					out = append(out, a)
				}
			}
			return out
		}
		sel, ok := x.Fun.(*ast.SelectorExpr)
		if !ok {
			return nil
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "exec" {
			return nil
		}
		switch sel.Sel.Name {
		case "Command":
			if len(x.Args) > 1 {
				return x.Args[1:]
			}
		case "CommandContext":
			if len(x.Args) > 2 {
				return x.Args[2:]
			}
		}
	}
	return nil
}

func isCmdType(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.SelectorExpr:
		return x.Sel.Name == "Cmd"
	case *ast.Ident:
		return x.Name == "Cmd"
	}
	return false
}

// localAssignments maps each variable name in the body to the expressions
// assigned to it (`x := …`, `x = …`, `var x = …`).
func localAssignments(body *ast.BlockStmt) map[string][]ast.Expr {
	m := map[string][]ast.Expr{}
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range x.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok {
					continue
				}
				if len(x.Rhs) == len(x.Lhs) {
					m[id.Name] = append(m[id.Name], x.Rhs[i])
				} else if len(x.Rhs) == 1 {
					m[id.Name] = append(m[id.Name], x.Rhs[0])
				}
			}
		case *ast.ValueSpec:
			for i, id := range x.Names {
				if i < len(x.Values) {
					m[id.Name] = append(m[id.Name], x.Values[i])
				}
			}
		}
		return true
	})
	return m
}

// inspectArgs walks the expressions, and through the local assignments the
// expressions that feed them, and returns the findings.
func inspectArgs(args ast.Expr, assigns map[string][]ast.Expr) []string {
	var out []string
	seen := map[string]bool{}
	work := []ast.Expr{args}
	for len(work) > 0 {
		e := work[0]
		work = work[1:]
		ast.Inspect(e, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.BasicLit:
				if x.Kind != token.STRING {
					return true
				}
				s, err := strconv.Unquote(x.Value)
				if err != nil {
					return true
				}
				if strings.HasPrefix(s, "--from-literal") || headerLiteral.MatchString(s) {
					out = append(out, "string "+x.Value)
				}
			case *ast.CallExpr:
				if name := callName(x.Fun); envReaderSuffix.MatchString(name) && len(x.Args) == 1 {
					if lit, ok := x.Args[0].(*ast.BasicLit); ok {
						if v, err := strconv.Unquote(lit.Value); err == nil && credentialEnv.MatchString(v) {
							out = append(out, name+"("+lit.Value+")")
						}
					}
				}
			case *ast.SelectorExpr:
				if credentialName.MatchString(x.Sel.Name) && !harmlessSuffix.MatchString(x.Sel.Name) {
					out = append(out, "field "+x.Sel.Name)
				}
				// The selected field decides; do not read the receiver as
				// a credential name.
				ast.Inspect(x.X, func(m ast.Node) bool {
					if id, ok := m.(*ast.Ident); ok && !seen[id.Name] {
						seen[id.Name] = true
						work = append(work, assigns[id.Name]...)
					}
					return true
				})
				return false
			case *ast.Ident:
				if _, ok := notCredentials[x.Name]; !ok && credentialName.MatchString(x.Name) && !harmlessSuffix.MatchString(x.Name) {
					out = append(out, "identifier "+x.Name)
				}
				if !seen[x.Name] {
					seen[x.Name] = true
					work = append(work, assigns[x.Name]...)
				}
			}
			return true
		})
	}
	return out
}

// calleeName is the bare name of the called function or method.
func calleeName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return x.Sel.Name
	}
	return ""
}

func callName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		if id, ok := x.X.(*ast.Ident); ok {
			return id.Name + "." + x.Sel.Name
		}
		return x.Sel.Name
	}
	return ""
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
