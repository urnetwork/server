package model

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// A synthetic legacy contract set: one contract of every category, written as
// the retired settlement left them (an outcome claimed, its escrows unsettled,
// no sweep, no debit) or as the excluded states are.
type contractLegacySettlementFixture struct {
	payerNetworkId    server.Id
	payerClientId     server.Id
	providerNetworkId server.Id
	providerClientId  server.Id
	// an expired grant with revenue, an expired Redis-reserved grant, a live grant
	expiredBalanceId server.Id
	redisBalanceId   server.Id
	liveBalanceId    server.Id
	missingBalanceId server.Id
	// category -> contract
	contractIds map[string]server.Id
	// the settle category's Redis-reserved contract
	redisContractId server.Id
}

const (
	contractLegacySettlementTestStart   = ByteCount(100000)
	contractLegacySettlementTestEscrow  = ByteCount(1000)
	contractLegacySettlementTestRevenue = NanoCents(4000)
)

func newContractLegacySettlementFixture(t testing.TB, ctx context.Context) *contractLegacySettlementFixture {
	t.Helper()
	f := &contractLegacySettlementFixture{
		payerNetworkId: server.NewId(), payerClientId: server.NewId(),
		providerNetworkId: server.NewId(), providerClientId: server.NewId(),
		expiredBalanceId: server.NewId(), redisBalanceId: server.NewId(), liveBalanceId: server.NewId(),
		missingBalanceId: server.NewId(),
		contractIds:      map[string]server.Id{},
	}
	now := server.NowUtc()
	longAgo := now.Add(-60 * 24 * time.Hour)
	server.Tx(ctx, func(tx server.PgTx) {
		for _, balance := range []struct {
			balanceId server.Id
			endTime   time.Time
			revenue   NanoCents
		}{
			{balanceId: f.expiredBalanceId, endTime: now.Add(-30 * 24 * time.Hour), revenue: contractLegacySettlementTestRevenue},
			{balanceId: f.redisBalanceId, endTime: now.Add(-30 * 24 * time.Hour), revenue: 0},
			{balanceId: f.liveBalanceId, endTime: now.Add(30 * 24 * time.Hour), revenue: 0},
		} {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_balance
				(balance_id,network_id,start_time,end_time,start_balance_byte_count,net_revenue_nano_cents,balance_byte_count,pro)
				VALUES ($1,$2,$3,$4,$5,$6,$5,false)`,
				balance.balanceId, f.payerNetworkId, longAgo.Add(-time.Hour), balance.endTime, contractLegacySettlementTestStart, balance.revenue))
		}
	})
	usage := `{"version":1,"byte_count":0,"providers":[],"excluded_reason":"expired_unconfirmed"}`
	// one contract: its two reports, its escrows on the grants, closed at closeTime
	// (open when nil)
	add := func(category string, closeTime *time.Time, sourceUsed ByteCount, destinationUsed ByteCount, balanceIds []server.Id, redis bool) server.Id {
		contractId := ContractIdTimeBound(longAgo.Add(time.Duration(len(f.contractIds)+1) * time.Second))
		entropy := server.NewId()
		copy(contractId[6:], entropy[6:])
		server.Tx(ctx, func(tx server.PgTx) {
			if closeTime == nil {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
					(contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,payer_network_id,create_time)
					VALUES ($1,$2,$3,$4,$5,$6,$2,$7)`,
					contractId, f.payerNetworkId, f.payerClientId, f.providerNetworkId, f.providerClientId, contractLegacySettlementTestEscrow, longAgo))
			} else {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
					(contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,payer_network_id,create_time,
					close_time,outcome,provider_usage)
					VALUES ($1,$2,$3,$4,$5,$6,$2,$7,$8,'settled',$9::jsonb)`,
					contractId, f.payerNetworkId, f.payerClientId, f.providerNetworkId, f.providerClientId, contractLegacySettlementTestEscrow, longAgo,
					*closeTime, usage))
			}
			for _, balanceId := range balanceIds {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow (contract_id,balance_id,balance_byte_count,redis_reserved)
					VALUES ($1,$2,$3,$4)`, contractId, balanceId, contractLegacySettlementTestEscrow, redis))
			}
			reports := []struct {
				party ContractParty
				used  ByteCount
			}{{party: ContractPartySource, used: sourceUsed}, {party: ContractPartyDestination, used: destinationUsed}}
			if category == ContractLegacySettlementFlagQuarantine {
				reports = reports[:1]
			}
			for _, report := range reports {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close (contract_id,party,used_transfer_byte_count,close_time,checkpoint)
					VALUES ($1,$2,$3,$4,false)`, contractId, report.party, report.used, longAgo))
			}
		})
		f.contractIds[category] = contractId
		return contractId
	}
	closedLongAgo := longAgo.Add(time.Minute)
	closedNow := now
	add(ContractLegacySettlementSettle, &closedLongAgo, 600, 600, []server.Id{f.expiredBalanceId}, false)
	f.redisContractId = add("settle_redis", &closedLongAgo, 400, 400, []server.Id{f.redisBalanceId}, true)
	add(ContractLegacySettlementFlagZero, &closedLongAgo, 0, 0, []server.Id{f.expiredBalanceId}, false)
	add(ContractLegacySettlementFlagQuarantine, &closedLongAgo, 300, 0, []server.Id{f.expiredBalanceId}, false)
	add(ContractLegacySettlementFlagUnfunded, &closedLongAgo, 500, 500, []server.Id{f.missingBalanceId}, false)
	swept := add(ContractLegacySettlementFlagSwept, &closedLongAgo, 200, 200, []server.Id{f.expiredBalanceId}, false)
	add(ContractLegacySettlementHoldLiveGrant, &closedLongAgo, 100, 100, []server.Id{f.liveBalanceId}, false)
	add(ContractLegacySettlementExcludedOpen, nil, 100, 100, []server.Id{f.expiredBalanceId}, false)
	add(ContractLegacySettlementExcludedRecent, &closedNow, 100, 100, []server.Id{f.expiredBalanceId}, false)
	pending := add(ContractLegacySettlementExcludedDebitPending, &closedLongAgo, 100, 100, []server.Id{f.redisBalanceId}, true)
	server.Tx(ctx, func(tx server.PgTx) {
		// the swept contract's sweep post ran, and a USDC payment owns it
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow_sweep
			(contract_id,balance_id,network_id,payout_byte_count,payout_net_revenue_nano_cents,sweep_time,payment_id)
			VALUES ($1,$2,$3,200,4,$4,$5)`, swept, f.expiredBalanceId, f.providerNetworkId, closedLongAgo, server.NewId()))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_debit_journal (contract_id,balance_id,debit_byte_count,shard)
			VALUES ($1,$2,100,$3)`, pending, f.redisBalanceId, transferDebitShard(f.redisBalanceId)))
	})
	return f
}

// The rows a settle may change, by contract, as one comparable text.
func contractLegacySettlementTestState(t testing.TB, ctx context.Context, f *contractLegacySettlementFixture) string {
	t.Helper()
	var builder strings.Builder
	server.Db(ctx, func(conn server.PgConn) {
		for _, category := range append(append([]string{}, ContractLegacySettlementCategories...), "settle_redis") {
			contractId, ok := f.contractIds[category]
			if !ok {
				continue
			}
			var settled, unsettled, sweeps, journals int
			var payout, sweepBytes, journalBytes ByteCount
			server.Raise(conn.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM transfer_escrow WHERE contract_id=$1 AND settled),
				(SELECT count(*) FROM transfer_escrow WHERE contract_id=$1 AND NOT settled),
				(SELECT coalesce(sum(payout_byte_count),0)::bigint FROM transfer_escrow WHERE contract_id=$1 AND settled),
				(SELECT count(*) FROM transfer_escrow_sweep WHERE contract_id=$1),
				(SELECT coalesce(sum(payout_byte_count),0)::bigint FROM transfer_escrow_sweep WHERE contract_id=$1),
				(SELECT count(*) FROM transfer_debit_journal WHERE contract_id=$1),
				(SELECT coalesce(sum(debit_byte_count),0)::bigint FROM transfer_debit_journal WHERE contract_id=$1)`,
				contractId).Scan(&settled, &unsettled, &payout, &sweeps, &sweepBytes, &journals, &journalBytes))
			fmt.Fprintf(&builder, "%s settled=%d unsettled=%d payout=%d sweeps=%d sweep_bytes=%d journals=%d journal_bytes=%d\n",
				category, settled, unsettled, payout, sweeps, sweepBytes, journals, journalBytes)
		}
		for _, balanceId := range []server.Id{f.expiredBalanceId, f.redisBalanceId, f.liveBalanceId} {
			var balance ByteCount
			server.Raise(conn.QueryRow(ctx, `SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$1`, balanceId).Scan(&balance))
			fmt.Fprintf(&builder, "balance %d\n", balance)
		}
	})
	return builder.String()
}

