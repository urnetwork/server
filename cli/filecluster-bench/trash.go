package main

// The trash mode (EVSB-M3-03; §22.8's trash row, §19.6's MinIO row): year two's per-set overwrite rate on a metadata
// cluster for 1 h at the trash sweep interval the cluster runs with (MINIO_API_DELETE_CLEANUP_INTERVAL, which the
// playbook sets and the operator declares with -sweep), recording each drive's .trash size and inodes through the
// host stats command every sample, the sweep's throughput (what .trash lost between samples, a second), and the
// drive bytes, raw bytes and inodes per overwrite. Run once at 5 min and once at 30 s; the verdict picks 30 s only if
// it helps, by §19.6's words: its peak .trash inodes lower and its busiest drive's bytes per overwrite no higher than
// the 5 min run's, within a tolerance, else the 5 min default.
//
// Year two's per-set overwrite rate: §11.13's year two of 2,000 pushes a second on the five to six sets §19.9 sizes,
// ~360 pushes a set, each overwriting its head and ~0.9 folded entries, ~700 overwrites a second; a setting. At M10
// (-purges, EVSB-M10-08) the row runs again with versionless purges at year two's rate beside the overwrites: slots
// created and deleted the purge's allowance later (purge_load.go), and its two runs are judged as their own pair.
//
// .trash is sampled on a schedule set from the window's start, each sample's timer armed before the load begins, so
// a run on a fake clock samples at each interval however its goroutines are scheduled.

import (
	"context"
	"encoding/json"
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
	"github.com/urnetwork/sidecar/sidecartest/load"

	"github.com/urnetwork/server"
)

// What the trash mode runs. DefaultTrashSettings is the run's.
type TrashSettings struct {
	// Overwrites a second: ~700, year two's per set (see the file header).
	OverwritesPerSecond float64
	// The objects overwritten in turn, and their size: 100,000 and 4 KiB, a small metadata object's.
	ObjectCount     int64
	ObjectByteCount int64
	// The window: 1 h.
	Duration time.Duration
	// How often .trash is read: 30 s, so a 30 s sweep is seen.
	SampleInterval time.Duration
	// How far 30 s's bytes per overwrite may sit above 5 min's and still help: 5%.
	HelpTolerance   float64
	MaxInFlight     int
	MaxFailureKinds int
	// The PUTs that make the objects go this many at once: 256.
	FillInFlight     int
	EvidenceDuration time.Duration
	CleanupListLimit int
	CleanupInFlight  int
	// M10's slot purges beside the overwrites; nil runs M3's row.
	Purges *PurgeLoadSettings
}

func DefaultTrashSettings() *TrashSettings {
	return &TrashSettings{
		OverwritesPerSecond: 700,
		ObjectCount:         100000,
		ObjectByteCount:     4 * 1024,
		Duration:            time.Hour,
		SampleInterval:      30 * time.Second,
		HelpTolerance:       0.05,
		MaxInFlight:         4096,
		MaxFailureKinds:     16,
		FillInFlight:        256,
		EvidenceDuration:    time.Hour,
		CleanupListLimit:    1000,
		CleanupInFlight:     64,
	}
}

// One read of every drive's .trash.
type TrashSample struct {
	Seconds float64 `json:"seconds"`
	// Summed over the drives that answered, and the most any one held.
	ByteCount          uint64 `json:"byte_count"`
	InodeCount         uint64 `json:"inode_count"`
	MaxDriveByteCount  uint64 `json:"max_drive_byte_count"`
	MaxDriveInodeCount uint64 `json:"max_drive_inode_count"`
	Drives             int    `json:"drives"`
}

