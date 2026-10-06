// Package prober opens a provider-pinned tunnel and publishes sampled website
// quality. Acknowledged health is independent of optional sampled exit evidence.
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

	"github.com/urnetwork/server/v2026/qualityprobe"
	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
)

// One provider to probe: its client id, and the place it is
// published under, which decides the destinations its sample may draw from
// (see egresshealth.Options.ProviderPlace). A provider with no place excludes
// nothing.
type Provider struct {
	ClientId             string
	Place                egresshealth.Place
	RunsNeeded           int
	SuccessesNeeded      int // deprecated compatibility alias for old due responses
	CycleStartedAt       time.Time
	OutcomeCount         int
	ClaimOrdinal         int64
	SecurityDestinations []egresshealth.Destination
}

// Opens a tunnel to one provider and returns an http.Client that
// egresses through it, plus a close function.
type TunnelOpener func(ctx context.Context, providerClientId string) (*http.Client, func() error, error)

// Runs the egress-health check over a client, for a
// provider published at place. In production this is egresshealth.Check,
// wired by fleetprobe with the pass's pool and the tunnel's path.
type EgressHealthChecker func(ctx context.Context, client *http.Client, place egresshealth.Place) (*egresshealth.Result, error)

// Measures the provider's throughput over the tunnel the
// probe already opened, and records what it measured. This is a standalone
// diagnostic hook; fleetprobe does not attach fixed bandwidth URL requests.
//
// It returns nothing, deliberately. Bandwidth is a diagnostic riding along on
// a probe: no outcome of the measurement -- not a skipped budget reservation,
// not a dead target, not a zero sample -- may change whether the probe
// succeeded or what failure class was reported for it.
type BandwidthSampler func(ctx context.Context, providerClientId string, client *http.Client)

// Records where a provider's traffic exits when an already-randomly-sampled
// HTTPS IP-text response provided valid independent evidence. The server places
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
	// The run happened but no quality trial was measurable. A local tunnel or
	// measurement limitation cannot accuse the provider's traffic. Independent
	// URL authentication can still be published before a paced retry.
	FailureNotMeasured = "run_not_measured"
	// Legacy pre-sampled-only failure class, retained to read historical rows.
	// Missing optional exit evidence no longer fails acknowledged website quality.
	FailureNoExitIp = "no_exit_ip"
	// Website health was measured but its ingest did not acknowledge it.
	// Older artifacts also used this class for an independent location failure.
	FailureSubmit = "submit_failed"
)

// ProbeOne's error for a run that measured nothing (see
// FailureNotMeasured), so a caller counting outcomes can tell it from a
// failure of the provider.
var ErrNotMeasured = errors.New("prober: no quality trial was measured; the local path or measurement was unavailable")

