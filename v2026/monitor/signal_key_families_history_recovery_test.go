package monitor

// Learned-band recovery controls drive public Run through the production
// scheduler, cadence gate and notifier. Sources and webhook transport are
// synthetic; handler/tick barriers order every baseline replacement.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

const keyFamilyHistoryMetric = "redis/family/ckey_<id>"
const keyFamilyHistoryTarget = "redis-history.example:6380"

// Complete measurements are independent of whether their learned band exists.
func keyFamilyHistorySettings(t *testing.T, histogram string, sourceErr error) SignalSettings {
	t.Helper()
	source := &syntheticSource{
		hostFn: func(host HostSettings, command string) (string, error) {
			if host.Name != "redis-history.example" || !strings.Contains(command, "INFO memory") {
				return "", errors.New("unexpected synthetic history census")
			}
			return "6380 9000000 10000000\n6381 4000000 10000000\n6382 3000000 10000000", nil
		},
		hostTimeoutFn: func(host HostSettings, _ string, _ time.Duration) (string, error) {
			if host.Name != "redis-history.example" {
				return "", errors.New("unexpected synthetic history scan")
			}
			return histogram, sourceErr
		},
	}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{Name: "redis-history.example", Roles: []string{"redis-cluster"}, RedisEntryPort: 6379, RedisNodePorts: []int{6380, 6381, 6382}}}
	settings.StateDir = t.TempDir()
	return settings.withDefaults().withRuntime()
}

// Model loss or aging of local learned history, without changing the current
// source sample. Both live and durable history are replaced at a handler barrier.
func keyFamilyHistoryReplace(t *testing.T, store *baselineStore, metric string, ages []time.Duration, value float64) {
	t.Helper()
	store.stateLock.Lock()
	delete(store.metricSamples, metric)
	delete(store.metricAppendCounts, metric)
	store.stateLock.Unlock()
	if err := os.WriteFile(store.path(metric), nil, 0600); err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	for _, age := range ages {
		store.record(metric, at.Add(-age), value)
	}
}

// Read only after real Handle has completed at the scheduler barrier.
func keyFamilyHistoryIncident(notifier *SlackAlertNotifier, target string) (bool, int) {
	notifier.stateLock.Lock()
	defer notifier.stateLock.Unlock()
	incident := notifier.keyIncidents["redis/key-families|family-growth|"+target]
	if incident == nil {
		return false, 0
	}
	return true, incident.signalKeyAbsentRuns["key-families"]
}

