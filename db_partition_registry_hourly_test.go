// The registry's hourly granularity (TLS-M5-12), which §19.7 gives
// tls_connection_record and mail_connection_record: canonical hours, their
// leaves' names and the parse back, the creation window across a UTC day, and
// the runway. Pure, as db_partition_registry_test.go is; model's tests create
// and drop real hourly leaves.
package server

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// An hourly timestamp family that is never registered.
func testPartitionHourlyFamily() *PartitionFamily {
	return &PartitionFamily{
		Name:        "synthetic_hourly",
		Tables:      []string{"synthetic_hourly_record"},
		KeyColumn:   "end_time",
		KeyKind:     PartitionKeyTimestamp,
		Granularity: PartitionGranularityHourly,
		AheadRanges: 6,
		Retention:   23 * time.Hour,
	}
}

// An hourly range starts on the UTC hour from any instant and any zone, holds
// one hour, and never straddles two days.
func TestPartitionGranularityHourlyCanonicalBounds(t *testing.T) {
	zone := time.FixedZone("synthetic", -7*60*60+30*60)
	for _, test := range []struct {
		at    time.Time
		start time.Time
	}{
		{time.Date(2026, 11, 8, 13, 0, 0, 0, time.UTC), time.Date(2026, 11, 8, 13, 0, 0, 0, time.UTC)},
		{time.Date(2026, 11, 8, 13, 59, 59, 999999999, time.UTC), time.Date(2026, 11, 8, 13, 0, 0, 0, time.UTC)},
		{time.Date(2026, 12, 31, 23, 30, 0, 0, time.UTC), time.Date(2026, 12, 31, 23, 0, 0, 0, time.UTC)},
		// 2026-11-08 20:10 at -06:30 is 2026-11-09 02:40 UTC: a zone off the hour
		// still lands on the UTC hour
		{time.Date(2026, 11, 8, 20, 10, 0, 0, zone), time.Date(2026, 11, 9, 2, 0, 0, 0, time.UTC)},
	} {
		start := PartitionGranularityHourly.RangeStart(test.at)
		if !start.Equal(test.start) || start.Location() != time.UTC {
			t.Errorf("hourly range of %s starts %s, want %s", test.at, start, test.start)
		}
		r := PartitionGranularityHourly.RangeOf(test.at)
		if !r.Lower.Equal(test.start) || r.Upper.Sub(r.Lower) != time.Hour {
			t.Errorf("hourly range of %s is %+v", test.at, r)
		}
		if r.Lower.YearDay() != r.Upper.Add(-time.Nanosecond).YearDay() {
			t.Errorf("hourly range %+v straddles two days", r)
		}
	}
	// counting back and forth crosses days, months and years by whole hours
	start := time.Date(2026, 12, 31, 22, 0, 0, 0, time.UTC)
	if got := PartitionGranularityHourly.AddRanges(start, 3); !got.Equal(time.Date(2027, 1, 1, 1, 0, 0, 0, time.UTC)) {
		t.Errorf("three hours after %s is %s", start, got)
	}
	if got := PartitionGranularityHourly.AddRanges(start, -23); !got.Equal(time.Date(2026, 12, 30, 23, 0, 0, 0, time.UTC)) {
		t.Errorf("23 hours before %s is %s", start, got)
	}
}

// Leaves are named <table>_hYYYYMMDDHH, the name parses back to its range, and
// a name off the hour, of another granularity or of the wrong length is not
// the registry's.
func TestPartitionHourlyLeafNames(t *testing.T) {
	family := testPartitionHourlyFamily()
	r := PartitionGranularityHourly.RangeOf(time.Date(2026, 11, 8, 7, 15, 0, 0, time.UTC))
	name := family.LeafName("synthetic_hourly_record", r)
	if name != "synthetic_hourly_record_h2026110807" {
		t.Fatalf("leaf name %q", name)
	}
	parsed, ok := family.ParseLeafName("synthetic_hourly_record", name)
	if !ok || !parsed.Lower.Equal(r.Lower) || !parsed.Upper.Equal(r.Upper) {
		t.Fatalf("%s parses to %+v, %t", name, parsed, ok)
	}
	for _, other := range []string{
		"synthetic_hourly_record_p20261108",
		"synthetic_hourly_record_h20261108",
		"synthetic_hourly_record_h202611080",
		"synthetic_hourly_record_h2026110824",
		"synthetic_hourly_record_h2026113107",
		"synthetic_hourly_record_hx026110807",
		"other_record_h2026110807",
	} {
		if _, ok := family.ParseLeafName("synthetic_hourly_record", other); ok {
			t.Errorf("%s parses as the family's leaf", other)
		}
	}
	// the catalog's bounds of a canonical hour read back as the registry's own
	// leaf, and an hour's bound one minute off does not
	lower := "2026-11-08 07:00:00.000000"
	upper := "2026-11-08 08:00:00.000000"
	leaf, ok := family.ParseLeaf("synthetic_hourly_record", name, "FOR VALUES FROM ('2026-11-08 07:00:00') TO ('2026-11-08 08:00:00')", &lower, &upper)
	if !ok || !leaf.Canonical || !leaf.Range.Lower.Equal(r.Lower) {
		t.Fatalf("the canonical hour reads as %+v, %t", leaf, ok)
	}
	offUpper := "2026-11-08 08:01:00.000000"
	leaf, ok = family.ParseLeaf("synthetic_hourly_record", name, "FOR VALUES FROM ('2026-11-08 07:00:00') TO ('2026-11-08 08:01:00')", &lower, &offUpper)
	if !ok || leaf.Canonical {
		t.Fatalf("an hour a minute long reads as canonical: %+v, %t", leaf, ok)
	}
	// each hourly period has its own name, so parents are analyzed once an hour
	if a, b := PartitionGranularityHourly.periodName(r.Lower), PartitionGranularityHourly.periodName(r.Upper); a == b || a != "2026-11-08T07" {
		t.Fatalf("periods %q and %q", a, b)
	}
}

