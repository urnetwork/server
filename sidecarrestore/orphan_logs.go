package sidecarrestore

// §19.10's postgres row at the frame (OPS-M3-03), with the deleted-network path the owner settled in QD-813
// (questions/ops-m3-03-orphan-deleted-signer.md): the ops step, which verifies what xops's restore script did and then
// lists every orphaned log, and the job sidecar_restore_orphan_log, which shreds the log's repos a week on and then
// deletes the log.
//
//	ops      the script restored the main dump, then the sidecar dump, under this incident: the database carries its
//	         marker and the list both snapshot times, which head it. The step never runs the script itself, which needs
//	         the dumps, a superuser and the restore host (xops/postgres-backup/restore.md)
//	orphans  a proj/ log on the first cluster whose network the restored rows lack belongs to a network newer than
//	         both dumps, lost with its rows. The step changes nothing of it: repo/ stays active, and nothing reaches a
//	         repo meanwhile, since the lost network's tokens are refused. It lists the log, each repo its verified log
//	         names with when it goes shredding, and the deletion owed, and queues the job for 7 d 10 min after the
//	         incident
//	job      once due, and only while the network's rows are still absent (rows that came back end it, nothing
//	         changed), it moves each repo the log names that is active, or still deleted under a RepoDelete of its
//	         own, to shredding{deletion_id = the incident id} by If-Match, which needs no signer, as DELETE with now
//	         does, and queues §11.12's shred for each repo shredding under the incident. A repo shredding or
//	         shredded under another deletion is left to that deletion's shred. It looks again on an interval until
//	         every repo is shredded, and only then deletes the log's projrows/ and then its proj/ objects
//
// The incident's time is the step's first run under it: the list keeps when the orphans' repos go shredding
// (orphan-shred-at), so a rerun lists every row as the first did and queues the same job, which the queue coalesces.
// A repo still deleted when the job runs is past its own RepoDelete's start, which came before the incident; that
// start reads a projection a lost network may not have, so the job shreds it under the incident too. A repo the path
// cannot take, keying or naming another network, holds its log for the operator: the step queues no job for it, and
// a job that finds one moves nothing. Every repo/ write rereads the object it changes and is If-Match on that read, so
// a rerun of the job after a crash, or two runs racing, move each repo once. The job's writes are P0 on the process's
// write budget (§11.13): repo/'s of kind lifecycle, as DELETE's are, and the log's deletes of kind log.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
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
	// one of an orphan log's repos: table repo, row the repo, detail what becomes of it
	ListKindOrphanRepo = "orphan-repo"
	// when the job moves the incident's orphaned repos to shredding, written once: table proj, row the incident,
	// detail the time
	ListKindOrphanShredAt = "orphan-shred-at"
	// the log's proj/ and projrows/ objects, deleted by the job after the last of its repos' shreds
	ListKindOrphanLogDeleteOwed = "orphan-log-delete-owed"
	// a log the path leaves for the operator, with why: no job is queued, so nothing of it is shredded or deleted
	ListKindOrphanLogHeld = "orphan-log-held"
)

// What becomes of one orphan's repo, its orphan-repo row's detail.
const (
	// followed by the time the job moves it, or moved it, to shredding under the incident
	orphanRepoShreddingAtPrefix = "shredding at "
	// shredding or shredded under another deletion, whose shred the job waits for
	orphanRepoPast      = "already shredding or shredded"
	orphanRepoNotPlaced = "never placed"
	// an active or deleted repo of a held log: it stays as it is
	orphanRepoKept = "kept with its held log"
	// followed by why the path cannot take it, which holds its log
	orphanRepoOtherPrefix = "left: "
)

// The job that shreds an orphan log's repos and then deletes the log, in taskworker's sidecar job registry.
const OrphanLogJobName = "sidecar_restore_orphan_log"

// evs's once key for a repo's first shred pass, the one evsservice's evsShredTaskRequest builds: the job's name, the
// repo's and the deletion's key ids, then pass 0. A shred the job queues coalesces with the ones evs's DELETE and its
// reconciler queue, which TestOrphanLogJobMovesEachRepoToShreddingAndQueuesItsShred pins against the reconciler.
const orphanShredOnceKeyFormat = "%s.%s.%s.0"

