package server

// Exercises actual post-acquisition resource push/pop, teardown ordering and
// candidate routing without connecting to PostgreSQL, Redis, or the network.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

// All service effects are intercepted. Only the real in-process Vault routing
// and lifecycle driver execute. No observation claims that a Redis command ran.
type testEnvStoreLifecycleFixture struct {
	env          *TestEnv
	flushDBs     []int
	releases     int
	effects      []string
	migrationErr error
}

// Each root owns its resolver, so deliberately leaked overrides cannot affect
// another test or any real credentials. No external effect closure is invoked.
func newTestEnvStoreLifecycleFixture(t *testing.T) *testEnvStoreLifecycleFixture {
	t.Helper()
	t.Setenv("WARP_ENV", "local")
	t.Setenv("WARP_VAULT_HOME", t.TempDir())
	previous := Vault
	Vault = NewResolver(MOUNT_TYPE_VAULT)
	t.Cleanup(func() { Vault = previous })
	pg := map[string]any{"authority": "owned-pg.invalid:5432", "user": "fixture", "password": "fixture", "db": "owned-base-pg"}
	maintenance := map[string]any{"authority": "owned-maintenance.invalid:5432", "user": "fixture", "password": "fixture", "db": "owned-base-maintenance"}
	Vault.PushSimpleResource(DefaultPgVaultResourceName, testPgResourceForDatabase(pg, "owned-base-pg"))
	Vault.PushSimpleResource(MaintenancePgVaultResourceName, testPgResourceForDatabase(maintenance, "owned-base-maintenance"))
	Vault.PushSimpleResource("redis.yml", []byte("authority: owned-redis.invalid:6379\npassword: fixture\ndb: 1\ncluster: false\n"))
	return &testEnvStoreLifecycleFixture{env: &TestEnv{ApplyDbMigrations: true}}
}

// Reads the same mutable resources that setup consumes. The caller supplies
// an already-acquired DB; retry tests choose it through the real candidate code.
func (self *testEnvStoreLifecycleFixture) setup(database int) func() {
	pg := Vault.RequireSimpleResource(DefaultPgVaultResourceName).Parse()
	maintenance := Vault.RequireSimpleResource(MaintenancePgVaultResourceName).Parse()
	redisResource := Vault.RequireSimpleResource("redis.yml")
	return self.env.setupWithAcquiredStores(
		context.Background(), pg, maintenance, fmt.Sprintf("test_123_%032x", database),
		redisResource.RequireString("authority"), redisResource.RequireString("password"), database,
		func() { self.releases++ },
		func(operation string, _ func()) {
			self.effects = append(self.effects, operation)
			switch operation {
			case "redis-flush":
				database := RedisDb()
				self.flushDBs = append(self.flushDBs, database)
			case "pg-migrate":
				if self.migrationErr != nil {
					panic(self.migrationErr)
				}
			}
		},
	)
}

// Captures only the explicit setup sentinel; a prerequisite panic is not a
// causal result. A returned teardown would mean the requested failure was lost.
func failTestEnvStoreMigration(t *testing.T, fixture *testEnvStoreLifecycleFixture) {
	t.Helper()
	sentinel := errors.New("owned post-override migration failure")
	fixture.migrationErr = sentinel
	var result any
	func() {
		defer func() { result = recover() }()
		if teardown := fixture.setup(2); teardown != nil {
			t.Error("setup unexpectedly returned teardown after migration failure")
			teardown()
		}
	}()
	fixture.migrationErr = nil
	if result != sentinel || fixture.releases != 1 {
		t.Fatalf("setup failure was not the owned sentinel: panic=%v releases=%d", result, fixture.releases)
	}
}

// A failure after both real overrides must restore the exact prior routes.
func TestTestEnvStoreSetupFailureRestoresOverrides(t *testing.T) {
	fixture := newTestEnvStoreLifecycleFixture(t)
	failTestEnvStoreMigration(t, fixture)
	var leaked []string
	if Vault.RequireSimpleResource(DefaultPgVaultResourceName).RequireString("db") != "owned-base-pg" {
		leaked = append(leaked, "postgres")
	}
	if Vault.RequireSimpleResource(MaintenancePgVaultResourceName).RequireString("db") != "owned-base-maintenance" {
		leaked = append(leaked, "maintenance")
	}
	if RedisDb() != 1 {
		leaked = append(leaked, "redis")
	}
	if len(leaked) != 0 {
		t.Fatalf("failed setup retained test resource overrides: %v", leaked)
	}
}

// Three logical databases leave exactly one candidate. The actual leaked
// reserved value deterministically routes the retry to the original dev DB.
func TestTestEnvStoreSetupRetryPreservesOriginalReservedDB(t *testing.T) {
	fixture := newTestEnvStoreLifecycleFixture(t)
	failTestEnvStoreMigration(t, fixture)
	candidates := testRedisDbCandidates(3, RedisDb(), 0)
	if len(candidates) != 1 {
		t.Fatalf("retry candidate census=%v", candidates)
	}
	if candidates[0] == 1 {
		t.Fatalf("setup retry reclassified original reserved Redis database as disposable: candidates=%v", candidates)
	}
	teardown := fixture.setup(candidates[0])
	teardown()
	if RedisDb() != 1 || fixture.releases != 2 {
		t.Fatalf("successful retry did not restore original route: db=%d releases=%d", RedisDb(), fixture.releases)
	}
}

// The existing successful setup/teardown route keeps its exact target and
// restores prior PostgreSQL/Redis resources before releasing its lease.
func TestTestEnvStoreSuccessfulLifecycleKeepsOriginalRoutes(t *testing.T) {
	fixture := newTestEnvStoreLifecycleFixture(t)
	teardown := fixture.setup(2)
	if RedisDb() != 2 || fixture.releases != 0 || !reflect.DeepEqual(fixture.flushDBs, []int{2}) {
		t.Fatalf("setup route=%d releases=%d flushes=%v", RedisDb(), fixture.releases, fixture.flushDBs)
	}
	teardown()
	if RedisDb() != 1 || fixture.releases != 1 || !reflect.DeepEqual(fixture.flushDBs, []int{2, 2}) {
		t.Fatalf("teardown route=%d releases=%d flushes=%v", RedisDb(), fixture.releases, fixture.flushDBs)
	}
	if Vault.RequireSimpleResource(DefaultPgVaultResourceName).RequireString("db") != "owned-base-pg" ||
		Vault.RequireSimpleResource(MaintenancePgVaultResourceName).RequireString("db") != "owned-base-maintenance" {
		t.Fatal("successful teardown did not restore exact original PostgreSQL routes")
	}
}
