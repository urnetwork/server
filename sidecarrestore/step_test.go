package sidecarrestore

// The frame on its own (OPS-M3-03): §19.10's order holds, a step that ended runs again harmlessly, a site step is
// refused in a Postgres incident, and a step no task registered stops a run until its end is stated by hand.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// A list as the restore script leaves it for incidentId: its head rows, and a row of its own with a tab and a
// backslash in COPY's escaping.
func testIncidentList(t testing.TB, incidentId server.Id) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "incident-"+incidentId.String()+".tsv")
	lines := strings.Join([]string{
		"network-renamed\tnetwork\t" + server.NewId().String() + "\tsynthetic\\tname\\\\x",
		"snapshot-time\tmain dump\t" + incidentId.String() + "\t2026-11-06T01:00:00Z",
		"snapshot-time\tsidecar dump\t" + incidentId.String() + "\t2026-11-06T02:30:00.250000Z",
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The incident at a fresh list, of kind.
func testIncident(t testing.TB, kind IncidentKind) *Incident {
	t.Helper()
	incidentId := server.NewId()
	incident, err := OpenIncident(incidentId, testIncidentList(t, incidentId), nil)
	if err != nil {
		t.Fatal(err)
	}
	if kind != "" {
		if err := incident.SetKind(kind); err != nil {
			t.Fatal(err)
		}
	}
	return incident
}

// A step function that counts its runs.
func testCountingStep(count *int) StepFunction {
	return func(ctx context.Context, run *StepRun) error {
		*count += 1
		return nil
	}
}

func TestIncidentReadsTheScriptsHead(t *testing.T) {
	incident := testIncident(t, "")
	sidecarTime, mainTime, err := incident.SnapshotTimes()
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, mainTime, time.Date(2026, 11, 6, 1, 0, 0, 0, time.UTC))
	connect.AssertEqual(t, sidecarTime.Equal(time.Date(2026, 11, 6, 2, 30, 0, 250_000_000, time.UTC)), true)
	older, err := incident.OlderSnapshotTime()
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, older, mainTime)
	renamed := incident.RowsOfKind("network-renamed")
	connect.AssertEqual(t, len(renamed), 1)
	connect.AssertEqual(t, renamed[0].Detail, "synthetic\tname\\x")
}

func TestIncidentRefusesAListWithoutBothSnapshotTimes(t *testing.T) {
	incidentId := server.NewId()
	path := filepath.Join(t.TempDir(), "incident.tsv")
	if err := os.WriteFile(path, []byte("snapshot-time\tmain dump\t"+incidentId.String()+"\t2026-11-06T01:00:00Z\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := OpenIncident(incidentId, path, nil)
	connect.AssertEqual(t, errors.Is(err, ErrIncidentList), true)

	// another incident's head
	other := testIncidentList(t, server.NewId())
	_, err = OpenIncident(incidentId, other, nil)
	connect.AssertEqual(t, errors.Is(err, ErrIncidentList), true)
}

func TestIncidentAppendsEachRowOnceAndRereads(t *testing.T) {
	incident := testIncident(t, IncidentKindPostgres)
	row := &ListRow{Kind: "acl-head-advanced", Table: "network_acl_head", RowId: server.NewId().String(), Detail: "1 -> 4"}
	connect.AssertEqual(t, incident.Append(row), nil)
	connect.AssertEqual(t, incident.Append(row), nil)
	reread, err := OpenIncident(incident.IncidentId(), incident.ListPath(), nil)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, len(reread.RowsOfKind("acl-head-advanced")), 1)
	kind, ok, err := reread.Kind()
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, kind, IncidentKindPostgres)
	connect.AssertEqual(t, errors.Is(reread.SetKind(IncidentKindSite), ErrIncidentList), true)
}

func TestFrameRefusesAStepBeforeItsPredecessorsEnded(t *testing.T) {
	frame := NewFrame()
	opsCount, apiCount := 0, 0
	frame.Register(StepOps, testCountingStep(&opsCount))
	frame.Register(StepApi, testCountingStep(&apiCount))
	incident := testIncident(t, IncidentKindPostgres)
	out := &bytes.Buffer{}

	err := frame.RunStep(t.Context(), incident, StepApi, out, "")
	connect.AssertEqual(t, errors.Is(err, ErrStepOutOfOrder), true)
	connect.AssertEqual(t, apiCount, 0)

	connect.AssertEqual(t, frame.RunStep(t.Context(), incident, StepOps, out, ""), nil)
	// abuse and dns have no function: api still waits for them
	err = frame.RunStep(t.Context(), incident, StepApi, out, "")
	connect.AssertEqual(t, errors.Is(err, ErrStepOutOfOrder), true)
	connect.AssertEqual(t, errors.Is(frame.EndStepByHand(incident, StepDns, "ran by hand"), ErrStepOutOfOrder), true)
	connect.AssertEqual(t, frame.EndStepByHand(incident, StepAbuse, "no mailbox action after the dump"), nil)
	connect.AssertEqual(t, frame.EndStepByHand(incident, StepDns, "sidecarctl dns restore ended"), nil)
	connect.AssertEqual(t, frame.RunStep(t.Context(), incident, StepApi, out, ""), nil)
	connect.AssertEqual(t, apiCount, 1)
	connect.AssertEqual(t, incident.StepEnded(StepApi), true)
}

