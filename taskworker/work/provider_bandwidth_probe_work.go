// The active provider bandwidth probe: a provider's throughput measured
// through its tunnel against a public CDN's speed test (qualityprobe/bandwidth),
// for the per-region capacity picture a URL turn's small sample cannot give.
//
// The URL probe is sampled-only (FP2FIX.md; af17d1d2d): a quality tunnel
// carries randomized destination-pool requests and nothing a modified
// provider could recognize as a probe and serve better. A bandwidth sample is
// recognizable by its fixed targets, so it is kept apart from everything the
// quality probes are judged by:
//
//   - each pass runs in a prober shard of its own: its own registry slot, and
//     a private network, credential parent and balance minted for the pass,
//     never a URL or capability shard's; every tunnel then mints a derived
//     client of its own;
//   - its tunnels carry none of the URL probe's client traits (data-only
//     admission without provider pings, unpublished client keys), so a
//     provider that recognizes a bandwidth tunnel learns nothing that marks a
//     URL tunnel;
//   - its providers come from a random draw of the connected fleet, not the
//     URL probe's due claims, so its timing follows no URL turn;
//   - its figures are advisory: provider_bandwidth ranks and selects nothing
//     (model/provider_bandwidth_readers_test.go);
//   - its tunnels carry no credential: the CDN is its only target, and the
//     operator's download endpoint, which authorizes with the operator ingest
//     secret, is never sampled (providerBandwidthProbeTargets).
//
// Every reservation is the deployment-wide budget's
// (model.ReserveProviderBandwidthSlot, provider_bandwidth.yml): a pass takes
// its share of the hour's budget and stops at the first refusal.
package work

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/qualityprobe/bandwidth"
	"github.com/urnetwork/server/qualityprobe/ingest"
	"github.com/urnetwork/server/qualityprobe/providertunnel"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// The prober shard slot the bandwidth probe owns: the registry's second to
// last. The URL probe's shards take [0, shard_count), and its settings and
// arguments refuse a shard_count past maxProviderUrlProbeShardCount, which
// stops before this slot; the capability probe owns the last.
const (
	providerBandwidthProbeShardIndex = maxProviderEgressProbeShardCount - 2
	providerBandwidthProbeShardCount = maxProviderEgressProbeShardCount
)

// Every tunable of the probe. The defaults constructor is their one source;
// the URL probe's settings file supplies the endpoints and the bandwidth keys
// it has always carried (full.bandwidth, full.bandwidth_timeout_seconds,
// bandwidth_cdn_url), so a deployment configures nothing new and the probe
// runs by default. public_api_url is not read: the operator target is never
// sampled (providerBandwidthProbeTargets).
type providerBandwidthProbeSettings struct {
	// full.bandwidth, true unless the URL probe's settings switch it off
	Enabled bool
	// one pass every PassInterval; its task row lives at most MaxTime
	PassInterval time.Duration
	MaxTime      time.Duration
	// the most providers one pass samples. The pass's share of the hour's
	// budget (providerBandwidthProbePassLimit) usually binds first
	MaxLimit int
	// tunnels open at once
	Concurrency int
	// providers drawn per sampled provider wanted, before the due and Online
	// filters thin the draw
	DrawFactor int
	// a provider's CDN figure is refreshed after this age
	RefreshAge time.Duration
	// the per-target measurement cap (full.bandwidth_timeout_seconds)
	SampleTimeout time.Duration
	// a tunnel's setup bound, its requests' client timeout, and its teardown
	TunnelOpenTimeout  time.Duration
	RequestTimeout     time.Duration
	TunnelCloseTimeout time.Duration
	RetryDelay         time.Duration
	// endpoints: the control plane, and the CDN target
	ApiUrl      string
	PlatformUrl string
	CdnUrl      string
	// how a pass reaches the operator ingest (operator_transport)
	OperatorTransport   string
	OperatorConcurrency int
}

func defaultProviderBandwidthProbeSettings() providerBandwidthProbeSettings {
	return providerBandwidthProbeSettings{
		Enabled:            true,
		PassInterval:       15 * time.Minute,
		MaxTime:            10 * time.Minute,
		MaxLimit:           32,
		Concurrency:        2,
		DrawFactor:         8,
		RefreshAge:         7 * 24 * time.Hour,
		SampleTimeout:      bandwidth.DefaultTimeout,
		TunnelOpenTimeout:  60 * time.Second,
		RequestTimeout:     30 * time.Second,
		TunnelCloseTimeout: 30 * time.Second,
		RetryDelay:         time.Minute,
	}
}

