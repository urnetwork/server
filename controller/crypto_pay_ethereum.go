package controller

// ETH on the crypto payment rail (mmm/BTCPAY.md §17.3, §17.7): each invoice
// has a Coinbase deposit address of its own on Ethereum mainnet, and payments
// are read from the chain through Alchemy alone:
//
//   - the source of truth: each invoice address's incoming transfers,
//     alchemy_getAssetTransfers with toAddress and the external and internal
//     categories (ETH sent by an account, and ETH a contract wallet sends),
//     each paying transaction's receipt status, and the finalized block tag:
//     a payment counts once its block is finalized (one "confirmation"),
//     about 15 minutes;
//   - the fast path: Alchemy's Address Activity webhook (crypto_pay_alchemy.go),
//     when alchemy.yml names it, only brings an address's read forward. New
//     pool addresses are added to it through the Notify API;
//   - the sweep (crypto_pay_sweep.go): every address an invoice was ever
//     given, read once a day from its own cursor through the finalized block
//     (SweepAddress), so a payment to an old or expired invoice is recorded
//     even when its webhook delivery is lost.
//
// A transfer in a block that is reorged away before finality stops being
// reported, and the scanner's vanished-receipt check re-reads its transaction
// (eth_getTransactionReceipt). Amounts are wei, exact (rawContract.value),
// never Alchemy's float `value`.

import (
	"context"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/server/model"
)

// The most pages of transfers one address's read takes.
const cryptoEthMaxPages = 5

// The newest block older than this is a stalled source: its height is unknown.
const cryptoEthStaleTip = 5 * time.Minute

type ethereumWallet struct {
	*cryptoCoinbaseAddresses
	cryptoAddressWatch
	network     string
	rpc         *cryptoRpcClient
	webhookId   string
	notifyToken string

	headsLock      sync.Mutex
	finalized      uint64
	latest         uint64
	latestTime     time.Time
	headsFetchedAt time.Time
}

func newEthereumWallet(settings *cryptoWalletSettings, assetConfig *CryptoPayAssetConfig) *ethereumWallet {
	return &ethereumWallet{
		cryptoCoinbaseAddresses: newCryptoCoinbaseAddresses(model.CryptoAssetEth, settings, cryptoEthereumAddressCanonical),
		network:                 assetConfig.Network,
		rpc:                     newCryptoRpcClient(settings.ChainRpcUrl, "", "", cryptoRpcAuthNone, "2.0"),
		webhookId:               settings.AlchemyWebhookId,
		notifyToken:             settings.AlchemyNotifyToken,
	}
}

// RegisterAddresses adds new pool addresses to the Address Activity webhook,
// when alchemy.yml names it and the Notify token.
func (self *ethereumWallet) RegisterAddresses(ctx context.Context, addresses []string) error {
	return cryptoAlchemyRegisterAddresses(ctx, self.webhookId, self.notifyToken, addresses)
}

// New pool addresses are registered with the Address Activity webhook, whose
// delivery brings an address's read forward whatever its age, when alchemy.yml
// names the webhook and the Notify token.
func (self *ethereumWallet) WebhookWatched() bool {
	return self.webhookId != "" && self.notifyToken != ""
}

// Reads an issued address's ETH transfers after its cursor (from the
// chain's start before its first read) through the finalized
// block, in one alchemy_getAssetTransfers read, and moves the cursor to that
// block: a finalized transfer never changes, so none is read twice and none
// is passed over (crypto_pay_sweep.go).
func (self *ethereumWallet) SweepAddress(
	ctx context.Context,
	address *model.CryptoAddressSweepAddress,
	settings *CryptoAddressSweepSettings,
	spend func() bool,
) ([]*cryptoWalletReceipt, *model.CryptoAddressSweepCursor, bool, error) {
	canonical, err := cryptoEthereumAddressCanonical(address.Address)
	if err != nil {
		return nil, nil, false, fmt.Errorf("an issued ETH address does not read: %w", err)
	}
	finalized, _, _, err := self.heads(ctx)
	if err != nil {
		return nil, nil, false, err
	}
	from := cryptoSweepFromHeight(address.Cursor)
	if finalized < from {
		// nothing was finalized since the last read
		return []*cryptoWalletReceipt{}, &model.CryptoAddressSweepCursor{Height: address.Cursor.Height}, true, nil
	}
	if !spend() {
		return nil, nil, false, nil
	}
	transfers, err := self.transfersTo(ctx, canonical, cryptoHexBlock(from), cryptoHexBlock(finalized))
	if err != nil {
		return nil, nil, false, err
	}
	receipts, err := self.receipts(ctx, transfers, map[string]int64{canonical: address.AddressIndex}, finalized, time.Now().UTC())
	if err != nil {
		return nil, nil, false, err
	}
	finalizedHeight := int64(min(finalized, 1<<62))
	return receipts, &model.CryptoAddressSweepCursor{Height: &finalizedHeight}, true, nil
}

