// This file implements the bounded, cheap reachability half of a fleet pass.
package fleetprobe

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/operator-proxy/egresshealth"
	"github.com/urnetwork/operator-proxy/ingest"
	"github.com/urnetwork/operator-proxy/prober"
	"github.com/urnetwork/operator-proxy/providertunnel"
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
	Now         func() time.Time
	CheckOne    BlackholeChecker
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
	if options.Concurrency < 0 {
		return fmt.Errorf("fleetprobe: blackhole concurrency must not be negative (got %d)", options.Concurrency)
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
	// One provider of the batch and its place in the results.
	type job struct {
		Index    int
		Provider prober.Provider
	}
	jobs := make(chan job)
	workerCount := min(options.concurrency(), len(providers))
	var waitGroup sync.WaitGroup
	for range workerCount {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			for job := range jobs {
				// Cancellation and a channel receive may become ready together;
				// select is intentionally random in that case. Re-check at the
				// execution boundary so an admitted-but-not-started job cannot
				// construct a doomed tunnel after task drain.
				if ctx.Err() != nil {
					continue
				}
				result := checkOne(job.Provider)
				// The check may have been admitted while its parent task still had
				// budget, then finish after the task/context was canceled. Its
				// canceled requests look exactly like a provider blackhole, but
				// the only fact established is that the prober lost its own budget.
				// Do not persist that as a negative provider verdict. This mirrors
				// the full health path's ErrNoBudget boundary.
				if ctx.Err() != nil {
					continue
				}
				results[job.Index] = result
			}
		}()
	}

sendJobs:
	for index, provider := range providers {
		if ctx.Err() != nil {
			break
		}
		select {
		case jobs <- job{Index: index, Provider: provider}:
		case <-ctx.Done():
			break sendJobs
		}
	}
	close(jobs)
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
