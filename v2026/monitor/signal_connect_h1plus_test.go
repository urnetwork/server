package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Invented placement only; both legacy aggregate and corrected raw-query
// responses below are derived from these same deterministic process samples.
type h1PlusTestProcess struct {
	host, block, instance string
	values, times         map[string]float64
}

func h1PlusTestNow() time.Time { return time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC) }

func h1PlusTestProcessAt(now time.Time, host, block, instance string) *h1PlusTestProcess {
	p := &h1PlusTestProcess{host: host, block: block, instance: instance, values: map[string]float64{}, times: map[string]float64{}}
	for _, bound := range []string{"now", "prior"} {
		sampleTime := now.Add(-10 * time.Second)
		if bound == "prior" {
			sampleTime = sampleTime.Add(-15 * time.Minute)
		}
		values := map[string]float64{"start": float64(now.Add(-2 * time.Hour).Unix()), "attempts": 10, "accepted": 8, "messages": 100, "bytes": 1000, "auth_failures": 0, "handshake_failures": 0, "fallbacks": 0}
		if bound == "now" {
			values["attempts"] = 12
			values["accepted"] = 10
			values["messages"] = 105
			values["bytes"] = 1128
		}
		for key, value := range values {
			p.values[bound+"/"+key] = value
			p.times[bound+"/"+key] = float64(sampleTime.Unix())
		}
	}
	return p
}

func h1PlusTestFleet(now time.Time) []*h1PlusTestProcess {
	result := []*h1PlusTestProcess{}
	for _, host := range []string{"alpha.example.test", "beta.example.test"} {
		for _, block := range []string{"blue", "green"} {
			result = append(result, h1PlusTestProcessAt(now, host, block, host+"/"+block+"/one"))
		}
	}
	return result
}

func h1PlusTestResponse(t testing.TB, now time.Time, service string, processes []*h1PlusTestProcess, legacy bool) string {
	t.Helper()
	rows := []map[string]any{}
	add := func(labels map[string]string, value float64) {
		rows = append(rows, map[string]any{"metric": labels, "value": []any{float64(now.Unix()), strconv.FormatFloat(value, 'g', -1, 64)}})
	}
	if legacy {
		// The original query erases slot and generation identity, and tests
		// only the freshest accepted timestamp. This is its exact synthetic
		// two-bound equivalent, not a hand-written arbitrary healthy response.
		m := map[string]float64{"series": 0, "source_age": 3600}
		for _, p := range processes {
			if _, ok := p.values["now/accepted"]; ok {
				m["series"]++
				if at, ok := p.times["now/accepted"]; ok {
					m["source_age"] = min(m["source_age"], float64(now.Unix())-at)
				}
			}
			for _, field := range []string{"attempts", "accepted", "messages", "bytes", "auth_failures", "handshake_failures", "fallbacks"} {
				current, a := p.values["now/"+field]
				prior, b := p.values["prior/"+field]
				if a && b {
					m[field] += max(0, current-prior)
				}
			}
		}
		for field, value := range m {
			add(map[string]string{"monitor_h1plus": field}, value)
		}
	} else {
		protocol := "urnetwork-framer/1"
		if service == "proxy" {
			protocol = "urnetwork-framerxl/1"
		}
		for _, p := range processes {
			for _, isTime := range []bool{false, true} {
				source := p.values
				if isTime {
					source = p.times
				}
				for field, value := range source {
					key := field
					if isTime {
						key += "/time"
					}
					labels := map[string]string{"env": "synthetic", "job": service, "host": p.host, "block": p.block, "instance": p.instance, "monitor_h1plus": key}
					if !strings.HasSuffix(field, "/start") {
						labels["protocol"] = protocol
					}
					add(labels, value)
				}
			}
		}
	}
	out, err := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": rows}})
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func h1PlusTestSettings(t testing.TB, now time.Time, service string, processes []*h1PlusTestProcess) SignalSettings {
	t.Helper()
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		decoded, err := url.QueryUnescape(command)
		if err != nil || host.Name != "metrics.example.test" || !strings.Contains(decoded, "urnetwork_"+service+"_h1plus_accepted_total") {
			return "", fmt.Errorf("unexpected synthetic H1+ command")
		}
		return h1PlusTestResponse(t, now, service, processes, strings.Contains(decoded, "sum(increase(")), nil
	}}
	settings := syntheticSettings(source)
	settings.Environment = "synthetic"
	settings.Now = func() time.Time { return now }
	settings.LogServices = []string{service}
	settings.LogServiceBlocks = map[string][]string{service: {"blue", "green"}}
	settings.LogServiceHosts = map[string][]string{service: {"alpha.example.test", "beta.example.test"}}
	settings.Hosts = []HostSettings{{Name: "metrics.example.test", Roles: []string{"services"}}, {Name: "alpha.example.test"}, {Name: "beta.example.test"}}
	return settings
}

