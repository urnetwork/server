package sidecarrestore

// §19.10's postgres row at the frame (OPS-M3-03): the ops step, which verifies what xops's restore script did and then
// takes every orphaned log down the deleted-network path.
//
//	ops      the script restored the main dump, then the sidecar dump, under this incident: the database carries its
//	         marker and the list both snapshot times, which head it. The step never runs the script itself, which needs
//	         the dumps, a superuser and the restore host (xops/postgres-backup/restore.md)
//	orphans  a proj/ log on the first cluster whose network the restored rows lack belongs to a network newer than
//	         both dumps, lost with its rows. Each repo its verified log names is marked deleted{deletion_id = the
//	         incident id}, so §11.12's shred takes it after 7 d, and the job sidecar_restore_orphan_log deletes the
//	         log's proj/ and projrows/ objects once the last of those repos is shredded. All of it is listed in the
//	         incident: the log, each repo and what became of it, and the deletion owed
//
// Marking a repo needs the signer repo/'s deleted state names (sidecar.CheckEvsRepoTransition refuses a deleted
// state without one), and no statement was signed for an orphan's repos: what goes there is
// questions/ops-m3-03-orphan-deleted-signer.md. Until it is answered OrphanLogSettings.DeletedSigner refuses, so
// nothing is marked and nothing deleted: each repo is listed as held, the log is listed as held, and no job is
// queued. Every write rereads the object it changes and is If-Match on that read, so a rerun under the incident, or
// two runs racing, mark each repo once.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/sidecarhost"
)

// The list's rows the orphan path writes.
const (
	// a log whose network the restored rows lack: table proj, row the network, detail its repo count
	ListKindOrphanLog = "orphan-log"
	// a log the step could not read: nothing of it was touched
	ListKindOrphanLogUnreadable = "orphan-log-unreadable"
	// one of an orphan log's repos: table repo, row the repo, detail what became of it
	ListKindOrphanRepo = "orphan-repo"
	// the log's proj/ and projrows/ objects, deleted with the last of its repos' shreds by the job
	ListKindOrphanLogDeleteOwed = "orphan-log-delete-owed"
	// a log none of whose deletions may start yet, with why
	ListKindOrphanLogHeld = "orphan-log-held"
)

// What became of one orphan's repo, its orphan-repo row's detail.
const (
	orphanRepoMarked      = "marked deleted"
	orphanRepoPast        = "already shredding or shredded"
	orphanRepoNotPlaced   = "never placed"
	orphanRepoHeldPrefix  = "held: "
	orphanRepoOtherPrefix = "left: "
)

// The job that deletes an orphan log once its repos are shredded, in taskworker's sidecar job registry.
const OrphanLogJobName = "sidecar_restore_orphan_log"

// No signer for an orphan's deleted state is settled yet (questions/ops-m3-03-orphan-deleted-signer.md).
var ErrOrphanDeletedSignerUnsettled = errors.New("no signer for an orphaned repo's deleted state is settled: question ops-m3-03-orphan-deleted-signer")

// The tunables of the orphan path.
type OrphanLogSettings struct {
	// The most keys one LIST of proj/ returns while the step finds the logs the first cluster holds. Each network
	// costs one LIST whatever its length, since the walk skips past a network's entries once it has seen one.
	ListPageCount int
	// The signer an orphan's repo's deleted state names. The default refuses, so nothing is marked until the question
	// is answered; a test supplies one to drive the rest of the path.
	DeletedSigner func(networkId server.Id, repoId sidecar.Id) (sidecar.Id, error)
	// How many times a repo/ write is tried again after its If-Match lost or its answer was lost: 8, as evs's own
	// transitions retry.
	RepoTransitionRetryCount int
	// When the job first looks for the repos' shreds after a repo is marked: §11.12's start, 7 d 10 min after the
	// deletion, plus a day for the shred's own phases.
	FirstCheckAfter time.Duration
	// How long the job waits before it looks again while a repo's shred has not ended: 1 h.
	RecheckInterval time.Duration
	// The most repos one job carries in its args. A log naming more is listed as held for the operator, rather than
	// carried in pending_task's row.
	JobRepoMaxCount int
}

