package controller

import (
	"context"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The sample generator must refuse to write fake audit rows in any
// non-development env. The guard runs before any db access, so no test env is
// needed for the refusal paths.
func TestAddSampleEventsForTestingRefusesProductionEnv(t *testing.T) {
	for _, env := range []string{"main", "canary", "staging"} {
		t.Setenv("WARP_ENV", env)
		if err := AddSampleEventsForTesting(context.Background(), 60); err == nil {
			t.Fatalf("expected refusal in env %q", env)
		}
	}
	t.Setenv("WARP_ENV", "")
	if err := AddSampleEventsForTesting(context.Background(), 60); err == nil {
		t.Fatalf("expected refusal when WARP_ENV is unset")
	}
}

// In a development env the generator runs and every fake row carries the
// sample provenance marker, so it stays distinguishable from real data and
// purgeable via PurgeSampleAuditEvents.
func TestAddSampleEventsForTestingMarksSampleRows(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		if err := AddSampleEventsForTesting(ctx, 60); err != nil {
			t.Fatalf("sample events in local env: %v", err)
		}

		unmarked := -1
		marked := -1
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
				SELECT
					COUNT(*) FILTER (WHERE event_details IS NULL),
					COUNT(*) FILTER (WHERE event_details = $1)
				FROM audit_provider_event
				`,
				model.AuditEventDetailsSample,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&unmarked, &marked))
				}
			})
		})
		if unmarked != 0 || marked != 1 {
			t.Fatalf("provider rows unmarked=%d marked=%d, want 0/1", unmarked, marked)
		}

		// the marker is what makes sample rows purgeable
		providerCount, contractCount := model.PurgeSampleAuditEvents(ctx)
		if providerCount != 1 || contractCount != 1 {
			t.Fatalf(
				"purge removed (%d, %d) sample rows, want (1, 1)",
				providerCount, contractCount,
			)
		}
	})
}
