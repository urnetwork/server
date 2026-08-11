// This file resolves PERFVAR scenarios, validates filters, identifies the
// build and host, and emits stable machine-readable run and aggregate records.
package perfvar

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"maps"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect"
)

const (
	perfvarSchemaVersion   = 3
	perfvarTraceVersion    = 1
	perfvarScheduleVersion = 1
	// The measured clean queue is 32 MiB. Keeping the accepted payload at or
	// below it keeps the long-transfer default explicit and also leaves ample
	// room beside the largest route-local BDP in the 256 MiB test contract.
	perfvarMaximumPayloadByteCount = 32 * 1024 * 1024
	// The test contract contains the largest supported 9-hop, one-second-RTT
	// BDP warmup plus the maximum measured payload without contract rollover.
	perfvarPerformanceContractByteCount = 256 * 1024 * 1024
)

// Workload names are stable filter and result values.
type perfvarWorkload string

const (
	perfvarWorkloadTCP              perfvarWorkload = "tcp"
	perfvarWorkloadTCPWarmed        perfvarWorkload = "tcp-warmed"
	perfvarWorkloadTCPParallel      perfvarWorkload = "tcp-parallel"
	perfvarWorkloadQUIC             perfvarWorkload = "quic"
	perfvarWorkloadUDP              perfvarWorkload = "udp"
	perfvarWorkloadLatencyUnderLoad perfvarWorkload = "latency-under-load"
	perfvarWorkloadWeb              perfvarWorkload = "web"
)

// Direction is named from the application user's perspective.
type perfvarDirection string

const (
	perfvarDirectionUpload   perfvarDirection = "upload"
	perfvarDirectionDownload perfvarDirection = "download"
)

// Resource profiles are explicit surrogates and never physical-device claims.
type perfvarResource string

const (
	perfvarResourceDefault perfvarResource = "default"
	perfvarResourceMobile  perfvarResource = "mobile-surrogate"
)

// Topology names are stable command-line and machine-record identities.
const (
	perfvarTopologyOneHop        = "one-hop"
	perfvarTopologyThreeHop      = "three-hop"
	perfvarTopologyFiveHop       = "five-hop"
	perfvarTopologyNineHop       = "nine-hop"
	perfvarTopologySplitExchange = "split-exchange"
)

// One resolved scenario contains every choice that can affect a comparison.
type perfvarScenario struct {
	Route                   fullTunRoute     `json:"route"`
	Profile                 networkProfile   `json:"application_access_and_p2p_profile"`
	ProviderAccessProfile   networkProfile   `json:"provider_access_profile"`
	InternalExchangeProfile *networkProfile  `json:"internal_exchange_profile,omitempty"`
	Workload                perfvarWorkload  `json:"workload"`
	Direction               perfvarDirection `json:"direction"`
	Topology                string           `json:"topology"`
	ExtenderCount           int              `json:"extender_count_per_user_path"`
	Resource                perfvarResource  `json:"resource"`
	Seed                    int64            `json:"seed"`
	RunCount                int              `json:"run_count"`
	PayloadByteCount        int64            `json:"payload_byte_count"`
	WarmupByteCount         int64            `json:"warmup_byte_count,omitempty"`
	FlowCount               int              `json:"flow_count"`
	UdpDuration             time.Duration    `json:"udp_duration_nanoseconds"`
	UdpOfferedBitRate       int64            `json:"udp_offered_bits_per_second"`
	UdpPayloadBytes         int              `json:"udp_payload_bytes"`
}

// Parsed selection values are kept separate from scenario defaults.
type perfvarConfig struct {
	Enabled          bool
	Routes           map[string]bool
	Profiles         map[string]bool
	Workloads        map[string]bool
	Directions       map[string]bool
	Topologies       map[string]bool
	InternalProfiles map[string]bool
	ExtenderCount    int
	Resources        map[string]bool
	Seed             int64
	RunCount         int
	PayloadBytes     int64
	PayloadSet       bool
}

// P2P topology names resolve to physical adjacent stream carriers. Split
// exchange is intentionally not a P2P hop count.
func perfvarTopologyP2pHopCount(topology string) (int, bool) {
	switch topology {
	case perfvarTopologyOneHop:
		return 1, true
	case perfvarTopologyThreeHop:
		return 3, true
	case perfvarTopologyFiveHop:
		return 5, true
	case perfvarTopologyNineHop:
		return 9, true
	default:
		return 0, false
	}
}

// Host metadata makes same-host comparisons attributable and rejects physical
// device interpretations of these userspace results.
type perfvarHostMetadata struct {
	GoVersion        string `json:"go_version"`
	OperatingSystem  string `json:"operating_system"`
	Architecture     string `json:"architecture"`
	CpuCount         int    `json:"cpu_count"`
	CpuDescription   string `json:"cpu_description,omitempty"`
	GoMaxProcs       int    `json:"go_max_procs"`
	RaceEnabled      bool   `json:"race_enabled"`
	GitRevision      string `json:"git_revision,omitempty"`
	GitModified      bool   `json:"git_modified"`
	GitStateHash     string `json:"git_state_hash,omitempty"`
	ConnectRevision  string `json:"connect_git_revision,omitempty"`
	ConnectModified  bool   `json:"connect_git_modified"`
	ConnectStateHash string `json:"connect_git_state_hash,omitempty"`
	MeasurementKind  string `json:"measurement_kind"`
}

// One deterministic trace identity gives comparable routes the same initial
// seed family while giving each repetition an independent, reproducible trace.
// Route-specific setup advances different prefixes before measurement, so the
// resulting workload decisions are reproducible but not packet-for-packet
// common-random-number pairs.
type perfvarTrace struct {
	Version                 int    `json:"version"`
	RunIndex                int    `json:"run_index"`
	IdentityHash            string `json:"identity_hash"`
	ApplicationOrDirectSeed int64  `json:"application_or_direct_seed"`
	ProviderSeed            int64  `json:"provider_seed"`
	InternalSeed            int64  `json:"internal_seed"`
}

// Carrier observations prove route selection and expose simulated path cost.
type perfvarCarrierObservation struct {
	Links                          map[string]directionalLinkSnapshot        `json:"links"`
	P2PNetwork                     p2pNetworkSnapshot                        `json:"p2p_network"`
	DeviceP2P                      clientconnect.P2pDataPlaneStatsSnapshot   `json:"device_p2p"`
	ProviderP2P                    clientconnect.P2pDataPlaneStatsSnapshot   `json:"provider_p2p"`
	StreamP2PHops                  []streamP2pHopSnapshot                    `json:"stream_p2p_hops,omitempty"`
	StreamP2PClientStats           []clientconnect.P2pDataPlaneStatsSnapshot `json:"stream_p2p_client_stats,omitempty"`
	StreamNonAdjacentDialCount     uint64                                    `json:"stream_non_adjacent_dial_count,omitempty"`
	StreamNonAdjacentStunDropCount uint64                                    `json:"stream_non_adjacent_stun_drop_count,omitempty"`
	StreamNonAdjacentDataDropCount uint64                                    `json:"stream_non_adjacent_data_drop_count,omitempty"`
	FenceInclusive                 bool                                      `json:"fence_inclusive,omitempty"`
	FenceApplicationPacketCount    int                                       `json:"fence_application_packet_count,omitempty"`
	Duration                       time.Duration                             `json:"duration_nanoseconds"`
	WireByteCount                  uint64                                    `json:"wire_byte_count"`
}

// Every run contains the calibration, tunneled result, and exact identities.
type perfvarRunRecord struct {
	SchemaVersion      int                       `json:"schema_version"`
	ScheduleVersion    int                       `json:"schedule_version"`
	RecordType         string                    `json:"record_type"`
	ScenarioHash       string                    `json:"scenario_hash"`
	ProfileHash        string                    `json:"profile_hash"`
	RunIndex           int                       `json:"run_index"`
	Trace              perfvarTrace              `json:"trace"`
	Scenario           perfvarScenario           `json:"scenario"`
	Host               perfvarHostMetadata       `json:"host"`
	Underlay           workloadResult            `json:"underlay"`
	Tunneled           workloadResult            `json:"tunneled"`
	Carrier            perfvarCarrierObservation `json:"carrier"`
	RouteSetupDuration time.Duration             `json:"route_setup_duration_nanoseconds"`
	Efficiency         float64                   `json:"tunneled_underlay_efficiency"`
	WireEfficiency     float64                   `json:"useful_wire_efficiency"`
	Correct            bool                      `json:"correct"`
	FailureStage       string                    `json:"failure_stage,omitempty"`
	FailureReason      string                    `json:"failure_reason,omitempty"`
	InvalidReason      string                    `json:"invalid_reason,omitempty"`
	// These process-wide point samples aid diagnosis but do not prove route
	// lifecycle reconciliation; deterministic resource tests own that claim.
	GoroutinesBefore int `json:"goroutines_before"`
	GoroutinesAfter  int `json:"goroutines_after"`
}