// A trash run's file.
type TrashMeasurement struct {
	RowHeader
	// The sweep interval the cluster ran with, as the operator declared it.
	SweepInterval time.Duration    `json:"sweep_interval"`
	Seconds       float64          `json:"seconds"`
	Overwrites    int64            `json:"overwrites"`
	Failed        int64            `json:"failed"`
	LatencyMillis load.Percentiles `json:"latency_ms"`
	Drives        []*DriveDelta    `json:"drives"`
	RawUsageDelta int64            `json:"raw_usage_delta"`
	InodeDelta    int64            `json:"inode_delta"`
	Samples       []*TrashSample   `json:"samples"`
	// M10's run: the purges beside the overwrites.
	M10    bool             `json:"m10,omitempty"`
	Purges *PurgeLoadCounts `json:"purges,omitempty"`
}

// A run's figures per overwrite and its .trash.
type TrashFigures struct {
	SweepInterval        time.Duration `json:"sweep_interval"`
	BytesPerOverwrite    float64       `json:"bytes_per_overwrite"`
	RawBytesPerOverwrite float64       `json:"raw_bytes_per_overwrite"`
	InodesPerOverwrite   float64       `json:"inodes_per_overwrite"`
	PeakTrashByteCount   uint64        `json:"peak_trash_byte_count"`
	PeakTrashInodeCount  uint64        `json:"peak_trash_inode_count"`
	// What .trash lost between samples, a second, over the window.
	SweepBytesPerSecond float64 `json:"sweep_bytes_per_second"`
	DrivesRead          bool    `json:"drives_read"`
	TrashRead           bool    `json:"trash_read"`
}

func trashFigures(measurement *TrashMeasurement) *TrashFigures {
	figures := &TrashFigures{SweepInterval: measurement.SweepInterval}
	if busiest, ok := busiestDrive(measurement.Drives); ok && !anyMissing(measurement.Drives) && 0 < measurement.Overwrites {
		figures.DrivesRead = true
		figures.BytesPerOverwrite = float64(busiest.WrittenByteCount) / float64(measurement.Overwrites)
		figures.RawBytesPerOverwrite = float64(measurement.RawUsageDelta) / float64(measurement.Overwrites)
		figures.InodesPerOverwrite = float64(measurement.InodeDelta) / float64(measurement.Overwrites)
	}
	var swept uint64
	for i, sample := range measurement.Samples {
		figures.PeakTrashByteCount = max(figures.PeakTrashByteCount, sample.ByteCount)
		figures.PeakTrashInodeCount = max(figures.PeakTrashInodeCount, sample.InodeCount)
		if 0 < i && sample.ByteCount < measurement.Samples[i-1].ByteCount {
			swept += measurement.Samples[i-1].ByteCount - sample.ByteCount
		}
	}
	figures.TrashRead = 1 < len(measurement.Samples)
	if figures.TrashRead && 0 < measurement.Seconds {
		figures.SweepBytesPerSecond = float64(swept) / measurement.Seconds
	}
	return figures
}

// What the trash row's two runs decide: §19.6's MINIO_API_DELETE_CLEANUP_INTERVAL.
type TrashVerdict struct {
	M10      bool            `json:"m10"`
	Runs     []*TrashFigures `json:"runs"`
	Interval time.Duration   `json:"interval"`
	Pass     bool            `json:"pass"`
	Reasons  []string        `json:"reasons"`
}

