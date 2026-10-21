// The database host's root-filesystem safety line is independent of the
// unqualified growth forecast. Raw scrape times own this absolute measurement.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"
)

const (
	pgDiskSpaceFloorBytes = 100 * 1000 * 1000 * 1000
	pgDiskSpaceFreshness  = 90 * time.Second
	pgDiskSpaceMaxBytes   = 64 * 1024
)

// SIGNALS.md §2.4b measures the configured primary host's root filesystem.
// The separate §2.4c forecast cannot prevent this predicate's recovery.
func NewPgDiskSpaceSignal() Signal {
	return &signalAdapter{
		number: "2.4b", key: "pg-disk-space", name: "Database root filesystem safety line",
		probe: pgDiskSpaceProbe{},
	}
}

// One bounded raw-range read supplies the latest coherent byte pair.
type pgDiskSpaceProbe struct{}

// Preserve the independent absolute predicate's stable identity.
func (pgDiskSpaceProbe) id() string { return "pg/disk-space" }

// The approved safety-line policy has no automatic page escalation.
func (pgDiskSpaceProbe) tier() string { return tierWarn }

// Normal sampling respects the operator's fifteen-minute minimum cadence.
func (pgDiskSpaceProbe) cadence() time.Duration { return 15 * time.Minute }

// The total budget includes source admission and service-gateway failover.
func (pgDiskSpaceProbe) runBudget() time.Duration { return 20 * time.Second }

// A raw selector returns original scrape timestamps, not an instant query's
// evaluation time. Preserve every source label instead of aggregating replicas.
func pgDiskSpaceCommand(environment, primary string, at time.Time) string {
	query := `{__name__=~"node_filesystem_(avail|size)_bytes",env=` + strconv.Quote(environment) +
		`,job="node",host=` + strconv.Quote(primary) + `,mountpoint="/"}[90s]`
	return "# monitor-pg-disk-space\n" +
		"curl -fsS --max-time 15 --max-filesize " + strconv.Itoa(pgDiskSpaceMaxBytes) +
		" --data-urlencode " + shellSingleQuote("query="+query) +
		" --data-urlencode " + shellSingleQuote("time="+strconv.FormatInt(at.Unix(), 10)) +
		" 'http://127.0.0.1:3100/prometheus/api/v1/query'"
}

// The only public values are available bytes, total bytes and source age.
// Device paths and arbitrary source labels remain private to validation.
type pgDiskSpaceObservation struct {
	available uint64
	size      uint64
	at        time.Time
}