func TestFrameRerunsAnEndedStepHarmlessly(t *testing.T) {
	frame := NewFrame()
	opsCount := 0
	frame.Register(StepOps, testCountingStep(&opsCount))
	incident := testIncident(t, IncidentKindPostgres)
	out := &bytes.Buffer{}
	connect.AssertEqual(t, frame.RunStep(t.Context(), incident, StepOps, out, ""), nil)
	connect.AssertEqual(t, frame.RunStep(t.Context(), incident, StepOps, out, ""), nil)
	connect.AssertEqual(t, opsCount, 2)
	connect.AssertEqual(t, len(incident.RowsOfKind(ListKindStepEnded)), 1)
}

func TestFrameRefusesASiteStepInAPostgresIncident(t *testing.T) {
	frame := NewFrame()
	hardwareCount := 0
	frame.Register(StepHardware, testCountingStep(&hardwareCount))
	incident := testIncident(t, IncidentKindPostgres)
	err := frame.RunStep(t.Context(), incident, StepHardware, &bytes.Buffer{}, "")
	connect.AssertEqual(t, errors.Is(err, ErrStepSiteOnly), true)
	connect.AssertEqual(t, errors.Is(frame.EndStepByHand(incident, StepClose, "by hand"), ErrStepSiteOnly), true)
	connect.AssertEqual(t, hardwareCount, 0)

	// in a site incident it waits for every Postgres step first
	site := testIncident(t, IncidentKindSite)
	err = frame.RunStep(t.Context(), site, StepHardware, &bytes.Buffer{}, "")
	connect.AssertEqual(t, errors.Is(err, ErrStepOutOfOrder), true)
}

func TestFrameRunStopsAtAnUnregisteredStep(t *testing.T) {
	frame := NewFrame()
	opsCount, apiCount := 0, 0
	frame.Register(StepOps, testCountingStep(&opsCount))
	frame.Register(StepApi, testCountingStep(&apiCount))
	incident := testIncident(t, IncidentKindPostgres)
	out := &bytes.Buffer{}

	err := frame.Run(t.Context(), incident, out, "")
	connect.AssertEqual(t, errors.Is(err, ErrStepUnregistered), true)
	connect.AssertEqual(t, opsCount, 1)
	connect.AssertEqual(t, apiCount, 0)
	connect.AssertEqual(t, incident.StepEnded(StepAbuse), false)

	connect.AssertEqual(t, frame.EndStepByHand(incident, StepAbuse, "no action after the dump"), nil)
	connect.AssertEqual(t, frame.EndStepByHand(incident, StepDns, "ran sidecarctl dns restore"), nil)
	// a registered step is not ended by hand
	connect.AssertEqual(t, frame.EndStepByHand(incident, StepApi, "by hand") != nil, true)
	err = frame.Run(t.Context(), incident, out, "")
	connect.AssertEqual(t, errors.Is(err, ErrStepUnregistered), true)
	connect.AssertEqual(t, opsCount, 1)
	connect.AssertEqual(t, apiCount, 1)
	connect.AssertEqual(t, strings.Contains(err.Error(), StepLabels), true)

	byHand := 0
	for _, row := range incident.RowsOfKind(ListKindStepEnded) {
		if strings.Contains(row.Detail, " by-hand: ") {
			byHand += 1
		}
	}
	connect.AssertEqual(t, byHand, 2)
}

func TestFrameRefusesAStepWithoutTheIncidentsKind(t *testing.T) {
	frame := NewFrame()
	opsCount := 0
	frame.Register(StepOps, testCountingStep(&opsCount))
	incident := testIncident(t, "")
	err := frame.RunStep(t.Context(), incident, StepOps, &bytes.Buffer{}, "")
	connect.AssertEqual(t, errors.Is(err, ErrIncidentKindUnknown), true)
	connect.AssertEqual(t, opsCount, 0)
}

func TestFrameRegistrationRefusesWhatWouldLeaveAStepUnreachable(t *testing.T) {
	panics := func(callback func()) (panicked bool) {
		defer func() { panicked = recover() != nil }()
		callback()
		return
	}
	frame := NewFrame()
	count := 0
	connect.AssertEqual(t, panics(func() { frame.Register("compact", testCountingStep(&count)) }), true)
	connect.AssertEqual(t, panics(func() { frame.Register(StepApi, nil) }), true)
	frame.Register(StepApi, testCountingStep(&count))
	connect.AssertEqual(t, panics(func() { frame.Register(StepApi, testCountingStep(&count)) }), true)
}
