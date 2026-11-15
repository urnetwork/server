// Canceled backup callbacks cannot change the earlier drive observation's
// remembered decline or the next public unknown-runway classification.
package monitor

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Preserve the actual source callback context for both native source seams.
type minioRunwayRiskSource struct {
	*syntheticSource
	after func(context.Context, HostSettings)
}

func (self *minioRunwayRiskSource) Host(ctx context.Context, host HostSettings, command string) (string, error) {
	output, err := self.syntheticSource.Host(ctx, host, command)
	if self.after != nil {
		self.after(ctx, host)
	}
	return output, err
}

func (self *minioRunwayRiskSource) HostTimeout(ctx context.Context, host HostSettings, command string, timeout time.Duration) (string, error) {
	output, err := self.syntheticSource.HostTimeout(ctx, host, command, timeout)
	if self.after != nil {
		self.after(ctx, host)
	}
	return output, err
}

// Read risk state only to check conservation, independently of the public
// future-severity assertion that discriminates the cause first.
func minioRunwayRiskSnapshot(signal Signal) map[string]storageRunwaySample {
	probe := signal.(*signalAdapter).probe.(*minioRunwayProbe)
	probe.risks.stateLock.Lock()
	defer probe.risks.stateLock.Unlock()
	if probe.risks.volumeRisks == nil {
		return nil
	}
	snapshot := make(map[string]storageRunwaySample, len(probe.risks.volumeRisks))
	for target, sample := range probe.risks.volumeRisks {
		snapshot[target] = sample
	}
	return snapshot
}

func minioRunwayRiskStore(fast bool) minioRunwayTestStore {
	store := minioRunwayTestHealthy()
	if fast {
		store.avail = func(_ int, age time.Duration) float64 {
			return 2000*storageRunwayTestGigabyte + 6*storageRunwayTestGigabyte*age.Hours()
		}
	}
	return store
}

func minioRunwayRiskSources(t *testing.T, now time.Time, store minioRunwayTestStore) minioRunwayTestSources {
	t.Helper()
	return minioRunwayTestSources{now: now, body: minioRunwayTestBody(t, DefaultMinioRunwaySettings().Estimator.grid(now), store)}
}

func minioRunwayRiskUnknown(t *testing.T, signal Signal, now time.Time) Alert {
	t.Helper()
	settings := minioRunwayTestSettings(t, minioRunwayTestSources{now: now})
	settings.Source = &syntheticSource{hostFn: func(HostSettings, string) (string, error) { return "", errors.New("private-source-canary") }}
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal("live unknown observation returned an unexpected error")
	}
	for _, alert := range alerts {
		requireAlertOmits(t, alert, "private-source-canary")
		if alert.Class == "minio-runway-unobservable" && alert.Target == minioRunwayTestHost+"/drives" {
			return alert
		}
	}
	t.Fatal("live unknown observation omitted the drive finding")
	return Alert{}
}

// Cancel only after the gateway's valid range was reduced and the later
// backup-unit source has returned a valid frame; no source error is needed.
func minioRunwayRiskCancelAtUnits(t *testing.T, signal Signal, now time.Time, store minioRunwayTestStore) {
	t.Helper()
	settings := minioRunwayTestSettings(t, minioRunwayRiskSources(t, now, store))
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	cause := errors.New("synthetic-private-minio-risk-cancellation")
	var gatewayCalls, unitCalls atomic.Int32
	var endedAtReturn atomic.Bool
	settings.Source = &minioRunwayRiskSource{syntheticSource: settings.Source.(*syntheticSource), after: func(sourceCtx context.Context, host HostSettings) {
		switch host.Name {
		case "metrics-1":
			gatewayCalls.Add(1)
		case minioRunwayTestHost:
			unitCalls.Add(1)
			cancel(cause)
			endedAtReturn.Store(errors.Is(sourceCtx.Err(), context.Canceled) && context.Cause(sourceCtx) == cause)
		default:
			t.Errorf("risk cancellation control contacted an unexpected source")
		}
	}}
	alerts, err := signal.Run(ctx, settings)
	if gatewayCalls.Load() != 1 || unitCalls.Load() != 1 || !endedAtReturn.Load() || context.Cause(ctx) != cause {
		t.Fatal("control did not cancel its actual parent at the later unit callback")
	}
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 {
		t.Fatal("canceled risk observation published a public result")
	}
	if strings.Contains(err.Error(), "synthetic-private-minio-risk-cancellation") {
		t.Fatal("canceled risk error exposed its private cause")
	}
}

func TestMinioRunwayCanceledFlatCannotReplacePriorRisk(t *testing.T) {
	now := syntheticSettings(nil).Now().UTC()
	signal := NewMinioRunwaySignal()
	minioRunwayTestRun(t, signal, minioRunwayRiskSources(t, now, minioRunwayRiskStore(true)))
	before := minioRunwayRiskSnapshot(signal)
	minioRunwayRiskCancelAtUnits(t, signal, now.Add(15*time.Minute), minioRunwayRiskStore(false))
	after := minioRunwayRiskSnapshot(signal)
	future := minioRunwayRiskUnknown(t, signal, now.Add(30*time.Minute))
	if future.Severity != SeverityPage {
		t.Fatal("canceled flat result changed future unknown drive severity")
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("canceled flat result replaced remembered drive risk")
	}
}

func TestMinioRunwayCanceledDeclineCannotSeedRisk(t *testing.T) {
	now := syntheticSettings(nil).Now().UTC()
	signal := NewMinioRunwaySignal()
	before := minioRunwayRiskSnapshot(signal)
	minioRunwayRiskCancelAtUnits(t, signal, now, minioRunwayRiskStore(true))
	after := minioRunwayRiskSnapshot(signal)
	future := minioRunwayRiskUnknown(t, signal, now.Add(15*time.Minute))
	if future.Severity != SeverityWarn {
		t.Fatal("canceled decline seeded future unknown drive severity")
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("canceled decline installed remembered drive risk")
	}
}

