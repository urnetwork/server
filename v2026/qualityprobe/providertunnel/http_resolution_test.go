// Resolution retries precede sockets and remain inside one request owner.
package providertunnel

import (
	"context"
	"errors"
	"net"
	"net/http/httptrace"
	"net/netip"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// Two transient lookups cannot turn a subsequently reachable URL into failure.
func TestProviderResolutionRetriesBeforeSocket(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		queries, dials := 0, 0
		target := netip.MustParseAddr("192.0.2.15")
		left, right := net.Pipe()
		defer left.Close()
		defer right.Close()
		resolver := &providerUrlResolver{
			query: func(ctx context.Context, kind, host string) ([]netip.Addr, bool) {
				queries++
				if kind != "A" || host != "sample.example" || httptrace.ContextClientTrace(ctx) != nil {
					t.Error("resolver escaped target/family/trace boundary")
				}
				if queries < 3 {
					return nil, false
				}
				return []netip.Addr{target}, true
			},
			dial: func(_ context.Context, network, host string, addrs []netip.Addr) (net.Conn, error) {
				dials++
				if queries != 3 || host != "sample.example:443" || network != "tcp" || len(addrs) != 1 || addrs[0] != target {
					return nil, errors.New("wrong resolution handoff")
				}
				return left, nil
			},
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		ctx, observation := traceProviderHttpDial(ctx, "tcp", "sample.example:443")
		conn, err := resolver.dialContext(ctx, "tcp", "sample.example:443", observation)
		observation.finish(err)
		if err != nil || conn != left || queries != 3 || dials != 1 {
			t.Fatalf("retry handoff queries=%d dials=%d err=%v", queries, dials, err)
		}
	})
}

// A never-answering resolver gets finite retries, no target socket, and no
// borrowed HTTP trace; the hard outer deadline cannot be renewed by a retry.
func TestProviderResolutionTimeoutsStayBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		queries := 0
		resolver := &providerUrlResolver{
			query: func(ctx context.Context, _, _ string) ([]netip.Addr, bool) {
				queries++
				<-ctx.Done()
				return nil, false
			},
			dial: func(context.Context, string, string, []netip.Addr) (net.Conn, error) {
				t.Error("unresolved target reached socket phase")
				return nil, errors.New("unexpected dial")
			},
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		ctx, observation := traceProviderHttpDial(ctx, "tcp", "sample.example:443")
		start := time.Now()
		_, err := resolver.dialContext(ctx, "tcp", "sample.example:443", observation)
		if stage := observation.finish(err); err == nil || stage != "dial_dns" || queries != 3 || time.Since(start) > 10*time.Second {
			t.Fatalf("unbounded/misattributed resolution: queries=%d duration=%s stage=%s err=%v", queries, time.Since(start), stage, err)
		}
	})
}

// Repeating a cached authoritative negative cannot discover anything new.
// The sampled load's existing spaced retries retain the recovery opportunity.
func TestProviderResolutionAuthoritativeMissDoesNotAmplify(t *testing.T) {
	queries := 0
	resolver := &providerUrlResolver{
		query: func(context.Context, string, string) ([]netip.Addr, bool) { queries++; return nil, true },
		dial: func(context.Context, string, string, []netip.Addr) (net.Conn, error) {
			t.Error("negative answer dialed")
			return nil, errors.New("unexpected dial")
		},
	}
	ctx, observation := traceProviderHttpDial(context.Background(), "tcp", "sample.example:443")
	_, err := resolver.dialContext(ctx, "tcp", "sample.example:443", observation)
	if stage := observation.finish(err); queries != 1 || err == nil || stage != "dial_dns" {
		t.Fatalf("negative amplified: queries=%d stage=%s err=%v", queries, stage, err)
	}
}

// One timed-out lookup leaves bounded time for a later answer and one socket.
func TestProviderResolutionRecoversAfterLookupTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		queries, dials := 0, 0
		target := netip.MustParseAddr("192.0.2.21")
		left, right := net.Pipe()
		defer left.Close()
		defer right.Close()
		resolver := &providerUrlResolver{
			query: func(ctx context.Context, _, _ string) ([]netip.Addr, bool) {
				queries++
				if queries == 1 {
					<-ctx.Done()
					return nil, false
				}
				return []netip.Addr{target}, true
			},
			dial: func(context.Context, string, string, []netip.Addr) (net.Conn, error) {
				dials++
				return left, nil
			},
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		ctx, observation := traceProviderHttpDial(ctx, "tcp", "sample.example:443")
		start := time.Now()
		conn, err := resolver.dialContext(ctx, "tcp", "sample.example:443", observation)
		observation.finish(err)
		if err != nil || conn != left || queries != 2 || dials != 1 || time.Since(start) >= 10*time.Second {
			t.Fatalf("lookup recovery lost: queries=%d dials=%d elapsed=%s err=%v", queries, dials, time.Since(start), err)
		}
	})
}

