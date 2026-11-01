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
// The readers of close evidence, and why the horizon keeps what each needs:
//   - close report admission (model/contract_close_report.go) reads a retained
//     report to answer an exact retry and to refuse a reused report id. Its
//     contract is then still open: admission refuses a resolved or missing
//     contract before it writes, and evidence of an unresolved contract is
//     never deleted, at any age. A retry after the deletion meets the resolved
//     or missing contract and is refused, as it is today after 91 days.
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
// read ever happens while a drop holds its locks. A run that cannot read the
// control keeps ContractRetention.
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

// A retention as whole seconds, for an interval literal.
func closeEvidenceRetentionSeconds(retention time.Duration) int64 {
	return int64(retention / time.Second)
}
