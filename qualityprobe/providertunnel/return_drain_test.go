package providertunnel

import (
	"context"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// One wait the drain asked for. The test fires it explicitly.
type drainTimer struct {
	duration time.Duration
	fire     chan time.Time
}

// Hands every wait the drain requests to the test, in order, so each interval
// ends at an explicit barrier instead of a clock.
type drainTimers struct {
	requests chan drainTimer
}

func newDrainTimers() *drainTimers {
	return &drainTimers{requests: make(chan drainTimer, 16)}
}

func (self *drainTimers) after(duration time.Duration) <-chan time.Time {
	timer := drainTimer{duration: duration, fire: make(chan time.Time, 1)}
	self.requests <- timer
	return timer.fire
}

func (self *drainTimers) next(t *testing.T, want time.Duration) drainTimer {
	t.Helper()
	timer := <-self.requests
	if timer.duration != want {
		t.Fatalf("drain asked to wait %s, want %s", timer.duration, want)
	}
	return timer
}

func newTestProviderReturnDrain() (*providerReturnDrain, *drainTimers) {
	timers := newDrainTimers()
	drain := newProviderReturnDrain(time.Second, 5*time.Second)
	drain.after = timers.after
	return drain, timers
}

// A provider return that arrives during the quiet interval means the provider
// is still sending, so the derived client must stay a live destination for
// another whole interval. Only an interval with no return ends the drain.
func TestProviderReturnDrainWaitsForAQuietInterval(t *testing.T) {
	drain, timers := newTestProviderReturnDrain()
	done := make(chan bool, 1)
	go func() { done <- drain.wait(t.Context(), nil) }()
	timers.next(t, 5*time.Second)
	first := timers.next(t, time.Second)

	drain.observe()
	first.fire <- time.Time{}
	second := timers.next(t, time.Second)
	select {
	case <-done:
		t.Fatal("the drain ended in the interval a provider return arrived in")
	default:
	}

	second.fire <- time.Time{}
	if quiet := <-done; !quiet {
		t.Fatal("a whole quiet interval did not end the drain as quiet")
	}
}

// A provider that never goes quiet only delays teardown by the drain timeout.
func TestProviderReturnDrainIsBoundedByItsTimeout(t *testing.T) {
	drain, timers := newTestProviderReturnDrain()
	done := make(chan bool, 1)
	go func() { done <- drain.wait(t.Context(), nil) }()
	timeout := timers.next(t, 5*time.Second)
	quiet := timers.next(t, time.Second)
	for range 3 {
		drain.observe()
		quiet.fire <- time.Time{}
		quiet = timers.next(t, time.Second)
	}

	drain.observe()
	timeout.fire <- time.Time{}
	if quiet := <-done; quiet {
		t.Fatal("a drain ended by its timeout reported a quiet provider")
	}
}

// Close's own deadline ends the drain too, so CloseTimeout still bounds
// teardown when a caller configures a drain longer than it.
func TestProviderReturnDrainEndsWithCloseDeadline(t *testing.T) {
	drain, timers := newTestProviderReturnDrain()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan bool, 1)
	go func() { done <- drain.wait(ctx, nil) }()
	timers.next(t, 5*time.Second)
	timers.next(t, time.Second)

	cancel()
	if quiet := <-done; quiet {
		t.Fatal("a drain ended by Close's deadline reported a quiet provider")
	}
}

// A caller that already canceled the tunnel also canceled its multi-client and
// derived client, so nothing is left to acknowledge returns: the drain must
// end at once instead of spending a quiet interval on a dead data path.
func TestProviderReturnDrainEndsWhenTheDataPathIsGone(t *testing.T) {
	drain, timers := newTestProviderReturnDrain()
	dataCtx, cancelData := context.WithCancel(t.Context())
	done := make(chan bool, 1)
	go func() { done <- drain.wait(t.Context(), dataCtx.Done()) }()
	timers.next(t, 5*time.Second)
	timers.next(t, time.Second)

	cancelData()
	if quiet := <-done; quiet {
		t.Fatal("a drain ended by a canceled data path reported a quiet provider")
	}
}

