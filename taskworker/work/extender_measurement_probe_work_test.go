package work

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/lagprobe"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The measurement api probe chain (lag.dev/PLAN.md A4). The probe itself is
// proved against in-process servers in package lagprobe, and the rule that
// reads what it stores in controller/lag_extender_reach_test.go. What is proved
// here is what the chain does between them: which addresses it probes, how it
// probes them, what it stores, and that it re-arms.

const testMeasurementProbeZone = "extender.probe-test.example"

// The measurement block on, in a zone of these tests.
func installTestMeasurementProbeConfig(t testing.TB) {
	t.Helper()
	installTestExtenderWorkConfig(t,
		"measurement:",
		"  enabled: true",
		"  zone_name: "+testMeasurementProbeZone,
	)
}

// An extender with a live certificate for its name, as far along as the steps
// say: a certificate read by the extender, and a name in the zone.
type testMeasurementProbeHost struct {
	delivered bool
	published bool
}

func createTestMeasurementProbeHost(t testing.TB, ctx context.Context, index int, host testMeasurementProbeHost, ips ...string) server.Id {
	t.Helper()
	extenderId := createTestMeasurementExtender(t, ctx, index, ips...)
	name := controller.ExtenderMeasurementName(extenderId, testMeasurementProbeZone)
	now := server.NowUtc()
	certPublicKey := server.NewId().Bytes()
	model.SetNetworkExtenderMeasurementRequest(ctx, extenderId, []byte{1}, certPublicKey, []string{name}, now.Add(-2*time.Hour))
	cert := &model.NetworkExtenderMeasurementCert{
		CertId:     server.NewId(),
		ExtenderId: extenderId,
		Names:      []string{name},
		Csr:        []byte{1},
		PublicKey:  certPublicKey,
		ChainPem:   "-",
		SerialHex:  "01",
		NotBefore:  now.Add(-time.Hour),
		NotAfter:   now.Add(60 * 24 * time.Hour),
		IssueTime:  now.Add(-time.Hour),
		CapCounted: true,
		RenewTime:  now.Add(40 * 24 * time.Hour),
	}
	model.AddNetworkExtenderMeasurementCert(ctx, cert)
	if host.delivered {
		connect.AssertEqual(t, model.MarkNetworkExtenderMeasurementCertDelivered(ctx, cert.CertId, now), true)
	}
	if host.published {
		model.PublishNetworkExtenderMeasurementNames(ctx, cert.Names, now)
	}
	return extenderId
}

var testMeasurementProbeServing = testMeasurementProbeHost{delivered: true, published: true}

// One pass as the stub saw it.
type testMeasurementProbePass struct {
	targets  []lagprobe.Target
	settings lagprobe.Settings
}

// Replaces the pass with a stub that answers each target as told, and records
// what it was asked.
func stubMeasurementProbes(t testing.TB, answer func(target lagprobe.Target) *lagprobe.Result, unreachable ...int) *[]*testMeasurementProbePass {
	t.Helper()
	passes := &[]*testMeasurementProbePass{}
	previous := probeExtenderMeasurementApis
	probeExtenderMeasurementApis = func(ctx context.Context, targets []lagprobe.Target, settings lagprobe.Settings) *lagprobe.Pass {
		*passes = append(*passes, &testMeasurementProbePass{targets: targets, settings: settings})
		results := make([]*lagprobe.Result, len(targets))
		for i, target := range targets {
			results[i] = answer(target)
		}
		return &lagprobe.Pass{Results: results, UnreachableIpVersions: unreachable}
	}
	t.Cleanup(func() {
		probeExtenderMeasurementApis = previous
	})
	return passes
}

