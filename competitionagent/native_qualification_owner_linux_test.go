// Virtual time and explicit barriers exercise collection lifetime/cleanup;
// injected observations are synthetic and cannot admit any native class.
package competitionagent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// No value below is a host identity, installed path, or operational receipt.
func nativeQualificationOwnerTestValues(t *testing.T) (string, RuntimeNativeKernelIdentity, RuntimeResourceBoundary) {
	t.Helper()
	fixture := newRuntimeNativeClassTest(t)
	fixture.boundary.NativeClass.Receipt = Pin{}
	identity, err := prospectiveNativeQualificationIdentity(fixture.boundary)
	if err != nil {
		t.Fatal(err)
	}
	return identity, fixture.host.kernel, fixture.boundary
}

// Caller-private facts are value snapshots; no acceptance field is produced.
func nativeQualificationOwnerTestObservation(name, identity string, kernel RuntimeNativeKernelIdentity) nativeQualificationObservation {
	return nativeQualificationObservation{Name: name, RuntimeIdentitySha256: identity, Kernel: kernel, Outcome: "observed", ArtifactManifest: Pin{Path: "/etc/synthetic-qualification/" + name + ".json", Sha256: strings.Repeat("4", 64)}, ArtifactBytes: 1}
}

// All12 observations stay explicitly outside the old native qualification and
// acceptance formats, even when every synthetic operation returns normally.
func TestNativeQualificationOwnerNeverConvertsObservationsIntoAdmission(t *testing.T) {
	identity, kernel, boundary := nativeQualificationOwnerTestValues(t)
	var names []string
	cleanups := 0
	result, err := collectNativeQualificationObservations(context.Background(), identity, kernel, boundary, defaultNativeQualificationOwnerSettings(), func(_ context.Context, name string) (nativeQualificationObservation, error) {
		names = append(names, name)
		return nativeQualificationOwnerTestObservation(name, identity, kernel), nil
	}, func(context.Context) error { cleanups++; return nil })
	if err != nil || !result.AllTrialsObserved || !result.OrdinaryJoined || !result.CleanupJoined || len(names) != len(runtimeNativeTrialNames()) || cleanups != 1 || result.Accepted || result.RuntimeQualified {
		t.Fatal("collection widened authority or lost its joined owners", result, err)
	}
	for index, name := range runtimeNativeTrialNames() {
		if names[index] != name || result.Trials[index].Name != name {
			t.Fatal("closed trial order changed")
		}
	}
	content, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var admission runtimeNativeClassQualification
	if decodeStrict(content, &admission) == nil || runtimeNativeDocumentIdentity(result.Schema, result.Kind, "runtime-native-class-qualification/v1", runtimeNativeClassProfile, identity, identity) {
		t.Fatal("observations became an accepted native format")
	}
}

// Every possible unsupported position retains its exact stopping boundary;
// later trials never execute, and cleanup is still one independent join.
func TestNativeQualificationOwnerRefusesEveryPartialUnsupportedSequence(t *testing.T) {
	for stop, name := range runtimeNativeTrialNames() {
		identity, kernel, boundary := nativeQualificationOwnerTestValues(t)
		calls, cleanups := 0, 0
		result, err := collectNativeQualificationObservations(context.Background(), identity, kernel, boundary, defaultNativeQualificationOwnerSettings(), func(_ context.Context, current string) (nativeQualificationObservation, error) {
			calls++
			if current == name {
				return nativeQualificationObservation{}, errNativeQualificationUnsupported
			}
			return nativeQualificationOwnerTestObservation(current, identity, kernel), nil
		}, func(context.Context) error { cleanups++; return nil })
		if !errors.Is(err, errNativeQualificationUnsupported) || result.AllTrialsObserved || result.StoppedTrial != name || result.StopOutcome != "unsupported" || calls != stop+1 || len(result.Trials) != stop || cleanups != 1 || !result.OrdinaryJoined || !result.CleanupJoined {
			t.Fatal("unsupported trial was lost/retried/admitted", stop, result, err)
		}
	}
}

