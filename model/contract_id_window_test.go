package model

// The contract weeks of the hot reads (contract_id_bound.go): whole partition
// weeks that hold every id the floor of unexpired contract ids admits, with
// the margin for the database clock against this process's on each side.

import (
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

func TestUnexpiredContractWeeksHoldEveryAdmittedIdInWholeWeeks(t *testing.T) {
	// 2026-11-16 is a Monday
	monday := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	week := 7 * 24 * time.Hour
	reach := unexpiredContractIdAge + ContractIdCreateTimeMargin
	ahead := 2 * ContractIdCreateTimeMargin
	times := []time.Time{
		monday,
		monday.Add(time.Millisecond),
		monday.Add(reach - time.Millisecond),
		monday.Add(reach),
		monday.Add(reach + time.Millisecond),
		monday.Add(3*24*time.Hour + 13*time.Hour + 37*time.Minute + 5*time.Second + 987654*time.Microsecond),
		monday.Add(week - ahead - time.Millisecond),
		monday.Add(week - ahead),
		monday.Add(week - time.Millisecond),
	}
	for _, now := range times {
		weeks := unexpiredContractWeeksAt(now)
		lower, upper := server.ContractIdTime(weeks.lower), server.ContractIdTime(weeks.upper)
		for _, bound := range []time.Time{lower, upper} {
			if !server.ContractPartitionWeekStart(bound).Equal(bound) {
				t.Fatalf("at %s: week bound %s is not a partition week's start", now, bound)
			}
		}
		// every id the floor admits on a database clock up to the margin
		// ahead of this one, up to an id minted twice the margin ahead
		if server.ContractIdFloor(now.Add(-reach)).Cmp(weeks.lower) < 0 || server.ContractIdFloor(now.Add(ahead)).Cmp(weeks.upper) >= 0 {
			t.Fatalf("at %s: weeks [%s, %s) do not hold [%s, %s]", now, lower, upper, now.Add(-reach), now.Add(ahead))
		}
		// and no more weeks than that
		if !lower.Equal(server.ContractPartitionWeekStart(now.Add(-reach))) || !upper.Equal(server.ContractPartitionWeekStart(now.Add(ahead)).Add(week)) {
			t.Fatalf("at %s: weeks [%s, %s) are not the fewest that hold the window", now, lower, upper)
		}
	}
	// one week in the middle of a week, two while the reach back crosses into
	// the previous week or the reach ahead into the next
	for _, test := range []struct {
		now   time.Time
		weeks int
	}{
		{now: monday.Add(time.Minute), weeks: 2},
		{now: monday.Add(reach - time.Millisecond), weeks: 2},
		{now: monday.Add(reach), weeks: 1},
		{now: monday.Add(3 * 24 * time.Hour), weeks: 1},
		{now: monday.Add(week - ahead - time.Millisecond), weeks: 1},
		{now: monday.Add(week - ahead), weeks: 2},
	} {
		weeks := unexpiredContractWeeksAt(test.now)
		if span := server.ContractIdTime(weeks.upper).Sub(server.ContractIdTime(weeks.lower)); span != time.Duration(test.weeks)*week {
			t.Fatalf("at %s: weeks span %s, want %d weeks", test.now, span, test.weeks)
		}
	}
}

// The stranded grant read's weeks end with the week of the window's end plus
// the margin.
func TestStrandedGrantEscrowWeeksHoldTheWindowsEnd(t *testing.T) {
	monday := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	week := 7 * 24 * time.Hour
	for _, test := range []struct {
		endedBefore time.Time
		upper       time.Time
	}{
		{endedBefore: monday.Add(2 * 24 * time.Hour), upper: monday.Add(week)},
		{endedBefore: monday.Add(week - ContractIdCreateTimeMargin - time.Millisecond), upper: monday.Add(week)},
		{endedBefore: monday.Add(week - ContractIdCreateTimeMargin), upper: monday.Add(2 * week)},
	} {
		if upper := server.ContractIdTime(strandedGrantEscrowWeeks(test.endedBefore).upper); !upper.Equal(test.upper) {
			t.Fatalf("ended before %s: weeks end %s, want %s", test.endedBefore, upper, test.upper)
		}
	}
}

// Rendering writes both bounds as uuid literals and leaves no placeholder;
// the statement keeps the floor; and its text changes only with the weeks, so
// a cached plan stays for a week.
func TestUnexpiredContractWeeksRender(t *testing.T) {
	monday := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	template := `SELECT 1 FROM transfer_contract WHERE ` + unexpiredContractIdBoundSql("contract_id")
	morning := unexpiredContractWeeksAt(monday.Add(10 * time.Hour))
	rendered := morning.render(template)
	for _, placeholder := range []string{contractWeeksLowerSql, contractWeeksUpperSql} {
		if strings.Contains(rendered, placeholder) {
			t.Fatalf("rendered statement keeps %s: %s", placeholder, rendered)
		}
	}
	for _, part := range []string{
		"contract_id >= " + unexpiredContractIdFloorSql,
		"contract_id >= '" + morning.lower.String() + "'::uuid",
		"contract_id < '" + morning.upper.String() + "'::uuid",
	} {
		if !strings.Contains(rendered, part) {
			t.Fatalf("rendered statement lacks %s: %s", part, rendered)
		}
	}
	if evening := unexpiredContractWeeksAt(monday.Add(22 * time.Hour)); evening.render(template) != rendered {
		t.Fatal("two reads of one week render different statements")
	}
	if next := unexpiredContractWeeksAt(monday.Add(7*24*time.Hour + 10*time.Hour)); next.render(template) == rendered {
		t.Fatal("reads of different weeks render the same statement")
	}
}
