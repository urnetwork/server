package controller

// Database-backed tests of the crypto payment rail: the credit's one-shot
// under concurrency, receipt idempotency, the address pool under concurrency,
// the scanner end to end against the fake daemon, late credits, the grant's
// renewal, balance and revenue fields, the welcome offer recorded inside the
// credit, the retention unlink, USDC through the Helius webhook, and the
// reconcile legs. Needs the test database (server.DefaultTestEnv) and
// pro.yml; the hermetic pieces are in crypto_pay_test.go.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// cryptoDbTestNetwork is a network with an email sign-in and a session of it.
func cryptoDbTestNetwork(t testing.TB, ctx context.Context, name string) (server.Id, string, *session.ClientSession) {
	networkId := server.NewId()
	userId := server.NewId()
	adminEmail := model.Testing_CreateNetwork(ctx, networkId, name, userId)
	clientId := server.NewId()
	clientSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{
		NetworkId: networkId,
		UserId:    userId,
		ClientId:  &clientId,
	})
	return networkId, adminEmail, clientSession
}

// cryptoTestResetLimiters starts the per-address and per-network windows
// afresh: every test session shares one client address.
func cryptoTestResetLimiters() {
	for _, limiter := range []*payDataIpLimiter{payDataCheckoutLimiter, payDataLookupLimiter, cryptoPayNetworkLimiter} {
		limiter.stateLock.Lock()
		limiter.window = time.Time{}
		limiter.stateLock.Unlock()
	}
	cryptoCodeIntentLimiter.reset()
}

func cryptoDbExec(ctx context.Context, sql string, args ...any) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, sql, args...))
	})
}

// cryptoDbRenewal is a network's renewal row in a market, with the fields the
// credit writes.
type cryptoDbRenewal struct {
	startTime     time.Time
	endTime       time.Time
	netRevenue    model.NanoCents
	transactionId string
	priceTier     *string
	count         int
}

func cryptoDbRenewals(t testing.TB, ctx context.Context, networkId server.Id, market string) *cryptoDbRenewal {
	renewal := &cryptoDbRenewal{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT start_time, end_time, net_revenue_nano_cents, COALESCE(transaction_id, ''), price_tier
				FROM subscription_renewal
				WHERE network_id = $1 AND market = $2
				ORDER BY start_time
			`,
			networkId,
			market,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				server.Raise(result.Scan(&renewal.startTime, &renewal.endTime, &renewal.netRevenue, &renewal.transactionId, &renewal.priceTier))
				renewal.count += 1
			}
		})
	})
	return renewal
}

// cryptoDbCreateIntent stores an open xmr or zec invoice for a network
// without the controller, with its pool address.
func cryptoDbCreateIntent(t testing.TB, ctx context.Context, asset string, networkId *server.Id, item string, priceUsd float64, index int64) *model.CryptoPaymentIntent {
	_, tokenHash := cryptoNewStatusToken()
	now := server.NowUtc()
	model.AddCryptoPaymentAddresses(ctx, []*model.CryptoPaymentAddress{{Asset: asset, AddressIndex: index, Address: fakeCryptoAddress(asset, uint64(index))}})
	rate := 555.36
	if asset == model.CryptoAssetZec {
		rate = 1304.60
	}
	amountAtomic, ok := cryptoQuoteAmountAtomic(priceUsd, rate, 0.5, cryptoAssetDecimals(asset))
	connect.AssertEqual(t, ok, true)
	delivery := model.CryptoDeliveryAccount
	if networkId == nil {
		delivery = model.CryptoDeliveryCode
	}
	intent := &model.CryptoPaymentIntent{
		IntentId:                 server.NewId(),
		StatusTokenHash:          tokenHash,
		Asset:                    asset,
		Delivery:                 delivery,
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
	connect.AssertEqual(t, *intent.AddressIndex, index)
	return intent
}

// cryptoDbReceipt records a confirmed receipt of the full amount for an
// invoice.
func cryptoDbReceipt(t testing.TB, ctx context.Context, intent *model.CryptoPaymentIntent, txId string, amountAtomic int64, confirmations int) *model.CryptoPaymentReceipt {
	receipt := cryptoTestReceipt(intent, txId, amountAtomic, confirmations, server.NowUtc(), model.CryptoReceiptStateConfirmed)
	if confirmations == 0 {
		receipt.State = model.CryptoReceiptStateSeen
	}
	inserted, stored := model.UpsertCryptoPaymentReceiptInConn(nil, ctx, receipt)
	connect.AssertEqual(t, inserted, true)
	return stored
}

// Two credits of one invoice at once -- the scanner and the reconciler --
// credit it once: one renewal, one Pro balance, the receipts credited once.
func TestCryptoConcurrentCreditsCreditOnce(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "cryptoconcurrent")
		intent := cryptoDbCreateIntent(t, ctx, model.CryptoAssetXmr, &networkId, model.SolanaPlanYearly, 40, 101)
		receipt := cryptoDbReceipt(t, ctx, intent, "tx-concurrent", intent.AmountAtomic, 1)

		credit := func() *cryptoCredit {
			return &cryptoCredit{
				usdNanoCents: intent.PriceUsdNanoCents,
				receipts:     []model.CryptoPaymentReceiptKey{receipt.Key()},
				receiptUsd:   map[model.CryptoPaymentReceiptKey]model.NanoCents{receipt.Key(): intent.PriceUsdNanoCents},
			}
		}
		start := make(chan struct{})
		var wait sync.WaitGroup
		results := make([]bool, 2)
		errs := make([]error, 2)
		for i := range 2 {
			wait.Add(1)
			go func() {
				defer wait.Done()
				<-start
				results[i], errs[i] = cryptoCreditIntent(clientSession, intent, credit())
			}()
		}
		close(start)
		wait.Wait()
		connect.AssertEqual(t, errs[0], nil)
		connect.AssertEqual(t, errs[1], nil)
		connect.AssertEqual(t, results[0] != results[1], true)

		renewal := cryptoDbRenewals(t, ctx, networkId, model.SubscriptionMarketMonero)
		connect.AssertEqual(t, renewal.count, 1)
		proBalances := 0
		for _, balance := range model.GetActiveTransferBalances(ctx, networkId) {
			if balance.Pro {
				proBalances += 1
			}
		}
		connect.AssertEqual(t, proBalances, 1)
		credited := model.GetCryptoPaymentIntent(ctx, intent.IntentId)
		connect.AssertEqual(t, credited.CreditedAt != nil, true)
		receipts := model.GetCryptoPaymentReceiptsForIntent(ctx, intent.IntentId)
		connect.AssertEqual(t, receipts[0].State, model.CryptoReceiptStateCredited)

		// a third credit after the fact adds nothing
		again, err := cryptoCreditIntent(clientSession, intent, credit())
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, again, false)
		connect.AssertEqual(t, cryptoDbRenewals(t, ctx, networkId, model.SubscriptionMarketMonero).count, 1)
	})
}

// A plan credit writes the renewal (market, transaction id, window, revenue,
// price tier), the Pro balance with the USD value as subsidy revenue, and
// the welcome offer's redemption, all in the credit's transaction.
func TestCryptoCreditRenewalBalanceRevenueAndOffer(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "cryptorenewal")
		_, _, err := model.IssueOnboardingOffer(ctx, &model.IssueOnboardingOfferArgs{
			NetworkId:  networkId,
			IssuedBy:   model.OnboardingOfferIssuedByInApp,
			Surface:    model.OnboardingOfferSurfaceIntroStep,
			Tier:       "regional",
			PercentOff: 25,
			Validity:   30 * 24 * time.Hour,
		})
		connect.AssertEqual(t, err, nil)

		intent := cryptoDbCreateIntent(t, ctx, model.CryptoAssetZec, &networkId, model.SolanaPlanYearlyOnboarding, 3, 4_609_858_083)
		cryptoDbExec(ctx, `UPDATE crypto_payment_intent SET tier = 'regional', offer_applied = true WHERE intent_id = $1`, intent.IntentId)
		intent = model.GetCryptoPaymentIntent(ctx, intent.IntentId)
		receipt := cryptoDbReceipt(t, ctx, intent, "tx-renewal", intent.AmountAtomic, 10)

		before := server.NowUtc()
		credited, err := cryptoCreditIntent(clientSession, intent, &cryptoCredit{
			usdNanoCents: intent.PriceUsdNanoCents,
			receipts:     []model.CryptoPaymentReceiptKey{receipt.Key()},
		})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, credited, true)

		renewal := cryptoDbRenewals(t, ctx, networkId, model.SubscriptionMarketZcash)
		connect.AssertEqual(t, renewal.count, 1)
		connect.AssertEqual(t, renewal.transactionId, intent.IntentId.String())
		connect.AssertEqual(t, renewal.netRevenue, model.UsdToNanoCents(3))
		connect.AssertEqual(t, *renewal.priceTier, "regional")
		// a year, the welcome offer's trial, and the manual-payment grace
		connect.AssertEqual(t, renewal.endTime.Sub(renewal.startTime), solanaPlanDuration(model.SolanaPlanYearlyOnboarding)+manualPaymentGracePeriod)
		connect.AssertEqual(t, !renewal.startTime.Before(before.Add(-time.Second)), true)

		var pro *model.TransferBalance
		for _, balance := range model.GetActiveTransferBalances(ctx, networkId) {
			if balance.Pro {
				pro = balance
			}
		}
		connect.AssertEqual(t, pro != nil, true)
		connect.AssertEqual(t, pro.StartBalanceByteCount, RefreshSupporterTransferBalance)
		connect.AssertEqual(t, pro.EndTime.Equal(renewal.endTime), true)
		var subsidyRevenue model.NanoCents
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT subsidy_net_revenue_nano_cents FROM transfer_balance WHERE balance_id = $1`, pro.BalanceId).Scan(&subsidyRevenue))
		})
		connect.AssertEqual(t, subsidyRevenue, model.UsdToNanoCents(3))
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)

		after := model.GetCryptoPaymentIntent(ctx, intent.IntentId)
		connect.AssertEqual(t, *after.BalanceId, pro.BalanceId)
		connect.AssertEqual(t, *after.CreditedUsdNanoCents, model.UsdToNanoCents(3))

		offer := model.GetOnboardingOffer(ctx, networkId)
		connect.AssertEqual(t, offer.RedeemedAt != nil, true)
		connect.AssertEqual(t, *offer.Store, model.OnboardingStoreZcash)

		// the balance result's crypto term: the year ends, then 30 days of grace
		term := cryptoTermForNetwork(ctx, networkId, server.NowUtc())
		connect.AssertEqual(t, term.Market, model.SubscriptionMarketZcash)
		connect.AssertEqual(t, term.GraceEndTime.Equal(renewal.endTime), true)
		connect.AssertEqual(t, term.GraceEndTime.Sub(term.TermEndTime), manualPaymentGracePeriod)

		// a crypto year blocks no new card purchase
		connect.AssertEqual(t, subscriptionMarketsBlockNewPurchase(model.GetActiveSubscriptionRenewalMarkets(ctx, networkId, model.SubscriptionTypeSupporter)), false)
	})
}

