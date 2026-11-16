// A read-only operator process opens only read-only sessions and
// transactions. The wire fixture records the exact statements a session
// receives; no database or production configuration is used.
package server

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// One transaction of each kind and one read-write callback, as the wire
// fixture's session receives them.
func pgReadOnlyWireStatements(t *testing.T) []string {
	t.Helper()
	ctx := context.Background()
	fixture, pool := newPgPoolWireFixture(t, nil, neverPingTestPool)
	txWithPool(ctx, pool, func(tx PgTx) {
		RaisePgResult(tx.Exec(ctx, "INSERT INTO synthetic_probe VALUES (1)"))
	}, OptNoRetry())
	txWithPool(ctx, pool, func(tx PgTx) {
		RaisePgResult(tx.Exec(ctx, "INSERT INTO synthetic_probe VALUES (2)"))
	}, pgx.ReadWrite, OptNoRetry())
	dbWithPool(ctx, pool, func(conn PgConn) {
		RaisePgResult(conn.Exec(ctx, "INSERT INTO synthetic_probe VALUES (3)"))
	}, OptReadWrite(), OptNoRetry())
	fixture.stateLock.Lock()
	defer fixture.stateLock.Unlock()
	statements := []string{}
	for _, query := range fixture.queries {
		if query != "-- ping" {
			statements = append(statements, query)
		}
	}
	return statements
}

func pgReadOnlyStartupParameter(t *testing.T) (string, bool) {
	t.Helper()
	config, err := pgxpool.ParseConfig("host=synthetic-pg.example user=synthetic dbname=synthetic sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	configurePgPoolReadOnly(config)
	value, ok := config.ConnConfig.RuntimeParams["default_transaction_read_only"]
	return value, ok
}

// Ordinary processes keep read-write transactions and the read-write
// escalation; a read-only process begins every transaction read only, even
// one that asks for read write, never escalates the session default, and
// starts every session with it.
func TestRequirePgReadOnlyLeavesNoWritablePath(t *testing.T) {
	ordinary := pgReadOnlyWireStatements(t)
	if !slices.Equal(ordinary, []string{
		"begin isolation level repeatable read read write not deferrable", "INSERT INTO synthetic_probe VALUES (1)", "commit",
		"begin isolation level repeatable read read write not deferrable", "INSERT INTO synthetic_probe VALUES (2)", "commit",
		"SET default_transaction_read_only=off", "INSERT INTO synthetic_probe VALUES (3)",
	}) {
		t.Fatalf("an ordinary process changed its statements: %q", ordinary)
	}
	if _, ok := pgReadOnlyStartupParameter(t); ok {
		t.Fatal("an ordinary process starts its sessions read only")
	}

	pgReadOnly.Store(true)
	t.Cleanup(func() { pgReadOnly.Store(false) })
	readOnly := pgReadOnlyWireStatements(t)
	if !slices.Equal(readOnly, []string{
		"begin isolation level repeatable read read only not deferrable", "INSERT INTO synthetic_probe VALUES (1)", "commit",
		"begin isolation level repeatable read read only not deferrable", "INSERT INTO synthetic_probe VALUES (2)", "commit",
		"INSERT INTO synthetic_probe VALUES (3)",
	}) {
		t.Fatalf("a read-only process issued a writable statement path: %q", readOnly)
	}
	for _, statement := range readOnly {
		if strings.Contains(strings.ToLower(statement), "read write") || strings.Contains(statement, "default_transaction_read_only") {
			t.Fatalf("a read-only process escalated its session: %q", statement)
		}
	}
	if value, ok := pgReadOnlyStartupParameter(t); !ok || value != "on" {
		t.Fatal("a read-only process does not start its sessions read only", value, ok)
	}
}

// A transaction pooler refuses the startup parameter, and a session SET
// through it would reach other clients, so a read-only process connects with
// the direct maintenance resource while an ordinary process keeps the pooled
// one. The resources are synthetic overrides; nothing connects.
func TestRequirePgReadOnlyConnectsDirectly(t *testing.T) {
	t.Cleanup(Vault.PushSimpleResource(DefaultPgVaultResourceName, []byte("authority: pooled.example:6432\nuser: synthetic\npassword: synthetic\ndb: synthetic\n")))
	t.Cleanup(Vault.PushSimpleResource(MaintenancePgVaultResourceName, []byte("authority: direct.example:5432\nuser: synthetic\npassword: synthetic\ndb: synthetic\n")))
	t.Cleanup(Config.PushSimpleResource(DefaultPgConfigResourceName, []byte("min_connections: 1\nmax_connections: 2\n")))
	authority := func() string {
		vaultKeys, _ := safePool.resolveResources()
		return vaultKeys.RequireString("authority")
	}
	if got := authority(); got != "pooled.example:6432" {
		t.Fatal("an ordinary process left its pooled resource", got)
	}
	pgReadOnly.Store(true)
	t.Cleanup(func() { pgReadOnly.Store(false) })
	if got := authority(); got != "direct.example:5432" {
		t.Fatal("a read-only process connects through the transaction pooler", got)
	}
}
