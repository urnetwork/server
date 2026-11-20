// Fixed workload-boundary observations do not change packet or route policy.
// Only the separately named research root opts into four-packet recovery.
package perfvar

import (
	"context"
	"errors"
	"os"
	"reflect"
	"runtime"
	runtimeMetrics "runtime/metrics"
	"strconv"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/sdk/v2026"
)

// Positive H1 preferences preserve the real Auto policy and legacy H1 lane.
func nativeBrowsingAutoH1OnlySettingsForTest() *sdk.TransportSettings {
	priorities := sdk.NewTransportModePriorityList()
	priorities.Add(&sdk.TransportModePriority{Mode: sdk.TransportModeH1, Priority: 1})
	return &sdk.TransportSettings{Mode: sdk.TransportModeAuto, AutoModePriorities: priorities}
}

// Structural eligibility is recorded separately from measured connectivity.
func nativeBrowsingAutoH1OnlyPolicyError(device *sdk.DeviceLocal) error {
	settings := device.GetTransportSettings()
	if settings == nil || settings.Mode != sdk.TransportModeAuto {
		return errors.New("Auto H1-only control lost ModeAuto")
	}
	modes := settings.AutoModes()
	if modes == nil || modes.Len() != 1 || modes.Get(0) != sdk.TransportModeH1 {
		return errors.New("Auto H1-only control enabled another carrier")
	}
	status := device.GetTransportStatus()
	if status == nil || status.AutoEligibleModes == nil || status.AutoEligibleModes.Len() != 1 || status.AutoEligibleModes.Get(0) != sdk.TransportModeH1 {
		return errors.New("Auto H1-only control lacks structurally eligible H1")
	}
	return nil
}

// The current source has a fixed one-packet policy and no configurable field.
// Read a candidate's actual configured field once, before NAT publication.
func c48ReturnFlightConfiguredPacketLimit(settings *clientconnect.TcpBufferSettings) int {
	if settings != nil {
		field := reflect.ValueOf(settings).Elem().FieldByName("ReturnRecoveryMaxPacketCount")
		if field.IsValid() && field.Kind() == reflect.Int && field.Int() > 0 {
			return int(field.Int())
		}
	}
	return 1
}

// These are process observations; they are never a phone memory qualification.
type c48ReturnFlightRuntimeMemory struct {
	HeapAlloc               uint64                  `json:"heap_alloc"`
	HeapInuse               uint64                  `json:"heap_inuse"`
	HeapReleased            uint64                  `json:"heap_released"`
	RuntimeSys              uint64                  `json:"runtime_sys"`
	RuntimeSysMinusReleased uint64                  `json:"runtime_sys_minus_released"`
	TotalAlloc              uint64                  `json:"total_alloc"`
	Mallocs                 uint64                  `json:"mallocs"`
	NumGc                   uint32                  `json:"num_gc"`
	PauseTotalNs            uint64                  `json:"pause_total_ns"`
	Goroutines              int                     `json:"goroutines"`
	PoolRoots               uint64                  `json:"pool_roots"`
	PoolBytes               clientconnect.ByteCount `json:"pool_bytes"`
	GoMemoryLimitBytes      string                  `json:"go_memory_limit_bytes"`
}

// One standard runtime sample at a workload boundary; no GC is forced.
func c48ReturnFlightRuntimeMemoryNow() c48ReturnFlightRuntimeMemory {
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	samples := []runtimeMetrics.Sample{{Name: "/gc/gomemlimit:bytes"}}
	runtimeMetrics.Read(samples)
	limit := "UNKNOWN"
	if samples[0].Value.Kind() == runtimeMetrics.KindUint64 {
		limit = strconv.FormatUint(samples[0].Value.Uint64(), 10)
	}
	return c48ReturnFlightRuntimeMemory{HeapAlloc: memory.HeapAlloc, HeapInuse: memory.HeapInuse,
		HeapReleased: memory.HeapReleased, RuntimeSys: memory.Sys, RuntimeSysMinusReleased: memory.Sys - memory.HeapReleased,
		TotalAlloc: memory.TotalAlloc, Mallocs: memory.Mallocs, NumGc: memory.NumGC, PauseTotalNs: memory.PauseTotalNs,
		Goroutines: runtime.NumGoroutine(), PoolRoots: clientconnect.MessagePoolOutstandingCount(),
		PoolBytes: clientconnect.MessagePoolOutstandingByteCount(), GoMemoryLimitBytes: limit}
}

