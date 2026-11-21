package sidecarrestore

// §19.10's close step, the last of a site loss (OPS-M3-05; D-OPS-14, R-OPS-9, FS-41, FI-5, QD-214): after the 30 days
// re-seeding runs, with reseed turned off in sidecar.yml, the close report tells each network's admins, through their
// admin events (§7.11), which of their repos the restore could not bring back whole:
//
//	not re-seeded  a repo the resumed log projects as live or restorable that nothing re-seeded: no place/ on the first
//	               cluster, or no repo/ where it is placed
//	under reseed   a repo that came back only below a surviving reader's pin, under an EvsReseed (reseed/<repo>/ holds
//	               a statement), where a lost ERASE may let a stale mirror's slot keys return (§11.12's residual): the
//	               report names the repo, never an erasure
//	drops          which drops' listed content a stale mirror returned until the re-run job deleted it: v1's drop jobs
//	               (§11.13) run from M10, and their re-run's list is not read here, so the report says it covers none
//
// An ERASE no surviving device saw leaves nothing to name, and the report says so rather than imply it covers one
// (QD-214). A repo the log shreds (deleted with now, or past its 7 days unrestored) is not expected back and is not
// named. Each repo is listed once in the incident, and its admin event is written with its first listing, so the step
// may run again and adds nothing.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The list's rows the close step writes.
const (
	// a projected repo nothing re-seeded: table repo, row the repo, detail its network
	ListKindCloseNotReseeded = "close-repo-not-reseeded"
	// a repo back only under an EvsReseed: table repo, row the repo, detail its network
	ListKindCloseUnderReseed = "close-repo-under-reseed"
	// a network's log that does not verify, whose repos the report cannot name: table proj, row the network, detail why
	ListKindCloseLogUnreadable = "close-log-unreadable"
	// what the report covers and does not: table close, row the incident, detail the words
	ListKindCloseReport = "close-report"
)

// The admin events' words for the report (§7.11): fixed, never built from input.
const (
	closeAdminEventActionNotReseeded = "restore.repo_not_reseeded"
	closeAdminEventActionUnderReseed = "restore.repo_under_reseed"
	closeAdminEventTargetRepo        = "repo"
	closeAdminEventResultListed      = "listed"
)

// The close report's words on what it cannot name.
const (
	closeReportEraseResidual = "an ERASE no surviving device saw leaves nothing to name: a repo under an EvsReseed is " +
		"where a lost ERASE may let a stale mirror's slot keys return (§11.12's residual)"
	closeReportDropsNotRead = "drops: v1's drop jobs run from M10, and their re-run's list is not read here, so no " +
		"drop's returned content is named"
)

var (
	// The close runs only after the re-seeding period.
	ErrCloseTooEarly = errors.New("the close step waits for §19.10's re-seeding period")
	// reseed is still on in sidecar.yml.
	ErrCloseReseedOn = errors.New("reseed is on in sidecar.yml")
	// The incident's list records no end of storage qualification.
	ErrCloseNoHardwareEnd = errors.New("the incident records no end of the hardware step")
	// The incident has not completed its API replay behind the restore hold.
	ErrCloseNoApiEnd = errors.New("the incident records no end of the api step")
)

// The close step's tunables.
type CloseStepSettings struct {
	// Minimum re-seeding time once both the new hardware and the held API replay are ready.
	ReseedPeriod time.Duration
	// How long a deleted repo stays restorable, past which an unrestored one is shredded: §11.12's 7 days.
	RestorablePeriod time.Duration
	// The most keys one LIST returns.
	ListPageCount int
}

// The defaults.
func DefaultCloseStepSettings() *CloseStepSettings {
	return &CloseStepSettings{
		ReseedPeriod:     30 * 24 * time.Hour,
		RestorablePeriod: 7 * 24 * time.Hour,
		ListPageCount:    1000,
	}
}

// What the close step runs on. A nil field takes the real one where it has one.
type CloseStepDeps struct {
	// Each repo row a network's verified log holds, false with no log: model's sequencer's RestoreRepoRows.
	RepoRows func(ctx context.Context, networkId server.Id) ([]*protocol.AclRepo, bool, error)
	// The new clusters: place/ and proj/ on the first, each repo's own where it is placed.
	Blobs sidecar.BlobStore
	// Whether sidecar.yml's reseed is on.
	ReseedOn func() bool
	// Writes one admin event: model.AddAdminEventInTx in a transaction of its own.
	AddEvent func(ctx context.Context, event *model.AdminEvent) error
	Now      func() time.Time
	Settings *CloseStepSettings
	// Whether to check the database's restore marker: always, but for a test with no database.
	SkipMarker bool
}

