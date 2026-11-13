// Fixed diagnostic projection for the fixture reader, never client arguments
// or source payloads. It does not turn reader exit into psql/server authority.
package monitor

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Only fixed reader and envelope authority; raw payloads and errors stay private.
type pgTailNativeFixedDiagnostic struct {
	ReaderError    bool
	ReaderExitCode int
	SourceFailure  bool
	SourcePhase    string
	SourceCause    string
}

// The existing strict envelope parser owns the explicit phase/cause allowlist.
// Missing/malformed envelopes and nil process states remain unknown.
func pgTailNativeFixedFailureDiagnostic(raw string, readerErr error) pgTailNativeFixedDiagnostic {
	result := pgTailNativeFixedDiagnostic{ReaderError: readerErr != nil, ReaderExitCode: -1, SourcePhase: "unknown", SourceCause: "unknown"}
	if readerErr == nil {
		result.ReaderExitCode = 0
	} else if exit, ok := readerErr.(*exec.ExitError); ok && exit != nil && exit.ProcessState != nil {
		result.ReaderExitCode = exit.ExitCode()
	}
	if failure, valid := parsePgSampleSourceFailure(raw); valid && failure != nil {
		result.SourceFailure, result.SourcePhase, result.SourceCause = true, failure.Phase, failure.Cause
	}
	return result
}

// Safety and privacy hold for absent/malformed source, errors and nil native
// state; none of these controls starts a process or reads a fixture.
func TestPgTailNativeDiagnosticSafetyAndPrivacy(t *testing.T) {
	const private = "synthetic-private-error-marker"
	var nilExit *exec.ExitError
	for _, item := range []struct {
		raw         string
		err         error
		wantError   bool
		wantCode    int
		wantFailure bool
		wantPhase   string
		wantCause   string
	}{
		{raw: "", err: nil, wantError: false, wantCode: 0, wantFailure: false, wantPhase: "unknown", wantCause: "unknown"},
		{raw: "{}", err: errors.New(private), wantError: true, wantCode: -1, wantFailure: false, wantPhase: "unknown", wantCause: "unknown"},
		{raw: "", err: nilExit, wantError: true, wantCode: -1, wantFailure: false, wantPhase: "unknown", wantCause: "unknown"},
		{raw: "", err: &exec.ExitError{}, wantError: true, wantCode: -1, wantFailure: false, wantPhase: "unknown", wantCause: "unknown"},
		{raw: `{"kind":"source_failure","schema":1,"phase":"child_process","cause":"owner_deadline","stderr_truncated":false}`, err: errors.New(private), wantError: true, wantCode: -1, wantFailure: true, wantPhase: "child_process", wantCause: "owner_deadline"},
		{raw: `{"kind":"source_failure","schema":1,"phase":"synthetic-private-error-marker","cause":"owner_deadline","stderr_truncated":false}`, err: errors.New(private), wantError: true, wantCode: -1, wantFailure: false, wantPhase: "unknown", wantCause: "unknown"},
		{raw: `{"kind":"source_failure","schema":1,"phase":"child_process","cause":"synthetic-private-error-marker","stderr_truncated":false}`, err: errors.New(private), wantError: true, wantCode: -1, wantFailure: false, wantPhase: "unknown", wantCause: "unknown"},
	} {
		got := pgTailNativeFixedFailureDiagnostic(item.raw, item.err)
		if got.ReaderError != item.wantError || got.ReaderExitCode != item.wantCode || got.SourceFailure != item.wantFailure || got.SourcePhase != item.wantPhase || got.SourceCause != item.wantCause {
			t.Fatal("fixed diagnostic lost nil/error/envelope authority")
		}
		if strings.Contains(fmt.Sprintf("%+v", got), private) {
			t.Fatal("fixed diagnostic retained private error or source text")
		}
	}
}

// Admission facts have fixed keys and booleans only. Missing checks were not
// reached; they are unknown, never implicitly healthy. No wire values survive.
type pgTailNativeAdmissionDiagnostic struct {
	ParseReason string          `json:"parse_reason"`
	Stage       string          `json:"stage"`
	Checks      map[string]bool `json:"checks"`
}

