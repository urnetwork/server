package controller

// The Zcash wallet daemon: zecd 0.8.x / 0.9.x, watch-only from the account's
// unified full viewing key, on our own Zebra
// (mmm/ZCASH-WALLET-DAEMONS.md Part A).
//
// What the wire looks like, and what this client does about it:
//   - bitcoind's JSON-RPC dialect: params are a positional ARRAY (an object is
//     silently zero arguments), HTTP basic auth, and an rpc error answered
//     with 400, 404 or 500 and the json envelope (crypto_pay_wallet.go).
//   - Amounts are JSON numbers in ZEC with 8 decimals: decoded as json.Number
//     and converted to zatoshi exactly, or taken from z_listtransactions'
//     integer amountZat.
//   - A diversifier index can exceed 2^64: decoded as json.Number. The ones
//     zecd picks are about 4.6e9; a larger one names no address of ours.
//   - zecd reports the single receiver that was paid, not the unified address
//     we handed out, so receipts are matched by diversifier index, and keyed
//     by (txid, diversifier index) with every output to it summed. The pool
//     label is data: a mempool receipt reads "orchard" and the mined one
//     "ironwood".
//   - confirmations is 0 unmined, -1 once an unmined transaction expired (it
//     no longer counts), and can fall back to 0 after a reorg.
//   - Addresses are unified addresses with an Orchard and a Sapling receiver
//     and no transparent one (decision 4). A transparent receipt is never
//     counted.

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// z_listtransactions page size.
const zcashListPageSize = 1000

// zecd's rpc error for an unknown or non-wallet txid.
const zcashRpcErrorInvalidAddressOrKey = -5

// The most blocks zebrad's tip may trail zebrad's own estimate of the
// network height (extrapolated from the tip's time) before that estimate is
// taken as the network height: about two hours of 75-second blocks, 42
// minutes of NU7's 25-second ones. Block times never stretch that far by
// chance (a gap of 100 mean block times has probability e^-100), so a slow
// block cannot stop sales; a zebrad stuck or still syncing does.
const zcashNodeStaleBlocks = 100

type zcashWallet struct {
	wallet *cryptoRpcClient
	// zebrad, for the node and network heights (required to sell zec except
	// on regtest; Zebra's cookie auth must be off on its loopback listener,
	// or its cookie given as the user and password)
	node    *cryptoRpcClient
	network string
}

func newZcashWallet(settings *cryptoWalletSettings, assetConfig *CryptoPayAssetConfig) *zcashWallet {
	wallet := &zcashWallet{
		wallet:  newCryptoRpcClient(settings.WalletRpcUrl, settings.WalletRpcUser, settings.WalletRpcPass, cryptoRpcAuthBasic, "1.0"),
		network: assetConfig.Network,
	}
	if settings.NodeRpcUrl != "" {
		wallet.node = newCryptoRpcClient(settings.NodeRpcUrl, settings.NodeRpcUser, settings.NodeRpcPass, cryptoRpcAuthBasic, "1.0")
	}
	return wallet
}

// zcashChainName is getblockchaininfo's chain for a configured network.
func zcashChainName(network string) string {
	switch network {
	case cryptoNetworkTestnet:
		return "test"
	case cryptoNetworkRegtest:
		return "regtest"
	default:
		return "main"
	}
}

// zcashUnifiedAddressPrefix is a unified address's human-readable part with
// its separator on the network.
func zcashUnifiedAddressPrefix(network string) string {
	switch network {
	case cryptoNetworkTestnet:
		return "utest1"
	case cryptoNetworkRegtest:
		return "uregtest1"
	default:
		return "u1"
	}
}

// zcashParseIndex reads a diversifier index. ok = false when it does not fit
// an int64 (and so is no index we ever issue).
func zcashParseIndex(number json.Number) (uint64, bool) {
	value, ok := new(big.Int).SetString(number.String(), 10)
	if !ok || value.Sign() < 0 || !value.IsInt64() {
		return 0, false
	}
	return value.Uint64(), true
}

