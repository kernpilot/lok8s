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
// starts a process: an execx.Cmd literal (its Args) and exec.Command or
// exec.CommandContext (the arguments after the name). For each one it reads
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
//     passwd, bearer, credential), unless the name ends in Name, Path,
//     File, Dir, URL, Hash or Ref.
//
// The test reads code, not data. A credential that comes in through a
// value without such a name stays hidden from it. One such case is known:
// `lo kubehz node join` runs the platform's `kubeadm join … --token <t>`
// line (internal/kubehz/node.go), so the short-lived bootstrap token is on
// kubeadm's argv in both implementations.
func TestNoCredentialOnArgv(t *testing.T) {
	root := testutil.RepoRoot(t)
	fset := token.NewFileSet()
	var findings []string
	sinks := 0
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
			found, n := argvFindings(fset, rel, f)
			findings = append(findings, found...)
			sinks += n
			return nil
		})
		if err != nil {
			t.Fatal(err)
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

// minArgvSinks is below the count when the test was written (124, see the
// log line of -v), so a refactor that moves the starts out of sight fails.
const minArgvSinks = 100

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
}`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	found, sinks := argvFindings(fset, "p.go", f)
	if sinks != 3 {
		t.Errorf("sinks = %d, want 3", sinks)
	}
	got := strings.Join(found, "\n")
	for _, want := range []string{
		`"--from-literal=k="`,
		`"Authorization: Bearer "`,
		`identifier token`,
		`os.Getenv("HROBOT_PASSWORD")`,
		`identifier password`,
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
	credentialName  = regexp.MustCompile(`(?i)(token|secret|password|passwd|bearer|credential)`)
	harmlessSuffix  = regexp.MustCompile(`(Name|Path|File|Dir|URL|Url|Hash|Ref)s?$`)
	credentialEnv   = regexp.MustCompile(`(TOKEN|SECRET|PASSWORD|PASSWD|_KEY$|^KEY_|_KEY_)`)
	headerLiteral   = regexp.MustCompile(`Authorization|Bearer `)
	envReaderSuffix = regexp.MustCompile(`(?i)getenv$`)
)

// argvFindings returns one line for each finding in the file, and the
// number of process starts it read.
func argvFindings(fset *token.FileSet, rel string, f *ast.File) ([]string, int) {
	var out []string
	sinks := 0
	walkFunc := func(body *ast.BlockStmt) {
		if body == nil {
			return
		}
		assigns := localAssignments(body)
		ast.Inspect(body, func(n ast.Node) bool {
			args := sinkArgs(n)
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
// an execx.Cmd or Cmd literal, and the arguments of exec.Command after the
// name (exec.CommandContext: after the context and the name).
func sinkArgs(n ast.Node) []ast.Expr {
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
				if credentialName.MatchString(x.Name) && !harmlessSuffix.MatchString(x.Name) {
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
