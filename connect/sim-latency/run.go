package main

// `sim-latency run`: stand up the environment and drive the load.
//
// Order matters: the sim region and ip_overrides settings are installed before
// the services start (so provider connections geolocate to the region), the
// fleet ramps and settles (so the reliability pipeline makes providers
// selectable) before the measured window, and only per-request performance
// stats go to stdout.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/stats"
)

type RunOptions struct {
	ConfigPath string
	SiteHome   string
	Ramp       time.Duration
	// reliability history backfilled so providers are established without the
	// ~8.4h cold-start warm-up (0 = pure organic warm-up)
	Prewarm  time.Duration
	Settle   time.Duration
	Duration time.Duration
	// hard deadline for establishing every long-lived client before measurement
	ClientWarmupTimeout time.Duration
	// deadline for a crawl and the score charged to every failed/incomplete
	// observation
	RequestTimeout time.Duration
	FleetShards    int
	// site listen address (providers egress here over loopback)
	SiteListen string
	Services   *ServicesConfig
	// run.json side-car output ("" = none): the run identity + metric
	// summaries the comparison tooling consumes
	MetaPath string
	// clear cross-run reliability state first, so this run is an
	// independent replicate (see reset.go)
	Reset bool

	// official evaluation identity and immutable-build enforcement
	EvaluationId     string
	Official         bool
	ExpectedRevision string
	ResourceReport   string
	AccountingReport string
	AccountingSource string
	FinalMarker      string
}

const apexScoreSchema = 1
const officialScorerVersion = "sim-latency-score/1"
const teardownTimeout = 20 * time.Second

var runEvaluationIdPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// EvaluationIncompleteError is a typed run failure. Any caller that receives
// it must preserve artifacts for diagnosis but must not produce a placeable
// score.
type EvaluationIncompleteError struct {
	Code  string
	Phase string
	Err   error
}

func (self *EvaluationIncompleteError) Error() string {
	if self.Phase == "" {
		return fmt.Sprintf("incomplete evaluation (%s): %v", self.Code, self.Err)
	}
	return fmt.Sprintf("incomplete evaluation (%s) during %s: %v", self.Code, self.Phase, self.Err)
}

func (self *EvaluationIncompleteError) Unwrap() error { return self.Err }

func incompleteError(code string, phase string, err error) error {
	if err == nil {
		err = errors.New("evaluation did not complete")
	}
	return &EvaluationIncompleteError{Code: code, Phase: phase, Err: err}
}

func phaseError(ctx context.Context, code string, phase string, err error) error {
	if ctx.Err() != nil {
		return incompleteError("interrupted", phase, ctx.Err())
	}
	return incompleteError(code, phase, err)
}

