package work

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/lagprobe"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
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

// One pass or recheck as the stub saw it.
type testMeasurementProbePass struct {
	targets  []lagprobe.Target
	recheck  bool
	settings lagprobe.Settings
}

// Replaces the pass and the recheck with stubs that answer each target as told,
// and records what they were asked. Only a pass can report an unreachable family.
func stubMeasurementProbes(t testing.TB, answer func(target lagprobe.Target) *lagprobe.Result, unreachable ...int) *[]*testMeasurementProbePass {
	t.Helper()
	passes := &[]*testMeasurementProbePass{}
	previous := probeExtenderMeasurementApis
	previousRecheck := recheckExtenderMeasurementApis
	answerAll := func(targets []lagprobe.Target, settings lagprobe.Settings, recheck bool) []*lagprobe.Result {
		*passes = append(*passes, &testMeasurementProbePass{targets: targets, recheck: recheck, settings: settings})
		results := make([]*lagprobe.Result, len(targets))
		for i, target := range targets {
			results[i] = answer(target)
		}
		return results
	}
	probeExtenderMeasurementApis = func(ctx context.Context, targets []lagprobe.Target, settings lagprobe.Settings) *lagprobe.Pass {
		return &lagprobe.Pass{Results: answerAll(targets, settings, false), UnreachableIpVersions: unreachable}
	}
	recheckExtenderMeasurementApis = func(ctx context.Context, targets []lagprobe.Target, settings lagprobe.Settings) *lagprobe.Pass {
		return &lagprobe.Pass{Results: answerAll(targets, settings, true)}
	}
	t.Cleanup(func() {
		probeExtenderMeasurementApis = previous
		recheckExtenderMeasurementApis = previousRecheck
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

// A result's counts, without the pass's time.
func testMeasurementProbeCounts(result *ExtenderMeasurementProbeResult) ExtenderMeasurementProbeResult {
	counts := *result
	counts.PassTime = time.Time{}
	return counts
}

func runTestMeasurementRecheck(t testing.TB, ctx context.Context, passTime time.Time) *ExtenderMeasurementProbeResult {
	t.Helper()
	clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
	defer clientSession.Cancel()
	result, err := ExtenderMeasurementRecheck(&ExtenderMeasurementRecheckArgs{PassTime: passTime}, clientSession)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// One outcome per address, by the address's text; an address not named passes.
func answerTestMeasurementProbes(outcomes map[string]lagprobe.Outcome) func(target lagprobe.Target) *lagprobe.Result {
	return func(target lagprobe.Target) *lagprobe.Result {
		outcome, ok := outcomes[target.Addr.Addr().String()]
		if !ok || outcome == lagprobe.OutcomeOk {
			return &lagprobe.Result{Outcome: lagprobe.OutcomeOk}
		}
		return &lagprobe.Result{Outcome: outcome, Err: errors.New(string(outcome))}
	}
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

	// the second look comes well before the next pass would have looked
	connect.AssertEqual(t, 2*ExtenderMeasurementRecheckTimeout < ExtenderMeasurementProbeTimeout, true)
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
		connect.AssertEqual(t, testMeasurementProbeCounts(result), ExtenderMeasurementProbeResult{Probed: 4, Ok: 1, Failed: 2, Unprobed: 1})
		connect.AssertEqual(t, result.PassTime.IsZero(), false)

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
		connect.AssertEqual(t, testMeasurementProbeCounts(result), ExtenderMeasurementProbeResult{Probed: 4, Ok: 1, Failed: 2, Unprobed: 1})
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
		connect.AssertEqual(t, testMeasurementProbeCounts(result), ExtenderMeasurementProbeResult{Skipped: 1})
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
		tripsV4 := testutil.ToFloat64(extenderMeasurementProbeGuardTripsTotal.WithLabelValues("ipv4"))
		tripsV6 := testutil.ToFloat64(extenderMeasurementProbeGuardTripsTotal.WithLabelValues("ipv6"))
		result := runTestMeasurementProbe(t, ctx)
		connect.AssertEqual(t, testMeasurementProbeCounts(result), ExtenderMeasurementProbeResult{Probed: 2, Failed: 1, Unprobed: 1})
		connect.AssertEqual(t, getTestMeasurementProbeRow(ctx, extenderId, 6).Outcome, lagprobe.OutcomeUnprobed)
		connect.AssertEqual(t, getTestMeasurementProbeRow(ctx, extenderId, 4).Outcome, lagprobe.OutcomeDial)
		// the trip is counted, since the extenders are otherwise offered unremarked
		connect.AssertEqual(t, testutil.ToFloat64(extenderMeasurementProbeGuardTripsTotal.WithLabelValues("ipv6")), tripsV6+1)
		connect.AssertEqual(t, testutil.ToFloat64(extenderMeasurementProbeGuardTripsTotal.WithLabelValues("ipv4")), tripsV4)
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

// The recheck's task row: when it is due, what it was asked to probe again, and
// its limit.
type testMeasurementRecheckRow struct {
	taskId         server.Id
	runAt          time.Time
	args           ExtenderMeasurementRecheckArgs
	maxTimeSeconds int
}

func getTestMeasurementRecheckRows(t testing.TB, ctx context.Context) []*testMeasurementRecheckRow {
	t.Helper()
	rows := []*testMeasurementRecheckRow{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT task_id, run_at, args_json, run_max_time_seconds
			FROM pending_task
			WHERE function_name = 'github.com/urnetwork/server/taskworker/work.ExtenderMeasurementRecheck'
			ORDER BY run_at
			`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				row := &testMeasurementRecheckRow{}
				var argsJson string
				server.Raise(result.Scan(&row.taskId, &row.runAt, &argsJson, &row.maxTimeSeconds))
				server.Raise(json.Unmarshal([]byte(argsJson), &row.args))
				row.runAt = row.runAt.UTC()
				rows = append(rows, row)
			}
		})
	})
	return rows
}

// A pass with a failure has it probed again about 30 s on, under the default
// limit, so the second look claims as ordinary work. A pass with none has
// nothing to look at again, and neither has one whose only trouble is what it
// could not probe: a family this host cannot reach gets no second failure.
func TestExtenderMeasurementProbePostSchedulesTheRecheck(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		post := func(result *ExtenderMeasurementProbeResult) {
			server.Tx(ctx, func(tx server.PgTx) {
				connect.AssertEqual(t, ExtenderMeasurementProbePost(&ExtenderMeasurementProbeArgs{}, result, clientSession, tx), nil)
			})
		}

		passTime := server.NowUtc().Truncate(time.Microsecond)
		post(&ExtenderMeasurementProbeResult{PassTime: passTime, Probed: 9, Ok: 9})
		post(&ExtenderMeasurementProbeResult{PassTime: passTime, Probed: 9, Ok: 4, Unprobed: 5})
		post(&ExtenderMeasurementProbeResult{})
		connect.AssertEqual(t, len(getTestMeasurementRecheckRows(t, ctx)), 0)

		before := server.NowUtc()
		post(&ExtenderMeasurementProbeResult{PassTime: passTime, Probed: 9, Ok: 8, Failed: 1})
		after := server.NowUtc()
		rows := getTestMeasurementRecheckRows(t, ctx)
		connect.AssertEqual(t, len(rows), 1)
		connect.AssertEqual(t, ExtenderMeasurementRecheckTimeout, 30*time.Second)
		if rows[0].runAt.Before(before.Add(ExtenderMeasurementRecheckTimeout).Truncate(time.Second)) || after.Add(ExtenderMeasurementRecheckTimeout+2*time.Second).Before(rows[0].runAt) {
			t.Fatalf("the recheck is due at %s, want 30 s after %s", rows[0].runAt, before)
		}
		connect.AssertEqual(t, rows[0].args.PassTime.Equal(passTime), true)
		connect.AssertEqual(t, rows[0].maxTimeSeconds, 120)

		// the post of one pass run again asks for the same recheck
		post(&ExtenderMeasurementProbeResult{PassTime: passTime, Probed: 9, Ok: 8, Failed: 1})
		connect.AssertEqual(t, len(getTestMeasurementRecheckRows(t, ctx)), 1)

		// the next pass's failures have their own, whatever is still waiting
		nextPassTime := passTime.Add(ExtenderMeasurementProbeTimeout)
		post(&ExtenderMeasurementProbeResult{PassTime: nextPassTime, Probed: 9, Ok: 8, Failed: 1})
		rows = getTestMeasurementRecheckRows(t, ctx)
		connect.AssertEqual(t, len(rows), 2)
		connect.AssertEqual(t, rows[1].args.PassTime.Equal(nextPassTime), true)
	})
}

// The second look probes exactly what failed in the pass, at the name and port it
// was probed at, and with no family rule (lagprobe.Recheck). It counts as a probe
// like any other: a failure makes two in a row, and a success clears the one. What
// passed, and what could not be probed, is left as the pass stored it.
func TestExtenderMeasurementRecheckProbesWhatFailed(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestMeasurementProbeConfig(t)

		stillFailing := createTestMeasurementProbeHost(t, ctx, 1, testMeasurementProbeServing, "192.0.2.1", "2001:db8::1")
		recovered := createTestMeasurementProbeHost(t, ctx, 2, testMeasurementProbeServing, "192.0.2.2")
		healthy := createTestMeasurementProbeHost(t, ctx, 3, testMeasurementProbeServing, "192.0.2.3")
		unprobed := createTestMeasurementProbeHost(t, ctx, 4, testMeasurementProbeServing, "2001:db8::4")

		stubMeasurementProbes(t, answerTestMeasurementProbes(map[string]lagprobe.Outcome{
			"192.0.2.1":   lagprobe.OutcomeTlsTimeout,
			"192.0.2.2":   lagprobe.OutcomeDial,
			"2001:db8::4": lagprobe.OutcomeUnprobed,
		}))
		pass := runTestMeasurementProbe(t, ctx)
		connect.AssertEqual(t, testMeasurementProbeCounts(pass), ExtenderMeasurementProbeResult{Probed: 5, Ok: 2, Failed: 2, Unprobed: 1})
		healthyRow := getTestMeasurementProbeRow(ctx, healthy, 4)
		unprobedRow := getTestMeasurementProbeRow(ctx, unprobed, 6)
		passingFamilyRow := getTestMeasurementProbeRow(ctx, stillFailing, 6)

		passes := stubMeasurementProbes(t, answerTestMeasurementProbes(map[string]lagprobe.Outcome{
			"192.0.2.1": lagprobe.OutcomeTlsTimeout,
		}))
		result := runTestMeasurementRecheck(t, ctx, pass.PassTime)
		connect.AssertEqual(t, *result, ExtenderMeasurementProbeResult{Probed: 2, Ok: 1, Failed: 1})

		connect.AssertEqual(t, len(*passes), 1)
		got := []string{}
		for _, target := range (*passes)[0].targets {
			got = append(got, fmt.Sprintf("%s %s", target.Name, target.Addr))
		}
		slices.Sort(got)
		want := []string{
			controller.ExtenderMeasurementName(stillFailing, testMeasurementProbeZone) + " 192.0.2.1:443",
			controller.ExtenderMeasurementName(recovered, testMeasurementProbeZone) + " 192.0.2.2:443",
		}
		slices.Sort(want)
		connect.AssertEqual(t, got, want)
		connect.AssertEqual(t, (*passes)[0].recheck, true)
		connect.AssertEqual(t, (*passes)[0].settings.Origin, lagprobe.DefaultSettings().Origin)

		row := getTestMeasurementProbeRow(ctx, stillFailing, 4)
		connect.AssertEqual(t, row.ConsecutiveFailures, 2)
		connect.AssertEqual(t, row.Outcome, lagprobe.OutcomeTlsTimeout)
		if !pass.PassTime.Before(row.ProbeTime) {
			t.Fatalf("the second probe is stored at %s, the pass at %s", row.ProbeTime, pass.PassTime)
		}
		row = getTestMeasurementProbeRow(ctx, recovered, 4)
		connect.AssertEqual(t, row.ConsecutiveFailures, 0)
		connect.AssertEqual(t, row.Outcome, lagprobe.OutcomeOk)
		connect.AssertEqual(t, row.SuccessTime != nil, true)

		connect.AssertEqual(t, getTestMeasurementProbeRow(ctx, healthy, 4), healthyRow)
		connect.AssertEqual(t, getTestMeasurementProbeRow(ctx, unprobed, 6), unprobedRow)
		connect.AssertEqual(t, getTestMeasurementProbeRow(ctx, stillFailing, 6), passingFamilyRow)

		// what failed in the second look waits for the next pass: asked again,
		// the same pass has nothing left to look at, the rows having moved on
		connect.AssertEqual(t, *runTestMeasurementRecheck(t, ctx, pass.PassTime), ExtenderMeasurementProbeResult{})
		connect.AssertEqual(t, len(*passes), 1)
		connect.AssertEqual(t, getTestMeasurementProbeRow(ctx, stillFailing, 4).ConsecutiveFailures, 2)
	})
}

// A second look that runs after the next pass has replaced what it was to look
// at does nothing: it must not probe that pass's failures seconds after they
// failed, which is no second look at all. A pass with no failure leaves nothing
// to look at either.
func TestExtenderMeasurementRecheckOfAnOverwrittenPassDoesNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestMeasurementProbeConfig(t)
		extenderId := createTestMeasurementProbeHost(t, ctx, 1, testMeasurementProbeServing, "192.0.2.1")

		stubMeasurementProbes(t, answerTestMeasurementProbes(map[string]lagprobe.Outcome{"192.0.2.1": lagprobe.OutcomeDial}))
		first := runTestMeasurementProbe(t, ctx)
		second := runTestMeasurementProbe(t, ctx)
		connect.AssertEqual(t, getTestMeasurementProbeRow(ctx, extenderId, 4).ConsecutiveFailures, 2)

		passes := stubMeasurementProbes(t, func(target lagprobe.Target) *lagprobe.Result {
			t.Errorf("probed %s", target.Addr)
			return nil
		})
		connect.AssertEqual(t, *runTestMeasurementRecheck(t, ctx, first.PassTime), ExtenderMeasurementProbeResult{})
		connect.AssertEqual(t, len(*passes), 0)
		connect.AssertEqual(t, getTestMeasurementProbeRow(ctx, extenderId, 4).ProbeTime.UTC(), second.PassTime)
		connect.AssertEqual(t, getTestMeasurementProbeRow(ctx, extenderId, 4).ConsecutiveFailures, 2)

		// a pass that found nothing wrong
		stubMeasurementProbes(t, answerTestMeasurementProbes(nil))
		clean := runTestMeasurementProbe(t, ctx)
		passes = stubMeasurementProbes(t, func(target lagprobe.Target) *lagprobe.Result {
			t.Errorf("probed %s", target.Addr)
			return nil
		})
		connect.AssertEqual(t, *runTestMeasurementRecheck(t, ctx, clean.PassTime), ExtenderMeasurementProbeResult{})
		connect.AssertEqual(t, len(*passes), 0)
	})
}

// With the measurement block off, a second look already scheduled probes nothing.
func TestExtenderMeasurementRecheckIsInertWhenDisabled(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestMeasurementProbeConfig(t)
		createTestMeasurementProbeHost(t, ctx, 1, testMeasurementProbeServing, "192.0.2.1")
		stubMeasurementProbes(t, answerTestMeasurementProbes(map[string]lagprobe.Outcome{"192.0.2.1": lagprobe.OutcomeDial}))
		pass := runTestMeasurementProbe(t, ctx)

		installTestExtenderWorkConfig(t,
			"measurement:",
			"  enabled: false",
			"  zone_name: "+testMeasurementProbeZone,
		)
		passes := stubMeasurementProbes(t, func(target lagprobe.Target) *lagprobe.Result {
			t.Errorf("probed %s", target.Addr)
			return nil
		})
		connect.AssertEqual(t, *runTestMeasurementRecheck(t, ctx, pass.PassTime), ExtenderMeasurementProbeResult{})
		connect.AssertEqual(t, len(*passes), 0)
	})
}

// Moves a pending task's run back so a worker may claim it now, rather than the
// test sleeping for it.
func dueTestMeasurementTask(t testing.TB, ctx context.Context, taskId server.Id) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE pending_task SET run_at = $2 WHERE task_id = $1`,
			taskId,
			time.Unix(1, 0).UTC(),
		))
	})
}

// A worker with the chain's two targets, as the taskworker registers them.
func newTestMeasurementProbeWorker(t testing.TB, ctx context.Context) *task.TaskWorker {
	t.Helper()
	settings := task.DefaultTaskWorkerSettings()
	settings.ClaimRegisteredTargetsOnly = true
	worker := task.NewTaskWorker(ctx, settings)
	t.Cleanup(worker.Close)
	worker.AddTargets(
		task.NewTaskTargetWithPost(ExtenderMeasurementProbe, ExtenderMeasurementProbePost),
		task.NewTaskTarget(ExtenderMeasurementRecheck),
	)
	return worker
}

// The whole of it through a real worker: a pass that fails leaves the second
// look waiting and not yet claimable, so the probe comes about 30 s later and not
// at once; it is the time in the task's own args that finds the pass; and it
// asks for no third look.
func TestExtenderMeasurementRecheckRunsAfterItsDelay(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestMeasurementProbeConfig(t)
		extenderId := createTestMeasurementProbeHost(t, ctx, 1, testMeasurementProbeServing, "192.0.2.1")
		createTestMeasurementProbeHost(t, ctx, 2, testMeasurementProbeServing, "192.0.2.2")
		passes := stubMeasurementProbes(t, answerTestMeasurementProbes(map[string]lagprobe.Outcome{"192.0.2.1": lagprobe.OutcomeTlsTimeout}))
		worker := newTestMeasurementProbeWorker(t, ctx)

		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		server.Tx(ctx, func(tx server.PgTx) {
			ScheduleExtenderMeasurementProbe(clientSession, tx)
		})
		var chainTaskId server.Id
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT task_id FROM pending_task WHERE run_once_key = '["extender_measurement_probe"]'`,
			).Scan(&chainTaskId))
		})
		dueTestMeasurementTask(t, ctx, chainTaskId)
		finished, retried, posts, err := worker.EvalTasks(1)
		if err != nil || len(finished) != 1 || finished[0] != chainTaskId || 0 < len(retried)+len(posts) {
			t.Fatalf("the pass did not run: %v %v %v %v", finished, retried, posts, err)
		}
		connect.AssertEqual(t, len(*passes), 1)
		connect.AssertEqual(t, len((*passes)[0].targets), 2)
		first := getTestMeasurementProbeRow(ctx, extenderId, 4)
		connect.AssertEqual(t, first.ConsecutiveFailures, 1)

		// waiting: nothing is claimable until its 30 s are up
		rows := getTestMeasurementRecheckRows(t, ctx)
		connect.AssertEqual(t, len(rows), 1)
		if rows[0].runAt.Before(first.ProbeTime.UTC().Add(ExtenderMeasurementRecheckTimeout)) {
			t.Fatalf("the second look is due at %s, under 30 s after the pass at %s", rows[0].runAt, first.ProbeTime)
		}
		finished, retried, posts, err = worker.EvalTasks(1)
		if err != nil || 0 < len(finished)+len(retried)+len(posts) {
			t.Fatalf("a task ran before its time: %v %v %v %v", finished, retried, posts, err)
		}
		connect.AssertEqual(t, len(*passes), 1)
		connect.AssertEqual(t, getTestMeasurementProbeRow(ctx, extenderId, 4).ConsecutiveFailures, 1)

		// due: it probes the one address again, which makes two in a row
		dueTestMeasurementTask(t, ctx, rows[0].taskId)
		finished, retried, posts, err = worker.EvalTasks(1)
		if err != nil || len(finished) != 1 || finished[0] != rows[0].taskId || 0 < len(retried)+len(posts) {
			t.Fatalf("the second look did not run: %v %v %v %v", finished, retried, posts, err)
		}
		connect.AssertEqual(t, len(*passes), 2)
		connect.AssertEqual(t, len((*passes)[1].targets), 1)
		connect.AssertEqual(t, (*passes)[1].targets[0].Addr, netip.MustParseAddrPort("192.0.2.1:443"))
		connect.AssertEqual(t, getTestMeasurementProbeRow(ctx, extenderId, 4).ConsecutiveFailures, 2)

		// and no third
		connect.AssertEqual(t, len(getTestMeasurementRecheckRows(t, ctx)), 0)
	})
}

// A guest session and the sample's extender ids, for what the rule reads of the
// chain's probes (controller.LagExtenders).
func testMeasurementProbeOffered(t testing.TB, ctx context.Context) []server.Id {
	t.Helper()
	byJwt := session.NewByJwt(server.NewId(), server.NewId(), "probe-test-guest", false, false).
		Client(server.NewId(), server.NewId())
	clientSession := session.NewLocalClientSession(ctx, "198.51.100.30:1234", byJwt)
	defer clientSession.Cancel()
	controller.Testing_ResetLagServing()
	result, err := controller.LagExtenders(clientSession)
	if err != nil {
		t.Fatal(err)
	}
	extenderIds := []server.Id{}
	for _, extender := range result.Extenders {
		extenderIds = append(extenderIds, extender.ExtenderId)
	}
	return extenderIds
}

// What the sample does as the chain runs. One failure leaves an extender offered,
// the second look's failure takes it out, within the 30 s or so between them, and
// it comes back with the next success. A second look that passes leaves it
// offered throughout, so a stutter never reaches the sample.
func TestExtenderMeasurementRecheckMovesTheSample(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestMeasurementProbeConfig(t)
		defer controller.Testing_SetLagSettings(controller.DefaultLagSettings())()
		restorePlaces := model.Testing_ReplaceLagAddressPlaces(func(ip netip.Addr) *model.LagAddressPlace {
			return &model.LagAddressPlace{
				Place:            &model.LagPlace{City: "Paris", CountryCode: "fr", CountryName: "France", Latitude: 48.9, Longitude: 2.4},
				AccuracyRadiusKm: 20,
			}
		})
		defer restorePlaces()
		t.Cleanup(controller.Testing_ResetLagServing)

		extenderId := createTestMeasurementProbeHost(t, ctx, 1, testMeasurementProbeServing, "192.0.2.1", "2001:db8::1")
		other := createTestMeasurementProbeHost(t, ctx, 2, testMeasurementProbeServing, "192.0.2.2")
		both := slices.Sorted(slices.Values([]string{extenderId.String(), other.String()}))
		offered := func() []string {
			ids := []string{}
			for _, id := range testMeasurementProbeOffered(t, ctx) {
				ids = append(ids, id.String())
			}
			slices.Sort(ids)
			return ids
		}
		failing := map[string]lagprobe.Outcome{
			"192.0.2.1":   lagprobe.OutcomeTlsTimeout,
			"2001:db8::1": lagprobe.OutcomeTlsTimeout,
		}
		connect.AssertEqual(t, offered(), both)

		// the first failure, on both families
		stubMeasurementProbes(t, answerTestMeasurementProbes(failing))
		pass := runTestMeasurementProbe(t, ctx)
		connect.AssertEqual(t, pass.Failed, 2)
		connect.AssertEqual(t, offered(), both)

		// the second, with no pass between them
		runTestMeasurementRecheck(t, ctx, pass.PassTime)
		connect.AssertEqual(t, getTestMeasurementProbeRow(ctx, extenderId, 4).ConsecutiveFailures, 2)
		connect.AssertEqual(t, offered(), []string{other.String()})

		// the next pass's success on one family brings it back
		stubMeasurementProbes(t, answerTestMeasurementProbes(map[string]lagprobe.Outcome{"192.0.2.1": lagprobe.OutcomeTlsTimeout}))
		pass = runTestMeasurementProbe(t, ctx)
		connect.AssertEqual(t, offered(), both)

		// a stutter: the second look passes, and nothing was ever left out
		stubMeasurementProbes(t, answerTestMeasurementProbes(failing))
		pass = runTestMeasurementProbe(t, ctx)
		connect.AssertEqual(t, offered(), both)
		stubMeasurementProbes(t, answerTestMeasurementProbes(nil))
		runTestMeasurementRecheck(t, ctx, pass.PassTime)
		connect.AssertEqual(t, getTestMeasurementProbeRow(ctx, extenderId, 4).ConsecutiveFailures, 0)
		connect.AssertEqual(t, getTestMeasurementProbeRow(ctx, extenderId, 6).ConsecutiveFailures, 0)
		connect.AssertEqual(t, offered(), both)
	})
}

// A pass runs again when its task raises, so it must not raise once it has stored
// its failures: the same stutter would count as a second failure in a row seconds
// later, with no recheck between them, and take the extender out. So the retention,
// which can raise, comes first. Here it raises, the pass stores nothing, and the
// run that follows counts the stutter once.
func TestExtenderMeasurementProbeRetriedPassCountsOneFailure(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestMeasurementProbeConfig(t)
		extenderId := createTestMeasurementProbeHost(t, ctx, 1, testMeasurementProbeServing, "192.0.2.1")
		createTestMeasurementProbeHost(t, ctx, 2, testMeasurementProbeServing, "192.0.2.2")
		passes := stubMeasurementProbes(t, answerTestMeasurementProbes(map[string]lagprobe.Outcome{"192.0.2.1": lagprobe.OutcomeTlsTimeout}))
		worker := newTestMeasurementProbeWorker(t, ctx)

		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
				CREATE FUNCTION test_measurement_probe_retention_raises() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN
					RAISE EXCEPTION 'the retention failed';
				END
				$$
			`))
			server.RaisePgResult(tx.Exec(ctx, `
				CREATE TRIGGER test_measurement_probe_retention_raises
				BEFORE DELETE ON network_extender_measurement_probe
				FOR EACH STATEMENT EXECUTE FUNCTION test_measurement_probe_retention_raises()
			`))
		})

		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		server.Tx(ctx, func(tx server.PgTx) {
			ScheduleExtenderMeasurementProbe(clientSession, tx)
		})
		chainTaskId := func() (taskId server.Id) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(
					ctx,
					`SELECT task_id FROM pending_task WHERE run_once_key = '["extender_measurement_probe"]'`,
				).Scan(&taskId))
			})
			return
		}

		// the retention raises: nothing was probed or stored, and it will run again
		dueTestMeasurementTask(t, ctx, chainTaskId())
		finished, retried, _, err := worker.EvalTasks(1)
		if err != nil || len(finished) != 0 || len(retried) != 1 {
			t.Fatalf("the pass should have failed and been rescheduled: %v %v %v", finished, retried, err)
		}
		connect.AssertEqual(t, len(*passes), 0)
		connect.AssertEqual(t, getTestMeasurementProbeRow(ctx, extenderId, 4), (*model.NetworkExtenderMeasurementProbe)(nil))
		connect.AssertEqual(t, len(getTestMeasurementRecheckRows(t, ctx)), 0)

		// it runs again, with the retention working: one failure, and its recheck
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER test_measurement_probe_retention_raises ON network_extender_measurement_probe`))
		})
		// its claim outlives the failure, and its retry waits on a backoff; the test
		// clears both rather than sleeping
		connect.AssertEqual(t, task.ReleaseTask(ctx, chainTaskId()), true)
		dueTestMeasurementTask(t, ctx, chainTaskId())
		finished, retried, posts, err := worker.EvalTasks(1)
		if err != nil || len(finished) != 1 || 0 < len(retried)+len(posts) {
			t.Fatalf("the pass did not run again: %v %v %v %v", finished, retried, posts, err)
		}
		connect.AssertEqual(t, len(*passes), 1)
		connect.AssertEqual(t, getTestMeasurementProbeRow(ctx, extenderId, 4).ConsecutiveFailures, 1)
		connect.AssertEqual(t, len(getTestMeasurementRecheckRows(t, ctx)), 1)
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
