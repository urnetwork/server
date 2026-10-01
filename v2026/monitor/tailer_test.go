package monitor

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fakeStream builds the tailer's injected stream around an arbitrary shell
// script. Tests that need runner.warpctlStream's stdout/stderr boundary use the
// real runner below.
func fakeStream(script string) func(ctx context.Context) (*exec.Cmd, io.ReadCloser, error) {
	return func(ctx context.Context) (*exec.Cmd, io.ReadCloser, error) {
		cmd := exec.CommandContext(ctx, "sh", "-c", script)
		pr, pw, err := os.Pipe()
		if err != nil {
			return nil, nil, err
		}
		cmd.Stdout = pw
		cmd.Stderr = pw
		if err := cmd.Start(); err != nil {
			pr.Close()
			pw.Close()
			return nil, nil, err
		}
		pw.Close()
		return cmd, pr, nil
	}
}

// A Loki failure belongs to the local observation transport, not to the
// remote service whose logs were requested. The production failure was an
// exhausted 502 retry whose stderr included `panic:`; when warpctlStream
// merged stderr with stdout, every standing service tailer classified that as
// a page-tier service panic.
func TestWarpctlStreamDoesNotClassifyTransportStderr(t *testing.T) {
	binDir := t.TempDir()
	warpctlPath := filepath.Join(binDir, "warpctl")
	script := `#!/bin/sh
printf '%s\n' '[edge-0][taskworker][g1][cid:abc] ordinary remote log line'
printf '%s\n' 'panic: Loki query error (502): Bad Gateway' >&2
`
	if err := os.WriteFile(warpctlPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)

	cfg := &monitorConfig{env: "main"}
	streamRunner := newRunner(cfg)
	var operatorDiagnostics strings.Builder
	streamRunner.operatorDiagnostics = &operatorDiagnostics
	tailer := newLogTailer("taskworker", &probeEnv{
		cfg:    cfg,
		runner: streamRunner,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tailer.tailOnce(ctx); err != nil {
		t.Fatalf("tailOnce: %v", err)
	}
	const expectedOperatorDiagnostic = "panic: Loki query error (502): Bad Gateway\n"
	if operatorDiagnostics.String() != expectedOperatorDiagnostic {
		t.Fatalf("operator diagnostics = %q; want %q", operatorDiagnostics.String(), expectedOperatorDiagnostic)
	}

	if finding := findingByClass(t, tailer.drainWindow(), "panic"); !finding.healthy {
		t.Fatalf("local warpctl stderr became a remote panic finding: %+v", finding)
	}
}

func TestWarpctlStreamAggregatesInternalIPv6RouteLossWithoutServiceClassification(t *testing.T) {
	binDir := t.TempDir()
	warpctlPath := filepath.Join(binDir, "warpctl")
	script := `#!/bin/sh
printf '%s\n' '[edge-0][api][g1][cid:abc] ordinary remote log line'
printf '%s\n' '2026/09/01 06:59:49 client.go:473: Tail read error (read tcp [2001:db8:1::10]:62001->[2001:db8:2::44]:443: read: no route to host). Reconnecting.' >&2
`
	if err := os.WriteFile(warpctlPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)

	cfg := &monitorConfig{
		env: "main",
		hosts: []*host{{
			name: "edge-4",
			edgeIPv6: []EdgeIPv6InterfaceSettings{{
				Interface: "eno3",
				Address:   "2001:db8:2::44",
			}},
		}},
	}
	streamRunner := newRunner(cfg)
	var operatorDiagnostics strings.Builder
	streamRunner.operatorDiagnostics = &operatorDiagnostics
	env := &probeEnv{cfg: cfg, runner: streamRunner}
	tailer := newLogTailer("api", env)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tailer.tailOnce(ctx); err != nil {
		t.Fatalf("tailOnce: %v", err)
	}
	const expectedOperatorDiagnostic = "2026/09/01 06:59:49 client.go:473: Tail read error (read tcp [2001:db8:1::10]:62001->[2001:db8:2::44]:443: read: no route to host). Reconnecting.\n"
	if operatorDiagnostics.String() != expectedOperatorDiagnostic {
		t.Fatalf("operator diagnostics = %q; want %q", operatorDiagnostics.String(), expectedOperatorDiagnostic)
	}

	probe := &logTailProbe{tailers: []*logTailer{tailer}}
	findings, err := probe.check(ctx, env)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	routeLoss := findingByClass(t, findings, "tailer-ipv6-route-loss")
	if routeLoss.healthy {
		t.Fatalf("warpctl's internal reconnect was invisible: %+v", routeLoss)
	}
	for _, want := range []string{
		"edge-4",
		"eno3/2001:db8:2::44",
		"route_errors=1",
		"services=1",
		"service_sample=api",
		"first_local=2026/09/01 06:59:49",
		"stderr",
		"three pinned HTTP/1.1 requests return 200",
		"unrelated provider IPv6 prefix",
	} {
		combined := strings.Join([]string{
			routeLoss.target,
			routeLoss.frame,
			routeLoss.observed,
			routeLoss.evidence,
			routeLoss.verify,
		}, "\n")
		if !strings.Contains(combined, want) {
			t.Fatalf("route-loss finding missing %q:\n%+v", want, routeLoss)
		}
	}
	if panicFinding := findingByClass(t, findings, "panic"); !panicFinding.healthy {
		t.Fatalf("transport stderr became a remote panic: %+v", panicFinding)
	}

	resolved, err := probe.check(ctx, env)
	if err != nil {
		t.Fatalf("resolved check: %v", err)
	}
	if routeFinding := findingByClass(t, resolved, "tailer-ipv6-route-loss"); !routeFinding.healthy {
		t.Fatalf("drained transport event did not resolve: %+v", routeFinding)
	}
}

func TestTailTransportDiagnosticWriterReassemblesAndAggregatesServices(t *testing.T) {
	const diagnostic = "2026/09/01 06:59:49 client.go:473: Tail read error (read tcp [2001:db8:1::10]:62001->[2001:db8:2::44]:443: read: no route to host). Reconnecting.\n"
	tailers := []*logTailer{
		newLogTailer("api", nil),
		newLogTailer("connect", nil),
		newLogTailer("taskworker", nil),
	}
	for _, tailer := range tailers {
		writer := &tailTransportDiagnosticWriter{tailer: tailer}
		for _, fragment := range []string{diagnostic[:17], diagnostic[17:73], diagnostic[73:]} {
			if _, err := writer.Write([]byte(fragment)); err != nil {
				t.Fatalf("Write: %v", err)
			}
		}
	}

	findings, err := (&logTailProbe{tailers: tailers}).check(context.Background(), nil)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	routeLoss := findingByClass(t, findings, "tailer-ipv6-route-loss")
	if routeLoss.healthy {
		t.Fatalf("fragmented diagnostics were not aggregated: %+v", routeLoss)
	}
	for _, want := range []string{
		"route_errors=3",
		"services=3",
		"service_sample=api,connect,taskworker",
		"unknown-edge-ipv6",
	} {
		combined := routeLoss.symptom + "\n" + routeLoss.target + "\n" + routeLoss.observed
		if !strings.Contains(combined, want) {
			t.Fatalf("aggregated finding missing %q:\n%+v", want, routeLoss)
		}
	}
}

func TestParseTailTransportMonitorRouteEvidence(t *testing.T) {
	out := strings.Join([]string{
		"2026-09-01 06:59:48.000 Df configd[1:2] AUTOMATIC-V6 en8: all autoconf addresses detached/deprecated",
		"2026-09-01 06:59:49.250 Df configd[1:2] RTADV en0: router lifetime became zero",
		"2026-09-01 06:59:49.500 Df configd[1:2] network changed: v4(en0)",
		"2026-09-01 06:59:50.000 Df configd[1:2] AUTOMATIC-V6 en0: all autoconf addresses detached/deprecated",
		"2026-09-01 06:59:55.750 Df configd[1:2] network changed: v4(en0) v6(en0:ready)",
	}, "\n")

	evidence := parseTailTransportMonitorRouteEvidence(out)
	if evidence.interfaceName != "en0" || evidence.routerLifetimeExpiredCount != 1 || evidence.autoconfDetachCount != 1 {
		t.Fatalf("wrong monitor route evidence: %+v", evidence)
	}
	if got := evidence.routerLifetimeExpiredAt.Format(monitorIPv6LogTimeLayout); got != "2026-09-01 06:59:49.250" {
		t.Fatalf("router lifetime expiry time = %q", got)
	}
	if got := evidence.ipv6AbsentAt.Format(monitorIPv6LogTimeLayout); got != "2026-09-01 06:59:49.500" {
		t.Fatalf("IPv6 absence time = %q", got)
	}
	if got := evidence.ipv6RestoredAt.Format(monitorIPv6LogTimeLayout); got != "2026-09-01 06:59:55.750" {
		t.Fatalf("IPv6 restoration time = %q", got)
	}
}

func TestParseTailTransportMonitorRouteEvidenceDoesNotInferExpiryFromExplicitZeroLifetimeLog(t *testing.T) {
	out := strings.Join([]string{
		"2026-09-01 06:59:49.250 Df configd[1:2] RTADV en0: ignoring RA (lifetime zero)",
		"2026-09-01 06:59:49.500 Df configd[1:2] network changed: v4(en0)",
	}, "\n")

	evidence := parseTailTransportMonitorRouteEvidence(out)
	if evidence.interfaceName != "" || evidence.routerLifetimeExpiredCount != 0 || !evidence.routerLifetimeExpiredAt.IsZero() {
		t.Fatalf("an adjacent configd message was misclassified as stored-router expiry: %+v", evidence)
	}
}

// Both configd prefixes carry detach evidence, not the cause of router expiry.
func TestTailTransportRouteLossRetainsRtadvDetachEvidence(t *testing.T) {
	lines := []string{
		"2026-01-02 03:04:05.000 Df configd[1:2] RTADV observer0: router lifetime became zero",
		"2026-01-02 03:04:05.030 Df configd[1:2] network changed: v4(observer0:192.0.2.10)",
		"2026-01-02 03:04:05.100 Df configd[1:2] RTADV other0: all autoconf addresses detached/deprecated",
		"2026-01-02 03:04:05.200 Df configd[1:2] AUTOMATIC-V6 other0: all autoconf addresses detached/deprecated",
		"2026-01-02 03:04:05.300 Df configd[1:2] OTHER observer0: all autoconf addresses detached/deprecated",
		"2026-01-02 03:04:05.900 Df configd[1:2] RTADV observer0: all autoconf addresses detached/deprecated",
		"2026-01-02 03:04:12.100 Df configd[1:2] RTADV observer0: all autoconf addresses detached/deprecated",
		"2026-01-02 03:04:12.130 Df configd[1:2] network changed: v4(observer0:192.0.2.10) v6(observer0:2001:db8:1::10)",
	}
	evidence := parseTailTransportMonitorRouteEvidence(strings.Join(lines, "\n"))
	if evidence.interfaceName != "observer0" || evidence.routerLifetimeExpiredCount != 1 || evidence.autoconfDetachCount != 2 {
		t.Fatalf("RTADV evidence: interface=%q expirations=%d detaches=%d; want observer0, 1, 2", evidence.interfaceName, evidence.routerLifetimeExpiredCount, evidence.autoconfDetachCount)
	}
	event := &tailTransportRouteAggregate{
		address:  "2001:db8:2::44",
		count:    2,
		first:    "2026/01/02 03:04:10",
		last:     "2026/01/02 03:04:11",
		services: map[string]struct{}{"synthetic-service": {}},
	}
	if !evidence.matches(event) || evidence.ipv6RestoredAt.Sub(evidence.ipv6AbsentAt) != 7100*time.Millisecond {
		t.Fatalf("RTADV interval: matches=%t duration=%s; want true, 7.1s", evidence.matches(event), evidence.ipv6RestoredAt.Sub(evidence.ipv6AbsentAt))
	}
	loss := findingByClass(t, tailTransportRouteFindings(nil, map[string]*tailTransportRouteAggregate{event.address: event}, evidence), "tailer-ipv6-route-loss")
	if loss.healthy || !strings.Contains(loss.observed, "monitor_autoconf_detach=2") {
		t.Fatalf("RTADV finding: healthy=%t observed=%q; want unhealthy with detach count 2", loss.healthy, loss.observed)
	}
	for _, want := range []string{
		"separately recorded 2 autoconfiguration detach/deprecate transition(s)",
		"does not distinguish an explicit zero-lifetime Router Advertisement from missed or late refresh advertisements",
		"or local RA-state invalidation",
		"IPv6 network state returned at 2026-01-02 03:04:12.130",
	} {
		if !strings.Contains(loss.mechanism, want) {
			t.Fatalf("RTADV mechanism %q omitted %q", loss.mechanism, want)
		}
	}
	if !strings.Contains(loss.action, "Do not change the named production edge") || !strings.Contains(loss.verify, "For at least 30 minutes") {
		t.Fatalf("RTADV action=%q verify=%q omitted the edge exclusion or 30-minute gate", loss.action, loss.verify)
	}
	alert := alertFromFinding(syntheticSettings(nil), "1.5", "log-errors", "Log error-class rates", loss)
	for _, want := range []string{
		"With explicit operator authorization",
		"Correlate complete packet coverage with local state",
		"local RA-state invalidation",
		"An absent packet in incomplete capture is not proof of missed delivery",
		"Select a router, delivery-path, or local-client repair only after that discriminator",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("route-loss Markdown omitted causal or authorization limit %q: %s", want, alert.Markdown())
		}
	}
	for _, unsupported := range []string{"Independent warpctl tails lost their common local route", "repair the identified RA source or delivery path"} {
		if strings.Contains(alert.Markdown(), unsupported) {
			t.Fatalf("single-tail route-loss Markdown retained unsupported scope or repair claim %q: %s", unsupported, alert.Markdown())
		}
	}
	for _, raw := range []string{"configd[", "network changed:", "192.0.2.10", "2001:db8:1::10", "other0", "RTADV observer0:"} {
		if strings.Contains(alert.Markdown(), raw) {
			t.Fatalf("route-loss Markdown retained synthetic local log fragment %q", raw)
		}
	}
}

// Detach records alone must not create a stored-router-expiration interval.
func TestParseTailTransportMonitorRouteEvidenceRequiresExpiryForEitherDetachPrefix(t *testing.T) {
	for _, prefix := range []string{"AUTOMATIC-V6", "RTADV"} {
		evidence := parseTailTransportMonitorRouteEvidence(
			"2026-01-02 03:04:05.000 Df configd[1:2] " + prefix + " observer0: all autoconf addresses detached/deprecated\n" +
				"2026-01-02 03:04:05.030 Df configd[1:2] network changed: v4(observer0:192.0.2.10)\n",
		)
		if evidence.interfaceName != "" || evidence.autoconfDetachCount != 0 || !evidence.routerLifetimeExpiredAt.IsZero() || !evidence.ipv6AbsentAt.IsZero() {
			t.Fatalf("%s detach-only records invented local router expiry: %+v", prefix, evidence)
		}
	}
}

func TestTailTransportRouteLossUsesMonitorRouterLifetimeExpirationDiscriminator(t *testing.T) {
	tailer := newLogTailer("api", nil)
	tailer.recordTransportDiagnostic("2026/09/01 06:59:49 client.go:473: Tail read error (read tcp [2001:db8:1::10]:62001->[2001:db8:2::44]:443: read: no route to host). Reconnecting.")
	probe := &logTailProbe{
		tailers: []*logTailer{tailer},
		monitorRouteEvidence: func(context.Context, *probeEnv, map[string]*tailTransportRouteAggregate) tailTransportMonitorRouteEvidence {
			expiredAt, err := time.ParseInLocation(monitorIPv6LogTimeLayout, "2026-09-01 06:59:49.250", time.Local)
			if err != nil {
				t.Fatal(err)
			}
			restoredAt, err := time.ParseInLocation(monitorIPv6LogTimeLayout, "2026-09-01 06:59:55.750", time.Local)
			if err != nil {
				t.Fatal(err)
			}
			return tailTransportMonitorRouteEvidence{
				interfaceName:              "en0",
				routerLifetimeExpiredAt:    expiredAt,
				routerLifetimeExpiredCount: 1,
				autoconfDetachCount:        2,
				ipv6AbsentAt:               expiredAt.Add(250 * time.Millisecond),
				ipv6RestoredAt:             restoredAt,
			}
		},
	}
	env := &probeEnv{cfg: &monitorConfig{hosts: []*host{{
		name: "edge-4",
		edgeIPv6: []EdgeIPv6InterfaceSettings{{
			Interface: "eno3",
			Address:   "2001:db8:2::44",
		}},
	}}}}

	findings, err := probe.check(context.Background(), env)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	routeLoss := findingByClass(t, findings, "tailer-ipv6-route-loss")
	if routeLoss.healthy {
		t.Fatalf("monitor-local router expiry was not reported: %+v", routeLoss)
	}
	combined := strings.Join([]string{
		routeLoss.mechanism,
		routeLoss.observed,
		routeLoss.evidence,
		routeLoss.context,
		routeLoss.action,
		routeLoss.verify,
	}, "\n")
	for _, want := range []string{
		"default-router lifetime on en0 reached zero",
		"monitor_interface=en0",
		"monitor_router_lifetime_expired=1",
		"monitor_autoconf_detach=2",
		"The bounded window separately recorded 2 autoconfiguration detach/deprecate transition(s).",
		"monitor_ipv6_absent=2026-09-01 06:59:49.500",
		"IPv6 network state returned at 2026-09-01 06:59:55.750.",
		"supersedes edge attribution",
		"not evidence that the named production edge",
		"Do not change the named production edge",
		"does not distinguish an explicit zero-lifetime Router Advertisement from missed or late refresh advertisements",
		"capture timestamped ICMPv6 type 134",
		"For at least 30 minutes",
	} {
		if !strings.Contains(combined, want) {
			t.Fatalf("monitor-local discriminator missing %q:\n%+v", want, routeLoss)
		}
	}
	if strings.HasPrefix(routeLoss.action, "Immediately run the §18.1 exact-address battery") {
		t.Fatalf("locally proven router expiry still starts with edge diagnosis: %s", routeLoss.action)
	}
}

func TestTailTransportRouteLossWithoutDetachDoesNotInventTransition(t *testing.T) {
	for _, restored := range []bool{false, true} {
		t.Run(fmt.Sprintf("restored=%t", restored), func(t *testing.T) {
			lines := []string{
				"2026-09-15 19:41:12.924 Df configd[1:2] RTADV en0: router lifetime became zero",
				"2026-09-15 19:41:12.961 Df configd[1:2] network changed: v4(en0:192.0.2.10) DNS! Proxy!",
			}
			if restored {
				lines = append(lines, "2026-09-15 19:41:21.562 Df configd[1:2] network changed: v4(en0:192.0.2.10) v6(en0:2001:db8:1::10) DNS! Proxy!")
			}
			tailer := newLogTailer("api", nil)
			tailer.recordTransportDiagnostic("2026/09/15 19:41:20 client.go:473: Tail read error (read tcp [2001:db8:1::10]:62001->[2001:db8:2::44]:443: read: no route to host). Reconnecting.")
			probe := &logTailProbe{
				tailers: []*logTailer{tailer},
				monitorRouteEvidence: func(context.Context, *probeEnv, map[string]*tailTransportRouteAggregate) tailTransportMonitorRouteEvidence {
					return parseTailTransportMonitorRouteEvidence(strings.Join(lines, "\n"))
				},
			}
			env := &probeEnv{cfg: &monitorConfig{hosts: []*host{{
				name:     "synthetic-edge",
				edgeIPv6: []EdgeIPv6InterfaceSettings{{Interface: "public0", Address: "2001:db8:2::44"}},
			}}}}
			findings, err := probe.check(context.Background(), env)
			if err != nil {
				t.Fatal(err)
			}
			loss := findingByClass(t, findings, "tailer-ipv6-route-loss")
			if loss.healthy || loss.target != "synthetic-edge" || loss.frame != "public0/2001:db8:2::44" || loss.sustain != 1 {
				t.Fatal("zero-detach evidence changed route-loss identity or visibility")
			}
			for _, want := range []string{
				"default-router lifetime on en0 reached zero at 2026-09-15 19:41:12.924",
				"loss of that interface's IPv6 network state at 2026-09-15 19:41:12.961",
				"does not distinguish an explicit zero-lifetime Router Advertisement from missed or late refresh advertisements",
			} {
				if !strings.Contains(loss.mechanism, want) {
					t.Fatalf("zero-detach mechanism omitted proved boundary %q", want)
				}
			}
			if strings.Contains(loss.mechanism, "detach") || strings.Contains(loss.mechanism, "deprecat") {
				t.Fatal("zero-detach evidence asserted an unobserved detach/deprecate transition")
			}
			if strings.Contains(loss.mechanism, "IPv6 network state returned at") != restored {
				t.Fatal("restoration claim does not match the bounded evidence")
			}
			if !strings.Contains(loss.observed, "monitor_autoconf_detach=0") ||
				!strings.Contains(loss.evidence, "0 autoconfiguration detach/deprecate transition(s)") ||
				!strings.Contains(loss.action, "Do not change the named production edge") ||
				!strings.Contains(loss.verify, "For at least 30 minutes") {
				t.Fatal("zero-detach correction lost the measured count, owner or verification gate")
			}
			alert := alertFromFinding(syntheticSettings(nil), "1.5", "log-errors", "Log error-class rates", loss)
			for _, raw := range []string{"configd[", "network changed:", "then detached/deprecated"} {
				if strings.Contains(alert.Markdown(), raw) {
					t.Fatal("route-loss Markdown retained raw source text or the old unsupported claim")
				}
			}
		})
	}
}

func TestTailTransportMonitorRouteEvidenceRequiresSameWindowIPv6Loss(t *testing.T) {
	event := &tailTransportRouteAggregate{
		first: "2026/09/01 06:59:49",
		last:  "2026/09/01 06:59:50",
	}
	expiredAt, err := time.ParseInLocation(monitorIPv6LogTimeLayout, "2026-09-01 06:59:49.250", time.Local)
	if err != nil {
		t.Fatal(err)
	}
	base := tailTransportMonitorRouteEvidence{
		interfaceName:           "en0",
		routerLifetimeExpiredAt: expiredAt,
		ipv6AbsentAt:            expiredAt.Add(250 * time.Millisecond),
	}
	if !base.matches(event) {
		t.Fatal("same-window router expiry and IPv6 loss did not match")
	}
	preceding := base
	preceding.routerLifetimeExpiredAt = expiredAt.Add(-9 * time.Second)
	preceding.ipv6AbsentAt = preceding.routerLifetimeExpiredAt.Add(250 * time.Millisecond)
	if !preceding.matches(event) {
		t.Fatal("proven nine-second router-expiry precursor did not match")
	}
	activeInterval := base
	activeInterval.routerLifetimeExpiredAt = expiredAt.Add(-27 * time.Second)
	activeInterval.ipv6AbsentAt = activeInterval.routerLifetimeExpiredAt.Add(33 * time.Millisecond)
	if !activeInterval.matches(event) {
		t.Fatal("active 27-second IPv6-loss interval did not match the later tail error")
	}
	restoredWithinDeliveryGrace := activeInterval
	restoredWithinDeliveryGrace.ipv6RestoredAt = expiredAt.Add(-time.Second)
	if !restoredWithinDeliveryGrace.matches(event) {
		t.Fatal("sub-second restoration-to-diagnostic propagation was not treated as causal")
	}
	restoredOutsideDeliveryGrace := activeInterval
	restoredOutsideDeliveryGrace.ipv6RestoredAt = expiredAt.Add(
		-monitorIPv6RestorationDiagnosticGracePeriod - 251*time.Millisecond,
	)
	if restoredOutsideDeliveryGrace.matches(event) {
		t.Fatal("route loss restored outside the bounded delivery grace was treated as causal")
	}
	first, _, ok := tailTransportRouteEventBounds(map[string]*tailTransportRouteAggregate{"synthetic": event})
	if !ok {
		t.Fatal("synthetic route event has no valid bounds")
	}
	for _, test := range []struct {
		delay time.Duration
		want  bool
	}{
		{delay: monitorIPv6RestorationDiagnosticGracePeriod - time.Millisecond, want: true},
		{delay: monitorIPv6RestorationDiagnosticGracePeriod, want: true},
		{delay: monitorIPv6RestorationDiagnosticGracePeriod + time.Millisecond, want: false},
	} {
		boundary := activeInterval
		boundary.ipv6RestoredAt = first.Add(-test.delay)
		if got := boundary.matches(event); got != test.want {
			t.Fatalf("restoration delay=%s matches=%t; want %t", test.delay, got, test.want)
		}
	}
	withoutLoss := base
	withoutLoss.ipv6AbsentAt = time.Time{}
	if withoutLoss.matches(event) {
		t.Fatal("router-lifetime log without local IPv6 loss was treated as causal")
	}
	distant := base
	distant.routerLifetimeExpiredAt = expiredAt.Add(-monitorIPv6RouteStateLookback - time.Second)
	distant.ipv6AbsentAt = distant.routerLifetimeExpiredAt.Add(time.Second)
	if distant.matches(event) {
		t.Fatal("distant router expiry was correlated to this transport event")
	}
}

func TestTailTransportRouteLossUsesBoundedPostRestoreDiagnosticGrace(t *testing.T) {
	const diagnostic = "2026/09/12 11:36:27 client.go:473: Tail read error (read tcp [2001:db8:1::10]:62001->[2001:db8:2::44]:443: read: no route to host). Reconnecting."
	tailer := newLogTailer("synthetic-service", nil)
	tailer.recordTransportDiagnostic(diagnostic)

	parseTime := func(value string) time.Time {
		t.Helper()
		parsed, err := time.ParseInLocation(monitorIPv6LogTimeLayout, value, time.Local)
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	probe := &logTailProbe{
		tailers: []*logTailer{tailer},
		monitorRouteEvidence: func(context.Context, *probeEnv, map[string]*tailTransportRouteAggregate) tailTransportMonitorRouteEvidence {
			return tailTransportMonitorRouteEvidence{
				interfaceName:           "en0",
				routerLifetimeExpiredAt: parseTime("2026-09-12 11:36:19.284"),
				ipv6AbsentAt:            parseTime("2026-09-12 11:36:19.313"),
				ipv6RestoredAt:          parseTime("2026-09-12 11:36:26.067"),
			}
		},
	}
	env := &probeEnv{cfg: &monitorConfig{hosts: []*host{{
		name: "synthetic-edge",
		edgeIPv6: []EdgeIPv6InterfaceSettings{{
			Interface: "public0",
			Address:   "2001:db8:2::44",
		}},
	}}}}

	findings, err := probe.check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	routeLoss := findingByClass(t, findings, "tailer-ipv6-route-loss")
	if routeLoss.healthy {
		t.Fatalf("bounded post-restoration diagnostic was not attributed locally: %+v", routeLoss)
	}
	alert := alertFromFinding(
		syntheticSettings(nil),
		"1.5",
		"log-errors",
		"Log error-class rates",
		routeLoss,
	)
	markdown := alert.Markdown()
	for _, want := range []string{
		"monitor_restoration_before_diagnostic=933ms",
		"bounded 2s delivery grace",
		"larger restored gaps remain ineligible",
		"monitor-side first-hop",
		"Do not change the named production edge",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("bounded restoration alert Markdown missing %q:\n%s", want, markdown)
		}
	}
	for _, rawLogFragment := range []string{"configd[", "network changed:"} {
		if strings.Contains(markdown, rawLogFragment) {
			t.Fatalf("bounded restoration alert retained raw local log fragment %q:\n%s", rawLogFragment, markdown)
		}
	}
}

func TestParseTailTransportMonitorRouteEvidenceUsesLatestActiveLossInterval(t *testing.T) {
	out := strings.Join([]string{
		"2026-09-04 10:17:46.534 Df configd[1:2] RTADV en0: router lifetime became zero",
		"2026-09-04 10:17:46.565 Df configd[1:2] network changed: v4(en0)",
		"2026-09-04 10:17:54.000 Df configd[1:2] network changed: v4(en0) v6(en0:ready)",
		"2026-09-04 10:18:25.000 Df configd[1:2] RTADV en0: router lifetime became zero",
		"2026-09-04 10:18:25.033 Df configd[1:2] network changed: v4(en0)",
		"2026-09-04 10:18:26.000 Df configd[1:2] AUTOMATIC-V6 en0: all autoconf addresses detached/deprecated",
	}, "\n")

	evidence := parseTailTransportMonitorRouteEvidence(out)
	event := &tailTransportRouteAggregate{
		first: "2026/09/04 10:18:52",
		last:  "2026/09/04 10:18:52",
	}
	if !evidence.matches(event) {
		t.Fatalf("latest active loss interval did not match 27-second-later tail error: %+v", evidence)
	}
	if got := evidence.routerLifetimeExpiredAt.Format(monitorIPv6LogTimeLayout); got != "2026-09-04 10:18:25.000" {
		t.Fatalf("latest router expiry = %q", got)
	}
	if evidence.routerLifetimeExpiredCount != 1 || evidence.autoconfDetachCount != 1 || !evidence.ipv6RestoredAt.IsZero() {
		t.Fatalf("latest loss interval retained prior restored state: %+v", evidence)
	}
}

func TestCollectTailTransportMonitorRouteEvidenceCoversProvenDiagnosticLag(t *testing.T) {
	var commandName string
	var commandArgs []string
	source := &syntheticSource{localFn: func(name string, args ...string) (string, error) {
		commandName = name
		commandArgs = append([]string(nil), args...)
		return strings.Join([]string{
			"2026-09-01 06:59:40.537 Df configd[1:2] RTADV en0: router lifetime became zero",
			"2026-09-01 06:59:40.570 Df configd[1:2] network changed: v4(en0)",
		}, "\n"), nil
	}}
	env, err := newProbeEnv(syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	events := map[string]*tailTransportRouteAggregate{
		"2001:db8:2::44": {
			first: "2026/09/01 06:59:49",
			last:  "2026/09/01 06:59:50",
		},
	}
	evidence := collectTailTransportMonitorRouteEvidenceFromRunner(context.Background(), env, events)
	if !evidence.matches(events["2001:db8:2::44"]) {
		t.Fatalf("collector missed the proven pre-diagnostic expiry: %+v", evidence)
	}
	if commandName != "/usr/bin/log" {
		t.Fatalf("local command = %q", commandName)
	}
	joined := strings.Join(commandArgs, " ")
	for _, want := range []string{
		"--start 2026-09-01 06:49:49",
		"--end 2026-09-01 07:00:05",
		`process == "configd"`,
		`eventMessage CONTAINS "RTADV "`,
		`eventMessage CONTAINS "AUTOMATIC-V6 "`,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("bounded configd command missing %q: %s", want, joined)
		}
	}
}

// Grafana's Loki/Mimir query engine logs the complete query at info level.
// Searching for an error signature therefore echoes that signature into the
// grafana service log; the standing grafana tailer must not feed the monitor's
// own observation back into the error classifier.
func TestGrafanaQueryEchoCannotCreateLogAlerts(t *testing.T) {
	tailer := newLogTailer("grafana", nil)
	redisEcho := `[by-us-fmt-5-edge-4][grafana][g1][cid:test][2026-08-31T15:16:29Z]level=info ts=2026-08-31T15:16:29Z caller=roundtrip.go:412 org_id=fake msg=\"executing query\" type=range query=\"{env=\\\"main\\\"} |= \\\"[redis][ttl]\\\"\" query_hash=1`
	panicEcho := `[fireside][grafana][g1][cid:test][2026-08-31T15:16:30Z]level=info ts=2026-08-31T15:16:30Z caller=engine.go:274 component=querier org_id=fake msg=\"executing query\" query=\"{env=\\\"main\\\"} |= \\\"panic:\\\"\" query_hash=2`
	metricsEcho := `[by-us-fmt-5-edge-4][grafana][g1][cid:test][2026-08-31T15:23:53Z]level=info ts=2026-08-31T15:23:53Z caller=metrics.go:285 component=querier org_id=fake latency=fast query=\"{env=\\\"main\\\", service=\\\"api\\\"} |= \\\"[redis][ttl]\\\"\" query_hash=3 query_type=filter range_type=range duration=2ms status=200 returned_lines=0`
	tailer.classify(redisEcho)
	tailer.classify(metricsEcho)
	for range 5 {
		tailer.classify(panicEcho)
	}

	findings := tailer.drainWindow()
	for _, class := range []string{"redis-ttl-suspect", "panic", "novel"} {
		if finding := findingByClass(t, findings, class); !finding.healthy {
			t.Fatalf("grafana query echo became a %s finding: %+v", class, finding)
		}
	}

	// The exclusion is deliberately narrow: an actual Grafana warning and the
	// same text from a non-Grafana service must remain visible.
	grafanaWarning := newLogTailer("grafana", nil)
	grafanaWarning.classify(`[fireside][grafana][g1][cid:test]level=warn caller=redis.go:89 [redis][ttl] suspicious ttl`)
	if finding := findingByClass(t, grafanaWarning.drainWindow(), "redis-ttl-suspect"); finding.healthy {
		t.Fatal("real Grafana TTL warning was hidden with query metadata")
	}
	apiEcho := newLogTailer("api", nil)
	apiEcho.classify(redisEcho)
	if finding := findingByClass(t, apiEcho.drainWindow(), "redis-ttl-suspect"); finding.healthy {
		t.Fatal("query-echo exclusion leaked outside the grafana service")
	}
}

// The production H1 admission rejection is an info-level diagnostic and does
// not contain a generic error-shaped word. Without an explicit class, the
// standing monitor silently misses a carrier that can never fit on retry.
func TestFramerMessageTooLargeIsClassifiedAndRedacted(t *testing.T) {
	for _, line := range []string{
		`[edge-4][connect][g2][cid:customer-one][I][2026-09-01T07:14:00Z][framer][reject]write messageLen=4232 > MaxMessageLen=4096 (maxFrameLen=4100)`,
		`[fireside][proxy][g3][cid:customer-two][I][2026-09-01T07:14:01Z][framer][reject]read messageLen=4950 > MaxMessageLen=4096 (maxFrameLen=4100)`,
		`[fireside][proxy][g3][cid:customer-three][I][2026-09-01T07:14:02Z][framer][reject]write batch messageLen=4187 > MaxMessageLen=4096 (maxFrameLen=4100)`,
	} {
		tailer := newLogTailer("connect", nil)
		tailer.classify(line)
		finding := findingByClass(t, tailer.drainWindow(), "framer-message-too-large")
		if finding.healthy {
			t.Fatalf("info-level framer rejection was not classified: %q", line)
		}
		for _, want := range []string{
			"messageLen=",
			"MaxMessageLen=4096",
			"same immutable Pack",
			"Connect and Proxy artifacts",
			"c1403f16",
			"096414ac",
			"§8.13",
			"§8.12",
			"three sustained HTTP/SOCKS/WireGuard overlap campaigns",
		} {
			if !strings.Contains(finding.evidence+finding.mechanism+finding.action+finding.verify, want) {
				t.Fatalf("framer rejection finding lacks %q: %+v", want, finding)
			}
		}
		for _, stale := range []string{"53780b3e", "7e0fcba"} {
			if strings.Contains(finding.action+finding.verify, stale) {
				t.Fatalf("framer rejection finding retained former non-ancestor hash %q: %+v", stale, finding)
			}
		}
		for _, secret := range []string{"customer-one", "customer-two", "customer-three", "fireside", "edge-4"} {
			if strings.Contains(finding.evidence, secret) {
				t.Fatalf("framer rejection evidence retained identity %q: %q", secret, finding.evidence)
			}
		}
	}

	adjacent := newLogTailer("connect", nil)
	adjacent.classify(`[edge-4][connect][g2][I][framer]write messageLen=4232 MaxMessageLen=8192`)
	if finding := findingByClass(t, adjacent.drainWindow(), "framer-message-too-large"); !finding.healthy {
		t.Fatalf("ordinary framer log was classified as a rejection: %+v", finding)
	}
}

func TestMimirSeriesLimitStandingWindowHasStablePrivateFrameAndHealthyControl(t *testing.T) {
	tailer := newLogTailer("grafana", nil)
	if finding := findingByClass(t, tailer.drainWindow(), "mimir-series-limit"); !finding.healthy {
		t.Fatal("empty ingestion-rejection window was not healthy")
	}
	for _, line := range []string{
		`Stats push rejected (400): per-user series limit exceeded; address="192.0.2.1:9999" instance="private-first"`,
		`Stats push rejected (400): per-user series limit exceeded; address="192.0.2.2:9999" instance="private-second"`,
	} {
		tailer.classify(line)
	}
	finding := findingByClass(t, tailer.drainWindow(), "mimir-series-limit")
	if finding.healthy || finding.tier != tierPage || finding.sustain != 1 || finding.frame != "tenant-series-admission" {
		t.Fatalf("series admission contract: healthy=%t tier=%s sustain=%d frame=%s", finding.healthy, finding.tier, finding.sustain, finding.frame)
	}
	if !strings.Contains(finding.observed, "rate=2/min") || strings.Contains(finding.evidence, "private-") || strings.Contains(finding.evidence, "192.0.2.") {
		t.Fatal("series admission count or privacy contract failed")
	}
	if next := findingByClass(t, tailer.drainWindow(), "mimir-series-limit"); !next.healthy || next.target != finding.target {
		t.Fatal("quiet standing window could not resolve the same service identity")
	}
}

func TestMimirStructuredRejectionsRetainOnlyFixedBatchClasses(t *testing.T) {
	tests := []struct {
		name  string
		line  string
		class string
		frame string
	}{
		{
			name:  "series",
			line:  "Stats push rejected status=400 reason=series-limit job=proxy metric_families=3 time_series=41 family_classes=process:1,redis:2 family_classes_truncated=false",
			class: "mimir-series-limit",
			frame: "job=proxy",
		},
		{
			name:  "rate",
			line:  "Stats push rejected status=429 reason=rate-limit job=taskworker metric_families=2 time_series=19 family_classes=go:1,process:1 family_classes_truncated=false",
			class: "mimir-ingestion-rate-limit",
			frame: "job=taskworker",
		},
		{
			name:  "other",
			line:  "Stats push rejected status=503 reason=server job=other metric_families=1 time_series=1 family_classes=other:1 family_classes_truncated=false",
			class: "mimir-push-rejected",
			frame: "job=other",
		},
	}
	for _, test := range tests {
		tailer := newLogTailer("grafana", nil)
		tailer.classify(test.line)
		finding := findingByClass(t, tailer.drainWindow(), test.class)
		if finding.healthy || finding.frame != test.frame {
			t.Fatalf("%s: structured rejection was not classified: %+v", test.name, finding)
		}
		if finding.evidence == "" || !strings.Contains(finding.evidence, "metric_families=") ||
			!strings.Contains(finding.evidence, "family_classes=") {
			t.Fatalf("%s: bounded rejected-batch evidence missing: %+v", test.name, finding)
		}
	}
}

func TestMimirGenericRejectionMarkdownDoesNotExcludeUnprovenTypedLimits(t *testing.T) {
	tailer := newLogTailer("grafana", nil)
	tailer.classify("Stats push rejected status=400 reason=other-client job=api metric_families=1 time_series=1 family_classes=go:1 family_classes_truncated=false")
	finding := findingByClass(t, tailer.drainWindow(), "mimir-push-rejected")
	if finding.healthy || finding.tier != tierWarn {
		t.Fatalf("generic upstream rejection lost affirmative batch loss: %+v", finding)
	}
	markdown := alertFromFinding(syntheticSettings(nil), "1.5", "log-errors", "Log errors", finding).Markdown()
	for _, required := range []string{
		"unclassified upstream rejection", "without a proven typed limit", "not evidence that either limit was absent",
		"Unknown, unreadable, or oversized", "direct admission counters remain the loss authority",
	} {
		if !strings.Contains(markdown, required) {
			t.Errorf("generic rejection Markdown lacks conservative attribution %q", required)
		}
	}
	if strings.Contains(markdown, "non-series, non-rate") {
		t.Fatal("generic rejection Markdown affirmatively excluded an unproven limit")
	}
}

func TestMimirStructuredRejectionsFailClosedOnImpossibleOrMalformedTuples(t *testing.T) {
	const valid = "Stats push rejected status=400 reason=series-limit job=api metric_families=2 time_series=3 family_classes=go:1,process:1 family_classes_truncated=false"
	for _, test := range []struct {
		name string
		line string
	}{
		{name: "series on server status", line: strings.Replace(valid, "status=400", "status=503", 1)},
		{name: "rate on client status", line: strings.Replace(valid, "reason=series-limit", "reason=rate-limit", 1)},
		{name: "server on client status", line: strings.Replace(valid, "reason=series-limit", "reason=server", 1)},
		{name: "client on server status", line: strings.Replace(strings.Replace(valid, "status=400", "status=503", 1), "reason=series-limit", "reason=other-client", 1)},
		{name: "unknown status", line: strings.Replace(valid, "status=400", "status=999", 1)},
		{name: "zero families", line: strings.Replace(valid, "metric_families=2", "metric_families=0", 1)},
		{name: "zero series", line: strings.Replace(valid, "time_series=3", "time_series=0", 1)},
		{name: "overflow", line: strings.Replace(valid, "time_series=3", "time_series=9999999999999999999", 1)},
		{name: "body bound", line: strings.Replace(valid, "time_series=3", "time_series=8388609", 1)},
		{name: "missing summary", line: strings.Replace(valid, "go:1,process:1", "none", 1)},
		{name: "duplicate classes", line: strings.Replace(valid, "go:1,process:1", "go:1,go:1", 1)},
		{name: "unsorted classes", line: strings.Replace(valid, "go:1,process:1", "process:1,go:1", 1)},
		{name: "zero class count", line: strings.Replace(valid, "go:1,process:1", "go:0,process:2", 1)},
		{name: "summary mismatch", line: strings.Replace(valid, "go:1,process:1", "go:1,process:2", 1)},
		{name: "impossible truncation", line: strings.Replace(valid, "truncated=false", "truncated=true", 1)},
		{name: "raw job", line: strings.Replace(valid, "job=api", "job=private-fixture", 1)},
		{name: "raw family", line: strings.Replace(valid, "go:1,process:1", "private-fixture:2", 1)},
		{name: "unknown reason", line: strings.Replace(valid, "reason=series-limit", "reason=private-fixture", 1)},
		{name: "missing field", line: strings.Replace(valid, "time_series=3 ", "", 1)},
		{name: "raw suffix", line: valid + " private-fixture=192.0.2.9"},
		{name: "missing value", line: "Stats push rejected status="},
	} {
		if _, ok := parseMimirRejectedLog(test.line); ok {
			t.Fatalf("%s: impossible rejection event parsed", test.name)
		}
		tailer := newLogTailer("grafana", nil)
		tailer.classify("[private-fixture.example.test][grafana] " + test.line)
		findings := tailer.drainWindow()
		unknown := findingByClass(t, findings, "mimir-rejection-unobservable")
		if unknown.healthy || unknown.tier != tierWarn || unknown.frame != "rejection-schema" {
			t.Fatalf("%s: malformed schema lost fixed visibility: %+v", test.name, unknown)
		}
		for _, finding := range findings {
			if finding.class == "mimir-series-limit" || finding.class == "mimir-ingestion-rate-limit" || finding.class == "mimir-push-rejected" {
				t.Fatalf("%s: malformed schema attributed or resolved typed loss: %+v", test.name, finding)
			}
		}
		markdown := alertFromFinding(syntheticSettings(nil), "1.5", "log-errors", "Log errors", unknown).Markdown()
		for _, secret := range []string{"private-fixture", "192.0.2.9"} {
			if strings.Contains(markdown, secret) {
				t.Fatalf("%s: malformed rejection retained private payload", test.name)
			}
		}
		for _, required := range []string{"visibility loss", "withholds healthy", "artifact schemas", "ten minutes"} {
			if !strings.Contains(markdown, required) {
				t.Errorf("%s: visibility alert lacks %q", test.name, required)
			}
		}
	}
}

func TestMimirMalformedSchemaWithholdsRecoveryButRetainsConcreteLoss(t *testing.T) {
	tailer := newLogTailer("grafana", nil)
	tailer.classify("Stats push rejected status=400 reason=series-limit job=api metric_families=1 time_series=1 family_classes=process:1 family_classes_truncated=false")
	tailer.classify("Stats push rejected status=429 reason=private-fixture")
	findings := tailer.drainWindow()
	if finding := findingByClass(t, findings, "mimir-series-limit"); finding.healthy {
		t.Fatal("a malformed sibling suppressed affirmative series loss")
	}
	for _, finding := range findings {
		if finding.healthy && (finding.class == "mimir-ingestion-rate-limit" || finding.class == "mimir-push-rejected") {
			t.Fatal("unknown schema resolved an unobservable typed sibling")
		}
	}
	clean := tailer.drainWindow()
	for _, class := range []string{"mimir-series-limit", "mimir-ingestion-rate-limit", "mimir-push-rejected", "mimir-rejection-unobservable"} {
		if next := findingByClass(t, clean, class); !next.healthy {
			t.Fatalf("a subsequent complete clean window cannot resolve %s", class)
		}
	}
}

func TestMimirLegacyRejectionCannotInheritReasonFromEchoedCallerLabel(t *testing.T) {
	for _, line := range []string{
		`Stats push rejected (400): invalid sample series={note="per-user series limit", address="192.0.2.9", private="synthetic-private-value"}`,
		`Stats push rejected (503): per-user series limit unavailable private="synthetic-private-value"`,
	} {
		tailer := newLogTailer("grafana", nil)
		tailer.classify(line)
		findings := tailer.drainWindow()
		visibility := findingByClass(t, findings, "mimir-rejection-unobservable")
		if visibility.healthy || strings.Contains(visibility.evidence, "synthetic-private-value") || strings.Contains(visibility.evidence, "192.0.2.9") {
			t.Fatal("unsupported legacy rejection lost fixed visibility/privacy")
		}
		for _, finding := range findings {
			if finding.class == "mimir-series-limit" || finding.class == "mimir-ingestion-rate-limit" || finding.class == "mimir-push-rejected" {
				t.Fatal("echoed caller phrase attributed or resolved typed Mimir loss")
			}
		}
	}
	tailer := newLogTailer("grafana", nil)
	tailer.classify("Stats push rejected (400): failed pushing to ingester ingester.example.test: user=synthetic-tenant: per-user series limit of 7 exceeded (err-mimir-max-series-per-user)")
	if finding := findingByClass(t, tailer.drainWindow(), "mimir-series-limit"); finding.healthy || finding.frame != "tenant-series-admission" {
		t.Fatal("source-reviewed legacy series prefix no longer preserves rollout evidence")
	}
}

func TestMimirRejectionTailCancellationHasNoFabricatedEvidence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tailer := newLogTailer("grafana", nil)
	tailer.stream = func(ctx context.Context) (*exec.Cmd, io.ReadCloser, error) { return nil, nil, ctx.Err() }
	if err := tailer.tailOnce(ctx); err != context.Canceled {
		t.Fatalf("tail cancellation=%v", err)
	}
	for _, finding := range tailer.drainWindow() {
		if strings.HasPrefix(finding.class, "mimir-") && !finding.healthy {
			t.Fatalf("cancellation manufactured rejection evidence: %+v", finding)
		}
	}
}