// Link counters are summed once per boundary, without retaining link identities.
type c48ReturnFlightNetworkTotals struct {
	LinkCount            int    `json:"link_count"`
	AdmittedPackets      uint64 `json:"admitted_packets"`
	DeliveredPackets     uint64 `json:"delivered_packets"`
	DeliveredBytes       uint64 `json:"delivered_bytes"`
	WireBytes            uint64 `json:"wire_bytes"`
	LossDrops            uint64 `json:"loss_drops"`
	AllowedLossDrops     uint64 `json:"allowed_loss_drops"`
	UnexpectedLossDrops  uint64 `json:"unexpected_loss_drops"`
	QueueDrops           uint64 `json:"queue_drops"`
	AllowedQueueDrops    uint64 `json:"allowed_queue_drops"`
	UnexpectedQueueDrops uint64 `json:"unexpected_queue_drops"`
	MtuDrops             uint64 `json:"mtu_drops"`
	OutageDrops          uint64 `json:"outage_drops"`
	ReceiverDrops        uint64 `json:"receiver_drops"`
	CanceledDrops        uint64 `json:"canceled_drops"`
	QueuedPackets        int    `json:"queued_packets"`
	QueuedBytes          int    `json:"queued_bytes"`
}

// Existing provider observations distinguish offers from completed Transfer sends.
// Neither counter proves the source kernel acknowledged the inner TCP bytes.
type c48ReturnFlightProviderTotals struct {
	ReadyProviders         int                           `json:"ready_providers"`
	ConfiguredPacketLimits [sdkBrowsingProviderCount]int `json:"configured_packet_limits"`
	InitialWrites          uint64                        `json:"initial_writes"`
	TimeoutResends         uint64                        `json:"timeout_resends"`
	ReturnStartedPackets   int64                         `json:"return_started_packets"`
	ReturnStartedBytes     int64                         `json:"return_started_bytes"`
	ReturnCompletedPackets int64                         `json:"return_completed_packets"`
	ReturnCompletedBytes   int64                         `json:"return_completed_bytes"`
	ReturnFailures         int64                         `json:"return_failures"`
	ReturnFailureBytes     int64                         `json:"return_failure_bytes"`
	RetainedReturnEntries  int64                         `json:"retained_return_entries"`
	ResendBudgetBytes      int64                         `json:"resend_budget_bytes"`
	ResendUsedBytes        int64                         `json:"resend_used_bytes"`
}

// Separate atomics are a boundary aggregate, never an event-time flow snapshot.
type c48ReturnFlightBoundary struct {
	AtUnixNs        int64                         `json:"at_unix_ns"`
	Joined          bool                          `json:"joined"`
	TransportMode   string                        `json:"transport_mode"`
	AutoPolicyValid bool                          `json:"auto_policy_valid"`
	Device          *sdk.DeviceLocalMemoryUsage   `json:"device"`
	Runtime         c48ReturnFlightRuntimeMemory  `json:"runtime"`
	Network         c48ReturnFlightNetworkTotals  `json:"network"`
	Providers       c48ReturnFlightProviderTotals `json:"providers"`
}