func runTestMeasurementProbe(t testing.TB, ctx context.Context) *ExtenderMeasurementProbeResult {
	t.Helper()
	clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
	defer clientSession.Cancel()
	result, err := ExtenderMeasurementProbe(&ExtenderMeasurementProbeArgs{}, clientSession)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func getTestMeasurementProbeRow(ctx context.Context, extenderId server.Id, ipVersion int) *model.NetworkExtenderMeasurementProbe {
	for _, probe := range model.GetNetworkExtenderMeasurementProbes(ctx, []server.Id{extenderId})[extenderId] {
		if probe.IpVersion == ipVersion {
			return probe
		}
	}
	return nil
}

// The plan's probe: the timeouts and the origin a page has (A4, M2).
func TestExtenderMeasurementProbeSettings(t *testing.T) {
	settings := lagprobe.DefaultSettings()
	connect.AssertEqual(t, settings.HandshakeTimeout, 5*time.Second)
	connect.AssertEqual(t, settings.ResponseTimeout, 5*time.Second)
	connect.AssertEqual(t, settings.Origin, "https://lag.dev")
	connect.AssertEqual(t, settings.Path, "/lag/v1/session")
	connect.AssertEqual(t, settings.RootCAs == nil, true)

	// the pass fits the task's default limit: the last probe starts within the
	// budget and takes at most both timeouts, which leaves the storing most of
	// a minute, and the probes are fresh for several passes
	connect.AssertEqual(t, settings.Budget+settings.HandshakeTimeout+settings.ResponseTimeout+45*time.Second <= 2*time.Minute, true)
	connect.AssertEqual(t, 5*ExtenderMeasurementProbeTimeout < controller.ExtenderMeasurementProbeMaxAge, true)
}

// A pass probes each published address of each extender that serves, at the
// name's port, with the name as what it asks for, and stores each family's
// outcome. An extender that does not serve yet, or at all, is not probed.
func TestExtenderMeasurementProbeProbesWhatServes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestMeasurementProbeConfig(t)

		dualStack := createTestMeasurementProbeHost(t, ctx, 1, testMeasurementProbeServing, "192.0.2.1", "2001:db8::1")
		ipv4Only := createTestMeasurementProbeHost(t, ctx, 2, testMeasurementProbeServing, "192.0.2.2")
		ipv6Only := createTestMeasurementProbeHost(t, ctx, 3, testMeasurementProbeServing, "2001:db8::3")
		// not yet in the zone, and not yet read by the extender
		unpublished := createTestMeasurementProbeHost(t, ctx, 4, testMeasurementProbeHost{delivered: true}, "192.0.2.4")
		undelivered := createTestMeasurementProbeHost(t, ctx, 5, testMeasurementProbeHost{published: true}, "192.0.2.5")
		// no certificate
		uncertified := createTestMeasurementExtender(t, ctx, 6, "192.0.2.6")

		outcomes := map[string]lagprobe.Outcome{
			"192.0.2.1":   lagprobe.OutcomeTlsTimeout,
			"2001:db8::1": lagprobe.OutcomeOk,
			"192.0.2.2":   lagprobe.OutcomeCors,
			"2001:db8::3": lagprobe.OutcomeUnprobed,
		}
		passes := stubMeasurementProbes(t, func(target lagprobe.Target) *lagprobe.Result {
			outcome, ok := outcomes[target.Addr.Addr().String()]
			if !ok {
				t.Errorf("probed %s, which does not serve", target.Addr)
			}
			return &lagprobe.Result{Outcome: outcome, Err: errors.New(string(outcome))}
		})

		start := server.NowUtc()
		result := runTestMeasurementProbe(t, ctx)
		connect.AssertEqual(t, *result, ExtenderMeasurementProbeResult{Probed: 4, Ok: 1, Failed: 2, Unprobed: 1})

		connect.AssertEqual(t, len(*passes), 1)
		pass := (*passes)[0]
		got := []string{}
		for _, target := range pass.targets {
			got = append(got, fmt.Sprintf("%s %s", target.Name, target.Addr))
		}
		slices.Sort(got)
		name := func(extenderId server.Id) string {
			return controller.ExtenderMeasurementName(extenderId, testMeasurementProbeZone)
		}
		want := []string{
			name(dualStack) + " 192.0.2.1:443",
			name(dualStack) + " [2001:db8::1]:443",
			name(ipv4Only) + " 192.0.2.2:443",
			name(ipv6Only) + " [2001:db8::3]:443",
		}
		slices.Sort(want)
		connect.AssertEqual(t, got, want)
		connect.AssertEqual(t, pass.settings.Origin, lagprobe.DefaultSettings().Origin)
		connect.AssertEqual(t, pass.settings.HandshakeTimeout, lagprobe.DefaultSettings().HandshakeTimeout)

		rows := getTestMeasurementProbeRow(ctx, dualStack, 4)
		connect.AssertEqual(t, rows.Outcome, lagprobe.OutcomeTlsTimeout)
		connect.AssertEqual(t, rows.Ip, netip.MustParseAddr("192.0.2.1"))
		connect.AssertEqual(t, rows.ConsecutiveFailures, 1)
		connect.AssertEqual(t, rows.SuccessTime, (*time.Time)(nil))
		if rows.ProbeTime.Before(start.Truncate(time.Microsecond)) || server.NowUtc().Before(rows.ProbeTime) {
			t.Fatalf("probed at %s, between %s and now", rows.ProbeTime, start)
		}
		rows = getTestMeasurementProbeRow(ctx, dualStack, 6)
		connect.AssertEqual(t, rows.Outcome, lagprobe.OutcomeOk)
		connect.AssertEqual(t, rows.Ip, netip.MustParseAddr("2001:db8::1"))
		connect.AssertEqual(t, rows.ConsecutiveFailures, 0)
		connect.AssertEqual(t, rows.SuccessTime != nil, true)
		connect.AssertEqual(t, getTestMeasurementProbeRow(ctx, ipv4Only, 4).Outcome, lagprobe.OutcomeCors)
		rows = getTestMeasurementProbeRow(ctx, ipv6Only, 6)
		connect.AssertEqual(t, rows.Outcome, lagprobe.OutcomeUnprobed)
		connect.AssertEqual(t, rows.ConsecutiveFailures, 0)
		for _, extenderId := range []server.Id{unpublished, undelivered, uncertified} {
			connect.AssertEqual(t, len(model.GetNetworkExtenderMeasurementProbes(ctx, []server.Id{extenderId})), 0)
		}

		// a second pass continues the runs: the failing family reaches the
		// rule's threshold, and the one that passes stays clear
		result = runTestMeasurementProbe(t, ctx)
		connect.AssertEqual(t, *result, ExtenderMeasurementProbeResult{Probed: 4, Ok: 1, Failed: 2, Unprobed: 1})
		connect.AssertEqual(t, getTestMeasurementProbeRow(ctx, dualStack, 4).ConsecutiveFailures, 2)
		connect.AssertEqual(t, getTestMeasurementProbeRow(ctx, dualStack, 6).ConsecutiveFailures, 0)
		connect.AssertEqual(t, getTestMeasurementProbeRow(ctx, ipv4Only, 4).ConsecutiveFailures, 2)

		// and a success ends one
		outcomes["192.0.2.1"] = lagprobe.OutcomeOk
		runTestMeasurementProbe(t, ctx)
		rows = getTestMeasurementProbeRow(ctx, dualStack, 4)
		connect.AssertEqual(t, rows.ConsecutiveFailures, 0)
		connect.AssertEqual(t, rows.SuccessTime != nil, true)
	})
}