func TestStandingTailAttributesStaleSourceTimeWithoutReplayingProductFailure(t *testing.T) {
	fixedNow := time.Date(2026, 9, 11, 22, 30, 0, 0, time.UTC)
	tailer := newLogTailer("grafana", nil)
	tailer.clock = func() time.Time { return fixedNow }

	stale := `[fixture.example][grafana][g1][cid:synthetic][2026-09-11T21:30:00Z] Stats push rejected (400): per-user series limit exceeded instance="sensitive-fixture"`
	tailer.ingestStanding(stale, true, true)
	tailer.ingestStanding(stale, true, true)
	tailer.ingestStanding(`[fixture.example][grafana][g1][cid:synthetic][2026-09-11T21:45:00Z] ordinary delayed fixture record`, true, true)
	findings := tailer.drainWindow()
	if product := findingByClass(t, findings, "mimir-series-limit"); !product.healthy {
		t.Fatalf("stale source record became a current product failure: %+v", product)
	}
	visibility := findingByClass(t, findings, "tailer-stale-arrival")
	if visibility.healthy || visibility.sustain != 1 {
		t.Fatalf("stale source record was not attributed to observation visibility: %+v", visibility)
	}
	for _, want := range []string{"stale_arrivals=2", "oldest_source_age=1h0m0s"} {
		if !strings.Contains(visibility.observed, want) {
			t.Fatalf("stale observation lacks %q: %+v", want, visibility)
		}
	}
	rendered := visibility.symptom + visibility.baseline + visibility.observed +
		visibility.mechanism + visibility.evidence + visibility.context +
		visibility.action + visibility.verify
	if strings.Contains(rendered, "sensitive-fixture") {
		t.Fatalf("stale observation retained source contents: %+v", visibility)
	}
	if novel := findingByClass(t, findings, "novel"); !novel.healthy {
		t.Fatalf("stale ordinary record became a novel product failure: %+v", novel)
	}

	current := `[fixture.example][grafana][g1][cid:synthetic][2026-09-11T22:28:30Z] Stats push rejected (400): per-user series limit exceeded instance="sensitive-fixture"`
	tailer.ingestStanding(current, true, true)
	findings = tailer.drainWindow()
	if product := findingByClass(t, findings, "mimir-series-limit"); product.healthy {
		t.Fatalf("current source record was discarded: %+v", product)
	}
	if next := findingByClass(t, findings, "tailer-stale-arrival"); !next.healthy {
		t.Fatalf("quiet stale-arrival window did not resolve: %+v", next)
	}
}

