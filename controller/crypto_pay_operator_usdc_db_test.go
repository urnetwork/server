package controller

// Database-backed tests of `bringyourctl crypto credit-usdc`
// (crypto_pay_operator_usdc.go): the root cause (support inbox 8228, 8229
// and 8241; owner queue OQ-26), the credit made exactly once, every refusal,
// a Helius redelivery or a reconcile run racing or following the credit, two
// commits racing, a digest that no longer matches, and the credit rolled back
// with its record. Needs the test database (server.DefaultTestEnv) and
// pro.yml; the pure pieces are in crypto_pay_operator_usdc_test.go.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// cryptoUsdcMemoless is an open USDC invoice for a year of Pro, and the
// payment an exchange sent for it without the reference, its own fee added to
// the quote: the case of support inbox 8228.
type cryptoUsdcMemoless struct {
	networkId     server.Id
	clientSession *session.ClientSession
	intent        *model.CryptoPaymentIntent
	statusToken   string
	reference     string
	signature     string
	amountUsd     float64
	payment       *SolanaTransaction
}

// newCryptoUsdcMemoless quotes the invoice (its unique suffix is suffixMicro)
// and builds the payment of the quote plus overMicro, sent a minute after the
// quote, without sending it.
func newCryptoUsdcMemoless(t testing.TB, ctx context.Context, name string, suffixMicro int64, overMicro int64) *cryptoUsdcMemoless {
	cryptoTestResetLimiters()
	networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, name)
	solanaTestAmountSuffixes(t, suffixMicro)
	result, err := CreateCryptoPaymentIntent(&CreateCryptoPaymentIntentArgs{Asset: model.CryptoAssetUsdcSolana, Plan: model.SolanaPlanYearly}, clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Error, (*CryptoPaymentError)(nil))
	intent := model.GetCryptoPaymentIntent(ctx, server.RequireParseId(result.IntentId))
	connect.AssertEqual(t, intent.SolanaReference != nil, true)
	fixture := &cryptoUsdcMemoless{
		networkId:     networkId,
		clientSession: clientSession,
		intent:        intent,
		statusToken:   result.StatusToken,
		reference:     *intent.SolanaReference,
		signature:     "sig-usdc-credit-" + name,
		amountUsd:     model.SolanaMicroToUsd(intent.AmountAtomic + overMicro),
	}
	fixture.payment = solanaMemolessPayment(fixture.signature, solanaReceiverAddresses[0], fixture.amountUsd, intent.QuotedAt.Add(time.Minute))
	return fixture
}

// send delivers the payment through the Helius webhook, which records it
// unfulfilled: no open intent was quoted its amount.
func (self *cryptoUsdcMemoless) send(t testing.TB, ctx context.Context) {
	webhook, err := HeliusWebhook([]*SolanaTransaction{self.payment}, session.Testing_CreateClientSession(ctx, nil))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, webhook.Message, "No payment intent found for this network ID")
	recorded, err := model.GetUnfulfilledSolanaPayment(ctx, self.signature)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, recorded.Reason, model.SolanaUnfulfilledReasonNoIntent)
}

