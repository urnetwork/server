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

// How long one evidence flush may take, apart from the pass's end. A released
// negative backlog shares this grace with the reading that released it.
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
	// negative readings held back from fresh modern evidence, and held ones
	// that this pass's negative reading confirmed and recorded
	DowngradesHeld      int `json:"downgrades_held,omitempty"`
	DowngradesConfirmed int `json:"downgrades_confirmed,omitempty"`
	// negative readings not recorded because the pass negotiated no session
	NegativesWithheld int `json:"negatives_withheld,omitempty"`
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
	// writes evidence unless it would downgrade fresh modern evidence
	// without downgradeConfirmed, in which case it writes nothing and held
	// is true (model.RecordProviderModernEvidenceHoldingDowngrade)
	record func(ctx context.Context, clientId server.Id, evidence model.ProviderModernEvidence, downgradeConfirmed bool) (held bool, err error)
	now    func() time.Time
	after  func(time.Duration) <-chan time.Time
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
// An error leaves the sweep's place where it was, so the task's retry
// selects the same page again: the providers this pass recorded are no
// longer due, and the rest are measured again. The sweep returned with the
// error carries the held downgrades as the pass left them, those it retired
// gone and those it held added, and the retry is to run from it.
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
		sweep = Sweep{StartedAt: startedAt, HeldDowngrades: sweep.HeldDowngrades}
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
	held := newHeldDowngradeSet(pass.sweep, pass.startedAt)
	counts, measureErr := passCounts{}, error(nil)
	if 0 < len(pass.selected.dueClientIds) {
		if self.open == nil {
			passesTotal.WithLabelValues(PassResultFailed).Inc()
			return pass.sweep, nil, errors.New("capability probe pass has providers to measure and no tunnels")
		}
		counts, measureErr = self.measureAll(ctx, pass.selected.dueClientIds, held)
	}
	observeSelection(pass.selected, counts.started)
	result := &PassResult{
		Scanned:             pass.selected.scanned,
		Due:                 pass.selected.due,
		Eligible:            pass.selected.eligible,
		Started:             counts.started,
		Recorded:            counts.recorded,
		DowngradesHeld:      counts.downgradesHeld,
		DowngradesConfirmed: counts.downgradesConfirmed,
		NegativesWithheld:   counts.negativesWithheld,
	}
	if measureErr != nil {
		passesTotal.WithLabelValues(PassResultFailed).Inc()
		// The retry selects the same page from the same place, but the
		// held downgrades stand as the pass left them. A hold it retired
		// answered a reading it recorded, whose provider is fresh now and
		// not measured again, so the old hold would let a later sweep's one
		// negative reading confirm a downgrade at once.
		failed := pass.sweep
		failed.HeldDowngrades = held.carried()
		return failed, result, measureErr
	}
	next, completed := nextSweep(pass.sweep, pass.selected, counts.started, self.settings.minSweepInterval())
	next.HeldDowngrades = held.carried()
	finishedAt := self.now()
	if completed {
		result.SweepCompleted = true
		sweepsTotal.Inc()
		lastSweepSeconds.Set(finishedAt.Sub(pass.sweep.StartedAt).Seconds())
	}
	passesTotal.WithLabelValues(PassResultMeasured).Inc()
	lastPassTimestampSeconds.Set(float64(finishedAt.Unix()))
	if 0 < counts.started || completed {
		glog.Infof(
			"[capprobe]pass: scanned=%d due=%d eligible=%d started=%d recorded=%d downgrades_held=%d downgrades_confirmed=%d negatives_withheld=%d sweep_completed=%t\n",
			result.Scanned, result.Due, result.Eligible, result.Started, result.Recorded,
			result.DowngradesHeld, result.DowngradesConfirmed, result.NegativesWithheld, result.SweepCompleted,
		)
	}
	return next, result, nil
}

// What one pass's measurements came to.
type passCounts struct {
	started             int
	recorded            int
	downgradesHeld      int
	downgradesConfirmed int
	negativesWithheld   int
}

