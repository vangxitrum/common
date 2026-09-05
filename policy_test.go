package apierr_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-template/internal/utils/apierr"
)

// constructors take the reason slug as their first argument.
var constructors = map[string]bool{
	"New": true, "Wrap": true, "Wrapf": true, "WrapWith": true,
	"BadRequest": true, "Unauthorized": true, "Forbidden": true,
	"NotFound": true, "Conflict": true, "TooManyRequests": true,
	"Internal": true, "Unavailable": true,
}

// TestEveryReasonInTheCodebaseIsUsable walks the whole module and holds every
// apierr reason slug to the same rule, so the convention is enforced rather
// than remembered.
//
// This is the guardrail against depin's failure mode: there, every missing
// record answered with the single slug "not-found", so a contract that did not
// exist was indistinguishable from a file that did not exist - and because the
// package tag supplied the subject, a contract lookup inside the file service
// reported "file: not found".
func TestEveryReasonInTheCodebaseIsUsable(t *testing.T) {
	t.Parallel()

	root := moduleRoot(t)
	found := 0

	require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Build products and vendored code are not ours to hold to this
			// rule, and generated Go does not always parse with a plain
			// go/parser - the SDK openapi-generator emits does not.
			switch d.Name() {
			case ".git", "bin", ".cache", "sdk", "vendor", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}

		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}

		// This test file names the bad slugs on purpose, to prove they are
		// rejected; holding it to the rule would be circular.
		if strings.HasSuffix(path, "policy_test.go") {
			return nil
		}

		ast.Inspect(file, func(node ast.Node) bool {
			reason, pos, ok := reasonArg(node, fset)
			if !ok {
				return true
			}
			found++
			require.NoErrorf(t, apierr.ValidReason(reason), "%s: %s", pos, reason)
			return true
		})
		return nil
	}))

	require.NotZero(t, found, "the walk matched no apierr calls, so it proves nothing")
	t.Logf("checked %d reason slugs", found)
}

// reasonArg returns the reason literal of an apierr constructor call.
func reasonArg(node ast.Node, fset *token.FileSet) (reason, pos string, ok bool) {
	call, isCall := node.(*ast.CallExpr)
	if !isCall || len(call.Args) == 0 {
		return "", "", false
	}

	sel, isSel := call.Fun.(*ast.SelectorExpr)
	if !isSel || !constructors[sel.Sel.Name] {
		return "", "", false
	}
	pkg, isIdent := sel.X.(*ast.Ident)
	if !isIdent || pkg.Name != "apierr" {
		return "", "", false
	}

	lit, isLit := call.Args[0].(*ast.BasicLit)
	if !isLit || lit.Kind != token.STRING {
		// A non-literal reason cannot be checked here. That is what the unit
		// tests of ValidReason are for.
		return "", "", false
	}
	value, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", "", false
	}

	return value, fset.Position(lit.Pos()).String(), true
}

// moduleRoot walks up from this test file to the directory holding go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)

	dir := filepath.Dir(file)
	for range 10 {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}

	t.Fatal("no go.mod found above " + file)
	return ""
}
