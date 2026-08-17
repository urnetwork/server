// Detection probes: one per SIGNALS.md signal. A probe measures a signal,
// compares it to a band (static from SIGNALS.md, refined by learned
// baselines), and emits findings.
//
// A probe detects; it does not diagnose or fix. When a signal trips, a probe
// also runs the cheap, perishable evidence-collection steps from the
// SIGNALS.md playbook (the escalation battery, battery.go) so the ticket
// arrives pre-loaded with the measurements a diagnostician would run first.
package main

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// probeEnv is what a probe needs to run.
type probeEnv struct {
	cfg      *monitorConfig
	runner   *runner
	baseline *baselineStore
}

// alert tiers (SIGNALS.md §7)
const (
	tierPage = "page"
	tierWarn = "warn"
)

// finding is one evaluated signal from a probe. healthy=true means the signal
// is in its healthy band (used to auto-resolve an open ticket); healthy=false
// is a band violation. Identity is (probeId, class, target, frame): one ticket
// per identity.
type finding struct {
	probeId string
	tier    string
	class   string
	target  string // host, host:port, table — the concrete thing the signal is about
	frame   string // innermost app frame / query id / node ports, or ""

	healthy bool
	// sustain is how many consecutive failing ticks before a ticket opens
	// (SIGNALS.md "for n min" translated to ticks at the probe's cadence)
	sustain int

	// SIGNALS.md §6b payload — real names and observed values
	symptom  string
	baseline string
	observed string
	evidence string
	context  string
	playbook string
}

// probe is one SIGNALS.md signal encoded as an automated check.
type probe interface {
	// id is the alert id from SIGNALS.md §7 (e.g. "pg/active-pileup")
	id() string
	tier() string
	cadence() time.Duration
	// check runs the probe. A returned error is a probe-execution failure
	// (e.g. the host is unreachable) — the caller turns that into a
	// monitor/visibility finding; it is distinct from a finding with
	// healthy=false, which is a real detection.
	check(ctx context.Context, env *probeEnv) ([]finding, error)
}

// healthyFinding is a convenience for a probe reporting its signal in-band.
func healthyFinding(probeId, tier, class, target string) finding {
	return finding{probeId: probeId, tier: tier, class: class, target: target, healthy: true}
}

// atoiRow parses the i'th cell of a psql row as an int, tolerating decimals.
func atoiRow(r pgRow, i int) int {
	s := r.str(i)
	if dot := strings.IndexByte(s, '.'); dot >= 0 {
		s = s[:dot]
	}
	n, _ := strconv.Atoi(s)
	return n
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

func atof(s string) float64 {
	v, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return v
}

func gb(v float64) float64 {
	return v / 1e9
}
