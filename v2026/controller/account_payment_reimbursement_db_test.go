// Database tests of `payouts reimburse`, with a fake Circle and an injected
// clock. Every test uses synthetic networks and wallets on its own test
// database.
package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// The injected clock of these tests.
var payoutReimbursementTestNow = time.Date(2026, 10, 26, 14, 0, 0, 0, time.UTC)

func payoutReimbursementTestClock() time.Time {
	return payoutReimbursementTestNow
}

const payoutReimbursementTestReason = "synthetic reimbursement"

func payoutReimbursementTestNetwork(t testing.TB, ctx context.Context, name string) server.Id {
	t.Helper()
	networkId := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO network (network_id, network_name, admin_user_id) VALUES ($1, $2, $3)`,
			networkId,
			name+"-"+networkId.String(),
			server.NewId(),
		))
	})
	return networkId
}

func payoutReimbursementTestWallet(t testing.TB, ctx context.Context, networkId server.Id, blockchain string, address string, active bool) server.Id {
	t.Helper()
	walletId := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO account_wallet (
					wallet_id, network_id, wallet_type, blockchain, wallet_address, active, default_token_type, create_time
				) VALUES ($1, $2, 'external', $3, $4, $5, 'USDC', $6)
			`,
			walletId,
			networkId,
			blockchain,
			address,
			active,
			payoutReimbursementTestNow.Add(-24*time.Hour),
		))
	})
	return walletId
}

// A synthetic Solana-style address, unique per call.
func payoutReimbursementTestAddress(prefix string) string {
	return prefix + strings.ReplaceAll(server.NewId().String(), "-", "")
}

type payoutReimbursementTestSend struct {
	key     server.Id
	amount  float64
	address string
	network string
}

// A fake Circle. Every transfer first passes the boundary the real client
// checks before its POST (requireCircleProviderTransfer).
type payoutReimbursementTestCircle struct {
	state     string
	txHash    string
	submitErr error
	sends     []payoutReimbursementTestSend
	reads     int
}

// Also declares USDC payments on, whatever the environment declares.
func newPayoutReimbursementTestCircle(t testing.TB) *payoutReimbursementTestCircle {
	t.Helper()
	usdcSwitchTestDeclare(t, "payments: on\n")
	circle := &payoutReimbursementTestCircle{state: "INITIATED"}
	previous := NewCircleClient()
	SetCircleClient(&mockCircleApiClient{
		CreateTransferTransactionFunc: func(ctx context.Context, key server.Id, amount float64, address string, network string) (*CreateTransferTransactionResult, error) {
			if err := requireCircleProviderTransfer(ctx, circleTransferArguments{IdempotencyKey: key, Amount: amount, Destination: address, Network: network}); err != nil {
				return nil, err
			}
			if circle.submitErr != nil {
				return nil, circle.submitErr
			}
			circle.sends = append(circle.sends, payoutReimbursementTestSend{key: key, amount: amount, address: address, network: network})
			return &CreateTransferTransactionResult{Id: "synthetic-transfer-" + key.String(), State: "INITIATED"}, nil
		},
		GetTransactionFunc: func(ctx context.Context, id string) (*GetTransactionResult, error) {
			circle.reads += 1
			return &GetTransactionResult{
				Transaction:       CircleTransaction{Id: id, State: circle.state, TxHash: circle.txHash},
				ResponseBodyBytes: []byte(`{"state":"` + circle.state + `"}`),
			}, nil
		},
		EstimateTransferFeeFunc: defaultEstimateFeeHandler,
	})
	t.Cleanup(func() {
		SetCircleClient(previous)
	})
	return circle
}

func payoutReimbursementTestCsv(rows ...[3]string) []byte {
	b := &strings.Builder{}
	b.WriteString(payoutReimbursementCsvHeader + "\n")
	for _, row := range rows {
		fmt.Fprintf(b, "%s,%s,%s\n", row[0], row[1], row[2])
	}
	return []byte(b.String())
}

func payoutReimbursementTestRow(networkId server.Id, walletId server.Id, amount string) [3]string {
	return [3]string{networkId.String(), walletId.String(), amount}
}

func payoutReimbursementTestRun(clientSession *session.ClientSession, data []byte, reason string, send bool, expect string) (*PayoutReimbursementReport, error) {
	return payoutsReimburse(clientSession, data, reason, send, expect, payoutReimbursementTestClock)
}

// The dry run's digest for the rows.
func payoutReimbursementTestDigest(t testing.TB, clientSession *session.ClientSession, data []byte) string {
	t.Helper()
	report, err := payoutReimbursementTestRun(clientSession, data, payoutReimbursementTestReason, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Digest) != 64 {
		t.Fatal("the dry run printed no digest")
	}
	return report.Digest
}

