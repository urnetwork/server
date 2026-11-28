// Synthetic source and actual native-producer controls for SIGNALS.md §3.6a.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Source fixtures decode only the generated request, never execute a shell.
func redisShardTestRequest(t *testing.T, command string) redisShardRequest {
	t.Helper()
	index := strings.LastIndex(command, " '{")
	if index < 0 || !strings.HasSuffix(command, "'") {
		t.Fatal("missing sharded source request")
	}
	var request redisShardRequest
	if err := json.Unmarshal([]byte(command[index+2:len(command)-1]), &request); err != nil {
		t.Fatal("invalid sharded source request")
	}
	return request
}

// Exact wire rows have no source-derived text outside the fixed grammar.
func redisShardTestFrame(request redisShardRequest, now time.Time, complete bool, rows []string) string {
	mapping := strings.Repeat("a", 64)
	if request.Phase == "allocations" {
		mapping = "-"
	}
	completeValue := "0"
	if complete {
		completeValue = "1"
	}
	return fmt.Sprintf("rsc1 %s %s %d %d 0 %d %s %s\n%s", request.Nonce, request.Phase, now.UnixNano(), now.UnixNano(), len(rows), completeValue, mapping,
		strings.Join(append(rows, fmt.Sprintf("end %d", len(rows))), "\n")+"\n")
}

// The fixture has one independent channel on each configured Redis owner.
func redisShardTestSettings(t *testing.T, mutate func(redisShardRequest, int, string) (string, error)) (SignalSettings, *[]redisShardRequest) {
	t.Helper()
	requests := []redisShardRequest{}
	calls := map[string]int{}
	now := time.Date(2026, 10, 19, 20, 0, 0, 0, time.UTC)
	source := &syntheticSource{hostTimeoutFn: func(h HostSettings, command string, timeout time.Duration) (string, error) {
		if timeout != redisShardSourceBudget {
			t.Fatal("source timeout changed")
		}
		request := redisShardTestRequest(t, command)
		if request.Host != h.Name {
			t.Fatal("source routed to wrong configured host")
		}
		requests = append(requests, request)
		key := request.Phase + "/" + h.Name
		calls[key]++
		rows := []string{}
		switch request.Phase {
		case "allocations":
			for index := range request.Blocks {
				rows = append(rows, fmt.Sprintf("a %d active 0 %s", index, strings.Repeat("b", 64)))
			}
		case "resolve":
			for _, channel := range request.Channels {
				rows = append(rows, fmt.Sprintf("r %d %d %d", channel.Index, channel.Index, channel.Index%2))
			}
		case "counts":
			for _, channel := range request.Channels {
				rows = append(rows, fmt.Sprintf("c %d %d %d 0", channel.Index, channel.Slot, channel.Owner))
			}
		default:
			t.Fatal("unexpected source phase")
		}
		result := redisShardTestFrame(request, now, true, rows)
		if mutate != nil {
			return mutate(request, calls[key], result)
		}
		return result, nil
	}}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return now }
	settings.SettingsGenerationCheck = func(context.Context, SignalSettings) (bool, error) { return true, nil }
	settings.PublicDomain = "example.test"
	settings.LogServices = []string{"proxy"}
	settings.LogServiceBlocks = map[string][]string{"proxy": {"g1", "g2"}}
	settings.LogServiceHosts = map[string][]string{"proxy": {"proxy-1"}}
	settings.ProxyPathExpectedHosts = 1
	settings.Hosts = []HostSettings{
		{Name: "proxy-1", Proxy: &ProxyHostSettings{PublicHostname: "proxy-1.example.test"}},
		{Name: "redis-1", Roles: []string{"redis-cluster"}, LANAddress: "192.0.2.1", RedisEntryPort: 6379, RedisNodePorts: []int{6380}},
		{Name: "redis-2", Roles: []string{"redis-cluster"}, LANAddress: "192.0.2.2", RedisNodePorts: []int{6381}},
	}
	return settings, &requests
}

