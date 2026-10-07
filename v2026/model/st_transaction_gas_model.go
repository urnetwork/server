package model

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"math/big"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server/v2026"
)

var ErrStOperatorGasAllowance = errors.New("operator gas allowance is unavailable or exhausted; original liabilities retained")

// These match the existing strecovery original-census hard byte ceilings.
const stGasMaximumTransactionBytes = 128 * 1024
const stGasMaximumHistoryBytes = 32 * 1024 * 1024

func StOperatorGasAccountEnrolled(ctx context.Context, chainId uint64, genesis, account string) (enrolled bool, resultErr error) {
	defer recoverStGasError(&resultErr)
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM st_operator_gas_account WHERE chain_id=$1 AND genesis_hash=$2 AND from_address=$3)`, int64(chainId), genesis, strings.ToLower(account)).Scan(&enrolled))
	})
	return enrolled, nil
}

// Database failures roll back before this outer boundary converts the error.
func recoverStGasError(resultErr *error) {
	if recovered := recover(); recovered != nil {
		if err, ok := recovered.(error); ok {
			*resultErr = errors.Join(*resultErr, err)
		} else {
			panic(recovered)
		}
	}
}

type StTransactionGasReservation struct {
	IntentId            server.Id
	Attempt             int
	ScopeKey            string
	LogicalKey          string
	PolicySha256        string
	Kind                string
	UnsignedTransaction []byte
	SigningHash         string
	MaximumLiabilityWei string
	SignedTxHash        *string
	Historical          bool
	Reused              bool
}

const stGasReservationColumns = `intent_id, attempt, scope_key, logical_key, policy_sha256,
 kind, unsigned_transaction, signing_hash, maximum_liability_wei::text, signed_tx_hash, historical`

func scanStGasReservation(row interface{ Scan(...any) error }) (*StTransactionGasReservation, error) {
	var value StTransactionGasReservation
	if err := row.Scan(&value.IntentId, &value.Attempt, &value.ScopeKey, &value.LogicalKey, &value.PolicySha256,
		&value.Kind, &value.UnsignedTransaction, &value.SigningHash, &value.MaximumLiabilityWei, &value.SignedTxHash, &value.Historical); err != nil {
		return nil, err
	}
	return &value, nil
}

// Envelope bounds are checked before reservation and again before signing.
// There is no receipt-based rebate: these are conservative outstanding/paid
// ceilings, never a claim that EVM gas is an authenticated native fee debit.
func StTransactionGasLiability(tx *types.Transaction) (*big.Int, error) {
	if tx == nil || tx.Gas() == 0 || tx.Gas() > uint64(^uint64(0)>>1) || tx.Value().Sign() != 0 || tx.To() == nil ||
		tx.Type() != types.LegacyTxType && tx.Type() != types.DynamicFeeTxType {
		return nil, errors.New("operator gas transaction type, gas or value is invalid")
	}
	fee := tx.GasFeeCap()
	if fee.Sign() <= 0 || fee.BitLen() > 256 || tx.GasTipCap().Sign() < 0 || tx.GasTipCap().Cmp(fee) > 0 {
		return nil, errors.New("operator gas transaction fee envelope is invalid")
	}
	maximum := new(big.Int).Mul(new(big.Int).SetUint64(tx.Gas()), fee)
	if maximum.BitLen() > 256 {
		return nil, errors.New("operator gas transaction liability exceeds uint256")
	}
	return maximum, nil
}

func ValidateStTransactionGasEnvelope(policy *server.StOperatorGasPolicy, tx *types.Transaction) (*big.Int, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	maximum, err := StTransactionGasLiability(tx)
	if err != nil {
		return nil, err
	}
	if tx.Type() == types.DynamicFeeTxType && (!tx.ChainId().IsUint64() || tx.ChainId().Uint64() != policy.ChainId) {
		return nil, fmt.Errorf("%w: typed transaction differs from approved chain", ErrStOperatorGasAllowance)
	}
	fee, _ := server.StOperatorGasQuantity(policy.MaximumFeePerGasWei)
	tip, _ := server.StOperatorGasQuantity(policy.MaximumTipPerGasWei)
	intent, _ := server.StOperatorGasQuantity(policy.MaximumIntentLiabilityWei)
	if tx.Gas() > policy.MaximumGas || tx.GasFeeCap().Cmp(fee) > 0 || tx.Type() == types.DynamicFeeTxType && tx.GasTipCap().Cmp(tip) > 0 || maximum.Cmp(intent) > 0 {
		return nil, fmt.Errorf("%w: unsigned gas/fee/tip/intent envelope exceeds approval", ErrStOperatorGasAllowance)
	}
	return maximum, nil
}

// History is all-status and includes every immutable operation and signature.
// Its framing is shared by the read-only independent approval producer and
// atomic initial adoption; terminal labels cannot hide original liability.
func stGasHistoryHashStart() hash.Hash {
	digest := sha256.New()
	_, _ = digest.Write([]byte("urnetwork-operator-gas-history-v1\n"))
	return digest
}

func stGasHistoryRecord(digest hash.Hash, intent *StTransactionIntent, attempt *StTransactionAttempt) error {
	encoded, err := json.Marshal(struct {
		IntentId     server.Id `json:"intent_id"`
		LogicalKey   string    `json:"logical_key"`
		Generation   int       `json:"generation"`
		ChainId      uint64    `json:"chain_id"`
		GenesisHash  string    `json:"genesis_hash"`
		FromAddress  string    `json:"from_address"`
		ToAddress    string    `json:"to_address"`
		Nonce        uint64    `json:"nonce"`
		CalldataHash string    `json:"calldata_hash"`
		Calldata     []byte    `json:"calldata"`
		Attempt      int       `json:"attempt"`
		Kind         string    `json:"kind"`
		Hash         string    `json:"hash"`
		Raw          []byte    `json:"raw"`
		Gas          uint64    `json:"gas"`
		Price        *string   `json:"price"`
		Tip          *string   `json:"tip"`
		Fee          *string   `json:"fee"`
	}{IntentId: intent.IntentId, LogicalKey: intent.LogicalKey, Generation: intent.Generation, ChainId: intent.ChainId, GenesisHash: intent.GenesisHash,
		FromAddress: intent.FromAddress, ToAddress: intent.ToAddress, Nonce: intent.Nonce, CalldataHash: intent.CalldataHash, Calldata: intent.Calldata,
		Attempt: attempt.Attempt, Kind: attempt.Kind, Hash: attempt.TxHash, Raw: attempt.RawTransaction, Gas: attempt.GasLimit, Price: attempt.GasPrice, Tip: attempt.GasTipCap, Fee: attempt.GasFeeCap})
	if err != nil {
		return err
	}
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(encoded)))
	_, _ = digest.Write(length[:])
	_, _ = digest.Write(encoded)
	return nil
}

func stGasSignedEnvelope(intent *StTransactionIntent, attempt *StTransactionAttempt) (*types.Transaction, *big.Int, error) {
	var transaction types.Transaction
	if err := transaction.UnmarshalBinary(attempt.RawTransaction); err != nil {
		return nil, nil, err
	}
	maximum, err := StTransactionGasLiability(&transaction)
	if err != nil {
		return nil, nil, err
	}
	signer := types.LatestSignerForChainID(new(big.Int).SetUint64(intent.ChainId))
	from, err := types.Sender(signer, &transaction)
	if err != nil || transaction.ChainId().Uint64() != intent.ChainId || !transaction.ChainId().IsUint64() ||
		strings.ToLower(from.Hex()) != intent.FromAddress || transaction.Nonce() != intent.Nonce || !strings.EqualFold(transaction.Hash().Hex(), attempt.TxHash) || transaction.Gas() != attempt.GasLimit {
		return nil, nil, errors.New("operator gas retained signature differs from its immutable account/nonce")
	}
	if attempt.Kind == StTxAttemptCancellation {
		if *transaction.To() != from || len(transaction.Data()) != 0 || transaction.Gas() != 21_000 {
			return nil, nil, errors.New("operator gas retained cancellation is invalid")
		}
	} else if attempt.Kind != StTxAttemptExecution || !strings.EqualFold(transaction.To().Hex(), intent.ToAddress) || !bytes.Equal(transaction.Data(), intent.Calldata) {
		return nil, nil, errors.New("operator gas retained payload differs from its original intent")
	}
	if transaction.Type() == types.LegacyTxType {
		if attempt.GasPrice == nil || *attempt.GasPrice != transaction.GasPrice().String() || attempt.GasFeeCap != nil || attempt.GasTipCap != nil {
			return nil, nil, errors.New("operator gas retained legacy fee differs from signed bytes")
		}
	} else if attempt.GasPrice != nil || attempt.GasFeeCap == nil || attempt.GasTipCap == nil || *attempt.GasFeeCap != transaction.GasFeeCap().String() || *attempt.GasTipCap != transaction.GasTipCap().String() {
		return nil, nil, errors.New("operator gas retained dynamic fee differs from signed bytes")
	}
	return &transaction, maximum, nil
}

// The callback streams under an account lock during adoption. The independent
// read-only producer must re-review a changed census instead of rebasing it.
func stGasAccountHistory(ctx context.Context, query server.PgTx, chainId uint64, genesis, account string, maximumAttempts uint64, remainingBytes *int64, visit func(*StTransactionIntent, *StTransactionAttempt, *types.Transaction, *big.Int) error) (string, error) {
	var ambiguous bool
	if err := query.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM st_transaction_intent WHERE chain_id=$1 AND from_address=$2 AND (genesis_hash !~ '^0x[0-9a-f]{64}$' OR genesis_hash=$3))`, int64(chainId), account, "0x"+strings.Repeat("0", 64)).Scan(&ambiguous); err != nil {
		return "", err
	}
	if ambiguous {
		return "", errors.New("operator gas original account history has unresolved legacy network identity")
	}
	// An unsigned legacy row may be a lost signer result. Refuse it before
	// loading its calldata; the signed-row aggregate cannot bound that body.
	var unsignedHistory bool
	if err := query.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM st_transaction_intent intent WHERE chain_id=$1 AND genesis_hash=$2 AND from_address=$3 AND NOT EXISTS(SELECT 1 FROM st_transaction_attempt attempt WHERE attempt.intent_id=intent.intent_id))`, int64(chainId), genesis, account).Scan(&unsignedHistory); err != nil {
		return "", err
	}
	if unsignedHistory {
		return "", errors.New("operator gas unsigned original history requires reconciliation before bounded census")
	}
	var count, largestRaw, largestCalldata, totalBytes int64
	if err := query.QueryRow(ctx, `SELECT COUNT(*),COALESCE(MAX(octet_length(attempt.raw_transaction)),0),COALESCE(MAX(octet_length(intent.calldata)),0),COALESCE(SUM(octet_length(attempt.raw_transaction)::bigint+octet_length(intent.calldata)::bigint),0)::bigint FROM st_transaction_attempt attempt JOIN st_transaction_intent intent USING(intent_id) WHERE intent.chain_id=$1 AND intent.genesis_hash=$2 AND intent.from_address=$3`, int64(chainId), genesis, account).Scan(&count, &largestRaw, &largestCalldata, &totalBytes); err != nil {
		return "", err
	}
	if uint64(count) > maximumAttempts || largestRaw > stGasMaximumTransactionBytes || largestCalldata > stGasMaximumTransactionBytes || remainingBytes == nil || totalBytes > *remainingBytes {
		return "", errors.New("operator gas original signature census exceeds its retained byte/count bound")
	}
	*remainingBytes -= totalBytes
	rows, err := query.Query(ctx, `SELECT intent_id FROM st_transaction_intent WHERE chain_id=$1 AND genesis_hash=$2 AND from_address=$3 ORDER BY nonce, generation`, int64(chainId), genesis, account)
	if err != nil {
		return "", err
	}
	var ids []server.Id
	for rows.Next() {
		var id server.Id
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return "", err
		}
		ids = append(ids, id)
		if uint64(len(ids)) > maximumAttempts+1 {
			rows.Close()
			return "", ErrStOperatorGasAllowance
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return "", err
	}
	digest := stGasHistoryHashStart()
	var seenCount uint64
	for _, id := range ids {
		intent := scanStTransactionIntent(query.QueryRow(ctx, `SELECT `+stTransactionIntentColumns+` FROM st_transaction_intent WHERE intent_id=$1`, id))
		rows, err := query.Query(ctx, `SELECT `+stTransactionAttemptColumns+` FROM st_transaction_attempt WHERE intent_id=$1 ORDER BY attempt`, id)
		if err != nil {
			return "", err
		}
		var attempts []*StTransactionAttempt
		for rows.Next() {
			attempt, err := scanStTransactionAttempt(rows)
			if err != nil {
				rows.Close()
				return "", err
			}
			attempts = append(attempts, attempt)
			seenCount++
			if seenCount > maximumAttempts {
				rows.Close()
				return "", ErrStOperatorGasAllowance
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return "", err
		}
		if len(attempts) != intent.AttemptCount || len(attempts) == 0 {
			return "", errors.New("operator gas historical attempt census is incomplete or has an unknown unsigned signing outcome")
		}
		for index, attempt := range attempts {
			if attempt.Attempt != index+1 {
				return "", errors.New("operator gas original attempt sequence has a gap")
			}
			transaction, liability, err := stGasSignedEnvelope(intent, attempt)
			if err != nil {
				return "", err
			}
			if err = stGasHistoryRecord(digest, intent, attempt); err != nil {
				return "", err
			}
			if visit != nil {
				if err = visit(intent, attempt, transaction, liability); err != nil {
					return "", err
				}
			}
		}
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// Read-only approval production pins the complete original signed history.
func StOperatorGasAccountHistorySha256(ctx context.Context, chainId uint64, genesis, account string, maximumAttempts uint64) (digest string, resultErr error) {
	defer recoverStGasError(&resultErr)
	if maximumAttempts == 0 || maximumAttempts > uint64(^uint64(0)>>1) {
		return "", ErrStOperatorGasAllowance
	}
	server.Tx(ctx, func(tx server.PgTx) {
		var ignored any
		server.Raise(tx.QueryRow(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, stTransactionAdvisoryLockKey(chainId, genesis, account)).Scan(&ignored))
		var err error
		remainingBytes := int64(stGasMaximumHistoryBytes)
		digest, err = stGasAccountHistory(ctx, tx, chainId, genesis, account, maximumAttempts, &remainingBytes, nil)
		server.Raise(err)
	})
	return digest, nil
}

func stGasLockPolicyAccounts(ctx context.Context, tx server.PgTx, policy *server.StOperatorGasPolicy) {
	var accounts []string
	for _, account := range policy.AllAccounts() {
		accounts = append(accounts, account.Address)
	}
	sort.Strings(accounts)
	for _, account := range accounts {
		var ignored any
		server.Raise(tx.QueryRow(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, stTransactionAdvisoryLockKey(policy.ChainId, policy.GenesisHash, account)).Scan(&ignored))
	}
	var ignored any
	server.Raise(tx.QueryRow(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, policy.Scope()).Scan(&ignored))
}

func stGasEnsureLogical(ctx context.Context, tx server.PgTx, policy *server.StOperatorGasPolicy, digest, logical string) {
	server.RaisePgResult(tx.Exec(ctx, `INSERT INTO st_operator_gas_intent(logical_key,scope_key,original_policy_sha256,maximum_liability_wei,maximum_attempts,create_time) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(logical_key) DO NOTHING`, logical, policy.Scope(), digest, policy.MaximumIntentLiabilityWei, int64(policy.MaximumIntentAttempts), server.NowUtc()))
	var scope string
	server.Raise(tx.QueryRow(ctx, `SELECT scope_key FROM st_operator_gas_intent WHERE logical_key=$1`, logical).Scan(&scope))
	if scope != policy.Scope() {
		panic(errors.New("operator gas logical intent belongs to another lifetime owner"))
	}
}

// Only an admitted original native settlement replaces a nonce's full ceiling.
// Terminal labels, receipt gas products and policy revisions change no charge.
// A reverted generation has its own nonce and remains part of the same total.
func stGasCheckTotals(ctx context.Context, tx server.PgTx, policy *server.StOperatorGasPolicy) {
	var nativeConflict bool
	server.Raise(tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM st_operator_native_fee_hold WHERE scope_key=$1)`, policy.Scope()).Scan(&nativeConflict))
	if nativeConflict {
		panic(errors.Join(ErrStOperatorGasAllowance, ErrStNativeFeeConflict))
	}
	var total string
	var attempts int64
	server.Raise(tx.QueryRow(ctx, `SELECT COALESCE(SUM(charge),0)::text FROM (`+stGasNonceChargesSql+`) nonces`, policy.Scope()).Scan(&total))
	server.Raise(tx.QueryRow(ctx, `SELECT COUNT(*) FROM st_operator_gas_reservation WHERE scope_key=$1`, policy.Scope()).Scan(&attempts))
	value, ok := new(big.Int).SetString(total, 10)
	limit, _ := server.StOperatorGasQuantity(policy.MaximumLifetimeLiabilityWei)
	if !ok || value.Cmp(limit) > 0 || uint64(attempts) > policy.MaximumLifetimeAttempts {
		panic(fmt.Errorf("%w: cumulative lifetime liability or signing count", ErrStOperatorGasAllowance))
	}
	rows, err := tx.Query(ctx, `SELECT limits.logical_key,limits.maximum_liability_wei::text,limits.maximum_attempts,COALESCE(totals.total,0)::text,COALESCE(counts.count,0) FROM st_operator_gas_intent limits LEFT JOIN (SELECT logical_key,SUM(charge) total FROM (`+stGasNonceChargesSql+`) nonces GROUP BY logical_key) totals ON totals.logical_key=limits.logical_key LEFT JOIN (SELECT logical_key,COUNT(*) count FROM st_operator_gas_reservation WHERE scope_key=$1 GROUP BY logical_key) counts ON counts.logical_key=limits.logical_key WHERE limits.scope_key=$1`, policy.Scope())
	server.Raise(err)
	defer rows.Close()
	currentLimit, _ := server.StOperatorGasQuantity(policy.MaximumIntentLiabilityWei)
	for rows.Next() {
		var logical, original, used string
		var count, maxCount int64
		server.Raise(rows.Scan(&logical, &original, &maxCount, &used, &count))
		limit, ok := new(big.Int).SetString(original, 10)
		if !ok {
			panic(errors.New("operator gas original intent ceiling is invalid"))
		}
		if currentLimit.Cmp(limit) < 0 {
			limit = currentLimit
		}
		value, ok := new(big.Int).SetString(used, 10)
		if !ok || value.Cmp(limit) > 0 || count > maxCount || uint64(count) > policy.MaximumIntentAttempts {
			panic(fmt.Errorf("%w: original logical intent %s", ErrStOperatorGasAllowance, logical))
		}
	}
	server.Raise(rows.Err())
}

