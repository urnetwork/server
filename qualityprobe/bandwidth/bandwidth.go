// Package bandwidth measures a provider's throughput over a tunnel that is
// already open, against two independent targets.
//
// It is deliberately adaptive rather than fixed-size: a fixed payload is either
// too small to mean anything on a fast link (the whole transfer finishes inside
// TCP slow start) or wastes budget on a slow one. Each measurement streams
// until either bound is hit -- 5 s elapsed or MaxSampleBytes transferred,
// whichever comes first -- and reports the steady-state rate after discarding
// the first 500 ms of slow-start noise.
//
// # Why the measurement is parallel: it used to measure the window, not the link
//
// A single TCP flow cannot exceed (send window / RTT), no matter how fast the
// far end is. connect's MaxWindowSize is scaledPow2WindowSize(mib(1), ...), so
// one flow through a provider's tunnel ceilings at 1 MiB / RTT -- 11.2 MiB/s at
// the fleet's ~89 ms.
//
// The single-stream version of this package measured exactly that ceiling and
// nothing else. Across the beta fleet, bandwidth-delay product (measured
// throughput x measured RTT = bytes in flight) came out at ~1 MiB for eleven of
// twelve providers:
//
//	mib_s   rtt_ms   bdp_mib          mib_s   rtt_ms   bdp_mib
//	 20.1      89     1.788            12.4      84     1.043
//	 19.8      82     1.624            10.5      76     0.801
//	 19.6      89     1.740             9.9      89     0.881
//	 18.8      89     1.671             9.1     100     0.905
//	 15.2      79     1.199            14.8      76     1.128
//	 13.1      76     0.992
//
// Eleven independent providers on eleven different hosts do not coincidentally
// have capacity equal to one window divided by their own RTT. Confirmed against
// a box under our control: a German provider does 79 MB/s measured directly on
// the host and the single-stream probe reported 4.8 MB/s through the tunnel.
//
// The fix is N parallel streams: one flow gets one window, N flows get N
// windows. This is why Cloudflare and Ookla use 4-16 connections. Raising
// connect's MaxWindowSize is not the fix -- that is the data path every real
// user rides, and a far larger decision than this probe.
//
// # One TCP connection per stream is load-bearing
//
// N streams only buy N windows if they are N transport connections. Multiplexed
// over one HTTP/2 connection they share one window and the probe measures
// exactly what it measured before, with every test still green. The tunnel's
// client (providertunnel.httpClientOverDialerWithHosts) guarantees this: it
// offers no ALPN protocols and sets DisableKeepAlives, so every request is its
// own HTTP/1.1 connection. Any *http.Client handed to this package must have
// the same property.
//
// # Two targets, never averaged
//
// A measurement is taken separately against the operator's own endpoint and
// against a public CDN, and the two figures are stored and logged separately.
// That separation is the entire point of having two targets: a provider that
// prioritises one path and not the other is invisible in a single figure and
// obvious in two. Averaging them destroys exactly the signal they exist to
// produce, so nothing in this package ever combines them.
//
// Both targets take the identical URL shape (`<url>?bytes=N`) and go through
// the identical measurement code, so neither figure is advantaged by a
// different request shape, redirect behaviour or transfer encoding.
package bandwidth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// How many parallel connections one measurement opens
	// against one target. It is a constant, not a knob, and it is the only
	// thing that sets the fan-out: the measurement opens exactly this many
	// streams and there is no path in this package to more.
	//
	// Why 8. The per-stream ceiling is one connect window over the RTT --
	// 1 MiB / 89 ms = 11.2 MiB/s at the fleet's median. Streams multiply it:
	//
	//	streams   ceiling at 89 ms RTT
	//	      1   11.2 MiB/s
	//	      4   44.9 MiB/s
	//	      8   89.9 MiB/s
	//	     16  179.8 MiB/s
	//
	// 8 puts the ceiling above the fastest capacity we have independently
	// confirmed on a real provider (79 MB/s, measured on the host itself), so
	// the figure is the provider's rather than the probe's. 16 would double
	// both the ceiling and the cost for no provider we can currently show is
	// being clipped.
	//
	// Cost of the server side of this: the api serves StreamCount transfers
	// per probing worker, and the prober runs -concurrency provider tunnels at
	// once. Beta runs -concurrency=2, so the worst case is 8 x 2 = 16
	// simultaneous transfers. That figure scales with -concurrency, not with
	// fleet size.
	StreamCount = 8

	// What each stream requests and is capped at. It must stay
	// at or below the operator download endpoint's per-request clamp
	// (handlers.maxProviderBandwidthTestBytes), or the endpoint truncates the
	// response and the aggregate falls short of what was reserved for it.
	//
	// 2 MiB per stream also keeps the transfer long enough to clear the
	// warmup window on ordinary providers: 16 MiB aggregate takes longer than
	// WarmupDuration for anything under 32 MiB/s, where the old 5 MiB single
	// stream crossed that line at 10 MiB/s and therefore fell back to the
	// lower-bound path on most of the fleet. (The steady window must also be
	// a large enough fraction of the transfer -- see MaxSteadyInflation --
	// but that bound is relative, so it does not move this crossover.)
	StreamBytes = 2 * 1024 * 1024

	// Bounds one measurement across all of its streams. It is
	// the figure the server's byte budget reserves against
	// (model.MaxProviderBandwidthBytesPerProbe), so a measurement can never
	// transfer more than it reserved -- the two must be changed together or
	// the budget under-counts, which is worse than having no budget.
	//
	// 8 x 2 MiB = 16 MiB per target, 32 MiB per provider across both targets,
	// 1.25 GiB for a full 40-provider sweep -- 0.36 MiB/s averaged over the
	// hour a sweep is spread across, against a measured 28-120 MB/s uplink.
	MaxSampleBytes = StreamCount * StreamBytes

	// Discarded from the head of a measurement. A TCP
	// connection opens in slow start, so bytes carried in the first few round
	// trips arrive at a fraction of the link's steady rate; counting them
	// depresses the figure by an amount that depends on RTT rather than on
	// throughput, which would systematically penalise distant providers.
	WarmupDuration = 500 * time.Millisecond

	// Bounds how much of the transfer's wall clock the
	// steady window may exclude: the window must cover at least
	// 1/MaxSteadyInflation of it, or the figure falls back to the
	// warmup-inclusive lower bound.
	//
	// Excluding the warmup can raise the reported rate by at most
	// totalElapsed/steadyElapsed -- the case where the discarded warmup
	// carried nothing -- so this is directly a bound on how far a "steady"
	// figure may exceed the whole-transfer aggregate, which is the physical
	// ceiling for that transfer. A transfer stalling across the warmup
	// boundary and then bursting (a windowed tunnel transport refilling just
	// after the boundary) is the shape that exceeds it; review measured 17x
	// the true aggregate published as steady.
	//
	// 4 leaves real slow-start exclusion intact -- a warmup carrying a
	// quarter of the transfer's average still passes -- while capping the
	// residual error at 4x rather than the unbounded original.
	//
	// It is deliberately a ratio and not a duration. Solving
	// T/(T-WarmupDuration) <= 4 for the 16 MiB transfer gives a steady-path
	// ceiling of ~24 MiB/s (measured ~21 with real overhead), against ~16
	// MiB/s for a fixed 500 ms floor. Above that the reported figure is the
	// warmup-inclusive lower bound. The pre-bound ceiling of ~32 MiB/s was
	// partly illusory: at 31 MiB/s the steady window is already under 20 ms,
	// and dividing 16 MiB by a window that thin is the inflation this bound
	// exists to stop, not a measurement worth keeping.
	MaxSteadyInflation = 4

	// The per-target wall-clock cap.
	DefaultTimeout = 5 * time.Second

	// The absolute floor on the remaining context budget
	// worth starting a measurement with. Below it the request would be cut off
	// mid-transfer and the resulting figure would describe the prober's
	// exhausted deadline rather than the provider.
	//
	// It is a floor, not the whole test: Sampler.Sample requires the
	// measurement's own per-target cap when that is larger, since a
	// reservation spent on a measurement the parent deadline will cut short
	// charges the fleet's byte budget for nothing.
	MinTimeBudget = time.Second
)

