//go:build sidecar_pending

package quotahost

// FS-46 on a Team's own rows (§20.9, FI-13 (d); QUOTA-M6-06) on T3: where QUOTA-M4-03 wrote a pack and a line
// straight to transfer_balance, this network holds what a Team's first paid invoice writes, its team renewal,
// its team_subscription row, the period's team grant and its team_credit line (AddTeamGrantInTx), and a data
// code's balance bought on top. Real contracts, a relayed reservation and a lease draw on them, and after every
// step FI-13 (d)'s E-a is read from the balances, their open escrow and their lease tokens:
//
//	E-a  the grant and the pack pay first, in end-time order, and the line only past them, for a contract's
//	     escrow, a relayed contract's debit and a lease alike
//	E-b  a partial settlement keeps bytes on the grant and the pack and returns the line's reservation first
//	E-e  the provider is paid for the line's bytes at the line's rate
//	E-c  each byte the line paid reaches Stripe once: the day's team_transfer_overage row is the line's bytes,
//	     its meter row is sent once under its identifier, and a rerun of the rollup and of the sender adds none
//	E-d  a lease on the line whose edge is killed leaves no reservation and no debit past its token's lifetime
//
// and escrow_draw_order refuses to turn off while the line is in window. The Stripe here is this file's: it takes
// each claimed row as the sender would and keeps what it was sent by identifier, as Stripe's meter does. The
// checkout and the webhooks that write these rows are controller's, with their own fake Stripe
// (team_checkout_controller_test.go, team_stripe_events_test.go, fs43_stripe_meter_test.go).

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

const (
	// what the team grant, the pack and the line each hold
	teamBillingRoom = model.ByteCount(4096)
	// the meters' unit here, so a few hundred bytes are whole units
	teamBillingMeterUnit = int64(256)
)

// A network's balances with what holds each: open contracts' escrow, and the lease tokens Redis keeps. A
// contract's own token mirrors its escrow, so only lease tokens are added.
type teamBillingOracleSource struct {
	ctx context.Context
}

func (self *teamBillingOracleSource) ReadQuota(ctx context.Context, networkId connect.Id) (*sidecartest.QuotaOracleReading, error) {
	reading := &sidecartest.QuotaOracleReading{}
	server.Db(self.ctx, func(conn server.PgConn) {
		rows, err := conn.Query(
			self.ctx,
			`
				SELECT
					transfer_balance.balance_id,
					transfer_balance.balance_byte_count,
					transfer_balance.grant_kind IS NOT DISTINCT FROM 'team_credit',
					transfer_balance.start_time,
					transfer_balance.end_time,
					COALESCE((
						SELECT SUM(transfer_escrow.balance_byte_count)
						FROM transfer_escrow
						WHERE transfer_escrow.balance_id = transfer_balance.balance_id AND NOT transfer_escrow.settled
					), 0)
				FROM transfer_balance
				WHERE transfer_balance.network_id = $1
				ORDER BY transfer_balance.grant_kind IS NOT DISTINCT FROM 'team_credit', transfer_balance.end_time, transfer_balance.start_time, transfer_balance.balance_id
			`,
			server.Id(networkId),
		)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var balanceId server.Id
				balance := &sidecartest.QuotaBalance{}
				server.Raise(rows.Scan(&balanceId, &balance.ByteCount, &balance.Credit, &balance.StartTime, &balance.EndTime, &balance.ReservedByteCount))
				balance.BalanceId = connect.Id(balanceId)
				reading.Balances = append(reading.Balances, balance)
			}
		})
	})
	for _, balance := range reading.Balances {
		for token, amount := range t3Tokens(self.ctx, server.Id(balance.BalanceId)) {
			if !strings.HasPrefix(token, "lease/") {
				continue
			}
			held, err := strconv.ParseInt(amount, 10, 64)
			if err != nil {
				return nil, err
			}
			balance.ReservedByteCount += held
		}
	}
	return reading, nil
}

