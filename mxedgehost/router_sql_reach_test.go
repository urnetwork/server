package mxedgehost

// The SQL the router's process can reach through server's model, listed from source, for router_test.go's
// TestRouterStoresWriteNoDirectMxOrDnsTable (QD-1103, owner decision 639; QD-992).
//
// The router's packages, this one and server/cli/mxedge's main, name model only for their stores. From each model
// declaration they name, and from every method of a type such a constructor builds, since the engine calls those
// through its interfaces, the listing follows every function, method, function value, package variable and constant
// of model that a reached body or initializer names, transitively, and keeps each string such a body or initializer
// names, as a literal or a constant, that reads as SQL. Names are resolved by go/types over model's files with every
// import left empty, which resolves model's own declarations without building the module; a method called on a
// model interface reaches every model method of that name. A call into another package is not followed: what the
// stores call there is server's pools, and its partition family's hours of mail_connection_record. Every file of
// model is read whatever its build constraints, so each variant of a declaration is followed.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The import path the router's packages name model by.
const routerSqlReachModelPath = "github.com/urnetwork/server/model"

// A statement's first keyword: the server writes SQL keywords in uppercase.
var routerSqlStatementPattern = regexp.MustCompile(`^\s*(SELECT|INSERT|UPDATE|DELETE|WITH|CREATE|ALTER|DROP|TRUNCATE|LOCK|MERGE|COPY|VACUUM|ANALYZE|REINDEX|GRANT|REVOKE|CALL)\b`)