// zcashParseZec converts an 8-decimal ZEC amount to zatoshi exactly.
func zcashParseZec(number json.Number) (int64, error) {
	value, ok := new(big.Rat).SetString(number.String())
	if !ok {
		return 0, fmt.Errorf("unreadable amount")
	}
	value.Mul(value, new(big.Rat).SetInt64(100_000_000))
	if !value.IsInt() || !value.Num().IsInt64() {
		return 0, fmt.Errorf("amount is not a whole number of zatoshi")
	}
	return value.Num().Int64(), nil
}

// zcashPoolFamily groups the pool labels of one kind of note: an output with
// no address row is keyed by it, so its key holds when the label changes
// from "orchard" (mempool) to "ironwood" (mined).
func zcashPoolFamily(pool string) string {
	switch pool {
	case "orchard", "ironwood":
		return "o"
	case "sapling":
		return "s"
	case "transparent":
		return "t"
	}
	return pool
}

// NewAddress derives the next unified address of account 0 with a Sapling
// and an Orchard receiver (Ironwood notes arrive at Orchard receivers) and no
// transparent one. zecd stores no labels, so label is not sent.
func (self *zcashWallet) NewAddress(ctx context.Context, label string) (string, uint64, error) {
	var result struct {
		Account          json.Number `json:"account"`
		DiversifierIndex json.Number `json:"diversifier_index"`
		ReceiverTypes    []string    `json:"receiver_types"`
		Address          string      `json:"address"`
	}
	// the index must stay omitted: about half of all Sapling diversifiers are
	// invalid, and an explicit index with a Sapling receiver answers -4 for
	// those; zecd's own (clock-based) choice skips them (mmm/ZCASH-WALLET-DAEMONS.md §A.5)
	if err := self.wallet.call(ctx, "z_getaddressforaccount", []any{0, []string{"sapling", "orchard"}}, &result); err != nil {
		return "", 0, err
	}
	index, ok := zcashParseIndex(result.DiversifierIndex)
	if !ok {
		return "", 0, fmt.Errorf("z_getaddressforaccount answered an unusable diversifier index")
	}
	for _, receiverType := range result.ReceiverTypes {
		if receiverType != "sapling" && receiverType != "orchard" {
			return "", 0, fmt.Errorf("z_getaddressforaccount answered a %s receiver", receiverType)
		}
	}
	if !strings.HasPrefix(result.Address, zcashUnifiedAddressPrefix(self.network)) || !cryptoUriAddressOk(result.Address) {
		return "", 0, fmt.Errorf("z_getaddressforaccount answered an address that is not a %s unified address", self.network)
	}
	return result.Address, index, nil
}

// zcashTxEntry is one z_listtransactions entry (receive side).
type zcashTxEntry struct {
	TxId             string       `json:"txid"`
	Status           string       `json:"status"`
	Confirmations    json.Number  `json:"confirmations"`
	Time             json.Number  `json:"time"`
	Pool             string       `json:"pool"`
	DiversifierIndex *json.Number `json:"diversifier_index"`
	Category         string       `json:"category"`
	AmountZat        json.Number  `json:"amountZat"`
	Address          string       `json:"address"`
	OutIndex         json.Number  `json:"outindex"`
	BlockHeight      *json.Number `json:"blockheight"`
	BlockTime        *json.Number `json:"blocktime"`
}

// zcashOutput is one received output, before receipts are summed.
type zcashOutput struct {
	txId             string
	vout             string
	pool             string
	diversifierIndex *json.Number
	address          string
	amountZatoshi    int64
	confirmations    int64
	blockHeight      uint64
	timestamp        time.Time
}

