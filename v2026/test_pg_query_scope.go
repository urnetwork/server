// Fixture-local driver observation without PostgreSQL privileges or global counters.
package server

import (
	"context"
	"errors"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/multitracer"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Owns cloned application pools only inside an exclusive TestEnv callback.
// The caller must join prior users before construction and traced users before
// Close; concurrent TestEnv, nested scopes, and PgReset are not supported.
// Close is concurrent-safe and idempotent, and must be deferred in the callback,
// not registered with testing.T.Cleanup after environment restoration.
type TestPgQueryScope struct {
	poolOwners [2]testPgQueryScopePool
	closeOnce  sync.Once
	closeErr   error
}

// A displaced prior pool remains owned until restoration or explicit disposal.
type testPgQueryScopePool struct {
	target *safePgPool
	prior  *pgxpool.Pool
	traced *pgxpool.Pool
}

// Embedding retains all optional tracer interfaces and identifies nested scopes.
type testPgQueryScopeTracer struct{ *multitracer.Tracer }

// Observe actual Db, ReplicaDb, Tx and maintenance calls in this disposable fixture.
// No arguments or credentials are copied by the scope; the caller owns its tracer.
func NewTestPgQueryScope(ctx context.Context, tracer pgx.QueryTracer) (*TestPgQueryScope, error) {
	databaseNames := [2]string{}
	for index, pool := range []*safePgPool{safePool, safeMaintenancePool} {
		resource, _ := pool.resolveResources()
		databaseNames[index] = resource.RequireString("db")
	}
	if err := testPgQueryScopeFixture(RequireEnv(), databaseNames); err != nil {
		return nil, err
	}
	return newTestPgQueryScope(ctx, tracer, [2]*safePgPool{safePool, safeMaintenancePool}, pgxpool.NewWithConfig, databaseNames[0])
}

// Both resources must identify the same generated disposable database.
func testPgQueryScopeFixture(env string, databaseNames [2]string) error {
	if env != "local" {
		return errors.New("query observation requires a local TestEnv")
	}
	if _, ok := parseTestPgDbName(databaseNames[0]); !ok || databaseNames[0] != databaseNames[1] {
		return errors.New("query observation requires one disposable TestEnv database")
	}
	return nil
}

// Build before publication; any constructor failure or Goexit retires all clones.
func newTestPgQueryScope(ctx context.Context, tracer pgx.QueryTracer, pools [2]*safePgPool, create func(context.Context, *pgxpool.Config) (*pgxpool.Pool, error), database string) (_ *TestPgQueryScope, returnErr error) {
	if tracer == nil {
		return nil, errors.New("query observation requires a tracer")
	}
	if pools[0] == nil || pools[1] == nil || pools[0] == pools[1] {
		return nil, errors.New("query observation requires two distinct pools")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	scope := &TestPgQueryScope{}
	installed := false
	defer func() {
		if !installed {
			for _, owner := range scope.poolOwners {
				if owner.traced != nil {
					owner.traced.Close()
				}
			}
		}
	}()
	for index, pool := range pools {
		prior := pool.open()
		if prior.Stat().AcquiredConns() != 0 {
			return nil, errors.New("query observation cannot displace acquired fixture connections")
		}
		config := prior.Config()
		if config.ConnConfig.Database != database {
			return nil, errors.New("active pool database differs from disposable fixture")
		}
		if _, nested := config.ConnConfig.Tracer.(*testPgQueryScopeTracer); nested {
			return nil, errors.New("nested query observation is not supported")
		}
		tracers := []pgx.QueryTracer{}
		if config.ConnConfig.Tracer != nil {
			tracers = append(tracers, config.ConnConfig.Tracer)
		}
		config.ConnConfig.Tracer = &testPgQueryScopeTracer{Tracer: multitracer.New(append(tracers, tracer)...)}
		traced, err := create(ctx, config)
		scope.poolOwners[index] = testPgQueryScopePool{target: pool, prior: prior, traced: traced}
		if err != nil {
			return nil, err
		}
		if traced == nil {
			return nil, errors.New("query observation constructor returned no pool")
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Diagnostic recheck, not an admission lock: all previous users must be joined.
	for _, owner := range scope.poolOwners {
		if owner.prior.Stat().AcquiredConns() != 0 {
			return nil, errors.New("fixture connection acquired during query observation construction")
		}
	}
	// Existing pool locks use application-before-maintenance ordering.
	installed = func() bool {
		pools[0].mutex.Lock()
		defer pools[0].mutex.Unlock()
		pools[1].mutex.Lock()
		defer pools[1].mutex.Unlock()
		for _, owner := range scope.poolOwners {
			if owner.target.pool != owner.prior {
				return false
			}
		}
		for _, owner := range scope.poolOwners {
			owner.target.pool = owner.traced
		}
		return true
	}()
	if !installed {
		return nil, errors.New("fixture pools changed while installing query observation")
	}
	return scope, nil
}

// Restore both identities before joining either clone; never close under pool locks.
// Unexpected replacement remains intact and fails cleanup, rather than being hidden.
func (self *TestPgQueryScope) Close() error {
	self.closeOnce.Do(func() {
		retire := []*pgxpool.Pool{}
		func() {
			self.poolOwners[0].target.mutex.Lock()
			defer self.poolOwners[0].target.mutex.Unlock()
			self.poolOwners[1].target.mutex.Lock()
			defer self.poolOwners[1].target.mutex.Unlock()
			for _, owner := range self.poolOwners {
				if owner.target.pool == owner.traced {
					owner.target.pool = owner.prior
				} else {
					self.closeErr = errors.Join(self.closeErr, errors.New("fixture pool replaced during query observation"))
					if owner.target.pool != owner.prior {
						retire = append(retire, owner.prior)
					}
				}
				retire = append(retire, owner.traced)
			}
		}()
		for _, pool := range retire {
			pool.Close()
		}
	})
	return self.closeErr
}