func waitPhase(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		return nil
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func Run(options *RunOptions) (retErr error) {
	if options == nil {
		return incompleteError("invalid_options", "setup", errors.New("nil run options"))
	}
	if options.RequestTimeout <= 0 {
		if options.Official {
			return incompleteError("invalid_options", "setup", errors.New("official request timeout must be positive"))
		}
		options.RequestTimeout = 2 * time.Minute
	}
	if options.ClientWarmupTimeout <= 0 {
		if options.Official {
			return incompleteError("invalid_options", "setup", errors.New("official client warm-up timeout must be positive"))
		}
		options.ClientWarmupTimeout = 30 * time.Minute
	}
	if options.RequestTimeout.Milliseconds() <= 0 || options.Duration.Milliseconds() <= 0 ||
		options.ClientWarmupTimeout.Milliseconds() <= 0 ||
		options.Ramp < 0 || options.Prewarm < 0 || options.Settle < 0 || options.FleetShards < 0 {
		return incompleteError("invalid_options", "setup", errors.New("run durations and fleet shard count are invalid"))
	}
	if options.Services != nil {
		if err := validateServicesConfig(options.Services); err != nil {
			return incompleteError("invalid_options", "setup", err)
		}
	}
	if options.Official && options.EvaluationId == "" {
		return incompleteError("invalid_options", "setup", errors.New("official run requires --evaluation-id"))
	}
	if options.EvaluationId == "" {
		options.EvaluationId = server.NewId().String()
	}
	if !runEvaluationIdPattern.MatchString(options.EvaluationId) {
		return incompleteError("invalid_options", "setup", errors.New("evaluation id has invalid characters or length"))
	}
	if options.FinalMarker == "" && options.MetaPath != "" {
		options.FinalMarker = options.MetaPath + ".complete.json"
	}
	if options.Official {
		if !options.Reset {
			return incompleteError("invalid_options", "setup", errors.New("official run requires --reset"))
		}
		if options.MetaPath == "" || options.FinalMarker == "" ||
			options.ResourceReport == "" || options.AccountingReport == "" ||
			options.AccountingSource == "" {
			return incompleteError("invalid_options", "setup", errors.New("official run requires every artifact path"))
		}
		if options.ExpectedRevision == "" {
			return incompleteError("invalid_options", "setup", errors.New("official run requires --expected-revision"))
		}
	}
	setLogEvaluationId(options.EvaluationId)
	defer setLogEvaluationId("")

	config, err := LoadConfig(options.ConfigPath)
	if err != nil {
		return incompleteError("invalid_workload", "load providers", err)
	}
	if err := config.validate(); err != nil {
		return incompleteError("invalid_workload", "validate providers", err)
	}

	ctx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()

	runStats, err := newRunStats(options, config)
	if err != nil {
		return incompleteError("manifest_init_failed", "setup", err)
	}
	if options.FinalMarker != "" {
		// A marker from a prior job must never make an interrupted rerun appear
		// complete. The target is the exact per-job path, never a broad glob.
		if err := os.Remove(options.FinalMarker); err != nil && !errors.Is(err, os.ErrNotExist) {
			return incompleteError("stale_marker_cleanup_failed", "setup", err)
		}
	}

	var driver *ClientDriver
	var driverDone <-chan error
	var fleet *Fleet
	var fleetProcs []*fleetProcess
	var services *Services
	var site *Site
	var statsHandle *stats.Stats
	var providerEgressStart int64
	var providerEgressEnd int64
	providerAccountingStarted := false
	providerAccountingComplete := false
	statsCtx, cancelStats := context.WithCancel(context.Background())

	// The sole finalizer owns teardown and artifact completion. It always runs,
	// including after a setup error or TERM, and changes a would-be success into
	// a typed incomplete result if any drain/durability check fails.
	defer func() {
		stopSignals()
		var cleanupErr error

		if driverDone != nil {
			cleanupErr = errors.Join(cleanupErr, waitAsync("client driver", driverDone, teardownTimeout))
		}
		if driver != nil {
			driver.Close()
			// Run flushes on its normal cancellation path. Setup failures (most
			// notably an incomplete warm pool) never enter Run, so flush again in
			// the sole lifecycle finalizer. This is idempotent and keeps the
			// sidecar's in-memory CSV identity equal to the bytes on stdout even
			// for failed-closed evaluations.
			cleanupErr = errors.Join(cleanupErr, driver.flush())
		}
		if 0 < len(fleetProcs) {
			cleanupErr = errors.Join(cleanupErr, stopFleetShards(fleetProcs, teardownTimeout))
		}
		if fleet != nil {
			cleanupErr = errors.Join(cleanupErr, waitCall("in-process fleet", fleet.Wait, teardownTimeout))
		}
		if site != nil {
			cleanupErr = errors.Join(cleanupErr, site.Close())
			cleanupErr = errors.Join(cleanupErr, waitCall("fake site", site.Wait, teardownTimeout))
		}
		if services != nil {
			cleanupErr = errors.Join(cleanupErr, waitCall("services", services.Close, teardownTimeout))
		}
		if statsHandle != nil {
			cleanupErr = errors.Join(cleanupErr, statsHandle.Close())
		}
		cancelStats()

		if cleanupErr != nil {
			if retErr == nil {
				retErr = incompleteError("unclean_teardown", "teardown", cleanupErr)
			} else {
				retErr = fmt.Errorf("%w; teardown: %v", retErr, cleanupErr)
			}
		}

		if options.AccountingSource != "" && retErr == nil {
			if !providerAccountingStarted || !providerAccountingComplete {
				retErr = incompleteError(
					"accounting_incomplete",
					"artifact finalization",
					errors.New("provider accounting did not cover both measurement boundaries"),
				)
			} else {
				accountingSha256, accountingBytes, accountingErr := writeAccountingSource(
					options.AccountingSource,
					runStats,
					providerEgressStart,
					providerEgressEnd,
				)
				if accountingErr != nil {
					retErr = incompleteError("accounting_write_failed", "artifact finalization", accountingErr)
				} else {
					runStats.AccountingSourceSha256 = accountingSha256
					runStats.AccountingSourceBytes = accountingBytes
				}
			}
		}

		if driver != nil {
			summarizeRows(driver.resultRows(), runStats)
			runStats.ResultsCsvSha256, runStats.ResultsCsvBytes = driver.csvIdentity()
		} else if runStats.Metrics == nil {
			runStats.Metrics = map[string]MetricSummary{}
		}
		runStats.CompletedUnixMs = server.NowUtc().UnixMilli()
		runStats.CompletionState = "complete"
		runStats.IncompleteCode = ""
		runStats.IncompleteMessage = ""
		if retErr != nil {
			runStats.CompletionState = "incomplete"
			runStats.IncompleteCode = "run_failed"
			runStats.IncompleteMessage = retErr.Error()
			var incomplete *EvaluationIncompleteError
			if errors.As(retErr, &incomplete) {
				runStats.IncompleteCode = incomplete.Code
			}
		}

		if options.MetaPath == "" {
			if options.Official && retErr == nil {
				retErr = incompleteError("missing_manifest_path", "artifact finalization", errors.New("official run requires --meta"))
			}
			return
		}
		if err := writeRunStats(options.MetaPath, runStats); err != nil {
			retErr = incompleteError("manifest_write_failed", "artifact finalization", err)
			return
		}
		if retErr != nil {
			return
		}
		if options.FinalMarker == "" {
			retErr = incompleteError("missing_final_marker_path", "artifact finalization", errors.New("no final marker path"))
			return
		}
		if err := writeFinalMarker(options.FinalMarker, options.MetaPath, runStats); err != nil {
			retErr = incompleteError("final_marker_write_failed", "artifact finalization", err)
			runStats.CompletionState = "incomplete"
			runStats.IncompleteCode = "final_marker_write_failed"
			runStats.IncompleteMessage = retErr.Error()
			_ = writeRunStats(options.MetaPath, runStats)
			return
		}
		logf("run summary written to %s", options.MetaPath)
		logf("completion marker written to %s", options.FinalMarker)
		logRunSummary(runStats)
	}()
	// This defer is registered after the lifecycle finalizer, so it runs first
	// during panic unwinding and marks the result incomplete before teardown is
	// allowed to consider writing a completion marker.
	defer func() {
		if recovered := recover(); recovered != nil {
			logf("evaluation panic: %v\n%s", recovered, debug.Stack())
			retErr = incompleteError("panic", "run", fmt.Errorf("%v", recovered))
		}
	}()

	// Cancellation is explicit so normal measurement completion and signal
	// interruption share the exact same joined teardown path.
	cancelRun := func() { stopSignals() }

	// bring the local database schema up to date (no-op once migrated)
	logf("applying db migrations")
	server.ApplyDbMigrations(ctx)
	if ctx.Err() != nil {
		return incompleteError("interrupted", "db migrations", ctx.Err())
	}

	if options.Reset {
		resetLocalState(ctx)
		if ctx.Err() != nil {
			return incompleteError("interrupted", "state reset", ctx.Err())
		}
	}

	// install the site settings (ip_overrides + stats knobs) before anything
	// geolocates a fake ip
	if err := writeSiteSettings(options.SiteHome, config); err != nil {
		return phaseError(ctx, "site_settings_failed", "site setup", err)
	}
	statsHandle = stats.Enable(statsCtx, nil)
	runStats.StatsRoot = statsHandle.Root()
	runStats.StatsInstanceId = statsHandle.InstanceId().String()
	logf("stats enabled=%t instance=%s", statsHandle.Enabled(), statsHandle.InstanceId())
	if options.Official && !statsHandle.Enabled() {
		return incompleteError("stats_disabled", "stats setup", errors.New("official evaluation requires stats"))
	}
	if err := validateOfficialBuild(runStats, options); err != nil {
		return incompleteError("unpinned_build", "build validation", err)
	}

	// sim region + provider/client identities
	locationId, err := provisionRegion(ctx, config.Region)
	if err != nil {
		return phaseError(ctx, "region_provision_failed", "provision region", err)
	}
	logf("sim region country location=%s", locationId)

	if err := provisionProviders(ctx, config.Fleet, locationId, config.Region.CountryCode); err != nil {
		return phaseError(ctx, "provider_provision_failed", "provision providers", err)
	}
	pool, err := provisionClientPool(ctx, config)
	if err != nil {
		return phaseError(ctx, "client_provision_failed", "provision clients", err)
	}

	// services
	servicesConfig := options.Services
	if servicesConfig == nil {
		servicesConfig = DefaultServicesConfig()
	}
	services, err = NewServices(ctx, servicesConfig)
	if err != nil {
		return phaseError(ctx, "services_start_failed", "start services", err)
	}
	logf("services up api=%s ws=%v", services.ApiUrl(), services.WsUrls())

	// fake site
	site, err = NewSite(ctx, options.SiteListen, config.Seed, config.Site)
	if err != nil {
		return phaseError(ctx, "site_start_failed", "start fake site", err)
	}
	logf("fake site at %s", site.Addr())

	// fleet: in-process, or sharded into subprocesses
	if 0 < options.FleetShards {
		fleetProcs, err = spawnFleetShards(options, config, services)
		if err != nil {
			return phaseError(ctx, "fleet_start_failed", "start fleet shards", err)
		}
	} else {
		fleet, err = NewFleet(ctx, config, config.Fleet, services.ApiUrl(), services.WsUrls(), services.WsPorts(), options.Ramp)
		if err != nil {
			return phaseError(ctx, "fleet_start_failed", "start in-process fleet", err)
		}
	}

	// ramp: stagger provider connects, then give the announce loop a moment to
	// register connections + locations before the pipeline reads them.
	logf(
		"ramp=%s prewarm=%s settle=%s client-warmup-timeout=%s then measure=%s",
		options.Ramp,
		options.Prewarm,
		options.Settle,
		options.ClientWarmupTimeout,
		options.Duration,
	)
	rampWait := options.Ramp + 15*time.Second
	if err := waitPhase(ctx, rampWait); err != nil {
		return incompleteError("interrupted", "ramp", err)
	}

	// prewarm: backfill reliability history so the established market exists
	// without waiting the ~8.4h the 12h-lookback gate needs from cold. Then run
	// the pipeline once so providers are selectable, and let a short settle
	// propagate the redis sample export.
	if 0 < options.Prewarm {
		logf("prewarming: establishing the connected fleet (~%s reliability window)", options.Prewarm)
		if err := provisionPrewarm(ctx, options.Prewarm, config.Fleet, services); err != nil {
			return phaseError(ctx, "prewarm_failed", "prewarm", err)
		}
		logf("prewarm complete; running pipeline")
		services.RunPipelineOnce(ctx)
	}

	if err := waitPhase(ctx, options.Settle); err != nil {
		return incompleteError("interrupted", "settle", err)
	}

	// build the warm client pool during warm-up (before the measured window),
	// so pool-setup time is not counted
	driver = NewClientDriver(
		ctx,
		config,
		services.ApiUrl(),
		services.WsUrls(),
		site.Addr(),
		locationId,
		pool,
		options.RequestTimeout,
	)
	warmupCtx, cancelWarmup := context.WithTimeout(ctx, options.ClientWarmupTimeout)
	warmupErr := driver.Warmup(warmupCtx)
	cancelWarmup()
	runStats.ClientsPool = len(pool)
	runStats.ClientsEstablished = driver.EstablishedCount()
	if warmupErr != nil {
		if ctx.Err() != nil {
			return incompleteError("interrupted", "warm client construction", ctx.Err())
		}
		if errors.Is(warmupErr, context.DeadlineExceeded) {
			return incompleteError(
				"client_warmup_timeout",
				"warm client construction",
				fmt.Errorf(
					"established %d/%d warm clients within %s: %w",
					driver.EstablishedCount(),
					len(pool),
					options.ClientWarmupTimeout,
					warmupErr,
				),
			)
		}
		return incompleteError("warm_pool_incomplete", "warm client construction", warmupErr)
	}
	if ctx.Err() != nil {
		return incompleteError("interrupted", "warm client construction", ctx.Err())
	}
	if driver.EstablishedCount() != len(pool) {
		return incompleteError(
			"warm_pool_incomplete",
			"warm client construction",
			fmt.Errorf("established %d/%d warm clients", driver.EstablishedCount(), len(pool)),
		)
	}

	// client driver, measured window
	if err := startProviderDynamics(ctx, fleet, fleetProcs); err != nil {
		return incompleteError("provider_dynamics_start_failed", "measurement setup", err)
	}
	providerEgressStart, err = providerEgressSnapshot(fleet, fleetProcs)
	if err != nil {
		return incompleteError("accounting_start_failed", "measurement setup", err)
	}
	providerAccountingStarted = true
	measureStart := server.NowUtc()
	measureEnd := measureStart.Add(options.Duration)
	runStats.MeasureStartMs = measureStart.UnixMilli()
	runStats.MeasureEndMs = measureEnd.UnixMilli()
	logf("MEASURE WINDOW: [%d, %d] unix-ms", measureStart.UnixMilli(), measureEnd.UnixMilli())
	probeTimeout := min(options.Duration, 10*time.Second)
	probeCtx, probeCancel := context.WithTimeout(ctx, probeTimeout)
	probeErr := driver.ProbeMatchmaking(probeCtx)
	probeCancel()
	if probeErr != nil {
		return incompleteError("matchmaking_probe_failed", "measurement setup", probeErr)
	}
	driverDoneMutable := make(chan error, 1)
	driverDone = driverDoneMutable
	go func() {
		driverDoneMutable <- runDriver(driver)
		close(driverDoneMutable)
	}()

	measureRemaining := measureEnd.Sub(server.NowUtc())
	if measureRemaining <= 0 {
		return incompleteError(
			"matchmaking_probe_timeout",
			"measurement setup",
			fmt.Errorf("matchmaking probe consumed the %s measured window", options.Duration),
		)
	}
	measureTimer := time.NewTimer(measureRemaining)
	defer measureTimer.Stop()
	select {
	case <-measureTimer.C:
		logf("measure window complete; stopping arrivals and draining admitted crawls")
	case <-ctx.Done():
		return incompleteError("interrupted", "measured window", ctx.Err())
	case driverErr := <-driverDone:
		driverDone = nil
		if driverErr == nil {
			driverErr = errors.New("client driver stopped before measure window ended")
		}
		return incompleteError("client_driver_stopped", "measured window", driverErr)
	}

	// Closing admission and canceling the run are intentionally separate. A
	// crawl whose arrival precedes measureEnd belongs to the sample and must be
	// allowed to finish (or reach its own RequestTimeout). Canceling ctx here
	// would manufacture status=0 rows at the duration boundary and bias the
	// failure-charged score upward.
	driver.StopArrivals()
	drainTimeout := options.RequestTimeout + teardownTimeout
	if err := waitAsync("client driver", driverDone, drainTimeout); err != nil {
		return incompleteError("client_driver_drain_failed", "measurement drain", err)
	}
	driverDone = nil
	if ctx.Err() != nil {
		return incompleteError("interrupted", "measurement drain", ctx.Err())
	}
	providerEgressEnd, err = providerEgressSnapshot(fleet, fleetProcs)
	if err != nil {
		return incompleteError("accounting_end_failed", "measurement drain", err)
	}
	providerAccountingComplete = true
	cancelRun()
	return nil
}

// newRunStats seeds the run.json side-car with the run's identity — the
// exact workload (providers.yml sha), build, host, and flags — so two
// artifacts can be checked for comparability. The window, client counts,
// and metrics are filled in as the run progresses.
func newRunStats(options *RunOptions, config *Config) (*RunStats, error) {
	configBytes, err := os.ReadFile(options.ConfigPath)
	if err != nil {
		return nil, err
	}
	hostname, _ := os.Hostname()
	revision, modified := buildFingerprint()
	servicesConfig := options.Services
	if servicesConfig == nil {
		servicesConfig = DefaultServicesConfig()
	}
	return &RunStats{
		Schema:           runStatsSchema,
		Kind:             runStatsKind,
		ScoreSchema:      apexScoreSchema,
		ScorerVersion:    officialScorerVersion,
		EvaluationId:     options.EvaluationId,
		Official:         options.Official,
		RequestTimeoutMs: options.RequestTimeout.Milliseconds(),
		ResourceReport:   options.ResourceReport,
		AccountingReport: options.AccountingReport,
		AccountingSource: options.AccountingSource,
		FinalMarker:      options.FinalMarker,
		CompletionState:  "running",
		ConfigSha256:     fmt.Sprintf("%x", sha256.Sum256(configBytes)),
		Seed:             config.Seed,
		BuildRevision:    revision,
		BuildModified:    modified,
		Hostname:         hostname,
		Os:               runtime.GOOS,
		Arch:             runtime.GOARCH,
		NumCpu:           runtime.NumCPU(),
		Flags: map[string]string{
			"ramp":                  options.Ramp.String(),
			"prewarm":               options.Prewarm.String(),
			"settle":                options.Settle.String(),
			"client_warmup_timeout": options.ClientWarmupTimeout.String(),
			"duration":              options.Duration.String(),
			"request_timeout":       options.RequestTimeout.String(),
			"fleet_shards":          intToStr(options.FleetShards),
			"site_listen":           options.SiteListen,
			"hosts":                 intToStr(servicesConfig.HostCount),
			"api_port":              intToStr(servicesConfig.ApiPort),
			"ws_port_base":          intToStr(servicesConfig.WsPortBase),
			"exchange_port_base":    intToStr(servicesConfig.ExchangePortBase),
			"pipeline_interval":     servicesConfig.PipelineInterval.String(),
			"test_timeout":          servicesConfig.SpeedTestTimeout.String(),
			"announce_timeout":      servicesConfig.AnnounceTimeout.String(),
			"forward_idle_timeout":  servicesConfig.ForwardIdleTimeout.String(),
			"impair":                fmt.Sprintf("%t", impairEnabled),
			"reset":                 fmt.Sprintf("%t", options.Reset),
		},
	}, nil
}

func validateOfficialBuild(runStats *RunStats, options *RunOptions) error {
	if !options.Official {
		return nil
	}
	if options.ExpectedRevision == "" {
		return errors.New("--official requires --expected-revision")
	}
	if runStats.BuildRevision == "" {
		return errors.New("binary carries no VCS revision")
	}
	if runStats.BuildModified {
		return errors.New("binary was built from a modified worktree")
	}
	if runStats.BuildRevision != options.ExpectedRevision {
		return fmt.Errorf(
			"binary revision %s does not match pinned revision %s",
			runStats.BuildRevision,
			options.ExpectedRevision,
		)
	}
	return nil
}

func runDriver(driver *ClientDriver) (retErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			logf("client driver panic: %v\n%s", recovered, debug.Stack())
			retErr = fmt.Errorf("client driver panic: %v", recovered)
		}
	}()
	return driver.Run()
}