func TestConnectH1PlusCurrentSlotsHealthyControl(t *testing.T) {
	now := h1PlusTestNow()
	alerts, err := NewConnectH1PlusSignal().Run(context.Background(), h1PlusTestSettings(t, now, "connect", h1PlusTestFleet(now)))
	if err != nil || len(alerts) != 0 {
		t.Fatalf("complete active slots: alerts=%+v err=%v", alerts, err)
	}
}

func TestConnectH1PlusLongLivedPayloadNeedsNoNewUpgrade(t *testing.T) {
	now := h1PlusTestNow()
	fleet := h1PlusTestFleet(now)
	for _, p := range fleet {
		p.values["now/accepted"] = p.values["prior/accepted"]
		p.values["now/attempts"] = p.values["prior/attempts"]
	}
	alerts, err := NewConnectH1PlusSignal().Run(context.Background(), h1PlusTestSettings(t, now, "connect", fleet))
	if err != nil || len(alerts) != 0 {
		t.Fatalf("long-lived accepted streams with new payload were not recognized: alerts=%+v err=%v", alerts, err)
	}
}

func TestConnectH1PlusCurrentSlotCoverageCannotBorrowSibling(t *testing.T) {
	now := h1PlusTestNow()
	for _, test := range []struct {
		name   string
		mutate func([]*h1PlusTestProcess) []*h1PlusTestProcess
	}{
		{name: "missing-process", mutate: func(p []*h1PlusTestProcess) []*h1PlusTestProcess { return p[1:] }},
		{name: "stale-process", mutate: func(p []*h1PlusTestProcess) []*h1PlusTestProcess {
			for key := range p[0].times {
				p[0].times[key] -= 91
			}
			return p
		}},
		{name: "missing-bytes", mutate: func(p []*h1PlusTestProcess) []*h1PlusTestProcess { delete(p[0].values, "now/bytes"); return p }},
		{name: "missing-source-time", mutate: func(p []*h1PlusTestProcess) []*h1PlusTestProcess { delete(p[0].times, "now/bytes"); return p }},
		{name: "stale-one-counter", mutate: func(p []*h1PlusTestProcess) []*h1PlusTestProcess { p[0].times["now/bytes"] -= 91; return p }},
		{name: "future-source-time", mutate: func(p []*h1PlusTestProcess) []*h1PlusTestProcess {
			p[0].times["now/bytes"] = float64(now.Add(31 * time.Second).Unix())
			return p
		}},
		{name: "different-scrape", mutate: func(p []*h1PlusTestProcess) []*h1PlusTestProcess { p[0].times["now/bytes"]--; return p }},
		{name: "counter-reset", mutate: func(p []*h1PlusTestProcess) []*h1PlusTestProcess { p[0].values["now/bytes"] = 0; return p }},
		{name: "missing-start", mutate: func(p []*h1PlusTestProcess) []*h1PlusTestProcess {
			delete(p[0].values, "now/start")
			delete(p[0].times, "now/start")
			return p
		}},
		{name: "invalid-start", mutate: func(p []*h1PlusTestProcess) []*h1PlusTestProcess { p[0].values["now/start"] = 0; return p }},
		{name: "start-after-scrape", mutate: func(p []*h1PlusTestProcess) []*h1PlusTestProcess {
			p[0].values["now/start"] = float64(now.Unix())
			return p
		}},
		{name: "reused-instance", mutate: func(p []*h1PlusTestProcess) []*h1PlusTestProcess { p[0].values["now/start"] += 60; return p }},
		{name: "stale-prior", mutate: func(p []*h1PlusTestProcess) []*h1PlusTestProcess { p[0].times["prior/start"] -= 91; return p }},
		{name: "nonadvancing-scrape", mutate: func(p []*h1PlusTestProcess) []*h1PlusTestProcess {
			p[0].times["prior/start"] = p[0].times["now/start"]
			return p
		}},
	} {
		fleet := test.mutate(h1PlusTestFleet(now))
		alerts, err := NewConnectH1PlusSignal().Run(context.Background(), h1PlusTestSettings(t, now, "connect", fleet))
		if err != nil || len(alerts) != 1 || alerts[0].Target != "connect/blue" || !strings.HasPrefix(alerts[0].Class, "h1plus-telemetry-") {
			t.Errorf("%s borrowed sibling authority: alerts=%+v err=%v", test.name, alerts, err)
		}
	}
}

