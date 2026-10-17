package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var testCanaryBaselineNow = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

// A config tree with two releases: the older one's database at
// testPreviousOldEpoch, and the selected latest at testPreviousNewEpoch with
// the older one copied beside it.
func testCanaryBaselineConfig(t *testing.T) (root string, olderPath string) {
	t.Helper()
	root = t.TempDir()
	older := testPreviousArinDatabase(t, "urnetwork arindb", testPreviousOldEpoch, "subscriber")
	olderPath = writeTestPreviousRelease(t, filepath.Join(root, "all", "arindb", "1.0.0"), older, testPreviousArinManifest(t, older, time.Unix(testPreviousOldEpoch, 0)))
	latest := testPreviousArinDatabase(t, "urnetwork arindb", testPreviousNewEpoch, "unknown")
	latestDir := filepath.Join(root, "all", "arindb", "1.1.0")
	writeTestPreviousRelease(t, latestDir, latest, testPreviousArinManifest(t, latest, time.Unix(testPreviousNewEpoch, 0)))
	if err := os.WriteFile(filepath.Join(latestDir, "arin-old.mmdb"), older, 0o644); err != nil {
		t.Fatal(err)
	}
	return root, olderPath
}

func writeTestCanaryDecision(t *testing.T, document map[string]any) string {
	t.Helper()
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "decision.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func testCanaryDecisionDocument(state string, servingEpoch int64, basis string, decidedAt time.Time) map[string]any {
	return map[string]any{
		"version": 1, "decision_id": "synthetic", "state": state, "serving_epoch": servingEpoch, "serving_basis": basis,
		"latest":     map[string]any{"state": "loaded", "build_epoch": testPreviousNewEpoch},
		"old":        map[string]any{"state": "loaded", "build_epoch": testPreviousOldEpoch},
		"decided_at": decidedAt, "source_started_at": decidedAt.Add(-time.Minute),
	}
}