// The defaults.
func DefaultOrphanLogSettings() *OrphanLogSettings {
	return &OrphanLogSettings{
		ListPageCount: 1000,
		DeletedSigner: func(networkId server.Id, repoId sidecar.Id) (sidecar.Id, error) {
			return sidecar.Id{}, ErrOrphanDeletedSignerUnsettled
		},
		RepoTransitionRetryCount: 8,
		FirstCheckAfter:          8*24*time.Hour + 10*time.Minute,
		RecheckInterval:          time.Hour,
		JobRepoMaxCount:          4096,
	}
}

// What the orphan path runs on.
type OrphanLogDeps struct {
	// every cluster: the first holds proj/, projrows/ and place/, each repo's own cluster its repo/
	Blobs sidecar.BlobStore
	// a log sequencer over the first cluster, which reads each orphan's log from the store
	Sequencer *model.ConfigLogSequencer
	// where the job is queued: the env's task queue
	Tasks sidecar.Tasks
	// the clock marks and jobs are timed by
	Now func() time.Time
	// nil for the defaults
	Settings *OrphanLogSettings
}

// The settings deps carry, or the defaults.
func (self *OrphanLogDeps) settings() *OrphanLogSettings {
	if self.Settings == nil {
		return DefaultOrphanLogSettings()
	}
	return self.Settings
}

// The networks whose logs the first cluster holds, in key order: one LIST each, skipping past every network's entries
// once it has seen one.
func listLogNetworks(ctx context.Context, firstCluster sidecar.BlobCluster, pageCount int) ([]server.Id, error) {
	networkIds := []server.Id{}
	startAfter := ""
	for {
		infos, _, err := firstCluster.List(ctx, "proj/", startAfter, pageCount)
		if err != nil {
			return nil, err
		}
		if len(infos) == 0 {
			return networkIds, nil
		}
		key := infos[0].Key
		rest := strings.TrimPrefix(key, "proj/")
		slash := strings.IndexByte(rest, '/')
		if slash <= 0 {
			// no entry key: step past it
			startAfter = key
			continue
		}
		networkId, err := server.ParseId(rest[:slash])
		if err == nil && strings.HasPrefix(key, sidecar.ConfigEntryKeyPrefix(sidecar.Id(networkId))) {
			networkIds = append(networkIds, networkId)
		}
		// '~' sorts after every digit and '_', so the next LIST begins at the next network
		startAfter = "proj/" + rest[:slash] + "/~"
	}
}

// The ids of networkIds the restored network rows hold.
func restoredNetworkIds(ctx context.Context, networkIds []server.Id) (restored map[server.Id]bool, returnErr error) {
	restored = map[server.Id]bool{}
	if len(networkIds) == 0 {
		return restored, nil
	}
	if r := server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `SELECT network_id FROM network WHERE network_id = ANY($1)`, networkIds)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var networkId server.Id
					server.Raise(result.Scan(&networkId))
					restored[networkId] = true
				}
			})
		})
	}); r != nil {
		return nil, fmt.Errorf("the restored network rows: %v", r)
	}
	return restored, nil
}

// The job's args: the incident, the orphan's network and its repos, ids alone (host_tasks.go).
type orphanLogJobArgs struct {
	IncidentId server.Id   `json:"incident_id"`
	NetworkId  server.Id   `json:"network_id"`
	RepoIds    []server.Id `json:"repo_ids"`
}

