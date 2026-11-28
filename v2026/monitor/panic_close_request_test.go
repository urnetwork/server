// Synthetic stack controls for retaining the public Redis adapter used by a
// close-request taker without labeling work outside that call.
package monitor

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Generates ErrorJson function and source pairs with nonpublic fixture values.
func closeTakeControlLine(t *testing.T, symbols ...string) string {
	t.Helper()
	if len(symbols)%2 != 0 {
		t.Fatal("expected function and source file pairs")
	}
	var stack []string
	for i := 0; i < len(symbols); i += 2 {
		stack = append(stack, "github.com/urnetwork/"+symbols[i]+"(0xabcde)", "/synthetic-stack/"+symbols[i+1]+":34 +0x99")
	}
	data, err := json.Marshal(map[string]any{"error": "*syntheticPrivate.Error=synthetic-close-detail", "stack": stack})
	if err != nil {
		t.Fatal(err)
	}
	return "Unexpected error: " + string(data)
}

// The same normalized owner has two supported release callsites.
func TestCloseTakeControlBothReleaseAdapters(t *testing.T) {
	for _, module := range []string{"server", "server/v2026"} {
		owner := module + "/model.TakeClientCloseRequests"
		for _, adapter := range []string{"Redis", "RedisDoOnce"} {
			line := closeTakeControlLine(t, module+"."+adapter, "redis.go", owner, "client_close_model.go")
			fields := strings.Fields(panicLogSample(line))
			for _, want := range []string{"owner=" + owner, "error_type=unknown", "sqlstate=unknown", "redis_adapter=" + adapter} {
				if !slices.Contains(fields, want) {
					t.Errorf("missing %s for direct %s call", want, adapter)
				}
			}
			for _, private := range []string{"synthetic-close-detail", "synthetic-stack", "0xabcde", "client_close_model.go"} {
				if strings.Contains(strings.Join(fields, " "), private) {
					t.Fatal("nonpublic stack material was retained")
				}
			}
		}
	}
}

// A stack hint from a different call, module or file cannot annotate the selected take.
func TestCloseTakeControlRequiresDirectAdapterAndCaller(t *testing.T) {
	const take = "server/v2026/model.TakeClientCloseRequests"
	const adapter = "server/v2026.RedisDoOnce"
	for _, test := range []struct {
		name  string
		pairs []string
	}{
		{name: "owner alone", pairs: []string{take, "client_close_model.go"}},
		{name: "adapter closure alone", pairs: []string{adapter + ".func1", "redis.go", take, "client_close_model.go"}},
		{name: "adapter lookalike", pairs: []string{adapter + "Other", "redis.go", take, "client_close_model.go"}},
		{name: "wrong adapter source", pairs: []string{adapter, "other.go", take, "client_close_model.go"}},
		{name: "wrong take source", pairs: []string{adapter, "redis.go", take, "other.go"}},
		{name: "mixed modules", pairs: []string{"server.Redis", "redis.go", take, "client_close_model.go"}},
		{name: "other owner", pairs: []string{adapter, "redis.go", take + "Other", "client_close_model.go"}},
		{name: "other selected owner", pairs: []string{take + "Other", "client_close_model.go", adapter, "redis.go", take, "client_close_model.go"}},
		{name: "standalone closure caller", pairs: []string{adapter, "redis.go", take + ".func1", "client_close_model.go"}},
		{name: "gap between frames", pairs: []string{adapter, "redis.go", "server/v2026.RedisWithDeadline", "redis.go", take, "client_close_model.go"}},
		{name: "unbound callback", pairs: []string{take + ".func1", "client_close_model.go", "server/v2026.SyntheticBridge", "other.go", adapter, "redis.go", take, "client_close_model.go"}},
		{name: "unrelated outer take", pairs: []string{take, "client_close_model.go", "server/v2026.SyntheticBridge", "other.go", adapter, "redis.go", take, "client_close_model.go"}},
		{name: "competing adapters", pairs: []string{"server/v2026.Redis", "redis.go", adapter, "redis.go", take, "client_close_model.go"}},
		{name: "competing takes", pairs: []string{adapter, "redis.go", take, "client_close_model.go", take, "client_close_model.go"}},
	} {
		if sample := panicLogSample(closeTakeControlLine(t, test.pairs...)); strings.Contains(sample, "redis_adapter=") {
			t.Errorf("unproved adapter retained for %s", test.name)
		}
	}
}

// A callback can establish the outer direct call only through its own wrappers.
func TestCloseTakeControlCallbackBridge(t *testing.T) {
	for _, module := range []string{"server", "server/v2026"} {
		take := module + "/model.TakeClientCloseRequests"
		for _, adapter := range []string{"Redis", "RedisDoOnce"} {
			call := module + "." + adapter
			line := closeTakeControlLine(t, take+".func1", "client_close_model.go", module+".redisWithClient.func1", "redis.go",
				module+".redisWithClient", "redis.go", call+".func1", "redis.go", call, "redis.go", take, "client_close_model.go")
			want := "panic owner=" + take + " error_type=unknown sqlstate=unknown redis_adapter=" + adapter
			if panicLogSample(line) != want {
				t.Fatal("the intact callback lost its adapter")
			}
			for _, change := range [][2]string{
				{take + ".func1(", take + ".func2("},
				{module + ".redisWithClient.func1(", module + ".redisWithClient.func2("},
				{module + ".redisWithClient(", module + ".Trace("},
				{call + ".func1(", module + ".redisWithClient.func1("},
				{"/synthetic-stack/client_close_model.go:", "/synthetic-stack/other.go:"},
			} {
				changed := strings.Replace(line, change[0], change[1], 1)
				if changed == line || strings.Contains(panicLogSample(changed), "redis_adapter=") {
					t.Fatal("a different callback path borrowed the outer adapter")
				}
			}
		}
	}
}

