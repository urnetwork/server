// Bounded fixture-only Ready observations separate modeled delay from forwarding.
// They retain protocol counts and process identity, never SQL or backend secrets.
package model

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const legacyCohortDiagnosticReadyLimit = 65536
const legacyCohortDiagnosticConnectionLimit = 128

// Times keep their monotonic component for in-process phase correlation. JSON
// exposes wall times; neither a process id nor these timings grants ownership.
type legacyCohortReadyDiagnostic struct {
	Ordinal            uint64    `json:"ordinal"`
	TransactionOrdinal uint64    `json:"transaction_ordinal"`
	TransactionStatus  string    `json:"transaction_status"`
	MixedTransactions  bool      `json:"mixed_transactions"`
	BeginCount         uint64    `json:"begin_count"`
	CommitCount        uint64    `json:"commit_count"`
	RollbackCount      uint64    `json:"rollback_count"`
	EndKind            string    `json:"end_kind,omitempty"`
	Frames             uint64    `json:"frames"`
	Bytes              uint64    `json:"bytes"`
	RequestedDelayNs   int64     `json:"requested_delay_ns"`
	ReadyReceived      time.Time `json:"ready_received"`
	TimerStart         time.Time `json:"timer_start"`
	TimerDone          time.Time `json:"timer_done"`
	ForwardDone        time.Time `json:"forward_done"`
	Forwarded          bool      `json:"forwarded"`
	StopCause          string    `json:"stop_cause,omitempty"`
}

type legacyCohortConnectionDiagnosticSnapshot struct {
	ConnectionOrdinal uint64                        `json:"connection_ordinal"`
	BackendPid        uint32                        `json:"backend_pid"`
	ReadyTotal        uint64                        `json:"ready_total"`
	DroppedReady      uint64                        `json:"dropped_ready"`
	Closed            bool                          `json:"closed"`
	Ready             []legacyCohortReadyDiagnostic `json:"ready"`
}

type legacyCohortProxyDiagnosticSnapshot struct {
	Enabled            bool                                       `json:"enabled"`
	ReadyLimit         uint64                                     `json:"ready_limit"`
	ConnectionLimit    int                                        `json:"connection_limit"`
	ReadyTotal         uint64                                     `json:"ready_total"`
	ReadyRecorded      uint64                                     `json:"ready_recorded"`
	PendingReady       uint64                                     `json:"pending_ready"`
	DroppedReady       uint64                                     `json:"dropped_ready"`
	ConnectionTotal    uint64                                     `json:"connection_total"`
	DroppedConnections uint64                                     `json:"dropped_connections"`
	Overflow           bool                                       `json:"overflow"`
	Connections        []legacyCohortConnectionDiagnosticSnapshot `json:"connections"`
}

// One forwarding goroutine writes each connection. Its mutex only coordinates
// snapshots; unrelated forwarding goroutines never take this same mutex.
type legacyCohortConnectionDiagnosticCapture struct {
	stateLock         sync.Mutex
	connectionOrdinal uint64
	backendPid        uint32
	readyTotal        uint64
	droppedReady      uint64
	closed            bool
	ready             []legacyCohortReadyDiagnostic
}

// Capacity is reserved atomically and never reused. Earliest observations
// survive later singleton traffic; exceeding either bound is explicit evidence.
type legacyCohortProxyDiagnosticCapture struct {
	readyLimit         uint64
	connectionLimit    int
	readyTotal         atomic.Uint64
	readyReserved      atomic.Uint64
	droppedReady       atomic.Uint64
	stateLock          sync.Mutex
	connectionTotal    uint64
	droppedConnections uint64
	connections        []*legacyCohortConnectionDiagnosticCapture
}

// Repeated enabling preserves the original window and its earliest records.
func (self *legacyCohortLatencyProxy) enableDiagnosticCapture() {
	self.diagnosticCapture.CompareAndSwap(nil, &legacyCohortProxyDiagnosticCapture{
		readyLimit: legacyCohortDiagnosticReadyLimit, connectionLimit: legacyCohortDiagnosticConnectionLimit,
	})
}