// the network's paid periods' own Pro balances: no grant kind, and no data (the
// period's cycle grants, kind pro, hold it)
func cryptoUsdcProBalances(ctx context.Context, networkId server.Id) []*model.TransferBalance {
	balances := []*model.TransferBalance{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT balance_id, start_time, end_time, start_balance_byte_count
				FROM transfer_balance
				WHERE network_id = $1 AND pro AND grant_kind IS NULL AND start_time <= $2 AND $2 < end_time
			`,
			networkId,
			server.NowUtc(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				balance := &model.TransferBalance{NetworkId: networkId, Pro: true}
				server.Raise(result.Scan(&balance.BalanceId, &balance.StartTime, &balance.EndTime, &balance.StartBalanceByteCount))
				balances = append(balances, balance)
			}
		})
	})
	return balances
}

// cryptoUsdcUncredited asserts nothing was written for the fixture: the
// invoice is open, its Solana Pay intent unpaid, the record still there, and
// the network has no Pro and no renewal.
func cryptoUsdcUncredited(t testing.TB, ctx context.Context, fixture *cryptoUsdcMemoless) {
	connect.AssertEqual(t, model.GetCryptoPaymentIntent(ctx, fixture.intent.IntentId).CreditedAt, (*time.Time)(nil))
	connect.AssertEqual(t, model.GetSolanaPaymentIntent(ctx, fixture.reference).TxSignature, (*string)(nil))
	connect.AssertEqual(t, model.IsSolanaPaymentCompleted(ctx, fixture.signature), false)
	_, err := model.GetUnfulfilledSolanaPayment(ctx, fixture.signature)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, model.IsProNetwork(ctx, fixture.networkId), false)
	connect.AssertEqual(t, len(cryptoUsdcProBalances(ctx, fixture.networkId)), 0)
	connect.AssertEqual(t, cryptoDbRenewals(t, ctx, fixture.networkId, model.SubscriptionMarketSolana).count, 0)
}

// cryptoUsdcCreditedOnce asserts the fixture's invoice is credited once by
// its payment: one renewal and one Pro balance, the invoice and its Solana
// Pay intent paid, the signature completed, the record gone.
func cryptoUsdcCreditedOnce(t testing.TB, ctx context.Context, fixture *cryptoUsdcMemoless) {
	connect.AssertEqual(t, cryptoDbRenewals(t, ctx, fixture.networkId, model.SubscriptionMarketSolana).count, 1)
	connect.AssertEqual(t, len(cryptoUsdcProBalances(ctx, fixture.networkId)), 1)
	connect.AssertEqual(t, model.IsProNetwork(ctx, fixture.networkId), true)
	connect.AssertEqual(t, model.GetCryptoPaymentIntent(ctx, fixture.intent.IntentId).CreditedAt != nil, true)
	connect.AssertEqual(t, *model.GetSolanaPaymentIntent(ctx, fixture.reference).TxSignature, fixture.signature)
	connect.AssertEqual(t, model.IsSolanaPaymentCompleted(ctx, fixture.signature), true)
	_, err := model.GetUnfulfilledSolanaPayment(ctx, fixture.signature)
	connect.AssertNotEqual(t, err, nil)
}

func cryptoUsdcReconcileSolana(t testing.TB, ctx context.Context) *PaymentReconcileRunResult {
	result, err := RunPaymentReconciliationWithOptions(
		reconcileTestSession(t, ctx),
		&PaymentReconcileRunOptions{Stores: []string{model.SubscriptionMarketSolana}},
	)
	connect.AssertEqual(t, err, nil)
	return result
}

// What a reconcile run credited in the solana store, by its events and its
// tally. A run that examined nothing there has no tally for the store.
func cryptoUsdcReconcileCredited(ctx context.Context, result *PaymentReconcileRunResult) int {
	credited := countReconcileEvents(
		model.GetPaymentReconciliationEvents(ctx, result.RunId),
		model.SubscriptionMarketSolana,
		model.PaymentReconcileActionCredited,
	)
	if storeResult := result.StoreResults[model.SubscriptionMarketSolana]; storeResult != nil {
		credited = max(credited, storeResult.Credited)
	}
	return credited
}

func cryptoUsdcHasRefusal(plan *CryptoUsdcCreditPlan, refusal string) bool {
	for _, text := range plan.Refusals {
		if strings.Contains(text, refusal) {
			return true
		}
	}
	return false
}

// Root cause (support inbox 8228, 8229 and 8241; owner queue OQ-26): an
// exchange sent the quote plus its own fee, without the reference. The
// webhook matches a memo-less payment only by its exact unique amount, so it
// records the payment no_intent; the reconciler credits a no_intent record
// only once its reference resolves to an open intent, and this one carries
// none; and `crypto credit` refuses a USDC invoice. The money bought nothing,
// and no tool could credit it. Once the owner approves, `crypto credit-usdc`
// credits it, once.
func TestCryptoUsdcOverpaidMemolessPaymentStaysUncreditedUntilTheOperatorCredit(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		solanaEnv := newSolanaReconcileTestEnv(t)
		cryptoTestEnable(t, cryptoTestConfig(), map[string]*fakeCryptoWallet{})
		fixture := newCryptoUsdcMemoless(t, ctx, "usdcrootcause", 5505, 87_604)
		solanaEnv.onChain[fixture.signature] = true
		connect.AssertEqual(t, model.SolanaUsdToMicro(fixture.amountUsd), int64(40_093_109))

		// the webhook: no reference, and no intent was quoted 40.093109 USDC
		fixture.send(t, ctx)
		recorded, err := model.GetUnfulfilledSolanaPayment(ctx, fixture.signature)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, *recorded.MatchNote, "memo-less: no intent was quoted 40.093109 USDC")
		cryptoUsdcUncredited(t, ctx, fixture)
		// a redelivery decides the same
		fixture.send(t, ctx)
		cryptoUsdcUncredited(t, ctx, fixture)

		// the reconciler: no reference resolves
		result := cryptoUsdcReconcileSolana(t, ctx)
		connect.AssertEqual(t, result.StoreResults[model.SubscriptionMarketSolana].Examined, 1)
		connect.AssertEqual(t, cryptoUsdcReconcileCredited(ctx, result), 0)
		cryptoUsdcUncredited(t, ctx, fixture)

		// `crypto credit`: a USDC invoice is not its to credit
		_, err = CryptoOperatorCredit(fixture.clientSession, fixture.intent.IntentId, false)
		connect.AssertEqual(t, err != nil && strings.Contains(err.Error(), "a USDC invoice is credited by its Solana payment"), true)
		cryptoUsdcUncredited(t, ctx, fixture)

		// the owner-approved credit of the record
		plan, err := CryptoOperatorPlanUsdcCredit(ctx, fixture.intent.IntentId, fixture.signature)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, plan.State, CryptoUsdcCreditStateCredit)
		committed, err := CryptoOperatorCommitUsdcCredit(fixture.clientSession, fixture.intent.IntentId, fixture.signature, plan.Digest)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, committed.State, CryptoUsdcCreditStateCredited)
		cryptoUsdcCreditedOnce(t, ctx, fixture)
	})
}

// An overpaid memo-less payment credits its invoice exactly once: on time at
// the price, a year of Pro with the plan's balance, the invoice and its Solana
// Pay intent paid by the signature, the record gone. A commit with another
// digest writes nothing first; after the credit, a Helius redelivery and a
// reconcile run credit nothing more, and the same commit again is refused.
func TestCryptoOperatorUsdcCreditCreditsAnOverpaidMemolessPaymentOnce(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		solanaEnv := newSolanaReconcileTestEnv(t)
		cryptoTestEnable(t, cryptoTestConfig(), map[string]*fakeCryptoWallet{})
		fixture := newCryptoUsdcMemoless(t, ctx, "usdccreditonce", 5505, 87_604)
		solanaEnv.onChain[fixture.signature] = true
		fixture.send(t, ctx)
		intent := fixture.intent

		// the dry run reads and writes nothing
		plan, err := CryptoOperatorPlanUsdcCredit(ctx, intent.IntentId, fixture.signature)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, plan.State, CryptoUsdcCreditStateCredit)
		connect.AssertEqual(t, plan.Refusals, []string(nil))
		connect.AssertEqual(t, plan.Late, false)
		connect.AssertEqual(t, plan.UsdNanoCents, intent.PriceUsdNanoCents)
		connect.AssertEqual(t, plan.OverQuoteAtomic, int64(87_604))
		connect.AssertEqual(t, plan.Invoice.AmountAtomic, intent.AmountAtomic)
		connect.AssertEqual(t, *plan.Invoice.NetworkId, fixture.networkId)
		connect.AssertEqual(t, plan.Invoice.NetworkExists, true)
		connect.AssertEqual(t, plan.Payment.AmountAtomic, intent.AmountAtomic+87_604)
		connect.AssertEqual(t, plan.Payment.TransactionTime.Unix(), fixture.payment.Timestamp)
		connect.AssertEqual(t, len(plan.Digest), 64)
		cryptoUsdcUncredited(t, ctx, fixture)
		again, err := CryptoOperatorPlanUsdcCredit(ctx, intent.IntentId, fixture.signature)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, again.Digest, plan.Digest)

		// another digest is refused
		mismatch, err := CryptoOperatorCommitUsdcCredit(fixture.clientSession, intent.IntentId, fixture.signature, strings.Repeat("0", 64))
		connect.AssertEqual(t, errors.Is(err, ErrCryptoUsdcCreditDigestMismatch), true)
		connect.AssertEqual(t, mismatch.Digest, plan.Digest)
		cryptoUsdcUncredited(t, ctx, fixture)

		before := server.NowUtc()
		committed, err := CryptoOperatorCommitUsdcCredit(fixture.clientSession, intent.IntentId, fixture.signature, plan.Digest)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, committed.State, CryptoUsdcCreditStateCredited)
		connect.AssertEqual(t, committed.Digest, plan.Digest)
		cryptoUsdcCreditedOnce(t, ctx, fixture)

		// a year of Pro with the plan's balance, valued at the price; its data
		// is the year's monthly cycle grants
		pro := cryptoUsdcProBalances(ctx, fixture.networkId)[0]
		connect.AssertEqual(t, pro.StartBalanceByteCount, SupporterPeriodTransferBalance)
		connect.AssertEqual(t, pro.EndTime.Sub(pro.StartTime), solanaPlanDuration(model.SolanaPlanYearly)+manualPaymentGracePeriod)
		connect.AssertEqual(t, !pro.StartTime.Before(before.Add(-time.Second)), true)
		var subsidyRevenue model.NanoCents
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT subsidy_net_revenue_nano_cents FROM transfer_balance WHERE balance_id = $1`, pro.BalanceId).Scan(&subsidyRevenue))
		})
		connect.AssertEqual(t, subsidyRevenue, intent.PriceUsdNanoCents)
		renewal := cryptoDbRenewals(t, ctx, fixture.networkId, model.SubscriptionMarketSolana)
		connect.AssertEqual(t, renewal.transactionId, fixture.reference)
		connect.AssertEqual(t, renewal.netRevenue, intent.PriceUsdNanoCents)
		connect.AssertEqual(t, *renewal.priceTier, intent.Tier)
		credited := model.GetCryptoPaymentIntent(ctx, intent.IntentId)
		connect.AssertEqual(t, *credited.CreditedUsdNanoCents, intent.PriceUsdNanoCents)
		connect.AssertEqual(t, *credited.BalanceId, pro.BalanceId)
		// the buyer's checkout reads paid
		status, err := CryptoPaymentStatus(&CryptoPaymentStatusArgs{IntentId: intent.IntentId.String(), StatusToken: fixture.statusToken}, fixture.clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, status.Status, CryptoPaymentStatusPaid)

		// Helius redelivers: the signature is credited, so it is skipped and
		// not recorded again
		webhook, err := HeliusWebhook([]*SolanaTransaction{fixture.payment}, session.Testing_CreateClientSession(ctx, nil))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, webhook.Message, "No payment intent found for this network ID")
		// a reconcile run
		result := cryptoUsdcReconcileSolana(t, ctx)
		connect.AssertEqual(t, cryptoUsdcReconcileCredited(ctx, result), 0)
		cryptoUsdcCreditedOnce(t, ctx, fixture)

		// the same commit again is refused
		rerun, err := CryptoOperatorCommitUsdcCredit(fixture.clientSession, intent.IntentId, fixture.signature, plan.Digest)
		connect.AssertEqual(t, errors.Is(err, ErrCryptoUsdcCreditRefused), true)
		connect.AssertEqual(t, cryptoUsdcHasRefusal(rerun, "already credited"), true)
		connect.AssertEqual(t, cryptoUsdcHasRefusal(rerun, ", by this payment"), true)
		connect.AssertEqual(t, cryptoUsdcHasRefusal(rerun, "the signature is already credited: it paid a Solana Pay intent"), true)
		connect.AssertEqual(t, cryptoUsdcHasRefusal(rerun, "no unfulfilled Solana payment is recorded"), true)
		cryptoUsdcCreditedOnce(t, ctx, fixture)
	})
}

