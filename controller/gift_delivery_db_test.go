package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// The day `days` from today, as a send-on date (00:00 UTC on that date).
func giftDeliveryTestDate(days int) time.Time {
	date := server.NowUtc().AddDate(0, 0, days)
	return time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, time.UTC)
}

// 9:00 in Chicago on the date.
func giftDeliveryTestChicagoMorning(t testing.TB, date time.Time) time.Time {
	t.Helper()
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	return time.Date(date.Year(), date.Month(), date.Day(), 9, 0, 0, 0, chicago).UTC()
}

// A pending order of quantity gifts, keyed to a synthetic Stripe session, as
// the checkout makes one. Delivery "them" sends on sendOn in Chicago.
func giftDeliveryTestPendingOrder(t testing.TB, ctx context.Context, quantity int, delivery string, sendOn time.Time) (order *model.GiftOrder, sessionId string) {
	t.Helper()
	order, sessionId, _ = giftDeliveryTestPendingOrderOn(t, ctx, quantity, delivery, model.GiftRailStripe, sendOn)
	return
}

// A pending order on the rail (a Stripe session or a crypto invoice), its
// purchase event base, and the buyer's order token.
func giftDeliveryTestPendingOrderOn(t testing.TB, ctx context.Context, quantity int, delivery string, rail string, sendOn time.Time) (order *model.GiftOrder, base string, token string) {
	t.Helper()
	order = &model.GiftOrder{
		Quantity:        quantity,
		UnitPriceCents:  model.Gift().UnitPriceCents(quantity),
		TotalCents:      int64(quantity) * model.Gift().UnitPriceCents(quantity),
		Rail:            rail,
		Delivery:        delivery,
		BuyerEmail:      "giver@example.invalid",
		SendTimezone:    "America/Chicago",
		Message:         "Thinking of you",
		Signature:       "Sam",
		Language:        "es",
		BittensorFooter: true,
	}
	if delivery == model.GiftDeliveryThem {
		order.SendOn = &sendOn
		for i := range quantity {
			order.Recipients = append(order.Recipients, fmt.Sprintf("friend%d@example.invalid", i))
		}
	}
	if err := model.CreateGiftOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	token = order.OrderToken
	if rail == model.GiftRailCrypto {
		intentId := server.NewId()
		if err := model.SetGiftOrderCryptoIntentId(ctx, order.GiftOrderId, intentId); err != nil {
			t.Fatal(err)
		}
		base = model.CryptoPaymentPurchaseEventId(intentId)
	} else {
		base = "cs_test_gift_delivery_" + server.NewId().String()
		if err := model.SetGiftOrderStripeSessionId(ctx, order.GiftOrderId, base); err != nil {
			t.Fatal(err)
		}
	}
	return model.GetGiftOrder(ctx, order.GiftOrderId), base, token
}

// Refunds the order as of refundTime.
func giftDeliveryTestRefund(t testing.TB, ctx context.Context, giftOrderId server.Id, refundTime time.Time) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		if err := model.SetGiftOrderRefundedInTx(tx, ctx, giftOrderId); err != nil {
			t.Fatal(err)
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE gift_order SET refunded_time = $2 WHERE gift_order_id = $1`,
			giftOrderId,
			refundTime,
		))
	})
}

// Fulfils the order at paidTime, as its payment does, and stores a synthetic
// PDF for each part the render would print.
func giftDeliveryTestFulfil(t testing.TB, ctx context.Context, giftOrderId server.Id, sessionId string, paidTime time.Time) []*model.GiftCode {
	t.Helper()
	codes := giftDeliveryTestFulfilUnprinted(t, ctx, giftOrderId, sessionId, paidTime)
	_, parts := giftOrderCardDocuments(model.GetGiftOrder(ctx, giftOrderId), codes)
	for _, part := range parts {
		if err := model.PutGiftOrderPdf(ctx, giftOrderId, part, []byte("%PDF-1.7 "+part)); err != nil {
			t.Fatal(err)
		}
	}
	return codes
}

// Fulfils the order at paidTime, its cards not printed yet.
func giftDeliveryTestFulfilUnprinted(t testing.TB, ctx context.Context, giftOrderId server.Id, sessionId string, paidTime time.Time) []*model.GiftCode {
	t.Helper()
	var codes []*model.GiftCode
	server.Tx(ctx, func(tx server.PgTx) {
		var newlyPaid bool
		var err error
		codes, newlyPaid, err = model.FulfillGiftOrderInTx(tx, ctx, giftOrderId, sessionId, model.UsdToNanoCents(1), paidTime)
		if err != nil || !newlyPaid {
			t.Fatalf("fulfil: newly paid %t, %v", newlyPaid, err)
		}
	}, server.TxReadCommitted)
	return codes
}

// A paid order of quantity gifts, fulfilled at paidTime, with a synthetic PDF
// stored for every part. Delivery "them" sends on sendOn in Chicago.
func giftDeliveryTestOrder(t testing.TB, ctx context.Context, quantity int, delivery string, paidTime time.Time, sendOn time.Time) (*model.GiftOrder, []*model.GiftCode) {
	t.Helper()
	order, sessionId := giftDeliveryTestPendingOrder(t, ctx, quantity, delivery, sendOn)
	codes := giftDeliveryTestFulfil(t, ctx, order.GiftOrderId, sessionId, paidTime)
	return model.GetGiftOrder(ctx, order.GiftOrderId), codes
}

// Sets when the order was made.
func giftDeliveryTestMadeAt(t testing.TB, ctx context.Context, giftOrderId server.Id, createTime time.Time) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE gift_order SET create_time = $2 WHERE gift_order_id = $1`,
			giftOrderId,
			createTime,
		))
		if tag.RowsAffected() != 1 {
			t.Fatalf("no gift order %s", giftOrderId)
		}
	})
}

