// Legacy keyed payments are bound, released or held only from Circle's
// destination history, and are never resubmitted without affirmative absence.
// The Circle lister is a fake owned by each test; the payout wallet id is
// synthetic.
package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

const legacyReconcileTestWalletId = "synthetic-payout-wallet"

type legacyReconcileTestLister struct {
	transactions []*CircleListedTransaction
	err          error
	calls        int
}

func (self *legacyReconcileTestLister) ListDestinationTransactions(ctx context.Context, blockchain string, destinationAddress string) ([]*CircleListedTransaction, error) {
	self.calls += 1
	if self.err != nil {
		return nil, self.err
	}
	return self.transactions, nil
}

func legacyReconcileTestPayoutWalletId(blockchain string) (string, error) {
	return legacyReconcileTestWalletId, nil
}

// A listed outbound developer transfer from the payout wallet.
func legacyReconcileTestTransfer(id string, state string, amount string, createDate time.Time) *CircleListedTransaction {
	return &CircleListedTransaction{
		Id:                 id,
		State:              state,
		Blockchain:         "MATIC",
		TransactionType:    "OUTBOUND",
		Operation:          "TRANSFER",
		CustodyType:        "DEVELOPER",
		WalletId:           legacyReconcileTestWalletId,
		DestinationAddress: "0x0000000000000000000000000000000000000012",
		Amounts:            []string{amount},
		CreateDate:         createDate,
	}
}

func legacyReconcileTestAttempt(payout model.NanoCents, createTime time.Time) *model.LegacyProviderPaymentAttempt {
	networkId, walletId, key := server.NewId(), server.NewId(), server.NewId()
	return &model.LegacyProviderPaymentAttempt{
		Payment: &model.AccountPayment{
			PaymentId:            server.NewId(),
			NetworkId:            networkId,
			WalletId:             &walletId,
			Payout:               payout,
			CreateTime:           createTime,
			CircleIdempotencyKey: &key,
		},
		WalletAddress:   "0x0000000000000000000000000000000000000012",
		Blockchain:      "MATIC",
		WalletNetworkId: &networkId,
	}
}

