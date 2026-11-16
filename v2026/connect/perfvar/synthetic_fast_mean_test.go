// Each iteration retains the equal arithmetic mean of all five original
// whole-run rates; the native roots select the median of three such means.
// Invalid, missing, or unjoined windows never supply an iteration mean.
package perfvar

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/sdk/v2026"
)

const syntheticFastMinimumMeanMbps = 200.0
const syntheticFastNavigationLimit = 5 * time.Second

// Only this owned error denotes complete evidence below the numeric bar.
// Joined workload or cleanup errors never have this concrete type.
type syntheticFastMeanMinimumError struct {
	MeanMbps float64
}

// Preserve the five-window error text and failed numeric verdict.
func (self *syntheticFastMeanMinimumError) Error() string {
	return fmt.Sprintf("synthetic Fast five-window arithmetic mean %.6f Mbps is below %.0f Mbps", self.MeanMbps, syntheticFastMinimumMeanMbps)
}

// The real SDK separates normal locations from its all-window exit list.
// These controls preserve both populated limited classes and fail closed on
// missing normal owners, unknown peers, unhealthy clients or a hidden excess.
func TestSyntheticFastWindowInventoryIncludesLimitedOwners(t *testing.T) {
	type snapshot struct {
		quality, speed, limit int
		providers             []string
		providerKeys          [][]byte
		exits                 *sdk.ExitList
		status                *sdk.WindowStatus
		locations             *sdk.ConnectedProviderLocationList
		identities            *sdk.ProviderIdentityList
	}
	newSnapshot := func(quality, speed, limit, limited int) *snapshot {
		s := &snapshot{quality: quality, speed: speed, limit: limit,
			exits: sdk.NewExitList(), locations: sdk.NewConnectedProviderLocationList(), identities: sdk.NewProviderIdentityList(),
			status: &sdk.WindowStatus{ConnectionGeneration: 1, TargetSize: quality + speed, LimitedWindowSize: limited}}
		providerIds := make([]*sdk.Id, quality+speed)
		for index := range providerIds {
			providerIds[index] = sdk.NewId()
			s.providers = append(s.providers, providerIds[index].String())
			s.providerKeys = append(s.providerKeys, bytes.Repeat([]byte{byte(index + 1)}, 32))
		}
		for _, index := range []int{0, quality} {
			s.identities.Add(&sdk.ProviderIdentity{ClientId: providerIds[index], PublicKey: bytes.Clone(s.providerKeys[index])})
		}
		for index := 0; index < quality+speed; index++ {
			window, provider := "quality", providerIds[0]
			if index >= quality {
				window, provider = "speed", providerIds[quality]
			}
			s.exits.Add(&sdk.Exit{ClientId: sdk.NewId(), WindowType: window, CapabilityIp: "dualstack", CapabilityProtocol: "dualproto"})
			s.locations.Add(&sdk.ConnectedProviderLocation{ClientId: provider})
		}
		for index := 0; index < limited; index++ {
			ip := "ipv4"
			if index%2 != 0 {
				ip = "dualstack"
			}
			s.exits.Add(&sdk.Exit{ClientId: sdk.NewId(), WindowType: sdk.ExitWindowTypeLimited, CapabilityIp: ip, CapabilityProtocol: "tcp"})
		}
		return s
	}
	for _, row := range []struct {
		name                        string
		quality, speed, limit, size int
		accepted                    bool
		mutate                      func(*snapshot)
	}{
		{name: "SDK32 empty actual limited inventory", quality: 5, speed: 1, limit: 2, accepted: true},
		{name: "SDK32 populated limited IPv4 and dualstack TCP", quality: 5, speed: 1, limit: 2, size: 2, accepted: true},
		{name: "SDK64 empty actual limited inventory", quality: 10, speed: 2, limit: 10, accepted: true},
		{name: "SDK64 populated current limited target", quality: 10, speed: 2, limit: 10, size: 10, accepted: true},
		{name: "cold sessions not yet identity-established", quality: 5, speed: 1, limit: 2, size: 2, accepted: true, mutate: func(s *snapshot) { s.identities = sdk.NewProviderIdentityList() }},
		{name: "missing normal quality owner", quality: 5, speed: 1, limit: 2, size: 2, mutate: func(s *snapshot) { s.exits.Get(0).WindowType = "speed" }},
		{name: "unknown window", quality: 5, speed: 1, limit: 2, size: 2, mutate: func(s *snapshot) { s.exits.Get(6).WindowType = "unknown" }},
		{name: "done limited owner", quality: 5, speed: 1, limit: 2, size: 2, mutate: func(s *snapshot) { s.exits.Get(6).Done = true }},
		{name: "warned limited owner", quality: 5, speed: 1, limit: 2, size: 2, mutate: func(s *snapshot) { s.exits.Get(6).Warning = true }},
		{name: "quarantined limited owner", quality: 5, speed: 1, limit: 2, size: 2, mutate: func(s *snapshot) { s.exits.Get(6).Quarantined = true }},
		{name: "duplicate client across windows", quality: 5, speed: 1, limit: 2, size: 2, mutate: func(s *snapshot) { s.exits.Get(6).ClientId = s.exits.Get(0).ClientId }},
		{name: "P2P-only limited owner", quality: 5, speed: 1, limit: 2, size: 2, mutate: func(s *snapshot) { s.exits.Get(6).P2pOnly = true }},
		{name: "SDK32 excess limited owners", quality: 5, speed: 1, limit: 2, size: 3},
		{name: "SDK64 excess limited owners", quality: 10, speed: 2, limit: 10, size: 11},
		{name: "limited count omitted from status", quality: 5, speed: 1, limit: 2, size: 2, mutate: func(s *snapshot) { s.status.LimitedWindowSize = 0 }},
		{name: "limited count includes normal owner", quality: 5, speed: 1, limit: 2, size: 2, mutate: func(s *snapshot) { s.status.LimitedWindowSize = 1 }},
		{name: "limited owner claims dualproto", quality: 5, speed: 1, limit: 2, size: 2, mutate: func(s *snapshot) { s.exits.Get(6).CapabilityProtocol = "dualproto" }},
		{name: "limited owner claims unknown IP label", quality: 5, speed: 1, limit: 2, size: 2, mutate: func(s *snapshot) { s.exits.Get(6).CapabilityIp = "unknown" }},
		{name: "limited owner incorrectly uses overflow", quality: 5, speed: 1, limit: 2, size: 2, mutate: func(s *snapshot) { s.exits.Get(6).OverflowStep = 1 }},
		{name: "missing local ID", quality: 5, speed: 1, limit: 2, size: 2, mutate: func(s *snapshot) { s.exits.Get(6).ClientId = nil }},
		{name: "unknown physical peer", quality: 5, speed: 1, limit: 2, size: 2, mutate: func(s *snapshot) {
			s.identities.Add(&sdk.ProviderIdentity{ClientId: sdk.NewId(), PublicKey: []byte{1}})
		}},
		{name: "duplicate physical identity", quality: 5, speed: 1, limit: 2, size: 2, mutate: func(s *snapshot) { s.identities.Add(s.identities.Get(0)) }},
		{name: "unverified normal provider", quality: 5, speed: 1, limit: 2, size: 2, mutate: func(s *snapshot) { s.identities.Get(0).PublicKey = nil }},
		{name: "known provider with a foreign identity key", quality: 5, speed: 1, limit: 2, size: 2, mutate: func(s *snapshot) { s.identities.Get(0).PublicKey = bytes.Repeat([]byte{255}, 32) }},
		{name: "unowned normal location", quality: 5, speed: 1, limit: 2, size: 2, mutate: func(s *snapshot) { s.locations.Get(0).ClientId = sdk.NewId() }},
		{name: "duplicate physical pool member", quality: 5, speed: 1, limit: 2, size: 2, mutate: func(s *snapshot) { s.providers[1] = s.providers[0] }},
		{name: "missing normal location", quality: 5, speed: 1, limit: 2, size: 2, mutate: func(s *snapshot) { s.locations = sdk.NewConnectedProviderLocationList() }},
		{name: "limited location mixed into normal list", quality: 5, speed: 1, limit: 2, size: 2, mutate: func(s *snapshot) { s.locations.Add(s.locations.Get(0)) }},
	} {
		s := newSnapshot(row.quality, row.speed, row.limit, row.size)
		if row.mutate != nil {
			row.mutate(s)
		}
		accepted := syntheticFastWindowOwnershipReady(s.quality, s.speed, s.limit, s.providers, s.providerKeys, s.exits, s.status, s.locations, s.identities)
		if accepted != row.accepted {
			t.Fatalf("%s: accepted=%t want=%t", row.name, accepted, row.accepted)
		}
	}
}

