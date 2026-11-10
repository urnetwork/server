// The soak (§22.8's soak row, D-TEST-7; TEST-M2-14): 72 h at 30% of each service's capacity before each release,
// then heap, goroutines, Redis keys and orphan objects back to baseline. Run from a server checkout in the
// monoroot:
//
//	go run ./local/sidecar-soak [-short] [-target t2] [-duration 72h] [-fraction 0.3] [-warmup 10m] [-quiet 30m]
//	    [-subscribers 1000] [-out report.json]
//
// A run is six phases on one clock:
//
//	warm-up   the mix for Warmup, filling caches and pools
//	quiet     Quiet with no load, past every engine cache's life
//	baseline  SampleCount readings SampleInterval apart through load.Sampler, each after a collection, and the
//	          orphan count
//	load      the mix for Duration, each service at Fraction of its capacity, open loop (load.Driver), with
//	          readings every SampleInterval for the report's series
//	quiet     Quiet again, so what the load cached has expired and only what it leaked stands
//	end       readings as at the baseline, and each signal compared within its tolerance (baseline.go)
//
// It exits 0 when no signal leaked, 1 when one did and 2 when the run could not start. A signal read open is no
// leak and no pass: the report says why, and gateEvidence refuses it, as it refuses a short run, which -short
// makes: minutes of the same phases, a smoke that proves nothing. The full run is external, on the host the owner
// names for the nightly form (QD-239), and this program is what it runs.
//
// Targets. t2 runs evsservice's engine on sidecartest's fakes in this process, with load's EvsSubscriberWorld:
// commits to one ref at Fraction of §11.15's 20 pushes a second, each hinting every subscriber. Its fakes hold every
// object in this process's heap, so its heap signal reads open, and no evsservice count of orphans deletes nothing,
// so neither target reads orphans (question test-m2-03-soak-orphan-count). stack, the private stack's stores through
// sidecarhost on the attested Redis and the local MinIO, is not built: server's sidecart3 builds its hosts only
// inside go test, and the target refuses, so a run there cannot pass by running on fakes. The mix holds evs alone:
// each other service's driver is its area's (SOCK-M5-17, TLS-M5-26, SMTP-M9-32) and joins here when it lands.
//
// Nothing here dials a production port or reads vault.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/load"
)

// The soak's tunables.
type soakSettings struct {
	// The load phase: §22.8's 72 h.
	Duration time.Duration
	// The share of each service's capacity the mix runs at: §22.8's 30%.
	Fraction float64
	// The load before the baseline, which fills caches and pools.
	Warmup time.Duration
	// The wait after each load phase before its readings, past every engine cache's life: §11.9's head cache's
	// 10 min, the fold's idle 10 min, repo/'s 5 s.
	Quiet time.Duration
	// Readings a phase takes for its median, and the time between them. Each must be positive.
	SampleCount    int
	SampleInterval time.Duration
	Tolerances     *soakTolerances
	// Subscribers on the evs target's ref: §22.8's 1,000.
	Subscribers int
	// Minutes of the same phases, which prove nothing: the report is a smoke.
	Short bool
	// What phases read time from and wait on: nil is real time.
	Clock sidecartest.Clock

	// a test's: what a phase waits through, nil waiting on Clock
	wait func(ctx context.Context, wait time.Duration) error
	// a test's: how a phase's arrivals run, nil pacing each service with load.Driver on Clock
	pace func(ctx context.Context, service *soakService, rate float64, duration time.Duration) *load.Report
}

// §22.8's soak: 72 h at 30%, after 10 min of warm-up, read through 30 min of quiet, 5 readings 10 s apart.
func defaultSoakSettings() *soakSettings {
	return &soakSettings{
		Duration:       72 * time.Hour,
		Fraction:       0.3,
		Warmup:         10 * time.Minute,
		Quiet:          30 * time.Minute,
		SampleCount:    5,
		SampleInterval: 10 * time.Second,
		Tolerances:     defaultSoakTolerances(),
		Subscribers:    load.SubscribersPerRepo,
	}
}

// -short: the same phases in minutes, quiet past the head cache's 10 min, a smoke.
func shortSoakSettings() *soakSettings {
	settings := defaultSoakSettings()
	settings.Duration = 5 * time.Minute
	settings.Warmup = 30 * time.Second
	settings.Quiet = 11 * time.Minute
	settings.SampleCount = 3
	settings.SampleInterval = 5 * time.Second
	settings.Short = true
	return settings
}

