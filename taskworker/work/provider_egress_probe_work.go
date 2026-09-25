// This file runs provider egress measurement as host-independent, durable
// taskworker shards rather than as one service assigned to each edge.
package work

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/glog"
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
	Limit int `json:"limit" yaml:"limit"`
	// Concurrency is how many providers are probed at once, each on its own
	// tunnel. Since GEOMAP step 7 a tunnel mostly waits -- between a load's
	// spaced retries, minutes apart -- so this is sized against the prober
	// host's memory (each tunnel carries its own network stack), not its CPU.
	Concurrency         int  `json:"concurrency" yaml:"concurrency"`
	ProbeTimeoutSeconds int  `json:"probe_timeout_seconds" yaml:"probe_timeout_seconds"`
	AllDestinations     bool `json:"all_destinations,omitempty" yaml:"all_destinations"`
	// IpEchoTimeoutSeconds bounds a blackhole check's warm-up -- the operator's
	// /ip echo, which pays the tunnel's cold start before any load starts its
	// clock (GEOMAP §11.3). Blackhole only: a full run's warm-up is its probe
	// timeout.
	IpEchoTimeoutSeconds    int  `json:"ip_echo_timeout_seconds,omitempty" yaml:"ip_echo_timeout_seconds"`
	Bandwidth               bool `json:"bandwidth,omitempty" yaml:"bandwidth"`
	BandwidthTimeoutSeconds int  `json:"bandwidth_timeout_seconds,omitempty" yaml:"bandwidth_timeout_seconds"`
	// Both zero copy default limit values into a fresh task/probe-kind owner.
	// A positive pair selects explicit limits for that same private owner.
	TransportBudgetByteCount connect.ByteCount `json:"transport_budget_byte_count,omitempty" yaml:"transport_budget_byte_count"`
	TransportBudgetCount     int               `json:"transport_budget_count,omitempty" yaml:"transport_budget_count"`
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
	// LoadAttempts, LoadRetryMeanIntervalSeconds and TunnelRecreateAttempts
	// are the load rules of GEOMAP §11.3, passed through to every run and
	// check: how many tries a load gets, the mean of the random spacing
	// between them, and how often one run may re-create a tunnel that died
	// under it.
	LoadAttempts                 int `json:"load_attempts"`
	LoadRetryMeanIntervalSeconds int `json:"load_retry_mean_interval_seconds"`
	TunnelRecreateAttempts       int `json:"tunnel_recreate_attempts"`
	// The dark and batch-guard rules, snapshotted with the rest; the api
	// reads the same keys from the same file at ingest.
	model.ProviderEgressRules
}

// ProviderEgressProbeResult records enough of the pass to drive its successor
// and diagnose whether useful work happened.
type ProviderEgressProbeResult struct {
	Full                  bool `json:"full"`
	Stale                 bool `json:"stale"`
	FullDue               int  `json:"full_due"`
	BlackholeDue          int  `json:"blackhole_due"`
	Attempted             int  `json:"attempted"`
	Submitted             int  `json:"submitted"`
	Failed                int  `json:"failed"`
	FullNotMeasured       int  `json:"full_not_measured"`
	FullGuardTripped      bool `json:"full_guard_tripped"`
	Checked               int  `json:"checked"`
	Dark                  int  `json:"dark"`
	TunnelFailed          int  `json:"tunnel_failed"`
	BlackholeNotMeasured  int  `json:"blackhole_not_measured"`
	BlackholeGuardTripped int  `json:"blackhole_guard_tripped"`
}

// Deployment settings are snapshotted into each task's durable arguments.
type providerEgressProbeSettings struct {
	Enabled                      bool                         `yaml:"enabled"`
	ShardCount                   int                          `yaml:"shard_count"`
	IdleDelaySeconds             int                          `yaml:"idle_delay_seconds"`
	MaxTimeSeconds               int                          `yaml:"max_time_seconds"`
	APIURL                       string                       `yaml:"api_url"`
	PlatformURL                  string                       `yaml:"platform_url"`
	PublicAPIURL                 string                       `yaml:"public_api_url"`
	BandwidthCDNURL              string                       `yaml:"bandwidth_cdn_url"`
	Full                         ProviderEgressProbeBatchArgs `yaml:"full"`
	Blackhole                    ProviderEgressProbeBatchArgs `yaml:"blackhole"`
	LoadAttempts                 int                          `yaml:"load_attempts"`
	LoadRetryMeanIntervalSeconds int                          `yaml:"load_retry_mean_interval_seconds"`
	TunnelRecreateAttempts       int                          `yaml:"tunnel_recreate_attempts"`
	model.ProviderEgressRules    `yaml:",inline"`
}

