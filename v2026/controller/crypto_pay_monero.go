package controller

// The Monero wallet daemon: monero-wallet-rpc v0.18.5.x, restored view-only
// from the primary address and the private view key, on our own monerod
// (mmm/ZCASH-WALLET-DAEMONS.md Part B).
//
// What the wire looks like, and what this client does about it:
//   - JSON-RPC 2.0 with named params at POST /json_rpc, HTTP digest auth (MD5,
//     qop=auth, realm monero-rpc) with the nonce per TCP connection, and every
//     rpc error answered with HTTP 200 (crypto_pay_wallet.go's transport).
//   - Fields equal to their default and empty arrays are left out:
//     get_transfers with nothing to say is `{}`, and a pool entry has no
//     confirmations key. The decoders below take absent as zero.
//   - The server is single-threaded, and get_transfers with pool:true calls
//     monerod first, so calls are made one at a time with long timeouts.
//   - create_address lives only in memory until `store`, so every issue is
//     followed by `store`, and the pool's own high-water mark guards against a
//     wallet that rolled back its counter and hands out an index again.
//   - The primary address (subaddress 0/0) is never issued: change from our
//     own offline spends lands there and looks like an incoming payment.
//   - get_transfers' min_height is exclusive.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/glog/v2026"
)

// The subaddress lookahead is kept at least this far past the highest index
// issued, so a wallet rebuilt from keys still sees every invoice address.
const moneroLookaheadMargin = 1000

// monero-wallet-rpc's default major lookahead (accounts), kept as it is when
// the minor lookahead is widened.
const moneroDefaultMajorLookahead = 50

// The rpc error monero-wallet-rpc answers an unknown txid with.
const moneroRpcErrorWrongTxid = -8

// The rpc error get_address_index answers an invalid address, or one the
// wallet does not own ("Address doesn't belong to the wallet"), with.
const moneroRpcErrorWrongAddress = -2

type moneroWallet struct {
	wallet *cryptoRpcClient
	// monerod, for the node and network heights (required to sell xmr)
	daemon         *cryptoRpcClient
	network        string
	primaryAddress string
	walletPassword string

	stateLock sync.Mutex
	// the pool's highest issued index: an index at or below it is never
	// handed out again
	highWater uint64
}

func newMoneroWallet(settings *cryptoWalletSettings, assetConfig *CryptoPayAssetConfig) *moneroWallet {
	wallet := &moneroWallet{
		wallet:         newCryptoRpcClient(settings.WalletRpcUrl, settings.WalletRpcUser, settings.WalletRpcPass, cryptoRpcAuthDigest, "2.0"),
		network:        assetConfig.Network,
		primaryAddress: settings.PrimaryAddress,
		walletPassword: settings.WalletPassword,
	}
	if settings.NodeRpcUrl != "" {
		auth := cryptoRpcAuthDigest
		wallet.daemon = newCryptoRpcClient(settings.NodeRpcUrl, settings.NodeRpcUser, settings.NodeRpcPass, auth, "2.0")
	}
	return wallet
}

// moneroSubaddressPrefix is the first character of a subaddress on the
// network: 8 on mainnet, 7 on stagenet, B on testnet.
func moneroSubaddressPrefix(network string) string {
	switch network {
	case cryptoNetworkStagenet:
		return "7"
	case cryptoNetworkTestnet:
		return "B"
	default:
		return "8"
	}
}

// moneroCheckSubaddress refuses an address that is not a subaddress of the
// configured network: a wallet on the wrong network must never issue one.
func moneroCheckSubaddress(network string, address string) error {
	if !cryptoUriAddressOk(address) || len(address) != 95 {
		return fmt.Errorf("the wallet returned an address that is not a subaddress")
	}
	if !strings.HasPrefix(address, moneroSubaddressPrefix(network)) {
		return fmt.Errorf("the wallet returned an address of another network than %s", network)
	}
	return nil
}

// SetAddressHighWater tells the client the pool's highest issued index.
func (self *moneroWallet) SetAddressHighWater(index uint64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.highWater = max(self.highWater, index)
}

