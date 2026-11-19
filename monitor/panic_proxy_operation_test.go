// Preserves same-record Redis ownership in both native panic formats.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Captures the native recovery record without copying it to test output.
type proxyOperationTestLogger struct {
	connect.Logger
	warnings chan string
}

// Keeps one warning so a concurrent producer cannot block.
func (self *proxyOperationTestLogger) Warningf(format string, args ...any) {
	select {
	case self.warnings <- fmt.Sprintf(format, args...):
	default:
	}
}

// The original Connect recovery must keep its returned error, handler and
// direct Redis caller when the unmarked device-config budget has expired.
func TestProxyRedisOperationNativeConnectRecovery(t *testing.T) {
	previous := connect.DefaultLogger()
	logger := &proxyOperationTestLogger{Logger: previous, warnings: make(chan string, 1)}
	connect.SetDefaultLogger(logger)
	defer connect.SetDefaultLogger(previous)

	ctx, cancel := context.WithDeadline(server.WithoutPostgres(t.Context()), time.Unix(1, 0))
	defer cancel()
	var handled error
	result := connect.HandleError(func() {
		model.GetProxyDeviceConfig(ctx, server.NewId())
	}, func(err error) { handled = err })
	if handled == nil || result != handled || !errors.Is(handled, context.DeadlineExceeded) ||
		fmt.Sprintf("%T", result) != "*errors.joinError" || server.PacketPostgresAttempts(ctx) != 0 {
		t.Fatal("native recovery or pre-dispatch error changed")
	}
	select {
	case record := <-logger.warnings:
		want := "panic owner=server/model.proxyAdmissionRedis error_type=unknown sqlstate=unknown redis_caller=GetProxyDeviceConfig redis_operation=device_config_read"
		if panicLogSample(record) != want {
			t.Fatal("native Connect recovery lost the direct caller or device configuration operation")
		}
	default:
		t.Fatal("native Connect recovery did not emit the expected warning")
	}
}

// Builds a synthetic stack using the application's source pairs. All argument
// values, source prefixes and error text are canaries excluded from the sample.
func proxyPlanOperationStack(module, caller string) string {
	frame := func(module, symbol, file string) string {
		return "github.com/urnetwork/" + module + "." + symbol + "(private-argument)\n/private-path/" + file + ":17 +0x10\n"
	}
	stack := frame(module, "proxyAdmissionRedis", "proxy_admission_model.go") + frame(module, caller, "pro_model.go")
	if caller == "setProNetworkCached" {
		for _, name := range []string{"storeProNetwork", "refreshProNetworkInConn", "refreshProNetwork"} {
			stack += frame(module, name, "pro_model.go")
		}
	}
	return stack + frame(module, "IsProNetwork", "pro_model.go") +
		frame(module, "(*dbProxyFeatureUsePlans).plans", "proxy_feature_use_model.go") +
		frame(module, "(*ProxyFeatureUses).countMarks.func1", "proxy_feature_use_model.go") +
		frame(strings.TrimSuffix(module, "/model"), "HandleError", "trace.go") +
		frame(module, "(*ProxyFeatureUses).countMarks", "proxy_feature_use_model.go") +
		frame(module, "(*ProxyFeatureUses).flush", "proxy_feature_use_model.go")
}

// Both stock error formatters retain the same original source chain. Require
// the planner and flush independently, even in the presence of lookalike text.
func TestProxyRedisOperationRequiresAccountingLineage(t *testing.T) {
	for _, module := range []string{"server/model", "server/v2026/model"} {
		for _, caller := range []string{"getProNetworkCached", "setProNetworkCached"} {
			original := proxyPlanOperationStack(module, caller)
			for _, tc := range []struct{ label, stack, operation string }{
				{"flush", original, "feature_use_flush_plan"},
				{"planner", strings.Replace(original, ".countMarks.func1(", ".unrelated(", 1), "feature_use_plan_lookup"},
				{"wrong recovery", strings.Replace(original, ".HandleError(", ".HandleError2(", 1), "feature_use_plan_lookup"},
				{"colliding recovery", strings.Replace(original, strings.TrimSuffix(module, "/model")+".HandleError", "connect.HandleError", 1), "feature_use_plan_lookup"},
				{"truncated flush", strings.Split(original, ".flush(")[0], "feature_use_plan_lookup"},
				{"wrong flush source", strings.Replace(original, ".flush(private-argument)\n/private-path/proxy_feature_use_model.go:17", ".flush(private-argument)\n/private-path/other.go:17", 1), "feature_use_plan_lookup"},
				{"truncated planner", strings.Split(original, ".plans(")[0], ""},
				{"other caller", strings.Replace(original, ".IsProNetwork(", ".IsProNetworkFresh(", 1), ""},
				{"wrong planner source", strings.Replace(original, "/proxy_feature_use_model.go:17", "/other.go:17", 1), ""},
				{"invalid planner line", strings.Replace(original, "/proxy_feature_use_model.go:17", "/proxy_feature_use_model.go:0", 1), ""},
				{"wrong planner module", strings.Replace(original, module+".(*dbProxyFeatureUsePlans)", "server/other.(*dbProxyFeatureUsePlans)", 1), ""},
				{"unrelated intervening frame", strings.Replace(original, "github.com/urnetwork/"+module+".(*dbProxyFeatureUsePlans)", "runtime.unrelated()\n/private-path/unrelated.go:3\ngithub.com/urnetwork/"+module+".(*dbProxyFeatureUsePlans)", 1), ""},
			} {
				for _, format := range []func(any, []byte) string{server.ErrorJson, connect.ErrorJson} {
					line := "Unexpected error: " + format(errors.Join(context.DeadlineExceeded, errors.New("private-error redis_operation=feature_use_flush_plan")), []byte(tc.stack))
					want := "panic owner=" + module + ".proxyAdmissionRedis error_type=unknown sqlstate=unknown redis_caller=" + caller
					if tc.operation != "" {
						want += " redis_operation=" + tc.operation
					}
					if sample := panicLogSample(line); sample != want || len(sample) > 200 {
						t.Errorf("%s %s %s lost bounded source evidence", module, caller, tc.label)
					}
				}
			}
		}
	}
}
