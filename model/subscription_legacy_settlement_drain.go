package model

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

const LegacySettlementDrainMaxContracts = 32
const legacySettlementDrainBudget = 15 * time.Second

type LegacySettlementDrainRequest struct {
	ExpectedPayerNetworkId server.Id   `json:"expected_payer_network_id"`
	ContractIds            []server.Id `json:"contract_ids"`
	Apply                  bool        `json:"apply"`
}

// Only an acknowledged financial commit sets FinancialCommitAcknowledged.
// A false value does not resolve an ambiguous commit or another worker's work.
// RunPosts has no ACK result: its return never proves a Redis mirror update.
type LegacySettlementDrainContract struct {
	ContractId                  server.Id `json:"contract_id"`
	Status                      string    `json:"status"`
	FinancialCommitAcknowledged bool      `json:"financial_commit_acknowledged"`
	PostProcessing              string    `json:"post_processing"`
	Mirror                      string    `json:"mirror"`
}

type LegacySettlementDrainResult struct {
	Trace     *LegacySettlementTrace          `json:"trace,omitempty"`
	Apply     bool                            `json:"apply"`
	Contracts []LegacySettlementDrainContract `json:"contracts"`
}

type legacySettlementDrainRefusal string

func (err legacySettlementDrainRefusal) Error() string { return string(err) }

// Apply follows the ordinary owner's intent -> contract -> sorted grants order.
// Both preflight owners remain locked while the unchanged owner reenters them.
// Preview is only a consistent custody observation, not accounting admission.
func checkLegacySettlementDrainInTx(ctx context.Context, tx server.PgTx, id, payer server.Id, lock bool) error {
	defer enterLegacyTargetTrace(ctx, "point_preflight")()
	lockSQL := ""
	if lock {
		lockSQL = " FOR UPDATE SKIP LOCKED"
	}
	var intentOutcome ContractOutcome
	var clearDispute, due bool
	var failure string
	err := tx.QueryRow(ctx, `SELECT outcome,clear_dispute,failure_code,
		next_attempt_time<=statement_timestamp() AT TIME ZONE 'UTC'
		FROM legacy_settlement_intent WHERE contract_id=$1`+lockSQL, id).Scan(&intentOutcome, &clearDispute, &failure, &due)
	if errors.Is(err, pgx.ErrNoRows) {
		if lock {
			return legacySettlementDrainRefusal("busy_intent_or_absent")
		}
		return legacySettlementDrainRefusal("intent_absent")
	}
	if err != nil {
		return err
	}
	var actualPayer *server.Id
	var outcome *ContractOutcome
	var disputed bool
	err = tx.QueryRow(ctx, `SELECT payer_network_id,outcome,dispute FROM transfer_contract WHERE contract_id=$1`+lockSQL, id).Scan(&actualPayer, &outcome, &disputed)
	if errors.Is(err, pgx.ErrNoRows) {
		if lock {
			return legacySettlementDrainRefusal("busy_contract_or_absent")
		}
		return legacySettlementDrainRefusal("contract_absent")
	}
	if err != nil {
		return err
	}
	if actualPayer == nil || *actualPayer != payer {
		return legacySettlementDrainRefusal("payer_mismatch")
	}
	if outcome != nil {
		return legacySettlementDrainRefusal("terminal")
	}
	if disputed {
		return legacySettlementDrainRefusal("disputed")
	}
	if failure != "none" {
		return legacySettlementDrainRefusal("held")
	}
	if !due {
		return legacySettlementDrainRefusal("not_due")
	}
	if intentOutcome != ContractOutcomeSettled || clearDispute {
		return legacySettlementDrainRefusal("intent_policy_changed")
	}
	var bothFinal, legacy, redis, debit bool
	err = tx.QueryRow(ctx, `SELECT
		(SELECT count(*)=2 AND COALESCE(bool_and(party IN ('source','destination') AND NOT checkpoint),false) FROM contract_close WHERE contract_id=$1),
		EXISTS(SELECT 1 FROM transfer_escrow WHERE contract_id=$1 AND NOT redis_reserved),
		EXISTS(SELECT 1 FROM transfer_escrow WHERE contract_id=$1 AND redis_reserved),
		EXISTS(SELECT 1 FROM transfer_debit_journal WHERE contract_id=$1)`, id).Scan(&bothFinal, &legacy, &redis, &debit)
	if err != nil {
		return err
	}
	if !bothFinal {
		return legacySettlementDrainRefusal("reports_changed")
	}
	if debit {
		return legacySettlementDrainRefusal("debit_present")
	}
	if !legacy || redis {
		return legacySettlementDrainRefusal("reservation_mode_changed")
	}
	return nil
}

// The caller must raise every returned error inside its transaction. This keeps
// the original intent deletion, debit, payout, proof, metadata and projection
// owner atomic, including failures after the legacy owner deletes the intent.
func drainLegacySettlementInTx(ctx context.Context, tx server.PgTx, id, payer server.Id) ([]func() any, bool, bool, legacySettlementBusyGate, error) {
	if err := checkLegacySettlementDrainInTx(ctx, tx, id, payer, true); err != nil {
		return nil, false, false, legacySettlementBusyNone, err
	}
	return flushLegacySettlementWithExpiryPolicyInTx(ctx, tx, id, nil, false)
}

