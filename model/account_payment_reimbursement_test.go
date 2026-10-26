package model

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

var manualReimbursementTestNow = time.Date(2026, 10, 26, 14, 0, 0, 0, time.UTC)

type manualReimbursementTestNetwork struct {
	networkId     server.Id
	pinnedWallet  server.Id
	pinnedAddress string
	payoutWallet  server.Id
}

// A network with two active Solana wallets, the second its payout wallet.
func newManualReimbursementTestNetwork(t testing.TB, ctx context.Context) *manualReimbursementTestNetwork {
	t.Helper()
	network := &manualReimbursementTestNetwork{
		networkId:     server.NewId(),
		pinnedWallet:  server.NewId(),
		pinnedAddress: "SyntheticPinned" + strings.ReplaceAll(server.NewId().String(), "-", ""),
		payoutWallet:  server.NewId(),
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network (network_id, network_name, admin_user_id) VALUES ($1, $2, $3)`,
			network.networkId, "synthetic-reimbursement-"+network.networkId.String(), server.NewId()))
		for walletId, address := range map[server.Id]string{
			network.pinnedWallet: network.pinnedAddress,
			network.payoutWallet: "SyntheticPayout" + strings.ReplaceAll(server.NewId().String(), "-", ""),
		} {
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO account_wallet (wallet_id, network_id, wallet_type, blockchain, wallet_address, active, default_token_type, create_time)
				VALUES ($1, $2, 'external', 'SOL', $3, true, 'USDC', $4)
			`, walletId, network.networkId, address, manualReimbursementTestNow.Add(-24*time.Hour)))
		}
	})
	if err := SetPayoutWallet(ctx, network.networkId, network.payoutWallet); err != nil {
		t.Fatal(err)
	}
	return network
}

// A one-payment plan paying `amountNanoCents` to the network's pinned wallet.
func manualReimbursementTestPlan(network *manualReimbursementTestNetwork, digest string, amountNanoCents NanoCents) *ManualReimbursementPlan {
	plan := &ManualReimbursementPlan{PaymentPlanId: server.NewId(), Digest: digest, Reason: "synthetic reimbursement"}
	plan.Markers = append(plan.Markers, &ManualReimbursementMarker{
		Version:         1,
		Digest:          digest,
		Reason:          plan.Reason,
		PaymentPlanId:   plan.PaymentPlanId,
		PaymentId:       server.NewId(),
		NetworkId:       network.networkId,
		WalletId:        network.pinnedWallet,
		WalletAddress:   network.pinnedAddress,
		Blockchain:      "SOL",
		AmountNanoCents: amountNanoCents,
		FeeNanoCents:    10_000_000,
		PayoutNanoCents: amountNanoCents + 10_000_000,
	})
	return plan
}