// The first time both qualified hardware and the restored API could accept re-seeds.
// Older incident lists recorded hardware after api. Both orders retain their full window.
func reseedWindowStart(incident *Incident) (time.Time, error) {
	var hardwareEnd, apiEnd time.Time
	for _, row := range incident.RowsOfKind(ListKindStepEnded) {
		if row.Table != StepHardware && row.Table != StepApi {
			continue
		}
		text, _, _ := strings.Cut(row.Detail, " ")
		endTime, err := time.Parse(time.RFC3339, text)
		if err != nil {
			return time.Time{}, fmt.Errorf("%w: invalid %s step time %q", ErrIncidentList, row.Table, text)
		}
		if row.Table == StepHardware {
			hardwareEnd = endTime
		} else {
			apiEnd = endTime
		}
	}
	if hardwareEnd.IsZero() {
		return time.Time{}, ErrCloseNoHardwareEnd
	}
	if apiEnd.IsZero() {
		return time.Time{}, ErrCloseNoApiEnd
	}
	if apiEnd.After(hardwareEnd) {
		return apiEnd, nil
	}
	return hardwareEnd, nil
}

// Whether the log shreds the repo: deleted with now, or unrestored past its restorable period.
func closeRepoShredded(row *protocol.AclRepo, now time.Time, restorablePeriod time.Duration) bool {
	deletion := row.GetDeletion()
	if deletion == nil || len(deletion.GetDeletionId()) == 0 || deletion.GetRestoredAt() != 0 {
		return false
	}
	if deletion.GetNow() {
		return true
	}
	return !now.Before(time.UnixMilli(deletion.GetDeletedTimeMs()).Add(restorablePeriod))
}

// How the restore left one projected repo.
type closeRepoOutcome int

const (
	closeRepoBack closeRepoOutcome = iota
	closeRepoNotReseeded
	closeRepoUnderReseed
)

// Where the new clusters leave repoId: not re-seeded, back under an EvsReseed, or back.
func readCloseRepo(ctx context.Context, blobs sidecar.BlobStore, firstCluster sidecar.BlobCluster, repoId sidecar.Id, pageCount int) (closeRepoOutcome, error) {
	place, err := sidecar.ReadEvsRepoPlace(ctx, firstCluster, repoId)
	if errors.Is(err, sidecar.ErrEvsRepoNotPlaced) {
		return closeRepoNotReseeded, nil
	}
	if err != nil {
		return closeRepoBack, err
	}
	cluster, err := blobs.Cluster(place.ClusterId)
	if err != nil {
		return closeRepoBack, err
	}
	if _, _, err := sidecar.ReadEvsRepoObject(ctx, cluster, repoId); errors.Is(err, sidecar.ErrEvsRepoNotFound) {
		return closeRepoNotReseeded, nil
	} else if err != nil {
		return closeRepoBack, err
	}
	statements, _, err := cluster.List(ctx, "reseed/"+sidecar.EvsKeyId(repoId)+"/", "", min(1, pageCount))
	if err != nil {
		return closeRepoBack, err
	}
	if 0 < len(statements) {
		return closeRepoUnderReseed, nil
	}
	return closeRepoBack, nil
}

