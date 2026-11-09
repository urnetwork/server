package model

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"strings"
	"time"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
)

// smtpedge's send log (§14.8, §14.14; SMTP-M9-18): smtp_send, MG9's one
// partitioned table, daily by send_day, the UTC day a connection started. An
// edge writes its send records in batches after their connections end, off
// the connection path (§14.9), and attribution, halts and §14.8's RUA
// reconciliation read them by SmtpJoinKey.
//
// A row holds smtp-3's fields alone, in smtpedgeservice.SmtpSendRecord, which
// TestSmtpRecordsHoldNoThirdPartyAddress holds to that row: the MX's name and
// never its IP, and no recipient, third-party address, subject or record
// length (D-SMTP-10).
//
// The registry's runway makes each day's leaf AheadDays ahead, and an insert
// that finds no leaf for a day makes it and stores once more, so a missing day
// never fails a batch. A record of a day already past its retention, or
// starting further ahead of the writer's clock than MaxFutureSkew, is dropped
// and counted, so no expired day is made again and no clock fault makes a leaf
// far ahead. SMTP-M9-17's retention takes each day at 30 days through the
// family's Retire, once every §21.9 hold has copied its network's rows
// (DropPrecondition): the registry never drops one on its own
// (RetireByOwnTask).
//
// Nothing here reads or writes the table unless the binary links MG9: the
// family is registered in every binary and reports itself not partitioned
// without the group, and every call answers ErrSmtpEdgeUnlinked.

// What a call answers in a binary that does not link MG9, so code reading
// smtp_send, smtp_halt and smtp_dkim_key stays off until M9's Gn-out (§19.7).
var ErrSmtpEdgeUnlinked = errors.New("smtp_send, smtp_halt and smtp_dkim_key need MG9")

const (
	smtpSendTable = "smtp_send"
	// The octets X-UR-Ref carries (plan-SMTP-small-readings#3): a format
	// value.
	smtpSendIdByteCount = 12
)

// Tunables of the send log.
type SmtpSendSettings struct {
	// A day of sends expires once its upper bound is this far past: smtp-3's
	// 30 days (§14.14). Positive.
	Retention time.Duration
	// Days after the current one that always have a leaf: three, so an outage
	// of the runway task that long leaves every insert one, and an insert that
	// finds none makes its own.
	AheadDays int
	// How far behind now the runway makes leaves. A record carries its
	// connection's start and is written at its end, up to §14.4's 2 h DATA
	// connection and a batch later, so 3 h covers the day before for a send
	// that crossed midnight.
	CreateBehind time.Duration
	// A record starting further ahead of the writer's clock than this is
	// dropped as a clock fault, so no leaf is made far ahead of time.
	// Positive.
	MaxFutureSkew time.Duration
}

func DefaultSmtpSendSettings() *SmtpSendSettings {
	return &SmtpSendSettings{
		Retention:     30 * 24 * time.Hour,
		AheadDays:     3,
		CreateBehind:  3 * time.Hour,
		MaxFutureSkew: 5 * time.Minute,
	}
}

// The registry's declaration of smtp_send: daily by send_day, leaves made
// ahead, retired by SMTP-M9-17's task once any hold has copied its network's
// rows. Hooks capture the declaration itself, so registration makes no
// initialization cycle.
func newSmtpSendPartitionFamily(settings *SmtpSendSettings) *server.PartitionFamily {
	family := &server.PartitionFamily{
		Name:        smtpSendTable,
		Description: "smtp_send, smtpedge's send records (§2.8 smtp-3), daily by send_day, retired at 30 d by SMTP-M9-17's retention once any §21.9 hold has copied its network's rows",
		Tables:      []string{smtpSendTable},
		KeyColumn:   "send_day",
		// a date column, whose bounds the registry writes and reads back as
		// timestamps at 00:00 UTC
		KeyKind:     server.PartitionKeyTimestamp,
		Granularity: server.PartitionGranularityDaily,
		AheadRanges: settings.AheadDays,
		Retention:   settings.Retention,
		// days go whole at 30 days, so a rebuild would buy nothing
		ReindexExempt:   true,
		RetireByOwnTask: true,
		CreateFrom: func(ctx context.Context, now time.Time) time.Time {
			return now.Add(-settings.CreateBehind)
		},
	}
	family.Partitioned = func(ctx context.Context) bool {
		return server.SidecarMg9Linked() && smtpSendTablePartitioned(ctx)
	}
	family.DropPrecondition = func(ctx context.Context, conn server.PgQuerier, r server.PartitionRange) string {
		// a day's rows are keyed by their start, as an hour's connection
		// records are
		return connectionRecordRangeHold(ctx, conn, family.LeafName(smtpSendTable, r), "start_time", r, server.NowUtc())
	}
	return family
}