func stGasAdmitPolicy(ctx context.Context, tx server.PgTx, policy *server.StOperatorGasPolicy, authority *server.StOperatorGasAuthority) string {
	server.Raise(policy.Verify(authority, server.NowUtc()))
	digest, err := policy.Digest()
	server.Raise(err)
	stGasLockPolicyAccounts(ctx, tx, policy)
	var priorDigest, priorApprover string
	var revision int64
	err = tx.QueryRow(ctx, `SELECT current_policy_sha256,current_revision,approver_public_key FROM st_operator_gas_budget WHERE scope_key=$1 FOR UPDATE`, policy.Scope()).Scan(&priorDigest, &revision, &priorApprover)
	now := server.NowUtc()
	if errors.Is(err, pgx.ErrNoRows) {
		if policy.Revision != 0 {
			panic(errors.New("operator gas initial policy skips its original approval"))
		}
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO st_operator_gas_budget(scope_key,chain_id,genesis_hash,no_id,approver_public_key,current_revision,current_policy_sha256,maximum_lifetime_wei,maximum_lifetime_attempts,create_time,update_time) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$10)`, policy.Scope(), int64(policy.ChainId), policy.GenesisHash, int64(policy.NoId), authority.ApproverPublicKey, int64(policy.Revision), digest, policy.MaximumLifetimeLiabilityWei, int64(policy.MaximumLifetimeAttempts), now))
	} else {
		server.Raise(err)
		if priorApprover != authority.ApproverPublicKey {
			panic(errors.New("operator gas independent approval key cannot replace its original trust root"))
		}
		if priorDigest != digest && (policy.Revision != uint64(revision)+1 || policy.PreviousPolicySha256 != priorDigest) {
			panic(errors.New("operator gas policy rollback or disconnected revision"))
		}
	}
	policyBytes, err := json.Marshal(policy)
	server.Raise(err)
	authorityBytes, err := json.Marshal(authority)
	server.Raise(err)
	server.RaisePgResult(tx.Exec(ctx, `INSERT INTO st_operator_gas_policy(policy_sha256,scope_key,revision,policy_json,authority_json,create_time) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(policy_sha256) DO NOTHING`, digest, policy.Scope(), int64(policy.Revision), policyBytes, authorityBytes, now))
	remainingBytes := int64(stGasMaximumHistoryBytes)
	declared := map[string]bool{}
	for _, account := range policy.AllAccounts() {
		declared[account.Address] = true
	}
	rows, err := tx.Query(ctx, `SELECT from_address FROM st_operator_gas_account WHERE scope_key=$1`, policy.Scope())
	server.Raise(err)
	for rows.Next() {
		var address string
		server.Raise(rows.Scan(&address))
		if !declared[address] {
			rows.Close()
			panic(errors.New("operator gas policy omitted an original account from its lifetime census"))
		}
	}
	server.Raise(rows.Err())
	rows.Close()
	for _, account := range policy.AllAccounts() {
		var scope, history string
		err = tx.QueryRow(ctx, `SELECT scope_key,initial_history_sha256 FROM st_operator_gas_account WHERE chain_id=$1 AND genesis_hash=$2 AND from_address=$3`, int64(policy.ChainId), policy.GenesisHash, account.Address).Scan(&scope, &history)
		if err == nil {
			if scope != policy.Scope() || history != account.InitialHistorySha256 {
				panic(errors.New("operator gas account owner or original history pin changed"))
			}
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			server.Raise(err)
		}
		history, err = stGasAccountHistory(ctx, tx, policy.ChainId, policy.GenesisHash, account.Address, policy.MaximumLifetimeAttempts, &remainingBytes, func(intent *StTransactionIntent, attempt *StTransactionAttempt, transaction *types.Transaction, liability *big.Int) error {
			stGasEnsureLogical(ctx, tx, policy, digest, intent.LogicalKey)
			signer := types.LatestSignerForChainID(new(big.Int).SetUint64(intent.ChainId))
			_, err := tx.Exec(ctx, `INSERT INTO st_operator_gas_reservation(intent_id,attempt,scope_key,logical_key,policy_sha256,kind,unsigned_transaction,signing_hash,maximum_liability_wei,signed_tx_hash,historical,create_time) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,true,$11)`, intent.IntentId, attempt.Attempt, policy.Scope(), intent.LogicalKey, digest, attempt.Kind, attempt.RawTransaction, strings.ToLower(signer.Hash(transaction).Hex()), liability.String(), attempt.TxHash, now)
			return err
		})
		server.Raise(err)
		if history != account.InitialHistorySha256 {
			panic(errors.New("operator gas initial signature census differs from independent approval"))
		}
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO st_operator_gas_account(chain_id,genesis_hash,from_address,scope_key,initial_history_sha256,create_time) VALUES($1,$2,$3,$4,$5,$6)`, int64(policy.ChainId), policy.GenesisHash, account.Address, policy.Scope(), history, now))
	}
	stGasCheckTotals(ctx, tx, policy)
	server.RaisePgResult(tx.Exec(ctx, `UPDATE st_operator_gas_budget SET current_revision=$2,current_policy_sha256=$3,maximum_lifetime_wei=$4,maximum_lifetime_attempts=$5,update_time=$6 WHERE scope_key=$1`, policy.Scope(), int64(policy.Revision), digest, policy.MaximumLifetimeLiabilityWei, int64(policy.MaximumLifetimeAttempts), now))
	return digest
}