// The registered connection list is copied before individual locks are taken.
// A live snapshot reports records not yet included as pending. Joined snapshots
// establish the complete window without holding a lock across connections.
func (self *legacyCohortLatencyProxy) diagnosticSnapshot() legacyCohortProxyDiagnosticSnapshot {
	capture := self.diagnosticCapture.Load()
	if capture == nil {
		return legacyCohortProxyDiagnosticSnapshot{}
	}
	result := legacyCohortProxyDiagnosticSnapshot{Enabled: true, ReadyLimit: capture.readyLimit, ConnectionLimit: capture.connectionLimit}
	var connections []*legacyCohortConnectionDiagnosticCapture
	func() {
		capture.stateLock.Lock()
		defer capture.stateLock.Unlock()
		result.ConnectionTotal, result.DroppedConnections = capture.connectionTotal, capture.droppedConnections
		connections = slices.Clone(capture.connections)
	}()
	for _, connection := range connections {
		func() {
			connection.stateLock.Lock()
			defer connection.stateLock.Unlock()
			result.Connections = append(result.Connections, legacyCohortConnectionDiagnosticSnapshot{
				ConnectionOrdinal: connection.connectionOrdinal, BackendPid: connection.backendPid,
				ReadyTotal: connection.readyTotal, DroppedReady: connection.droppedReady, Closed: connection.closed,
				Ready: slices.Clone(connection.ready),
			})
			result.ReadyRecorded += uint64(len(connection.ready))
		}()
	}
	slices.SortFunc(result.Connections, func(a, b legacyCohortConnectionDiagnosticSnapshot) int {
		if a.ConnectionOrdinal < b.ConnectionOrdinal {
			return -1
		}
		if a.ConnectionOrdinal > b.ConnectionOrdinal {
			return 1
		}
		return 0
	})
	result.DroppedReady = capture.droppedReady.Load()
	result.ReadyTotal = capture.readyTotal.Load()
	result.PendingReady = result.ReadyTotal - result.ReadyRecorded - result.DroppedReady
	result.Overflow = result.DroppedReady > 0 || result.DroppedConnections > 0
	return result
}

func (self *legacyCohortProxyDiagnosticCapture) connection(ordinal uint64, backendPid uint32) *legacyCohortConnectionDiagnosticCapture {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.connectionTotal++
	if len(self.connections) >= self.connectionLimit {
		self.droppedConnections++
		return nil
	}
	connection := &legacyCohortConnectionDiagnosticCapture{connectionOrdinal: ordinal, backendPid: backendPid}
	self.connections = append(self.connections, connection)
	return connection
}

// A refused connection still contributes every enabled Ready to dropped counts.
func (self *legacyCohortProxyDiagnosticCapture) beginReady(connection *legacyCohortConnectionDiagnosticCapture) bool {
	self.readyTotal.Add(1)
	reserved := false
	if connection != nil {
		for count := self.readyReserved.Load(); count < self.readyLimit; count = self.readyReserved.Load() {
			if self.readyReserved.CompareAndSwap(count, count+1) {
				reserved = true
				break
			}
		}
		func() {
			connection.stateLock.Lock()
			defer connection.stateLock.Unlock()
			connection.readyTotal++
			if !reserved {
				connection.droppedReady++
			}
		}()
	}
	if !reserved {
		self.droppedReady.Add(1)
	}
	return reserved
}

func (self *legacyCohortConnectionDiagnosticCapture) setBackendPid(backendPid uint32) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.backendPid = backendPid
}

func (self *legacyCohortConnectionDiagnosticCapture) finishReady(value legacyCohortReadyDiagnostic) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.ready = append(self.ready, value)
}

func (self *legacyCohortConnectionDiagnosticCapture) close() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.closed = true
}