// Cleanup dispatches while the ordinary operation is still unwinding under a
// canceled owner. The result cannot escape before both barriers have joined.
func TestNativeQualificationOwnerDeadlineDispatchesIndependentJoinedCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		identity, kernel, boundary := nativeQualificationOwnerTestValues(t)
		settings := defaultNativeQualificationOwnerSettings()
		settings.TotalTimeout = 100 * time.Millisecond
		cleanupEntered := make(chan struct{})
		ordinaryReleased := make(chan struct{})
		finishCleanup := make(chan struct{})
		type outcome struct {
			value nativeQualificationCollectionObservation
			err   error
		}
		result := make(chan outcome, 1)
		go func() {
			value, err := collectNativeQualificationObservations(context.Background(), identity, kernel, boundary, settings, func(ctx context.Context, name string) (nativeQualificationObservation, error) {
				<-ctx.Done()
				<-ordinaryReleased
				return nativeQualificationOwnerTestObservation(name, identity, kernel), ctx.Err()
			}, func(ctx context.Context) error {
				if ctx.Err() != nil {
					return errors.New("cleanup inherited cancellation")
				}
				close(cleanupEntered)
				close(ordinaryReleased)
				<-finishCleanup
				return ctx.Err()
			})
			result <- outcome{value: value, err: err}
		}()
		<-cleanupEntered
		synctest.Wait()
		select {
		case <-result:
			t.Fatal("unjoined cleanup escaped collection")
		default:
		}
		close(finishCleanup)
		got := <-result
		if !errors.Is(got.err, context.DeadlineExceeded) || got.value.AllTrialsObserved || !got.value.OrdinaryJoined || !got.value.CleanupJoined || got.value.StopOutcome != "owner-expired" {
			t.Fatal("deadline or independent joined cleanup was lost", got.value, got.err)
		}
	})
}

// A late nil return cannot erase the original trial deadline, and aggregate
// evidence cannot grow by resetting its allowance at each observation.
func TestNativeQualificationOwnerRefusesLateFactsAndEvidenceAmplification(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		identity, kernel, boundary := nativeQualificationOwnerTestValues(t)
		settings := defaultNativeQualificationOwnerSettings()
		settings.TrialTimeout = 100 * time.Millisecond
		result, err := collectNativeQualificationObservations(context.Background(), identity, kernel, boundary, settings, func(ctx context.Context, name string) (nativeQualificationObservation, error) {
			<-ctx.Done()
			return nativeQualificationOwnerTestObservation(name, identity, kernel), nil
		}, func(context.Context) error { return nil })
		if !errors.Is(err, context.DeadlineExceeded) || result.AllTrialsObserved || len(result.Trials) != 1 {
			t.Fatal("late nil facts acquired fresh authority", result, err)
		}
	})
	identity, kernel, boundary := nativeQualificationOwnerTestValues(t)
	settings := defaultNativeQualificationOwnerSettings()
	settings.EvidenceBytes = 1
	result, err := collectNativeQualificationObservations(context.Background(), identity, kernel, boundary, settings, func(_ context.Context, name string) (nativeQualificationObservation, error) {
		return nativeQualificationOwnerTestObservation(name, identity, kernel), nil
	}, func(context.Context) error { return nil })
	if err == nil || result.AllTrialsObserved || len(result.Trials) != 1 {
		t.Fatal("per-trial reset amplified aggregate evidence", result, err)
	}
}