// The rows the command could write: payments, payment audit events (markers
// and attempts), queued tasks and payout wallet pointers.
func payoutReimbursementTestWrites(t testing.TB, ctx context.Context) (counts [4]int64) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
				SELECT
					(SELECT COUNT(*) FROM account_payment),
					(SELECT COUNT(*) FROM audit_account_payment),
					(SELECT COUNT(*) FROM pending_task),
					(SELECT COUNT(*) FROM payout_wallet)
			`,
		).Scan(&counts[0], &counts[1], &counts[2], &counts[3]))
	})
	return
}

func payoutReimbursementTestAdvanceTasks(t testing.TB, ctx context.Context, paymentId server.Id) (count int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT COUNT(*) FROM pending_task WHERE function_name LIKE '%AdvancePayment' AND args_json LIKE '%' || $1 || '%'`,
			paymentId.String(),
		).Scan(&count))
	})
	return
}

func payoutReimbursementTestPayment(t testing.TB, ctx context.Context, paymentId server.Id) *model.AccountPayment {
	t.Helper()
	payment, err := model.GetPayment(ctx, paymentId)
	if err != nil || payment == nil {
		t.Fatalf("payment %s: %v", paymentId, err)
	}
	return payment
}

// An unmarked pending payment of the network with no sweeps, created at `createTime`.
func payoutReimbursementTestOrdinaryPayment(t testing.TB, ctx context.Context, networkId server.Id, walletId server.Id, createTime time.Time) server.Id {
	t.Helper()
	paymentId := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO account_payment (
					payment_id, payment_plan_id, network_id, wallet_id, payout_byte_count, payout_nano_cents, min_sweep_time, create_time
				) VALUES ($1, $2, $3, $4, 0, $5, $6, $6)
			`,
			paymentId,
			server.NewId(),
			networkId,
			walletId,
			model.UsdToNanoCents(2),
			createTime,
		))
	})
	return paymentId
}

// The dry run only reads: in a process whose sessions PostgreSQL holds read
// only, it prints every row, the fee handling and the digest, writes no row
// and never calls Circle.
func TestPayoutReimbursementDryRunWritesNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		defer clientSession.Cancel()
		circle := newPayoutReimbursementTestCircle(t)

		solanaNetwork := payoutReimbursementTestNetwork(t, ctx, "synthetic-solana")
		solanaAddress := payoutReimbursementTestAddress("SyntheticSol")
		solanaWallet := payoutReimbursementTestWallet(t, ctx, solanaNetwork, "SOL", solanaAddress, true)
		maticNetwork := payoutReimbursementTestNetwork(t, ctx, "synthetic-matic")
		maticWallet := payoutReimbursementTestWallet(t, ctx, maticNetwork, "MATIC", "0x00000000000000000000000000000000000000C1", true)
		data := payoutReimbursementTestCsv(
			payoutReimbursementTestRow(solanaNetwork, solanaWallet, "123.456789"),
			payoutReimbursementTestRow(maticNetwork, maticWallet, "0.987654"),
		)

		before := payoutReimbursementTestWrites(t, ctx)
		server.RequirePgReadOnly()
		t.Cleanup(server.Testing_ReleasePgReadOnly)
		report, err := payoutReimbursementTestRun(clientSession, data, payoutReimbursementTestReason, false, "")
		// the process really was read only: a write on its path fails
		writeRefused := false
		func() {
			defer func() {
				writeRefused = recover() != nil
			}()
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO audit_account_payment (event_id, payment_id, event_type) VALUES ($1, $1, 'synthetic_write_probe')`, server.NewId()))
			}, server.OptNoRetry())
		}()
		server.Testing_ReleasePgReadOnly()
		if err != nil {
			t.Fatal(err)
		}
		if !writeRefused {
			t.Fatal("the dry run's process accepted a write")
		}
		if after := payoutReimbursementTestWrites(t, ctx); after != before {
			t.Fatal("the dry run wrote", before, after)
		}
		if len(circle.sends) != 0 || circle.reads != 0 {
			t.Fatal("the dry run called Circle", circle.sends, circle.reads)
		}

		if report.Mode != "dry_run" || len(report.Digest) != 64 || report.PlanPaymentCount != 0 || len(report.Rows) != 2 ||
			report.AmountNanoCents != 124_444_443_000 || report.PayoutNanoCents != report.AmountNanoCents+2*10_000_000 ||
			report.UsdcPayments != "on" || report.Created {
			t.Fatalf("dry run report %+v", report)
		}
		solana, matic := report.Rows[0], report.Rows[1]
		if solana.WalletAddress != solanaAddress || solana.Chain != "SOL" || solana.Amount != "123.456789" || solana.PayoutNanoCents != 123_466_789_000 ||
			solana.CircleAmount != "123.456789" || solana.PaymentId != payoutReimbursementPaymentId(report.Digest, solanaNetwork) || solana.State != "" {
			t.Fatalf("solana row %+v", solana)
		}
		if matic.Chain != "MATIC" || matic.Amount != "0.987654" || matic.PayoutNanoCents != 997_654_000 || matic.CircleAmount != "0.987654" {
			t.Fatalf("matic row %+v", matic)
		}
		text := report.Text()
		for _, want := range []string{
			"mode dry_run", "digest " + report.Digest, "amount 123.456789 USDC, payout_nano_cents 123466789000, circle amount 123.456789",
			"fee: the sender deducts a fixed 0.010000 USDC (10000000 nano cents)", "total: 124.444443 USDC to 2 wallet(s)",
			solanaAddress, "0 payment(s) created",
		} {
			if !strings.Contains(text, want) {
				t.Fatalf("the dry run text lacks %q:\n%s", want, text)
			}
		}
	})
}