// The orphan path for the incident (see the file header).
func RunOrphanLogs(ctx context.Context, deps *OrphanLogDeps, run *StepRun) error {
	settings := deps.settings()
	incident := run.Incident
	firstCluster, err := deps.Blobs.FirstCluster()
	if err != nil {
		return err
	}
	networkIds, err := listLogNetworks(ctx, firstCluster, settings.ListPageCount)
	if err != nil {
		return fmt.Errorf("the logs on the first cluster: %w", err)
	}
	restored, err := restoredNetworkIds(ctx, networkIds)
	if err != nil {
		return err
	}
	orphanCount := 0
	for _, networkId := range networkIds {
		if restored[networkId] {
			continue
		}
		orphanCount += 1
		if err := runOrphanLog(ctx, deps, settings, firstCluster, incident, networkId); err != nil {
			return fmt.Errorf("orphaned log of %s: %w", networkId, err)
		}
	}
	fmt.Fprintf(run.Out, "%d logs on the first cluster, %d of them orphaned\n", len(networkIds), orphanCount)
	return nil
}

// One orphan's log: list it, mark its repos, and queue the deletion owed once they are shredded.
func runOrphanLog(ctx context.Context, deps *OrphanLogDeps, settings *OrphanLogSettings, firstCluster sidecar.BlobCluster, incident *Incident, networkId server.Id) error {
	repoIds, found, err := deps.Sequencer.RestoreRepoIds(ctx, networkId)
	if err != nil || !found {
		reason := "the log does not verify from its genesis"
		if err != nil {
			reason = err.Error()
		}
		// a log the step cannot read is left whole, for the operator
		return incident.Append(&ListRow{Kind: ListKindOrphanLogUnreadable, Table: "proj", RowId: networkId.String(), Detail: reason})
	}
	if err := incident.Append(&ListRow{Kind: ListKindOrphanLog, Table: "proj", RowId: networkId.String(), Detail: fmt.Sprintf("%d repos", len(repoIds))}); err != nil {
		return err
	}
	held := ""
	jobRepoIds := []server.Id{}
	for _, repoId := range repoIds {
		outcome, err := markOrphanRepo(ctx, deps, settings, firstCluster, incident, networkId, repoId)
		if err != nil {
			return err
		}
		if err := incident.Append(&ListRow{Kind: ListKindOrphanRepo, Table: "repo", RowId: repoId.String(), Detail: outcome}); err != nil {
			return err
		}
		switch {
		case outcome == orphanRepoMarked, outcome == orphanRepoPast:
			jobRepoIds = append(jobRepoIds, server.Id(repoId))
		case outcome == orphanRepoNotPlaced:
		default:
			held = outcome
		}
	}
	if held == "" && settings.JobRepoMaxCount < len(jobRepoIds) {
		held = fmt.Sprintf("%s%d repos, past the job's %d", orphanRepoHeldPrefix, len(jobRepoIds), settings.JobRepoMaxCount)
	}
	if held != "" {
		// a repo that is not on its way to a shred keeps the log, which its readers need
		return incident.Append(&ListRow{Kind: ListKindOrphanLogHeld, Table: "proj", RowId: networkId.String(), Detail: held})
	}
	args, err := json.Marshal(&orphanLogJobArgs{IncidentId: incident.IncidentId(), NetworkId: networkId, RepoIds: jobRepoIds})
	if err != nil {
		return err
	}
	if err := deps.Tasks.Schedule(ctx, &sidecar.TaskRequest{
		Name:    OrphanLogJobName,
		OnceKey: incident.IncidentId().String() + "/" + networkId.String(),
		Args:    args,
		RunAt:   deps.Now().Add(settings.FirstCheckAfter),
	}); err != nil {
		return err
	}
	return incident.Append(&ListRow{
		Kind:   ListKindOrphanLogDeleteOwed,
		Table:  "proj",
		RowId:  networkId.String(),
		Detail: "proj/ and projrows/ deleted with the last shred of its repos",
	})
}