// Built-in values keep non-main environments usable when no override exists.
// The load rules are the prober module's own defaults, so the two cannot
// drift apart unnoticed.
func defaultProviderEgressProbeSettings(domain string) providerEgressProbeSettings {
	apiURL := "https://api." + domain
	return providerEgressProbeSettings{
		Enabled:          true,
		ShardCount:       defaultProviderEgressProbeShardCount,
		IdleDelaySeconds: 5 * 60,
		// a full run whose loads all keep failing spans its whole retry
		// schedule, about 36 minutes, and a blackhole batch still in flight
		// when the full batch ends may take a check's, about 32 more; see
		// providerEgressProbeMinMaxTime
		MaxTimeSeconds:  75 * 60,
		APIURL:          apiURL,
		PlatformURL:     "wss://connect." + domain,
		PublicAPIURL:    apiURL,
		BandwidthCDNURL: bandwidth.CdnTestUrl,
		Full: ProviderEgressProbeBatchArgs{
			Limit: 8,
			// one round: every provider of a batch waits out its retries
			// at the same time rather than in turn
			Concurrency:             8,
			ProbeTimeoutSeconds:     60,
			AllDestinations:         false,
			Bandwidth:               true,
			BandwidthTimeoutSeconds: int(bandwidth.DefaultTimeout / time.Second),
		},
		Blackhole: ProviderEgressProbeBatchArgs{
			Limit:                250,
			Concurrency:          fleetprobe.DefaultBlackholeConcurrency,
			ProbeTimeoutSeconds:  15,
			IpEchoTimeoutSeconds: int(egresshealth.DefaultIpEchoTimeout / time.Second),
		},
		LoadAttempts:                 egresshealth.DefaultLoadAttempts,
		LoadRetryMeanIntervalSeconds: int(egresshealth.DefaultLoadRetryMeanInterval / time.Second),
		TunnelRecreateAttempts:       egresshealth.DefaultTunnelRecreateAttempts,
		ProviderEgressRules:          model.DefaultProviderEgressRules(),
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
	if args.TransportBudgetByteCount < 0 || args.TransportBudgetCount < 0 ||
		(args.TransportBudgetByteCount == 0) != (args.TransportBudgetCount == 0) {
		return fmt.Errorf("provider egress probe %s transport budget byte/count limits must both be zero or both be positive", name)
	}
	return nil
}

// A full run's health geometry: the warm-up is its probe timeout, each load
// attempt the smaller of that and the module's per-request floor, with the
// load rules the arguments carry.
func providerEgressFullHealthOptions(args *ProviderEgressProbeArgs) egresshealth.Options {
	probeTimeout := time.Duration(args.Full.ProbeTimeoutSeconds) * time.Second
	options := fleetprobe.EgressHealthOptions(probeTimeout, args.Full.AllDestinations)
	options.LoadAttempts = args.LoadAttempts
	options.LoadRetryMeanInterval = time.Duration(args.LoadRetryMeanIntervalSeconds) * time.Second
	options.TunnelRecreateAttempts = args.TunnelRecreateAttempts
	return options
}

// The shortest max_time_seconds a pass can be given: one full run's whole
// retry schedule (Options.RunBudget over the loads it draws, with its warm-up)
// plus its tunnel open and bandwidth sample, and a blackhole check's, since a
// blackhole batch in flight when the full batch ends runs to completion. A
// pass that outlives its max time is read as stalled by §2.19, and a run the
// task ends mid-way is never submitted (egresshealth.ErrInterrupted), so the
// bound has to move with the load rules.
func providerEgressProbeMinMaxTime(args *ProviderEgressProbeArgs) time.Duration {
	fullOptions := providerEgressFullHealthOptions(args)
	// the budget counts the warm-up only for a run that has one, which every
	// production run does
	fullOptions.IpEchoUrl = egresshealth.IpEchoPath
	loads := egresshealth.SamplePerRun()
	if args.Full.AllDestinations {
		loads = len(egresshealth.Destinations())
	}
	probeTimeout := time.Duration(args.Full.ProbeTimeoutSeconds) * time.Second
	full := fullOptions.RunBudget(loads) + probeTimeout
	if args.Full.Bandwidth {
		full += time.Duration(args.Full.BandwidthTimeoutSeconds) * time.Second
	}

	// a blackhole check's geometry: its warm-up has its own timeout
	checkOptions := egresshealth.Options{
		PerRequestTimeout:      time.Duration(args.Blackhole.ProbeTimeoutSeconds) * time.Second,
		IpEchoUrl:              egresshealth.IpEchoPath,
		IpEchoTimeout:          time.Duration(args.Blackhole.IpEchoTimeoutSeconds) * time.Second,
		LoadAttempts:           args.LoadAttempts,
		LoadRetryMeanInterval:  time.Duration(args.LoadRetryMeanIntervalSeconds) * time.Second,
		TunnelRecreateAttempts: args.TunnelRecreateAttempts,
	}
	check := checkOptions.RunBudget(egresshealth.BlackholeSampleSize) +
		max(checkOptions.PerRequestTimeout, checkOptions.IpEchoTimeout)
	return full + check
}

// Every invariant of one argument snapshot, whether it came from settings or a
// durable row: shard geometry, deadlines, endpoints, the load and dark rules,
// the health sampler's per-request floor, and a max time that covers a run.
func validateProviderEgressProbeArgsConfig(args *ProviderEgressProbeArgs) error {
	if args.ShardCount < 1 || maxProviderEgressProbeShardCount < args.ShardCount {
		return fmt.Errorf("provider egress probe shard_count must be in [1,%d] (got %d)", maxProviderEgressProbeShardCount, args.ShardCount)
	}
	if args.IdleDelaySeconds < 1 || args.MaxTimeSeconds < 1 {
		return fmt.Errorf("provider egress probe idle delay and max time must be positive")
	}
	if strings.TrimSpace(args.APIURL) == "" || strings.TrimSpace(args.PlatformURL) == "" {
		return fmt.Errorf("provider egress probe api_url and platform_url are required")
	}
	if err := validateProviderEgressProbeBatchArgs("full", args.Full); err != nil {
		return err
	}
	if err := validateProviderEgressProbeBatchArgs("blackhole", args.Blackhole); err != nil {
		return err
	}
	if args.Full.IpEchoTimeoutSeconds != 0 {
		return fmt.Errorf("provider egress probe full.ip_echo_timeout_seconds must be unset: a full run's warm-up is its probe timeout")
	}
	if args.Blackhole.IpEchoTimeoutSeconds < 1 {
		return fmt.Errorf("provider egress probe blackhole.ip_echo_timeout_seconds must be positive")
	}
	if args.LoadAttempts < 1 || args.LoadRetryMeanIntervalSeconds < 1 || args.TunnelRecreateAttempts < 1 {
		return fmt.Errorf("provider egress probe load_attempts, load_retry_mean_interval_seconds and tunnel_recreate_attempts must be positive")
	}
	if err := args.ProviderEgressRules.Validate(); err != nil {
		return err
	}
	if options := providerEgressFullHealthOptions(args); options.PerRequestTimeout < egresshealth.DefaultPerRequestTimeout {
		return fmt.Errorf(
			"provider egress probe full timeout %s leaves %s per health request, below the %s minimum",
			time.Duration(args.Full.ProbeTimeoutSeconds)*time.Second,
			options.PerRequestTimeout,
			egresshealth.DefaultPerRequestTimeout,
		)
	}
	if minMaxTime := providerEgressProbeMinMaxTime(args); time.Duration(args.MaxTimeSeconds)*time.Second < minMaxTime {
		return fmt.Errorf(
			"provider egress probe max_time_seconds %d is shorter than one full run and one blackhole check at these load rules (%d)",
			args.MaxTimeSeconds,
			int64((minMaxTime+time.Second-1)/time.Second),
		)
	}
	return nil
}

// Every deployment-level invariant is applied to one settings snapshot.
func (self providerEgressProbeSettings) validate() error {
	if !self.Enabled {
		return nil
	}
	return validateProviderEgressProbeArgsConfig(providerEgressProbeArgs(self, 0))
}

// The optional environment resource overlays defaults and is validated before
// any task is scheduled from it.
func loadProviderEgressProbeSettings() (providerEgressProbeSettings, error) {
	domain, err := server.Domain()
	if err != nil {
		return providerEgressProbeSettings{}, err
	}
	settings := defaultProviderEgressProbeSettings(domain)
	resource, err := server.Config.SimpleResource(model.ProviderEgressProbeResourceName)
	if err != nil && !errors.Is(err, server.ErrResourceNotFound) {
		return providerEgressProbeSettings{}, err
	}
	if err == nil {
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
	rules := settings.ProviderEgressRules
	rules.DarkBackoffSeconds = append([]int(nil), settings.DarkBackoffSeconds...)
	return &ProviderEgressProbeArgs{
		ShardIndex:                   shardIndex,
		ShardCount:                   settings.ShardCount,
		IdleDelaySeconds:             settings.IdleDelaySeconds,
		MaxTimeSeconds:               settings.MaxTimeSeconds,
		Full:                         settings.Full,
		Blackhole:                    settings.Blackhole,
		APIURL:                       settings.APIURL,
		PlatformURL:                  settings.PlatformURL,
		PublicAPIURL:                 settings.PublicAPIURL,
		BandwidthCDNURL:              settings.BandwidthCDNURL,
		LoadAttempts:                 settings.LoadAttempts,
		LoadRetryMeanIntervalSeconds: settings.LoadRetryMeanIntervalSeconds,
		TunnelRecreateAttempts:       settings.TunnelRecreateAttempts,
		ProviderEgressRules:          rules,
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
	return validateProviderEgressProbeArgsConfig(args)
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
	// a durable snapshot written before the load and dark rules were part of
	// the arguments is a stale generation, not an invalid configuration:
	// retiring it lets its post-step write the current snapshot, where
	// failing it would retry the old arguments forever
	if args != nil && args.LoadAttempts == 0 && args.DarkConsecutiveFailures == 0 {
		return &ProviderEgressProbeResult{Stale: true}, nil
	}
	if err := validateProviderEgressProbeArgs(args); err != nil {
		return nil, err
	}
	if args.ShardCount != settings.ShardCount || settings.ShardCount <= args.ShardIndex {
		return &ProviderEgressProbeResult{Stale: true}, nil
	}
	result, err := executeProviderEgressProbe(clientSession.Ctx, args)
	if providerEgressProbeUnfundedOnly(err) {
		err = task.WithRetryDelay(err, time.Duration(args.IdleDelaySeconds)*time.Second)
	}
	return result, err
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

// The operator's /ip echo on the api's public address: every run and check
// fetches it through the provider's tunnel, so the request leaves from the
// provider and must reach the api from the open internet.
func providerEgressIpEchoUrl(args *ProviderEgressProbeArgs) string {
	if strings.TrimSpace(args.PublicAPIURL) != "" {
		return fleetprobe.IpEchoUrl(args.PublicAPIURL)
	}
	return fleetprobe.IpEchoUrl(args.APIURL)
}

// One immutable pass owns the API operations and bounded batch runners used by
// a shard task. The explicit boundaries make it deterministic to prove that
// the two independent probe schedules cannot starve each other when one fails.
// It is safe for concurrent use after construction.
type providerEgressProbePass struct {
	readiness    *providerEgressProbeReadiness
	blackholeDue func(context.Context, int) ([]ingest.DueProvider, error)
	fullDue      func(context.Context, int) ([]ingest.DueProvider, error)
	loadPins     func(context.Context) (map[string][]string, error)
	// loadPool fetches the pass's destination pool. It always returns a pool
	// that can be run -- the built-in table when the server's cannot be had --
	// and the error only says why it fell back. Nil runs the built-in table.
	loadPool func(context.Context) (*egresshealth.Pool, error)
	// loadScoring reads which pooled sites may count in a provider's run (see
	// model.ProviderEgressHealthScoring) and the refresh's settings, for the
	// full batch's scoring and site tally. Nil scores every load and records
	// no tally.
	loadScoring           func(context.Context) (*model.ProviderEgressHealthScoring, *model.ProviderEgressSiteSettings)
	submitBlackholeChecks func(context.Context, []ingest.BlackholeCheck) error
	blackholeOptions      fleetprobe.BlackholeOptions
	fullOptions           fleetprobe.FullOptions
	// fullSink is where a full batch's submissions go once the run guard has
	// passed the batch; the prober's reporters only collect into the batch.
	fullSink *egressProbeMetricsReporter
	// recordTally records one submitted run in the site tally the pool
	// refresh judges sites by. Nil records nothing.
	recordTally  func(context.Context, time.Time, model.ProviderEgressRunTally, []model.ProviderEgressSiteLoad)
	runBlackhole func(context.Context, []prober.Provider, fleetprobe.BlackholeOptions) (fleetprobe.BlackholeSummary, error)
	runFull      func(context.Context, []prober.Provider, fleetprobe.FullOptions) (prober.Summary, error)
	// refreshFleet re-counts the fleet gauges after every non-canceled pass,
	// including idle and failed passes. A process-local snapshot that is not
	// refreshed must age out visibly rather than be pushed forever as current.
	// Nil skips it in focused tests.
	refreshFleet func(context.Context)
}

type providerEgressBlackholeOutcome struct {
	due          int
	checked      int
	dark         int
	tunnelFailed int
	notMeasured  int
	guardTripped int
	full         bool
	err          error
}

type providerEgressFullOutcome struct {
	summary      prober.Summary
	guardTripped bool
	err          error
}

// The dark batch guard of GEOMAP §11.3 over one batch's checks: when more than
// DarkBatchGuard of the measured checks are dark, the batch is the prober's
// fault until proven otherwise -- a check that fails everyone at once is a
// prober host, a request profile or a route, not a fifth of the fleet going
// dark together. Its negative results are then discarded: each becomes a check
// that measured nothing, which the server stores as not measured and
// reschedules after the backoff, so the batch's providers are re-checked soon
// without a failure counted against them. Passes and TLS-authentication
// failures are kept, as the admission-failure path keeps them: a pass is
// evidence whatever the prober's state, and a forged certificate is not
// something a prober fault produces.
//
// A check that measured nothing counts toward neither side of the share, and
// a batch with fewer than DarkBatchGuardMinChecks measured checks is not
// judged: one dark provider in a three-provider tail batch is a provider.
func providerEgressBlackholeGuard(
	summary fleetprobe.BlackholeSummary,
	rules model.ProviderEgressRules,
) (guarded fleetprobe.BlackholeSummary, share float64, tripped bool) {
	measured := len(summary.Checks) - summary.NotMeasured
	if measured < 1 {
		return summary, 0, false
	}
	share = float64(summary.Dark) / float64(measured)
	if measured < rules.DarkBatchGuardMinChecks || share <= rules.DarkBatchGuard {
		return summary, share, false
	}
	guarded = fleetprobe.BlackholeSummary{
		Checks: make([]ingest.BlackholeCheck, 0, len(summary.Checks)),
	}
	for _, check := range summary.Checks {
		switch {
		case check.Ok || check.NotMeasured:
		case check.Failure == egresshealth.FailureTlsAuthentication:
			guarded.Dark++
		default:
			check = ingest.BlackholeCheck{
				ClientId:    check.ClientId,
				Ok:          false,
				Failure:     egresshealth.FailureNotMeasured,
				NotMeasured: true,
				CheckedAt:   check.CheckedAt,
			}
		}
		if check.NotMeasured {
			guarded.NotMeasured++
		}
		guarded.Checks = append(guarded.Checks, check)
	}
	return guarded, share, true
}

// runBlackholeBatch owns the accounting and submission for one due batch.
// Returning the summary with a submission error preserves the work that was
// actually measured while preventing the caller from immediately measuring the
// same still-due rows again.
func (self *providerEgressProbePass) runBlackholeBatch(
	ctx context.Context,
	args *ProviderEgressProbeArgs,
	pinSource fleetprobe.PinSource,
	poolSource fleetprobe.PoolSource,
	concurrency int,
	providers []prober.Provider,
) (fleetprobe.BlackholeSummary, bool, error) {
	if err := self.readiness.check(ctx); err != nil {
		egressProbePassesTotal.WithLabelValues("blackhole", "error").Inc()
		return fleetprobe.BlackholeSummary{}, false, err
	}
	options := self.blackholeOptions
	options.Pins = pinSource
	options.Pool = poolSource
	options.Timeout = time.Duration(args.Blackhole.ProbeTimeoutSeconds) * time.Second
	options.IpEchoTimeout = time.Duration(args.Blackhole.IpEchoTimeoutSeconds) * time.Second
	options.IpEchoUrl = providerEgressIpEchoUrl(args)
	options.LoadAttempts = args.LoadAttempts
	options.LoadRetryMeanInterval = time.Duration(args.LoadRetryMeanIntervalSeconds) * time.Second
	options.TunnelRecreateAttempts = args.TunnelRecreateAttempts
	options.Concurrency = concurrency
	startTime := time.Now()
	summary, runErr := self.runBlackhole(ctx, providers, options)
	egressProbePassSeconds.WithLabelValues("blackhole").Observe(time.Since(startTime).Seconds())
	if runErr != nil {
		egressProbePassesTotal.WithLabelValues("blackhole", "error").Inc()
		egressProbePassErrorsTotal.WithLabelValues("blackhole_run").Inc()
		return fleetprobe.BlackholeSummary{}, false, fmt.Errorf("run blackhole batch: %w", runErr)
	}

	var measurementErr error
	for _, check := range summary.Checks {
		if !check.Ok && !check.NotMeasured && check.Failure != egresshealth.FailureTlsAuthentication {
			measurementErr = self.readiness.check(ctx)
			break
		}
	}
	if measurementErr != nil {
		// Credit can run out during a batch. Keep successful traffic,
		// authenticated TLS failures and checks that measured nothing, but
		// retain previous provider state for negative reachability measured
		// across this shared admission failure.
		checks := make([]ingest.BlackholeCheck, 0, len(summary.Checks))
		summary.Dark = 0
		summary.TunnelFailed = 0
		summary.NotMeasured = 0
		for _, check := range summary.Checks {
			if check.Ok || check.NotMeasured || check.Failure == egresshealth.FailureTlsAuthentication {
				checks = append(checks, check)
				if check.NotMeasured {
					summary.NotMeasured++
				} else if !check.Ok {
					summary.Dark++
				}
			}
		}
		summary.Checks = checks
		egressProbePassesTotal.WithLabelValues("blackhole", "error").Inc()
	} else {
		egressProbePassesTotal.WithLabelValues("blackhole", "ok").Inc()
	}

	guarded, share, tripped := providerEgressBlackholeGuard(summary, args.ProviderEgressRules)
	egressProbeBatchShare.WithLabelValues("blackhole").Set(share)
	if tripped {
		egressProbeBatchGuardTripsTotal.WithLabelValues("blackhole").Inc()
		// alert class: a fifth of a batch dark at once is the prober until
		// proven otherwise (GEOMAP §11.3), and the negatives are gone
		glog.Errorf(
			"[egress]dark batch guard tripped: shard=%d/%d dark_share=%.3f guard=%.3f measured=%d discarded=%d; the negatives were resubmitted as not measured and are re-checked after the backoff\n",
			args.ShardIndex,
			args.ShardCount,
			share,
			args.DarkBatchGuard,
			len(summary.Checks)-summary.NotMeasured,
			guarded.NotMeasured-summary.NotMeasured,
		)
		summary = guarded
	}

	egressProbePassProvidersTotal.WithLabelValues("blackhole", "checked").Add(float64(len(summary.Checks)))
	egressProbePassProvidersTotal.WithLabelValues("blackhole", "dark").Add(float64(summary.Dark))
	egressProbePassProvidersTotal.WithLabelValues("blackhole", "tunnel_failed").Add(float64(summary.TunnelFailed))
	egressProbePassNotMeasuredTotal.WithLabelValues("blackhole").Add(float64(summary.NotMeasured))
	if 0 < len(summary.Checks) {
		if submitErr := self.submitBlackholeChecks(ctx, summary.Checks); submitErr != nil {
			egressProbePassErrorsTotal.WithLabelValues("blackhole_submit").Inc()
			return summary, tripped, errors.Join(measurementErr, fmt.Errorf("submit blackhole batch: %w", submitErr))
		}
	}
	return summary, tripped, measurementErr
}

// One buffered location submission.
type providerEgressFullBatchExit struct {
	exitIp     string
	observedAt time.Time
}

// Everything one provider's probe submitted.
type providerEgressFullBatchEntry struct {
	health  *egresshealth.Result
	exit    *providerEgressFullBatchExit
	attempt *string
}

// Holds one full batch's submissions until the run guard has judged the batch
// (GEOMAP §11.3): a batch that fails too much at once is a CDN outage, a
// broken request profile or a saturated prober host, and submitting it would
// stamp every provider in it with a bad index for days. It stands where the
// prober's submitter, attempt reporter and health reporter would, answers
// every call as accepted, and hands each provider's calls on, in the order
// they were made, only when the batch is released. Bandwidth reservations and
// samples pass straight through: they are a diagnostic the guard does not
// judge.
type providerEgressFullBatch struct {
	sink *egressProbeMetricsReporter

	stateLock  sync.Mutex
	order      []string
	byProvider map[string]*providerEgressFullBatchEntry
}

// An empty batch whose release goes to sink.
func newProviderEgressFullBatch(sink *egressProbeMetricsReporter) *providerEgressFullBatch {
	return &providerEgressFullBatch{
		sink:       sink,
		byProvider: map[string]*providerEgressFullBatchEntry{},
	}
}

// The provider's entry, created in submission order. Held under stateLock.
func (self *providerEgressFullBatch) entryWithLock(providerClientId string) *providerEgressFullBatchEntry {
	entry, ok := self.byProvider[providerClientId]
	if !ok {
		entry = &providerEgressFullBatchEntry{}
		self.byProvider[providerClientId] = entry
		self.order = append(self.order, providerClientId)
	}
	return entry
}

// Implements prober.Submitter: the exit is held for the release.
func (self *providerEgressFullBatch) Submit(_ context.Context, providerClientId string, exitIp string, observedAt time.Time) error {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.entryWithLock(providerClientId).exit = &providerEgressFullBatchExit{exitIp: exitIp, observedAt: observedAt}
	}()
	return nil
}

// Implements prober.AttemptReporter: the attempt is held for the release.
func (self *providerEgressFullBatch) ReportAttempt(_ context.Context, providerClientId string, probeFailure string) error {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.entryWithLock(providerClientId).attempt = &probeFailure
	}()
	return nil
}

// Implements prober.HealthReporter: the run is held for the release.
func (self *providerEgressFullBatch) SubmitEgressHealth(_ context.Context, providerClientId string, res *egresshealth.Result) error {
	if res == nil {
		return nil
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.entryWithLock(providerClientId).health = res
	}()
	return nil
}

// Passes a bandwidth reservation straight to the sink.
func (self *providerEgressFullBatch) ReserveBandwidth(ctx context.Context, providerClientId string, byteCount int64) error {
	return self.sink.ReserveBandwidth(ctx, providerClientId, byteCount)
}

// Passes a bandwidth sample straight to the sink.
func (self *providerEgressFullBatch) SubmitBandwidth(ctx context.Context, providerClientId string, source string, bytesPerSecond float64, sampleByteCount int64) error {
	return self.sink.SubmitBandwidth(ctx, providerClientId, source, bytesPerSecond, sampleByteCount)
}

// Passes blackhole checks straight to the sink.
func (self *providerEgressFullBatch) SubmitBlackholeChecks(ctx context.Context, checks []ingest.BlackholeCheck) error {
	return self.sink.SubmitBlackholeChecks(ctx, checks)
}

// The run batch guard of GEOMAP §11.3 over one batch's scored runs: the failed
// share of their scored loads, and whether it is above RunBatchGuard. Loads
// that were not measured are in no run's total already, and a batch with fewer
// than RunBatchGuardMinRuns runs is not judged.
func providerEgressRunGuard(runs []*egresshealth.Result, rules model.ProviderEgressRules) (share float64, tripped bool) {
	measuredRuns, loads, failures := 0, 0, 0
	for _, run := range runs {
		if run == nil || run.Total < 1 {
			continue
		}
		measuredRuns++
		loads += run.Total
		failures += run.Total - run.OkCount
	}
	if loads < 1 {
		return 0, false
	}
	share = float64(failures) / float64(loads)
	return share, rules.RunBatchGuardMinRuns <= measuredRuns && rules.RunBatchGuard < share
}

// A run as it may count: every load of a site the pool has on probation, or
// marked incompatible with the provider's place, taken out of the counts, the
// class tallies and the checks, so neither the egress index nor the 90 % rule
// sees it pass or fail (GEOMAP §11.3, §11.4). The server can only take out
// what a run says failed; this task holds every load, so it takes out the
// passes too. Canaries and loads that were not measured are already in no
// count and are left as they are.
func scoreEgressHealthResult(
	res *egresshealth.Result,
	place model.ProviderEgressPlace,
	scoring *model.ProviderEgressHealthScoring,
) *egresshealth.Result {
	if res == nil || scoring == nil {
		return res
	}
	scored := *res
	scored.Checks = make([]egresshealth.CheckResult, 0, len(res.Checks))
	scored.ByClass = map[egresshealth.Class]egresshealth.ClassSummary{}
	for class, summary := range res.ByClass {
		scored.ByClass[class] = summary
	}
	for _, check := range res.Checks {
		if check.Canary || check.NotMeasured || scoring.Scores(check.Name, place) {
			scored.Checks = append(scored.Checks, check)
			continue
		}
		summary := scored.ByClass[check.Class]
		summary.Total--
		scored.Total--
		if check.Ok {
			summary.Ok--
			scored.OkCount--
		}
		scored.ByClass[check.Class] = summary
	}
	for class, summary := range scored.ByClass {
		if summary.Total < 1 {
			delete(scored.ByClass, class)
		}
	}
	return &scored
}

// One run's loads as the site tally counts them, and whether the run was
// healthy. A load's exit is healthy when it passed at least healthyShare of
// the other scored sites of the same run -- so a site that fails everyone is
// still judged on exits that work, not on the exits it drags under the line
// itself. Probationary and incompatible loads are tallied against the run's
// scored sites without being among them; a canary is tallied as a canary; a
// load that was not measured is not tallied at all.
func egressHealthSiteLoads(
	res *egresshealth.Result,
	place model.ProviderEgressPlace,
	scoring *model.ProviderEgressHealthScoring,
	healthyShare float64,
) (loads []model.ProviderEgressSiteLoad, runHealthy bool) {
	scoredOk, scoredTotal := 0, 0
	scores := func(check egresshealth.CheckResult) bool {
		return !check.Canary && !check.NotMeasured && scoring.Scores(check.Name, place)
	}
	for _, check := range res.Checks {
		if scores(check) {
			scoredTotal++
			if check.Ok {
				scoredOk++
			}
		}
	}
	healthy := func(ok int, total int) bool {
		return 0 < total && healthyShare*float64(total) <= float64(ok)
	}
	runHealthy = healthy(scoredOk, scoredTotal)
	for _, check := range res.Checks {
		if check.NotMeasured {
			continue
		}
		if check.Canary {
			loads = append(loads, model.ProviderEgressSiteLoad{Name: check.Name, Ok: check.Ok, Canary: true})
			continue
		}
		othersOk, othersTotal := scoredOk, scoredTotal
		if scores(check) {
			othersTotal--
			if check.Ok {
				othersOk--
			}
		}
		loads = append(loads, model.ProviderEgressSiteLoad{
			Name:    check.Name,
			Ok:      check.Ok,
			Healthy: healthy(othersOk, othersTotal),
		})
	}
	return loads, runHealthy
}

// Hands the batch on. A batch the guard held back submits nothing but
// its attempts, each as the run guard's class, so its providers come round
// again after the first backoff step rather than the ordinary attempt backoff
// (model.ProbeRunBatchGuardClass). Otherwise every provider's health run goes
// on scored (scoreEgressHealthResult) and is tallied, its exit is submitted --
// a submission the server refuses turns its attempt into submit_failed, as the
// prober would have reported it -- and its attempt follows. It returns how
// many location submissions failed.
func (self *providerEgressFullBatch) release(
	ctx context.Context,
	tripped bool,
	places map[string]model.ProviderEgressPlace,
	scoring *model.ProviderEgressHealthScoring,
	healthyShare float64,
	recordTally func(context.Context, time.Time, model.ProviderEgressRunTally, []model.ProviderEgressSiteLoad),
) (submitFailures int) {
	// the release calls the sink, so it works on a copy taken under the lock
	var order []string
	var entries map[string]*providerEgressFullBatchEntry
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		order = append([]string(nil), self.order...)
		entries = make(map[string]*providerEgressFullBatchEntry, len(self.byProvider))
		for providerClientId, entry := range self.byProvider {
			entries[providerClientId] = entry
		}
	}()

	for _, providerClientId := range order {
		entry := entries[providerClientId]
		if tripped {
			if entry.attempt != nil {
				_ = self.sink.ReportAttempt(ctx, providerClientId, model.ProbeRunBatchGuardClass)
			}
			continue
		}
		place := places[providerClientId]
		if entry.health != nil {
			scored := scoreEgressHealthResult(entry.health, place, scoring)
			// a run left with no scored load measured nothing that may count,
			// and is not submitted over the provider's last real run; its
			// loads still go to the tally, which judges the sites that were
			// left out
			submitted := false
			if 0 < scored.Total {
				submitted = self.sink.submitEgressHealthScored(ctx, providerClientId, entry.health, scored) == nil
			}
			if (submitted || scored.Total < 1) && recordTally != nil {
				loads, runHealthy := egressHealthSiteLoads(entry.health, place, scoring, healthyShare)
				recordTally(ctx, server.NowUtc(), model.ProviderEgressRunTally{
					Place:      place,
					Healthy:    runHealthy,
					EchoFailed: entry.health.ExitIp == "",
				}, loads)
			}
		}
		failure := ""
		if entry.attempt != nil {
			failure = *entry.attempt
		}
		if entry.exit != nil {
			if err := self.sink.Submit(ctx, providerClientId, entry.exit.exitIp, entry.exit.observedAt); err != nil {
				submitFailures++
				failure = prober.FailureSubmit
			}
		}
		if entry.attempt != nil {
			_ = self.sink.ReportAttempt(ctx, providerClientId, failure)
		}
	}
	return submitFailures
}

// Snapshots health runs with their provider identity so the guard uses the
// same per-provider place as publication. Guard totals do not depend on order.
func (self *providerEgressFullBatch) resultsByProvider() map[string]*egresshealth.Result {
	results := map[string]*egresshealth.Result{}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		for _, providerClientId := range self.order {
			if health := self.byProvider[providerClientId].health; health != nil {
				results[providerClientId] = health
			}
		}
	}()
	return results
}

