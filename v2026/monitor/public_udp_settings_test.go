package monitor

// The public UDP probe's settings (PublicUdpProbeSettings): one instance of a
// pass reaches the matrix, the attempt context, the worker pool, the
// transport's wire budgets and the findings' validation of the counts the
// transport reports, so a tuned bound holds everywhere at once.

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"sync"
	"testing"
	"time"

	connectcore "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/quic-go/v2026"
)

// The defaults are the probe's documented bounds, and each call returns its
// own instance, the api request settings included.
func TestPublicUdpProbeSettingsDefaultsAreFreshInstances(t *testing.T) {
	want := PublicUdpProbeSettings{
		Cadence:                          5 * time.Minute,
		AttemptTimeout:                   5 * time.Second,
		MaxTargetCount:                   64,
		WorkerCount:                      4,
		MaxPacketCount:                   2048,
		MaxByteCount:                     1024 * 1024,
		StreamReceiveWindowByteCount:     16 * 1024,
		ConnectionReceiveWindowByteCount: 32 * 1024,
		ApiRequest:                       PublicUdpApiRequestSettings{ResponseHeaderByteLimit: 16 * 1024, ResponseBodyByteLimit: 16 * 1024},
	}
	first := DefaultPublicUdpProbeSettings()
	if *first != want {
		t.Fatalf("defaults = %+v, want %+v", *first, want)
	}
	first.MaxPacketCount = 1
	first.ApiRequest.ResponseBodyByteLimit = 1
	if second := DefaultPublicUdpProbeSettings(); *second != want {
		t.Fatalf("a mutated instance reached the next: %+v", *second)
	}
}

// A pass runs at its settings' cadence, and at the default's when that is not
// positive, since the scheduler cannot run a non-positive interval.
func TestPublicUdpCadenceFollowsItsSettings(t *testing.T) {
	defaultCadence := DefaultPublicUdpProbeSettings().Cadence
	cases := []struct {
		settings *PublicUdpProbeSettings
		cadence  time.Duration
	}{
		{settings: &PublicUdpProbeSettings{Cadence: 30 * time.Second}, cadence: 30 * time.Second},
		{settings: &PublicUdpProbeSettings{Cadence: 0}, cadence: defaultCadence},
		{settings: &PublicUdpProbeSettings{Cadence: -time.Minute}, cadence: defaultCadence},
		{settings: nil, cadence: defaultCadence},
	}
	for _, c := range cases {
		if cadence := (publicUdpProbe{settings: c.settings}).cadence(); cadence != c.cadence {
			t.Errorf("settings %+v gave a cadence of %s, want %s", c.settings, cadence, c.cadence)
		}
	}
	if cadence := NewPublicUdpSignal().Cadence(); cadence != defaultCadence {
		t.Errorf("the registered signal runs every %s", cadence)
	}
}

// A matrix of n single-family rows on one enrolled host.
func publicUdpTestMatrix(n int) PublicUdpSettings {
	settings := PublicUdpSettings{Enabled: true, ExpectedTargets: n}
	for index := range n {
		settings.Targets = append(settings.Targets, PublicUdpTargetSettings{
			Name: fmt.Sprintf("synthetic-row-%d", index), Host: "edge-synthetic", Interface: "eth1", Service: "connect",
			Alias: fmt.Sprintf("alias-%d", index), Front: "connect", Carrier: "quic", Families: []string{"ipv4"},
			IPv4Address: "192.0.2.20", Port: 443, ServerName: "connect.synthetic.example",
		})
	}
	return settings
}

