package monitor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

func TestPgBouncerStallsSignalSyntheticUnreachable(t *testing.T) {
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) { return "closed", nil }}
	alerts, err := NewPgBouncerStallsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "pgbouncer-unreachable")
}

func TestPgBouncerStallsSignalSyntheticClientWriteStall(t *testing.T) {
	source := &syntheticSource{
		hostFn: func(HostSettings, string) (string, error) { return "open", nil },
		localFn: func(_ string, args ...string) (string, error) {
			if len(args) > 2 && args[2] == "api" {
				return "pgproto3.writeError=write failed: write tcp 192.0.2.2:50000->192.0.2.1:6432: i/o timeout", nil
			}
			return "", nil
		},
	}
	alerts, err := NewPgBouncerStallsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "pgbouncer-write-stall")
}

func TestPgBouncerStallsSignalListenerCommandDistinguishesObservationFailure(t *testing.T) {
	for _, test := range []struct {
		name        string
		timeoutMode string
		omitTimeout bool
		omitBash    bool
		closed      bool
		wantClass   string
	}{
		{name: "successful connect"},
		{name: "proved refused connect", closed: true, wantClass: "pgbouncer-unreachable"},
		{name: "proved connect timeout", timeoutMode: "timeout-started", wantClass: "pgbouncer-unreachable"},
		{name: "missing timeout", omitTimeout: true, wantClass: "cannot-observe"},
		{name: "missing bash", omitBash: true, wantClass: "cannot-observe"},
		{name: "timeout before owned start", timeoutMode: "timeout-unstarted", wantClass: "cannot-observe"},
		{name: "interrupted owned command", timeoutMode: "interrupted", wantClass: "cannot-observe"},
		{name: "failed command after start", timeoutMode: "failed-started", wantClass: "cannot-observe"},
		{name: "inconsistent timeout outcome", timeoutMode: "timeout-open", wantClass: "cannot-observe"},
		{name: "malformed successful command", timeoutMode: "malformed", wantClass: "cannot-observe"},
	} {
		settings := pgbouncerCommandTestSettings(t, test.timeoutMode, test.omitTimeout, test.omitBash, test.closed)
		alerts, err := NewPgBouncerStallsSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatalf("%s: source failed instead of preserving siblings: %v", test.name, err)
		}
		if test.wantClass == "" {
			if len(alerts) != 0 {
				t.Errorf("%s: successful listener alerted", test.name)
			}
			continue
		}
		found := false
		for _, alert := range alerts {
			if alert.Class == test.wantClass {
				found = true
			}
			if test.wantClass == "cannot-observe" && alert.Class == "pgbouncer-unreachable" {
				t.Errorf("%s: observation failure became a listener outage", test.name)
			}
		}
		if !found {
			t.Errorf("%s: actual command result lacks %s", test.name, test.wantClass)
		}
	}
}

func TestPgBouncerStallsSignalMalformedListenerCannotResolveOutage(t *testing.T) {
	for _, output := range []string{"", "unrelated", "open\nclosed", "OPEN", "closed\nsynthetic-secret"} {
		settings := pgbouncerTestSettings(&syntheticSource{
			hostFn:  func(HostSettings, string) (string, error) { return output, nil },
			localFn: func(string, ...string) (string, error) { return "", nil },
		})
		findings := pgbouncerTestFindings(t, settings)
		visibility := false
		for _, observed := range findings {
			if observed.class == "pgbouncer-unreachable" {
				t.Errorf("invalid listener stdout became outage or recovery: class=%s healthy=%t", observed.class, observed.healthy)
			}
			visibility = visibility || observed.class == "cannot-observe" && !observed.healthy
		}
		if !visibility {
			t.Errorf("invalid listener stdout silently lost source visibility")
		}
	}
}

