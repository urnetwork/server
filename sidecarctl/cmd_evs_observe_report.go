// `sidecarctl evs observe-report <env>` prints how far format 2's two observe soaks have come: D-TEST-12's, which lets
// an operator set `format2.purge: on`, and D-TEST-18's, which lets one set `format2.drop: on` (owner decision 513,
// QD-987, questions/evs2-m10-33-observe-report-command.md; §19.11's rollout, EVS2-M10-33). It only reads: it changes
// no gate, schedules nothing and writes no object, so turning a mode on stays an operator's act in sidecar.yml, and
// it exits 0 whether or not a soak is met. A run that cannot read what a soak rests on exits 1 and prints no report,
// since a report over part of the evidence would read as the whole.
//
// What it reads, for work.EvsObserveSoakAt (server/taskworker/work/evs_purge_observe_work.go), whose Lines() it
// prints:
//
//	purge   urnetwork_evs_versionless_observe_total{result="clean"|"retained"}, which evs's observe check raises
//	        for each candidate it holds to every retained version (evsservice.PurgeObserveCheck), from the monitor's
//	        query API (monitor.ReadMimirQuerySamples, the authenticated read §19.8's rows use) as daily samples
//	        over --days days, the last at the run's time. Each sample is, per process series, the counter's largest
//	        reading in the day ending there (max_over_time), so a process that started and ended between two samples
//	        is still read, and a series that fell reads as a restart. A sample with a result the report does not
//	        know, or a reading that is no whole count, fails the run
//	drop    each repo with a drop table, found as taskworker's daily start finds them, by a LIST of drops/ on every
//	        metadata cluster; a repo is read on the cluster its place/ names, and a table on another cluster is
//	        counted and left out, as is one of a repo place/ does not name. Per repo, its table and, from a LIST of
//	        dropscan/<repo>/ on the segment cluster, each job's snap-0 file, which a job keeps for the repo's life
//	        (owner decision 570) and a dry run never writes: the drops the job covered and the two booleans it
//	        recorded of its listed versions (renames, cross-ref copies). A job is observed when a drop it covers
//	        stands observed in the table, and refused {verify} when one stands so there or in the table's void
//	        records, which outlive the entry; it ended when its newest object was written, which once its other
//	        files are deleted, 30 d after, is when its resolve wrote the list files. A snap-0 that does not read
//	        fails the run: a job left out could be the one the oracle refused
//
// It reaches the clusters as taskworker does (vault's file-clusters-taskworker.yml through server.LoadFileClusters,
// as `evs drop-reindex` does) and Mimir with the monitor's settings (monitor.LoadSignalSettings), both in the env
// WARP_ENV names. The process reaches one env, so the command refuses an <env> that is not it before reading
// anything, as `evs purge-turn-on` does.
//
// A report prints counts, days and thresholds only: no network, repo, ref or path, and no label of a series (I-13).
// A read that fails names the repo and job it stopped on by their opaque ids, as `evs drop-reindex` takes them.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/monitor"
	"github.com/urnetwork/server/taskworker/work"
)

// The counter the purge soak reads, its label and the label's two values (evsservice/purge_observe.go).
const (
	evsObserveReportMetric         = "urnetwork_evs_versionless_observe_total"
	evsObserveReportResultLabel    = "result"
	evsObserveReportResultClean    = "clean"
	evsObserveReportResultRetained = "retained"
)

// The block whose vault file names the clusters a run reaches: taskworker's, whose MinIO user reads every cluster,
// the segment cluster's dropscan/ among them.
const evsObserveReportFileClustersService = "taskworker"

// The prefix of the drop tables on a metadata cluster and what follows a repo's id in its table's key
// (evsservice.DropsKey).
const (
	evsObserveReportDropsPrefix = "drops/"
	evsObserveReportDropsSuffix = "/_"
)

// The prefix of every repo's job and dry-run files on the segment cluster (evsservice.DropFileKey).
const evsObserveReportDropscanPrefix = "dropscan/"

