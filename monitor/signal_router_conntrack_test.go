package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

const syntheticRouterConntrack = "--count--\n100\n--max--\n1000\n--hash--\n256\n--stat--\nentries searched found new invalid ignore delete delete_list insert insert_failed drop early_drop\n00000064 00000000 00000000 00000000 00000000 00000000 00000000 00000000 00000000 00000001 00000000 00000000\n00000064 00000000 00000000 00000000 00000000 00000000 00000000 00000000 00000000 00000001 00000000 00000000"

func TestRouterConntrackFirstAndResetSamplesStayUnknown(t *testing.T) {
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	source := newSyntheticRouterSource()
	source.body = syntheticRouterConntrack
	settings := syntheticRouterSettings(source, &now)
	signal := NewRouterConntrackSignal()
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "cannot-observe")
	now = now.Add(5 * time.Minute)
	alerts, err = signal.Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 {
		t.Fatalf("stable zero-delta healthy sample alerts=%d error=%v", len(alerts), err)
	}
	source.body = strings.ReplaceAll(syntheticRouterConntrack, "00000001", "00000000")
	now = now.Add(5 * time.Minute)
	alerts, err = signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "cannot-observe")
	requireRouterPrivate(t, alerts)
}

func TestRouterConntrackMissingCountersPreserveCapacity(t *testing.T) {
	for _, counters := range []string{"unavailable", "entries insert_failed drop early_drop\n00040000 0 0 0\ntruncated"} {
		t.Run(counters[:7], func(t *testing.T) {
			now := time.Date(2026, 10, 9, 23, 3, 19, 0, time.UTC)
			source := newSyntheticRouterSource()
			source.body = "--count--\n262144\n--max--\n262144\n--hash--\n32768\n--stat--\n" + counters
			settings := syntheticRouterSettings(source, &now)
			signal := NewRouterConntrackSignal()
			for range 2 {
				alerts, err := signal.Run(context.Background(), settings)
				if err != nil {
					t.Fatal(err)
				}
				requireAlertClass(t, alerts, "router-conntrack-capacity")
				requireAlertClass(t, alerts, "router-conntrack-pressure")
				requireAlertClass(t, alerts, "cannot-observe")
				for _, alert := range alerts {
					if alert.Class == "router-conntrack-drops" {
						t.Fatal("full table with unavailable counters became packet-drop proof")
					}
				}
				requireRouterPrivate(t, alerts)
				now = now.Add(time.Second)
			}
		})
	}
}

func syntheticRouterKernelTableFull(uptime, messages, latest string) string {
	return "\n--kernel-table-full--\nuptime_seconds=" + uptime + "\nretained_messages=" + messages + "\nlatest_uptime_seconds=" + latest
}

func TestRouterConntrackFullTableKernelEvidenceIsQualified(t *testing.T) {
	for _, tc := range []struct {
		name, kernel string
		wantDrops    bool
	}{
		{"recent explicit loss", syntheticRouterKernelTableFull("21282381", "32", "21282074.363152"), true},
		{"freshness boundary", syntheticRouterKernelTableFull("1800", "1", "900"), true},
		{"expired historical loss", syntheticRouterKernelTableFull("1800.001", "32", "900"), false},
		{"no retained loss", syntheticRouterKernelTableFull("1800", "0", "0"), false},
		{"unavailable kernel log", "\n--kernel-table-full--\nunavailable", false},
		{"future kernel time", syntheticRouterKernelTableFull("1800", "1", "1801"), false},
		{"nonfinite kernel time", syntheticRouterKernelTableFull("NaN", "1", "1800"), false},
		{"malformed kernel evidence", syntheticRouterKernelTableFull("1800", "synthetic-secret", "900"), false},
		{"duplicate kernel fields", syntheticRouterKernelTableFull("1800", "1", "900") + "\nretained_messages=4", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 10, 9, 23, 3, 19, 0, time.UTC)
			source := newSyntheticRouterSource()
			source.body = "--count--\n262144\n--max--\n262144\n--hash--\n32768\n--stat--\nunavailable" + tc.kernel
			alerts, err := NewRouterConntrackSignal().Run(context.Background(), syntheticRouterSettings(source, &now))
			if err != nil {
				t.Fatal(err)
			}
			requireAlertClass(t, alerts, "router-conntrack-pressure")
			requireAlertClass(t, alerts, "cannot-observe")
			found := false
			for _, alert := range alerts {
				found = found || alert.Class == "router-conntrack-drops"
			}
			if found != tc.wantDrops {
				t.Fatalf("qualified full-table loss=%t, want %t", found, tc.wantDrops)
			}
			requireRouterPrivate(t, alerts)
		})
	}
}