// Replays only the exact parser admission region preceding the first Samples
// increment. Later row/body validation remains unknown, not diagnosed here.
func pgTailNativeAdmission(raw string, now time.Time, parseErr error) pgTailNativeAdmissionDiagnostic {
	result := pgTailNativeAdmissionDiagnostic{ParseReason: "unknown", Stage: "framing", Checks: map[string]bool{}}
	if parseErr == nil {
		result.ParseReason = "none"
	} else if reflect.TypeOf(parseErr) == reflect.TypeOf(errors.New("")) && !reflect.ValueOf(parseErr).IsNil() && parseErr.Error() == "monitor: invalid bounded PostgreSQL sample" {
		result.ParseReason = "invalid_bounded_sample"
	}
	check := func(stage string, checks map[string]bool) bool {
		result.Stage = stage
		ok := true
		for key, value := range checks {
			result.Checks[stage+"_"+key] = value
			ok = ok && value
		}
		return ok
	}
	if !check("framing", map[string]bool{"bytes": len(raw) <= pgQuerySampleMaxBytes, "newline": strings.HasSuffix(raw, "\n")}) {
		return result
	}
	seenIdentity := false
	seenHistory := [2]bool{}
	lastClock := float64(0)
	for ordinal, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if ordinal >= 64 {
			result.Stage = "diagnostic_limit"
			return result
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		var w pgSampleWire
		decoder := json.NewDecoder(strings.NewReader(line))
		decoder.DisallowUnknownFields()
		if !check("wire", map[string]bool{"unique": pgSampleUniqueObject(line), "decode": decoder.Decode(&w) == nil}) {
			return result
		}
		if !check("wire", map[string]bool{"clock_lower": w.At >= float64(now.Add(-time.Minute).Unix()), "clock_upper": w.At <= float64(now.Add(pgQuerySampleBudget).Unix()), "clock_order": w.At >= lastClock, "total_lower": w.Total >= 0, "total_upper": w.Total <= 1000000, "capture_scope": w.SqlCaptureVersion == nil || w.Kind == "activity", "states_scope": w.StateTotals == nil || w.Kind == "activity"}) {
			return result
		}
		lastClock = w.At
		switch w.Kind {
		case "identity":
			if !check("identity", map[string]bool{"track_present": w.TrackQuerySize != nil, "track_range": w.TrackQuerySize != nil && *w.TrackQuerySize >= 1024 && *w.TrackQuerySize <= 1048576, "pgss_version": regexp.MustCompile(`^[0-9]+(?:\.[0-9]+){1,2}$`).MatchString(w.PGSSVersion), "unique": !seenIdentity, "primary": w.Primary, "read_only": w.ReadOnly, "database_ok": w.DatabaseOK}) {
				return result
			}
			seenIdentity = true
		case "history":
			if !check("history", map[string]bool{"identity": seenIdentity, "sample_range": w.Sample >= 0 && w.Sample <= 1, "unique": w.Sample >= 0 && w.Sample <= 1 && !seenHistory[w.Sample], "rows_cap": len(w.Rows) <= 5000, "rows_total": len(w.Rows) <= w.Total}) {
				return result
			}
			seenHistory[w.Sample] = true
			seen := map[string]bool{}
			for _, row := range w.Rows {
				if !check("history_row", map[string]bool{"width": len(row) == 5}) {
					return result
				}
				query, queryErr := pgSampleText(row[0])
				calls, callsErr := pgSampleNumber(row[1])
				_, execErr := pgSampleNumber(row[2])
				_, maxErr := pgSampleNumber(row[3])
				family, familyErr := pgSampleText(row[4])
				if !check("history_row", map[string]bool{"query_type": queryErr == nil, "query_shape": pgSampleID.MatchString(query), "query_present": query != "none", "calls_number": callsErr == nil, "calls_integer": math.Trunc(calls) == calls, "exec_number": execErr == nil, "max_number": maxErr == nil, "family_type": familyErr == nil, "family_allowed": pgSampleAllowedValue("family", family), "query_unique": !seen[query]}) {
					return result
				}
				seen[query] = true
			}
		case "activity":
			if !check("activity", map[string]bool{"capture_version": w.SqlCaptureVersion == nil || *w.SqlCaptureVersion == 1, "truncation_present": w.QueryTextTruncated != nil, "truncation_range": w.QueryTextTruncated != nil && *w.QueryTextTruncated >= 0 && *w.QueryTextTruncated <= w.Total, "history_start": seenHistory[0], "history_end_absent": !seenHistory[1], "sample_zero": w.Sample == 0, "sample_upper": w.Sample < pgQuerySampleCount, "rows_cap": len(w.Rows) <= 128, "groups_rows": w.Groups >= len(w.Rows), "groups_total": w.Groups <= w.Total, "states_width": w.StateTotals == nil || len(w.StateTotals) == 7}) {
				return result
			}
			sum := float64(0)
			for _, state := range w.StateTotals {
				count, err := pgSampleNumber(state)
				if !check("activity_state", map[string]bool{"number": err == nil, "integer": math.Trunc(count) == count, "total_bound": count <= float64(w.Total)}) {
					return result
				}
				sum += count
			}
			if !check("activity", map[string]bool{"states_sum": w.StateTotals == nil || sum == float64(w.Total)}) {
				return result
			}
			result.Stage = "first_activity_admitted"
			return result
		default:
			result.Stage = "other_kind_before_activity"
			return result
		}
	}
	result.Stage = "no_activity"
	return result
}

