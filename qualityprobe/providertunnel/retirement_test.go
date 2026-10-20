package providertunnel

// Acknowledging late provider returns before a probe's derived client is
// revoked, and the pass-owned Retirements that holds that wait off the
// caller. Every wait is released at an explicit barrier (drainTimers), never
// by a clock.

import (
	"context"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/connect"
)

// The acknowledgment's next wait after a fired interval. An acknowledgment
// that ended instead fails the test rather than leaving it waiting.
func nextAcknowledgeWait(t *testing.T, timers *drainTimers, done <-chan returnAcknowledgment, want time.Duration, ended string) drainTimer {
	t.Helper()
	select {
	case timer := <-timers.requests:
		if timer.duration != want {
			t.Fatalf("acknowledgment asked to wait %s, want %s", timer.duration, want)
		}
		return timer
	case acknowledgment := <-done:
		t.Fatalf("%s: %+v", ended, acknowledgment)
		return drainTimer{}
	}
}

// A return the receive path refused, or the network lost, comes back only on
// the provider's resend timer, which a provider sdk backs off to 8s: long
// after the drain's quiet second. The derived client keeps acknowledging
// until a whole acknowledge interval passes with no return while the provider
// is present.
func TestProviderReturnAcknowledgeWaitsOutTheProviderResendInterval(t *testing.T) {
	drain, timers := newTestProviderReturnDrain()
	done := make(chan returnAcknowledgment, 1)
	go func() { done <- drain.acknowledge(t.Context(), func() bool { return true }) }()
	timers.next(t, time.Minute)
	first := timers.next(t, 10*time.Second)

	// the retransmission arrives within the interval
	drain.observe()
	first.fire <- time.Time{}
	second := nextAcknowledgeWait(t, timers, done, 10*time.Second, "acknowledgment ended in the interval a retransmitted return arrived in")

	second.fire <- time.Time{}
	acknowledgment := <-done
	want := returnAcknowledgment{outcome: retirementOutcomeQuiet, lateReturnCount: 1}
	if acknowledgment != want {
		t.Fatalf("acknowledgment = %+v, want %+v", acknowledgment, want)
	}
}

// A quiet interval no longer than the provider's longest wait between two
// retransmissions of one return, or between two contract attempts for it, can
// end between them. The default outlasts both caps of the provider sdk.
func TestReturnAcknowledgeQuietIntervalOutlastsTheProviderResendInterval(t *testing.T) {
	sendBuffer := connect.DefaultSendBufferSettings()
	resend := max(sendBuffer.MaxResendInterval, sendBuffer.CreateContractRetryMaxInterval)
	if quiet := (Config{}).returnAcknowledgeQuietInterval(); quiet <= resend {
		t.Fatalf("acknowledge quiet interval %s does not outlast the provider's resend interval %s", quiet, resend)
	}
	if timeout := (Config{}).returnAcknowledgeTimeout(); timeout < sendBuffer.AckTimeout {
		t.Fatalf("acknowledge timeout %s ends before the provider gives up an ordinary return at %s", timeout, sendBuffer.AckTimeout)
	}
}

// A provider that keeps returning only delays revocation by the acknowledge
// timeout, and the returns still arriving at the end are counted as
// trailing.
func TestProviderReturnAcknowledgeIsBoundedByItsTimeout(t *testing.T) {
	drain, timers := newTestProviderReturnDrain()
	done := make(chan returnAcknowledgment, 1)
	go func() { done <- drain.acknowledge(t.Context(), func() bool { return true }) }()
	timeout := timers.next(t, time.Minute)
	quiet := timers.next(t, 10*time.Second)
	for range 3 {
		drain.observe()
		quiet.fire <- time.Time{}
		quiet = nextAcknowledgeWait(t, timers, done, 10*time.Second, "acknowledgment ended in an interval a return arrived in")
	}

	drain.observe()
	drain.observe()
	timeout.fire <- time.Time{}
	acknowledgment := <-done
	want := returnAcknowledgment{outcome: retirementOutcomeTimeBound, lateReturnCount: 5, trailingReturnCount: 2}
	if acknowledgment != want {
		t.Fatalf("acknowledgment = %+v, want %+v", acknowledgment, want)
	}
}

