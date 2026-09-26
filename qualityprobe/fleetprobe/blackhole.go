// This file implements the bounded, cheap reachability half of a fleet pass.
package fleetprobe

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server/qualityprobe/egresshealth"
	"github.com/urnetwork/server/qualityprobe/ingest"
	"github.com/urnetwork/server/qualityprobe/prober"
	"github.com/urnetwork/server/qualityprobe/providertunnel"
)

// One provider's cheap reachability result plus diagnostic
// detail. The check is ready to submit directly to the operator endpoint.
type BlackholeResult struct {
	Check ingest.BlackholeCheck
	// A failed check: a failure the server counts towards dark. A
	// not-measured check is never Dark.
	Dark         bool
	TunnelFailed bool
	// A check whose tunnel was gone and could not be
	// re-created for any of its loads: submitted as not measured, which the
	// server stores without counting anything against the provider.
	NotMeasured bool
	Details     string
}

// The per-provider boundary. Production leaves it nil;
// tests use it to force worker-pool ordering without real tunnels.
type BlackholeChecker func(context.Context, prober.Provider) BlackholeResult

// Configures one bounded batch. Scheduling and due-provider
// selection stay with the caller.
type BlackholeOptions struct {
	TunnelConfig providertunnel.Config
	Pins         PinSource
	// The pass's destination pool (see LoadPool); nil is the built-in
	// table. The check draws from its connectivity class.
	Pool PoolSource
	// Bounds each load attempt of the check.
	Timeout time.Duration
	// The operator's /ip echo; empty derives it from
	// TunnelConfig.ApiUrl. IpEchoTimeout bounds the warm-up alone; zero uses
	// egresshealth.DefaultIpEchoTimeout, the cold-start allowance.
	IpEchoUrl     string
	IpEchoTimeout time.Duration
	// Passed through to egresshealth.Options, like the two fields below;
	// zero uses its defaults (3, 5 minutes, 2).
	LoadAttempts           int
	LoadRetryMeanInterval  time.Duration
	TunnelRecreateAttempts int
	// How many providers are checked at once. Zero uses
	// DefaultBlackholeConcurrency.
	Concurrency int
	// Optional pass-owned fixed worker set. Sharing it between logical cohorts
	// does not merge their results or guards. Nil owns a private batch pool.
	WorkerPool *BlackholeWorkerPool
	// Stops successor admission without canceling an active check's context.
	// The first MinimumAdmission selected checks still start unless ctx is
	// canceled, even when this signal is already closed. All workers join.
	// Nil leaves admission governed only by ctx and the selected batch.
	AdmissionDone <-chan struct{}
	// A second independent stop edge (for example an owning lease cutoff).
	// Both edges are checked directly at submission and execution; neither
	// relies on an asynchronous goroutine propagating cancellation.
	AdditionalAdmissionDone <-chan struct{}
	// An initial per-batch cohort, bounded by the selected provider count.
	// Zero allows AdmissionDone to stop the batch before its first check.
	MinimumAdmission int
	Now              func() time.Time
	CheckOne         BlackholeChecker
	// A retained result may be observed before its slow siblings finish. The
	// callback runs in a worker, must not block, and cannot publish ordinary
	// negatives before the caller's batch-wide guard has judged them.
	OnCompleted func(BlackholeResult)
	// Optional concurrent, nonblocking observer. It receives no provider IDs.
	// It must not panic; callbacks finish before RunBlackhole returns.
	ObserveProgress func(BlackholeProgress)
}

// Returns Concurrency, or DefaultBlackholeConcurrency when unset.
func (self BlackholeOptions) concurrency() int {
	if 0 < self.Concurrency {
		return self.Concurrency
	}
	return DefaultBlackholeConcurrency
}

// Returns IpEchoUrl, or the echo derived from the tunnel's api url.
func (self BlackholeOptions) ipEchoUrl() string {
	if self.IpEchoUrl != "" {
		return self.IpEchoUrl
	}
	return IpEchoUrl(self.TunnelConfig.ApiUrl)
}

// Returns IpEchoTimeout, or egresshealth.DefaultIpEchoTimeout when unset.
func (self BlackholeOptions) ipEchoTimeout() time.Duration {
	if 0 < self.IpEchoTimeout {
		return self.IpEchoTimeout
	}
	return egresshealth.DefaultIpEchoTimeout
}

