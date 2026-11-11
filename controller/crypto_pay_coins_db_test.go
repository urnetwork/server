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
	// a coin whose reader scans blocks (Bitcoin): it watches no address list
	blockScanner bool
	// called before the scan that credits: a block scanner's chain moves on
	// to the block that holds the payment, mined after the invoice was made
	beforeCredit func(t testing.TB)
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
			// no memo: the invoice's own deposit address identifies it; its
			// Solana Pay reference is in the link only
			connect.AssertEqual(t, result.Memo, "")
			reference := cryptoSolReference(server.RequireParseId(result.IntentId))
			connect.AssertEqual(t, strings.HasPrefix(result.Uri, "solana:"+flow.address+"?amount="+result.Amount+"&reference="+reference+"&label=URnetwork&message="), true)
		case model.CryptoAssetTao:
			connect.AssertEqual(t, result.Uri, "")
			connect.AssertEqual(t, result.UriNote != "", true)
		}
		connect.AssertEqual(t, result.Status.Status, CryptoPaymentStatusPending)

		// the scan reads the invoice's address on the chain (a block scanner:
		// the next block, which holds the payment): the recorded payment is
		// screened clean and credited
		if flow.beforeCredit != nil {
			flow.beforeCredit(t)
		}
		scan, err = RunCryptoPaymentScan(taskSession, flow.asset)
		connect.AssertEqual(t, err, nil)
		if scan.Error != "" {
			t.Fatalf("%s scan: %s", flow.asset, scan.Error)
		}
		if flow.blockScanner {
			connect.AssertEqual(t, scan.Watched, 0)
		} else {
			connect.AssertEqual(t, scan.Watched, 1)
		}
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
		// the balance's term and Manage subscription name the coin (SOL is
		// not USDC, though both are the solana market)
		term := cryptoTermForNetwork(ctx, networkId, server.NowUtc())
		connect.AssertEqual(t, term.Market, flow.market)
		connect.AssertEqual(t, term.Asset, flow.asset)
		connect.AssertEqual(t, model.CryptoRenewalAsset(ctx, flow.market, renewal.transactionId), flow.asset)

		// scanned again: nothing new, credited once
		scan, err = RunCryptoPaymentScan(taskSession, flow.asset)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, scan.NewReceipts, 0)
		connect.AssertEqual(t, scan.Credited, 0)
		connect.AssertEqual(t, cryptoDbRenewals(t, ctx, networkId, flow.market).count, 1)
	})
}

