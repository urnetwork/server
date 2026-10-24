// The partition registry's pure rules: canonical bounds and names, key
// literals and their parse back from the catalog, coverage and the creation
// window, expiry, and the validation of declarations. No database is read.
package server

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A daily timestamp family that is never registered.
func testPartitionDailyFamily() *PartitionFamily {
	return &PartitionFamily{
		Name:        "synthetic_daily",
		Tables:      []string{"synthetic_daily_event"},
		KeyColumn:   "observed_at",
		KeyKind:     PartitionKeyTimestamp,
		Granularity: PartitionGranularityDaily,
		AheadRanges: 3,
		Retention:   7 * 24 * time.Hour,
	}
}

// A weekly id family with a legacy partition, never registered.
func testPartitionWeeklyFamily() *PartitionFamily {
	return &PartitionFamily{
		Name:        "synthetic_weekly",
		Tables:      []string{"synthetic_weekly_parent", "synthetic_weekly_child"},
		KeyColumn:   "contract_id",
		KeyKind:     PartitionKeyId,
		Granularity: PartitionGranularityWeekly,
		AheadRanges: 4,
		Retention:   ContractRetention,
	}
}

// Daily ranges start at 00:00 UTC and weekly ones at Monday 00:00 UTC, from
// any instant and any zone, and a range never straddles two partitions.
func TestPartitionGranularityCanonicalBounds(t *testing.T) {
	zone := time.FixedZone("synthetic", -7*60*60)
	for _, test := range []struct {
		granularity PartitionGranularity
		at          time.Time
		start       time.Time
		next        time.Time
	}{
		{PartitionGranularityDaily, time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)},
		{PartitionGranularityDaily, time.Date(2026, 10, 24, 23, 59, 59, 999999999, time.UTC), time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)},
		// 2026-10-24 20:00 at -07:00 is 2026-10-25 03:00 UTC
		{PartitionGranularityDaily, time.Date(2026, 10, 24, 20, 0, 0, 0, zone), time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)},
		{PartitionGranularityDaily, time.Date(2026, 12, 31, 12, 0, 0, 0, time.UTC), time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)},
		// 2026-10-26 is a Monday
		{PartitionGranularityWeekly, time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)},
		{PartitionGranularityWeekly, time.Date(2026, 11, 1, 23, 59, 59, 0, time.UTC), time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)},
		{PartitionGranularityWeekly, time.Date(2026, 10, 25, 23, 0, 0, 0, time.UTC), time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)},
		{PartitionGranularityWeekly, time.Date(2026, 12, 30, 0, 0, 0, 0, time.UTC), time.Date(2026, 12, 28, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 4, 0, 0, 0, 0, time.UTC)},
	} {
		start := test.granularity.RangeStart(test.at)
		if !start.Equal(test.start) || start.Location() != time.UTC {
			t.Errorf("%s range of %s starts %s, want %s", test.granularity, test.at, start, test.start)
		}
		if next := test.granularity.NextRangeStart(start); !next.Equal(test.next) {
			t.Errorf("%s range after %s starts %s, want %s", test.granularity, start, next, test.next)
		}
		r := test.granularity.RangeOf(test.at)
		if !r.Lower.Equal(test.start) || !r.Upper.Equal(test.next) {
			t.Errorf("%s range of %s is %+v", test.granularity, test.at, r)
		}
	}
	// the weekly ranges are the contract weeks
	for at := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC); at.Before(time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC)); at = at.Add(7 * time.Hour) {
		if !PartitionGranularityWeekly.RangeStart(at).Equal(ContractPartitionWeekStart(at)) {
			t.Fatalf("weekly range of %s differs from its contract week", at)
		}
	}
	if got := PartitionGranularityWeekly.AddRanges(time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC), -2); !got.Equal(time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("two weeks back is %s", got)
	}
}