// A lost tunnel's provider is not in the multi-client, so no return can reach
// the derived client and a quiet interval proves nothing: the wait goes on
// for the path. A provider that comes back and returns what it held is
// acknowledged before the derived client is revoked.
func TestProviderReturnAcknowledgeWaitsForTheProviderPath(t *testing.T) {
	drain, timers := newTestProviderReturnDrain()
	var providerUp atomic.Bool
	done := make(chan returnAcknowledgment, 1)
	go func() { done <- drain.acknowledge(t.Context(), providerUp.Load) }()
	timers.next(t, time.Minute)
	quiet := timers.next(t, 10*time.Second)

	quiet.fire <- time.Time{}
	quiet = nextAcknowledgeWait(t, timers, done, 10*time.Second, "a quiet interval with no provider to return through ended the acknowledgment")

	providerUp.Store(true)
	drain.observe()
	quiet.fire <- time.Time{}
	quiet = nextAcknowledgeWait(t, timers, done, 10*time.Second, "acknowledgment ended in the interval the returned provider's return arrived in")
	quiet.fire <- time.Time{}
	acknowledgment := <-done
	want := returnAcknowledgment{outcome: retirementOutcomeQuiet, lateReturnCount: 1}
	if acknowledgment != want {
		t.Fatalf("acknowledgment = %+v, want %+v", acknowledgment, want)
	}
}

// A provider that never comes back ends the wait at its timeout, as no path.
func TestProviderReturnAcknowledgeWithoutAPathEndsAsNoPath(t *testing.T) {
	drain, timers := newTestProviderReturnDrain()
	done := make(chan returnAcknowledgment, 1)
	go func() { done <- drain.acknowledge(t.Context(), func() bool { return false }) }()
	timeout := timers.next(t, time.Minute)
	quiet := timers.next(t, 10*time.Second)
	quiet.fire <- time.Time{}
	nextAcknowledgeWait(t, timers, done, 10*time.Second, "a quiet interval with no provider to return through ended the acknowledgment")

	timeout.fire <- time.Time{}
	acknowledgment := <-done
	want := returnAcknowledgment{outcome: retirementOutcomeNoPath}
	if acknowledgment != want {
		t.Fatalf("acknowledgment = %+v, want %+v", acknowledgment, want)
	}
}

// A teardown whose drain and acknowledgment run on the shared test drain and
// whose owners record their retirement.
func newAcknowledgingTeardown(
	drain *providerReturnDrain,
	recorder *teardownRecorder,
	multiClient *acknowledgingMultiClient,
	retirements *Retirements,
) *tunnelTeardown {
	pumpDone := make(chan struct{})
	close(pumpDone)
	return &tunnelTeardown{
		path:          retirementPathClosed,
		closeTimeout:  time.Minute,
		retirements:   retirements,
		closeResolver: func() { recorder.record("resolver") },
		drainReturns: func(ctx context.Context) {
			drain.wait(ctx, nil)
		},
		acknowledgeReturns: func() returnAcknowledgment {
			return drain.acknowledge(context.Background(), func() bool { return true })
		},
		retireParts: func(ctx context.Context) error {
			return retireTunnelParts(
				ctx,
				func() { recorder.record("cancel-data") },
				func() error { recorder.record("tun"); return nil },
				pumpDone,
				multiClient,
				&recordedGenerator{recorder: recorder},
				func() { recorder.record("client-strategy") },
				func() { recorder.record("cancel-lifecycle") },
			)
		},
	}
}

