package controller

// The Coinbase deposit addresses, the Bitcoin and ETH chain readers, the
// Alchemy webhook and its setup, the sanctions screen and the quote endpoint,
// against recorded answers served by local test servers. Hermetic: no
// database, no network, no live Coinbase or Alchemy call.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// ----- Coinbase -----

// cryptoTestCoinbaseKey is a fresh ES256 key in the CDP key's PEM form.
func cryptoTestCoinbaseKey(t testing.TB) (*ecdsa.PrivateKey, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return key, string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
}

// cryptoTestCoinbaseServer answers the App API as Coinbase does: the wallets
// by currency code, and a new address per POST unless static is set.
type cryptoTestCoinbaseServer struct {
	t         testing.TB
	publicKey *ecdsa.PublicKey
	keyName   string
	host      string

	stateLock    sync.Mutex
	accounts     map[string]string
	created      []map[string]string
	uris         []string
	static       bool
	network      string
	accountReads int
}

func (self *cryptoTestCoinbaseServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	// every request carries a fresh CDP JWT for exactly its method, host and path
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	token, err := jwt.Parse(bearer, func(token *jwt.Token) (any, error) {
		return self.publicKey, nil
	}, jwt.WithValidMethods([]string{"ES256"}))
	if err != nil || !token.Valid {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"errors":[{"id":"authentication_error","message":"invalid signature"}]}`))
		return
	}
	claims := token.Claims.(jwt.MapClaims)
	if claims["sub"] != self.keyName || claims["iss"] != "cdp" || token.Header["kid"] != self.keyName {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	uris, _ := claims["uris"].([]any)
	if len(uris) != 1 || uris[0] != r.Method+" "+self.host+r.URL.Path {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	self.uris = append(self.uris, uris[0].(string))
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v2/accounts/"):
		self.accountReads += 1
		code := strings.TrimPrefix(r.URL.Path, "/v2/accounts/")
		id, ok := self.accounts[code]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"errors":[{"id":"not_found","message":"Not found"}]}`))
			return
		}
		fmt.Fprintf(w, `{"data":{"id":%q,"name":"%s Wallet","primary":true,"type":"wallet","currency":{"code":%q,"name":"x","exponent":8}}}`, id, code, code)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/addresses"):
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		self.created = append(self.created, body)
		network := body["network"]
		if self.network != "" {
			network = self.network
		}
		address := fmt.Sprintf("bc1qsynthetic%d", len(self.created))
		if self.static {
			address = "bc1qsynthetic1"
		}
		fmt.Fprintf(w, `{"data":{"id":"a%d","address":%q,"name":%q,"network":%q,"resource":"address","destination_tag":""}}`,
			len(self.created), address, body["name"], network)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func cryptoTestCoinbase(t testing.TB) (*cryptoTestCoinbaseServer, *cryptoCoinbaseSettings) {
	t.Helper()
	key, keyPem := cryptoTestCoinbaseKey(t)
	fake := &cryptoTestCoinbaseServer{
		t:         t,
		publicKey: &key.PublicKey,
		keyName:   "organizations/synthetic/apiKeys/synthetic",
		host:      "api.coinbase.test",
		accounts:  map[string]string{"BTC": "11111111-1111-1111-1111-111111111111"},
	}
	testServer := httptest.NewServer(fake)
	t.Cleanup(testServer.Close)
	previousBase := cryptoCoinbaseBaseUrl
	cryptoCoinbaseBaseUrl = func(host string) string { return testServer.URL }
	resetCryptoCoinbaseAccounts()
	t.Cleanup(func() {
		cryptoCoinbaseBaseUrl = previousBase
		resetCryptoCoinbaseAccounts()
	})
	return fake, &cryptoCoinbaseSettings{Host: fake.host, KeyName: fake.keyName, PrivateKey: keyPem}
}

// A Coinbase deposit address is made in the coin's own wallet, on the coin's
// own network, named with the invoice id, at the next pool index; the wallet
// is looked up once by its code.
func TestCryptoCoinbaseMakesANamedAddressPerInvoice(t *testing.T) {
	fake, settings := cryptoTestCoinbase(t)
	addresses := newCryptoCoinbaseAddresses(model.CryptoAssetBtc, &cryptoWalletSettings{Coinbase: settings}, func(address string) (string, error) {
		return address, nil
	})
	addresses.SetAddressHighWater(41)
	first := server.NewId().String()
	address, index, err := addresses.NewNamedAddress(context.Background(), first)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, address, "bc1qsynthetic1")
	connect.AssertEqual(t, index, uint64(42))
	_, index, err = addresses.NewNamedAddress(context.Background(), server.NewId().String())
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, index, uint64(43))
	connect.AssertEqual(t, fake.created[0], map[string]string{"name": first, "network": "bitcoin"})
	connect.AssertEqual(t, fake.accountReads, 1)
	connect.AssertEqual(t, fake.uris[0], "GET api.coinbase.test/v2/accounts/BTC")
	connect.AssertEqual(t, fake.uris[1], "POST api.coinbase.test/v2/accounts/11111111-1111-1111-1111-111111111111/addresses")
	// plain NewAddress is refused: an address is made for a named invoice
	_, _, err = addresses.NewAddress(context.Background(), cryptoPoolAddressLabel)
	connect.AssertEqual(t, err != nil, true)

	// a coin with no wallet in the account (TAO on 2026-11-10)
	tao := newCryptoCoinbaseAddresses(model.CryptoAssetTao, &cryptoWalletSettings{Coinbase: settings}, cryptoTaoAddressCanonical)
	_, _, err = tao.NewNamedAddress(context.Background(), server.NewId().String())
	connect.AssertEqual(t, errors.Is(err, errCryptoCoinbaseNoAccount), true)
	// an address made on another network is refused
	fake.network = "base"
	_, _, err = addresses.NewNamedAddress(context.Background(), server.NewId().String())
	connect.AssertEqual(t, err != nil && strings.Contains(err.Error(), "network"), true)
	fake.network = ""
	// an address Coinbase answers that is not the coin's is refused
	bad := newCryptoCoinbaseAddresses(model.CryptoAssetBtc, &cryptoWalletSettings{Coinbase: settings}, cryptoBitcoinAddressCanonical)
	_, _, err = bad.NewNamedAddress(context.Background(), server.NewId().String())
	connect.AssertEqual(t, err != nil, true)
	// no key: nothing is asked
	none := newCryptoCoinbaseAddresses(model.CryptoAssetBtc, &cryptoWalletSettings{}, cryptoBitcoinAddressCanonical)
	_, _, err = none.NewNamedAddress(context.Background(), server.NewId().String())
	connect.AssertEqual(t, err != nil, true)
	// the errors never carry the key or a token
	_, keyPem := cryptoTestCoinbaseKey(t)
	connect.AssertEqual(t, strings.Contains(fmt.Sprint(err), keyPem), false)
}

