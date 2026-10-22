// Public proxy-memory controls preserve UDP history at an already-canceled publication checkpoint.
package monitor

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"strings"
	"testing"
	"time"
)

// One same-instance timeline returns its whole private-map transition and next public finding.
func proxyUdpCancellationTimeline(t *testing.T, includeCanceled bool) (map[string]proxyUDPCounterSample, map[string]proxyUDPCounterSample, Alerts) {
	t.Helper()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sample := proxyMemoryDeltaFixtureSample()
	var cancelDuringSource context.CancelFunc
	calls := 0
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		calls++
		if !strings.Contains(command, proxyMemoryMarker) {
			t.Fatal("UDP cancellation control reached another producer")
		}
		if cancelDuringSource != nil {
			cancelDuringSource()
		}
		return proxyMemoryFixture(sample), nil
	}}
	settings := proxyMemorySyntheticSettings(source)
	settings.Now = func() time.Time { return now }
	signal := NewProxyMemorySignal()
	probe := signal.(*signalAdapter).probe.(*proxyMemoryProbe)
	probe.udpSamples["synthetic-unrelated"] = proxyUDPCounterSample{bootID: sample.bootID, observedAt: now, inDatagrams: 73, rcvbufErrors: 11}
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 {
		t.Fatal("first live UDP frame did not remain healthy warmup")
	}
	before := maps.Clone(probe.udpSamples)
	sample.udpInDatagrams, sample.udpRcvbufErrors = 1000, 1000
	after := maps.Clone(before)
	if includeCanceled {
		now = now.Add(time.Minute)
		ctx, cancel := context.WithCancel(context.Background())
		cancelDuringSource = cancel
		alerts, err = signal.Run(ctx, settings)
		cancel()
		cancelDuringSource = nil
		if !errors.Is(err, context.Canceled) || len(alerts) != 0 {
			t.Fatal("late valid source frame escaped canceled public execution")
		}
		after = maps.Clone(probe.udpSamples)
		now = now.Add(time.Minute)
	} else {
		now = now.Add(2 * time.Minute)
	}
	expectedCalls := 2
	if includeCanceled {
		expectedCalls++
	}
	alerts, err = signal.Run(context.Background(), settings)
	if err != nil || calls != expectedCalls {
		t.Fatal("accepted endpoint did not execute exactly once per requested frame")
	}
	return before, after, alerts
}

// A valid late frame rejected by public Run cannot alter any cached host entry.
func TestProxyMemoryCanceledUdpFramePreservesWholeBaseline(t *testing.T) {
	before, after, _ := proxyUdpCancellationTimeline(t, true)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("canceled valid UDP frame changed the private baseline map")
	}
}

// Skipping a canceled middle observation retains the loss visible between accepted endpoints.
func TestProxyMemoryCanceledUdpFramePreservesNextPublicDelta(t *testing.T) {
	_, _, actual := proxyUdpCancellationTimeline(t, true)
	_, _, expected := proxyUdpCancellationTimeline(t, false)
	if len(actual) != 1 || len(expected) != 1 {
		t.Fatal("canceled middle UDP frame consumed the accepted-endpoint loss finding")
	}
	got := requireAlertClass(t, actual, "proxy-udp-receive-drops")
	want := requireAlertClass(t, expected, "proxy-udp-receive-drops")
	if got.Severity != SeverityPage || got.Identity() != want.Identity() || got.Observed != want.Observed || got.Severity != want.Severity {
		t.Fatal("accepted UDP endpoints diverged from the no-middle-frame public counterfactual")
	}
	for _, field := range []string{"sample_elapsed_s=120 ", "udp_in_datagrams_delta=1000 ", "udp_rcvbuf_errors_delta=1000 ", "udp_rcvbuf_errors_rate_per_min=500.00 ", "udp_receive_drop_ratio_pct=50.0000 ", "udp_in_datagrams_before=0 ", "udp_in_datagrams_after=1000 ", "udp_rcvbuf_errors_before=0 ", "udp_rcvbuf_errors_after=1000 "} {
		if !strings.Contains(got.Observed, field) {
			t.Fatal("retained UDP interval lost an expected endpoint, rate or ratio")
		}
	}
}

// Cancellation precedes initialization, invalid-cache eviction and every baseline replacement.
func TestProxyMemoryCanceledUdpFramePreservesNilAndInvalidCache(t *testing.T) {
	for _, seed := range []map[string]proxyUDPCounterSample{nil, {}, {"proxy-1": {bootID: "synthetic-invalid", inDatagrams: 8, rcvbufErrors: 2}}} {
		ctx, cancel := context.WithCancel(context.Background())
		source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
			cancel()
			return proxyMemoryFixture(proxyMemoryDeltaFixtureSample()), nil
		}}
		signal := NewProxyMemorySignal()
		probe := signal.(*signalAdapter).probe.(*proxyMemoryProbe)
		probe.udpSamples = seed
		before := maps.Clone(seed)
		alerts, err := signal.Run(ctx, proxyMemorySyntheticSettings(source))
		cancel()
		if !errors.Is(err, context.Canceled) || len(alerts) != 0 || !reflect.DeepEqual(before, probe.udpSamples) {
			t.Fatal("canceled frame initialized or evicted private UDP state")
		}
	}
}