// Sample only existing public state and existing test counters at three edges.
func c48ReturnFlightCaptureBoundary(fixture *sdkBrowsingSetupFixture, joined bool) *c48ReturnFlightBoundary {
	row := &c48ReturnFlightBoundary{AtUnixNs: time.Now().UnixNano(), Joined: joined, Runtime: c48ReturnFlightRuntimeMemoryNow()}
	if joined {
		row.Device = fixture.afterJoin
	} else {
		row.Device = fixture.device.MemoryUsed()
		if settings := fixture.device.GetTransportSettings(); settings != nil {
			row.TransportMode = settings.Mode
			if settings.Mode == sdk.TransportModeAuto {
				row.AutoPolicyValid = nativeBrowsingAutoH1OnlyPolicyError(fixture.device) == nil
			}
		}
	}
	for _, link := range fixture.environment.network.snapshotLinks() {
		row.Network.LinkCount++
		row.Network.AdmittedPackets += link.AdmittedPacketCount
		row.Network.DeliveredPackets += link.DeliveredPacketCount
		row.Network.DeliveredBytes += link.DeliveredByteCount
		row.Network.WireBytes += link.WireByteCount
		row.Network.LossDrops += link.LossDropPacketCount
		row.Network.AllowedLossDrops += link.AllowedLossDropPacketCount
		row.Network.UnexpectedLossDrops += link.UnexpectedLossDropPacketCount
		row.Network.QueueDrops += link.QueueDropPacketCount
		row.Network.AllowedQueueDrops += link.AllowedQueueDropPacketCount
		row.Network.UnexpectedQueueDrops += link.UnexpectedQueueDropPacketCount
		row.Network.MtuDrops += link.MtuDropPacketCount
		row.Network.OutageDrops += link.OutageDropPacketCount
		row.Network.ReceiverDrops += link.ReceiverDropPacketCount
		row.Network.CanceledDrops += link.CanceledDropPacketCount
		row.Network.QueuedPackets += link.QueuedPacketCount
		row.Network.QueuedBytes += link.QueuedByteCount
	}
	for ordinal := range fixture.providers {
		provider := &fixture.providers[ordinal]
		if !provider.ready.Load() && !joined {
			continue
		}
		if provider.client == nil {
			continue
		}
		row.Providers.ReadyProviders++
		row.Providers.ConfiguredPacketLimits[ordinal] = provider.returnRecoveryPacketLimitForTest
		recovery := provider.client.SendRecoveryStats()
		row.Providers.InitialWrites += recovery.InitialWriteCount
		row.Providers.TimeoutResends += recovery.TimeoutResendWriteCount
		if provider.returns != nil {
			row.Providers.ReturnStartedPackets += provider.returns.startedPacketCount.Load()
			row.Providers.ReturnStartedBytes += provider.returns.startedByteCount.Load()
			row.Providers.ReturnCompletedPackets += provider.returns.completedPacketCount.Load()
			row.Providers.ReturnCompletedBytes += provider.returns.completedByteCount.Load()
			row.Providers.ReturnFailures += provider.returns.failures.Load()
			row.Providers.ReturnFailureBytes += provider.returns.failureByteCount.Load()
			row.Providers.RetainedReturnEntries += provider.returns.retainedEntryCount.Load()
		}
		if provider.resendBudget != nil {
			row.Providers.ResendBudgetBytes += int64(provider.resendBudget.TotalByteCount())
			row.Providers.ResendUsedBytes += int64(provider.resendBudget.UsedByteCount())
		}
	}
	return row
}

// Failures remain failures; the next independent arm may collect a numeric miss
// only after raw Go and full cleanup receipts have been inspected.
type c48ReturnFlightOutcome struct {
	ClassificationVersion      int    `json:"classification_version"`
	LatencyDataValid           bool   `json:"latency_data_valid"`
	GoalGatePassed             bool   `json:"goal_gate_passed"`
	WorkloadDeadlineOnly       bool   `json:"workload_deadline_only"`
	WorkloadOwnershipFailed    bool   `json:"workload_ownership_failed"`
	WorkloadAdditionalFailure  bool   `json:"workload_additional_failure"`
	WorkloadErrorClass         string `json:"workload_error_class"`
	VerdictEvaluated           bool   `json:"verdict_evaluated"`
	LatencyGateFailed          bool   `json:"latency_gate_failed"`
	ColdChainLatencyGateFailed bool   `json:"cold_chain_latency_gate_failed"`
	PayloadMismatch            bool   `json:"payload_mismatch"`
	LateConstructorFailed      bool   `json:"late_constructor_failed"`
	WitnessVerdictFailed       bool   `json:"witness_verdict_failed"`
	WitnessSnapshotFailed      bool   `json:"witness_snapshot_failed"`
	CleanupFailed              bool   `json:"cleanup_failed"`
	AutoPolicyFailed           bool   `json:"auto_policy_failed"`
}

