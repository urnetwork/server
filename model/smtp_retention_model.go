package model

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
)

// smtpedge's taskworker jobs, their model half (§14.14, §2.8 smtp-3 to smtp-6, D-SMTP-10, D-SMTP-12, D-SMTP-23,
// I-13, I-15; SMTP-M9-17): retention, sealed reports, key expiry and the RUA check's reads.
//
//	smtp_send          daily leaves the registry's runway keeps AheadDays ahead, and retention's own pass again
//	                   (EnsureSmtpSendRunway), so a missing day never fails an insert; each day retired at 30 days
//	                   through the family's Retire once every §21.9 hold has copied its network's rows
//	                   (DropPrecondition, which runs again on the detached leaf inside the drop): a held day stays,
//	                   is reported each run, and holds back no other day
//	smtp_complaint     13 months from arrival; sealed_report cleared at 30 days, the row kept (owner decision 78)
//	smtp_dmarc_daily   13 months from the day it counts
//	smtp_halt          13 months from its start
//	smtp_dkim_key      a retired selector leaves DNS DkimRetiredKeepTimeout after its retirement, through §12's DKIM
//	                   writers in one transaction with its row's state, and the row goes DkimRemovedRowRetention
//	                   after its selector left DNS, so a late report still checks against the key (§14.8)
//
// Every sweep honors §21.9 in its own SQL: it skips a network with a live part 0, so a held network keeps its
// rows, sealed copies included, until its hold ends. Each sweep works oldest first in batches on an index of its
// time column under a run budget, so a backlog drains over runs and a run that stops resumes where the index
// says.
//
// Nothing here reads or writes a table its group does not link: MG5's report tables, MG9's smtp_send, smtp_halt
// and smtp_dkim_key.

// Tunables of the retention jobs.
type SmtpRetentionSettings struct {
	// smtp_complaint's, smtp_dmarc_daily's and smtp_halt's rows go this many months after their time: §14.14's
	// 13. Positive.
	RowRetentionMonths int
	// A sealed copy is cleared this long after its report arrived: §14.8's 30 days. Positive.
	SealedReportRetention time.Duration
	// A retired selector stays in DNS this long: §14.6's 7 days. Positive.
	DkimRetiredKeepTimeout time.Duration
	// A key's row stays this long after its selector left DNS: §14.14's 30 days. Positive.
	DkimRemovedRowRetention time.Duration
	// Rows one transaction deletes or clears. Positive.
	BatchSize int
	// How long one sweep runs before it leaves the rest of a backlog to its next run. Positive.
	RunBudget time.Duration
	// Retired selectors one run takes out of DNS, each its own transaction and journal write. Positive.
	DkimExpiryBatchSize int
}

func DefaultSmtpRetentionSettings() *SmtpRetentionSettings {
	return &SmtpRetentionSettings{
		RowRetentionMonths:      13,
		SealedReportRetention:   30 * 24 * time.Hour,
		DkimRetiredKeepTimeout:  7 * 24 * time.Hour,
		DkimRemovedRowRetention: 30 * 24 * time.Hour,
		BatchSize:               1000,
		RunBudget:               30 * time.Second,
		DkimExpiryBatchSize:     100,
	}
}

// The registry's runway for smtp_send at now: every missing day of its window made, AheadDays ahead. Nothing in a
// binary that does not link MG9, whose family reports itself not partitioned.
func EnsureSmtpSendRunway(ctx context.Context, now time.Time) *server.PartitionRunwayReport {
	return smtpSendPartitionFamily.EnsureRunway(ctx, now)
}

// Retires smtp_send's days past their 30 days at now, oldest first, until deadline: each drops once every hold has
// copied its network's rows, and a held day stays in the report's Blocked, holding back no later day.
func RetireSmtpSendDays(ctx context.Context, now time.Time, deadline time.Time) *server.PartitionRetireReport {
	report, _ := smtpSendPartitionFamily.Retire(ctx, now, server.PartitionFamilyState{}, deadline)
	return report
}

