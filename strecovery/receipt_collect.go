// A production collection reads every archived candidate under one explicit
// EVM boundary, builds committed proofs, then replays the offline verifier.
// The external native boundary/mapping and runtime fees remain unapproved.
package strecovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
)

// Calls borrow immutable archive/config values. Each invocation owns all
// network state and stops within fifteen minutes even without a caller timeout.
func CollectReceiptEvidence(ctx context.Context, archive *Archive, config ReceiptCollectionConfig) (*ReceiptCollection, error) {
	if ctx == nil {
		return nil, errors.New("receipt collection context is absent")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	if err := archive.Validate(ctx); err != nil {
		return nil, err
	}
	if err := config.validate(archive); err != nil {
		return nil, err
	}
	client := newReceiptCollectorRpc(config.RpcUrl)
	if config.RetryWindowSeconds != 0 {
		client.retryWindow = time.Duration(config.RetryWindowSeconds) * time.Second
	}
	defer client.client.CloseIdleConnections()
	return collectReceiptEvidence(ctx, archive, config, client)
}

// All mutable state is invocation-owned. This internal port permits local
// deterministic transport controls; the public command supplies only HTTP.
func collectReceiptEvidence(ctx context.Context, archive *Archive, config ReceiptCollectionConfig, client *receiptCollectorRpc) (*ReceiptCollection, error) {
	if err := client.network(ctx, archive, config); err != nil {
		return nil, err
	}
	if _, err := client.canonicalBlock(ctx, config.EvmFinalized); err != nil {
		return nil, err
	}
	observations := &ReceiptObservations{Schema: ReceiptObservationsSchema, CensusHash: archive.CensusHash, ChainId: archive.Selection.ChainId, Genesis: archive.Selection.Genesis,
		Source: config.Source, NativeFinalized: config.NativeFinalized, EvmFinalized: config.EvmFinalized, MappingEvidenceHash: config.MappingEvidenceHash,
		Blocks: []ObservedCanonicalBlock{}, Accounts: []ObservedAccount{}, Receipts: []ReceiptObservation{}}
	commitments := &ReceiptCommitments{Schema: ReceiptCommitmentsSchema, CensusHash: archive.CensusHash, Headers: []string{}, Receipts: []ReceiptInclusionProof{}}
	firstNumber := config.EvmFinalized.Number
	blockReceiptIndexes := map[uint64][]int{}
	for _, transaction := range archive.Transactions {
		raw, err := client.call(ctx, "eth_getTransactionReceipt", []any{transaction.Hash})
		if err != nil {
			return nil, err
		}
		observation := ReceiptObservation{Hash: transaction.Hash, Outcome: "not-found"}
		if !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			receipt, err := collectionReceipt(raw, transaction)
			if err != nil {
				return nil, err
			}
			if receipt.BlockNumber > config.EvmFinalized.Number || config.EvmFinalized.Number-receipt.BlockNumber >= maximumCommitmentHeaders {
				return nil, errors.New("receipt collection inclusion is beyond the selected boundary or ancestry bound")
			}
			observation.Outcome, observation.Receipt = "found", receipt
			firstNumber = min(firstNumber, receipt.BlockNumber)
			blockReceiptIndexes[receipt.BlockNumber] = append(blockReceiptIndexes[receipt.BlockNumber], len(observations.Receipts))
		}
		observations.Receipts = append(observations.Receipts, observation)
	}
	for _, role := range archive.Selection.Roles {
		var encoded string
		if err := client.read(ctx, "eth_getTransactionCount", []any{role.Address, collectionBlockSelector(config.EvmFinalized.Hash)}, &encoded); err != nil {
			return nil, err
		}
		nonce, err := hexutil.DecodeUint64(encoded)
		if err != nil {
			return nil, errors.New("receipt collection account nonce is missing or malformed")
		}
		observations.Accounts = append(observations.Accounts, ObservedAccount{Role: role.Id, Address: role.Address, BlockHash: config.EvmFinalized.Hash, Outcome: "available", Nonce: &nonce})
	}
	// Follow child-authenticated parent hashes backwards. Neither sparse
	// number lookups nor the native height can select these EVM ancestors.
	headerNumberHeaders := map[uint64]*types.Header{}
	hash := config.EvmFinalized.Hash
	remaining := MaximumCommitmentBytes
	for number := config.EvmFinalized.Number; ; number-- {
		var encoded string
		if err := client.read(ctx, "debug_getRawHeader", []any{collectionBlockSelector(hash)}, &encoded); err != nil {
			return nil, err
		}
		header, err := collectionHeader(encoded, hash, number)
		if err != nil {
			return nil, err
		}
		if len(encoded) > remaining {
			return nil, errors.New("receipt collection headers exceed shared proof byte budget")
		}
		remaining -= len(encoded)
		commitments.Headers = append(commitments.Headers, encoded)
		if len(blockReceiptIndexes[number]) != 0 || number == config.EvmFinalized.Number {
			headerNumberHeaders[number] = header
		}
		if number == firstNumber {
			break
		}
		hash = header.ParentHash.Hex()
	}
	slices.Reverse(commitments.Headers)
	blockNumbers := make([]uint64, 0, len(headerNumberHeaders))
	for number := range headerNumberHeaders {
		blockNumbers = append(blockNumbers, number)
	}
	slices.Sort(blockNumbers)
	for _, number := range blockNumbers {
		header := headerNumberHeaders[number]
		block, err := client.canonicalBlock(ctx, ObservedBlockIdentity{Number: number, Hash: header.Hash().Hex()})
		if err != nil {
			return nil, err
		}
		if block.GasUsed != header.GasUsed || block.GasLimit != header.GasLimit {
			return nil, errors.New("receipt collection canonical block gas differs from raw header")
		}
		observations.Blocks = append(observations.Blocks, block)
		indexes := blockReceiptIndexes[number]
		if len(indexes) == 0 {
			continue
		}
		var encodedBlock string
		var encodedReceipts []string
		selector := []any{collectionBlockSelector(block.Hash)}
		if err := client.read(ctx, "debug_getRawBlock", selector, &encodedBlock); err != nil {
			return nil, err
		}
		if err := client.read(ctx, "debug_getRawReceipts", selector, &encodedReceipts); err != nil {
			return nil, err
		}
		transactionTrie, receiptTrie, err := collectionTries(ctx, header, encodedBlock, encodedReceipts)
		if err != nil {
			return nil, err
		}
		for _, observationIndex := range indexes {
			observation := observations.Receipts[observationIndex]
			proof := ReceiptInclusionProof{Hash: observation.Hash, PreviousReceiptNodes: []string{}}
			index := *observation.Receipt.TransactionIndex
			proof.TransactionNodes, err = collectionProof(ctx, transactionTrie, index, &remaining)
			if err != nil {
				return nil, err
			}
			proof.ReceiptNodes, err = collectionProof(ctx, receiptTrie, index, &remaining)
			if err != nil {
				return nil, err
			}
			if index > 0 {
				proof.PreviousReceiptNodes, err = collectionProof(ctx, receiptTrie, index-1, &remaining)
				if err != nil {
					return nil, err
				}
			}
			commitments.Receipts = append(commitments.Receipts, proof)
		}
	}
	// Recheck route/network and selected canonical boundaries only after all
	// bodies and nonces were read. These remain RPC assertions, not consensus.
	if err := client.network(ctx, archive, config); err != nil {
		return nil, err
	}
	if _, err := client.canonicalBlock(ctx, config.EvmFinalized); err != nil {
		return nil, err
	}
	commitments.ObservationHash = objectDigest(observations)
	collection := &ReceiptCollection{Schema: ReceiptCollectionSchema, Admission: "unapproved_observation", Observations: observations, Commitments: commitments}
	collection.ContentHash = collection.hash()
	if _, err := VerifyReceiptCollection(ctx, archive, collection); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return collection, nil
}