// Judges the 5 min and 30 s runs: see the file header. M3's pair and M10's are each a pair of their own.
func JudgeTrash(measurements []*TrashMeasurement, settings *TrashSettings) *TrashVerdict {
	verdict := &TrashVerdict{}
	for _, measurement := range measurements {
		if measurement.M10 != measurements[0].M10 {
			verdict.Reasons = append(verdict.Reasons, "M3's runs and M10's are judged apart, each pair alone")
			return verdict
		}
		if measurement.M10 && (measurement.Purges == nil || measurement.Purges.Puts == 0 || 0 < measurement.Purges.Failed) {
			verdict.Reasons = append(verdict.Reasons, fmt.Sprintf("the %s M10 run's purges did not run whole", measurement.SweepInterval))
		}
	}
	verdict.M10 = 0 < len(measurements) && measurements[0].M10
	byInterval := map[time.Duration]*TrashFigures{}
	for _, measurement := range measurements {
		if measurement.Smoke {
			verdict.Reasons = append(verdict.Reasons, fmt.Sprintf("the %s run is a smoke, which proves nothing", measurement.SweepInterval))
		}
		if len(measurement.CommitSet) == 0 {
			verdict.Reasons = append(verdict.Reasons, fmt.Sprintf("the %s run names no build", measurement.SweepInterval))
		}
		figures := trashFigures(measurement)
		verdict.Runs = append(verdict.Runs, figures)
		byInterval[measurement.SweepInterval] = figures
	}
	slow, fast := byInterval[5*time.Minute], byInterval[30*time.Second]
	if slow == nil || fast == nil {
		verdict.Reasons = append(verdict.Reasons, "the row needs a run at 5 min and one at 30 s")
		return verdict
	}
	for _, figures := range []*TrashFigures{slow, fast} {
		if !figures.DrivesRead || !figures.TrashRead {
			verdict.Reasons = append(verdict.Reasons, fmt.Sprintf("the %s run read no drive counters or no .trash", figures.SweepInterval))
		}
	}
	verdict.Interval = 5 * time.Minute
	if fast.PeakTrashInodeCount < slow.PeakTrashInodeCount && fast.BytesPerOverwrite <= slow.BytesPerOverwrite*(1+settings.HelpTolerance) {
		verdict.Interval = 30 * time.Second
	}
	verdict.Pass = len(verdict.Reasons) == 0
	return verdict
}

func WriteTrashVerdict(out io.Writer, verdict *TrashVerdict) error {
	b := &strings.Builder{}
	fmt.Fprintf(b, "§22.8's trash row%s: %s\n", map[bool]string{true: " at M10, with purges", false: ""}[verdict.M10], map[bool]string{true: "pass", false: "fail"}[verdict.Pass])
	for _, run := range verdict.Runs {
		fmt.Fprintf(b, "  sweep %-6s %.0f B a drive, %.0f raw bytes and %.3f inodes an overwrite; .trash peak %d B, %d inodes; swept %.0f B/s\n",
			run.SweepInterval, run.BytesPerOverwrite, run.RawBytesPerOverwrite, run.InodesPerOverwrite, run.PeakTrashByteCount, run.PeakTrashInodeCount, run.SweepBytesPerSecond)
	}
	for _, reason := range verdict.Reasons {
		fmt.Fprintf(b, "  %s\n", reason)
	}
	if verdict.Pass {
		fmt.Fprintf(b, "MINIO_API_DELETE_CLEANUP_INTERVAL=%s (file_cluster_delete_cleanup_interval)\n", verdict.Interval)
	}
	_, err := io.WriteString(out, b.String())
	return err
}

// Reads every drive's .trash through the host stats command.
type TrashReader interface {
	ReadTrash(ctx context.Context, clusterId string) (*TrashSample, error)
}

