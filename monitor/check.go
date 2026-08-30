// Internal checks used by the public named Signal adapters. A check measures a signal,
// compares it to a band (static from SIGNALS.md, refined by learned
// baselines), and emits findings.
//
// A probe detects; it does not diagnose or fix. When a signal trips, a probe
// also runs the cheap, perishable evidence-collection steps from the
// SIGNALS.md playbook (the escalation battery, battery.go) so the ticket
// arrives pre-loaded with the measurements a diagnostician would run first.
package monitor

import (
	"context"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// probeEnv is what a probe needs to run.
type probeEnv struct {
	cfg      *monitorConfig
	runner   probeRunner
	baseline *baselineStore
	now      func() time.Time
}

// probeRunner is the internal transport shape used by the named probes.
// Both the production SSH runner and the exported SignalSource adapter
// implement it.
type probeRunner interface {
	pg(ctx context.Context, sql string) ([]pgRow, error)
	redis(ctx context.Context, h *host, port int, args ...string) (string, error)
	redisRaw(ctx context.Context, h *host, port int, args ...string) (string, error)
	shell(ctx context.Context, h *host, command string) (string, error)
	sshTimeout(ctx context.Context, h *host, command string, stdin string, timeout time.Duration) (string, error)
	local(ctx context.Context, name string, args ...string) (string, error)
	tcpExchange(ctx context.Context, network, address string, payload []byte, responseBytes int) ([]byte, error)
	warpctl(ctx context.Context, args ...string) (string, error)
	warpctlStream(ctx context.Context, args ...string) (*exec.Cmd, io.ReadCloser, error)
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
	symptom   string
	mechanism string
	baseline  string
	observed  string
	evidence  string
	context   string
	action    string
	verify    string
	playbook  string
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

// cannotObserveFinding preserves partial signal results when one target or
// one layer of a composite probe is unreachable. Returning an error from the
// whole probe would discard concrete findings already collected elsewhere.
func cannotObserveFinding(target string, err error) finding {
	return finding{
		probeId: "monitor/visibility", tier: tierWarn,
		class: "cannot-observe", target: target, sustain: 2,
		symptom:   "The monitor could not complete an observation for " + target + ": " + err.Error(),
		mechanism: "The source command, parser, or network path failed, so this target's production state is unknown even if sibling checks completed.",
		baseline:  "Every configured target returns a bounded, parseable observation at each signal cadence.",
		observed:  err.Error(),
		action:    "Restore the observation path and rerun the named signal; also determine whether the unreachable target is the incident.",
		verify:    "The same target returns a concrete healthy or broken observation on the next run.",
		playbook:  "MONITOR.md §3.6",
	}
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

func atoi64(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n
}

func atof(s string) float64 {
	v, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return v
}

func gb(v float64) float64 {
	return v / 1e9
}

func pgTarget(env *probeEnv) string {
	if host := env.cfg.hostByRole("pg-primary"); host != nil {
		return host.name
	}
	return "pg"
}
