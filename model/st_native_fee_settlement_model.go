// Settlement consumes only the owned native verifier result. Original signed
// reservations remain immutable; a finalized nonce is charged its actual fee
// once while every unresolved nonce keeps its maximum candidate ceiling.
package model

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"reflect"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urfoundation/sn/nativefee"
	"github.com/urnetwork/server"
)

var ErrStNativeFeeConflict = errors.New("authenticated native fee outcome conflicts; original liability is held")

// The maximum remains available separately from the actual cumulative charge.
// A fee above its ceiling is retained as an overspend, never rounded down.
type StTransactionNativeFeeSettlement struct {
	IntentId           server.Id
	Attempt            int
	TransactionHash    string
	PolicySha256       string
	StatementSha256    string
	DebitRao           string
	DebitWei           string
	OriginalCeilingWei string
	ExceedsCeiling     bool
	Reused             bool
}

// A public caller cannot manufacture a Verified value by decoding JSON. Its
// original invocation and independent authority are rechecked before the
// private database reducer receives any amount or receipt identity.
func SettleStTransactionNativeFee(ctx context.Context, intentId server.Id, policy *server.StNativeFeeDenominationPolicy, authority *server.StNativeFeeDenominationAuthority, verified *nativefee.Verified) (*StTransactionNativeFeeSettlement, error) {
	if ctx == nil {
		return nil, errors.New("native fee settlement has no lifecycle owner")
	}
	policy = policy.Clone()
	if authority != nil {
		original := *authority
		authority = &original
	}
	if err := errors.Join(ctx.Err(), policy.Verify(authority)); err != nil {
		return nil, err
	}
	if verified == nil {
		return nil, errors.New("native fee settlement has no owned verifier result; original ceiling retained")
	}
	if err := verified.Check(ctx, policy.NativeAuthority); err != nil {
		return nil, err
	}
	statement := verified.Facts()
	return settleStTransactionNativeFeeOwned(ctx, intentId, policy, authority, statement, func(ctx context.Context, tx server.PgTx, statementHash string) error {
		return verified.RetainOriginals(ctx, func(kind string, reference nativefee.Reference, reader io.Reader) error {
			return stNativeFeeRetainOriginal(ctx, tx, statementHash, kind, reference, reader)
		})
	})
}

type stNativeFeeOriginalRetention func(context.Context, server.PgTx, string) error

// Once actual verification has observed a contradiction, owner cancellation
// can stop proof custody but cannot turn that contradiction back into credit.
func settleStTransactionNativeFeeOwned(ctx context.Context, intentId server.Id, policy *server.StNativeFeeDenominationPolicy, authority *server.StNativeFeeDenominationAuthority, statement nativefee.Statement, retention stNativeFeeOriginalRetention) (*StTransactionNativeFeeSettlement, error) {
	result, err := settleStTransactionNativeFee(ctx, intentId, policy, authority, statement, retention)
	if errors.Is(err, ErrStNativeFeeConflict) && !errors.Is(err, errStNativeFeeHoldCommitted) {
		// An already observed authenticated contradiction may outlive its
		// caller. A bounded denial-only retry cannot grant any fee credit.
		owner, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_, heldErr := settleStTransactionNativeFee(owner, intentId, policy, authority, statement, func(context.Context, server.PgTx, string) error {
			return errors.New("contradictory proof custody is incomplete; original ceiling remains held")
		})
		return nil, errors.Join(err, heldErr)
	}
	return result, err
}

var errStNativeFeeHoldCommitted = errors.New("native fee contradiction hold is durable")

