package model

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"

	"github.com/urnetwork/server/session"
)

// A historical synthetic boundary makes delayed planning exercise the actual
// public path without changing process clocks. SQL uses microsecond precision.
var payoutTestCutoff = time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC)

func usePayoutTransition(t testing.TB) {
	t.Helper()
	data := fmt.Sprintf("schema: urnetwork-provider-payout-transition-v1\ncutoff_utc: %q\nattribution: settled_contract_close_time\nlegacy_usdc: finish_pre_cutoff_obligations\nmainnet:\n  profile: mainnet\n  chain_id: 964\n  genesis_hash: %q\n  netuid: 25\n  activation: blocked\n", payoutTestCutoff.Format(time.RFC3339), "0x"+strings.Repeat("11", 32))
	t.Cleanup(server.Config.PushSimpleResource("sn.yml", []byte(data)))
	policyForPreparation, preparationErr := server.LoadProviderPayoutTransition(context.Background())
	if preparationErr != nil {
		t.Fatal(preparationErr)
	}
	if _, err := server.PrepareProviderPayoutBoundary(context.Background(), policyForPreparation.ConfigSha256); err != nil {
		t.Fatal(err)
	}
}

type payoutTransitionCohort struct {
	sourceNetwork, source, network, client, balance server.Id
	session                                         *session.ClientSession
}

func newPayoutTransitionCohort(t testing.TB, ctx context.Context) *payoutTransitionCohort {
	t.Helper()
	f := &payoutTransitionCohort{sourceNetwork: server.NewId(), source: server.NewId(), network: server.NewId(), client: server.NewId()}
	testingCreatePaymentClient(ctx, f.sourceNetwork, f.source)
	testingCreatePaymentClient(ctx, f.network, f.client)
	sourceSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: f.sourceNetwork, ClientId: &f.source})
	t.Cleanup(sourceSession.Cancel)
	f.session = session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: f.network, ClientId: &f.client})
	t.Cleanup(f.session.Cancel)
	code, err := CreateBalanceCode(ctx, 1024*1024*1024, 365*24*time.Hour, UsdToNanoCents(100), "transition-"+server.NewId().String(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	testingRedeemPaymentBalanceCode(t, ctx, f.sourceNetwork, code.Secret)
	balances := GetActiveTransferBalances(ctx, f.sourceNetwork)
	if len(balances) != 1 {
		t.Fatal("missing fixture balance")
	}
	f.balance = balances[0].BalanceId
	wallet := CreateAccountWalletExternal(f.session, &CreateAccountWalletExternalArgs{NetworkId: f.network, Blockchain: "MATIC", WalletAddress: "0x0000000000000000000000000000000000000012", DefaultTokenType: "USDC"})
	if wallet == nil {
		t.Fatal("missing fixture wallet")
	}
	if err := SetPayoutWallet(ctx, f.network, *wallet); err != nil {
		t.Fatal(err)
	}
	return f
}

func (self *payoutTransitionCohort) insert(t testing.TB, ctx context.Context, created time.Time, closed *time.Time, swept time.Time, bytes ByteCount, revenue NanoCents) server.Id {
	t.Helper()
	id := server.NewId()
	var outcome *string
	var usage *contractUsageSnapshot
	if closed != nil {
		settled := "settled"
		outcome = &settled
		usage = &contractUsageSnapshot{Version: 1, ByteCount: bytes, Providers: []contractProviderUsage{{ClientId: self.client, NetworkId: self.network, ByteCount: bytes}}}
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
		(contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,create_time,close_time,outcome,provider_usage,usage_origin_is_source)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,true)`, id, self.sourceNetwork, self.source, self.network, self.client, bytes, created, closed, outcome, usage))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow_sweep
		(contract_id,balance_id,network_id,destination_id,payout_byte_count,payout_net_revenue_nano_cents,sweep_time)
		VALUES($1,$2,$3,$4,$5,$6,$7)`, id, self.balance, self.network, self.client, bytes, revenue, swept))
	})
	return id
}