func TestPgBouncerStallsSignalFailedLogSourcesCannotResolveWriteStall(t *testing.T) {
	for _, output := range []string{"", "partial nonmatching result", "pgproto3.writeError=write failed: write tcp 192.0.2.2:50000->192.0.2.1:6432: i/o timeout synthetic-secret"} {
		settings := pgbouncerTestSettings(&syntheticSource{
			hostFn: func(HostSettings, string) (string, error) { return "open", nil },
			localFn: func(_ string, args ...string) (string, error) {
				if args[2] == "api" {
					return output, errors.New("synthetic-secret transport failure")
				}
				return "", nil
			},
		})
		findings := pgbouncerTestFindings(t, settings)
		visibility, siblingHealthy := false, false
		for _, observed := range findings {
			if observed.class == "pgbouncer-write-stall" && observed.target == "api" {
				t.Errorf("failed log pull supplied incident or recovery evidence: healthy=%t", observed.healthy)
			}
			visibility = visibility || observed.class == "cannot-observe" && observed.target == "api/pgbouncer-write-stall"
			siblingHealthy = siblingHealthy || observed.class == "pgbouncer-write-stall" && observed.target == "connect" && observed.healthy
			if strings.Contains(observed.evidence, "synthetic-secret") || strings.Contains(observed.observed, "synthetic-secret") {
				t.Errorf("failed source retained a raw error or partial payload")
			}
		}
		if !visibility || !siblingHealthy {
			t.Errorf("failed source lost visibility or valid siblings: visibility=%t sibling=%t", visibility, siblingHealthy)
		}
	}
}

func TestPgBouncerStallsSignalCappedLogWindowCannotResolveWriteStall(t *testing.T) {
	settings := pgbouncerTestSettings(&syntheticSource{
		hostFn: func(HostSettings, string) (string, error) { return "open", nil },
		localFn: func(_ string, args ...string) (string, error) {
			if args[2] == "api" {
				return strings.Repeat("pgproto3.writeError=nonmatching synthetic event\n", 1000), nil
			}
			return "", nil
		},
	})
	findings := pgbouncerTestFindings(t, settings)
	visibility := false
	for _, observed := range findings {
		if observed.target == "api" && observed.class == "pgbouncer-write-stall" {
			t.Errorf("limit-sized incomplete log window supplied incident or recovery evidence")
		}
		visibility = visibility || observed.class == "cannot-observe" && observed.target == "api/pgbouncer-write-stall"
	}
	if !visibility {
		t.Fatal("capped source silently became a healthy zero")
	}
}

func TestPgBouncerStallsSignalTruncatedTimestampCannotResolveWriteStall(t *testing.T) {
	settings := pgbouncerTestSettings(&syntheticSource{
		hostFn: func(HostSettings, string) (string, error) { return "open", nil },
		localFn: func(_ string, args ...string) (string, error) {
			if args[2] == "api" {
				return "Warning: at least 1000 log entries share one timestamp. The range api cannot page within one nanosecond; skipping the rest of this timestamp.\n", nil
			}
			return "", nil
		},
	})
	findings := pgbouncerTestFindings(t, settings)
	visibility := false
	for _, observed := range findings {
		if observed.class == "pgbouncer-write-stall" && observed.target == "api" {
			t.Errorf("a skipped timestamp supplied incident or recovery evidence")
		}
		visibility = visibility || observed.class == "cannot-observe" && observed.target == "api/pgbouncer-write-stall"
	}
	if !visibility {
		t.Fatal("a known incomplete timestamp lost source visibility")
	}
}

func TestPgBouncerStallsSignalMatchesOnlyConfiguredDestinationPort(t *testing.T) {
	for _, test := range []struct {
		line      string
		wantStall bool
	}{
		{line: "pgproto3.writeError=write failed: write tcp 192.0.2.2:50000->192.0.2.1:6432: i/o timeout", wantStall: true},
		{line: "pgproto3.writeError=write failed: write tcp [2001:db8::2]:50000->[2001:db8::1]:6432: i/o timeout", wantStall: true},
		{line: "pgproto3.writeError=write failed: write tcp 192.0.2.2:6432->192.0.2.1:5432: i/o timeout"},
		{line: "pgproto3.writeError=write failed: write tcp 192.0.2.2:50000->192.0.2.1:64320: i/o timeout"},
		{line: "pgproto3.writeError=unrelated :6432 diagnostic i/o timeout"},
	} {
		settings := pgbouncerTestSettings(&syntheticSource{
			hostFn: func(HostSettings, string) (string, error) { return "open", nil },
			localFn: func(_ string, args ...string) (string, error) {
				if args[2] == "api" {
					return test.line, nil
				}
				return "", nil
			},
		})
		findings := pgbouncerTestFindings(t, settings)
		found := false
		for _, observed := range findings {
			if observed.class == "pgbouncer-write-stall" && observed.target == "api" {
				found = true
				if observed.healthy == test.wantStall {
					t.Errorf("destination-port classifier gave wrong finding: healthy=%t wantStall=%t", observed.healthy, test.wantStall)
				}
			}
		}
		if !found {
			t.Error("successful complete window lost its typed finding")
		}
	}
}

