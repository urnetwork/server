// Fixed guard diagnostics distinguish refusal stages without changing authority.
package monitor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestContractDegradationSettingsGuardPhaseAndResult(t *testing.T) {
	for _, phase := range []string{"initial", "disabled-intent", "post-read"} {
		for _, result := range []string{"not-current", "error"} {
			t.Run(phase+"/"+result, func(t *testing.T) {
				now := syntheticSettings(nil).Now()
				reads, checks := 0, 0
				privateCause := errors.New("synthetic-private-settings-canary")
				settings := contractDegradationTestSettings(&syntheticSource{hostTimeoutFn: func(HostSettings, string, time.Duration) (string, error) {
					reads++
					return contractDegradationTestFrame(t, now, contractDegradationTestState(now, true), 3600000), nil
				}})
				if phase == "disabled-intent" {
					settings.ContractDegradationConfig = ContractDegradationConfigDisabled
				}
				settings.SettingsGenerationCheck = func(context.Context, SignalSettings) (bool, error) {
					checks++
					if phase != "initial" && checks == 1 {
						return true, nil
					}
					if result == "error" {
						return true, privateCause
					}
					return false, nil
				}
				alerts, err := NewContractDegradationSignal().Run(context.Background(), settings)
				if err != nil || len(alerts) != 1 {
					t.Fatal("guard refusal lost its single visibility finding")
				}
				alert := alerts[0]
				want := "scope=configured-publication reason=settings-generation guard_phase=" + phase + " checker_result=" + result
				if alert.Class != "contract-degradation-unobservable" || alert.Observed != want || alert.Severity != SeverityWarn || alert.Sustain != 2 || alert.Frame != "publication" || alert.Target != "contract-degradation" {
					t.Fatal("fixed guard diagnostic changed the visibility identity or result")
				}
				wantReads, wantChecks := 0, 1
				if phase != "initial" {
					wantChecks = 2
				}
				if phase == "post-read" {
					wantReads = 1
				}
				if reads != wantReads || checks != wantChecks {
					t.Fatal("diagnostic changed source/check call custody")
				}
				requireAlertOmits(t, alert, privateCause.Error())
			})
		}
	}
}

func TestContractDegradationSettingsGuardHealthyAndNilCompatibility(t *testing.T) {
	for _, mode := range []string{"charging", "active", "disabled"} {
		for _, checked := range []bool{false, true} {
			now := syntheticSettings(nil).Now()
			reads, checks := 0, 0
			settings := contractDegradationTestSettings(&syntheticSource{hostTimeoutFn: func(HostSettings, string, time.Duration) (string, error) {
				reads++
				return contractDegradationTestFrame(t, now, contractDegradationTestState(now, mode == "active"), 3600000), nil
			}})
			settings.SettingsGenerationCheck = nil
			if mode == "disabled" {
				settings.ContractDegradationConfig = ContractDegradationConfigDisabled
			}
			if checked {
				settings.SettingsGenerationCheck = func(context.Context, SignalSettings) (bool, error) { checks++; return true, nil }
			}
			alerts, err := NewContractDegradationSignal().Run(context.Background(), settings)
			if err != nil {
				t.Fatal("stable or explicit immutable settings were refused")
			}
			if mode == "active" {
				if len(alerts) != 1 || alerts[0].Class != "contract-degradation-active" || alerts[0].Severity != SeverityPage || strings.Contains(alerts[0].Observed, "guard_phase=") {
					t.Fatal("active publication contract changed")
				}
			} else if len(alerts) != 0 {
				t.Fatal("stable charging or disabled intent lost its healthy verdict")
			}
			wantReads := 1
			if mode == "disabled" {
				wantReads = 0
			}
			wantChecks := 0
			if checked {
				wantChecks = 2
			}
			if reads != wantReads || checks != wantChecks {
				t.Fatal("stable/nil diagnostic path changed native calls")
			}
		}
	}
}

func TestContractDegradationSettingsGuardCancellationPreservesCause(t *testing.T) {
	for _, phase := range []string{"initial", "disabled-intent", "post-read"} {
		for _, withCause := range []bool{false, true} {
			ctx, cancel := context.WithCancel(context.Background())
			now := syntheticSettings(nil).Now()
			reads, checks := 0, 0
			privateCause := errors.New("synthetic-private-cancel-canary")
			settings := contractDegradationTestSettings(&syntheticSource{hostTimeoutFn: func(HostSettings, string, time.Duration) (string, error) {
				reads++
				return contractDegradationTestFrame(t, now, contractDegradationTestState(now, true), 3600000), nil
			}})
			if phase == "disabled-intent" {
				settings.ContractDegradationConfig = ContractDegradationConfigDisabled
			}
			settings.SettingsGenerationCheck = func(context.Context, SignalSettings) (bool, error) {
				checks++
				if phase != "initial" && checks == 1 {
					return true, nil
				}
				cancel()
				if withCause {
					return false, privateCause
				}
				return false, nil
			}
			alerts, err := NewContractDegradationSignal().Run(ctx, settings)
			cancel()
			if len(alerts) != 0 || !errors.Is(err, context.Canceled) {
				t.Fatal("ended owner gained guard/publication findings")
			}
			if withCause && !errors.Is(err, privateCause) {
				t.Fatal("guard diagnostic dropped its observed source cause")
			}
			if strings.Contains(err.Error(), privateCause.Error()) {
				t.Fatal("guard diagnostic rendered private error")
			}
			wantReads := 0
			if phase == "post-read" {
				wantReads = 1
			}
			if reads != wantReads {
				t.Fatal("ended guard changed source call boundary")
			}
		}
	}
}

func TestContractDegradationSettingsGuardOtherUnknownReasonsUnchanged(t *testing.T) {
	for _, reason := range []string{"configuration", "redis-read"} {
		reads := 0
		settings := contractDegradationTestSettings(&syntheticSource{hostTimeoutFn: func(HostSettings, string, time.Duration) (string, error) {
			reads++
			return "", errors.New("synthetic-private-source-canary")
		}})
		settings.SettingsGenerationCheck = func(context.Context, SignalSettings) (bool, error) { return true, nil }
		if reason == "configuration" {
			settings.ContractDegradationConfig = ContractDegradationConfigUnknown
		}
		alerts := contractDegradationTestRun(t, settings)
		alert := requireAlertClass(t, alerts, "contract-degradation-unobservable")
		if len(alerts) != 1 || alert.Observed != "scope=configured-publication reason="+reason {
			t.Fatal("non-generation reason gained guard fields")
		}
		requireAlertOmits(t, alert, "synthetic-private-source-canary")
		wantReads := 0
		if reason == "redis-read" {
			wantReads = 1
		}
		if reads != wantReads {
			t.Fatal("non-generation source boundary changed")
		}
	}
}
