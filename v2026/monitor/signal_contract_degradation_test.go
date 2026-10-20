package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026/model"
)

func contractDegradationTestState(now time.Time, active bool) model.ContractDegradationState {
	state := model.ContractDegradationState{ZeroCost: active, OpenCount: 100, CloseCount: 10, Ratio: 0.1, EvaluatedAt: now}
	if !active {
		state.CloseCount, state.Ratio, state.ConsecutiveHealthyCount = 90, 0.9, 2
	}
	return state
}

func contractDegradationTestFrame(t testing.TB, now time.Time, state model.ContractDegradationState, ttl int64) string {
	t.Helper()
	value, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("contract-degradation-v1\n%d\n%d\npresent\n%d\n%s\ncomplete\n", now.Unix(), now.Nanosecond()/1000, ttl, value)
}

func contractDegradationTestSettings(source SignalSource) SignalSettings {
	settings := syntheticSettings(source)
	settings.ContractDegradationConfig = ContractDegradationConfigEnabled
	return settings
}

func contractDegradationTestRun(t testing.TB, settings SignalSettings) Alerts {
	t.Helper()
	alerts, err := NewContractDegradationSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal("valve control failed before its observation assertion")
	}
	return alerts
}

// Read the exact authoritative value even when a publisher metric could be an
// initialized zero, a stale one, or a superseded sibling's last publication.
func TestContractDegradationActivePublication(t *testing.T) {
	now := syntheticSettings(nil).Now()
	states := []model.ContractDegradationState{
		contractDegradationTestState(now, true),
		{ZeroCost: true, OpenCount: 100, CloseCount: 70, Ratio: 0.7, ConsecutiveHealthyCount: 1, EvaluatedAt: now},
		{ZeroCost: true, OpenCount: 0, CloseCount: 0, Ratio: 0, ConsecutiveHealthyCount: 1, EvaluatedAt: now},
		{ZeroCost: true, OpenCount: 9007199254740993, CloseCount: 0, Ratio: 0, EvaluatedAt: now},
	}
	for index, state := range states {
		reads := 0
		source := &syntheticSource{hostTimeoutFn: func(h HostSettings, command string, timeout time.Duration) (string, error) {
			reads++
			if h.Name != "redis-1" || command != "timeout --kill-after=1s 15s redis-cli -p 6379 -c --raw EVAL_RO "+shellSingleQuote(contractDegradationReadScript)+" 1 "+contractDegradationKey || timeout != 20*time.Second {
				t.Fatal("publication did not use the fixed atomic read-only source")
			}
			return contractDegradationTestFrame(t, now, state, model.ContractDegradationStateTtl.Milliseconds()), nil
		}}
		alerts := contractDegradationTestRun(t, contractDegradationTestSettings(source))
		alert := requireAlertClass(t, alerts, "contract-degradation-active")
		if reads != 1 || len(alerts) != 1 || alert.Severity != SeverityPage || alert.Sustain != 1 || NewContractDegradationSignal().Cadence() != 15*time.Minute || isVisibilityAlert(alert) {
			t.Fatalf("active published valve lost its PAGE1/15m contract, case=%d", index)
		}
		if !strings.Contains(alert.Observed, "open_count="+strconv.FormatInt(state.OpenCount, 10)) {
			t.Fatal("exact integer publication was rounded")
		}
		requireAlertOmits(t, alert, contractDegradationKey)
	}
}

func TestContractDegradationHealthyAndDisabled(t *testing.T) {
	now := syntheticSettings(nil).Now()
	for _, state := range []model.ContractDegradationState{
		contractDegradationTestState(now, false),
		{ZeroCost: false, OpenCount: 100, CloseCount: 70, Ratio: 0.7, ConsecutiveHealthyCount: 1, EvaluatedAt: now},
		{ZeroCost: false, OpenCount: 100, CloseCount: 200, Ratio: 2, ConsecutiveHealthyCount: 2, EvaluatedAt: now},
		{ZeroCost: false, OpenCount: 0, CloseCount: 17, Ratio: 0, ConsecutiveHealthyCount: 2, EvaluatedAt: now},
	} {
		source := &syntheticSource{hostTimeoutFn: func(HostSettings, string, time.Duration) (string, error) {
			return contractDegradationTestFrame(t, now, state, 3000000), nil
		}}
		if alerts := contractDegradationTestRun(t, contractDegradationTestSettings(source)); len(alerts) != 0 {
			t.Fatal("complete charging publication did not grant configured recovery")
		}
	}
	reads := 0
	settings := contractDegradationTestSettings(&syntheticSource{hostTimeoutFn: func(HostSettings, string, time.Duration) (string, error) {
		reads++
		return "", errors.New("unexpected")
	}})
	settings.ContractDegradationConfig = ContractDegradationConfigDisabled
	if alerts := contractDegradationTestRun(t, settings); len(alerts) != 0 || reads != 0 {
		t.Fatal("explicit disabled intent read Redis or lost its narrow configured verdict")
	}
}

