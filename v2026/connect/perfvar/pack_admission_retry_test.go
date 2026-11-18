package perfvar

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// This is a proposed test-contract change, not a production bug regression:
// genuine native pre-Transfer pure-ACK pressure stays in the raw census while
// admitted, generic and unknown failures remain fatal through the actual gate.
func TestMeasuredNativeReadyPureAckRefusalAccountingContract(t *testing.T) {
	for _, test := range []struct {
		name                                        string
		alter                                       func(*clientconnect.SendPackAdmissionError)
		firstSuccess, firstUnknown, terminalUnknown bool
		optedOut, extraFailure, upstreamRecoverable bool
		noAck, wrongDirection, healthProbe          bool
		firstNoProvenance, terminalNoProvenance     bool
		wantNative                                  uint64
		wantFailure                                 bool
	}{
		{name: "proven native refusal", wantNative: 1},
		{name: "not opted in", optedOut: true, wantNative: 1, wantFailure: true},
		{name: "no Transfer ACK", noAck: true, wantFailure: true},
		{name: "wrong message direction", wrongDirection: true, wantFailure: true},
		{name: "qualification probe is independent", healthProbe: true},
		{name: "terminal provenance alone", firstNoProvenance: true, wantFailure: true},
		{name: "first provenance alone", terminalNoProvenance: true, wantFailure: true},
		{name: "generic zero timeout", alter: func(e *clientconnect.SendPackAdmissionError) { e.NativeReadyPureTcpAck = false }, wantFailure: true},
		{name: "wrong boundary", alter: func(e *clientconnect.SendPackAdmissionError) { e.Boundary = "resend-capacity" }, wantFailure: true},
		{name: "positive timeout", alter: func(e *clientconnect.SendPackAdmissionError) { e.Timeout = time.Second }, wantFailure: true},
		{name: "unknown cause", alter: func(e *clientconnect.SendPackAdmissionError) { e.Err = errors.New("unknown cause") }, wantFailure: true},
		{name: "tracking overflow", alter: func(e *clientconnect.SendPackAdmissionError) { e.OwnerTrackingOverflow = true }, wantFailure: true},
		{name: "written group prefix then refused tail", firstSuccess: true, wantFailure: true},
		{name: "first writer evidence unknown", firstUnknown: true, wantFailure: true},
		{name: "terminal unknown", terminalUnknown: true, wantFailure: true},
		{name: "upstream recovery is not native evidence", upstreamRecoverable: true, wantFailure: true},
		{name: "legal refusal cannot cover unrelated admitted error", extraFailure: true, wantNative: 1, wantFailure: true},
		{name: "same input recovered keeps existing category", alter: func(e *clientconnect.SendPackAdmissionError) { e.RecoveredByOwner = true }},
	} {
		tracker := newSendPackLifecycleTracker()
		observer := tracker.newObserver()
		path := &fullTunPath{devicePackSends: tracker, activePackFailureFloor: &perfvarPackFailureCounts{},
			allowDeviceNativeAckPressure: !test.optedOut}
		detail := &clientconnect.SendPackAdmissionError{Boundary: "pack-admission", NativeReadyPureTcpAck: true, Err: clientconnect.ErrSendPackNotAdmitted}
		if test.alter != nil {
			test.alter(detail)
		}
		var firstErr, terminalErr error = detail, detail
		if test.firstNoProvenance {
			copy := *detail
			copy.NativeReadyPureTcpAck = false
			firstErr = &copy
		}
		if test.terminalNoProvenance {
			copy := *detail
			copy.NativeReadyPureTcpAck = false
			terminalErr = &copy
		}
		if test.firstSuccess {
			firstErr = nil
		} else if test.firstUnknown {
			firstErr = clientconnect.ErrSendPackNotAdmitted
		}
		if test.terminalUnknown {
			terminalErr = errors.New("admitted delivery failed")
		}
		observation := clientconnect.SendPackLifecycleObservation{
			ClientId: clientconnect.NewId(), DestinationId: clientconnect.NewId(), Token: 1,
			AckRequired: !test.noAck, UpstreamRecoverable: test.upstreamRecoverable, HealthProbe: test.healthProbe,
			MessageType: protocol.MessageType_IpIpPacketToProvider,
		}
		if test.wrongDirection {
			observation.MessageType = protocol.MessageType_IpIpPacketFromProvider
		}
		publish := func(token uint64, first, terminal error) {
			observation.Token, observation.Phase, observation.Err = token, clientconnect.SendPackLifecyclePhaseStarted, nil
			observer(observation)
			observation.Phase, observation.Err = clientconnect.SendPackLifecyclePhaseFirstRouteWrite, first
			observer(observation)
			observation.Phase, observation.Err = clientconnect.SendPackLifecyclePhaseTerminal, terminal
			observer(observation)
		}
		publish(1, firstErr, terminalErr)
		if test.extraFailure {
			publish(2, nil, errors.New("independent admitted delivery failed"))
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		boundary, ok := tracker.workloadBoundary(ctx)
		joined := ok && tracker.waitThrough(ctx, boundary)
		cancel()
		counts := snapshotPerfvarPackFailures(path)
		wantRaw := uint64(1)
		if test.healthProbe {
			wantRaw = 0
		}
		if test.extraFailure {
			wantRaw++
		}
		wantAllFailures := uint64(1)
		if test.extraFailure {
			wantAllFailures++
		}
		if !joined || tracker.invalid.Load() || counts.deviceFailureCount != wantRaw ||
			counts.deviceNativeReadyPureAckRefusalCount != test.wantNative ||
			uint64(len(tracker.workloadFailureSnapshot())) != wantRaw {
			t.Errorf("%s: lifecycle/raw/native census changed: joined=%t invalid=%t counts=%+v", test.name, joined, tracker.invalid.Load(), counts)
		}
		err := path.validateMeasuredPackFailures(perfvarCarrierBoundary{packFailures: counts})
		if (err != nil) != test.wantFailure {
			t.Errorf("%s: proposed contract gate error=%v want failure=%t raw=%+v", test.name, err, test.wantFailure, counts)
		}
		if tracker.workloadFailures.Load() != wantRaw || uint64(len(tracker.workloadFailureSnapshot())) != wantRaw || tracker.failures.Load() != wantAllFailures {
			t.Errorf("%s: gate suppressed raw failures or samples", test.name)
		}
		tracker.close()
	}
}

// Categories must stay disjoint and monotonic; a successful earlier interval
// cannot create an allowance for a later admitted error or setup history.
func TestMeasuredNativeReadyPureAckRefusalIntervalsAndCategories(t *testing.T) {
	for _, counts := range []perfvarPackFailureCounts{
		{deviceFailureCount: 1, deviceRecoveredAdmissionFailureCount: 1, deviceNativeReadyPureAckRefusalCount: 1},
		{deviceFailureCount: 1, deviceNativeReadyPureAckRefusalCount: 2},
	} {
		path := &fullTunPath{activePackFailureFloor: &perfvarPackFailureCounts{}, allowDeviceNativeAckPressure: true}
		if err := path.validateMeasuredPackFailures(perfvarCarrierBoundary{packFailures: counts}); err == nil {
			t.Fatalf("overlapping or excessive native pressure passed: %+v", counts)
		}
	}
	floor := perfvarPackFailureCounts{deviceFailureCount: 7, deviceNativeReadyPureAckRefusalCount: 3}
	path := &fullTunPath{activePackFailureFloor: &floor, allowDeviceNativeAckPressure: true}
	if err := path.validateMeasuredPackFailures(perfvarCarrierBoundary{packFailures: floor}); err != nil {
		t.Fatal("setup-only history poisoned unchanged measured interval", err)
	}
	end := perfvarPackFailureCounts{deviceFailureCount: 8, deviceNativeReadyPureAckRefusalCount: 4}
	if err := path.validateMeasuredPackFailures(perfvarCarrierBoundary{packFailures: end}); err != nil {
		t.Fatal("new exact native refusal failed proposed contract", err)
	}
	end.deviceFailureCount++
	if err := path.validateMeasuredPackFailures(perfvarCarrierBoundary{packFailures: end}); err == nil {
		t.Fatal("accounted native pressure masked a later unknown failure")
	}
	end = *path.activePackFailureFloor
	end.deviceNativeReadyPureAckRefusalCount--
	if err := path.validateMeasuredPackFailures(perfvarCarrierBoundary{packFailures: end}); err == nil || !strings.Contains(err.Error(), "moved backward") {
		t.Fatal("backward native pressure watermark passed", err)
	}
}

// Exact unwritten replay supersession is a separate observed outcome. It
// accounts for one raw failure. Independent delivery errors fail the interval.
func TestMeasuredNativeAckRetrySupersessionAccounting(t *testing.T) {
	floor := perfvarPackFailureCounts{
		deviceFailureCount: 3, deviceRecoveredAdmissionFailureCount: 1,
		deviceNativeReadyPureAckRefusalCount: 1, deviceNativeAckRetrySupersededCount: 1,
	}
	healthy := floor
	healthy.deviceFailureCount++
	healthy.deviceNativeAckRetrySupersededCount++
	independentFailure := healthy
	independentFailure.deviceFailureCount++
	overlap := healthy
	overlap.deviceNativeReadyPureAckRefusalCount++
	backward := floor
	backward.deviceNativeAckRetrySupersededCount--
	for _, test := range []struct {
		name      string
		end       perfvarPackFailureCounts
		optedOut  bool
		wantError string
	}{
		{name: "exact unwritten replay after floor", end: healthy},
		{name: "independent delivery error", end: independentFailure, wantError: "device-unrecovered=1"},
		{name: "strict gate", end: healthy, optedOut: true, wantError: "device-unrecovered=1"},
		{name: "overlapping outcomes", end: overlap, wantError: "exceeded raw failures"},
		{name: "backward outcome count", end: backward, wantError: "moved backward"},
	} {
		start := floor
		path := &fullTunPath{
			activePackFailureFloor:       &start,
			allowDeviceNativeAckPressure: !test.optedOut,
		}
		err := path.validateMeasuredPackFailures(perfvarCarrierBoundary{packFailures: test.end})
		if test.wantError == "" {
			if err != nil || *path.activePackFailureFloor != test.end {
				t.Errorf("%s: exact unwritten supersession lost its own raw outcome: floor=%+v err=%v", test.name, *path.activePackFailureFloor, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), test.wantError) || *path.activePackFailureFloor != floor {
			t.Errorf("%s: failure classification or floor changed: floor=%+v error=%v want=%q", test.name, *path.activePackFailureFloor, err, test.wantError)
		}
	}
}

// Connect's real full-queue regression produces these exact terminal
// dispositions. Exercise the actual tracker and measured gate, not a copied
// predicate: successful enclosing admission resolves only its own refusal.
func TestSendPackLifecycleRecoveredAdmissionMeasuredGate(t *testing.T) {
	for _, test := range []struct {
		name        string
		err         error
		wantFailure bool
	}{
		{"recovered same input", &clientconnect.SendPackAdmissionError{Boundary: "pack-admission", RecoveredByOwner: true, Err: clientconnect.ErrSendPackNotAdmitted}, false},
		{"final refused input", &clientconnect.SendPackAdmissionError{Boundary: "pack-admission", Err: clientconnect.ErrSendPackNotAdmitted}, true},
		{"tracking overflow", &clientconnect.SendPackAdmissionError{Boundary: "pack-admission", RecoveredByOwner: true, OwnerTrackingOverflow: true, Err: clientconnect.ErrSendPackNotAdmitted}, true},
		{"unclassified sentinel", clientconnect.ErrSendPackNotAdmitted, true},
		{"post admission error", errors.New("terminal after admission"), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			tracker := newSendPackLifecycleTracker()
			defer tracker.close()
			path := &fullTunPath{devicePackSends: tracker, activePackFailureFloor: &perfvarPackFailureCounts{}}
			observer := tracker.newObserver()
			observation := clientconnect.SendPackLifecycleObservation{
				ClientId: clientconnect.NewId(), DestinationId: clientconnect.NewId(),
				Token: 1, AckRequired: true, MessageType: protocol.MessageType_IpIpPacketToProvider,
			}
			for _, phase := range []clientconnect.SendPackLifecyclePhase{
				clientconnect.SendPackLifecyclePhaseStarted,
				clientconnect.SendPackLifecyclePhaseFirstRouteWrite,
				clientconnect.SendPackLifecyclePhaseTerminal,
			} {
				observation.Phase = phase
				if phase != clientconnect.SendPackLifecyclePhaseStarted {
					observation.Err = test.err
				}
				observer(observation)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			boundary, ok := tracker.boundary(ctx)
			if !ok || !tracker.waitThrough(ctx, boundary) || tracker.invalid.Load() {
				t.Fatal("failed exact lifecycle boundary")
			}
			if tracker.workloadFailures.Load() != 1 || len(tracker.workloadFailureSnapshot()) != 1 {
				t.Fatal("attempt failure was suppressed from diagnostics")
			}
			err := path.validateMeasuredPackFailures(perfvarCarrierBoundary{packFailures: snapshotPerfvarPackFailures(path)})
			if (err != nil) != test.wantFailure {
				t.Fatalf("measured gate error=%v want failure=%t", err, test.wantFailure)
			}
		})
	}
}

// The real p2p-legacy download failure had three owned UDP refusals, each
// present in both raw counters. Exercise observer publication and the actual
// gate together so disjoint synthetic counters cannot hide this regression.
func TestProviderDatagramRecoveryOverlapMeasuredGate(t *testing.T) {
	for _, allowDatagrams := range []bool{false, true} {
		for _, extra := range []string{"none", "unrecoverable datagram", "unclassified reliable"} {
			t.Run(fmt.Sprintf("allow=%t/%s", allowDatagrams, extra), func(t *testing.T) {
				tracker := newSendPackLifecycleTracker()
				defer tracker.close()
				path := &fullTunPath{providerPackSends: tracker,
					activePackFailureFloor:            &perfvarPackFailureCounts{},
					allowProviderDatagramPackFailures: allowDatagrams}
				observer := tracker.newObserver()
				publish := func(token uint64, ack, recoverable bool, err error) {
					observation := clientconnect.SendPackLifecycleObservation{
						ClientId: clientconnect.NewId(), DestinationId: clientconnect.NewId(),
						Token: token, AckRequired: ack, UpstreamRecoverable: recoverable,
						MessageType: protocol.MessageType_IpIpPacketFromProvider,
					}
					for _, phase := range []clientconnect.SendPackLifecyclePhase{
						clientconnect.SendPackLifecyclePhaseStarted,
						clientconnect.SendPackLifecyclePhaseFirstRouteWrite,
						clientconnect.SendPackLifecyclePhaseTerminal,
					} {
						observation.Phase = phase
						if phase != clientconnect.SendPackLifecyclePhaseStarted {
							observation.Err = err
						}
						observer(observation)
					}
				}
				for token := uint64(1); token <= 3; token++ {
					publish(token, false, true, &clientconnect.SendPackAdmissionError{
						Boundary: "pack-admission", RecoveredByOwner: true, Err: clientconnect.ErrSendPackNotAdmitted})
				}
				if extra != "none" {
					publish(4, extra == "unclassified reliable", false, errors.New("unrecovered terminal"))
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				boundary, ok := tracker.workloadBoundary(ctx)
				if !ok || !tracker.waitThrough(ctx, boundary) || tracker.invalid.Load() {
					t.Fatal("failed exact provider lifecycle boundary")
				}
				counts := snapshotPerfvarPackFailures(path)
				wantDatagrams := uint64(3)
				if extra == "unrecoverable datagram" {
					wantDatagrams++
				}
				if counts.providerRecoverableFailureCount != 3 ||
					counts.providerDatagramFailureCount != wantDatagrams ||
					counts.providerRecoverableDatagramFailureCount != 3 ||
					uint64(len(tracker.workloadFailureSnapshot())) != counts.providerFailureCount {
					t.Fatalf("raw failure diagnostics changed: %+v", counts)
				}
				err := path.validateMeasuredPackFailures(perfvarCarrierBoundary{packFailures: counts})
				wantFailure := extra == "unclassified reliable" || extra == "unrecoverable datagram" && !allowDatagrams
				if (err != nil) != wantFailure {
					t.Fatalf("gate error=%v, want failure=%t counts=%+v", err, wantFailure, counts)
				}
				if !wantFailure {
					path.setAllowProviderDatagramPackFailures(false)
					if err := path.validateMeasuredPackFailures(perfvarCarrierBoundary{packFailures: counts}); err != nil {
						t.Fatalf("accounted overlap poisoned later boundary: %v", err)
					}
				}
			})
		}
	}
}

func TestProviderDatagramRecoveryOverlapRejectsInvalidEvidence(t *testing.T) {
	for _, counts := range []perfvarPackFailureCounts{
		{providerFailureCount: 1, providerRecoverableFailureCount: 1, providerRecoverableDatagramFailureCount: 1},
		{providerFailureCount: 1, providerDatagramFailureCount: 1, providerRecoverableDatagramFailureCount: 1},
		{providerFailureCount: 1, providerRecoverableFailureCount: 2, providerDatagramFailureCount: 2, providerRecoverableDatagramFailureCount: 2},
	} {
		path := &fullTunPath{activePackFailureFloor: &perfvarPackFailureCounts{}, allowProviderDatagramPackFailures: true}
		if err := path.validateMeasuredPackFailures(perfvarCarrierBoundary{packFailures: counts}); err == nil {
			t.Fatalf("invalid overlap passed: %+v", counts)
		}
	}
	path := &fullTunPath{activePackFailureFloor: &perfvarPackFailureCounts{
		providerFailureCount: 1, providerRecoverableFailureCount: 1,
		providerDatagramFailureCount: 1, providerRecoverableDatagramFailureCount: 1}}
	if err := path.validateMeasuredPackFailures(perfvarCarrierBoundary{packFailures: perfvarPackFailureCounts{
		providerFailureCount: 1, providerRecoverableFailureCount: 1, providerDatagramFailureCount: 1,
	}}); err == nil || !strings.Contains(err.Error(), "moved backward") {
		t.Fatalf("backward overlap error=%v", err)
	}
}
