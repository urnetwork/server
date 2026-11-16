package sidecarrestore

// §19.10's close step (OPS-M3-05) on a fake store with no database: it waits 30 days after the hardware step and for
// reseed to be off; it names each projected repo nothing re-seeded and each that came back only under an EvsReseed, in
// the incident and its network's admin events, never a repo the log shreds; it says what it cannot name; and a rerun
// adds nothing (QD-214's flag by repo, FI-5's stated residual).

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A fake store of one metadata cluster and a segment cluster, on a fake clock.
func testCloseBlobs() *sidecartest.FakeBlobs {
	return sidecartest.NewFakeBlobs(sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), sidecartest.NewFaultQueue(), nil, sidecartest.DefaultFakeBlobsSettings())
}

// Places repoId on the store's first cluster and writes its repo/ there, as a re-seed's first writes leave it; with
// underStatement, a Signed EvsReseed under reseed/<repo>/<ref> too.
func testCloseReseedRepo(t testing.TB, ctx context.Context, blobs *sidecartest.FakeBlobs, networkId server.Id, repoId sidecar.Id, underStatement bool) {
	t.Helper()
	first, err := blobs.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	placement, err := (&sidecar.EvsRepoPlace{ClusterId: first.ClusterId(), CreatorSidecarId: sidecar.NewId()}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.PutIfNoneMatch(ctx, sidecar.EvsPlaceKey(repoId), placement, nil); err != nil {
		t.Fatal(err)
	}
	object, err := sidecar.MarshalEvsRepoObject(&protocol.EvsRepoObject{
		RepoId:    repoId.Bytes(),
		Kind:      protocol.RepoKind_REPO_KIND_GENERAL,
		NetworkId: sidecar.Id(networkId).Bytes(),
		State:     protocol.EvsRepoState_EVS_REPO_ACTIVE,
		Format:    sidecar.EvsRepoFormat1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.PutIfNoneMatch(ctx, sidecar.EvsRepoObjectKey(repoId), object, nil); err != nil {
		t.Fatal(err)
	}
	if underStatement {
		if _, err := first.PutIfNoneMatch(ctx, "reseed/"+repoId.String()+"/"+sidecar.NewId().String(), []byte("a signed EvsReseed"), nil); err != nil {
			t.Fatal(err)
		}
	}
}

// Writes an entry key under networkId's log on the store's first cluster, so the step lists the network.
func testCloseLogEntry(t testing.TB, ctx context.Context, blobs *sidecartest.FakeBlobs, networkId server.Id) {
	t.Helper()
	first, err := blobs.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.PutIfNoneMatch(ctx, sidecar.ConfigEntryKeyPrefix(sidecar.Id(networkId))+"0000000000000001", []byte("an entry"), nil); err != nil {
		t.Fatal(err)
	}
}

// A repo row, deleted at deletedAt with now or not, or live when deletedAt is zero.
func testCloseRepoRow(repoId sidecar.Id, deletedAt time.Time, now bool) *protocol.AclRepo {
	row := &protocol.AclRepo{RepoId: repoId.Bytes()}
	if !deletedAt.IsZero() {
		row.Deletion = &protocol.AclRepoDeletionRow{DeletionId: sidecar.NewId().Bytes(), Now: now, DeletedAt: 3, DeletedTimeMs: deletedAt.UnixMilli()}
	}
	return row
}

// A site incident whose steps through hardware have ended, by hand, now.
func testSiteIncidentPastHardware(t testing.TB) *Incident {
	t.Helper()
	incident := testIncident(t, IncidentKindSite)
	frame := NewFrame()
	for _, step := range []string{StepOps, StepAbuse, StepDns, StepApi, StepLabels, StepRows, StepBilling, StepHardware} {
		if err := frame.EndStepByHand(incident, step, "by hand in this test"); err != nil {
			t.Fatal(err)
		}
	}
	return incident
}

func TestCloseStepNamesWhatTheRestoreCouldNotBringBack(t *testing.T) {
	ctx := t.Context()
	blobs := testCloseBlobs()
	networkId := server.NewId()
	back, missing, underReseed, restorable, shreddedNow, shreddedOld := sidecar.NewId(), sidecar.NewId(), sidecar.NewId(), sidecar.NewId(), sidecar.NewId(), sidecar.NewId()
	testCloseLogEntry(t, ctx, blobs, networkId)
	testCloseReseedRepo(t, ctx, blobs, networkId, back, false)
	testCloseReseedRepo(t, ctx, blobs, networkId, underReseed, true)

	incident := testSiteIncidentPastHardware(t)
	hardwareEnd, err := hardwareStepEnd(incident)
	connect.AssertEqual(t, err, nil)
	now := hardwareEnd.Add(29 * 24 * time.Hour)
	reseedOn := true
	events := []*model.AdminEvent{}
	deps := &CloseStepDeps{
		RepoRows: func(ctx context.Context, logNetworkId server.Id) ([]*protocol.AclRepo, bool, error) {
			connect.AssertEqual(t, logNetworkId, networkId)
			return []*protocol.AclRepo{
				testCloseRepoRow(back, time.Time{}, false),
				testCloseRepoRow(missing, time.Time{}, false),
				testCloseRepoRow(underReseed, time.Time{}, false),
				testCloseRepoRow(restorable, now.Add(-2*24*time.Hour), false),
				testCloseRepoRow(shreddedNow, now.Add(-time.Hour), true),
				testCloseRepoRow(shreddedOld, now.Add(-8*24*time.Hour), false),
			}, true, nil
		},
		Blobs:    blobs,
		ReseedOn: func() bool { return reseedOn },
		AddEvent: func(ctx context.Context, event *model.AdminEvent) error {
			events = append(events, event)
			return nil
		},
		Now:        func() time.Time { return now },
		SkipMarker: true,
	}
	run := &StepRun{Incident: incident, Out: &bytes.Buffer{}}

	// 29 days after the hardware step: re-seeding still runs
	connect.AssertEqual(t, errors.Is(RunCloseStep(ctx, deps, run), ErrCloseTooEarly), true)
	now = hardwareEnd.Add(31 * 24 * time.Hour)
	deps.RepoRows = func(ctx context.Context, logNetworkId server.Id) ([]*protocol.AclRepo, bool, error) {
		return []*protocol.AclRepo{
			testCloseRepoRow(back, time.Time{}, false),
			testCloseRepoRow(missing, time.Time{}, false),
			testCloseRepoRow(underReseed, time.Time{}, false),
			testCloseRepoRow(restorable, now.Add(-2*24*time.Hour), false),
			testCloseRepoRow(shreddedNow, now.Add(-time.Hour), true),
			testCloseRepoRow(shreddedOld, now.Add(-8*24*time.Hour), false),
		}, true, nil
	}
	// reseed still on
	connect.AssertEqual(t, errors.Is(RunCloseStep(ctx, deps, run), ErrCloseReseedOn), true)
	connect.AssertEqual(t, len(events), 0)
	reseedOn = false

	for range 2 {
		connect.AssertEqual(t, RunCloseStep(ctx, deps, run), nil)
	}
	notReseeded := map[string]bool{}
	for _, row := range incident.RowsOfKind(ListKindCloseNotReseeded) {
		notReseeded[row.RowId] = true
		connect.AssertEqual(t, row.Detail, "network "+networkId.String())
	}
	connect.AssertEqual(t, notReseeded, map[string]bool{missing.String(): true, restorable.String(): true})
	under := incident.RowsOfKind(ListKindCloseUnderReseed)
	connect.AssertEqual(t, len(under), 1)
	connect.AssertEqual(t, under[0].RowId, underReseed.String())
	// one event a repo named, in its network's chain, written once
	connect.AssertEqual(t, len(events), 3)
	for _, event := range events {
		connect.AssertEqual(t, event.NetworkId, networkId)
		connect.AssertEqual(t, event.ActorKind, model.AdminEventActorSystem)
		connect.AssertEqual(t, event.TargetKind, closeAdminEventTargetRepo)
		connect.AssertEqual(t, event.TargetId == nil, false)
	}
	// the report says what it cannot name, never an erasure
	report := []string{}
	for _, row := range incident.RowsOfKind(ListKindCloseReport) {
		report = append(report, row.Detail)
	}
	connect.AssertEqual(t, report, []string{closeReportEraseResidual, closeReportDropsNotRead})
	for _, row := range incident.Rows() {
		connect.AssertEqual(t, strings.Contains(row.Detail, "erased "), false)
	}
}

// The close waits for the hardware step's end, which the list must record.
func TestCloseStepNeedsTheHardwareStepsEnd(t *testing.T) {
	incident := testIncident(t, IncidentKindSite)
	deps := &CloseStepDeps{Blobs: testCloseBlobs(), ReseedOn: func() bool { return false }, SkipMarker: true}
	err := RunCloseStep(t.Context(), deps, &StepRun{Incident: incident, Out: &bytes.Buffer{}})
	connect.AssertEqual(t, errors.Is(err, ErrCloseNoHardwareEnd), true)
}

// A log the step cannot read is listed, its repos unnamed, and the report goes on.
func TestCloseStepListsALogItCannotRead(t *testing.T) {
	ctx := t.Context()
	blobs := testCloseBlobs()
	networkId := server.NewId()
	testCloseLogEntry(t, ctx, blobs, networkId)
	incident := testSiteIncidentPastHardware(t)
	hardwareEnd, _ := hardwareStepEnd(incident)
	deps := &CloseStepDeps{
		RepoRows: func(ctx context.Context, networkId server.Id) ([]*protocol.AclRepo, bool, error) {
			return nil, true, model.ErrRestoreRepoIdsUnreadable
		},
		Blobs:      blobs,
		ReseedOn:   func() bool { return false },
		AddEvent:   func(ctx context.Context, event *model.AdminEvent) error { return errors.New("no event expected") },
		Now:        func() time.Time { return hardwareEnd.Add(31 * 24 * time.Hour) },
		SkipMarker: true,
	}
	connect.AssertEqual(t, RunCloseStep(ctx, deps, &StepRun{Incident: incident, Out: &bytes.Buffer{}}), nil)
	unreadable := incident.RowsOfKind(ListKindCloseLogUnreadable)
	connect.AssertEqual(t, len(unreadable), 1)
	connect.AssertEqual(t, unreadable[0].RowId, networkId.String())
}

// The process's frame holds the close step's function, which `sidecarctl sidecar restore close` runs.
func TestCloseStepIsRegisteredInTheProcesssFrame(t *testing.T) {
	for _, step := range DefaultFrame().Steps() {
		if step.Name == StepClose {
			connect.AssertEqual(t, step.Function != nil, true)
			connect.AssertEqual(t, step.SiteOnly, true)
		}
	}
}
