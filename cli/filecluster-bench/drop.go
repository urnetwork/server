package main

// The drop mode (EVSB-M10-08; §22.8's drop row, §11.13's drop rows, §19.6's drop jobs row, D-OPS-21): at M10 on T3,
// the drop job's costs from its own operations run on the clusters, each in a window of its own between two readings
// of every drive's counters, on synthetic objects under the run's prefix, deleted after:
//
//	scan     the job's reads of a history: objects of a segment's size read whole, once at the job's pace
//	         (evsservice.DropSettings.ReadBytesPerSecond, 32 MB/s) as a job reads, and once as fast as they come as a
//	         dry run's snapshot may: the bytes a second each achieved and the HDDs' drive operations a MB
//	head     HEADs of seg/ objects at the job's DELETE pace, as F8 to F7 read each candidate: drive operations each
//	delete   a DELETE of each, naming the LastModified its HEAD read, as F9 issues it, at DeletesPerSecond (10):
//	         drive operations each on the segment cluster
//	table    on the metadata cluster, drops/<repo>/_'s overwrites at its working size, as each DROP's append and each
//	         running job's minute write it, and slot DELETEs: drive bytes and write IOs each on the busiest drive
//
// and from them the row's figures: dry-run and job scan rates, the HDD drive operations a second a running job
// takes, HEAD and DELETE costs, NVMe bytes per drop and per job-hour, and a 60 GB history's job time, computed from
// the measured rates: mark and verify each read the history once, resolve its dropped lineages' share, at the
// slower of the measured and the job's paced read rate, and the candidates' HEAD and DELETE go at the slower of the
// measured rate and DeletesPerSecond. A job pauses while the segment cluster's drive operations pass HddPauseShare
// of its capacity (§11.13's bound row), so with -capacity the scan and deletes here pause alike, and the time paused
// is reported.
//
// It is the job's operations, not a DropJob: a job over a real format-2 history needs format 2's writers composed,
// which no composition does yet, so the job time is computed and never measured, and the file says so.

import (
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/sidecartest/load"

	"github.com/urnetwork/server"
)

// The drop row's file's mode.
const ModeDropBench = "drop_bench"

// What the drop mode runs. DefaultDropBenchSettings is the run's.
type DropBenchSettings struct {
	// The job's bounds: evsservice's own, so the run paces as a job does.
	Drop *evsservice.DropSettings
	// The history the scan windows read: objects of a 4 MiB segment chunk, 256 MiB in all.
	ScanObjectByteCount int64
	ScanObjectCount     int
	// The candidates HEADed and DELETEd: 600, a minute of a job's deletes, each 64 KiB.
	CandidateCount     int
	CandidateByteCount int64
	// The table's working size, inside §11.8's 64 KiB bound, and its overwrites: 32 KiB and 120.
	TableByteCount      int64
	TableOverwriteCount int
	// The slots a job deletes, each 12 KiB, and how many: 200.
	SlotByteCount   int64
	SlotDeleteCount int
	// §22.8's history: 60 GB, which mark and verify each read once, and resolve this share of: 2 passes, a tenth.
	HistoryByteCount     int64
	HistoryScanPassCount float64
	HistoryResolveShare  float64
	// Of the history, the share a drop's candidates hold: a quarter.
	HistoryCandidateShare float64
	// The segment cluster's capacity in drive operations a second, past HddPauseShare of which the run pauses;
	// zero pauses nothing, which the run notes. How often the pause is checked: 5 s, the counters' interval.
	CapacityOps        int64
	PauseCheckInterval time.Duration
	// The most operations in flight, the reads of the scan's unpaced pass at once, and the distinct failure texts
	// kept: 64, 16 and 16.
	MaxInFlight     int
	ScanInFlight    int
	MaxFailureKinds int
	// The PUTs that make the objects go this many at once: 32.
	FillInFlight int
	// A run whose history read is less than this is a smoke: 1 GB.
	EvidenceScanByteCount int64
	CleanupListLimit      int
	CleanupInFlight       int
}

