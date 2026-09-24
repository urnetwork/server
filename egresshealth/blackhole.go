package egresshealth

import (
	"context"
	"math/rand"
	"net/http"
)

// The blackhole check: the cheap hourly question of whether any traffic gets
// through a provider at all, asked of a few connectivity destinations.

// How many destinations one blackhole check asks for.
//
// Three, not one: a single destination conflates "this provider carries no
// traffic" with "this destination is having a bad minute", and the check's
// whole job is to be trusted enough to remove a provider from the public list.
// Three drawn from different operators makes a false positive require three
// independent failures at once -- and, now that each is retried, three
// independent failures of every one of their tries.
//
// Not more than three, because this runs hourly against the entire fleet and
// every added destination multiplies by the population. The rich picture is
// what Check is for; this answers one bit.
const BlackholeSampleSize = 3

// The outcome of one provider's check.
type BlackholeResult struct {
	// True when at least one load passed on any of its attempts and
	// none failed TLS authentication. Ordinary partial reachability is
	// degradation, not a blackhole. A forged certificate is different:
	// accepting the provider after one unrelated endpoint succeeded would
	// knowingly expose clients to an unauthenticated peer.
	Ok bool
	// "" when OK, otherwise a short class suitable for the server's
	// varchar(64): all_destinations_failed, tls_authentication_failed, or
	// not_measured (see FailureNotMeasured).
	Failure string
	// Every destination tried, after its retries, each with its
	// Attempts, LastFailure and NotMeasured, for logging. A caller that
	// reports only the bit throws away the reason.
	Results []CheckResult
	// The address the warm-up's /ip echo saw, "" when it did not
	// answer (IpEchoErr says why) or no echo was configured. It does not
	// decide OK: the check asks whether ordinary destinations are reachable,
	// and a provider could carry the one operator host and nothing else.
	ExitIp    string
	IpEchoErr string
	// How many of the check's loads could not be measured
	// because their tunnel was gone and could not be re-created in time.
	NotMeasured int
	// [connectivity] when fewer connectivity destinations are
	// compatible with the provider's place than the check draws.
	ShortClasses []Class
}

const (
	// Means every sampled destination failed every
	// one of its attempts.
	FailureAllDestinationsFailed = "all_destinations_failed"
	// Means a sampled HTTPS peer -- or the warm-up's,
	// the operator's own api host -- could not authenticate the requested
	// host. It is a hard integrity failure even when a different destination
	// worked.
	FailureTlsAuthentication = "tls_authentication_failed"
	// Means none of the check's loads was measured: its
	// tunnel was gone and could not be re-created within the loads' attempts.
	// Nothing about the provider was learned. It must be submitted as "not
	// measured" -- the server reschedules the check and counts nothing
	// against the provider -- never as a failed check, or a provider that
	// merely disconnected mid-check would start its way to dark.
	FailureNotMeasured = "not_measured"
)

// Answers one question about a provider: did any traffic get
// through.
//
// It exists beside Check rather than inside it because they answer different
// questions on different cadences. Check samples 50 loads across four classes
// to describe how a provider is failing, and sweeping a fleet with it takes
// hours to days. In that window a provider that silently stops forwarding
// keeps its last passing measurement, and every consumer of that measurement
// keeps believing it -- while the provider stays connected and goes on
// accepting clients, because nothing about being dark looks different from the
// outside.
//
// So this is deliberately the cheapest useful check, and since GEOMAP step 7 a
// patient one. It opens with the warm-up (see warmUp), because a check whose
// first and only attempt had to absorb the tunnel's cold start is how half the
// fleet came to read dark (§11.2). Then three connectivity loads, each with
// the retries and spacing of Check, all concurrent: the check takes one round
// when anything answers and up to about fifteen minutes when nothing does. It
// reuses fetch, and therefore the destinations' headers, body caps and checks
// -- a captive portal that answers 200 with its own body fails here exactly as
// it fails a full run, which is the property that makes "something got
// through" mean anything.
//
// Only the connectivity class is drawn -- from Options.Destinations when the
// caller has the server's pool, else the built-in table, and only the entries
// compatible with the provider's place; canaries are never loaded here, since
// a check has no unscored half. Those destinations exist to answer "is there
// internet", they are operated by several independent parties, they return a
// few hundred bytes at most, and they are the least likely in the table to be
// blocked for a reason that has nothing to do with the provider.
//
// With Options.Path, a tunnel that dies part-way is re-created for the loads'
// remaining attempts (see Path), and a check none of whose loads could be
// measured is FailureNotMeasured. A failed check is a failure, not a verdict:
// the server calls a provider dark only after consecutive failed checks
// (§11.3).
func Blackhole(ctx context.Context, client *http.Client, opts Options) *BlackholeResult {
	return blackhole(ctx, client, opts.table(), opts)
}

