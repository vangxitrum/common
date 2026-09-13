package password_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTheKeyComparisonIsConstantTime reads the source, for the reason
// secret/constant_time_test.go gives: a timing test cannot see the difference
// between a constant-time compare and a short circuit through scheduling
// noise, and reading the code fails on the day somebody writes ==.
func TestTheKeyComparisonIsConstantTime(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "password.go", nil, 0)
	require.NoError(t, err)

	var fn *ast.FuncDecl
	for _, decl := range file.Decls {
		if d, ok := decl.(*ast.FuncDecl); ok && d.Name.Name == "verifyArgon2" {
			fn = d
		}
	}
	require.NotNil(t, fn, "password.go must declare verifyArgon2")

	var constantTime bool
	var banned []string
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			switch name := callName(node); name {
			case "subtle.ConstantTimeCompare":
				constantTime = true
			case "bytes.Equal",
				"slices.Equal",
				"reflect.DeepEqual",
				"strings.EqualFold",
				"strings.Compare":
				banned = append(banned, name)
			}
		case *ast.BinaryExpr:
			if node.Op != token.EQL && node.Op != token.NEQ {
				return true
			}
			if harmless(node.X) || harmless(node.Y) {
				return true
			}
			banned = append(
				banned,
				fset.Position(node.Pos()).String()+": "+node.Op.String(),
			)
		}
		return true
	})

	require.True(
		t,
		constantTime,
		"verifyArgon2 must compare with subtle.ConstantTimeCompare",
	)
	require.Empty(t, banned, "verifyArgon2 must not compare keys any other way")
}

func callName(call *ast.CallExpr) string {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return ""
	}
	return pkg.Name + "." + sel.Sel.Name
}

// harmless is an operand that cannot be key material: a literal, nil, a
// length, or the result of the constant-time compare itself.
func harmless(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.BasicLit:
		return true
	case *ast.Ident:
		return e.Name == "nil"
	case *ast.CallExpr:
		if id, ok := e.Fun.(*ast.Ident); ok && id.Name == "len" {
			return true
		}
		return callName(e) == "subtle.ConstantTimeCompare"
	}
	return false
}