func DefaultDropBenchSettings() *DropBenchSettings {
	return &DropBenchSettings{
		Drop:                  evsservice.DefaultDropSettings(),
		ScanObjectByteCount:   4 * 1024 * 1024,
		ScanObjectCount:       64,
		CandidateCount:        600,
		CandidateByteCount:    64 * 1024,
		TableByteCount:        32 * 1024,
		TableOverwriteCount:   120,
		SlotByteCount:         12 * 1024,
		SlotDeleteCount:       200,
		HistoryByteCount:      60 * 1000 * 1000 * 1000,
		HistoryScanPassCount:  2,
		HistoryResolveShare:   0.1,
		HistoryCandidateShare: 0.25,
		PauseCheckInterval:    5 * time.Second,
		MaxInFlight:           64,
		ScanInFlight:          16,
		MaxFailureKinds:       16,
		FillInFlight:          32,
		EvidenceScanByteCount: 1000 * 1000 * 1000,
		CleanupListLimit:      1000,
		CleanupInFlight:       64,
	}
}

// One window of the drop mode's operations on one cluster.
type DropWindow struct {
	Name      string  `json:"name"`
	ClusterId string  `json:"cluster_id"`
	Seconds   float64 `json:"seconds"`
	// The seconds of it the pause held the load.
	PausedSeconds float64          `json:"paused_seconds"`
	Count         int64            `json:"count"`
	Failed        int64            `json:"failed"`
	ByteCount     int64            `json:"byte_count"`
	LatencyMillis load.Percentiles `json:"latency_ms"`
	Drives        []*DriveDelta    `json:"drives"`
}

// Every drive's reads and writes completed over the window, summed, and the busiest drive's written bytes and
// writes completed, with whether every drive was read.
func (self *DropWindow) driveFigures() (operations uint64, busiestBytes uint64, busiestWrites uint64, read bool) {
	if len(self.Drives) == 0 || anyMissing(self.Drives) {
		return 0, 0, 0, false
	}
	for _, drive := range self.Drives {
		operations += drive.ReadsCompleted + drive.WritesCompleted
		if !drive.Healing {
			busiestBytes = max(busiestBytes, drive.WrittenByteCount)
			busiestWrites = max(busiestWrites, drive.WritesCompleted)
		}
	}
	return operations, busiestBytes, busiestWrites, true
}

// A drop run's file.
type DropBenchMeasurement struct {
	RowHeader
	MetadataClusterId string        `json:"metadata_cluster_id"`
	Windows           []*DropWindow `json:"windows"`
}

// The window named name, nil when the run has none.
func (self *DropBenchMeasurement) Window(name string) *DropWindow {
	for _, window := range self.Windows {
		if window.Name == name {
			return window
		}
	}
	return nil
}

// The drop mode's windows, by name.
const (
	DropWindowScanPaced  = "scan-paced"
	DropWindowScanFull   = "scan-full"
	DropWindowHead       = "head"
	DropWindowDelete     = "delete"
	DropWindowTable      = "table"
	DropWindowSlotDelete = "slot-delete"
)