// Every refusal writes nothing: the invoice stays open, the record stays,
// and the network gets no Pro.
func TestCryptoOperatorUsdcCreditRefusals(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cryptoTestEnable(t, cryptoTestConfig(), map[string]*fakeCryptoWallet{})

		refused := func(fixture *cryptoUsdcMemoless, intentId server.Id, signature string, refusal string) {
			t.Helper()
			plan, err := CryptoOperatorPlanUsdcCredit(ctx, intentId, signature)
			connect.AssertEqual(t, err, nil)
			if plan.State != CryptoUsdcCreditStateRefused || plan.Digest != "" || !cryptoUsdcHasRefusal(plan, refusal) {
				t.Fatalf("state %s, digest %q, refusals %q: want refused with %q", plan.State, plan.Digest, plan.Refusals, refusal)
			}
			committed, err := CryptoOperatorCommitUsdcCredit(fixture.clientSession, intentId, signature, strings.Repeat("a", 64))
			connect.AssertEqual(t, errors.Is(err, ErrCryptoUsdcCreditRefused), true)
			connect.AssertEqual(t, committed.State, CryptoUsdcCreditStateRefused)
		}

		// the invoice is missing
		fixture := newCryptoUsdcMemoless(t, ctx, "usdcrefusemissing", 6001, 87_604)
		fixture.send(t, ctx)
		refused(fixture, server.NewId(), fixture.signature, "no invoice has id")
		cryptoUsdcUncredited(t, ctx, fixture)

		// the invoice is not usdc_solana
		xmr := cryptoDbCreateIntent(t, ctx, model.CryptoAssetXmr, &fixture.networkId, model.SolanaPlanYearly, 40, 7001)
		refused(fixture, xmr.IntentId, fixture.signature, "the invoice is xmr, not usdc_solana")
		cryptoUsdcUncredited(t, ctx, fixture)
		connect.AssertEqual(t, model.GetCryptoPaymentIntent(ctx, xmr.IntentId).CreditedAt, (*time.Time)(nil))

		// the invoice is credited: the buyer paid the exact quote afterwards,
		// which the webhook matched by its unique amount
		fixture = newCryptoUsdcMemoless(t, ctx, "usdcrefusecredited", 6002, 87_604)
		fixture.send(t, ctx)
		exact := solanaMemolessPayment("sig-usdc-credit-exact", solanaReceiverAddresses[0], model.SolanaMicroToUsd(fixture.intent.AmountAtomic), fixture.intent.QuotedAt.Add(2*time.Minute))
		webhook, err := HeliusWebhook([]*SolanaTransaction{exact}, session.Testing_CreateClientSession(ctx, nil))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, webhook.Message, "Processed 1 matching payments")
		refused(fixture, fixture.intent.IntentId, fixture.signature, "the invoice is already credited")
		connect.AssertEqual(t, cryptoDbRenewals(t, ctx, fixture.networkId, model.SubscriptionMarketSolana).count, 1)
		_, err = model.GetUnfulfilledSolanaPayment(ctx, fixture.signature)
		connect.AssertEqual(t, err, nil)

		// the invoice is void
		fixture = newCryptoUsdcMemoless(t, ctx, "usdcrefusevoid", 6003, 87_604)
		fixture.send(t, ctx)
		voided, err := CryptoOperatorVoid(ctx, fixture.intent.IntentId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, voided, true)
		refused(fixture, fixture.intent.IntentId, fixture.signature, "the invoice is void")
		cryptoUsdcUncredited(t, ctx, fixture)

		// the invoice is unlinked from its network
		fixture = newCryptoUsdcMemoless(t, ctx, "usdcrefuseunlinked", 6004, 87_604)
		fixture.send(t, ctx)
		cryptoDbExec(ctx, `UPDATE crypto_payment_intent SET network_id = NULL, unlinked_at = $2 WHERE intent_id = $1`, fixture.intent.IntentId, server.NowUtc())
		refused(fixture, fixture.intent.IntentId, fixture.signature, "the invoice is unlinked from its network")
		cryptoUsdcUncredited(t, ctx, fixture)

		// the payment is not recorded
		fixture = newCryptoUsdcMemoless(t, ctx, "usdcrefusenorecord", 6005, 87_604)
		refused(fixture, fixture.intent.IntentId, fixture.signature, "no unfulfilled Solana payment is recorded with signature "+fixture.signature)
		connect.AssertEqual(t, model.GetCryptoPaymentIntent(ctx, fixture.intent.IntentId).CreditedAt, (*time.Time)(nil))

		// the payment is recorded underpaid: it carried the reference
		fixture = newCryptoUsdcMemoless(t, ctx, "usdcrefuseunderpaid", 6006, 0)
		underpaid := solanaTestPayment(fixture.reference, fixture.signature, fixture.amountUsd-1)
		webhook, err = HeliusWebhook([]*SolanaTransaction{underpaid}, session.Testing_CreateClientSession(ctx, nil))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, webhook.Message, "Payment is less than the quoted price")
		refused(fixture, fixture.intent.IntentId, fixture.signature, "recorded as underpaid, not no_intent")
		cryptoUsdcUncredited(t, ctx, fixture)

		// the payment is under the quote: the plain price, without the suffix
		fixture = newCryptoUsdcMemoless(t, ctx, "usdcrefuseunder", 6007, 0)
		fixture.payment = solanaMemolessPayment(fixture.signature, solanaReceiverAddresses[0], model.SolanaMicroToUsd(fixture.intent.AmountAtomic-6007), fixture.intent.QuotedAt.Add(time.Minute))
		fixture.send(t, ctx)
		refused(fixture, fixture.intent.IntentId, fixture.signature, "is under the quote of")
		cryptoUsdcUncredited(t, ctx, fixture)

		// the payment is tied to a different network: by its record's network,
		// and by its match note
		otherNetworkId, _, _ := cryptoDbTestNetwork(t, ctx, "usdcrefuseother")
		fixture = newCryptoUsdcMemoless(t, ctx, "usdcrefusetied", 6008, 87_604)
		err = model.RecordUnfulfilledSolanaPayment(ctx, &model.UnfulfilledSolanaPayment{
			TxSignature:    fixture.signature,
			Reason:         model.SolanaUnfulfilledReasonNoIntent,
			TokenAmountUsd: fixture.amountUsd,
			NetworkId:      &otherNetworkId,
		})
		connect.AssertEqual(t, err, nil)
		refused(fixture, fixture.intent.IntentId, fixture.signature, "ties it to network "+otherNetworkId.String()+", not the invoice's network")
		cryptoUsdcUncredited(t, ctx, fixture)
		noteSignature := fixture.signature + "-note"
		note := "memo-less: intent OtherReference (network " + otherNetworkId.String() + ", plan yearly, quoted 40.093109 USDC) was already paid by sig-earlier"
		err = model.RecordUnfulfilledSolanaPayment(ctx, &model.UnfulfilledSolanaPayment{
			TxSignature:    noteSignature,
			Reason:         model.SolanaUnfulfilledReasonNoIntent,
			TokenAmountUsd: fixture.amountUsd,
			MatchNote:      &note,
		})
		connect.AssertEqual(t, err, nil)
		refused(fixture, fixture.intent.IntentId, noteSignature, "ties it to network "+otherNetworkId.String()+", not the invoice's network")

		// the signature is credited elsewhere: it paid another Solana Pay
		// intent, a renewal names it, a balance was bought with it
		fixture = newCryptoUsdcMemoless(t, ctx, "usdcrefusesignature", 6009, 87_604)
		fixture.send(t, ctx)
		otherSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: otherNetworkId, UserId: server.NewId()})
		connect.AssertEqual(t, model.CreateSolanaPaymentIntent("usdc-credit-other-reference", 5, model.SolanaPlanMonthly, otherSession), nil)
		completed, err := model.MarkPaymentIntentCompleted("usdc-credit-other-reference", fixture.signature, otherSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, completed, true)
		refused(fixture, fixture.intent.IntentId, fixture.signature, "the signature is already credited: it paid a Solana Pay intent")
		connect.AssertEqual(t, model.GetCryptoPaymentIntent(ctx, fixture.intent.IntentId).CreditedAt, (*time.Time)(nil))
		connect.AssertEqual(t, model.IsProNetwork(ctx, fixture.networkId), false)

		renewalSignature := fixture.signature + "-renewal"
		connect.AssertEqual(t, model.RecordUnfulfilledSolanaPayment(ctx, &model.UnfulfilledSolanaPayment{
			TxSignature: renewalSignature, Reason: model.SolanaUnfulfilledReasonNoIntent, TokenAmountUsd: fixture.amountUsd,
		}), nil)
		connect.AssertEqual(t, model.AddSubscriptionRenewal(ctx, &model.SubscriptionRenewal{
			NetworkId:          otherNetworkId,
			SubscriptionType:   model.SubscriptionTypeSupporter,
			StartTime:          server.NowUtc(),
			EndTime:            server.NowUtc().Add(30 * 24 * time.Hour),
			NetRevenue:         model.UsdToNanoCents(5),
			SubscriptionMarket: model.SubscriptionMarketX402,
			TransactionId:      renewalSignature,
		}), nil)
		refused(fixture, fixture.intent.IntentId, renewalSignature, "the signature is already credited: 1 solana or x402 renewals name it")

		balanceSignature := fixture.signature + "-balance"
		connect.AssertEqual(t, model.RecordUnfulfilledSolanaPayment(ctx, &model.UnfulfilledSolanaPayment{
			TxSignature: balanceSignature, Reason: model.SolanaUnfulfilledReasonNoIntent, TokenAmountUsd: fixture.amountUsd,
		}), nil)
		model.AddTransferBalance(ctx, &model.TransferBalance{
			NetworkId:             otherNetworkId,
			StartTime:             server.NowUtc(),
			EndTime:               server.NowUtc().Add(30 * 24 * time.Hour),
			StartBalanceByteCount: model.Gib,
			BalanceByteCount:      model.Gib,
			NetRevenue:            model.UsdToNanoCents(5),
			PurchaseToken:         balanceSignature,
		})
		refused(fixture, fixture.intent.IntentId, balanceSignature, "the signature is already credited: 1 balances were bought with it")
		connect.AssertEqual(t, model.GetCryptoPaymentIntent(ctx, fixture.intent.IntentId).CreditedAt, (*time.Time)(nil))
		connect.AssertEqual(t, model.IsProNetwork(ctx, fixture.networkId), false)
	})
}

