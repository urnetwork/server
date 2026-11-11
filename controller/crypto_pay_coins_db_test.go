package controller

// Database-backed API flows of the coins received through Coinbase: TAO,
// Bitcoin, ETH and SOL. Each makes an invoice through POST /pay/crypto/intent,
// which draws a Coinbase deposit address named with the invoice's id; the
// coin's own reader reads a recorded payment to that address from its chain;
// the scanner credits it; and POST /pay/crypto/status answers paid, with the
// excess. Bitcoin, ETH and SOL replay payments recorded from mainnet (the
// fixtures of crypto_pay_coins_readers_test.go and
// crypto_pay_solana_native_test.go); TAO replays a recorded Finney
// transfer_keep_alive in a block of the reader's test node. USDC on Solana,
// Zcash and Monero have theirs in crypto_pay_db_test.go
// (TestCryptoUsdcThroughHelius, TestCryptoScannerEndToEnd).
//
// Needs the test database (server.DefaultTestEnv) and pro.yml.

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// cryptoCoinFlow is one coin's recorded chain behind its own reader.
type cryptoCoinFlow struct {
	asset string
	// Coinbase's currency code and network for the coin
	code    string
	network string
	// the recorded payment's address, the deposit address Coinbase answers
	address string
	// the rate both sources answer: low enough that any configured price
	// up to the invoice cap is below the recorded payment
	rate float64
	// what the recorded payment moved to the address, in atomic units
	paidAtomic int64
	market     string
	// the coin's reader over its recorded chain, with the vault's settings;
	// nil builds it as production does (cryptoWalletFor)
	wallet func(settings *cryptoWalletSettings, assetConfig *CryptoPayAssetConfig) cryptoWallet
	// the settings the vault would give, but the Coinbase key
	settings func() *cryptoWalletSettings
	// called at the start of each attempt: the chain clock and the recorded
	// chain's times
	start func(t testing.TB)
}

// cryptoCoinFlowConfig is the coin alone, on, with the contract's numbers and
// one Coinbase address (the recorded one) in its pool.
func cryptoCoinFlowConfig(asset string) *CryptoPayConfig {
	config := cryptoTestConfig()
	config.Assets = map[string]*CryptoPayAssetConfig{
		asset: {
			Enabled:           true,
			Network:           cryptoNetworkMainnet,
			Plans:             []string{model.SolanaPlanYearly, model.SolanaPlanYearlyOnboarding},
			Items:             []string{StripeItemData1Tib, StripeItemData10Tib},
			RateSources:       []string{cryptoRateSourceKraken, cryptoRateSourceCoinbase},
			AddressPoolTarget: 1,
		},
	}
	return config
}

