package monitor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// Produces the same two-field ErrorJson as HandleError without exposing any
// real request, key or host. Each path and argument is synthetic and private.
func proxyAdmissionPanicTestLine(module, caller, callerFile string, err error) string {
	stack := []string{
		"goroutine 5 [running]:",
		"github.com/urnetwork/" + module + ".proxyAdmissionRedis(synthetic-private-argument)",
		"/synthetic-private-path/proxy_admission_model.go:44 +0x89",
		"github.com/urnetwork/" + module + "." + caller + "(synthetic-private-argument)",
		"/synthetic-private-path/" + callerFile + ":123 +0x45",
	}
	return "Unexpected error: " + server.ErrorJson(err, []byte(strings.Join(stack, "\n")))
}

func TestProxyAdmissionPanicRetainsDirectRedisCaller(t *testing.T) {
	for _, module := range []string{"server/model", "server/v2026/model"} {
		for _, test := range []struct{ caller, file string }{
			{"GetProxyDeviceConfig", "network_client_proxy_model.go"},
			{"getProNetworkCached", "pro_model.go"},
			{"setProNetworkCached", "pro_model.go"},
		} {
			tailer := newLogTailer("api", nil)
			line := proxyAdmissionPanicTestLine(module, test.caller, test.file, context.DeadlineExceeded)
			for range 5 {
				tailer.classify(line)
			}
			frame := module + ".proxyAdmissionRedis"
			want := "sample: panic owner=" + frame + " error_type=context.deadlineExceededError sqlstate=unknown redis_caller=" + test.caller
			alerts := panicOwnerTestAlerts(t, tailer)
			if len(alerts) != 2 {
				t.Fatal("five diagnostics must reach the aggregate and framed thresholds")
			}
			for _, bucket := range []string{"", frame} {
				alert := alerts[bucket]
				if !strings.Contains(alert.Observed, "rate=5/min") || !strings.Contains(alert.Evidence, want) {
					t.Fatal("same diagnostic lost its direct Redis caller or typed deadline")
				}
				requireAlertOmits(t, alert, "synthetic-private")
			}
		}
	}
}

func TestProxyAdmissionPanicRequiresAdjacentSourcePair(t *testing.T) {
	line := proxyAdmissionPanicTestLine("server/model", "getProNetworkCached", "pro_model.go", errors.New("context deadline exceeded"))
	controls := map[string]string{
		"wrong wrapper source": strings.Replace(line, "proxy_admission_model.go:", "unrelated.go:", 1),
		"wrong caller source":  strings.Replace(line, "pro_model.go:", "unrelated.go:", 1),
		"wrong module":         strings.Replace(line, "server/model.getProNetworkCached", "server/v2026/model.getProNetworkCached", 1),
		"unrelated caller":     strings.Replace(line, ".getProNetworkCached(", ".UnknownCaller(", 1),
		"closure":              strings.Replace(line, ".proxyAdmissionRedis(", ".proxyAdmissionRedis.func1(", 1),
		"caller closure":       strings.Replace(line, ".getProNetworkCached(", ".getProNetworkCached.func1(", 1),
		"intervening frame":    strings.Replace(line, "github.com/urnetwork/server/model.getProNetworkCached", "runtime.other()\",\"/runtime/file.go:1\",\"github.com/urnetwork/server/model.getProNetworkCached", 1),
	}
	for name, input := range controls {
		if strings.Contains(panicLogSample(input), "redis_caller=") {
			t.Errorf("%s gained caller authority", name)
		}
	}
	if sample := panicLogSample(line); !strings.Contains(sample, "error_type=*errors.errorString") || strings.Contains(sample, "context.deadlineExceededError") {
		t.Fatal("deadline message acquired a typed context cause")
	}
	for _, invalid := range []string{"context.deadlineExceededError=private text", "context.deadlineExceededError"} {
		if sample := panicLogSample(strings.Replace(line, "*errors.errorString=context deadline exceeded", invalid, 1)); !strings.Contains(sample, "error_type=unknown") {
			t.Fatal("invalid producer error acquired typed deadline authority")
		}
	}
}