var (
	// Job args that name no orphaned log or no time: no run of them can succeed, and none touched anything.
	ErrOrphanLogJobArgs = errors.New("sidecar_restore_orphan_log args invalid")
	// A repo the job found in a state the path cannot take: it moved nothing, and the log stays for the operator.
	ErrOrphanRepoLeft = errors.New("an orphaned log names a repo the deleted-network path cannot take")
)

// The tunables of the orphan path.
type OrphanLogSettings struct {
	// The most keys one LIST returns: of proj/, while the step finds the logs the first cluster holds, which costs
	// each network one LIST whatever its length, and of a log's objects while the job deletes them.
	ListPageCount int
	// How long after the incident the job moves the orphans' repos to shredding: 7 d 10 min, the start §11.12 gives a
	// RepoDelete (QD-813), so the network's rows may come back for a week.
	ShredAfter time.Duration
	// How many times a repo/ write is tried again after its If-Match lost or its answer was lost: 8, as evs's own
	// transitions retry.
	RepoTransitionRetryCount int
	// How long the job waits before it looks again while a repo's shred has not ended: 1 h.
	RecheckInterval time.Duration
	// The most repos one job carries in its args. A log naming more is held for the operator, rather than carried
	// in pending_task's row.
	JobRepoMaxCount int
	// The XFS block size the job prices its P0 writes at (§11.13's cost rows): evs's own setting, whose default
	// stays in evsservice.
	PriceBlockByteCount int64
}

// The defaults.
func DefaultOrphanLogSettings() *OrphanLogSettings {
	return &OrphanLogSettings{
		ListPageCount:            1000,
		ShredAfter:               7*24*time.Hour + 10*time.Minute,
		RepoTransitionRetryCount: 8,
		RecheckInterval:          time.Hour,
		JobRepoMaxCount:          4096,
		PriceBlockByteCount:      evsservice.DefaultEvsSettings().PriceBlockByteCount,
	}
}

