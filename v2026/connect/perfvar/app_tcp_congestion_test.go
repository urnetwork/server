// These diagnostic counters are read only at existing carrier boundaries.
// They do not change packet handling, boundary readiness, or workload gates.
package perfvar

import (
	"encoding/json"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
)

// The app TUN owns the inner TCP endpoint, separate from exchange H1's outer
// TCP stacks. These counts cover every app TCP connection in the interval,
// including concurrent probes; the host egress peer is not a gVisor endpoint.
type perfvarAppTCPCounters struct {
	SegmentsSent         uint64 `json:"segments_sent"`
	SegmentsReceived     uint64 `json:"segments_received"`
	Retransmits          uint64 `json:"retransmits"`
	Timeouts             uint64 `json:"timeouts"`
	FastRetransmits      uint64 `json:"fast_retransmits"`
	SlowStartRetransmits uint64 `json:"slow_start_retransmits"`
}

type perfvarAppTCPBoundary struct {
	tun      *clientconnect.Tun
	counters perfvarAppTCPCounters
}

type perfvarAppTCPObservation struct {
	Available         bool `json:"available"`
	GenerationChanged bool `json:"generation_changed"`
	perfvarAppTCPCounters
}

func snapshotPerfvarAppTCP(tun *clientconnect.Tun) perfvarAppTCPBoundary {
	if tun == nil {
		return perfvarAppTCPBoundary{}
	}
	stats := tun.Stats().TCP
	return perfvarAppTCPBoundary{tun: tun, counters: perfvarAppTCPCounters{
		SegmentsSent:         stats.SegmentsSent.Value(),
		SegmentsReceived:     stats.ValidSegmentsReceived.Value(),
		Retransmits:          stats.Retransmits.Value(),
		Timeouts:             stats.Timeouts.Value(),
		FastRetransmits:      stats.FastRetransmit.Value(),
		SlowStartRetransmits: stats.SlowStartRetransmits.Value(),
	}}
}

func subtractPerfvarAppTCP(before, after perfvarAppTCPBoundary) perfvarAppTCPObservation {
	observation := perfvarAppTCPObservation{
		Available:         before.tun != nil && after.tun != nil,
		GenerationChanged: before.tun != after.tun,
	}
	if !observation.Available || observation.GenerationChanged {
		return observation
	}
	observation.perfvarAppTCPCounters = perfvarAppTCPCounters{
		SegmentsSent:         after.counters.SegmentsSent - before.counters.SegmentsSent,
		SegmentsReceived:     after.counters.SegmentsReceived - before.counters.SegmentsReceived,
		Retransmits:          after.counters.Retransmits - before.counters.Retransmits,
		Timeouts:             after.counters.Timeouts - before.counters.Timeouts,
		FastRetransmits:      after.counters.FastRetransmits - before.counters.FastRetransmits,
		SlowStartRetransmits: after.counters.SlowStartRetransmits - before.counters.SlowStartRetransmits,
	}
	return observation
}

type perfvarProviderCongestionBoundary struct {
	provider *clientconnect.RemoteUserNatProvider
	counters clientconnect.ProviderCongestionDrops
}

type perfvarProviderCongestionObservation struct {
	Available              bool  `json:"available"`
	GenerationChanged      bool  `json:"generation_changed"`
	IngressNatPacketCount  int64 `json:"ingress_nat_packet_count"`
	IngressNatByteCount    int64 `json:"ingress_nat_byte_count"`
	ReturnQueuePacketCount int64 `json:"return_queue_packet_count"`
	ReturnQueueByteCount   int64 `json:"return_queue_byte_count"`
	ReturnSendPacketCount  int64 `json:"return_send_packet_count"`
	ReturnSendByteCount    int64 `json:"return_send_byte_count"`
}

func snapshotPerfvarProviderCongestion(provider *clientconnect.RemoteUserNatProvider) perfvarProviderCongestionBoundary {
	if provider == nil {
		return perfvarProviderCongestionBoundary{}
	}
	return perfvarProviderCongestionBoundary{provider: provider, counters: provider.CongestionDropStats()}
}