// The source tags a measurement is stored under. These are the wire values of
// the server's model.ProviderBandwidthSourceActiveOperator /
// ...ActiveCDN and must stay in step with them: the result endpoint validates
// the submitted source against its known set and rejects anything else.
const (
	SourceOperator = "active-operator"
	SourceCdn      = "active-cdn"
)

// The second target: Cloudflare's public speed-test download,
// size-parameterised exactly like the operator's own endpoint, so both targets
// share one URL shape and one code path.
//
// Two alternatives were measured and rejected. proof.ovh.net answered at
// ~1.0 MB/s from a datacenter host, which would dominate the per-provider time
// budget and measure the target rather than the provider.
// cachefly.cachefly.net/10mb.test is fast (~81 MB/s) but fixed-size with no
// byte parameter, so it could not share this package's byte cap.
const CdnTestUrl = "https://speed.cloudflare.com/__down"

// The reasons a target can be skipped. They are distinct strings on purpose: a
// provider skipped because the deployment's hourly byte budget is spent is a
// normal, expected outcome that says the budget is working, while one skipped
// because the probe ran out of time says the probe's own deadline is too tight.
// Collapsing them into one "skipped" would hide the second behind the first.
const (
	SkipNoBudget    = "no byte budget this hour"
	SkipNoTime      = "no time left in this probe"
	SkipUnsupported = "server has no bandwidth endpoints"
)