// OLD must fail the precise unchecked-recovery assertion after two successful
// public runs. GREEN must preserve the incident, then clear only after two
// qualified comparisons. No native or network source is used.
func TestKeyFamilyHistoryPendingCannotClearThroughGateAndNotifier(t *testing.T) {
	for _, test := range []struct {
		name string
		ages []time.Duration
	}{
		{name: "missing"},
		{name: "insufficient", ages: []time.Duration{time.Hour, 2 * time.Hour}},
		{name: "expired", ages: []time.Duration{8 * 24 * time.Hour, 9 * 24 * time.Hour, 10 * 24 * time.Hour}},
		{name: "future", ages: []time.Duration{-24 * time.Hour, -25 * time.Hour, -26 * time.Hour}},
	} {
		t.Run(test.name, func(t *testing.T) {
			settings := keyFamilyHistorySettings(t, "30000 ckey_<id>\nfamily_source complete 30000\n", nil)
			store := settings.runtime.baseline
			notifier, webhook := newJournalRefillNotifier(t)
			postSignalRunsThroughTheGate(t, NewKeyFamiliesSignal(), settings, 5, func(run int) {
				switch run {
				case 0:
					keyFamilyHistoryReplace(t, store, keyFamilyHistoryMetric, []time.Duration{time.Hour, 2 * time.Hour, 3 * time.Hour}, 1000)
				case 1, 2:
					if present, _ := keyFamilyHistoryIncident(notifier, keyFamilyHistoryTarget); !present {
						t.Fatal("known family-growth did not establish the mature incident")
					}
					keyFamilyHistoryReplace(t, store, keyFamilyHistoryMetric, test.ages, 1000)
				case 3:
					if samples := store.metricSamples[keyFamilyHistoryMetric]; len(samples) != len(test.ages)+1 || samples[len(samples)-1].v != 30000 {
						t.Fatal("pending comparison discarded the complete current measurement")
					}
					if present, absent := keyFamilyHistoryIncident(notifier, keyFamilyHistoryTarget); !present || absent != 0 {
						t.Fatal("key-family learned-history recovery assertion: two unchecked complete scans cleared or credited the existing incident")
					}
					keyFamilyHistoryReplace(t, store, keyFamilyHistoryMetric, []time.Duration{time.Hour, 2 * time.Hour, 3 * time.Hour}, 30000)
				case 4:
					if present, absent := keyFamilyHistoryIncident(notifier, keyFamilyHistoryTarget); !present || absent != 1 {
						t.Fatal("first qualified family comparison did not retain exactly one recovery credit")
					}
				}
			}, notifier)
			if present, _ := keyFamilyHistoryIncident(notifier, keyFamilyHistoryTarget); present {
				t.Fatal("two qualified family comparisons did not clear the incident")
			}
			closeSlackNotifier(t, notifier)
			texts := webhook.posted()
			if len(texts) != 2 || !strings.Contains(texts[0], "[WARN] raised") || !strings.Contains(texts[1], "[CLEARED] WARN") {
				t.Fatal("pending history emitted an extra incident or changed measured recovery transitions")
			}
		})
	}
}

// Observe the adapter's actual per-execution hold publication, including the
// successful silent path that cannot be inferred from empty Alerts alone.
// Return only selected-target holds while checking every other hold's scope.
func keyFamilyHistoryRun(t *testing.T, settings SignalSettings) (Alerts, []SignalClassClearHold, error) {
	t.Helper()
	observation := &signalClearHoldObservation{}
	ctx := context.WithValue(context.Background(), signalClearHoldContextKey{}, observation)
	alerts, err := NewKeyFamiliesSignal().Run(ctx, settings)
	selected := []SignalClassClearHold{}
	for _, hold := range observation.snapshot(err != nil) {
		if hold.SignalId != "redis/key-families" || hold.Class != "family-growth" {
			t.Fatal("family history hold escaped its own class")
		}
		switch hold.Target {
		case keyFamilyHistoryTarget:
			selected = append(selected, hold)
		case "redis-history.example:6381", "redis-history.example:6382":
			// These configured nodes supplied INFO but were not scanned.
		default:
			t.Fatal("family history hold invented a target outside the configured census")
		}
	}
	return alerts, selected, err
}

