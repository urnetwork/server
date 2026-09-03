// This file runs provider egress measurement as host-independent, durable
// taskworker shards rather than as one service assigned to each edge.
package work

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/operator-proxy/bandwidth"
	"github.com/urnetwork/operator-proxy/controlplane"
	"github.com/urnetwork/operator-proxy/egresshealth"
	"github.com/urnetwork/operator-proxy/fleetprobe"
	"github.com/urnetwork/operator-proxy/ingest"
	"github.com/urnetwork/operator-proxy/prober"
	"github.com/urnetwork/operator-proxy/providertunnel"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

const (
	defaultProviderEgressProbeShardCount = 4
	maxProviderEgressProbeShardCount     = 256
)

// ProviderEgressProbeBatchArgs is one kind of work inside a shard pass. Full
// and blackhole probes deliberately share the same recurring task; their due
// queries retain separate ages and make whichever work is currently due cheap.
type ProviderEgressProbeBatchArgs struct {
	Limit                   int  `json:"limit" yaml:"limit"`
	Concurrency             int  `json:"concurrency" yaml:"concurrency"`
	ProbeTimeoutSeconds     int  `json:"probe_timeout_seconds" yaml:"probe_timeout_seconds"`
	AllDestinations         bool `json:"all_destinations,omitempty" yaml:"all_destinations"`
	Bandwidth               bool `json:"bandwidth,omitempty" yaml:"bandwidth"`
	BandwidthTimeoutSeconds int  `json:"bandwidth_timeout_seconds,omitempty" yaml:"bandwidth_timeout_seconds"`
}

// ProviderEgressProbeArgs is the complete, durable description of one shard
// pass. The task system is the deployment unit: any taskworker can claim these
// arguments, and no edge host owns a shard.
type ProviderEgressProbeArgs struct {
	ShardIndex       int                          `json:"shard_index"`
	ShardCount       int                          `json:"shard_count"`
	IdleDelaySeconds int                          `json:"idle_delay_seconds"`
	MaxTimeSeconds   int                          `json:"max_time_seconds"`
	Full             ProviderEgressProbeBatchArgs `json:"full"`
	Blackhole        ProviderEgressProbeBatchArgs `json:"blackhole"`
	APIURL           string                       `json:"api_url"`
	PlatformURL      string                       `json:"platform_url"`
	PublicAPIURL     string                       `json:"public_api_url,omitempty"`
	BandwidthCDNURL  string                       `json:"bandwidth_cdn_url,omitempty"`
}

// ProviderEgressProbeResult records enough of the pass to drive its successor
// and diagnose whether useful work happened.
type ProviderEgressProbeResult struct {
	Full         bool `json:"full"`
	Stale        bool `json:"stale"`
	FullDue      int  `json:"full_due"`
	BlackholeDue int  `json:"blackhole_due"`
	Attempted    int  `json:"attempted"`
	Submitted    int  `json:"submitted"`
	Failed       int  `json:"failed"`
	Checked      int  `json:"checked"`
	Dark         int  `json:"dark"`
	TunnelFailed int  `json:"tunnel_failed"`
}

// Deployment settings are snapshotted into each task's durable arguments.
type providerEgressProbeSettings struct {
	Enabled          bool                         `yaml:"enabled"`
	ShardCount       int                          `yaml:"shard_count"`
	IdleDelaySeconds int                          `yaml:"idle_delay_seconds"`
	MaxTimeSeconds   int                          `yaml:"max_time_seconds"`
	APIURL           string                       `yaml:"api_url"`
	PlatformURL      string                       `yaml:"platform_url"`
	PublicAPIURL     string                       `yaml:"public_api_url"`
	BandwidthCDNURL  string                       `yaml:"bandwidth_cdn_url"`
	Full             ProviderEgressProbeBatchArgs `yaml:"full"`
	Blackhole        ProviderEgressProbeBatchArgs `yaml:"blackhole"`
}

