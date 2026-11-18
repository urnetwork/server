package model

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server/v2026"
)

// The row version (xmin), refresh block and measured speed of a client's
// location reliability row.
func testingClientLocationReliabilityState(ctx context.Context, clientId server.Id) (xmin string, updateBlockNumber int64, maxBytesPerSecond int64) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT xmin::text, update_block_number, max_bytes_per_second
			FROM network_client_location_reliability WHERE client_id = $1`,
			clientId,
		).Scan(&xmin, &updateBlockNumber, &maxBytesPerSecond))
	})
	return
}

// A pass rewrites a connected client's row only when a value changed, keeping
// its refresh block, and refreshes the block once it is a day old.
func TestUpdateClientLocationReliabilitiesRewritesOnlyChangedRows(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		location := &Location{
			LocationType: LocationTypeCity,
			City:         "location_write_city",
			Region:       "location_write_region",
			Country:      "Location Write Country",
			CountryCode:  "lw",
		}
		CreateLocation(ctx, location)
		networkId := server.NewId()
		clientId := server.NewId()
		testingConnectClientWithLocation(ctx, t, networkId, clientId, "192.0.2.61:20001", location)

		passTime := server.NowUtc().Truncate(ReliabilityBlockDuration)
		pass := func(maxTime time.Time) {
			UpdateClientLocationReliabilities(ctx, maxTime.Add(-time.Hour), maxTime)
		}

		pass(passTime)
		xmin, block, _ := testingClientLocationReliabilityState(ctx, clientId)
		if block != reliabilityBlockNumber(passTime) {
			t.Fatalf("first pass block=%d, want %d", block, reliabilityBlockNumber(passTime))
		}

		// the next pass, half an hour later, finds nothing changed
		pass(passTime.Add(30 * time.Minute))
		unchangedXmin, unchangedBlock, _ := testingClientLocationReliabilityState(ctx, clientId)
		if unchangedXmin != xmin || unchangedBlock != block {
			t.Fatalf("an unchanged pass rewrote the row: xmin %s -> %s, block %d -> %d", xmin, unchangedXmin, block, unchangedBlock)
		}

		// a measured speed changes a value: the row is rewritten, its block kept
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO network_client_speed (connection_id, bytes_per_second)
				SELECT connection_id, 4096 FROM network_client_connection WHERE client_id = $1 AND connected`,
				clientId,
			))
		})
		pass(passTime.Add(time.Hour))
		changedXmin, changedBlock, maxBytesPerSecond := testingClientLocationReliabilityState(ctx, clientId)
		if changedXmin == xmin || changedBlock != block || maxBytesPerSecond != 4096 {
			t.Fatalf("a changed value: xmin %s -> %s, block %d -> %d, speed %d; want a rewrite keeping the block", xmin, changedXmin, block, changedBlock, maxBytesPerSecond)
		}

		// a day on, the block is refreshed though nothing changed
		refreshTime := passTime.Add(ClientLocationReliabilityRefreshInterval + time.Minute)
		pass(refreshTime)
		refreshedXmin, refreshedBlock, _ := testingClientLocationReliabilityState(ctx, clientId)
		if refreshedXmin == changedXmin || refreshedBlock != reliabilityBlockNumber(refreshTime) {
			t.Fatalf("a day-old block: xmin %s -> %s, block %d, want %d", changedXmin, refreshedXmin, refreshedBlock, reliabilityBlockNumber(refreshTime))
		}

		// an earlier pass time never moves the block back
		pass(passTime.Add(2 * time.Hour))
		_, laterBlock, _ := testingClientLocationReliabilityState(ctx, clientId)
		if laterBlock != refreshedBlock {
			t.Fatalf("an earlier pass moved the block back: %d -> %d", refreshedBlock, laterBlock)
		}
	})
}