// The hold clause every sweep below shares: no live part 0 names the row's network.
const smtpRetentionNotHeldSql = `
	NOT EXISTS (
		SELECT 1
		FROM abuse_legal_hold hold
		WHERE
			hold.network_id = %[1]s.network_id AND
			hold.part = 0 AND
			(hold.expire_time IS NULL OR $2 < hold.expire_time)
	)
`

func smtpRetentionNotHeld(alias string) string {
	return fmt.Sprintf(smtpRetentionNotHeldSql, alias)
}

// smtp_complaint's rows past 13 months: $1 the cutoff, $2 now, $3 the batch.
var removeExpiredSmtpComplaintsSql = `
	DELETE FROM smtp_complaint
	WHERE complaint_id IN (
		SELECT complaint_row.complaint_id
		FROM smtp_complaint complaint_row
		WHERE
			complaint_row.arrival_time < $1 AND
			` + smtpRetentionNotHeld("complaint_row") + `
		ORDER BY complaint_row.arrival_time
		LIMIT $3
		FOR UPDATE SKIP LOCKED
	)
`

// smtp_dmarc_daily's rows past 13 months, by day.
var removeExpiredSmtpDmarcDaysSql = `
	DELETE FROM smtp_dmarc_daily
	WHERE (network_id, day, reporter, header_from, report_id, policy_type) IN (
		SELECT day_row.network_id, day_row.day, day_row.reporter, day_row.header_from, day_row.report_id, day_row.policy_type
		FROM smtp_dmarc_daily day_row
		WHERE
			day_row.day < $1 AND
			` + smtpRetentionNotHeld("day_row") + `
		ORDER BY day_row.day
		LIMIT $3
		FOR UPDATE SKIP LOCKED
	)
`

// smtp_halt's rows past 13 months from their start.
var removeExpiredSmtpHaltsSql = `
	DELETE FROM smtp_halt
	WHERE halt_id IN (
		SELECT halt_row.halt_id
		FROM smtp_halt halt_row
		WHERE
			halt_row.start_time < $1 AND
			` + smtpRetentionNotHeld("halt_row") + `
		ORDER BY halt_row.start_time
		LIMIT $3
		FOR UPDATE SKIP LOCKED
	)
`

// Sealed copies past 30 days, cleared with their row kept, on the partial index of rows that hold one.
var clearExpiredSmtpSealedReportsSql = `
	UPDATE smtp_complaint
	SET sealed_report = NULL
	WHERE complaint_id IN (
		SELECT complaint_row.complaint_id
		FROM smtp_complaint complaint_row
		WHERE
			complaint_row.sealed_report IS NOT NULL AND
			complaint_row.arrival_time < $1 AND
			` + smtpRetentionNotHeld("complaint_row") + `
		ORDER BY complaint_row.arrival_time
		LIMIT $3
		FOR UPDATE SKIP LOCKED
	)
`

// smtp_dkim_key's rows past 30 days out of DNS.
var removeExpiredSmtpDkimKeysSql = `
	DELETE FROM smtp_dkim_key
	WHERE (selector, domain) IN (
		SELECT key_row.selector, key_row.domain
		FROM smtp_dkim_key key_row
		WHERE
			key_row.state = '` + SmtpDkimKeyStateRemoved + `' AND
			key_row.remove_time < $1 AND
			` + smtpRetentionNotHeld("key_row") + `
		ORDER BY key_row.remove_time
		LIMIT $3
		FOR UPDATE SKIP LOCKED
	)
`

// The 13-month cutoff of a date column: the UTC midnight starting now's day, that many months back.
func smtpRetentionDayCutoff(now time.Time, settings *SmtpRetentionSettings) time.Time {
	return smtpUtcDay(now).AddDate(0, -settings.RowRetentionMonths, 0)
}