// An extender the sample leaves out is still probed, for a restart loop or for
// its probes, and so is one it cannot place: the probes test the api, and only
// a probe can bring an extender back.
func TestExtenderMeasurementProbeProbesWhatTheSampleLeavesOut(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestMeasurementProbeConfig(t)
		now := server.NowUtc()

		looping := createTestMeasurementProbeHost(t, ctx, 1, testMeasurementProbeServing, "192.0.2.1", "2001:db8::1")
		for _, ago := range []time.Duration{110 * time.Minute, 80 * time.Minute, 50 * time.Minute, 20 * time.Minute} {
			model.Testing_AddNetworkExtenderActivation(ctx, looping, 4, now.Add(-ago))
			model.Testing_AddNetworkExtenderActivation(ctx, looping, 6, now.Add(-ago).Add(2*time.Second))
		}
		failing := createTestMeasurementProbeHost(t, ctx, 2, testMeasurementProbeServing, "192.0.2.2")

		stubMeasurementProbes(t, func(target lagprobe.Target) *lagprobe.Result {
			if target.Addr.Addr() == netip.MustParseAddr("192.0.2.2") {
				return &lagprobe.Result{Outcome: lagprobe.OutcomeDial, Err: errors.New("refused")}
			}
			return &lagprobe.Result{Outcome: lagprobe.OutcomeOk}
		})
		for range 3 {
			connect.AssertEqual(t, runTestMeasurementProbe(t, ctx).Probed, 3)
		}
		connect.AssertEqual(t, getTestMeasurementProbeRow(ctx, failing, 4).ConsecutiveFailures, 3)
		connect.AssertEqual(t, getTestMeasurementProbeRow(ctx, looping, 4).Outcome, lagprobe.OutcomeOk)
		connect.AssertEqual(t, getTestMeasurementProbeRow(ctx, looping, 6).Outcome, lagprobe.OutcomeOk)
	})
}