// One service of the mix.
type soakService struct {
	Name string
	// Arrivals a second it is taken to sustain, and where that figure comes from.
	CapacityPerSecond float64
	CapacitySource    string
	Workload          load.Workload
	// the next arrival's number, so phases never repeat one
	arrivals int64
}

// What a soak runs on.
type soakTarget interface {
	Name() string
	Mix() []*soakService
	// What counts Redis keys; nil counts none.
	RedisKeys() load.RedisKeyCounter
	// What counts orphan objects by GC's definition; nil counts none, and the signal reads open.
	Orphans() orphanCounter
	// The signals it cannot read honestly, with why.
	Open() map[soakSignal]string
	Close()
}

// A soak's report.
type soakReport struct {
	Target   string            `json:"target"`
	Started  time.Time         `json:"started"`
	Ended    time.Time         `json:"ended"`
	Smoke    bool              `json:"smoke"`
	Settings map[string]string `json:"settings"`
	Warmup   []*load.Report    `json:"warmup"`
	Load     []*load.Report    `json:"load"`
	// readings every SampleInterval through the load phase
	Series   []load.Sample  `json:"series"`
	Baseline *soakReading   `json:"baseline"`
	End      *soakReading   `json:"end"`
	Verdicts []*soakVerdict `json:"verdicts"`
	// No signal leaked. A signal read open is not a leak, and not a pass's evidence either.
	Passed bool     `json:"passed"`
	Notes  []string `json:"notes"`
}

// Why the report cannot stand as D-TEST-7's soak evidence, or nil: a short run, a signal read open, a leak, a load
// phase that failed attempts, or a target on fakes.
func (self *soakReport) gateEvidence() error {
	reasons := []string{}
	if self.Smoke {
		reasons = append(reasons, "it is a short run, a smoke that proves nothing")
	}
	if self.Target != "stack" {
		reasons = append(reasons, fmt.Sprintf("its target, %s, runs on fakes", self.Target))
	}
	for _, verdict := range self.Verdicts {
		switch verdict.Outcome {
		case soakOpen:
			reasons = append(reasons, fmt.Sprintf("%s read open: %s", verdict.Signal, verdict.Why))
		case soakLeaked:
			reasons = append(reasons, fmt.Sprintf("%s leaked: %s", verdict.Signal, verdict.Why))
		}
	}
	for _, report := range self.Load {
		if 0 < report.Failed {
			reasons = append(reasons, fmt.Sprintf("%s failed %d attempts", report.Name, report.Failed))
		}
	}
	if len(reasons) == 0 {
		return nil
	}
	return errors.New("the soak is no evidence: " + strings.Join(reasons, "; "))
}

