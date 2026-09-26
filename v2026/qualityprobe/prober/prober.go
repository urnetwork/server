// Package prober probes one provider: open a tunnel pinned to it, run the
// egress-health check through it -- whose warm-up, the operator's own /ip
// echo, is also where the exit address comes from -- and submit what it
// measured.
package prober

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
)

// One provider to probe: its client id, and the place it is
// published under, which decides the destinations its sample may draw from
// (see egresshealth.Options.ProviderPlace). A provider with no place excludes
// nothing.
type Provider struct {
	ClientId string
	Place    egresshealth.Place
}

// Opens a tunnel to one provider and returns an http.Client that
// egresses through it, plus a close function.
type TunnelOpener func(ctx context.Context, providerClientId string) (*http.Client, func() error, error)

// Runs the egress-health check over a client, for a
// provider published at place. In production this is egresshealth.Check,
// wired by fleetprobe with the pass's pool and the tunnel's path.
type EgressHealthChecker func(ctx context.Context, client *http.Client, place egresshealth.Place) (*egresshealth.Result, error)

// Measures the provider's throughput over the tunnel the
// probe already opened, and records what it measured. In production this is
// bandwidth.Sampler.Sample plus the log line, wired by fleetprobe.
//
// It returns nothing, deliberately. Bandwidth is a diagnostic riding along on
// a probe: no outcome of the measurement -- not a skipped budget reservation,
// not a dead target, not a zero sample -- may change whether the probe
// succeeded or what failure class was reported for it.
type BandwidthSampler func(ctx context.Context, providerClientId string, client *http.Client)

// Records where a provider's traffic exits: the address the
// operator's own /ip echo saw through its tunnel, and when. The server places
// it with its own GeoLite2 (GEOMAP §11.3). In production this is
// *ingest.Client.
type Submitter interface {
	Submit(ctx context.Context, providerClientId string, exitIp string, observedAt time.Time) error
}

// Records one egress-health run. In production this is
// *ingest.Client.
//
// It is an interface on the Prober, like Submitter and AttemptReporter, so
// this package never imports the submitter implementation and a test can drive
// the whole flow without a server.
type HealthReporter interface {
	SubmitEgressHealth(ctx context.Context, providerClientId string, res *egresshealth.Result) error
}

// Records that a probe was tried, whether or not it produced a
// result. In production this is *ingest.Client.
type AttemptReporter interface {
	ReportAttempt(ctx context.Context, providerClientId string, probeFailure string) error
}

// The failure classes reported to the server. They are short, stable strings
// (the server's column is varchar(64) and rejects anything longer), and "" is
// reserved to mean success.
const (
	// No tunnel to the provider. Refused contract, unreachable
	// platform, pin set rejected -- the probe never started.
	FailureTunnel = "tunnel_failed"
	// The egress-health run did not happen at all -- no
	// checker, no budget left, a structural error, or the pass ended under it.
	// Nothing was measured, so nothing was submitted.
	FailureHealthNotRun = "health_not_run"
	// The run happened but measured nothing -- its tunnel
	// died and could not be re-created within any load's attempts. It is not a
	// fault of the provider's traffic; nothing is submitted, and the attempt
	// backoff brings the provider round again.
	FailureNotMeasured = "run_not_measured"
	// The run measured loads but its warm-up never got an
	// answer from the operator's /ip echo, so there is no exit address to
	// submit. The health run itself was submitted.
	FailureNoExitIp = "no_exit_ip"
	// The exit address was observed and submitting it still
	// failed. Usually the server would not take it (a rejection, a 5xx, a dead
	// connection), but the submitter also refuses some locally, before any
	// request -- ingest.ErrMissingExitIp and ingest.ErrMissingProbedAt.
	// Either way the provider has no recorded location, which is what this
	// class reports.
	FailureSubmit = "submit_failed"
)

// ProbeOne's error for a run that measured nothing (see
// FailureNotMeasured), so a caller counting outcomes can tell it from a
// failure of the provider.
var ErrNotMeasured = errors.New("prober: the run measured nothing; its tunnel could not be re-created in time")