// What a pass did not reach, for its budget or its end, stores nothing: its row
// keeps its last probe and its run, and ages out if the pass never reaches it.
func TestExtenderMeasurementProbeStoresNothingForWhatItDidNotReach(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestMeasurementProbeConfig(t)
		extenderId := createTestMeasurementProbeHost(t, ctx, 1, testMeasurementProbeServing, "192.0.2.1")

		stubMeasurementProbes(t, func(target lagprobe.Target) *lagprobe.Result {
			return &lagprobe.Result{Outcome: lagprobe.OutcomeDial, Err: errors.New("refused")}
		})
		runTestMeasurementProbe(t, ctx)
		before := getTestMeasurementProbeRow(ctx, extenderId, 4)
		connect.AssertEqual(t, before.ConsecutiveFailures, 1)

		stubMeasurementProbes(t, func(target lagprobe.Target) *lagprobe.Result {
			return nil
		})
		result := runTestMeasurementProbe(t, ctx)
		connect.AssertEqual(t, *result, ExtenderMeasurementProbeResult{Skipped: 1})
		after := getTestMeasurementProbeRow(ctx, extenderId, 4)
		connect.AssertEqual(t, after.ConsecutiveFailures, 1)
		connect.AssertEqual(t, after.ProbeTime, before.ProbeTime)
	})
}

// A family the pass took to be unreachable is stored as unprobed, which is what
// the pass says of it, and the task goes on.
func TestExtenderMeasurementProbeStoresAnUnreachableFamilyUnprobed(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestMeasurementProbeConfig(t)
		extenderId := createTestMeasurementProbeHost(t, ctx, 1, testMeasurementProbeServing, "192.0.2.1", "2001:db8::1")

		stubMeasurementProbes(t, func(target lagprobe.Target) *lagprobe.Result {
			if target.IpVersion() == 6 {
				return &lagprobe.Result{Outcome: lagprobe.OutcomeUnprobed, Err: errors.New("no ipv6")}
			}
			return &lagprobe.Result{Outcome: lagprobe.OutcomeDial, Err: errors.New("refused")}
		}, 6)
		result := runTestMeasurementProbe(t, ctx)
		connect.AssertEqual(t, *result, ExtenderMeasurementProbeResult{Probed: 2, Failed: 1, Unprobed: 1})
		connect.AssertEqual(t, getTestMeasurementProbeRow(ctx, extenderId, 6).Outcome, lagprobe.OutcomeUnprobed)
		connect.AssertEqual(t, getTestMeasurementProbeRow(ctx, extenderId, 4).Outcome, lagprobe.OutcomeDial)
	})
}

// With nothing replacing it, the chain makes the real pass. Whatever this host
// runs on its loopback's 443, if anything, it is not this name's measurement
// api, so the probe fails, and what it found is stored.
func TestExtenderMeasurementProbeRunsTheRealPass(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestMeasurementProbeConfig(t)
		extenderId := createTestMeasurementProbeHost(t, ctx, 1, testMeasurementProbeServing, "127.0.0.1")

		result := runTestMeasurementProbe(t, ctx)
		connect.AssertEqual(t, result.Probed, 1)
		connect.AssertEqual(t, result.Ok, 0)
		row := getTestMeasurementProbeRow(ctx, extenderId, 4)
		connect.AssertEqual(t, row.Outcome == lagprobe.OutcomeDial || row.Outcome == lagprobe.OutcomeTls || row.Outcome == lagprobe.OutcomeTlsTimeout, true)
		connect.AssertEqual(t, row.ConsecutiveFailures, 1)
	})
}

