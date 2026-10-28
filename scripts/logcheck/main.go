// Command logcheck fails when non-test Go code logs raw error text. Logger
// calls must record errclass categories under "error_class" instead of
// passing err or err.Error(), which may embed URLs, payloads or paths.
package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Restrict type checking to standard constructors; it must not load project sources.
type constructorImporter struct{ standard types.Importer }

func (i constructorImporter) Import(path string) (*types.Package, error) {
	if path == "errors" || path == "fmt" {
		return i.standard.Import(path)
	}
	return nil, fmt.Errorf("unsupported import %q", path)
}

var (
	importMu sync.Mutex
	imports  = constructorImporter{standard: importer.Default()}
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

// checkSource reports logger calls that pass raw errors, error keys, or error text.
func checkSource(fset *token.FileSet, name string, src []byte) ([]finding, error) {
	file, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		return nil, err
	}
	typeInfo := &types.Info{Types: make(map[ast.Expr]types.TypeAndValue)}
	config := types.Config{Importer: imports, Error: func(error) {}}
	importMu.Lock()
	_, _ = config.Check("logcheck/source", fset, []*ast.File{file}, typeInfo)
	importMu.Unlock()
	trustedImports := make(map[string]bool)
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil || path != "github.com/LeXwDeX/tribunal/internal/errclass" {
			continue
		}
		name := "errclass"
		if spec.Name != nil {
			name = spec.Name.Name
		}
		trustedImports[name] = true
	}
	isError := func(e ast.Expr) bool { return isRawError(e, typeInfo, trustedImports) }
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
		errorKeys := errorKeyPositions(call, typeInfo)
		for i, arg := range call.Args {
			if errorKeys[i] {
				report(arg, `logger call uses key "error" for a raw error`)
			}
			if isError(arg) && !errorKeys[i-1] {
				report(arg, "logger call records a raw error")
			}
			ast.Inspect(arg, func(n ast.Node) bool {
				inner, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if isErrorTextCall(inner) {
					report(inner, "logger call records err.Error()")
				}
				if isAttrCall(inner) && len(inner.Args) > 0 {
					if isStringLit(inner.Args[0], "error") {
						report(inner, `logger attribute uses key "error" for a raw error`)
					} else if len(inner.Args) == 2 && inner.Fun.(*ast.SelectorExpr).Sel.Name == "Any" && isError(inner.Args[1]) {
						report(inner.Args[1], "logger attribute records a raw error")
					}
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

// shortcut: unresolved imports and dynamic interface values can hide errors; use full package analysis to cover them.
func isRawError(e ast.Expr, info *types.Info, trustedImports map[string]bool) bool {
	for {
		paren, ok := e.(*ast.ParenExpr)
		if !ok {
			break
		}
		e = paren.X
	}
	if typ := info.TypeOf(e); typ != nil && typ != types.Typ[types.Invalid] {
		return types.Implements(typ, types.Universe.Lookup("error").Type().Underlying().(*types.Interface))
	}
	id, ok := e.(*ast.Ident)
	if !ok {
		return false
	}
	if id.Obj != nil {
		var declaredType ast.Expr
		var initializer ast.Expr
		switch decl := id.Obj.Decl.(type) {
		case *ast.Field:
			declaredType = decl.Type
		case *ast.ValueSpec:
			declaredType = decl.Type
			if len(decl.Names) == len(decl.Values) {
				for i, name := range decl.Names {
					if name.Obj == id.Obj {
						initializer = decl.Values[i]
					}
				}
			}
		case *ast.AssignStmt:
			if len(decl.Lhs) == len(decl.Rhs) {
				for i, lhs := range decl.Lhs {
					if assigned, ok := lhs.(*ast.Ident); ok && assigned.Obj == id.Obj {
						initializer = decl.Rhs[i]
					}
				}
			}
		}
		if _, literal := initializer.(*ast.BasicLit); literal && declaredType == nil {
			return false
		}
		if call, ok := initializer.(*ast.CallExpr); ok && declaredType == nil {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Of" {
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Obj == nil && trustedImports[pkg.Name] {
					return false
				}
			}
		}
		if typ, ok := declaredType.(*ast.Ident); ok && typ.Obj == nil {
			switch typ.Name {
			case "error":
				return true
			case "string", "bool", "byte", "rune", "int", "int8", "int16", "int32", "int64",
				"uint", "uint8", "uint16", "uint32", "uint64", "uintptr", "float32", "float64", "complex64", "complex128":
				return false
			}
		}
	}
	return id.Name == "err" || strings.HasSuffix(id.Name, "Err")
}

// errorKeyPositions follows slog's key/value and Attr argument layout.
func errorKeyPositions(call *ast.CallExpr, info *types.Info) map[int]bool {
	keys := make(map[int]bool)
	method := call.Fun.(*ast.SelectorExpr).Sel.Name
	start := 1
	switch method {
	case "With":
		start = 0
	case "DebugContext", "InfoContext", "WarnContext", "ErrorContext":
		start = 2
	case "Log":
		start = 3
	case "LogAttrs":
		return keys
	}
	for i := start; i < len(call.Args); i++ {
		arg := call.Args[i]
		isString := false
		if typ := info.TypeOf(arg); typ != nil {
			if basic, ok := typ.Underlying().(*types.Basic); ok {
				isString = basic.Info()&types.IsString != 0
			}
		}
		if lit, ok := arg.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			isString = true
		}
		if isString && i+1 < len(call.Args) {
			if isStringLit(arg, "error") {
				keys[i] = true
			}
			i++
		}
	}
	return keys
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