// zcashReceipts sums received outputs into receipts, one per transaction and
// diversifier index; a transparent output is a receipt of its own, keyed by
// its output index, and never counted. Expired unmined outputs are dropped.
//
// One listing can hold the same output twice: z_listtransactions is read in
// pages, and a block arriving between two pages shifts the entries, so an
// output read unmined ("orchard") on one page can be read again mined
// ("ironwood") on the next (research trap 10). So a transaction mined in any
// entry is taken as mined and its unmined entries are dropped as stale. A
// mined output's pool is settled, and mined outputs are de-duplicated on
// (txid, pool, vout) exactly as reported: an Orchard action and an Ironwood
// action at the same index are two outputs, and both count. An unmined
// output's label is not settled, so unmined outputs are de-duplicated per
// pool family: two unmined outputs at one index in the two bundles read as
// one until they are mined, and the receipt's amount follows the mined
// listing then (it is updated until the receipt is credited, and nothing is
// credited unmined).
func zcashReceipts(outputs []*zcashOutput) []*cryptoWalletReceipt {
	byKey := map[string]*cryptoWalletReceipt{}
	order := []string{}
	pools := map[string]map[string]bool{}
	seenOutputs := map[string]bool{}
	mined := map[string]bool{}
	for _, output := range outputs {
		if 0 < output.blockHeight {
			mined[output.txId] = true
		}
	}
	for _, output := range outputs {
		if output.confirmations < 0 {
			// an unmined transaction that expired: it will never be mined
			continue
		}
		if mined[output.txId] && output.blockHeight == 0 {
			continue
		}
		label := output.pool
		if output.blockHeight == 0 {
			label = zcashPoolFamily(output.pool)
		}
		outputKey := output.txId + "/" + label + "/" + output.vout
		if seenOutputs[outputKey] {
			continue
		}
		seenOutputs[outputKey] = true

		var key string
		var addressIndex *uint64
		switch {
		case output.pool == cryptoZcashPoolTransparent:
			key = "t:" + output.vout
		case output.diversifierIndex != nil:
			key = output.diversifierIndex.String()
			if index, ok := zcashParseIndex(*output.diversifierIndex); ok {
				addressIndex = &index
			}
		default:
			// a shielded output linked to no address row
			key = "n:" + zcashPoolFamily(output.pool) + ":" + output.vout
		}
		receiptKey := output.txId + "/" + key
		receipt, ok := byKey[receiptKey]
		if !ok {
			receipt = &cryptoWalletReceipt{
				TxId:          output.txId,
				OutputKey:     key,
				Address:       output.address,
				AddressIndex:  addressIndex,
				BlockHeight:   output.blockHeight,
				Confirmations: uint64(output.confirmations),
				Timestamp:     output.timestamp,
			}
			byKey[receiptKey] = receipt
			order = append(order, receiptKey)
			pools[receiptKey] = map[string]bool{}
		} else if receipt.Timestamp.IsZero() || (!output.timestamp.IsZero() && output.timestamp.Before(receipt.Timestamp)) {
			// the earliest time any output of it was seen
			receipt.Timestamp = output.timestamp
		}
		receipt.AmountAtomic += uint64(output.amountZatoshi)
		pools[receiptKey][output.pool] = true
	}
	receipts := make([]*cryptoWalletReceipt, 0, len(order))
	for _, receiptKey := range order {
		receipt := byKey[receiptKey]
		names := []string{}
		for _, pool := range []string{"transparent", "sapling", "orchard", "ironwood"} {
			if pools[receiptKey][pool] {
				names = append(names, pool)
			}
		}
		receipt.Pool = strings.Join(names, ",")
		receipts = append(receipts, receipt)
	}
	return receipts
}

func zcashJsonInt(number *json.Number) int64 {
	if number == nil {
		return 0
	}
	value, err := number.Int64()
	if err != nil {
		return 0
	}
	return value
}

// zcashOutputOfEntry is one z_listtransactions receive entry as an output.
func zcashOutputOfEntry(entry *zcashTxEntry) (*zcashOutput, error) {
	amount, err := entry.AmountZat.Int64()
	if err != nil {
		return nil, fmt.Errorf("unreadable amountZat")
	}
	confirmations, err := entry.Confirmations.Int64()
	if err != nil {
		return nil, fmt.Errorf("unreadable confirmations")
	}
	if entry.Status == "expired" {
		confirmations = -1
	}
	output := &zcashOutput{
		txId:             strings.ToLower(entry.TxId),
		vout:             entry.OutIndex.String(),
		pool:             entry.Pool,
		diversifierIndex: entry.DiversifierIndex,
		address:          entry.Address,
		amountZatoshi:    amount,
		confirmations:    confirmations,
	}
	if entry.BlockHeight != nil && 0 < confirmations {
		output.blockHeight = uint64(zcashJsonInt(entry.BlockHeight))
	}
	if seconds := zcashJsonInt(entry.BlockTime); 0 < seconds {
		output.timestamp = time.Unix(seconds, 0).UTC()
	} else if seconds, err := entry.Time.Int64(); err == nil && 0 < seconds {
		output.timestamp = time.Unix(seconds, 0).UTC()
	}
	return output, nil
}