// Receipts are idempotent on (asset, tx_id, output_key): a scan that sees one
// again updates what it observes and never adds a second; decisions
// (credited, review, ignored) are kept; a receipt dropped before its credit
// comes back when the chain shows it again.
func TestCryptoReceiptUpsertIsIdempotent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		intent := cryptoDbCreateIntent(t, ctx, model.CryptoAssetZec, nil, StripeItemData1Tib, 3, 4_609_858_100)
		receipt := cryptoTestReceipt(intent, "tx-idempotent", intent.AmountAtomic, 0, server.NowUtc(), model.CryptoReceiptStateSeen)
		receipt.Pool = "orchard"
		inserted, stored := model.UpsertCryptoPaymentReceiptInConn(nil, ctx, receipt)
		connect.AssertEqual(t, inserted, true)
		connect.AssertEqual(t, stored.State, model.CryptoReceiptStateSeen)
		firstSeen := stored.FirstSeenAt

		// mined: confirmed, the pool relabelled, the first sight kept
		height := int64(4_499_990)
		receipt.BlockHeight = &height
		receipt.Confirmations = 3
		receipt.Pool = "ironwood"
		receipt.FirstSeenAt = server.NowUtc().Add(time.Hour)
		rate := 1304.60
		receipt.FirstSeenRateUsd = &rate
		inserted, stored = model.UpsertCryptoPaymentReceiptInConn(nil, ctx, receipt)
		connect.AssertEqual(t, inserted, false)
		connect.AssertEqual(t, stored.State, model.CryptoReceiptStateConfirmed)
		connect.AssertEqual(t, stored.Confirmations, 3)
		connect.AssertEqual(t, stored.Pool, "ironwood")
		connect.AssertEqual(t, stored.FirstSeenAt.Equal(firstSeen), true)
		connect.AssertEqual(t, *stored.FirstSeenRateUsd, rate)
		connect.AssertEqual(t, len(model.GetCryptoPaymentReceiptsForIntent(ctx, intent.IntentId)), 1)

		// a decision stays put
		intentId := intent.IntentId
		model.SetCryptoPaymentReceiptsStateInConn(nil, ctx, &intentId, []model.CryptoPaymentReceiptKey{stored.Key()}, model.CryptoReceiptStateCredited, "")
		receipt.Confirmations = 30
		_, stored = model.UpsertCryptoPaymentReceiptInConn(nil, ctx, receipt)
		connect.AssertEqual(t, stored.State, model.CryptoReceiptStateCredited)
		connect.AssertEqual(t, stored.Confirmations, 30)

		// dropped before its credit, then mined again: it counts again
		other := cryptoTestReceipt(intent, "tx-dropped", 5, 0, server.NowUtc(), model.CryptoReceiptStateSeen)
		model.UpsertCryptoPaymentReceiptInConn(nil, ctx, other)
		model.SetCryptoPaymentReceiptsStateInConn(nil, ctx, &intentId, []model.CryptoPaymentReceiptKey{other.Key()}, model.CryptoReceiptStateOrphaned, "")
		other.BlockHeight = &height
		other.Confirmations = 1
		_, stored = model.UpsertCryptoPaymentReceiptInConn(nil, ctx, other)
		connect.AssertEqual(t, stored.State, model.CryptoReceiptStateConfirmed)

		// a mempool sighting flagged as a double spend is seen and not counted;
		// mined (never flagged then) it is confirmed (mmm/ZCASH-CONTRACT.md §3.13.3). Its
		// amount follows the chain until it is credited, and its first
		// sighting's height stays
		firstHeight := int64(4_499_900)
		flagged := cryptoTestReceipt(intent, "tx-flagged", 6, 0, server.NowUtc(), model.CryptoReceiptStateSeen)
		flagged.ReviewReason = model.CryptoReviewReasonDoubleSpend
		flagged.FirstSeenHeight = &firstHeight
		inserted, stored = model.UpsertCryptoPaymentReceiptInConn(nil, ctx, flagged)
		connect.AssertEqual(t, inserted, true)
		connect.AssertEqual(t, stored.State, model.CryptoReceiptStateSeen)
		connect.AssertEqual(t, stored.ReviewReason, model.CryptoReviewReasonDoubleSpend)
		connect.AssertEqual(t, cryptoReceiptCounts(stored), false)
		// still in the mempool, no longer flagged: it counts
		flagged.ReviewReason = ""
		laterHeight := int64(4_499_950)
		flagged.FirstSeenHeight = &laterHeight
		_, stored = model.UpsertCryptoPaymentReceiptInConn(nil, ctx, flagged)
		connect.AssertEqual(t, stored.State, model.CryptoReceiptStateSeen)
		connect.AssertEqual(t, stored.ReviewReason, "")
		connect.AssertEqual(t, *stored.FirstSeenHeight, firstHeight)
		flagged.BlockHeight = &height
		flagged.Confirmations = 2
		flagged.AmountAtomic = 7
		_, stored = model.UpsertCryptoPaymentReceiptInConn(nil, ctx, flagged)
		connect.AssertEqual(t, stored.State, model.CryptoReceiptStateConfirmed)
		connect.AssertEqual(t, stored.AmountAtomic, int64(7))
		// once credited, the amount is the credit's
		model.SetCryptoPaymentReceiptsStateInConn(nil, ctx, &intentId, []model.CryptoPaymentReceiptKey{flagged.Key()}, model.CryptoReceiptStateCredited, "")
		flagged.AmountAtomic = 8
		_, stored = model.UpsertCryptoPaymentReceiptInConn(nil, ctx, flagged)
		connect.AssertEqual(t, stored.AmountAtomic, int64(7))

		// a receipt stored ignored as a double spend before this rule becomes
		// confirmed once mined
		legacy := cryptoTestReceipt(intent, "tx-legacy-flagged", 9, 0, server.NowUtc(), model.CryptoReceiptStateIgnored)
		legacy.ReviewReason = model.CryptoReviewReasonDoubleSpend
		model.UpsertCryptoPaymentReceiptInConn(nil, ctx, legacy)
		legacy.ReviewReason = ""
		legacy.State = model.CryptoReceiptStateSeen
		_, stored = model.UpsertCryptoPaymentReceiptInConn(nil, ctx, legacy)
		connect.AssertEqual(t, stored.State, model.CryptoReceiptStateIgnored)
		legacy.BlockHeight = &height
		legacy.Confirmations = 1
		_, stored = model.UpsertCryptoPaymentReceiptInConn(nil, ctx, legacy)
		connect.AssertEqual(t, stored.State, model.CryptoReceiptStateConfirmed)
		connect.AssertEqual(t, stored.ReviewReason, "")
		// other ignored receipts stay ignored
		locked := cryptoTestReceipt(intent, "tx-locked", 9, 0, server.NowUtc(), model.CryptoReceiptStateIgnored)
		locked.ReviewReason = model.CryptoReviewReasonLocked
		model.UpsertCryptoPaymentReceiptInConn(nil, ctx, locked)
		locked.BlockHeight = &height
		_, stored = model.UpsertCryptoPaymentReceiptInConn(nil, ctx, locked)
		connect.AssertEqual(t, stored.State, model.CryptoReceiptStateIgnored)
		connect.AssertEqual(t, stored.ReviewReason, model.CryptoReviewReasonLocked)
	})
}

// Concurrent invoices each take a different pool address (FOR UPDATE SKIP
// LOCKED), and an empty pool refuses without writing an invoice.
func TestCryptoAddressPoolAllocationUnderConcurrency(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		addresses := []*model.CryptoPaymentAddress{}
		for index := int64(1); index <= 20; index += 1 {
			addresses = append(addresses, &model.CryptoPaymentAddress{Asset: model.CryptoAssetXmr, AddressIndex: index, Address: fakeCryptoAddress(model.CryptoAssetXmr, uint64(index))})
		}
		connect.AssertEqual(t, model.AddCryptoPaymentAddresses(ctx, addresses), int64(20))
		// adding them again adds nothing
		connect.AssertEqual(t, model.AddCryptoPaymentAddresses(ctx, addresses), int64(0))
		connect.AssertEqual(t, model.CountFreeCryptoPaymentAddresses(ctx, model.CryptoAssetXmr), 20)

		start := make(chan struct{})
		var wait sync.WaitGroup
		var stateLock sync.Mutex
		allocated := map[string]server.Id{}
		empty := 0
		for range 30 {
			wait.Add(1)
			go func() {
				defer wait.Done()
				<-start
				_, tokenHash := cryptoNewStatusToken()
				now := server.NowUtc()
				intent := &model.CryptoPaymentIntent{
					IntentId:                 server.NewId(),
					StatusTokenHash:          tokenHash,
					Asset:                    model.CryptoAssetXmr,
					Delivery:                 model.CryptoDeliveryCode,
					Item:                     StripeItemData1Tib,
					PriceUsdNanoCents:        model.UsdToNanoCents(3),
					RegularPriceUsdNanoCents: model.UsdToNanoCents(3),
					AmountAtomic:             5_402_000_000,
					RateUsd:                  555.36,
					QuotedAt:                 now,
					ExpiresAt:                now.Add(30 * time.Minute),
				}
				err := model.CreateCryptoPaymentIntentWithAddress(ctx, intent, true)
				stateLock.Lock()
				defer stateLock.Unlock()
				if err == model.ErrCryptoAddressPoolEmpty {
					empty += 1
					return
				}
				if err != nil {
					t.Errorf("create: %s", err)
					return
				}
				if previous, ok := allocated[intent.Address]; ok {
					t.Errorf("address issued twice, to %s and %s", previous, intent.IntentId)
				}
				allocated[intent.Address] = intent.IntentId
			}()
		}
		close(start)
		wait.Wait()
		connect.AssertEqual(t, len(allocated), 20)
		connect.AssertEqual(t, empty, 10)
		connect.AssertEqual(t, model.CountFreeCryptoPaymentAddresses(ctx, model.CryptoAssetXmr), 0)
		// each pool row names its invoice
		for address, intentId := range allocated {
			intents := model.GetCryptoPaymentIntentsByAddressesInConn(nil, ctx, model.CryptoAssetXmr, []string{address})
			connect.AssertEqual(t, intents[address].IntentId, intentId)
		}
	})
}

