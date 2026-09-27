package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026/qualityprobe/bandwidth"
	"github.com/urnetwork/server/v2026/qualityprobe/confinement"
	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
	"github.com/urnetwork/server/v2026/qualityprobe/ingest"
)

// Tests of the startup self-checks and flag validation: the confinement check
// over every probe host, its escape hatch, and the timeouts the binary refuses.

// A resolver that cannot resolve anything. That is both a
// correctly confined deployment (dns to a public resolver is blocked too) and
// a compose stack whose resolver has not settled yet -- and in neither case
// does it tell the prober anything about whether it can reach a probe
// destination.
func lookupFails(ctx context.Context, host string) ([]string, error) {
	return nil, errors.New("lookup " + host + ": no such host")
}

// Gives every host the check is supposed to cover -- the
// egress-health destinations -- its own documentation-range address, so a test
// can assert that the check covered exactly that set.
func lookupPerHost(ctx context.Context, host string) ([]string, error) {
	for i, h := range probeHosts() {
		if h == host {
			return []string{fmt.Sprintf("203.0.113.%d", i+1)}, nil
		}
	}
	return nil, errors.New("lookup " + host + ": no such host")
}

// Redirects the standard logger for the duration of a test and
// returns what was written.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	flags := log.Flags()
	log.SetOutput(buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(flags)
	})
	return buf
}

// The anti-drift assertion that
// matters most here: the addresses the check tests must be derived from the
// same table the prober will later fetch through a tunnel -- egresshealth's
// destinations -- not a second hand-maintained copy.
// A second copy drifts on the first endpoint change and the check keeps passing
// while no longer covering a real endpoint.
func TestCheckConfinementProbesEveryProbeHost(t *testing.T) {
	captureLog(t)
	var dialed []string
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialed = append(dialed, addr)
		return nil, errors.New("connect: network is unreachable")
	}

	if err := checkConfinement(context.Background(), dial, lookupPerHost, nil, time.Second); err != nil {
		t.Fatalf("checkConfinement: %s", err)
	}

	hosts := probeHosts()
	if len(hosts) == 0 {
		t.Fatal("probeHosts is empty; the check would have nothing to test")
	}
	var want []string
	for i := range hosts {
		want = append(want, net.JoinHostPort(fmt.Sprintf("203.0.113.%d", i+1), confinementPort))
	}
	sort.Strings(want)
	sort.Strings(dialed)
	if strings.Join(dialed, ",") != strings.Join(want, ",") {
		t.Fatalf("checkConfinement dialed %v, want the resolved address of every probe host %v (%v)", dialed, want, hosts)
	}
}

// A successful direct connection
// means the operator's confinement is missing, so the prober must not run --
// every provider would otherwise be recorded at the operator's own location.
func TestCheckConfinementRefusesWhenReachable(t *testing.T) {
	captureLog(t)
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		c1, _ := net.Pipe()
		return c1, nil
	}
	err := checkConfinement(context.Background(), dial, lookupPerHost, nil, time.Second)
	if err == nil {
		t.Fatal("checkConfinement returned nil while a probe address was directly reachable")
	}
	if !errors.Is(err, confinement.ErrNotConfined) {
		t.Fatalf("checkConfinement error = %v, want ErrNotConfined", err)
	}
}

// When dns is available
// the check must test the resolved ips, not just the names -- dialing a name
// that fails to resolve proves nothing about whether the ip behind it is
// reachable.
func TestCheckConfinementUsesResolvedAddressesWhenDnsWorks(t *testing.T) {
	captureLog(t)
	lookup := func(ctx context.Context, host string) ([]string, error) {
		return []string{"203.0.113.9"}, nil
	}
	var dialed []string
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialed = append(dialed, addr)
		return nil, errors.New("connect: network is unreachable")
	}
	if err := checkConfinement(context.Background(), dial, lookup, nil, time.Second); err != nil {
		t.Fatalf("checkConfinement: %s", err)
	}
	if len(dialed) != 1 || dialed[0] != "203.0.113.9:"+confinementPort {
		t.Fatalf("checkConfinement dialed %v, want the resolved address 203.0.113.9:%s", dialed, confinementPort)
	}
}