// Zero requires the current configured owner and an active canonical channel.
func TestRedisShardSubscriptionsMissingAndHealthy(t *testing.T) {
	for _, subscribers := range []int{0, 1, 2} {
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
		alerts, err := NewRedisShardSubscriptionsSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		if subscribers > 0 {
			if len(alerts) != 0 {
				t.Fatal("positive owner count did not establish canonical presence")
			}
		} else {
			if len(alerts) != 2 {
				t.Fatal("independent missing channels were not preserved")
			}
			for _, alert := range alerts {
				if alert.SignalNumber != "3.6a" || alert.SignalKey != "redis-shard-subscriptions" || alert.SignalID != "redis/shard-subscriptions" || alert.Class != "redis-shard-subscriber-missing" || alert.Severity != SeverityWarn || alert.Sustain != 1 {
					t.Fatal("wrong missing subscriber alert identity")
				}
				requireAlertOmits(t, alert, "proxy_client_", strings.Repeat("b", 64), "192.0.2.")
				if !strings.Contains(alert.Markdown(), "compatibility_aliases=not-observed") || !strings.Contains(alert.Markdown(), "five-second") {
					t.Fatal("scope or fallback qualifier missing")
				}
			}
		}
		for _, request := range *requests {
			for _, channel := range request.Channels {
				if channel.Name != "proxy_client_proxy-1_g1" && channel.Name != "proxy_client_proxy-1_g2" {
					t.Fatal("invented channel coverage")
				}
				if request.Phase == "counts" && request.Endpoint != channel.Owner {
					t.Fatal("wrong-owner read admitted")
				}
			}
		}
	}
}

// A missing sibling never removes an independently authoritative zero.
func TestRedisShardSubscriptionsPartialPreservesIndependentZero(t *testing.T) {
	settings, _ := redisShardTestSettings(t, func(request redisShardRequest, _ int, output string) (string, error) {
		if request.Phase == "counts" && request.Endpoint == 1 {
			return output, errors.New("private-native-error-marker")
		}
		return output, nil
	})
	alerts, err := NewRedisShardSubscriptionsSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	missing := requireAlertClass(t, alerts, "redis-shard-subscriber-missing")
	unknown := requireAlertClass(t, alerts, "redis-shard-subscriptions-unobservable")
	if missing.Target != "proxy-1/proxy/g1" || unknown.Target != "proxy-1/proxy/g2" {
		t.Fatal("partial source paired unrelated channels")
	}
	requireAlertOmits(t, unknown, "private-native-error-marker")
}

// Allocation or map changes invalidate a previously observed zero.
func TestRedisShardSubscriptionsRevalidation(t *testing.T) {
	for _, boundary := range []string{"map-change", "allocation-change", "nonrunning", "duplicate-row", "wrong-owner", "stale-clock", "future-clock", "foreign-nonce", "native-failed-positive", "capped"} {
		settings, _ := redisShardTestSettings(t, func(request redisShardRequest, call int, output string) (string, error) {
			switch boundary {
			case "map-change":
				if request.Phase == "resolve" && call == 2 {
					output = strings.ReplaceAll(output, strings.Repeat("a", 64), strings.Repeat("c", 64))
				}
			case "allocation-change":
				if request.Phase == "allocations" && call == 2 {
					output = strings.ReplaceAll(output, strings.Repeat("b", 64), strings.Repeat("c", 64))
				}
			case "nonrunning":
				if request.Phase == "allocations" {
					output = strings.ReplaceAll(output, "active 0 "+strings.Repeat("b", 64), "inactive - -")
				}
			case "duplicate-row":
				if request.Phase == "allocations" {
					output = strings.Replace(output, "a 1 active", "a 0 active", 1)
				}
			case "wrong-owner":
				if request.Phase == "counts" {
					output = strings.Replace(output, fmt.Sprintf(" %d 0\n", request.Endpoint), fmt.Sprintf(" %d 0\n", 1-request.Endpoint), 1)
				}
			case "stale-clock":
				output = strings.ReplaceAll(output, strconv.FormatInt(time.Date(2026, 10, 19, 20, 0, 0, 0, time.UTC).UnixNano(), 10), strconv.FormatInt(time.Date(2026, 10, 19, 19, 59, 0, 0, time.UTC).UnixNano(), 10))
			case "future-clock":
				output = strings.ReplaceAll(output, strconv.FormatInt(time.Date(2026, 10, 19, 20, 0, 0, 0, time.UTC).UnixNano(), 10), strconv.FormatInt(time.Date(2026, 10, 19, 20, 1, 0, 0, time.UTC).UnixNano(), 10))
			case "foreign-nonce":
				output = strings.Replace(output, request.Nonce, strings.Repeat("f", 32), 1)
			case "native-failed-positive":
				return output, errors.New("native failed")
			case "capped":
				return strings.Repeat("x", redisShardSourceLimit+1), nil
			}
			return output, nil
		})
		alerts, err := NewRedisShardSubscriptionsSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		requireAlertClass(t, alerts, "redis-shard-subscriptions-unobservable")
		for _, alert := range alerts {
			if alert.Class == "redis-shard-subscriber-missing" {
				t.Fatal("unproved zero became subscriber incident")
			}
		}
	}
}

