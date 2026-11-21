//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// The world's shared concurrency helpers for the scenario tracks (the e2e conventions' "Concurrency"):
//
//   - bursts: RunBursts runs each burst on a goroutine of its own, its steps in order with a random pause before each,
//     every burst at once, until each is done or the phase's bound passes; a step's error ends its burst and is kept;
//   - the progress dump: FailProgress, as connect's failProgress, logs every block's /status and run count, every
//     sidecar's state, and every burst's progress and errors, then fails the test, so a stall shows where it stands;
//   - chaos: World.Restart, Stop and Start (world_test.go) stop a named block mid-run and start it again on its stores;
//   - the ledgers: ReadLedgers reads the usage, budget and quota ledgers for a scenario's exact reconciliation at its
//     end (WorldLedgers).
//
// Nothing here sleeps for an outcome: a pause is a burst's own random spacing, and every wait is a bound on a hang.

package sidecare2e

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar/evsservice"

	"github.com/urnetwork/server"
)

// How bursts run (QD-702).
type WorldBurstSettings struct {
	// The longest random pause before each step of a burst.
	MaxPause time.Duration
	// The pauses' seed, logged by RunBursts so a run's shape repeats; each burst's generator is seeded by it and the
	// burst's place.
	Seed uint64
	// The phase's bound, after which RunBursts answers an error and FailProgress shows where each burst stood.
	PhaseTimeout time.Duration
}

// Pauses up to 50 ms, a seed from the clock, and a phase of 4 min, inside the conventions' 10-minute case.
func DefaultWorldBurstSettings() *WorldBurstSettings {
	return &WorldBurstSettings{
		MaxPause:     50 * time.Millisecond,
		Seed:         uint64(time.Now().UnixNano()),
		PhaseTimeout: 4 * time.Minute,
	}
}

// One burst: StepCount steps of one actor, run in order by RunBursts. Its progress is safe to read while it runs.
type WorldBurst struct {
	// What its progress line and errors name it by.
	Name string
	// How many steps it runs.
	StepCount int
	// One step, index from 0, with the burst's own generator for any choice it makes. An error ends the burst.
	Step func(ctx context.Context, index int, random *rand.Rand) error

	stateLock sync.Mutex
	stepped   int
	errs      []error
	done      bool
}

// Adds err to the burst's errors, for a check a step makes beside its own error.
func (self *WorldBurst) Fail(err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.errs = append(self.errs, err)
}

// What failed in the burst so far.
func (self *WorldBurst) Errors() []error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.errs)
}

// How many steps finished.
func (self *WorldBurst) Stepped() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.stepped
}

// One line of where the burst stands.
func (self *WorldBurst) Progress() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return fmt.Sprintf("%s: %d of %d steps, done %t, %d errors", self.Name, self.stepped, self.StepCount, self.done, len(self.errs))
}

// The burst's run: a random pause, then each step in order, until one fails or ctx ends.
func (self *WorldBurst) run(ctx context.Context, settings *WorldBurstSettings, random *rand.Rand) {
	defer func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.done = true
	}()
	for index := range self.StepCount {
		select {
		case <-ctx.Done():
			self.Fail(fmt.Errorf("%s stopped before step %d: %w", self.Name, index, ctx.Err()))
			return
		case <-time.After(time.Duration(random.Int64N(int64(settings.MaxPause) + 1))):
		}
		if err := self.Step(ctx, index, random); err != nil {
			self.Fail(fmt.Errorf("%s's step %d: %w", self.Name, index, err))
			return
		}
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.stepped += 1
		}()
	}
}

// Runs every burst at once, one goroutine each, and waits until each is done or the phase's bound or ctx ends: the
// bound's error then, nil once every burst ended, whatever its errors, which each burst keeps for the caller to fail
// on. settings nil takes the defaults. The seed is logged.
func RunBursts(ctx context.Context, t testing.TB, phase string, bursts []*WorldBurst, settings *WorldBurstSettings) error {
	if settings == nil {
		settings = DefaultWorldBurstSettings()
	}
	t.Logf("%s: %d bursts, seed %d", phase, len(bursts), settings.Seed)
	phaseCtx, cancel := context.WithTimeout(ctx, settings.PhaseTimeout)
	defer cancel()
	var group sync.WaitGroup
	for index, burst := range bursts {
		random := rand.New(rand.NewPCG(settings.Seed, uint64(index)))
		group.Go(func() {
			burst.run(phaseCtx, settings, random)
		})
	}
	group.Wait()
	if err := phaseCtx.Err(); err != nil {
		return fmt.Errorf("%s did not finish: %w", phase, err)
	}
	return nil
}

// Fails every burst's errors on t, one each.
func FailBurstErrors(t testing.TB, bursts []*WorldBurst) {
	t.Helper()
	for _, burst := range bursts {
		for _, err := range burst.Errors() {
			t.Errorf("%v", err)
		}
	}
}

