// The client location reliability retention against PostgreSQL with the
// migrated schema (network_client_location_reliability_retention.go): the rows
// of children and gone clients go 11 days after their last refresh and those
// of top-level clients at 31, the provider status page still reads an offline
// provider's row at 20 days, the walk resumes from its cursor at its pace,
// the brake stops the walk and not the trim, and a failed batch keeps its
// cursor.
package model

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server"
)

// Sets the brake, or removes it with nil fields.
func setLocationReliabilityRetentionTestBrake(t testing.TB, fields map[string]string) {
	t.Helper()
	controlCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := RemoveClientLocationReliabilityRetentionControl(controlCtx); err != nil {
		t.Fatal(err)
	}
	if err := SetClientLocationReliabilityRetentionControl(controlCtx, fields); err != nil {
		t.Fatal(err)
	}
}

// A client of networkId whose parent is parentId, as a window mint writes it.
func insertLocationReliabilityRetentionTestChild(t testing.TB, networkId server.Id, parentId server.Id) server.Id {
	t.Helper()
	ctx := t.Context()
	childId := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO network_client (client_id, network_id, description, create_time, auth_time, source_client_id)
			VALUES ($1, $2, 'test', now(), now(), $3)
			`,
			childId,
			networkId,
			parentId,
		))
	})
	return childId
}

// A top-level client of networkId.
func insertLocationReliabilityRetentionTestTopLevel(t testing.TB, networkId server.Id) server.Id {
	t.Helper()
	clientId := server.NewId()
	Testing_CreateDevice(t.Context(), networkId, server.NewId(), clientId, "test", "test")
	return clientId
}

// The rows of a hosted child and of a gone client go once their block is at
// the short window's cutoff, 11 days after their last refresh; a block newer
// stays. A top-level client's row stays past it, at 20 days too, until the
// trim takes it at the top-level cutoff, 31 days, where a child's row past
// both goes as well. The walk examines only the rows between the cutoffs.
func TestClientLocationReliabilityRetentionKeepsTopLevelRowsToThirtyOneDays(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		settings := DefaultClientLocationReliabilityRetentionSettings()
		now := server.NowUtc()
		shortCutoff := clientLocationReliabilityCutoffBlockNumber(now, settings.ShortRetention)
		topLevelCutoff := clientLocationReliabilityCutoffBlockNumber(now, settings.TopLevelRetention)
		if shortCutoff != reliabilityBlockNumber(now.Add(-11*24*time.Hour))-1 || topLevelCutoff != reliabilityBlockNumber(now.Add(-31*24*time.Hour))-1 {
			t.Fatalf("cutoffs %d and %d are not 11 and 31 days back", shortCutoff, topLevelCutoff)
		}

		networkId := server.NewId()
		parentId := insertLocationReliabilityRetentionTestTopLevel(t, networkId)
		child := func() server.Id { return insertLocationReliabilityRetentionTestChild(t, networkId, parentId) }
		topLevel := func() server.Id { return insertLocationReliabilityRetentionTestTopLevel(t, networkId) }
		gone := func() server.Id { return server.NewId() }
		rows := []struct {
			name     string
			clientId server.Id
			block    int64
			kept     bool
		}{
			{name: "child at the short cutoff", clientId: child(), block: shortCutoff, kept: false},
			{name: "child a block newer", clientId: child(), block: shortCutoff + 1, kept: true},
			{name: "gone client at the short cutoff", clientId: gone(), block: shortCutoff, kept: false},
			{name: "gone client a block newer", clientId: gone(), block: shortCutoff + 1, kept: true},
			{name: "top-level client at the short cutoff", clientId: topLevel(), block: shortCutoff, kept: true},
			{name: "top-level client at 20 days", clientId: topLevel(), block: reliabilityBlockNumber(now.Add(-20 * 24 * time.Hour)), kept: true},
			{name: "top-level client a block newer than the top-level cutoff", clientId: topLevel(), block: topLevelCutoff + 1, kept: true},
			{name: "top-level client at the top-level cutoff", clientId: topLevel(), block: topLevelCutoff, kept: false},
			{name: "child past the top-level cutoff", clientId: child(), block: topLevelCutoff - 1, kept: false},
		}
		for _, row := range rows {
			insertLocationReliabilityRetentionTestRow(t, row.clientId, networkId, row.block)
		}

		deletedBefore := testutil.ToFloat64(retentionDeletedRowsCounter.WithLabelValues(RetentionTableNetworkClientLocationReliability))
		caughtUpBefore := testutil.ToFloat64(clientLocationReliabilityRetentionRunsCounter.WithLabelValues(ClientLocationReliabilityRetentionStopCaughtUp))
		report, cursor := RemoveOldClientLocationReliabilities(ctx, now, ClientLocationReliabilityRetentionCursor{}, settings)
		for _, row := range rows {
			if exists := locationReliabilityRetentionTestRowExists(t, row.clientId); exists != row.kept {
				t.Errorf("%s: exists=%t, want %t", row.name, exists, row.kept)
			}
		}
		if report.TrimmedCount != 2 || report.ExaminedCount != 5 || report.RemovedCount != 2 ||
			report.Stop != ClientLocationReliabilityRetentionStopCaughtUp || report.More {
			t.Fatalf("report %+v, want 2 trimmed, then 2 removed of 5 examined and caught up", report)
		}
		if cursor.AfterUpdateBlockNumber == nil || *cursor.AfterUpdateBlockNumber != shortCutoff {
			t.Fatalf("the caught-up walk left cursor %+v, want the last row at the short cutoff", cursor)
		}
		if deleted := testutil.ToFloat64(retentionDeletedRowsCounter.WithLabelValues(RetentionTableNetworkClientLocationReliability)) - deletedBefore; deleted != 4 {
			t.Fatalf("%v deletes counted, want 4", deleted)
		}
		if caughtUp := testutil.ToFloat64(clientLocationReliabilityRetentionRunsCounter.WithLabelValues(ClientLocationReliabilityRetentionStopCaughtUp)) - caughtUpBefore; caughtUp != 1 {
			t.Fatalf("%v caught-up runs counted", caughtUp)
		}
		cursorTime := time.UnixMilli(shortCutoff * ReliabilityBlockDuration.Milliseconds())
		if gauge := testutil.ToFloat64(clientLocationReliabilityRetentionCursorGauge); gauge != float64(cursorTime.Unix()) {
			t.Fatalf("cursor gauge %v, want %d", gauge, cursorTime.Unix())
		}
	})
}

// The provider status page reads every active top-level client's row with no
// time bound, and a top-level client stays active 30 days idle: a provider
// offline for 20 days still shows the valid location and country of its row
// once the retention has run, while a child's row as old is gone.
func TestProviderStatusFindsATopLevelRowAtTwentyDays(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		city := egressTestCity(ctx, "Retention City", "Retention Region", "Retention Country", "rc")
		networkId := server.NewId()
		clientId := insertLocationReliabilityRetentionTestTopLevel(t, networkId)
		SetProvide(ctx, clientId, egressTestPublicAndNetwork)
		childId := insertLocationReliabilityRetentionTestChild(t, networkId, clientId)
		twentyDaysBlock := reliabilityBlockNumber(now.Add(-20 * 24 * time.Hour))
		// valid, as generated from one address and one location in a country;
		// disconnected
		for _, rowClientId := range []server.Id{clientId, childId} {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
					INSERT INTO network_client_location_reliability (
						client_id, network_id, update_block_number,
						city_location_id, region_location_id, country_location_id,
						client_address_hash_count, location_count, connected
					) VALUES ($1, $2, $3, $4, $5, $6, 1, 1, false)
					`,
					rowClientId,
					networkId,
					twentyDaysBlock,
					city.LocationId,
					city.RegionLocationId,
					city.CountryLocationId,
				))
			})
		}
		status := func() *ProviderStatus {
			t.Helper()
			status := GetClientProviderStatus(ctx, networkId, clientId)
			if status == nil {
				t.Fatal("no status for the provider")
			}
			return status
		}
		if before := status(); !before.Admission.LocationValid || before.Country == nil || before.Country.CountryCode != "rc" {
			t.Fatalf("before the retention the status reads admission %+v country %+v", before.Admission, before.Country)
		}

		report, _ := RemoveOldClientLocationReliabilities(ctx, now, ClientLocationReliabilityRetentionCursor{}, DefaultClientLocationReliabilityRetentionSettings())
		if locationReliabilityRetentionTestRowExists(t, childId) {
			t.Fatalf("the retention %+v kept the child's 20-day row", report)
		}
		after := status()
		if !after.Admission.LocationValid || after.Admission.Connected || after.Country == nil || after.Country.CountryCode != "rc" {
			t.Fatalf("after the retention %+v the status reads admission %+v country %+v, want the 20-day row's valid location in rc", report, after.Admission, after.Country)
		}
		if report.RemovedCount != 1 {
			t.Fatalf("the retention %+v removed more than the child's row", report)
		}
	})
}