// Transaction identity survives its COMMIT/ROLLBACK until the following Ready.
// An unusual cycle spanning two transactions is marked instead of misattributed.
type legacyCohortDiagnosticFrames struct {
	nextTransaction   uint64
	activeTransaction uint64
	readyOrdinal      uint64
	cycle             legacyCohortReadyDiagnostic
}

func (self *legacyCohortDiagnosticFrames) observe(kind byte, body []byte, enabled bool) {
	if enabled {
		self.cycle.Frames++
		self.cycle.Bytes += uint64(5 + len(body))
	}
	if kind != 'C' {
		return
	}
	switch {
	case bytes.Equal(body, []byte("BEGIN\x00")):
		self.nextTransaction++
		self.activeTransaction = self.nextTransaction
		if enabled {
			if self.cycle.TransactionOrdinal != 0 && self.cycle.TransactionOrdinal != self.activeTransaction {
				self.cycle.MixedTransactions = true
			}
			self.cycle.TransactionOrdinal = self.activeTransaction
			self.cycle.BeginCount++
		}
	case bytes.Equal(body, []byte("COMMIT\x00")), bytes.Equal(body, []byte("ROLLBACK\x00")):
		if enabled {
			if self.cycle.TransactionOrdinal == 0 {
				self.cycle.TransactionOrdinal = self.activeTransaction
			}
			if bytes.Equal(body, []byte("COMMIT\x00")) {
				self.cycle.CommitCount++
				self.cycle.EndKind = "commit"
			} else {
				self.cycle.RollbackCount++
				self.cycle.EndKind = "rollback"
			}
		}
		self.activeTransaction = 0
	}
}

func (self *legacyCohortDiagnosticFrames) resetCycle() {
	self.cycle = legacyCohortReadyDiagnostic{TransactionOrdinal: self.activeTransaction}
}

func (self *legacyCohortDiagnosticFrames) ready(received time.Time, body []byte) legacyCohortReadyDiagnostic {
	self.readyOrdinal++
	value := self.cycle
	value.Ordinal, value.ReadyReceived = self.readyOrdinal, received
	value.TransactionStatus = "unknown"
	if len(body) == 1 {
		switch body[0] {
		case 'I':
			value.TransactionStatus = "I"
		case 'T':
			value.TransactionStatus = "T"
		case 'E':
			value.TransactionStatus = "E"
		}
	}
	self.resetCycle()
	return value
}

func legacyCohortDiagnosticFrame(kind byte, body []byte) []byte {
	frame := make([]byte, 5+len(body))
	frame[0] = kind
	binary.BigEndian.PutUint32(frame[1:5], uint32(4+len(body)))
	copy(frame[5:], body)
	return frame
}

// Actual TCP fragmentation exercises the existing proxy parser and forwarding.
// Closing the synthetic backend supplies EOF; close joins every forwarding actor.
func legacyCohortDiagnosticExchange(t testing.TB, ctx context.Context, proxy *legacyCohortLatencyProxy, listener *net.TCPListener, frames [][]byte) []byte {
	t.Helper()
	deadline, _ := ctx.Deadline()
	if err := listener.SetDeadline(deadline); err != nil {
		t.Fatal("synthetic backend deadline failed", err)
	}
	client, err := (&net.Dialer{}).DialContext(ctx, "tcp", proxy.listener.Addr().String())
	if err != nil {
		t.Fatal("synthetic proxy connection failed", err)
	}
	defer client.Close()
	if err := client.SetDeadline(deadline); err != nil {
		t.Fatal("synthetic client deadline failed", err)
	}
	backend, err := listener.AcceptTCP()
	if err != nil {
		t.Fatal("synthetic backend accept failed", err)
	}
	defer backend.Close()
	if err := backend.SetDeadline(deadline); err != nil {
		t.Fatal("synthetic backend connection deadline failed", err)
	}
	joined := make(chan error, 1)
	go func() {
		var writeErr error
		defer func() { _ = backend.Close(); joined <- writeErr }()
		for _, frame := range frames {
			for offset := 0; offset < len(frame); {
				end := min(offset+3, len(frame))
				count, err := backend.Write(frame[offset:end])
				if err != nil {
					writeErr = err
					return
				}
				if count == 0 {
					writeErr = io.ErrShortWrite
					return
				}
				offset += count
			}
		}
	}()
	received, readErr := io.ReadAll(client)
	writeErr := <-joined
	if readErr != nil || writeErr != nil {
		t.Fatal("synthetic frame exchange failed", readErr, writeErr)
	}
	return received
}

