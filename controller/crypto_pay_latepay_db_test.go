package controller

// Database-backed scans of payments to old invoices: a payment to any
// address an invoice of ETH, native SOL or TAO was ever given is recorded,
// however long the invoice has been closed, and judged by the late-payment
// rules; a late payment to an invoice the retention unlink passed is held.
// Each coin's reader reads its recorded chain, as the coin flows do
// (crypto_pay_coins_db_test.go). These tests name only what existed before
// the fix, so they also run, and fail, against the code before it.
//
// Needs the test database (server.DefaultTestEnv) and pro.yml.

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// A coin's recorded chain behind its own reader, and an invoice made three
// days before the scan at the recorded payment's address.
type cryptoOldInvoiceFlow struct {
	asset   string
	address string
	// the rate both sources answer
	rate float64
	// the invoice's item and price: priced at most what the recorded payment
	// is worth at the invoice's own rate (100), so it pays it
	item     string
	priceUsd float64
	settings func() *cryptoWalletSettings
	wallet   func(settings *cryptoWalletSettings, assetConfig *CryptoPayAssetConfig) cryptoWallet
	start    func(t testing.TB)
	// a scan before the invoice is made: a block reader's cursor
	scanFirst bool
}

func runCryptoOldInvoiceFlow(t *testing.T, flow *cryptoOldInvoiceFlow) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cryptoTestResetLimiters()
		if flow.start != nil {
			flow.start(t)
		}
		cryptoTestRates(t, map[string]map[string]float64{
			cryptoRateSourceKraken:   {flow.asset: flow.rate},
			cryptoRateSourceCoinbase: {flow.asset: flow.rate},
		})
		// no sender of the recorded payments is listed
		cryptoTestSanctions(t, map[string]bool{}, nil)
		restoreSkus := model.Testing_SetDataCodeSkus(dataPackTestSkus()...)
		restoreConfig := setCryptoPayConfigForTest(cryptoCoinFlowConfig(flow.asset))
		previousSettings := cryptoWalletSettingsFunc
		previousWalletFor := cryptoWalletFor
		cryptoWalletSettingsFunc = func(asset string) (*cryptoWalletSettings, error) {
			if asset != flow.asset {
				return nil, fmt.Errorf("%s has no chain source in this test", asset)
			}
			return flow.settings(), nil
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
			restoreSkus()
			cryptoWalletSettingsFunc = previousSettings
			cryptoWalletFor = previousWalletFor
			resetCryptoAvailability()
		})

		networkId, _, _ := cryptoDbTestNetwork(t, ctx, "oldinvoice"+flow.asset)
		taskSession := session.Testing_CreateClientSession(ctx, nil)
		if flow.scanFirst {
			scan, err := RunCryptoPaymentScan(taskSession, flow.asset)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, scan.Error, "")
		}

		// an invoice made, quoted and expired three days ago, never paid:
		// out of every watch of the invoices in play
		intent := cryptoDbCreateCoinbaseIntent(t, ctx, flow.asset, &networkId, flow.item, flow.priceUsd, 1, flow.address)
		madeAt := server.NowUtc().Add(-72 * time.Hour)
		cryptoDbExec(
			ctx,
			`UPDATE crypto_payment_intent SET create_time = $2, quoted_at = $2, expires_at = $3 WHERE intent_id = $1`,
			intent.IntentId,
			madeAt,
			madeAt.Add(30*time.Minute),
		)

		scan, err := RunCryptoPaymentScan(taskSession, flow.asset)
		connect.AssertEqual(t, err, nil)
		if scan.Error != "" {
			t.Fatalf("%s scan: %s", flow.asset, scan.Error)
		}
		receipts := model.GetCryptoPaymentReceiptsForIntent(ctx, intent.IntentId)
		if len(receipts) == 0 {
			t.Fatalf("the %s payment to an invoice closed three days ago was never recorded", flow.asset)
		}
		// judged by the late-payment rules: it covers the price, so it is
		// credited, once
		judged := model.GetCryptoPaymentIntent(ctx, intent.IntentId)
		connect.AssertEqual(t, judged.CreditedAt != nil, true)
		connect.AssertEqual(t, scan.Credited, 1)
		for _, receipt := range model.GetCryptoPaymentReceiptsForIntent(ctx, intent.IntentId) {
			connect.AssertEqual(t, receipt.State, model.CryptoReceiptStateCredited)
		}
	})
}

