package monitor

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// Both callback adapters share the lower retry and rethrow wrapper. None is
// the application caller, including closure and trimpath stack forms.
func TestPanicOwnerRedisWrappersRetainCaller(t *testing.T) {
	for _, module := range []string{"server", "server/v2026"} {
		for _, wrapper := range []string{"redisWithClient", "redisWithClient.func1.1", "Redis", "Redis.func1", "RedisDoOnce", "RedisDoOnce.func1"} {
			for _, source := range []string{"/synthetic-private-path/redis.go:12345 +0x99", "github.com/urnetwork/" + module + "/redis.go:12345 +0x99"} {
				caller := module + "/model.SyntheticRedisWork"
				line := panicOwnerTestLine(t, "*syntheticprivate.Error=synthetic-secret",
					"github.com/urnetwork/"+module+"."+wrapper,
					"github.com/urnetwork/"+caller+".func3")
				line = strings.ReplaceAll(line, "/synthetic-private-path/owner.go:12345 +0x99", source)
				observation := parsePanicLogObservation(line)
				if observation.owner != caller || observation.errorType != "unknown" || observation.sqlstate != "unknown" {
					t.Fatalf("Redis callback wrapper hid the application caller: got=%q want=%q wrapper=%s module=%s", observation.owner, caller, wrapper, module)
				}
			}
		}
	}
}

// The aggregate's first exemplar is independent of a later owner bucket.
// Correcting that bucket cannot assign its error to every aggregate event.
func TestPanicOwnerRedisWrappersPreservePagesAndSamples(t *testing.T) {
	tailer := newLogTailer("api", nil)
	for range 2 {
		tailer.classify(panicOwnerTestLine(t, "*errors.errorString=synthetic-secret",
			"github.com/urnetwork/server/v2026/controller.SyntheticOther"))
	}
	line := panicOwnerTestLine(t, "*syntheticprivate.Error=synthetic-secret SELECT private synthetic-customer 192.0.2.88",
		"github.com/urnetwork/server/v2026.redisWithClient.func1.1",
		"github.com/urnetwork/server/v2026.redisWithClient",
		"github.com/urnetwork/server/v2026.RedisDoOnce.func1",
		"github.com/urnetwork/server/v2026.RedisDoOnce",
		"github.com/urnetwork/server/v2026/model.SyntheticRedisWork")
	for range 9 {
		tailer.classify(line)
	}
	caller := "server/v2026/model.SyntheticRedisWork"
	if tailer.classCounts["panic"] != 11 || tailer.classCounts["panic\x00"+caller] != 9 {
		t.Fatal("Redis wrapper correction changed overlapping event counts")
	}
	alerts := panicOwnerTestAlerts(t, tailer)
	if len(alerts) != 2 || !strings.Contains(alerts[""].Observed, "rate=11/min") ||
		!strings.Contains(alerts[""].Evidence, "owner=server/v2026/controller.SyntheticOther error_type=*errors.errorString") {
		t.Fatal("Redis wrapper correction lost the aggregate or replaced its first sample")
	}
	callerAlert := alerts[caller]
	if !strings.Contains(callerAlert.Observed, "rate=9/min") ||
		!strings.Contains(callerAlert.Evidence, "owner="+caller+" error_type=unknown sqlstate=unknown") {
		t.Fatal("Redis caller page borrowed a sample or invented an error cause")
	}
	for _, alert := range alerts {
		for _, want := range []string{"counts overlap", "changed owner after parser correction is not recovery"} {
			if !strings.Contains(alert.Markdown(), want) {
				t.Fatalf("Redis wrapper authority qualifier missing %q", want)
			}
		}
		for _, private := range []string{"redisWithClient", "RedisDoOnce", "syntheticprivate.Error", "SELECT private", "synthetic-secret", "synthetic-customer", "192.0.2.88"} {
			requireAlertOmits(t, alert, private)
		}
	}
}

// Wrapper-only, malformed and unsupported stacks retain the original page.
// An unknown inner function cannot borrow an outer caller's ownership.
func TestPanicOwnerRedisWrappersUnknownStayAggregate(t *testing.T) {
	valid := panicOwnerTestLine(t, "*syntheticprivate.Error=synthetic-secret",
		"github.com/urnetwork/server/v2026.redisWithClient",
		"github.com/urnetwork/server/v2026.Redis",
		"github.com/urnetwork/server/v2026/model.SyntheticRedisWork")
	for _, line := range []string{
		panicOwnerTestLine(t, "*syntheticprivate.Error=synthetic-secret", "github.com/urnetwork/server/v2026.redisWithClient.func1.1", "github.com/urnetwork/server/v2026.Redis"),
		panicOwnerTestLine(t, "*syntheticprivate.Error=synthetic-secret", "github.com/urnetwork/server/v2026.redisWithClient", "github.com/urnetwork/unlisted.SyntheticUnknown", "github.com/urnetwork/server/v2026/model.SyntheticOuter"),
		panicOwnerTestLine(t, "*syntheticprivate.Error=synthetic-secret", "github.com/urnetwork/server/v2026.redisWithClient", "github.com/urnetwork/server/v2026/model.SyntheticUnknown[unsupported]", "github.com/urnetwork/server/v2026/model.SyntheticOuter"),
		valid[:len(valid)-1],
		strings.Replace(valid, "/synthetic-private-path/owner.go:12345 +0x99", "synthetic-secret", 1),
		strings.Replace(valid, `"error":`, `"error":"synthetic-secret","error":`, 1),
		strings.Repeat("x", panicLogMaxBytes) + valid,
	} {
		tailer := newLogTailer("api", nil)
		for range 5 {
			tailer.classify(line)
		}
		alerts := panicOwnerTestAlerts(t, tailer)
		if len(alerts) != 1 || !strings.Contains(alerts[""].Observed, "rate=5/min") || !strings.Contains(alerts[""].Evidence, "owner=unknown") {
			t.Fatal("unproved Redis caller gained an owner or lost aggregate visibility")
		}
	}
}

