package model

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// §2.8's tls-3 (TLS-M5-12): tlsedge's connection records and their daily
// counts. A record is one visitor connection to a published host: network,
// host, connection id, start, end, bytes and the keyed hash of the visitor's
// /24 or /48, never an address (I-13). tlsedge writes them in 10 s batches off
// the connection path (§13.9), so nothing here is on a packet's way (I-12).
//
// Records sit in hourly UNLOGGED partitions of end_time, which the partition
// registry's runway creates ahead. The family's own chain (work.TlsConnection-
// Rollup, through RollUpTlsConnectionHours) retires each hour once its upper
// bound is RecordRetention past, so no record lives past tls-3's 24 h but by
// that chain's cadence. Retirement waits while a §21.9 hold has not copied its
// network's rows (connection_record_hold.go), and that wait holds back no
// other hour. The transaction that retires an hour first adds its counts to
// tls_connection_day, so the counts land exactly once, whether the drop commits
// on its first try or its fifth. Daily counts are kept 13 months, skipping a
// held network's rows (RemoveExpiredTlsConnectionDays).
//
// Unlogged leaves keep the records out of every physical copy of the primary,
// so their 24 h holds in backups too, and a crash of the primary empties them,
// as a restore would (xops-dump-physical-copies). No dump holds the table
// (D-OPS-18; xops's sidecar-tables.conf).
//
// Nothing here reads or writes these tables unless the binary links MG5
// (server.SidecarMg5Linked): the family is registered in every binary and
// reports itself not partitioned without the group.

const (
	tlsConnectionRecordTable = "tls_connection_record"
	tlsConnectionDayTable    = "tls_connection_day"
	// tls-3's keyed hash: HMAC-SHA256 cut to 16 bytes (plan-TLS-visitor-hash-key)
	tlsConnectionVisitorHashByteCount = 16
	// RFC 1035's longest name, the longest host a record can name
	tlsConnectionHostMaxByteCount = 253
)

// Tunables of the records and their counts.
type TlsConnectionSettings struct {
	// An hour of records retires once its upper bound is this far past. 23 h,
	// so the hour's oldest record has lived tls-3's 24 h when it goes; the
	// retention task's 15-minute cadence adds at most that. Positive.
	RecordRetention time.Duration
	// Hours after the current one that always have a partition: six, so an
	// outage of the 10-minute runway task that long leaves every insert one.
	AheadHours int
	// How far behind now the runway creates, for a batch written late. One
	// hour covers a batch held across the hour's end.
	CreateBehind time.Duration
	// A record ending further ahead of the writer's clock than this is refused
	// as a clock fault, so no partition is made far ahead of time. Five minutes.
	MaxFutureSkew time.Duration
	// The longest a retiring hour's statements may run: its roll-up reads the
	// whole hour. Five minutes, past the registry's default minute.
	DropStatementTimeout time.Duration
	// Daily counts are kept this many months (tls-3: 13).
	DayRetentionMonths int
	// Rows one retention transaction deletes, and how long one retention run
	// lasts before it leaves the rest to its next run.
	DayRetentionBatchSize int
	DayRetentionRunBudget time.Duration
}

// tls-3's lifetimes and §13.9's batches.
func DefaultTlsConnectionSettings() *TlsConnectionSettings {
	return &TlsConnectionSettings{
		RecordRetention:       23 * time.Hour,
		AheadHours:            6,
		CreateBehind:          1 * time.Hour,
		MaxFutureSkew:         5 * time.Minute,
		DropStatementTimeout:  5 * time.Minute,
		DayRetentionMonths:    13,
		DayRetentionBatchSize: 10000,
		DayRetentionRunBudget: 30 * time.Second,
	}
}

// One row of tls_connection_record.
type TlsConnectionRecord struct {
	// 16 random bytes from tlsedge (§13.5), which joins abuse reports.
	ConnectionId server.Id
	NetworkId    server.Id
	// The published host within the network: @ or a label, or a scope host.
	Host      string
	StartTime time.Time
	// When the connection ended, which places the record in its hour.
	EndTime time.Time
	// Bytes passed, both ways.
	ByteCount int64
	// The visitor's /24 or /48 under the day's key, 16 bytes.
	VisitorHash []byte
}

