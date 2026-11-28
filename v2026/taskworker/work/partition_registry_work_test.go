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

// The retention chain reaches each range's new leaves a minute after the
// range opens: at the cadence's time unless the open-leaf analyze comes
// first, at once when that passed while the run ran, and on the cadence alone
// for a result from a build without it.
func TestMaintainPartitionRetentionNextRunAtMeetsTheOpenedRange(t *testing.T) {
	settings := DefaultPartitionRegistryTaskSettings()
	before := time.Date(2026, 11, 25, 23, 55, 0, 0, time.UTC)
	opened := time.Date(2026, 11, 26, 0, 1, 0, 0, time.UTC)
	for _, c := range []struct {
		result MaintainPartitionRetentionResult
		now    time.Time
		want   time.Time
	}{
		// the cadence alone runs at 00:10
		{result: MaintainPartitionRetentionResult{OpenLeafAnalyzeAt: opened}, now: before, want: opened},
		{result: MaintainPartitionRetentionResult{More: true, OpenLeafAnalyzeAt: opened}, now: before, want: before.Add(settings.RetentionRetryInterval)},
		{result: MaintainPartitionRetentionResult{OpenLeafAnalyzeAt: opened}, now: before.Add(-time.Hour), want: before.Add(-time.Hour).Add(settings.RetentionInterval)},
		// a run that started before 00:01 and ended after it
		{result: MaintainPartitionRetentionResult{OpenLeafAnalyzeAt: opened}, now: opened.Add(4 * time.Minute), want: opened.Add(4 * time.Minute)},
		{result: MaintainPartitionRetentionResult{}, now: before, want: before.Add(settings.RetentionInterval)},
	} {
		if got := maintainPartitionRetentionNextRunAt(settings, &c.result, c.now); !got.Equal(c.want) {
			t.Errorf("result %+v at %s: next run %s, want %s", c.result, c.now, got, c.want)
		}
	}
}

// A run that left a family's opened leaf unanalyzed runs again after the
// retry interval, not the cadence.
func TestMaintainPartitionRetentionNextRunAtRetriesALeftOpenedLeaf(t *testing.T) {
	settings := DefaultPartitionRegistryTaskSettings()
	ended := time.Date(2026, 11, 28, 0, 1, 30, 0, time.UTC)
	tomorrow := time.Date(2026, 11, 29, 0, 1, 0, 0, time.UTC)
	result := MaintainPartitionRetentionResult{OpenLeafRetry: true, OpenLeafAnalyzeAt: tomorrow}
	if got, want := maintainPartitionRetentionNextRunAt(settings, &result, ended), ended.Add(settings.RetentionRetryInterval); !got.Equal(want) {
		t.Errorf("a run that left an opened leaf: next run %s, want %s", got, want)
	}
	result.OpenLeafRetry = false
	if got, want := maintainPartitionRetentionNextRunAt(settings, &result, ended), ended.Add(settings.RetentionInterval); !got.Equal(want) {
		t.Errorf("a run that left nothing: next run %s, want %s", got, want)
	}
}