type moneroCreateAddressResult struct {
	Address        string   `json:"address"`
	AddressIndex   uint64   `json:"address_index"`
	Addresses      []string `json:"addresses"`
	AddressIndices []uint64 `json:"address_indices"`
}

// NewAddress issues the next subaddress of account 0 past the pool's
// high-water mark and stores the wallet, so a restart cannot issue it again.
func (self *moneroWallet) NewAddress(ctx context.Context, label string) (string, uint64, error) {
	self.stateLock.Lock()
	highWater := self.highWater
	self.stateLock.Unlock()

	for attempt := 0; attempt < 3; attempt += 1 {
		var created moneroCreateAddressResult
		if err := self.wallet.call(ctx, "create_address", map[string]any{
			"account_index": 0,
			"label":         label,
		}, &created); err != nil {
			return "", 0, err
		}
		if err := self.wallet.call(ctx, "store", map[string]any{}, nil); err != nil {
			return "", 0, err
		}
		index := created.AddressIndex
		if index == 0 {
			return "", 0, fmt.Errorf("create_address answered the primary address")
		}
		if index <= highWater {
			// the wallet's counter is behind the pool (a wallet restored from
			// keys, or a crash before a store): burn the indices up to the
			// high-water mark in one call and take the next
			skip := highWater - index
			if 65536 < skip {
				skip = 65536
			}
			if 0 < skip {
				if err := self.wallet.call(ctx, "create_address", map[string]any{
					"account_index": 0,
					"count":         skip,
					"label":         label,
				}, nil); err != nil {
					return "", 0, err
				}
				if err := self.wallet.call(ctx, "store", map[string]any{}, nil); err != nil {
					return "", 0, err
				}
			}
			glog.Warningf("[cryptopay]xmr wallet issued index %d at or below the pool's %d; skipped ahead\n", index, highWater)
			continue
		}
		if err := moneroCheckSubaddress(self.network, created.Address); err != nil {
			return "", 0, err
		}
		self.SetAddressHighWater(index)
		return created.Address, index, nil
	}
	return "", 0, fmt.Errorf("the xmr wallet kept issuing indices below the pool's high-water mark")
}

// EnsureLookahead makes sure the wallet scans the pool's highest address. A
// wallet that created it does; a wallet rebuilt from keys scans only its
// default lookahead (200 subaddresses) and misses every payment beyond it.
// get_address_index answers which: when the wallet does not own the address,
// the lookahead is widened past it with the wallet file's password, and the
// error is logged (payments it already missed need a rescan). The check runs
// on every refill and nothing of it is remembered by the process, so a
// wallet rebuilt behind a running server is caught on the next refill
// (mmm/ZCASH-CONTRACT.md §3.13.9).
func (self *moneroWallet) EnsureLookahead(ctx context.Context, highestIndex uint64, highestAddress string) error {
	var owned struct {
		Index moneroSubaddrIndex `json:"index"`
	}
	err := self.wallet.call(ctx, "get_address_index", map[string]any{"address": highestAddress}, &owned)
	if err == nil {
		if owned.Index.Major != 0 || uint64(owned.Index.Minor) != highestIndex {
			return fmt.Errorf("the xmr wallet places the pool's address %d at %d.%d", highestIndex, owned.Index.Major, owned.Index.Minor)
		}
		return nil
	}
	rpcErr, ok := err.(*cryptoRpcError)
	if !ok || rpcErr.Code != moneroRpcErrorWrongAddress || !strings.Contains(strings.ToLower(rpcErr.Message), "belong") {
		return err
	}
	if self.walletPassword == "" {
		return fmt.Errorf("the xmr wallet does not see pool address %d, and monero.yml has no wallet_password to widen its lookahead", highestIndex)
	}
	want := min(highestIndex+moneroLookaheadMargin, 1<<32-1)
	glog.Errorf(
		"[cryptopay]the xmr wallet does not see pool address %d (rebuilt from keys?); widening its subaddress lookahead to %d. Payments it missed need rescan_blockchain\n",
		highestIndex, want,
	)
	return self.wallet.call(ctx, "set_subaddress_lookahead", map[string]any{
		"password":  self.walletPassword,
		"major_idx": moneroDefaultMajorLookahead,
		"minor_idx": want,
	}, nil)
}