var smtpSendPartitionFamily = server.RegisterPartitionFamily(
	newSmtpSendPartitionFamily(DefaultSmtpSendSettings()),
)

// Whether smtp_send is a partitioned table in this database.
func smtpSendTablePartitioned(ctx context.Context) (partitioned bool) {
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT COALESCE((SELECT relkind = 'p' FROM pg_class WHERE oid = to_regclass($1)), false)`,
			"public."+smtpSendTable,
		).Scan(&partitioned))
	})
	return
}

// What one write did with its batch.
type SmtpSendWriteResult struct {
	// Rows the batch added: a send already stored under its day and id adds
	// none.
	StoredCount int
	// Not a record a row could hold: a send id that is not 12 octets, no
	// network or client, no address, a name past its column, an end before
	// its start. A writer's bug; dropped and counted.
	RefusedCount int
	// Starting on a day already past its retention, or too far ahead of the
	// clock. Dropped and counted.
	LateCount int
}

// Whether send can be a smtp_send row.
func smtpSendStorable(send *smtpedgeservice.SmtpSendRecord) bool {
	switch {
	case send == nil:
		return false
	case len(send.SendId) != smtpSendIdByteCount:
		return false
	case send.NetworkId == (sidecar.Id{}) || send.ClientId == (sidecar.Id{}):
		return false
	case !send.SendingAddress.IsValid():
		return false
	case send.RecipientDomain == "" || !smtpTextStorable(send.RecipientDomain, smtpDomainMaxByteCount):
		return false
	case send.MxHost == "" || !smtpTextStorable(send.MxHost, smtpDomainMaxByteCount):
		return false
	case send.StartTime.IsZero() || send.EndTime.Before(send.StartTime):
		return false
	case len(send.MessageIdHash) != 0 && len(send.MessageIdHash) != smtpMessageIdHashByteCount:
		return false
	case len(send.RetryOfSendId) != 0 && len(send.RetryOfSendId) != smtpSendIdByteCount:
		return false
	case send.Result == "" || !smtpTextStorable(send.Result, smtpWordMaxByteCount):
		return false
	}
	return true
}

// One send's key in a batch.
type smtpSendKey struct {
	sendDay time.Time
	sendId  string
}

// Stores a batch of send records as one insert: a record whose day and send id
// are stored already is skipped, so a retried batch stores nothing twice, and
// an insert that finds no leaf for a day makes the day and stores once more.
// Records no row can hold, and records of an expired day or too far ahead, are
// dropped and counted, never stored, and are not an error.
func InsertSmtpSends(ctx context.Context, sends []*smtpedgeservice.SmtpSendRecord) (*SmtpSendWriteResult, error) {
	return insertSmtpSendsAt(ctx, sends, server.NowUtc(), DefaultSmtpSendSettings())
}

// The write as the clock reads now, which judges only what is late or early;
// a test moves it.
func insertSmtpSendsAt(ctx context.Context, sends []*smtpedgeservice.SmtpSendRecord, now time.Time, settings *SmtpSendSettings) (*SmtpSendWriteResult, error) {
	result := &SmtpSendWriteResult{}
	if !server.SidecarMg9Linked() {
		return result, ErrSmtpEdgeUnlinked
	}
	family := smtpSendPartitionFamily
	sendDays := []time.Time{}
	sendIds := [][]byte{}
	networkIds := []server.Id{}
	clientIds := []server.Id{}
	sendingAddresses := []netip.Addr{}
	recipientDomains := []string{}
	mxHosts := []string{}
	startTimes := []time.Time{}
	endTimes := []time.Time{}
	byteCounts := []int64{}
	messageIdHashes := [][]byte{}
	xUrRefKeyIds := []int16{}
	recipientCounts := []int64{}
	messageByteCounts := []int64{}
	tlsVersions := []int32{}
	tlsLevels := []int16{}
	results := []string{}
	retryOfSendIds := [][]byte{}
	days := map[time.Time]bool{}
	seen := map[smtpSendKey]bool{}
	for _, send := range sends {
		if !smtpSendStorable(send) {
			result.RefusedCount += 1
			continue
		}
		startTime := send.StartTime.UTC()
		if family.IsExpired(ctx, now, family.RangeAt(startTime)) || now.Add(settings.MaxFutureSkew).Before(startTime) {
			result.LateCount += 1
			continue
		}
		sendDay := smtpUtcDay(startTime)
		key := smtpSendKey{sendDay: sendDay, sendId: string(send.SendId)}
		if seen[key] {
			// a batch that repeats a send stores it once, as a retried one does
			continue
		}
		seen[key] = true
		sendDays = append(sendDays, sendDay)
		sendIds = append(sendIds, send.SendId)
		networkIds = append(networkIds, server.Id(send.NetworkId))
		clientIds = append(clientIds, server.Id(send.ClientId))
		sendingAddresses = append(sendingAddresses, send.SendingAddress.Unmap())
		recipientDomains = append(recipientDomains, send.RecipientDomain)
		mxHosts = append(mxHosts, send.MxHost)
		startTimes = append(startTimes, startTime)
		endTimes = append(endTimes, send.EndTime.UTC())
		byteCounts = append(byteCounts, smtpCountBigint(send.ByteCount))
		messageIdHashes = append(messageIdHashes, smtpNullableBytes(send.MessageIdHash))
		xUrRefKeyIds = append(xUrRefKeyIds, int16(send.XUrRefKeyId))
		recipientCounts = append(recipientCounts, int64(send.RecipientCount))
		messageByteCounts = append(messageByteCounts, smtpCountBigint(send.MessageByteCount))
		tlsVersions = append(tlsVersions, int32(send.TlsVersion))
		tlsLevels = append(tlsLevels, int16(send.TlsLevel))
		results = append(results, send.Result)
		retryOfSendIds = append(retryOfSendIds, smtpNullableBytes(send.RetryOfSendId))
		days[family.RangeAt(startTime).Lower] = true
	}
	if 0 < result.RefusedCount || 0 < result.LateCount {
		glog.Warningf("[smtpsend]dropped %d malformed and %d late or early send records\n", result.RefusedCount, result.LateCount)
	}
	if len(sendIds) == 0 {
		return result, nil
	}
	err := insertConnectionRecordsCreatingHours(ctx, family, smtpSendTable, days, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO smtp_send (
				send_day,
				send_id,
				network_id,
				client_id,
				sending_address,
				recipient_domain,
				mx_host,
				start_time,
				end_time,
				byte_count,
				message_id_hash,
				x_ur_ref_key_id,
				recipient_count,
				message_byte_count,
				tls_version,
				tls_level,
				result,
				retry_of_send_id
			)
			SELECT * FROM unnest(
				$1::date[],
				$2::bytea[],
				$3::uuid[],
				$4::uuid[],
				$5::inet[],
				$6::varchar[],
				$7::varchar[],
				$8::timestamp[],
				$9::timestamp[],
				$10::bigint[],
				$11::bytea[],
				$12::smallint[],
				$13::bigint[],
				$14::bigint[],
				$15::integer[],
				$16::smallint[],
				$17::varchar[],
				$18::bytea[]
			)
			ON CONFLICT (send_day, send_id) DO NOTHING
			`,
			sendDays,
			sendIds,
			networkIds,
			clientIds,
			sendingAddresses,
			recipientDomains,
			mxHosts,
			startTimes,
			endTimes,
			byteCounts,
			messageIdHashes,
			xUrRefKeyIds,
			recipientCounts,
			messageByteCounts,
			tlsVersions,
			tlsLevels,
			results,
			retryOfSendIds,
		))
		result.StoredCount = int(tag.RowsAffected())
	})
	if err != nil {
		return result, err
	}
	return result, nil
}