// Aggregate values retain all comparison statistics requested by the plan.
type perfvarAggregateRecord struct {
	SchemaVersion      int             `json:"schema_version"`
	ScheduleVersion    int             `json:"schedule_version"`
	RecordType         string          `json:"record_type"`
	ScenarioHash       string          `json:"scenario_hash"`
	ProfileHash        string          `json:"profile_hash"`
	Scenario           perfvarScenario `json:"scenario"`
	RunCount           int             `json:"run_count"`
	GoodputMedianGbps  float64         `json:"goodput_median_gigabits_per_second"`
	GoodputP95Gbps     float64         `json:"goodput_p95_gigabits_per_second"`
	GoodputWorstGbps   float64         `json:"goodput_worst_gigabits_per_second"`
	DurationMedian     time.Duration   `json:"duration_median_nanoseconds"`
	DurationP95        time.Duration   `json:"duration_p95_nanoseconds"`
	DurationWorst      time.Duration   `json:"duration_worst_nanoseconds"`
	SetupMedian        time.Duration   `json:"setup_median_nanoseconds"`
	LatencyP95Median   time.Duration   `json:"latency_p95_median_nanoseconds"`
	LoadedP95Median    time.Duration   `json:"loaded_latency_p95_median_nanoseconds"`
	EfficiencyMedian   float64         `json:"efficiency_median"`
	WireEfficiency     float64         `json:"wire_efficiency_median"`
	CorrectRunCount    int             `json:"correct_run_count"`
	FailureRunCount    int             `json:"failure_run_count"`
	InvalidRunCount    int             `json:"invalid_run_count"`
	ValidRunCount      int             `json:"valid_run_count"`
	IndividualCorrect  []bool          `json:"individual_run_correct"`
	IndividualRunValid []bool          `json:"individual_run_valid"`
}

// A lookup function makes filter validation deterministic without mutating the
// process environment in unit tests.
func loadPerfvarConfig(getenv func(string) string) (perfvarConfig, error) {
	parsePositiveInt := func(name string, defaultValue int) (int, error) {
		value := strings.TrimSpace(getenv(name))
		if value == "" {
			return defaultValue, nil
		}
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed <= 0 {
			return 0, fmt.Errorf("%s must be a positive decimal integer", name)
		}
		return parsed, nil
	}
	parseInt64 := func(name string, defaultValue int64, positive bool) (int64, error) {
		value := strings.TrimSpace(getenv(name))
		if value == "" {
			return defaultValue, nil
		}
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || positive && parsed <= 0 {
			return 0, fmt.Errorf("%s must be a valid decimal integer", name)
		}
		return parsed, nil
	}
	parseSet := func(name string, allowed []string, defaults []string) (map[string]bool, error) {
		value := strings.TrimSpace(getenv(name))
		selected := defaults
		if value != "" {
			selected = strings.Split(value, ",")
		}
		allowedSet := map[string]bool{}
		for _, item := range allowed {
			allowedSet[item] = true
		}
		result := map[string]bool{}
		for _, item := range selected {
			item = strings.TrimSpace(item)
			if !allowedSet[item] {
				return nil, fmt.Errorf("%s has unknown value %q; allowed values are %s", name, item, strings.Join(allowed, ","))
			}
			result[item] = true
		}
		return result, nil
	}

	routes, err := parseSet(
		"CONNECT_PERFVAR_ROUTE",
		[]string{string(fullTunRouteP2pFast), string(fullTunRouteP2pLegacy), string(fullTunRouteExchangeH1), string(fullTunRouteExchangeH3)},
		[]string{string(fullTunRouteP2pFast), string(fullTunRouteP2pLegacy), string(fullTunRouteExchangeH1), string(fullTunRouteExchangeH3)},
	)
	if err != nil {
		return perfvarConfig{}, err
	}
	profileNames := make([]string, 0, len(allNetworkProfiles(1)))
	for name := range allNetworkProfiles(1) {
		profileNames = append(profileNames, name)
	}
	slices.Sort(profileNames)
	profiles, err := parseSet("CONNECT_PERFVAR_PROFILE", profileNames, []string{"clean-lan"})
	if err != nil {
		return perfvarConfig{}, err
	}
	workloads, err := parseSet(
		"CONNECT_PERFVAR_WORKLOAD",
		[]string{
			string(perfvarWorkloadTCP),
			string(perfvarWorkloadTCPWarmed),
			string(perfvarWorkloadTCPParallel),
			string(perfvarWorkloadQUIC),
			string(perfvarWorkloadUDP),
			string(perfvarWorkloadLatencyUnderLoad),
			string(perfvarWorkloadWeb),
		},
		[]string{string(perfvarWorkloadTCP)},
	)
	if err != nil {
		return perfvarConfig{}, err
	}
	directions, err := parseSet(
		"CONNECT_PERFVAR_DIRECTION",
		[]string{string(perfvarDirectionUpload), string(perfvarDirectionDownload)},
		[]string{string(perfvarDirectionUpload), string(perfvarDirectionDownload)},
	)
	if err != nil {
		return perfvarConfig{}, err
	}
	topologies, err := parseSet(
		"CONNECT_PERFVAR_TOPOLOGY",
		[]string{
			perfvarTopologyOneHop,
			perfvarTopologyThreeHop,
			perfvarTopologyFiveHop,
			perfvarTopologyNineHop,
			perfvarTopologySplitExchange,
		},
		[]string{perfvarTopologyOneHop},
	)
	if err != nil {
		return perfvarConfig{}, err
	}
	internalProfiles, err := parseSet(
		"CONNECT_PERFVAR_INTERNAL_PROFILE",
		profileNames,
		[]string{"clean-lan"},
	)
	if err != nil {
		return perfvarConfig{}, err
	}
	resources, err := parseSet(
		"CONNECT_PERFVAR_RESOURCE",
		[]string{string(perfvarResourceDefault), string(perfvarResourceMobile)},
		[]string{string(perfvarResourceDefault)},
	)
	if err != nil {
		return perfvarConfig{}, err
	}
	runCount, err := parsePositiveInt("CONNECT_PERFVAR_RUN_COUNT", 5)
	if err != nil {
		return perfvarConfig{}, err
	}
	seed, err := parseInt64("CONNECT_PERFVAR_SEED", 20260810, false)
	if err != nil {
		return perfvarConfig{}, err
	}
	payloadBytes, err := parseInt64("CONNECT_PERFVAR_BYTE_COUNT", 32*1024*1024, true)
	if err != nil {
		return perfvarConfig{}, err
	}
	if perfvarMaximumPayloadByteCount < payloadBytes {
		return perfvarConfig{}, fmt.Errorf(
			"CONNECT_PERFVAR_BYTE_COUNT must not exceed %d",
			perfvarMaximumPayloadByteCount,
		)
	}
	extenderCount := 0
	if value := strings.TrimSpace(getenv("CONNECT_PERFVAR_EXTENDERS")); value != "" {
		parsed, parseErr := strconv.Atoi(value)
		if parseErr != nil || parsed < 0 || 1 < parsed {
			return perfvarConfig{}, fmt.Errorf("CONNECT_PERFVAR_EXTENDERS must be 0 or 1")
		}
		extenderCount = parsed
	}
	return perfvarConfig{
		Enabled:          getenv("CONNECT_PERFVAR_MEASURE") == "1",
		Routes:           routes,
		Profiles:         profiles,
		Workloads:        workloads,
		Directions:       directions,
		Topologies:       topologies,
		InternalProfiles: internalProfiles,
		ExtenderCount:    extenderCount,
		Resources:        resources,
		Seed:             seed,
		RunCount:         runCount,
		PayloadBytes:     payloadBytes,
		PayloadSet:       strings.TrimSpace(getenv("CONNECT_PERFVAR_BYTE_COUNT")) != "",
	}, nil
}

