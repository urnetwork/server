// Observe live PostgreSQL huge-page use on the configured primary; kernel
// reservation, page-table size and NUMA pressure remain separate contracts.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// SIGNALS.md §8.14b owns only the specified live primary setting predicate.
func NewPgHugePagesSignal() Signal {
	return &signalAdapter{
		number: "8.14b", key: "pg-huge-pages", name: "PostgreSQL primary huge pages",
		probe: pgHugePagesProbe{},
	}
}

// A single bounded direct database observation owns identity and value.
type pgHugePagesProbe struct{}

func (pgHugePagesProbe) id() string             { return "pg/huge-pages" }
func (pgHugePagesProbe) tier() string           { return tierWarn }
func (pgHugePagesProbe) cadence() time.Duration { return time.Minute }

// The shared PostgreSQL transport already bounds SQL to 30 seconds and uses
// direct localhost with a read-only transaction. Include queue/SSH time here.
func (pgHugePagesProbe) runBudget() time.Duration { return 40 * time.Second }

// psql's fixed read-only host command runs within the same SSH call as the
// direct SQL. Missing settings and PostgreSQL's unknown status are not off.
// psql quotes the captured hostname as one SQL literal. The only output is
// one four-cell CSV row, preserving the shared decoder's fixed-width contract.
// No raw setting text crosses the SQL boundary; the source clock is numeric.
const pgHugePagesQuery = "\\set monitor_primary_hostname `hostname -s`\n" + `SELECT :'monitor_primary_hostname',
       extract(epoch FROM clock_timestamp())::text,
       CASE WHEN pg_is_in_recovery() THEN 'replica' ELSE 'primary' END,
       CASE current_setting('huge_pages_status', true)
         WHEN 'on' THEN 'on'
         WHEN 'off' THEN 'off'
         ELSE 'unavailable'
       END;
`

// Require one configured role owner and the exact host's direct primary
// status before considering the setting. A replica cannot certify the primary.
func (self pgHugePagesProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	primaries := env.cfg.hostsWithRole("pg-primary")
	if len(primaries) != 1 {
		return []finding{pgHugePagesUnknown("primary", "primary-inventory-unavailable-or-ambiguous", nil)}, nil
	}
	primary := primaries[0]
	rows, err := env.runner.pg(ctx, pgHugePagesQuery)
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
		return []finding{pgHugePagesUnknown(primary.name, "bounded-source-unavailable", err)}, nil
	}
	status, reason := parsePgHugePages(rows, primary.name, env.now())
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if reason != "" {
		return []finding{pgHugePagesUnknown(primary.name, reason, nil)}, nil
	}
	if status == "on" {
		return []finding{healthyFinding(self.id(), tierWarn, "pg-huge-pages-disabled", primary.name)}, nil
	}
	return []finding{{
		probeId: self.id(), tier: tierWarn, class: "pg-huge-pages-disabled", target: primary.name, sustain: 1,
		symptom:   "The configured PostgreSQL primary is running without huge pages",
		mechanism: "The live huge_pages_status is off. huge_pages=try can fall back to ordinary pages even when configuration requests huge pages.",
		baseline:  "The single configured, source-identified PostgreSQL primary reports huge_pages_status=on.",
		observed:  "primary_identity=matched database_role=primary huge_pages_status=off",
		evidence:  "One bounded direct PostgreSQL read returned the source host, database recovery role, database clock and an allowlisted live setting; no aggregate memory or configured reservation substitutes for the setting.",
		context:   "False-positive qualifier: expected page-table regrowth on ordinary pages is not a leak, and brief kswapd activity does not establish sustained NUMA pressure. False-negative qualifier: this predicate does not measure the configured pool, page-table baseline or per-node pressure; a missing or unsupported setting remains unknown.",
		action:    "Have the database and host owners verify the intended huge-page reservation and startup fallback. Any boot reservation or primary restart requires separate operator authorization and the §5.13 plan.",
		verify:    "A fresh same-host direct observation identifies the configured primary and reports huge_pages_status=on. This clears only the live setting predicate; reservation size, PageTables and NUMA remain separately unqualified.",
		playbook:  "SIGNALS.md §8.14b and §5.13",
	}}, nil
}

// Reject partial, stale, wrong-host and unknown source values without echoing
// source cells. The timestamp is PostgreSQL's clock, never query completion.
func parsePgHugePages(rows []pgRow, primaryName string, now time.Time) (string, string) {
	if len(rows) != 1 || len(rows[0]) != 4 {
		return "", "invalid-source-shape"
	}
	shortName, _, _ := strings.Cut(primaryName, ".")
	if shortName == "" || rows[0][0] != shortName {
		return "", "source-host-mismatch"
	}
	stamp, err := strconv.ParseFloat(rows[0][1], 64)
	age := float64(now.UnixNano())/1e9 - stamp
	if err != nil || math.IsNaN(stamp) || math.IsInf(stamp, 0) || stamp <= 0 || age < -30 || age > 30 {
		return "", "source-clock-unavailable"
	}
	if rows[0][2] != "primary" {
		return "", "source-not-primary"
	}
	switch rows[0][3] {
	case "on", "off":
		return rows[0][3], ""
	default:
		return "", "setting-unavailable-or-unsupported"
	}
}

// Fixed reason fields preserve source uncertainty and shared typed errors.
func pgHugePagesUnknown(target, reason string, err error) finding {
	observed := "reason=" + reason
	if err != nil {
		observed += " " + observationFailureDetails(err, classifyObservationError(err))
	}
	return finding{
		probeId: "pg/huge-pages", tier: tierWarn, class: "pg-huge-pages-unobservable", target: target, sustain: 1,
		symptom:   "The configured primary's live huge-page state is unknown",
		mechanism: "An absent, ambiguous, stale, unsupported, failed or replica observation cannot establish huge_pages_status on the configured primary.",
		baseline:  "One enabled primary owner returns its matching source host, primary role, fresh source clock and supported live setting.",
		observed:  observed,
		evidence:  "No source cells, error text, configuration contents or credentials are retained in the finding.",
		context:   "Unknown is neither off nor healthy. Aggregate memory availability, huge_pages=try and a configured kernel pool do not establish live PostgreSQL huge-page use.",
		action:    observationFailureAction(err, classifyObservationError(err), "Restore the bounded primary observation and verify inventory ownership and PostgreSQL setting support; retain any existing huge-page finding."),
		verify:    fmt.Sprintf("A fresh complete observation of %s identifies the primary and returns a supported live setting; only on clears pg-huge-pages-disabled.", target),
		playbook:  "SIGNALS.md §8.14b",
	}
}
