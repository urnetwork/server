package model

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/session"
	"maps"
)

func TestCancelAccountPayment(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		netTransferByteCount := ByteCount(1024 * 1024 * 1024 * 1024)
		netRevenue := UsdToNanoCents(10.00)

		sourceNetworkId := server.NewId()
		sourceId := server.NewId()
		destinationNetworkId := server.NewId()
		destinationId := server.NewId()

		sourceSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: sourceNetworkId,
			ClientId:  &sourceId,
		})
		destinationSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: destinationNetworkId,
			ClientId:  &destinationId,
		})

		subscriptionYearDuration := 365 * 24 * time.Hour

		balanceCode, err := CreateBalanceCode(
			ctx,
			netTransferByteCount,
			subscriptionYearDuration,
			netRevenue,
			"",
			"",
			"",
		)

		connect.AssertEqual(t, err, nil)
		RedeemBalanceCode(&RedeemBalanceCodeArgs{
			Secret:    balanceCode.Secret,
			NetworkId: sourceNetworkId,
		}, sourceSession.Ctx)

		transferEscrow, err := CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, 1024*1024)
		connect.AssertEqual(t, err, nil)

		usedTransferByteCount := ByteCount(1024)
		paidByteCount := usedTransferByteCount

		CloseContract(ctx, transferEscrow.ContractId, sourceId, usedTransferByteCount, false)
		CloseContract(ctx, transferEscrow.ContractId, destinationId, usedTransferByteCount, false)

		paid := UsdToNanoCents(ProviderRevenueShare * NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))

		destinationWalletAddress := "0x1234567890"

		args := &CreateAccountWalletExternalArgs{
			NetworkId:        destinationNetworkId,
			Blockchain:       "MATIC",
			WalletAddress:    destinationWalletAddress,
			DefaultTokenType: "USDC",
		}
		walletId := CreateAccountWalletExternal(destinationSession, args)
		connect.AssertNotEqual(t, walletId, nil)

		wallet := GetAccountWallet(ctx, *walletId)

		err = SetPayoutWallet(ctx, destinationNetworkId, wallet.WalletId)
		connect.AssertEqual(t, err, nil)

		paymentPlan, err := PlanPayments(ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(paymentPlan.NetworkPayments), 0)
		connect.AssertEqual(t, paymentPlan.WithheldNetworkIds, []server.Id{destinationNetworkId})

		usedTransferByteCount = ByteCount(1024 * 1024 * 1024)

		for paid < 2*UsdToNanoCents(EnvSubsidyConfig().MinWalletPayoutUsd) {
			transferEscrow, err := CreateTransferEscrow(
				ctx,
				sourceNetworkId,
				sourceId,
				destinationNetworkId,
				destinationId,
				usedTransferByteCount,
			)
			connect.AssertEqual(t, err, nil)

			err = CloseContract(ctx, transferEscrow.ContractId, sourceId, usedTransferByteCount, false)
			connect.AssertEqual(t, err, nil)
			err = CloseContract(ctx, transferEscrow.ContractId, destinationId, usedTransferByteCount, false)
			connect.AssertEqual(t, err, nil)
			paidByteCount += usedTransferByteCount
			paid += UsdToNanoCents(ProviderRevenueShare * NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))
		}

		contractIds := GetOpenContractIds(ctx, sourceId, destinationId)
		connect.AssertEqual(t, len(contractIds), 0)

		paymentPlan, err = PlanPayments(ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, slices.Collect(maps.Keys(paymentPlan.NetworkPayments)), []server.Id{destinationNetworkId})

		for _, payment := range paymentPlan.NetworkPayments {

			connect.AssertEqual(t, payment.Canceled, false)

			CancelPayment(ctx, payment.PaymentId)

			payment, err := GetPayment(ctx, payment.PaymentId)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, payment.Canceled, true)
			connect.AssertEqual(t, payment.Blockchain, "MATIC")
			connect.AssertNotEqual(t, payment.CancelTime, nil)
		}

	})
}