// An unmarked pending payment of the network on its pinned wallet, without sweeps.
func manualReimbursementTestOrdinaryPayment(t testing.TB, ctx context.Context, network *manualReimbursementTestNetwork) server.Id {
	t.Helper()
	paymentId := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO account_payment (payment_id, payment_plan_id, network_id, wallet_id, payout_byte_count, payout_nano_cents, min_sweep_time, create_time)
			VALUES ($1, $2, $3, $4, 0, $5, $6, $6)
		`, paymentId, server.NewId(), network.networkId, network.pinnedWallet, UsdToNanoCents(2), manualReimbursementTestNow))
	})
	return paymentId
}

func manualReimbursementTestWallet(t testing.TB, ctx context.Context, paymentId server.Id) server.Id {
	t.Helper()
	payment, err := GetPayment(ctx, paymentId)
	if err != nil || payment == nil || payment.WalletId == nil {
		t.Fatalf("payment %s: %v", paymentId, err)
	}
	return *payment.WalletId
}

// The pin: the wallet update moves an ordinary unsent payment to the network's
// payout wallet, and leaves a manual reimbursement on its pinned wallet. This
// test fails without the pin.
func TestManualReimbursementKeepsItsPinnedWallet(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		network := newManualReimbursementTestNetwork(t, ctx)
		plan := manualReimbursementTestPlan(network, "synthetic-pin-digest", 45_678_901_000)
		if created, err := CreateManualReimbursementPlan(ctx, plan, manualReimbursementTestNow); err != nil || !created {
			t.Fatal(created, err)
		}
		reimbursement := plan.Markers[0].PaymentId
		ordinary := manualReimbursementTestOrdinaryPayment(t, ctx, network)

		UpdatePaymentWallet(ctx, ordinary)
		UpdatePaymentWallet(ctx, reimbursement)

		if wallet := manualReimbursementTestWallet(t, ctx, ordinary); wallet != network.payoutWallet {
			t.Fatal("the wallet update did not run for the ordinary payment")
		}
		if wallet := manualReimbursementTestWallet(t, ctx, reimbursement); wallet != network.pinnedWallet {
			t.Fatalf("the wallet update re-pointed the reimbursement to %s, not its pinned wallet %s", wallet, network.pinnedWallet)
		}
	})
}

// The hung-payment job cancels an ordinary unsent payment past its age, never
// a manual reimbursement of the same age.
func TestManualReimbursementIsNeverCanceledAsHung(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		network := newManualReimbursementTestNetwork(t, ctx)
		plan := manualReimbursementTestPlan(network, "synthetic-hung-digest", 1_000_000_000)
		if _, err := CreateManualReimbursementPlan(ctx, plan, manualReimbursementTestNow); err != nil {
			t.Fatal(err)
		}
		ordinary := manualReimbursementTestOrdinaryPayment(t, ctx, network)

		if canceled := CancelHungAccountPayments(ctx, manualReimbursementTestNow.Add(HungPaymentExpiration-time.Hour)); canceled != 0 {
			t.Fatal("young payments were canceled")
		}
		if canceled := CancelHungAccountPayments(ctx, manualReimbursementTestNow.Add(10*HungPaymentExpiration)); canceled != 1 {
			t.Fatalf("the job canceled %d payments, want only the ordinary one", canceled)
		}
		for paymentId, wantCanceled := range map[server.Id]bool{ordinary: true, plan.Markers[0].PaymentId: false} {
			payment, err := GetPayment(ctx, paymentId)
			if err != nil || payment.Canceled != wantCanceled {
				t.Fatalf("payment %s canceled %t, want %t (%v)", paymentId, payment.Canceled, wantCanceled, err)
			}
		}
	})
}

// A marked payment is admitted by its marker alone, and only while the payment
// and the request match it exactly; an unmarked payment is admitted as before.
func TestManualReimbursementAdmission(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		network := newManualReimbursementTestNetwork(t, ctx)
		plan := manualReimbursementTestPlan(network, "synthetic-admission-digest", 123_456_789_000)
		if _, err := CreateManualReimbursementPlan(ctx, plan, manualReimbursementTestNow); err != nil {
			t.Fatal(err)
		}
		paymentId := plan.Markers[0].PaymentId
		listed := NanoCentsToUsd(plan.Markers[0].PayoutNanoCents) - 0.01
		other := listed + 0.000001

		if err := RequireProviderUsdcPayment(ctx, paymentId); err != nil {
			t.Fatal("the reimbursement was refused", err)
		}
		if err := requireProviderUsdcPayment(ctx, paymentId, &listed); err != nil {
			t.Fatal("the listed amount was refused", err)
		}
		if err := requireProviderUsdcPayment(ctx, paymentId, &other); !errors.Is(err, ErrManualReimbursementChanged) {
			t.Fatal("another amount was admitted", err)
		}
		ordinary := manualReimbursementTestOrdinaryPayment(t, ctx, network)
		if err := requireProviderUsdcPayment(ctx, ordinary, &other); err != nil {
			t.Fatal("an unmarked payment without a policy was refused", err)
		}

		// re-pointed by a writer older than the pin: refused until restored
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE account_payment SET wallet_id = $2 WHERE payment_id = $1`, paymentId, network.payoutWallet))
		})
		if err := RequireProviderUsdcPayment(ctx, paymentId); !errors.Is(err, ErrManualReimbursementChanged) {
			t.Fatal("a re-pointed reimbursement was admitted", err)
		}
		if repinned, err := RepinManualReimbursementWallet(ctx, paymentId); err != nil || !repinned {
			t.Fatal("the pin was not restored", repinned, err)
		}
		if repinned, err := RepinManualReimbursementWallet(ctx, paymentId); err != nil || repinned {
			t.Fatal("restoring an intact pin changed it", repinned, err)
		}
		if err := RequireProviderUsdcPayment(ctx, paymentId); err != nil {
			t.Fatal("the restored reimbursement was refused", err)
		}

		// the pinned wallet's address changed under it
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE account_wallet SET wallet_address = $2 WHERE wallet_id = $1`, network.pinnedWallet, network.pinnedAddress+"x"))
		})
		if err := RequireProviderUsdcPayment(ctx, paymentId); !errors.Is(err, ErrManualReimbursementChanged) {
			t.Fatal("a reimbursement to a changed address was admitted", err)
		}

		// a marker that does not parse admits nothing
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE audit_account_payment SET event_details = '{' WHERE event_id = $1`, paymentId))
		})
		if err := RequireProviderUsdcPayment(ctx, paymentId); !errors.Is(err, ErrManualReimbursementChanged) {
			t.Fatal("an unreadable marker was admitted", err)
		}
	})
}