func TestLegacyProviderPaymentGroupDecisions(t *testing.T) {
	planned := time.Date(2026, 7, 9, 13, 0, 0, 0, time.UTC)
	five := legacyReconcileTestAttempt(model.UsdToNanoCents(5), planned)
	three := legacyReconcileTestAttempt(model.UsdToNanoCents(3), planned.Add(7*24*time.Hour))
	twin := legacyReconcileTestAttempt(model.UsdToNanoCents(5), planned.Add(7*24*time.Hour))
	after := planned.Add(time.Hour)
	foreignWallet := legacyReconcileTestTransfer("foreign-wallet", "COMPLETE", "4.990000", after)
	foreignWallet.WalletId = "synthetic-other-wallet"
	inbound := legacyReconcileTestTransfer("inbound-mirror", "COMPLETE", "4.990000", after)
	inbound.TransactionType = "INBOUND"
	customer := legacyReconcileTestTransfer("customer-transfer", "COMPLETE", "1.000000", after)
	customer.CustodyType = "ENDUSER"
	failedWithHash := legacyReconcileTestTransfer("failed-on-chain", "FAILED", "4.990000", after)
	failedWithHash.TxHash = "0xsynthetic"

	cases := []struct {
		name         string
		attempts     []*model.LegacyProviderPaymentAttempt
		transactions []*CircleListedTransaction
		bound        map[string]bool
		found        map[server.Id]string
		absent       []server.Id
		held         int
	}{
		{name: "no transfers is absence", attempts: []*model.LegacyProviderPaymentAttempt{five}, absent: []server.Id{five.Payment.PaymentId}},
		{name: "a terminal failure moved no funds", attempts: []*model.LegacyProviderPaymentAttempt{five},
			transactions: []*CircleListedTransaction{legacyReconcileTestTransfer("failed", "FAILED", "4.990000", after)},
			absent:       []server.Id{five.Payment.PaymentId}},
		{name: "an exact live transfer is found", attempts: []*model.LegacyProviderPaymentAttempt{five},
			transactions: []*CircleListedTransaction{legacyReconcileTestTransfer("sent", "SENT", "4.99", after)},
			found:        map[server.Id]string{five.Payment.PaymentId: "sent"}},
		{name: "a transfer bound to another attempt is not evidence", attempts: []*model.LegacyProviderPaymentAttempt{five},
			transactions: []*CircleListedTransaction{legacyReconcileTestTransfer("bound", "COMPLETE", "4.990000", after)},
			bound:        map[string]bool{"bound": true}, absent: []server.Id{five.Payment.PaymentId}},
		{name: "inbound mirrors and customer transfers are not payouts", attempts: []*model.LegacyProviderPaymentAttempt{five},
			transactions: []*CircleListedTransaction{inbound, customer}, absent: []server.Id{five.Payment.PaymentId}},
		{name: "a transfer before the payment was planned is not its attempt", attempts: []*model.LegacyProviderPaymentAttempt{five},
			transactions: []*CircleListedTransaction{legacyReconcileTestTransfer("older", "COMPLETE", "4.990000", planned.Add(-30*24*time.Hour))},
			absent:       []server.Id{five.Payment.PaymentId}},
		{name: "attribution finds one attempt and proves the other absent", attempts: []*model.LegacyProviderPaymentAttempt{five, three},
			transactions: []*CircleListedTransaction{legacyReconcileTestTransfer("three", "COMPLETE", "2.990000", three.Payment.CreateTime.Add(time.Hour))},
			found:        map[server.Id]string{three.Payment.PaymentId: "three"}, absent: []server.Id{five.Payment.PaymentId}},
		{name: "a live transfer of another amount is ambiguous", attempts: []*model.LegacyProviderPaymentAttempt{five},
			transactions: []*CircleListedTransaction{legacyReconcileTestTransfer("other", "COMPLETE", "4.980000", after)}, held: 1},
		{name: "a live transfer from another wallet is ambiguous", attempts: []*model.LegacyProviderPaymentAttempt{five},
			transactions: []*CircleListedTransaction{foreignWallet}, held: 1},
		{name: "a failed transfer with a chain hash is bound for the status flow to retain", attempts: []*model.LegacyProviderPaymentAttempt{five},
			transactions: []*CircleListedTransaction{failedWithHash}, found: map[server.Id]string{five.Payment.PaymentId: "failed-on-chain"}},
		{name: "an amount shared by two attempts is ambiguous", attempts: []*model.LegacyProviderPaymentAttempt{five, twin},
			transactions: []*CircleListedTransaction{legacyReconcileTestTransfer("one", "COMPLETE", "4.990000", twin.Payment.CreateTime.Add(time.Hour))}, held: 2},
		{name: "two transfers for one attempt are ambiguous", attempts: []*model.LegacyProviderPaymentAttempt{five},
			transactions: []*CircleListedTransaction{legacyReconcileTestTransfer("a", "COMPLETE", "4.990000", after), legacyReconcileTestTransfer("b", "QUEUED", "4.990000", after)}, held: 1},
	}
	for _, c := range cases {
		bound := c.bound
		if bound == nil {
			bound = map[string]bool{}
		}
		decision := decideLegacyProviderPaymentGroup(c.attempts, c.transactions, bound, legacyReconcileTestWalletId)
		if len(decision.heldPaymentIds) != c.held {
			t.Errorf("%s: held %d attempts, want %d (%s)", c.name, len(decision.heldPaymentIds), c.held, decision.reason)
			continue
		}
		if c.held != 0 {
			if len(decision.foundTransactions) != 0 || len(decision.absentPaymentIds) != 0 {
				t.Errorf("%s: a held group also decided attempts", c.name)
			}
			continue
		}
		if len(decision.foundTransactions) != len(c.found) {
			t.Errorf("%s: found %d attempts, want %d", c.name, len(decision.foundTransactions), len(c.found))
		}
		for paymentId, record := range c.found {
			if transaction, ok := decision.foundTransactions[paymentId]; !ok || transaction.Id != record {
				t.Errorf("%s: attempt was not bound to %s", c.name, record)
			}
		}
		if fmt.Sprint(decision.absentPaymentIds) != fmt.Sprint(c.absent) {
			t.Errorf("%s: absent %v, want %v", c.name, decision.absentPaymentIds, c.absent)
		}
	}
}

