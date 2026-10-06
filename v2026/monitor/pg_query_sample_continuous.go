package monitor

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The process lock covers collection and terminal state. A precontact
// reservation uses the full40s owner plus the15m floor, so even a crash cannot
// admit an overlapping/early successor. Normal termination records its actual
// clock. Successful and failed attempts consume the same cadence.
type pgSampleCadenceState struct {
	Schema         int       `json:"schema"`
	Mode           string    `json:"mode"`
	AttemptedAt    time.Time `json:"last_attempted_at"`
	TerminalAt     time.Time `json:"last_terminal_at"`
	CompletedAt    time.Time `json:"last_completed_at"`
	NextEligibleAt time.Time `json:"next_eligible_at"`
	Outcome        string    `json:"outcome"`
	ReceiptSHA256  string    `json:"latest_receipt_sha256,omitempty"`
}

func pgSampleReadCadence(dir string, now time.Time) (pgSampleCadenceState, error) {
	var state pgSampleCadenceState
	f, err := os.Open(filepath.Join(dir, "continuous.json"))
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, errors.New("unavailable sample cadence state")
	}
	raw, readErr := io.ReadAll(io.LimitReader(f, 2049))
	closeErr := f.Close()
	if readErr != nil || closeErr != nil || len(raw) > 2048 {
		return state, errors.New("invalid sample cadence state")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if !pgSampleUniqueObject(string(raw)) || d.Decode(&state) != nil || d.Decode(new(any)) != io.EOF || state.Schema != 1 || state.Mode != "continuous" || state.AttemptedAt.IsZero() || state.AttemptedAt.After(now) || state.TerminalAt.After(now) || state.CompletedAt.After(now) || (!state.TerminalAt.IsZero() && state.TerminalAt.Before(state.AttemptedAt)) || (!state.CompletedAt.IsZero() && !state.TerminalAt.IsZero() && state.CompletedAt.After(state.TerminalAt)) {
		return state, errors.New("invalid sample cadence state")
	}
	floor := state.TerminalAt.Add(pgQuerySampleCadence)
	if state.TerminalAt.IsZero() {
		floor = state.AttemptedAt.Add(pgQuerySampleBudget + pgQuerySampleCadence)
	}
	if state.ReceiptSHA256 != "" && (len(state.ReceiptSHA256) != 64 || strings.Trim(state.ReceiptSHA256, "0123456789abcdef") != "") {
		return state, errors.New("invalid sample receipt identity")
	}
	if !state.TerminalAt.IsZero() && state.ReceiptSHA256 == "" {
		return state, errors.New("missing sample receipt identity")
	}
	if !floor.Equal(state.NextEligibleAt) || !pgSampleCadenceOutcome(state.Outcome) || (state.TerminalAt.IsZero() != (state.Outcome == "attempting")) || (state.Outcome == "complete" && !state.CompletedAt.Equal(state.TerminalAt)) {
		return state, errors.New("invalid sample cadence state")
	}
	return state, nil
}
func pgSampleCadenceOutcome(value string) bool {
	switch value {
	case "attempting", "complete", "source-unavailable", "projection-unavailable", "deadline":
		return true
	}
	return false
}
func pgSampleWriteCadence(dir string, state pgSampleCadenceState, syncFile func(*os.File) error) error {
	if syncFile == nil {
		syncFile = func(f *os.File) error { return f.Sync() }
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".cadence-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, writeErr := tmp.Write(append(raw, '\n'))
	syncErr := syncFile(tmp)
	closeErr := tmp.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, "continuous.json")); err != nil {
		return err
	}
	for _, directory := range []string{dir, filepath.Dir(dir)} {
		d, err := os.Open(directory)
		if err != nil {
			return err
		}
		syncErr, closeErr := syncFile(d), d.Close()
		if err := errors.Join(syncErr, closeErr); err != nil {
			return err
		}
	}
	return nil
}
func pgSampleContinuousAdmission(dir string, now time.Time, syncFile func(*os.File) error) (bool, error) {
	state, err := pgSampleReadCadence(dir, now)
	if err != nil {
		return false, err
	}
	if !state.NextEligibleAt.IsZero() && now.Before(state.NextEligibleAt) {
		return false, nil
	}
	state = pgSampleCadenceState{Schema: 1, Mode: "continuous", AttemptedAt: now, CompletedAt: state.CompletedAt, NextEligibleAt: now.Add(pgQuerySampleBudget + pgQuerySampleCadence), Outcome: "attempting", ReceiptSHA256: state.ReceiptSHA256}
	if err := pgSampleWriteCadence(dir, state, syncFile); err != nil {
		return false, err
	}
	return true, nil
}
func pgSampleContinuousFinish(dir string, attempted, finished time.Time, complete bool, reason string, receiptSHA256 string, syncFile func(*os.File) error) error {
	state, err := pgSampleReadCadence(dir, finished)
	if err != nil {
		return err
	}
	if !state.AttemptedAt.Equal(attempted) || !state.TerminalAt.IsZero() || finished.Before(attempted) {
		return errors.New("invalid sample terminal clock")
	}
	state.TerminalAt = finished
	state.NextEligibleAt = finished.Add(pgQuerySampleCadence)
	state.Outcome = reason
	if len(receiptSHA256) != 64 || strings.Trim(receiptSHA256, "0123456789abcdef") != "" {
		return errors.New("invalid sample receipt identity")
	}
	state.ReceiptSHA256 = receiptSHA256
	if complete {
		state.CompletedAt = finished
		state.Outcome = "complete"
	}
	if !pgSampleCadenceOutcome(state.Outcome) {
		return errors.New("invalid sample terminal outcome")
	}
	return pgSampleWriteCadence(dir, state, syncFile)
}