// The plan is created in one transaction: all of its payments and markers or
// none. A rerun creates nothing; another plan for a network with an open
// reimbursement, a refused address, or a wallet that changed is refused.
func TestCreateManualReimbursementPlanIsAtomicAndIdempotent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		count := func(paymentPlanId server.Id) (payments int, markers int) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `
					SELECT COUNT(*), COUNT(*) FILTER (WHERE `+manualReimbursementMarked("p")+`)
					FROM account_payment p WHERE p.payment_plan_id = $1
				`, paymentPlanId).Scan(&payments, &markers))
			})
			return
		}

		first := newManualReimbursementTestNetwork(t, ctx)
		second := newManualReimbursementTestNetwork(t, ctx)
		plan := manualReimbursementTestPlan(first, "synthetic-atomic-digest", 1_000_000)
		plan.Markers = append(plan.Markers, manualReimbursementTestPlan(second, "synthetic-atomic-digest", 2_000_000).Markers[0])
		plan.Markers[1].PaymentPlanId = plan.PaymentPlanId

		// the second wallet is deactivated after the census: nothing is created
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE account_wallet SET active = false WHERE wallet_id = $1`, second.pinnedWallet))
		})
		if _, err := CreateManualReimbursementPlan(ctx, plan, manualReimbursementTestNow); !errors.Is(err, ErrManualReimbursementWalletChanged) {
			t.Fatal("an inactive wallet was accepted", err)
		}
		if payments, markers := count(plan.PaymentPlanId); payments != 0 || markers != 0 {
			t.Fatal("a refused plan wrote", payments, markers)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE account_wallet SET active = true WHERE wallet_id = $1`, second.pinnedWallet))
		})

		if created, err := CreateManualReimbursementPlan(ctx, plan, manualReimbursementTestNow); err != nil || !created {
			t.Fatal(created, err)
		}
		if payments, markers := count(plan.PaymentPlanId); payments != 2 || markers != 2 {
			t.Fatal("the plan", payments, markers)
		}
		if created, err := CreateManualReimbursementPlan(ctx, plan, manualReimbursementTestNow.Add(time.Hour)); err != nil || created {
			t.Fatal("the rerun", created, err)
		}
		if payments, markers := count(plan.PaymentPlanId); payments != 2 || markers != 2 {
			t.Fatal("the rerun wrote", payments, markers)
		}
		for _, marker := range plan.Markers {
			payment, err := GetPayment(ctx, marker.PaymentId)
			if err != nil || payment == nil || payment.Payout != marker.PayoutNanoCents || *payment.WalletId != marker.WalletId ||
				payment.PayoutByteCount != 0 || payment.SubsidyPayout != 0 || payment.ReliabilitySubsidy != 0 || !payment.CreateTime.Equal(manualReimbursementTestNow) {
				t.Fatalf("payment %+v %v", payment, err)
			}
		}

		// the same plan id with other payments
		changed := manualReimbursementTestPlan(first, "synthetic-atomic-digest", 1_000_001)
		changed.PaymentPlanId = plan.PaymentPlanId
		changed.Markers[0].PaymentPlanId = plan.PaymentPlanId
		changed.Markers[0].PaymentId = plan.Markers[0].PaymentId
		if _, err := CreateManualReimbursementPlan(ctx, changed, manualReimbursementTestNow); !errors.Is(err, ErrManualReimbursementPlanChanged) {
			t.Fatal("a changed plan was accepted", err)
		}

		// another plan for a network with an open reimbursement
		another := manualReimbursementTestPlan(first, "synthetic-another-digest", 1_000_000)
		if _, err := CreateManualReimbursementPlan(ctx, another, manualReimbursementTestNow); err == nil || !strings.Contains(err.Error(), "already has manual reimbursement") {
			t.Fatal("a second open reimbursement was accepted", err)
		}
		if payments, _ := count(another.PaymentPlanId); payments != 0 {
			t.Fatal("the refused plan wrote")
		}

		// the refused address, whatever the wallet row says
		refused := newManualReimbursementTestNetwork(t, ctx)
		attackerPlan := manualReimbursementTestPlan(refused, "synthetic-refused-digest", 1_000_000)
		attackerPlan.Markers[0].WalletAddress = ManualReimbursementRefusedAddress
		if _, err := CreateManualReimbursementPlan(ctx, attackerPlan, manualReimbursementTestNow); err == nil || !strings.Contains(err.Error(), "refused address") {
			t.Fatal("the refused address was accepted", err)
		}
	})
}