// The target bound is the settings' own: a matrix past the default bound is
// admitted under a raised one and refused under a lowered one, and settings
// that bound no attempt refuse every matrix before any socket.
func TestPublicUdpMatrixBoundFollowsItsSettings(t *testing.T) {
	hosts := []*host{{name: "edge-synthetic"}}
	raised := DefaultPublicUdpProbeSettings()
	raised.MaxTargetCount = 128
	lowered := DefaultPublicUdpProbeSettings()
	lowered.MaxTargetCount = 0
	settingsWith := func(mutate func(*PublicUdpProbeSettings)) *PublicUdpProbeSettings {
		settings := DefaultPublicUdpProbeSettings()
		mutate(settings)
		return settings
	}
	cases := []struct {
		name     string
		rows     int
		settings *PublicUdpProbeSettings
		reason   string
	}{
		{name: "the default bound", rows: 64, settings: DefaultPublicUdpProbeSettings(), reason: ""},
		{name: "past the default bound", rows: 65, settings: DefaultPublicUdpProbeSettings(), reason: "target-denominator"},
		{name: "a raised bound", rows: 65, settings: raised, reason: ""},
		{name: "no bound", rows: 1, settings: lowered, reason: "target-denominator"},
		{name: "no settings", rows: 1, settings: nil, reason: "probe-settings"},
		{name: "no attempt timeout", rows: 1, settings: settingsWith(func(settings *PublicUdpProbeSettings) { settings.AttemptTimeout = 0 }), reason: "probe-settings"},
		{name: "a negative attempt timeout", rows: 1, settings: settingsWith(func(settings *PublicUdpProbeSettings) { settings.AttemptTimeout = -time.Second }), reason: "probe-settings"},
		{name: "no packet budget", rows: 1, settings: settingsWith(func(settings *PublicUdpProbeSettings) { settings.MaxPacketCount = 0 }), reason: "probe-settings"},
		{name: "no byte budget", rows: 1, settings: settingsWith(func(settings *PublicUdpProbeSettings) { settings.MaxByteCount = 0 }), reason: "probe-settings"},
	}
	for _, c := range cases {
		requests, reason := publicUdpRequests(publicUdpTestMatrix(c.rows), c.settings, hosts)
		if reason != c.reason || (reason == "") != (len(requests) == c.rows) || reason != "" && requests != nil {
			t.Errorf("%s: reason %q with %d requests, want %q", c.name, reason, len(requests), c.reason)
		}
	}
}

// A transport that records what each attempt was handed and answers with a
// verified handshake whose received packet count is the test's. With a
// barrier count, each attempt waits until that many have entered.
type publicUdpTestSettingsRunner struct {
	probeRunner
	receivedPackets int
	barrierCount    int
	barrierReached  chan struct{}

	stateLock        sync.Mutex
	attemptSettings  []*PublicUdpProbeSettings
	attemptDeadlines []time.Time
	unboundedCount   int
	enteredCount     int
}

func (self *publicUdpTestSettingsRunner) publicUdp(ctx context.Context, request PublicUdpRequest, settings *PublicUdpProbeSettings) (PublicUdpObservation, error) {
	barrierReached := func() <-chan struct{} {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.attemptSettings = append(self.attemptSettings, settings)
		if deadline, ok := ctx.Deadline(); ok {
			self.attemptDeadlines = append(self.attemptDeadlines, deadline)
		} else {
			self.unboundedCount++
		}
		self.enteredCount++
		if self.barrierCount == 0 {
			return nil
		}
		if self.enteredCount == self.barrierCount {
			close(self.barrierReached)
		}
		return self.barrierReached
	}()
	if barrierReached != nil {
		select {
		case <-barrierReached:
		case <-ctx.Done():
			return PublicUdpObservation{}, ctx.Err()
		}
	}
	observation := publicUdpTestHealthy(request)
	observation.ReceivedPackets = self.receivedPackets
	return observation, nil
}

// One pass over one enrolled connect row per name, in both families, with the
// transport in place of the source. With excluded hosts the host scope is
// armed, and the transport sits behind it.
func publicUdpTestSettingsPass(t *testing.T, transport *publicUdpTestSettingsRunner, probe publicUdpProbe, excludedHosts []string, names ...string) []finding {
	t.Helper()
	settings := publicUdpTestSettings(&publicUdpTestSource{syntheticSource: &syntheticSource{}})
	settings.ExcludedHosts = excludedHosts
	template := settings.PublicUdp.Targets[0]
	settings.PublicUdp.Targets, settings.PublicUdp.ExpectedTargets = nil, len(names)
	for index, name := range names {
		target := template
		target.Name, target.Alias = name, fmt.Sprintf("alias-%d", index)
		settings.PublicUdp.Targets = append(settings.PublicUdp.Targets, target)
	}
	env, err := newProbeEnv(settings)
	if err != nil {
		t.Fatal(err)
	}
	if scoped, ok := env.runner.(*hostScopeRunner); ok {
		transport.probeRunner = scoped.probeRunner
		scoped.probeRunner = transport
	} else {
		transport.probeRunner = env.runner
		env.runner = transport
	}
	if _, scoped := env.runner.(*hostScopeRunner); scoped != (0 < len(excludedHosts)) {
		t.Fatal("the host scope was not armed as the test asked")
	}
	findings, err := probe.check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	return findings
}