func TestStandingTailStaleGateRequiresAnAuthoritativePastTimestamp(t *testing.T) {
	fixedNow := time.Date(2026, 9, 11, 22, 30, 0, 0, time.UTC)
	for _, line := range []string{
		`[fixture.example][grafana][g1][cid:synthetic][timestamp-unavailable] Stats push rejected (400): per-user series limit exceeded`,
		`[fixture.example][grafana][g1][cid:synthetic][2026-09-11T22:31:00Z] Stats push rejected (400): per-user series limit exceeded`,
	} {
		tailer := newLogTailer("grafana", nil)
		tailer.clock = func() time.Time { return fixedNow }
		tailer.ingestStanding(line, true, true)
		findings := tailer.drainWindow()
		if product := findingByClass(t, findings, "mimir-series-limit"); product.healthy {
			t.Fatalf("non-stale source record was hidden: %q", line)
		}
		if visibility := findingByClass(t, findings, "tailer-stale-arrival"); !visibility.healthy {
			t.Fatalf("unproven source staleness raised stale-arrival: %+v", visibility)
		}
	}
}

func TestStandingTailClassifiesWarpctlPreCursorReductionWithoutRawHistory(t *testing.T) {
	tailer := newLogTailer("fixture-service", nil)
	tailer.ingestStanding(
		`[warpctl][loki-tail-pre-cursor-entries] service=fixture-service count=17`,
		true,
		true,
	)

	finding := findingByClass(t, tailer.drainWindow(), "loki-tail-pre-cursor-entries")
	if finding.healthy || finding.sustain != 1 {
		t.Fatalf("pre-cursor reduction was not an immediate visibility warning: %+v", finding)
	}
	for _, want := range []string{"fixture-service", "count=17", "monotonic live-tail cursor"} {
		rendered := finding.symptom + finding.observed + finding.evidence + finding.mechanism
		if !strings.Contains(rendered, want) {
			t.Fatalf("pre-cursor finding lacks %q: %+v", want, finding)
		}
	}
	rendered := finding.symptom + finding.baseline + finding.observed +
		finding.mechanism + finding.evidence + finding.context +
		finding.action + finding.verify
	if strings.Contains(rendered, "private-stale-fixture") {
		t.Fatalf("pre-cursor finding retained suppressed contents: %s", rendered)
	}
}