// The command's registration. It is the only change to the binary this command needs.
func init() {
	registerSidecarctlCommand(&sidecarctlCommand{
		words:   "evs observe-report",
		usage:   "<env> [--days=<n>]",
		summary: "Print how far format 2's purge and drop observe soaks (D-TEST-12, D-TEST-18) have come in the env WARP_ENV names, from the monitor's observe counter over the last n days and the repos' drop tables and jobs' snap-0 files. It only reads and exits 0 whether or not a soak is met; setting purge: on or drop: on stays an operator's act.",
		run:     runEvsObserveReport,
	})
}

// One instant query of the env's Mimir, evaluated at a time, its answer's body bounded by maxByteCount.
type evsObserveReportQuery func(ctx context.Context, query string, at time.Time, maxByteCount int) ([]*monitor.MimirQuerySample, error)

// One env as a run reaches it.
type evsObserveReportEnv struct {
	// the monitor's query API
	query evsObserveReportQuery
	// the env's file clusters
	blobs sidecar.BlobStore
	// ends what the open started; may be nil
	close func()
}

// How one run reads its env. A caller changes its own copy; tests replace the clock and the env.
type evsObserveReportSettings struct {
	// Bounds the whole run: every query and every table and snap-0 read. Non-positive expires at once, so nothing is
	// read.
	timeout time.Duration
	// How many days back the counter's samples go when --days names none: 60, past twice D-TEST-18's 30 days, so a
	// soak that is met reads as met and one that began before the window reads its length as the window's. Positive.
	sampleDays int
	// The most --days may ask: each day is one query. Positive.
	sampleMaxDays int
	// The time between two samples, and the range each one's max_over_time covers: a day. Positive.
	sampleInterval time.Duration
	// Bounds one query's answer: two results a process series, 256 B a series, so a megabyte holds 2,000 processes.
	queryMaxByteCount int
	// The objects one LIST of drops/ or of a repo's dropscan/ asks for. Positive.
	listLimit int
	// The soaks' thresholds, D-TEST-12's and D-TEST-18's.
	soakSettings *work.EvsObserveSoakSettings
	// The drop job's settings, whose table and file bounds the reads hold what they read to.
	dropSettings *evsservice.DropSettings
	// The time a run is judged at and its last sample read.
	now func() time.Time
	// What place/'s resolver reads.
	clock sidecar.Clock
	// Opens env as its taskworker and its monitor see it, refusing an env it cannot reach as itself.
	openEnv func(ctx context.Context, env string) (*evsObserveReportEnv, error)
}

// The settings of a run. A caller changes its own copy.
func defaultEvsObserveReportSettings() *evsObserveReportSettings {
	dropSettings := evsservice.DefaultDropSettings()
	return &evsObserveReportSettings{
		// 61 queries at the monitor's 15 s bound each, and a table and a few snap-0 files a repo with drops
		timeout:           30 * time.Minute,
		sampleDays:        60,
		sampleMaxDays:     400,
		sampleInterval:    24 * time.Hour,
		queryMaxByteCount: 1024 * 1024,
		listLimit:         dropSettings.ListLimit,
		soakSettings:      work.DefaultEvsObserveSoakSettings(),
		dropSettings:      dropSettings,
		now:               time.Now,
		clock:             sidecar.RealClock{},
		openEnv:           openServerEvsObserveReportEnv,
	}
}

