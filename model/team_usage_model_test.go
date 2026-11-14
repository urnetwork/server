//go:build sidecar_pending

package model

// QUOTA-M6-03: a Team's seats and its daily rows, under -tags sidecar_pending, since network_member,
// team_subscription and stripe_meter_report are MG6's. The line's debits are written here as each
// path leaves them, a lower balance_byte_count, since the row reads nothing else of a path; the four
// paths themselves taking a line's bytes on real rows is QUOTA-M6-06's run of FS-46.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

func testingAddTeamMember(t testing.TB, ctx context.Context, networkId server.Id, role NetworkMemberRole) *NetworkMember {
	t.Helper()
	var member *NetworkMember
	server.Tx(ctx, func(tx server.PgTx) {
		var err error
		member, err = AddNetworkMemberInTx(ctx, tx, &AddNetworkMemberArgs{
			NetworkId: networkId,
			UserId:    server.NewId(),
			Role:      role,
		})
		if err != nil {
			t.Fatal(err)
		}
	})
	return member
}

// A client of the network that last signed in at authTime, owned by the member when memberId is set.
func testingAddTeamClient(t testing.TB, ctx context.Context, networkId server.Id, memberId *server.Id, authTime time.Time) server.Id {
	t.Helper()
	clientId := server.NewId()
	Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "synthetic", "synthetic")
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET auth_time = $2 WHERE client_id = $1`, clientId, authTime.UTC()))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_owner (client_id, member_id, tagged) VALUES ($1, $2, $3)`, clientId, memberId, memberId == nil))
	})
	return clientId
}

// A credit line over [start, end) of byteCount bytes, as AddTeamGrantInTx writes one.
func testingAddTeamCreditLine(t testing.TB, ctx context.Context, networkId server.Id, start time.Time, end time.Time, byteCount ByteCount) server.Id {
	t.Helper()
	balanceId := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO transfer_balance (
					balance_id,
					network_id,
					start_time,
					end_time,
					start_balance_byte_count,
					net_revenue_nano_cents,
					subsidy_net_revenue_nano_cents,
					balance_byte_count,
					pro,
					grant_kind
				)
				VALUES ($1, $2, $3, $4, $5, $6, 0, $5, false, $7)
			`,
			balanceId,
			networkId,
			start.UTC(),
			end.UTC(),
			byteCount,
			UsdToNanoCents(30),
			GrantKindTeamCredit,
		))
	})
	return balanceId
}

// Takes byteCount from the balance, as a debit path does.
func testingDebitBalance(t testing.TB, ctx context.Context, balanceId server.Id, byteCount ByteCount) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE transfer_balance SET balance_byte_count = balance_byte_count - $2 WHERE balance_id = $1 AND $2 <= balance_byte_count`,
			balanceId,
			byteCount,
		))
		connect.AssertEqual(t, tag.RowsAffected(), int64(1))
	})
}

func testingMeterValues(ctx context.Context, networkId server.Id, day time.Time) map[string][]int64 {
	values := map[string][]int64{}
	for _, report := range GetStripeMeterReports(ctx, networkId, day) {
		if report.UsageDay.Equal(networkUsageDay(day)) {
			values[report.Meter] = append(values[report.Meter], report.Value)
		}
	}
	return values
}