func TestStandingTailRejectsMalformedPreCursorReduction(t *testing.T) {
	for _, line := range []string{
		`[warpctl][loki-tail-pre-cursor-entries] service=fixture-service count=0`,
		`[warpctl][loki-tail-pre-cursor-entries] service=fixture-service count=1 private-stale-fixture`,
	} {
		tailer := newLogTailer("fixture-service", nil)
		tailer.ingestStanding(line, true, true)
		finding := findingByClass(t, tailer.drainWindow(), "loki-tail-pre-cursor-entries")
		if !finding.healthy {
			t.Fatalf("malformed pre-cursor reduction was trusted: %q %+v", line, finding)
		}
	}
}

func TestStandingTailPreCursorReductionIsDocumented(t *testing.T) {
	catalogBytes, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	catalog := string(catalogBytes)
	for _, required := range []string{
		"[warpctl][loki-tail-pre-cursor-entries] service=<service> count=<n>",
		"observation-path evidence rather than a current product failure",
		"Distinct records at the cursor timestamp",
		"remain visible",
		"never replay suppressed contents",
		"resolved Warpctl executable",
		"d857872c4cae8e4768ed2314fdb53fc96b4fdbdb",
		"false-positive qualifier",
		"false-negative qualifier",
	} {
		if !strings.Contains(catalog, required) {
			t.Errorf("pre-cursor catalog guidance omits %q", required)
		}
	}
	if !strings.Contains(catalog, "| loki-tail-pre-cursor-entries | logs |") {
		t.Error("SIGNALS.md alert-emission table omits loki-tail-pre-cursor-entries")
	}
}

func TestMimirBucketIndexLagSeparatesNormalPhaseSkew(t *testing.T) {
	const normal = `[by-us-fmt-5-edge-1][grafana][g1][cid:normal][2026-08-31T22:42:00Z]level=warn ts=2026-08-31T22:42:00Z caller=bucket.go:1248 user=anonymous level=warn ours=2026-08-31T22:12:17Z requested=2026-08-31T22:26:50Z diff=-873 msg="bucket index version (updated_at) is older than requested"`
	const belowThreshold = `[by-us-fmt-5-edge-1][grafana][g1][cid:below][2026-08-31T22:42:01Z]level=warn ts=2026-08-31T22:42:01Z caller=bucket.go:1248 user=anonymous level=warn ours=2026-08-31T21:56:51Z requested=2026-08-31T22:26:50Z diff=-1799 msg="bucket index version (updated_at) is older than requested"`
	tailer := newLogTailer("grafana", nil)
	tailer.classify(normal)
	tailer.classify(belowThreshold)
	if finding := findingByClass(t, tailer.drainWindow(), "mimir-bucket-index-lag"); !finding.healthy {
		t.Fatalf("sub-threshold Mimir phase skew alerted: %+v", finding)
	}

	const stale = `[by-us-fmt-5-edge-3][grafana][g4][cid:stale][2026-08-31T22:43:00Z]level=warn ts=2026-08-31T22:43:00Z caller=bucket.go:1248 user=anonymous level=warn ours=2026-08-31T21:56:50Z requested=2026-08-31T22:26:50Z diff=-1800 msg="bucket index version (updated_at) is older than requested"`
	tailer.classify(stale)
	finding := findingByClass(t, tailer.drainWindow(), "mimir-bucket-index-lag")
	if finding.healthy {
		t.Fatal("multi-generation Mimir bucket-index lag did not alert")
	}
	for _, want := range []string{
		"host=by-us-fmt-5-edge-3 generation=g4",
		"ours=2026-08-31T21:56:50Z",
		"requested=2026-08-31T22:26:50Z",
		"diff=-1800",
		"production control's exact -873-second gap",
		"Warp 13fcd05 sets the single-tenant fleet's store-gateway discovery interval to one minute",
		"not by itself a failed query",
		"Verify the running Grafana artifact contains Warp 13fcd05",
		"last successful sync remains under two minutes old",
		"Do not suppress every bucket warning",
		"no >=1,800-second warning",
	} {
		alert := alertFromFinding(
			SignalSettings{Environment: "synthetic", Now: time.Now},
			"1.5", "log-errors", "Log error-class rates", finding,
		)
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("Mimir lag alert lacks %q: %+v", want, finding)
		}
	}
}

func TestNetEscrowAlertRetainsSiteAndRedactsEntityIDs(t *testing.T) {
	tailer := newLogTailer("taskworker", nil)
	tailer.classify("[netescrow]negative counter after settle: balance=01a04ff7-83b0-1970-2353-4b9ccf6e461d contract=01a05086-db24-dde0-dd4b-cbd20ace42ca result=-21434368")
	finding := findingByClass(t, tailer.drainWindow(), "netescrow-negative")
	if finding.healthy {
		t.Fatal("one negative mirror must alert")
	}
	if !strings.Contains(finding.evidence, "after settle") || !strings.Contains(finding.evidence, "balance=<id> contract=<id>") {
		t.Fatalf("redacted evidence lost its useful site: %q", finding.evidence)
	}
	if logIDRe.MatchString(finding.evidence) {
		t.Fatalf("net-escrow alert leaked an entity id: %q", finding.evidence)
	}
	if !strings.Contains(finding.evidence, "clamp_marker=absent") {
		t.Fatalf("net-escrow alert did not distinguish an absent clamp marker from truncation: %q", finding.evidence)
	}
	if finding.frame != "site=settle" || !strings.Contains(finding.observed, "frame=site=settle") {
		t.Fatalf("net-escrow alert lost its structured mutation site: frame=%q observed=%q", finding.frame, finding.observed)
	}
}

// The production line's clamp marker follows enough metadata and identifiers
// to fall outside the generic sample limit. Preserve it because it separates
// an atomically contained current-binary aftermath from legacy behavior.
func TestNetEscrowAlertPreservesClampMarkerBeyondSampleLimit(t *testing.T) {
	tailer := newLogTailer("taskworker", nil)
	line := "[by-us-fmt-5-edge-4][taskworker][g1][cid:16a73fdaca8f][E][2026-08-31T11:30:52.458075-05:00][subscription_model.go:748][netescrow]negative counter after settle: balance=01a04ff7-83b0-1970-2353-4b9ccf6e461d contract=01a05086-db24-dde0-dd4b-cbd20ace42ca result=-10066 clamped_to=0"
	if strings.Index(line, "clamped_to=0") <= 200 {
		t.Fatal("fixture no longer places the clamp marker beyond the generic sample limit")
	}
	tailer.classify(line)

	finding := findingByClass(t, tailer.drainWindow(), "netescrow-negative")
	if !strings.Contains(finding.evidence, "clamped_to=0") {
		t.Fatalf("net-escrow evidence lost the clamp marker: %q", finding.evidence)
	}
	if strings.Contains(finding.evidence, "clamp_marker=absent") {
		t.Fatalf("present clamp marker was classified absent: %q", finding.evidence)
	}
	for _, want := range []string{
		"absent when there are no new reservations",
		"exactly equal the current PostgreSQL open-reservation sum",
		"key presence alone does not disprove the atomic clamp",
	} {
		if !strings.Contains(finding.verify, want) {
			t.Fatalf("net-escrow verification cannot distinguish legitimate key recreation; missing %q: %q", want, finding.verify)
		}
	}
	if logIDRe.MatchString(finding.evidence) {
		t.Fatalf("net-escrow clamp evidence leaked an entity id: %q", finding.evidence)
	}
}

func TestNetEscrowAlertSeparatesMutationSites(t *testing.T) {
	tailer := newLogTailer("api", nil)
	tailer.classify("[netescrow]negative counter after settle: balance=01a04ff7-83b0-1970-2353-4b9ccf6e461d contract=01a05086-db24-dde0-dd4b-cbd20ace42ca result=-1")
	tailer.classify("[netescrow]negative counter after quarantine release: balance=01a04ff7-83b0-1970-2353-4b9ccf6e461d contract=01a05086-db24-dde0-dd4b-cbd20ace42ca result=-2")

	findings := tailer.drainWindow()
	frames := map[string]bool{}
	for _, finding := range findings {
		if finding.class == "netescrow-negative" && !finding.healthy {
			frames[finding.frame] = true
			if logIDRe.MatchString(finding.evidence) {
				t.Fatalf("net-escrow alert leaked an entity id: %q", finding.evidence)
			}
		}
	}
	for _, want := range []string{"site=settle", "site=quarantine release"} {
		if !frames[want] {
			t.Fatalf("net-escrow findings frames = %v, missing %q", frames, want)
		}
	}
}

func TestNetEscrowNegativeStormPages(t *testing.T) {
	tailer := newLogTailer("taskworker", nil)
	for range netEscrowNegativePageRate {
		tailer.classify("[netescrow]negative counter after settle: balance=01a04ff7-83b0-1970-2353-4b9ccf6e461d contract=01a05086-db24-dde0-dd4b-cbd20ace42ca result=-10760936105")
	}

	finding := findingByClass(t, tailer.drainWindow(), "netescrow-negative")
	if finding.tier != tierPage {
		t.Fatalf("storm tier = %q, want page: %+v", finding.tier, finding)
	}
	for _, want := range []string{
		"rate=100/min",
		"page_threshold=100/min",
		"frame=site=settle",
	} {
		if !strings.Contains(finding.observed, want) {
			t.Fatalf("storm observation missing %q: %s", want, finding.observed)
		}
	}
	if logIDRe.MatchString(finding.evidence) {
		t.Fatalf("storm evidence leaked an entity id: %q", finding.evidence)
	}
}

func TestRedisNetEscrowTTLAlertRedactsEntityIDs(t *testing.T) {
	tailer := newLogTailer("api", nil)
	tailer.classify(`[redis][ttl]"expireat" key="{escrow_019c640e-f467-4fa7-177f-d7ca43c33b6f}net" ttl 3139421360s-from-now exceeds 9600h0m0s`)
	finding := findingByClass(t, tailer.drainWindow(), "redis-netescrow-ttl")
	if finding.healthy {
		t.Fatal("one suspect Redis TTL must alert")
	}
	if !strings.Contains(finding.evidence, `"expireat"`) ||
		!strings.Contains(finding.evidence, `{escrow_<id>}net`) {
		t.Fatalf("redacted evidence lost command or key family: %q", finding.evidence)
	}
	if logIDRe.MatchString(finding.evidence) {
		t.Fatalf("Redis TTL alert leaked an entity id: %q", finding.evidence)
	}
}

func payoutAttemptLogLines(second string, attempt int) (string, string) {
	id := fmt.Sprintf("019f77ae-de17-db98-b22d-%012x", attempt)
	processorLine := fmt.Sprintf(
		`[edge-3][taskworker][g2][cid:test][I][%s.100000Z][circle_client_controller.go:142][circlec]error sending payment: wallet %s: asset amount owned by the wallet is insufficient`,
		second,
		id,
	)
	evaluatorLine := fmt.Sprintf(
		`[edge-3][taskworker][g2][cid:test][I][%s.200000Z][task.go:1930][%s]eval error = asset amount owned by the wallet is insufficient`,
		second,
		id,
	)
	return processorLine, evaluatorLine
}

func payoutAdmissionLogLine(second string, admission int) string {
	admittedAt, err := time.Parse(time.RFC3339, second+"Z")
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf(
		`[worker-a.invalid][taskworker][generation-a][cid:synthetic][I][%s.%06dZ][circle_transfer_limiter.go:317][circlec][transfer-admission] admitted observable=v1 redis_second=%d sequence=%d deferrals=0 wait_ms=0`,
		second,
		admission+1,
		admittedAt.Unix(),
		admission+1,
	)
}

// Response and evaluator timestamps occur after POST and can coalesce even
// when Redis admitted each request in a different rolling second. They retain
// the separate liquidity finding and 429 correlation state, but must never
// drive the admission-ceiling finding.
func TestPayoutRetryMicroburstIgnoresIndependentResponseCompletions(t *testing.T) {
	tailer := newLogTailer("taskworker", nil)
	for attempt := 0; attempt < 4; attempt++ {
		processorLine, evaluatorLine := payoutAttemptLogLines("2026-08-31T15:46:23", attempt)
		tailer.classify(processorLine)
		tailer.classify(evaluatorLine)
	}
	processorLine, evaluatorLine := payoutAttemptLogLines("2026-08-31T15:46:23", 0)
	tailer.classify(processorLine)
	tailer.classify(evaluatorLine)

	findings := tailer.drainWindow()
	if liquidity := findingByClass(t, findings, "payout-wallet-insufficient"); liquidity.healthy {
		t.Fatal("wallet response controls lost the liquidity finding")
	}
	if burst := findingByClass(t, findings, "payout-retry-microburst"); !burst.healthy {
		t.Fatalf("response completions manufactured an admission burst: %+v", burst)
	}
}

// Four exact admission markers in one normalized source second cannot fit
// under the three-per-rolling-second gate. A replayed marker must not
// manufacture a fifth admission.
func TestPayoutRetryMicroburstCountsPrePostAdmissionsPerSecond(t *testing.T) {
	tailer := newLogTailer("taskworker", nil)
	lines := make([]string, 0, 4)
	for admission := 0; admission < 4; admission++ {
		line := payoutAdmissionLogLine("2026-09-12T19:10:23", admission)
		lines = append(lines, line)
		tailer.classify(line)
	}
	tailer.classify(lines[0])

	finding := findingByClass(t, tailer.drainWindow(), "payout-retry-microburst")
	if finding.healthy {
		t.Fatal("four same-second admissions did not create a microburst finding")
	}
	for _, want := range []string{
		"peak_admitted_submissions_per_second=4",
		"threshold=4/s",
		"admitted_submissions=4",
		"diagnostic_lines=5",
		"exact-replay-deduplicated pre-POST admission markers",
		"authoritative Redis TIME second from the atomic admission decision and is emitted before the processor POST",
		"cannot fit under a three-admission rolling-second ceiling",
		"mixed rollout or telemetry loss",
		"absent markers are unknown rather than zero",
		"Payout-wallet-insufficient remains a separate finance/operations condition",
		"complete §2.14 admission-observable coverage",
		"An uninstrumented Circle caller is instead a 429-source investigation",
		"peak_admitted_submissions_per_second stays below 4",
		"[circlec][transfer-admission] admitted observable=v1 redis_second=1789240223 sequence=4 deferrals=0 wait_ms=0",
	} {
		if combined := finding.observed + "\n" + finding.evidence + "\n" + finding.mechanism + "\n" + finding.context + "\n" + finding.action + "\n" + finding.verify; !strings.Contains(combined, want) {
			t.Fatalf("microburst finding missing %q: %+v", want, finding)
		}
	}
	for _, forbidden := range []string{"worker-a.invalid", "cid:synthetic"} {
		if strings.Contains(finding.evidence, forbidden) {
			t.Fatalf("microburst evidence leaked %q: %q", forbidden, finding.evidence)
		}
	}
	if logIDRe.MatchString(finding.evidence) {
		t.Fatalf("microburst evidence retained an identifier: %q", finding.evidence)
	}
}

