package model

// §21.10's retention (ABUSE-M5-16; D-ABUSE-7, D-TRUST-7, R-ABUSE-5, I-18): §2.8's list is the only TTL table, and
// api-14's rows go by it and by nothing else:
//
//	abuse_case, abuse_action, abuse_appeal  13 months (CaseRetentionMonths) after the case last closes
//	                                        (plan-ABUSE-small-readings #9); an open case is never swept, and a case's
//	                                        actions and appeals go with it
//	legal_request                           3 years (LegalRetentionYears) after it closes; productions are its rows
//	abuse_access_log                        3 years after each read, its own ttl, so a row outlives the case it names
//	abuse_legal_hold                        to its kind's deadline, through AbuseHoldPass (ABUSE-M5-14)
//	network_abuse_state                     only while current, through AbuseStandingPass (ABUSE-M5-07)
//
// Holds (§21.9): this pass skips every in-scope row of a network HeldNetworkIds names, a case of a held network with
// its actions and appeals, a legal request naming one, and an access-log row naming one's case, so expiry resumes
// the sweep with no notice. Holds pause only server sweeps, GC and drops' deletions, never a user's shredding
// (D-ABUSE-8), and nothing here touches a shredding path. TestEveryRetentionSweepHonorsHolds holds every retention
// sweep of a §2.8 row to a line saying how it honors holds, or why the row is outside their reach.
//
// A network's own deletion of a held network (model.RemoveNetwork) reads as done to the user and, in its own
// transaction, copies the in-scope rows it deletes into each live hold's copied_rows, minus every credential:
// plan-ABUSE-hold-deletions #1's silent deferral (QD-008), with no refusal, hint or admin event.
//
// Every batch is its own transaction of at most RetentionBatchSize rows, so a pass stopped midway resumes where it
// stood at its next run. Nothing here reads or writes the tables unless the binary links MG5.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server"
)

// What one retention pass deleted, by table, and whether it left some for the next.
type AbuseRetentionResult struct {
	CaseCount         int64
	ActionCount       int64
	AppealCount       int64
	LegalRequestCount int64
	AccessLogCount    int64
	// the pass ran out of time with rows left
	More bool
}

// The cutoffs at now: a closed case older than caseCutoff goes, and a legal request or access-log row older than
// legalCutoff.
func abuseRetentionCutoffs(now time.Time, settings *AbuseSettings) (caseCutoff time.Time, legalCutoff time.Time) {
	now = now.UTC()
	return now.AddDate(0, -settings.CaseRetentionMonths, 0), now.AddDate(-settings.LegalRetentionYears, 0, 0)
}

// The held networks as a parameter.
func abuseRetentionHeldIds(held map[server.Id]bool) []server.Id {
	heldIds := []server.Id{}
	for networkId := range held {
		heldIds = append(heldIds, networkId)
	}
	slices.SortFunc(heldIds, func(a server.Id, b server.Id) int {
		return a.Cmp(b)
	})
	return heldIds
}

// One batch of cases past their 13 months, each with its actions and appeals, skipping a held network's.
const removeExpiredAbuseCasesSql = `
	WITH expired AS (
		SELECT abuse_case.case_id
		FROM abuse_case
		WHERE
			abuse_case.state = 'closed' AND
			abuse_case.close_time < $1 AND
			(abuse_case.network_id IS NULL OR NOT abuse_case.network_id = ANY($2))
		ORDER BY abuse_case.close_time, abuse_case.case_id
		LIMIT $3
		FOR UPDATE SKIP LOCKED
	),
	removed_actions AS (
		DELETE FROM abuse_action
		WHERE abuse_action.case_id IN (SELECT case_id FROM expired)
		RETURNING 1
	),
	removed_appeals AS (
		DELETE FROM abuse_appeal
		WHERE abuse_appeal.case_id IN (SELECT case_id FROM expired)
		RETURNING 1
	),
	removed_cases AS (
		DELETE FROM abuse_case
		WHERE abuse_case.case_id IN (SELECT case_id FROM expired)
		RETURNING 1
	)
	SELECT
		(SELECT COUNT(*) FROM removed_cases),
		(SELECT COUNT(*) FROM removed_actions),
		(SELECT COUNT(*) FROM removed_appeals)
`

// One batch of legal requests 3 years past their close, skipping one naming a held network.
const removeExpiredLegalRequestsSql = `
	DELETE FROM legal_request
	WHERE legal_request.request_id IN (
		SELECT legal_request.request_id
		FROM legal_request
		WHERE
			legal_request.close_time < $1 AND
			NOT legal_request.network_ids && $2::uuid[]
		ORDER BY legal_request.close_time, legal_request.request_id
		LIMIT $3
		FOR UPDATE SKIP LOCKED
	)
`