// Reject partial API responses, duplicate sources and mismatched gauge times.
// Older samples never replace a newer malformed or incomplete observation.
func parsePgDiskSpace(output, environment, primary string, requestedAt, completedAt time.Time) (pgDiskSpaceObservation, string) {
	if len(output) > pgDiskSpaceMaxBytes {
		return pgDiskSpaceObservation{}, "response-byte-bound"
	}
	var response struct {
		Status   string            `json:"status"`
		Warnings []json.RawMessage `json:"warnings"`
		Error    json.RawMessage   `json:"error"`
		Data     struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string   `json:"metric"`
				Values [][]json.RawMessage `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(output), &response) != nil || response.Status != "success" ||
		response.Data.ResultType != "matrix" || len(response.Warnings) != 0 || len(response.Error) != 0 || len(response.Data.Result) != 2 {
		return pgDiskSpaceObservation{}, "incomplete-or-invalid-response"
	}
	if completedAt.Before(requestedAt) {
		return pgDiskSpaceObservation{}, "observer-clock-reversed"
	}
	observation := pgDiskSpaceObservation{}
	identity := ""
	seen := map[string]bool{}
	for _, series := range response.Data.Result {
		labels := series.Metric
		for name := range labels {
			switch name {
			case "__name__", "env", "job", "host", "device", "fstype", "mountpoint", "instance":
			default:
				return pgDiskSpaceObservation{}, "unsupported-source-label"
			}
		}
		metric := labels["__name__"]
		if (metric != "node_filesystem_avail_bytes" && metric != "node_filesystem_size_bytes") || seen[metric] ||
			labels["env"] != environment || labels["job"] != "node" || labels["host"] != primary ||
			labels["mountpoint"] != "/" || labels["device"] == "" || labels["fstype"] == "" {
			return pgDiskSpaceObservation{}, "unowned-or-ambiguous-filesystem"
		}
		seen[metric] = true
		delete(labels, "__name__")
		encoded, _ := json.Marshal(labels)
		if identity == "" {
			identity = string(encoded)
		} else if identity != string(encoded) {
			return pgDiskSpaceObservation{}, "mismatched-filesystem-source"
		}
		if len(series.Values) == 0 || len(series.Values) > 256 {
			return pgDiskSpaceObservation{}, "missing-or-excessive-samples"
		}
		var previousMillis int64
		var latestBytes uint64
		for _, pair := range series.Values {
			var stamp float64
			var text string
			if len(pair) != 2 || json.Unmarshal(pair[0], &stamp) != nil || json.Unmarshal(pair[1], &text) != nil ||
				math.IsNaN(stamp) || math.IsInf(stamp, 0) || stamp <= 0 || stamp > float64(requestedAt.UnixMilli())/1000 ||
				stamp <= float64(requestedAt.Add(-pgDiskSpaceFreshness).UnixMilli())/1000 {
				return pgDiskSpaceObservation{}, "invalid-source-time-or-value"
			}
			millis := int64(math.Round(stamp * 1000))
			if stamp != float64(millis)/1000 || millis <= previousMillis {
				return pgDiskSpaceObservation{}, "unordered-or-duplicate-source-time"
			}
			value, err := strconv.ParseFloat(text, 64)
			if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || math.Trunc(value) != value || value > 9007199254740991 {
				return pgDiskSpaceObservation{}, "invalid-byte-value"
			}
			previousMillis = millis
			latestBytes = uint64(value)
		}
		latestAt := time.UnixMilli(previousMillis)
		if completedAt.Sub(latestAt) > pgDiskSpaceFreshness {
			return pgDiskSpaceObservation{}, "stale-source"
		}
		if !observation.at.IsZero() && !observation.at.Equal(latestAt) {
			return pgDiskSpaceObservation{}, "mismatched-gauge-time"
		}
		observation.at = latestAt
		if metric == "node_filesystem_avail_bytes" {
			observation.available = latestBytes
		} else {
			observation.size = latestBytes
		}
	}
	if observation.size == 0 || observation.available > observation.size {
		return pgDiskSpaceObservation{}, "inconsistent-filesystem-bytes"
	}
	return observation, ""
}

// An unavailable floor read cannot clear an earlier measured low-space alert.
func pgDiskSpaceUnknown(target, reason string, err error) finding {
	observed := "reason=" + reason
	if err != nil {
		observed += " " + observationFailureDetails(err, classifyObservationError(err))
	}
	return finding{
		probeId: "pg/disk-space", tier: tierWarn, class: "pg-disk-space-unobservable", target: target, sustain: 2,
		symptom:   "The database host's root-filesystem safety-line state is unknown",
		mechanism: "The configured host, unique root filesystem, complete source response or fresh aligned available/size samples could not be established.",
		baseline:  "One enabled pg-primary inventory host supplies one coherent, fresh root-filesystem byte pair.",
		observed:  observed,
		evidence:  "Only fixed reasons and source error classes are retained; raw response values, labels, device paths and source errors stay private.",
		context:   "Unknown available space is neither zero nor healthy. Backup archive free space and PostgreSQL relation size do not replace this root-filesystem measurement.",
		action:    observationFailureAction(err, classifyObservationError(err), "Restore the inventory-owned filesystem metric source and bounded Mimir read; retain any earlier low-space incident until the same absolute predicate is observed completely."),
		verify:    "A complete fresh root-filesystem pair restores only the absolute safety-line observation; the separate pg-disk-runway forecast remains unqualified.",
		playbook:  "SIGNALS.md §2.4b",
	}
}

// A measured crossing survives independent forecast uncertainty. Cancellation
// retains original causes and never publishes a completed observation.
func (self pgDiskSpaceProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	primaries := env.cfg.hostsWithRole("pg-primary")
	if len(primaries) != 1 {
		return []finding{pgDiskSpaceUnknown("primary/root", "primary-inventory-unavailable-or-ambiguous", nil)}, nil
	}
	primary := primaries[0]
	target := primary.name + "/root"
	if scoped, ok := env.runner.(*hostScopeRunner); ok {
		if err := scoped.guardHost(ctx, primary); err != nil {
			return nil, err
		}
	}
	gateways := env.cfg.hostsWithRole("services")
	if len(gateways) == 0 {
		return []finding{pgDiskSpaceUnknown(target, "service-gateway-unavailable", nil)}, nil
	}
	requestedAt := env.now().UTC().Truncate(time.Second)
	output, _, err := shellFirstServiceGateway(ctx, env.runner, gateways, nil, pgDiskSpaceCommand(env.cfg.env, primary.name, requestedAt))
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
		return []finding{pgDiskSpaceUnknown(target, "source-command-unavailable", err)}, nil
	}
	completedAt := env.now().UTC()
	observation, reason := parsePgDiskSpace(output, env.cfg.env, primary.name, requestedAt, completedAt)
	if reason != "" {
		return []finding{pgDiskSpaceUnknown(target, reason, nil)}, nil
	}
	if observation.available > pgDiskSpaceFloorBytes {
		return []finding{healthyFinding(self.id(), tierWarn, "pg-disk-space-low", target)}, nil
	}
	return []finding{{
		probeId: self.id(), tier: tierWarn, class: "pg-disk-space-low", target: target, sustain: 1,
		symptom:   "The database host's root filesystem is at or below its 100 GB safety line",
		mechanism: "Fresh available bytes crossed the incident's decimal-byte safety line. This measures present headroom without predicting lasting growth or time to exhaustion.",
		baseline:  "Available root-filesystem space > 100000000000 bytes (100 decimal GB); WARN at or below the line on one observation, every fifteen minutes.",
		observed:  fmt.Sprintf("available_bytes=%d filesystem_size_bytes=%d safety_line_bytes=%d source_age_seconds=%.3f", observation.available, observation.size, pgDiskSpaceFloorBytes, completedAt.Sub(observation.at).Seconds()),
		evidence:  "One bounded raw Mimir range identifies the configured primary host and a single root filesystem; both gauges retain the same full source labels and latest original scrape timestamp.",
		context:   "False-positive qualifier: a temporary spill can cause a real momentary safety-line crossing and later recover; this alert does not attribute persistent growth. False-negative qualifiers: a fifteen-minute cadence can miss brief dips, and root headroom does not qualify other PostgreSQL tablespaces, inode availability, read-only status, WAL retention or the separate growth forecast.",
		action:    "Have the database/storage owner inspect bounded temp-file, WAL, relation-growth and vacuum evidence. Storage additions, retention changes and workload mutations remain owner decisions; do not treat this sample as a net-growth forecast.",
		verify:    "Fresh complete same-host root-filesystem observations exceed 100000000000 bytes for the notifier's recovery window; pg-disk-runway retains its independent unknown state.",
		playbook:  "SIGNALS.md §2.4b and §2.4c",
	}}, nil
}