// A cold tunnel may become usable after thirty seconds but still inside the
// request's explicit sixty-second establishment allowance. DNS retries must
// share that allowance, not discard its latter half before the path exists.
func TestProviderResolutionUsesColdStartAllowance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		queries, dials := 0, 0
		ready := time.After(35 * time.Second)
		target := netip.MustParseAddr("192.0.2.35")
		left, right := net.Pipe()
		defer left.Close()
		defer right.Close()
		resolver := &providerUrlResolver{
			query: func(ctx context.Context, _, _ string) ([]netip.Addr, bool) {
				queries++
				select {
				case <-ready:
					return []netip.Addr{target}, true
				case <-ctx.Done():
					return nil, false
				}
			},
			dial: func(ctx context.Context, _, _ string, addrs []netip.Addr) (net.Conn, error) {
				dials++
				deadline, bounded := ctx.Deadline()
				if !bounded || time.Until(deadline) < 15*time.Second || len(addrs) != 1 || addrs[0] != target {
					return nil, errors.New("cold lookup lost the original connection allowance")
				}
				return left, nil
			},
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		ctx, observation := traceProviderHttpDial(ctx, "tcp", "cold-sample.example:443")
		start := time.Now()
		conn, err := resolver.dialContext(ctx, "tcp", "cold-sample.example:443", observation)
		observation.finish(err)
		if err != nil || conn != left || queries != 3 || dials != 1 || time.Since(start) != 35*time.Second {
			t.Fatalf("cold-start allowance discarded: queries=%d dials=%d elapsed=%s err=%v", queries, dials, time.Since(start), err)
		}
	})
}

// A never-answering cold path still gets only three lookup attempts, retains
// time for the final connection, and cannot renew the request deadline.
func TestProviderResolutionColdTimeoutsKeepConnectionAllowance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		queries := 0
		resolver := &providerUrlResolver{
			query: func(ctx context.Context, _, _ string) ([]netip.Addr, bool) {
				queries++
				<-ctx.Done()
				return nil, false
			},
			dial: func(context.Context, string, string, []netip.Addr) (net.Conn, error) {
				t.Error("unresolved cold target reached socket phase")
				return nil, errors.New("unexpected dial")
			},
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		ctx, observation := traceProviderHttpDial(ctx, "tcp", "cold-timeout.example:443")
		start := time.Now()
		_, err := resolver.dialContext(ctx, "tcp", "cold-timeout.example:443", observation)
		elapsed := time.Since(start)
		if err == nil || queries != 3 || elapsed < 45*time.Second || 46*time.Second <= elapsed || ctx.Err() != nil {
			t.Fatalf("cold retry budget changed: queries=%d elapsed=%s ctx_error=%v err=%v", queries, elapsed, ctx.Err(), err)
		}
	})
}

// Diagnostic callers without a parent deadline retain finite ten-second
// lookup waves instead of accidentally creating an unbounded resolver owner.
func TestProviderResolutionUnboundedCallerKeepsFiniteLookupTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		queries := 0
		resolver := &providerUrlResolver{
			query: func(ctx context.Context, _, _ string) ([]netip.Addr, bool) {
				queries++
				deadline, bounded := ctx.Deadline()
				if !bounded || time.Until(deadline) != 10*time.Second {
					t.Error("unbounded diagnostic caller lost finite DNS attempt")
				}
				<-ctx.Done()
				return nil, false
			},
			dial: func(context.Context, string, string, []netip.Addr) (net.Conn, error) {
				t.Error("unresolved diagnostic target reached socket phase")
				return nil, errors.New("unexpected dial")
			},
		}
		ctx, observation := traceProviderHttpDial(t.Context(), "tcp", "unbounded-caller.example:443")
		start := time.Now()
		_, err := resolver.dialContext(ctx, "tcp", "unbounded-caller.example:443", observation)
		if err == nil || queries != 3 || time.Since(start) < 30*time.Second || 31*time.Second <= time.Since(start) {
			t.Fatalf("diagnostic lookup escaped finite budget: queries=%d elapsed=%s err=%v", queries, time.Since(start), err)
		}
	})
}

// A lookup that answers once it is released, or fails when its context ends
// first. A nil release never answers.
func releasedProviderUrlQuery(release <-chan struct{}, addrs []netip.Addr, authoritative bool, canceled *atomic.Bool) providerUrlQuery {
	return func(ctx context.Context, _ string, _ string) ([]netip.Addr, bool) {
		select {
		case <-release:
			return addrs, authoritative
		case <-ctx.Done():
			if canceled != nil {
				canceled.Store(true)
			}
			return nil, false
		}
	}
}

