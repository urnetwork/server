// Every rescue handler this module passes to a HandleError-style helper is one
// the helper can run, and every context cancel passed to HandleError is at a
// reviewed call site.
//
// The helpers take their handlers as any, so the compiler accepts a handler
// of any type, and a handler the helper's type switch does not match is never
// run. That is how 24 context.CancelFunc handlers went dormant for years. The
// scan parses every Go file in the module, build constraints aside, and finds
// the helper calls by their import names. A function literal handler is
// checked by its syntax; for HandleError1 and HandleError2 its results must
// read the same as the helper's type arguments or its do literal's results.
// Any other handler is checked by its type: its package is type-checked from
// source against the standard library's export data, with every other import
// left empty, which resolves the context cancels and the module's own
// declarations without building the module. Neither helper module converts
// named function types, so a named handler type passes only as one of the two
// context cancels that HandleError runs.
package server

import (
	"bytes"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// One context cancel handler passed to HandleError outside tests, keyed by
// file, enclosing declaration and handler expression.
type rescueCancelSite struct {
	path     string
	function string
	handler  string
}

// HandleError runs a cancel handler on every raise, a cancellation raise of a
// narrower context included, so each site must cancel only an owner whose
// life ends with the work. The count is the number of calls at the site.
var reviewedRescueCancelSites = map[rescueCancelSite]int{
	// The run function's first defer calls the same cancel, so the handler
	// only repeats it.
	{path: "connect/resident.go", function: "(*Exchange).handleExchangeConnection", handler: "handleCancel"}:   2,
	{path: "connect/resident.go", function: "NewExchangeConnection", handler: "cancel"}:                        1,
	{path: "connect/resident.go", function: "(*ExchangeConnection).Run", handler: "self.cancel"}:               1,
	{path: "connect/resident.go", function: "newResidentDuringAdmission", handler: "cancel"}:                   1,
	{path: "connect/transport.go", function: "newConnectHandlerWithPacketConns", handler: "cancel"}:            1,
	{path: "connect/transport_announce.go", function: "newConnectionAnnounceWithReconcile", handler: "cancel"}: 1,
	{path: "proxy/proxy_client_notification.go", function: "newProxyClientNotification", handler: "cancel"}:    1,
	{path: "proxy/server.go", function: "NewWgServer", handler: "cancel"}:                                      1,
	{path: "search/search_local.go", function: "NewSearchLocal", handler: "cancel"}:                            1,
	{path: "session/jwk_validator.go", function: "NewJwkValidator", handler: "cancel"}:                         1,
	// The worker's owner should end with it: a raise leaves the resident
	// without the consumer, and the client reconnects to a fresh resident.
	{path: "connect/resident.go", function: "(*Resident).startClientCallbackWorkers", handler: "self.cancel"}: 1,
	{path: "connect/resident.go", function: "(*Resident).startClientForwardIngress", handler: "self.cancel"}:  1,
}

// The helpers whose trailing arguments are rescue handlers.
type rescueHelper struct {
	// server or connect
	module string
	name   string
	// index of the first handler argument
	firstHandler int
	// the number of results a handler returns
	results int
}

var rescueHelpers = []rescueHelper{
	{module: "server", name: "HandleError", firstHandler: 1},
	{module: "server", name: "HandleError1", firstHandler: 1, results: 1},
	{module: "server", name: "HandleError2", firstHandler: 1, results: 2},
	{module: "connect", name: "HandleError", firstHandler: 1},
	{module: "connect", name: "HandleError1", firstHandler: 1, results: 1},
	{module: "connect", name: "HandleError2", firstHandler: 1, results: 2},
	{module: "connect", name: "ExpectErrorWithReturn", firstHandler: 2},
}

// One handler argument the scan found.
type rescueHandlerArgument struct {
	helper rescueHelper
	file   *ast.File
	path   string
	arg    ast.Expr
}

// A package of the module: one directory and one package clause.
type rescuePackage struct {
	dir   string
	name  string
	files []*ast.File
	paths []string
}

func TestEveryRescueHandlerArgumentIsRunnable(t *testing.T) {
	root := "."
	fset := token.NewFileSet()
	packages := map[string]*rescuePackage{}
	// the files without a helper call, parsed only for packages that need types
	unparsedPaths := map[string][]string{}
	arguments := []rescueHandlerArgument{}
	typedPackages := map[string]bool{}
	callCount := 0

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path == root {
				return nil
			}
			if strings.HasPrefix(entry.Name(), ".") || entry.Name() == "vendor" || entry.Name() == "testdata" {
				return filepath.SkipDir
			}
			// a nested module is not part of this one
			if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// these test the helpers' own dispatch, unsupported handlers included
		if filepath.Dir(path) == root && strings.HasPrefix(entry.Name(), "trace_") && strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Contains(source, []byte("HandleError")) && !bytes.Contains(source, []byte("ExpectErrorWithReturn")) {
			unparsedPaths[filepath.Dir(path)] = append(unparsedPaths[filepath.Dir(path)], path)
			return nil
		}
		file, err := parser.ParseFile(fset, path, source, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		key := filepath.Dir(path) + " " + file.Name.Name
		pkg := packages[key]
		if pkg == nil {
			pkg = &rescuePackage{dir: filepath.Dir(path), name: file.Name.Name}
			packages[key] = pkg
		}
		pkg.files = append(pkg.files, file)
		pkg.paths = append(pkg.paths, path)

		// the local names of the helper modules in this file
		moduleNames := map[string]string{}
		if filepath.Dir(path) == root && file.Name.Name == "server" {
			moduleNames[""] = "server"
		}
		for _, spec := range file.Imports {
			importPath, _ := strconv.Unquote(spec.Path.Value)
			module := ""
			switch importPath {
			case "github.com/urnetwork/server":
				module = "server"
			case "github.com/urnetwork/connect":
				module = "connect"
			default:
				continue
			}
			name := module
			if spec.Name != nil {
				name = spec.Name.Name
			}
			moduleNames[name] = module
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			fun := call.Fun
			if index, ok := fun.(*ast.IndexExpr); ok {
				fun = index.X
			} else if index, ok := fun.(*ast.IndexListExpr); ok {
				fun = index.X
			}
			qualifier, name := "", ""
			switch f := fun.(type) {
			case *ast.Ident:
				name = f.Name
			case *ast.SelectorExpr:
				x, ok := f.X.(*ast.Ident)
				if !ok {
					return true
				}
				qualifier, name = x.Name, f.Sel.Name
			default:
				return true
			}
			module, ok := moduleNames[qualifier]
			if !ok {
				return true
			}
			for _, helper := range rescueHelpers {
				if helper.module != module || helper.name != name {
					continue
				}
				callCount += 1
				if call.Ellipsis.IsValid() && helper.firstHandler < len(call.Args) {
					t.Errorf("%s: %s.%s receives a handler list it cannot be checked through; pass each handler", fset.Position(call.Pos()), module, name)
					return true
				}
				// HandleError1 and HandleError2 run only a handler whose results
				// are their own, so the results are compared as written
				var results []string
				if 0 < helper.results {
					results = rescueHelperResults(call)
				}
				for i := helper.firstHandler; i < len(call.Args); i++ {
					arg := call.Args[i]
					lit, ok := arg.(*ast.FuncLit)
					if ok && !rescueFuncLitRunnable(lit, helper.results) {
						t.Errorf("%s: %s.%s cannot run this handler: %s", fset.Position(arg.Pos()), module, name, rescueHandlerRule(helper))
						continue
					}
					if 0 < helper.results {
						switch {
						case !ok:
							t.Errorf("%s: pass each %s.%s handler as a function literal, so its results can be compared with the helper's", fset.Position(arg.Pos()), module, name)
						case results == nil:
							t.Errorf("%s: write %s.%s's type arguments or pass its do as a function literal, so the handler's results can be compared with the helper's", fset.Position(arg.Pos()), module, name)
						case !slices.Equal(rescueFuncLitResults(lit), results):
							t.Errorf("%s: %s.%s cannot run this handler: it returns (%s) and the helper returns (%s)", fset.Position(arg.Pos()), module, name, strings.Join(rescueFuncLitResults(lit), ", "), strings.Join(results, ", "))
						}
						continue
					}
					if ok {
						continue
					}
					arguments = append(arguments, rescueHandlerArgument{helper: helper, file: file, path: path, arg: arg})
					typedPackages[key] = true
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if callCount < 100 {
		t.Fatalf("found %d rescue helper calls, want the module's hundreds; the scan is not reading the module", callCount)
	}

	// Type every package that passes a handler other than a function literal,
	// against export data for its standard library imports.
	typedKeys := []string{}
	for key := range typedPackages {
		typedKeys = append(typedKeys, key)
	}
	slices.Sort(typedKeys)
	for _, key := range typedKeys {
		pkg := packages[key]
		for _, path := range unparsedPaths[pkg.dir] {
			file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			if file.Name.Name == pkg.name {
				pkg.files = append(pkg.files, file)
				pkg.paths = append(pkg.paths, path)
			}
		}
	}
	argumentFiles := map[*ast.File]bool{}
	for _, argument := range arguments {
		argumentFiles[argument.file] = true
	}
	standardImports := map[string]bool{}
	for _, key := range typedKeys {
		for _, file := range packages[key].files {
			for _, spec := range file.Imports {
				importPath, _ := strconv.Unquote(spec.Path.Value)
				if !strings.Contains(strings.Split(importPath, "/")[0], ".") && importPath != "C" {
					standardImports[importPath] = true
				}
			}
			// only the bodies around the handlers need types
			for _, decl := range file.Decls {
				if function, ok := decl.(*ast.FuncDecl); ok && function.Body != nil && !argumentFiles[file] {
					function.Body = nil
				}
			}
		}
	}
	exportPaths := map[string]string{}
	if 0 < len(standardImports) {
		listArgs := []string{"list", "-deps", "-export", "-f", "{{.ImportPath}}\t{{.Export}}"}
		for importPath := range standardImports {
			listArgs = append(listArgs, importPath)
		}
		output, err := exec.Command("go", listArgs...).Output()
		if err != nil {
			t.Fatalf("list standard library export data: %v", err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
			if importPath, exportPath, ok := strings.Cut(line, "\t"); ok && exportPath != "" {
				exportPaths[importPath] = exportPath
			}
		}
	}
	standard := importer.ForCompiler(fset, "gc", func(importPath string) (io.ReadCloser, error) {
		exportPath, ok := exportPaths[importPath]
		if !ok {
			return nil, os.ErrNotExist
		}
		return os.Open(exportPath)
	})
	emptyPackages := map[string]*types.Package{}
	imports := rescueImporterFunc(func(importPath string) (*types.Package, error) {
		if _, ok := exportPaths[importPath]; ok {
			return standard.Import(importPath)
		}
		pkg := emptyPackages[importPath]
		if pkg == nil {
			pkg = types.NewPackage(importPath, filepath.Base(importPath))
			pkg.MarkComplete()
			emptyPackages[importPath] = pkg
		}
		return pkg, nil
	})
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}}
	for _, key := range typedKeys {
		pkg := packages[key]
		config := types.Config{Importer: imports, FakeImportC: true, Error: func(error) {}}
		config.Check(pkg.dir, fset, pkg.files, info)
	}

	cancelSites := map[rescueCancelSite]int{}
	for _, argument := range arguments {
		position := fset.Position(argument.arg.Pos())
		handler := types.ExprString(argument.arg)
		typeAndValue, ok := info.Types[argument.arg]
		if !ok || typeAndValue.Type == nil || typeAndValue.Type == types.Typ[types.Invalid] {
			t.Errorf("%s: the type of handler %s cannot be resolved here; pass a function literal", position, handler)
			continue
		}
		handlerType := typeAndValue.Type
		if !rescueTypeRunnable(handlerType, argument.helper) {
			t.Errorf("%s: %s.%s cannot run handler %s of type %s: %s", position, argument.helper.module, argument.helper.name, handler, handlerType, rescueHandlerRule(argument.helper))
			continue
		}
		if argument.helper.module == "server" && argument.helper.name == "HandleError" && rescueContextCancel(handlerType) && !strings.HasSuffix(argument.path, "_test.go") {
			site := rescueCancelSite{path: filepath.ToSlash(argument.path), function: rescueEnclosingFunction(argument.file, argument.arg.Pos()), handler: handler}
			cancelSites[site] += 1
		}
	}
	for site, count := range cancelSites {
		if reviewedRescueCancelSites[site] != count {
			t.Errorf("%s %s passes %s to HandleError %d times, and the review lists %d: a cancel handler ends its owner on every raise, including a narrower context's cancellation raise; confirm the owner should end with the work and update reviewedRescueCancelSites", site.path, site.function, site.handler, count, reviewedRescueCancelSites[site])
		}
	}
	for site, count := range reviewedRescueCancelSites {
		if _, ok := cancelSites[site]; !ok {
			t.Errorf("%s %s no longer passes %s to HandleError (reviewed %d); remove it from reviewedRescueCancelSites", site.path, site.function, site.handler, count)
		}
	}
}

// Adapts a function to types.Importer.
type rescueImporterFunc func(string) (*types.Package, error)

func (self rescueImporterFunc) Import(path string) (*types.Package, error) {
	return self(path)
}

// The handler shapes the helper's type switch runs.
func rescueHandlerRule(helper rescueHelper) string {
	switch {
	case helper.name == "HandleError":
		return "pass a func(), a func(error), a context.CancelFunc or a context.CancelCauseFunc"
	case helper.results == 0:
		return "pass a func() or func(error)"
	default:
		return "pass a func() or func(error) literal returning the helper's results"
	}
}

// A function literal runs when it takes nothing or one error and returns the
// helper's number of results.
func rescueFuncLitRunnable(lit *ast.FuncLit, results int) bool {
	resultCount := 0
	if lit.Type.Results != nil {
		resultCount = lit.Type.Results.NumFields()
	}
	if resultCount != results {
		return false
	}
	params := lit.Type.Params.List
	if len(params) == 0 {
		return true
	}
	if len(params) != 1 || 1 < len(params[0].Names) {
		return false
	}
	ident, ok := params[0].Type.(*ast.Ident)
	return ok && ident.Name == "error"
}

// A typed handler runs when its signature takes nothing or one error and
// returns the helper's number of results. No helper converts a named
// function type: HandleError has cases for the two context cancels only.
func rescueTypeRunnable(handlerType types.Type, helper rescueHelper) bool {
	signature, ok := handlerType.Underlying().(*types.Signature)
	if !ok || signature.Variadic() || signature.Results().Len() != helper.results {
		return false
	}
	params := signature.Params()
	if params.Len() == 1 {
		if !types.Identical(params.At(0).Type(), types.Universe.Lookup("error").Type()) {
			return false
		}
	} else if params.Len() != 0 {
		return false
	}
	if _, named := types.Unalias(handlerType).(*types.Named); named {
		return helper.name == "HandleError" && rescueContextCancel(handlerType)
	}
	return true
}

// A function literal's result types as written, one per result.
func rescueFuncLitResults(lit *ast.FuncLit) []string {
	results := []string{}
	if lit.Type.Results == nil {
		return results
	}
	for _, field := range lit.Type.Results.List {
		for range max(1, len(field.Names)) {
			results = append(results, types.ExprString(field.Type))
		}
	}
	return results
}

// The result types of a HandleError1 or HandleError2 call as written: its
// type arguments, or else its do literal's results. Nil when the call writes
// neither.
func rescueHelperResults(call *ast.CallExpr) []string {
	switch fun := call.Fun.(type) {
	case *ast.IndexExpr:
		return []string{types.ExprString(fun.Index)}
	case *ast.IndexListExpr:
		results := []string{}
		for _, index := range fun.Indices {
			results = append(results, types.ExprString(index))
		}
		return results
	}
	if 0 < len(call.Args) {
		if do, ok := call.Args[0].(*ast.FuncLit); ok {
			return rescueFuncLitResults(do)
		}
	}
	return nil
}

// A context.CancelFunc or context.CancelCauseFunc.
func rescueContextCancel(handlerType types.Type) bool {
	named, ok := types.Unalias(handlerType).(*types.Named)
	if !ok || named.Obj().Pkg() == nil || named.Obj().Pkg().Path() != "context" {
		return false
	}
	return named.Obj().Name() == "CancelFunc" || named.Obj().Name() == "CancelCauseFunc"
}

// The top-level declaration around pos, a method as (*T).name.
func rescueEnclosingFunction(file *ast.File, pos token.Pos) string {
	for _, decl := range file.Decls {
		if decl.Pos() > pos || pos >= decl.End() {
			continue
		}
		function, ok := decl.(*ast.FuncDecl)
		if !ok {
			return "package scope"
		}
		if function.Recv != nil && 0 < len(function.Recv.List) {
			return "(" + types.ExprString(function.Recv.List[0].Type) + ")." + function.Name.Name
		}
		return function.Name.Name
	}
	return "package scope"
}