func waitAsync(name string, done <-chan error, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err, ok := <-done:
		if !ok {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		return nil
	case <-timer.C:
		return fmt.Errorf("%s did not drain within %s", name, timeout)
	}
}

func waitCall(name string, call func() error, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- call() }()
	return waitAsync(name, done, timeout)
}

type providerAccountingSource struct {
	Schema              int    `json:"schema"`
	Kind                string `json:"kind"`
	EvaluationId        string `json:"evaluation_id"`
	Complete            bool   `json:"complete"`
	MeasureStartMs      int64  `json:"measure_start_ms"`
	MeasureEndMs        int64  `json:"measure_end_ms"`
	ProviderEgressBytes int64  `json:"provider_egress_bytes"`
	CounterStartBytes   int64  `json:"counter_start_bytes"`
	CounterEndBytes     int64  `json:"counter_end_bytes"`
	CounterKind         string `json:"counter_kind"`
}

// Writes the fixed simulator's independent provider return-path counters.
// The evaluator authenticates this source and derives accounting.json outside
// the candidate container; the scorer never consumes this file directly.
func writeAccountingSource(
	path string,
	runStats *RunStats,
	providerEgressStart int64,
	providerEgressEnd int64,
) (string, int64, error) {
	if path == "" || runStats == nil || runStats.EvaluationId == "" ||
		runStats.MeasureStartMs <= 0 || runStats.MeasureEndMs < runStats.MeasureStartMs {
		return "", 0, errors.New("provider accounting identity or window is incomplete")
	}
	if providerEgressStart < 0 || providerEgressEnd < providerEgressStart {
		return "", 0, errors.New("provider egress counter regressed")
	}
	source := providerAccountingSource{
		Schema:              1,
		Kind:                "sim-latency-provider-accounting-source",
		EvaluationId:        runStats.EvaluationId,
		Complete:            true,
		MeasureStartMs:      runStats.MeasureStartMs,
		MeasureEndMs:        runStats.MeasureEndMs,
		ProviderEgressBytes: providerEgressEnd - providerEgressStart,
		CounterStartBytes:   providerEgressStart,
		CounterEndBytes:     providerEgressEnd,
		CounterKind:         "provider_remote_egress_packet_bytes",
	}
	sourceBytes, err := json.MarshalIndent(&source, "", "  ")
	if err != nil {
		return "", 0, err
	}
	sourceBytes = append(sourceBytes, '\n')
	if err := writeExclusiveAtomicFile(path, sourceBytes, 0600); err != nil {
		return "", 0, err
	}
	digest := sha256.Sum256(sourceBytes)
	return fmt.Sprintf("%x", digest), int64(len(sourceBytes)), nil
}