func payoutTransitionRevenueConfig() *SubsidyConfig {
	config := *EnvSubsidyConfig()
	config.MinPayoutUsd = 0
	config.UsdPerActiveUser = 0
	config.SubscriptionNetRevenueFraction = 0
	config.MinWalletPayoutUsd = 0
	return &config
}

func TestProviderTransitionPlannerPartitionAndDelayedSweep(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		usePayoutTransition(t)
		f := newPayoutTransitionCohort(t, ctx)
		before := payoutTestCutoff.Add(-time.Microsecond)
		at := payoutTestCutoff
		after := at.Add(time.Microsecond)
		legacy := f.insert(t, ctx, at.Add(-time.Hour), &before, at.Add(3*time.Hour), 11, UsdToNanoCents(1))
		f.insert(t, ctx, at.Add(-time.Hour), &at, at.Add(time.Hour), 22, UsdToNanoCents(2))
		f.insert(t, ctx, at.Add(-time.Hour), &after, at.Add(time.Hour), 33, UsdToNanoCents(3))
		plan, err := CreatePaymentPlan(ctx, payoutTransitionRevenueConfig(), false, 0)
		if err != nil {
			t.Fatal(err)
		}
		payment := plan.NetworkPayments[f.network]
		if payment == nil || payment.PayoutByteCount != 11 || payment.Payout != UsdToNanoCents(1) {
			t.Fatalf("legacy plan included cutoff/new usage: %+v", payment)
		}
		if err := RequireProviderUsdcPayment(ctx, payment.PaymentId); err != nil {
			t.Fatalf("late original debt refused: %v", err)
		}
		usages, err := GetStEpochProviderUsageAtEpoch(ctx, 7, at.Add(-time.Hour), at.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if len(usages) != 1 || usages[0].PayoutByteCount != 55 {
			t.Fatalf("subnet duplicate or lost earning: %+v", usages)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var retained int
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM transfer_escrow_sweep WHERE contract_id<>$1 AND payment_id IS NULL`, legacy).Scan(&retained))
			if retained != 2 {
				t.Fatal("new subnet earning was consumed by legacy plan")
			}
		})
		if err := CancelPayment(ctx, payment.PaymentId); err != nil {
			t.Fatal(err)
		}
		plan, err = CreatePaymentPlan(ctx, payoutTransitionRevenueConfig(), false, 0)
		if err != nil {
			t.Fatal(err)
		}
		if p := plan.NetworkPayments[f.network]; p == nil || p.PayoutByteCount != 11 || p.PaymentId == payment.PaymentId {
			t.Fatalf("cancel/replan changed earning asset: %+v", p)
		}
	})
}

func TestProviderTransitionSubsidyOnlyFinalShortTail(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		usePayoutTransition(t)
		f := newPayoutTransitionCohort(t, ctx)
		closed := payoutTestCutoff.Add(-time.Microsecond)
		f.insert(t, ctx, closed.Add(-90*time.Second), &closed, payoutTestCutoff.Add(time.Hour), 1024, 0)
		config := payoutTransitionRevenueConfig()
		config.MinPayoutUsd = 100
		config.MinWalletPayoutUsd = 1000
		plan, err := CreatePaymentPlan(ctx, config, false, 0)
		if err != nil {
			t.Fatal(err)
		}
		payment := plan.NetworkPayments[f.network]
		if plan.SubsidyPayment == nil || payment == nil || payment.SubsidyPayout <= 0 || payment.Payout != payment.SubsidyPayout {
			t.Fatalf("final short subsidy-only entitlement stranded: %+v", payment)
		}
		if !plan.SubsidyPayment.EndTime.Before(payoutTestCutoff) {
			t.Fatal("legacy subsidy crossed cutoff")
		}
		if err := RequireProviderUsdcPayment(ctx, payment.PaymentId); err != nil {
			t.Fatalf("valid subsidy-only debt refused: %v", err)
		}
		second, err := CreatePaymentPlan(ctx, config, false, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(second.NetworkPayments) != 0 {
			t.Fatal("subsidy paid twice")
		}
	})
}

func TestProviderTransitionQuarantinesAmbiguousWithoutBlockingPeer(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		usePayoutTransition(t)
		f := newPayoutTransitionCohort(t, ctx)
		closed := payoutTestCutoff.Add(-time.Microsecond)
		f.insert(t, ctx, closed.Add(-time.Hour), &closed, payoutTestCutoff.Add(time.Hour), 11, UsdToNanoCents(1))
		ambiguous := f.insert(t, ctx, payoutTestCutoff.Add(-time.Hour), nil, payoutTestCutoff.Add(time.Hour), 22, UsdToNanoCents(2))
		plan, err := CreatePaymentPlan(ctx, payoutTransitionRevenueConfig(), false, 0)
		if err != nil {
			t.Fatal(err)
		}
		if p := plan.NetworkPayments[f.network]; p == nil || p.PayoutByteCount != 11 {
			t.Fatalf("ambiguous source contaminated or stopped exact debt: %+v", p)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var unpaid bool
			server.Raise(conn.QueryRow(ctx, `SELECT payment_id IS NULL FROM transfer_escrow_sweep WHERE contract_id=$1`, ambiguous).Scan(&unpaid))
			if !unpaid {
				t.Fatal("ambiguous liability discarded")
			}
		})
		if strings.Count(paymentTransitionUnresolvedSampleSql, "LIMIT 1024") != 2 || !strings.Contains(paymentTransitionUnresolvedSampleSql, "UNION ALL") {
			t.Fatal("unresolved diagnostic lost bounded indexed drivers")
		}
	})
}

func TestProviderTransitionLegacyAdmissionRejectsUnboundAndMixed(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		f := newPayoutTransitionCohort(t, ctx)
		before := payoutTestCutoff.Add(-time.Microsecond)
		at := payoutTestCutoff
		f.insert(t, ctx, at.Add(-time.Hour), &before, at.Add(time.Hour), 11, UsdToNanoCents(1))
		f.insert(t, ctx, at.Add(-time.Hour), &at, at.Add(time.Hour), 22, UsdToNanoCents(2))
		plan, err := CreatePaymentPlan(ctx, payoutTransitionRevenueConfig(), false, 0)
		if err != nil {
			t.Fatal(err)
		}
		payment := plan.NetworkPayments[f.network]
		if payment == nil {
			t.Fatal("missing historical mixed fixture")
		}
		usePayoutTransition(t)
		if err := RequireProviderUsdcPayment(ctx, payment.PaymentId); err == nil {
			t.Fatal("mixed queued payment admitted")
		}
		unbound := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO account_payment(payment_id,payment_plan_id,network_id,payout_byte_count,payout_nano_cents,min_sweep_time) VALUES($1,$2,$3,0,100,$4)`, unbound, server.NewId(), f.network, before))
		})
		if err := RequireProviderUsdcPayment(ctx, unbound); err == nil {
			t.Fatal("unbound invented payment admitted")
		}
		got, err := GetPayment(ctx, payment.PaymentId)
		if err != nil || got.Canceled || got.Completed {
			t.Fatal("refusal destroyed queued liability")
		}
	})
}