// Foreign error methods are never invoked by the fixed parser classifier.
type pgTailNativeDiagnosticForeignError struct{}

// A panic makes accidental foreign Error invocation deterministic in controls.
func (*pgTailNativeDiagnosticForeignError) Error() string {
	panic("foreign diagnostic error method must not be invoked")
}

// Synthetic cases cover healthy admission, each first-stage refusal, and
// unknown errors without projecting source strings or changing the parser.
func TestPgTailNativeAdmissionDiagnosticSafety(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, item := range []struct {
		name   string
		stage  string
		mutate func([]map[string]any)
	}{
		{name: "healthy", stage: "first_activity_admitted", mutate: func(_ []map[string]any) {}},
		{name: "track_missing", stage: "identity", mutate: func(f []map[string]any) { delete(f[0], "track_activity_query_size") }},
		{name: "track_range", stage: "identity", mutate: func(f []map[string]any) { f[0]["track_activity_query_size"] = 128 }},
		{name: "version", stage: "identity", mutate: func(f []map[string]any) { f[0]["pgss_version"] = "synthetic-private-marker" }},
		{name: "primary", stage: "identity", mutate: func(f []map[string]any) { f[0]["primary"] = false }},
		{name: "read_only", stage: "identity", mutate: func(f []map[string]any) { f[0]["read_only"] = false }},
		{name: "database", stage: "identity", mutate: func(f []map[string]any) { f[0]["database_ok"] = false }},
		{name: "history", stage: "history", mutate: func(f []map[string]any) { f[1]["sample"] = -1 }},
		{name: "history_row", stage: "history_row", mutate: func(f []map[string]any) { f[1]["rows"] = [][]any{{"synthetic-private-marker", 1, 1, 1, "other"}} }},
		{name: "capture", stage: "activity", mutate: func(f []map[string]any) { f[2]["sql_capture_version"] = 2 }},
		{name: "truncation", stage: "activity", mutate: func(f []map[string]any) { delete(f[2], "query_text_truncated") }},
		{name: "groups", stage: "activity", mutate: func(f []map[string]any) { f[2]["groups"] = 0 }},
		{name: "state_width", stage: "activity", mutate: func(f []map[string]any) { f[2]["state_totals"] = []any{1} }},
		{name: "state_number", stage: "activity_state", mutate: func(f []map[string]any) { f[2]["state_totals"] = []any{-1, 0, 0, 0, 0, 0, 0} }},
		{name: "state_sum", stage: "activity", mutate: func(f []map[string]any) { f[2]["state_totals"] = []any{0, 0, 0, 0, 0, 0, 0} }},
	} {
		frames := pgSampleTestFrames(now)
		item.mutate(frames)
		raw := pgSampleTestEncode(t, frames)
		_, parseErr := parsePgQuerySample(raw, now)
		got := pgTailNativeAdmission(raw, now, parseErr)
		if got.Stage != item.stage || (item.name == "healthy") != (parseErr == nil) {
			t.Fatal("synthetic admission diagnostic disagreed with parser boundary")
		}
		encoded, err := json.Marshal(got)
		if err != nil || strings.Contains(string(encoded), "synthetic-private-marker") || strings.Contains(string(encoded), "714727414314") {
			t.Fatal("admission diagnostic disclosed source")
		}
	}
	var nilExit *exec.ExitError
	nilParserError := reflect.Zero(reflect.TypeOf(errors.New(""))).Interface().(error)
	for _, item := range []struct {
		raw    string
		err    error
		reason string
		stage  string
	}{
		{raw: "{}\n", err: errors.New("synthetic-private-error"), reason: "unknown", stage: "wire"},
		{raw: "{}\n", err: nilExit, reason: "unknown", stage: "wire"},
		{raw: "{}\n", err: &exec.ExitError{}, reason: "unknown", stage: "wire"},
		{raw: "{}\n", err: nilParserError, reason: "unknown", stage: "wire"},
		{raw: "{}\n", err: &pgTailNativeDiagnosticForeignError{}, reason: "unknown", stage: "wire"},
		{raw: "", err: nil, reason: "none", stage: "framing"},
		{raw: "{broken}\n", err: errors.New("monitor: invalid bounded PostgreSQL sample"), reason: "invalid_bounded_sample", stage: "wire"},
	} {
		got := pgTailNativeAdmission(item.raw, now, item.err)
		if got.ParseReason != item.reason || got.Stage != item.stage {
			t.Fatal("unknown or malformed diagnostic acquired false authority")
		}
	}
}