func contractLegacySettlementTestSettings() *ContractLegacySettlementSettings {
	settings := DefaultContractLegacySettlementSettings()
	settings.MaxContractsPerSecond = 1000
	settings.PlanBatchPause = 0
	return settings
}

// Before this, nothing settled an escrow its settlement's lost post or the
// force close's quarantine left unsettled: the reaper kept the contract for
// good. A planned settle writes each writing category once and exactly what
// the plan totals say, leaves the held and excluded contracts untouched, and a
// second run changes nothing.
func TestContractLegacySettlementSettlesOnceAndRerunIsNoop(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		f := newContractLegacySettlementFixture(t, ctx)
		settings := contractLegacySettlementTestSettings()
		logf := func(string, ...any) {}
		before := contractLegacySettlementTestState(t, ctx, f)

		plan, err := PlanContractLegacySettlement(ctx, settings, logf)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]int64{
			ContractLegacySettlementSettle:               2,
			ContractLegacySettlementFlagZero:             1,
			ContractLegacySettlementFlagQuarantine:       1,
			ContractLegacySettlementFlagUnfunded:         1,
			ContractLegacySettlementFlagSwept:            1,
			ContractLegacySettlementHoldLiveGrant:        1,
			ContractLegacySettlementExcludedOpen:         1,
			ContractLegacySettlementExcludedRecent:       1,
			ContractLegacySettlementExcludedDebitPending: 1,
		}
		for _, category := range ContractLegacySettlementCategories {
			if got := plan.Categories[category].Contracts; got != want[category] {
				t.Errorf("plan %s: %d contracts, want %d", category, got, want[category])
			}
		}
		planned := plan.Categories[ContractLegacySettlementSettle]
		if planned.ChargeByteCount != 600+400 || planned.SweepByteCount != 600+400 || planned.ProviderNetworks != 1 || planned.PayerNetworks != 1 ||
			planned.SweepNanoCents != NanoCents(0.5*float64(contractLegacySettlementTestRevenue)*600/float64(contractLegacySettlementTestStart)) {
			t.Fatalf("plan settle totals %+v", planned)
		}
		if plan.WriteContracts() != 6 {
			t.Fatalf("plan writes %d contracts, want 6", plan.WriteContracts())
		}
		if after := contractLegacySettlementTestState(t, ctx, f); after != before {
			t.Fatalf("the plan wrote:\n%s\nwant\n%s", after, before)
		}

		if _, _, err := SettleContractLegacyEscrows(ctx, settings, "0000000000000000", logf); err == nil {
			t.Fatal("a settle ran without the plan's token")
		}
		if after := contractLegacySettlementTestState(t, ctx, f); after != before {
			t.Fatalf("a refused settle wrote:\n%s", after)
		}

		_, settled, err := SettleContractLegacyEscrows(ctx, settings, plan.Token, logf)
		if err != nil {
			t.Fatal(err)
		}
		for _, category := range contractLegacySettlementWriteCategories {
			if got, want := settled.Categories[category].Contracts, plan.Categories[category].Contracts; got != want {
				t.Errorf("settled %s: %d contracts, planned %d", category, got, want)
			}
			// the totals written are the totals planned
			s, p := settled.Categories[category], plan.Categories[category]
			if s.EscrowRows != p.EscrowRows || s.EscrowByteCount != p.EscrowByteCount || s.ChargeByteCount != p.ChargeByteCount ||
				s.SweepByteCount != p.SweepByteCount || s.SweepNanoCents != p.SweepNanoCents ||
				s.ProviderNetworks != p.ProviderNetworks || s.PayerNetworks != p.PayerNetworks {
				t.Errorf("settled %s totals %+v, planned %+v", category, s, p)
			}
		}
		for _, category := range []string{ContractLegacySettlementBusy, ContractLegacySettlementChanged, ContractLegacySettlementFailed} {
			if 0 < settled.Categories[category].Contracts {
				t.Errorf("settle %s: %d contracts", category, settled.Categories[category].Contracts)
			}
		}
		state := contractLegacySettlementTestState(t, ctx, f)
		wantState := strings.Join([]string{
			"settle settled=1 unsettled=0 payout=600 sweeps=1 sweep_bytes=600 journals=0 journal_bytes=0",
			"flag_zero settled=1 unsettled=0 payout=0 sweeps=0 sweep_bytes=0 journals=0 journal_bytes=0",
			"flag_quarantine settled=1 unsettled=0 payout=0 sweeps=0 sweep_bytes=0 journals=0 journal_bytes=0",
			"flag_unfunded settled=1 unsettled=0 payout=0 sweeps=0 sweep_bytes=0 journals=0 journal_bytes=0",
			"flag_swept settled=1 unsettled=0 payout=0 sweeps=1 sweep_bytes=200 journals=0 journal_bytes=0",
			"hold_live_grant settled=0 unsettled=1 payout=0 sweeps=0 sweep_bytes=0 journals=0 journal_bytes=0",
			"excluded_open settled=0 unsettled=1 payout=0 sweeps=0 sweep_bytes=0 journals=0 journal_bytes=0",
			"excluded_recent settled=0 unsettled=1 payout=0 sweeps=0 sweep_bytes=0 journals=0 journal_bytes=0",
			"excluded_debit_pending settled=0 unsettled=1 payout=0 sweeps=0 sweep_bytes=0 journals=1 journal_bytes=100",
			"settle_redis settled=1 unsettled=0 payout=400 sweeps=1 sweep_bytes=400 journals=1 journal_bytes=400",
			fmt.Sprintf("balance %d", contractLegacySettlementTestStart-600),
			fmt.Sprintf("balance %d", contractLegacySettlementTestStart),
			fmt.Sprintf("balance %d", contractLegacySettlementTestStart),
		}, "\n") + "\n"
		if state != wantState {
			t.Fatalf("after the settle:\n%s\nwant\n%s", state, wantState)
		}

		// a second run finds only the held and excluded contracts, plans no
		// write, and writes nothing
		replan, err := PlanContractLegacySettlement(ctx, settings, logf)
		if err != nil {
			t.Fatal(err)
		}
		if replan.WriteContracts() != 0 || replan.Token == plan.Token {
			t.Fatalf("replan writes %d contracts with token %s (first %s)", replan.WriteContracts(), replan.Token, plan.Token)
		}
		_, resettled, err := SettleContractLegacyEscrows(ctx, settings, replan.Token, logf)
		if err != nil {
			t.Fatal(err)
		}
		for _, category := range ContractLegacySettlementCategories {
			if 0 < resettled.Categories[category].Contracts {
				t.Errorf("rerun %s: %d contracts", category, resettled.Categories[category].Contracts)
			}
		}
		if again := contractLegacySettlementTestState(t, ctx, f); again != state {
			t.Fatalf("the rerun wrote:\n%s\nwant\n%s", again, state)
		}
	})
}