// Scenario expansion is curated and rejects unsupported semantic combinations.
func resolvePerfvarScenarios(config perfvarConfig) ([]perfvarScenario, error) {
	profiles := allNetworkProfiles(config.Seed)
	scenarios := []perfvarScenario{}
	for routeName := range config.Routes {
		route := fullTunRoute(routeName)
		if config.ExtenderCount != 0 && route != fullTunRouteExchangeH1 {
			continue
		}
		for profileName := range config.Profiles {
			profile := profiles[profileName]
			providerAccessProfile := profile
			if strings.HasPrefix(profileName, "single-region-") {
				providerAccessProfile = profiles["clean-lan"]
				providerAccessProfile.SourceNote = "synthetic provider colocated with server/connect"
			}
			for workloadName := range config.Workloads {
				workload := perfvarWorkload(workloadName)
				for directionName := range config.Directions {
					direction := perfvarDirection(directionName)
					if direction == perfvarDirectionDownload &&
						workload != perfvarWorkloadTCP &&
						workload != perfvarWorkloadTCPWarmed &&
						workload != perfvarWorkloadTCPParallel &&
						workload != perfvarWorkloadUDP &&
						workload != perfvarWorkloadWeb {
						continue
					}
					if direction == perfvarDirectionUpload && workload == perfvarWorkloadWeb {
						continue
					}
					for topology := range config.Topologies {
						p2pHopCount, isP2pTopology := perfvarTopologyP2pHopCount(topology)
						if topology == perfvarTopologySplitExchange {
							if (route != fullTunRouteExchangeH1 && route != fullTunRouteExchangeH3) ||
								config.ExtenderCount != 0 {
								continue
							}
						} else if !isP2pTopology {
							continue
						} else if 1 < p2pHopCount && (route != fullTunRouteP2pFast || config.ExtenderCount != 0) {
							continue
						}
						internalProfileNames := []string{""}
						if topology == perfvarTopologySplitExchange {
							internalProfileNames = slices.Sorted(maps.Keys(config.InternalProfiles))
						}
						for _, internalProfileName := range internalProfileNames {
							for resourceName := range config.Resources {
								payloadByteCount := config.PayloadBytes
								if !config.PayloadSet {
									switch profileName {
									case "single-region-500ms-rtt", "single-region-1000ms-rtt":
										if workload != perfvarWorkloadTCPWarmed {
											payloadByteCount = 64 * 1024
										}
									case "lte", "mobile-poor":
										payloadByteCount = 20 * 1024 * 1024
									}
								}
								scenario := perfvarScenario{
									Route:                 route,
									Profile:               profile,
									ProviderAccessProfile: providerAccessProfile,
									Workload:              workload,
									Direction:             direction,
									Topology:              topology,
									ExtenderCount:         config.ExtenderCount,
									Resource:              perfvarResource(resourceName),
									Seed:                  config.Seed,
									RunCount:              config.RunCount,
									PayloadByteCount:      payloadByteCount,
									FlowCount:             1,
									UdpDuration:           time.Second,
									UdpOfferedBitRate:     5_000_000,
									UdpPayloadBytes:       1000,
								}
								if internalProfileName != "" {
									internalProfile := profiles[internalProfileName]
									scenario.InternalExchangeProfile = &internalProfile
								}
								if workload == perfvarWorkloadTCPParallel {
									scenario.FlowCount = 4
									scenario.PayloadByteCount = max(64*1024, payloadByteCount/4)
								}
								if workload == perfvarWorkloadTCPWarmed {
									scenario.WarmupByteCount = perfvarDirectionalBandwidthDelayByteCount(scenario)
									if scenario.WarmupByteCount <= 0 {
										return nil, fmt.Errorf(
											"PERFVAR warmed TCP resolved no positive bandwidth-delay product for %s/%s/%s",
											route,
											profileName,
											direction,
										)
									}
									if err := validatePerfvarWarmedTCPContract(scenario); err != nil {
										return nil, err
									}
								}
								scenarios = append(scenarios, scenario)
							}
						}
					}
				}
			}
		}
	}
	if len(scenarios) == 0 {
		return nil, fmt.Errorf("PERFVAR filters select no supported scenarios")
	}
	slices.SortFunc(scenarios, func(left perfvarScenario, right perfvarScenario) int {
		internalName := func(scenario perfvarScenario) string {
			if scenario.InternalExchangeProfile == nil {
				return ""
			}
			return scenario.InternalExchangeProfile.Name
		}
		leftName := fmt.Sprintf("%s/%s/%s/%s/%s/%s/%d/%s", left.Route, left.Profile.Name, left.Workload, left.Direction, left.Topology, internalName(left), left.ExtenderCount, left.Resource)
		rightName := fmt.Sprintf("%s/%s/%s/%s/%s/%s/%d/%s", right.Route, right.Profile.Name, right.Workload, right.Direction, right.Topology, internalName(right), right.ExtenderCount, right.Resource)
		return strings.Compare(leftName, rightName)
	})
	return scenarios, nil
}

// Warmup and measurement must fit one opening contract. A future profile that
// exceeds the explicit bound is rejected instead of silently truncating BDP.
func validatePerfvarWarmedTCPContract(scenario perfvarScenario) error {
	if scenario.PayloadByteCount <= 0 || scenario.WarmupByteCount <= 0 {
		return fmt.Errorf(
			"PERFVAR warmed TCP requires positive warmup and payload bytes: warmup=%d payload=%d",
			scenario.WarmupByteCount,
			scenario.PayloadByteCount,
		)
	}
	if perfvarPerformanceContractByteCount-scenario.PayloadByteCount < scenario.WarmupByteCount {
		return fmt.Errorf(
			"PERFVAR warmed TCP requires %d warmup + %d measured bytes, exceeding the %d-byte test contract",
			scenario.WarmupByteCount,
			scenario.PayloadByteCount,
			perfvarPerformanceContractByteCount,
		)
	}
	return nil
}

