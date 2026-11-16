//go:build acklineagetrace

package perfvar

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
)

// This private corrected-H1 control is a recurrence diagnostic, not a
// canonical PERF record. The unqualified runtime replacement experiment was
// removed; no replacement flag, hot-path hook or synthetic counter remains.
func TestH1ReadinessScreenIdentity(t *testing.T) {
	readinessFailureRun1Scenario(t)
}

type h1ReadinessScreenMemory struct {
	Samples       uint64 `json:"samples"`
	PeakHeapStack uint64 `json:"peak_heap_stack_bytes"`
	PeakGoRuntime uint64 `json:"peak_go_runtime_bytes"`
	PeakSys       uint64 `json:"peak_sys_bytes"`
	Allocated     uint64 `json:"allocated_bytes"`
	Mallocs       uint64 `json:"mallocs"`
	MaxRSS        int64  `json:"process_lifetime_max_rss_bytes"`
}

// Cold construction/route-publication evidence only. No packet/owner-loop
// callback, timer, history, address or identifier is added to the screen.
type h1ReadinessScreenWiring struct {
	GeneratorConfigurations uint64 `json:"generator_configurations"`
	DeviceH1, ProviderH1    uint64
	Unknown, Other          uint64
}

type h1ReadinessScreenWiringCounters struct {
	configured, deviceH1, providerH1, unknown, other atomic.Uint64
}

func (self *h1ReadinessScreenWiringCounters) observe(device bool, event platformSendRouteEvent) {
	if event.kind != platformSendRouteEventPlatformRoute || !event.connected {
		return
	}
	switch transportTypeForH1TypingTest(event.transport) {
	case clientconnect.TransportTypeH1:
		if device {
			self.deviceH1.Add(1)
		} else {
			self.providerH1.Add(1)
		}
	case clientconnect.TransportTypeUnknown:
		self.unknown.Add(1)
	default:
		self.other.Add(1)
	}
}

func (self *h1ReadinessScreenWiringCounters) snapshot() h1ReadinessScreenWiring {
	return h1ReadinessScreenWiring{self.configured.Load(), self.deviceH1.Load(), self.providerH1.Load(), self.unknown.Load(), self.other.Load()}
}

func TestH1ReadinessScreenWiringObserver(t *testing.T) {
	var counters h1ReadinessScreenWiringCounters
	for _, kind := range []clientconnect.TransportType{clientconnect.TransportTypeH1, clientconnect.TransportTypeUnknown, clientconnect.TransportTypeP2p} {
		event := platformSendRouteEvent{kind: platformSendRouteEventPlatformRoute, connected: true, transport: clientconnect.NewSendGatewayTransportWithType(kind)}
		counters.observe(true, event)
		counters.observe(false, event)
		event.connected = false
		counters.observe(true, event)
		event.connected, event.kind = true, platformSendRouteEventP2pRoute
		counters.observe(true, event)
	}
	if got := counters.snapshot(); got != (h1ReadinessScreenWiring{DeviceH1: 1, ProviderH1: 1, Unknown: 2, Other: 2}) {
		t.Fatalf("cold wiring evidence was misclassified: %+v", got)
	}
}

func startH1ReadinessScreenMemory() func() h1ReadinessScreenMemory {
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	done, joined := make(chan struct{}), make(chan struct{})
	var stop sync.Once
	var result h1ReadinessScreenMemory
	go func() {
		defer close(joined)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			var sample runtime.MemStats
			runtime.ReadMemStats(&sample)
			result.Samples++
			result.PeakHeapStack = max(result.PeakHeapStack, sample.HeapAlloc+sample.StackInuse)
			result.PeakGoRuntime = max(result.PeakGoRuntime, sample.Sys-sample.HeapReleased)
			result.PeakSys = max(result.PeakSys, sample.Sys)
			select {
			case <-done:
				return
			case <-ticker.C:
			}
		}
	}()
	return func() h1ReadinessScreenMemory {
		stop.Do(func() { close(done); <-joined })
		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		result.Allocated, result.Mallocs = after.TotalAlloc-before.TotalAlloc, after.Mallocs-before.Mallocs
		result.PeakHeapStack = max(result.PeakHeapStack, after.HeapAlloc+after.StackInuse)
		result.PeakGoRuntime = max(result.PeakGoRuntime, after.Sys-after.HeapReleased)
		result.PeakSys = max(result.PeakSys, after.Sys)
		result.MaxRSS = tcpRecoveryMaxRSS()
		return result
	}
}