// A Helius redelivery of the payment or a reconcile run racing the commit,
// and two commits racing each other, credit the invoice exactly once. A
// record a racing redelivery wrote again after the credit is cleared by the
// next reconcile run, as the reconciler clears any record of a credited
// signature.
func TestCryptoOperatorUsdcCreditRacesCreditOnce(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		solanaEnv := newSolanaReconcileTestEnv(t)
		cryptoTestEnable(t, cryptoTestConfig(), map[string]*fakeCryptoWallet{})

		race := func(name string, suffixMicro int64, other func(fixture *cryptoUsdcMemoless, digest string) error) {
			fixture := newCryptoUsdcMemoless(t, ctx, name, suffixMicro, 87_604)
			solanaEnv.onChain[fixture.signature] = true
			fixture.send(t, ctx)
			plan, err := CryptoOperatorPlanUsdcCredit(ctx, fixture.intent.IntentId, fixture.signature)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, plan.State, CryptoUsdcCreditStateCredit)

			start := make(chan struct{})
			var wait sync.WaitGroup
			errs := make([]error, 2)
			wait.Add(2)
			go func() {
				defer wait.Done()
				<-start
				_, errs[0] = CryptoOperatorCommitUsdcCredit(fixture.clientSession, fixture.intent.IntentId, fixture.signature, plan.Digest)
			}()
			go func() {
				defer wait.Done()
				<-start
				errs[1] = other(fixture, plan.Digest)
			}()
			close(start)
			// both finish before any assertion
			wait.Wait()
			if errs[0] != nil && errs[1] != nil {
				t.Fatalf("%s: neither side succeeded: %v; %v", name, errs[0], errs[1])
			}

			// whichever order, one credit; the next reconcile run clears a
			// record the race wrote again
			cryptoUsdcReconcileSolana(t, ctx)
			cryptoUsdcCreditedOnce(t, ctx, fixture)
		}

		// a Helius redelivery of the same payment
		race("usdcraceredelivery", 7101, func(fixture *cryptoUsdcMemoless, digest string) error {
			_, err := HeliusWebhook([]*SolanaTransaction{fixture.payment}, session.Testing_CreateClientSession(ctx, nil))
			return err
		})
		// a reconcile run
		race("usdcracereconcile", 7102, func(fixture *cryptoUsdcMemoless, digest string) error {
			_, err := RunPaymentReconciliationWithOptions(
				reconcileTestSession(t, ctx),
				&PaymentReconcileRunOptions{Stores: []string{model.SubscriptionMarketSolana}},
			)
			return err
		})
		// the same commit twice at once: one credits, the other is refused or
		// loses the invoice one-shot
		race("usdcracecommit", 7103, func(fixture *cryptoUsdcMemoless, digest string) error {
			_, err := CryptoOperatorCommitUsdcCredit(fixture.clientSession, fixture.intent.IntentId, fixture.signature, digest)
			if err != nil && !errors.Is(err, ErrCryptoUsdcCreditNotCredited) && !errors.Is(err, ErrCryptoUsdcCreditRefused) {
				t.Errorf("the second commit failed: %v", err)
			}
			return err
		})
	})
}

