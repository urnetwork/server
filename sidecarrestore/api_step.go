package sidecarrestore

// §19.10's api step (OPS-M3-03), which runs after the abuse and dns steps and before api's public routes, the token
// exchange and CreateContract reopen. Each part is idempotent and keyed by the incident, and its outcomes go in the
// incident's list:
//
//	marker   the database is the one xops's restore script loaded under this incident (CheckRestoreMarker)
//	hold     sidecar.yml's restore_hold holds api's routes, the exchange and CreateContract (plan-OPS-restore-hold,
//	         QD-076), or the operator states by hand how they are held; neither refuses the step
//	heads    each network's proj/ entries past its restored network_acl_head are verified as §7.9's readers verify
//	         them and the head advanced (n, A_n, has_document), so a first policy document filed after the dumps
//	         holds: model.AdvanceAclHeadsAfterRestore for the restored heads, and model.AdvanceAclHeadAfterRestore for
//	         a network with a log and a restored row but no head, whose log began after the sidecar dump
//	effects  §7.10's effects again for every removal or revocation the log records past what the restored rows show,
//	         behind the head too (model.ReapplyRemovalEffects), each applied once, by its admin event
//	rev      rev/ objects owed again for every client and sidecar the restored rows record as removed within rev/'s
//	         20 minutes (model.RecreateRecentRevocations), which WriteSidecarRevTask lands
//
// It never reissues a statement or signs anything for a device (I-4): it applies what the log's signed entries and the
// restored rows already say. The end is the operator's: clear restore_hold, which reopens everything at once. The
// push tokens' revision raise in §19.10's api row is OPS-M8-01's, plugged in beside this at M8.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The list's rows the api step writes.
const (
	// a head that moved: table network_acl_head, row the network, detail from and to
	ListKindAclHeadAdvanced = "acl-head-advanced"
	// a head that could not move: detail why
	ListKindAclHeadRefused = "acl-head-refused"
	// effects applied again: table network, row the network, detail how many entries'
	ListKindEffectsReapplied = "effects-reapplied"
	// a network whose effects could not be applied: detail why
	ListKindEffectsRefused = "effects-refused"
	// rev/ objects owed again: table rev, row the incident, detail how many
	ListKindRevOwed = "rev-owed"
	// how the hold stood while the step ran: table restore_hold, detail sidecar.yml or the operator's statement
	ListKindRestoreHold = "restore-hold"
)

// Neither sidecar.yml's restore_hold nor the operator's statement holds api's routes, the exchange and CreateContract,
// so the step would replay the log while they serve the restored rows.
var ErrRestoreHoldOpen = errors.New("restore_hold does not hold api's routes, the token exchange and CreateContract")

// The api step's tunables.
type ApiStepSettings struct {
	// The most keys one LIST of proj/ returns while the step finds the logs the first cluster holds.
	ListPageCount int
	// nil for model's defaults.
	Effects *model.ConfigLogEffectsSettings
	// nil for model's defaults; DeleteAfter is the 20 minutes rev/ objects are owed for.
	Rev *model.SidecarRevSettings
}

// The defaults.
func DefaultApiStepSettings() *ApiStepSettings {
	return &ApiStepSettings{
		ListPageCount: 1000,
	}
}

// What the api step runs on.
type ApiStepDeps struct {
	// a log sequencer over the first metadata cluster, which reads every log from the store
	Sequencer *model.ConfigLogSequencer
	// the first metadata cluster, whose proj/ the step lists for networks with a log but no restored head
	FirstCluster sidecar.BlobCluster
	// whether sidecar.yml holds api's routes, the exchange and CreateContract now
	RestoreHeld func() bool
	// the clock rev/'s 20 minutes count back from
	Now func() time.Time
	// nil for the defaults
	Settings *ApiStepSettings
}