// Links a fully flushed temporary file into place so an existing path is
// never overwritten and an interrupted write cannot become authoritative.
func writeExclusiveAtomicFile(path string, content []byte, mode os.FileMode) (retErr error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".sim-latency-exclusive-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Link(tmpPath, path); err != nil {
		return err
	}
	dirHandle, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := dirHandle.Sync(); err != nil {
		dirHandle.Close()
		return err
	}
	return dirHandle.Close()
}

type runFinalMarker struct {
	Schema            int    `json:"schema"`
	Kind              string `json:"kind"`
	ScoreSchema       int    `json:"score_schema"`
	ScorerVersion     string `json:"scorer_version"`
	EvaluationId      string `json:"evaluation_id"`
	RunManifestSha256 string `json:"run_manifest_sha256"`
	RunManifestBytes  int64  `json:"run_manifest_bytes"`
	CompletedUnixMs   int64  `json:"completed_unix_ms"`
}

func writeFinalMarker(markerPath string, metaPath string, runStats *RunStats) error {
	manifestBytes, err := os.ReadFile(metaPath)
	if err != nil {
		return fmt.Errorf("read finalized run manifest: %w", err)
	}
	marker := runFinalMarker{
		Schema:            1,
		Kind:              "sim-latency-complete",
		ScoreSchema:       apexScoreSchema,
		ScorerVersion:     officialScorerVersion,
		EvaluationId:      runStats.EvaluationId,
		RunManifestSha256: fmt.Sprintf("%x", sha256.Sum256(manifestBytes)),
		RunManifestBytes:  int64(len(manifestBytes)),
		CompletedUnixMs:   runStats.CompletedUnixMs,
	}
	markerBytes, err := json.MarshalIndent(&marker, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomicFile(markerPath, append(markerBytes, '\n'), 0o644)
}

func writeAtomicFile(path string, content []byte, mode os.FileMode) (retErr error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".sim-latency-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		if retErr != nil {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	dirHandle, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := dirHandle.Sync(); err != nil {
		dirHandle.Close()
		return err
	}
	if err := dirHandle.Close(); err != nil {
		return err
	}
	return nil
}

