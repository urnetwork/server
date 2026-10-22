// Public host-scope authority for the canonical sharded-subscription census.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The shared adapter owns the public class for a denied required input. Its
// per-probe frame and visibility bit preserve recovery state without labels.
func redisShardRequireScopeUnknown(t *testing.T, alerts Alerts, excludedCount int) Alert {
	t.Helper()
	partial := requireAlertClass(t, alerts, "monitor-host-scope-partial")
	if partial.SignalNumber != "3.6a" || partial.SignalKey != "redis-shard-subscriptions" || partial.SignalID != "monitor/host-scope" || partial.Target != "monitor-host-scope" || partial.Frame != "redis-shard-subscriptions" {
		t.Fatal("denied census lost its stable public scope identity")
	}
	if partial.Severity != SeverityWarn || partial.Sustain != 1 || partial.PageSustain != 0 || !isVisibilityAlert(partial) || partial.configuredOff {
		t.Fatal("denied census became policy-only, healthy or paging evidence")
	}
	if !strings.Contains(partial.Observed, fmt.Sprintf("excluded_hosts=%d blocked_hosts=1", excludedCount)) || !strings.Contains(partial.Observed, "desired_topology_unchanged=true") {
		t.Fatal("required denied input was not recorded as incomplete coverage")
	}
	requireAlertOmits(t, partial, "proxy-1", "proxy_client_", "192.0.2.")
	return partial
}

// Model the loader's immutable disabled inventory separately from ExcludeHosts.
// Desired service host/block counts remain intact in each fresh settings copy.
func redisShardScopeTestSettings(t *testing.T, settings SignalSettings, mode string) SignalSettings {
	t.Helper()
	if mode == "excluded" {
		paused, err := ExcludeHosts(settings, "proxy-1")
		if err != nil {
			t.Fatal(err)
		}
		return paused
	}
	if mode != "disabled" && mode != "disabled-without-proxy-metadata" {
		t.Fatal("unknown scope fixture mode")
	}
	disabled := settings.Hosts[0]
	if disabled.Name != "proxy-1" {
		t.Fatal("scope fixture lost its desired Proxy host")
	}
	if mode == "disabled-without-proxy-metadata" {
		disabled.Proxy = nil
	}
	settings.disabledHosts = []HostSettings{disabled}
	settings.Hosts = append([]HostSettings(nil), settings.Hosts[1:]...)
	return settings
}

// This reaches the former false-health path: private placement unknown was
// suppressed before a scope guard had recorded the disabled required input.
func TestRedisShardSubscriptionsDisabledPlacementIsUnknown(t *testing.T) {
	for _, mode := range []string{"disabled", "disabled-without-proxy-metadata"} {
		settings, requests := redisShardTestSettings(t, nil)
		settings = redisShardScopeTestSettings(t, settings, mode)
		alerts, err := NewRedisShardSubscriptionsSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 1 || len(*requests) != 0 {
			t.Fatalf("disabled Proxy placement lost public unknown coverage or contacted a source: mode=%s alerts=%d calls=%d", mode, len(alerts), len(*requests))
		}
		redisShardRequireScopeUnknown(t, alerts, 0)
	}
}

// A denied desired sibling cannot suppress a permitted owner's authoritative
// zero, or turn positive permitted presence into a complete fleet observation.
func TestRedisShardSubscriptionsPartialScopePreservesPermittedEvidence(t *testing.T) {
	for _, mode := range []string{"excluded", "disabled", "disabled-without-proxy-metadata"} {
		for _, subscribers := range []int{0, 1} {
			settings, requests := redisShardTestSettings(t, func(request redisShardRequest, _ int, output string) (string, error) {
				if request.Phase == "counts" {
					rows := []string{}
					for _, channel := range request.Channels {
						rows = append(rows, fmt.Sprintf("c %d %d %d %d", channel.Index, channel.Slot, channel.Owner, subscribers))
					}
					output = redisShardTestFrame(request, time.Date(2026, 10, 19, 20, 0, 0, 0, time.UTC), true, rows)
				}
				return output, nil
			})
			settings.Hosts = append(settings.Hosts, HostSettings{Name: "proxy-2", Proxy: &ProxyHostSettings{PublicHostname: "proxy-2.example.test"}})
			settings.LogServiceHosts["proxy"] = []string{"proxy-1", "proxy-2"}
			settings.LogServiceBlocks["proxy"] = []string{"g1"}
			settings.ProxyPathExpectedHosts = 2
			settings = redisShardScopeTestSettings(t, settings, mode)
			alerts, err := NewRedisShardSubscriptionsSignal().Run(context.Background(), settings)
			if err != nil {
				t.Fatal(err)
			}
			excludedCount := 0
			if mode == "excluded" {
				excludedCount = 1
			}
			redisShardRequireScopeUnknown(t, alerts, excludedCount)
			wantAlerts := 1
			if subscribers == 0 {
				wantAlerts++
				missing := requireAlertClass(t, alerts, "redis-shard-subscriber-missing")
				if missing.Target != "proxy-2/proxy/g1" || missing.Severity != SeverityWarn || isVisibilityAlert(missing) {
					t.Fatal("permitted sibling's independent zero lost its incident identity")
				}
			}
			if len(alerts) != wantAlerts {
				t.Fatal("partial scope manufactured an excluded incident or lost visibility")
			}
			permitted := false
			for _, request := range *requests {
				if request.Host == "proxy-1" {
					t.Fatal("partial scope contacted its denied desired host")
				}
				permitted = permitted || request.Host == "proxy-2"
			}
			if !permitted {
				t.Fatal("partial scope suppressed the permitted source control")
			}
		}
	}
}