func (self *providerEgressProbePass) runFullBatch(
	ctx context.Context,
	args *ProviderEgressProbeArgs,
	pinSource fleetprobe.PinSource,
	poolSource fleetprobe.PoolSource,
	due []ingest.DueProvider,
) providerEgressFullOutcome {
	if err := self.readiness.check(ctx); err != nil {
		egressProbePassesTotal.WithLabelValues("full", "error").Inc()
		return providerEgressFullOutcome{err: err}
	}
	var scoring *model.ProviderEgressHealthScoring
	siteSettings := model.DefaultProviderEgressSiteSettings()
	if self.loadScoring != nil {
		scoring, siteSettings = self.loadScoring(ctx)
	}
	places := map[string]model.ProviderEgressPlace{}
	for _, provider := range due {
		places[provider.ClientId] = model.ProviderEgressPlace{
			CountryCode: strings.ToLower(strings.TrimSpace(provider.CountryCode)),
			Region:      strings.TrimSpace(provider.Region),
		}
	}

	batch := newProviderEgressFullBatch(self.fullSink)
	guarded := &providerEgressProbeReadinessReporter{
		egressProbeIngest: batch,
		readiness:         self.readiness,
	}
	options := self.fullOptions
	options.Pins = pinSource
	options.Pool = poolSource
	options.ProbeTimeout = time.Duration(args.Full.ProbeTimeoutSeconds) * time.Second
	options.Concurrency = args.Full.Concurrency
	options.AllDestinations = args.Full.AllDestinations
	options.IpEchoUrl = providerEgressIpEchoUrl(args)
	options.LoadAttempts = args.LoadAttempts
	options.LoadRetryMeanInterval = time.Duration(args.LoadRetryMeanIntervalSeconds) * time.Second
	options.TunnelRecreateAttempts = args.TunnelRecreateAttempts
	options.Submit = batch
	options.Attempts = guarded
	options.HealthResults = guarded
	startTime := time.Now()
	summary, runErr := self.runFull(ctx, fleetprobe.ProvidersFromDue(due), options)
	egressProbePassSeconds.WithLabelValues("full").Observe(time.Since(startTime).Seconds())
	if runErr != nil {
		egressProbePassesTotal.WithLabelValues("full", "error").Inc()
		egressProbePassErrorsTotal.WithLabelValues("full_run").Inc()
		return providerEgressFullOutcome{err: fmt.Errorf("run full-probe batch: %w", runErr)}
	}

	// the guard and publication score the same provider/place: neither
	// probationary nor incompatible loads may trip or dilute the guard
	scoredRuns := []*egresshealth.Result{}
	for providerClientId, run := range batch.resultsByProvider() {
		scoredRuns = append(scoredRuns, scoreEgressHealthResult(run, places[providerClientId], scoring))
	}
	share, tripped := providerEgressRunGuard(scoredRuns, args.ProviderEgressRules)
	egressProbeBatchShare.WithLabelValues("full").Set(share)
	if tripped {
		egressProbeBatchGuardTripsTotal.WithLabelValues("full").Inc()
		// alert class: a batch failing this much at once is the prober until
		// proven otherwise, and nothing it measured is submitted
		glog.Errorf(
			"[egress]run batch guard tripped: shard=%d/%d failure_share=%.3f guard=%.3f runs=%d; the batch was not submitted and its providers are re-queued after the backoff\n",
			args.ShardIndex,
			args.ShardCount,
			share,
			args.RunBatchGuard,
			len(scoredRuns),
		)
	}

	// submitted on a context detached from the task's: a batch measured before
	// a drain still lands, and the prober itself never submits a run the task
	// ended under. The release stays bounded without a deadline of its own:
	// it makes at most three requests per provider of the batch, each bounded
	// by the control-plane client's timeout.
	submitFailures := batch.release(context.WithoutCancel(ctx), tripped, places, scoring, siteSettings.SiteHealthyExitShare, self.recordTally)
	if tripped {
		summary.Failed += summary.Submitted
		summary.Submitted = 0
	} else {
		summary.Submitted -= submitFailures
		summary.Failed += submitFailures
	}

	measurementErr := self.readiness.err()
	if measurementErr != nil {
		egressProbePassesTotal.WithLabelValues("full", "error").Inc()
	} else {
		egressProbePassesTotal.WithLabelValues("full", "ok").Inc()
	}
	egressProbePassProvidersTotal.WithLabelValues("full", "attempted").Add(float64(summary.Attempted))
	egressProbePassProvidersTotal.WithLabelValues("full", "submitted").Add(float64(summary.Submitted))
	egressProbePassProvidersTotal.WithLabelValues("full", "skipped").Add(float64(summary.Skipped))
	egressProbePassProvidersTotal.WithLabelValues("full", "failed").Add(float64(summary.Failed))
	egressProbePassNotMeasuredTotal.WithLabelValues("full").Add(float64(summary.NotMeasured))
	return providerEgressFullOutcome{summary: summary, guardTripped: tripped, err: measurementErr}
}