// buildFingerprint returns the vcs revision baked into the binary (empty for
// a non-vcs build).
func buildFingerprint() (string, bool) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", false
	}
	revision := ""
	modified := false
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	return revision, modified
}

// logRunSummary logs the measured metrics to stderr (stdout carries only the
// CSV).
func logRunSummary(stats *RunStats) {
	logf("measured: %d rows in window, %d failures", stats.RowsInWindow, stats.Failures)
	for _, def := range metricDefs() {
		if summary, ok := stats.Metrics[def.name]; ok {
			mark := ""
			if def.primary {
				mark = " (primary)"
			}
			logf("  %s = %s ± %s%s", def.name, formatValue(summary.Value), formatValue(summary.BlockSe), mark)
		}
	}
}

const fleetAccountingSnapshotCommand = "snapshot"
const fleetDynamicsStartCommand = "start-dynamics"
const fleetAccountingSnapshotTimeout = 5 * time.Second

// Serves the private parent/child measurement-control pipe. Snapshot never
// resets counters, and repeated dynamics starts are idempotent, so retrying a
// command cannot redraw workload state or double count bytes.
func serveFleetAccounting(
	commandReader io.Reader,
	responseWriter io.Writer,
	snapshot func() int64,
	startDynamics func() error,
) error {
	if commandReader == nil || responseWriter == nil || snapshot == nil || startDynamics == nil {
		return errors.New("fleet accounting protocol is incomplete")
	}
	scanner := bufio.NewScanner(commandReader)
	writer := bufio.NewWriter(responseWriter)
	for scanner.Scan() {
		var response int64
		switch scanner.Text() {
		case fleetAccountingSnapshotCommand:
			response = snapshot()
			if response < 0 {
				return errors.New("fleet accounting counter is negative")
			}
		case fleetDynamicsStartCommand:
			if err := startDynamics(); err != nil {
				return fmt.Errorf("start fleet dynamics: %w", err)
			}
			response = 0
		default:
			return fmt.Errorf("unknown fleet accounting command %q", scanner.Text())
		}
		if _, err := fmt.Fprintf(writer, "%d\n", response); err != nil {
			return err
		}
		if err := writer.Flush(); err != nil {
			return err
		}
	}
	return scanner.Err()
}