// Each refused row names its problem, and nothing is written or sent, in a dry
// run or a send.
func TestPayoutReimbursementRefusesInvalidRows(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		defer clientSession.Cancel()
		circle := newPayoutReimbursementTestCircle(t)

		owner := payoutReimbursementTestNetwork(t, ctx, "synthetic-owner")
		ownerWallet := payoutReimbursementTestWallet(t, ctx, owner, "SOL", payoutReimbursementTestAddress("SyntheticOwner"), true)
		other := payoutReimbursementTestNetwork(t, ctx, "synthetic-other")
		otherWallet := payoutReimbursementTestWallet(t, ctx, other, "SOL", payoutReimbursementTestAddress("SyntheticOther"), true)
		inactive := payoutReimbursementTestNetwork(t, ctx, "synthetic-inactive")
		inactiveWallet := payoutReimbursementTestWallet(t, ctx, inactive, "SOL", payoutReimbursementTestAddress("SyntheticInactive"), false)
		attacker := payoutReimbursementTestNetwork(t, ctx, "synthetic-attacker")
		attackerWallet := payoutReimbursementTestWallet(t, ctx, attacker, "SOL", model.ManualReimbursementRefusedAddress, true)
		attackerCase := payoutReimbursementTestNetwork(t, ctx, "synthetic-attacker-case")
		attackerCaseWallet := payoutReimbursementTestWallet(t, ctx, attackerCase, "SOL", " "+strings.ToLower(model.ManualReimbursementRefusedAddress), true)
		tao := payoutReimbursementTestNetwork(t, ctx, "synthetic-tao")
		taoWallet := payoutReimbursementTestWallet(t, ctx, tao, "TAO", payoutReimbursementTestAddress("SyntheticTao"), true)
		missingNetwork := server.NewId()
		orphanWallet := payoutReimbursementTestWallet(t, ctx, missingNetwork, "SOL", payoutReimbursementTestAddress("SyntheticOrphan"), true)

		for name, test := range map[string]struct {
			data   []byte
			reason string
			want   string
		}{
			"foreign wallet":      {payoutReimbursementTestCsv(payoutReimbursementTestRow(owner, otherWallet, "1")), payoutReimbursementTestReason, fmt.Sprintf("belongs to network %s, not %s", other, owner)},
			"inactive wallet":     {payoutReimbursementTestCsv(payoutReimbursementTestRow(inactive, inactiveWallet, "1")), payoutReimbursementTestReason, "is not active"},
			"attacker wallet":     {payoutReimbursementTestCsv(payoutReimbursementTestRow(attacker, attackerWallet, "1")), payoutReimbursementTestReason, "is the refused address " + model.ManualReimbursementRefusedAddress},
			"attacker any case":   {payoutReimbursementTestCsv(payoutReimbursementTestRow(attackerCase, attackerCaseWallet, "1")), payoutReimbursementTestReason, "is the refused address"},
			"unsupported chain":   {payoutReimbursementTestCsv(payoutReimbursementTestRow(tao, taoWallet, "1")), payoutReimbursementTestReason, `is on "TAO"; only SOL and MATIC are paid`},
			"unknown wallet":      {payoutReimbursementTestCsv(payoutReimbursementTestRow(owner, server.NewId(), "1")), payoutReimbursementTestReason, "does not exist"},
			"unknown network":     {payoutReimbursementTestCsv(payoutReimbursementTestRow(missingNetwork, orphanWallet, "1")), payoutReimbursementTestReason, fmt.Sprintf("network %s does not exist", missingNetwork)},
			"duplicate network":   {payoutReimbursementTestCsv(payoutReimbursementTestRow(owner, ownerWallet, "1"), payoutReimbursementTestRow(owner, ownerWallet, "2")), payoutReimbursementTestReason, "repeats line 2"},
			"seven decimals":      {payoutReimbursementTestCsv(payoutReimbursementTestRow(owner, ownerWallet, "1.0000001")), payoutReimbursementTestReason, "at most 6 decimals"},
			"zero amount":         {payoutReimbursementTestCsv(payoutReimbursementTestRow(owner, ownerWallet, "0")), payoutReimbursementTestReason, "is not positive"},
			"no reason":           {payoutReimbursementTestCsv(payoutReimbursementTestRow(owner, ownerWallet, "1")), "  ", "--reason must be set"},
			"one bad row of many": {payoutReimbursementTestCsv(payoutReimbursementTestRow(owner, ownerWallet, "1"), payoutReimbursementTestRow(inactive, inactiveWallet, "1")), payoutReimbursementTestReason, "line 3: wallet"},
		} {
			for _, send := range []bool{false, true} {
				before := payoutReimbursementTestWrites(t, ctx)
				report, err := payoutReimbursementTestRun(clientSession, test.data, test.reason, send, "any")
				if !errors.Is(err, ErrPayoutReimbursementRefused) || !strings.Contains(err.Error(), test.want) {
					t.Fatalf("%s (send %t): %v; want a refusal saying %q", name, send, err, test.want)
				}
				if report == nil || report.Digest != "" || !strings.Contains(report.Text(), "refused: ") {
					t.Fatalf("%s (send %t): the refusal printed a digest or no problem", name, send)
				}
				if after := payoutReimbursementTestWrites(t, ctx); after != before {
					t.Fatalf("%s (send %t): the refusal wrote %v -> %v", name, send, before, after)
				}
			}
		}
		if len(circle.sends) != 0 || circle.reads != 0 {
			t.Fatal("a refusal called Circle")
		}
	})
}