// Leaves are <table>_pYYYYMMDD and <table>_wYYYYMMDD, plus the legacy and
// DEFAULT partitions; only names the registry gives parse back, and a weekly
// name must be a Monday.
func TestPartitionLeafNames(t *testing.T) {
	daily := testPartitionDailyFamily()
	weekly := testPartitionWeeklyFamily()
	day := daily.RangeAt(time.Date(2026, 10, 24, 13, 0, 0, 0, time.UTC))
	if name := daily.LeafName("synthetic_daily_event", day); name != "synthetic_daily_event_p20261024" {
		t.Errorf("daily leaf %s", name)
	}
	week := weekly.RangeAt(time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC))
	if names := weekly.LeafNames(week); !slices.Equal(names, []string{"synthetic_weekly_parent_w20261026", "synthetic_weekly_child_w20261026"}) {
		t.Errorf("weekly leaves %v", names)
	}
	// the contract partitions keep their names
	if name := weekly.LeafName("transfer_contract", week); name != ContractPartitionName("transfer_contract", week.Lower) {
		t.Errorf("weekly leaf %s is not the contract partition's name", name)
	}
	if name := weekly.LeafName("transfer_contract", PartitionRange{Legacy: true}); name != ContractLegacyPartitionName("transfer_contract") {
		t.Errorf("legacy leaf %s", name)
	}
	if name := daily.LeafName("synthetic_daily_event", PartitionRange{Default: true}); name != "synthetic_daily_event_default" {
		t.Errorf("default leaf %s", name)
	}

	r, ok := daily.ParseLeafName("synthetic_daily_event", "synthetic_daily_event_p20261024")
	if !ok || !r.Lower.Equal(day.Lower) || !r.Upper.Equal(day.Upper) {
		t.Errorf("daily parse %+v %t", r, ok)
	}
	r, ok = weekly.ParseLeafName("synthetic_weekly_child", "synthetic_weekly_child_w20261026")
	if !ok || !r.Lower.Equal(week.Lower) || !r.Upper.Equal(week.Upper) {
		t.Errorf("weekly parse %+v %t", r, ok)
	}
	if r, ok := weekly.ParseLeafName("synthetic_weekly_child", "synthetic_weekly_child_legacy"); !ok || !r.Legacy {
		t.Errorf("legacy parse %+v %t", r, ok)
	}
	for _, name := range []string{
		// a Tuesday, a short date, another granularity, another table
		"synthetic_weekly_child_w20261027",
		"synthetic_weekly_child_w2026102",
		"synthetic_weekly_child_p20261026",
		"synthetic_weekly_parent_w20261026x",
		"synthetic_weekly_childx_w20261026",
		// a DEFAULT partition the family does not keep
		"synthetic_weekly_child_default",
	} {
		if _, ok := weekly.ParseLeafName("synthetic_weekly_child", name); ok {
			t.Errorf("%s parsed as a leaf", name)
		}
	}
	keeping := testPartitionDailyFamily()
	keeping.DefaultPartition = true
	if r, ok := keeping.ParseLeafName("synthetic_daily_event", "synthetic_daily_event_default"); !ok || !r.Default {
		t.Errorf("default parse %+v %t", r, ok)
	}
}

// Each key kind writes its bound literal, and reads it back as pg_get_expr
// prints it.
func TestPartitionKeyBounds(t *testing.T) {
	at := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	if literal := PartitionKeyTimestamp.BoundSql(at); literal != "'2026-10-26 00:00:00'" {
		t.Errorf("timestamp literal %s", literal)
	}
	if literal := PartitionKeyId.BoundSql(at); literal != contractIdLiteral(ContractIdFloor(at)) {
		t.Errorf("id literal %s", literal)
	}
	dayBlock := strconv.FormatInt(at.Unix()/60, 10)
	if literal := PartitionKeyEpochMinute.BoundSql(at); literal != dayBlock {
		t.Errorf("epoch minute literal %s, want the day's first block %s", literal, dayBlock)
	}
	for _, test := range []struct {
		kind  PartitionKeyKind
		value string
		want  time.Time
		exact bool
	}{
		{PartitionKeyTimestamp, "2026-10-26 00:00:00", at, true},
		{PartitionKeyTimestamp, "2026-10-26 00:00:00.000000", at, true},
		{PartitionKeyTimestamp, "2026-10-26", at, true},
		{PartitionKeyId, ContractIdFloor(at).String(), at, true},
		{PartitionKeyEpochMinute, dayBlock, at, true},
	} {
		got, exact, ok := test.kind.ParseBound(test.value)
		if !ok || !got.Equal(test.want) || exact != test.exact {
			t.Errorf("%s %s parsed %s exact=%t ok=%t", test.kind, test.value, got, exact, ok)
		}
	}
	// an id inside its millisecond is a bound, but not one the registry makes
	inside := ContractIdFloor(at)
	inside[15] = 1
	if got, exact, ok := PartitionKeyId.ParseBound(inside.String()); !ok || exact || !got.Equal(at) {
		t.Errorf("an id inside its millisecond parsed %s exact=%t ok=%t", got, exact, ok)
	}
	for _, test := range []struct {
		kind  PartitionKeyKind
		value string
	}{
		{PartitionKeyTimestamp, "next tuesday"},
		{PartitionKeyId, "not-a-uuid"},
		{PartitionKeyEpochMinute, "2026-10-26"},
	} {
		if _, _, ok := test.kind.ParseBound(test.value); ok {
			t.Errorf("%s %q parsed", test.kind, test.value)
		}
	}
}