// A missing band does not erase measured positives; the unchanged static
// size floor and complete qualified comparisons retain their known authority.
func TestKeyFamilyHistoryPublicRunAuthorityAndTargetScope(t *testing.T) {
	for _, test := range []struct {
		name      string
		histogram string
		seed      bool
		wantAlert bool
		wantHold  bool
	}{
		{name: "material unknown", histogram: "30000 ckey_<id>\nfamily_source complete 30000\n", wantHold: true},
		{name: "static size boundary", histogram: "20000 ckey_<id>\nfamily_source complete 20000\n"},
		{name: "complete empty", histogram: "family_source complete 0\n"},
		{name: "qualified in band", histogram: "30000 ckey_<id>\nfamily_source complete 30000\n", seed: true},
		{name: "known positive beside pending", histogram: "90000 ckey_<id>\n30000 ncr_<id>\nfamily_source complete 120000\n", seed: true, wantAlert: true, wantHold: true},
		{name: "private pending family", histogram: "30000 customer@example.invalid\nfamily_source complete 30000\n", wantHold: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			settings := keyFamilyHistorySettings(t, test.histogram, nil)
			if test.seed {
				keyFamilyHistoryReplace(t, settings.runtime.baseline, keyFamilyHistoryMetric, []time.Duration{time.Hour, 2 * time.Hour, 3 * time.Hour}, 20000)
			}
			alerts, holds, err := keyFamilyHistoryRun(t, settings)
			if err != nil || len(alerts) > 1 || (len(alerts) == 1) != test.wantAlert || len(holds) > 1 || (len(holds) == 1) != test.wantHold {
				t.Fatal("family comparison changed positive, static healthy or pending authority")
			}
			if test.wantAlert {
				alert := requireAlertClass(t, alerts, "family-growth")
				if alert.Target != keyFamilyHistoryTarget || alert.Frame != "ckey_<id>" || !strings.Contains(alert.Observed, "count=90000 median=20000") {
					t.Fatal("qualified positive lost its measured target, family or band")
				}
			}
			if test.wantHold && holds[0] != (SignalClassClearHold{SignalId: "redis/key-families", Class: "family-growth", Target: keyFamilyHistoryTarget}) {
				t.Fatal("pending hold escaped the selected class/host/port identity")
			}
			for metric := range settings.runtime.baseline.metricSamples {
				if strings.Contains(metric, "customer") || strings.Contains(metric, "@") {
					t.Fatal("pending sample leaked a raw family before persistence")
				}
			}
		})
	}
}

// Keep existing required-source failure and partial-census authority distinct
// from successful pending history. Neither can append a bad baseline or hold.
func TestKeyFamilyHistoryMalformedCanceledAndPartialRemainUnknown(t *testing.T) {
	for _, test := range []struct {
		name      string
		histogram string
		err       error
		partial   bool
	}{
		{name: "malformed", histogram: "30000 customer@example.invalid\nprivate-bad-footer\n"},
		{name: "canceled source", histogram: "30000 ckey_<id>\nfamily_source complete 30000\n", err: context.Canceled},
		{name: "partial fleet", histogram: "30000 ckey_<id>\nfamily_source complete 30000\n", partial: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			settings := keyFamilyHistorySettings(t, test.histogram, test.err)
			if test.partial {
				settings.Source.(*syntheticSource).hostFn = func(HostSettings, string) (string, error) {
					return "6380 9000000 10000000\n6381 4000000 10000000", nil
				}
			}
			alerts, holds, err := keyFamilyHistoryRun(t, settings)
			if len(holds) != 0 || len(settings.runtime.baseline.metricSamples) != 0 {
				t.Fatal("failed or partial source acquired pending hold or baseline authority")
			}
			if test.err != nil {
				if !errors.Is(err, context.Canceled) || len(alerts) != 0 {
					t.Fatal("canceled source published a successful comparison")
				}
			} else {
				if err != nil || len(alerts) != 1 || !alerts[0].unobservable {
					t.Fatal("malformed or partial source lost its independent visibility")
				}
				requireAlertOmits(t, alerts[0], "customer@example.invalid", "private-bad-footer")
			}
		})
	}
}