// The send creates one plan with one payment and marker per row, each with its
// wallet pinned, and asks Circle for exactly each listed amount on the wallet's
// chain. The taskworker then follows each transfer, and completion records it
// as any payout.
func TestPayoutReimbursementSendPaysExactlyTheListedAmounts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		defer clientSession.Cancel()
		circle := newPayoutReimbursementTestCircle(t)

		type listed struct {
			networkId server.Id
			walletId  server.Id
			address   string
			chain     string
			amount    string
		}
		rows := []*listed{}
		for i, entry := range [][2]string{{"SOL", "123.456789"}, {"SOL", "0.000001"}, {"SOL", "98.765432"}, {"MATIC", "0.987654"}} {
			networkId := payoutReimbursementTestNetwork(t, ctx, fmt.Sprintf("synthetic-send-%d", i))
			address := payoutReimbursementTestAddress("SyntheticSend")
			if entry[0] == "MATIC" {
				address = "0x" + strings.ReplaceAll(server.NewId().String(), "-", "") + "00000000"
			}
			rows = append(rows, &listed{networkId: networkId, walletId: payoutReimbursementTestWallet(t, ctx, networkId, entry[0], address, true), address: address, chain: entry[0], amount: entry[1]})
		}
		csvRows := [][3]string{}
		for _, row := range rows {
			csvRows = append(csvRows, payoutReimbursementTestRow(row.networkId, row.walletId, row.amount))
		}
		data := payoutReimbursementTestCsv(csvRows...)
		digest := payoutReimbursementTestDigest(t, clientSession, data)

		report, err := payoutReimbursementTestRun(clientSession, data, payoutReimbursementTestReason, true, digest)
		if err != nil {
			t.Fatal(err)
		}
		if !report.Created || report.Mode != "send" || report.PlanPaymentCount != len(rows) || report.PaymentPlanId != payoutReimbursementPlanId(digest) {
			t.Fatalf("send report %+v", report)
		}
		if len(circle.sends) != len(rows) {
			t.Fatalf("%d transfers for %d rows", len(circle.sends), len(rows))
		}
		for i, row := range rows {
			send := circle.sends[i]
			if send.address != row.address || send.network != row.chain || fmt.Sprintf("%f", send.amount) != row.amount {
				t.Fatalf("row %d: Circle was asked for %f to %s on %s; want %s to %s on %s", i, send.amount, send.address, send.network, row.amount, row.address, row.chain)
			}
			reported := report.Rows[i]
			payment := payoutReimbursementTestPayment(t, ctx, reported.PaymentId)
			micro, _ := parsePayoutReimbursementAmount(row.amount)
			if payment.PaymentPlanId != report.PaymentPlanId || payment.NetworkId != row.networkId || *payment.WalletId != row.walletId ||
				payment.Payout != model.NanoCents(micro)*1000+10_000_000 || payment.PayoutByteCount != 0 || payment.SubsidyPayout != 0 ||
				payment.TokenAmount == nil || *payment.TokenAmount != send.amount || payment.PaymentRecord == nil ||
				*payment.PaymentRecord != "synthetic-transfer-"+send.key.String() || !payment.CreateTime.Equal(payoutReimbursementTestNow) {
				t.Fatalf("row %d payment %+v", i, payment)
			}
			if reported.State != "submitted" || reported.TransferId != *payment.PaymentRecord || reported.Outcome != "sent to Circle" {
				t.Fatalf("row %d report %+v", i, reported)
			}
			marker, err := model.GetManualReimbursementMarker(ctx, payment.PaymentId)
			if err != nil || marker == nil || marker.Digest != digest || marker.Reason != payoutReimbursementTestReason ||
				marker.WalletId != row.walletId || marker.WalletAddress != row.address || marker.AmountNanoCents != model.NanoCents(micro)*1000 ||
				marker.PayoutNanoCents != payment.Payout || marker.PaymentPlanId != report.PaymentPlanId {
				t.Fatalf("row %d marker %+v %v", i, marker, err)
			}
			if count := payoutReimbursementTestAdvanceTasks(t, ctx, payment.PaymentId); count != 1 {
				t.Fatalf("row %d: %d follow-up tasks, want 1", i, count)
			}
		}

		// the taskworker's follow-up completes each transfer as any payout
		circle.state = "COMPLETE"
		for i, reported := range report.Rows {
			circle.txHash = fmt.Sprintf("synthetic-chain-hash-%d", i)
			result, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: reported.PaymentId}, clientSession)
			if err != nil || !result.Complete {
				t.Fatalf("row %d completion %+v %v", i, result, err)
			}
			payment := payoutReimbursementTestPayment(t, ctx, reported.PaymentId)
			if !payment.Completed || payment.AttributionReviewRequired || payment.TxHash == nil || *payment.TxHash != circle.txHash {
				t.Fatalf("row %d completed payment %+v", i, payment)
			}
		}
		if len(circle.sends) != len(rows) {
			t.Fatal("completion sent again")
		}
	})
}

