package controller

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// S9: an x402 payment the facilitator settled but whose grant failed must leave a
// durable record, and the payment reconciler must retry the idempotent grant from
// it. The event table and the grants are in-memory fakes; no Postgres.

// x402FakeEventTable is payment_reconciliation_event in memory. Details are
// round-tripped through json, as the db column does.
type x402FakeEventTable struct {
	events []*model.PaymentReconciliationEvent
}

func (self *x402FakeEventTable) add(_ context.Context, event *model.PaymentReconciliationEvent) error {
	stored := *event
	if event.Details != nil {
		detailsJson, err := json.Marshal(event.Details)
		if err != nil {
			return err
		}
		stored.Details = nil
		if err := json.Unmarshal(detailsJson, &stored.Details); err != nil {
			return err
		}
	}
	self.events = append(self.events, &stored)
	return nil
}

// unresolved mirrors model.GetUnresolvedSettledNotGrantedEvents
func (self *x402FakeEventTable) unresolved(_ context.Context, limit int) []*model.PaymentReconciliationEvent {
	resolving := []string{
		model.PaymentReconcileActionCredited,
		model.PaymentReconcileActionAlreadyCredited,
		model.PaymentReconcileActionCreditUnfulfillable,
	}
	resolved := map[string]bool{}
	for _, event := range self.events {
		if event.Store == model.SubscriptionMarketX402 && !event.DryRun && slices.Contains(resolving, event.Action) {
			resolved[event.Evidence] = true
		}
	}
	seen := map[string]bool{}
	unresolved := []*model.PaymentReconciliationEvent{}
	for _, event := range self.events {
		if event.Store != model.SubscriptionMarketX402 ||
			event.Action != model.PaymentReconcileActionSettledNotGranted ||
			event.Evidence == "" || event.DryRun ||
			resolved[event.Evidence] || seen[event.Evidence] {
			continue
		}
		seen[event.Evidence] = true
		unresolved = append(unresolved, event)
	}
	if limit < len(unresolved) {
		unresolved = unresolved[:limit]
	}
	return unresolved
}

func (self *x402FakeEventTable) count(action string, evidence string, dryRun bool) int {
	n := 0
	for _, event := range self.events {
		if event.Action == action && event.Evidence == evidence && event.DryRun == dryRun {
			n += 1
		}
	}
	return n
}

// x402FakeGrants stands in for the two grant functions and the granted check
type x402FakeGrants struct {
	err error
	// settle transaction -> what was granted
	granted map[string]*X402Sku
	calls   int
}

func (self *x402FakeGrants) grant(_ context.Context, _ server.Id, sku *X402Sku, _ model.NanoCents, settleResponse *X402SettleResponse, _ *x402Receipt) error {
	self.calls += 1
	if self.err != nil {
		return self.err
	}
	if _, ok := self.granted[settleResponse.Transaction]; !ok {
		self.granted[settleResponse.Transaction] = sku
	}
	return nil
}

func (self *x402FakeGrants) isGranted(_ context.Context, _ server.Id, transaction string) bool {
	_, ok := self.granted[transaction]
	return ok
}

func installX402SettledNotGrantedFakes(t *testing.T) (*x402FakeEventTable, *x402FakeGrants) {
	table := &x402FakeEventTable{}
	grants := &x402FakeGrants{granted: map[string]*X402Sku{}}

	prevAdd := addPaymentReconciliationEvent
	prevProMonth := x402GrantProMonthFunc
	prevData := x402GrantDataFunc
	prevList := x402ListUnresolvedSettledNotGranted
	prevGranted := x402ReconcileTransactionGranted
	t.Cleanup(func() {
		addPaymentReconciliationEvent = prevAdd
		x402GrantProMonthFunc = prevProMonth
		x402GrantDataFunc = prevData
		x402ListUnresolvedSettledNotGranted = prevList
		x402ReconcileTransactionGranted = prevGranted
	})

	addPaymentReconciliationEvent = table.add
	x402GrantProMonthFunc = grants.grant
	x402GrantDataFunc = grants.grant
	x402ListUnresolvedSettledNotGranted = table.unresolved
	x402ReconcileTransactionGranted = grants.isGranted
	return table, grants
}

func x402ReconcileTestRun(dryRun bool) *paymentReconcileRun {
	return &paymentReconcileRun{
		clientSession: &session.ClientSession{Ctx: context.Background()},
		runId:         server.NewId(),
		now:           time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC),
		dryRun:        dryRun,
		budget:        paymentReconcileApiBudget,
		storeDetails:  map[string]map[string]any{},
		storeResults:  map[string]*PaymentReconcileStoreResult{},
	}
}

