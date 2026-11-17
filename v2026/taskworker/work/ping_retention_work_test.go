package work

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// The ping retention sweep (connect/GEOMAP.md §5.7, D18, D26): the day
// partitions it keeps ahead and drops behind, that it never deletes a ping
// row, its chain, and the derived rows it removes.

// A ping is read for a day, the sweep runs hourly well inside that, the days
// ahead outlast any sweep outage shorter than a day, and a row is kept past
// its retention for as long as a copy of its claim could still arrive. Pure.
func TestPingRetentionCadence(t *testing.T) {
	settings := DefaultRemoveExpiredPingsSettings()
	reportSettings := settings.ReportSettings
	connect.AssertEqual(t, reportSettings.Retention, 24*time.Hour)
	connect.AssertEqual(t, reportSettings.SweepTimeout, time.Hour)
	if reportSettings.Retention <= reportSettings.SweepTimeout {
		t.Fatal("the sweep runs less often than a ping is read")
	}
	if settings.PartitionSettings.AheadDays < 1 {
		t.Fatalf("the sweep keeps %d days ahead, so one missed run can leave an insert without its day", settings.PartitionSettings.AheadDays)
	}
	if reportSettings.KeepTimeout() < reportSettings.Retention+reportSettings.MaxForwardClockSkew+reportSettings.SweepTimeout {
		t.Fatalf("a row is kept %s, less than the retention, the forward skew and a sweep interval", reportSettings.KeepTimeout())
	}
}

// A test ping created at `createTime`, co-signed, with a fresh pinger and nonce.
func testRetentionPing(targetExtenderId server.Id, createTime time.Time) *model.NetworkPing {
	return &model.NetworkPing{
		PingerKind:       model.NetworkPingPingerKindExtender,
		PingerId:         server.NewId(),
		TargetExtenderId: targetExtenderId,
		ProbeNonce:       server.NewId().Bytes(),
		RttMs:            20,
		ProbeTime:        createTime,
		Cosign:           model.NetworkPingCosignCosigned,
		PingerSignature:  []byte("pinger-signature"),
		Cosignature:      []byte("cosignature"),
		CreateTime:       createTime,
	}
}

// The tables the sweep keeps in day partitions, in the order it keeps them:
// network_ping and the tallies that grow with it.
var testRetentionPartitionedTables = []string{
	"network_ping",
	"network_ping_pinger_day",
	"network_ping_target_day",
	"network_ping_target_hour_tally",
}

// The names of the partitions attached to `table`, in name order.
func testRetentionPartitionNames(ctx context.Context, table string) []string {
	partitionNames := []string{}
	for _, partition := range model.GetNetworkPingTablePartitions(ctx, table) {
		partitionNames = append(partitionNames, partition.Name)
	}
	return partitionNames
}

// The names of the partitions of every day-kept table for `days`, in the
// sweep's order: table by table, day by day.
func testRetentionDayPartitionNames(days ...time.Time) []string {
	partitionNames := []string{}
	for _, table := range testRetentionPartitionedTables {
		for _, day := range days {
			partitionNames = append(partitionNames, table+"_p"+day.UTC().Format("20060102"))
		}
	}
	return partitionNames
}