// Missing inventory, intentional exclusion and a genuine configured dark
// deployment have distinct outcomes and never contact a forbidden host.
func TestRedisShardSubscriptionsInventoryAndDisabledScope(t *testing.T) {
	settings, requests := redisShardTestSettings(t, nil)
	settings.LogServiceBlocks = nil
	alerts, err := NewRedisShardSubscriptionsSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "redis-shard-subscriptions-unobservable")
	if len(*requests) != 0 {
		t.Fatal("missing inventory contacted a target")
	}

	settings, requests = redisShardTestSettings(t, nil)
	settings.ExcludedHosts = []string{"proxy-1"}
	alerts, err = NewRedisShardSubscriptionsSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatal("excluded placement did not retain exactly one public scope visibility alert")
	}
	redisShardRequireScopeUnknown(t, alerts, 1)
	if len(*requests) != 0 {
		t.Fatal("excluded placement contacted a target")
	}

	settings, requests = redisShardTestSettings(t, nil)
	settings.Hosts = settings.Hosts[1:]
	settings.LogServiceHosts["proxy"] = []string{}
	settings.ProxyPathExpectedHosts = 0
	alerts, err = NewRedisShardSubscriptionsSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 || len(*requests) != 0 {
		t.Fatal("explicit configured dark state did not remain contact-free")
	}
}

// Cancellation terminates the source sequence rather than manufacturing a
// healthy empty census or contacting the next owner.
func TestRedisShardSubscriptionsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings, requests := redisShardTestSettings(t, func(_ redisShardRequest, _ int, output string) (string, error) { cancel(); return output, nil })
	_, err := NewRedisShardSubscriptionsSignal().Run(ctx, settings)
	if !errors.Is(err, context.Canceled) || len(*requests) != 1 {
		t.Fatal("source cancellation did not own the remaining census")
	}
}

// The generation seam protects configured dark state and every source
// admission/result boundary. Its private failure text never becomes evidence.
func TestRedisShardSubscriptionsSettingsGenerationAuthority(t *testing.T) {
	for _, boundary := range []string{"missing", "stale", "error", "dark-missing", "dark-stale", "dark-error", "dark-final"} {
		settings, requests := redisShardTestSettings(t, nil)
		checks := 0
		settings.SettingsGenerationCheck = func(context.Context, SignalSettings) (bool, error) {
			checks++
			if boundary == "dark-final" {
				return checks == 1, nil
			}
			if strings.Contains(boundary, "error") {
				return false, errors.New("private-settings-generation-marker")
			}
			return false, nil
		}
		if strings.Contains(boundary, "missing") {
			settings.SettingsGenerationCheck = nil
		}
		if strings.HasPrefix(boundary, "dark-") {
			settings.Hosts = settings.Hosts[1:]
			settings.LogServiceHosts["proxy"] = []string{}
			settings.ProxyPathExpectedHosts = 0
		}
		alerts, err := NewRedisShardSubscriptionsSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 1 || len(*requests) != 0 {
			t.Fatalf("generation case %s did not stay unknown and contact-free", boundary)
		}
		unknown := requireAlertClass(t, alerts, "redis-shard-subscriptions-unobservable")
		if !isVisibilityAlert(unknown) || !strings.Contains(unknown.Observed, "reason=settings-generation") {
			t.Fatal("settings failure did not retain fixed visibility identity")
		}
		requireAlertOmits(t, unknown, "private-settings-generation-marker")
	}
	// Two block owners need six native calls; each has a pre/post generation
	// check in addition to the initial admission and final reduction checks.
	for failedCheck := 1; failedCheck <= 14; failedCheck++ {
		settings, requests := redisShardTestSettings(t, nil)
		checks := 0
		settings.SettingsGenerationCheck = func(context.Context, SignalSettings) (bool, error) {
			checks++
			return checks != failedCheck, nil
		}
		alerts, err := NewRedisShardSubscriptionsSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 1 || checks != failedCheck || len(*requests) != (failedCheck-1)/2 {
			t.Fatalf("settings boundary %d did not stop its source sequence: checks=%d calls=%d", failedCheck, checks, len(*requests))
		}
		requireAlertClass(t, alerts, "redis-shard-subscriptions-unobservable")
		if alerts[0].Class == "redis-shard-subscriber-missing" {
			t.Fatal("invalid settings generation retained a previously measured zero")
		}
	}
}