// drainBlackhole runs the already-selected batch and, while a concurrent full
// batch remains in flight, keeps selecting saturated successor batches. It
// stops on the first error, partial batch, cancellation, or full completion so
// the durable task remains bounded by its existing full-batch lifetime.
func (self *providerEgressProbePass) drainBlackhole(
	ctx context.Context,
	args *ProviderEgressProbeArgs,
	pinSource fleetprobe.PinSource,
	poolSource fleetprobe.PoolSource,
	concurrency int,
	initialDue []ingest.DueProvider,
	fullFinished <-chan struct{},
) providerEgressBlackholeOutcome {
	outcome := providerEgressBlackholeOutcome{}
	due := initialDue
	for 0 < len(due) {
		outcome.due += len(due)
		outcome.full = outcome.full || len(due) == args.Blackhole.Limit
		summary, tripped, err := self.runBlackholeBatch(ctx, args, pinSource, poolSource, concurrency, fleetprobe.ProvidersFromDue(due))
		outcome.checked += len(summary.Checks)
		outcome.dark += summary.Dark
		outcome.tunnelFailed += summary.TunnelFailed
		outcome.notMeasured += summary.NotMeasured
		if tripped {
			outcome.guardTripped++
		}
		if err != nil {
			outcome.err = err
			break
		}
		if ctx.Err() != nil || len(due) < args.Blackhole.Limit || fullFinished == nil {
			break
		}
		select {
		case <-fullFinished:
			return outcome
		default:
		}

		due, err = self.blackholeDue(ctx, args.Blackhole.Limit)
		egressProbePassDue.WithLabelValues("blackhole").Set(float64(len(due)))
		if err != nil {
			egressProbePassErrorsTotal.WithLabelValues("blackhole_due").Inc()
			outcome.err = fmt.Errorf("get blackhole due providers: %w", err)
			break
		}
		// A full batch can finish during the bounded due lookup. Do not admit
		// another tunnel batch after that closure boundary.
		select {
		case <-fullFinished:
			return outcome
		default:
		}
	}
	return outcome
}

