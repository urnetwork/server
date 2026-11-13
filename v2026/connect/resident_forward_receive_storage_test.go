package connect

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"syscall"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
)

type privateForwardStoragePair struct {
	connection   *ExchangeConnection
	client, peer net.Conn
	peerDone     <-chan error
	cancel       context.CancelFunc
}

// Both variants run the actual constructor, framing and joined connection owner.
// Only the carrier is a local pipe; the peer always uses the unchanged settings.
func privateOpenForwardStorage(op ExchangeOp, size int, mode string, payloads [][]byte) (*privateForwardStoragePair, error) {
	settings := DefaultExchangeSettingsWithBufferSize(size)
	settings.ExchangePingTimeout = time.Hour
	settings.ExchangeReadTimeout = time.Minute
	settings.ExchangeReadHeaderTimeout = time.Second
	settings.ExchangeWriteHeaderTimeout = time.Second
	client, peer := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	settings.DialContext = func(context.Context, string, string) (net.Conn, error) { return client, nil }
	pair := &privateForwardStoragePair{client: client, peer: peer, peerDone: done, cancel: cancel}
	go func() {
		var serveErr error
		defer func() { peer.Close(); done <- serveErr }()
		buffer := NewDefaultExchangeBuffer(settings)
		header, err := buffer.ReadHeader(ctx, peer)
		if err != nil {
			serveErr = err
			return
		}
		if mode == "refused" {
			return
		}
		if mode == "scripted" {
			// One stream write deliberately carries the echo plus ping/data.
			// The receive buffer must retain any header read-ahead bytes.
			wire := &exchangeFramerStorageConn{limit: -1}
			if err = buffer.WriteHeader(ctx, wire, header); err != nil {
				serveErr = err
				return
			}
			framer := clientconnect.NewFramer(settings.FramerSettings)
			for _, body := range payloads {
				if err = framer.Write(wire, body); err != nil {
					serveErr = err
					return
				}
			}
			if _, err = peer.Write(wire.Bytes()); err != nil {
				serveErr = err
				return
			}
		} else if err = buffer.WriteHeader(ctx, peer, header); err != nil {
			serveErr = err
			return
		}
		_, serveErr = io.Copy(io.Discard, peer)
	}()
	header := ExchangeHeader{Version: 1, ClientId: server.NewId(), ResidentId: server.NewId(), Op: op}
	connection, err := NewExchangeConnection(ctx, header, "forward-storage.invalid", 1, nil, settings)
	pair.connection = connection
	if err != nil {
		closeErr := pair.close()
		if closeErr != nil {
			return nil, errors.Join(err, closeErr)
		}
		return nil, err
	}
	return pair, nil
}

func (p *privateForwardStoragePair) close() error {
	p.cancel()
	p.client.Close()
	p.peer.Close()
	if p.connection != nil {
		p.connection.Close()
	}
	select {
	case err := <-p.peerDone:
		if !isExpectedExchangeFixtureCloseError(err) && !errors.Is(err, io.ErrClosedPipe) {
			return err
		}
		return nil
	case <-time.After(2 * time.Second):
		return fmt.Errorf("synthetic peer did not join")
	}
}

func TestForwardReceiveStorageOperationAndCapacity(t *testing.T) {
	for _, op := range []ExchangeOp{ExchangeOpForward, ExchangeOpTransport} {
		for _, capacity := range []int{0, 1, 4096} {
			t.Run(fmt.Sprintf("op%d_capacity%d", op, capacity), func(t *testing.T) {
				p, err := privateOpenForwardStorage(op, capacity, "live", nil)
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := p.close(); err != nil {
						t.Error(err)
					}
				}()
				c := p.connection
				expectedReceive, expectedReader := capacity, 64*1024
				if op == ExchangeOpForward {
					expectedReceive, expectedReader = 0, 4*1024
				}
				if cap(c.send) != capacity || cap(c.receive) != expectedReceive || c.receiveBuffer.reader.Size() != expectedReader {
					t.Fatalf("unexpected storage: send=%d receive=%d reader=%d", cap(c.send), cap(c.receive), c.receiveBuffer.reader.Size())
				}
				if c.settings.ExchangeBufferSize != capacity || c.settings.ExchangeReadBufferByteCount != 64*1024 {
					t.Fatal("shared settings changed")
				}
				if op == ExchangeOpForward {
					select {
					case _, ok := <-c.receive:
						if ok {
							t.Fatal("forward receive emitted a value")
						}
					case <-time.After(time.Second):
						t.Fatal("forward receive did not close")
					}
				}
			})
		}
	}
}