func legacySettlementDrainErrorStatus(err error) string {
	var refusal legacySettlementDrainRefusal
	if errors.As(err, &refusal) {
		return string(refusal)
	}
	if errors.Is(err, errContractInsufficientEscrow) {
		return "accounting_refused"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline"
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, server.DbContextDoneError) {
		return "canceled"
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if pgErr.Code == "55P03" {
			return "busy"
		}
		if pgErr.Code == "57014" && pgErr.Message == "canceling statement due to statement timeout" {
			return "statement_timeout"
		}
	}
	return "failed"
}

// DrainLegacySettlements invokes only the established financial owner for a
// finite explicit scope. It neither changes reports nor prepares expiry proof,
// clears holds, writes retry state, selects global pages or bypasses accounting.
// Existing detached commit/rollback and mirror cleanup bounds remain in force.
func DrainLegacySettlements(ctx context.Context, request LegacySettlementDrainRequest) (result LegacySettlementDrainResult, returnErr error) {
	if err := validateContractExpiryRepair(ContractExpiryRepairRequest{ExpectedPayerNetworkId: request.ExpectedPayerNetworkId, ContractIds: request.ContractIds}); err != nil {
		return result, errors.New("invalid legacy settlement drain scope")
	}
	var trace *legacyTargetTrace
	if len(request.ContractIds) == 1 {
		id := request.ContractIds[0]
		origin := "explicit_preview"
		if request.Apply {
			origin = "explicit_apply"
		}
		trace, _ = ctx.Value(legacyTargetTraceKey{}).(*legacyTargetTrace)
		if trace == nil {
			trace = legacyTargetTraceRuntimeState.begin(int(id[15])%LegacySettlementShardCount, nil, origin, &id)
		}
		if trace != nil {
			ctx = trace.selectTarget(ctx, id, false)
		}
	}
	defer func() { result.Trace = trace.finish(LegacySettlementFlushResult{}, returnErr) }()
	bounded, cancel := context.WithTimeout(ctx, legacySettlementDrainBudget)
	defer cancel()
	result.Apply = request.Apply
	result.Contracts = make([]LegacySettlementDrainContract, len(request.ContractIds))
	for i, id := range request.ContractIds {
		result.Contracts[i] = LegacySettlementDrainContract{ContractId: id, Status: "not_attempted", PostProcessing: "not_started", Mirror: "not_verified"}
	}
	for i, id := range request.ContractIds {
		if bounded.Err() != nil {
			return result, errors.New("legacy settlement drain budget stopped")
		}
		entry := &result.Contracts[i]
		err := captureContractExpiryRepair(func() error {
			dbTiming, finishDatabaseTrace := legacyTargetTraceDatabase(bounded)
			defer finishDatabaseTrace()
			if !request.Apply {
				server.Tx(bounded, func(tx server.PgTx) {
					configureContractExpiryRepairTx(bounded, tx)
					server.Raise(checkLegacySettlementDrainInTx(bounded, tx, id, request.ExpectedPayerNetworkId, false))
				}, pgx.RepeatableRead, pgx.ReadOnly, server.OptNoRetry(), dbTiming)
				traceLegacySettlement(bounded, "preview_commit", "confirmed_tx_return")
				entry.Status = "eligible"
				return nil
			}
			var posts []func() any
			var completed, busy bool
			var gate legacySettlementBusyGate
			server.Tx(bounded, func(tx server.PgTx) {
				configureContractExpiryRepairTx(bounded, tx)
				var err error
				posts, completed, busy, gate, err = drainLegacySettlementInTx(bounded, tx, id, request.ExpectedPayerNetworkId)
				server.Raise(err)
			}, server.TxReadCommitted, server.OptNoRetry(), dbTiming)
			traceLegacySettlement(bounded, "commit", "confirmed_tx_return")
			traceLegacySettlementResult(bounded, completed, busy, gate, nil)
			if busy {
				switch gate {
				case legacySettlementBusyIntent:
					entry.Status = "busy_intent_or_absent"
				case legacySettlementBusyContract:
					entry.Status = "busy_contract_or_absent"
				case legacySettlementBusyGrantSet:
					entry.Status = "busy_grant_set"
				default:
					entry.Status = "busy"
				}
				return nil
			}
			if !completed {
				entry.Status = "not_completed"
				return nil
			}
			entry.FinancialCommitAcknowledged = true
			entry.Status = "financial_committed"
			if bounded.Err() != nil {
				entry.PostProcessing = "not_started_canceled"
				return nil
			}
			entry.PostProcessing = "started_unverified"
			func() {
				defer enterLegacyTargetTrace(bounded, "joined_posts")()
				server.RunPosts(bounded, posts...)
			}()
			entry.PostProcessing = "returned_unverified"
			return nil
		})
		if err != nil {
			traceLegacySettlementResult(bounded, false, false, legacySettlementBusyNone, err)
			if entry.FinancialCommitAcknowledged {
				entry.Status = "financial_committed_post_interrupted"
			} else {
				entry.Status = legacySettlementDrainErrorStatus(err)
			}
		}
		if bounded.Err() != nil {
			return result, errors.New("legacy settlement drain budget stopped")
		}
	}
	return result, nil
}