func h1ReadinessScreenCPU() (int64, error) {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0, err
	}
	return (int64(usage.Utime.Sec)+int64(usage.Stime.Sec))*1000000 + int64(usage.Utime.Usec) + int64(usage.Stime.Usec), nil
}

type h1ReadinessWholeRunCarrier struct {
	Links                                                       map[string]directionalLinkSnapshot `json:"exchange_links"`
	Application, Device, Provider, Edge                         h1FailureTCPStackCounters
	ApplicationQueueDrops, DeviceQueueDrops, ProviderQueueDrops uint64
	DeviceRecovery, ProviderRecovery                            clientconnect.ClientSendRecoveryStatsSnapshot
}

// Joined lifetime counters include setup handshakes and retransmits,
// unlike the post-readiness workload interval. Failed controls remain records.
func h1ReadinessWholeRun(path *fullTunPath) h1ReadinessWholeRunCarrier {
	var result h1ReadinessWholeRunCarrier
	if path == nil {
		return result
	}
	result.Application, result.Device, result.Provider = h1FailureTCPStats(path.appTun), h1FailureTCPStats(path.deviceCarrierTun), h1FailureTCPStats(path.providerCarrierTun)
	if path.environment != nil {
		result.Edge = h1FailureTCPStats(path.environment.edgeTun)
		if path.environment.network != nil {
			result.Links = path.environment.network.snapshotLinks()
		}
	}
	if path.appTun != nil {
		result.ApplicationQueueDrops = path.appTun.OutboundDropCount()
	}
	if path.deviceCarrierTun != nil {
		result.DeviceQueueDrops = path.deviceCarrierTun.OutboundDropCount()
	}
	if path.providerCarrierTun != nil {
		result.ProviderQueueDrops = path.providerCarrierTun.OutboundDropCount()
	}
	if path.providerClient != nil {
		result.ProviderRecovery = path.providerClient.SendRecoveryStats()
	}
	if path.deviceClient != nil {
		if client := path.deviceClient.Load(); client != nil {
			result.DeviceRecovery = client.SendRecoveryStats()
		}
	}
	return result
}

func TestH1ReadinessScreenResourceSampler(t *testing.T) {
	before, err := h1ReadinessScreenCPU()
	if err != nil {
		t.Fatal(err)
	}
	finish := startH1ReadinessScreenMemory()
	first, second := finish(), finish()
	after, err := h1ReadinessScreenCPU()
	if err != nil || after < before || first.Samples == 0 || first.PeakGoRuntime == 0 || second.Samples != first.Samples {
		t.Fatalf("screen sampler failed: before=%d after=%d first=%+v second=%+v err=%v", before, after, first, second, err)
	}
}