// A bounded plan (maxDuration > 0) anchors on the most recent subsidy epoch end
// and includes only contracts closing before anchor+maxDuration, leaving later
// contracts for a later plan. Because each plan records a new subsidy epoch that
// advances the anchor, running bounded plans repeatedly drains a backlog forward
// one slice per run, and together they pay everything an unbounded plan would
// have paid at once.
func TestPlanPaymentsMaxDuration(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		netTransferByteCount := ByteCount(1024 * 1024 * 1024 * 1024) // 1 TiB
		netRevenue := UsdToNanoCents(100.00)

		sourceNetworkId := server.NewId()
		sourceId := server.NewId()
		destinationNetworkId := server.NewId()
		destinationId := server.NewId()

		sourceSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: sourceNetworkId,
			ClientId:  &sourceId,
		})
		destinationSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: destinationNetworkId,
			ClientId:  &destinationId,
		})

		balanceCode, err := CreateBalanceCode(ctx, netTransferByteCount, 365*24*time.Hour, netRevenue, "", "", "")
		connect.AssertEqual(t, err, nil)
		RedeemBalanceCode(&RedeemBalanceCodeArgs{
			Secret:    balanceCode.Secret,
			NetworkId: sourceNetworkId,
		}, sourceSession.Ctx)

		walletId := CreateAccountWalletExternal(destinationSession, &CreateAccountWalletExternalArgs{
			NetworkId:        destinationNetworkId,
			Blockchain:       "MATIC",
			WalletAddress:    "0x1234567890",
			DefaultTokenType: "USDC",
		})
		connect.AssertNotEqual(t, walletId, nil)
		err = SetPayoutWallet(ctx, destinationNetworkId, *walletId)
		connect.AssertEqual(t, err, nil)

		// close a cohort of contracts whose total provider revenue share clears
		// the wallet minimum, so the cohort is never withheld for being small.
		usedTransferByteCount := ByteCount(50 * 1024 * 1024 * 1024) // 50 GiB
		closeCohort := func() {
			paid := NanoCents(0)
			for paid < UsdToNanoCents(EnvSubsidyConfig().MinWalletPayoutUsd) {
				transferEscrow, err := CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, usedTransferByteCount)
				connect.AssertEqual(t, err, nil)
				err = CloseContract(ctx, transferEscrow.ContractId, sourceId, usedTransferByteCount, false)
				connect.AssertEqual(t, err, nil)
				err = CloseContract(ctx, transferEscrow.ContractId, destinationId, usedTransferByteCount, false)
				connect.AssertEqual(t, err, nil)
				paid += UsdToNanoCents(ProviderRevenueShare * NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))
			}
		}

		// backdate the currently-unpaid cohort's contract create/close time into a
		// chosen historical window. The bound is on close_time, so this is what
		// places a cohort inside or outside a plan's slice; create_time is set
		// behind close_time so the cohort still spans enough time to form a
		// subsidy epoch. When onlyClosedAfter is set, only contracts still closing
		// at/after it are moved, which targets just the freshly-created cohort and
		// leaves earlier, already-backdated cohorts in place.
		moveUnpaidContracts := func(createTime, closeTime time.Time, onlyClosedAfter *time.Time) {
			server.Tx(ctx, func(tx server.PgTx) {
				if onlyClosedAfter == nil {
					server.RaisePgResult(tx.Exec(ctx,
						`UPDATE transfer_contract SET create_time = $1, close_time = $2
						 WHERE contract_id IN (
						     SELECT contract_id FROM transfer_escrow_sweep WHERE payment_id IS NULL
						 )`,
						createTime, closeTime,
					))
				} else {
					server.RaisePgResult(tx.Exec(ctx,
						`UPDATE transfer_contract SET create_time = $1, close_time = $2
						 WHERE contract_id IN (
						     SELECT contract_id FROM transfer_escrow_sweep WHERE payment_id IS NULL
						 ) AND close_time >= $3`,
						createTime, closeTime, *onlyClosedAfter,
					))
				}
			})
		}

		// seed a prior subsidy epoch [startTime, endTime]. A bounded plan anchors
		// on the most recent subsidy end, so this is the frontier the drain
		// continues from (a fresh deployment with no epoch plans unbounded).
		seedSubsidyEpoch := func(startTime, endTime time.Time) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx,
					`INSERT INTO subsidy_payment (
						payment_plan_id, start_time, end_time,
						active_user_count, paid_user_count,
						net_payout_byte_count_paid, net_payout_byte_count_unpaid,
						net_revenue_nano_cents, net_payout_nano_cents
					) VALUES ($1, $2, $3, 0, 0, 0, 0, 0, 0)`,
					server.NewId(), startTime, endTime,
				))
			})
		}

		sumUnpaidBytes := func() ByteCount {
			var total ByteCount
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(ctx,
					`SELECT COALESCE(SUM(payout_byte_count), 0)::bigint FROM transfer_escrow_sweep WHERE payment_id IS NULL`,
				)
				server.WithPgResult(result, err, func() {
					if result.Next() {
						server.Raise(result.Scan(&total))
					}
				})
			})
			return total
		}

		now := server.NowUtc()
		maxDuration := 20 * 24 * time.Hour
		// the frontier the drain continues from: the last payout covered up to here
		subsidyEnd := now.Add(-60 * 24 * time.Hour)
		seedSubsidyEpoch(subsidyEnd.Add(-5*24*time.Hour), subsidyEnd)

		// cohort A closes inside the first slice [subsidyEnd, subsidyEnd+maxDuration
		// = now-40d): closed now-50d, created now-58d.
		closeCohort()
		moveUnpaidContracts(now.Add(-58*24*time.Hour), now.Add(-50*24*time.Hour), nil)
		expectedBytesA := sumUnpaidBytes()
		connect.AssertEqual(t, 0 < expectedBytesA, true)

		// cohort B closes after the first slice (now-38d), so it is deferred to a
		// later plan. Only the freshly-created, not-yet-backdated contracts move.
		closeCohort()
		recentThreshold := now.Add(-24 * time.Hour)
		moveUnpaidContracts(now.Add(-45*24*time.Hour), now.Add(-38*24*time.Hour), &recentThreshold)
		expectedBytesB := sumUnpaidBytes() - expectedBytesA
		connect.AssertEqual(t, 0 < expectedBytesB, true)

		// baseline: an unbounded plan would pay both cohorts at once. Use a dry
		// run so it persists nothing and the real bounded plans below still see
		// the full backlog.
		dryAll, err := CreatePaymentPlan(ctx, EnvSubsidyConfig(), true, 0)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, dryAll.NetworkPayments[destinationNetworkId].PayoutByteCount, expectedBytesA+expectedBytesB)

		// plan 1: window is close_time < subsidyEnd+maxDuration = now-40d; includes
		// cohort A (closed now-50d), excludes cohort B (closed now-38d).
		plan1, err := CreatePaymentPlan(ctx, EnvSubsidyConfig(), false, maxDuration)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(plan1.NetworkPayments), 1)
		payment1, ok := plan1.NetworkPayments[destinationNetworkId]
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, payment1.PayoutByteCount, expectedBytesA)
		// cohort B is untouched, still unpaid for a later plan
		connect.AssertEqual(t, sumUnpaidBytes(), expectedBytesB)

		// plan 1 recorded a new subsidy epoch ending at cohort A's close (now-50d),
		// advancing the frontier. plan 2 anchors there: window close_time < now-30d
		// now reaches cohort B and drains it.
		plan2, err := CreatePaymentPlan(ctx, EnvSubsidyConfig(), false, maxDuration)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(plan2.NetworkPayments), 1)
		payment2, ok := plan2.NetworkPayments[destinationNetworkId]
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, payment2.PayoutByteCount, expectedBytesB)
		// everything is now paid
		connect.AssertEqual(t, sumUnpaidBytes(), ByteCount(0))

		// plan 3: nothing left to pay
		plan3, err := CreatePaymentPlan(ctx, EnvSubsidyConfig(), false, maxDuration)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(plan3.NetworkPayments), 0)
	})
}

// PlanPaymentsWithMaxDurationLoop must drain the whole backlog in a single call:
// the same two-cohort backlog that TestPlanPaymentsMaxDuration drains with three
// manual bounded plans should be fully paid after one loop call, across separate
// committed slices whose frontier advances forward.
func TestPlanPaymentsMaxDurationLoop(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		netTransferByteCount := ByteCount(1024 * 1024 * 1024 * 1024) // 1 TiB
		netRevenue := UsdToNanoCents(100.00)

		sourceNetworkId := server.NewId()
		sourceId := server.NewId()
		destinationNetworkId := server.NewId()
		destinationId := server.NewId()

		sourceSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: sourceNetworkId,
			ClientId:  &sourceId,
		})
		destinationSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: destinationNetworkId,
			ClientId:  &destinationId,
		})

		balanceCode, err := CreateBalanceCode(ctx, netTransferByteCount, 365*24*time.Hour, netRevenue, "", "", "")
		connect.AssertEqual(t, err, nil)
		RedeemBalanceCode(&RedeemBalanceCodeArgs{
			Secret:    balanceCode.Secret,
			NetworkId: sourceNetworkId,
		}, sourceSession.Ctx)

		walletId := CreateAccountWalletExternal(destinationSession, &CreateAccountWalletExternalArgs{
			NetworkId:        destinationNetworkId,
			Blockchain:       "MATIC",
			WalletAddress:    "0x1234567890",
			DefaultTokenType: "USDC",
		})
		connect.AssertNotEqual(t, walletId, nil)
		err = SetPayoutWallet(ctx, destinationNetworkId, *walletId)
		connect.AssertEqual(t, err, nil)

		usedTransferByteCount := ByteCount(50 * 1024 * 1024 * 1024) // 50 GiB
		closeCohort := func() {
			paid := NanoCents(0)
			for paid < UsdToNanoCents(EnvSubsidyConfig().MinWalletPayoutUsd) {
				transferEscrow, err := CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, usedTransferByteCount)
				connect.AssertEqual(t, err, nil)
				err = CloseContract(ctx, transferEscrow.ContractId, sourceId, usedTransferByteCount, false)
				connect.AssertEqual(t, err, nil)
				err = CloseContract(ctx, transferEscrow.ContractId, destinationId, usedTransferByteCount, false)
				connect.AssertEqual(t, err, nil)
				paid += UsdToNanoCents(ProviderRevenueShare * NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))
			}
		}

		moveUnpaidContracts := func(createTime, closeTime time.Time, onlyClosedAfter *time.Time) {
			server.Tx(ctx, func(tx server.PgTx) {
				if onlyClosedAfter == nil {
					server.RaisePgResult(tx.Exec(ctx,
						`UPDATE transfer_contract SET create_time = $1, close_time = $2
						 WHERE contract_id IN (
						     SELECT contract_id FROM transfer_escrow_sweep WHERE payment_id IS NULL
						 )`,
						createTime, closeTime,
					))
				} else {
					server.RaisePgResult(tx.Exec(ctx,
						`UPDATE transfer_contract SET create_time = $1, close_time = $2
						 WHERE contract_id IN (
						     SELECT contract_id FROM transfer_escrow_sweep WHERE payment_id IS NULL
						 ) AND close_time >= $3`,
						createTime, closeTime, *onlyClosedAfter,
					))
				}
			})
		}

		seedSubsidyEpoch := func(startTime, endTime time.Time) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx,
					`INSERT INTO subsidy_payment (
						payment_plan_id, start_time, end_time,
						active_user_count, paid_user_count,
						net_payout_byte_count_paid, net_payout_byte_count_unpaid,
						net_revenue_nano_cents, net_payout_nano_cents
					) VALUES ($1, $2, $3, 0, 0, 0, 0, 0, 0)`,
					server.NewId(), startTime, endTime,
				))
			})
		}

		sumUnpaidBytes := func() ByteCount {
			var total ByteCount
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(ctx,
					`SELECT COALESCE(SUM(payout_byte_count), 0)::bigint FROM transfer_escrow_sweep WHERE payment_id IS NULL`,
				)
				server.WithPgResult(result, err, func() {
					if result.Next() {
						server.Raise(result.Scan(&total))
					}
				})
			})
			return total
		}

		now := server.NowUtc()
		maxDuration := 20 * 24 * time.Hour
		subsidyEnd := now.Add(-60 * 24 * time.Hour)
		seedSubsidyEpoch(subsidyEnd.Add(-5*24*time.Hour), subsidyEnd)

		// cohort A closes inside the first slice; cohort B one slice later.
		closeCohort()
		moveUnpaidContracts(now.Add(-58*24*time.Hour), now.Add(-50*24*time.Hour), nil)
		expectedBytesA := sumUnpaidBytes()
		connect.AssertEqual(t, 0 < expectedBytesA, true)

		closeCohort()
		recentThreshold := now.Add(-24 * time.Hour)
		moveUnpaidContracts(now.Add(-45*24*time.Hour), now.Add(-38*24*time.Hour), &recentThreshold)
		expectedBytesB := sumUnpaidBytes() - expectedBytesA
		connect.AssertEqual(t, 0 < expectedBytesB, true)

		// a single loop call drains the whole backlog, slice by slice.
		var sliceEnds []time.Time
		plans, err := PlanPaymentsWithMaxDurationLoop(ctx, maxDuration, func(p *PaymentPlan) {
			if p.SubsidyPayment != nil {
				sliceEnds = append(sliceEnds, p.SubsidyPayment.EndTime)
			}
		})
		connect.AssertEqual(t, err, nil)

		// everything is paid after the single loop call
		connect.AssertEqual(t, sumUnpaidBytes(), ByteCount(0))

		// the two cohorts were paid across separate committed slices, and the
		// total paid to the destination matches the full backlog.
		connect.AssertEqual(t, 2 <= len(plans), true)
		paidToDestination := ByteCount(0)
		for _, p := range plans {
			if payment, ok := p.NetworkPayments[destinationNetworkId]; ok {
				paidToDestination += payment.PayoutByteCount
			}
		}
		connect.AssertEqual(t, paidToDestination, expectedBytesA+expectedBytesB)

		// each subsidy slice advanced the frontier strictly forward
		connect.AssertEqual(t, 2 <= len(sliceEnds), true)
		for i := 1; i < len(sliceEnds); i += 1 {
			connect.AssertEqual(t, sliceEnds[i-1].Before(sliceEnds[i]), true)
		}

		// a subsequent loop call has nothing left to pay
		plans2, err := PlanPaymentsWithMaxDurationLoop(ctx, maxDuration, nil)
		connect.AssertEqual(t, err, nil)
		for _, p := range plans2 {
			connect.AssertEqual(t, len(p.NetworkPayments), 0)
		}
	})
}