// Fixed hash selectors are used for every historic body, header and nonce read.
func collectionBlockSelector(hash string) map[string]any {
	return map[string]any{"blockHash": hash, "requireCanonical": true}
}

// The archive supplies network context; it does not independently approve the
// node, native finalized assertion or relationship between native/EVM hashes.
func (self *receiptCollectorRpc) network(ctx context.Context, archive *Archive, config ReceiptCollectionConfig) error {
	var chainIdHex, chain, genesis, nativeHash string
	for _, read := range []struct {
		method string
		params []any
		target *string
	}{
		{method: "eth_chainId", params: []any{}, target: &chainIdHex},
		{method: "system_chain", params: []any{}, target: &chain},
		{method: "chain_getBlockHash", params: []any{0}, target: &genesis},
		{method: "chain_getBlockHash", params: []any{config.NativeFinalized.Number}, target: &nativeHash},
	} {
		if err := self.read(ctx, read.method, read.params, read.target); err != nil {
			return err
		}
	}
	chainId, err := hexutil.DecodeUint64(chainIdHex)
	if err != nil || chainId != archive.Selection.ChainId || chain != config.NativeChain || genesis != archive.Selection.Genesis || nativeHash != config.NativeFinalized.Hash {
		return errors.New("receipt collection RPC network or supplied native boundary changed")
	}
	return nil
}