// A listed partition is the registry's own only under its name and bounds; a
// legacy partition reaches MINVALUE, a DEFAULT partition is the family's only
// when it keeps one, and a bound that is not a range of the key is refused.
func TestPartitionParseLeaf(t *testing.T) {
	daily := testPartitionDailyFamily()
	cast := func(value string) *string { return &value }
	day := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	leaf, ok := daily.ParseLeaf(
		"synthetic_daily_event",
		"synthetic_daily_event_p20261024",
		"FOR VALUES FROM ('2026-10-24 00:00:00') TO ('2026-10-25 00:00:00')",
		cast("2026-10-24 00:00:00.000000"),
		cast("2026-10-25 00:00:00.000000"),
	)
	if !ok || !leaf.Canonical || !leaf.Range.Lower.Equal(day) || !leaf.Range.Upper.Equal(day.AddDate(0, 0, 1)) {
		t.Errorf("canonical daily leaf %+v %t", leaf, ok)
	}
	// the right name on two days' bounds is not the registry's
	leaf, ok = daily.ParseLeaf(
		"synthetic_daily_event",
		"synthetic_daily_event_p20261024",
		"FOR VALUES FROM ('2026-10-24 00:00:00') TO ('2026-10-26 00:00:00')",
		cast("2026-10-24 00:00:00.000000"),
		cast("2026-10-26 00:00:00.000000"),
	)
	if !ok || leaf.Canonical {
		t.Errorf("two-day leaf %+v %t", leaf, ok)
	}
	leaf, ok = daily.ParseLeaf("synthetic_daily_event", "synthetic_daily_event_legacy", "FOR VALUES FROM (MINVALUE) TO ('2026-10-24 00:00:00')", nil, cast("2026-10-24 00:00:00.000000"))
	if !ok || !leaf.Canonical || !leaf.Range.Legacy || !leaf.Range.Lower.IsZero() || !leaf.Range.Upper.Equal(day) {
		t.Errorf("legacy leaf %+v %t", leaf, ok)
	}
	if leaf, ok := daily.ParseLeaf("synthetic_daily_event", "synthetic_daily_event_default", "DEFAULT", nil, nil); !ok || leaf.Canonical || !leaf.Range.Default {
		t.Errorf("a default partition the family does not keep %+v %t", leaf, ok)
	}

	weekly := testPartitionWeeklyFamily()
	week := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	leaf, ok = weekly.ParseLeaf(
		"synthetic_weekly_child",
		"synthetic_weekly_child_w20261026",
		"FOR VALUES FROM ('"+ContractIdFloor(week).String()+"') TO ('"+ContractIdFloor(week.AddDate(0, 0, 7)).String()+"')",
		nil,
		nil,
	)
	if !ok || !leaf.Canonical || !leaf.Range.Lower.Equal(week) {
		t.Errorf("canonical weekly leaf %+v %t", leaf, ok)
	}
	inside := ContractIdFloor(week)
	inside[15] = 1
	if leaf, ok := weekly.ParseLeaf("synthetic_weekly_child", "synthetic_weekly_child_w20261026", "FOR VALUES FROM ('"+inside.String()+"') TO ('"+ContractIdFloor(week.AddDate(0, 0, 7)).String()+"')", nil, nil); !ok || leaf.Canonical {
		t.Errorf("a weekly leaf with an inexact bound %+v %t", leaf, ok)
	}
	minutes := &PartitionFamily{Name: "synthetic_minutes", Tables: []string{"synthetic_minutes"}, KeyColumn: "block_number", KeyKind: PartitionKeyEpochMinute, Granularity: PartitionGranularityDaily, AheadRanges: 3}
	firstBlock := week.Unix() / 60
	if leaf, ok := minutes.ParseLeaf("synthetic_minutes", "synthetic_minutes_p20261026", fmt.Sprintf("FOR VALUES FROM (%d) TO (%d)", firstBlock, firstBlock+24*60), nil, nil); !ok || !leaf.Canonical {
		t.Errorf("epoch minute leaf %+v %t", leaf, ok)
	}
	for _, expression := range []string{
		"FOR VALUES IN ('a')",
		"FOR VALUES FROM ('2026-10-25 00:00:00') TO ('2026-10-24 00:00:00')",
		"FOR VALUES WITH (modulus 4, remainder 0)",
	} {
		lower, upper := cast("2026-10-25 00:00:00.000000"), cast("2026-10-24 00:00:00.000000")
		if _, ok := daily.ParseLeaf("synthetic_daily_event", "synthetic_daily_event_other", expression, lower, upper); ok {
			t.Errorf("%s parsed", expression)
		}
	}
}

