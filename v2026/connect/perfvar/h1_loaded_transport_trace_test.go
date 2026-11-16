//go:build acklineagetrace

package perfvar

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
)

const (
	h1LoadedPacingCapacity = 512
	h1LoadedPacketCapacity = 8192
	h1LoadedFlowCapacity   = 64
)

type h1LoadedTCPMetadata struct {
	Flow         uint16 `json:"flow"`
	Sequence     uint32 `json:"tcp_sequence"`
	Ack          uint32 `json:"tcp_ack"`
	PayloadBytes uint16 `json:"tcp_payload_bytes"`
	Flags        uint8  `json:"tcp_flags"`
}

type h1LoadedPacketEvent struct {
	Link       string `json:"link"`
	Stage      string `json:"stage"`
	At         int64  `json:"at_unix_nano,string"`
	LinkNumber uint64 `json:"link_number,string"`
	RateReady  int64  `json:"rate_ready_unix_nano,string"`
	Release    int64  `json:"release_unix_nano,string"`
	Drop       uint8  `json:"scheduled_drop"`
	h1LoadedTCPMetadata
}

// Decode generated IPv4/TCP headers only. The private flow key is never
// emitted; canonical endpoint order pairs both directions without raw ports
// or addresses in the evidence. Neither payload nor a borrowed slice survives.
func h1LoadedTCPHeader(packet []byte) (key [12]byte, metadata h1LoadedTCPMetadata, tcp, valid bool) {
	if len(packet) < 20 || packet[0]>>4 != 4 {
		return key, metadata, false, false
	}
	if packet[9] != 6 {
		return key, metadata, false, true
	}
	ipLen := int(packet[0]&15) * 4
	total := int(binary.BigEndian.Uint16(packet[2:4]))
	if ipLen < 20 || total > len(packet) || total < ipLen+20 || binary.BigEndian.Uint16(packet[6:8])&0x3fff != 0 {
		return key, metadata, true, false
	}
	tcpLen := int(packet[ipLen+12]>>4) * 4
	if tcpLen < 20 || ipLen+tcpLen > total {
		return key, metadata, true, false
	}
	var left, right [6]byte
	copy(left[:4], packet[12:16])
	copy(left[4:], packet[ipLen:ipLen+2])
	copy(right[:4], packet[16:20])
	copy(right[4:], packet[ipLen+2:ipLen+4])
	if bytes.Compare(left[:], right[:]) > 0 {
		left, right = right, left
	}
	copy(key[:6], left[:])
	copy(key[6:], right[:])
	metadata.Sequence = binary.BigEndian.Uint32(packet[ipLen+4 : ipLen+8])
	metadata.Ack = binary.BigEndian.Uint32(packet[ipLen+8 : ipLen+12])
	metadata.PayloadBytes = uint16(total - ipLen - tcpLen)
	metadata.Flags = packet[ipLen+13]
	return key, metadata, true, true
}

type h1LoadedTransportRecorder struct {
	loaded                                         atomic.Bool
	provider                                       atomic.Pointer[clientconnect.Client]
	pacingNext                                     atomic.Uint64
	pacingOverflow                                 atomic.Uint64
	pacing                                         [h1LoadedPacingCapacity]clientconnect.AckLineagePacingSnapshot
	pacingPublished                                [h1LoadedPacingCapacity]atomic.Bool
	packetLock                                     sync.Mutex
	packets                                        [h1LoadedPacketCapacity]h1LoadedPacketEvent
	packetCount, packetOverflow, malformed, nonTCP uint64
	flows                                          [h1LoadedFlowCapacity][12]byte
	flowCount                                      int
	flowOverflow                                   uint64
	cleanup                                        []func()
	finishOnce                                     sync.Once
	phases                                         []h1LoadedTransportPhase
}

type h1LoadedTransportPhase struct {
	Phase                  string `json:"phase"`
	At                     int64  `json:"at_unix_nano,string"`
	Provider, Device, Edge h1FailureTCPStackCounters
}

func (r *h1LoadedTransportRecorder) claim(identity clientconnect.AckLineagePacingIdentity) bool {
	provider := r.provider.Load()
	if !r.loaded.Load() || provider == nil || identity.Client != provider.ClientId() || identity.Peer == (clientconnect.Id{}) {
		return false
	}
	if r.pacingNext.Load() >= h1LoadedPacingCapacity {
		r.pacingOverflow.Add(1)
		return false
	}
	return true
}

func (r *h1LoadedTransportRecorder) observePacing(value clientconnect.AckLineagePacingSnapshot) {
	index := r.pacingNext.Add(1) - 1
	if index >= h1LoadedPacingCapacity {
		r.pacingOverflow.Add(1)
		return
	}
	r.pacing[index] = value
	r.pacingPublished[index].Store(true)
}