func TestX402SettledButNotGrantedIsRecordedAndReconciled(t *testing.T) {
	table, grants := installX402SettledNotGrantedFakes(t)
	ctx := context.Background()

	networkId := server.NewId()
	sku := &X402Sku{
		SkuId:     X402SkuProMonth,
		PriceUsd:  5,
		Pro:       true,
		ByteCount: model.ByteCount(600) * 1024 * 1024 * 1024,
	}
	settleResponse := &X402SettleResponse{
		Success:     true,
		Transaction: "0xsettled-not-granted-1",
		Network:     "base",
	}

	// the facilitator settled; the grant fails
	grants.err = errors.New("synthetic grant failure")
	err := x402GrantSettled(ctx, networkId, sku, settleResponse, nil)
	connect.AssertNotEqual(t, err, nil)

	recorded := table.count(model.PaymentReconcileActionSettledNotGranted, settleResponse.Transaction, false)
	if recorded != 1 {
		t.Fatalf("settled-but-not-granted tx %s left %d durable records, want 1; the only trace is a log line", settleResponse.Transaction, recorded)
	}
	event := table.events[0]
	connect.AssertEqual(t, event.Store, model.SubscriptionMarketX402)
	connect.AssertEqual(t, *event.NetworkId, networkId)

	// a dry run audits the would-be credit and grants nothing
	run := x402ReconcileTestRun(true)
	complete, err := reconcileX402(run, run.now)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, complete, true)
	connect.AssertEqual(t, table.count(model.PaymentReconcileActionWouldCredit, settleResponse.Transaction, true), 1)
	connect.AssertEqual(t, len(grants.granted), 0)

	// the grant still fails: an error, and the record stays unresolved
	run = x402ReconcileTestRun(false)
	_, err = reconcileX402(run, run.now)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, run.storeResult(model.SubscriptionMarketX402).Errors, 1)
	connect.AssertEqual(t, len(table.unresolved(ctx, 10)), 1)

	// the grant recovers: credited exactly as bought, and resolved
	grants.err = nil
	run = x402ReconcileTestRun(false)
	_, err = reconcileX402(run, run.now)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, run.credited, 1)
	connect.AssertEqual(t, table.count(model.PaymentReconcileActionCredited, settleResponse.Transaction, false), 1)
	granted := grants.granted[settleResponse.Transaction]
	connect.AssertNotEqual(t, granted, nil)
	connect.AssertEqual(t, *granted, *sku)
	connect.AssertEqual(t, len(table.unresolved(ctx, 10)), 0)

	// later runs leave it alone
	callsBefore := grants.calls
	run = x402ReconcileTestRun(false)
	_, err = reconcileX402(run, run.now)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, run.storeResult(model.SubscriptionMarketX402).Examined, 0)
	connect.AssertEqual(t, grants.calls, callsBefore)
}

func TestX402ReconcileResolvesWithoutDoubleGrant(t *testing.T) {
	table, grants := installX402SettledNotGrantedFakes(t)
	ctx := context.Background()

	dataSku := &X402Sku{SkuId: "data_100gib", PriceUsd: 2, ByteCount: model.ByteCount(100) * 1024 * 1024 * 1024}

	// an agent retry granted the tx after the first attempt failed
	retriedNetworkId := server.NewId()
	retried := &X402SettleResponse{Success: true, Transaction: "0xretried", Network: "base"}
	grants.err = errors.New("synthetic grant failure")
	connect.AssertNotEqual(t, x402GrantSettled(ctx, retriedNetworkId, dataSku, retried, nil), nil)
	grants.err = nil
	connect.AssertEqual(t, x402GrantSettled(ctx, retriedNetworkId, dataSku, retried, nil), nil)

	// a network deleted after it paid
	deletedNetworkId := server.NewId()
	deleted := &X402SettleResponse{Success: true, Transaction: "0xdeleted", Network: "base"}
	grants.err = errors.New("synthetic grant failure")
	connect.AssertNotEqual(t, x402GrantSettled(ctx, deletedNetworkId, dataSku, deleted, nil), nil)

	grants.err = nil
	callsBefore := grants.calls
	x402GrantDataFunc = func(ctx context.Context, networkId server.Id, sku *X402Sku, netRevenue model.NanoCents, settleResponse *X402SettleResponse, receipt *x402Receipt) error {
		if networkId == deletedNetworkId {
			return model.ErrPaymentNetworkNotFound
		}
		return grants.grant(ctx, networkId, sku, netRevenue, settleResponse, receipt)
	}

	run := x402ReconcileTestRun(false)
	_, err := reconcileX402(run, run.now)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, run.credited, 0)
	connect.AssertEqual(t, grants.calls, callsBefore)
	connect.AssertEqual(t, table.count(model.PaymentReconcileActionAlreadyCredited, retried.Transaction, false), 1)
	connect.AssertEqual(t, table.count(model.PaymentReconcileActionCreditUnfulfillable, deleted.Transaction, false), 1)
	connect.AssertEqual(t, len(table.unresolved(ctx, 10)), 0)
}