// Each value includes startup and join through the workload's existing
// WholeRunMbps denominator; active-body and measurement-only rates are unused.
type syntheticFastMeanReport struct {
	WindowMbps         [5]float64 `json:"window_whole_run_mbps"`
	ArithmeticMeanMbps float64    `json:"arithmetic_mean_whole_run_mbps"`
	MinimumMeanMbps    float64    `json:"minimum_mean_mbps"`
	Valid              bool       `json:"valid"`
}

// Completed and cancellation-censored probes retain the existing strict
// five-second navigation bar independently of bulk download throughput.
func syntheticFastLoadedProbeError(report syntheticFastReport) error {
	if !report.Settings.LoadedLatency || report.Settings.Warm || report.LoadedLatencyError != "" ||
		len(report.LoadedLatencyNs) == 0 || len(report.LoadedFirstPayloadNs) != len(report.LoadedLatencyNs) ||
		len(report.LoadedProbeStartNs) != len(report.LoadedLatencyNs)+len(report.LoadedCanceledProbeNs) {
		return errors.New("synthetic Fast loaded-probe premise is incomplete")
	}
	limit := int64(syntheticFastNavigationLimit)
	for _, samples := range [][]int64{report.LoadedFirstPayloadNs, report.LoadedLatencyNs, report.LoadedCanceledProbeNs} {
		for _, elapsed := range samples {
			if elapsed < 0 || elapsed >= limit {
				return fmt.Errorf("synthetic Fast loaded probe reached navigation limit: elapsed_ns=%d limit_ns=%d", elapsed, limit)
			}
		}
	}
	return nil
}