// A canceled settings comparison remains the adapter's lifecycle error.
func TestRedisShardSubscriptionsSettingsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings, requests := redisShardTestSettings(t, nil)
	settings.SettingsGenerationCheck = func(context.Context, SignalSettings) (bool, error) {
		cancel()
		return false, errors.New("private-loader-marker")
	}
	alerts, err := NewRedisShardSubscriptionsSignal().Run(ctx, settings)
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 || len(*requests) != 0 {
		t.Fatal("settings cancellation manufactured a verdict or admitted a source")
	}
	if strings.Contains(err.Error(), "private-loader-marker") {
		t.Fatal("settings cancellation exposed loader text")
	}
}

// Real Signal.Run observations cross the actual run loop, cadence gate and
// public notifier. Unknown resets the preceding complete absence; the shared
// gate clears only after two uninterrupted complete observations after the gap.
func TestRedisShardSubscriptionsUnknownCannotConsumeNotifierRecovery(t *testing.T) {
	for _, runs := range []int{4, 5, 6} {
		var run atomic.Int32
		settings, _ := redisShardTestSettings(t, func(request redisShardRequest, _ int, output string) (string, error) {
			if run.Load() > 0 && request.Phase == "counts" {
				output = redisShardTestFrame(request, time.Date(2026, 10, 19, 20, 0, 0, 0, time.UTC), true, []string{"c 0 0 0 1"})
			}
			return output, nil
		})
		settings.LogServiceBlocks["proxy"] = []string{"g1"}
		settings.SettingsGenerationCheck = func(context.Context, SignalSettings) (bool, error) {
			current := run.Load()
			return current < 2 || current >= 4, nil
		}
		webhook, webhookUrl := newFakeSlackWebhook(t)
		harness := newSlackTestHarness()
		notifier := NewSlackAlertNotifier(harness.settings(webhookUrl))
		t.Cleanup(func() { closeSlackNotifier(t, notifier) })
		postSignalRunsThroughTheGate(t, NewRedisShardSubscriptionsSignal(), settings, runs, func(index int) { run.Store(int32(index)) }, notifier)
		closeSlackNotifier(t, notifier)
		texts := webhook.posted()
		identity := "redis/shard-subscriptions · redis-shard-subscriber-missing · proxy-1/proxy/g1"
		wantCleared := 0
		if runs == 6 {
			wantCleared = 1
		}
		if countSlackPosts(texts, "[WARN] raised", identity) != 1 || countSlackPosts(texts, "[CLEARED]", identity) != wantCleared {
			t.Fatalf("unknown consumed notifier recovery at %d runs", runs)
		}
		if countSlackPosts(texts, "[WARN] raised", "redis/shard-subscriptions · redis-shard-subscriptions-unobservable · proxy-fleet") != 1 {
			t.Fatal("stale generation did not reach the public notifier as visibility")
		}
		for _, text := range texts {
			if strings.Contains(text, "proxy_client_") || strings.Contains(text, "192.0.2.") {
				t.Fatal("public notification exposed private source fields")
			}
		}
	}
}