func TestForwardReceiveStorageReadAheadAndOwners(t *testing.T) {
	clientconnect.MessagePoolReturn(clientconnect.MessagePoolGet(1))
	for _, op := range []ExchangeOp{ExchangeOpForward, ExchangeOpTransport} {
		t.Run(fmt.Sprintf("op%d", op), func(t *testing.T) {
			before := clientconnect.MessagePoolOutstandingCount()
			dataBefore := snapshotExchangeIO("received", "data").frames
			payloads := [][]byte{{}, bytes.Repeat([]byte{0x53}, 8193), {}, bytes.Repeat([]byte{0x71}, 16384), {}}
			p, err := privateOpenForwardStorage(op, 4096, "scripted", payloads)
			if err != nil {
				t.Fatal(err)
			}
			closed := false
			defer func() {
				if !closed {
					if err := p.close(); err != nil {
						t.Error(err)
					}
				}
			}()
			if op == ExchangeOpTransport {
				for _, want := range payloads {
					if len(want) == 0 {
						continue
					}
					select {
					case got := <-p.connection.receive:
						equal := bytes.Equal(got, want)
						clientconnect.MessagePoolReturn(got)
						if !equal {
							t.Fatal("transport payload or order changed")
						}
					case <-time.After(time.Second):
						t.Fatal("transport payload was lost")
					}
				}
			}
			deadline := time.Now().Add(time.Second)
			for snapshotExchangeIO("received", "data").frames-dataBefore < 2 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if snapshotExchangeIO("received", "data").frames-dataBefore != 2 {
				t.Fatal("coalesced frames were not fully consumed")
			}
			if op == ExchangeOpForward {
				select {
				case _, ok := <-p.connection.receive:
					if ok {
						t.Fatal("forward retained unexpected payload")
					}
				default:
					t.Fatal("forward receive remains open")
				}
			}
			if err := p.close(); err != nil {
				t.Fatal(err)
			}
			closed = true
			if after := clientconnect.MessagePoolOutstandingCount(); after != before {
				t.Fatalf("pool owners changed: before=%d after=%d", before, after)
			}
		})
	}
}

func TestForwardReceiveStorageRefusedHeader(t *testing.T) {
	p, err := privateOpenForwardStorage(ExchangeOpForward, 4096, "refused", nil)
	if p != nil || err == nil {
		if p != nil {
			p.close()
		}
		t.Fatal("refused generation returned a live connection")
	}
}

func privateStorageCpuMicros(r *syscall.Rusage) int64 {
	// Timeval uses different field widths across platforms (Darwin Usec is
	// int32). Widen every field before arithmetic, including the seconds sum.
	return (int64(r.Utime.Sec)+int64(r.Stime.Sec))*1_000_000 + int64(r.Utime.Usec) + int64(r.Stime.Usec)
}

func TestForwardReceiveStorageCpuMicros(t *testing.T) {
	for _, test := range []struct {
		name  string
		usage syscall.Rusage
		want  int64
	}{
		{name: "zero"},
		{
			name:  "user only",
			usage: syscall.Rusage{Utime: syscall.Timeval{Sec: 2, Usec: 345_678}},
			want:  2_345_678,
		},
		{
			name:  "system only",
			usage: syscall.Rusage{Stime: syscall.Timeval{Sec: 3, Usec: 654_321}},
			want:  3_654_321,
		},
		{
			name: "mixed seconds and microseconds",
			usage: syscall.Rusage{
				Utime: syscall.Timeval{Sec: 2, Usec: 345_678},
				Stime: syscall.Timeval{Sec: 3, Usec: 654_321},
			},
			want: 5_999_999,
		},
		{
			name: "microsecond carry",
			usage: syscall.Rusage{
				Utime: syscall.Timeval{Usec: 999_999},
				Stime: syscall.Timeval{Usec: 2},
			},
			want: 1_000_001,
		},
		{
			name:  "microsecond total exceeds int32",
			usage: syscall.Rusage{Utime: syscall.Timeval{Sec: 2_148}},
			want:  2_148_000_000,
		},
		{
			name: "seconds sum exceeds int32",
			usage: syscall.Rusage{
				Utime: syscall.Timeval{Sec: 2_000_000_000, Usec: 999_999},
				Stime: syscall.Timeval{Sec: 1_000_000_000, Usec: 2},
			},
			want: 3_000_000_001_000_001,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := privateStorageCpuMicros(&test.usage); got != test.want {
				t.Fatalf("process CPU microseconds = %d, want %d", got, test.want)
			}
		})
	}
}

func TestForwardReceiveStorageChurnProfile(t *testing.T) {
	clientconnect.MessagePoolReturn(clientconnect.MessagePoolGet(1))
	for _, mode := range []string{"refused", "live"} {
		// Warm global pools/codecs once and release every owner before timing.
		run := func() error {
			p, err := privateOpenForwardStorage(ExchangeOpForward, 4096, mode, nil)
			if mode == "refused" {
				if p != nil {
					p.close()
					return fmt.Errorf("refusal accepted")
				}
				if err == nil {
					return fmt.Errorf("refusal lost error")
				}
				return nil
			}
			if err != nil {
				return err
			}
			return p.close()
		}
		for range 16 {
			if err := run(); err != nil {
				t.Fatal(err)
			}
		}
		runtime.GC()
		beforeOutstanding := clientconnect.MessagePoolOutstandingCount()
		var before, after syscall.Rusage
		var mb, ma runtime.MemStats
		runtime.ReadMemStats(&mb)
		_ = syscall.Getrusage(syscall.RUSAGE_SELF, &before)
		started := time.Now()
		const operations = 512
		for range operations {
			if err := run(); err != nil {
				t.Fatal(err)
			}
		}
		elapsed := time.Since(started)
		_ = syscall.Getrusage(syscall.RUSAGE_SELF, &after)
		runtime.ReadMemStats(&ma)
		if clientconnect.MessagePoolOutstandingCount() != beforeOutstanding {
			t.Fatal("profile leaked pooled owners")
		}
		t.Logf("forward_storage_profile mode=%s operations=%d process_cpu_us=%d wall_ns=%d alloc_bytes=%d alloc_count=%d", mode, operations, privateStorageCpuMicros(&after)-privateStorageCpuMicros(&before), elapsed.Nanoseconds(), ma.TotalAlloc-mb.TotalAlloc, ma.Mallocs-mb.Mallocs)
	}
}
