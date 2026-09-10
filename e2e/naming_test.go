//go:build e2e

package e2e

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"testing"
)

// scenarioTestName is docs/testing.md's naming convention:
// Test<Tier>_<Group>_<Scenario>, tier in {Smoke, Full, Regression}. CI
// selects tests by matching this prefix with -run, so a name that drifts
// from it is silently dropped from every workflow rather than failing loud.
var scenarioTestName = regexp.MustCompile(`^Test(Smoke|Full|Regression)_[A-Za-z0-9]+_[A-Za-z0-9]+$`)

// TestRegression_Naming_ScenarioTestsMatchConvention parses every e2e test
// file as source (go/ast — build tags do not affect parsing, so this needs
// no live infrastructure) and fails on any top-level scenario test function
// whose name does not match scenarioTestName. A test function is identified
// by signature (func(*testing.T)), not by name, so TestMain and ordinary
// helpers are never mistaken for scenario tests.
func TestRegression_Naming_ScenarioTestsMatchConvention(t *testing.T) {
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatalf("glob e2e test files: %v", err)
	}
	if len(files) == 0 {
		t.Fatalf("no e2e test files found — glob pattern is likely wrong")
	}

	fset := token.NewFileSet()
	var violations []string
	for _, file := range files {
		f, err := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !isTestFunc(fn) {
				continue
			}
			if !scenarioTestName.MatchString(fn.Name.Name) {
				violations = append(violations, fset.Position(fn.Pos()).String()+": "+fn.Name.Name)
			}
		}
	}
	if len(violations) > 0 {
		t.Fatalf("test names must match %s (Test<Tier>_<Group>_<Scenario>):\n%s",
			scenarioTestName.String(), joinLines(violations))
	}
}

// goTestName is go test's own discovery rule for which functions it runs —
// "Test" followed by nothing or a non-lowercase rune (golang.org/x/tools
// convention). A helper like fullTarget(t *testing.T) Target shares the
// signature real tests use but go test never runs it, so it must not be
// held to the scenario naming convention either.
var goTestName = regexp.MustCompile(`^Test([^a-z].*)?$`)

// isTestFunc identifies a function go test will actually execute: a name
// goTestName accepts, no return values, and exactly one *testing.T
// parameter — matching go test's own rules, not just a signature a
// non-test helper could share.
func isTestFunc(fn *ast.FuncDecl) bool {
	if !goTestName.MatchString(fn.Name.Name) {
		return false
	}
	if fn.Type.Results != nil && len(fn.Type.Results.List) > 0 {
		return false
	}
	params := fn.Type.Params.List
	if len(params) != 1 {
		return false
	}
	star, ok := params[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "testing" && sel.Sel.Name == "T"
}

func joinLines(lines []string) string {
	out := ""
	for _, l := range lines {
		out += "  " + l + "\n"
	}
	return out
}