// This reducer is private: its facts have already passed the invoked native
// proof boundary. It still binds the exact signed winner and denomination in
// one account/scope/intent transaction, including lost-ack identical retries.
func settleStTransactionNativeFee(ctx context.Context, intentId server.Id, policy *server.StNativeFeeDenominationPolicy, authority *server.StNativeFeeDenominationAuthority, statement nativefee.Statement, retainers ...stNativeFeeOriginalRetention) (result *StTransactionNativeFeeSettlement, resultErr error) {
	conflict := false
	observedConflict := false
	defer func() {
		if observedConflict && resultErr != nil {
			resultErr = errors.Join(ErrStNativeFeeConflict, resultErr)
		}
	}()
	defer func() {
		if resultErr != nil {
			result = nil
		}
	}()
	defer recoverStGasError(&resultErr)
	if err := errors.Join(ctx.Err(), policy.Verify(authority)); err != nil {
		return nil, err
	}
	if statement.Genesis != policy.GenesisHash || statement.EvmChainId != policy.ChainId {
		return nil, errors.New("native fee original execution belongs to another network")
	}
	withdrawal, e1 := stNativeFeeRao(statement.WithdrawalRao)
	refund, e2 := stNativeFeeRao(statement.RefundRao)
	debit, e3 := stNativeFeeRao(statement.DebitRao)
	if e1 != nil || e2 != nil || e3 != nil || refund.Cmp(withdrawal) > 0 || new(big.Int).Sub(withdrawal, refund).Cmp(debit) != 0 {
		return nil, errors.New("native fee original withdrawal and refund do not conserve the debit")
	}
	// A valid native contradiction remains a contradiction when its amount
	// cannot map to the selected historical denomination. Defer this refusal
	// until the retained signed original and any earlier outcome are joined.
	debitWei, denominationErr := policy.DebitWei(statement.DebitRao)
	if statement.RuntimeCodeSha256 != policy.RuntimeCodeSha256 || statement.NativeBlockNumber < policy.FirstNativeBlock || statement.NativeBlockNumber > policy.LastNativeBlock {
		denominationErr = errors.New("native fee original execution is outside its approved denomination")
	}
	var mappedDebit *string
	if denominationErr == nil {
		value := debitWei.String()
		mappedDebit = &value
	}
	statementBytes, err := json.Marshal(statement)
	if err != nil || len(statementBytes) > 512*1024 {
		return nil, errors.Join(errors.New("native fee settlement statement exceeds its retained bound"), err)
	}
	digest := sha256.Sum256(statementBytes)
	statementHash := hex.EncodeToString(digest[:])
	policyHash, err := policy.Digest()
	if err != nil {
		return nil, err
	}
	policyBytes, err := json.Marshal(policy)
	if err != nil {
		return nil, err
	}
	authorityBytes, err := json.Marshal(authority)
	if err != nil {
		return nil, err
	}
	server.Tx(ctx, func(tx server.PgTx) {
		conflict = false
		intent := scanStTransactionIntent(tx.QueryRow(ctx, `SELECT `+stTransactionIntentColumns+` FROM st_transaction_intent WHERE intent_id=$1`, intentId))
		if intent.ChainId != policy.ChainId || intent.GenesisHash != policy.GenesisHash || intent.Profile != policy.Profile || intent.FromAddress != statement.Sender || intent.Nonce != statement.Nonce {
			panic(errors.New("native fee original sender or nonce differs from the retained intent"))
		}
		var ignored any
		server.Raise(tx.QueryRow(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, stTransactionAdvisoryLockKey(intent.ChainId, intent.GenesisHash, intent.FromAddress)).Scan(&ignored))
		server.Raise(tx.QueryRow(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, policy.Scope()).Scan(&ignored))
		var scope string
		server.Raise(tx.QueryRow(ctx, `SELECT scope_key FROM st_operator_gas_budget WHERE scope_key=$1 FOR UPDATE`, policy.Scope()).Scan(&scope))
		intent = scanStTransactionIntent(tx.QueryRow(ctx, `SELECT `+stTransactionIntentColumns+` FROM st_transaction_intent WHERE intent_id=$1 FOR UPDATE`, intentId))
		server.Raise(tx.QueryRow(ctx, `SELECT scope_key FROM st_operator_gas_account WHERE chain_id=$1 AND genesis_hash=$2 AND from_address=$3`, int64(intent.ChainId), intent.GenesisHash, intent.FromAddress).Scan(&scope))
		if scope != policy.Scope() {
			panic(errors.New("native fee original account belongs to another gas lifetime owner"))
		}
		attempt, err := scanStTransactionAttempt(tx.QueryRow(ctx, `SELECT `+stTransactionAttemptColumns+` FROM st_transaction_attempt WHERE intent_id=$1 AND tx_hash=$2`, intentId, statement.TransactionHash))
		if errors.Is(err, pgx.ErrNoRows) {
			attempt, err = stNativeFeeRecoverSignedOriginal(ctx, tx, intent, statement)
		}
		server.Raise(err)
		_, liability, err := stGasSignedEnvelope(intent, attempt)
		server.Raise(err)
		if !bytes.Equal(attempt.RawTransaction, statement.RawTransaction) || attempt.TxHash != statement.TransactionHash {
			panic(errors.New("native fee proof differs from the exact retained signed transaction"))
		}
		reservation, err := scanStGasReservation(tx.QueryRow(ctx, `SELECT `+stGasReservationColumns+` FROM st_operator_gas_reservation WHERE intent_id=$1 AND attempt=$2`, intentId, attempt.Attempt))
		server.Raise(err)
		if reservation.ScopeKey != scope || reservation.LogicalKey != intent.LogicalKey || reservation.SignedTxHash == nil || *reservation.SignedTxHash != attempt.TxHash || reservation.MaximumLiabilityWei != liability.String() {
			panic(errors.New("native fee signed winner lost its original gas reservation"))
		}
		var ceiling string
		server.Raise(tx.QueryRow(ctx, `SELECT MAX(maximum_liability_wei)::text FROM st_operator_gas_reservation WHERE intent_id=$1`, intentId).Scan(&ceiling))
		maximum, err := server.StOperatorGasQuantity(ceiling)
		server.Raise(err)
		result = &StTransactionNativeFeeSettlement{IntentId: intentId, Attempt: attempt.Attempt, TransactionHash: statement.TransactionHash, PolicySha256: policyHash, StatementSha256: statementHash, DebitRao: statement.DebitRao, OriginalCeilingWei: ceiling}
		if mappedDebit != nil {
			result.DebitWei, result.ExceedsCeiling = *mappedDebit, debitWei.Cmp(maximum) > 0
		}
		var originalApprover string
		err = tx.QueryRow(ctx, `SELECT approver_public_key FROM st_operator_native_fee_owner WHERE scope_key=$1`, scope).Scan(&originalApprover)
		if errors.Is(err, pgx.ErrNoRows) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO st_operator_native_fee_owner(scope_key,approver_public_key,create_time) VALUES($1,$2,$3)`, scope, authority.ApproverPublicKey, server.NowUtc()))
		} else {
			server.Raise(err)
			if originalApprover != authority.ApproverPublicKey {
				panic(errors.New("native fee denomination cannot replace its original approval key"))
			}
		}
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO st_operator_native_fee_policy(policy_sha256,scope_key,policy_json,authority_json,create_time) VALUES($1,$2,$3,$4,$5) ON CONFLICT(policy_sha256) DO NOTHING`, policyHash, scope, policyBytes, authorityBytes, server.NowUtc()))
		retain := func() {
			for _, retention := range retainers {
				server.Raise(retention(ctx, tx, statementHash))
			}
		}
		hold := func(reason string) {
			observedConflict = true
			conflict, result = true, nil
			var priorMaximum *string
			err := tx.QueryRow(ctx, `SELECT maximum_debit_wei::text FROM st_operator_native_fee_hold WHERE intent_id=$1`, intentId).Scan(&priorMaximum)
			needsOriginal := errors.Is(err, pgx.ErrNoRows)
			if errors.Is(err, pgx.ErrNoRows) {
			} else {
				server.Raise(err)
				if mappedDebit != nil {
					if priorMaximum == nil {
						needsOriginal = true
					} else {
						prior, ok := new(big.Int).SetString(*priorMaximum, 10)
						if !ok {
							panic(errors.New("native fee conflict lost its original amount"))
						}
						needsOriginal = debitWei.Cmp(prior) > 0
					}
				}
			}
			if needsOriginal {
				server.RaisePgResult(tx.Exec(ctx, `SAVEPOINT native_fee_hold_originals`))
				var custodyErr error
				for _, retention := range retainers {
					if custodyErr = retention(ctx, tx, statementHash); custodyErr != nil {
						break
					}
				}
				if custodyErr != nil {
					server.RaisePgResult(tx.Exec(ctx, `ROLLBACK TO SAVEPOINT native_fee_hold_originals`))
					reason = "authenticated contradiction lost complete original custody; liability remains held"
				}
				server.RaisePgResult(tx.Exec(ctx, `RELEASE SAVEPOINT native_fee_hold_originals`))
			}
			stNativeFeeHold(ctx, tx, intent, scope, policyHash, statementHash, statementBytes, mappedDebit, reason)
		}
		var held bool
		server.Raise(tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM st_operator_native_fee_hold WHERE intent_id=$1)`, intentId).Scan(&held))
		if held {
			hold("original authenticated conflict remains unresolved")
			return
		}
		if observedConflict {
			hold("authenticated contradiction observed before a transaction retry")
			return
		}
		if reason := stNativeFeeRetainedConflict(ctx, tx, intent, attempt, statement); reason != "" {
			hold(reason)
			return
		}
		var priorPolicy, priorStatement, priorDebit, priorCeiling string
		var priorBytes []byte
		err = tx.QueryRow(ctx, `SELECT policy_sha256,statement_sha256,statement_json,debit_wei::text,original_ceiling_wei::text FROM st_operator_native_fee_settlement WHERE intent_id=$1`, intentId).Scan(&priorPolicy, &priorStatement, &priorBytes, &priorDebit, &priorCeiling)
		if err == nil {
			var prior nativefee.Statement
			server.Raise(json.Unmarshal(priorBytes, &prior))
			if !stNativeFeeSameOutcome(prior, statement) {
				hold("authenticated original native outcomes disagree")
				return
			}
			server.Raise(denominationErr)
			if priorPolicy != policyHash || priorDebit != result.DebitWei || priorCeiling != ceiling {
				panic(errors.New("native fee denomination differs from its retained original settlement"))
			}
			result.StatementSha256 = priorStatement
			result.Reused = true
			return
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			server.Raise(err)
		}
		server.Raise(denominationErr)
		retain()
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO st_operator_native_fee_settlement(intent_id,attempt,scope_key,logical_key,policy_sha256,transaction_hash,statement_sha256,statement_json,debit_rao,debit_wei,original_ceiling_wei,create_time) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, intentId, attempt.Attempt, scope, intent.LogicalKey, policyHash, statement.TransactionHash, statementHash, statementBytes, statement.DebitRao, result.DebitWei, ceiling, server.NowUtc()))
		// Default transactions use repeatable read. A waiter whose snapshot
		// predates this settlement must retry before reading its fee tables.
		server.RaisePgResult(tx.Exec(ctx, `UPDATE st_operator_gas_budget SET update_time=$2 WHERE scope_key=$1`, scope, server.NowUtc()))
		server.RaisePgResult(tx.Exec(ctx, `UPDATE st_transaction_intent SET update_time=$2 WHERE intent_id=$1`, intentId, server.NowUtc()))
		server.Raise(ctx.Err())
	})
	if conflict {
		return nil, errors.Join(ErrStNativeFeeConflict, errStNativeFeeHoldCommitted)
	}
	return result, nil
}

// A later certificate, alternate proof wrapper or independent replay can prove
// the same immutable original outcome. Its transport hashes do not make new
// spending or replace the first retained receipt and fee statement.
func stNativeFeeSameOutcome(first, second nativefee.Statement) bool {
	if !bytes.Equal(first.RawTransaction, second.RawTransaction) {
		return false
	}
	for _, value := range []*nativefee.Statement{&first, &second} {
		value.RawTransaction = nil
		value.Originals = nil
		value.RequestSha256, value.RequestHash, value.PolicyHash, value.ApprovalHash, value.ProofHash = "", "", "", "", ""
		value.EngineSha256, value.ProfileSha256, value.PayerProfile, value.PayerRuntimeSource = "", "", "", ""
		value.NativeFinalizedNumber, value.NativeFinalizedHash = 0, ""
	}
	return reflect.DeepEqual(first, second)
}

// Previously recorded canonical outcomes are also original evidence. Stronger
// newly admitted proof cannot silently choose between contradictory winners or
// change a confirmed success/revert and its retained receipt block.
func stNativeFeeRetainedConflict(ctx context.Context, tx server.PgTx, intent *StTransactionIntent, attempt *StTransactionAttempt, statement nativefee.Statement) string {
	if (intent.Status == StTxFinalized || intent.Status == StTxCanceled) && (intent.CurrentTxHash == nil || *intent.CurrentTxHash != statement.TransactionHash) {
		return "native proof contradicts the retained canonical transaction winner"
	}
	if attempt.Status == StTxFinalized && statement.ReceiptStatus != 1 || attempt.Status == StTxReverted && statement.ReceiptStatus != 0 || attempt.Status == StTxCanceled && attempt.Kind != StTxAttemptCancellation {
		return "native proof contradicts the retained canonical execution result"
	}
	// FinalizedBlock is the observed finalized head, which can be later than
	// the receipt. Only an earlier height or an equal-height hash conflicts.
	if attempt.FinalizedBlock != nil && (*attempt.FinalizedBlock < statement.EvmBlockNumber || *attempt.FinalizedBlock == statement.EvmBlockNumber && (attempt.FinalizedHash == nil || *attempt.FinalizedHash != statement.EvmBlockHash)) {
		return "native proof contradicts the retained finalized observation boundary"
	}
	if (attempt.Status == StTxFinalized || attempt.Status == StTxReverted || attempt.Status == StTxCanceled) && attempt.InclusionBlock != nil && (*attempt.InclusionBlock != statement.EvmBlockNumber || attempt.InclusionHash == nil || *attempt.InclusionHash != statement.EvmBlockHash) {
		return "native proof contradicts the retained canonical inclusion"
	}
	var otherWinner bool
	server.Raise(tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM st_transaction_attempt WHERE intent_id=$1 AND attempt<>$2 AND status IN ($3,$4,$5))`, intent.IntentId, attempt.Attempt, StTxFinalized, StTxReverted, StTxCanceled).Scan(&otherWinner))
	if otherWinner {
		return "native proof contradicts another retained canonical nonce winner"
	}
	return ""
}