func runCryptoCoinFlow(t *testing.T, flow *cryptoCoinFlow) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cryptoTestResetLimiters()
		if flow.start != nil {
			flow.start(t)
		}
		fake, coinbaseSettings := cryptoTestCoinbase(t)
		fake.accounts[flow.code] = "22222222-2222-2222-2222-222222222222"
		fake.answers = []string{flow.address}
		cryptoTestRates(t, map[string]map[string]float64{
			cryptoRateSourceKraken:   {flow.asset: flow.rate},
			cryptoRateSourceCoinbase: {flow.asset: flow.rate},
		})
		// no sender of the recorded payments is listed
		cryptoTestSanctions(t, map[string]bool{}, nil)

		config := cryptoCoinFlowConfig(flow.asset)
		restoreConfig := setCryptoPayConfigForTest(config)
		previousSettings := cryptoWalletSettingsFunc
		previousWalletFor := cryptoWalletFor
		cryptoWalletSettingsFunc = func(asset string) (*cryptoWalletSettings, error) {
			if asset != flow.asset {
				return nil, fmt.Errorf("%s has no chain source in this test", asset)
			}
			settings := flow.settings()
			settings.Coinbase = coinbaseSettings
			return settings, nil
		}
		if flow.wallet != nil {
			cryptoWalletFor = func(asset string, settings *cryptoWalletSettings, assetConfig *CryptoPayAssetConfig) (cryptoWallet, error) {
				if asset == flow.asset {
					return flow.wallet(settings, assetConfig), nil
				}
				return previousWalletFor(asset, settings, assetConfig)
			}
		}
		resetCryptoAvailability()
		t.Cleanup(func() {
			restoreConfig()
			cryptoWalletSettingsFunc = previousSettings
			cryptoWalletFor = previousWalletFor
			resetCryptoAvailability()
		})

		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "cryptoflow"+flow.asset)
		taskSession := session.Testing_CreateClientSession(ctx, nil)

		// the refill asks Coinbase for one deposit address, named with the id
		// of the invoice that will draw it
		refill, err := RunCryptoAddressRefill(taskSession, flow.asset)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, refill.Added, 1)
		connect.AssertEqual(t, len(fake.created), 1)
		connect.AssertEqual(t, fake.created[0]["network"], flow.network)
		// the first scan watches nothing yet; it makes the coin sellable
		scan, err := RunCryptoPaymentScan(taskSession, flow.asset)
		connect.AssertEqual(t, err, nil)
		if scan.Error != "" {
			t.Fatalf("first %s scan: %s", flow.asset, scan.Error)
		}
		resetCryptoAvailability()

		// listed with its network and decimals
		assets, err := CryptoPaymentAssets(clientSession)
		connect.AssertEqual(t, err, nil)
		var listed *CryptoPaymentAsset
		for _, asset := range assets.Assets {
			if asset.Asset == flow.asset {
				listed = asset
			}
		}
		if listed == nil {
			t.Fatalf("%s is not listed as available: %+v", flow.asset, assets.UnavailableAssets)
		}
		connect.AssertEqual(t, listed.Network, cryptoAssetNetwork(flow.asset))
		connect.AssertEqual(t, listed.Decimals, cryptoAssetDecimals(flow.asset))

		// the quote and the invoice agree
		quote, err := CryptoPaymentQuote(&CryptoPaymentQuoteArgs{Asset: flow.asset, Plan: model.SolanaPlanYearly}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, quote.Error, (*CryptoPaymentError)(nil))
		result, err := CreateCryptoPaymentIntent(&CreateCryptoPaymentIntentArgs{Asset: flow.asset, Plan: model.SolanaPlanYearly}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, (*CryptoPaymentError)(nil))
		connect.AssertEqual(t, result.AmountAtomic, quote.AmountAtomic)
		expected, ok := cryptoAssetQuoteAmountAtomic(flow.asset, result.PriceUsd, flow.rate, config.BufferPercentFor(flow.asset))
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, result.AmountAtomic, fmt.Sprintf("%d", expected))
		if flow.paidAtomic < expected {
			t.Fatalf("the recorded %s payment (%d) is below the invoice (%d): lower the test rate", flow.asset, flow.paidAtomic, expected)
		}
		// the invoice is the one the address was named for, at that address
		connect.AssertEqual(t, result.IntentId, fake.created[0]["name"])
		connect.AssertEqual(t, result.Address, flow.address)
		connect.AssertEqual(t, result.Network, cryptoAssetNetwork(flow.asset))
		connect.AssertEqual(t, result.NetworkWarning != "", true)
		switch flow.asset {
		case model.CryptoAssetBtc:
			connect.AssertEqual(t, strings.HasPrefix(result.Uri, "bitcoin:"+flow.address+"?amount="+result.Amount+"&label=URnetwork&message="), true)
		case model.CryptoAssetEth:
			connect.AssertEqual(t, result.Uri, "ethereum:"+flow.address+"@1?value="+result.AmountAtomic)
		case model.CryptoAssetSol:
			connect.AssertEqual(t, result.Memo, cryptoSolReference(server.RequireParseId(result.IntentId)))
			connect.AssertEqual(t, strings.HasPrefix(result.Uri, "solana:"+flow.address+"?amount="+result.Amount+"&reference="+result.Memo+"&label=URnetwork&message="), true)
		case model.CryptoAssetTao:
			connect.AssertEqual(t, result.Uri, "")
			connect.AssertEqual(t, result.UriNote != "", true)
		}
		connect.AssertEqual(t, result.Status.Status, CryptoPaymentStatusPending)

		// the scan reads the invoice's address on the chain: the recorded
		// payment is final, screened clean and credited
		scan, err = RunCryptoPaymentScan(taskSession, flow.asset)
		connect.AssertEqual(t, err, nil)
		if scan.Error != "" {
			t.Fatalf("%s scan: %s", flow.asset, scan.Error)
		}
		connect.AssertEqual(t, scan.Watched, 1)
		connect.AssertEqual(t, scan.Credited, 1)

		status, err := CryptoPaymentStatus(&CryptoPaymentStatusArgs{IntentId: result.IntentId, StatusToken: result.StatusToken}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, status.Status, CryptoPaymentStatusPaid)
		connect.AssertEqual(t, status.ReviewReason, "")
		connect.AssertEqual(t, status.CreditedAt != nil, true)
		connect.AssertEqual(t, status.Network, cryptoAssetNetwork(flow.asset))
		connect.AssertEqual(t, status.ReceivedAtomic, fmt.Sprintf("%d", flow.paidAtomic))
		connect.AssertEqual(t, status.ExcessAtomic, fmt.Sprintf("%d", flow.paidAtomic-expected))
		connect.AssertEqual(t, status.RemainingAtomic, "0")
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)
		renewal := cryptoDbRenewals(t, ctx, networkId, flow.market)
		connect.AssertEqual(t, renewal.count, 1)
		connect.AssertEqual(t, renewal.netRevenue, model.UsdToNanoCents(result.PriceUsd))

		// scanned again: nothing new, credited once
		scan, err = RunCryptoPaymentScan(taskSession, flow.asset)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, scan.NewReceipts, 0)
		connect.AssertEqual(t, scan.Credited, 0)
		connect.AssertEqual(t, cryptoDbRenewals(t, ctx, networkId, flow.market).count, 1)
	})
}