// §20.5's Seats: a member is a seat when any of its clients signed in within 30 days; an inactive
// member, an Auditor, a Billing admin and a member with no client are none, and an agent, which is no
// member, adds none.
func TestCountTeamSeats(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		at := time.Date(2026, time.November, 14, 0, 0, 0, 0, time.UTC)
		networkId := testingCreateTeamNetwork(t, ctx)
		connect.AssertEqual(t, CountTeamSeats(ctx, networkId, at), int64(0))

		// active on one device of three
		oneOfThree := testingAddTeamMember(t, ctx, networkId, NetworkMemberRoleMember)
		testingAddTeamClient(t, ctx, networkId, &oneOfThree.MemberId, at.AddDate(0, 0, -5))
		testingAddTeamClient(t, ctx, networkId, &oneOfThree.MemberId, at.AddDate(0, 0, -40))
		testingAddTeamClient(t, ctx, networkId, &oneOfThree.MemberId, at.AddDate(0, 0, -90))
		connect.AssertEqual(t, CountTeamSeats(ctx, networkId, at), int64(1))

		// last signed in 31 days ago
		inactive := testingAddTeamMember(t, ctx, networkId, NetworkMemberRoleMember)
		testingAddTeamClient(t, ctx, networkId, &inactive.MemberId, at.AddDate(0, 0, -31))
		// the free roles, each active
		auditor := testingAddTeamMember(t, ctx, networkId, NetworkMemberRoleAuditor)
		testingAddTeamClient(t, ctx, networkId, &auditor.MemberId, at.Add(-time.Hour))
		billingAdmin := testingAddTeamMember(t, ctx, networkId, NetworkMemberRoleBillingAdmin)
		testingAddTeamClient(t, ctx, networkId, &billingAdmin.MemberId, at.Add(-time.Hour))
		// an agent: a tagged client no member owns
		testingAddTeamClient(t, ctx, networkId, nil, at.Add(-time.Hour))
		// a member that never enrolled a device
		testingAddTeamMember(t, ctx, networkId, NetworkMemberRoleMember)
		connect.AssertEqual(t, CountTeamSeats(ctx, networkId, at), int64(1))

		// an admin, active exactly at the window's edge, and an owner
		admin := testingAddTeamMember(t, ctx, networkId, NetworkMemberRoleAdmin)
		testingAddTeamClient(t, ctx, networkId, &admin.MemberId, at.AddDate(0, 0, -30).Add(time.Second))
		owner := testingAddTeamMember(t, ctx, networkId, NetworkMemberRoleOwner)
		testingAddTeamClient(t, ctx, networkId, &owner.MemberId, at)
		connect.AssertEqual(t, CountTeamSeats(ctx, networkId, at), int64(3))

		// a sign-in after at is not yet in a count at at, and the window moves with at
		connect.AssertEqual(t, CountTeamSeats(ctx, networkId, at.Add(-time.Second)), int64(2))
		connect.AssertEqual(t, CountTeamSeats(ctx, networkId, at.AddDate(0, 0, 26)), int64(1))

		// another network's members are not this one's
		other := testingCreateTeamNetwork(t, ctx)
		connect.AssertEqual(t, CountTeamSeats(ctx, other, at), int64(0))
	})
}