// An earlier trial deadline must dispatch the shared independent cleanup;
// waiting for the much later total owner would deadlock this real join shape.
func TestNativeQualificationOwnerTrialDeadlineDispatchesBeforeTotalDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		identity, kernel, boundary := nativeQualificationOwnerTestValues(t)
		settings := defaultNativeQualificationOwnerSettings()
		settings.TotalTimeout = time.Hour
		settings.TrialTimeout = 100 * time.Millisecond
		released := make(chan struct{})
		cleanups := 0
		started := time.Now()
		result, err := collectNativeQualificationObservations(context.Background(), identity, kernel, boundary, settings, func(ctx context.Context, name string) (nativeQualificationObservation, error) {
			<-ctx.Done()
			<-released
			return nativeQualificationOwnerTestObservation(name, identity, kernel), ctx.Err()
		}, func(ctx context.Context) error {
			cleanups++
			if ctx.Err() != nil {
				return ctx.Err()
			}
			close(released)
			return nil
		})
		if !errors.Is(err, context.DeadlineExceeded) || result.AllTrialsObserved || cleanups != 1 || time.Since(started) != 100*time.Millisecond || !result.OriginalDeadline.Equal(started.Add(time.Hour)) || !result.OrdinaryJoined || !result.CleanupJoined {
			t.Fatal("trial expiry waited for total deadline or reset its owner", result, err, time.Since(started))
		}
	})
}

// Cleanup failure and its own expired deadline invalidate a complete ordinary
// sequence; a pre-canceled owner dispatches neither trials nor cleanup work.
func TestNativeQualificationOwnerRefusesFailedCleanupAndPrecanceledDispatch(t *testing.T) {
	identity, kernel, boundary := nativeQualificationOwnerTestValues(t)
	sentinel := errors.New("synthetic independent cleanup failure")
	run := func(_ context.Context, name string) (nativeQualificationObservation, error) {
		return nativeQualificationOwnerTestObservation(name, identity, kernel), nil
	}
	result, err := collectNativeQualificationObservations(context.Background(), identity, kernel, boundary, defaultNativeQualificationOwnerSettings(), run, func(context.Context) error { return sentinel })
	if !errors.Is(err, sentinel) || result.AllTrialsObserved || len(result.Trials) != 12 || !result.CleanupJoined {
		t.Fatal("cleanup failure was waived", result, err)
	}
	synctest.Test(t, func(t *testing.T) {
		settings := defaultNativeQualificationOwnerSettings()
		settings.CleanupTimeout = 100 * time.Millisecond
		result, err := collectNativeQualificationObservations(context.Background(), identity, kernel, boundary, settings, run, func(ctx context.Context) error { <-ctx.Done(); return nil })
		if !errors.Is(err, context.DeadlineExceeded) || result.AllTrialsObserved || !result.CleanupJoined {
			t.Fatal("late nil cleanup acquired success", result, err)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	result, err = collectNativeQualificationObservations(ctx, identity, kernel, boundary, defaultNativeQualificationOwnerSettings(), func(context.Context, string) (nativeQualificationObservation, error) {
		calls++
		return nativeQualificationObservation{}, nil
	}, func(context.Context) error { calls++; return nil })
	if !errors.Is(err, context.Canceled) || calls != 0 || result.AllTrialsObserved || result.OrdinaryJoined || result.CleanupJoined {
		t.Fatal("pre-canceled owner dispatched new work", result, err, calls)
	}
}

// An authenticated failed manifest remains forensic evidence; changing the
// overall outcome never discards it or turns the trial into a positive record.
func TestNativeQualificationOwnerRetainsFailedObservationArtifacts(t *testing.T) {
	identity, kernel, boundary := nativeQualificationOwnerTestValues(t)
	sentinel := errors.New("synthetic retained operation failure")
	result, err := collectNativeQualificationObservations(context.Background(), identity, kernel, boundary, defaultNativeQualificationOwnerSettings(), func(_ context.Context, name string) (nativeQualificationObservation, error) {
		value := nativeQualificationOwnerTestObservation(name, identity, kernel)
		value.Outcome = "failed"
		return value, sentinel
	}, func(context.Context) error { return nil })
	if !errors.Is(err, sentinel) || result.AllTrialsObserved || len(result.Trials) != 1 || result.Trials[0].ArtifactManifest.Sha256 == "" || result.Trials[0].Outcome != "failed" {
		t.Fatal("terminal failure discarded its retained artifact", result, err)
	}
}