func TestContractDegradationMissingPartialMalformed(t *testing.T) {
	now := syntheticSettings(nil).Now()
	valid := contractDegradationTestFrame(t, now, contractDegradationTestState(now, true), 3600000)
	unknown := fmt.Sprintf("contract-degradation-v1\n%d\n0\nmissing\n-2\n-\ncomplete\n", now.Unix())
	values := []string{
		"", "(error) ERR unknown command EVAL_RO private-token", unknown,
		strings.Replace(valid, "\ncomplete\n", "\n", 1), valid + "complete\n",
		strings.Replace(valid, `"open_count":100`, `"open_count":100,"private":"secret-value"`, 1),
		strings.Replace(valid, `"open_count":100`, `"open_count":100,"open_count":100`, 1),
		strings.Replace(valid, `"open_count":100`, `"open_count":-1`, 1),
		strings.Replace(valid, `"open_count":100`, `"open_count":100.0`, 1),
		strings.Replace(valid, `"ratio":0.1`, `"ratio":0.2`, 1),
		strings.Replace(valid, `"consecutive_healthy_count":0`, `"consecutive_healthy_count":2`, 1),
		strings.Replace(valid, `"evaluated_at":`, `"missing_clock":`, 1),
		strings.Replace(valid, `"zero_cost":true`, `"zero_cost":false`, 1),
		strings.Replace(valid, "\n3600000\n", "\n3600000.0\n", 1),
		strings.Replace(valid, "\n0\npresent\n", "\n1000000\npresent\n", 1),
		strings.Repeat("x", 2049),
	}
	overflow := contractDegradationTestState(now, true)
	overflow.OpenCount, overflow.CloseCount, overflow.Ratio = math.MaxInt64, 0, 0
	values = append(values, contractDegradationTestFrame(t, now, overflow, 3600000))
	values = append(values, contractDegradationTestFrame(t, now, model.ContractDegradationState{ZeroCost: true, OpenCount: 100, CloseCount: 70, Ratio: 0.7, ConsecutiveHealthyCount: 2, EvaluatedAt: now}, 3600000))
	for index, value := range values {
		source := &syntheticSource{hostTimeoutFn: func(HostSettings, string, time.Duration) (string, error) { return value, nil }}
		alerts := contractDegradationTestRun(t, contractDegradationTestSettings(source))
		alert := requireAlertClass(t, alerts, "contract-degradation-unobservable")
		if len(alerts) != 1 || alert.Severity != SeverityWarn || alert.Sustain != 2 || !isVisibilityAlert(alert) {
			t.Fatalf("partial publication became health or incident, case=%d", index)
		}
		requireAlertOmits(t, alert, "private-token", "secret-value", "unknown command", contractDegradationKey)
	}
}

