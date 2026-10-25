// Reduce bounded PostgreSQL duration-log snapshots to witnessed release
// clusters. Log observations do not prove a whole-database storage cause.
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
	pgFreezeMaxBytes  = 40000000
	pgFreezeMaxRows   = 100000
	pgFreezeMaxOutput = 8 * 1024 * 1024
	pgFreezeWindow    = 15 * time.Minute
	pgFreezeTimeout   = 25 * time.Second
)

// The fixed program projects timestamps, durations and bounded source framing.
// SQL, process IDs, database/user names and log/error text stay on the host.
//
//go:embed pg_freeze_program.py
var pgFreezeProgram string

// The catalog's proposed threshold now has explicit WARN1/15-minute policy.
func NewPgFreezeSignal() Signal {
	return &signalAdapter{number: "2.2b", key: "pg-freeze", name: "PostgreSQL duration-log release clusters", probe: pgFreezeProbe{}}
}

// Each run owns one source snapshot; no cursor or incident state is persisted.
type pgFreezeProbe struct{}

func (pgFreezeProbe) id() string               { return "pg/freeze" }
func (pgFreezeProbe) tier() string             { return tierWarn }
func (pgFreezeProbe) cadence() time.Duration   { return pgFreezeWindow }
func (pgFreezeProbe) runBudget() time.Duration { return 30 * time.Second }

// Authenticate the inventory host before the fixed reader opens any log.
func pgFreezeCommand(name string) string {
	shortName, _, _ := strings.Cut(name, ".")
	return "[ \"$(hostname -s)\" = " + shellSingleQuote(shortName) + " ] || exit 74; exec timeout -k 2s 20s python3 -c " + shellSingleQuote(pgFreezeProgram)
}