// Bitcoin: the recorded payment of 17,657,843 sats to
// bc1q0wd209cv5k9pd9mhk7nspacywcj038xxdhnt5u (block 860860), read from the
// address history of Alchemy's UTXO API.
func TestCryptoCoinFlowBitcoin(t *testing.T) {
	var endpoint string
	runCryptoCoinFlow(t, &cryptoCoinFlow{
		asset:      model.CryptoAssetBtc,
		code:       "BTC",
		network:    "bitcoin",
		address:    "bc1q0wd209cv5k9pd9mhk7nspacywcj038xxdhnt5u",
		rate:       73596.9,
		paidAtomic: 17_657_843,
		market:     model.SubscriptionMarketBitcoin,
		settings: func() *cryptoWalletSettings {
			return &cryptoWalletSettings{Asset: model.CryptoAssetBtc, ChainRpcUrl: endpoint}
		},
		start: func(t testing.TB) {
			testServer := cryptoTestBitcoinServer(t, false)
			endpoint = testServer.URL + "/v2/synthetic-key"
			// the recorded tip's time
			cryptoTestChainClock(t, time.Unix(1794350912, 0).Add(10*time.Minute))
		},
	})
}

// ETH: the two recorded transfers to 0xd8dA…6045 (an external send and a
// contract's internal one, 13,808,775,157,042,675 wei together), both below
// the finalized block, read with alchemy_getAssetTransfers.
func TestCryptoCoinFlowEthereum(t *testing.T) {
	var endpoint string
	runCryptoCoinFlow(t, &cryptoCoinFlow{
		asset:      model.CryptoAssetEth,
		code:       "ETH",
		network:    "ethereum",
		address:    "0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045",
		rate:       10000,
		paidAtomic: 0x01884008f415fc + 0x2f86c1f910b7f7,
		market:     model.SubscriptionMarketEthereum,
		settings: func() *cryptoWalletSettings {
			return &cryptoWalletSettings{Asset: model.CryptoAssetEth, ChainRpcUrl: endpoint}
		},
		start: func(t testing.TB) {
			fake, _ := cryptoTestEthereum(t)
			testServer := httptest.NewServer(fake)
			t.Cleanup(testServer.Close)
			endpoint = testServer.URL + "/v2/synthetic-key"
			// the recorded latest block's time
			cryptoTestChainClock(t, time.Unix(0x6a5f5d73, 0).Add(time.Minute))
		},
	})
}

