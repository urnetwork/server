//go:build acklineagetrace

package perfvar

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc64"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	connectserver "github.com/urnetwork/server/v2026/connect"
)

const h1DownloadPacketCapacity = 32768

// Only scalar TCP headers and opaque flow/Transfer identities survive the
// callback. No packet bytes, payload hashes, addresses or errors are retained.
// Repeated physical writes retain their exact original message identity.
type h1DownloadPacketEvent struct {
	AtNS                      int64
	Message, Sequence         clientconnect.Id
	Number, FlowHash          uint64
	Frame, PayloadBytes       int
	TCPSequence, TCPAck       uint32
	Flags                     byte
	ToProvider, Resend, NoAck bool
}

type h1DownloadPacketRecorder struct {
	next, malformed atomic.Uint64
	events          [h1DownloadPacketCapacity]h1DownloadPacketEvent
	published       [h1DownloadPacketCapacity]atomic.Bool
}

func (self *h1DownloadPacketRecorder) observe(observation clientconnect.TransferWireMessageObservation) {
	pack, err := dohTransferDecodePack(observation.TransferFrameBytes)
	if err != nil {
		self.malformed.Add(1)
		return
	}
	if pack == nil {
		return
	}
	for index, frame := range pack.Frames {
		if frame == nil || (frame.MessageType != protocol.MessageType_IpIpPacketToProvider && frame.MessageType != protocol.MessageType_IpIpPacketFromProvider) {
			continue
		}
		message, err := clientconnect.FromFrame(frame)
		if err != nil {
			self.malformed.Add(1)
			continue
		}
		var packet []byte
		switch value := message.(type) {
		case *protocol.IpPacketToProvider:
			packet = value.GetIpPacket().GetPacketBytes()
		case *protocol.IpPacketFromProvider:
			packet = value.GetIpPacket().GetPacketBytes()
		}
		// This exact replay uses IPv4. Do not interpret UDP probes as TCP;
		// unsupported IP or malformed TCP invalidates packet evidence.
		if len(packet) >= 20 && packet[0]>>4 == 4 && packet[9] != 6 {
			continue
		}
		header, ok := dohTransferTCPPacket(packet)
		if !ok || len(pack.MessageId) != 16 || len(pack.SequenceId) != 16 {
			self.malformed.Add(1)
			continue
		}
		ipHeader := int(packet[0]&15) * 4
		toProvider := frame.MessageType == protocol.MessageType_IpIpPacketToProvider
		var tuple [12]byte
		if toProvider {
			copy(tuple[:8], packet[12:20])
			copy(tuple[8:], packet[ipHeader:ipHeader+4])
		} else {
			copy(tuple[:4], packet[16:20])
			copy(tuple[4:8], packet[12:16])
			copy(tuple[8:10], packet[ipHeader+2:ipHeader+4])
			copy(tuple[10:], packet[ipHeader:ipHeader+2])
		}
		event := h1DownloadPacketEvent{AtNS: time.Now().UnixNano(), Number: pack.SequenceNumber, Frame: index,
			FlowHash: crc64.Checksum(tuple[:], dohTerminalTCPChecksum), PayloadBytes: header.PayloadBytes,
			TCPSequence: header.Sequence, TCPAck: binary.BigEndian.Uint32(packet[ipHeader+8:]), Flags: packet[ipHeader+13],
			ToProvider: toProvider, Resend: observation.Resend, NoAck: pack.Nack}
		copy(event.Message[:], pack.MessageId)
		copy(event.Sequence[:], pack.SequenceId)
		slot := self.next.Add(1) - 1
		if slot < h1DownloadPacketCapacity {
			self.events[slot] = event
			self.published[slot].Store(true)
		}
	}
}

func (self *h1DownloadPacketRecorder) dump(t testing.TB, role string, run int) {
	count := self.next.Load()
	var pending uint64
	for index := uint64(0); index < min(count, h1DownloadPacketCapacity); index++ {
		if !self.published[index].Load() {
			pending++
			continue
		}
		event := self.events[index]
		row, _ := json.Marshal(map[string]any{
			"role": role, "run": run, "ordinal": index, "at_ns": event.AtNS,
			"message": progressTraceIdentity(event.Message), "sequence": progressTraceIdentity(event.Sequence),
			"number": event.Number, "frame": event.Frame, "flow": fmt.Sprintf("%016x", event.FlowHash),
			"tcp_sequence": event.TCPSequence, "tcp_ack": event.TCPAck, "flags": event.Flags,
			"payload_bytes": event.PayloadBytes, "to_provider": event.ToProvider, "resend": event.Resend, "no_ack": event.NoAck,
		})
		t.Logf("[h1-download-packet] %s", row)
	}
	t.Logf("[h1-download-packet-header] role=%s run=%d count=%d capacity=%d overflow=%d unpublished=%d malformed=%d baseline_eligible=false",
		role, run, count, h1DownloadPacketCapacity, count-min(count, h1DownloadPacketCapacity), pending, self.malformed.Load())
	if count > h1DownloadPacketCapacity || pending != 0 || self.malformed.Load() != 0 {
		t.Errorf("H1 packet lineage is incomplete: role=%s", role)
	}
}