// The 13-month cutoff of a timestamp column.
func smtpRetentionTimeCutoff(now time.Time, settings *SmtpRetentionSettings) time.Time {
	return now.UTC().AddDate(0, -settings.RowRetentionMonths, 0)
}

// What one row sweep removed, by table, and whether a budget left some for the next run.
type SmtpRowRetentionResult struct {
	RemovedCounts map[string]int64
	More          bool
}

// Deletes the rows of smtp_complaint, smtp_dmarc_daily and smtp_halt past their 13 months at now, oldest first,
// in batches, skipping a held network. The MG5 tables need MG5 and smtp_halt MG9; a table whose group the binary
// does not link is left alone.
func RemoveExpiredSmtpRows(ctx context.Context, now time.Time, settings *SmtpRetentionSettings) *SmtpRowRetentionResult {
	result := &SmtpRowRetentionResult{RemovedCounts: map[string]int64{}}
	type sweep struct {
		table  string
		sql    string
		cutoff time.Time
		linked func() bool
	}
	sweeps := []sweep{
		{table: RetentionTableSmtpComplaint, sql: removeExpiredSmtpComplaintsSql, cutoff: smtpRetentionTimeCutoff(now, settings), linked: server.SidecarMg5Linked},
		{table: RetentionTableSmtpDmarcDaily, sql: removeExpiredSmtpDmarcDaysSql, cutoff: smtpRetentionDayCutoff(now, settings), linked: server.SidecarMg5Linked},
		{table: RetentionTableSmtpHalt, sql: removeExpiredSmtpHaltsSql, cutoff: smtpRetentionTimeCutoff(now, settings), linked: server.SidecarMg9Linked},
	}
	for _, s := range sweeps {
		// abuse_legal_hold is MG5's, which every sweep reads
		if !s.linked() || !server.SidecarMg5Linked() {
			continue
		}
		removedCount, more := removeRetentionBatches(ctx, s.table, settings.BatchSize, time.Now().Add(settings.RunBudget), func(tx server.PgTx) int64 {
			tag := server.RaisePgResult(tx.Exec(ctx, s.sql, s.cutoff, now.UTC(), settings.BatchSize))
			return tag.RowsAffected()
		})
		result.RemovedCounts[s.table] = removedCount
		result.More = result.More || more
	}
	return result
}

// Clears the sealed copies of reports that arrived more than 30 days before now, the rows kept, oldest first, in
// batches, skipping a held network. Returns how many were cleared and whether the run left some for the next.
// Nothing in a binary that does not link MG5.
func ClearExpiredSmtpSealedReports(ctx context.Context, now time.Time, settings *SmtpRetentionSettings) (clearedCount int64, more bool) {
	if !server.SidecarMg5Linked() {
		return 0, false
	}
	cutoff := now.UTC().Add(-settings.SealedReportRetention)
	deadline := time.Now().Add(settings.RunBudget)
	for {
		select {
		case <-ctx.Done():
			return clearedCount, true
		default:
		}
		var batchCount int64
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			tag := server.RaisePgResult(tx.Exec(ctx, clearExpiredSmtpSealedReportsSql, cutoff, now.UTC(), settings.BatchSize))
			batchCount = tag.RowsAffected()
		}, server.TxReadCommitted)
		clearedCount += batchCount
		if batchCount < int64(settings.BatchSize) {
			return clearedCount, false
		}
		if !time.Now().Before(deadline) {
			return clearedCount, true
		}
	}
}

// What one key expiry run did.
type SmtpDkimExpiryResult struct {
	// Retired selectors taken out of DNS, their rows now removed.
	RemovedFromDnsCount int
	// Rows deleted 30 days after their selector left DNS.
	DeletedCount int64
	// Selectors whose DNS write failed, left retired for the next run.
	FailedCount int
	More        bool
}

