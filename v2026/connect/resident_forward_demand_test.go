package connect

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

func newForwardDemandFixture() (*ResidentForward, *ExchangeSettings) {
	settings := DefaultExchangeSettingsWithBufferSize(4)
	settings.ForwardBufferSize = 4
	settings.ExchangePingTimeout = time.Hour
	settings.ExchangeReadTimeout = time.Hour
	settings.ExchangeReadHeaderTimeout = time.Minute
	settings.ExchangeWriteHeaderTimeout = time.Minute
	forward := NewResidentForward(context.Background(), &Exchange{settings: settings}, server.NewId())
	return forward, settings
}

func startForwardDemand(forward *ResidentForward, lookup func(context.Context, server.Id, time.Duration) *model.NetworkClientResident) <-chan struct{} {
	done := make(chan struct{})
	go func() { defer close(done); forward.runWithResidentLookup(lookup) }()
	return done
}

func warmForwardDemandPool() {
	// The pool's process-global statistics worker must live outside a bubble.
	clientconnect.MessagePoolReturn(clientconnect.MessagePoolGet(1))
}

// A missing destination with no payload must produce no Redis polling, even
// when this owner remains alive for the full production idle lifetime.
func TestResidentForwardDemandEmptyDoesNotLookup(t *testing.T) {
	warmForwardDemandPool()
	synctest.Test(t, func(t *testing.T) {
		forward, _ := newForwardDemandFixture()
		var calls atomic.Int64
		done := startForwardDemand(forward, func(context.Context, server.Id, time.Duration) *model.NetworkClientResident { calls.Add(1); return nil })
		time.Sleep(15 * time.Minute)
		forward.Cancel()
		<-done
		t.Logf("idle virtual_seconds=900 resident_lookups=%d", calls.Load())
		if got := calls.Load(); got != 0 {
			t.Fatalf("empty forward polled missing destination %d times", got)
		}
	})
}