func AdmitStOperatorGasPolicy(ctx context.Context, policy *server.StOperatorGasPolicy, authority *server.StOperatorGasAuthority) (resultErr error) {
	defer recoverStGasError(&resultErr)
	if err := policy.Verify(authority, server.NowUtc()); err != nil {
		return err
	}
	server.Tx(ctx, func(tx server.PgTx) { stGasAdmitPolicy(ctx, tx, policy, authority) })
	return nil
}

func GetPendingStTransactionGasReservation(ctx context.Context, intentId server.Id) (result *StTransactionGasReservation, resultErr error) {
	defer func() {
		if resultErr != nil {
			result = nil
		}
	}()
	defer recoverStGasError(&resultErr)
	server.Db(ctx, func(conn server.PgConn) {
		row := conn.QueryRow(ctx, `SELECT `+stGasReservationColumns+` FROM st_operator_gas_reservation reservation WHERE intent_id=$1 AND NOT EXISTS(SELECT 1 FROM st_transaction_attempt attempt WHERE attempt.intent_id=reservation.intent_id AND attempt.attempt=reservation.attempt) ORDER BY attempt LIMIT 1`, intentId)
		var err error
		result, err = scanStGasReservation(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return
		}
		server.Raise(err)
		result.Reused = true
	})
	return result, nil
}

