package model

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

const ContractExpiryRepairMaxContracts = 32
const contractExpiryRepairBudget = 15 * time.Second

// ContractExpiryRepairRequest is an explicit operator-selected scope. Payer
// ownership is checked again in every transaction; a preview is not an apply token.
type ContractExpiryRepairRequest struct {
	ExpectedPayerNetworkId server.Id   `json:"expected_payer_network_id"`
	ContractIds            []server.Id `json:"contract_ids"`
	Apply                  bool        `json:"apply"`
}

// Results contain private contract identities, but no balances, report amounts,
// credentials or raw errors. An observed intent is delegated work, not settlement.
type ContractExpiryRepairContract struct {
	ContractId          server.Id        `json:"contract_id"`
	Status              string           `json:"status"`
	ProofCommitted      bool             `json:"proof_committed"`
	ObservedOutcome     *ContractOutcome `json:"observed_outcome"`
	LegacyIntentPresent *bool            `json:"legacy_intent_present"`
}

type ContractExpiryRepairResult struct {
	Apply     bool                           `json:"apply"`
	Cutoff    time.Time                      `json:"cutoff"`
	Contracts []ContractExpiryRepairContract `json:"contracts"`
}

type contractExpiryRepairRefusal string

func (r contractExpiryRepairRefusal) Error() string { return string(r) }

type contractExpiryRepairScope struct {
	contractId    server.Id
	expectedPayer server.Id
	continuation  *contractExpiryRepairState
	redis         *contractRedisExpiryRepairScope
	// Tests may change custody between real committed transaction boundaries.
	// Production never supplies this callback.
	beforeTxForTest func()
	// Simulates lost acknowledgement after a real commit, before authority moves.
	afterCommitForTest func()
}

type contractExpiryRepairReport struct {
	count      ByteCount
	checkpoint bool
	time       time.Time
}

// The explicit repair withdraws if an outside report or proof changes between
// its acknowledged commits. Its own finalized reports advance this snapshot.
type contractExpiryRepairState struct {
	source, destination server.Id
	unverified          bool
	proof               []byte
	reports             map[ContractParty]contractExpiryRepairReport
}

func readContractExpiryRepairState(ctx context.Context, tx server.PgTx, id server.Id) *contractExpiryRepairState {
	state := &contractExpiryRepairState{reports: map[ContractParty]contractExpiryRepairReport{}}
	server.Raise(tx.QueryRow(ctx, `SELECT source_id,destination_id,usage_unverified,provider_usage FROM transfer_contract WHERE contract_id=$1`, id).Scan(&state.source, &state.destination, &state.unverified, &state.proof))
	rows, err := tx.Query(ctx, `SELECT party,used_transfer_byte_count,checkpoint,close_time FROM contract_close WHERE contract_id=$1`, id)
	server.WithPgResult(rows, err, func() {
		for rows.Next() {
			var party ContractParty
			var report contractExpiryRepairReport
			server.Raise(rows.Scan(&party, &report.count, &report.checkpoint, &report.time))
			state.reports[party] = report
		}
	})
	return state
}

func (state *contractExpiryRepairState) equals(other *contractExpiryRepairState) bool {
	if state.source != other.source || state.destination != other.destination || state.unverified != other.unverified || !bytes.Equal(state.proof, other.proof) || len(state.reports) != len(other.reports) {
		return false
	}
	for party, report := range state.reports {
		peer, ok := other.reports[party]
		if !ok || report.count != peer.count || report.checkpoint != peer.checkpoint || !report.time.Equal(peer.time) {
			return false
		}
	}
	return true
}

func validateContractExpiryRepair(request ContractExpiryRepairRequest) error {
	if request.ExpectedPayerNetworkId == (server.Id{}) || len(request.ContractIds) == 0 || len(request.ContractIds) > ContractExpiryRepairMaxContracts {
		return errors.New("invalid contract expiry repair scope")
	}
	seen := map[server.Id]bool{}
	for _, id := range request.ContractIds {
		if id == (server.Id{}) || seen[id] {
			return errors.New("invalid contract expiry repair scope")
		}
		seen[id] = true
	}
	return nil
}

func configureContractExpiryRepairTx(ctx context.Context, tx server.PgTx) {
	server.RaisePgResult(tx.Exec(ctx, `SET LOCAL statement_timeout='2s'; SET LOCAL lock_timeout='250ms'`))
}