// A dry run must compute the same plan a real run would, but persist nothing:
// no payments, no swept contracts marked paid. A real run immediately after
// must therefore still see the same contracts and produce the same payout.
func TestPlanPaymentsDryRun(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		netTransferByteCount := ByteCount(1024 * 1024 * 1024 * 1024)
		netRevenue := UsdToNanoCents(10.00)

		sourceNetworkId := server.NewId()
		sourceId := server.NewId()
		destinationNetworkId := server.NewId()
		destinationId := server.NewId()

		sourceSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: sourceNetworkId,
			ClientId:  &sourceId,
		})
		destinationSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: destinationNetworkId,
			ClientId:  &destinationId,
		})

		subscriptionYearDuration := 365 * 24 * time.Hour

		balanceCode, err := CreateBalanceCode(
			ctx,
			netTransferByteCount,
			subscriptionYearDuration,
			netRevenue,
			"",
			"",
			"",
		)
		connect.AssertEqual(t, err, nil)
		RedeemBalanceCode(&RedeemBalanceCodeArgs{
			Secret:    balanceCode.Secret,
			NetworkId: sourceNetworkId,
		}, sourceSession.Ctx)

		destinationWalletAddress := "0x1234567890"
		walletId := CreateAccountWalletExternal(destinationSession, &CreateAccountWalletExternalArgs{
			NetworkId:        destinationNetworkId,
			Blockchain:       "MATIC",
			WalletAddress:    destinationWalletAddress,
			DefaultTokenType: "USDC",
		})
		connect.AssertNotEqual(t, walletId, nil)
		err = SetPayoutWallet(ctx, destinationNetworkId, *walletId)
		connect.AssertEqual(t, err, nil)

		// sweep enough volume that the payout clears the minimum wallet threshold
		// (otherwise the payment would be withheld from the plan)
		paid := NanoCents(0)
		usedTransferByteCount := ByteCount(1024 * 1024 * 1024)
		for paid < 2*UsdToNanoCents(EnvSubsidyConfig().MinWalletPayoutUsd) {
			transferEscrow, err := CreateTransferEscrow(
				ctx,
				sourceNetworkId,
				sourceId,
				destinationNetworkId,
				destinationId,
				usedTransferByteCount,
			)
			connect.AssertEqual(t, err, nil)
			err = CloseContract(ctx, transferEscrow.ContractId, sourceId, usedTransferByteCount, false)
			connect.AssertEqual(t, err, nil)
			err = CloseContract(ctx, transferEscrow.ContractId, destinationId, usedTransferByteCount, false)
			connect.AssertEqual(t, err, nil)
			paid += UsdToNanoCents(ProviderRevenueShare * NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))
		}

		// dry run: computes the plan but must not persist anything
		dryPlan, err := PlanPaymentsDryRun(ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, slices.Collect(maps.Keys(dryPlan.NetworkPayments)), []server.Id{destinationNetworkId})
		dryPayment := dryPlan.NetworkPayments[destinationNetworkId]
		connect.AssertEqual(t, dryPayment.Payout > 0, true)

		// nothing persisted: the dry-run payment does not exist, and there are no
		// pending payments at all
		persistedDryPayment, err := GetPayment(ctx, dryPayment.PaymentId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, persistedDryPayment, (*AccountPayment)(nil))
		connect.AssertEqual(t, len(GetPendingPayments(ctx)), 0)
		connect.AssertEqual(t, len(GetPendingPaymentsInPlan(ctx, dryPlan.PaymentPlanId)), 0)

		// a real run immediately after sees the same (still unpaid) contracts and
		// produces the same payout, and this time it does persist
		realPlan, err := PlanPayments(ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, slices.Collect(maps.Keys(realPlan.NetworkPayments)), []server.Id{destinationNetworkId})
		realPayment := realPlan.NetworkPayments[destinationNetworkId]
		connect.AssertEqual(t, realPayment.Payout, dryPayment.Payout)

		persistedRealPayment, err := GetPayment(ctx, realPayment.PaymentId)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, persistedRealPayment, (*AccountPayment)(nil))
		connect.AssertEqual(t, persistedRealPayment.Payout, realPayment.Payout)
		connect.AssertEqual(t, len(GetPendingPayments(ctx)), 1)
	})
}

