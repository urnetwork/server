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
	"sync"
	"testing"
	"time"
)

// The defaults are the probe's documented bounds, and each call returns its
// own instance, the api request settings included.
func TestPublicUdpProbeSettingsDefaultsAreFreshInstances(t *testing.T) {
	want := PublicUdpProbeSettings{
		Cadence:        5 * time.Minute,
		AttemptTimeout: 5 * time.Second,
		MaxTargetCount: 32,
		WorkerCount:    4,
		MaxPacketCount: 2048,
		MaxByteCount:   1024 * 1024,
		ApiRequest:     PublicUdpApiRequestSettings{ResponseHeaderByteLimit: 16 * 1024, ResponseBodyByteLimit: 16 * 1024},
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
	raised.MaxTargetCount = 64
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
		{name: "the default bound", rows: 32, settings: DefaultPublicUdpProbeSettings(), reason: ""},
		{name: "past the default bound", rows: 33, settings: DefaultPublicUdpProbeSettings(), reason: "target-denominator"},
		{name: "a raised bound", rows: 33, settings: raised, reason: ""},
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
// the same attempt, with no server to answer it, only times out. The context
// deadline bounds a failing test.
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
}
