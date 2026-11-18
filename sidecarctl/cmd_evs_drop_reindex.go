// `sidecarctl evs drop-reindex <repo_id> <job_id> [<drop_id>:<ref_id>:<version>:<push_id>...]` writes a drop job's
// list files again after drop_index_corrupt pages (§19.8's page row; DESIGN-DETAILS 10.5's corrupt list file row and
// 10.2.9's listed index; owner decision 512, QD-965). It is (*evsservice.DropResolve).Reindex on the env WARP_ENV
// names: the job's list files computed anew from its drops' bodies and their refs' history, as the job's resolve
// computed them, then each of the job's list files that fails its trailer or layout, or holds other bytes, deleted by
// a DELETE naming the LastModified it read and written again with resolve's bytes. A drop that no longer resolves is
// an error and nothing is written, and nothing is deleted while the segment cluster has not passed the settle cases.
//
// With no drops named, Reindex takes them from the first of the job's list files that reads whole. With none reading
// whole, the command takes them from the job's snap-0 file (dropscan/<repo>/<job>/snap-0000000000 on the segment
// cluster, read through evsservice.DropFiles.Read), which names the drops the job's resolve wrote the list files for
// and which the job keeps for the repo's life, as it keeps the list files (owner decision 570, QD-1021); it prints
// each drop it took, as <drop_id>:<ref_id>:<version>:<push_id>.
//
// Drops may still be named, each as <drop_id>:<ref_id>:<version>:<push_id>, and with a snap-0 the named set must be
// the set it covers: Reindex trusts the drops it is handed, and a set that leaves one of the job's drops out resolves
// and would be written without that drop's rows, so that once the drop's table entry has left, §11.9's serve would
// answer a version it removed. A set that differs is refused, writing nothing, with the ids that differ. So is every
// run that needs a snap-0 which is there and does not read whole, and one whose snap-0 covers no drop. A job with no
// snap-0, one whose cleanup ran before snap-0 was kept, runs as owner decision 512 gave it: with no list file reading
// whole and no drops named it refuses, and named drops are taken as named, which the run prints as unchecked.
//
// A drop named twice is a usage mistake, since its rows would be listed twice and the files written would not be the
// ones resolve wrote; so is an id that does not parse, the zero id, and a version that is not a positive decimal.
// Each drop must resolve on its ref's history, or Reindex refuses them all.
//
// It reaches the clusters as taskworker does, through vault's file-clusters-taskworker.yml (server.LoadFileClusters,
// as taskworker's evs host and the restore frame's ops step load it): that block's MinIO user reaches every cluster
// and reads, writes and deletes on the segment cluster, where dropscan/ lives, and the job's resolve wrote the list
// files as it. The host Reindex runs on holds those Blobs alone, the one field Reindex reads of a host. The repo's
// metadata cluster is the one its place/ object names, through sidecar.NewEvsRepoPlaceResolver as evs.sweep resolves
// it, and the segment cluster is the store's one. The list files' layout and the read pace are the drop job's
// DropSettings, the defaults taskworker's drop job runs under, so the bytes computed are the ones its resolve wrote and
// the scan reads no faster than a job does.
//
// It prints the job's and repo's ids, where the drops came from when a line named them or snap-0 gave them, the
// sequences rewritten, written where none stood and found stray (past what the drops give, left as they are), and the
// bytes the scan read: ids and counts only, never a path or a name (I-13).
// Any error exits 1 with Reindex's words, saying first what the run had already rewritten or written. Running it again
// is safe: a file holding resolve's bytes is left alone, so a second run rewrites nothing, and a run stopped between a
// DELETE and its write leaves that sequence missing, which the next run writes fresh, from snap-0's drops when no
// other list file of the job reads whole.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
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
)

// The block whose vault file names the clusters a run reaches: taskworker's, whose MinIO user reaches every cluster
// and deletes on the segment cluster (xops's policy-taskworker.json.j2), as which the drop job's resolve wrote the
// files.
const evsDropReindexFileClustersService = "taskworker"

// The command's registration. It is the only change to the binary this command needs.
func init() {
	registerSidecarctlCommand(&sidecarctlCommand{
		words:   "evs drop-reindex",
		usage:   "<repo_id> <job_id> [<drop_id>:<ref_id>:<version>:<push_id>...]",
		summary: "Write a drop job's list files again after drop_index_corrupt pages, in the env WARP_ENV names: each that fails its trailer or layout, or holds other bytes, is deleted and written again with the bytes the job's resolve wrote. With no drops named they come from the first of the job's list files that reads whole, else from the job's snap-0 file. Drops named are refused unless they are the ones snap-0 covers. Safe to run again.",
		run:     runEvsDropReindex,
	})
}