// Keep the first contradiction and the largest independently proved expense.
// Both complete statements retain their denomination authority. An unmappable
// expense is null, never guessed zero; the original ceiling and known expense
// still count. A hold is never cleared by a policy change or proof retry.
func stNativeFeeHold(ctx context.Context, tx server.PgTx, intent *StTransactionIntent, scope, policyHash, statementHash string, statement []byte, debit *string, reason string) {
	server.RaisePgResult(tx.Exec(ctx, `INSERT INTO st_operator_native_fee_hold(intent_id,scope_key,logical_key,first_policy_sha256,first_statement_sha256,first_statement_json,first_reason,maximum_policy_sha256,maximum_statement_sha256,maximum_statement_json,maximum_debit_wei,create_time,update_time)
 VALUES($1,$2,$3,$4,$5,$6,$7,$4,$5,$6,$8,$9,$9)
 ON CONFLICT(intent_id) DO UPDATE SET
 maximum_policy_sha256=CASE WHEN EXCLUDED.maximum_debit_wei IS NOT NULL AND (st_operator_native_fee_hold.maximum_debit_wei IS NULL OR EXCLUDED.maximum_debit_wei>st_operator_native_fee_hold.maximum_debit_wei) THEN EXCLUDED.maximum_policy_sha256 ELSE st_operator_native_fee_hold.maximum_policy_sha256 END,
 maximum_statement_sha256=CASE WHEN EXCLUDED.maximum_debit_wei IS NOT NULL AND (st_operator_native_fee_hold.maximum_debit_wei IS NULL OR EXCLUDED.maximum_debit_wei>st_operator_native_fee_hold.maximum_debit_wei) THEN EXCLUDED.maximum_statement_sha256 ELSE st_operator_native_fee_hold.maximum_statement_sha256 END,
 maximum_statement_json=CASE WHEN EXCLUDED.maximum_debit_wei IS NOT NULL AND (st_operator_native_fee_hold.maximum_debit_wei IS NULL OR EXCLUDED.maximum_debit_wei>st_operator_native_fee_hold.maximum_debit_wei) THEN EXCLUDED.maximum_statement_json ELSE st_operator_native_fee_hold.maximum_statement_json END,
 maximum_debit_wei=GREATEST(EXCLUDED.maximum_debit_wei,st_operator_native_fee_hold.maximum_debit_wei),update_time=EXCLUDED.update_time`, intent.IntentId, scope, intent.LogicalKey, policyHash, statementHash, statement, reason, debit, server.NowUtc()))
	server.RaisePgResult(tx.Exec(ctx, `UPDATE st_operator_gas_budget SET update_time=$2 WHERE scope_key=$1`, scope, server.NowUtc()))
	server.RaisePgResult(tx.Exec(ctx, `UPDATE st_transaction_intent SET update_time=$2 WHERE intent_id=$1`, intent.IntentId, server.NowUtc()))
	server.Raise(ctx.Err())
}