// Parent cancellation still wins before a required-input denial is recorded.
func TestRedisShardSubscriptionsScopeCancellation(t *testing.T) {
	for _, mode := range []string{"excluded", "disabled"} {
		settings, requests := redisShardTestSettings(t, nil)
		settings = redisShardScopeTestSettings(t, settings, mode)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		alerts, err := NewRedisShardSubscriptionsSignal().Run(ctx, settings)
		if !errors.Is(err, context.Canceled) || len(alerts) != 0 || len(*requests) != 0 {
			t.Fatal("canceled scope manufactured visibility or contacted a source")
		}
	}
}

// The current probe raises WARN only. A PAGE is seeded solely as pre-existing
// notifier state to prove severity retention; this adds no probe paging policy.
// Each stage creates a new immutable settings observation, never a live pause.
func TestRedisShardSubscriptionsPartialScopeCannotClearExistingPage(t *testing.T) {
	for _, mode := range []string{"excluded", "disabled"} {
		for _, completeRecovery := range []bool{false, true} {
			var subscribers atomic.Int64
			settings, requests := redisShardTestSettings(t, func(request redisShardRequest, _ int, output string) (string, error) {
				if request.Phase == "counts" {
					output = redisShardTestFrame(request, time.Date(2026, 10, 19, 20, 0, 0, 0, time.UTC), true, []string{fmt.Sprintf("c 0 0 0 %d", subscribers.Load())})
				}
				return output, nil
			})
			settings.LogServiceBlocks["proxy"] = []string{"g1"}
			signal := NewRedisShardSubscriptionsSignal()
			initial, err := signal.Run(context.Background(), settings)
			if err != nil || len(initial) != 1 || initial[0].Class != "redis-shard-subscriber-missing" || initial[0].Severity != SeverityWarn {
				t.Fatal("scope notifier control did not reach its actual WARN seed")
			}
			webhook, webhookUrl := newFakeSlackWebhook(t)
			harness := newSlackTestHarness()
			notifier := NewSlackAlertNotifier(harness.settings(webhookUrl))
			t.Cleanup(func() { closeSlackNotifier(t, notifier) })
			existing := initial[0]
			existing.Severity = SeverityPage
			notifier.Handle(signal, SignalExecution{Alerts: Alerts{existing}})

			subscribers.Store(1)
			postSignalRunsThroughTheGate(t, signal, settings, 1, func(int) {}, notifier)
			paused := redisShardScopeTestSettings(t, settings, mode)
			callsBeforePause := len(*requests)
			postSignalRunsThroughTheGate(t, signal, paused, 2, func(int) {}, notifier)
			if len(*requests) != callsBeforePause {
				t.Fatal("excluded observation contacted a source during notifier retention")
			}
			if completeRecovery {
				postSignalRunsThroughTheGate(t, signal, settings, 1, func(int) {}, notifier)
			}
			closeSlackNotifier(t, notifier)
			texts := webhook.posted()
			identity := "redis/shard-subscriptions · redis-shard-subscriber-missing · proxy-1/proxy/g1"
			wantCleared := 0
			if completeRecovery {
				wantCleared = 1
			}
			if countSlackPosts(texts, "[PAGE] raised", identity) != 1 || countSlackPosts(texts, "[WARN] raised", identity) != 0 || countSlackPosts(texts, "[CLEARED] PAGE", identity) != wantCleared {
				t.Fatal("partial scope cleared, duplicated or downgraded the existing PAGE")
			}
			if countSlackPosts(texts, "[WARN] raised", "monitor/host-scope · monitor-host-scope-partial · monitor-host-scope") != 1 {
				t.Fatal("denied observation never reached the public notifier as scope visibility")
			}
		}
	}
}