func TestPgBouncerStallsSignalPublisherJsonAndConfiguredPort(t *testing.T) {
	for _, port := range []int{0, 16432} {
		destinationPort := port
		if destinationPort == 0 {
			destinationPort = 6432
		}
		message := fmt.Sprintf("pgproto3.writeError=write failed: write tcp 192.0.2.2:50000->192.0.2.1:%d: i/o timeout", destinationPort)
		for _, test := range []struct {
			line      string
			wantStall bool
		}{
			{line: message, wantStall: true},
			{line: server.ErrorJsonNoStack(errors.New(message)), wantStall: true},
			{line: server.ErrorJson(errors.New(message), []byte("synthetic frame\n")), wantStall: true},
			{line: server.ErrorJsonWithCustomNoStack(errors.New(message), map[string]any{"route": "synthetic-route"}), wantStall: true},
			{line: message + "ish"},
			{line: server.ErrorJsonNoStack(errors.New(message + "ish"))},
			{line: fmt.Sprintf("pgproto3.writeError=write failed: write tcp 192.0.2.2:%d->192.0.2.1:5432: i/o timeout", destinationPort)},
		} {
			settings := pgbouncerTestSettings(&syntheticSource{
				hostFn: func(HostSettings, string) (string, error) { return "open", nil },
				localFn: func(_ string, args ...string) (string, error) {
					if args[2] == "api" {
						return test.line, nil
					}
					return "", nil
				},
			})
			settings.PostgreSQL.PgBouncerPort = port
			findings := pgbouncerTestFindings(t, settings)
			found := false
			for _, observed := range findings {
				if observed.class == "pgbouncer-write-stall" && observed.target == "api" {
					found = true
					if observed.healthy == test.wantStall {
						t.Errorf("publisher-formatted error classifier gave wrong finding: port=%d healthy=%t wantStall=%t", destinationPort, observed.healthy, test.wantStall)
					}
					if strings.Contains(observed.evidence, "192.0.2.") || strings.Contains(observed.evidence, "synthetic frame") {
						t.Error("publisher error payload entered bounded evidence")
					}
				}
			}
			if !found {
				t.Error("complete publisher-formatted window lost its typed finding")
			}
		}
	}
}

func TestPgBouncerStallsSignalListenerSourceFailurePreservesServiceFindings(t *testing.T) {
	settings := pgbouncerTestSettings(&syntheticSource{
		hostFn: func(HostSettings, string) (string, error) {
			return "closed synthetic-secret", errors.New("synthetic-secret command failed")
		},
		localFn: func(_ string, args ...string) (string, error) {
			if args[2] == "connect" {
				return "pgproto3.writeError=write failed: write tcp 192.0.2.2:50000->192.0.2.1:6432: i/o timeout", nil
			}
			return "", nil
		},
	})
	findings := pgbouncerTestFindings(t, settings)
	visibility, sibling := false, false
	for _, observed := range findings {
		if observed.class == "pgbouncer-unreachable" {
			t.Error("failed listener source supplied outage or recovery evidence")
		}
		visibility = visibility || observed.class == "cannot-observe" && observed.target == "pg.example.test:6432/listener"
		sibling = sibling || observed.class == "pgbouncer-write-stall" && observed.target == "connect" && !observed.healthy
	}
	if !visibility || !sibling {
		t.Fatal("failed listener lost visibility or a valid service sibling")
	}
}

func TestPgBouncerStallsSignalPreservesValidListenerAndLogSiblings(t *testing.T) {
	settings := pgbouncerTestSettings(&syntheticSource{
		hostFn: func(HostSettings, string) (string, error) { return "closed", nil },
		localFn: func(_ string, args ...string) (string, error) {
			switch args[2] {
			case "api":
				return "", errors.New("synthetic-secret observation failure")
			case "connect":
				return "pgproto3.writeError=write failed: write tcp 192.0.2.2:50000->192.0.2.1:6432: i/o timeout synthetic-secret", nil
			default:
				return "", nil
			}
		},
	})
	alerts, err := NewPgBouncerStallsSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	listener := requireAlertClass(t, alerts, "pgbouncer-unreachable")
	stall := requireAlertClass(t, alerts, "pgbouncer-write-stall")
	visibility := requireAlertClass(t, alerts, "cannot-observe")
	if listener.Target != "pg.example.test:6432" || listener.Severity != SeverityWarn || listener.Sustain != 2 || stall.Target != "connect" || visibility.Target != "api/pgbouncer-write-stall" {
		t.Fatalf("composite source loss discarded valid sibling findings")
	}
	for _, value := range []string{"### Mechanism", "### Evidence", "### Context", "SIGNALS.md §2.11", "authentication", "direct 5432"} {
		if !strings.Contains(listener.Markdown(), value) {
			t.Errorf("listener Markdown lacks %q", value)
		}
	}
	for _, alert := range alerts {
		for _, value := range []string{"synthetic-secret", "192.0.2.2", "192.0.2.1", "50000"} {
			if strings.Contains(alert.Markdown(), value) {
				t.Errorf("bounded Alert retained a raw error/tuple exemplar")
			}
		}
	}
}

