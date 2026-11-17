// Five retained-state measurements share one fixture. Invalid runs remain
// visible; only quiescent requests may permit the next original measurement.
package perfvar

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

type syntheticFastCumulative struct {
	Offered, Accepted, Refused                                             uint64
	TcpRetransmits, TcpTimeouts                                            uint64
	ProviderReceiveRefusals, ProviderTimeoutResends, ProviderInitialWrites uint64
}

func syntheticFastCounterDelta(before, after syntheticFastCumulative) (syntheticFastCumulative, error) {
	a := [8]uint64{before.Offered, before.Accepted, before.Refused, before.TcpRetransmits, before.TcpTimeouts, before.ProviderReceiveRefusals, before.ProviderTimeoutResends, before.ProviderInitialWrites}
	b := [8]uint64{after.Offered, after.Accepted, after.Refused, after.TcpRetransmits, after.TcpTimeouts, after.ProviderReceiveRefusals, after.ProviderTimeoutResends, after.ProviderInitialWrites}
	var delta [8]uint64
	for index := range a {
		if b[index] < a[index] {
			return syntheticFastCumulative{}, fmt.Errorf("retained fixture counter%d reset", index)
		}
		delta[index] = b[index] - a[index]
	}
	return syntheticFastCumulative{Offered: delta[0], Accepted: delta[1], Refused: delta[2], TcpRetransmits: delta[3], TcpTimeouts: delta[4], ProviderReceiveRefusals: delta[5], ProviderTimeoutResends: delta[6], ProviderInitialWrites: delta[7]}, nil
}

type syntheticFastRun struct {
	ProviderMask                uint32              `json:"provider_mask"`
	UnknownProviderRequests     uint64              `json:"unknown_provider_requests"`
	Index                       int                 `json:"index"`
	State                       string              `json:"state"`
	Designation                 string              `json:"designation"`
	FixtureLabel                string              `json:"fixture_label"`
	Workload                    syntheticFastReport `json:"workload"`
	Before, After, Delta        syntheticFastCumulative
	BeforeDetails, AfterDetails any
	RequestsQuiescent           bool   `json:"requests_quiescent"`
	Error                       string `json:"error,omitempty"`
	NotRunReason                string `json:"not_run_reason,omitempty"`
}

type syntheticFastSequenceReport struct {
	Schema              string `json:"schema"`
	ExpectedRuns        int    `json:"expected_runs"`
	FixtureLabel        string `json:"fixture_label"`
	BudgetNs, ElapsedNs int64
	Runs                []syntheticFastRun `json:"runs"`
	Valid               bool               `json:"valid"`
	Stop                string             `json:"stop"`
}

// Warmup/startup is already inside each per-run overall bound. One fixed
// sequence budget also reserves every request drain and final fixture join.
func syntheticFastSequenceBound(settings syntheticFastSettings, count int) time.Duration {
	// Origin close, existing SDK fixture's30s join, then owned HTTP workers.
	return time.Duration(count)*(settings.OverallTimeout+settings.RunDrainTimeout) + 2*settings.FixtureDrainTimeout + 30*time.Second
}

