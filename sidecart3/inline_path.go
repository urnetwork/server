package sidecart3

// §22.5's hot-path gate (TEST-M1-09; D-TEST-3, I-12, R-TEST-2): an inline server path, the contract gate,
// Find, tlsedge, mxedge, the preamble, quota leases or the write budget, runs its whole entry with every
// PostgreSQL acquisition in the process refused and counted (server.DenyPostgresForTest) and its Redis
// calls counted (sidecarhost.CountingRedis), and is held to its row of sidecar's testdata/budgets.yml: no
// acquisition, and no more Redis calls than the row allows. A row whose redis_calls its owner has not set
// fails as missing, since a path held to nothing has not passed.
//
// The entry is the composed one a request reaches, built with its constructors' effective settings and
// every feature on, never a helper of it: a mocked check would hide the acquisition the tripwire is for
// (connect/CODESTYLE.md:113, :215). Serial, as DenyPostgresForTest is.

import (
	"fmt"
	"maps"
	"testing"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/sidecarhost"
)

// What one run of an inline path did.
type InlinePathResult struct {
	Name string
	// Acquisitions the tripwire refused: each one a failure.
	PostgresAttempts uint64
	// The path's Redis calls, by CountingRedis's names, and their sum.
	RedisCalls     map[string]int
	RedisCallCount int
	// What the path raised, a refused acquisition's ErrPacketPostgres among them, or nil.
	Raised any
}

func (self *InlinePathResult) String() string {
	return fmt.Sprintf("%s: %d PostgreSQL acquisitions, %d Redis calls %v", self.Name, self.PostgresAttempts, self.RedisCallCount, self.RedisCalls)
}

// Runs path, the entry of budgets.yml's path row name, on redis counted and with PostgreSQL refused, and
// fails t on any acquisition and on more Redis calls than the row allows. path reaches Redis only through
// what it is given.
func InlinePath(t testing.TB, name string, redis sidecar.Redis, path func(redis sidecar.Redis)) *InlinePathResult {
	t.Helper()
	ledger, err := sidecartest.LoadBudgets()
	if err != nil {
		t.Fatal(err)
	}
	return inlinePath(t, ledger, name, redis, path)
}

func inlinePath(t testing.TB, ledger *sidecartest.BudgetLedger, name string, redis sidecar.Redis, path func(redis sidecar.Redis)) *InlinePathResult {
	t.Helper()
	budget := ledger.Named(sidecartest.BudgetKindPath, name)
	if budget == nil {
		t.Fatalf("budgets.yml has no path row named %q, so nothing says what %s may do", name, name)
	}
	counting := sidecarhost.NewCountingRedis(redis)
	attempts, stop := server.DenyPostgresForTest(t)
	result := &InlinePathResult{Name: name}
	func() {
		defer stop()
		defer func() {
			result.Raised = recover()
		}()
		path(counting)
	}()
	result.PostgresAttempts = attempts()
	result.RedisCalls = maps.Clone(counting.OpCounts())
	result.RedisCallCount = counting.OpCount()
	if 0 < result.PostgresAttempts {
		t.Errorf("%s: the inline path reached PostgreSQL %d times (I-12; %s)", result, result.PostgresAttempts, budget.Spec)
	}
	if result.Raised != nil && result.PostgresAttempts == 0 {
		t.Errorf("%s raised %v", result, result.Raised)
	}
	limit := budget.Limits.RedisCalls
	switch {
	case limit == nil:
		t.Errorf("%s: missing, which is never a pass: %s has set no redis_calls in budgets.yml's %s row", result, budget.Owner, budget.Id)
	case int64(result.RedisCallCount) > *limit:
		t.Errorf("%s: over the %d Redis calls budgets.yml's %s row allows (%s)", result, *limit, budget.Id, budget.Spec)
	}
	return result
}