// §22.8's drop row's figures, computed from the windows.
type DropFigures struct {
	PacedScanBytesPerSecond float64 `json:"paced_scan_bytes_per_second"`
	FullScanBytesPerSecond  float64 `json:"full_scan_bytes_per_second"`
	HddOpsPerScanMegabyte   float64 `json:"hdd_ops_per_scan_megabyte"`
	HddOpsPerHead           float64 `json:"hdd_ops_per_head"`
	HddOpsPerDelete         float64 `json:"hdd_ops_per_delete"`
	DeletesPerSecond        float64 `json:"deletes_per_second"`
	// A running job's HDD drive operations a second: its paced reads and its deletes with their HEADs.
	HddOpsPerSecondRunning float64 `json:"hdd_ops_per_second_running"`
	// NVMe bytes and write IOs on the busiest drive.
	NvmeBytesPerTableWrite float64 `json:"nvme_bytes_per_table_write"`
	NvmeBytesPerSlotDelete float64 `json:"nvme_bytes_per_slot_delete"`
	NvmeBytesPerDrop       float64 `json:"nvme_bytes_per_drop"`
	NvmeBytesPerJobHour    float64 `json:"nvme_bytes_per_job_hour"`
	// The 60 GB history's job, computed: its scans, its candidates' HEADs and DELETEs, its slot DELETEs' bytes.
	HistoryCandidateCount float64  `json:"history_candidate_count"`
	HistoryJobSeconds     float64  `json:"history_job_seconds"`
	HistoryJobNvmeBytes   float64  `json:"history_job_nvme_bytes"`
	Computed              bool     `json:"computed"`
	Evidence              bool     `json:"evidence"`
	Reasons               []string `json:"reasons"`
}

// Computes the row's figures from a run: see the file header.
func ComputeDropFigures(measurement *DropBenchMeasurement, settings *DropBenchSettings) *DropFigures {
	figures := &DropFigures{Computed: true, Evidence: !measurement.Smoke && 0 < len(measurement.CommitSet)}
	if measurement.Smoke {
		figures.Reasons = append(figures.Reasons, "it is a smoke run")
	}
	if len(measurement.CommitSet) == 0 {
		figures.Reasons = append(figures.Reasons, "it names no build")
	}
	// a window priced per operation needs its operations; one read as a rate needs its time too
	need := func(name string, timed bool) *DropWindow {
		window := measurement.Window(name)
		if window == nil || window.Count <= 0 || (timed && window.Seconds-window.PausedSeconds <= 0) {
			figures.Evidence = false
			figures.Reasons = append(figures.Reasons, fmt.Sprintf("no %s window", name))
			return nil
		}
		if _, _, _, read := window.driveFigures(); !read {
			figures.Evidence = false
			figures.Reasons = append(figures.Reasons, fmt.Sprintf("the %s window read no drive's counters", name))
		}
		return window
	}
	busy := func(window *DropWindow) float64 { return window.Seconds - window.PausedSeconds }
	if window := need(DropWindowScanPaced, true); window != nil {
		figures.PacedScanBytesPerSecond = float64(window.ByteCount) / busy(window)
		if operations, _, _, read := window.driveFigures(); read && 0 < window.ByteCount {
			figures.HddOpsPerScanMegabyte = float64(operations) / (float64(window.ByteCount) / (1000 * 1000))
		}
	}
	if window := need(DropWindowScanFull, true); window != nil {
		figures.FullScanBytesPerSecond = float64(window.ByteCount) / busy(window)
	}
	if window := need(DropWindowHead, false); window != nil {
		if operations, _, _, read := window.driveFigures(); read {
			figures.HddOpsPerHead = float64(operations) / float64(window.Count)
		}
	}
	if window := need(DropWindowDelete, true); window != nil {
		figures.DeletesPerSecond = float64(window.Count) / busy(window)
		if operations, _, _, read := window.driveFigures(); read {
			figures.HddOpsPerDelete = float64(operations) / float64(window.Count)
		}
	}
	if window := need(DropWindowTable, false); window != nil {
		if _, busiest, _, read := window.driveFigures(); read {
			figures.NvmeBytesPerTableWrite = float64(busiest) / float64(window.Count)
		}
	}
	if window := need(DropWindowSlotDelete, false); window != nil {
		if _, busiest, _, read := window.driveFigures(); read {
			figures.NvmeBytesPerSlotDelete = float64(busiest) / float64(window.Count)
		}
	}
	jobReadRate := float64(settings.Drop.ReadBytesPerSecond)
	if 0 < figures.PacedScanBytesPerSecond {
		jobReadRate = min(jobReadRate, figures.PacedScanBytesPerSecond)
	}
	deleteRate := float64(settings.Drop.DeletesPerSecond)
	if 0 < figures.DeletesPerSecond {
		deleteRate = min(deleteRate, figures.DeletesPerSecond)
	}
	figures.HddOpsPerSecondRunning = jobReadRate/(1000*1000)*figures.HddOpsPerScanMegabyte + deleteRate*(figures.HddOpsPerHead+figures.HddOpsPerDelete)
	figures.NvmeBytesPerDrop = figures.NvmeBytesPerTableWrite
	minutesPerHour := time.Hour.Minutes() / settings.Drop.CheckpointInterval.Minutes()
	figures.NvmeBytesPerJobHour = minutesPerHour * figures.NvmeBytesPerTableWrite
	figures.HistoryCandidateCount = settings.HistoryCandidateShare * float64(settings.HistoryByteCount) / float64(settings.ScanObjectByteCount)
	if 0 < jobReadRate && 0 < deleteRate {
		scanned := (settings.HistoryScanPassCount + settings.HistoryResolveShare) * float64(settings.HistoryByteCount)
		figures.HistoryJobSeconds = scanned/jobReadRate + figures.HistoryCandidateCount/deleteRate
		jobHours := figures.HistoryJobSeconds / time.Hour.Seconds()
		figures.HistoryJobNvmeBytes = jobHours*figures.NvmeBytesPerJobHour + figures.NvmeBytesPerDrop
	}
	return figures
}

