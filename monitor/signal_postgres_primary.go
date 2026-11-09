package monitor

// Signal postgres-primary implements SIGNALS.md §30.3 (OPS-M3-02; §19.8's "the Postgres primary down (the
// token exchange stops)" page, owner decision 143, FS-50). Every 30 s it asks the pg-primary host's
// Postgres, through the monitor's own read-only session, which host answered (pg_primary_guard.go) and
// whether it is in recovery:
//
//   - psql reaching the host and refused there (connection refused, the system starting up, shutting down
//     or in recovery mode), or an answer in recovery, which is a replica where the primary should be, pages
//     postgres-primary-down at any hour from M3: repair or restore (§19.10);
//   - an answer from another host than the inventory's primary, a malformed answer, or no way to reach the
//     host at all is an error, which the registry turns into a cannot-observe visibility alert: the host's
//     own failure and the monitor's path to it look alike from here, and neither may read as healthy.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

func NewPostgresPrimarySignal() Signal {
	return &signalAdapter{
		number: "30.3", key: "postgres-primary", name: "PostgreSQL primary serving",
		probe: postgresPrimaryProbe{},
	}
}

const postgresPrimaryProbeId = "pg/primary"

// SIGNALS.md §30.3's class.
const postgresPrimaryClassDown = "postgres-primary-down"

// What psql prints when it reached the host and the server there would not serve. psql prefixes every
// error of its own so; ssh's own failures never carry it.
const postgresPrimaryPsqlErrorPrefix = "psql: error:"

var postgresPrimaryRefusedPatterns = []string{
	"connection refused",
	"could not connect to server",
	"the database system is starting up",
	"the database system is shutting down",
	"the database system is in recovery mode",
	"the database system is not yet accepting connections",
	"no such file or directory",
}

// The query: the answering host's short name, as psql captured it on that host, and whether its Postgres
// is in recovery.
const postgresPrimaryQuery = pgPrimaryHostnameMetaCommand + "\nSELECT :'monitor_primary_hostname', pg_is_in_recovery();\n"

// The sustained runs before the page opens: 2, a minute at 30 s.
const postgresPrimaryPageSustain = 2

type postgresPrimaryProbe struct{}

func (postgresPrimaryProbe) id() string             { return postgresPrimaryProbeId }
func (postgresPrimaryProbe) tier() string           { return tierPage }
func (postgresPrimaryProbe) cadence() time.Duration { return 30 * time.Second }

// Whether err says psql reached the primary's host and its Postgres refused to serve.
func postgresPrimaryRefused(err error) bool {
	text := strings.ToLower(err.Error())
	if !strings.Contains(text, postgresPrimaryPsqlErrorPrefix) {
		return false
	}
	for _, pattern := range postgresPrimaryRefusedPatterns {
		if strings.Contains(text, pattern) {
			return true
		}
	}
	return false
}

func (self postgresPrimaryProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	primary := env.cfg.hostByRole("pg-primary")
	if primary == nil {
		return nil, errors.New("no pg-primary host in inventory")
	}
	down := func(symptom string, observed string) []finding {
		return []finding{{
			probeId: postgresPrimaryProbeId, tier: tierPage, class: postgresPrimaryClassDown, target: primary.name,
			sustain:  postgresPrimaryPageSustain,
			symptom:  symptom,
			observed: observed,
			mechanism: "With no primary serving, the token exchange stops, so no sidecar gets a service token, and every " +
				"write api takes fails.",
			baseline: "The inventory's primary answers as itself, out of recovery.",
			action:   "Repair the primary, or restore it (§19.10's restore order, its api step behind restore_hold).",
			verify:   "The primary answers as itself, out of recovery, for two runs.",
			playbook: "SIGNALS.md §30.3; §19.10 (DESIGN.md)",
		}}
	}
	rows, err := env.runner.pg(ctx, postgresPrimaryQuery)
	if err != nil {
		if postgresPrimaryRefused(err) {
			return down("the primary's host answers and its Postgres refuses connections", "error_class="+classifyObservationError(err)), nil
		}
		return nil, err
	}
	if len(rows) != 1 || len(rows[0]) != 2 {
		return nil, fmt.Errorf("the primary's answer has %d rows, want one host and its recovery state", len(rows))
	}
	answered := rows[0].str(0)
	want, _, _ := strings.Cut(primary.name, ".")
	if answered != want {
		return nil, fmt.Errorf("host %q answered for the primary, %q", answered, want)
	}
	switch rows[0].str(1) {
	case "f":
		return []finding{healthyFinding(postgresPrimaryProbeId, tierPage, postgresPrimaryClassDown, primary.name)}, nil
	case "t":
		return down("the primary answers in recovery: a replica stands where the primary should", "pg_is_in_recovery=t"), nil
	}
	return nil, fmt.Errorf("the primary's recovery state reads %q", rows[0].str(1))
}