// The api step (see the file header) under run's incident.
func RunApiStep(ctx context.Context, deps *ApiStepDeps, run *StepRun) error {
	settings := deps.Settings
	if settings == nil {
		settings = DefaultApiStepSettings()
	}
	incident := run.Incident
	if err := CheckRestoreMarker(ctx, incident.IncidentId()); err != nil {
		return err
	}
	holdDetail := "sidecar.yml restore_hold"
	if !deps.RestoreHeld() {
		if run.Statement == "" {
			return fmt.Errorf("%w: set restore_hold: true in sidecar.yml, or state how they are held with --statement", ErrRestoreHoldOpen)
		}
		holdDetail = "stated by hand: " + run.Statement
	}
	older, err := incident.OlderSnapshotTime()
	if err != nil {
		return err
	}
	fmt.Fprintf(run.Out, "replaying the logs past the restored heads; the older dump's snapshot is %s\n", older.Format(time.RFC3339Nano))

	// heads
	outcomes, err := model.AdvanceAclHeadsAfterRestore(ctx, deps.Sequencer)
	if err != nil {
		return fmt.Errorf("the heads: %w", err)
	}
	headNetworkIds := map[server.Id]bool{}
	for _, outcome := range outcomes {
		headNetworkIds[outcome.NetworkId] = true
	}
	logNetworkIds, err := listLogNetworks(ctx, deps.FirstCluster, settings.ListPageCount)
	if err != nil {
		return fmt.Errorf("the logs on the first cluster: %w", err)
	}
	restored, err := restoredNetworkIds(ctx, logNetworkIds)
	if err != nil {
		return err
	}
	for _, networkId := range logNetworkIds {
		if headNetworkIds[networkId] || !restored[networkId] {
			continue
		}
		outcome, err := model.AdvanceAclHeadAfterRestore(ctx, deps.Sequencer, networkId)
		if err != nil {
			return fmt.Errorf("the head of %s: %w", networkId, err)
		}
		outcomes = append(outcomes, outcome)
	}
	advancedCount := 0
	for _, outcome := range outcomes {
		var appendErr error
		switch {
		case outcome.Err != nil:
			appendErr = incident.Append(&ListRow{Kind: ListKindAclHeadRefused, Table: "network_acl_head", RowId: outcome.NetworkId.String(), Detail: outcome.Err.Error()})
		case outcome.To != outcome.From:
			advancedCount += 1
			appendErr = incident.Append(&ListRow{Kind: ListKindAclHeadAdvanced, Table: "network_acl_head", RowId: outcome.NetworkId.String(), Detail: fmt.Sprintf("%d -> %d", outcome.From, outcome.To)})
		}
		if appendErr != nil {
			return appendErr
		}
	}
	fmt.Fprintf(run.Out, "%d heads read, %d advanced\n", len(outcomes), advancedCount)

	// effects, for every network with a log whose rows came back
	effectNetworkIds := []server.Id{}
	for networkId := range headNetworkIds {
		effectNetworkIds = append(effectNetworkIds, networkId)
	}
	for _, networkId := range logNetworkIds {
		if restored[networkId] && !headNetworkIds[networkId] {
			effectNetworkIds = append(effectNetworkIds, networkId)
		}
	}
	slices.SortFunc(effectNetworkIds, func(a server.Id, b server.Id) int { return a.Cmp(b) })
	effectsRefused := 0
	for _, networkId := range effectNetworkIds {
		count, effectsErr := model.ReapplyRemovalEffects(ctx, deps.Sequencer, networkId, settings.Effects)
		var appendErr error
		switch {
		case effectsErr != nil:
			// listed, and the step does not end: a removal left unapplied must keep the routes held
			effectsRefused += 1
			appendErr = incident.Append(&ListRow{Kind: ListKindEffectsRefused, Table: "network", RowId: networkId.String(), Detail: effectsErr.Error()})
		case 0 < count:
			appendErr = incident.Append(&ListRow{Kind: ListKindEffectsReapplied, Table: "network", RowId: networkId.String(), Detail: fmt.Sprintf("%d entries", count)})
		}
		if appendErr != nil {
			return appendErr
		}
	}
	if 0 < effectsRefused {
		return fmt.Errorf("%d networks' removals could not be applied again; the list names them, and the step runs again once they can", effectsRefused)
	}

	// rev/
	revCount, err := model.RecreateRecentRevocations(ctx, deps.Now(), settings.Rev)
	if err != nil {
		return fmt.Errorf("rev/: %w", err)
	}
	if err := incident.Append(&ListRow{Kind: ListKindRevOwed, Table: "rev", RowId: incident.IncidentId().String(), Detail: fmt.Sprintf("%d objects", revCount)}); err != nil {
		return err
	}
	if err := incident.Append(&ListRow{Kind: ListKindRestoreHold, Table: "restore_hold", RowId: incident.IncidentId().String(), Detail: holdDetail}); err != nil {
		return err
	}
	fmt.Fprintf(run.Out, "%d networks' removals replayed, %d rev/ objects owed\n", len(effectNetworkIds), revCount)
	fmt.Fprintf(run.Out, "the api step is done: clear restore_hold in sidecar.yml to reopen api's routes, the token exchange and CreateContract\n")
	return nil
}

// The api step as the process's frame runs it: on the env WARP_ENV names, the first cluster through api's own
// file-clusters-api.yml (plan-OPS-small-readings #7), a sequencer of its own over it that advances network_acl_head,
// and the hold as this process reads sidecar.yml.
func init() {
	RegisterStep(StepApi, func(ctx context.Context, run *StepRun) error {
		clusters, err := server.LoadFileClusters(ctx, apiStepFileClustersService, nil)
		if err != nil {
			return fmt.Errorf("the first metadata cluster: %w", err)
		}
		defer clusters.Close()
		firstCluster, err := clusters.FirstCluster()
		if err != nil {
			return err
		}
		return RunApiStep(ctx, &ApiStepDeps{
			Sequencer:    model.NewConfigLogSequencer(firstCluster, nil, model.AdvanceNetworkAclHead),
			FirstCluster: firstCluster,
			RestoreHeld:  func() bool { return server.CurrentSidecarConfig().RestoreHold() },
			Now:          server.NowUtc,
		}, run)
	})
}

// The block whose cluster credentials the api step uses: api's, which reach proj/, projrows/ and rev/ on the first
// cluster, all the step reads and owes.
const apiStepFileClustersService = "api"