// Every modeled bucket is observed on real framing. The oracle compares exact
// bytes, identities, counts and event order, never elapsed throughput or speed.
func TestLegacyCohortReadyDiagnosticPreservesFramingAndJitter(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal("synthetic backend listen failed", err)
	}
	defer listener.Close()
	proxy := newLegacyCohortLatencyProxy(t, ctx, listener.Addr().String())
	defer proxy.close()
	proxy.enableDiagnosticCapture()
	proxy.enableDiagnosticCapture()
	proxy.jitter.Store(true)
	proxy.enabled.Store(true)
	key := make([]byte, 8)
	binary.BigEndian.PutUint32(key[:4], 101)
	binary.BigEndian.PutUint32(key[4:], 0x10203040)
	cycles := [][][]byte{
		{legacyCohortDiagnosticFrame('K', key), legacyCohortDiagnosticFrame('C', []byte("BEGIN\x00")), legacyCohortDiagnosticFrame('Z', []byte("T"))},
		{legacyCohortDiagnosticFrame('C', []byte("SELECT 8\x00")), legacyCohortDiagnosticFrame('Z', []byte("T"))},
		{legacyCohortDiagnosticFrame('C', []byte("COMMIT\x00")), legacyCohortDiagnosticFrame('Z', []byte("I"))},
		{legacyCohortDiagnosticFrame('C', []byte("BEGIN\x00")), legacyCohortDiagnosticFrame('C', []byte("ROLLBACK\x00")), legacyCohortDiagnosticFrame('Z', []byte("I"))},
		{legacyCohortDiagnosticFrame('Z', []byte("I"))},
	}
	var frames [][]byte
	for _, cycle := range cycles {
		frames = append(frames, cycle...)
	}
	received := legacyCohortDiagnosticExchange(t, ctx, proxy, listener, frames)
	proxy.close()
	if !bytes.Equal(received, bytes.Join(frames, nil)) {
		t.Fatal("diagnostic capture changed forwarded bytes")
	}
	snapshot := proxy.diagnosticSnapshot()
	if !snapshot.Enabled || snapshot.Overflow || snapshot.PendingReady != 0 || snapshot.ReadyTotal != 5 || snapshot.ReadyRecorded != 5 || snapshot.DroppedReady != 0 ||
		snapshot.ConnectionTotal != 1 || snapshot.DroppedConnections != 0 || len(snapshot.Connections) != 1 {
		t.Fatal("diagnostic capture lost complete finite framing", snapshot)
	}
	connection := snapshot.Connections[0]
	if connection.ConnectionOrdinal != 1 || connection.BackendPid != 101 || !connection.Closed || connection.ReadyTotal != 5 || connection.DroppedReady != 0 || len(connection.Ready) != 5 {
		t.Fatal("diagnostic capture lost backend identity or connection custody", connection)
	}
	transactions := []uint64{1, 1, 1, 2, 0}
	begins, commits, rollbacks := []uint64{1, 0, 0, 1, 0}, []uint64{0, 0, 1, 0, 0}, []uint64{0, 0, 0, 1, 0}
	statuses, ends := []string{"T", "T", "I", "I", "I"}, []string{"", "", "commit", "rollback", ""}
	for index, event := range connection.Ready {
		var byteCount uint64
		for _, frame := range cycles[index] {
			byteCount += uint64(len(frame))
		}
		if event.Ordinal != uint64(index+1) || event.TransactionOrdinal != transactions[index] || event.TransactionStatus != statuses[index] || event.MixedTransactions ||
			event.BeginCount != begins[index] || event.CommitCount != commits[index] || event.RollbackCount != rollbacks[index] || event.EndKind != ends[index] ||
			event.Frames != uint64(len(cycles[index])) || event.Bytes != byteCount || event.RequestedDelayNs != int64(time.Duration(index+1)*time.Millisecond) ||
			!event.Forwarded || event.StopCause != "" || event.ReadyReceived.IsZero() || event.TimerStart.IsZero() || event.TimerDone.IsZero() || event.ForwardDone.IsZero() ||
			event.TimerStart.Before(event.ReadyReceived) || event.TimerDone.Before(event.TimerStart) || event.ForwardDone.Before(event.TimerDone) {
			t.Fatal("diagnostic event changed a Ready boundary or its ordering", index, event)
		}
		if index > 0 && event.ReadyReceived.Before(connection.Ready[index-1].ForwardDone) {
			t.Fatal("connection reordered sequential Ready forwarding", index)
		}
	}
	wire := proxy.snapshot()
	if wire["ready_replies_observed"] != 5 || wire["ready_replies_charged_delay"] != 5 || wire["begin_commands_observed"] != 2 ||
		wire["commit_commands_observed"] != 1 || wire["rollback_commands_observed"] != 1 || wire["connections_closed_after_begin_without_end_command"] != 0 {
		t.Fatal("diagnostic capture changed existing protocol counters", wire)
	}
	for bucket := 1; bucket <= 5; bucket++ {
		if wire[fmt.Sprintf("ready_jitter_charged_%dms", bucket)] != 1 || wire[fmt.Sprintf("ready_jitter_applied_%dms", bucket)] != 1 {
			t.Fatal("diagnostic capture changed a modeled jitter bucket", bucket, wire)
		}
	}
}