// Wires a tunnel opener, the health check, a submitter and an attempt
// reporter. Each dependency is injected so the flow is testable without a live
// provider or server.
type Prober struct {
	// Optional identity-free timing of one returned call, including joined Close.
	// Called concurrently outside locks; it must not block or perform I/O.
	ObserveTiming func(ProbeTiming)
	Open          TunnelOpener
	// Optional place-aware opener. When set, it takes precedence over Open so
	// the private tunnel can select the provider country's resolver at birth.
	OpenProvider func(context.Context, Provider) (*http.Client, func() error, error)
	// Runs the required sampled website check over the tunnel.
	// The result is logged and acknowledged by HealthResults: the
	// server's egress index and one-in-ten rule read it. No verdict is derived
	// from it here.
	Health EgressHealthChecker
	// Optional independent location reporter for sampled public-IP evidence.
	Submit Submitter
	// Required: quality succeeds only after this reporter acknowledges evidence.
	// A failure is deduplicated in logs and reported as submit_failed.
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
	// Optional location failures never revoke already-acknowledged quality.
	locationErr errGate
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
		"location submit":      &self.locationErr,
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

// Probes a single provider. The tunnel is always closed. An unmeasured URL
// outcome may still publish independently validated per-hop security evidence.
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
// Legacy/manual attempt reporting is best effort. A claimed URL turn requires
// an identity-bearing completion acknowledgement, independent of health credit.
func (self *Prober) ProbeOne(ctx context.Context, provider Provider) error {
	timing := newProbeTimingRecorder(self.ObserveTiming)
	err := self.probeOne(ctx, provider, timing)
	timing.finish()
	return err
}

func (self *Prober) probeOne(ctx context.Context, provider Provider, timing *probeTimingRecorder) error {
	providerClientId := provider.ClientId

	// A measurement is not coverage until its required ingest acknowledges it.
	// Unsupported and failed endpoints retain their distinct diagnostic errors.
	reportEgressHealth := func(res *egresshealth.Result) error {
		if self.HealthResults == nil {
			return errors.New("prober: health reporter is required for acknowledged quality")
		}
		err := self.HealthResults.SubmitEgressHealth(ctx, providerClientId, res)
		if err == nil {
			return nil
		}
		if errors.Is(err, egresshealth.ErrUnsupported) {
			// No acknowledged health on this deployment. Deduplicate the diagnostic
			// since it can hold for every provider in the pass.
			self.logHealthErrOnce(err, fmt.Sprintf("prober: this server does not store egress-health results (%s) -- health is logged but not persisted. Logged once.", err))
			return err
		}
		self.logHealthErrOnce(err, fmt.Sprintf("prober: could not submit an egress-health result (provider=%s): %s -- while this persists, the health signal exists only in these logs and rolls off with them. Logged once per distinct error.", providerClientId, err))
		return err
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
			return nil, FailureHealthNotRun, errors.New("prober: no egress-health checker is configured")
		}
		if err := ctx.Err(); err != nil {
			log.Printf("egress-health: provider=%s skipped: no budget left in this probe (%s) -- a run on an expired deadline would fail every destination and be indistinguishable from a blackhole", providerClientId, err)
			return nil, FailureHealthNotRun, fmt.Errorf("egress health skipped: %w", err)
		}

		res, err := self.Health(ctx, client, provider.Place)
		if err == nil && res == nil {
			err = errors.New("prober: health checker returned no evidence")
		}
		if err != nil {
			// Structural: the check did not happen. Deliberately not rendered as a
			// zero score, for the same reason as above.
			log.Printf("egress-health: provider=%s did not run: %s", providerClientId, err)
			return nil, FailureHealthNotRun, fmt.Errorf("egress health did not run: %w", err)
		}
		// A checker may reuse immutable evidence across callers. Admission and
		// receipt identity belong to this turn, never to that shared input.
		reported := *res
		res = &reported
		res.CycleStartedAt = provider.CycleStartedAt

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
		if timing := res.TimingSummary(); timing != "" {
			line += " " + timing
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

		// An unmeasured quality load is not a provider error. Authenticated TLS
		// may still have happened before that local failure, and must reach the
		// independent same-URL security projection without inventing a trial.
		if res.Total == 0 {
			if evidence := res.UrlProbeEvidence; evidence != nil && len(evidence.Security) > 0 {
				if err := evidence.ValidateOutcome(res.OkCount, res.Total, res.TlsAuthenticationFailure); err != nil {
					return nil, FailureHealthNotRun, fmt.Errorf("invalid URL security evidence: %w", err)
				}
				log.Print(line + " -- quality unmeasured; publishing independent URL security evidence")
				if err := reportEgressHealth(res); err != nil {
					return nil, FailureSubmit, fmt.Errorf("submit URL security evidence: %w", err)
				}
				return nil, FailureNotMeasured, ErrNotMeasured
			}
			log.Print(line + " -- nothing measured; not submitted")
			return nil, FailureNotMeasured, ErrNotMeasured
		}
		log.Print(line)

		// Submitted only on this path, after a run that measured something.
		// Neither early return above has one, and sending a zero for them would be
		// indistinguishable from a total blackhole -- a false accusation against a
		// provider whose check was skipped for the prober's own exhausted deadline.
		if err := reportEgressHealth(res); err != nil {
			return nil, FailureSubmit, fmt.Errorf("submit health: %w", err)
		}
		return res, "", nil
	}

	// Runs the probe and returns the failure class ("" on success)
	// alongside the error, so the attempt is reported for every outcome.
	probe := func() (string, error) {
		timing.enter(probeTimingOpen)
		var client *http.Client
		var closeTunnel func() error
		var err error
		if self.OpenProvider != nil {
			client, closeTunnel, err = self.OpenProvider(ctx, provider)
		} else if self.Open != nil {
			client, closeTunnel, err = self.Open(ctx, providerClientId)
		} else {
			err = errors.New("prober: no provider tunnel opener")
		}
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
		timing.enter(probeTimingCheck)
		defer func() {
			timing.enter(probeTimingClose)
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

		// Only a successful sampled IP-text response can supply this independent
		// evidence. Its absence, conflict or publication error cannot revoke the
		// website health already acknowledged above.
		if res.ExitIp != "" && !res.ExitObservedAt.IsZero() && self.Submit != nil {
			if err := self.Submit.Submit(ctx, providerClientId, res.ExitIp, res.ExitObservedAt); err != nil {
				if self.locationErr.allow(err, self.maxLoggedDistinctErrors()) {
					log.Printf("prober: optional location submission failed; acknowledged health remains valid: %v", err)
				}
			}
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
	timing.enter(probeTimingAttempt)
	if provider.ClaimOrdinal > 0 {
		completion := qualityprobe.UrlProbeCompletion{
			ClientId: providerClientId, ClaimOrdinal: provider.ClaimOrdinal,
			CompletedAt: time.Now().UTC(), ProbeFailure: failure, AllowPacing: true,
		}
		reporter, ok := self.Attempts.(qualityprobe.UrlProbeCompletionReporter)
		if !ok {
			return errors.Join(err, qualityprobe.ErrUrlProbeCompletionUnsupported)
		}
		return errors.Join(err, reporter.ReportUrlProbeCompletion(ctx, completion))
	}
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
