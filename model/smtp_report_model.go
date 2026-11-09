package model

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
)

// smtpedge's report tables (§14.8, §14.14; SMTP-M5-06): smtp_complaint and
// smtp_dmarc_daily, MG5's §14 part (plan-SMTP-report-intake-home). mxedge's
// platform block writes them through SmtpEdgeStore's InsertComplaint and
// InsertAggregate while its listener waits to answer a DATA or BDAT, which is
// a reporter's session and never a connection's packets (I-12).
//
// A row holds only §2.8 smtp-5's fields, in smtpedgeservice's record types,
// which TestSmtpRecordsHoldNoThirdPartyAddress holds to that row: no recipient,
// no reporter's address, no last-hop Received IP and no report text
// (D-SMTP-10). A string a column cannot hold whole, or that Postgres cannot
// store as text (invalid UTF-8, a NUL), refuses the row rather than storing it
// cut: report fields are attribution evidence, and a cut one would join wrong.
//
// Nothing here reads or writes either table unless the binary links MG5: every
// write answers ErrSmtpReportUnlinked and every read finds nothing.

// What a write answers in a binary that does not link MG5, so code reading the
// report tables stays off until M5's Gn-out (§19.7).
var ErrSmtpReportUnlinked = errors.New("smtp report tables need MG5")

// A complaint, aggregate, halt or DKIM key a row of smtpedge's tables cannot
// hold: a missing key, a word outside its closed set, or a string past its
// column or not storable as text.
var ErrSmtpRowRefused = errors.New("smtp row refused")

// The column bounds of smtpedge's tables, in bytes, which bound characters
// too. Format values, as the migrations fix them.
const (
	smtpDomainMaxByteCount = 253
	// a DNS label's: a DKIM selector or a burner label
	smtpLabelMaxByteCount      = 63
	smtpWordMaxByteCount       = 32
	smtpXUrRefMaxByteCount     = 64
	smtpFeedbackIdMaxByteCount = 128
	smtpReportIdMaxByteCount   = 1024
	smtpPolicyTypeMaxByteCount = 16
	// SHA-256's, as smtpedgeservice.SmtpMessageIdHash gives it.
	smtpMessageIdHashByteCount = 32
)