// The pin: the network's payout wallet is another of its wallets, yet the
// reimbursement is paid to the listed wallet, and the
// ordinary wallet update leaves it there. Without the pin, UpdatePaymentWallet
// re-points the payment to the payout wallet before the send, and this test
// fails.
func TestPayoutReimbursementPaysThePinnedWalletNotThePayoutWallet(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		defer clientSession.Cancel()
		circle := newPayoutReimbursementTestCircle(t)

		networkId := payoutReimbursementTestNetwork(t, ctx, "synthetic-pinned")
		pinnedAddress := payoutReimbursementTestAddress("SyntheticPinned")
		pinnedWallet := payoutReimbursementTestWallet(t, ctx, networkId, "SOL", pinnedAddress, true)
		payoutAddress := payoutReimbursementTestAddress("SyntheticPayout")
		payoutWallet := payoutReimbursementTestWallet(t, ctx, networkId, "SOL", payoutAddress, true)
		if err := model.SetPayoutWallet(ctx, networkId, payoutWallet); err != nil {
			t.Fatal(err)
		}

		data := payoutReimbursementTestCsv(payoutReimbursementTestRow(networkId, pinnedWallet, "45.678901"))
		digest := payoutReimbursementTestDigest(t, clientSession, data)
		report, err := payoutReimbursementTestRun(clientSession, data, payoutReimbursementTestReason, true, digest)
		if err != nil {
			t.Fatal(err)
		}
		if len(circle.sends) != 1 || circle.sends[0].address != pinnedAddress || fmt.Sprintf("%f", circle.sends[0].amount) != "45.678901" {
			t.Fatalf("Circle was asked for %+v; want 45.678901 to the pinned wallet %s, not the payout wallet %s", circle.sends, pinnedAddress, payoutAddress)
		}
		paymentId := report.Rows[0].PaymentId
		payment := payoutReimbursementTestPayment(t, ctx, paymentId)
		if *payment.WalletId != pinnedWallet || *payment.WalletAddress != pinnedAddress {
			t.Fatalf("the payment moved to wallet %s", *payment.WalletId)
		}

		// the regular payout run's follow-up runs the same path: the wallet stays
		// pinned and the transfer is not repeated
		model.UpdatePaymentWallet(ctx, paymentId)
		if result, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: paymentId}, clientSession); err != nil || result.Complete || result.Canceled {
			t.Fatalf("follow-up %+v %v", result, err)
		}
		payment = payoutReimbursementTestPayment(t, ctx, paymentId)
		if *payment.WalletId != pinnedWallet || len(circle.sends) != 1 {
			t.Fatal("the follow-up re-pointed or resent the reimbursement")
		}
	})
}