// The digest binds the quote: the buyer reopening the checkout between the
// dry run and the commit moves the expiry, and the old digest is refused
// with nothing written. A new dry run's digest credits.
func TestCryptoOperatorUsdcCreditDigestMismatchRefuses(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cryptoTestEnable(t, cryptoTestConfig(), map[string]*fakeCryptoWallet{})
		fixture := newCryptoUsdcMemoless(t, ctx, "usdcdigestmismatch", 7201, 87_604)
		fixture.send(t, ctx)
		plan, err := CryptoOperatorPlanUsdcCredit(ctx, fixture.intent.IntentId, fixture.signature)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, plan.State, CryptoUsdcCreditStateCredit)

		now := server.NowUtc()
		connect.AssertEqual(t, model.ExtendSolanaCryptoPaymentIntent(ctx, fixture.intent.IntentId, fixture.reference, now, now.Add(2*time.Hour)), true)
		stale, err := CryptoOperatorCommitUsdcCredit(fixture.clientSession, fixture.intent.IntentId, fixture.signature, plan.Digest)
		connect.AssertEqual(t, errors.Is(err, ErrCryptoUsdcCreditDigestMismatch), true)
		connect.AssertNotEqual(t, stale.Digest, plan.Digest)
		cryptoUsdcUncredited(t, ctx, fixture)

		replanned, err := CryptoOperatorPlanUsdcCredit(ctx, fixture.intent.IntentId, fixture.signature)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, replanned.Digest, stale.Digest)
		_, err = CryptoOperatorCommitUsdcCredit(fixture.clientSession, fixture.intent.IntentId, fixture.signature, replanned.Digest)
		connect.AssertEqual(t, err, nil)
		cryptoUsdcCreditedOnce(t, ctx, fixture)
	})
}

