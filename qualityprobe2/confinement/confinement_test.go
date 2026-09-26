package confinement

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// dialRefused is the healthy production dialer from inside a confined
// process: the packet is dropped or rejected, so the dial errors.
func dialRefused(ctx context.Context, network, addr string) (net.Conn, error) {
	return nil, errors.New("connect: network is unreachable")
}

// lookupFails is a resolver that cannot answer, which is what a deny-all
// confinement looks like from inside: dns to a public resolver is blocked too.
func lookupFails(ctx context.Context, host string) ([]string, error) {
	return nil, errors.New("lookup " + host + ": no such host")
}

func TestVerifyFailsWhenDirectConnectionSucceeds(t *testing.T) {
	// a dialer that connects means nothing is stopping the prober reaching a
	// geolocation api directly -- the confinement is absent and the whole
	// guarantee is void, so Verify must refuse
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		c1, _ := net.Pipe()
		return c1, nil
	}
	err := Verify(context.Background(), dial, []string{"34.117.59.81:443"}, nil, time.Second)
	if err == nil {
		t.Fatal("Verify returned nil when a direct connection succeeded; it must refuse to run")
	}
	if !errors.Is(err, ErrNotConfined) {
		t.Fatalf("Verify error = %v, want it to wrap ErrNotConfined so callers can tell this apart from a plumbing failure", err)
	}
}

func TestVerifyPassesWhenDirectConnectionRefused(t *testing.T) {
	if err := Verify(context.Background(), dialRefused, []string{"34.117.59.81:443"}, nil, time.Second); err != nil {
		t.Fatalf("Verify errored when the connection was refused: %s", err)
	}
}

// TestVerifyTreatsADialTimeoutAsConfined pins the reading the whole check
// depends on: a dropped packet is what a deny rule looks like from inside, so
// a dial that runs out its budget without answering counts as REFUSED, not as
// an inconclusive plumbing error and certainly not as a reachable address.
// Nothing asserted this, and resolving it the other way would break the check
// in the dangerous direction on the single most common shape of a real
// firewall.
func TestVerifyTreatsADialTimeoutAsConfined(t *testing.T) {
	var timedOut int
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		<-ctx.Done() // the packet went nowhere, exactly as a DROP rule does
		timedOut++
		return nil, ctx.Err()
	}
	start := time.Now()
	if err := Verify(context.Background(), dial, []string{"1.1.1.1:443"}, nil, MinTimeout); err != nil {
		t.Fatalf("Verify error = %v, want nil: a dial that timed out is a blocked dial, which is the healthy outcome", err)
	}
	if timedOut != 1 {
		t.Fatalf("the dialer was called %d time(s), want 1", timedOut)
	}
	if elapsed := time.Since(start); elapsed < MinTimeout {
		t.Fatalf("Verify returned after %s, less than the %s budget; the dial was not actually allowed to time out", elapsed, MinTimeout)
	}
}

// TestVerifyInterruptedContextIsNotEvidence: with the parent context already
// cancelled (or expiring mid-loop), every dial fails in microseconds with a
// context error -- which the loop would happily count as "refused", the
// exact vacuous-pass shape TestVerifyTreatsADialTimeoutAsConfined guards
// against, reached through a path the MinTimeout floor cannot see.
func TestVerifyInterruptedContextIsNotEvidence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return nil, ctx.Err()
	}
	err := Verify(ctx, dial, []string{"203.0.113.7:443"}, nil, MinTimeout)
	if err == nil {
		t.Fatal("Verify reported confinement from a run whose every dial was cut short by the caller's dead context; an interrupted check has no evidence")
	}
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("Verify error = %v, want ErrInterrupted", err)
	}
}

func TestVerifyRequiresAtLeastOneAddress(t *testing.T) {
	// an empty address list would vacuously "pass" and silently disable the
	// entire check
	if err := Verify(context.Background(), dialRefused, nil, nil, time.Second); err == nil {
		t.Fatal("Verify accepted an empty address list; that would disable the check")
	}
}