// A rerun with the same digest creates nothing and sends nothing again; it
// only checks on the transfers. Another reason for the same networks is
// refused while their reimbursements are open.
func TestPayoutReimbursementRerunIsIdempotent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		defer clientSession.Cancel()
		circle := newPayoutReimbursementTestCircle(t)

		networkA := payoutReimbursementTestNetwork(t, ctx, "synthetic-rerun-a")
		walletA := payoutReimbursementTestWallet(t, ctx, networkA, "SOL", payoutReimbursementTestAddress("SyntheticRerunA"), true)
		networkB := payoutReimbursementTestNetwork(t, ctx, "synthetic-rerun-b")
		walletB := payoutReimbursementTestWallet(t, ctx, networkB, "MATIC", "0x00000000000000000000000000000000000000b1", true)
		data := payoutReimbursementTestCsv(
			payoutReimbursementTestRow(networkA, walletA, "13.579246"),
			payoutReimbursementTestRow(networkB, walletB, "0.642086"),
		)
		digest := payoutReimbursementTestDigest(t, clientSession, data)

		first, err := payoutReimbursementTestRun(clientSession, data, payoutReimbursementTestReason, true, digest)
		if err != nil || !first.Created || len(circle.sends) != 2 {
			t.Fatalf("first send %+v %v, %d transfers", first, err, len(circle.sends))
		}
		written := payoutReimbursementTestWrites(t, ctx)

		second, err := payoutReimbursementTestRun(clientSession, data, payoutReimbursementTestReason, true, digest)
		if err != nil {
			t.Fatal(err)
		}
		if second.Created || second.PlanPaymentCount != 2 || len(circle.sends) != 2 || circle.reads != 2 {
			t.Fatalf("the rerun created or sent: %+v, %d transfers, %d reads", second, len(circle.sends), circle.reads)
		}
		for i, row := range second.Rows {
			if row.PaymentId != first.Rows[i].PaymentId || row.Outcome != "pending at Circle" || row.TransferId != first.Rows[i].TransferId {
				t.Fatalf("rerun row %d %+v", i, row)
			}
		}
		// the rerun added only its status reads' journal entries
		after := payoutReimbursementTestWrites(t, ctx)
		if after[0] != written[0] || after[2] != written[2] || after[3] != written[3] {
			t.Fatal("the rerun created payments or tasks", written, after)
		}

		// a dry run of the same rows now shows the payments
		dry, err := payoutReimbursementTestRun(clientSession, data, payoutReimbursementTestReason, false, "")
		if err != nil || dry.PlanPaymentCount != 2 || dry.Rows[0].State != "submitted" || !strings.Contains(dry.Text(), "circle transfer synthetic-transfer-") {
			t.Fatalf("dry run after the send %+v %v", dry, err)
		}

		// the same networks under another reason are open reimbursements
		otherReason := payoutReimbursementTestReason + " (edited)"
		other, err := payoutReimbursementTestRun(clientSession, data, otherReason, false, "")
		if !errors.Is(err, ErrPayoutReimbursementRefused) || !strings.Contains(err.Error(), "already has manual reimbursement") || len(other.Conflicts) != 2 {
			t.Fatalf("another reason for open reimbursements: %v", err)
		}
		before := payoutReimbursementTestWrites(t, ctx)
		if _, err := payoutReimbursementTestRun(clientSession, data, otherReason, true, other.Digest); !errors.Is(err, ErrPayoutReimbursementRefused) {
			t.Fatalf("another reason's send: %v", err)
		}
		if after := payoutReimbursementTestWrites(t, ctx); after != before || len(circle.sends) != 2 {
			t.Fatal("the refused send wrote or sent")
		}
	})
}

// While USDC payments are off, the dry run says so and the send refuses before
// writing anything. A send whose digest does not match refuses the same way.
func TestPayoutReimbursementSendRefusesWithoutPaymentsOrDigest(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		defer clientSession.Cancel()
		circle := newPayoutReimbursementTestCircle(t)

		networkId := payoutReimbursementTestNetwork(t, ctx, "synthetic-off")
		walletId := payoutReimbursementTestWallet(t, ctx, networkId, "SOL", payoutReimbursementTestAddress("SyntheticOff"), true)
		data := payoutReimbursementTestCsv(payoutReimbursementTestRow(networkId, walletId, "6.283185"))
		digest := payoutReimbursementTestDigest(t, clientSession, data)
		before := payoutReimbursementTestWrites(t, ctx)

		if _, err := payoutReimbursementTestRun(clientSession, data, payoutReimbursementTestReason, true, strings.Repeat("0", 64)); !errors.Is(err, ErrPayoutReimbursementDigestMismatch) {
			t.Fatalf("a stale digest: %v", err)
		}
		if _, err := payoutReimbursementTestRun(clientSession, data, payoutReimbursementTestReason+".", true, digest); !errors.Is(err, ErrPayoutReimbursementDigestMismatch) {
			t.Fatalf("a changed reason: %v", err)
		}

		usdcSwitchTestDeclare(t, "payments: off\n")
		dry, err := payoutReimbursementTestRun(clientSession, data, payoutReimbursementTestReason, false, "")
		if err != nil || dry.UsdcPayments != "off" || dry.Digest != digest || !strings.Contains(dry.Text(), "usdc payments: off") {
			t.Fatalf("dry run while off %+v %v", dry, err)
		}
		if _, err := payoutReimbursementTestRun(clientSession, data, payoutReimbursementTestReason, true, digest); !errors.Is(err, ErrUsdcPaymentsOff) {
			t.Fatalf("send while off: %v", err)
		}
		if after := payoutReimbursementTestWrites(t, ctx); after != before {
			t.Fatal("a refused send wrote", before, after)
		}
		if len(circle.sends) != 0 || circle.reads != 0 {
			t.Fatal("a refused send called Circle")
		}
	})
}

