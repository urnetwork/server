// Synthetic controls for the observed-volume signal in §25.4.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// The normal registry and catalog must expose the scoped byte-floor reader.
func TestDeliveredTrafficFloorRegistry(t *testing.T) {
	for _, signal := range NewSignals() {
		if signal.Number() == "25.4" && signal.Key() == "delivered-traffic-floor" {
			return
		}
	}
	t.Fatal("missing independent matched-slot traffic floor")
}

// Unclassified classes default to visibility in the notifier. A confirmed
// low-volume measurement must be delivered as an incident on its own target.
func TestDeliveredTrafficFloorClassifications(t *testing.T) {
	incident, knownIncident := alertClassKinds[deliveredTrafficClass]
	visibility, knownVisibility := alertClassKinds[deliveredTrafficUnknownClass]
	if !knownIncident || incident != incidentAlertClass || !knownVisibility || visibility != visibilityAlertClass {
		t.Fatal("traffic classifications must distinguish measurement from missing evidence")
	}
}

// Replay eight disjoint ranges through the public adapter and check both the
// requested time bounds and the reducer's per-target recovery holds.
func testDeliveredTrafficWindows(t *testing.T, currentConnect, currentProxy float64, mutate func(day int, rows []stormSyntheticSeries) []stormSyntheticSeries) (Alerts, map[string]bool) {
	t.Helper()
	policy := DefaultDeliveredTrafficFloorSettings()
	now := time.Date(2026, 3, 10, 0, 8, 9, 0, time.UTC)
	days := []float64{0, 100, 1, 500, 80, 120, 160, 40} // median 100, despite one high day
	calls := 0
	source := &syntheticSource{hostFn: func(h HostSettings, cmd string) (string, error) {
		day := calls
		calls++
		if day >= len(days) || h.Name != "collector.example" {
			t.Fatal("unexpected history read")
		}
		end := now.Add(-policy.EvaluationLag).AddDate(0, 0, -day)
		start := end.Add(-policy.WarnHold)
		for _, part := range []string{"query_range", "--max-time", "--max-filesize", fmt.Sprintf("end=%d", end.Unix()), fmt.Sprintf("start=%d", start.Unix()), "step=60", "process_start_time_seconds", "urnetwork_connect_exchange_local_bytes_total", "urnetwork_proxy_session_bytes_total"} {
			if !strings.Contains(cmd, part) {
				t.Fatalf("day %d lacks a requested source or range bound", day)
			}
		}
		connectRate, proxyRate := days[day], days[day]
		if day == 0 {
			connectRate, proxyRate = currentConnect, currentProxy
		}
		rows := []stormSyntheticSeries{}
		for _, row := range []struct {
			service, part string
			rate          float64
		}{
			{service: "connect", part: "publishers", rate: 1}, {service: "proxy", part: "publishers", rate: 1},
			{service: "connect", part: "socket", rate: connectRate}, {service: "connect", part: "local", rate: 0}, {service: "proxy", part: "session", rate: proxyRate},
		} {
			rows = append(rows, stormSyntheticSeries{part: row.part, labels: map[string]string{"job": row.service, "host": row.service + ".example", "block": "g0"}, values: stormRepeat(row.rate, 16)})
		}
		if mutate != nil {
			rows = mutate(day, rows)
		}
		return stormSyntheticResponse(t, start, time.Minute, rows), nil
	}}
	settings := syntheticSettings(source)
	settings.Environment = "synthetic"
	settings.Now = func() time.Time { return now }
	settings.Hosts = []HostSettings{{Name: "collector.example", Roles: []string{"services"}}, {Name: "connect.example"}, {Name: "proxy.example"}}
	settings.LogServices = []string{"connect", "proxy"}
	settings.LogServiceHosts = map[string][]string{"connect": {"connect.example"}, "proxy": {"proxy.example"}}
	settings.LogServiceBlocks = map[string][]string{"connect": {"g0"}, "proxy": {"g0"}}
	obs := &signalClearHoldObservation{}
	ctx := context.WithValue(context.Background(), signalClearHoldContextKey{}, obs)
	alerts, err := NewDeliveredTrafficFloorSignal().Run(ctx, settings)
	if err != nil || calls != len(days) {
		t.Fatalf("public history observation calls=%d err=%v", calls, err)
	}
	holds := map[string]bool{}
	for _, hold := range obs.snapshot(false) {
		if hold.Class == deliveredTrafficClass {
			holds[hold.Target] = true
		}
	}
	return alerts, holds
}