// The env this process runs as, which must be the one named: its Mimir through the monitor's settings and its file
// clusters through vault's file-clusters-taskworker.yml, one check of every cluster run before this returns. Any
// other env, or no WARP_ENV, is refused before anything is read.
func openServerEvsObserveReportEnv(ctx context.Context, env string) (*evsObserveReportEnv, error) {
	processEnv, err := sidecarctlProcessEnv()
	if err != nil {
		return nil, err
	}
	if processEnv != env {
		return nil, fmt.Errorf("this process runs as WARP_ENV=%s and was asked for %s; run it with WARP_ENV=%s", processEnv, env, env)
	}
	signalSettings, err := monitor.LoadSignalSettings()
	if err != nil {
		return nil, fmt.Errorf("the monitor's settings do not load, so the observe counter cannot be read: %w", err)
	}
	if signalSettings.Environment != env {
		return nil, fmt.Errorf("the monitor's settings name env %s, not %s", signalSettings.Environment, env)
	}
	clusters, err := server.LoadFileClusters(ctx, evsObserveReportFileClustersService, nil)
	if err != nil {
		return nil, fmt.Errorf("the env's file clusters did not open: %w", err)
	}
	clusters.Check(ctx)
	return &evsObserveReportEnv{
		query: func(ctx context.Context, query string, at time.Time, maxByteCount int) ([]*monitor.MimirQuerySample, error) {
			return monitor.ReadMimirQuerySamples(ctx, signalSettings, query, at, maxByteCount)
		},
		blobs: clusters,
		close: clusters.Close,
	}, nil
}

// `sidecarctl evs observe-report <env>`, against the env as server reaches it, ending early on SIGINT or SIGTERM.
func runEvsObserveReport(args []string, out io.Writer) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runEvsObserveReportWithSettings(ctx, args, defaultEvsObserveReportSettings(), out)
}

// Parses the command line and prints the report of the env it names.
func runEvsObserveReportWithSettings(ctx context.Context, args []string, settings *evsObserveReportSettings, out io.Writer) error {
	flags := flag.NewFlagSet("evs observe-report", flag.ContinueOnError)
	days := flags.Int("days", settings.sampleDays, "how many days back the observe counter's daily samples go")
	positionals, err := parseSidecarctlArgs(flags, args)
	if err != nil {
		return err
	}
	if len(positionals) != 1 {
		return sidecarctlUsageErrorf("want <env>, got %q", positionals)
	}
	env := positionals[0]
	if !evsPurgeTurnOnEnvRegex.MatchString(env) {
		return sidecarctlUsageErrorf("%q is not an env's name, one directory under config/", env)
	}
	if *days < 1 || settings.sampleMaxDays < *days {
		return sidecarctlUsageErrorf("--days=%d is not from 1 to %d", *days, settings.sampleMaxDays)
	}
	return reportEvsObserve(ctx, env, *days, settings, out)
}

// The query one sample asks: each series of the counter in env, at its largest reading over interval. The env's
// name is held to an env's characters before it reaches here, so quoting it adds nothing a selector could break on.
func evsObserveReportSampleQuery(env string, interval time.Duration) string {
	return "max_over_time(" + evsObserveReportMetric + "{env=" + strconv.Quote(env) + "}[" + strconv.FormatInt(int64(interval/time.Second), 10) + "s])"
}