// A minute can begin with a sparse attempt and peak later. The alert must
// retain a representative line from the actual peak second, not the first
// canonical line seen in the window (the live 16:31 window peaked at five at
// 16:31:47 but previously rendered a 16:31:33 sample).
func TestPayoutRetryMicroburstSampleComesFromPeakSecond(t *testing.T) {
	tailer := newLogTailer("taskworker", nil)
	first := payoutAdmissionLogLine("2026-09-12T19:11:33", 1)
	tailer.classify(first)
	for admission := 10; admission < 15; admission++ {
		peak := payoutAdmissionLogLine("2026-09-12T19:11:47", admission)
		tailer.classify(peak)
	}

	finding := findingByClass(t, tailer.drainWindow(), "payout-retry-microburst")
	for _, want := range []string{
		"peak_admitted_submissions_per_second=5",
		"peak_source_second=2026-09-12T19:11:47Z",
		"peak source second: 2026-09-12T19:11:47Z",
		"sample from peak second: [circlec][transfer-admission] admitted observable=v1 redis_second=1789240307 sequence=15",
	} {
		if combined := finding.observed + "\n" + finding.evidence; !strings.Contains(combined, want) {
			t.Fatalf("peak finding missing %q: %+v", want, finding)
		}
	}
	if strings.Contains(finding.evidence, "2026-09-12T19:11:33") {
		t.Fatalf("peak evidence retained the first sparse second: %q", finding.evidence)
	}
}

// Host clocks, logger scheduling, and delivery order cannot own the invariant.
// Four markers with deliberately skewed, out-of-order envelopes must group on
// the authoritative Redis TIME second returned by the atomic admission script.
func TestPayoutRetryMicroburstUsesRedisSecondDespiteSkewedOutOfOrderHostTimestamps(t *testing.T) {
	tailer := newLogTailer("taskworker", nil)
	redisSecond := time.Date(2026, 9, 12, 19, 12, 16, 0, time.UTC).Unix()
	timestamps := []string{
		"2031-01-02T03:04:59.000001Z",
		"2024-02-03T04:05:01.000002Z",
		"2029-04-05T01:06:42.000003-05:00",
		"2025-06-07T08:09:03.000004Z",
	}
	for admission, timestamp := range timestamps {
		line := fmt.Sprintf(
			`[worker-b.invalid][taskworker][generation-b][cid:synthetic][I][%s][circle_transfer_limiter.go:317][circlec][transfer-admission] admitted observable=v1 redis_second=%d sequence=%d deferrals=0 wait_ms=0`,
			timestamp,
			redisSecond,
			admission+1,
		)
		tailer.classify(line)
	}

	finding := findingByClass(t, tailer.drainWindow(), "payout-retry-microburst")
	combined := finding.observed + "\n" + finding.evidence + "\n" + finding.mechanism
	for _, want := range []string{
		"peak_source_second=2026-09-12T19:12:16Z",
		"peak source second: 2026-09-12T19:12:16Z",
		"authoritative Redis TIME second",
		"rather than an inference from host clocks",
		"sample from peak second: [circlec][transfer-admission] admitted observable=v1",
	} {
		if !strings.Contains(combined, want) {
			t.Fatalf("UTC-normalized peak finding missing %q: %+v", want, finding)
		}
	}
	for _, timestamp := range timestamps {
		if strings.Contains(combined, timestamp) {
			t.Fatalf("peak finding retained skewed host time %q: %+v", timestamp, finding)
		}
	}
}

func TestPaymentProcessorRateLimitCountsOneLogicalEventPerDiagnosticPair(t *testing.T) {
	tailer := newLogTailer("taskworker", nil)
	for attempt := 100; attempt < 105; attempt++ {
		_, evaluatorLine := payoutAttemptLogLines("2026-08-31T16:31:47", attempt)
		tailer.classify(evaluatorLine)
	}
	id := "019f77ae-de17-db98-b22d-2642f6f67594"
	providerLine := "[edge-1][taskworker][g2][cid:test][I][2026-08-31T16:31:47.578203Z][circle_client_controller.go:142][circlec]error sending payment: Bad status: 429 Too Many Requests {\"code\":5,\"message\":\"API rate limit error\",\"payment_id\":\"" + id + "\"}"
	evaluatorLine := "[edge-1][taskworker][g2][cid:test][I][2026-08-31T16:31:47.578638Z][task.go:1930][" + id + "]eval error = Bad status: 429 Too Many Requests {\"code\":5,\"message\":\"API rate limit error\"}"
	tailer.classify(providerLine)
	tailer.classify(evaluatorLine)
	tailer.classify(evaluatorLine)

	finding := findingByClass(t, tailer.drainWindow(), "payment-processor-rate-limit")
	for _, want := range []string{
		"rate=3/min",
		"processor_rate_limit_events=1",
		"diagnostic_lines=3",
		"canonical_source=exact-replay-deduplicated-task-evaluator",
		"logical event count: 1 exact-replay-deduplicated task evaluator line(s) from 3 diagnostic line(s)",
		"correlated_source_seconds=1",
		"correlated_cohort_seconds=1",
		"coincident_wallet_attempts=5",
		"peak_coincident_wallet_attempts_per_second=5",
		"correlation_threshold=4/s",
		"1/1 payment-processor-rate-limit source second(s) shared at least 4 canonical payout-wallet-insufficient attempt(s)",
		"5 attempt(s) shared those seconds, peaking at 5/s",
	} {
		if combined := finding.observed + "\n" + finding.evidence; !strings.Contains(combined, want) {
			t.Fatalf("processor rate-limit finding missing %q: %+v", want, finding)
		}
	}
	if logIDRe.MatchString(finding.evidence) {
		t.Fatalf("processor rate-limit evidence leaked an entity id: %q", finding.evidence)
	}

	// A reconnect can replay the final evaluator line after the cadence drain.
	// Preserve its diagnostic visibility but do not manufacture another logical
	// provider event in the next window.
	tailer.classify(evaluatorLine)
	replay := findingByClass(t, tailer.drainWindow(), "payment-processor-rate-limit")
	if !strings.Contains(replay.observed, "processor_rate_limit_events=0") ||
		!strings.Contains(replay.observed, "diagnostic_lines=1") ||
		!strings.Contains(replay.observed, "correlated_source_seconds=0") ||
		!strings.Contains(replay.evidence, "diagnostic replay is not a new provider event") {
		t.Fatalf("cross-window replay manufactured a logical event: %+v", replay)
	}
}

// A late evaluator line can arrive after the minute containing its triggering
// wallet cohort was drained. Retain source-second attempt counts only for the
// bounded reconciliation horizon, join an exact second across that boundary,
// and never join the adjacent second.
func TestPaymentProcessorRateLimitCorrelatesAcrossDrainByExactSourceSecond(t *testing.T) {
	now := time.Date(2026, 8, 31, 16, 32, 0, 0, time.UTC)
	tailer := newLogTailer("taskworker", nil)
	tailer.clock = func() time.Time { return now }
	for attempt := 200; attempt < 205; attempt++ {
		_, evaluatorLine := payoutAttemptLogLines("2026-08-31T16:31:47", attempt)
		tailer.classify(evaluatorLine)
	}
	_ = tailer.drainWindow()

	now = now.Add(time.Minute)
	rateLine := `[edge-1][taskworker][g2][cid:test][I][2026-08-31T16:31:47.900000Z][task.go:1930][019f77ae-de17-db98-b22d-aaaaaaaaaaaa]eval error = Bad status: 429 Too Many Requests {"code":5,"message":"API rate limit error"}`
	tailer.classify(rateLine)
	correlated := findingByClass(t, tailer.drainWindow(), "payment-processor-rate-limit")
	for _, want := range []string{
		"correlated_source_seconds=1",
		"correlated_cohort_seconds=1",
		"coincident_wallet_attempts=5",
		"peak_coincident_wallet_attempts_per_second=5",
	} {
		if combined := correlated.observed + "\n" + correlated.evidence; !strings.Contains(combined, want) {
			t.Fatalf("cross-drain correlation missing %q: %+v", want, correlated)
		}
	}

	adjacentLine := `[edge-1][taskworker][g2][cid:test][I][2026-08-31T16:31:48.100000Z][task.go:1930][019f77ae-de17-db98-b22d-bbbbbbbbbbbb]eval error = Bad status: 429 Too Many Requests {"code":5,"message":"API rate limit error"}`
	tailer.classify(adjacentLine)
	adjacent := findingByClass(t, tailer.drainWindow(), "payment-processor-rate-limit")
	if !strings.Contains(adjacent.observed, "correlated_source_seconds=1") ||
		!strings.Contains(adjacent.observed, "correlated_cohort_seconds=0") ||
		!strings.Contains(adjacent.observed, "coincident_wallet_attempts=0") {
		t.Fatalf("adjacent source second falsely inherited the wallet cohort: %+v", adjacent)
	}

	now = now.Add(logReconcileRetention + time.Second)
	_ = tailer.drainWindow()
	if len(tailer.burstRecentSecondCounts) != 0 || len(tailer.burstRecentSecondSeen) != 0 {
		t.Fatalf("expired correlation state was not pruned: counts=%v seen=%v", tailer.burstRecentSecondCounts, tailer.burstRecentSecondSeen)
	}
}

func TestStandingReconciliationUsesBoundedTwoMinuteOverlap(t *testing.T) {
	fixedNow := time.Date(2026, 8, 31, 18, 54, 0, 0, time.UTC)
	var got string
	source := &syntheticSource{localFn: func(name string, args ...string) (string, error) {
		got = name + " " + strings.Join(args, " ")
		return "", nil
	}}
	env := &probeEnv{
		cfg:    &monitorConfig{env: "main"},
		runner: &sourceRunner{source: source},
		now:    func() time.Time { return fixedNow },
	}
	tailer := newLogTailer("taskworker", env)
	tailer.reconcileOnce(context.Background())
	want := "warpctl logs main taskworker --since=2026-08-31T18:52:00Z --limit=20000"
	if got != want {
		t.Fatalf("reconciliation command = %q, want %q", got, want)
	}
}

// A connected Loki tail can miss a record ingested behind its source-time
// cursor. The bounded overlap must add only the absent records, including the
// canonical 429 and the two missing members of a same-second admission burst;
// replaying the same overlap on the next cadence must add nothing.
func TestStandingReconciliationRecoversLateRecordsWithoutReplay(t *testing.T) {
	fixedNow := time.Date(2026, 8, 31, 18, 54, 0, 0, time.UTC)
	tailer := newLogTailer("taskworker", nil)
	tailer.clock = func() time.Time { return fixedNow }
	tailer.startedAt = fixedNow.Add(-time.Minute)
	tailer.lastLineTime = tailer.startedAt

	walletLines := make([]string, 0, 4)
	admissionLines := make([]string, 0, 4)
	for attempt := 0; attempt < 4; attempt++ {
		_, evaluatorLine := payoutAttemptLogLines("2026-08-31T18:53:30", attempt)
		walletLines = append(walletLines, evaluatorLine)
		admissionLines = append(admissionLines, payoutAdmissionLogLine("2026-08-31T18:53:30", attempt))
	}
	rateLimitLine := `[edge-0][taskworker][g1][cid:test][I][2026-08-31T18:53:31.100000Z][task.go:1930][019f77ae-de17-db98-b22d-2642f6f67594]eval error = Bad status: 429 Too Many Requests {"code":5,"message":"API rate limit error"}`

	// The stable stream delivered newer traffic but omitted admissions one and
	// three plus all response evidence and the 429.
	tailer.ingestStanding(admissionLines[0], true, true)
	tailer.ingestStanding(admissionLines[2], true, true)
	tailer.ingestStanding(walletLines[0], true, true)
	tailer.ingestStanding(walletLines[2], true, true)
	reconciledLines := append([]string{}, admissionLines...)
	reconciledLines = append(reconciledLines, walletLines...)
	reconciledLines = append(reconciledLines, rateLimitLine)
	reconciled := strings.Join(reconciledLines, "\n")
	tailer.reconcile = func(context.Context, time.Time, []string) (string, error) { return reconciled, nil }
	tailer.reconcileOnce(context.Background())

	findings := tailer.drainWindow()
	burst := findingByClass(t, findings, "payout-retry-microburst")
	for _, want := range []string{
		"peak_admitted_submissions_per_second=4",
		"admitted_submissions=4",
		"diagnostic_lines=4",
	} {
		if !strings.Contains(burst.observed, want) {
			t.Fatalf("reconciled burst missing %q: %+v", want, burst)
		}
	}
	rateLimit := findingByClass(t, findings, "payment-processor-rate-limit")
	for _, want := range []string{
		"rate=1/min",
		"processor_rate_limit_events=1",
		"diagnostic_lines=1",
	} {
		if !strings.Contains(rateLimit.observed, want) {
			t.Fatalf("reconciled rate limit missing %q: %+v", want, rateLimit)
		}
	}

	// The next overlapping query necessarily contains the same records.
	tailer.reconcileOnce(context.Background())
	replayed := tailer.drainWindow()
	if finding := findingByClass(t, replayed, "payout-retry-microburst"); !finding.healthy {
		t.Fatalf("overlap replay manufactured a second burst: %+v", finding)
	}
	if finding := findingByClass(t, replayed, "payment-processor-rate-limit"); !finding.healthy {
		t.Fatalf("overlap replay manufactured a second 429: %+v", finding)
	}
}

func TestFirstStandingReconciliationDoesNotCountPreStartHistory(t *testing.T) {
	fixedNow := time.Date(2026, 8, 31, 18, 54, 0, 0, time.UTC)
	tailer := newLogTailer("taskworker", nil)
	tailer.clock = func() time.Time { return fixedNow }
	tailer.startedAt = fixedNow.Add(-time.Minute)
	tailer.lastLineTime = tailer.startedAt
	oldLine := `[edge-0][taskworker][g1][cid:old][I][2026-08-31T18:52:30.100000Z][task.go:1930][019f77ae-de17-db98-b22d-111111111111]eval error = Bad status: 429 Too Many Requests {"code":5,"message":"API rate limit error"}`
	currentLine := `[edge-0][taskworker][g1][cid:new][I][2026-08-31T18:53:30.100000Z][task.go:1930][019f77ae-de17-db98-b22d-222222222222]eval error = Bad status: 429 Too Many Requests {"code":5,"message":"API rate limit error"}`
	tailer.reconcile = func(context.Context, time.Time, []string) (string, error) {
		return oldLine + "\n" + currentLine, nil
	}
	tailer.reconcileOnce(context.Background())

	finding := findingByClass(t, tailer.drainWindow(), "payment-processor-rate-limit")
	if !strings.Contains(finding.observed, "rate=1/min") ||
		!strings.Contains(finding.observed, "processor_rate_limit_events=1") {
		t.Fatalf("first reconciliation counted pre-start history: %+v", finding)
	}

	// Remembering the old line is also important: the next overlap must not
	// introduce it after the startup boundary has passed.
	tailer.reconcileOnce(context.Background())
	if replay := findingByClass(t, tailer.drainWindow(), "payment-processor-rate-limit"); !replay.healthy {
		t.Fatalf("pre-start history entered a later window: %+v", replay)
	}
}