type moneroSubaddrIndex struct {
	Major uint32 `json:"major"`
	Minor uint32 `json:"minor"`
}

// moneroTransfer is one get_transfers / get_transfer_by_txid entry. Absent
// fields decode as zero, which is what monero-wallet-rpc means by leaving them
// out.
type moneroTransfer struct {
	TxId            string             `json:"txid"`
	Height          uint64             `json:"height"`
	Timestamp       uint64             `json:"timestamp"`
	Amount          uint64             `json:"amount"`
	Type            string             `json:"type"`
	UnlockTime      uint64             `json:"unlock_time"`
	SubaddrIndex    moneroSubaddrIndex `json:"subaddr_index"`
	Address         string             `json:"address"`
	DoubleSpendSeen bool               `json:"double_spend_seen"`
	Confirmations   uint64             `json:"confirmations"`
}

type moneroGetTransfersResult struct {
	In   []*moneroTransfer `json:"in"`
	Pool []*moneroTransfer `json:"pool"`
}

// moneroReceipt is a transfer entry as a receipt. A mined entry wins over
// the pool copy of the same transaction and subaddress.
func moneroReceipt(transfer *moneroTransfer) *cryptoWalletReceipt {
	receipt := &cryptoWalletReceipt{
		TxId:          strings.ToLower(transfer.TxId),
		OutputKey:     fmt.Sprintf("%d.%d", transfer.SubaddrIndex.Major, transfer.SubaddrIndex.Minor),
		Address:       transfer.Address,
		AmountAtomic:  transfer.Amount,
		BlockHeight:   transfer.Height,
		Confirmations: transfer.Confirmations,
		UnlockTime:    transfer.UnlockTime,
		DoubleSpend:   transfer.DoubleSpendSeen,
	}
	if transfer.Type == "pool" {
		receipt.BlockHeight = 0
		receipt.Confirmations = 0
	}
	if 0 < transfer.Timestamp {
		receipt.Timestamp = time.Unix(int64(transfer.Timestamp), 0).UTC()
	}
	// only subaddresses of account 0 are ever issued; another account's, and
	// the primary address 0/0, name no invoice
	if transfer.SubaddrIndex.Major == 0 && transfer.SubaddrIndex.Minor != 0 {
		minor := uint64(transfer.SubaddrIndex.Minor)
		receipt.AddressIndex = &minor
	}
	return receipt
}

// moneroReceipts folds in and pool entries into receipts, one per
// (txid, subaddress), preferring the mined copy.
func moneroReceipts(transfers ...[]*moneroTransfer) []*cryptoWalletReceipt {
	byKey := map[string]*cryptoWalletReceipt{}
	order := []string{}
	for _, list := range transfers {
		for _, transfer := range list {
			if transfer == nil || transfer.TxId == "" {
				continue
			}
			switch transfer.Type {
			case "in", "pool", "block":
			default:
				// out, pending and failed are our own spends
				continue
			}
			receipt := moneroReceipt(transfer)
			key := receipt.TxId + "/" + receipt.OutputKey
			if existing, ok := byKey[key]; ok {
				if existing.BlockHeight == 0 && 0 < receipt.BlockHeight {
					byKey[key] = receipt
				}
				continue
			}
			byKey[key] = receipt
			order = append(order, key)
		}
	}
	receipts := make([]*cryptoWalletReceipt, 0, len(order))
	for _, key := range order {
		receipts = append(receipts, byKey[key])
	}
	return receipts
}

// IncomingSince lists every incoming transfer of account 0 mined at or above
// minHeight, and the pool's.
func (self *moneroWallet) IncomingSince(ctx context.Context, minHeight uint64) ([]*cryptoWalletReceipt, uint64, error) {
	walletHeight, err := self.walletHeight(ctx)
	if err != nil {
		return nil, 0, err
	}
	params := map[string]any{
		"in":            true,
		"pool":          true,
		"account_index": 0,
	}
	if 0 < minHeight {
		// min_height is exclusive: min_height < height <= max_height
		params["filter_by_height"] = true
		params["min_height"] = minHeight - 1
	}
	var transfers moneroGetTransfersResult
	if err := self.wallet.call(ctx, "get_transfers", params, &transfers); err != nil {
		return nil, 0, err
	}
	return moneroReceipts(transfers.In, transfers.Pool), walletHeight, nil
}

