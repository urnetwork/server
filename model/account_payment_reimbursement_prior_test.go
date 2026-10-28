// Tests of manual reimbursements created with --allow_prior_reimbursement: the
// marker's bytes, the plan's checks, and the creation's refusal of open
// reimbursements its markers do not list.
package model

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

func manualReimbursementPriorTestMarker() *ManualReimbursementMarker {
	return &ManualReimbursementMarker{
		Version:         1,
		Digest:          "synthetic-digest",
		Reason:          "synthetic reimbursement",
		PaymentPlanId:   server.RequireParseId("00000000-0000-8000-8000-000000000001"),
		PaymentId:       server.RequireParseId("00000000-0000-8000-8000-000000000002"),
		NetworkId:       server.RequireParseId("00000000-0000-8000-8000-000000000003"),
		WalletId:        server.RequireParseId("00000000-0000-8000-8000-000000000004"),
		WalletAddress:   "SyntheticSolanaAddress1111111111111111111111",
		Blockchain:      "SOL",
		AmountNanoCents: 1_000_000,
		FeeNanoCents:    10_000_000,
		PayoutNanoCents: 11_000_000,
	}
}

const manualReimbursementPriorTestDetails = `{"version":1,"digest":"synthetic-digest","reason":"synthetic reimbursement","payment_plan_id":"00000000-0000-8000-8000-000000000001","payment_id":"00000000-0000-8000-8000-000000000002","network_id":"00000000-0000-8000-8000-000000000003","wallet_id":"00000000-0000-8000-8000-000000000004","wallet_address":"SyntheticSolanaAddress1111111111111111111111","blockchain":"SOL","amount_nano_cents":1000000,"fee_nano_cents":10000000,"payout_nano_cents":11000000}`

// Without --allow_prior_reimbursement a marker is the exact JSON of before the
// flag, so a rerun of an earlier plan matches its stored markers byte for byte.
func TestManualReimbursementMarkerDetailsWithoutPriors(t *testing.T) {
	if details := manualReimbursementPriorTestMarker().details(); details != manualReimbursementPriorTestDetails {
		t.Fatalf("marker details changed:\n%s\nwant\n%s", details, manualReimbursementPriorTestDetails)
	}
}

// With it, the flag and the prior payment ids follow the fields of before.
func TestManualReimbursementMarkerDetailsWithPriors(t *testing.T) {
	marker := manualReimbursementPriorTestMarker()
	marker.AllowPriorReimbursement = true
	if details := marker.details(); details != strings.TrimSuffix(manualReimbursementPriorTestDetails, "}")+`,"allow_prior_reimbursement":true}` {
		t.Fatalf("marker details %s", details)
	}
	marker.PriorPaymentIds = []server.Id{server.RequireParseId("00000000-0000-8000-8000-000000000005"), server.RequireParseId("00000000-0000-8000-8000-000000000006")}
	want := strings.TrimSuffix(manualReimbursementPriorTestDetails, "}") + `,"allow_prior_reimbursement":true,"prior_payment_ids":["00000000-0000-8000-8000-000000000005","00000000-0000-8000-8000-000000000006"]}`
	if details := marker.details(); details != want {
		t.Fatalf("marker details %s", details)
	}
}