// ----- Bitcoin -----

// cryptoTestBitcoinServer answers Alchemy's Bitcoin endpoint: the UTXO API
// (Blockbook) and the JSON-RPC, from recorded answers.
func cryptoTestBitcoinServer(t testing.TB, utxoRefused bool) *httptest.Server {
	t.Helper()
	address := cryptoTestFixture(t, "btc_blockbook_address.json")
	info := cryptoTestFixture(t, "btc_blockchaininfo.json")
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			var request struct {
				Method string `json:"method"`
			}
			json.NewDecoder(r.Body).Decode(&request)
			if request.Method != "getblockchaininfo" {
				w.Write([]byte(`{"jsonrpc":"2.0","id":"0","error":{"code":-32601,"message":"Method not found"}}`))
				return
			}
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":"0","result":%s}`, info)
		case utxoRefused:
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"UTXO requests are not allowed."}`))
		case strings.HasPrefix(r.URL.Path, "/v2/synthetic-key/api/v2/address/bc1q0wd209cv5k9pd9mhk7nspacywcj038xxdhnt5u"):
			w.Write(address)
		case strings.HasPrefix(r.URL.Path, "/v2/synthetic-key/api/v2/address/"):
			w.Write([]byte(`{"page":1,"totalPages":1,"itemsOnPage":0,"txs":0,"transactions":[]}`))
		case r.URL.Path == "/v2/synthetic-key/api/v2/tx/877266da537525669e890666f60e282f074f3414566487734abd2ab37ee33b68":
			var history bitcoinBlockbookAddress
			json.Unmarshal(address, &history)
			for _, tx := range history.Transactions {
				if tx.Txid == "877266da537525669e890666f60e282f074f3414566487734abd2ab37ee33b68" {
					json.NewEncoder(w).Encode(tx)
					return
				}
			}
		default:
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"not found"}`))
		}
	}))
	t.Cleanup(testServer.Close)
	return testServer
}

func cryptoTestChainClock(t testing.TB, now time.Time) {
	previous := cryptoChainNow
	cryptoChainNow = func() time.Time { return now }
	t.Cleanup(func() { cryptoChainNow = previous })
}

// The address's history is the source of truth: only outputs to the invoice
// address count (Coinbase's sweep spends them; that is never a payment), each
// payment names its inputs for the screen, and the heights come from the
// RPC.
func TestCryptoBitcoinReaderReadsTheAddressHistory(t *testing.T) {
	testServer := cryptoTestBitcoinServer(t, false)
	restore := cryptoTestHermeticSeams()
	t.Cleanup(restore)
	wallet := newBitcoinWallet(
		&cryptoWalletSettings{Asset: model.CryptoAssetBtc, ChainRpcUrl: testServer.URL + "/v2/synthetic-key"},
		&CryptoPayAssetConfig{Network: cryptoNetworkMainnet},
	)
	wallet.SetWatched([]*model.CryptoPaymentWatchedAddress{{Address: "bc1q0wd209cv5k9pd9mhk7nspacywcj038xxdhnt5u", AddressIndex: 7, Hot: true}})
	receipts, _, err := wallet.IncomingSince(context.Background(), 0)
	connect.AssertEqual(t, err, nil)
	if len(receipts) != 1 {
		t.Fatalf("receipts = %d, want the one payment to the address", len(receipts))
	}
	receipt := receipts[0]
	connect.AssertEqual(t, receipt.TxId, "877266da537525669e890666f60e282f074f3414566487734abd2ab37ee33b68")
	connect.AssertEqual(t, receipt.OutputKey, "7")
	connect.AssertEqual(t, *receipt.AddressIndex, uint64(7))
	connect.AssertEqual(t, receipt.Address, "bc1q0wd209cv5k9pd9mhk7nspacywcj038xxdhnt5u")
	connect.AssertEqual(t, 0 < receipt.AmountAtomic, true)
	connect.AssertEqual(t, 0 < receipt.BlockHeight, true)
	connect.AssertEqual(t, 1 <= receipt.Confirmations, true)
	connect.AssertEqual(t, len(receipt.Senders), 1)
	connect.AssertEqual(t, receipt.Senders[0] != "", true)

	// the heights: Alchemy's best block, with a tip of its time
	cryptoTestChainClock(t, time.Unix(1794350912, 0).Add(10*time.Minute))
	walletHeight, nodeHeight, networkHeight, err := wallet.Heights(context.Background())
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, walletHeight, uint64(975356))
	connect.AssertEqual(t, nodeHeight, uint64(975356))
	connect.AssertEqual(t, networkHeight, uint64(975356))
	connect.AssertEqual(t, wallet.CheckNetwork(context.Background()), nil)
	// a best block hours old is a stalled source: the network height is unknown
	cryptoTestChainClock(t, time.Unix(1794350912, 0).Add(4*time.Hour))
	_, _, networkHeight, _ = wallet.Heights(context.Background())
	connect.AssertEqual(t, networkHeight, uint64(0))

	// a transaction read again, for the screen and the reconciler
	cryptoResolveAddresses = func(ctx context.Context, asset string, addresses []string) map[string]int64 {
		resolved := map[string]int64{}
		for _, address := range addresses {
			if address == "bc1q0wd209cv5k9pd9mhk7nspacywcj038xxdhnt5u" {
				resolved[address] = 7
			}
		}
		return resolved
	}
	again, found, err := wallet.Transaction(context.Background(), receipt.TxId)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, found, true)
	connect.AssertEqual(t, len(again), 1)
	connect.AssertEqual(t, again[0].AmountAtomic, receipt.AmountAtomic)
	connect.AssertEqual(t, again[0].Senders, receipt.Senders)
	_, found, err = wallet.Transaction(context.Background(), strings.Repeat("ab", 32))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, found, false)
}