// Built-in values keep non-main environments usable when no override exists.
func defaultProviderEgressProbeSettings(domain string) providerEgressProbeSettings {
	apiURL := "https://api." + domain
	return providerEgressProbeSettings{
		Enabled:          true,
		ShardCount:       defaultProviderEgressProbeShardCount,
		IdleDelaySeconds: 5 * 60,
		MaxTimeSeconds:   30 * 60,
		APIURL:           apiURL,
		PlatformURL:      "wss://connect." + domain,
		PublicAPIURL:     apiURL,
		BandwidthCDNURL:  bandwidth.CDNTestURL,
		Full: ProviderEgressProbeBatchArgs{
			Limit:                   8,
			Concurrency:             2,
			ProbeTimeoutSeconds:     60,
			AllDestinations:         false,
			Bandwidth:               true,
			BandwidthTimeoutSeconds: int(bandwidth.DefaultTimeout / time.Second),
		},
		Blackhole: ProviderEgressProbeBatchArgs{
			Limit:               250,
			Concurrency:         4,
			ProbeTimeoutSeconds: 15,
		},
	}
}

// Batch bounds prevent an invalid worker pool or unbounded request deadline.
func validateProviderEgressProbeBatchArgs(name string, args ProviderEgressProbeBatchArgs) error {
	if args.Limit < 1 {
		return fmt.Errorf("provider egress probe %s limit must be positive", name)
	}
	if args.Concurrency < 1 || args.Limit < args.Concurrency {
		return fmt.Errorf("provider egress probe %s concurrency must be in [1,limit]", name)
	}
	if args.ProbeTimeoutSeconds < 1 {
		return fmt.Errorf("provider egress probe %s timeout must be positive", name)
	}
	if args.Bandwidth && args.BandwidthTimeoutSeconds < 1 {
		return fmt.Errorf("provider egress probe %s bandwidth timeout must be positive when bandwidth is enabled", name)
	}
	return nil
}

// Cross-field checks cover shard geometry, deadlines, endpoints, and the cold
// tunnel floor required by the health sampler.
func validateProviderEgressProbeConfig(
	shardCount int,
	idleDelaySeconds int,
	maxTimeSeconds int,
	apiURL string,
	platformURL string,
	fullArgs ProviderEgressProbeBatchArgs,
	blackholeArgs ProviderEgressProbeBatchArgs,
) error {
	if shardCount < 1 || maxProviderEgressProbeShardCount < shardCount {
		return fmt.Errorf("provider egress probe shard_count must be in [1,%d] (got %d)", maxProviderEgressProbeShardCount, shardCount)
	}
	if idleDelaySeconds < 1 || maxTimeSeconds < 1 {
		return fmt.Errorf("provider egress probe idle delay and max time must be positive")
	}
	if strings.TrimSpace(apiURL) == "" || strings.TrimSpace(platformURL) == "" {
		return fmt.Errorf("provider egress probe api_url and platform_url are required")
	}
	if err := validateProviderEgressProbeBatchArgs("full", fullArgs); err != nil {
		return err
	}
	if err := validateProviderEgressProbeBatchArgs("blackhole", blackholeArgs); err != nil {
		return err
	}
	probeTimeout := time.Duration(fullArgs.ProbeTimeoutSeconds) * time.Second
	if options := fleetprobe.EgressHealthOptions(probeTimeout, fullArgs.AllDestinations); options.PerRequestTimeout < egresshealth.DefaultPerRequestTimeout {
		return fmt.Errorf(
			"provider egress probe full timeout %s leaves %s per health request, below the %s minimum",
			probeTimeout,
			options.PerRequestTimeout,
			egresshealth.DefaultPerRequestTimeout,
		)
	}
	return nil
}

// Every deployment-level invariant is applied to one settings snapshot.
func (self providerEgressProbeSettings) validate() error {
	if !self.Enabled {
		return nil
	}
	return validateProviderEgressProbeConfig(
		self.ShardCount,
		self.IdleDelaySeconds,
		self.MaxTimeSeconds,
		self.APIURL,
		self.PlatformURL,
		self.Full,
		self.Blackhole,
	)
}

