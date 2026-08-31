package monitor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type lifecycleTimeoutSource struct {
	*syntheticSource
	gatewayDone chan error
}

func (s *lifecycleTimeoutSource) Local(ctx context.Context, _ string, _ ...string) (string, error) {
	<-ctx.Done()
	s.gatewayDone <- ctx.Err()
	return "", ctx.Err()
}

func TestReadTaskLifecycleLogBoundsGatewayBeforeJournalFallback(t *testing.T) {
	now := time.Date(2026, 8, 31, 4, 16, 50, 0, time.UTC)
	journal := fmt.Sprintf(
		`{"SYSLOG_TIMESTAMP":%q,"MESSAGE":%q,"CONTAINER_TAG":"warp|synthetic|taskworker|g2","CONTAINER_ID":"hot","_HOSTNAME":"metrics-1"}`,
		now.Add(-5*time.Second).Format(time.RFC3339Nano),
		"I0831 04:16:45.000000 1 task.go:1938] [01a055c8-759e-406e-4061-603f0dc86869]eval active(4190.00s) github.com/urnetwork/server/taskworker/work.UpdateClientScores({})",
	)
	source := &lifecycleTimeoutSource{
		syntheticSource: &syntheticSource{hostTimeoutFn: func(_ HostSettings, command string, timeout time.Duration) (string, error) {
			if timeout != taskworkerJournalTimeout || !strings.Contains(command, "--grep='eval'") {
				t.Fatalf("unexpected journal fallback: timeout=%s command=%s", timeout, command)
			}
			return journal, nil
		}},
		gatewayDone: make(chan error, 1),
	}
	settings := workerMemorySyntheticSettings(source, now).withDefaults()
	env, err := newProbeEnv(settings)
	if err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	output, logSource, err := readTaskLifecycleLogWithGatewayTimeout(
		context.Background(),
		env,
		"eval",
		2*time.Minute,
		5000,
		10*time.Millisecond,
	)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("bounded gateway fallback took %s", elapsed)
	}
	if logSource != "host-journal-fallback" || !strings.Contains(output, "UpdateClientScores") {
		t.Fatalf("fallback source/output = %q %q", logSource, output)
	}
	if gatewayErr := <-source.gatewayDone; !errors.Is(gatewayErr, context.DeadlineExceeded) {
		t.Fatalf("gateway context error = %v, want deadline exceeded", gatewayErr)
	}
}

func TestReadTaskLifecycleLogTreatsJournalNoMatchAsEmptyHost(t *testing.T) {
	now := time.Date(2026, 8, 31, 5, 46, 0, 0, time.UTC)
	journal := fmt.Sprintf(
		`{"SYSLOG_TIMESTAMP":%q,"MESSAGE":%q,"CONTAINER_TAG":"warp|synthetic|taskworker|g1","CONTAINER_ID":"hot","_HOSTNAME":"metrics-1"}`,
		now.Add(-5*time.Second).Format(time.RFC3339Nano),
		"I0831 05:45:55.000000 1 task.go:1938] [01a05616-2af9-07af-9ce6-8ba1bc304862]eval active(4450.00s) github.com/urnetwork/server/taskworker/work.UpdateClientScores({})",
	)
	calls := make(chan string, 2)
	source := &syntheticSource{
		hostTimeoutFn: func(host HostSettings, command string, _ time.Duration) (string, error) {
			calls <- host.Name + "\n" + command
			if host.Name == "metrics-1" {
				return journal, nil
			}
			// The rendered remote command converts journalctl --grep's status 1
			// into this successful empty observation on a host with no matches.
			return "", nil
		},
		localFn: func(string, ...string) (string, error) {
			return "", errors.New("synthetic fleet gateway unavailable")
		},
	}
	settings := workerMemorySyntheticSettings(source, now)
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-empty", Roles: []string{"services"}})
	env, err := newProbeEnv(settings.withDefaults())
	if err != nil {
		t.Fatal(err)
	}

	output, logSource, err := readTaskLifecycleLog(
		context.Background(),
		env,
		"eval",
		2*time.Minute,
		5000,
	)
	if err != nil {
		t.Fatal(err)
	}
	if logSource != "host-journal-fallback" || !strings.Contains(output, "UpdateClientScores") {
		t.Fatalf("fallback source/output = %q %q", logSource, output)
	}
	for range 2 {
		call := <-calls
		for _, want := range []string{
			"journal_status=$?",
			`if [ "$journal_status" -eq 1 ]; then exit 0; fi`,
			`exit "$journal_status"`,
		} {
			if !strings.Contains(call, want) {
				t.Fatalf("journal command did not preserve no-match semantics %q:\n%s", want, call)
			}
		}
	}
}