// What the command calls: (*evsservice.DropResolve).Reindex.
type evsDropReindexer interface {
	Reindex(ctx context.Context, request *evsservice.DropReindexRequest) (*evsservice.DropReindexResult, error)
}

// How one run reaches its env. A caller changes its own copy; tests replace the clusters, the clock and the reindex.
type evsDropReindexSettings struct {
	// Bounds the whole run: the clusters' open and first check, the place/ read, the scan of every ref the job's
	// drops are on and the writes. Non-positive expires at once, so nothing is opened, read or written.
	timeout time.Duration
	// The drop job's settings: the list files' layout (FileRowCount) and the read pace (ReadBytesPerSecond) are its,
	// so the files computed are the ones its resolve wrote and the scan reads no faster than a job.
	dropSettings *evsservice.DropSettings
	// What the pace waits on and place/'s resolver reads.
	clock sidecar.Clock
	// Opens the file clusters of the env this process runs as, and returns what ends their checks.
	openBlobs func(ctx context.Context) (sidecar.BlobStore, func(), error)
	// The reindex over blobs under dropSettings.
	newReindexer func(blobs sidecar.BlobStore, dropSettings *evsservice.DropSettings) (evsDropReindexer, error)
}

// The settings of a run. A caller changes its own copy.
func defaultEvsDropReindexSettings() *evsDropReindexSettings {
	return &evsDropReindexSettings{
		// the scan reads every version of each ref the job's drops are on at the job's 32 MB/s, so two hours read
		// 230 GB of history: room for a large ref, and a bound on a run that stopped moving, which is run again
		timeout:      2 * time.Hour,
		dropSettings: evsservice.DefaultDropSettings(),
		clock:        sidecar.RealClock{},
		openBlobs:    openServerEvsDropReindexBlobs,
		newReindexer: newServerEvsDropReindexer,
	}
}

// The env's file clusters as taskworker reaches them: vault's file-clusters-taskworker.yml through server's loader,
// in the env WARP_ENV names, refused with no WARP_ENV before anything is read. One check of every cluster runs before
// this returns, so a write never races the checks' first read of admin info (one pool, the settled build), and the
// checks' own loop keeps it fresh through a long scan.
func openServerEvsDropReindexBlobs(ctx context.Context) (sidecar.BlobStore, func(), error) {
	if _, err := sidecarctlProcessEnv(); err != nil {
		return nil, nil, err
	}
	clusters, err := server.LoadFileClusters(ctx, evsDropReindexFileClustersService, nil)
	if err != nil {
		return nil, nil, err
	}
	clusters.Check(ctx)
	return clusters, clusters.Close, nil
}

// (*evsservice.DropResolve) on a host holding blobs alone, which is all Reindex reads of its host.
func newServerEvsDropReindexer(blobs sidecar.BlobStore, dropSettings *evsservice.DropSettings) (evsDropReindexer, error) {
	resolve, err := evsservice.NewDropResolve(&sidecar.ServiceHost{Blobs: blobs}, &evsservice.DropResolveDeps{Settings: dropSettings})
	if err != nil {
		return nil, err
	}
	return resolve, nil
}

// What a command line names.
type evsDropReindexJob struct {
	repoId sidecar.Id
	jobId  sidecar.Id
	// as named, in order; nil when the line names none, so Reindex takes them from the job's list files, or the run
	// from its snap-0
	drops []*evsservice.DropListDrop
}

// The drops the job's snap-0 covers, as a list file names a drop, in snap-0's order, and whether the job has a snap-0:
// absent is no error. A snap-0 that fails its trailer or layout, is not this job's, or names a drop twice, which no
// job writes and whose rows a reindex would list twice, is evsservice.ErrDropFileInvalid.
func readEvsDropReindexCovered(ctx context.Context, files *evsservice.DropFiles) ([]*evsservice.DropListDrop, bool, error) {
	file, err := files.Read(ctx, evsservice.DropFileSnap, 0)
	if err != nil {
		return nil, false, err
	}
	if file == nil {
		return nil, false, nil
	}
	coveredDrops := make([]*evsservice.DropListDrop, 0, len(file.Covered))
	coveredDropIds := map[sidecar.Id]bool{}
	for _, covered := range file.Covered {
		if coveredDropIds[covered.DropId] {
			return nil, true, fmt.Errorf("%w: snap 0 names drop %s twice", evsservice.ErrDropFileInvalid, sidecar.EvsKeyId(covered.DropId))
		}
		coveredDropIds[covered.DropId] = true
		coveredDrops = append(coveredDrops, &evsservice.DropListDrop{DropId: covered.DropId, RefId: covered.RefId, Version: covered.Version, PushId: covered.PushId})
	}
	return coveredDrops, true, nil
}