func subtractPerfvarProviderCongestion(before, after perfvarProviderCongestionBoundary) perfvarProviderCongestionObservation {
	observation := perfvarProviderCongestionObservation{
		Available:         before.provider != nil && after.provider != nil,
		GenerationChanged: before.provider != after.provider,
	}
	if !observation.Available || observation.GenerationChanged {
		return observation
	}
	observation.IngressNatPacketCount = after.counters.IngressNatPacketCount - before.counters.IngressNatPacketCount
	observation.IngressNatByteCount = int64(after.counters.IngressNatByteCount - before.counters.IngressNatByteCount)
	observation.ReturnQueuePacketCount = after.counters.ReturnQueuePacketCount - before.counters.ReturnQueuePacketCount
	observation.ReturnQueueByteCount = int64(after.counters.ReturnQueueByteCount - before.counters.ReturnQueueByteCount)
	observation.ReturnSendPacketCount = after.counters.ReturnSendPacketCount - before.counters.ReturnSendPacketCount
	observation.ReturnSendByteCount = int64(after.counters.ReturnSendByteCount - before.counters.ReturnSendByteCount)
	return observation
}

func TestPerfvarAppTCPSnapshotExcludesPriorActivity(t *testing.T) {
	tun, err := clientconnect.CreateTun(t.Context(), clientconnect.DefaultTunSettings())
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()
	stats := tun.Stats().TCP
	stats.SegmentsSent.IncrementBy(100)
	stats.ValidSegmentsReceived.IncrementBy(200)
	stats.Retransmits.IncrementBy(30)
	stats.Timeouts.IncrementBy(40)
	stats.FastRetransmit.IncrementBy(50)
	stats.SlowStartRetransmits.IncrementBy(60)
	before := snapshotPerfvarAppTCP(tun)
	stats.SegmentsSent.IncrementBy(11)
	stats.ValidSegmentsReceived.IncrementBy(12)
	stats.Retransmits.IncrementBy(13)
	stats.Timeouts.IncrementBy(14)
	stats.FastRetransmit.IncrementBy(15)
	stats.SlowStartRetransmits.IncrementBy(16)
	got := subtractPerfvarAppTCP(before, snapshotPerfvarAppTCP(tun))
	want := perfvarAppTCPObservation{Available: true, perfvarAppTCPCounters: perfvarAppTCPCounters{11, 12, 13, 14, 15, 16}}
	if got != want {
		t.Fatalf("app TCP delta=%+v, want=%+v", got, want)
	}
}

func TestPerfvarDiagnosticCounterIdentity(t *testing.T) {
	tun := &clientconnect.Tun{}
	provider := &clientconnect.RemoteUserNatProvider{}
	for _, testCase := range []struct {
		name      string
		tun       *clientconnect.Tun
		provider  *clientconnect.RemoteUserNatProvider
		available bool
		changed   bool
	}{
		{"same", tun, provider, true, false},
		{"replaced", &clientconnect.Tun{}, &clientconnect.RemoteUserNatProvider{}, true, true},
		{"missing", nil, nil, false, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// An older endpoint has larger counts. Identity mismatches must
			// not manufacture unsigned wraparound or negative drop deltas.
			var endRetransmits uint64
			var endIngress int64
			if !testCase.changed {
				endRetransmits, endIngress = 7, 8
			}
			app := subtractPerfvarAppTCP(
				perfvarAppTCPBoundary{tun, perfvarAppTCPCounters{Retransmits: 7}},
				perfvarAppTCPBoundary{testCase.tun, perfvarAppTCPCounters{Retransmits: endRetransmits}},
			)
			if app.Available != testCase.available || app.GenerationChanged != testCase.changed || app.perfvarAppTCPCounters != (perfvarAppTCPCounters{}) {
				t.Fatalf("app identity=%+v", app)
			}
			drops := subtractPerfvarProviderCongestion(
				perfvarProviderCongestionBoundary{provider, clientconnect.ProviderCongestionDrops{IngressNatPacketCount: 8}},
				perfvarProviderCongestionBoundary{provider: testCase.provider, counters: clientconnect.ProviderCongestionDrops{IngressNatPacketCount: endIngress}},
			)
			want := perfvarProviderCongestionObservation{Available: testCase.available, GenerationChanged: testCase.changed}
			if drops != want {
				t.Fatalf("provider identity=%+v, want=%+v", drops, want)
			}
		})
	}
	if snapshotPerfvarAppTCP(nil) != (perfvarAppTCPBoundary{}) || snapshotPerfvarProviderCongestion(nil) != (perfvarProviderCongestionBoundary{}) {
		t.Fatal("absent endpoints reported counters")
	}
	if subtractPerfvarAppTCP(perfvarAppTCPBoundary{}, perfvarAppTCPBoundary{}) != (perfvarAppTCPObservation{}) ||
		subtractPerfvarProviderCongestion(perfvarProviderCongestionBoundary{}, perfvarProviderCongestionBoundary{}) != (perfvarProviderCongestionObservation{}) {
		t.Fatal("absent endpoints reported available deltas")
	}
	if got := snapshotPerfvarProviderCongestion(provider); got.provider != provider || got.counters != (clientconnect.ProviderCongestionDrops{}) {
		t.Fatalf("empty provider snapshot=%+v", got)
	}
}