// What the orphan path runs on.
type OrphanLogDeps struct {
	// every cluster: the first holds proj/, projrows/ and place/, each repo's own cluster its repo/
	Blobs sidecar.BlobStore
	// a log sequencer over the first cluster, which reads each orphan's log from the store: the step's alone
	Sequencer *model.ConfigLogSequencer
	// where the job and the shreds are queued: the env's task queue
	Tasks sidecar.Tasks
	// the process's write budget, to which the job issues its writes as P0 (§11.13); nil issues them unpriced, as
	// in the ops step's process, which holds none and writes nothing to the clusters
	Budget sidecar.WriteBudget
	// the clock the incident and the job are timed by
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

// The job's args: the incident, the orphan's network, its repos and when they go shredding, ids and a time alone
// (host_tasks.go).
type orphanLogJobArgs struct {
	IncidentId server.Id   `json:"incident_id"`
	NetworkId  server.Id   `json:"network_id"`
	RepoIds    []server.Id `json:"repo_ids"`
	ShredAt    time.Time   `json:"shred_at"`
}

// The job's queue entry for one orphan's log, once-keyed by incident and network.
func orphanLogJobRequest(incidentId server.Id, networkId server.Id, args []byte, runAt time.Time) *sidecar.TaskRequest {
	return &sidecar.TaskRequest{
		Name:    OrphanLogJobName,
		OnceKey: incidentId.String() + "/" + networkId.String(),
		Args:    args,
		RunAt:   runAt,
	}
}

// §11.12's first shred pass of a repo shredding under deletionId, due at runAt, as evs queues it (shred.go): its
// once key and args are evs's own, so a repeat runs once.
func orphanShredRequest(repoId sidecar.Id, deletionId sidecar.Id, runAt time.Time) *sidecar.TaskRequest {
	return &sidecar.TaskRequest{
		Name:    evsservice.EvsShredTaskName,
		OnceKey: fmt.Sprintf(orphanShredOnceKeyFormat, evsservice.EvsShredTaskName, sidecar.EvsKeyId(repoId), sidecar.EvsKeyId(deletionId)),
		Args:    evsservice.EvsShredArgs(repoId, deletionId, false),
		RunAt:   runAt,
	}
}

// One repo an orphan's log names, as its clusters hold it.
type orphanRepoRead struct {
	// its metadata cluster, nil when it was never placed
	cluster sidecar.BlobCluster
	// repo/ and its ETag, nil when the repo was never placed or its create never wrote repo/
	object *protocol.EvsRepoObject
	etag   string
	// why the path cannot take it, empty when it can
	left string
}

// Whether the job moves it to shredding under the incident: active, or deleted under a RepoDelete of its own.
func (self *orphanRepoRead) movable() bool {
	if self.object == nil || self.left != "" {
		return false
	}
	state := self.object.GetState()
	return state == protocol.EvsRepoState_EVS_REPO_ACTIVE || state == protocol.EvsRepoState_EVS_REPO_DELETED
}

// Whether it is shredding or shredded under deletionId: moved by the job.
func (self *orphanRepoRead) movedUnder(deletionId sidecar.Id) bool {
	if self.object == nil {
		return false
	}
	state := self.object.GetState()
	if state != protocol.EvsRepoState_EVS_REPO_SHREDDING && state != protocol.EvsRepoState_EVS_REPO_SHREDDED {
		return false
	}
	named, err := sidecar.IdFromBytes(self.object.GetDeletionId())
	return err == nil && named == deletionId
}

// Whether nothing of it is left to shred: never placed, or shredded under any deletion.
func (self *orphanRepoRead) shredded() bool {
	return self.object == nil || self.object.GetState() == protocol.EvsRepoState_EVS_REPO_SHREDDED
}

// One repo the orphan's log names, from place/ on the first cluster and repo/ on its own.
func readOrphanRepo(ctx context.Context, blobs sidecar.BlobStore, firstCluster sidecar.BlobCluster, networkId server.Id, repoId sidecar.Id) (*orphanRepoRead, error) {
	place, err := sidecar.ReadEvsRepoPlace(ctx, firstCluster, repoId)
	if errors.Is(err, sidecar.ErrEvsRepoNotPlaced) {
		return &orphanRepoRead{}, nil
	}
	if err != nil {
		return nil, err
	}
	cluster, err := blobs.Cluster(place.ClusterId)
	if err != nil {
		return nil, err
	}
	return rereadOrphanRepo(ctx, cluster, networkId, repoId)
}

// repo/ on the repo's cluster. The path cannot take a keying repo, which no transition takes to shredding, nor an
// active or deleted one whose repo/ names another network.
func rereadOrphanRepo(ctx context.Context, cluster sidecar.BlobCluster, networkId server.Id, repoId sidecar.Id) (*orphanRepoRead, error) {
	object, etag, err := sidecar.ReadEvsRepoObject(ctx, cluster, repoId)
	if errors.Is(err, sidecar.ErrEvsRepoNotFound) {
		return &orphanRepoRead{cluster: cluster}, nil
	}
	if err != nil {
		return nil, err
	}
	read := &orphanRepoRead{cluster: cluster, object: object, etag: etag}
	switch object.GetState() {
	case protocol.EvsRepoState_EVS_REPO_ACTIVE, protocol.EvsRepoState_EVS_REPO_DELETED:
		if len(object.GetNetworkId()) == 16 && sidecar.Id(object.GetNetworkId()) != sidecar.Id(networkId) {
			read.left = "repo/ names another network"
		}
	case protocol.EvsRepoState_EVS_REPO_SHREDDING, protocol.EvsRepoState_EVS_REPO_SHREDDED:
	default:
		read.left = object.GetState().String()
	}
	return read, nil
}

// What becomes of one repo, its orphan-repo row's detail. It reads the same on every run of the step under the
// incident, so a rerun after the job moved a repo lists nothing new.
func orphanRepoDetail(read *orphanRepoRead, incidentId sidecar.Id, held bool, shredAt time.Time) string {
	switch {
	case read.object == nil:
		return orphanRepoNotPlaced
	case read.left != "":
		return orphanRepoOtherPrefix + read.left
	case read.movable() && held:
		return orphanRepoKept
	case read.movable(), read.movedUnder(incidentId) && !held:
		return orphanRepoShreddingAtPrefix + shredAt.UTC().Format(time.RFC3339Nano)
	}
	return orphanRepoPast
}

// When the incident's orphaned repos go shredding: the time the list records, or, at the step's first need of it,
// now and ShredAfter, recorded before any row names it.
func orphanShredTime(incident *Incident, now time.Time, settings *OrphanLogSettings) (time.Time, error) {
	rows := incident.RowsOfKind(ListKindOrphanShredAt)
	if len(rows) == 0 {
		shredAt := now.Add(settings.ShredAfter).UTC()
		err := incident.Append(&ListRow{
			Kind:   ListKindOrphanShredAt,
			Table:  "proj",
			RowId:  incident.IncidentId().String(),
			Detail: shredAt.Format(time.RFC3339Nano),
		})
		if err != nil {
			return time.Time{}, err
		}
		return shredAt, nil
	}
	for _, row := range rows {
		if row.RowId != incident.IncidentId().String() || row.Detail != rows[0].Detail {
			return time.Time{}, fmt.Errorf("%w: the orphans' shred times are not one time of this incident", ErrIncidentList)
		}
	}
	shredAt, err := time.Parse(time.RFC3339Nano, rows[0].Detail)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: the orphans' shred time %q is no time", ErrIncidentList, rows[0].Detail)
	}
	return shredAt.UTC(), nil
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

// One orphan's log: list it and each of its repos, and queue the job that shreds them a week on and then deletes the
// log, unless a repo holds it. Nothing of the log or its repos changes here.
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
	reads := make([]*orphanRepoRead, 0, len(repoIds))
	held := ""
	jobRepoIds := []server.Id{}
	for _, repoId := range repoIds {
		read, err := readOrphanRepo(ctx, deps.Blobs, firstCluster, networkId, repoId)
		if err != nil {
			return err
		}
		reads = append(reads, read)
		if read.left != "" && held == "" {
			held = orphanRepoOtherPrefix + read.left
		}
		if read.object != nil {
			jobRepoIds = append(jobRepoIds, server.Id(repoId))
		}
	}
	if held == "" && settings.JobRepoMaxCount < len(jobRepoIds) {
		held = fmt.Sprintf("%d repos, past the job's %d", len(jobRepoIds), settings.JobRepoMaxCount)
	}
	shredAt := time.Time{}
	if held == "" {
		shredAt, err = orphanShredTime(incident, deps.Now(), settings)
		if err != nil {
			return err
		}
	}
	incidentId := sidecar.Id(incident.IncidentId())
	for index, repoId := range repoIds {
		detail := orphanRepoDetail(reads[index], incidentId, held != "", shredAt)
		if err := incident.Append(&ListRow{Kind: ListKindOrphanRepo, Table: "repo", RowId: repoId.String(), Detail: detail}); err != nil {
			return err
		}
	}
	if held != "" {
		// a repo the path cannot take keeps the whole log, and every repo of it, for the operator
		return incident.Append(&ListRow{Kind: ListKindOrphanLogHeld, Table: "proj", RowId: networkId.String(), Detail: held})
	}
	args, err := json.Marshal(&orphanLogJobArgs{IncidentId: incident.IncidentId(), NetworkId: networkId, RepoIds: jobRepoIds, ShredAt: shredAt})
	if err != nil {
		return err
	}
	// a rerun's request coalesces with the first, at its time and with its args
	if err := deps.Tasks.Schedule(ctx, orphanLogJobRequest(incident.IncidentId(), networkId, args, shredAt)); err != nil {
		return err
	}
	return incident.Append(&ListRow{
		Kind:   ListKindOrphanLogDeleteOwed,
		Table:  "proj",
		RowId:  networkId.String(),
		Detail: "proj/ and projrows/ deleted after the last shred of its repos",
	})
}

