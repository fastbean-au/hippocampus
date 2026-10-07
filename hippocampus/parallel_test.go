package hippocampus

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// sharedTestState is what a test in this package must not touch while running in parallel: the
// package variables tests replace to shorten a delay or observe a seam, and the process-wide logger
// and OTEL provider. Go resumes parallel tests only after every sequential top-level test has
// finished, so a test that touches these is safe exactly when it is not parallel.
var sharedTestState = regexp.MustCompile(
	`\b(callbackBusyDelay|callbackIdleDelay|outboxBusyDelay|outboxIdleDelay|reconcileInitialDelay|` +
		`reconcilePageDelay|sharedCallJoined|tel|walCheckInterval)\s*(,\s*[\w.]+\s*)*=[^=]` +
		`|\botel\.Set\w+\(|\blogtest\.NewGlobal\(|\blog\.Set\w+\(|SetOutput\(|\bcaptureLogs\(`,
)

// TestEveryTestDecidesWhetherItIsParallel holds the package to the arrangement that made its tests
// parallel (TODO-3 item 177): every top-level test either calls t.Parallel() as its first statement
// or says, in a "Not parallel:" line of its doc comment, which shared state it touches. A new test
// therefore has to make the choice rather than inherit one, and a parallel test that reaches for
// shared state directly is refused - the race detector would catch some of those, but only the ones
// that happen to overlap on the run that has it on.
//
// The direct check sees only the test's own body. A helper that touches shared state reaches its
// callers through the note on each of them, which is what the scan that applied this arrangement
// worked out transitively.
func TestEveryTestDecidesWhetherItIsParallel(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatalf("listing test files: %s", err)
	}

	for _, path := range files {
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %s", path, err)
		}

		fset := token.NewFileSet()

		file, err := parser.ParseFile(fset, path, source, parser.ParseComments)
		if err != nil {
			t.Fatalf("parsing %s: %s", path, err)
		}

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !isTopLevelTest(fn) {
				continue
			}

			body := string(source[fset.Position(fn.Body.Lbrace).Offset:fset.Position(fn.Body.Rbrace).Offset])
			parallel := firstStatementIsParallel(fn)
			noted := hasNotParallelNote(fn)

			switch {

			case parallel && noted:
				t.Errorf("%s: %s is parallel but its doc comment says it is not", path, fn.Name.Name)

			case parallel && sharedTestState.MatchString(body):
				t.Errorf("%s: %s runs in parallel but touches shared test state (%q): drop t.Parallel() and add a \"Not parallel:\" line saying why",
					path, fn.Name.Name, sharedTestState.FindString(body))

			case !parallel && !noted:
				t.Errorf("%s: %s neither calls t.Parallel() first nor says why not - add one, or a \"Not parallel:\" line to its doc comment",
					path, fn.Name.Name)

			}
		}
	}
}

// hasNotParallelNote reports whether fn's doc comment has a line beginning "Not parallel:".
func hasNotParallelNote(fn *ast.FuncDecl) bool {
	if fn.Doc == nil {
		return false
	}

	for _, line := range strings.Split(fn.Doc.Text(), "\n") {
		if strings.HasPrefix(line, "Not parallel:") {
			return true
		}
	}

	return false
}

// isTopLevelTest reports whether fn is a test the go tool runs: TestXxx(t *testing.T).
func isTopLevelTest(fn *ast.FuncDecl) bool {
	if !strings.HasPrefix(fn.Name.Name, "Test") || fn.Type.Params == nil || len(fn.Type.Params.List) != 1 {
		return false
	}

	star, ok := fn.Type.Params.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}

	selector, ok := star.X.(*ast.SelectorExpr)

	return ok && selector.Sel.Name == "T"
}

// firstStatementIsParallel reports whether fn's body opens with t.Parallel().
func firstStatementIsParallel(fn *ast.FuncDecl) bool {
	if len(fn.Body.List) == 0 {
		return false
	}

	expr, ok := fn.Body.List[0].(*ast.ExprStmt)
	if !ok {
		return false
	}

	call, ok := expr.X.(*ast.CallExpr)
	if !ok {
		return false
	}

	selector, ok := call.Fun.(*ast.SelectorExpr)

	return ok && selector.Sel.Name == "Parallel"
}
