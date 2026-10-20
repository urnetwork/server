package work

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
)

// The ping table at the scale target (connect/GEOMAP.md §5.8 item 4, D27): the
// rows and bytes a day brings, the partitions the sweep's own rule keeps live,
// and the row size that the growth model is priced at, measured.

// The target in pings a day: a million extenders pinging 64 peers eight times
// a peer a day -- two probes on each of two address families at each of two
// refreshes -- and a million providers of whom half make a day's probe pass,
// 32 pings to 16 candidates at two probes each.
const (
	growthModelExtenderCount        = 1_000_000
	growthModelPeerCount            = 64
	growthModelPingsPerPeerPerDay   = 8
	growthModelProviderCount        = 1_000_000
	growthModelPingsPerProviderPass = 32
	growthModelProviderPassShare    = 0.5
	// heap and index bytes a stored ping costs, which
	// TestNetworkPingRowSizeWithinTheGrowthModel measures on co-signed rows
	// and holds to: about 531 at 200k rows, 283 heap and 248 index, rounded up
	growthModelRowBytes = 560
)

// Pings the target stores a day.
func growthModelDailyRows() int64 {
	extenderRows := int64(growthModelExtenderCount * growthModelPeerCount * growthModelPingsPerPeerPerDay)
	providerRows := int64(float64(growthModelProviderCount*growthModelPingsPerProviderPass) * growthModelProviderPassShare)
	return extenderRows + providerRows
}

// The table grown at the target's rate under the sweep's own rule and
// settings, a minute at a time for a week from the partitions the conversion
// makes: every insert finds its day; at most three partitions ever hold rows
// -- yesterday, today, and the day past its keep timeout until the next
// sweep drops it -- so the live size is a day, the keep timeout and one sweep
// interval of pings; and the days ahead outlast a sweep outage of that many
// days. Pure.
func TestNetworkPingTableGrowthModel(t *testing.T) {
	settings := DefaultRemoveExpiredPingsSettings()
	keepTimeout := settings.ReportSettings.KeepTimeout()
	sweepTimeout := settings.ReportSettings.SweepTimeout
	dailyRows := growthModelDailyRows()
	minuteRows := float64(dailyRows) / (24 * 60)

	// a synthetic week, the sweep every sweep interval from minute 37
	start := time.Date(2031, 3, 10, 0, 0, 0, 0, time.UTC)
	sweepOffset := 37 * time.Minute
	partitions := []*model.NetworkPingPartition{}
	for day := start.AddDate(0, 0, -1); !start.AddDate(0, 0, settings.PartitionSettings.AheadDays).Before(day); day = day.AddDate(0, 0, 1) {
		partitions = append(partitions, &model.NetworkPingPartition{
			Name:  model.NetworkPingPartitionName(day),
			Lower: day,
			Upper: day.AddDate(0, 0, 1),
		})
	}
	partitionNameRows := map[string]float64{}
	maxLiveRows := 0.0
	maxLivePartitionCount := 0
	maxPartitionCount := 0
	minAheadTimeout := time.Duration(-1)
	for minute := 0; minute < 7*24*60; minute += 1 {
		now := start.Add(time.Duration(minute) * time.Minute)
		partitionName := model.NetworkPingPartitionName(now)
		found := false
		for _, partition := range partitions {
			found = found || partition.Name == partitionName
		}
		if !found {
			t.Fatalf("an insert at %s found no partition", now)
		}
		partitionNameRows[partitionName] += minuteRows

		// the most the table holds is just before a sweep drops a day
		liveRows := 0.0
		livePartitionCount := 0
		for _, partition := range partitions {
			if 0 < partitionNameRows[partition.Name] {
				liveRows += partitionNameRows[partition.Name]
				livePartitionCount += 1
			}
		}
		maxLiveRows = max(maxLiveRows, liveRows)
		maxLivePartitionCount = max(maxLivePartitionCount, livePartitionCount)
		maxPartitionCount = max(maxPartitionCount, len(partitions))

		if (time.Duration(minute)*time.Minute-sweepOffset)%sweepTimeout != 0 {
			continue
		}
		createDays, dropPartitions := model.PlanNetworkPingPartitions("network_ping", now, keepTimeout, partitions, settings.PartitionSettings)
		for _, day := range createDays {
			partitions = append(partitions, &model.NetworkPingPartition{
				Name:  model.NetworkPingPartitionName(day),
				Lower: day,
				Upper: day.AddDate(0, 0, 1),
			})
		}
		for _, dropPartition := range dropPartitions {
			keptPartitions := []*model.NetworkPingPartition{}
			for _, partition := range partitions {
				if partition.Name != dropPartition.Name {
					keptPartitions = append(keptPartitions, partition)
				}
			}
			partitions = keptPartitions
			delete(partitionNameRows, dropPartition.Name)
		}
		// if this were the last sweep, inserts would still find their day until
		// the last partition ends
		lastUpper := time.Time{}
		for _, partition := range partitions {
			if lastUpper.Before(partition.Upper) {
				lastUpper = partition.Upper
			}
		}
		if aheadTimeout := lastUpper.Sub(now); minAheadTimeout < 0 || aheadTimeout < minAheadTimeout {
			minAheadTimeout = aheadTimeout
		}
	}

	dailyBytes := float64(dailyRows) * growthModelRowBytes
	liveBytes := maxLiveRows * growthModelRowBytes
	gib := float64(1024 * 1024 * 1024)
	t.Logf(
		"target: %d pings a day (%d extender, %d provider), %.0f a second; %d bytes a row is %.1f GiB a day",
		dailyRows,
		int64(growthModelExtenderCount*growthModelPeerCount*growthModelPingsPerPeerPerDay),
		dailyRows-int64(growthModelExtenderCount*growthModelPeerCount*growthModelPingsPerPeerPerDay),
		float64(dailyRows)/(24*time.Hour).Seconds(),
		growthModelRowBytes,
		dailyBytes/gib,
	)
	t.Logf(
		"live: at most %d partitions hold rows (%d with the days ahead), %.0f rows, %.1f GiB, %.3f days of pings; an outage of %s leaves every insert a partition",
		maxLivePartitionCount,
		maxPartitionCount,
		maxLiveRows,
		liveBytes/gib,
		maxLiveRows/float64(dailyRows),
		minAheadTimeout,
	)
	if 3 < maxLivePartitionCount {
		t.Fatalf("%d partitions held rows at once, more than three", maxLivePartitionCount)
	}
	if wantMaxLiveRows := float64(dailyRows) * float64(24*time.Hour+keepTimeout+sweepTimeout) / float64(24*time.Hour); wantMaxLiveRows+1 < maxLiveRows {
		t.Fatalf("the table held %.0f rows, more than a day, the keep timeout and a sweep interval of pings (%.0f)", maxLiveRows, wantMaxLiveRows)
	}
	if minAheadTimeout < time.Duration(settings.PartitionSettings.AheadDays)*24*time.Hour {
		t.Fatalf("the days ahead cover an outage of %s, under %d days", minAheadTimeout, settings.PartitionSettings.AheadDays)
	}
}