// ETH: the two recorded transfers to 0xd8dA…6045, read by the scan although
// their invoice closed three days ago.
func TestCryptoPaymentToAnOldEthereumInvoiceIsRecorded(t *testing.T) {
	var endpoint string
	runCryptoOldInvoiceFlow(t, &cryptoOldInvoiceFlow{
		asset:   model.CryptoAssetEth,
		address: "0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045",
		rate:    10000,
		// the recorded 0.0203 ETH pays a 0.01 ETH invoice
		item:     StripeItemData1Tib,
		priceUsd: 1,
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

// SOL: the recorded native transfer of 108,600 lamports to 5tzFki…uAi9, read
// by the scan although its invoice closed three days ago.
func TestCryptoPaymentToAnOldSolanaInvoiceIsRecorded(t *testing.T) {
	var rpc *fakeSolanaRpc
	runCryptoOldInvoiceFlow(t, &cryptoOldInvoiceFlow{
		asset:   model.CryptoAssetSol,
		address: solanaFixtureWallet,
		rate:    1_000_000,
		// 108,600 lamports pay a 100,500 lamport invoice
		item:     StripeItemData1Tib,
		priceUsd: 0.01,
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

// TAO: the recorded Balances.transfer_keep_alive of 1 TAO, in a block mined
// now, read by the scan although its invoice closed three days ago, and
// credited as a late payment at its first-seen rate.
func TestCryptoPaymentToAnOldBittensorInvoiceIsRecorded(t *testing.T) {
	plain := cryptoTaoTestPlainTransfer(t)
	signer, _ := cryptoTaoTestSigner(t, plain)
	invoice := cryptoTaoTestAccount(0x53)
	recorded := cryptoTaoTestRecorded(t)
	const paymentBlock = uint64(9478400)
	node := newFakeCryptoTaoNode()
	node.addBlock(t, paymentBlock-1, 488, cryptoTaoTestEvents(t, cryptoTaoTestMeta(t), nil), time.Now(), []string{recorded.Extrinsics[0]})
	cryptoTaoTestPlainBlock(t, node, paymentBlock, plain, []cryptoTaoTestTransfer{
		{extrinsic: 1, from: signer, to: invoice, amount: 1_000_000_000},
	}, time.Now())
	node.finalized, node.best = paymentBlock, paymentBlock+2
	node.serveMetadata(t, paymentBlock-1, paymentBlock)

	runCryptoOldInvoiceFlow(t, &cryptoOldInvoiceFlow{
		asset:   model.CryptoAssetTao,
		address: cryptoTaoAddressOf(invoice),
		// 1 TAO at $400 covers the $40 year
		rate:     400,
		item:     model.SolanaPlanYearly,
		priceUsd: 40,
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
		// the cursor at the payment block, before the invoice is made
		scanFirst: true,
	})
}

// A late payment the rules would credit, to an XMR invoice the retention
// unlink passed (its network forgotten), is held by the scan as unlinked at
// once: no credit is tried, none fails, and payment-received-held reports it.
func TestCryptoLatePaymentToAnUnlinkedInvoiceIsHeldByTheScan(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cryptoTestResetLimiters()
		cryptoTestStandardRates(t)
		wallet := newFakeCryptoWallet(model.CryptoAssetXmr)
		cryptoTestEnable(t, cryptoTestConfig(), map[string]*fakeCryptoWallet{model.CryptoAssetXmr: wallet})
		networkId, _, _ := cryptoDbTestNetwork(t, ctx, "unlinkedlate")
		taskSession := session.Testing_CreateClientSession(ctx, nil)

		intent := cryptoDbCreateIntent(t, ctx, model.CryptoAssetXmr, &networkId, model.SolanaPlanYearly, 40, 7)
		// made forty days ago, never paid, and past the retention unlink
		madeAt := server.NowUtc().Add(-40 * 24 * time.Hour)
		cryptoDbExec(
			ctx,
			`UPDATE crypto_payment_intent SET create_time = $2, quoted_at = $2, expires_at = $3, unlinked_at = $4, network_id = NULL WHERE intent_id = $1`,
			intent.IntentId,
			madeAt,
			madeAt.Add(30*time.Minute),
			server.NowUtc().Add(-10*24*time.Hour),
		)
		// paid now, twice the quoted amount: it covers the price today
		wallet.setHeight(100)
		wallet.pay("xmr-late-unlinked", 7, uint64(2*intent.AmountAtomic))
		wallet.mine("xmr-late-unlinked", 100)

		scan, err := RunCryptoPaymentScan(taskSession, model.CryptoAssetXmr)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, scan.Error, "")
		connect.AssertEqual(t, scan.Credited, 0)
		connect.AssertEqual(t, scan.CreditErrored, 0)
		receipts := model.GetCryptoPaymentReceiptsForIntent(ctx, intent.IntentId)
		connect.AssertEqual(t, len(receipts), 1)
		connect.AssertEqual(t, receipts[0].State, model.CryptoReceiptStateReview)
		connect.AssertEqual(t, receipts[0].ReviewReason, "unlinked")
		held := model.GetCryptoPaymentIntent(ctx, intent.IntentId)
		connect.AssertEqual(t, held.CreditedAt == nil, true)
		connect.AssertEqual(t, held.CreditFailureCount, 0)

		// the next scan leaves it held: nothing tried, nothing failed
		scan, err = RunCryptoPaymentScan(taskSession, model.CryptoAssetXmr)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, scan.CreditErrored, 0)
		connect.AssertEqual(t, model.GetCryptoPaymentIntent(ctx, intent.IntentId).CreditFailureCount, 0)
	})
}