// The receipt statuses of finalized transactions, per process: a finalized
// transaction's status never changes.
var cryptoEthFinalStatusLock sync.Mutex
var cryptoEthFinalStatus = map[string]*ethereumTxStatus{}

const cryptoEthFinalStatusMax = 10000

type ethereumTxStatus struct {
	success bool
	from    string
}

type ethereumBlock struct {
	Number    string `json:"number"`
	Timestamp string `json:"timestamp"`
}

func cryptoHexUint64(value string) (uint64, error) {
	parsed, ok := new(big.Int).SetString(strings.TrimPrefix(strings.TrimSpace(value), "0x"), 16)
	if !ok || !parsed.IsUint64() {
		return 0, fmt.Errorf("not a hex quantity")
	}
	return parsed.Uint64(), nil
}

// heads reads the finalized and the latest block, at most once a scan.
func (self *ethereumWallet) heads(ctx context.Context) (finalized uint64, latest uint64, latestTime time.Time, err error) {
	self.headsLock.Lock()
	defer self.headsLock.Unlock()
	if !self.headsFetchedAt.IsZero() && time.Since(self.headsFetchedAt) < 5*time.Second {
		return self.finalized, self.latest, self.latestTime, nil
	}
	var finalizedBlock ethereumBlock
	if err := self.rpc.call(ctx, "eth_getBlockByNumber", []any{"finalized", false}, &finalizedBlock); err != nil {
		return 0, 0, time.Time{}, err
	}
	var latestBlock ethereumBlock
	if err := self.rpc.call(ctx, "eth_getBlockByNumber", []any{"latest", false}, &latestBlock); err != nil {
		return 0, 0, time.Time{}, err
	}
	if finalized, err = cryptoHexUint64(finalizedBlock.Number); err != nil {
		return 0, 0, time.Time{}, fmt.Errorf("eth finalized block: %w", err)
	}
	if latest, err = cryptoHexUint64(latestBlock.Number); err != nil {
		return 0, 0, time.Time{}, fmt.Errorf("eth latest block: %w", err)
	}
	seconds, err := cryptoHexUint64(latestBlock.Timestamp)
	if err != nil {
		return 0, 0, time.Time{}, fmt.Errorf("eth latest block time: %w", err)
	}
	self.finalized, self.latest, self.latestTime = finalized, latest, time.Unix(int64(seconds), 0).UTC()
	self.headsFetchedAt = time.Now()
	return self.finalized, self.latest, self.latestTime, nil
}

// ethereumTransfer is one alchemy_getAssetTransfers entry (the fields read).
type ethereumTransfer struct {
	BlockNum    string `json:"blockNum"`
	UniqueId    string `json:"uniqueId"`
	Hash        string `json:"hash"`
	From        string `json:"from"`
	To          string `json:"to"`
	Asset       string `json:"asset"`
	Category    string `json:"category"`
	RawContract struct {
		Value   string  `json:"value"`
		Address *string `json:"address"`
	} `json:"rawContract"`
	Metadata struct {
		BlockTimestamp string `json:"blockTimestamp"`
	} `json:"metadata"`
}

type ethereumTransfers struct {
	Transfers []*ethereumTransfer `json:"transfers"`
	PageKey   string              `json:"pageKey"`
}

// transfersTo reads the ETH transfers to an address in [fromBlock, toBlock].
func (self *ethereumWallet) transfersTo(ctx context.Context, address string, fromBlock string, toBlock string) ([]*ethereumTransfer, error) {
	transfers := []*ethereumTransfer{}
	pageKey := ""
	for page := 0; page < cryptoEthMaxPages; page += 1 {
		params := map[string]any{
			"fromBlock":        fromBlock,
			"toBlock":          toBlock,
			"toAddress":        address,
			"category":         []string{"external", "internal"},
			"withMetadata":     true,
			"excludeZeroValue": true,
			"maxCount":         "0x3e8",
		}
		if pageKey != "" {
			params["pageKey"] = pageKey
		}
		var result ethereumTransfers
		if err := self.rpc.call(ctx, "alchemy_getAssetTransfers", []any{params}, &result); err != nil {
			return nil, err
		}
		transfers = append(transfers, result.Transfers...)
		if result.PageKey == "" {
			return transfers, nil
		}
		pageKey = result.PageKey
	}
	return nil, fmt.Errorf("an ETH invoice address has more transfers than one read takes")
}