// One batch of access-log rows 3 years old, skipping one naming a case of a held network.
const removeExpiredAbuseAccessLogSql = `
	DELETE FROM abuse_access_log
	WHERE abuse_access_log.log_id IN (
		SELECT abuse_access_log.log_id
		FROM abuse_access_log
		WHERE
			abuse_access_log.access_time < $1 AND
			NOT EXISTS (
				SELECT 1
				FROM abuse_case
				WHERE
					abuse_case.case_id = abuse_access_log.case_id AND
					abuse_case.network_id = ANY($2)
			)
		ORDER BY abuse_access_log.access_time, abuse_access_log.log_id
		LIMIT $3
		FOR UPDATE SKIP LOCKED
	)
`

// One pass of §21.10's ttls at now (see the file header), until each table has no row left past its ttl or
// RetentionPassMaxTime has passed. Reads HeldNetworkIds once at its start; a read that fails raises, and the pass
// deletes nothing. A binary that does not link MG5 does nothing.
func RemoveExpiredAbuseRows(ctx context.Context, now time.Time, settings *AbuseSettings) *AbuseRetentionResult {
	result := &AbuseRetentionResult{}
	if !server.SidecarMg5Linked() {
		return result
	}
	heldIds := abuseRetentionHeldIds(HeldNetworkIds(ctx, now))
	caseCutoff, legalCutoff := abuseRetentionCutoffs(now, settings)
	deadline := time.Now().Add(settings.RetentionPassMaxTime)
	batchSize := max(settings.RetentionBatchSize, 1)

	caseCount, more := removeRetentionBatches(ctx, RetentionTableAbuseCase, batchSize, deadline, func(tx server.PgTx) int64 {
		var cases, actions, appeals int64
		server.Raise(tx.QueryRow(ctx, removeExpiredAbuseCasesSql, caseCutoff, heldIds, batchSize).Scan(&cases, &actions, &appeals))
		countRetentionDeletedRows(RetentionTableAbuseAction, actions)
		countRetentionDeletedRows(RetentionTableAbuseAppeal, appeals)
		result.ActionCount += actions
		result.AppealCount += appeals
		return cases
	})
	result.CaseCount = caseCount
	result.More = more

	legalCount, more := removeRetentionBatches(ctx, RetentionTableLegalRequest, batchSize, deadline, func(tx server.PgTx) int64 {
		tag := server.RaisePgResult(tx.Exec(ctx, removeExpiredLegalRequestsSql, legalCutoff, heldIds, batchSize))
		return tag.RowsAffected()
	})
	result.LegalRequestCount = legalCount
	result.More = result.More || more

	accessCount, more := removeRetentionBatches(ctx, RetentionTableAbuseAccessLog, batchSize, deadline, func(tx server.PgTx) int64 {
		tag := server.RaisePgResult(tx.Exec(ctx, removeExpiredAbuseAccessLogSql, legalCutoff, heldIds, batchSize))
		return tag.RowsAffected()
	})
	result.AccessLogCount = accessCount
	result.More = result.More || more
	return result
}

// ----- a network's deletion under a hold -----

// The in-scope rows model.RemoveNetwork deletes, each table with the columns a hold keeps (api-1, api-5,
// network_acl_head; QD-008): every credential left out, a password's hash and salt, an SSO token, a seed phrase's
// lookup, hash and salt, so a hold keeps who and when and never a way in.
type holdDeletionTable struct {
	table string
	// the column the deletion matches: the network, or a user it removes
	keyColumn string
	byUser    bool
	columns   []string
	linked    func() bool
}