// Marks one orphan's repo deleted{deletion_id = the incident id}, If-Match on what it read, and answers what became of
// it, the orphan-repo row's detail.
func markOrphanRepo(ctx context.Context, deps *OrphanLogDeps, settings *OrphanLogSettings, firstCluster sidecar.BlobCluster, incident *Incident, networkId server.Id, repoId sidecar.Id) (string, error) {
	place, err := sidecar.ReadEvsRepoPlace(ctx, firstCluster, repoId)
	if errors.Is(err, sidecar.ErrEvsRepoNotPlaced) {
		return orphanRepoNotPlaced, nil
	}
	if err != nil {
		return "", err
	}
	cluster, err := deps.Blobs.Cluster(place.ClusterId)
	if err != nil {
		return "", err
	}
	deletionId := sidecar.Id(incident.IncidentId())
	for attempt := 0; ; attempt += 1 {
		object, etag, err := sidecar.ReadEvsRepoObject(ctx, cluster, repoId)
		if errors.Is(err, sidecar.ErrEvsRepoNotFound) {
			return orphanRepoNotPlaced, nil
		}
		if err != nil {
			return "", err
		}
		named, _ := sidecar.IdFromBytes(object.GetDeletionId())
		switch object.GetState() {
		case protocol.EvsRepoState_EVS_REPO_DELETED:
			if named == deletionId {
				return orphanRepoMarked, nil
			}
			// deleted by its own RepoDelete before the loss: its shred is on its way already
			return orphanRepoPast, nil
		case protocol.EvsRepoState_EVS_REPO_SHREDDING, protocol.EvsRepoState_EVS_REPO_SHREDDED:
			return orphanRepoPast, nil
		case protocol.EvsRepoState_EVS_REPO_ACTIVE:
		default:
			return orphanRepoOtherPrefix + object.GetState().String(), nil
		}
		if len(object.GetNetworkId()) == 16 && sidecar.Id(object.GetNetworkId()) != sidecar.Id(networkId) {
			return orphanRepoOtherPrefix + "repo/ names another network", nil
		}
		signer, err := settings.DeletedSigner(networkId, repoId)
		if err != nil {
			return orphanRepoHeldPrefix + err.Error(), nil
		}
		next := proto.Clone(object).(*protocol.EvsRepoObject)
		next.State = protocol.EvsRepoState_EVS_REPO_DELETED
		next.DeletionId = deletionId.Bytes()
		next.StateSigner = signer.Bytes()
		next.StateMs = deps.Now().UnixMilli()
		if err := sidecar.CheckEvsRepoTransition(object, next); err != nil {
			return "", err
		}
		nextBytes, err := sidecar.MarshalEvsRepoObject(next)
		if err != nil {
			return "", err
		}
		_, err = cluster.PutIfMatch(ctx, sidecar.EvsRepoObjectKey(repoId), etag, nextBytes, nil)
		var unknown *sidecar.BlobUnknownError
		switch {
		case err == nil:
			return orphanRepoMarked, nil
		case errors.Is(err, sidecar.ErrBlobPrecondition), errors.As(err, &unknown):
			// another write came first, or the answer was lost: the next read decides again
			if settings.RepoTransitionRetryCount <= attempt {
				return "", fmt.Errorf("repo %s: repo/ kept moving under %d tries", repoId, attempt+1)
			}
		default:
			return "", err
		}
	}
}

// One run of the job (see the file header): while a repo it carries is not shredded it looks again later; once every
// one is, it deletes the log's projrows/ and then its proj/ objects, and ends. A network whose rows came back since
// keeps its log. It is idempotent: a run after a crash finds what is left and deletes it.
func RunOrphanLogJob(ctx context.Context, deps *OrphanLogDeps, args []byte) error {
	settings := deps.settings()
	jobArgs := &orphanLogJobArgs{}
	if err := json.Unmarshal(args, jobArgs); err != nil {
		return fmt.Errorf("the job's args: %w", err)
	}
	restored, err := restoredNetworkIds(ctx, []server.Id{jobArgs.NetworkId})
	if err != nil {
		return err
	}
	if restored[jobArgs.NetworkId] {
		// the network's rows came back: its log is its own again
		return nil
	}
	firstCluster, err := deps.Blobs.FirstCluster()
	if err != nil {
		return err
	}
	for _, repoId := range jobArgs.RepoIds {
		shredded, err := orphanRepoShredded(ctx, deps, firstCluster, sidecar.Id(repoId))
		if err != nil {
			return err
		}
		if !shredded {
			return deps.Tasks.Schedule(ctx, &sidecar.TaskRequest{
				Name:    OrphanLogJobName,
				OnceKey: jobArgs.IncidentId.String() + "/" + jobArgs.NetworkId.String(),
				Args:    args,
				RunAt:   deps.Now().Add(settings.RecheckInterval),
			})
		}
	}
	for _, prefix := range []string{
		fmt.Sprintf("projrows/%s/", sidecar.Id(jobArgs.NetworkId)),
		sidecar.ConfigEntryKeyPrefix(sidecar.Id(jobArgs.NetworkId)),
	} {
		if err := deletePrefix(ctx, firstCluster, prefix, settings.ListPageCount); err != nil {
			return err
		}
	}
	return nil
}

