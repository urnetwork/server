// The missing-wallet notice of a payout run: each plan slice holds a notice per
// wallet-less network in its own transaction, the run releases one notice per
// network with the total withheld across its slices, and the delivery task
// sends it. A run that stops after committing slices loses nothing: its held
// notices are released by the next run or, after the hold timeout, by the
// delivery task. The planners here commit synthetic slices in real
// transactions; the payout planner itself is not involved.
package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// A wallet-less network whose admin signs in by email, and its recipient.
func newMissingWalletTestNetwork(ctx context.Context, name string) (networkId server.Id, userAuth string) {
	networkId = server.NewId()
	userAuth = model.Testing_CreateNetwork(ctx, networkId, name, server.NewId())
	return
}

// A synthetic plan slice with one payment per network; a nil wallet is a
// wallet-less payment.
func missingWalletTestPlan(payments map[server.Id]model.NanoCents, walletIds map[server.Id]*server.Id) *model.PaymentPlan {
	plan := &model.PaymentPlan{
		PaymentPlanId:   server.NewId(),
		NetworkPayments: map[server.Id]*model.AccountPayment{},
	}
	for networkId, payout := range payments {
		plan.NetworkPayments[networkId] = &model.AccountPayment{
			PaymentId:     server.NewId(),
			PaymentPlanId: plan.PaymentPlanId,
			NetworkId:     networkId,
			WalletId:      walletIds[networkId],
			Payout:        payout,
		}
	}
	return plan
}

