// Exchange operations retain their entire configured budget after an earlier
// frame. The virtual clock advances only while the pipe owners are blocked.
package connect

import (
	"io"
	"net"
	"testing"
	"testing/synctest"
	"time"

	clientconnect "github.com/urnetwork/connect"
)

// A healthy next frame may arrive after the previous frame's deadline without
// exceeding the new read's budget.
func TestExchangeReadPreservesConfiguredTolerance(t *testing.T) {
	clientconnect.MessagePoolReturn(clientconnect.MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		local, remote := net.Pipe()
		defer local.Close()
		defer remote.Close()
		settings := DefaultExchangeSettings()
		receiver := NewReceiveOnlyExchangeBuffer(settings)
		firstWritten := make(chan error, 1)
		go func() {
			_, err := remote.Write([]byte{0, 1, 0, 0, 7})
			firstWritten <- err
		}()
		message, err := receiver.ReadMessage(local)
		clientconnect.MessagePoolReturn(message)
		if err != nil {
			t.Fatal(err)
		}
		if err := <-firstWritten; err != nil {
			t.Fatal(err)
		}
		time.Sleep(settings.ExchangeReadTimeout / 3)
		arrival := settings.ExchangeReadTimeout * 3 / 4
		secondWritten := make(chan error, 1)
		go func() {
			time.Sleep(arrival)
			_, err := remote.Write([]byte{0, 1, 0, 0, 8})
			secondWritten <- err
		}()
		defer func() {
			local.Close()
			remote.Close()
			<-secondWritten
		}()
		start := time.Now()
		message, err = receiver.ReadMessage(local)
		defer clientconnect.MessagePoolReturn(message)
		if err != nil {
			t.Fatalf("read failed after %s although peer returns at %s within configured %s: %v", time.Since(start), arrival, settings.ExchangeReadTimeout, err)
		}
		if len(message) != 1 || message[0] != 8 {
			t.Fatalf("unexpected message %v", message)
		}
	})
}

// Both singleton and gathered writes may wait for the peer to resume after the
// old deadline, provided they finish within their own budget.
func TestExchangeWritePreservesConfiguredTolerance(t *testing.T) {
	clientconnect.MessagePoolReturn(clientconnect.MessagePoolGet(1))
	for _, batch := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			local, remote := net.Pipe()
			defer local.Close()
			defer remote.Close()
			settings := DefaultExchangeSettings()
			sender := NewDefaultExchangeBuffer(settings)
			write := func() error {
				if batch {
					return sender.WriteMessages(local, [][]byte{clientconnect.MessagePoolGet(1), clientconnect.MessagePoolGet(1)})
				}
				return sender.WriteMessage(local, clientconnect.MessagePoolGet(1))
			}
			frameByteCount := 5
			if batch {
				frameByteCount *= 2
			}
			firstRead := make(chan error, 1)
			go func() {
				_, err := io.ReadFull(remote, make([]byte, frameByteCount))
				firstRead <- err
			}()
			if err := write(); err != nil {
				t.Fatal(err)
			}
			if err := <-firstRead; err != nil {
				t.Fatal(err)
			}
			time.Sleep(settings.WriteTimeout / 3)
			readiness := settings.WriteTimeout * 3 / 4
			secondRead := make(chan error, 1)
			go func() {
				time.Sleep(readiness)
				_, err := io.ReadFull(remote, make([]byte, frameByteCount))
				secondRead <- err
			}()
			defer func() {
				local.Close()
				remote.Close()
				<-secondRead
			}()
			start := time.Now()
			if err := write(); err != nil {
				t.Fatalf("batch=%t: write failed after %s although peer resumes at %s within configured %s: %v", batch, time.Since(start), readiness, settings.WriteTimeout, err)
			}
		})
	}
}