// A missing store is also a pending comparison. Its hold raises nothing and
// protects only an incident this exact signal already owns at that target.
func TestKeyFamilyHistoryNoStoreHoldDoesNotEnrollOrProtectAnotherTarget(t *testing.T) {
	settings := keyFamilyHistorySettings(t, "30000 ckey_<id>\nfamily_source complete 30000\n", nil)
	settings.runtime.baseline = nil
	alerts, holds, err := keyFamilyHistoryRun(t, settings)
	if err != nil || len(alerts) != 0 || len(holds) != 1 || holds[0].Target != keyFamilyHistoryTarget {
		t.Fatal("absent store supplied a completed family comparison")
	}
	notifier, webhook := newJournalRefillNotifier(t)
	signal := NewKeyFamiliesSignal()
	pending := SignalExecution{Alerts: alerts, ClearHolds: holds}
	notifier.Handle(signal, pending)
	if present, _ := keyFamilyHistoryIncident(notifier, keyFamilyHistoryTarget); present {
		t.Fatal("silent pending history created an incident")
	}
	otherTarget := "other-history.example:6380"
	seed := func(target string) Alert {
		return Alert{SignalID: "redis/key-families", Class: "family-growth", Target: target, Severity: SeverityWarn, Environment: "synthetic"}
	}
	notifier.Handle(signal, SignalExecution{Alerts: Alerts{seed(keyFamilyHistoryTarget), seed(otherTarget)}})
	notifier.Handle(signal, pending)
	notifier.Handle(signal, pending)
	if present, absent := keyFamilyHistoryIncident(notifier, keyFamilyHistoryTarget); !present || absent != 0 {
		t.Fatal("pending selected target gained absence credit")
	}
	if present, _ := keyFamilyHistoryIncident(notifier, otherTarget); present {
		t.Fatal("selected target hold became a whole-signal or other-target hold")
	}
	closeSlackNotifier(t, notifier)
	if texts := webhook.posted(); len(texts) != 3 {
		t.Fatal("hold created a notification or erased an unrelated measured transition")
	}
}

// Unknown fleet selection retains the existing qualified sample and separate
// census gap, without advancing the complete-fleet learned series.
func TestKeyFamilyHistoryPartialFleetKeepsQualifiedPositive(t *testing.T) {
	settings := keyFamilyHistorySettings(t, "30000 ckey_<id>\nfamily_source complete 30000\n", nil)
	settings.Source.(*syntheticSource).hostFn = func(HostSettings, string) (string, error) {
		return "6380 9000000 10000000\n6381 4000000 10000000", nil
	}
	keyFamilyHistoryReplace(t, settings.runtime.baseline, keyFamilyHistoryMetric, []time.Duration{time.Hour, 2 * time.Hour, 3 * time.Hour}, 1000)
	alerts, holds, err := keyFamilyHistoryRun(t, settings)
	if err != nil || len(alerts) != 2 || len(holds) != 0 || len(settings.runtime.baseline.metricSamples[keyFamilyHistoryMetric]) != 3 {
		t.Fatal("partial fleet changed qualified growth, gap or baseline ownership")
	}
	growth := requireAlertClass(t, alerts, "family-growth")
	requireAlertClass(t, alerts, "cannot-observe")
	if !strings.Contains(growth.Observed, "fleet_fullest=unknown") {
		t.Fatal("partial positive lost its fleet-selection qualification")
	}
}

// Direct native-framing fixtures inspect their selected sample separately
// from silent holds for the same fixture's two configured unscanned ports.
func keyFamilyHistoryTargetFindings(t *testing.T, findings []finding, target string) []finding {
	t.Helper()
	selected := []finding{}
	for _, value := range findings {
		switch value.target {
		case target, target + "/key-families":
			selected = append(selected, value)
		case "redis-1:6381", "redis-1:6382":
			if !value.clearHold || value.healthy || value.probeId != "redis/key-families" || value.class != "family-growth" {
				t.Fatal("unscanned fixture node acquired an alert or measured recovery")
			}
		default:
			t.Fatal("family source returned a target outside the fixture's configured census")
		}
	}
	return selected
}