// Raw protocol precision and complete framing remain independent of fixtures.
func TestRedisShardSubscriptionsFrameProtocol(t *testing.T) {
	now := time.Date(2026, 10, 19, 20, 0, 0, 0, time.UTC)
	request := redisShardRequest{Phase: "counts", Nonce: strings.Repeat("d", 32), Endpoint: 0, Endpoints: []redisShardEndpoint{{Address: "192.0.2.1", Port: 6380}}, Channels: []redisShardChannel{{Index: 0, Slot: 7, Owner: 0}}}
	for _, value := range []string{"0", "1", "9223372036854775807", "-1", "+1", "01", "1.0", "NaN", "9223372036854775808", "0 extra"} {
		out := redisShardTestFrame(request, now, true, []string{"c 0 7 0 " + value})
		_, err := parseRedisShardFrame(out, request, now, now)
		valid := value == "0" || value == "1" || value == "9223372036854775807"
		if (err == nil) != valid {
			t.Fatalf("integer grammar mismatch for case %q", value)
		}
	}
	good := redisShardTestFrame(request, now, true, []string{"c 0 7 0 0"})
	for _, out := range []string{strings.TrimSuffix(good, "\n"), good + "end 1\n", strings.Replace(good, "end 1", "end 0", 1), strings.Replace(good, "c 0 7", "c 1 7", 1)} {
		if _, err := parseRedisShardFrame(out, request, now, now); err == nil {
			t.Fatal("incomplete or foreign row admitted")
		}
	}
}

// CRC16 is used only to construct the requested native fixture slot.
func redisShardTestSlot(channel string) int {
	var crc uint16
	for _, value := range []byte(channel) {
		crc ^= uint16(value) << 8
		for range 8 {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return int(crc % 16384)
}

// Native fixtures execute the unchanged embedded producer with fake clients.
// The fake clients keep every address local and reveal actual argv in a private
// per-test file; no Redis, Docker daemon, HTTP service or production is used.
func redisShardNativeFixture(t *testing.T, phase, mode string) (redisShardRequest, []string, string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("native producer requires the configured Linux host primitives")
	}
	t.Logf("native fixture phase=%s case=%s", phase, mode)
	directory := t.TempDir()
	hostName, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(directory, "calls")
	for _, name := range []string{"redis-cli", "docker", "curl"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(redisShardNativeClientFixture), 0700); err != nil {
			t.Fatal(err)
		}
	}
	channel := "proxy_client_" + hostName + "_g1"
	slot := redisShardTestSlot(channel)
	owner := slot / 8192
	request := redisShardRequest{Phase: phase, Nonce: strings.Repeat("d", 32), Environment: "synthetic", Host: hostName, HostNames: []string{hostName}, Blocks: []string{"g1"}, Endpoints: []redisShardEndpoint{{Address: "192.0.2.1", Port: 6380}, {Address: "192.0.2.2", Port: 6381}}, Endpoint: owner, Channels: []redisShardChannel{{Index: 0, Name: channel, Slot: slot, Owner: owner}}}
	if phase == "allocations" {
		request.Channels = []redisShardChannel{}
	}
	if mode == "wrong-owner" {
		request.Endpoint = 1 - owner
		request.Channels[0].Owner = request.Endpoint
	}
	environment := []string{"PATH=" + directory + ":/usr/bin:/bin", "LC_ALL=C", "REDISCLI_AUTH=private-auth-marker", "RSC_MODE=" + mode, "RSC_CALLS=" + calls, "RSC_STATE=" + filepath.Join(directory, "state"), "RSC_HOST=" + hostName}
	return request, environment, calls
}

