// Database tests of `payouts reimburse --allow_prior_reimbursement`, with the
// fake Circle and injected clock of account_payment_reimbursement_db_test.go.
// A network's earlier reimbursement is an ordinary send without the flag.
package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

const payoutReimbursementPriorTestReason = "synthetic per-window reliability subsidy"

func payoutReimbursementTestRunAllowingPrior(clientSession *session.ClientSession, data []byte, reason string, send bool, expect string) (*PayoutReimbursementReport, error) {
	return payoutsReimburse(clientSession, data, reason, send, expect, true, payoutReimbursementTestClock)
}

// Sends the rows under `reason` without --allow_prior_reimbursement: the
// earlier reimbursement of their networks.
func payoutReimbursementPriorTestSend(t testing.TB, clientSession *session.ClientSession, data []byte, reason string) *PayoutReimbursementReport {
	t.Helper()
	dry, err := payoutReimbursementTestRun(clientSession, data, reason, false, "")
	if err != nil {
		t.Fatal(err)
	}
	report, err := payoutReimbursementTestRun(clientSession, data, reason, true, dry.Digest)
	if err != nil || !report.Created {
		t.Fatalf("the earlier reimbursement %+v %v", report, err)
	}
	return report
}

// A dry run with --allow_prior_reimbursement that must not refuse.
func payoutReimbursementPriorTestDryRun(t testing.TB, clientSession *session.ClientSession, data []byte, reason string) *PayoutReimbursementReport {
	t.Helper()
	report, err := payoutReimbursementTestRunAllowingPrior(clientSession, data, reason, false, "")
	if err != nil || len(report.Digest) != 64 {
		t.Fatalf("dry run with --allow_prior_reimbursement %+v %v", report, err)
	}
	return report
}