// At one row a second a batch of two ends the run, since the next batch could
// not start inside its budget, and the next run resumes after the cursor. The
// top-level row the first run passed over is never examined again, and a row
// that crosses the cutoff later is ahead of the cursor.
func TestClientLocationReliabilityRetentionResumesFromItsCursorAtItsPace(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		settings := DefaultClientLocationReliabilityRetentionSettings()
		settings.WalkBatchCount = 2
		settings.WalkBudget = time.Second
		now := server.NowUtc()
		cutoff := clientLocationReliabilityCutoffBlockNumber(now, settings.ShortRetention)
		networkId := server.NewId()
		topLevelId := insertLocationReliabilityRetentionTestTopLevel(t, networkId)
		childId := insertLocationReliabilityRetentionTestChild(t, networkId, topLevelId)
		goneIds := []server.Id{server.NewId(), server.NewId(), server.NewId()}
		// in walk order: the top-level row, a gone client's, a gone client's,
		// the child's, a gone client's
		insertLocationReliabilityRetentionTestRow(t, topLevelId, networkId, cutoff-5)
		insertLocationReliabilityRetentionTestRow(t, goneIds[0], networkId, cutoff-4)
		insertLocationReliabilityRetentionTestRow(t, goneIds[1], networkId, cutoff-3)
		insertLocationReliabilityRetentionTestRow(t, childId, networkId, cutoff-2)
		insertLocationReliabilityRetentionTestRow(t, goneIds[2], networkId, cutoff-1)

		setLocationReliabilityRetentionTestBrake(t, map[string]string{ClientLocationReliabilityRetentionControlFieldMaxRowsPerSecond: "1"})
		report, cursor := RemoveOldClientLocationReliabilities(ctx, now, ClientLocationReliabilityRetentionCursor{}, settings)
		if report.Stop != ClientLocationReliabilityRetentionStopBudget || !report.More || report.ExaminedCount != 2 || report.RemovedCount != 1 ||
			cursor.AfterClientId == nil || *cursor.AfterClientId != goneIds[0] || *cursor.AfterUpdateBlockNumber != cutoff-4 {
			t.Fatalf("the first run %+v left cursor %+v, want one batch of 2 and more", report, cursor)
		}
		if report, cursor = RemoveOldClientLocationReliabilities(ctx, now, cursor, settings); report.Stop != ClientLocationReliabilityRetentionStopBudget ||
			report.ExaminedCount != 2 || report.RemovedCount != 2 || *cursor.AfterClientId != childId {
			t.Fatalf("the resumed run %+v left cursor %+v", report, cursor)
		}

		setLocationReliabilityRetentionTestBrake(t, nil)
		settings.WalkBudget = time.Minute
		if report, cursor = RemoveOldClientLocationReliabilities(ctx, now, cursor, settings); report.Stop != ClientLocationReliabilityRetentionStopCaughtUp ||
			report.More || report.ExaminedCount != 1 || report.RemovedCount != 1 || *cursor.AfterClientId != goneIds[2] {
			t.Fatalf("the last run %+v left cursor %+v, want the last row and caught up", report, cursor)
		}

		// an hour on, a gone client's row refreshed then crosses the cutoff
		laterGoneId := server.NewId()
		insertLocationReliabilityRetentionTestRow(t, laterGoneId, networkId, cutoff+30)
		if report, cursor = RemoveOldClientLocationReliabilities(ctx, now.Add(time.Hour), cursor, settings); report.ExaminedCount != 1 ||
			report.RemovedCount != 1 || *cursor.AfterClientId != laterGoneId {
			t.Fatalf("an hour later the walk %+v left cursor %+v, want only the row that crossed the cutoff", report, cursor)
		}
		if !locationReliabilityRetentionTestRowExists(t, topLevelId) {
			t.Fatal("the walk removed the top-level row")
		}
		for _, clientId := range append(goneIds, childId, laterGoneId) {
			if locationReliabilityRetentionTestRowExists(t, clientId) {
				t.Fatalf("row %s stayed", clientId)
			}
		}
	})
}