// The tables a statement writes, and whether it writes or locks at all: each write's target, a DDL statement's
// object, and any row or table lock or sequence write, which a role with SELECT alone cannot run.
var (
	routerSqlWriteTablePatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\bINSERT\s+INTO\s+([a-z0-9_."]+)`),
		regexp.MustCompile(`(?i)\bUPDATE\s+(?:ONLY\s+)?([a-z0-9_."]+)\s+SET\b`),
		regexp.MustCompile(`(?i)\bDELETE\s+FROM\s+(?:ONLY\s+)?([a-z0-9_."]+)`),
		regexp.MustCompile(`(?i)\bTRUNCATE\s+(?:TABLE\s+)?([a-z0-9_."]+)`),
		regexp.MustCompile(`(?i)\bMERGE\s+INTO\s+([a-z0-9_."]+)`),
		regexp.MustCompile(`(?i)\b(?:CREATE|ALTER|DROP)\s+(?:UNLOGGED\s+)?(?:TABLE|INDEX|VIEW|SEQUENCE|FUNCTION|TRIGGER|TYPE|SCHEMA)\s+(?:IF\s+(?:NOT\s+)?EXISTS\s+)?([a-z0-9_."]*)`),
	}
	routerSqlWritePattern = regexp.MustCompile(`(?i)\b(?:INSERT|UPDATE|DELETE|TRUNCATE|MERGE|CREATE|ALTER|DROP|GRANT|REVOKE|COPY|LOCK)\b|\bFOR\s+(?:NO\s+KEY\s+)?UPDATE\b|\bFOR\s+(?:KEY\s+)?SHARE\b|\b(?:nextval|setval)\s*\(`)
)

// Whether a table is the direct MX's or one of the zone's, which only api and the dns block write (D-OPS-1).
func routerSqlZoneTable(table string) bool {
	table = strings.Trim(strings.ToLower(table), `"`)
	table = strings.TrimPrefix(table, "public.")
	return table == "mail_direct_mx" || strings.HasPrefix(table, "dns_") || strings.HasPrefix(table, "network_dns_")
}

// Whether a statement only reads: it starts with SELECT or WITH and holds no write, lock or sequence write.
func routerSqlReadOnly(statement string) bool {
	keyword := routerSqlStatementPattern.FindStringSubmatch(statement)
	return keyword != nil && (keyword[1] == "SELECT" || keyword[1] == "WITH") && !routerSqlWritePattern.MatchString(statement)
}

// The tables a statement writes, "" for a write whose table the statement does not name.
func routerSqlWriteTables(statement string) []string {
	tables := []string{}
	for _, pattern := range routerSqlWriteTablePatterns {
		for _, match := range pattern.FindAllStringSubmatch(statement, -1) {
			tables = append(tables, strings.ToLower(match[1]))
		}
	}
	return tables
}

// model's declarations by name, with go/types' resolution of every name its files use.
type routerSqlReachModel struct {
	pkg  *types.Package
	info *types.Info
	// package-level functions, every variant
	funcs map[string][]*ast.FuncDecl
	// "Type.Method" -> its declarations; and every method by its name alone
	methods       map[string][]*ast.FuncDecl
	methodsByName map[string][]*ast.FuncDecl
	// package-level variables' and constants' initializers by name
	values map[string][]ast.Expr
	// a method's declaration -> its receiver's type name
	receivers map[*ast.FuncDecl]string
}

// The receiver type's name of a method declaration: T for T, *T, T[...] and *T[...].
func routerSqlReceiverName(decl *ast.FuncDecl) string {
	if decl.Recv == nil || len(decl.Recv.List) == 0 {
		return ""
	}
	expr := decl.Recv.List[0].Type
	for {
		switch e := expr.(type) {
		case *ast.StarExpr:
			expr = e.X
		case *ast.IndexExpr:
			expr = e.X
		case *ast.IndexListExpr:
			expr = e.X
		case *ast.Ident:
			return e.Name
		default:
			return ""
		}
	}
}

// Parses every Go file of model but its tests, and resolves their names with every import empty.
func loadRouterSqlReachModel(t *testing.T, directory string) *routerSqlReachModel {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	files := []*ast.File{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(directory, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		if file.Name.Name != "model" {
			continue
		}
		files = append(files, file)
	}
	if len(files) == 0 {
		t.Fatalf("no model file in %s", directory)
	}
	emptyPackages := map[string]*types.Package{}
	config := types.Config{
		Importer: routerSqlReachImporter(func(importPath string) (*types.Package, error) {
			pkg := emptyPackages[importPath]
			if pkg == nil {
				pkg = types.NewPackage(importPath, filepath.Base(importPath))
				pkg.MarkComplete()
				emptyPackages[importPath] = pkg
			}
			return pkg, nil
		}),
		FakeImportC: true,
		// every name from an import is unresolved; model's own still resolve
		Error: func(error) {},
	}
	info := &types.Info{
		Defs:       map[*ast.Ident]types.Object{},
		Uses:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
	}
	pkg, _ := config.Check(routerSqlReachModelPath, fset, files, info)
	if pkg == nil {
		t.Fatal("model did not type-check at all")
	}
	self := &routerSqlReachModel{
		pkg:           pkg,
		info:          info,
		funcs:         map[string][]*ast.FuncDecl{},
		methods:       map[string][]*ast.FuncDecl{},
		methodsByName: map[string][]*ast.FuncDecl{},
		values:        map[string][]ast.Expr{},
		receivers:     map[*ast.FuncDecl]string{},
	}
	for _, file := range files {
		for _, decl := range file.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				if decl.Recv == nil {
					self.funcs[decl.Name.Name] = append(self.funcs[decl.Name.Name], decl)
					continue
				}
				receiver := routerSqlReceiverName(decl)
				self.receivers[decl] = receiver
				self.methods[receiver+"."+decl.Name.Name] = append(self.methods[receiver+"."+decl.Name.Name], decl)
				self.methodsByName[decl.Name.Name] = append(self.methodsByName[decl.Name.Name], decl)
			case *ast.GenDecl:
				if decl.Tok != token.VAR && decl.Tok != token.CONST {
					continue
				}
				for _, spec := range decl.Specs {
					valueSpec := spec.(*ast.ValueSpec)
					for index, name := range valueSpec.Names {
						switch {
						case len(valueSpec.Values) == len(valueSpec.Names):
							self.values[name.Name] = append(self.values[name.Name], valueSpec.Values[index])
						case len(valueSpec.Values) == 1:
							// one call answering every name
							self.values[name.Name] = append(self.values[name.Name], valueSpec.Values[0])
						}
					}
				}
			}
		}
	}
	return self
}

// Adapts a function to types.Importer.
type routerSqlReachImporter func(string) (*types.Package, error)

func (self routerSqlReachImporter) Import(importPath string) (*types.Package, error) {
	return self(importPath)
}

// One listing's walk from its roots.
type routerSqlReach struct {
	model *routerSqlReachModel
	// what the walk reached: declarations, and package values by name
	reachedDecls  map[*ast.FuncDecl]bool
	reachedValues map[string]bool
	pending       []ast.Node
	// every string a reached body or initializer names that reads as SQL, by its text
	statements map[string]bool
}

func newRouterSqlReach(model *routerSqlReachModel) *routerSqlReach {
	return &routerSqlReach{
		model:         model,
		reachedDecls:  map[*ast.FuncDecl]bool{},
		reachedValues: map[string]bool{},
		statements:    map[string]bool{},
	}
}

func (self *routerSqlReach) reachDecls(decls []*ast.FuncDecl) {
	for _, decl := range decls {
		if self.reachedDecls[decl] {
			continue
		}
		self.reachedDecls[decl] = true
		self.pending = append(self.pending, decl)
	}
}

