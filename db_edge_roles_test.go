//go:build sidecar_pending

// The edge blocks' restricted Postgres roles (QD-992, QD-029; D-OPS-1): server/db/roles/<service>.sql, each run here
// exactly as the owner runs it, then the block's calls under the role on the vault its warp fragment mounts, which
// holds the role's pg-<service>.yml and nothing of the platform's, so the main pool connects as the role and the
// maintenance pool connects as nothing.
//
// Each test holds a file to its block's code twice. It runs the block's store calls under the role, which must pass,
// and statements outside the grants, which must be refused with 42501. And it lists every SQL statement the block's
// model code reaches, from the model names the block's own files name, and asks Postgres to plan each statement the
// block runs under the role (EXPLAIN (GENERIC_PLAN), which checks every table's and column's privilege and runs
// nothing), so a query added to a store fails here until the file grants it. Every declaration holding SQL that the
// listing reaches is named below, as one the block runs under its role or as one it never runs there, with why.
//
// Roles are the cluster's, not the test database's, so each test makes its own, <service>_t_<random>, and drops it.
// Only the private test stack is dialed (server/test-env.sh).
package server_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/mxedgeservice"
	"github.com/urnetwork/sidecar/smtpedgeservice"
	"github.com/urnetwork/sidecar/tlsedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

// Postgres' insufficient_privilege.
const edgeRoleRefusedCode = "42501"

// The import path the blocks name model by.
const edgeRoleModelPath = "github.com/urnetwork/server/model"

// One block's role and what its process reaches of model.
type edgeRoleBlock struct {
	// The warp service, which names the role, its file and its vault file.
	service string
	// The block's own Go files, relative to server: what its process names of model is read from them.
	files []string
	// Each model name the files name, and the roots it brings the process to: a function's or a value's name,
	// Type.Method, or new:Constructor, whose built types' methods all run. A name with no root reaches no SQL.
	names map[string][]string
	// Each declaration holding SQL that the block runs on its main pool, under its role, and the call of this test
	// that runs it there.
	runs map[string]string
	// Each declaration holding SQL that the listing reaches but the block never runs under its role, and why.
	notRun map[string]string
}

// tlsedge (server/cli/tlsedge and server/tlsedgehost): its store alone.
var edgeRoleTlsedge = &edgeRoleBlock{
	service: "tlsedge",
	files:   append(edgeRoleGoFiles("cli/tlsedge"), edgeRoleGoFiles("tlsedgehost")...),
	names: map[string][]string{
		"NewTlsEdgeStore": {"new:NewTlsEdgeStore"},
	},
	runs: map[string]string{
		"readDnsLabelMap":             "NameSnapshot and NamesSince",
		"getDnsRootStateInTx":         "NameSnapshot and NamesSince",
		"getDnsCustomHostRowsInTx":    "CustomHosts, whose where clause is DnsCustomHostActiveRowsInTx's",
		"GetTlsSuspensions":           "Suspensions",
		"writeTlsConnectionRecordsAt": "InsertConnectionRecords",
	},
	notRun: map[string]string{
		"tlsConnectionTablePartitioned": "the partition family's Partitioned hook, on the maintenance pool, which the block does not have: the runway's and the retirement's in taskworker, and EnsureRangeFor's in a batch whose hour has no partition, which fails",
		"connectionRecordRangeHold":     "the partition family's DropPrecondition: the retirement's, in taskworker",
		"AbuseHoldCopyPending":          "the partition family's DropPrecondition: the retirement's, in taskworker",
		"unregisteredHoldCopyPending":   "the partition family's DropPrecondition: the retirement's, in taskworker",
		"rollUpTlsConnectionHourInTx":   "the partition family's DropWork, the roll-up into tls_connection_day: the retirement's, in taskworker",
	},
}

// mxedge's router (server/cli/mxedge --role router, and server/mxedgehost): its two stores. cli/mxedge's other files
// are the platform listener's (--role platform, the block mxedge-platform, on pg.yml), which the router never runs:
// report_intake.go builds the abuse sink there.
var edgeRoleMxedge = &edgeRoleBlock{
	service: "mxedge",
	files:   append([]string{"cli/mxedge/main.go"}, edgeRoleGoFiles("mxedgehost")...),
	names: map[string][]string{
		"NewMxEdgeStore":     {"new:NewMxEdgeStore"},
		"NewMxDirectMxStore": {"new:NewMxDirectMxStore"},
	},
	runs: map[string]string{
		"writeMailConnectionRecordsAt":         "InsertConnectionRecords",
		"mailConnectionTablePartitionedInConn": "Ping, on the main pool (QD-1133)",
		"mxDirectMxStore.ListDirectMx":         "ListDirectMx",
		"getNetworkLabelInTx":                  "ListDirectMx",
		"mailDirectMxPublishedKeysWithQuery":   "ListDirectMx",
	},
	notRun: map[string]string{
		"mailConnectionTablePartitioned": "the partition family's Partitioned hook, on the maintenance pool, which the block does not have: the runway's and the retirement's in taskworker",
		"connectionRecordRangeHold":      "the partition family's DropPrecondition: the retirement's, in taskworker",
		"AbuseHoldCopyPending":           "the partition family's DropPrecondition: the retirement's, in taskworker",
		"unregisteredHoldCopyPending":    "the partition family's DropPrecondition: the retirement's, in taskworker",
	},
}

// smtpedge (server/cli/smtpedge and server/smtpedgehost): its store as the engine calls it in this process, the send
// gate's facts and the mail breaker (gate.go), §21.7's abuse gate, and the abuse sink's suspension request. The
// store's other methods are other processes': complaints, aggregates, DKIM keys and the join are the report
// intake's (mxedge-platform), the reconcile is taskworker's, a halt's lift is api's appeal, and SendingClients is the
// status hints', which need a Push this block's host is not given. The sink's OpenSmtpCase is the report intake's,
// and a ladder's only for a complaint event, which smtpedge's ladder is never given (it judges cuts and hard
// bounces), so it is no root here, and the test holds that the role refuses it.
var edgeRoleSmtpedge = &edgeRoleBlock{
	service: "smtpedge",
	files:   append(edgeRoleGoFiles("cli/smtpedge"), edgeRoleGoFiles("smtpedgehost")...),
	names: map[string][]string{
		"NewSmtpEdgeStore": {
			"NewSmtpEdgeStore",
			"SmtpEdgeStore.ActiveHalts",
			"SmtpEdgeStore.InsertSends",
			"SmtpEdgeStore.FindHalts",
			"SmtpEdgeStore.CountSends",
			"SmtpEdgeStore.RecipientCount",
			"SmtpEdgeStore.SetHalt",
		},
		"SmtpEdgeStore":            {},
		"NewAbuseGate":             {"NewAbuseGate", "new:newAbuseGate"},
		"NewAbuseHardDenySnapshot": {"NewAbuseHardDenySnapshot", "new:newAbuseHardDenySnapshot"},
		"GetSmtpGateFacts":         {"GetSmtpGateFacts"},
		"GetMailBreakerState":      {"GetMailBreakerState"},
		"Pro":                      {"Pro"},
		"NetworkTierFree":          {},
		"NewAbuseSmtpSink":         {"NewAbuseSmtpSink", "abuseSmtpSink.RequestSmtpSuspension"},
		// the X-UR-Ref keys, from the block's vault: no SQL
		"LoadSmtpXUrRefKeyFile": {"LoadSmtpXUrRefKeyFile"},
	},
	runs: map[string]string{
		"GetActiveSmtpHalts":              "ActiveHalts",
		"FindSmtpHalts":                   "FindHalts",
		"SetSmtpHaltInTx":                 "SetHalt",
		"insertSmtpSendsAt":               "InsertSends",
		"CountSmtpSends":                  "CountSends",
		"SmtpRecipientCount":              "RecipientCount",
		"GetSmtpGateFactsWithSettings":    "GetSmtpGateFactsWithSettings",
		"getNetworkLabelInTx":             "GetSmtpGateFactsWithSettings",
		"getNetworkLiveAliasesInTx":       "GetSmtpGateFactsWithSettings",
		"GetNetworkVerifiedDomains":       "GetSmtpGateFactsWithSettings",
		"getDnsCustomHostRowsInTx":        "GetSmtpGateFactsWithSettings with strict_envelope on, DnsStrictMxHostRows' where clause",
		"NetworkDowngradeTime":            "GetSmtpGateFactsWithSettings",
		"loadNetworkTierWithConn":         "GetSmtpGateFactsWithSettings, through NetworkDowngradeTime",
		"loadProNetworkWithConn":          "loadNetworkTierWithConn in a binary without MG6, planned here",
		"GetMailBreakerRows":              "GetMailBreakerState",
		"loadAbuseHardDeny":               "the hard-deny snapshot's load",
		"loadNetworkAbuseFactsWithConn":   "the abuse gate's load, whose columns and joins its binary's groups choose",
		"loadNetworkAbuseTrustedWithConn": "the abuse gate's load",

		"GetNetworkScreenAllowedNamesInTx": "GetSmtpGateFactsWithSettings",
	},
	notRun: map[string]string{
		"smtpSendTablePartitioned":    "the partition family's Partitioned hook, on the maintenance pool, which the block does not have: the runway's and the retirement's in taskworker, and EnsureRangeFor's in a batch whose day has no partition, which fails",
		"connectionRecordRangeHold":   "the partition family's DropPrecondition: the retirement's, in taskworker",
		"AbuseHoldCopyPending":        "the partition family's DropPrecondition: the retirement's, in taskworker",
		"unregisteredHoldCopyPending": "the partition family's DropPrecondition: the retirement's, in taskworker",
	},
}