// Reports that the deployment's active-probe byte budget for the
// current hour is spent. A Reserver returns it (wrapped or not) when the
// server answers 429; it is a clean skip, not a probe failure.
var ErrNoBudget = errors.New("bandwidth: no active-probe byte budget for this hour")

// Reports that the server does not implement the bandwidth
// endpoints (404). Like ErrNoBudget it is a clean skip: a prober running
// against a deployment that has not shipped them yet keeps probing health,
// it simply records no bandwidth -- rather than logging a failure
// per provider, per pass, forever.
var ErrUnsupported = errors.New("bandwidth: the server does not implement the provider bandwidth endpoints")

// Reports that nothing measurable was transferred, so no rate can
// be computed. Distinct from a transport error: the request itself may have
// succeeded and simply delivered nothing.
var ErrNoSample = errors.New("bandwidth: no bytes transferred, cannot compute a rate")

// Reports that bytes did move but the wall clock did
// not observably advance while they did, so no rate can be computed. This is
// unreachable where the prober deploys (Linux clocks have nanosecond
// granularity) but real on Windows dev machines, whose monotonic clock ticks
// at ~0.5ms -- a loopback transfer can complete inside one tick. It is kept
// distinct from ErrNoSample because "no bytes" and "no time" point at
// opposite problems, and a message claiming no bytes moved when 16 MiB did
// made this failure expensive to diagnose.
var ErrUnmeasuredDuration = errors.New("bandwidth: bytes transferred but the clock did not advance, cannot compute a rate")

// One thing to download from. Both targets in production carry the
// same URL shape and differ only in host, source tag, and whether an operator
// secret is attached.
type Target struct {
	// The short label used in the log line ("operator", "cdn").
	Name string
	// The tag the figure is stored under server-side.
	Source string
	// The base download url; the byte count is appended as a `bytes`
	// query parameter.
	Url string
	// Carries per-target request headers (the operator secret, for the
	// operator target). Never logged.
	Header http.Header
}

// One measurement.
type Sample struct {
	// The aggregate rate across all streams: the total bytes
	// they moved inside one common wall-clock window, divided by that window.
	// Per-stream rates are never summed -- see measure for why that would
	// inflate the figure.
	BytesPerSecond float64
	// The total across all streams.
	SampleByteCount int64
	// How many streams contributed, always StreamCount for a
	// successful measurement. Recorded so a figure can be read back against
	// the fan-out that produced it.
	Streams int
	// Reports whether the rate was computed over the
	// steady-state window only. It is false in two cases -- the whole
	// transfer finished inside WarmupDuration (16 MiB completes in under
	// 500 ms above ~32 MiB/s, which the fastest datacenter-hosted providers
	// clear), or the steady window covered too small a fraction of the
	// transfer to describe it (see MaxSteadyInflation) -- in which case the
	// rate is computed over the full transfer instead and is a lower bound on
	// the real throughput, because it includes slow start.
	//
	// This flag does not reach the server: ingest submits only the rate and
	// the byte count, so a lower-bound figure is stored indistinguishably
	// from a steady one. That is a gap worth closing on the server side --
	// until it is, the distinction lives only in this process's log line.
	//
	// Reporting a lower bound rather than zero is deliberate: returning zero
	// here would make the fastest providers, the ones most worth measuring,
	// the only ones that produce no usable figure at all (the server rejects a
	// non-positive rate outright).
	WarmupExcluded bool
	Elapsed        time.Duration
}