// JSON block gas/baseFee are projections only. Exact raw headers separately
// authenticate hash and gas; baseFee retains its explicitly conditional status.
func (self *receiptCollectorRpc) canonicalBlock(ctx context.Context, identity ObservedBlockIdentity) (ObservedCanonicalBlock, error) {
	var reply struct {
		Hash     string  `json:"hash"`
		Number   string  `json:"number"`
		GasUsed  string  `json:"gasUsed"`
		GasLimit string  `json:"gasLimit"`
		BaseFee  *string `json:"baseFeePerGas"`
	}
	if err := self.read(ctx, "eth_getBlockByNumber", []any{fmt.Sprintf("0x%x", identity.Number), false}, &reply); err != nil {
		return ObservedCanonicalBlock{}, err
	}
	number, numberErr := hexutil.DecodeUint64(reply.Number)
	gasUsed, usedErr := hexutil.DecodeUint64(reply.GasUsed)
	gasLimit, limitErr := hexutil.DecodeUint64(reply.GasLimit)
	if numberErr != nil || usedErr != nil || limitErr != nil || number != identity.Number || reply.Hash != identity.Hash || gasLimit == 0 || gasUsed > gasLimit {
		return ObservedCanonicalBlock{}, errors.New("receipt collection canonical EVM block identity or gas differs")
	}
	block := ObservedCanonicalBlock{Number: number, Hash: reply.Hash, GasUsed: gasUsed, GasLimit: gasLimit}
	if reply.BaseFee != nil {
		value, err := hexutil.DecodeBig(*reply.BaseFee)
		if err != nil {
			return ObservedCanonicalBlock{}, errors.New("receipt collection block base fee is malformed")
		}
		price := value.String()
		block.BaseFeePerGas = &price
	}
	return block, nil
}

// Explicit quantity strings distinguish missing type, status and index from
// their valid zero values. No successful zero-value decode is accepted.
func collectionReceipt(raw []byte, transaction Transaction) (*ObservedReceipt, error) {
	var reply struct {
		Hash              string `json:"transactionHash"`
		Type              string `json:"type"`
		Status            string `json:"status"`
		BlockNumber       string `json:"blockNumber"`
		BlockHash         string `json:"blockHash"`
		Index             string `json:"transactionIndex"`
		GasUsed           string `json:"gasUsed"`
		CumulativeGasUsed string `json:"cumulativeGasUsed"`
		Price             string `json:"effectiveGasPrice"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, errors.New("receipt collection found receipt is malformed")
	}
	var kind, status, number, index, gas, cumulative uint64
	for _, field := range []struct {
		encoded string
		value   *uint64
	}{
		{encoded: reply.Type, value: &kind}, {encoded: reply.Status, value: &status}, {encoded: reply.BlockNumber, value: &number},
		{encoded: reply.Index, value: &index}, {encoded: reply.GasUsed, value: &gas}, {encoded: reply.CumulativeGasUsed, value: &cumulative},
	} {
		value, err := hexutil.DecodeUint64(field.encoded)
		if err != nil {
			return nil, errors.New("receipt collection required receipt quantity is missing or malformed")
		}
		*field.value = value
	}
	price, err := hexutil.DecodeBig(reply.Price)
	if err != nil || reply.Hash != transaction.Hash || !canonicalHex(reply.BlockHash, 32) || kind != uint64(transaction.Type) || status > types.ReceiptStatusSuccessful ||
		number == 0 || index >= maximumCollectionBlockTransactions || gas == 0 || gas > transaction.GasLimit || cumulative < gas {
		return nil, errors.New("receipt collection receipt transaction, inclusion, gas or price differs")
	}
	kindByte := uint8(kind)
	return &ObservedReceipt{TransactionHash: reply.Hash, Type: &kindByte, Status: &status, BlockNumber: number, BlockHash: reply.BlockHash, TransactionIndex: &index,
		GasUsed: gas, CumulativeGasUsed: cumulative, EffectiveGasPrice: price.String()}, nil
}
