//go:build sidecar_pending

package sidecarrestore

// FS-41's frame at M3 on T3 (D-OPS-14, R-OPS-9, FS-41, FI-5, FI-12; OPS-M3-05): a site loss through §19.10's order
// whole, the Postgres steps then the site's, each step twice, on SiteLossFixture: both dumps restored through xops's
// real scripts on the private server under a site incident, every bucket lost, reseed on. Three networks' logs were
// on the site; after the loss the hardware step finds the new store empty and answering, the authority step resumes
// each log verbatim, the repos step re-seeds one network's config repo whole and another's only under an EvsReseed,
// and the third's not at all. Then the close: refused before §19.10's 30 days and while reseed is on; after, it names
// the third's config repo as nothing re-seeded and the second's as back only under an EvsReseed, in the incident and
// in each network's admin events, never the first's, says it names no erasure, and its rerun adds nothing.
//
// Each owner's own FS-41 clauses stay theirs and are not rerun here: no reader sees a FORK, one member's forged TICK
// takes no position, a run failing the new-device check is discarded and a Team of read_only others resumes
// (TEAM-M3-02, on T2); a shredded repo is not resurrected and an entry erased after a stale mirror's sync stays erased
// (EVSB-M2-20); a recovery-code-only member keeps access (KEYS-M2-25). The resumed log and the re-seeds here are the
// fixture's stand-ins for those mechanisms' outcomes. It needs what FS-42's T3 needs and runs where SIDECAR_RESTORE_T3=1.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The config repo a network's log projects, read from the store it is on.
func fs41ConfigRepoId(t testing.TB, ctx context.Context, cluster sidecar.BlobCluster, networkId server.Id) sidecar.Id {
	t.Helper()
	rows, found, err := model.NewConfigLogSequencer(cluster, nil, nil).RestoreRepoRows(ctx, networkId)
	if err != nil || !found {
		t.Fatalf("network %s's log: %v, found %t", networkId, err, found)
	}
	for _, row := range rows {
		if row.GetKind() == protocol.RepoKind_REPO_KIND_CONFIG {
			repoId, err := sidecar.IdFromBytes(row.GetRepoId())
			if err != nil {
				t.Fatal(err)
			}
			return repoId
		}
	}
	t.Fatalf("network %s's log projects no config repo", networkId)
	return sidecar.Id{}
}

// The admin events the close step wrote for the network, by action.
func fs41CloseEvents(ctx context.Context, networkId server.Id) map[string][]server.Id {
	events := map[string][]server.Id{}
	for _, event := range model.GetNetworkAdminEvents(ctx, networkId, 0, 100) {
		if event.TargetKind == closeAdminEventTargetRepo && event.TargetId != nil {
			events[event.Action] = append(events[event.Action], *event.TargetId)
		}
	}
	return events
}

func TestFs41SiteLossThroughTheCloseOnT3(t *testing.T) {
	requireFs42T3(t)
	server.DefaultTestEnv().Run(t, runFs41SiteLoss)
}

