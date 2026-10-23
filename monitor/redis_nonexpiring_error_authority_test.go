// Keep private source causes and existing lifecycle/taxonomy authority intact
// across direct collection, the probe and both public adapter paths.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// An unrelated exclusion selects the public scoped adapter while the Redis
// host remains admitted. This changes no native observation or inventory.
func redisNonexpiringErrorSettings(t *testing.T, source SignalSource, scoped bool) SignalSettings {
	t.Helper()
	settings := syntheticSettings(source)
	if scoped {
		var err error
		settings, err = ExcludeHosts(settings, "pg-1")
		if err != nil {
			t.Fatal("could not enable the unrelated synthetic scope")
		}
	}
	return settings
}

// Every mode invokes an existing production entry point. No new production
// wrapper type is named, so these controls also compile against the held draft.
func redisNonexpiringErrorRun(t *testing.T, mode string, ctx context.Context, settings SignalSettings) (Alerts, error) {
	t.Helper()
	if mode == "unscoped" || mode == "scoped" {
		return NewRedisNonexpiringSignal().Run(ctx, settings)
	}
	env, err := newProbeEnv(settings.withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	if mode == "collector" {
		h := env.cfg.redisEntryHost()
		nodes, err := collectRedisKeyspaceNodes(ctx, env, h, h.redisNodePorts())
		if len(nodes) != 0 {
			t.Fatal("failed collector emitted authoritative keyspace nodes")
		}
		return nil, err
	}
	findings, err := (redisNonexpiringProbe{}).check(ctx, env)
	if len(findings) != 0 {
		t.Fatal("failed whole-census probe emitted data-state findings")
	}
	return nil, err
}

// Classification is observed through real visibility rendering; bounded cause
// inspection verifies every original error remains privately reachable.
func requireRedisNonexpiringErrorIdentity(t *testing.T, settings SignalSettings, err error, wantClass, wantPhases string, causes ...error) {
	t.Helper()
	if err == nil {
		t.Fatal("nonexpiring error authority assertion: failed source became successful")
	}
	if !strings.HasPrefix(err.Error(), "Redis keyspace census command failed on configured host redis-") &&
		err.Error() != context.Canceled.Error() && err.Error() != context.DeadlineExceeded.Error() {
		t.Fatal("nonexpiring error authority assertion: public error lost its fixed observation boundary")
	}
	inspection := server.InspectErrorCauses(err)
	if !inspection.Complete {
		t.Fatal("nonexpiring error authority assertion: original source graph became incomplete")
	}
	for _, cause := range causes {
		if !errors.Is(err, cause) {
			t.Fatal("nonexpiring error authority assertion: original source cause was discarded")
		}
		found := false
		for _, node := range inspection.Nodes {
			found = found || node.Err == cause
		}
		if !found {
			t.Fatal("nonexpiring error authority assertion: original source cause is absent from bounded inspection")
		}
		var wantNative, native *exec.ExitError
		var wantSsh, ssh *sshCommandError
		var wantUnreachable, unreachable *unreachableError
		var wantAdmission, admission *sshAdmissionUnavailableError
		if errors.As(cause, &wantNative) && !errors.As(err, &native) ||
			errors.As(cause, &wantSsh) && !errors.As(err, &ssh) ||
			errors.As(cause, &wantUnreachable) && !errors.As(err, &unreachable) ||
			errors.As(cause, &wantAdmission) && !errors.As(err, &admission) {
			t.Fatal("nonexpiring error authority assertion: typed source identity was discarded")
		}
	}
	if classifyObservationError(err) != wantClass || sshAdmissionFailurePhases(err) != wantPhases {
		t.Fatal("nonexpiring error authority assertion: source taxonomy or admission phase changed")
	}
	signal := NewRedisNonexpiringSignal()
	alert := visibilityAlert(settings, signal, err)
	if alert.Class != "cannot-observe" || alert.Severity != SeverityWarn || alert.Sustain != 2 ||
		alert.Target != "redis/nonexpiring-skew" || !alert.unobservable ||
		!strings.Contains(alert.Observed, "error_class="+wantClass) {
		t.Fatal("nonexpiring source error changed visibility authority")
	}
	redisNonexpiringAuthorityPrivate(t, Alerts{alert}, err)
}

// Source-only cancellation/deadlines keep their existing classification while
// the caller remains live. Native statuses are real private child exit errors,
// never strings that pretend to carry a native process identity.
func TestRedisNonexpiringAuthorityErrorLiveParent(t *testing.T) {
	privateCause := errors.New("synthetic-private-sentinel")
	cases := []struct {
		name      string
		sourceErr error
		class     string
		phases    string
	}{
		{name: "canceled", sourceErr: errors.Join(context.Canceled, privateCause), class: observationErrorClassCanceled},
		{name: "deadline", sourceErr: errors.Join(context.DeadlineExceeded, privateCause), class: observationErrorClassTimeout},
		{name: "unreachable", sourceErr: &unreachableError{host: "synthetic-private-sentinel", err: privateCause}, class: observationErrorClassUnreachable},
		{name: "transport timeout", sourceErr: &unreachableError{host: "synthetic-private-sentinel", err: fmt.Errorf("timeout after synthetic interval: %w", privateCause)}, class: observationErrorClassTimeout},
		{name: "source text cancellation", sourceErr: fmt.Errorf("context canceled: %w", privateCause), class: observationErrorClassCanceled},
		{name: "access", sourceErr: fmt.Errorf("permission denied: %w", privateCause), class: observationErrorClassAccessDenied},
		{name: "invalid response", sourceErr: fmt.Errorf("invalid response: %w", privateCause), class: observationErrorClassInvalidResponse},
		{name: "contract", sourceErr: fmt.Errorf("source contract mismatch: %w", privateCause), class: observationErrorClassContractMismatch},
		{name: "bound", sourceErr: fmt.Errorf("identity bound exceeded: %w", privateCause), class: observationErrorClassBoundExceeded},
		{name: "counter", sourceErr: fmt.Errorf("monotonic counter decreased: %w", privateCause), class: observationErrorClassCounterReset},
		{name: "state", sourceErr: fmt.Errorf("durable state unavailable: %w", privateCause), class: observationErrorClassStateUnavailable},
		{name: "admission acquire", sourceErr: &sshAdmissionUnavailableError{err: privateCause, phase: sshAdmissionFailureAcquire}, class: observationErrorClassAdmissionUnavailable, phases: "acquire"},
		{name: "admission release", sourceErr: &sshAdmissionUnavailableError{err: privateCause, phase: sshAdmissionFailureRelease}, class: observationErrorClassAdmissionUnavailable, phases: "release"},
		{name: "other", sourceErr: privateCause, class: observationErrorClassUnclassified},
	}
	for _, status := range []int{1, 124, 127, 137, 255} {
		native := syntheticProcessExit(t, status)
		class := observationErrorClassCommandFailed
		if status == 255 {
			class = observationErrorClassSSHExit255
		}
		cases = append(cases, struct {
			name      string
			sourceErr error
			class     string
			phases    string
		}{name: fmt.Sprintf("native %d", status), sourceErr: &sshCommandError{err: fmt.Errorf("synthetic-private-sentinel: %w", errors.Join(native, privateCause))}, class: class})
	}
	for _, test := range cases {
		for _, mode := range []string{"collector", "probe", "unscoped", "scoped"} {
			calls := 0
			source := &redisNonexpiringAuthoritySource{syntheticSource: &syntheticSource{}, host: func(ctx context.Context, _ HostSettings, _ string) (string, error) {
				calls++
				if ctx.Err() != nil {
					t.Fatalf("%s/%s: source-only lifecycle error ended its live parent", test.name, mode)
				}
				return "synthetic-private-sentinel", test.sourceErr
			}}
			settings := redisNonexpiringErrorSettings(t, source, mode == "scoped")
			alerts, err := redisNonexpiringErrorRun(t, mode, context.Background(), settings)
			if calls != 1 || len(alerts) != 0 {
				t.Fatalf("%s/%s: failed source changed call count or emitted a data-state verdict", test.name, mode)
			}
			requireRedisNonexpiringErrorIdentity(t, settings, err, test.class, test.phases, test.sourceErr, privateCause)
		}
	}
}

// Ending the actual caller inside the source does not erase its returned
// cause. Both live-entry adapter paths add the canonical parent verdict;
// direct collection and probe entry keep their existing source authority.
func TestRedisNonexpiringAuthorityErrorEndedParent(t *testing.T) {
	for _, mode := range []string{"collector", "probe", "unscoped", "scoped"} {
		for _, sourceContext := range []error{context.Canceled, context.DeadlineExceeded} {
			ctx, cancel := context.WithCancel(context.Background())
			privateCause := errors.New("synthetic-private-sentinel")
			admission := &sshAdmissionUnavailableError{err: privateCause, phase: sshAdmissionFailureRelease}
			sourceErr := fmt.Errorf("synthetic-private-sentinel: %w", errors.Join(sourceContext, admission))
			calls := 0
			source := &redisNonexpiringAuthoritySource{syntheticSource: &syntheticSource{}, host: func(sourceCtx context.Context, _ HostSettings, _ string) (string, error) {
				calls++
				if sourceCtx.Err() != nil {
					t.Fatal("ended-parent control was not live at source entry")
				}
				cancel()
				if sourceCtx.Err() != context.Canceled {
					t.Fatal("source did not receive the actual caller context")
				}
				return "synthetic-private-sentinel", sourceErr
			}}
			settings := redisNonexpiringErrorSettings(t, source, mode == "scoped")
			alerts, err := redisNonexpiringErrorRun(t, mode, ctx, settings)
			cancel()
			if calls != 1 || len(alerts) != 0 {
				t.Fatal("ended parent changed source count or published a verdict")
			}
			adapter := mode == "unscoped" || mode == "scoped"
			wantClass := observationErrorClassCanceled
			if sourceContext == context.DeadlineExceeded && !adapter {
				wantClass = observationErrorClassTimeout
			}
			causes := []error{sourceErr, sourceContext, admission, privateCause}
			if adapter {
				causes = append(causes, context.Canceled)
			}
			requireRedisNonexpiringErrorIdentity(t, settings, err, wantClass, "release", causes...)
		}
	}
}

// Scoped admission refuses already-ended callers before contacting the source.
// Other entry points still pass the ended context to their existing source;
// its returned cancellation and private cause remain distinguishable.
func TestRedisNonexpiringAuthorityErrorAlreadyEndedParent(t *testing.T) {
	for _, mode := range []string{"collector", "probe", "unscoped", "scoped"} {
		for _, deadline := range []bool{false, true} {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if deadline {
				ctx, cancel = context.WithDeadline(context.Background(), time.Unix(0, 0))
			}
			privateCause := errors.New("synthetic-private-sentinel")
			sourceErr := fmt.Errorf("synthetic-private-sentinel: %w", errors.Join(ctx.Err(), privateCause))
			calls := 0
			source := &redisNonexpiringAuthoritySource{syntheticSource: &syntheticSource{}, host: func(sourceCtx context.Context, _ HostSettings, _ string) (string, error) {
				calls++
				if sourceCtx.Err() != ctx.Err() {
					t.Fatal("source lost its already-ended parent")
				}
				return "synthetic-private-sentinel", sourceErr
			}}
			settings := redisNonexpiringErrorSettings(t, source, mode == "scoped")
			alerts, err := redisNonexpiringErrorRun(t, mode, ctx, settings)
			cancel()
			wantCalls := 1
			causes := []error{ctx.Err(), sourceErr, privateCause}
			if mode == "scoped" {
				wantCalls, causes = 0, []error{ctx.Err()}
			}
			if calls != wantCalls || len(alerts) != 0 {
				t.Fatal("already-ended parent changed source or scope admission policy")
			}
			wantClass := observationErrorClassCanceled
			if deadline {
				wantClass = observationErrorClassTimeout
			}
			requireRedisNonexpiringErrorIdentity(t, settings, err, wantClass, "", causes...)
		}
	}
}

// Joined hosts retain every private cause and the established class precedence
// in either inventory order; the first host cannot erase a later timeout or
// local admission phase merely because its public error is sanitized.
func TestRedisNonexpiringAuthorityErrorJoinedHosts(t *testing.T) {
	privateA, privateB := errors.New("synthetic-private-sentinel-a"), errors.New("synthetic-private-sentinel-b")
	native := syntheticProcessExit(t, 1)
	commandErr := &sshCommandError{err: fmt.Errorf("synthetic-private-sentinel: %w", errors.Join(native, privateA))}
	for _, test := range []struct {
		first, second error
		class, phases string
	}{
		{first: commandErr, second: &unreachableError{host: "synthetic-private-sentinel", err: fmt.Errorf("timeout after synthetic interval: %w", privateB)}, class: observationErrorClassTimeout},
		{first: commandErr, second: fmt.Errorf("access denied: %w", privateB), class: observationErrorClassAccessDenied},
		{first: &sshAdmissionUnavailableError{err: privateA, phase: sshAdmissionFailureAcquire}, second: &sshAdmissionUnavailableError{err: privateB, phase: sshAdmissionFailureRelease}, class: observationErrorClassAdmissionUnavailable, phases: "acquire,release"},
		{first: errors.Join(context.Canceled, privateA), second: errors.Join(context.DeadlineExceeded, privateB), class: observationErrorClassTimeout},
	} {
		for _, scoped := range []bool{false, true} {
			for _, reverse := range []bool{false, true} {
				calls := 0
				source := &redisNonexpiringAuthoritySource{syntheticSource: &syntheticSource{}, host: func(ctx context.Context, configured HostSettings, _ string) (string, error) {
					calls++
					if ctx.Err() != nil {
						t.Fatal("joined source fixture unexpectedly ended the caller")
					}
					if configured.Name == "redis-1" {
						return "synthetic-private-sentinel", test.first
					}
					return "synthetic-private-sentinel", test.second
				}}
				settings := redisNonexpiringErrorSettings(t, source, scoped)
				settings.Hosts = append(settings.Hosts, HostSettings{Name: "redis-2", Roles: []string{"redis-cluster"}, RedisNodePorts: []int{6383, 6384, 6385}})
				if reverse {
					settings.Hosts[1], settings.Hosts[2] = settings.Hosts[2], settings.Hosts[1]
				}
				alerts, err := NewRedisNonexpiringSignal().Run(context.Background(), settings)
				if calls != 2 || len(alerts) != 0 {
					t.Fatal("joined source errors changed census coverage or emitted a verdict")
				}
				requireRedisNonexpiringErrorIdentity(t, settings, err, test.class, test.phases, test.first, test.second, privateA, privateB)
				if !strings.Contains(err.Error(), "redis-1") || !strings.Contains(err.Error(), "redis-2") {
					t.Fatal("joined source errors lost configured-host diagnostic scope")
				}
			}
		}
	}
}

// One valid host does not hide another host's source classification. The
// survivor's numbers have no product or health authority for a partial median.
func TestRedisNonexpiringAuthorityErrorPartialHost(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		privateCause := errors.New("synthetic-private-sentinel")
		sourceErr := &unreachableError{host: "synthetic-private-sentinel", err: fmt.Errorf("timeout after synthetic interval: %w", privateCause)}
		source := &redisNonexpiringAuthoritySource{syntheticSource: &syntheticSource{}, host: func(_ context.Context, configured HostSettings, _ string) (string, error) {
			if configured.Name == "redis-1" {
				return "6380 204900 100 2048\n6381 204900 100 2048\n6382 204900 100 2048\n", nil
			}
			return "synthetic-private-sentinel", sourceErr
		}}
		settings := redisNonexpiringErrorSettings(t, source, scoped)
		settings.Hosts = append(settings.Hosts, HostSettings{Name: "redis-2", Roles: []string{"redis-cluster"}, RedisNodePorts: []int{6383, 6384, 6385}})
		alerts, err := NewRedisNonexpiringSignal().Run(context.Background(), settings)
		redisNonexpiringAuthorityPrivate(t, alerts, err)
		if err != nil || len(alerts) != 1 || alerts[0].Class != "cannot-observe" ||
			alerts[0].Target != "redis-2/keyspace-census" || !alerts[0].unobservable ||
			!strings.Contains(alerts[0].Observed, "error_class="+observationErrorClassTimeout) ||
			!strings.Contains(alerts[0].Observed, "observed_masters=3 configured_masters=6 fleet_median=unknown") {
			t.Fatal("partial census changed source taxonomy or acquired relative product authority")
		}
	}
}