// The close step (see the file header) under run's incident.
func RunCloseStep(ctx context.Context, deps *CloseStepDeps, run *StepRun) error {
	settings := deps.Settings
	if settings == nil {
		settings = DefaultCloseStepSettings()
	}
	now := deps.Now
	if now == nil {
		now = server.NowUtc
	}
	addEvent := deps.AddEvent
	if addEvent == nil {
		addEvent = addCloseAdminEvent
	}
	incident := run.Incident
	if !deps.SkipMarker {
		if err := CheckRestoreMarker(ctx, incident.IncidentId()); err != nil {
			return err
		}
	}
	reseedStart, err := reseedWindowStart(incident)
	if err != nil {
		return err
	}
	if closeAt := reseedStart.Add(settings.ReseedPeriod); now().Before(closeAt) {
		return fmt.Errorf("%w: re-seeding runs until %s", ErrCloseTooEarly, closeAt.UTC().Format(time.RFC3339))
	}
	if deps.ReseedOn() {
		return fmt.Errorf("%w: set reseed: false, wait for the blocks' next read, then run close again", ErrCloseReseedOn)
	}
	firstCluster, err := deps.Blobs.FirstCluster()
	if err != nil {
		return err
	}
	networkIds, err := listLogNetworks(ctx, firstCluster, settings.ListPageCount)
	if err != nil {
		return fmt.Errorf("the logs on the first cluster: %w", err)
	}
	notReseededCount, underReseedCount := 0, 0
	for _, networkId := range networkIds {
		rows, found, err := deps.RepoRows(ctx, networkId)
		if err != nil || !found {
			reason := "the log does not verify from its genesis"
			if err != nil {
				reason = err.Error()
			}
			if err := appendOnce(incident, &ListRow{Kind: ListKindCloseLogUnreadable, Table: "proj", RowId: networkId.String(), Detail: reason}); err != nil {
				return err
			}
			continue
		}
		for _, row := range rows {
			if closeRepoShredded(row, now(), settings.RestorablePeriod) {
				continue
			}
			repoId, err := sidecar.IdFromBytes(row.GetRepoId())
			if err != nil {
				return fmt.Errorf("network %s's log holds a repo row with no id", networkId)
			}
			outcome, err := readCloseRepo(ctx, deps.Blobs, firstCluster, repoId, settings.ListPageCount)
			if err != nil {
				return fmt.Errorf("repo %s: %w", repoId, err)
			}
			kind, action := "", ""
			switch outcome {
			case closeRepoNotReseeded:
				kind, action = ListKindCloseNotReseeded, closeAdminEventActionNotReseeded
				notReseededCount += 1
			case closeRepoUnderReseed:
				kind, action = ListKindCloseUnderReseed, closeAdminEventActionUnderReseed
				underReseedCount += 1
			default:
				continue
			}
			listed := false
			for _, held := range incident.RowsOfKind(kind) {
				listed = listed || held.RowId == repoId.String()
			}
			if listed {
				continue
			}
			// the event first, so a run cut short between the two writes the event again rather than never
			targetId := server.Id(repoId)
			event := &model.AdminEvent{
				NetworkId:  networkId,
				ActorKind:  model.AdminEventActorSystem,
				Action:     action,
				TargetKind: closeAdminEventTargetRepo,
				TargetId:   &targetId,
				Result:     closeAdminEventResultListed,
			}
			if err := addEvent(ctx, event); err != nil {
				return fmt.Errorf("network %s's admin event for repo %s: %w", networkId, repoId, err)
			}
			if err := incident.Append(&ListRow{Kind: kind, Table: "repo", RowId: repoId.String(), Detail: "network " + networkId.String()}); err != nil {
				return err
			}
		}
	}
	for _, words := range []string{closeReportEraseResidual, closeReportDropsNotRead} {
		if err := incident.Append(&ListRow{Kind: ListKindCloseReport, Table: "close", RowId: incident.IncidentId().String(), Detail: words}); err != nil {
			return err
		}
	}
	fmt.Fprintf(run.Out, "the close report is done: %d networks' logs read, %d repos nothing re-seeded, %d back only under an EvsReseed; %s; %s\n",
		len(networkIds), notReseededCount, underReseedCount, closeReportEraseResidual, closeReportDropsNotRead)
	return nil
}

// One admin event in a transaction of its own.
func addCloseAdminEvent(ctx context.Context, event *model.AdminEvent) (returnErr error) {
	if !server.SidecarMg1Linked() {
		return errors.New("this binary does not link MG1's admin events")
	}
	if r := server.HandleError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			model.AddAdminEventInTx(ctx, tx, event)
		})
	}); r != nil {
		return fmt.Errorf("%v", r)
	}
	return nil
}

// The close step as the process's frame runs it: the new clusters through api's own file-clusters-api.yml, as the
// api step reads them, a sequencer of its own over the first, and sidecar.yml's reseed as this process reads it.
func init() {
	RegisterStep(StepClose, func(ctx context.Context, run *StepRun) error {
		clusters, err := server.LoadFileClusters(ctx, apiStepFileClustersService, nil)
		if err != nil {
			return fmt.Errorf("the file clusters: %w", err)
		}
		defer clusters.Close()
		firstCluster, err := clusters.FirstCluster()
		if err != nil {
			return err
		}
		sequencer := model.NewConfigLogSequencer(firstCluster, nil, model.AdvanceNetworkAclHead)
		return RunCloseStep(ctx, &CloseStepDeps{
			RepoRows: sequencer.RestoreRepoRows,
			Blobs:    clusters,
			ReseedOn: func() bool { return server.CurrentSidecarConfig().Reseed() },
		}, run)
	})
}