// A reimbursement that Circle refused before any transfer stays pending with
// its pinned wallet: the wallet update leaves it, the hung-payment job never
// cancels it however old it is, and a rerun pays it once.
func TestPayoutReimbursementSurvivesTheOrdinaryJobs(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		defer clientSession.Cancel()
		circle := newPayoutReimbursementTestCircle(t)

		networkId := payoutReimbursementTestNetwork(t, ctx, "synthetic-jobs")
		pinnedAddress := payoutReimbursementTestAddress("SyntheticJobsPinned")
		pinnedWallet := payoutReimbursementTestWallet(t, ctx, networkId, "SOL", pinnedAddress, true)
		payoutWallet := payoutReimbursementTestWallet(t, ctx, networkId, "SOL", payoutReimbursementTestAddress("SyntheticJobsPayout"), true)
		if err := model.SetPayoutWallet(ctx, networkId, payoutWallet); err != nil {
			t.Fatal(err)
		}
		data := payoutReimbursementTestCsv(payoutReimbursementTestRow(networkId, pinnedWallet, "2.718281"))
		digest := payoutReimbursementTestDigest(t, clientSession, data)

		// Circle's definitive invalid-destination refusal: no transfer exists
		circle.submitErr = &server.HttpStatusError{
			StatusCode:   http.StatusBadRequest,
			Status:       "400 Bad Request",
			ResponseBody: `{"code":155219,"message":"Invalid destination address."}`,
		}
		report, err := payoutReimbursementTestRun(clientSession, data, payoutReimbursementTestReason, true, digest)
		if err == nil || !report.Created {
			t.Fatalf("a refused destination was reported as sent: %+v %v", report, err)
		}
		paymentId := report.Rows[0].PaymentId
		payment := payoutReimbursementTestPayment(t, ctx, paymentId)
		if payment.CircleIdempotencyKey != nil || payment.PaymentRecord != nil || payment.Canceled || report.Rows[0].State != "pending" {
			t.Fatalf("the refused attempt was retained %+v", payment)
		}
		if count := payoutReimbursementTestAdvanceTasks(t, ctx, paymentId); count != 0 {
			t.Fatal("a payment without an attempt was queued for the taskworker")
		}

		model.UpdatePaymentWallet(ctx, paymentId)
		if payment := payoutReimbursementTestPayment(t, ctx, paymentId); *payment.WalletId != pinnedWallet {
			t.Fatal("the wallet update re-pointed the reimbursement")
		}

		// an ordinary payment of the same age is canceled by the job; the
		// reimbursement is not
		ordinary := payoutReimbursementTestOrdinaryPayment(t, ctx, networkId, payoutWallet, payoutReimbursementTestNow)
		if canceled := model.CancelHungAccountPayments(ctx, payoutReimbursementTestNow.Add(model.HungPaymentExpiration+24*time.Hour)); canceled != 1 {
			t.Fatalf("the hung-payment job canceled %d payments, want only the ordinary one", canceled)
		}
		if payment := payoutReimbursementTestPayment(t, ctx, ordinary); !payment.Canceled {
			t.Fatal("the hung-payment job did not run")
		}
		if payment := payoutReimbursementTestPayment(t, ctx, paymentId); payment.Canceled {
			t.Fatal("the hung-payment job canceled the reimbursement")
		}

		// the rerun pays it once, to the pinned wallet
		circle.submitErr = nil
		rerun, err := payoutReimbursementTestRun(clientSession, data, payoutReimbursementTestReason, true, digest)
		if err != nil || rerun.Created {
			t.Fatalf("rerun %+v %v", rerun, err)
		}
		if len(circle.sends) != 1 || circle.sends[0].address != pinnedAddress || fmt.Sprintf("%f", circle.sends[0].amount) != "2.718281" {
			t.Fatalf("the rerun sent %+v", circle.sends)
		}
		for range 2 {
			if _, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: paymentId}, clientSession); err != nil {
				t.Fatal(err)
			}
		}
		if len(circle.sends) != 1 {
			t.Fatal("a later advance sent the reimbursement again")
		}
	})
}