// Native event amounts are canonical exact u64 quantities, including zero.
func stNativeFeeRao(raw string) (*big.Int, error) {
	if raw == "0" {
		return new(big.Int), nil
	}
	value, err := server.StOperatorGasQuantity(raw)
	if err != nil || value.BitLen() > 64 {
		return nil, errors.New("native fee amount is not an original canonical u64")
	}
	return value, nil
}

// Both pre-sign and broadcast admission call this under their existing intent
// lock. A stale nonterminal receipt journal cannot reopen a proved spent nonce.
func stGasRequireUnsettled(ctx context.Context, tx server.PgTx, intentId server.Id) {
	var settled bool
	server.Raise(tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM st_operator_native_fee_settlement WHERE intent_id=$1) OR EXISTS(SELECT 1 FROM st_transaction_intent original JOIN st_operator_gas_account account ON account.chain_id=original.chain_id AND account.genesis_hash=original.genesis_hash AND account.from_address=original.from_address JOIN st_operator_native_fee_hold hold ON hold.scope_key=account.scope_key WHERE original.intent_id=$1)`, intentId).Scan(&settled))
	if settled {
		panic(errors.Join(ErrStOperatorGasAllowance, errors.New("native fee nonce is consumed or its original scope has an unresolved conflict")))
	}
}

// Pending pre-sign reservations also need this admission. The transaction
// closes before external signing; a concurrently produced signature must still
// be retained, while later broadcast rechecks the same settled-nonce boundary.
func RequireStTransactionGasUnsettled(ctx context.Context, intentId server.Id) (resultErr error) {
	defer recoverStGasError(&resultErr)
	if ctx == nil {
		return errors.New("native fee signing admission has no lifecycle owner")
	}
	server.Tx(ctx, func(tx server.PgTx) {
		// A conflict on another nonce writes this shared budget row, so an
		// older repeatable-read snapshot cannot miss a scope-wide hold.
		var scope string
		server.Raise(tx.QueryRow(ctx, `SELECT budget.scope_key FROM st_operator_gas_budget budget JOIN st_operator_gas_account account ON account.scope_key=budget.scope_key JOIN st_transaction_intent original ON original.chain_id=account.chain_id AND original.genesis_hash=account.genesis_hash AND original.from_address=account.from_address WHERE original.intent_id=$1 FOR SHARE OF budget`, intentId).Scan(&scope))
		var original server.Id
		server.Raise(tx.QueryRow(ctx, `SELECT intent_id FROM st_transaction_intent WHERE intent_id=$1 FOR SHARE`, intentId).Scan(&original))
		stGasRequireUnsettled(ctx, tx, intentId)
	})
	return nil
}

// The same nonce aggregation feeds admission and reporting. Retained attempt
// counts and original ceilings never decrease, including on a zero actual fee.
const stGasNonceChargesSql = `SELECT reservation.intent_id,reservation.logical_key,
 MAX(reservation.maximum_liability_wei) original_ceiling,
 CASE WHEN hold.intent_id IS NOT NULL THEN GREATEST(MAX(reservation.maximum_liability_wei),COALESCE(settlement.debit_wei,0),hold.maximum_debit_wei) ELSE COALESCE(settlement.debit_wei,MAX(reservation.maximum_liability_wei)) END charge,
 CASE WHEN hold.intent_id IS NOT NULL THEN 0 ELSE COALESCE(settlement.debit_wei,0) END paid,
 CASE WHEN hold.intent_id IS NOT NULL THEN GREATEST(MAX(reservation.maximum_liability_wei),COALESCE(settlement.debit_wei,0),hold.maximum_debit_wei) WHEN settlement.intent_id IS NULL THEN MAX(reservation.maximum_liability_wei) ELSE 0 END outstanding,
 settlement.intent_id IS NOT NULL AND hold.intent_id IS NULL settled,
 hold.intent_id IS NOT NULL held
 FROM st_operator_gas_reservation reservation
 LEFT JOIN st_operator_native_fee_settlement settlement ON settlement.intent_id=reservation.intent_id
 LEFT JOIN st_operator_native_fee_hold hold ON hold.intent_id=reservation.intent_id
 WHERE reservation.scope_key=$1
 GROUP BY reservation.intent_id,reservation.logical_key,settlement.intent_id,settlement.debit_wei,hold.intent_id,hold.maximum_debit_wei`

// Inspect exact retained evidence without publishing an ingress constructor.
func GetStTransactionNativeFeeSettlement(ctx context.Context, intentId server.Id) (result *StTransactionNativeFeeSettlement, resultErr error) {
	defer func() {
		if resultErr != nil {
			result = nil
		}
	}()
	defer recoverStGasError(&resultErr)
	server.Db(ctx, func(conn server.PgConn) {
		value := &StTransactionNativeFeeSettlement{IntentId: intentId}
		err := conn.QueryRow(ctx, `SELECT attempt,transaction_hash,policy_sha256,statement_sha256,debit_rao::text,debit_wei::text,original_ceiling_wei::text FROM st_operator_native_fee_settlement WHERE intent_id=$1`, intentId).Scan(&value.Attempt, &value.TransactionHash, &value.PolicySha256, &value.StatementSha256, &value.DebitRao, &value.DebitWei, &value.OriginalCeilingWei)
		if errors.Is(err, pgx.ErrNoRows) {
			return
		}
		server.Raise(err)
		paid, ok := new(big.Int).SetString(value.DebitWei, 10)
		ceiling, ceilingOk := new(big.Int).SetString(value.OriginalCeilingWei, 10)
		if !ok || !ceilingOk || paid.Sign() < 0 || ceiling.Sign() <= 0 || strings.TrimSpace(value.DebitWei) != value.DebitWei {
			panic(errors.New("native fee retained settlement amount is invalid"))
		}
		value.ExceedsCeiling = paid.Cmp(ceiling) > 0
		result = value
	})
	return result, nil
}