// Return native output/status separately so positive-looking failed stdout
// cannot accidentally become a successful fixture result.
func redisShardRunNative(t *testing.T, request redisShardRequest, environment []string) (redisShardFrame, error, string) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("required native Python fixture unavailable")
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, python, "-c", redisShardSourceProgram, string(encoded))
	command.Env = environment
	started := time.Now()
	output, err := command.CombinedOutput()
	finished := time.Now()
	if strings.Contains(string(output), "private-native-marker") || strings.Contains(string(output), "private-auth-marker") || strings.Contains(string(output), redisShardTestPrivateChannel(request)) {
		t.Fatal("native producer leaked private source bytes")
	}
	if err != nil {
		return redisShardFrame{}, err, string(output)
	}
	frame, err := parseRedisShardFrame(string(output), request, started, finished)
	return frame, err, string(output)
}

// A test-only representative private label is never empty in omission checks.
func redisShardTestPrivateChannel(self redisShardRequest) string {
	if len(self.Channels) == 0 {
		return "private-unrequested-channel"
	}
	return self.Channels[0].Name
}

// Test both successful presence boundaries and failed real native commands.
func TestRedisShardSubscriptionsNativeOwnerAndStatus(t *testing.T) {
	for _, mode := range []string{"zero", "one", "failed-positive", "moved", "wrong-channel", "negative", "cap", "stderr-cap", "slot-mismatch", "wrong-owner", "partial-map", "foreign-endpoint", "failed-map-positive", "map-change", "run-change"} {
		request, environment, calls := redisShardNativeFixture(t, "counts", mode)
		frame, err, _ := redisShardRunNative(t, request, environment)
		positive := mode == "zero" || mode == "one"
		if positive {
			if err != nil || len(frame.rows) != 1 || frame.rows[0].count != map[string]int64{"zero": 0, "one": 1}[mode] {
				t.Fatal("native healthy/broken boundary was not reached")
			}
		} else if err == nil && (len(frame.rows) != 1 || frame.rows[0].count != -1) {
			t.Fatal("native failed evidence admitted a zero or presence")
		}
		called, readErr := os.ReadFile(calls)
		if readErr != nil {
			t.Fatal(readErr)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(called)), "\n") {
			if strings.Contains(line, "redis-cli") && !strings.Contains(line, "-h 127.0.0.1") {
				t.Fatal("native read escaped configured loopback route")
			}
			if strings.Contains(line, "PUBLISH") || strings.Contains(line, "-c ") {
				t.Fatal("mutation or redirection entered native census")
			}
		}
		if mode == "wrong-owner" && strings.Contains(string(called), "SHARDNUMSUB") {
			t.Fatal("wrong-owner zero was queried before authority check")
		}
	}
}

// A real allocation producer binds running/readiness identity and its second
// inspect, rejects duplicates and refuses to equate nonrunning with loss.
func TestRedisShardSubscriptionsNativeAllocations(t *testing.T) {
	for _, mode := range []string{"active", "inactive", "duplicate", "inspect-failed-positive", "allocation-change", "wrong-host", "wrong-block", "not-ready", "docker-failed-empty", "docker-cap"} {
		request, environment, _ := redisShardNativeFixture(t, "allocations", mode)
		frame, err, _ := redisShardRunNative(t, request, environment)
		if mode == "active" {
			if err != nil || !frame.complete || len(frame.rows) != 1 || frame.rows[0].state != "active" {
				t.Fatal("native active allocation control did not reach its assertion")
			}
		} else if mode == "inactive" {
			if err != nil || !frame.complete || frame.rows[0].state != "inactive" {
				t.Fatal("complete empty native inventory was not distinguished")
			}
		} else if err == nil && (frame.complete || frame.rows[0].state != "unknown") {
			t.Fatal("invalid allocation became active or complete")
		}
	}
}

