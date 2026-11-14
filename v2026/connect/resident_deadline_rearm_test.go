// Every exchange operation receives its full native socket deadline budget.
package connect

import (
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"testing/synctest"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
)

// Records each deadline installed on the synthetic exchange connection.
type exchangeDeadlineRecordingConn struct {
	exchangeDeadlineConn
	readDeadlines  []time.Time
	writeDeadlines []time.Time
}

// Records a read deadline before the synthetic setter result.
func (self *exchangeDeadlineRecordingConn) SetReadDeadline(deadline time.Time) error {
	self.readDeadlines = append(self.readDeadlines, deadline)
	return self.exchangeDeadlineConn.SetReadDeadline(deadline)
}

// Records a write deadline before the synthetic setter result.
func (self *exchangeDeadlineRecordingConn) SetWriteDeadline(deadline time.Time) error {
	self.writeDeadlines = append(self.writeDeadlines, deadline)
	return self.exchangeDeadlineConn.SetWriteDeadline(deadline)
}

// Traffic cannot borrow an earlier operation's deadline or extend its own
// configured budget. Identical absolute deadlines need no extra setter call.
func TestExchangeDeadlinesPreserveEachOperationBudget(t *testing.T) {
	clientconnect.MessagePoolReturn(clientconnect.MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultExchangeSettings()
		conn := &exchangeDeadlineRecordingConn{}
		sender := NewDefaultExchangeBuffer(settings)
		receiver := NewReceiveOnlyExchangeBuffer(settings)
		requireInForce := func(name string, deadlines []time.Time, timeout time.Duration) {
			t.Helper()
			if len(deadlines) == 0 {
				t.Fatalf("no %s deadline was installed", name)
			}
			if remaining := time.Until(deadlines[len(deadlines)-1]); remaining != timeout {
				t.Fatalf("%s ran under a deadline %s away, want %s", name, remaining, timeout)
			}
		}
		start := time.Now()
		frameCount := 0
		for time.Since(start) < settings.ExchangeReadTimeout {
			if err := sender.WriteMessage(conn, clientconnect.MessagePoolGet(37)); err != nil {
				t.Fatal(err)
			}
			requireInForce("write", conn.writeDeadlines, settings.WriteTimeout)
			message, err := receiver.ReadMessage(conn)
			clientconnect.MessagePoolReturn(message)
			if err != nil {
				t.Fatal(err)
			}
			requireInForce("read", conn.readDeadlines, settings.ExchangeReadTimeout)
			frameCount++
			time.Sleep(10 * time.Millisecond)
		}
		t.Logf("frames=%d write_deadlines=%d read_deadlines=%d", frameCount, len(conn.writeDeadlines), len(conn.readDeadlines))
		if len(conn.writeDeadlines) != frameCount || len(conn.readDeadlines) != frameCount {
			t.Fatalf("%d frames installed %d write and %d read deadlines", frameCount, len(conn.writeDeadlines), len(conn.readDeadlines))
		}
	})
}

// A peer that stops reading is cut off at the write's own timeout regardless
// of how much time passed after an earlier successful frame.
func TestExchangeDeadlineStalledWriteIsBoundedByTheWriteTimeout(t *testing.T) {
	clientconnect.MessagePoolReturn(clientconnect.MessagePoolGet(1))
	settings := DefaultExchangeSettings()
	for _, c := range []struct {
		name string
		idle time.Duration
	}{
		{name: "earlier boundary", idle: settings.WriteTimeout / 3},
		{name: "past earlier boundary", idle: settings.WriteTimeout/3 + time.Millisecond},
	} {
		synctest.Test(t, func(t *testing.T) {
			local, remote := net.Pipe()
			defer local.Close()
			defer remote.Close()
			sender := NewDefaultExchangeBuffer(settings)
			// the peer reads the first frame, then stops reading
			read := make(chan error, 1)
			go func() {
				frame := make([]byte, exchangeIOFrameHeaderByteCount+37)
				_, err := io.ReadFull(remote, frame)
				read <- err
			}()
			if err := sender.WriteMessage(local, clientconnect.MessagePoolGet(37)); err != nil {
				t.Fatal(err)
			}
			if err := <-read; err != nil {
				t.Fatal(err)
			}
			time.Sleep(c.idle)
			start := time.Now()
			err := sender.WriteMessage(local, clientconnect.MessagePoolGet(37))
			if !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("stalled write returned %v, want the deadline", err)
			}
			if stalled := time.Since(start); stalled != settings.WriteTimeout {
				t.Fatalf("%s: stalled write was cut off after %s, want %s", c.name, stalled, settings.WriteTimeout)
			}
		})
	}
}