// The Go files of a package directory of server, but its tests.
func edgeRoleGoFiles(directory string) []string {
	entries, err := os.ReadDir(directory)
	if err != nil {
		panic(err)
	}
	files := []string{}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() && strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			files = append(files, filepath.Join(directory, name))
		}
	}
	return files
}

// What a block's files name of model: each selector on model's import.
func edgeRoleModelNames(t testing.TB, files []string) []string {
	t.Helper()
	names := []string{}
	for _, path := range files {
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		importName := ""
		for _, spec := range file.Imports {
			if importPath, _ := strconv.Unquote(spec.Path.Value); importPath == edgeRoleModelPath {
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
			if selector, ok := node.(*ast.SelectorExpr); ok {
				if ident, ok := selector.X.(*ast.Ident); ok && ident.Name == importName && !slices.Contains(names, selector.Sel.Name) {
					names = append(names, selector.Sel.Name)
				}
			}
			return true
		})
	}
	slices.Sort(names)
	return names
}

// A statement's first keyword: the server writes SQL keywords in uppercase.
var edgeRoleStatementPattern = regexp.MustCompile(`^\s*(SELECT|INSERT|UPDATE|DELETE|WITH|CREATE|ALTER|DROP|TRUNCATE|LOCK|MERGE|COPY|VACUUM|ANALYZE|REINDEX|GRANT|REVOKE|CALL|SET|SHOW)\b`)

// model's declarations by name, with go/types' resolution of every name its files use, every import left empty:
// that resolves model's own names without building the module, as mxedgehost's router_sql_reach_test.go reads it.
// Every file of model is read whatever its build constraints, so each variant of a declaration is followed.
type edgeRoleModel struct {
	pkg  *types.Package
	info *types.Info
	// package-level functions, every variant
	funcs map[string][]*ast.FuncDecl
	// "Type.Method" -> its declarations, and every method by its name alone
	methods       map[string][]*ast.FuncDecl
	methodsByName map[string][]*ast.FuncDecl
	// package-level variables' and constants' initializers by name
	values map[string][]ast.Expr
}

type edgeRoleImporter func(string) (*types.Package, error)

func (self edgeRoleImporter) Import(importPath string) (*types.Package, error) {
	return self(importPath)
}

// A method's declared name, Type.Method, or a function's.
func edgeRoleDeclName(decl *ast.FuncDecl) string {
	if decl.Recv == nil || len(decl.Recv.List) == 0 {
		return decl.Name.Name
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
			return e.Name + "." + decl.Name.Name
		default:
			return decl.Name.Name
		}
	}
}