// The optional environment resource overlays defaults and is validated before
// any task is scheduled from it.
func loadProviderEgressProbeSettings() (providerEgressProbeSettings, error) {
	domain, err := server.Domain()
	if err != nil {
		return providerEgressProbeSettings{}, err
	}
	settings := defaultProviderEgressProbeSettings(domain)
	if resource, err := server.Config.SimpleResource("provider_egress_probe.yml"); err == nil {
		if err := resource.UnmarshalYamlE(&settings); err != nil {
			return providerEgressProbeSettings{}, err
		}
	}
	if err := settings.validate(); err != nil {
		return providerEgressProbeSettings{}, err
	}
	return settings, nil
}

// Package-level seams keep scheduling and lifecycle tests deterministic. They
// are immutable in production; tests replace and restore them without running
// real provider tunnels.
var getProviderEgressProbeSettings = loadProviderEgressProbeSettings
var executeProviderEgressProbe = runProviderEgressProbe

// One task argument snapshot contains everything needed to repeat its index.
func providerEgressProbeArgs(
	settings providerEgressProbeSettings,
	shardIndex int,
) *ProviderEgressProbeArgs {
	return &ProviderEgressProbeArgs{
		ShardIndex:       shardIndex,
		ShardCount:       settings.ShardCount,
		IdleDelaySeconds: settings.IdleDelaySeconds,
		MaxTimeSeconds:   settings.MaxTimeSeconds,
		Full:             settings.Full,
		Blackhole:        settings.Blackhole,
		APIURL:           settings.APIURL,
		PlatformURL:      settings.PlatformURL,
		PublicAPIURL:     settings.PublicAPIURL,
		BandwidthCDNURL:  settings.BandwidthCDNURL,
	}
}

// Every index in the configured geometry receives one argument snapshot.
func allProviderEgressProbeArgs(settings providerEgressProbeSettings) []*ProviderEgressProbeArgs {
	args := make([]*ProviderEgressProbeArgs, 0, settings.ShardCount)
	for shardIndex := range settings.ShardCount {
		args = append(args, providerEgressProbeArgs(settings, shardIndex))
	}
	return args
}

// ScheduleProviderEgressProbeTasks ensures every configured shard has exactly
// one pending task. RunOnce makes repeated initialization idempotent.
func ScheduleProviderEgressProbeTasks(clientSession *session.ClientSession, tx server.PgTx) {
	settings, err := getProviderEgressProbeSettings()
	server.Raise(err)
	if !settings.Enabled {
		return
	}
	for _, args := range allProviderEgressProbeArgs(settings) {
		scheduleProviderEgressProbeAt(clientSession, tx, args, server.NowUtc())
	}
}

// ProviderEgressProbeTaskFunctionNames is the canonical pending-task surface
// owned by the probe subsystem. Deriving the name from the registered function
// prevents disabled cleanup from drifting away from task serialization.
func ProviderEgressProbeTaskFunctionNames() []string {
	return []string{
		task.NewTaskTarget(ProviderEgressProbe).TargetFunctionName(),
	}
}

// RemoveDisabledProviderEgressProbeTasks removes recurring rows left by an
// enabled deployment after probing is disabled. Claims are deliberately not a
// barrier: the task reaper deletes every matching row, so stale and actively
// leased generations cannot perpetuate themselves through their post hook.
func RemoveDisabledProviderEgressProbeTasks(ctx context.Context, tx server.PgTx) int64 {
	settings, err := getProviderEgressProbeSettings()
	server.Raise(err)
	if settings.Enabled {
		return 0
	}
	var removedCount int64
	for _, functionName := range ProviderEgressProbeTaskFunctionNames() {
		removedCount += task.RemovePendingTasksForFunctionInTx(ctx, tx, functionName)
	}
	return removedCount
}

