package secret_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestEqualIsConstantTime guards the one property of this package that cannot
// be observed from the outside.
//
// A statistical timing test is the obvious approach and it is worthless here:
// on a modern CPU the difference between a constant-time compare and a short
// circuit is far below the noise of scheduling and cache, so such a test is
// flaky when it fails and proves nothing when it passes. Reading the source
// is deterministic, costs milliseconds, and fails on the exact day somebody
// "simplifies" Equal into a == b.
func TestEqualIsConstantTime(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "hash.go", nil, 0)
	require.NoError(t, err)

	t.Run("hash.go imports crypto/subtle", func(t *testing.T) {
		var found bool
		for _, imp := range file.Imports {
			if imp.Path.Value == `"crypto/subtle"` {
				found = true
			}
		}
		require.True(t, found, "hash.go must import crypto/subtle")
	})

	equal := findMethod(file, "Hash", "Equal")
	require.NotNil(t, equal, "hash.go must declare func (h Hash) Equal")

	var (
		callsConstantTime bool
		banned            []string
	)
	ast.Inspect(equal.Body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			if name := selectorName(node.Fun); name != "" {
				switch name {
				case "subtle.ConstantTimeCompare":
					callsConstantTime = true
				case "bytes.Equal",
					"strings.EqualFold",
					"strings.Compare",
					"subtle.ConstantTimeEq":
					banned = append(banned, name)
				}
			}
		case *ast.BinaryExpr:
			// A == over the receiver or the parameter short circuits on the
			// first differing byte. Comparing the result of
			// ConstantTimeCompare against 1 is fine, and is an *ast.Ident on
			// one side with a BasicLit on the other.
			if node.Op != token.EQL && node.Op != token.NEQ {
				return true
			}
			// Checking the result of ConstantTimeCompare against 1 is the
			// correct idiom, and its operands do mention the hash.
			if containsCall(node.X, "subtle.ConstantTimeCompare") ||
				containsCall(node.Y, "subtle.ConstantTimeCompare") {
				return true
			}
			if referencesSecret(node.X, equal) ||
				referencesSecret(node.Y, equal) {
				banned = append(
					banned,
					"comparison of the hash with "+node.Op.String(),
				)
			}
		}
		return true
	})

	require.True(
		t,
		callsConstantTime,
		"Hash.Equal must call subtle.ConstantTimeCompare",
	)
	require.Empty(t, banned, "Hash.Equal must not compare hash bytes directly")
}

// findMethod returns the method named name on the given receiver type.
func findMethod(file *ast.File, recv, name string) *ast.FuncDecl {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Name.Name != name || fn.Body == nil {
			continue
		}
		if len(fn.Recv.List) == 1 && typeName(fn.Recv.List[0].Type) == recv {
			return fn
		}
	}
	return nil
}

// referencesSecret reports whether expr mentions the receiver or a parameter
// of fn, directly or through a conversion such as []byte(h).
func referencesSecret(expr ast.Expr, fn *ast.FuncDecl) bool {
	names := map[string]bool{}
	for _, field := range append(fn.Recv.List, fn.Type.Params.List...) {
		for _, ident := range field.Names {
			names[ident.Name] = true
		}
	}

	var found bool
	ast.Inspect(expr, func(n ast.Node) bool {
		if ident, ok := n.(*ast.Ident); ok && names[ident.Name] {
			found = true
		}
		return !found
	})
	return found
}

// typeName renders an identifier or a pointer to one as a bare name.
func typeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return typeName(t.X)
	}
	return ""
}

// selectorName renders pkg.Fn as "pkg.Fn".
func selectorName(expr ast.Expr) string {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	ident, ok := sel.X.(*ast.Ident)
	if !ok {
		return ""
	}
	return ident.Name + "." + sel.Sel.Name
}

// containsCall reports whether expr contains a call to the named function.
func containsCall(expr ast.Expr, name string) bool {
	var found bool
	ast.Inspect(expr, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && selectorName(call.Fun) == name {
			found = true
		}
		return !found
	})
	return found
}