// A cold first window has exactly the same weight as each retained successor.
// Failed attempts remain in the sequence and cannot be replaced or filtered.
func syntheticFastFiveWindowMean(sequence syntheticFastSequenceReport) (syntheticFastMeanReport, error) {
	report := syntheticFastMeanReport{MinimumMeanMbps: syntheticFastMinimumMeanMbps}
	if sequence.ExpectedRuns != 5 || len(sequence.Runs) != 5 || !sequence.Valid || sequence.Stop != "completed" {
		return report, errors.New("synthetic Fast requires exactly five valid retained windows")
	}
	for index, run := range sequence.Runs {
		workload := run.Workload
		if run.Index != index+1 || run.State != "valid" || run.Error != "" || !run.RequestsQuiescent ||
			!workload.Valid || !workload.Joined || workload.WorkerJoinState != "WORKERS_JOINED" ||
			workload.Stop != "measurement_window" || workload.OverallElapsedNs <= 0 || workload.ValidatedBytes <= 0 ||
			workload.MeasurementNs <= 0 || workload.MeasurementBytes <= 0 {
			return report, fmt.Errorf("synthetic Fast window %d is invalid or not joined", index+1)
		}
		if err := syntheticFastLoadedProbeError(workload); err != nil {
			return report, fmt.Errorf("synthetic Fast window %d: %w", index+1, err)
		}
		rate := workload.WholeRunMbps
		if math.IsNaN(rate) || math.IsInf(rate, 0) || rate <= 0 {
			return report, fmt.Errorf("synthetic Fast window %d has no finite positive whole-run rate", index+1)
		}
		report.WindowMbps[index] = rate
		report.ArithmeticMeanMbps += rate / 5
	}
	if math.IsNaN(report.ArithmeticMeanMbps) || math.IsInf(report.ArithmeticMeanMbps, 0) {
		return report, errors.New("synthetic Fast five-window arithmetic mean is not finite")
	}
	if report.ArithmeticMeanMbps < syntheticFastMinimumMeanMbps {
		return report, &syntheticFastMeanMinimumError{MeanMbps: report.ArithmeticMeanMbps}
	}
	report.Valid = true
	return report, nil
}