// Runs the render task's post as the task's finish does.
func giftDeliveryTestPost(t testing.TB, ctx context.Context, giftOrderId server.Id) {
	t.Helper()
	clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
	defer clientSession.Cancel()
	server.Tx(ctx, func(tx server.PgTx) {
		if err := GiftRenderCardsPost(
			&GiftRenderCardsArgs{GiftOrderId: giftOrderId},
			&GiftRenderCardsResult{Rendered: true, Parts: []string{model.GiftOrderPdfPartAll}},
			clientSession,
			tx,
		); err != nil {
			t.Fatal(err)
		}
	})
}

// The payment's transaction owes one render of the order; the render's post
// owes the buyer's email now and each friend's at 9:00 Chicago time on the
// buyer's date, once however often it runs, storing only the order's keys.
// Each email reads its order and its own card when it is sent: the buyer's
// every card, each friend's their own, replying to the buyer.
func TestGiftDeliveryOwesAndSendsTheEmails(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer model.Testing_SetGiftConfig(model.Testing_GiftConfig())()

		sendOn := giftDeliveryTestDate(40)
		friendDue := giftDeliveryTestChicagoMorning(t, sendOn)
		order, codes := giftDeliveryTestOrder(t, ctx, 3, model.GiftDeliveryThem, server.NowUtc(), sendOn)

		// the payment's transaction schedules one render; twice is still one
		for range 2 {
			server.Tx(ctx, func(tx server.PgTx) {
				if err := ScheduleGiftDeliveryInTx(tx, ctx, order.GiftOrderId); err != nil {
					t.Fatal(err)
				}
			})
		}
		renderName := task.NewTaskTarget(GiftRenderCards).TargetFunctionName()
		var renderCount int
		var maxTimeSeconds int
		var argsJson string
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT COUNT(*), MAX(run_max_time_seconds), MAX(args_json::text) FROM pending_task WHERE function_name = $1 AND run_once_key = $2`,
				renderName,
				giftRenderCardsRunOnce(order.GiftOrderId).String(),
			).Scan(&renderCount, &maxTimeSeconds, &argsJson))
		})
		if renderCount != 1 || maxTimeSeconds != int(giftRenderCardsMaxTime/time.Second) {
			t.Fatalf("%d pending renders, max time %ds", renderCount, maxTimeSeconds)
		}
		var args GiftRenderCardsArgs
		if err := json.Unmarshal([]byte(argsJson), &args); err != nil || args.GiftOrderId != order.GiftOrderId {
			t.Fatalf("render args %s", argsJson)
		}

		beforePost := server.NowUtc()
		giftDeliveryTestPost(t, ctx, order.GiftOrderId)
		giftDeliveryTestPost(t, ctx, order.GiftOrderId)

		buyer := model.GetAccountMessageByKey(ctx, giftCardsBuyerTemplateName, giftBuyerEmailKey(order.GiftOrderId))
		if buyer == nil || buyer.UserAuth != "giver@example.invalid" || buyer.DeliverTime.Before(beforePost.Add(-time.Second)) || buyer.NetworkId != nil {
			t.Fatalf("buyer email %+v", buyer)
		}
		var buyerFields map[string]any
		if err := json.Unmarshal([]byte(buyer.TemplateJson), &buyerFields); err != nil || len(buyerFields) != 1 || buyerFields["gift_order_id"] != order.GiftOrderId.String() {
			t.Fatalf("buyer email stores %s", buyer.TemplateJson)
		}
		for i := range 3 {
			friend := model.GetAccountMessageByKey(ctx, giftCardFriendTemplateName, giftFriendEmailKey(order.GiftOrderId, i))
			if friend == nil || friend.UserAuth != fmt.Sprintf("friend%d@example.invalid", i) || !friend.DeliverTime.Equal(friendDue) {
				t.Fatalf("friend %d email %+v, want due %s", i, friend, friendDue)
			}
			var fields map[string]any
			if err := json.Unmarshal([]byte(friend.TemplateJson), &fields); err != nil || len(fields) != 2 || fields["recipient_index"] != float64(i) {
				t.Fatalf("friend %d email stores %s", i, friend.TemplateJson)
			}
		}

		// now: the buyer's email, with every card
		sender := newOutboxTestSender()
		deliverAccountMessagesAt(ctx, sender, server.NowUtc())
		sends := sender.sent()
		if len(sends) != 1 || sends[0].userAuth != "giver@example.invalid" {
			t.Fatalf("sent %+v, want the buyer's email", sends)
		}
		buyerTemplate := sends[0].template.(*GiftCardsBuyerTemplate)
		if attachments := buyerTemplate.EmailAttachments(); len(attachments) != 1 || string(attachments[0].Data) != "%PDF-1.7 all" {
			t.Fatalf("the buyer's email carries %+v", attachments)
		}
		subject, _, bodyText, err := RenderEmailTemplate(buyerTemplate)
		if err != nil || subject != "Tus tarjetas de regalo de URnetwork" {
			t.Fatalf("buyer subject %q, %v", subject, err)
		}
		for _, code := range codes {
			if !strings.Contains(bodyText, giftCardCodeGrouped(code.Secret)) {
				t.Fatalf("the buyer's email does not list %s", code.Secret)
			}
		}
		if !strings.Contains(bodyText, giftCardDate("es", sendOn)) {
			t.Fatalf("the buyer's email does not say when the friends get theirs: %s", bodyText)
		}

		// just before 9:00 in Chicago nothing more; after it, each friend's
		// email with their own card
		deliverAccountMessagesAt(ctx, sender, friendDue.Add(-time.Minute))
		if sends := sender.sent(); len(sends) != 1 {
			t.Fatalf("sent %d emails before the friends' time", len(sends))
		}
		deliverAccountMessagesAt(ctx, sender, friendDue.Add(time.Minute))
		sends = sender.sent()
		if len(sends) != 4 {
			t.Fatalf("sent %d emails, want 4", len(sends))
		}
		for _, send := range sends[1:] {
			friendTemplate := send.template.(*GiftCardFriendTemplate)
			i := friendTemplate.RecipientIndex
			if send.userAuth != fmt.Sprintf("friend%d@example.invalid", i) || friendTemplate.ReplyTo() != "giver@example.invalid" {
				t.Fatalf("friend %d email to %s replying to %s", i, send.userAuth, friendTemplate.ReplyTo())
			}
			if attachments := friendTemplate.EmailAttachments(); len(attachments) != 1 || string(attachments[0].Data) != fmt.Sprintf("%%PDF-1.7 r%d", i) {
				t.Fatalf("friend %d's email carries %+v", i, attachments)
			}
			if friendTemplate.Code() != giftCardCodeGrouped(codes[i].Secret) {
				t.Fatalf("friend %d's email has code %s", i, friendTemplate.Code())
			}
			subject, _, _, err := RenderEmailTemplate(friendTemplate)
			if err != nil || subject != "Sam te ha enviado un regalo de privacidad" {
				t.Fatalf("friend subject %q, %v", subject, err)
			}
		}
	})
}

// An order refunded before its date sends its friends nothing: each email is
// withdrawn when it comes due, not retried. A render of an unpaid order prints
// nothing and owes nothing.
func TestGiftDeliveryWithdrawsTheEmailsOfARefundedOrder(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer model.Testing_SetGiftConfig(model.Testing_GiftConfig())()

		sendOn := giftDeliveryTestDate(40)
		order, _ := giftDeliveryTestOrder(t, ctx, 2, model.GiftDeliveryThem, server.NowUtc(), sendOn)
		giftDeliveryTestPost(t, ctx, order.GiftOrderId)
		server.Tx(ctx, func(tx server.PgTx) {
			if err := model.SetGiftOrderRefundedInTx(tx, ctx, order.GiftOrderId); err != nil {
				t.Fatal(err)
			}
		})

		sender := newOutboxTestSender()
		deliverAccountMessagesAt(ctx, sender, sendOn.AddDate(0, 0, 1))
		if sends := sender.sent(); len(sends) != 0 {
			t.Fatalf("a refunded order sent %d emails", len(sends))
		}
		for i := range 2 {
			friend := model.GetAccountMessageByKey(ctx, giftCardFriendTemplateName, giftFriendEmailKey(order.GiftOrderId, i))
			if friend == nil || friend.AbandonTime == nil || friend.AttemptCount != 1 || !strings.Contains(friend.LastError, "refunded") {
				t.Fatalf("friend %d email %+v, want it withdrawn on its first attempt", i, friend)
			}
		}
		buyer := model.GetAccountMessageByKey(ctx, giftCardsBuyerTemplateName, giftBuyerEmailKey(order.GiftOrderId))
		if buyer == nil || buyer.AbandonTime == nil {
			t.Fatalf("buyer email %+v, want it withdrawn", buyer)
		}

		// the render of an order that is no longer paid, or of no order
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		result, err := GiftRenderCards(&GiftRenderCardsArgs{GiftOrderId: order.GiftOrderId}, clientSession)
		if err != nil || result.Rendered {
			t.Fatalf("render of a refunded order: %+v, %v", result, err)
		}
		result, err = GiftRenderCards(&GiftRenderCardsArgs{GiftOrderId: server.NewId()}, clientSession)
		if err != nil || result.Rendered {
			t.Fatalf("render of no order: %+v, %v", result, err)
		}
	})
}

// Sets when an email went out, as if delivered then.
func giftRetentionTestSent(t testing.TB, ctx context.Context, templateName string, key string, sentTime time.Time) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE account_message_outbox SET sent_time = $2 WHERE message_key = $1`,
			model.AccountMessageKey(templateName, key),
			sentTime,
		))
		if tag.RowsAffected() != 1 {
			t.Fatalf("no outbox message %s %s", templateName, key)
		}
	})
}