// The brake stops the walk when off, when its rate is zero or does not parse,
// and when it cannot be read, while the trim takes the rows past 31 days as
// before; without the brake the walk runs.
func TestClientLocationReliabilityRetentionBrake(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		settings := DefaultClientLocationReliabilityRetentionSettings()
		now := server.NowUtc()
		shortCutoff := clientLocationReliabilityCutoffBlockNumber(now, settings.ShortRetention)
		topLevelCutoff := clientLocationReliabilityCutoffBlockNumber(now, settings.TopLevelRetention)
		networkId := server.NewId()
		goneId := server.NewId()
		insertLocationReliabilityRetentionTestRow(t, goneId, networkId, shortCutoff)
		run := func(expired bool) *ClientLocationReliabilityRetentionReport {
			t.Helper()
			expiredId := server.NewId()
			if expired {
				insertLocationReliabilityRetentionTestRow(t, expiredId, networkId, topLevelCutoff)
			}
			report, _ := RemoveOldClientLocationReliabilities(ctx, now, ClientLocationReliabilityRetentionCursor{}, settings)
			if expired && (report.TrimmedCount != 1 || locationReliabilityRetentionTestRowExists(t, expiredId)) {
				t.Fatalf("the trim did not run under the brake: %+v", report)
			}
			return report
		}

		for _, fields := range []map[string]string{
			{ClientLocationReliabilityRetentionControlFieldMode: "off"},
			{ClientLocationReliabilityRetentionControlFieldMode: "paused", ClientLocationReliabilityRetentionControlFieldMaxRowsPerSecond: "500"},
			{ClientLocationReliabilityRetentionControlFieldMaxRowsPerSecond: "0"},
			{ClientLocationReliabilityRetentionControlFieldMaxRowsPerSecond: "fast"},
		} {
			setLocationReliabilityRetentionTestBrake(t, fields)
			if report := run(true); report.Stop != ClientLocationReliabilityRetentionStopBrakeOff || report.ExaminedCount != 0 || !locationReliabilityRetentionTestRowExists(t, goneId) {
				t.Fatalf("brake %v: %+v", fields, report)
			}
		}
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Set(ctx, clientLocationReliabilityRetentionControlKey, "not a hash", 0).Err())
		})
		if report := run(true); report.Stop != ClientLocationReliabilityRetentionStopBrakeUnread || !locationReliabilityRetentionTestRowExists(t, goneId) {
			t.Fatalf("an unreadable brake: %+v", report)
		}
		setLocationReliabilityRetentionTestBrake(t, map[string]string{ClientLocationReliabilityRetentionControlFieldMode: "on"})
		if report := run(false); report.Stop != ClientLocationReliabilityRetentionStopCaughtUp || report.RemovedCount != 1 || locationReliabilityRetentionTestRowExists(t, goneId) {
			t.Fatalf("mode on: %+v", report)
		}
	})
}

