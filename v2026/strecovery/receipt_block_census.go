// Complete Frontier block decoding is shared with the qualified receipt
// collector. This API authenticates byte commitments, not execution or finality.
package strecovery

import (
	"context"
	"errors"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
)

// A direct caller has the same finite aggregate allowance as two raw RPC
// replies. Bounds are checked before hex decoding or building either trie.
const MaximumReceiptBlockCensusEncodedBytes = 32 * 1024 * 1024

// All decoded values belong to the caller. Transaction and receipt order is
// the complete committed order, including transactions outside any local census.
type ReceiptBlockCensus struct {
	Header       *types.Header
	Transactions types.Transactions
	Receipts     types.Receipts
}

// Reuses the collector's exact RLP15, typed-transaction, receipt, gas and trie
// checks. The caller must independently bind expectedHash to its native header
// and establish the required finality/canonicality authority.
func AuthenticateReceiptBlockCensus(ctx context.Context, expectedHash string, expectedNumber uint64, encodedHeader, encodedBlock string, encodedReceipts []string) (*ReceiptBlockCensus, error) {
	if ctx == nil {
		return nil, errors.New("receipt block census context is absent")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if encodedReceipts == nil || len(encodedReceipts) > maximumCollectionBlockTransactions {
		return nil, errors.New("receipt block census complete receipt vector is absent or exceeds bound")
	}
	remaining := MaximumReceiptBlockCensusEncodedBytes
	for _, size := range []int{len(encodedHeader), len(encodedBlock)} {
		if size > remaining {
			return nil, errors.New("receipt block census exceeds aggregate encoded byte bound")
		}
		remaining -= size
	}
	for _, receipt := range encodedReceipts {
		if len(receipt) > remaining {
			return nil, errors.New("receipt block census exceeds aggregate encoded byte bound")
		}
		remaining -= len(receipt)
	}
	header, err := collectionHeader(encodedHeader, expectedHash, expectedNumber)
	if err != nil {
		return nil, err
	}
	transactions, receipts, err := collectionTries(ctx, header, encodedBlock, encodedReceipts)
	if err != nil {
		return nil, err
	}
	result := &ReceiptBlockCensus{Header: header, Transactions: types.Transactions{}, Receipts: types.Receipts{}}
	for index := range encodedReceipts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		key, _ := rlp.EncodeToBytes(uint64(index))
		rawTransaction, transactionErr := transactions.Get(key)
		rawReceipt, receiptErr := receipts.Get(key)
		if err := errors.Join(transactionErr, receiptErr); err != nil {
			return nil, err
		}
		transaction, receipt := new(types.Transaction), new(types.Receipt)
		if err := errors.Join(transaction.UnmarshalBinary(rawTransaction), receipt.UnmarshalBinary(rawReceipt)); err != nil {
			return nil, err
		}
		result.Transactions = append(result.Transactions, transaction)
		result.Receipts = append(result.Receipts, receipt)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