// Exhausting either bound retains the earliest records and names every dropped
// Ready. Bounded observation cannot silently manufacture a complete trace.
func TestLegacyCohortReadyDiagnosticOverflowRetainsEarliestRecords(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal("synthetic backend listen failed", err)
	}
	defer listener.Close()
	proxy := newLegacyCohortLatencyProxy(t, ctx, listener.Addr().String())
	defer proxy.close()
	proxy.diagnosticCapture.Store(&legacyCohortProxyDiagnosticCapture{readyLimit: 2, connectionLimit: 1})
	proxy.enabled.Store(true)
	for _, count := range []int{3, 1} {
		var frames [][]byte
		for range count {
			frames = append(frames, legacyCohortDiagnosticFrame('Z', []byte("I")))
		}
		if received := legacyCohortDiagnosticExchange(t, ctx, proxy, listener, frames); !bytes.Equal(received, bytes.Join(frames, nil)) {
			t.Fatal("overflow changed forwarded framing")
		}
	}
	proxy.close()
	snapshot := proxy.diagnosticSnapshot()
	if !snapshot.Overflow || snapshot.ReadyTotal != 4 || snapshot.ReadyRecorded != 2 || snapshot.DroppedReady != 2 || snapshot.PendingReady != 0 ||
		snapshot.ConnectionTotal != 2 || snapshot.DroppedConnections != 1 || len(snapshot.Connections) != 1 {
		t.Fatal("overflow was not completely accounted", snapshot)
	}
	connection := snapshot.Connections[0]
	if connection.ReadyTotal != 3 || connection.DroppedReady != 1 || len(connection.Ready) != 2 || connection.Ready[0].Ordinal != 1 || connection.Ready[1].Ordinal != 2 {
		t.Fatal("overflow replaced early observations", connection)
	}
	for _, event := range connection.Ready {
		if event.RequestedDelayNs != 0 || !event.TimerStart.IsZero() || !event.TimerDone.IsZero() || !event.Forwarded || event.StopCause != "" ||
			event.ReadyReceived.IsZero() || event.ForwardDone.Before(event.ReadyReceived) {
			t.Fatal("zero-delay observation introduced a timer or lost forwarding", event)
		}
	}
	if wire := proxy.snapshot(); wire["ready_replies_observed"] != 4 || wire["ready_replies_charged_delay"] != 0 {
		t.Fatal("overflow changed protocol observation or added delay", wire)
	}
}