// TestVerifyChecksEveryAddress guards the loop itself: a Verify that returned
// after the first refused address would pass a host that is reachable while a
// sibling host is not -- which is exactly the partial-confinement case (one
// allow rule too many) the check exists to catch.
func TestVerifyChecksEveryAddress(t *testing.T) {
	var dialed []string
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialed = append(dialed, addr)
		if addr == "2.2.2.2:443" {
			c1, _ := net.Pipe()
			return c1, nil
		}
		return nil, errors.New("connect: network is unreachable")
	}
	err := Verify(context.Background(), dial, []string{"1.1.1.1:443", "2.2.2.2:443"}, nil, time.Second)
	if !errors.Is(err, ErrNotConfined) {
		t.Fatalf("Verify error = %v, want ErrNotConfined; the second address accepted a direct connection", err)
	}
	if len(dialed) != 2 {
		t.Fatalf("dialed %v, want every address attempted", dialed)
	}
}

// TestVerifyClosesTheConnectionItOpened: the failing path still opened a real
// socket to a third party. It must not be leaked -- this process is about to
// exit, but Verify is also callable from a test or a long-running caller.
func TestVerifyClosesTheConnectionItOpened(t *testing.T) {
	c1, c2 := net.Pipe()
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return c1, nil
	}
	if err := Verify(context.Background(), dial, []string{"1.1.1.1:443"}, nil, time.Second); err == nil {
		t.Fatal("Verify must refuse when a direct connection succeeded")
	}
	// the peer end of a closed pipe reports io.ErrClosedPipe rather than blocking
	_ = c2.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := c2.Read(make([]byte, 1)); err == nil {
		t.Fatal("the connection Verify opened is still readable; it was not closed")
	}
}

// TestVerifyRejectsNonPositiveTimeout: context.WithTimeout with a zero or
// negative duration produces an already-expired context, so every dial would
// fail instantly for a reason that has nothing to do with confinement and the
// check would pass vacuously. That is the same defect as an empty address
// list, reached by a different route.
func TestVerifyRejectsNonPositiveTimeout(t *testing.T) {
	for _, timeout := range []time.Duration{0, -time.Second} {
		if err := Verify(context.Background(), dialRefused, []string{"1.1.1.1:443"}, nil, timeout); err == nil {
			t.Fatalf("Verify accepted timeout=%s; an expired context makes every dial fail and the check vacuous", timeout)
		}
	}
}

// TestVerifyRejectsATimeoutBelowTheFloor is the same defect as a zero timeout
// with a positive number in front of it, and it was the live hole: on one
// unconfined host, in the same second, -confinement-timeout 10ms reported "not
// confined" and 1ms reported "passed". Any budget too short for a connection
// to have completed makes every dial fail on the clock rather than on a deny
// rule, so the check reports success having tested nothing. Nothing below
// MinTimeout is evidence.
func TestVerifyRejectsATimeoutBelowTheFloor(t *testing.T) {
	for _, timeout := range []time.Duration{time.Nanosecond, time.Millisecond, 10 * time.Millisecond, MinTimeout - time.Nanosecond} {
		err := Verify(context.Background(), dialRefused, []string{"1.1.1.1:443"}, nil, timeout)
		if err == nil {
			t.Fatalf("Verify accepted timeout=%s; a dial that expires before a connection could complete proves nothing about confinement", timeout)
		}
		if !errors.Is(err, ErrInvalidTimeout) {
			t.Fatalf("Verify(timeout=%s) error = %v, want ErrInvalidTimeout", timeout, err)
		}
	}
	if err := Verify(context.Background(), dialRefused, []string{"1.1.1.1:443"}, nil, MinTimeout); err != nil {
		t.Fatalf("Verify rejected the floor value %s itself: %s", MinTimeout, err)
	}
}

func TestVerifyRequiresADialer(t *testing.T) {
	if err := Verify(context.Background(), nil, []string{"1.1.1.1:443"}, nil, time.Second); err == nil {
		t.Fatal("Verify accepted a nil dialer")
	}
}