func TestStandingReconciliationFailurePreservesStreamAndRaisesVisibility(t *testing.T) {
	fixedNow := time.Date(2026, 8, 31, 18, 54, 0, 0, time.UTC)
	tailer := newLogTailer("taskworker", nil)
	tailer.clock = func() time.Time { return fixedNow }
	tailer.startedAt = fixedNow.Add(time.Minute * -2)
	tailer.lastLineTime = tailer.startedAt
	streamLine := `[edge-0][taskworker][g1][cid:live][I][2026-08-31T18:53:31.100000Z][task.go:1930][019f77ae-de17-db98-b22d-333333333333]eval error = Bad status: 429 Too Many Requests {"code":5,"message":"API rate limit error"}`
	tailer.ingestStanding(streamLine, true, true)
	tailer.reconcile = func(context.Context, time.Time, []string) (string, error) {
		return "", fmt.Errorf("Loki query error (502): Bad Gateway")
	}
	tailer.reconcileOnce(context.Background())

	probe := &logTailProbe{tailers: []*logTailer{tailer}}
	findings, err := probe.check(context.Background(), &probeEnv{now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatal(err)
	}
	if live := findingByClass(t, findings, "payment-processor-rate-limit"); live.healthy {
		t.Fatalf("failed reconciliation discarded the live-stream finding: %+v", live)
	}
	visibility := findingByClass(t, findings, "tailer-reconcile")
	if visibility.healthy || !strings.Contains(visibility.observed, "Loki query error (502)") {
		t.Fatalf("reconciliation failure was not visible: %+v", visibility)
	}
}

func TestStandingReconciliationLimitIsVisibilityFailure(t *testing.T) {
	fixedNow := time.Date(2026, 8, 31, 18, 54, 0, 0, time.UTC)
	tailer := newLogTailer("grafana", nil)
	tailer.clock = func() time.Time { return fixedNow }
	tailer.startedAt = fixedNow.Add(-time.Minute)

	var output strings.Builder
	for i := 0; i < logReconcileLimit; i++ {
		fmt.Fprintf(&output, "[edge-0][grafana][g1][cid:test][2026-08-31T18:53:30.100000Z]ordinary line %d\n", i)
	}
	tailer.reconcile = func(context.Context, time.Time, []string) (string, error) { return output.String(), nil }
	tailer.reconcileOnce(context.Background())

	_, _, lastSuccess, lastError := tailer.reconcileSnapshot()
	if !lastSuccess.IsZero() || !strings.Contains(lastError, "20000-line limit") {
		t.Fatalf("truncated overlap was accepted: last_success=%s error=%q", lastSuccess, lastError)
	}
	visibility := tailerReconcileFinding("grafana", fixedNow, tailer.startedAt, lastSuccess, lastError)
	if visibility.healthy {
		t.Fatalf("truncated overlap did not raise visibility: %+v", visibility)
	}
}

func TestStandingReconciliationPartitionsSaturatedAggregateByBlock(t *testing.T) {
	fixedNow := time.Date(2026, 8, 31, 18, 54, 0, 0, time.UTC)
	tailer := newLogTailer("proxy", nil)
	tailer.clock = func() time.Time { return fixedNow }
	tailer.startedAt = fixedNow.Add(-time.Minute)
	tailer.blocks = []string{"g1", "g2"}

	var saturated strings.Builder
	for i := 0; i < logReconcileLimit; i++ {
		fmt.Fprintf(&saturated, "[edge-0][proxy][g1][cid:test][I][2026-08-31T18:53:30.%06dZ][server.go:764]ordinary peer sync %d\n", i, i)
	}
	blockLines := map[string]string{
		"g1": `[edge-0][proxy][g1][cid:test][E][2026-08-31T18:53:31Z][synthetic.go:1]synthetic proxy error alpha`,
		"g2": `[edge-0][proxy][g2][cid:test][E][2026-08-31T18:53:32Z][synthetic.go:1]synthetic proxy error beta`,
	}
	type queryCall struct {
		start  time.Time
		blocks []string
	}
	var calls []queryCall
	tailer.reconcile = func(_ context.Context, start time.Time, blocks []string) (string, error) {
		calls = append(calls, queryCall{start: start, blocks: append([]string(nil), blocks...)})
		if len(blocks) == 0 {
			return saturated.String(), nil
		}
		return blockLines[blocks[0]], nil
	}

	tailer.reconcileOnce(context.Background())
	if len(calls) != 3 {
		t.Fatalf("reconciliation calls = %#v, want aggregate plus two block partitions", calls)
	}
	wantStart := fixedNow.Add(-logReconcileLookback)
	for _, call := range calls {
		if !call.start.Equal(wantStart) {
			t.Fatalf("partition start = %s, want shared absolute start %s", call.start, wantStart)
		}
	}
	if len(calls[0].blocks) != 0 || !reflect.DeepEqual(calls[1].blocks, []string{"g1"}) || !reflect.DeepEqual(calls[2].blocks, []string{"g2"}) {
		t.Fatalf("partition calls = %#v, want aggregate, g1, g2", calls)
	}

	_, _, lastSuccess, lastError := tailer.reconcileSnapshot()
	if !lastSuccess.Equal(fixedNow) || lastError != "" {
		t.Fatalf("partitioned overlap not accepted: last_success=%s error=%q", lastSuccess, lastError)
	}
	tailer.stateLock.Lock()
	novelCount := 0
	for _, count := range tailer.novelCounts {
		novelCount += count
	}
	tailer.stateLock.Unlock()
	if novelCount != 2 {
		t.Fatalf("partitioned novel records = %d, want 2", novelCount)
	}
}

func TestStandingReconciliationRejectsSaturatedBlockPartition(t *testing.T) {
	fixedNow := time.Date(2026, 8, 31, 18, 54, 0, 0, time.UTC)
	tailer := newLogTailer("proxy", nil)
	tailer.clock = func() time.Time { return fixedNow }
	tailer.startedAt = fixedNow.Add(-time.Minute)
	tailer.blocks = []string{"g1", "g2"}

	var saturated strings.Builder
	for i := 0; i < logReconcileLimit; i++ {
		fmt.Fprintf(&saturated, "[edge-0][proxy][g2][cid:test][I][2026-08-31T18:53:30.%06dZ][server.go:764]ordinary peer sync %d\n", i, i)
	}
	tailer.reconcile = func(_ context.Context, _ time.Time, blocks []string) (string, error) {
		if len(blocks) == 0 || blocks[0] == "g2" {
			return saturated.String(), nil
		}
		return "", nil
	}

	tailer.reconcileOnce(context.Background())
	_, _, lastSuccess, lastError := tailer.reconcileSnapshot()
	if !lastSuccess.IsZero() || !strings.Contains(lastError, "proxy block g2") {
		t.Fatalf("saturated block was accepted: last_success=%s error=%q", lastSuccess, lastError)
	}
}

func TestStandingReconciliationContinuesSaturatedBlockFromInclusiveBoundary(t *testing.T) {
	fixedNow := time.Date(2026, 8, 31, 18, 54, 0, 0, time.UTC)
	wantStart := fixedNow.Add(-logReconcileLookback)
	boundaryAt := time.Date(2026, 8, 31, 18, 53, 30, 19999*1000, time.UTC)
	boundaryLine := `[edge-0][proxy][g1][cid:test][E][2026-08-31T18:53:30.019999Z][synthetic.go:1]synthetic proxy error boundary`
	laterLine := `[edge-0][proxy][g1][cid:test][E][2026-08-31T18:53:31Z][synthetic.go:1]synthetic proxy error later`

	var saturated strings.Builder
	for i := 0; i < logReconcileLimit-1; i++ {
		fmt.Fprintf(&saturated, "[edge-0][proxy][g1][cid:test][I][2026-08-31T18:53:30.%06dZ][server.go:764]ordinary peer sync %d\n", i, i)
	}
	saturated.WriteString(boundaryLine + "\n")

	tailer := newLogTailer("proxy", nil)
	tailer.clock = func() time.Time { return fixedNow }
	tailer.startedAt = wantStart
	tailer.blocks = []string{"g1", "g2"}
	type queryCall struct {
		start  time.Time
		blocks []string
	}
	var calls []queryCall
	tailer.reconcile = func(_ context.Context, start time.Time, blocks []string) (string, error) {
		calls = append(calls, queryCall{start: start, blocks: append([]string(nil), blocks...)})
		if len(blocks) == 0 || (blocks[0] == "g1" && start.Equal(wantStart)) {
			return saturated.String(), nil
		}
		if blocks[0] == "g1" {
			return boundaryLine + "\n" + laterLine + "\n", nil
		}
		return "", nil
	}

	tailer.reconcileOnce(context.Background())
	if len(calls) != 4 {
		t.Fatalf("reconciliation calls = %#v, want aggregate, g1 page 1, g1 continuation, g2", calls)
	}
	if !calls[2].start.Equal(boundaryAt) || !reflect.DeepEqual(calls[2].blocks, []string{"g1"}) {
		t.Fatalf("continuation = %#v, want g1 at inclusive boundary %s", calls[2], boundaryAt)
	}
	_, _, lastSuccess, lastError := tailer.reconcileSnapshot()
	if !lastSuccess.Equal(fixedNow) || lastError != "" {
		t.Fatalf("continued block overlap not accepted: last_success=%s error=%q", lastSuccess, lastError)
	}

	tailer.stateLock.Lock()
	novelCount := 0
	for _, count := range tailer.novelCounts {
		novelCount += count
	}
	tailer.stateLock.Unlock()
	if novelCount != 2 {
		t.Fatalf("continued novel records = %d, want boundary replay deduped plus one later record", novelCount)
	}
}

func TestStandingReconciliationBoundsAdvancingContinuationPages(t *testing.T) {
	fixedNow := time.Date(2026, 8, 31, 18, 54, 0, 0, time.UTC)
	tailer := newLogTailer("grafana", nil)
	tailer.clock = func() time.Time { return fixedNow }
	tailer.startedAt = fixedNow.Add(-logReconcileLookback)

	calls := 0
	tailer.reconcile = func(_ context.Context, _ time.Time, _ []string) (string, error) {
		calls++
		observedAt := fixedNow.Add(-time.Minute).Add(time.Duration(calls) * time.Second)
		var output strings.Builder
		for i := 0; i < logReconcileLimit; i++ {
			fmt.Fprintf(
				&output,
				"[edge-0][grafana][g1][cid:test][%s]ordinary advancing page %d line %d\n",
				observedAt.Format(time.RFC3339Nano),
				calls,
				i,
			)
		}
		return output.String(), nil
	}

	tailer.reconcileOnce(context.Background())
	_, _, lastSuccess, lastError := tailer.reconcileSnapshot()
	if calls != logReconcileMaxPages {
		t.Fatalf("continuation queries = %d, want bounded %d", calls, logReconcileMaxPages)
	}
	if !lastSuccess.IsZero() || !strings.Contains(lastError, "across 8 pages") {
		t.Fatalf("unbounded hot partition was accepted: last_success=%s error=%q", lastSuccess, lastError)
	}
}

func TestStandingTailAttributesDirectDroppedEntriesToAffectedService(t *testing.T) {
	tailer := newLogTailer("proxy", nil)
	tailer.classify(`[warpctl][loki-tail-dropped-entries] service=proxy count=2`)

	finding := findingByClass(t, tailer.drainWindow(), "loki-tail-dropped-entries")
	if finding.healthy {
		t.Fatal("direct dropped_entries response was classified healthy")
	}
	if finding.target != "proxy" {
		t.Fatalf("direct dropped_entries target = %q, want proxy", finding.target)
	}
	if !strings.Contains(finding.observed, "rate=1/min") {
		t.Fatalf("direct dropped_entries observation = %q, want one loss response", finding.observed)
	}
}

func TestStandingTailCountsIdenticalDroppedEntryResponsesAcrossWindows(t *testing.T) {
	now := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	tailer := newLogTailer("fixture-service", nil)
	tailer.clock = func() time.Time { return now }
	line := `[warpctl][loki-tail-dropped-entries] service=fixture-service count=2`

	for window := 1; window <= 2; window++ {
		tailer.ingestStanding(line, true, true)
		finding := findingByClass(t, tailer.drainWindow(), "loki-tail-dropped-entries")
		if finding.healthy || !strings.Contains(finding.observed, "rate=1/min") {
			t.Fatalf("window %d lost a distinct identical response: %+v", window, finding)
		}
		now = now.Add(time.Minute)
	}
}

func TestStandingTailReconciliationDeduplicatesDroppedEntryReplay(t *testing.T) {
	now := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	tailer := newLogTailer("fixture-service", nil)
	tailer.clock = func() time.Time { return now }
	line := `[warpctl][loki-tail-dropped-entries] service=fixture-service count=2`

	tailer.ingestStanding(line, false, true)
	if finding := findingByClass(t, tailer.drainWindow(), "loki-tail-dropped-entries"); finding.healthy {
		t.Fatalf("first reconciliation occurrence was lost: %+v", finding)
	}
	now = now.Add(time.Minute)
	tailer.ingestStanding(line, false, true)
	if finding := findingByClass(t, tailer.drainWindow(), "loki-tail-dropped-entries"); !finding.healthy {
		t.Fatalf("reconciliation replay was counted twice: %+v", finding)
	}
}

func TestStandingTailDroppedEntrySummaryRequiresExactPrivateSchema(t *testing.T) {
	for _, line := range []string{
		`[warpctl][loki-tail-dropped-entries] service=fixture-service count=0`,
		`[warpctl][loki-tail-dropped-entries] service=fixture-service count=2 error=fixture-sensitive-suffix`,
		`[warpctl][loki-tail-dropped-entries] service=fixture-service count=fixture-sensitive-suffix`,
		`[warpctl][loki-tail-dropped-entries]error=fixture-sensitive-suffix`,
	} {
		t.Run(line, func(t *testing.T) {
			tailer := newLogTailer("fixture-service", nil)
			tailer.ingestStanding(line, true, true)
			if count := tailer.classCounts["loki-tail-dropped-entries"]; count != 0 {
				t.Fatalf("invalid summary count = %d, want 0", count)
			}
			if _, retained := tailer.classSamples["loki-tail-dropped-entries"]; retained {
				t.Fatal("invalid summary retained a typed sample")
			}
			if sample := tailer.classSamples["loki-tail-dropped-entries-unobservable"]; sample != "[warpctl][loki-tail-dropped-entries] invalid_schema" {
				t.Fatalf("invalid summary visibility sample = %q", sample)
			}
			for _, sample := range tailer.novelSamples {
				if strings.Contains(sample, "fixture-sensitive-suffix") {
					t.Fatalf("invalid summary suffix reached novel sample %q", sample)
				}
			}
			findings := tailer.drainWindow()
			for _, finding := range findings {
				if finding.class == "loki-tail-dropped-entries" {
					t.Fatalf("invalid summary emitted a loss or false-healthy sibling: %+v", finding)
				}
			}
			if finding := findingByClass(t, findings, "loki-tail-dropped-entries-unobservable"); finding.healthy {
				t.Fatalf("invalid summary did not produce visibility finding: %+v", finding)
			}
			for _, finding := range findings {
				rendered := finding.symptom + finding.baseline + finding.observed + finding.mechanism + finding.evidence + finding.context + finding.action + finding.verify
				if strings.Contains(rendered, "fixture-sensitive-suffix") {
					t.Fatalf("invalid summary suffix reached finding: %+v", finding)
				}
			}
		})
	}
}

// Minute volume is the liquidity/retry-amplification signal, but admissions
// spread across distinct seconds do not violate the short-window invariant.
// The subsequent empty window must also resolve a prior burst identity.
func TestPayoutRetryMicroburstRejectsSpreadMinuteAndResets(t *testing.T) {
	tailer := newLogTailer("taskworker", nil)
	for attempt := 0; attempt < 8; attempt++ {
		second := fmt.Sprintf("2026-08-31T15:47:%02d", attempt)
		processorLine, evaluatorLine := payoutAttemptLogLines(second, attempt)
		tailer.classify(processorLine)
		tailer.classify(evaluatorLine)
		tailer.classify(payoutAdmissionLogLine(second, attempt))
	}
	findings := tailer.drainWindow()
	if payout := findingByClass(t, findings, "payout-wallet-insufficient"); payout.healthy {
		t.Fatal("spread minute lost the parent liquidity finding")
	}
	if burst := findingByClass(t, findings, "payout-retry-microburst"); !burst.healthy {
		t.Fatalf("spread minute became a synchronized burst: %+v", burst)
	}
	if burst := findingByClass(t, tailer.drainWindow(), "payout-retry-microburst"); !burst.healthy {
		t.Fatalf("empty window did not resolve burst identity: %+v", burst)
	}
}

// A tail reconnect uses --since=1s and can replay the final source second of
// the prior drain window. Preserve exactly one prior fingerprint window so a
// cadence-boundary reconnect cannot open the same burst twice.
func TestPayoutRetryMicroburstDeduplicatesReplayAcrossDrainBoundary(t *testing.T) {
	tailer := newLogTailer("taskworker", nil)
	lines := make([]string, 0, 4)
	for admission := 0; admission < 4; admission++ {
		line := payoutAdmissionLogLine("2026-08-31T15:46:33", admission)
		lines = append(lines, line)
		tailer.classify(line)
	}
	if first := findingByClass(t, tailer.drainWindow(), "payout-retry-microburst"); first.healthy {
		t.Fatal("initial same-second burst was not detected")
	}

	for _, line := range lines {
		tailer.classify(line)
	}
	if replay := findingByClass(t, tailer.drainWindow(), "payout-retry-microburst"); !replay.healthy {
		t.Fatalf("cross-window exact replay manufactured another burst: %+v", replay)
	}
}

// a > 1MB line must cost one counted stream restart, not a dead tailer:
// before the fix the scan loop exited on bufio.ErrTooLong but cmd.Wait()
// blocked forever on the still-writing child and the full pipe.
func TestTailerOversizedLineDoesNotWedge(t *testing.T) {
	tailer := newLogTailer("api", nil)
	// one classifiable line, then a ~2MB single line (overflowing the 1MB
	// scanner buffer), then the child keeps the pipe open forever — the wedge
	// shape.
	// One Go child owns both the oversized write and the open pipe; a shell
	// pipeline leaves grandchildren outside the command's cancellation owner.
	tailer.stream = tailerFixtureProcessStream("oversized")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type result struct{ err error }
	done := make(chan result, 1)
	go func() {
		defer close(done)
		done <- result{err: tailer.tailOnce(ctx)}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("oversized stream owner did not join after cancellation")
		}
	})

	select {
	case r := <-done:
		if r.err != bufio.ErrTooLong {
			t.Fatalf("tailOnce err = %v; want bufio.ErrTooLong", r.err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("tailOnce wedged on an oversized line (child not killed before Wait)")
	}

	_, _, scanErrors := tailer.healthSnapshot()
	if scanErrors != 1 {
		t.Fatalf("scanErrorCount = %d; want 1", scanErrors)
	}
}

// a clean stream end (child exits, pipe closes) returns nil so run() resets
// its backoff, and the lines were classified.
func TestTailerCleanStreamEnd(t *testing.T) {
	tailer := newLogTailer("api", nil)
	tailer.stream = fakeStream(`echo "a error b"; echo "plain line"`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- tailer.tailOnce(ctx)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("tailOnce err = %v; want nil", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("tailOnce did not return after a clean stream end")
	}

	lastLine, _, scanErrors := tailer.healthSnapshot()
	if scanErrors != 0 {
		t.Fatalf("scanErrorCount = %d; want 0", scanErrors)
	}
	if time.Since(lastLine) > time.Minute {
		t.Fatalf("lastLineTime not updated by classify: %s", lastLine)
	}
}

// An exhausted observation-transport request makes warpctl exit nonzero.
// tailOnce must preserve that exit status so run() uses its escalating
// failure backoff; discarding cmd.Wait's error caused every service tailer to
// retry once a second through a Grafana startup outage.
func TestTailerFailedStreamReturnsChildError(t *testing.T) {
	tailer := newLogTailer("api", nil)
	tailer.stream = fakeStream(`exit 2`)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tailer.tailOnce(ctx); err == nil {
		t.Fatal("tailOnce returned nil for a nonzero child exit")
	}
}

func findingByClass(t *testing.T, findings []finding, class string) finding {
	t.Helper()
	for _, f := range findings {
		if f.class == class {
			return f
		}
	}
	t.Fatalf("no finding with class %q in %d findings", class, len(findings))
	return finding{}
}

func TestLogTailerRendersServiceTargetAndEndpointFrame(t *testing.T) {
	tailer := newLogTailer("api", nil)
	for range 10 {
		tailer.classify("dial tcp 192.0.2.10:6380: i/o timeout")
	}
	finding := findingByClass(t, tailer.drainWindow(), "dial-io-timeout")
	if finding.healthy {
		t.Fatal("dial timeout at threshold did not alert")
	}
	if finding.target != "api" || finding.frame != "192.0.2.10:6380" {
		t.Fatalf("finding identity target=%q frame=%q", finding.target, finding.frame)
	}
	for _, want := range []string{"target=api", "frame=192.0.2.10:6380"} {
		if !strings.Contains(finding.observed, want) {
			t.Fatalf("observed values missing %q: %s", want, finding.observed)
		}
	}
	if strings.Contains(finding.observed, "target=192.0.2.10:6380") {
		t.Fatalf("endpoint replaced stable service target: %s", finding.observed)
	}
}

func TestHTTPDrainCutIsDetailedPageAndOmitsWarpIdentity(t *testing.T) {
	tailer := newLogTailer("api", nil)
	tailer.classify("[synthetic-edge][api][synthetic-generation][cid:synthetic-container]" +
		"[I][2026-09-13T07:30:00Z][http]drain deadline after 1m10.25s: 2 connection(s) cut")

	finding := findingByClass(t, tailer.drainWindow(), "http-drain-cut")
	if finding.healthy || finding.tier != tierPage || finding.sustain != 1 {
		t.Fatalf("drain-cut finding = %+v, want immediate page", finding)
	}
	markdown := alertFromFinding(
		syntheticSettings(nil),
		"1.5",
		"log-errors",
		"Log error-class rates",
		finding,
	).Markdown()
	for _, want := range []string{
		"1m10.25s: 2 connection(s) cut",
		"hard-cut those connections",
		"ambiguous execution outcome",
		"Repair the handler or timeout-ordering cause",
		"max_over_time(urnetwork_http_server_drain_cut_connections[15m]) remains zero",
		"SIGNALS.md §13.1",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("drain-cut alert missing %q:\n%s", want, markdown)
		}
	}
	for _, omitted := range []string{"synthetic-edge", "synthetic-generation", "synthetic-container"} {
		if strings.Contains(markdown, omitted) {
			t.Fatalf("drain-cut alert retained Warp identity %q:\n%s", omitted, markdown)
		}
	}

	zero := newLogTailer("api", nil)
	zero.classify("[http]drain deadline after 1m10s: 0 connection(s) cut")
	if zeroFinding := findingByClass(t, zero.drainWindow(), "http-drain-cut"); !zeroFinding.healthy {
		t.Fatalf("zero connection drain became a hard-cut alert: %+v", zeroFinding)
	}
}

// the §3.7 tailer self-health thresholds: silent-too-long and restarting-hot
// raise monitor/visibility findings; a live, stable tailer reports healthy.
func TestTailerHealthFindings(t *testing.T) {
	now := time.Now()

	healthy := tailerHealthFindings("api", now, now.Add(-time.Minute), 0, 0)
	if f := findingByClass(t, healthy, "tailer-silent"); !f.healthy {
		t.Fatalf("recent line reported silent: %+v", f)
	}
	if f := findingByClass(t, healthy, "tailer-restarting"); !f.healthy {
		t.Fatalf("no restarts reported hot: %+v", f)
	}

	silent := tailerHealthFindings("api", now, now.Add(-11*time.Minute), 0, 0)
	f := findingByClass(t, silent, "tailer-silent")
	if f.healthy {
		t.Fatal("11 minutes silent must raise a tailer-silent finding")
	}
	if f.probeId != "monitor/visibility" || f.target != "logs/api" {
		t.Fatalf("wrong identity: probeId=%s target=%s", f.probeId, f.target)
	}
	if !strings.Contains(f.baseline, "idle service may emit none") ||
		!strings.Contains(f.context, "unknown visibility") ||
		!strings.Contains(f.action, "Do not restart") {
		t.Fatalf("silence was misclassified as a proven outage or broken stream: %+v", f)
	}
	if f := findingByClass(t, silent, "tailer-restarting"); !f.healthy {
		t.Fatalf("silent-only case reported restarting: %+v", f)
	}

	hot := tailerHealthFindings("api", now, now.Add(-time.Minute), tailerHotRestartThreshold, 2)
	if f := findingByClass(t, hot, "tailer-restarting"); f.healthy {
		t.Fatal("restart delta at threshold must raise a tailer-restarting finding")
	}
	calm := tailerHealthFindings("api", now, now.Add(-time.Minute), tailerHotRestartThreshold-1, 0)
	if f := findingByClass(t, calm, "tailer-restarting"); !f.healthy {
		t.Fatalf("restart delta below threshold reported hot: %+v", f)
	}
}

type recordingEmitter struct {
	events []ticketEvent
}

func (self *recordingEmitter) emit(ctx context.Context, ev ticketEvent) error {
	self.events = append(self.events, ev)
	return nil
}

// the novel class carries a varying top shape; if that shape were the ticket
// frame (as it once was), two minutes with different shapes would never
// accumulate the sustain-2 streak and the ticket could never open.
func TestNovelTicketOpensAcrossVaryingShapes(t *testing.T) {
	ctx := context.Background()
	emitter := &recordingEmitter{}
	manager := newTicketManager("test", emitter)
	tailer := newLogTailer("api", nil)

	// minute 1: one novel shape at rate
	for i := 0; i < novelRateThreshold+5; i += 1 {
		tailer.classify(fmt.Sprintf("widget error: alpha failure %d", i))
	}
	findings := tailer.drainWindow()
	novel := findingByClass(t, findings, "novel")
	if novel.healthy {
		t.Fatal("novel lines at rate must produce a broken finding")
	}
	if novel.frame != "" {
		t.Fatalf("novel finding frame = %q; the varying shape must not be identity", novel.frame)
	}
	manager.ingest(ctx, findings)
	for _, ev := range emitter.events {
		if ev.kind == ticketOpen {
			t.Fatalf("ticket opened after one tick despite sustain 2: %+v", ev.t.ticketIdentity)
		}
	}

	// minute 2: a different top shape — the streak must still accumulate
	for i := 0; i < novelRateThreshold+5; i += 1 {
		tailer.classify(fmt.Sprintf("gadget error: beta mode %d", i))
	}
	manager.ingest(ctx, tailer.drainWindow())

	opened := false
	for _, ev := range emitter.events {
		if ev.kind == ticketOpen && ev.t.probeId == "logs/novel" {
			opened = true
		}
	}
	if !opened {
		t.Fatal("two consecutive novel minutes with different top shapes did not open a ticket")
	}
}

// Public endpoints receive bursts of unrelated vulnerability probes. Nginx
// logs every nonexistent path as an error, but many one-off paths are not one
// novel server failure recurring at rate. The novelty threshold is per
// normalized shape, not the sum of unrelated shapes in the minute.
func TestNovelDiverseOneOffShapesDoNotAlert(t *testing.T) {
	tailer := newLogTailer("web", nil)
	for i := 0; i < novelRateThreshold*3; i += 1 {
		path := fmt.Sprintf("%c%c", 'a'+rune(i/26), 'a'+rune(i%26))
		tailer.classify(fmt.Sprintf(
			`2026/08/30 04:08:45 [error] 16#16: *4607 open() "/etc/nginx/html/probe-%s.php" failed (2: No such file or directory)`,
			path,
		))
	}

	novel := findingByClass(t, tailer.drainWindow(), "novel")
	if !novel.healthy {
		t.Fatalf("unrelated one-off web probes produced a novel alert: %+v", novel)
	}
}

// A minute may contain several unmatched failure shapes. The representative
// sample must belong to the selected top shape; retaining the first global
// sample falsely paired production's provider-tunnel top shape with unrelated
// reliability and evaluation failures.
func TestNovelSampleBelongsToTopShapeAndRedactsID(t *testing.T) {
	const firstID = "11111111-1111-1111-1111-111111111111"
	const topID = "22222222-2222-2222-2222-222222222222"
	const correlationID = "raw-customer-correlation"
	const customerID = "raw-customer-id"
	const providerID = "raw-provider-id"
	tailer := newLogTailer("taskworker", nil)
	tailer.classify("widget error: alpha session " + firstID)
	for i := 0; i < novelRateThreshold; i += 1 {
		tailer.classify(fmt.Sprintf(
			`[edge-private][taskworker][g2][cid:%s] gadget failure: beta session %s customer_id=%s {"provider_id":"%s"} attempt %d`,
			correlationID,
			topID,
			customerID,
			providerID,
			i,
		))
	}

	novel := findingByClass(t, tailer.drainWindow(), "novel")
	if novel.healthy {
		t.Fatal("top novel shape at threshold did not alert")
	}
	for _, want := range []string{
		`top shape: [edge-private][taskworker][g2][cid:<id>] gadget failure: beta session # <redacted-id> {<redacted-id>} attempt #`,
		`sample from top shape: gadget failure: beta session <id> <redacted-id> {<redacted-id>} attempt 0`,
	} {
		if !strings.Contains(novel.evidence, want) {
			t.Fatalf("novel evidence lacks %q: %q", want, novel.evidence)
		}
	}
	for _, unwanted := range []string{
		"widget error",
		firstID,
		topID,
		correlationID,
		customerID,
		providerID,
	} {
		if strings.Contains(novel.evidence, unwanted) {
			t.Fatalf("novel evidence retained unrelated or private value %q: %q", unwanted, novel.evidence)
		}
	}
}

func TestWindowStallStructuredStatesStayOutOfNovel(t *testing.T) {
	const privateCorrelation = "private-correlation-value"
	nonterminalLine := "[edge-private][taskworker][g1][cid:" + privateCorrelation + "]" +
		"[I][2026-09-08T21:56:22Z][ip_remote_multi_client_outcome.go:374][rel] event=window_stall window=quality reason=platform-unreachable failed=0"

	quiet := newLogTailer("taskworker", nil)
	quiet.classify(nonterminalLine)
	quietFindings := quiet.drainWindow()
	if finding := findingByClass(t, quietFindings, "window-stall"); !finding.healthy {
		t.Fatalf("one nonterminal transition crossed the rate threshold: %+v", finding)
	}
	if novel := findingByClass(t, quietFindings, "novel"); !novel.healthy {
		t.Fatalf("failed=0 became a generic novel error: %+v", novel)
	}

	atRate := newLogTailer("taskworker", nil)
	for i := 0; i < novelRateThreshold; i++ {
		atRate.classify(nonterminalLine)
	}
	atRateFindings := atRate.drainWindow()
	stall := findingByClass(t, atRateFindings, "window-stall")
	if stall.healthy {
		t.Fatal("nonterminal window-stall transitions at rate were hidden")
	}
	for _, want := range []string{
		"failed=0 is explicitly nonterminal",
		"neither a count of failed windows nor an unclassified error",
		"event=window_stall window=quality reason=platform-unreachable failed=0",
		"Do not infer terminal user failure",
	} {
		if !strings.Contains(stall.evidence+stall.mechanism+stall.action, want) {
			t.Fatalf("nonterminal window-stall finding lacks %q: %+v", want, stall)
		}
	}
	if strings.Contains(stall.evidence, privateCorrelation) || strings.Contains(stall.evidence, "edge-private") {
		t.Fatalf("window-stall sample retained private prefix: %q", stall.evidence)
	}
	if novel := findingByClass(t, atRateFindings, "novel"); !novel.healthy {
		t.Fatalf("classified nonterminal transitions also became novel: %+v", novel)
	}

	terminal := newLogTailer("taskworker", nil)
	terminal.classify(strings.Replace(nonterminalLine, "failed=0", "failed=1", 1))
	terminalFindings := terminal.drainWindow()
	terminalStall := findingByClass(t, terminalFindings, "window-stall-terminal")
	if terminalStall.healthy || !strings.Contains(terminalStall.evidence, "failed=1") {
		t.Fatalf("terminal window-stall state was not visible: %+v", terminalStall)
	}
	if nonterminal := findingByClass(t, terminalFindings, "window-stall"); !nonterminal.healthy {
		t.Fatalf("terminal state was also counted as nonterminal: %+v", nonterminal)
	}
	if novel := findingByClass(t, terminalFindings, "novel"); !novel.healthy {
		t.Fatalf("classified terminal transition also became novel: %+v", novel)
	}

	// Unknown flag values are schema drift, not a state the classifier may
	// silently reinterpret. They remain in the generic novelty safety net.
	ambiguous := newLogTailer("taskworker", nil)
	for i := 0; i < novelRateThreshold; i++ {
		ambiguous.classify(strings.Replace(nonterminalLine, "failed=0", "failed=unknown", 1))
	}
	ambiguousFindings := ambiguous.drainWindow()
	if finding := findingByClass(t, ambiguousFindings, "window-stall"); !finding.healthy {
		t.Fatalf("ambiguous state was classified as nonterminal: %+v", finding)
	}
	if finding := findingByClass(t, ambiguousFindings, "window-stall-terminal"); !finding.healthy {
		t.Fatalf("ambiguous state was classified as terminal: %+v", finding)
	}
	if novel := findingByClass(t, ambiguousFindings, "novel"); novel.healthy {
		t.Fatal("ambiguous window-stall schema drift disappeared from the novelty safety net")
	}
}

// Connect's terminal outcome has its own event name. failOutcome logs this
// line and calls SetStallStatus directly, so a window_stall failed=1 line is
// not required for the terminal condition to remain visible.
func TestWindowFailedUsesStableTerminalWindowClass(t *testing.T) {
	const privateCorrelation = "synthetic-private-correlation"
	line := "[synthetic-host][taskworker][synthetic-generation][cid:" + privateCorrelation + "]" +
		"[I][2000-01-01T00:00:00Z][synthetic.go:1][rel] event=window_failed window=quality reason=providers-unresponsive after=45000"

	tailer := newLogTailer("taskworker", nil)
	tailer.classify(line)
	findings := tailer.drainWindow()
	terminal := findingByClass(t, findings, "window-stall-terminal")
	if terminal.healthy {
		t.Fatal("authoritative window_failed event was hidden")
	}
	if nonterminal := findingByClass(t, findings, "window-stall"); !nonterminal.healthy {
		t.Fatalf("terminal outcome was also counted as nonterminal: %+v", nonterminal)
	}
	if novel := findingByClass(t, findings, "novel"); !novel.healthy {
		t.Fatalf("classified terminal outcome also became novel: %+v", novel)
	}
	markdown := alertFromFinding(
		SignalSettings{Environment: "synthetic", Now: time.Now},
		"1.5", "log-errors", "Log error-class rates", terminal,
	).Markdown()
	for _, want := range []string{
		"event=window_failed window=quality reason=providers-unresponsive after=45000",
		"window_failed is authoritative terminal state",
		"calls SetStallStatus directly",
		"does not itself emit window_stall failed=1",
		"failed_window_events=1",
		"diagnostic_lines=1",
		"canonical_source=exact-replay-deduplicated-authoritative-window-failed-event",
		"do not restart or deploy from the terminal bit alone",
		"No window_failed event or compatible failed=1 transition recurs",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("window_failed finding lacks %q: %+v", want, terminal)
		}
	}
	for _, private := range []string{"synthetic-host", privateCorrelation} {
		if strings.Contains(markdown, private) {
			t.Fatalf("window_failed alert retained private value %q", private)
		}
	}

	// A recovery event is healthy context, not a terminal or generic error.
	recovered := newLogTailer("taskworker", nil)
	recovered.classify("[I][2000-01-01T00:00:01Z][synthetic.go:2][rel] event=window_recovered window=quality after=46000")
	recoveredFindings := recovered.drainWindow()
	if terminal := findingByClass(t, recoveredFindings, "window-stall-terminal"); !terminal.healthy {
		t.Fatalf("window recovery was classified as terminal: %+v", terminal)
	}
	if novel := findingByClass(t, recoveredFindings, "novel"); !novel.healthy {
		t.Fatalf("window recovery became novel: %+v", novel)
	}

	// Unknown duration syntax is schema drift. The terminal matcher must not
	// accept it merely because the event name contains the word failed.
	malformed := newLogTailer("taskworker", nil)
	for i := 0; i < novelRateThreshold; i++ {
		malformed.classify("[I][2000-01-01T00:00:02Z][synthetic.go:3][rel] event=window_failed window=quality reason=providers-unresponsive after=unknown")
	}
	malformedFindings := malformed.drainWindow()
	if terminal := findingByClass(t, malformedFindings, "window-stall-terminal"); !terminal.healthy {
		t.Fatalf("malformed window_failed event was accepted as terminal: %+v", terminal)
	}
	if novel := findingByClass(t, malformedFindings, "novel"); novel.healthy {
		t.Fatal("malformed window_failed event disappeared from the novelty safety net")
	}
}