// Test reports retain the same completion and probe receipts as real windows.
func syntheticFastMeanControl(rates [5]float64) syntheticFastSequenceReport {
	settings := defaultSyntheticFastSettings()
	settings.LoadedLatency = true
	sequence := syntheticFastSequenceReport{ExpectedRuns: 5, Valid: true, Stop: "completed", Runs: make([]syntheticFastRun, 5)}
	for index, rate := range rates {
		elapsed := int64(time.Duration(index+1) * time.Second)
		sequence.Runs[index] = syntheticFastRun{Index: index + 1, State: "valid", RequestsQuiescent: true,
			Workload: syntheticFastReport{Valid: true, Joined: true, WorkerJoinState: "WORKERS_JOINED", Stop: "measurement_window",
				Settings: settings, WholeRunMbps: rate, OverallElapsedNs: elapsed, ValidatedBytes: int64(rate * float64(elapsed) / 8000),
				MeasurementNs: elapsed, MeasurementBytes: 1, LoadedProbeStartNs: []int64{1}, LoadedFirstPayloadNs: []int64{2}, LoadedLatencyNs: []int64{3}}}
	}
	return sequence
}

// Unequal run durations distinguish the required equal mean from aggregate
// bytes/time; a slow cold window must remain included rather than discarded.
func TestSyntheticFastMeanIncludesAllFiveWholeRuns(t *testing.T) {
	sequence := syntheticFastMeanControl([5]float64{100, 150, 200, 250, 300})
	report, err := syntheticFastFiveWindowMean(sequence)
	if err != nil || !report.Valid || report.ArithmeticMeanMbps != 200 || report.WindowMbps != [5]float64{100, 150, 200, 250, 300} {
		t.Fatalf("five-window equal mean changed: report=%+v err=%v", report, err)
	}
}

// The user's minimum is inclusive; no epsilon or rounded display rate grants
// acceptance to an actual mean below the threshold.
func TestSyntheticFastMeanThresholdBoundary(t *testing.T) {
	for _, row := range []struct {
		rate     float64
		accepted bool
	}{{rate: 199.999, accepted: false}, {rate: 200, accepted: true}, {rate: 200.001, accepted: true}} {
		report, err := syntheticFastFiveWindowMean(syntheticFastMeanControl([5]float64{row.rate, row.rate, row.rate, row.rate, row.rate}))
		if (err == nil) != row.accepted || report.Valid != row.accepted {
			t.Fatalf("threshold %.6f: report=%+v err=%v", row.rate, report, err)
		}
	}
}