func holdDeletionTables() []*holdDeletionTable {
	always := func() bool { return true }
	return []*holdDeletionTable{
		{table: "network", keyColumn: "network_id", columns: []string{"network_id", "network_name", "admin_user_id"}, linked: always},
		{table: "network_user", keyColumn: "user_id", byUser: true, columns: []string{"user_id", "user_name", "auth_type", "user_auth", "verified", "wallet_address", "wallet_blockchain"}, linked: always},
		{table: "network_user_auth_password", keyColumn: "user_id", byUser: true, columns: []string{"user_id", "user_auth", "auth_type", "verified", "create_time"}, linked: always},
		{table: "network_user_auth_sso", keyColumn: "user_id", byUser: true, columns: []string{"user_id", "auth_type", "user_auth", "create_time"}, linked: always},
		{table: "network_user_auth_wallet", keyColumn: "user_id", byUser: true, columns: []string{"user_id", "wallet_address", "blockchain", "create_time"}, linked: always},
		{table: "network_user_auth_seedphrase", keyColumn: "user_id", byUser: true, columns: []string{"user_id", "create_time"}, linked: always},
		{table: "sidecar_identity", keyColumn: "network_id", columns: []string{"sidecar_id", "network_id", "identity_public_key", "identity_alg", "status", "create_time", "revoke_time"}, linked: server.SidecarMg1Linked},
		{table: "network_acl_head", keyColumn: "network_id", columns: []string{"network_id", "version", "chain", "has_document"}, linked: server.SidecarMg2Linked},
	}
}

// A part kind: rows a network's own deletion removed under a hold.
const HoldCopyPartKindDeletion = "deletion"

// Copies the in-scope rows a network's deletion is about to remove into every live hold of the network, in the
// deletion's transaction (QD-008's silent deferral): one part per hold and table that has rows, each after the
// hold's highest part. Nothing for a network no hold covers at now. Writes no hint, admin event or notice.
func copyHeldNetworkRowsBeforeRemovalInTx(ctx context.Context, tx server.PgTx, networkId server.Id, userIds []server.Id, now time.Time) {
	if !server.SidecarMg5Linked() {
		return
	}
	holds := []*LegalHold{}
	result, err := tx.Query(
		ctx,
		`
			SELECT `+legalHoldColumns+`
			FROM abuse_legal_hold
			WHERE
				abuse_legal_hold.network_id = $1 AND
				abuse_legal_hold.part = 0 AND
				(abuse_legal_hold.expire_time IS NULL OR $2 < abuse_legal_hold.expire_time)
			ORDER BY abuse_legal_hold.case_id
			FOR UPDATE
		`,
		networkId,
		now.UTC(),
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			holds = append(holds, scanLegalHold(result))
		}
	})
	if len(holds) == 0 {
		return
	}
	settings := DefaultAbuseHoldCopySettings()
	type tableRows struct {
		table *holdDeletionTable
		lines [][]byte
	}
	tablesRows := []*tableRows{}
	for _, table := range holdDeletionTables() {
		if !table.linked() {
			continue
		}
		keys := []server.Id{networkId}
		if table.byUser {
			keys = userIds
		}
		selected := []string{}
		for _, column := range table.columns {
			selected = append(selected, fmt.Sprintf("deleted.%s::text", pgx.Identifier{column}.Sanitize()))
		}
		rows := &tableRows{table: table, lines: [][]byte{}}
		result, err := tx.Query(
			ctx,
			fmt.Sprintf(
				`SELECT %s FROM public.%s deleted WHERE deleted.%s = ANY($1) ORDER BY 1`,
				strings.Join(selected, ", "),
				pgx.Identifier{table.table}.Sanitize(),
				pgx.Identifier{table.keyColumn}.Sanitize(),
			),
			keys,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				values := make([]*string, len(table.columns))
				targets := make([]any, len(values))
				for i := range values {
					targets[i] = &values[i]
				}
				server.Raise(result.Scan(targets...))
				line, err := json.Marshal(values)
				server.Raise(err)
				rows.lines = append(rows.lines, line)
			}
		})
		if 0 < len(rows.lines) {
			tablesRows = append(tablesRows, rows)
		}
	}
	for _, hold := range holds {
		var maxPart int
		server.Raise(tx.QueryRow(
			ctx,
			`SELECT COALESCE(MAX(part), 0) FROM abuse_legal_hold WHERE network_id = $1 AND case_id = $2`,
			hold.NetworkId,
			hold.CaseId,
		).Scan(&maxPart))
		for _, rows := range tablesRows {
			body, err := encodeHoldCopyPart(&HoldCopyPartHeader{
				Version:  holdCopyPartVersion,
				CaseId:   hold.CaseId,
				Kind:     HoldCopyPartKindDeletion,
				Table:    rows.table.table,
				Columns:  slices.Clone(rows.table.columns),
				RowCount: len(rows.lines),
			}, rows.lines, settings)
			server.Raise(err)
			maxPart += 1
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO abuse_legal_hold (network_id, case_id, part, copied_rows) VALUES ($1, $2, $3, $4)`,
				hold.NetworkId,
				hold.CaseId,
				maxPart,
				body,
			))
		}
	}
}