// Test exact band equality, complete elapsed holds and independent proxy volume.
func TestDeliveredTrafficFloorSeasonalBands(t *testing.T) {
	for _, tc := range []struct {
		name           string
		connect, proxy float64
		warn, page     string
	}{
		{name: "both healthy", connect: 100, proxy: 100},
		{name: "exact warn recovers", connect: 50, proxy: 50},
		{name: "exact page is warning", connect: 25, proxy: 100, warn: "synthetic/connect"},
		{name: "October scale", connect: 7, proxy: 35, warn: "synthetic/proxy", page: "synthetic/connect"},
		{name: "observed zero", connect: 100, proxy: 0, page: "synthetic/proxy"},
	} {
		alerts, holds := testDeliveredTrafficWindows(t, tc.connect, tc.proxy, nil)
		count := 0
		for _, x := range []string{tc.warn, tc.page} {
			if x != "" {
				count++
			}
		}
		if len(alerts) != count || len(holds) != 0 {
			t.Fatalf("%s: alerts=%v holds=%v", tc.name, alerts, holds)
		}
		for _, a := range alerts {
			if a.unobservable || a.Class != deliveredTrafficClass || a.Severity == SeverityPage && a.Target != tc.page || a.Severity == SeverityWarn && a.Target != tc.warn {
				t.Fatalf("%s: wrong band or target: %s %s", tc.name, a.Class, a.Target)
			}
			if !strings.Contains(a.Observed, "reference_median_bytes_per_second=100.000") {
				t.Fatalf("%s: wrong reference median", tc.name)
			}
		}
	}
}

// Missing old days, partial local delivery, source ambiguity and idle baselines
// cannot clear a volume incident. The qualified sibling retains its own page.
func TestDeliveredTrafficFloorUnknownCohorts(t *testing.T) {
	for _, mode := range []string{"missing reference", "historical extra", "overlap", "missing local", "duplicate", "zero baseline", "nonfinite", "sum overflow", "ratio overflow"} {
		alerts, holds := testDeliveredTrafficWindows(t, 20, 10, func(day int, rows []stormSyntheticSeries) []stormSyntheticSeries {
			switch mode {
			case "missing reference":
				if day == 7 {
					rows[2].values[15] = math.NaN()
				}
			case "historical extra":
				if day == 3 {
					rows = append(rows, stormSyntheticSeries{part: "publishers", labels: map[string]string{"job": "connect", "host": "retired.example", "block": "g9"}, values: stormRepeat(1, 16)})
				}
			case "overlap":
				if day == 0 {
					rows[0].values[15] = 2
				}
			case "missing local":
				if day == 0 {
					rows[3].values[15] = math.NaN()
				}
			case "duplicate":
				if day == 2 {
					rows = append(rows, rows[2])
				}
			case "zero baseline":
				if day != 0 {
					rows[2].values = stormRepeat(0, 16)
				}
			case "sum overflow":
				if day == 0 {
					rows[2].values[15], rows[3].values[15] = math.MaxFloat64, math.MaxFloat64
				}
			case "ratio overflow":
				if day == 0 {
					rows[2].values[15] = math.MaxFloat64
				} else {
					rows[2].values[15] = math.SmallestNonzeroFloat64
				}
			case "nonfinite":
				if day == 0 {
					rows[3].values[15] = math.Inf(1)
				}
			}
			return rows
		})
		if len(alerts) != 2 || !holds["synthetic/connect"] {
			t.Fatalf("%s: alerts=%v holds=%v", mode, alerts, holds)
		}
		for _, a := range alerts {
			if a.Target == "synthetic/connect" && (a.Class != deliveredTrafficUnknownClass || !a.unobservable) ||
				a.Target == "synthetic/proxy" && (a.Class != deliveredTrafficClass || a.Severity != SeverityPage) {
				t.Fatalf("%s: wrong independent classification", mode)
			}
			requireAlertOmits(t, a, "retired.example", "connect.example", "proxy.example")
		}
	}
}

