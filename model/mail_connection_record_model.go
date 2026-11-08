package model

import (
	"context"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// mx-2's router session records (§15.4, §15.15; MAIL-M7-21): one row for each
// session whose SNI named a network, holding the receiving network, the
// session's start, its bytes and a result word, and no sender address or
// burner label. A session that named no network leaves no row
// (plan-MAIL-small-readings #3). mxedge writes them in 10 s batches off the
// connection path (model.NewMxEdgeStore), so nothing here is on a session's
// way (I-12).
//
// Records sit in hourly UNLOGGED partitions of start_time, which the
// registry's runway creates ahead. work.MailRetention retires each hour once
// its upper bound is RecordRetention past (RetireMailConnectionHours), so no
// record lives past mx-2's 24 h but by that chain's cadence. A §21.9 hold whose
// copy has not landed keeps the hours it covers, and holds back no other
// (connection_record_hold.go). Unlogged leaves keep the records out of every
// physical copy of the primary (xops-dump-physical-copies), and no dump holds
// the table (D-OPS-18).
//
// Nothing here reads or writes the table unless the binary links MG7: the
// family is registered in every binary and reports itself not partitioned
// without the group.

const (
	mailConnectionRecordTable = "mail_connection_record"
	// the longest result word a row holds, a metric label's
	mailConnectionResultMaxByteCount = 32
)

// Tunables of the session records.
type MailConnectionSettings struct {
	// An hour of records retires once its upper bound is this far past. 23 h,
	// so the hour's oldest record has lived mx-2's 24 h when it goes. Positive.
	RecordRetention time.Duration
	// Hours after the current one that always have a partition: six, so an
	// outage of the 10-minute runway task that long leaves every insert one.
	AheadHours int
	// How far behind now the runway creates. A session's record carries its
	// start and is written when it ends, at most §15.4's 30 min splice and a
	// 10 s batch later, so an hour behind covers every late record.
	CreateBehind time.Duration
	// A record starting further ahead of the writer's clock than this is
	// refused as a clock fault, so no partition is made far ahead of time.
	MaxFutureSkew time.Duration
}

func DefaultMailConnectionSettings() *MailConnectionSettings {
	return &MailConnectionSettings{
		RecordRetention: 23 * time.Hour,
		AheadHours:      6,
		CreateBehind:    1 * time.Hour,
		MaxFutureSkew:   5 * time.Minute,
	}
}

// One row of mail_connection_record.
type MailConnectionRecord struct {
	NetworkId server.Id
	// When the session began, which places the record in its hour.
	StartTime time.Time
	// Bytes the session passed, both ways.
	ByteCount int64
	// A word from mxedge's closed set of session results, a metric label's
	// shape: lowercase letters, digits and underscores.
	Result string
}

// What one write did with its batch.
type MailConnectionWriteResult struct {
	WrittenCount int
	// Not a record a row could hold: no network, negative bytes, or a result
	// that is not a closed-set word. A writer's bug; dropped and counted.
	RefusedCount int
	// Starting in an hour already retired, or too far ahead of the clock.
	// Dropped and counted, so no expired hour is made again.
	LateCount int
}

// The registry's declaration of mail_connection_record: hourly by start_time,
// unlogged, retired at RecordRetention by work.MailRetention once any hold has
// copied its network's rows. Hooks capture the declaration itself, so
// registration makes no initialization cycle.
func newMailConnectionRecordPartitionFamily(settings *MailConnectionSettings) *server.PartitionFamily {
	family := &server.PartitionFamily{
		Name:        mailConnectionRecordTable,
		Description: "mail_connection_record, mxedge's session records (§2.8 mx-2), hourly by start_time, retired at 24 h by work.MailRetention once any §21.9 hold has copied its network's rows",
		Tables:      []string{mailConnectionRecordTable},
		KeyColumn:   "start_time",
		KeyKind:     server.PartitionKeyTimestamp,
		Granularity: server.PartitionGranularityHourly,
		AheadRanges: settings.AheadHours,
		Retention:   settings.RecordRetention,
		// leaves go whole at 24 h, so a rebuild would buy nothing
		ReindexExempt:       true,
		MonitorRunwayRanges: 2,
		UnloggedLeaves:      true,
		RetireByOwnTask:     true,
		CreateFrom: func(ctx context.Context, now time.Time) time.Time {
			return now.Add(-settings.CreateBehind)
		},
	}
	family.Partitioned = func(ctx context.Context) bool {
		return server.SidecarMg7Linked() && mailConnectionTablePartitioned(ctx)
	}
	family.DropPrecondition = func(ctx context.Context, conn server.PgQuerier, r server.PartitionRange) string {
		return connectionRecordRangeHold(ctx, conn, family.LeafName(mailConnectionRecordTable, r), "start_time", r, server.NowUtc())
	}
	return family
}

var mailConnectionRecordPartitionFamily = server.RegisterPartitionFamily(
	newMailConnectionRecordPartitionFamily(DefaultMailConnectionSettings()),
)

// Whether mail_connection_record is a partitioned table in this database.
func mailConnectionTablePartitioned(ctx context.Context) (partitioned bool) {
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT COALESCE((SELECT relkind = 'p' FROM pg_class WHERE oid = to_regclass($1)), false)`,
			"public."+mailConnectionRecordTable,
		).Scan(&partitioned))
	})
	return
}

// Whether result is a closed-set word.
func mailConnectionResultValid(result string) bool {
	if result == "" || mailConnectionResultMaxByteCount < len(result) {
		return false
	}
	for i := range len(result) {
		c := result[i]
		if !('a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

// Stores a batch of records, as one insert. An insert that finds no partition
// for an hour makes the hour and stores once more. Records no row can hold and
// records of an expired hour are dropped and counted, never stored. A record
// carries no identity of its own, so a batch the caller retries after an
// uncertain failure may store its rows twice: a count read off this table is
// an upper bound.
func WriteMailConnectionRecords(ctx context.Context, records []*MailConnectionRecord) (*MailConnectionWriteResult, error) {
	return writeMailConnectionRecordsAt(ctx, records, server.NowUtc(), DefaultMailConnectionSettings())
}

// The write as the clock reads now, which judges only what is late; a test
// moves it.
func writeMailConnectionRecordsAt(ctx context.Context, records []*MailConnectionRecord, now time.Time, settings *MailConnectionSettings) (*MailConnectionWriteResult, error) {
	result := &MailConnectionWriteResult{}
	if !server.SidecarMg7Linked() {
		return result, ErrMailUnlinked
	}
	family := mailConnectionRecordPartitionFamily
	networkIds := []server.Id{}
	startTimes := []time.Time{}
	byteCounts := []int64{}
	results := []string{}
	hours := map[time.Time]bool{}
	for _, record := range records {
		if record == nil || record.NetworkId == (server.Id{}) || record.ByteCount < 0 || !mailConnectionResultValid(record.Result) {
			result.RefusedCount += 1
			continue
		}
		startTime := record.StartTime.UTC()
		if family.IsExpired(ctx, now, family.RangeAt(startTime)) || now.Add(settings.MaxFutureSkew).Before(startTime) {
			result.LateCount += 1
			continue
		}
		networkIds = append(networkIds, record.NetworkId)
		startTimes = append(startTimes, startTime)
		byteCounts = append(byteCounts, record.ByteCount)
		results = append(results, record.Result)
		hours[family.RangeAt(startTime).Lower] = true
	}
	if 0 < result.RefusedCount || 0 < result.LateCount {
		glog.Warningf("[mailconnection]dropped %d malformed and %d late records\n", result.RefusedCount, result.LateCount)
	}
	if len(networkIds) == 0 {
		return result, nil
	}
	err := insertConnectionRecordsCreatingHours(ctx, family, mailConnectionRecordTable, hours, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO mail_connection_record (network_id, start_time, byte_count, result)
			SELECT * FROM unnest($1::uuid[], $2::timestamp[], $3::bigint[], $4::varchar[])
			`,
			networkIds,
			startTimes,
			byteCounts,
			results,
		))
	})
	if err != nil {
		return result, err
	}
	result.WrittenCount = len(networkIds)
	return result, nil
}