// TestVerifyErrorsWhenNoHostResolved states the fail-closed rule directly:
// inability to verify is not evidence of confinement. When every host failed to
// resolve there is nothing to dial, so the check obtained no evidence at all --
// and the deployment where that happens (deny-all with dns blocked, or a
// compose stack started before its resolver settles) is exactly the one where a
// false "passed" lets the prober record every provider at the operator's own
// location. It must refuse to start, with an error distinguishable from
// ErrNotConfined: this does not say the host is unconfined, it says the check
// cannot tell.
func TestVerifyErrorsWhenNoHostResolved(t *testing.T) {
	hosts := []string{"ip.pn", "free.freeipapi.com", "ipinfo.io"}
	addrs, unresolved, err := Addresses(context.Background(), lookupFails, hosts, "443")
	if err != nil {
		t.Fatalf("Addresses: %s", err)
	}
	if len(addrs) != 0 {
		t.Fatalf("Addresses = %v, want nothing dialable when not one host resolved", addrs)
	}
	if len(unresolved) != len(hosts) {
		t.Fatalf("unresolved = %v, want all %d hosts", unresolved, len(hosts))
	}

	err = Verify(context.Background(), dialRefused, addrs, unresolved, time.Second)
	if err == nil {
		t.Fatal("Verify returned nil although not one host resolved; the check tested nothing and must refuse to run rather than report a pass")
	}
	if !errors.Is(err, ErrNoEvidence) {
		t.Fatalf("Verify error = %v, want ErrNoEvidence", err)
	}
	if errors.Is(err, ErrNotConfined) {
		t.Fatalf("Verify error = %v, want ErrNoEvidence to stay distinguishable from ErrNotConfined: nothing was learned about whether this host is confined", err)
	}
	// the operator has to be told what to do about it
	for _, want := range []string{"dns", "explicitly"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Verify error %q does not mention %q; the message must name both remedies", err, want)
		}
	}
	// and which hosts were the problem
	for _, host := range hosts {
		if !strings.Contains(err.Error(), host) {
			t.Errorf("Verify error %q does not name the unresolved host %q", err, host)
		}
	}
}

// TestVerifyStillChecksTheAddressesItHasWhenSomeHostsFailed: a partly working
// resolver must not empty the check. What did resolve is still tested.
func TestVerifyStillChecksTheAddressesItHasWhenSomeHostsFailed(t *testing.T) {
	var dialed []string
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialed = append(dialed, addr)
		c1, _ := net.Pipe()
		return c1, nil
	}
	err := Verify(context.Background(), dial, []string{"1.1.1.1:443"}, []string{"b.example"}, time.Second)
	if !errors.Is(err, ErrNotConfined) {
		t.Fatalf("Verify error = %v, want ErrNotConfined; the one resolved address accepted a connection", err)
	}
	if len(dialed) != 1 || dialed[0] != "1.1.1.1:443" {
		t.Fatalf("dialed %v, want only the resolved address", dialed)
	}
}

func TestAddressesResolvesEveryHost(t *testing.T) {
	lookup := func(ctx context.Context, host string) ([]string, error) {
		switch host {
		case "a.example":
			return []string{"1.1.1.1", "2606:4700::1111"}, nil
		case "b.example":
			return []string{"2.2.2.2"}, nil
		}
		return nil, errors.New("no such host")
	}
	got, unresolved, err := Addresses(context.Background(), lookup, []string{"a.example", "b.example"}, "443")
	if err != nil {
		t.Fatalf("Addresses: %s", err)
	}
	if len(unresolved) != 0 {
		t.Fatalf("unresolved = %v, want none", unresolved)
	}
	want := []string{"1.1.1.1:443", "[2606:4700::1111]:443", "2.2.2.2:443"}
	if len(got) != len(want) {
		t.Fatalf("Addresses = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Addresses = %v, want %v", got, want)
		}
	}
}

// TestAddressesDeduplicates: two hosts behind the same anycast address must not
// be dialed twice, and a duplicate must not inflate an operator's sense of how
// much the check covers.
func TestAddressesDeduplicates(t *testing.T) {
	lookup := func(ctx context.Context, host string) ([]string, error) {
		return []string{"1.1.1.1"}, nil
	}
	got, _, err := Addresses(context.Background(), lookup, []string{"a.example", "b.example"}, "443")
	if err != nil {
		t.Fatalf("Addresses: %s", err)
	}
	if len(got) != 1 || got[0] != "1.1.1.1:443" {
		t.Fatalf("Addresses = %v, want exactly [1.1.1.1:443]", got)
	}
}