// A failed window can produce the authoritative window_failed line and then a
// compatibility window_stall failed=1 transition when its reason changes.
// Both remain diagnostic evidence, but they are one failed-window event.
func TestWindowTerminalCanonicalCardinalitySeparatesCompatibilityTransition(t *testing.T) {
	prefix := "[synthetic-host][taskworker][synthetic-generation][cid:synthetic-correlation]"
	authoritative := prefix +
		"[I][2000-01-01T00:00:00.000001Z][synthetic.go:1][rel] event=window_failed window=quality reason=providers-unresponsive after=45021"
	compatibility := prefix +
		"[I][2000-01-01T00:00:00.008201Z][synthetic.go:2][rel] event=window_stall window=quality reason=platform-unreachable failed=1"

	tailer := newLogTailer("taskworker", nil)
	tailer.classify(authoritative)
	tailer.classify(compatibility)
	terminal := findingByClass(t, tailer.drainWindow(), "window-stall-terminal")
	for _, want := range []string{
		"rate=2/min",
		"failed_window_events=1",
		"diagnostic_lines=2",
		"logical event count: 1 exact-replay-deduplicated authoritative window_failed event line(s) from 2 diagnostic line(s)",
		"diagnostic_lines as terminal-class telemetry, not incident size",
	} {
		if !strings.Contains(terminal.observed+terminal.evidence+terminal.context, want) {
			t.Fatalf("paired terminal finding lacks %q: %+v", want, terminal)
		}
	}

	// Two windows can fail nearly simultaneously on one emitter. Their two
	// distinct authoritative records must remain two logical events even when
	// each is followed by its own compatibility transition.
	two := newLogTailer("taskworker", nil)
	for _, line := range []string{
		authoritative,
		strings.Replace(authoritative, "00.000001Z", "00.002141Z", 1),
		compatibility,
		strings.Replace(compatibility, "00.008201Z", "00.010601Z", 1),
	} {
		two.classify(line)
	}
	twoTerminal := findingByClass(t, two.drainWindow(), "window-stall-terminal")
	for _, want := range []string{"rate=4/min", "failed_window_events=2", "diagnostic_lines=4"} {
		if !strings.Contains(twoTerminal.observed, want) {
			t.Fatalf("simultaneous terminal finding lacks %q: %+v", want, twoTerminal)
		}
	}
}

