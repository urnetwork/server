package perfvar

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	clientconnect "github.com/urnetwork/connect"
)

const perfvarProgressTraceCapacity = 4096

// Diagnostic runs cannot qualify throughput/memory or replace a baseline.
func perfvarProgressTraceEnabled() bool {
	return os.Getenv("CONNECT_PERFVAR_PROGRESS_TRACE") == "1"
}

// The fixed tail exists only on opt-in. TryLock prevents diagnostic contention
// from feeding backpressure into a production callback.
type perfvarProgressTrace struct {
	mutex          sync.Mutex
	events         []clientconnect.TransferProgressEvent
	next           uint64
	seen           atomic.Uint64
	dropped        atomic.Uint64
	dumped         sync.Once
	observeForTest func(clientconnect.TransferProgressEvent)
	dumpForTest    func(testing.TB, string)
	// Opt-in diagnostics may attach an endpoint-local logger. Ordinary traces
	// and all production settings keep this nil.
	configureForTest func(*clientconnect.ClientSettings)
}

// The opt-in one-cell diagnostic installs this before constructing clients
// and restores it after their joins. Canonical execution leaves it nil.
var newPerfvarProgressTraceForTest func() *perfvarProgressTrace

func newPerfvarProgressTrace() *perfvarProgressTrace {
	if newPerfvarProgressTraceForTest != nil {
		return newPerfvarProgressTraceForTest()
	}
	if !perfvarProgressTraceEnabled() {
		return nil
	}
	return &perfvarProgressTrace{events: make([]clientconnect.TransferProgressEvent, perfvarProgressTraceCapacity)}
}

func (self *perfvarProgressTrace) configure(settings *clientconnect.ClientSettings) {
	if self == nil {
		return
	}
	settings.SendBufferSettings.ProgressObserver = self.observe
	settings.ReceiveBufferSettings.ProgressObserver = self.observe
	settings.StreamManagerSettings.StreamBufferSettings.P2pTransportSettings.ProgressObserver = self.observe
	if self.configureForTest != nil {
		self.configureForTest(settings)
	}
}

func (self *perfvarProgressTrace) configurePlatform(settings *clientconnect.PlatformTransportSettings) {
	if self != nil {
		settings.ProgressObserver = self.observe
	}
}

func (self *perfvarProgressTrace) observe(event clientconnect.TransferProgressEvent) {
	if self.observeForTest != nil {
		self.observeForTest(event)
		return
	}
	self.seen.Add(1)
	if !self.mutex.TryLock() {
		self.dropped.Add(1)
		return
	}
	self.events[self.next%uint64(len(self.events))] = event
	self.next++
	self.mutex.Unlock()
}

// Snapshot runs outside the packet path. Truncation is explicit: a missing old
// event is not evidence of a missing packet.
func (self *perfvarProgressTrace) snapshot() ([]clientconnect.TransferProgressEvent, uint64, uint64) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	count := min(self.next, uint64(len(self.events)))
	result := make([]clientconnect.TransferProgressEvent, 0, count)
	for index := self.next - count; index < self.next; index++ {
		result = append(result, self.events[index%uint64(len(self.events))])
	}
	return result, self.next - count, self.dropped.Load()
}

func progressTraceIdentity(id clientconnect.Id) string {
	if id == (clientconnect.Id{}) {
		return ""
	}
	digest := sha256.Sum256(id[:])
	return hex.EncodeToString(digest[:8])
}

func (self *perfvarProgressTrace) dump(t testing.TB, role string) {
	if self == nil {
		return
	}
	self.dumped.Do(func() {
		if self.dumpForTest != nil {
			self.dumpForTest(t, role)
			return
		}
		events, overwritten, dropped := self.snapshot()
		header, _ := json.Marshal(map[string]any{
			"record_type": "diagnostic", "kind": "transfer-progress-header", "role": role,
			"capacity": len(self.events), "seen": self.seen.Load(), "overwritten": overwritten,
			"dropped": dropped, "baseline_eligible": false,
		})
		t.Logf("[perfvar-progress] %s", header)
		for _, event := range events {
			row, _ := json.Marshal(map[string]any{
				"record_type": "diagnostic", "kind": "transfer-progress", "role": role,
				"stage": event.Stage, "at_unix_nano": event.AtUnixNano, "elapsed_nanos": event.ElapsedNanos,
				"client": progressTraceIdentity(event.ClientId), "peer": progressTraceIdentity(event.PeerId),
				"sequence": progressTraceIdentity(event.SequenceId), "message": progressTraceIdentity(event.MessageId),
				"sequence_number": event.SequenceNumber, "wire_hash": fmt.Sprintf("%016x", event.WireHash),
				"bytes": event.ByteCount, "queue_length": event.QueueLength, "queue_capacity": event.QueueCapacity,
				"transport": event.TransportType, "no_ack": event.NoAck, "selective": event.Selective,
				"success": event.Success, "error_kind": event.ErrorKind, "outcome": event.Outcome,
			})
			t.Logf("[perfvar-progress] %s", row)
		}
	})
}

func TestPerfvarProgressTraceIsOptInBoundedAndNonblocking(t *testing.T) {
	t.Setenv("CONNECT_PERFVAR_PROGRESS_TRACE", "")
	if newPerfvarProgressTrace() != nil {
		t.Fatal("disabled trace allocated a ring")
	}
	platformSettings := clientconnect.DefaultPlatformTransportSettings()
	var disabled *perfvarProgressTrace
	disabled.configurePlatform(platformSettings)
	if platformSettings.ProgressObserver != nil {
		t.Fatal("disabled trace attached the H1 physical observer")
	}
	t.Setenv("CONNECT_PERFVAR_PROGRESS_TRACE", "1")
	trace := newPerfvarProgressTrace()
	settings := clientconnect.DefaultClientSettings()
	trace.configure(settings)
	trace.configurePlatform(platformSettings)
	if settings.SendBufferSettings.ProgressObserver == nil || settings.ReceiveBufferSettings.ProgressObserver == nil ||
		settings.StreamManagerSettings.StreamBufferSettings.P2pTransportSettings.ProgressObserver == nil ||
		platformSettings.ProgressObserver == nil {
		t.Fatal("trace did not attach all production boundaries")
	}
	for index := range perfvarProgressTraceCapacity + 7 {
		trace.observe(clientconnect.TransferProgressEvent{SequenceNumber: uint64(index)})
	}
	trace.mutex.Lock()
	trace.observe(clientconnect.TransferProgressEvent{SequenceNumber: 99999})
	trace.mutex.Unlock()
	events, overwritten, dropped := trace.snapshot()
	if len(events) != perfvarProgressTraceCapacity || overwritten != 7 || dropped != 1 ||
		events[0].SequenceNumber != 7 || events[len(events)-1].SequenceNumber != perfvarProgressTraceCapacity+6 {
		t.Fatalf("tail length=%d overwritten=%d dropped=%d", len(events), overwritten, dropped)
	}
	if allocations := testing.AllocsPerRun(100, func() { trace.observe(clientconnect.TransferProgressEvent{}) }); allocations != 0 {
		t.Fatalf("trace callback allocated %g objects", allocations)
	}
	if kind := loadPerfvarHostMetadata().MeasurementKind; kind != "diagnostic-transfer-progress" {
		t.Fatalf("trace run can masquerade as baseline measurement: %q", kind)
	}
}