// One key's place in the selector table.
type smtpDkimKeyRef struct {
	selector  string
	domain    string
	networkId server.Id
	sidecarId server.Id
}

// Who a DKIM record write's admin event names as its actor (§7.11). A zero one is the system's.
type smtpDkimActor struct {
	kind      string
	id        *server.Id
	requestId *server.Id
}

// Takes the selectors retired more than DkimRetiredKeepTimeout before now out of DNS, each in its own transaction
// with its row's move to removed, then deletes the rows removed more than DkimRemovedRowRetention before now,
// skipping a held network's. A selector whose DNS write fails stays retired, and the next run takes it again.
// Nothing in a binary that does not link MG9, MG5 and MG2.
func ExpireSmtpDkimKeys(ctx context.Context, now time.Time, settings *SmtpRetentionSettings) *SmtpDkimExpiryResult {
	result := &SmtpDkimExpiryResult{}
	if !server.SidecarMg9Linked() || !server.SidecarMg5Linked() || !server.SidecarMg2Linked() {
		return result
	}
	retiredBefore := now.UTC().Add(-settings.DkimRetiredKeepTimeout)
	refs := []*smtpDkimKeyRef{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(
			ctx,
			`
			SELECT selector, domain, network_id, sidecar_id
			FROM smtp_dkim_key
			WHERE state = $1 AND retire_time < $2
			ORDER BY retire_time, selector, domain
			LIMIT $3
			`,
			SmtpDkimKeyStateRetired,
			retiredBefore,
			settings.DkimExpiryBatchSize,
		)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				ref := &smtpDkimKeyRef{}
				server.Raise(rows.Scan(&ref.selector, &ref.domain, &ref.networkId, &ref.sidecarId))
				refs = append(refs, ref)
			}
		})
	})
	result.More = len(refs) == settings.DkimExpiryBatchSize
	for _, ref := range refs {
		removed := false
		err := dnsCatchRefusal(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				lockSmtpDkimNetworkZonesInTx(ctx, tx, ref.networkId)
				var state string
				var retireTime *time.Time
				found := false
				rows, err := tx.Query(
					ctx,
					`SELECT state, retire_time FROM smtp_dkim_key WHERE selector = $1 AND domain = $2 FOR UPDATE`,
					ref.selector,
					ref.domain,
				)
				server.WithPgResult(rows, err, func() {
					if rows.Next() {
						server.Raise(rows.Scan(&state, &retireTime))
						found = true
					}
				})
				// a removal or another run took it since the read
				if !found || state != SmtpDkimKeyStateRetired || retireTime == nil || !retireTime.Before(retiredBefore) {
					return
				}
				if err := deleteSmtpDkimSelectorRecordsInTx(ctx, tx, ref, smtpDkimActor{}); err != nil {
					panic(dnsWriteRefusal{err: err})
				}
				server.RaisePgResult(tx.Exec(
					ctx,
					`UPDATE smtp_dkim_key SET state = $3, remove_time = $4 WHERE selector = $1 AND domain = $2`,
					ref.selector,
					ref.domain,
					SmtpDkimKeyStateRemoved,
					now.UTC(),
				))
				removed = true
			}, server.TxReadCommitted)
		})
		switch {
		case err != nil:
			// the selector's id is opaque and random, so a log may carry it (I-13)
			result.FailedCount += 1
			glog.Warningf("[smtpretention]retired selector %s stays in dns for the next run: %v\n", ref.selector, err)
		case removed:
			result.RemovedFromDnsCount += 1
		}
	}
	deletedCount, moreDeletes := removeRetentionBatches(ctx, RetentionTableSmtpDkimKey, settings.BatchSize, time.Now().Add(settings.RunBudget), func(tx server.PgTx) int64 {
		tag := server.RaisePgResult(tx.Exec(ctx, removeExpiredSmtpDkimKeysSql, now.UTC().Add(-settings.DkimRemovedRowRetention), now.UTC(), settings.BatchSize))
		return tag.RowsAffected()
	})
	result.DeletedCount = deletedCount
	result.More = result.More || moreDeletes
	return result
}