// Builds the operator-endpoint target. The operator secret is
// carried in the same X-UR-Operator-Secret header the other operator routes
// use.
//
// publicServerUrl is not the same value as the -api-url the prober uses for
// its control-plane calls, and conflating the two is a real bug that was
// deployed once: the control-plane url is reached prober -> api directly, so
// on a docker deployment it is an internal name like http://api:8080, but
// this request travels prober -> platform -> provider -> internet -> back to
// the api, so it must be the address the api answers on from the public
// internet. An internal name fails the same way for every provider
// ("context deadline exceeded"), which reads like a fleet-wide provider fault
// and is not one.
func OperatorTarget(publicServerUrl string, operatorSecret string) Target {
	h := http.Header{}
	h.Set("X-UR-Operator-Secret", operatorSecret)
	return Target{
		Name:   "operator",
		Source: SourceOperator,
		Url:    strings.TrimRight(publicServerUrl, "/") + "/network/provider-bandwidth-test",
		Header: h,
	}
}

// Builds the public-CDN target. No credentials: it is an ordinary
// public download.
func CdnTarget() Target {
	return Target{Name: "cdn", Source: SourceCdn, Url: CdnTestUrl}
}

// The production pair, in log order. An empty
// publicServerUrl drops the operator target rather than emitting one that
// cannot resolve from the far side of the tunnel: a target that fails for
// every provider is worse than no target, because it looks like a finding.
func DefaultTargets(publicServerUrl string, operatorSecret string) []Target {
	if strings.TrimSpace(publicServerUrl) == "" {
		return []Target{CdnTarget()}
	}
	return []Target{OperatorTarget(publicServerUrl, operatorSecret), CdnTarget()}
}

// Returns the hostnames the targets are reached at, for the
// tunnel's host allowlist (providertunnel.Tunnel.HttpClientForHosts). A host
// that is neither pinned nor allowlisted is refused by the tunnel dialer, so
// the measurement would fail before it started without this.
func TargetHosts(targets []Target) []string {
	hosts := []string{}
	for _, t := range targets {
		u, err := url.Parse(t.Url)
		if err != nil || u.Hostname() == "" {
			continue
		}
		hosts = append(hosts, u.Hostname())
	}
	return hosts
}

