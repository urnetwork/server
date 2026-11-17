package controller

// A Solana RPC serving receivers' USDC token accounts and their histories, for
// the receiver leg's tests (solana_receiver_sweep_test.go,
// solana_receiver_sweep_db_test.go). Each history is a list of transactions,
// newest first, served by getSignaturesForAddress with its until, before and
// limit, and each transaction by getTransaction in the jsonParsed shape the
// reconcile reads (pre and post token balances, account keys). Every account,
// signature and amount is synthetic. Hermetic: no network.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"testing"
	"time"
)

// The SPL token program, which moves USDC.
const solanaReceiverFakeTokenProgram = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"

// One transaction of a token account's history.
type solanaReceiverFakeTransaction struct {
	signature string
	slot      uint64
	blockTime time.Time
	// a failed transaction: listed with an err, moved nothing
	failed bool
	// what getTransaction serves; nil serves null
	parsed map[string]any
}

// Serves token accounts by owner, their histories and their transactions, and
// counts the calls. Not safe for concurrent use: the leg calls it from one
// goroutine.
type fakeSolanaReceiverRpc struct {
	t testing.TB
	// owner -> its USDC token accounts
	ownerTokenAccounts map[string][]string
	// token account -> its transactions, newest first
	tokenAccountHistories map[string][]*solanaReceiverFakeTransaction
	// method -> an error every call of it answers
	methodErrs  map[string]error
	methodCalls map[string]int
	// the signatures getTransaction was asked for, in order
	readSignatures []string
}

// An empty fake for one test.
func newFakeSolanaReceiverRpc(t testing.TB) *fakeSolanaReceiverRpc {
	return &fakeSolanaReceiverRpc{
		t:                     t,
		ownerTokenAccounts:    map[string][]string{},
		tokenAccountHistories: map[string][]*solanaReceiverFakeTransaction{},
		methodErrs:            map[string]error{},
		methodCalls:           map[string]int{},
	}
}

// Decodes value into result through JSON, as the RPC client does.
func (self *fakeSolanaReceiverRpc) answer(value any, result any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, result)
}

// One JSON-RPC call: getTokenAccountsByOwner, getSignaturesForAddress and
// getTransaction, each checked for the commitment and encoding the reconcile
// asks for.
func (self *fakeSolanaReceiverRpc) call(ctx context.Context, method string, params any, result any) error {
	self.methodCalls[method] += 1
	if err := self.methodErrs[method]; err != nil {
		return err
	}
	args, _ := params.([]any)
	switch method {
	case "getTokenAccountsByOwner":
		owner, _ := args[0].(string)
		filter, _ := args[1].(map[string]any)
		if filter["mint"] != solanaUsdcMint {
			return fmt.Errorf("getTokenAccountsByOwner asked for mint %v", filter["mint"])
		}
		values := []any{}
		for _, tokenAccount := range self.ownerTokenAccounts[owner] {
			values = append(values, map[string]any{"pubkey": tokenAccount, "account": map[string]any{}})
		}
		return self.answer(map[string]any{"context": map[string]any{"slot": 1}, "value": values}, result)
	case "getSignaturesForAddress":
		address, _ := args[0].(string)
		options, _ := args[1].(map[string]any)
		if options["commitment"] != "finalized" {
			return fmt.Errorf("getSignaturesForAddress asked at commitment %v", options["commitment"])
		}
		limit := 1000
		if value, ok := options["limit"].(int); ok {
			limit = value
		}
		untilSignature, _ := options["until"].(string)
		beforeSignature, _ := options["before"].(string)
		history := self.tokenAccountHistories[address]
		start := 0
		if beforeSignature != "" {
			start = len(history)
			for i, transaction := range history {
				if transaction.signature == beforeSignature {
					start = i + 1
					break
				}
			}
		}
		pageValues := []any{}
		for _, transaction := range history[start:] {
			if transaction.signature == untilSignature || len(pageValues) == limit {
				break
			}
			var transactionErr any
			if transaction.failed {
				transactionErr = map[string]any{"InstructionError": []any{0, "InvalidArgument"}}
			}
			pageValues = append(pageValues, map[string]any{
				"signature":          transaction.signature,
				"slot":               transaction.slot,
				"err":                transactionErr,
				"blockTime":          transaction.blockTime.Unix(),
				"confirmationStatus": "finalized",
			})
		}
		return self.answer(pageValues, result)
	case "getTransaction":
		signature, _ := args[0].(string)
		options, _ := args[1].(map[string]any)
		if options["encoding"] != "jsonParsed" || options["commitment"] != "finalized" {
			return fmt.Errorf("getTransaction asked for %v at %v", options["encoding"], options["commitment"])
		}
		self.readSignatures = append(self.readSignatures, signature)
		for _, history := range self.tokenAccountHistories {
			for _, transaction := range history {
				if transaction.signature == signature {
					return self.answer(transaction.parsed, result)
				}
			}
		}
		return self.answer(nil, result)
	}
	return fmt.Errorf("unexpected method %s", method)
}