// Use the real exchange header, socket pumps and pooled payload handoff, then
// disconnect the peer. An idle owner must stop looking up that departed peer.
func TestResidentForwardDemandDisconnectedStopsAndNewWorkRetries(t *testing.T) {
	warmForwardDemandPool()
	synctest.Test(t, func(t *testing.T) {
		forward, settings := newForwardDemandFixture()
		var calls atomic.Int64
		var dials atomic.Int64
		payload := make(chan string, 1)
		serverDone := make(chan struct{})
		settings.DialContext = func(context.Context, string, string) (net.Conn, error) {
			dials.Add(1)
			local, remote := net.Pipe()
			go func() {
				defer close(serverDone)
				defer remote.Close()
				buffer := NewDefaultExchangeBuffer(settings)
				header, err := buffer.ReadHeader(context.Background(), remote)
				if err != nil {
					t.Error(err)
					return
				}
				if err := buffer.WriteHeader(context.Background(), remote, header); err != nil {
					t.Error(err)
					return
				}
				message, err := buffer.ReadMessage(remote)
				if err != nil {
					t.Error(err)
					return
				}
				payload <- string(message)
				clientconnect.MessagePoolReturn(message)
			}()
			return local, nil
		}
		first := clientconnect.MessagePoolCopy([]byte("first packet"))
		firstWitness := retainResidentPoolWitness(first)
		forward.send <- first
		done := startForwardDemand(forward, func(context.Context, server.Id, time.Duration) *model.NetworkClientResident {
			if calls.Add(1) == 1 {
				return &model.NetworkClientResident{ResidentId: server.NewId(), ResidentHost: "fixture.invalid", ResidentInternalPorts: []int{1}}
			}
			return nil
		})
		select {
		case got := <-payload:
			if got != "first packet" {
				t.Errorf("payload changed: %q", got)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("first payload was not delivered")
		}
		<-serverDone
		synctest.Wait()
		before := calls.Load()
		time.Sleep(10 * time.Second)
		idleCalls := calls.Load() - before
		second := clientconnect.MessagePoolCopy([]byte("new work for temporarily missing peer"))
		secondWitness := retainResidentPoolWitness(second)
		forward.send <- second
		time.Sleep(3 * time.Second)
		newCalls := calls.Load() - before - idleCalls
		forward.Cancel()
		<-done
		requireResidentPoolOwnersReturned(t, [][]byte{firstWitness, secondWitness}, "disconnected forward")
		t.Logf("idle virtual_seconds=10 lookups=%d; new_work virtual_seconds=3 lookups=%d; dials=%d", idleCalls, newCalls, dials.Load())
		if idleCalls != 0 {
			t.Errorf("empty disconnected forward performed %d lookups", idleCalls)
		}
		if newCalls == 0 {
			t.Error("new work did not resume missing-peer retries")
		}
		if dials.Load() != 1 {
			t.Errorf("unexpected dial count %d", dials.Load())
		}
	})
}

// Pending data keeps retrying while the peer is absent. When it returns, its
// first packet must stay ahead of later queued packets, with no duplication.
func TestResidentForwardDemandRecoveryPreservesFifo(t *testing.T) {
	warmForwardDemandPool()
	synctest.Test(t, func(t *testing.T) {
		forward, settings := newForwardDemandFixture()
		var calls atomic.Int64
		var available atomic.Bool
		var dials atomic.Int64
		payloads := make(chan string, 4)
		peerStop := make(chan struct{})
		serverDone := make(chan struct{})
		settings.DialContext = func(context.Context, string, string) (net.Conn, error) {
			if dials.Add(1) <= 2 {
				return nil, errors.New("temporary synthetic dial failure")
			}
			local, remote := net.Pipe()
			go func() {
				defer close(serverDone)
				defer remote.Close()
				buffer := NewDefaultExchangeBuffer(settings)
				header, err := buffer.ReadHeader(context.Background(), remote)
				if err != nil {
					t.Error(err)
					return
				}
				if err := buffer.WriteHeader(context.Background(), remote, header); err != nil {
					t.Error(err)
					return
				}
				for range 3 {
					message, err := buffer.ReadMessage(remote)
					if err != nil {
						t.Error(err)
						return
					}
					payloads <- string(message)
					clientconnect.MessagePoolReturn(message)
				}
				<-peerStop
			}()
			return local, nil
		}
		var witnesses [][]byte
		for _, value := range []string{"one", "two", "three"} {
			message := clientconnect.MessagePoolCopy([]byte(value))
			witnesses = append(witnesses, retainResidentPoolWitness(message))
			forward.send <- message
		}
		done := startForwardDemand(forward, func(context.Context, server.Id, time.Duration) *model.NetworkClientResident {
			calls.Add(1)
			if available.Load() {
				return &model.NetworkClientResident{ResidentId: server.NewId(), ResidentHost: "fixture.invalid", ResidentInternalPorts: []int{1}}
			}
			return nil
		})
		time.Sleep(3 * time.Second)
		if calls.Load() < 2 {
			t.Error("queued work did not retry unavailable peer")
		}
		available.Store(true)
		for _, want := range []string{"one", "two", "three"} {
			select {
			case got := <-payloads:
				if got != want {
					t.Errorf("received %q, want %q", got, want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("pending data did not recover")
			}
		}
		forward.Cancel()
		close(peerStop)
		<-done
		<-serverDone
		requireResidentPoolOwnersReturned(t, witnesses, "recovered forward FIFO")
	})
}

// A late first producer must wake discovery without a periodic polling timer.
func TestResidentForwardDemandLatePacketWakesLookup(t *testing.T) {
	warmForwardDemandPool()
	synctest.Test(t, func(t *testing.T) {
		forward, _ := newForwardDemandFixture()
		var calls atomic.Int64
		entered := make(chan struct{}, 1)
		done := startForwardDemand(forward, func(ctx context.Context, _ server.Id, _ time.Duration) *model.NetworkClientResident {
			calls.Add(1)
			select {
			case entered <- struct{}{}:
			default:
			}
			return nil
		})
		time.Sleep(time.Minute)
		idleCalls := calls.Load()
		message := clientconnect.MessagePoolCopy([]byte("late first packet"))
		witness := retainResidentPoolWitness(message)
		forward.send <- message
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Error("new packet did not wake discovery")
		}
		forward.Cancel()
		<-done
		requireResidentPoolOwnerReturned(t, witness, "late first packet")
		if idleCalls != 0 {
			t.Errorf("idle owner performed %d lookups", idleCalls)
		}
		if calls.Load() == 0 {
			t.Error("new packet did not start lookup")
		}
	})
}

// Model cancellation/failure can unwind as a panic. The privately held first
// packet must be returned along with its queued siblings on that path.
func TestResidentForwardDemandLookupPanicReleasesPending(t *testing.T) {
	warmForwardDemandPool()
	synctest.Test(t, func(t *testing.T) {
		forward, _ := newForwardDemandFixture()
		var witnesses [][]byte
		for range 2 {
			message := clientconnect.MessagePoolCopy([]byte("lookup unwind"))
			witnesses = append(witnesses, retainResidentPoolWitness(message))
			forward.send <- message
		}
		recovered := make(chan any, 1)
		go func() {
			defer func() { recovered <- recover() }()
			forward.runWithResidentLookup(func(context.Context, server.Id, time.Duration) *model.NetworkClientResident {
				panic("synthetic lookup failure")
			})
		}()
		if got := <-recovered; got != "synthetic lookup failure" {
			t.Errorf("lookup failure changed: %v", got)
		}
		requireResidentPoolOwnersReturned(t, witnesses, "lookup panic")
		if !forward.IsDone() {
			t.Error("unwound owner remained live")
		}
	})
}

// Canceled owners must not perform discovery merely because the queue is
// ready. Cancellation still drains every admitted pooled reference.
func TestResidentForwardDemandCanceledQueueDoesNotLookup(t *testing.T) {
	warmForwardDemandPool()
	synctest.Test(t, func(t *testing.T) {
		forward, _ := newForwardDemandFixture()
		var calls atomic.Int64
		var witnesses [][]byte
		for range 4 {
			message := clientconnect.MessagePoolCopy([]byte("canceled"))
			witnesses = append(witnesses, retainResidentPoolWitness(message))
			forward.send <- message
		}
		forward.Cancel()
		<-startForwardDemand(forward, func(context.Context, server.Id, time.Duration) *model.NetworkClientResident { calls.Add(1); return nil })
		requireResidentPoolOwnersReturned(t, witnesses, "canceled forward")
		if calls.Load() != 0 {
			t.Errorf("canceled owner made %d lookups", calls.Load())
		}
	})
}

func TestResidentForwardDemandCloseJoinsLookup(t *testing.T) {
	warmForwardDemandPool()
	synctest.Test(t, func(t *testing.T) {
		forward, _ := newForwardDemandFixture()
		message := clientconnect.MessagePoolCopy([]byte("lookup blocked on cancellation"))
		witness := retainResidentPoolWitness(message)
		forward.send <- message
		entered := make(chan struct{})
		done := startForwardDemand(forward, func(ctx context.Context, _ server.Id, _ time.Duration) *model.NetworkClientResident {
			close(entered)
			<-ctx.Done()
			return nil
		})
		<-entered
		forward.Close()
		<-done
		requireResidentPoolOwnerReturned(t, witness, "blocked lookup cancellation")
	})
}
