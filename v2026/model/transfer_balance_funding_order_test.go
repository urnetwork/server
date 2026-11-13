package model

// The funding order (transfer_balance_funding_order.go), without the database:
// a payer's paid Pro grants fund first, and a payer without one keeps the
// earliest-expiry order. Which rows are paid Pro grants is the SQL expression's
// job (escrowTransferBalanceSql), against the database in
// pro_daily_free_grant_db_test.go.

import (
	"math/rand"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

type fundingOrderTestGrant struct {
	name    string
	balance *escrowTransferBalance
}

func newFundingOrderTestGrant(name string, fundsFirst bool, paid bool, startTime time.Time, endTime time.Time) fundingOrderTestGrant {
	return fundingOrderTestGrant{
		name: name,
		balance: &escrowTransferBalance{
			balanceId:        server.NewId(),
			fundsFirst:       fundsFirst,
			paid:             paid,
			balanceByteCount: 1 * Gib,
			startTime:        startTime,
			endTime:          endTime,
		},
	}
}

// The order before paid Pro grants funded first: the earliest end, then the
// earliest start, then the balance id.
func compareFundingOrderTestEarliestExpiry(a *escrowTransferBalance, b *escrowTransferBalance) int {
	if c := a.endTime.Compare(b.endTime); c != 0 {
		return c
	}
	if c := a.startTime.Compare(b.startTime); c != 0 {
		return c
	}
	return a.balanceId.Cmp(b.balanceId)
}

func fundingOrderTestNames(grants []fundingOrderTestGrant, order func(a *escrowTransferBalance, b *escrowTransferBalance) int) []string {
	names := map[server.Id]string{}
	balances := []*escrowTransferBalance{}
	for _, grant := range grants {
		names[grant.balance.balanceId] = grant.name
		balances = append(balances, grant.balance)
	}
	// from a shuffled order, so the result is the comparator's alone
	rand.New(rand.NewSource(1)).Shuffle(len(balances), func(i int, j int) {
		balances[i], balances[j] = balances[j], balances[i]
	})
	slices.SortFunc(balances, order)
	ordered := []string{}
	for _, balance := range balances {
		ordered = append(ordered, names[balance.balanceId])
	}
	return ordered
}

// A Pro network's paid Pro grants fund before its free daily grant and every
// other grant, even though the daily grant ends first; the rest keep the
// earliest-expiry order, paid or not.
func TestTransferBalanceFundingOrderDrawsPaidProGrantsFirst(t *testing.T) {
	now := time.Date(2026, 11, 11, 15, 0, 0, 0, time.UTC)
	dailyStart, dailyEnd := FreeGrantWindow(now)
	grants := []fundingOrderTestGrant{
		newFundingOrderTestGrant("daily free grant", false, false, dailyStart, dailyEnd),
		newFundingOrderTestGrant("paid cycle grant", true, true, now.Add(-24*time.Hour), now.Add(20*24*time.Hour)),
		newFundingOrderTestGrant("legacy calendar grant", true, true, now.Add(-10*24*time.Hour), now.Add(21*24*time.Hour)),
		newFundingOrderTestGrant("600 GiB activation credit", false, true, now.Add(-5*24*time.Hour), now.Add(12*24*time.Hour)),
		newFundingOrderTestGrant("trial cycle grant", false, false, now.Add(-time.Hour), now.Add(5*24*time.Hour)),
		newFundingOrderTestGrant("referral grant", false, false, now.Add(-time.Hour), now.Add(7*24*time.Hour)),
		newFundingOrderTestGrant("x402 month", false, false, now.Add(-time.Hour), now.Add(25*24*time.Hour)),
		newFundingOrderTestGrant("data pack", false, true, now.Add(-time.Hour), now.Add(365*24*time.Hour)),
		newFundingOrderTestGrant("old Pro data code", false, true, now.Add(-200*24*time.Hour), now.Add(100*365*24*time.Hour)),
	}
	got := fundingOrderTestNames(grants, compareTransferBalanceFunding)
	want := []string{
		"paid cycle grant",
		"legacy calendar grant",
		"daily free grant",
		"trial cycle grant",
		"referral grant",
		"600 GiB activation credit",
		"x402 month",
		"data pack",
		"old Pro data code",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("funding order %v, want %v", got, want)
	}
}

// A payer that holds no paid Pro grant funds in exactly the earliest-expiry
// order, ties included. That is every free network (a free network with a data
// pack still spends its daily grant first), a Pro network on an unpaid trial,
// and a network whose Pro comes from an old data code.
func TestTransferBalanceFundingOrderKeepsEarliestExpiryWithoutPaidPro(t *testing.T) {
	// in the hour after midnight, while yesterday's daily grant is in its grace
	now := time.Date(2026, 11, 11, 0, 30, 0, 0, time.UTC)
	dailyStart, dailyEnd := FreeGrantWindow(now)
	tie := now.Add(3 * 24 * time.Hour)
	for _, test := range []struct {
		name   string
		grants []fundingOrderTestGrant
	}{
		{
			name: "a free network",
			grants: []fundingOrderTestGrant{
				newFundingOrderTestGrant("yesterday's daily grant", false, false, dailyStart.Add(-24*time.Hour), dailyStart.Add(FreeGrantGrace)),
				newFundingOrderTestGrant("daily free grant", false, false, dailyStart, dailyEnd),
				newFundingOrderTestGrant("data pack", false, true, now.Add(-time.Hour), now.Add(365*24*time.Hour)),
				newFundingOrderTestGrant("data code", false, true, now.Add(-2*time.Hour), tie),
				newFundingOrderTestGrant("referral grant", false, false, now.Add(-time.Hour), tie),
				newFundingOrderTestGrant("referral grant, same window", false, false, now.Add(-time.Hour), tie),
			},
		},
		{
			name: "a Pro network on an unpaid trial",
			grants: []fundingOrderTestGrant{
				newFundingOrderTestGrant("daily free grant", false, false, dailyStart, dailyEnd),
				newFundingOrderTestGrant("trial cycle grant", false, false, now.Add(-time.Hour), now.Add(7*24*time.Hour)),
				newFundingOrderTestGrant("data pack", false, true, now.Add(-time.Hour), now.Add(365*24*time.Hour)),
			},
		},
		{
			name: "a network whose Pro comes from an old data code",
			grants: []fundingOrderTestGrant{
				newFundingOrderTestGrant("daily free grant", false, false, dailyStart, dailyEnd),
				newFundingOrderTestGrant("old Pro data code", false, true, now.Add(-200*24*time.Hour), now.Add(100*365*24*time.Hour)),
			},
		},
	} {
		got := fundingOrderTestNames(test.grants, compareTransferBalanceFunding)
		want := fundingOrderTestNames(test.grants, compareFundingOrderTestEarliestExpiry)
		if !slices.Equal(got, want) {
			t.Errorf("%s: funding order %v, want the earliest-expiry order %v", test.name, got, want)
		}
		if test.name == "a free network" && got[0] != "yesterday's daily grant" {
			t.Errorf("%s: %s funds first, want the daily grant that ends first", test.name, got[0])
		}
	}
}

// The Redis census reads one earliest-expiry pass, with the statements it had
// before, for a payer that is not Pro, and two complementary phases for a Pro
// payer.
func TestTransferBalanceFundingPhases(t *testing.T) {
	if got := transferBalanceFundingPhases(false); !slices.Equal(got, []string{""}) {
		t.Fatalf("phases for a payer that is not Pro %q, want the one unfiltered census", got)
	}
	got := transferBalanceFundingPhases(true)
	if !slices.Equal(got, []string{transferBalanceFundsFirstPhaseSql, transferBalanceFundsLaterPhaseSql}) {
		t.Fatalf("phases for a Pro payer %q, want paid Pro grants, then the rest", got)
	}
	if transferBalanceFundsLaterPhaseSql != " AND NOT "+transferBalanceFundsFirstSql {
		t.Fatalf("the second phase %q is not the first phase's complement", transferBalanceFundsLaterPhaseSql)
	}
	// a NULL grant kind (every balance that is not a recurring grant) must
	// fall in exactly one phase, so the expression may not compare with =
	if !strings.Contains(transferBalanceFundsFirstSql, "grant_kind IS NOT DISTINCT FROM 'pro'") {
		t.Fatalf("the first group %q can be NULL for a balance with no grant kind", transferBalanceFundsFirstSql)
	}
	if want := "(b.pro AND b.paid AND b.grant_kind IS NOT DISTINCT FROM 'pro')"; transferBalanceFundsFirstSqlOf("b") != want {
		t.Fatalf("qualified first group %q, want %q", transferBalanceFundsFirstSqlOf("b"), want)
	}
}