// The pass hands its own settings to every attempt, bounds each attempt by
// their timeout, and validates the transport's counts against the same
// bounds: a count past the default packet budget is healthy under a raised
// budget, and a count past a lowered budget is not.
func TestPublicUdpPassHandsItsSettingsToTransportAndFindings(t *testing.T) {
	raised := DefaultPublicUdpProbeSettings()
	raised.AttemptTimeout = time.Hour
	raised.MaxPacketCount = 4096
	// directly, and behind the host scope an exclusion arms
	for _, excludedHosts := range [][]string{nil, {"pg-1"}} {
		transport := &publicUdpTestSettingsRunner{receivedPackets: 3000}
		started := time.Now()
		findings := publicUdpTestSettingsPass(t, transport, publicUdpProbe{settings: raised}, excludedHosts, "synthetic-row")
		if len(transport.attemptSettings) != 2 || transport.unboundedCount != 0 {
			t.Fatalf("excluded %v: the transport saw %d attempts, %d of them unbounded", excludedHosts, len(transport.attemptSettings), transport.unboundedCount)
		}
		for index, settings := range transport.attemptSettings {
			if settings != raised {
				t.Fatalf("excluded %v: attempt %d ran under other settings: %+v", excludedHosts, index, settings)
			}
			if transport.attemptDeadlines[index].Before(started.Add(30 * time.Minute)) {
				t.Fatalf("excluded %v: attempt %d was not bounded by the settings' timeout", excludedHosts, index)
			}
		}
		for _, result := range findings {
			if !result.healthy {
				t.Fatalf("excluded %v: a count within the raised budget was refused: %s %s", excludedHosts, result.class, result.observed)
			}
		}
	}

	lowered := DefaultPublicUdpProbeSettings()
	lowered.MaxPacketCount = 8
	transport := &publicUdpTestSettingsRunner{receivedPackets: 9}
	unknown := 0
	for _, result := range publicUdpTestSettingsPass(t, transport, publicUdpProbe{settings: lowered}, nil, "synthetic-row") {
		if result.class == "public-udp-inventory" {
			continue
		}
		if result.healthy || result.class != "public-udp-observation" || result.observed != "verified=false reason=observation-authority-invalid" {
			t.Fatalf("a count past the lowered budget gave %s healthy=%t %s", result.class, result.healthy, result.observed)
		}
		unknown++
	}
	if unknown != 2 {
		t.Fatalf("%d attempts were refused, want both families", unknown)
	}
}

// The worker pool is the settings' size: with six workers all six attempts of
// a pass are in flight together. The attempts' entry is the barrier; the
// attempt timeout only bounds a failing test.
func TestPublicUdpWorkerCountFollowsItsSettings(t *testing.T) {
	settings := DefaultPublicUdpProbeSettings()
	settings.WorkerCount = 6
	transport := &publicUdpTestSettingsRunner{receivedPackets: 2, barrierCount: 6, barrierReached: make(chan struct{})}
	findings := publicUdpTestSettingsPass(t, transport, publicUdpProbe{settings: settings}, nil, "synthetic-row-a", "synthetic-row-b", "synthetic-row-c")
	for _, result := range findings {
		if !result.healthy {
			t.Fatalf("six attempts were not in flight together: %s %s", result.class, result.observed)
		}
	}
	if transport.enteredCount != 6 {
		t.Fatalf("the transport saw %d attempts", transport.enteredCount)
	}
}

