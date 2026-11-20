package work

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Pure: the sweep's cursor survives the task args' JSON exactly, to the
// microsecond the database keeps, so the next run resumes after the last
// assignment walked, ties broken by the assignment id. Args written before the
// cursor existed, and a run that passed the newest assignment, start at the
// oldest.
func TestSweepProxyExitsArgsCarryTheCursor(t *testing.T) {
	cursor := &model.ProxyExitSweepCursor{
		AssignedAt:   time.Date(2026, 11, 15, 10, 7, 28, 123456000, time.UTC),
		AssignmentId: server.NewId(),
	}
	argsJson, err := json.Marshal(&SweepProxyExitsArgs{Cursor: cursor})
	if err != nil {
		t.Fatal(err)
	}
	var args SweepProxyExitsArgs
	if err := json.Unmarshal(argsJson, &args); err != nil {
		t.Fatal(err)
	}
	if args.Cursor == nil || !args.Cursor.AssignedAt.Equal(cursor.AssignedAt) || args.Cursor.AssignmentId != cursor.AssignmentId {
		t.Fatalf("cursor %+v from %s, want %+v", args.Cursor, argsJson, cursor)
	}

	for _, legacy := range []string{`{}`, `{"cursor":null}`} {
		var args SweepProxyExitsArgs
		if err := json.Unmarshal([]byte(legacy), &args); err != nil {
			t.Fatal(err)
		}
		if args.Cursor != nil {
			t.Fatalf("%s resumes at %+v, want the oldest", legacy, args.Cursor)
		}
	}
	nilJson, err := json.Marshal(&SweepProxyExitsArgs{})
	if err != nil {
		t.Fatal(err)
	}
	if string(nilJson) != `{}` {
		t.Fatalf("a run from the oldest writes %s, want {}", nilJson)
	}
}

// Pure: the run's bound stays 10 minutes, and its walk stops inside it with
// time left for the assignment in flight and the run's cleanup.
func TestSweepProxyExitsWalkStopsInsideItsBound(t *testing.T) {
	settings := model.DefaultUniqueExitSettings()
	if settings.SweepMaxTime != 10*time.Minute {
		t.Fatalf("bound %s, want 10m", settings.SweepMaxTime)
	}
	if settings.SweepMaxTime <= settings.SweepWalkTime || settings.SweepWalkTime <= 0 {
		t.Fatalf("walk time %s, want within the bound %s", settings.SweepWalkTime, settings.SweepMaxTime)
	}
}