func TestConnectH1PlusSelectionAcrossScrapeBoundaryIsHealthy(t *testing.T) {
	// RecordH1PlusSelection increments attempts before accepted, and Snapshot
	// loads independent atomics. Across scrapes accepted_delta may exceed
	// attempts_delta without any per-counter reset or ownership violation.
	now := h1PlusTestNow()
	fleet := h1PlusTestFleet(now)
	for _, p := range fleet {
		p.values["now/attempts"] = p.values["prior/attempts"]
	}
	alerts, err := NewConnectH1PlusSignal().Run(context.Background(), h1PlusTestSettings(t, now, "connect", fleet))
	if err != nil || len(alerts) != 0 {
		t.Fatalf("independent atomic snapshot became invalid ratio: alerts=%+v err=%v", alerts, err)
	}
}

func TestConnectH1PlusNewGenerationCannotBorrowDrainingPayload(t *testing.T) {
	now := h1PlusTestNow()
	for _, mode := range []string{"warming", "newest-stale", "equal-start-ambiguous", "newest-missing-counter"} {
		fleet := h1PlusTestFleet(now)
		current := h1PlusTestProcessAt(now, fleet[0].host, fleet[0].block, "new-generation.example.test")
		switch mode {
		case "warming":
			current.values["now/start"] = float64(now.Add(-time.Minute).Unix())
			for key := range current.values {
				if strings.HasPrefix(key, "prior/") {
					delete(current.values, key)
					delete(current.times, key)
				}
			}
		case "newest-stale":
			current.values["now/start"] += 120
			for key := range current.times {
				current.times[key] -= 91
			}
		case "equal-start-ambiguous":
		case "newest-missing-counter":
			current.values["now/start"] += 120
			current.values["prior/start"] += 120
			delete(current.values, "now/bytes")
		}
		fleet = append(fleet, current)
		alerts, err := NewConnectH1PlusSignal().Run(context.Background(), h1PlusTestSettings(t, now, "connect", fleet))
		if err != nil || len(alerts) != 1 || alerts[0].Target != "connect/blue" || !strings.HasPrefix(alerts[0].Class, "h1plus-telemetry-") {
			t.Errorf("%s borrowed draining activity: alerts=%+v err=%v", mode, alerts, err)
		}
	}
}

func TestConnectH1PlusDrainedGenerationDoesNotContaminateCurrentControl(t *testing.T) {
	now := h1PlusTestNow()
	fleet := h1PlusTestFleet(now)
	old := h1PlusTestProcessAt(now, fleet[0].host, fleet[0].block, "draining.example.test")
	old.values["now/start"] -= 3600
	old.values["prior/start"] -= 3600
	for key := range old.times {
		old.times[key] -= 91
	}
	delete(old.values, "now/bytes")
	fleet = append(fleet, old)
	alerts, err := NewConnectH1PlusSignal().Run(context.Background(), h1PlusTestSettings(t, now, "connect", fleet))
	if err != nil || len(alerts) != 0 {
		t.Fatalf("complete current generation lost authority: alerts=%+v err=%v", alerts, err)
	}
}

func TestConnectH1PlusNoPayloadIsUnverifiedNotFeatureFailure(t *testing.T) {
	now := h1PlusTestNow()
	for _, mode := range []string{"zero-counters", "heartbeat-only", "no-accepted-witness", "different-process-payload"} {
		fleet := h1PlusTestFleet(now)
		for _, p := range fleet {
			p.values["now/bytes"] = p.values["prior/bytes"]
			if mode == "zero-counters" {
				for key := range p.values {
					if !strings.HasSuffix(key, "/start") {
						p.values[key] = 0
					}
				}
			}
			if mode == "no-accepted-witness" {
				p.values["now/bytes"]++
				p.values["now/accepted"] = 0
				p.values["prior/accepted"] = 0
			}
			if mode == "different-process-payload" {
				if p.host == "alpha.example.test" {
					p.values["now/bytes"] += 12
					p.values["now/messages"] = p.values["prior/messages"]
				}
			}
		}
		alerts, err := NewConnectH1PlusSignal().Run(context.Background(), h1PlusTestSettings(t, now, "connect", fleet))
		if err != nil || len(alerts) != 2 {
			t.Errorf("%s: alerts=%+v err=%v", mode, alerts, err)
			continue
		}
		for _, alert := range alerts {
			if alert.Class != "h1plus-activity-unverified" || !strings.Contains(alert.Markdown(), "not a feature failure") {
				t.Errorf("%s: lost uncertainty: %s", mode, alert.Markdown())
			}
		}
	}
}