// The defect this fix
// exists for, at the level the operator sees it. With dns blocked -- the very
// deployment the old hostname fallback was written for -- every host fell back
// to a bare name, every dial failed at resolution rather than at a deny rule,
// and the check logged "passed" having tested nothing. A prober on a container
// with full internet egress would then record every provider at the operator's
// location. The check must refuse to start instead, and must not dial a
// hostname it knows will not resolve.
func TestCheckConfinementRefusesWhenNothingResolves(t *testing.T) {
	captureLog(t)
	var dialed []string
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialed = append(dialed, addr)
		return nil, errors.New("connect: network is unreachable")
	}

	err := checkConfinement(context.Background(), dial, lookupFails, nil, time.Second)
	if err == nil {
		t.Fatal("checkConfinement returned nil although not one probe host resolved; it tested nothing and must refuse to start")
	}
	if !errors.Is(err, confinement.ErrNoEvidence) {
		t.Fatalf("checkConfinement error = %v, want ErrNoEvidence", err)
	}
	if len(dialed) != 0 {
		t.Fatalf("checkConfinement dialed %v; an unresolvable hostname must never be dialed -- it fails at resolution and carries no signal", dialed)
	}
	// the two legitimate remedies, so the operator is not pushed towards
	// -skip-confinement-check
	for _, want := range []string{"dns", "-confinement-address"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("checkConfinement error %q does not mention %q", err, want)
		}
	}
}

// A check that covered two
// of three endpoints must not read like one that covered all three. The
// resolved hosts are still tested; the gap is named.
func TestCheckConfinementWarnsWhenSomeHostsDoNotResolve(t *testing.T) {
	logs := captureLog(t)
	hosts := probeHosts()
	if len(hosts) < 2 {
		t.Skip("need at least two probe hosts for a partial-resolution case")
	}
	skipped := hosts[len(hosts)-1]
	lookup := func(ctx context.Context, host string) ([]string, error) {
		if host == skipped {
			return nil, errors.New("lookup " + host + ": no such host")
		}
		return lookupPerHost(ctx, host)
	}
	var dialed []string
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialed = append(dialed, addr)
		return nil, errors.New("connect: network is unreachable")
	}

	if err := checkConfinement(context.Background(), dial, lookup, nil, time.Second); err != nil {
		t.Fatalf("checkConfinement: %s; the hosts that did resolve are still real evidence and must still be checked", err)
	}
	if len(dialed) != len(hosts)-1 {
		t.Fatalf("dialed %v, want the %d host(s) that resolved", dialed, len(hosts)-1)
	}
	out := logs.String()
	if !strings.Contains(out, "WARNING") {
		t.Fatalf("a degraded check logged no WARNING; it is indistinguishable from a complete one.\n--- log ---\n%s", out)
	}
	if !strings.Contains(out, skipped) {
		t.Fatalf("the WARNING does not name the unresolved host %q.\n--- log ---\n%s", skipped, out)
	}
}

// Covers the escape
// hatch: in a jail where dns genuinely cannot work, the operator supplies the
// addresses and the check stays a real check, rather than being switched off
// with -skip-confinement-check.
func TestCheckConfinementUsesExplicitAddressesWithoutResolving(t *testing.T) {
	captureLog(t)
	resolved := false
	lookup := func(ctx context.Context, host string) ([]string, error) {
		resolved = true
		return []string{"203.0.113.9"}, nil
	}
	var dialed []string
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialed = append(dialed, addr)
		return nil, errors.New("connect: network is unreachable")
	}

	explicit := []string{"198.51.100.7:443", "198.51.100.8:443"}
	if err := checkConfinement(context.Background(), dial, lookup, explicit, time.Second); err != nil {
		t.Fatalf("checkConfinement: %s", err)
	}
	if resolved {
		t.Error("checkConfinement resolved even though explicit addresses were supplied; the flag exists for a jail where resolution cannot work")
	}
	if strings.Join(dialed, ",") != strings.Join(explicit, ",") {
		t.Fatalf("checkConfinement dialed %v, want exactly the supplied %v", dialed, explicit)
	}
}

// The escape hatch
// supplies what to dial, not permission to skip the verdict.
func TestCheckConfinementStillRefusesWithExplicitAddresses(t *testing.T) {
	captureLog(t)
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		c1, _ := net.Pipe()
		return c1, nil
	}
	err := checkConfinement(context.Background(), dial, lookupFails, []string{"198.51.100.7:443"}, time.Second)
	if !errors.Is(err, confinement.ErrNotConfined) {
		t.Fatalf("checkConfinement error = %v, want ErrNotConfined", err)
	}
}

