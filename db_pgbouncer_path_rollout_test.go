package server

// The rollout of the direct path to PgBouncer, block by block: the table that
// lists the blocks, the choice each deployed service's main makes from it, and
// that no other code chooses a path. No database is involved.

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestPgBouncerPathOfBlock(t *testing.T) {
	directBlocks := map[string][]string{
		"api":     {"beta"},
		"connect": {"beta", "g1"},
		"proxy":   {PgBouncerEveryBlock},
	}
	for _, check := range []struct {
		service string
		block   string
		want    PgBouncerPath
	}{
		{"api", "beta", PgBouncerPathDirect},
		{"api", "g1", PgBouncerPathSinglePort},
		{"connect", "g1", PgBouncerPathDirect},
		{"connect", "g2", PgBouncerPathSinglePort},
		{"proxy", "g7", PgBouncerPathDirect},
		// a service the table leaves out
		{"taskworker", "beta", PgBouncerPathSinglePort},
		// a process warp does not run, such as one an operator starts through
		// the tunnel, which forwards only the single port
		{"api", "", PgBouncerPathSinglePort},
		{"proxy", "", PgBouncerPathSinglePort},
	} {
		if got := pgBouncerPathOfBlock(directBlocks, check.service, check.block); got != check.want {
			t.Errorf("%s block %q: %s, want %s", check.service, check.block, got, check.want)
		}
	}
	if got := pgBouncerPathOfBlock(map[string][]string{}, "api", "beta"); got != PgBouncerPathSinglePort {
		t.Errorf("an empty table: %s", got)
	}
}

// A main's choice follows the block warp runs the process in, and reaches the
// main pool `Db` opens. The resources are synthetic overrides and the pools
// open no connection.
func TestSetServicePgBouncerPathChoosesTheProcessBlock(t *testing.T) {
	pg := []byte("authority: 192.0.2.10:6432\nuser: synthetic\npassword: synthetic\ndb: synthetic\n")
	pool := []byte("min_connections: 0\nmax_connections: 1\n")
	t.Cleanup(Vault.PushSimpleResource(DefaultPgVaultResourceName, pg))
	t.Cleanup(Vault.PushSimpleResource(MaintenancePgVaultResourceName, pg))
	t.Cleanup(Config.PushSimpleResource(DefaultPgConfigResourceName, pool))
	t.Cleanup(Config.PushSimpleResource(MaintenancePgConfigResourceName, pool))
	stageBlocks := pgBouncerDirectBlocks
	t.Cleanup(func() {
		pgBouncerDirectBlocks = stageBlocks
		SetPgBouncerPath(PgBouncerPathSinglePort)
		PgReset()
	})
	pgBouncerDirectBlocks = map[string][]string{"api": {"beta"}}
	PgReset()
	directDialFunc := reflect.ValueOf((&pgBouncerDirectDialer{}).dialContext).Pointer()
	dialsDirect := func() bool {
		return reflect.ValueOf(safePool.open().Config().ConnConfig.DialFunc).Pointer() == directDialFunc
	}

	t.Setenv("WARP_BLOCK", "beta")
	if path := SetServicePgBouncerPath("api"); path != PgBouncerPathDirect || currentPgBouncerPath() != PgBouncerPathDirect {
		t.Fatalf("api beta chose %s, the process is on %s", path, currentPgBouncerPath())
	}
	if !dialsDirect() {
		t.Fatal("api beta's main pool dials the single port")
	}

	t.Setenv("WARP_BLOCK", "g1")
	if path := SetServicePgBouncerPath("api"); path != PgBouncerPathSinglePort || currentPgBouncerPath() != PgBouncerPathSinglePort {
		t.Fatalf("api g1 chose %s, the process is on %s", path, currentPgBouncerPath())
	}
	if dialsDirect() {
		t.Fatal("api g1's main pool dials the instances")
	}

	t.Setenv("WARP_BLOCK", "beta")
	if path := SetServicePgBouncerPath("connect"); path != PgBouncerPathSinglePort {
		t.Fatalf("connect beta chose %s", path)
	}

	t.Setenv("WARP_BLOCK", "")
	if path := SetServicePgBouncerPath("api"); path != PgBouncerPathSinglePort {
		t.Fatalf("an api process outside warp chose %s", path)
	}
	if dialsDirect() {
		t.Fatal("an api process outside warp dials the instances")
	}
}