// A later-host cancellation does not promise transactional rollback of earlier accepted hosts.
func TestProxyMemoryUdpPerHostCheckpointKeepsEarlierLiveCommit(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sample := proxyMemoryDeltaFixtureSample()
	var cancelOnSecond context.CancelFunc
	source := &syntheticSource{hostFn: func(host HostSettings, _ string) (string, error) {
		if host.Name == "proxy-2" && cancelOnSecond != nil {
			cancelOnSecond()
		}
		return proxyMemoryFixture(sample), nil
	}}
	settings := proxyMemorySyntheticSettings(source)
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "proxy-2", Roles: []string{"services"}, Proxy: &ProxyHostSettings{PublicHostname: "proxy-two.example"}})
	settings.Now = func() time.Time { return now }
	signal := NewProxyMemorySignal()
	if alerts, err := signal.Run(context.Background(), settings); err != nil || len(alerts) != 0 {
		t.Fatal("two-host baseline did not warm up")
	}
	probe := signal.(*signalAdapter).probe.(*proxyMemoryProbe)
	before := maps.Clone(probe.udpSamples)
	now = now.Add(time.Minute)
	sample.udpInDatagrams, sample.udpRcvbufErrors = 1000, 1000
	ctx, cancel := context.WithCancel(context.Background())
	cancelOnSecond = cancel
	alerts, err := signal.Run(ctx, settings)
	cancel()
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 || len(probe.udpSamples) != len(before) {
		t.Fatal("multi-host canceled execution escaped public lifecycle or map scope")
	}
	first := probe.udpSamples["proxy-1"]
	if first.inDatagrams != 1000 || first.rcvbufErrors != 1000 || first.observedAt != now || probe.udpSamples["proxy-2"] != before["proxy-2"] {
		t.Fatal("per-host checkpoint either rolled back earlier live evidence or accepted canceled-host history")
	}
}

// Context method callbacks must run outside the baseline mutex, including custom contexts.
type proxyUdpLockCheckingContext struct {
	context.Context
	probe *proxyMemoryProbe
	calls int
}

// TryLock makes a misplaced context callback fail immediately without goroutines or sleeps.
func (self *proxyUdpLockCheckingContext) Done() <-chan struct{} {
	if !self.probe.udpLock.TryLock() {
		panic("context callback invoked under UDP baseline lock")
	}
	self.probe.udpLock.Unlock()
	self.calls++
	return self.Context.Done()
}

// The actual public commit path uses the captured channel rather than calling context under lock.
func TestProxyMemoryUdpContextCallbackOutsideBaselineLock(t *testing.T) {
	signal := NewProxyMemorySignal()
	probe := signal.(*signalAdapter).probe.(*proxyMemoryProbe)
	ctx := &proxyUdpLockCheckingContext{Context: context.Background(), probe: probe}
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		return proxyMemoryFixture(proxyMemoryDeltaFixtureSample()), nil
	}}
	alerts, err := signal.Run(ctx, proxyMemorySyntheticSettings(source))
	if err != nil || len(alerts) != 0 || ctx.calls == 0 || len(probe.udpSamples) != 1 {
		t.Fatal("live publication did not use an unlocked context callback and retain warmup")
	}
}

// Rejecting a canceled frame does not disable independent live memory findings or future UDP loss.
func TestProxyMemoryUdpAfterCanceledFrameRetainsIndependentMemory(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sample := proxyMemoryDeltaFixtureSample()
	var cancelDuringSource context.CancelFunc
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		if cancelDuringSource != nil {
			cancelDuringSource()
		}
		return proxyMemoryFixture(sample), nil
	}}
	settings := proxyMemorySyntheticSettings(source)
	settings.Now = func() time.Time { return now }
	signal := NewProxyMemorySignal()
	if _, err := signal.Run(context.Background(), settings); err != nil {
		t.Fatal("independent-memory baseline failed")
	}
	now = now.Add(time.Minute)
	sample.udpInDatagrams, sample.udpRcvbufErrors = 1000, 1000
	ctx, cancel := context.WithCancel(context.Background())
	cancelDuringSource = cancel
	if alerts, err := signal.Run(ctx, settings); !errors.Is(err, context.Canceled) || len(alerts) != 0 {
		t.Fatal("canceled memory frame escaped public lifecycle")
	}
	cancel()
	cancelDuringSource = nil
	now = now.Add(time.Minute)
	sample.memAvailableKiB = 1024 * 1024
	sample.proxyProcesses = sample.runningUnits + 1
	sample.proxyMemoryUnbounded = sample.proxyProcesses
	sample.recentProxyOOMKills = 1
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal("live independent memory frame failed")
	}
	for _, class := range []string{"proxy-host-oom", "proxy-rollout-overlap", "proxy-udp-receive-drops"} {
		if requireAlertClass(t, alerts, class).Severity != SeverityPage {
			t.Fatal("independent memory/UDP page was suppressed")
		}
	}
}
