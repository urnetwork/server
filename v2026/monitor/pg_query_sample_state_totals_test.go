// Preserve complete per-snapshot state counts independently of selected groups,
// final blocker state and unavailable source frames. No live source is contacted.
package monitor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Decode the public field without referring to the new production type; these
// controls can also run against the baseline to expose its missing field.
func pgSampleStateTotalsProjection(t testing.TB, receipt pgQuerySampleReceipt) []map[string]int {
	t.Helper()
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal("receipt encoding failed")
	}
	var projection struct {
		States []map[string]int `json:"backend_state_totals"`
	}
	if json.Unmarshal(raw, &projection) != nil {
		t.Fatal("state projection encoding failed")
	}
	return projection.States
}

// The idle population changes each snapshot but never reaches the selected
// rows; the final lock sample is independently zero despite earlier waits.
func TestPgQuerySampleStateTotalsPreserveClocksAndFinalBoundary(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	frames := pgSampleTestFrames(now)
	for _, frame := range frames {
		if frame["kind"] == "activity" {
			i := frame["sample"].(int)
			frame["total"], frame["groups"] = 1009+i, 3
			frame["state_totals"] = []int{5, 1000 + i, 4, 0, 0, 0, 0}
		} else if frame["kind"] == "blockers" {
			frame["total"], frame["rows"] = 0, [][]any{}
		}
	}
	r, err := parsePgQuerySample(pgSampleTestEncode(t, frames), now)
	if err != nil || !r.Complete || r.Samples != 12 || r.LockWaiters != 0 || r.OmittedGroups != 12 {
		t.Fatal("qualified state extension changed the existing sample or final-lock boundary")
	}
	states := pgSampleStateTotalsProjection(t, r)
	if len(states) != len(r.SampleClocks) || len(states) != len(r.BackendTotals) {
		t.Fatal("complete states lost their per-snapshot clock/denominator alignment")
	}
	for i, counts := range states {
		if len(counts) != 7 || counts["active"] != 5 || counts["idle"] != 1000+i || counts["idle_in_transaction"] != 4 {
			t.Fatalf("snapshot %d lost an unselected state population", i)
		}
		sum := 0
		for _, count := range counts {
			sum += count
		}
		if sum != r.BackendTotals[i] || r.SampleClocks[i] != frames[i+2]["at"] {
			t.Fatalf("snapshot %d states use another denominator or clock", i)
		}
	}
	if len(r.Load) != 2 || r.Load[0].State != "active" || r.Load[0].Wait != "Lock:transactionid" {
		t.Fatal("whole-state extension replaced the existing selected positive wait evidence")
	}
}

// Every fixed source slot has a distinct count, so neither an aborted
// transaction nor a present unknown state can be silently remapped.
func TestPgQuerySampleStateTotalsNormalizedStatesRetainTheirMeaning(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	frames := pgSampleTestFrames(now)
	for _, frame := range frames {
		if frame["kind"] == "activity" {
			frame["total"], frame["groups"] = 30, 7
			frame["state_totals"] = []int{5, 7, 4, 2, 3, 1, 8}
		}
	}
	r, err := parsePgQuerySample(pgSampleTestEncode(t, frames), now)
	if err != nil || !r.Complete {
		t.Fatal("fixed normalized states were rejected")
	}
	states := pgSampleStateTotalsProjection(t, r)
	if len(states) != 12 {
		t.Fatal("normalized states lost their snapshot positions")
	}
	want := map[string]int{"active": 5, "idle": 7, "idle_in_transaction": 4,
		"idle_in_transaction_aborted": 2, "fastpath_function_call": 3, "disabled": 1, "unknown": 8}
	for _, counts := range states {
		if len(counts) != len(want) {
			t.Fatal("normalized state dimensions changed")
		}
		for state, count := range want {
			if counts[state] != count {
				t.Fatalf("normalized state %s changed meaning", state)
			}
		}
	}
}

// Missing legacy fields, or a missing single snapshot, remain null rather than
// manufactured zero counts or a fabricated unknown-state population.
func TestPgQuerySampleStateTotalsMissingSourceRemainsUnknown(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, mode := range []string{"legacy", "null", "mixed"} {
		frames := pgSampleTestFrames(now)
		for _, frame := range frames {
			if frame["kind"] != "activity" {
				continue
			}
			if mode == "null" {
				frame["state_totals"] = nil
			} else if mode == "mixed" && frame["sample"] != 4 {
				frame["state_totals"] = []int{5, 0, 4, 0, 0, 0, 0}
			}
		}
		r, err := parsePgQuerySample(pgSampleTestEncode(t, frames), now)
		if err != nil || !r.Complete {
			t.Fatalf("%s source lost its existing qualified sample", mode)
		}
		states := pgSampleStateTotalsProjection(t, r)
		if len(states) != 12 {
			t.Fatalf("%s missing-source positions are not explicit", mode)
		}
		for i, counts := range states {
			if wantMissing := mode != "mixed" || i == 4; (counts == nil) != wantMissing {
				t.Fatalf("%s snapshot %d manufactured or lost state coverage", mode, i)
			}
		}
	}
}