// Transaction re-reads one transaction's incoming entries. found = false when
// the wallet no longer has it (a dropped or reorged-away transaction).
func (self *moneroWallet) Transaction(ctx context.Context, txId string) ([]*cryptoWalletReceipt, bool, error) {
	var result struct {
		Transfers []*moneroTransfer `json:"transfers"`
	}
	err := self.wallet.call(ctx, "get_transfer_by_txid", map[string]any{
		"txid":          txId,
		"account_index": 0,
	}, &result)
	if rpcErr, ok := err.(*cryptoRpcError); ok && rpcErr.Code == moneroRpcErrorWrongTxid &&
		strings.Contains(strings.ToLower(rpcErr.Message), "not found") {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	receipts := moneroReceipts(result.Transfers)
	return receipts, 0 < len(receipts), nil
}

// walletHeight is the height of the wallet's top scanned block. get_height
// answers the block count (top + 1).
func (self *moneroWallet) walletHeight(ctx context.Context) (uint64, error) {
	var result struct {
		Height uint64 `json:"height"`
	}
	if err := self.wallet.call(ctx, "get_height", map[string]any{}, &result); err != nil {
		return 0, err
	}
	if result.Height == 0 {
		return 0, nil
	}
	return result.Height - 1, nil
}

// Heights is the wallet's top block, and monerod's top block and the height
// it syncs to. Without monerod the node's and the network's are unknown (0),
// and the coin is not sold: the wallet's own height says nothing about how
// far behind it is (mmm/ZCASH-CONTRACT.md §3.13.10). target_height comes from monerod's
// peers, not from block times, so a slow block does not move it.
func (self *moneroWallet) Heights(ctx context.Context) (uint64, uint64, uint64, error) {
	walletHeight, err := self.walletHeight(ctx)
	if err != nil {
		return 0, 0, 0, err
	}
	if self.daemon == nil {
		return walletHeight, 0, 0, nil
	}
	var info struct {
		Height       uint64 `json:"height"`
		TargetHeight uint64 `json:"target_height"`
	}
	if err := self.daemon.call(ctx, "get_info", map[string]any{}, &info); err != nil {
		return 0, 0, 0, err
	}
	if info.Height == 0 {
		// a monerod with no chain says nothing
		return walletHeight, 0, 0, nil
	}
	nodeHeight := info.Height - 1
	// target_height is the chain being synced to; 0 (or below) once synced
	networkHeight := nodeHeight
	if 0 < info.TargetHeight && nodeHeight < info.TargetHeight-1 {
		networkHeight = info.TargetHeight - 1
	}
	return walletHeight, nodeHeight, networkHeight, nil
}

// moneroPrimaryPrefix is the first character of a standard (primary)
// address on the network: 4 on mainnet, 5 on stagenet, 9 on testnet.
func moneroPrimaryPrefix(network string) string {
	switch network {
	case cryptoNetworkStagenet:
		return "5"
	case cryptoNetworkTestnet:
		return "9"
	default:
		return "4"
	}
}

// CheckNetwork refuses a wallet that is not the one the vault names on the
// configured network: the vault's primary address must be of that network,
// and the wallet's own primary address must be it.
func (self *moneroWallet) CheckNetwork(ctx context.Context) error {
	if !strings.HasPrefix(self.primaryAddress, moneroPrimaryPrefix(self.network)) {
		return fmt.Errorf("the vault's primary address is not a %s address", self.network)
	}
	var result struct {
		Address string `json:"address"`
	}
	if err := self.wallet.call(ctx, "get_address", map[string]any{"account_index": 0}, &result); err != nil {
		return err
	}
	if result.Address != self.primaryAddress {
		return fmt.Errorf("the wallet daemon serves another wallet than the vault's primary address")
	}
	return nil
}

var _ cryptoWallet = (*moneroWallet)(nil)
var _ cryptoWalletNetworkCheck = (*moneroWallet)(nil)
var _ cryptoWalletHighWater = (*moneroWallet)(nil)
var _ cryptoWalletLookahead = (*moneroWallet)(nil)