// The whole rail for XMR against the fake daemon: the pool is filled, an
// invoice is quoted from the pool at the rates, reused for a second request,
// seen in the mempool, credited at one confirmation, and a second payment is
// held for review while the buyer still sees paid.
func TestCryptoScannerEndToEnd(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cryptoTestResetLimiters()
		cryptoTestStandardRates(t)
		wallet := newFakeCryptoWallet(model.CryptoAssetXmr)
		cryptoTestEnable(t, cryptoTestConfig(), map[string]*fakeCryptoWallet{model.CryptoAssetXmr: wallet})
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "cryptoendtoend")
		taskSession := session.Testing_CreateClientSession(ctx, nil)

		// before any scan the coin cannot be quoted: its payments would not be seen
		result, err := CreateCryptoPaymentIntent(&CreateCryptoPaymentIntentArgs{Asset: model.CryptoAssetXmr, Plan: model.SolanaPlanYearly}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error.Code, PurchaseErrorCodeAssetUnavailable)

		refill, err := RunCryptoAddressRefill(taskSession, model.CryptoAssetXmr)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, refill.Added, 5)
		scan, err := RunCryptoPaymentScan(taskSession, model.CryptoAssetXmr)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, scan.Error, "")
		resetCryptoAvailability()

		result, err = CreateCryptoPaymentIntent(&CreateCryptoPaymentIntentArgs{Asset: model.CryptoAssetXmr, Plan: model.SolanaPlanYearly}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, (*CryptoPaymentError)(nil))
		connect.AssertEqual(t, result.Amount, "0.072386")
		connect.AssertEqual(t, result.AmountAtomic, "72386000000")
		connect.AssertEqual(t, result.PriceUsd, float64(40))
		connect.AssertEqual(t, result.RateUsd, 555.36)
		connect.AssertEqual(t, result.Delivery, model.CryptoDeliveryAccount)
		connect.AssertEqual(t, result.Uri, "monero:"+result.Address+"?tx_amount=0.072386&tx_description=URnetwork%20Pro%20yearly")
		connect.AssertEqual(t, result.Status.Status, CryptoPaymentStatusPending)
		connect.AssertEqual(t, model.CountFreeCryptoPaymentAddresses(ctx, model.CryptoAssetXmr), 4)
		intent := model.GetCryptoPaymentIntent(ctx, server.RequireParseId(result.IntentId))
		connect.AssertEqual(t, len(model.GetCryptoPaymentReceiptsForIntent(ctx, intent.IntentId)), 0)

		// one open invoice per network, coin and item: the same address and
		// quote, with a token of its own
		again, err := CreateCryptoPaymentIntent(&CreateCryptoPaymentIntentArgs{Asset: model.CryptoAssetXmr, Plan: model.SolanaPlanYearly}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, again.IntentId, result.IntentId)
		connect.AssertEqual(t, again.Address, result.Address)
		connect.AssertEqual(t, again.StatusToken != result.StatusToken, true)
		connect.AssertEqual(t, model.CountFreeCryptoPaymentAddresses(ctx, model.CryptoAssetXmr), 4)
		for _, token := range []string{result.StatusToken, again.StatusToken} {
			status, err := CryptoPaymentStatus(&CryptoPaymentStatusArgs{IntentId: result.IntentId, StatusToken: token}, clientSession)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, status.Status, CryptoPaymentStatusPending)
		}
		// a wrong token is unknown, and says nothing more
		wrongToken, _ := cryptoNewStatusToken()
		status, err := CryptoPaymentStatus(&CryptoPaymentStatusArgs{IntentId: result.IntentId, StatusToken: wrongToken}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, status.Status, CryptoPaymentStatusUnknown)
		connect.AssertEqual(t, status.Asset, "")

		// re-quote while nothing arrived: the same address, a new rate, the old
		// quote kept
		cryptoTestRates(t, map[string]map[string]float64{
			cryptoRateSourceKraken:    {model.CryptoAssetXmr: 600},
			cryptoRateSourceCoingecko: {model.CryptoAssetXmr: 600},
		})
		requoted, err := CryptoPaymentRequote(&CryptoPaymentStatusArgs{IntentId: result.IntentId, StatusToken: result.StatusToken}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, requoted.Error, (*CryptoPaymentError)(nil))
		connect.AssertEqual(t, requoted.Address, result.Address)
		connect.AssertEqual(t, requoted.RateUsd, float64(600))
		connect.AssertEqual(t, requoted.StatusToken, result.StatusToken)
		var quotes int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM crypto_payment_quote WHERE intent_id = $1`, intent.IntentId).Scan(&quotes))
		})
		connect.AssertEqual(t, quotes, 2)
		intent = model.GetCryptoPaymentIntent(ctx, intent.IntentId)

		// paid: seen in the mempool, never credited at 0 confirmations
		wallet.pay("tx-endtoend", uint64(*intent.AddressIndex), uint64(intent.AmountAtomic))
		scan, err = RunCryptoPaymentScan(taskSession, model.CryptoAssetXmr)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, scan.NewReceipts, 1)
		connect.AssertEqual(t, scan.Credited, 0)
		status, err = CryptoPaymentStatus(&CryptoPaymentStatusArgs{IntentId: result.IntentId, StatusToken: result.StatusToken}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, status.Status, CryptoPaymentStatusConfirming)
		connect.AssertEqual(t, status.Confirmations, 0)
		connect.AssertEqual(t, status.Received, requoted.Amount)
		// nothing can be re-quoted under a payment
		refused, err := CryptoPaymentRequote(&CryptoPaymentStatusArgs{IntentId: result.IntentId, StatusToken: result.StatusToken}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, refused.Error.Code, PurchaseErrorCodeInvalidRequest)

		// one confirmation: credited
		wallet.mine("tx-endtoend", 1001)
		wallet.setHeight(1001)
		scan, err = RunCryptoPaymentScan(taskSession, model.CryptoAssetXmr)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, scan.Credited, 1)
		status, err = CryptoPaymentStatus(&CryptoPaymentStatusArgs{IntentId: result.IntentId, StatusToken: result.StatusToken}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, status.Status, CryptoPaymentStatusPaid)
		connect.AssertEqual(t, status.CreditedAt != nil, true)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)
		renewal := cryptoDbRenewals(t, ctx, networkId, model.SubscriptionMarketMonero)
		connect.AssertEqual(t, renewal.count, 1)
		connect.AssertEqual(t, renewal.netRevenue, model.UsdToNanoCents(40))
		connect.AssertEqual(t, *renewal.priceTier, intent.Tier)

		// a second payment to the paid invoice: held for review, never a
		// second year, and the buyer still sees paid
		wallet.pay("tx-extra", uint64(*intent.AddressIndex), uint64(intent.AmountAtomic))
		scan, err = RunCryptoPaymentScan(taskSession, model.CryptoAssetXmr)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, scan.Reviewed, 1)
		connect.AssertEqual(t, cryptoDbRenewals(t, ctx, networkId, model.SubscriptionMarketMonero).count, 1)
		status, _ = CryptoPaymentStatus(&CryptoPaymentStatusArgs{IntentId: result.IntentId, StatusToken: result.StatusToken}, clientSession)
		connect.AssertEqual(t, status.Status, CryptoPaymentStatusPaid)
		receipts := model.GetCryptoPaymentReceiptsForIntent(ctx, intent.IntentId)
		reasons := map[string]string{}
		for _, receipt := range receipts {
			reasons[receipt.TxId] = receipt.State + "/" + receipt.ReviewReason
		}
		connect.AssertEqual(t, reasons["tx-endtoend"], model.CryptoReceiptStateCredited+"/")
		connect.AssertEqual(t, reasons["tx-extra"], model.CryptoReceiptStateReview+"/"+model.CryptoReviewReasonExtra)

		// a payment to an address no invoice was given, and a time-locked one
		wallet.pay("tx-unknown", 99, 1_000_000)
		locked := wallet.pay("tx-locked", uint64(*intent.AddressIndex), 1_000_000)
		locked.UnlockTime = 5000
		scan, err = RunCryptoPaymentScan(taskSession, model.CryptoAssetXmr)
		connect.AssertEqual(t, err, nil)
		unmatched := CryptoOperatorUnmatchedReceipts(ctx, model.CryptoAssetXmr, 10)
		connect.AssertEqual(t, len(unmatched), 1)
		connect.AssertEqual(t, unmatched[0].ReviewReason, model.CryptoReviewReasonUnknownAddress)
		for _, receipt := range model.GetCryptoPaymentReceiptsForIntent(ctx, intent.IntentId) {
			if receipt.TxId == "tx-locked" {
				connect.AssertEqual(t, receipt.State, model.CryptoReceiptStateIgnored)
				connect.AssertEqual(t, receipt.ReviewReason, model.CryptoReviewReasonLocked)
			}
		}

		// final depth stamps the receipt final
		wallet.setHeight(1010)
		_, err = RunCryptoPaymentScan(taskSession, model.CryptoAssetXmr)
		connect.AssertEqual(t, err, nil)
		for _, receipt := range model.GetCryptoPaymentReceiptsForIntent(ctx, intent.IntentId) {
			if receipt.TxId == "tx-endtoend" {
				connect.AssertEqual(t, receipt.FinalAt != nil, true)
			}
		}

		// a lagging daemon stops sales: the cursor keeps its last success, and
		// the quote path refuses after three intervals
		cursor := model.GetCryptoScanCursor(ctx, model.CryptoAssetXmr)
		connect.AssertEqual(t, *cursor.WalletHeight, int64(1010))
		wallet.err = fmt.Errorf("synthetic daemon outage")
		scan, err = RunCryptoPaymentScan(taskSession, model.CryptoAssetXmr)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, scan.Error != "", true)
		cursor = model.GetCryptoScanCursor(ctx, model.CryptoAssetXmr)
		connect.AssertEqual(t, cursor.LastError != "", true)
	})
}

// A late payment is credited at its first-seen value, capped at the price; a
// short one at expiry is held as underpaid; a top-up after expiry that makes
// it whole clears the review.
func TestCryptoScannerLateAndUnderpaid(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cryptoTestResetLimiters()
		cryptoTestStandardRates(t)
		wallet := newFakeCryptoWallet(model.CryptoAssetXmr)
		cryptoTestEnable(t, cryptoTestConfig(), map[string]*fakeCryptoWallet{model.CryptoAssetXmr: wallet})
		networkId, _, _ := cryptoDbTestNetwork(t, ctx, "cryptolate")
		taskSession := session.Testing_CreateClientSession(ctx, nil)

		late := cryptoDbCreateIntent(t, ctx, model.CryptoAssetXmr, &networkId, StripeItemData1Tib, 3, 201)
		short := cryptoDbCreateIntent(t, ctx, model.CryptoAssetXmr, &networkId, StripeItemData10Tib, 20, 202)
		cryptoDbExec(ctx, `UPDATE crypto_payment_intent SET expires_at = $2 WHERE intent_id = $1`, late.IntentId, server.NowUtc().Add(-time.Hour))

		// the late one pays in full after its quote ended; the short one pays
		// half in time
		wallet.pay("tx-late", 201, uint64(late.AmountAtomic))
		wallet.mine("tx-late", 1000)
		wallet.pay("tx-short", 202, uint64(short.AmountAtomic/2))
		wallet.mine("tx-short", 1000)
		scan, err := RunCryptoPaymentScan(taskSession, model.CryptoAssetXmr)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, scan.Credited, 1)

		credited := model.GetCryptoPaymentIntent(ctx, late.IntentId)
		connect.AssertEqual(t, credited.CreditedAt != nil, true)
		// the first-seen value of the quoted coins is just above the $3 price:
		// capped at the price
		connect.AssertEqual(t, *credited.CreditedUsdNanoCents, model.UsdToNanoCents(3))
		receipts := model.GetCryptoPaymentReceiptsForIntent(ctx, late.IntentId)
		connect.AssertEqual(t, *receipts[0].FirstSeenRateUsd, 555.36)
		// the data lands on the network, data only
		dataBalances := 0
		for _, balance := range model.GetActiveTransferBalances(ctx, networkId) {
			if !balance.Pro && balance.StartBalanceByteCount == model.Tib {
				dataBalances += 1
			}
		}
		connect.AssertEqual(t, dataBalances, 1)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), false)

		// the short one at expiry: its half was seen ten minutes ago, and its
		// quote ended a minute ago. Held as underpaid
		cryptoDbExec(ctx, `UPDATE crypto_payment_receipt SET first_seen_at = $2 WHERE intent_id = $1`, short.IntentId, server.NowUtc().Add(-10*time.Minute))
		cryptoDbExec(ctx, `UPDATE crypto_payment_intent SET expires_at = $2 WHERE intent_id = $1`, short.IntentId, server.NowUtc().Add(-time.Minute))
		_, err = RunCryptoPaymentScan(taskSession, model.CryptoAssetXmr)
		connect.AssertEqual(t, err, nil)
		shortIntent := model.GetCryptoPaymentIntent(ctx, short.IntentId)
		view := cryptoEvaluateIntent(shortIntent, model.GetCryptoPaymentReceiptsForIntent(ctx, short.IntentId), cryptoIntentPolicyFor(cryptoPayConfigFunc(), model.CryptoAssetXmr), server.NowUtc())
		connect.AssertEqual(t, view.Status, CryptoPaymentStatusReview)
		connect.AssertEqual(t, view.ReviewReason, model.CryptoReviewReasonUnderpaid)
		for _, receipt := range model.GetCryptoPaymentReceiptsForIntent(ctx, short.IntentId) {
			connect.AssertEqual(t, receipt.State, model.CryptoReceiptStateReview)
			connect.AssertEqual(t, receipt.ReviewReason, model.CryptoReviewReasonUnderpaid)
		}

		// a top-up after expiry makes it whole at its first-seen rate: the
		// review clears and it is credited, capped at the price
		wallet.pay("tx-topup", 202, uint64(short.AmountAtomic/2))
		wallet.mine("tx-topup", 1001)
		wallet.setHeight(1001)
		scan, err = RunCryptoPaymentScan(taskSession, model.CryptoAssetXmr)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, scan.Credited, 1)
		shortIntent = model.GetCryptoPaymentIntent(ctx, short.IntentId)
		connect.AssertEqual(t, shortIntent.CreditedAt != nil, true)
		connect.AssertEqual(t, *shortIntent.CreditedUsdNanoCents <= model.UsdToNanoCents(20), true)
		connect.AssertEqual(t, *shortIntent.CreditedUsdNanoCents >= cryptoAcceptValueNanoCents(model.UsdToNanoCents(20), 99.5), true)
	})
}

// A data code bought with Zcash and no network: the code is created in the
// credit, shown by the status once paid, and emailed through the outbox.
func TestCryptoCodeDelivery(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cryptoTestResetLimiters()
		cryptoTestStandardRates(t)
		wallet := newFakeCryptoWallet(model.CryptoAssetZec)
		cryptoTestEnable(t, cryptoTestConfig(), map[string]*fakeCryptoWallet{model.CryptoAssetZec: wallet})
		pageSession := session.Testing_CreateClientSession(ctx, nil)
		taskSession := session.Testing_CreateClientSession(ctx, nil)

		sentTo := []string{}
		var sentTemplate Template
		previousSender := GetAWSMessageSender()
		SetMessageSender(&mockAWSMessageSender{SendMessageFunc: func(userAuth string, template Template, sendOpts ...any) error {
			sentTo = append(sentTo, userAuth)
			sentTemplate = template
			return nil
		}})
		defer SetMessageSender(previousSender)

		_, err := RunCryptoAddressRefill(taskSession, model.CryptoAssetZec)
		connect.AssertEqual(t, err, nil)
		_, err = RunCryptoPaymentScan(taskSession, model.CryptoAssetZec)
		connect.AssertEqual(t, err, nil)
		resetCryptoAvailability()

		result, err := CreateDataCryptoPaymentIntent(&CreateDataCryptoPaymentIntentArgs{
			Asset:  model.CryptoAssetZec,
			ItemId: StripeItemData10Tib,
			Email:  "buyer@synthetic.example",
		}, pageSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, (*CryptoPaymentError)(nil))
		connect.AssertEqual(t, result.Delivery, model.CryptoDeliveryCode)
		connect.AssertEqual(t, result.Amount, "0.015408")
		connect.AssertEqual(t, result.Uri, "zcash:"+result.Address+"?amount=0.015408&message=URnetwork%2010%20TiB%20data")
		// a code is a new invoice every time: no reuse
		second, err := CreateDataCryptoPaymentIntent(&CreateDataCryptoPaymentIntentArgs{Asset: model.CryptoAssetZec, ItemId: StripeItemData10Tib}, pageSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, second.IntentId != result.IntentId, true)

		intent := model.GetCryptoPaymentIntent(ctx, server.RequireParseId(result.IntentId))
		wallet.pay("tx-code", uint64(*intent.AddressIndex), uint64(intent.AmountAtomic))
		wallet.mine("tx-code", 991)
		_, err = RunCryptoPaymentScan(taskSession, model.CryptoAssetZec)
		connect.AssertEqual(t, err, nil)
		status, err := CryptoPaymentStatus(&CryptoPaymentStatusArgs{IntentId: result.IntentId, StatusToken: result.StatusToken}, pageSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, status.Status, CryptoPaymentStatusPaid)
		connect.AssertEqual(t, status.BalanceCode != "", true)

		balanceCode, err := model.GetBalanceCode(ctx, *model.GetCryptoPaymentIntent(ctx, intent.IntentId).BalanceCodeId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, balanceCode.Secret, status.BalanceCode)
		connect.AssertEqual(t, balanceCode.BalanceByteCount, 10*model.Tib)
		connect.AssertEqual(t, balanceCode.NetRevenue, model.UsdToNanoCents(20))
		connect.AssertEqual(t, balanceCode.PurchaseEventId, model.CryptoPaymentPurchaseEventId(intent.IntentId))

		deliverAccountMessagesAt(ctx, GetAWSMessageSender(), server.NowUtc())
		connect.AssertEqual(t, sentTo, []string{"buyer@synthetic.example"})
		codeTemplate, ok := sentTemplate.(*SubscriptionTransferBalanceCodeTemplate)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, codeTemplate.Secret, balanceCode.Secret)
	})
}

// USDC on the generic API: the invoice links a Solana Pay intent, the Helius
// webhook's credit of it goes through the generic credit (both one-shots,
// the renewal under market solana keyed by the reference, the tier inside
// the credit), and a redelivery credits nothing more. A code bought with
// USDC has no network and is delivered by the status.
func TestCryptoUsdcThroughHelius(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cryptoTestResetLimiters()
		cryptoTestEnable(t, cryptoTestConfig(), map[string]*fakeCryptoWallet{})
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "cryptousdc")
		webhookSession := session.Testing_CreateClientSession(ctx, nil)
		solanaTestAmountSuffixes(t, 301, 302)

		result, err := CreateCryptoPaymentIntent(&CreateCryptoPaymentIntentArgs{Asset: model.CryptoAssetUsdcSolana, Plan: model.SolanaPlanYearly}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, (*CryptoPaymentError)(nil))
		connect.AssertEqual(t, result.Amount, "40.000301")
		connect.AssertEqual(t, result.Address, solanaPaymentRecipient())
		reference := result.Memo
		solanaIntent := model.GetSolanaPaymentIntent(ctx, reference)
		connect.AssertEqual(t, solanaIntent.NetworkId, networkId)
		connect.AssertEqual(t, solanaIntent.SubscriptionPlan, model.SolanaPlanYearly)
		connect.AssertEqual(t, solanaIntent.ExpectedAmountUsd, 40.000301)

		webhook, err := HeliusWebhook([]*SolanaTransaction{solanaTestPayment(reference, "sig-usdc-plan", 40.000301)}, webhookSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, webhook.Message, "Processed 1 matching payments")
		// both one-shots
		connect.AssertEqual(t, *model.GetSolanaPaymentIntent(ctx, reference).TxSignature, "sig-usdc-plan")
		intent := model.GetCryptoPaymentIntent(ctx, server.RequireParseId(result.IntentId))
		connect.AssertEqual(t, intent.CreditedAt != nil, true)
		renewal := cryptoDbRenewals(t, ctx, networkId, model.SubscriptionMarketSolana)
		connect.AssertEqual(t, renewal.count, 1)
		connect.AssertEqual(t, renewal.transactionId, reference)
		connect.AssertEqual(t, *renewal.priceTier, intent.Tier)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)
		status, err := CryptoPaymentStatus(&CryptoPaymentStatusArgs{IntentId: result.IntentId, StatusToken: result.StatusToken}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, status.Status, CryptoPaymentStatusPaid)

		// Helius redelivers: nothing more
		webhook, err = HeliusWebhook([]*SolanaTransaction{solanaTestPayment(reference, "sig-usdc-plan", 40.000301)}, webhookSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, cryptoDbRenewals(t, ctx, networkId, model.SubscriptionMarketSolana).count, 1)

		// a USDC code: no network until it is redeemed
		code, err := CreateDataCryptoPaymentIntent(&CreateDataCryptoPaymentIntentArgs{Asset: model.CryptoAssetUsdcSolana, ItemId: StripeItemData1Tib}, webhookSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, code.Error, (*CryptoPaymentError)(nil))
		connect.AssertEqual(t, code.Delivery, model.CryptoDeliveryCode)
		codeSolanaIntent := model.GetSolanaPaymentIntent(ctx, code.Memo)
		connect.AssertEqual(t, codeSolanaIntent.NetworkId, server.Id{})
		webhook, err = HeliusWebhook([]*SolanaTransaction{solanaTestMemoPayment(code.Memo, "sig-usdc-code", 3.000302)}, webhookSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, webhook.Message, "Processed 1 matching payments")
		codeStatus, err := CryptoPaymentStatus(&CryptoPaymentStatusArgs{IntentId: code.IntentId, StatusToken: code.StatusToken}, webhookSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, codeStatus.Status, CryptoPaymentStatusPaid)
		connect.AssertEqual(t, codeStatus.BalanceCode != "", true)

		// the old status endpoint does not answer for a reference of the
		// generic API: the reference is on the chain for anyone to read, and
		// must not name the network it paid for (mmm/ZCASH-CONTRACT.md §3.13.7)
		legacy, err := PayDataSolanaStatus(&PayDataSolanaStatusArgs{Reference: code.Memo}, webhookSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, legacy.Status, PayDataSolanaStatusUnknown)
		connect.AssertEqual(t, legacy.NetworkName, "")
	})
}

// After unlink_after, a paid invoice forgets its network: the invoice's
// network, email and grant handles are cleared, the renewal's transaction id
// is random, a code stops naming the invoice, and a USDC invoice's Solana
// intent loses its network. Amounts and dates stay.
func TestCryptoRetentionUnlink(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cryptoTestEnable(t, cryptoTestConfig(), map[string]*fakeCryptoWallet{})
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "cryptounlink")

		plan := cryptoDbCreateIntent(t, ctx, model.CryptoAssetXmr, &networkId, model.SolanaPlanYearly, 40, 401)
		planReceipt := cryptoDbReceipt(t, ctx, plan, "tx-unlink-plan", plan.AmountAtomic, 2)
		credited, err := cryptoCreditIntent(clientSession, plan, &cryptoCredit{usdNanoCents: plan.PriceUsdNanoCents, receipts: []model.CryptoPaymentReceiptKey{planReceipt.Key()}})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, credited, true)

		code := cryptoDbCreateIntent(t, ctx, model.CryptoAssetXmr, nil, StripeItemData1Tib, 3, 402)
		cryptoDbExec(ctx, `UPDATE crypto_payment_intent SET email = 'buyer@synthetic.example' WHERE intent_id = $1`, code.IntentId)
		code = model.GetCryptoPaymentIntent(ctx, code.IntentId)
		codeReceipt := cryptoDbReceipt(t, ctx, code, "tx-unlink-code", code.AmountAtomic, 2)
		credited, err = cryptoCreditIntent(clientSession, code, &cryptoCredit{usdNanoCents: code.PriceUsdNanoCents, receipts: []model.CryptoPaymentReceiptKey{codeReceipt.Key()}})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, credited, true)
		code = model.GetCryptoPaymentIntent(ctx, code.IntentId)

		// an invoice never paid, and an event that names the plan's invoice
		// with the network (a credit the reconciler made, say)
		abandoned := cryptoDbCreateIntent(t, ctx, model.CryptoAssetXmr, &networkId, StripeItemData10Tib, 20, 403)
		cryptoDbExec(ctx, `
			INSERT INTO payment_reconciliation_event (event_id, run_id, store, network_id, action, evidence, details, event_time)
			VALUES ($1, $2, 'monero', $3, 'credited', $4, '{}', $5)
		`, server.NewId(), server.NewId(), networkId, plan.IntentId.String(), server.NowUtc())
		otherEvent := server.NewId()
		cryptoDbExec(ctx, `
			INSERT INTO payment_reconciliation_event (event_id, run_id, store, network_id, action, evidence, details, event_time)
			VALUES ($1, $2, 'stripe', $3, 'credited', 'unrelated', '{}', $4)
		`, otherEvent, server.NewId(), networkId, server.NowUtc())

		// recent ones are kept linked
		result, err := RunCryptoPaymentUnlink(clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Unlinked, 0)

		for _, intentId := range []server.Id{plan.IntentId, code.IntentId} {
			cryptoDbExec(ctx, `UPDATE crypto_payment_intent SET credited_at = $2 WHERE intent_id = $1`, intentId, server.NowUtc().Add(-721*time.Hour))
		}
		// never credited: unlinked once unlink_after has passed since it was made
		cryptoDbExec(ctx, `UPDATE crypto_payment_intent SET create_time = $2 WHERE intent_id = $1`, abandoned.IntentId, server.NowUtc().Add(-721*time.Hour))
		result, err = RunCryptoPaymentUnlink(clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Unlinked, 3)

		// the abandoned invoice forgot its network too, and kept its day
		forgotten := model.GetCryptoPaymentIntent(ctx, abandoned.IntentId)
		connect.AssertEqual(t, forgotten.NetworkId, (*server.Id)(nil))
		connect.AssertEqual(t, forgotten.UnlinkedAt != nil, true)
		connect.AssertEqual(t, forgotten.CreateTime, forgotten.CreateTime.Truncate(24*time.Hour))

		unlinked := model.GetCryptoPaymentIntent(ctx, plan.IntentId)
		connect.AssertEqual(t, unlinked.NetworkId, (*server.Id)(nil))
		connect.AssertEqual(t, unlinked.BalanceId, (*server.Id)(nil))
		connect.AssertEqual(t, unlinked.UnlinkedAt != nil, true)
		connect.AssertEqual(t, unlinked.AmountAtomic, plan.AmountAtomic)
		// no exact time is left to join with the renewal: every time of the
		// invoice, its receipts and its pool address is the day
		day := func(value time.Time) bool { return value.Equal(value.Truncate(24 * time.Hour)) }
		connect.AssertEqual(t, day(*unlinked.CreditedAt), true)
		connect.AssertEqual(t, day(unlinked.QuotedAt), true)
		connect.AssertEqual(t, day(unlinked.ExpiresAt), true)
		connect.AssertEqual(t, day(unlinked.CreateTime), true)
		connect.AssertEqual(t, day(*unlinked.UnlinkedAt), true)
		for _, receipt := range model.GetCryptoPaymentReceiptsForIntent(ctx, plan.IntentId) {
			connect.AssertEqual(t, day(receipt.FirstSeenAt), true)
			connect.AssertEqual(t, day(receipt.UpdateTime), true)
		}
		var allocatedAt time.Time
		var quotes int
		var eventNetworks int
		var otherEventNetwork *server.Id
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT allocated_at FROM crypto_payment_address WHERE intent_id = $1`, plan.IntentId).Scan(&allocatedAt))
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM crypto_payment_quote WHERE intent_id = ANY($1)`, []server.Id{plan.IntentId, code.IntentId, abandoned.IntentId}).Scan(&quotes))
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM payment_reconciliation_event WHERE store = 'monero' AND network_id IS NOT NULL`).Scan(&eventNetworks))
			server.Raise(conn.QueryRow(ctx, `SELECT network_id FROM payment_reconciliation_event WHERE event_id = $1`, otherEvent).Scan(&otherEventNetwork))
		})
		connect.AssertEqual(t, day(allocatedAt), true)
		connect.AssertEqual(t, quotes, 0)
		// the crypto store's event about the invoice lost its network; another
		// store's event is not ours to touch
		connect.AssertEqual(t, eventNetworks, 0)
		connect.AssertEqual(t, *otherEventNetwork, networkId)
		renewal := cryptoDbRenewals(t, ctx, networkId, model.SubscriptionMarketMonero)
		connect.AssertEqual(t, renewal.count, 1)
		connect.AssertEqual(t, renewal.transactionId != plan.IntentId.String(), true)
		connect.AssertEqual(t, renewal.transactionId != "", true)
		// the entitlement itself stays
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)

		unlinkedCode := model.GetCryptoPaymentIntent(ctx, code.IntentId)
		connect.AssertEqual(t, unlinkedCode.Email, "")
		connect.AssertEqual(t, unlinkedCode.BalanceCodeId, (*server.Id)(nil))
		_, err = model.GetBalanceCodeIdForPurchaseEventId(ctx, model.CryptoPaymentPurchaseEventId(code.IntentId))
		connect.AssertEqual(t, err != nil, true)
		balanceCode, err := model.GetBalanceCode(ctx, *code.BalanceCodeId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, balanceCode.PurchaseRecord, "")
		connect.AssertEqual(t, balanceCode.PurchaseEmail, "")

		// a second pass finds nothing left
		result, err = RunCryptoPaymentUnlink(clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Unlinked, 0)
	})
}