// The drain's quiet second used to end teardown, so a return the provider
// retransmitted after it reached a revoked derived client, and a provider sdk
// before September 2026 retransmits it forever: about 2% of URL probes on
// 2026-10-19. The derived client is revoked only after that return is
// acknowledged and a whole acknowledge interval passes.
func TestTunnelTeardownRevokesOnlyAfterLateReturnsAreAcknowledged(t *testing.T) {
	recorder := &teardownRecorder{}
	multiClient := &acknowledgingMultiClient{recorder: recorder}
	drain, timers := newTestProviderReturnDrain()
	teardown := newAcknowledgingTeardown(drain, recorder, multiClient, nil)
	closed := make(chan error, 1)
	go func() { closed <- teardown.close() }()

	// the drain ends at its first quiet second
	timers.next(t, 5*time.Second)
	timers.next(t, time.Second).fire <- time.Time{}
	var acknowledgeTimeout drainTimer
	select {
	case acknowledgeTimeout = <-timers.requests:
	case err := <-closed:
		t.Fatalf("the derived client was revoked at the drain's quiet second (err=%v, steps=%v), before a retransmitted return could be acknowledged", err, recorder.snapshot())
	}
	if acknowledgeTimeout.duration != time.Minute {
		t.Fatalf("acknowledgment asked to wait %s first, want its timeout %s", acknowledgeTimeout.duration, time.Minute)
	}

	// the provider retransmits a return the receive path had refused
	quiet := timers.next(t, 10*time.Second)
	multiClient.receiveReturn()
	drain.observe()
	quiet.fire <- time.Time{}
	quiet = timers.next(t, 10*time.Second)
	if steps, want := recorder.snapshot(), []string{"resolver"}; !reflect.DeepEqual(steps, want) {
		t.Fatalf("teardown steps while a retransmitted return arrived = %v, want %v", steps, want)
	}

	quiet.fire <- time.Time{}
	if err := <-closed; err != nil {
		t.Fatalf("tunnel teardown: %v", err)
	}
	want := []string{"resolver", "cancel-data", "tun", "multi-client", "generator", "client-strategy", "cancel-lifecycle"}
	if steps := recorder.snapshot(); !reflect.DeepEqual(steps, want) {
		t.Fatalf("teardown order = %v, want %v", steps, want)
	}
	if multiClient.acked != 1 || multiClient.lost != 0 {
		t.Fatalf("retransmitted returns: acknowledged=%d lost=%d, want 1 and 0", multiClient.acked, multiClient.lost)
	}
}

// A pass's Retirements holds the acknowledgment off the caller: Close returns
// once the provider's flows ended, the derived client is revoked later, and
// closing the owner waits for that revocation.
func TestRetirementsHoldTheAcknowledgmentOffTheCaller(t *testing.T) {
	recorder := &teardownRecorder{}
	multiClient := &acknowledgingMultiClient{recorder: recorder}
	drain, timers := newTestProviderReturnDrain()
	retirements := NewRetirements(1)
	teardown := newAcknowledgingTeardown(drain, recorder, multiClient, retirements)
	closed := make(chan error, 1)
	go func() { closed <- teardown.close() }()
	timers.next(t, 5*time.Second)
	timers.next(t, time.Second).fire <- time.Time{}

	if err := <-closed; err != nil {
		t.Fatalf("held teardown: %v", err)
	}
	// the held tunnel occupies the owner's only room
	if retirements.hold(func() {}) {
		t.Fatal("a full owner held a second tunnel")
	}

	ownerClosed := make(chan struct{})
	go func() {
		retirements.Close()
		close(ownerClosed)
	}()
	var acknowledgeTimeout drainTimer
	select {
	case acknowledgeTimeout = <-timers.requests:
	case <-ownerClosed:
		t.Fatalf("the held tunnel's derived client was revoked without acknowledging: %v", recorder.snapshot())
	}
	if acknowledgeTimeout.duration != time.Minute {
		t.Fatalf("acknowledgment asked to wait %s first, want its timeout %s", acknowledgeTimeout.duration, time.Minute)
	}
	quiet := timers.next(t, 10*time.Second)
	if steps, want := recorder.snapshot(), []string{"resolver"}; !reflect.DeepEqual(steps, want) {
		t.Fatalf("steps while the held tunnel acknowledges = %v, want %v", steps, want)
	}
	quiet.fire <- time.Time{}
	<-ownerClosed
	want := []string{"resolver", "cancel-data", "tun", "multi-client", "generator", "client-strategy", "cancel-lifecycle"}
	if steps := recorder.snapshot(); !reflect.DeepEqual(steps, want) {
		t.Fatalf("steps when the owner closed = %v, want %v", steps, want)
	}
}

