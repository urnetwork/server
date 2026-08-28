// This file verifies latency-probe sequence recovery independently of the
// wall-clock network profiles used by the measurement campaign.
package perfvar

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// One scripted read has either one complete datagram or one terminal error.
type latencyProbeScriptRead struct {
	packet []byte
	err    error
}

// A deterministic datagram-shaped connection records writes and returns the
// exact scripted read sequence without scheduler or timer dependencies.
type latencyProbeScriptConn struct {
	stateLock sync.Mutex
	reads     []latencyProbeScriptRead
	writes    [][]byte
}

// Records one complete probe datagram.
func (self *latencyProbeScriptConn) Write(packet []byte) (int, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.writes = append(self.writes, append([]byte(nil), packet...))
	return len(packet), nil
}

// Returns one complete scripted datagram or error.
func (self *latencyProbeScriptConn) Read(packet []byte) (int, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if len(self.reads) == 0 {
		return 0, io.EOF
	}
	read := self.reads[0]
	self.reads = self.reads[1:]
	if read.err != nil {
		return 0, read.err
	}
	return copy(packet, read.packet), nil
}

// No resources are retained by the scripted connection.
func (self *latencyProbeScriptConn) Close() error { return nil }

// The scripted endpoint identity is deliberately absent.
func (self *latencyProbeScriptConn) LocalAddr() net.Addr { return nil }

// The scripted endpoint identity is deliberately absent.
func (self *latencyProbeScriptConn) RemoteAddr() net.Addr { return nil }

// Scripted reads carry their own terminal result.
func (self *latencyProbeScriptConn) SetDeadline(time.Time) error { return nil }

// Scripted reads carry their own terminal result.
func (self *latencyProbeScriptConn) SetReadDeadline(time.Time) error { return nil }

// Scripted writes complete synchronously.
func (self *latencyProbeScriptConn) SetWriteDeadline(time.Time) error { return nil }

// Builds the exact 32-byte payload used on the probe wire.
func latencyProbeTestPacket(sequence uint64) []byte {
	packet := make([]byte, 32)
	binary.BigEndian.PutUint64(packet, sequence)
	return packet
}

// A late response from a timed-out probe must not poison the next sequence.
func TestLatencyProbeIgnoresWellFormedStaleResponse(t *testing.T) {
	connection := &latencyProbeScriptConn{
		reads: []latencyProbeScriptRead{
			{err: context.DeadlineExceeded},
			{packet: latencyProbeTestPacket(1000)},
			{packet: latencyProbeTestPacket(1001)},
		},
	}
	if _, err := runLatencyProbe(context.Background(), connection, 1000, time.Second); err == nil {
		t.Fatal("first probe did not report its scripted timeout")
	}
	if _, err := runLatencyProbe(context.Background(), connection, 1001, time.Second); err != nil {
		t.Fatalf("next probe rejected a stale predecessor: %v", err)
	}
	if len(connection.writes) != 2 {
		t.Fatalf("probe writes=%d want=2", len(connection.writes))
	}
}

// A future or malformed response remains a hard integrity failure.
func TestLatencyProbeRejectsNonStaleSequenceMismatch(t *testing.T) {
	connection := &latencyProbeScriptConn{
		reads: []latencyProbeScriptRead{{packet: latencyProbeTestPacket(43)}},
	}
	_, err := runLatencyProbe(context.Background(), connection, 42, time.Second)
	if err == nil || !strings.Contains(err.Error(), "corrupted") {
		t.Fatalf("future response error=%v, want corruption", err)
	}
}

// Response delay cannot reduce the offered probe count or hide timeouts from
// the loaded-phase accounting.
func TestLoadedLatencyProbeStateAccountsFixedOfferedTrain(t *testing.T) {
	timeout := 3 * time.Second
	state := newLoadedLatencyProbeState(timeout)
	startTime := time.Unix(100, 0)
	for sequence := uint64(1000); sequence < 1004; sequence += 1 {
		state.attempt(
			sequence,
			startTime.Add(time.Duration(sequence-1000)*500*time.Millisecond),
			nil,
		)
	}

	var response [32]byte
	binary.BigEndian.PutUint64(response[:], 1002)
	state.receive(response, startTime.Add(1300*time.Millisecond))
	binary.BigEndian.PutUint64(response[:], 1000)
	state.receive(response, startTime.Add(1500*time.Millisecond))
	state.expire(startTime.Add(5 * time.Second))
	state.finish()

	if state.samples.attemptCount != 4 ||
		len(state.samples.latencies) != 2 ||
		state.samples.failureCount != 2 {
		t.Fatalf("fixed-rate samples=%+v", state.samples)
	}
	if state.samples.latencies[0] != 300*time.Millisecond ||
		state.samples.latencies[1] != 1500*time.Millisecond {
		t.Fatalf("out-of-order latencies=%v", state.samples.latencies)
	}
}

func TestLoadedLatencyProbeIntervalBoundsOfferedRate(t *testing.T) {
	if interval := loadedLatencyProbeIntervalForRate(256*1024, 250_000); interval != 500*time.Millisecond {
		t.Fatalf("one-bar probe interval=%s want=500ms", interval)
	}
	if interval := loadedLatencyProbeIntervalForRate(1024*1024, 1_000_000_000); interval != time.Millisecond {
		t.Fatalf("clean probe interval=%s want=1ms", interval)
	}
}

func TestLoadedLatencyProbeStateRejectsResponsePastDeadline(t *testing.T) {
	state := newLoadedLatencyProbeState(3 * time.Second)
	startTime := time.Unix(200, 0)
	state.attempt(1000, startTime, nil)
	var response [32]byte
	binary.BigEndian.PutUint64(response[:], 1000)
	state.receive(response, startTime.Add(3*time.Second+time.Nanosecond))
	if state.samples.attemptCount != 1 ||
		len(state.samples.latencies) != 0 ||
		state.samples.failureCount != 1 ||
		!errors.Is(state.samples.firstFailure, context.DeadlineExceeded) {
		t.Fatalf("late-response samples=%+v", state.samples)
	}
}
