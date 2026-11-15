package main

// The hdd mode (EVSB-M3-03; §22.8's hdd row as g0-recheck-0-hdd-trigger amends it, QD-182 and QD-200; §19.6's hdd
// row, §19.9's hdd trigger, D-OPS-17), on a segment cluster before it holds data:
//
//	capacity   small PUTs and GETs at year two's 1:4 ratio in 10-minute steps, each 25% above the last, the page cache
//	           dropped on every host before each step and no object read twice within one, until achieved drive
//	           operations (the HDDs' /sys/block reads and writes completed, fields 1 and 5) rise under 5% from the step
//	           before; capacity is that plateau, the larger of its last two steps, with its p99, and it is what the
//	           operator records as config/<env>/sidecar.yml's file_clusters.<cluster>.hdd_capacity_ops, which §19.6's
//	           hdd loop publishes in {nvb_<cluster>}
//	year one   1 h at year one's mix (~40 PUTs and ~160 GETs a second) with §11.13's HDD admission on at 50% of that
//	           capacity: every upload must be admitted
//	year two   1 h at year two's (~400 and ~1,600): admitted PUTs and GETs a second, the refusal rate and p99; while
//	           refusals reach 0.1% of PUTs, shorter runs step the PUT rate down by a tenth until they do not, and the
//	           highest PUT rate admitted with refusals under 0.1% is §19.9's hdd trigger's admitted throughput, neither
//	           a higher line nor refused uploads counting as throughput
//
// Admission is evsservice's own, in this process: an HddLoop publishing the cluster's drive operations and the
// measured capacity into an in-process store, and an HddAdmission refusing the heaviest of NetworkCount networks'
// uploads past the line, as evs's segment route does. The page cache is dropped through -drop_caches_command, a
// template run per host; a run without it notes so and is a smoke, since cached reads would raise the plateau.
// Objects are written with PutStream under the run's prefix, as segments are, and removed after.

import (
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/load"

	"github.com/urnetwork/server"
)

// The modes of the hdd, hosts and trash rows' files.
const (
	ModeHdd   = "hdd"
	ModeHosts = "hosts"
	ModeTrash = "trash"
)

// What the hdd mode runs. DefaultHddSettings is the run's.
type HddSettings struct {
	// A small segment's size: 16 KiB, inline in its xl.meta on every drive of its set (§19.6's basis).
	ObjectByteCount int64
	// Year two's 1:4 PUT to GET ratio: a fifth of operations are PUTs.
	PutShare float64
	// The capacity step's first offered rate, operations a second over PUTs and GETs: 1,000, half of year two's.
	CapacityStartRate float64
	// Each step's rate over the last's, its window, the rise under which the drive operations have plateaued, and
	// the most steps before the run gives up finding one: 1.25, 10 min, 5% and 24.
	CapacityStepFactor   float64
	CapacityStepDuration time.Duration
	CapacityPlateauRise  float64
	CapacityMaxSteps     int
	// Year one's and year two's PUTs and GETs a second (§19.6): 40 and 160, 400 and 1,600.
	YearOnePutsPerSecond float64
	YearOneGetsPerSecond float64
	YearTwoPutsPerSecond float64
	YearTwoGetsPerSecond float64
	// Each mix run's window: 1 h.
	MixDuration time.Duration
	// The share of PUTs refused under which a rate counts as admitted: 0.1%.
	MaxRefusalShare float64
	// Each search step's PUT rate over the last's, its window and the most steps: 0.9, 10 min and 20.
	SearchFactor   float64
	SearchDuration time.Duration
	SearchMaxSteps int
	// The networks the uploads come from, round in turn, for the fair queue's heaviest-first rule: 100.
	NetworkCount int
	// The HDDs a GET asks when nothing is cached, for the page-cache hit estimate: 16, a set's drives (§19.6's
	// basis).
	ReadsPerGetUncached float64
	// The most operations in flight, and the distinct failure texts kept: 4,096 and 16.
	MaxInFlight     int
	MaxFailureKinds int
	// The most latencies kept per operation in a window, a uniform sample past it: 1,000,000.
	LatencySampleCount int
	// The PUTs that fill the read pool before a step go this many at once: 256.
	FillInFlight int
	// How long admission's figures take to reach the admitter before a run starts: 15 s, three intervals.
	AdmissionSettle time.Duration
	// Runs shorter than these are smokes.
	EvidenceStepDuration time.Duration
	EvidenceMixDuration  time.Duration
	// Cleanup's listing and deletes in flight.
	CleanupListLimit int
	CleanupInFlight  int
}