// Whether key names a send to match beyond a time range, as
// smtpedgeservice.SmtpJoinKey asks.
func smtpJoinKeySelective(key *smtpedgeservice.SmtpJoinKey) bool {
	return key != nil && (len(key.SendId) != 0 || len(key.MessageIdHash) != 0 || key.NetworkId != (sidecar.Id{}) || key.SendingAddress.IsValid())
}

// Every stored send matching key, oldest first: each field set must match,
// and Since and Until bound the start, inclusive. The query names only the
// fields set, so each key uses its index (smtp_send's four), and a bounded
// window names its days too, so only those days' leaves are read.
// smtpedgeservice.ErrSmtpJoinKeyEmpty for a key of a time range alone.
func FindSmtpSends(ctx context.Context, key *smtpedgeservice.SmtpJoinKey) (sends []*smtpedgeservice.SmtpSendRecord, returnErr error) {
	if !server.SidecarMg9Linked() {
		returnErr = ErrSmtpEdgeUnlinked
		return
	}
	query, args := smtpFindSendsQuery(key)
	if query == "" {
		returnErr = smtpedgeservice.ErrSmtpJoinKeyEmpty
		return
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, query, args...)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				send := &smtpedgeservice.SmtpSendRecord{}
				var networkId server.Id
				var clientId server.Id
				var sendingAddress netip.Prefix
				var byteCount int64
				var xUrRefKeyId int16
				var recipientCount int64
				var messageByteCount int64
				var tlsVersion int32
				var tlsLevel int16
				server.Raise(result.Scan(
					&send.SendId,
					&networkId,
					&clientId,
					&sendingAddress,
					&send.RecipientDomain,
					&send.MxHost,
					&send.StartTime,
					&send.EndTime,
					&byteCount,
					&send.MessageIdHash,
					&xUrRefKeyId,
					&recipientCount,
					&messageByteCount,
					&tlsVersion,
					&tlsLevel,
					&send.Result,
					&send.RetryOfSendId,
				))
				send.NetworkId = sidecar.Id(networkId)
				send.ClientId = sidecar.Id(clientId)
				send.SendingAddress = sendingAddress.Addr()
				send.ByteCount = uint64(byteCount)
				send.XUrRefKeyId = uint8(xUrRefKeyId)
				send.RecipientCount = uint32(min(recipientCount, math.MaxUint32))
				send.MessageByteCount = uint64(messageByteCount)
				send.TlsVersion = uint16(tlsVersion)
				send.TlsLevel = uint8(tlsLevel)
				sends = append(sends, send)
			}
		})
	})
	return
}