// Removes an email's outbox row, as the outbox does a week after a finished
// message was added.
func giftRetentionTestRemoveMessage(t testing.TB, ctx context.Context, templateName string, key string) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM account_message_outbox WHERE message_key = $1`,
			model.AccountMessageKey(templateName, key),
		))
	})
}

// The retention removes an order's cards, note, signature and friends'
// addresses once its last email is 30 days old, and keeps every other order
// whole: one paid recently, one whose friend's email went out last week, one
// whose friend's email is still due. The codes and the buyer stay.
func TestGiftRetention(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer model.Testing_SetGiftConfig(model.Testing_GiftConfig())()

		// the retention runs four months from now; the orders were paid two
		// months before that, and their friends' date three weeks after
		now := giftDeliveryTestDate(120).Add(12 * time.Hour)
		paidTime := now.Add(-60 * 24 * time.Hour)
		sendOn := giftDeliveryTestDate(81)
		friendTime := giftDeliveryTestChicagoMorning(t, sendOn)

		// reaped: the buyer's email went out when it was paid
		reaped, _ := giftDeliveryTestOrder(t, ctx, 1, model.GiftDeliveryMe, paidTime, sendOn)
		giftDeliveryTestPost(t, ctx, reaped.GiftOrderId)
		giftRetentionTestSent(t, ctx, giftCardsBuyerTemplateName, giftBuyerEmailKey(reaped.GiftOrderId), paidTime.Add(time.Minute))

		// reaped: friends on their date, every row since removed by the outbox
		reapedThem, _ := giftDeliveryTestOrder(t, ctx, 2, model.GiftDeliveryThem, paidTime, sendOn)
		giftDeliveryTestPost(t, ctx, reapedThem.GiftOrderId)
		giftRetentionTestRemoveMessage(t, ctx, giftCardsBuyerTemplateName, giftBuyerEmailKey(reapedThem.GiftOrderId))
		for i := range 2 {
			giftRetentionTestRemoveMessage(t, ctx, giftCardFriendTemplateName, giftFriendEmailKey(reapedThem.GiftOrderId, i))
		}

		// kept: paid 20 days before the run
		recent, _ := giftDeliveryTestOrder(t, ctx, 1, model.GiftDeliveryMe, now.Add(-20*24*time.Hour), sendOn)
		giftDeliveryTestPost(t, ctx, recent.GiftOrderId)
		giftRetentionTestSent(t, ctx, giftCardsBuyerTemplateName, giftBuyerEmailKey(recent.GiftOrderId), now.Add(-20*24*time.Hour))

		// kept: a friend's email went out a week before the run (retried late)
		lateFriend, _ := giftDeliveryTestOrder(t, ctx, 2, model.GiftDeliveryThem, paidTime, sendOn)
		giftDeliveryTestPost(t, ctx, lateFriend.GiftOrderId)
		giftRetentionTestSent(t, ctx, giftCardsBuyerTemplateName, giftBuyerEmailKey(lateFriend.GiftOrderId), paidTime)
		giftRetentionTestSent(t, ctx, giftCardFriendTemplateName, giftFriendEmailKey(lateFriend.GiftOrderId, 0), friendTime)
		giftRetentionTestSent(t, ctx, giftCardFriendTemplateName, giftFriendEmailKey(lateFriend.GiftOrderId, 1), now.Add(-7*24*time.Hour))

		// kept: a friend's email is still due
		pending, _ := giftDeliveryTestOrder(t, ctx, 2, model.GiftDeliveryThem, paidTime, sendOn)
		giftDeliveryTestPost(t, ctx, pending.GiftOrderId)
		giftRetentionTestSent(t, ctx, giftCardsBuyerTemplateName, giftBuyerEmailKey(pending.GiftOrderId), paidTime)
		giftRetentionTestSent(t, ctx, giftCardFriendTemplateName, giftFriendEmailKey(pending.GiftOrderId, 0), friendTime)

		checkReaped := func(order *model.GiftOrder) {
			t.Helper()
			after := model.GetGiftOrder(ctx, order.GiftOrderId)
			if after.Message != "" || after.Signature != "" || after.PdfReady || after.BuyerEmail != "giver@example.invalid" {
				t.Fatalf("after retention %+v", after)
			}
			for _, recipient := range after.Recipients {
				if recipient != "" {
					t.Fatalf("a friend's address outlived the retention: %q", recipient)
				}
			}
			if model.GetGiftOrderPdf(ctx, order.GiftOrderId, model.GiftOrderPdfPartAll) != nil {
				t.Fatal("the cards outlived the retention")
			}
			if order.Delivery == model.GiftDeliveryThem && model.GetGiftOrderPdf(ctx, order.GiftOrderId, model.GiftOrderPdfRecipientPart(1)) != nil {
				t.Fatal("a friend's card outlived the retention")
			}
			if len(model.GetGiftOrderCodes(ctx, order.GiftOrderId)) != order.Quantity {
				t.Fatal("the retention removed codes")
			}
		}
		checkKept := func(order *model.GiftOrder) {
			t.Helper()
			after := model.GetGiftOrder(ctx, order.GiftOrderId)
			if after.Message != "Thinking of you" || after.Signature != "Sam" || !after.PdfReady {
				t.Fatalf("the retention removed a kept order's data: %+v", after)
			}
		}

		result, err := runGiftRetention(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		if result.RemovedCount != 2 || result.KeptCount != 2 {
			t.Fatalf("retention %+v, want 2 orders removed and 2 kept", result)
		}
		checkReaped(reaped)
		checkReaped(reapedThem)
		checkKept(recent)
		checkKept(lateFriend)
		checkKept(pending)

		// a second run removes nothing more
		result, err = runGiftRetention(ctx, now)
		if err != nil || result.RemovedCount != 0 {
			t.Fatalf("second retention %+v, %v", result, err)
		}
		// a month on, the recent order and the late friend's go too; the one
		// still due stays
		result, err = runGiftRetention(ctx, now.Add(30*24*time.Hour))
		if err != nil || result.RemovedCount != 2 {
			t.Fatalf("retention a month on %+v, %v", result, err)
		}
		checkReaped(recent)
		checkReaped(lateFriend)
		checkKept(pending)
	})
}

// The retention task schedules its next day's run.
func TestGiftRetentionTaskSchedulesTheNextRun(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		result, err := GiftRetention(&GiftRetentionArgs{}, clientSession)
		if err != nil {
			t.Fatal(err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			if err := GiftRetentionPost(&GiftRetentionArgs{}, result, clientSession, tx); err != nil {
				t.Fatal(err)
			}
		})
		var runAt time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT run_at FROM pending_task WHERE run_once_key = $1`,
				task.RunOnce("gift_retention").String(),
			).Scan(&runAt))
		})
		if want := NextGiftRetentionAt(server.NowUtc()); !runAt.Equal(want) {
			t.Fatalf("next retention at %s, want %s", runAt, want)
		}
	})
}

