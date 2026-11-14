// History source errors keep fixed reader text and typed authority without
// changing client-growth policy, contacting Redis or publishing retired results.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// A live reader still returns the same private-safe message; source identity
// remains reachable through the real gateway helper and bounded reader.
func TestRedisClientsHistoryReadPreservesSourceAuthority(t *testing.T) {
	completed, failedCases := 0, 0
	for _, kind := range []string{"private", "deadline", "native", "admission", "joined"} {
		caseName := kind
		checkCase := func() {
			caseFailed := false
			defer func() {
				if caseFailed {
					failedCases++
				}
			}()
			fail := func(message string) {
				caseFailed = true
				t.Errorf("case=%s: %s", caseName, message)
			}
			private := "password=history-private-token SELECT private_customer FROM private_relation"
			cause := errors.New(private)
			admission := &sshAdmissionUnavailableError{err: cause, phase: sshAdmissionFailureAcquire}
			precontact := &observationBoundaryError{err: admission, boundary: observationBoundaryHost}
			native := &observationBoundaryError{err: cause, boundary: observationBoundaryNativeWait}
			replies := []error{cause}
			switch kind {
			case "deadline":
				replies = []error{fmt.Errorf("%s: %w", private, context.DeadlineExceeded)}
			case "native":
				replies = []error{native}
			case "admission":
				replies = []error{precontact}
			case "joined":
				replies = []error{precontact, native}
			}
			calls := 0
			settings := syntheticSettings(&syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
				if !strings.Contains(command, redisClientsHistoryMarker) || !strings.Contains(command, "--max-time 15 --max-filesize 1048576") || calls >= len(replies) {
					fail("history reader changed source scope or bounds")
					return "", errors.New("synthetic source scope refused")
				}
				err := replies[calls]
				calls++
				return private, err
			}})
			settings.Hosts = nil
			for i := range replies {
				settings.Hosts = append(settings.Hosts, HostSettings{Name: fmt.Sprintf("history-gateway-%d.invalid", i), Roles: []string{"services"}})
			}
			env, err := newProbeEnv(settings.withDefaults())
			if err != nil {
				fail("synthetic history environment unavailable")
				return
			}
			targets := map[string]redisClientsHistoryTarget{"redis-1:6380": {host: &host{name: "redis-1"}, port: 6380}}
			observations, err := observeRedisClientsHistory(context.Background(), env, targets, settings.Now())
			if len(observations) != 0 || calls != len(replies) || err == nil || err.Error() != "Redis client history query unavailable" {
				fail("history source failure changed its fixed reader contract")
			}
			for _, source := range replies {
				if !errors.Is(err, source) {
					fail("history reader discarded returned source authority")
				}
			}
			if kind == "admission" || kind == "joined" {
				var returned *sshAdmissionUnavailableError
				if !errors.As(err, &returned) || returned != admission {
					fail("history reader lost the original typed admission error")
				}
			}
			if kind == "native" || kind == "joined" {
				if !strings.Contains(observationBoundaryDetails(err), "native-wait/started-joined") {
					fail("history reader erased a returned local execution proof")
				}
			}
			alert := visibilityAlert(settings, NewRedisConnectionsSignal(), err)
			for index, value := range []string{private, "history-private-token", "private_customer", "history-gateway-"} {
				if value != "" && strings.Contains(alert.Markdown(), value) {
					fail(fmt.Sprintf("alert leaked forbidden identifier at fixture index %d", index))
				}
			}
			completed++
			t.Logf("case=%s completed failed=%t", caseName, caseFailed)
		}
		checkCase()
	}
	if completed != 5 {
		t.Errorf("reader matrix incomplete: completed=%d expected=5", completed)
	}
	t.Logf("reader matrix completed=%d expected=5 failed_cases=%d", completed, failedCases)
}

