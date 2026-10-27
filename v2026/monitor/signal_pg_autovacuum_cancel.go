// Observe bounded structured autovacuum cancellation records without inferring
// discarded work, maintenance intent, relation size or a harmful vacuum reset.
package monitor

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	pgAutovacuumCancelMaxBytes  = 40000000
	pgAutovacuumCancelMaxRows   = 100000
	pgAutovacuumCancelMaxOutput = 8 * 1024 * 1024
	pgAutovacuumCancelWindow    = 15 * time.Minute
	pgAutovacuumCancelTimeout   = 25 * time.Second
)

// The fixed program validates PG18 JSON primary fields and projects only
// cancellation timestamps plus bounded source framing. Version2 rejects
// all prior plaintext-derived frames; there is no plaintext fallback.
// SQL, process IDs, database/user names and log/error text stay on the host.
//
//go:embed pg_autovacuum_cancel_program.py
var pgAutovacuumCancelProgram string

// SIGNALS.md §2.4e observes records at WARN1 every fifteen minutes; harm remains unknown.
func NewPgAutovacuumCancelSignal() Signal {
	return &signalAdapter{number: "2.4e", key: "pg-autovacuum-cancel", name: "Observed PostgreSQL autovacuum cancellations", probe: pgAutovacuumCancelProbe{}}
}

// Each run owns one source snapshot; no cursor or incident state is persisted.
type pgAutovacuumCancelProbe struct{}

// Keep the owned probe identity distinct from debt and freeze-age probes.
func (pgAutovacuumCancelProbe) id() string { return "pg/autovacuum-cancel" }

// Recorded cancellations are observations with warning severity.
func (pgAutovacuumCancelProbe) tier() string { return tierWarn }

// The bounded observation window matches the scheduled cadence.
func (pgAutovacuumCancelProbe) cadence() time.Duration { return pgAutovacuumCancelWindow }

// Include transport admission and reduction in a finite signal lifetime.
func (pgAutovacuumCancelProbe) runBudget() time.Duration { return 30 * time.Second }

// Authenticate the inventory host before the fixed reader opens any log.
func pgAutovacuumCancelCommand(name string) string {
	shortName, _, _ := strings.Cut(name, ".")
	return "[ \"$(hostname -s)\" = " + shellSingleQuote(shortName) + " ] || exit 74; exec timeout -k 2s 20s python3 -c " + shellSingleQuote(pgAutovacuumCancelProgram)
}

// All numbers are decimal strings, so null, fractional and overflowed source
// fields cannot silently become valid zero-valued metadata.
type pgAutovacuumCancelWire struct {
	Version    string     `json:"version"`
	Status     string     `json:"status"`
	Host       string     `json:"host"`
	Clock      []string   `json:"clock"`
	Boot       []string   `json:"boot"`
	FileBefore []string   `json:"file_before"`
	FileAfter  []string   `json:"file_after"`
	Read       []string   `json:"read"`
	Span       []string   `json:"span"`
	Flags      []string   `json:"flags"`
	Rows       [][]string `json:"rows"`
}

// The only public measurement is a lower bound on observed cancellation
// records, accompanied by fixed coverage reasons rather than raw source text.
type pgAutovacuumCancelObservation struct {
	records int
	reasons []string
}

// Bounded canonical nonnegative decimal fields reject aliases and overflow.
func pgAutovacuumCancelNumbers(values []string, count int) ([]int64, bool) {
	if len(values) != count {
		return nil, false
	}
	numbers := make([]int64, count)
	for i, value := range values {
		if value == "" || len(value) > 19 || (len(value) > 1 && value[0] == '0') {
			return nil, false
		}
		for _, c := range value {
			if c < '0' || c > '9' {
				return nil, false
			}
		}
		number, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return nil, false
		}
		numbers[i] = number
	}
	return numbers, true
}

var pgAutovacuumCancelBootPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Frame identity and source clocks bind each observed record. Incomplete
// history cannot prove absence, but separate valid records remain positives.
func parsePgAutovacuumCancelSnapshot(output, expectedHost string, now time.Time) (pgAutovacuumCancelObservation, string) {
	observation := pgAutovacuumCancelObservation{}
	if len(output) > pgAutovacuumCancelMaxOutput || !strings.HasSuffix(output, "\n") || !pgSampleUniqueObject(output) {
		return observation, "invalid-source-frame"
	}
	var wire pgAutovacuumCancelWire
	decoder := json.NewDecoder(strings.NewReader(output))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&wire) != nil || wire.Version != "2" || wire.Status != "snapshot" {
		return observation, "source-unavailable-or-schema"
	}
	shortName, _, _ := strings.Cut(expectedHost, ".")
	if shortName == "" || wire.Host != shortName {
		return observation, "source-host-mismatch"
	}
	clock, clockOk := pgAutovacuumCancelNumbers(wire.Clock, 5)
	before, beforeOk := pgAutovacuumCancelNumbers(wire.FileBefore, 5)
	after, afterOk := pgAutovacuumCancelNumbers(wire.FileAfter, 5)
	read, readOk := pgAutovacuumCancelNumbers(wire.Read, 2)
	span, spanOk := pgAutovacuumCancelNumbers(wire.Span, 2)
	if !clockOk || !beforeOk || !afterOk || !readOk || !spanOk || len(wire.Boot) != 2 ||
		wire.Flags == nil || wire.Rows == nil || len(wire.Rows) > pgAutovacuumCancelMaxRows {
		return observation, "incomplete-source-frame"
	}
	if !pgAutovacuumCancelBootPattern.MatchString(wire.Boot[0]) || wire.Boot[0] != wire.Boot[1] {
		return observation, "source-boot-changed"
	}
	// Compare integer microseconds before constructing durations or timestamps.
	current := now.UnixMicro()
	if clock[0] <= 0 || clock[0] > clock[1] || clock[1] > clock[2] ||
		clock[2]-clock[0] > 20000000 || clock[3] > clock[4] || clock[4] > 20000000 ||
		current-clock[1] < -30000000 || current-clock[1] > 30000000 ||
		pgAutovacuumCancelAbs(clock[1]-clock[0]-clock[3]) > 1000000 || pgAutovacuumCancelAbs(clock[2]-clock[0]-clock[4]) > 1000000 {
		return observation, "source-clock-unavailable"
	}
	if before[0] == 0 || before[1] == 0 || read[0] > before[2] || read[1] > pgAutovacuumCancelMaxBytes ||
		read[1] > before[2]-read[0] || before[3] <= 0 || before[4] <= 0 {
		return observation, "invalid-file-snapshot"
	}
	for i := range before {
		if before[i] != after[i] {
			return observation, "source-snapshot-changed"
		}
	}
	if read[0] != max(int64(0), before[2]-pgAutovacuumCancelMaxBytes) || span[0] > span[1] ||
		(span[0] == 0) != (span[1] == 0) || (read[1] == 0 && span[1] != 0) {
		return observation, "invalid-source-range"
	}
	reasonSet := map[string]bool{}
	for _, flag := range wire.Flags {
		if reasonSet[flag] {
			return observation, "duplicate-source-flag"
		}
		switch flag {
		case "tail-capped", "partial-first", "path-changed", "record-order", "short-read", "snapshot-changed", "partial-last", "record-schema", "record-clock", "future-record", "row-cap", "record-session", "source-index-changed", "record-capped":
		default:
			return observation, "unknown-source-flag"
		}
		reasonSet[flag] = true
	}
	if reasonSet["snapshot-changed"] {
		return observation, "source-snapshot-changed"
	}
	if read[0] != 0 {
		reasonSet["tail-capped"] = true
	}
	if read[1] != before[2]-read[0] {
		reasonSet["short-read"] = true
	}
	if span[1] > clock[1] {
		reasonSet["future-record"] = true
	}
	cutoff := clock[1] - pgAutovacuumCancelWindow.Microseconds()
	if span[0] == 0 || span[0] > cutoff {
		reasonSet["window-start-uncovered"] = true
	}
	lastOffset := int64(-1)
	lastTime := int64(0)
	for _, fields := range wire.Rows {
		values, ok := pgAutovacuumCancelNumbers(fields, 2)
		if !ok || values[0] < read[0] || values[0] <= lastOffset || values[0] >= read[0]+read[1] ||
			values[1] <= 0 || values[1] < span[0] || values[1] > span[1] || values[1] > clock[1] {
			return pgAutovacuumCancelObservation{}, "invalid-or-duplicated-record"
		}
		if values[1] < lastTime {
			reasonSet["record-order"] = true
		}
		lastOffset, lastTime = values[0], values[1]
		if values[1] >= cutoff {
			observation.records++
		}
	}
	for reason := range reasonSet {
		observation.reasons = append(observation.reasons, reason)
	}
	sort.Strings(observation.reasons)
	return observation, ""
}

// All source timestamps have already passed canonical positive integer checks.
func pgAutovacuumCancelAbs(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}

