// Command logcheck fails when non-test Go code logs raw error text. Logger
// calls must record errclass categories under "error_class" instead of
// passing err or err.Error(), which may embed URLs, payloads or paths.
package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// logMethods are slog-style methods whose arguments are logged.
var logMethods = map[string]bool{
	"Debug": true, "Info": true, "Warn": true, "Error": true,
	"DebugContext": true, "InfoContext": true, "WarnContext": true, "ErrorContext": true,
	"Log": true, "LogAttrs": true, "With": true,
}

// skipDirs are not production Go code.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "web": true, "prototype": true, "third_party": true, "testdata": true,
}

type finding struct {
	Pos    token.Position
	Reason string
}

func (f finding) String() string {
	return fmt.Sprintf("%s:%d: %s", f.Pos.Filename, f.Pos.Line, f.Reason)
}

func main() {
	root := flag.String("root", ".", "repository root")
	flag.Parse()
	os.Exit(run(*root, os.Stdout, os.Stderr))
}

func run(root string, stdout, stderr io.Writer) int {
	findings, err := checkTree(root)
	if err != nil {
		fmt.Fprintf(stderr, "logcheck: %v\n", err)
		return 2
	}
	for _, f := range findings {
		fmt.Fprintln(stderr, f)
	}
	if len(findings) > 0 {
		fmt.Fprintf(stderr, "logcheck: %d logger call(s) record raw errors; log \"error_class\", errclass.Of(err) instead\n", len(findings))
		return 1
	}
	fmt.Fprintln(stdout, "logcheck: ok")
	return 0
}

// checkTree scans every non-test .go file under root.
func checkTree(root string) ([]finding, error) {
	var out []finding
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (skipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
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
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		found, err := checkSource(fset, filepath.ToSlash(rel), src)
		if err != nil {
			return err
		}
		out = append(out, found...)
		return nil
	})
	return out, err
}

// checkSource reports logger calls that pass a raw error: an "error" key
// followed by a value, slog.Any/String("error", ...), or any x.Error() call.
func checkSource(fset *token.FileSet, name string, src []byte) ([]finding, error) {
	file, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var out []finding
	report := func(n ast.Node, reason string) {
		out = append(out, finding{Pos: fset.Position(n.Pos()), Reason: reason})
	}
	var visit func(ast.Node) bool
	visit = func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isLogCall(call) {
			return true
		}
		for i, arg := range call.Args {
			if isStringLit(arg, "error") && i+1 < len(call.Args) {
				report(arg, `logger call uses key "error" for a raw error`)
			}
			ast.Inspect(arg, func(n ast.Node) bool {
				inner, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if isErrorTextCall(inner) {
					report(inner, "logger call records err.Error()")
				}
				if isAttrCall(inner) && len(inner.Args) > 0 && isStringLit(inner.Args[0], "error") {
					report(inner, `logger attribute uses key "error" for a raw error`)
				}
				return true
			})
		}
		// Arguments were inspected above; only the receiver chain, such as
		// logger.With(...).Info(...), still needs a visit.
		ast.Inspect(call.Fun, visit)
		return false
	}
	ast.Inspect(file, visit)
	return out, nil
}

func isLogCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || !logMethods[sel.Sel.Name] {
		return false
	}
	// x.Error() with no arguments is an error's text, not a logger call.
	return len(call.Args) > 0
}

func isErrorTextCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Error" && len(call.Args) == 0
}

func isAttrCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "slog" && (sel.Sel.Name == "Any" || sel.Sel.Name == "String")
}

func isStringLit(e ast.Expr, want string) bool {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	s, err := strconv.Unquote(lit.Value)
	return err == nil && s == want
}