// All numbers are decimal strings, so null, fractional and overflowed source
// fields cannot silently become valid zero-valued metadata.
type pgFreezeWire struct {
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

// Byte positions distinguish identical observations from duplicated wire rows.
type pgFreezeRow struct {
	offset   int64
	ended    int64
	duration int64
}

// Only source-independent aggregates can enter a public finding.
type pgFreezeObservation struct {
	clusters50  int
	clusters100 int
	clusters250 int
	clusters2s  int
	maxMedian   float64
	rows        int
	reasons     []string
}

// Bounded canonical nonnegative decimal fields reject aliases and overflow.
func pgFreezeNumbers(values []string, count int) ([]int64, bool) {
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

var pgFreezeBootPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// The complete transport frame proves which file bytes were observed. A
// partial or malformed event cannot be dropped: doing so changes medians.
func parsePgFreezeSnapshot(output, expectedHost string, now time.Time) (pgFreezeObservation, string) {
	observation := pgFreezeObservation{}
	if len(output) > pgFreezeMaxOutput || !strings.HasSuffix(output, "\n") || !pgSampleUniqueObject(output) {
		return observation, "invalid-source-frame"
	}
	var wire pgFreezeWire
	decoder := json.NewDecoder(strings.NewReader(output))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&wire) != nil || wire.Version != "2" || wire.Status != "snapshot" {
		return observation, "json-native-source-unavailable-or-schema"
	}
	shortName, _, _ := strings.Cut(expectedHost, ".")
	if shortName == "" || wire.Host != shortName {
		return observation, "source-host-mismatch"
	}
	clock, clockOk := pgFreezeNumbers(wire.Clock, 5)
	before, beforeOk := pgFreezeNumbers(wire.FileBefore, 5)
	after, afterOk := pgFreezeNumbers(wire.FileAfter, 5)
	read, readOk := pgFreezeNumbers(wire.Read, 2)
	span, spanOk := pgFreezeNumbers(wire.Span, 2)
	if !clockOk || !beforeOk || !afterOk || !readOk || !spanOk || len(wire.Boot) != 2 ||
		wire.Flags == nil || wire.Rows == nil || len(wire.Rows) > pgFreezeMaxRows {
		return observation, "incomplete-source-frame"
	}
	if !pgFreezeBootPattern.MatchString(wire.Boot[0]) || wire.Boot[0] != wire.Boot[1] {
		return observation, "source-boot-changed"
	}
	// Compare integer microseconds before constructing durations or timestamps.
	current := now.UnixMicro()
	if clock[0] <= 0 || clock[0] > clock[1] || clock[1] > clock[2] ||
		clock[2]-clock[0] > 20000000 || clock[3] > clock[4] || clock[4] > 20000000 ||
		current-clock[1] < -30000000 || current-clock[1] > 30000000 ||
		pgFreezeAbs(clock[1]-clock[0]-clock[3]) > 1000000 || pgFreezeAbs(clock[2]-clock[0]-clock[4]) > 1000000 {
		return observation, "source-clock-unavailable"
	}
	if before[0] == 0 || before[1] == 0 || read[0] > before[2] || read[1] > pgFreezeMaxBytes ||
		read[1] != before[2]-read[0] || before[3] <= 0 || before[4] <= 0 {
		return observation, "invalid-file-snapshot"
	}
	for i := range before {
		if before[i] != after[i] {
			return observation, "source-snapshot-changed"
		}
	}
	if read[0] != max(int64(0), before[2]-pgFreezeMaxBytes) || span[0] > span[1] || span[1] > clock[1] {
		return observation, "invalid-source-range"
	}
	reasonSet := map[string]bool{}
	blockPositive := false
	for _, flag := range wire.Flags {
		if reasonSet[flag] {
			return observation, "duplicate-source-flag"
		}
		switch flag {
		case "tail-capped", "partial-first", "path-changed", "record-order", "source-index-changed":
		case "short-read", "snapshot-changed", "partial-last", "record-schema", "record-clock", "future-record", "duration-schema", "duration-range", "duration-origin", "record-cap", "row-cap":
			blockPositive = true
		default:
			return observation, "unknown-source-flag"
		}
		reasonSet[flag] = true
	}
	if read[0] != 0 {
		reasonSet["tail-capped"] = true
	}
	cutoff := clock[1] - pgFreezeWindow.Microseconds()
	if span[0] == 0 || span[0] > cutoff {
		reasonSet["window-start-uncovered"] = true
	}
	rows := make([]pgFreezeRow, 0, len(wire.Rows))
	lastOffset := int64(-1)
	lastTime := int64(0)
	for _, fields := range wire.Rows {
		values, ok := pgFreezeNumbers(fields, 3)
		if !ok || values[0] < read[0] || values[0] <= lastOffset || values[0] >= before[2] ||
			values[1] <= 0 || values[1] < span[0] || values[1] > span[1] ||
			values[2] < 1000000000 {
			return pgFreezeObservation{}, "invalid-or-duplicated-event"
		}
		if values[1] < lastTime {
			reasonSet["record-order"] = true
		}
		lastOffset, lastTime = values[0], values[1]
		rows = append(rows, pgFreezeRow{offset: values[0], ended: values[1], duration: values[2]})
	}
	if reasonSet["record-order"] && read[0] != 0 {
		blockPositive = true
	}
	observation.rows = len(rows)
	if !blockPositive {
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].ended == rows[j].ended {
				return rows[i].offset < rows[j].offset
			}
			return rows[i].ended < rows[j].ended
		})
		for start := 0; start < len(rows); {
			end := start + 1
			for end < len(rows) && rows[end].ended-rows[end-1].ended <= 100000 {
				end++
			}
			if (start == 0 || end == len(rows)) && rows[end-1].ended >= cutoff {
				reasonSet["unclosed-window-cluster"] = true
			}
			if start > 0 && end < len(rows) && rows[start].ended >= cutoff {
				durations := make([]int64, end-start)
				for i := start; i < end; i++ {
					durations[i-start] = rows[i].duration
				}
				sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
				median := float64(durations[len(durations)/2]) / 1e9
				if len(durations)%2 == 0 {
					median = (float64(durations[len(durations)/2-1]) + float64(durations[len(durations)/2])) / 2e9
				}
				if len(durations) >= 50 {
					observation.clusters50++
				}
				if len(durations) >= 100 {
					observation.clusters100++
				}
				if len(durations) >= 250 {
					observation.clusters250++
					observation.maxMedian = max(observation.maxMedian, median)
					if median >= 2 {
						observation.clusters2s++
					}
				}
			}
			start = end
		}
	}
	for reason := range reasonSet {
		observation.reasons = append(observation.reasons, reason)
	}
	sort.Strings(observation.reasons)
	return observation, ""
}

// Source clocks are positive bounded epoch microseconds before subtraction.
func pgFreezeAbs(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}