// Without Alchemy's UTXO add-on every scan fails, naming the add-on, and the
// url (which carries the key) is in no error.
func TestCryptoBitcoinReaderNamesTheMissingUtxoAddOn(t *testing.T) {
	testServer := cryptoTestBitcoinServer(t, true)
	wallet := newBitcoinWallet(
		&cryptoWalletSettings{Asset: model.CryptoAssetBtc, ChainRpcUrl: testServer.URL + "/v2/synthetic-key"},
		&CryptoPayAssetConfig{Network: cryptoNetworkMainnet},
	)
	wallet.SetWatched([]*model.CryptoPaymentWatchedAddress{{Address: "bc1q0wd209cv5k9pd9mhk7nspacywcj038xxdhnt5u", AddressIndex: 7, Hot: true}})
	_, _, err := wallet.IncomingSince(context.Background(), 0)
	connect.AssertEqual(t, err != nil, true)
	connect.AssertEqual(t, strings.Contains(err.Error(), "UTXO API is an add-on"), true)
	connect.AssertEqual(t, strings.Contains(err.Error(), "synthetic-key"), false)
	connect.AssertEqual(t, strings.Contains(err.Error(), testServer.URL), false)
}

// ----- ETH -----

// cryptoTestEthereumServer answers Alchemy's ETH JSON-RPC from recorded
// answers: two transfers to one address (an internal one from a contract and
// an external one), their receipts, and the finalized and latest blocks.
type cryptoTestEthereumServer struct {
	t         testing.TB
	transfers []byte
	receipts  map[string]json.RawMessage
	finalized []byte
	latest    string
	chainId   string
	failed    map[string]bool
}

func (self *cryptoTestEthereumServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	json.NewDecoder(r.Body).Decode(&request)
	result := func(value string) {
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":"0","result":%s}`, value)
	}
	switch request.Method {
	case "eth_getBlockByNumber":
		if strings.Contains(string(request.Params[0]), "finalized") {
			result(string(self.finalized))
		} else {
			result(self.latest)
		}
	case "alchemy_getAssetTransfers":
		var params struct {
			FromBlock string `json:"fromBlock"`
			ToBlock   string `json:"toBlock"`
			ToAddress string `json:"toAddress"`
		}
		json.Unmarshal(request.Params[0], &params)
		if !strings.EqualFold(params.ToAddress, "0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045") {
			result(`{"transfers":[]}`)
			return
		}
		if params.FromBlock != "0x0" {
			// the single-block read of a re-verification
			var all ethereumTransfers
			json.Unmarshal(self.transfers, &all)
			matched := []*ethereumTransfer{}
			for _, transfer := range all.Transfers {
				if transfer.BlockNum == params.FromBlock {
					matched = append(matched, transfer)
				}
			}
			encoded, _ := json.Marshal(&ethereumTransfers{Transfers: matched})
			result(string(encoded))
			return
		}
		result(string(self.transfers))
	case "eth_getTransactionReceipt":
		var hash string
		json.Unmarshal(request.Params[0], &hash)
		receipt, ok := self.receipts[hash]
		if !ok {
			result("null")
			return
		}
		if self.failed[hash] {
			var decoded map[string]any
			json.Unmarshal(receipt, &decoded)
			decoded["status"] = "0x0"
			encoded, _ := json.Marshal(decoded)
			result(string(encoded))
			return
		}
		result(string(receipt))
	case "eth_chainId":
		result(fmt.Sprintf("%q", self.chainId))
	default:
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":"0","error":{"code":-32601,"message":"Method not found"}}`)
	}
}