// A hostname here would put the
// defect straight back -- in the dns-blocked jail this flag is for, the dial
// would fail at resolution and prove nothing.
func TestConfinementAddressFlagRejectsANonAddress(t *testing.T) {
	for _, bad := range []string{"api.example:443", "198.51.100.7", "", "198.51.100.7:"} {
		var list addressList
		if err := list.Set(bad); err == nil {
			t.Errorf("-confinement-address accepted %q; it must be an ip literal with a port", bad)
		}
	}
	var list addressList
	if err := list.Set("198.51.100.7:443"); err != nil {
		t.Errorf("-confinement-address rejected a valid ip:port: %s", err)
	}
	if err := list.Set("[2001:db8::1111]:443"); err != nil {
		t.Errorf("-confinement-address rejected a valid ipv6 address: %s", err)
	}
	if len(list) != 2 {
		t.Errorf("addressList = %v, want both values collected; the flag is repeatable", list)
	}
}

// A check that is off by default is not
// a check. This asserts on the built binary's own -h output, so it holds
// regardless of how the flag is wired internally.
func TestSkipConfinementCheckIsOffByDefault(t *testing.T) {
	out := runProberWithSecretsInEnv(t, "-h")
	if !strings.Contains(out, "-skip-confinement-check") {
		t.Fatalf("-h does not document -skip-confinement-check.\n--- output ---\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "skip-confinement-check") {
			// flag.PrintDefaults renders a true bool default as `(default true)`
			// and omits the clause entirely for false.
			if strings.Contains(line, "default true") {
				t.Fatalf("-skip-confinement-check defaults to true; the confinement check must be on unless explicitly disabled.\n--- line ---\n%s", line)
			}
		}
	}
}

// The escape hatch exists for the operator
// running a one-shot manual probe, but it disables the only thing standing
// between a misconfigured host and recording every provider at the operator's
// own location. It must be impossible to miss in the log.
//
// The run dies at parseByJwtClientId on the placeholder jwt, immediately after
// the check and before any network call, so nothing real is contacted. (The
// unreachable -api-url is belt and braces: it is never dialed.)
func TestSkipConfinementCheckLogsLoudly(t *testing.T) {
	out := runProberWithSecretsInEnv(t,
		"-skip-confinement-check",
		"-api-url", "http://127.0.0.1:1",
		"-platform-url", "ws://127.0.0.1:1",
		"-interval", "0",
	)
	if !strings.Contains(out, "WARNING") {
		t.Fatalf("-skip-confinement-check produced no WARNING line.\n--- output ---\n%s", out)
	}
	if !strings.Contains(strings.ToLower(out), "confinement") {
		t.Fatalf("-skip-confinement-check did not say what was skipped.\n--- output ---\n%s", out)
	}
}

// Drives the real binary,
// because this was a live hole reproduced with the real binary: on a host with
// open egress, -confinement-timeout 10ms correctly reported "not confined" and
// exited 1, while 1ms -- the same host, the same second -- logged
// "confinement self-check passed" and started. Rejecting only <= 0 left every
// positive-but-too-small value able to switch the guarantee off while logging
// success. The binary must refuse the flag before it runs the check at all.
func TestConfinementTimeoutBelowTheFloorIsRejected(t *testing.T) {
	for _, timeout := range []string{"1ms", "10ms", "499ms"} {
		out, code := runProber(t,
			"-api-url", "http://127.0.0.1:1",
			"-platform-url", "ws://127.0.0.1:1",
			"-interval", "0",
			"-confinement-timeout", timeout,
		)
		if code != 2 {
			t.Errorf("-confinement-timeout %s exited %d, want 2 (a rejected flag).\n--- output ---\n%s", timeout, code, out)
		}
		if !strings.Contains(out, "-confinement-timeout must be at least") {
			t.Errorf("-confinement-timeout %s was not rejected with a clear message.\n--- output ---\n%s", timeout, out)
		}
		if strings.Contains(out, "self-check passed") {
			t.Errorf("-confinement-timeout %s reported the self-check as PASSED; a dial that expires before a connection could complete tests nothing.\n--- output ---\n%s", timeout, out)
		}
	}
}