// The network's roots in name order, each as the zone writers key it.
func smtpDkimNetworkRootsInTx(ctx context.Context, tx server.PgTx, networkId server.Id) []string {
	roots := []string{}
	for _, root := range getNetworkDnsRootsInTx(ctx, tx, networkId) {
		root = dnsLabelRoot(root)
		if !slices.Contains(roots, root) {
			roots = append(roots, root)
		}
	}
	slices.Sort(roots)
	return roots
}

// Takes the network's label lock, then each of its zones' locks in root order, as every zone writer takes them
// (dns_write_model.go), before a DKIM write reads its rows: the route, the removal effect and key expiry all lock
// in this order, then the selector rows, so none waits on another holding what it needs next.
func lockSmtpDkimNetworkZonesInTx(ctx context.Context, tx server.PgTx, networkId server.Id) {
	lockDnsLabelInTx(ctx, tx, networkId)
	for _, root := range smtpDkimNetworkRootsInTx(ctx, tx, networkId) {
		lockDnsZoneInTx(ctx, tx, root, networkId)
	}
}

// Where a key's selector record sits in a root whose apex is apex: the burner writer's label for a domain that is a
// replying burner's word-number label under the apex, or "" with ok for the apex itself; ok false for a domain
// under neither, a Team domain's, whose _domainkey CNAMEs each selector to the apex (§14.6).
func smtpDkimBurnerLabelOf(domain string, apex string) (burnerLabel string, ok bool) {
	domain = strings.ToLower(strings.TrimSuffix(domain, "."))
	apex = strings.ToLower(strings.TrimSuffix(apex, "."))
	if domain == apex {
		return "", true
	}
	label, under := strings.CutSuffix(domain, "."+apex)
	if under && label != "" && !strings.Contains(label, ".") && sidecar.IsWordNumberDnsLabel(label) {
		return label, true
	}
	return "", false
}

