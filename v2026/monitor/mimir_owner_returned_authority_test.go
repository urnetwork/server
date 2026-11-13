// Returned Mimir source proofs survive owner termination without publishing
// retired findings, exposing private diagnostics or contacting another gateway.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The synchronous source ends a live parent only on its final owned reply.
// Deadline mode uses the existing deterministic Err fixture, never elapsed time.
func testMimirOwnerReturnedAuthority(t *testing.T, owner string) {
	t.Helper()
	completed := 0
	for _, deadline := range []bool{false, true} {
		for _, scoped := range []bool{false, true} {
			for _, kind := range []string{"private", "native", "admission", "joined", "nil-source"} {
				caseName := fmt.Sprintf("owner=%s/deadline=%t/scoped=%t/%s", owner, deadline, scoped, kind)
				func() {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					if deadline {
						ctx = observationBoundaryDeadlineContext{Context: ctx}
					}
					private := "password=owner-private-token SELECT private_customer FROM private_relation"
					cause := errors.New(private)
					admission := &sshAdmissionUnavailableError{err: cause, phase: sshAdmissionFailureAcquire}
					precontact := &observationBoundaryError{err: admission, boundary: observationBoundaryHost}
					native := &observationBoundaryError{err: cause, boundary: observationBoundaryNativeWait}
					replies := []error{cause}
					wantBoundary, wantAdmission := "", ""
					switch kind {
					case "native":
						replies = []error{native}
						wantBoundary = "execution_boundaries=native-wait/started-joined"
					case "admission":
						replies = []error{precontact}
						wantBoundary = "execution_boundaries=host-admission/not-attempted"
						wantAdmission = "acquire"
					case "joined":
						replies = []error{precontact, native}
						wantBoundary = "execution_boundaries=host-admission/not-attempted,native-wait/started-joined"
						wantAdmission = "acquire"
					case "nil-source":
						replies = []error{nil}
					}
					calls, pgCalls, records := 0, 0, 0
					source := &syntheticSource{
						postgresFn: func(query string) ([]Row, error) {
							pgCalls++
							if owner != "signup" || query != signupLivenessQuery {
								t.Fatal("owner used an unrelated source")
							}
							return []Row{{fmt.Sprint(signupTestTime.Unix()), "1016192", "64", "17", "30", "64", "64"}}, nil
						},
						hostFn: func(_ HostSettings, command string) (string, error) {
							if ctx.Err() != nil || calls >= len(replies) {
								t.Fatal("source contacted a gateway after owner termination")
							}
							cap := "2097152"
							if owner == "signup" {
								cap = "65536"
							}
							if !strings.Contains(command, "--max-time 15 --max-filesize "+cap) {
								t.Fatal("owner changed its existing source bounds")
							}
							err := replies[calls]
							calls++
							if calls == len(replies) {
								cancel()
							}
							return private, err
						},
					}
					settings := syntheticSettings(source)
					settings.Now = func() time.Time { return signupTestTime }
					settings.Hosts = nil
					for i := 0; i <= len(replies); i++ {
						settings.Hosts = append(settings.Hosts, HostSettings{Name: fmt.Sprintf("owner-source-%d.example", i), Roles: []string{"services"}})
					}
					if scoped {
						settings.disabledHosts = []HostSettings{{Name: "excluded-owner-source.example"}}
					}
					settings.LogServices = []string{"api", "taskworker"}
					settings.LogServiceHosts = map[string][]string{"api": {"api-a.example"}, "taskworker": {"worker-a.example"}}
					settings.LogServiceBlocks = map[string][]string{"api": {"lane-a"}, "taskworker": {"worker-group"}}
					settings.UrlProbeCoverageObserver = func(UrlProbeCoverageObservation) error { records++; return nil }
					var signal *signalAdapter
					switch owner {
					case "url":
						signal = NewUrlProbeCoverageSignal().(*signalAdapter)
						signal.probe = urlProbeCoverageProbe{loadDesired: func() (urlProbeCoverageDesired, error) {
							return urlProbeCoverageDesired{enabled: true, shardCount: 2}, nil
						}}
					case "control":
						signal = NewControlRoutePressureSignal().(*signalAdapter)
					case "signup":
						signal = NewSignupLivenessSignal().(*signalAdapter)
					default:
						t.Fatal("unowned fixture")
					}
					alerts, err := signal.Run(ctx, settings)
					if err == nil {
						t.Errorf("%s: terminated owner returned nil error", caseName)
						return
					}
					if calls != len(replies) || len(alerts) != 0 || records != 0 || !errors.Is(err, ctx.Err()) || err.Error() != ctx.Err().Error() {
						t.Errorf("%s: terminated owner changed its lifecycle, publication or request contract", caseName)
					}
					if (owner == "signup" && pgCalls != 1) || (owner != "signup" && pgCalls != 0) {
						t.Errorf("%s: owner repeated or added a durable source read", caseName)
					}
					if kind == "nil-source" {
						if observationBoundaryDetails(err) != "" || sshAdmissionFailurePhases(err) != "" || errors.Is(err, cause) {
							t.Errorf("%s: nil source manufactured execution authority", caseName)
						}
					} else {
						if !errors.Is(err, cause) {
							t.Errorf("%s: returned source cause was erased", caseName)
						}
						for _, returned := range replies {
							if !errors.Is(err, returned) {
								t.Errorf("%s: a returned gateway error left the error graph", caseName)
							}
						}
					}
					if wantBoundary != "" && observationBoundaryDetails(err) != " "+wantBoundary {
						t.Errorf("%s: returned execution boundary was erased or expanded", caseName)
					}
					if sshAdmissionFailurePhases(err) != wantAdmission {
						t.Errorf("%s: returned typed admission phase was erased or invented", caseName)
					}
					if wantAdmission != "" {
						var returned *sshAdmissionUnavailableError
						if !errors.As(err, &returned) || returned != admission {
							t.Errorf("%s: original typed admission error identity was lost", caseName)
						}
					}
					alert := visibilityAlert(settings, signal, err)
					parentOnly := visibilityAlert(settings, signal, ctx.Err())
					if alert.Identity() != parentOnly.Identity() || alert.Class != "cannot-observe" || alert.Severity != SeverityWarn || alert.Sustain != 2 || classifyObservationError(err) != classifyObservationError(ctx.Err()) {
						t.Errorf("%s: source proof changed the owning visibility verdict", caseName)
					}
					if wantBoundary != "" && !strings.Contains(alert.Observed, wantBoundary) {
						t.Errorf("%s: proved local boundary did not reach the owning visibility projection", caseName)
					}
					requireAlertOmits(t, alert, private, "owner-private-token", "private_customer", "remote-contact", "owner-source-", "excluded-owner-source")
				}()
				completed++
			}
		}
	}
	t.Logf("mimir owner authority cases complete: owner=%s count=%d", owner, completed)
}

