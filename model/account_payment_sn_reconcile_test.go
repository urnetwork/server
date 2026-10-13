package model

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Synthetic legacy rows are written directly, as the historical planner and
// sender left them. No processor is involved in this file.

func snReconcileTestExec(t testing.TB, ctx context.Context, sql string, args ...any) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, sql, args...))
	})
}

// A planned legacy payment that owns the sweeps of `contracts`, with the exact
// amounts of its census.
func snReconcileTestPayment(t testing.TB, ctx context.Context, networkId server.Id, walletId *server.Id, created time.Time, contracts ...server.Id) server.Id {
	t.Helper()
	return snReconcileTestPaymentPlus(t, ctx, 0, networkId, walletId, created, contracts...)
}

// The same payment with `plus` more payout than its census explains. The
// amount guard refuses that change on update, so it is written on insert.
func snReconcileTestPaymentPlus(t testing.TB, ctx context.Context, plus NanoCents, networkId server.Id, walletId *server.Id, created time.Time, contracts ...server.Id) server.Id {
	t.Helper()
	paymentId := server.NewId()
	snReconcileTestExec(t, ctx, `INSERT INTO account_payment(payment_id,payment_plan_id,network_id,wallet_id,payout_byte_count,payout_nano_cents,min_sweep_time,create_time)
		SELECT $1,$2,$3,$4,COALESCE(SUM(payout_byte_count),0),COALESCE(SUM(payout_net_revenue_nano_cents),0)+$7,$5,$5
		FROM transfer_escrow_sweep WHERE contract_id=ANY($6)`, paymentId, server.NewId(), networkId, walletId, created, contracts, plus)
	snReconcileTestExec(t, ctx, `UPDATE transfer_escrow_sweep SET payment_id=$1 WHERE contract_id=ANY($2)`, paymentId, contracts)
	return paymentId
}

// A bare row for the reference reads, which never read its earnings.
func snReconcileTestBareRow(t testing.TB, ctx context.Context, record *string, paymentTime *time.Time, receipt *string) server.Id {
	t.Helper()
	paymentId := server.NewId()
	snReconcileTestExec(t, ctx, `INSERT INTO account_payment(payment_id,payment_plan_id,network_id,payout_byte_count,payout_nano_cents,min_sweep_time,payment_record,payment_time,payment_receipt)
		VALUES($1,$2,$3,0,0,$4,$5,$6,$7)`, paymentId, server.NewId(), server.NewId(), payoutTestCutoff, record, paymentTime, receipt)
	return paymentId
}

// Moves a correction the way the planner carries it: the origin is safely
// canceled, then its bonus row moves to `target`. The origin keeps its amounts.
func snReconcileTestMoveBonus(t testing.TB, ctx context.Context, origin server.Id, target server.Id, amount NanoCents) {
	t.Helper()
	snReconcileTestExec(t, ctx, `INSERT INTO account_payment_bonus(operation_id,origin_payment_id,payment_id,payment_plan_id,network_id,amount_nano_cents,reason,latest_contract_close_time,policy_sha256)
		SELECT $1,payment_id,payment_id,payment_plan_id,network_id,$2,'synthetic correction',$3,'' FROM account_payment WHERE payment_id=$4`,
		server.NewId(), amount, payoutTestCutoff.Add(-time.Hour), origin)
	snReconcileTestExec(t, ctx, `UPDATE account_payment SET payout_nano_cents=payout_nano_cents+$2,bonus_payout_nano_cents=bonus_payout_nano_cents+$2 WHERE payment_id=$1`, origin, amount)
	snReconcileTestExec(t, ctx, `UPDATE account_payment SET canceled=true,cancel_time=create_time WHERE payment_id=$1`, origin)
	snReconcileTestExec(t, ctx, `UPDATE account_payment_bonus SET payment_id=$2 WHERE origin_payment_id=$1`, origin, target)
	snReconcileTestExec(t, ctx, `UPDATE account_payment SET payout_nano_cents=payout_nano_cents+$2,bonus_payout_nano_cents=bonus_payout_nano_cents+$2 WHERE payment_id=$1`, target, amount)
}