// The pass counts its clients by identity kind: a hosted child identity
// (source_client_id set) and a top-level client.
func TestUpdateClientLocationReliabilitiesCountsHostedChildren(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		location := &Location{
			LocationType: LocationTypeCity,
			City:         "hosted_share_city",
			Region:       "hosted_share_region",
			Country:      "Hosted Share Country",
			CountryCode:  "hs",
		}
		CreateLocation(ctx, location)
		networkId := server.NewId()
		parentClientId := server.NewId()
		childClientId := server.NewId()
		testingConnectClientWithLocation(ctx, t, networkId, parentClientId, "192.0.2.71:20001", location)
		testingConnectClientWithLocation(ctx, t, networkId, childClientId, "192.0.2.72:20001", location)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE network_client SET source_client_id = $2 WHERE client_id = $1`,
				childClientId,
				parentClientId,
			))
		})

		now := server.NowUtc()
		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)
		hostedChildCount := testutil.ToFloat64(clientLocationReliabilityPassClients.WithLabelValues("hosted_child"))
		topLevelCount := testutil.ToFloat64(clientLocationReliabilityPassClients.WithLabelValues("top_level"))
		writtenRowCount := testutil.ToFloat64(clientLocationReliabilityPassWrittenRows)
		if hostedChildCount != 1 || topLevelCount != 1 || writtenRowCount != 2 {
			t.Fatalf("pass counted hosted_child=%g top_level=%g written=%g, want 1, 1 and 2", hostedChildCount, topLevelCount, writtenRowCount)
		}

		// an unchanged pass writes nothing
		UpdateClientLocationReliabilities(ctx, now.Add(-30*time.Minute), now.Add(30*time.Minute))
		if writtenRowCount := testutil.ToFloat64(clientLocationReliabilityPassWrittenRows); writtenRowCount != 0 {
			t.Fatalf("an unchanged pass wrote %g rows", writtenRowCount)
		}
	})
}

// Each window's cutoff keeps a row its retention past its client's last pass
// even when its block lags that pass by the whole refresh interval, and
// deletes a row past both: 31 days after the last refresh for a top-level
// client, 11 days for a child or a gone client.
func TestClientLocationReliabilityRetentionAllowsForTheRefreshLag(t *testing.T) {
	settings := DefaultClientLocationReliabilityRetentionSettings()
	maxTime := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	for _, retention := range []time.Duration{settings.TopLevelRetention, settings.ShortRetention} {
		maxBlockNumber := clientLocationReliabilityCutoffBlockNumber(maxTime, retention)
		lastSeen := maxTime.Add(-retention).Add(ReliabilityBlockDuration)
		laggedBlock := reliabilityBlockNumber(lastSeen.Add(-ClientLocationReliabilityRefreshInterval))
		if laggedBlock <= maxBlockNumber {
			t.Fatalf("retention %s: a client last seen %s ago with a lagged block %d is past the cutoff %d", retention, maxTime.Sub(lastSeen), laggedBlock, maxBlockNumber)
		}
		expiredBlock := reliabilityBlockNumber(maxTime.Add(-retention - ClientLocationReliabilityRefreshInterval - ReliabilityBlockDuration))
		if maxBlockNumber < expiredBlock {
			t.Fatalf("retention %s: a block older than the retention and the lag, %d, is kept by the cutoff %d", retention, expiredBlock, maxBlockNumber)
		}
	}
	if days := (settings.TopLevelRetention + ClientLocationReliabilityRefreshInterval) / (24 * time.Hour); days != 31 {
		t.Fatalf("a top-level row is kept %d days after its last refresh, want 31", days)
	}
	if days := (settings.ShortRetention + ClientLocationReliabilityRefreshInterval) / (24 * time.Hour); days != 11 {
		t.Fatalf("a child's or gone client's row is kept %d days after its last refresh, want 11", days)
	}
}

// The trim deletes every row past the top-level window across batches and
// keeps the rest; it stops starting batches once its budget has passed, and
// the next run continues.
func TestRemoveOldClientLocationReliabilitiesBatches(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		maxTime := server.NowUtc()
		settings := DefaultClientLocationReliabilityRetentionSettings()
		maxBlockNumber := clientLocationReliabilityCutoffBlockNumber(maxTime, settings.TopLevelRetention)

		insert := func(updateBlockNumber int64) server.Id {
			clientId := server.NewId()
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`INSERT INTO network_client_location_reliability (client_id, network_id, update_block_number) VALUES ($1, $1, $2)`,
					clientId,
					updateBlockNumber,
				))
			})
			return clientId
		}
		expiredClientIds := []server.Id{}
		for i := range 5 {
			expiredClientIds = append(expiredClientIds, insert(maxBlockNumber-int64(i)))
		}
		// past 30 days, within the refresh lag
		lagClientId := insert(reliabilityBlockNumber(maxTime.Add(-settings.TopLevelRetention - time.Hour)))
		currentClientId := insert(reliabilityBlockNumber(maxTime))
		exists := func(clientId server.Id) (found bool) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(
					ctx,
					`SELECT EXISTS (SELECT 1 FROM network_client_location_reliability WHERE client_id = $1)`,
					clientId,
				).Scan(&found))
			})
			return
		}

		// a budget that has passed once the first batch is done
		startTime := maxTime
		clockReads := 0
		budgeted := DefaultClientLocationReliabilityRetentionSettings()
		budgeted.TrimBatchCount = 2
		budgeted.TrimBudget = time.Minute
		budgeted.now = func() time.Time {
			clockReads += 1
			if clockReads == 1 {
				return startTime
			}
			return startTime.Add(time.Minute)
		}
		if removedCount, more := budgeted.trim(ctx, maxTime); removedCount != 2 || !more {
			t.Fatalf("the budgeted trim removed %d rows (more %t), want one batch of 2 and more", removedCount, more)
		}

		drained := DefaultClientLocationReliabilityRetentionSettings()
		drained.TrimBatchCount = 2
		drained.TrimBudget = time.Hour
		drained.now = func() time.Time { return startTime }
		if removedCount, more := drained.trim(ctx, maxTime); removedCount != 3 || more {
			t.Fatalf("the next trim removed %d rows (more %t), want the other 3", removedCount, more)
		}
		for _, clientId := range expiredClientIds {
			if exists(clientId) {
				t.Fatalf("expired row %s survived", clientId)
			}
		}
		if !exists(lagClientId) || !exists(currentClientId) {
			t.Fatal("the trim removed a row within the top-level window and its refresh lag")
		}
	})
}

// The pass's gauges count its clients by identity kind and carry the rows its
// upsert wrote.
func TestRecordClientLocationReliabilityPassCountsIdentityKinds(t *testing.T) {
	clientLocationReliabilities := map[server.Id]*clientLocationReliability{
		server.NewId(): {hostedChild: true},
		server.NewId(): {hostedChild: true},
		server.NewId(): {hostedChild: false},
	}
	recordClientLocationReliabilityPass(clientLocationReliabilities, 1)
	hostedChildCount := testutil.ToFloat64(clientLocationReliabilityPassClients.WithLabelValues("hosted_child"))
	topLevelCount := testutil.ToFloat64(clientLocationReliabilityPassClients.WithLabelValues("top_level"))
	writtenRowCount := testutil.ToFloat64(clientLocationReliabilityPassWrittenRows)
	if hostedChildCount != 2 || topLevelCount != 1 || writtenRowCount != 1 {
		t.Fatalf("hosted_child=%g top_level=%g written=%g, want 2, 1 and 1", hostedChildCount, topLevelCount, writtenRowCount)
	}
}

// The brake narrows to the code's bounds: absent, the walk runs at the
// default rate; off, or any mode but on, stops it; a rate is capped at the
// maximum, and one that does not parse stops the walk.
func TestNarrowClientLocationReliabilityRetentionControl(t *testing.T) {
	settings := DefaultClientLocationReliabilityRetentionSettings()
	rate := settings.DefaultRowsPerSecond
	for _, test := range []struct {
		fields map[string]string
		rate   float64
	}{
		{fields: nil, rate: rate},
		{fields: map[string]string{"mode": "on"}, rate: rate},
		{fields: map[string]string{"mode": " on "}, rate: rate},
		{fields: map[string]string{"mode": "off"}, rate: 0},
		{fields: map[string]string{"mode": "paused", "maxRowsPerSecond": "500"}, rate: 0},
		{fields: map[string]string{"maxRowsPerSecond": "500"}, rate: 500},
		{fields: map[string]string{"mode": "on", "maxRowsPerSecond": "1e9"}, rate: settings.MaxRowsPerSecond},
		{fields: map[string]string{"maxRowsPerSecond": "0"}, rate: 0},
		{fields: map[string]string{"maxRowsPerSecond": "NaN"}, rate: 0},
		{fields: map[string]string{"maxRowsPerSecond": "fast"}, rate: 0},
	} {
		control := NarrowClientLocationReliabilityRetentionControl(test.fields, settings)
		if control.RowsPerSecond != test.rate || control.Present != (0 < len(test.fields)) {
			t.Fatalf("fields %v narrowed to %+v, want rate %v", test.fields, control, test.rate)
		}
	}
}

// The owner's windows (2026-11-16, "11 days except top-level"), on by default
// with no setting: a child's or gone client's row 11 days and a top-level
// client's 31 days after its last refresh, the top-level window as long as a
// top-level client stays active idle. A run that spends both budgets finishes
// inside its task's max time, so its post hands on the cursor.
func TestClientLocationReliabilityRetentionDefaults(t *testing.T) {
	settings := DefaultClientLocationReliabilityRetentionSettings()
	if settings.ShortRetention+ClientLocationReliabilityRefreshInterval != 11*24*time.Hour {
		t.Fatalf("short window %s after the last refresh, want 11 days", settings.ShortRetention+ClientLocationReliabilityRefreshInterval)
	}
	if settings.TopLevelRetention+ClientLocationReliabilityRefreshInterval != 31*24*time.Hour || settings.TopLevelRetention < TopLevelClientIdleExpiration {
		t.Fatalf("top-level window %s, shorter than a top-level client stays active idle (%s) or not 31 days after the last refresh", settings.TopLevelRetention, TopLevelClientIdleExpiration)
	}
	if !(0 < settings.DefaultRowsPerSecond) || settings.MaxRowsPerSecond < settings.DefaultRowsPerSecond || 2000 < settings.MaxRowsPerSecond {
		t.Fatalf("rates default %v max %v", settings.DefaultRowsPerSecond, settings.MaxRowsPerSecond)
	}
	if control := NarrowClientLocationReliabilityRetentionControl(nil, settings); !(0 < control.RowsPerSecond) {
		t.Fatal("the walk is not on without a brake")
	}
	if settings.TaskMaxTime < 2*(settings.TrimBudget+settings.WalkBudget) || settings.MoreInterval <= 0 || settings.RunInterval <= settings.MoreInterval {
		t.Fatalf("schedule %+v", settings)
	}
}