// A single primary-owned, bounded read supplies both observations and coverage.
// Parent cancellation wins even at the completion clock; no later read follows.
func (self pgAutovacuumCancelProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	primaries := env.cfg.hostsWithRole("pg-primary")
	if len(primaries) != 1 {
		return []finding{pgAutovacuumCancelUnknown("primary", "primary-inventory-unavailable-or-ambiguous", nil)}, nil
	}
	primary := primaries[0]
	output, err := env.runner.sshTimeout(ctx, primary, pgAutovacuumCancelCommand(primary.name), "", pgAutovacuumCancelTimeout)
	if ctx.Err() != nil {
		if err != nil {
			return nil, &signalContextError{contextErr: ctx.Err(), sourceErr: err}
		}
		return nil, ctx.Err()
	}
	if errors.Is(err, context.Canceled) {
		return nil, &signalContextError{contextErr: context.Canceled, sourceErr: err}
	}
	if err != nil {
		return []finding{pgAutovacuumCancelUnknown(primary.name, "bounded-source-unavailable", err)}, nil
	}
	observation, reason := parsePgAutovacuumCancelSnapshot(output, primary.name, env.now())
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if reason != "" {
		return []finding{pgAutovacuumCancelUnknown(primary.name, reason, nil)}, nil
	}
	findings := []finding{}
	coverage := "complete-observed-file"
	if len(observation.reasons) != 0 {
		coverage = "partial-observed-file"
		findings = append(findings, pgAutovacuumCancelUnknown(primary.name, strings.Join(observation.reasons, ","), nil))
	} else {
		findings = append(findings, healthyFinding(self.id(), tierWarn, "pg-autovacuum-cancel-unobservable", primary.name))
	}
	if observation.records == 0 {
		if len(observation.reasons) == 0 {
			findings = append(findings, healthyFinding(self.id(), tierWarn, "pg-autovacuum-cancel-observed", primary.name))
		}
		return findings, nil
	}
	findings = append(findings, finding{
		probeId: self.id(), tier: tierWarn, class: "pg-autovacuum-cancel-observed", target: primary.name, sustain: 1,
		symptom:   "The observed PostgreSQL JSON log records an autovacuum-worker cancellation error",
		mechanism: "One structured primary record identifies an autovacuum worker, ERROR severity, SQLSTATE 57014 and the exact cancellation message, with qualified server timestamp, PID and session fields. Separate byte positions count recorded messages, not unique passes or lost work.",
		baseline:  "Observation-only policy: WARN on at least one qualified PG18 JSON cancellation record in the preceding fifteen minutes. Ordinary lock conflicts and deliberate maintenance can also produce this observation.",
		observed:  fmt.Sprintf("window_s=900 cancellation_records_at_least=%d coverage=%s", observation.records, coverage),
		evidence:  "A bounded current_logfiles index selects the JSON file under the fixed trusted cluster/owner boundary. Each physical object is decoded once with duplicate-key rejection; SQL, identity, context and message strings cannot become additional records. Only numeric offsets/timestamps and fixed framing leave the collector.",
		context:   "False-positive qualifier: this server record does not establish faulty maintenance or lost work; small replacement passes may complete. Trusted server code, cluster/path administration and file ownership are premises, not cryptographic origin. False-negative qualifiers: missing JSON configuration/source, unsupported locale/schema, rotation, caps and unflushed records remain outside complete observation. Active logging and continuous historical configuration are unproved. The full size/phase/pass/intent/lost-work policy in §2.4a remains missing.",
		action:    "Correlate the exact pass with existing progress/completion and maintenance-owner evidence before deciding whether work was repeated. Let progressing vacuum finish; do not cancel work, retune thresholds or restart PostgreSQL from this record count.",
		verify:    "Two complete quiet observed-file windows can clear this recorded-event finding under notifier policy. They do not prove completion of an earlier pass or repair of an unobserved cause.",
		playbook:  "SIGNALS.md §2.4e and §2.4a",
	})
	return findings, nil
}

// Structural reasons retain incomplete-source authority without exposing the
// source error. This visibility class prevents quiet gaps from clearing events.
func pgAutovacuumCancelUnknown(target, reason string, err error) finding {
	observed := "reason=" + reason
	if err != nil {
		observed += " " + observationFailureDetails(err, classifyObservationError(err))
	}
	return finding{
		probeId: "pg/autovacuum-cancel", tier: tierWarn, class: "pg-autovacuum-cancel-unobservable", target: target, sustain: 2,
		symptom:   "The observed PostgreSQL cancellation history is incomplete",
		mechanism: "A missing JSON destination, unqualified owner/index, changed file, malformed object or incomplete history cannot establish absence of structured cancellation records. Independent complete records can still warn beside partial coverage; plaintext is never a fallback.",
		baseline:  "A fresh same-host stable regular JSON-file snapshot selected by one bounded current_logfiles index covers the preceding fifteen-minute observed history under the reviewed PG18 UTC/English schema.",
		observed:  observed,
		evidence:  "Only fixed reason codes and typed source-failure categories are public. Boot/file identities, source cells, exceptions and log content remain private.",
		context:   "Unknown is not healthy. Matching a plaintext line cannot establish worker origin. Even qualified structured records do not attest live/continuous logging, flushed messages, maintenance intent, completed replacement passes or lost-work quantity.",
		action:    observationFailureAction(err, classifyObservationError(err), "Verify the existing trusted PG18 cluster/current_logfiles JSON source and retained history. Preserve earlier findings; missing JSON needs a separate owner/configuration decision. Do not enable logging, change permissions, enlarge caps or retry from this observation."),
		verify:    "A fresh stable full-file observation from the qualified JSON index begins before the cutoff and has complete structured records, no source flags and no current cancellation records.",
		playbook:  "SIGNALS.md §2.4e and §2.4a",
	}
}