// SOL: the recorded native transfer of 108,600 lamports to the exchange
// wallet 5tzFki…uAi9 (slot 467670518), read with getSignaturesForAddress at
// the finalized commitment.
func TestCryptoCoinFlowSolana(t *testing.T) {
	var rpc *fakeSolanaRpc
	runCryptoCoinFlow(t, &cryptoCoinFlow{
		asset:      model.CryptoAssetSol,
		code:       "SOL",
		network:    "solana",
		address:    solanaFixtureWallet,
		rate:       1_000_000,
		paidAtomic: 108_600,
		market:     model.SubscriptionMarketSolana,
		settings: func() *cryptoWalletSettings {
			return &cryptoWalletSettings{Asset: model.CryptoAssetSol, ChainRpcUrl: "https://helius.invalid/?api-key=synthetic"}
		},
		wallet: func(settings *cryptoWalletSettings, assetConfig *CryptoPayAssetConfig) cryptoWallet {
			return &solanaNativeWallet{
				cryptoCoinbaseAddresses: newCryptoCoinbaseAddresses(model.CryptoAssetSol, settings, cryptoSolanaAddressCanonical),
				network:                 assetConfig.Network,
				rpc:                     rpc,
			}
		},
		start: func(t testing.TB) {
			rpc = newFakeSolanaRpc(t)
			rpc.signatures[solanaFixtureWallet] = "sol_signatures.json"
			rpc.transactions[solanaFixtureNativeSignature] = "sol_tx_native.json"
			rpc.transactions[solanaFixtureUsdcSignature] = "sol_tx_usdc.json"
			rpc.finalized = 467670518 + 10
			rpc.confirmed = 467670518 + 42
			rpc.blockTime = time.Now().Add(-10 * time.Second).Unix()
			solanaParsedTransactions.reset()
			t.Cleanup(solanaParsedTransactions.reset)
		},
	})
}

// TAO: the recorded Finney Balances.transfer_keep_alive, signed by its
// sender, paying 1 TAO to the invoice's address in a finalized block of
// runtime 488, decoded with that runtime's own metadata.
func TestCryptoCoinFlowBittensor(t *testing.T) {
	// the blocks are built here, on the test goroutine (the helpers fail
	// the *testing.T); each attempt gives them fresh times
	plain := cryptoTaoTestPlainTransfer(t)
	signer, _ := cryptoTaoTestSigner(t, plain)
	invoice := cryptoTaoTestAccount(0x52)
	recorded := cryptoTaoTestRecorded(t)
	const paymentBlock = uint64(9478400)
	node := newFakeCryptoTaoNode()
	node.addBlock(t, paymentBlock-1, 488, cryptoTaoTestEvents(t, cryptoTaoTestMeta(t), nil), time.Now(), []string{recorded.Extrinsics[0]})
	cryptoTaoTestPlainBlock(t, node, paymentBlock, plain, []cryptoTaoTestTransfer{
		{extrinsic: 1, from: signer, to: invoice, amount: 1_000_000_000},
	}, time.Now())
	node.finalized, node.best = paymentBlock, paymentBlock+2
	node.serveMetadata(t, paymentBlock-1, paymentBlock)

	runCryptoCoinFlow(t, &cryptoCoinFlow{
		asset:      model.CryptoAssetTao,
		code:       "TAO",
		network:    "bittensor",
		address:    cryptoTaoAddressOf(invoice),
		rate:       400,
		paidAtomic: 1_000_000_000,
		market:     model.SubscriptionMarketBittensor,
		settings: func() *cryptoWalletSettings {
			return &cryptoWalletSettings{Asset: model.CryptoAssetTao, ChainRpcUrl: "ws://fake", ChainProfile: cryptoNetworkMainnet}
		},
		wallet: func(settings *cryptoWalletSettings, assetConfig *CryptoPayAssetConfig) cryptoWallet {
			wallet := newBittensorWallet(settings, assetConfig)
			wallet.rpc = node
			return wallet
		},
		start: func(t testing.TB) {
			// the reader judges finality's age by the clock: blocks of now
			resetCryptoTaoCaches()
			t.Cleanup(resetCryptoTaoCaches)
			node.stateLock.Lock()
			for number, block := range node.blocks {
				block.timestamp = cryptoTaoTestTimestamp(time.Now().Add(-time.Duration(paymentBlock-number+1) * 12 * time.Second))
			}
			node.stateLock.Unlock()
		},
	})
}
