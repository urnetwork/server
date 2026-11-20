// Forced P2P readiness must join every platform owner after route publication
// before admitting workload traffic to the remaining carriers.
package perfvar

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	clientconnect "github.com/urnetwork/connect/v2026"
)

// The remaining carrier pauses the second selector's rebuild after the first
// selector has published withdrawal, independent of map iteration order.
type heldRouteSelectorPublication struct {
	clientconnect.Transport
	armed       atomic.Bool
	count       atomic.Int32
	entered     chan struct{}
	released    chan struct{}
	releaseOnce sync.Once
}

// A single remaining route evaluates its priority once per rebuilt selector.
func (self *heldRouteSelectorPublication) Priority() int {
	if self.armed.Load() && self.count.Add(1) == 2 {
		close(self.entered)
		<-self.released
	}
	return self.Transport.Priority()
}

// Releasing the rebuild also lets physical retirement join its old writers.
func (self *heldRouteSelectorPublication) release() {
	self.releaseOnce.Do(func() { close(self.released) })
}

// The helper observer can publish withdrawal while a different actual writer
// still spills a full P2P queue into H1. The owner join must close that gap.
func TestCloseRouteClientTransportsPreventsClosingPlatformSpill(t *testing.T) {
	// The process-global pool worker must outlive this virtual-time bubble.
	clientconnect.MessagePoolReturn(clientconnect.MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		manager := clientconnect.NewRouteManager(t.Context(), "forced-p2p-transition")
		destination := clientconnect.DestinationId(clientconnect.NewId())
		platform := clientconnect.NewSendGatewayTransportWithType(clientconnect.TransportTypeH1)
		platformRoute := make(clientconnect.Route, 1)
		p2p := &heldRouteSelectorPublication{
			Transport: clientconnect.NewSendClientTransport(destination),
			entered:   make(chan struct{}),
			released:  make(chan struct{}),
		}
		defer p2p.release()
		p2pRoute := make(clientconnect.Route, 1)
		p2pRoute <- nil // A full preferred carrier exposes fallback admission.
		manager.UpdateTransport(platform, []clientconnect.Route{platformRoute})
		manager.UpdateTransport(p2p, []clientconnect.Route{p2pRoute})
		writers := make([]clientconnect.MultiRouteWriter, 2)
		observers := make([]*clientconnect.TestingMultiRouteWriterRouteStateObserver, len(writers))
		for writerIndex := range writers {
			writer := manager.OpenMultiRouteWriter(destination)
			observer := clientconnect.TestingObserveMultiRouteWriterRouteState(writer)
			writers[writerIndex] = writer
			observers[writerIndex] = observer
			t.Cleanup(func() {
				observer.Close()
				manager.CloseMultiRouteWriter(writer)
			})
		}
		t.Cleanup(func() {
			manager.RemoveTransport(platform)
			manager.RemoveTransport(p2p)
		})
		barriers := []clientconnect.TestingMultiRouteWriterRouteState{
			observers[0].Snapshot(), observers[1].Snapshot(),
		}
		closed := make(chan struct{})
		joinStarted := make(chan struct{})
		var closeOnce sync.Once
		closeTransport := func() {
			closeOnce.Do(func() {
				go func() {
					manager.RemoveTransport(platform)
					// The stopped platform owns the same final drain as H1.
					select {
					case message := <-platformRoute:
						clientconnect.MessagePoolReturn(message)
					default:
					}
					close(closed)
				}()
			})
		}
		lifecycle := routeClientLifecycle{
			closeTransport: closeTransport,
			closeTransportAndWait: func(ctx context.Context) error {
				closeTransport()
				close(joinStarted)
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-closed:
					return nil
				}
			},
		}
		p2p.armed.Store(true)
		closeTransport()
		synctest.Wait()
		select {
		case <-p2p.entered:
		default:
			t.Fatal("platform removal did not pause between selector publications")
		}
		observerIndex := 0
		if observers[observerIndex].Snapshot().ActiveRouteCount != 1 {
			observerIndex = 1
		}
		writerIndex := 1 - observerIndex
		if observers[observerIndex].Snapshot().ActiveRouteCount != 1 ||
			observers[writerIndex].Snapshot().ActiveRouteCount != 2 {
			t.Fatal("test did not expose a withdrawn helper and stale workload selector")
		}
		expected := []byte("forced P2P workload")
		result := make(chan error, 1)
		go func() {
			if err := closeRouteClientTransportsAndWait(t.Context(), []routeClientLifecycle{lifecycle}); err != nil {
				result <- err
				return
			}
			if _, err := waitForRouteCountAfter(t.Context(), observers[observerIndex], barriers[observerIndex], 1); err != nil {
				result <- err
				return
			}
			message := clientconnect.MessagePoolCopy(expected)
			err := writers[writerIndex].Write(t.Context(), message, 0)
			if err != nil {
				clientconnect.MessagePoolReturn(message)
			}
			result <- err
		}()
		select {
		case message := <-platformRoute:
			clientconnect.MessagePoolReturn(message)
			t.Fatal("forced P2P workload entered closing H1 after helper selector withdrawal")
		case err := <-result:
			t.Fatalf("forced workload completed before platform retirement: %v", err)
		case <-joinStarted:
		}
		select {
		case err := <-result:
			t.Fatalf("forced workload completed before platform retirement: %v", err)
		default:
		}
		<-p2pRoute
		p2p.release()
		if err := <-result; err != nil {
			t.Fatalf("forced workload after platform completion: %v", err)
		}
		message := <-p2pRoute
		defer clientconnect.MessagePoolReturn(message)
		if !bytes.Equal(message, expected) {
			t.Fatalf("forced P2P payload=%q, want %q", message, expected)
		}
	})
}