// The hourly reconcile's Monero leg: a review cleared by a top-up is
// credited; a credited transaction that left the chain ends its grant and is
// held for review; a ledger the daemon disagrees with is an error.
func TestCryptoReconcileLegs(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		moneroReconcileHasCredentials = func() bool { return true }
		cryptoTestStandardRates(t)
		wallet := newFakeCryptoWallet(model.CryptoAssetXmr)
		cryptoTestEnable(t, cryptoTestConfig(), map[string]*fakeCryptoWallet{model.CryptoAssetXmr: wallet})
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "cryptoreconcile")

		// a review the scanner did not clear: half held as underpaid, and a
		// top-up beside it. The daemon shows both, in blocks too new for the
		// ledger's final window.
		short := cryptoDbCreateIntent(t, ctx, model.CryptoAssetXmr, &networkId, StripeItemData1Tib, 3, 501)
		first := cryptoDbReceipt(t, ctx, short, "tx-short-a", short.AmountAtomic/2, 5)
		shortId := short.IntentId
		model.SetCryptoPaymentReceiptsStateInConn(nil, ctx, &shortId, []model.CryptoPaymentReceiptKey{first.Key()}, model.CryptoReceiptStateReview, model.CryptoReviewReasonUnderpaid)
		cryptoDbReceipt(t, ctx, short, "tx-short-b", short.AmountAtomic/2+1, 3)
		wallet.pay("tx-short-a", 501, uint64(short.AmountAtomic/2))
		wallet.mine("tx-short-a", 996)
		wallet.pay("tx-short-b", 501, uint64(short.AmountAtomic/2+1))
		wallet.mine("tx-short-b", 998)

		// a credited plan whose transaction the daemon no longer has
		plan := cryptoDbCreateIntent(t, ctx, model.CryptoAssetXmr, &networkId, model.SolanaPlanYearly, 40, 502)
		planReceipt := cryptoDbReceipt(t, ctx, plan, "tx-gone", plan.AmountAtomic, 12)
		credited, err := cryptoCreditIntent(clientSession, plan, &cryptoCredit{
			usdNanoCents: plan.PriceUsdNanoCents,
			receipts:     []model.CryptoPaymentReceiptKey{planReceipt.Key()},
			receiptUsd:   map[model.CryptoPaymentReceiptKey]model.NanoCents{planReceipt.Key(): plan.PriceUsdNanoCents},
		})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, credited, true)
		model.MarkCryptoPaymentReceiptsFinalInConn(nil, ctx, model.CryptoAssetXmr, []model.CryptoPaymentReceiptKey{planReceipt.Key()}, server.NowUtc())
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)

		// the scanner is current, so the ledger leg runs; the daemon shows an
		// output in a final block that our receipts do not hold
		model.SetCryptoScanCursorSuccessInConn(nil, ctx, model.CryptoAssetXmr, 1000, 1000, 1000, 1000, server.NowUtc())
		missedIndex := uint64(77)
		wallet.stateLock.Lock()
		wallet.receipts = append(wallet.receipts, &cryptoWalletReceipt{
			TxId: "tx-missed", OutputKey: "0.77", AddressIndex: &missedIndex, AmountAtomic: 12345, BlockHeight: 950,
		})
		wallet.stateLock.Unlock()

		result, err := RunPaymentReconciliationWithOptions(
			reconcileTestSession(t, ctx),
			&PaymentReconcileRunOptions{Stores: []string{model.SubscriptionMarketMonero}},
		)
		connect.AssertEqual(t, err, nil)
		storeResult := result.StoreResults[model.SubscriptionMarketMonero]
		connect.AssertEqual(t, storeResult.Credited, 1)
		connect.AssertEqual(t, storeResult.Ended, 1)
		connect.AssertEqual(t, storeResult.Errors, 1)

		connect.AssertEqual(t, model.GetCryptoPaymentIntent(ctx, short.IntentId).CreditedAt != nil, true)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), false)
		for _, receipt := range model.GetCryptoPaymentReceiptsForIntent(ctx, plan.IntentId) {
			connect.AssertEqual(t, receipt.State, model.CryptoReceiptStateReview)
			connect.AssertEqual(t, receipt.ReviewReason, model.CryptoReviewReasonOrphaned)
		}
		// the buyer's status says so
		planIntent := model.GetCryptoPaymentIntent(ctx, plan.IntentId)
		view := cryptoEvaluateIntent(planIntent, model.GetCryptoPaymentReceiptsForIntent(ctx, plan.IntentId), cryptoIntentPolicyFor(cryptoPayConfigFunc(), model.CryptoAssetXmr), server.NowUtc())
		connect.AssertEqual(t, view.Status, CryptoPaymentStatusReview)
		connect.AssertEqual(t, view.ReviewReason, model.CryptoReviewReasonOrphaned)

		events := model.GetPaymentReconciliationEvents(ctx, result.RunId)
		connect.AssertEqual(t, countReconcileEvents(events, model.SubscriptionMarketMonero, model.PaymentReconcileActionCredited), 1)
		connect.AssertEqual(t, countReconcileEvents(events, model.SubscriptionMarketMonero, model.PaymentReconcileActionEnded), 1)
		connect.AssertEqual(t, countReconcileEvents(events, model.SubscriptionMarketMonero, model.PaymentReconcileActionError), 1)

		// the credit was taken back with its grant: the invoice is open again,
		// held for review, and an operator who sees the payment on the chain
		// grants it again through the one credit (mmm/ZCASH-CONTRACT.md §3.13.2)
		connect.AssertEqual(t, planIntent.CreditedAt, (*time.Time)(nil))
		connect.AssertEqual(t, planIntent.BalanceId, (*server.Id)(nil))
		credited, err = CryptoOperatorCredit(clientSession, plan.IntentId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, credited, true)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)
		regranted := model.GetCryptoPaymentIntent(ctx, plan.IntentId)
		connect.AssertEqual(t, regranted.CreditedAt != nil, true)
		connect.AssertEqual(t, *regranted.CreditedUsdNanoCents, plan.PriceUsdNanoCents)
		connect.AssertEqual(t, cryptoDbRenewals(t, ctx, networkId, model.SubscriptionMarketMonero).count, 2)
		for _, receipt := range model.GetCryptoPaymentReceiptsForIntent(ctx, plan.IntentId) {
			connect.AssertEqual(t, receipt.State, model.CryptoReceiptStateCredited)
		}
		// once
		_, err = CryptoOperatorCredit(clientSession, plan.IntentId)
		connect.AssertEqual(t, err != nil, true)
	})
}