// The progress dump, as connect's failProgress: every block's /status and run count, every sidecar's state with its
// reason, its follower's position and its newest log lines, and every burst's progress and errors, logged, then t
// fails with phase and err. What a stalled phase
// shows, before cleanup turns the stall into a shutdown's error.
func (self *World) FailProgress(t testing.TB, phase string, err error, bursts []*WorldBurst) {
	t.Helper()
	lines := []string{}
	for _, block := range self.blocks {
		status, answered := block.Status()
		if !answered {
			status = "no answer"
		}
		lines = append(lines, fmt.Sprintf("  block %s: run %d, /status %q", block.name, block.RunCount(), status))
	}
	for _, network := range self.t3.Networks() {
		for _, device := range network.Devices() {
			status := device.Sidecar().Status()
			line := fmt.Sprintf("  sidecar %s: %s", device.Name, status.State)
			if status.Reason != "" {
				line += " (" + string(status.Reason) + ")"
			}
			if follower := device.Follower(); follower != nil {
				if client := follower.Client(); client != nil {
					line += fmt.Sprintf(", following at %d", client.Pin().GetVersion())
				}
			}
			lines = append(lines, line)
			for _, logLine := range self.SidecarLogLines(device.Name) {
				lines = append(lines, "    | "+logLine)
			}
		}
	}
	for _, burst := range bursts {
		lines = append(lines, "  "+burst.Progress())
		for _, burstErr := range burst.Errors() {
			lines = append(lines, "    "+burstErr.Error())
		}
	}
	t.Fatalf("%s stalled: %v\n%s", phase, err, strings.Join(lines, "\n"))
}

// The ledgers a scenario reconciles at its end, as they stand when read.
type WorldLedgers struct {
	// Each network's quota ledger, by its id, as server holds it once the debit journal is flushed.
	Networks map[server.Id]*WorldNetworkLedger
	// Each metadata cluster's write budget reports, by cluster id: every process's spend as it last reported it, each
	// block's and api's (§11.13's report:<process> fields, which expire 15 s after a process's last write).
	BudgetReports map[string][]*evsservice.WriteBudgetReport
	// api's process among them: the budget its sequencer and rev/'s store spend on (T3World.ApiWriteBudget).
	ApiProcessId string
	// Each network's service debits by meter, read from service_debit_journal before the flush applied them and from
	// network_service_usage after (WorldMeterLedger).
	Meters *WorldMeterLedger
}

// One network's quota ledger: its in-window transfer balances and its quota state.
type WorldNetworkLedger struct {
	// What its in-window balances started with, and what is left of them, in bytes.
	StartBalanceByteCount int64
	BalanceByteCount      int64
	// network_quota_state's stored bytes, the usage batches' stored deltas summed, and whether it is spent.
	StoredByteCount int64
	Spent           bool
}

// What the debits drew from the network's balances.
func (self *WorldNetworkLedger) DebitedByteCount() int64 {
	return self.StartBalanceByteCount - self.BalanceByteCount
}

// Each network's service debits by meter (§20.6's meters: evs, tls, mx, smtp and relay), as server holds them: what
// service_debit_journal held when the ledgers were read, before the flush applied it, and what network_service_usage
// holds once the flush has, with the rows the flush still left in the journal. A scenario reconciles the bytes it moved
// through a service with its meter here, exactly.
type WorldMeterLedger struct {
	// By network and meter: the journal's rows not yet applied when read, before the flush.
	Journaled map[server.Id]map[string]int64
	// By network and meter: network_service_usage's quantity summed over its days, after the flush: every row it
	// applied, the one an applied row's release still keeps in the journal included.
	Applied map[server.Id]map[string]int64
	// By network and meter: the journal's rows the flush left unapplied, a balance it found busy, which no balance
	// shows yet.
	Unapplied map[server.Id]map[string]int64
}

// Every byte networkId was debited on meter: what the flush applied, and what the journal still holds unapplied.
func (self *WorldMeterLedger) ByteCount(networkId server.Id, meter string) int64 {
	return self.Applied[networkId][meter] + self.Unapplied[networkId][meter]
}

// Every byte networkId was debited on every meter, applied or still journaled.
func (self *WorldMeterLedger) TotalByteCount(networkId server.Id) int64 {
	total := int64(0)
	for _, byteCount := range self.Applied[networkId] {
		total += byteCount
	}
	for _, byteCount := range self.Unapplied[networkId] {
		total += byteCount
	}
	return total
}

// The ledger's meters for networkId, journaled, applied and unapplied, as one line for a failure's message.
func (self *WorldMeterLedger) Describe(networkId server.Id) string {
	return fmt.Sprintf("journaled before the flush %v, applied %v, left unapplied %v", self.Journaled[networkId], self.Applied[networkId], self.Unapplied[networkId])
}

