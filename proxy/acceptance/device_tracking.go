package acceptance

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"
)

const (
	hostedDevicePollInterval      = time.Second
	hostedDeviceHistorySize       = 64
	hostedDeviceCausalHistorySize = 32
	hostedDeviceDiagnosticMaxSize = 768
)

// hostedDeviceTracker retains a credential-free timeline around a data-plane
// failure. Implementations must be safe for a probe failure and Close racing.
type hostedDeviceTracker interface {
	Diagnostic() string
	Close()
}

type hostedDeviceTrackerFactory func(context.Context, provisionResult) (hostedDeviceTracker, error)

// sdkHostedDeviceTracker reads the temporary hosted DeviceLocal through its
// existing device-rpc endpoint. Provider ids never leave aliases; the raw ids
// exist only in this in-memory map for the lifetime of one acceptance client.
type sdkHostedDeviceTracker struct {
	ctx     context.Context
	cancel  context.CancelFunc
	remote  *sdk.DeviceRemote
	aliases map[string]string

	stateLock     sync.Mutex
	history       []string
	causalHistory []string
	lastSignature string
	closeOnce     sync.Once
	packetState   hostedDevicePacketState
	lastMetrics   sdk.ReliabilityMetrics
	metricsReady  bool
	lastExitState map[string]hostedDeviceExitState
}

type hostedDevicePacketState struct {
	initialized         bool
	lastStats           sdk.PacketStats
	lastRemoteIngressAt time.Time
	lastRemoteIngressN  int64
	lastRemoteIngressB  sdk.ByteCount
}

type hostedDeviceExitState struct {
	alias       string
	flowCount   int32
	warning     bool
	quarantined bool
	done        bool
	cause       string
}

// productionHostedDeviceTracker attaches only when provisioning returned the
// three credentials that identify the exact hosted DeviceLocal generation.
func (r *runner) productionHostedDeviceTracker(ctx context.Context, provisioned provisionResult) (hostedDeviceTracker, error) {
	config := provisioned.ProxyConfigResult
	if provisioned.ByClientJWT == "" || config == nil || config.APIBaseURL == "" || config.AuthToken == "" || config.InstanceID == "" {
		return nil, errors.New("provision response omitted hosted device-rpc credentials")
	}
	instanceID, err := sdk.ParseId(config.InstanceID)
	if err != nil {
		return nil, fmt.Errorf("parse hosted device instance id: %w", err)
	}

	trackerCtx, cancel := context.WithCancel(ctx)
	settings := connect.DefaultClientStrategySettings()
	settings.Log = connect.NewNoopLogger()
	settings.ConnectSettings.Log = settings.Log
	networkSpace := sdk.NewNetworkSpaceWithUrls(
		trackerCtx,
		r.opts.APIURL,
		"wss://connect.bringyour.com",
		settings,
	)
	remote, err := sdk.NewPlatformDeviceRemote(
		networkSpace,
		provisioned.ByClientJWT,
		config.APIBaseURL,
		config.AuthToken,
		instanceID,
	)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("create hosted device tracker: %w", err)
	}
	tracker := &sdkHostedDeviceTracker{
		ctx:     trackerCtx,
		cancel:  cancel,
		remote:  remote,
		aliases: map[string]string{},
	}
	tracker.record(time.Now(), "remote=connecting")
	go tracker.run()
	return tracker, nil
}

// run samples more frequently than the sustained request cadence, preserving
// the provider that owned a flow before request cancellation removes it.
func (self *sdkHostedDeviceTracker) run() {
	for {
		self.record(time.Now(), self.snapshot())
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(hostedDevicePollInterval):
		}
	}
}

// snapshot performs read-only RPCs. An unavailable remote is still a useful
// state transition, but rejection text is categorized so ids cannot leak.
func (self *sdkHostedDeviceTracker) snapshot() string {
	if !self.remote.GetRemoteConnected() {
		syncState := "none"
		syncError := self.remote.GetSyncError()
		switch {
		case strings.HasPrefix(syncError, "device rpc version mismatch"):
			syncState = "version-mismatch"
		case strings.HasPrefix(syncError, "device instance mismatch"):
			syncState = "instance-mismatch"
		case syncError != "":
			syncState = "rejected"
		}
		return fmt.Sprintf("remote=disconnected sync_error=%s", syncState)
	}
	now := time.Now()
	metrics := self.remote.GetReliabilityMetrics()
	exits := self.remote.GetExits()
	snapshot := formatHostedDeviceState(
		self.remote.GetWindowStatus(),
		metrics,
		self.packetState.summary(now, self.remote.GetPacketStats()),
		exits,
		self.remote.GetDestinationExits(),
		self.aliases,
	)
	self.recordReliability(now, metrics)
	self.recordExitTransitions(now, exits)
	return snapshot
}