// The operator tools: void closes an open invoice (a later payment is held),
// a manual credit after review grants it, and a refund ends the grant and
// records the payout address.
func TestCryptoOperatorTools(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cryptoTestEnable(t, cryptoTestConfig(), map[string]*fakeCryptoWallet{})
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "cryptooperator")

		open := cryptoDbCreateIntent(t, ctx, model.CryptoAssetXmr, &networkId, StripeItemData1Tib, 3, 601)
		voided, err := CryptoOperatorVoid(ctx, open.IntentId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, voided, true)
		voided, _ = CryptoOperatorVoid(ctx, open.IntentId)
		connect.AssertEqual(t, voided, false)

		reviewed := cryptoDbCreateIntent(t, ctx, model.CryptoAssetXmr, &networkId, model.SolanaPlanYearly, 40, 602)
		shortReceipt := cryptoDbReceipt(t, ctx, reviewed, "tx-operator", reviewed.AmountAtomic*98/100, 4)
		reviewedId := reviewed.IntentId
		model.SetCryptoPaymentReceiptsStateInConn(nil, ctx, &reviewedId, []model.CryptoPaymentReceiptKey{shortReceipt.Key()}, model.CryptoReceiptStateReview, model.CryptoReviewReasonUnderpaid)
		credited, err := CryptoOperatorCredit(clientSession, reviewed.IntentId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, credited, true)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)
		after := model.GetCryptoPaymentIntent(ctx, reviewed.IntentId)
		// credited at what arrived: 98% of the price
		connect.AssertEqual(t, *after.CreditedUsdNanoCents < reviewed.PriceUsdNanoCents, true)

		_, err = CryptoOperatorRefund(clientSession, reviewed.IntentId, "not an address")
		connect.AssertEqual(t, err != nil, true)
		payout := "5" + fmt.Sprintf("%094d", 7)
		refund, err := CryptoOperatorRefund(clientSession, reviewed.IntentId, payout)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(refund.EndedNetworkIds), 1)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), false)
		// once (mmm/ZCASH-CONTRACT.md §3.13.8)
		connect.AssertEqual(t, model.GetCryptoPaymentIntent(ctx, reviewed.IntentId).RefundedAt != nil, true)
		_, err = CryptoOperatorRefund(clientSession, reviewed.IntentId, payout)
		connect.AssertEqual(t, err != nil, true)
		var refundEvents int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM payment_reconciliation_event WHERE action = 'refunded' AND evidence = $1`, reviewed.IntentId.String()).Scan(&refundEvents))
		})
		connect.AssertEqual(t, refundEvents, 1)

		// a refund that would end nothing is refused, and not recorded
		pack := cryptoDbCreateIntent(t, ctx, model.CryptoAssetXmr, &networkId, StripeItemData1Tib, 3, 603)
		packReceipt := cryptoDbReceipt(t, ctx, pack, "tx-operator-pack", pack.AmountAtomic, 4)
		credited, err = cryptoCreditIntent(clientSession, pack, &cryptoCredit{usdNanoCents: pack.PriceUsdNanoCents, receipts: []model.CryptoPaymentReceiptKey{packReceipt.Key()}})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, credited, true)
		pack = model.GetCryptoPaymentIntent(ctx, pack.IntentId)
		cryptoDbExec(ctx, `UPDATE transfer_balance SET end_time = $2 WHERE balance_id = $1`, *pack.BalanceId, server.NowUtc().Add(-time.Hour))
		_, err = CryptoOperatorRefund(clientSession, pack.IntentId, payout)
		connect.AssertEqual(t, err != nil, true)
		connect.AssertEqual(t, model.GetCryptoPaymentIntent(ctx, pack.IntentId).RefundedAt, (*time.Time)(nil))
		// an unlinked invoice is refused
		cryptoDbExec(ctx, `UPDATE crypto_payment_intent SET unlinked_at = $2 WHERE intent_id = $1`, pack.IntentId, server.NowUtc())
		_, err = CryptoOperatorRefund(clientSession, pack.IntentId, payout)
		connect.AssertEqual(t, err != nil, true)

		// a voided invoice is never credited
		_, err = CryptoOperatorCredit(clientSession, open.IntentId)
		connect.AssertEqual(t, err != nil, true)

		listed := CryptoOperatorListInvoices(ctx, model.CryptoAssetXmr, "", 10)
		connect.AssertEqual(t, 2 <= len(listed), true)
	})
}

// The kill switch gates selling only (mmm/ZCASH-CONTRACT.md §3.13.1): a coin switched
// off is not quoted, but its scanner still credits payments to invoices
// handed out before. Here also: a double-spend-flagged mempool sighting
// counts once mined (§3.13.3); a payment the scanner sees only after the
// quote ended is on time by its block's time (§3.13.4); every refill checks
// the wallet still owns the pool's highest address (§3.13.9); and a wallet
// that derives another address than the pool holds stops the scan (§3.13.13).
func TestCryptoScanWhileSwitchedOff(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cryptoTestResetLimiters()
		cryptoTestStandardRates(t)
		wallet := newFakeCryptoWallet(model.CryptoAssetXmr)
		cryptoTestEnable(t, cryptoTestConfig(), map[string]*fakeCryptoWallet{model.CryptoAssetXmr: wallet})
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "cryptoswitchedoff")
		taskSession := session.Testing_CreateClientSession(ctx, nil)

		refill, err := RunCryptoAddressRefill(taskSession, model.CryptoAssetXmr)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, refill.Configured, true)
		connect.AssertEqual(t, refill.Added, 5)
		// the pool was empty: nothing to check yet; the next refill checks the
		// highest address
		connect.AssertEqual(t, len(wallet.lookaheadChecks), 0)
		_, err = RunCryptoAddressRefill(taskSession, model.CryptoAssetXmr)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, wallet.lookaheadChecks, []string{fmt.Sprintf("5 %s", fakeCryptoAddress(model.CryptoAssetXmr, 5))})
		_, err = RunCryptoPaymentScan(taskSession, model.CryptoAssetXmr)
		connect.AssertEqual(t, err, nil)
		// the identity check derived the lowest pool address again
		connect.AssertEqual(t, wallet.addressChecks[len(wallet.addressChecks)-1], fmt.Sprintf("1 %s", fakeCryptoAddress(model.CryptoAssetXmr, 1)))
		resetCryptoAvailability()

		plan, err := CreateCryptoPaymentIntent(&CreateCryptoPaymentIntentArgs{Asset: model.CryptoAssetXmr, Plan: model.SolanaPlanYearly}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, plan.Error, (*CryptoPaymentError)(nil))
		pack, err := CreateCryptoPaymentIntent(&CreateCryptoPaymentIntentArgs{Asset: model.CryptoAssetXmr, ItemId: StripeItemData1Tib}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, pack.Error, (*CryptoPaymentError)(nil))

		// switched off: not sold any more
		cryptoPayConfigFunc().Assets[model.CryptoAssetXmr].Enabled = false
		refused, err := CreateCryptoPaymentIntent(&CreateCryptoPaymentIntentArgs{Asset: model.CryptoAssetXmr, ItemId: StripeItemData10Tib}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, refused.Error.Code, PurchaseErrorCodeAssetUnavailable)
		requote, err := CryptoPaymentRequote(&CryptoPaymentStatusArgs{IntentId: plan.IntentId, StatusToken: plan.StatusToken}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, requote.Error.Code, PurchaseErrorCodeAssetUnavailable)

		// the plan is paid by a transaction the wallet flags in the mempool
		planIntent := model.GetCryptoPaymentIntent(ctx, server.RequireParseId(plan.IntentId))
		wallet.payFlagged("tx-flagged", uint64(*planIntent.AddressIndex), uint64(planIntent.AmountAtomic))
		scan, err := RunCryptoPaymentScan(taskSession, model.CryptoAssetXmr)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, scan.Configured, true)
		connect.AssertEqual(t, scan.NewReceipts, 1)
		receipts := model.GetCryptoPaymentReceiptsForIntent(ctx, planIntent.IntentId)
		connect.AssertEqual(t, receipts[0].State, model.CryptoReceiptStateSeen)
		connect.AssertEqual(t, receipts[0].ReviewReason, model.CryptoReviewReasonDoubleSpend)
		status, err := CryptoPaymentStatus(&CryptoPaymentStatusArgs{IntentId: plan.IntentId, StatusToken: plan.StatusToken}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, status.Status, CryptoPaymentStatusPending)
		// mined, unflagged: confirmed, counted, and credited while switched off
		wallet.mineAt("tx-flagged", 1001, server.NowUtc())
		wallet.setHeight(1001)
		scan, err = RunCryptoPaymentScan(taskSession, model.CryptoAssetXmr)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, scan.Credited, 1)
		status, err = CryptoPaymentStatus(&CryptoPaymentStatusArgs{IntentId: plan.IntentId, StatusToken: plan.StatusToken}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, status.Status, CryptoPaymentStatusPaid)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)

		// the scanner was down: the pack was paid in a block 20 minutes into
		// its quote and is seen only after the quote ended. On time, at the price
		packIntentId := server.RequireParseId(pack.IntentId)
		now := server.NowUtc()
		cryptoDbExec(ctx, `UPDATE crypto_payment_intent SET quoted_at = $2, expires_at = $3 WHERE intent_id = $1`,
			packIntentId, now.Add(-40*time.Minute), now.Add(-10*time.Minute))
		packIntent := model.GetCryptoPaymentIntent(ctx, packIntentId)
		blockTime := now.Add(-20 * time.Minute)
		wallet.pay("tx-while-down", uint64(*packIntent.AddressIndex), uint64(packIntent.AmountAtomic))
		wallet.mineAt("tx-while-down", 1002, blockTime)
		wallet.setHeight(1002)
		scan, err = RunCryptoPaymentScan(taskSession, model.CryptoAssetXmr)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, scan.Credited, 1)
		packIntent = model.GetCryptoPaymentIntent(ctx, packIntentId)
		connect.AssertEqual(t, packIntent.CreditedAt != nil, true)
		connect.AssertEqual(t, *packIntent.CreditedUsdNanoCents, packIntent.PriceUsdNanoCents)
		receipts = model.GetCryptoPaymentReceiptsForIntent(ctx, packIntentId)
		connect.AssertEqual(t, receipts[0].FirstSeenAt.Unix(), blockTime.Unix())
		connect.AssertEqual(t, *receipts[0].FirstSeenHeight, int64(1002))

		// a wallet that derives another address than the pool holds: the scan
		// stops, and says so
		wallet.addressErr = fmt.Errorf("synthetic: another wallet")
		scan, err = RunCryptoPaymentScan(taskSession, model.CryptoAssetXmr)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, strings.Contains(scan.Error, "another wallet"), true)
		connect.AssertEqual(t, strings.Contains(model.GetCryptoScanCursor(ctx, model.CryptoAssetXmr).LastError, "another wallet"), true)
		refill, err = RunCryptoAddressRefill(taskSession, model.CryptoAssetXmr)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, strings.Contains(refill.Error, "another wallet"), true)
	})
}

// A credit that keeps failing is retried scan after scan, then held for
// review (credit_failed) after model.CryptoPaymentCreditFailureLimit in a row,
// and the scanner stops trying (mmm/ZCASH-CONTRACT.md §3.13.16).
func TestCryptoCreditFailuresGoToReview(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cryptoTestStandardRates(t)
		wallet := newFakeCryptoWallet(model.CryptoAssetXmr)
		cryptoTestEnable(t, cryptoTestConfig(), map[string]*fakeCryptoWallet{model.CryptoAssetXmr: wallet})
		taskSession := session.Testing_CreateClientSession(ctx, nil)

		// an invoice whose network is gone: every credit fails
		missingNetworkId := server.NewId()
		intent := cryptoDbCreateIntent(t, ctx, model.CryptoAssetXmr, &missingNetworkId, StripeItemData1Tib, 3, 701)
		wallet.pay("tx-failing", 701, uint64(intent.AmountAtomic))
		wallet.mine("tx-failing", 1000)
		for i := 1; i <= model.CryptoPaymentCreditFailureLimit; i += 1 {
			scan, err := RunCryptoPaymentScan(taskSession, model.CryptoAssetXmr)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, scan.CreditErrored, 1)
			connect.AssertEqual(t, model.GetCryptoPaymentIntent(ctx, intent.IntentId).CreditFailureCount, i)
		}
		receipts := model.GetCryptoPaymentReceiptsForIntent(ctx, intent.IntentId)
		connect.AssertEqual(t, receipts[0].State, model.CryptoReceiptStateReview)
		connect.AssertEqual(t, receipts[0].ReviewReason, model.CryptoReviewReasonCreditFailed)
		view := cryptoEvaluateIntent(model.GetCryptoPaymentIntent(ctx, intent.IntentId), receipts, cryptoIntentPolicyFor(cryptoPayConfigFunc(), model.CryptoAssetXmr), server.NowUtc())
		connect.AssertEqual(t, view.Status, CryptoPaymentStatusReview)
		connect.AssertEqual(t, view.ReviewReason, model.CryptoReviewReasonCreditFailed)
		// no more attempts
		scan, err := RunCryptoPaymentScan(taskSession, model.CryptoAssetXmr)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, scan.CreditErrored, 0)
		connect.AssertEqual(t, model.GetCryptoPaymentIntent(ctx, intent.IntentId).CreditFailureCount, model.CryptoPaymentCreditFailureLimit)
	})
}

// The scanner takes a daemon's "not found" as proof that a payment left the
// chain only from a wallet that has caught up with a known network height,
// and only the final depth past the receipt: its block, or the wallet's
// height when it was first seen in the mempool (mmm/ZCASH-CONTRACT.md §3.13.2).
func TestCryptoScannerOrphansOnlyFromACaughtUpWallet(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cryptoTestStandardRates(t)
		// zec: credited at 10 confirmations, final at 24
		wallet := newFakeCryptoWallet(model.CryptoAssetZec)
		cryptoTestEnable(t, cryptoTestConfig(), map[string]*fakeCryptoWallet{model.CryptoAssetZec: wallet})
		networkId, _, _ := cryptoDbTestNetwork(t, ctx, "cryptoorphanproof")
		taskSession := session.Testing_CreateClientSession(ctx, nil)
		scan := func() *CryptoPaymentScanResult {
			result, err := RunCryptoPaymentScan(taskSession, model.CryptoAssetZec)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, result.Error, "")
			return result
		}
		state := func(intentId server.Id) string {
			return model.GetCryptoPaymentReceiptsForIntent(ctx, intentId)[0].State
		}

		mined := cryptoDbCreateIntent(t, ctx, model.CryptoAssetZec, &networkId, StripeItemData1Tib, 3, 801)
		wallet.pay("tx-mined", 801, uint64(mined.AmountAtomic))
		wallet.mine("tx-mined", 995)
		scan()
		connect.AssertEqual(t, state(mined.IntentId), model.CryptoReceiptStateConfirmed)
		wallet.drop("tx-mined")

		// a wallet behind a known network is no proof
		wallet.setHeights(1000, 1100)
		result := scan()
		connect.AssertEqual(t, 0 < result.OrphanChecksSkipped, true)
		connect.AssertEqual(t, state(mined.IntentId), model.CryptoReceiptStateConfirmed)
		// nor one that does not know the network's height
		wallet.setHeights(1000, 0)
		scan()
		connect.AssertEqual(t, state(mined.IntentId), model.CryptoReceiptStateConfirmed)
		// caught up, but not yet the final depth past the block
		wallet.setHeight(1018)
		scan()
		connect.AssertEqual(t, state(mined.IntentId), model.CryptoReceiptStateConfirmed)
		// the final depth past it: gone
		wallet.setHeight(1019)
		result = scan()
		connect.AssertEqual(t, result.Orphaned, 1)
		connect.AssertEqual(t, state(mined.IntentId), model.CryptoReceiptStateOrphaned)

		// a mempool sighting that disappears: gone the final depth after it was
		// first seen
		mempool := cryptoDbCreateIntent(t, ctx, model.CryptoAssetZec, &networkId, StripeItemData10Tib, 20, 802)
		wallet.pay("tx-mempool", 802, uint64(mempool.AmountAtomic))
		scan()
		first := model.GetCryptoPaymentReceiptsForIntent(ctx, mempool.IntentId)[0]
		connect.AssertEqual(t, *first.FirstSeenHeight, int64(1019))
		wallet.drop("tx-mempool")
		wallet.setHeight(1042)
		scan()
		connect.AssertEqual(t, state(mempool.IntentId), model.CryptoReceiptStateSeen)
		wallet.setHeight(1043)
		scan()
		connect.AssertEqual(t, state(mempool.IntentId), model.CryptoReceiptStateOrphaned)
	})
}

// Reconcile leg 2 ends a credit only on the word of a wallet that passed its
// identity check, whose scanner is current and within max_lag_blocks of a
// known network height, the final depth past the receipt; and never more
// than cryptoReconcileMaxOrphansPerRun credits in one run (mmm/ZCASH-CONTRACT.md
// §3.13.2). A wallet rebuilt or resyncing shows nothing, and must end nothing.
func TestCryptoReconcileOrphanNeedsATrustedWallet(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		moneroReconcileHasCredentials = func() bool { return true }
		cryptoTestStandardRates(t)
		wallet := newFakeCryptoWallet(model.CryptoAssetXmr)
		cryptoTestEnable(t, cryptoTestConfig(), map[string]*fakeCryptoWallet{model.CryptoAssetXmr: wallet})
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "cryptoreconcileproof")

		// a plan and three data packs, credited, final, and all four unknown to
		// the wallet (as a wallet rebuilt from keys would answer)
		credit := func(item string, priceUsd float64, index int64, txId string) *model.CryptoPaymentIntent {
			intent := cryptoDbCreateIntent(t, ctx, model.CryptoAssetXmr, &networkId, item, priceUsd, index)
			receipt := cryptoDbReceipt(t, ctx, intent, txId, intent.AmountAtomic, 12)
			credited, err := cryptoCreditIntent(clientSession, intent, &cryptoCredit{
				usdNanoCents: intent.PriceUsdNanoCents,
				receipts:     []model.CryptoPaymentReceiptKey{receipt.Key()},
				receiptUsd:   map[model.CryptoPaymentReceiptKey]model.NanoCents{receipt.Key(): intent.PriceUsdNanoCents},
			})
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, credited, true)
			model.MarkCryptoPaymentReceiptsFinalInConn(nil, ctx, model.CryptoAssetXmr, []model.CryptoPaymentReceiptKey{receipt.Key()}, server.NowUtc())
			return intent
		}
		plan := credit(model.SolanaPlanYearly, 40, 901, "tx-plan")
		packs := []*model.CryptoPaymentIntent{
			credit(StripeItemData1Tib, 3, 902, "tx-pack-a"),
			credit(StripeItemData1Tib, 3, 903, "tx-pack-b"),
			credit(StripeItemData1Tib, 3, 904, "tx-pack-c"),
		}
		reconcile := func() *PaymentReconcileStoreResult {
			result, err := RunPaymentReconciliationWithOptions(
				reconcileTestSession(t, ctx),
				&PaymentReconcileRunOptions{Stores: []string{model.SubscriptionMarketMonero}},
			)
			connect.AssertEqual(t, err, nil)
			return result.StoreResults[model.SubscriptionMarketMonero]
		}
		stillCredited := func() {
			connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)
			connect.AssertEqual(t, model.GetCryptoPaymentIntent(ctx, plan.IntentId).CreditedAt != nil, true)
		}

		// the scanner not current
		model.SetCryptoScanCursorSuccessInConn(nil, ctx, model.CryptoAssetXmr, 1000, 1000, 1000, 1000, server.NowUtc().Add(-time.Hour))
		result := reconcile()
		connect.AssertEqual(t, result.Ended, 0)
		connect.AssertEqual(t, result.Errors, 1)
		stillCredited()
		// the network's height unknown, or the wallet behind it
		model.SetCryptoScanCursorSuccessInConn(nil, ctx, model.CryptoAssetXmr, 1000, 1000, 0, 0, server.NowUtc())
		connect.AssertEqual(t, reconcile().Ended, 0)
		model.SetCryptoScanCursorSuccessInConn(nil, ctx, model.CryptoAssetXmr, 1000, 1000, 1010, 1010, server.NowUtc())
		connect.AssertEqual(t, reconcile().Ended, 0)
		stillCredited()
		// another wallet than the pool's
		model.SetCryptoScanCursorSuccessInConn(nil, ctx, model.CryptoAssetXmr, 1000, 1000, 1000, 1000, server.NowUtc())
		wallet.addressErr = fmt.Errorf("synthetic: another wallet")
		connect.AssertEqual(t, reconcile().Ended, 0)
		stillCredited()
		wallet.addressErr = nil

		// a trusted wallet that shows none of the four: too many at once, so
		// nothing is ended and an operator is told (the ledger leg disagrees
		// too, as it should)
		result = reconcile()
		connect.AssertEqual(t, result.Ended, 0)
		connect.AssertEqual(t, 1 <= result.Errors, true)
		var breakerEvents int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `
				SELECT count(*) FROM payment_reconciliation_event
				WHERE store = 'monero' AND action = 'error' AND details LIKE '%no longer shown by the wallet at once%'
			`).Scan(&breakerEvents))
		})
		connect.AssertEqual(t, breakerEvents, 1)
		stillCredited()

		// it shows the three packs again: the plan alone is gone, and ended
		for i, pack := range packs {
			txId := []string{"tx-pack-a", "tx-pack-b", "tx-pack-c"}[i]
			wallet.pay(txId, uint64(*pack.AddressIndex), uint64(pack.AmountAtomic))
			wallet.mine(txId, 989)
		}
		result = reconcile()
		connect.AssertEqual(t, result.Ended, 1)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), false)
		taken := model.GetCryptoPaymentIntent(ctx, plan.IntentId)
		connect.AssertEqual(t, taken.CreditedAt, (*time.Time)(nil))
		for _, pack := range packs {
			connect.AssertEqual(t, model.GetCryptoPaymentIntent(ctx, pack.IntentId).CreditedAt != nil, true)
		}

		// a receipt not yet the final depth behind the wallet is left alone
		young := cryptoDbCreateIntent(t, ctx, model.CryptoAssetXmr, &networkId, StripeItemData10Tib, 20, 905)
		youngReceipt := cryptoDbReceipt(t, ctx, young, "tx-young", young.AmountAtomic, 5)
		credited, err := cryptoCreditIntent(clientSession, young, &cryptoCredit{usdNanoCents: young.PriceUsdNanoCents, receipts: []model.CryptoPaymentReceiptKey{youngReceipt.Key()}})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, credited, true)
		model.MarkCryptoPaymentReceiptsFinalInConn(nil, ctx, model.CryptoAssetXmr, []model.CryptoPaymentReceiptKey{youngReceipt.Key()}, server.NowUtc())
		result = reconcile()
		connect.AssertEqual(t, result.Ended, 0)
		connect.AssertEqual(t, model.GetCryptoPaymentIntent(ctx, young.IntentId).CreditedAt != nil, true)
	})
}

// Invoice reuse is per delivery: the buy-data page naming a network never
// gets the signed-in account's own invoice, nor the reverse (mmm/ZCASH-CONTRACT.md
// §3.13.5). Open code invoices of a coin are capped (§3.13.15).
func TestCryptoReusePerDeliveryAndCodeCap(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cryptoTestResetLimiters()
		cryptoTestStandardRates(t)
		wallet := newFakeCryptoWallet(model.CryptoAssetXmr)
		config := cryptoTestConfig()
		config.AddressPoolTarget = 10
		cryptoTestEnable(t, config, map[string]*fakeCryptoWallet{model.CryptoAssetXmr: wallet})
		_, _, clientSession := cryptoDbTestNetwork(t, ctx, "cryptoreuse")
		pageSession := session.Testing_CreateClientSession(ctx, nil)
		taskSession := session.Testing_CreateClientSession(ctx, nil)
		_, err := RunCryptoAddressRefill(taskSession, model.CryptoAssetXmr)
		connect.AssertEqual(t, err, nil)
		_, err = RunCryptoPaymentScan(taskSession, model.CryptoAssetXmr)
		connect.AssertEqual(t, err, nil)
		resetCryptoAvailability()

		account, err := CreateCryptoPaymentIntent(&CreateCryptoPaymentIntentArgs{Asset: model.CryptoAssetXmr, ItemId: StripeItemData1Tib}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, account.Error, (*CryptoPaymentError)(nil))
		named, err := CreateDataCryptoPaymentIntent(&CreateDataCryptoPaymentIntentArgs{Asset: model.CryptoAssetXmr, ItemId: StripeItemData1Tib, NetworkName: "cryptoreuse"}, pageSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, named.Error, (*CryptoPaymentError)(nil))
		connect.AssertEqual(t, named.Delivery, model.CryptoDeliveryNetwork)
		connect.AssertEqual(t, named.IntentId != account.IntentId, true)
		// the page's token reads its own invoice, not the account's
		status, err := CryptoPaymentStatus(&CryptoPaymentStatusArgs{IntentId: account.IntentId, StatusToken: named.StatusToken}, pageSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, status.Status, CryptoPaymentStatusUnknown)
		// each delivery reuses its own
		namedAgain, err := CreateDataCryptoPaymentIntent(&CreateDataCryptoPaymentIntentArgs{Asset: model.CryptoAssetXmr, ItemId: StripeItemData1Tib, NetworkName: "cryptoreuse"}, pageSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, namedAgain.IntentId, named.IntentId)
		accountAgain, err := CreateCryptoPaymentIntent(&CreateCryptoPaymentIntentArgs{Asset: model.CryptoAssetXmr, ItemId: StripeItemData1Tib}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, accountAgain.IntentId, account.IntentId)

		// two open code invoices of the coin, and no third
		cryptoPayConfigFunc().MaxOpenCodeIntentsPerAsset = 2
		for i := 0; i < 2; i += 1 {
			code, err := CreateDataCryptoPaymentIntent(&CreateDataCryptoPaymentIntentArgs{Asset: model.CryptoAssetXmr, ItemId: StripeItemData1Tib}, pageSession)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, code.Error, (*CryptoPaymentError)(nil))
		}
		third, err := CreateDataCryptoPaymentIntent(&CreateDataCryptoPaymentIntentArgs{Asset: model.CryptoAssetXmr, ItemId: StripeItemData1Tib}, pageSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, third.Error.Code, PurchaseErrorCodeAssetUnavailable)
		// what is left of the pool stays for everyone else
		connect.AssertEqual(t, model.CountFreeCryptoPaymentAddresses(ctx, model.CryptoAssetXmr), 6)
	})
}

// A voided invoice is never credited (the one-shot requires voided_at IS
// NULL), and reads review/voided once paid; a USDC payment to a voided
// invoice is recorded as unfulfilled (mmm/ZCASH-CONTRACT.md §3.13.12). The expired
// Solana intent cleanup keeps an intent its invoice still links (§3.13.11),
// and the old buy-data status does not answer for it (§3.13.7).
func TestCryptoVoidHoldsPaymentsAndKeepsLinkedIntents(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cryptoTestResetLimiters()
		cryptoTestStandardRates(t)
		wallet := newFakeCryptoWallet(model.CryptoAssetXmr)
		cryptoTestEnable(t, cryptoTestConfig(), map[string]*fakeCryptoWallet{model.CryptoAssetXmr: wallet})
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "cryptovoid")
		webhookSession := session.Testing_CreateClientSession(ctx, nil)
		taskSession := session.Testing_CreateClientSession(ctx, nil)
		solanaTestAmountSuffixes(t, 401, 402)

		// xmr: voided, then paid
		voidable := cryptoDbCreateIntent(t, ctx, model.CryptoAssetXmr, &networkId, StripeItemData1Tib, 3, 1001)
		voided, err := CryptoOperatorVoid(ctx, voidable.IntentId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, voided, true)
		receipt := cryptoDbReceipt(t, ctx, voidable, "tx-to-voided", voidable.AmountAtomic, 3)
		// the one-shot refuses a voided invoice
		credited, err := cryptoCreditIntent(clientSession, voidable, &cryptoCredit{usdNanoCents: voidable.PriceUsdNanoCents, receipts: []model.CryptoPaymentReceiptKey{receipt.Key()}})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, credited, false)
		wallet.pay("tx-to-voided", 1001, uint64(voidable.AmountAtomic))
		wallet.mine("tx-to-voided", 998)
		_, err = RunCryptoPaymentScan(taskSession, model.CryptoAssetXmr)
		connect.AssertEqual(t, err, nil)
		for range 2 {
			// it stays review/voided after the scanner moved it there
			view := cryptoEvaluateIntent(model.GetCryptoPaymentIntent(ctx, voidable.IntentId), model.GetCryptoPaymentReceiptsForIntent(ctx, voidable.IntentId), cryptoIntentPolicyFor(cryptoPayConfigFunc(), model.CryptoAssetXmr), server.NowUtc())
			connect.AssertEqual(t, view.Status, CryptoPaymentStatusReview)
			connect.AssertEqual(t, view.ReviewReason, model.CryptoReviewReasonVoided)
			_, err = RunCryptoPaymentScan(taskSession, model.CryptoAssetXmr)
			connect.AssertEqual(t, err, nil)
		}
		connect.AssertEqual(t, model.GetCryptoPaymentIntent(ctx, voidable.IntentId).CreditedAt, (*time.Time)(nil))

		// usdc: voided, then paid through Helius: unfulfilled, for an operator
		usdc, err := CreateCryptoPaymentIntent(&CreateCryptoPaymentIntentArgs{Asset: model.CryptoAssetUsdcSolana, Plan: model.SolanaPlanYearly}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, usdc.Error, (*CryptoPaymentError)(nil))
		voided, err = CryptoOperatorVoid(ctx, server.RequireParseId(usdc.IntentId))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, voided, true)
		webhook, err := HeliusWebhook([]*SolanaTransaction{solanaTestPayment(usdc.Memo, "sig-to-voided", 40.000401)}, webhookSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, webhook.Message != "Processed 1 matching payments", true)
		unfulfilled, err := model.GetUnfulfilledSolanaPayment(ctx, "sig-to-voided")
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, strings.Contains(*unfulfilled.MatchNote, "voided"), true)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), false)
		connect.AssertEqual(t, model.GetSolanaPaymentIntent(ctx, usdc.Memo).TxSignature, (*string)(nil))
		usdcStatus, err := CryptoPaymentStatus(&CryptoPaymentStatusArgs{IntentId: usdc.IntentId, StatusToken: usdc.StatusToken}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, usdcStatus.Status, CryptoPaymentStatusExpired)

		// the cleanup keeps the expired Solana intent while its invoice links
		// it, and sweeps it after the invoice's unlink
		cryptoDbExec(ctx, `UPDATE solana_payment_intent SET expires_at = $2 WHERE payment_reference = $1`, usdc.Memo, server.NowUtc().Add(-48*time.Hour))
		connect.AssertEqual(t, model.CleanupExpiredPaymentIntents(ctx, server.NowUtc().Add(-24*time.Hour)), nil)
		connect.AssertEqual(t, model.GetSolanaPaymentIntent(ctx, usdc.Memo) != nil, true)
		cryptoDbExec(ctx, `UPDATE crypto_payment_intent SET unlinked_at = $2 WHERE intent_id = $1`, server.RequireParseId(usdc.IntentId), server.NowUtc())
		connect.AssertEqual(t, model.CleanupExpiredPaymentIntents(ctx, server.NowUtc().Add(-24*time.Hour)), nil)
		connect.AssertEqual(t, model.GetSolanaPaymentIntent(ctx, usdc.Memo), (*model.SolanaPaymentIntent)(nil))

		// a USDC data pack for the named network: the old status does not name it
		named, err := CreateDataCryptoPaymentIntent(&CreateDataCryptoPaymentIntentArgs{Asset: model.CryptoAssetUsdcSolana, ItemId: StripeItemData1Tib, NetworkName: "cryptovoid"}, webhookSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, named.Error, (*CryptoPaymentError)(nil))
		legacy, err := PayDataSolanaStatus(&PayDataSolanaStatusArgs{Reference: named.Memo}, webhookSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, legacy.Status, PayDataSolanaStatusUnknown)
		connect.AssertEqual(t, legacy.NetworkName, "")
	})
}