// Wires a tunnel opener, the health check, a submitter and an attempt
// reporter. Each dependency is injected so the flow is testable without a live
// provider or server.
type Prober struct {
	Open TunnelOpener
	// Runs the egress-health check over the tunnel. Required: its
	// warm-up is where the exit address comes from, so there is no probe
	// without it.
	//
	// The result is logged and, if HealthResults is set, submitted: the
	// server's egress index and one-in-ten rule read it. No verdict is derived
	// from it here.
	Health EgressHealthChecker
	// Records the exit address the health run's warm-up saw.
	Submit Submitter
	// Records each egress-health run so it outlives the log line.
	// Optional -- a nil reporter simply skips submitting.
	//
	// Fire-and-forget, exactly like Attempts: a submission failure is logged
	// once per distinct error and never changes the probe's outcome.
	HealthResults HealthReporter
	// Measures throughput over the same tunnel, after the exit has
	// been submitted. Optional -- a nil sampler skips it entirely.
	//
	// It runs last, and only after a probe that succeeded, for two reasons.
	// Last, because the location and health are the product of this pass and
	// must never lose budget or fail because of a diagnostic. Only after
	// success, because the deployment-wide byte budget it spends is scarce
	// (each measurement pulls megabytes of real, paid contract traffic through
	// the provider's tunnel), and spending it on a tunnel that could not carry
	// the probe buys a number that describes the failure rather than the link.
	Bandwidth BandwidthSampler
	// Reports every probe attempt. Optional -- a nil reporter simply
	// skips reporting -- but production must set it: see ProbeOne.
	Attempts AttemptReporter
	// Caps how many distinct error messages are logged in detail per pass,
	// by each of this prober's log gates and by a Scheduler driving it (I2).
	// Without a cap, a pass where every provider fails the same way (a wrong
	// -platform-url, a revoked jwt) would flood the log with the same message
	// hundreds of times, drowning out anything that could distinguish it from
	// a handful of unrelated failures. Zero or negative means 10: enough to
	// see the shape of what is failing (one pin mismatch, one auth error, a
	// few dial timeouts) without a flood. A notice is still logged once the
	// cap is hit, and the pass's total Failed count is always visible via the
	// Summary the caller logs.
	MaxLoggedDistinctErrors int

	// Deduplicates attempt-reporting error messages. Whatever stops
	// the reports getting through -- an older server with no attempt endpoint,
	// a wrong secret, a dead network -- stops them for every provider, so
	// logging per provider would bury the pass's real output under one
	// identical line per provider, every pass.
	attemptErr errGate

	// Deduplicates health-submission error messages, for the same
	// reason attemptErr does. A separate gate rather than a shared one: the
	// two reporters fail for different reasons and say different things about
	// what is lost, and a shared map would let a noisy attempt error suppress
	// the first health error (or the reverse).
	healthErr errGate

	// Deduplicates tunnel-teardown errors, on its own gate for the
	// same reason the two above are separate: a noisy teardown failure must
	// not consume the log budget that would otherwise have surfaced the first
	// health or attempt failure.
	closeErr errGate
}

// Deduplicates error messages within one pass and bounds how many
// distinct ones it logs.
//
// The cap is what makes this safe on a message that varies: the maps are
// keyed on the full error text, and ingest.ErrRejected embeds up to 4096
// bytes of server response body -- a body carrying a request id or a
// timestamp, which is ordinary, makes every message distinct. Without a cap
// the gate inverted, logging one line per provider per pass (the flood it
// exists to prevent) while the map grew an entry per probe.
//
// The reset is what makes the cap safe. These gates live on the Prober,
// which lives for the whole process -- months -- so a permanent cap is not
// the same mechanism the scheduler uses, even though it looks like it: the
// scheduler's map is local to one Run and re-arms every pass. Ten transient
// errors would otherwise burn the gate forever, and a later fault that
// breaks every attempt report (a rotated operator secret answering 401)
// would log nothing at all -- re-creating exactly the silent failure the
// logging exists to prevent. Each pass starts with a clean gate and closes
// by reporting what it suppressed.
//
// The mutex and its map are one type rather than two arguments so a caller
// cannot pair the wrong ones.
type errGate struct {
	stateLock  sync.Mutex
	seen       map[string]bool
	suppressed int
}

// Reports whether this error should be logged now, and records it so
// an identical message is not logged again this pass. At most limit distinct
// messages are allowed per pass.
func (self *errGate) allow(err error, limit int) bool {
	msg := err.Error()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.seen == nil {
		self.seen = map[string]bool{}
	}
	if self.seen[msg] {
		return false
	}
	if limit <= len(self.seen) {
		self.suppressed++
		return false
	}
	self.seen[msg] = true
	return true
}

// Re-arms the gate and returns how many distinct messages it withheld.
func (self *errGate) reset() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	n := self.suppressed
	self.seen = nil
	self.suppressed = 0
	return n
}

// Re-arms the per-pass error-log gates and reports what the
// finished pass withheld. The scheduler calls it at the start of every Run;
// a Prober driven directly can call it per pass for the same effect.
func (self *Prober) ResetErrorLogging() {
	for what, gate := range map[string]*errGate{
		"probe-attempt report": &self.attemptErr,
		"egress-health submit": &self.healthErr,
		"tunnel teardown":      &self.closeErr,
	} {
		if n := gate.reset(); 0 < n {
			log.Printf("prober: suppressed %d further distinct %s error(s) in the previous pass (detail is capped at %d per pass)", n, what, self.maxLoggedDistinctErrors())
		}
	}
}