// The run-once key is shard-index stable so initialization and post scheduling
// cannot create duplicate owners for one slice.
func scheduleProviderEgressProbeAt(
	clientSession *session.ClientSession,
	tx server.PgTx,
	args *ProviderEgressProbeArgs,
	runAt time.Time,
) {
	task.ScheduleTaskInTx(
		tx,
		ProviderEgressProbe,
		args,
		clientSession,
		task.RunOnce("provider_egress_probe", args.ShardIndex),
		task.RunAt(runAt),
		task.MaxTime(time.Duration(args.MaxTimeSeconds)*time.Second),
	)
}

// Persisted arguments are revalidated before any network work begins.
func validateProviderEgressProbeArgs(args *ProviderEgressProbeArgs) error {
	if args == nil {
		return fmt.Errorf("provider egress probe args are required")
	}
	if args.ShardIndex < 0 || args.ShardCount <= args.ShardIndex {
		return fmt.Errorf("provider egress probe shard %d/%d is invalid", args.ShardIndex, args.ShardCount)
	}
	return validateProviderEgressProbeConfig(
		args.ShardCount,
		args.IdleDelaySeconds,
		args.MaxTimeSeconds,
		args.APIURL,
		args.PlatformURL,
		args.Full,
		args.Blackhole,
	)
}

// ProviderEgressProbe runs one bounded shard batch. A configuration geometry
// change makes an old task a no-op; its post-step replaces it with current args.
func ProviderEgressProbe(
	args *ProviderEgressProbeArgs,
	clientSession *session.ClientSession,
) (*ProviderEgressProbeResult, error) {
	settings, err := getProviderEgressProbeSettings()
	if err != nil {
		return nil, err
	}
	// A disabled deployment may still have an old claimed row in a worker's
	// input batch. Retire it before touching its arguments or the execution
	// path, which is where identity, Vault credentials, and network clients are
	// acquired.
	if !settings.Enabled {
		return &ProviderEgressProbeResult{Stale: true}, nil
	}
	if err := validateProviderEgressProbeArgs(args); err != nil {
		return nil, err
	}
	if args.ShardCount != settings.ShardCount || settings.ShardCount <= args.ShardIndex {
		return &ProviderEgressProbeResult{Stale: true}, nil
	}
	return executeProviderEgressProbe(clientSession.Ctx, args)
}

// ProviderEgressProbePost checkpoints one batch before scheduling its
// successor. Backlog drains immediately; a partial batch waits its idle cadence.
func ProviderEgressProbePost(
	args *ProviderEgressProbeArgs,
	result *ProviderEgressProbeResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	settings, err := getProviderEgressProbeSettings()
	if err != nil {
		return err
	}
	if !settings.Enabled {
		return nil
	}
	if args.ShardIndex < 0 || settings.ShardCount <= args.ShardIndex {
		return nil
	}

	nextArgs := providerEgressProbeArgs(settings, args.ShardIndex)
	runAt := server.NowUtc()
	if !result.Stale && !result.Full {
		runAt = runAt.Add(time.Duration(nextArgs.IdleDelaySeconds) * time.Second)
	}
	scheduleProviderEgressProbeAt(clientSession, tx, nextArgs, runAt)
	return nil
}

// The ingest credential stays in Vault and is never copied into task JSON.
func readProviderEgressOperatorSecret() (string, error) {
	resource, err := server.Vault.SimpleResource("provider_egress.yml")
	if err != nil {
		return "", err
	}
	values := resource.String("ingest_secret")
	if len(values) != 1 || strings.TrimSpace(values[0]) == "" {
		return "", fmt.Errorf("provider_egress.yml must contain one non-empty ingest_secret")
	}
	return values[0], nil
}