// Create-ahead at 22:40 UTC reaches through the next day's first hours with no
// gap at midnight, and a runway counts the hours across it.
func TestPartitionHourlyCreateAheadAcrossADay(t *testing.T) {
	family := testPartitionHourlyFamily()
	now := time.Date(2026, 11, 8, 22, 40, 0, 0, time.UTC)
	missing := family.MissingRanges(nil, now.Add(-time.Hour), now)
	names := []string{}
	for _, r := range missing {
		names = append(names, family.LeafName("synthetic_hourly_record", r))
	}
	want := []string{
		"synthetic_hourly_record_h2026110821",
		"synthetic_hourly_record_h2026110822",
		"synthetic_hourly_record_h2026110823",
		"synthetic_hourly_record_h2026110900",
		"synthetic_hourly_record_h2026110901",
		"synthetic_hourly_record_h2026110902",
		"synthetic_hourly_record_h2026110903",
		"synthetic_hourly_record_h2026110904",
	}
	if !slices.Equal(names, want) {
		t.Fatalf("missing %v, want %v", names, want)
	}
	for i := 1; i < len(missing); i++ {
		if !missing[i].Lower.Equal(missing[i-1].Upper) {
			t.Fatalf("a gap between %+v and %+v", missing[i-1], missing[i])
		}
	}
	leaves := []PartitionLeaf{}
	for _, r := range missing {
		leaves = append(leaves, PartitionLeaf{Table: "synthetic_hourly_record", Name: family.LeafName("synthetic_hourly_record", r), Range: r, Canonical: true})
	}
	if covered, future := family.Runway(leaves, now); !covered || future != family.AheadRanges {
		t.Fatalf("runway %t, %d", covered, future)
	}
	// an hour later, with nothing created, the window moved by one hour
	later := family.MissingRanges(leaves, now.Add(time.Hour), now.Add(time.Hour))
	if len(later) != 1 || family.LeafName("synthetic_hourly_record", later[0]) != "synthetic_hourly_record_h2026110905" {
		t.Fatalf("an hour later, missing %+v", later)
	}
}

// An hour expires once its upper bound is the retention past, and no sooner.
func TestPartitionHourlyExpiry(t *testing.T) {
	family := testPartitionHourlyFamily()
	r := PartitionGranularityHourly.RangeOf(time.Date(2026, 11, 8, 7, 0, 0, 0, time.UTC))
	dropAt := r.Upper.Add(23 * time.Hour)
	if family.IsExpired(t.Context(), dropAt.Add(-time.Nanosecond), r) {
		t.Fatal("expired before the retention passed")
	}
	if !family.IsExpired(t.Context(), dropAt, r) {
		t.Fatal("not expired once the retention passed")
	}
	// the oldest key of the range has then lived 24 hours
	if age := dropAt.Sub(r.Lower); age != 24*time.Hour {
		t.Fatalf("the oldest key lived %s", age)
	}
	if err := validatePartitionFamily(family); err != nil {
		t.Fatal(err)
	}
}

// Every leaf name fits Postgres's 63 bytes, which would otherwise cut it short
// and leave the registry unable to find its own leaf: an hourly leaf's
// _hYYYYMMDDHH is the longest suffix, and a daily leaf's _pYYYYMMDD is longer
// than _default, which the check once measured alone.
func TestPartitionLeafNamesFitPostgres(t *testing.T) {
	for _, test := range []struct {
		granularity PartitionGranularity
		suffix      string
	}{
		{PartitionGranularityHourly, "_h2026110807"},
		{PartitionGranularityDaily, "_p20261108"},
		{PartitionGranularityWeekly, "_w20261102"},
	} {
		if got := test.granularity.longestLeafSuffixLength(); got != len(test.suffix) {
			t.Fatalf("%s's longest suffix is %d bytes, want %d", test.granularity, got, len(test.suffix))
		}
		longest := strings.Repeat("a", pgMaxIdentifierLength-len(test.suffix))
		family := testPartitionHourlyFamily()
		family.Granularity = test.granularity
		family.Tables = []string{longest}
		if err := validatePartitionFamily(family); err != nil {
			t.Fatalf("%s: a table whose leaf names just fit is refused: %s", test.granularity, err)
		}
		r := test.granularity.RangeOf(time.Date(2026, 11, 8, 7, 0, 0, 0, time.UTC))
		if name := family.LeafName(longest, r); pgMaxIdentifierLength < len(name) {
			t.Fatalf("%s: leaf %s is %d bytes", test.granularity, name, len(name))
		}
		family.Tables = []string{longest + "a"}
		if err := validatePartitionFamily(family); err == nil {
			t.Fatalf("%s: a table one byte too long to name its leaves validates", test.granularity)
		}
	}
}
