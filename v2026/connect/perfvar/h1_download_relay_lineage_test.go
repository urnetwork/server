//go:build acklineagetrace

package perfvar

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc64"
	"sync/atomic"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	connectserver "github.com/urnetwork/server/v2026/connect"
)

const (
	h1DownloadRelayCapacity = 262144
	h1DownloadLinkCapacity  = 131072
)

type h1DownloadRelayRecorder struct {
	next      atomic.Uint64
	events    [h1DownloadRelayCapacity]connectserver.H1RelayLineageEvent
	published [h1DownloadRelayCapacity]atomic.Bool
}

func (r *h1DownloadRelayRecorder) observe(event connectserver.H1RelayLineageEvent) {
	index := r.next.Add(1) - 1
	if index < h1DownloadRelayCapacity {
		r.events[index] = event
		r.published[index].Store(true)
	}
}

func (r *h1DownloadRelayRecorder) dump(t testing.TB, run int) {
	count, unpublished, batchOverflow := r.next.Load(), 0, 0
	for index := uint64(0); index < min(count, h1DownloadRelayCapacity); index++ {
		if !r.published[index].Load() {
			unpublished++
			continue
		}
		event := r.events[index]
		if event.Stage == "relay_trace_batch_overflow" {
			batchOverflow++
		}
		encoded, _ := json.Marshal(map[string]any{
			"run": run, "ordinal": index, "stage": event.Stage, "at_ns": fmt.Sprint(event.AtNS),
			"client": progressTraceIdentity(clientconnect.Id(event.Client)), "peer": progressTraceIdentity(clientconnect.Id(event.Peer)),
			"wire_hash": fmt.Sprintf("%016x", event.WireHash), "bytes": event.Bytes, "success": event.Success,
			"queue_length": event.QueueLength, "queue_capacity": event.QueueCapacity,
		})
		t.Logf("[h1-download-relay] %s", encoded)
	}
	t.Logf("[h1-download-relay-header] run=%d count=%d capacity=%d overflow=%d unpublished=%d batch_overflow=%d baseline_eligible=false",
		run, count, h1DownloadRelayCapacity, count-min(count, h1DownloadRelayCapacity), unpublished, batchOverflow)
	if count > h1DownloadRelayCapacity || unpublished != 0 || batchOverflow != 0 {
		t.Error("H1 relay evidence incomplete")
	}
}

type h1DownloadLinkEvent struct {
	Link, Stage                  string
	AtNS, RateReadyNS, ReleaseNS int64
	Number, FlowHash             uint64
	TCPSequence, TCPAck          uint32
	PayloadBytes, Window         uint16
	Flags, Drop                  uint8
}

// Scalar-only first-event storage. No unbounded flow map and no packet or
// payload copies: reverse directions share a canonical private-tuple hash.
type h1DownloadLinkRecorder struct {
	next, malformed, nonTCP atomic.Uint64
	events                  [h1DownloadLinkCapacity]h1DownloadLinkEvent
	published               [h1DownloadLinkCapacity]atomic.Bool
	cleanup                 []func()
}

func (r *h1DownloadLinkRecorder) observe(link, stage string, observation linkScheduleObservation, packet []byte) {
	event := h1DownloadLinkEvent{Link: link, Stage: stage, AtNS: time.Now().UnixNano(), Number: observation.sequence,
		RateReadyNS: probeLineageNanos(observation.rateReadyTime), ReleaseNS: probeLineageNanos(observation.releaseTime), Drop: uint8(observation.terminalDropCause)}
	if packet != nil {
		key, metadata, tcp, valid := h1LoadedTCPHeader(packet)
		if !valid {
			r.malformed.Add(1)
			return
		}
		if !tcp {
			r.nonTCP.Add(1)
			return
		}
		event.FlowHash = crc64.Checksum(key[:], dohTerminalTCPChecksum)
		event.TCPSequence, event.TCPAck, event.PayloadBytes, event.Flags = metadata.Sequence, metadata.Ack, metadata.PayloadBytes, metadata.Flags
		event.Window = binary.BigEndian.Uint16(packet[int(packet[0]&15)*4+14:])
	}
	index := r.next.Add(1) - 1
	if index < h1DownloadLinkCapacity {
		r.events[index] = event
		r.published[index].Store(true)
	}
}