// The sweep creates the days ahead that are missing and drops every day whose
// pings are all past their keep timeout, whole, in network_ping and in every
// tally kept by day, and deletes the fleet's hour tally behind the oldest ping
// day it keeps; yesterday and today stay with their rows, and a second sweep
// finds nothing to do. The clock is a synthetic noon a month out, so the real
// days the migrations made are all past it.
func TestRemoveExpiredPingsKeepsDaysAheadAndDropsDaysBehind(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultRemoveExpiredPingsSettings()
		today := server.NowUtc().AddDate(0, 1, 0).Truncate(24 * time.Hour)
		now := today.Add(12 * time.Hour)
		tableRealPartitionNames := map[string][]string{}
		for _, table := range testRetentionPartitionedTables {
			tableRealPartitionNames[table] = testRetentionPartitionNames(ctx, table)
		}

		// the days behind have no partition until their insert makes one, in
		// every table
		targetExtenderId := server.NewId()
		threeDaysBack := today.AddDate(0, 0, -3)
		twoDaysBack := today.AddDate(0, 0, -2)
		yesterday := today.AddDate(0, 0, -1)
		connect.AssertEqual(t, model.AddNetworkPings(ctx, []*model.NetworkPing{
			testRetentionPing(targetExtenderId, threeDaysBack.Add(12*time.Hour)),
			testRetentionPing(targetExtenderId, twoDaysBack.Add(23*time.Hour)),
			testRetentionPing(targetExtenderId, yesterday.Add(time.Minute)),
			testRetentionPing(targetExtenderId, now),
		}), 4)

		result := removeExpiredPings(ctx, now, settings)
		aheadDays := []time.Time{}
		for i := 1; i <= settings.PartitionSettings.AheadDays; i += 1 {
			aheadDays = append(aheadDays, today.AddDate(0, 0, i))
		}
		connect.AssertEqual(t, result.CreatedPingPartitions, testRetentionDayPartitionNames(aheadDays...))
		wantDroppedPartitionNames := []string{}
		for _, table := range testRetentionPartitionedTables {
			wantDroppedPartitionNames = append(wantDroppedPartitionNames, tableRealPartitionNames[table]...)
			wantDroppedPartitionNames = append(wantDroppedPartitionNames,
				table+"_p"+threeDaysBack.Format("20060102"),
				table+"_p"+twoDaysBack.Format("20060102"),
			)
		}
		connect.AssertEqual(t, result.DroppedPingPartitions, wantDroppedPartitionNames)
		// the two dropped days' pings each had an hour of the fleet's tally
		connect.AssertEqual(t, result.RemovedPingHourTallies, 2)

		keptDays := []time.Time{yesterday}
		for day := today; !today.AddDate(0, 0, settings.PartitionSettings.AheadDays).Before(day); day = day.AddDate(0, 0, 1) {
			keptDays = append(keptDays, day)
		}
		for _, table := range testRetentionPartitionedTables {
			wantPartitionNames := []string{}
			for _, day := range keptDays {
				wantPartitionNames = append(wantPartitionNames, table+"_p"+day.Format("20060102"))
			}
			connect.AssertEqual(t, testRetentionPartitionNames(ctx, table), wantPartitionNames)
		}
		// the dropped days took their rows with them, and nothing else did
		keptPings := model.GetNetworkPings(ctx, targetExtenderId, time.Time{})
		connect.AssertEqual(t, len(keptPings), 2)
		for _, keptPing := range keptPings {
			if keptPing.CreateTime.Before(yesterday) {
				t.Fatalf("a ping of %s survived its day's drop", keptPing.CreateTime)
			}
		}

		result = removeExpiredPings(ctx, now.Add(settings.ReportSettings.SweepTimeout), settings)
		connect.AssertEqual(t, len(result.CreatedPingPartitions), 0)
		connect.AssertEqual(t, len(result.DroppedPingPartitions), 0)
		connect.AssertEqual(t, result.RemovedPingHourTallies, 0)
	})
}