// The probe's settings with the URL probe's deployment settings applied.
func providerBandwidthProbeSettingsFrom(egress providerEgressProbeSettings) providerBandwidthProbeSettings {
	settings := defaultProviderBandwidthProbeSettings()
	settings.Enabled = egress.Enabled && egress.Full.Bandwidth
	if 0 < egress.Full.BandwidthTimeoutSeconds {
		settings.SampleTimeout = time.Duration(egress.Full.BandwidthTimeoutSeconds) * time.Second
	}
	settings.ApiUrl = egress.APIURL
	settings.PlatformUrl = egress.PlatformURL
	settings.CdnUrl = egress.BandwidthCDNURL
	settings.OperatorTransport = egress.OperatorTransport
	settings.OperatorConcurrency = egress.OperatorConcurrency
	return settings
}

func (self providerBandwidthProbeSettings) validate() error {
	if !self.Enabled {
		return nil
	}
	if self.PassInterval <= 0 || self.MaxTime <= 0 || self.PassInterval < self.MaxTime || 24*time.Hour < self.MaxTime {
		return fmt.Errorf("bandwidth probe pass interval %s and max time %s must be positive, the max time within the interval and a day", self.PassInterval, self.MaxTime)
	}
	if self.MaxLimit < 1 || self.Concurrency < 1 || self.DrawFactor < 1 {
		return fmt.Errorf("bandwidth probe limit, concurrency and draw factor must be positive")
	}
	if self.RefreshAge <= 0 || self.SampleTimeout <= 0 || self.TunnelOpenTimeout <= 0 || self.RequestTimeout < self.SampleTimeout || self.TunnelCloseTimeout <= 0 || self.RetryDelay <= 0 {
		return fmt.Errorf("bandwidth probe timeouts must be positive, and the request timeout at least the sample timeout")
	}
	if strings.TrimSpace(self.ApiUrl) == "" || strings.TrimSpace(self.PlatformUrl) == "" {
		return fmt.Errorf("bandwidth probe api_url and platform_url are required")
	}
	if targetCount := len(providerBandwidthProbeTargets(self)); self.MaxTime < self.turnBudget(targetCount) {
		return fmt.Errorf("bandwidth probe max time %s cannot hold one provider's turn (%s)", self.MaxTime, self.turnBudget(targetCount))
	}
	return nil
}

// The longest one provider's turn takes: its tunnel's setup, every target's
// measurement with its reservation and submission, and the teardown.
func (self providerBandwidthProbeSettings) turnBudget(targetCount int) time.Duration {
	return self.TunnelOpenTimeout + time.Duration(targetCount)*(self.SampleTimeout+2*providerEgressControlPlaneTimeout) + self.TunnelCloseTimeout
}

// The budget of a pass scheduled with scheduledMaxTimeSeconds: that, when it
// was scheduled with one, or the settings' own.
func (self providerBandwidthProbeSettings) passMaxTime(scheduledMaxTimeSeconds int) time.Duration {
	if 0 < scheduledMaxTimeSeconds && time.Duration(scheduledMaxTimeSeconds)*time.Second <= 24*time.Hour {
		return time.Duration(scheduledMaxTimeSeconds) * time.Second
	}
	return self.MaxTime
}

func loadProviderBandwidthProbeSettings() (providerBandwidthProbeSettings, error) {
	egress, err := getProviderEgressProbeSettings()
	if err != nil {
		return providerBandwidthProbeSettings{}, err
	}
	settings := providerBandwidthProbeSettingsFrom(egress)
	if err := settings.validate(); err != nil {
		return providerBandwidthProbeSettings{}, err
	}
	return settings, nil
}

// Package-level seams like the capability probe's: immutable in production,
// replaced by tests that must not open tunnels or read the deployment.
var getProviderBandwidthProbeSettings = loadProviderBandwidthProbeSettings
var executeProviderBandwidthProbe = runProviderBandwidthProbe
var openBandwidthProviderTunnel = providertunnel.Open
var getProviderBandwidthProbeSample = model.GetProviderBandwidthProbeSample
var filterProviderBandwidthProbeEligible = func(ctx context.Context, clientIds []server.Id) []server.Id {
	// the Online population, as the score export decides it: the providers a
	// client can be given, which is the population a capacity figure describes
	return model.FilterProviderCapabilityProbeEligible(ctx, clientIds).ClientIds
}