// run executes both independently due schedules with one certificate-pin and
// one destination-pool snapshot. When both queues have work and the configured
// blackhole pool can reserve the full pool without raising the shard's prior
// peak concurrency, full work and a repeated blackhole drain run together. A
// failure in one lane is retained but does not suppress the other; task retry
// then revisits only work whose server-side due state remains stale.
func (self *providerEgressProbePass) run(
	ctx context.Context,
	args *ProviderEgressProbeArgs,
) (*ProviderEgressProbeResult, error) {
	if self.refreshFleet != nil {
		defer func() {
			if ctx.Err() == nil {
				self.refreshFleet(ctx)
			}
		}()
	}
	if err := self.readiness.check(ctx); err != nil {
		return &ProviderEgressProbeResult{}, err
	}
	errList := []error{}
	blackholeDue, err := self.blackholeDue(ctx, args.Blackhole.Limit)
	if err != nil {
		egressProbePassErrorsTotal.WithLabelValues("blackhole_due").Inc()
		errList = append(errList, fmt.Errorf("get blackhole due providers: %w", err))
	}
	fullDue, err := self.fullDue(ctx, args.Full.Limit)
	if err != nil {
		egressProbePassErrorsTotal.WithLabelValues("full_due").Inc()
		errList = append(errList, fmt.Errorf("get full-probe due providers: %w", err))
	}
	egressProbePassDue.WithLabelValues("blackhole").Set(float64(len(blackholeDue)))
	egressProbePassDue.WithLabelValues("full").Set(float64(len(fullDue)))
	result := &ProviderEgressProbeResult{
		Full:         len(blackholeDue) == args.Blackhole.Limit || len(fullDue) == args.Full.Limit,
		FullDue:      len(fullDue),
		BlackholeDue: len(blackholeDue),
	}
	if len(blackholeDue) == 0 && len(fullDue) == 0 {
		egressProbePassesTotal.WithLabelValues("blackhole", "empty").Inc()
		egressProbePassesTotal.WithLabelValues("full", "empty").Inc()
		return result, errors.Join(errList...)
	}

	pins, err := self.loadPins(ctx)
	if err != nil {
		egressProbePassErrorsTotal.WithLabelValues("pins").Inc()
		errList = append(errList, fmt.Errorf("load certificate pins: %w", err))
		return result, errors.Join(errList...)
	}
	pinSource := func() map[string][]string {
		return pins
	}
	// One pool snapshot for every batch of the pass, like the pins. A fetch
	// failure is never a reason to stop: the built-in table is always a
	// well-defined measurement, and the metric and log say it was used.
	var pool *egresshealth.Pool
	if self.loadPool != nil {
		var poolErr error
		pool, poolErr = self.loadPool(ctx)
		if poolErr != nil {
			egressProbePoolFetchesTotal.WithLabelValues("builtin").Inc()
			log.Printf("provider-egress task: shard=%d/%d destination pool unavailable, probing the built-in table: %s", args.ShardIndex, args.ShardCount, poolErr)
		} else {
			egressProbePoolFetchesTotal.WithLabelValues("server").Inc()
		}
	}
	poolSource := func() *egresshealth.Pool {
		return pool
	}
	if err := ctx.Err(); err != nil {
		egressProbePassErrorsTotal.WithLabelValues("canceled").Inc()
		errList = append(errList, err)
		return result, errors.Join(errList...)
	}

	blackholeConcurrency := args.Blackhole.Concurrency
	parallel := 0 < len(blackholeDue) && 0 < len(fullDue) &&
		args.Full.Concurrency < args.Blackhole.Concurrency
	applyBlackhole := func(outcome providerEgressBlackholeOutcome) {
		result.BlackholeDue = outcome.due
		result.Checked = outcome.checked
		result.Dark = outcome.dark
		result.TunnelFailed = outcome.tunnelFailed
		result.BlackholeNotMeasured = outcome.notMeasured
		result.BlackholeGuardTripped = outcome.guardTripped
		result.Full = result.Full || outcome.full
	}
	applyFull := func(outcome providerEgressFullOutcome) {
		result.Attempted = outcome.summary.Attempted
		result.Submitted = outcome.summary.Submitted
		result.Failed = outcome.summary.Failed
		result.FullNotMeasured = outcome.summary.NotMeasured
		result.FullGuardTripped = outcome.guardTripped
	}
	if parallel {
		// Full probes retain their configured pool. Reserving those slots from
		// blackhole work keeps the combined shard peak at the previously
		// configured blackhole peak while allowing the cheap lane to advance.
		blackholeConcurrency -= args.Full.Concurrency
		start := make(chan struct{})
		fullFinished := make(chan struct{})
		blackholeOutcomeCh := make(chan providerEgressBlackholeOutcome, 1)
		fullOutcomeCh := make(chan providerEgressFullOutcome, 1)
		go func() {
			<-start
			blackholeOutcomeCh <- self.drainBlackhole(
				ctx, args, pinSource, poolSource, blackholeConcurrency, blackholeDue, fullFinished,
			)
		}()
		go func() {
			<-start
			fullOutcomeCh <- self.runFullBatch(ctx, args, pinSource, poolSource, fullDue)
			close(fullFinished)
		}()
		close(start)

		blackholeOutcome := <-blackholeOutcomeCh
		fullOutcome := <-fullOutcomeCh
		applyBlackhole(blackholeOutcome)
		applyFull(fullOutcome)
		errList = append(errList, blackholeOutcome.err, fullOutcome.err)
	} else {
		// A one-slot configuration cannot overlap two lanes without exceeding
		// its prior peak. Preserve the old bounded order for that geometry.
		if 0 < len(blackholeDue) {
			blackholeOutcome := self.drainBlackhole(
				ctx, args, pinSource, poolSource, blackholeConcurrency, blackholeDue, nil,
			)
			applyBlackhole(blackholeOutcome)
			errList = append(errList, blackholeOutcome.err)
		}
		// Cancellation is different from an isolated batch failure: starting
		// more tunnels after task drain would extend shutdown and duplicate
		// work after the lease is recovered by another worker.
		if ctx.Err() == nil && 0 < len(fullDue) {
			fullOutcome := self.runFullBatch(ctx, args, pinSource, poolSource, fullDue)
			applyFull(fullOutcome)
			errList = append(errList, fullOutcome.err)
		}
	}
	if err := ctx.Err(); err != nil {
		egressProbePassErrorsTotal.WithLabelValues("canceled").Inc()
		errList = append(errList, err)
	}

	return result, errors.Join(errList...)
}

