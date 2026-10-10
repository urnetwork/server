// Current-main integration preserves the deployed catalog before adding payout
// capabilities. Numeric versions alone never authorize a conflicting history.
package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// Captured from the complete 18ec7c05 source catalog, not the merged candidate.
func TestProviderCurrentMigrationPreservesPublishedHistory(t *testing.T) {
	const publishedPrefix = "8befd2c6303ee0cfb8ed74f53e2477341ae6d1b84446090ca2d9de5608381933"
	if got, err := migrationPrefixIdentity(755); err != nil || got != publishedPrefix {
		t.Fatalf("published 755-entry migration prefix changed: %s, %v", got, err)
	}
	for offset, expected := range []string{
		"0e1408f81b07a741a2618ff4309aec6b395ad4847d6881cfce146a2e9845fa2d",
		"50d93343ac5a0618421fadbb9f282b311f64d56aaa006adc616fec182fadc587",
	} {
		if got, err := MigrationIdentity(755 + offset); err != nil || got != expected {
			t.Fatalf("payout append %d changed identity or order: %s, %v", 755+offset, got, err)
		}
	}
}

// Observe the actual installed Redis feature and exact retained catalog rows.
// The deprecated policy table remains compatibility metadata, not activation.
func currentPayoutMigrationWitness(t testing.TB, ctx context.Context) (string, string, int) {
	t.Helper()
	var redisSchema, catalog string
	var audits int
	MaintenanceDb(ctx, func(conn PgConn) {
		var redisPresent bool
		Raise(conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_attribute
			WHERE attrelid='transfer_escrow'::regclass AND attname='redis_reserved'
			AND atttypid='boolean'::regtype AND attnotnull AND NOT attisdropped)
			AND to_regclass('public.redis_contract_admission_policy') IS NOT NULL`).Scan(&redisPresent))
		if !redisPresent {
			t.Fatal("published Redis admission feature missing at version 755")
		}
		Raise(conn.QueryRow(ctx, `SELECT jsonb_build_object(
			'column', (SELECT row_to_json(a) FROM pg_attribute a WHERE a.attrelid='transfer_escrow'::regclass AND a.attname='redis_reserved'),
			'default', (SELECT pg_get_expr(d.adbin,d.adrelid) FROM pg_attrdef d JOIN pg_attribute a ON a.attrelid=d.adrelid AND a.attnum=d.adnum WHERE a.attrelid='transfer_escrow'::regclass AND a.attname='redis_reserved'),
			'policy', (SELECT jsonb_agg(to_jsonb(p) ORDER BY singleton) FROM redis_contract_admission_policy p),
			'escrow_function', pg_get_functiondef('transfer_escrow_revision()'::regprocedure),
			'contract_function', pg_get_functiondef('transfer_contract_escrow_revision()'::regprocedure))::text`).Scan(&redisSchema))
		Raise(conn.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(c) ORDER BY migration_index)::text
			FROM migration_catalog c WHERE migration_index<755`).Scan(&catalog))
		Raise(conn.QueryRow(ctx, `SELECT count(*) FROM migration_audit`).Scan(&audits))
	}, OptReadOnly(), OptNoRetry())
	return redisSchema, catalog, audits
}