// The text of a run's figures, saying the job time is computed.
func WriteDropFigures(out io.Writer, measurement *DropBenchMeasurement, figures *DropFigures) error {
	b := &strings.Builder{}
	fmt.Fprintf(b, "§22.8's drop row on %s and %s, %s: the job's operations, not a drop job; the job time is computed\n",
		measurement.ClusterId, measurement.MetadataClusterId, measurement.Started.UTC().Format(time.RFC3339))
	fmt.Fprintf(b, "  scan: %.1f MB/s at the job's pace, %.1f MB/s unpaced, %.1f HDD operations a MB\n",
		figures.PacedScanBytesPerSecond/1e6, figures.FullScanBytesPerSecond/1e6, figures.HddOpsPerScanMegabyte)
	fmt.Fprintf(b, "  HEAD %.1f and DELETE %.1f HDD operations each, %.1f DELETEs a second; a running job %.0f HDD operations a second\n",
		figures.HddOpsPerHead, figures.HddOpsPerDelete, figures.DeletesPerSecond, figures.HddOpsPerSecondRunning)
	fmt.Fprintf(b, "  NVMe: %.0f B a table write, %.0f B a slot DELETE, %.0f B a drop, %.0f B a job-hour\n",
		figures.NvmeBytesPerTableWrite, figures.NvmeBytesPerSlotDelete, figures.NvmeBytesPerDrop, figures.NvmeBytesPerJobHour)
	fmt.Fprintf(b, "  a 60 GB history: %.0f candidates, %.1f h, %.0f B of NVMe writes, computed from the rates above\n",
		figures.HistoryCandidateCount, figures.HistoryJobSeconds/3600, figures.HistoryJobNvmeBytes)
	for _, reason := range figures.Reasons {
		fmt.Fprintf(b, "  no evidence: %s\n", reason)
	}
	_, err := io.WriteString(out, b.String())
	return err
}

// Holds the load while a cluster's drive operations pass the pause line, as a drop job pauses past half its segment
// cluster's capacity (§11.13's bound row) and §22.8's M10 hdd run stays under §19.6's 50% drive-operation pause.
// Unread counters pause, as a job pauses while no capacity is known. It keeps the wall time it held, once however
// many operations waited on it. Safe for concurrent use.
type loadPause struct {
	target   *BenchTarget
	line     float64
	interval time.Duration
	notes    *benchNotes

	stateLock   sync.Mutex
	last        *driveReading
	paused      bool
	pausedSince time.Time
	pausedTotal time.Duration
}