// The testable form: the destination table is injected.
func blackhole(ctx context.Context, client *http.Client, dests []Destination, opts Options) *BlackholeResult {
	// One generator for the draw and the spacing, as in Check.
	rng := opts.rng()
	compatible, _ := forPlace(dests, opts.ProviderPlace)
	sample := blackholeSample(compatible, rng)

	result := &BlackholeResult{Failure: FailureAllDestinationsFailed}
	if len(sample) < BlackholeSampleSize {
		result.ShortClasses = []Class{ClassConnectivity}
	}
	// Every load at once: three small requests over one tunnel, and the
	// check's deadline should be one load's chain, not three in a row.
	budget := opts.budget(len(sample))
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	exit := &exitRecord{}
	r := newRun(opts.path(client), opts, max(1, len(sample)), budget, rng, exit)
	r.warmUp(ctx)

	result.Results = r.loadAll(ctx, sample)
	var echoTlsFailure bool
	result.ExitIp, _, result.IpEchoErr, echoTlsFailure = exit.read()

	if echoTlsFailure {
		result.Failure = FailureTlsAuthentication
		return result
	}
	sawSuccess, measured := false, 0
	for _, cr := range result.Results {
		if cr.TlsAuthenticationFailure {
			result.Failure = FailureTlsAuthentication
			return result
		}
		if cr.NotMeasured {
			result.NotMeasured++
			continue
		}
		measured++
		if cr.Ok {
			sawSuccess = true
		}
	}
	switch {
	case sawSuccess:
		result.Ok = true
		result.Failure = ""
	case measured == 0 && 0 < result.NotMeasured:
		result.Failure = FailureNotMeasured
	}
	return result
}

// Draws up to BlackholeSampleSize connectivity destinations.
//
// Drawn fresh per run rather than fixed, for the same anti-gaming reason the
// full check samples: a provider that knew the three addresses could carry
// those and blackhole everything else.
func blackholeSample(dests []Destination, r *rand.Rand) []Destination {
	candidates := []Destination{}
	for _, d := range dests {
		if d.Class == ClassConnectivity {
			candidates = append(candidates, d)
		}
	}
	if len(candidates) == 0 {
		return nil
	}

	r.Shuffle(len(candidates), func(i, j int) {
		candidates[i], candidates[j] = candidates[j], candidates[i]
	})
	return candidates[:min(BlackholeSampleSize, len(candidates))]
}

// The closed allowlist a blackhole check over the built-in
// table needs from the provider-tunnel client: its connectivity hosts. The
// standalone command also uses it for its defense-in-depth confinement check.
// fleetprobe has no direct dialer to these hosts, so the taskworker's ordinary
// LAN route cannot satisfy a check when the provider tunnel is dark.
func BlackholeHosts() []string {
	return BlackholeHostsOf(destinations)
}

// Like BlackholeHosts, for any table, such as a pool.
func BlackholeHostsOf(dests []Destination) []string {
	connectivity := []Destination{}
	for _, d := range dests {
		if d.Class == ClassConnectivity {
			connectivity = append(connectivity, d)
		}
	}
	return HostsOf(connectivity)
}