// Appends the byte count to the target url. Both targets are
// size-parameterised the same way, which is why they can share this.
func sizedUrl(rawUrl string, byteCount int64) (string, error) {
	u, err := url.Parse(rawUrl)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("bytes", strconv.FormatInt(byteCount, 10))
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// Streams from testUrl and reports the aggregate throughput of
// StreamCount parallel streams. It stops at MaxSampleBytes or at timeout,
// whichever comes first.
//
// This is the plain-url form used by tests and one-off diagnostics;
// MeasureTarget is what production uses, and both run the same code beneath --
// including the parallelism, so a diagnostic run reports the same figure the
// fleet does.
func Measure(ctx context.Context, client *http.Client, testUrl string, timeout time.Duration) (float64, int64, error) {
	sample, err := measure(ctx, client, testUrl, nil, timeout)
	return sample.BytesPerSecond, sample.SampleByteCount, err
}

// Measures one target with StreamCount parallel streams of
// StreamBytes each, which is MaxSampleBytes in total.
func MeasureTarget(ctx context.Context, client *http.Client, target Target, timeout time.Duration) (Sample, error) {
	return measure(ctx, client, target.Url, target.Header, timeout)
}

// One parallel connection's state and outcome.
type stream struct {
	resp *http.Response
	err  error
	// What this stream read, capped at StreamBytes.
	total int64
	// The part of total that landed at or after the common
	// warmup boundary.
	steadyBytes int64
	// When this stream's first read at or after the warmup
	// boundary completed. The aggregate window opens at the earliest of these
	// across all streams -- see measure.
	steadyStart time.Time
	// When this stream's last read completed.
	end time.Time
}

// Runs StreamCount streams against testUrl at once and reports their
// aggregate rate.
//
// # Why the window is common to all streams
//
// The figure is (bytes all streams moved inside one wall-clock window) / (that
// window). Per-stream rates are deliberately not computed and summed: a stream
// that finishes early has a short denominator of its own, so its rate is high,
// and adding it to streams that are still running reports throughput the link
// never simultaneously carried.
//
// The window opens at the first read, on any stream, that completes at or
// after start+WarmupDuration -- where start is taken after every stream has
// its response headers (see the barrier below), so connect, TLS and the
// request round-trip are excluded for all streams uniformly.
//
// It opens at that first read rather than at the boundary instant itself
// because a link that is still stalled when the boundary passes has not
// started its steady state yet, and charging it for the gap would report a
// rate for a period in which the link was, by construction, not the thing
// being measured. The single-stream version of this package behaved the same
// way; making the boundary a fixed instant instead quietly changed the figure
// on any target that pauses across it.
//
// The window closes at the last stream's final read. A straggler therefore
// leaves a tail during which fewer than StreamCount streams were running,
// which pulls the aggregate down. That is the safe direction and it is why
// this is preferred over closing the window at the first stream's completion:
// closing early would need in-flight reads cancelled, and would then have to
// tell "cancelled because we chose to stop" apart from "cancelled because the
// probe was abandoned", muddying the one classification below that has to stay
// exactly right.
func measure(
	ctx context.Context,
	client *http.Client,
	testUrl string,
	header http.Header,
	timeout time.Duration,
) (Sample, error) {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	// Every stream asks for the same size. The URL shape is identical across
	// both targets, so this is the one place the byte count is set.
	sizedTargetUrl, err := sizedUrl(testUrl, StreamBytes)
	if err != nil {
		return Sample{}, err
	}
	// parent is kept so a read cut short by this measurement's own time cap can
	// be told apart from one cut short by the probe being cancelled: the former
	// is the designed stopping condition, the latter is not a measurement.
	parent := ctx
	// WithTimeout never extends an earlier parent deadline, so a probe whose
	// own budget is nearly spent still cannot be overrun by this. One deadline
	// is shared by all streams, so they stop together.
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if client == nil {
		client = http.DefaultClient
	}

	// measureStart is the wall clock the time cap is measured against, so the
	// cap bounds the whole measurement including connection setup rather than
	// only the reading part.
	measureStart := time.Now()

	// Issues one stream's request and leaves the body open and unread, ready
	// for the barrier to release.
	openStream := func(s *stream) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, sizedTargetUrl, nil)
		if err != nil {
			return err
		}
		for k, values := range header {
			for _, v := range values {
				req.Header.Add(k, v)
			}
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			resp.Body.Close()
			return fmt.Errorf(
				"bandwidth: %s answered %d: %s",
				sizedTargetUrl, resp.StatusCode, strings.TrimSpace(string(msg)),
			)
		}
		s.resp = resp
		return nil
	}

	// Fixed-size, indexed by stream: the fan-out is StreamCount and nothing
	// here can grow it.
	streams := make([]stream, StreamCount)
	var setup sync.WaitGroup
	var done sync.WaitGroup
	// gun is the barrier. Every stream blocks on it after client.Do returns
	// and before its first Body.Read, so the shared clock starts when all of
	// them are actually ready to move bytes.
	gun := make(chan struct{})
	// Written before close(gun) and read only after <-gun, so the channel
	// close carries the happens-before.
	var start time.Time
	var steadyFrom time.Time
	abort := false

	// Drains one stream up to StreamBytes, recording what it moved and how
	// much of that landed inside the common steady-state window. It runs only
	// after <-gun, so it reads steadyFrom after close(gun) published it.
	readStream := func(s *stream) error {
		buf := make([]byte, 32*1024)
		for {
			// Read into no more than this stream's remaining allowance. Reading a
			// whole buffer and checking afterwards overshoots by up to one buffer
			// per stream -- with StreamCount streams that is StreamCount buffers
			// past a cap that the server already reserved budget against, on every
			// measurement. The cap is therefore enforced on the remaining
			// allowance, not per read.
			readBuf := buf
			if remaining := int64(StreamBytes) - s.total; remaining < int64(len(readBuf)) {
				readBuf = readBuf[:remaining]
			}
			n, readErr := s.resp.Body.Read(readBuf)
			now := time.Now()
			s.end = now
			s.total += int64(n)
			// A read that completes at or after the common warmup boundary counts
			// towards the steady-state window. The read that first crosses the
			// boundary carries a fraction of one 32 KiB buffer from before it,
			// which is under a thousandth of the sample.
			if !now.Before(steadyFrom) {
				if s.steadyStart.IsZero() {
					s.steadyStart = now
				}
				s.steadyBytes += int64(n)
			}
			if StreamBytes <= s.total {
				return nil
			}
			if readErr == io.EOF {
				return nil
			}
			if readErr != nil {
				if errors.Is(readErr, context.DeadlineExceeded) && parent.Err() == nil {
					// the time cap fired while a read was in flight. That is this
					// measurement's designed stopping condition, reached a few
					// milliseconds before the loop's own check would have reached
					// it, so the bytes already timed are a valid (smaller) sample
					// rather than a failure. Every stream shares the one deadline,
					// so they all take this path together.
					return nil
				}
				// Anything else -- including the probe itself being cancelled --
				// leaves a window shaped by something other than the link, so no
				// figure is reported.
				return readErr
			}
			if timeout <= now.Sub(measureStart) {
				return nil
			}
		}
	}

	for i := range streams {
		setup.Add(1)
		done.Add(1)
		go func(s *stream) {
			defer done.Done()
			s.err = openStream(s)
			setup.Done()

			<-gun
			if s.resp == nil {
				return
			}
			defer s.resp.Body.Close()
			if abort || s.err != nil {
				return
			}
			s.err = readStream(s)
		}(&streams[i])
	}

	setup.Wait()
	for i := range streams {
		if streams[i].err != nil {
			abort = true
			break
		}
	}
	start = time.Now()
	steadyFrom = start.Add(WarmupDuration)
	close(gun)
	done.Wait()

	var total int64
	var steadyBytes int64
	var windowStart time.Time
	var windowEnd time.Time
	var firstErr error
	for i := range streams {
		total += streams[i].total
		steadyBytes += streams[i].steadyBytes
		if windowEnd.Before(streams[i].end) {
			windowEnd = streams[i].end
		}
		if steadyStart := streams[i].steadyStart; !steadyStart.IsZero() {
			if windowStart.IsZero() || steadyStart.Before(windowStart) {
				windowStart = steadyStart
			}
		}
		if firstErr == nil {
			firstErr = streams[i].err
		}
	}
	if firstErr != nil {
		// One stream's failure fails the measurement. A partial aggregate is
		// not the figure this reports: it would be N-minus-something windows'
		// worth of throughput published as if it were N. The byte count is
		// still the true total across every stream, so the caller is not told
		// a smaller number moved than actually did.
		return Sample{SampleByteCount: total, Streams: StreamCount}, firstErr
	}

	if total <= 0 {
		return Sample{}, ErrNoSample
	}

	totalElapsed := windowEnd.Sub(start)
	if totalElapsed <= 0 {
		return Sample{SampleByteCount: total, Streams: StreamCount}, ErrUnmeasuredDuration
	}

	// The steady window must be a representative fraction of the transfer,
	// not merely wider than some fixed duration.
	//
	// Excluding the warmup can only raise the reported rate by the factor
	// totalElapsed/steadyElapsed (the extreme where the discarded warmup
	// carried no bytes at all), so bounding that ratio bounds how far a
	// steady figure may exceed the whole-transfer aggregate -- which is the
	// physical ceiling, since those bytes demonstrably moved in that wall
	// clock. A stall straddling the warmup boundary followed by a burst is
	// exactly the shape that blows past it: the tail's bytes divided by the
	// tail's spread. Reported inflation is now bounded at
	// MaxSteadyInflation whatever the stall's width, where an absolute floor
	// only caught stalls whose tail happened to be narrow.
	//
	// A relative bound also costs nothing at the top of the range, which an
	// absolute one did: a healthy 20 MiB/s provider moves 16 MiB in ~800ms
	// and keeps a ~300ms steady window, comfortably inside the ratio. That
	// band is the first one the parallel-stream rewrite unlocked, so giving
	// it back to the lower-bound path would have undone the point of it.
	steadyElapsed := windowEnd.Sub(windowStart)
	representative := steadyWindowIsRepresentative(
		!windowStart.IsZero(),
		steadyBytes,
		totalElapsed,
		steadyElapsed,
	)
	if representative {
		return Sample{
			BytesPerSecond:  float64(steadyBytes) / steadyElapsed.Seconds(),
			SampleByteCount: total,
			Streams:         StreamCount,
			WarmupExcluded:  true,
			Elapsed:         steadyElapsed,
		}, nil
	}

	// Every stream finished inside the warmup window, or the post-warmup
	// window was too small a fraction of the transfer to describe it. Rather
	// than report no rate at all -- which would silently exclude the fastest
	// providers, the ones most worth measuring -- fall back to the
	// warmup-inclusive aggregate over the full transfer. That figure includes
	// slow start, so it understates the link: it is a lower bound, and
	// WarmupExcluded=false says so. It must stay honest when it happens.
	return Sample{
		BytesPerSecond:  float64(total) / totalElapsed.Seconds(),
		SampleByteCount: total,
		Streams:         StreamCount,
		WarmupExcluded:  false,
		Elapsed:         totalElapsed,
	}, nil
}