// Logical withdrawal is immediate, while an explicit release holds the same
// owner-completion boundary exposed by PlatformTransport.CloseAndWait.
type heldRoutePlatformLifecycle struct {
	fixture      *liveP2pReadinessFixture
	closeStarted chan struct{}
	joinStarted  chan struct{}
	released     chan struct{}
	closeOnce    sync.Once
	joinOnce     sync.Once
	releaseOnce  sync.Once
	closeErr     error
}

// Each owner has independent production route publications and completion.
func newHeldRoutePlatformLifecycle(t *testing.T) *heldRoutePlatformLifecycle {
	owner := &heldRoutePlatformLifecycle{
		fixture:      newLiveP2pReadinessFixture(t),
		closeStarted: make(chan struct{}),
		joinStarted:  make(chan struct{}),
		released:     make(chan struct{}),
	}
	t.Cleanup(owner.release)
	return owner
}

// Closing withdraws the helper's platform route without completing the owner.
func (self *heldRoutePlatformLifecycle) close() {
	self.closeOnce.Do(func() {
		self.fixture.manager.RemoveTransport(self.fixture.platform)
		close(self.closeStarted)
	})
}

// Joining is bounded by the caller while completion remains independently held.
func (self *heldRoutePlatformLifecycle) closeAndWait(ctx context.Context) error {
	self.close()
	self.joinOnce.Do(func() { close(self.joinStarted) })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-self.released:
		return self.closeErr
	}
}

// Completion release is idempotent so assertion failures also unblock owners.
func (self *heldRoutePlatformLifecycle) release() {
	self.releaseOnce.Do(func() { close(self.released) })
}

// The exercised operations are exactly those used by both raw P2P fixtures.
func (self *heldRoutePlatformLifecycle) lifecycle() routeClientLifecycle {
	return routeClientLifecycle{
		closeTransport:        self.close,
		closeTransportAndWait: self.closeAndWait,
	}
}