// Production migration upgrades 755 to 756 and 757; restart neither replays
// earlier SQL nor enrolls a new earning identity from an edited schedule.
func TestProviderCurrentMigrationUpgradesPublishedSchemaWithoutReplay(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		ApplyDbMigrationsUpTo(ctx, 755)
		if got := DbVersion(ctx); got != 755 {
			t.Fatalf("published starting version=%d", got)
		}
		redisBefore, catalogBefore, auditsBefore := currentPayoutMigrationWitness(t, ctx)
		if err := RequireProviderPayoutSchema(ctx); err == nil {
			t.Fatal("uninstalled bonus capability admitted")
		}
		_, policy := boundaryTestPolicy(t)
		if _, err := prepareProviderPayoutBoundary(ctx, policy, policy.ConfigSha256); !errors.Is(err, ErrProviderEarningBoundarySchema) {
			t.Fatal("uninstalled earning boundary admitted", err)
		}
		ApplyDbMigrationsUpTo(ctx, 756)
		if got := DbVersion(ctx); got != 756 {
			t.Fatalf("bonus upgrade version=%d", got)
		}
		if err := RequireProviderPayoutSchema(ctx); err != nil {
			t.Fatal("relocated bonus capability refused", err)
		}
		if _, err := prepareProviderPayoutBoundary(ctx, policy, policy.ConfigSha256); !errors.Is(err, ErrProviderEarningBoundarySchema) {
			t.Fatal("boundary admitted before its migration", err)
		}
		ApplyDbMigrationsUpTo(ctx, 757)
		if got := DbVersion(ctx); got != 757 {
			t.Fatalf("boundary upgrade version=%d", got)
		}
		binding, err := prepareProviderPayoutBoundary(ctx, policy, policy.ConfigSha256)
		if err != nil || binding == nil {
			t.Fatal("relocated boundary preparation refused", err)
		}
		verifyMigrationCatalog(ctx, 757)
		redisAfter, catalogAfter, auditsAfter := currentPayoutMigrationWitness(t, ctx)
		if redisBefore != redisAfter || catalogBefore != catalogAfter || auditsAfter != auditsBefore+4 {
			t.Fatal("payout upgrade rewrote published schema/history or replayed migrations")
		}
		ApplyDbMigrationsUpTo(ctx, 757)
		again, err := prepareProviderPayoutBoundary(ctx, policy, policy.ConfigSha256)
		if err != nil || again == nil || *again != *binding {
			t.Fatal("restart replaced earning authority", err)
		}
		redisRepeat, catalogRepeat, auditsRepeat := currentPayoutMigrationWitness(t, ctx)
		if redisRepeat != redisAfter || catalogRepeat != catalogAfter || auditsRepeat != auditsAfter {
			t.Fatal("restart replayed migration or changed retained authority")
		}
	})
}

// An isolated 2a branch installed its bonus at index 754. Reproduce that exact
// schema/catalog identity, then require the current production runner to stop
// before any new SQL, audit update, or catalog relabeling.
func TestProviderCurrentMigrationRefusesConflictingBranchHistory(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		ApplyDbMigrationsUpTo(ctx, 754)
		const oldBonus = "0e1408f81b07a741a2618ff4309aec6b395ad4847d6881cfce146a2e9845fa2d"
		MaintenanceTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, providerPaymentBonusSchemaSql))
			RaisePgResult(tx.Exec(ctx, `INSERT INTO migration_audit(start_version_number,end_version_number,status) VALUES(754,755,'success')`))
			Raise(recordMigrationIdentity(ctx, tx, 754, oldBonus))
		})
		var beforeAudits int
		MaintenanceDb(ctx, func(conn PgConn) {
			Raise(conn.QueryRow(ctx, `SELECT count(*) FROM migration_audit`).Scan(&beforeAudits))
		}, OptReadOnly(), OptNoRetry())
		var refusal any
		func() {
			defer func() { refusal = recover() }()
			ApplyDbMigrations(ctx)
		}()
		if refusal == nil || !strings.Contains(fmt.Sprint(refusal), "migration 754 identity differs from durable catalog") {
			t.Fatal("conflicting old payout migration history was not refused", refusal)
		}
		if got := DbVersion(ctx); got != 755 {
			t.Fatal("refusal advanced old branch version", got)
		}
		MaintenanceDb(ctx, func(conn PgConn) {
			var identity string
			var count int
			var boundaryAbsent, redisAbsent bool
			Raise(conn.QueryRow(ctx, `SELECT identity_sha256 FROM migration_catalog WHERE migration_index=754`).Scan(&identity))
			Raise(conn.QueryRow(ctx, `SELECT count(*) FROM migration_audit`).Scan(&count))
			Raise(conn.QueryRow(ctx, `SELECT to_regclass('public.provider_payout_boundary') IS NULL, to_regclass('public.redis_contract_admission_policy') IS NULL`).Scan(&boundaryAbsent, &redisAbsent))
			if identity != oldBonus || count != beforeAudits || !boundaryAbsent || !redisAbsent {
				t.Fatal("conflict refusal mutated original branch evidence")
			}
		}, OptReadOnly(), OptNoRetry())
	})
}