// Exercise the normal run-record path with frozen workload boundaries. Later
// live activity and the caller's broader setup boundary cannot enter the delta.
func TestPerfvarDiagnosticCounterRecordUsesFrozenBoundaries(t *testing.T) {
	tun := &clientconnect.Tun{}
	provider := &clientconnect.RemoteUserNatProvider{}
	before := perfvarCarrierBoundary{
		capturedAt: time.Unix(10, 0),
		appTCP:     perfvarAppTCPBoundary{tun, perfvarAppTCPCounters{100, 200, 300, 400, 500, 600}},
		providerCongestionDrops: perfvarProviderCongestionBoundary{provider, clientconnect.ProviderCongestionDrops{
			IngressNatPacketCount: 10, IngressNatByteCount: 100,
			ReturnQueuePacketCount: 20, ReturnQueueByteCount: 200,
			ReturnSendPacketCount: 30, ReturnSendByteCount: 300,
		}},
	}
	after := perfvarCarrierBoundary{
		capturedAt: time.Unix(11, 0),
		appTCP:     perfvarAppTCPBoundary{tun, perfvarAppTCPCounters{101, 202, 303, 404, 505, 606}},
		providerCongestionDrops: perfvarProviderCongestionBoundary{provider, clientconnect.ProviderCongestionDrops{
			IngressNatPacketCount: 17, IngressNatByteCount: 108,
			ReturnQueuePacketCount: 29, ReturnQueueByteCount: 210,
			ReturnSendPacketCount: 41, ReturnSendByteCount: 312,
		}},
	}
	path := &fullTunPath{}
	path.setCarrierMeasurementStart(before)
	path.setCarrierMeasurementEnd(after, 0)
	record := perfvarRunRecord{
		SchemaVersion: perfvarSchemaVersion, RecordType: "run",
		Carrier: observePerfvarWorkloadCarrier(path, perfvarCarrierBoundary{}),
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		SchemaVersion int `json:"schema_version"`
		Carrier       struct {
			AppTCP                  map[string]any `json:"app_tcp"`
			ProviderCongestionDrops map[string]any `json:"provider_congestion_drops"`
		} `json:"carrier"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.SchemaVersion != 14 {
		t.Fatalf("diagnostic schema=%d, want=14", decoded.SchemaVersion)
	}
	for _, item := range []struct {
		got   map[string]any
		keys  []string
		first int
	}{
		{decoded.Carrier.AppTCP, []string{"segments_sent", "segments_received", "retransmits", "timeouts", "fast_retransmits", "slow_start_retransmits"}, 1},
		{decoded.Carrier.ProviderCongestionDrops, []string{"ingress_nat_packet_count", "ingress_nat_byte_count", "return_queue_packet_count", "return_queue_byte_count", "return_send_packet_count", "return_send_byte_count"}, 7},
	} {
		if item.got["available"] != true || item.got["generation_changed"] != false || len(item.got) != len(item.keys)+2 {
			t.Fatalf("counter serialization=%v", item.got)
		}
		for index, key := range item.keys {
			if item.got[key] != float64(item.first+index) {
				t.Fatalf("counter %s=%v, want=%d", key, item.got[key], item.first+index)
			}
		}
	}
}