// An exit that carries TCP but not UDP never answers a regional
// recommendation's plaintext DNS, and the tunnel's DoH answers. The race
// returns that answer as soon as it arrives and cancels the silent lookup;
// the race is symmetric, so an exit behind a national block of foreign DoH
// is answered by the regional servers in the same way.
func TestRaceProviderUrlQueriesTakesWhicheverResolverTheExitCarries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		target := netip.MustParseAddr("185.15.59.224")
		for _, dohAnswers := range []bool{true, false} {
			var answering, silent providerUrlQuery
			var silentCanceled atomic.Bool
			release := make(chan struct{})
			answering = releasedProviderUrlQuery(release, []netip.Addr{target}, true, nil)
			silent = releasedProviderUrlQuery(nil, nil, false, &silentCanceled)
			race := raceProviderUrlQueries(answering, silent)
			if !dohAnswers {
				race = raceProviderUrlQueries(silent, answering)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			go func() {
				time.Sleep(1200 * time.Millisecond)
				close(release)
			}()
			start := time.Now()
			addrs, authoritative := race(ctx, "A", "sample.example")
			elapsed := time.Since(start)
			cancel()
			synctest.Wait()
			if len(addrs) != 1 || addrs[0] != target || !authoritative || elapsed != 1200*time.Millisecond || !silentCanceled.Load() {
				t.Fatalf("doh answers=%t: addrs=%v authoritative=%t elapsed=%s silent canceled=%t", dohAnswers, addrs, authoritative, elapsed, silentCanceled.Load())
			}
		}
	})
}

// A miss from one resolver does not end the lookup while another may still
// answer: a censor's NXDOMAIN or a server that only refers would otherwise
// fail a name the other resolver resolves. Without any answer the lookup is
// authoritative when one resolver's miss was, and fails when none was.
func TestRaceProviderUrlQueriesMissWaitsForTheOtherResolver(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		target := netip.MustParseAddr("185.15.59.224")
		miss := func(context.Context, string, string) ([]netip.Addr, bool) { return nil, true }
		failure := func(context.Context, string, string) ([]netip.Addr, bool) { return nil, false }
		release := make(chan struct{})
		go func() {
			time.Sleep(2 * time.Second)
			close(release)
		}()
		start := time.Now()
		addrs, authoritative := raceProviderUrlQueries(miss, releasedProviderUrlQuery(release, []netip.Addr{target}, true, nil))(t.Context(), "A", "sample.example")
		if len(addrs) != 1 || addrs[0] != target || !authoritative || time.Since(start) != 2*time.Second {
			t.Fatalf("a miss ended the race: addrs=%v authoritative=%t elapsed=%s", addrs, authoritative, time.Since(start))
		}
		for _, test := range []struct {
			queries       []providerUrlQuery
			authoritative bool
		}{
			{queries: []providerUrlQuery{miss, failure}, authoritative: true},
			{queries: []providerUrlQuery{failure, miss}, authoritative: true},
			{queries: []providerUrlQuery{miss, miss}, authoritative: true},
			{queries: []providerUrlQuery{failure, failure}, authoritative: false},
		} {
			addrs, authoritative := raceProviderUrlQueries(test.queries...)(t.Context(), "A", "sample.example")
			if len(addrs) != 0 || authoritative != test.authoritative {
				t.Fatalf("unanswered race: addrs=%v authoritative=%t want %t", addrs, authoritative, test.authoritative)
			}
		}
	})
}

// The URL probe's DNS phase through an exit that drops UDP: with the regional
// plaintext lookup alone, as before the race, the five-second phase expires
// and the load fails at dial_dns; racing the tunnel's DoH, it reaches its
// socket with the DoH answer.
func TestProviderResolutionRegionalRaceSurvivesAnExitWithoutUdp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		target := netip.MustParseAddr("185.15.59.224")
		regional := releasedProviderUrlQuery(nil, nil, false, nil)
		for _, raced := range []bool{false, true} {
			release := make(chan struct{})
			go func() {
				time.Sleep(1500 * time.Millisecond)
				close(release)
			}()
			doh := releasedProviderUrlQuery(release, []netip.Addr{target}, true, nil)
			query := regional
			if raced {
				query = raceProviderUrlQueries(doh, regional)
			}
			left, right := net.Pipe()
			dials := 0
			resolver := &providerUrlResolver{
				query: query,
				dial: func(_ context.Context, network string, address string, addrs []netip.Addr) (net.Conn, error) {
					dials++
					// a URL probe request dials on its family's network alone
					// (providerUrlDialNetwork)
					if network != "tcp4" || address != "sample.example:443" || len(addrs) != 1 || addrs[0] != target {
						return nil, errors.New("wrong resolution handoff")
					}
					return left, nil
				},
			}
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			ctx = urlPhaseTestContext(t, ctx, "sample.example")
			ctx, observation := traceProviderHttpDial(ctx, "tcp", "sample.example:443")
			start := time.Now()
			conn, err := resolver.dialContext(ctx, "tcp", "sample.example:443", observation)
			stage := observation.finish(err)
			elapsed := time.Since(start)
			cancel()
			left.Close()
			right.Close()
			synctest.Wait()
			if raced && (err != nil || conn != left || dials != 1 || elapsed != 1500*time.Millisecond) {
				t.Fatalf("the race lost the DoH answer: dials=%d elapsed=%s stage=%s err=%v", dials, elapsed, stage, err)
			}
			if !raced && (err == nil || dials != 0 || stage != "dial_dns" || elapsed != urlProbeDnsTimeout) {
				t.Fatalf("plaintext alone resolved through an exit without UDP: dials=%d elapsed=%s stage=%s err=%v", dials, elapsed, stage, err)
			}
		}
	})
}