// A co-signed ping row as the ingest stores it -- random ids, a 32 byte nonce,
// 64 byte signature and co-signature -- costs no more heap and index than the
// growth model is priced at. The rows go into one day partition in one
// statement, in random key order as the fleet's reports arrive, which is what
// the b-tree fill of the indexes depends on. With GEOMAP_SCALE=1 it loads a
// hundredth of the target's day instead and times the drop of the whole
// partition, which is the sweep's one statement on it.
func TestNetworkPingRowSizeWithinTheGrowthModel(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		rowCount := int64(200_000)
		if os.Getenv("GEOMAP_SCALE") == "1" {
			rowCount = growthModelDailyRows() / 100
		}
		day := server.NowUtc().AddDate(0, 1, 0).Truncate(24 * time.Hour)
		model.MaintainNetworkPingPartitions(ctx, day, controller.DefaultExtenderPingReportSettings().KeepTimeout(), model.DefaultNetworkPingPartitionSettings())
		partitionName := model.NetworkPingPartitionName(day)

		loadStart := time.Now()
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO network_ping (
					ping_id,
					pinger_kind,
					pinger_id,
					target_extender_id,
					probe_nonce,
					rtt_ms,
					probe_time,
					cosign,
					cosign_reason,
					pinger_signature,
					cosignature,
					hop_count,
					create_time
				)
				SELECT
					gen_random_uuid(),
					2,
					gen_random_uuid(),
					gen_random_uuid(),
					uuid_send(gen_random_uuid()) || uuid_send(gen_random_uuid()),
					(random() * 300)::int,
					$1::timestamp + (row_index % 86400) * interval '1 second',
					1,
					0,
					uuid_send(gen_random_uuid()) || uuid_send(gen_random_uuid()) || uuid_send(gen_random_uuid()) || uuid_send(gen_random_uuid()),
					uuid_send(gen_random_uuid()) || uuid_send(gen_random_uuid()) || uuid_send(gen_random_uuid()) || uuid_send(gen_random_uuid()),
					0,
					$1::timestamp + (row_index % 86400) * interval '1 second'
				FROM generate_series(1, $2::bigint) AS row_index
				`,
				day,
				rowCount,
			))
		})
		loadTimeout := time.Since(loadStart)

		var heapBytes int64
		var indexBytes int64
		var totalBytes int64
		var storedCount int64
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
				SELECT
					pg_relation_size(to_regclass($1)),
					pg_indexes_size(to_regclass($1)),
					pg_total_relation_size(to_regclass($1)),
					(SELECT count(*) FROM network_ping WHERE $2 <= create_time AND create_time < $3)
				`,
				"public."+partitionName,
				day,
				day.AddDate(0, 0, 1),
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&heapBytes, &indexBytes, &totalBytes, &storedCount))
				}
			})
		})
		if storedCount != rowCount {
			t.Fatalf("%d rows landed in %s, want %d", storedCount, partitionName, rowCount)
		}
		rowBytes := float64(totalBytes) / float64(rowCount)
		t.Logf(
			"%d rows loaded in %s: %.1f bytes a row, %.1f heap and %.1f index",
			rowCount,
			loadTimeout.Round(time.Millisecond),
			rowBytes,
			float64(heapBytes)/float64(rowCount),
			float64(indexBytes)/float64(rowCount),
		)
		if growthModelRowBytes < rowBytes {
			t.Fatalf("a row costs %.1f bytes, over the growth model's %d", rowBytes, growthModelRowBytes)
		}

		if os.Getenv("GEOMAP_SCALE") == "1" {
			dropStart := time.Now()
			createdPartitionNames, droppedPartitionNames, _ := model.MaintainNetworkPingPartitions(
				ctx,
				day.AddDate(0, 0, 1).Add(controller.DefaultExtenderPingReportSettings().KeepTimeout()+time.Millisecond),
				controller.DefaultExtenderPingReportSettings().KeepTimeout(),
				model.DefaultNetworkPingPartitionSettings(),
			)
			t.Logf(
				"the sweep dropped %v (%.1f GiB) in %s, creating %v",
				droppedPartitionNames,
				float64(totalBytes)/float64(1024*1024*1024),
				time.Since(dropStart).Round(time.Millisecond),
				createdPartitionNames,
			)
		}
	})
}