func TestConnectH1PlusActiveBlockDoesNotCertifyIdleSibling(t *testing.T) {
	now := h1PlusTestNow()
	fleet := h1PlusTestFleet(now)
	for _, p := range fleet {
		if p.block == "blue" {
			p.values["now/messages"] = p.values["prior/messages"]
			p.values["now/bytes"] = p.values["prior/bytes"]
		}
	}
	alerts, err := NewConnectH1PlusSignal().Run(context.Background(), h1PlusTestSettings(t, now, "connect", fleet))
	if err != nil || len(alerts) != 1 || alerts[0].Target != "connect/blue" || alerts[0].Class != "h1plus-activity-unverified" {
		t.Fatalf("active sibling hid unverified block: alerts=%+v err=%v", alerts, err)
	}
}

func TestConnectH1PlusInventoryAndExcludedSlotsRemainUnknown(t *testing.T) {
	now := h1PlusTestNow()
	for _, mode := range []string{"missing-host-placement", "missing-block-placement", "unconfigured-host", "disabled-host", "excluded-host"} {
		settings := h1PlusTestSettings(t, now, "connect", h1PlusTestFleet(now))
		switch mode {
		case "missing-host-placement":
			settings.LogServiceHosts = nil
		case "missing-block-placement":
			settings.LogServiceBlocks = nil
		case "unconfigured-host":
			settings.Hosts = settings.Hosts[:2]
		case "disabled-host":
			settings.disabledHosts = []HostSettings{settings.Hosts[2]}
			settings.Hosts = settings.Hosts[:2]
		case "excluded-host":
			var err error
			settings, err = ExcludeHosts(settings, "beta.example.test")
			if err != nil {
				t.Fatal(err)
			}
		}
		alerts, err := NewConnectH1PlusSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) == 0 {
			t.Errorf("%s inferred health without authority: alerts=%+v err=%v", mode, alerts, err)
		}
		for _, alert := range alerts {
			if !strings.HasPrefix(alert.Class, "h1plus-telemetry-") {
				t.Errorf("%s: unexpected finding: %+v", mode, alert)
			}
		}
		if mode == "excluded-host" && !reflect.DeepEqual(settings.LogServiceHosts["connect"], []string{"alpha.example.test", "beta.example.test"}) {
			t.Fatal("exclusion shrank desired placement")
		}
	}
}

func TestConnectH1PlusSourceResponseFailsClosed(t *testing.T) {
	now := h1PlusTestNow()
	for _, mode := range []string{"warnings", "truncated", "duplicate", "wrong-protocol", "wrong-job", "missing-identity", "unknown-field", "not-finite", "stale-evaluation", "over-byte-bound", "over-row-bound"} {
		settings := h1PlusTestSettings(t, now, "connect", h1PlusTestFleet(now))
		settings.Source = &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
			decoded, _ := url.QueryUnescape(command)
			out := h1PlusTestResponse(t, now, "connect", h1PlusTestFleet(now), strings.Contains(decoded, "sum(increase("))
			var response map[string]any
			if err := json.Unmarshal([]byte(out), &response); err != nil {
				t.Fatal(err)
			}
			rows := response["data"].(map[string]any)["result"].([]any)
			row := rows[0].(map[string]any)
			labels := row["metric"].(map[string]any)
			switch mode {
			case "warnings":
				response["warnings"] = []string{"synthetic partial response"}
			case "truncated":
				return out[:len(out)/2], nil
			case "duplicate":
				response["data"].(map[string]any)["result"] = append(rows, rows[0])
			case "wrong-protocol":
				labels["protocol"] = "other"
				labels["monitor_h1plus"] = "now/accepted"
			case "wrong-job":
				labels["job"] = "other"
			case "missing-identity":
				delete(labels, "instance")
			case "unknown-field":
				labels["monitor_h1plus"] = "now/unbounded"
			case "not-finite":
				row["value"].([]any)[1] = "NaN"
			case "stale-evaluation":
				row["value"].([]any)[0] = float64(now.Add(-2 * time.Minute).Unix())
			case "over-byte-bound":
				return strings.Repeat(" ", 524289), nil
			case "over-row-bound":
				for len(rows) <= 512 {
					rows = append(rows, rows[0])
				}
				response["data"].(map[string]any)["result"] = rows
			}
			encoded, err := json.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			return string(encoded), nil
		}}
		alerts, err := NewConnectH1PlusSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) == 0 || alerts[0].Class != "h1plus-telemetry-incomplete" {
			t.Errorf("%s did not fail visibility closed: alerts=%+v err=%v", mode, alerts, err)
		}
	}
}