// The priced bytes every process reported against clusterId's write budget, P0 by kind, P1 and P2 together: what a
// scenario's writes reconcile with while the processes that spent them still report.
func (self *WorldLedgers) BudgetByteCount(clusterId string) int64 {
	total := int64(0)
	for _, report := range self.BudgetReports[clusterId] {
		for _, byteCount := range report.P0ByteCounts {
			total += byteCount
		}
		total += report.P1ByteCount + report.UnfundedByteCount + report.FundedByteCount
	}
	return total
}

// The ledgers once every block process's budget report was written at since or after: each network's quota ledger
// as server holds it, the debit journal flushed, every metadata cluster's budget reports from the world's Redis, and
// the api-side budget's violations. A process reports every WriteBudgetSettings.Interval (5 s), so
// a scenario passes the time its writes ended and reads what each process had spent by then; the zero time reads the
// reports as they stand. The reports are asked every BudgetPollInterval, since a report has no hook to wait on,
// within the hosts' StatusTimeout and the case's deadline.
func (self *World) ReadLedgers(ctx context.Context, since time.Time) (*WorldLedgers, error) {
	deadline := self.stepDeadline(self.HostsSettings().StatusTimeout)
	for {
		ledgers, err := self.readLedgers(ctx)
		if err != nil {
			return nil, err
		}
		stale := 0
		for _, reports := range ledgers.BudgetReports {
			for _, report := range reports {
				if report.TimeMs < since.UnixMilli() {
					stale += 1
				}
			}
		}
		if stale == 0 {
			return ledgers, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline:
			return nil, fmt.Errorf("%d budget reports stayed older than %s", stale, since)
		case <-time.After(self.HostsSettings().BudgetPollInterval):
		}
	}
}

// The ledgers as they stand: the debit journal read by meter, then flushed, then each network's balances, quota state
// and applied usage by meter.
func (self *World) readLedgers(ctx context.Context) (*WorldLedgers, error) {
	ledgers := &WorldLedgers{
		Networks:      map[server.Id]*WorldNetworkLedger{},
		BudgetReports: map[string][]*evsservice.WriteBudgetReport{},
		Meters: &WorldMeterLedger{
			Journaled: map[server.Id]map[string]int64{},
			Applied:   map[server.Id]map[string]int64{},
			Unapplied: map[server.Id]map[string]int64{},
		},
	}
	networks := self.t3.Networks()
	for _, network := range networks {
		unapplied, err := worldJournaledServiceDebits(ctx, network.NetworkId)
		if err != nil {
			return nil, fmt.Errorf("%s's debits: %w", network.Name, err)
		}
		ledgers.Meters.Journaled[network.NetworkId] = unapplied
	}
	if err := worldFlushServiceDebits(ctx); err != nil {
		return nil, err
	}
	for _, network := range networks {
		ledger, err := worldNetworkLedger(ctx, network.NetworkId)
		if err != nil {
			return nil, fmt.Errorf("%s's quota ledger: %w", network.Name, err)
		}
		ledgers.Networks[network.NetworkId] = ledger
		applied, err := worldAppliedServiceUsage(ctx, network.NetworkId)
		if err != nil {
			return nil, fmt.Errorf("%s's usage: %w", network.Name, err)
		}
		ledgers.Meters.Applied[network.NetworkId] = applied
		unapplied, err := worldJournaledServiceDebits(ctx, network.NetworkId)
		if err != nil {
			return nil, fmt.Errorf("%s's debits after the flush: %w", network.Name, err)
		}
		ledgers.Meters.Unapplied[network.NetworkId] = unapplied
	}
	host := self.t3.Host()
	for _, clusterId := range host.Blobs.MetadataClusterIds() {
		reports, err := evsservice.ReadWriteBudgetReports(ctx, host.Redis, clusterId)
		if err != nil {
			return nil, fmt.Errorf("%s's budget reports: %w", clusterId, err)
		}
		ledgers.BudgetReports[clusterId] = reports
	}
	if budget := self.t3.ApiWriteBudget(); budget != nil {
		ledgers.ApiProcessId = budget.ProcessId()
	}
	return ledgers, nil
}

// The priced bytes api's process reported against clusterId's write budget: its sequencer's and rev/'s writes, every
// one P0 (owner decision 186). 0 while api's report is absent.
func (self *WorldLedgers) ApiBudgetByteCount(clusterId string) int64 {
	total := int64(0)
	for _, report := range self.BudgetReports[clusterId] {
		if report.ProcessId != self.ApiProcessId || self.ApiProcessId == "" {
			continue
		}
		for _, byteCount := range report.P0ByteCounts {
			total += byteCount
		}
		total += report.P1ByteCount + report.UnfundedByteCount + report.FundedByteCount
	}
	return total
}