// Takes ref's selector record out of every zone of its network that holds it, in tx, which has its zones' locks:
// the burner writer at a burner's label, else the network writer at the apex, for the network's own name in its
// root and for a Team domain in each root, since a Team domain names no root. A record already gone writes nothing.
// actor is the writes' admin events', the system's for a removal effect or expiry.
func deleteSmtpDkimSelectorRecordsInTx(ctx context.Context, tx server.PgTx, ref *smtpDkimKeyRef, actor smtpDkimActor) error {
	type write struct {
		root        string
		burnerLabel string
	}
	writes := []write{}
	teamDomain := true
	roots := smtpDkimNetworkRootsInTx(ctx, tx, ref.networkId)
	for _, root := range roots {
		label := getNetworkLabelInTx(ctx, tx, ref.networkId, root)
		if label == "" {
			continue
		}
		if burnerLabel, ok := smtpDkimBurnerLabelOf(ref.domain, label+"."+root); ok {
			writes = append(writes, write{root: root, burnerLabel: burnerLabel})
			teamDomain = false
		}
	}
	if teamDomain {
		for _, root := range roots {
			if getNetworkLabelInTx(ctx, tx, ref.networkId, root) != "" {
				writes = append(writes, write{root: root})
			}
		}
	}
	sidecarId := ref.sidecarId
	for _, w := range writes {
		selector := &DnsDkimSelector{
			Root:        w.root,
			NetworkId:   ref.networkId,
			Selector:    ref.selector,
			BurnerLabel: w.burnerLabel,
			SidecarId:   &sidecarId,
			ActorKind:   actor.kind,
			ActorId:     actor.id,
			RequestId:   actor.requestId,
		}
		var err error
		if w.burnerLabel != "" {
			_, err = DnsDeleteBurnerSelectorInTx(ctx, tx, selector)
		} else {
			_, err = DnsDeleteDkimSelectorInTx(ctx, tx, selector)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// The RUA check's reads (smtpedgeservice.SmtpReconcileStore) on smtp_dmarc_daily and smtp_send: one day's
// networks with DMARC aggregate counts, in id order after a cursor, each with its reported messages and the
// recipients its sends that day proved. smtp_dmarc_daily's (day, network_id) index pages the networks, and each
// network's sends read its day's leaf by smtp_send_network_id. Needs MG5 and MG9.
var _ smtpedgeservice.SmtpReconcileStore = (*SmtpEdgeStore)(nil)

// What a reconcile read answers in a binary that does not link MG5 and MG9.
var ErrSmtpReconcileUnlinked = errors.New("the rua check needs MG5 and MG9")

// See smtpedgeservice.SmtpReconcileStore.
func (self *SmtpEdgeStore) ReconcileDays(ctx context.Context, day time.Time, afterNetworkId sidecar.Id, limit int) ([]*smtpedgeservice.SmtpReconcileDay, error) {
	var days []*smtpedgeservice.SmtpReconcileDay
	linked := func() bool {
		return server.SidecarMg5Linked() && server.SidecarMg9Linked()
	}
	err := self.call(ctx, linked, ErrSmtpReconcileUnlinked, func(ctx context.Context) (err error) {
		days, err = GetSmtpReconcileDays(ctx, day, server.Id(afterNetworkId), limit)
		return
	})
	if err != nil {
		return nil, err
	}
	return days, nil
}

// The largest count a bigint holds, which a sum is held at rather than overflowing.
const smtpReconcileBigintMax = "9223372036854775807"

// One page of day's networks for the RUA check: those with DMARC aggregate counts that day after afterNetworkId,
// in id order, at most limit.
func GetSmtpReconcileDays(ctx context.Context, day time.Time, afterNetworkId server.Id, limit int) (days []*smtpedgeservice.SmtpReconcileDay, returnErr error) {
	if !server.SidecarMg5Linked() || !server.SidecarMg9Linked() {
		returnErr = ErrSmtpReconcileUnlinked
		return
	}
	if limit <= 0 {
		returnErr = fmt.Errorf("%w: a page of %d", ErrSmtpRowRefused, limit)
		return
	}
	sendDay := smtpUtcDay(day)
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				reported.network_id,
				reported.message_count,
				COALESCE(proven.recipient_count, 0)
			FROM (
				SELECT
					day_row.network_id,
					LEAST(COALESCE(SUM(day_row.message_count), 0), `+smtpReconcileBigintMax+`)::bigint AS message_count
				FROM smtp_dmarc_daily day_row
				WHERE day_row.day = $1 AND day_row.kind = $2 AND $3 < day_row.network_id
				GROUP BY day_row.network_id
				ORDER BY day_row.network_id
				LIMIT $4
			) reported
			LEFT JOIN LATERAL (
				-- LEAST passes over a NULL, so a day with no sends sums 0 first
				SELECT LEAST(COALESCE(SUM(send.recipient_count), 0), `+smtpReconcileBigintMax+`)::bigint AS recipient_count
				FROM smtp_send send
				WHERE send.send_day = $1 AND send.network_id = reported.network_id
			) proven ON true
			ORDER BY reported.network_id
			`,
			sendDay,
			smtpedgeservice.SmtpAggregateKindDmarc,
			afterNetworkId,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId server.Id
				var messageCount int64
				var recipientCount int64
				server.Raise(result.Scan(&networkId, &messageCount, &recipientCount))
				days = append(days, &smtpedgeservice.SmtpReconcileDay{
					NetworkId:            sidecar.Id(networkId),
					Day:                  sendDay,
					ReportedMessageCount: uint64(max(messageCount, 0)),
					ProvenRecipientCount: uint64(max(recipientCount, 0)),
				})
			}
		})
	})
	return
}