// E-c: a day's transfer row is the bytes the line paid that the pass first sees that day, each once,
// across a period boundary at 00:00 UTC, a pass killed before its commit and a rerun; the Stripe
// values are whole GiB whose sum never drifts from the running bytes.
func TestTeamTransferOverageCountsEachLineByteOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer testingSetTeamProConfig()()
		gib := int64(Gib)
		day1 := time.Date(2026, time.November, 10, 0, 0, 0, 0, time.UTC)
		day2 := day1.AddDate(0, 0, 1)
		day3 := day1.AddDate(0, 0, 2)
		day4 := day1.AddDate(0, 0, 3)
		pass := func(day time.Time) time.Time {
			return day.AddDate(0, 0, 1).Add(10 * time.Minute)
		}

		networkId := testingCreateTeamNetwork(t, ctx)
		testingAddTeamSubscription(t, ctx, networkId, day1.AddDate(0, 0, -28), day1.AddDate(0, 0, -28))
		// the first period's line ends a grace day past the boundary at day3's start, where the
		// second period's begins
		line1 := testingAddTeamCreditLine(t, ctx, networkId, day1.AddDate(0, 0, -28), day3.Add(ProGrantGrace), 10*Tib)
		// a network that is no Team, and a Team whose subscription ended before day1: no rows
		plain := server.NewId()
		Testing_CreateNetwork(ctx, plain, "plain-"+plain.String(), server.NewId())
		ended := testingCreateTeamNetwork(t, ctx)
		endedSubscriptionId := testingAddTeamSubscription(t, ctx, ended, day1.AddDate(0, 0, -60), day1.AddDate(0, 0, -60))
		server.Tx(ctx, func(tx server.PgTx) {
			connect.AssertEqual(t, EndTeamSubscriptionRowInTx(ctx, tx, ended, endedSubscriptionId, day1.Add(-time.Hour)), true)
		})
		testingAddTeamCreditLine(t, ctx, ended, day1.AddDate(0, 0, -28), day3, 10*Tib)

		quantity := func(day time.Time, meter string) int64 {
			t.Helper()
			quantity, found := testingUsageQuantity(ctx, networkId, day, meter)
			connect.AssertEqual(t, found, true)
			return quantity
		}

		// day1: 1.5 GiB off the line
		testingDebitBalance(t, ctx, line1, 3*gib/2)

		// a pass killed inside the Team's transaction writes nothing
		killed := errors.New("killed")
		_, err := rollupTeamServiceUsage(ctx, day1, nil, 10, pass(day1), &teamUsageRollupHooks{
			beforeCommit: func(id server.Id) error {
				if id == networkId {
					return killed
				}
				return nil
			},
		})
		connect.AssertEqual(t, errors.Is(err, killed), true)
		_, found := testingUsageQuantity(ctx, networkId, day1, TeamMeterSeats)
		connect.AssertEqual(t, found, false)
		connect.AssertEqual(t, len(GetStripeMeterReports(ctx, networkId, day1)), 0)

		// the rerun writes the day once
		result, err := RollupTeamServiceUsage(ctx, day1, nil, 10, pass(day1))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.LookedCount, 1)
		connect.AssertEqual(t, result.WrittenCount, 1)
		connect.AssertEqual(t, result.LastNetworkId == nil, true)
		connect.AssertEqual(t, quantity(day1, TeamMeterTransferOverage), 3*gib/2)
		connect.AssertEqual(t, quantity(day1, TeamMeterCreditWindow), 3*gib/2)
		connect.AssertEqual(t, quantity(day1, TeamMeterCreditRunning), 3*gib/2)
		connect.AssertEqual(t, quantity(day1, TeamMeterSeats), int64(0))
		// storage is unpriced: no row and no meter value
		_, found = testingUsageQuantity(ctx, networkId, day1, TeamMeterStorageOverage)
		connect.AssertEqual(t, found, false)
		connect.AssertEqual(t, testingMeterValues(ctx, networkId, day1), map[string][]int64{
			TeamMeterSeats:           {0},
			TeamMeterTransferOverage: {2},
		})

		// a late debit after the day was written never rewrites it: a second run of day1 writes
		// nothing, and the bytes land in day2
		testingDebitBalance(t, ctx, line1, gib/4)
		result, err = RollupTeamServiceUsage(ctx, day1, nil, 10, pass(day1).Add(time.Hour))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.LookedCount, 1)
		connect.AssertEqual(t, result.WrittenCount, 0)
		connect.AssertEqual(t, quantity(day1, TeamMeterTransferOverage), 3*gib/2)
		connect.AssertEqual(t, len(GetStripeMeterReports(ctx, networkId, day1)), 2)

		result, err = RollupTeamServiceUsage(ctx, day2, nil, 10, pass(day2))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.WrittenCount, 1)
		connect.AssertEqual(t, quantity(day2, TeamMeterTransferOverage), gib/4)
		connect.AssertEqual(t, quantity(day2, TeamMeterCreditRunning), 3*gib/2+gib/4)
		// round(1.75) - round(1.5) = 0
		connect.AssertEqual(t, testingMeterValues(ctx, networkId, day2)[TeamMeterTransferOverage], []int64{0})

		// day3 starts the second period at 00:00 UTC: its line takes 1 GiB while a contract of the
		// first period settles 0.25 GiB late on the first line
		line2 := testingAddTeamCreditLine(t, ctx, networkId, day3, day3.AddDate(0, 0, 30).Add(ProGrantGrace), 10*Tib)
		testingDebitBalance(t, ctx, line1, gib/4)
		testingDebitBalance(t, ctx, line2, gib)
		result, err = RollupTeamServiceUsage(ctx, day3, nil, 10, pass(day3))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.WrittenCount, 1)
		connect.AssertEqual(t, quantity(day3, TeamMeterTransferOverage), gib/4+gib)
		connect.AssertEqual(t, quantity(day3, TeamMeterCreditRunning), 3*gib)
		// round(3.0) - round(1.75) = 1
		connect.AssertEqual(t, testingMeterValues(ctx, networkId, day3)[TeamMeterTransferOverage], []int64{1})

		// day4: nothing moved
		result, err = RollupTeamServiceUsage(ctx, day4, nil, 10, pass(day4))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, quantity(day4, TeamMeterTransferOverage), int64(0))
		connect.AssertEqual(t, testingMeterValues(ctx, networkId, day4)[TeamMeterTransferOverage], []int64{0})

		// the rows sum to the bytes the lines paid, each once, and the values to their whole GiB
		byteSum := int64(0)
		valueSum := int64(0)
		for _, day := range []time.Time{day1, day2, day3, day4} {
			byteSum += quantity(day, TeamMeterTransferOverage)
			valueSum += testingMeterValues(ctx, networkId, day)[TeamMeterTransferOverage][0]
		}
		connect.AssertEqual(t, byteSum, 3*gib)
		connect.AssertEqual(t, valueSum, int64(3))

		// neither the plain network nor the ended Team got a row
		for _, id := range []server.Id{plain, ended} {
			_, found := testingUsageQuantity(ctx, id, day1, TeamMeterSeats)
			connect.AssertEqual(t, found, false)
			connect.AssertEqual(t, len(GetStripeMeterReports(ctx, id, day1)), 0)
		}
	})
}