// The credit and the record's removal commit together: a credit that
// resolves a record no longer there is rolled back whole, with nothing
// written, and the same credit with its record commits both.
func TestCryptoUsdcCreditRollsBackWithoutItsRecord(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cryptoTestEnable(t, cryptoTestConfig(), map[string]*fakeCryptoWallet{})
		fixture := newCryptoUsdcMemoless(t, ctx, "usdcrollback", 7301, 87_604)
		credit := func() *cryptoCredit {
			return &cryptoCredit{
				usdNanoCents:        fixture.intent.PriceUsdNanoCents,
				solanaSignature:     fixture.signature,
				resolvesUnfulfilled: true,
			}
		}

		// never recorded
		credited, err := cryptoCreditIntentInConn(nil, fixture.clientSession, fixture.intent, credit())
		connect.AssertEqual(t, errors.Is(err, errCryptoUnfulfilledPaymentChanged), true)
		connect.AssertEqual(t, credited, false)
		connect.AssertEqual(t, model.GetCryptoPaymentIntent(ctx, fixture.intent.IntentId).CreditedAt, (*time.Time)(nil))
		connect.AssertEqual(t, model.GetSolanaPaymentIntent(ctx, fixture.reference).TxSignature, (*string)(nil))
		connect.AssertEqual(t, model.IsProNetwork(ctx, fixture.networkId), false)
		connect.AssertEqual(t, cryptoDbRenewals(t, ctx, fixture.networkId, model.SubscriptionMarketSolana).count, 0)

		// recorded underpaid, not no_intent
		connect.AssertEqual(t, model.RecordUnfulfilledSolanaPayment(ctx, &model.UnfulfilledSolanaPayment{
			TxSignature: fixture.signature, Reason: model.SolanaUnfulfilledReasonUnderpaid, TokenAmountUsd: fixture.amountUsd,
		}), nil)
		credited, err = cryptoCreditIntentInConn(nil, fixture.clientSession, fixture.intent, credit())
		connect.AssertEqual(t, errors.Is(err, errCryptoUnfulfilledPaymentChanged), true)
		connect.AssertEqual(t, credited, false)
		connect.AssertEqual(t, model.GetCryptoPaymentIntent(ctx, fixture.intent.IntentId).CreditedAt, (*time.Time)(nil))
		recorded, err := model.GetUnfulfilledSolanaPayment(ctx, fixture.signature)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, recorded.Reason, model.SolanaUnfulfilledReasonUnderpaid)

		// recorded no_intent: both commit
		cryptoDbExec(ctx, `DELETE FROM solana_unfulfilled_payment WHERE tx_signature = $1`, fixture.signature)
		fixture.send(t, ctx)
		credited, err = cryptoCreditIntentInConn(nil, fixture.clientSession, fixture.intent, credit())
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, credited, true)
		cryptoUsdcCreditedOnce(t, ctx, fixture)
	})
}
