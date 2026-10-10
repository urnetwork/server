// The payment notices are owed exactly when their payment's grant or credit
// commits: the x402 receipt with the grant of its settle transaction, the
// Solana data pack's applied note with the credit of its intent. A deferred
// constraint trigger on the outbox fails the commit of the transaction that
// adds the notice, after everything in it ran, so a grant or credit that rolls
// back is shown to leave no notice behind and a retry to owe exactly one.
package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// Fails, at commit, every transaction that adds an account message for the
// network, until the returned function removes the fault.
func failOutboxCommitsForNetwork(t testing.TB, ctx context.Context, networkId server.Id) (remove func()) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`
			CREATE FUNCTION synthetic_outbox_commit_fault() RETURNS trigger AS $fault$
			BEGIN
				RAISE EXCEPTION 'synthetic failure at commit';
			END
			$fault$ LANGUAGE plpgsql;
			CREATE CONSTRAINT TRIGGER synthetic_outbox_commit_fault
				AFTER INSERT ON account_message_outbox
				DEFERRABLE INITIALLY DEFERRED
				FOR EACH ROW WHEN (NEW.network_id = '%s')
				EXECUTE FUNCTION synthetic_outbox_commit_fault();
		`, networkId)))
	})
	return func() {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
				DROP TRIGGER synthetic_outbox_commit_fault ON account_message_outbox;
				DROP FUNCTION synthetic_outbox_commit_fault();
			`))
		})
	}
}

// Runs `f` and returns what it panicked with.
func recoverPanic(f func()) (value any) {
	defer func() {
		value = recover()
	}()
	f()
	return
}

// A data purchase over x402 with an email owes one receipt with its grant, to
// the normalized address, with what was bought; a retry of the same settle
// transaction grants nothing and owes nothing more. A Pro month purchase owes
// its own receipt.
func TestX402GrantOwesOneReceiptWithTheGrant(t *testing.T) {
	skipWithoutProYml(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "x402receipt", server.NewId())
		dataSku := &X402Sku{
			SkuId:       "data_synthetic",
			Description: "Synthetic data pack",
			PriceUsd:    5,
			ByteCount:   model.Gib,
		}
		settle := &X402SettleResponse{Success: true, Transaction: "0xsyntheticreceipt1", Network: "base"}

		err := x402GrantSettled(ctx, networkId, dataSku, settle, &x402Receipt{email: " Agent@Synthetic.Example ", asset: "USDC"})
		connect.AssertEqual(t, err, nil)
		err = x402GrantSettled(ctx, networkId, dataSku, settle, &x402Receipt{email: "agent@synthetic.example", asset: "USDC"})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 1)
		connect.AssertEqual(t, outboxMessageCount(t, ctx, networkId), 1)
		receipt := model.GetAccountMessageByKey(ctx, "x402_receipt", settle.Transaction)
		if receipt == nil || receipt.UserAuth != "agent@synthetic.example" || receipt.DeliverTime == nil {
			t.Fatalf("receipt = %+v, want due for the normalized address", receipt)
		}

		proSku := &X402Sku{
			SkuId:       X402SkuProMonth,
			Description: "Synthetic Pro month",
			PriceUsd:    5,
			Pro:         true,
			ByteCount:   model.Pro().DataAmount(true),
		}
		proSettle := &X402SettleResponse{Success: true, Transaction: "0xsyntheticreceipt2", Network: "base"}
		err = x402GrantSettled(ctx, networkId, proSku, proSettle, &x402Receipt{email: "agent@synthetic.example", asset: "USDC"})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, outboxMessageCount(t, ctx, networkId), 2)

		sender := newOutboxTestSender()
		deliverAccountMessagesAt(ctx, sender, server.NowUtc())
		sends := sender.sent()
		if len(sends) != 2 {
			t.Fatalf("delivered %d receipts, want 2", len(sends))
		}
		transactions := map[string]*X402ReceiptTemplate{}
		for _, send := range sends {
			template, ok := send.template.(*X402ReceiptTemplate)
			if !ok || send.userAuth != "agent@synthetic.example" {
				t.Fatalf("delivered %T to %q, want an x402 receipt to the agent's address", send.template, send.userAuth)
			}
			transactions[template.Transaction] = template
		}
		dataReceipt := transactions[settle.Transaction]
		if dataReceipt == nil || dataReceipt.Description != dataSku.Description || dataReceipt.PriceUsd != 5 ||
			dataReceipt.Network != "base" || dataReceipt.Pro || dataReceipt.BalanceByteCount != model.Gib ||
			dataReceipt.Asset != "USDC" {
			t.Fatalf("data receipt = %+v, want what was bought", dataReceipt)
		}
		if proReceipt := transactions[proSettle.Transaction]; proReceipt == nil || !proReceipt.Pro {
			t.Fatalf("Pro receipt = %+v, want the Pro month", proReceipt)
		}
	})
}

// An email that is not an address an account message can go to owes no
// receipt and never fails the grant: it is the agent's input.
func TestX402GrantOwesNoReceiptForAnUndeliverableEmail(t *testing.T) {
	skipWithoutProYml(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "x402noreceipt", server.NewId())
		sku := &X402Sku{SkuId: "data_synthetic", Description: "Synthetic data pack", PriceUsd: 5, ByteCount: model.Gib}
		before := testutil.ToFloat64(x402ReceiptRefusedCounter)

		emails := []string{
			"not-an-address",
			strings.Repeat("a", 250) + "@synthetic.example",
		}
		for i, email := range emails {
			settle := &X402SettleResponse{Success: true, Transaction: fmt.Sprintf("0xsyntheticnoreceipt%d", i), Network: "base"}
			if err := x402GrantSettled(ctx, networkId, sku, settle, &x402Receipt{email: email, asset: "USDC"}); err != nil {
				t.Fatalf("grant with email %q: %v", email, err)
			}
		}
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), len(emails))
		connect.AssertEqual(t, outboxMessageCount(t, ctx, networkId), 0)
		if delta := testutil.ToFloat64(x402ReceiptRefusedCounter) - before; delta != float64(len(emails)) {
			t.Fatalf("counted %v refused receipts, want %d", delta, len(emails))
		}
	})
}

// The receipt commits with the grant: a grant whose transaction fails at
// commit leaves neither the balance nor the receipt, and the reconciler's
// retry of the grant, which carries no email, owes none.
func TestX402ReceiptCommitsWithTheGrant(t *testing.T) {
	skipWithoutProYml(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "x402receiptcommit", server.NewId())
		sku := &X402Sku{SkuId: "data_synthetic", Description: "Synthetic data pack", PriceUsd: 5, ByteCount: model.Gib}
		settle := &X402SettleResponse{Success: true, Transaction: "0xsyntheticreceiptcommit", Network: "base"}

		removeFault := failOutboxCommitsForNetwork(t, ctx, networkId)
		value := recoverPanic(func() {
			x402GrantData(ctx, networkId, sku, model.UsdToNanoCents(5), settle, &x402Receipt{email: "agent@synthetic.example", asset: "USDC"})
		})
		if err, ok := value.(error); !ok || !strings.Contains(err.Error(), "synthetic failure at commit") {
			t.Fatalf("grant panic = %v, want the commit failure", value)
		}
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 0)
		connect.AssertEqual(t, outboxMessageCount(t, ctx, networkId), 0)
		removeFault()

		// the reconciler grants a settled purchase without its email
		err := x402GrantData(ctx, networkId, sku, model.UsdToNanoCents(5), settle, nil)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 1)
		connect.AssertEqual(t, outboxMessageCount(t, ctx, networkId), 0)
	})
}

// The Solana data pack's applied note commits with the credit: a credit whose
// transaction fails at commit leaves no balance, no note and the intent open,
// and Helius's redelivery credits once and owes one note, delivered once.
func TestSolanaDataPackAppliedNoteCommitsWithTheCredit(t *testing.T) {
	skipWithoutProYml(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		adminEmail := model.Testing_CreateNetwork(ctx, networkId, "buydatacommit", server.NewId())
		pageSession := session.Testing_CreateClientSession(ctx, nil)
		webhookSession := session.Testing_CreateClientSession(ctx, nil)

		reference := "8Hk4tRy0qMnO3wC5dE7fG9iK2lN6oQ8sT1uV3xZ5aB7C"
		solanaTestAmountSuffixes(t, 204)
		intentResult, err := PayDataSolanaIntent(&PayDataSolanaIntentArgs{
			ItemId:      StripeItemData1Tib,
			NetworkName: "BuyDataCommit",
			Reference:   reference,
		}, pageSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, intentResult.Error, (*PayDataCheckoutError)(nil))
		priceUsd, ok := dataPackPriceUsd(StripeItemData1Tib)
		connect.AssertEqual(t, ok, true)

		removeFault := failOutboxCommitsForNetwork(t, ctx, networkId)
		value := recoverPanic(func() {
			HeliusWebhook(
				[]*SolanaTransaction{solanaTestMemoPayment(reference, "sig-datapack-commit", priceUsd)},
				webhookSession,
			)
		})
		if err, ok := value.(error); !ok || !strings.Contains(err.Error(), "synthetic failure at commit") {
			t.Fatalf("credit panic = %v, want the commit failure", value)
		}
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 0)
		connect.AssertEqual(t, outboxMessageCount(t, ctx, networkId), 0)
		status, err := PayDataSolanaStatus(&PayDataSolanaStatusArgs{Reference: reference}, pageSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, status.Status, PayDataSolanaStatusPending)
		removeFault()

		result, err := HeliusWebhook(
			[]*SolanaTransaction{solanaTestMemoPayment(reference, "sig-datapack-commit", priceUsd)},
			webhookSession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Message, "Processed 1 matching payments")
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 1)
		connect.AssertEqual(t, outboxMessageCount(t, ctx, networkId), 1)

		sender := newOutboxTestSender()
		deliverAccountMessagesAt(ctx, sender, server.NowUtc())
		deliverAccountMessagesAt(ctx, sender, server.NowUtc())
		sends := sender.sent()
		if len(sends) != 1 || sends[0].userAuth != adminEmail {
			t.Fatalf("delivered %d notes (%+v), want one to %s", len(sends), sends, adminEmail)
		}
		applied, ok := sends[0].template.(*SubscriptionDataAppliedTemplate)
		if !ok || applied.NetworkName != "buydatacommit" || applied.BalanceByteCount != model.Tib || applied.Secret != "" {
			t.Fatalf("delivered %+v, want the applied note for 1 TiB on buydatacommit", sends[0].template)
		}
	})
}