// Starts the child side of the private accounting pipe descriptors inherited
// from the parent run process. Both descriptors must be absent or both valid.
func startFleetAccountingServer(
	ctx context.Context,
	fleet *Fleet,
	commandFd int,
	responseFd int,
) (<-chan error, error) {
	if commandFd == 0 && responseFd == 0 {
		return nil, nil
	}
	if ctx == nil || fleet == nil || commandFd < 3 || responseFd < 3 || commandFd == responseFd {
		return nil, errors.New("fleet accounting descriptors are invalid")
	}
	commandFile := os.NewFile(uintptr(commandFd), "fleet-accounting-command")
	responseFile := os.NewFile(uintptr(responseFd), "fleet-accounting-response")
	if commandFile == nil || responseFile == nil {
		if commandFile != nil {
			commandFile.Close()
		}
		if responseFile != nil {
			responseFile.Close()
		}
		return nil, errors.New("fleet accounting descriptors are unavailable")
	}
	done := make(chan error, 1)
	go func() {
		defer close(done)
		defer commandFile.Close()
		defer responseFile.Close()
		err := serveFleetAccounting(
			commandFile,
			responseFile,
			fleet.ProviderEgressByteCount,
			func() error { return fleet.StartDynamics(ctx) },
		)
		if errors.Is(err, io.EOF) || (err == nil && ctx.Err() != nil) {
			err = nil
		}
		done <- err
	}()
	return done, nil
}

