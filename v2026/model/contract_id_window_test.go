package model

// The contract days of the hot reads (contract_id_bound.go): whole partition
// days that hold every id the floor of unexpired contract ids admits, with
// the margin for the database clock against this process's on each side.

import (
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func TestUnexpiredContractDaysHoldEveryAdmittedIdInWholeDays(t *testing.T) {
	midnight := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	reach := unexpiredContractIdAge + ContractIdCreateTimeMargin
	ahead := 2 * ContractIdCreateTimeMargin
	if reach != 90*time.Minute || ahead != 30*time.Minute {
		t.Fatalf("the window reaches back %s and ahead %s", reach, ahead)
	}
	times := []time.Time{
		midnight,
		midnight.Add(time.Millisecond),
		midnight.Add(reach - time.Millisecond),
		midnight.Add(reach),
		midnight.Add(reach + time.Millisecond),
		midnight.Add(13*time.Hour + 37*time.Minute + 5*time.Second + 987654*time.Microsecond),
		midnight.Add(day - ahead - time.Millisecond),
		midnight.Add(day - ahead),
		midnight.Add(day - time.Millisecond),
	}
	for _, now := range times {
		days := unexpiredContractDaysAt(now)
		lower, upper := server.ContractIdTime(days.lower), server.ContractIdTime(days.upper)
		for _, bound := range []time.Time{lower, upper} {
			if !server.ContractPartitionDayStart(bound).Equal(bound) {
				t.Fatalf("at %s: day bound %s is not a partition day's start", now, bound)
			}
		}
		// every id the floor admits on a database clock up to the margin
		// ahead of this one, up to an id minted twice the margin ahead
		if server.ContractIdFloor(now.Add(-reach)).Cmp(days.lower) < 0 || server.ContractIdFloor(now.Add(ahead)).Cmp(days.upper) >= 0 {
			t.Fatalf("at %s: days [%s, %s) do not hold [%s, %s]", now, lower, upper, now.Add(-reach), now.Add(ahead))
		}
		// and no more days than that
		if !lower.Equal(server.ContractPartitionDayStart(now.Add(-reach))) || !upper.Equal(server.ContractPartitionDayStart(now.Add(ahead)).Add(day)) {
			t.Fatalf("at %s: days [%s, %s) are not the fewest that hold the window", now, lower, upper)
		}
	}
	// one day, the partition of the current day, but while the reach back
	// crosses into the previous day (00:00 to 01:30 UTC) or the reach ahead
	// into the next (23:30 to 24:00 UTC)
	for _, test := range []struct {
		now  time.Time
		days int
	}{
		{now: midnight.Add(time.Minute), days: 2},
		{now: midnight.Add(reach - time.Millisecond), days: 2},
		{now: midnight.Add(reach), days: 1},
		{now: midnight.Add(12 * time.Hour), days: 1},
		{now: midnight.Add(day - ahead - time.Millisecond), days: 1},
		{now: midnight.Add(day - ahead), days: 2},
	} {
		days := unexpiredContractDaysAt(test.now)
		if span := server.ContractIdTime(days.upper).Sub(server.ContractIdTime(days.lower)); span != time.Duration(test.days)*day {
			t.Fatalf("at %s: days span %s, want %d days", test.now, span, test.days)
		}
	}
}

// The stranded grant read's days end with the day of the window's end plus
// the margin.
func TestStrandedGrantEscrowDaysHoldTheWindowsEnd(t *testing.T) {
	midnight := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	for _, test := range []struct {
		endedBefore time.Time
		upper       time.Time
	}{
		{endedBefore: midnight.Add(2 * time.Hour), upper: midnight.Add(day)},
		{endedBefore: midnight.Add(day - ContractIdCreateTimeMargin - time.Millisecond), upper: midnight.Add(day)},
		{endedBefore: midnight.Add(day - ContractIdCreateTimeMargin), upper: midnight.Add(2 * day)},
	} {
		if upper := server.ContractIdTime(strandedGrantEscrowDays(test.endedBefore).upper); !upper.Equal(test.upper) {
			t.Fatalf("ended before %s: days end %s, want %s", test.endedBefore, upper, test.upper)
		}
	}
}

// Rendering writes both bounds as uuid literals and leaves no placeholder;
// the statement keeps the floor; and its text changes only when a day boundary
// enters or leaves the window: twice a day, at 23:30 and at 01:30 UTC, so each
// connection plans it afresh twice a day (weekly partitions changed it twice a
// week, Sunday 23:30 and Monday 01:30).
func TestUnexpiredContractDaysRender(t *testing.T) {
	midnight := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	template := `SELECT 1 FROM transfer_contract WHERE ` + unexpiredContractIdBoundSql("contract_id")
	morning := unexpiredContractDaysAt(midnight.Add(10 * time.Hour))
	rendered := morning.render(template)
	for _, placeholder := range []string{contractDaysLowerSql, contractDaysUpperSql} {
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
	if evening := unexpiredContractDaysAt(midnight.Add(22 * time.Hour)); evening.render(template) != rendered {
		t.Fatal("two reads of one day render different statements")
	}
	if next := unexpiredContractDaysAt(midnight.Add(24*time.Hour + 10*time.Hour)); next.render(template) == rendered {
		t.Fatal("reads of different days render the same statement")
	}

	// over three days, a statement every second: the text changes at 23:30
	// and at 01:30 UTC, and at no other time
	changes := []time.Time{}
	previous := ""
	for at := midnight.Add(2 * time.Hour); at.Before(midnight.Add(74 * time.Hour)); at = at.Add(time.Second) {
		sql := unexpiredContractDaysAt(at).render(template)
		if previous != "" && sql != previous {
			changes = append(changes, at)
		}
		previous = sql
	}
	want := []time.Time{}
	for day := 0; day < 3; day += 1 {
		dayStart := midnight.AddDate(0, 0, day)
		want = append(want, dayStart.Add(23*time.Hour+30*time.Minute), dayStart.AddDate(0, 0, 1).Add(90*time.Minute))
	}
	if len(changes) != len(want) {
		t.Fatalf("the text changed at %v, want %v", changes, want)
	}
	for i := range want {
		if !changes[i].Equal(want[i]) {
			t.Fatalf("the text changed at %v, want %v", changes, want)
		}
	}
}