// A plan's own checks of --allow_prior_reimbursement: prior reimbursements only
// with the flag, the flag on the plan and every marker or on none, each prior
// once and in increasing order, and never a payment of the plan itself.
func TestManualReimbursementPlanChecksItsPriors(t *testing.T) {
	plan := func() *ManualReimbursementPlan {
		first := manualReimbursementPriorTestMarker()
		first.AllowPriorReimbursement = true
		first.PriorPaymentIds = []server.Id{server.RequireParseId("00000000-0000-8000-8000-0000000000c1"), server.RequireParseId("00000000-0000-8000-8000-0000000000c2")}
		second := manualReimbursementPriorTestMarker()
		second.PaymentId = server.RequireParseId("00000000-0000-8000-8000-0000000000b2")
		second.NetworkId = server.RequireParseId("00000000-0000-8000-8000-0000000000b3")
		second.AllowPriorReimbursement = true
		return &ManualReimbursementPlan{
			PaymentPlanId:           first.PaymentPlanId,
			Digest:                  first.Digest,
			Reason:                  first.Reason,
			AllowPriorReimbursement: true,
			Markers:                 []*ManualReimbursementMarker{first, second},
		}
	}
	if err := plan().check(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(plan *ManualReimbursementPlan){
		"prior reimbursements without the flag": func(plan *ManualReimbursementPlan) {
			plan.AllowPriorReimbursement = false
			for _, marker := range plan.Markers {
				marker.AllowPriorReimbursement = false
			}
		},
		"the flag on the plan only": func(plan *ManualReimbursementPlan) {
			plan.Markers[1].AllowPriorReimbursement = false
		},
		"the flag on the markers only": func(plan *ManualReimbursementPlan) {
			plan.AllowPriorReimbursement = false
			plan.Markers[0].PriorPaymentIds = nil
		},
		"out of order": func(plan *ManualReimbursementPlan) {
			slices.Reverse(plan.Markers[0].PriorPaymentIds)
		},
		"twice": func(plan *ManualReimbursementPlan) {
			plan.Markers[0].PriorPaymentIds[1] = plan.Markers[0].PriorPaymentIds[0]
		},
		"a zero id": func(plan *ManualReimbursementPlan) {
			plan.Markers[0].PriorPaymentIds[0] = server.Id{}
		},
		"a payment of the plan": func(plan *ManualReimbursementPlan) {
			plan.Markers[0].PriorPaymentIds = []server.Id{plan.Markers[1].PaymentId}
		},
	} {
		changed := plan()
		change(changed)
		if changed.check() == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

// With --allow_prior_reimbursement a plan may pay a network that has open
// manual reimbursements of other plans, but only those its marker lists: a plan
// without the flag, or one whose list misses a reimbursement (one that appeared
// after its census), writes nothing. A rerun of a created plan creates nothing.
func TestCreateManualReimbursementPlanAcceptsOnlyListedPriors(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		count := func(paymentPlanId server.Id) (payments int) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM account_payment WHERE payment_plan_id = $1`, paymentPlanId).Scan(&payments))
			})
			return
		}
		allowing := func(plan *ManualReimbursementPlan, priorPaymentIds ...server.Id) *ManualReimbursementPlan {
			plan.AllowPriorReimbursement = true
			plan.Markers[0].AllowPriorReimbursement = true
			plan.Markers[0].PriorPaymentIds = priorPaymentIds
			return plan
		}
		network := newManualReimbursementTestNetwork(t, ctx)
		first := manualReimbursementTestPlan(network, "synthetic-first-digest", 1_000_000)
		if created, err := CreateManualReimbursementPlan(ctx, first, manualReimbursementTestNow); err != nil || !created {
			t.Fatal(created, err)
		}
		firstPaymentId := first.Markers[0].PaymentId
		refusal := fmt.Sprintf("network %s already has manual reimbursement %s in plan %s; nothing was created", network.networkId, firstPaymentId, first.PaymentPlanId)

		// without the flag, or with it and no list, the open reimbursement refuses
		for _, plan := range []*ManualReimbursementPlan{
			manualReimbursementTestPlan(network, "synthetic-second-digest", 2_000_000),
			allowing(manualReimbursementTestPlan(network, "synthetic-second-digest", 2_000_000)),
		} {
			if _, err := CreateManualReimbursementPlan(ctx, plan, manualReimbursementTestNow); err == nil || err.Error() != refusal {
				t.Fatalf("an unlisted open reimbursement was accepted: %v", err)
			}
			if payments := count(plan.PaymentPlanId); payments != 0 {
				t.Fatal("the refused plan wrote", payments)
			}
		}

		// listing it creates the plan, with the list in its marker; a rerun
		// creates nothing
		second := allowing(manualReimbursementTestPlan(network, "synthetic-second-digest", 2_000_000), firstPaymentId)
		if created, err := CreateManualReimbursementPlan(ctx, second, manualReimbursementTestNow); err != nil || !created {
			t.Fatal(created, err)
		}
		if created, err := CreateManualReimbursementPlan(ctx, second, manualReimbursementTestNow); err != nil || created {
			t.Fatal("the rerun", created, err)
		}
		if payments := count(second.PaymentPlanId); payments != 1 {
			t.Fatal("the plan", payments)
		}
		secondPaymentId := second.Markers[0].PaymentId
		marker, err := GetManualReimbursementMarker(ctx, secondPaymentId)
		if err != nil || marker == nil || !marker.AllowPriorReimbursement || !slices.Equal(marker.PriorPaymentIds, []server.Id{firstPaymentId}) {
			t.Fatalf("marker %+v %v", marker, err)
		}

		// a census from before the second plan lists only the first
		third := allowing(manualReimbursementTestPlan(network, "synthetic-third-digest", 3_000_000), firstPaymentId)
		if _, err := CreateManualReimbursementPlan(ctx, third, manualReimbursementTestNow); err == nil ||
			!strings.Contains(err.Error(), fmt.Sprintf("already has manual reimbursement %s in plan %s", secondPaymentId, second.PaymentPlanId)) {
			t.Fatalf("a reimbursement missing from the list was accepted: %v", err)
		}
		if payments := count(third.PaymentPlanId); payments != 0 {
			t.Fatal("the refused plan wrote", payments)
		}
		priorPaymentIds := []server.Id{firstPaymentId, secondPaymentId}
		slices.SortFunc(priorPaymentIds, func(a server.Id, b server.Id) int {
			return a.Cmp(b)
		})
		third.Markers[0].PriorPaymentIds = priorPaymentIds
		if created, err := CreateManualReimbursementPlan(ctx, third, manualReimbursementTestNow); err != nil || !created {
			t.Fatal(created, err)
		}
	})
}