// Runs the trash mode into measurement: see the file header.
func RunTrash(ctx context.Context, target *BenchTarget, trash TrashReader, settings *TrashSettings, prefix string, measurement *TrashMeasurement) error {
	clock := target.clock()
	notes := &benchNotes{}
	measurement.Mode = ModeTrash
	measurement.ClusterId = target.ClusterId
	measurement.Started = clock.Now()
	defer func() {
		measurement.Ended = clock.Now()
		measurement.Notes = append(measurement.Notes, notes.list()...)
	}()
	defer func() {
		if err := cleanupPrefix(context.WithoutCancel(ctx), target.Cluster, prefix, settings.CleanupListLimit, settings.CleanupInFlight); err != nil {
			notes.add(fmt.Sprintf("cleanup left objects under %s: %v", prefix, err))
		}
	}()
	// the objects, unmeasured
	objects := &opsObjects{etags: map[int64]string{}}
	work := make(chan int64)
	var failedFill atomic.Int64
	var wait sync.WaitGroup
	for range max(1, settings.FillInFlight) {
		wait.Go(func() {
			for n := range work {
				etag, err := target.Cluster.PutIfNoneMatch(ctx, opsKey(prefix, settings.ObjectByteCount, n), opsBody(settings.ObjectByteCount, n, 0), nil)
				if err != nil {
					failedFill.Add(1)
					continue
				}
				objects.set(n, etag)
			}
		})
	}
	for n := range settings.ObjectCount {
		work <- n
	}
	close(work)
	wait.Wait()
	if 0 < failedFill.Load() {
		return fmt.Errorf("%d of the objects were not made", failedFill.Load())
	}

	start := clock.Now()
	sampleLock := sync.Mutex{}
	sample := func() {
		if trash == nil {
			notes.add("no host stats command: .trash unread")
			return
		}
		reading, err := trash.ReadTrash(ctx, target.ClusterId)
		if err != nil {
			notes.add(fmt.Sprintf(".trash unread: %v", err))
			return
		}
		sampleLock.Lock()
		defer sampleLock.Unlock()
		reading.Seconds = clock.Now().Sub(start).Seconds()
		measurement.Samples = append(measurement.Samples, reading)
	}
	before := target.read(ctx, notes)
	sample()
	// each periodic sample's timer is armed from the start, before the load, and the next armed as it fires
	samplingStopped := false
	var stopSample func() bool
	var arm func(k int)
	arm = func(k int) {
		due := start.Add(time.Duration(k) * settings.SampleInterval)
		// never a timer due already, which a clock may fire on this goroutine while it holds the lock
		stopSample = clock.AfterFunc(max(due.Sub(clock.Now()), time.Nanosecond), func() {
			sample()
			sampleLock.Lock()
			defer sampleLock.Unlock()
			if !samplingStopped {
				arm(k + 1)
			}
		})
	}
	func() {
		sampleLock.Lock()
		defer sampleLock.Unlock()
		arm(1)
	}()
	stopSampling := func() {
		sampleLock.Lock()
		defer sampleLock.Unlock()
		samplingStopped = true
		stopSample()
	}
	var purges chan *PurgeLoadCounts
	if settings.Purges != nil {
		measurement.M10 = true
		purges = make(chan *PurgeLoadCounts, 1)
		go func() {
			purges <- runPurgeLoad(ctx, clock, target.Cluster, false, settings.Purges, settings.Duration, func(n int64) string {
				return fmt.Sprintf("%su%012d", prefix, n)
			}, nil, notes)
		}()
	}
	var overwrites atomic.Int64
	driver := load.NewDriver(&load.DriverSettings{
		Name:            "trash",
		Rate:            settings.OverwritesPerSecond,
		Steady:          settings.Duration,
		MaxInFlight:     settings.MaxInFlight,
		MaxFailureKinds: settings.MaxFailureKinds,
		Clock:           clock,
	}, func(ctx context.Context, arrival int64) error {
		n := arrival % settings.ObjectCount
		etag, ok := objects.get(n)
		if !ok {
			return errors.New("no object to overwrite")
		}
		if err := trashOverwrite(ctx, target.Cluster, objects, opsKey(prefix, settings.ObjectByteCount, n), n, etag, opsBody(settings.ObjectByteCount, n, int(arrival/settings.ObjectCount)+1)); err != nil {
			return err
		}
		overwrites.Add(1)
		return nil
	})
	report := driver.Run(ctx)
	if purges != nil {
		measurement.Purges = <-purges
	}
	stopSampling()
	after := target.read(ctx, notes)
	sample()
	for kind, count := range report.FailureKinds {
		notes.add(fmt.Sprintf("%d overwrites failed: %s", count, kind))
	}
	measurement.Seconds = readingSeconds(before, after)
	measurement.Overwrites = overwrites.Load()
	measurement.Failed = report.Failed
	measurement.LatencyMillis = report.LatencyMillis
	measurement.Drives = driveDeltas(before, after)
	measurement.RawUsageDelta, measurement.InodeDelta = usageDeltas(before, after)
	if settings.Duration < settings.EvidenceDuration || anyMissing(measurement.Drives) || len(measurement.Samples) < 2 {
		measurement.Smoke = true
		notes.add("a window under 1 h, or one that read no drive counters or no .trash: a smoke")
	}
	return ctx.Err()
}