// Four finite first-event chunks cover retransmit-heavy failures without
// overwriting early evidence. Existing recorder publication/overflow rules
// remain intact. This diagnostic allocation cannot qualify mobile memory.
func newH1DownloadOwnerTrace(run int) *perfvarProgressTrace {
	packets := &h1DownloadPacketRecorder{}
	var next atomic.Uint64
	chunks := new([4]ackReplayRecorder)
	return &perfvarProgressTrace{
		observeForTest: func(event clientconnect.TransferProgressEvent) {
			index := next.Add(1) - 1
			chunks[min(index/ackReplayCapacity, 3)].observe(event)
		},
		configureForTest: func(settings *clientconnect.ClientSettings) {
			previous := settings.SendBufferSettings.TransferWireMessageObserver
			settings.SendBufferSettings.TransferWireMessageObserver = func(event clientconnect.TransferWireMessageObservation) {
				if previous != nil {
					previous(event)
				}
				packets.observe(event)
			}
		},
		dumpForTest: func(t testing.TB, role string) {
			for index := range chunks {
				if chunks[index].next.Load() > 0 {
					chunks[index].dump(t, fmt.Sprintf("%s/run%d/chunk%d", role, run, index))
				}
			}
			packets.dump(t, role, run)
		},
	}
}

type h1DownloadCompletionRecorder struct {
	next      atomic.Uint64
	events    [3]fullTunDownloadCompletionEvent
	published [3]atomic.Bool
}

func (self *h1DownloadCompletionRecorder) observe(event fullTunDownloadCompletionEvent) {
	index := self.next.Add(1) - 1
	if index < uint64(len(self.events)) {
		// The synthetic body's hash and arbitrary error text need not be kept.
		event.Hash = ""
		if event.Err != nil {
			event.Count = -1
			event.Err = nil
		}
		self.events[index] = event
		self.published[index].Store(true)
	}
}

func (self *h1DownloadCompletionRecorder) dump(t testing.TB, run int) {
	count := self.next.Load()
	for index := uint64(0); index < min(count, uint64(len(self.events))); index++ {
		if !self.published[index].Load() {
			t.Error("unpublished H1 application completion event")
			continue
		}
		event := self.events[index]
		t.Logf("[h1-download-completion] run=%d phase=%s at_ns=%d count=%d value=%d baseline_eligible=false", run, event.Phase, event.At.UnixNano(), event.Count, event.Value)
	}
	if count > uint64(len(self.events)) {
		t.Error("H1 application completion event overflow")
	}
}

func h1DownloadOwnerScenario(t testing.TB) perfvarScenario {
	t.Helper()
	values := map[string]string{
		"CONNECT_PERFVAR_ROUTE": "exchange-h1", "CONNECT_PERFVAR_PROFILE": "cell-edge-5m-down-1m-up",
		"CONNECT_PERFVAR_WORKLOAD": "latency-under-load", "CONNECT_PERFVAR_DIRECTION": "download",
		"CONNECT_PERFVAR_RESOURCE": "mobile-surrogate", "CONNECT_PERFVAR_TOPOLOGY": "one-hop",
		"CONNECT_PERFVAR_RUN_COUNT": "2", "CONNECT_PERFVAR_SEED": "20260810", "CONNECT_PERFVAR_EXTENDERS": "0",
	}
	config, err := loadPerfvarConfig(func(key string) string { return values[key] })
	if err != nil {
		t.Fatal(err)
	}
	scenarios, err := resolvePerfvarScenarios(config)
	if err != nil || len(scenarios) != 1 {
		t.Fatalf("H1 download scenario: count=%d err=%v", len(scenarios), err)
	}
	scenario := scenarios[0]
	hash, err := scenario.hash()
	if err != nil || hash != "242471c707f0771d13c5329c076ef40b4db6be33304b2282c77c2e4b6d0549c3" {
		t.Fatalf("failed C3 scenario changed: %s %v", hash, err)
	}
	trace, err := perfvarTraceForRun(scenario, 2)
	if err != nil || trace.IdentityHash != "e22af0738e992d47d13f1b5b3914d107c544da166e8d25c6102a1106059ce407" {
		t.Fatalf("failed C3 impairment identity changed: %+v %v", trace, err)
	}
	return scenario
}

func TestH1DownloadOwnerLineageIdentity(t *testing.T) { h1DownloadOwnerScenario(t) }