// IncomingSince lists every receive mined at or above minHeight, and the
// unmined ones.
func (self *zcashWallet) IncomingSince(ctx context.Context, minHeight uint64) ([]*cryptoWalletReceipt, uint64, error) {
	walletHeight, _, err := self.chainInfo(ctx)
	if err != nil {
		return nil, 0, err
	}
	outputs := []*zcashOutput{}
	for from := 0; ; from += zcashListPageSize {
		var entries []*zcashTxEntry
		// count, from, includeWatchonly, start_height (inclusive); the end
		// left open, so unmined transactions are included
		if err := self.wallet.call(ctx, "z_listtransactions", []any{zcashListPageSize, from, nil, minHeight}, &entries); err != nil {
			return nil, 0, err
		}
		for _, entry := range entries {
			if entry == nil || entry.Category != "receive" {
				continue
			}
			output, err := zcashOutputOfEntry(entry)
			if err != nil {
				return nil, 0, fmt.Errorf("z_listtransactions: %w", err)
			}
			outputs = append(outputs, output)
		}
		if len(entries) < zcashListPageSize {
			break
		}
		if 1_000_000 <= from {
			return nil, 0, fmt.Errorf("z_listtransactions: too many entries since height %d", minHeight)
		}
	}
	return zcashReceipts(outputs), walletHeight, nil
}

