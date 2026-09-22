package connect

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	connectlib "github.com/urnetwork/connect"
)

type exchangeFramerStorageConn struct {
	bytes.Buffer
	writes int
	limit  int
	err    error
}

func (c *exchangeFramerStorageConn) Write(p []byte) (int, error) {
	c.writes++
	n := len(p)
	if 0 <= c.limit {
		n = min(n, c.limit)
	}
	_, _ = c.Buffer.Write(p[:n])
	return n, c.err
}
func (c *exchangeFramerStorageConn) Close() error                     { return nil }
func (c *exchangeFramerStorageConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *exchangeFramerStorageConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c *exchangeFramerStorageConn) SetDeadline(time.Time) error      { return nil }
func (c *exchangeFramerStorageConn) SetReadDeadline(time.Time) error  { return nil }
func (c *exchangeFramerStorageConn) SetWriteDeadline(time.Time) error { return nil }

func TestExchangeSingletonFramerStorageIsLazyAndBounded(t *testing.T) {
	settings := DefaultExchangeSettings()
	sender := NewDefaultExchangeBuffer(settings)
	receiver := NewReceiveOnlyExchangeBuffer(settings)
	if sender.writeStorage != nil || receiver.writeStorage != nil {
		t.Fatal("constructor retained write scratch")
	}
	conn := &exchangeFramerStorageConn{limit: -1}
	if err := sender.WriteMessage(conn, nil); err != nil {
		t.Fatal(err)
	}
	if sender.writeStorage != nil || conn.writes != 1 {
		t.Fatal("idle heartbeat allocated payload scratch or made extra writes")
	}
	conn.Reset()
	conn.writes = 0
	for _, size := range []int{1200, 777, 0, 3000, 1200} {
		payload := bytes.Repeat([]byte{byte(size)}, size)
		message := connectlib.MessagePoolCopy(payload)
		witness := connectlib.MessagePoolShareReadOnly(message)
		beforeWrites := conn.writes
		if err := sender.WriteMessages(conn, [][]byte{message}); err != nil {
			t.Fatal(err)
		}
		if size != 0 && !connectlib.MessagePoolReturn(witness) {
			t.Fatal("singleton did not release its message owner")
		}
		if size == 0 {
			connectlib.MessagePoolReturn(witness)
		}
		wantWrites := 1
		if size+4 > exchangeSingletonWriteStorageByteCount {
			wantWrites = 2
		}
		if conn.writes-beforeWrites != wantWrites {
			t.Fatalf("size %d: writes=%d want=%d", size, conn.writes-beforeWrites, wantWrites)
		}
		if len(sender.writeStorage) != exchangeSingletonWriteStorageByteCount {
			t.Fatal("scratch grew beyond its packet-sized bound")
		}
		got, err := receiver.ReadMessage(conn)
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("size %d: framed payload changed: %v", size, err)
		}
		connectlib.MessagePoolReturn(got)
		if receiver.writeStorage != nil {
			t.Fatal("reader acquired write scratch")
		}
	}
}

func TestExchangeLargeOrInvalidSingletonDoesNotAllocateScratch(t *testing.T) {
	for _, tc := range []struct {
		max, size int
		wantError bool
	}{{4096, 3000, false}, {8, 9, true}} {
		settings := DefaultExchangeSettings()
		settings.FramerSettings.MaxMessageLen = tc.max
		sender := NewDefaultExchangeBuffer(settings)
		conn := &exchangeFramerStorageConn{limit: -1}
		message := connectlib.MessagePoolGet(tc.size)
		witness := connectlib.MessagePoolShareReadOnly(message)
		err := sender.WriteMessage(conn, message)
		if (err != nil) != tc.wantError {
			t.Fatalf("size %d error=%v", tc.size, err)
		}
		if !connectlib.MessagePoolReturn(witness) {
			t.Fatal("message owner was not returned")
		}
		if sender.writeStorage != nil {
			t.Fatal("no-benefit singleton allocated payload scratch")
		}
		if tc.wantError && conn.writes != 0 {
			t.Fatal("invalid singleton wrote a prefix")
		}
	}
}

func TestExchangeSingletonStorageErrorsReturnOwnership(t *testing.T) {
	injected := errors.New("injected singleton write failure")
	for _, tc := range []struct {
		limit     int
		err, want error
	}{{0, nil, io.ErrShortWrite}, {600, nil, io.ErrShortWrite}, {0, injected, injected}, {-1, injected, injected}} {
		sender := NewDefaultExchangeBuffer(DefaultExchangeSettings())
		conn := &exchangeFramerStorageConn{limit: tc.limit, err: tc.err}
		message := connectlib.MessagePoolGet(1200)
		witness := connectlib.MessagePoolShareReadOnly(message)
		if err := sender.WriteMessage(conn, message); !errors.Is(err, tc.want) {
			t.Fatalf("error=%v want=%v", err, tc.want)
		}
		if !connectlib.MessagePoolReturn(witness) {
			t.Fatal("failed singleton retained message ownership")
		}
		if conn.writes != 1 {
			t.Fatal("failure made a second write")
		}
	}
}

func TestExchangeReadyBatchKeepsGatheredWritesWithoutSingletonScratch(t *testing.T) {
	sender := NewDefaultExchangeBuffer(DefaultExchangeSettings())
	conn := &exchangeFramerStorageConn{limit: -1}
	messages := [][]byte{connectlib.MessagePoolGet(1200), connectlib.MessagePoolGet(1200)}
	if err := sender.WriteMessages(conn, messages); err != nil {
		t.Fatal(err)
	}
	if sender.writeStorage != nil {
		t.Fatal("ready writev batch allocated singleton scratch")
	}
	// A generic wrapper deliberately exposes the existing four header/body
	// writes; bare TCP takes net.Buffers' single writev instead.
	if conn.writes != 4 {
		t.Fatalf("gathered batch path changed: writes=%d", conn.writes)
	}
}

func TestExchangeSingletonStorageSteadyWritesDoNotAllocate(t *testing.T) {
	sender := NewDefaultExchangeBuffer(DefaultExchangeSettings())
	conn := &exchangeFramerStorageConn{limit: -1}
	payload := make([]byte, 1200)
	allocations := testing.AllocsPerRun(1000, func() {
		conn.Reset()
		if err := sender.WriteMessage(conn, payload); err != nil {
			panic(err)
		}
	})
	if allocations != 0 {
		t.Fatalf("allocations/singleton=%g", allocations)
	}
}