func newLoadPause(target *BenchTarget, line float64, interval time.Duration, notes *benchNotes) *loadPause {
	return &loadPause{target: target, line: line, interval: interval, notes: notes}
}

// Whether the load may go on now: the drives' operations a second since the last check, read at most each interval.
func (self *loadPause) proceed(ctx context.Context) bool {
	if self == nil || self.line <= 0 {
		return true
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	now := self.target.clock().Now()
	if self.last != nil && now.Sub(self.last.at) < self.interval {
		return !self.paused
	}
	reading := self.target.read(ctx, self.notes)
	if self.last != nil {
		seconds := readingSeconds(self.last, reading)
		operations := uint64(0)
		read := 0 < seconds
		deltas := driveDeltas(self.last, reading)
		for _, drive := range deltas {
			if drive.Missing {
				read = false
				break
			}
			operations += drive.ReadsCompleted + drive.WritesCompleted
		}
		paused := len(deltas) == 0 || !read || self.line < float64(operations)/seconds
		switch {
		case paused && !self.paused:
			self.pausedSince = now
		case !paused && self.paused:
			self.pausedTotal += now.Sub(self.pausedSince)
		}
		self.paused = paused
	}
	self.last = reading
	return !self.paused
}

// Waits out the pause, checking each interval, until the load may go on or ctx ends.
func (self *loadPause) wait(ctx context.Context) {
	if self == nil {
		return
	}
	clock := self.target.clock()
	for !self.proceed(ctx) {
		select {
		case <-ctx.Done():
			return
		case <-clock.After(self.interval):
		}
	}
}

// The wall time the pause has held so far.
func (self *loadPause) pausedDuration() time.Duration {
	if self == nil {
		return 0
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	total := self.pausedTotal
	if self.paused {
		total += self.target.clock().Now().Sub(self.pausedSince)
	}
	return total
}

// Runs one window of count operations of op at rate (unpaced when rate is zero, inFlight at once), between two
// readings of the target's counters, waiting out the pause before each.
func runDropWindow(ctx context.Context, target *BenchTarget, name string, count int, rate float64, inFlight int, settings *DropBenchSettings, pause *loadPause, notes *benchNotes, op func(ctx context.Context, n int64) (int64, error)) *DropWindow {
	clock := target.clock()
	window := &DropWindow{Name: name, ClusterId: target.ClusterId}
	latencies := newLatencySample(count, 9)
	var succeeded, failed, byteCount atomic.Int64
	pausedBefore := pause.pausedDuration()
	do := func(ctx context.Context, n int64) error {
		pause.wait(ctx)
		started := clock.Now()
		bytes, err := op(ctx, n)
		if err != nil {
			failed.Add(1)
			return err
		}
		latencies.add(clock.Now().Sub(started))
		succeeded.Add(1)
		byteCount.Add(bytes)
		return nil
	}
	before := target.read(ctx, notes)
	if 0 < rate {
		driver := load.NewDriver(&load.DriverSettings{
			Name:            "drop-" + name,
			Rate:            rate,
			Steady:          time.Duration(float64(count) / rate * float64(time.Second)),
			MaxInFlight:     settings.MaxInFlight,
			MaxFailureKinds: settings.MaxFailureKinds,
			Clock:           clock,
		}, func(ctx context.Context, arrival int64) error {
			if int64(count) <= arrival {
				return nil
			}
			return do(ctx, arrival)
		})
		report := driver.Run(ctx)
		for kind, kindCount := range report.FailureKinds {
			notes.add(fmt.Sprintf("%s: %d failed: %s", name, kindCount, kind))
		}
	} else {
		work := make(chan int64)
		var group sync.WaitGroup
		for range max(1, inFlight) {
			group.Go(func() {
				for n := range work {
					if err := do(ctx, n); err != nil {
						notes.add(fmt.Sprintf("%s: %v", name, err))
					}
				}
			})
		}
		for n := range int64(count) {
			select {
			case work <- n:
			case <-ctx.Done():
			}
			if ctx.Err() != nil {
				break
			}
		}
		close(work)
		group.Wait()
	}
	after := target.read(ctx, notes)
	window.Seconds = readingSeconds(before, after)
	window.PausedSeconds = (pause.pausedDuration() - pausedBefore).Seconds()
	window.Count = succeeded.Load()
	window.Failed = failed.Load()
	window.ByteCount = byteCount.Load()
	window.LatencyMillis = load.PercentilesOf(latencies.take())
	window.Drives = driveDeltas(before, after)
	return window
}

// Writes count objects of byteCount under keyOf, unmeasured, fillInFlight at once, streamed with their checksums as
// segments are on a segment cluster and created as a slot is otherwise.
func fillDropObjects(ctx context.Context, cluster sidecar.BlobCluster, segment bool, count int, byteCount int64, fillInFlight int, keyOf func(n int64) string) error {
	work := make(chan int64)
	var failed atomic.Int64
	var group sync.WaitGroup
	for range max(1, fillInFlight) {
		group.Go(func() {
			for n := range work {
				body := opsBody(byteCount, n, 0)
				var err error
				if segment {
					checksum := sha256.Sum256(body)
					_, err = cluster.PutStream(ctx, keyOf(n), strings.NewReader(string(body)), int64(len(body)), checksum[:])
				} else {
					_, err = cluster.PutIfNoneMatch(ctx, keyOf(n), body, nil)
				}
				if err != nil {
					failed.Add(1)
				}
			}
		})
	}
	for n := range int64(count) {
		select {
		case work <- n:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(work)
	group.Wait()
	if 0 < failed.Load() {
		return fmt.Errorf("%d of %d objects were not written", failed.Load(), count)
	}
	return ctx.Err()
}

// Runs the drop mode's windows into measurement: see the file header. segment is the segment cluster's target,
// metadata the metadata cluster's; each writes only under prefix, removed after.
func RunDropBench(ctx context.Context, segment *BenchTarget, metadata *BenchTarget, settings *DropBenchSettings, prefix string, measurement *DropBenchMeasurement) error {
	clock := segment.clock()
	notes := &benchNotes{}
	measurement.Mode = ModeDropBench
	measurement.ClusterId = segment.ClusterId
	measurement.MetadataClusterId = metadata.ClusterId
	measurement.Started = clock.Now()
	defer func() {
		measurement.Ended = clock.Now()
		measurement.Notes = append(measurement.Notes, notes.list()...)
	}()
	defer func() {
		for _, target := range []*BenchTarget{segment, metadata} {
			if err := cleanupPrefix(context.WithoutCancel(ctx), target.Cluster, prefix, settings.CleanupListLimit, settings.CleanupInFlight); err != nil {
				notes.add(fmt.Sprintf("cleanup left objects under %s on %s: %v", prefix, target.ClusterId, err))
			}
		}
	}()
	var pause *loadPause
	if 0 < settings.CapacityOps {
		pause = newLoadPause(segment, settings.Drop.HddPauseShare*float64(settings.CapacityOps), settings.PauseCheckInterval, notes)
	} else {
		notes.add("no -capacity: nothing paused the load, as a job pauses past half its cluster's capacity")
	}
	historyKey := func(n int64) string { return fmt.Sprintf("%sh%09d", prefix, n) }
	candidateKey := func(n int64) string { return fmt.Sprintf("%sc%09d", prefix, n) }
	tableKey := prefix + "drops/_"
	slotKey := func(n int64) string { return fmt.Sprintf("%ss%09d", prefix, n) }

	if err := fillDropObjects(ctx, segment.Cluster, true, settings.ScanObjectCount, settings.ScanObjectByteCount, settings.FillInFlight, historyKey); err != nil {
		return err
	}
	read := func(ctx context.Context, n int64) (int64, error) {
		object, err := segment.Cluster.Get(ctx, historyKey(n))
		if err != nil {
			return 0, err
		}
		defer object.Body.Close()
		return io.Copy(io.Discard, object.Body)
	}
	pacedRate := float64(settings.Drop.ReadBytesPerSecond) / float64(settings.ScanObjectByteCount)
	measurement.Windows = append(measurement.Windows,
		runDropWindow(ctx, segment, DropWindowScanPaced, settings.ScanObjectCount, pacedRate, settings.MaxInFlight, settings, pause, notes, read))
	if err := ctx.Err(); err != nil {
		return err
	}
	measurement.Windows = append(measurement.Windows,
		runDropWindow(ctx, segment, DropWindowScanFull, settings.ScanObjectCount, 0, settings.ScanInFlight, settings, pause, notes, read))

	if err := fillDropObjects(ctx, segment.Cluster, true, settings.CandidateCount, settings.CandidateByteCount, settings.FillInFlight, candidateKey); err != nil {
		return err
	}
	lastModifieds := make([]time.Time, settings.CandidateCount)
	deleteRate := float64(settings.Drop.DeletesPerSecond)
	measurement.Windows = append(measurement.Windows,
		runDropWindow(ctx, segment, DropWindowHead, settings.CandidateCount, deleteRate, settings.MaxInFlight, settings, pause, notes, func(ctx context.Context, n int64) (int64, error) {
			info, err := segment.Cluster.Head(ctx, candidateKey(n))
			if err != nil {
				return 0, err
			}
			lastModifieds[n] = info.LastModified
			return 0, nil
		}))
	measurement.Windows = append(measurement.Windows,
		runDropWindow(ctx, segment, DropWindowDelete, settings.CandidateCount, deleteRate, settings.MaxInFlight, settings, pause, notes, func(ctx context.Context, n int64) (int64, error) {
			if lastModifieds[n].IsZero() {
				return 0, errors.New("no HEAD read the candidate")
			}
			deadline := clock.Now().Add(settings.Drop.LeaseTime - settings.Drop.DeleteDeadlineMargin)
			return 0, segment.Cluster.Delete(sidecar.WithBlobUnmodified(sidecar.WithBlobDeadline(ctx, deadline), lastModifieds[n]), candidateKey(n))
		}))

	// the table: an overwrite at its working size, each a DROP's append or a running job's minute
	etag, err := metadata.Cluster.PutIfNoneMatch(ctx, tableKey, opsBody(settings.TableByteCount, 0, 0), nil)
	if err != nil {
		return err
	}
	var etagLock sync.Mutex
	measurement.Windows = append(measurement.Windows,
		runDropWindow(ctx, metadata, DropWindowTable, settings.TableOverwriteCount, 0, 1, settings, nil, notes, func(ctx context.Context, n int64) (int64, error) {
			etagLock.Lock()
			defer etagLock.Unlock()
			newEtag, err := metadata.Cluster.PutIfMatch(ctx, tableKey, etag, opsBody(settings.TableByteCount, 0, int(n)+1), nil)
			if err != nil {
				return 0, err
			}
			etag = newEtag
			return settings.TableByteCount, nil
		}))
	if err := fillDropObjects(ctx, metadata.Cluster, false, settings.SlotDeleteCount, settings.SlotByteCount, settings.FillInFlight, slotKey); err != nil {
		return err
	}
	measurement.Windows = append(measurement.Windows,
		runDropWindow(ctx, metadata, DropWindowSlotDelete, settings.SlotDeleteCount, deleteRate, settings.MaxInFlight, settings, nil, notes, func(ctx context.Context, n int64) (int64, error) {
			return 0, metadata.Cluster.Delete(ctx, slotKey(n))
		}))

	if int64(settings.ScanObjectCount)*settings.ScanObjectByteCount < settings.EvidenceScanByteCount {
		measurement.Smoke = true
		notes.add(fmt.Sprintf("a history of %d B read, under %d: a smoke", int64(settings.ScanObjectCount)*settings.ScanObjectByteCount, settings.EvidenceScanByteCount))
	}
	for _, window := range measurement.Windows {
		if anyMissing(window.Drives) || 0 < window.Failed {
			measurement.Smoke = true
			notes.add("a window read no drive's counters or had failures: a smoke")
			break
		}
	}
	return ctx.Err()
}

func runDropMode(ctx context.Context, args []string, stdout io.Writer) (bool, error) {
	defaults := DefaultDropBenchSettings()
	settings := *defaults
	flags := flag.NewFlagSet("filecluster-bench drop", flag.ContinueOnError)
	common := &runFlags{}
	common.register(flags, true)
	metadataClusterId := flags.String("metadata_cluster", "", "the metadata cluster the table and slot windows run on; the first when empty")
	figuresPath := flags.String("figures", "", "where the row's figures as text go, if anywhere")
	flags.Int64Var(&settings.CapacityOps, "capacity", 0, "the segment cluster's capacity in drive operations a second, past half of which the run pauses")
	flags.IntVar(&settings.ScanObjectCount, "scan_objects", defaults.ScanObjectCount, "the history's objects the scan reads")
	flags.IntVar(&settings.CandidateCount, "candidates", defaults.CandidateCount, "the candidates HEADed and DELETEd")
	if err := flags.Parse(args); err != nil {
		return false, err
	}
	clusters, err := openClusters(ctx, common.clustersPath)
	if err != nil {
		return false, err
	}
	defer clusters.Close()
	if common.clusterId == "" {
		segmentCluster, err := clusters.SegmentCluster()
		if err != nil {
			return false, refuseStart("%v", err)
		}
		common.clusterId = segmentCluster.ClusterId()
	}
	if kind := clusters.ClusterKind(common.clusterId); kind != server.FileClusterKindSegment {
		return false, refuseStart("%s is a %q cluster: the drop row's scans and deletes run on a segment cluster", common.clusterId, kind)
	}
	if *metadataClusterId == "" {
		*metadataClusterId = clusters.MetadataClusterIds()[0]
	}
	if kind := clusters.ClusterKind(*metadataClusterId); kind != server.FileClusterKindMetadata {
		return false, refuseStart("%s is a %q cluster: the table and slots are on a metadata cluster", *metadataClusterId, kind)
	}
	measurement := &DropBenchMeasurement{}
	if err := common.rowHeader(ctx, &measurement.RowHeader, ModeDropBench, clusters, common.clusterId, map[string]string{
		"capacity":     fmt.Sprint(settings.CapacityOps),
		"scan_objects": fmt.Sprint(settings.ScanObjectCount),
		"candidates":   fmt.Sprint(settings.CandidateCount),
	}); err != nil {
		return false, err
	}
	segment, err := common.target(ctx, clusters, common.clusterId)
	if err != nil {
		return false, err
	}
	metadata, err := common.target(ctx, clusters, *metadataClusterId)
	if err != nil {
		return false, err
	}
	runErr := RunDropBench(ctx, segment, metadata, &settings, runPrefix(), measurement)
	if err := writeRowFile(common.outPath, stdout, measurement); err != nil {
		return false, err
	}
	if runErr != nil {
		return false, runErr
	}
	if *figuresPath != "" {
		var text strings.Builder
		if err := WriteDropFigures(&text, measurement, ComputeDropFigures(measurement, &settings)); err != nil {
			return false, err
		}
		if err := os.WriteFile(*figuresPath, []byte(text.String()), 0o644); err != nil {
			return false, err
		}
	}
	return true, nil
}
