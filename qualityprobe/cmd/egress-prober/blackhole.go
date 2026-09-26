package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/urnetwork/server/qualityprobe/egresshealth"
	"github.com/urnetwork/server/qualityprobe/fleetprobe"
	"github.com/urnetwork/server/qualityprobe/ingest"
	"github.com/urnetwork/server/qualityprobe/prober"
	"github.com/urnetwork/server/qualityprobe/providertunnel"
)

// The standalone command's blackhole sweep: bounded sweeps of the whole fleet
// on their own cadence, beside the full passes.

// Runs the cheap liveness check across the whole fleet on its
// own cadence in the standalone command. Server deployments use the same
// fleetprobe pass from recurring taskworker shards instead.
type blackholeSweeper struct {
	operator  *ingest.Client
	tunnelCfg providertunnel.Config
	pins      *pinSet
	// Fetched at the start of every sweep, which is this loop's
	// pass; a failed fetch sweeps the built-in table.
	poolUrl string
	// The operator's /ip echo, every check's warm-up.
	ipEchoUrl string
	// Bounds each load attempt of a check; echoTimeout its warm-up.
	timeout     time.Duration
	echoTimeout time.Duration
	concurrency int
	limit       int
	// The deterministic test seam for the bounded worker pool.
	// Production leaves it nil and fleetprobe opens the real tunnel.
	checkOneFn func(context.Context, string) blackholeResult
}

// Forty rounds times the server's 5000 ceiling is above a real fleet. The cap
// prevents a misbehaving due endpoint from holding one command pass forever.
const maxBlackholeRounds = 40

// One provider's check as the test seam returns it.
type blackholeResult struct {
	check        ingest.BlackholeCheck
	dark         bool
	tunnelFailed bool
	details      string
}

// Asks what is due, runs one fixed-worker-pool batch, and submits the
// whole result atomically.
func (self *blackholeSweeper) sweep(ctx context.Context) (checked int, err error) {
	due, err := self.operator.BlackholeDue(ctx, self.limit)
	if err != nil {
		return 0, err
	}
	if len(due) == 0 {
		return 0, nil
	}

	var checker fleetprobe.BlackholeChecker
	if self.checkOneFn != nil {
		checker = func(ctx context.Context, provider prober.Provider) fleetprobe.BlackholeResult {
			result := self.checkOneFn(ctx, provider.ClientId)
			return fleetprobe.BlackholeResult{
				Check:        result.check,
				Dark:         result.dark,
				TunnelFailed: result.tunnelFailed,
				Details:      result.details,
			}
		}
	}
	var pool fleetprobe.PoolSource
	if self.poolUrl != "" {
		loaded, err := fleetprobe.LoadPool(ctx, self.operator.Http, self.poolUrl, self.operator.OperatorSecret)
		if err != nil {
			log.Printf("blackhole: sweeping the built-in destination table: %s", err)
		}
		pool = func() *egresshealth.Pool { return loaded }
	}
	summary, err := fleetprobe.RunBlackhole(ctx, fleetprobe.ProvidersFromDue(due), fleetprobe.BlackholeOptions{
		TunnelConfig:  self.tunnelCfg,
		Pins:          self.pins.get,
		Pool:          pool,
		Timeout:       self.timeout,
		IpEchoUrl:     self.ipEchoUrl,
		IpEchoTimeout: self.echoTimeout,
		Concurrency:   self.concurrency,
		CheckOne:      checker,
	})
	if err != nil {
		return 0, err
	}
	if len(summary.Checks) == 0 {
		return 0, nil
	}
	if err := self.operator.SubmitBlackholeChecks(ctx, summary.Checks); err != nil {
		return 0, fmt.Errorf("submitting %d checks: %w", len(summary.Checks), err)
	}

	log.Printf(
		"blackhole: pass checked=%d dark=%d (tunnel_failed=%d) not_measured=%d ok=%d",
		len(summary.Checks),
		summary.Dark,
		summary.TunnelFailed,
		summary.NotMeasured,
		len(summary.Checks)-summary.Dark-summary.NotMeasured,
	)
	return len(summary.Checks), nil
}

// Repeats complete bounded sweeps until cancellation. Taskworker mode does
// not use this loop; its durable post-step owns repetition instead.
func (self *blackholeSweeper) run(ctx context.Context, interval time.Duration) {
	for {
		start := time.Now()
		total := 0
		var err error
		for range maxBlackholeRounds {
			var checked int
			checked, err = self.sweep(ctx)
			total += checked
			if err != nil || checked == 0 || ctx.Err() != nil {
				break
			}
		}
		switch {
		case err == nil:
			if total == 0 {
				log.Printf("blackhole: pass found nothing due")
			} else {
				log.Printf("blackhole: sweep complete: %d checked in %s", total, time.Since(start).Round(time.Second))
			}
		case errors.Is(err, ingest.ErrBlackholeUnsupported):
			log.Printf("blackhole: the server does not implement the blackhole endpoints; sweeping is disabled")
			return
		case errors.Is(err, ingest.ErrUnauthorized):
			log.Printf("blackhole: the server rejected the operator secret; sweeping is disabled. Fix -operator-secret and restart.")
			return
		default:
			log.Printf("blackhole: pass failed after %s: %s", time.Since(start).Round(time.Second), err)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}