// The creation window runs from the range holding CreateFrom through the
// ranges ahead; a leaf that covers or overlaps a range keeps it from being
// created; the runway counts consecutive covered ranges after the current one,
// with the legacy partition covering the weeks below its cutover and the
// DEFAULT partition covering nothing.
func TestPartitionCreationWindowAndRunway(t *testing.T) {
	family := testPartitionDailyFamily()
	now := time.Date(2026, 10, 24, 15, 0, 0, 0, time.UTC)
	day := func(offset int) PartitionRange {
		return family.RangeAt(now.AddDate(0, 0, offset))
	}
	leafOf := func(r PartitionRange) PartitionLeaf {
		return PartitionLeaf{Table: "synthetic_daily_event", Name: family.LeafName("synthetic_daily_event", r), Range: r, Canonical: true}
	}
	starts := func(ranges []PartitionRange) (lowers []time.Time) {
		for _, r := range ranges {
			lowers = append(lowers, r.Lower)
		}
		return
	}

	missing := family.MissingRanges(nil, now, now)
	if want := []time.Time{day(0).Lower, day(1).Lower, day(2).Lower, day(3).Lower}; !slices.Equal(starts(missing), want) {
		t.Errorf("no leaves: missing %v, want %v", starts(missing), want)
	}
	// late writes: the window starts at the range holding CreateFrom
	missing = family.MissingRanges(nil, now.Add(-36*time.Hour), now)
	if len(missing) != 5 || !missing[0].Lower.Equal(day(-1).Lower) {
		t.Errorf("late window missing %v", starts(missing))
	}
	// a CreateFrom after now is now
	if missing := family.MissingRanges(nil, now.Add(72*time.Hour), now); len(missing) != 4 {
		t.Errorf("future window missing %v", starts(missing))
	}
	leaves := []PartitionLeaf{leafOf(day(0)), leafOf(day(1)), leafOf(day(2)), leafOf(day(3))}
	if missing := family.MissingRanges(leaves, now, now); len(missing) != 0 {
		t.Errorf("a full window missing %v", starts(missing))
	}
	if covered, future := family.Runway(leaves, now); !covered || future != 3 {
		t.Errorf("full window runway %t %d", covered, future)
	}
	// a gap ends the count
	gapped := []PartitionLeaf{leafOf(day(0)), leafOf(day(1)), leafOf(day(3))}
	if covered, future := family.Runway(gapped, now); !covered || future != 1 {
		t.Errorf("gapped runway %t %d", covered, future)
	}
	if missing := family.MissingRanges(gapped, now, now); !slices.Equal(starts(missing), []time.Time{day(2).Lower}) {
		t.Errorf("gapped window missing %v", starts(missing))
	}
	// a leaf spanning two days covers both; one overlapping a day partly keeps
	// it from being created, though the runway counts it uncovered
	spanning := []PartitionLeaf{leafOf(day(0)), {Table: "synthetic_daily_event", Name: "synthetic_other", Range: PartitionRange{Lower: day(1).Lower, Upper: day(3).Lower}}}
	if covered, future := family.Runway(spanning, now); !covered || future != 2 {
		t.Errorf("spanning runway %t %d", covered, future)
	}
	partial := []PartitionLeaf{leafOf(day(0)), {Table: "synthetic_daily_event", Name: "synthetic_other", Range: PartitionRange{Lower: day(1).Lower.Add(12 * time.Hour), Upper: day(2).Lower}}}
	if missing := family.MissingRanges(partial, now, now); !slices.Equal(starts(missing), []time.Time{day(2).Lower, day(3).Lower}) {
		t.Errorf("partial overlap missing %v", starts(missing))
	}
	if _, future := family.Runway(partial, now); future != 0 {
		t.Errorf("a partly covered day counted, runway %d", future)
	}
	// the DEFAULT partition covers nothing
	withDefault := append(slices.Clone(leaves[:1]), PartitionLeaf{Table: "synthetic_daily_event", Name: "synthetic_daily_event_default", Range: PartitionRange{Default: true}, Canonical: true})
	if _, future := family.Runway(withDefault, now); future != 0 {
		t.Errorf("the default partition covered ranges: %d", future)
	}
	if missing := family.MissingRanges(withDefault, now, now); len(missing) != 3 {
		t.Errorf("the default partition kept ranges from being created: %v", starts(missing))
	}
	// the current range uncovered
	if covered, _ := family.Runway(leaves[1:], now); covered {
		t.Error("an uncovered current range counted covered")
	}

	// the legacy partition covers the weeks below the cutover week
	weekly := testPartitionWeeklyFamily()
	weekNow := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	cutover := weekly.Granularity.AddRanges(weekly.Granularity.RangeStart(weekNow), 2)
	legacy := PartitionLeaf{Table: "synthetic_weekly_parent", Name: "synthetic_weekly_parent_legacy", Range: PartitionRange{Upper: cutover, Legacy: true}, Canonical: true}
	weekLeaves := []PartitionLeaf{legacy}
	for i := 0; i <= 4; i += 1 {
		r := weekly.Granularity.RangeOf(weekly.Granularity.AddRanges(cutover, i))
		weekLeaves = append(weekLeaves, PartitionLeaf{Table: "synthetic_weekly_parent", Name: weekly.LeafName("synthetic_weekly_parent", r), Range: r, Canonical: true})
	}
	if missing := weekly.MissingRanges(weekLeaves, weekNow, weekNow); len(missing) != 0 {
		t.Errorf("weeks below the cutover missing %v", starts(missing))
	}
	if covered, future := weekly.Runway(weekLeaves, weekNow); !covered || future != 6 {
		t.Errorf("legacy runway %t %d", covered, future)
	}
	// an open upper bound ends the count at its limit
	open := []PartitionLeaf{{Table: "synthetic_daily_event", Name: "synthetic_open", Range: PartitionRange{Lower: day(0).Lower}}}
	if covered, future := family.Runway(open, now); !covered || future != partitionRunwayCountLimit {
		t.Errorf("open runway %t %d", covered, future)
	}
}