// Reports whether the steady window is a representative fraction of the
// transfer (see MaxSteadyInflation). Kept as a pure duration predicate so the
// boundary is testable without scheduler timing. Tests that manufacture a
// stall with time.Sleep are still useful end-to-end coverage, but under the
// race detector their paced tail legitimately widens across this threshold;
// wall-clock scheduling must not decide which side of the contract a unit test
// believes it exercised.
func steadyWindowIsRepresentative(
	hasWindowStart bool,
	steadyBytes int64,
	totalElapsed time.Duration,
	steadyElapsed time.Duration,
) bool {
	return hasWindowStart &&
		0 < steadyBytes &&
		0 < steadyElapsed &&
		totalElapsed <= time.Duration(MaxSteadyInflation)*steadyElapsed
}

// Takes deployment-wide byte budget for one active probe. In
// production this is *ingest.Client, which posts to
// /network/provider-bandwidth-reserve and maps the server's 429 to
// ErrNoBudget.
type Reserver interface {
	ReserveBandwidth(ctx context.Context, providerClientId string, byteCount int64) error
}

// Records one measured figure under its own source tag.
type Submitter interface {
	SubmitBandwidth(
		ctx context.Context,
		providerClientId string,
		source string,
		bytesPerSecond float64,
		sampleByteCount int64,
	) error
}