// cryptoEthTransferWei is a transfer's exact amount in wei: ETH only (no
// contract), from the raw hex value.
func cryptoEthTransferWei(transfer *ethereumTransfer) (*big.Int, bool) {
	if transfer.Asset != "ETH" || (transfer.Category != "external" && transfer.Category != "internal") {
		return nil, false
	}
	if transfer.RawContract.Address != nil && *transfer.RawContract.Address != "" {
		return nil, false
	}
	value, ok := new(big.Int).SetString(strings.TrimPrefix(strings.TrimSpace(transfer.RawContract.Value), "0x"), 16)
	if !ok || value.Sign() <= 0 {
		return nil, false
	}
	return value, true
}

type ethereumTxReceipt struct {
	Status      string `json:"status"`
	From        string `json:"from"`
	To          string `json:"to"`
	BlockNumber string `json:"blockNumber"`
}

// txStatus is a transaction's receipt status and origin; found = false when
// it is not mined (or was reorged away).
func (self *ethereumWallet) txStatus(ctx context.Context, hash string, blockNumber uint64, finalized uint64) (status *ethereumTxStatus, found bool, err error) {
	cryptoEthFinalStatusLock.Lock()
	cached, ok := cryptoEthFinalStatus[strings.ToLower(hash)]
	cryptoEthFinalStatusLock.Unlock()
	if ok {
		return cached, true, nil
	}
	var receipt *ethereumTxReceipt
	if err := self.rpc.call(ctx, "eth_getTransactionReceipt", []any{hash}, &receipt); err != nil {
		return nil, false, err
	}
	if receipt == nil {
		return nil, false, nil
	}
	status = &ethereumTxStatus{success: receipt.Status == "0x1", from: receipt.From}
	if 0 < blockNumber && blockNumber <= finalized {
		cryptoEthFinalStatusLock.Lock()
		if cryptoEthFinalStatusMax <= len(cryptoEthFinalStatus) {
			cryptoEthFinalStatus = map[string]*ethereumTxStatus{}
		}
		cryptoEthFinalStatus[strings.ToLower(hash)] = status
		cryptoEthFinalStatusLock.Unlock()
	}
	return status, true, nil
}

// receipts turns transfers to our addresses into receipts: one per
// transaction and address, summed, for successful transactions only.
func (self *ethereumWallet) receipts(ctx context.Context, transfers []*ethereumTransfer, ours map[string]int64, finalized uint64, now time.Time) ([]*cryptoWalletReceipt, error) {
	type key struct {
		hash    string
		address string
	}
	sums := map[key]*big.Int{}
	firsts := map[key]*ethereumTransfer{}
	senders := map[key][]string{}
	order := []key{}
	for _, transfer := range transfers {
		wei, ok := cryptoEthTransferWei(transfer)
		if !ok {
			continue
		}
		address, err := cryptoEthereumAddressCanonical(transfer.To)
		if err != nil {
			continue
		}
		if _, ok := ours[address]; !ok {
			continue
		}
		k := key{hash: strings.ToLower(transfer.Hash), address: address}
		if _, seen := sums[k]; !seen {
			sums[k] = new(big.Int)
			firsts[k] = transfer
			order = append(order, k)
		}
		sums[k].Add(sums[k], wei)
		if from, err := cryptoEthereumAddressCanonical(transfer.From); err == nil {
			if !slices.Contains(senders[k], from) {
				senders[k] = append(senders[k], from)
			}
		} else {
			senders[k] = append(senders[k], "")
		}
	}
	receipts := []*cryptoWalletReceipt{}
	for _, k := range order {
		first := firsts[k]
		blockNumber, err := cryptoHexUint64(first.BlockNum)
		if err != nil {
			continue
		}
		status, found, err := self.txStatus(ctx, k.hash, blockNumber, finalized)
		if err != nil {
			return nil, err
		}
		if !found || !status.success {
			// a reverted transaction moved no ETH
			continue
		}
		transferSenders := senders[k]
		if origin, err := cryptoEthereumAddressCanonical(status.from); err == nil && !slices.Contains(transferSenders, origin) {
			// a contract wallet's send: its origin is screened too
			transferSenders = append(transferSenders, origin)
		}
		receipt := &cryptoWalletReceipt{
			TxId:         k.hash,
			OutputKey:    fmt.Sprintf("%d", ours[k.address]),
			Address:      k.address,
			AddressIndex: cryptoIndexOf(ours[k.address]),
			AmountAtomic: cryptoClampUint64(sums[k]),
			BlockHeight:  blockNumber,
			Senders:      transferSenders,
			Timestamp:    now,
		}
		if blockNumber <= finalized {
			receipt.Confirmations = 1
		}
		if t, err := time.Parse(time.RFC3339, first.Metadata.BlockTimestamp); err == nil {
			receipt.Timestamp = t.UTC()
		}
		receipts = append(receipts, receipt)
	}
	return receipts, nil
}