// The applying caller holds the contract before checking intent absence. Intent
// producers also own that contract, so they cannot enter between the check and
// this transaction's writes. Do not wait for an intent while holding the contract.
func (scope *contractExpiryRepairScope) checkInTx(ctx context.Context, tx server.PgTx, contractId server.Id, lock bool) error {
	if contractId != scope.contractId {
		return contractExpiryRepairRefusal("scope_mismatch")
	}
	query := `SELECT payer_network_id,outcome,dispute FROM transfer_contract WHERE contract_id=$1`
	if lock {
		query += ` FOR UPDATE`
	}
	var payer *server.Id
	var outcome *ContractOutcome
	var dispute bool
	if err := tx.QueryRow(ctx, query, contractId).Scan(&payer, &outcome, &dispute); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return contractExpiryRepairRefusal("missing")
		}
		return err
	}
	if payer == nil || *payer != scope.expectedPayer {
		return contractExpiryRepairRefusal("payer_mismatch")
	}
	if outcome != nil {
		return contractExpiryRepairRefusal("terminal")
	}
	if dispute {
		return contractExpiryRepairRefusal("disputed")
	}
	if scope.redis != nil {
		return scope.checkRedisInTx(ctx, tx, contractId, lock)
	}
	var intent, legacy, redis, debit bool
	if err := tx.QueryRow(ctx, `SELECT
		EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=$1),
		EXISTS(SELECT 1 FROM transfer_escrow WHERE contract_id=$1 AND NOT redis_reserved),
		EXISTS(SELECT 1 FROM transfer_escrow WHERE contract_id=$1 AND redis_reserved),
		EXISTS(SELECT 1 FROM transfer_debit_journal WHERE contract_id=$1)`, contractId).Scan(&intent, &legacy, &redis, &debit); err != nil {
		return err
	}
	if intent {
		return contractExpiryRepairRefusal("legacy_intent_present")
	}
	if debit {
		return contractExpiryRepairRefusal("debit_present")
	}
	if !legacy || redis {
		return contractExpiryRepairRefusal("reservation_mode_changed")
	}
	return nil
}

// Nil scope preserves the ordinary request/sweep transaction policy exactly.
// The repair adds only custody admission and execution bounds to the same owners.
func contractExpiryContinuationTx(ctx context.Context, contractId server.Id, scope *contractExpiryRepairScope, callback func(server.PgTx)) {
	if scope == nil {
		server.Tx(ctx, callback, server.TxReadCommitted)
		return
	}
	if scope.beforeTxForTest != nil {
		scope.beforeTxForTest()
	}
	var next *contractExpiryRepairState
	server.Tx(ctx, func(tx server.PgTx) {
		configureContractExpiryRepairTx(ctx, tx)
		server.Raise(scope.checkInTx(ctx, tx, contractId, true))
		if scope.continuation != nil && !scope.continuation.equals(readContractExpiryRepairState(ctx, tx, contractId)) {
			server.Raise(contractExpiryRepairRefusal("continuation_changed"))
		}
		callback(tx)
		next = readContractExpiryRepairState(ctx, tx, contractId)
	}, server.TxReadCommitted, server.OptNoRetry())
	if scope.afterCommitForTest != nil {
		scope.afterCommitForTest()
	}
	scope.continuation = next
	if scope.redis != nil {
		scope.redis.custody = scope.redis.observed
	}
}

// Catch privately: the standard logging recovery may include IDs and financial
// error text. Fixed result classes remain conservative on an ambiguous commit.
func captureContractExpiryRepair(do func() error) (returnErr error) {
	defer func() {
		if r := recover(); r != nil {
			var ok bool
			if returnErr, ok = r.(error); !ok {
				returnErr = errors.New("contract expiry operation failed")
			}
		}
	}()
	return do()
}

func contractExpiryRepairErrorStatus(err error) string {
	if refusal, ok := err.(contractExpiryRepairRefusal); ok {
		return string(refusal)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline"
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, server.DbContextDoneError) {
		return "canceled"
	}
	if pgErr, ok := err.(*pgconn.PgError); ok {
		if pgErr.Code == "55P03" {
			return "busy"
		}
		if pgErr.Code == "57014" && pgErr.Message == "canceling statement due to statement timeout" {
			return "statement_timeout"
		}
	}
	return "failed"
}