// Fixed length, integer counts, exact denominator and selected-state lower
// bounds prevent malformed source data from becoming plausible state totals.
func TestPgQuerySampleStateTotalsMalformedSourceFailsClosed(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, invalid := range []any{
		[]any{}, []any{5, 0, 4, 0, 0, 0}, []any{5, 0, 4, 0, 0, 0, 0, 0},
		[]any{-1, 0, 4, 0, 0, 0, 0}, []any{nil, 0, 4, 0, 0, 0, 0},
		[]any{5.5, 0, 3.5, 0, 0, 0, 0}, []any{1000001, 0, 4, 0, 0, 0, 0},
		[]any{5, 0, 3, 0, 0, 0, 0}, []any{4, 1, 4, 0, 0, 0, 0},
		[]any{"synthetic-private-state", 0, 4, 0, 0, 0, 0}, map[string]int{"active": 9},
	} {
		frames := pgSampleTestFrames(now)
		frames[2]["state_totals"] = []int{5, 0, 4, 0, 0, 0, 0}
		frames[3]["state_totals"] = invalid
		r, err := parsePgQuerySample(pgSampleTestEncode(t, frames), now)
		if err == nil || r.Complete || pgSampleStateTotalsProjection(t, r) != nil {
			t.Fatal("malformed source retained a qualified or partial whole-state projection")
		}
		if strings.Contains(err.Error(), "synthetic-private-state") {
			t.Fatal("malformed source leaked its value")
		}
	}
	frames := pgSampleTestFrames(now)
	frames[0]["state_totals"] = []int{0, 0, 0, 0, 0, 0, 0}
	if _, err := parsePgQuerySample(pgSampleTestEncode(t, frames), now); err == nil {
		t.Fatal("non-activity frame supplied activity state totals")
	}
}

// An incomplete sequence cannot advertise its earlier state arrays as a
// complete interval, even when those individual source frames were valid.
func TestPgQuerySampleStateTotalsPartialSequenceIsUnavailable(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	frames := pgSampleTestFrames(now)
	for _, frame := range frames {
		if frame["kind"] == "activity" {
			frame["state_totals"] = []int{5, 0, 4, 0, 0, 0, 0}
		}
	}
	r, err := parsePgQuerySample(pgSampleTestEncode(t, frames[:len(frames)-1]), now)
	if err == nil || r.Complete || pgSampleStateTotalsProjection(t, r) != nil {
		t.Fatal("partial sequence became a whole-window state measurement")
	}
}

// Use the existing stubbed transport, so source failure/partial projection
// persists unknown totals and consumes one attempt without contacting a host.
func TestPgQuerySampleStateTotalsUnavailableProbePersistsUnknown(t *testing.T) {
	for _, sourceFailure := range []bool{false, true} {
		now := time.Now().UTC().Truncate(time.Second)
		frames := pgSampleTestFrames(now)
		for _, frame := range frames {
			if frame["kind"] == "activity" {
				frame["state_totals"] = []int{5, 0, 4, 0, 0, 0, 0}
			}
		}
		calls := 0
		env := pgSampleTestEnv(t, func([]string, string) (string, string, error) {
			calls++
			if sourceFailure {
				return "", "", context.DeadlineExceeded
			}
			return pgSampleTestEncode(t, frames[:len(frames)-1]), "", nil
		})
		probe := pgQuerySampleProbe{}
		if _, err := probe.check(t.Context(), env); err != nil || calls != 1 {
			t.Fatal("unavailable source changed the one-attempt contract")
		}
		dir := filepath.Join(env.cfg.stateDir, "pg-query-sample")
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal("unavailable receipt was not persisted")
		}
		found := false
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), "receipt-") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			var receipt pgQuerySampleReceipt
			if err != nil || json.Unmarshal(raw, &receipt) != nil || receipt.Complete || receipt.Samples != 0 || pgSampleStateTotalsProjection(t, receipt) != nil {
				t.Fatal("unavailable receipt manufactured a state measurement")
			}
			found = true
		}
		if !found {
			t.Fatal("missing terminal unavailable receipt")
		}
	}
}

// Add the new public counts to a saturated private SQL receipt without raising
// either existing byte budget or allowing private SQL into the public field.
func TestPgQuerySampleStateTotalsPreservesCombinedReceiptBound(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	frames := pgSampleSqlTestFrames(now)
	for _, frame := range frames {
		if frame["kind"] != "activity" {
			continue
		}
		rows := [][]any{}
		for i := range 16 {
			row := append([]any(nil), frame["rows"].([][]any)[0]...)
			row[0], row[8] = fmt.Sprint(i+1), 1
			text := fmt.Sprintf("/* synthetic state sample %v row %d */", frame["sample"], i) + strings.Repeat("x", 4096)
			row[12] = []any{len(text), base64.StdEncoding.EncodeToString([]byte(text)[:4096]), false}
			rows = append(rows, row)
		}
		frame["rows"], frame["total"], frame["groups"] = rows, 16, 16
		frame["query_text_truncated"] = 16
		frame["state_totals"] = []int{16, 0, 0, 0, 0, 0, 0}
	}
	r, err := parsePgQuerySample(pgSampleTestEncode(t, frames), now)
	if err != nil {
		t.Fatal("bounded state/private fixture was rejected")
	}
	public, private, err := pgSampleEncodeReceipts(&r)
	if err != nil || len(public)+len(private) > pgSampleReceiptBytes || r.PrivateSql == nil || r.PrivateSql.OmittedStatements == 0 {
		t.Fatal("state extension raised or lost the combined retained-evidence bound")
	}
	var persisted pgQuerySampleReceipt
	if json.Unmarshal(public, &persisted) != nil || len(pgSampleStateTotalsProjection(t, persisted)) != 12 {
		t.Fatal("bounded persistence dropped the complete state arrays")
	}
	if bytes.Contains(public, []byte("synthetic state sample")) {
		t.Fatal("private SQL entered the public state projection")
	}
}