// What one write did with its batch.
type TlsConnectionWriteResult struct {
	// Stored now or by an earlier try of the same batch.
	WrittenCount int
	// Not a record any row could hold: no network, connection or host, a
	// host too long, a hash of another size, an end before its start or
	// negative bytes. A writer's bug; dropped and counted.
	RefusedCount int
	// Ending in an hour already retired or past its retention, or too far
	// ahead of the clock. Dropped and counted, so no expired hour is made
	// again and no hour made early.
	LateCount int
}

// The binary does not link MG5, so the tables are not there to write.
var ErrTlsConnectionRecordsUnlinked = errors.New("tls connection records need MG5")

// The registry's declaration of tls_connection_record: hourly by end_time,
// retired at RecordRetention once any hold has copied its network's rows, and
// rolled up into tls_connection_day in the retiring transaction. Hooks capture
// the declaration itself, not the package var, so registration makes no
// initialization cycle.
func newTlsConnectionRecordPartitionFamily(settings *TlsConnectionSettings) *server.PartitionFamily {
	partitionSettings := server.DefaultPartitionFamilySettings()
	partitionSettings.DropStatementTimeout = settings.DropStatementTimeout
	family := &server.PartitionFamily{
		Name:        tlsConnectionRecordTable,
		Description: "tls_connection_record, tlsedge's connection records (§2.8 tls-3), hourly by end_time, retired at 24 h once any §21.9 hold has copied its network's rows, each hour rolled up into tls_connection_day as it goes",
		Tables:      []string{tlsConnectionRecordTable},
		KeyColumn:   "end_time",
		KeyKind:     server.PartitionKeyTimestamp,
		Granularity: server.PartitionGranularityHourly,
		AheadRanges: settings.AheadHours,
		Retention:   settings.RecordRetention,
		// leaves go whole at 24 h, so a rebuild would buy nothing
		ReindexExempt:       true,
		MonitorRunwayRanges: 2,
		UnloggedLeaves:      true,
		// work.TlsConnectionRollup retires the hours, warning on a held one
		RetireByOwnTask: true,
		Settings:        partitionSettings,
		CreateFrom: func(ctx context.Context, now time.Time) time.Time {
			return now.Add(-settings.CreateBehind)
		},
	}
	family.Partitioned = func(ctx context.Context) bool {
		return server.SidecarMg5Linked() && tlsConnectionTablePartitioned(ctx)
	}
	family.DropPrecondition = func(ctx context.Context, conn server.PgQuerier, r server.PartitionRange) string {
		return connectionRecordRangeHold(ctx, conn, family.LeafName(tlsConnectionRecordTable, r), "end_time", r, server.NowUtc())
	}
	family.DropWork = func(ctx context.Context, tx server.PgTx, r server.PartitionRange) {
		rollUpTlsConnectionHourInTx(ctx, tx, family.LeafName(tlsConnectionRecordTable, r))
	}
	return family
}

var tlsConnectionRecordPartitionFamily = server.RegisterPartitionFamily(
	newTlsConnectionRecordPartitionFamily(DefaultTlsConnectionSettings()),
)