// A range expires once its upper bound is the retention past, the bound
// itself included; the DEFAULT partition, a kept legacy partition, an open
// bound and a family without retention never expire; a family's own rule
// replaces the horizon.
func TestPartitionExpiry(t *testing.T) {
	ctx := context.Background()
	family := testPartitionDailyFamily()
	r := family.RangeAt(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	cut := r.Upper.Add(family.Retention)
	if family.IsExpired(ctx, cut.Add(-time.Nanosecond), r) {
		t.Error("expired before its upper bound was the retention past")
	}
	if !family.IsExpired(ctx, cut, r) {
		t.Error("not expired at its upper bound plus the retention")
	}
	if family.IsExpired(ctx, cut.AddDate(1, 0, 0), PartitionRange{Default: true}) {
		t.Error("the default partition expired")
	}
	if family.IsExpired(ctx, cut.AddDate(1, 0, 0), PartitionRange{Lower: r.Lower}) {
		t.Error("an open upper bound expired")
	}
	legacy := PartitionRange{Upper: r.Upper, Legacy: true}
	if !family.IsExpired(ctx, cut, legacy) {
		t.Error("the legacy partition did not retire like a range")
	}
	family.KeepLegacyPartition = true
	if family.IsExpired(ctx, cut.AddDate(1, 0, 0), legacy) {
		t.Error("a kept legacy partition expired")
	}
	never := testPartitionDailyFamily()
	never.Retention = 0
	if never.IsExpired(ctx, cut.AddDate(10, 0, 0), r) {
		t.Error("a family without retention dropped a range")
	}
	own := testPartitionDailyFamily()
	own.RangeExpired = func(ctx context.Context, now time.Time, r PartitionRange) bool {
		return r.Upper.Before(now.Add(-time.Hour))
	}
	if !own.IsExpired(ctx, r.Upper.Add(2*time.Hour), r) || own.IsExpired(ctx, r.Upper.Add(30*time.Minute), r) {
		t.Error("the family's own expiry was not used")
	}
}

// Registration validates each declaration and refuses a second family of one
// name or a table two families claim.
func TestPartitionRegistryValidatesDeclarations(t *testing.T) {
	registry := newPartitionRegistry()
	if err := registry.register(testPartitionDailyFamily()); err != nil {
		t.Fatal(err)
	}
	if err := registry.register(testPartitionWeeklyFamily()); err != nil {
		t.Fatal(err)
	}
	if err := registry.register(testPartitionDailyFamily()); err == nil {
		t.Error("a second family of one name registered")
	}
	claiming := testPartitionDailyFamily()
	claiming.Name = "synthetic_claiming"
	if err := registry.register(claiming); err == nil || !strings.Contains(err.Error(), "owns") {
		t.Errorf("a table of another family registered: %v", err)
	}
	if names := registry.list(); len(names) != 2 || names[0].Name != "synthetic_daily" || names[1].Name != "synthetic_weekly" {
		t.Errorf("families listed %v", names)
	}

	for _, test := range []struct {
		name   string
		modify func(family *PartitionFamily)
	}{
		{"an uppercase name", func(family *PartitionFamily) { family.Name = "Synthetic" }},
		{"no tables", func(family *PartitionFamily) { family.Tables = nil }},
		{"a table twice", func(family *PartitionFamily) { family.Tables = []string{"a", "a"} }},
		{"a quoted table", func(family *PartitionFamily) { family.Tables = []string{"a b"} }},
		{"a table too long to name its leaves", func(family *PartitionFamily) { family.Tables = []string{strings.Repeat("a", 60)} }},
		{"no key column", func(family *PartitionFamily) { family.KeyColumn = "" }},
		{"an unknown key kind", func(family *PartitionFamily) { family.KeyKind = "hash" }},
		{"an unknown granularity", func(family *PartitionFamily) { family.Granularity = "hourly" }},
		{"no ranges ahead", func(family *PartitionFamily) { family.AheadRanges = 0 }},
		{"a negative retention", func(family *PartitionFamily) { family.Retention = -time.Hour }},
		{"a runway alarm past the runway", func(family *PartitionFamily) { family.MonitorRunwayRanges = 4 }},
		{"a guard setting without a prefix", func(family *PartitionFamily) { family.DropGuardSetting = "retention" }},
		{"a guard setting with sql", func(family *PartitionFamily) { family.DropGuardSetting = "urnetwork.x = 'on'; DROP" }},
		{"a zero lock timeout", func(family *PartitionFamily) {
			family.Settings = DefaultPartitionFamilySettings()
			family.Settings.DropLockTimeout = 0
		}},
	} {
		family := testPartitionDailyFamily()
		test.modify(family)
		if err := validatePartitionFamily(family); err == nil {
			t.Errorf("%s validated", test.name)
		}
	}
	guarded := testPartitionDailyFamily()
	guarded.DropGuardSetting = "urnetwork.close_evidence_retention"
	if err := validatePartitionFamily(guarded); err != nil {
		t.Errorf("a guard setting refused: %s", err)
	}
}

// Two ranges overlap when they share a key and one covers the other when it
// holds every key of it; MINVALUE and MAXVALUE are open, and the DEFAULT
// partition neither overlaps nor covers.
func TestPartitionRangeOverlapAndCover(t *testing.T) {
	day := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	r := PartitionRange{Lower: day, Upper: day.AddDate(0, 0, 1)}
	for _, test := range []struct {
		name     string
		other    PartitionRange
		overlaps bool
		covers   bool
	}{
		{"itself", r, true, true},
		{"the day after", PartitionRange{Lower: r.Upper, Upper: r.Upper.AddDate(0, 0, 1)}, false, false},
		{"the day before", PartitionRange{Lower: day.AddDate(0, 0, -1), Upper: day}, false, false},
		{"two days", PartitionRange{Lower: day, Upper: day.AddDate(0, 0, 2)}, true, true},
		{"half a day", PartitionRange{Lower: day.Add(12 * time.Hour), Upper: r.Upper}, true, false},
		{"minvalue to the day", PartitionRange{Upper: day, Legacy: true}, false, false},
		{"minvalue through the day", PartitionRange{Upper: r.Upper, Legacy: true}, true, true},
		{"the day to maxvalue", PartitionRange{Lower: day}, true, true},
		{"default", PartitionRange{Default: true}, false, false},
	} {
		if overlaps := test.other.Overlaps(r); overlaps != test.overlaps {
			t.Errorf("%s overlaps=%t", test.name, overlaps)
		}
		if covers := test.other.Covers(r); covers != test.covers {
			t.Errorf("%s covers=%t", test.name, covers)
		}
	}
}