func pgSampleUnavailable(env *probeEnv, reason string) finding {
	mode := "disabled"
	if env.cfg.pgQuerySampleContinuous {
		mode = "continuous"
	} else if !env.cfg.pgQuerySampleUntil.IsZero() {
		mode = "one-shot"
	}
	observed := "reason=" + reason + " mode=" + mode
	if mode == "continuous" {
		if state, err := pgSampleReadCadence(filepath.Join(env.cfg.stateDir, "pg-query-sample"), env.now()); err == nil && !state.AttemptedAt.IsZero() {
			completed := "unknown"
			if !state.CompletedAt.IsZero() {
				completed = state.CompletedAt.Format(time.RFC3339Nano)
			}
			observed += fmt.Sprintf(" last_attempted_at=%s last_completed_at=%s next_eligible_at=%s due_age_s=%.3f", state.AttemptedAt.Format(time.RFC3339Nano), completed, state.NextEligibleAt.Format(time.RFC3339Nano), max(0, env.now().Sub(state.NextEligibleAt).Seconds()))
		}
	}
	return finding{probeId: "pg/query-sample", tier: tierWarn, class: "pg-query-sample-unavailable", target: pgTarget(env), sustain: 1,
		symptom: "PostgreSQL query/load observation is unavailable", observed: observed,
		mechanism: "An expired or incomplete observation cannot establish current slow-query or repeated-work coverage.",
		baseline:  "Continuous mode collects one bounded catalog sample per 15-minute interval; expiry mode is one shot.",
		action:    "Inspect the sampler mode and private receipt; promote a tested recurring configuration through the singleton watcher handoff. Preserve other alerts and cadence floors.",
		verify:    "Fresh complete catalog receipts recur at the configured cadence with source clocks and explicit coverage.", playbook: "SIGNALS.md §2.1a"}
}