// No fixture is created or reset here. The adapter closes only each run's
// request owner, retaining SDK/provider/TCP/origin/transport objects across runs.
func measureSyntheticFastSequence(parent context.Context, settings syntheticFastSettings, count int, fixtureLabel string,
	execute func(context.Context, int) (syntheticFastReport, error), quiesce func(context.Context, int) error,
	capture func() (syntheticFastCumulative, any)) (syntheticFastSequenceReport, error) {
	report := syntheticFastSequenceReport{Schema: "perfvar-synthetic-fast-sequence-v5", ExpectedRuns: count, FixtureLabel: fixtureLabel, Stop: "completed"}
	if count != 1 && count != 5 {
		return report, errors.New("synthetic sequence requires exactly five benchmarks or one diagnostic")
	}
	if err := settings.validate(); err != nil {
		return report, err
	}
	started := time.Now()
	bound := syntheticFastSequenceBound(settings, count)
	report.BudgetNs = int64(bound)
	ctx, cancel := context.WithTimeout(parent, bound)
	defer cancel()
	report.Runs = make([]syntheticFastRun, count)
	for index := range report.Runs {
		designation := "retained_state"
		if index == 0 {
			designation = "cold_start"
			if settings.Warm {
				designation = "explicit_warmup"
			}
		}
		report.Runs[index] = syntheticFastRun{Index: index + 1, State: "NOTRUN", Designation: designation, FixtureLabel: fixtureLabel, NotRunReason: "not yet attempted"}
	}
	var aggregate error
	for index := range report.Runs {
		if ctx.Err() != nil {
			report.Stop = "sequence_canceled_or_deadline"
			aggregate = errors.Join(aggregate, ctx.Err())
			break
		}
		row := &report.Runs[index]
		row.NotRunReason = ""
		row.State = "running"
		row.Before, row.BeforeDetails = capture()
		runCtx, stopRun := context.WithTimeout(ctx, settings.OverallTimeout)
		workload, aggregateRunErr := execute(runCtx, index)
		row.Workload = workload
		runDeadlineErr := runCtx.Err()
		stopRun()
		joinCtx, stopJoin := context.WithTimeout(ctx, settings.RunDrainTimeout)
		joinErr := quiesce(joinCtx, index)
		stopJoin()
		row.RequestsQuiescent = row.Workload.Joined && joinErr == nil
		row.After, row.AfterDetails = capture()
		var deltaErr error
		row.Delta, deltaErr = syntheticFastCounterDelta(row.Before, row.After)
		runErr := errors.Join(aggregateRunErr, runDeadlineErr, joinErr, deltaErr)
		if !row.RequestsQuiescent {
			runErr = errors.Join(runErr, errors.New("request ownership did not quiesce"))
		}
		if !row.Workload.Valid && runErr == nil {
			runErr = errors.New("individual synthetic measurement invalid")
		}
		if row.Workload.Settings != settings {
			runErr = errors.Join(runErr, errors.New("settings changed inside retained sequence"))
		}
		if runErr != nil {
			row.State = "invalid"
			row.Error = runErr.Error()
			aggregate = errors.Join(aggregate, fmt.Errorf("run%d: %w", index+1, runErr))
		} else {
			row.State = "valid"
		}
		if !row.RequestsQuiescent || deltaErr != nil {
			report.Stop = "unsafe_or_unknown_ownership"
			break
		}
		if ctx.Err() != nil {
			report.Stop = "sequence_canceled_or_deadline"
			aggregate = errors.Join(aggregate, ctx.Err())
			break
		}
	}
	for index := range report.Runs {
		if report.Runs[index].State == "NOTRUN" {
			report.Runs[index].NotRunReason = report.Stop
		}
	}
	report.ElapsedNs = int64(time.Since(started))
	report.Valid = aggregate == nil && report.Stop == "completed"
	return report, aggregate
}

// A sealed run admits no later server body, even if a canceled request reaches
// the listener late. Already registered handlers notify their actual return.
type syntheticFastOriginRun struct {
	upload              *syntheticFastUploadRun
	providerMask        atomic.Uint32
	streamProviderMasks [8]atomic.Uint32
	providerUnknown     atomic.Uint64
	ctx                 context.Context
	stateLock           sync.Mutex
	active              int
	sealed              bool
	updated             chan struct{}
}

func (self *syntheticFastOriginRun) notifyWithLock() {
	if self.updated != nil {
		close(self.updated)
	}
	self.updated = make(chan struct{})
}

func (self *syntheticFastOriginRun) enter() (func(), bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.sealed || self.ctx.Err() != nil {
		return nil, false
	}
	self.active++
	self.notifyWithLock()
	return func() { self.stateLock.Lock(); defer self.stateLock.Unlock(); self.active--; self.notifyWithLock() }, true
}

func (self *syntheticFastOriginRun) sealAndWait(ctx context.Context) error {
	for {
		self.stateLock.Lock()
		self.sealed = true
		if self.updated == nil {
			self.updated = make(chan struct{})
		}
		update := self.updated
		active := self.active
		self.stateLock.Unlock()
		if active == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-update:
		}
	}
}

// Latency probes retain the same original run token rather than falling into
// another run's origin scope when warm connections are reused.
func syntheticFastLatencyUrl(target string) string {
	parsed, err := url.Parse(target)
	if err != nil {
		return target
	}
	parsed.Path = "/latency"
	return parsed.String()
}

// Publish the empty origin owner before a diagnostic gate can return. The
// retained sequence always quiesces even an execute callback that never sent HTTP.
func syntheticFastCarrierBeginOriginRun(origin *syntheticFastOrigin, ctx context.Context, ordinal int, upload bool, diagnosticErr error) (*syntheticFastOriginRun, error) {
	run := &syntheticFastOriginRun{ctx: ctx}
	if upload {
		run.upload = &syntheticFastUploadRun{}
	}
	origin.runs[ordinal].Store(run)
	return run, diagnosticErr
}