// ctx for one write on cluster, issued to budget as P0 of kind at operation's price; ctx as it is with no budget.
func issueOrphanP0(ctx context.Context, budget sidecar.WriteBudget, priceTable *evsservice.PriceTable, cluster sidecar.BlobCluster, kind sidecar.WriteP0Kind, operation evsservice.WriteOperation) (context.Context, error) {
	if budget == nil {
		return ctx, nil
	}
	price, err := priceTable.Price(operation)
	if err != nil {
		return nil, err
	}
	return budget.IssueP0(ctx, cluster.ClusterId(), kind, price), nil
}

// Moves one orphan's repo to shredding{incidentId, at} by If-Match while it is active or deleted, rereading repo/ on
// each try, and answers repo/ as it then stands. A repo in any other state is answered as read, unchanged.
func moveOrphanRepo(ctx context.Context, deps *OrphanLogDeps, settings *OrphanLogSettings, priceTable *evsservice.PriceTable, firstCluster sidecar.BlobCluster, incidentId sidecar.Id, networkId server.Id, repoId sidecar.Id, at time.Time) (*orphanRepoRead, error) {
	read, err := readOrphanRepo(ctx, deps.Blobs, firstCluster, networkId, repoId)
	if err != nil {
		return nil, err
	}
	for attempt := 0; read.movable(); attempt += 1 {
		next := proto.Clone(read.object).(*protocol.EvsRepoObject)
		next.State = protocol.EvsRepoState_EVS_REPO_SHREDDING
		next.DeletionId = incidentId.Bytes()
		// shredding names no signer: nothing was signed for an orphan's repos (QD-813)
		next.StateSigner = nil
		// the shred's first step waits from it
		next.StateMs = at.UnixMilli()
		if err := sidecar.CheckEvsRepoTransition(read.object, next); err != nil {
			return nil, err
		}
		nextBytes, err := sidecar.MarshalEvsRepoObject(next)
		if err != nil {
			return nil, err
		}
		writeCtx, err := issueOrphanP0(ctx, deps.Budget, priceTable, read.cluster, sidecar.WriteP0KindLifecycle, evsservice.WriteOperation{
			Kind:            evsservice.WriteOperationOverwrite,
			ObjectByteCount: int64(len(nextBytes)),
			Count:           1,
		})
		if err != nil {
			return nil, err
		}
		etag, err := read.cluster.PutIfMatch(writeCtx, sidecar.EvsRepoObjectKey(repoId), read.etag, nextBytes, nil)
		var unknown *sidecar.BlobUnknownError
		switch {
		case err == nil:
			return &orphanRepoRead{cluster: read.cluster, object: next, etag: etag}, nil
		case errors.Is(err, sidecar.ErrBlobPrecondition), errors.As(err, &unknown):
			// another write came first, or this one's answer was lost: the next read decides again
			if settings.RepoTransitionRetryCount <= attempt {
				return nil, fmt.Errorf("repo %s: repo/ kept moving under %d tries", repoId, attempt+1)
			}
		default:
			return nil, err
		}
		// err here is the loop body's own, so the reread's error is checked where it is assigned
		read, err = rereadOrphanRepo(ctx, read.cluster, networkId, repoId)
		if err != nil {
			return nil, err
		}
	}
	return read, nil
}

