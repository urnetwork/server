package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Root/Low own the attested local Redis fixture. Only this unique synthetic
// key is mutated; the production key is never set/deleted by these controls.
// The exact production EVAL_RO executes in Redis, then feeds the public signal.
// Unsupported EVAL_RO is an unmet gate, never an optional skipped control.
func TestContractDegradationActualRedisSource(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		key := "monitor:test:contract-degradation:" + server.NewId().String()
		for _, config := range []struct {
			raw  string
			want ContractDegradationConfigStatus
		}{
			{raw: "enabled: true\n", want: ContractDegradationConfigEnabled},
			{raw: "enabled: false\n", want: ContractDegradationConfigDisabled},
			{raw: "", want: ContractDegradationConfigUnknown},
			{raw: "enabled: false\nprivate: SECRET-CONFIG-CANARY", want: ContractDegradationConfigUnknown},
		} {
			popConfig := server.Config.PushSimpleResource(model.NetworkDegradationResourceName, []byte(config.raw))
			got := loadContractDegradationConfig()
			popConfig()
			if got != config.want {
				t.Fatal("actual Config resource lost explicit intent or admitted partial source")
			}
		}
		pop := server.Config.PushSimpleResource(model.NetworkDegradationResourceName, []byte("enabled: true\n"))
		defer pop()
		if loadContractDegradationConfig() != ContractDegradationConfigEnabled {
			t.Fatal("actual Config resource did not arm desired-state authority")
		}
		err := server.RedisWithDeadline(ctx, func(client server.RedisClient) error {
			defer func() {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Second)
				defer cleanupCancel()
				if err := client.Del(cleanupCtx, key).Err(); err != nil {
					t.Error("synthetic valve key cleanup failed")
				}
			}()
			for _, scenario := range []string{"active", "charging", "resuming", "large-int", "missing", "wrong-type", "oversized", "malformed", "duplicate", "no-ttl", "ttl-too-long", "expired-state"} {
				if err := client.Del(ctx, key).Err(); err != nil {
					return err
				}
				now := time.Now().UTC()
				state := contractDegradationTestState(now, scenario != "charging")
				if scenario == "resuming" {
					state.CloseCount, state.Ratio, state.ConsecutiveHealthyCount = 70, 0.7, 1
				}
				if scenario == "large-int" {
					state.OpenCount, state.CloseCount, state.Ratio = 9007199254740993, 0, 0
				}
				if scenario == "expired-state" {
					state.EvaluatedAt = now.Add(-time.Hour)
				}
				value, err := json.Marshal(state)
				if err != nil {
					return err
				}
				privateCanary := "PRIVATE-VALVE-STATE-CANARY"
				switch scenario {
				case "oversized":
					value = []byte(strings.Repeat(privateCanary, 100))
				case "malformed":
					value = []byte(`{"private":"` + privateCanary + `"}`)
				case "duplicate":
					value = []byte(strings.Replace(string(value), `"open_count":100`, `"open_count":100,"open_count":100`, 1))
				}
				ttl := model.ContractDegradationStateTtl
				if scenario == "no-ttl" {
					ttl = 0
				}
				if scenario == "ttl-too-long" {
					ttl += time.Hour
				}
				if scenario == "wrong-type" {
					if err := client.HSet(ctx, key, "private", privateCanary).Err(); err != nil {
						return err
					}
				} else if scenario != "missing" {
					if err := client.Set(ctx, key, value, ttl).Err(); err != nil {
						return err
					}
				}
				reads := 0
				source := &contractDegradationContextSource{syntheticSource: &syntheticSource{}, read: func(readCtx context.Context, h HostSettings, command string, timeout time.Duration) (string, error) {
					reads++
					if h.Name != "redis-1" || command != contractDegradationReadCommand(6379) || timeout != 20*time.Second {
						return "", fmt.Errorf("unexpected publication command")
					}
					result, err := client.Do(readCtx, "EVAL_RO", contractDegradationReadScript, 1, key).Result()
					if err != nil {
						return "", err
					}
					reply, ok := result.(string)
					if !ok {
						return "", fmt.Errorf("non-string script frame")
					}
					if strings.Contains(reply, privateCanary) {
						t.Fatal("malformed native value escaped its fixed projection")
					}
					return reply + "\n", nil
				}}
				settings := contractDegradationTestSettings(source)
				settings.ContractDegradationConfig = loadContractDegradationConfig()
				settings.Now = func() time.Time { return time.Now().UTC() }
				alerts, err := NewContractDegradationSignal().Run(ctx, settings)
				if err != nil {
					return err
				}
				if reads != 1 {
					t.Fatal("actual source was not one atomic read")
				}
				active := scenario == "active" || scenario == "resuming" || scenario == "large-int"
				unknown := !active && scenario != "charging"
				if (requireAlertClassCount(alerts, "contract-degradation-active") == 1) != active || (requireAlertClassCount(alerts, "contract-degradation-unobservable") == 1) != unknown {
					t.Fatalf("actual Lua/public decision mismatched scenario %s", scenario)
				}
				if scenario != "missing" && scenario != "wrong-type" {
					after, err := client.Get(ctx, key).Result()
					if err != nil {
						return err
					}
					if after != string(value) {
						t.Fatal("EVAL_RO changed the source value")
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal("actual read-only Redis source gate failed: ", err)
		}
	})
}
