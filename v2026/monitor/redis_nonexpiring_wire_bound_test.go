// Exercise the producer's numeric bound through public signal evaluation.
// Synthetic Host callbacks never execute Redis, a shell or a network request.
package monitor

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// Encode independent declared totals; do not sum corrupt fixture counters.
func redisWireBoundFrame(header [5]int64, families map[string][5]int64) string {
	parts := []string{}
	for _, value := range header {
		parts = append(parts, fmt.Sprint(value))
	}
	for _, name := range []string{"provide-pms", "provide-rp", "provide-sk", "provide-other", "score", "client-key", "connect", "stream", "other"} {
		parts = append(parts, name)
		for _, value := range families[name] {
			parts = append(parts, fmt.Sprint(value))
		}
	}
	return strings.Join(parts, " ")
}

// The two full frames have a strong fixed provide-family delta.
func redisWireBoundProvide(seen, missing int64) (string, string) {
	candidate := [5]int64{seen, seen - missing, 0, missing, 0}
	control := [5]int64{seen, 0, seen, 0, 0}
	return redisWireBoundFrame(candidate, map[string][5]int64{"provide-pms": candidate}),
		redisWireBoundFrame(control, map[string][5]int64{"other": control})
}

// Preserve a complete independently positive census while changing only detail.
func redisWireBoundSettings(t *testing.T, candidate, control string, healthy bool) (SignalSettings, *int) {
	t.Helper()
	calls := 0
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		switch {
		case strings.Contains(command, "INFO keyspace"):
			if healthy {
				return "6380 4810000 2555000 512\n6381 4810000 2555000 512\n6382 4810000 2555000 512", nil
			}
			return "6380 4810000 2555000 512\n6381 4811000 2555000 512\n6382 5399000 2554000 512", nil
		case strings.Contains(command, "EVAL_RO"):
			calls++
			if !strings.Contains(command, " 0 5000 25 250") {
				t.Fatal("wire fixture changed the actual Lua sampling bounds")
			}
			if strings.Contains(command, "-p 6382 ") {
				return candidate, nil
			}
			if strings.Contains(command, "-p 6381 ") {
				return control, nil
			}
		}
		t.Fatal("wire fixture received an unexpected public source command")
		return "", nil
	}}
	return syntheticSettings(source), &calls
}

// Rejected detail must not erase measured skew, invent zeroes, or grant cleanup.
func redisWireBoundRequireUnavailable(t *testing.T, candidate, control string) {
	t.Helper()
	settings, calls := redisWireBoundSettings(t, candidate, control, false)
	signal := NewRedisNonexpiringSignal()
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil || *calls != 2 {
		t.Fatal("public wire fixture did not finish both bounded detail callbacks")
	}
	skew := requireAlertClass(t, alerts, "nonexpiring-key-skew")
	visibility := requireAlertClass(t, alerts, "cannot-observe")
	if skew.Severity != SeverityWarn || skew.Sustain != 1 || skew.Target != "redis-1:6382" ||
		!strings.Contains(skew.Observed, "estimated_excess_keys=589000") ||
		!strings.Contains(skew.Observed, "detail_status=unavailable") ||
		strings.Contains(skew.Observed, "candidate_sampled=") || strings.Contains(skew.Observed, "control_sampled=") ||
		strings.Contains(skew.Action, "EXPIRE-NX") || !isVisibilityAlert(visibility) || visibility.SignalKey != signal.Key() {
		t.Fatal("rejected detail changed independent skew, unknown or maintenance authority")
	}
	for _, alert := range alerts {
		requireAlertOmits(t, alert, "wire-private-sentinel@example.invalid", "9223372036854775807", "9223372036854775808")
	}
}

// These conserved inputs pass the old parser and falsely select provide.
// The actual bounded Lua cannot emit them; they are consumer-contract controls.
func TestRedisNonexpiringWireBoundRejectsConservedImpossibleDetail(t *testing.T) {
	goodCandidate, goodControl := redisWireBoundProvide(5000, 0)
	overCandidate, overControl := redisWireBoundProvide(5001, 0)
	largeCandidate, largeControl := redisWireBoundProvide(6000, 0)
	wrapFamilies := redisWireBoundFrame([5]int64{1000, 1000, 0, 0, 0}, map[string][5]int64{
		"provide-pms": {math.MaxInt64, math.MaxInt64, 0, 0, 0},
		"provide-rp":  {math.MaxInt64, math.MaxInt64, 0, 0, 0},
		"provide-sk":  {1002, 1002, 0, 0, 0},
	})
	// Two MaxInt64 categories plus1002 wrap to1000; missing*10 wraps negative.
	wrappedCategories := [5]int64{1000, math.MaxInt64, 1002, math.MaxInt64, 0}
	wrapPTTL := redisWireBoundFrame(wrappedCategories, map[string][5]int64{"provide-pms": wrappedCategories})
	_, smallControl := redisWireBoundProvide(1000, 0)
	for _, item := range []struct {
		name, candidate, control string
	}{
		{"candidate-one-over", overCandidate, goodControl},
		{"control-one-over", goodCandidate, overControl},
		{"both-conserved-six-thousand", largeCandidate, largeControl},
		{"family-total-and-provide-group-wrap", wrapFamilies, smallControl},
		{"row-header-and-churn-wrap", wrapPTTL, smallControl},
	} {
		t.Run(item.name, func(t *testing.T) {
			redisWireBoundRequireUnavailable(t, item.candidate, item.control)
		})
	}
}