// A real 2 -> 1 source publication cannot authorize traffic while any source,
// intermediary, or destination platform owner still has unfinished work.
func TestCloseRouteClientTransportsWaitsForEveryOwner(t *testing.T) {
	for _, clientCount := range []int{2, 4, 6, 10} {
		synctest.Test(t, func(t *testing.T) {
			owners := make([]*heldRoutePlatformLifecycle, clientCount)
			lifecycles := make([]routeClientLifecycle, clientCount)
			for clientIndex := range owners {
				owners[clientIndex] = newHeldRoutePlatformLifecycle(t)
				defer owners[clientIndex].release()
				lifecycles[clientIndex] = owners[clientIndex].lifecycle()
			}
			observer := owners[0].fixture.route.routeStateObserver
			barrier := observer.Snapshot()
			ready := make(chan error, 1)
			go func() {
				if err := closeRouteClientTransportsAndWait(t.Context(), lifecycles); err != nil {
					ready <- err
					return
				}
				_, err := waitForRouteCountAfter(t.Context(), observer, barrier, 1)
				ready <- err
			}()
			synctest.Wait()
			for clientIndex, owner := range owners {
				select {
				case <-owner.closeStarted:
				default:
					t.Fatalf("%d-client transition did not close platform %d before joining", clientCount, clientIndex)
				}
			}
			if _, err := waitForRouteCountAfter(t.Context(), observer, barrier, 1); err != nil {
				t.Fatalf("source withdrawal was not published: %v", err)
			}
			for clientIndex, owner := range owners {
				select {
				case err := <-ready:
					t.Fatalf("%d-client P2P workload admitted before platform %d completed: %v", clientCount, clientIndex, err)
				default:
				}
				select {
				case <-owner.joinStarted:
				default:
					t.Fatalf("%d-client transition did not reach platform %d join", clientCount, clientIndex)
				}
				owner.release()
				synctest.Wait()
			}
			if err := <-ready; err != nil {
				t.Fatalf("completed %d-client transition failed: %v", clientCount, err)
			}
			if routes := owners[0].fixture.route.writer.GetActiveRoutes(); len(routes) != 1 {
				t.Fatalf("completed transition has %d source routes, want one", len(routes))
			}
		})
	}
}

// One failed owner cannot skip the joins that retire the other fallback routes.
func TestCloseRouteClientTransportsJoinsAfterIndependentErrors(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		owners := make([]*heldRoutePlatformLifecycle, 3)
		lifecycles := make([]routeClientLifecycle, len(owners))
		for clientIndex := range owners {
			owners[clientIndex] = newHeldRoutePlatformLifecycle(t)
			defer owners[clientIndex].release()
			lifecycles[clientIndex] = owners[clientIndex].lifecycle()
		}
		firstErr := errors.New("source platform completion failed")
		lastErr := errors.New("destination platform completion failed")
		owners[0].closeErr = firstErr
		owners[2].closeErr = lastErr
		result := make(chan error, 1)
		go func() { result <- closeRouteClientTransportsAndWait(t.Context(), lifecycles) }()
		for clientIndex, owner := range owners {
			synctest.Wait()
			select {
			case err := <-result:
				t.Fatalf("transition returned before platform %d joined: %v", clientIndex, err)
			default:
			}
			select {
			case <-owner.joinStarted:
			default:
				t.Fatalf("platform %d join was skipped after earlier error", clientIndex)
			}
			owner.release()
		}
		err := <-result
		if !errors.Is(err, firstErr) || !errors.Is(err, lastErr) {
			t.Fatalf("platform completion errors=%v, want both independent errors", err)
		}
	})
}

// Caller cancellation ends the wait but must still close every fallback owner
// and must never become successful forced-route readiness.
func TestCloseRouteClientTransportsCancellationKeepsCloseRequests(t *testing.T) {
	owners := make([]*heldRoutePlatformLifecycle, 3)
	lifecycles := make([]routeClientLifecycle, len(owners))
	for clientIndex := range owners {
		owners[clientIndex] = newHeldRoutePlatformLifecycle(t)
		lifecycles[clientIndex] = owners[clientIndex].lifecycle()
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := closeRouteClientTransportsAndWait(ctx, lifecycles)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled platform transition error=%v, want context cancellation", err)
	}
	for clientIndex, owner := range owners {
		for name, signal := range map[string]<-chan struct{}{
			"close": owner.closeStarted,
			"join":  owner.joinStarted,
		} {
			select {
			case <-signal:
			default:
				t.Errorf("platform %d %s was skipped after cancellation", clientIndex, name)
			}
		}
	}
}
