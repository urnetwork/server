// Close evidence retention at its own window (owner, 2026-11-23: 7 days).
//
// A close report's evidence was kept 91 days past its acceptance (migration
// 857, ContractRetention), then followed the contract horizon reaper's
// horizon, down to 35 days (owner, 2026-11-01; migration 913). Since
// 2026-11-23 it keeps its own window instead, CloseEvidenceRetentionWindow,
// 7 days, decoupled from the contract horizon (DBDISK-RETENTION-REVIEW.md 4.5
// A, CONTRACT-PARTITION-CUTOVER-NOW.md decision 4): a report is deleted once
// it was accepted longer ago than the window, never less than
// CloseEvidenceRetentionFloor and never more than ContractRetention, and only
// when its contract is resolved or gone (RemoveExpiredCloseEvidence; once the
// evidence is partitioned, a whole week drops on the same rule). The window is
// no longer than any contract horizon the reaper may run (14 days at the
// least), so the evidence of a contract the reaper deleted never outlives it
// by more than the window, and the reaper's keep rules do not apply here: a
// resolved contract the reaper keeps, for a payment hold, operator block
// points or an unsettled escrow, loses its evidence at the window all the
// same.
//
// The readers of close evidence, and why the window keeps what each needs
// (DBDISK-RETENTION-REVIEW.md 4.1):
//   - close report admission (model/contract_close_report.go) reads the
//     retained report first: an exact retry is answered from it, and a reused
//     report id is refused. Retries come within hours. A report is written
//     only while its contract is unresolved, and the evidence of an
//     unresolved contract is never deleted, at any age. After the deletion an
//     exact retry is answered from the contract's close report, its receipt,
//     while the contract stays, and is refused only once the contract is gone.
//   - the zero-carry check (model/provider_zero_carry.go) reads the final
//     source reports of contracts closed in the last 24 hours.
//   - the SN epoch census (model/st_provider_close_originals.go) reads the
//     reports of the contracts that closed in an epoch window, and only for a
//     window of at most payoutartifact.MaxClosedWorkRecords contracts.
//     Production windows hold hundreds of millions, so it reads none there.
//   - settlement, disputes, payout planning and reconciliation, and the
//     provider and transfer stats read the contract tables, the payments and
//     the archive, never the evidence. A disputed contract has no outcome, so
//     its evidence stays.
//
// The tasks that delete or drop evidence read the evidence brake
// ({close_evidence_retention}control) once per run, before any transaction,
// and carry the window in their context only while the brake is on
// (WithCloseEvidenceRetentionControl). A run that finds the brake off, or
// cannot read it, keeps ContractRetention, so setting the brake's mode off
// stops the row deletes from their next batch and the weekly drops below 91
// days from the next run. The contract horizon reaper's control no longer
// moves the evidence: its mode, rate and horizon reach the contracts alone.
//
// The installed evidence guard bounds the retention as well
// (closeEvidenceRetentionIn): until the window guard migration is applied the
// horizon guard admits only rows older than 35 days, and any other body only
// rows older than 91 days, so the row deletes and the weekly drops keep that
// long. A drop bypasses the row guard, so without that bound a release ahead
// of its migration would drop weeks the database's guard still keeps. The
// code may therefore deploy before or after the migration.
//
// A census of a small window (above) can still miss reports in one case: a
// contract that stays unresolved longer than the window keeps its reports,
// but once it resolves the ones accepted before the window go at the next
// pass, while the epoch it closed in may not have been read yet. No production
// window is small enough to read reports at all.
package server

import (
	"context"
	"time"

	"github.com/urnetwork/glog"
)

// How long close evidence is kept past its acceptance while the evidence
// brake is on (owner, 2026-11-23). Admission retries need hours and the
// zero-carry check a day; the rest is margin.
const CloseEvidenceRetentionWindow = 7 * 24 * time.Hour

type contractHorizonContextKey struct{}

