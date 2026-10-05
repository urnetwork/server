// An actually finalized signed transaction can recover a lost signer reply.
// Recovery matches the original pre-sign reservation and never signs again.
package model

import (
	"context"
	"errors"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/urfoundation/sn/nativefee"
	"github.com/urnetwork/server"
)

// Called only after the owned verifier, denomination and account/intent locks.
// The exact original unsigned digest, kind, nonce and ceiling are immutable;
// expired signing authority or a retired private key is irrelevant to recovery.
func stNativeFeeRecoverSignedOriginal(ctx context.Context, tx server.PgTx, intent *StTransactionIntent, statement nativefee.Statement) (*StTransactionAttempt, error) {
	var signed types.Transaction
	if len(statement.RawTransaction) > stGasMaximumTransactionBytes {
		return nil, errors.New("native fee recovered signature exceeds the original envelope bound")
	}
	if err := signed.UnmarshalBinary(statement.RawTransaction); err != nil {
		return nil, err
	}
	signer := types.LatestSignerForChainID(new(big.Int).SetUint64(intent.ChainId))
	signingHash := strings.ToLower(signer.Hash(&signed).Hex())
	reservation, err := scanStGasReservation(tx.QueryRow(ctx, `SELECT `+stGasReservationColumns+` FROM st_operator_gas_reservation WHERE intent_id=$1 AND attempt=$2`, intent.IntentId, intent.AttemptCount+1))
	if err != nil {
		return nil, err
	}
	if reservation.Historical || reservation.SignedTxHash != nil || reservation.SigningHash != signingHash || reservation.LogicalKey != intent.LogicalKey || intent.AttemptCount >= 3 {
		return nil, errors.New("native fee proved signature has no exact pending original reservation")
	}
	var original types.Transaction
	if err := original.UnmarshalBinary(reservation.UnsignedTransaction); err != nil || strings.ToLower(signer.Hash(&original).Hex()) != signingHash {
		return nil, errors.New("native fee recovered signature differs from the original unsigned bytes")
	}
	attempt := &StTransactionAttempt{IntentId: intent.IntentId, Attempt: reservation.Attempt, Kind: reservation.Kind, TxHash: statement.TransactionHash, RawTransaction: statement.RawTransaction, GasLimit: signed.Gas(), Status: StTxSigned}
	if signed.Type() == types.LegacyTxType {
		price := signed.GasPrice().String()
		attempt.GasPrice = &price
	} else {
		fee, tip := signed.GasFeeCap().String(), signed.GasTipCap().String()
		attempt.GasFeeCap, attempt.GasTipCap = &fee, &tip
	}
	_, liability, err := stGasSignedEnvelope(intent, attempt)
	if err != nil || reservation.MaximumLiabilityWei != liability.String() {
		return nil, errors.Join(errors.New("native fee proved signature changes its original gas ceiling"), err)
	}
	now := server.NowUtc()
	if _, err := tx.Exec(ctx, `INSERT INTO st_transaction_attempt(intent_id,attempt,kind,tx_hash,raw_transaction,gas_limit,gas_price,gas_tip_cap,gas_fee_cap,status,create_time,update_time) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$11)`, intent.IntentId, attempt.Attempt, attempt.Kind, attempt.TxHash, attempt.RawTransaction, int64(attempt.GasLimit), attempt.GasPrice, attempt.GasTipCap, attempt.GasFeeCap, attempt.Status, now); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE st_operator_gas_reservation SET signed_tx_hash=$3 WHERE intent_id=$1 AND attempt=$2`, intent.IntentId, attempt.Attempt, attempt.TxHash); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE st_transaction_intent SET attempt_count=$2,update_time=$3 WHERE intent_id=$1`, intent.IntentId, attempt.Attempt, now); err != nil {
		return nil, err
	}
	intent.AttemptCount = attempt.Attempt
	return attempt, nil
}
