// Exact RPC logs accompany decoded event projections. A retained log is still
// RPC evidence; it needs an independently verified block/finality authority.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/urnetwork/server"
)

// Distinguish an actual contradictory row from an unavailable database read.
var ErrStEventOriginalIntegrity = errors.New("retained chain event contradicts its original")

// Check a returned original against its routing tuple before durable admission.
func validateStEventOriginal(event *StChainEvent) error {
	if event == nil {
		return ErrStEventOriginalIntegrity
	}
	if len(event.OriginalLog) == 0 {
		return nil
	}
	if len(event.OriginalLog) > 65536 {
		return ErrStEventOriginalIntegrity
	}
	var original types.Log
	if err := json.Unmarshal(event.OriginalLog, &original); err != nil {
		return errors.Join(ErrStEventOriginalIntegrity, err)
	}
	if original.Removed || original.BlockNumber != event.BlockNumber || original.BlockHash.Hex() != event.BlockHash || int(original.Index) != event.LogIndex || original.TxHash.Hex() != event.TxHash {
		return ErrStEventOriginalIntegrity
	}
	return nil
}

// An idempotent replay must match every retained field. Historical rows without
// an original remain missing; a later RPC read cannot manufacture that custody.
func verifyStEventOriginalInTx(ctx context.Context, tx server.PgTx, deploymentKey string, event *StChainEvent) {
	server.Raise(validateStEventOriginal(event))
	rows, err := tx.Query(ctx, `SELECT block_hash,tx_hash,kind,data_json,original_log FROM st_event WHERE deployment_key=$1 AND block_number=$2 AND log_index=$3`, deploymentKey, int64(event.BlockNumber), event.LogIndex)
	server.WithPgResult(rows, err, func() {
		if !rows.Next() {
			panic(ErrStEventOriginalIntegrity)
		}
		var blockHash, txHash, kind, dataJson string
		var originalLog []byte
		server.Raise(rows.Scan(&blockHash, &txHash, &kind, &dataJson, &originalLog))
		if (blockHash != "" && event.BlockHash != "" && blockHash != event.BlockHash) || txHash != event.TxHash || kind != event.Kind || !stEventProjectionEqual(dataJson, event.DataJson) || (len(originalLog) != 0 && len(event.OriginalLog) != 0 && !bytes.Equal(originalLog, event.OriginalLog)) {
			panic(ErrStEventOriginalIntegrity)
		}
	})
}

// Different JSON whitespace/order is not contradictory chain evidence.
func stEventProjectionEqual(left, right string) bool {
	if left == right {
		return true
	}
	if !json.Valid([]byte(left)) || !json.Valid([]byte(right)) {
		return false
	}
	var leftValue, rightValue any
	leftDecoder := json.NewDecoder(bytes.NewBufferString(left))
	leftDecoder.UseNumber()
	rightDecoder := json.NewDecoder(bytes.NewBufferString(right))
	rightDecoder.UseNumber()
	if leftDecoder.Decode(&leftValue) != nil || rightDecoder.Decode(&rightValue) != nil {
		return false
	}
	leftBytes, leftErr := json.Marshal(leftValue)
	rightBytes, rightErr := json.Marshal(rightValue)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftBytes, rightBytes)
}