func (r *h1DownloadLinkRecorder) attach(t testing.TB, path *fullTunPath) {
	network := path.environment.network
	network.stateLock.Lock()
	links := make(map[string]*directionalLink)
	for key, link := range network.links {
		from, to := network.nodes[key.source].tun, network.nodes[key.destination].tun
		label := ""
		switch {
		case from == path.deviceCarrierTun && to == path.environment.edgeTun:
			label = "device-to-edge"
		case from == path.environment.edgeTun && to == path.deviceCarrierTun:
			label = "edge-to-device"
		case from == path.providerCarrierTun && to == path.environment.edgeTun:
			label = "provider-to-edge"
		case from == path.environment.edgeTun && to == path.providerCarrierTun:
			label = "edge-to-provider"
		}
		if label != "" {
			links[label] = link
		}
	}
	network.stateLock.Unlock()
	if len(links) != 4 {
		t.Fatalf("H1 lineage requires four links: got=%d", len(links))
	}
	for label, link := range links {
		cleanup, ok := link.installPacketTraceForTest(func(stage string, observation linkScheduleObservation, packet []byte) {
			r.observe(label, stage, observation, packet)
		})
		if !ok {
			t.Fatal("H1 lineage link observer already owned")
		}
		r.cleanup = append(r.cleanup, cleanup)
	}
}

func (r *h1DownloadLinkRecorder) finish(t testing.TB, run int) {
	for index := len(r.cleanup) - 1; index >= 0; index-- {
		r.cleanup[index]()
	}
	r.cleanup = nil
	count, unpublished := r.next.Load(), 0
	for index := uint64(0); index < min(count, h1DownloadLinkCapacity); index++ {
		if !r.published[index].Load() {
			unpublished++
			continue
		}
		event := r.events[index]
		encoded, _ := json.Marshal(map[string]any{
			"run": run, "ordinal": index, "link": event.Link, "stage": event.Stage, "number": event.Number, "at_ns": fmt.Sprint(event.AtNS),
			"rate_ready_ns": fmt.Sprint(event.RateReadyNS), "release_ns": fmt.Sprint(event.ReleaseNS), "drop": event.Drop,
			"flow": fmt.Sprintf("%016x", event.FlowHash), "tcp_sequence": event.TCPSequence, "tcp_ack": event.TCPAck,
			"payload_bytes": event.PayloadBytes, "flags": event.Flags, "window": event.Window,
		})
		t.Logf("[h1-download-link] %s", encoded)
	}
	t.Logf("[h1-download-link-header] run=%d count=%d capacity=%d overflow=%d unpublished=%d malformed=%d non_tcp=%d baseline_eligible=false",
		run, count, h1DownloadLinkCapacity, count-min(count, h1DownloadLinkCapacity), unpublished, r.malformed.Load(), r.nonTCP.Load())
	if count > h1DownloadLinkCapacity || unpublished != 0 || r.malformed.Load() != 0 {
		t.Error("H1 link evidence incomplete")
	}
}

func TestH1DownloadOwnerLineageLinkMapping(t *testing.T) {
	r := &h1DownloadLinkRecorder{}
	packet := dohTransferTestPacket(1234, 443, 1)
	binary.BigEndian.PutUint32(packet[24:28], 0xfffffffe)
	binary.BigEndian.PutUint32(packet[28:32], 99)
	binary.BigEndian.PutUint16(packet[34:36], 37)
	packet[33] = 0x18
	observation := linkScheduleObservation{sequence: 73, scheduleTime: time.Now(), rateReadyTime: time.Now(), releaseTime: time.Now()}
	r.observe("device-to-edge", "scheduled", observation, packet)
	first := r.events[0]
	if first.Number != 73 || first.TCPSequence != 0xfffffffe || first.TCPAck != 99 || first.PayloadBytes != 1 || first.Window != 37 || first.Flags != 0x18 || first.FlowHash == 0 {
		t.Fatalf("outer packet mapping changed: %+v", first)
	}
	// Reverse endpoint order must preserve flow identity without retaining its
	// addresses/ports. Terminal delivery can only join by link sequence.
	copy(packet[12:20], []byte{packet[16], packet[17], packet[18], packet[19], packet[12], packet[13], packet[14], packet[15]})
	copy(packet[20:24], []byte{packet[22], packet[23], packet[20], packet[21]})
	r.observe("edge-to-device", "delivery-offer", observation, packet)
	r.observe("device-to-edge", "delivered", observation, nil)
	if r.events[1].FlowHash != first.FlowHash || r.events[2].Number != 73 || r.events[2].FlowHash != 0 {
		t.Fatal("link ownership join changed")
	}
	clear(packet)
	if r.events[0] != first {
		t.Fatal("link trace retained borrowed packet")
	}
	for range h1DownloadLinkCapacity {
		r.observe("device-to-edge", "delivered", observation, nil)
	}
	if r.next.Load() <= h1DownloadLinkCapacity || r.events[0] != first {
		t.Fatal("bounded link prefix was overwritten")
	}
}
