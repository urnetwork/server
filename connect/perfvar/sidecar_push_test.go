package perfvar

// The push cell (PROTO-M2-07) at a smoke's size on the private test stack: every hint reaches its
// client by publisher or long-poll within S2's 2 s at the p99, the storm writes without failing, Redis
// CPU is read per master, and the reports say Smoke, so no gate takes them. §22.8's figures come from a
// run at DefaultSidecarPushCellSettings on T5 (sidecar_push_load_test.go), which is external.

import (
	"context"
	"testing"

	"github.com/urnetwork/sidecar/pushservice"
	"github.com/urnetwork/sidecar/sidecartest/load"

	"github.com/urnetwork/server"
)

func TestSidecarPushCellSmoke(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		server.Testing_EnableKeyspaceNotifications(ctx)
		report, err := RunSidecarPushCell(ctx, SmokeSidecarPushCellSettings())
		if err != nil {
			t.Fatal(err)
		}
		hints := report.Hints
		t.Logf("hints: %d of %d at p50 %.0f ms, p99 %.0f ms; storm: %d at p99 %.0f ms; %d hint frames, %d resets; redis cpu %v",
			hints.Succeeded, hints.Arrivals, hints.LatencyMillis.P50, hints.LatencyMillis.P99,
			report.ConfigStorm.Succeeded, report.ConfigStorm.LatencyMillis.P99,
			report.HintFrameCount, report.ResetFrameCount, report.RedisCpuPercents)
		if hints.Succeeded == 0 || hints.Failed != 0 || hints.Arrivals != hints.Scheduled {
			t.Fatalf("hints: %d of %d succeeded, %d failed %v", hints.Succeeded, hints.Arrivals, hints.Failed, hints.FailureKinds)
		}
		if 2000 < hints.LatencyMillis.P99 {
			t.Errorf("hints: p99 %.0f ms, past S2's 2 s", hints.LatencyMillis.P99)
		}
		if storm := report.ConfigStorm; storm.Succeeded == 0 || storm.Failed != 0 {
			t.Errorf("storm: %d succeeded, %d failed %v", storm.Succeeded, storm.Failed, storm.FailureKinds)
		}
		if report.HintFrameCount < hints.Succeeded || len(report.RedisCpuPercents) == 0 {
			t.Errorf("%d hint frames for %d hints, cpu %v", report.HintFrameCount, hints.Succeeded, report.RedisCpuPercents)
		}
		// a smoke is never evidence, and its report is the shape every run reports in
		if report.GateEvidence() == nil || hints.GateEvidence() == nil {
			t.Error("a smoke stood as gate evidence")
		}
		if len(hints.Settings) == 0 || len(hints.Targets) == 0 {
			t.Errorf("settings %v, targets %v", hints.Settings, hints.Targets)
		}
		encoded, err := hints.Json()
		if err != nil {
			t.Fatal(err)
		}
		if decoded, err := load.ParseReport(encoded); err != nil || decoded.Succeeded != hints.Succeeded || !decoded.Smoke {
			t.Errorf("the report did not round-trip: %v", err)
		}
	})
}

// The gate's run is §22.8's: twice year two's 6,000 hints a second beside 10,000 config heads a minute,
// on the constructors' own settings, and no smoke.
func TestSidecarPushCellDefaultsAreTheGatesRun(t *testing.T) {
	settings := DefaultSidecarPushCellSettings()
	if settings.HintRate != 12000 || settings.ConfigNetworksPerMinute != 10000 || settings.Smoke {
		t.Errorf("the gate's run is %+v", settings)
	}
	if *settings.Publisher != *pushservice.DefaultSidecarPublisherSettings() || *settings.LongPoll != *pushservice.DefaultLongPollSettings() || *settings.HintWriter != *pushservice.DefaultHintWriterSettings() {
		t.Error("the gate's run is not on the constructors' settings")
	}
	if err := settings.validate(); err != nil {
		t.Error(err)
	}
	if !SmokeSidecarPushCellSettings().Smoke {
		t.Error("the smoke's settings are not marked a smoke")
	}
}