// Qualified source paths can attest the package. A file basename in another
// module, malformed frame, or incomplete ErrorJson supplies no adapter.
func TestCloseTakeControlSourceAndRecordBounds(t *testing.T) {
	const take = "server/v2026/model.TakeClientCloseRequests"
	line := closeTakeControlLine(t, "server/v2026.Redis", "redis.go", take, "client_close_model.go")
	for _, qualified := range []string{"github.com/urnetwork/server/v2026/", "/synthetic-cache/github.com/urnetwork/server/v2026@v2026.1.2/"} {
		withPath := strings.ReplaceAll(line, "/synthetic-stack/redis.go", qualified+"redis.go")
		withPath = strings.ReplaceAll(withPath, "/synthetic-stack/client_close_model.go", qualified+"model/client_close_model.go")
		if !strings.HasSuffix(panicLogSample(withPath), " redis_adapter=Redis") {
			t.Fatal("matching qualified source did not identify the adapter")
		}
		if strings.Contains(panicLogSample(strings.Replace(withPath, qualified+"redis.go", qualified+"other/redis.go", 1)), "redis_adapter=") {
			t.Fatal("a different source package supplied the adapter")
		}
	}
	for _, changed := range []string{
		line[:len(line)-1],
		strings.Repeat("x", panicLogMaxBytes) + line,
		strings.Replace(line, `"stack":`, `"stack":[],"stack":`, 1),
		strings.Replace(line, "synthetic-close-detail", string([]byte{0xff}), 1),
		strings.Replace(line, "redis.go:34", "redis.go:0", 1),
		strings.Replace(line, "redis.go:34", "redis.go:1000001", 1),
		strings.Replace(line, ".Redis(0xabcde)", ".Redis-fm(0xabcde)", 1),
		strings.Replace(line, ".Redis(0xabcde)", ".Redis(0xabcde", 1),
		strings.Replace(line, ".Redis(0xabcde)", ".Redis(0xabcde)other()", 1),
	} {
		if changed == line || strings.Contains(panicLogSample(changed), "redis_adapter=") {
			t.Fatal("malformed, ambiguous or oversized evidence supplied an adapter")
		}
	}
}

// An already expired context fails before opening a Redis connection. The
// native Connect recovery still identifies the current taker's direct adapter.
func TestCloseTakeNativePredispatchRecovery(t *testing.T) {
	previous := connect.DefaultLogger()
	logger := &proxyOperationTestLogger{Logger: previous, warnings: make(chan string, 1)}
	connect.SetDefaultLogger(logger)
	defer connect.SetDefaultLogger(previous)
	ctx, cancel := context.WithDeadline(t.Context(), time.Unix(1, 0))
	defer cancel()
	result := connect.HandleError(func() { model.TakeClientCloseRequests(ctx, server.Id{}, 1) })
	if result != context.DeadlineExceeded {
		t.Fatal("expected the pre-dispatch deadline")
	}
	select {
	case record := <-logger.warnings:
		want := "panic owner=server/model.TakeClientCloseRequests error_type=context.deadlineExceededError sqlstate=unknown redis_adapter=RedisDoOnce"
		if panicLogSample(record) != want {
			t.Fatal("the current taker lost its same-record adapter in native recovery")
		}
	default:
		t.Fatal("native recovery did not emit the pre-dispatch warning")
	}
}

// Service and owner buckets keep their independent first samples while later
// events with a different adapter increase the same owner count.
func TestCloseTakeControlPreservesOverlappingFirstExemplars(t *testing.T) {
	const take = "server/v2026/model.TakeClientCloseRequests"
	tailer := newLogTailer("connect", nil)
	tailer.classify(closeTakeControlLine(t, "server/v2026/model.SyntheticOther", "other.go"))
	tailer.classify(closeTakeControlLine(t, "server/v2026.RedisDoOnce", "redis.go", take, "client_close_model.go"))
	for range 4 {
		tailer.classify(closeTakeControlLine(t, "server/v2026.Redis", "redis.go", take, "client_close_model.go"))
	}
	pageCounts := map[string]int{}
	for _, f := range tailer.drainWindow() {
		if f.class != "panic" || f.healthy {
			continue
		}
		a := alertFromFinding(syntheticSettings(nil), "1.5", "log-errors", "Log error-class rates", f)
		pageCounts[a.Frame]++
		if a.Severity != SeverityPage || a.SignalID != "logs/panic" || a.Target != "connect" {
			t.Fatal("existing Connect panic page identity or severity changed")
		}
		switch a.Frame {
		case "":
			if !strings.Contains(a.Observed, "rate=6/min") || strings.Contains(a.Evidence, "redis_adapter=") {
				t.Fatal("aggregate lost its count or borrowed a later adapter")
			}
		case take:
			if !strings.Contains(a.Observed, "rate=5/min") || !strings.Contains(a.Evidence, "redis_adapter=RedisDoOnce") {
				t.Fatal("owner lost its first adapter or its mixed-adapter count")
			}
		default:
			t.Fatal("an unexpected owner gained a page")
		}
	}
	if len(pageCounts) != 2 || pageCounts[""] != 1 || pageCounts[take] != 1 {
		t.Fatalf("overlapping service and owner pages changed: %v", pageCounts)
	}
}