// Measures dueClientIds in their order, at most concurrency at once, each in
// a tunnel of its own, and records every conclusive reading. A provider
// starts only while ctx is live and its deadline leaves the admission reserve,
// so the pass ends with every tunnel it opened torn down; the providers it
// did not start are the first the next pass selects. A failed write stops
// further starts, and the pass fails with it once every started measurement
// has finished.
//
// Two guards keep a passing fault from taking modern providers out of
// Quality and Speed. A negative reading, one that is not modern, is recorded
// only once some measurement of the pass has negotiated an end-to-end
// session: until then it waits, and a pass that negotiates none at all, which
// is how a fault on the probe's own side shows across the fleet, withholds
// them all and trips the breaker. And a negative reading recorded over
// modern evidence that is still fresh is held back, unless held, the
// downgrades the sweep carries, has one an earlier sweep held for the same
// provider; the pass updates held with what it holds, confirms and records.
func (self *passRunner) measureAll(ctx context.Context, dueClientIds []server.Id, held *heldDowngradeSet) (passCounts, error) {
	deadline, bounded := ctx.Deadline()
	slots := make(chan struct{}, self.settings.Concurrency)
	var wait sync.WaitGroup
	startedCount := 0
	var stateLock sync.Mutex
	counts := passCounts{}
	// the first failed write, and how many failed: one error rather than a
	// join, so the task's retry can carry the pass's sweep
	var recordErr error
	recordFailures := 0
	// some measurement of the pass negotiated a session
	negotiated := false
	// recordable negative readings waiting for a negotiation
	waitingNegatives := []measurement{}
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
	// records one recordable reading, and counts it
	recordReading := func(recordCtx context.Context, result measurement) {
		outcome := result.outcome()
		confirmed := false
		if modernOutcomeOf(result.evidence) != model.ProviderModernOutcomeModern {
			func() {
				stateLock.Lock()
				defer stateLock.Unlock()
				confirmed = held.confirms(result.providerId)
			}()
		}
		wasHeld, err := self.recordEvidence(recordCtx, result.providerId, result.evidence, confirmed)
		func() {
			stateLock.Lock()
			defer stateLock.Unlock()
			switch {
			case err != nil:
				outcome = outcomeRecordFailed
				recordFailures += 1
				if recordErr == nil {
					recordErr = err
				}
			case wasHeld:
				outcome = outcomeDowngradeHeld
				held.hold(result.providerId)
				counts.downgradesHeld += 1
			default:
				held.retire(result.providerId)
				counts.recorded += 1
				if confirmed {
					counts.downgradesConfirmed += 1
				}
			}
		}()
		switch {
		case err != nil:
			glog.Errorf("[capprobe]evidence of %s not recorded: %s\n", result.providerId, err)
		case wasHeld:
			downgradesTotal.WithLabelValues(downgradeHeld).Inc()
		case confirmed:
			downgradesTotal.WithLabelValues(downgradeConfirmed).Inc()
		}
		observeMeasurement(result, outcome)
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
				recordNow := false
				var released []measurement
				func() {
					stateLock.Lock()
					defer stateLock.Unlock()
					if result.reading.negotiated() && !negotiated {
						// a modern reading has negotiated too
						negotiated = true
						released = waitingNegatives
						waitingNegatives = nil
					}
					switch {
					case !result.recordable:
					case negotiated:
						recordNow = true
					default:
						waitingNegatives = append(waitingNegatives, result)
					}
				}()
				if !result.recordable {
					observeMeasurement(result, result.outcome())
				}
				if recordNow || 0 < len(released) {
					recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
					defer cancel()
					if recordNow {
						recordReading(recordCtx, result)
					}
					for _, waiting := range released {
						if recordFailed() {
							break
						}
						recordReading(recordCtx, waiting)
					}
				}
			}, func(err error) {
				// the measurement's own teardown has run while unwinding
				providersTotal.WithLabelValues(outcomeTunnelFailed).Inc()
				glog.Errorf("[capprobe]measurement of %s failed: %s\n", providerId, err)
			})
		}()
	}
	wait.Wait()
	withheld := func() []measurement {
		stateLock.Lock()
		defer stateLock.Unlock()
		if negotiated {
			return nil
		}
		withheld := waitingNegatives
		waitingNegatives = nil
		return withheld
	}()
	if 0 < len(withheld) {
		breakerTripsTotal.Inc()
		for _, result := range withheld {
			observeMeasurement(result, outcomeNegativeWithheld)
		}
		glog.Infof("[capprobe]pass negotiated no end-to-end session with any provider; withheld %d negative readings\n", len(withheld))
	}
	stateLock.Lock()
	defer stateLock.Unlock()
	counts.started = startedCount
	counts.negativesWithheld = len(withheld)
	if 1 < recordFailures {
		// each failure is logged with its provider
		return counts, fmt.Errorf("%d evidence writes failed, the first: %w", recordFailures, recordErr)
	}
	return counts, recordErr
}

// Writes one provider's evidence within the flush's shared grace. Expiry
// leaves unwritten evidence due for the failed pass's retry.
func (self *passRunner) recordEvidence(ctx context.Context, providerId server.Id, evidence model.ProviderModernEvidence, downgradeConfirmed bool) (held bool, err error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return self.record(ctx, providerId, evidence, downgradeConfirmed)
}

// The production write: model.RecordProviderModernEvidenceHoldingDowngrade,
// with a datastore failure returned rather than raised.
func recordProviderModernEvidence(ctx context.Context, clientId server.Id, evidence model.ProviderModernEvidence, downgradeConfirmed bool) (held bool, returnErr error) {
	server.HandleError(func() {
		held, returnErr = model.RecordProviderModernEvidenceHoldingDowngrade(ctx, clientId, evidence, downgradeConfirmed)
	}, func(err error) {
		held, returnErr = false, err
	})
	return
}