// One target's outcome for one provider.
type Result struct {
	Target Target
	Sample Sample
	// "" when the target was measured, otherwise the reason it was
	// not (SkipNoBudget, SkipNoTime).
	Skip string
	Err  error
}

// Reports whether this target produced a usable figure.
func (self Result) Measured() bool {
	return self.Skip == "" && self.Err == nil && 0 < self.Sample.BytesPerSecond
}

// Measures every target for one provider, over a tunnel that is
// already open, reserving budget for each separately.
//
// Budget is reserved per target, not once per provider: each target pulls its
// own MaxSampleBytes through the provider's tunnel, which is real paid
// contract traffic in both cases, and the reservation endpoint clamps a single
// request to the per-probe figure anyway. Reserving per target also makes the
// skip granular -- the second target can be skipped for budget while the first
// still produced a figure -- instead of losing both.
type Sampler struct {
	Targets []Target
	Reserve Reserver
	Submit  Submitter
	// The per-target wall-clock cap; zero means DefaultTimeout.
	Timeout time.Duration
}

// Measures every target and submits each figure separately. It never
// returns an error: a bandwidth measurement is a diagnostic riding along on a
// probe whose product is the health run and the exit address, and no outcome
// here may fail that probe.
//
// Nothing is measured before its budget is reserved, and nothing is combined:
// there is one Result per target, in Targets order.
func (self *Sampler) Sample(ctx context.Context, providerClientId string, client *http.Client) []Result {
	timeout := self.timeout()
	// Reports whether enough of the probe's deadline remains to take a
	// measurement that describes the provider rather than the deadline.
	//
	// The need is the measurement's own per-target cap, not a fixed floor. A
	// floor below that cap admits a measurement the parent deadline will cut
	// short: with 2s left and a 5s cap the reservation (16 MiB of
	// deployment-wide byte budget) is spent, measure correctly classifies the
	// parent's deadline as a failure, and nothing is recorded -- the provider
	// then logs failed(context deadline exceeded) instead of the SkipNoTime
	// this exact situation has a dedicated string for.
	//
	// Note that the shipped wiring cannot currently reach that state: nothing
	// on cmd/egress-prober's path puts a deadline on the context it hands the
	// scheduler (-probe-timeout bounds the http.Client and the per-source
	// lookups, not the pass -- see the comment on egressHealthOptions), so
	// ctx.Deadline() reports none and this returns true regardless of the
	// need. The guard is here for the day a per-provider deadline is added,
	// which is exactly when getting it wrong would start costing byte budget
	// silently. If that day comes, weigh requiring the full cap against
	// skipping fast providers that would have finished 16 MiB well inside it.
	hasTimeBudget := func() bool {
		if ctx.Err() != nil {
			return false
		}
		need := max(timeout, MinTimeBudget)
		deadline, ok := ctx.Deadline()
		if !ok {
			return true
		}
		return need <= time.Until(deadline)
	}
	sampleOne := func(target Target) Result {
		// The probe's remaining budget is checked before the reservation, not
		// after: a reservation spends deployment-wide budget, and spending it
		// on a measurement that cannot finish would charge the fleet for
		// nothing.
		if !hasTimeBudget() {
			return Result{Target: target, Skip: SkipNoTime}
		}

		if self.Reserve != nil {
			if err := self.Reserve.ReserveBandwidth(ctx, providerClientId, MaxSampleBytes); err != nil {
				if errors.Is(err, ErrNoBudget) {
					return Result{Target: target, Skip: SkipNoBudget}
				}
				if errors.Is(err, ErrUnsupported) {
					return Result{Target: target, Skip: SkipUnsupported}
				}
				// Any other reservation failure (server down, wrong secret)
				// means the transfer was never accounted for, so it must not
				// happen.
				return Result{Target: target, Err: err}
			}
		}

		sample, err := MeasureTarget(ctx, client, target, timeout)
		if err != nil {
			return Result{Target: target, Sample: sample, Err: err}
		}
		if sample.BytesPerSecond <= 0 {
			return Result{Target: target, Sample: sample, Err: ErrNoSample}
		}

		if self.Submit != nil {
			if err := self.Submit.SubmitBandwidth(
				ctx, providerClientId, target.Source, sample.BytesPerSecond, sample.SampleByteCount,
			); err != nil {
				return Result{Target: target, Sample: sample, Err: err}
			}
		}
		return Result{Target: target, Sample: sample}
	}

	results := make([]Result, 0, len(self.Targets))
	for _, target := range self.Targets {
		results = append(results, sampleOne(target))
	}
	return results
}