func (self *routerSqlReach) reachValue(name string) {
	if self.reachedValues[name] {
		return
	}
	self.reachedValues[name] = true
	for _, value := range self.model.values[name] {
		self.pending = append(self.pending, value)
	}
}

// Reaches what obj declares, when model declares it at package level or as a method.
func (self *routerSqlReach) reachObject(obj types.Object) {
	if obj == nil || obj.Pkg() != self.model.pkg {
		return
	}
	switch obj := obj.(type) {
	case *types.Func:
		signature, _ := obj.Type().(*types.Signature)
		if signature == nil || signature.Recv() == nil {
			if obj.Parent() == self.model.pkg.Scope() {
				self.reachDecls(self.model.funcs[obj.Name()])
			}
			return
		}
		receiver := signature.Recv().Type()
		if pointer, ok := receiver.(*types.Pointer); ok {
			receiver = pointer.Elem()
		}
		if types.IsInterface(receiver) {
			// any model type's method of the name may answer the call
			self.reachDecls(self.model.methodsByName[obj.Name()])
			return
		}
		if named, ok := receiver.(*types.Named); ok {
			self.reachDecls(self.model.methods[named.Obj().Name()+"."+obj.Name()])
		}
	case *types.Var, *types.Const:
		if obj.Parent() == self.model.pkg.Scope() {
			self.reachValue(obj.Name())
		}
	}
}

// Reaches a model declaration by name: a package-level function, or Type.Method.
func (self *routerSqlReach) reachName(name string) bool {
	if decls, ok := self.model.funcs[name]; ok {
		self.reachDecls(decls)
		return true
	}
	if decls, ok := self.model.methods[name]; ok {
		self.reachDecls(decls)
		return true
	}
	return false
}

// Reaches every method of the model types a constructor builds by composite literal in its own body.
func (self *routerSqlReach) reachBuiltTypes(constructor string) []string {
	typeNames := []string{}
	for _, decl := range self.model.funcs[constructor] {
		ast.Inspect(decl.Body, func(node ast.Node) bool {
			literal, ok := node.(*ast.CompositeLit)
			if !ok {
				return true
			}
			ident, ok := literal.Type.(*ast.Ident)
			if !ok {
				return true
			}
			if _, isType := self.model.info.Uses[ident].(*types.TypeName); isType && !slices.Contains(typeNames, ident.Name) {
				typeNames = append(typeNames, ident.Name)
			}
			return true
		})
	}
	for _, typeName := range typeNames {
		for decl, receiver := range self.model.receivers {
			if receiver == typeName {
				self.reachDecls([]*ast.FuncDecl{decl})
			}
		}
	}
	slices.Sort(typeNames)
	return typeNames
}

// Walks everything pending to the end, and keeps the statements found.
func (self *routerSqlReach) run() {
	for 0 < len(self.pending) {
		node := self.pending[len(self.pending)-1]
		self.pending = self.pending[:len(self.pending)-1]
		ast.Inspect(node, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.BasicLit:
				if node.Kind == token.STRING {
					if text, err := strconv.Unquote(node.Value); err == nil && routerSqlStatementPattern.MatchString(text) {
						self.statements[strings.Join(strings.Fields(text), " ")] = true
					}
				}
			case *ast.Ident:
				self.reachObject(self.model.info.Uses[node])
			}
			return true
		})
	}
}

// The statements found, sorted.
func (self *routerSqlReach) sortedStatements() []string {
	statements := []string{}
	for statement := range self.statements {
		statements = append(statements, statement)
	}
	slices.Sort(statements)
	return statements
}

// What the router's packages name of model: each selector on model's import in this package's files but its tests,
// and in server/cli/mxedge's main.
func routerModelNames(t *testing.T) []string {
	t.Helper()
	paths := []string{filepath.Join("..", "cli", "mxedge", "main.go")}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".go") && !strings.HasSuffix(entry.Name(), "_test.go") {
			paths = append(paths, entry.Name())
		}
	}
	names := []string{}
	for _, path := range paths {
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		importName := ""
		for _, spec := range file.Imports {
			if importPath, _ := strconv.Unquote(spec.Path.Value); importPath == routerSqlReachModelPath {
				importName = "model"
				if spec.Name != nil {
					importName = spec.Name.Name
				}
			}
		}
		if importName == "" {
			continue
		}
		ast.Inspect(file, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if ident, ok := selector.X.(*ast.Ident); ok && ident.Name == importName && !slices.Contains(names, selector.Sel.Name) {
				names = append(names, selector.Sel.Name)
			}
			return true
		})
	}
	slices.Sort(names)
	return names
}