// A day stays past the retention for as long as its keep timeout: a sweep
// after the day's last ping passed the retention but before it passed the keep
// timeout keeps the day and its rows -- a copy of one of its claims could
// still arrive, and must find its row -- and the first sweep past the keep
// timeout drops it, in every table kept by day.
func TestRemoveExpiredPingsKeepsADayUntilItsKeepTimeout(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultRemoveExpiredPingsSettings()
		reportSettings := settings.ReportSettings
		day := server.NowUtc().AddDate(0, 1, 0).Truncate(24 * time.Hour)
		dayEnd := day.AddDate(0, 0, 1)
		removeExpiredPings(ctx, day, settings)
		targetExtenderId := server.NewId()
		connect.AssertEqual(t, model.AddNetworkPings(ctx, []*model.NetworkPing{
			testRetentionPing(targetExtenderId, dayEnd.Add(-time.Minute)),
		}), 1)
		if reportSettings.KeepTimeout() <= reportSettings.Retention {
			t.Fatal("the keep timeout is no longer than the retention, so this proves nothing")
		}

		// past the retention, inside the keep timeout
		result := removeExpiredPings(ctx, dayEnd.Add(reportSettings.Retention+time.Minute), settings)
		for _, droppedPartitionName := range result.DroppedPingPartitions {
			if droppedPartitionName == model.NetworkPingPartitionName(day) {
				t.Fatal("the sweep dropped a day inside its keep timeout")
			}
		}
		connect.AssertEqual(t, len(model.GetNetworkPings(ctx, targetExtenderId, time.Time{})), 1)

		// the last sweep inside it, and the first past it
		result = removeExpiredPings(ctx, dayEnd.Add(reportSettings.KeepTimeout()), settings)
		for _, droppedPartitionName := range result.DroppedPingPartitions {
			if droppedPartitionName == model.NetworkPingPartitionName(day) {
				t.Fatal("the sweep dropped a day at its keep timeout, not past it")
			}
		}
		result = removeExpiredPings(ctx, dayEnd.Add(reportSettings.KeepTimeout()+time.Millisecond), settings)
		connect.AssertEqual(t, result.DroppedPingPartitions, testRetentionDayPartitionNames(day))
		connect.AssertEqual(t, len(model.GetNetworkPings(ctx, targetExtenderId, time.Time{})), 0)
	})
}

// The sweep never deletes a ping row (§5.7, D26): no string in the code of the
// packages that write network_ping -- the table's model, the sweep and the
// ingest -- holds a delete from it, from a tally kept by day, or from a
// partition of either, while the model's drop of a partition is there, and so
// is the one delete the design allows, of whole days of the small fleet-wide
// hour tally. A statement pieced together at run time is outside what this
// reads. Pure.
func TestRemoveExpiredPingsNeverDeletesPingRows(t *testing.T) {
	deletePattern := regexp.MustCompile(`(?is)\bdelete\s+from\s+(only\s+)?("?public"?\s*\.\s*)?"?(network_ping_pinger_day|network_ping_target_day|network_ping_target_hour_tally|network_ping(_p[0-9]|\b))`)
	dropPattern := regexp.MustCompile(`(?is)\bdrop\s+table\s+if\s+exists\s+%s`)
	hourTallyDeletePattern := regexp.MustCompile(`(?is)\bdelete\s+from\s+network_ping_hour_tally\s+where\s+hour\s*<`)
	// the pattern must see every spelling of a forbidden delete, and not the
	// allowed one, or passing proves nothing
	for _, rowDelete := range []string{
		"\n\t\t\tDELETE FROM network_ping\n\t\t\tWHERE create_time < $1\n",
		"delete from only public.network_ping where true",
		`DELETE FROM "network_ping" USING stale WHERE true`,
		"DELETE FROM network_ping_p20310310",
		"DELETE FROM network_ping_pinger_day WHERE day < $1",
		"DELETE FROM network_ping_target_day_p20310310",
		"DELETE FROM network_ping_target_hour_tally WHERE hour < $1",
	} {
		if !deletePattern.MatchString(rowDelete) {
			t.Fatalf("the pattern misses the row delete %q", rowDelete)
		}
	}
	if deletePattern.MatchString("DELETE FROM network_ping_hour_tally WHERE hour < $1") {
		t.Fatal("the pattern forbids the hour tally's whole-day delete")
	}
	sourcePaths := []string{}
	for _, directory := range []string{"../../model", ".", "../../controller"} {
		directoryPaths, err := filepath.Glob(filepath.Join(directory, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range directoryPaths {
			if !strings.HasSuffix(path, "_test.go") {
				sourcePaths = append(sourcePaths, path)
			}
		}
	}
	for _, wantPath := range []string{
		filepath.Join("../../model", "network_ping_model.go"),
		"ping_retention_work.go",
		filepath.Join("../../controller", "extender_ping_controller.go"),
	} {
		if !slices.Contains(sourcePaths, wantPath) {
			t.Fatalf("%s is not among the sources read", wantPath)
		}
	}

	fileSet := token.NewFileSet()
	dropFound := false
	hourTallyDeleteFound := false
	for _, path := range sourcePaths {
		file, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Fatalf("%s: %v", fileSet.Position(literal.Pos()), err)
			}
			if deletePattern.MatchString(value) {
				t.Errorf("%s: a row delete on network_ping or a day-kept tally: %q", fileSet.Position(literal.Pos()), value)
			}
			if filepath.Base(path) == "network_ping_model.go" {
				dropFound = dropFound || dropPattern.MatchString(value)
				hourTallyDeleteFound = hourTallyDeleteFound || hourTallyDeletePattern.MatchString(value)
			}
			return true
		})
	}
	if !dropFound {
		t.Fatal("the model has no partition drop, so the reading above proves nothing")
	}
	if !hourTallyDeleteFound {
		t.Fatal("the model has no whole-day delete of the hour tally")
	}
}