// How the drops a line names differ from the ones the job's snap-0 covers, as sets, the order named never mattering:
// "" when they are the same set, else the drop ids snap-0 covers that the line leaves out, the ones the line names
// that snap-0 does not cover, and the ones named with another ref, version or push than snap-0 gives them, each list
// ascending. Ids only (I-13).
func evsDropReindexDropsDifference(namedDrops []*evsservice.DropListDrop, coveredDrops []*evsservice.DropListDrop) string {
	coveredDropIdDrops := map[sidecar.Id]*evsservice.DropListDrop{}
	for _, covered := range coveredDrops {
		coveredDropIdDrops[covered.DropId] = covered
	}
	namedDropIds := map[sidecar.Id]bool{}
	leftOut := []string{}
	notCovered := []string{}
	otherwise := []string{}
	for _, named := range namedDrops {
		namedDropIds[named.DropId] = true
		covered, ok := coveredDropIdDrops[named.DropId]
		switch {
		case !ok:
			notCovered = append(notCovered, sidecar.EvsKeyId(named.DropId))
		case *covered != *named:
			otherwise = append(otherwise, sidecar.EvsKeyId(named.DropId))
		}
	}
	for _, covered := range coveredDrops {
		if !namedDropIds[covered.DropId] {
			leftOut = append(leftOut, sidecar.EvsKeyId(covered.DropId))
		}
	}
	parts := []string{}
	for _, part := range []struct {
		what    string
		dropIds []string
	}{
		{what: "it covers and the line leaves out", dropIds: leftOut},
		{what: "the line names and it does not cover", dropIds: notCovered},
		{what: "the line names with another ref, version or push", dropIds: otherwise},
	} {
		if len(part.dropIds) != 0 {
			slices.Sort(part.dropIds)
			parts = append(parts, part.what+" "+strings.Join(part.dropIds, " "))
		}
	}
	return strings.Join(parts, "; ")
}

// A drop as a command line names it and a run prints it.
func evsDropReindexDropText(drop *evsservice.DropListDrop) string {
	return fmt.Sprintf("%s:%s:%d:%s", sidecar.EvsKeyId(drop.DropId), sidecar.EvsKeyId(drop.RefId), drop.Version, sidecar.EvsKeyId(drop.PushId))
}

// `sidecarctl evs drop-reindex`, against the env as server reaches it, ending early on SIGINT or SIGTERM.
func runEvsDropReindex(args []string, out io.Writer) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runEvsDropReindexWithSettings(ctx, args, defaultEvsDropReindexSettings(), out)
}

// Parses the command line and reindexes the job it names.
func runEvsDropReindexWithSettings(ctx context.Context, args []string, settings *evsDropReindexSettings, out io.Writer) error {
	flags := flag.NewFlagSet("evs drop-reindex", flag.ContinueOnError)
	positionals, err := parseSidecarctlArgs(flags, args)
	if err != nil {
		return err
	}
	job, err := parseEvsDropReindexJob(positionals)
	if err != nil {
		return err
	}
	return reindexEvsDropJob(ctx, job, settings, out)
}

