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
	"github.com/urnetwork/operator-proxy/providertunnel"
)

// BlackholeResult is one provider's cheap reachability result plus diagnostic
// detail. The check is ready to submit directly to the operator endpoint.
type BlackholeResult struct {
	Check        ingest.BlackholeCheck
	Dark         bool
	TunnelFailed bool
	Details      string
}

// BlackholeChecker is the per-provider boundary. Production leaves it nil;
// tests use it to force worker-pool ordering without real tunnels.
type BlackholeChecker func(context.Context, string) BlackholeResult

// BlackholeOptions configures one bounded batch. Scheduling and due-provider
// selection stay with the caller.
type BlackholeOptions struct {
	TunnelConfig providertunnel.Config
	Pins         PinSource
	Timeout      time.Duration
	Concurrency  int
	Now          func() time.Time
	CheckOne     BlackholeChecker
}

// BlackholeSummary carries the ordered results and aggregate counts.
type BlackholeSummary struct {
	Checks       []ingest.BlackholeCheck
	Dark         int
	TunnelFailed int
}

// The cheap reachability pass uses a private config copy; full and bandwidth
// passes may share the caller's original config without inheriting its policy.
func blackholeTunnelConfig(options BlackholeOptions) providertunnel.Config {
	config := options.TunnelConfig
	config.Pins = options.Pins()
	// Three tiny HTTPS connectivity checks do not need the ordinary 1MiB ->
	// 32.75MiB -> 128MiB contract ramp. An unused prefetched successor keeps
	// its reservation until server expiry even after a final source close.
	config.ContractReservationByteCount = 1024 * 1024
	return config
}

// Required pins, timeouts, and concurrency fail before a tunnel is opened.
func validateBlackholeOptions(options BlackholeOptions) error {
	if options.TunnelConfig.ContractReservationByteCount < 0 {
		return providertunnel.ErrContractReservation
	}
	if options.Pins == nil || len(options.Pins()) == 0 {
		return providertunnel.ErrPinsRequired
	}
	if options.Timeout <= 0 {
		return fmt.Errorf("fleetprobe: blackhole timeout must be positive (got %s)", options.Timeout)
	}
	if options.Concurrency < 1 {
		return fmt.Errorf("fleetprobe: blackhole concurrency must be positive (got %d)", options.Concurrency)
	}
	return nil
}

// RunBlackhole checks one batch with a fixed worker pool. It never creates one
// goroutine per provider, so a large due limit does not become a memory spike.
func RunBlackhole(
	ctx context.Context,
	providerClientIds []string,
	options BlackholeOptions,
) (BlackholeSummary, error) {
	if err := validateBlackholeOptions(options); err != nil {
		return BlackholeSummary{}, err
	}
	if len(providerClientIds) == 0 {
		return BlackholeSummary{}, nil
	}

	results := make([]BlackholeResult, len(providerClientIds))
	type job struct {
		Index            int
		ProviderClientId string
	}
	jobs := make(chan job)
	workerCount := min(options.Concurrency, len(providerClientIds))
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
				result := checkBlackholeProvider(ctx, job.ProviderClientId, options)
				// The check may have been admitted while its parent task still had
				// budget, then finish after the task/context was canceled. Its three
				// canceled HTTP requests look exactly like a provider blackhole, but
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
	for index, providerClientId := range providerClientIds {
		if ctx.Err() != nil {
			break
		}
		select {
		case jobs <- job{Index: index, ProviderClientId: providerClientId}:
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
		if result.Dark {
			summary.Dark++
			if result.TunnelFailed {
				summary.TunnelFailed++
			}
			log.Printf("blackhole: provider=%s dark %s", result.Check.ClientId, result.Details)
		}
	}
	return summary, nil
}

// One provider is measured only through its constructed tunnel and returns a
// submission-ready verdict even when parsing or tunnel setup fails.
func checkBlackholeProvider(
	ctx context.Context,
	providerClientId string,
	options BlackholeOptions,
) BlackholeResult {
	if options.CheckOne != nil {
		return options.CheckOne(ctx, providerClientId)
	}

	checkedAt := time.Now().UTC()
	if options.Now != nil {
		checkedAt = options.Now().UTC()
	}
	clientId, err := connect.ParseId(providerClientId)
	if err != nil {
		return BlackholeResult{
			Check: ingest.BlackholeCheck{
				ClientId:  providerClientId,
				OK:        false,
				Failure:   "bad_client_id",
				CheckedAt: checkedAt,
			},
			Dark:    true,
			Details: err.Error(),
		}
	}

	tunnelConfig := blackholeTunnelConfig(options)
	tunnel, err := providertunnel.Open(ctx, tunnelConfig, clientId)
	if err != nil {
		return BlackholeResult{
			Check: ingest.BlackholeCheck{
				ClientId:  providerClientId,
				OK:        false,
				Failure:   "tunnel_failed",
				CheckedAt: checkedAt,
			},
			Dark:         true,
			TunnelFailed: true,
			Details:      err.Error(),
		}
	}
	defer func() {
		if err := tunnel.Close(); err != nil {
			log.Printf("blackhole: provider=%s tunnel teardown failed: %s", providerClientId, err)
		}
	}()

	client := tunnel.HTTPClientForHosts(options.Timeout, egresshealth.BlackholeHosts())
	result := egresshealth.Blackhole(ctx, client, egresshealth.Options{
		PerRequestTimeout: options.Timeout,
	})
	check := ingest.BlackholeCheck{
		ClientId:  providerClientId,
		OK:        result.OK,
		CheckedAt: checkedAt,
	}
	if !result.OK {
		check.Failure = result.Failure
	}
	details := ""
	for _, checkResult := range result.Results {
		if checkResult.Err != "" {
			details += checkResult.Name + "=" + checkResult.Err + " "
		}
	}
	return BlackholeResult{
		Check:   check,
		Dark:    !result.OK,
		Details: details,
	}
}