// Runs the soak's six phases on target and reports.
func runSoak(ctx context.Context, settings *soakSettings, target soakTarget) *soakReport {
	clock := settings.Clock
	if clock == nil {
		clock = sidecar.RealClock{}
	}
	wait := settings.wait
	if wait == nil {
		wait = func(ctx context.Context, duration time.Duration) error {
			select {
			case <-clock.After(duration):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	report := &soakReport{
		Target:  target.Name(),
		Started: clock.Now(),
		Smoke:   settings.Short,
		Settings: map[string]string{
			"duration": settings.Duration.String(), "fraction": fmt.Sprint(settings.Fraction), "warmup": settings.Warmup.String(),
			"quiet": settings.Quiet.String(), "sample_count": fmt.Sprint(settings.SampleCount), "sample_interval": settings.SampleInterval.String(),
			"heap_growth": fmt.Sprint(settings.Tolerances.HeapGrowth), "heap_slack_bytes": fmt.Sprint(settings.Tolerances.HeapSlackBytes),
			"goroutine_slack": fmt.Sprint(settings.Tolerances.GoroutineSlack), "redis_key_slack": fmt.Sprint(settings.Tolerances.RedisKeySlack),
			"orphan_slack": fmt.Sprint(settings.Tolerances.OrphanSlack), "subscribers": fmt.Sprint(settings.Subscribers),
		},
	}
	for _, service := range target.Mix() {
		report.Settings["capacity "+service.Name] = fmt.Sprintf("%g/s (%s)", service.CapacityPerSecond, service.CapacitySource)
	}
	sampler := load.NewSampler(&load.SamplerSettings{Interval: settings.SampleInterval, Clock: clock}, target.RedisKeys())
	fail := func(phase string, err error) *soakReport {
		report.Notes = append(report.Notes, fmt.Sprintf("the %s phase ended early: %v", phase, err))
		report.Ended = clock.Now()
		return report
	}
	read := func() (*soakReading, error) {
		reading := &soakReading{Orphans: -1}
		for i := range settings.SampleCount {
			runtime.GC()
			reading.Samples = append(reading.Samples, sampler.Sample(ctx))
			if i < settings.SampleCount-1 {
				if err := wait(ctx, settings.SampleInterval); err != nil {
					return nil, err
				}
			}
		}
		counter := target.Orphans()
		if counter == nil {
			reading.OrphansWhy = "no orphan count: evsservice counts none that deletes nothing (question test-m2-03-soak-orphan-count)"
			return reading, nil
		}
		orphans, err := counter.CountOrphans(ctx)
		if err != nil {
			reading.OrphansWhy = "the orphan count failed: " + err.Error()
			return reading, nil
		}
		reading.Orphans = orphans
		return reading, nil
	}

	report.Warmup = runSoakMix(ctx, settings, clock, target.Mix(), settings.Warmup)
	if err := wait(ctx, settings.Quiet); err != nil {
		return fail("first quiet", err)
	}
	baseline, err := read()
	if err != nil {
		return fail("baseline", err)
	}
	report.Baseline = baseline

	seriesCtx, stopSeries := context.WithCancel(ctx)
	series := load.NewSampler(&load.SamplerSettings{Interval: settings.SampleInterval, Clock: clock}, target.RedisKeys())
	seriesDone := make(chan struct{})
	if settings.pace == nil {
		go func() {
			defer close(seriesDone)
			series.Run(seriesCtx)
		}()
	} else {
		// a stepped run moves its own clock; a series would only race it
		close(seriesDone)
	}
	report.Load = runSoakMix(ctx, settings, clock, target.Mix(), settings.Duration)
	stopSeries()
	<-seriesDone
	report.Series = series.Samples()

	if err := wait(ctx, settings.Quiet); err != nil {
		return fail("second quiet", err)
	}
	end, err := read()
	if err != nil {
		return fail("end", err)
	}
	report.End = end
	report.Verdicts = compareSoak(report.Baseline, report.End, settings.Tolerances, target.Open())
	report.Passed = !slices.ContainsFunc(report.Verdicts, func(verdict *soakVerdict) bool { return verdict.Outcome == soakLeaked })
	report.Ended = clock.Now()
	return report
}

// Every service of mix at Fraction of its capacity for duration, together, each report in the mix's order.
func runSoakMix(ctx context.Context, settings *soakSettings, clock sidecartest.Clock, mix []*soakService, duration time.Duration) []*load.Report {
	reports := make([]*load.Report, len(mix))
	if duration <= 0 {
		return reports[:0]
	}
	group := sync.WaitGroup{}
	for i, service := range mix {
		rate := settings.Fraction * service.CapacityPerSecond
		if settings.pace != nil {
			// a test's steps run one service after another, on the clock it moves
			reports[i] = settings.pace(ctx, service, rate, duration)
			continue
		}
		first := service.arrivals
		driver := load.NewDriver(&load.DriverSettings{
			Name: service.Name, Rate: rate, Steady: duration, MaxFailureKinds: 16, Clock: clock,
		}, func(ctx context.Context, arrival int64) error {
			return service.Workload(ctx, first+arrival)
		})
		group.Add(1)
		go func() {
			defer group.Done()
			reports[i] = driver.Run(ctx)
			service.arrivals = first + reports[i].Arrivals
		}()
	}
	group.Wait()
	return reports
}

// The t2 target: see the file header.
type t2SoakTarget struct {
	fakes *sidecartest.FakeHost
	world *load.EvsSubscriberWorld
	mix   []*soakService
}

// §11.15's concurrency row: one ref sustains roughly 20 to 50 pushes a second; the soak takes the low end.
const t2EvsCapacityPerSecond = 20

func newT2SoakTarget(ctx context.Context, clock sidecartest.Clock, subscribers int) (*t2SoakTarget, error) {
	if clock == nil {
		clock = sidecar.RealClock{}
	}
	fakes := sidecartest.NewFakeHost(ctx, clock, nil, nil, sidecartest.DefaultFakeHostSettings())
	worldSettings := load.DefaultEvsSubscriberWorldSettings()
	worldSettings.Subscribers = subscribers
	world, err := load.NewEvsSubscriberWorld(ctx, fakes, fakes.ServiceHost(), clock, worldSettings)
	if err != nil {
		fakes.Redis.Close()
		return nil, err
	}
	workloadSettings := load.DefaultSubscriberSettings()
	workloadSettings.Driver = &load.DriverSettings{Name: "evs-commit-hint", Rate: 1, Steady: time.Second, Clock: clock}
	workload := load.NewSubscriberWorkload(workloadSettings, world)
	return &t2SoakTarget{
		fakes: fakes,
		world: world,
		mix: []*soakService{{
			Name:              "evs-commit-hint",
			CapacityPerSecond: t2EvsCapacityPerSecond,
			CapacitySource:    "§11.15's concurrency row, one ref's 20 to 50 pushes a second at its low end",
			Workload:          workload.Attempt,
		}},
	}, nil
}

func (self *t2SoakTarget) Name() string {
	return "t2"
}

func (self *t2SoakTarget) Mix() []*soakService {
	return self.mix
}

func (self *t2SoakTarget) RedisKeys() load.RedisKeyCounter {
	return &load.FakeRedisKeys{Redis: self.fakes.Redis}
}

func (self *t2SoakTarget) Orphans() orphanCounter {
	return nil
}

func (self *t2SoakTarget) Open() map[soakSignal]string {
	return map[soakSignal]string{
		soakSignalHeap: "t2's fake stores hold every object in this process's heap, which grows with the history they keep: the heap is the stack target's to read",
	}
}

func (self *t2SoakTarget) Close() {
	self.world.Close()
	self.fakes.Redis.Close()
}

// Why the stack target refuses.
var errSoakStackUnbuilt = errors.New("the stack target is not built: the private stack's engines need sidecarhost.NewServiceHost over the attested Redis and the local MinIO, which server's sidecart3 builds only inside go test (TEST-M2-14's gap)")

func openSoakTarget(ctx context.Context, name string, clock sidecartest.Clock, subscribers int) (soakTarget, error) {
	switch name {
	case "t2":
		return newT2SoakTarget(ctx, clock, subscribers)
	case "stack":
		return nil, errSoakStackUnbuilt
	}
	return nil, fmt.Errorf("no target %q: t2 or stack", name)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// The program: 0 when nothing leaked, 1 when a signal did, 2 when it could not run.
func run(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("sidecar-soak", flag.ContinueOnError)
	flags.SetOutput(stderr)
	short := flags.Bool("short", false, "minutes of the same phases, a smoke that proves nothing")
	targetName := flags.String("target", "t2", "t2 (fakes in this process) or stack (the private stack, not built)")
	duration := flags.Duration("duration", 0, "the load phase (0 keeps the run's default: 72h, or 5m with -short)")
	fraction := flags.Float64("fraction", 0, "the share of each service's capacity (0 keeps 0.3)")
	warmup := flags.Duration("warmup", 0, "the load before the baseline (0 keeps the run's default)")
	quiet := flags.Duration("quiet", 0, "each quiet period, which a cache's life must fit in for the comparison to mean anything (0 keeps the run's default)")
	subscribers := flags.Int("subscribers", 0, "subscribers on the evs ref (0 keeps 1000)")
	out := flags.String("out", "", "where the JSON report goes (empty: stdout)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	settings := defaultSoakSettings()
	if *short {
		settings = shortSoakSettings()
	}
	if 0 < *duration {
		settings.Duration = *duration
	}
	if 0 < *fraction {
		settings.Fraction = *fraction
	}
	if 0 < *warmup {
		settings.Warmup = *warmup
	}
	if 0 < *quiet {
		settings.Quiet = *quiet
	}
	if 0 < *subscribers {
		settings.Subscribers = *subscribers
	}
	target, err := openSoakTarget(ctx, *targetName, settings.Clock, settings.Subscribers)
	if err != nil {
		fmt.Fprintf(stderr, "sidecar-soak: %v\n", err)
		return 2
	}
	defer target.Close()
	report := runSoak(ctx, settings, target)
	reportBytes, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "sidecar-soak: %v\n", err)
		return 2
	}
	if *out == "" {
		stdout.Write(append(reportBytes, '\n'))
	} else if err := os.WriteFile(*out, reportBytes, 0o644); err != nil {
		fmt.Fprintf(stderr, "sidecar-soak: %v\n", err)
		return 2
	}
	if report.Verdicts == nil {
		// a phase ended early: nothing was compared
		fmt.Fprintf(stderr, "sidecar-soak: %s\n", strings.Join(report.Notes, "; "))
		return 2
	}
	if evidence := report.gateEvidence(); evidence != nil {
		fmt.Fprintf(stderr, "sidecar-soak: %v\n", evidence)
	}
	if !report.Passed {
		return 1
	}
	return 0
}