// One immutable pass owns the API operations and bounded batch runners used by
// a shard task. The explicit boundaries make it deterministic to prove that
// the two independent probe schedules cannot starve each other when one fails.
// It is safe for concurrent use after construction.
type providerEgressProbePass struct {
	blackholeDue          func(context.Context, int) ([]string, error)
	fullDue               func(context.Context, int) ([]string, error)
	loadPins              func(context.Context) (map[string][]string, error)
	submitBlackholeChecks func(context.Context, []ingest.BlackholeCheck) error
	blackholeOptions      fleetprobe.BlackholeOptions
	fullOptions           fleetprobe.FullOptions
	runBlackhole          func(context.Context, []string, fleetprobe.BlackholeOptions) (fleetprobe.BlackholeSummary, error)
	runFull               func(context.Context, []string, fleetprobe.FullOptions) (prober.Summary, error)
}

// run executes both independently due batches with one certificate-pin
// snapshot. A failure in one batch is retained but does not suppress the other;
// task retry then revisits only work whose server-side due state remains stale.
func (self *providerEgressProbePass) run(
	ctx context.Context,
	args *ProviderEgressProbeArgs,
) (*ProviderEgressProbeResult, error) {
	errList := []error{}
	blackholeClientIds, err := self.blackholeDue(ctx, args.Blackhole.Limit)
	if err != nil {
		errList = append(errList, fmt.Errorf("get blackhole due providers: %w", err))
	}
	fullClientIds, err := self.fullDue(ctx, args.Full.Limit)
	if err != nil {
		errList = append(errList, fmt.Errorf("get full-probe due providers: %w", err))
	}
	result := &ProviderEgressProbeResult{
		Full:         len(blackholeClientIds) == args.Blackhole.Limit || len(fullClientIds) == args.Full.Limit,
		FullDue:      len(fullClientIds),
		BlackholeDue: len(blackholeClientIds),
	}
	if len(blackholeClientIds) == 0 && len(fullClientIds) == 0 {
		return result, errors.Join(errList...)
	}

	pins, err := self.loadPins(ctx)
	if err != nil {
		errList = append(errList, fmt.Errorf("load geolocation pins: %w", err))
		return result, errors.Join(errList...)
	}
	pinSource := func() map[string][]string {
		return pins
	}

	if 0 < len(blackholeClientIds) {
		options := self.blackholeOptions
		options.Pins = pinSource
		options.Timeout = time.Duration(args.Blackhole.ProbeTimeoutSeconds) * time.Second
		options.Concurrency = args.Blackhole.Concurrency
		summary, runErr := self.runBlackhole(ctx, blackholeClientIds, options)
		if runErr != nil {
			errList = append(errList, fmt.Errorf("run blackhole batch: %w", runErr))
		} else {
			result.Checked = len(summary.Checks)
			result.Dark = summary.Dark
			result.TunnelFailed = summary.TunnelFailed
			if submitErr := self.submitBlackholeChecks(ctx, summary.Checks); submitErr != nil {
				errList = append(errList, fmt.Errorf("submit blackhole batch: %w", submitErr))
			}
		}
	}

	// Cancellation is different from an isolated batch failure: starting more
	// tunnels after task drain would extend shutdown and duplicate work after
	// the lease is recovered by another worker.
	if err := ctx.Err(); err != nil {
		errList = append(errList, err)
		return result, errors.Join(errList...)
	}

	if 0 < len(fullClientIds) {
		options := self.fullOptions
		options.Pins = pinSource
		options.ProbeTimeout = time.Duration(args.Full.ProbeTimeoutSeconds) * time.Second
		options.Concurrency = args.Full.Concurrency
		options.AllDestinations = args.Full.AllDestinations
		summary, runErr := self.runFull(ctx, fullClientIds, options)
		if runErr != nil {
			errList = append(errList, fmt.Errorf("run full-probe batch: %w", runErr))
		} else {
			result.Attempted = summary.Attempted
			result.Submitted = summary.Submitted
			result.Failed = summary.Failed
		}
	}
	if err := ctx.Err(); err != nil {
		errList = append(errList, err)
	}

	return result, errors.Join(errList...)
}