func snReconcileTestStuck(t testing.TB, ctx context.Context) []*SnReconcileRow {
	t.Helper()
	rows, err := GetSnReconcileStuckRows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func snReconcileTestIds(rows []*SnReconcileRow) []server.Id {
	ids := []server.Id{}
	for _, row := range rows {
		ids = append(ids, row.PaymentId)
	}
	return ids
}

// Every column of the row with its sweep and audit counts, for exact
// before/after comparison.
func snReconcileTestFingerprint(t testing.TB, ctx context.Context, paymentId server.Id) (fingerprint string) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT row_to_json(p)::text||' sweeps='||(SELECT COUNT(*) FROM transfer_escrow_sweep s WHERE s.payment_id=p.payment_id)
			||' audits='||(SELECT COUNT(*) FROM audit_account_payment a WHERE a.payment_id=p.payment_id) FROM account_payment p WHERE p.payment_id=$1`, paymentId).Scan(&fingerprint))
	})
	return
}

// RC-1
func TestSnReconcileStuckRowsCanceledWithEvidence(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		f := newPayoutTransitionCohort(t, ctx)
		wallet := GetPayoutWalletId(ctx, f.network)
		closed := payoutTestCutoff.Add(-time.Hour)
		contract := func() server.Id {
			return f.insert(t, ctx, closed.Add(-time.Hour), &closed, closed, 10, UsdToNanoCents(1))
		}
		created := payoutTestCutoff.Add(-60 * 24 * time.Hour)
		keyOnly := snReconcileTestPayment(t, ctx, f.network, wallet, created, contract())
		recordOnly := snReconcileTestPayment(t, ctx, f.network, wallet, created.Add(time.Hour), contract())
		hashOnly := snReconcileTestPayment(t, ctx, f.network, nil, created.Add(2*time.Hour), contract(), contract())
		safelyCanceled := snReconcileTestPayment(t, ctx, f.network, wallet, created.Add(3*time.Hour), contract())
		completed := snReconcileTestPayment(t, ctx, f.network, wallet, created.Add(4*time.Hour), contract())
		snReconcileTestPayment(t, ctx, f.network, wallet, created.Add(5*time.Hour), contract())
		otherNetwork := server.NewId()
		withoutPayoutWallet := snReconcileTestPayment(t, ctx, otherNetwork, wallet, created.Add(6*time.Hour))
		key := server.NewId()
		cancelTime := created.Add(30 * 24 * time.Hour)
		snReconcileTestExec(t, ctx, `UPDATE account_payment SET canceled=true,cancel_time=$2,circle_idempotency_key=$3 WHERE payment_id=$1`, keyOnly, cancelTime, key)
		snReconcileTestExec(t, ctx, `UPDATE account_payment SET canceled=true,cancel_time=$2,payment_record='record-only',token_type='USDC',token_amount=0.99,payment_time=$2,payment_receipt='{}' WHERE payment_id=$1`, recordOnly, cancelTime)
		snReconcileTestExec(t, ctx, `UPDATE account_payment SET canceled=true,tx_hash='0xhash-only' WHERE payment_id=$1`, hashOnly)
		snReconcileTestExec(t, ctx, `UPDATE account_payment SET canceled=true,cancel_time=$2 WHERE payment_id=$1`, safelyCanceled, cancelTime)
		snReconcileTestExec(t, ctx, `UPDATE account_payment SET completed=true,complete_time=$2,payment_record='completed' WHERE payment_id=$1`, completed, cancelTime)
		snReconcileTestExec(t, ctx, `UPDATE account_payment SET canceled=true,payment_record='other-network' WHERE payment_id=$1`, withoutPayoutWallet)

		rows := snReconcileTestStuck(t, ctx)
		if got, want := snReconcileTestIds(rows), []server.Id{keyOnly, recordOnly, hashOnly, withoutPayoutWallet}; !slices.Equal(got, want) {
			t.Fatalf("stuck rows = %v, want %v", got, want)
		}
		for _, row := range rows {
			if row.Kind != SnReconcileCanceledWithEvidence || !row.Canceled {
				t.Fatal("canceled row not classified as canceled_with_evidence", row.PaymentId, row.Kind)
			}
		}
		row := rows[0]
		if *row.CircleIdempotencyKey != key || !row.CancelTime.Equal(cancelTime) || row.PaymentRecord != nil || row.TxHash != nil ||
			row.NetworkId != f.network || *row.WalletId != *wallet || row.Payout != UsdToNanoCents(1) || row.PayoutByteCount != 10 ||
			!row.CreateTime.Equal(created) || row.SweepCount != 1 || row.AttributionReviewRequired || !row.EarliestClose.Equal(closed) {
			t.Fatalf("key-only snapshot = %+v", row)
		}
		if *row.PinnedWalletAddress != "0x0000000000000000000000000000000000000012" || *row.PayoutWalletId != *wallet ||
			*row.PayoutWalletAddress != "0x0000000000000000000000000000000000000012" || *row.PayoutWalletBlockchain != "MATIC" || !row.PayoutWalletActive {
			t.Fatalf("key-only wallets = %+v", row)
		}
		row = rows[1]
		if *row.PaymentRecord != "record-only" || *row.TokenType != "USDC" || *row.TokenAmount != 0.99 || !row.PaymentTime.Equal(cancelTime) || *row.PaymentReceipt != "{}" {
			t.Fatalf("record-only snapshot = %+v", row)
		}
		row = rows[2]
		if *row.TxHash != "0xhash-only" || row.CancelTime != nil || row.WalletId != nil || row.PinnedWalletAddress != nil || row.SweepCount != 2 || row.Payout != UsdToNanoCents(2) {
			t.Fatalf("hash-only snapshot = %+v", row)
		}
		row = rows[3]
		if row.PayoutWalletId != nil || row.PayoutWalletAddress != nil || row.PayoutWalletBlockchain != nil || row.PayoutWalletActive || row.SweepCount != 0 || row.NetworkId != otherNetwork ||
			row.EarliestClose != nil {
			t.Fatalf("payout wallet of another network was joined: %+v", row)
		}
	})
}

// RC-2
func TestSnReconcileStuckRowsUnretainedKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		f := newPayoutTransitionCohort(t, ctx)
		wallet := GetPayoutWalletId(ctx, f.network)
		created := payoutTestCutoff.Add(-10 * 24 * time.Hour)
		payment := func(offset time.Duration) (server.Id, server.Id) {
			key := server.NewId()
			paymentId := snReconcileTestPayment(t, ctx, f.network, wallet, created.Add(offset))
			snReconcileTestExec(t, ctx, `UPDATE account_payment SET circle_idempotency_key=$2 WHERE payment_id=$1`, paymentId, key)
			return paymentId, key
		}
		unretained, _ := payment(0)
		journaled, journaledKey := payment(time.Hour)
		invalid, invalidKey := payment(2 * time.Hour)
		foreign, foreignKey := payment(3 * time.Hour)
		recorded, _ := payment(4 * time.Hour)
		hashed, _ := payment(5 * time.Hour)
		snReconcileTestExec(t, ctx, `INSERT INTO audit_account_payment(event_id,payment_id,event_type,event_details) VALUES($1,$2,'circle_attempt_request','{"basis":{}}')`, journaledKey, journaled)
		// A journaled request that fails the sender's validation was still journaled.
		snReconcileTestExec(t, ctx, `INSERT INTO audit_account_payment(event_id,payment_id,event_type,event_details) VALUES($1,$2,'circle_attempt_request','{')`, invalidKey, invalid)
		// Any row under the key counts, whatever its payment or type.
		snReconcileTestExec(t, ctx, `INSERT INTO audit_account_payment(event_id,payment_id,event_type,event_details) VALUES($1,$2,'circle_payout_failed',NULL)`, foreignKey, server.NewId())
		snReconcileTestExec(t, ctx, `UPDATE account_payment SET payment_record='in-flight' WHERE payment_id=$1`, recorded)
		snReconcileTestExec(t, ctx, `UPDATE account_payment SET tx_hash='0xon-chain' WHERE payment_id=$1`, hashed)

		rows := snReconcileTestStuck(t, ctx)
		if got := snReconcileTestIds(rows); !slices.Equal(got, []server.Id{unretained}) {
			t.Fatalf("stuck rows = %v, want only the unretained key %s (journaled %s, invalid %s, foreign %s)", got, unretained, journaled, invalid, foreign)
		}
		if rows[0].Kind != SnReconcileUnretainedKey || rows[0].Canceled || rows[0].CircleIdempotencyKey == nil {
			t.Fatalf("unretained row = %+v", rows[0])
		}
	})
}

// RC-1: a stuck row without a network cannot be attributed and refuses.
func TestSnReconcileStuckRowWithoutNetworkRefuses(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		paymentId := snReconcileTestBareRow(t, ctx, nil, nil, nil)
		snReconcileTestExec(t, ctx, `UPDATE account_payment SET network_id=NULL,canceled=true,payment_record='no-network' WHERE payment_id=$1`, paymentId)
		rows, err := GetSnReconcileStuckRows(ctx)
		if err == nil || rows != nil || !strings.Contains(err.Error(), paymentId.String()) {
			t.Fatal("a stuck row without a network was not refused", rows, err)
		}
	})
}

// RC-5
func TestSnReconcileReceiptWalletIds(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		from := payoutTestCutoff
		at := func(offset time.Duration) *time.Time {
			value := from.Add(offset)
			return &value
		}
		text := func(value string) *string {
			return &value
		}
		snReconcileTestBareRow(t, ctx, text("a"), at(time.Hour), text(`{"data":{"transaction":{"id":"a","walletId":"wallet-a","state":"COMPLETE"}}}`))
		snReconcileTestBareRow(t, ctx, text("b"), at(0), text(`truncated "walletId" : "wallet-b", "amounts":[`))
		snReconcileTestBareRow(t, ctx, text("c"), at(-time.Microsecond), text(`{"walletId":"wallet-c"}`))
		snReconcileTestBareRow(t, ctx, text("d"), nil, text(`{"walletId":"wallet-d"}`))
		snReconcileTestBareRow(t, ctx, text("e"), at(2*time.Hour), text(`{"walletId":"wallet-a"},{"walletId":"wallet-e"}`))
		snReconcileTestBareRow(t, ctx, text("f"), at(3*time.Hour), nil)
		if got, want := GetSnReconcileReceiptWalletIds(ctx, from), []string{"wallet-a", "wallet-b", "wallet-e"}; !slices.Equal(got, want) {
			t.Fatalf("receipt wallets = %v, want %v", got, want)
		}
	})
}

// RC-6
func TestSnReconcileRecordsPaidBetween(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		start, end := payoutTestCutoff, payoutTestCutoff.Add(time.Hour)
		for record, paymentTime := range map[string]time.Time{
			"before": start.Add(-time.Microsecond), "start": start, "middle": start.Add(time.Minute), "end": end, "after": end.Add(time.Microsecond),
		} {
			snReconcileTestBareRow(t, ctx, &record, &paymentTime, nil)
		}
		duplicate := "middle"
		middle := start.Add(2 * time.Minute)
		snReconcileTestBareRow(t, ctx, &duplicate, &middle, nil)
		unpaid := "unpaid"
		snReconcileTestBareRow(t, ctx, &unpaid, nil, nil)
		snReconcileTestBareRow(t, ctx, nil, &middle, nil)
		if got, want := GetSnReconcileRecordsPaidBetween(ctx, start, end), []string{"end", "middle", "start"}; !slices.Equal(got, want) {
			t.Fatalf("records = %v, want %v", got, want)
		}
	})
}

// RC-7
func TestSnReconcileReferencedIds(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		text := func(value string) *string {
			return &value
		}
		snReconcileTestBareRow(t, ctx, text("by-record"), nil, nil)
		snReconcileTestBareRow(t, ctx, nil, nil, text(`{"data":{"transaction":{"id" : "by-receipt"`))
		snReconcileTestBareRow(t, ctx, nil, nil, text(`{"walletId":"wallet-key-only","tokenId":"token-key-only"}`))
		snReconcileTestBareRow(t, ctx, text("not-listed"), nil, text(`{"id":"also-not-listed"}`))
		got := GetSnReconcileReferencedIds(ctx, []string{"by-record", "by-receipt", "wallet-key-only", "token-key-only", "unreferenced"})
		if len(got) != 2 || !got["by-record"] || !got["by-receipt"] {
			t.Fatalf("referenced = %v", got)
		}
	})
}

// RC-7, A4
func TestSnReconcileAddressNetworks(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networks := []server.Id{server.NewId(), server.NewId(), server.NewId()}
		slices.SortFunc(networks, func(a server.Id, b server.Id) int {
			return a.Cmp(b)
		})
		for i, wallet := range []struct {
			address string
			active  bool
		}{{"0xAbC", true}, {"0xabc", false}, {"0xdef", true}} {
			snReconcileTestExec(t, ctx, `INSERT INTO account_wallet(wallet_id,network_id,wallet_type,blockchain,wallet_address,active,default_token_type)
				VALUES($1,$2,'external','MATIC',$3,$4,'USDC')`, server.NewId(), networks[i], wallet.address, wallet.active)
		}
		snReconcileTestExec(t, ctx, `INSERT INTO account_wallet(wallet_id,network_id,wallet_type,blockchain,wallet_address,active,default_token_type)
			VALUES($1,$2,'external','MATIC',E'\t0xABC \n',true,'USDC')`, server.NewId(), networks[0])
		got := GetSnReconcileAddressNetworks(ctx, []string{"0xabc", "0x999"})
		if len(got) != 1 || !slices.Equal(got["0xabc"], networks[:2]) {
			t.Fatalf("address networks = %v, want 0xabc -> %v", got, networks[:2])
		}
		// A4: Go and SQL strip the same characters.
		if address := SnReconcileAddress("\t\n\v\f\r 0xAbC \r\n"); address != "0xabc" || SnReconcileAddress("0x\u00a0") != "0x\u00a0" {
			t.Fatal("address form", address)
		}
	})
}

// RC-8: the same census with and without a policy; only the policy bounds the
// contract close time.
func TestSnReconcileCensusExact(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		f := newPayoutTransitionCohort(t, ctx)
		wallet := GetPayoutWalletId(ctx, f.network)
		before := payoutTestCutoff.Add(-time.Hour)
		after := payoutTestCutoff.Add(time.Hour)
		created := payoutTestCutoff.Add(2 * time.Hour)
		exact := snReconcileTestPayment(t, ctx, f.network, wallet, created, f.insert(t, ctx, before.Add(-time.Hour), &before, before, 10, UsdToNanoCents(1)))
		mismatched := snReconcileTestPaymentPlus(t, ctx, 1, f.network, wallet, created, f.insert(t, ctx, before.Add(-time.Hour), &before, before, 10, UsdToNanoCents(1)))
		lateClose := snReconcileTestPayment(t, ctx, f.network, wallet, created, f.insert(t, ctx, before, &after, after, 10, UsdToNanoCents(1)))
		for _, paymentId := range []server.Id{exact, mismatched, lateClose} {
			snReconcileTestExec(t, ctx, `UPDATE account_payment SET canceled=true,circle_idempotency_key=$2 WHERE payment_id=$1`, paymentId, server.NewId())
		}
		check := func(policy *server.ProviderPayoutTransition, want map[server.Id]bool) {
			for paymentId, exactWant := range want {
				if got := SnReconcileCensusExact(ctx, paymentId, policy); got != exactWant {
					t.Fatalf("census of %s with policy %v = %v, want %v", paymentId, policy != nil, got, exactWant)
				}
			}
		}
		check(nil, map[server.Id]bool{exact: true, mismatched: false, lateClose: true})
		usePayoutTransition(t)
		policy, err := server.LoadProviderPayoutEarningPolicy(ctx)
		if err != nil || policy == nil {
			t.Fatal("policy not loaded", err)
		}
		check(policy, map[server.Id]bool{exact: true, mismatched: false, lateClose: false})
	})
}

// A canceled RC-1 row with a failed record, ready for the revive.
func snReconcileTestRevivable(t testing.TB, ctx context.Context) (*payoutTransitionCohort, *SnReconcileRow) {
	t.Helper()
	f := newPayoutTransitionCohort(t, ctx)
	wallet := GetPayoutWalletId(ctx, f.network)
	closed := payoutTestCutoff.Add(-time.Hour)
	created := payoutTestCutoff.Add(-40 * 24 * time.Hour)
	paymentId := snReconcileTestPayment(t, ctx, f.network, wallet, created, f.insert(t, ctx, closed.Add(-time.Hour), &closed, closed, 10, UsdToNanoCents(3)))
	snReconcileTestExec(t, ctx, `UPDATE account_payment SET canceled=true,cancel_time=$2,circle_idempotency_key=$3,payment_record='failed-record',
		token_type='USDC',token_amount=2.99,payment_time=$4,payment_receipt='{"state":"FAILED"}' WHERE payment_id=$1`,
		paymentId, created.Add(30*24*time.Hour), server.NewId(), created.Add(time.Hour))
	rows := snReconcileTestStuck(t, ctx)
	if len(rows) != 1 || rows[0].PaymentId != paymentId {
		t.Fatal("revivable fixture is not the only stuck row")
	}
	return f, rows[0]
}

// RC-14
func TestSnReconcileReviveClearsMarkersInOneUpdate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for _, withPolicy := range []bool{false, true} {
			var policy *server.ProviderPayoutTransition
			if withPolicy {
				usePayoutTransition(t)
				var err error
				policy, err = server.LoadProviderPayoutEarningPolicy(ctx)
				if err != nil || policy == nil {
					t.Fatal("policy not loaded", err)
				}
			}
			_, row := snReconcileTestRevivable(t, ctx)
			before, err := GetPayment(ctx, row.PaymentId)
			if err != nil {
				t.Fatal(err)
			}
			runId := server.NewId()
			evidence := &SnReconcileEvidence{TransactionId: "failed-record", State: "FAILED"}
			scheduled := 0
			revived := ReviveSnReconcileRow(ctx, row, policy, runId, evidence, func(tx server.PgTx) {
				scheduled++
				var canceled bool
				server.Raise(tx.QueryRow(ctx, `SELECT canceled FROM account_payment WHERE payment_id=$1`, row.PaymentId).Scan(&canceled))
				if canceled {
					t.Fatal("schedule ran before the revive in its transaction")
				}
			})
			if !revived || scheduled != 1 {
				t.Fatal("exact row was not revived and scheduled once", revived, scheduled)
			}
			after, err := GetPayment(ctx, row.PaymentId)
			if err != nil {
				t.Fatal(err)
			}
			if after.Canceled || after.Completed || after.CircleIdempotencyKey != nil || after.PaymentRecord != nil || after.TokenType != nil || after.TokenAmount != nil ||
				after.PaymentTime != nil || after.PaymentReceipt != nil || after.TxHash != nil {
				t.Fatalf("revive left a marker: %+v", after)
			}
			if !after.CancelTime.Equal(*before.CancelTime) || after.Payout != before.Payout || after.PayoutByteCount != before.PayoutByteCount ||
				after.PaymentPlanId != before.PaymentPlanId || *after.WalletId != *before.WalletId || after.NetworkId != before.NetworkId ||
				after.SubsidyPayout != before.SubsidyPayout || after.ReliabilitySubsidy != before.ReliabilitySubsidy || after.BonusPayout != before.BonusPayout ||
				!after.CreateTime.Equal(before.CreateTime) || !after.MinSweepTime.Equal(before.MinSweepTime) {
				t.Fatalf("revive changed the obligation: before %+v after %+v", before, after)
			}
			var details string
			var audits int
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT (SELECT COUNT(*) FROM audit_account_payment WHERE payment_id=$1),
					(SELECT event_details FROM audit_account_payment WHERE payment_id=$1 AND event_type='sn_reconcile_revived')`, row.PaymentId).Scan(&audits, &details))
			})
			var revival map[string]any
			if err := json.Unmarshal([]byte(details), &revival); err != nil || audits != 1 {
				t.Fatal("revival audit missing", err, audits)
			}
			policyHash := ""
			if policy != nil {
				policyHash = policy.ConfigSha256
			}
			evidenceJson := revival["circle_evidence"].(map[string]any)
			if revival["run_id"] != runId.String() || revival["policy_sha256"] != policyHash || evidenceJson["transaction_id"] != "failed-record" || evidenceJson["state"] != "FAILED" ||
				revival["canceled"] != true || revival["circle_idempotency_key"] != row.CircleIdempotencyKey.String() || revival["payment_record"] != "failed-record" ||
				revival["tx_hash"] != nil || revival["token_type"] != "USDC" || revival["token_amount"] != 2.99 || revival["payment_receipt"] != `{"state":"FAILED"}` ||
				revival["payment_time"] != row.PaymentTime.Format(time.RFC3339Nano) || revival["cancel_time"] != row.CancelTime.Format(time.RFC3339Nano) {
				t.Fatalf("revival audit = %s", details)
			}
			// The revived row is not stuck, and a later plan never selects its sweeps.
			if rows := snReconcileTestStuck(t, ctx); len(rows) != 0 {
				t.Fatal("revived row is still stuck")
			}
			plan, err := CreatePaymentPlan(ctx, payoutTransitionRevenueConfig(), false, 0)
			if err != nil {
				t.Fatal(err)
			}
			var sweeps int
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM transfer_escrow_sweep WHERE payment_id=$1`, row.PaymentId).Scan(&sweeps))
			})
			if _, ok := plan.NetworkPayments[row.NetworkId]; ok || sweeps != 1 {
				t.Fatal("a later plan moved the revived row's sweeps")
			}
		}
	})
}

// RC-15: any snapshot difference, either way, leaves the row untouched.
func TestSnReconcileReviveLeavesChangedRow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, row := snReconcileTestRevivable(t, ctx)
		before := snReconcileTestFingerprint(t, ctx, row.PaymentId)
		text := func(value string) *string {
			return &value
		}
		other := server.NewId()
		later := row.CreateTime.Add(time.Microsecond)
		amount := 1.5
		for name, change := range map[string]func(*SnReconcileRow){
			"canceled":     func(r *SnReconcileRow) { r.Canceled = false },
			"cancel_time":  func(r *SnReconcileRow) { r.CancelTime = nil },
			"key":          func(r *SnReconcileRow) { r.CircleIdempotencyKey = &other },
			"record":       func(r *SnReconcileRow) { r.PaymentRecord = text("another-record") },
			"tx_hash":      func(r *SnReconcileRow) { r.TxHash = text("0xhash") },
			"wallet":       func(r *SnReconcileRow) { r.WalletId = nil },
			"network":      func(r *SnReconcileRow) { r.NetworkId = other },
			"byte_count":   func(r *SnReconcileRow) { r.PayoutByteCount++ },
			"payout":       func(r *SnReconcileRow) { r.Payout++ },
			"subsidy":      func(r *SnReconcileRow) { r.SubsidyPayout++ },
			"reliability":  func(r *SnReconcileRow) { r.ReliabilitySubsidy++ },
			"bonus":        func(r *SnReconcileRow) { r.BonusPayout++ },
			"review":       func(r *SnReconcileRow) { r.AttributionReviewRequired = true },
			"plan":         func(r *SnReconcileRow) { r.PaymentPlanId = other },
			"create_time":  func(r *SnReconcileRow) { r.CreateTime = later },
			"token_type":   func(r *SnReconcileRow) { r.TokenType = nil },
			"token_amount": func(r *SnReconcileRow) { r.TokenAmount = &amount },
			"payment_time": func(r *SnReconcileRow) { r.PaymentTime = &later },
			"receipt":      func(r *SnReconcileRow) { r.PaymentReceipt = text("{}") },
		} {
			changed := *row
			change(&changed)
			scheduled := 0
			if ReviveSnReconcileRow(ctx, &changed, nil, server.NewId(), nil, func(server.PgTx) { scheduled++ }) || scheduled != 0 {
				t.Fatal("a changed snapshot revived the row", name)
			}
			if after := snReconcileTestFingerprint(t, ctx, row.PaymentId); after != before {
				t.Fatalf("%s: a skipped revive wrote: %s", name, after)
			}
		}
		// The database changed the row instead: the payment completed meanwhile.
		snReconcileTestExec(t, ctx, `UPDATE account_payment SET completed=true WHERE payment_id=$1`, row.PaymentId)
		completed := snReconcileTestFingerprint(t, ctx, row.PaymentId)
		if ReviveSnReconcileRow(ctx, row, nil, server.NewId(), nil, func(server.PgTx) { t.Fatal("completed row scheduled") }) {
			t.Fatal("completed row revived")
		}
		if after := snReconcileTestFingerprint(t, ctx, row.PaymentId); after != completed {
			t.Fatalf("completed row changed: %s", after)
		}
	})
}

// RC-15: a census that is no longer exact inside the transaction writes nothing.
func TestSnReconcileReviveCensusFailureWritesNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		f, row := snReconcileTestRevivable(t, ctx)
		closed := payoutTestCutoff.Add(-time.Hour)
		snReconcileTestExec(t, ctx, `UPDATE transfer_escrow_sweep SET payment_id=$1 WHERE contract_id=$2`, row.PaymentId, f.insert(t, ctx, closed.Add(-time.Hour), &closed, closed, 1, 1))
		before := snReconcileTestFingerprint(t, ctx, row.PaymentId)
		if ReviveSnReconcileRow(ctx, row, nil, server.NewId(), nil, func(server.PgTx) { t.Fatal("inexact row scheduled") }) {
			t.Fatal("inexact row revived")
		}
		if after := snReconcileTestFingerprint(t, ctx, row.PaymentId); after != before {
			t.Fatalf("inexact revive wrote: %s", after)
		}
	})
}

// RC-14: an UPDATE that does not land rolls back the audit and the schedule.
func TestSnReconcileReviveLostUpdateRollsBack(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, row := snReconcileTestRevivable(t, ctx)
		snReconcileTestExec(t, ctx, `CREATE FUNCTION synthetic_skip_revive() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NULL; END $$;
			CREATE TRIGGER synthetic_skip_revive BEFORE UPDATE ON account_payment FOR EACH ROW EXECUTE FUNCTION synthetic_skip_revive()`)
		before := snReconcileTestFingerprint(t, ctx, row.PaymentId)
		recovered := func() (value any) {
			defer func() {
				value = recover()
			}()
			ReviveSnReconcileRow(ctx, row, nil, server.NewId(), nil, func(server.PgTx) { t.Fatal("lost revive scheduled") })
			return nil
		}()
		err, ok := recovered.(error)
		if !ok || !strings.Contains(err.Error(), "did not update its locked row") {
			t.Fatal("lost update did not stop the revive", recovered)
		}
		if after := snReconcileTestFingerprint(t, ctx, row.PaymentId); after != before {
			t.Fatalf("lost update left writes: %s", after)
		}
		snReconcileTestExec(t, ctx, `DROP TRIGGER synthetic_skip_revive ON account_payment; DROP FUNCTION synthetic_skip_revive()`)
	})
}

// The subsidy epoch of the payment's plan starts at `start`.
func snReconcileTestEpoch(t testing.TB, ctx context.Context, paymentId server.Id, start time.Time) {
	t.Helper()
	snReconcileTestExec(t, ctx, `INSERT INTO subsidy_payment(payment_plan_id,start_time,end_time,active_user_count,paid_user_count,
		net_payout_byte_count_paid,net_payout_byte_count_unpaid,net_revenue_nano_cents,net_payout_nano_cents)
		SELECT payment_plan_id,$2::timestamp,$2::timestamp+INTERVAL '1 microsecond',0,0,0,0,0,0 FROM account_payment WHERE payment_id=$1`, paymentId, start)
}

// A correction the row itself earned, not moved from another row.
func snReconcileTestOwnBonus(t testing.TB, ctx context.Context, paymentId server.Id, amount NanoCents) {
	t.Helper()
	snReconcileTestExec(t, ctx, `INSERT INTO account_payment_bonus(operation_id,origin_payment_id,payment_id,payment_plan_id,network_id,amount_nano_cents,reason,latest_contract_close_time,policy_sha256)
		SELECT $1,payment_id,payment_id,payment_plan_id,network_id,$2,'own correction',$3,'' FROM account_payment WHERE payment_id=$4`,
		server.NewId(), amount, payoutTestCutoff.Add(-time.Hour), paymentId)
	snReconcileTestExec(t, ctx, `UPDATE account_payment SET payout_nano_cents=payout_nano_cents+$2,bonus_payout_nano_cents=bonus_payout_nano_cents+$2 WHERE payment_id=$1`, paymentId, amount)
}

func snReconcileTestStuckById(t testing.TB, ctx context.Context) map[server.Id]*SnReconcileRow {
	t.Helper()
	rows := map[server.Id]*SnReconcileRow{}
	for _, row := range snReconcileTestStuck(t, ctx) {
		rows[row.PaymentId] = row
	}
	return rows
}

// A revenue-only planner configuration whose subsidy epochs need `minimum`.
func snReconcileTestPlanConfig(minimum time.Duration) *SubsidyConfig {
	config := payoutTransitionRevenueConfig()
	config.Days = 30
	config.MinDaysFraction = minimum.Hours() / (30 * 24)
	return config
}

// A2: early sweeps and bonuses of stuck rows, RC-2 included.
func TestSnReconcileStuckRowsEarlyEarnings(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		closed := payoutTestCutoff.Add(-10 * 24 * time.Hour)
		created := payoutTestCutoff.Add(-5 * 24 * time.Hour)
		f := newPayoutTransitionCohort(t, ctx)
		wallet := GetPayoutWalletId(ctx, f.network)
		row := func(cohort *payoutTransitionCohort, offset time.Duration, epoch *time.Time) server.Id {
			paymentId := snReconcileTestPayment(t, ctx, cohort.network, wallet, created.Add(offset), cohort.insert(t, ctx, closed.Add(-time.Hour), &closed, closed, 10, UsdToNanoCents(1)))
			if epoch != nil {
				snReconcileTestEpoch(t, ctx, paymentId, *epoch)
			}
			return paymentId
		}
		at := func(value time.Time) *time.Time {
			return &value
		}
		cancelWithKey := func(paymentId server.Id) {
			snReconcileTestExec(t, ctx, `UPDATE account_payment SET canceled=true,circle_idempotency_key=$2 WHERE payment_id=$1`, paymentId, server.NewId())
		}
		explained := row(f, 0, at(closed.Add(-time.Microsecond)))
		atStart := row(f, time.Hour, at(closed))
		ownBonus := row(f, 2*time.Hour, at(closed.Add(-time.Hour)))
		snReconcileTestOwnBonus(t, ctx, ownBonus, UsdToNanoCents(1))
		movedBonus := row(f, 3*time.Hour, at(closed.Add(-time.Hour)))
		origin := snReconcileTestPayment(t, ctx, f.network, wallet, created.Add(-24*time.Hour))
		snReconcileTestEpoch(t, ctx, origin, closed.Add(-time.Hour))
		snReconcileTestMoveBonus(t, ctx, origin, movedBonus, UsdToNanoCents(2))
		unretained := row(f, 4*time.Hour, at(closed))
		snReconcileTestExec(t, ctx, `UPDATE account_payment SET circle_idempotency_key=$2 WHERE payment_id=$1`, unretained, server.NewId())
		// Without its own epoch every sweep is early. Created last, it cannot
		// raise the bound of an earlier row.
		withoutEpoch := row(f, 10*time.Hour, nil)
		for _, paymentId := range []server.Id{explained, atStart, ownBonus, movedBonus, withoutEpoch} {
			cancelWithKey(paymentId)
		}
		// An earlier row of the network whose plan recorded no epoch raises the
		// bound to its creation; an earlier row with an epoch, or a later one
		// without, does not.
		g, h := newPayoutTransitionCohort(t, ctx), newPayoutTransitionCohort(t, ctx)
		snReconcileTestPayment(t, ctx, g.network, nil, closed.Add(time.Hour))
		raised := row(g, 0, at(closed.Add(-time.Hour)))
		earlier := snReconcileTestPayment(t, ctx, h.network, nil, closed.Add(time.Hour))
		snReconcileTestEpoch(t, ctx, earlier, closed.Add(-48*time.Hour))
		notRaised := row(h, 0, at(closed.Add(-time.Hour)))
		snReconcileTestPayment(t, ctx, h.network, nil, created.Add(time.Hour))
		cancelWithKey(raised)
		cancelWithKey(notRaised)

		rows := snReconcileTestStuckById(t, ctx)
		for paymentId, want := range map[server.Id]bool{explained: false, atStart: true, ownBonus: false, movedBonus: true, unretained: true,
			withoutEpoch: true, raised: true, notRaised: false} {
			if rows[paymentId] == nil || rows[paymentId].EarlyEarnings != want {
				t.Fatal("early earnings", paymentId, rows[paymentId], want)
			}
		}
		if !rows[explained].EarliestClose.Equal(closed) {
			t.Fatal("earliest close", rows[explained].EarliestClose)
		}
	})
}

// A5: the evidence that a stuck row's missing sweeps were re-planned.
func TestSnReconcileStuckRowsReplanEvidence(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		created := payoutTestCutoff.Add(-5 * 24 * time.Hour)
		day := 24 * time.Hour
		// Every row records an epoch so no bound is raised by a missing one.
		row := func(cohort *payoutTransitionCohort, at time.Time, epoch time.Time, closes ...time.Time) server.Id {
			contracts := []server.Id{}
			for _, closed := range closes {
				contracts = append(contracts, cohort.insert(t, ctx, closed.Add(-time.Hour), &closed, closed, 10, UsdToNanoCents(1)))
			}
			paymentId := snReconcileTestPayment(t, ctx, cohort.network, nil, at, contracts...)
			snReconcileTestEpoch(t, ctx, paymentId, epoch)
			return paymentId
		}
		reaped := func(cohort *payoutTransitionCohort) server.Id {
			paymentId := row(cohort, created, created.Add(-30*day))
			snReconcileTestExec(t, ctx, `UPDATE account_payment SET canceled=true,payment_record=$2 WHERE payment_id=$1`, paymentId, "record-"+paymentId.String())
			return paymentId
		}
		a, b, c, d := newPayoutTransitionCohort(t, ctx), newPayoutTransitionCohort(t, ctx), newPayoutTransitionCohort(t, ctx), newPayoutTransitionCohort(t, ctx)
		// A later row holds an early sweep that closed before this row was created.
		replanned := reaped(a)
		row(a, created.Add(day), created, created.Add(-day))
		// A row that holds sweeps itself never reads the evidence.
		withSweeps := row(a, created.Add(-time.Hour), created.Add(-3*day), created.Add(-2*day))
		snReconcileTestExec(t, ctx, `UPDATE account_payment SET canceled=true,circle_idempotency_key=$2 WHERE payment_id=$1`, withSweeps, server.NewId())
		// The later early sweep closed after this row was created.
		closedAfter := reaped(b)
		row(b, created.Add(2*day), created.Add(2*time.Hour), created.Add(time.Hour))
		// Only an earlier row holds the early sweep.
		laterThanHolder := reaped(c)
		row(c, created.Add(-day), created.Add(-day), created.Add(-2*day))
		// The later row's sweep is explained by its own epoch.
		explained := reaped(d)
		row(d, created.Add(day), created.Add(-2*day), created.Add(-day))

		rows := snReconcileTestStuckById(t, ctx)
		for paymentId, want := range map[server.Id]bool{replanned: true, withSweeps: false, closedAfter: false, laterThanHolder: false, explained: false} {
			if rows[paymentId] == nil || rows[paymentId].ReplanEvidence != want {
				t.Fatal("replan evidence", paymentId, rows[paymentId], want)
			}
		}
		if rows[replanned].EarliestClose != nil || rows[replanned].SweepCount != 0 {
			t.Fatal("reaped row", rows[replanned])
		}
	})
}

// A2 (§6): pending and safely canceled rows with early earnings.
func TestSnReconcileEarlyHolders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		f := newPayoutTransitionCohort(t, ctx)
		wallet := GetPayoutWalletId(ctx, f.network)
		closed := payoutTestCutoff.Add(-10 * 24 * time.Hour)
		late := payoutTestCutoff.Add(-6 * 24 * time.Hour)
		created := payoutTestCutoff.Add(-5 * 24 * time.Hour)
		contract := func(at time.Time, revenue NanoCents) server.Id {
			return f.insert(t, ctx, at.Add(-time.Hour), &at, at, 10, revenue)
		}
		holder := func(offset time.Duration, contracts ...server.Id) server.Id {
			paymentId := snReconcileTestPayment(t, ctx, f.network, wallet, created.Add(offset), contracts...)
			snReconcileTestEpoch(t, ctx, paymentId, closed)
			return paymentId
		}
		planned := holder(0, contract(closed, UsdToNanoCents(3)), contract(late, UsdToNanoCents(5)))
		reserved := holder(time.Hour, contract(closed, UsdToNanoCents(1)))
		key := server.NewId()
		snReconcileTestExec(t, ctx, `UPDATE account_payment SET circle_idempotency_key=$2 WHERE payment_id=$1`, reserved, key)
		snReconcileTestExec(t, ctx, `INSERT INTO audit_account_payment(event_id,payment_id,event_type,event_details) VALUES($1,$2,'circle_attempt_request','{}')`, key, reserved)
		submitted := holder(2*time.Hour, contract(closed, UsdToNanoCents(1)))
		snReconcileTestExec(t, ctx, `UPDATE account_payment SET circle_idempotency_key=$2,payment_record='submitted' WHERE payment_id=$1`, submitted, server.NewId())
		onChain := holder(3*time.Hour, contract(closed, UsdToNanoCents(1)))
		snReconcileTestExec(t, ctx, `UPDATE account_payment SET circle_idempotency_key=$2,payment_record='on-chain',tx_hash='0xon-chain' WHERE payment_id=$1`, onChain, server.NewId())
		safelyCanceled := holder(4*time.Hour, contract(closed, UsdToNanoCents(1)))
		snReconcileTestExec(t, ctx, `UPDATE account_payment SET canceled=true WHERE payment_id=$1`, safelyCanceled)
		bonusOnly := holder(5*time.Hour, contract(late, UsdToNanoCents(1)))
		origin := holder(-24 * time.Hour)
		snReconcileTestMoveBonus(t, ctx, origin, bonusOnly, UsdToNanoCents(2))
		// Completed and stuck rows, and rows without early earnings, are not listed.
		completed := holder(6*time.Hour, contract(closed, UsdToNanoCents(1)))
		snReconcileTestExec(t, ctx, `UPDATE account_payment SET completed=true WHERE payment_id=$1`, completed)
		canceledWithKey := holder(7*time.Hour, contract(closed, UsdToNanoCents(1)))
		snReconcileTestExec(t, ctx, `UPDATE account_payment SET canceled=true,circle_idempotency_key=$2 WHERE payment_id=$1`, canceledWithKey, server.NewId())
		unretained := holder(8*time.Hour, contract(closed, UsdToNanoCents(1)))
		snReconcileTestExec(t, ctx, `UPDATE account_payment SET circle_idempotency_key=$2 WHERE payment_id=$1`, unretained, server.NewId())
		holder(9*time.Hour, contract(late, UsdToNanoCents(1)))

		want := []SnReconcileHolder{
			{PaymentId: planned, State: "planned", Payout: UsdToNanoCents(8), PayoutByteCount: 20, EarlyRevenue: UsdToNanoCents(3), EarlyByteCount: 10},
			{PaymentId: reserved, State: "reserved", Payout: UsdToNanoCents(1), PayoutByteCount: 10, EarlyRevenue: UsdToNanoCents(1), EarlyByteCount: 10},
			{PaymentId: submitted, State: "submitted", Payout: UsdToNanoCents(1), PayoutByteCount: 10, EarlyRevenue: UsdToNanoCents(1), EarlyByteCount: 10},
			{PaymentId: onChain, State: "on_chain", Payout: UsdToNanoCents(1), PayoutByteCount: 10, EarlyRevenue: UsdToNanoCents(1), EarlyByteCount: 10},
			{PaymentId: safelyCanceled, State: "safely_canceled", Payout: UsdToNanoCents(1), PayoutByteCount: 10, EarlyRevenue: UsdToNanoCents(1), EarlyByteCount: 10},
			{PaymentId: bonusOnly, State: "planned", Payout: UsdToNanoCents(3), PayoutByteCount: 10, EarlyBonus: UsdToNanoCents(2)},
		}
		holders := GetSnReconcileEarlyHolders(ctx)
		if len(holders) != len(want) {
			t.Fatalf("holders = %d, want %d", len(holders), len(want))
		}
		for i, holder := range holders {
			plan, network := holder.PaymentPlanId, holder.NetworkId
			holder.PaymentPlanId, holder.NetworkId = server.Id{}, server.Id{}
			if *holder != want[i] || plan == (server.Id{}) || network != f.network {
				t.Fatalf("holder[%d] = %+v, want %+v", i, *holder, want[i])
			}
		}
	})
}

// A2: the epoch rule follows the real planner. Consecutive plans leave no
// early sweep, and every sweep of a canceled row is early in the row that
// re-plans it, including the one that closed exactly at the clamped start.
func TestSnReconcileEarlyEarningsFollowThePlanner(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		f := newPayoutTransitionCohort(t, ctx)
		at := func(hours int) time.Time {
			return payoutTestCutoff.Add(time.Duration(hours) * time.Hour)
		}
		contract := func(created int, closed int, revenue NanoCents) {
			closeTime := at(closed)
			f.insert(t, ctx, at(created), &closeTime, closeTime, 10, revenue)
		}
		plan := func() *PaymentPlan {
			plan, err := CreatePaymentPlan(ctx, snReconcileTestPlanConfig(0), false, 0)
			if err != nil || plan.SubsidyPayment == nil || plan.NetworkPayments[f.network] == nil {
				t.Fatal("plan", plan, err)
			}
			return plan
		}
		contract(0, 1, UsdToNanoCents(1))
		contract(1, 2, UsdToNanoCents(1))
		plan()
		contract(3, 4, UsdToNanoCents(2))
		contract(4, 5, UsdToNanoCents(3))
		canceled := plan().NetworkPayments[f.network]
		if holders := GetSnReconcileEarlyHolders(ctx); len(holders) != 0 {
			t.Fatal("consecutive plans left early sweeps", holders)
		}
		if err := CancelPayment(ctx, canceled.PaymentId); err != nil {
			t.Fatal(err)
		}
		contract(6, 7, UsdToNanoCents(4))
		third := plan()
		if !third.SubsidyPayment.StartTime.Equal(at(5)) {
			t.Fatal("the re-plan's epoch did not start at the canceled plan's end", third.SubsidyPayment.StartTime)
		}
		holders := GetSnReconcileEarlyHolders(ctx)
		if len(holders) != 1 || holders[0].PaymentId != third.NetworkPayments[f.network].PaymentId || holders[0].State != "planned" ||
			holders[0].EarlyRevenue != UsdToNanoCents(5) || holders[0].EarlyByteCount != 20 {
			t.Fatal("re-planned sweeps are not early in their new row", holders)
		}
	})
}

// A2: a plan that recorded no epoch does not clamp the next one, so the row
// that re-plans its sweeps finds them early through the earlier row.
func TestSnReconcileEarlyEarningsAfterPlanWithoutEpoch(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		f := newPayoutTransitionCohort(t, ctx)
		now := server.NowUtc().Truncate(time.Microsecond)
		contract := func(created time.Time, closed time.Time, revenue NanoCents) {
			f.insert(t, ctx, created, &closed, closed, 10, revenue)
		}
		contract(now.Add(-7*time.Hour), now.Add(-6*time.Hour), UsdToNanoCents(2))
		short, err := CreatePaymentPlan(ctx, snReconcileTestPlanConfig(30*24*time.Hour), false, 0)
		if err != nil || short.SubsidyPayment != nil || short.NetworkPayments[f.network] == nil {
			t.Fatal("a short plan recorded an epoch", short, err)
		}
		if err := CancelPayment(ctx, short.NetworkPayments[f.network].PaymentId); err != nil {
			t.Fatal(err)
		}
		contract(now.Add(time.Hour), now.Add(2*time.Hour), UsdToNanoCents(4))
		replan, err := CreatePaymentPlan(ctx, snReconcileTestPlanConfig(0), false, 0)
		if err != nil || replan.SubsidyPayment == nil || !replan.SubsidyPayment.StartTime.Equal(now.Add(-7*time.Hour)) {
			t.Fatal("the re-plan's epoch was clamped", replan, err)
		}
		holders := GetSnReconcileEarlyHolders(ctx)
		if len(holders) != 1 || holders[0].PaymentId != replan.NetworkPayments[f.network].PaymentId || holders[0].EarlyRevenue != UsdToNanoCents(2) || holders[0].EarlyByteCount != 10 {
			t.Fatal("the sweep re-planned from a plan without an epoch is not early", holders)
		}
	})
}

// A2: a bounded multi-slice drain shows no false early sweep, also for a
// contract that opened in one slice and closed in a later one.
func TestSnReconcileEarlyEarningsNotInDrainSlices(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		f := newPayoutTransitionCohort(t, ctx)
		base := server.NowUtc().Truncate(time.Microsecond).Add(-4 * 24 * time.Hour)
		contract := func(created int, closed int) {
			closeTime := base.Add(time.Duration(closed) * time.Hour)
			f.insert(t, ctx, base.Add(time.Duration(created)*time.Hour), &closeTime, closeTime, 10, UsdToNanoCents(1))
		}
		contract(0, 1)
		if _, err := CreatePaymentPlan(ctx, snReconcileTestPlanConfig(0), false, 0); err != nil {
			t.Fatal(err)
		}
		contract(2, 10)
		contract(3, 50)
		contract(20, 30)
		contract(40, 45)
		rows := 1
		for slice := 0; slice < 3; slice++ {
			plan, err := CreatePaymentPlan(ctx, snReconcileTestPlanConfig(0), false, 24*time.Hour)
			if err != nil || plan.SubsidyPayment == nil {
				t.Fatal("slice", slice, plan, err)
			}
			rows += len(plan.NetworkPayments)
		}
		var unpaid int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM transfer_escrow_sweep WHERE payment_id IS NULL`).Scan(&unpaid))
		})
		if rows != 4 || unpaid != 0 {
			t.Fatal("the drain did not plan one row per slice", rows, unpaid)
		}
		if holders := GetSnReconcileEarlyHolders(ctx); len(holders) != 0 {
			t.Fatal("drain slices show early sweeps", holders)
		}
	})
}

