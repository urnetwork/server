// Exchange socket deadlines are re-armed once a third of their timeout has
// passed, not per frame, and still bound every read and write.
package connect

import (
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"testing/synctest"
	"time"

	clientconnect "github.com/urnetwork/connect"
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

// A busy exchange socket re-arms each deadline once a third of its timeout has
// passed, not on every frame: each re-arm modifies a runtime timer. Every read
// and write still runs under a deadline between two thirds of its timeout and
// its timeout away.
func TestExchangeDeadlinesRearmOnlyAfterAThirdOfTheTimeout(t *testing.T) {
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
			if remaining := time.Until(deadlines[len(deadlines)-1]); remaining < 2*timeout/3 || timeout < remaining {
				t.Fatalf("%s ran under a deadline %s away, want between %s and %s", name, remaining, 2*timeout/3, timeout)
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
			frameCount += 1
			time.Sleep(10 * time.Millisecond)
		}
		t.Logf("frames=%d write_deadlines=%d read_deadlines=%d", frameCount, len(conn.writeDeadlines), len(conn.readDeadlines))
		// 30 s of frames re-arm the 15 s write deadline just after 0, 5, 10,
		// 15, 20 and 25 s, and the 30 s read deadline just after 0, 10 and 20 s.
		if len(conn.writeDeadlines) != 6 || len(conn.readDeadlines) != 3 {
			t.Fatalf("%d frames installed %d write and %d read deadlines, want 6 and 3", frameCount, len(conn.writeDeadlines), len(conn.readDeadlines))
		}
	})
}

// A write to a peer that stopped reading is still cut off by the deadline in
// force: two thirds of WriteTimeout after it starts when the deadline was kept,
// the worst case, and WriteTimeout when the write re-armed it.
func TestExchangeDeadlineStalledWriteIsBoundedByTheWriteTimeout(t *testing.T) {
	clientconnect.MessagePoolReturn(clientconnect.MessagePoolGet(1))
	settings := DefaultExchangeSettings()
	for _, c := range []struct {
		name    string
		idle    time.Duration
		stalled time.Duration
	}{
		{name: "deadline kept", idle: settings.WriteTimeout / 3, stalled: 2 * settings.WriteTimeout / 3},
		{name: "deadline re-armed", idle: settings.WriteTimeout/3 + time.Millisecond, stalled: settings.WriteTimeout},
	} {
		t.Run(c.name, func(t *testing.T) {
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
				if stalled := time.Since(start); stalled != c.stalled {
					t.Fatalf("stalled write was cut off after %s, want %s", stalled, c.stalled)
				}
			})
		})
	}
}