func TestConnectH1PlusQueryRetainsTwoBoundAuthorityAndBudget(t *testing.T) {
	query := h1PlusQuery("synthetic", h1PlusProbe{service: "connect", protocol: "urnetwork-framer/1"})
	for _, want := range []string{"process_start_time_seconds", "offset 15m", "timestamp(", "prior/start/time", "now/bytes/time"} {
		if !strings.Contains(query, want) {
			t.Errorf("query lacks %s", want)
		}
	}
	for _, forbidden := range []string{"sum(", "max(", "increase(", "vector(0)"} {
		if strings.Contains(query, forbidden) {
			t.Errorf("query loses authority with %s", forbidden)
		}
	}
	now := h1PlusTestNow()
	settings := h1PlusTestSettings(t, now, "connect", h1PlusTestFleet(now))
	calls := 0
	settings.Source = &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		calls++
		if !strings.Contains(command, "--max-time 15 --max-filesize 524288 ") {
			t.Errorf("not inventory-bounded: %s", command)
		}
		return h1PlusTestResponse(t, now, "connect", h1PlusTestFleet(now), false), nil
	}}
	if _, err := NewConnectH1PlusSignal().Run(context.Background(), settings); err != nil || calls != 1 {
		t.Fatalf("bounded single query: calls=%d err=%v", calls, err)
	}
}

func TestConnectH1PlusOneFreshSlotCannotCertifyManyStaleSlots(t *testing.T) {
	now := h1PlusTestNow()
	fleet := h1PlusTestFleet(now)
	for _, p := range fleet[1:] {
		for key := range p.times {
			p.times[key] -= 120
		}
	}
	alerts, err := NewConnectH1PlusSignal().Run(context.Background(), h1PlusTestSettings(t, now, "connect", fleet))
	if err != nil || len(alerts) != 2 {
		t.Fatalf("freshest source hid stale slots: alerts=%+v err=%v", alerts, err)
	}
	for _, alert := range alerts {
		if alert.Target == "connect/blue" && (alert.Class != "h1plus-telemetry-incomplete" || !strings.Contains(alert.Markdown(), "complete_slots=1") || !strings.Contains(alert.Markdown(), "stale_slots=1")) {
			t.Error("mixed block lost coverage counts")
		}
		if alert.Target == "connect/green" && alert.Class != "h1plus-telemetry-stale" {
			t.Error("wholly stale block borrowed fresh sibling")
		}
	}
}

func TestConnectH1PlusOnlyDrainingPayloadLeavesCurrentBlockUnverified(t *testing.T) {
	now := h1PlusTestNow()
	fleet := h1PlusTestFleet(now)
	old := h1PlusTestProcessAt(now, fleet[0].host, "blue", "draining-only.example.test")
	old.values["now/start"] -= 3600
	old.values["prior/start"] -= 3600
	for _, p := range fleet {
		if p.block == "blue" {
			p.values["now/messages"] = p.values["prior/messages"]
			p.values["now/bytes"] = p.values["prior/bytes"]
		}
	}
	fleet = append(fleet, old)
	alerts, err := NewConnectH1PlusSignal().Run(context.Background(), h1PlusTestSettings(t, now, "connect", fleet))
	if err != nil || len(alerts) != 1 || alerts[0].Target != "connect/blue" || alerts[0].Class != "h1plus-activity-unverified" {
		t.Fatalf("old payload certified current block: alerts=%+v err=%v", alerts, err)
	}
}

