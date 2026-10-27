package server_test

// Exercises the public clock projection with real go-redis clients and an
// in-memory RESP peer. No service, resource configuration, or socket is used.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Applies each increment before optionally closing its connection without a
// reply. Every dial is a net.Pipe; only synthetic commands reach this peer.
type clockCounterPeer struct {
	stateLock   sync.Mutex
	connections []net.Conn
	closing     bool
	workers     sync.WaitGroup
	loseReply   atomic.Bool
	dials       atomic.Int64
	increments  atomic.Int64
	byteCount   atomic.Int64
}

// A closed net.Pipe rejects a new read deadline before Read can report EOF.
// Let that Read observe the lost reply regardless of the peer's close timing.
type clockCounterConnection struct{ net.Conn }

// Suppresses only the pipe's closed-read-deadline setter result; command reads
// and writes remain real, and the peer still applies the increment before close.
func (self clockCounterConnection) SetReadDeadline(deadline time.Time) error {
	err := self.Conn.SetReadDeadline(deadline)
	if errors.Is(err, io.ErrClosedPipe) {
		return nil
	}
	return err
}

// Installs both retry policies so the public entry point selects its real
// wrapper. The ordinary client's four retries match the production default.
func newClockCounterPeer(t *testing.T, loseReply bool) *clockCounterPeer {
	t.Helper()
	peer := &clockCounterPeer{}
	peer.loseReply.Store(loseReply)
	t.Cleanup(func() {
		peer.stateLock.Lock()
		peer.closing = true
		for _, connection := range peer.connections {
			connection.Close()
		}
		peer.stateLock.Unlock()
		peer.workers.Wait()
	})
	newClient := func(retries int) *redis.Client {
		client := redis.NewClient(&redis.Options{
			Addr: "192.0.2.1:6379", Protocol: 2, DisableIdentity: true,
			MaxRetries: retries, MinRetryBackoff: -1, MaxRetryBackoff: -1,
			DialerRetries: 1, Dialer: peer.dial, PoolSize: 1, MaxActiveConns: 1,
			ReadTimeout: time.Second, WriteTimeout: time.Second,
			PoolTimeout: time.Second, DialTimeout: time.Second,
		})
		t.Cleanup(func() { client.Close() })
		return client
	}
	server.TestingClockCounterClients(t, newClient(4), newClient(-1))
	return peer
}

// Parses bounded RESP arrays, returns valid initialization and integer replies,
// and forces the lost-reply ordering after increment application.
func (self *clockCounterPeer) dial(context.Context, string, string) (net.Conn, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closing {
		return nil, net.ErrClosed
	}
	self.dials.Add(1)
	peer, client := net.Pipe()
	self.connections = append(self.connections, peer, client)
	self.workers.Add(1)
	go func() {
		defer self.workers.Done()
		defer peer.Close()
		reader := bufio.NewReader(peer)
		for {
			header, err := reader.ReadString('\n')
			if err != nil || !strings.HasPrefix(header, "*") {
				return
			}
			count, err := strconv.Atoi(strings.TrimSpace(header[1:]))
			if err != nil || count < 1 || 32 < count {
				return
			}
			args := make([]string, count)
			for i := range args {
				header, err := reader.ReadString('\n')
				if err != nil || !strings.HasPrefix(header, "$") {
					return
				}
				length, err := strconv.Atoi(strings.TrimSpace(header[1:]))
				if err != nil || length < 0 || 16384 < length {
					return
				}
				value := make([]byte, length+2)
				if _, err := io.ReadFull(reader, value); err != nil {
					return
				}
				args[i] = string(value[:length])
			}
			reply := "+OK\r\n"
			switch strings.ToLower(args[0]) {
			case "hello":
				reply = "*0\r\n"
			case "ping":
				reply = "+PONG\r\n"
			case "incrby":
				if len(args) != 3 {
					return
				}
				increment, err := strconv.ParseInt(args[2], 10, 64)
				if err != nil {
					return
				}
				self.increments.Add(1)
				byteCount := self.byteCount.Add(increment)
				if self.loseReply.Swap(false) {
					return
				}
				reply = fmt.Sprintf(":%d\r\n", byteCount)
			}
			if _, err := io.WriteString(peer, reply); err != nil {
				return
			}
		}
	}()
	return clockCounterConnection{Conn: client}, nil
}

// Retains the exact error value raised by the public projection boundary.
func clockCounterPanic(run func()) (recovered any) {
	defer func() { recovered = recover() }()
	run()
	return nil
}

// A command read fails only after the peer applied it. Both callback and driver
// retries must be disabled; a successful retry would conceal the double count.
func TestClockCounterNeverReplaysLostReply(t *testing.T) {
	peer := newClockCounterPeer(t, true)
	recovered := clockCounterPanic(func() {
		model.AddClockTransferByteCount(t.Context(), 7)
	})
	err, _ := recovered.(error)
	t.Logf("applied_increments=%d bytes=%d returned_error=%v", peer.increments.Load(), peer.byteCount.Load(), recovered)
	if peer.increments.Load() != 1 || peer.byteCount.Load() != 7 {
		t.Fatalf("lost reply replayed the clock increment: increments=%d bytes=%d", peer.increments.Load(), peer.byteCount.Load())
	}
	if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("ambiguous result was not surfaced: %v", recovered)
	}
}

// An acknowledged positive update still applies once on the selected pool.
func TestClockCounterAcknowledgedIncrement(t *testing.T) {
	peer := newClockCounterPeer(t, false)
	model.AddClockTransferByteCount(t.Context(), 7)
	if peer.increments.Load() != 1 || peer.byteCount.Load() != 7 {
		t.Fatalf("acknowledged update changed: increments=%d bytes=%d", peer.increments.Load(), peer.byteCount.Load())
	}
}

// Nonpositive inputs must not open a client or issue a command.
func TestClockCounterNonpositiveDoesNotDial(t *testing.T) {
	peer := newClockCounterPeer(t, false)
	model.AddClockTransferByteCount(t.Context(), 0)
	model.AddClockTransferByteCount(t.Context(), -1)
	if peer.dials.Load() != 0 || peer.increments.Load() != 0 {
		t.Fatalf("nonpositive input performed work: dials=%d increments=%d", peer.dials.Load(), peer.increments.Load())
	}
}

// A canceled direct caller fails before any connection or increment attempt.
func TestClockCounterCanceledBeforeAdmission(t *testing.T) {
	peer := newClockCounterPeer(t, false)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	recovered := clockCounterPanic(func() {
		model.AddClockTransferByteCount(ctx, 7)
	})
	err, _ := recovered.(error)
	if !errors.Is(err, context.Canceled) || peer.dials.Load() != 0 || peer.increments.Load() != 0 {
		t.Fatalf("canceled caller performed work or lost cause: dials=%d increments=%d error=%v", peer.dials.Load(), peer.increments.Load(), recovered)
	}
}