// The transport refuses settings that bound nothing before its socket opens,
// and the native adapter does so without reading them first.
func TestPublicUdpTransportRefusesSettingsThatBoundNothing(t *testing.T) {
	untimed := DefaultPublicUdpProbeSettings()
	untimed.AttemptTimeout = 0
	negativelyTimed := DefaultPublicUdpProbeSettings()
	negativelyTimed.AttemptTimeout = -time.Second
	noPackets := DefaultPublicUdpProbeSettings()
	noPackets.MaxPacketCount = 0
	noBytes := DefaultPublicUdpProbeSettings()
	noBytes.MaxByteCount = -1
	native := &runner{cfg: &monitorConfig{commandTimeout: time.Minute}}
	for index, settings := range []*PublicUdpProbeSettings{untimed, negativelyTimed, noPackets, noBytes, nil} {
		request := publicUdpTestNativeRequest("ipv4", "connect", "quic")
		listens := 0
		observed, err := observePublicUdp(context.Background(), request, settings, func(context.Context, string, string) (net.PacketConn, error) {
			listens++
			return nil, errors.New("synthetic-private-socket-error")
		}, nil)
		if err == nil || listens != 0 || observed.Failure != PublicUdpFailureConfiguration || observed.FreshSocket {
			t.Errorf("settings %d: %+v with %d sockets, err=%v", index, observed, listens, err)
		}
		observed, err = native.publicUdp(context.Background(), request, settings)
		findings := publicUdpFindings(request, observed, err, DefaultPublicUdpProbeSettings())
		if err == nil || observed.Failure != PublicUdpFailureConfiguration || observed.FreshSocket ||
			len(findings) != 1 || findings[0].observed != "verified=false reason=configuration-invalid" {
			t.Errorf("settings %d: the native adapter gave %+v err=%v", index, observed, err)
		}
	}
}

// The transport's wire budgets are the settings' own: a byte budget under one
// QUIC Initial datagram refuses the first write, and a one-datagram budget the
// second, either closing the attempt's socket as an exhausted budget that the
// findings under the same settings read as unknown. Under the default budgets
// the same attempt, with no server to answer it, writes a second datagram
// and goes on until it is canceled; the arrival of that datagram is the
// barrier. The context deadline bounds a failing test.
func TestPublicUdpTransportWireBudgetFollowsItsSettings(t *testing.T) {
	underOneInitial := DefaultPublicUdpProbeSettings()
	// QUIC pads a client's Initial datagrams to at least 1200 bytes
	underOneInitial.MaxByteCount = 1000
	oneDatagram := DefaultPublicUdpProbeSettings()
	oneDatagram.MaxPacketCount = 1
	cases := []struct {
		name        string
		settings    *PublicUdpProbeSettings
		sentPackets int
	}{
		{name: "a byte budget under one initial datagram", settings: underOneInitial, sentPackets: 0},
		{name: "a one-datagram budget", settings: oneDatagram, sentPackets: 1},
	}
	for _, c := range cases {
		request := publicUdpTestNativeRequest("ipv4", "connect", "quic")
		client, server := publicUdpTestSocketPair("198.51.100.20:42000", publicUdpTuple(request))
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		observed, err := observePublicUdp(ctx, request, c.settings, func(context.Context, string, string) (net.PacketConn, error) {
			return client, nil
		}, x509.NewCertPool())
		cancel()
		server.Close()
		if err == nil || observed.Failure != PublicUdpFailureBudget || observed.HandshakeComplete || observed.SentPackets != c.sentPackets {
			t.Errorf("%s: %+v err=%v", c.name, observed, err)
			continue
		}
		select {
		case <-client.closed:
		default:
			t.Errorf("%s: the exhausted budget left the socket open", c.name)
		}
		findings := publicUdpFindings(request, observed, err, c.settings)
		if len(findings) != 1 || findings[0].healthy || findings[0].observed != "verified=false reason=wire-budget-exhausted" {
			t.Errorf("%s: the findings read %+v", c.name, findings)
		}
	}

	// the default budgets, with an attempt timeout past the test's own bound
	defaultBudgets := DefaultPublicUdpProbeSettings()
	defaultBudgets.AttemptTimeout = time.Minute
	request := publicUdpTestNativeRequest("ipv4", "connect", "quic")
	client, server := publicUdpTestSocketPair("198.51.100.20:42000", publicUdpTuple(request))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	type result struct {
		observed PublicUdpObservation
		err      error
	}
	done := make(chan result, 1)
	go func() {
		observed, err := observePublicUdp(ctx, request, defaultBudgets, func(context.Context, string, string) (net.PacketConn, error) {
			return client, nil
		}, x509.NewCertPool())
		done <- result{observed: observed, err: err}
	}()
	for range 2 {
		select {
		case <-server.incoming:
		case attempt := <-done:
			t.Fatalf("under the default budgets the attempt ended before its second datagram: %+v err=%v", attempt.observed, attempt.err)
		case <-ctx.Done():
			t.Fatal("under the default budgets the attempt never wrote a second datagram")
		}
	}
	cancel()
	attempt := <-done
	if !errors.Is(attempt.err, context.Canceled) || attempt.observed.Failure == PublicUdpFailureBudget || attempt.observed.SentPackets < 1 {
		t.Fatalf("under the default budgets the attempt gave %+v err=%v", attempt.observed, attempt.err)
	}
}