func TestConnectH1PlusBoundedSourceErrorIsUnknownAndNotRetried(t *testing.T) {
	now := h1PlusTestNow()
	settings := h1PlusTestSettings(t, now, "connect", h1PlusTestFleet(now))
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "other-metrics.example.test", Roles: []string{"services"}})
	calls := 0
	settings.Source = &syntheticSource{hostFn: func(_ HostSettings, _ string) (string, error) {
		calls++
		return "", fmt.Errorf("private synthetic transport body")
	}}
	alerts, err := NewConnectH1PlusSignal().Run(context.Background(), settings)
	if err != nil || calls != 1 || len(alerts) != 1 || alerts[0].Class != "h1plus-telemetry-incomplete" {
		t.Fatalf("source failure calls=%d alerts=%+v err=%v", calls, alerts, err)
	}
	if strings.Contains(alerts[0].Markdown(), "private synthetic transport body") {
		t.Error("raw transport error rendered")
	}
}

func TestConnectH1PlusCancellationRemainsLifecycle(t *testing.T) {
	now := h1PlusTestNow()
	settings := h1PlusTestSettings(t, now, "connect", h1PlusTestFleet(now))
	ctx, cancel := context.WithCancel(context.Background())
	settings.Source = &syntheticSource{hostFn: func(_ HostSettings, _ string) (string, error) { cancel(); return "", context.Canceled }}
	alerts, err := NewConnectH1PlusSignal().Run(ctx, settings)
	cancel()
	if err != context.Canceled || len(alerts) != 0 {
		t.Fatalf("cancellation became activity or health: alerts=%+v err=%v", alerts, err)
	}
}

func TestConnectH1PlusSingleBlockDrainingOnlyCannotCertifyCurrent(t *testing.T) {
	now := h1PlusTestNow()
	fleet := h1PlusTestFleet(now)
	// Keep exactly one desired block, two current slots, and one older
	// generation in that same block. All new messages/bytes belong ONLY to
	// the draining generation: there is no healthy sibling block to confound
	// the original fleet reducer's positive result.
	current := []*h1PlusTestProcess{fleet[0], fleet[2]}
	for _, p := range current {
		p.values["now/messages"] = p.values["prior/messages"]
		p.values["now/bytes"] = p.values["prior/bytes"]
	}
	draining := h1PlusTestProcessAt(now, current[0].host, "blue", "draining-isolated.example.test")
	draining.values["now/start"] -= 3600
	draining.values["prior/start"] -= 3600
	processes := append(current, draining)
	settings := h1PlusTestSettings(t, now, "connect", processes)
	settings.LogServiceBlocks = map[string][]string{"connect": {"blue"}}
	alerts, err := NewConnectH1PlusSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 1 || alerts[0].Target != "connect/blue" || alerts[0].Class != "h1plus-activity-unverified" {
		t.Fatalf("draining-only activity certified current slots: alerts=%+v err=%v", alerts, err)
	}
	for _, want := range []string{"expected_slots=2", "complete_slots=2", "payload_active_slots=0", "paired_messages_delta=0", "paired_bytes_delta=0"} {
		if !strings.Contains(alerts[0].Markdown(), want) {
			t.Errorf("isolated drain boundary lacks %q", want)
		}
	}
}

func TestConnectH1PlusSingleBlockCurrentPayloadHealthyDespiteDraining(t *testing.T) {
	now := h1PlusTestNow()
	fleet := h1PlusTestFleet(now)
	current := []*h1PlusTestProcess{fleet[0], fleet[2]}
	// A single active current slot is sufficient with complete visibility;
	// the other current slot need not have any recent payload.
	current[1].values["now/messages"] = current[1].values["prior/messages"]
	current[1].values["now/bytes"] = current[1].values["prior/bytes"]
	draining := h1PlusTestProcessAt(now, current[0].host, "blue", "draining-isolated.example.test")
	draining.values["now/start"] -= 3600
	draining.values["prior/start"] -= 3600
	settings := h1PlusTestSettings(t, now, "connect", append(current, draining))
	settings.LogServiceBlocks = map[string][]string{"connect": {"blue"}}
	alerts, err := NewConnectH1PlusSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 {
		t.Fatalf("current payload was hidden by drain: alerts=%+v err=%v", alerts, err)
	}
}

func TestConnectH1PlusDocumentationKeepsEvidenceBoundary(t *testing.T) {
	// Use the canonical documentation reader so this tests the same catalog
	// path as the crosswalk, without accessing live configuration.
	data, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	for _, phrase := range []string{"accepted lifetime", "server-side framed writes", "current process", "receive-only", "early rejections", "same-process", "LogServiceHosts"} {
		if !strings.Contains(body, phrase) {
			t.Errorf("H1+ catalog lacks %q", phrase)
		}
	}
}