// A legacy attempt: the payment's key was created by a submit before requests
// were retained, so it has no retained request and no record.
func legacyReconcileTestFixture(t testing.TB) (*session.ClientSession, *model.AccountPayment, server.Id) {
	t.Helper()
	cutoff := time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC)
	controllerPayoutSchedule(t, cutoff)
	owner, payment := controllerPayoutFixture(t, cutoff.Add(-time.Microsecond))
	legacyKey := server.NewId()
	server.Tx(owner.Ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(owner.Ctx, `UPDATE account_payment SET circle_idempotency_key=$2 WHERE payment_id=$1`, payment.PaymentId, legacyKey))
	})
	controllerPayoutMock(t, func(ctx context.Context, key server.Id, amount float64, address, network string) (*CreateTransferTransactionResult, error) {
		if err := requireCircleProviderPayment(ctx); err != nil {
			return nil, err
		}
		if key == legacyKey {
			t.Fatal("a legacy key was resubmitted")
		}
		return &CreateTransferTransactionResult{Id: "synthetic-fresh-transfer", State: "INITIATED"}, nil
	})
	payment, err := model.GetPayment(owner.Ctx, payment.PaymentId)
	if err != nil || payment == nil || payment.CircleIdempotencyKey == nil || *payment.CircleIdempotencyKey != legacyKey {
		t.Fatal("legacy fixture lost its key", payment, err)
	}
	return owner, payment, legacyKey
}

func legacyReconcileTestRun(t testing.TB, owner *session.ClientSession, lister *legacyReconcileTestLister, dryRun bool) *LegacyProviderPaymentReconcileResult {
	t.Helper()
	settings := &LegacyProviderPaymentReconcileSettings{DryRun: dryRun}
	// the fixture payment was planned now; observe it past the minimum age
	later := func() time.Time { return server.NowUtc().Add(2 * legacyProviderPaymentMinAge) }
	result, err := reconcileLegacyProviderPaymentAttempts(owner, settings, lister, legacyReconcileTestPayoutWalletId, later)
	if err != nil {
		t.Fatal("reconciliation failed", err)
	}
	return result
}

func legacyReconcileTestJournal(t testing.TB, owner *session.ClientSession, paymentId server.Id) (statuses []string) {
	t.Helper()
	server.Db(owner.Ctx, func(conn server.PgConn) {
		result, err := conn.Query(owner.Ctx, `SELECT event_details::jsonb->>'status' FROM audit_account_payment
			WHERE payment_id=$1 AND event_type='circle_attempt_outcome' ORDER BY event_time, event_id`, paymentId)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var status string
				server.Raise(result.Scan(&status))
				statuses = append(statuses, status)
			}
		})
	})
	return
}

// The held state: the legacy key, no record and no fresh attempt.
func requireLegacyAttemptHeld(t testing.TB, owner *session.ClientSession, payment *model.AccountPayment, legacyKey server.Id) {
	t.Helper()
	held, err := model.GetPayment(owner.Ctx, payment.PaymentId)
	if err != nil || held.CircleIdempotencyKey == nil || *held.CircleIdempotencyKey != legacyKey || held.PaymentRecord != nil || held.Completed || held.Canceled {
		t.Fatal("held legacy attempt changed", held, err)
	}
	if statuses := legacyReconcileTestJournal(t, owner, payment.PaymentId); len(statuses) != 0 {
		t.Fatal("held legacy attempt journaled a decision", statuses)
	}
	// the ordinary path still refuses to guess a retry
	result, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: payment.PaymentId}, owner)
	if !errors.Is(err, model.ErrProviderPaymentAttemptChanged) || result.Complete || result.Canceled {
		t.Fatal("held legacy attempt advanced", result, err)
	}
}

func TestLegacyProviderPaymentFoundTransferIsBoundAndCompletes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		owner, payment, legacyKey := legacyReconcileTestFixture(t)
		lister := &legacyReconcileTestLister{transactions: []*CircleListedTransaction{
			legacyReconcileTestTransfer("synthetic-lost-acknowledgement", "COMPLETE", "9.990000", payment.CreateTime.Add(time.Minute)),
		}}

		preview := legacyReconcileTestRun(t, owner, lister, true)
		if preview.AttemptCount != 1 || preview.FoundCount != 1 || preview.AbsentCount != 0 || lister.calls != 1 {
			t.Fatalf("dry run did not report the found attempt: %+v", preview)
		}
		requireLegacyAttemptHeld(t, owner, payment, legacyKey)

		result := legacyReconcileTestRun(t, owner, lister, false)
		if result.FoundCount != 1 || result.AbsentCount != 0 || result.AmbiguousCount != 0 || result.RefusedCount != 0 {
			t.Fatalf("found attempt was not bound: %+v", result)
		}
		bound, err := model.GetPayment(owner.Ctx, payment.PaymentId)
		if err != nil || bound.PaymentRecord == nil || *bound.PaymentRecord != "synthetic-lost-acknowledgement" || bound.CircleIdempotencyKey == nil ||
			*bound.CircleIdempotencyKey != legacyKey || bound.TokenAmount == nil || *bound.TokenAmount != 9.99 {
			t.Fatal("found transfer was not bound to the original attempt", bound, err)
		}
		if statuses := legacyReconcileTestJournal(t, owner, payment.PaymentId); len(statuses) != 1 || statuses[0] != "LEGACY_RECONCILED_FOUND" {
			t.Fatal("binding was not journaled", statuses)
		}

		// the ordinary status flow completes the bound transfer without a send
		client := NewCircleClient().(*mockCircleApiClient)
		client.GetTransactionFunc = func(_ context.Context, id string) (*GetTransactionResult, error) {
			if id != "synthetic-lost-acknowledgement" {
				t.Fatal("status read a different transfer", id)
			}
			return &GetTransactionResult{Transaction: CircleTransaction{Id: id, State: "COMPLETE", TxHash: "0xsynthetic-found"}, ResponseBodyBytes: []byte(`{"state":"COMPLETE"}`)}, nil
		}
		advanced, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: payment.PaymentId}, owner)
		if err != nil || !advanced.Complete {
			t.Fatal("bound transfer did not complete", advanced, err)
		}

		// a completed attempt is no longer a legacy attempt
		if again := legacyReconcileTestRun(t, owner, lister, false); again.AttemptCount != 0 {
			t.Fatalf("completed attempt was reconciled again: %+v", again)
		}
	})
}