// The deployment's budget in reservations an hour (provider_bandwidth.yml
// probes_per_bucket, or its default).
var getProviderBandwidthProbesPerHour = func() int64 {
	return model.MaxProviderBandwidthBytesPerBucket() / model.MaxProviderBandwidthBytesPerProbe
}

// The most providers one pass samples: its share of the hour's budget, each
// provider taking one reservation per target, never under one provider and
// never over the settings' cap.
func providerBandwidthProbePassLimit(settings providerBandwidthProbeSettings, probesPerHour int64, targetCount int) int {
	if targetCount < 1 || probesPerHour < 1 {
		return 0
	}
	passesPerHour := max(1, int64(time.Hour/settings.PassInterval))
	limit := probesPerHour / (passesPerHour * int64(targetCount))
	return int(min(max(1, limit), int64(settings.MaxLimit)))
}

// The targets of a pass: the public CDN alone, which carries no credential.
//
// The operator's own download endpoint (bandwidth.OperatorTarget) is never
// sampled. Its route authorizes with X-UR-Operator-Secret, the operator ingest
// secret that authorizes every operator ingest call, and a sample sends its
// request through the provider's tunnel. TLS would keep the header from the
// provider, but that secret has no business on a provider-controlled path.
// Sampling the operator target needs a secret scoped to
// /network/provider-bandwidth-test alone, which that route does not accept
// today (docs/operator/prober-taskworker.md).
func providerBandwidthProbeTargets(settings providerBandwidthProbeSettings) []bandwidth.Target {
	cdn := bandwidth.CdnTarget()
	if strings.TrimSpace(settings.CdnUrl) != "" {
		cdn.Url = settings.CdnUrl
	}
	return []bandwidth.Target{cdn}
}

// The bandwidth probe's slot is free of the URL probe's geometry.
func providerBandwidthProbeShardAvailable(urlProbeShardCount int) error {
	if providerBandwidthProbeShardIndex < urlProbeShardCount {
		return fmt.Errorf("the URL probe's shard_count %d reaches the bandwidth probe's prober shard %d", urlProbeShardCount, providerBandwidthProbeShardIndex)
	}
	return nil
}

// The private balance of one pass's shard: the standard contract ceiling of
// every tunnel the pass can open (each provider of its limit and a tunnel per
// concurrent slot), origin and companion both, every renewal stage, ten times
// over as the URL probe's funding is, and never under the shared shard
// headroom. This is accounting credit in the shard's own network, not a
// traffic quota; the deployment's byte budget bounds what a sample moves.
func providerBandwidthProbeShardCredit(settings providerBandwidthProbeSettings, limit int) (model.ByteCount, error) {
	contracts := connect.DefaultContractManagerSettings()
	if contracts.ContractTransferByteSeqScale > uint64(math.MaxInt64-3) {
		return 0, fmt.Errorf("bandwidth probe credit geometry overflows")
	}
	amount := int64(contracts.StandardContractTransferByteCount)
	for _, factor := range []int64{
		int64(max(1, limit)) + int64(settings.Concurrency),
		2, // origin and companion both charge the private payer
		int64(contracts.ContractTransferByteSeqScale) + 3,
		providerUrlProbeFundingMultiplier,
	} {
		if amount < 1 || factor < 1 || amount > math.MaxInt64/factor {
			return 0, fmt.Errorf("bandwidth probe credit geometry overflows")
		}
		amount *= factor
	}
	return model.ByteCount(max(amount, int64(model.ProberShardTransferHeadroom))), nil
}

// What a pass's task row carries: the max time it was scheduled with, the
// pass's whole budget; zero for a row scheduled without one.
type ProviderBandwidthProbeArgs struct {
	MaxTimeSeconds int `json:"max_time_seconds,omitempty"`
}

// What one pass did.
type ProviderBandwidthProbeResult struct {
	// the settings switched the probe off: the pass ran nothing, and no
	// successor is scheduled
	Disabled bool `json:"disabled,omitempty"`
	// providers drawn, of them due, of those Online, and the pass's limit
	Drawn    int `json:"drawn"`
	Due      int `json:"due"`
	Eligible int `json:"eligible"`
	Limit    int `json:"limit"`
	// providers whose turn began, whose tunnel opened, and with a figure
	Started      int `json:"started"`
	TunnelFailed int `json:"tunnel_failed"`
	Measured     int `json:"measured"`
	// a reservation was refused: the hour's budget is spent
	BudgetSpent bool `json:"budget_spent,omitempty"`
}

func providerBandwidthProbeRunOnce() *task.RunOnceOption {
	return task.RunOnce("provider_bandwidth_probe")
}