func DefaultHddSettings() *HddSettings {
	return &HddSettings{
		ObjectByteCount:      16 * 1024,
		PutShare:             0.2,
		CapacityStartRate:    1000,
		CapacityStepFactor:   1.25,
		CapacityStepDuration: 10 * time.Minute,
		CapacityPlateauRise:  0.05,
		CapacityMaxSteps:     24,
		YearOnePutsPerSecond: 40,
		YearOneGetsPerSecond: 160,
		YearTwoPutsPerSecond: 400,
		YearTwoGetsPerSecond: 1600,
		MixDuration:          time.Hour,
		MaxRefusalShare:      0.001,
		SearchFactor:         0.9,
		SearchDuration:       10 * time.Minute,
		SearchMaxSteps:       20,
		NetworkCount:         100,
		ReadsPerGetUncached:  16,
		MaxInFlight:          4096,
		MaxFailureKinds:      16,
		LatencySampleCount:   1000 * 1000,
		FillInFlight:         256,
		AdmissionSettle:      15 * time.Second,
		EvidenceStepDuration: 10 * time.Minute,
		EvidenceMixDuration:  time.Hour,
		CleanupListLimit:     1000,
		CleanupInFlight:      64,
	}
}

// The header of an hdd, hosts or trash file: who ran what, on which build and cluster.
type RowHeader struct {
	Mode      string            `json:"mode"`
	Label     string            `json:"label"`
	Host      string            `json:"host"`
	Smoke     bool              `json:"smoke"`
	Started   time.Time         `json:"started"`
	Ended     time.Time         `json:"ended"`
	Notes     []string          `json:"notes"`
	CommitSet map[string]string `json:"commit_set"`
	Settings  map[string]string `json:"settings"`
	ClusterId string            `json:"cluster_id"`
}

// One window of PUTs and GETs on the segment cluster.
type HddWindow struct {
	Name                 string           `json:"name"`
	OfferedPutsPerSecond float64          `json:"offered_puts_per_second"`
	OfferedGetsPerSecond float64          `json:"offered_gets_per_second"`
	Seconds              float64          `json:"seconds"`
	PutsStored           int64            `json:"puts_stored"`
	PutsRefused          int64            `json:"puts_refused"`
	PutsFailed           int64            `json:"puts_failed"`
	GetsServed           int64            `json:"gets_served"`
	GetsFailed           int64            `json:"gets_failed"`
	PutLatencyMillis     load.Percentiles `json:"put_latency_ms"`
	GetLatencyMillis     load.Percentiles `json:"get_latency_ms"`
	// Every HDD's reads and writes completed over the window, a second, summed over the cluster.
	DriveOpsPerSecond float64 `json:"drive_ops_per_second"`
	DriveReads        uint64  `json:"drive_reads"`
	DrivesRead        bool    `json:"drives_read"`
	// The page cache was dropped on every host before the window.
	CacheDropped bool `json:"cache_dropped"`
}

// The PUTs a second the window stored, and the share of offered PUTs refused.
func (self *HddWindow) admittedPutsPerSecond() float64 {
	if self.Seconds <= 0 {
		return 0
	}
	return float64(self.PutsStored) / self.Seconds
}

func (self *HddWindow) refusalShare() float64 {
	offered := self.PutsStored + self.PutsRefused + self.PutsFailed
	if offered == 0 {
		return 0
	}
	return float64(self.PutsRefused) / float64(offered)
}

// The share of GETs the page cache answered, by how few of the drive reads a GET asks for came: an estimate, from
// §19.6's basis that an uncached GET asks every drive of its set.
func (self *HddWindow) cacheHitEstimate(readsPerGet float64) float64 {
	if self.GetsServed == 0 || readsPerGet <= 0 || !self.DrivesRead {
		return 0
	}
	return max(0, min(1, 1-float64(self.DriveReads)/(float64(self.GetsServed)*readsPerGet)))
}