// Invalid evidence cannot become a high-rate pass through filtering, a
// replacement sixth window, nonfinite values, or an omitted latency gate.
func TestSyntheticFastMeanRejectsInvalidWindows(t *testing.T) {
	for _, row := range []struct {
		name   string
		mutate func(*syntheticFastSequenceReport)
	}{
		{name: "missing window", mutate: func(s *syntheticFastSequenceReport) { s.Runs = s.Runs[:4] }},
		{name: "extra window", mutate: func(s *syntheticFastSequenceReport) { s.Runs = append(s.Runs, s.Runs[0]) }},
		{name: "diagnostic count", mutate: func(s *syntheticFastSequenceReport) { s.ExpectedRuns = 1 }},
		{name: "invalid sequence", mutate: func(s *syntheticFastSequenceReport) { s.Valid = false }},
		{name: "stopped sequence", mutate: func(s *syntheticFastSequenceReport) { s.Stop = "unsafe_or_unknown_ownership" }},
		{name: "not run", mutate: func(s *syntheticFastSequenceReport) { s.Runs[0].State = "NOTRUN" }},
		{name: "run error", mutate: func(s *syntheticFastSequenceReport) { s.Runs[0].Error = "retained failure" }},
		{name: "invalid workload", mutate: func(s *syntheticFastSequenceReport) { s.Runs[0].Workload.Valid = false }},
		{name: "unjoined workload", mutate: func(s *syntheticFastSequenceReport) { s.Runs[0].Workload.Joined = false }},
		{name: "active request owner", mutate: func(s *syntheticFastSequenceReport) { s.Runs[0].RequestsQuiescent = false }},
		{name: "zero elapsed", mutate: func(s *syntheticFastSequenceReport) { s.Runs[0].Workload.OverallElapsedNs = 0 }},
		{name: "zero payload", mutate: func(s *syntheticFastSequenceReport) { s.Runs[0].Workload.MeasurementBytes = 0 }},
		{name: "nan", mutate: func(s *syntheticFastSequenceReport) { s.Runs[0].Workload.WholeRunMbps = math.NaN() }},
		{name: "infinite", mutate: func(s *syntheticFastSequenceReport) { s.Runs[0].Workload.WholeRunMbps = math.Inf(1) }},
		{name: "zero rate", mutate: func(s *syntheticFastSequenceReport) { s.Runs[0].Workload.WholeRunMbps = 0 }},
		{name: "missing loaded probes", mutate: func(s *syntheticFastSequenceReport) { s.Runs[0].Workload.LoadedLatencyNs = nil }},
		{name: "disabled loaded probes", mutate: func(s *syntheticFastSequenceReport) { s.Runs[0].Workload.Settings.LoadedLatency = false }},
		{name: "warm override", mutate: func(s *syntheticFastSequenceReport) { s.Runs[0].Workload.Settings.Warm = true }},
		{name: "first byte at limit", mutate: func(s *syntheticFastSequenceReport) {
			s.Runs[0].Workload.LoadedFirstPayloadNs[0] = int64(5 * time.Second)
		}},
		{name: "completed probe at limit", mutate: func(s *syntheticFastSequenceReport) { s.Runs[0].Workload.LoadedLatencyNs[0] = int64(5 * time.Second) }},
		{name: "canceled probe at limit", mutate: func(s *syntheticFastSequenceReport) {
			s.Runs[0].Workload.LoadedProbeStartNs = append(s.Runs[0].Workload.LoadedProbeStartNs, 4)
			s.Runs[0].Workload.LoadedCanceledProbeNs = []int64{int64(5 * time.Second)}
		}},
	} {
		sequence := syntheticFastMeanControl([5]float64{300, 300, 300, 300, 300})
		row.mutate(&sequence)
		if report, err := syntheticFastFiveWindowMean(sequence); err == nil || report.Valid {
			t.Fatalf("%s supplied an accepted mean: %+v", row.name, report)
		}
	}
}

// The actual bounded origin must attribute SDK64's twelfth provider while
// refusing to invent an owner for an ordinal outside the complete population.
func TestSyntheticFastOriginAttributesCompleteProviderPopulation(t *testing.T) {
	for _, row := range []struct {
		provider int
		mask     uint32
		unknown  uint64
	}{
		{provider: 11, mask: uint32(1) << 11, unknown: 0},
		{provider: 12, mask: 0, unknown: 1},
		{provider: -1, mask: 0, unknown: 1},
	} {
		settings := defaultSyntheticFastSettings()
		settings.BytesPerStream = 32
		origin := &syntheticFastOrigin{settings: settings, ctx: context.Background(),
			providerForRequestForTest: func(*http.Request) int { return row.provider }}
		owner, err := syntheticFastCarrierBeginOriginRun(origin, context.Background(), 0, false, nil)
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		origin.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "https://origin0.test/bulk/0?run=0", nil))
		if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), syntheticFastBlock(0)[:32]) ||
			owner.providerMask.Load() != row.mask || owner.streamProviderMasks[0].Load() != row.mask || owner.providerUnknown.Load() != row.unknown ||
			origin.started.Load() != 1 || origin.finished.Load() != 1 || owner.sealAndWait(context.Background()) != nil {
			t.Fatalf("provider %d: status=%d mask=%x stream_mask=%x unknown=%d", row.provider, response.Code,
				owner.providerMask.Load(), owner.streamProviderMasks[0].Load(), owner.providerUnknown.Load())
		}
	}
}