func TestLegacyProviderPaymentAbsentTransferReleasesKeyForFreshAttempt(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		owner, payment, legacyKey := legacyReconcileTestFixture(t)
		// only an earlier failed submit, which moved no funds
		lister := &legacyReconcileTestLister{transactions: []*CircleListedTransaction{
			legacyReconcileTestTransfer("synthetic-failed-submit", "FAILED", "9.990000", payment.CreateTime.Add(time.Minute)),
		}}

		preview := legacyReconcileTestRun(t, owner, lister, true)
		if preview.AbsentCount != 1 || preview.FoundCount != 0 {
			t.Fatalf("dry run did not report the absent attempt: %+v", preview)
		}
		requireLegacyAttemptHeld(t, owner, payment, legacyKey)

		result := legacyReconcileTestRun(t, owner, lister, false)
		if result.AbsentCount != 1 || result.FoundCount != 0 || result.RefusedCount != 0 {
			t.Fatalf("absent attempt was not released: %+v", result)
		}
		released, err := model.GetPayment(owner.Ctx, payment.PaymentId)
		if err != nil || released.CircleIdempotencyKey != nil || released.PaymentRecord != nil || released.Completed || released.Canceled {
			t.Fatal("absent attempt kept its key", released, err)
		}
		if statuses := legacyReconcileTestJournal(t, owner, payment.PaymentId); len(statuses) != 1 || statuses[0] != "LEGACY_RECONCILED_ABSENT" {
			t.Fatal("release was not journaled", statuses)
		}

		// the next AdvancePayment reserves a fresh key with a retained request
		advanced, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: payment.PaymentId}, owner)
		if err != nil || advanced.Complete || advanced.Canceled {
			t.Fatal("released attempt did not submit afresh", advanced, err)
		}
		fresh, err := model.GetPayment(owner.Ctx, payment.PaymentId)
		if err != nil || fresh.CircleIdempotencyKey == nil || *fresh.CircleIdempotencyKey == legacyKey ||
			fresh.PaymentRecord == nil || *fresh.PaymentRecord != "synthetic-fresh-transfer" {
			t.Fatal("fresh attempt did not use a new key", fresh, err)
		}
		server.Db(owner.Ctx, func(conn server.PgConn) {
			var retained bool
			server.Raise(conn.QueryRow(owner.Ctx, `SELECT EXISTS (SELECT 1 FROM audit_account_payment
				WHERE event_id=$1 AND payment_id=$2 AND event_type='circle_attempt_request')`, *fresh.CircleIdempotencyKey, fresh.PaymentId).Scan(&retained))
			if !retained {
				t.Fatal("fresh attempt did not retain its request")
			}
		})
	})
}

func TestLegacyProviderPaymentAmbiguousTransferIsHeld(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		owner, payment, legacyKey := legacyReconcileTestFixture(t)
		// a live transfer to the destination that is not this attempt's amount
		lister := &legacyReconcileTestLister{transactions: []*CircleListedTransaction{
			legacyReconcileTestTransfer("synthetic-unexplained", "COMPLETE", "9.980000", payment.CreateTime.Add(time.Minute)),
		}}
		result := legacyReconcileTestRun(t, owner, lister, false)
		if result.AmbiguousCount != 1 || result.FoundCount != 0 || result.AbsentCount != 0 {
			t.Fatalf("ambiguous attempt was decided: %+v", result)
		}
		requireLegacyAttemptHeld(t, owner, payment, legacyKey)
	})
}

