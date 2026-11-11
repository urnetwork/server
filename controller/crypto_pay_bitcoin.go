package controller

// Bitcoin on the crypto payment rail (mmm/BTCPAY.md §17.3, §17.7): each
// invoice has a Coinbase deposit address of its own, and payments are read
// from the chain through Alchemy alone:
//
//   - the source of truth: each invoice address's transaction history, polled
//     every minute, from Alchemy's UTXO API (Blockbook-compatible,
//     GET <endpoint>/api/v2/address/<address>?details=txs). The history names
//     each payment's inputs, which the sanctions screen reads;
//   - the heights: Alchemy's Bitcoin JSON-RPC (getblockchaininfo);
//   - the fast path: an Alchemy webhook delivery, when Alchemy offers one for
//     Bitcoin, only brings an address's read forward (crypto_pay_alchemy.go).
//
// Credited at 1 confirmation, final at 6. A payment in the mempool shows as
// seen. Alchemy's UTXO API is an add-on: a key without it answers 401 ("UTXO
// requests are not allowed", seen on the owner's key 2026-11-10), every scan
// fails, and Bitcoin stays unavailable (scanner_lagging) until the add-on is
// on.

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/urnetwork/server/model"
)

// The most pages of 50 transactions one address's read takes.
const cryptoBitcoinMaxPages = 4

// The best block older than this is a stalled source: its height is unknown.
const cryptoBitcoinStaleTip = 3 * time.Hour

type bitcoinWallet struct {
	*cryptoCoinbaseAddresses
	cryptoAddressWatch
	network    string
	endpoint   string
	rpc        *cryptoRpcClient
	httpClient *http.Client
}

func newBitcoinWallet(settings *cryptoWalletSettings, assetConfig *CryptoPayAssetConfig) *bitcoinWallet {
	return &bitcoinWallet{
		cryptoCoinbaseAddresses: newCryptoCoinbaseAddresses(model.CryptoAssetBtc, settings, cryptoBitcoinAddressCanonical),
		network:                 assetConfig.Network,
		endpoint:                strings.TrimRight(settings.ChainRpcUrl, "/"),
		rpc:                     newCryptoRpcClient(settings.ChainRpcUrl, "", "", cryptoRpcAuthNone, "2.0"),
		httpClient:              cryptoProviderClient(),
	}
}

// Blockbook's address and transaction shapes (the fields read here).
type bitcoinBlockbookAddress struct {
	Page         int                   `json:"page"`
	TotalPages   int                   `json:"totalPages"`
	Transactions []*bitcoinBlockbookTx `json:"transactions"`
}

type bitcoinBlockbookTx struct {
	Txid string `json:"txid"`
	Vin  []struct {
		Addresses []string `json:"addresses"`
		IsAddress bool     `json:"isAddress"`
	} `json:"vin"`
	Vout []struct {
		Value     string   `json:"value"`
		N         int      `json:"n"`
		Addresses []string `json:"addresses"`
		IsAddress bool     `json:"isAddress"`
	} `json:"vout"`
	// -1 or 0 in the mempool
	BlockHeight   int64  `json:"blockHeight"`
	Confirmations uint64 `json:"confirmations"`
	// the block's time; for a mempool transaction, when it was first seen
	BlockTime int64 `json:"blockTime"`
}

func (self *bitcoinWallet) addressUrl(address string, page int) string {
	return fmt.Sprintf("%s/api/v2/address/%s?details=txs&pageSize=50&page=%d", self.endpoint, url.PathEscape(address), page)
}

func (self *bitcoinWallet) txUrl(txId string) string {
	return fmt.Sprintf("%s/api/v2/tx/%s", self.endpoint, url.PathEscape(txId))
}

// cryptoBitcoinUtxoError names the add-on a refused key lacks.
func cryptoBitcoinUtxoError(err error) error {
	var statusErr *cryptoProviderStatusError
	if errors.As(err, &statusErr) && (statusErr.Status == http.StatusUnauthorized || statusErr.Status == http.StatusForbidden) {
		return fmt.Errorf("%w (Alchemy's UTXO API is an add-on: the key needs it for Bitcoin's address history)", err)
	}
	return err
}

// cryptoBitcoinReceipts reads one transaction's outputs to our addresses
// (summed per address) as receipts, with its inputs as the senders.
func cryptoBitcoinReceipts(tx *bitcoinBlockbookTx, ours map[string]int64, now time.Time) []*cryptoWalletReceipt {
	senders := []string{}
	for _, vin := range tx.Vin {
		if vin.IsAddress && len(vin.Addresses) == 1 && vin.Addresses[0] != "" {
			senders = append(senders, vin.Addresses[0])
		} else {
			// an input that names no single address cannot be screened
			senders = append(senders, "")
		}
	}
	sums := map[string]*big.Int{}
	order := []string{}
	for _, vout := range tx.Vout {
		if !vout.IsAddress || len(vout.Addresses) != 1 {
			continue
		}
		address, err := cryptoBitcoinAddressCanonical(vout.Addresses[0])
		if err != nil {
			continue
		}
		if _, ok := ours[address]; !ok {
			continue
		}
		value, ok := new(big.Int).SetString(strings.TrimSpace(vout.Value), 10)
		if !ok || value.Sign() <= 0 {
			continue
		}
		if _, seen := sums[address]; !seen {
			sums[address] = new(big.Int)
			order = append(order, address)
		}
		sums[address].Add(sums[address], value)
	}
	receipts := []*cryptoWalletReceipt{}
	for _, address := range order {
		index := ours[address]
		receipt := &cryptoWalletReceipt{
			TxId:         tx.Txid,
			OutputKey:    fmt.Sprintf("%d", index),
			Address:      address,
			AddressIndex: cryptoIndexOf(index),
			AmountAtomic: cryptoClampUint64(sums[address]),
			Senders:      senders,
			Timestamp:    now,
		}
		if 0 < tx.BlockHeight {
			receipt.BlockHeight = uint64(tx.BlockHeight)
			receipt.Confirmations = max(1, tx.Confirmations)
		}
		if 0 < tx.BlockTime {
			receipt.Timestamp = time.Unix(tx.BlockTime, 0).UTC()
		}
		receipts = append(receipts, receipt)
	}
	return receipts
}

