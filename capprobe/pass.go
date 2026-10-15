// One pass: its selection from the database, then bounded concurrent
// measurement and recording, and its result.
package capprobe

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/qualityprobe/providertunnel"
)

// How long one evidence write may take, apart from the pass's end: a reading
// that came in as the pass was ending is still worth storing.
const recordTimeout = 10 * time.Second

// What a pass keeps for its selection reads, ahead of its measurements.
const passSelectionAllowance = time.Minute

// What one pass did, the task's result.
type PassResult struct {
	// the sweep's next start had not come, and nothing was read
	Waiting bool `json:"waiting,omitempty"`
	// providers the pass's pages held, how many were due for evidence, how
	// many of those the egress rules admit to quality or speed, how many it
	// started measuring and how many it recorded evidence for
	Scanned  int `json:"scanned"`
	Due      int `json:"due"`
	Eligible int `json:"eligible"`
	Started  int `json:"started"`
	Recorded int `json:"recorded"`
	// the pass reached the end of the connected providers
	SweepCompleted bool `json:"sweep_completed,omitempty"`
}

// One pass's owners and seams. Production builds one in SelectPass and one
// in FinishPass; tests replace the reads, the tunnels, the write and the
// clock. It is safe for concurrent use after construction: measurements
// share only the record write, and the pass's own counts are kept under its
// own lock.
type passRunner struct {
	settings Settings
	// the establishment window a ClientHello has to be answered in
	tlsTimeout     time.Duration
	readPage       func(ctx context.Context, afterClientId server.Id, pageSize int, modernRefreshBefore time.Time, otherRefreshBefore time.Time) ([]model.ProviderCapabilityProbeCandidate, error)
	filterEligible func(ctx context.Context, clientIds []server.Id) ([]server.Id, error)
	open           openCapabilityTunnel
	record         func(ctx context.Context, clientId server.Id, evidence model.ProviderModernEvidence) error
	now            func() time.Time
	after          func(time.Duration) <-chan time.Time
}

// The production runner, with tunnels from tunnelConfig, nil for a runner
// that only selects.
func newPassRunner(settings Settings, tunnelConfig *providertunnel.Config) *passRunner {
	runner := &passRunner{
		settings:       settings,
		tlsTimeout:     tlsTimeout(),
		readPage:       readProviderCapabilityProbePage,
		filterEligible: filterProviderCapabilityProbeEligible,
		record:         recordProviderModernEvidence,
		now:            server.NowUtc,
		after:          time.After,
	}
	if tunnelConfig != nil {
		runner.open = newProviderCapabilityTunnelOpener(*tunnelConfig, settings)
	}
	return runner
}

// One pass's selection, before any tunnel: whether it waits for the sweep's
// next start, the sweep it continues, and the providers it measures.
type PassSelection struct {
	waiting   bool
	sweep     Sweep
	selected  selection
	startedAt time.Time
}

// Whether the pass measures anything, and so needs the prober identity its
// tunnels open with.
func (self *PassSelection) NeedsTunnels() bool {
	return self != nil && !self.waiting && 0 < len(self.selected.dueClientIds)
}

// Selects one pass of the sweep with settings, which the caller has found
// enabled: whether it waits for the sweep's next start, and otherwise which
// due, eligible providers it measures. It reads the database and opens
// nothing, so a pass with nothing to measure costs no prober shard.
func SelectPass(ctx context.Context, settings Settings, sweep Sweep) (*PassSelection, error) {
	return newPassRunner(settings, nil).begin(ctx, sweep)
}

// Finishes a selected pass: measures its providers with tunnels from
// tunnelConfig, the pass's identity, endpoints, credentials and control,
// records what each read conclusively, and returns where the sweep stands
// after it. tunnelConfig is unused, and may be empty, for a pass that needs
// no tunnels. ctx's deadline is the pass's budget: no measurement starts that
// could not finish, with its teardown, before it.
//
// An error leaves the sweep where it was, so the task's retry selects the
// same page again: the providers this pass recorded are no longer due, and
// the rest are measured again.
func FinishPass(ctx context.Context, settings Settings, pass *PassSelection, tunnelConfig providertunnel.Config) (Sweep, *PassResult, error) {
	var runner *passRunner
	if pass.NeedsTunnels() {
		runner = newPassRunner(settings, &tunnelConfig)
	} else {
		runner = newPassRunner(settings, nil)
	}
	return runner.finish(ctx, pass)
}

// One pass, selection and measurement together.
func (self *passRunner) run(ctx context.Context, sweep Sweep) (Sweep, *PassResult, error) {
	pass, err := self.begin(ctx, sweep)
	if err != nil {
		return sweep, nil, err
	}
	return self.finish(ctx, pass)
}

// Starts or continues the sweep and selects the pass's providers.
func (self *passRunner) begin(ctx context.Context, sweep Sweep) (*PassSelection, error) {
	if err := self.settings.Validate(); err != nil || !self.settings.Enabled {
		passesTotal.WithLabelValues(PassResultFailed).Inc()
		return nil, errors.Join(errors.New("capability probe pass needs enabled, valid settings"), err)
	}
	startedAt := self.now()
	if !sweep.inProgress() {
		if startedAt.Before(sweep.NextSweepAt) {
			return &PassSelection{waiting: true, sweep: sweep, startedAt: startedAt}, nil
		}
		sweep = Sweep{StartedAt: startedAt}
	}
	selected, err := self.selectDue(ctx, sweep.Cursor, startedAt)
	if err != nil {
		observeSelection(selected, 0)
		passesTotal.WithLabelValues(PassResultFailed).Inc()
		return nil, fmt.Errorf("capability probe selection: %w", err)
	}
	return &PassSelection{sweep: sweep, selected: selected, startedAt: startedAt}, nil
}