// Every packet the multi-client hands the tun is a provider return: it is
// recorded for the drain and still written to the tun unchanged.
func TestProviderReturnReceiverRecordsEveryReturn(t *testing.T) {
	drain, _ := newTestProviderReturnDrain()
	written := [][]byte{}
	receive := providerReturnReceiver(func(packet []byte) (int, error) {
		written = append(written, slices.Clone(packet))
		return len(packet), nil
	}, drain)
	packets := [][]byte{{0x45, 0x00, 0x01}, {0x45, 0x00, 0x02}}
	for _, packet := range packets {
		receive(connect.TransferPath{}, protocol.ProvideMode_Network, nil, packet)
	}
	if count := drain.returnCount.Load(); count != uint64(len(packets)) {
		t.Fatalf("drain recorded %d provider returns, want %d", count, len(packets))
	}
	if !reflect.DeepEqual(written, packets) {
		t.Fatalf("tun got %v, want %v", written, packets)
	}
}

// Records teardown steps from the closing goroutine for the test goroutine.
type teardownRecorder struct {
	stateLock sync.Mutex
	steps     []string
}

func (self *teardownRecorder) record(step string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.steps = append(self.steps, step)
}

func (self *teardownRecorder) snapshot() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.steps)
}

// A multi-client that acknowledges a provider return only while it is open.
type acknowledgingMultiClient struct {
	recorder *teardownRecorder

	stateLock sync.Mutex
	closed    bool
	acked     int
	lost      int
}

func (self *acknowledgingMultiClient) receiveReturn() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		self.lost++
	} else {
		self.acked++
	}
}

func (self *acknowledgingMultiClient) CloseAndWait(context.Context) error {
	self.recorder.record("multi-client")
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.closed = true
	return nil
}

type recordedGenerator struct {
	recorder *teardownRecorder
}

func (self *recordedGenerator) CloseAndWait(context.Context) error {
	self.recorder.record("generator")
	return nil
}

// The derived client is a destination the provider keeps retransmitting to
// until it acknowledges. Close must end the provider's flows while that client
// and the whole data path are still live: retire the private resolver's pooled
// connections, then hold teardown while the provider is still returning data.
// Nothing that stops acknowledgement -- the data path, the multi-client or the
// generator's revocation -- may start until the drain ends, and the rest of
// the teardown keeps its order afterwards.
func TestCloseTunnelPartsEndsProviderFlowsBeforeRevokingTheDerivedClient(t *testing.T) {
	recorder := &teardownRecorder{}
	multiClient := &acknowledgingMultiClient{recorder: recorder}
	generator := &recordedGenerator{recorder: recorder}
	pumpDone := make(chan struct{})
	close(pumpDone)
	drainEntered := make(chan struct{})
	drainRelease := make(chan struct{})
	closed := make(chan error, 1)
	go func() {
		closed <- closeTunnelParts(
			t.Context(),
			func() { recorder.record("resolver") },
			func(context.Context) {
				recorder.record("drain")
				close(drainEntered)
				// a late provider return reaches the derived client mid-drain
				multiClient.receiveReturn()
				<-drainRelease
			},
			func() { recorder.record("cancel-data") },
			func() error { recorder.record("tun"); return nil },
			pumpDone,
			multiClient,
			generator,
			func() { recorder.record("client-strategy") },
			func() { recorder.record("cancel-lifecycle") },
		)
	}()

	<-drainEntered
	if steps, want := recorder.snapshot(), []string{"resolver", "drain"}; !reflect.DeepEqual(steps, want) {
		t.Fatalf("teardown steps while the provider is still returning data = %v, want %v", steps, want)
	}
	close(drainRelease)
	if err := <-closed; err != nil {
		t.Fatalf("closeTunnelParts: %v", err)
	}
	want := []string{"resolver", "drain", "cancel-data", "tun", "multi-client", "generator", "client-strategy", "cancel-lifecycle"}
	if steps := recorder.snapshot(); !reflect.DeepEqual(steps, want) {
		t.Fatalf("teardown order = %v, want %v", steps, want)
	}
	if multiClient.acked != 1 || multiClient.lost != 0 {
		t.Fatalf("provider returns during the drain: acknowledged=%d lost=%d, want 1 and 0", multiClient.acked, multiClient.lost)
	}
}
