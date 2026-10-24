package work

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// The registry chains' cadences: the runway runs more often than retention,
// a run that left work behind retries sooner, and every budget fits its run.
func TestPartitionRegistryTaskSettings(t *testing.T) {
	settings := DefaultPartitionRegistryTaskSettings()
	if settings.RunwayInterval <= 0 || settings.RetentionInterval < settings.RunwayInterval ||
		settings.RetentionRetryInterval <= 0 || settings.RetentionInterval <= settings.RetentionRetryInterval ||
		settings.RetentionBudget <= 0 || settings.MaxTime <= settings.RetentionBudget {
		t.Fatalf("registry task settings %+v", settings)
	}
}

// Each chain re-arms in its post: the runway after its interval, and the
// retention after its interval, or a minute after a run that left work
// behind, carrying the families' resume state into the next run.
func TestPartitionRegistryPostsRearmTheChains(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:0", nil)
		defer clientSession.Cancel()
		settings := DefaultPartitionRegistryTaskSettings()

		before := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			connect.AssertEqual(t, MaintainPartitionRunwayPost(&MaintainPartitionRunwayArgs{}, &MaintainPartitionRunwayResult{}, clientSession, tx), nil)
		})
		runAt := testExtenderTaskRunAt(t, ctx, "maintain_partition_runway")
		if want := before.Add(settings.RunwayInterval); runAt.Before(want.Add(-time.Second)) || want.Add(5*time.Second).Before(runAt) {
			t.Fatalf("runway run_at = %s, want about %s", runAt, want)
		}

		state := server.PartitionMaintenanceState{
			Families:     map[string]server.PartitionFamilyState{"synthetic": {AnalyzedPeriod: "2026-10-24", ArchiveLeaf: "synthetic_p20261001", ArchiveCursor: "cursor"}},
			ResumeFamily: "synthetic",
		}
		before = server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			connect.AssertEqual(t, MaintainPartitionRetentionPost(&MaintainPartitionRetentionArgs{}, &MaintainPartitionRetentionResult{State: state, More: true}, clientSession, tx), nil)
		})
		runAt = testExtenderTaskRunAt(t, ctx, "maintain_partition_retention")
		if want := before.Add(settings.RetentionRetryInterval); runAt.Before(want.Add(-time.Second)) || want.Add(5*time.Second).Before(runAt) {
			t.Fatalf("retention retry run_at = %s, want about %s", runAt, want)
		}
		var argsJson string
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT args_json FROM pending_task WHERE run_once_key = $1`, fmt.Sprintf("[%q]", "maintain_partition_retention")).Scan(&argsJson))
		})
		var args MaintainPartitionRetentionArgs
		server.Raise(json.Unmarshal([]byte(argsJson), &args))
		connect.AssertEqual(t, args.State.ResumeFamily, "synthetic")
		connect.AssertEqual(t, args.State.Families["synthetic"], state.Families["synthetic"])
	})
}