func loadEdgeRoleModel(t testing.TB) *edgeRoleModel {
	t.Helper()
	entries, err := os.ReadDir("model")
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
		file, err := parser.ParseFile(fset, filepath.Join("model", name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		if file.Name.Name == "model" {
			files = append(files, file)
		}
	}
	emptyPackages := map[string]*types.Package{}
	config := types.Config{
		Importer: edgeRoleImporter(func(importPath string) (*types.Package, error) {
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
		Types: map[ast.Expr]types.TypeAndValue{},
		Uses:  map[*ast.Ident]types.Object{},
	}
	pkg, _ := config.Check(edgeRoleModelPath, fset, files, info)
	if pkg == nil {
		t.Fatal("model did not type-check at all")
	}
	self := &edgeRoleModel{
		pkg:           pkg,
		info:          info,
		funcs:         map[string][]*ast.FuncDecl{},
		methods:       map[string][]*ast.FuncDecl{},
		methodsByName: map[string][]*ast.FuncDecl{},
		values:        map[string][]ast.Expr{},
	}
	for _, file := range files {
		for _, decl := range file.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				if decl.Recv == nil {
					self.funcs[decl.Name.Name] = append(self.funcs[decl.Name.Name], decl)
					continue
				}
				name := edgeRoleDeclName(decl)
				self.methods[name] = append(self.methods[name], decl)
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

// One declaration's node waiting to be walked.
type edgeRolePending struct {
	node ast.Node
	name string
}

// One listing's walk from its roots: the declarations reached and, by declaration, the SQL each holds.
type edgeRoleReach struct {
	model         *edgeRoleModel
	reachedDecls  map[*ast.FuncDecl]bool
	reachedValues map[string]bool
	pending       []edgeRolePending
	// every declaration reached, by name
	reached map[string]bool
	// by declaration: each statement it holds whole, as written
	statements map[string][]string
	// by declaration: each statement it builds at run time, its run-time parts written <?>
	built map[string][]string
	// by declaration: what it names of server's own package
	serverNames map[string][]string
}

func newEdgeRoleReach(model *edgeRoleModel) *edgeRoleReach {
	return &edgeRoleReach{
		model:         model,
		reachedDecls:  map[*ast.FuncDecl]bool{},
		reachedValues: map[string]bool{},
		reached:       map[string]bool{},
		statements:    map[string][]string{},
		built:         map[string][]string{},
		serverNames:   map[string][]string{},
	}
}

func (self *edgeRoleReach) reachDecls(decls []*ast.FuncDecl) {
	for _, decl := range decls {
		if self.reachedDecls[decl] {
			continue
		}
		self.reachedDecls[decl] = true
		name := edgeRoleDeclName(decl)
		self.reached[name] = true
		self.pending = append(self.pending, edgeRolePending{node: decl, name: name})
	}
}

func (self *edgeRoleReach) reachValue(name string) {
	if self.reachedValues[name] {
		return
	}
	self.reachedValues[name] = true
	self.reached[name] = true
	for _, value := range self.model.values[name] {
		self.pending = append(self.pending, edgeRolePending{node: value, name: name})
	}
}

// Reaches what obj declares, when model declares it at package level or as a method. A call through a model
// interface reaches every model method of the name.
func (self *edgeRoleReach) reachObject(obj types.Object) {
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

// Reaches one root: a function's or a value's name, Type.Method, or new:Constructor with every method of the model
// types it builds by composite literal in its own body. False when model declares no such thing.
func (self *edgeRoleReach) reachRoot(root string) bool {
	if constructor, ok := strings.CutPrefix(root, "new:"); ok {
		decls := self.model.funcs[constructor]
		if len(decls) == 0 {
			return false
		}
		self.reachDecls(decls)
		typeNames := []string{}
		for _, decl := range decls {
			ast.Inspect(decl.Body, func(node ast.Node) bool {
				if literal, ok := node.(*ast.CompositeLit); ok {
					if ident, ok := literal.Type.(*ast.Ident); ok {
						if _, isType := self.model.info.Uses[ident].(*types.TypeName); isType {
							typeNames = append(typeNames, ident.Name)
						}
					}
				}
				return true
			})
		}
		if len(typeNames) == 0 {
			return false
		}
		for name, decls := range self.model.methods {
			if typeName, _, _ := strings.Cut(name, "."); slices.Contains(typeNames, typeName) {
				self.reachDecls(decls)
			}
		}
		return true
	}
	if decls, ok := self.model.methods[root]; ok {
		self.reachDecls(decls)
		return true
	}
	if decls, ok := self.model.funcs[root]; ok {
		self.reachDecls(decls)
		return true
	}
	if _, ok := self.model.values[root]; ok {
		self.reachValue(root)
		return true
	}
	return false
}

// The text of a + chain or a format, each part that is not a constant string written <?>, with every node it
// read marked consumed so its parts are not taken again as statements of their own.
func (self *edgeRoleReach) buildText(expr ast.Expr, consumed map[ast.Node]bool) string {
	if typeAndValue, ok := self.model.info.Types[expr]; ok && typeAndValue.Value != nil && typeAndValue.Value.Kind() == constant.String {
		ast.Inspect(expr, func(node ast.Node) bool {
			consumed[node] = true
			return true
		})
		return constant.StringVal(typeAndValue.Value)
	}
	switch e := expr.(type) {
	case *ast.ParenExpr:
		return self.buildText(e.X, consumed)
	case *ast.BinaryExpr:
		if e.Op == token.ADD {
			consumed[e] = true
			return self.buildText(e.X, consumed) + self.buildText(e.Y, consumed)
		}
	}
	return "<?>"
}

// Keeps text when it reads as SQL: whole, or built at run time.
func (self *edgeRoleReach) addStatement(name string, text string, built bool) {
	if !edgeRoleStatementPattern.MatchString(text) {
		return
	}
	statements := self.statements
	if built || strings.Contains(text, "<?>") {
		statements = self.built
	}
	if !slices.Contains(statements[name], text) {
		statements[name] = append(statements[name], text)
	}
}

// Walks everything pending to the end.
func (self *edgeRoleReach) run() {
	consumed := map[ast.Node]bool{}
	for 0 < len(self.pending) {
		pending := self.pending[len(self.pending)-1]
		self.pending = self.pending[:len(self.pending)-1]
		ast.Inspect(pending.node, func(node ast.Node) bool {
			if node == nil || consumed[node] {
				return node != nil
			}
			switch node := node.(type) {
			case *ast.BinaryExpr:
				if node.Op == token.ADD {
					self.addStatement(pending.name, self.buildText(node, consumed), false)
				}
			case *ast.CallExpr:
				// a statement formatted at run time: its format, with each verb a run-time part
				if selector, ok := node.Fun.(*ast.SelectorExpr); ok && 0 < len(node.Args) {
					if ident, ok := selector.X.(*ast.Ident); ok {
						if pkgName, ok := self.model.info.Uses[ident].(*types.PkgName); ok && pkgName.Imported().Path() == "fmt" && strings.HasSuffix(selector.Sel.Name, "printf") {
							self.addStatement(pending.name, self.buildText(node.Args[0], consumed), true)
						}
					}
				}
			case *ast.BasicLit:
				if node.Kind == token.STRING {
					if text, err := strconv.Unquote(node.Value); err == nil {
						self.addStatement(pending.name, text, false)
					}
				}
			case *ast.SelectorExpr:
				if ident, ok := node.X.(*ast.Ident); ok {
					if pkgName, ok := self.model.info.Uses[ident].(*types.PkgName); ok && pkgName.Imported().Path() == "github.com/urnetwork/server" {
						if !slices.Contains(self.serverNames[pending.name], node.Sel.Name) {
							self.serverNames[pending.name] = append(self.serverNames[pending.name], node.Sel.Name)
						}
					}
				}
			case *ast.Ident:
				self.reachObject(self.model.info.Uses[node])
			}
			return true
		})
	}
}

// Every declaration the walk reached that holds SQL, whole or built.
func (self *edgeRoleReach) sqlDeclarations() []string {
	names := []string{}
	for name := range self.statements {
		names = append(names, name)
	}
	for name := range self.built {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// The block's listing: its files' model names, each name's roots, walked to the end. Fails when the files name
// something of model the block does not account for, or a root model does not declare.
func reachEdgeRoleBlock(t testing.TB, block *edgeRoleBlock) *edgeRoleReach {
	t.Helper()
	names := edgeRoleModelNames(t, block.files)
	expected := []string{}
	for name := range block.names {
		expected = append(expected, name)
	}
	slices.Sort(expected)
	if !slices.Equal(names, expected) {
		t.Fatalf("%s's files name model's %v, and the test accounts for %v: list what each new name reaches", block.service, names, expected)
	}
	reach := newEdgeRoleReach(loadEdgeRoleModel(t))
	for _, name := range expected {
		for _, root := range block.names[name] {
			if !reach.reachRoot(root) {
				t.Fatalf("%s: model declares no %s", block.service, root)
			}
		}
	}
	reach.run()
	return reach
}

// Holds the listing to the block's lists: every declaration holding SQL is one it runs under its role or one it
// never runs there; no list names a declaration the listing does not reach with SQL; and nothing it runs reads the
// maintenance pool, while every reached declaration that does is listed as never run, since the block has none.
func checkEdgeRoleReachLists(t testing.TB, block *edgeRoleBlock, reach *edgeRoleReach) {
	t.Helper()
	sqlDeclarations := reach.sqlDeclarations()
	for _, name := range sqlDeclarations {
		_, runs := block.runs[name]
		_, notRun := block.notRun[name]
		switch {
		case runs && notRun:
			t.Errorf("%s: %s is listed both as run and as never run", block.service, name)
		case !runs && !notRun:
			t.Errorf("%s reaches SQL in %s: grant what it runs in db/roles/%s.sql and list it in runs, or say in notRun why the block never runs it: %q %q",
				block.service, name, block.service, reach.statements[name], reach.built[name])
		}
	}
	// what names server's maintenance pool, MaintenanceDb or MaintenanceTx
	maintenanceDeclarations := []string{}
	for name, serverNames := range reach.serverNames {
		if slices.ContainsFunc(serverNames, func(serverName string) bool { return strings.HasPrefix(serverName, "Maintenance") }) {
			maintenanceDeclarations = append(maintenanceDeclarations, name)
		}
	}
	for _, name := range maintenanceDeclarations {
		if _, runs := block.runs[name]; runs {
			t.Errorf("%s runs %s, which reads the maintenance pool, which the block does not have", block.service, name)
		} else if _, notRun := block.notRun[name]; !notRun {
			t.Errorf("%s reaches %s, which reads the maintenance pool, which the block does not have: list it as never run, with why", block.service, name)
		}
	}
	for name := range block.runs {
		if !slices.Contains(sqlDeclarations, name) {
			t.Errorf("%s's runs name %s, which the listing does not reach with SQL", block.service, name)
		}
	}
	for name := range block.notRun {
		if !slices.Contains(sqlDeclarations, name) && !slices.Contains(maintenanceDeclarations, name) {
			t.Errorf("%s's notRun names %s, which the listing reaches with neither SQL nor the maintenance pool", block.service, name)
		}
	}
}

// The private test stack's Postgres container, where each role file runs through psql as the cluster's superuser,
// as the owner runs it as an admin: the platform's test role may not create roles. sidecarrestore's FS-42 fixture
// reaches the private server the same way, and nothing here reaches any other.
const edgeRoleServerContainer = "sidecar-test-pg"

// The longest one psql run here may take.
const edgeRolePsqlTimeout = 2 * time.Minute

// Runs script through psql in the private server's container, as its superuser, connected to database: psql's own
// meta-commands and variables, stopping at the first error.
func edgeRolePsql(ctx context.Context, database string, script string) (string, error) {
	psqlCtx, cancel := context.WithTimeout(ctx, edgeRolePsqlTimeout)
	defer cancel()
	command := exec.CommandContext(psqlCtx, "docker", "exec", "-i", edgeRoleServerContainer,
		"psql", "-U", "postgres", "-X", "-q", "-v", "ON_ERROR_STOP=1", "-d", database, "-f", "-")
	command.Stdin = strings.NewReader(script)
	output, err := command.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

// A role made from its file for one test, and the vault its block mounts.
type edgeRoleSession struct {
	block    *edgeRoleBlock
	role     string
	password string
	// the test's database on the private test stack, as the test's pg.yml names it
	authority string
	db        string
	vaultHome string
}

// A random lowercase hex string of byteCount bytes.
func edgeRoleRandomHex(t testing.TB, byteCount int) string {
	bytes := make([]byte, byteCount)
	if _, err := rand.Read(bytes); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(bytes)
}

// Makes the block's role from exactly its file, run by psql in the test's database, and writes the vault its
// fragment mounts: pg-<service>.yml and nothing else. Two things differ from the owner's run, each the least the test
// needs: the role is named <service>_t_<random>, since roles are the cluster's and tests run side by side, and
// \password, which asks a terminal, is what it sends, an ALTER ROLE with the test's own password. Any other
// meta-command fails the test, which would not know what psql does with it. The caller drops the role (drop)
// before its test's database goes.
func newEdgeRoleSession(t testing.TB, ctx context.Context, block *edgeRoleBlock) *edgeRoleSession {
	t.Helper()
	pg := server.Vault.RequireSimpleResource("pg.yml")
	self := &edgeRoleSession{
		block:     block,
		role:      fmt.Sprintf("%s_t_%s", block.service, edgeRoleRandomHex(t, 6)),
		password:  edgeRoleRandomHex(t, 24),
		authority: pg.RequireString("authority"),
		db:        pg.RequireString("db"),
		vaultHome: t.TempDir(),
	}
	text, err := os.ReadFile(filepath.Join("db", "roles", block.service+".sql"))
	if err != nil {
		t.Fatal(err)
	}
	rolePattern := regexp.MustCompile(`\b` + regexp.QuoteMeta(block.service) + `\b`)
	lines := []string{}
	passwordSet := false
	for _, line := range strings.Split(string(text), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case !strings.HasPrefix(trimmed, `\`):
		case trimmed == `\set ON_ERROR_STOP on`:
		case trimmed == `\password `+block.service && !passwordSet:
			// what psql's \password sends: the role's password, here the test's own
			line = fmt.Sprintf(`ALTER ROLE %s PASSWORD '%s';`, block.service, self.password)
			passwordSet = true
		default:
			t.Fatalf("%s.sql has a meta-command this test does not run as psql does: %q", block.service, trimmed)
		}
		lines = append(lines, rolePattern.ReplaceAllString(line, self.role))
	}
	if !passwordSet {
		t.Fatalf("%s.sql sets no password for its role", block.service)
	}
	if output, err := edgeRolePsql(ctx, self.db, strings.Join(lines, "\n")); err != nil {
		// a role made before the error stays only if the file committed it, and then drop removes it
		self.drop(t, ctx)
		t.Fatalf("psql -f %s.sql: %v: %s", block.service, err, output)
	}
	roleFile := fmt.Sprintf("authority: %s\nuser: %s\npassword: %s\ndb: %s\n", self.authority, self.role, self.password, self.db)
	if err := os.WriteFile(filepath.Join(self.vaultHome, "pg-"+block.service+".yml"), []byte(roleFile), 0o600); err != nil {
		t.Fatal(err)
	}
	return self
}

// Drops the role and what it was granted, in the test's database, where every grant is: a role is the cluster's,
// so one left behind outlives the test. The block's pools are closed by then (asBlock).
func (self *edgeRoleSession) drop(t testing.TB, ctx context.Context) {
	script := fmt.Sprintf(`
		SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '%[1]s') AS made \gset
		\if :made
			DROP OWNED BY %[1]s;
			DROP ROLE %[1]s;
		\endif
	`, self.role)
	if output, err := edgeRolePsql(ctx, self.db, script); err != nil {
		t.Errorf("dropping the test's role %s: %v: %s", self.role, err, output)
	}
}

// Runs callback as the block's process connects: WARP_SERVICE is the block's, its vault holds only its role's
// file, so the main pool reads pg-<service>.yml (QD-992) and the maintenance pool, which reads only
// pg_maintenance.yml or pg.yml, has none to read. The process's pools are the test's own again after.
func (self *edgeRoleSession) asBlock(callback func()) {
	restores := []func(){}
	for name, value := range map[string]string{"WARP_SERVICE": self.block.service, "WARP_VAULT_HOME": self.vaultHome} {
		previous, had := os.LookupEnv(name)
		os.Setenv(name, value)
		restores = append(restores, func() {
			if had {
				os.Setenv(name, previous)
			} else {
				os.Unsetenv(name)
			}
		})
	}
	// an override with no value hides the test's own, so each resolves from the block's vault, which lacks it
	restores = append(restores, server.Vault.PushSimpleResource("pg.yml", nil))
	restores = append(restores, server.Vault.PushSimpleResource("pg_maintenance.yml", nil))
	server.PgReset()
	defer func() {
		for _, restore := range slices.Backward(restores) {
			restore()
		}
		server.PgReset()
	}()
	callback()
}

// The error of one statement on the main pool, which asBlock connects as the role, in the simple protocol, so a
// statement with placeholders reaches Postgres as text.
func edgeRoleExec(ctx context.Context, statement string) (returnErr error) {
	defer func() {
		if r := recover(); r != nil {
			if err, ok := r.(error); ok {
				returnErr = err
			} else {
				returnErr = fmt.Errorf("%v", r)
			}
		}
	}()
	server.Db(ctx, func(conn server.PgConn) {
		_, returnErr = conn.Exec(ctx, statement, pgx.QueryExecModeSimpleProtocol)
	})
	return
}

// Whether err is Postgres refusing a privilege.
func edgeRoleRefused(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == edgeRoleRefusedCode
}

// Each statement the block runs, planned under its role: EXPLAIN (GENERIC_PLAN) checks the privileges of every
// table and column the statement names, and runs nothing. A statement built at run time is the listed call's to
// run.
func checkEdgeRoleStatementsPlan(t testing.TB, ctx context.Context, block *edgeRoleBlock, reach *edgeRoleReach) {
	t.Helper()
	planned := 0
	for name := range block.runs {
		for _, statement := range reach.statements[name] {
			if err := edgeRoleExec(ctx, "EXPLAIN (GENERIC_PLAN) "+statement); err != nil {
				t.Errorf("%s's role may not run %s's %q: %v", block.service, name, strings.Join(strings.Fields(statement), " "), err)
			}
			planned += 1
		}
	}
	if planned == 0 {
		t.Errorf("%s: no statement planned, so an empty listing would pass", block.service)
	}
}

// Each statement must be refused with insufficient_privilege under the block's role.
func checkEdgeRoleRefuses(t testing.TB, ctx context.Context, block *edgeRoleBlock, statements []string) {
	t.Helper()
	for _, statement := range statements {
		if err := edgeRoleExec(ctx, statement); !edgeRoleRefused(err) {
			t.Errorf("%s's role ran %q, which it is not granted: %v", block.service, statement, err)
		}
	}
}

// Ensures the family's runway at now, as taskworker's runway task does, under the test's own role.
func ensureEdgeRoleRunway(t testing.TB, ctx context.Context, familyName string, now time.Time) {
	t.Helper()
	family := server.GetPartitionFamily(familyName)
	if family == nil {
		t.Fatalf("no partition family %s", familyName)
	}
	report := family.EnsureRunway(ctx, now)
	if 0 < len(report.Errors) || !report.CurrentCovered {
		t.Fatalf("%s's runway: %+v", familyName, report)
	}
}

// Whether the range of at, an hour or a day as table's family has them, has its leaf of table.
func edgeRoleLeafExists(t testing.TB, ctx context.Context, table string, at time.Time) (exists bool) {
	t.Helper()
	family := server.GetPartitionFamily(table)
	leaf := family.LeafName(table, family.RangeAt(at))
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, "public."+leaf).Scan(&exists))
	})
	return
}

// Runs statements under the test's own role.
func edgeRoleSetup(t testing.TB, ctx context.Context, statements map[string][]any) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		for statement, args := range statements {
			if _, err := tx.Exec(ctx, statement, args...); err != nil {
				t.Fatalf("setup %q: %v", statement, err)
			}
		}
	})
}

// The root the tests' names are in.
const edgeRoleRoot = "roles.example."

func TestEdgeRoleTlsedgeListsWhatItsProcessReaches(t *testing.T) {
	reach := reachEdgeRoleBlock(t, edgeRoleTlsedge)
	checkEdgeRoleReachLists(t, edgeRoleTlsedge, reach)
	for name := range edgeRoleTlsedge.runs {
		t.Logf("tlsedge runs %s: %q %q", name, reach.statements[name], reach.built[name])
	}
}

func TestEdgeRoleMxedgeListsWhatItsProcessReaches(t *testing.T) {
	reach := reachEdgeRoleBlock(t, edgeRoleMxedge)
	checkEdgeRoleReachLists(t, edgeRoleMxedge, reach)
	for name := range edgeRoleMxedge.runs {
		t.Logf("mxedge runs %s: %q %q", name, reach.statements[name], reach.built[name])
	}
	// ABUSE-M9-01's question: the abuse sink's cases and its kick of §21's mail pass never run in the router, whose
	// files build no sink (report_intake.go's is the platform listener's, on pg.yml)
	for _, name := range []string{"NewAbuseSmtpSink", "abuseSmtpSink.OpenSmtpCase", "abuseSmtpSink.RequestSmtpSuspension", "KickAbuseMailPass", "OpenAbuseReportCase"} {
		if reach.reached[name] {
			t.Errorf("the router reaches %s", name)
		}
	}
}

func TestEdgeRoleTlsedgePlansEveryStatementItRuns(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg6Linked() {
			return
		}
		ctx := context.Background()
		reach := reachEdgeRoleBlock(t, edgeRoleTlsedge)
		session := newEdgeRoleSession(t, ctx, edgeRoleTlsedge)
		defer session.drop(t, ctx)
		session.asBlock(func() {
			checkEdgeRoleStatementsPlan(t, ctx, edgeRoleTlsedge, reach)
		})
	})
}

func TestEdgeRoleMxedgePlansEveryStatementItRuns(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg7Linked() {
			return
		}
		ctx := context.Background()
		reach := reachEdgeRoleBlock(t, edgeRoleMxedge)
		session := newEdgeRoleSession(t, ctx, edgeRoleMxedge)
		defer session.drop(t, ctx)
		session.asBlock(func() {
			checkEdgeRoleStatementsPlan(t, ctx, edgeRoleMxedge, reach)
		})
	})
}

// tlsedge's store under its role: the label map with a zone suspension and a withheld host, a custom host, a kill,
// and a batch into an hour the runway made all pass. A batch into an hour with no partition fails, since making the
// hour needs the maintenance pool, and it makes no partition. And what the role is not granted is refused.
func TestEdgeRoleTlsedgeStoreRunsUnderItsGrants(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg6Linked() {
			return
		}
		ctx := context.Background()
		now := server.NowUtc()
		networkId := server.NewId()
		suspendUntil := now.Add(time.Hour).Truncate(time.Microsecond)
		edgeRoleSetup(t, ctx, map[string][]any{
			`INSERT INTO dns_root (root, serial, restore_serial) VALUES ($1, 9, 7)`: {edgeRoleRoot},
			`INSERT INTO dns_label (root, label, network_id, kind, state, state_time, change_serial) VALUES ($1, 'abcdefghij', $2, $3, $4, $5, 5)`: {
				edgeRoleRoot, networkId, model.DnsLabelKindLabel, model.DnsLabelStateLive, now,
			},
			`INSERT INTO network_dns_zone (root, network_id, create_time, suspend_reason, suspend_until) VALUES ($1, $2, $3, 'abuse', $4)`: {
				edgeRoleRoot, networkId, now, suspendUntil,
			},
			`INSERT INTO network_dns_record (root, owner, rrtype, network_id, kind, ttl, create_time, withheld_time) VALUES ($1, $2, 1, $3, 'withheld', 60, $4, $4)`: {
				edgeRoleRoot, "shop.abcdefghij." + edgeRoleRoot, networkId, now,
			},
			`INSERT INTO network_dns_custom_host (host, root, network_id, owner, state, caa_mode, create_time) VALUES ('www.customer.example.', $1, $2, $3, $4, 'weak', $5)`: {
				edgeRoleRoot, networkId, "shop.abcdefghij." + edgeRoleRoot, sidecar.DnsCustomHostStateActive, now,
			},
			`INSERT INTO network_tls_suspension (network_id, host, reason, until_time) VALUES ($1, 'shop', 'abuse', $2)`: {networkId, suspendUntil},
		})
		ensureEdgeRoleRunway(t, ctx, "tls_connection_record", now)
		// an hour within the records' retention that the runway, which creates from an hour back, did not make
		unmadeHour := now.Add(-3 * time.Hour)
		if edgeRoleLeafExists(t, ctx, "tls_connection_record", unmadeHour) {
			t.Fatal("the hour three hours back already has its partition")
		}
		connectionId := server.NewId()
		visitorHash := make([]byte, 16)

		session := newEdgeRoleSession(t, ctx, edgeRoleTlsedge)
		defer session.drop(t, ctx)
		session.asBlock(func() {
			store := model.NewTlsEdgeStore(nil)

			snapshot, err := store.NameSnapshot(ctx, edgeRoleRoot)
			if err != nil {
				t.Fatal("NameSnapshot under tlsedge's role:", err)
			}
			if snapshot.Serial != 5 || snapshot.RestoreSerial != 7 || len(snapshot.Entries) != 1 {
				t.Fatalf("NameSnapshot read %+v", snapshot)
			}
			entry := snapshot.Entries[0]
			if entry.Name != "abcdefghij" || entry.NetworkId != networkId || !entry.ZoneSuspended || !entry.ZoneSuspendUntil.Equal(suspendUntil) || !slices.Equal(entry.WithheldHosts, []string{"shop"}) {
				t.Fatalf("NameSnapshot's entry %+v", entry)
			}
			since, err := store.NamesSince(ctx, edgeRoleRoot, 4)
			if err != nil || len(since.Entries) != 1 {
				t.Fatal("NamesSince under tlsedge's role:", since, err)
			}
			if since, err := store.NamesSince(ctx, edgeRoleRoot, 5); err != nil || len(since.Entries) != 0 {
				t.Fatal("NamesSince past the last change under tlsedge's role:", since, err)
			}

			customHosts, err := store.CustomHosts(ctx, edgeRoleRoot)
			if err != nil {
				t.Fatal("CustomHosts under tlsedge's role:", err)
			}
			if len(customHosts) != 1 || customHosts[0].Host != "www.customer.example" || customHosts[0].OwnerHost != "shop" || customHosts[0].OwnerLabel != "abcdefghij" {
				t.Fatalf("CustomHosts read %+v", customHosts)
			}

			suspensions, err := store.Suspensions(ctx)
			if err != nil {
				t.Fatal("Suspensions under tlsedge's role:", err)
			}
			if len(suspensions) != 1 || suspensions[0].NetworkId != networkId || suspensions[0].Host != "shop" {
				t.Fatalf("Suspensions read %+v", suspensions)
			}

			err = store.InsertConnectionRecords(ctx, []*tlsedgeservice.TlsConnectionRecord{{
				ConnectionId: connectionId,
				NetworkId:    networkId,
				Host:         "shop",
				StartTime:    now.Add(-time.Minute),
				EndTime:      now,
				ByteCount:    1024,
				VisitorHash:  visitorHash,
			}})
			if err != nil {
				t.Fatal("InsertConnectionRecords under tlsedge's role:", err)
			}
			// a retry of the batch stores nothing twice, through the key the role reads
			err = store.InsertConnectionRecords(ctx, []*tlsedgeservice.TlsConnectionRecord{{
				ConnectionId: connectionId,
				NetworkId:    networkId,
				Host:         "shop",
				StartTime:    now.Add(-time.Minute),
				EndTime:      now,
				ByteCount:    1024,
				VisitorHash:  visitorHash,
			}})
			if err != nil {
				t.Fatal("a retried InsertConnectionRecords under tlsedge's role:", err)
			}

			err = store.InsertConnectionRecords(ctx, []*tlsedgeservice.TlsConnectionRecord{{
				ConnectionId: server.NewId(),
				NetworkId:    networkId,
				Host:         "shop",
				StartTime:    unmadeHour.Add(-time.Minute),
				EndTime:      unmadeHour,
				ByteCount:    1024,
				VisitorHash:  visitorHash,
			}})
			if err == nil {
				t.Fatal("tlsedge stored a batch whose hour has no partition, with no maintenance pool to make it")
			}

			checkEdgeRoleRefuses(t, ctx, edgeRoleTlsedge, []string{
				// the label map and the zone are api's and the dns block's to write
				`UPDATE dns_label SET state = state WHERE false`,
				`UPDATE network_dns_zone SET suspend_reason = NULL WHERE false`,
				`DELETE FROM network_dns_record WHERE false`,
				`UPDATE network_dns_custom_host SET state = state WHERE false`,
				`DELETE FROM network_tls_suspension WHERE false`,
				// no column the store does not read
				`SELECT state_time FROM dns_label LIMIT 1`,
				`SELECT * FROM network_dns_zone LIMIT 1`,
				// the records are written, never read back but their key
				`SELECT host FROM tls_connection_record LIMIT 1`,
				`DELETE FROM tls_connection_record WHERE false`,
				// no table the store does not read
				`SELECT 1 FROM network LIMIT 1`,
				`SELECT 1 FROM tls_connection_day LIMIT 1`,
				`SELECT 1 FROM mail_direct_mx LIMIT 1`,
				`SELECT 1 FROM migration_audit LIMIT 1`,
				// no object of its own
				`CREATE TABLE edge_role_probe (probe int)`,
			})
		})

		records := model.Testing_ReadTlsConnectionRecords(ctx, networkId, now.Add(-24*time.Hour))
		if len(records) != 1 || records[0].ConnectionId != connectionId {
			t.Fatalf("the records stored under tlsedge's role: %+v", records)
		}
		if edgeRoleLeafExists(t, ctx, "tls_connection_record", unmadeHour) {
			t.Fatal("tlsedge's role made an hour's partition")
		}
	})
}

// mxedge's router under its role: its start's read of the migration head, ListDirectMx with a row, a label and a
// published key, a batch into an hour the runway made, and Ping, the router's readiness, which reads the catalog on
// the block's own pool with no grant (QD-1133), all pass. A batch into an hour with no partition fails and makes
// none. And what the role is not granted is refused: the direct MX's and the zone's writes, which are api's
// (QD-1103), and §21's kick and cases.
func TestEdgeRoleMxedgeRouterRunsUnderItsGrants(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg7Linked() {
			return
		}
		ctx := context.Background()
		now := server.NowUtc()
		networkId := server.NewId()
		receiverId := server.NewId()
		spkiSha256 := make([]byte, 32)
		spkiSha256[0] = 1
		edgeRoleSetup(t, ctx, map[string][]any{
			`INSERT INTO dns_label (root, label, network_id, kind, state, state_time) VALUES ($1, 'abcdefghij', $2, $3, $4, $5)`: {
				edgeRoleRoot, networkId, model.DnsLabelKindLabel, model.DnsLabelStateLive, now,
			},
			`INSERT INTO mail_direct_mx (network_id, receiver_sidecar_id, address, state, verify_time) VALUES ($1, $2, '192.0.2.10', $3, $4)`: {
				networkId, receiverId, string(model.MailDirectMxStateVerified), now,
			},
			`INSERT INTO mail_host_key (network_id, host, receiver_sidecar_id, spki_sha256, state, publish_time) VALUES ($1, 'mx', $2, $3, $4, $5)`: {
				networkId, receiverId, spkiSha256, string(model.MailHostKeyStatePublished), now,
			},
		})
		ensureEdgeRoleRunway(t, ctx, "mail_connection_record", now)
		unmadeHour := now.Add(-3 * time.Hour)
		if edgeRoleLeafExists(t, ctx, "mail_connection_record", unmadeHour) {
			t.Fatal("the hour three hours back already has its partition")
		}

		session := newEdgeRoleSession(t, ctx, edgeRoleMxedge)
		defer session.drop(t, ctx)
		session.asBlock(func() {
			if err := router.CheckStartupReadiness(ctx); err != nil {
				t.Fatal("the router's start under mxedge's role:", err)
			}

			directMx := model.NewMxDirectMxStore(edgeRoleRoot, nil)
			rows, err := directMx.ListDirectMx(ctx)
			if err != nil {
				t.Fatal("ListDirectMx under mxedge's role:", err)
			}
			if len(rows) != 1 {
				t.Fatalf("ListDirectMx read %+v", rows)
			}
			row := rows[0]
			if row.NetworkId != networkId || row.ReceiverSidecarId != receiverId || row.Address != netip.MustParseAddr("192.0.2.10") ||
				row.Host != "mx.abcdefghij."+edgeRoleRoot || len(row.SpkiSha256s) != 1 || !slices.Equal(row.HintSidecarIds, []sidecar.Id{receiverId}) {
				t.Fatalf("ListDirectMx's row %+v", row)
			}

			store := model.NewMxEdgeStore(nil)
			err = store.InsertConnectionRecords(ctx, []*mxedgeservice.MxConnectionRecord{{
				NetworkId: networkId,
				StartTime: now,
				ByteCount: 2048,
				Result:    "relayed",
			}})
			if err != nil {
				t.Fatal("InsertConnectionRecords under mxedge's role:", err)
			}
			err = store.InsertConnectionRecords(ctx, []*mxedgeservice.MxConnectionRecord{{
				NetworkId: networkId,
				StartTime: unmadeHour,
				ByteCount: 2048,
				Result:    "relayed",
			}})
			if err == nil {
				t.Fatal("mxedge stored a batch whose hour has no partition, with no maintenance pool to make it")
			}

			// the router's readiness: Ping reads the catalog on the block's own pool, where the batch above found no
			// maintenance pool, and answers that the table is partitioned under the role (QD-1133)
			if err := store.Ping(ctx); err != nil {
				t.Fatal("Ping under mxedge's role on the block's own vault:", err)
			}

			checkEdgeRoleRefuses(t, ctx, edgeRoleMxedge, []string{
				// the direct MX and the zone are api's to write (QD-1103)
				`UPDATE mail_direct_mx SET state = state WHERE false`,
				`INSERT INTO mail_direct_mx (network_id, receiver_sidecar_id, address, state) SELECT network_id, receiver_sidecar_id, address, state FROM mail_direct_mx WHERE false`,
				`DELETE FROM mail_direct_mx WHERE false`,
				`UPDATE dns_label SET state = state WHERE false`,
				`UPDATE network_dns_zone SET mail = mail WHERE false`,
				`DELETE FROM network_dns_record WHERE false`,
				`UPDATE mail_host_key SET state = state WHERE false`,
				// no column the stores do not read
				`SELECT state_time FROM dns_label LIMIT 1`,
				`SELECT start_version_number FROM migration_audit LIMIT 1`,
				// the records are written, never read back
				`SELECT network_id FROM mail_connection_record LIMIT 1`,
				// no table the stores do not read
				`SELECT 1 FROM mail_network LIMIT 1`,
				`SELECT 1 FROM network LIMIT 1`,
				// §21's: the mail pass's kick and the sink's cases are not the router's (ABUSE-M9-01)
				`UPDATE pending_task SET run_at = run_at WHERE false`,
				`INSERT INTO abuse_case (case_id) SELECT case_id FROM abuse_case WHERE false`,
				// no object of its own
				`CREATE TABLE edge_role_probe (probe int)`,
			})
		})

		records := model.GetMailConnectionRecords(ctx, networkId, now.Add(-24*time.Hour), now.Add(time.Hour))
		if len(records) != 1 || records[0].ByteCount != 2048 {
			t.Fatalf("the records stored under mxedge's role: %+v", records)
		}
		if edgeRoleLeafExists(t, ctx, "mail_connection_record", unmadeHour) {
			t.Fatal("mxedge's role made an hour's partition")
		}
	})
}

func TestEdgeRoleSmtpedgeListsWhatItsProcessReaches(t *testing.T) {
	reach := reachEdgeRoleBlock(t, edgeRoleSmtpedge)
	checkEdgeRoleReachLists(t, edgeRoleSmtpedge, reach)
	for name := range edgeRoleSmtpedge.runs {
		t.Logf("smtpedge runs %s: %q %q", name, reach.statements[name], reach.built[name])
	}
	// ABUSE-M9-01's question: the sink's suspension request reaches no SQL of model's own, its kick being
	// server/task's UPDATE of pending_task, and its cases are not reached at all
	for _, name := range []string{"abuseSmtpSink.OpenSmtpCase", "OpenAbuseReportCase"} {
		if reach.reached[name] {
			t.Errorf("smtpedge reaches %s", name)
		}
	}
	if !reach.reached["KickAbuseMailPass"] {
		t.Error("smtpedge's suspension request reaches no kick of the mail pass")
	}
}

func TestEdgeRoleSmtpedgePlansEveryStatementItRuns(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg9Linked() {
			return
		}
		ctx := context.Background()
		reach := reachEdgeRoleBlock(t, edgeRoleSmtpedge)
		session := newEdgeRoleSession(t, ctx, edgeRoleSmtpedge)
		defer session.drop(t, ctx)
		session.asBlock(func() {
			checkEdgeRoleStatementsPlan(t, ctx, edgeRoleSmtpedge, reach)
		})
	})
}

// The longest a test waits for a hook it started, so one that never runs fails the test rather than hanging it.
const edgeRoleAwaitTimeout = 2 * time.Minute

// Waits for one value a hook sent.
func edgeRoleAwait[T any](t testing.TB, values chan T, what string) T {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(edgeRoleAwaitTimeout):
		t.Fatalf("no %s", what)
		panic("unreachable")
	}
}

// smtpedge under its role: its start's read of the migration head, the store's calls the engine makes here, the
// send gate's facts with strict_envelope on, the mail breaker, and the abuse gate's and the hard-deny snapshot's
// loads all pass, the loads seen through their OnLoad hooks. A batch into a day with no partition fails and makes
// none. The sink's suspension request fails on its kick of the mail pass, an owned transaction the block has no
// maintenance resource for, which leaves the suspension to the pass's next minute, and the kick's UPDATE of
// pending_task is refused besides. Its OpenSmtpCase, which never runs here, is refused too. And what the role is not
// granted is refused.
func TestEdgeRoleSmtpedgeRunsUnderItsGrants(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg9Linked() {
			return
		}
		ctx := context.Background()
		now := server.NowUtc()
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "roles"+edgeRoleRandomHex(t, 4), server.NewId())
		edgeRoleSetup(t, ctx, map[string][]any{
			`INSERT INTO dns_label (root, label, network_id, kind, state, state_time) VALUES ($1, 'abcdefghij', $2, $3, $4, $5)`: {
				edgeRoleRoot, networkId, model.DnsLabelKindLabel, model.DnsLabelStateLive, now,
			},
			`INSERT INTO dns_label (root, label, network_id, kind, state, state_time) VALUES
				($1, 'live-alias', $2, $3, $4, $6), ($1, 'retired-alias', $2, $3, $5, $6),
				($1, 'other-alias', $7, $3, $4, $6), ('other.example.', 'other-root', $2, $3, $4, $6)`: {
				edgeRoleRoot, networkId, model.DnsLabelKindAlias, model.DnsLabelStateLive, model.DnsLabelStateRetiring, now, server.NewId(),
			},
			`INSERT INTO mail_network (network_id, create_time) VALUES ($1, $2)`: {networkId, now},
			`INSERT INTO network_domain (network_id, domain, challenge, verify_time) VALUES ($1, 'customer.example', 'challenge', $2)`: {
				networkId, now,
			},
			`INSERT INTO network_dns_zone (root, network_id, create_time, report_token) VALUES ($1, $2, $3, 'token')`: {
				edgeRoleRoot, networkId, now,
			},
			`INSERT INTO network_dns_custom_host (host, root, network_id, owner, state, caa_mode, create_time) VALUES ('mail.customer.example.', $1, $2, $3, $4, 'weak', $5)`: {
				edgeRoleRoot, networkId, "mx.abcdefghij." + edgeRoleRoot, sidecar.DnsCustomHostStateActive, now,
			},
		})
		ensureEdgeRoleRunway(t, ctx, "smtp_send", now)
		unmadeDay := now.AddDate(0, 0, -2)
		if edgeRoleLeafExists(t, ctx, "smtp_send", unmadeDay) {
			t.Fatal("the day two days back already has its partition")
		}
		haltId := server.NewId()
		sendId := make([]byte, 12)
		sendId[0] = 1

		session := newEdgeRoleSession(t, ctx, edgeRoleSmtpedge)
		defer session.drop(t, ctx)
		session.asBlock(func() {
			if err := router.CheckStartupReadiness(ctx); err != nil {
				t.Fatal("smtpedge's start under its role:", err)
			}

			store := model.NewSmtpEdgeStore(nil)
			halt := &smtpedgeservice.SmtpHalt{HaltId: haltId, NetworkId: networkId, Trigger: smtpedgeservice.SmtpHaltTriggerRecordCuts, StartTime: now}
			if err := store.SetHalt(ctx, halt); err != nil {
				t.Fatal("SetHalt under smtpedge's role:", err)
			}
			// the same halt again takes the upsert's update
			if err := store.SetHalt(ctx, halt); err != nil {
				t.Fatal("SetHalt again under smtpedge's role:", err)
			}
			if halts, err := store.ActiveHalts(ctx); err != nil || len(halts) != 1 || halts[0].HaltId != haltId {
				t.Fatal("ActiveHalts under smtpedge's role:", halts, err)
			}
			if halts, err := store.FindHalts(ctx, networkId, now.Add(-time.Hour)); err != nil || len(halts) != 1 {
				t.Fatal("FindHalts under smtpedge's role:", halts, err)
			}
			send := &smtpedgeservice.SmtpSendRecord{
				SendId:           sendId,
				NetworkId:        networkId,
				ClientId:         server.NewId(),
				SendingAddress:   netip.MustParseAddr("192.0.2.25"),
				RecipientDomain:  "recipient.example",
				MxHost:           "mx.recipient.example",
				StartTime:        now.Add(-time.Second),
				EndTime:          now,
				ByteCount:        4096,
				RecipientCount:   3,
				MessageByteCount: 2048,
				TlsVersion:       0x0304,
				TlsLevel:         2,
				Result:           "cut",
			}
			if err := store.InsertSends(ctx, []*smtpedgeservice.SmtpSendRecord{send}); err != nil {
				t.Fatal("InsertSends under smtpedge's role:", err)
			}
			// a retried batch stores nothing twice, through the key the role reads
			if err := store.InsertSends(ctx, []*smtpedgeservice.SmtpSendRecord{send}); err != nil {
				t.Fatal("a retried InsertSends under smtpedge's role:", err)
			}
			if count, err := store.CountSends(ctx, networkId, now.Add(-time.Hour), "cut"); err != nil || count != 1 {
				t.Fatal("CountSends under smtpedge's role:", count, err)
			}
			if count, err := store.RecipientCount(ctx, networkId, now.Add(-time.Hour)); err != nil || count != 3 {
				t.Fatal("RecipientCount under smtpedge's role:", count, err)
			}
			late := *send
			late.SendId = append([]byte{2}, sendId[1:]...)
			late.StartTime = unmadeDay
			late.EndTime = unmadeDay.Add(time.Second)
			if err := store.InsertSends(ctx, []*smtpedgeservice.SmtpSendRecord{&late}); err == nil {
				t.Fatal("smtpedge stored a batch whose day has no partition, with no maintenance pool to make it")
			}

			customDomainSettings := model.DefaultSmtpCustomDomainSettings()
			customDomainSettings.StrictEnvelope = true
			facts, err := model.GetSmtpGateFactsWithSettings(ctx, networkId, edgeRoleRoot, customDomainSettings)
			if err != nil {
				t.Fatal("GetSmtpGateFactsWithSettings under smtpedge's role:", err)
			}
			if facts.Label != "abcdefghij" || !slices.Equal(facts.Aliases, []string{"live-alias"}) || facts.MailEnableTime.IsZero() || !slices.Equal(facts.VerifiedDomains, []string{"customer.example"}) ||
				!slices.Equal(facts.StrictDomains, []string{"customer.example"}) || facts.ReportToken != "token" {
				t.Fatalf("the gate's facts under smtpedge's role: %+v", facts)
			}
			if _, ok := model.GetMailBreakerState(ctx); !ok {
				t.Fatal("GetMailBreakerState under smtpedge's role did not read whole")
			}

			hardDenyLoads := make(chan error, 16)
			hardDenySettings := model.DefaultAbuseHardDenySettings()
			hardDenySettings.OnLoad = func(count int, err error) {
				select {
				case hardDenyLoads <- err:
				default:
				}
			}
			hardDeny := model.NewAbuseHardDenySnapshot(ctx, hardDenySettings)
			defer hardDeny.Close()
			if err := edgeRoleAwait(t, hardDenyLoads, "load of the hard-deny snapshot"); err != nil {
				t.Fatal("the hard-deny snapshot's load under smtpedge's role:", err)
			}
			gateLoads := make(chan error, 16)
			gateSettings := model.DefaultAbuseGateSettings()
			gateSettings.OnLoad = func(networkIds []server.Id, err error) {
				select {
				case gateLoads <- err:
				default:
				}
			}
			gate := model.NewAbuseGate(ctx, hardDeny, gateSettings)
			defer gate.Close()
			// a miss asks for the network's load
			gate.Verified(connect.Id(networkId))
			if err := edgeRoleAwait(t, gateLoads, "load of the abuse gate"); err != nil {
				t.Fatal("the abuse gate's load under smtpedge's role:", err)
			}

			// the kick is an owned transaction, which needs the direct maintenance resource the block does not have, so
			// it fails before it reaches Postgres; its UPDATE of pending_task is refused below besides
			sink := model.NewAbuseSmtpSink(nil)
			err = sink.RequestSmtpSuspension(ctx, &smtpedgeservice.SmtpSuspensionRequest{NetworkId: networkId, HaltIds: []sidecar.Id{haltId}, RequestTime: now})
			if err == nil || !strings.Contains(err.Error(), "maintenance resource") {
				t.Fatal("the suspension request's kick of the mail pass in smtpedge's block:", err)
			}
			err = sink.OpenSmtpCase(ctx, &smtpedgeservice.SmtpAbuseReport{
				ComplaintId:   server.NewId(),
				NetworkId:     networkId,
				Domain:        "customer.example",
				Category:      smtpedgeservice.SmtpComplaintTypeAbuse,
				JoinResult:    "high",
				ReporterClass: smtpedgeservice.SmtpReporterClassLoop,
				ArrivalTime:   now,
			})
			if !edgeRoleRefused(err) {
				t.Fatal("a case opened under smtpedge's role:", err)
			}

			checkEdgeRoleRefuses(t, ctx, edgeRoleSmtpedge, []string{
				// §21's: the mail pass's kick and the cases
				`UPDATE pending_task SET run_at = run_at WHERE false`,
				`INSERT INTO abuse_case (case_id) SELECT case_id FROM abuse_case WHERE false`,
				`UPDATE abuse_action SET review_state = review_state WHERE false`,
				`UPDATE network_abuse_state SET state = state WHERE false`,
				// a halt's lift is api's, and a send is never rewritten
				`DELETE FROM smtp_halt WHERE false`,
				`UPDATE smtp_send SET result = result WHERE false`,
				`DELETE FROM smtp_send WHERE false`,
				// the intake's tables
				`SELECT 1 FROM smtp_complaint LIMIT 1`,
				`SELECT 1 FROM smtp_dkim_key LIMIT 1`,
				`INSERT INTO smtp_complaint (complaint_id) SELECT complaint_id FROM smtp_complaint WHERE false`,
				// no column the reads do not name: a send's addresses, a user's secrets, a network's name
				`SELECT sending_address FROM smtp_send LIMIT 1`,
				`SELECT password_hash FROM network_user_auth_password LIMIT 1`,
				`SELECT user_auth FROM network_user_auth_password LIMIT 1`,
				`SELECT network_name FROM network LIMIT 1`,
				`SELECT challenge FROM network_domain LIMIT 1`,
				`SELECT state_time FROM dns_label LIMIT 1`,
				// no table the block does not read
				`SELECT 1 FROM network_user LIMIT 1`,
				`SELECT 1 FROM mail_direct_mx LIMIT 1`,
				// no object of its own
				`CREATE TABLE edge_role_probe (probe int)`,
			})
		})

		if edgeRoleLeafExists(t, ctx, "smtp_send", unmadeDay) {
			t.Fatal("smtpedge's role made a day's partition")
		}
	})
}