// At a taskworker's start, ensures the probe's task row while the URL probe's
// settings leave full.bandwidth on (the default), due at once. A row that
// exists keeps its arguments and run time. Settings that cannot be read
// schedule nothing, and are logged.
func ScheduleProviderBandwidthProbe(clientSession *session.ClientSession, tx server.PgTx) {
	settings, err := getProviderBandwidthProbeSettings()
	if err != nil {
		glog.Errorf("[bwprobe]not scheduled: settings cannot be used: %s\n", err)
		return
	}
	if !settings.Enabled {
		return
	}
	scheduleProviderBandwidthProbeAt(clientSession, tx, server.NowUtc(), settings)
}

// Schedules a pass due at once when the probe's chain has no task row, and
// reports whether it did; the taskworker calls this every few minutes, so a
// chain a stale taskworker or maintenance ended returns.
func EnsureProviderBandwidthProbe(ctx context.Context) bool {
	settings, err := getProviderBandwidthProbeSettings()
	if err != nil {
		glog.Errorf("[bwprobe]chain not checked: settings cannot be used: %s\n", err)
		return false
	}
	if !settings.Enabled {
		return false
	}
	clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
	defer clientSession.Cancel()
	scheduled, _ := task.ScheduleTaskIfAbsent(
		ProviderBandwidthProbe,
		providerBandwidthProbeScheduledArgs(settings),
		clientSession,
		providerBandwidthProbeRunOnce(),
		task.RunAt(server.NowUtc()),
		task.MaxTime(settings.MaxTime),
	)
	if scheduled {
		glog.Infof("[bwprobe]the probe's chain had no task row; scheduled a pass due at once\n")
	}
	return scheduled
}

func providerBandwidthProbeScheduledArgs(settings providerBandwidthProbeSettings) *ProviderBandwidthProbeArgs {
	return &ProviderBandwidthProbeArgs{MaxTimeSeconds: int(settings.MaxTime / time.Second)}
}

func scheduleProviderBandwidthProbeAt(clientSession *session.ClientSession, tx server.PgTx, runAt time.Time, settings providerBandwidthProbeSettings) {
	task.ScheduleTaskInTx(
		tx,
		ProviderBandwidthProbe,
		providerBandwidthProbeScheduledArgs(settings),
		clientSession,
		providerBandwidthProbeRunOnce(),
		task.RunAt(runAt),
		task.MaxTime(settings.MaxTime),
	)
}

// One pass. Settings are read at every pass, so a change applies at the next
// one; a switched-off probe runs nothing, and its chain ends there. The pass's
// budget is the max time its row was scheduled with. A failed pass is retried
// after the retry delay.
func ProviderBandwidthProbe(
	args *ProviderBandwidthProbeArgs,
	clientSession *session.ClientSession,
) (*ProviderBandwidthProbeResult, error) {
	settings, err := getProviderBandwidthProbeSettings()
	if err != nil {
		providerBandwidthProbePassesTotal.WithLabelValues("failed").Inc()
		return nil, task.WithRetryDelay(fmt.Errorf("bandwidth probe settings: %w", err), defaultProviderBandwidthProbeSettings().RetryDelay)
	}
	if !settings.Enabled {
		providerBandwidthProbePassesTotal.WithLabelValues("disabled").Inc()
		return &ProviderBandwidthProbeResult{Disabled: true}, nil
	}
	scheduledMaxTimeSeconds := 0
	if args != nil {
		scheduledMaxTimeSeconds = args.MaxTimeSeconds
	}
	maxTime := settings.passMaxTime(scheduledMaxTimeSeconds)
	// Task MaxTime cancels the session with a timer but sets no deadline; the
	// pass's admission needs the deadline to finish what it starts.
	probeCtx, cancel := context.WithTimeout(clientSession.Ctx, maxTime)
	defer cancel()
	result, err := executeProviderBandwidthProbe(probeCtx, settings, maxTime)
	if err != nil {
		providerBandwidthProbePassesTotal.WithLabelValues("failed").Inc()
		return nil, task.WithRetryDelay(err, settings.RetryDelay)
	}
	if result.Started == 0 {
		providerBandwidthProbePassesTotal.WithLabelValues("empty").Inc()
	} else {
		providerBandwidthProbePassesTotal.WithLabelValues("measured").Inc()
	}
	return result, nil
}