// An hdd run's file.
type HddMeasurement struct {
	RowHeader
	CapacitySteps []*HddWindow `json:"capacity_steps"`
	YearOne       *HddWindow   `json:"year_one"`
	YearTwo       *HddWindow   `json:"year_two"`
	Search        []*HddWindow `json:"search"`
	// The capacity admission ran against, which the capacity step found unless the operator gave one.
	AdmissionCapacityOps int64 `json:"admission_capacity_ops"`
}

// What an hdd run found (§22.8's hdd row, §19.9's hdd trigger).
type HddVerdict struct {
	// The plateau, 0 when none was reached.
	CapacityOps         int64   `json:"capacity_ops"`
	CapacityPutP99      float64 `json:"capacity_put_p99"`
	CapacityGetP99      float64 `json:"capacity_get_p99"`
	PlateauReached      bool    `json:"plateau_reached"`
	YearOneAdmitsAll    bool    `json:"year_one_admits_all"`
	YearTwoRefusalShare float64 `json:"year_two_refusal_share"`
	// §19.9's hdd trigger's admitted throughput, 0 when no rate was admitted under the share.
	AdmittedPutsPerSecond float64  `json:"admitted_puts_per_second"`
	YearTwoCacheHit       float64  `json:"year_two_cache_hit"`
	Evidence              bool     `json:"evidence"`
	Pass                  bool     `json:"pass"`
	Reasons               []string `json:"reasons"`
}

// The plateau of a capacity step's windows: the first step whose drive operations rose under rise from the one before
// it ends the search, and capacity is the larger of the two; none when no step rose so little.
func hddCapacityPlateau(steps []*HddWindow, rise float64) (int, bool) {
	for i := 1; i < len(steps); i += 1 {
		previous, step := steps[i-1], steps[i]
		if !previous.DrivesRead || !step.DrivesRead {
			return 0, false
		}
		if step.DriveOpsPerSecond < previous.DriveOpsPerSecond*(1+rise) {
			if previous.DriveOpsPerSecond > step.DriveOpsPerSecond {
				return i - 1, true
			}
			return i, true
		}
	}
	return 0, false
}

// Judges an hdd run: see the file header.
func JudgeHdd(measurement *HddMeasurement, settings *HddSettings) *HddVerdict {
	verdict := &HddVerdict{Evidence: !measurement.Smoke}
	if measurement.Smoke {
		verdict.Reasons = append(verdict.Reasons, "it is a smoke run, which proves nothing")
	}
	if len(measurement.CommitSet) == 0 {
		verdict.Evidence = false
		verdict.Reasons = append(verdict.Reasons, "it names no build")
	}
	if index, ok := hddCapacityPlateau(measurement.CapacitySteps, settings.CapacityPlateauRise); ok {
		step := measurement.CapacitySteps[index]
		verdict.PlateauReached = true
		verdict.CapacityOps = int64(step.DriveOpsPerSecond)
		verdict.CapacityPutP99 = step.PutLatencyMillis.P99
		verdict.CapacityGetP99 = step.GetLatencyMillis.P99
		for _, step := range measurement.CapacitySteps {
			if !step.CacheDropped {
				verdict.Evidence = false
				verdict.Reasons = append(verdict.Reasons, "a capacity step ran with the page cache not dropped")
				break
			}
		}
	} else {
		verdict.Reasons = append(verdict.Reasons, "the capacity step reached no plateau")
	}
	if one := measurement.YearOne; one != nil {
		verdict.YearOneAdmitsAll = one.PutsRefused == 0 && 0 < one.PutsStored
		if !verdict.YearOneAdmitsAll {
			verdict.Reasons = append(verdict.Reasons, fmt.Sprintf("year one's mix refused %d uploads, where it must admit every one", one.PutsRefused))
		}
	} else {
		verdict.Reasons = append(verdict.Reasons, "no year one run")
	}
	if two := measurement.YearTwo; two != nil {
		verdict.YearTwoRefusalShare = two.refusalShare()
		verdict.YearTwoCacheHit = two.cacheHitEstimate(settings.ReadsPerGetUncached)
		// the highest PUT rate admitted with refusals under the share, year two's own or a search step's
		for _, window := range append([]*HddWindow{two}, measurement.Search...) {
			if window.refusalShare() < settings.MaxRefusalShare && 0 < window.PutsStored {
				verdict.AdmittedPutsPerSecond = max(verdict.AdmittedPutsPerSecond, window.admittedPutsPerSecond())
			}
		}
		if verdict.AdmittedPutsPerSecond == 0 {
			verdict.Reasons = append(verdict.Reasons, "no PUT rate was admitted with refusals under the share")
		}
	} else {
		verdict.Reasons = append(verdict.Reasons, "no year two run")
	}
	verdict.Pass = verdict.Evidence && verdict.PlateauReached && verdict.YearOneAdmitsAll && 0 < verdict.AdmittedPutsPerSecond
	return verdict
}

