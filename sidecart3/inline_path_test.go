package sidecart3

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/sidecarhost"
)

// Toy rows for the gate itself, so these tests hold InlinePath and no owner's path.
func inlinePathLedger(t *testing.T) *sidecartest.BudgetLedger {
	t.Helper()
	ledger, err := sidecartest.ParseBudgets([]byte(`budgets:
  - {id: toy-one-call, spec: "§22.5's §9 row: one script per Find", kind: path, owner: SOCK-M5-17, module: server, name: toy.one-call, limits: {postgres: 0, redis_calls: 1}}
  - {id: toy-unset, spec: "§22.5: tlsedge inline", kind: path, owner: TLS-M5-26, module: server, name: toy.unset, limits: {postgres: 0}}
`))
	if err != nil {
		t.Fatal(err)
	}
	return ledger
}

// A key no other run shares, in a tag of its own.
func inlinePathKey(name string) string {
	return "{" + runInlineId() + "}" + name
}

func runInlineId() string {
	return "t3inline_" + strings.ToLower(sidecar.NewId().String())
}

// A planted PostgreSQL read inside a path fails the gate, whatever the path did with the refusal.
func TestInlinePathFailsAPlantedPostgresRead(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(tb testing.TB) {
		ledger := inlinePathLedger(t)
		var result *InlinePathResult
		recorder := record(t, func(tb testing.TB) {
			result = inlinePath(tb, ledger, "toy.one-call", sidecarhost.NewRedis(nil), func(redis sidecar.Redis) {
				server.Db(context.Background(), func(conn server.PgConn) {})
			})
		})
		if !strings.Contains(recorder.text(), "the inline path reached PostgreSQL 1 times (I-12") {
			t.Errorf("the gate said %s", recorder.text())
		}
		if result == nil || result.PostgresAttempts != 1 || result.Raised == nil {
			t.Errorf("the result %v", result)
		}
	})
}

// One Redis call over the row fails; within it, the path passes.
func TestInlinePathFailsOneRedisCallOverItsRow(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(tb testing.TB) {
		ledger := inlinePathLedger(t)
		redis := sidecarhost.NewRedis(nil)
		key := inlinePathKey("k")
		over := record(t, func(tb testing.TB) {
			inlinePath(tb, ledger, "toy.one-call", redis, func(redis sidecar.Redis) {
				redis.Set(t.Context(), key, []byte("v"), time.Minute)
				redis.Get(t.Context(), key)
			})
		})
		if !strings.Contains(over.text(), "over the 1 Redis calls budgets.yml's toy-one-call row allows") {
			t.Errorf("the gate said %s", over.text())
		}
		within := record(t, func(tb testing.TB) {
			result := inlinePath(tb, ledger, "toy.one-call", redis, func(redis sidecar.Redis) {
				redis.Get(t.Context(), key)
			})
			if result.RedisCallCount != 1 || result.RedisCalls["get"] != 1 {
				tb.Errorf("counted %v", result.RedisCalls)
			}
		})
		if within.Failed() {
			t.Errorf("a path within its row failed: %s", within.text())
		}
	})
}

// A row whose owner has not set redis_calls fails as missing, and a name with no row fails outright.
func TestInlinePathReadsAnUnsetLimitAsMissing(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(tb testing.TB) {
		ledger := inlinePathLedger(t)
		unset := record(t, func(tb testing.TB) {
			inlinePath(tb, ledger, "toy.unset", sidecarhost.NewRedis(nil), func(redis sidecar.Redis) {})
		})
		if !strings.Contains(unset.text(), "missing, which is never a pass: TLS-M5-26 has set no redis_calls") {
			t.Errorf("the gate said %s", unset.text())
		}
		unnamed := record(t, func(tb testing.TB) {
			inlinePath(tb, ledger, "toy.nothing", sidecarhost.NewRedis(nil), func(redis sidecar.Redis) {})
		})
		if !unnamed.fatal || !strings.Contains(unnamed.text(), `no path row named "toy.nothing"`) {
			t.Errorf("the gate said %s", unnamed.text())
		}
	})
}

// The path rows' names that server's tests run through InlinePath.
func inlinePathCalls(t *testing.T, serverRoot string) []string {
	t.Helper()
	names := []string{}
	err := filepath.WalkDir(serverRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if name := entry.Name(); path != serverRoot && (strings.HasPrefix(name, ".") || name == "third_party" || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) < 2 {
				return true
			}
			name := ""
			switch function := call.Fun.(type) {
			case *ast.Ident:
				name = function.Name
			case *ast.SelectorExpr:
				name = function.Sel.Name
			}
			if name != "InlinePath" {
				return true
			}
			if literal, ok := call.Args[1].(*ast.BasicLit); ok && literal.Kind == token.STRING {
				if value, err := strconv.Unquote(literal.Value); err == nil {
					names = append(names, value)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return names
}

// Each path row of the ledger resolves to a server test that runs it through InlinePath once its owner
// lands it; until then it reads missing, never held.
func TestEveryPathRowResolvesOrReadsMissing(t *testing.T) {
	ledger, err := sidecartest.LoadBudgets()
	if err != nil {
		t.Fatal(err)
	}
	calls := inlinePathCalls(t, "..")
	paths := 0
	for _, budget := range ledger.Budgets {
		if budget.Kind != sidecartest.BudgetKindPath {
			continue
		}
		paths += 1
		if slices.Contains(calls, budget.Name) {
			t.Logf("%s runs through InlinePath as %s", budget.Id, budget.Name)
			continue
		}
		t.Logf("missing, which is never a pass: %s's path %s runs through no InlinePath yet (%s)", budget.Id, budget.Name, budget.Owner)
	}
	if paths == 0 {
		t.Error("the ledger holds no inline path")
	}
}