// An owner with no room keeps the acknowledgment in the caller's Close,
// which slows the pass instead of growing it, and counts that.
func TestRetirementsWithoutRoomKeepTheAcknowledgmentInClose(t *testing.T) {
	recorder := &teardownRecorder{}
	multiClient := &acknowledgingMultiClient{recorder: recorder}
	drain, timers := newTestProviderReturnDrain()
	before := testutil.ToFloat64(tunnelRetirementBackpressureCounter)
	teardown := newAcknowledgingTeardown(drain, recorder, multiClient, NewRetirements(0))
	closed := make(chan error, 1)
	go func() { closed <- teardown.close() }()
	timers.next(t, 5*time.Second)
	timers.next(t, time.Second).fire <- time.Time{}
	var acknowledgeTimeout drainTimer
	select {
	case acknowledgeTimeout = <-timers.requests:
	case err := <-closed:
		t.Fatalf("Close returned without acknowledging with no room to hold it (err=%v)", err)
	}
	if acknowledgeTimeout.duration != time.Minute {
		t.Fatalf("acknowledgment asked to wait %s first, want its timeout %s", acknowledgeTimeout.duration, time.Minute)
	}
	quiet := timers.next(t, 10*time.Second)
	select {
	case err := <-closed:
		t.Fatalf("Close returned before acknowledging with no room to hold it (err=%v)", err)
	default:
	}

	quiet.fire <- time.Time{}
	if err := <-closed; err != nil {
		t.Fatalf("tunnel teardown: %v", err)
	}
	if steps := recorder.snapshot(); len(steps) == 0 || steps[len(steps)-1] != "cancel-lifecycle" {
		t.Fatalf("Close returned before the derived client retired: %v", steps)
	}
	if after := testutil.ToFloat64(tunnelRetirementBackpressureCounter); after != before+1 {
		t.Fatalf("backpressure counted %v, want %v", after, before+1)
	}
}

// A closed owner holds nothing more, and its Close is idempotent.
func TestRetirementsClosedOwnerHoldsNothing(t *testing.T) {
	retirements := NewRetirements(4)
	retirements.Close()
	if retirements.hold(func() { t.Error("a closed owner ran a retirement") }) {
		t.Fatal("a closed owner held a tunnel")
	}
	retirements.Close()
	var unset *Retirements
	if unset.hold(func() { t.Error("a nil owner ran a retirement") }) {
		t.Fatal("a nil owner held a tunnel")
	}
}

// Every revoked derived client is counted by path and outcome, with the
// returns acknowledged late and the returns still arriving at revocation.
func TestRecordTunnelRetirementCountsPathAndOutcome(t *testing.T) {
	retirements := tunnelRetirementCounter.WithLabelValues(retirementPathLost, retirementOutcomeTimeBound)
	late := tunnelRetirementLateReturnCounter.WithLabelValues(retirementPathLost)
	trailing := tunnelRetirementTrailingReturnCounter.WithLabelValues(retirementPathLost, retirementOutcomeTimeBound)
	retirementsBefore, lateBefore, trailingBefore := testutil.ToFloat64(retirements), testutil.ToFloat64(late), testutil.ToFloat64(trailing)
	recordTunnelRetirement(retirementPathLost, returnAcknowledgment{outcome: retirementOutcomeTimeBound, lateReturnCount: 3, trailingReturnCount: 2})
	if got := testutil.ToFloat64(retirements) - retirementsBefore; got != 1 {
		t.Fatalf("retirements counted %v, want 1", got)
	}
	if got := testutil.ToFloat64(late) - lateBefore; got != 3 {
		t.Fatalf("late returns counted %v, want 3", got)
	}
	if got := testutil.ToFloat64(trailing) - trailingBefore; got != 2 {
		t.Fatalf("trailing returns counted %v, want 2", got)
	}
}