func TestPgBouncerStallsSignalCancellationDoesNotContactOrAlert(t *testing.T) {
	calls := 0
	settings := pgbouncerTestSettings(&syntheticSource{
		hostFn:  func(HostSettings, string) (string, error) { calls++; return "closed", nil },
		localFn: func(string, ...string) (string, error) { calls++; return "", nil },
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	alerts, err := NewPgBouncerStallsSignal().Run(ctx, settings)
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 || calls != 0 {
		t.Fatalf("canceled transport work became incident evidence: alerts=%d calls=%d err=%v", len(alerts), calls, err)
	}
}

func TestPgBouncerStallsSignalMidObservationCancellationWithholdsFindings(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	settings := pgbouncerTestSettings(&syntheticSource{
		hostFn: func(HostSettings, string) (string, error) {
			calls++
			cancel()
			return "closed", nil
		},
		localFn: func(string, ...string) (string, error) { calls++; return "", nil },
	})
	alerts, err := NewPgBouncerStallsSignal().Run(ctx, settings)
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 || calls != 1 {
		t.Fatalf("cancellation after listener observation supplied incident evidence: alerts=%d calls=%d err=%v", len(alerts), calls, err)
	}
}

func pgbouncerTestSettings(source *syntheticSource) SignalSettings {
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{Name: "pg.example.test", Roles: []string{"pg-primary"}}}
	return settings
}

func pgbouncerTestFindings(t *testing.T, settings SignalSettings) []finding {
	t.Helper()
	env, err := newProbeEnv(settings.withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	findings, err := (pgbouncerProbe{}).check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	return findings
}

func pgbouncerCommandTestSettings(t *testing.T, timeoutMode string, omitTimeout, omitBash, closed bool) SignalSettings {
	t.Helper()
	binDir := t.TempDir()
	realSh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	realBash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	realSed, err := exec.LookPath("sed")
	if err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(binDir, "socket")
	if closed {
		socketPath = filepath.Join(binDir, "missing", "socket")
	} else if err := os.WriteFile(socketPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	commands := map[string]string{}
	if !omitTimeout {
		commands["timeout"] = `#!/bin/sh
case "$PGBOUNCER_TEST_TIMEOUT_MODE" in
  timeout-started) printf '%s\n' started; exit 124 ;;
  timeout-unstarted) exit 124 ;;
  interrupted) printf '%s\n' started; exit 143 ;;
  failed-started) printf '%s\n' started; exit 1 ;;
  timeout-open) printf '%s\n' started open; exit 124 ;;
  malformed) printf '%s\n' malformed; exit 0 ;;
  *) shift; exec "$@" ;;
esac
`
	}
	if !omitBash {
		commands["bash"] = `#!/bin/sh
[ "$1" = -c ] || exit 64
script=$(printf '%s' "$2" | "$PGBOUNCER_TEST_REAL_SED" "s#/dev/tcp/127.0.0.1/6432#$PGBOUNCER_TEST_SOCKET#g")
exec "$PGBOUNCER_TEST_REAL_BASH" -c "$script"
`
	}
	for name, body := range commands {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return pgbouncerTestSettings(&syntheticSource{
		hostFn: func(_ HostSettings, command string) (string, error) {
			execute := exec.Command(realSh, "-c", command)
			execute.Env = append(os.Environ(), "PATH="+binDir, "PGBOUNCER_TEST_TIMEOUT_MODE="+timeoutMode,
				"PGBOUNCER_TEST_SOCKET="+socketPath, "PGBOUNCER_TEST_REAL_BASH="+realBash, "PGBOUNCER_TEST_REAL_SED="+realSed)
			output, err := execute.CombinedOutput()
			if err != nil {
				return string(output), fmt.Errorf("synthetic listener command: %w", err)
			}
			return string(output), nil
		},
		localFn: func(string, ...string) (string, error) { return "", nil },
	})
}