func ReserveStTransactionGasAttempt(ctx context.Context, policy *server.StOperatorGasPolicy, authority *server.StOperatorGasAuthority, intentId server.Id, expectedAttempt int, kind string, unsigned *types.Transaction) (result *StTransactionGasReservation, resultErr error) {
	defer func() {
		if resultErr != nil {
			result = nil
		}
	}()
	defer recoverStGasError(&resultErr)
	if err := policy.Verify(authority, server.NowUtc()); err != nil {
		return nil, err
	}
	liability, err := ValidateStTransactionGasEnvelope(policy, unsigned)
	if err != nil {
		return nil, err
	}
	raw, err := unsigned.MarshalBinary()
	if err != nil {
		return nil, err
	}
	if len(raw) > stGasMaximumTransactionBytes {
		return nil, errors.New("operator gas unsigned envelope exceeds retained original byte bound")
	}
	// Project the largest signature encoding before signing. The signature is
	// deliberately synthetic and is used only for byte length, never authority.
	var widestSignature [65]byte
	for index := 0; index < 64; index++ {
		widestSignature[index] = 255
	}
	widestSignature[64] = 1
	projected, err := unsigned.WithSignature(types.LatestSignerForChainID(new(big.Int).SetUint64(policy.ChainId)), widestSignature[:])
	if err != nil {
		return nil, err
	}
	projectedRaw, err := projected.MarshalBinary()
	if err != nil {
		return nil, err
	}
	if len(projectedRaw) > stGasMaximumTransactionBytes {
		return nil, errors.New("operator gas signed envelope would exceed retained original byte bound")
	}
	server.Tx(ctx, func(tx server.PgTx) {
		digest := stGasAdmitPolicy(ctx, tx, policy, authority)
		intent := scanStTransactionIntent(tx.QueryRow(ctx, `SELECT `+stTransactionIntentColumns+` FROM st_transaction_intent WHERE intent_id=$1 FOR UPDATE`, intentId))
		stGasRequireUnsettled(ctx, tx, intentId)
		allowed := false
		for _, account := range policy.Accounts {
			allowed = allowed || account.Address == intent.FromAddress
		}
		if !allowed || intent.ChainId != policy.ChainId || intent.GenesisHash != policy.GenesisHash || intent.Nonce != unsigned.Nonce() || stTransactionIntentTerminal(intent.Status) || intent.Status == StTxMined || expectedAttempt <= 0 || expectedAttempt > intent.AttemptCount+1 {
			panic(ErrStOperatorGasAllowance)
		}
		if expectedAttempt <= intent.AttemptCount {
			var err error
			result, err = scanStGasReservation(tx.QueryRow(ctx, `SELECT `+stGasReservationColumns+` FROM st_operator_gas_reservation WHERE intent_id=$1 AND attempt=$2`, intentId, intent.AttemptCount))
			server.Raise(err)
			result.Reused = true
			return
		}
		if intent.AttemptCount >= 3 {
			panic(ErrStOperatorGasAllowance)
		}
		if kind == StTxAttemptCancellation {
			if *unsigned.To() != common.HexToAddress(intent.FromAddress) || len(unsigned.Data()) != 0 || unsigned.Gas() != 21_000 {
				panic(errors.New("operator gas cancellation differs from original account"))
			}
		} else if kind != StTxAttemptExecution || !strings.EqualFold(unsigned.To().Hex(), intent.ToAddress) || !bytes.Equal(unsigned.Data(), intent.Calldata) {
			panic(errors.New("operator gas unsigned payload differs from original intent"))
		}
		var err error
		result, err = scanStGasReservation(tx.QueryRow(ctx, `SELECT `+stGasReservationColumns+` FROM st_operator_gas_reservation WHERE intent_id=$1 AND attempt=$2`, intentId, intent.AttemptCount+1))
		if err == nil {
			result.Reused = true
			return
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			server.Raise(err)
		}
		stGasEnsureLogical(ctx, tx, policy, digest, intent.LogicalKey)
		signer := types.LatestSignerForChainID(new(big.Int).SetUint64(intent.ChainId))
		hash := strings.ToLower(signer.Hash(unsigned).Hex())
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO st_operator_gas_reservation(intent_id,attempt,scope_key,logical_key,policy_sha256,kind,unsigned_transaction,signing_hash,maximum_liability_wei,historical,create_time) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,false,$10)`, intentId, intent.AttemptCount+1, policy.Scope(), intent.LogicalKey, digest, kind, raw, hash, liability.String(), server.NowUtc()))
		stGasCheckTotals(ctx, tx, policy)
		result = &StTransactionGasReservation{IntentId: intentId, Attempt: intent.AttemptCount + 1, ScopeKey: policy.Scope(), LogicalKey: intent.LogicalKey, PolicySha256: digest, Kind: kind, UnsignedTransaction: bytes.Clone(raw), SigningHash: hash, MaximumLiabilityWei: liability.String()}
	})
	return result, nil
}

// Called inside AddStTransactionAttempt after its account and intent locks.
// An admitted account cannot bypass pre-sign reservation through an old API.
func checkStTransactionGasAttempt(ctx context.Context, tx server.PgTx, intent *StTransactionIntent, candidate *StTransactionAttempt) {
	var scope string
	err := tx.QueryRow(ctx, `SELECT scope_key FROM st_operator_gas_account WHERE chain_id=$1 AND genesis_hash=$2 AND from_address=$3`, int64(intent.ChainId), intent.GenesisHash, intent.FromAddress).Scan(&scope)
	if errors.Is(err, pgx.ErrNoRows) {
		return
	}
	server.Raise(err)
	reservation, err := scanStGasReservation(tx.QueryRow(ctx, `SELECT `+stGasReservationColumns+` FROM st_operator_gas_reservation WHERE intent_id=$1 AND attempt=$2`, intent.IntentId, candidate.Attempt))
	server.Raise(err)
	transaction, liability, err := stGasSignedEnvelope(intent, candidate)
	server.Raise(err)
	signer := types.LatestSignerForChainID(new(big.Int).SetUint64(intent.ChainId))
	if reservation.ScopeKey != scope || reservation.Kind != candidate.Kind || reservation.SigningHash != strings.ToLower(signer.Hash(transaction).Hex()) || reservation.MaximumLiabilityWei != liability.String() || reservation.SignedTxHash != nil && !strings.EqualFold(*reservation.SignedTxHash, candidate.TxHash) {
		panic(errors.New("operator gas signed result differs from its durable reservation"))
	}
	server.RaisePgResult(tx.Exec(ctx, `UPDATE st_operator_gas_reservation SET signed_tx_hash=$3 WHERE intent_id=$1 AND attempt=$2`, intent.IntentId, candidate.Attempt, strings.ToLower(candidate.TxHash)))
}

func ValidateStTransactionGasBroadcast(ctx context.Context, policy *server.StOperatorGasPolicy, authority *server.StOperatorGasAuthority, intentId server.Id, candidate *StTransactionAttempt) (resultErr error) {
	defer recoverStGasError(&resultErr)
	if err := policy.Verify(authority, server.NowUtc()); err != nil {
		return err
	}
	server.Tx(ctx, func(tx server.PgTx) {
		stGasAdmitPolicy(ctx, tx, policy, authority)
		intent := scanStTransactionIntent(tx.QueryRow(ctx, `SELECT `+stTransactionIntentColumns+` FROM st_transaction_intent WHERE intent_id=$1 FOR UPDATE`, intentId))
		stGasRequireUnsettled(ctx, tx, intentId)
		allowed := false
		for _, account := range policy.Accounts {
			allowed = allowed || account.Address == intent.FromAddress
		}
		if !allowed || intent.ChainId != policy.ChainId || intent.GenesisHash != policy.GenesisHash {
			panic(ErrStOperatorGasAllowance)
		}
		transaction, _, err := stGasSignedEnvelope(intent, candidate)
		server.Raise(err)
		_, err = ValidateStTransactionGasEnvelope(policy, transaction)
		server.Raise(err)
		checkStTransactionGasAttempt(ctx, tx, intent, candidate)
	})
	return nil
}

type StOperatorGasBudgetSnapshot struct {
	MaximumLiabilityWei string
	BudgetChargeWei     string
	PaidFeesWei         string
	OutstandingWei      string
	SettledNonces       int64
	HeldNonces          int64
	Attempts            int64
	PolicySha256        string
	Revision            uint64
}

func GetStOperatorGasBudgetSnapshot(ctx context.Context, scope string) (result *StOperatorGasBudgetSnapshot, resultErr error) {
	defer func() {
		if resultErr != nil {
			result = nil
		}
	}()
	defer recoverStGasError(&resultErr)
	server.Tx(ctx, func(tx server.PgTx) {
		var revision int64
		result = &StOperatorGasBudgetSnapshot{}
		err := tx.QueryRow(ctx, `SELECT current_policy_sha256,current_revision FROM st_operator_gas_budget WHERE scope_key=$1 FOR SHARE`, scope).Scan(&result.PolicySha256, &revision)
		if errors.Is(err, pgx.ErrNoRows) {
			result = nil
			return
		}
		server.Raise(err)
		result.Revision = uint64(revision)
		server.Raise(tx.QueryRow(ctx, `SELECT COALESCE(SUM(original_ceiling),0)::text,COALESCE(SUM(charge),0)::text,COALESCE(SUM(paid),0)::text,COALESCE(SUM(outstanding),0)::text,COUNT(*) FILTER(WHERE settled),COUNT(*) FILTER(WHERE held) FROM (`+stGasNonceChargesSql+`) nonces`, scope).Scan(&result.MaximumLiabilityWei, &result.BudgetChargeWei, &result.PaidFeesWei, &result.OutstandingWei, &result.SettledNonces, &result.HeldNonces))
		server.Raise(tx.QueryRow(ctx, `SELECT COUNT(*) FROM st_operator_gas_reservation WHERE scope_key=$1`, scope).Scan(&result.Attempts))
	})
	return result, nil
}