// A walk batch that fails ends the run with an error and the cursor of the
// last committed batch, after the trim has run, and the next run retries it.
func TestClientLocationReliabilityRetentionFailedBatchKeepsItsCursor(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		settings := DefaultClientLocationReliabilityRetentionSettings()
		now := server.NowUtc()
		networkId := server.NewId()
		goneId, expiredId := server.NewId(), server.NewId()
		insertLocationReliabilityRetentionTestRow(t, goneId, networkId, clientLocationReliabilityCutoffBlockNumber(now, settings.ShortRetention))
		insertLocationReliabilityRetentionTestRow(t, expiredId, networkId, clientLocationReliabilityCutoffBlockNumber(now, settings.TopLevelRetention))
		start := ClientLocationReliabilityRetentionCursor{}
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE network_client RENAME TO network_client_away`))
		})
		report, cursor := RemoveOldClientLocationReliabilities(ctx, now, start, settings)
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE network_client_away RENAME TO network_client`))
		})
		if report.Stop != ClientLocationReliabilityRetentionStopError || !report.More || cursor != start ||
			report.TrimmedCount != 1 || !locationReliabilityRetentionTestRowExists(t, goneId) {
			t.Fatalf("a failing walk batch: %+v, cursor %+v", report, cursor)
		}
		if report, _ = RemoveOldClientLocationReliabilities(ctx, now, cursor, settings); report.Stop != ClientLocationReliabilityRetentionStopCaughtUp || report.RemovedCount != 1 {
			t.Fatalf("the retry: %+v", report)
		}
	})
}