// The verdict as text, ending in the line the operator records (plan-EVSB-settle-gate's sidecar.yml).
func WriteHddVerdict(out io.Writer, measurement *HddMeasurement, verdict *HddVerdict, settings *HddSettings) error {
	b := &strings.Builder{}
	fmt.Fprintf(b, "§22.8's hdd run on %s, %s: %s\n", measurement.ClusterId, measurement.Started.UTC().Format(time.RFC3339), map[bool]string{true: "pass", false: "fail"}[verdict.Pass])
	for _, step := range measurement.CapacitySteps {
		fmt.Fprintf(b, "  step %-9s %7.0f ops/s offered: %8.0f drive ops/s, put p99 %.1f ms, get p99 %.1f ms, cache dropped %t\n",
			step.Name, step.OfferedPutsPerSecond+step.OfferedGetsPerSecond, step.DriveOpsPerSecond, step.PutLatencyMillis.P99, step.GetLatencyMillis.P99, step.CacheDropped)
	}
	for _, window := range append([]*HddWindow{measurement.YearOne, measurement.YearTwo}, measurement.Search...) {
		if window == nil {
			continue
		}
		fmt.Fprintf(b, "  %-12s %6.0f puts/s offered, %6.1f admitted, refusals %.4f%%, %6.1f gets/s, put p99 %.1f ms, get p99 %.1f ms, cache hit ~%.2f\n",
			window.Name, window.OfferedPutsPerSecond, window.admittedPutsPerSecond(), 100*window.refusalShare(), float64(window.GetsServed)/max(window.Seconds, 1),
			window.PutLatencyMillis.P99, window.GetLatencyMillis.P99, window.cacheHitEstimate(settings.ReadsPerGetUncached))
	}
	for _, reason := range verdict.Reasons {
		fmt.Fprintf(b, "  %s\n", reason)
	}
	fmt.Fprintf(b, "  admitted throughput for §19.9's hdd trigger: %.1f PUTs a second\n", verdict.AdmittedPutsPerSecond)
	if verdict.Pass {
		fmt.Fprintf(b, "config/<env>/sidecar.yml: file_clusters: {%s: {hdd_capacity_ops: %d}}\n", measurement.ClusterId, verdict.CapacityOps)
	} else {
		fmt.Fprintf(b, "no hdd_capacity_ops to record: the run did not pass\n")
	}
	_, err := io.WriteString(out, b.String())
	return err
}

// Admits a segment PUT, or refuses it, and counts what the cluster stored: evsservice.HddAdmission's two calls.
type hddAdmitter interface {
	Admit(networkId connect.Id, byteCount int64, class evsservice.HddUploadClass) error
	CountPut()
	Close()
}

// Makes the admission the mix runs go through, against capacityOps.
type hddAdmissionFactory func(ctx context.Context, capacityOps int64) (hddAdmitter, error)

// evsservice's HDD share in this process: an HddLoop over the cluster's counters, publishing capacityOps, and an
// HddAdmission reading what it publishes, on an in-process store.
type inProcessHddAdmission struct {
	*evsservice.HddAdmission
	cancel context.CancelFunc
	wait   sync.WaitGroup
}