// The copy follows the database the published decision serves: the selected
// latest copies as before; a fallback pins the database it serves, so the
// rejected latest never becomes the next release's baseline; each row's latest
// slot copies the selection.
func TestCanaryBaselineFollowsTheServedDatabase(t *testing.T) {
	root, olderPath := testCanaryBaselineConfig(t)
	for _, c := range []struct {
		name     string
		document map[string]any
		want     string
	}{
		{name: "the latest passes", document: testCanaryDecisionDocument("latest", testPreviousNewEpoch, "passed", testCanaryBaselineNow), want: "copy"},
		{name: "a fallback", document: testCanaryDecisionDocument("fallback", testPreviousOldEpoch, "tripped", testCanaryBaselineNow), want: "pin " + olderPath},
		{name: "a latched fallback", document: testCanaryDecisionDocument("fallback", testPreviousOldEpoch, "latched", testCanaryBaselineNow.Add(-time.Hour)), want: "pin " + olderPath},
		{name: "warming holding the latest out", document: testCanaryDecisionDocument("warming", testPreviousOldEpoch, "latest_blocked", testCanaryBaselineNow), want: "pin " + olderPath},
		{name: "each row's latest slot", document: testCanaryDecisionDocument("warming", 0, "uncovered", testCanaryBaselineNow), want: "copy"},
		{name: "single", document: testCanaryDecisionDocument("single", 0, "single", testCanaryBaselineNow), want: "copy"},
	} {
		if got := canaryBaseline(writeTestCanaryDecision(t, c.document), root, "main", canaryBaselineMaxAge, testCanaryBaselineNow); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// A decision the copy cannot follow pauses the refresh rather than copying an
// unchecked baseline: unreadable, another version, too old, or serving a
// database no release directory holds.
func TestCanaryBaselinePausesOnADecisionItCannotFollow(t *testing.T) {
	root, _ := testCanaryBaselineConfig(t)
	another := testCanaryDecisionDocument("latest", testPreviousNewEpoch, "passed", testCanaryBaselineNow)
	another["version"] = 2
	malformed := filepath.Join(t.TempDir(), "decision.json")
	if err := os.WriteFile(malformed, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		path string
		want string
	}{
		{name: "missing", path: filepath.Join(t.TempDir(), "absent.json"), want: "pause the canary decision is unreadable"},
		{name: "malformed", path: malformed, want: "pause the canary decision is unreadable"},
		{name: "another version", path: writeTestCanaryDecision(t, another), want: "pause the canary decision is version 2"},
		{name: "too old", path: writeTestCanaryDecision(t, testCanaryDecisionDocument("latest", testPreviousNewEpoch, "passed", testCanaryBaselineNow.Add(-7*time.Hour))),
			want: "pause the canary decision of"},
		{name: "a served database the tree lacks", path: writeTestCanaryDecision(t, testCanaryDecisionDocument("fallback", testPreviousOldEpoch-86400, "tripped", testCanaryBaselineNow)),
			want: "pause the canary protects the database built at"},
		{name: "a dropped fallback it does not name", path: writeTestCanaryDecision(t, func() map[string]any {
			document := testCanaryDecisionDocument("single", 0, "single", testCanaryBaselineNow)
			document["fallback_dropped"] = true
			return document
		}()), want: "pause the canary dropped a fallback"},
	} {
		if got := canaryBaseline(c.path, root, "main", canaryBaselineMaxAge, testCanaryBaselineNow); !strings.HasPrefix(got, c.want) {
			t.Errorf("%s: %q, want %q...", c.name, got, c.want)
		}
	}
}

// A latched fallback protects its old database whatever the decision serves:
// warming serving each row's latest slot while the latch is held, or a
// release whose old database is the held-out latest, still pins the latch's
// old database rather than copying the rejected latest forward.
func TestCanaryBaselineFollowsALatchTheDecisionNoLongerServes(t *testing.T) {
	root, olderPath := testCanaryBaselineConfig(t)
	latch := map[string]any{"latest_epoch": testPreviousNewEpoch, "old_epoch": testPreviousOldEpoch}
	held := testCanaryDecisionDocument("warming", 0, "uncovered", testCanaryBaselineNow)
	held["latch"], held["fallback_dropped"] = latch, true
	dropped := testCanaryDecisionDocument("latest", testPreviousNewEpoch, "passed", testCanaryBaselineNow)
	dropped["dropped_latch"], dropped["fallback_dropped"] = latch, true
	// warming whose paired sample trips blocks the latest with no latch, and may
	// serve each row's latest slot when the old covers too little
	tripped := testCanaryDecisionDocument("warming", 0, "uncovered", testCanaryBaselineNow)
	tripped["warming_tripped"] = true
	// a latch held downstream of a dropped one: the dropped latch's old
	// database is the one before the rejected latest
	both := testCanaryDecisionDocument("fallback", testPreviousNewEpoch, "tripped", testCanaryBaselineNow)
	both["latch"] = map[string]any{"latest_epoch": testPreviousNewEpoch + 86400, "old_epoch": testPreviousNewEpoch}
	both["dropped_latch"], both["fallback_dropped"] = latch, true
	for name, document := range map[string]map[string]any{
		"a held latch on each row's latest slot": held, "a dropped latch": dropped,
		"a tripped warming sample": tripped, "a latch held after a dropped one": both,
	} {
		if got := canaryBaseline(writeTestCanaryDecision(t, document), root, "main", canaryBaselineMaxAge, testCanaryBaselineNow); got != "pin "+olderPath {
			t.Errorf("%s: %q, want the latch's old database %q", name, got, olderPath)
		}
	}
}

// The command prints the one line and refuses incomplete flags.
func TestCanaryBaselineCommandPrintsOneLine(t *testing.T) {
	root, olderPath := testCanaryBaselineConfig(t)
	decision := writeTestCanaryDecision(t, testCanaryDecisionDocument("fallback", testPreviousOldEpoch, "tripped", time.Now().UTC()))
	var output bytes.Buffer
	if err := runCommand(t.Context(), []string{"canary-baseline", "--decision", decision, "--previous-config", root, "--previous-env", "main"}, &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "pin "+olderPath+"\n" {
		t.Fatalf("output %q", output.String())
	}
	for _, args := range [][]string{
		{"canary-baseline", "--previous-config", root, "--previous-env", "main"},
		{"canary-baseline", "--decision", decision, "--previous-env", "main"},
		{"canary-baseline", "--decision", decision, "--previous-config", root},
		{"canary-baseline", "--decision", decision, "--previous-config", root, "--previous-env", "main", "extra"},
	} {
		if err := runCommand(t.Context(), args, &bytes.Buffer{}); err == nil {
			t.Errorf("%v was accepted", args)
		}
	}
}