// One overwrite of object n at key from etag, keeping its new ETag. A failed one may have landed, an unknown outcome,
// so the object's ETag is read again for its next overwrite, which would otherwise fail on the old one for the rest
// of the window.
func trashOverwrite(ctx context.Context, cluster sidecar.BlobCluster, objects *opsObjects, key string, n int64, etag string, body []byte) error {
	newEtag, err := cluster.PutIfMatch(ctx, key, etag, body, nil)
	if err != nil {
		if info, headErr := cluster.Head(ctx, key); headErr == nil {
			objects.set(n, info.Etag)
		}
		return err
	}
	objects.set(n, newEtag)
	return nil
}

// The .trash of every drive through the host stats command: its answer's trash fields, by the drives admin info
// lists.
type commandTrash struct {
	hostStats *commandHostStats
}

func (self *commandTrash) ReadTrash(ctx context.Context, clusterId string) (*TrashSample, error) {
	stats, err := self.hostStats.ReadHostStats(ctx, clusterId)
	if err != nil {
		return nil, err
	}
	sample := &TrashSample{}
	for _, drive := range stats {
		if drive.TrashByteCount == nil || drive.TrashInodeCount == nil {
			continue
		}
		sample.Drives += 1
		sample.ByteCount += *drive.TrashByteCount
		sample.InodeCount += *drive.TrashInodeCount
		sample.MaxDriveByteCount = max(sample.MaxDriveByteCount, *drive.TrashByteCount)
		sample.MaxDriveInodeCount = max(sample.MaxDriveInodeCount, *drive.TrashInodeCount)
	}
	if sample.Drives == 0 {
		return nil, errors.New("no drive answered its .trash")
	}
	return sample, nil
}

func runTrashMode(ctx context.Context, args []string, stdout io.Writer) (bool, error) {
	defaults := DefaultTrashSettings()
	settings := *defaults
	flags := flag.NewFlagSet("filecluster-bench trash", flag.ContinueOnError)
	common := &runFlags{}
	common.register(flags, true)
	sweep := flags.Duration("sweep", 0, "the trash sweep interval the cluster runs with, MINIO_API_DELETE_CLEANUP_INTERVAL: 5m or 30s")
	flags.Float64Var(&settings.OverwritesPerSecond, "rate", defaults.OverwritesPerSecond, "overwrites a second")
	flags.DurationVar(&settings.Duration, "duration", defaults.Duration, "the window")
	flags.DurationVar(&settings.SampleInterval, "sample", defaults.SampleInterval, "how often .trash is read")
	flags.Int64Var(&settings.ObjectCount, "objects", defaults.ObjectCount, "the objects overwritten in turn")
	m10 := flags.Bool("purges", false, "M10's run: slot purges at year two's rate beside the overwrites")
	purgeRate := flags.Float64("purge_rate", DefaultSlotPurgeLoadSettings().Rates[0].PerSecond, "the purges' slots a second")
	if err := flags.Parse(args); err != nil {
		return false, err
	}
	if *m10 {
		settings.Purges = DefaultSlotPurgeLoadSettings()
		settings.Purges.Rates[0].PerSecond = *purgeRate
	}
	if *sweep <= 0 {
		return false, refuseStart("-sweep must declare the interval the cluster runs with")
	}
	clusters, err := openClusters(ctx, common.clustersPath)
	if err != nil {
		return false, err
	}
	defer clusters.Close()
	if common.clusterId == "" {
		common.clusterId = clusters.MetadataClusterIds()[0]
	}
	if kind := clusters.ClusterKind(common.clusterId); kind != server.FileClusterKindMetadata {
		return false, refuseStart("%s is a %q cluster: the trash row runs on a metadata cluster", common.clusterId, kind)
	}
	measurement := &TrashMeasurement{SweepInterval: *sweep}
	if err := common.rowHeader(ctx, &measurement.RowHeader, ModeTrash, clusters, common.clusterId, map[string]string{
		"sweep":      sweep.String(),
		"rate":       fmt.Sprint(settings.OverwritesPerSecond),
		"duration":   settings.Duration.String(),
		"objects":    fmt.Sprint(settings.ObjectCount),
		"purges":     fmt.Sprint(*m10),
		"purge_rate": fmt.Sprint(*purgeRate),
	}); err != nil {
		return false, err
	}
	target, err := common.target(ctx, clusters, common.clusterId)
	if err != nil {
		return false, err
	}
	var trash TrashReader
	if hostStats, ok := target.HostStats.(*commandHostStats); ok {
		trash = &commandTrash{hostStats: hostStats}
	}
	runErr := RunTrash(ctx, target, trash, &settings, runPrefix(), measurement)
	if err := writeRowFile(common.outPath, stdout, measurement); err != nil {
		return false, err
	}
	if runErr != nil {
		return false, runErr
	}
	return true, nil
}