// Retiring the full public owner preserves both earlier native and returned
// history causes. The spare gateway, battery and retired findings stay unused.
func TestRedisConnectionsHistoryInterruptedSourceAuthority(t *testing.T) {
	completed, cleaned, failedCases := 0, 0, 0
	for _, deadline := range []bool{false, true} {
		for _, nativeUnavailable := range []bool{false, true} {
			for _, kind := range []string{"native", "admission", "joined", "nil-source"} {
				caseName := fmt.Sprintf("deadline=%t/native-unavailable=%t/%s", deadline, nativeUnavailable, kind)
				checkCase := func() {
					caseFailed := false
					defer func() {
						if caseFailed {
							failedCases++
						}
					}()
					fail := func(message string) {
						caseFailed = true
						t.Errorf("case=%s: %s", caseName, message)
					}
					ctx, cancel := context.WithCancel(context.Background())
					defer func() {
						cancel()
						cleaned++
					}()
					if deadline {
						ctx = observationBoundaryDeadlineContext{Context: ctx}
					}
					private := "token=history-private-token SELECT private_customer FROM private_relation"
					cause := errors.New(private)
					priorCause := errors.New("private-native-history-cohort")
					prior := &observationBoundaryError{err: priorCause, boundary: observationBoundaryNativeWait}
					admission := &sshAdmissionUnavailableError{err: cause, phase: sshAdmissionFailureAcquire}
					precontact := &observationBoundaryError{err: admission, boundary: observationBoundaryHost}
					native := &observationBoundaryError{err: cause, boundary: observationBoundaryNativeWait}
					replies := []error{native}
					switch kind {
					case "admission":
						replies = []error{precontact}
					case "joined":
						replies = []error{precontact, native}
					case "nil-source":
						replies = []error{nil}
					}
					calls, nativeCalls := 0, 0
					source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
						if strings.Contains(command, "CLIENT LIST") {
							fail("retired history attempt started a connection battery")
							return "", errors.New("synthetic battery refused")
						}
						if !strings.Contains(command, redisClientsHistoryMarker) {
							nativeCalls++
							if nativeUnavailable {
								return "", prior
							}
							return redisClientsHistoryTestNative(map[int]int{6380: 100, 6381: 100, 6382: 100}), nil
						}
						if calls >= len(replies) || ctx.Err() != nil {
							fail("retired history owner contacted another gateway")
							return "", errors.New("synthetic gateway refused")
						}
						err := replies[calls]
						calls++
						if calls == len(replies) {
							cancel()
						}
						return private, err
					}}
					settings := syntheticSettings(source)
					for i := 0; i <= len(replies); i++ {
						settings.Hosts = append(settings.Hosts, HostSettings{Name: fmt.Sprintf("history-gateway-%d.invalid", i), Roles: []string{"services"}})
					}
					signal := NewRedisConnectionsSignal()
					alerts, err := signal.Run(ctx, settings)
					if calls != len(replies) || nativeCalls != 1 || len(alerts) != 0 || !errors.Is(err, ctx.Err()) || err.Error() != ctx.Err().Error() {
						fail("history retirement changed its context, source count or publication contract")
					}
					if kind != "nil-source" && !errors.Is(err, cause) {
						fail("history owner discarded its returned history source authority")
					}
					if nativeUnavailable && !errors.Is(err, priorCause) {
						fail("history retirement discarded an earlier native source failure")
					}
					if kind == "admission" || kind == "joined" {
						var returned *sshAdmissionUnavailableError
						if !errors.As(err, &returned) || returned != admission {
							fail("history owner lost original typed admission identity")
						}
					}
					if kind == "nil-source" && !nativeUnavailable && observationBoundaryDetails(err) != "" {
						fail("nil history source manufactured an execution proof")
					}
					alert := visibilityAlert(settings, signal, err)
					parentOnly := visibilityAlert(settings, signal, ctx.Err())
					if alert.Identity() != parentOnly.Identity() || classifyObservationError(err) != classifyObservationError(ctx.Err()) {
						fail("history source proof changed the parent visibility verdict")
					}
					for index, value := range []string{private, "history-private-token", "private_customer", "private-native-history-cohort", "history-gateway-"} {
						if value != "" && strings.Contains(alert.Markdown(), value) {
							fail(fmt.Sprintf("alert leaked forbidden identifier at fixture index %d", index))
						}
					}
					completed++
					t.Logf("case=%s completed failed=%t", caseName, caseFailed)
				}
				checkCase()
			}
		}
	}
	if completed != 16 || cleaned != 16 {
		t.Errorf("owner matrix incomplete: completed=%d cleaned=%d expected=16", completed, cleaned)
	}
	t.Logf("owner matrix completed=%d cleaned=%d expected=16 failed_cases=%d", completed, cleaned, failedCases)
}