// The Stripe of this file: what each identifier was sent, and how often.
type teamBillingStripe struct {
	identifierValues map[string]int64
	identifierMeters map[string]string
	sendCount        int
}

// Sends every row due at now, as the sender does, until none is: each claimed row once, an identifier Stripe
// already holds adding nothing.
func (self *teamBillingStripe) sendDue(t testing.TB, ctx context.Context, now time.Time) {
	t.Helper()
	for range 20 {
		claimed, err := model.ClaimStripeMeterReports(ctx, now, nil, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(claimed.Sends) == 0 {
			return
		}
		for _, send := range claimed.Sends {
			self.sendCount += 1
			if _, held := self.identifierValues[send.Report.Identifier]; !held {
				self.identifierValues[send.Report.Identifier] = send.Report.Value
				self.identifierMeters[send.Report.Identifier] = send.Report.Meter
			}
			if _, err := model.RecordStripeMeterOutcome(ctx, send, model.StripeMeterOutcomeSent, now); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Fatal("rows stayed due after 20 rounds")
}

// What Stripe bills on meter: the sum of what its identifiers hold.
func (self *teamBillingStripe) billed(meter string) (value int64) {
	for identifier, identifierMeter := range self.identifierMeters {
		if identifierMeter == meter {
			value += self.identifierValues[identifier]
		}
	}
	return
}

type teamBillingWorld struct {
	ctx               context.Context
	oracle            *sidecartest.QuotaOracle
	networkId         server.Id
	payerId           server.Id
	relayedId         server.Id
	providerNetworkId server.Id
	providerId        server.Id
	grantId           server.Id
	packId            server.Id
	lineId            server.Id
}

// Inserts active clients, each in its network.
func teamBillingClients(ctx context.Context, clientNetworkIds map[server.Id]server.Id) {
	now := server.NowUtc()
	server.Tx(ctx, func(tx server.PgTx) {
		for clientId, networkId := range clientNetworkIds {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO network_client (client_id, network_id, active, create_time, auth_time)
					VALUES ($1, $2, true, $3, $3)
				`,
				clientId,
				networkId,
				now,
			))
		}
	})
}

// A Team as its first paid invoice leaves it, with a data code's balance on top and a provider to pay. The
// period began yesterday at 00:00 UTC.
func newTeamBillingWorld(t testing.TB, ctx context.Context) *teamBillingWorld {
	t.Helper()
	world := &teamBillingWorld{
		ctx:               ctx,
		networkId:         server.NewId(),
		payerId:           server.NewId(),
		relayedId:         server.NewId(),
		providerNetworkId: server.NewId(),
		providerId:        server.NewId(),
	}
	world.oracle = sidecartest.NewQuotaOracle(&teamBillingOracleSource{ctx: ctx})
	model.Testing_CreateNetwork(ctx, world.networkId, "team-"+world.networkId.String(), server.NewId())
	teamBillingClients(ctx, map[server.Id]server.Id{
		world.payerId:    world.networkId,
		world.relayedId:  world.networkId,
		world.providerId: world.providerNetworkId,
	})
	now := server.NowUtc()
	periodStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1)
	periodEnd := periodStart.AddDate(0, 0, 30)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_team (network_id) VALUES ($1)`, world.networkId))
		server.Raise(model.AddSubscriptionRenewalInTx(tx, ctx, &model.SubscriptionRenewal{
			NetworkId:          world.networkId,
			SubscriptionType:   model.SubscriptionTypeTeam,
			StartTime:          periodStart,
			EndTime:            periodEnd,
			SubscriptionMarket: model.SubscriptionMarketStripe,
		}))
		if !model.AddTeamSubscriptionInTx(ctx, tx, &model.TeamSubscription{
			NetworkId:      world.networkId,
			SubscriptionId: "sub_" + world.networkId.String(),
			CustomerId:     "cus_" + world.networkId.String(),
			AnchorTime:     periodStart,
			StartTime:      periodStart,
		}) {
			t.Fatal("the team_subscription row was not written")
		}
		grant, err := model.AddTeamGrantInTx(ctx, tx, world.networkId, periodStart, periodEnd, 0)
		if err != nil {
			t.Fatal(err)
		}
		if grant.LineBalanceId == nil {
			t.Fatalf("the line was withheld: %s", grant.LineWithheld)
		}
		world.grantId = grant.GrantBalanceId
		world.lineId = *grant.LineBalanceId
	})
	model.UpdateProNetwork(ctx, world.networkId)
	// the data code, bought on top: sold at 3,000,000 nano cents, ending in a year
	model.AddTransferBalance(ctx, &model.TransferBalance{
		NetworkId:             world.networkId,
		StartTime:             now.Add(-time.Hour),
		EndTime:               now.AddDate(1, 0, 0),
		StartBalanceByteCount: teamBillingRoom,
		BalanceByteCount:      teamBillingRoom,
		NetRevenue:            3_000_000,
	})
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT balance_id FROM transfer_balance WHERE network_id = $1 AND balance_id <> $2 AND balance_id <> $3`,
			world.networkId,
			world.grantId,
			world.lineId,
		).Scan(&world.packId))
	})
	return world
}

func (self *teamBillingWorld) checkCreditLast(t testing.TB, what string) {
	t.Helper()
	if err := self.oracle.CheckCreditLast(self.ctx, connect.Id(self.networkId), server.NowUtc()); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

func (self *teamBillingWorld) balance(balanceId server.Id) (byteCount model.ByteCount) {
	server.Db(self.ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(self.ctx, `SELECT balance_byte_count FROM transfer_balance WHERE balance_id = $1`, balanceId).Scan(&byteCount))
	})
	return
}

// Each balance's escrow and payout for the contract.
func (self *teamBillingWorld) escrow(contractId server.Id) (escrowed map[server.Id]model.ByteCount, paid map[server.Id]model.ByteCount) {
	escrowed = map[server.Id]model.ByteCount{}
	paid = map[server.Id]model.ByteCount{}
	server.Db(self.ctx, func(conn server.PgConn) {
		rows, err := conn.Query(self.ctx, `SELECT balance_id, balance_byte_count, COALESCE(payout_byte_count, 0) FROM transfer_escrow WHERE contract_id = $1`, contractId)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var balanceId server.Id
				var escrow, payout model.ByteCount
				server.Raise(rows.Scan(&balanceId, &escrow, &payout))
				escrowed[balanceId] = escrow
				paid[balanceId] = payout
			}
		})
	})
	return
}

// What the provider was paid for the contract's bytes on each balance: net revenue.
func (self *teamBillingWorld) payoutRevenues(contractId server.Id) map[server.Id]model.NanoCents {
	revenues := map[server.Id]model.NanoCents{}
	server.Db(self.ctx, func(conn server.PgConn) {
		rows, err := conn.Query(
			self.ctx,
			`
				SELECT balance_id, SUM(payout_net_revenue_nano_cents)::bigint
				FROM transfer_escrow_sweep
				WHERE contract_id = $1
				GROUP BY balance_id
			`,
			contractId,
		)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var balanceId server.Id
				var revenue model.NanoCents
				server.Raise(rows.Scan(&balanceId, &revenue))
				revenues[balanceId] = revenue
			}
		})
	})
	return revenues
}

// A contract from the Team's client to the provider.
func (self *teamBillingWorld) contract(t testing.TB, byteCount model.ByteCount) server.Id {
	t.Helper()
	transferEscrow, err := model.CreateTransferEscrow(self.ctx, self.networkId, self.payerId, self.providerNetworkId, self.providerId, byteCount)
	if err != nil {
		t.Fatal(err)
	}
	return transferEscrow.ContractId
}

// Both sides close the contract at used.
func (self *teamBillingWorld) close(t testing.TB, contractId server.Id, used model.ByteCount) {
	t.Helper()
	connect.AssertEqual(t, nil, model.CloseContract(self.ctx, contractId, self.payerId, used, false))
	connect.AssertEqual(t, nil, model.CloseContract(self.ctx, contractId, self.providerId, used, false))
	if _, closed := model.GetContractClose(self.ctx, contractId); !closed {
		t.Fatalf("contract %s did not close", contractId)
	}
	teamBillingFlushTransferDebits(t, self.ctx)
}

// The settlement's debits reach the balances, as taskworker's flush applies them.
func teamBillingFlushTransferDebits(t testing.TB, ctx context.Context) {
	t.Helper()
	for shard := range model.TransferDebitShardCount {
		var after *server.Id
		for range 64 {
			flushed, err := model.FlushTransferDebits(ctx, shard, after, 64)
			if err != nil {
				t.Fatal(err)
			}
			if flushed.Failed != 0 || flushed.Busy != 0 {
				t.Fatalf("debit flush of shard %d: %+v", shard, flushed)
			}
			if !flushed.More {
				break
			}
			after = flushed.LastBalanceId
		}
	}
}

// The day's team_transfer_overage row in bytes, and whether the day has one.
func (self *teamBillingWorld) overage(day time.Time) (byteCount int64, written bool) {
	server.Db(self.ctx, func(conn server.PgConn) {
		rows, err := conn.Query(
			self.ctx,
			`SELECT quantity FROM network_service_usage WHERE network_id = $1 AND usage_day = $2 AND meter = $3`,
			self.networkId,
			day,
			model.TeamMeterTransferOverage,
		)
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				server.Raise(rows.Scan(&byteCount))
				written = true
			}
		})
	})
	return
}

func TestTeamBillingFs46T3(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer model.Testing_SetEscrowDrawOrder(true)()
		// a price a few thousand bytes carry in whole nano cents; the restore puts pro's data and the line back
		defer model.Testing_SetTeamPrices(model.Testing_TeamPrices{BaseUsd: 100, SeatUsd: 10, IncludedSeats: 5, TransferTibUsd: 9_000_000})()
		model.Pro().Pro.Data = teamBillingRoom
		model.Pro().Team.CreditLine = teamBillingRoom
		usageSettings := model.DefaultTeamUsageSettings()
		usageSettings.MeterUnitByteCount = teamBillingMeterUnit
		defer model.Testing_SetTeamUsageSettings(usageSettings)()
		connect.AssertEqual(t, model.EscrowDrawOrderEnabled(), true)

		world := newTeamBillingWorld(t, ctx)
		connect.AssertEqual(t, model.NetworkTierOfFresh(ctx, world.networkId), model.NetworkTierTeam)
		connect.AssertEqual(t, world.balance(world.grantId), teamBillingRoom)
		connect.AssertEqual(t, world.balance(world.packId), teamBillingRoom)
		connect.AssertEqual(t, world.balance(world.lineId), teamBillingRoom)
		world.checkCreditLast(t, "a Team as its first invoice leaves it")

		// E-a, a relayed pair of the Team's own clients: its bytes debit the grant, first in draw order
		relayedId, relayedGrant, _, err := model.ReserveRelayedContract(ctx, world.networkId, world.payerId, world.relayedId, 512, 1, true)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, relayedGrant, model.ByteCount(512))
		world.checkCreditLast(t, "E-a, a relayed reservation")
		connect.AssertEqual(t, nil, model.CloseContract(ctx, relayedId, world.payerId, 256, false))
		connect.AssertEqual(t, nil, model.CloseContract(ctx, relayedId, world.relayedId, 256, false))
		if err := t3FlushServiceDebits(ctx, sidecartest.NewFakeClock(server.NowUtc())); err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, world.balance(world.grantId), teamBillingRoom-256)
		connect.AssertEqual(t, world.balance(world.packId), teamBillingRoom)
		connect.AssertEqual(t, world.balance(world.lineId), teamBillingRoom)
		world.checkCreditLast(t, "E-a, a relayed contract's debit")
		grantRoom := teamBillingRoom - 256

		// E-a: a contract within the grant takes neither the pack nor the line
		small := world.contract(t, 3000)
		escrowed, _ := world.escrow(small)
		connect.AssertEqual(t, escrowed[world.grantId], model.ByteCount(3000))
		connect.AssertEqual(t, escrowed[world.packId], model.ByteCount(0))
		connect.AssertEqual(t, escrowed[world.lineId], model.ByteCount(0))
		world.checkCreditLast(t, "E-a, a contract within the grant")

		// E-a: one past the grant's rest takes it, then the pack, then the line
		large := world.contract(t, 6000)
		escrowed, _ = world.escrow(large)
		connect.AssertEqual(t, escrowed[world.grantId], grantRoom-3000)
		connect.AssertEqual(t, escrowed[world.packId], teamBillingRoom)
		lineEscrow := 6000 - (grantRoom - 3000) - teamBillingRoom
		connect.AssertEqual(t, escrowed[world.lineId], lineEscrow)
		world.checkCreditLast(t, "E-a, a contract past the grant and the pack")

		// E-b: the large contract settles all of the grant's and the pack's bytes and 512 of the line's, whose
		// other bytes return first
		const lineFirstDay = model.ByteCount(512)
		world.close(t, large, (grantRoom-3000)+teamBillingRoom+lineFirstDay)
		_, paid := world.escrow(large)
		connect.AssertEqual(t, paid[world.grantId], grantRoom-3000)
		connect.AssertEqual(t, paid[world.packId], teamBillingRoom)
		connect.AssertEqual(t, paid[world.lineId], lineFirstDay)
		connect.AssertEqual(t, world.balance(world.lineId), teamBillingRoom-lineFirstDay)
		connect.AssertEqual(t, world.balance(world.packId), model.ByteCount(0))
		world.checkCreditLast(t, "E-b, settled")

		// E-e: the provider's payout for the line's bytes is priced at the line's rate, whatever the pack's is
		var lineRevenue model.NanoCents
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT net_revenue_nano_cents FROM transfer_balance WHERE balance_id = $1`, world.lineId).Scan(&lineRevenue))
		})
		if lineRevenue <= 0 {
			t.Fatalf("E-e: the line carries no revenue: %d", lineRevenue)
		}
		revenues := world.payoutRevenues(large)
		paidPack := revenues[world.packId]
		if paidPack <= 0 || 3_000_000 < paidPack {
			t.Fatalf("E-e: the pack's bytes paid %d of the 3000000 they were sold for", paidPack)
		}
		// the pack's payout shows the provider's share of a byte's revenue
		wantLine := lineRevenue * model.NanoCents(lineFirstDay) / model.NanoCents(teamBillingRoom) * paidPack / 3_000_000
		if got := revenues[world.lineId]; got < wantLine-wantLine/100-2 || wantLine+wantLine/100+2 < got {
			t.Fatalf("E-e: the line's %d bytes paid %d, want %d at the line's rate", lineFirstDay, got, wantLine)
		}

		// the small contract settles 2000 of its 3000 on the grant, whose rest returns
		world.close(t, small, 2000)
		connect.AssertEqual(t, world.balance(world.grantId), model.ByteCount(1000))
		world.checkCreditLast(t, "E-b, the grant's contract settled")

		// E-c: the day's row is the line's bytes, sent once
		now := server.NowUtc()
		today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		stripe := &teamBillingStripe{identifierValues: map[string]int64{}, identifierMeters: map[string]string{}}
		dayClose := today.AddDate(0, 0, 1).Add(10 * time.Minute)
		rollup, err := model.RollupTeamServiceUsage(ctx, today, nil, 100, dayClose)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, rollup.WrittenCount, 1)
		byteCount, written := world.overage(today)
		connect.AssertEqual(t, written, true)
		connect.AssertEqual(t, byteCount, int64(lineFirstDay))
		stripe.sendDue(t, ctx, dayClose.Add(time.Minute))
		connect.AssertEqual(t, stripe.billed(model.TeamMeterTransferOverage), int64(lineFirstDay)/teamBillingMeterUnit)
		// a rerun of the pass and of the sender adds nothing
		sendCount := stripe.sendCount
		rollup, err = model.RollupTeamServiceUsage(ctx, today, nil, 100, dayClose.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, rollup.WrittenCount, 0)
		stripe.sendDue(t, ctx, dayClose.Add(2*time.Hour))
		connect.AssertEqual(t, stripe.sendCount, sendCount)

		// the next day the line pays 256 more, past the grant's last 1000
		const lineSecondDay = model.ByteCount(256)
		next := world.contract(t, 1000+lineSecondDay)
		escrowed, _ = world.escrow(next)
		connect.AssertEqual(t, escrowed[world.grantId], model.ByteCount(1000))
		connect.AssertEqual(t, escrowed[world.lineId], lineSecondDay)
		world.checkCreditLast(t, "E-a, the second day's contract")
		world.close(t, next, 1000+lineSecondDay)
		tomorrow := today.AddDate(0, 0, 1)
		nextClose := tomorrow.AddDate(0, 0, 1).Add(10 * time.Minute)
		rollup, err = model.RollupTeamServiceUsage(ctx, tomorrow, nil, 100, nextClose)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, rollup.WrittenCount, 1)
		byteCount, written = world.overage(tomorrow)
		connect.AssertEqual(t, written, true)
		connect.AssertEqual(t, byteCount, int64(lineSecondDay))
		stripe.sendDue(t, ctx, nextClose.Add(time.Minute))
		// FI-13 (d) E-c: what Stripe bills is what the line paid, each byte once
		lineUsed := int64(teamBillingRoom - world.balance(world.lineId))
		connect.AssertEqual(t, lineUsed, int64(lineFirstDay+lineSecondDay))
		connect.AssertEqual(t, stripe.billed(model.TeamMeterTransferOverage)*teamBillingMeterUnit, lineUsed)
		for identifier, value := range stripe.identifierValues {
			for _, report := range model.GetStripeMeterReports(ctx, world.networkId, today.AddDate(0, 0, -1)) {
				if report.Identifier == identifier && report.Value != value {
					t.Fatalf("E-c: %s was sent %d and holds %d", identifier, value, report.Value)
				}
			}
		}

		// E-d: every other balance is empty, so an edge leases the line, and is killed: no extend, no carve,
		// no post
		connect.AssertEqual(t, world.balance(world.grantId), model.ByteCount(0))
		lineLeft := world.balance(world.lineId)
		redis := model.NewQuotaRateServerRedis(10 * time.Second)
		token := model.QuotaLeaseToken(server.NewId(), 1)
		reserved, err := model.ReserveLease(ctx, redis, world.lineId, token, lineLeft, 300, time.Minute, 2*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, reserved, model.ByteCount(300))
		world.checkCreditLast(t, "E-d, the lease's draw on the line")
		t3LapseTokens(ctx, world.lineId)
		if tokens := t3Tokens(ctx, world.lineId); len(tokens) != 0 {
			t.Fatalf("E-d: tokens past their lifetime: %v", tokens)
		}
		var journaled int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM service_debit_journal WHERE balance_id = $1`, world.lineId).Scan(&journaled))
		})
		connect.AssertEqual(t, journaled, 0)
		connect.AssertEqual(t, world.balance(world.lineId), lineLeft)
		world.checkCreditLast(t, "E-d, after the lifetime")

		// the gate: a file that says off reads as on while the line is in window
		restoreOff := model.Testing_SetEscrowDrawOrder(false)
		connect.AssertEqual(t, model.EscrowDrawOrderEnabled(), true)
		restoreOff()
	})
}