// A source failure or uncovered window holds recovery without discarding
// independently complete witnessed release clusters from the same snapshot.
func (self pgFreezeProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	primaries := env.cfg.hostsWithRole("pg-primary")
	if len(primaries) != 1 {
		return []finding{pgFreezeUnknown("primary", "primary-inventory-unavailable-or-ambiguous", nil)}, nil
	}
	primary := primaries[0]
	output, err := env.runner.sshTimeout(ctx, primary, pgFreezeCommand(primary.name), "", pgFreezeTimeout)
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
		return []finding{pgFreezeUnknown(primary.name, "bounded-source-unavailable", err)}, nil
	}
	observation, reason := parsePgFreezeSnapshot(output, primary.name, env.now())
	// Completion clocks and reduction remain inside this observation's lifetime.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if reason != "" {
		return []finding{pgFreezeUnknown(primary.name, reason, nil)}, nil
	}
	findings := []finding{}
	if len(observation.reasons) != 0 {
		findings = append(findings, pgFreezeUnknown(primary.name, strings.Join(observation.reasons, ","), nil))
	} else {
		findings = append(findings, healthyFinding(self.id(), tierWarn, "pg-freeze-unobservable", primary.name))
	}
	if observation.maxMedian < 3 && observation.clusters2s < 2 {
		if len(observation.reasons) == 0 {
			findings = append(findings, healthyFinding(self.id(), tierWarn, "pg-freeze-clusters", primary.name))
		}
		return findings, nil
	}
	findings = append(findings, finding{
		probeId: self.id(), tier: tierWarn, class: "pg-freeze-clusters", target: primary.name, sustain: 1,
		symptom:   "The observed PostgreSQL duration log contains qualified release clusters",
		mechanism: "At least 250 native duration records of one second or longer ended in a chain of gaps at most 100 ms. Real native duration records more than 100 ms before and after each counted chain witness its observed membership.",
		baseline:  "Proposed §2.2b policy: WARN after one 15-minute snapshot with any closed cluster median at least 3 s, or two closed cluster medians at least 2 s; no independent PAGE policy.",
		observed:  fmt.Sprintf("window_s=900 observed_rows=%d clusters_50=%d clusters_100=%d clusters_250=%d clusters_at_least_2s=%d max_median_s=%.6f", observation.rows, observation.clusters50, observation.clusters100, observation.clusters250, observation.clusters2s, observation.maxMedian),
		evidence:  "A bounded same-host PG18 JSON snapshot admits primary duration messages only with typed writer identity and existing verbose postgres.c/exec_* source metadata. Byte positions distinguish separate identical records; no SQL, user, database, process or raw error text is retained in the alert.",
		context:   "False-positive qualifier: one heavy query or relation-lock queue can create a release cluster; Parse, Bind and Execute may be separate native records; this observed-log symptom does not establish a whole-database freeze or storage cause. False-negative qualifiers: statements below one second, delayed/unflushed logging, unresolved edge chains, output/byte caps and rotation can hide events. Visibility findings retain these coverage limitations beside independent positives.",
		action:    "Follow §5.12. Corroborate waits, affected relations and the PgBouncer pool signal before attributing a whole-database freeze. If pools collapse, restore them under the existing owner and authorization first; do not restart PostgreSQL or tune storage from this cluster alone.",
		verify:    "A complete observed-log window no longer meets the cluster threshold, while the affected database and pool paths recover. This establishes only the observed log, not backend-flush or global logger completeness.",
		playbook:  "SIGNALS.md §2.2b and §5.12",
	})
	return findings, nil
}

// Fixed structural visibility prevents an incomplete observation from clearing
// a prior cluster incident. Operational failures keep their typed classifier.
func pgFreezeUnknown(target, reason string, err error) finding {
	observed := "reason=" + reason
	if err != nil {
		observed += " " + observationFailureDetails(err, classifyObservationError(err))
	}
	return finding{
		probeId: "pg/freeze", tier: tierWarn, class: "pg-freeze-unobservable", target: target, sustain: 1,
		symptom:   "The PostgreSQL duration-log observation cannot establish a complete window",
		mechanism: "A missing JSON source or verbose emitter metadata, or a changed, malformed, capped or temporally incomplete snapshot cannot prove absence of release clusters. A partial cluster's median is not a lower bound.",
		baseline:  "One stable same-host observed-log snapshot covers the preceding 15 minutes with complete chronological records and no unresolved edge chain intersecting the window.",
		observed:  observed,
		evidence:  "Only fixed structural reasons are public; raw log lines, SQL, file/boot incarnation values and exception text are discarded.",
		context:   "Independent closed clusters can still warn beside an uncovered window. No observation proves all backend events were flushed or that a cluster affected unrelated statements.",
		action:    observationFailureAction(err, classifyObservationError(err), "Restore the bounded reader and verify the existing PG18 JSON destination in /var/lib/postgresql/18/main/current_logfiles, UTC/English schema, verbose source metadata and append-only source. This probe does not enable logging or fall back to plaintext. Preserve prior incidents until a complete observed window is available; do not widen the cap or retry blindly."),
		verify:    "A fresh stable full-file snapshot covers the window and has no missing records or unresolved boundary cluster intersecting the window.",
		playbook:  "SIGNALS.md §2.2b",
	}
}