// The samples of one query's answer at `at`: one per process series, its clean and retained readings side by side. A
// series is its labels but the result, sorted, so the two results of one process meet and two processes never do.
func evsObserveReportSamples(at time.Time, answer []*monitor.MimirQuerySample) ([]*work.EvsObserveSample, error) {
	seriesSamples := map[string]*work.EvsObserveSample{}
	seriesSeen := map[string]bool{}
	for _, one := range answer {
		if one == nil {
			return nil, errors.New("the answer holds an empty sample")
		}
		names := make([]string, 0, len(one.Labels))
		for name := range one.Labels {
			if name != evsObserveReportResultLabel && name != "__name__" {
				names = append(names, name)
			}
		}
		slices.Sort(names)
		parts := make([]string, 0, len(names))
		for _, name := range names {
			parts = append(parts, strconv.Quote(name)+"="+strconv.Quote(one.Labels[name]))
		}
		series := strings.Join(parts, ",")
		result := one.Labels[evsObserveReportResultLabel]
		if result != evsObserveReportResultClean && result != evsObserveReportResultRetained {
			return nil, fmt.Errorf("a series of %s carries result %q, which is neither %s nor %s", evsObserveReportMetric, result, evsObserveReportResultClean, evsObserveReportResultRetained)
		}
		// a counter's reading is a whole count that an int64 holds exactly
		if math.IsNaN(one.Value) || one.Value < 0 || 1<<53 < one.Value || one.Value != math.Trunc(one.Value) {
			return nil, fmt.Errorf("a series of %s reads %v, which is no count", evsObserveReportMetric, one.Value)
		}
		if seriesSeen[series+"\x00"+result] {
			return nil, fmt.Errorf("the answer holds one series of %s twice", evsObserveReportMetric)
		}
		seriesSeen[series+"\x00"+result] = true
		sample := seriesSamples[series]
		if sample == nil {
			sample = &work.EvsObserveSample{At: at, Series: series}
			seriesSamples[series] = sample
		}
		if result == evsObserveReportResultClean {
			sample.Clean = int64(one.Value)
		} else {
			sample.Retained = int64(one.Value)
		}
	}
	keys := make([]string, 0, len(seriesSamples))
	for series := range seriesSamples {
		keys = append(keys, series)
	}
	slices.Sort(keys)
	samples := make([]*work.EvsObserveSample, 0, len(keys))
	for _, series := range keys {
		samples = append(samples, seriesSamples[series])
	}
	return samples, nil
}

// What a run read of the env's drop jobs.
type evsObserveReportDrops struct {
	jobs []*work.EvsObserveDropJob
	// repos whose table was read, on the cluster their place/ names
	repoCount int
	// tables left out: on a cluster their repo's place/ does not name, or of a repo place/ does not name
	strayTableCount int
}

// One repo's drop jobs, in job id order: its table on cluster, the metadata cluster its place/ names, and each job's
// snap-0 on the segment cluster. See the file header's drop row.
func readEvsObserveReportRepoJobs(ctx context.Context, cluster sidecar.BlobCluster, segmentCluster sidecar.BlobCluster, repoId sidecar.Id, settings *evsObserveReportSettings) ([]*work.EvsObserveDropJob, error) {
	object, err := cluster.Get(ctx, evsservice.DropsKey(repoId))
	if errors.Is(err, sidecar.ErrBlobNotFound) {
		// its repo was shredded between the LIST and here
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("its drop table: %w", err)
	}
	encoded, err := io.ReadAll(io.LimitReader(object.Body, int64(settings.dropSettings.TableByteCount)+1))
	object.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("its drop table: %w", err)
	}
	if settings.dropSettings.TableByteCount < len(encoded) {
		return nil, fmt.Errorf("its drop table: %w: past %d bytes", evsservice.ErrDropTableInvalid, settings.dropSettings.TableByteCount)
	}
	table, err := evsservice.DecodeDropTable(encoded)
	if err != nil {
		return nil, fmt.Errorf("its drop table: %w", err)
	}
	// each job's newest object, for the jobs and dry runs with any, and whether a snap-0 is among them
	prefix := evsObserveReportDropscanPrefix + sidecar.EvsKeyId(repoId) + "/"
	jobNewests := map[sidecar.Id]time.Time{}
	jobCovers := map[sidecar.Id]bool{}
	startAfter := ""
	for {
		objects, more, err := segmentCluster.List(ctx, prefix, startAfter, settings.listLimit)
		if err != nil {
			return nil, fmt.Errorf("its jobs' files did not list: %w", err)
		}
		for _, object := range objects {
			jobText, _, ok := strings.Cut(object.Key[len(prefix):], "/")
			if !ok {
				continue
			}
			jobId, err := sidecar.ParseId(jobText)
			if err != nil || sidecar.EvsKeyId(jobId) != jobText {
				continue
			}
			if jobNewests[jobId].Before(object.LastModified) {
				jobNewests[jobId] = object.LastModified
			}
			if object.Key == evsservice.DropFileKey(repoId, jobId, evsservice.DropFileSnap, 0) {
				jobCovers[jobId] = true
			}
		}
		if !more || len(objects) == 0 {
			break
		}
		startAfter = objects[len(objects)-1].Key
	}
	jobIds := make([]sidecar.Id, 0, len(jobCovers))
	for jobId := range jobCovers {
		jobIds = append(jobIds, jobId)
	}
	slices.SortFunc(jobIds, func(a sidecar.Id, b sidecar.Id) int { return strings.Compare(sidecar.EvsKeyId(a), sidecar.EvsKeyId(b)) })
	jobs := make([]*work.EvsObserveDropJob, 0, len(jobIds))
	for _, jobId := range jobIds {
		file, err := evsservice.NewDropFiles(segmentCluster, repoId, jobId, settings.dropSettings).Read(ctx, evsservice.DropFileSnap, 0)
		if err != nil {
			return nil, fmt.Errorf("job %s's snap-0: %w", sidecar.EvsKeyId(jobId), err)
		}
		if file == nil {
			// gone with its repo since the LIST
			continue
		}
		job := &work.EvsObserveDropJob{EndedAt: jobNewests[jobId], Renames: file.Shape.Renames, CrossRefCopies: file.Shape.CrossRefCopies}
		for _, covered := range file.Covered {
			entry := table.Drop(covered.DropId)
			void := table.Void(covered.DropId)
			switch {
			case entry != nil && entry.State == evsservice.DropStateRefusedVerify, void != nil && void.State == evsservice.DropStateRefusedVerify:
				job.RefusedVerify = true
			case entry != nil && entry.State == evsservice.DropStateObserved:
				job.Observed = true
			}
		}
		jobs = append(jobs, job)
	}
	return jobs, nil
}