// Whether the repo's shred has ended, or it was never placed.
func orphanRepoShredded(ctx context.Context, deps *OrphanLogDeps, firstCluster sidecar.BlobCluster, repoId sidecar.Id) (bool, error) {
	place, err := sidecar.ReadEvsRepoPlace(ctx, firstCluster, repoId)
	if errors.Is(err, sidecar.ErrEvsRepoNotPlaced) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	cluster, err := deps.Blobs.Cluster(place.ClusterId)
	if err != nil {
		return false, err
	}
	object, _, err := sidecar.ReadEvsRepoObject(ctx, cluster, repoId)
	if errors.Is(err, sidecar.ErrEvsRepoNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return object.GetState() == protocol.EvsRepoState_EVS_REPO_SHREDDED, nil
}

// Deletes every object under prefix, a page at a time.
func deletePrefix(ctx context.Context, cluster sidecar.BlobCluster, prefix string, pageCount int) error {
	for {
		infos, _, err := cluster.List(ctx, prefix, "", pageCount)
		if err != nil {
			return err
		}
		if len(infos) == 0 {
			return nil
		}
		for _, info := range infos {
			if err := cluster.Delete(ctx, info.Key); err != nil && !errors.Is(err, sidecar.ErrBlobNotFound) {
				return err
			}
		}
	}
}

// The ops step (see the file header): the script's work verified, then the orphan path.
func RunOpsStep(ctx context.Context, deps *OrphanLogDeps, run *StepRun) error {
	if err := CheckRestoreMarker(ctx, run.Incident.IncidentId()); err != nil {
		return err
	}
	sidecarTime, mainTime, err := run.Incident.SnapshotTimes()
	if err != nil {
		return err
	}
	fmt.Fprintf(run.Out, "restored under incident %s: the sidecar dump of %s and the main dump of %s\n",
		run.Incident.IncidentId(), sidecarTime.Format(time.RFC3339Nano), mainTime.Format(time.RFC3339Nano))
	return RunOrphanLogs(ctx, deps, run)
}

// The ops step as the process's frame runs it: on the env WARP_ENV names, every cluster through taskworker's
// file-clusters-taskworker.yml, which alone reaches repo/ on every metadata cluster and proj/ to delete, and the
// env's task queue for the job.
func init() {
	RegisterStep(StepOps, func(ctx context.Context, run *StepRun) error {
		clusters, err := server.LoadFileClusters(ctx, orphanLogFileClustersService, nil)
		if err != nil {
			return fmt.Errorf("the file clusters: %w", err)
		}
		defer clusters.Close()
		firstCluster, err := clusters.FirstCluster()
		if err != nil {
			return err
		}
		return RunOpsStep(ctx, &OrphanLogDeps{
			Blobs:     clusters,
			Sequencer: model.NewConfigLogSequencer(firstCluster, nil, nil),
			Tasks:     sidecarhost.NewTasks(nil),
			Now:       server.NowUtc,
		}, run)
	})
}

// The block whose cluster credentials the ops step and the job use: taskworker's.
const orphanLogFileClustersService = "taskworker"
