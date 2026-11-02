// Close evidence retention at the contract horizon (owner, 2026-11-01).
//
// A close report's evidence was kept 91 days past its acceptance (migration
// 857, ContractRetention), while the contract horizon reaper
// (model/contract_horizon_reaper.go) can delete the contract itself as soon as
// its runtime control's horizon, down to 35 days. Evidence now follows that
// horizon: a report is deleted once it was accepted longer ago than the
// contract horizon, never less than CloseEvidenceRetentionFloor and never more
// than ContractRetention, and only when its contract is resolved or gone
// (RemoveExpiredCloseEvidence; once the evidence is partitioned, a whole week
// drops on the same rule). "Gone" covers the evidence of every contract the
// reaper deleted: the reaper deletes a contract only once it is resolved and
// its close time is past the horizon, and admission accepts a report only
// before its contract resolves, so the deleted contract's evidence is past the
// horizon too.
//
// The deletes do not apply the reaper's keep rules. A resolved contract the
// reaper keeps, for a payment hold, operator block points or an unsettled
// escrow, loses its evidence at the horizon all the same. With the reaper's
// mode on at horizonDays 35 the deletes are permanent and large: about a
// billion rows, the evidence accepted from 2026-10-06 on, as each day of it
// passes 35 days, from about 2026-11-10. The row deletes are paced and stopped
// by their own brake (db_contract_close_evidence_retention.go), not the
// reaper's. The weekly drops never read that brake, and follow the reaper's
// mode.
//
// The readers of close evidence, and why the horizon keeps what each needs:
//   - close report admission (model/contract_close_report.go) reads the
//     retained report first: an exact retry is answered from it, and a reused
//     report id is refused. A report is written only while its contract is
//     unresolved, and the evidence of an unresolved contract is never deleted,
//     at any age. After the deletion an exact retry is answered from the
//     contract's close report, its receipt, while the contract stays, and is
//     refused only once the contract is gone, as after 91 days before.
//   - the SN epoch census (model/st_provider_close_originals.go) reads the
//     reports of the contracts that closed in an epoch window, and only for a
//     window of at most payoutartifact.MaxClosedWorkRecords contracts.
//     Production windows hold hundreds of millions, so it reads none there. A
//     small window older than the horizon has lost its contracts to the
//     reaper as well, and a recorded epoch rebuilds from its published chunks.
//   - settlement, disputes, payout planning and reconciliation, and the
//     provider and transfer stats read the contract tables, the payments and
//     the archive, never the evidence. A disputed contract has no outcome, so
//     its evidence stays.
//
// The tasks that delete or drop evidence read the horizon from the reaper's
// control once per run, before any transaction, and carry it in their context
// (WithContractHorizon, through model.WithContractHorizonControl), so no Redis
// read ever happens while a drop holds its locks. The horizon applies only
// while the reaper's mode is on: a run that finds it off or in a dry run, or
// cannot read the control, keeps ContractRetention, so setting the mode off
// stops the evidence deletes below 91 days from the next run too. The row
// deletes' own brake stops them without touching the reaper.
//
// The installed evidence guard bounds the retention as well
// (closeEvidenceRetentionIn): until the horizon guard migration is applied,
// or after its body is replaced again, the guard admits only rows older than
// 91 days, so the row deletes and the weekly drops keep 91 days. A drop
// bypasses the row guard, so without that bound a release ahead of its
// migration would drop weeks the database's guard still keeps. The code may
// therefore deploy before or after the migration.
//
// A census of a small window (above) can still miss reports in one case: a
// contract that stays unresolved longer than the horizon keeps its reports,
// but once it resolves the ones accepted before the horizon go at the next
// pass, while the epoch it closed in may not have been read yet. No production
// window is small enough to read reports at all.
package server

import (
	"context"
	"time"
)

type contractHorizonContextKey struct{}

// ctx carrying the contract horizon: how long past its creation and its close
// a contract is kept, as the contract horizon reaper's runtime control sets it.
// Close evidence retention follows it (CloseEvidenceRetention).
func WithContractHorizon(ctx context.Context, horizon time.Duration) context.Context {
	return context.WithValue(ctx, contractHorizonContextKey{}, horizon)
}

// How long close evidence is kept past its acceptance: ctx's contract horizon,
// between CloseEvidenceRetentionFloor and ContractRetention. Without a horizon
// in ctx, ContractRetention, the 13 weeks evidence was kept before the horizon
// could shorten (migration 857).
func CloseEvidenceRetention(ctx context.Context) time.Duration {
	horizon, ok := ctx.Value(contractHorizonContextKey{}).(time.Duration)
	if !ok {
		return ContractRetention
	}
	return min(max(horizon, CloseEvidenceRetentionFloor), ContractRetention)
}

// The age from which the installed evidence guard admits a retention delete:
// CloseEvidenceRetentionFloor when its body is the horizon guard's, and
// ContractRetention for any other body (857's, the move guard's, or one an
// operator restored to stop the deletes). Read through conn, which may be the
// drop's own transaction.
func closeEvidenceGuardFloor(ctx context.Context, conn PgQuerier) time.Duration {
	var body string
	err := conn.QueryRow(
		ctx,
		`SELECT coalesce((SELECT prosrc FROM pg_proc WHERE oid = to_regprocedure('public.contract_close_report_evidence_guard()')), '')`,
	).Scan(&body)
	return closeEvidenceGuardFloorOf(body, err)
}

// The guard floor of a read of the installed body: the horizon guard's body
// admits CloseEvidenceRetentionFloor. A read that failed, a missing function
// (an empty body) and any other body admit ContractRetention.
func closeEvidenceGuardFloorOf(body string, readErr error) time.Duration {
	if readErr == nil && body == ContractCloseEvidenceHorizonGuardBodySql() {
		return CloseEvidenceRetentionFloor
	}
	return ContractRetention
}

// The floor the installed evidence guard admits (closeEvidenceGuardFloor),
// and whether that is the horizon guard migration's floor. For operator
// tools.
func GetCloseEvidenceRetentionGuardFloor(ctx context.Context) (floor time.Duration, horizonGuard bool) {
	MaintenanceDb(ctx, func(conn PgConn) {
		floor = closeEvidenceGuardFloor(ctx, conn)
	}, OptReadOnly())
	return floor, floor == CloseEvidenceRetentionFloor
}

// The retention the row deletes and the drops apply: ctx's
// (CloseEvidenceRetention), and never less than the installed guard admits.
func closeEvidenceRetentionIn(ctx context.Context, conn PgQuerier) time.Duration {
	return max(CloseEvidenceRetention(ctx), closeEvidenceGuardFloor(ctx, conn))
}

// A retention as whole seconds, for an interval literal.
func closeEvidenceRetentionSeconds(retention time.Duration) int64 {
	return int64(retention / time.Second)
}
