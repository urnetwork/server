package controller

import (
	"context"
	"errors"
	"math"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The run of the extender process on each activation (connect/EXTENDER.md G3).

const testExtenderRunId = "00112233445566778899aabbccddeeff"

// A documentation address for the activations the gauge test stores.
var testExtenderRunAddr = netip.MustParseAddr("192.0.2.70")

// A run id is kept as 32 lower case hex characters of 16 bytes, and anything
// else is dropped.
func TestExtenderActivationRunKeepsOnlyAWellFormedId(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		runId    string
		expected string
	}{
		{runId: testExtenderRunId, expected: testExtenderRunId},
		{runId: strings.ToUpper(testExtenderRunId), expected: testExtenderRunId},
		{runId: "", expected: ""},
		// one byte short and one byte long
		{runId: testExtenderRunId[:30], expected: ""},
		{runId: testExtenderRunId + "00", expected: ""},
		// an odd length, and the right length with a character that is not hex
		{runId: testExtenderRunId[:31], expected: ""},
		{runId: "g" + testExtenderRunId[1:], expected: ""},
		{runId: "0x" + testExtenderRunId[2:], expected: ""},
		// a uuid is 128 bits too, but not the form the field names
		{runId: "00112233-4455-6677-8899-aabbccddeeff", expected: ""},
	} {
		runId, _, _ := extenderActivationRun(&ExtenderActivateArgs{RunId: c.runId}, now)
		if c.expected == "" {
			if runId != nil {
				t.Errorf("run id %q was kept as %q", c.runId, *runId)
			}
			continue
		}
		if runId == nil || *runId != c.expected {
			t.Errorf("run id %q = %v, expected %q", c.runId, runId, c.expected)
		}
	}
}

// A run start is kept in utc when it is no earlier than the day the field was
// added and no later than an hour past the operator's clock.
func TestExtenderActivationRunKeepsOnlyASaneStart(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name      string
		startTime time.Time
		kept      bool
	}{
		{name: "just started", startTime: now.Add(-time.Second), kept: true},
		// connect posts all of its clock's digits
		{name: "with nanoseconds", startTime: now.Add(-time.Second - 123456789*time.Nanosecond), kept: true},
		{name: "the day the field was added", startTime: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), kept: true},
		{name: "an hour ahead of the operator", startTime: now.Add(time.Hour), kept: true},
		{name: "past the skew", startTime: now.Add(time.Hour + time.Second), kept: false},
		{name: "before the field existed", startTime: time.Date(2026, 10, 31, 23, 59, 59, 0, time.UTC), kept: false},
		// a host without a battery-backed clock, before its clock is set
		{name: "the epoch", startTime: time.Unix(0, 0).UTC(), kept: false},
		{name: "the zero time", startTime: time.Time{}, kept: false},
		// the same instant as now, written in another zone
		{name: "another zone", startTime: now.In(time.FixedZone("test", 5*60*60)), kept: true},
	} {
		_, runStartTime, _ := extenderActivationRun(&ExtenderActivateArgs{RunStartTime: c.startTime.Format(time.RFC3339Nano)}, now)
		if !c.kept {
			if runStartTime != nil {
				t.Errorf("%s: start %s was kept", c.name, c.startTime)
			}
			continue
		}
		if runStartTime == nil {
			t.Errorf("%s: start %s was dropped", c.name, c.startTime)
			continue
		}
		if !runStartTime.Equal(c.startTime) || runStartTime.Location() != time.UTC {
			t.Errorf("%s: start = %s, expected %s in utc", c.name, runStartTime, c.startTime)
		}
	}

	// no start is no start, and a string that is no time is none either
	for _, startTime := range []string{"", "yesterday", "2026-11-02", "1793620800"} {
		_, runStartTime, _ := extenderActivationRun(&ExtenderActivateArgs{RunStartTime: startTime}, now)
		if runStartTime != nil {
			t.Errorf("start %q was stored as %s", startTime, runStartTime)
		}
	}
}