// Retires every hour of mail_connection_record past its retention at now,
// oldest first, until deadline. An hour a §21.9 hold keeps stays in the
// report's Blocked with why, and the hours after it still go. A binary that
// does not link MG7 retires nothing.
func RetireMailConnectionHours(ctx context.Context, now time.Time, deadline time.Time) *server.PartitionRetireReport {
	report, _ := mailConnectionRecordPartitionFamily.Retire(ctx, now, server.PartitionFamilyState{}, deadline)
	return report
}

// One network's records starting in [since, until), oldest first: what an
// abuse join and a test read. Nil when the binary does not link MG7.
func GetMailConnectionRecords(ctx context.Context, networkId server.Id, since time.Time, until time.Time) (records []*MailConnectionRecord) {
	if !server.SidecarMg7Linked() {
		return nil
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT network_id, start_time, byte_count, result
			FROM mail_connection_record
			WHERE network_id = $1 AND $2 <= start_time AND start_time < $3
			ORDER BY start_time, byte_count, result
			`,
			networkId,
			since.UTC(),
			until.UTC(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				record := &MailConnectionRecord{}
				server.Raise(result.Scan(&record.NetworkId, &record.StartTime, &record.ByteCount, &record.Result))
				records = append(records, record)
			}
		})
	})
	return
}

// The write with the clock at now and the default settings, for another
// package's tests, which write an hour long past without its being late.
func Testing_WriteMailConnectionRecordsAt(ctx context.Context, records []*MailConnectionRecord, now time.Time) (*MailConnectionWriteResult, error) {
	return writeMailConnectionRecordsAt(ctx, records, now, DefaultMailConnectionSettings())
}