func TestContractDegradationClockAndTtlBoundaries(t *testing.T) {
	now := syntheticSettings(nil).Now()
	for _, control := range []struct {
		evaluated  time.Time
		sourceTime time.Time
		completed  time.Time
		ttl        int64
		known      bool
	}{
		{evaluated: now.Add(-time.Hour).Add(time.Nanosecond), sourceTime: now, completed: now, ttl: 1, known: true},
		{evaluated: now.Add(-time.Hour), sourceTime: now, completed: now, ttl: 3600000},
		{evaluated: now.Add(time.Minute), sourceTime: now, completed: now, ttl: 3600000, known: true},
		{evaluated: now.Add(time.Minute).Add(time.Nanosecond), sourceTime: now, completed: now, ttl: 3600000},
		{evaluated: now, sourceTime: now.Add(time.Minute), completed: now, ttl: 3600000, known: true},
		{evaluated: now, sourceTime: now.Add(time.Minute).Add(time.Microsecond), completed: now, ttl: 3600000},
		{evaluated: now, sourceTime: now.Add(-time.Minute).Add(-time.Microsecond), completed: now, ttl: 3600000},
		{evaluated: now, sourceTime: now, completed: now, ttl: -1},
		{evaluated: now, sourceTime: now, completed: now, ttl: 0},
		{evaluated: now, sourceTime: now, completed: now, ttl: 3600001},
		{evaluated: now, sourceTime: now, completed: now.Add(time.Second), ttl: 1000},
		{evaluated: now, sourceTime: now, completed: now.Add(time.Second), ttl: 1001, known: true},
		{evaluated: now, sourceTime: now, completed: now.Add(-time.Nanosecond), ttl: 3600000},
		{evaluated: now, sourceTime: now, completed: now.Add(30 * time.Second).Add(time.Nanosecond), ttl: 3600000},
	} {
		state := contractDegradationTestState(control.evaluated, true)
		raw := contractDegradationTestFrame(t, control.sourceTime, state, control.ttl)
		reads := 0
		source := &syntheticSource{hostTimeoutFn: func(HostSettings, string, time.Duration) (string, error) { reads++; return raw, nil }}
		settings := contractDegradationTestSettings(source)
		clockCalls := 0
		settings.Now = func() time.Time {
			clockCalls++
			if clockCalls == 1 {
				return now
			}
			return control.completed
		}
		alerts := contractDegradationTestRun(t, settings)
		if reads != 1 || (requireAlertClassCount(alerts, "contract-degradation-active") == 1) != control.known || (requireAlertClassCount(alerts, "contract-degradation-unobservable") == 1) == control.known {
			t.Fatal("publication clock/TTL boundary granted the wrong authority")
		}
	}
}

func TestContractDegradationConfigObservation(t *testing.T) {
	for _, document := range []struct {
		raw  string
		want ContractDegradationConfigStatus
	}{
		{raw: "enabled: true\n", want: ContractDegradationConfigEnabled},
		{raw: "enabled: false\n", want: ContractDegradationConfigDisabled},
		{raw: "", want: ContractDegradationConfigUnknown},
		{raw: "{}", want: ContractDegradationConfigUnknown},
		{raw: "enabled: null", want: ContractDegradationConfigUnknown},
		{raw: "enabled: [true]", want: ContractDegradationConfigUnknown},
		{raw: "enabled: true\nprivate: secret-value", want: ContractDegradationConfigUnknown},
		{raw: "enabled: false\nenabled: true", want: ContractDegradationConfigUnknown},
		{raw: "enabled: false\n---\nenabled: true", want: ContractDegradationConfigUnknown},
		{raw: strings.Repeat("x", 4097), want: ContractDegradationConfigUnknown},
	} {
		if got := inspectContractDegradationConfig([]byte(document.raw)); got != document.want {
			t.Fatal("configuration did not preserve explicit intent versus missing/malformed input")
		}
	}
	settings := contractDegradationTestSettings(&syntheticSource{})
	for _, status := range []ContractDegradationConfigStatus{"", ContractDegradationConfigUnknown, "private-token"} {
		settings.ContractDegradationConfig = status
		alert := requireAlertClass(t, contractDegradationTestRun(t, settings), "contract-degradation-unobservable")
		requireAlertOmits(t, alert, "private-token")
	}
	startup := contractDegradationTestSettings(&syntheticSource{})
	latest := startup
	check := NewSettingsGenerationCheck(func() (SignalSettings, error) { return latest, nil })
	if current, err := check(context.Background(), startup); err != nil || !current {
		t.Fatal("identical desired state rejected")
	}
	latest.ContractDegradationConfig = ContractDegradationConfigDisabled
	if current, err := check(context.Background(), startup); err != nil || current {
		t.Fatal("valve config escaped the generation check")
	}
}

type contractDegradationContextSource struct {
	*syntheticSource
	read func(context.Context, HostSettings, string, time.Duration) (string, error)
}

func (self *contractDegradationContextSource) HostTimeout(ctx context.Context, h HostSettings, command string, timeout time.Duration) (string, error) {
	return self.read(ctx, h, command, timeout)
}