func (self *ethereumWallet) IncomingSince(ctx context.Context, minHeight uint64) ([]*cryptoWalletReceipt, uint64, error) {
	finalized, latest, _, err := self.heads(ctx)
	if err != nil {
		return nil, 0, err
	}
	watched := self.watchedAddresses()
	ours := map[string]int64{}
	for _, address := range watched {
		if canonical, err := cryptoEthereumAddressCanonical(address.Address); err == nil {
			ours[canonical] = address.AddressIndex
		}
	}
	now := time.Now().UTC()
	receipts := []*cryptoWalletReceipt{}
	for address := range ours {
		transfers, err := self.transfersTo(ctx, address, "0x0", "latest")
		if err != nil {
			return nil, 0, err
		}
		addressReceipts, err := self.receipts(ctx, transfers, ours, finalized, now)
		if err != nil {
			return nil, 0, err
		}
		receipts = append(receipts, addressReceipts...)
	}
	return receipts, latest, nil
}

func (self *ethereumWallet) Transaction(ctx context.Context, txId string) ([]*cryptoWalletReceipt, bool, error) {
	finalized, _, _, err := self.heads(ctx)
	if err != nil {
		return nil, false, err
	}
	var receipt *ethereumTxReceipt
	if err := self.rpc.call(ctx, "eth_getTransactionReceipt", []any{txId}, &receipt); err != nil {
		return nil, false, err
	}
	if receipt == nil {
		return nil, false, nil
	}
	blockNumber, err := cryptoHexUint64(receipt.BlockNumber)
	if err != nil {
		return nil, false, nil
	}
	if receipt.Status != "0x1" {
		return []*cryptoWalletReceipt{}, true, nil
	}
	// the addresses it may have paid: the stored receipts' and its recipient
	candidates := cryptoReceiptAddressesForTx(ctx, model.CryptoAssetEth, strings.ToLower(txId))
	if to, err := cryptoEthereumAddressCanonical(receipt.To); err == nil && !slices.Contains(candidates, to) {
		candidates = append(candidates, to)
	}
	ours := cryptoResolveAddresses(ctx, model.CryptoAssetEth, candidates)
	block := fmt.Sprintf("0x%x", blockNumber)
	transfers := []*ethereumTransfer{}
	for address := range ours {
		addressTransfers, err := self.transfersTo(ctx, address, block, block)
		if err != nil {
			return nil, false, err
		}
		for _, transfer := range addressTransfers {
			if strings.EqualFold(transfer.Hash, txId) {
				transfers = append(transfers, transfer)
			}
		}
	}
	receipts, err := self.receipts(ctx, transfers, ours, finalized, time.Now().UTC())
	if err != nil {
		return nil, false, err
	}
	return receipts, true, nil
}

// Heights: the wallet's and the network's height is the latest block, the
// node's the finalized one. A latest block older than cryptoEthStaleTip is a
// stalled source: the network height is unknown and the coin is not sold.
func (self *ethereumWallet) Heights(ctx context.Context) (uint64, uint64, uint64, error) {
	finalized, latest, latestTime, err := self.heads(ctx)
	if err != nil {
		return 0, 0, 0, err
	}
	networkHeight := latest
	if cryptoEthStaleTip < cryptoChainNow().Sub(latestTime) {
		networkHeight = 0
	}
	return latest, finalized, networkHeight, nil
}

// CheckNetwork refuses a node of another chain than Ethereum mainnet.
func (self *ethereumWallet) CheckNetwork(ctx context.Context) error {
	var chainId string
	if err := self.rpc.call(ctx, "eth_chainId", []any{}, &chainId); err != nil {
		return err
	}
	if self.network == cryptoNetworkMainnet && !strings.EqualFold(chainId, "0x1") {
		return fmt.Errorf("the ETH source serves chain %s, not Ethereum mainnet", chainId)
	}
	return nil
}