// Optional capture cannot retain a connection or Ready unless explicitly enabled.
func TestLegacyCohortReadyDiagnosticDisabledKeepsNoTrace(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal("synthetic backend listen failed", err)
	}
	defer listener.Close()
	proxy := newLegacyCohortLatencyProxy(t, ctx, listener.Addr().String())
	defer proxy.close()
	proxy.enabled.Store(true)
	frames := [][]byte{legacyCohortDiagnosticFrame('Z', []byte("I"))}
	if received := legacyCohortDiagnosticExchange(t, ctx, proxy, listener, frames); !bytes.Equal(received, frames[0]) {
		t.Fatal("disabled capture changed forwarding")
	}
	proxy.close()
	snapshot := proxy.diagnosticSnapshot()
	if snapshot.Enabled || snapshot.ReadyTotal != 0 || snapshot.ReadyRecorded != 0 || snapshot.PendingReady != 0 || snapshot.ConnectionTotal != 0 || len(snapshot.Connections) != 0 {
		t.Fatal("disabled capture retained a diagnostic trace", snapshot)
	}
	if wire := proxy.snapshot(); wire["ready_replies_observed"] != 1 || wire["ready_replies_charged_delay"] != 0 {
		t.Fatal("disabled capture changed existing observation", wire)
	}
}

// A reserved record cannot count as a completed reply. Multiple transaction
// identities within one Ready cycle remain explicitly unsuitable for correlation.
func TestLegacyCohortReadyDiagnosticPendingAndMixedTransactions(t *testing.T) {
	proxy := &legacyCohortLatencyProxy{}
	proxy.enableDiagnosticCapture()
	capture := proxy.diagnosticCapture.Load()
	connection := capture.connection(1, 101)
	if !capture.beginReady(connection) {
		t.Fatal("empty diagnostic window refused its first Ready")
	}
	if snapshot := proxy.diagnosticSnapshot(); snapshot.ReadyTotal != 1 || snapshot.ReadyRecorded != 0 || snapshot.PendingReady != 1 || snapshot.Overflow {
		t.Fatal("unfinished Ready was reported as complete", snapshot)
	}
	var frames legacyCohortDiagnosticFrames
	for _, command := range []string{"BEGIN\x00", "COMMIT\x00", "BEGIN\x00", "ROLLBACK\x00"} {
		frames.observe('C', []byte(command), true)
	}
	frames.observe('Z', []byte("I"), true)
	now := time.Unix(1, 0)
	value := frames.ready(now, []byte("I"))
	value.ForwardDone, value.Forwarded = now, true
	connection.finishReady(value)
	connection.close()
	snapshot := proxy.diagnosticSnapshot()
	if snapshot.ReadyTotal != 1 || snapshot.ReadyRecorded != 1 || snapshot.PendingReady != 0 || snapshot.Overflow || len(snapshot.Connections) != 1 || len(snapshot.Connections[0].Ready) != 1 {
		t.Fatal("completed Ready did not finish observation custody", snapshot)
	}
	event := snapshot.Connections[0].Ready[0]
	if !event.MixedTransactions || event.BeginCount != 2 || event.CommitCount != 1 || event.RollbackCount != 1 || event.EndKind != "rollback" || event.TransactionOrdinal != 2 {
		t.Fatal("multiple transactions were silently assigned to one owner", event)
	}
	frames.observe('Z', []byte("I"), true)
	if next := frames.ready(now, []byte("I")); next.TransactionOrdinal != 0 || next.MixedTransactions || next.BeginCount != 0 || next.CommitCount != 0 || next.RollbackCount != 0 || next.EndKind != "" {
		t.Fatal("ended transaction leaked into the next idle exchange", next)
	}
}