// Schedules the next pass after the pass interval. A switched-off probe's pass
// schedules nothing, which ends the chain; a taskworker that reads the probe
// as on schedules it again (ScheduleProviderBandwidthProbe and
// EnsureProviderBandwidthProbe).
func ProviderBandwidthProbePost(
	args *ProviderBandwidthProbeArgs,
	result *ProviderBandwidthProbeResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	settings, err := getProviderBandwidthProbeSettings()
	if err != nil {
		settings = defaultProviderBandwidthProbeSettings()
	}
	if runAt, scheduled := providerBandwidthProbeSuccessor(result, settings, server.NowUtc()); scheduled {
		scheduleProviderBandwidthProbeAt(clientSession, tx, runAt, settings)
	}
	return nil
}

// When the next pass runs after a pass that finished at now, and false for
// none: a switched-off probe's pass ends the chain.
func providerBandwidthProbeSuccessor(result *ProviderBandwidthProbeResult, settings providerBandwidthProbeSettings, now time.Time) (time.Time, bool) {
	if result != nil && result.Disabled {
		return time.Time{}, false
	}
	return now.Add(settings.PassInterval), true
}

func NewProviderBandwidthProbeTaskTarget() *task.TaskTarget[*ProviderBandwidthProbeArgs, *ProviderBandwidthProbeResult] {
	return task.NewTaskTargetWithPost(ProviderBandwidthProbe, ProviderBandwidthProbePost)
}

// The providers one pass samples: a random draw of the connected fleet,
// thinned to those due and Online, in the draw's random order, at most limit.
type providerBandwidthProbeSelection struct {
	clientIds []server.Id
	drawn     int
	due       int
	eligible  int
}

func selectProviderBandwidthProbePass(ctx context.Context, settings providerBandwidthProbeSettings, limit int, now time.Time) (selection providerBandwidthProbeSelection, returnErr error) {
	if limit < 1 {
		return selection, nil
	}
	if failure := server.HandleError(func() {
		drawSize := min(limit*settings.DrawFactor, model.ProviderBandwidthProbeMaxSampleSize)
		candidates := getProviderBandwidthProbeSample(ctx, drawSize, now.Add(-settings.RefreshAge))
		selection.drawn = len(candidates)
		dueClientIds := []server.Id{}
		for _, candidate := range candidates {
			if candidate.Due {
				dueClientIds = append(dueClientIds, candidate.ClientId)
			}
		}
		selection.due = len(dueClientIds)
		// Online is judged in chunks a little larger than the room left, so a
		// draw that fills early reads no more evidence than it needs
		for judged := 0; judged < len(dueClientIds) && len(selection.clientIds) < limit; {
			room := limit - len(selection.clientIds)
			chunkEnd := min(len(dueClientIds), judged+max(2*room, 16))
			eligible := filterProviderBandwidthProbeEligible(ctx, dueClientIds[judged:chunkEnd])
			selection.eligible += len(eligible)
			selection.clientIds = append(selection.clientIds, eligible[:min(room, len(eligible))]...)
			judged = chunkEnd
		}
	}); failure != nil {
		return selection, fmt.Errorf("bandwidth probe selection: %v", failure)
	}
	return selection, nil
}

// One provider's tunnel for a sample: a client that dials only the targets'
// hosts through it, and its teardown.
type openProviderBandwidthTunnel func(ctx context.Context, providerId server.Id) (*http.Client, func() error, error)

// What a tunnel of this probe is, apart from the pass's identity, endpoints,
// credentials, control, placement and retirements in base. It is an ordinary
// client's tunnel: none of the URL probe's traits (the data-only admission
// without provider pings, unpublished client keys), no certificate pins, so
// the tunnel's allowlist is the targets' hosts alone, and a platform
// transport budget of its own.
func providerBandwidthTunnelConfig(base providertunnel.Config, settings providerBandwidthProbeSettings) providertunnel.Config {
	cfg := base
	cfg.DataOnlyProbe = false
	cfg.KeepClientKeysLocal = false
	cfg.ContractReservationByteCount = 0
	cfg.Pins = nil
	cfg.RedirectPins = nil
	cfg.CapabilityObservation = nil
	cfg.CloseTimeout = settings.TunnelCloseTimeout
	limits := connect.DefaultPlatformTransportSettings().PlatformTransportBudget.Stats()
	cfg.PlatformTransportBudget = connect.NewPlatformTransportBudget(limits.TotalByteCount, limits.MaxTransportCount)
	return cfg
}