// Returns MaxLoggedDistinctErrors, or its default when unset. A nil prober
// has the default too.
func (self *Prober) maxLoggedDistinctErrors() int {
	if self != nil && 0 < self.MaxLoggedDistinctErrors {
		return self.MaxLoggedDistinctErrors
	}
	return 10
}

// Probes a single provider. The tunnel is always closed, and nothing
// is submitted about a run that measured nothing.
//
// The attempt is reported afterwards whatever happened, success or failure.
// That is not bookkeeping: the server defers a provider from the due queue when
// a probe was recently attempted, not only when one succeeded. A provider that
// always fails to probe never gets a location row, so its observed_at stays
// NULL, and the due query sorts NULLs first -- it comes back at the head of
// every poll, forever, starving every healthy provider. The failure is silent,
// because the endpoint keeps returning a full and plausible batch. Reporting a
// success is redundant (the submitted location defers the provider for far
// longer than the attempt backoff) but harmless, and reporting unconditionally
// means no path through this function can forget.
//
// A failure to report never fails the probe itself.
func (self *Prober) ProbeOne(ctx context.Context, provider Provider) error {
	providerClientId := provider.ClientId

	// Submits one health run, fire-and-forget. It returns
	// nothing and can fail nothing: a diagnostic submission must never be able to
	// change what the probe reports.
	//
	// A server that does not implement the endpoint answers
	// egresshealth.ErrUnsupported, which is a clean skip rather than an error --
	// the prober keeps working against an older deployment, it just records no
	// health. Every other failure is logged once per distinct message, because it
	// will be the same failure for every provider in the pass.
	reportEgressHealth := func(res *egresshealth.Result) {
		if self.HealthResults == nil {
			return
		}
		err := self.HealthResults.SubmitEgressHealth(ctx, providerClientId, res)
		if err == nil {
			return
		}
		if errors.Is(err, egresshealth.ErrUnsupported) {
			// not a failure: this deployment has not shipped the endpoint. Still
			// deduplicated, since it holds for every provider in the pass.
			self.logHealthErrOnce(err, fmt.Sprintf("prober: this server does not store egress-health results (%s) -- health is logged but not persisted. Logged once.", err))
			return
		}
		self.logHealthErrOnce(err, fmt.Sprintf("prober: could not submit an egress-health result (provider=%s): %s -- while this persists, the health signal exists only in these logs and rolls off with them. Logged once per distinct error.", providerClientId, err))
	}

	// Runs the egress-health check, logs one line per provider,
	// and submits the run. It returns the run, or the failure class and error of
	// a probe that has nothing to submit.
	//
	// A run started on an expired context comes back 0/N -- which reads in the log
	// exactly like a total blackhole, but is the prober's own exhausted deadline
	// rather than anything the provider did. That would be a false accusation
	// against a working provider, so it is refused and logged as skipped instead.
	// egresshealth.Check makes the same check itself (ErrNoBudget), and refuses a
	// run the pass's context ended under (ErrInterrupted); this keeps the log
	// honest about why nothing ran.
	checkEgressHealth := func(client *http.Client) (*egresshealth.Result, string, error) {
		if self.Health == nil {
			return nil, FailureHealthNotRun, errors.New("prober: no egress-health checker is configured, and the exit address comes from its warm-up")
		}
		if err := ctx.Err(); err != nil {
			log.Printf("egress-health: provider=%s skipped: no budget left in this probe (%s) -- a run on an expired deadline would fail every destination and be indistinguishable from a blackhole", providerClientId, err)
			return nil, FailureHealthNotRun, fmt.Errorf("egress health skipped: %w", err)
		}

		res, err := self.Health(ctx, client, provider.Place)
		if err != nil {
			// Structural: the check did not happen. Deliberately not rendered as a
			// zero score, for the same reason as above.
			log.Printf("egress-health: provider=%s did not run: %s", providerClientId, err)
			return nil, FailureHealthNotRun, fmt.Errorf("egress health did not run: %w", err)
		}

		// The line reads:
		//
		//	egress-health: provider=<id> ok=48/50 dns=6/6 connectivity=8/8
		//	cdn=9/10 site=25/26 table=139 retried=3 failed=cachefly,reddit
		//
		// Every tally is over the destinations this run sampled and measured,
		// after their retries: egresshealth draws a bounded random subset of each
		// class per run, so cdn=9/10 is nine of the ten drawn today out of
		// eighteen, and table=139 is on the line so that cannot be misread. It
		// also means "failed=" is the only record of which destinations a given
		// provider failed -- two consecutive lines for the same provider name
		// different endpoints, and that is the check working, not drifting.
		// not-measured= names the loads whose tunnel was gone for their last
		// attempt (the Summary counts them as not_measured=N), and canary= the
		// unscored canaries. failure_stages= has only fixed stage names and
		// counts from failed loads; it never includes a raw request error.
		line := fmt.Sprintf("egress-health: provider=%s %s", providerClientId, res.Summary())
		if stages := res.FailureStageSummary(); stages != "" {
			line += " failure_stages=" + stages
		}
		if failed := res.FailedNames(); 0 < len(failed) {
			line += " failed=" + strings.Join(failed, ",")
		}
		if unmeasured := res.NotMeasuredNames(); 0 < len(unmeasured) {
			line += " not-measured=" + strings.Join(unmeasured, ",")
		}
		if 0 < len(res.CanaryFailedNames()) {
			line += " canary-failed=" + strings.Join(res.CanaryFailedNames(), ",")
		}

		// A run that measured nothing is not submitted: 0/0 is not evidence of
		// anything, and its not-measured loads are the prober's lost tunnel, not
		// the provider's traffic. Returning a failure class instead lets the
		// attempt backoff bring the provider round again.
		if res.Total == 0 && 0 < res.NotMeasured {
			log.Print(line + " -- nothing measured; not submitted")
			return nil, FailureNotMeasured, ErrNotMeasured
		}
		log.Print(line)

		// Submitted only on this path, after a run that measured something.
		// Neither early return above has one, and sending a zero for them would be
		// indistinguishable from a total blackhole -- a false accusation against a
		// provider whose check was skipped for the prober's own exhausted deadline.
		reportEgressHealth(res)
		return res, "", nil
	}

	// Runs the probe and returns the failure class ("" on success)
	// alongside the error, so the attempt is reported for every outcome.
	probe := func() (string, error) {
		client, closeTunnel, err := self.Open(ctx, providerClientId)
		if err != nil {
			// No provider id in the message: the scheduler dedups on the error
			// text and keeps only MaxLoggedDistinctErrors distinct ones, so an id
			// here made a fleet-wide identical failure (a wrong -platform-url, a
			// revoked jwt) look like one distinct error per provider, filling
			// every detail slot with copies of a single failure mode and
			// suppressing genuinely different ones. The id is already in the
			// caller's log line, as provider=.
			return FailureTunnel, fmt.Errorf("open tunnel: %w", err)
		}
		defer func() {
			if closeTunnel == nil {
				return
			}
			if err := closeTunnel(); err != nil && self.closeErr.allow(err, self.maxLoggedDistinctErrors()) {
				// Deduplicated on its own gate, for the reason the other two are
				// separate: a noisy teardown error must not consume the budget
				// that would have shown the first health or attempt failure. This
				// never fails the probe -- everything is submitted by the time it
				// runs -- but discarding it entirely made a tunnel leaking its
				// netstack completely invisible.
				log.Printf("prober: tunnel teardown failed (provider=%s): %s. Logged once per distinct error.", providerClientId, err)
			}
		}()

		res, failure, err := checkEgressHealth(client)
		if failure != "" {
			return failure, err
		}

		// The exit is the health run's warm-up answer: the address the operator's
		// own /ip echo saw through this tunnel. Without it there is nothing to
		// place the provider by, and the health run -- already submitted -- is all
		// this probe produced.
		if res.ExitIp == "" {
			return FailureNoExitIp, fmt.Errorf("the /ip echo gave no exit address: %s", res.IpEchoErr)
		}
		if err := self.Submit.Submit(ctx, providerClientId, res.ExitIp, res.ExitObservedAt); err != nil {
			return FailureSubmit, err
		}

		// The bandwidth sample rides the tunnel that is still open, never a second
		// one, and cannot change what this function returns: the probe has already
		// succeeded by the time it runs.
		if self.Bandwidth != nil {
			self.Bandwidth(ctx, providerClientId, client)
		}
		return "", nil
	}

	failure, err := probe()
	self.reportAttempt(ctx, providerClientId, failure)
	return err
}

// Logs line unless err was already logged this pass or the health gate is
// full.
func (self *Prober) logHealthErrOnce(err error, line string) {
	if self.healthErr.allow(err, self.maxLoggedDistinctErrors()) {
		log.Print(line)
	}
}

// Reports one probe attempt, fire-and-forget; a failure to report is logged
// once per distinct error and never fails the probe.
func (self *Prober) reportAttempt(ctx context.Context, providerClientId string, failure string) {
	if self.Attempts == nil {
		return
	}
	err := self.Attempts.ReportAttempt(ctx, providerClientId, failure)
	if err == nil {
		return
	}

	if self.attemptErr.allow(err, self.maxLoggedDistinctErrors()) {
		log.Printf("prober: could not report a probe attempt (provider=%s failure=%q): %s -- while this persists, providers that always fail to probe stay at the head of the server's due queue. Logged once per distinct error.", providerClientId, failure, err)
	}
}