// Moving traffic from sockets onto local links preserves the matched total.
// A broken hold remains open until every recovery endpoint is healthy.
func TestDeliveredTrafficFloorSustainAndLocalDelivery(t *testing.T) {
	for _, mode := range []string{"path move", "page oldest recovered", "warn oldest recovered", "gap before page"} {
		alerts, holds := testDeliveredTrafficWindows(t, 10, 100, func(day int, rows []stormSyntheticSeries) []stormSyntheticSeries {
			if day != 0 {
				return rows
			}
			switch mode {
			case "path move":
				rows[3].values = stormRepeat(90, 16)
			case "page oldest recovered":
				rows[2].values[5] = 35
			case "warn oldest recovered":
				rows[2].values = stormRepeat(35, 16)
				rows[2].values[0] = 80
			case "gap before page":
				rows[2].values[0] = math.NaN()
			}
			return rows
		})
		switch mode {
		case "path move":
			if len(alerts) != 0 || len(holds) != 0 {
				t.Fatal("healthy path move alerted")
			}
		case "page oldest recovered":
			if len(alerts) != 1 || alerts[0].Severity != SeverityWarn {
				t.Fatal("ten-minute elapsed page hold shortened")
			}
		case "warn oldest recovered":
			if len(alerts) != 0 || !holds["synthetic/connect"] {
				t.Fatal("mixed fifteen-minute history cleared")
			}
		case "gap before page":
			if len(alerts) != 2 || requireAlertClass(t, alerts, deliveredTrafficClass).Severity != SeverityPage {
				t.Fatal("earlier gap erased independently proved page")
			}
		}
	}
}

// A canceled read returns lifecycle; unavailable transport and warnings preserve visibility.
func TestDeliveredTrafficFloorReadFailures(t *testing.T) {
	for _, mode := range []string{"canceled", "failed", "warning", "invalid policy", "unplaced"} {
		calls := 0
		settings := syntheticSettings(&syntheticSource{hostFn: func(HostSettings, string) (string, error) {
			calls++
			switch mode {
			case "canceled":
				return "", context.Canceled
			case "failed":
				return "", errors.New("private-source.example")
			}
			return `{"status":"success","warnings":["private-source.example"],"data":{"resultType":"matrix","result":[]}}`, nil
		}})
		settings.Environment = "synthetic"
		settings.Hosts = []HostSettings{{Name: "collector.example", Roles: []string{"services"}}, {Name: "connect.example"}}
		settings.LogServices = []string{"connect"}
		settings.LogServiceHosts = map[string][]string{"connect": {"connect.example"}}
		settings.LogServiceBlocks = map[string][]string{"connect": {"g0"}}
		policy := DefaultDeliveredTrafficFloorSettings()
		if mode == "invalid policy" {
			policy.WarnRatio = math.NaN()
		}
		if mode == "unplaced" {
			settings.LogServiceHosts = nil
		}
		alerts, err := NewDeliveredTrafficFloorSignalWithSettings(policy).Run(context.Background(), settings)
		if mode == "canceled" {
			if !errors.Is(err, context.Canceled) || len(alerts) != 0 {
				t.Fatal("cancellation lost its lifecycle")
			}
			continue
		}
		if (mode == "invalid policy" || mode == "unplaced") && calls != 0 {
			t.Fatalf("%s contacted a source", mode)
		}
		if err != nil || len(alerts) != 2 {
			t.Fatalf("%s err=%v alerts=%v", mode, err, alerts)
		}
		for _, a := range alerts {
			if !a.unobservable || a.Class != deliveredTrafficUnknownClass {
				t.Fatalf("%s opened a volume incident", mode)
			}
			requireAlertOmits(t, a, "private-source.example")
		}
	}
}