// The attempt's deadline is the settings' attempt timeout, whatever its
// caller's context allows: a raised timeout reaches the socket's context
// although the caller set no deadline, and a lowered one holds under a
// longer caller deadline. The listener records the deadline it was handed.
func TestPublicUdpTransportBoundsTheAttemptByItsSettings(t *testing.T) {
	raised := DefaultPublicUdpProbeSettings()
	raised.AttemptTimeout = time.Hour
	lowered := DefaultPublicUdpProbeSettings()
	lowered.AttemptTimeout = time.Second
	cases := []struct {
		name           string
		settings       *PublicUdpProbeSettings
		callerDeadline time.Duration
	}{
		{name: "a raised timeout", settings: raised, callerDeadline: 0},
		{name: "a lowered timeout", settings: lowered, callerDeadline: 2 * time.Hour},
	}
	for _, c := range cases {
		ctx, cancel := context.Background(), context.CancelFunc(func() {})
		if 0 < c.callerDeadline {
			ctx, cancel = context.WithTimeout(ctx, c.callerDeadline)
		}
		var deadline time.Time
		bounded := false
		started := time.Now()
		observed, err := observePublicUdp(ctx, publicUdpTestNativeRequest("ipv4", "connect", "quic"), c.settings, func(ctx context.Context, _ string, _ string) (net.PacketConn, error) {
			deadline, bounded = ctx.Deadline()
			return nil, errors.New("synthetic-private-socket-error")
		}, nil)
		finished := time.Now()
		cancel()
		if err == nil || observed.Failure != PublicUdpFailureSocket {
			t.Fatalf("%s: the attempt did not reach its socket: %+v err=%v", c.name, observed, err)
		}
		if !bounded || deadline.Before(started.Add(c.settings.AttemptTimeout)) || deadline.After(finished.Add(c.settings.AttemptTimeout)) {
			t.Errorf("%s: the socket's deadline was %s (bounded=%t), %s after the start", c.name, deadline, bounded, deadline.Sub(started))
		}
	}
}

// A native attempt runs within the runner's command timeout and under the
// settings' own bound, so the shorter applies. A raised attempt timeout
// reaches the attempt up to the command timeout, and in full when no positive
// command timeout caps it; a lowered one and the default hold under the
// monitor's default command timeout. The attempt goes through the runner's
// own adapter, and its socket records the deadline it was opened under.
func TestPublicUdpNativeAttemptIsBoundedWithinTheCommandTimeout(t *testing.T) {
	defaultCommandTimeout := (SignalSettings{}).withDefaults().CommandTimeout
	raised := DefaultPublicUdpProbeSettings()
	raised.AttemptTimeout = time.Hour
	lowered := DefaultPublicUdpProbeSettings()
	lowered.AttemptTimeout = time.Second
	cases := []struct {
		name           string
		settings       *PublicUdpProbeSettings
		commandTimeout time.Duration
		// how long after its start the attempt's deadline falls
		bound time.Duration
	}{
		{name: "a raised timeout under a shorter command timeout", settings: raised, commandTimeout: time.Minute, bound: time.Minute},
		{name: "a raised timeout within a longer command timeout", settings: raised, commandTimeout: 2 * time.Hour, bound: time.Hour},
		{name: "a raised timeout and no command timeout", settings: raised, commandTimeout: 0, bound: time.Hour},
		{name: "a raised timeout and a negative command timeout", settings: raised, commandTimeout: -time.Second, bound: time.Hour},
		{name: "a lowered timeout", settings: lowered, commandTimeout: defaultCommandTimeout, bound: time.Second},
		{name: "the default under the default command timeout", settings: DefaultPublicUdpProbeSettings(), commandTimeout: defaultCommandTimeout, bound: 5 * time.Second},
		{name: "the default and no command timeout", settings: DefaultPublicUdpProbeSettings(), commandTimeout: 0, bound: 5 * time.Second},
	}
	for _, c := range cases {
		var deadline time.Time
		bounded := false
		listens := 0
		native := &runner{cfg: &monitorConfig{commandTimeout: c.commandTimeout}, publicUdpListen: func(ctx context.Context, _ string, _ string) (net.PacketConn, error) {
			listens++
			deadline, bounded = ctx.Deadline()
			return nil, errors.New("synthetic-private-socket-error")
		}}
		started := time.Now()
		observed, err := native.publicUdp(context.Background(), publicUdpTestNativeRequest("ipv4", "connect", "quic"), c.settings)
		finished := time.Now()
		if err == nil || listens != 1 || observed.Failure != PublicUdpFailureSocket {
			t.Fatalf("%s: the attempt did not reach its socket: %+v err=%v", c.name, observed, err)
		}
		if !bounded || deadline.Before(started.Add(c.bound)) || deadline.After(finished.Add(c.bound)) {
			t.Errorf("%s: the socket was opened under a deadline %s after the start (bounded=%t), want %s", c.name, deadline.Sub(started), bounded, c.bound)
		}
	}
}