func newInProcessHddAdmission(ctx context.Context, store sidecar.BlobStore, clusterId string, counters DriveCounterReader, capacityOps int64) (*inProcessHddAdmission, error) {
	admissionCtx, cancel := context.WithCancel(ctx)
	fakes := sidecartest.NewFakeHost(admissionCtx, sidecar.RealClock{}, nil, nil, nil)
	if err := fakes.Gates.Set(fmt.Sprintf("sidecar:\n  file_clusters:\n    %s:\n      hdd_capacity_ops: %d\n", clusterId, capacityOps)); err != nil {
		cancel()
		return nil, err
	}
	host := fakes.ServiceHost()
	host.Blobs = store
	loop, err := evsservice.NewHddLoop(host, clusterId, &engineDriveCounters{counters: counters}, sidecar.NewNoopLogger(), nil)
	if err != nil {
		cancel()
		return nil, err
	}
	admission, err := evsservice.NewHddAdmission(admissionCtx, host, clusterId, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	self := &inProcessHddAdmission{HddAdmission: admission, cancel: cancel}
	self.wait.Go(func() { loop.Run(admissionCtx) })
	return self, nil
}

func (self *inProcessHddAdmission) Close() {
	self.cancel()
	self.HddAdmission.Close()
	self.wait.Wait()
}

// Every host's page cache dropped through the operator's template, {host} each host's name from admin info.
func dropCachesCommand(template string, adminInfo func() *server.FileClusterAdminInfo, run func(ctx context.Context, command string) ([]byte, error)) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		info := adminInfo()
		if info == nil {
			return errors.New("admin info unread")
		}
		hosts := []string{}
		for _, host := range info.Hosts {
			name, _, err := net.SplitHostPort(strings.TrimPrefix(strings.TrimPrefix(host.Endpoint, "http://"), "https://"))
			if err != nil {
				name = host.Endpoint
			}
			if !hostStatsHostPattern.MatchString(name) {
				return fmt.Errorf("a host name the shell must not see: %q", name)
			}
			hosts = append(hosts, name)
		}
		if len(hosts) == 0 {
			return errors.New("admin info lists no host")
		}
		for _, host := range hosts {
			if _, err := run(ctx, strings.ReplaceAll(template, "{host}", host)); err != nil {
				return fmt.Errorf("%s: %w", host, err)
			}
		}
		return nil
	}
}

// One hdd run's target and parts.
type hddRun struct {
	target     *BenchTarget
	settings   *HddSettings
	prefix     string
	notes      *benchNotes
	dropCaches func(ctx context.Context) error
	// the objects GETs read, filled before each window, each read once a window, and the windows' PUTs so far
	poolCount atomic.Int64
	putCount  atomic.Int64
}

// The pool's objects, which GETs read, and the windows' own PUTs, which nothing reads, under keys of one length.
func (self *hddRun) poolKey(n int64) string {
	return fmt.Sprintf("%sp%012d", self.prefix, n)
}

func (self *hddRun) putKey(n int64) string {
	return fmt.Sprintf("%sw%012d", self.prefix, n)
}

func (self *hddRun) put(ctx context.Context, key string, n int64) error {
	body := opsBody(self.settings.ObjectByteCount, n, 0)
	checksum := sha256.Sum256(body)
	_, err := self.target.Cluster.PutStream(ctx, key, strings.NewReader(string(body)), int64(len(body)), checksum[:])
	return err
}