// The retirement path names how the tunnel ended: its provider leaving takes
// precedence over its caller's context ending, and an ordinary close is
// neither. A tunnel closed by Close itself is not lost.
func TestTunnelRetirementPathNamesHowTheTunnelEnded(t *testing.T) {
	lost, lose := context.WithCancelCause(context.Background())
	callerDone := make(chan struct{})
	tunnel := &Tunnel{lost: lost, callerDone: callerDone}
	if path := tunnel.retirementPath(); path != retirementPathClosed {
		t.Fatalf("path of a live tunnel with a live caller = %s, want %s", path, retirementPathClosed)
	}
	close(callerDone)
	if path := tunnel.retirementPath(); path != retirementPathCanceled {
		t.Fatalf("path after the caller ended = %s, want %s", path, retirementPathCanceled)
	}
	lose(fmt.Errorf("%w: the provider left", ErrTunnelLost))
	if path := tunnel.retirementPath(); path != retirementPathLost {
		t.Fatalf("path after the provider left = %s, want %s", path, retirementPathLost)
	}
	closedLost, closeLost := context.WithCancelCause(context.Background())
	closeLost(ErrTunnelClosed)
	if path := (&Tunnel{lost: closedLost, callerDone: make(chan struct{})}).retirementPath(); path != retirementPathClosed {
		t.Fatalf("path of a tunnel Close itself ended = %s, want %s", path, retirementPathClosed)
	}
}

// A pass canceled by a deploy, a shutdown or its deadline used to cancel the
// data path under every open tunnel, so Close found nothing left to
// acknowledge the provider's returns with. The caller's context ending leaves
// the data path to Close.
func TestOpenDataPathOutlivesItsCallersContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tunnel, err := Open(ctx, dummyOpenConfig(), connect.NewId())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	cancel()
	select {
	case <-tunnel.dataDone:
		t.Fatal("the caller's cancellation ended the tunnel's data path before Close")
	default:
	}
	if err := tunnel.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-tunnel.dataDone:
	default:
		t.Fatal("Close did not end the data path")
	}
}

// A provider that never joined the multi-client was never sent a contract and
// holds no return, so its tunnel retires without waiting, counted as unused.
func TestCloseRetiresANeverConnectedTunnelWithoutAcknowledging(t *testing.T) {
	unused := tunnelRetirementCounter.WithLabelValues(retirementPathClosed, retirementOutcomeUnused)
	before := testutil.ToFloat64(unused)
	tunnel, err := Open(context.Background(), dummyOpenConfig(), connect.NewId())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := tunnel.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := testutil.ToFloat64(unused) - before; got != 1 {
		t.Fatalf("unused retirements counted %v, want 1", got)
	}
}

// Probe tunnels never upgrade to a direct connection, although their
// window's Network provide mode would force it on: their clients are hosted,
// and a direct stream outlives the measurement.
func TestProviderTunnelMultiClientSettingsDisallowDirect(t *testing.T) {
	for _, cfg := range []Config{
		{},
		{DataOnlyProbe: true},
		{CapabilityObservation: &CapabilityObservation{RedialMin: time.Second}},
	} {
		settings := providerTunnelMultiClientSettingsForConfig(cfg)
		if settings.OverrideAllowDirect == nil || *settings.OverrideAllowDirect {
			t.Fatalf("OverrideAllowDirect = %v for %+v, want false", settings.OverrideAllowDirect, cfg)
		}
	}
}