// Opens this probe's tunnels from base, each reaching hosts alone.
func newProviderBandwidthTunnelOpener(base providertunnel.Config, hosts []string, settings providerBandwidthProbeSettings) openProviderBandwidthTunnel {
	return func(ctx context.Context, providerId server.Id) (*http.Client, func() error, error) {
		tunnel, err := openBandwidthProviderTunnel(ctx, providerBandwidthTunnelConfig(base, settings), connect.Id(providerId))
		if err != nil {
			return nil, nil, err
		}
		return tunnel.HttpClientForHosts(settings.RequestTimeout, hosts), tunnel.Close, nil
	}
}

// What a pass measures with: bandwidth.Sampler in production.
type providerBandwidthSampler interface {
	Sample(ctx context.Context, providerClientId string, client *http.Client) []bandwidth.Result
}

// Samples the selection's providers, at most concurrency at a time, each in a
// tunnel of its own that closes before its slot frees. No turn starts once a
// reservation was refused (the hour's budget is spent) or the pass's deadline
// cannot hold one more turn.
func measureProviderBandwidthProbePass(
	ctx context.Context,
	settings providerBandwidthProbeSettings,
	clientIds []server.Id,
	targetCount int,
	open openProviderBandwidthTunnel,
	sampler providerBandwidthSampler,
) *ProviderBandwidthProbeResult {
	result := &ProviderBandwidthProbeResult{}
	var stateLock sync.Mutex
	budgetSpent := func() bool {
		stateLock.Lock()
		defer stateLock.Unlock()
		return result.BudgetSpent
	}
	hasTime := func() bool {
		if ctx.Err() != nil {
			return false
		}
		deadline, ok := ctx.Deadline()
		return !ok || settings.turnBudget(targetCount) <= time.Until(deadline)
	}
	slots := make(chan struct{}, settings.Concurrency)
	var turns sync.WaitGroup
	for _, clientId := range clientIds {
		if budgetSpent() || !hasTime() {
			break
		}
		slots <- struct{}{}
		// a turn that finished while this one waited for its slot may have
		// found the budget spent
		if budgetSpent() || !hasTime() {
			<-slots
			break
		}
		stateLock.Lock()
		result.Started += 1
		stateLock.Unlock()
		turns.Add(1)
		go func() {
			defer turns.Done()
			defer func() { <-slots }()
			opened, results := measureProviderBandwidthTurn(ctx, settings, targetCount, clientId, open, sampler)
			stateLock.Lock()
			defer stateLock.Unlock()
			if !opened {
				result.TunnelFailed += 1
				return
			}
			measured := false
			for _, sample := range results {
				providerBandwidthProbeSamplesTotal.WithLabelValues(sample.Target.Source, providerBandwidthSampleResult(sample)).Inc()
				measured = measured || sample.Measured()
				if sample.Skip == bandwidth.SkipNoBudget {
					result.BudgetSpent = true
				}
			}
			if measured {
				result.Measured += 1
			}
		}()
	}
	turns.Wait()
	return result
}

// One provider's turn, bounded by its own budget: open the tunnel, sample
// every target, close it.
func measureProviderBandwidthTurn(
	ctx context.Context,
	settings providerBandwidthProbeSettings,
	targetCount int,
	clientId server.Id,
	open openProviderBandwidthTunnel,
	sampler providerBandwidthSampler,
) (opened bool, results []bandwidth.Result) {
	turnCtx, cancel := context.WithTimeout(ctx, settings.turnBudget(targetCount))
	defer cancel()
	openCtx, cancelOpen := context.WithTimeout(turnCtx, settings.TunnelOpenTimeout)
	client, closeTunnel, err := open(openCtx, clientId)
	cancelOpen()
	if err != nil {
		providerBandwidthProbeSamplesTotal.WithLabelValues("tunnel", "failed").Inc()
		glog.V(1).Infof("[bwprobe]provider=%s tunnel did not open: %s\n", clientId, err)
		return false, nil
	}
	defer func() {
		if err := closeTunnel(); err != nil {
			glog.V(1).Infof("[bwprobe]provider=%s tunnel close: %s\n", clientId, err)
		}
	}()
	results = sampler.Sample(turnCtx, clientId.String(), client)
	glog.Infof("[bwprobe]provider=%s %s\n", clientId, bandwidth.Summary(results))
	return true, results
}

// A sample's result label.
func providerBandwidthSampleResult(sample bandwidth.Result) string {
	switch {
	case sample.Skip == bandwidth.SkipNoBudget:
		return "no_budget"
	case sample.Skip == bandwidth.SkipNoTime:
		return "no_time"
	case sample.Skip == bandwidth.SkipUnsupported:
		return "unsupported"
	case sample.Measured():
		return "measured"
	default:
		return "failed"
	}
}