func TestGetNetworkProvideStats(t *testing.T) {

	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		sourceNetworkId := server.NewId()
		sourceId := server.NewId()
		destinationNetworkId := server.NewId()
		destinationId := server.NewId()

		sourceSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: sourceNetworkId,
			ClientId:  &sourceId,
		})
		destinationSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: destinationNetworkId,
			ClientId:  &destinationId,
		})

		// fund the source network
		netTransferByteCount := ByteCount(1024 * 1024)
		netRevenue := UsdToNanoCents(10.00)
		subscriptionYearDuration := 365 * 24 * time.Hour

		balanceCode, err := CreateBalanceCode(
			ctx,
			netTransferByteCount,
			subscriptionYearDuration,
			netRevenue,
			"",
			"",
			"",
		)
		connect.AssertEqual(t, err, nil)
		RedeemBalanceCode(&RedeemBalanceCodeArgs{
			Secret:    balanceCode.Secret,
			NetworkId: sourceSession.ByJwt.NetworkId,
		}, sourceSession.Ctx)

		// create a wallet to receive the payout
		args := &CreateAccountWalletExternalArgs{
			NetworkId:        destinationNetworkId,
			Blockchain:       "matic",
			WalletAddress:    "",
			DefaultTokenType: "usdc",
		}
		walletId := CreateAccountWalletExternal(destinationSession, args)
		connect.AssertNotEqual(t, walletId, nil)

		wallet := GetAccountWallet(ctx, *walletId)
		connect.AssertNotEqual(t, wallet, nil)

		err = SetPayoutWallet(ctx, destinationNetworkId, wallet.WalletId)
		connect.AssertEqual(t, err, nil)

		// Check network stats
		// Everything should be 0
		transferStats := GetTransferStats(ctx, destinationNetworkId)
		connect.AssertEqual(t, transferStats.UnpaidBytesProvided, ByteCount(0))
		connect.AssertEqual(t, transferStats.PaidBytesProvided, ByteCount(0))

		usedTransferByteCount := ByteCount(1024)
		paidByteCount := ByteCount(0)
		paid := NanoCents(0)

		// we want to meet MinWalletPayoutThreshold
		// otherwise the plan will not include the payout
		for paid < UsdToNanoCents(EnvSubsidyConfig().MinWalletPayoutUsd) {
			transferEscrow, err := CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, usedTransferByteCount)
			connect.AssertEqual(t, err, nil)

			err = CloseContract(ctx, transferEscrow.ContractId, sourceId, usedTransferByteCount, false)
			connect.AssertEqual(t, err, nil)
			err = CloseContract(ctx, transferEscrow.ContractId, destinationId, usedTransferByteCount, false)
			connect.AssertEqual(t, err, nil)
			paidByteCount += usedTransferByteCount
			paid += UsdToNanoCents(ProviderRevenueShare * NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))
		}

		// Check network stats
		// Should register unpaid byte count
		transferStats = GetTransferStats(ctx, destinationNetworkId)
		connect.AssertEqual(t, transferStats.UnpaidBytesProvided, paidByteCount)
		connect.AssertEqual(t, transferStats.PaidBytesProvided, ByteCount(0))

		// Plan payments
		plan, err := PlanPayments(ctx)
		connect.AssertEqual(t, err, nil)

		// Since the plan is in progress
		// and the payments are not marked as completed, should be still marked as unpaid
		transferStats = GetTransferStats(ctx, destinationNetworkId)
		connect.AssertEqual(t, transferStats.UnpaidBytesProvided, paidByteCount)
		connect.AssertEqual(t, transferStats.PaidBytesProvided, ByteCount(0))

		// mark plan items as complete
		for _, payment := range plan.NetworkPayments {
			RemovePaymentRecord(ctx, payment.PaymentId)
			SetPaymentRecord(ctx, payment.PaymentId, "usdc", NanoCentsToUsd(payment.Payout), "")
			RemovePaymentRecord(ctx, payment.PaymentId)
			SetPaymentRecord(ctx, payment.PaymentId, "usdc", NanoCentsToUsd(payment.Payout), "")

			mockTxHash := "0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"

			CompletePayment(ctx, payment.PaymentId, "", mockTxHash)
		}

		// items are completed, should be marked as paid bytes
		transferStats = GetTransferStats(ctx, destinationNetworkId)
		connect.AssertEqual(t, transferStats.PaidBytesProvided, paidByteCount)
		connect.AssertEqual(t, transferStats.UnpaidBytesProvided, ByteCount(0))

	})
}

func TestPlanPaymentsNeverAssignsForeignWallet(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		netTransferByteCount := ByteCount(1024 * 1024 * 1024 * 1024)
		netRevenue := UsdToNanoCents(10.00)

		sourceNetworkId := server.NewId()
		sourceId := server.NewId()
		destinationNetworkId := server.NewId()
		destinationId := server.NewId()
		foreignNetworkId := server.NewId()
		foreignClientId := server.NewId()

		sourceSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: sourceNetworkId,
			ClientId:  &sourceId,
		})
		destinationSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: destinationNetworkId,
			ClientId:  &destinationId,
		})
		foreignSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: foreignNetworkId,
			ClientId:  &foreignClientId,
		})

		balanceCode, err := CreateBalanceCode(
			ctx,
			netTransferByteCount,
			365*24*time.Hour,
			netRevenue,
			"",
			"",
			"",
		)
		connect.AssertEqual(t, err, nil)
		RedeemBalanceCode(&RedeemBalanceCodeArgs{
			Secret:    balanceCode.Secret,
			NetworkId: sourceNetworkId,
		}, sourceSession.Ctx)

		// the destination owns an active wallet and sets it for payout
		destinationWalletAddress := "0xdddd"
		destinationWalletId := CreateAccountWalletExternal(destinationSession, &CreateAccountWalletExternalArgs{
			NetworkId:        destinationNetworkId,
			Blockchain:       "MATIC",
			WalletAddress:    destinationWalletAddress,
			DefaultTokenType: "USDC",
		})
		connect.AssertNotEqual(t, destinationWalletId, nil)
		err = SetPayoutWallet(ctx, destinationNetworkId, *destinationWalletId)
		connect.AssertEqual(t, err, nil)

		// another network owns a separate wallet
		foreignWalletId := CreateAccountWalletExternal(foreignSession, &CreateAccountWalletExternalArgs{
			NetworkId:        foreignNetworkId,
			Blockchain:       "MATIC",
			WalletAddress:    "0xffff",
			DefaultTokenType: "USDC",
		})
		connect.AssertNotEqual(t, foreignWalletId, nil)

		// provide enough traffic to exceed the min payout
		usedTransferByteCount := ByteCount(1024 * 1024 * 1024)
		paid := NanoCents(0)
		for paid < UsdToNanoCents(EnvSubsidyConfig().MinWalletPayoutUsd) {
			transferEscrow, err := CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, usedTransferByteCount)
			connect.AssertEqual(t, err, nil)

			err = CloseContract(ctx, transferEscrow.ContractId, sourceId, usedTransferByteCount, false)
			connect.AssertEqual(t, err, nil)
			err = CloseContract(ctx, transferEscrow.ContractId, destinationId, usedTransferByteCount, false)
			connect.AssertEqual(t, err, nil)
			paid += UsdToNanoCents(ProviderRevenueShare * NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))
		}

		// corrupt the payout wallet to point at another network's wallet,
		// simulating rows written before ownership validation existed
		corruptPayoutWallet := func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`UPDATE payout_wallet SET wallet_id = $2 WHERE network_id = $1`,
					destinationNetworkId,
					*foreignWalletId,
				))
			})
		}
		corruptPayoutWallet()

		paymentPlan, err := PlanPayments(ctx)
		connect.AssertEqual(t, err, nil)
		payment, ok := paymentPlan.NetworkPayments[destinationNetworkId]
		connect.AssertEqual(t, ok, true)
		// the plan must never assign a wallet owned by another network
		connect.AssertEqual(t, payment.WalletId, nil)

		// updating the payment wallet must also refuse the foreign wallet
		UpdatePaymentWallet(ctx, payment.PaymentId)
		updatedPayment, err := GetPayment(ctx, payment.PaymentId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, updatedPayment.WalletId, nil)

		// once the network sets its own wallet, the payment adopts it
		err = SetPayoutWallet(ctx, destinationNetworkId, *destinationWalletId)
		connect.AssertEqual(t, err, nil)
		UpdatePaymentWallet(ctx, payment.PaymentId)
		updatedPayment, err = GetPayment(ctx, payment.PaymentId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, *updatedPayment.WalletId, *destinationWalletId)
		connect.AssertEqual(t, *updatedPayment.WalletAddress, destinationWalletAddress)

		// corrupting the payout wallet after assignment must not redirect the payment
		corruptPayoutWallet()
		UpdatePaymentWallet(ctx, payment.PaymentId)
		updatedPayment, err = GetPayment(ctx, payment.PaymentId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, *updatedPayment.WalletId, *destinationWalletId)
		connect.AssertEqual(t, *updatedPayment.WalletAddress, destinationWalletAddress)

	})
}