// The never-paid orders' retention: an order still pending 30 days after it
// was made, or refunded 30 days ago before it was paid, loses its note,
// signature and friends' addresses; a pending card order (a Stripe session is
// never paid late) and an order of either rail refunded before it was paid
// lose their buyer's address too, a pending crypto order keeps it; one 29 days
// in keeps everything. A paid order refunded before its cards were printed
// owed no email and goes 30 days after its refund, keeping its buyer. A paid
// order made long ago keeps its data until 30 days after its last email. A
// crypto order paid late after its scrub still gets its codes, and only its
// buyer is emailed, with every card and without the promise of the friends'
// emails.
func TestGiftRetentionScrubsUnpaidOrders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer model.Testing_SetGiftConfig(model.Testing_GiftConfig())()

		now := server.NowUtc()
		days := func(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }
		sendOn := giftDeliveryTestDate(10)

		unpaidCoin, unpaidCoinBase, _ := giftDeliveryTestPendingOrderOn(t, ctx, 2, model.GiftDeliveryThem, model.GiftRailCrypto, sendOn)
		giftDeliveryTestMadeAt(t, ctx, unpaidCoin.GiftOrderId, now.Add(-days(31)))
		unpaidCard, _, _ := giftDeliveryTestPendingOrderOn(t, ctx, 1, model.GiftDeliveryMe, model.GiftRailStripe, sendOn)
		giftDeliveryTestMadeAt(t, ctx, unpaidCard.GiftOrderId, now.Add(-days(31)))
		recentUnpaid, _ := giftDeliveryTestPendingOrder(t, ctx, 2, model.GiftDeliveryThem, sendOn)
		giftDeliveryTestMadeAt(t, ctx, recentUnpaid.GiftOrderId, now.Add(-days(29)))

		refundedUnpaid, _ := giftDeliveryTestPendingOrder(t, ctx, 2, model.GiftDeliveryThem, sendOn)
		giftDeliveryTestMadeAt(t, ctx, refundedUnpaid.GiftOrderId, now.Add(-days(40)))
		giftDeliveryTestRefund(t, ctx, refundedUnpaid.GiftOrderId, now.Add(-days(31)))
		recentlyRefundedUnpaid, _ := giftDeliveryTestPendingOrder(t, ctx, 2, model.GiftDeliveryThem, sendOn)
		giftDeliveryTestMadeAt(t, ctx, recentlyRefundedUnpaid.GiftOrderId, now.Add(-days(40)))
		giftDeliveryTestRefund(t, ctx, recentlyRefundedUnpaid.GiftOrderId, now.Add(-days(29)))
		// a crypto order refunded before it was paid, its buyer's address the
		// only personal data it holds
		refundedUnpaidCoin, _, _ := giftDeliveryTestPendingOrderOn(t, ctx, 1, model.GiftDeliveryMe, model.GiftRailCrypto, sendOn)
		giftDeliveryTestMadeAt(t, ctx, refundedUnpaidCoin.GiftOrderId, now.Add(-days(40)))
		giftDeliveryTestRefund(t, ctx, refundedUnpaidCoin.GiftOrderId, now.Add(-days(31)))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE gift_order SET message = '', signature = '' WHERE gift_order_id = $1`, refundedUnpaidCoin.GiftOrderId))
		})

		refundedUnprinted, refundedUnprintedBase := giftDeliveryTestPendingOrder(t, ctx, 2, model.GiftDeliveryThem, sendOn)
		giftDeliveryTestFulfilUnprinted(t, ctx, refundedUnprinted.GiftOrderId, refundedUnprintedBase, now.Add(-days(45)))
		giftDeliveryTestRefund(t, ctx, refundedUnprinted.GiftOrderId, now.Add(-days(31)))
		recentlyRefundedUnprinted, recentlyRefundedUnprintedBase := giftDeliveryTestPendingOrder(t, ctx, 1, model.GiftDeliveryMe, sendOn)
		giftDeliveryTestFulfilUnprinted(t, ctx, recentlyRefundedUnprinted.GiftOrderId, recentlyRefundedUnprintedBase, now.Add(-days(45)))
		giftDeliveryTestRefund(t, ctx, recentlyRefundedUnprinted.GiftOrderId, now.Add(-days(29)))
		// the refunded orders as the retention finds them (refunded)
		refundedUnpaid = model.GetGiftOrder(ctx, refundedUnpaid.GiftOrderId)
		recentlyRefundedUnpaid = model.GetGiftOrder(ctx, recentlyRefundedUnpaid.GiftOrderId)
		refundedUnpaidCoin = model.GetGiftOrder(ctx, refundedUnpaidCoin.GiftOrderId)
		refundedUnprinted = model.GetGiftOrder(ctx, refundedUnprinted.GiftOrderId)
		recentlyRefundedUnprinted = model.GetGiftOrder(ctx, recentlyRefundedUnprinted.GiftOrderId)

		// made 60 days ago, paid and emailed 10 days ago
		paid, _ := giftDeliveryTestOrder(t, ctx, 1, model.GiftDeliveryMe, now.Add(-days(10)), sendOn)
		giftDeliveryTestMadeAt(t, ctx, paid.GiftOrderId, now.Add(-days(60)))
		giftDeliveryTestPost(t, ctx, paid.GiftOrderId)
		giftRetentionTestSent(t, ctx, giftCardsBuyerTemplateName, giftBuyerEmailKey(paid.GiftOrderId), now.Add(-days(10)))

		checkWhole := func(order *model.GiftOrder) {
			t.Helper()
			after := model.GetGiftOrder(ctx, order.GiftOrderId)
			if after.Message != "Thinking of you" || after.Signature != "Sam" || after.BuyerEmail != "giver@example.invalid" {
				t.Fatalf("the retention changed a kept order: %+v", after)
			}
			for i, recipient := range after.Recipients {
				if recipient != order.Recipients[i] {
					t.Fatalf("the retention changed a kept order's friend %d: %q", i, recipient)
				}
			}
		}
		checkScrubbed := func(order *model.GiftOrder, buyerEmail string) {
			t.Helper()
			after := model.GetGiftOrder(ctx, order.GiftOrderId)
			if after.Message != "" || after.Signature != "" || after.BuyerEmail != buyerEmail || after.Status != order.Status {
				t.Fatalf("after retention %+v, want it scrubbed with buyer %q", after, buyerEmail)
			}
			for i, recipient := range after.Recipients {
				if recipient != "" {
					t.Fatalf("friend %d's address %q outlived the retention", i, recipient)
				}
			}
		}

		result, err := runGiftRetention(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		if result.UnpaidRemovedCount != 4 || result.RemovedCount != 1 || result.KeptCount != 1 {
			t.Fatalf("retention %+v, want 4 never-paid orders and 1 refunded unprinted one removed, 1 kept", result)
		}
		checkScrubbed(unpaidCoin, "giver@example.invalid")
		checkScrubbed(unpaidCard, "")
		checkScrubbed(refundedUnpaid, "")
		checkScrubbed(refundedUnpaidCoin, "")
		checkScrubbed(refundedUnprinted, "giver@example.invalid")
		checkWhole(recentUnpaid)
		checkWhole(recentlyRefundedUnpaid)
		checkWhole(recentlyRefundedUnprinted)
		checkWhole(paid)
		if result, err := runGiftRetention(ctx, now); err != nil || result.UnpaidRemovedCount != 0 || result.RemovedCount != 0 {
			t.Fatalf("second retention %+v, %v", result, err)
		}

		// a late coin payment of the scrubbed order: its codes, every card, and
		// only the buyer's email
		codes := giftDeliveryTestFulfil(t, ctx, unpaidCoin.GiftOrderId, unpaidCoinBase, now)
		if len(codes) != 2 {
			t.Fatalf("the late payment made %d codes", len(codes))
		}
		paidLate := model.GetGiftOrder(ctx, unpaidCoin.GiftOrderId)
		if _, parts := giftOrderCardDocuments(paidLate, codes); len(parts) != 1 || parts[0] != model.GiftOrderPdfPartAll {
			t.Fatalf("the late order prints %v, want every card only", parts)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			if err := ScheduleGiftDeliveryInTx(tx, ctx, unpaidCoin.GiftOrderId); err != nil {
				t.Fatal(err)
			}
		})
		giftDeliveryTestPost(t, ctx, unpaidCoin.GiftOrderId)
		for i := range 2 {
			if friend := model.GetAccountMessageByKey(ctx, giftCardFriendTemplateName, giftFriendEmailKey(unpaidCoin.GiftOrderId, i)); friend != nil {
				t.Fatalf("the late order owes friend %d an email", i)
			}
		}
		sender := newOutboxTestSender()
		deliverAccountMessagesAt(ctx, sender, sendOn.AddDate(0, 0, 1))
		sends := sender.sent()
		if len(sends) != 1 || sends[0].userAuth != "giver@example.invalid" {
			t.Fatalf("the late order sent %+v, want the buyer's email alone", sends)
		}
		buyerTemplate := sends[0].template.(*GiftCardsBuyerTemplate)
		if buyerTemplate.Them() || len(buyerTemplate.EmailAttachments()) != 1 {
			t.Fatal("the late order's buyer email promises the friends' emails or has no cards")
		}
		_, _, bodyText, err := RenderEmailTemplate(buyerTemplate)
		if err != nil || !strings.Contains(bodyText, giftCardText("es", "buyer.me.other")) || strings.Contains(bodyText, giftCardText("es", "buyer.them_copy.other")) {
			t.Fatalf("the late order's buyer email reads %q, %v", bodyText, err)
		}

		// a month on (the fixtures' cards were stored today): the paid order 30
		// days after its email, the order refunded unprinted 29 days ago, and
		// both never-paid ones still whole; the late order keeps its cards (its
		// friends' date is still after the cutoff, so it is not even listed)
		result, err = runGiftRetention(ctx, now.Add(days(31)))
		if err != nil || result.RemovedCount != 2 || result.UnpaidRemovedCount != 2 || result.KeptCount != 0 {
			t.Fatalf("retention a month on %+v, %v", result, err)
		}
		if after := model.GetGiftOrder(ctx, paid.GiftOrderId); after.Message != "" || after.PdfReady {
			t.Fatalf("the paid order after its month: %+v", after)
		}
		checkScrubbed(recentUnpaid, "")
		checkScrubbed(recentlyRefundedUnpaid, "")
		checkScrubbed(recentlyRefundedUnprinted, "giver@example.invalid")
		if !model.GetGiftOrder(ctx, unpaidCoin.GiftOrderId).PdfReady {
			t.Fatal("the late order lost its cards before a month after its email")
		}
		// and 30 days after the late order's email, its cards go too
		result, err = runGiftRetention(ctx, now.Add(days(42)))
		if err != nil || result.RemovedCount != 1 || result.UnpaidRemovedCount != 0 {
			t.Fatalf("retention after the late order's month %+v, %v", result, err)
		}
		if model.GetGiftOrder(ctx, unpaidCoin.GiftOrderId).PdfReady || len(model.GetGiftOrderCodes(ctx, unpaidCoin.GiftOrderId)) != 2 {
			t.Fatal("the late order kept its cards, or lost its codes")
		}
	})
}

// The order's token routes: cards.pdf answers until the retention removes the
// cards and then 404s, while the status still answers; past 400 days from the
// order neither answers. The token is the buyer's, never a stored value.
func TestGiftOrderTokenRoutesAfterRetention(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer model.Testing_SetGiftConfig(model.Testing_GiftConfig())()

		order, base, token := giftDeliveryTestPendingOrderOn(t, ctx, 1, model.GiftDeliveryMe, model.GiftRailStripe, giftDeliveryTestDate(10))
		giftDeliveryTestFulfil(t, ctx, order.GiftOrderId, base, server.NowUtc())
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		args := &GiftOrderStatusArgs{OrderId: order.GiftOrderId.String(), Token: token}

		pdf, err := GiftCardsPdf(args, clientSession)
		if err != nil || string(pdf.Pdf) != "%PDF-1.7 all" {
			t.Fatalf("cards.pdf before the retention: %v", err)
		}
		model.RemoveGiftOrderPersonalData(ctx, order.GiftOrderId)
		if _, err := GiftCardsPdf(args, clientSession); err == nil || err != errGiftOrderNotFound {
			t.Fatalf("cards.pdf after the retention: %v, want 404", err)
		}
		status, err := GiftOrderStatus(args, clientSession)
		if err != nil || status.PdfReady || status.Status != model.GiftOrderStatusPaid {
			t.Fatalf("status after the retention: %+v, %v", status, err)
		}

		giftDeliveryTestMadeAt(t, ctx, order.GiftOrderId, server.NowUtc().Add(-401*24*time.Hour))
		if _, err := GiftOrderStatus(args, clientSession); err != errGiftOrderNotFound {
			t.Fatalf("status after 400 days: %v, want 404", err)
		}
		if _, err := GiftCardsPdf(args, clientSession); err != errGiftOrderNotFound {
			t.Fatalf("cards.pdf after 400 days: %v, want 404", err)
		}
	})
}

// A friend's card whose code expired before its email is never sent: the email
// is withdrawn, and the operator gets an event per card.
func TestGiftFriendEmailRefusesAnExpiredCode(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer model.Testing_SetGiftConfig(model.Testing_GiftConfig())()

		sendOn := giftDeliveryTestDate(40)
		friendDue := giftDeliveryTestChicagoMorning(t, sendOn)
		order, _ := giftDeliveryTestOrder(t, ctx, 2, model.GiftDeliveryThem, server.NowUtc(), sendOn)
		giftDeliveryTestPost(t, ctx, order.GiftOrderId)
		// as if the window had ended before the delivery
		expired := server.NowUtc().Add(-time.Hour)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance_code SET end_time = $2 WHERE gift_order_id = $1`, order.GiftOrderId, expired))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE gift_order SET redeem_by = $2 WHERE gift_order_id = $1`, order.GiftOrderId, expired))
		})

		sender := newOutboxTestSender()
		deliverAccountMessagesAt(ctx, sender, friendDue.Add(time.Minute))
		for _, send := range sender.sent() {
			if _, ok := send.template.(*GiftCardFriendTemplate); ok {
				t.Fatalf("a friend was sent an expired code: %+v", send)
			}
		}
		for i := range 2 {
			friend := model.GetAccountMessageByKey(ctx, giftCardFriendTemplateName, giftFriendEmailKey(order.GiftOrderId, i))
			if friend == nil || friend.AbandonTime == nil || friend.AttemptCount != 1 || !strings.Contains(friend.LastError, "expired") {
				t.Fatalf("friend %d email %+v, want it withdrawn as expired", i, friend)
			}
		}
		var eventCount int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT COUNT(*) FROM payment_reconciliation_event WHERE store = $1 AND action = $2 AND evidence = $3`,
				model.SubscriptionMarketGift,
				model.PaymentReconcileActionError,
				order.GiftOrderId.String(),
			).Scan(&eventCount))
		})
		if eventCount != 2 {
			t.Fatalf("%d operator events, want one per expired card", eventCount)
		}
	})
}