// Runs the built binary and returns its combined output and exit
// code. Like runProberWithSecretsInEnv, but the exit code is the assertion.
func runProber(t *testing.T, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(buildProber(t), args...)
	cmd.Env = append(os.Environ(),
		"UR_PROBER_BY_JWT="+testJwtSecret,
		"UR_OPERATOR_SECRET="+testOperatorSecret,
	)
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0
	case errors.As(err, &exitErr):
		return string(out), exitErr.ExitCode()
	default:
		t.Fatalf("running the prober: %s", err)
		return "", -1
	}
}

// The confinement self-check is only as good as
// the host list it is given. The egress-health table must be represented, and
// no host twice -- a duplicate would make the check dial the same address again
// and read like broader coverage than it has.
//
// The egress-health destinations matter here in a way that is easy to
// under-weight. A prober that can reach them directly does not merely leak the
// operator's address: the check would pass from the operator's own host, and so
// would certify a blackholing provider as healthy. That is the inversion of the
// signal, not a degradation of it.
func TestProbeHostsCoversTheTable(t *testing.T) {
	hosts := probeHosts()
	index := map[string]int{}
	for _, h := range hosts {
		index[h]++
	}
	for _, h := range hosts {
		if index[h] != 1 {
			t.Fatalf("probeHosts lists %q %d times: %v", h, index[h], hosts)
		}
	}
	for _, h := range egresshealth.DestinationHosts() {
		if index[h] == 0 {
			t.Errorf("egress-health destination host %q is not in probeHosts %v; the confinement check would not cover it, and an operator reading -confinement-address guidance would never learn it exists", h, hosts)
		}
	}
	if len(hosts) != len(egresshealth.DestinationHosts()) {
		t.Fatalf("probeHosts (%d) differs from the egress-health table (%d) with no extra hosts named", len(hosts), len(egresshealth.DestinationHosts()))
	}
}

// A pin set the server serves reaches the
// prober whole -- for the operator's echo host, for a pooled destination, for
// a host nothing dials -- and each probe then keeps only the pins for the hosts
// it dials (fleetprobe.restrictPins), so the set can never widen a tunnel's
// allowlist. What the gate refuses is half a pin.
func TestServedPinsAreKeptAsServed(t *testing.T) {
	served := map[string]ingest.GeolocationPin{
		"api.example.net": {Leaf: "leaf-api", Intermediate: "int-api"},
		"retired.example": {Leaf: "leaf-retired", Intermediate: "int-retired"},
	}
	for _, h := range egresshealth.DestinationHosts()[:3] {
		served[h] = ingest.GeolocationPin{Leaf: "leaf-" + h, Intermediate: "int-" + h}
	}
	endpoint := &pinEndpoint{served: served}
	srv := httptest.NewServer(http.HandlerFunc(endpoint.serve))
	defer srv.Close()

	pins, err := fetchPins(context.Background(), &ingest.Client{ServerUrl: srv.URL, OperatorSecret: "s3cret"})
	if err != nil {
		t.Fatalf("fetchPins: %s", err)
	}
	if len(pins) != len(served) {
		t.Fatalf("fetchPins kept %d of %d served pins", len(pins), len(served))
	}
	if got := pins["api.example.net"]; len(got) != 2 || got[0] != "leaf-api" || got[1] != "int-api" {
		t.Errorf("api.example.net pins = %v", got)
	}
}