// Unstarted pages retain -1 timepoints and their explicit state in the fixed100.
type c48ReturnFlightPerformance struct {
	Version                       int                             `json:"version"`
	PlannedPages                  int                             `json:"planned_pages"`
	AutoH1Only                    bool                            `json:"auto_h1_only"`
	AutoPolicyBeforeCloseValid    bool                            `json:"auto_policy_before_close_valid"`
	WorkloadStartUnixNs           int64                           `json:"workload_start_unix_ns"`
	WorkloadEndOffsetNs           int64                           `json:"workload_end_offset_ns"`
	BeforeNavigationNs            [nativeBrowsingPageCount]int64  `json:"before_navigation_ns"`
	AfterHtmlNs                   [nativeBrowsingPageCount]int64  `json:"after_html_ns"`
	PageStates                    [nativeBrowsingPageCount]string `json:"page_states"`
	BeforeWork                    *c48ReturnFlightBoundary        `json:"before_work"`
	AfterWork                     *c48ReturnFlightBoundary        `json:"after_work"`
	AfterJoin                     *c48ReturnFlightBoundary        `json:"after_join"`
	Outcome                       c48ReturnFlightOutcome          `json:"outcome"`
	InnerReturnRepairPackets      *uint64                         `json:"inner_return_repair_packets"`
	InnerValidatedAckPayloadBytes *uint64                         `json:"inner_validated_ack_payload_bytes"`
	InnerReturnMetricState        string                          `json:"inner_return_metric_state"`
	start                         time.Time
}

// No new trace or per-packet observer is enrolled by this performance lane.
func newC48ReturnFlightPerformance(autoH1Only bool) *c48ReturnFlightPerformance {
	row := &c48ReturnFlightPerformance{Version: 1, PlannedPages: nativeBrowsingPageCount, AutoH1Only: autoH1Only,
		InnerReturnMetricState: "MISSING_PLAIN_PERFORMANCE_LANE_SCALAR_DIAGNOSTIC_HELD"}
	for page := range row.PageStates {
		row.PageStates[page] = "not_started"
		row.BeforeNavigationNs[page], row.AfterHtmlNs[page] = -1, -1
	}
	return row
}

// The workload invokes navigation serially before its existing witness hook.
func (self *c48ReturnFlightPerformance) beforeNavigation(page int) {
	self.PageStates[page] = "started"
	self.BeforeNavigationNs[page] = int64(time.Since(self.start))
}

// Only the HTML slot writes its own fixed row; asset callbacks do not write here.
func (self *c48ReturnFlightPerformance) afterHtml(page int, err error) {
	self.PageStates[page] = "html_returned"
	if err != nil {
		self.PageStates[page] = "html_error"
	}
	self.AfterHtmlNs[page] = int64(time.Since(self.start))
}

// This retains Auto's positive H1 preference rather than switching to H1 mode.
func TestFullTunH1Sdk32InnerH2ColdLinkCell100PagesAutoH1Only(t *testing.T) {
	runSdkBrowsingColdLink100(t, cellEdge5mDown1mUpName, true, sdkBrowsingColdLinkOptions{autoH1Only: true})
}

// This separate opt-in row retains the historical B4 research configuration;
// it does not turn ordinary SDK/provider roots into recovery experiments.
func TestFullTunH1Sdk32InnerH2ColdLinkCell100PagesAutoH1OnlyReturnFlight4(t *testing.T) {
	if os.Getenv("CONNECT_PERFVAR_RETURN_FLIGHT_RESEARCH") != "1" {
		t.Skip("opt-in return-flight4 research; set CONNECT_PERFVAR_RETURN_FLIGHT_RESEARCH=1")
	}
	runSdkBrowsingColdLink100(t, cellEdge5mDown1mUpName, true, sdkBrowsingColdLinkOptions{
		autoH1Only: true, providerReturnRecoveryMaxPacketCount: 4,
	})
}

// Preserve the original joined error text while retaining an owner-cleanup
// failure that a deadline elsewhere in the error tree must not disguise.
type c48ReturnFlightWorkloadOwnerFailure struct {
	cause error
}

// Existing fatal logging keeps its original error bytes and first-error order.
func (self *c48ReturnFlightWorkloadOwnerFailure) Error() string {
	return self.cause.Error()
}

// Payload and context sentinels remain visible through standard error queries.
func (self *c48ReturnFlightWorkloadOwnerFailure) Unwrap() error {
	return self.cause
}