func TestH1DownloadOwnerLineagePacketMapping(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			recorder := &h1DownloadPacketRecorder{}
			first, second := dohTransferTestPacket(1234, 443, 0), dohTransferTestPacket(1234, 443, 1)
			binary.BigEndian.PutUint32(second[24:28], 0xfffffffe)
			binary.BigEndian.PutUint32(second[28:32], 99)
			second[33], second[40] = 0x18, 1
			message, sequence := clientconnect.NewId(), clientconnect.NewId()
			pack := &protocol.Pack{MessageId: message[:], SequenceId: sequence[:], SequenceNumber: 115, Frames: []*protocol.Frame{
				{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true, MessageBytes: first},
				{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true, MessageBytes: second},
			}}
			frame := &protocol.TransferFrame{Pack: pack}
			if legacy {
				frame = &protocol.TransferFrame{Frame: clientconnect.RequireToFrame(pack, 1)}
				defer clientconnect.MessagePoolReturn(frame.Frame.MessageBytes)
			}
			wire, err := clientconnect.ProtoMarshal(frame)
			if err != nil {
				t.Fatal(err)
			}
			defer clientconnect.MessagePoolReturn(wire)
			observation := clientconnect.TransferWireMessageObservation{TransferFrameBytes: wire, Resend: true}
			recorder.observe(observation)
			got := recorder.events[1]
			if recorder.next.Load() != 2 || recorder.malformed.Load() != 0 || !recorder.published[1].Load() ||
				got.Message != message || got.Sequence != sequence || got.Number != 115 || got.Frame != 1 || got.PayloadBytes != 1 ||
				got.TCPSequence != 0xfffffffe || got.TCPAck != 99 || got.Flags != 0x18 || !got.Resend || !got.ToProvider || got.NoAck ||
				got.FlowHash != recorder.events[0].FlowHash {
				t.Fatalf("coalesced completion packet lost its owner: %+v", got)
			}
			for range h1DownloadPacketCapacity/2 + 1 {
				recorder.observe(observation)
			}
			if recorder.next.Load() <= h1DownloadPacketCapacity || recorder.events[1] != got {
				t.Fatal("bounded first-event packet prefix was overwritten")
			}
			clear(wire)
			clear(second)
			if recorder.events[1] != got {
				t.Fatal("packet recorder retained borrowed buffers")
			}
		})
	}
}

// Explicitly diagnostic. The original shared-process run1→run2 ordering,
// seeded scenario, timeout and full correctness checks stay in force.
func TestH1DownloadOwnerLineageReplay(t *testing.T) {
	if os.Getenv("CONNECT_PERFVAR_H1_DOWNLOAD_OWNER_LINEAGE") != "1" {
		t.Skip("explicit H1 download owner diagnostic required")
	}
	if os.Getenv("CONNECT_PERFVAR_PROGRESS_TRACE") != "1" ||
		!ackLineageReplayRuntimeSupported(runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.GOMAXPROCS(0)) || runtime.GOMAXPROCS(0) != 8 {
		t.Fatal("H1 download diagnostic requires progress tracing and the original CPU8 runtime")
	}
	if newPerfvarProgressTraceForTest != nil || newFullTunConstructionHooksForTest != nil {
		t.Fatal("H1 download diagnostic factories already owned")
	}
	defer func() { newPerfvarProgressTraceForTest, newFullTunConstructionHooksForTest = nil, nil }()
	scenario := h1DownloadOwnerScenario(t)
	environment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	environment.Run(t, func(t testing.TB) {
		for _, run := range []int{1, 2} {
			completion := &h1DownloadCompletionRecorder{}
			relay, links := &h1DownloadRelayRecorder{}, &h1DownloadLinkRecorder{}
			stopRelay, ok := connectserver.InstallH1RelayLineageObserver(relay.observe)
			if !ok {
				t.Fatal("H1 relay observer already owned")
			}
			defer stopRelay()
			newPerfvarProgressTraceForTest = func() *perfvarProgressTrace { return newH1DownloadOwnerTrace(run) }
			newFullTunConstructionHooksForTest = func() *fullTunConstructionTestHooks {
				return &fullTunConstructionTestHooks{afterStage: func(stage fullTunConstructionStage, path *fullTunPath) error {
					if stage == fullTunConstructionStageRouteReady {
						path.downloadCompletionPhaseForTest = completion.observe
						links.attach(t, path)
					}
					return nil
				}}
			}
			ctx, cancel := context.WithTimeout(context.Background(), perfvarRunTimeout(scenario))
			record, err := measurePerfvarRun(ctx, t, scenario, run)
			cancel()
			stopRelay()
			links.finish(t, run)
			relay.dump(t, run)
			completion.dump(t, run)
			if err != nil {
				t.Fatal(err)
			}
			emitPerfvarRecord(t, record)
			if !record.Correct || record.InvalidReason != "" {
				t.Errorf("H1 download diagnostic failed: run=%d reason=%s invalid=%s baseline_eligible=false", run, record.FailureReason, record.InvalidReason)
			}
		}
	})
}