func TestContractDegradationGenerationAndCause(t *testing.T) {
	now := syntheticSettings(nil).Now()
	for _, boundary := range []string{"pre-generation", "post-generation", "pre-generation-error", "post-generation-error", "pre-cancel", "source-error", "partial-source-error", "parent-ended", "source-cancel", "source-cancel-nil", "post-cancel", "final-cancel"} {
		ctx, cancel := context.WithCancel(context.Background())
		privateCause := errors.New("private-source-cause")
		reads, checks, clocks := 0, 0, 0
		if boundary == "parent-ended" {
			cancel()
		}
		source := &contractDegradationContextSource{syntheticSource: &syntheticSource{}, read: func(readCtx context.Context, _ HostSettings, _ string, timeout time.Duration) (string, error) {
			reads++
			deadline, bounded := readCtx.Deadline()
			if !bounded || time.Until(deadline) > 20*time.Second || timeout != 20*time.Second {
				t.Fatal("Redis queue/read lost its finite owner")
			}
			if boundary == "source-cancel" {
				cancel()
				return "", privateCause
			}
			if boundary == "source-error" {
				return "", privateCause
			}
			raw := contractDegradationTestFrame(t, now, contractDegradationTestState(now, true), 3600000)
			if boundary == "source-cancel-nil" {
				cancel()
				return raw, nil
			}
			if boundary == "partial-source-error" {
				return raw, privateCause
			}
			return raw, nil
		}}
		settings := contractDegradationTestSettings(source)
		settings.SettingsGenerationCheck = func(context.Context, SignalSettings) (bool, error) {
			checks++
			if boundary == "pre-generation" || boundary == "post-generation" && checks == 2 {
				return false, nil
			}
			if boundary == "pre-generation-error" || boundary == "post-generation-error" && checks == 2 {
				return false, privateCause
			}
			if boundary == "pre-cancel" {
				cancel()
				return false, privateCause
			}
			if boundary == "post-cancel" && checks == 2 {
				cancel()
				return false, privateCause
			}
			return true, nil
		}
		settings.Now = func() time.Time {
			clocks++
			if boundary == "final-cancel" && clocks == 2 {
				cancel()
			}
			return now
		}
		alerts, err := NewContractDegradationSignal().Run(ctx, settings)
		ended := boundary == "parent-ended" || boundary == "pre-cancel" || boundary == "source-cancel" || boundary == "source-cancel-nil" || boundary == "post-cancel" || boundary == "final-cancel"
		if ended {
			if !errors.Is(err, context.Canceled) || len(alerts) != 0 {
				t.Fatal("ended owner returned product or healthy findings")
			}
			if (boundary == "pre-cancel" || boundary == "source-cancel" || boundary == "post-cancel") && !errors.Is(err, privateCause) {
				t.Fatal("observed source cause was discarded")
			}
			if strings.Contains(err.Error(), privateCause.Error()) {
				t.Fatal("private cause rendered in lifecycle error")
			}
		} else {
			if err != nil {
				t.Fatal("live parent lost its component visibility")
			}
			alert := requireAlertClass(t, alerts, "contract-degradation-unobservable")
			requireAlertOmits(t, alert, privateCause.Error())
		}
		if (boundary == "pre-generation" || boundary == "pre-generation-error" || boundary == "pre-cancel" || boundary == "parent-ended") && reads != 0 {
			t.Fatal("source read started beyond a refused boundary")
		}
		if (boundary == "source-error" || boundary == "source-cancel" || boundary == "source-cancel-nil" || boundary == "partial-source-error") && checks != 1 {
			t.Fatal("dependent read followed failed source")
		}
		cancel()
	}
}

func TestContractDegradationExcludedRedisAndMissingInventory(t *testing.T) {
	for _, excluded := range []bool{false, true} {
		reads := 0
		settings := contractDegradationTestSettings(&syntheticSource{hostTimeoutFn: func(HostSettings, string, time.Duration) (string, error) { reads++; return "", nil }})
		if excluded {
			settings.ExcludedHosts = []string{"redis-1"}
		} else {
			settings.Hosts = settings.Hosts[:1]
		}
		alerts := contractDegradationTestRun(t, settings)
		if reads != 0 || len(alerts) == 0 || requireAlertClassCount(alerts, "contract-degradation-active") != 0 {
			t.Fatal("missing or excluded Redis supplied authority")
		}
	}
}