// Stable hashes prevent comparisons after a silent scenario change.
func (self perfvarScenario) hash() (string, error) {
	encoded, err := json.Marshal(self)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// The profile identity includes both exchange access segments. P2P uses the
// application profile for its direct link, but retaining the provider control
// profile still makes setup comparisons reproducible.
func (self perfvarScenario) profilesHash() (string, error) {
	profiles := []networkProfile{self.Profile, self.ProviderAccessProfile}
	if self.InternalExchangeProfile != nil {
		profiles = append(profiles, *self.InternalExchangeProfile)
	}
	encoded, err := json.Marshal(profiles)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// Route-independent derivation pairs H1/H3 and fast/legacy on the same trace.
// Run count is excluded so extending a campaign preserves its earlier traces.
func perfvarTraceForRun(scenario perfvarScenario, runIndex int) (perfvarTrace, error) {
	identityScenario := scenario
	identityScenario.Route = ""
	identityScenario.RunCount = 0
	// Warmup is one route-local BDP. Excluding its derived byte count keeps
	// route comparisons on the same impairment trace even when their physical
	// segment composition gives them slightly different BDPs.
	identityScenario.WarmupByteCount = 0
	identityScenario.Profile.Seed = 0
	identityScenario.ProviderAccessProfile.Seed = 0
	if identityScenario.InternalExchangeProfile != nil {
		internalProfile := *identityScenario.InternalExchangeProfile
		internalProfile.Seed = 0
		identityScenario.InternalExchangeProfile = &internalProfile
	}
	identity := struct {
		Version  int             `json:"version"`
		RunIndex int             `json:"run_index"`
		Scenario perfvarScenario `json:"scenario"`
	}{
		Version:  perfvarTraceVersion,
		RunIndex: runIndex,
		Scenario: identityScenario,
	}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return perfvarTrace{}, err
	}
	root := sha256.Sum256(encoded)
	seed := func(label string) int64 {
		digest := sha256.New()
		_, _ = digest.Write(root[:])
		_, _ = digest.Write([]byte(label))
		seedBytes := digest.Sum(nil)
		value := int64(binary.BigEndian.Uint64(seedBytes[:8]) & uint64(math.MaxInt64))
		if value == 0 {
			return 1
		}
		return value
	}
	return perfvarTrace{
		Version:                 perfvarTraceVersion,
		RunIndex:                runIndex,
		IdentityHash:            hex.EncodeToString(root[:]),
		ApplicationOrDirectSeed: seed("application-or-direct"),
		ProviderSeed:            seed("provider"),
		InternalSeed:            seed("internal"),
	}, nil
}

// A cloned execution scenario carries trace-specific segment seeds without
// changing the aggregate scenario identity or its comparison hash.
func perfvarScenarioForTrace(scenario perfvarScenario, trace perfvarTrace) perfvarScenario {
	executionScenario := scenario
	executionScenario.Profile.Seed = trace.ApplicationOrDirectSeed
	executionScenario.ProviderAccessProfile.Seed = trace.ProviderSeed
	if executionScenario.InternalExchangeProfile != nil {
		internalProfile := *executionScenario.InternalExchangeProfile
		internalProfile.Seed = trace.InternalSeed
		executionScenario.InternalExchangeProfile = &internalProfile
	}
	return executionScenario
}

// Repetitions use distinct deterministic traces, while routes being compared
// receive identical per-run seeds and campaign length does not perturb them.
func TestPerfvarRunTracePairingAndIdentity(t *testing.T) {
	profiles := allNetworkProfiles(20260810)
	internalProfile := profiles["wan"]
	scenario := perfvarScenario{
		Route:                   fullTunRouteExchangeH1,
		Profile:                 profiles["clean-lan"],
		ProviderAccessProfile:   profiles["wifi-good"],
		InternalExchangeProfile: &internalProfile,
		Workload:                perfvarWorkloadTCP,
		Direction:               perfvarDirectionUpload,
		Topology:                perfvarTopologySplitExchange,
		ExtenderCount:           0,
		Resource:                perfvarResourceDefault,
		Seed:                    20260810,
		RunCount:                5,
		PayloadByteCount:        32 * 1024 * 1024,
		FlowCount:               1,
		UdpDuration:             time.Second,
		UdpOfferedBitRate:       5_000_000,
		UdpPayloadBytes:         1000,
	}
	first, err := perfvarTraceForRun(scenario, 1)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := perfvarTraceForRun(scenario, 1)
	if err != nil {
		t.Fatal(err)
	}
	if first != repeated {
		t.Fatalf("trace derivation changed across identical calls: first=%+v repeated=%+v", first, repeated)
	}
	second, err := perfvarTraceForRun(scenario, 2)
	if err != nil {
		t.Fatal(err)
	}
	if first.IdentityHash == second.IdentityHash {
		t.Fatal("two run indexes reused one impairment trace")
	}
	if first.ApplicationOrDirectSeed == first.ProviderSeed ||
		first.ApplicationOrDirectSeed == first.InternalSeed ||
		first.ProviderSeed == first.InternalSeed {
		t.Fatalf("trace segments reused a seed: %+v", first)
	}

	pairedRoute := scenario
	pairedRoute.Route = fullTunRouteExchangeH3
	paired, err := perfvarTraceForRun(pairedRoute, 1)
	if err != nil {
		t.Fatal(err)
	}
	if first != paired {
		t.Fatalf("paired routes received different traces: H1=%+v H3=%+v", first, paired)
	}
	extendedCampaign := scenario
	extendedCampaign.RunCount = 9
	extended, err := perfvarTraceForRun(extendedCampaign, 1)
	if err != nil {
		t.Fatal(err)
	}
	if first != extended {
		t.Fatalf("extending run count changed an earlier trace: first=%+v extended=%+v", first, extended)
	}
	warmupChanged := scenario
	warmupChanged.WarmupByteCount += 1
	warmupTrace, err := perfvarTraceForRun(warmupChanged, 1)
	if err != nil {
		t.Fatal(err)
	}
	if warmupTrace != first {
		t.Fatalf(
			"route-local warmup bytes changed the paired impairment trace: first=%+v changed=%+v",
			first,
			warmupTrace,
		)
	}
	originalHash, err := scenario.hash()
	if err != nil {
		t.Fatal(err)
	}
	changedHash, err := warmupChanged.hash()
	if err != nil {
		t.Fatal(err)
	}
	if originalHash == changedHash {
		t.Fatal("warmup byte count did not change the scenario record identity")
	}

	mutations := []struct {
		name   string
		mutate func(*perfvarScenario)
	}{
		{name: "profile", mutate: func(value *perfvarScenario) { value.Profile.Name = "other-profile" }},
		{name: "provider", mutate: func(value *perfvarScenario) { value.ProviderAccessProfile.Name = "other-provider" }},
		{name: "internal", mutate: func(value *perfvarScenario) { value.InternalExchangeProfile.Name = "other-internal" }},
		{name: "workload", mutate: func(value *perfvarScenario) { value.Workload = perfvarWorkloadUDP }},
		{name: "direction", mutate: func(value *perfvarScenario) { value.Direction = perfvarDirectionDownload }},
		{name: "topology", mutate: func(value *perfvarScenario) { value.Topology = perfvarTopologyOneHop }},
		{name: "extender", mutate: func(value *perfvarScenario) { value.ExtenderCount = 1 }},
		{name: "resource", mutate: func(value *perfvarScenario) { value.Resource = perfvarResourceMobile }},
		{name: "seed", mutate: func(value *perfvarScenario) { value.Seed += 1 }},
		{name: "payload", mutate: func(value *perfvarScenario) { value.PayloadByteCount += 1 }},
		{name: "flow count", mutate: func(value *perfvarScenario) { value.FlowCount += 1 }},
		{name: "UDP duration", mutate: func(value *perfvarScenario) { value.UdpDuration += time.Millisecond }},
		{name: "UDP rate", mutate: func(value *perfvarScenario) { value.UdpOfferedBitRate += 1 }},
		{name: "UDP payload", mutate: func(value *perfvarScenario) { value.UdpPayloadBytes += 1 }},
	}
	for _, mutation := range mutations {
		changed := scenario
		changedInternalProfile := *scenario.InternalExchangeProfile
		changed.InternalExchangeProfile = &changedInternalProfile
		mutation.mutate(&changed)
		trace, traceErr := perfvarTraceForRun(changed, 1)
		if traceErr != nil {
			t.Errorf("%s trace: %v", mutation.name, traceErr)
			continue
		}
		if trace.IdentityHash == first.IdentityHash {
			t.Errorf("changing %s reused trace identity %s", mutation.name, trace.IdentityHash)
		}
	}

	executionScenario := perfvarScenarioForTrace(scenario, first)
	if executionScenario.Profile.Seed != first.ApplicationOrDirectSeed ||
		executionScenario.ProviderAccessProfile.Seed != first.ProviderSeed ||
		executionScenario.InternalExchangeProfile.Seed != first.InternalSeed {
		t.Fatalf("execution scenario omitted trace seeds: scenario=%+v trace=%+v", executionScenario, first)
	}
	if scenario.InternalExchangeProfile.Seed == first.InternalSeed {
		t.Fatal("trace application mutated the aggregate scenario")
	}
}

// Length-prefixing prevents path and content boundaries from aliasing one state.
func writePerfvarHashField(hashWriter hash.Hash, value []byte) {
	lengthBytes := [8]byte{}
	binary.BigEndian.PutUint64(lengthBytes[:], uint64(len(value)))
	_, _ = hashWriter.Write(lengthBytes[:])
	_, _ = hashWriter.Write(value)
}

// A Git state hash covers the revision, tracked diff, status, untracked paths,
// file modes, and untracked contents used by a dirty measurement build.
func perfvarGitState(directory string) (string, bool, string) {
	revisionBytes, revisionErr := exec.Command(
		"git", "-C", directory, "rev-parse", "HEAD",
	).Output()
	if revisionErr != nil {
		return "", false, ""
	}
	statusBytes, statusErr := exec.Command(
		"git", "-C", directory, "status", "--porcelain=v1", "-z", "--untracked-files=all",
	).Output()
	if statusErr != nil {
		return strings.TrimSpace(string(revisionBytes)), false, ""
	}
	diffBytes, diffErr := exec.Command(
		"git", "-C", directory, "diff", "--binary", "--no-ext-diff", "HEAD", "--", ".",
	).Output()
	if diffErr != nil {
		return strings.TrimSpace(string(revisionBytes)), 0 < len(statusBytes), ""
	}
	untrackedBytes, untrackedErr := exec.Command(
		"git", "-C", directory, "ls-files", "--others", "--exclude-standard", "-z",
	).Output()
	if untrackedErr != nil {
		return strings.TrimSpace(string(revisionBytes)), 0 < len(statusBytes), ""
	}
	untrackedPaths := []string{}
	for _, pathBytes := range bytes.Split(untrackedBytes, []byte{0}) {
		if 0 < len(pathBytes) {
			untrackedPaths = append(untrackedPaths, string(pathBytes))
		}
	}
	slices.Sort(untrackedPaths)
	hashWriter := sha256.New()
	writePerfvarHashField(hashWriter, []byte("PERFVAR Git state v1"))
	writePerfvarHashField(hashWriter, bytes.TrimSpace(revisionBytes))
	writePerfvarHashField(hashWriter, statusBytes)
	writePerfvarHashField(hashWriter, diffBytes)
	for _, path := range untrackedPaths {
		info, err := os.Lstat(filepath.Join(directory, filepath.FromSlash(path)))
		if err != nil {
			return strings.TrimSpace(string(revisionBytes)), true, ""
		}
		var contents []byte
		if info.Mode()&os.ModeSymlink != 0 {
			target, readErr := os.Readlink(filepath.Join(directory, filepath.FromSlash(path)))
			if readErr != nil {
				return strings.TrimSpace(string(revisionBytes)), true, ""
			}
			contents = []byte(target)
		} else if info.Mode().IsRegular() {
			contents, err = os.ReadFile(filepath.Join(directory, filepath.FromSlash(path)))
			if err != nil {
				return strings.TrimSpace(string(revisionBytes)), true, ""
			}
		} else {
			return strings.TrimSpace(string(revisionBytes)), true, ""
		}
		writePerfvarHashField(hashWriter, []byte(path))
		writePerfvarHashField(hashWriter, []byte(info.Mode().String()))
		writePerfvarHashField(hashWriter, contents)
	}
	return strings.TrimSpace(string(revisionBytes)), 0 < len(statusBytes), hex.EncodeToString(hashWriter.Sum(nil))
}

// One test binary has one immutable source and host identity.
var cachedPerfvarHostMetadata = sync.OnceValue(loadPerfvarHostMetadata)

// Build information is available in go test binaries without invoking git.
func loadPerfvarHostMetadata() perfvarHostMetadata {
	metadata := perfvarHostMetadata{
		GoVersion:       runtime.Version(),
		OperatingSystem: runtime.GOOS,
		Architecture:    runtime.GOARCH,
		CpuCount:        runtime.NumCPU(),
		GoMaxProcs:      runtime.GOMAXPROCS(0),
		RaceEnabled:     perfvarRaceEnabled,
		MeasurementKind: "userspace-same-host",
	}
	if buildInfo, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range buildInfo.Settings {
			switch setting.Key {
			case "vcs.revision":
				metadata.GitRevision = setting.Value
			case "vcs.modified":
				metadata.GitModified = setting.Value == "true"
			}
		}
	}
	if _, filename, _, ok := runtime.Caller(0); ok {
		serverRoot := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
		metadata.GitRevision, metadata.GitModified, metadata.GitStateHash = perfvarGitState(serverRoot)
		metadata.ConnectRevision, metadata.ConnectModified, metadata.ConnectStateHash = perfvarGitState(
			filepath.Join(serverRoot, "..", "connect"),
		)
	}
	if runtime.GOOS == "darwin" {
		if cpuBytes, err := exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output(); err == nil {
			metadata.CpuDescription = strings.TrimSpace(string(cpuBytes))
		}
	}
	return metadata
}