// Run the production expression against real counters in the pinned PromQL engine.
func TestDeliveredTrafficFloorPromqlEngine(t *testing.T) {
	runCatalogPromqlScript(t, func() string {
		inventories := map[string]fleetSlotInventory{}
		for _, service := range []string{"connect", "proxy"} {
			inventories[service] = fleetSlotInventory{slots: []fleetSlot{{service: service, host: "alpha", block: "g0", allowed: true}}}
		}
		query := deliveredTrafficQuery("synthetic", inventories, DefaultDeliveredTrafficFloorSettings())
		var script strings.Builder
		for _, mode := range []string{"complete", "lazy", "reset", "stale push", "duplicate", "legacy socket", "missing local", "incarnation", "duplicate start", "zero", "missing day start"} {
			script.WriteString("# " + mode + "\nclear\nload 1m\n")
			identity := func(service string) string {
				return `env="synthetic",job="` + service + `",service="` + service + `",host="alpha",block="g0",instance="aaaabb00"`
			}
			for _, service := range []string{"connect", "proxy"} {
				val := "1+0x60"
				if service == "proxy" && mode == "incarnation" {
					val = "1+0x49 2960+0x10"
				}
				fmt.Fprintf(&script, "  process_start_time_seconds{%s} %s\n", identity(service), val)
				if service == "proxy" && mode == "duplicate start" {
					fmt.Fprintf(&script, "  process_start_time_seconds{%s,extra=\"collision\"} %s\n", identity(service), val)
				}
			}
			for _, family := range deliveredTrafficFamilies {
				var labels []string
				if family.service == "proxy" {
					for _, proto := range []string{"http", "socks"} {
						for _, dir := range []string{"client_to_destination", "destination_to_client"} {
							labels = append(labels, fmt.Sprintf("protocol=%q,direction=%q", proto, dir))
						}
					}
				} else {
					ops := []string{"transport", "forward"}
					if family.part == "socket" {
						ops = append(ops, "unknown")
					}
					for _, op := range ops {
						for _, dir := range []string{"sent", "received"} {
							for _, kind := range []string{"data", "ping", "handshake"} {
								cell := fmt.Sprintf("direction=%q,kind=%q", dir, kind)
								if mode != "legacy socket" || family.part != "socket" {
									cell += fmt.Sprintf(",op=%q", op)
								} else if op != "transport" {
									continue
								}
								labels = append(labels, cell)
							}
						}
					}
				}
				for index, labels := range labels {
					val := "100+60x60"
					if mode == "zero" {
						val = "0+0x60"
					}
					if mode == "missing local" && family.part == "local" {
						continue
					}
					if family.part == "session" && index == 0 {
						switch mode {
						case "lazy":
							continue
						case "reset":
							val = "100+60x42 1+60x17"
						case "stale push":
							val = "100+60x59"
						case "missing day start":
							val = strings.Repeat("_ ", 36) + "100+60x24"
						case "duplicate":
							fmt.Fprintf(&script, "  %s{%s,%s,extra=\"collision\"} %s\n", family.metric, identity(family.service), labels, val)
						}
					}
					fmt.Fprintf(&script, "  %s{%s,%s} %s\n", family.metric, identity(family.service), labels, val)
				}
			}
			fmt.Fprintf(&script, "\neval instant at 60m %s\n", query)
			for _, service := range []string{"connect", "proxy"} {
				fmt.Fprintf(&script, "  {job=%q,host=\"alpha\",block=\"g0\",monitor_part=\"publishers\"} 1\n", service)
			}
			for _, family := range deliveredTrafficFamilies {
				if family.part == "session" && mode != "complete" && mode != "zero" && mode != "legacy socket" && mode != "missing local" {
					continue
				}
				if family.part == "socket" && mode == "legacy socket" || family.part == "local" && mode == "missing local" {
					continue
				}
				rate := family.cells
				if mode == "zero" {
					rate = 0
				}
				fmt.Fprintf(&script, "  {job=%q,host=\"alpha\",block=\"g0\",monitor_part=%q} %d\n", family.service, family.part, rate)
			}
			script.WriteString("\n")
		}
		return script.String()
	})
}