// A fast H1 download cannot hide a known non-H1 ACK/request direction. Pending
// logical egress may be classified later; unknown ingress remains unproved.
func TestSyntheticFastH1WindowRequiresPhysicalCarrierEvidence(t *testing.T) {
	stats := func(h1Ingress, h1Egress int64, other sdk.TransportType, otherIngress, otherEgress int64) *sdk.PacketStats {
		rows := sdk.NewTransportPacketStatsList()
		for _, transport := range []sdk.TransportType{sdk.TransportTypeH1, sdk.TransportTypeH3, sdk.TransportTypeDns, sdk.TransportTypeDnsPump, sdk.TransportTypeP2p, sdk.TransportTypeUnknown} {
			var ingress, egress int64
			if transport == sdk.TransportTypeH1 {
				ingress, egress = h1Ingress, h1Egress
			} else if transport == other {
				ingress, egress = otherIngress, otherEgress
			}
			rows.Add(&sdk.TransportPacketStats{TransportType: transport, Stats: &sdk.PacketStats{RemoteIngressByteCount: ingress, RemoteEgressByteCount: egress}})
		}
		return &sdk.PacketStats{RemoteIngressByteCount: h1Ingress + otherIngress, RemoteEgressByteCount: h1Egress + otherEgress, TransportStats: rows}
	}
	baseline := stats(10, 5, sdk.TransportTypeUnknown, 0, 0)
	for _, row := range []struct {
		name          string
		before, after *sdk.PacketStats
		accepted      bool
	}{
		{name: "H1 both directions", before: baseline, after: stats(100, 50, sdk.TransportTypeUnknown, 0, 0), accepted: true},
		{name: "unchanged earlier other carrier", before: stats(10, 5, sdk.TransportTypeP2p, 7, 3), after: stats(100, 50, sdk.TransportTypeP2p, 7, 3), accepted: true},
		{name: "P2p body", before: baseline, after: stats(100, 50, sdk.TransportTypeP2p, 20, 0), accepted: false},
		{name: "P2p ACK/request egress", before: baseline, after: stats(100, 50, sdk.TransportTypeP2p, 0, 1), accepted: false},
		{name: "H3 ACK/request egress", before: baseline, after: stats(100, 50, sdk.TransportTypeH3, 0, 1), accepted: false},
		{name: "DNS ACK/request egress", before: baseline, after: stats(100, 50, sdk.TransportTypeDns, 0, 1), accepted: false},
		{name: "pending logical egress admitted", before: baseline, after: stats(100, 50, sdk.TransportTypeUnknown, 0, 1), accepted: true},
		{name: "pending logical egress resolves to H1", before: stats(10, 5, sdk.TransportTypeUnknown, 0, 9), after: stats(100, 50, sdk.TransportTypeUnknown, 0, 0), accepted: true},
		{name: "pending migration and new admission overlap", before: stats(10, 5, sdk.TransportTypeUnknown, 0, 10), after: stats(100, 50, sdk.TransportTypeUnknown, 0, 7), accepted: true},
		{name: "unknown ingress is not pending egress", before: baseline, after: stats(100, 50, sdk.TransportTypeUnknown, 1, 0), accepted: false},
		{name: "unknown ingress reset", before: stats(10, 5, sdk.TransportTypeUnknown, 3, 0), after: stats(100, 50, sdk.TransportTypeUnknown, 0, 0), accepted: false},
		{name: "aggregate egress reset hidden by pending decrease", before: stats(10, 5, sdk.TransportTypeUnknown, 0, 100), after: stats(100, 50, sdk.TransportTypeUnknown, 0, 0), accepted: false},
		{name: "H1 ingress reset", before: baseline, after: stats(1, 50, sdk.TransportTypeUnknown, 0, 0), accepted: false},
		{name: "H1 egress reset", before: baseline, after: stats(100, 1, sdk.TransportTypeUnknown, 0, 0), accepted: false},
		{name: "other ingress reset", before: stats(10, 5, sdk.TransportTypeP2p, 7, 3), after: stats(100, 50, sdk.TransportTypeP2p, 0, 3), accepted: false},
		{name: "other egress reset", before: stats(10, 5, sdk.TransportTypeP2p, 7, 3), after: stats(100, 50, sdk.TransportTypeP2p, 7, 0), accepted: false},
		{name: "compensating other-carrier changes", before: stats(10, 5, sdk.TransportTypeP2p, 10, 4), after: stats(100, 50, sdk.TransportTypeH3, 10, 4), accepted: false},
		{name: "no download", before: baseline, after: stats(10, 50, sdk.TransportTypeUnknown, 0, 0), accepted: false},
		{name: "no request/ACK traffic", before: baseline, after: stats(100, 5, sdk.TransportTypeUnknown, 0, 0), accepted: false},
		{name: "missing before", before: nil, after: stats(100, 50, sdk.TransportTypeUnknown, 0, 0), accepted: false},
		{name: "missing partition", before: baseline, after: &sdk.PacketStats{RemoteIngressByteCount: 100, RemoteEgressByteCount: 50}, accepted: false},
	} {
		if err := syntheticFastH1WindowError(row.before, row.after); (err == nil) != row.accepted {
			t.Fatalf("%s: accepted=%t err=%v", row.name, row.accepted, err)
		}
	}
	for _, direction := range []string{"ingress", "egress"} {
		partial := stats(100, 50, sdk.TransportTypeUnknown, 0, 0)
		if direction == "ingress" {
			partial.RemoteIngressByteCount++
		} else {
			partial.RemoteEgressByteCount++
		}
		if syntheticFastH1WindowError(baseline, partial) == nil {
			t.Fatalf("incomplete %s carrier partition qualified", direction)
		}
	}
	duplicate := stats(100, 50, sdk.TransportTypeUnknown, 0, 0)
	duplicate.TransportStats.Add(&sdk.TransportPacketStats{TransportType: sdk.TransportTypeH1, Stats: &sdk.PacketStats{}})
	if syntheticFastH1WindowError(baseline, duplicate) == nil {
		t.Fatal("duplicate carrier partition qualified")
	}
}