// The chain re-arms every sweep interval; a Post that did not reschedule
// would let yesterday's partition outlive its keep timeout.
func TestRemoveExpiredPingsPostRearmsTheChain(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:0", nil)
		defer clientSession.Cancel()

		before := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			if err := RemoveExpiredPingsPost(
				&RemoveExpiredPingsArgs{},
				&RemoveExpiredPingsResult{},
				clientSession,
				tx,
			); err != nil {
				t.Fatalf("RemoveExpiredPingsPost: %v", err)
			}
		})

		runAt := testExtenderTaskRunAt(t, ctx, "remove_expired_pings")
		want := before.Add(DefaultRemoveExpiredPingsSettings().ReportSettings.SweepTimeout)
		if runAt.Before(want.Add(-time.Second)) || want.Add(5*time.Second).Before(runAt) {
			t.Fatalf("sweep run_at = %s, want about %s", runAt, want)
		}
	})
}

// The same sweep removes a derived location a day after the derivation that
// wrote it (§5.7): a node that stops pinging falls back to its genesis on its
// own, even when no derivation runs to remove it. The table holds one row per
// node, so it is swept row by row.
func TestRemoveExpiredPingsSweepsDerivedLocations(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:0", nil)
		defer clientSession.Cancel()

		now := server.NowUtc()
		derivedLocation := func(age time.Duration) *model.DerivedLocation {
			return &model.DerivedLocation{
				NodeKind:          model.DerivedLocationNodeKindProvider,
				NodeId:            server.NewId(),
				GenesisAccuracyKm: 25,
				LocationId:        server.NewId(),
				CityLocationId:    server.NewId(),
				RegionLocationId:  server.NewId(),
				CountryLocationId: server.NewId(),
				UpdateTime:        now.Add(-age),
			}
		}
		retention := DefaultRemoveExpiredPingsSettings().ReportSettings.Retention
		expired := derivedLocation(retention + time.Minute)
		fresh := derivedLocation(retention - time.Hour)
		written, _ := model.ReplaceDerivedLocations(ctx, []*model.DerivedLocation{expired, fresh})
		connect.AssertEqual(t, written, 2)

		result, err := RemoveExpiredPings(&RemoveExpiredPingsArgs{}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.RemovedDerivedLocations, 1)
		if model.GetDerivedLocation(ctx, expired.NodeKind, expired.NodeId) != nil {
			t.Fatal("the expired derived location survived the sweep")
		}
		if model.GetDerivedLocation(ctx, fresh.NodeKind, fresh.NodeId) == nil {
			t.Fatal("the sweep removed a fresh derived location")
		}

		result, err = RemoveExpiredPings(&RemoveExpiredPingsArgs{}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.RemovedDerivedLocations, 0)
	})
}