// Scripted native clients are deliberately independent of the source parser.
const redisShardNativeClientFixture = `#!/usr/bin/env python3
import binascii, json, os, signal, sys
name = os.path.basename(sys.argv[0])
args = sys.argv[1:]
mode = os.environ['RSC_MODE']
host = os.environ['RSC_HOST']
with open(os.environ['RSC_CALLS'],'a') as calls:
    calls.write(name+' '+' '.join(args)+'\n')
def tick(key):
    path = os.environ['RSC_STATE']+'-'+key
    try:
        with open(path) as value: count = int(value.read())+1
    except FileNotFoundError:
        count = 1
    with open(path,'w') as value: value.write(str(count))
    return count
if name == 'redis-cli':
    assert os.environ['REDISCLI_AUTH'] == 'private-auth-marker'
    if mode == 'block':
        signal.alarm(4)
        with open(os.environ['RSC_READY'], 'w') as ready:
            ready.write(str(os.getpid())+'\n'); ready.flush()
        while True: signal.pause()
    assert args[:4] == ['-2','--raw','-h','127.0.0.1'] and args[4] == '-p'
    endpoint = int(args[5])-6380
    operation = args[6:]
    if operation == ['INFO','server']:
        count = tick('run')
        print('# Server\r\nrun_id:'+('c' if mode == 'run-change' and count > 2 else 'd')*40+'\r')
    elif operation == ['CLUSTER','INFO']:
        print('cluster_state:ok\r\ncluster_slots_assigned:16384\r\ncluster_slots_ok:16384\r\ncluster_slots_pfail:0\r\ncluster_slots_fail:0\r\ncluster_known_nodes:2\r\ncluster_current_epoch:7\r')
    elif operation == ['CLUSTER','NODES']:
        count = tick('nodes')
        for i in range(2):
            if mode == 'partial-map' and i == 1: continue
            ip = '203.0.113.9' if mode == 'foreign-endpoint' and i == 1 else '192.0.2.'+str(i+1)
            flags = 'myself,master' if i == endpoint else 'master'
            epoch = 6 if mode == 'map-change' and count > 1 else 7
            print(('a' if i == 0 else 'b')*40+' '+ip+':'+str(6380+i)+'@'+str(16380+i)+' '+flags+' - 0 0 '+str(epoch)+' connected '+str(i*8192)+'-'+str((i+1)*8192-1))
        if mode == 'failed-map-positive': sys.exit(7)
    elif operation[:2] == ['CLUSTER','KEYSLOT']:
        slot = binascii.crc_hqx(operation[2].encode(),0)%16384
        print((slot+1)%16384 if mode == 'slot-mismatch' else slot)
    elif operation[:2] == ['PUBSUB','SHARDNUMSUB']:
        if mode == 'cap': print('private-native-marker'*18000)
        elif mode == 'stderr-cap': sys.stderr.write('private-native-marker'*1000)
        elif mode == 'moved': print('MOVED 1 private-native-marker:1234')
        else:
            print('private-native-marker' if mode == 'wrong-channel' else operation[2])
            print(-1 if mode == 'negative' else (1 if mode == 'one' else 0))
        if mode == 'failed-positive': sys.exit(9)
    else: sys.exit(126)
elif name == 'docker':
    if args[0] == 'ps':
        if mode == 'docker-failed-empty': sys.exit(7)
        if mode == 'docker-cap': print('private-native-marker'*5000); sys.exit(0)
        if mode != 'inactive':
            print('1'*64+' synthetic-proxy-g1-v1')
            if mode == 'duplicate': print('2'*64+' synthetic-proxy-g1-v2')
    elif args[0] == 'inspect':
        count = tick('inspect')
        container = args[-1]
        metadata = [container,'/synthetic-proxy-g1-'+('v2' if container[0] == '2' else 'v1'),True,False,False,99 if mode == 'allocation-change' and count > 1 else 98,'2026-10-19T19:00:00.000000000Z',0]
        for value in metadata: print(json.dumps(value))
        values = {'WARP_ENV':'synthetic','WARP_SERVICE':'proxy','WARP_HOST':'private-native-marker' if mode == 'wrong-host' else host,'WARP_BLOCK':'private-native-marker' if mode == 'wrong-block' else 'g1','WARP_PORTS':'80:18080,8080:18081','WARP_VERSION':'v1'}
        for key,value in values.items(): print(key+'='+value)
        if mode == 'inspect-failed-positive': sys.exit(8)
    else: sys.exit(126)
elif name == 'curl':
    assert args[-1] == 'http://127.0.0.1:18080/status'
    if mode == 'not-ready': print('private-native-marker'); sys.exit(22)
    print(json.dumps({'status':'ok','host':host,'block':'g1','service':'proxy','version':'v1','client_address':'private-native-marker'}))
else:
    sys.exit(126)
`