// The QUIC configuration is the settings': the handshake and idle timeouts
// are the attempt timeout, so a raised one is not cut at quic-go's
// five-second default, and the receive windows are the settings' own and
// never grow, while a non-positive window takes quic-go's default. Only a
// connect api front lets the server open its three h3 streams.
func TestPublicUdpQuicConfigFollowsItsSettings(t *testing.T) {
	tuned := DefaultPublicUdpProbeSettings()
	tuned.AttemptTimeout = time.Hour
	tuned.StreamReceiveWindowByteCount = 24 * 1024
	tuned.ConnectionReceiveWindowByteCount = 48 * 1024
	unwindowed := DefaultPublicUdpProbeSettings()
	unwindowed.StreamReceiveWindowByteCount = 0
	unwindowed.ConnectionReceiveWindowByteCount = -1
	cases := []struct {
		name       string
		settings   *PublicUdpProbeSettings
		apiRequest bool
		want       quic.Config
	}{
		{name: "tuned, handshake only", settings: tuned, apiRequest: false, want: quic.Config{
			HandshakeIdleTimeout: time.Hour, MaxIdleTimeout: time.Hour, InitialPacketSize: connectcore.H3InitialPacketByteCount,
			InitialStreamReceiveWindow: 24 * 1024, MaxStreamReceiveWindow: 24 * 1024,
			InitialConnectionReceiveWindow: 48 * 1024, MaxConnectionReceiveWindow: 48 * 1024,
			MaxIncomingStreams: -1, MaxIncomingUniStreams: -1, EnableDatagrams: true,
		}},
		{name: "tuned, with /hello", settings: tuned, apiRequest: true, want: quic.Config{
			HandshakeIdleTimeout: time.Hour, MaxIdleTimeout: time.Hour, InitialPacketSize: connectcore.H3InitialPacketByteCount,
			InitialStreamReceiveWindow: 24 * 1024, MaxStreamReceiveWindow: 24 * 1024,
			InitialConnectionReceiveWindow: 48 * 1024, MaxConnectionReceiveWindow: 48 * 1024,
			MaxIncomingStreams: -1, MaxIncomingUniStreams: 3, EnableDatagrams: true,
		}},
		{name: "the defaults", settings: DefaultPublicUdpProbeSettings(), apiRequest: false, want: quic.Config{
			HandshakeIdleTimeout: 5 * time.Second, MaxIdleTimeout: 5 * time.Second, InitialPacketSize: connectcore.H3InitialPacketByteCount,
			InitialStreamReceiveWindow: 16 * 1024, MaxStreamReceiveWindow: 16 * 1024,
			InitialConnectionReceiveWindow: 32 * 1024, MaxConnectionReceiveWindow: 32 * 1024,
			MaxIncomingStreams: -1, MaxIncomingUniStreams: -1, EnableDatagrams: true,
		}},
		{name: "no windows", settings: unwindowed, apiRequest: false, want: quic.Config{
			HandshakeIdleTimeout: 5 * time.Second, MaxIdleTimeout: 5 * time.Second, InitialPacketSize: connectcore.H3InitialPacketByteCount,
			MaxIncomingStreams: -1, MaxIncomingUniStreams: -1, EnableDatagrams: true,
		}},
	}
	for _, c := range cases {
		if quicConfig := c.settings.quicConfig(c.apiRequest); !reflect.DeepEqual(*quicConfig, c.want) {
			t.Errorf("%s: %+v, want %+v", c.name, *quicConfig, c.want)
		}
	}
}