// The repo, the job and each drop a command line names. An id is 32 hex characters, as keys and pages write it, or a
// uuid's 36, and never the zero id; a version is a positive decimal. Anything else, or a drop named twice, is a usage
// mistake, refused before anything is opened.
func parseEvsDropReindexJob(positionals []string) (*evsDropReindexJob, error) {
	if len(positionals) < 2 {
		return nil, sidecarctlUsageErrorf("want <repo_id> <job_id>, then any drops, got %q", positionals)
	}
	parseId := func(what string, text string) (sidecar.Id, error) {
		id, err := sidecar.ParseId(text)
		if err != nil || id == (sidecar.Id{}) {
			return sidecar.Id{}, sidecarctlUsageErrorf("%s %q is not an id: 32 hex characters, or a uuid's 36, and not zero", what, text)
		}
		return id, nil
	}
	repoId, err := parseId("repo_id", positionals[0])
	if err != nil {
		return nil, err
	}
	jobId, err := parseId("job_id", positionals[1])
	if err != nil {
		return nil, err
	}
	job := &evsDropReindexJob{repoId: repoId, jobId: jobId}
	namedDropIds := map[sidecar.Id]bool{}
	for _, tuple := range positionals[2:] {
		fields := strings.Split(tuple, ":")
		if len(fields) != 4 {
			return nil, sidecarctlUsageErrorf("drop %q is not <drop_id>:<ref_id>:<version>:<push_id>", tuple)
		}
		dropId, err := parseId(fmt.Sprintf("drop %q's drop_id", tuple), fields[0])
		if err != nil {
			return nil, err
		}
		refId, err := parseId(fmt.Sprintf("drop %q's ref_id", tuple), fields[1])
		if err != nil {
			return nil, err
		}
		version, err := strconv.ParseUint(fields[2], 10, 64)
		if err != nil || version == 0 {
			return nil, sidecarctlUsageErrorf("drop %q's version %q is not a positive decimal", tuple, fields[2])
		}
		pushId, err := parseId(fmt.Sprintf("drop %q's push_id", tuple), fields[3])
		if err != nil {
			return nil, err
		}
		if namedDropIds[dropId] {
			return nil, sidecarctlUsageErrorf("drop %s is named twice; name each of the job's drops once", sidecar.EvsKeyId(dropId))
		}
		namedDropIds[dropId] = true
		job.drops = append(job.drops, &evsservice.DropListDrop{DropId: dropId, RefId: refId, Version: version, PushId: pushId})
	}
	return job, nil
}