// The drop jobs of every repo with a drop table: see the file header's drop row. An error from any read is the
// run's, since a soak counted over some of the repos would say none of the rest refused.
func readEvsObserveReportDrops(ctx context.Context, blobs sidecar.BlobStore, settings *evsObserveReportSettings) (*evsObserveReportDrops, error) {
	segmentCluster, err := blobs.SegmentCluster()
	if err != nil {
		return nil, fmt.Errorf("this env's file-clusters-%s.yml reaches no segment cluster: %w", evsObserveReportFileClustersService, err)
	}
	places := sidecar.NewEvsRepoPlaceResolver(blobs, settings.clock, nil)
	drops := &evsObserveReportDrops{}
	for _, clusterId := range blobs.MetadataClusterIds() {
		cluster, err := blobs.Cluster(clusterId)
		if err != nil {
			return nil, fmt.Errorf("a metadata cluster this env's file-clusters-%s.yml names does not open: %w", evsObserveReportFileClustersService, err)
		}
		startAfter := ""
		for {
			objects, more, err := cluster.List(ctx, evsObserveReportDropsPrefix, startAfter, settings.listLimit)
			if err != nil {
				return nil, fmt.Errorf("the drop tables of a metadata cluster did not list: %w", err)
			}
			for _, object := range objects {
				repoText, ok := strings.CutSuffix(strings.TrimPrefix(object.Key, evsObserveReportDropsPrefix), evsObserveReportDropsSuffix)
				if !ok {
					continue
				}
				repoId, err := sidecar.ParseId(repoText)
				if err != nil || evsservice.DropsKey(repoId) != object.Key {
					continue
				}
				place, err := places.Resolve(ctx, repoId)
				if errors.Is(err, sidecar.ErrEvsRepoNotPlaced) {
					drops.strayTableCount += 1
					continue
				}
				if err != nil {
					return nil, fmt.Errorf("repo %s's place/ does not read: %w", sidecar.EvsKeyId(repoId), err)
				}
				if place.ClusterId != clusterId {
					drops.strayTableCount += 1
					continue
				}
				jobs, err := readEvsObserveReportRepoJobs(ctx, cluster, segmentCluster, repoId, settings)
				if err != nil {
					return nil, fmt.Errorf("repo %s's drop jobs do not read: %w", sidecar.EvsKeyId(repoId), err)
				}
				drops.repoCount += 1
				drops.jobs = append(drops.jobs, jobs...)
			}
			if !more || len(objects) == 0 {
				break
			}
			startAfter = objects[len(objects)-1].Key
		}
	}
	return drops, nil
}