// Returns the per-target cap, or DefaultTimeout when unset.
func (self *Sampler) timeout() time.Duration {
	if 0 < self.Timeout {
		return self.Timeout
	}
	return DefaultTimeout
}

// Renders one provider's results as `operator=12.4MB/s cdn=11.8MB/s`,
// per target and in Targets order. The two figures are always printed side by
// side and never combined, so a provider prioritising one path is visible at a
// glance -- which is the whole reason there are two targets.
//
// A skipped target says so explicitly rather than being omitted: a silently
// missing figure reads as "nothing was measured" when the truth is "the budget
// this deployment set was reached", and those need different responses.
func Summary(results []Result) string {
	parts := make([]string, 0, len(results))
	for _, r := range results {
		switch {
		case r.Skip != "":
			parts = append(parts, fmt.Sprintf("%s=skipped(%s)", r.Target.Name, r.Skip))
		case r.Err != nil:
			parts = append(parts, fmt.Sprintf("%s=failed(%s)", r.Target.Name, r.Err))
		default:
			figure := fmt.Sprintf("%s=%.1fMB/s", r.Target.Name, r.Sample.BytesPerSecond/(1024*1024))
			if !r.Sample.WarmupExcluded {
				// the transfer finished inside the warmup window, so this
				// figure includes slow start and understates the link
				figure += "(lower-bound)"
			}
			parts = append(parts, figure)
		}
	}
	return strings.Join(parts, " ")
}