// Reindexes the job on its repo's clusters and prints what it did to out. Every refusal and failure returns an error,
// and only a reindex that finished prints to out.
func reindexEvsDropJob(ctx context.Context, job *evsDropReindexJob, settings *evsDropReindexSettings, out io.Writer) error {
	if err := settings.dropSettings.Validate(); err != nil {
		return fmt.Errorf("nothing was read: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, settings.timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("nothing was read: the run's %v bound passed: %w", settings.timeout, err)
	}
	jobLabel := fmt.Sprintf("job %s of repo %s", sidecar.EvsKeyId(job.jobId), sidecar.EvsKeyId(job.repoId))
	sequences := func(values []uint32) string {
		if len(values) == 0 {
			return "none"
		}
		texts := make([]string, 0, len(values))
		for _, value := range values {
			texts = append(texts, strconv.FormatUint(uint64(value), 10))
		}
		return strings.Join(texts, " ")
	}
	// waits as long as byteCount bytes take at the job's read pace (§11.13's bound row), as the drop job's own pace
	pace := func(ctx context.Context, byteCount int64) error {
		if byteCount <= 0 {
			return nil
		}
		wait := time.Duration(float64(byteCount) / float64(settings.dropSettings.ReadBytesPerSecond) * float64(time.Second))
		if wait <= 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-settings.clock.After(wait):
			return nil
		}
	}

	var result *evsservice.DropReindexResult
	// where the drops came from when the line named them or snap-0 gave them, and the ones snap-0 gave
	dropsLine := ""
	var takenDrops []*evsservice.DropListDrop
	err := sidecarctlServerCall(func() error {
		blobs, closeBlobs, err := settings.openBlobs(ctx)
		if err == nil && blobs == nil {
			err = errors.New("they opened as no store")
		}
		if err != nil {
			return fmt.Errorf("the env's file clusters did not open, so nothing was read: %w", err)
		}
		if closeBlobs != nil {
			defer closeBlobs()
		}
		place, err := sidecar.NewEvsRepoPlaceResolver(blobs, settings.clock, nil).Resolve(ctx, job.repoId)
		if errors.Is(err, sidecar.ErrEvsRepoNotPlaced) {
			return fmt.Errorf("repo %s is not placed: no place/ object names it, so it has no list file to reindex", sidecar.EvsKeyId(job.repoId))
		}
		if err != nil {
			return fmt.Errorf("repo %s's place/ does not read, so nothing else was read: %w", sidecar.EvsKeyId(job.repoId), err)
		}
		cluster, err := blobs.Cluster(place.ClusterId)
		if err != nil {
			return fmt.Errorf("repo %s's metadata cluster is not one this env's file-clusters-%s.yml reaches: %w", sidecar.EvsKeyId(job.repoId), evsDropReindexFileClustersService, err)
		}
		segmentCluster, err := blobs.SegmentCluster()
		if err != nil {
			return fmt.Errorf("this env's file-clusters-%s.yml reaches no segment cluster: %w", evsDropReindexFileClustersService, err)
		}
		reindexer, err := settings.newReindexer(blobs, settings.dropSettings)
		if err != nil {
			return err
		}
		files := evsservice.NewDropFiles(segmentCluster, job.repoId, job.jobId, settings.dropSettings)
		reindex := func(drops []*evsservice.DropListDrop) error {
			reindexed, err := reindexer.Reindex(ctx, &evsservice.DropReindexRequest{
				Cluster:        cluster,
				SegmentCluster: segmentCluster,
				RepoId:         job.repoId,
				JobId:          job.jobId,
				Drops:          drops,
				Pace:           pace,
			})
			result = reindexed
			if err == nil && result == nil {
				err = errors.New("the reindex answered no result")
			}
			return err
		}
		if job.drops != nil {
			// named drops are written only as the set snap-0 covers, checked before Reindex reads or writes anything
			coveredDrops, covers, err := readEvsDropReindexCovered(ctx, files)
			if err != nil {
				return fmt.Errorf("its snap-0 does not read, so the drops named cannot be held to the ones it covers and nothing was written; if it stays so, report it: %w", err)
			}
			if !covers {
				dropsLine = fmt.Sprintf("drops %d named, unchecked: the job has no snap-0", len(job.drops))
				return reindex(job.drops)
			}
			if difference := evsDropReindexDropsDifference(job.drops, coveredDrops); difference != "" {
				return fmt.Errorf("the drops named are not the %d its snap-0 covers, so nothing was written: %s; name none and a run takes the drops it needs itself", len(coveredDrops), difference)
			}
			dropsLine = fmt.Sprintf("drops %d named, the ones the job's snap-0 covers", len(job.drops))
			return reindex(job.drops)
		}
		err = reindex(nil)
		if !errors.Is(err, evsservice.ErrDropReindexNoDrops) {
			return err
		}
		// no list file reads whole, which Reindex found before it wrote anything: the drops are snap-0's
		coveredDrops, covers, snapErr := readEvsDropReindexCovered(ctx, files)
		switch {
		case snapErr != nil:
			return fmt.Errorf("no list file of it reads whole and its snap-0 does not read, so nothing was written; if it stays so, report it: %w", snapErr)
		case !covers:
			// a job cleaned before snap-0 was kept: Reindex's refusal stands, as owner decision 512 has it
			return err
		case len(coveredDrops) == 0:
			return errors.New("no list file of it reads whole and its snap-0 covers no drop, so it has no list file to write and nothing was written: report it")
		}
		dropsLine = fmt.Sprintf("drops %d taken from the job's snap-0, no list file of it reading whole", len(coveredDrops))
		takenDrops = coveredDrops
		return reindex(coveredDrops)
	})
	if err != nil {
		// what changed before it stopped, so the operator reads the store's state from the error alone
		done := []string{}
		if result != nil && len(result.Rewritten) != 0 {
			done = append(done, "rewriting list files "+sequences(result.Rewritten))
		}
		if result != nil && len(result.Written) != 0 {
			done = append(done, "writing list files "+sequences(result.Written))
		}
		if len(done) != 0 {
			return fmt.Errorf("%s stopped after %s: %w", jobLabel, strings.Join(done, " and "), err)
		}
		return fmt.Errorf("%s stopped: %w", jobLabel, err)
	}
	fmt.Fprintf(out, "%s: its list files hold the bytes its drops resolve to\n", jobLabel)
	if dropsLine != "" {
		fmt.Fprintf(out, "%s\n", dropsLine)
	}
	for _, drop := range takenDrops {
		fmt.Fprintf(out, "drop %s\n", evsDropReindexDropText(drop))
	}
	fmt.Fprintf(out, "rewritten %s\n", sequences(result.Rewritten))
	fmt.Fprintf(out, "written %s\n", sequences(result.Written))
	fmt.Fprintf(out, "stray %s\n", sequences(result.Stray))
	fmt.Fprintf(out, "scanned_bytes %d\n", result.ScanByteCount)
	if len(result.Stray) != 0 {
		fmt.Fprintf(out, "a stray file is past the sequences the job's drops give and was left as it is: report it, as for a refused job\n")
	}
	return nil
}