// The missing-wallet notices the outbox holds or owes for the network.
func missingWalletTestMessages(t testing.TB, ctx context.Context, networkId server.Id) (heldCount int, dueCount int) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
				SELECT
					COUNT(*) FILTER (WHERE deliver_time IS NULL),
					COUNT(*) FILTER (WHERE deliver_time IS NOT NULL)
				FROM account_message_outbox
				WHERE network_id = $1 AND template_name = 'subscription_missing_wallet'
			`,
			networkId,
		).Scan(&heldCount, &dueCount))
	})
	return
}

// A fixed release minimum.
func missingWalletTestMinPayout(minPayout model.NanoCents) func() model.NanoCents {
	return func() model.NanoCents {
		return minPayout
	}
}

// A slice holds one notice per wallet-less network whose admin has an email or
// phone, in the slice's transaction: none for a network with a wallet, none for
// a guest admin, and none at all when the slice rolls back.
func TestMissingWalletNoticeIsHeldInTheSliceTransaction(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		walletLessNetworkId, _ := newMissingWalletTestNetwork(ctx, "synthetic-wallet-less")
		walletNetworkId, _ := newMissingWalletTestNetwork(ctx, "synthetic-with-wallet")
		guestNetworkId := server.NewId()
		model.Testing_CreateGuestNetwork(ctx, guestNetworkId, "synthetic-guest-admin", server.NewId())
		walletId := server.NewId()
		plan := missingWalletTestPlan(
			map[server.Id]model.NanoCents{
				walletLessNetworkId: model.UsdToNanoCents(2),
				walletNetworkId:     model.UsdToNanoCents(2),
				guestNetworkId:      model.UsdToNanoCents(2),
			},
			map[server.Id]*server.Id{walletNetworkId: &walletId},
		)

		rollbackErr := errors.New("synthetic slice rollback")
		func() {
			defer func() {
				if value := recover(); value != rollbackErr {
					panic(value)
				}
			}()
			server.Tx(ctx, func(tx server.PgTx) {
				holdMissingWalletNoticesInTx(ctx, tx, plan)
				panic(rollbackErr)
			})
		}()
		if heldCount, dueCount := missingWalletTestMessages(t, ctx, walletLessNetworkId); heldCount+dueCount != 0 {
			t.Fatalf("a rolled-back slice left %d held and %d due notices", heldCount, dueCount)
		}

		server.Tx(ctx, func(tx server.PgTx) {
			holdMissingWalletNoticesInTx(ctx, tx, plan)
		})
		if heldCount, dueCount := missingWalletTestMessages(t, ctx, walletLessNetworkId); heldCount != 1 || dueCount != 0 {
			t.Fatalf("the wallet-less network has %d held and %d due notices, want one held", heldCount, dueCount)
		}
		for _, networkId := range []server.Id{walletNetworkId, guestNetworkId} {
			if count := outboxMessageCount(t, ctx, networkId); count != 0 {
				t.Fatalf("network %s has %d notices, want none", networkId, count)
			}
		}
		sender := newOutboxTestSender()
		deliverAccountMessagesAt(ctx, sender, server.NowUtc().Add(time.Hour))
		if sends := sender.sent(); len(sends) != 0 {
			t.Fatalf("delivered %d held notices before the run released them", len(sends))
		}
	})
}

// A run whose slices hold notices for the same network sends that network one
// notice, carried by the first slice's payment, with the total withheld; a
// slice that rolled back adds nothing, a network below the minimum gets none,
// and a planner error after committed slices still releases them.
func TestSendPaymentsReleasesOneMissingWalletNoticePerRun(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		defer clientSession.Cancel()
		networkId, userAuth := newMissingWalletTestNetwork(ctx, "synthetic-two-slices")
		smallNetworkId, _ := newMissingWalletTestNetwork(ctx, "synthetic-small-payout")
		minPayout := configuredMissingWalletNoticeMinPayout()

		firstSlice := missingWalletTestPlan(map[server.Id]model.NanoCents{
			networkId:      model.UsdToNanoCents(1.25),
			smallNetworkId: minPayout / 4,
		}, nil)
		secondSlice := missingWalletTestPlan(map[server.Id]model.NanoCents{
			networkId: model.UsdToNanoCents(0.75),
		}, nil)
		rolledBackSlice := missingWalletTestPlan(map[server.Id]model.NanoCents{
			networkId: model.UsdToNanoCents(100),
		}, nil)
		laterSliceErr := errors.New("synthetic later slice failure")

		err := sendPaymentsWithPlanner(clientSession, func(
			plannerCtx context.Context,
			maxDuration time.Duration,
			writeInTx func(server.PgTx, *model.PaymentPlan),
			onSlice func(*model.PaymentPlan),
		) ([]*model.PaymentPlan, error) {
			for _, slice := range []*model.PaymentPlan{firstSlice, secondSlice} {
				server.Tx(plannerCtx, func(tx server.PgTx) {
					writeInTx(tx, slice)
				})
			}
			func() {
				defer func() {
					recover()
				}()
				server.Tx(plannerCtx, func(tx server.PgTx) {
					writeInTx(tx, rolledBackSlice)
					panic(laterSliceErr)
				})
			}()
			return []*model.PaymentPlan{firstSlice, secondSlice}, laterSliceErr
		})
		if !errors.Is(err, laterSliceErr) {
			t.Fatalf("send error = %v, want the later slice's error", err)
		}
		if heldCount, dueCount := missingWalletTestMessages(t, ctx, networkId); heldCount != 0 || dueCount != 1 {
			t.Fatalf("after the run: %d held and %d due notices, want one due", heldCount, dueCount)
		}
		if count := outboxMessageCount(t, ctx, smallNetworkId); count != 0 {
			t.Fatalf("a payout below the minimum left %d notices", count)
		}

		sender := newOutboxTestSender()
		deliverAccountMessagesAt(ctx, sender, server.NowUtc())
		deliverAccountMessagesAt(ctx, sender, server.NowUtc().Add(time.Hour))
		sends := sender.sent()
		if len(sends) != 1 || sends[0].userAuth != userAuth {
			t.Fatalf("delivered %d notices (%+v), want one to %s", len(sends), sends, userAuth)
		}
		notice, ok := sends[0].template.(*MissingWalletTemplate)
		wantPaymentId := firstSlice.NetworkPayments[networkId].PaymentId
		if !ok || notice.PaymentId != wantPaymentId || notice.Payout != model.UsdToNanoCents(2) || notice.AmountUsd() != "2.00" {
			t.Fatalf("delivered %+v, want the first slice's payment with 2.00 USDC withheld", sends[0].template)
		}
	})
}

// A run that stops after committing a slice, before its release, loses no
// notice: the delivery task releases it once the hold timeout has passed, and a
// later run's release does not send it again.
func TestMissingWalletNoticeOfAStoppedRunIsReleasedLater(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		defer clientSession.Cancel()
		networkId, userAuth := newMissingWalletTestNetwork(ctx, "synthetic-stopped-run")
		slice := missingWalletTestPlan(map[server.Id]model.NanoCents{
			networkId: model.UsdToNanoCents(3),
		}, nil)
		stopErr := errors.New("synthetic stop after the slice")

		value := recoverPanic(func() {
			sendPaymentsWithPlanner(clientSession, func(
				plannerCtx context.Context,
				maxDuration time.Duration,
				writeInTx func(server.PgTx, *model.PaymentPlan),
				onSlice func(*model.PaymentPlan),
			) ([]*model.PaymentPlan, error) {
				server.Tx(plannerCtx, func(tx server.PgTx) {
					writeInTx(tx, slice)
				})
				// the process stops here, before the run releases its notices
				panic(stopErr)
			})
		})
		if value != stopErr {
			t.Fatalf("run panic = %v, want the stop", value)
		}
		if heldCount, _ := missingWalletTestMessages(t, ctx, networkId); heldCount != 1 {
			t.Fatalf("the stopped run left %d held notices, want 1", heldCount)
		}
		holdTime := outboxHeldCreateTime(t, ctx, networkId)

		// the delivery task's release waits for the hold timeout
		minPayout := missingWalletTestMinPayout(model.UsdToNanoCents(0.01))
		if releasedCount := releaseMissingWalletNotices(ctx, holdTime.Add(-time.Second), minPayout); releasedCount != 0 {
			t.Fatalf("released %d notices before the hold timeout", releasedCount)
		}
		if releasedCount := releaseMissingWalletNotices(ctx, holdTime, minPayout); releasedCount != 1 {
			t.Fatalf("released %d notices after the hold timeout, want 1", releasedCount)
		}

		// a later run releases nothing more
		err := sendPaymentsWithPlanner(clientSession, func(
			context.Context,
			time.Duration,
			func(server.PgTx, *model.PaymentPlan),
			func(*model.PaymentPlan),
		) ([]*model.PaymentPlan, error) {
			return nil, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		sender := newOutboxTestSender()
		deliverAccountMessagesAt(ctx, sender, server.NowUtc())
		if sends := sender.sent(); len(sends) != 1 || sends[0].userAuth != userAuth {
			t.Fatalf("delivered %d notices, want one to %s", len(sends), userAuth)
		}
	})
}

// A later run's own release also picks up what a stopped run held, merged with
// its own slices into one notice.
func TestMissingWalletNoticeOfAStoppedRunJoinsTheNextRun(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		defer clientSession.Cancel()
		networkId, _ := newMissingWalletTestNetwork(ctx, "synthetic-next-run")
		stoppedSlice := missingWalletTestPlan(map[server.Id]model.NanoCents{
			networkId: model.UsdToNanoCents(1.5),
		}, nil)
		nextSlice := missingWalletTestPlan(map[server.Id]model.NanoCents{
			networkId: model.UsdToNanoCents(2.5),
		}, nil)
		server.Tx(ctx, func(tx server.PgTx) {
			holdMissingWalletNoticesInTx(ctx, tx, stoppedSlice)
		})

		err := sendPaymentsWithPlanner(clientSession, func(
			plannerCtx context.Context,
			maxDuration time.Duration,
			writeInTx func(server.PgTx, *model.PaymentPlan),
			onSlice func(*model.PaymentPlan),
		) ([]*model.PaymentPlan, error) {
			server.Tx(plannerCtx, func(tx server.PgTx) {
				writeInTx(tx, nextSlice)
			})
			return []*model.PaymentPlan{nextSlice}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		sender := newOutboxTestSender()
		deliverAccountMessagesAt(ctx, sender, server.NowUtc())
		sends := sender.sent()
		if len(sends) != 1 {
			t.Fatalf("delivered %d notices, want one", len(sends))
		}
		notice, ok := sends[0].template.(*MissingWalletTemplate)
		if !ok || notice.PaymentId != stoppedSlice.NetworkPayments[networkId].PaymentId || notice.Payout != model.UsdToNanoCents(4) {
			t.Fatalf("delivered %+v, want the stopped run's payment with 4.00 USDC withheld", sends[0].template)
		}
	})
}

// The create time of the network's held notice.
func outboxHeldCreateTime(t testing.TB, ctx context.Context, networkId server.Id) (createTime time.Time) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT create_time FROM account_message_outbox WHERE network_id = $1 AND deliver_time IS NULL`,
			networkId,
		).Scan(&createTime))
	})
	return
}