// One run of the job (see the file header). Before its time, or with the network's rows back, it changes nothing. It
// is idempotent: a run after a crash finds what is left and does it.
func RunOrphanLogJob(ctx context.Context, deps *OrphanLogDeps, args []byte) error {
	settings := deps.settings()
	jobArgs := &orphanLogJobArgs{}
	if err := json.Unmarshal(args, jobArgs); err != nil {
		return fmt.Errorf("%w: %v", ErrOrphanLogJobArgs, err)
	}
	if jobArgs.IncidentId == (server.Id{}) || jobArgs.NetworkId == (server.Id{}) || jobArgs.ShredAt.IsZero() {
		return fmt.Errorf("%w: no incident, network or shred time", ErrOrphanLogJobArgs)
	}
	restored, err := restoredNetworkIds(ctx, []server.Id{jobArgs.NetworkId})
	if err != nil {
		return err
	}
	if restored[jobArgs.NetworkId] {
		// the network's rows came back: its log and its repos are its own again, and the job ends
		return nil
	}
	now := deps.Now()
	if now.Before(jobArgs.ShredAt) {
		// early, as a run the queue gave sooner: nothing changes before the week is out
		return deps.Tasks.Schedule(ctx, orphanLogJobRequest(jobArgs.IncidentId, jobArgs.NetworkId, args, jobArgs.ShredAt))
	}
	firstCluster, err := deps.Blobs.FirstCluster()
	if err != nil {
		return err
	}
	// every repo read before any moves, so one the path cannot take leaves them all as they are
	for _, repoId := range jobArgs.RepoIds {
		read, err := readOrphanRepo(ctx, deps.Blobs, firstCluster, jobArgs.NetworkId, sidecar.Id(repoId))
		if err != nil {
			return err
		}
		if read.left != "" {
			return fmt.Errorf("%w: repo %s: %s", ErrOrphanRepoLeft, repoId, read.left)
		}
	}
	priceTable, err := evsservice.DefaultPriceTable(settings.PriceBlockByteCount)
	if err != nil {
		return err
	}
	incidentId := sidecar.Id(jobArgs.IncidentId)
	shredding := false
	for _, repoId := range jobArgs.RepoIds {
		read, err := moveOrphanRepo(ctx, deps, settings, priceTable, firstCluster, incidentId, jobArgs.NetworkId, sidecar.Id(repoId), now)
		if err != nil {
			return err
		}
		switch {
		case read.left != "":
			return fmt.Errorf("%w: repo %s: %s", ErrOrphanRepoLeft, repoId, read.left)
		case read.shredded():
		case read.movedUnder(incidentId):
			// moved now or by a run before, which may have ended before it queued the shred: queued again, it
			// coalesces with one already queued
			if err := deps.Tasks.Schedule(ctx, orphanShredRequest(sidecar.Id(repoId), incidentId, now)); err != nil {
				return err
			}
			shredding = true
		default:
			// shredding under another deletion, whose own shred runs
			shredding = true
		}
	}
	if shredding {
		return deps.Tasks.Schedule(ctx, orphanLogJobRequest(jobArgs.IncidentId, jobArgs.NetworkId, args, now.Add(settings.RecheckInterval)))
	}
	deletePrice, err := priceTable.Price(evsservice.WriteOperation{Kind: evsservice.WriteOperationDelete, Count: 1})
	if err != nil {
		return err
	}
	for _, prefix := range []string{
		fmt.Sprintf("projrows/%s/", sidecar.Id(jobArgs.NetworkId)),
		sidecar.ConfigEntryKeyPrefix(sidecar.Id(jobArgs.NetworkId)),
	} {
		if err := deleteOrphanPrefix(ctx, deps.Budget, deletePrice, firstCluster, prefix, settings.ListPageCount); err != nil {
			return err
		}
	}
	return nil
}

// Deletes every object under prefix on the first cluster, a page at a time, each delete P0 of kind log on budget when
// there is one.
func deleteOrphanPrefix(ctx context.Context, budget sidecar.WriteBudget, deletePrice int64, cluster sidecar.BlobCluster, prefix string, pageCount int) error {
	for {
		infos, _, err := cluster.List(ctx, prefix, "", pageCount)
		if err != nil {
			return err
		}
		if len(infos) == 0 {
			return nil
		}
		for _, info := range infos {
			deleteCtx := ctx
			if budget != nil {
				deleteCtx = budget.IssueP0(ctx, cluster.ClusterId(), sidecar.WriteP0KindLog, deletePrice)
			}
			if err := cluster.Delete(deleteCtx, info.Key); err != nil && !errors.Is(err, sidecar.ErrBlobNotFound) {
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
// file-clusters-taskworker.yml, which alone reaches repo/ on every metadata cluster and proj/, and the env's task
// queue for the job. The step writes nothing to the clusters, so it holds no write budget.
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