// A pass pages by network and resumes from its cursor: a page of one names the Team it stopped at,
// and the pages together write each Team's day once.
func TestRollupTeamServiceUsagePagesByNetwork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer testingSetTeamProConfig()()
		day := time.Date(2026, time.November, 10, 0, 0, 0, 0, time.UTC)
		now := day.AddDate(0, 0, 1).Add(10 * time.Minute)
		networkIds := map[server.Id]bool{}
		for range 3 {
			networkId := testingCreateTeamNetwork(t, ctx)
			testingAddTeamSubscription(t, ctx, networkId, day.AddDate(0, 0, -3), day.AddDate(0, 0, -3))
			networkIds[networkId] = true
		}
		_, err := RollupTeamServiceUsage(ctx, day, nil, 0, now)
		connect.AssertEqual(t, err != nil, true)

		var after *server.Id
		writtenCount := 0
		pageCount := 0
		for {
			result, err := RollupTeamServiceUsage(ctx, day, after, 1, now)
			connect.AssertEqual(t, err, nil)
			writtenCount += result.WrittenCount
			pageCount += 1
			if result.LastNetworkId == nil {
				break
			}
			connect.AssertEqual(t, networkIds[*result.LastNetworkId], true)
			after = result.LastNetworkId
			if 10 < pageCount {
				t.Fatal("the pass does not end")
			}
		}
		connect.AssertEqual(t, writtenCount, 3)
		for networkId := range networkIds {
			_, found := testingUsageQuantity(ctx, networkId, day, TeamMeterSeats)
			connect.AssertEqual(t, found, true)
		}
	})
}

// Storage overage is the day's stored bytes past the included limit, in whole GiB-days: none at the
// limit exactly, and none at all while storage_gib_month is unset.
func TestTeamStorageOverageAtTheLimit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer testingSetTeamProConfig()()
		limit := int64(200) * int64(Gib)
		defer Testing_SetSidecarLimits(NetworkTierTeam, map[string]int64{"storage": limit})()
		c := Pro()
		price := c.Team.overageStorageGibMonthUsd
		defer func() {
			c.Team.overageStorageGibMonthUsd = price
		}()

		now := server.NowUtc()
		day := networkUsageDay(now).AddDate(0, 0, -1)
		start, end := testingTeamPeriod()
		pass := day.AddDate(0, 0, 1).Add(10 * time.Minute)
		newTeam := func(storedByteCount int64) server.Id {
			networkId := testingCreateTeamNetwork(t, ctx)
			testingAddTeamGrant(t, ctx, networkId, start, end)
			connect.AssertEqual(t, NetworkTierOf(ctx, networkId), NetworkTierTeam)
			testingAddTeamSubscription(t, ctx, networkId, start, start)
			testingInsertUsageDay(ctx, networkId, day, NetworkUsageStorageMeter, storedByteCount)
			return networkId
		}

		// unpriced: no storage row and no meter value, whatever is stored
		c.Team.overageStorageGibMonthUsd = 0
		unpriced := newTeam(limit + 50*int64(Gib))
		_, err := RollupTeamServiceUsage(ctx, day, nil, 100, pass)
		connect.AssertEqual(t, err, nil)
		_, found := testingUsageQuantity(ctx, unpriced, day, TeamMeterStorageOverage)
		connect.AssertEqual(t, found, false)
		_, found = testingMeterValues(ctx, unpriced, day)[TeamMeterStorageOverage]
		connect.AssertEqual(t, found, false)

		c.Team.overageStorageGibMonthUsd = 0.02
		for _, vector := range []struct {
			storedByteCount int64
			overage         int64
			value           int64
		}{
			{limit - 1, 0, 0},
			{limit, 0, 0},
			{limit + 1, 1, 0},
			{limit + int64(Gib)/2 - 1, int64(Gib)/2 - 1, 0},
			{limit + int64(Gib)/2, int64(Gib) / 2, 1},
			{limit + 3*int64(Gib), 3 * int64(Gib), 3},
		} {
			networkId := newTeam(vector.storedByteCount)
			_, err := RollupTeamServiceUsage(ctx, day, nil, 100, pass)
			connect.AssertEqual(t, err, nil)
			overage, found := testingUsageQuantity(ctx, networkId, day, TeamMeterStorageOverage)
			connect.AssertEqual(t, found, true)
			connect.AssertEqual(t, overage, vector.overage)
			connect.AssertEqual(t, testingMeterValues(ctx, networkId, day)[TeamMeterStorageOverage], []int64{vector.value})
		}
		// the unpriced Team's day was written once and stays as it was
		_, found = testingUsageQuantity(ctx, unpriced, day, TeamMeterStorageOverage)
		connect.AssertEqual(t, found, false)
	})
}