// Whether tls_connection_record is a partitioned table in this database.
func tlsConnectionTablePartitioned(ctx context.Context) (partitioned bool) {
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT COALESCE((SELECT relkind = 'p' FROM pg_class WHERE oid = to_regclass($1)), false)`,
			"public."+tlsConnectionRecordTable,
		).Scan(&partitioned))
	})
	return
}

// Adds an hour's counts to its day's rows, in the transaction that retires
// the hour, so a retried retirement adds them once. leaf is the hour's leaf,
// still attached.
func rollUpTlsConnectionHourInTx(ctx context.Context, tx server.PgTx, leaf string) {
	server.RaisePgResult(tx.Exec(
		ctx,
		fmt.Sprintf(
			`
			INSERT INTO tls_connection_day (network_id, host, day, connection_count, byte_count)
			SELECT network_id, host, end_time::date, count(*), sum(byte_count)::bigint
			FROM public.%s
			GROUP BY network_id, host, end_time::date
			ON CONFLICT (network_id, host, day) DO UPDATE SET
				connection_count = tls_connection_day.connection_count + excluded.connection_count,
				byte_count = tls_connection_day.byte_count + excluded.byte_count
			`,
			pgx.Identifier{leaf}.Sanitize(),
		),
	))
}

// Retires every hour of tls_connection_record past its retention at now,
// oldest first, until deadline: each retirement adds the hour's counts to
// tls_connection_day in the transaction that drops it, so a held or failed
// hour leaves no count behind. An hour a §21.9 hold keeps stays in the report's
// Blocked with why, and the hours after it still go. The family has no archive
// step, so it carries no state from run to run. A binary that does not link
// MG5 retires nothing.
func RollUpTlsConnectionHours(ctx context.Context, now time.Time, deadline time.Time) *server.PartitionRetireReport {
	report, _ := tlsConnectionRecordPartitionFamily.Retire(ctx, now, server.PartitionFamilyState{}, deadline)
	return report
}

// Whether a record is one a row can hold.
func tlsConnectionRecordValid(record *TlsConnectionRecord) bool {
	return record != nil &&
		record.ConnectionId != (server.Id{}) &&
		record.NetworkId != (server.Id{}) &&
		record.Host != "" && len(record.Host) <= tlsConnectionHostMaxByteCount &&
		len(record.VisitorHash) == tlsConnectionVisitorHashByteCount &&
		!record.EndTime.Before(record.StartTime) &&
		0 <= record.ByteCount
}

// Stores a batch of records, each once however often the batch is retried. An
// insert that finds no partition for an hour makes the hour (the registry's
// EnsureRangeFor) and stores once more. Records no row can hold and records of
// an expired hour are dropped and counted, never stored.
func WriteTlsConnectionRecords(ctx context.Context, records []*TlsConnectionRecord) (*TlsConnectionWriteResult, error) {
	return writeTlsConnectionRecordsAt(ctx, records, server.NowUtc(), DefaultTlsConnectionSettings())
}

// The write with the clock at now and the default settings, for another
// package's tests, which write an hour long past without its being late.
func Testing_WriteTlsConnectionRecordsAt(ctx context.Context, records []*TlsConnectionRecord, now time.Time) (*TlsConnectionWriteResult, error) {
	return writeTlsConnectionRecordsAt(ctx, records, now, DefaultTlsConnectionSettings())
}

// The write as the clock reads now, which judges only what is late; a test
// moves it.
func writeTlsConnectionRecordsAt(ctx context.Context, records []*TlsConnectionRecord, now time.Time, settings *TlsConnectionSettings) (*TlsConnectionWriteResult, error) {
	result := &TlsConnectionWriteResult{}
	if !server.SidecarMg5Linked() {
		return result, ErrTlsConnectionRecordsUnlinked
	}
	family := tlsConnectionRecordPartitionFamily
	connectionIds := []server.Id{}
	networkIds := []server.Id{}
	hosts := []string{}
	startTimes := []time.Time{}
	endTimes := []time.Time{}
	byteCounts := []int64{}
	visitorHashes := [][]byte{}
	hours := map[time.Time]bool{}
	for _, record := range records {
		if !tlsConnectionRecordValid(record) {
			result.RefusedCount += 1
			continue
		}
		endTime := record.EndTime.UTC()
		if family.IsExpired(ctx, now, family.RangeAt(endTime)) || now.Add(settings.MaxFutureSkew).Before(endTime) {
			result.LateCount += 1
			continue
		}
		connectionIds = append(connectionIds, record.ConnectionId)
		networkIds = append(networkIds, record.NetworkId)
		hosts = append(hosts, record.Host)
		startTimes = append(startTimes, record.StartTime.UTC())
		endTimes = append(endTimes, endTime)
		byteCounts = append(byteCounts, record.ByteCount)
		visitorHashes = append(visitorHashes, record.VisitorHash)
		hours[family.RangeAt(endTime).Lower] = true
	}
	if 0 < result.RefusedCount || 0 < result.LateCount {
		glog.Warningf("[tlsconnection]dropped %d malformed and %d late records\n", result.RefusedCount, result.LateCount)
	}
	if len(connectionIds) == 0 {
		return result, nil
	}
	err := insertConnectionRecordsCreatingHours(ctx, family, tlsConnectionRecordTable, hours, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO tls_connection_record (connection_id, network_id, host, start_time, end_time, byte_count, visitor_hash)
			SELECT * FROM unnest($1::uuid[], $2::uuid[], $3::varchar[], $4::timestamp[], $5::timestamp[], $6::bigint[], $7::bytea[])
			ON CONFLICT (connection_id, end_time) DO NOTHING
			`,
			connectionIds,
			networkIds,
			hosts,
			startTimes,
			endTimes,
			byteCounts,
			visitorHashes,
		))
	})
	if err != nil {
		return result, err
	}
	result.WrittenCount = len(connectionIds)
	return result, nil
}