// The source ranks all configured nodes but scans only one. Two healthy scans
// elsewhere cannot clear the physical target of an earlier growth incident.
func TestKeyFamilyHistoryPriorTargetCannotClearWhenFullestNodeChanges(t *testing.T) {
	settings := keyFamilyHistorySettings(t, "30000 ckey_<id>\nfamily_source complete 30000\n", nil)
	otherTarget := "redis-next.example"
	settings.Hosts = []HostSettings{
		{Name: "redis-history.example", Roles: []string{"redis-cluster"}, RedisEntryPort: 6379, RedisNodePorts: []int{6380}},
		{Name: otherTarget, Roles: []string{"redis-cluster"}, RedisNodePorts: []int{6383}},
	}
	if err := settings.Validate(); err != nil {
		t.Fatalf("prior-target fixture has invalid Redis inventory: %v", err)
	}
	stage := 0
	censusCalls := 0
	scanHosts := []string{}
	source := settings.Source.(*syntheticSource)
	source.hostFn = func(host HostSettings, command string) (string, error) {
		censusCalls++
		if !strings.Contains(command, "INFO memory") || (host.Name != "redis-history.example" && host.Name != otherTarget) {
			return "", errors.New("unexpected target-switch census")
		}
		selected := "redis-history.example"
		if stage == 1 || stage == 2 {
			selected = otherTarget
		}
		if host.Name == selected {
			return fmt.Sprintf("%d 900 1000", host.RedisNodePorts[0]), nil
		}
		return fmt.Sprintf("%d 400 1000", host.RedisNodePorts[0]), nil
	}
	source.hostTimeoutFn = func(host HostSettings, command string, _ time.Duration) (string, error) {
		if !strings.Contains(command, "--scan") {
			return "", errors.New("unexpected target-switch scan")
		}
		scanHosts = append(scanHosts, host.Name)
		return "30000 ckey_<id>\nfamily_source complete 30000\n", nil
	}
	notifier, webhook := newJournalRefillNotifier(t)
	postSignalRunsThroughTheGate(t, NewKeyFamiliesSignal(), settings, 5, func(run int) {
		stage = run
		switch run {
		case 0:
			keyFamilyHistoryReplace(t, settings.runtime.baseline, keyFamilyHistoryMetric, []time.Duration{time.Hour, 2 * time.Hour, 3 * time.Hour}, 1000)
		case 1:
			if present, _ := keyFamilyHistoryIncident(notifier, keyFamilyHistoryTarget); !present {
				t.Fatal("prior-target control did not establish actual family-growth")
			}
			keyFamilyHistoryReplace(t, settings.runtime.baseline, keyFamilyHistoryMetric, []time.Duration{time.Hour, 2 * time.Hour, 3 * time.Hour}, 30000)
		case 3:
			if present, absent := keyFamilyHistoryIncident(notifier, keyFamilyHistoryTarget); !present || absent != 0 {
				t.Fatal("key-family prior-target recovery assertion: two scans of another fullest node cleared or credited the unscanned incident")
			}
		case 4:
			if present, absent := keyFamilyHistoryIncident(notifier, keyFamilyHistoryTarget); !present || absent != 1 {
				t.Fatal("reselected qualified target did not earn exactly one recovery credit")
			}
		}
	}, notifier)
	if present, _ := keyFamilyHistoryIncident(notifier, keyFamilyHistoryTarget); present {
		t.Fatal("two qualified scans of the prior target failed to clear")
	}
	wantHosts := "redis-history.example,redis-next.example,redis-next.example,redis-history.example,redis-history.example"
	if censusCalls != 10 || strings.Join(scanHosts, ",") != wantHosts {
		t.Fatal("target recovery added a source read or scanned an unselected node")
	}
	closeSlackNotifier(t, notifier)
	if texts := webhook.posted(); len(texts) != 2 {
		t.Fatal("target holds created an incident or extra notification")
	}
}

// Twenty known shapes may hide many small raw shapes. Because redaction
// merges after top-20 selection, their omitted total can be a material family.
func keyFamilyHistoryOmittedFrame(sampled int) string {
	var output strings.Builder
	for index := 0; index < 20; index++ {
		fmt.Fprintf(&output, "10000 {pm_<id>}sk_%d\n", index)
	}
	fmt.Fprintf(&output, "family_source complete %d\n", sampled)
	return output.String()
}