func TestPanicOwnerRedisMalformedSourceStopsAttribution(t *testing.T) {
	for _, wrapper := range []string{"redisWithClient", "Redis", "RedisDoOnce"} {
		body, err := json.Marshal(map[string]any{
			"error": "*syntheticprivate.Error=synthetic-secret",
			"stack": []string{
				"github.com/urnetwork/server/v2026." + wrapper + "(0x12345678)",
				"github.com/urnetwork/server/v2026/redis.go:unknown",
				"github.com/urnetwork/server/v2026/model.SyntheticOuter(0x12345678)",
				"github.com/urnetwork/server/v2026/model/model.go:12345",
			},
		})
		if err != nil {
			t.Fatal("synthetic fixture encoding failed")
		}
		if owner := panicLogOwner("Unexpected error: " + string(body)); owner != "" {
			t.Fatal("malformed Redis wrapper borrowed a later caller")
		}
	}
}

// Short-name lookalikes and specific deadline/authentication/lifecycle
// boundaries remain owners. A function named recover is not a generic wrapper.
func TestPanicOwnerRedisLookalikesAndPolicyBoundariesRemainOwners(t *testing.T) {
	for _, function := range []string{
		"server/model.redisWithClient", "server/v2026/model.Redis", "connect.RedisDoOnce",
		"server.redisWithClientOther", "server.SyntheticClient.Redis", "server.RedisAuth",
		"server/v2026.RedisWithDeadline", "server/v2026/connect.ConnectionAnnounce.recoverLiveCall",
	} {
		line := panicOwnerTestLine(t, "*syntheticprivate.Error=synthetic-secret", "github.com/urnetwork/"+function, "github.com/urnetwork/server/model.SyntheticOuter")
		if owner := panicLogOwner(line); owner != function {
			t.Fatal("Redis lookalike or specific policy boundary lost its ownership")
		}
	}
}

func TestPanicOwnerRedisThresholdAndHealthyControls(t *testing.T) {
	for _, count := range []int{0, 4} {
		tailer := newLogTailer("api", nil)
		line := panicOwnerTestLine(t, "*syntheticprivate.Error=synthetic-secret", "github.com/urnetwork/server/v2026.redisWithClient")
		for range count {
			tailer.classify(line)
		}
		if alerts := panicOwnerTestAlerts(t, tailer); len(alerts) != 0 {
			t.Fatal("Redis wrapper correction changed threshold or healthy-window behavior")
		}
	}
}

// A Redis wrapper permits the finite stats class only with its reviewed source
// file and the existing first business caller. Neither a literal nor a wrapper
// supplies ownership when the remaining caller is absent or foreign.
func TestPanicOwnerRedisFiniteClassifierRequiresSourcePairs(t *testing.T) {
	for _, version := range []string{"", "/v2026"} {
		for _, wrapper := range []string{"redisWithClient.func1.1", "Redis.func1", "RedisDoOnce"} {
			for _, validFile := range []bool{true, false} {
				stack := panicProviderTestStack(version)
				file := "redis.go"
				if !validFile {
					file = "synthetic_other.go"
				}
				pair := []string{"github.com/urnetwork/server" + version + "." + wrapper + "(0x12345678)", "/synthetic-private-path/" + file + ":12345"}
				stack = append(stack[:9], append(pair, stack[9:]...)...)
				line := panicProviderTestLine(errors.New("provider egress stats refresh in progress"), stack)
				classified := strings.Contains(panicLogSample(line), "error_class=provider_egress_refresh_busy")
				if classified != validFile {
					t.Fatal("finite classifier ignored the Redis wrapper's source-file contract")
				}
				panicProviderRequirePrivate(t, panicLogSample(line))
				if validFile {
					wrapperOnly := panicProviderTestLine(errors.New("provider egress stats refresh in progress"), stack[:11])
					if strings.Contains(panicLogSample(wrapperOnly), "error_class=provider_egress_") {
						t.Fatal("Redis wrapper alone invented a finite source owner")
					}
					foreign := strings.Replace(line, "statsRefreshProviderEgress", "SyntheticOther", 1)
					if strings.Contains(panicLogSample(foreign), "error_class=provider_egress_") {
						t.Fatal("Redis wrapper gave a foreign caller the stats literal")
					}
				}
			}
		}
	}
}