func cryptoTestEthereum(t testing.TB) (*cryptoTestEthereumServer, *ethereumWallet) {
	t.Helper()
	fake := &cryptoTestEthereumServer{
		t:         t,
		transfers: cryptoTestFixture(t, "eth_asset_transfers.json"),
		receipts:  map[string]json.RawMessage{},
		finalized: cryptoTestFixture(t, "eth_block_finalized.json"),
		// a latest block a few above the finalized one, at its time
		latest:  `{"number":"0x192a4d5","timestamp":"0x6a5f5d73"}`,
		chainId: "0x1",
		failed:  map[string]bool{},
	}
	json.Unmarshal(cryptoTestFixture(t, "eth_receipts.json"), &fake.receipts)
	testServer := httptest.NewServer(fake)
	t.Cleanup(testServer.Close)
	cryptoEthFinalStatusLock.Lock()
	cryptoEthFinalStatus = map[string]*ethereumTxStatus{}
	cryptoEthFinalStatusLock.Unlock()
	wallet := newEthereumWallet(
		&cryptoWalletSettings{Asset: model.CryptoAssetEth, ChainRpcUrl: testServer.URL + "/v2/synthetic-key"},
		&CryptoPayAssetConfig{Network: cryptoNetworkMainnet},
	)
	wallet.SetWatched([]*model.CryptoPaymentWatchedAddress{{Address: "0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045", AddressIndex: 3, Hot: true}})
	return fake, wallet
}

// Incoming transfers are the source of truth, in exact wei: an external send
// and a contract's internal one, each a receipt of its transaction, final at
// the finalized tag; a contract's send names its transaction's origin for the
// screen too; a reverted transaction moved nothing.
func TestCryptoEthereumReaderReadsIncomingTransfers(t *testing.T) {
	restore := cryptoTestHermeticSeams()
	t.Cleanup(restore)
	fake, wallet := cryptoTestEthereum(t)
	receipts, walletHeight, err := wallet.IncomingSince(context.Background(), 0)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, walletHeight, uint64(0x192a4d5))
	if len(receipts) != 2 {
		t.Fatalf("receipts = %d, want 2", len(receipts))
	}
	byTx := map[string]*cryptoWalletReceipt{}
	for _, receipt := range receipts {
		byTx[receipt.TxId] = receipt
	}
	internal := byTx["0x7c17b0b357ea17dfd6a06b3e1d831226f4d222c0589fea38aad33bee416a07f1"]
	external := byTx["0xd75f67f4cf551015772d8c6e7c53328ef4f12153819367ab9f2865c7d99f53f1"]
	if internal == nil || external == nil {
		t.Fatalf("receipts = %+v", receipts)
	}
	connect.AssertEqual(t, internal.AmountAtomic, uint64(0x01884008f415fc))
	connect.AssertEqual(t, external.AmountAtomic, uint64(0x2f86c1f910b7f7))
	connect.AssertEqual(t, internal.OutputKey, "3")
	connect.AssertEqual(t, internal.Address, "0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045")
	// both blocks are below the finalized one: final
	connect.AssertEqual(t, internal.Confirmations, uint64(1))
	connect.AssertEqual(t, external.Confirmations, uint64(1))
	// the internal transfer's sender is a contract: its origin is screened too
	connect.AssertEqual(t, 2 <= len(internal.Senders), true)
	connect.AssertEqual(t, len(external.Senders), 1)

	// a reverted transaction moved no ETH
	fake.failed["0xd75f67f4cf551015772d8c6e7c53328ef4f12153819367ab9f2865c7d99f53f1"] = true
	cryptoEthFinalStatusLock.Lock()
	cryptoEthFinalStatus = map[string]*ethereumTxStatus{}
	cryptoEthFinalStatusLock.Unlock()
	receipts, _, err = wallet.IncomingSince(context.Background(), 0)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, len(receipts), 1)
	delete(fake.failed, "0xd75f67f4cf551015772d8c6e7c53328ef4f12153819367ab9f2865c7d99f53f1")

	// re-read for the screen: the stored receipt names the address
	cryptoReceiptAddressesForTx = func(ctx context.Context, asset string, txId string) []string {
		return []string{"0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045"}
	}
	cryptoResolveAddresses = func(ctx context.Context, asset string, addresses []string) map[string]int64 {
		return map[string]int64{"0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045": 3}
	}
	again, found, err := wallet.Transaction(context.Background(), internal.TxId)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, found, true)
	connect.AssertEqual(t, len(again), 1)
	connect.AssertEqual(t, again[0].AmountAtomic, internal.AmountAtomic)
	_, found, err = wallet.Transaction(context.Background(), "0x"+strings.Repeat("00", 32))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, found, false)

	// heights: latest, finalized, latest; stale after five minutes
	cryptoTestChainClock(t, time.Unix(0x6a5f5d73, 0).Add(time.Minute))
	walletHeight, nodeHeight, networkHeight, err := wallet.Heights(context.Background())
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, walletHeight, uint64(0x192a4d5))
	connect.AssertEqual(t, nodeHeight, uint64(26387592))
	connect.AssertEqual(t, networkHeight, uint64(0x192a4d5))
	cryptoTestChainClock(t, time.Unix(0x6a5f5d73, 0).Add(10*time.Minute))
	_, _, networkHeight, _ = wallet.Heights(context.Background())
	connect.AssertEqual(t, networkHeight, uint64(0))
	// another chain is refused
	connect.AssertEqual(t, wallet.CheckNetwork(context.Background()), nil)
	fake.chainId = "0x2105"
	connect.AssertEqual(t, wallet.CheckNetwork(context.Background()) != nil, true)
}

// ----- the Alchemy webhook -----

func cryptoTestAlchemyWebhooks(t testing.TB, webhooks ...*cryptoAlchemyWebhook) {
	previous := cryptoAlchemyWebhooks
	cryptoAlchemyWebhooks = func() []*cryptoAlchemyWebhook { return webhooks }
	t.Cleanup(func() { cryptoAlchemyWebhooks = previous })
}