// Unknown publications preserve an absent incident's existing recovery credit.
// Only complete observations advance it; a held visibility warning still resets
// that warning's own credit without granting recovery to the active incident.
func TestContractDegradationNotifierRetainsUnknown(t *testing.T) {
	var run, reads atomic.Int32
	now := syntheticSettings(nil).Now()
	source := &syntheticSource{hostTimeoutFn: func(HostSettings, string, time.Duration) (string, error) {
		reads.Add(1)
		i := run.Load()
		at := now.Add(time.Duration(i) * 15 * time.Minute)
		if i == 1 || i == 4 {
			return fmt.Sprintf("contract-degradation-v1\n%d\n0\nmissing\n-2\n-\ncomplete\n", at.Unix()), nil
		}
		state := contractDegradationTestState(at, i == 0 || i == 2)
		if i == 2 {
			state.EvaluatedAt = at.Add(-time.Hour)
		}
		return contractDegradationTestFrame(t, at, state, 3600000), nil
	}}
	settings := contractDegradationTestSettings(source)
	settings.Now = func() time.Time { return now.Add(time.Duration(run.Load()) * 15 * time.Minute) }
	signal := NewContractDegradationSignal()
	webhook, url := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(url))
	t.Cleanup(func() { closeSlackNotifier(t, notifier) })
	if notifier.settings.ClearAfterCompleteRuns != 2 {
		t.Fatal("fixture changed production recovery policy")
	}
	assertIncident := func(completedRuns int, class string, present bool, credit int) {
		t.Helper()
		notifier.stateLock.Lock()
		defer notifier.stateLock.Unlock()
		key := slackIncidentKey(Alert{SignalID: signal.ID(), Class: class, Target: "contract-degradation"})
		incident, found := notifier.keyIncidents[key]
		if found != present {
			t.Fatalf("completed_runs=%d class=%s present=%t, want %t", completedRuns, class, found, present)
		}
		if found {
			absenceCredit, owned := incident.signalKeyAbsentRuns[signal.Key()]
			if !owned || absenceCredit != credit {
				t.Fatalf("completed_runs=%d class=%s absence_credit=%d owned=%t, want credit=%d", completedRuns, class, absenceCredit, owned, credit)
			}
		}
	}
	expectedStates := []struct {
		pagePresent, unknownPresent bool
		pageCredit, unknownCredit   int
	}{
		{},
		{pagePresent: true}, // active run 0 raises PAGE immediately
		{pagePresent: true}, // missing run 1 is held below WARN sustain
		{pagePresent: true, unknownPresent: true},                                  // stale run 2 raises WARN
		{pagePresent: true, pageCredit: 1, unknownPresent: true, unknownCredit: 1}, // healthy run 3
		{pagePresent: true, pageCredit: 1, unknownPresent: true},                   // missing run 4 preserves PAGE credit
		{unknownPresent: true, unknownCredit: 1},                                   // healthy run 5 is the second complete PAGE absence
		{},                                                                         // healthy run 6 clears the independently observed WARN
	}
	assertRun := func(completedRuns int) {
		t.Helper()
		if observed := int(reads.Load()); observed != completedRuns {
			t.Fatalf("completed_runs=%d source_reads=%d, want one read per completed run", completedRuns, observed)
		}
		expected := expectedStates[completedRuns]
		assertIncident(completedRuns, "contract-degradation-active", expected.pagePresent, expected.pageCredit)
		assertIncident(completedRuns, "contract-degradation-unobservable", expected.unknownPresent, expected.unknownCredit)
		t.Logf("completed_runs=%d active_present=%t active_credit=%d unknown_present=%t unknown_credit=%d", completedRuns, expected.pagePresent, expected.pageCredit, expected.unknownPresent, expected.unknownCredit)
	}
	postSignalRunsThroughTheGate(t, signal, settings, 7, func(i int) {
		// The previous Handle has returned before this barrier, so assertions
		// do not depend on the asynchronous webhook sender's progress.
		assertRun(i)
		run.Store(int32(i))
		if i > 0 {
			harness.advance(15 * time.Minute)
		}
	}, notifier)
	assertRun(7)
	closeSlackNotifier(t, notifier)
	posts := webhook.posted()
	pageIdentity := "\n" + signal.ID() + " · contract-degradation-active · contract-degradation\n"
	unknownIdentity := "\n" + signal.ID() + " · contract-degradation-unobservable · contract-degradation\n"
	pageRaised, pageCleared := countSlackPosts(posts, "[PAGE] raised", pageIdentity), countSlackPosts(posts, "[CLEARED] PAGE", pageIdentity)
	unknownRaised, unknownCleared := countSlackPosts(posts, "[WARN] raised", unknownIdentity), countSlackPosts(posts, "[CLEARED] WARN", unknownIdentity)
	if pageRaised != 1 || pageCleared != 1 || unknownRaised != 1 || unknownCleared != 1 || len(posts) != 4 {
		t.Fatalf("active_raised=%d active_cleared=%d unknown_raised=%d unknown_cleared=%d total=%d, want one of each transition", pageRaised, pageCleared, unknownRaised, unknownCleared, len(posts))
	}
	t.Logf("active_raised=%d active_cleared=%d unknown_raised=%d unknown_cleared=%d", pageRaised, pageCleared, unknownRaised, unknownCleared)
}