// cryptoClampUint64 is an amount as a uint64, saturated (no payment is near).
func cryptoClampUint64(value *big.Int) uint64 {
	if value.IsUint64() {
		return value.Uint64()
	}
	return 1 << 62
}

func (self *bitcoinWallet) IncomingSince(ctx context.Context, minHeight uint64) ([]*cryptoWalletReceipt, uint64, error) {
	now := time.Now().UTC()
	watched := self.watchedAddresses()
	ours := map[string]int64{}
	for _, address := range watched {
		ours[address.Address] = address.AddressIndex
	}
	receipts := []*cryptoWalletReceipt{}
	seen := map[string]bool{}
	for _, address := range watched {
		for page := 1; page <= cryptoBitcoinMaxPages; page += 1 {
			var history bitcoinBlockbookAddress
			if err := cryptoProviderGet(ctx, self.httpClient, self.addressUrl(address.Address, page), "alchemy bitcoin address", &history); err != nil {
				return nil, 0, cryptoBitcoinUtxoError(err)
			}
			for _, tx := range history.Transactions {
				if tx == nil || tx.Txid == "" || seen[tx.Txid] {
					continue
				}
				seen[tx.Txid] = true
				receipts = append(receipts, cryptoBitcoinReceipts(tx, ours, now)...)
			}
			if history.TotalPages <= page {
				break
			}
		}
	}
	return receipts, 0, nil
}

func (self *bitcoinWallet) Transaction(ctx context.Context, txId string) ([]*cryptoWalletReceipt, bool, error) {
	var tx bitcoinBlockbookTx
	err := cryptoProviderGet(ctx, self.httpClient, self.txUrl(txId), "alchemy bitcoin transaction", &tx)
	var statusErr *cryptoProviderStatusError
	if errors.As(err, &statusErr) && (statusErr.Status == http.StatusNotFound ||
		(statusErr.Status == http.StatusBadRequest && strings.Contains(strings.ToLower(statusErr.Message), "not found"))) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, cryptoBitcoinUtxoError(err)
	}
	if tx.Txid == "" {
		return nil, false, nil
	}
	addresses := []string{}
	for _, vout := range tx.Vout {
		if vout.IsAddress && len(vout.Addresses) == 1 {
			if address, err := cryptoBitcoinAddressCanonical(vout.Addresses[0]); err == nil {
				addresses = append(addresses, address)
			}
		}
	}
	ours := cryptoResolveAddresses(ctx, model.CryptoAssetBtc, addresses)
	return cryptoBitcoinReceipts(&tx, ours, time.Now().UTC()), true, nil
}

type bitcoinChainInfo struct {
	Chain                string `json:"chain"`
	Blocks               uint64 `json:"blocks"`
	Headers              uint64 `json:"headers"`
	InitialBlockDownload bool   `json:"initialblockdownload"`
	// the best block's time
	Time int64 `json:"time"`
}

func (self *bitcoinWallet) chainInfo(ctx context.Context) (*bitcoinChainInfo, error) {
	var info bitcoinChainInfo
	if err := self.rpc.call(ctx, "getblockchaininfo", []any{}, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// Heights: the scanner reads Alchemy's own view, so the wallet's height is
// its best block; the network's is its best header. A best block older than
// cryptoBitcoinStaleTip, or a node still syncing, is an unknown network
// height: the coin is not sold until it moves again.
func (self *bitcoinWallet) Heights(ctx context.Context) (uint64, uint64, uint64, error) {
	info, err := self.chainInfo(ctx)
	if err != nil {
		return 0, 0, 0, err
	}
	networkHeight := max(info.Blocks, info.Headers)
	if info.InitialBlockDownload || (0 < info.Time && cryptoBitcoinStaleTip < time.Since(time.Unix(info.Time, 0))) {
		networkHeight = 0
	}
	return info.Blocks, info.Blocks, networkHeight, nil
}

// CheckNetwork refuses a node of another chain than the coin's.
func (self *bitcoinWallet) CheckNetwork(ctx context.Context) error {
	info, err := self.chainInfo(ctx)
	if err != nil {
		return err
	}
	want := map[string]string{cryptoNetworkMainnet: "main", cryptoNetworkTestnet: "test", cryptoNetworkRegtest: "regtest"}[self.network]
	if info.Chain != want {
		return fmt.Errorf("the Bitcoin source serves chain %q, the coin's network is %s", info.Chain, self.network)
	}
	return nil
}