func TestRouterConntrackFullWithoutLossIsPressureOnly(t *testing.T) {
	now := time.Date(2026, 10, 9, 23, 3, 19, 0, time.UTC)
	source := newSyntheticRouterSource()
	source.body = strings.Replace(syntheticRouterConntrack, "--count--\n100\n", "--count--\n1000\n", 1) + syntheticRouterKernelTableFull("1800", "0", "0")
	settings := syntheticRouterSettings(source, &now)
	signal := NewRouterConntrackSignal()
	if _, err := signal.Run(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 || alerts[0].Class != "router-conntrack-pressure" {
		t.Fatalf("full table with measured zero counter deltas is pressure only: %v", alerts)
	}
}

func TestRouterConntrackOperatorCapacityChangeDoesNotProveRecovery(t *testing.T) {
	now := time.Date(2026, 10, 9, 23, 3, 19, 0, time.UTC)
	source := newSyntheticRouterSource()
	source.summary = strings.Replace(syntheticRouterSummary, `"table_size":1000,"hash_size":256`, `"table_size":262144,"hash_size":32768`, 1)
	settings := syntheticRouterSettings(source, &now)
	signal := NewRouterConntrackSignal()
	for index, sample := range [][2]int{{262144, 262144}, {306563, 1048576}, {306528, 1048576}} {
		source.body = fmt.Sprintf("--count--\n%d\n--max--\n%d\n--hash--\n32768\n--stat--\nunavailable", sample[0], sample[1]) + syntheticRouterKernelTableFull("21282381", "32", "21282074.363152")
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		requireAlertClass(t, alerts, "cannot-observe")
		if index == 0 {
			requireAlertClass(t, alerts, "router-conntrack-pressure")
			requireAlertClass(t, alerts, "router-conntrack-drops")
		} else {
			requireAlertClass(t, alerts, "router-conntrack-capacity")
			for _, alert := range alerts {
				if alert.Class == "router-conntrack-pressure" || alert.Class == "router-conntrack-drops" {
					t.Fatal("old full-table messages proved current pressure or loss after an external capacity change")
				}
			}
		}
		requireRouterPrivate(t, alerts)
		now = now.Add(time.Second)
	}
}

func TestRouterConntrackCounterGapRequiresFreshPair(t *testing.T) {
	now := time.Date(2026, 10, 9, 23, 3, 19, 0, time.UTC)
	source := newSyntheticRouterSource()
	settings := syntheticRouterSettings(source, &now)
	signal := NewRouterConntrackSignal()
	for index, body := range []string{
		syntheticRouterConntrack,
		"--count--\n100\n--max--\n1000\n--hash--\n256\n--stat--\nunavailable",
		strings.Replace(syntheticRouterConntrack, "00000001 00000000 00000000", "00000001 00000003 00000000", 1),
		strings.Replace(syntheticRouterConntrack, "00000001 00000000 00000000", "00000001 00000003 00000000", 1),
	} {
		source.body = body
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		if index < 3 {
			requireAlertClass(t, alerts, "cannot-observe")
		} else if len(alerts) != 0 {
			t.Fatal("fresh complete zero-delta pair did not restore counter observation")
		}
		for _, alert := range alerts {
			if alert.Class == "router-conntrack-drops" {
				t.Fatal("a counter delta bridged an unavailable observation")
			}
		}
		now = now.Add(time.Second)
	}
}

func TestRouterConntrackKernelEvidenceCannotRescueInvalidCapacity(t *testing.T) {
	now := time.Date(2026, 10, 9, 23, 3, 19, 0, time.UTC)
	source := newSyntheticRouterSource()
	source.body = "--count--\n262144\n--max--\n0\n--hash--\n32768\n--stat--\nunavailable" + syntheticRouterKernelTableFull("1800", "1", "900")
	alerts, err := NewRouterConntrackSignal().Run(context.Background(), syntheticRouterSettings(source, &now))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 || alerts[0].Class != "cannot-observe" {
		t.Fatal("kernel evidence bypassed required live capacity validity")
	}
}

func TestRouterConntrackCorroboratedDropsAndAppliedCapacity(t *testing.T) {
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	source := newSyntheticRouterSource()
	source.body = syntheticRouterConntrack
	settings := syntheticRouterSettings(source, &now)
	signal := NewRouterConntrackSignal()
	if _, err := signal.Run(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	source.body = strings.Replace(syntheticRouterConntrack, "00000001 00000000 00000000", "00000002 00000003 00000004", 1)
	now = now.Add(5 * time.Minute)
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "router-conntrack-drops")
	source.body = strings.Replace(syntheticRouterConntrack, "--max--\n1000", "--max--\n500", 1)
	alerts, err = NewRouterConntrackSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "router-conntrack-capacity")
	requireRouterPrivate(t, alerts)
}