// Forgets the order's crypto invoice, as the 30-day unlink does to an order
// whose friends' emails are still owed (model.UnlinkCryptoPaymentIntents keeps
// its buyer's address then).
func giftDeliveryTestUnlink(t testing.TB, ctx context.Context, giftOrderId server.Id) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE gift_order SET crypto_intent_id = NULL WHERE gift_order_id = $1 AND rail = $2 AND crypto_intent_id IS NOT NULL`,
			giftOrderId,
			model.GiftRailCrypto,
		))
		if tag.RowsAffected() != 1 {
			t.Fatalf("no linked crypto gift order %s", giftOrderId)
		}
	})
}

// A crypto order the 30-day unlink passed while its friends' emails were owed
// keeps its buyer's address, their reply-to, until the last of them is sent or
// given up, then forgets it; an order still linked to its invoice keeps it.
// The daily retention forgets it for an order whose last friend's email went
// out without that, keeps it while a friend's email is owed, and never looks
// at a crypto order made since the cutoff (one whose invoice is not linked
// yet).
func TestGiftUnlinkedCryptoOrderForgetsItsBuyerAfterTheFriends(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer model.Testing_SetGiftConfig(model.Testing_GiftConfig())()

		now := server.NowUtc()
		sendOn := giftDeliveryTestDate(40)
		friendDue := giftDeliveryTestChicagoMorning(t, sendOn)
		// a paid crypto order of two friends' cards, printed, its emails owed
		cryptoThem := func() *model.GiftOrder {
			t.Helper()
			order, base, _ := giftDeliveryTestPendingOrderOn(t, ctx, 2, model.GiftDeliveryThem, model.GiftRailCrypto, sendOn)
			giftDeliveryTestFulfil(t, ctx, order.GiftOrderId, base, now)
			giftDeliveryTestPost(t, ctx, order.GiftOrderId)
			return order
		}
		buyerEmail := func(order *model.GiftOrder) string {
			t.Helper()
			return model.GetGiftOrder(ctx, order.GiftOrderId).BuyerEmail
		}

		unlinked := cryptoThem()
		giftDeliveryTestUnlink(t, ctx, unlinked.GiftOrderId)
		unlinkedRefunded := cryptoThem()
		giftDeliveryTestUnlink(t, ctx, unlinkedRefunded.GiftOrderId)
		giftDeliveryTestRefund(t, ctx, unlinkedRefunded.GiftOrderId, now)
		linked := cryptoThem()

		// the buyers' emails go now; the friends' are still owed
		sender := newOutboxTestSender()
		deliverAccountMessagesAt(ctx, sender, now)
		for _, order := range []*model.GiftOrder{unlinked, unlinkedRefunded, linked} {
			if email := buyerEmail(order); email != "giver@example.invalid" {
				t.Fatalf("order %s forgot its buyer before its friends' emails: %q", order.GiftOrderId, email)
			}
		}

		// the friends' emails: sent, every one replying to the buyer, or
		// withdrawn for the refunded order
		deliverAccountMessagesAt(ctx, sender, friendDue.Add(time.Minute))
		unlinkedFriendCount := 0
		for _, send := range sender.sent() {
			friendTemplate, ok := send.template.(*GiftCardFriendTemplate)
			if !ok {
				continue
			}
			if friendTemplate.GiftOrderId == unlinkedRefunded.GiftOrderId {
				t.Fatalf("the refunded order sent friend %d an email", friendTemplate.RecipientIndex)
			}
			if friendTemplate.ReplyTo() != "giver@example.invalid" {
				t.Fatalf("order %s friend %d's email replies to %q", friendTemplate.GiftOrderId, friendTemplate.RecipientIndex, friendTemplate.ReplyTo())
			}
			if friendTemplate.GiftOrderId == unlinked.GiftOrderId {
				unlinkedFriendCount += 1
			}
		}
		if unlinkedFriendCount != 2 {
			t.Fatalf("the unlinked order sent %d friends' emails, want 2", unlinkedFriendCount)
		}
		if email := buyerEmail(unlinked); email != "" {
			t.Fatalf("the unlinked order kept its buyer %q after its friends' emails", email)
		}
		if email := buyerEmail(unlinkedRefunded); email != "" {
			t.Fatalf("the unlinked refunded order kept its buyer %q after its friends' emails were withdrawn", email)
		}
		if email := buyerEmail(linked); email != "giver@example.invalid" {
			t.Fatalf("the linked order lost its buyer: %q", email)
		}

		// the daily retention: an unlinked order whose friends' emails went
		// out without forgetting its buyer, one whose are still owed, and a
		// crypto order made just now, its invoice not linked yet
		days := func(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }
		missed := cryptoThem()
		giftDeliveryTestUnlink(t, ctx, missed.GiftOrderId)
		giftDeliveryTestMadeAt(t, ctx, missed.GiftOrderId, now.Add(-days(31)))
		for i := range 2 {
			giftRetentionTestSent(t, ctx, giftCardFriendTemplateName, giftFriendEmailKey(missed.GiftOrderId, i), now)
		}
		owing := cryptoThem()
		giftDeliveryTestUnlink(t, ctx, owing.GiftOrderId)
		giftDeliveryTestMadeAt(t, ctx, owing.GiftOrderId, now.Add(-days(31)))
		unlinkedYet := &model.GiftOrder{
			Quantity:       1,
			UnitPriceCents: model.Gift().UnitPriceCents(1),
			TotalCents:     model.Gift().UnitPriceCents(1),
			Rail:           model.GiftRailCrypto,
			Delivery:       model.GiftDeliveryMe,
			BuyerEmail:     "giver@example.invalid",
			Language:       "en",
		}
		if err := model.CreateGiftOrder(ctx, unlinkedYet); err != nil {
			t.Fatal(err)
		}

		result, err := runGiftRetention(ctx, now)
		if err != nil || result.UnlinkedBuyerEmailClearedCount != 1 || result.RemovedCount != 0 || result.UnpaidRemovedCount != 0 {
			t.Fatalf("retention %+v, %v, want one unlinked buyer forgotten", result, err)
		}
		if email := buyerEmail(missed); email != "" {
			t.Fatalf("the retention kept the missed order's buyer %q", email)
		}
		if email := buyerEmail(owing); email != "giver@example.invalid" {
			t.Fatalf("the retention forgot a buyer whose friends' emails are owed: %q", email)
		}
		if email := buyerEmail(unlinkedYet); email != "giver@example.invalid" {
			t.Fatalf("the retention forgot the buyer of an order made just now: %q", email)
		}
		if result, err := runGiftRetention(ctx, now); err != nil || result.UnlinkedBuyerEmailClearedCount != 0 {
			t.Fatalf("second retention %+v, %v", result, err)
		}
	})
}