func TestMinioRunwayCanceledDeclineCannotRefreshRisk(t *testing.T) {
	now := syntheticSettings(nil).Now().UTC()
	signal := NewMinioRunwaySignal()
	minioRunwayTestRun(t, signal, minioRunwayRiskSources(t, now, minioRunwayRiskStore(true)))
	before := minioRunwayRiskSnapshot(signal)
	minioRunwayRiskCancelAtUnits(t, signal, now.Add(15*time.Minute), minioRunwayRiskStore(true))
	after := minioRunwayRiskSnapshot(signal)
	future := minioRunwayRiskUnknown(t, signal, now.Add(DefaultMinioRunwaySettings().Estimator.UnknownRiskWindow+time.Minute))
	if future.Severity != SeverityWarn {
		t.Fatal("canceled decline extended future remembered-risk severity")
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("canceled decline refreshed the risk timestamp")
	}
}

func TestMinioRunwayLiveRiskSamplesCommit(t *testing.T) {
	for _, fast := range []bool{false, true} {
		now := syntheticSettings(nil).Now().UTC()
		signal := NewMinioRunwaySignal()
		minioRunwayTestRun(t, signal, minioRunwayRiskSources(t, now, minioRunwayRiskStore(!fast)))
		acceptedAt := now.Add(15 * time.Minute)
		minioRunwayTestRun(t, signal, minioRunwayRiskSources(t, acceptedAt, minioRunwayRiskStore(fast)))
		snapshot := minioRunwayRiskSnapshot(signal)
		sample, ok := snapshot[minioRunwayTestHost+"/drives"]
		if !ok || !sample.at.Equal(acceptedAt) {
			t.Fatal("live completed observation did not commit its risk sample")
		}
		future := minioRunwayRiskUnknown(t, signal, now.Add(30*time.Minute))
		want := SeverityWarn
		if fast {
			want = SeverityPage
		}
		if future.Severity != want {
			t.Fatal("live accepted drive data lost its future unknown severity")
		}
	}
}

func TestMinioRunwayUnitSourceFailureStillCommitsDriveRisk(t *testing.T) {
	now := syntheticSettings(nil).Now().UTC()
	signal := NewMinioRunwaySignal()
	sources := minioRunwayRiskSources(t, now, minioRunwayRiskStore(true))
	sources.unitsErr = errors.New("private-source-canary")
	alerts := minioRunwayTestRun(t, signal, sources)
	for _, alert := range alerts {
		requireAlertOmits(t, alert, "private-source-canary")
	}
	if minioRunwayRiskUnknown(t, signal, now.Add(15*time.Minute)).Severity != SeverityPage {
		t.Fatal("independent unit source loss suppressed accepted drive risk")
	}
}

func TestMinioRunwayMalformedGatewayKeepsPriorRisk(t *testing.T) {
	now := syntheticSettings(nil).Now().UTC()
	signal := NewMinioRunwaySignal()
	minioRunwayTestRun(t, signal, minioRunwayRiskSources(t, now, minioRunwayRiskStore(true)))
	before := minioRunwayRiskSnapshot(signal)
	minioRunwayTestRun(t, signal, minioRunwayTestSources{now: now.Add(15 * time.Minute), body: "{"})
	if !reflect.DeepEqual(before, minioRunwayRiskSnapshot(signal)) {
		t.Fatal("malformed gateway observation replaced prior risk")
	}
	if minioRunwayRiskUnknown(t, signal, now.Add(30*time.Minute)).Severity != SeverityPage {
		t.Fatal("malformed gateway observation lost independent prior severity")
	}
}

func TestMinioRunwayIncompleteCensusRetainsMeasuredRisk(t *testing.T) {
	now := syntheticSettings(nil).Now().UTC()
	signal := NewMinioRunwaySignal()
	store := minioRunwayRiskStore(true)
	store.driveCount = func(time.Duration) float64 { return 5 }
	alerts := minioRunwayTestRun(t, signal, minioRunwayRiskSources(t, now, store))
	found := false
	for _, alert := range alerts {
		if alert.Class == "minio-runway-unobservable" && alert.Target == minioRunwayTestHost+"/drives" && alert.Severity == SeverityPage {
			found = true
		}
	}
	if !found {
		t.Fatal("incomplete census suppressed independently measured fast decline")
	}
	if minioRunwayRiskUnknown(t, signal, now.Add(15*time.Minute)).Severity != SeverityPage {
		t.Fatal("accepted partial census lost measured drive risk")
	}
}

func TestMinioRunwayEndedParentHasNoSourceOrRiskMutation(t *testing.T) {
	now := syntheticSettings(nil).Now().UTC()
	signal := NewMinioRunwaySignal()
	minioRunwayTestRun(t, signal, minioRunwayRiskSources(t, now, minioRunwayRiskStore(true)))
	before := minioRunwayRiskSnapshot(signal)
	settings := minioRunwayTestSettings(t, minioRunwayRiskSources(t, now.Add(15*time.Minute), minioRunwayRiskStore(false)))
	var calls atomic.Int32
	settings.Source = &minioRunwayRiskSource{syntheticSource: settings.Source.(*syntheticSource), after: func(context.Context, HostSettings) { calls.Add(1) }}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	alerts, err := signal.Run(ctx, settings)
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 || calls.Load() != 0 {
		t.Fatal("ended parent entered source or published findings")
	}
	if !reflect.DeepEqual(before, minioRunwayRiskSnapshot(signal)) {
		t.Fatal("ended parent changed remembered risk")
	}
}