// A previous exit is kept only as one of the operator's words, or exit-<code>
// for a code from 0 to 255.
func TestExtenderActivationRunKeepsOnlyAKnownPreviousExit(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		previousExit string
		expected     string
	}{
		{previousExit: "oom", expected: "oom"},
		{previousExit: "stop", expected: "stop"},
		{previousExit: "signal", expected: "signal"},
		{previousExit: "sigkill", expected: "sigkill"},
		{previousExit: "sigterm", expected: "sigterm"},
		{previousExit: "sigsegv", expected: "sigsegv"},
		{previousExit: " SIGKILL\n", expected: "sigkill"},
		{previousExit: "exit-0", expected: "exit-0"},
		{previousExit: "exit-2", expected: "exit-2"},
		{previousExit: "exit-137", expected: "exit-137"},
		{previousExit: "exit-255", expected: "exit-255"},
		{previousExit: "", expected: ""},
		{previousExit: "exit-256", expected: ""},
		{previousExit: "exit-1000", expected: ""},
		{previousExit: "exit-", expected: ""},
		{previousExit: "exit-01", expected: ""},
		{previousExit: "exit--1", expected: ""},
		{previousExit: "exit-+5", expected: ""},
		{previousExit: "exit-2a", expected: ""},
		{previousExit: "exit", expected: ""},
		{previousExit: "sigfoo", expected: ""},
		{previousExit: "killed by the kernel for memory", expected: ""},
		{previousExit: "sigkill; drop table", expected: ""},
	} {
		_, _, previousExit := extenderActivationRun(&ExtenderActivateArgs{PreviousExit: c.previousExit}, now)
		if c.expected == "" {
			if previousExit != nil {
				t.Errorf("previous exit %q was kept as %q", c.previousExit, *previousExit)
			}
			continue
		}
		if previousExit == nil || *previousExit != c.expected {
			t.Errorf("previous exit %q = %v, expected %q", c.previousExit, previousExit, c.expected)
		}
		// every word the operator keeps fits the column
		if 16 < len(*previousExit) {
			t.Errorf("previous exit %q is wider than its column", *previousExit)
		}
	}
	for word := range extenderActivationPreviousExits {
		if 16 < len(word) {
			t.Errorf("previous exit %q is wider than its column", word)
		}
	}
}

// The fields are judged one by one, so one that is malformed costs only itself.
func TestExtenderActivationRunJudgesEachFieldOnItsOwn(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	wrongClock := now.Add(24 * time.Hour)
	runId, runStartTime, previousExit := extenderActivationRun(&ExtenderActivateArgs{
		RunId:        testExtenderRunId,
		RunStartTime: wrongClock.Format(time.RFC3339),
		PreviousExit: "sigkill",
	}, now)
	if runId == nil || *runId != testExtenderRunId {
		t.Fatalf("the well formed id was lost beside a wrong clock: %v", runId)
	}
	if runStartTime != nil {
		t.Fatalf("the wrong clock was kept: %s", runStartTime)
	}
	if previousExit == nil || *previousExit != "sigkill" {
		t.Fatalf("the well formed previous exit was lost beside a wrong clock: %v", previousExit)
	}
}