// Carries the ordered results and aggregate counts.
type BlackholeSummary struct {
	Checks       []ingest.BlackholeCheck
	Dark         int
	TunnelFailed int
	// Counts the checks submitted as not measured; they are in
	// Checks, and in neither Dark nor TunnelFailed.
	NotMeasured int
}

// The cheap reachability pass uses a private config copy; full and bandwidth
// passes may share the caller's original config without inheriting its policy.
func blackholeTunnelConfig(options BlackholeOptions) providertunnel.Config {
	config := options.TunnelConfig
	config.Pins = options.Pins.pins()
	// Three tiny HTTPS connectivity checks do not need the ordinary 1MiB ->
	// 32.75MiB -> 128MiB contract ramp. An unused prefetched successor keeps
	// its reservation until server expiry even after a final source close.
	config.ContractReservationByteCount = 1024 * 1024
	return config
}

// Required timeouts and concurrency fail before a tunnel is opened.
func validateBlackholeOptions(options BlackholeOptions) error {
	if options.TunnelConfig.ContractReservationByteCount < 0 {
		return providertunnel.ErrContractReservation
	}
	if options.Timeout <= 0 {
		return fmt.Errorf("fleetprobe: blackhole timeout must be positive (got %s)", options.Timeout)
	}
	if options.MinimumAdmission < 0 {
		return fmt.Errorf("fleetprobe: minimum admission must not be negative (got %d)", options.MinimumAdmission)
	}
	if options.Concurrency < 0 {
		return fmt.Errorf("fleetprobe: blackhole concurrency must not be negative (got %d)", options.Concurrency)
	}
	if options.WorkerPool != nil && options.concurrency() < options.WorkerPool.workerCount {
		return fmt.Errorf("fleetprobe: shared blackhole pool exceeds the batch concurrency")
	}
	return nil
}