func TestWindowTerminalCompatibilityOnlyKeepsUnknownCanonicalCardinality(t *testing.T) {
	tailer := newLogTailer("taskworker", nil)
	tailer.classify("[synthetic-host][taskworker][synthetic-generation][I][2000-01-01T00:00:00Z][synthetic.go:1][rel] event=window_stall window=quality reason=platform-unreachable failed=1")
	terminal := findingByClass(t, tailer.drainWindow(), "window-stall-terminal")
	if terminal.healthy {
		t.Fatal("compatibility-only terminal transition was hidden")
	}
	for _, want := range []string{
		"rate=1/min",
		"failed_window_events=unknown",
		"canonical_source=absent",
		"logical event count: unknown; no authoritative window_failed event line was present among 1 diagnostic line(s)",
	} {
		if !strings.Contains(terminal.observed+terminal.evidence, want) {
			t.Fatalf("compatibility-only finding lacks %q: %+v", want, terminal)
		}
	}
}

// A natural evaluation-pass deadline has its own bounded, identity-free event.
// It is diagnostic evidence about where admission stopped, not a terminal
// window count or proof of the remote cause.
func TestWindowEvaluationBudgetUsesStructuredClass(t *testing.T) {
	const privateCorrelation = "synthetic-private-correlation"
	line := "[synthetic-host][taskworker][synthetic-generation][cid:" + privateCorrelation + "]" +
		"[I][2000-01-01T00:00:00Z][synthetic.go:1][rel] event=evaluation_budget_exhausted window=quality candidates=2 effective_min=14980 observed_max=15001 ping_timeout=30000 expand_timeout=15000 suppressed=0"

	tailer := newLogTailer("taskworker", nil)
	for i := 0; i < novelRateThreshold; i++ {
		tailer.classify(line)
	}
	findings := tailer.drainWindow()
	budget := findingByClass(t, findings, "window-evaluation-budget")
	if budget.healthy {
		t.Fatal("repeated evaluation-budget exhaustion was hidden")
	}
	if novel := findingByClass(t, findings, "novel"); !novel.healthy {
		t.Fatalf("classified evaluation-budget events also became novel: %+v", novel)
	}
	markdown := alertFromFinding(
		SignalSettings{Environment: "synthetic", Now: time.Now},
		"1.5", "log-errors", "Log error-class rates", budget,
	).Markdown()
	for _, want := range []string{
		"event=evaluation_budget_exhausted window=quality candidates=2 effective_min=14980 observed_max=15001 ping_timeout=30000 expand_timeout=15000 suppressed=0",
		"pre-fix artifact signature",
		"acquisition phase clipped",
		"b11d722 or later",
		"not why the receiver stayed silent",
		"Lifecycle cancellation",
		"do not lengthen either timeout as an HMAC remedy",
		"no-overlap/no-late-admission cleanup",
		"full acquisition-plus-ping safety expiry",
		"exactly one terminal owner",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("evaluation-budget finding lacks %q: %+v", want, budget)
		}
	}
	for _, private := range []string{"synthetic-host", "synthetic-generation", privateCorrelation} {
		if strings.Contains(markdown, private) {
			t.Fatalf("evaluation-budget alert retained private value %q", private)
		}
	}

	malformed := newLogTailer("taskworker", nil)
	for i := 0; i < novelRateThreshold; i++ {
		malformed.classify(strings.Replace(line, "candidates=2", "candidates=unknown", 1))
	}
	malformedFindings := malformed.drainWindow()
	if finding := findingByClass(t, malformedFindings, "window-evaluation-budget"); !finding.healthy {
		t.Fatalf("malformed budget event was accepted: %+v", finding)
	}
	if novel := findingByClass(t, malformedFindings, "novel"); novel.healthy {
		t.Fatal("malformed evaluation-budget schema drift disappeared")
	}
}

// The exact canceled-generator shapes belong to an artifact-bounded class,
// not generic novelty. One line stays quiet; the production-rate population
// remains paired with its independently structured nonterminal stall signal.
func TestWindowGeneratorCanceledUsesArtifactBoundedClass(t *testing.T) {
	const privateCorrelation = "synthetic-private-correlation"
	lines := []string{
		"[synthetic-host][taskworker][synthetic-generation][cid:" + privateCorrelation + "]" +
			"[I][2000-01-01T00:00:00Z][synthetic.go:1][multi]window enumerate error timeout = generator call canceled",
		"[synthetic-host][taskworker][synthetic-generation][cid:" + privateCorrelation + "]" +
			"[I][2000-01-01T00:00:01Z][synthetic.go:2][multi]create client args error = generator call canceled",
	}
	stallLine := "[synthetic-host][taskworker][synthetic-generation][cid:" + privateCorrelation + "]" +
		"[I][2000-01-01T00:00:02Z][synthetic.go:3][rel] event=window_stall window=quality reason=platform-unreachable failed=0"

	quiet := newLogTailer("taskworker", nil)
	quiet.classify(lines[0])
	quietFindings := quiet.drainWindow()
	if finding := findingByClass(t, quietFindings, "window-generator-canceled"); !finding.healthy {
		t.Fatalf("one canceled-generator diagnostic crossed the rate threshold: %+v", finding)
	}
	if finding := findingByClass(t, quietFindings, "novel"); !finding.healthy {
		t.Fatalf("one classified canceled-generator diagnostic became novel: %+v", finding)
	}

	atRate := newLogTailer("taskworker", nil)
	for i := 0; i < novelRateThreshold; i++ {
		atRate.classify(lines[i%len(lines)])
		atRate.classify(stallLine)
	}
	findings := atRate.drainWindow()
	canceled := findingByClass(t, findings, "window-generator-canceled")
	if canceled.healthy {
		t.Fatal("canceled-generator diagnostics at rate were hidden")
	}
	if stall := findingByClass(t, findings, "window-stall"); stall.healthy {
		t.Fatal("paired structured nonterminal stalls at rate were hidden")
	}
	if novel := findingByClass(t, findings, "novel"); !novel.healthy {
		t.Fatalf("classified cancellation and stall lines also became novel: %+v", novel)
	}
	markdown := alertFromFinding(
		SignalSettings{Environment: "synthetic", Now: time.Now},
		"1.5", "log-errors", "Log error-class rates", canceled,
	).Markdown()
	for _, want := range []string{
		"[multi]window enumerate error timeout = generator call canceled",
		"line alone cannot prove outer-window cancellation",
		"legacy log-before-context ordering",
		"proved fixed artifact",
		"outer context was live",
		"recorded Connect build input",
		"one teardown boundary",
		"ten minutes",
		"identical text is still logged and classified",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("canceled-generator finding lacks %q: %+v", want, canceled)
		}
	}
	for _, private := range []string{"synthetic-host", privateCorrelation} {
		if strings.Contains(markdown, private) {
			t.Fatalf("canceled-generator alert retained private value %q", private)
		}
	}
}

// A wrapper abandonment is affirmative hung/deadline evidence and must never
// be swallowed by the narrower exact canceled-generator class.
func TestWindowGeneratorAbandonmentRemainsNovel(t *testing.T) {
	tailer := newLogTailer("taskworker", nil)
	for i := 0; i < novelRateThreshold; i++ {
		tailer.classify("[multi]window enumerate error timeout = generator call abandoned after 20s")
	}
	findings := tailer.drainWindow()
	if finding := findingByClass(t, findings, "window-generator-canceled"); !finding.healthy {
		t.Fatalf("generator abandonment was mislabeled cancellation: %+v", finding)
	}
	if finding := findingByClass(t, findings, "novel"); finding.healthy {
		t.Fatal("generator abandonment disappeared from the novelty safety net")
	}
}

// A near-miss cancellation suffix can be a genuine inner/platform error. It
// remains visible to generic novelty rather than being broadly suppressed.
func TestWindowGeneratorCancellationNearMissRemainsNovel(t *testing.T) {
	tailer := newLogTailer("taskworker", nil)
	for i := 0; i < novelRateThreshold; i++ {
		tailer.classify("[multi]create client args error = generator call canceled by synthetic platform")
	}
	findings := tailer.drainWindow()
	if finding := findingByClass(t, findings, "window-generator-canceled"); !finding.healthy {
		t.Fatalf("non-exact inner error was mislabeled exact cancellation: %+v", finding)
	}
	if finding := findingByClass(t, findings, "novel"); finding.healthy {
		t.Fatal("non-exact inner error disappeared from the novelty safety net")
	}
}

// TestAutomaticBalanceCodeDeliveryFailurePagesPrivately pins the exact paid,
// no-recovery error without retaining its provider or account details.
func TestAutomaticBalanceCodeDeliveryFailurePagesPrivately(t *testing.T) {
	line := "[synthetic-host][api][synthetic-generation][cid:synthetic-private]" +
		"[E][2000-01-01T00:00:00Z][synthetic.go:1] Unexpected error: " +
		"automatic balance-code delivery failed without email recovery: " +
		"payment network does not exist id=00000000-0000-0000-0000-000000000001 " +
		"secret=synthetic-code-secret email=synthetic@example.invalid"
	tailer := newLogTailer("api", nil)
	tailer.classify(line)
	findings := tailer.drainWindow()
	finding := findingByClass(t, findings, "payment-balance-code-undelivered")
	if finding.healthy || finding.tier != tierPage {
		t.Fatalf("undelivered balance-code finding = %+v", finding)
	}
	if novel := findingByClass(t, findings, "novel"); !novel.healthy {
		t.Fatalf("classified balance-code failure also became novel: %+v", novel)
	}
	markdown := alertFromFinding(
		SignalSettings{Environment: "synthetic", Now: time.Now},
		"1.5", "log-errors", "Log error-class rates", finding,
	).Markdown()
	for _, want := range []string{
		"paid balance code was durably created",
		"no email delivery fallback",
		"Intentional operator-issued",
		"privileged payment tooling",
		"consumed at most once",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("undelivered balance-code alert missing %q:\n%s", want, markdown)
		}
	}
	for _, forbidden := range []string{
		"synthetic-host",
		"synthetic-private",
		"00000000-0000-0000-0000-000000000001",
		"synthetic-code-secret",
		"synthetic@example.invalid",
	} {
		if strings.Contains(markdown, forbidden) {
			t.Fatalf("undelivered balance-code alert retained %q:\n%s", forbidden, markdown)
		}
	}
}

func TestProviderTunnelReadDoneUsesArtifactBoundedClass(t *testing.T) {
	const entityID = "raw-customer-correlation"
	line := "[edge-private][taskworker][g2][cid:" + entityID + "] providertunnel: tun read error: Done"
	tailer := newLogTailer("taskworker", nil)
	for i := 0; i < novelRateThreshold; i += 1 {
		tailer.classify(line)
	}

	findings := tailer.drainWindow()
	readDone := findingByClass(t, findings, "provider-tunnel-read-done")
	if readDone.healthy {
		t.Fatal("provider-tunnel terminal Done rate did not alert")
	}
	markdown := alertFromFinding(
		SignalSettings{Environment: "synthetic", Now: time.Now},
		"1.5", "log-errors", "Log error-class rates", readDone,
	).Markdown()
	for _, want := range []string{
		"providertunnel: tun read error: Done",
		"v2026.9.3-1036806790",
		"4ba0dd88",
		"20e289bd",
		"active Taskworker artifact",
		"does not encode the outer context state",
		"If it contains 20e289bd",
		"zero for 10 minutes",
		"live-context TUN read failure is still logged",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("provider-tunnel finding lacks %q: %+v", want, readDone)
		}
	}
	for _, private := range []string{"edge-private", entityID} {
		if strings.Contains(markdown, private) {
			t.Fatalf("provider-tunnel alert retained private value %q", private)
		}
	}
	if novel := findingByClass(t, findings, "novel"); !novel.healthy {
		t.Fatalf("classified provider-tunnel Done also became novel: %+v", novel)
	}

	// The product fix intentionally preserves errors while the tunnel context
	// is live. Only the exact terminal Done text belongs to this neutral class;
	// no other read error may be suppressed or reinterpreted.
	live := newLogTailer("taskworker", nil)
	for i := 0; i < novelRateThreshold; i += 1 {
		live.classify("providertunnel: tun read error: synthetic live read failure")
	}
	liveFindings := live.drainWindow()
	if finding := findingByClass(t, liveFindings, "provider-tunnel-read-done"); !finding.healthy {
		t.Fatalf("live-context read failure was mislabeled terminal Done: %+v", finding)
	}
	if finding := findingByClass(t, liveFindings, "novel"); finding.healthy {
		t.Fatal("live-context read failure disappeared from the novel safety net")
	}
}