// The production pass, whose whole budget is maxTime. It draws its providers
// from the database first; a pass with nothing to sample takes no shard.
// Otherwise it samples in a fresh prober shard for this task execution
// (runProviderBandwidthProbeShard).
func runProviderBandwidthProbe(ctx context.Context, settings providerBandwidthProbeSettings, maxTime time.Duration) (*ProviderBandwidthProbeResult, error) {
	execution, ok := task.ExecutionIdentityFromContext(ctx)
	if !ok {
		return nil, errors.New("bandwidth probe requires a durable task execution owner")
	}
	egressSettings, err := getProviderEgressProbeSettings()
	if err != nil {
		return nil, fmt.Errorf("bandwidth probe control-plane endpoints: %w", err)
	}
	if err := providerBandwidthProbeShardAvailable(egressSettings.ShardCount); err != nil {
		return nil, err
	}
	targets := providerBandwidthProbeTargets(settings)
	limit := providerBandwidthProbePassLimit(settings, getProviderBandwidthProbesPerHour(), len(targets))
	selection, err := selectProviderBandwidthProbePass(ctx, settings, limit, server.NowUtc())
	if err != nil {
		return nil, err
	}
	if len(selection.clientIds) == 0 {
		return providerBandwidthProbeSelectionResult(selection, limit, &ProviderBandwidthProbeResult{}), nil
	}
	operator, err := newProviderEgressOperator(&ProviderEgressProbeArgs{
		ShardIndex: providerBandwidthProbeShardIndex,
		ShardCount: providerBandwidthProbeShardCount,
		APIURL:     settings.ApiUrl,
	}, egressSettings)
	if err != nil {
		return nil, err
	}
	credit, err := providerBandwidthProbeShardCredit(settings, limit)
	if err != nil {
		return nil, err
	}
	key := model.ProberShardKey{
		TaskId:     execution.TaskId,
		Epoch:      execution.Epoch,
		ShardIndex: providerBandwidthProbeShardIndex,
		ShardCount: providerBandwidthProbeShardCount,
	}
	measured, err := runProviderBandwidthProbeShard(ctx, key, settings, credit, maxTime, func(tunnelConfig providertunnel.Config) (*ProviderBandwidthProbeResult, error) {
		// every figure is observed, by source and the provider's egress
		// country, as it is submitted
		sink := newEgressProbeMetricsReporter(operator, lookupProviderEgressCountry)
		sampler := &bandwidth.Sampler{Targets: targets, Reserve: sink, Submit: sink, Timeout: settings.SampleTimeout}
		open := newProviderBandwidthTunnelOpener(tunnelConfig, bandwidth.TargetHosts(targets), settings)
		return measureProviderBandwidthProbePass(ctx, settings, selection.clientIds, len(targets), open, sampler), nil
	})
	var cleanupErr *proberShardCleanupError
	if errors.As(err, &cleanupErr) {
		// a finished pass stands; the prober reaper retries the cleanup
		glog.Errorf("[bwprobe]prober shard cleanup failed, left to the prober reaper: %s\n", cleanupErr)
		if measured != nil {
			err = nil
		}
	}
	if err != nil {
		return nil, err
	}
	result := providerBandwidthProbeSelectionResult(selection, limit, measured)
	glog.Infof(
		"[bwprobe]pass: drawn=%d due=%d eligible=%d limit=%d started=%d tunnel_failed=%d measured=%d budget_spent=%t\n",
		result.Drawn, result.Due, result.Eligible, result.Limit, result.Started, result.TunnelFailed, result.Measured, result.BudgetSpent,
	)
	return result, nil
}

func providerBandwidthProbeSelectionResult(selection providerBandwidthProbeSelection, limit int, result *ProviderBandwidthProbeResult) *ProviderBandwidthProbeResult {
	result.Drawn, result.Due, result.Eligible, result.Limit = selection.drawn, selection.due, selection.eligible, limit
	return result
}