// One fresh task/probe-kind owner follows all drained batches. No unrelated
// task or full/blackhole sibling inherits this mutable admission budget.
func providerEgressProbeTunnelConfig(
	base providertunnel.Config,
	args ProviderEgressProbeBatchArgs,
) providertunnel.Config {
	byteCount, transportCount := args.TransportBudgetByteCount, args.TransportBudgetCount
	if byteCount == 0 && transportCount == 0 {
		limits := connect.DefaultPlatformTransportSettings().PlatformTransportBudget.Stats()
		byteCount, transportCount = limits.TotalByteCount, limits.MaxTransportCount
	}
	base.PlatformTransportBudget = connect.NewPlatformTransportBudget(byteCount, transportCount)
	return base
}

// The production scoring read: the pool as it stands, and the refresh's
// settings. A pool that cannot be read scores every load -- the stored counts
// are then the prober's, which is what they were before the pool existed --
// and broken settings keep their defaults for the tally, since the refresh
// itself refuses to run on them.
func loadProviderEgressHealthScoring(ctx context.Context) (scoring *model.ProviderEgressHealthScoring, settings *model.ProviderEgressSiteSettings) {
	settings = model.DefaultProviderEgressSiteSettings()
	if loaded, err := model.GetProviderEgressSiteSettings(); err == nil {
		settings = loaded
	} else {
		glog.Errorf("[egress]egress site settings are unusable (%s); the site tally uses the defaults\n", err)
	}
	if r := server.HandleError(func() {
		scoring = model.GetProviderEgressHealthScoring(ctx)
	}); r != nil {
		glog.Errorf("[egress]the destination pool could not be read for scoring (%v); every load of this batch counts\n", r)
		scoring = nil
	}
	return scoring, settings
}