// A4: the wallet address of each row holding a listed record.
func TestSnReconcileRecordAddresses(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		walletId := server.NewId()
		snReconcileTestExec(t, ctx, `INSERT INTO account_wallet(wallet_id,network_id,wallet_type,blockchain,wallet_address,active,default_token_type)
			VALUES($1,$2,'external','MATIC',E' 0xAA\t',false,'USDC')`, walletId, networkId)
		withWallet := snReconcileTestBareRow(t, ctx, nil, nil, nil)
		snReconcileTestExec(t, ctx, `UPDATE account_payment SET payment_record='rec-1',wallet_id=$2 WHERE payment_id=$1`, withWallet, walletId)
		withoutWallet := snReconcileTestBareRow(t, ctx, nil, nil, nil)
		snReconcileTestExec(t, ctx, `UPDATE account_payment SET payment_record='rec-2' WHERE payment_id=$1`, withoutWallet)
		unlisted := "unlisted"
		snReconcileTestBareRow(t, ctx, &unlisted, nil, nil)
		got := GetSnReconcileRecordAddresses(ctx, []string{"rec-1", "rec-2", "rec-3"})
		if len(got) != 2 || got[0].Record != "rec-1" || *got[0].WalletAddress != " 0xAA\t" || got[1].Record != "rec-2" || got[1].WalletAddress != nil {
			t.Fatal("record addresses", got)
		}
	})
}

