// Virtual-time tracker and actual framed-write controls separate inflated
// heartbeat cadence from intentional terminal I/O deadlines under pressure.
package connect

import (
	"context"
	"errors"
	"net"
	"testing"
	"testing/synctest"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
)

// With a dedicated reliable reader paused at downstream capacity, the next
// observed ping can include the pause. It is not permission to stop our pings.
func TestConnectHeartbeatBackpressureCapsFirstObservedPause(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultConnectHandlerSettings()
		tracker := NewPingTracker(settings.PingTrackerCount)
		time.Sleep(45 * time.Second)
		tracker.ReceivePing()
		if got := settings.heartbeatInterval(tracker); got != settings.MaxPingTimeout || settings.ReadTimeout <= got {
			t.Fatalf("observed pause armed heartbeat=%s, want maximum=%s below read timeout=%s", got, settings.MaxPingTimeout, settings.ReadTimeout)
		}
	})
}

// The minimum-of-history filter stops protecting the cadence after every
// sample has seen pressure. Force that replacement without scheduler timing.
func TestConnectHeartbeatBackpressureCapsReplacedHistory(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultConnectHandlerSettings()
		tracker := NewPingTracker(settings.PingTrackerCount)
		time.Sleep(5 * time.Second)
		tracker.ReceivePing()
		for range settings.PingTrackerCount {
			time.Sleep(40 * time.Second)
			tracker.ReceivePing()
		}
		if got := tracker.MinPingTimeout(); got != 40*time.Second {
			t.Fatalf("fixture retained an unstretched history sample: %s", got)
		}
		if got := settings.heartbeatInterval(tracker); got != settings.MaxPingTimeout {
			t.Fatalf("replaced history armed heartbeat=%s, want %s", got, settings.MaxPingTimeout)
		}
	})
}

func TestConnectHeartbeatBackpressurePreservesNaturalCadence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultConnectHandlerSettings()
		tracker := NewPingTracker(settings.PingTrackerCount)
		time.Sleep(5 * time.Second)
		tracker.ReceivePing()
		if got := settings.heartbeatInterval(tracker); got != 5*time.Second {
			t.Fatalf("healthy cadence changed to %s", got)
		}
	})
}

func TestConnectHeartbeatBackpressurePreservesMinimumAndEmptyHistory(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultConnectHandlerSettings()
		tracker := NewPingTracker(settings.PingTrackerCount)
		if got := settings.heartbeatInterval(tracker); got != settings.MinPingTimeout {
			t.Fatalf("empty history lost minimum cadence: %s", got)
		}
		time.Sleep(100 * time.Millisecond)
		tracker.ReceivePing()
		if got := settings.heartbeatInterval(tracker); got != settings.MinPingTimeout {
			t.Fatalf("fast peer bypassed minimum cadence: %s", got)
		}
	})
}

func TestConnectHeartbeatBackpressurePayloadActivityKeepsShortSample(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultConnectHandlerSettings()
		tracker := NewPingTracker(settings.PingTrackerCount)
		time.Sleep(time.Minute)
		tracker.Receive()
		time.Sleep(2 * time.Second)
		tracker.ReceivePing()
		if got := settings.heartbeatInterval(tracker); got != 2*time.Second {
			t.Fatalf("payload activity did not reset the peer observation: %s", got)
		}
	})
}

// A custom zero maximum previously imposed no upper bound; retain that
// compatibility rather than silently creating a new timeout configuration.
func TestConnectHeartbeatBackpressureUnsetMaximumKeepsCompatibility(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultConnectHandlerSettings()
		settings.MaxPingTimeout = 0
		tracker := NewPingTracker(settings.PingTrackerCount)
		time.Sleep(time.Minute)
		tracker.ReceivePing()
		if got := settings.heartbeatInterval(tracker); got != time.Minute {
			t.Fatalf("unset maximum changed explicit custom behavior: %s", got)
		}
	})
}

// The owning H1 batch helper must return a real pipe timeout, release its
// payload and record no successful frame. It must not retry a partial frame.
func TestConnectHeartbeatBackpressureH1WriterTimeoutReturnsOwner(t *testing.T) {
	clientconnect.MessagePoolReturn(clientconnect.MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		writer, peer := net.Pipe()
		defer writer.Close()
		defer peer.Close()
		framed, err := clientconnect.NewFramedMessageConn(writer, clientconnect.H1FramerProtocol, 1024, nil)
		if err != nil {
			t.Fatal(err)
		}
		message := clientconnect.MessagePoolGet(64)
		witness := clientconnect.MessagePoolShareReadOnly(message)
		done := make(chan error, 1)
		sent := 0
		go func() {
			_, err := writeConnectH1UserReadyBatch(context.Background(), framed, nil, make(chan []byte), message, true, 2*time.Second, func(ByteCount) { sent++ })
			done <- err
		}()
		synctest.Wait()
		time.Sleep(2*time.Second - time.Nanosecond)
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("writer returned before its bound: %v", err)
		default:
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		var timeout net.Error
		if err := <-done; !errors.As(err, &timeout) || !timeout.Timeout() || sent != 0 {
			t.Fatalf("write timeout=%v, successful records=%d", err, sent)
		}
		if !clientconnect.MessagePoolReturn(witness) {
			t.Fatal("timed-out H1 batch retained the pooled input")
		}
	})
}

// The H3 heartbeat uses the same production deadline helper with a stream
// driver that actually blocks, rather than a fake returning immediate errors.
func TestConnectHeartbeatBackpressureH3HeartbeatHasWriteBound(t *testing.T) {
	clientconnect.MessagePoolReturn(clientconnect.MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		writer, peer := net.Pipe()
		defer writer.Close()
		defer peer.Close()
		framer := clientconnect.NewFramer(clientconnect.DefaultFramerSettings(1024))
		done := make(chan error, 1)
		go func() {
			done <- writeConnectQuicHeartbeatWithDeadline(framer, writer, 2*time.Second, nil)
		}()
		synctest.Wait()
		time.Sleep(2*time.Second - time.Nanosecond)
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("heartbeat returned before its bound: %v", err)
		default:
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		var timeout net.Error
		if err := <-done; !errors.As(err, &timeout) || !timeout.Timeout() {
			t.Fatalf("heartbeat did not expose the real stream write timeout: %v", err)
		}
	})
}
