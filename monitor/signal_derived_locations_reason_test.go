package monitor

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// SIGNALS.md §2.19c against the hours the extender ping review measured
// (2026-11-11). Gossip publishing stopped on 2026-11-06, every record the
// mesh had carried lapsed a day later, and from then on the targets refused
// most extender pingers as unknown (reason 4): their directories no longer
// held the pingers' records. The refused share counted those refusals, so the
// stale directories read as targets refusing honest claims -- 17-24% of an
// hour's verdicts against the 10% line, 5.6-9.1% without them -- and nothing
// said the directories were stale.

// The column names of the ping query's select list, after the hour.
var derivedLocationsPingColumnPattern = regexp.MustCompile(`\bAS ([a-z_]+)`)

// The rows the ping query returns for `hours`, oldest first, the last the last
// complete hour: each row in the columns and the order the query names them,
// from each hour's counts by column name. A column an hour has no count for
// fails the test, so no count is silently zero.
func derivedLocationsPingRowsByColumn(t *testing.T, query string, hours []map[string]int64) []Row {
	t.Helper()
	columns := []string{}
	for _, match := range derivedLocationsPingColumnPattern.FindAllStringSubmatch(query, -1) {
		columns = append(columns, match[1])
	}
	if len(columns) == 0 || columns[0] != "last_complete_hour" {
		t.Fatalf("the ping query names the columns %v", columns)
	}
	rows := []Row{}
	for i, hour := range hours {
		row := Row{fmt.Sprintf("2026-11-11 %02d:00:00", 21-len(hours)+i)}
		for _, column := range columns {
			if column == "last_complete_hour" {
				row = append(row, strconv.FormatBool(i == len(hours)-1))
				continue
			}
			count, ok := hour[column]
			if !ok {
				t.Fatalf("the hour has no count for the ping query's column %s", column)
			}
			row = append(row, strconv.FormatInt(count, 10))
		}
		rows = append(rows, row)
	}
	return rows
}

// The alerts of one run of the signal at its defaults, every other query
// healthy and the ping query answered from `hours`.
func derivedLocationsAlertsForPingHours(t *testing.T, hours []map[string]int64) Alerts {
	t.Helper()
	source := healthyDerivedLocationsFixture().source(t)
	healthy := source.postgresFn
	source.postgresFn = func(query string) ([]Row, error) {
		if strings.Contains(query, "monitor-signal-2.19c-derived-locations-pings */") {
			return derivedLocationsPingRowsByColumn(t, query, hours), nil
		}
		return healthy(query)
	}
	alerts, err := NewDerivedLocationsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

// An average hour of the 24 to 21Z on 2026-11-11, from the hour tally the
// review read: providers co-signed 11,575 and were refused 1,201 times for a
// round trip below the observed one; extender pingers co-signed 146, were
// refused 15 times for the round trip and 1,075 times as unknown pingers; 336
// pings got no verdict. The refused share counted every refusal, and the hour
// read as targets refusing 16.4% of claims, the derive-refusals finding. Left
// out, as the derive leaves them out, the unknown pingers are 87% of the
// extender pingers' verdicts, and the targets refused 9.4% of the claims they
// judged: the hour is the stale directories, and only that. Once records flow
// and the targets know their pingers again, the hour is quiet.
func TestDerivedLocationsSignalReadsAGossipOutageAsStaleDirectories(t *testing.T) {
	outageHour := map[string]int64{
		"cosigned":                11575 + 146,
		"rejected":                1201 + 15 + 1075,
		"rate_limited":            0,
		"unknown":                 336,
		"relayed":                 0,
		"zero_rtt":                0,
		"beyond_half_planet":      0,
		"unknown_pinger":          1075,
		"extender_cosigned":       146,
		"extender_rejected":       15 + 1075,
		"extender_unknown_pinger": 1075,
	}
	alerts := derivedLocationsAlertsForPingHours(t, []map[string]int64{outageHour, outageHour})
	if got := derivedLocationsAlertKeys(alerts); strings.Join(got, ",") != "extender-directories-stale/" {
		t.Fatalf("the gossip outage's hour raised %v, want the stale directories and nothing else", got)
	}
	stale := requireAlertClass(t, alerts, "extender-directories-stale")
	for _, want := range []string{
		"unknown_pinger_last_hour=1075",
		"extender_verdicts_last_hour=1236",
		"share=0.8697",
		"threshold=DeriveMaxUnknownPingerShare=0.30",
	} {
		if !strings.Contains(stale.Observed, want) {
			t.Fatalf("observed %q lacks %q", stale.Observed, want)
		}
	}
	if !strings.Contains(stale.Action, "Directories are stale: check gossip and DNS publishing") {
		t.Fatalf("action %q does not point at publishing", stale.Action)
	}
	if !strings.Contains(stale.Context, "day: extender_unknown_pinger=2150 extender_verdicts=2472") {
		t.Fatalf("context %q does not carry the day", stale.Context)
	}

	// the targets refusing on the claims' merits, at the same hour's rate, are
	// still found once past the line: unknown pingers neither hide nor make a
	// refusal
	judgedHour := maps.Clone(outageHour)
	judgedHour["rejected"] += 300
	alerts = derivedLocationsAlertsForPingHours(t, []map[string]int64{judgedHour})
	if got := derivedLocationsAlertKeys(alerts); strings.Join(got, ",") != "derive-refusals/rejected,extender-directories-stale/" {
		t.Fatalf("an hour of targets refusing on the merits beside stale directories raised %v", got)
	}
	refusals := requireAlertClass(t, alerts, "derive-refusals")
	if !strings.Contains(refusals.Observed, "rejected_last_hour=1516 verdicts_last_hour=13237") ||
		!strings.Contains(refusals.Observed, "excluded_unknown_pinger=1075 excluded_rate_limited=0") {
		t.Fatalf("refusals observed %q", refusals.Observed)
	}

	// records flowing: the targets know their pingers again
	recoveredHour := maps.Clone(outageHour)
	recoveredHour["cosigned"] = 11575 + 1100
	recoveredHour["rejected"] = 1201 + 75 + 61
	recoveredHour["unknown_pinger"] = 61
	recoveredHour["extender_cosigned"] = 1100
	recoveredHour["extender_rejected"] = 75 + 61
	recoveredHour["extender_unknown_pinger"] = 61
	alerts = derivedLocationsAlertsForPingHours(t, []map[string]int64{outageHour, recoveredHour})
	if got := derivedLocationsAlertKeys(alerts); len(got) != 0 {
		t.Fatalf("an hour with records flowing raised %v", got)
	}
}
