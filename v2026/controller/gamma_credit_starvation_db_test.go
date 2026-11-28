package controller

// Database tests of the biller's credit retries (mmm/BTCPAY.md §18.4). Each
// run credited the oldest 200 spends not yet credited, with no regard to how
// often each had failed: 200 credits that fail every run (a spend that landed
// for a network that is gone fails for good) kept every newer credit out, so a
// customer whose gamma was spent never got what they paid for. Now the credits
// that failed least go first, and those held for review (gammaMaxCreditFailures
// failures or more) are retried a few at a time, the least retried first.
// Against the test database and the simulated chain. Needs pro.yml.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// gammaStarveTestFailing records count spends that landed for networks that
// do not exist, each with failures failed credits behind it, the oldest at
// from and one a second apart: their credits fail every run. It returns the
// intent ids, oldest first.
func gammaStarveTestFailing(t testing.TB, ctx context.Context, tag string, count int, failures int, from time.Time) []server.Id {
	ids := []server.Id{}
	server.Tx(ctx, func(tx server.PgTx) {
		for i := range count {
			intentId := server.NewId()
			reference := append(append([]byte{}, intentId.Bytes()...), server.NewId().Bytes()...)
			createTime := from.Add(time.Duration(i) * time.Second)
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO gamma_payment_intent (
						intent_id, status_token_hash, network_id, hotkey, purpose,
						reference, delivery, item, price_usd_nano_cents,
						regular_price_usd_nano_cents, create_time, expires_at,
						credit_failure_count
					)
					VALUES ($1, $2, $3, $4, 'purchase', $5, 'account', $6, $7, $7, $8, $9, $10)
				`,
				intentId,
				intentId.Bytes(),
				server.NewId(),
				tag,
				reference,
				StripeItemData1Tib,
				model.UsdToNanoCents(3),
				createTime,
				createTime.Add(24*time.Hour),
				failures,
			))
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO gamma_charge (
						reference, intent_id, hotkey, amount, state, create_time,
						resolved_at, block_number, block_hash, event_index
					)
					VALUES ($1, $2, $3, $4, 'spent', $5, $5, 1, '0x01', 0)
				`,
				reference,
				intentId,
				tag,
				model.UsdToNanoCents(3),
				createTime,
			))
			ids = append(ids, intentId)
		}
	})
	return ids
}

func gammaStarveTestFailures(t testing.TB, ctx context.Context, tag string) int {
	return gammaDbCount(t, ctx, `SELECT COALESCE(SUM(credit_failure_count), 0)::int FROM gamma_payment_intent WHERE hotkey = $1`, tag)
}

// More than a run's worth of spends whose credit fails every run, all older
// than a customer's purchase: the purchase is credited in the run that sees
// its spend.
func TestGammaNewCreditIsNotStarvedByCreditsThatKeepFailing(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		f.run()
		gammaStarveTestFailing(t, ctx, "gamma-starve-failing", 201, 3, f.now.Add(-time.Hour))

		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammastarvednew")
		intent := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{ItemId: StripeItemData1Tib})
		connect.AssertEqual(t, intent.Error == nil, true)
		gammaReplaceTestDeposit(t, f, gammaTestAccount(t, intent.DepositHotkey), 10_000_000_000)
		f.run()
		connect.AssertEqual(t, len(f.sim.submitted), 1)
		f.now = f.now.Add(time.Minute)
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, gammaDbStatus(t, clientSession, intent).Status, gammaStatusPaid)
		connect.AssertEqual(t, model.GetActiveTransferBalanceByteCount(ctx, networkId), model.Tib)
	})
}

// Credits held for review (failed 10 times or more) are retried a few at a
// time, the least retried first, so they never crowd out a new credit and
// still recover once their cause is fixed: here the network a spend was for
// comes back.
func TestGammaCreditsHeldForReviewAreRetriedAFewAtATime(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		f.run()
		const tag = "gamma-starve-review"
		gammaStarveTestFailing(t, ctx, tag, 201, 15, f.now.Add(-time.Hour))
		// the least retried of them, and the newest: its network comes back
		recovering := gammaStarveTestFailing(t, ctx, "gamma-starve-recovering", 1, 10, f.now.Add(-time.Minute))[0]
		recoveringNetworkId := model.GetGammaPaymentIntent(ctx, recovering).NetworkId
		model.Testing_CreateNetwork(ctx, recoveringNetworkId, "gammastarverecovers", server.NewId())

		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammastarvedreview")
		intent := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{ItemId: StripeItemData1Tib})
		connect.AssertEqual(t, intent.Error == nil, true)
		gammaReplaceTestDeposit(t, f, gammaTestAccount(t, intent.DepositHotkey), 10_000_000_000)
		// a few of them are retried each run, at most ten, not all
		retriedInRun := func(run func()) int {
			before := gammaStarveTestFailures(t, ctx, tag)
			run()
			retried := gammaStarveTestFailures(t, ctx, tag) - before
			if retried <= 0 || 10 < retried {
				t.Fatalf("%d credits held for review were retried in one run; want a few", retried)
			}
			return retried
		}
		retriedInRun(func() { f.run() })
		// the least retried went first: its network is back, and it is credited
		connect.AssertEqual(t, model.GetGammaPaymentIntent(ctx, recovering).CreditedAt != nil, true)
		connect.AssertEqual(t, model.GetActiveTransferBalanceByteCount(ctx, recoveringNetworkId), model.Tib)
		connect.AssertEqual(t, len(f.sim.submitted), 1)

		f.now = f.now.Add(time.Minute)
		f.sim.mine()
		retriedInRun(func() { f.run() })
		connect.AssertEqual(t, gammaDbStatus(t, clientSession, intent).Status, gammaStatusPaid)
		connect.AssertEqual(t, model.GetActiveTransferBalanceByteCount(ctx, networkId), model.Tib)
	})
}