func cryptoTestAlchemySign(body []byte, key string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// A delivery is verified with its own webhook's signing key before it is
// read; it only names the invoice addresses for the next scan.
func TestCryptoAlchemyWebhookVerifiesAndHints(t *testing.T) {
	restore := cryptoTestHermeticSeams()
	t.Cleanup(restore)
	cryptoTestAlchemyWebhooks(t,
		&cryptoAlchemyWebhook{asset: model.CryptoAssetEth, id: "wh_eth", signingKey: "whsec_eth"},
		&cryptoAlchemyWebhook{asset: model.CryptoAssetBtc, id: "wh_btc", signingKey: "whsec_btc"},
	)
	hinted := map[string][]string{}
	cryptoAddScanHints = func(ctx context.Context, asset string, addresses []string) {
		hinted[asset] = append(hinted[asset], addresses...)
	}
	cryptoInvoiceAddresses = func(ctx context.Context, asset string, addresses []string) []string {
		out := []string{}
		for _, address := range addresses {
			if address == "0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045" {
				out = append(out, address)
			}
		}
		return out
	}
	body := []byte(`{"webhookId":"wh_eth","id":"whevt_1","createdAt":"2026-11-10T18:00:00Z","type":"ADDRESS_ACTIVITY","event":{"network":"ETH_MAINNET","activity":[` +
		`{"blockNum":"0x1922f1e","hash":"0xd75f","fromAddress":"0x1111111111111111111111111111111111111111","toAddress":"0xd8da6bf26964af9d7eed9e03e53415d37aa96045","value":0.0133,"asset":"ETH","category":"external","rawContract":{"rawValue":"0x2f86c1f910b7f7","decimals":18}},` +
		`{"blockNum":"0x1922f1e","hash":"0xd760","fromAddress":"0x1111111111111111111111111111111111111111","toAddress":"0xd8da6bf26964af9d7eed9e03e53415d37aa96045","value":5,"asset":"USDC","category":"token","rawContract":{"rawValue":"0x4c4b40","decimals":6}},` +
		`{"blockNum":"0x1922f1e","hash":"0xd761","fromAddress":"0x1111111111111111111111111111111111111111","toAddress":"0x2222222222222222222222222222222222222222","value":1,"asset":"ETH","category":"external","rawContract":{"rawValue":"0x1","decimals":18}}]}}`)
	request := func(body []byte, signature string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/pay/alchemy", strings.NewReader(string(body)))
		r.Header.Set("X-Alchemy-Signature", signature)
		return r
	}
	reader, err := VerifyAlchemyBody(nil, request(body, cryptoTestAlchemySign(body, "whsec_eth")))
	connect.AssertEqual(t, err, nil)
	read, _ := io.ReadAll(reader)
	connect.AssertEqual(t, string(read), string(body))
	// another webhook's key, a bad signature, an unknown webhook: refused
	_, err = VerifyAlchemyBody(nil, request(body, cryptoTestAlchemySign(body, "whsec_btc")))
	connect.AssertEqual(t, err != nil, true)
	_, err = VerifyAlchemyBody(nil, request(body, "00"))
	connect.AssertEqual(t, err != nil, true)
	unknown := []byte(strings.Replace(string(body), "wh_eth", "wh_other", 1))
	_, err = VerifyAlchemyBody(nil, request(unknown, cryptoTestAlchemySign(unknown, "whsec_eth")))
	connect.AssertEqual(t, err != nil, true)

	var args AlchemyWebhookArgs
	connect.AssertEqual(t, json.Unmarshal(body, &args), nil)
	result, err := AlchemyWebhook(&args, &session.ClientSession{Ctx: context.Background()})
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Message, "1 invoice addresses to read")
	// only the ETH transfer to an invoice address; the token transfer and the
	// unrelated address are not read
	connect.AssertEqual(t, hinted[model.CryptoAssetEth], []string{"0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045"})
	// another network's delivery to the ETH webhook is ignored
	args.Event.Network = "BASE_MAINNET"
	result, err = AlchemyWebhook(&args, &session.ClientSession{Ctx: context.Background()})
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Message, "Not an Ethereum mainnet delivery")
	// no webhook configured: refused before any body is read
	cryptoTestAlchemyWebhooks(t)
	connect.AssertEqual(t, AlchemyWebhookVerifiable() != nil, true)
}

// cryptoTestAlchemyDashboard answers the Notify API.
type cryptoTestAlchemyDashboard struct {
	stateLock sync.Mutex
	token     string
	webhooks  []*cryptoAlchemyTeamWebhook
	requests  []string
	bodies    []map[string]any
}