// Checks one batch with a fixed worker pool. It never creates one
// goroutine per provider, so a large due limit does not become a memory spike.
// Each provider's place decides which connectivity destinations it is asked
// for.
func RunBlackhole(
	ctx context.Context,
	providers []prober.Provider,
	options BlackholeOptions,
) (BlackholeSummary, error) {
	if err := validateBlackholeOptions(options); err != nil {
		return BlackholeSummary{}, err
	}
	if len(providers) == 0 {
		return BlackholeSummary{}, nil
	}

	// One provider is measured only through its constructed tunnel -- re-created
	// if it dies part-way -- and returns a submission-ready verdict even when
	// parsing or tunnel setup fails.
	checkOne := func(provider prober.Provider) BlackholeResult {
		if options.CheckOne != nil {
			return options.CheckOne(ctx, provider)
		}

		checkedAt := time.Now().UTC()
		if options.Now != nil {
			checkedAt = options.Now().UTC()
		}
		providerClientId := provider.ClientId
		clientId, err := connect.ParseId(providerClientId)
		if err != nil {
			return BlackholeResult{
				Check: ingest.BlackholeCheck{
					ClientId:  providerClientId,
					Ok:        false,
					Failure:   "bad_client_id",
					CheckedAt: checkedAt,
				},
				Dark:    true,
				Details: err.Error(),
			}
		}

		pool := options.Pool.pool()
		echoUrl := options.ipEchoUrl()
		echoTimeout := options.ipEchoTimeout()
		hosts := dialHosts(egresshealth.BlackholeHostsOf(pool.Destinations), echoUrl)
		// The client's own timeout bounds every request, the warm-up included, so
		// it is the longer of the two.
		clientTimeout := max(options.Timeout, echoTimeout)
		path, err := openProbePath(ctx, providerTunnelOpener(blackholeTunnelConfig(options), clientId, hosts), hosts, clientTimeout)
		if err != nil {
			return BlackholeResult{
				Check: ingest.BlackholeCheck{
					ClientId:  providerClientId,
					Ok:        false,
					Failure:   "tunnel_failed",
					CheckedAt: checkedAt,
				},
				Dark:         true,
				TunnelFailed: true,
				Details:      err.Error(),
			}
		}
		defer func() {
			if err := path.Close(); err != nil {
				log.Printf("blackhole: provider=%s tunnel teardown failed: %s", providerClientId, err)
			}
		}()

		client, _ := path.Current()
		result := egresshealth.Blackhole(ctx, client, egresshealth.Options{
			PerRequestTimeout:      options.Timeout,
			IpEchoUrl:              echoUrl,
			IpEchoTimeout:          echoTimeout,
			LoadAttempts:           options.LoadAttempts,
			LoadRetryMeanInterval:  options.LoadRetryMeanInterval,
			TunnelRecreateAttempts: options.TunnelRecreateAttempts,
			Destinations:           pool.Destinations,
			Profile:                profileOf(pool),
			ProviderPlace:          provider.Place,
			Path:                   path,
		})
		return blackholeResultOf(providerClientId, checkedAt, result, path.reopens())
	}

	results := make([]BlackholeResult, len(providers))
	workerPool := options.WorkerPool
	if workerPool == nil {
		var err error
		workerPool, err = NewBlackholeWorkerPool(ctx, min(options.concurrency(), len(providers)))
		if err != nil {
			return BlackholeSummary{}, err
		}
		defer workerPool.CloseAndWait()
	}
	var waitGroup sync.WaitGroup
sendJobs:
	for index, provider := range providers {
		if ctx.Err() != nil {
			break
		}
		admissionDone := options.AdmissionDone
		additionalAdmissionDone := options.AdditionalAdmissionDone
		if index < options.MinimumAdmission {
			admissionDone = nil
			additionalAdmissionDone = nil
		}
		select {
		case <-admissionDone:
			break sendJobs
		case <-additionalAdmissionDone:
			break sendJobs
		default:
		}
		waitGroup.Add(1)
		accepted := workerPool.submit(ctx, admissionDone, additionalAdmissionDone, func() {
			defer waitGroup.Done()
			// Cancellation and receipt may become ready together. Do not open
			// a tunnel after either boundary merely because a worker was free.
			if ctx.Err() != nil {
				return
			}
			select {
			case <-admissionDone:
				return
			case <-additionalAdmissionDone:
				return
			default:
			}
			if options.ObserveProgress != nil {
				options.ObserveProgress(BlackholeStarted)
			}
			result := checkOne(provider)
			// A canceled request is not a negative provider verdict. Keep only
			// results which completed before the task lost its own budget.
			if ctx.Err() != nil {
				if options.ObserveProgress != nil {
					options.ObserveProgress(BlackholeCanceled)
				}
				return
			}
			results[index] = result
			if result.Check.ClientId != "" && options.OnCompleted != nil {
				options.OnCompleted(result)
			}
			if options.ObserveProgress != nil {
				if result.Check.ClientId == "" {
					options.ObserveProgress(BlackholeDiscarded)
				} else {
					options.ObserveProgress(BlackholeCompleted)
				}
			}
		})
		if !accepted {
			waitGroup.Done()
			break
		}
	}
	waitGroup.Wait()

	summary := BlackholeSummary{
		Checks: make([]ingest.BlackholeCheck, 0, len(results)),
	}
	for _, result := range results {
		if result.Check.ClientId == "" {
			continue
		}
		summary.Checks = append(summary.Checks, result.Check)
		switch {
		case result.NotMeasured:
			summary.NotMeasured++
			log.Printf("blackhole: provider=%s not measured %s", result.Check.ClientId, result.Details)
		case result.Dark:
			summary.Dark++
			if result.TunnelFailed {
				summary.TunnelFailed++
			}
			log.Printf("blackhole: provider=%s dark %s", result.Check.ClientId, result.Details)
		}
	}
	if workerPool.ctx.Err() != nil && ctx.Err() == nil {
		return summary, ErrBlackholeWorkerPoolClosed
	}
	return summary, nil
}

// Turns a check's outcome into its submission.
func blackholeResultOf(providerClientId string, checkedAt time.Time, result *egresshealth.BlackholeResult, reopens int) BlackholeResult {
	check := ingest.BlackholeCheck{
		ClientId:  providerClientId,
		Ok:        result.Ok,
		CheckedAt: checkedAt,
	}
	notMeasured := !result.Ok && result.Failure == egresshealth.FailureNotMeasured
	if !result.Ok {
		check.Failure = result.Failure
		check.NotMeasured = notMeasured
	}
	details := ""
	for _, checkResult := range result.Results {
		if checkResult.Err != "" {
			details += fmt.Sprintf("%s=%s (attempts=%d) ", checkResult.Name, checkResult.Err, checkResult.Attempts)
		}
	}
	if stages := result.FailureStageSummary(); stages != "" {
		details += "failure_stages=" + stages + " "
	}
	if 0 < reopens {
		details += fmt.Sprintf("tunnel_recreated=%d ", reopens)
	}
	return BlackholeResult{
		Check:       check,
		Dark:        !result.Ok && !notMeasured,
		NotMeasured: notMeasured,
		Details:     details,
	}
}