// The production tally write. A failed write loses one run from the refresh's
// evidence and nothing else, so it is logged rather than allowed to fail the
// batch after its submissions landed.
func recordProviderEgressRunTally(ctx context.Context, measuredAt time.Time, run model.ProviderEgressRunTally, loads []model.ProviderEgressSiteLoad) {
	if r := server.HandleError(func() {
		model.AddProviderEgressRunTally(ctx, measuredAt, run, loads)
	}); r != nil {
		glog.Errorf("[egress]could not record a run in the site tally: %v\n", r)
	}
}

// Runtime-only identity, credentials, and clients are joined to the durable
// arguments immediately before the bounded pass begins.
func runProviderEgressProbe(
	ctx context.Context,
	args *ProviderEgressProbeArgs,
) (*ProviderEgressProbeResult, error) {
	identity := model.GetProberIdentity(ctx)
	if identity == nil || identity.NetworkId == nil || identity.ClientId == nil || identity.ByClientJwt == "" {
		return nil, fmt.Errorf("provider egress prober identity is not bootstrapped")
	}
	operatorSecret, err := readProviderEgressOperatorSecret()
	if err != nil {
		return nil, err
	}
	operator := &ingest.Client{
		ServerUrl:      args.APIURL,
		OperatorSecret: operatorSecret,
		ShardIndex:     args.ShardIndex,
		ShardCount:     args.ShardCount,
		Http:           controlplane.NewHTTPClient(30 * time.Second),
	}
	tunnelConfig := providertunnel.Config{
		ApiUrl:            args.APIURL,
		PlatformUrl:       args.PlatformURL,
		ByJwt:             identity.ByClientJwt,
		ClientId:          connect.Id(*identity.ClientId),
		DeviceDescription: model.ProberClientDescription,
		DeviceSpec:        model.ProberClientDeviceSpec,
		Version:           server.RequireVersion(),
	}

	// every finding the prober submits passes through the metrics reporter
	// first (see provider_egress_probe_metrics.go), then to the operator
	reporter := newEgressProbeMetricsReporter(operator, lookupProviderEgressCountry)
	readiness := newProviderEgressProbeReadiness(*identity.NetworkId)

	var bandwidthSampler *bandwidth.Sampler
	bandwidthTargets := []bandwidth.Target{}
	if args.Full.Bandwidth {
		if strings.TrimSpace(args.PublicAPIURL) != "" {
			bandwidthTargets = append(bandwidthTargets, bandwidth.OperatorTarget(args.PublicAPIURL, operatorSecret))
		}
		cdnURL := args.BandwidthCDNURL
		if strings.TrimSpace(cdnURL) == "" {
			cdnURL = bandwidth.CdnTestUrl
		}
		bandwidthTargets = append(bandwidthTargets, bandwidth.Target{
			Name:   "cdn",
			Source: bandwidth.SourceCdn,
			Url:    cdnURL,
		})
		bandwidthSampler = &bandwidth.Sampler{
			Targets: bandwidthTargets,
			Reserve: reporter,
			Submit:  reporter,
			Timeout: time.Duration(args.Full.BandwidthTimeoutSeconds) * time.Second,
		}
	}

	pass := &providerEgressProbePass{
		readiness:    readiness,
		blackholeDue: operator.BlackholeDue,
		fullDue:      operator.Due,
		loadPins: func(ctx context.Context) (map[string][]string, error) {
			servedPins, err := operator.GeolocationPins(ctx)
			if err != nil {
				return nil, err
			}
			return fleetprobe.ValidatePins(servedPins)
		},
		loadPool: func(ctx context.Context) (*egresshealth.Pool, error) {
			return fleetprobe.LoadPool(ctx, operator.Http, fleetprobe.PoolUrl(args.APIURL), operatorSecret)
		},
		loadScoring:           loadProviderEgressHealthScoring,
		submitBlackholeChecks: reporter.SubmitBlackholeChecks,
		blackholeOptions: fleetprobe.BlackholeOptions{
			TunnelConfig: providerEgressProbeTunnelConfig(tunnelConfig, args.Blackhole),
		},
		fullOptions: fleetprobe.FullOptions{
			TunnelConfig:   providerEgressProbeTunnelConfig(tunnelConfig, args.Full),
			Bandwidth:      bandwidthSampler,
			BandwidthHosts: bandwidth.TargetHosts(bandwidthTargets),
		},
		fullSink:     reporter,
		recordTally:  recordProviderEgressRunTally,
		runBlackhole: fleetprobe.RunBlackhole,
		runFull:      fleetprobe.RunFull,
		refreshFleet: refreshEgressProbeFleetMetrics,
	}
	result, err := pass.run(ctx, args)
	if result == nil {
		return nil, err
	}

	log.Printf(
		"provider-egress task: shard=%d/%d full_due=%d attempted=%d submitted=%d failed=%d not_measured=%d guard=%t blackhole_due=%d checked=%d dark=%d tunnel_failed=%d not_measured=%d guard_trips=%d backlog=%t",
		args.ShardIndex,
		args.ShardCount,
		result.FullDue,
		result.Attempted,
		result.Submitted,
		result.Failed,
		result.FullNotMeasured,
		result.FullGuardTripped,
		result.BlackholeDue,
		result.Checked,
		result.Dark,
		result.TunnelFailed,
		result.BlackholeNotMeasured,
		result.BlackholeGuardTripped,
		result.Full,
	)
	return result, err
}
