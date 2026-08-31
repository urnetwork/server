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
	lastSignature string
	closeOnce     sync.Once
	packetState   hostedDevicePacketState
}

type hostedDevicePacketState struct {
	initialized         bool
	lastStats           sdk.PacketStats
	lastRemoteIngressAt time.Time
	lastRemoteIngressN  int64
	lastRemoteIngressB  sdk.ByteCount
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
	return formatHostedDeviceState(
		self.remote.GetWindowStatus(),
		self.remote.GetReliabilityMetrics(),
		self.packetState.summary(now, self.remote.GetPacketStats()),
		self.remote.GetExits(),
		self.remote.GetDestinationExits(),
		self.aliases,
	)
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
			"flows=%d exit_loss=%d lost=%d recovery=%d/%d pending=%d dial=%d reraced=%d held=%d deferred=%d",
			metrics.FlowsOpened,
			metrics.ExitLossEvents,
			metrics.FlowsLostToExit,
			metrics.RecoveryCount,
			metrics.RecoveryMissed,
			metrics.RecoveryPending,
			metrics.DialFailuresIntercepted,
			metrics.FlowsReraced,
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
	// Public API targets sort ahead of resolver traffic in descending textual
	// order. That keeps the destination/provider mapping which carried the
	// failed acceptance request ahead of the bounded diagnostic suffix.
	sort.Sort(sort.Reverse(sort.StringSlice(destinationSummaries)))
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
	// Keep the destination-to-provider join immediately after the compact
	// aggregate state. Packet totals and the number of exits are useful, but they
	// cannot identify the carrier that lost one already-established flow. In a
	// busy production device those fields previously consumed the bounded
	// diagnostic before `destinations` and `active`, erasing the exact evidence
	// the tracker exists to retain.
	return fmt.Sprintf(
		"remote=connected window={%s} reliability={%s} destinations=[%s] active=[%s] packets={%s} exits=%d",
		windowSummary,
		metricsSummary,
		strings.Join(destinationSummaries, ","),
		strings.Join(activeExitSummaries, ","),
		packetSummary,
		totalExits,
	)
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