// The table names only services whose mains read it, and blocks as warp names
// them, so no entry can go unread.
func TestPgBouncerDirectBlocksNameServicesThatChoose(t *testing.T) {
	blockName := regexp.MustCompile(`^(beta|g[1-9][0-9]*)$`)
	for service, blocks := range pgBouncerDirectBlocks {
		if !slices.Contains(PgBouncerPathServices, service) {
			t.Errorf("%s is in the table, but its main does not choose from it", service)
		}
		if len(blocks) == 0 {
			t.Errorf("%s lists no block", service)
		}
		for i, block := range blocks {
			if block != PgBouncerEveryBlock && !blockName.MatchString(block) {
				t.Errorf("%s lists %q, which is not a block name", service, block)
			}
			if slices.Contains(blocks[:i], block) {
				t.Errorf("%s lists %q twice", service, block)
			}
		}
		if slices.Contains(blocks, PgBouncerEveryBlock) && len(blocks) != 1 {
			t.Errorf("%s lists every block and more: %v", service, blocks)
		}
	}
}

// The rollout's current stage. A stage's merge, in its window, changes the
// table and this expectation together (owner, 2026-11-13: each database-path
// window's go comes back to the owner). Until the first stage, every block of
// every service keeps nginx's single port.
func TestPgBouncerDirectRolloutStage(t *testing.T) {
	stage := map[string][]string{}
	if !reflect.DeepEqual(pgBouncerDirectBlocks, stage) {
		t.Fatalf("the table is %v, the stage %v", pgBouncerDirectBlocks, stage)
	}
}

// A call to one of the path choosers in non-test code.
type pgBouncerPathChoiceCall struct {
	// the file, from the repository root
	path string
	// the function the call is in
	function string
	// SetPgBouncerPath or SetServicePgBouncerPath
	chooser string
	// a string literal's value or a selector's name; "?" for anything else
	argument string
}

// The chooser calls in one Go file.
func pgBouncerPathChoiceCalls(t *testing.T, path string, file *ast.File) []pgBouncerPathChoiceCall {
	t.Helper()
	calls := []pgBouncerPathChoiceCall{}
	for _, decl := range file.Decls {
		function, ok := decl.(*ast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			var name string
			switch fun := call.Fun.(type) {
			case *ast.Ident:
				name = fun.Name
			case *ast.SelectorExpr:
				name = fun.Sel.Name
			}
			if name != "SetPgBouncerPath" && name != "SetServicePgBouncerPath" {
				return true
			}
			argument := "?"
			if len(call.Args) == 1 {
				switch arg := call.Args[0].(type) {
				case *ast.BasicLit:
					if arg.Kind == token.STRING {
						if value, err := strconv.Unquote(arg.Value); err == nil {
							argument = value
						}
					}
				case *ast.SelectorExpr:
					argument = arg.Sel.Name
				case *ast.Ident:
					argument = arg.Name
				}
			}
			calls = append(calls, pgBouncerPathChoiceCall{
				path:     path,
				function: function.Name.Name,
				chooser:  name,
				argument: argument,
			})
			return true
		})
	}
	return calls
}

// Each deployed service's main chooses its path from the table, once, under
// its own name. Nothing else chooses a path: every other main, tools such as
// bringyourctl among them, and every library keep the single port, and only
// the table's chooser sets the path it reads.
func TestServiceMainsChooseTheirPgBouncerPath(t *testing.T) {
	calls := []pgBouncerPathChoiceCall{}
	mainDirs := map[string]bool{}
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			name := entry.Name()
			if path != "." && (strings.HasPrefix(name, ".") || name == "third_party" || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		packageClause, err := parser.ParseFile(token.NewFileSet(), path, source, parser.PackageClauseOnly)
		if err != nil {
			// not a Go file the build reads
			return nil
		}
		if packageClause.Name.Name == "main" {
			mainDirs[filepath.Dir(path)] = true
		}
		if !bytes.Contains(source, []byte("PgBouncerPath(")) {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, source, parser.SkipObjectResolution)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			return nil
		}
		calls = append(calls, pgBouncerPathChoiceCalls(t, filepath.ToSlash(path), file)...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	want := []pgBouncerPathChoiceCall{{
		path:     "db_pgbouncer_path_rollout.go",
		function: "SetServicePgBouncerPath",
		chooser:  "SetPgBouncerPath",
		argument: "path",
	}}
	for _, service := range PgBouncerPathServices {
		want = append(want, pgBouncerPathChoiceCall{
			path:     "cli/" + service + "/main.go",
			function: "main",
			chooser:  "SetServicePgBouncerPath",
			argument: service,
		})
		if !mainDirs[filepath.Join("cli", service)] {
			t.Errorf("no main package for %s", service)
		}
	}
	order := func(a pgBouncerPathChoiceCall, b pgBouncerPathChoiceCall) int {
		return strings.Compare(a.path+" "+a.function, b.path+" "+b.function)
	}
	slices.SortFunc(calls, order)
	slices.SortFunc(want, order)
	if !reflect.DeepEqual(calls, want) {
		t.Errorf("path choices:\n%v\nwant:\n%v", calls, want)
	}
	// bringyourctl, the cli tools and the competition binaries among them
	if len(mainDirs) < 20 {
		t.Fatal("found only", len(mainDirs), "main packages")
	}
}