// Whether value can be stored in a text column of maxByteCount bytes: valid
// UTF-8 with no NUL, which Postgres refuses in text, and no longer than the
// column.
func smtpTextStorable(value string, maxByteCount int) bool {
	return len(value) <= maxByteCount && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

// value, or nil for an empty string, for a NULL column.
func smtpNullableText(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// value, or nil for an empty slice, for a NULL column.
func smtpNullableBytes(value []byte) []byte {
	if len(value) == 0 {
		return nil
	}
	return value
}

// A stored text column read back, "" for NULL.
func smtpTextOf(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// A count as a bigint holds it. A report cannot honestly count past 2^63-1
// messages, so a larger count is held at the largest.
func smtpCountBigint(count uint64) int64 {
	if math.MaxInt64 < count {
		return math.MaxInt64
	}
	return int64(count)
}

// Why complaint cannot be a smtp_complaint row, or nil.
func smtpComplaintRefusal(complaint *smtpedgeservice.SmtpComplaint) error {
	refuse := func(field string) error {
		return fmt.Errorf("%w: a complaint's %s", ErrSmtpRowRefused, field)
	}
	switch {
	case complaint == nil:
		return refuse("row")
	case complaint.ComplaintId == (sidecar.Id{}):
		return refuse("complaint id")
	case complaint.NetworkId == (sidecar.Id{}):
		return refuse("network")
	case complaint.Type == "" || !smtpTextStorable(complaint.Type, smtpWordMaxByteCount):
		return refuse("type")
	case complaint.ArrivalTime.IsZero():
		return refuse("arrival")
	case !smtpTextStorable(complaint.Reporter, smtpDomainMaxByteCount):
		return refuse("reporter")
	case !smtpTextStorable(complaint.Domain, smtpDomainMaxByteCount):
		return refuse("domain")
	case !smtpTextStorable(complaint.DkimDomain, smtpDomainMaxByteCount):
		return refuse("DKIM domain")
	case !smtpTextStorable(complaint.DkimSelector, smtpLabelMaxByteCount):
		return refuse("DKIM selector")
	case !smtpTextStorable(complaint.DkimResult, smtpWordMaxByteCount):
		return refuse("DKIM result")
	case len(complaint.MessageIdHash) != 0 && len(complaint.MessageIdHash) != smtpMessageIdHashByteCount:
		return refuse("Message-ID hash")
	case !smtpTextStorable(complaint.XUrRef, smtpXUrRefMaxByteCount):
		return refuse("X-UR-Ref")
	case !smtpTextStorable(complaint.FeedbackId, smtpFeedbackIdMaxByteCount):
		return refuse("Feedback-ID")
	case !smtpTextStorable(complaint.JoinResult, smtpWordMaxByteCount):
		return refuse("join result")
	}
	return nil
}

// Stores complaint in tx: an attributed report's fields and its sealed copy,
// or an aggregate's sealed copy under type rua or tlsrpt. A complaint id
// already stored is left as first written, so a retried write stores nothing
// twice. ErrSmtpRowRefused for a row the table cannot hold.
func InsertSmtpComplaintInTx(ctx context.Context, tx server.PgTx, complaint *smtpedgeservice.SmtpComplaint) error {
	if !server.SidecarMg5Linked() {
		return ErrSmtpReportUnlinked
	}
	if err := smtpComplaintRefusal(complaint); err != nil {
		return err
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`
		INSERT INTO smtp_complaint (
			complaint_id,
			network_id,
			reporter,
			complaint_type,
			arrival_time,
			domain,
			dkim_domain,
			dkim_selector,
			dkim_result,
			message_id_hash,
			x_ur_ref,
			feedback_id,
			join_result,
			sealed_report
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		ON CONFLICT (complaint_id) DO NOTHING
		`,
		server.Id(complaint.ComplaintId),
		server.Id(complaint.NetworkId),
		smtpNullableText(complaint.Reporter),
		complaint.Type,
		complaint.ArrivalTime.UTC(),
		smtpNullableText(complaint.Domain),
		smtpNullableText(complaint.DkimDomain),
		smtpNullableText(complaint.DkimSelector),
		smtpNullableText(complaint.DkimResult),
		smtpNullableBytes(complaint.MessageIdHash),
		smtpNullableText(complaint.XUrRef),
		smtpNullableText(complaint.FeedbackId),
		smtpNullableText(complaint.JoinResult),
		smtpNullableBytes(complaint.SealedReport),
	))
	return nil
}

// InsertSmtpComplaintInTx in a transaction of its own, opened only for a row
// it can store.
func InsertSmtpComplaint(ctx context.Context, complaint *smtpedgeservice.SmtpComplaint) (returnErr error) {
	if !server.SidecarMg5Linked() {
		return ErrSmtpReportUnlinked
	}
	if err := smtpComplaintRefusal(complaint); err != nil {
		return err
	}
	server.Tx(ctx, func(tx server.PgTx) {
		returnErr = InsertSmtpComplaintInTx(ctx, tx, complaint)
	})
	return
}

// One smtp_dmarc_daily row's key within a report.
type smtpAggregateRowKey struct {
	networkId  sidecar.Id
	domain     string
	policyType string
}

// Adds b to a, stopping at the largest count rather than wrapping.
func smtpSaturatingAdd(a uint64, b uint64) uint64 {
	if a+b < a {
		return math.MaxUint64
	}
	return a + b
}

// report's counts as smtp_dmarc_daily's rows, one per network, header_from and
// policy type, counts a report repeats for one key summed, or why report cannot
// be stored.
func smtpAggregateRows(report *smtpedgeservice.SmtpAggregateReport) ([]*smtpedgeservice.SmtpAggregateCount, error) {
	refuse := func(field string) error {
		return fmt.Errorf("%w: an aggregate's %s", ErrSmtpRowRefused, field)
	}
	switch {
	case report == nil:
		return nil, refuse("report")
	case report.Reporter == "" || !smtpTextStorable(report.Reporter, smtpDomainMaxByteCount):
		return nil, refuse("reporter")
	case report.ReportId == "" || !smtpTextStorable(report.ReportId, smtpReportIdMaxByteCount):
		return nil, refuse("report id")
	case report.Kind != smtpedgeservice.SmtpAggregateKindDmarc && report.Kind != smtpedgeservice.SmtpAggregateKindTlsRpt:
		return nil, refuse("kind")
	case report.Day.IsZero():
		return nil, refuse("day")
	case report.ArrivalTime.IsZero():
		return nil, refuse("arrival")
	case len(report.Counts) == 0:
		// no row would remember the report, so a repeat would count again
		return nil, refuse("counts, which are none")
	}
	rowKeyCounts := map[smtpAggregateRowKey]*smtpedgeservice.SmtpAggregateCount{}
	rows := []*smtpedgeservice.SmtpAggregateCount{}
	for _, count := range report.Counts {
		switch {
		case count == nil:
			return nil, refuse("count")
		case count.NetworkId == (sidecar.Id{}):
			return nil, refuse("count's network")
		case count.Domain == "" || !smtpTextStorable(count.Domain, smtpDomainMaxByteCount):
			return nil, refuse("count's header_from")
		case !smtpTextStorable(count.PolicyType, smtpPolicyTypeMaxByteCount):
			return nil, refuse("count's policy type")
		}
		key := smtpAggregateRowKey{networkId: count.NetworkId, domain: count.Domain, policyType: count.PolicyType}
		row, ok := rowKeyCounts[key]
		if !ok {
			row = &smtpedgeservice.SmtpAggregateCount{NetworkId: count.NetworkId, Domain: count.Domain, PolicyType: count.PolicyType}
			rowKeyCounts[key] = row
			rows = append(rows, row)
		}
		row.MessageCount = smtpSaturatingAdd(row.MessageCount, count.MessageCount)
		row.DkimPassCount = smtpSaturatingAdd(row.DkimPassCount, count.DkimPassCount)
		row.SpfPassCount = smtpSaturatingAdd(row.SpfPassCount, count.SpfPassCount)
		row.QuarantineCount = smtpSaturatingAdd(row.QuarantineCount, count.QuarantineCount)
		row.RejectCount = smtpSaturatingAdd(row.RejectCount, count.RejectCount)
		row.SuccessCount = smtpSaturatingAdd(row.SuccessCount, count.SuccessCount)
		row.FailureCount = smtpSaturatingAdd(row.FailureCount, count.FailureCount)
	}
	return rows, nil
}

// The UTC midnight starting t's day, as a date column holds it.
func smtpUtcDay(t time.Time) time.Time {
	year, month, day := t.UTC().Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

// The key of the transaction-scoped lock that orders writers of one
// (reporter, report_id): its parts length-prefixed, so no two pairs share it.
// A hash that two pairs did share would only order their writes.
func smtpAggregateLockKey(reporter string, reportId string) string {
	return fmt.Sprintf("smtp_dmarc_daily:%d:%s:%s", len(reporter), reporter, reportId)
}

// Stores report's counts once per (Reporter, ReportId), whatever networks its
// counts name (§14.8, D-SMTP-23): inserted is false, and nothing changes, when
// any row of that pair is stored, so a replay with other counts counts
// nothing. Writers of one pair take a transaction-scoped lock on it, then read
// whether it is stored at read committed, so two blocks taking the same
// report at once store it once. ErrSmtpRowRefused for a report the table
// cannot hold.
func InsertSmtpAggregate(ctx context.Context, report *smtpedgeservice.SmtpAggregateReport) (inserted bool, returnErr error) {
	if !server.SidecarMg5Linked() {
		returnErr = ErrSmtpReportUnlinked
		return
	}
	rows, err := smtpAggregateRows(report)
	if err != nil {
		returnErr = err
		return
	}
	networkIds := make([]server.Id, 0, len(rows))
	domains := make([]string, 0, len(rows))
	policyTypes := make([]string, 0, len(rows))
	messageCounts := make([]int64, 0, len(rows))
	dkimPassCounts := make([]int64, 0, len(rows))
	spfPassCounts := make([]int64, 0, len(rows))
	quarantineCounts := make([]int64, 0, len(rows))
	rejectCounts := make([]int64, 0, len(rows))
	successCounts := make([]int64, 0, len(rows))
	failureCounts := make([]int64, 0, len(rows))
	for _, row := range rows {
		networkIds = append(networkIds, server.Id(row.NetworkId))
		domains = append(domains, row.Domain)
		policyTypes = append(policyTypes, row.PolicyType)
		messageCounts = append(messageCounts, smtpCountBigint(row.MessageCount))
		dkimPassCounts = append(dkimPassCounts, smtpCountBigint(row.DkimPassCount))
		spfPassCounts = append(spfPassCounts, smtpCountBigint(row.SpfPassCount))
		quarantineCounts = append(quarantineCounts, smtpCountBigint(row.QuarantineCount))
		rejectCounts = append(rejectCounts, smtpCountBigint(row.RejectCount))
		successCounts = append(successCounts, smtpCountBigint(row.SuccessCount))
		failureCounts = append(failureCounts, smtpCountBigint(row.FailureCount))
	}
	server.Tx(ctx, func(tx server.PgTx) {
		// a retried attempt starts over
		inserted = false
		server.RaisePgResult(tx.Exec(
			ctx,
			`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
			smtpAggregateLockKey(report.Reporter, report.ReportId),
		))
		var stored bool
		server.Raise(tx.QueryRow(
			ctx,
			`SELECT EXISTS (SELECT 1 FROM smtp_dmarc_daily WHERE reporter = $1 AND report_id = $2)`,
			report.Reporter,
			report.ReportId,
		).Scan(&stored))
		if stored {
			return
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO smtp_dmarc_daily (
				network_id,
				day,
				reporter,
				header_from,
				report_id,
				policy_type,
				kind,
				arrival_time,
				message_count,
				dkim_pass_count,
				spf_pass_count,
				quarantine_count,
				reject_count,
				success_count,
				failure_count
			)
			SELECT
				item.network_id,
				$1,
				$2,
				item.header_from,
				$3,
				item.policy_type,
				$4,
				$5,
				item.message_count,
				item.dkim_pass_count,
				item.spf_pass_count,
				item.quarantine_count,
				item.reject_count,
				item.success_count,
				item.failure_count
			FROM unnest(
				$6::uuid[],
				$7::varchar[],
				$8::varchar[],
				$9::bigint[],
				$10::bigint[],
				$11::bigint[],
				$12::bigint[],
				$13::bigint[],
				$14::bigint[],
				$15::bigint[]
			) AS item (
				network_id,
				header_from,
				policy_type,
				message_count,
				dkim_pass_count,
				spf_pass_count,
				quarantine_count,
				reject_count,
				success_count,
				failure_count
			)
			`,
			smtpUtcDay(report.Day),
			report.Reporter,
			report.ReportId,
			report.Kind,
			report.ArrivalTime.UTC(),
			networkIds,
			domains,
			policyTypes,
			messageCounts,
			dkimPassCounts,
			spfPassCounts,
			quarantineCounts,
			rejectCounts,
			successCounts,
			failureCounts,
		))
		inserted = true
	}, server.TxReadCommitted)
	return
}

// One network's complaints that arrived at or after since, oldest first, their
// sealed copies included: what GET /smtp/reports reads (SMTP-M9-19) and a test
// checks. Nil when the binary does not link MG5.
func GetSmtpComplaints(ctx context.Context, networkId server.Id, since time.Time) (complaints []*smtpedgeservice.SmtpComplaint) {
	if !server.SidecarMg5Linked() {
		return nil
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				complaint_id,
				network_id,
				reporter,
				complaint_type,
				arrival_time,
				domain,
				dkim_domain,
				dkim_selector,
				dkim_result,
				message_id_hash,
				x_ur_ref,
				feedback_id,
				join_result,
				sealed_report
			FROM smtp_complaint
			WHERE network_id = $1 AND $2 <= arrival_time
			ORDER BY arrival_time, complaint_id
			`,
			networkId,
			since.UTC(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var complaintId server.Id
				var rowNetworkId server.Id
				var reporter *string
				var domain *string
				var dkimDomain *string
				var dkimSelector *string
				var dkimResult *string
				var xUrRef *string
				var feedbackId *string
				var joinResult *string
				complaint := &smtpedgeservice.SmtpComplaint{}
				server.Raise(result.Scan(
					&complaintId,
					&rowNetworkId,
					&reporter,
					&complaint.Type,
					&complaint.ArrivalTime,
					&domain,
					&dkimDomain,
					&dkimSelector,
					&dkimResult,
					&complaint.MessageIdHash,
					&xUrRef,
					&feedbackId,
					&joinResult,
					&complaint.SealedReport,
				))
				complaint.ComplaintId = sidecar.Id(complaintId)
				complaint.NetworkId = sidecar.Id(rowNetworkId)
				complaint.Reporter = smtpTextOf(reporter)
				complaint.Domain = smtpTextOf(domain)
				complaint.DkimDomain = smtpTextOf(dkimDomain)
				complaint.DkimSelector = smtpTextOf(dkimSelector)
				complaint.DkimResult = smtpTextOf(dkimResult)
				complaint.XUrRef = smtpTextOf(xUrRef)
				complaint.FeedbackId = smtpTextOf(feedbackId)
				complaint.JoinResult = smtpTextOf(joinResult)
				complaints = append(complaints, complaint)
			}
		})
	})
	return
}

// One network's aggregate counts of the UTC days from since's on, a report's
// rows together as one report holding only this network's counts, in the order
// the reports arrived: what GET /smtp/reports reads (SMTP-M9-19) and a test
// checks. Nil when the binary does not link MG5.
func GetSmtpAggregateReports(ctx context.Context, networkId server.Id, since time.Time) (reports []*smtpedgeservice.SmtpAggregateReport) {
	if !server.SidecarMg5Linked() {
		return nil
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				day,
				reporter,
				header_from,
				report_id,
				policy_type,
				kind,
				arrival_time,
				message_count,
				dkim_pass_count,
				spf_pass_count,
				quarantine_count,
				reject_count,
				success_count,
				failure_count
			FROM smtp_dmarc_daily
			WHERE network_id = $1 AND $2 <= day
			ORDER BY arrival_time, reporter, report_id, header_from, policy_type
			`,
			networkId,
			smtpUtcDay(since),
		)
		server.WithPgResult(result, err, func() {
			var report *smtpedgeservice.SmtpAggregateReport
			for result.Next() {
				var day time.Time
				var reporter string
				var reportId string
				var kind string
				var arrivalTime time.Time
				var counts [7]int64
				count := &smtpedgeservice.SmtpAggregateCount{NetworkId: sidecar.Id(networkId)}
				server.Raise(result.Scan(
					&day,
					&reporter,
					&count.Domain,
					&reportId,
					&count.PolicyType,
					&kind,
					&arrivalTime,
					&counts[0],
					&counts[1],
					&counts[2],
					&counts[3],
					&counts[4],
					&counts[5],
					&counts[6],
				))
				count.MessageCount = uint64(counts[0])
				count.DkimPassCount = uint64(counts[1])
				count.SpfPassCount = uint64(counts[2])
				count.QuarantineCount = uint64(counts[3])
				count.RejectCount = uint64(counts[4])
				count.SuccessCount = uint64(counts[5])
				count.FailureCount = uint64(counts[6])
				if report == nil || report.Reporter != reporter || report.ReportId != reportId {
					report = &smtpedgeservice.SmtpAggregateReport{
						Reporter:    reporter,
						ReportId:    reportId,
						Kind:        kind,
						Day:         smtpUtcDay(day),
						ArrivalTime: arrivalTime,
					}
					reports = append(reports, report)
				}
				report.Counts = append(report.Counts, count)
			}
		})
	})
	return
}