// A writer older than the pin may have re-pointed an unsent reimbursement. The
// admission refuses to send it anywhere but its pinned wallet, and the rerun
// restores the pin and pays the pinned wallet.
func TestPayoutReimbursementRestoresARepointedWallet(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		defer clientSession.Cancel()
		circle := newPayoutReimbursementTestCircle(t)

		networkId := payoutReimbursementTestNetwork(t, ctx, "synthetic-repointed")
		pinnedAddress := payoutReimbursementTestAddress("SyntheticRepinned")
		pinnedWallet := payoutReimbursementTestWallet(t, ctx, networkId, "SOL", pinnedAddress, true)
		otherWallet := payoutReimbursementTestWallet(t, ctx, networkId, "SOL", payoutReimbursementTestAddress("SyntheticElsewhere"), true)
		data := payoutReimbursementTestCsv(payoutReimbursementTestRow(networkId, pinnedWallet, "0.577215"))
		digest := payoutReimbursementTestDigest(t, clientSession, data)

		plan := &model.ManualReimbursementPlan{PaymentPlanId: payoutReimbursementPlanId(digest), Digest: digest, Reason: payoutReimbursementTestReason}
		dry, err := payoutReimbursementTestRun(clientSession, data, payoutReimbursementTestReason, false, "")
		if err != nil {
			t.Fatal(err)
		}
		row := dry.Rows[0]
		plan.Markers = append(plan.Markers, &model.ManualReimbursementMarker{
			Version: 1, Digest: digest, Reason: payoutReimbursementTestReason, PaymentPlanId: plan.PaymentPlanId, PaymentId: row.PaymentId,
			NetworkId: networkId, WalletId: pinnedWallet, WalletAddress: pinnedAddress, Blockchain: "SOL",
			AmountNanoCents: row.AmountNanoCents, FeeNanoCents: 10_000_000, PayoutNanoCents: row.PayoutNanoCents,
		})
		if created, err := model.CreateManualReimbursementPlan(ctx, plan, payoutReimbursementTestNow); err != nil || !created {
			t.Fatal(created, err)
		}
		// an old writer re-points it
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE account_payment SET wallet_id = $2 WHERE payment_id = $1`, row.PaymentId, otherWallet))
		})
		if _, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: row.PaymentId}, clientSession); !errors.Is(err, model.ErrManualReimbursementChanged) {
			t.Fatalf("a re-pointed reimbursement was admitted: %v", err)
		}
		if len(circle.sends) != 0 {
			t.Fatal("a re-pointed reimbursement was sent")
		}

		report, err := payoutReimbursementTestRun(clientSession, data, payoutReimbursementTestReason, true, digest)
		if err != nil || report.Created {
			t.Fatalf("rerun %+v %v", report, err)
		}
		if len(circle.sends) != 1 || circle.sends[0].address != pinnedAddress {
			t.Fatalf("the rerun sent %+v", circle.sends)
		}
	})
}

// sn.yml declares the earning boundary in production. A reimbursement has no
// sweeps, so the earning census would refuse it; its marker admits it, while
// an unmarked payment without sweeps stays refused.
func TestPayoutReimbursementIsAdmittedUnderTheEarningPolicy(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		controllerPayoutSchedule(t, time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC))
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		defer clientSession.Cancel()
		circle := newPayoutReimbursementTestCircle(t)

		networkId := payoutReimbursementTestNetwork(t, ctx, "synthetic-policy")
		address := payoutReimbursementTestAddress("SyntheticPolicy")
		walletId := payoutReimbursementTestWallet(t, ctx, networkId, "SOL", address, true)
		data := payoutReimbursementTestCsv(payoutReimbursementTestRow(networkId, walletId, "1.414213"))
		dry, err := payoutReimbursementTestRun(clientSession, data, payoutReimbursementTestReason, false, "")
		if err != nil || dry.EarningPolicy == "none" || strings.HasPrefix(dry.EarningPolicy, "unavailable") {
			t.Fatalf("dry run under the policy %+v %v", dry, err)
		}
		report, err := payoutReimbursementTestRun(clientSession, data, payoutReimbursementTestReason, true, dry.Digest)
		if err != nil {
			t.Fatal(err)
		}
		if len(circle.sends) != 1 || circle.sends[0].address != address || fmt.Sprintf("%f", circle.sends[0].amount) != "1.414213" {
			t.Fatalf("Circle was asked for %+v", circle.sends)
		}

		ordinary := payoutReimbursementTestOrdinaryPayment(t, ctx, networkId, walletId, payoutReimbursementTestNow)
		if err := model.RequireProviderUsdcPayment(ctx, ordinary); !errors.Is(err, model.ErrProviderUsdcAttributionUnresolved) {
			t.Fatalf("an unmarked payment without sweeps was admitted: %v", err)
		}

		circle.state, circle.txHash = "COMPLETE", "synthetic-policy-hash"
		result, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: report.Rows[0].PaymentId}, clientSession)
		if err != nil || !result.Complete {
			t.Fatalf("completion %+v %v", result, err)
		}
		if payment := payoutReimbursementTestPayment(t, ctx, report.Rows[0].PaymentId); payment.AttributionReviewRequired {
			t.Fatal("the completed reimbursement was held for attribution review")
		}
	})
}