// A7: revived rows that are not yet completed, with their network's payout wallet.
func TestSnReconcileRevivedPending(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		f := newPayoutTransitionCohort(t, ctx)
		wallet := GetPayoutWalletId(ctx, f.network)
		revived := func(networkId server.Id, eventType string, payout NanoCents) server.Id {
			paymentId := snReconcileTestPaymentPlus(t, ctx, payout, networkId, wallet, payoutTestCutoff)
			snReconcileTestExec(t, ctx, `INSERT INTO audit_account_payment(event_id,payment_id,event_type,event_details) VALUES($1,$2,$3,'{}')`, server.NewId(), paymentId, eventType)
			return paymentId
		}
		pending := revived(f.network, "sn_reconcile_revived", UsdToNanoCents(3))
		walletless := revived(server.NewId(), "sn_reconcile_revived", UsdToNanoCents(2))
		completed := revived(f.network, "sn_reconcile_revived", UsdToNanoCents(1))
		snReconcileTestExec(t, ctx, `UPDATE account_payment SET completed=true WHERE payment_id=$1`, completed)
		canceled := revived(f.network, "sn_reconcile_revived", UsdToNanoCents(1))
		snReconcileTestExec(t, ctx, `UPDATE account_payment SET canceled=true WHERE payment_id=$1`, canceled)
		revived(f.network, "circle_attempt_request", UsdToNanoCents(1))
		snReconcileTestPayment(t, ctx, f.network, wallet, payoutTestCutoff)

		got := map[server.Id]*SnReconcileRevived{}
		for _, row := range GetSnReconcileRevivedPending(ctx) {
			got[row.PaymentId] = row
		}
		if len(got) != 2 || got[pending].Payout != UsdToNanoCents(3) || *got[pending].PayoutWalletBlockchain != "MATIC" || !got[pending].PayoutWalletActive ||
			got[walletless].Payout != UsdToNanoCents(2) || got[walletless].PayoutWalletBlockchain != nil || got[walletless].PayoutWalletActive {
			t.Fatal("revived pending", got)
		}
	})
}

// A1: a request journaled under the key after the census leaves the row
// untouched, whatever its payment or type.
func TestSnReconcileReviveRefusesJournaledKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, row := snReconcileTestRevivable(t, ctx)
		snReconcileTestExec(t, ctx, `INSERT INTO audit_account_payment(event_id,payment_id,event_type,event_details) VALUES($1,$2,'circle_attempt_request','{}')`,
			*row.CircleIdempotencyKey, server.NewId())
		before := snReconcileTestFingerprint(t, ctx, row.PaymentId)
		if ReviveSnReconcileRow(ctx, row, nil, server.NewId(), nil, func(server.PgTx) { t.Fatal("journaled row scheduled") }) {
			t.Fatal("a row whose key was journaled was revived")
		}
		if after := snReconcileTestFingerprint(t, ctx, row.PaymentId); after != before {
			t.Fatalf("journaled revive wrote: %s", after)
		}
	})
}