// A pass removes the rows of families past their retention, and leaves the rest.
func TestExtenderMeasurementProbeRemovesWhatIsPastItsRetention(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestMeasurementProbeConfig(t)
		now := server.NowUtc()
		gone := server.NewId()
		away := server.NewId()
		model.RecordNetworkExtenderMeasurementProbes(ctx, []*model.NetworkExtenderMeasurementProbeResult{
			{ExtenderId: gone, Ip: netip.MustParseAddr("192.0.2.8"), Outcome: lagprobe.OutcomeOk},
		}, now.Add(-ExtenderMeasurementProbeRetention-time.Hour), controller.ExtenderMeasurementProbeMaxAge)
		model.RecordNetworkExtenderMeasurementProbes(ctx, []*model.NetworkExtenderMeasurementProbeResult{
			{ExtenderId: away, Ip: netip.MustParseAddr("192.0.2.9"), Outcome: lagprobe.OutcomeOk},
		}, now.Add(-ExtenderMeasurementProbeRetention+time.Hour), controller.ExtenderMeasurementProbeMaxAge)
		stubMeasurementProbes(t, func(target lagprobe.Target) *lagprobe.Result {
			return &lagprobe.Result{Outcome: lagprobe.OutcomeOk}
		})

		result := runTestMeasurementProbe(t, ctx)
		connect.AssertEqual(t, result.Removed, 1)
		connect.AssertEqual(t, getTestMeasurementProbeRow(ctx, gone, 4), (*model.NetworkExtenderMeasurementProbe)(nil))
		connect.AssertEqual(t, getTestMeasurementProbeRow(ctx, away, 4) != nil, true)
	})
}

// With the measurement block off, or no extender network at all, nothing is
// probed, and the chain still re-arms.
func TestExtenderMeasurementProbeIsInertWhenDisabled(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		passes := stubMeasurementProbes(t, func(target lagprobe.Target) *lagprobe.Result {
			t.Errorf("probed %s", target.Addr)
			return nil
		})

		// no extender network
		controller.Testing_ResetExtenderConfig()
		connect.AssertEqual(t, *runTestMeasurementProbe(t, ctx), ExtenderMeasurementProbeResult{})

		// the block off
		installTestExtenderWorkConfig(t,
			"measurement:",
			"  enabled: false",
			"  zone_name: "+testMeasurementProbeZone,
		)
		createTestMeasurementProbeHost(t, ctx, 1, testMeasurementProbeServing, "192.0.2.1")
		connect.AssertEqual(t, *runTestMeasurementProbe(t, ctx), ExtenderMeasurementProbeResult{})
		connect.AssertEqual(t, len(*passes), 0)
	})
}

// The post schedules the next pass two minutes on, one per chain, under the
// default limit, so the task claims as ordinary work.
func TestExtenderMeasurementProbePostRearmsTheChain(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()

		before := server.NowUtc()
		for range 2 {
			server.Tx(ctx, func(tx server.PgTx) {
				connect.AssertEqual(t, ExtenderMeasurementProbePost(
					&ExtenderMeasurementProbeArgs{},
					&ExtenderMeasurementProbeResult{Failed: 3},
					clientSession,
					tx,
				), nil)
			})
		}
		after := server.NowUtc()

		var count int
		var runAt time.Time
		var maxTimeSeconds int
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
				SELECT run_at, run_max_time_seconds
				FROM pending_task
				WHERE run_once_key = '["extender_measurement_probe"]'
				`,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					count += 1
					server.Raise(result.Scan(&runAt, &maxTimeSeconds))
				}
			})
		})
		connect.AssertEqual(t, count, 1)
		runAt = runAt.UTC()
		if runAt.Before(before.Add(ExtenderMeasurementProbeTimeout).Truncate(time.Second)) || after.Add(ExtenderMeasurementProbeTimeout+2*time.Second).Before(runAt) {
			t.Fatalf("armed for %s, want two minutes after %s", runAt, before)
		}
		connect.AssertEqual(t, maxTimeSeconds, 120)
	})
}