func TestH1ReadinessExactCellExperiment(t *testing.T) {
	if os.Getenv("CONNECT_PERFVAR_H1_TYPED_READINESS_SCREEN") == "" {
		t.Skip("explicit parent-owned corrected-H1 control required")
	}
	if os.Getenv("CONNECT_PERFVAR_H1_TYPED_READINESS_SCREEN") != "record36" || os.Getenv("URNETWORK_ACK_LINEAGE_REPLAY_SLOT") != "granted" ||
		runtime.GOMAXPROCS(0) != 2 || perfvarRaceEnabled || !ackLineageReplayRuntimeSupported(runtime.Version(), runtime.GOOS, runtime.GOARCH, 2) {
		t.Fatal("screen requires pinned two-core non-race parent-owned source/host slot")
	}
	if perfvarProgressTraceEnabled() || clientconnect.DefaultLogger().V(1).Enabled() || newFullTunConstructionHooksForTest != nil || newPerfvarProgressTraceForTest != nil {
		t.Fatal("screen requires V0 and unowned hooks")
	}
	scenario := readinessFailureRun1Scenario(t)
	environment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	environment.Run(t, func(t testing.TB) {
		recorder := newReadinessFailureRecorder()
		var captured *fullTunPath
		var budget *clientconnect.PlatformTransportBudget
		var wiring h1ReadinessScreenWiringCounters
		newFullTunConstructionHooksForTest = func() *fullTunConstructionTestHooks {
			hooks := recorder.hooks()
			after, platform := hooks.afterStage, hooks.configureDevicePlatformSettings
			hooks.afterStage = func(stage fullTunConstructionStage, path *fullTunPath) error {
				captured = path
				if stage == fullTunConstructionStageSendRouteControllers {
					for index, controller := range []*platformSendRouteController{path.deviceSendRoutes, path.providerSendRoutes} {
						if controller == nil {
							return fmt.Errorf("screen lost its platform route controller")
						}
						prior := controller.afterEventApplyForTest
						controller.afterEventApplyForTest = func(event platformSendRouteEvent) {
							if prior != nil {
								prior(event)
							}
							wiring.observe(index == 0, event)
						}
					}
				}
				return after(stage, path)
			}
			hooks.configureDevicePlatformSettings = func(settings *clientconnect.PlatformTransportSettings) {
				if platform != nil {
					platform(settings)
				}
				budget = settings.PlatformTransportBudget
			}
			hooks.configureDeviceGeneratorSettings = func(settings *clientconnect.ApiMultiClientGeneratorSettings) {
				wiring.configured.Add(1)
				if settings.PlatformTransportMode != clientconnect.TransportModeH1 {
					t.Fatal("original readiness is not explicit H1")
				}
			}
			return hooks
		}
		defer func() { newFullTunConstructionHooksForTest = nil }()
		cpuBefore, err := h1ReadinessScreenCPU()
		if err != nil {
			t.Fatal(err)
		}
		finishMemory := startH1ReadinessScreenMemory()
		defer finishMemory()
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), perfvarRunTimeout(scenario))
		record, runErr := measurePerfvarRun(ctx, t, scenario, 1)
		cancel()
		elapsed := time.Since(start)
		memory := finishMemory()
		cpuAfter, cpuErr := h1ReadinessScreenCPU()
		recorder.dump(t)
		if cpuErr != nil {
			t.Fatal(cpuErr)
		}
		var claims clientconnect.PlatformTransportBudgetStats
		if budget != nil {
			claims = budget.Stats()
		}
		record.Host.MeasurementKind = "diagnostic-h1-typed-readiness-screen"
		value := struct {
			Kind             string                                     `json:"record_type"`
			BaselineEligible bool                                       `json:"baseline_eligible"`
			Record           perfvarRunRecord                           `json:"record"`
			Budget           clientconnect.PlatformTransportBudgetStats `json:"budget_after_close"`
			Memory           h1ReadinessScreenMemory                    `json:"whole_run_memory"`
			WholeRunCarrier  h1ReadinessWholeRunCarrier                 `json:"whole_run_carrier"`
			CPU              int64                                      `json:"whole_run_cpu_microseconds"`
			Elapsed          int64                                      `json:"whole_run_nanoseconds"`
			Wiring           h1ReadinessScreenWiring                    `json:"wiring"`
		}{"h1-typed-readiness-screen", false, record, claims, memory, h1ReadinessWholeRun(captured), cpuAfter - cpuBefore, int64(elapsed), wiring.snapshot()}
		encoded, marshalErr := json.Marshal(value)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		t.Logf("[h1-typed-readiness-screen] %s", encoded)
		if value.Wiring.GeneratorConfigurations != 1 || value.Wiring.DeviceH1 == 0 || value.Wiring.ProviderH1 == 0 || value.Wiring.Unknown != 0 || value.Wiring.Other != 0 {
			t.Errorf("screen physical carrier wiring invalid: %+v", value.Wiring)
		}
		if budget == nil || claims.UsedByteCount != 0 || claims.PendingH1Count != 0 || claims.ActiveHandoffCount != 0 || claims.ReservedByteCount != claims.ReleasedByteCount {
			t.Error("screen transport claims did not reconcile")
		}
		if runErr != nil {
			t.Fatal(runErr)
		}
		if !record.Correct {
			t.Errorf("screen failed: %s: %s", record.FailureStage, record.FailureReason)
		}
	})
}