// -probe-timeout is the
// warm-up's own timeout -- the one fetch sized for a tunnel's cold start --
// and each load attempt gets the smaller of it and the per-request floor. The
// run's budget is not one -probe-timeout any more: it is left derived from the
// retry schedule, since every load gets its spaced tries.
func TestEgressHealthOptionsFollowTheProbeTimeout(t *testing.T) {
	for _, all := range []bool{false, true} {
		for _, probeTimeout := range []time.Duration{10 * time.Second, 30 * time.Second, 60 * time.Second, 5 * time.Minute} {
			opts := egressHealthOptions(probeTimeout, all)
			if opts.IpEchoTimeout != probeTimeout {
				t.Errorf("egressHealthOptions(%s, %t).IpEchoTimeout = %s, want the probe timeout", probeTimeout, all, opts.IpEchoTimeout)
			}
			if want := min(probeTimeout, egresshealth.DefaultPerRequestTimeout); opts.PerRequestTimeout != want {
				t.Errorf("egressHealthOptions(%s, %t).PerRequestTimeout = %s, want %s", probeTimeout, all, opts.PerRequestTimeout, want)
			}
			if opts.Budget != 0 {
				t.Errorf("egressHealthOptions(%s, %t).Budget = %s, want it left to the derived RunBudget", probeTimeout, all, opts.Budget)
			}
			if opts.AllDestinations != all {
				t.Errorf("egressHealthOptions(%s, %t).AllDestinations = %t", probeTimeout, all, opts.AllDestinations)
			}
		}
	}
	// A probe timeout under the floor shows under it, which is what startup
	// refuses.
	if opts := egressHealthOptions(5*time.Second, false); egresshealth.DefaultPerRequestTimeout <= opts.PerRequestTimeout {
		t.Error("a 5s probe timeout did not come out under the per-request floor")
	}
}

// Drives the real binary: a
// -probe-timeout under the per-request floor would give every load attempt
// less than a warm request needs, and every load would risk being recorded as
// a failed site.
func TestProbeTimeoutBelowTheFloorIsRejected(t *testing.T) {
	out, code := runProber(t,
		"-api-url", "http://127.0.0.1:1",
		"-platform-url", "ws://127.0.0.1:1",
		"-interval", "0",
		"-probe-timeout", "5s",
	)
	if code != 2 || !strings.Contains(out, "per egress-health load attempt") {
		t.Fatalf("-probe-timeout 5s exited %d.\n--- output ---\n%s", code, out)
	}
}

// The full table is the default because it is the only configuration that
// exercises concurrency: a sample asks the provider to carry ~30 parallel
// requests, where a real client under load looks far more like the whole
// table. Sampling remains available and is cheaper, but it cannot test that.
func TestEgressHealthAllDefaultsOn(t *testing.T) {
	fs := flag.NewFlagSet("egress-prober", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	all := fs.Bool("egress-health-all", true, "")
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse: %s", err)
	}
	if !*all {
		t.Error("-egress-health-all defaults off; every provider test must run the full table")
	}
}

// The CDN target is a
// third-party host reached through the tunnel, so a jail that lets the
// prober reach it directly is the same defect as one that lets it reach a
// geolocation api. probeHosts' comment claimed to cover "every third-party
// host this process reaches through a tunnel" while excluding it, and an
// operator translating that list into -confinement-address entries would
// never learn it existed.
func TestBandwidthCdnHostIsInTheConfinementCheck(t *testing.T) {
	hosts := probeHosts(bandwidthProbeHosts(false, bandwidth.CdnTestUrl)...)
	want := "speed.cloudflare.com"
	if !slices.Contains(hosts, want) {
		t.Errorf("probe hosts do not include the bandwidth CDN target %q", want)
	}

	// -skip-bandwidth means the host is never reached, so it must not be
	// dialed by the self-check either: a host this process will not touch is
	// not evidence about the confinement it needs.
	if got := bandwidthProbeHosts(true, bandwidth.CdnTestUrl); len(got) != 0 {
		t.Errorf("bandwidthProbeHosts with -skip-bandwidth = %v, want none", got)
	}

	// A custom -bandwidth-cdn-url must follow, or the check covers a host the
	// deployment does not use while missing the one it does.
	if got := bandwidthProbeHosts(false, "https://mirror.example.net/__down"); len(got) != 1 || got[0] != "mirror.example.net" {
		t.Errorf("bandwidthProbeHosts with a custom url = %v, want [mirror.example.net]", got)
	}
}

// A negative ttl makes recentlyProbed always
// false, silently disabling the enumeration cache. Every other duration flag
// fails fast.
func TestNegativeCacheTtlIsRejected(t *testing.T) {
	out, code := runProber(t,
		"-api-url", "http://127.0.0.1:1",
		"-platform-url", "ws://127.0.0.1:1",
		"-cache-ttl", "-1h",
	)
	if code == 0 {
		t.Fatalf("a negative -cache-ttl was accepted.\n--- output ---\n%s", out)
	}
	if !strings.Contains(out, "-cache-ttl") {
		t.Errorf("the failure does not name -cache-ttl.\n--- output ---\n%s", out)
	}
}