// Existing shape, integer, conservation, minimum and churn rules stay strict.
func TestRedisNonexpiringWireBoundPreservesRejectedDetailRules(t *testing.T) {
	good, control := redisWireBoundProvide(5000, 0)
	fields := strings.Fields(good)
	tooSmall, _ := redisWireBoundProvide(999, 0)
	tooMuchChurn, _ := redisWireBoundProvide(1000, 101)
	invalid := [5]int64{1000, 999, 0, 0, 1}
	badTotals := [5]int64{5000, 4999, 0, 0, 0}
	for _, item := range []struct{ name, wire string }{
		{"private-unknown-label", strings.Replace(good, "provide-pms", "wire-private-sentinel@example.invalid", 1)},
		{"reordered-label", strings.Replace(good, "provide-rp", "provide-pms", 1)},
		{"negative-count", strings.Replace(good, "5000", "-1", 1)},
		{"integer-outside-int64", strings.Replace(good, "5000", "9223372036854775808", 1)},
		{"partial-frame", strings.Join(fields[:len(fields)-1], " ")},
		{"extra-field", good + " 0"},
		{"small-sample", tooSmall},
		{"churn-over-ten-percent", tooMuchChurn},
		{"invalid-pttl", redisWireBoundFrame(invalid, map[string][5]int64{"provide-pms": invalid})},
		{"inconsistent-family", redisWireBoundFrame([5]int64{5000, 5000, 0, 0, 0}, map[string][5]int64{"provide-pms": badTotals})},
	} {
		t.Run(item.name, func(t *testing.T) {
			redisWireBoundRequireUnavailable(t, item.wire, control)
		})
	}
}

// Exact source cap, minimum, and ten-percent churn equality remain admissible.
func TestRedisNonexpiringWireBoundPreservesAdmissibleDetail(t *testing.T) {
	for _, item := range []struct {
		name          string
		seen, missing int64
	}{
		{"exact-five-thousand", 5000, 0},
		{"exact-one-thousand", 1000, 0},
		{"exact-ten-percent-missing", 1000, 100},
	} {
		t.Run(item.name, func(t *testing.T) {
			candidate, control := redisWireBoundProvide(item.seen, item.missing)
			settings, calls := redisWireBoundSettings(t, candidate, control, false)
			alerts, err := NewRedisNonexpiringSignal().Run(context.Background(), settings)
			if err != nil || *calls != 2 || len(alerts) != 1 {
				t.Fatal("valid bounded detail lost public source authority")
			}
			skew := requireAlertClass(t, alerts, "nonexpiring-key-skew")
			if !strings.Contains(skew.Observed, "detail_status=provide") ||
				!strings.Contains(skew.Observed, fmt.Sprintf("candidate_sampled=%d", item.seen)) ||
				!strings.Contains(skew.Action, "EXPIRE-NX") {
				t.Fatal("valid bounded detail changed existing family attribution")
			}
		})
	}
}

// Complete equal densities are healthy and do not request attribution at all.
func TestRedisNonexpiringWireBoundPreservesHealthyFleet(t *testing.T) {
	settings, calls := redisWireBoundSettings(t, "", "", true)
	alerts, err := NewRedisNonexpiringSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 || *calls != 0 {
		t.Fatal("complete healthy census gained an attribution dependency")
	}
}

// The real public results feed the in-memory notifier; no network notifier runs.
func TestRedisNonexpiringWireBoundRejectedDetailCannotClearSkew(t *testing.T) {
	consumer := &SlackAlertNotifier{
		settings:              SlackAlertNotifierSettings{ClearAfterCompleteRuns: 2},
		keyIncidents:          map[string]*slackIncident{},
		signalKeyCompleteRuns: map[string]int{},
	}
	good, control := redisWireBoundProvide(5000, 0)
	over, _ := redisWireBoundProvide(5001, 0)
	signal := NewRedisNonexpiringSignal()
	for run := 0; run < 5; run++ {
		candidate := good
		if run == 1 || run == 2 {
			candidate = over
		}
		settings, _ := redisWireBoundSettings(t, candidate, control, run >= 3)
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil {
			t.Fatal("public wire notifier fixture failed")
		}
		if run == 1 || run == 2 {
			skew := requireAlertClass(t, alerts, "nonexpiring-key-skew")
			if !strings.Contains(skew.Observed, "detail_status=unavailable") {
				t.Fatal("rejected detail retained family maintenance authority")
			}
			requireAlertClass(t, alerts, "cannot-observe")
		}
		transitions := consumer.observeWithLock(settings.Now().Add(time.Duration(run)*15*time.Minute), signal.Key(), SignalExecution{Alerts: alerts})
		raised, cleared := 0, 0
		for _, transition := range transitions {
			if transition.alert.Class != "nonexpiring-key-skew" {
				continue
			}
			if transition.kind == slackRaised {
				raised++
			}
			if transition.kind == slackCleared {
				cleared++
			}
		}
		if run == 0 && (raised != 1 || cleared != 0) ||
			run > 0 && run < 4 && (raised != 0 || cleared != 0) ||
			run == 4 && (raised != 0 || cleared != 1) {
			t.Fatal("rejected detail cleared skew or complete recovery lost its two-run boundary")
		}
	}
}