func observeContractExpiryRepair(ctx context.Context, scope *contractExpiryRepairScope, result *ContractExpiryRepairContract) error {
	return captureContractExpiryRepair(func() error {
		server.Tx(ctx, func(tx server.PgTx) {
			configureContractExpiryRepairTx(ctx, tx)
			var payer *server.Id
			var outcome *ContractOutcome
			var intent bool
			server.Raise(tx.QueryRow(ctx, `SELECT payer_network_id,outcome,
				EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=$1)
				FROM transfer_contract WHERE contract_id=$1`, scope.contractId).Scan(&payer, &outcome, &intent))
			if payer == nil || *payer != scope.expectedPayer {
				server.Raise(contractExpiryRepairRefusal("payer_mismatch"))
			}
			result.ObservedOutcome, result.LegacyIntentPresent = outcome, &intent
		}, server.TxReadCommitted, pgx.ReadOnly, server.OptNoRetry())
		return nil
	})
}

// RepairContractExpiry previews or continues only the selected quiet legacy
// contracts. The five-minute cutoff is the deployed ordinary expiry policy.
// Apply retains original report proof before invoking the shared continuation;
// it never quarantines accounting failures, flushes grants or releases custody.
// A deadline may leave a committed proof/report prefix for the ordinary owner.
func RepairContractExpiry(ctx context.Context, request ContractExpiryRepairRequest) (result ContractExpiryRepairResult, returnErr error) {
	return repairContractExpiry(ctx, request, false)
}

// Each entry point chooses its own reservation admission; reports, proof,
// cancellation, and ordinary continuation retain one owner.
func repairContractExpiry(ctx context.Context, request ContractExpiryRepairRequest, redis bool) (result ContractExpiryRepairResult, returnErr error) {
	if err := validateContractExpiryRepair(request); err != nil {
		return result, err
	}
	bounded, cancel := context.WithTimeout(ctx, contractExpiryRepairBudget)
	defer cancel()
	result.Apply, result.Cutoff = request.Apply, server.NowUtc().Add(-5*time.Minute)
	result.Contracts = make([]ContractExpiryRepairContract, len(request.ContractIds))
	for i, id := range request.ContractIds {
		result.Contracts[i] = ContractExpiryRepairContract{ContractId: id, Status: "not_attempted"}
	}
	for i, id := range request.ContractIds {
		if bounded.Err() != nil {
			return result, errors.New("contract expiry repair budget stopped")
		}
		entry := &result.Contracts[i]
		scope := &contractExpiryRepairScope{contractId: id, expectedPayer: request.ExpectedPayerNetworkId}
		if redis {
			scope.redis = &contractRedisExpiryRepairScope{cutoff: result.Cutoff}
		}
		var fresh *contractExpiryState
		err := captureContractExpiryRepair(func() error {
			if request.Apply {
				contractExpiryContinuationTx(bounded, id, scope, func(tx server.PgTx) {
					var err error
					fresh, err = prepareContractExpiryInTx(bounded, tx, id, result.Cutoff)
					server.Raise(err)
				})
				entry.ProofCommitted = fresh != nil
			} else {
				server.Tx(bounded, func(tx server.PgTx) {
					configureContractExpiryRepairTx(bounded, tx)
					server.Raise(scope.checkInTx(bounded, tx, id, false))
					var err error
					fresh, err = inspectContractExpiryInTx(bounded, tx, id, result.Cutoff, false)
					server.Raise(err)
				}, pgx.RepeatableRead, pgx.ReadOnly, server.OptNoRetry())
			}
			if fresh == nil {
				entry.Status = "recent_report"
				return nil
			}
			entry.Status = "eligible"
			if request.Apply {
				if err := continueContractExpiry(bounded, "[scoped-expiry]", fresh, scope); err != nil {
					return err
				}
				entry.Status = "continuation_returned"
			}
			return nil
		})
		if err != nil {
			entry.Status = contractExpiryRepairErrorStatus(err)
		}
		if bounded.Err() != nil {
			if err == nil {
				entry.Status = contractExpiryRepairErrorStatus(bounded.Err())
			}
			return result, errors.New("contract expiry repair budget stopped")
		}
		if bounded.Err() == nil {
			observationErr := observeContractExpiryRepair(bounded, scope, entry)
			if observationErr != nil && err == nil {
				entry.Status = "observation_failed"
			} else if entry.Status == "continuation_returned" {
				switch {
				case entry.ObservedOutcome != nil:
					entry.Status = "terminal"
				case entry.LegacyIntentPresent != nil && *entry.LegacyIntentPresent:
					entry.Status = "legacy_intent_present"
				default:
					entry.Status = "open"
				}
			}
		}
	}
	return result, nil
}