func payoutReimbursementTestNetworkPaymentCount(t testing.TB, ctx context.Context, networkId server.Id) (count int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM account_payment WHERE network_id = $1`, networkId).Scan(&count))
	})
	return
}

// Without --allow_prior_reimbursement, a network that already has an open
// manual reimbursement is refused exactly as before the flag: the dry run and
// the send name the earlier payment and its plan, list it among the conflicts,
// keep the v1 digest, print no prior reimbursement, and write nothing.
func TestPayoutReimbursementWithoutAllowPriorRefusesAnEarlierReimbursement(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		defer clientSession.Cancel()
		circle := newPayoutReimbursementTestCircle(t)

		networkId := payoutReimbursementTestNetwork(t, ctx, "synthetic-prior-refused")
		walletId := payoutReimbursementTestWallet(t, ctx, networkId, "SOL", payoutReimbursementTestAddress("SyntheticPriorRefused"), true)
		earlier := payoutReimbursementPriorTestSend(t, clientSession, payoutReimbursementTestCsv(payoutReimbursementTestRow(networkId, walletId, "10.000001")), payoutReimbursementTestReason)
		data := payoutReimbursementTestCsv(payoutReimbursementTestRow(networkId, walletId, "2.5"))
		want := fmt.Sprintf("network %s already has manual reimbursement %s in plan %s", networkId, earlier.Rows[0].PaymentId, earlier.PaymentPlanId)
		before := payoutReimbursementTestWrites(t, ctx)
		sends := len(circle.sends)

		dry, err := payoutReimbursementTestRun(clientSession, data, payoutReimbursementPriorTestReason, false, "")
		if !errors.Is(err, ErrPayoutReimbursementRefused) || !strings.Contains(err.Error(), want) {
			t.Fatalf("dry run: %v; want a refusal saying %q", err, want)
		}
		if !slices.Equal(dry.Problems, []string{want}) || len(dry.Conflicts) != 1 || dry.Conflicts[0].PaymentId != earlier.Rows[0].PaymentId ||
			dry.AllowPriorReimbursement || dry.Rows[0].PriorReimbursements != nil {
			t.Fatalf("dry run report %+v", dry)
		}
		if dry.Digest != payoutReimbursementDigest(payoutReimbursementPriorTestReason, dry.FeeNanoCents, dry.Rows) || dry.PaymentPlanId != payoutReimbursementPlanId(dry.Digest) {
			t.Fatal("the refusal's digest or plan is not the v1 digest's", dry.Digest, dry.PaymentPlanId)
		}
		text := dry.Text()
		if !strings.Contains(text, "refused: "+want+"\n") || strings.Contains(text, "prior reimbursement") {
			t.Fatalf("dry run text:\n%s", text)
		}

		send, err := payoutReimbursementTestRun(clientSession, data, payoutReimbursementPriorTestReason, true, dry.Digest)
		if !errors.Is(err, ErrPayoutReimbursementRefused) || !strings.Contains(err.Error(), want) || send.Created {
			t.Fatalf("send: %v", err)
		}
		if after := payoutReimbursementTestWrites(t, ctx); after != before || len(circle.sends) != sends {
			t.Fatal("the refusal wrote or sent", before, after)
		}
	})
}

// With --allow_prior_reimbursement, networks that already have open manual
// reimbursements are accepted: the dry run, in a read-only process, lists each
// earlier payment on its network's row with its plan, amount, state and reason,
// refuses nothing, binds them in the digest, and writes nothing.
func TestPayoutReimbursementAllowPriorListsTheEarlierReimbursements(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		defer clientSession.Cancel()
		circle := newPayoutReimbursementTestCircle(t)

		completeNetwork := payoutReimbursementTestNetwork(t, ctx, "synthetic-prior-complete")
		completeWallet := payoutReimbursementTestWallet(t, ctx, completeNetwork, "SOL", payoutReimbursementTestAddress("SyntheticPriorComplete"), true)
		openNetwork := payoutReimbursementTestNetwork(t, ctx, "synthetic-prior-open")
		openWallet := payoutReimbursementTestWallet(t, ctx, openNetwork, "MATIC", "0x00000000000000000000000000000000000000d1", true)
		freshNetwork := payoutReimbursementTestNetwork(t, ctx, "synthetic-prior-none")
		freshWallet := payoutReimbursementTestWallet(t, ctx, freshNetwork, "SOL", payoutReimbursementTestAddress("SyntheticPriorNone"), true)
		earlier := payoutReimbursementPriorTestSend(t, clientSession, payoutReimbursementTestCsv(
			payoutReimbursementTestRow(completeNetwork, completeWallet, "10.000001"),
			payoutReimbursementTestRow(openNetwork, openWallet, "0.5"),
		), payoutReimbursementTestReason)
		circle.state, circle.txHash = "COMPLETE", "synthetic-prior-hash"
		if result, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: earlier.Rows[0].PaymentId}, clientSession); err != nil || !result.Complete {
			t.Fatalf("completion %+v %v", result, err)
		}

		data := payoutReimbursementTestCsv(
			payoutReimbursementTestRow(completeNetwork, completeWallet, "1.25"),
			payoutReimbursementTestRow(openNetwork, openWallet, "0.75"),
			payoutReimbursementTestRow(freshNetwork, freshWallet, "3.5"),
		)
		before := payoutReimbursementTestWrites(t, ctx)
		sends, reads := len(circle.sends), circle.reads
		server.RequirePgReadOnly()
		t.Cleanup(server.Testing_ReleasePgReadOnly)
		report, err := payoutReimbursementTestRunAllowingPrior(clientSession, data, payoutReimbursementPriorTestReason, false, "")
		server.Testing_ReleasePgReadOnly()
		if err != nil {
			t.Fatal(err)
		}
		if after := payoutReimbursementTestWrites(t, ctx); after != before || len(circle.sends) != sends || circle.reads != reads {
			t.Fatal("the dry run wrote or called Circle", before, after)
		}

		if !report.AllowPriorReimbursement || len(report.Problems) != 0 || len(report.Conflicts) != 0 || report.PlanPaymentCount != 0 || len(report.Rows) != 3 {
			t.Fatalf("report %+v", report)
		}
		wantPriors := [][]PayoutReimbursementPrior{
			{{PaymentId: earlier.Rows[0].PaymentId, PaymentPlanId: earlier.PaymentPlanId, AmountNanoCents: 10_000_001_000, Reason: payoutReimbursementTestReason, Completed: true}},
			{{PaymentId: earlier.Rows[1].PaymentId, PaymentPlanId: earlier.PaymentPlanId, AmountNanoCents: 500_000_000, Reason: payoutReimbursementTestReason}},
			{},
		}
		for i, row := range report.Rows {
			if len(row.PriorReimbursements) != len(wantPriors[i]) {
				t.Fatalf("row %d lists %d prior reimbursements, want %d", i, len(row.PriorReimbursements), len(wantPriors[i]))
			}
			for j, prior := range row.PriorReimbursements {
				if *prior != wantPriors[i][j] {
					t.Fatalf("row %d prior reimbursement %+v, want %+v", i, *prior, wantPriors[i][j])
				}
			}
		}
		// the digest binds the flag and the earlier payments; the ids derive
		// from the v1 digest
		planDigest := payoutReimbursementDigest(payoutReimbursementPriorTestReason, report.FeeNanoCents, report.Rows)
		if report.Digest != payoutReimbursementAllowPriorDigest(payoutReimbursementPriorTestReason, report.FeeNanoCents, report.Rows) || report.Digest == planDigest ||
			report.PaymentPlanId != payoutReimbursementPlanId(planDigest) || report.Rows[0].PaymentId != payoutReimbursementPaymentId(planDigest, completeNetwork) {
			t.Fatal("digest or ids", report.Digest, report.PaymentPlanId)
		}
		text := report.Text()
		for _, want := range []string{
			"prior reimbursements: allowed (--allow_prior_reimbursement); the 2 open manual reimbursement(s) of other plans listed below are not refused, and the digest binds exactly them\n",
			fmt.Sprintf("    prior reimbursement %s in plan %s: 10.000001 USDC, complete, reason %q\n", earlier.Rows[0].PaymentId, earlier.PaymentPlanId, payoutReimbursementTestReason),
			fmt.Sprintf("    prior reimbursement %s in plan %s: 0.500000 USDC, not complete, reason %q\n", earlier.Rows[1].PaymentId, earlier.PaymentPlanId, payoutReimbursementTestReason),
			"digest " + report.Digest + "\n",
		} {
			if !strings.Contains(text, want) {
				t.Fatalf("the dry run text lacks %q:\n%s", want, text)
			}
		}
		if strings.Contains(text, "refused: ") {
			t.Fatalf("the dry run text refuses:\n%s", text)
		}
	})
}

// The digest binds --allow_prior_reimbursement and the exact earlier
// reimbursements: it differs with and without the flag (the plan does not), and
// changes when another reimbursement of a listed network appears. A send with
// the other flag, or with a dry run's digest from before that reimbursement
// appeared, refuses and writes nothing.
func TestPayoutReimbursementAllowPriorDigestBindsTheFlagAndThePriors(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		defer clientSession.Cancel()
		circle := newPayoutReimbursementTestCircle(t)

		// without an earlier reimbursement, the flag alone changes the digest
		freshNetwork := payoutReimbursementTestNetwork(t, ctx, "synthetic-digest-fresh")
		freshWallet := payoutReimbursementTestWallet(t, ctx, freshNetwork, "SOL", payoutReimbursementTestAddress("SyntheticDigestFresh"), true)
		freshData := payoutReimbursementTestCsv(payoutReimbursementTestRow(freshNetwork, freshWallet, "4.2"))
		plain, err := payoutReimbursementTestRun(clientSession, freshData, payoutReimbursementPriorTestReason, false, "")
		if err != nil {
			t.Fatal(err)
		}
		allowing := payoutReimbursementPriorTestDryRun(t, clientSession, freshData, payoutReimbursementPriorTestReason)
		if allowing.Digest == plain.Digest || allowing.PaymentPlanId != plain.PaymentPlanId {
			t.Fatal("the flag did not change the digest, or changed the plan", plain.Digest, allowing.Digest)
		}

		networkId := payoutReimbursementTestNetwork(t, ctx, "synthetic-digest-prior")
		walletId := payoutReimbursementTestWallet(t, ctx, networkId, "SOL", payoutReimbursementTestAddress("SyntheticDigestPrior"), true)
		earlier := payoutReimbursementPriorTestSend(t, clientSession, payoutReimbursementTestCsv(payoutReimbursementTestRow(networkId, walletId, "10")), payoutReimbursementTestReason)
		data := payoutReimbursementTestCsv(payoutReimbursementTestRow(networkId, walletId, "2.5"))
		stale := payoutReimbursementPriorTestDryRun(t, clientSession, data, payoutReimbursementPriorTestReason)
		if len(stale.Rows[0].PriorReimbursements) != 1 || stale.Rows[0].PriorReimbursements[0].PaymentId != earlier.Rows[0].PaymentId {
			t.Fatalf("prior reimbursements %+v", stale.Rows[0].PriorReimbursements)
		}

		// a send with the other flag refuses
		before := payoutReimbursementTestWrites(t, ctx)
		sends := len(circle.sends)
		if _, err := payoutReimbursementTestRun(clientSession, freshData, payoutReimbursementPriorTestReason, true, allowing.Digest); !errors.Is(err, ErrPayoutReimbursementDigestMismatch) {
			t.Fatalf("the flag's digest sent without the flag: %v", err)
		}
		if _, err := payoutReimbursementTestRunAllowingPrior(clientSession, freshData, payoutReimbursementPriorTestReason, true, plain.Digest); !errors.Is(err, ErrPayoutReimbursementDigestMismatch) {
			t.Fatalf("the digest without the flag sent with it: %v", err)
		}
		if _, err := payoutReimbursementTestRun(clientSession, data, payoutReimbursementPriorTestReason, true, stale.Digest); !errors.Is(err, ErrPayoutReimbursementRefused) || !strings.Contains(err.Error(), "already has manual reimbursement") {
			t.Fatalf("the flag's digest sent without the flag, for a network with an earlier reimbursement: %v", err)
		}
		if after := payoutReimbursementTestWrites(t, ctx); after != before || len(circle.sends) != sends {
			t.Fatal("a refused send wrote or sent", before, after)
		}

		// another reimbursement of the network appears after the dry run
		otherData := payoutReimbursementTestCsv(payoutReimbursementTestRow(networkId, walletId, "0.333333"))
		other := payoutReimbursementPriorTestDryRun(t, clientSession, otherData, "synthetic other obligation")
		if report, err := payoutReimbursementTestRunAllowingPrior(clientSession, otherData, "synthetic other obligation", true, other.Digest); err != nil || !report.Created {
			t.Fatalf("the other reimbursement %+v %v", report, err)
		}
		fresh := payoutReimbursementPriorTestDryRun(t, clientSession, data, payoutReimbursementPriorTestReason)
		if fresh.Digest == stale.Digest || fresh.PaymentPlanId != stale.PaymentPlanId || len(fresh.Rows[0].PriorReimbursements) != 2 {
			t.Fatalf("the digest did not change with the new reimbursement %+v", fresh)
		}
		before = payoutReimbursementTestWrites(t, ctx)
		sends = len(circle.sends)
		if _, err := payoutReimbursementTestRunAllowingPrior(clientSession, data, payoutReimbursementPriorTestReason, true, stale.Digest); !errors.Is(err, ErrPayoutReimbursementDigestMismatch) {
			t.Fatalf("a stale dry run's digest: %v", err)
		}
		if after := payoutReimbursementTestWrites(t, ctx); after != before || len(circle.sends) != sends || payoutReimbursementTestNetworkPaymentCount(t, ctx, networkId) != 2 {
			t.Fatal("the stale send wrote or sent", before, after)
		}
	})
}

// A send with --allow_prior_reimbursement creates exactly one new payment and
// marker per row and pays each once; each marker carries the digest, the flag
// and its network's earlier payments. The run's own payments are never prior
// to it: a repeated send with the same digest matches, creates and sends
// nothing, and a dry run afterwards prints the same digest.
func TestPayoutReimbursementAllowPriorSendCreatesOnePaymentPerRowOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		defer clientSession.Cancel()
		circle := newPayoutReimbursementTestCircle(t)

		networkA := payoutReimbursementTestNetwork(t, ctx, "synthetic-once-a")
		walletA := payoutReimbursementTestWallet(t, ctx, networkA, "SOL", payoutReimbursementTestAddress("SyntheticOnceA"), true)
		networkB := payoutReimbursementTestNetwork(t, ctx, "synthetic-once-b")
		walletB := payoutReimbursementTestWallet(t, ctx, networkB, "MATIC", "0x00000000000000000000000000000000000000e1", true)
		networkC := payoutReimbursementTestNetwork(t, ctx, "synthetic-once-c")
		walletC := payoutReimbursementTestWallet(t, ctx, networkC, "SOL", payoutReimbursementTestAddress("SyntheticOnceC"), true)
		earlier := payoutReimbursementPriorTestSend(t, clientSession, payoutReimbursementTestCsv(
			payoutReimbursementTestRow(networkA, walletA, "10"),
			payoutReimbursementTestRow(networkB, walletB, "20"),
		), payoutReimbursementTestReason)

		data := payoutReimbursementTestCsv(
			payoutReimbursementTestRow(networkA, walletA, "1.25"),
			payoutReimbursementTestRow(networkB, walletB, "0.75"),
			payoutReimbursementTestRow(networkC, walletC, "3.5"),
		)
		dry := payoutReimbursementPriorTestDryRun(t, clientSession, data, payoutReimbursementPriorTestReason)
		networkIds := []server.Id{networkA, networkB, networkC}
		counts := []int{}
		for _, networkId := range networkIds {
			counts = append(counts, payoutReimbursementTestNetworkPaymentCount(t, ctx, networkId))
		}
		if !slices.Equal(counts, []int{1, 1, 0}) {
			t.Fatal("payments before the send", counts)
		}
		sends := len(circle.sends)

		report, err := payoutReimbursementTestRunAllowingPrior(clientSession, data, payoutReimbursementPriorTestReason, true, dry.Digest)
		if err != nil || !report.Created || report.Digest != dry.Digest || report.PlanPaymentCount != 3 || len(circle.sends) != sends+3 {
			t.Fatalf("send %+v %v, %d transfers", report, err, len(circle.sends)-sends)
		}
		for i, networkId := range networkIds {
			if count := payoutReimbursementTestNetworkPaymentCount(t, ctx, networkId); count != counts[i]+1 {
				t.Fatalf("network %d has %d payments, want %d", i, count, counts[i]+1)
			}
			row := report.Rows[i]
			if row.PaymentId != dry.Rows[i].PaymentId || row.State != "submitted" || row.Outcome != "sent to Circle" ||
				fmt.Sprintf("%f", circle.sends[sends+i].amount) != row.CircleAmount {
				t.Fatalf("row %d %+v, transfer %+v", i, row, circle.sends[sends+i])
			}
			var wantPriors []server.Id
			if i < 2 {
				wantPriors = []server.Id{earlier.Rows[i].PaymentId}
			}
			marker, err := model.GetManualReimbursementMarker(ctx, row.PaymentId)
			if err != nil || marker == nil || marker.Digest != dry.Digest || marker.Reason != payoutReimbursementPriorTestReason || marker.PaymentPlanId != report.PaymentPlanId ||
				!marker.AllowPriorReimbursement || !slices.Equal(marker.PriorPaymentIds, wantPriors) {
				t.Fatalf("row %d marker %+v %v", i, marker, err)
			}
		}
		written := payoutReimbursementTestWrites(t, ctx)

		rerun, err := payoutReimbursementTestRunAllowingPrior(clientSession, data, payoutReimbursementPriorTestReason, true, dry.Digest)
		if err != nil || rerun.Created || rerun.Digest != dry.Digest || rerun.PlanPaymentCount != 3 || len(circle.sends) != sends+3 {
			t.Fatalf("the rerun created or sent: %+v %v, %d transfers", rerun, err, len(circle.sends)-sends)
		}
		for i, row := range rerun.Rows {
			if row.PaymentId != report.Rows[i].PaymentId || row.Outcome != "pending at Circle" || len(row.PriorReimbursements) != len(dry.Rows[i].PriorReimbursements) {
				t.Fatalf("rerun row %d %+v", i, row)
			}
		}
		// the rerun added only its status reads' journal entries
		after := payoutReimbursementTestWrites(t, ctx)
		if after[0] != written[0] || after[2] != written[2] || after[3] != written[3] {
			t.Fatal("the rerun created payments or tasks", written, after)
		}
		for i, networkId := range networkIds {
			if count := payoutReimbursementTestNetworkPaymentCount(t, ctx, networkId); count != counts[i]+1 {
				t.Fatalf("network %d has %d payments after the rerun, want %d", i, count, counts[i]+1)
			}
		}

		again := payoutReimbursementPriorTestDryRun(t, clientSession, data, payoutReimbursementPriorTestReason)
		if again.Digest != dry.Digest || again.PlanPaymentCount != 3 || again.Rows[0].State != "submitted" {
			t.Fatalf("dry run after the send %+v", again)
		}
	})
}

// The same rows and reason name the same plan with or without
// --allow_prior_reimbursement, so the flag never pays them twice: after a send
// without it, the flag's dry run finds that plan's payments as its own, not as
// prior reimbursements, and its send refuses because the plan exists with other
// markers. Nothing is written or sent.
func TestPayoutReimbursementAllowPriorNeverRepaysTheSameRowsAndReason(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		defer clientSession.Cancel()
		circle := newPayoutReimbursementTestCircle(t)

		networkId := payoutReimbursementTestNetwork(t, ctx, "synthetic-prior-same")
		walletId := payoutReimbursementTestWallet(t, ctx, networkId, "SOL", payoutReimbursementTestAddress("SyntheticPriorSame"), true)
		data := payoutReimbursementTestCsv(payoutReimbursementTestRow(networkId, walletId, "7.5"))
		sent := payoutReimbursementPriorTestSend(t, clientSession, data, payoutReimbursementTestReason)

		dry := payoutReimbursementPriorTestDryRun(t, clientSession, data, payoutReimbursementTestReason)
		if dry.PaymentPlanId != sent.PaymentPlanId || dry.Rows[0].PaymentId != sent.Rows[0].PaymentId || dry.PlanPaymentCount != 1 ||
			dry.Rows[0].PriorReimbursements != nil || dry.Digest == sent.Digest {
			t.Fatalf("dry run with the flag %+v", dry)
		}
		before := payoutReimbursementTestWrites(t, ctx)
		sends := len(circle.sends)
		if report, err := payoutReimbursementTestRunAllowingPrior(clientSession, data, payoutReimbursementTestReason, true, dry.Digest); !errors.Is(err, model.ErrManualReimbursementPlanChanged) || report.Created {
			t.Fatalf("the flag's send of the same rows and reason: %v", err)
		}
		if after := payoutReimbursementTestWrites(t, ctx); after != before || len(circle.sends) != sends || payoutReimbursementTestNetworkPaymentCount(t, ctx, networkId) != 1 {
			t.Fatal("the refused send wrote or sent", before, after)
		}
	})
}

// With --allow_prior_reimbursement the send still refuses while USDC payments
// are off, before it writes anything; the dry run says they are off.
func TestPayoutReimbursementAllowPriorSendRefusesWhilePaymentsOff(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		defer clientSession.Cancel()
		circle := newPayoutReimbursementTestCircle(t)

		networkId := payoutReimbursementTestNetwork(t, ctx, "synthetic-prior-off")
		walletId := payoutReimbursementTestWallet(t, ctx, networkId, "SOL", payoutReimbursementTestAddress("SyntheticPriorOff"), true)
		payoutReimbursementPriorTestSend(t, clientSession, payoutReimbursementTestCsv(payoutReimbursementTestRow(networkId, walletId, "10")), payoutReimbursementTestReason)
		data := payoutReimbursementTestCsv(payoutReimbursementTestRow(networkId, walletId, "2.5"))
		dry := payoutReimbursementPriorTestDryRun(t, clientSession, data, payoutReimbursementPriorTestReason)

		usdcSwitchTestDeclare(t, "payments: off\n")
		off := payoutReimbursementPriorTestDryRun(t, clientSession, data, payoutReimbursementPriorTestReason)
		if off.UsdcPayments != "off" || off.Digest != dry.Digest || !strings.Contains(off.Text(), "usdc payments: off") {
			t.Fatalf("dry run while off %+v", off)
		}
		before := payoutReimbursementTestWrites(t, ctx)
		sends, reads := len(circle.sends), circle.reads
		if _, err := payoutReimbursementTestRunAllowingPrior(clientSession, data, payoutReimbursementPriorTestReason, true, dry.Digest); !errors.Is(err, ErrUsdcPaymentsOff) {
			t.Fatalf("send while off: %v", err)
		}
		if after := payoutReimbursementTestWrites(t, ctx); after != before || len(circle.sends) != sends || circle.reads != reads ||
			payoutReimbursementTestNetworkPaymentCount(t, ctx, networkId) != 1 {
			t.Fatal("a refused send wrote or called Circle", before, after)
		}
	})
}

// --allow_prior_reimbursement waives only the earlier-reimbursement refusal: a
// foreign, inactive, refused-address or unsupported-chain wallet is still
// refused for a network with an earlier reimbursement, and the send still
// refuses without the earning policy. Nothing is written or sent.
func TestPayoutReimbursementAllowPriorKeepsEveryOtherRefusal(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		defer clientSession.Cancel()
		circle := newPayoutReimbursementTestCircle(t)

		owner := payoutReimbursementTestNetwork(t, ctx, "synthetic-prior-owner")
		ownerWallet := payoutReimbursementTestWallet(t, ctx, owner, "SOL", payoutReimbursementTestAddress("SyntheticPriorOwner"), true)
		payoutReimbursementPriorTestSend(t, clientSession, payoutReimbursementTestCsv(payoutReimbursementTestRow(owner, ownerWallet, "10")), payoutReimbursementTestReason)
		other := payoutReimbursementTestNetwork(t, ctx, "synthetic-prior-other")
		otherWallet := payoutReimbursementTestWallet(t, ctx, other, "SOL", payoutReimbursementTestAddress("SyntheticPriorOther"), true)
		inactiveWallet := payoutReimbursementTestWallet(t, ctx, owner, "SOL", payoutReimbursementTestAddress("SyntheticPriorInactive"), false)
		attackerWallet := payoutReimbursementTestWallet(t, ctx, owner, "SOL", model.ManualReimbursementRefusedAddress, true)
		taoWallet := payoutReimbursementTestWallet(t, ctx, owner, "TAO", payoutReimbursementTestAddress("SyntheticPriorTao"), true)
		sends, reads := len(circle.sends), circle.reads

		for name, test := range map[string]struct {
			walletId server.Id
			want     string
		}{
			"foreign wallet":    {otherWallet, fmt.Sprintf("belongs to network %s, not %s", other, owner)},
			"inactive wallet":   {inactiveWallet, "is not active"},
			"refused address":   {attackerWallet, "is the refused address " + model.ManualReimbursementRefusedAddress},
			"unsupported chain": {taoWallet, `is on "TAO"; only SOL and MATIC are paid`},
		} {
			data := payoutReimbursementTestCsv(payoutReimbursementTestRow(owner, test.walletId, "1"))
			for _, send := range []bool{false, true} {
				before := payoutReimbursementTestWrites(t, ctx)
				report, err := payoutReimbursementTestRunAllowingPrior(clientSession, data, payoutReimbursementPriorTestReason, send, "any")
				if !errors.Is(err, ErrPayoutReimbursementRefused) || !strings.Contains(err.Error(), test.want) || report.Digest != "" {
					t.Fatalf("%s (send %t): %v; want a refusal saying %q", name, send, err, test.want)
				}
				if after := payoutReimbursementTestWrites(t, ctx); after != before {
					t.Fatalf("%s (send %t): the refusal wrote %v -> %v", name, send, before, after)
				}
			}
		}

		data := payoutReimbursementTestCsv(payoutReimbursementTestRow(owner, ownerWallet, "2.5"))
		t.Cleanup(server.Config.PushSimpleResource("sn.yml", []byte("not: a transition")))
		dry := payoutReimbursementPriorTestDryRun(t, clientSession, data, payoutReimbursementPriorTestReason)
		if !strings.HasPrefix(dry.EarningPolicy, "unavailable: ") {
			t.Fatalf("earning policy %q", dry.EarningPolicy)
		}
		before := payoutReimbursementTestWrites(t, ctx)
		if report, err := payoutReimbursementTestRunAllowingPrior(clientSession, data, payoutReimbursementPriorTestReason, true, dry.Digest); err == nil || report.Created ||
			errors.Is(err, ErrPayoutReimbursementDigestMismatch) || errors.Is(err, ErrPayoutReimbursementRefused) {
			t.Fatalf("send without the earning policy: %v", err)
		}
		if after := payoutReimbursementTestWrites(t, ctx); after != before || len(circle.sends) != sends || circle.reads != reads ||
			payoutReimbursementTestNetworkPaymentCount(t, ctx, owner) != 1 {
			t.Fatal("a refusal wrote or called Circle", before, after)
		}
	})
}