// Only one cause reaching the actual deadline sentinel qualifies. Multiple
// joined causes, owner-cleanup markers and unknown leaves stay hard failures.
func c48ReturnFlightDeadlineOnly(err error) bool {
	var visit func(error, int) bool
	visit = func(current error, depth int) bool {
		if current == nil || depth > 32 {
			return false
		}
		if current == context.DeadlineExceeded {
			return true
		}
		if _, owned := current.(*c48ReturnFlightWorkloadOwnerFailure); owned {
			return false
		}
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			causes := joined.Unwrap()
			return len(causes) == 1 && visit(causes[0], depth+1)
		}
		if wrapped, ok := current.(interface{ Unwrap() error }); ok {
			return visit(wrapped.Unwrap(), depth+1)
		}
		return false
	}
	return visit(err, 0)
}

// The marker is set only by the measurement's existing cleanup-error branches.
func c48ReturnFlightWorkloadOwnershipFailed(err error) bool {
	var owned *c48ReturnFlightWorkloadOwnerFailure
	return errors.As(err, &owned)
}

// Preserve payload/cancellation categories, but never let errors.Is(deadline)
// turn an additional cause or an owner-cleanup failure into a numeric miss.
func c48ReturnFlightWorkloadErrorClass(err error) string {
	class := nativeBrowsingErrorClass(err)
	if class == "deadline" && !c48ReturnFlightDeadlineOnly(err) {
		return "other"
	}
	return class
}

// A measured exceedance remains latency data when all100 original requests,
// timepoints and owners are valid. GoalGatePassed is deliberately independent;
// the parent must still bind raw Go/assertion/source and child/group receipts.
func c48ReturnFlightLatencyDataValid(observation *nativeBrowsingObservation, cold nativeBrowsingColdLinkObservation,
	outcome c48ReturnFlightOutcome, afterJoin *c48ReturnFlightBoundary) bool {
	if outcome.ClassificationVersion != 2 || outcome.WorkloadErrorClass != "" || !outcome.VerdictEvaluated ||
		outcome.WorkloadDeadlineOnly || outcome.WorkloadOwnershipFailed || outcome.WorkloadAdditionalFailure ||
		outcome.PayloadMismatch || outcome.LateConstructorFailed || outcome.WitnessVerdictFailed ||
		outcome.WitnessSnapshotFailed || outcome.CleanupFailed || outcome.AutoPolicyFailed {
		return false
	}
	if observation == nil || observation.Pressure == nil || observation.CompletedNavigations != 100 || observation.OverlapProofs != 99 ||
		cold.Protocol != "h2" || cold.Html != 100 || cold.Assets != 400 || cold.ClientAcquisitions != 500 || cold.ServerAttempts != 500 ||
		cold.ColdEdges != 1 || cold.VisitedDestinationEdges != 98 || cold.SameSessionEdges != 98 ||
		cold.ClientSessions != 5 || cold.ServerSessions != 5 || !cold.CssOwnerRetained || cold.ColdClientReused || !cold.ColdTupleOwned {
		return false
	}
	if afterJoin == nil || !afterJoin.Joined || afterJoin.Device == nil || afterJoin.Providers.RetainedReturnEntries != 0 ||
		afterJoin.Providers.ResendUsedBytes != 0 {
		return false
	}
	memory := afterJoin.Device
	if memory.TargetByteCount != 32*1024*1024 || memory.TransferRootUsedByteCount != 0 ||
		memory.TransferRootReservedByteCount != memory.TransferRootReleasedByteCount || memory.NatUsedByteCount != 0 ||
		memory.NatReservedByteCount != memory.NatReleasedByteCount || memory.PlatformTransportUsedByteCount != 0 ||
		memory.PlatformTransportUsedCount != 0 || memory.PlatformTransportPendingH1Count != 0 ||
		memory.PlatformTransportPendingHandoffCount != 0 || memory.PlatformTransportActiveHandoffCount != 0 ||
		memory.PlatformTransportReservedBytes != memory.PlatformTransportReleasedBytes {
		return false
	}
	limit := int64(sdk.DefaultDeviceLocalSettings().SendTimeout)
	positiveExceedance := false
	for page, row := range observation.Pages {
		combined := cold.CombinedFirstByteNs[page]
		if row.Page != page || row.FirstByteNanoseconds <= 0 || row.CompletionNanoseconds <= 0 || combined <= 0 {
			return false
		}
		if row.FirstByteNanoseconds >= limit || combined >= limit {
			positiveExceedance = true
		}
	}
	return outcome.LatencyGateFailed == positiveExceedance
}