func TestPaymentPlanSubsidyEqualWeight(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		netTransferByteCount := ByteCount(1024 * 1024 * 1024 * 1024)
		netRevenue := UsdToNanoCents(10.00)

		paidPayerNetworkId := server.NewId()
		paidPayerClientId := server.NewId()
		freePayerNetworkId := server.NewId()
		freePayerClientId := server.NewId()
		providerANetworkId := server.NewId()
		providerAClientId := server.NewId()
		providerBNetworkId := server.NewId()
		providerBClientId := server.NewId()

		paidPayerSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: paidPayerNetworkId,
			ClientId:  &paidPayerClientId,
		})
		providerASession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: providerANetworkId,
			ClientId:  &providerAClientId,
		})
		providerBSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: providerBNetworkId,
			ClientId:  &providerBClientId,
		})

		// the paid payer purchases a balance (paid traffic)
		balanceCode, err := CreateBalanceCode(
			ctx,
			netTransferByteCount,
			365*24*time.Hour,
			netRevenue,
			"",
			"",
			"",
		)
		connect.AssertEqual(t, err, nil)
		RedeemBalanceCode(&RedeemBalanceCodeArgs{
			Secret:    balanceCode.Secret,
			NetworkId: paidPayerNetworkId,
		}, paidPayerSession.Ctx)

		// the free payer gets a no-cost balance (free traffic)
		err = AddBasicTransferBalance(
			ctx,
			freePayerNetworkId,
			netTransferByteCount,
			server.NowUtc().Add(-1*time.Hour),
			server.NowUtc().Add(365*24*time.Hour),
		)
		connect.AssertEqual(t, err, nil)

		// both providers own wallets set for payout
		providerAWalletId := CreateAccountWalletExternal(providerASession, &CreateAccountWalletExternalArgs{
			NetworkId:        providerANetworkId,
			Blockchain:       "MATIC",
			WalletAddress:    "0xaaaa",
			DefaultTokenType: "USDC",
		})
		connect.AssertNotEqual(t, providerAWalletId, nil)
		err = SetPayoutWallet(ctx, providerANetworkId, *providerAWalletId)
		connect.AssertEqual(t, err, nil)

		providerBWalletId := CreateAccountWalletExternal(providerBSession, &CreateAccountWalletExternalArgs{
			NetworkId:        providerBNetworkId,
			Blockchain:       "MATIC",
			WalletAddress:    "0xbbbb",
			DefaultTokenType: "USDC",
		})
		connect.AssertNotEqual(t, providerBWalletId, nil)
		err = SetPayoutWallet(ctx, providerBNetworkId, *providerBWalletId)
		connect.AssertEqual(t, err, nil)

		// equal traffic: the paid payer routes via provider A,
		// the free payer routes via provider B
		usedTransferByteCount := ByteCount(1024 * 1024 * 1024)

		escrowA, err := CreateTransferEscrow(ctx, paidPayerNetworkId, paidPayerClientId, providerANetworkId, providerAClientId, usedTransferByteCount)
		connect.AssertEqual(t, err, nil)
		err = CloseContract(ctx, escrowA.ContractId, paidPayerClientId, usedTransferByteCount, false)
		connect.AssertEqual(t, err, nil)
		err = CloseContract(ctx, escrowA.ContractId, providerAClientId, usedTransferByteCount, false)
		connect.AssertEqual(t, err, nil)

		escrowB, err := CreateTransferEscrow(ctx, freePayerNetworkId, freePayerClientId, providerBNetworkId, providerBClientId, usedTransferByteCount)
		connect.AssertEqual(t, err, nil)
		err = CloseContract(ctx, escrowB.ContractId, freePayerClientId, usedTransferByteCount, false)
		connect.AssertEqual(t, err, nil)
		err = CloseContract(ctx, escrowB.ContractId, providerBClientId, usedTransferByteCount, false)
		connect.AssertEqual(t, err, nil)

		// backdate the contracts so the subsidy covers half an epoch
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE transfer_contract SET create_time = $1`,
				server.NowUtc().Add(-15*24*time.Hour),
			))
		})

		paymentPlan, err := PlanPayments(ctx)
		connect.AssertEqual(t, err, nil)

		subsidyPayment := GetSubsidyPayment(ctx, paymentPlan.PaymentPlanId)
		connect.AssertNotEqual(t, subsidyPayment, nil)
		// paid and unpaid byte counts are still recorded for reporting
		connect.AssertEqual(t, subsidyPayment.NetPayoutByteCountPaid, usedTransferByteCount)
		connect.AssertEqual(t, subsidyPayment.NetPayoutByteCountUnpaid, usedTransferByteCount)
		connect.AssertEqual(t, subsidyPayment.PaidUserCount, 1)
		connect.AssertEqual(t, subsidyPayment.ActiveUserCount, 2)

		paymentA, ok := paymentPlan.NetworkPayments[providerANetworkId]
		connect.AssertEqual(t, ok, true)
		paymentB, ok := paymentPlan.NetworkPayments[providerBNetworkId]
		connect.AssertEqual(t, ok, true)

		// all traffic is weighted equally:
		// equal bytes earn equal subsidy whether the traffic was paid or free
		connect.AssertNotEqual(t, paymentA.SubsidyPayout, NanoCents(0))
		connect.AssertEqual(t, paymentA.SubsidyPayout, paymentB.SubsidyPayout)

		// the subsidy net payout is the sum of both provider subsidies
		connect.AssertEqual(t, subsidyPayment.NetPayout, paymentA.SubsidyPayout+paymentB.SubsidyPayout)

		// approximately half of the min payout pot (0.5 scale of the epoch)
		minExpected := UsdToNanoCents(0.499 * EnvSubsidyConfig().MinPayoutUsd)
		maxExpected := UsdToNanoCents(0.502 * EnvSubsidyConfig().MinPayoutUsd)
		if subsidyPayment.NetPayout < minExpected || maxExpected < subsidyPayment.NetPayout {
			connect.AssertEqual(t, subsidyPayment.NetPayout, UsdToNanoCents(0.5*EnvSubsidyConfig().MinPayoutUsd))
		}

		// the payments also carry the wallets owned by each provider
		connect.AssertEqual(t, *paymentA.WalletId, *providerAWalletId)
		connect.AssertEqual(t, *paymentB.WalletId, *providerBWalletId)

	})
}

func TestPaymentPlanSubsidy(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		netTransferByteCount := ByteCount(1024 * 1024 * 1024 * 1024)
		netRevenue := UsdToNanoCents(10.00)

		sourceNetworkId := server.NewId()
		sourceId := server.NewId()
		destinationNetworkId := server.NewId()
		destinationId := server.NewId()

		// add subscription to both source and destination
		AddSubscriptionRenewal(ctx, &SubscriptionRenewal{
			NetworkId:        sourceNetworkId,
			SubscriptionType: SubscriptionTypeSupporter,
			StartTime:        server.NowUtc(),
			EndTime:          server.NowUtc().Add(24 * time.Hour),
		})
		AddSubscriptionRenewal(ctx, &SubscriptionRenewal{
			NetworkId:        destinationNetworkId,
			SubscriptionType: SubscriptionTypeSupporter,
			StartTime:        server.NowUtc(),
			EndTime:          server.NowUtc().Add(24 * time.Hour),
		})

		sourceSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: sourceNetworkId,
			ClientId:  &sourceId,
		})
		destinationSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: destinationNetworkId,
			ClientId:  &destinationId,
		})

		getAccountBalanceResult := GetAccountBalance(sourceSession)
		connect.AssertEqual(t, getAccountBalanceResult.Balance.ProvidedByteCount, ByteCount(0))
		connect.AssertEqual(t, getAccountBalanceResult.Balance.ProvidedNetRevenue, NanoCents(0))
		connect.AssertEqual(t, getAccountBalanceResult.Balance.PaidByteCount, ByteCount(0))
		connect.AssertEqual(t, getAccountBalanceResult.Balance.PaidNetRevenue, NanoCents(0))

		getAccountBalanceResult = GetAccountBalance(destinationSession)
		connect.AssertEqual(t, getAccountBalanceResult.Balance.ProvidedByteCount, ByteCount(0))
		connect.AssertEqual(t, getAccountBalanceResult.Balance.ProvidedNetRevenue, NanoCents(0))
		connect.AssertEqual(t, getAccountBalanceResult.Balance.PaidByteCount, ByteCount(0))
		connect.AssertEqual(t, getAccountBalanceResult.Balance.PaidNetRevenue, NanoCents(0))

		subscriptionYearDuration := 365 * 24 * time.Hour

		balanceCode, err := CreateBalanceCode(
			ctx,
			netTransferByteCount,
			subscriptionYearDuration,
			netRevenue,
			"",
			"",
			"",
		)
		connect.AssertEqual(t, err, nil)
		RedeemBalanceCode(&RedeemBalanceCodeArgs{
			Secret:    balanceCode.Secret,
			NetworkId: sourceSession.ByJwt.NetworkId,
		}, sourceSession.Ctx)

		contractIds := GetOpenContractIds(ctx, sourceId, destinationId)
		connect.AssertEqual(t, len(contractIds), 0)

		// test that escrow prevents concurrent contracts

		transferEscrow, err := CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, netTransferByteCount)
		connect.AssertEqual(t, err, nil)

		transferBalances := GetActiveTransferBalances(ctx, sourceNetworkId)
		netBalanceByteCount := ByteCount(0)
		for _, transferBalance := range transferBalances {
			netBalanceByteCount += transferBalance.BalanceByteCount
		}
		// nothing left
		connect.AssertEqual(t, netBalanceByteCount, ByteCount(0))

		_, err = CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, netTransferByteCount)
		connect.AssertNotEqual(t, err, nil)

		CloseContract(ctx, transferEscrow.ContractId, sourceId, 0, false)
		CloseContract(ctx, transferEscrow.ContractId, destinationId, 0, false)

		transferBalances = GetActiveTransferBalances(ctx, sourceNetworkId)
		netBalanceByteCount = ByteCount(0)
		for _, transferBalance := range transferBalances {
			netBalanceByteCount += transferBalance.BalanceByteCount
		}
		connect.AssertEqual(t, netBalanceByteCount, netTransferByteCount)

		transferEscrow, err = CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, 1024*1024)
		connect.AssertEqual(t, err, nil)

		contractIds = GetOpenContractIds(ctx, sourceId, destinationId)
		connect.AssertEqual(t, contractIds, map[server.Id][]ContractParty{
			transferEscrow.ContractId: []ContractParty{},
		})

		usedTransferByteCount := ByteCount(1024)
		CloseContract(ctx, transferEscrow.ContractId, sourceId, usedTransferByteCount, false)
		CloseContract(ctx, transferEscrow.ContractId, destinationId, usedTransferByteCount, false)
		paidByteCount := usedTransferByteCount
		paid := UsdToNanoCents(ProviderRevenueShare * NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))

		contractIds = GetOpenContractIds(ctx, sourceId, destinationId)
		connect.AssertEqual(t, len(contractIds), 0)

		// check that the payout is pending
		getAccountBalanceResult = GetAccountBalance(sourceSession)
		connect.AssertEqual(t, getAccountBalanceResult.Balance.ProvidedByteCount, ByteCount(0))
		connect.AssertEqual(t, getAccountBalanceResult.Balance.ProvidedNetRevenue, NanoCents(0))
		connect.AssertEqual(t, getAccountBalanceResult.Balance.PaidByteCount, ByteCount(0))
		connect.AssertEqual(t, getAccountBalanceResult.Balance.PaidNetRevenue, NanoCents(0))

		getAccountBalanceResult = GetAccountBalance(destinationSession)
		connect.AssertEqual(t, getAccountBalanceResult.Balance.ProvidedByteCount, paidByteCount)
		connect.AssertEqual(t, getAccountBalanceResult.Balance.ProvidedNetRevenue, paid)
		connect.AssertEqual(t, getAccountBalanceResult.Balance.PaidByteCount, ByteCount(0))
		connect.AssertEqual(t, getAccountBalanceResult.Balance.PaidNetRevenue, NanoCents(0))

		transferBalances = GetActiveTransferBalances(ctx, sourceNetworkId)
		netBalanceByteCount = 0
		for _, transferBalance := range transferBalances {
			netBalanceByteCount += transferBalance.BalanceByteCount
		}
		connect.AssertEqual(t, netBalanceByteCount, netTransferByteCount-paidByteCount)

		args := &CreateAccountWalletExternalArgs{
			NetworkId:        destinationNetworkId,
			Blockchain:       "matic",
			WalletAddress:    "",
			DefaultTokenType: "usdc",
		}
		walletId := CreateAccountWalletExternal(destinationSession, args)
		connect.AssertNotEqual(t, walletId, nil)

		wallet := GetAccountWallet(ctx, *walletId)
		connect.AssertNotEqual(t, wallet, nil)

		err = SetPayoutWallet(ctx, destinationNetworkId, wallet.WalletId)
		connect.AssertEqual(t, err, nil)

		// plan a payment and complete the payment
		// nothing to plan because the payout does not meet the min threshold
		paymentPlan, err := PlanPayments(ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(paymentPlan.NetworkPayments), 0)
		connect.AssertEqual(t, paymentPlan.WithheldNetworkIds, []server.Id{destinationNetworkId})

		subsidyPayment := GetSubsidyPayment(ctx, paymentPlan.PaymentPlanId)
		connect.AssertNotEqual(t, subsidyPayment, nil)
		connect.AssertEqual(t, subsidyPayment.NetPayout, NanoCents(0))
		connect.AssertEqual(t, subsidyPayment.PaidUserCount, 1)
		connect.AssertEqual(t, subsidyPayment.ActiveUserCount, 1)
		connect.AssertEqual(t, subsidyPayment.NetPayoutByteCountPaid, paidByteCount)
		connect.AssertEqual(t, subsidyPayment.NetPayoutByteCountUnpaid, ByteCount(0))

		usedTransferByteCount = ByteCount(1024 * 1024 * 1024)
		for paid < UsdToNanoCents(EnvSubsidyConfig().MinWalletPayoutUsd) {
			transferEscrow, err := CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, usedTransferByteCount)
			connect.AssertEqual(t, err, nil)

			err = CloseContract(ctx, transferEscrow.ContractId, sourceId, usedTransferByteCount, false)
			connect.AssertEqual(t, err, nil)
			err = CloseContract(ctx, transferEscrow.ContractId, destinationId, usedTransferByteCount, false)
			connect.AssertEqual(t, err, nil)
			paidByteCount += usedTransferByteCount
			paid += UsdToNanoCents(ProviderRevenueShare * NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))
		}

		contractIds = GetOpenContractIds(ctx, sourceId, destinationId)
		connect.AssertEqual(t, len(contractIds), 0)

		getAccountBalanceResult = GetAccountBalance(destinationSession)
		connect.AssertEqual(t, getAccountBalanceResult.Balance.ProvidedByteCount, paidByteCount)
		connect.AssertEqual(t, getAccountBalanceResult.Balance.ProvidedNetRevenue, paid)
		connect.AssertEqual(t, getAccountBalanceResult.Balance.PaidByteCount, ByteCount(0))
		connect.AssertEqual(t, getAccountBalanceResult.Balance.PaidNetRevenue, NanoCents(0))

		paymentPlan, err = PlanPayments(ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, slices.Collect(maps.Keys(paymentPlan.NetworkPayments)), []server.Id{destinationNetworkId})

		subsidyPayment = GetSubsidyPayment(ctx, paymentPlan.PaymentPlanId)
		connect.AssertNotEqual(t, subsidyPayment, nil)
		connect.AssertEqual(t, subsidyPayment.NetPayout, NanoCents(0))
		connect.AssertEqual(t, subsidyPayment.PaidUserCount, 1)
		connect.AssertEqual(t, subsidyPayment.ActiveUserCount, 1)
		connect.AssertEqual(t, subsidyPayment.NetPayoutByteCountPaid, paidByteCount)
		connect.AssertEqual(t, subsidyPayment.NetPayoutByteCountUnpaid, ByteCount(0))
		subsidyPaidByteCount := paidByteCount

		mockTxHash := "0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"

		for _, payment := range paymentPlan.NetworkPayments {
			RemovePaymentRecord(ctx, payment.PaymentId)
			SetPaymentRecord(ctx, payment.PaymentId, "usdc", NanoCentsToUsd(payment.Payout), "")
			RemovePaymentRecord(ctx, payment.PaymentId)
			SetPaymentRecord(ctx, payment.PaymentId, "usdc", NanoCentsToUsd(payment.Payout), "")

			CompletePayment(ctx, payment.PaymentId, "", mockTxHash)
		}

		// check that the payment is recorded
		getAccountBalanceResult = GetAccountBalance(sourceSession)
		connect.AssertEqual(t, getAccountBalanceResult.Balance.ProvidedByteCount, ByteCount(0))
		connect.AssertEqual(t, getAccountBalanceResult.Balance.ProvidedNetRevenue, NanoCents(0))
		connect.AssertEqual(t, getAccountBalanceResult.Balance.PaidByteCount, ByteCount(0))
		connect.AssertEqual(t, getAccountBalanceResult.Balance.PaidNetRevenue, NanoCents(0))

		getAccountBalanceResult = GetAccountBalance(destinationSession)
		connect.AssertEqual(t, getAccountBalanceResult.Balance.ProvidedByteCount, paidByteCount)
		connect.AssertEqual(t, getAccountBalanceResult.Balance.ProvidedNetRevenue, paid)
		connect.AssertEqual(t, getAccountBalanceResult.Balance.PaidByteCount, paidByteCount)
		connect.AssertEqual(t, getAccountBalanceResult.Balance.PaidNetRevenue, paid)

		// repeat escrow until it fails due to no balance
		contractCount := 0
		usedTransferByteCount = ByteCount(1024 * 1024 * 1024)
		for {
			transferEscrow, err := CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, usedTransferByteCount)
			if err != nil && 1024 < usedTransferByteCount {
				usedTransferByteCount = usedTransferByteCount / 1024
				glog.Infof("Step down contract size to %d bytes.\n", usedTransferByteCount)
				continue
			}
			if netTransferByteCount <= paidByteCount {
				connect.AssertNotEqual(t, err, nil)
				break
			} else {
				connect.AssertEqual(t, err, nil)
			}

			CloseContract(ctx, transferEscrow.ContractId, sourceId, usedTransferByteCount, false)
			CloseContract(ctx, transferEscrow.ContractId, destinationId, usedTransferByteCount, false)
			paidByteCount += usedTransferByteCount
			paid += UsdToNanoCents(ProviderRevenueShare * NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))
			contractCount += 1
		}
		// at this point the balance should be fully used up

		transferBalances = GetActiveTransferBalances(ctx, sourceNetworkId)
		connect.AssertEqual(t, transferBalances, []*TransferBalance{})

		paymentPlan, err = PlanPayments(ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, slices.Collect(maps.Keys(paymentPlan.NetworkPayments)), []server.Id{destinationNetworkId})

		subsidyPayment = GetSubsidyPayment(ctx, paymentPlan.PaymentPlanId)
		connect.AssertNotEqual(t, subsidyPayment, nil)
		connect.AssertEqual(t, subsidyPayment.NetPayout, NanoCents(0))
		connect.AssertEqual(t, subsidyPayment.PaidUserCount, 1)
		connect.AssertEqual(t, subsidyPayment.ActiveUserCount, 1)
		connect.AssertEqual(t, subsidyPayment.NetPayoutByteCountPaid, paidByteCount-subsidyPaidByteCount)
		connect.AssertEqual(t, subsidyPayment.NetPayoutByteCountUnpaid, ByteCount(0))
		subsidyPaidByteCount = paidByteCount - subsidyPaidByteCount

		for _, payment := range paymentPlan.NetworkPayments {
			RemovePaymentRecord(ctx, payment.PaymentId)
			SetPaymentRecord(ctx, payment.PaymentId, "usdc", NanoCentsToUsd(payment.Payout), "")
			RemovePaymentRecord(ctx, payment.PaymentId)
			SetPaymentRecord(ctx, payment.PaymentId, "usdc", NanoCentsToUsd(payment.Payout), "")

			CompletePayment(ctx, payment.PaymentId, "", mockTxHash)
			UpdatePaymentWallet(ctx, payment.PaymentId)
		}

		// check that the payment is recorded
		getAccountBalanceResult = GetAccountBalance(sourceSession)
		connect.AssertEqual(t, getAccountBalanceResult.Balance.ProvidedByteCount, ByteCount(0))
		connect.AssertEqual(t, getAccountBalanceResult.Balance.ProvidedNetRevenue, NanoCents(0))
		connect.AssertEqual(t, getAccountBalanceResult.Balance.PaidByteCount, ByteCount(0))
		connect.AssertEqual(t, getAccountBalanceResult.Balance.PaidNetRevenue, NanoCents(0))

		// the revenue from
		getAccountBalanceResult = GetAccountBalance(destinationSession)
		connect.AssertEqual(t, getAccountBalanceResult.Balance.ProvidedByteCount, netTransferByteCount)
		// each contract can have a 1 nanocent rounding error
		if e := getAccountBalanceResult.Balance.ProvidedNetRevenue - UsdToNanoCents(ProviderRevenueShare*NanoCentsToUsd(netRevenue)); e < -int64(contractCount) || int64(contractCount) < e {
			connect.AssertEqual(t, getAccountBalanceResult.Balance.ProvidedNetRevenue, UsdToNanoCents(ProviderRevenueShare*NanoCentsToUsd(netRevenue)))
		}
		connect.AssertEqual(t, getAccountBalanceResult.Balance.PaidByteCount, netTransferByteCount)
		// each contract can have a 1 nanocent rounding error
		if e := getAccountBalanceResult.Balance.PaidNetRevenue - UsdToNanoCents(ProviderRevenueShare*NanoCentsToUsd(netRevenue)); e < -int64(contractCount) || int64(contractCount) < e {
			connect.AssertEqual(t, getAccountBalanceResult.Balance.PaidNetRevenue, UsdToNanoCents(ProviderRevenueShare*NanoCentsToUsd(netRevenue)))
		}

		// there shoud be no more payments
		paymentPlan, err = PlanPayments(ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(paymentPlan.NetworkPayments), 0)

		subsidyPayment = GetSubsidyPayment(ctx, paymentPlan.PaymentPlanId)
		connect.AssertEqual(t, subsidyPayment, nil)

	})
}

// TestPlanPaymentsNeedsRepaymentSetEquivalence pins the payout planner's
// "needs (re)payment" selection (the temp_account_payment set built at the top
// of planPayments) to be identical under the new UNION-ALL query and the old
// LEFT-JOIN anti-join it replaced. It seeds sweeps in every payment state and
// asserts the selected (contract_id, balance_id) multiset is byte-for-byte the
// same as the old query's, for both the unbounded (live) plan and the bounded
// (backlog-slice, close_time < upperBound) plan.
//
// Payment states:
//   - unpaid    (payment_id NULL)                 -> selected (unpaid arm)
//   - canceled  (payment_id -> canceled payment)  -> selected (canceled arm)
//   - completed (payment_id -> completed payment) -> NOT selected
//   - active    (payment_id -> pending payment)   -> NOT selected
//
// Canceling a payment sets account_payment.canceled = true but does not null the
// sweep's payment_id (see CancelHungAccountPayments), which is exactly why the
// canceled arm joins the canceled-payment set back to its sweeps by payment_id.
func TestPlanPaymentsNeedsRepaymentSetEquivalence(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		planId := server.NewId()
		now := server.NowUtc()

		type sweepFixture struct {
			contractId server.Id
			balanceId  server.Id
			state      string
			closeTime  time.Time
			eligible   bool // expected in the needs-(re)payment set
		}

		oldCloseTime := now.Add(-10 * 24 * time.Hour)
		recentCloseTime := now.Add(-1 * 24 * time.Hour)

		fixtures := []sweepFixture{
			{server.NewId(), server.NewId(), "unpaid", oldCloseTime, true},
			{server.NewId(), server.NewId(), "canceled", oldCloseTime, true},
			{server.NewId(), server.NewId(), "completed", oldCloseTime, false},
			{server.NewId(), server.NewId(), "active", oldCloseTime, false},
			// a second unpaid + canceled pair closing recently, so the bounded
			// (close_time < upperBound) slice excludes them while the unbounded
			// plan includes them.
			{server.NewId(), server.NewId(), "unpaid", recentCloseTime, true},
			{server.NewId(), server.NewId(), "canceled", recentCloseTime, true},
			{server.NewId(), server.NewId(), "completed", recentCloseTime, false},
		}

		server.Tx(ctx, func(tx server.PgTx) {
			for _, f := range fixtures {
				var paymentId *server.Id
				if f.state != "unpaid" {
					pid := server.NewId()
					paymentId = &pid
					server.RaisePgResult(tx.Exec(ctx,
						`
						INSERT INTO account_payment (
							payment_id, payment_plan_id, wallet_id,
							payout_byte_count, payout_nano_cents, min_sweep_time,
							completed, canceled
						) VALUES ($1, $2, NULL, 0, 0, $3, $4, $5)
						`,
						pid, planId, now,
						f.state == "completed",
						f.state == "canceled",
					))
				}
				server.RaisePgResult(tx.Exec(ctx,
					`
					INSERT INTO transfer_contract (
						contract_id, source_network_id, source_id,
						destination_network_id, destination_id,
						transfer_byte_count, create_time, close_time
					) VALUES ($1, $2, $2, $3, $3, 0, $4, $5)
					`,
					f.contractId, server.NewId(), networkId,
					f.closeTime.Add(-time.Hour), f.closeTime,
				))
				server.RaisePgResult(tx.Exec(ctx,
					`
					INSERT INTO transfer_escrow_sweep (
						contract_id, balance_id, network_id,
						payout_byte_count, payout_net_revenue_nano_cents, payment_id
					) VALUES ($1, $2, $3, 100, 100, $4)
					`,
					f.contractId, f.balanceId, networkId, paymentId,
				))
			}
		})

		key := func(c, b server.Id) string { return c.String() + ":" + b.String() }

		runSet := func(query string, args ...any) []string {
			set := []string{}
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(ctx, query, args...)
				server.WithPgResult(result, err, func() {
					for result.Next() {
						var c, b server.Id
						server.Raise(result.Scan(&c, &b))
						set = append(set, key(c, b))
					}
				})
			})
			slices.Sort(set)
			return set
		}

		// the exact pre-fix anti-join query, parameterized by the same
		// closeTimeJoin/closeTimeBound fragments the planner substitutes.
		oldQuery := func(closeTimeJoin, closeTimeBound string) string {
			return fmt.Sprintf(`
				SELECT
					transfer_escrow_sweep.contract_id,
					transfer_escrow_sweep.balance_id
				FROM transfer_escrow_sweep
				LEFT JOIN account_payment ON
					account_payment.payment_id = transfer_escrow_sweep.payment_id
				%s
				WHERE
					(account_payment.payment_id IS NULL OR
					account_payment.canceled = true)
					%s
			`, closeTimeJoin, closeTimeBound)
		}
		// the post-fix UNION-ALL query, mirroring planPayments.
		newQuery := func(closeTimeJoin, closeTimeBound string) string {
			return fmt.Sprintf(`
				SELECT
					u.contract_id,
					u.balance_id
				FROM (
					SELECT
						transfer_escrow_sweep.contract_id,
						transfer_escrow_sweep.balance_id
					FROM transfer_escrow_sweep
					WHERE transfer_escrow_sweep.payment_id IS NULL

					UNION ALL

					SELECT
						s.contract_id,
						s.balance_id
					FROM account_payment ap
					INNER JOIN transfer_escrow_sweep s ON
						s.payment_id = ap.payment_id
					WHERE ap.canceled = true
				) u
				%s
				%s
			`, closeTimeJoin, closeTimeBound)
		}

		// --- unbounded (live plan): no close-time join/bound ---
		oldUnbounded := runSet(oldQuery("", ""))
		newUnbounded := runSet(newQuery("", ""))
		connect.AssertEqual(t, newUnbounded, oldUnbounded)

		expectedUnbounded := []string{}
		for _, f := range fixtures {
			if f.eligible {
				expectedUnbounded = append(expectedUnbounded, key(f.contractId, f.balanceId))
			}
		}
		slices.Sort(expectedUnbounded)
		connect.AssertEqual(t, newUnbounded, expectedUnbounded)

		// --- bounded (backlog slice): close_time < upperBound ---
		// upperBound includes oldCloseTime, excludes recentCloseTime.
		upperBound := now.Add(-5 * 24 * time.Hour)
		oldBounded := runSet(
			oldQuery(
				`
				INNER JOIN transfer_contract ON
					transfer_contract.contract_id = transfer_escrow_sweep.contract_id`,
				"AND transfer_contract.close_time < $1",
			),
			upperBound,
		)
		newBounded := runSet(
			newQuery(
				`
				INNER JOIN transfer_contract ON
					transfer_contract.contract_id = u.contract_id`,
				"WHERE transfer_contract.close_time < $1",
			),
			upperBound,
		)
		connect.AssertEqual(t, newBounded, oldBounded)

		expectedBounded := []string{}
		for _, f := range fixtures {
			if f.eligible && f.closeTime.Before(upperBound) {
				expectedBounded = append(expectedBounded, key(f.contractId, f.balanceId))
			}
		}
		slices.Sort(expectedBounded)
		connect.AssertEqual(t, newBounded, expectedBounded)
	})
}