// Judges hdd, hosts and trash files, writing the verdict and exiting by it: the trash row's two runs go in together.
func runVerdictMode(ctx context.Context, args []string, stdout io.Writer) (bool, error) {
	flags := flag.NewFlagSet("filecluster-bench verdict", flag.ContinueOnError)
	inPaths := stringsFlag{}
	flags.Var(&inPaths, "in", "an hdd or hosts file, or the trash row's two files, repeatable")
	if err := flags.Parse(args); err != nil {
		return false, err
	}
	if len(inPaths) == 0 {
		return false, refuseStart("no -in file")
	}
	trashRuns := []*TrashMeasurement{}
	pass := true
	for _, path := range inPaths {
		data, err := os.ReadFile(path)
		if err != nil {
			return false, refuseStart("%v", err)
		}
		header := &struct {
			Mode string `json:"mode"`
		}{}
		if err := json.Unmarshal(data, header); err != nil {
			return false, refuseStart("%s: %v", path, err)
		}
		switch header.Mode {
		case ModeHdd:
			measurement := &HddMeasurement{}
			if err := parseRowFile(data, measurement, true); err != nil {
				return false, refuseStart("%s: %v", path, err)
			}
			settings := DefaultHddSettings()
			if measurement.M10 {
				verdict := JudgeHddM10(measurement, settings)
				pass = pass && verdict.Pass
				if err := WriteHddM10Verdict(stdout, measurement, verdict); err != nil {
					return false, err
				}
				continue
			}
			verdict := JudgeHdd(measurement, settings)
			pass = pass && verdict.Pass
			if err := WriteHddVerdict(stdout, measurement, verdict, settings); err != nil {
				return false, err
			}
		case ModeHosts:
			measurement := &HostsMeasurement{}
			if err := parseRowFile(data, measurement, true); err != nil {
				return false, refuseStart("%s: %v", path, err)
			}
			verdict := JudgeHosts(measurement, DefaultHostsSettings())
			pass = pass && verdict.Pass
			if err := WriteHostsVerdict(stdout, measurement, verdict); err != nil {
				return false, err
			}
		case ModeTrash:
			measurement := &TrashMeasurement{}
			if err := parseRowFile(data, measurement, true); err != nil {
				return false, refuseStart("%s: %v", path, err)
			}
			trashRuns = append(trashRuns, measurement)
		default:
			return false, refuseStart("%s: mode %q is no hdd, hosts or trash file", path, header.Mode)
		}
	}
	if 0 < len(trashRuns) {
		verdict := JudgeTrash(trashRuns, DefaultTrashSettings())
		pass = pass && verdict.Pass
		if err := WriteTrashVerdict(stdout, verdict); err != nil {
			return false, err
		}
	}
	return pass, nil
}