// The receive side reads its budgets from the settings too: past a lowered
// packet or byte budget an inbound datagram, expected or not, closes the
// socket as an exhausted budget, and past the default budgets a raised one
// accepts every datagram from the pinned peer.
func TestPublicUdpTransportReceiveBudgetFollowsItsSettings(t *testing.T) {
	peer := netip.MustParseAddrPort("192.0.2.20:443")
	stranger := net.UDPAddrFromAddrPort(netip.MustParseAddrPort("192.0.2.21:443"))
	fewPackets := DefaultPublicUdpProbeSettings()
	fewPackets.MaxPacketCount = 4
	fewBytes := DefaultPublicUdpProbeSettings()
	fewBytes.MaxByteCount = 100
	morePackets := DefaultPublicUdpProbeSettings()
	morePackets.MaxPacketCount = 2 * DefaultPublicUdpProbeSettings().MaxPacketCount
	moreBytes := DefaultPublicUdpProbeSettings()
	moreBytes.MaxByteCount = 2 * DefaultPublicUdpProbeSettings().MaxByteCount
	cases := []struct {
		name     string
		settings *PublicUdpProbeSettings
		// unexpected datagrams first, then the pinned peer's
		unexpectedCount   int
		expectedCount     int
		datagramByteCount int
		// the pinned peer's datagrams accepted before the budget closes the
		// socket; all of them when it never does
		acceptedCount int
		exhausted     bool
	}{
		{name: "a lowered packet budget", settings: fewPackets, unexpectedCount: 4, expectedCount: 1, datagramByteCount: 1, acceptedCount: 0, exhausted: true},
		{name: "a lowered byte budget", settings: fewBytes, unexpectedCount: 0, expectedCount: 2, datagramByteCount: 60, acceptedCount: 1, exhausted: true},
		{
			name: "a raised packet budget", settings: morePackets, unexpectedCount: 0, expectedCount: DefaultPublicUdpProbeSettings().MaxPacketCount + 1, datagramByteCount: 1,
			acceptedCount: DefaultPublicUdpProbeSettings().MaxPacketCount + 1, exhausted: false,
		},
		{
			name: "a raised byte budget", settings: moreBytes, unexpectedCount: 0, expectedCount: DefaultPublicUdpProbeSettings().MaxByteCount/(32*1024) + 1, datagramByteCount: 32 * 1024,
			acceptedCount: DefaultPublicUdpProbeSettings().MaxByteCount/(32*1024) + 1, exhausted: false,
		},
	}
	for _, c := range cases {
		client, server := publicUdpTestSocketPair("198.51.100.20:42000", peer.String())
		wire := &publicUdpPacketConn{PacketConn: client, peer: peer, settings: c.settings}
		for range c.unexpectedCount {
			client.incoming <- publicUdpTestPacket{data: make([]byte, c.datagramByteCount), from: stranger}
		}
		accepted := 0
		var err error
		buffer := make([]byte, 64*1024)
		for range c.expectedCount {
			// one at a time, past the synthetic socket's queue
			client.incoming <- publicUdpTestPacket{data: make([]byte, c.datagramByteCount), from: server.address}
			if _, _, err = wire.ReadFrom(buffer); err != nil {
				break
			}
			accepted++
		}
		observed := wire.observation()
		exhausted := errors.Is(err, errPublicUdpBudget) && observed.Failure == PublicUdpFailureBudget
		if exhausted != c.exhausted || accepted != c.acceptedCount || observed.ReceivedPackets != c.acceptedCount || observed.UnexpectedPackets != c.unexpectedCount {
			t.Errorf("%s: %d accepted, exhausted=%t, %+v err=%v", c.name, accepted, exhausted, observed, err)
		}
		select {
		case <-client.closed:
			if !c.exhausted {
				t.Errorf("%s: the socket closed within its budget", c.name)
			}
		default:
			if c.exhausted {
				t.Errorf("%s: the exhausted budget left the socket open", c.name)
			}
		}
		client.Close()
		server.Close()
	}
}