// The run an activation posts is stored on its history row, and fields that are
// malformed are stored as null without costing the extender its activation: a
// new run on each process start is what makes a restart visible.
func TestExtenderActivateStoresTheRun(t *testing.T) {
	useGeolocationThatPlacesNothing(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newTestExtenderFixture(t)
		installTestExtenderConfig(t, fixture.api)
		clientSession := newTestExtenderSession(t, ctx, fixture.clientAddress("127.0.0.1"))

		// microseconds, the precision of the column
		runStartTime := server.NowUtc().Add(-time.Minute).Truncate(time.Microsecond)
		args := fixture.activateArgs()
		args.RunId = testExtenderRunId
		args.RunStartTime = runStartTime.Format(time.RFC3339Nano)
		args.PreviousExit = "sigkill"
		result, err := ExtenderActivate(args, clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Activated {
			t.Fatalf("the activation with a run was refused: %s", result.Error)
		}

		// a process whose fields are all malformed still activates
		args = fixture.activateArgs()
		args.RunId = "not a run id"
		args.RunStartTime = "not a time"
		args.PreviousExit = "a word the operator does not know"
		result, err = ExtenderActivate(args, clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Activated {
			t.Fatalf("a malformed run refused the activation: %s", result.Error)
		}

		extenderId := testExtenderIdForKey(ctx, t, fixture.publicKey)
		activations := model.GetNetworkExtenderActivations(ctx, extenderId, time.Time{})
		connect.AssertEqual(t, len(activations), 2)

		stored := activations[0]
		if stored.RunId == nil || *stored.RunId != testExtenderRunId {
			t.Fatalf("stored run id = %v, expected %s", stored.RunId, testExtenderRunId)
		}
		if stored.RunStartTime == nil || !stored.RunStartTime.Equal(runStartTime) {
			t.Fatalf("stored run start = %v, expected %s", stored.RunStartTime, runStartTime)
		}
		if stored.PreviousExit == nil || *stored.PreviousExit != "sigkill" {
			t.Fatalf("stored previous exit = %v, expected sigkill", stored.PreviousExit)
		}

		malformed := activations[1]
		if malformed.RunId != nil || malformed.RunStartTime != nil || malformed.PreviousExit != nil {
			t.Fatalf("malformed fields were stored: %v %v %v", malformed.RunId, malformed.RunStartTime, malformed.PreviousExit)
		}
	})
}

// An extender on an older release sends no run, and its rows hold null in all
// three columns.
func TestExtenderActivateWithoutARunStoresNull(t *testing.T) {
	useGeolocationThatPlacesNothing(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newTestExtenderFixture(t)
		installTestExtenderConfig(t, fixture.api)
		clientSession := newTestExtenderSession(t, ctx, fixture.clientAddress("127.0.0.1"))

		result, err := ExtenderActivate(fixture.activateArgs(), clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Activated {
			t.Fatalf("the activation was refused: %s", result.Error)
		}

		extenderId := testExtenderIdForKey(ctx, t, fixture.publicKey)
		activations := model.GetNetworkExtenderActivations(ctx, extenderId, time.Time{})
		connect.AssertEqual(t, len(activations), 1)
		activation := activations[0]
		if activation.RunId != nil || activation.RunStartTime != nil || activation.PreviousExit != nil {
			t.Fatalf("an activation with no run stored %v %v %v", activation.RunId, activation.RunStartTime, activation.PreviousExit)
		}
	})
}

// A refusal of any stage stores nothing of the run: the run is read only once
// the probes have passed, past the busy, budget and probe refusals' returns. Each
// refused post names a run of its own, and the history keeps the one activation
// that was stored.
func TestExtenderActivateRefusedStoresNoRun(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestExtenderConfig(t, nil)

		account := newTestExtenderAccount(ctx)
		host := account.newHost(t, ctx)
		const hostIp = "198.51.100.4"
		runStartTime := server.NowUtc().Add(-time.Minute).Format(time.RFC3339Nano)
		// spent by the activation that is stored and the probe that is refused
		const budget = 2
		activate := func(
			settings *ExtenderActivateSettings,
			carrierErr error,
			runId string,
		) *ExtenderActivateResult {
			service := newExtenderActivateService(settings)
			service.testProbes = testFakeExtenderProbes(carrierErr)
			args := host.activateArgs()
			args.RunId = runId
			args.RunStartTime = runStartTime
			args.PreviousExit = "sigkill"
			result, err := service.activate(args, host.session(ctx, hostIp))
			if err != nil {
				t.Fatal(err)
			}
			return result
		}

		settings := DefaultExtenderActivateSettings()
		settings.ClientRateLimit = budget
		stored := activate(settings, nil, testExtenderRunId)
		if !stored.Activated {
			t.Fatalf("the first activation was refused: %s", stored.Error)
		}
		extenderId := *stored.ExtenderId
		expectStoredOnly := func(refusal string, result *ExtenderActivateResult) {
			t.Helper()
			if result.Activated {
				t.Fatalf("the %s was activated", refusal)
			}
			activations := model.GetNetworkExtenderActivations(ctx, extenderId, time.Time{})
			connect.AssertEqual(t, len(activations), 1)
			if activations[0].RunId == nil || *activations[0].RunId != testExtenderRunId {
				t.Fatalf("after the %s the history holds run %v", refusal, activations[0].RunId)
			}
		}

		busySettings := DefaultExtenderActivateSettings()
		busySettings.MaxConcurrentProbes = 0
		busy := activate(busySettings, nil, "11112233445566778899aabbccddeeff")
		connect.AssertEqual(t, 0 < busy.RetryAfterSeconds, true)
		expectStoredOnly("busy refusal", busy)

		probeRefused := activate(settings, errors.New("test carrier refused"), "22222233445566778899aabbccddeeff")
		connect.AssertEqual(t, probeRefused.RetryAfterSeconds, 0)
		expectStoredOnly("probe refusal", probeRefused)

		budgetRefused := activate(settings, nil, "33332233445566778899aabbccddeeff")
		connect.AssertEqual(t, 0 < budgetRefused.RetryAfterSeconds, true)
		expectStoredOnly("budget refusal", budgetRefused)
	})
}