func TestProviderTransitionReliabilityBucketCrossingIsRetained(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		usePayoutTransition(t)
		f := newPayoutTransitionCohort(t, ctx)
		UpsertVerifyProviderStats(ctx, []*VerifyProviderStatsRow{{PeriodStart: payoutTestCutoff.Add(-time.Second), PeriodEnd: payoutTestCutoff.Add(time.Second), ClientId: f.client, Assignments: 2, Confirmations: 2}})
		if _, err := GetStEpochPayoutReliability(ctx, payoutTestCutoff.Add(-time.Hour), payoutTestCutoff.Add(time.Hour), []server.Id{f.client}); err == nil {
			t.Fatal("aggregate exposure was prorated or double-attributed")
		}
		if rows, err := GetStEpochPayoutReliability(ctx, payoutTestCutoff.Add(time.Hour), payoutTestCutoff.Add(2*time.Hour), []server.Id{f.client}); err != nil || len(rows) != 0 {
			t.Fatalf("independent later epoch blocked: %v", err)
		}
		if _, err := GetStEpochPayoutReliability(ctx, payoutTestCutoff.Add(-time.Hour), payoutTestCutoff.Add(time.Hour), []server.Id{server.NewId()}); err != nil {
			t.Fatalf("irrelevant provider blocked peer: %v", err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var n int
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM verify_provider_stats`).Scan(&n))
			if n != 1 {
				t.Fatal("ambiguous original evidence deleted")
			}
		})
	})
}

func TestProviderTransitionPublicPayoutStatsRetainLegacyAsset(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		usePayoutTransition(t)
		f := newPayoutTransitionCohort(t, ctx)
		before := payoutTestCutoff.Add(-time.Microsecond)
		at := payoutTestCutoff
		swept := at.Add(time.Hour)
		f.insert(t, ctx, at.Add(-time.Hour), &before, swept, 11, UsdToNanoCents(1))
		f.insert(t, ctx, at.Add(-time.Hour), &at, swept, 22, UsdToNanoCents(2))
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := queryProviderPayoutStats(ctx, conn, f.network, at, at.Add(2*time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			total := NanoCents(0)
			for _, amount := range rows[f.client] {
				total += amount
			}
			if total != UsdToNanoCents(1) {
				t.Fatalf("post-cutoff usage reported as USDC: %d", total)
			}
		})
	})
}

func TestProviderTransitionFreeAndPaidUsageHaveEqualSubnetWeight(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		usePayoutTransition(t)
		free, paid := newPayoutTransitionCohort(t, ctx), newPayoutTransitionCohort(t, ctx)
		closed := payoutTestCutoff.Add(time.Microsecond)
		free.insert(t, ctx, payoutTestCutoff.Add(-time.Hour), &closed, closed.Add(time.Hour), 1024, 0)
		paid.insert(t, ctx, payoutTestCutoff.Add(-time.Hour), &closed, closed.Add(time.Hour), 1024, UsdToNanoCents(9))
		// Real nonterminal and canceled snapshots cannot mint usage even if an
		// escrow row exists. Both share the same actual model read below.
		free.insert(t, ctx, payoutTestCutoff.Add(-time.Hour), nil, closed.Add(time.Hour), 4096, 0)
		canceled := paid.insert(t, ctx, payoutTestCutoff.Add(-time.Hour), nil, closed.Add(time.Hour), 8192, 0)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET outcome='canceled',close_time=$2 WHERE contract_id=$1`, canceled, closed))
		})
		// An open contract has a NULL outcome, just like the production writer.
		// The legal NULL -> canceled transition above must then be immutable.
		server.Db(ctx, func(conn server.PgConn) {
			if _, err := conn.Exec(ctx, `UPDATE transfer_contract SET close_time=$2 WHERE contract_id=$1`, canceled, closed.Add(time.Microsecond)); err == nil {
				t.Fatal("terminal canceled attribution remained mutable")
			}
		})
		rows, err := GetStEpochProviderUsageAtEpoch(ctx, 9, payoutTestCutoff.Add(-time.Hour), closed.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 2 || rows[0].PayoutByteCount != 1024 || rows[1].PayoutByteCount != 1024 {
			t.Fatalf("subnet weights inherited USDC revenue: %+v", rows)
		}
		plan, err := CreatePaymentPlan(ctx, payoutTransitionRevenueConfig(), false, 0)
		if err != nil || len(plan.NetworkPayments) != 0 {
			t.Fatalf("post-cutoff paid/free traffic also became USDC: %+v %v", plan, err)
		}
	})
}