func (self *cryptoTestAlchemyDashboard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if r.Header.Get("X-Alchemy-Token") != self.token {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	self.requests = append(self.requests, r.Method+" "+r.URL.Path)
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	self.bodies = append(self.bodies, body)
	switch r.Method + " " + r.URL.Path {
	case "GET /api/team-webhooks":
		json.NewEncoder(w).Encode(map[string]any{"data": self.webhooks})
	case "POST /api/create-webhook":
		created := &cryptoAlchemyTeamWebhook{
			Id: "wh_created", Network: fmt.Sprint(body["network"]), WebhookType: fmt.Sprint(body["webhook_type"]),
			WebhookUrl: fmt.Sprint(body["webhook_url"]), IsActive: true, SigningKey: "whsec_created",
		}
		self.webhooks = append(self.webhooks, created)
		json.NewEncoder(w).Encode(map[string]any{"data": created})
	case "PATCH /api/update-webhook-addresses":
		w.Write([]byte(`{}`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func cryptoTestDashboard(t testing.TB) *cryptoTestAlchemyDashboard {
	dashboard := &cryptoTestAlchemyDashboard{token: "synthetic-token"}
	testServer := httptest.NewServer(dashboard)
	t.Cleanup(testServer.Close)
	previousUrl := cryptoAlchemyDashboardUrl
	previousSettings := cryptoAlchemySetupSettings
	cryptoAlchemyDashboardUrl = testServer.URL + "/api"
	cryptoAlchemySetupSettings = func() (string, string, error) {
		return "synthetic-app", "synthetic-token", nil
	}
	t.Cleanup(func() {
		cryptoAlchemyDashboardUrl = previousUrl
		cryptoAlchemySetupSettings = previousSettings
	})
	return dashboard
}

// The deploy-time step finds the webhook for our url before it makes one,
// makes it with the app id and one placeholder address, and writes its
// signing key to the operator's file only (never over one, never printed).
func TestCryptoAlchemyEnsureEthWebhookFindsBeforeItMakes(t *testing.T) {
	dashboard := cryptoTestDashboard(t)
	keyPath := filepath.Join(t.TempDir(), "signing-key")
	result, err := CryptoAlchemyEnsureEthWebhook(context.Background(), keyPath)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Created, true)
	connect.AssertEqual(t, result.WebhookId, "wh_created")
	connect.AssertEqual(t, result.WebhookUrl, "https://api.bringyour.com/pay/alchemy")
	connect.AssertEqual(t, result.SigningKeyWritten, true)
	connect.AssertEqual(t, dashboard.requests, []string{"GET /api/team-webhooks", "POST /api/create-webhook"})
	created := dashboard.bodies[1]
	connect.AssertEqual(t, created["network"], "ETH_MAINNET")
	connect.AssertEqual(t, created["webhook_type"], "ADDRESS_ACTIVITY")
	connect.AssertEqual(t, created["webhook_url"], "https://api.bringyour.com/pay/alchemy")
	connect.AssertEqual(t, created["app_id"], "synthetic-app")
	connect.AssertEqual(t, len(created["addresses"].([]any)), 1)
	connect.AssertEqual(t, cryptoEthereumAddressOk(created["addresses"].([]any)[0].(string)), true)
	key, err := os.ReadFile(keyPath)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, string(key), "whsec_created\n")
	stat, _ := os.Stat(keyPath)
	connect.AssertEqual(t, stat.Mode().Perm(), os.FileMode(0o600))
	connect.AssertEqual(t, strings.Contains(fmt.Sprintf("%+v", result), "whsec"), false)
	// run again: found, nothing made, and an existing key file is never overwritten
	_, err = CryptoAlchemyEnsureEthWebhook(context.Background(), keyPath)
	connect.AssertEqual(t, err != nil, true)
	result, err = CryptoAlchemyEnsureEthWebhook(context.Background(), "")
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Created, false)
	connect.AssertEqual(t, result.WebhookId, "wh_created")
	connect.AssertEqual(t, result.SigningKeyWritten, false)
	connect.AssertEqual(t, dashboard.requests[len(dashboard.requests)-1], "GET /api/team-webhooks")
}

// New pool addresses are added to the ETH webhook in batches of 500, only
// when alchemy.yml names the webhook and the Notify token.
func TestCryptoAlchemyRegistersNewAddresses(t *testing.T) {
	dashboard := cryptoTestDashboard(t)
	addresses := []string{}
	for i := 0; i < 501; i += 1 {
		addresses = append(addresses, fmt.Sprintf("0x%040x", i+1))
	}
	connect.AssertEqual(t, cryptoAlchemyRegisterAddresses(context.Background(), "wh_eth", "synthetic-token", addresses), nil)
	connect.AssertEqual(t, dashboard.requests, []string{"PATCH /api/update-webhook-addresses", "PATCH /api/update-webhook-addresses"})
	connect.AssertEqual(t, len(dashboard.bodies[0]["addresses_to_add"].([]any)), 500)
	connect.AssertEqual(t, len(dashboard.bodies[1]["addresses_to_add"].([]any)), 1)
	connect.AssertEqual(t, dashboard.bodies[0]["webhook_id"], "wh_eth")
	connect.AssertEqual(t, errors.Is(cryptoAlchemyRegisterAddresses(context.Background(), "", "synthetic-token", addresses), errCryptoAlchemyRegistrationOff), true)
	connect.AssertEqual(t, errors.Is(cryptoAlchemyRegisterAddresses(context.Background(), "wh_eth", "", addresses), errCryptoAlchemyRegistrationOff), true)
	// a refused token is an error, which never names the token
	err := cryptoAlchemyRegisterAddresses(context.Background(), "wh_eth", "wrong-token", addresses)
	connect.AssertEqual(t, err != nil, true)
	connect.AssertEqual(t, strings.Contains(err.Error(), "wrong-token"), false)
}

// ----- the sanctions screen -----

// cryptoTestScreenWallet answers Transaction with receipts that name senders.
type cryptoTestScreenWallet struct {
	*fakeCryptoWallet
	senders map[string][]string
	failing bool
}

func (self *cryptoTestScreenWallet) Transaction(ctx context.Context, txId string) ([]*cryptoWalletReceipt, bool, error) {
	if self.failing {
		return nil, false, errors.New("synthetic: the source is down")
	}
	senders, ok := self.senders[txId]
	if !ok {
		return nil, false, nil
	}
	return []*cryptoWalletReceipt{{TxId: txId, OutputKey: "7", AmountAtomic: 1, Senders: senders}}, true, nil
}

func cryptoTestSanctions(t testing.TB, listed map[string]bool, err error) {
	previous := cryptoSanctionsMatch
	cryptoSanctionsMatch = func(asset string, address string) (bool, string, error) {
		if err != nil {
			return false, "", err
		}
		return listed[address], "20261110-synthetic", nil
	}
	t.Cleanup(func() { cryptoSanctionsMatch = previous })
}

// Before a credit the senders are screened: a match holds the payment
// (sanctions_hold), a sender that cannot be screened holds it too
// (screening_unavailable), a source that cannot be read now waits, and a held
// payment screened clean later is let through. Never credited unscreened.
func TestCryptoScreeningHoldsBeforeCredit(t *testing.T) {
	writes := []string{}
	previousWrite := cryptoSetReceiptsState
	cryptoSetReceiptsState = func(connOwner server.PgConn, ctx context.Context, intentId *server.Id, keys []model.CryptoPaymentReceiptKey, state string, reviewReason string) {
		for _, key := range keys {
			writes = append(writes, key.TxId+" "+state+" "+reviewReason)
		}
	}
	t.Cleanup(func() { cryptoSetReceiptsState = previousWrite })
	wallet := &cryptoTestScreenWallet{
		fakeCryptoWallet: newFakeCryptoWallet(model.CryptoAssetBtc),
		senders: map[string][]string{
			"clean":   {"bc1qclean"},
			"listed":  {"bc1qclean", "bc1qlisted"},
			"unnamed": {"bc1qclean", ""},
			"nobody":  {},
		},
	}
	intentId := server.NewId()
	key := func(txId string) []model.CryptoPaymentReceiptKey {
		return []model.CryptoPaymentReceiptKey{{Asset: model.CryptoAssetBtc, TxId: txId, OutputKey: "7"}}
	}
	cryptoTestSanctions(t, map[string]bool{"bc1qlisted": true}, nil)
	connect.AssertEqual(t, cryptoScreenCredit(context.Background(), model.CryptoAssetBtc, intentId, wallet, key("clean")), cryptoScreenClean)
	connect.AssertEqual(t, cryptoScreenCredit(context.Background(), model.CryptoAssetBtc, intentId, wallet, key("listed")), cryptoScreenHeld)
	connect.AssertEqual(t, cryptoScreenCredit(context.Background(), model.CryptoAssetBtc, intentId, wallet, key("unnamed")), cryptoScreenHeld)
	connect.AssertEqual(t, cryptoScreenCredit(context.Background(), model.CryptoAssetBtc, intentId, wallet, key("nobody")), cryptoScreenHeld)
	connect.AssertEqual(t, writes, []string{
		"listed review sanctions_hold",
		"unnamed review screening_unavailable",
		"nobody review screening_unavailable",
	})
	// a transaction that cannot be read now: the credit waits, nothing is held
	wallet.failing = true
	connect.AssertEqual(t, cryptoScreenCredit(context.Background(), model.CryptoAssetBtc, intentId, wallet, key("clean")), cryptoScreenRetry)
	wallet.failing = false
	// no list loaded: held, never credited unscreened
	cryptoTestSanctions(t, nil, errCryptoSanctionsListUnavailable)
	writes = nil
	connect.AssertEqual(t, cryptoScreenCredit(context.Background(), model.CryptoAssetBtc, intentId, wallet, key("clean")), cryptoScreenHeld)
	connect.AssertEqual(t, writes, []string{"clean review screening_unavailable"})
	// once the list is back, the held payment screens clean and goes back to
	// confirmed for its invoice to be judged again
	cryptoTestSanctions(t, map[string]bool{"bc1qlisted": true}, nil)
	writes = nil
	height := int64(975000)
	held := []*model.CryptoPaymentReceipt{
		{Asset: model.CryptoAssetBtc, TxId: "clean", OutputKey: "7", IntentId: &intentId, BlockHeight: &height, State: model.CryptoReceiptStateReview, ReviewReason: model.CryptoReviewReasonScreeningUnavailable},
	}
	result := &CryptoPaymentScanResult{}
	moved := cryptoRescreenHeld(context.Background(), model.CryptoAssetBtc, wallet, held, result)
	connect.AssertEqual(t, moved, []server.Id{intentId})
	connect.AssertEqual(t, writes, []string{"clean confirmed "})
	connect.AssertEqual(t, result.ScreeningReleased, 1)
	// a sanctions hold is never screened again by the scanner
	held[0].ReviewReason = model.CryptoReviewReasonSanctionsHold
	writes = nil
	connect.AssertEqual(t, len(cryptoRescreenHeld(context.Background(), model.CryptoAssetBtc, wallet, held, result)), 0)
	connect.AssertEqual(t, len(writes), 0)
}

// Every refund destination is screened, in every coin.
func TestCryptoRefundDestinationIsScreened(t *testing.T) {
	cryptoTestSanctions(t, map[string]bool{"bc1qlisted": true}, nil)
	connect.AssertEqual(t, cryptoScreenRefundDestination(model.CryptoAssetBtc, "bc1qclean"), nil)
	err := cryptoScreenRefundDestination(model.CryptoAssetBtc, "bc1qlisted")
	connect.AssertEqual(t, err != nil && strings.Contains(err.Error(), "sanctions list"), true)
	cryptoTestSanctions(t, nil, errCryptoSanctionsListUnavailable)
	connect.AssertEqual(t, cryptoScreenRefundDestination(model.CryptoAssetXmr, "8x") != nil, true)
	connect.AssertEqual(t, cryptoScreenRefundDestination(model.CryptoAssetUsdcSolana, "4Fj9RCwJqHLdLNK28DwWHunHqWapxKbbzeYZLmreSYCM") != nil, true)
}

// ----- the quote endpoint -----

// An item is priced in a coin without an invoice, by the intent's own rules:
// the coin's buffer and decimals, the same refusals; USDC at its price.
func TestCryptoQuotePricesAnItemWithoutAnInvoice(t *testing.T) {
	config := cryptoTestConfig()
	config.Assets[model.CryptoAssetBtc] = &CryptoPayAssetConfig{
		Enabled: true,
		Plans:   []string{model.SolanaPlanYearly, model.SolanaPlanYearlyOnboarding},
		Items:   []string{StripeItemData1Tib, StripeItemData10Tib},
	}
	cryptoTestEnable(t, config, map[string]*fakeCryptoWallet{})
	cryptoWalletSettingsFunc = func(asset string) (*cryptoWalletSettings, error) {
		return &cryptoWalletSettings{
			Asset: asset, ChainRpcUrl: "https://synthetic.invalid/v2/k", Coinbase: &cryptoCoinbaseSettings{Host: "h", KeyName: "k", PrivateKey: "p"},
			WalletRpcUrl: "http://127.0.0.1:1/", WalletRpcUser: "u", WalletRpcPass: "p", PrimaryAddress: "5x", WalletPassword: "w", NodeRpcUrl: "http://127.0.0.1:2/",
		}, nil
	}
	now := server.NowUtc()
	fresh := now.Add(-5 * time.Second)
	height := func(value int64) *int64 { return &value }
	cryptoTestScanCursors(t, map[string]*model.CryptoScanCursor{
		model.CryptoAssetBtc: {Asset: model.CryptoAssetBtc, LastSuccessAt: &fresh, WalletHeight: height(100), NetworkHeight: height(100)},
	})
	cryptoTestRates(t, map[string]map[string]float64{
		cryptoRateSourceKraken:   {model.CryptoAssetBtc: 73596.90},
		cryptoRateSourceCoinbase: {model.CryptoAssetBtc: 73596.90},
	})
	clientSession := &session.ClientSession{Ctx: context.Background()}
	quote, err := CryptoPaymentQuote(&CryptoPaymentQuoteArgs{Asset: model.CryptoAssetBtc, ItemId: StripeItemData1Tib}, clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, quote.Error, (*CryptoPaymentError)(nil))
	price, _ := dataPackPriceUsd(StripeItemData1Tib)
	connect.AssertEqual(t, quote.PriceUsd, price)
	want, _ := cryptoAssetQuoteAmountAtomic(model.CryptoAssetBtc, price, 73596.90, 0.5)
	connect.AssertEqual(t, quote.AmountAtomic, fmt.Sprintf("%d", want))
	connect.AssertEqual(t, quote.Network, "bitcoin")
	connect.AssertEqual(t, quote.QuoteDecimals, 8)
	connect.AssertEqual(t, quote.PaymentLink, true)
	// USDC: at the price, the invoice adds its unique suffix
	quote, err = CryptoPaymentQuote(&CryptoPaymentQuoteArgs{Asset: model.CryptoAssetUsdcSolana, ItemId: StripeItemData1Tib}, clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, quote.RateUsd, 1.0)
	connect.AssertEqual(t, quote.Amount, fmt.Sprintf("%g", price))
	// the refusals of the intent
	refusal := func(args *CryptoPaymentQuoteArgs) string {
		t.Helper()
		result, err := CryptoPaymentQuote(args, clientSession)
		connect.AssertEqual(t, err, nil)
		if result.Error == nil {
			t.Fatalf("%+v was not refused", args)
		}
		return result.Error.Code
	}
	connect.AssertEqual(t, refusal(&CryptoPaymentQuoteArgs{Asset: "ltc", ItemId: StripeItemData1Tib}), PurchaseErrorCodeInvalidRequest)
	connect.AssertEqual(t, refusal(&CryptoPaymentQuoteArgs{Asset: model.CryptoAssetBtc}), PurchaseErrorCodeInvalidRequest)
	connect.AssertEqual(t, refusal(&CryptoPaymentQuoteArgs{Asset: model.CryptoAssetBtc, Plan: model.SolanaPlanMonthly}), PurchaseErrorCodePlanUnavailable)
	connect.AssertEqual(t, refusal(&CryptoPaymentQuoteArgs{Asset: model.CryptoAssetBtc, ItemId: "data_2tib"}), PurchaseErrorCodeItemUnavailable)
	// the welcome offer needs a signed-in network that may redeem it
	connect.AssertEqual(t, refusal(&CryptoPaymentQuoteArgs{Asset: model.CryptoAssetBtc, Plan: model.SolanaPlanYearlyOnboarding}), PurchaseErrorCodeOfferUnavailable)
	// a coin whose rates are down is refused, never quoted on one source
	cryptoTestRates(t, map[string]map[string]float64{cryptoRateSourceKraken: {model.CryptoAssetBtc: 73596.90}})
	connect.AssertEqual(t, refusal(&CryptoPaymentQuoteArgs{Asset: model.CryptoAssetBtc, ItemId: StripeItemData1Tib}), PurchaseErrorCodeAssetUnavailable)
}