// Each root exercises the real owner, gateway joining and public adapter.
func TestUrlProbeCoverageInterruptedSourceAuthority(t *testing.T) {
	testMimirOwnerReturnedAuthority(t, "url")
}

// Control-route retirement keeps its returned source proof and context verdict.
func TestControlRoutePressureInterruptedSourceAuthority(t *testing.T) {
	testMimirOwnerReturnedAuthority(t, "control")
}

// Signup retirement withholds its durable witness and preserves the metrics cause.
func TestSignupLivenessInterruptedSourceAuthority(t *testing.T) {
	testMimirOwnerReturnedAuthority(t, "signup")
}

// The synchronous fixture ends its real parent at the caller's post-read check.
// The preceding gateway check sees a live parent and returns a successful body.
type signupPostReadCancelContext struct {
	context.Context
	cancel            context.CancelFunc
	checksUntilCancel int
}

// No goroutine or clock controls this exact successful-read retirement boundary.
func (self *signupPostReadCancelContext) Err() error {
	if self.checksUntilCancel > 0 {
		self.checksUntilCancel--
		if self.checksUntilCancel == 0 {
			self.cancel()
		}
	}
	return self.Context.Err()
}

// Success and parser failure retain the parent verdict at late retirement.
func TestSignupLivenessLateReadRetirement(t *testing.T) {
	completed := 0
	for _, deadline := range []bool{false, true} {
		for _, malformed := range []bool{false, true} {
			caseName := fmt.Sprintf("deadline=%t/malformed=%t", deadline, malformed)
			func() {
				parent, cancel := context.WithCancel(context.Background())
				defer cancel()
				var parentContext context.Context = parent
				if deadline {
					parentContext = observationBoundaryDeadlineContext{Context: parent}
				}
				ctx := &signupPostReadCancelContext{Context: parentContext, cancel: cancel}
				calls, pgCalls := 0, 0
				source := &syntheticSource{
					postgresFn: func(query string) ([]Row, error) {
						pgCalls++
						if query != signupLivenessQuery {
							t.Fatal("owner used an unrelated durable source")
						}
						return []Row{{fmt.Sprint(signupTestTime.Unix()), "1016192", "64", "17", "30", "64", "64"}}, nil
					},
					hostFn: func(_ HostSettings, _ string) (string, error) {
						calls++
						if calls != 1 {
							t.Fatal("retired owner added a metrics read")
						}
						ctx.checksUntilCancel = 2
						if malformed {
							return "{", nil
						}
						return signupTestMetrics(t, signupTestDemand()), nil
					},
				}
				settings := syntheticSettings(source)
				settings.Now = func() time.Time { return signupTestTime }
				settings.Hosts = append(settings.Hosts, HostSettings{Name: "signup-late-success.example", Roles: []string{"services"}})
				env, err := newProbeEnv(settings)
				if err != nil {
					t.Fatal(err)
				}
				findings, err := (signupLivenessProbe{}).check(ctx, env)
				if calls != 1 || pgCalls != 1 || ctx.checksUntilCancel != 0 || len(findings) != 1 {
					t.Errorf("%s: fixture missed the successful-read retirement boundary", caseName)
				}
				if err == nil {
					t.Errorf("%s: late read retirement lost its parent error", caseName)
					return
				}
				if !errors.Is(err, ctx.Err()) || err.Error() != ctx.Err().Error() {
					t.Errorf("%s: late read retirement lost its parent error", caseName)
				}
				var interrupted *signalContextError
				if malformed {
					if !errors.As(err, &interrupted) || interrupted.sourceErr == nil || interrupted.sourceErr.Error() != "signup metric response is incomplete or invalid" || !errors.Is(err, interrupted.sourceErr) {
						t.Errorf("%s: late parser retirement lost its source error", caseName)
					}
				} else if errors.As(err, &interrupted) {
					t.Errorf("%s: successful late retirement invented source authority", caseName)
				}
			}()
			completed++
		}
	}
	t.Logf("signup late retirement cases complete: count=%d", completed)
}