// Transaction re-reads one transaction's received outputs. found = false when
// the wallet no longer has it, or it expired unmined.
func (self *zcashWallet) Transaction(ctx context.Context, txId string) ([]*cryptoWalletReceipt, bool, error) {
	var result struct {
		Confirmations json.Number  `json:"confirmations"`
		BlockHeight   *json.Number `json:"blockheight"`
		BlockTime     *json.Number `json:"blocktime"`
		Time          json.Number  `json:"time"`
		Details       []*struct {
			Address          string       `json:"address"`
			Category         string       `json:"category"`
			Amount           json.Number  `json:"amount"`
			Vout             json.Number  `json:"vout"`
			Pool             string       `json:"pool"`
			DiversifierIndex *json.Number `json:"diversifier_index"`
		} `json:"details"`
	}
	err := self.wallet.call(ctx, "gettransaction", []any{txId}, &result)
	if rpcErr, ok := err.(*cryptoRpcError); ok && rpcErr.Code == zcashRpcErrorInvalidAddressOrKey {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	confirmations, err := result.Confirmations.Int64()
	if err != nil {
		return nil, false, fmt.Errorf("gettransaction: unreadable confirmations")
	}
	if confirmations < 0 {
		return nil, false, nil
	}
	var blockHeight uint64
	if 0 < confirmations {
		blockHeight = uint64(zcashJsonInt(result.BlockHeight))
	}
	var timestamp time.Time
	if seconds := zcashJsonInt(result.BlockTime); 0 < seconds {
		timestamp = time.Unix(seconds, 0).UTC()
	}
	outputs := []*zcashOutput{}
	for _, detail := range result.Details {
		if detail == nil || detail.Category != "receive" {
			continue
		}
		amount, err := zcashParseZec(detail.Amount)
		if err != nil {
			return nil, false, fmt.Errorf("gettransaction: %w", err)
		}
		outputs = append(outputs, &zcashOutput{
			txId:             strings.ToLower(txId),
			vout:             detail.Vout.String(),
			pool:             detail.Pool,
			diversifierIndex: detail.DiversifierIndex,
			address:          detail.Address,
			amountZatoshi:    amount,
			confirmations:    confirmations,
			blockHeight:      blockHeight,
			timestamp:        timestamp,
		})
	}
	receipts := zcashReceipts(outputs)
	return receipts, 0 < len(receipts), nil
}

// chainInfo is zecd's getblockchaininfo: the wallet's fully scanned height
// (blocks) and the Zebra tip it last saw (headers).
func (self *zcashWallet) chainInfo(ctx context.Context) (uint64, uint64, error) {
	var info struct {
		Chain   string      `json:"chain"`
		Blocks  json.Number `json:"blocks"`
		Headers json.Number `json:"headers"`
	}
	if err := self.wallet.call(ctx, "getblockchaininfo", []any{}, &info); err != nil {
		return 0, 0, err
	}
	if info.Chain != zcashChainName(self.network) {
		return 0, 0, fmt.Errorf("zecd serves the %q chain, not %s", info.Chain, self.network)
	}
	blocks, err := info.Blocks.Int64()
	if err != nil || blocks < 0 {
		return 0, 0, fmt.Errorf("getblockchaininfo: unreadable blocks")
	}
	headers, err := info.Headers.Int64()
	if err != nil || headers < blocks {
		headers = blocks
	}
	return uint64(blocks), uint64(headers), nil
}

// Heights is the wallet's scanned height, zebrad's tip, and the network's
// height. The network's is zebrad's tip, not its estimatedheight, which is
// extrapolated from the tip's time and runs ahead during every slow block
// (and is meaningless on regtest, whose blocks keep the genesis time): only
// when the tip trails the estimate by zcashNodeStaleBlocks (zebrad stuck or
// still syncing) is the estimate taken. Without zebrad the network's height
// is unknown (0) and the coin is not sold (mmm/ZCASH-CONTRACT.md §3.13.10), except on
// regtest, where zecd's view of Zebra's tip stands in.
func (self *zcashWallet) Heights(ctx context.Context) (uint64, uint64, uint64, error) {
	walletHeight, headers, err := self.chainInfo(ctx)
	if err != nil {
		return 0, 0, 0, err
	}
	if self.node == nil {
		if self.network == cryptoNetworkRegtest {
			return walletHeight, headers, headers, nil
		}
		return walletHeight, 0, 0, nil
	}
	var info struct {
		Blocks          json.Number  `json:"blocks"`
		EstimatedHeight *json.Number `json:"estimatedheight"`
	}
	if err := self.node.call(ctx, "getblockchaininfo", []any{}, &info); err != nil {
		return 0, 0, 0, err
	}
	blocks, err := info.Blocks.Int64()
	if err != nil || blocks <= 0 {
		// a zebrad with no chain says nothing
		return walletHeight, 0, 0, nil
	}
	nodeHeight := uint64(blocks)
	networkHeight := nodeHeight
	if self.network != cryptoNetworkRegtest {
		if estimated := zcashJsonInt(info.EstimatedHeight); 0 < estimated && nodeHeight+zcashNodeStaleBlocks < uint64(estimated) {
			networkHeight = uint64(estimated)
		}
	}
	return walletHeight, nodeHeight, networkHeight, nil
}

// CheckNetwork refuses a zecd serving another chain than the configured
// network.
func (self *zcashWallet) CheckNetwork(ctx context.Context) error {
	_, _, err := self.chainInfo(ctx)
	return err
}

// CheckAddress derives the unified address of a pool index again and
// refuses a zecd that derives another one: it watches another wallet (another
// UFVK) than the pool was made from (mmm/ZCASH-CONTRACT.md §3.13.13). An explicit index
// is safe here: zecd answers -4 only for a Sapling diversifier index it never
// issued, and every pool index is one it issued.
func (self *zcashWallet) CheckAddress(ctx context.Context, index uint64, address string) error {
	var result struct {
		Address string `json:"address"`
	}
	if err := self.wallet.call(ctx, "z_getaddressforaccount", []any{0, []string{"sapling", "orchard"}, index}, &result); err != nil {
		return err
	}
	if result.Address != address {
		return fmt.Errorf("zecd derives another address at diversifier index %d than the pool holds: it watches another wallet", index)
	}
	return nil
}

var _ cryptoWallet = (*zcashWallet)(nil)
var _ cryptoWalletNetworkCheck = (*zcashWallet)(nil)
var _ cryptoWalletAddressCheck = (*zcashWallet)(nil)
