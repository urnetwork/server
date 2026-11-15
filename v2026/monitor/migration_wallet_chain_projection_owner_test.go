// The test-only wallet projection must keep the real observation routing key;
// no planner memory, timeout or canceled-context coincidence proves this guard.
package monitor

import (
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// The entire emitted artifact row survives bounded planning, while the injected
// catalog relation has one explicit statement-local materialization fence.
func TestMigrationCensusWalletProjectionPreservesBoundedSource(t *testing.T) {
	_, query, columns := migrationPublicColumnQueries(t)
	settings := DefaultMigrationCensusSettings()
	for _, state := range []struct {
		valid bool
		ready bool
	}{{valid: true, ready: true}, {valid: false, ready: true}, {valid: true, ready: false}} {
		projected, err := migrationWalletIndexProjectionQuery(query, state.valid, state.ready)
		if err != nil {
			t.Fatal(err)
		}
		shape := func(candidate string) bool {
			return strings.HasPrefix(candidate, "WITH pg_index AS MATERIALIZED (") && strings.Count(candidate, "FROM pg_catalog.pg_index actual") == 1 && strings.Count(candidate, "CASE WHEN actual.indexrelid=to_regclass('public.network_user_auth_wallet_pkey') THEN ") == 2 && !strings.Contains(candidate, "pg_index AS (")
		}
		if !shape(projected) {
			t.Fatal("wallet projection lost its closed materialized catalog owner")
		}
		historical := strings.Replace(projected, "pg_index AS MATERIALIZED (", "pg_index AS (", 1)
		if shape(historical) {
			t.Fatal("inline historical projection passed the planner-boundary control")
		}
		plan, err := planMigrationCensus(projected, settings)
		if err != nil || plan.columns != columns || len(plan.batches) <= 1 {
			t.Fatal("wallet projection lost positional full-census batching", err)
		}
		count := 0
		for index, batch := range plan.batches {
			if batch.index != index || batch.offset != count || batch.columns <= 0 || batch.columns > settings.MaximumColumnsPerStatement || len(batch.query) > settings.MaximumStatementBytes || !strings.Contains(batch.query, "pg_index AS MATERIALIZED (") {
				t.Fatal("wallet projection bypassed bounded native transport", index)
			}
			count += batch.columns
		}
		if count != columns {
			t.Fatal("a complete catalog column was omitted")
		}
	}
}

// A live unmarked caller is refused before the acquisition tripwire. Recreating
// the old query-only wrapper loses the key even when the signal itself sets it.
func TestMigrationCensusWalletProjectionUnownedRefusesBeforeDatabase(t *testing.T) {
	ctx := server.WithoutPostgres(t.Context())
	source := newMigrationWalletIndexProjectionSource(true, true)
	rows, err := source.PostgreSQL(ctx, "WITH version AS (SELECT 1 AS value) SELECT version.value,true FROM version;")
	if err == nil || err.Error() != "native migration parity fixture requires an owned census observation" || rows != nil || server.PacketPostgresAttempts(ctx) != 0 || ctx.Err() != nil {
		t.Fatal("unmarked wallet projection reached storage or changed refusal provenance", err)
	}
	historical := &syntheticSource{postgresFn: func(query string) ([]Row, error) { return source.PostgreSQL(ctx, query) }}
	_, err = NewMigrationsSignal().Run(ctx, syntheticSettings(historical))
	if err == nil || !strings.Contains(err.Error(), "requires an owned census observation") || server.PacketPostgresAttempts(ctx) != 0 || source.artifactRow != nil {
		t.Fatal("query-only context-loss control fabricated ownership or reached storage", err)
	}
}

// This is causal: the same full signal and typed adapter preserve the marker and
// reach the actual guarded DB acquisition exactly once, without fabricating rows.
func TestMigrationCensusWalletProjectionSignalRouteReachesOwnedDatabase(t *testing.T) {
	ctx := server.WithoutPostgres(t.Context())
	source := newMigrationWalletIndexProjectionSource(true, true)
	recovered := func() (failure any) {
		defer func() { failure = recover() }()
		_, _ = NewMigrationsSignal().Run(ctx, syntheticSettings(source))
		return nil
	}()
	if recovered != server.ErrPacketPostgres || server.PacketPostgresAttempts(ctx) != 1 || source.artifactRow != nil || ctx.Err() != nil {
		t.Fatal("typed wallet fixture lost the real acquisition owner", recovered)
	}
}