// Emit finite family/wait investigations, never query text, PID or a claim that
// sampled activity or execution wall time measures CPU. A one-snapshot burst
// cannot satisfy the repeated-work boundary. Separate low-concurrency slow
// statements from busy families so a large lock queue cannot hide its holder.
func pgSampleFindings(r pgQuerySampleReceipt, target string) []finding {
	findings := []finding{}
	chosen := map[string]pgSampleLoad{}
	for _, load := range r.Load {
		if load.State != "active" || load.Scope != "current" {
			continue
		}
		repeated := load.PressureSamples >= 6
		slow := pgSampleIsSlow(load)
		for class, match := range map[string]bool{"pg-query-repeated-work": repeated, "pg-query-slow": slow} {
			if !match {
				continue
			}
			key := class + "/" + load.Family + "/" + load.Wait
			if previous, ok := chosen[key]; !ok || previous.BackendSamples < load.BackendSamples {
				chosen[key] = load
			}
		}
	}
	// Sorted fixed identities keep receipts and alert order deterministic.
	keys := make([]string, 0, len(chosen))
	for key := range chosen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		load := chosen[key]
		class, _, _ := strings.Cut(key, "/")
		findings = append(findings, finding{probeId: "pg/query-sample", tier: tierWarn, class: class, target: target, frame: load.Family + "/" + load.Wait, sustain: 1,
			symptom:   "Sampled PostgreSQL work warrants a bounded query/holder investigation",
			observed:  fmt.Sprintf("family=%s wait=%s state=%q backend=%q client_owner=%s declared_application=%s backend_samples=%d samples=%d peak=%d samples_ge5=%d age_ge30s_samples=%d age_ge2h_samples=%d max_query_s=%.3f max_xact_s=%.3f sample_requested_at=%s", load.Family, load.Wait, load.State, load.Backend, load.Owner, load.Application, load.BackendSamples, load.SeenSamples, load.Peak, load.PressureSamples, load.SlowSamples, load.MaintenanceSlowSamples, load.QueryAge, load.TransactionAge, r.RequestedAt.Format(time.RFC3339)),
			baseline:  "Repeated: at least 5 backends in 6 of 12 snapshots. Slow: active query age 30s in at least 2 snapshots; concurrent reindex/vacuum uses 2h. These are investigation bands, not service deadlines.",
			mechanism: "Repeated short work and lock convoys can consume capacity without crossing a long-query fence. A slow low-concurrency statement may own the contested resource.",
			evidence:  "Private finite receipt retains all 12 source clocks, count/age views, endpoint-lifetime statistics and bounded blocker links; interval PGSS deltas remain unknown.",
			context:   "The displayed application/client/backend fields describe the selected group, not a native process identity. A declared pg_dump COPY can match an application-table family; ClientWrite is server output waiting, not proof of a contract-close worker or database lock. A group can contain different backends and statement instances across snapshots. Samples are not distinct requests, continuous wait time, query CPU share or a service/customer join. Family may match a truncated prefix. Useful bulk maintenance can explain the load; preserve its owner and successful-work control before tuning.",
			action:    "Resolve the source family and bounded blocker chain; compare exact index/plan, cache reuse/reload and successful demand. Do not infer a payer from a query prefix, cancel financial work or raise concurrency from this sample.",
			verify:    "Fresh samples under successful comparable traffic leave the investigation band, with the independent CPU/wait/capacity and financial correctness signals healthy.", playbook: "SIGNALS.md §2.1a"})
	}
	textTruncated := 0
	for _, n := range r.QueryTextTruncated {
		textTruncated += n
	}
	if r.OmittedGroups > 0 || r.LoadOutputTruncated || r.HistoryTruncated || r.BlockerSelectionTruncated || textTruncated > 0 {
		findings = append(findings, finding{probeId: "pg/query-sample", tier: tierWarn, class: "pg-query-sample-coverage", target: target, sustain: 1,
			symptom:   "Bounded PostgreSQL sample has incomplete query or blocker coverage",
			observed:  fmt.Sprintf("omitted_group_samples=%d load_output_truncated=%t history_truncated=%t blockers_truncated=%t query_text_truncated_samples=%d activity_query_bytes=%d", r.OmittedGroups, r.LoadOutputTruncated, r.HistoryTruncated, r.BlockerSelectionTruncated, textTruncated, r.TrackQuerySize),
			mechanism: "Finite caps or query-text truncation can hide a slow holder or collapse source identity; retained positive observations remain valid lower bounds.",
			baseline:  "Complete source counts and query identities are required to rule out an unobserved family; partial samples can only identify retained work.",
			action:    "Use one separately bounded source/holder discriminator for the missing fact. Keep caps and privacy; do not dump SQL, extend transaction lifetime or treat omitted work as zero.",
			verify:    "A source-qualified bounded observation resolves the specific missing family/holder while preserving the prior positive findings.", playbook: "SIGNALS.md §2.1a"})
	}
	return findings
}

// The first40 preserve backend-sample load. The remaining40 preserve the
// oldest active statements independently, so a peak-one slow holder survives
// a large queue made of many high-count groups. This remains a partial sample.
func pgSampleRetainLoad(load []pgSampleLoad) []pgSampleLoad {
	if len(load) <= 80 {
		return load
	}
	selected := append([]pgSampleLoad(nil), load[:40]...)
	remainder := append([]pgSampleLoad(nil), load[40:]...)
	sort.SliceStable(remainder, func(i, j int) bool {
		a, b := remainder[i], remainder[j]
		if (a.State == "active") != (b.State == "active") {
			return a.State == "active"
		}
		if pgSampleIsSlow(a) != pgSampleIsSlow(b) {
			return pgSampleIsSlow(a)
		}
		if a.QueryAge != b.QueryAge {
			return a.QueryAge > b.QueryAge
		}
		return a.Query < b.Query
	})
	return append(selected, remainder[:40]...)
}

func pgSampleIsSlow(load pgSampleLoad) bool {
	if load.Family == "reindex_concurrent" || load.Family == "vacuum" {
		return load.MaintenanceSlowSamples >= 2
	}
	return load.SlowSamples >= 2
}

// Immutable content-addressed receipts are never overwritten or deleted by the
// sampler. The cadence file is only an index; rotating that index cannot erase
// an incident's evidence. Global retention is an explicit operator policy.
func pgSampleStoreReceipt(dir string, raw []byte) (string, error) {
	if len(raw) > 262144 {
		return "", errors.New("sample receipt exceeds bound")
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(raw))
	target := filepath.Join(dir, "receipt-"+digest+".json")
	tmp, err := os.CreateTemp(dir, ".receipt-")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	_, writeErr := tmp.Write(raw)
	syncErr := tmp.Sync()
	closeErr := tmp.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return "", err
	}
	if err := os.Link(tmp.Name(), target); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
		existing, err := os.Open(target)
		if err != nil {
			return "", err
		}
		old, readErr := io.ReadAll(io.LimitReader(existing, 262145))
		closeErr := existing.Close()
		if readErr != nil || closeErr != nil || !bytes.Equal(old, raw) {
			return "", errors.New("immutable sample receipt conflict")
		}
	}
	d, err := os.Open(dir)
	if err != nil {
		return "", err
	}
	syncErr, closeErr = d.Sync(), d.Close()
	if err := errors.Join(syncErr, closeErr); err != nil {
		return "", err
	}
	return digest, nil
}