// One USDC token balance entry of a parsed transaction.
func solanaReceiverFakeBalance(index int, owner string, amountMicro int64) map[string]any {
	return map[string]any{
		"accountIndex": index,
		"mint":         solanaUsdcMint,
		"owner":        owner,
		"programId":    solanaReceiverFakeTokenProgram,
		"uiTokenAmount": map[string]any{
			"amount":   strconv.FormatInt(amountMicro, 10),
			"decimals": 6,
		},
	}
}

// A finalized USDC transfer of amountMicro from sender (out of
// senderTokenAccount) to receiver (into receiverTokenAccount), with extra
// account keys (a Solana Pay reference is one). A transfer out of one of our
// receivers is the same shape with our receiver as the sender.
func solanaReceiverFakeTransfer(
	signature string,
	slot uint64,
	blockTime time.Time,
	sender string,
	senderTokenAccount string,
	receiver string,
	receiverTokenAccount string,
	amountMicro int64,
	references ...string,
) *solanaReceiverFakeTransaction {
	const senderBeforeMicro = 1_000_000_000
	accountKeys := []any{
		map[string]any{"pubkey": sender, "signer": true},
		map[string]any{"pubkey": senderTokenAccount, "signer": false},
		map[string]any{"pubkey": receiverTokenAccount, "signer": false},
		map[string]any{"pubkey": solanaUsdcMint, "signer": false},
		map[string]any{"pubkey": solanaReceiverFakeTokenProgram, "signer": false},
	}
	for _, reference := range references {
		accountKeys = append(accountKeys, map[string]any{"pubkey": reference, "signer": false})
	}
	return &solanaReceiverFakeTransaction{
		signature: signature,
		slot:      slot,
		blockTime: blockTime,
		parsed: map[string]any{
			"slot":      slot,
			"blockTime": blockTime.Unix(),
			"meta": map[string]any{
				"err":               nil,
				"innerInstructions": []any{},
				"preTokenBalances": []any{
					solanaReceiverFakeBalance(1, sender, senderBeforeMicro),
					solanaReceiverFakeBalance(2, receiver, 0),
				},
				"postTokenBalances": []any{
					solanaReceiverFakeBalance(1, sender, senderBeforeMicro-amountMicro),
					solanaReceiverFakeBalance(2, receiver, amountMicro),
				},
			},
			"transaction": map[string]any{
				"signatures": []any{signature},
				"message": map[string]any{
					"accountKeys": accountKeys,
					"instructions": []any{
						map[string]any{
							"program":   "spl-token",
							"programId": solanaReceiverFakeTokenProgram,
							"parsed": map[string]any{
								"type": "transferChecked",
								"info": map[string]any{
									"authority":   sender,
									"source":      senderTokenAccount,
									"destination": receiverTokenAccount,
									"mint":        solanaUsdcMint,
									"tokenAmount": map[string]any{"amount": strconv.FormatInt(amountMicro, 10), "decimals": 6},
								},
							},
						},
					},
				},
			},
		},
	}
}