// Every record receives a value copy of the process-wide source identity.
func currentPerfvarHostMetadata() perfvarHostMetadata {
	return cachedPerfvarHostMetadata()
}

// A measurement source must be identifiable even when either worktree is
// dirty. Returning an error prevents an exact-tree campaign from silently
// emitting a revision without the content fingerprint needed to compare it.
func validatePerfvarHostMetadata(metadata perfvarHostMetadata) error {
	if metadata.GitRevision == "" {
		return errors.New("PERFVAR server Git revision is unavailable")
	}
	if metadata.GitStateHash == "" {
		return errors.New("PERFVAR server Git state hash is unavailable")
	}
	if metadata.ConnectRevision == "" {
		return errors.New("PERFVAR Connect Git revision is unavailable")
	}
	if metadata.ConnectStateHash == "" {
		return errors.New("PERFVAR Connect Git state hash is unavailable")
	}
	return nil
}

// Every missing identity component fails independently, while complete clean
// and dirty metadata share the same acceptance rule.
func TestPerfvarHostMetadataRequiresCompleteSourceIdentity(t *testing.T) {
	complete := perfvarHostMetadata{
		GitRevision:      "server-revision",
		GitStateHash:     "server-state",
		ConnectRevision:  "connect-revision",
		ConnectStateHash: "connect-state",
	}
	if err := validatePerfvarHostMetadata(complete); err != nil {
		t.Fatalf("complete source identity: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*perfvarHostMetadata)
	}{
		{name: "server revision", mutate: func(value *perfvarHostMetadata) { value.GitRevision = "" }},
		{name: "server state", mutate: func(value *perfvarHostMetadata) { value.GitStateHash = "" }},
		{name: "Connect revision", mutate: func(value *perfvarHostMetadata) { value.ConnectRevision = "" }},
		{name: "Connect state", mutate: func(value *perfvarHostMetadata) { value.ConnectStateHash = "" }},
	}
	for _, testCase := range cases {
		metadata := complete
		testCase.mutate(&metadata)
		if err := validatePerfvarHostMetadata(metadata); err == nil {
			t.Errorf("missing %s source identity was accepted", testCase.name)
		}
	}
}

// Dirty source identity changes for tracked and untracked content while one
// unchanged state remains stable across repeated reads.
func TestPerfvarGitStateHashesDirtyContents(t *testing.T) {
	repository := t.TempDir()
	runGit := func(arguments ...string) {
		t.Helper()
		commandArguments := append([]string{"-C", repository}, arguments...)
		if output, err := exec.Command("git", commandArguments...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", arguments, err, output)
		}
	}
	runGit("init", "--quiet")
	trackedPath := filepath.Join(repository, "tracked.txt")
	if err := os.WriteFile(trackedPath, []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "tracked.txt")
	runGit(
		"-c", "user.name=PERFVAR",
		"-c", "user.email=perfvar@example.invalid",
		"commit", "--quiet", "-m", "base",
	)
	revision, modified, cleanHash := perfvarGitState(repository)
	if revision == "" || modified || cleanHash == "" {
		t.Fatalf("clean state revision=%q modified=%t hash=%q", revision, modified, cleanHash)
	}
	_, _, repeatedCleanHash := perfvarGitState(repository)
	if repeatedCleanHash != cleanHash {
		t.Fatalf("unchanged clean hash changed: %s -> %s", cleanHash, repeatedCleanHash)
	}
	if err := os.WriteFile(trackedPath, []byte("modified\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, trackedModified, trackedHash := perfvarGitState(repository)
	if !trackedModified || trackedHash == cleanHash || trackedHash == "" {
		t.Fatalf("tracked state modified=%t clean=%s dirty=%s", trackedModified, cleanHash, trackedHash)
	}
	untrackedPath := filepath.Join(repository, "untracked.txt")
	if err := os.WriteFile(untrackedPath, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, untrackedModified, firstUntrackedHash := perfvarGitState(repository)
	if !untrackedModified || firstUntrackedHash == trackedHash || firstUntrackedHash == "" {
		t.Fatalf(
			"untracked state modified=%t tracked=%s untracked=%s",
			untrackedModified,
			trackedHash,
			firstUntrackedHash,
		)
	}
	if err := os.WriteFile(untrackedPath, []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, secondUntrackedHash := perfvarGitState(repository)
	if secondUntrackedHash == firstUntrackedHash || secondUntrackedHash == "" {
		t.Fatalf("untracked content hash did not change: %s", secondUntrackedHash)
	}
	_, _, repeatedDirtyHash := perfvarGitState(repository)
	if repeatedDirtyHash != secondUntrackedHash {
		t.Fatalf("unchanged dirty hash changed: %s -> %s", secondUntrackedHash, repeatedDirtyHash)
	}
}

// Each JSON record occupies one extractable go-test log line.
func emitPerfvarRecord(t testing.TB, record any) {
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal PERFVAR record: %v", err)
	}
	t.Logf("[perfvar] %s", encoded)
}

// Nearest-rank selection is shared by scalar and duration aggregates.
func perfvarPercentileFloat(values []float64, percentile int) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	index := (percentile*len(sorted) + 99) / 100
	return sorted[min(max(index, 1), len(sorted))-1]
}

// Duration aggregation preserves nanosecond precision in JSON.
func perfvarPercentileDuration(values []time.Duration, percentile int) time.Duration {
	if len(values) == 0 {
		return 0
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	index := (percentile*len(sorted) + 99) / 100
	return sorted[min(max(index, 1), len(sorted))-1]
}

// Individual results remain visible while the aggregate makes comparisons easy.
func aggregatePerfvarRuns(records []perfvarRunRecord) perfvarAggregateRecord {
	first := records[0]
	goodputs := make([]float64, 0, len(records))
	durations := make([]time.Duration, 0, len(records))
	setups := make([]time.Duration, 0, len(records))
	latencies := make([]time.Duration, 0, len(records))
	loadedLatencies := make([]time.Duration, 0, len(records))
	efficiencies := make([]float64, 0, len(records))
	wireEfficiencies := make([]float64, 0, len(records))
	valid := make([]bool, 0, len(records))
	correct := make([]bool, 0, len(records))
	correctRunCount := 0
	failureRunCount := 0
	invalidRunCount := 0
	for _, record := range records {
		correct = append(correct, record.Correct)
		isValid := record.Correct && record.InvalidReason == ""
		valid = append(valid, isValid)
		if record.Correct {
			correctRunCount += 1
		} else {
			failureRunCount += 1
		}
		if isValid {
			goodputs = append(goodputs, record.Tunneled.GoodputGigabits)
			durations = append(durations, record.Tunneled.Duration)
			setups = append(setups, record.Tunneled.SetupDuration)
			latencies = append(latencies, record.Tunneled.Latency.P95)
			loadedLatencies = append(loadedLatencies, record.Tunneled.LoadedLatency.P95)
			efficiencies = append(efficiencies, record.Efficiency)
			wireEfficiencies = append(wireEfficiencies, record.WireEfficiency)
		} else if record.Correct {
			invalidRunCount += 1
		}
	}
	goodputWorst := float64(0)
	durationWorst := time.Duration(0)
	if 0 < len(goodputs) {
		goodputWorst = slices.Min(goodputs)
		durationWorst = slices.Max(durations)
	}
	return perfvarAggregateRecord{
		SchemaVersion:      perfvarSchemaVersion,
		ScheduleVersion:    first.ScheduleVersion,
		RecordType:         "aggregate",
		ScenarioHash:       first.ScenarioHash,
		ProfileHash:        first.ProfileHash,
		Scenario:           first.Scenario,
		RunCount:           len(records),
		GoodputMedianGbps:  perfvarPercentileFloat(goodputs, 50),
		GoodputP95Gbps:     perfvarPercentileFloat(goodputs, 95),
		GoodputWorstGbps:   goodputWorst,
		DurationMedian:     perfvarPercentileDuration(durations, 50),
		DurationP95:        perfvarPercentileDuration(durations, 95),
		DurationWorst:      durationWorst,
		SetupMedian:        perfvarPercentileDuration(setups, 50),
		LatencyP95Median:   perfvarPercentileDuration(latencies, 50),
		LoadedP95Median:    perfvarPercentileDuration(loadedLatencies, 50),
		EfficiencyMedian:   perfvarPercentileFloat(efficiencies, 50),
		WireEfficiency:     perfvarPercentileFloat(wireEfficiencies, 50),
		CorrectRunCount:    correctRunCount,
		FailureRunCount:    failureRunCount,
		InvalidRunCount:    invalidRunCount,
		ValidRunCount:      len(records) - failureRunCount - invalidRunCount,
		IndividualCorrect:  correct,
		IndividualRunValid: valid,
	}
}

// Filter parsing rejects typos and retains the required five-run default.
func TestPerfvarScenarioFilterValidation(t *testing.T) {
	values := map[string]string{
		"CONNECT_PERFVAR_ROUTE":     "p2p-fast,exchange-h3",
		"CONNECT_PERFVAR_PROFILE":   "lte,single-region-1000ms-rtt",
		"CONNECT_PERFVAR_RUN_COUNT": "5",
	}
	config, err := loadPerfvarConfig(func(name string) string { return values[name] })
	if err != nil {
		t.Fatal(err)
	}
	if config.RunCount != 5 || !config.Profiles["single-region-1000ms-rtt"] {
		t.Fatalf("unexpected config: %+v", config)
	}
	values["CONNECT_PERFVAR_PROFILE"] = "lte-typo"
	if _, err := loadPerfvarConfig(func(name string) string { return values[name] }); err == nil {
		t.Fatal("unknown profile was accepted")
	}
	values["CONNECT_PERFVAR_PROFILE"] = "lte"
	values["CONNECT_PERFVAR_TOPOLOGY"] = "three-hops"
	if _, err := loadPerfvarConfig(func(name string) string { return values[name] }); err == nil {
		t.Fatal("unknown topology was accepted")
	}
	values["CONNECT_PERFVAR_TOPOLOGY"] = perfvarTopologySplitExchange
	values["CONNECT_PERFVAR_INTERNAL_PROFILE"] = "wan-typo"
	if _, err := loadPerfvarConfig(func(name string) string { return values[name] }); err == nil {
		t.Fatal("unknown internal exchange profile was accepted")
	}
}

// Focused impairment names remain exact config values and produce stable identities.
func TestPerfvarFocusedJitterAndReorderResolution(t *testing.T) {
	const profileFilter = "jitter-0ms,jitter-1ms,jitter-5ms,jitter-25ms,reorder-0bp,reorder-10bp,reorder-100bp,reorder-500bp"
	values := map[string]string{
		"CONNECT_PERFVAR_ROUTE":      "exchange-h3",
		"CONNECT_PERFVAR_PROFILE":    profileFilter,
		"CONNECT_PERFVAR_WORKLOAD":   "tcp",
		"CONNECT_PERFVAR_DIRECTION":  "upload",
		"CONNECT_PERFVAR_TOPOLOGY":   perfvarTopologyOneHop,
		"CONNECT_PERFVAR_RUN_COUNT":  "1",
		"CONNECT_PERFVAR_BYTE_COUNT": "65536",
	}
	config, err := loadPerfvarConfig(func(name string) string { return values[name] })
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Profiles) != 8 {
		t.Fatalf("selected profile count=%d", len(config.Profiles))
	}
	scenarios, err := resolvePerfvarScenarios(config)
	if err != nil {
		t.Fatal(err)
	}
	if len(scenarios) != 8 {
		t.Fatalf("resolved scenario count=%d", len(scenarios))
	}
	profileNames := map[string]bool{}
	scenarioHashes := map[string]bool{}
	for _, scenario := range scenarios {
		profileNames[scenario.Profile.Name] = true
		firstHash, hashErr := scenario.hash()
		if hashErr != nil {
			t.Fatal(hashErr)
		}
		secondHash, hashErr := scenario.hash()
		if hashErr != nil {
			t.Fatal(hashErr)
		}
		if firstHash == "" || firstHash != secondHash {
			t.Fatalf("profile=%s unstable scenario hashes %q %q", scenario.Profile.Name, firstHash, secondHash)
		}
		if scenarioHashes[firstHash] {
			t.Fatalf("profile=%s reused scenario hash %q", scenario.Profile.Name, firstHash)
		}
		scenarioHashes[firstHash] = true
	}
	for _, profileName := range strings.Split(profileFilter, ",") {
		if !profileNames[profileName] {
			t.Errorf("profile %q did not resolve", profileName)
		}
	}
}

// The explicit measured-payload ceiling preserves simulator and contract headroom.
func TestPerfvarPayloadBoundProtectsOpeningContract(t *testing.T) {
	values := map[string]string{
		"CONNECT_PERFVAR_BYTE_COUNT": strconv.FormatInt(perfvarMaximumPayloadByteCount, 10),
	}
	if _, err := loadPerfvarConfig(func(name string) string { return values[name] }); err != nil {
		t.Fatalf("maximum payload rejected: %v", err)
	}
	values["CONNECT_PERFVAR_BYTE_COUNT"] = strconv.FormatInt(perfvarMaximumPayloadByteCount+1, 10)
	if _, err := loadPerfvarConfig(func(name string) string { return values[name] }); err == nil {
		t.Fatal("payload beyond the contract-safe maximum was accepted")
	}
}

// The largest accepted long-transfer payload spans multiple configured BDPs
// at both regional RTTs, while the separate default remains the 64 KiB startup
// case. This proves one shared long-payload command can cover both profiles.
func TestPerfvarMaximumPayloadSpansMultipleRegionalBandwidthDelayProducts(t *testing.T) {
	profiles := initialNetworkProfiles(20260810)
	for _, profileName := range []string{
		"single-region-500ms-rtt",
		"single-region-1000ms-rtt",
	} {
		profile := profiles[profileName]
		for _, direction := range []linkProfile{profile.Forward, profile.Reverse} {
			if perfvarMaximumPayloadByteCount < 2*direction.QueueByteCount {
				t.Errorf(
					"profile=%s maximum payload=%d spans less than two BDP queues of %d bytes",
					profileName,
					perfvarMaximumPayloadByteCount,
					direction.QueueByteCount,
				)
			}
		}
	}
}

// Scenario and profile hashes are deterministic and include the resolved seed.
func TestPerfvarScenarioIdentity(t *testing.T) {
	config, err := loadPerfvarConfig(func(name string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	scenarios, err := resolvePerfvarScenarios(config)
	if err != nil {
		t.Fatal(err)
	}
	firstHash, err := scenarios[0].hash()
	if err != nil {
		t.Fatal(err)
	}
	secondHash, err := scenarios[0].hash()
	if err != nil {
		t.Fatal(err)
	}
	if firstHash == "" || firstHash != secondHash {
		t.Fatalf("unstable scenario hashes %q %q", firstHash, secondHash)
	}
}

// Existing invocations retain one-hop topology, no internal segment, and the
// established payload defaults unless an extended filter is explicit.
func TestPerfvarDefaultTopologyCompatibility(t *testing.T) {
	config, err := loadPerfvarConfig(func(name string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Topologies) != 1 || !config.Topologies[perfvarTopologyOneHop] {
		t.Fatalf("default topologies=%v", config.Topologies)
	}
	if config.PayloadBytes != 32*1024*1024 {
		t.Fatalf("default payload=%d", config.PayloadBytes)
	}
	scenarios, err := resolvePerfvarScenarios(config)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range scenarios {
		if scenario.Topology != perfvarTopologyOneHop || scenario.InternalExchangeProfile != nil {
			t.Fatalf("default scenario changed topology: %+v", scenario)
		}
	}
}

// Default bulk transfers outgrow transport buffers while keeping deliberately
// slow regional cases focused on startup behavior.
func TestPerfvarDefaultPayloadsUseLongBulkTransfers(t *testing.T) {
	values := map[string]string{
		"CONNECT_PERFVAR_ROUTE":     "exchange-h1",
		"CONNECT_PERFVAR_PROFILE":   "clean-lan,wifi-good,wan,lte,mobile-poor,single-region-500ms-rtt,single-region-1000ms-rtt",
		"CONNECT_PERFVAR_WORKLOAD":  "tcp",
		"CONNECT_PERFVAR_DIRECTION": "upload",
	}
	config, err := loadPerfvarConfig(func(name string) string { return values[name] })
	if err != nil {
		t.Fatal(err)
	}
	scenarios, err := resolvePerfvarScenarios(config)
	if err != nil {
		t.Fatal(err)
	}
	wantByteCounts := map[string]int64{
		"clean-lan":                32 * 1024 * 1024,
		"wifi-good":                32 * 1024 * 1024,
		"wan":                      32 * 1024 * 1024,
		"lte":                      20 * 1024 * 1024,
		"mobile-poor":              20 * 1024 * 1024,
		"single-region-500ms-rtt":  64 * 1024,
		"single-region-1000ms-rtt": 64 * 1024,
	}
	if len(scenarios) != len(wantByteCounts) {
		t.Fatalf("scenario count=%d want=%d", len(scenarios), len(wantByteCounts))
	}
	for _, scenario := range scenarios {
		wantByteCount, ok := wantByteCounts[scenario.Profile.Name]
		if !ok {
			t.Fatalf("unexpected profile=%s", scenario.Profile.Name)
		}
		if scenario.PayloadByteCount != wantByteCount {
			t.Errorf(
				"profile=%s payload=%d want=%d",
				scenario.Profile.Name,
				scenario.PayloadByteCount,
				wantByteCount,
			)
		}
	}
}

// Warmed TCP defaults to a 32 MiB measured transfer at regional RTTs and
// records one route-local BDP without splitting comparable route traces.
func TestPerfvarWarmedTCPResolutionIsSchemaVisible(t *testing.T) {
	values := map[string]string{
		"CONNECT_PERFVAR_ROUTE":     "p2p-fast,p2p-legacy,exchange-h1,exchange-h3",
		"CONNECT_PERFVAR_PROFILE":   "single-region-500ms-rtt,single-region-1000ms-rtt",
		"CONNECT_PERFVAR_WORKLOAD":  string(perfvarWorkloadTCPWarmed),
		"CONNECT_PERFVAR_DIRECTION": "upload,download",
	}
	config, err := loadPerfvarConfig(func(name string) string { return values[name] })
	if err != nil {
		t.Fatal(err)
	}
	scenarios, err := resolvePerfvarScenarios(config)
	if err != nil {
		t.Fatal(err)
	}
	if len(scenarios) != 16 {
		t.Fatalf("warmed regional scenario count=%d, want=16", len(scenarios))
	}
	for _, scenario := range scenarios {
		if scenario.PayloadByteCount != 32*1024*1024 {
			t.Errorf("%s/%s measured bytes=%d", scenario.Route, scenario.Profile.Name, scenario.PayloadByteCount)
		}
		wantWarmupByteCount := int64(0)
		switch {
		case scenario.Profile.Name == "single-region-500ms-rtt" &&
			(scenario.Route == fullTunRouteP2pFast || scenario.Route == fullTunRouteP2pLegacy):
			wantWarmupByteCount = 6_250_000
		case scenario.Profile.Name == "single-region-500ms-rtt":
			wantWarmupByteCount = 6_275_000
		case scenario.Profile.Name == "single-region-1000ms-rtt" &&
			(scenario.Route == fullTunRouteP2pFast || scenario.Route == fullTunRouteP2pLegacy):
			wantWarmupByteCount = 12_500_000
		case scenario.Profile.Name == "single-region-1000ms-rtt":
			wantWarmupByteCount = 12_525_000
		}
		if scenario.WarmupByteCount != wantWarmupByteCount {
			t.Errorf(
				"%s/%s/%s warmup=%d, want=%d",
				scenario.Route,
				scenario.Profile.Name,
				scenario.Direction,
				scenario.WarmupByteCount,
				wantWarmupByteCount,
			)
		}
		encoded, marshalErr := json.Marshal(scenario)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if !bytes.Contains(encoded, []byte(`"workload":"tcp-warmed"`)) ||
			!bytes.Contains(encoded, []byte(`"warmup_byte_count":`)) {
			t.Errorf("warmed schema fields missing from %s", encoded)
		}
	}
}

// The largest supported composed BDP remains within one explicit 256 MiB
// contract, while the validator rejects a future oversized profile exactly.
func TestPerfvarWarmedTCPContractBoundIsExplicit(t *testing.T) {
	profiles := initialNetworkProfiles(20260810)
	scenario := perfvarScenario{
		Route:                 fullTunRouteP2pFast,
		Profile:               profiles["single-region-1000ms-rtt"],
		ProviderAccessProfile: profiles["clean-lan"],
		Workload:              perfvarWorkloadTCPWarmed,
		Direction:             perfvarDirectionUpload,
		Topology:              perfvarTopologyNineHop,
		PayloadByteCount:      perfvarMaximumPayloadByteCount,
	}
	scenario.WarmupByteCount = perfvarDirectionalBandwidthDelayByteCount(scenario)
	if scenario.WarmupByteCount != 112_500_000 {
		t.Fatalf("nine-hop one-second warmup=%d, want=112500000", scenario.WarmupByteCount)
	}
	if err := validatePerfvarWarmedTCPContract(scenario); err != nil {
		t.Fatalf("largest supported warmed scenario rejected: %v", err)
	}
	scenario.WarmupByteCount = perfvarPerformanceContractByteCount -
		scenario.PayloadByteCount + 1
	if err := validatePerfvarWarmedTCPContract(scenario); err == nil {
		t.Fatal("oversized route-local BDP was silently accepted")
	}
}

// Extended filters retain only routes whose production architecture supports
// the selected topology and resolve one explicit internal profile per split.
func TestPerfvarExtendedTopologyResolution(t *testing.T) {
	values := map[string]string{
		"CONNECT_PERFVAR_ROUTE":            "p2p-fast,p2p-legacy,exchange-h1,exchange-h3",
		"CONNECT_PERFVAR_PROFILE":          "clean-lan",
		"CONNECT_PERFVAR_WORKLOAD":         "tcp",
		"CONNECT_PERFVAR_DIRECTION":        "upload",
		"CONNECT_PERFVAR_TOPOLOGY":         "three-hop,five-hop,nine-hop,split-exchange",
		"CONNECT_PERFVAR_INTERNAL_PROFILE": "rtt-25ms",
	}
	config, err := loadPerfvarConfig(func(name string) string { return values[name] })
	if err != nil {
		t.Fatal(err)
	}
	scenarios, err := resolvePerfvarScenarios(config)
	if err != nil {
		t.Fatal(err)
	}
	if len(scenarios) != 5 {
		t.Fatalf("extended scenario count=%d scenarios=%+v", len(scenarios), scenarios)
	}
	seen := map[string]bool{}
	for _, scenario := range scenarios {
		seen[fmt.Sprintf("%s/%s", scenario.Route, scenario.Topology)] = true
		if scenario.Topology == perfvarTopologySplitExchange {
			if scenario.InternalExchangeProfile == nil || scenario.InternalExchangeProfile.Name != "rtt-25ms" {
				t.Fatalf("split scenario internal profile=%+v", scenario.InternalExchangeProfile)
			}
		} else if scenario.Route != fullTunRouteP2pFast || scenario.InternalExchangeProfile != nil {
			t.Fatalf("unsupported extended scenario escaped filtering: %+v", scenario)
		}
	}
	expected := []string{
		"p2p-fast/three-hop",
		"p2p-fast/five-hop",
		"p2p-fast/nine-hop",
		"exchange-h1/split-exchange",
		"exchange-h3/split-exchange",
	}
	for _, identity := range expected {
		if !seen[identity] {
			t.Fatalf("missing extended scenario %s from %v", identity, seen)
		}
	}
}

// Internal exchange profiles and repeated adjacent P2P links contribute to
// both calibration direction and stable profile identity.
func TestPerfvarExtendedTopologyCalibrationAndIdentity(t *testing.T) {
	profiles := allNetworkProfiles(7007)
	device := profiles["clean-lan"]
	device.Name = "device"
	device.Forward.BaseDelay = 3 * time.Millisecond
	device.Reverse.BaseDelay = 5 * time.Millisecond
	provider := profiles["clean-lan"]
	provider.Name = "provider"
	provider.Forward.BaseDelay = 7 * time.Millisecond
	provider.Reverse.BaseDelay = 11 * time.Millisecond
	internal := profiles["clean-lan"]
	internal.Name = "internal"
	internal.Forward.BaseDelay = 13 * time.Millisecond
	internal.Reverse.BaseDelay = 17 * time.Millisecond
	split := perfvarScenario{
		Route:                   fullTunRouteExchangeH3,
		Profile:                 device,
		ProviderAccessProfile:   provider,
		InternalExchangeProfile: &internal,
		Topology:                perfvarTopologySplitExchange,
	}
	calibration := perfvarCalibrationProfile(split)
	if calibration.Forward.BaseDelay != 27*time.Millisecond ||
		calibration.Reverse.BaseDelay != 29*time.Millisecond {
		t.Fatalf("split calibration forward=%s reverse=%s", calibration.Forward.BaseDelay, calibration.Reverse.BaseDelay)
	}
	withoutInternal := split
	withoutInternal.InternalExchangeProfile = nil
	withHash, err := split.profilesHash()
	if err != nil {
		t.Fatal(err)
	}
	withoutHash, err := withoutInternal.profilesHash()
	if err != nil {
		t.Fatal(err)
	}
	if withHash == withoutHash {
		t.Fatal("internal exchange profile did not change the profile hash")
	}
	multiHop := perfvarScenario{
		Route:                 fullTunRouteP2pFast,
		Profile:               device,
		ProviderAccessProfile: provider,
		Topology:              perfvarTopologyFiveHop,
	}
	multiHopCalibration := perfvarCalibrationProfile(multiHop)
	if multiHopCalibration.Forward.BaseDelay != 15*time.Millisecond ||
		multiHopCalibration.Reverse.BaseDelay != 25*time.Millisecond {
		t.Fatalf(
			"five-hop calibration forward=%s reverse=%s",
			multiHopCalibration.Forward.BaseDelay,
			multiHopCalibration.Reverse.BaseDelay,
		)
	}
}

// Failed runs remain visible without contaminating the throughput aggregate.
func TestPerfvarAggregateSeparatesCorrectAndFailedRuns(t *testing.T) {
	records := []perfvarRunRecord{
		{
			ScheduleVersion: perfvarScheduleVersion,
			ScenarioHash:    "scenario",
			ProfileHash:     "profile",
			Correct:         true,
			Tunneled: workloadResult{
				GoodputGigabits: 0.5,
				Duration:        2 * time.Second,
			},
			Efficiency:     0.75,
			WireEfficiency: 0.80,
		},
		{
			ScenarioHash:  "scenario",
			ProfileHash:   "profile",
			FailureStage:  "route-readiness",
			FailureReason: "timed out",
		},
	}
	aggregate := aggregatePerfvarRuns(records)
	if aggregate.ScheduleVersion != perfvarScheduleVersion {
		t.Fatalf("aggregate schedule version=%d", aggregate.ScheduleVersion)
	}
	if aggregate.CorrectRunCount != 1 || aggregate.FailureRunCount != 1 {
		t.Fatalf("aggregate counts=%d correct %d failed", aggregate.CorrectRunCount, aggregate.FailureRunCount)
	}
	if aggregate.ValidRunCount != 1 || aggregate.InvalidRunCount != 0 {
		t.Fatalf("aggregate validity=%d valid %d invalid", aggregate.ValidRunCount, aggregate.InvalidRunCount)
	}
	if aggregate.GoodputMedianGbps != 0.5 || aggregate.DurationMedian != 2*time.Second {
		t.Fatalf("aggregate included failed run: %+v", aggregate)
	}
	if !slices.Equal(aggregate.IndividualCorrect, []bool{true, false}) {
		t.Fatalf("individual correctness=%v", aggregate.IndividualCorrect)
	}
}

// Correct but calibration-invalid runs stay visible without contributing to
// performance percentiles or comparison claims.
func TestPerfvarAggregateExcludesCalibrationInvalidRuns(t *testing.T) {
	aggregate := aggregatePerfvarRuns([]perfvarRunRecord{
		{
			ScenarioHash: "scenario",
			ProfileHash:  "profile",
			Correct:      true,
			Tunneled: workloadResult{
				GoodputGigabits: 0.5,
				Duration:        2 * time.Second,
				SetupDuration:   3 * time.Millisecond,
				Latency:         latencyDistribution{P95: 4 * time.Millisecond},
				LoadedLatency:   latencyDistribution{P95: 5 * time.Millisecond},
			},
			Efficiency:     0.6,
			WireEfficiency: 0.7,
		},
		{
			ScenarioHash:  "scenario",
			ProfileHash:   "profile",
			Correct:       true,
			InvalidReason: "calibration ceiling",
			Tunneled: workloadResult{
				GoodputGigabits: 10,
				Duration:        time.Millisecond,
				SetupDuration:   time.Nanosecond,
				Latency:         latencyDistribution{P95: time.Nanosecond},
				LoadedLatency:   latencyDistribution{P95: time.Nanosecond},
			},
			Efficiency:     10,
			WireEfficiency: 10,
		},
	})
	if aggregate.CorrectRunCount != 2 || aggregate.ValidRunCount != 1 || aggregate.InvalidRunCount != 1 {
		t.Fatalf("aggregate validity counts: %+v", aggregate)
	}
	if aggregate.GoodputMedianGbps != 0.5 || aggregate.DurationMedian != 2*time.Second {
		t.Fatalf("invalid run contaminated aggregate: %+v", aggregate)
	}
	if aggregate.SetupMedian != 3*time.Millisecond ||
		aggregate.LatencyP95Median != 4*time.Millisecond ||
		aggregate.LoadedP95Median != 5*time.Millisecond ||
		aggregate.EfficiencyMedian != 0.6 || aggregate.WireEfficiency != 0.7 {
		t.Fatalf("invalid run contaminated secondary metrics: %+v", aggregate)
	}
	if !slices.Equal(aggregate.IndividualCorrect, []bool{true, true}) ||
		!slices.Equal(aggregate.IndividualRunValid, []bool{true, false}) {
		t.Fatalf(
			"aggregate validity vectors correct=%v valid=%v",
			aggregate.IndividualCorrect,
			aggregate.IndividualRunValid,
		)
	}
	if aggregate.ValidRunCount+aggregate.InvalidRunCount+aggregate.FailureRunCount != aggregate.RunCount {
		t.Fatalf("aggregate validity categories do not sum to run count: %+v", aggregate)
	}
}

// An entirely failed repetition set is a valid diagnostic aggregate with
// zero performance metrics rather than a percentile panic.
func TestPerfvarAggregateHandlesAllFailedRuns(t *testing.T) {
	aggregate := aggregatePerfvarRuns([]perfvarRunRecord{
		{
			ScenarioHash:  "scenario",
			ProfileHash:   "profile",
			FailureStage:  "workload",
			FailureReason: "timed out",
		},
	})
	if aggregate.CorrectRunCount != 0 || aggregate.FailureRunCount != 1 {
		t.Fatalf("aggregate counts=%d correct %d failed", aggregate.CorrectRunCount, aggregate.FailureRunCount)
	}
	if aggregate.ValidRunCount != 0 || aggregate.InvalidRunCount != 0 {
		t.Fatalf("failed-only validity=%d valid %d invalid", aggregate.ValidRunCount, aggregate.InvalidRunCount)
	}
	if aggregate.GoodputMedianGbps != 0 || aggregate.DurationWorst != 0 {
		t.Fatalf("failed-only aggregate has performance values: %+v", aggregate)
	}
	if !slices.Equal(aggregate.IndividualCorrect, []bool{false}) ||
		!slices.Equal(aggregate.IndividualRunValid, []bool{false}) {
		t.Fatalf(
			"failed-only vectors correct=%v valid=%v",
			aggregate.IndividualCorrect,
			aggregate.IndividualRunValid,
		)
	}
}

// Single-region profiles model the requested latency between the application
// user and server/connect without accidentally charging the same regional RTT
// to a colocated provider. Dual-region profiles retain the symmetric stress
// case as an explicit, separately named scenario.
func TestPerfvarSingleRegionAccessScope(t *testing.T) {
	values := map[string]string{
		"CONNECT_PERFVAR_ROUTE":     "exchange-h3",
		"CONNECT_PERFVAR_PROFILE":   "single-region-500ms-rtt",
		"CONNECT_PERFVAR_WORKLOAD":  "tcp",
		"CONNECT_PERFVAR_DIRECTION": "upload",
	}
	config, err := loadPerfvarConfig(func(name string) string { return values[name] })
	if err != nil {
		t.Fatal(err)
	}
	scenarios, err := resolvePerfvarScenarios(config)
	if err != nil {
		t.Fatal(err)
	}
	if len(scenarios) != 1 {
		t.Fatalf("single-region scenario count=%d", len(scenarios))
	}
	scenario := scenarios[0]
	applicationRoundTrip := scenario.Profile.Forward.BaseDelay + scenario.Profile.Reverse.BaseDelay
	providerRoundTrip := scenario.ProviderAccessProfile.Forward.BaseDelay +
		scenario.ProviderAccessProfile.Reverse.BaseDelay
	if applicationRoundTrip != singleRegionMinimumRoundTrip {
		t.Fatalf("application access RTT=%s", applicationRoundTrip)
	}
	if providerRoundTrip != 2*time.Millisecond {
		t.Fatalf("provider access RTT=%s want=2ms", providerRoundTrip)
	}
	calibration := perfvarCalibrationProfile(scenario)
	endToEndRoundTrip := calibration.Forward.BaseDelay + calibration.Reverse.BaseDelay
	if endToEndRoundTrip != singleRegionMinimumRoundTrip+2*time.Millisecond {
		t.Fatalf("end-to-end calibration RTT=%s want=%s", endToEndRoundTrip, singleRegionMinimumRoundTrip+2*time.Millisecond)
	}
}

// The process environment is read only by the opt-in performance entry point.
func currentPerfvarConfig() (perfvarConfig, error) {
	return loadPerfvarConfig(os.Getenv)
}
