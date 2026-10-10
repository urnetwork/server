//go:build acklineagetrace

package connect

import (
	"hash/crc64"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/server/v2026"
)

const h1RelayLineageTraceEnabled = true

// H1RelayLineageEvent is a diagnostic boundary, not a delivery signal. Only
// scalar metadata survives the call; callbacks must not block. The wire hash
// uses the endpoint progress trace's CRC64 solely to join the same generated
// Transfer frame across independent FIFO owners. It is not a payload dump.
type H1RelayLineageEvent struct {
	Stage                             string
	Client, Peer                      server.Id
	AtNS                              int64
	WireHash                          uint64
	Bytes, QueueLength, QueueCapacity int
	Success                           bool
}

type h1RelayLineageObserver struct {
	join   sync.RWMutex
	closed bool
	call   func(H1RelayLineageEvent)
}

var h1RelayLineageOwner atomic.Pointer[h1RelayLineageObserver]
var h1RelayLineageChecksum = crc64.MakeTable(crc64.ECMA)

// InstallH1RelayLineageObserver is available only in tagged diagnostic builds.
// One owner is allowed. Cleanup fences and joins callbacks before returning.
func InstallH1RelayLineageObserver(call func(H1RelayLineageEvent)) (func(), bool) {
	if call == nil {
		return nil, false
	}
	owner := &h1RelayLineageObserver{call: call}
	if !h1RelayLineageOwner.CompareAndSwap(nil, owner) {
		return nil, false
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			owner.join.Lock()
			owner.closed = true
			h1RelayLineageOwner.CompareAndSwap(owner, nil)
			owner.join.Unlock()
		})
	}, true
}

type h1RelayLineageSpan struct {
	owner *h1RelayLineageObserver
	event H1RelayLineageEvent
}

func beginH1RelayLineage(stage string, client, peer server.Id, wire []byte, length, capacity int) h1RelayLineageSpan {
	owner := h1RelayLineageOwner.Load()
	if owner == nil {
		return h1RelayLineageSpan{}
	}
	span := h1RelayLineageSpan{owner: owner, event: H1RelayLineageEvent{
		Client: client, Peer: peer, Bytes: len(wire), WireHash: crc64.Checksum(wire, h1RelayLineageChecksum),
	}}
	span.end(stage, true, length, capacity)
	return span
}

func (span h1RelayLineageSpan) end(stage string, success bool, length, capacity int) {
	if span.owner == nil {
		return
	}
	span.owner.join.RLock()
	defer span.owner.join.RUnlock()
	if span.owner.closed {
		return
	}
	defer func() { _ = recover() }()
	span.event.Stage, span.event.AtNS, span.event.Success = stage, time.Now().UnixNano(), success
	span.event.QueueLength, span.event.QueueCapacity = length, capacity
	span.owner.call(span.event)
}

// Settings normally cap a batch at 256. Flag rather than allocate an unbounded
// diagnostic scratch if an unrelated caller exceeds that explicit budget.
func beginH1RelayLineageBatch(stage string, client, peer server.Id, messages [][]byte) []h1RelayLineageSpan {
	if h1RelayLineageOwner.Load() == nil {
		return nil
	}
	if len(messages) > 256 {
		beginH1RelayLineage("relay_trace_batch_overflow", client, peer, nil, len(messages), 256)
	}
	spans := make([]h1RelayLineageSpan, min(len(messages), 256))
	for index := range spans {
		spans[index] = beginH1RelayLineage(stage, client, peer, messages[index], len(messages), 256)
	}
	return spans
}

func endH1RelayLineageBatch(spans []h1RelayLineageSpan, stage string, success bool) {
	for _, span := range spans {
		span.end(stage, success, 0, 0)
	}
}