type fleetAccountingResponse struct {
	byteCount int64
	err       error
}

type fleetProcess struct {
	cmd                        *exec.Cmd
	index                      int
	done                       chan struct{}
	accountingCommandWriter    *os.File
	accountingResponseReader   *os.File
	accountingResponses        chan fleetAccountingResponse
	accountingCommandCloseOnce sync.Once
	lock                       sync.Mutex
	err                        error
}

func (self *fleetProcess) reap() {
	err := self.cmd.Wait()
	self.lock.Lock()
	self.err = err
	self.lock.Unlock()
	close(self.done)
}

func (self *fleetProcess) readAccountingResponses() {
	defer close(self.accountingResponses)
	defer self.accountingResponseReader.Close()
	scanner := bufio.NewScanner(self.accountingResponseReader)
	for scanner.Scan() {
		byteCount, err := strconv.ParseInt(scanner.Text(), 10, 64)
		if err == nil && byteCount < 0 {
			err = errors.New("fleet accounting response is negative")
		}
		self.accountingResponses <- fleetAccountingResponse{
			byteCount: byteCount,
			err:       err,
		}
		if err != nil {
			return
		}
	}
	if err := scanner.Err(); err != nil {
		self.accountingResponses <- fleetAccountingResponse{err: err}
	}
}

func (self *fleetProcess) closeAccountingCommand() {
	self.accountingCommandCloseOnce.Do(func() {
		_ = self.accountingCommandWriter.Close()
	})
}

func (self *fleetProcess) waitError() error {
	<-self.done
	self.lock.Lock()
	defer self.lock.Unlock()
	return self.err
}

func (self *fleetProcess) finished() bool {
	select {
	case <-self.done:
		return true
	default:
		return false
	}
}

func providerEgressSnapshot(fleet *Fleet, procs []*fleetProcess) (int64, error) {
	if fleet != nil {
		if 0 < len(procs) {
			return 0, errors.New("both in-process and sharded fleets are active")
		}
		byteCount := fleet.ProviderEgressByteCount()
		if byteCount < 0 {
			return 0, errors.New("in-process provider egress counter is negative")
		}
		return byteCount, nil
	}
	if len(procs) == 0 {
		return 0, errors.New("provider fleet is unavailable")
	}
	for _, proc := range procs {
		if proc.finished() {
			return 0, fmt.Errorf("fleet shard %d exited before accounting snapshot", proc.index)
		}
		if _, err := io.WriteString(proc.accountingCommandWriter, fleetAccountingSnapshotCommand+"\n"); err != nil {
			return 0, fmt.Errorf("request fleet shard %d accounting snapshot: %w", proc.index, err)
		}
	}
	var totalByteCount int64
	for _, proc := range procs {
		select {
		case response, ok := <-proc.accountingResponses:
			if !ok {
				return 0, fmt.Errorf("fleet shard %d accounting pipe closed", proc.index)
			}
			if response.err != nil {
				return 0, fmt.Errorf("fleet shard %d accounting response: %w", proc.index, response.err)
			}
			if math.MaxInt64-totalByteCount < response.byteCount {
				return 0, errors.New("provider egress counter overflow")
			}
			totalByteCount += response.byteCount
		case <-time.After(fleetAccountingSnapshotTimeout):
			return 0, fmt.Errorf("fleet shard %d accounting snapshot timed out", proc.index)
		}
	}
	return totalByteCount, nil
}