// ctx carrying the contract horizon: how long past its creation and its close
// a contract is kept, as the contract horizon reaper's runtime control sets it.
// The retirement of the contract days (ContractDayRetention) and the provider
// work session journal (ProviderWorkSessionEventRetentionAt) follow it; close
// evidence does not (WithCloseEvidenceWindow).
func WithContractHorizon(ctx context.Context, horizon time.Duration) context.Context {
	return context.WithValue(ctx, contractHorizonContextKey{}, horizon)
}

// The contract horizon ctx carries, set only while the reaper's mode is on.
func contractHorizonIn(ctx context.Context) (horizon time.Duration, ok bool) {
	horizon, ok = ctx.Value(contractHorizonContextKey{}).(time.Duration)
	return
}

type closeEvidenceWindowContextKey struct{}

// ctx carrying the close evidence window: how long past its acceptance a
// report's evidence is kept (CloseEvidenceRetention). The evidence tasks set
// CloseEvidenceRetentionWindow through WithCloseEvidenceRetentionControl;
// tests set others.
func WithCloseEvidenceWindow(ctx context.Context, window time.Duration) context.Context {
	return context.WithValue(ctx, closeEvidenceWindowContextKey{}, window)
}

// ctx carrying CloseEvidenceRetentionWindow while the evidence brake is on, as
// one bounded Redis read finds it now. With the brake off, or unread, ctx is
// returned as is, so the evidence keeps ContractRetention: the brake stops the
// weekly drops below 91 days too, from the next run. Call it once per task
// run, before any transaction.
func WithCloseEvidenceRetentionControl(ctx context.Context) context.Context {
	brake, err := GetCloseEvidenceRetentionControl(ctx, DefaultCloseEvidenceRetentionSettings())
	if err != nil {
		glog.V(1).Infof("[cp]close evidence retention control unread, close evidence keeps %s: %s\n", ContractRetention, err)
		return ctx
	}
	if !brake.On {
		return ctx
	}
	return WithCloseEvidenceWindow(ctx, CloseEvidenceRetentionWindow)
}

// How long close evidence is kept past its acceptance: ctx's window, between
// CloseEvidenceRetentionFloor and ContractRetention. Without a window in ctx,
// ContractRetention, the 13 weeks evidence was kept before it could shorten
// (migration 857). The contract horizon in ctx does not move it.
func CloseEvidenceRetention(ctx context.Context) time.Duration {
	window, ok := ctx.Value(closeEvidenceWindowContextKey{}).(time.Duration)
	if !ok {
		return ContractRetention
	}
	return min(max(window, CloseEvidenceRetentionFloor), ContractRetention)
}

// The age from which the installed evidence guard admits a retention delete:
// CloseEvidenceRetentionFloor when its body is the window guard's, the
// horizon guard's 35 days when it is that one's, and ContractRetention for any
// other body (857's, the move guard's, or one an operator restored to stop the
// deletes). Read through conn, which may be the drop's own transaction.
func closeEvidenceGuardFloor(ctx context.Context, conn PgQuerier) time.Duration {
	var body string
	err := conn.QueryRow(
		ctx,
		`SELECT coalesce((SELECT prosrc FROM pg_proc WHERE oid = to_regprocedure('public.contract_close_report_evidence_guard()')), '')`,
	).Scan(&body)
	return closeEvidenceGuardFloorOf(body, err)
}

// The guard floor of a read of the installed body: the window guard's body
// admits CloseEvidenceRetentionFloor, and the horizon guard's 35 days. A read
// that failed, a missing function (an empty body) and any other body admit
// ContractRetention.
func closeEvidenceGuardFloorOf(body string, readErr error) time.Duration {
	if readErr != nil {
		return ContractRetention
	}
	switch body {
	case ContractCloseEvidenceWindowGuardBodySql():
		return CloseEvidenceRetentionFloor
	case ContractCloseEvidenceHorizonGuardBodySql():
		return closeEvidenceHorizonGuardFloor
	default:
		return ContractRetention
	}
}

// The floor the installed evidence guard admits (closeEvidenceGuardFloor),
// and whether that is the window guard migration's floor. For operator tools.
func GetCloseEvidenceRetentionGuardFloor(ctx context.Context) (floor time.Duration, windowGuard bool) {
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