func TestLegacyProviderPaymentLookupErrorIsHeld(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		owner, payment, legacyKey := legacyReconcileTestFixture(t)
		lister := &legacyReconcileTestLister{err: errors.New("synthetic Circle listing outage")}
		result := legacyReconcileTestRun(t, owner, lister, false)
		if result.LookupErrorCount != 1 || result.FoundCount != 0 || result.AbsentCount != 0 || lister.calls != 1 {
			t.Fatalf("failed lookup decided the attempt: %+v", result)
		}
		requireLegacyAttemptHeld(t, owner, payment, legacyKey)
	})
}

// A retained attempt to the same destination may have an unbound transfer, so
// the destination's legacy attempts wait for it.
func TestLegacyProviderPaymentRetainedAttemptToDestinationIsHeld(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		owner, payment, legacyKey := legacyReconcileTestFixture(t)
		// a second payment of the same network and wallet with a retained request
		inFlightId := server.NewId()
		server.Tx(owner.Ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(owner.Ctx, `INSERT INTO account_payment (payment_id, payment_plan_id, network_id, wallet_id, payout_byte_count, payout_nano_cents, min_sweep_time, circle_idempotency_key)
				VALUES ($1, $2, $3, $4, 1, $5, $6, $7)`, inFlightId, server.NewId(), payment.NetworkId, *payment.WalletId, model.UsdToNanoCents(2), server.NowUtc(), inFlightId))
			server.RaisePgResult(tx.Exec(owner.Ctx, `INSERT INTO audit_account_payment (event_id, payment_id, event_type, event_details)
				VALUES ($1, $2, 'circle_attempt_request', '{}')`, inFlightId, inFlightId))
		})
		lister := &legacyReconcileTestLister{}
		result := legacyReconcileTestRun(t, owner, lister, false)
		if result.AmbiguousCount != 1 || result.AbsentCount != 0 || lister.calls != 0 {
			t.Fatalf("legacy attempt was decided beside a retained attempt: %+v", result)
		}
		requireLegacyAttemptHeld(t, owner, payment, legacyKey)
	})
}

// The real listing follows Circle's pagination and refuses a row outside the
// requested destination instead of treating a partial listing as evidence.
func TestCircleListDestinationTransactionsPaginatesAndBindsDestination(t *testing.T) {
	const destination = "0x00000000000000000000000000000000000000d2"
	transaction := func(id string, address string) map[string]any {
		return map[string]any{"id": id, "state": "COMPLETE", "blockchain": "MATIC", "transactionType": "OUTBOUND", "operation": "TRANSFER",
			"custodyType": "DEVELOPER", "walletId": "synthetic-wallet", "destinationAddress": address, "amounts": []string{"1.000000"},
			"createDate": "2026-07-10T00:00:00Z"}
	}
	for _, foreign := range []bool{false, true} {
		calls, closed := 0, 0
		client := paymentReadTestCircle(paymentReadHooks{
			wait: func(ctx context.Context, delay time.Duration) error { return ctx.Err() },
			do: func(request *http.Request) (*http.Response, error) {
				requirePaymentReadRequest(t, request)
				query := request.URL.Query()
				if !strings.HasSuffix(request.URL.Path, "/v1/w3s/transactions") || query.Get("destinationAddress") != destination ||
					query.Get("blockchain") != "MATIC" || query.Get("includeAll") != "true" {
					t.Fatal("listing request lost its destination filters", request.URL.String())
				}
				calls++
				if calls == 1 {
					response := paymentReadTestResponse(200, circleReadTestPage(t, "transactions", []any{transaction("synthetic-first", destination)}), &closed)
					response.Header = http.Header{"Link": []string{circleReadTestNext(request, "synthetic-first")}}
					return response, nil
				}
				address := destination
				if foreign {
					address = "0x00000000000000000000000000000000000000d3"
				}
				return paymentReadTestResponse(200, circleReadTestPage(t, "transactions", []any{transaction("synthetic-second", address)}), &closed), nil
			},
		})
		listed, err := client.ListDestinationTransactions(context.Background(), "MATIC", destination)
		if foreign {
			if err == nil || listed != nil {
				t.Fatal("a row outside the destination was accepted", listed)
			}
		} else if err != nil || len(listed) != 2 || listed[0].Id != "synthetic-first" || listed[1].Id != "synthetic-second" || calls != 2 || closed != 2 {
			t.Fatal("listing did not follow pagination to the end", listed, err, calls, closed)
		}
	}
}