// Crosses the dynamics boundary in-process or on every shard before either
// accounting or the measured clock starts. All shard commands are sent first
// so their schedules begin within one control round trip rather than serially.
func startProviderDynamics(ctx context.Context, fleet *Fleet, procs []*fleetProcess) error {
	if fleet != nil {
		if 0 < len(procs) {
			return errors.New("both in-process and sharded fleets are active")
		}
		return fleet.StartDynamics(ctx)
	}
	if len(procs) == 0 {
		return errors.New("provider fleet is unavailable")
	}
	for _, proc := range procs {
		if proc.finished() {
			return fmt.Errorf("fleet shard %d exited before dynamics start", proc.index)
		}
		if _, err := io.WriteString(proc.accountingCommandWriter, fleetDynamicsStartCommand+"\n"); err != nil {
			return fmt.Errorf("request fleet shard %d dynamics start: %w", proc.index, err)
		}
	}
	for _, proc := range procs {
		select {
		case response, ok := <-proc.accountingResponses:
			if !ok {
				return fmt.Errorf("fleet shard %d accounting pipe closed", proc.index)
			}
			if response.err != nil {
				return fmt.Errorf("fleet shard %d dynamics response: %w", proc.index, response.err)
			}
			if response.byteCount != 0 {
				return fmt.Errorf("fleet shard %d returned invalid dynamics acknowledgement %d", proc.index, response.byteCount)
			}
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(fleetAccountingSnapshotTimeout):
			return fmt.Errorf("fleet shard %d dynamics start timed out", proc.index)
		}
	}
	return nil
}

// spawnFleetShards launches the fleet as N subprocesses, each connecting to
// this run's services. Every process is reaped immediately by a dedicated
// waiter; successful evaluation teardown later verifies each exits cleanly.
func spawnFleetShards(options *RunOptions, config *Config, services *Services) ([]*fleetProcess, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	procs := []*fleetProcess{}
	for i := 0; i < options.FleetShards; i += 1 {
		accountingCommandReader, accountingCommandWriter, err := os.Pipe()
		if err != nil {
			return procs, errors.Join(err, stopFleetShards(procs, teardownTimeout))
		}
		accountingResponseReader, accountingResponseWriter, err := os.Pipe()
		if err != nil {
			accountingCommandReader.Close()
			accountingCommandWriter.Close()
			return procs, errors.Join(err, stopFleetShards(procs, teardownTimeout))
		}
		cmd := exec.Command(self,
			"fleet",
			"--providers", options.ConfigPath,
			"--shard", intToStr(i)+"/"+intToStr(options.FleetShards),
			"--api-url", services.ApiUrl(),
			"--ws-urls", strings.Join(services.WsUrls(), ","),
			"--ramp", options.Ramp.String(),
			"--evaluation-id", options.EvaluationId,
			"--accounting-command-fd", "3",
			"--accounting-response-fd", "4",
		)
		cmd.Env = os.Environ()
		cmd.Stdout = os.Stderr // fleet emits no CSV; keep stdout clean
		cmd.Stderr = os.Stderr
		cmd.ExtraFiles = []*os.File{accountingCommandReader, accountingResponseWriter}
		if err := cmd.Start(); err != nil {
			accountingCommandReader.Close()
			accountingCommandWriter.Close()
			accountingResponseReader.Close()
			accountingResponseWriter.Close()
			return procs, errors.Join(err, stopFleetShards(procs, teardownTimeout))
		}
		accountingCommandReader.Close()
		accountingResponseWriter.Close()
		logf("spawned fleet shard %d/%d pid=%d", i, options.FleetShards, cmd.Process.Pid)
		proc := &fleetProcess{
			cmd:                      cmd,
			index:                    i,
			done:                     make(chan struct{}),
			accountingCommandWriter:  accountingCommandWriter,
			accountingResponseReader: accountingResponseReader,
			accountingResponses:      make(chan fleetAccountingResponse, 4),
		}
		procs = append(procs, proc)
		go proc.readAccountingResponses()
		go proc.reap()
	}
	return procs, nil
}

// stopFleetShards sends TERM, joins every child, and uses KILL only as a final
// containment boundary. An early exit, non-zero exit, or required KILL makes
// the evaluation incomplete.
func stopFleetShards(procs []*fleetProcess, timeout time.Duration) error {
	var stopErr error
	for _, proc := range procs {
		proc.closeAccountingCommand()
		if proc.finished() {
			stopErr = errors.Join(stopErr, fmt.Errorf("fleet shard %d exited before teardown", proc.index))
			continue
		}
		if err := proc.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			stopErr = errors.Join(stopErr, fmt.Errorf("fleet shard %d TERM: %w", proc.index, err))
		}
	}

	deadline := time.Now().Add(timeout)
	for _, proc := range procs {
		if !proc.finished() {
			remaining := time.Until(deadline)
			if remaining < 0 {
				remaining = 0
			}
			timer := time.NewTimer(remaining)
			select {
			case <-proc.done:
				timer.Stop()
			case <-timer.C:
				stopErr = errors.Join(stopErr, fmt.Errorf("fleet shard %d did not exit after TERM", proc.index))
				if err := proc.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
					stopErr = errors.Join(stopErr, fmt.Errorf("fleet shard %d KILL: %w", proc.index, err))
				}
				<-proc.done
			}
		}
		if err := proc.waitError(); err != nil {
			stopErr = errors.Join(stopErr, fmt.Errorf("fleet shard %d exit: %w", proc.index, err))
		}
	}
	return stopErr
}