// Runtime-only identity, credentials, and clients are joined to the durable
// arguments immediately before the bounded pass begins.
func runProviderEgressProbe(
	ctx context.Context,
	args *ProviderEgressProbeArgs,
) (*ProviderEgressProbeResult, error) {
	identity := model.GetProberIdentity(ctx)
	if identity == nil || identity.ClientId == nil || identity.ByClientJwt == "" {
		return nil, fmt.Errorf("provider egress prober identity is not bootstrapped")
	}
	operatorSecret, err := readProviderEgressOperatorSecret()
	if err != nil {
		return nil, err
	}
	operator := &ingest.Client{
		ServerURL:      args.APIURL,
		OperatorSecret: operatorSecret,
		ShardIndex:     args.ShardIndex,
		ShardCount:     args.ShardCount,
		HTTP:           controlplane.NewHTTPClient(30 * time.Second),
	}
	tunnelConfig := providertunnel.Config{
		ApiURL:            args.APIURL,
		PlatformURL:       args.PlatformURL,
		ByJwt:             identity.ByClientJwt,
		ClientId:          connect.Id(*identity.ClientId),
		DeviceDescription: model.ProberClientDescription,
		DeviceSpec:        model.ProberClientDeviceSpec,
		Version:           server.RequireVersion(),
	}

	var bandwidthSampler *bandwidth.Sampler
	bandwidthTargets := []bandwidth.Target{}
	if args.Full.Bandwidth {
		if strings.TrimSpace(args.PublicAPIURL) != "" {
			bandwidthTargets = append(bandwidthTargets, bandwidth.OperatorTarget(args.PublicAPIURL, operatorSecret))
		}
		cdnURL := args.BandwidthCDNURL
		if strings.TrimSpace(cdnURL) == "" {
			cdnURL = bandwidth.CDNTestURL
		}
		bandwidthTargets = append(bandwidthTargets, bandwidth.Target{
			Name:   "cdn",
			Source: bandwidth.SourceCDN,
			URL:    cdnURL,
		})
		bandwidthSampler = &bandwidth.Sampler{
			Targets: bandwidthTargets,
			Reserve: operator,
			Submit:  operator,
			Timeout: time.Duration(args.Full.BandwidthTimeoutSeconds) * time.Second,
		}
	}

	pass := &providerEgressProbePass{
		blackholeDue: operator.BlackholeDue,
		fullDue:      operator.Due,
		loadPins: func(ctx context.Context) (map[string][]string, error) {
			servedPins, err := operator.GeolocationPins(ctx)
			if err != nil {
				return nil, err
			}
			return fleetprobe.ValidateGeolocationPins(servedPins)
		},
		submitBlackholeChecks: operator.SubmitBlackholeChecks,
		blackholeOptions: fleetprobe.BlackholeOptions{
			TunnelConfig: tunnelConfig,
		},
		fullOptions: fleetprobe.FullOptions{
			TunnelConfig:   tunnelConfig,
			Submit:         operator,
			Attempts:       operator,
			HealthResults:  operator,
			Bandwidth:      bandwidthSampler,
			BandwidthHosts: bandwidth.TargetHosts(bandwidthTargets),
		},
		runBlackhole: fleetprobe.RunBlackhole,
		runFull:      fleetprobe.RunFull,
	}
	result, err := pass.run(ctx, args)

	log.Printf(
		"provider-egress task: shard=%d/%d full_due=%d attempted=%d submitted=%d failed=%d blackhole_due=%d checked=%d dark=%d tunnel_failed=%d backlog=%t",
		args.ShardIndex,
		args.ShardCount,
		result.FullDue,
		result.Attempted,
		result.Submitted,
		result.Failed,
		result.BlackholeDue,
		result.Checked,
		result.Dark,
		result.TunnelFailed,
		result.Full,
	)
	return result, err
}