// The first day tls_connection_day keeps at now: DayRetentionMonths back from
// now's UTC day.
func tlsConnectionDayCutoff(now time.Time, settings *TlsConnectionSettings) time.Time {
	now = now.UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, -settings.DayRetentionMonths, 0)
}

const removeExpiredTlsConnectionDaysSql = `
	DELETE FROM tls_connection_day
	WHERE (network_id, host, day) IN (
		SELECT day_row.network_id, day_row.host, day_row.day
		FROM tls_connection_day day_row
		WHERE
			day_row.day < $1 AND
			NOT EXISTS (
				SELECT 1
				FROM abuse_legal_hold hold
				WHERE
					hold.network_id = day_row.network_id AND
					hold.part = 0 AND
					(hold.expire_time IS NULL OR $2 < hold.expire_time)
			)
		ORDER BY day_row.day
		LIMIT $3
		FOR UPDATE SKIP LOCKED
	)
`

// Deletes the daily counts past their 13 months, oldest first, in batches,
// skipping a network that holds a live §21.9 hold. Returns how many went and
// whether the run left some for the next.
func RemoveExpiredTlsConnectionDays(ctx context.Context, now time.Time) (removedCount int64, more bool) {
	if !server.SidecarMg5Linked() {
		return 0, false
	}
	settings := DefaultTlsConnectionSettings()
	cutoff := tlsConnectionDayCutoff(now, settings)
	return removeRetentionBatches(
		ctx,
		RetentionTableTlsConnectionDay,
		settings.DayRetentionBatchSize,
		time.Now().Add(settings.DayRetentionRunBudget),
		func(tx server.PgTx) int64 {
			tag := server.RaisePgResult(tx.Exec(ctx, removeExpiredTlsConnectionDaysSql, cutoff, now.UTC(), settings.DayRetentionBatchSize))
			return tag.RowsAffected()
		},
	)
}

// One network's daily counts from day on, by host and day: what §21's review
// and the network's usage pages read.
type TlsConnectionDay struct {
	NetworkId       server.Id
	Host            string
	Day             time.Time
	ConnectionCount int64
	ByteCount       int64
}

// One network's daily counts on or after since, by day then host.
func GetTlsConnectionDays(ctx context.Context, networkId server.Id, since time.Time) (days []*TlsConnectionDay) {
	if !server.SidecarMg5Linked() {
		return nil
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT network_id, host, day, connection_count, byte_count
			FROM tls_connection_day
			WHERE network_id = $1 AND $2 <= day
			ORDER BY day, host
			`,
			networkId,
			since.UTC(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				day := &TlsConnectionDay{}
				server.Raise(result.Scan(&day.NetworkId, &day.Host, &day.Day, &day.ConnectionCount, &day.ByteCount))
				days = append(days, day)
			}
		})
	})
	return
}