// One sampling pass in the prober shard key, which lives for the pass's budget
// maxTime: the shard's private credit, its identity, derived-client
// credentials and in-process control, the funding readiness check, and then
// finish with the pass's tunnel configuration. The shard is drained and
// settled when finish returns, after every tunnel has joined.
func runProviderBandwidthProbeShard(
	ctx context.Context,
	key model.ProberShardKey,
	settings providerBandwidthProbeSettings,
	credit model.ByteCount,
	maxTime time.Duration,
	finish func(providertunnel.Config) (*ProviderBandwidthProbeResult, error),
) (*ProviderBandwidthProbeResult, error) {
	return runWithProberShard(ctx, "bandwidth probe", key, credit, maxTime, func(identity *model.ProberIdentity) (*ProviderBandwidthProbeResult, error) {
		credentials, err := newProviderEgressCredentials(identity)
		if err != nil {
			return nil, err
		}
		return runWithProviderEgressControl(ctx, credentials, func(localControl connect.NetworkClientControl) (*ProviderBandwidthProbeResult, error) {
			if err := newProviderBandwidthProbeReadiness(*identity.NetworkId).check(ctx); err != nil {
				return nil, err
			}
			placement := newProbeEdgePlacement(ctx)
			defer placement.Close()
			// closed tunnels' derived clients acknowledge late provider
			// returns here, joined before the shard drains
			retirements := providertunnel.NewRetirements(2 * settings.Concurrency)
			defer retirements.Close()
			return finish(providertunnel.Config{
				ApiUrl:            settings.ApiUrl,
				PlatformUrl:       settings.PlatformUrl,
				ByJwt:             identity.ByClientJwt,
				ClientId:          connect.Id(*identity.ClientId),
				DeviceDescription: model.ProberClientDescription,
				DeviceSpec:        model.ProberClientDeviceSpec,
				Version:           server.RequireVersion(),
				ClientCredentials: credentials,
				ClientControl:     localControl,
				PlatformPlacement: placement,
				Retirements:       retirements,
			})
		})
	})
}

// A pass's funding readiness over its shard's network: the URL probe's check,
// whose failures count in this probe's own metrics, never in the URL probe's
// pass errors.
func newProviderBandwidthProbeReadiness(networkId server.Id) *providerEgressProbeReadiness {
	readiness := newProviderEgressProbeReadiness(networkId)
	readiness.observeFailure = func(step string) {
		providerBandwidthProbeReadinessFailuresTotal.WithLabelValues(step).Inc()
	}
	return readiness
}

var providerBandwidthProbePassesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_probe",
	Name:      "bandwidth_passes_total",
	Help:      "Active bandwidth probe passes by result: measured, empty (nothing to sample), disabled, or failed",
}, []string{"result"})

var providerBandwidthProbeSamplesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_probe",
	Name:      "bandwidth_samples_total",
	Help:      "Active bandwidth target samples by source (active-operator, active-cdn) and result (measured, no_budget, no_time, unsupported, failed); source tunnel counts providers whose tunnel did not open",
}, []string{"source", "result"})

var providerBandwidthProbeReadinessFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_probe",
	Name:      "bandwidth_readiness_failures_total",
	Help:      "Active bandwidth probe passes refused by their shard's funding readiness, by step",
}, []string{"step"})

// Active bandwidth figures by the provider's egress country: a sum and a count
// per source and country, no quantiles, so a country costs two series a source.
var egressProbeBandwidthCountryBytesPerSecond = prometheus.NewSummaryVec(prometheus.SummaryOpts{
	Namespace:  "urnetwork",
	Subsystem:  "egress_probe",
	Name:       "bandwidth_country_bytes_per_second",
	Help:       "Active bandwidth samples through the provider tunnel by sample source and the provider's egress country (unknown when never located); sum and count only",
	Objectives: nil,
}, []string{"source", "country"})

// The fixed zero children are an observation contract: a deployed probe whose
// passes never measure still exports its zero counts.
func init() {
	for _, result := range []string{"measured", "empty", "disabled", "failed"} {
		providerBandwidthProbePassesTotal.WithLabelValues(result)
	}
	for _, source := range []string{bandwidth.SourceOperator, bandwidth.SourceCdn} {
		for _, result := range []string{"measured", "no_budget", "no_time", "unsupported", "failed"} {
			providerBandwidthProbeSamplesTotal.WithLabelValues(source, result)
		}
	}
	providerBandwidthProbeSamplesTotal.WithLabelValues("tunnel", "failed")
	for _, step := range []string{"funding_unavailable", "funding_unknown"} {
		providerBandwidthProbeReadinessFailuresTotal.WithLabelValues(step)
	}
	prometheus.MustRegister(
		providerBandwidthProbePassesTotal,
		providerBandwidthProbeSamplesTotal,
		providerBandwidthProbeReadinessFailuresTotal,
		egressProbeBandwidthCountryBytesPerSecond,
	)
}

var _ ingest.BandwidthRecorder = (*egressProbeMetricsReporter)(nil)
