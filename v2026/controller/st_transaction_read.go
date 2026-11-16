// Transaction reconciliation retries only reads. Each complete observation
// stays on the selected endpoint and retains one original bounded read owner.
package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// A selected write endpoint is never replaced midway through its receipt or
// nonce proof. No signing, storage mutation or send belongs in this closure.
func (self *CoreStClient) readTransactionRpc(ctx context.Context, client *ethclient.Client, read func(context.Context) error) error {
	if self == nil || client == nil || read == nil {
		return errors.New("st: transaction read owner is absent")
	}
	ctx, stop, err := beginStRpcRead(ctx, self.readHooks)
	if err != nil {
		return err
	}
	defer stop()
	scope := ctx.Value(stRpcReadScopeKey{}).(*stRpcReadScope)
	var last error
	for {
		remaining := scope.deadline.Sub(scope.hooks.now())
		if err := ctx.Err(); err != nil {
			return errors.Join(last, err)
		}
		if remaining <= 0 {
			return errors.Join(last, context.DeadlineExceeded)
		}
		attempt, cancel := context.WithTimeout(ctx, min(stReadAttemptBudget, remaining))
		err := read(attempt)
		if err == nil {
			err = attempt.Err()
		}
		cancel()
		if ownerErr := ctx.Err(); ownerErr != nil {
			return errors.Join(err, ownerErr)
		}
		remaining = scope.deadline.Sub(scope.hooks.now())
		if remaining <= 0 {
			return errors.Join(err, context.DeadlineExceeded)
		}
		if err == nil || !retryableStRpcRead(err) {
			return err
		}
		last = err
		if err := scope.hooks.wait(ctx, min(time.Second, remaining)); err != nil {
			return errors.Join(last, err)
		}
	}
}

// A nonnil finalized boundary means the receipt's exact inclusion survived
// canonical readback and the closing read of that same finalized boundary.
type stTransactionReceiptObservation struct {
	receipt   *types.Receipt
	finalized *stBlockIdentity
	orphaned  bool
}

// Repeats the whole read after transport failure. A hash conflict is hard and
// cannot be laundered by retrying a different endpoint or a later boundary.
func (self *CoreStClient) readTransactionReceipt(ctx context.Context, client *ethclient.Client, hash common.Hash) (*stTransactionReceiptObservation, error) {
	var result *stTransactionReceiptObservation
	var pinnedFinalized *stBlockIdentity
	err := self.readTransactionRpc(ctx, client, func(ctx context.Context) error {
		result = nil
		var raw json.RawMessage
		if err := client.Client().CallContext(ctx, &raw, "eth_getTransactionReceipt", hash); err != nil {
			return err
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			result = &stTransactionReceiptObservation{}
			return nil
		}
		// The Ethereum decoder defaults an absent status to zero. Absence or
		// an unknown value is not a proved revert authorizing another debit.
		var status struct {
			Status *hexutil.Uint64 `json:"status"`
		}
		if err := json.Unmarshal(raw, &status); err != nil {
			return err
		}
		if status.Status == nil || uint64(*status.Status) != types.ReceiptStatusSuccessful && uint64(*status.Status) != types.ReceiptStatusFailed {
			return errors.New("st: receipt execution status is absent or invalid")
		}
		var receipt *types.Receipt
		if err := json.Unmarshal(raw, &receipt); err != nil {
			return err
		}
		if receipt == nil || receipt.TxHash != hash || receipt.BlockHash == (common.Hash{}) || receipt.BlockNumber == nil || !receipt.BlockNumber.IsInt64() || receipt.BlockNumber.Sign() <= 0 {
			return fmt.Errorf("st: receipt response differs from requested transaction %s or has an invalid inclusion block", hash)
		}
		result = &stTransactionReceiptObservation{receipt: receipt}
		if pinnedFinalized == nil {
			var err error
			pinnedFinalized, err = readStRPCBlockIdentity(ctx, client, rpc.FinalizedBlockNumber.String(), nil)
			if err != nil {
				return err
			}
		}
		finalized := pinnedFinalized
		if finalized.Number > math.MaxInt64 {
			return errors.New("st: finalized transaction boundary exceeds postgres bigint")
		}
		if finalized.Number < receipt.BlockNumber.Uint64() {
			return nil
		}
		inclusionNumber := receipt.BlockNumber.Uint64()
		canonical, err := readStRPCBlockIdentity(ctx, client, hexutil.EncodeUint64(inclusionNumber), &inclusionNumber)
		if err != nil {
			return err
		}
		if canonical.Hash != [32]byte(receipt.BlockHash) {
			result.orphaned = true
			return nil
		}
		closing, err := readStRPCBlockIdentity(ctx, client, hexutil.EncodeUint64(finalized.Number), &finalized.Number)
		if err != nil {
			return err
		}
		if closing.Hash != finalized.Hash {
			return errors.New("st: finalized transaction boundary changed during receipt observation")
		}
		result.finalized = finalized
		return nil
	})
	return result, err
}

// A hash-selected canonical nonce cannot borrow another endpoint's finalized
// height. The closing hash also catches a route changing beneath one client.
func (self *CoreStClient) readFinalizedTransactionNonce(ctx context.Context, client *ethclient.Client, from common.Address) (*stBlockIdentity, uint64, error) {
	var finalized *stBlockIdentity
	var nonce hexutil.Uint64
	err := self.readTransactionRpc(ctx, client, func(ctx context.Context) error {
		var err error
		if finalized == nil {
			finalized, err = readStRPCBlockIdentity(ctx, client, rpc.FinalizedBlockNumber.String(), nil)
			if err != nil {
				return err
			}
		}
		if finalized.Number > math.MaxInt64 {
			return errors.New("st: finalized nonce boundary exceeds postgres bigint")
		}
		selector := rpc.BlockNumberOrHashWithHash(common.Hash(finalized.Hash), true)
		if err := client.Client().CallContext(ctx, &nonce, "eth_getTransactionCount", from, selector); err != nil {
			return err
		}
		closing, err := readStRPCBlockIdentity(ctx, client, hexutil.EncodeUint64(finalized.Number), &finalized.Number)
		if err != nil {
			return err
		}
		if closing.Hash != finalized.Hash {
			return errors.New("st: finalized transaction boundary changed during nonce observation")
		}
		return nil
	})
	return finalized, uint64(nonce), err
}