// Bitcoin: the recorded payment of 140,682 sats to
// bc1qxpjhlx9w22nccplvvtpxs5hlv768c92409cvew in block 975401, found by the
// block scanner reading the block through Alchemy's JSON-RPC (getblock at
// verbosity 2), its sender read from the transaction's prevout
// (getrawtransaction at verbosity 2). The first scan reads up to 975400,
// before the invoice; 975401 is mined after it.
func TestCryptoCoinFlowBitcoin(t *testing.T) {
	var node *cryptoTestBitcoinNode
	var endpoint string
	runCryptoCoinFlow(t, &cryptoCoinFlow{
		asset:        model.CryptoAssetBtc,
		code:         "BTC",
		network:      "bitcoin",
		address:      cryptoTestBtcP2wpkhSingle,
		rate:         1_000_000,
		paidAtomic:   140_682,
		market:       model.SubscriptionMarketBitcoin,
		blockScanner: true,
		settings: func() *cryptoWalletSettings {
			return &cryptoWalletSettings{Asset: model.CryptoAssetBtc, ChainRpcUrl: endpoint}
		},
		start: func(t testing.TB) {
			node = newCryptoTestBitcoinNode(t, 975400)
			// the recorded blocks read as mined in the last minutes: 975401
			// a minute ago
			node.shift = time.Now().Unix() - 1794380897 - 60
			endpoint = node.serve()
		},
		beforeCredit: func(t testing.TB) {
			node.setTip(975401)
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

// cryptoDbCreateCoinbaseIntent stores an open invoice of a Coinbase coin for
// a network without the controller: its pool row is a deposit address the
// refill made, named with the invoice id it reserved, and the invoice takes
// that id when it draws the address.
func cryptoDbCreateCoinbaseIntent(t testing.TB, ctx context.Context, asset string, networkId *server.Id, item string, priceUsd float64, index int64, address string) *model.CryptoPaymentIntent {
	reserved := server.NewId()
	model.AddCryptoPaymentAddresses(ctx, []*model.CryptoPaymentAddress{{Asset: asset, AddressIndex: index, Address: address, ReservedIntentId: &reserved}})
	_, tokenHash := cryptoNewStatusToken()
	now := server.NowUtc()
	rate := 100.0
	amountAtomic, ok := cryptoAssetQuoteAmountAtomic(asset, priceUsd, rate, 0.5)
	connect.AssertEqual(t, ok, true)
	intent := &model.CryptoPaymentIntent{
		IntentId:                 server.NewId(),
		StatusTokenHash:          tokenHash,
		Asset:                    asset,
		Delivery:                 model.CryptoDeliveryAccount,
		NetworkId:                networkId,
		Item:                     item,
		Tier:                     "standard",
		PriceUsdNanoCents:        model.UsdToNanoCents(priceUsd),
		RegularPriceUsdNanoCents: model.UsdToNanoCents(priceUsd),
		AmountAtomic:             amountAtomic,
		RateUsd:                  rate,
		QuotedAt:                 now,
		ExpiresAt:                now.Add(30 * time.Minute),
	}
	connect.AssertEqual(t, model.CreateCryptoPaymentIntentWithAddress(ctx, intent, true), nil)
	connect.AssertEqual(t, intent.IntentId, reserved)
	connect.AssertEqual(t, *intent.AddressIndex, index)
	return intent
}

// The 30-day unlink covers the Coinbase coins as it covers Monero (ur.io's
// FAQ: a crypto payment's link to its account is removed after 30 days). A
// SOL year paid to its own deposit address forgets its network and its exact
// times and its pool row's, and its renewal keeps only its coin, so the term
// and Manage subscription still read SOL (not USDC, whose market it shares).
// A Bitcoin payment held by the sanctions screen forgets its network and
// exact times and stays held for the owner and counsel. The screen keeps no
// other record: a hold is its receipts' state and reason, and the senders
// it read are never stored.
func TestCryptoRetentionUnlinkCoversTheCoinbaseCoins(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		config := cryptoTestConfig()
		for _, asset := range []string{model.CryptoAssetSol, model.CryptoAssetBtc} {
			config.Assets[asset] = &CryptoPayAssetConfig{
				Enabled: true,
				Plans:   []string{model.SolanaPlanYearly, model.SolanaPlanYearlyOnboarding},
				Items:   []string{StripeItemData1Tib, StripeItemData10Tib},
			}
		}
		cryptoTestEnable(t, config, map[string]*fakeCryptoWallet{})
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "cryptounlinkcoinbase")

		sol := cryptoDbCreateCoinbaseIntent(t, ctx, model.CryptoAssetSol, &networkId, model.SolanaPlanYearly, 40, 601, solanaFixtureWallet)
		solReceipt := cryptoDbReceipt(t, ctx, sol, "sol-unlink-signature", sol.AmountAtomic, 1)
		credited, err := cryptoCreditIntent(clientSession, sol, &cryptoCredit{usdNanoCents: sol.PriceUsdNanoCents, receipts: []model.CryptoPaymentReceiptKey{solReceipt.Key()}})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, credited, true)
		term := cryptoTermForNetwork(ctx, networkId, server.NowUtc())
		connect.AssertEqual(t, term.Market, model.SubscriptionMarketSolana)
		connect.AssertEqual(t, term.Asset, model.CryptoAssetSol)

		held := cryptoDbCreateCoinbaseIntent(t, ctx, model.CryptoAssetBtc, &networkId, StripeItemData1Tib, 3, 602, "bc1q0wd209cv5k9pd9mhk7nspacywcj038xxdhnt5u")
		heldReceipt := cryptoTestReceipt(held, "btc-unlink-held", held.AmountAtomic, 1, server.NowUtc(), model.CryptoReceiptStateReview)
		heldReceipt.ReviewReason = model.CryptoReviewReasonSanctionsHold
		inserted, _ := model.UpsertCryptoPaymentReceiptInConn(nil, ctx, heldReceipt)
		connect.AssertEqual(t, inserted, true)

		// recent ones are kept linked
		result, err := RunCryptoPaymentUnlink(clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Unlinked, 0)
		cryptoDbExec(ctx, `UPDATE crypto_payment_intent SET credited_at = $2 WHERE intent_id = $1`, sol.IntentId, server.NowUtc().Add(-721*time.Hour))
		cryptoDbExec(ctx, `UPDATE crypto_payment_intent SET create_time = $2 WHERE intent_id = $1`, held.IntentId, server.NowUtc().Add(-721*time.Hour))
		result, err = RunCryptoPaymentUnlink(clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Unlinked, 2)

		day := func(value time.Time) bool { return value.Equal(value.Truncate(24 * time.Hour)) }
		for _, intentId := range []server.Id{sol.IntentId, held.IntentId} {
			unlinked := model.GetCryptoPaymentIntent(ctx, intentId)
			connect.AssertEqual(t, unlinked.NetworkId, (*server.Id)(nil))
			connect.AssertEqual(t, unlinked.UnlinkedAt != nil, true)
			connect.AssertEqual(t, day(unlinked.CreateTime), true)
			for _, receipt := range model.GetCryptoPaymentReceiptsForIntent(ctx, intentId) {
				connect.AssertEqual(t, day(receipt.FirstSeenAt), true)
				connect.AssertEqual(t, day(receipt.UpdateTime), true)
			}
			// the Coinbase pool row: its allocation is the day; it still
			// names the invoice it was made for (the address's name at
			// Coinbase), which names no network now
			var allocatedAt time.Time
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT allocated_at FROM crypto_payment_address WHERE intent_id = $1`, intentId).Scan(&allocatedAt))
			})
			connect.AssertEqual(t, day(allocatedAt), true)
		}
		// the held payment stays held: the owner and counsel decide it
		heldReceipts := model.GetCryptoPaymentReceiptsForIntent(ctx, held.IntentId)
		connect.AssertEqual(t, len(heldReceipts), 1)
		connect.AssertEqual(t, heldReceipts[0].State, model.CryptoReceiptStateReview)
		connect.AssertEqual(t, heldReceipts[0].ReviewReason, model.CryptoReviewReasonSanctionsHold)

		// the SOL renewal lost its link to the invoice, not its coin
		renewal := cryptoDbRenewals(t, ctx, networkId, model.SubscriptionMarketSolana)
		connect.AssertEqual(t, renewal.count, 1)
		connect.AssertEqual(t, renewal.transactionId != sol.IntentId.String(), true)
		connect.AssertEqual(t, strings.HasPrefix(renewal.transactionId, model.CryptoUnlinkedSolTransactionPrefix), true)
		connect.AssertEqual(t, strings.Contains(renewal.transactionId, sol.IntentId.String()), false)
		term = cryptoTermForNetwork(ctx, networkId, server.NowUtc())
		connect.AssertEqual(t, term.Asset, model.CryptoAssetSol)
		details, err := SubscriptionDetails(clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(details.Subscriptions), 1)
		connect.AssertEqual(t, details.Subscriptions[0].Store, model.SubscriptionMarketSolana)
		connect.AssertEqual(t, details.Subscriptions[0].CryptoAsset, model.CryptoAssetSol)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)
	})
}