func TestRouterConntrackInsertFailureAloneAndMissingIntentAreUnknown(t *testing.T) {
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	source := newSyntheticRouterSource()
	source.body = syntheticRouterConntrack
	settings := syntheticRouterSettings(source, &now)
	signal := NewRouterConntrackSignal()
	if _, err := signal.Run(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	source.body = strings.Replace(syntheticRouterConntrack, "00000001 00000000 00000000", "00000002 00000000 00000000", 1)
	now = now.Add(5 * time.Minute)
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "cannot-observe")
	for _, alert := range alerts {
		if alert.Class == "router-conntrack-drops" {
			t.Fatal("duplicate insertion alone became a packet-drop claim")
		}
	}
	source.summary = strings.Replace(syntheticRouterSummary, `"explicit":true`, `"explicit":false`, 1)
	alerts, err = NewRouterConntrackSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "cannot-observe")
}

func TestRouterConntrackPartialDesiredFieldsRemainIndependent(t *testing.T) {
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	for _, capacity := range []string{`"table_size":500,"hash_size":0`, `"table_size":0,"hash_size":128`} {
		source := newSyntheticRouterSource()
		source.body = syntheticRouterConntrack
		source.summary = strings.Replace(syntheticRouterSummary, `"table_size":1000,"hash_size":256`, capacity, 1)
		alerts, err := NewRouterConntrackSignal().Run(context.Background(), syntheticRouterSettings(source, &now))
		if err != nil {
			t.Fatal(err)
		}
		requireAlertClass(t, alerts, "router-conntrack-capacity")
		requireAlertClass(t, alerts, "cannot-observe")
	}
}

func TestRouterConntrackOccupancyNeverSumsPerCpuEntries(t *testing.T) {
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	source := newSyntheticRouterSource()
	source.body = strings.Replace(syntheticRouterConntrack, "--max--\n1000", "--max--\n150", 1)
	source.summary = strings.Replace(syntheticRouterSummary, `"table_size":1000`, `"table_size":150`, 1)
	settings := syntheticRouterSettings(source, &now)
	signal := NewRouterConntrackSignal()
	if _, err := signal.Run(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	now = now.Add(5 * time.Minute)
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 {
		t.Fatal("repeated per-CPU entries inflated 100/150 live occupancy")
	}
}