// Measures and records the selection's providers, and counts the pass.
func (self *passRunner) finish(ctx context.Context, pass *PassSelection) (Sweep, *PassResult, error) {
	if pass.waiting {
		passesTotal.WithLabelValues(PassResultWaiting).Inc()
		lastPassTimestampSeconds.Set(float64(pass.startedAt.Unix()))
		return pass.sweep, &PassResult{Waiting: true}, nil
	}
	startedCount, recordedCount, measureErr := 0, 0, error(nil)
	if 0 < len(pass.selected.dueClientIds) {
		if self.open == nil {
			passesTotal.WithLabelValues(PassResultFailed).Inc()
			return pass.sweep, nil, errors.New("capability probe pass has providers to measure and no tunnels")
		}
		startedCount, recordedCount, measureErr = self.measureAll(ctx, pass.selected.dueClientIds)
	}
	observeSelection(pass.selected, startedCount)
	result := &PassResult{
		Scanned:  pass.selected.scanned,
		Due:      pass.selected.due,
		Eligible: pass.selected.eligible,
		Started:  startedCount,
		Recorded: recordedCount,
	}
	if measureErr != nil {
		passesTotal.WithLabelValues(PassResultFailed).Inc()
		return pass.sweep, result, measureErr
	}
	next, completed := nextSweep(pass.sweep, pass.selected, startedCount, self.settings.minSweepInterval())
	finishedAt := self.now()
	if completed {
		result.SweepCompleted = true
		sweepsTotal.Inc()
		lastSweepSeconds.Set(finishedAt.Sub(pass.sweep.StartedAt).Seconds())
	}
	passesTotal.WithLabelValues(PassResultMeasured).Inc()
	lastPassTimestampSeconds.Set(float64(finishedAt.Unix()))
	if 0 < startedCount || completed {
		glog.Infof(
			"[capprobe]pass: scanned=%d due=%d eligible=%d started=%d recorded=%d sweep_completed=%t\n",
			result.Scanned, result.Due, result.Eligible, result.Started, result.Recorded, result.SweepCompleted,
		)
	}
	return next, result, nil
}

// Measures dueClientIds in their order, at most concurrency at once, each in
// a tunnel of its own, and records every conclusive reading. A provider
// starts only while ctx is live and its deadline leaves the admission reserve,
// so the pass ends with every tunnel it opened torn down; the providers it
// did not start are the first the next pass selects. A failed write stops
// further starts, and the pass fails with it once every started measurement
// has finished. Returns how many it started and how many it recorded.
func (self *passRunner) measureAll(ctx context.Context, dueClientIds []server.Id) (startedCount int, recordedCount int, returnErr error) {
	deadline, bounded := ctx.Deadline()
	slots := make(chan struct{}, self.settings.Concurrency)
	var wait sync.WaitGroup
	var stateLock sync.Mutex
	var recordErr error
	recordFailed := func() bool {
		stateLock.Lock()
		defer stateLock.Unlock()
		return recordErr != nil
	}
	admitted := func() bool {
		if ctx.Err() != nil || recordFailed() {
			return false
		}
		return !bounded || self.settings.admissionReserve() <= deadline.Sub(self.now())
	}
	for _, providerId := range dueClientIds {
		acquired := false
		select {
		case slots <- struct{}{}:
			acquired = true
		case <-ctx.Done():
		}
		if !acquired {
			break
		}
		if !admitted() {
			<-slots
			break
		}
		startedCount += 1
		inFlight.Inc()
		wait.Add(1)
		go func() {
			defer wait.Done()
			defer func() { <-slots }()
			defer inFlight.Dec()
			server.HandleError(func() {
				result := self.measure(ctx, providerId)
				outcome := result.outcome()
				if result.recordable {
					if err := self.recordEvidence(ctx, providerId, result.evidence); err != nil {
						outcome = outcomeRecordFailed
						glog.Errorf("[capprobe]evidence of %s not recorded: %s\n", providerId, err)
						func() {
							stateLock.Lock()
							defer stateLock.Unlock()
							recordErr = errors.Join(recordErr, err)
						}()
					} else {
						func() {
							stateLock.Lock()
							defer stateLock.Unlock()
							recordedCount += 1
						}()
					}
				}
				observeMeasurement(result, outcome)
			}, func(err error) {
				// the measurement's own teardown has run while unwinding
				providersTotal.WithLabelValues(outcomeTunnelFailed).Inc()
				glog.Errorf("[capprobe]measurement of %s failed: %s\n", providerId, err)
			})
		}()
	}
	wait.Wait()
	stateLock.Lock()
	defer stateLock.Unlock()
	return startedCount, recordedCount, recordErr
}

// Writes one provider's evidence under a bound of its own, apart from the
// pass's cancellation.
func (self *passRunner) recordEvidence(ctx context.Context, providerId server.Id, evidence model.ProviderModernEvidence) error {
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	return self.record(recordCtx, providerId, evidence)
}

// The production write: model.RecordProviderModernEvidence, with a datastore
// failure returned rather than raised.
func recordProviderModernEvidence(ctx context.Context, clientId server.Id, evidence model.ProviderModernEvidence) (returnErr error) {
	server.HandleError(func() {
		returnErr = model.RecordProviderModernEvidence(ctx, clientId, evidence)
	}, func(err error) {
		returnErr = err
	})
	return
}
