// Customer challenge custody appends after the deployed financial catalog;
// installing it cannot reprice provider debt or replace the earning boundary.
package server

import (
	"context"
	"testing"
)

func customerPublishedMigrationWitness(t testing.TB, ctx context.Context) (string, string) {
	t.Helper()
	var catalog, boundary string
	MaintenanceDb(ctx, func(conn PgConn) {
		Raise(conn.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(c) ORDER BY migration_index)::text FROM migration_catalog c WHERE migration_index<757`).Scan(&catalog))
		Raise(conn.QueryRow(ctx, `SELECT row_to_json(b)::text FROM provider_payout_boundary b`).Scan(&boundary))
	}, OptReadOnly(), OptNoRetry())
	return catalog, boundary
}

func TestCustomerCurrentMigrationRetainsPublishedFinancialAuthority(t *testing.T) {
	if len(migrations) < 758 {
		t.Fatal("customer custody migration was not appended after the published financial prefix")
	}
	sql, ok := migrations[757].(*SqlMigration)
	if !ok || sql.sql != circleTransferRequestSchemaSql {
		t.Fatal("customer custody migration changed the published financial order")
	}
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		ApplyDbMigrationsUpTo(ctx, 757)
		_, policy := boundaryTestPolicy(t)
		binding, err := prepareProviderPayoutBoundary(ctx, policy, policy.ConfigSha256)
		if err != nil || binding == nil {
			t.Fatal("published earning boundary did not prepare", err)
		}
		if err := RequireCircleTransferSchema(ctx); err == nil {
			t.Fatal("uninstalled customer custody admitted")
		}
		catalog, boundary := customerPublishedMigrationWitness(t, ctx)
		redis, _, audits := currentPayoutMigrationWitness(t, ctx)
		ApplyDbMigrationsUpTo(ctx, 758)
		if DbVersion(ctx) != 758 {
			t.Fatal("customer custody upgrade did not complete")
		}
		if err := RequireCircleTransferSchema(ctx); err != nil {
			t.Fatal("appended customer capability refused", err)
		}
		if err := RequireProviderPayoutSchema(ctx); err != nil {
			t.Fatal("customer migration displaced provider capability", err)
		}
		afterCatalog, afterBoundary := customerPublishedMigrationWitness(t, ctx)
		afterRedis, _, afterAudits := currentPayoutMigrationWitness(t, ctx)
		if catalog != afterCatalog || boundary != afterBoundary || redis != afterRedis || afterAudits != audits+2 {
			t.Fatal("customer upgrade rewrote published financial authority or replayed its prefix")
		}
		ApplyDbMigrationsUpTo(ctx, 758)
		repeatedCatalog, repeatedBoundary := customerPublishedMigrationWitness(t, ctx)
		repeatedRedis, _, repeatedAudits := currentPayoutMigrationWitness(t, ctx)
		if repeatedCatalog != afterCatalog || repeatedBoundary != afterBoundary || repeatedRedis != afterRedis || repeatedAudits != afterAudits {
			t.Fatal("customer restart changed original financial authority")
		}
		again, err := prepareProviderPayoutBoundary(ctx, policy, policy.ConfigSha256)
		if err != nil || again == nil || *again != *binding {
			t.Fatal("customer upgrade replaced the earning boundary", err)
		}
	})
}