// OLD and the first candidate must fail this exact prior-incident assertion;
// a valid truncated histogram has no complete redacted-aggregate recovery proof.
func TestKeyFamilyHistoryOmittedRedactedAggregateCannotClearPriorIncident(t *testing.T) {
	initial := "11000 private_shape_a\n11000 private_shape_b\n11000 private_shape_c\nfamily_source complete 33000\n"
	current := initial
	settings := keyFamilyHistorySettings(t, initial, nil)
	settings.Source.(*syntheticSource).hostTimeoutFn = func(HostSettings, string, time.Duration) (string, error) {
		return current, nil
	}
	metric := "redis/family/redacted-unnormalized-family"
	notifier, webhook := newJournalRefillNotifier(t)
	postSignalRunsThroughTheGate(t, NewKeyFamiliesSignal(), settings, 5, func(run int) {
		switch run {
		case 0:
			keyFamilyHistoryReplace(t, settings.runtime.baseline, metric, []time.Duration{time.Hour, 2 * time.Hour, 3 * time.Hour}, 1000)
		case 1:
			if present, _ := keyFamilyHistoryIncident(notifier, keyFamilyHistoryTarget); !present {
				t.Fatal("redacted aggregate did not establish the mature growth incident")
			}
			current = keyFamilyHistoryOmittedFrame(300000)
		case 3:
			if present, absent := keyFamilyHistoryIncident(notifier, keyFamilyHistoryTarget); !present || absent != 0 {
				t.Fatal("key-family omitted-aggregate recovery assertion: two top-20 omissions cleared or credited the unmeasured redacted family")
			}
			current = "10000 private_shape_a\nfamily_source complete 10000\n"
		case 4:
			if present, absent := keyFamilyHistoryIncident(notifier, keyFamilyHistoryTarget); !present || absent != 1 {
				t.Fatal("complete subthreshold aggregate did not earn one recovery credit")
			}
		}
	}, notifier)
	if present, _ := keyFamilyHistoryIncident(notifier, keyFamilyHistoryTarget); present {
		t.Fatal("two complete subthreshold samples did not clear redacted family growth")
	}
	closeSlackNotifier(t, notifier)
	texts := webhook.posted()
	if len(texts) != 2 {
		t.Fatal("omission holds created a notification or discarded qualified recovery")
	}
	for _, text := range texts {
		if strings.Contains(text, "private_shape") {
			t.Fatal("raw omitted-family identity reached notification text")
		}
	}
}

// The raw omitted count is meaningful only after strict framing. A measured
// population at the material floor cannot hide an above-floor aggregate.
func TestKeyFamilyHistoryTypedHeaderPreservesStaticPopulationAuthority(t *testing.T) {
	var small strings.Builder
	for index := 0; index < 20; index++ {
		fmt.Fprintf(&small, "999 private_shape_%d\n", index)
	}
	small.WriteString("family_source complete 20000\n")
	for _, test := range []struct {
		name     string
		frame    string
		wantHold bool
		wantGap  bool
	}{
		{name: "material omission", frame: keyFamilyHistoryOmittedFrame(300000), wantHold: true},
		{name: "fully represented low families", frame: keyFamilyHistoryOmittedFrame(200000)},
		{name: "omitted but measured population at size floor", frame: small.String()},
		{name: "impossible returned total", frame: keyFamilyHistoryOmittedFrame(199999), wantGap: true},
		{name: "negative population", frame: keyFamilyHistoryOmittedFrame(-1), wantGap: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			settings := keyFamilyHistorySettings(t, test.frame, nil)
			alerts, selectedHolds, err := keyFamilyHistoryRun(t, settings)
			if err != nil || len(selectedHolds) > 1 || (len(selectedHolds) == 1) != test.wantHold {
				t.Fatal("raw omitted count changed selected recovery without valid population authority")
			}
			if test.wantGap {
				if len(alerts) != 1 || !alerts[0].unobservable || len(settings.runtime.baseline.metricSamples) != 0 {
					t.Fatal("invalid header supplied coverage, sample or baseline authority")
				}
			} else if len(alerts) != 0 {
				t.Fatal("valid coverage guard changed existing growth thresholds or emitted a new warning")
			}
		})
	}
}