// TestAddressesNeverEmitsAnUnresolvedHostname is the defect this package was
// shipping. A bare "ipinfo.io:443" handed to the production dialer resolves
// through the very resolver that just failed, so the dial is guaranteed to fail
// at resolution and carries no signal whatsoever about confinement -- yet it
// counted as an address that "refused a direct connection". The host must be
// reported as unresolved instead, so the caller can warn or refuse.
func TestAddressesNeverEmitsAnUnresolvedHostname(t *testing.T) {
	lookup := func(ctx context.Context, host string) ([]string, error) {
		if host == "a.example" {
			return []string{"1.1.1.1"}, nil
		}
		return nil, errors.New("lookup b.example: server misbehaving")
	}
	got, unresolved, err := Addresses(context.Background(), lookup, []string{"a.example", "b.example"}, "443")
	if err != nil {
		t.Fatalf("Addresses: %s", err)
	}
	if len(got) != 1 || got[0] != "1.1.1.1:443" {
		t.Fatalf("Addresses = %v, want exactly the resolved [1.1.1.1:443] and no hostname placeholder", got)
	}
	if len(unresolved) != 1 || unresolved[0] != "b.example" {
		t.Fatalf("unresolved = %v, want [b.example]", unresolved)
	}
}

// TestAddressesTreatsAnEmptyRecordSetAsUnresolved: a resolver that answers with
// no records is the same situation as one that errors.
func TestAddressesTreatsAnEmptyRecordSetAsUnresolved(t *testing.T) {
	lookup := func(ctx context.Context, host string) ([]string, error) {
		return nil, nil
	}
	got, unresolved, err := Addresses(context.Background(), lookup, []string{"a.example"}, "443")
	if err != nil {
		t.Fatalf("Addresses: %s", err)
	}
	if len(got) != 0 {
		t.Fatalf("Addresses = %v, want nothing dialable", got)
	}
	if len(unresolved) != 1 || unresolved[0] != "a.example" {
		t.Fatalf("unresolved = %v, want [a.example]", unresolved)
	}
}

// TestAddressesRejectsANonIpRecord: a resolver answering with a name rather
// than an address would reintroduce the defect one level down -- the "address"
// would just be re-resolved at dial time.
func TestAddressesRejectsANonIpRecord(t *testing.T) {
	lookup := func(ctx context.Context, host string) ([]string, error) {
		return []string{"cdn.example"}, nil
	}
	got, unresolved, err := Addresses(context.Background(), lookup, []string{"a.example"}, "443")
	if err != nil {
		t.Fatalf("Addresses: %s", err)
	}
	if len(got) != 0 {
		t.Fatalf("Addresses = %v, want nothing: %q is not an ip literal", got, "cdn.example")
	}
	if len(unresolved) != 1 || unresolved[0] != "a.example" {
		t.Fatalf("unresolved = %v, want [a.example]", unresolved)
	}
}

// TestAddressesRoutesBlockedResolverAnswersToUnresolved: filtering resolvers
// (Pi-hole, AdGuard, NextDNS, plenty of corporate DNS) answer 0.0.0.0 or ::
// for a blocked name, and free geolocation apis are plausible blocklist
// members. Those answers are not the host's address, and dialing them is
// worse than useless: 0.0.0.0 dials loopback on Linux, so with nothing
// listening the refusal reads as evidence of confinement (a vacuous pass --
// the defect class this package exists to refuse), and with anything on the
// port it reads as ErrNotConfined with a misleading diagnosis. The same
// holds for loopback, link-local, multicast and the like: none of them are
// the internet host the check is meant to prove unreachable, so they take
// the unresolved path like a non-ip record.
func TestAddressesRoutesBlockedResolverAnswersToUnresolved(t *testing.T) {
	cases := []struct {
		name   string
		answer string
	}{
		{"ipv4 unspecified", "0.0.0.0"},
		{"ipv6 unspecified", "::"},
		{"ipv4 loopback", "127.0.0.1"},
		{"ipv6 loopback", "::1"},
		{"ipv4 link-local", "169.254.169.254"},
		{"ipv6 link-local", "fe80::1"},
		{"multicast", "ff02::1"},
		{"rfc1918 answer from a split-horizon resolver", "192.168.1.1"},
		{"rfc1918 10/8", "10.0.0.53"},
		{"rfc1918 172.16/12", "172.16.0.1"},
		{"ipv6 ULA", "fd00::1"},
		{"cgnat", "100.64.0.1"},
		{"ipv4-mapped unspecified", "::ffff:0.0.0.0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lookup := func(ctx context.Context, host string) ([]string, error) {
				return []string{tc.answer}, nil
			}
			got, unresolved, err := Addresses(context.Background(), lookup, []string{"a.example"}, "443")
			if err != nil {
				t.Fatalf("Addresses: %s", err)
			}
			if len(got) != 0 {
				t.Fatalf("Addresses = %v, want nothing dialable: %q is a filtering-resolver answer, not the host's address", got, tc.answer)
			}
			if len(unresolved) != 1 || unresolved[0] != "a.example" {
				t.Fatalf("unresolved = %v, want [a.example]", unresolved)
			}
		})
	}
}