// Fills the pool to count objects, unmeasured.
func (self *hddRun) fill(ctx context.Context, count int64) error {
	work := make(chan int64)
	var failed atomic.Int64
	var wait sync.WaitGroup
	for range max(1, self.settings.FillInFlight) {
		wait.Go(func() {
			for n := range work {
				if err := self.put(ctx, self.poolKey(n), n); err != nil {
					failed.Add(1)
				}
			}
		})
	}
	for n := self.poolCount.Load(); n < count; n += 1 {
		select {
		case work <- n:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(work)
	wait.Wait()
	if 0 < failed.Load() {
		return fmt.Errorf("%d pool PUTs failed", failed.Load())
	}
	self.poolCount.Store(max(self.poolCount.Load(), count))
	return ctx.Err()
}

// One window at putRate and getRate for duration: PUTs of new objects through admission when it is given, GETs of
// pool objects each read once.
func (self *hddRun) window(ctx context.Context, name string, putRate float64, getRate float64, duration time.Duration, admission hddAdmitter, dropCache bool) (*HddWindow, error) {
	clock := self.target.clock()
	rate := putRate + getRate
	window := &HddWindow{Name: name, OfferedPutsPerSecond: putRate, OfferedGetsPerSecond: getRate}
	// GETs read no object twice: the pool holds at least the window's GETs
	if err := self.fill(ctx, int64(getRate*duration.Seconds())+1); err != nil {
		return nil, err
	}
	if dropCache {
		if self.dropCaches == nil {
			self.notes.add("no -drop_caches_command: the page cache was not dropped")
		} else if err := self.dropCaches(ctx); err != nil {
			self.notes.add(fmt.Sprintf("the page cache was not dropped: %v", err))
		} else {
			window.CacheDropped = true
		}
	}
	var putsStored, putsRefused, putsFailed, getsServed, getsFailed atomic.Int64
	putLatencies := newLatencySample(self.settings.LatencySampleCount, 7)
	getLatencies := newLatencySample(self.settings.LatencySampleCount, 8)
	var nextGet atomic.Int64
	putEvery := 0.0
	if 0 < putRate {
		putEvery = rate / putRate
	}
	before := self.target.read(ctx, self.notes)
	driver := load.NewDriver(&load.DriverSettings{
		Name:            "hdd-" + name,
		Rate:            rate,
		Steady:          duration,
		MaxInFlight:     self.settings.MaxInFlight,
		MaxFailureKinds: self.settings.MaxFailureKinds,
		Clock:           clock,
	}, func(ctx context.Context, arrival int64) error {
		start := clock.Now()
		// arrival n is a PUT when it crosses the next multiple of putEvery, so PUTs are spread evenly at their share
		isPut := 0 < putEvery && int64(float64(arrival+1)/putEvery) != int64(float64(arrival)/putEvery)
		if isPut {
			n := self.putCount.Add(1) - 1
			if admission != nil {
				networkId := connect.Id(hddNetworkId(n % int64(max(1, self.settings.NetworkCount))))
				if err := admission.Admit(networkId, self.settings.ObjectByteCount, evsservice.HddUploadOrdinary); err != nil {
					putsRefused.Add(1)
					return err
				}
			}
			if err := self.put(ctx, self.putKey(n), n); err != nil {
				putsFailed.Add(1)
				return err
			}
			if admission != nil {
				admission.CountPut()
			}
			putsStored.Add(1)
			putLatencies.add(clock.Now().Sub(start))
			return nil
		}
		n := nextGet.Add(1) - 1
		if self.poolCount.Load() <= n {
			getsFailed.Add(1)
			return errors.New("the pool holds no object not read in this window")
		}
		object, err := self.target.Cluster.Get(ctx, self.poolKey(n))
		if err == nil {
			_, err = io.Copy(io.Discard, object.Body)
			object.Body.Close()
		}
		if err != nil {
			getsFailed.Add(1)
			return err
		}
		getsServed.Add(1)
		getLatencies.add(clock.Now().Sub(start))
		return nil
	})
	report := driver.Run(ctx)
	after := self.target.read(ctx, self.notes)
	for kind, count := range report.FailureKinds {
		self.notes.add(fmt.Sprintf("%s: %d failed: %s", name, count, kind))
	}
	window.Seconds = readingSeconds(before, after)
	window.PutsStored = putsStored.Load()
	window.PutsRefused = putsRefused.Load()
	window.PutsFailed = putsFailed.Load()
	window.GetsServed = getsServed.Load()
	window.GetsFailed = getsFailed.Load()
	window.PutLatencyMillis = load.PercentilesOf(putLatencies.take())
	window.GetLatencyMillis = load.PercentilesOf(getLatencies.take())
	deltas := driveDeltas(before, after)
	window.DrivesRead = 0 < len(deltas) && !anyMissing(deltas) && 0 < window.Seconds
	if window.DrivesRead {
		var operations uint64
		for _, drive := range deltas {
			operations += drive.ReadsCompleted + drive.WritesCompleted
			window.DriveReads += drive.ReadsCompleted
		}
		window.DriveOpsPerSecond = float64(operations) / window.Seconds
	}
	return window, ctx.Err()
}

// The networks uploads come from: fixed ids, so a run repeats.
func hddNetworkId(n int64) sidecar.Id {
	var id sidecar.Id
	id[0] = 0x0b
	id[8] = byte(n >> 8)
	id[9] = byte(n)
	return id
}

// Runs the hdd mode into measurement: see the file header. capacityOps, when positive, is the capacity admission runs
// against in place of the step's plateau.
func RunHdd(ctx context.Context, target *BenchTarget, settings *HddSettings, prefix string, dropCaches func(ctx context.Context) error, capacityOps int64, newAdmission hddAdmissionFactory, measurement *HddMeasurement) error {
	clock := target.clock()
	notes := &benchNotes{}
	measurement.Mode = ModeHdd
	measurement.ClusterId = target.ClusterId
	measurement.Started = clock.Now()
	defer func() {
		measurement.Ended = clock.Now()
		measurement.Notes = append(measurement.Notes, notes.list()...)
	}()
	run := &hddRun{target: target, settings: settings, prefix: prefix, notes: notes, dropCaches: dropCaches}
	defer func() {
		if err := cleanupPrefix(context.WithoutCancel(ctx), target.Cluster, prefix, settings.CleanupListLimit, settings.CleanupInFlight); err != nil {
			notes.add(fmt.Sprintf("cleanup left objects under %s: %v", prefix, err))
		}
	}()

	if capacityOps <= 0 {
		rate := settings.CapacityStartRate
		for step := range settings.CapacityMaxSteps {
			window, err := run.window(ctx, fmt.Sprintf("capacity-%02d", step), rate*settings.PutShare, rate*(1-settings.PutShare), settings.CapacityStepDuration, nil, true)
			if window != nil {
				measurement.CapacitySteps = append(measurement.CapacitySteps, window)
			}
			if err != nil {
				return err
			}
			if index, ok := hddCapacityPlateau(measurement.CapacitySteps, settings.CapacityPlateauRise); ok {
				capacityOps = int64(measurement.CapacitySteps[index].DriveOpsPerSecond)
				break
			}
			if !window.DrivesRead {
				notes.add("a capacity step read no drive's counters: no plateau can be found")
				break
			}
			rate *= settings.CapacityStepFactor
		}
	} else {
		notes.add(fmt.Sprintf("admission ran against the given capacity, %d drive operations a second, not a measured one", capacityOps))
	}
	measurement.AdmissionCapacityOps = capacityOps
	if capacityOps <= 0 {
		notes.add("no capacity: the mix runs were not run, since admission would refuse nothing")
	} else {
		admission, err := newAdmission(ctx, capacityOps)
		if err != nil {
			return err
		}
		defer admission.Close()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-clock.After(settings.AdmissionSettle):
		}
		measurement.YearOne, err = run.window(ctx, "year-one", settings.YearOnePutsPerSecond, settings.YearOneGetsPerSecond, settings.MixDuration, admission, false)
		if err != nil {
			return err
		}
		measurement.YearTwo, err = run.window(ctx, "year-two", settings.YearTwoPutsPerSecond, settings.YearTwoGetsPerSecond, settings.MixDuration, admission, false)
		if err != nil {
			return err
		}
		putRate := settings.YearTwoPutsPerSecond
		last := measurement.YearTwo
		for step := 0; step < settings.SearchMaxSteps && settings.MaxRefusalShare <= last.refusalShare(); step += 1 {
			putRate *= settings.SearchFactor
			last, err = run.window(ctx, fmt.Sprintf("search-%02d", step), putRate, putRate*settings.YearTwoGetsPerSecond/settings.YearTwoPutsPerSecond, settings.SearchDuration, admission, false)
			if last != nil {
				measurement.Search = append(measurement.Search, last)
			}
			if err != nil {
				return err
			}
		}
	}
	if settings.CapacityStepDuration < settings.EvidenceStepDuration || settings.MixDuration < settings.EvidenceMixDuration {
		measurement.Smoke = true
		notes.add("windows shorter than §22.8's 10 min steps and 1 h runs: a smoke")
	}
	for _, step := range measurement.CapacitySteps {
		if !step.DrivesRead || !step.CacheDropped {
			measurement.Smoke = true
			notes.add("a capacity step read no counters or kept the page cache: a smoke")
			break
		}
	}
	return nil
}

func runHddMode(ctx context.Context, args []string, stdout io.Writer) (bool, error) {
	defaults := DefaultHddSettings()
	settings := *defaults
	flags := flag.NewFlagSet("filecluster-bench hdd", flag.ContinueOnError)
	common := &runFlags{}
	common.register(flags, true)
	dropCachesTemplate := flags.String("drop_caches_command", "", "a command that drops one host's page cache; {host} names the host")
	capacity := flags.Int64("capacity", 0, "run admission against this capacity, drive operations a second, in place of the capacity step")
	verdictPath := flags.String("verdict", "", "where the verdict's text goes, if anywhere")
	flags.DurationVar(&settings.CapacityStepDuration, "step", defaults.CapacityStepDuration, "each capacity step's window")
	flags.Float64Var(&settings.CapacityStartRate, "start_rate", defaults.CapacityStartRate, "the first capacity step's operations a second")
	flags.IntVar(&settings.CapacityMaxSteps, "max_steps", defaults.CapacityMaxSteps, "the most capacity steps")
	flags.DurationVar(&settings.MixDuration, "mix", defaults.MixDuration, "each mix run's window")
	flags.DurationVar(&settings.SearchDuration, "search", defaults.SearchDuration, "each search step's window")
	flags.Float64Var(&settings.YearOnePutsPerSecond, "year_one_puts", defaults.YearOnePutsPerSecond, "year one's PUTs a second")
	flags.Float64Var(&settings.YearOneGetsPerSecond, "year_one_gets", defaults.YearOneGetsPerSecond, "year one's GETs a second")
	flags.Float64Var(&settings.YearTwoPutsPerSecond, "year_two_puts", defaults.YearTwoPutsPerSecond, "year two's PUTs a second")
	flags.Float64Var(&settings.YearTwoGetsPerSecond, "year_two_gets", defaults.YearTwoGetsPerSecond, "year two's GETs a second")
	if err := flags.Parse(args); err != nil {
		return false, err
	}
	clusters, err := openClusters(ctx, common.clustersPath)
	if err != nil {
		return false, err
	}
	defer clusters.Close()
	if common.clusterId == "" {
		segment, err := clusters.SegmentCluster()
		if err != nil {
			return false, refuseStart("%v", err)
		}
		common.clusterId = segment.ClusterId()
	}
	if kind := clusters.ClusterKind(common.clusterId); kind != server.FileClusterKindSegment {
		return false, refuseStart("%s is a %q cluster: the hdd row runs on a segment cluster", common.clusterId, kind)
	}
	measurement := &HddMeasurement{}
	if err := common.rowHeader(ctx, &measurement.RowHeader, ModeHdd, clusters, common.clusterId, map[string]string{
		"object_size": fmt.Sprint(settings.ObjectByteCount),
		"start_rate":  fmt.Sprint(settings.CapacityStartRate),
		"step":        settings.CapacityStepDuration.String(),
		"mix":         settings.MixDuration.String(),
		"capacity":    fmt.Sprint(*capacity),
	}); err != nil {
		return false, err
	}
	target, err := common.target(ctx, clusters, common.clusterId)
	if err != nil {
		return false, err
	}
	target.Store = clusters
	var dropCaches func(ctx context.Context) error
	if *dropCachesTemplate != "" {
		dropCaches = dropCachesCommand(*dropCachesTemplate, func() *server.FileClusterAdminInfo {
			info, err := clusters.ReadAdminInfo(ctx, common.clusterId)
			if err != nil {
				return nil
			}
			return info
		}, func(ctx context.Context, command string) ([]byte, error) {
			return execCommand(ctx, "/", "/bin/sh", "-c", command)
		})
	}
	newAdmission := func(ctx context.Context, capacityOps int64) (hddAdmitter, error) {
		return newInProcessHddAdmission(ctx, clusters, common.clusterId, target.Counters, capacityOps)
	}
	runErr := RunHdd(ctx, target, &settings, runPrefix(), dropCaches, *capacity, newAdmission, measurement)
	if err := writeRowFile(common.outPath, stdout, measurement); err != nil {
		return false, err
	}
	if runErr != nil {
		return false, runErr
	}
	verdict := JudgeHdd(measurement, &settings)
	if *verdictPath != "" {
		file, err := os.Create(*verdictPath)
		if err != nil {
			return false, err
		}
		defer file.Close()
		if err := WriteHddVerdict(file, measurement, verdict, &settings); err != nil {
			return false, err
		}
	}
	return true, nil
}