func (r *h1LoadedTransportRecorder) observePacket(link, stage string, observation linkScheduleObservation, packet []byte) {
	event := h1LoadedPacketEvent{Link: link, Stage: stage, At: time.Now().UnixNano(), LinkNumber: observation.sequence,
		RateReady: probeLineageNanos(observation.rateReadyTime), Release: probeLineageNanos(observation.releaseTime), Drop: uint8(observation.terminalDropCause)}
	var key [12]byte
	var tcp, valid bool
	if packet != nil {
		key, event.h1LoadedTCPMetadata, tcp, valid = h1LoadedTCPHeader(packet)
	}
	r.packetLock.Lock()
	defer r.packetLock.Unlock()
	if packet != nil {
		if !valid {
			r.malformed++
			return
		}
		if !tcp {
			r.nonTCP++
			return
		}
		for i := 0; i < r.flowCount; i++ {
			if r.flows[i] == key {
				event.Flow = uint16(i + 1)
				break
			}
		}
		if event.Flow == 0 {
			if r.flowCount == len(r.flows) {
				r.flowOverflow++
				return
			}
			r.flows[r.flowCount] = key
			r.flowCount++
			event.Flow = uint16(r.flowCount)
		}
	}
	if r.packetCount >= h1LoadedPacketCapacity {
		r.packetOverflow++
		return
	}
	r.packets[r.packetCount] = event
	r.packetCount++
}

func (r *h1LoadedTransportRecorder) attach(t testing.TB, path *fullTunPath, base *fullTunLatencyProbeTestObserver) *fullTunLatencyProbeTestObserver {
	r.provider.Store(path.providerClient)
	network := path.environment.network
	network.stateLock.Lock()
	links := make(map[string]*directionalLink)
	for key, link := range network.links {
		from, to := network.nodes[key.source].tun, network.nodes[key.destination].tun
		label := ""
		switch {
		case from == path.providerCarrierTun && to == path.environment.edgeTun:
			label = "provider-to-edge"
		case from == path.environment.edgeTun && to == path.providerCarrierTun:
			label = "edge-to-provider"
		case from == path.deviceCarrierTun && to == path.environment.edgeTun:
			label = "device-to-edge"
		case from == path.environment.edgeTun && to == path.deviceCarrierTun:
			label = "edge-to-device"
		}
		if label != "" {
			links[label] = link
		}
	}
	network.stateLock.Unlock()
	if len(links) != 4 {
		t.Fatalf("transport observer expected four H1 links, got %d", len(links))
	}
	for label, link := range links {
		cleanup, ok := link.installPacketTraceForTest(func(stage string, observation linkScheduleObservation, packet []byte) {
			r.observePacket(label, stage, observation, packet)
		})
		if !ok {
			t.Fatal("transport packet trace already owned")
		}
		r.cleanup = append(r.cleanup, cleanup)
	}
	phase := base.phase
	base.phase = func(name string) {
		phase(name)
		if len(r.phases) >= 8 {
			t.Error("TCP phase trace overflow")
			return
		}
		r.phases = append(r.phases, h1LoadedTransportPhase{name, time.Now().UnixNano(), h1FailureTCPStats(path.providerCarrierTun), h1FailureTCPStats(path.deviceCarrierTun), h1FailureTCPStats(path.environment.edgeTun)})
		if name == "loaded-start" {
			r.loaded.Store(true)
		}
		if name == "loaded-end" {
			r.loaded.Store(false)
		}
	}
	return base
}

func (r *h1LoadedTransportRecorder) finish(t testing.TB) {
	r.finishOnce.Do(func() {
		r.loaded.Store(false)
		for i := len(r.cleanup) - 1; i >= 0; i-- {
			r.cleanup[i]()
		}
		r.packetLock.Lock()
		defer r.packetLock.Unlock()
		unpublished := uint64(0)
		for i := uint64(0); i < min(r.pacingNext.Load(), h1LoadedPacingCapacity); i++ {
			if !r.pacingPublished[i].Load() {
				unpublished++
			}
		}
		emit := func(kind string, value any) {
			encoded, _ := json.Marshal(value)
			t.Logf("[h1-loaded-%s] %s", kind, encoded)
		}
		emit("transport-header", map[string]any{"packet_capacity": h1LoadedPacketCapacity, "packet_count": r.packetCount, "packet_overflow": r.packetOverflow,
			"pacing_capacity": h1LoadedPacingCapacity, "pacing_count": r.pacingNext.Load(), "pacing_overflow": r.pacingOverflow.Load(), "pacing_unpublished": unpublished,
			"flows": r.flowCount, "flow_overflow": r.flowOverflow, "malformed": r.malformed, "non_tcp": r.nonTCP, "baseline_eligible": false})
		for _, phase := range r.phases {
			emit("tcp-phase", phase)
		}
		for i := uint64(0); i < min(r.pacingNext.Load(), h1LoadedPacingCapacity); i++ {
			if !r.pacingPublished[i].Load() {
				continue
			}
			value := r.pacing[i]
			emit("pacing", map[string]any{"at_unix_nano": strconv.FormatInt(value.AtUnixNano, 10), "sequence": progressTraceIdentity(value.Sequence), "message": progressTraceIdentity(value.Message), "number": value.Number,
				"resend": value.Resend, "no_ack": value.NoAck, "cached_rate": value.CachedRate, "cached_service_rate": value.CachedServiceRate,
				"cached_probe_rate": value.CachedProbeRate, "cached_probe_limit": value.CachedProbeLimit, "cached_age_nanoseconds": value.CachedAge,
				"item_pacing_bytes": value.ItemPacingBytes, "has_service": value.HasService, "window": value.Window})
		}
		for _, packet := range r.packets[:r.packetCount] {
			emit("tcp-packet", packet)
		}
		if r.packetOverflow != 0 || r.pacingOverflow.Load() != 0 || unpublished != 0 || r.flowOverflow != 0 || r.malformed != 0 || r.pacingNext.Load() == 0 {
			t.Error("H1 loaded transport diagnostic incomplete")
		}
	})
}