// formatHostedDeviceState keeps the timeline compact enough to survive the
// acceptance result limit while retaining every causal provider signal.
func formatHostedDeviceState(
	window *sdk.WindowStatus,
	metrics *sdk.ReliabilityMetrics,
	packetSummary string,
	exits *sdk.ExitList,
	destinations *sdk.DestinationExitList,
	aliases map[string]string,
) string {
	if aliases == nil {
		aliases = map[string]string{}
	}
	providerIDs := []string{}
	if exits != nil {
		for i := 0; i < exits.Len(); i++ {
			exit := exits.Get(i)
			if exit != nil && exit.ClientId != nil {
				providerIDs = append(providerIDs, exit.ClientId.String())
			}
		}
	}
	if destinations != nil {
		for i := 0; i < destinations.Len(); i++ {
			destination := destinations.Get(i)
			if destination != nil && destination.ClientId != nil {
				providerIDs = append(providerIDs, destination.ClientId.String())
			}
		}
	}
	sort.Strings(providerIDs)
	for _, providerID := range providerIDs {
		if _, ok := aliases[providerID]; !ok {
			aliases[providerID] = fmt.Sprintf("p%d", len(aliases)+1)
		}
	}
	providerAlias := func(id *sdk.Id) string {
		if id == nil {
			return "unknown"
		}
		if alias, ok := aliases[id.String()]; ok {
			return alias
		}
		return "unknown"
	}

	windowSummary := "unknown"
	if window != nil {
		windowSummary = fmt.Sprintf(
			"%d/%d min=%t eval=%d failed_eval=%d not_added=%d removed=%d stall=%s failed=%t",
			window.ProviderStateAdded,
			window.TargetSize,
			window.MinSatisfied,
			window.ProviderStateInEvaluation,
			window.ProviderStateEvaluationFailed,
			window.ProviderStateNotAdded,
			window.ProviderStateRemoved,
			compactDiagnosticToken(window.StallReason),
			window.Failed,
		)
	}
	metricsSummary := "unknown"
	if metrics != nil {
		metricsSummary = fmt.Sprintf(
			"flows=%d exit_loss=%d lost=%d recovery=%d/%d pending=%d dial=%d reraced=%d qreset=%d sticky=%d held=%d deferred=%d",
			metrics.FlowsOpened,
			metrics.ExitLossEvents,
			metrics.FlowsLostToExit,
			metrics.RecoveryCount,
			metrics.RecoveryMissed,
			metrics.RecoveryPending,
			metrics.DialFailuresIntercepted,
			metrics.FlowsReraced,
			metrics.QuarantineTcpResets,
			metrics.StickyFlowsRetired,
			metrics.VerdictsHeldUplinkStale+metrics.VerdictsHeldTransportDown+metrics.VerdictsHeldSharedFate,
			metrics.RemovalsDeferred,
		)
	}
	if packetSummary == "" {
		packetSummary = "unknown"
	}

	destinationSummaries := []string{}
	if destinations != nil {
		for i := 0; i < destinations.Len(); i++ {
			destination := destinations.Get(i)
			if destination == nil {
				continue
			}
			destinationSummaries = append(destinationSummaries, fmt.Sprintf(
				"%s->%s(%d)",
				compactDiagnosticToken(destination.DestinationIp),
				providerAlias(destination.ClientId),
				destination.FlowCount,
			))
		}
	}
	// Resolver and qualification traffic is usually present on every sample.
	// Put it after ordinary destinations so it cannot consume the bounded list
	// before the HTTPS address whose request is currently stalled. A reverse
	// lexical sort did not provide that property: 8.8.8.8 sorted ahead of a
	// Cloudflare 172.x target and erased the target/provider join in production
	// diagnostics.
	sort.Slice(destinationSummaries, func(i int, j int) bool {
		iInfrastructure := isHostedDeviceInfrastructureDestination(destinationSummaries[i])
		jInfrastructure := isHostedDeviceInfrastructureDestination(destinationSummaries[j])
		if iInfrastructure != jInfrastructure {
			return !iInfrastructure
		}
		return destinationSummaries[j] < destinationSummaries[i]
	})
	if 8 < len(destinationSummaries) {
		destinationSummaries = destinationSummaries[:8]
	}

	type activeExitSummary struct {
		flowCount int32
		value     string
	}
	activeExits := []activeExitSummary{}
	totalExits := 0
	if exits != nil {
		totalExits = exits.Len()
		for i := 0; i < exits.Len(); i++ {
			exit := exits.Get(i)
			if exit == nil || (exit.FlowCount == 0 && exit.DialFailureCount == 0 && !exit.Warning && !exit.Quarantined && !exit.Done) {
				continue
			}
			activeExits = append(activeExits, activeExitSummary{
				flowCount: exit.FlowCount,
				value: fmt.Sprintf(
					"%s(flow=%d dial=%d tier=%d/%d warn=%t quarantine=%t done=%t cause=%s proven=%t blocks=%d/%d seq=%d build=%s)",
					providerAlias(exit.ClientId),
					exit.FlowCount,
					exit.DialFailureCount,
					exit.Tier,
					exit.EffectiveTier,
					exit.Warning,
					exit.Quarantined,
					exit.Done,
					compactDiagnosticToken(exit.WarningCause),
					exit.Proven,
					exit.ProviderBlockIngressPacketCount,
					exit.ProviderBlockEgressPacketCount,
					exit.ProviderDiagnosticsSequence,
					compactDiagnosticToken(exit.ProviderBuildVersion),
				),
			})
		}
	}
	sort.Slice(activeExits, func(i int, j int) bool {
		if activeExits[i].flowCount != activeExits[j].flowCount {
			return activeExits[j].flowCount < activeExits[i].flowCount
		}
		return activeExits[i].value < activeExits[j].value
	})
	activeExitSummaries := make([]string, 0, len(activeExits))
	for _, exit := range activeExits {
		activeExitSummaries = append(activeExitSummaries, exit.value)
	}
	if 8 < len(activeExitSummaries) {
		activeExitSummaries = activeExitSummaries[:8]
	}
	// Keep the packet boundary immediately after readiness. A long causal-event
	// prefix can consume most of the bounded failure detail; if packet ingress is
	// placed after provider lists, the exact one-way-stall discriminator is the
	// first field truncated. Reliability changes are already retained separately
	// in causalHistory, while destinations and active exits follow here to join a
	// surviving flow to its carrier.
	return fmt.Sprintf(
		"remote=connected window={%s} packets={%s} destinations=[%s] active=[%s] reliability={%s} exits=%d",
		windowSummary,
		packetSummary,
		strings.Join(destinationSummaries, ","),
		strings.Join(activeExitSummaries, ","),
		metricsSummary,
		totalExits,
	)
}