// A dry run's token is refused once the candidates change: an unsettled
// contract added after the dry run makes the settle refuse, and nothing is
// written.
func TestContractLegacySettlementRefusesAStalePlan(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		f := newContractLegacySettlementFixture(t, ctx)
		settings := contractLegacySettlementTestSettings()
		logf := func(string, ...any) {}
		plan, err := PlanContractLegacySettlement(ctx, settings, logf)
		if err != nil {
			t.Fatal(err)
		}
		// another contract left unsettled since the dry run
		contractId := server.NewId()
		closeTime := server.NowUtc().Add(-30 * 24 * time.Hour)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
				(contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,payer_network_id,create_time,
				close_time,outcome,provider_usage)
				VALUES ($1,$2,$3,$4,$5,1000,$2,$6,$6,'settled','{"version":1,"byte_count":0,"providers":[],"excluded_reason":"expired_unconfirmed"}'::jsonb)`,
				contractId, f.payerNetworkId, f.payerClientId, f.providerNetworkId, f.providerClientId, closeTime))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow (contract_id,balance_id,balance_byte_count) VALUES ($1,$2,0)`,
				contractId, f.expiredBalanceId))
		})
		before := contractLegacySettlementTestState(t, ctx, f)
		if _, settled, err := SettleContractLegacyEscrows(ctx, settings, plan.Token, logf); err == nil || settled != nil {
			t.Fatalf("a stale plan settled: %v", err)
		}
		if after := contractLegacySettlementTestState(t, ctx, f); after != before {
			t.Fatalf("a refused settle wrote:\n%s\nwant\n%s", after, before)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var unsettled int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_escrow WHERE contract_id=$1 AND NOT settled`, contractId).Scan(&unsettled))
			if unsettled != 1 {
				t.Fatalf("the added contract has %d unsettled escrows, want 1", unsettled)
			}
		})
	})
}

// A contract whose category changed after the plan is not written: the settle
// writes a planned contract only while its category under the locks is the
// planned one.
func TestContractLegacySettlementSkipsAContractThatChanged(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		f := newContractLegacySettlementFixture(t, ctx)
		settings := contractLegacySettlementTestSettings()
		plan, err := PlanContractLegacySettlement(ctx, settings, func(string, ...any) {})
		if err != nil {
			t.Fatal(err)
		}
		// the settle contract's sweep appears after the plan
		contractId := f.contractIds[ContractLegacySettlementSettle]
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow_sweep
				(contract_id,balance_id,network_id,payout_byte_count,payout_net_revenue_nano_cents,sweep_time)
				VALUES ($1,$2,$3,600,0,now())`, contractId, f.expiredBalanceId, f.providerNetworkId))
		})
		decision := settleContractLegacyContract(ctx, contractId, plan.contractCategories[contractId], settings)
		if decision.category != ContractLegacySettlementChanged {
			t.Fatalf("category %s, want changed", decision.category)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var unsettled int
			var balance ByteCount
			server.Raise(conn.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM transfer_escrow WHERE contract_id=$1 AND NOT settled),
				(SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2)`, contractId, f.expiredBalanceId).Scan(&unsettled, &balance))
			if unsettled != 1 || balance != contractLegacySettlementTestStart {
				t.Fatalf("a changed contract was written: %d unsettled escrows, balance %d", unsettled, balance)
			}
		})
	})
}

// The root cause: no ordinary path settles an escrow left unsettled under a
// claimed outcome. The deadline close finds the contract already closed and
// writes nothing, and the contract horizon reaper keeps every such contract as
// unsettled. After the settle the reaper holds none of them as unsettled;
// only the swept contract's USDC payment rule still applies to it.
func TestContractLegacySettlementReleasesTheReaperHold(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		f := newContractLegacySettlementFixture(t, ctx)
		writing := []server.Id{}
		for _, category := range append(append([]string{}, contractLegacySettlementWriteCategories...), "settle_redis") {
			writing = append(writing, f.contractIds[category])
		}
		hold := func() (unsettled int64, paymentIncomplete int64, eligible []server.Id) {
			server.Db(ctx, func(conn server.PgConn) {
				var paymentHold, paymentRecent, blockPoints, paymentReview int64
				server.Raise(conn.QueryRow(ctx, contractHorizonHoldSql, writing, server.NowUtc(), []time.Time{}, []time.Time{}).Scan(
					&unsettled, &paymentHold, &paymentIncomplete, &paymentRecent, &blockPoints, &paymentReview, &eligible))
			})
			return
		}

		settleContractId := f.contractIds[ContractLegacySettlementSettle]
		result, err := ReconcileContractAtDeadline(ctx, settleContractId, server.NowUtc())
		if err != nil || result == nil || !result.AlreadyClosed {
			t.Fatalf("the deadline close of a terminal contract: %+v %v", result, err)
		}
		if unsettled, _, eligible := hold(); unsettled != int64(len(writing)) || len(eligible) != 0 {
			t.Fatalf("before: the reaper holds %d of %d as unsettled, %d eligible", unsettled, len(writing), len(eligible))
		}

		settings := contractLegacySettlementTestSettings()
		plan, err := PlanContractLegacySettlement(ctx, settings, func(string, ...any) {})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := SettleContractLegacyEscrows(ctx, settings, plan.Token, func(string, ...any) {}); err != nil {
			t.Fatal(err)
		}
		unsettled, paymentIncomplete, eligible := hold()
		if unsettled != 0 || paymentIncomplete != 1 || len(eligible) != len(writing)-1 {
			t.Fatalf("after: unsettled %d, payment incomplete %d, eligible %d of %d", unsettled, paymentIncomplete, len(eligible), len(writing))
		}
	})
}

// On partitioned contract tables the scope is the legacy partition: its upper
// bound is read from the catalog, and an unsettled contract of a day
// partition is not a candidate.
func TestContractLegacySettlementScopesToTheLegacyPartition(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		cutoverDay := server.ContractPartitionDayStart(server.NowUtc().Add(-3 * 24 * time.Hour))
		server.Testing_PartitionContractsFrom(ctx, cutoverDay)
		f := newContractLegacySettlementFixture(t, ctx)
		dayContractId := ContractIdTimeBound(cutoverDay.Add(time.Hour))
		entropy := server.NewId()
		copy(dayContractId[6:], entropy[6:])
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
				(contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,payer_network_id,create_time,
				close_time,outcome,provider_usage)
				VALUES ($1,$2,$3,$4,$5,1000,$2,$6,$6,'settled','{"version":1,"byte_count":0,"providers":[],"excluded_reason":"expired_unconfirmed"}'::jsonb)`,
				dayContractId, f.payerNetworkId, f.payerClientId, f.providerNetworkId, f.providerClientId, cutoverDay.Add(time.Hour)))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow (contract_id,balance_id,balance_byte_count) VALUES ($1,$2,0)`,
				dayContractId, f.expiredBalanceId))
		})
		plan, err := PlanContractLegacySettlement(ctx, contractLegacySettlementTestSettings(), func(string, ...any) {})
		if err != nil {
			t.Fatal(err)
		}
		if plan.MaxContractId == nil || *plan.MaxContractId != server.ContractIdFloor(cutoverDay) {
			t.Fatalf("bound %v, want the legacy partition's %s", plan.MaxContractId, server.ContractIdFloor(cutoverDay))
		}
		var contracts int64
		for _, category := range ContractLegacySettlementCategories {
			contracts += plan.Categories[category].Contracts
		}
		if _, ok := plan.contractCategories[dayContractId]; ok || contracts != int64(len(f.contractIds)) {
			t.Fatalf("plan read %d contracts (the day partition's included %t), want the %d legacy ones", contracts, ok, len(f.contractIds))
		}
	})
}