// Reads env's observe counter over days days and its drop jobs, and prints both soaks to out. Every failure to read
// returns an error and prints nothing; a report prints whether or not its soaks are met, and returns nil.
func reportEvsObserve(ctx context.Context, env string, days int, settings *evsObserveReportSettings, out io.Writer) error {
	if err := settings.dropSettings.Validate(); err != nil {
		return fmt.Errorf("nothing was read: %w", err)
	}
	if settings.sampleInterval <= 0 || settings.listLimit <= 0 || settings.queryMaxByteCount <= 0 {
		return errors.New("nothing was read: the run's sample interval, list limit and query bound must be positive")
	}
	ctx, cancel := context.WithTimeout(ctx, settings.timeout)
	defer cancel()
	// checked before each step, so a run past its bound starts nothing more, whatever the env does with ctx
	expired := func() error {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("no report of %s: the run's %v bound passed: %w", env, settings.timeout, err)
		}
		return nil
	}
	if err := expired(); err != nil {
		return err
	}
	now := settings.now()
	var samples []*work.EvsObserveSample
	var drops *evsObserveReportDrops
	sampleTimeCount := 0
	err := sidecarctlServerCall(func() error {
		reach, err := settings.openEnv(ctx, env)
		if err == nil && (reach == nil || reach.query == nil || reach.blobs == nil) {
			err = errors.New("it opened as no env")
		}
		if err != nil {
			return fmt.Errorf("%s did not open, so nothing was read: %w", env, err)
		}
		if reach.close != nil {
			defer reach.close()
		}
		query := evsObserveReportSampleQuery(env, settings.sampleInterval)
		for back := days; 0 <= back; back -= 1 {
			if err := expired(); err != nil {
				return err
			}
			at := now.Add(-time.Duration(back) * settings.sampleInterval)
			answer, err := reach.query(ctx, query, at, settings.queryMaxByteCount)
			if err != nil {
				return fmt.Errorf("the observe counter did not read at %s: %w", at.UTC().Format(time.RFC3339), err)
			}
			atSamples, err := evsObserveReportSamples(at, answer)
			if err != nil {
				return fmt.Errorf("the observe counter's answer at %s does not read: %w", at.UTC().Format(time.RFC3339), err)
			}
			if 0 < len(atSamples) {
				sampleTimeCount += 1
			}
			samples = append(samples, atSamples...)
		}
		if err := expired(); err != nil {
			return err
		}
		drops, err = readEvsObserveReportDrops(ctx, reach.blobs, settings)
		return err
	})
	if err != nil {
		return err
	}
	report := work.EvsObserveSoakAt(samples, drops.jobs, now, settings.soakSettings)
	fmt.Fprintf(out, "observe soaks of %s at %s\n", env, now.UTC().Format(time.RFC3339))
	fmt.Fprintf(out, "read %s at %d of %d daily samples over %d days, and %d drop jobs of %d repos with a drop table\n",
		evsObserveReportMetric, sampleTimeCount, days+1, days, len(drops.jobs), drops.repoCount)
	if 0 < drops.strayTableCount {
		fmt.Fprintf(out, "left out %d drop tables on a cluster their repo's place/ does not name\n", drops.strayTableCount)
	}
	for _, line := range report.Lines() {
		fmt.Fprintf(out, "%s\n", line)
	}
	fmt.Fprintf(out, "this only read: format2.purge: on and format2.drop: on stay an operator's act in %s's sidecar.yml\n", env)
	return nil
}