// Pure intentional denial remains an exact scope cause, not a contacted-source
// command failure. Mixed scope/source leaves retain both and cannot be denied.
func TestRedisNonexpiringAuthorityErrorScopeCauses(t *testing.T) {
	denial := &hostScopeExcludedError{}
	privateCause := errors.New("synthetic-private-sentinel")
	for _, mixed := range []bool{false, true} {
		var sourceErr error = denial
		if mixed {
			sourceErr = errors.Join(denial, privateCause)
		}
		source := &redisNonexpiringAuthoritySource{syntheticSource: &syntheticSource{}, host: func(context.Context, HostSettings, string) (string, error) {
			return "", sourceErr
		}}
		settings := redisNonexpiringErrorSettings(t, source, false)
		_, err := redisNonexpiringErrorRun(t, "collector", context.Background(), settings)
		var got *hostScopeExcludedError
		if !errors.Is(err, denial) || !errors.As(err, &got) || got != denial || hostScopeOnlyError(err) == mixed {
			t.Fatal("nonexpiring error wrapper changed intentional scope identity")
		}
		if mixed && !errors.Is(err, privateCause) {
			t.Fatal("mixed scope/source error discarded the returned source cause")
		}
		redisNonexpiringAuthorityPrivate(t, nil, err)
	}
}