// The query and arguments FindSmtpSends runs for key, or "" for a key that is
// not selective, kept apart so a test can plan it.
func smtpFindSendsQuery(key *smtpedgeservice.SmtpJoinKey) (string, []any) {
	if !smtpJoinKeySelective(key) {
		return "", nil
	}
	conditions := []string{}
	args := []any{}
	condition := func(format string, value any) {
		args = append(args, value)
		conditions = append(conditions, fmt.Sprintf(format, len(args)))
	}
	if len(key.SendId) != 0 {
		condition("send_id = $%d", key.SendId)
	}
	if len(key.MessageIdHash) != 0 {
		condition("message_id_hash = $%d", key.MessageIdHash)
	}
	if key.NetworkId != (sidecar.Id{}) {
		condition("network_id = $%d", server.Id(key.NetworkId))
	}
	if key.SendingAddress.IsValid() {
		condition("sending_address = $%d", key.SendingAddress.Unmap())
	}
	if !key.Since.IsZero() {
		condition("$%d <= start_time", key.Since.UTC())
		condition("$%d <= send_day", smtpUtcDay(key.Since))
	}
	if !key.Until.IsZero() {
		condition("start_time <= $%d", key.Until.UTC())
		condition("send_day <= $%d", smtpUtcDay(key.Until))
	}
	query := `
		SELECT
			send_id,
			network_id,
			client_id,
			sending_address,
			recipient_domain,
			mx_host,
			start_time,
			end_time,
			byte_count,
			message_id_hash,
			x_ur_ref_key_id,
			recipient_count,
			message_byte_count,
			tls_version,
			tls_level,
			result,
			retry_of_send_id
		FROM smtp_send
		WHERE ` + strings.Join(conditions, " AND ") + `
		ORDER BY start_time, send_id
	`
	return query, args
}

// The proven recipients of networkId's sends that started at or after since,
// for §14.8's rates. Zero when the binary does not link MG9, with
// ErrSmtpEdgeUnlinked.
func SmtpRecipientCount(ctx context.Context, networkId server.Id, since time.Time) (count uint64, returnErr error) {
	if !server.SidecarMg9Linked() {
		returnErr = ErrSmtpEdgeUnlinked
		return
	}
	server.Db(ctx, func(conn server.PgConn) {
		var sum int64
		server.Raise(conn.QueryRow(
			ctx,
			`
			SELECT COALESCE(SUM(recipient_count), 0)::bigint
			FROM smtp_send
			WHERE network_id = $1 AND $2 <= start_time AND $3 <= send_day
			`,
			networkId,
			since.UTC(),
			smtpUtcDay(since),
		).Scan(&sum))
		count = uint64(sum)
	})
	return
}