// TestAddressesKeepsTheRealRecordsWhenABlockedAnswerIsMixedIn: one poisoned
// record must not discard the genuine ones alongside it.
func TestAddressesKeepsTheRealRecordsWhenABlockedAnswerIsMixedIn(t *testing.T) {
	lookup := func(ctx context.Context, host string) ([]string, error) {
		return []string{"0.0.0.0", "203.0.113.7"}, nil
	}
	got, unresolved, err := Addresses(context.Background(), lookup, []string{"a.example"}, "443")
	if err != nil {
		t.Fatalf("Addresses: %s", err)
	}
	if len(got) != 1 || got[0] != "203.0.113.7:443" {
		t.Fatalf("Addresses = %v, want [203.0.113.7:443]", got)
	}
	if len(unresolved) != 0 {
		t.Fatalf("unresolved = %v, want none: a real record was obtained", unresolved)
	}
}

func TestAddressesRequiresAtLeastOneHost(t *testing.T) {
	lookup := func(ctx context.Context, host string) ([]string, error) {
		return []string{"1.1.1.1"}, nil
	}
	if _, _, err := Addresses(context.Background(), lookup, nil, "443"); err == nil {
		t.Fatal("Addresses accepted an empty host list; the check must not be reducible to nothing")
	}
}

// TestAddressesRequiresALookup: Verify refuses a nil dialer rather than
// substituting a default one, and Addresses must take the same posture. In the
// one package whose job is failing closed, quietly resolving through a resolver
// the caller never passed is exactly the kind of assumption this package exists
// to remove.
func TestAddressesRequiresALookup(t *testing.T) {
	_, _, err := Addresses(context.Background(), nil, []string{"a.example"}, "443")
	if err == nil {
		t.Fatal("Addresses accepted a nil lookup and silently substituted the real resolver")
	}
	if !errors.Is(err, ErrNoLookup) {
		t.Fatalf("Addresses error = %v, want ErrNoLookup", err)
	}
}

// TestAddressesRefusesWhenResolutionIsInterrupted: the resolution budget is
// shared across all hosts, so a resolver that hangs on the first one leaves
// every remaining host failing instantly for the caller's reason. Those land
// in `unresolved`, the caller logs a degraded WARNING and proceeds -- having
// proven confinement only for whatever resolved before the clock ran out.
// Verify refuses on exactly this shape; Addresses must too.
func TestAddressesRefusesWhenResolutionIsInterrupted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	first := true
	lookup := func(ctx context.Context, host string) ([]string, error) {
		if first {
			first = false
			cancel() // the shared budget is burned resolving host #1
			return []string{"203.0.113.7"}, nil
		}
		return nil, ctx.Err()
	}
	defer cancel()

	_, _, err := Addresses(ctx, lookup, []string{"a.example", "b.example", "c.example", "d.example"}, "443")
	if err == nil {
		t.Fatal("Addresses reported success after resolution was cut short; the hosts it never resolved would be logged as a degraded pass")
	}
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("Addresses error = %v, want ErrInterrupted", err)
	}
}

// TestVerifyKeepsEvidenceFromACheckThatFinished: a context that dies in the
// window AFTER the final genuine refusal came back describes a check that
// did finish. Reporting "interrupted before it finished" there discards real
// evidence and exits non-zero on a correctly confined host.
func TestVerifyKeepsEvidenceFromACheckThatFinished(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	dialed := 0
	dial := func(dialCtx context.Context, network, addr string) (net.Conn, error) {
		dialed++
		// A real deny rule: the refusal has nothing to do with the context.
		err := errors.New("connect: permission denied")
		if dialed == 3 {
			cancel() // the operator's signal lands after the last real answer
		}
		return nil, err
	}
	defer cancel()

	err := Verify(ctx, dial, []string{"203.0.113.1:443", "203.0.113.2:443", "203.0.113.3:443"}, nil, MinTimeout)
	if err != nil {
		t.Fatalf("Verify error = %v, want nil: every address was dialed and every one was genuinely refused", err)
	}
}
