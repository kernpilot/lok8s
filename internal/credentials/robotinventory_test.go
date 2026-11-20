package credentials

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/testutil"
)

// robotCredentialReaders are the Go files (not tests) that hold a string
// literal with HROBOT_USER or HROBOT_PASSWORD, each with the reason it may.
// A new file that reads either name must check it for a control character
// (NoControl) or say why not, here.
var robotCredentialReaders = map[string]string{
	"internal/provision/creds.go":        "exports the value from the secrets store into the environment, unchecked, as the bash twin does",
	"internal/driver/kubeone/kubeone.go": "checks the value (NoControl) before each step that passes it on",
	"internal/driver/capi/generate.go":   "refuses a CR or an LF in the value (NoNewline), as the bash twin does",
}

// The scan covers every .go file under the repository root that is not a
// test: the main module, and the modules of kustomize/ and ai/ in their
// directories. It skips .git and node_modules. It finds a name in a string
// literal, not in a comment, and not a name that is built at run time.
func TestRobotCredentialReadersAreKnown(t *testing.T) {
	t.Parallel()
	root := testutil.RepoRoot(t)
	var found []string
	scanned := 0
	err := filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			if e.Name() == ".git" || e.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, src, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		scanned++
		reads := false
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Errorf("%s: %s: %v", path, lit.Value, err)
				return true
			}
			if strings.Contains(value, "HROBOT_USER") || strings.Contains(value, "HROBOT_PASSWORD") {
				reads = true
			}
			return true
		})
		if reads {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			found = append(found, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 100 {
		t.Fatalf("scanned %d Go files under %s, want the repository", scanned, root)
	}
	for _, rel := range found {
		if _, ok := robotCredentialReaders[rel]; !ok {
			t.Errorf("%s reads HROBOT_USER or HROBOT_PASSWORD: check the value (credentials.NoControl), or add the file to robotCredentialReaders with the reason", rel)
		}
	}
	for rel := range robotCredentialReaders {
		if !slices.Contains(found, rel) {
			t.Errorf("robotCredentialReaders names %s, which reads neither name now: remove the entry", rel)
		}
	}
}