// FS-41's M3 schedule (see the file header).
func runFs41SiteLoss(t testing.TB) {
	ctx := t.Context()
	siteBlobs := sidecartest.NewFakeBlobs(sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), sidecartest.NewFaultQueue(), nil, sidecartest.DefaultFakeBlobsSettings())
	siteFirst, err := siteBlobs.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	wholeId, statedId, lostId := server.NewId(), server.NewId(), server.NewId()
	fixture := NewSiteLossFixture(t, nil, siteBlobs, fmt.Sprintf("  preview_networks: [%q, %q, %q]\n", wholeId, statedId, lostId))
	defer fixture.Close()
	defer server.Testing_PushSidecarConfig(fmt.Sprintf("sidecar:\n  preview_networks: [%q, %q, %q]\n", wholeId, statedId, lostId))()

	// before the loss: three networks, each with its log on the site's first cluster
	networks := []*fs42Network{}
	for index, networkId := range []server.Id{wholeId, statedId, lostId} {
		network := newFs42Network(t, ctx, networkId, fmt.Sprint("site", index))
		newTestLogWorld(t, ctx, siteFirst, network.networkId, network.userId)
		networks = append(networks, network)
	}
	configRepoIds := map[server.Id]sidecar.Id{}
	for _, network := range networks {
		configRepoIds[network.networkId] = fs41ConfigRepoId(t, ctx, siteFirst, network.networkId)
	}
	fixture.TakeMainDump()
	fixture.TakeSidecarDump()

	incident := fixture.LoseSite(server.NewId())
	connect.AssertEqual(t, server.CurrentSidecarConfig().Reseed(), true)
	newBlobs := fixture.NewBlobs()
	newFirst, err := newBlobs.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}

	closeNow := time.Time{}
	frame := NewFrame()
	frame.Register(StepOps, func(ctx context.Context, run *StepRun) error {
		settings := model.DefaultConfigLogSettings()
		return RunOpsStep(ctx, &OrphanLogDeps{
			Blobs:     newBlobs,
			Sequencer: model.NewConfigLogSequencer(newFirst, settings, nil),
			Tasks:     sidecartest.NewFakeTasks(sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), sidecartest.NewFaultQueue(), nil),
			Now:       server.NowUtc,
		}, run)
	})
	frame.Register(StepHardware, func(ctx context.Context, run *StepRun) error {
		// §19.10's hardware row: new hardware with empty buckets passes §22's suite, which runs on T3's real
		// clusters (EVSB-M2-17's filecluster-suite); here the new store is empty and answers
		if incident.StepEnded(StepHardware) {
			return nil
		}
		return fixture.NewStoreEmpty(ctx)
	})
	frame.Register(StepAuthority, func(ctx context.Context, run *StepRun) error {
		for _, network := range networks {
			fixture.ResumeLog(ctx, network.networkId)
		}
		return nil
	})
	frame.Register(StepRepos, func(ctx context.Context, run *StepRun) error {
		fixture.ReseedRepo(ctx, wholeId, configRepoIds[wholeId], protocol.RepoKind_REPO_KIND_CONFIG, false)
		fixture.ReseedRepo(ctx, statedId, configRepoIds[statedId], protocol.RepoKind_REPO_KIND_CONFIG, true)
		return nil
	})
	frame.Register(StepClose, func(ctx context.Context, run *StepRun) error {
		return RunCloseStep(ctx, &CloseStepDeps{
			RepoRows: model.NewConfigLogSequencer(newFirst, nil, nil).RestoreRepoRows,
			Blobs:    newBlobs,
			ReseedOn: func() bool { return server.CurrentSidecarConfig().Reseed() },
			Now:      func() time.Time { return closeNow },
		}, run)
	})
	byHand := map[string]string{
		StepAbuse:   "no abuse mailbox record after the sidecar dump in this drill",
		StepDns:     "no dns tables are restored before M5 in this drill",
		StepApi:     "a site loss's api replay waits for the resumed log; the authority row's api re-creates rev/ from tombstones",
		StepLabels:  "no dns_label replay before M5 in this drill",
		StepRows:    "the daemons re-register on restore_required (§19.10's rows step), outside the server",
		StepBilling: "no meter rows before MG6 in this drill",
		StepGroups:  "the groups re-form under GroupForm (KEYS-M2-25's tests)",
		StepDrops:   "no drop job runs before M10",
	}
	out := &bytes.Buffer{}

	// every step through drops, the close refused before 30 days of re-seeding and while reseed is on
	err = fixture.RunFrameThrough(ctx, frame, StepDrops, byHand, out, nil)
	connect.AssertEqual(t, err, nil)
	hardwareEnd, err := hardwareStepEnd(incident)
	connect.AssertEqual(t, err, nil)
	closeNow = hardwareEnd.Add(29 * 24 * time.Hour)
	connect.AssertEqual(t, errors.Is(frame.RunStep(ctx, incident, StepClose, out, ""), ErrCloseTooEarly), true)
	closeNow = hardwareEnd.Add(31 * 24 * time.Hour)
	connect.AssertEqual(t, errors.Is(frame.RunStep(ctx, incident, StepClose, out, ""), ErrCloseReseedOn), true)
	connect.AssertEqual(t, incident.StepEnded(StepClose), false)

	fixture.EndReseed()
	connect.AssertEqual(t, server.CurrentSidecarConfig().Reseed(), false)
	for range 2 {
		connect.AssertEqual(t, frame.RunStep(ctx, incident, StepClose, out, ""), nil)
	}
	connect.AssertEqual(t, incident.StepEnded(StepClose), true)

	notReseeded := incident.RowsOfKind(ListKindCloseNotReseeded)
	connect.AssertEqual(t, len(notReseeded), 1)
	connect.AssertEqual(t, notReseeded[0].RowId, configRepoIds[lostId].String())
	under := incident.RowsOfKind(ListKindCloseUnderReseed)
	connect.AssertEqual(t, len(under), 1)
	connect.AssertEqual(t, under[0].RowId, configRepoIds[statedId].String())
	connect.AssertEqual(t, len(incident.RowsOfKind(ListKindCloseReport)), 2)

	connect.AssertEqual(t, len(fs41CloseEvents(ctx, wholeId)), 0)
	connect.AssertEqual(t, fs41CloseEvents(ctx, statedId), map[string][]server.Id{closeAdminEventActionUnderReseed: {server.Id(configRepoIds[statedId])}})
	connect.AssertEqual(t, fs41CloseEvents(ctx, lostId), map[string][]server.Id{closeAdminEventActionNotReseeded: {server.Id(configRepoIds[lostId])}})
}