// Activates a new key once per run, each run started at `runStartTime`, so its
// history holds each of the runs. An empty run id is an activation with none.
// Every run is a client of the one network that holds the key: an activation
// from any other network is refused and stores nothing.
func testActivateExtenderRuns(ctx context.Context, name string, runStartTime time.Time, runIds ...string) server.Id {
	publicKey := []byte("extender-run-test-" + name)
	networkId := server.NewId()
	var extenderId server.Id
	for _, runId := range runIds {
		activation := &model.NetworkExtenderActivation{
			NetworkId:   networkId,
			ClientId:    server.NewId(),
			PublicKey:   publicKey,
			TcpPort:     443,
			UdpPort:     443,
			DnsPort:     53,
			DnsTld:      connect.DefaultExtenderDnsTld,
			CountryCode: "us",
			IpVersion:   4,
			Ip:          testExtenderRunAddr,
			Carriers:    []string{connect.ExtenderCarrierTcp},
		}
		if runId != "" {
			activation.RunId = &runId
			activation.RunStartTime = &runStartTime
		}
		activated := model.ActivateNetworkExtender(ctx, activation, func(
			extender *model.NetworkExtender,
			addresses []*model.NetworkExtenderAddress,
			issueTime time.Time,
		) ([]byte, error) {
			return []byte("record"), nil
		})
		extenderId = activated.Extender.ExtenderId
	}
	return extenderId
}

// The gauges count extenders, through the refresh the collector runs: five runs
// started in the window make an extender restarting, four do not, and one run
// re-activating many times is one run. Long-lived copies of one key, which
// started before the window, are no restart however many there are. An extender
// on an older release, with no run id, is in neither. Both publish a zero rather
// than go silent: a zero is the count.
func TestStatsRefreshExtendersCountsRestartingExtenders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		startedInTheWindow := now.Add(-time.Hour)
		startedBeforeTheWindow := now.Add(-3 * statsExtenderRunsWindow)

		// nothing reports yet, which is the fleet until a connect release
		// with run ids reaches it
		testActivateExtenderRuns(ctx, "older-release", time.Time{}, "", "", "", "", "", "")
		statsRefreshExtenders(ctx, now)
		connect.AssertEqual(t, testStatsGaugeValue(t, statsExtendersRestarting24hGauge), float64(0))
		connect.AssertEqual(t, testStatsGaugeValue(t, statsExtendersReportingRuns24hGauge), float64(0))

		testActivateExtenderRuns(ctx, "restarting", startedInTheWindow, "run-1", "run-2", "run-3", "run-4", "run-5")
		testActivateExtenderRuns(ctx, "restarted-a-few-times", startedInTheWindow, "run-1", "run-2", "run-3", "run-4")
		testActivateExtenderRuns(ctx, "stable", startedInTheWindow, "run-1", "run-1", "run-1", "run-1", "run-1", "run-1")
		testActivateExtenderRuns(ctx, "long-lived-copies", startedBeforeTheWindow, "copy-1", "copy-2", "copy-3", "copy-4", "copy-5")
		statsRefreshExtenders(ctx, now)
		connect.AssertEqual(t, testStatsGaugeValue(t, statsExtendersRestarting24hGauge), float64(1))
		connect.AssertEqual(t, testStatsGaugeValue(t, statsExtendersReportingRuns24hGauge), float64(4))

		// the window trails the collector's clock: a day on, those runs have
		// left it, and the gauges read zero again
		statsRefreshExtenderRuns(ctx, now.Add(statsExtenderRunsWindow+time.Hour))
		connect.AssertEqual(t, testStatsGaugeValue(t, statsExtendersRestarting24hGauge), float64(0))
		connect.AssertEqual(t, testStatsGaugeValue(t, statsExtendersReportingRuns24hGauge), float64(0))
	})
}

// A read that fails withdraws both gauges rather than hold their last values
// through a database incident, whether it was their own read or an earlier one
// of the refresh that never reached it, and the next read that works publishes
// them again.
func TestStatsExtenderRunsAreWithdrawnWhenAReadFails(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		testActivateExtenderRuns(ctx, "reporting", now.Add(-time.Hour), "run-1")

		expectPublished := func() {
			t.Helper()
			statsRefreshExtenderRuns(ctx, now)
			connect.AssertEqual(t, testStatsGaugeValue(t, statsExtendersRestarting24hGauge), float64(0))
			connect.AssertEqual(t, testStatsGaugeValue(t, statsExtendersReportingRuns24hGauge), float64(1))
		}
		expectWithdrawn := func() {
			t.Helper()
			if value := testStatsGaugeValue(t, statsExtendersRestarting24hGauge); !math.IsNaN(value) {
				t.Fatalf("extenders_restarting_24h = %f, want it withdrawn", value)
			}
			if value := testStatsGaugeValue(t, statsExtendersReportingRuns24hGauge); !math.IsNaN(value) {
				t.Fatalf("extenders_reporting_runs_24h = %f, want it withdrawn", value)
			}
		}
		failedCtx, cancel := context.WithCancel(ctx)
		cancel()

		expectPublished()
		statsRefreshExtenderRuns(failedCtx, now)
		expectWithdrawn()

		expectPublished()
		statsRefreshDbIsolated(failedCtx, newStatsEpochClock().window(now))
		expectWithdrawn()

		expectPublished()
	})
}