func isHostedDeviceInfrastructureDestination(summary string) bool {
	for _, prefix := range []string{
		"1.0.0.1->",
		"1.1.1.1->",
		"8.8.4.4->",
		"8.8.8.8->",
		"9.9.9.9->",
		"149.112.112.112->",
		"208.67.220.220->",
		"208.67.222.222->",
	} {
		if strings.HasPrefix(summary, prefix) {
			return true
		}
	}
	return false
}

// recordReliability retains only causal control-plane changes. FlowsOpened and
// packet totals intentionally stay out: they advance on every healthy request
// and used to evict the one provider-removal or quarantine event that explains
// a failure several seconds later.
func (self *sdkHostedDeviceTracker) recordReliability(now time.Time, metrics *sdk.ReliabilityMetrics) {
	if metrics == nil {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !self.metricsReady {
		self.lastMetrics = *metrics
		self.metricsReady = true
		return
	}

	previous := self.lastMetrics
	self.lastMetrics = *metrics
	if reliabilityMetricsRegressed(previous, *metrics) {
		// DeviceRemote reports a zero snapshot while its rpc generation is
		// reconnecting, and an explicit metrics reset can also move every
		// counter backwards. Neither is a negative causal event. Retain one
		// boundary marker, then let the next successful sample establish a
		// fresh baseline so restored counters are not reported as a huge jump.
		self.metricsReady = false
		self.appendCausalWithLock(fmt.Sprintf("%s metrics_reset", now.UTC().Format("15:04:05.000Z")))
		return
	}
	parts := make([]string, 0, 16)
	appendDelta := func(name string, before, after int64) {
		if before == after {
			return
		}
		parts = append(parts, fmt.Sprintf("%s=%+d", name, after-before))
	}
	appendDelta("exit_loss", previous.ExitLossEvents, metrics.ExitLossEvents)
	appendDelta("lost", previous.FlowsLostToExit, metrics.FlowsLostToExit)
	appendDelta("recovery", previous.RecoveryCount, metrics.RecoveryCount)
	appendDelta("missed", previous.RecoveryMissed, metrics.RecoveryMissed)
	appendDelta("dial", previous.DialFailuresIntercepted, metrics.DialFailuresIntercepted)
	appendDelta("reraced", previous.FlowsReraced, metrics.FlowsReraced)
	appendDelta("qreset", previous.QuarantineTcpResets, metrics.QuarantineTcpResets)
	appendDelta("qaffinity", previous.QuarantineAffinityInvalidations, metrics.QuarantineAffinityInvalidations)
	appendDelta("sticky", previous.StickyFlowsRetired, metrics.StickyFlowsRetired)
	appendDelta("held_uplink", previous.VerdictsHeldUplinkStale, metrics.VerdictsHeldUplinkStale)
	appendDelta("held_transport", previous.VerdictsHeldTransportDown, metrics.VerdictsHeldTransportDown)
	appendDelta("held_shared", previous.VerdictsHeldSharedFate, metrics.VerdictsHeldSharedFate)
	appendDelta("deferred", previous.RemovalsDeferred, metrics.RemovalsDeferred)
	appendDelta("probe", previous.ProbesSent, metrics.ProbesSent)
	appendDelta("probe_ok", previous.ProbesAnswered, metrics.ProbesAnswered)
	appendDelta("busy_probe", previous.BusyProbesSent, metrics.BusyProbesSent)
	appendDelta("busy_ok", previous.BusyProbesAcquitted, metrics.BusyProbesAcquitted)
	appendDelta("pause", previous.SchedulerPausesDetected, metrics.SchedulerPausesDetected)
	if previous.RecoveryPending != metrics.RecoveryPending {
		parts = append(parts, fmt.Sprintf("pending=%d", metrics.RecoveryPending))
	}
	if len(parts) == 0 {
		return
	}
	self.appendCausalWithLock(fmt.Sprintf("%s %s", now.UTC().Format("15:04:05.000Z"), strings.Join(parts, ",")))
}

// reliabilityMetricsRegressed compares only cumulative counters. Gauges such
// as RecoveryPending and derived means legitimately decrease during recovery.
func reliabilityMetricsRegressed(before, after sdk.ReliabilityMetrics) bool {
	beforeCounters := [...]int64{
		before.FlowsOpened,
		before.ExitLossEvents,
		before.FlowsLostToExit,
		before.MaxFlowsLostInOneEvent,
		before.RecoveryCount,
		before.RecoveryMissed,
		before.DialFailuresIntercepted,
		before.FlowsReraced,
		before.FlowsRebound,
		before.RebindsAccepted,
		before.RebindsRedialed,
		before.VerdictsHeldUplinkStale,
		before.VerdictsHeldTransportDown,
		before.VerdictsHeldSharedFate,
		before.RemovalsDeferred,
		before.ProbesSent,
		before.ProbesAnswered,
		before.ProvidersQualified,
		before.BusyProbesSent,
		before.BusyProbesAcquitted,
		before.SchedulerPausesDetected,
		before.GroupsFollowed,
		before.GroupsScattered,
		before.QuarantineTcpResets,
		before.QuarantineAffinityInvalidations,
		before.StickyFlowsRetired,
		before.AffinityPerformanceSamples,
		before.AffinityPerformanceDonorBypasses,
		before.AffinityPerformanceCandidatesFiltered,
	}
	afterCounters := [...]int64{
		after.FlowsOpened,
		after.ExitLossEvents,
		after.FlowsLostToExit,
		after.MaxFlowsLostInOneEvent,
		after.RecoveryCount,
		after.RecoveryMissed,
		after.DialFailuresIntercepted,
		after.FlowsReraced,
		after.FlowsRebound,
		after.RebindsAccepted,
		after.RebindsRedialed,
		after.VerdictsHeldUplinkStale,
		after.VerdictsHeldTransportDown,
		after.VerdictsHeldSharedFate,
		after.RemovalsDeferred,
		after.ProbesSent,
		after.ProbesAnswered,
		after.ProvidersQualified,
		after.BusyProbesSent,
		after.BusyProbesAcquitted,
		after.SchedulerPausesDetected,
		after.GroupsFollowed,
		after.GroupsScattered,
		after.QuarantineTcpResets,
		after.QuarantineAffinityInvalidations,
		after.StickyFlowsRetired,
		after.AffinityPerformanceSamples,
		after.AffinityPerformanceDonorBypasses,
		after.AffinityPerformanceCandidatesFiltered,
	}
	for i := range beforeCounters {
		if afterCounters[i] < beforeCounters[i] {
			return true
		}
	}
	return false
}

// appendCausalWithLock keeps causal history bounded. The caller holds
// stateLock so a reconnect boundary cannot interleave with a real event.
func (self *sdkHostedDeviceTracker) appendCausalWithLock(event string) {
	self.causalHistory = append(self.causalHistory, event)
	if hostedDeviceCausalHistorySize < len(self.causalHistory) {
		self.causalHistory = append(
			[]string(nil),
			self.causalHistory[len(self.causalHistory)-hostedDeviceCausalHistorySize:]...,
		)
	}
}

// recordExitTransitions preserves the provider state change adjacent to a
// reset or exit-loss counter. Provider ids remain in the private alias map;
// artifacts contain only pN aliases and compact causes. Flow-count-only churn
// is deliberately ignored because every healthy request changes it.
func (self *sdkHostedDeviceTracker) recordExitTransitions(now time.Time, exits *sdk.ExitList) {
	if exits == nil || exits.Len() == 0 {
		// DeviceRemote briefly reports an empty list while its rpc generation
		// reconnects. Treat that as unavailable, not removal of every exit.
		return
	}

	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	next := make(map[string]hostedDeviceExitState, exits.Len())
	events := []string{}
	for i := 0; i < exits.Len(); i++ {
		exit := exits.Get(i)
		if exit == nil || exit.ClientId == nil {
			continue
		}
		providerID := exit.ClientId.String()
		alias := self.aliases[providerID]
		if alias == "" {
			alias = "unknown"
		}
		state := hostedDeviceExitState{
			alias:       alias,
			flowCount:   exit.FlowCount,
			warning:     exit.Warning,
			quarantined: exit.Quarantined,
			done:        exit.Done,
			cause:       compactDiagnosticToken(exit.WarningCause),
		}
		next[providerID] = state
		previous, found := self.lastExitState[providerID]
		if !found || (previous.warning == state.warning &&
			previous.quarantined == state.quarantined &&
			previous.done == state.done &&
			previous.cause == state.cause) {
			continue
		}
		events = append(events, fmt.Sprintf(
			"exit=%s warning=%t quarantine=%t done=%t cause=%s flows=%d",
			state.alias,
			state.warning,
			state.quarantined,
			state.done,
			state.cause,
			state.flowCount,
		))
	}
	for providerID, previous := range self.lastExitState {
		if _, found := next[providerID]; found ||
			(previous.flowCount == 0 && !previous.warning && !previous.quarantined && !previous.done) {
			continue
		}
		events = append(events, fmt.Sprintf(
			"exit=%s removed flows=%d warning=%t quarantine=%t done=%t cause=%s",
			previous.alias,
			previous.flowCount,
			previous.warning,
			previous.quarantined,
			previous.done,
			previous.cause,
		))
	}
	self.lastExitState = next
	sort.Strings(events)
	stamp := now.UTC().Format("15:04:05.000Z")
	for _, event := range events {
		self.appendCausalWithLock(stamp + " " + event)
	}
}

// summary retains the most recent remote-ingress transition across quiet
// polls. Acceptance failures are formatted many seconds after the missing
// response; a plain per-poll delta would have returned to zero and erased the
// evidence needed to join the DeviceLocal boundary to the origin LB log.
func (self *hostedDevicePacketState) summary(now time.Time, stats *sdk.PacketStats) string {
	if stats == nil {
		return "unavailable"
	}
	if self.initialized {
		packetDelta := stats.RemoteIngressPacketCount - self.lastStats.RemoteIngressPacketCount
		byteDelta := stats.RemoteIngressByteCount - self.lastStats.RemoteIngressByteCount
		if 0 < packetDelta && 0 <= byteDelta {
			self.lastRemoteIngressAt = now
			self.lastRemoteIngressN = packetDelta
			self.lastRemoteIngressB = byteDelta
		} else if packetDelta < 0 || byteDelta < 0 {
			// A DeviceLocal generation reset makes the cumulative counters move
			// backward. Do not mislabel its existing totals as fresh response
			// traffic; subsequent positive deltas establish a new timestamp.
			self.lastRemoteIngressAt = time.Time{}
			self.lastRemoteIngressN = 0
			self.lastRemoteIngressB = 0
		}
	} else {
		self.initialized = true
	}
	self.lastStats = *stats

	lastIngress := "none"
	if !self.lastRemoteIngressAt.IsZero() {
		lastIngress = fmt.Sprintf(
			"%s(+%d/%dB)",
			self.lastRemoteIngressAt.UTC().Format("15:04:05.000Z"),
			self.lastRemoteIngressN,
			self.lastRemoteIngressB,
		)
	}
	return fmt.Sprintf(
		"out=%d/%dB in=%d/%dB last_in=%s",
		stats.RemoteEgressPacketCount,
		stats.RemoteEgressByteCount,
		stats.RemoteIngressPacketCount,
		stats.RemoteIngressByteCount,
		lastIngress,
	)
}

func compactDiagnosticToken(value string) string {
	if value == "" {
		return "none"
	}
	return strings.Join(strings.Fields(value), "_")
}

// record stores only state transitions; repeated steady-state polling cannot
// evict the moment a flow first selected its provider.
func (self *sdkHostedDeviceTracker) record(now time.Time, signature string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if signature == self.lastSignature {
		return
	}
	self.lastSignature = signature
	self.history = append(self.history, fmt.Sprintf("%s %s", now.UTC().Format("15:04:05.000Z"), signature))
	if hostedDeviceHistorySize < len(self.history) {
		self.history = append([]string(nil), self.history[len(self.history)-hostedDeviceHistorySize:]...)
	}
}

// Diagnostic prefers the latest snapshot with a live flow. Cancellation can
// remove that flow before the HTTP error is formatted, and the bounded result
// detail must retain packet tracing as well as this control-plane evidence.
func (self *sdkHostedDeviceTracker) Diagnostic() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if len(self.history) == 0 {
		return "unavailable"
	}
	diagnostic := self.history[len(self.history)-1]
	for i := len(self.history) - 1; 0 <= i; i-- {
		event := self.history[i]
		if (strings.Contains(event, "destinations=[") && !strings.Contains(event, "destinations=[]")) ||
			(strings.Contains(event, "active=[") && !strings.Contains(event, "active=[]")) {
			diagnostic = self.history[i]
			break
		}
	}
	causal := "none"
	if 0 < len(self.causalHistory) {
		const causalEventCount = 6
		start := max(0, len(self.causalHistory)-causalEventCount)
		causal = strings.Join(self.causalHistory[start:], " | ")
	}
	diagnostic = "events=[" + causal + "] state={" + diagnostic + "}"
	if hostedDeviceDiagnosticMaxSize < len(diagnostic) {
		diagnostic = diagnostic[:hostedDeviceDiagnosticMaxSize] + "..."
	}
	return diagnostic
}

func (self *sdkHostedDeviceTracker) Close() {
	self.closeOnce.Do(func() {
		self.remote.Close()
		self.cancel()
	})
}

func withHostedDeviceDiagnostics(err error, tracker hostedDeviceTracker) error {
	if err == nil || tracker == nil {
		return err
	}
	return fmt.Errorf("%w; hosted device timeline: %s", err, tracker.Diagnostic())
}