// Owned identity alone cannot prove a one-hop relay path: intermediary hops
// from the authenticated API remain authoritative and must not be stripped.
func TestSyntheticFastNativeDiscoveryRequiresOneHopPublicResult(t *testing.T) {
	owned, foreign, intermediary := clientconnect.Id{1}, clientconnect.Id{2}, clientconnect.Id{3}
	fixture := &syntheticFastNativeFixture{quality: 1, providers: []syntheticFastNativeProvider{{id: owned}}}
	for _, row := range []struct {
		name     string
		provider *clientconnect.FindProvidersProvider
		accepted bool
	}{
		{name: "owned one-hop", provider: &clientconnect.FindProvidersProvider{ClientId: owned}, accepted: true},
		{name: "owned multihop", provider: &clientconnect.FindProvidersProvider{ClientId: owned, IntermediaryIds: []clientconnect.Id{intermediary}}, accepted: false},
		{name: "foreign one-hop", provider: &clientconnect.FindProvidersProvider{ClientId: foreign}, accepted: false},
	} {
		called := false
		reply := &clientconnect.FindProviders2Result{Providers: []*clientconnect.FindProvidersProvider{row.provider}}
		discovery := &syntheticFastNativeDiscovery{fixture: fixture, base: sdkBrowsingSetupDiscoveryFunc(func(_ context.Context, token string, query *clientconnect.FindProviders2Args) (*clientconnect.FindProviders2Result, error) {
			called = true
			if token != "synthetic-test-token" || query.Count != 1 || query.IpFamily != clientconnect.IpFamilyFilterV4Capable ||
				len(query.Specs) != 1 || query.Specs[0].ClientId == nil || *query.Specs[0].ClientId != owned ||
				len(query.ExcludeClientIds) != 1 || query.ExcludeClientIds[0] != foreign ||
				len(query.ExcludeDestinations) != 1 || len(query.ExcludeDestinations[0]) != 1 || query.ExcludeDestinations[0][0] != intermediary {
				t.Fatal("one-hop scope replaced the actual caller authority or exclusions")
			}
			return reply, nil
		})}
		args := &clientconnect.FindProviders2Args{Count: 1, RankMode: "quality", IpFamily: clientconnect.IpFamilyFilterV4Capable,
			Specs: []*clientconnect.ProviderSpec{{BestAvailable: true}}, ExcludeClientIds: []clientconnect.Id{foreign}, ExcludeDestinations: [][]clientconnect.Id{{intermediary}}}
		result, err := discovery.FindProviders2(context.Background(), "synthetic-test-token", args)
		if !called || (err == nil) != row.accepted || row.accepted && result != reply {
			t.Fatalf("%s: called=%t result=%+v err=%v", row.name, called, result, err)
		}
		if len(row.provider.IntermediaryIds) != 0 && row.provider.IntermediaryIds[0] != intermediary {
			t.Fatal("one-hop refusal rewrote the API's intermediary authority")
		}
	}
}
