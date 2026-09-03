package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

type syntheticSource struct {
	postgresFn    func(string) ([]Row, error)
	redisFn       func(HostSettings, int, ...string) (string, error)
	hostFn        func(HostSettings, string) (string, error)
	hostTimeoutFn func(HostSettings, string, time.Duration) (string, error)
	localFn       func(string, ...string) (string, error)
	tcpFn         func(string, string, []byte, int) ([]byte, error)
}

func (s *syntheticSource) PostgreSQL(_ context.Context, query string) ([]Row, error) {
	if s.postgresFn == nil {
		return nil, nil
	}
	return s.postgresFn(query)
}

func (s *syntheticSource) Redis(_ context.Context, host HostSettings, port int, args ...string) (string, error) {
	if s.redisFn == nil {
		return "", nil
	}
	return s.redisFn(host, port, args...)
}

func (s *syntheticSource) Host(_ context.Context, host HostSettings, command string) (string, error) {
	if s.hostFn == nil {
		return "", nil
	}
	return s.hostFn(host, command)
}

func (s *syntheticSource) HostTimeout(_ context.Context, host HostSettings, command string, timeout time.Duration) (string, error) {
	if s.hostTimeoutFn != nil {
		return s.hostTimeoutFn(host, command, timeout)
	}
	return s.Host(context.Background(), host, command)
}

func (s *syntheticSource) Local(_ context.Context, name string, args ...string) (string, error) {
	if s.localFn == nil {
		return "", fmt.Errorf("unexpected local command %s %s", name, strings.Join(args, " "))
	}
	return s.localFn(name, args...)
}

func (s *syntheticSource) TCPExchange(_ context.Context, network, address string, payload []byte, responseBytes int) ([]byte, error) {
	if s.tcpFn == nil {
		return nil, fmt.Errorf("unexpected TCP exchange %s %s", network, address)
	}
	return s.tcpFn(network, address, payload, responseBytes)
}

func syntheticSettings(source SignalSource) SignalSettings {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	return SignalSettings{
		Environment: "synthetic",
		Source:      source,
		Now:         func() time.Time { return now },
		Hosts: []HostSettings{
			{Name: "pg-1", Roles: []string{"pg-primary"}},
			{Name: "redis-1", Roles: []string{"redis-cluster"}, RedisEntryPort: 6379, RedisNodePorts: []int{6380, 6381, 6382}},
		},
	}
}

func requireAlertClass(t *testing.T, alerts []Alert, class string) Alert {
	t.Helper()
	for _, alert := range alerts {
		if alert.Class == class {
			if alert.SignalNumber == "" || alert.SignalID == "" || alert.Target == "" {
				t.Fatalf("alert is not fully identified: %+v", alert)
			}
			markdown := alert.Markdown()
			for _, section := range []string{"### Symptom", "### Mechanism", "### Expected baseline", "### Observed values", "### Action", "### Verify"} {
				if !strings.Contains(markdown, section) {
					t.Fatalf("alert markdown has no %s: %s", section, markdown)
				}
			}
			return alert
		}
	}
	t.Fatalf("no alert class %q in %+v", class, alerts)
	return Alert{}
}

func requireAlertOmits(t testing.TB, alert Alert, forbidden ...string) {
	t.Helper()
	markdown := alert.Markdown()
	for index, value := range forbidden {
		if value != "" && strings.Contains(markdown, value) {
			t.Fatalf("alert leaked forbidden identifier at fixture index %d", index)
		}
	}
}

func populateMetric(t *testing.T, stateDir, metric string, values ...float64) {
	t.Helper()
	store, err := newBaselineStore(stateDir + "/baseline")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i, value := range values {
		store.record(metric, now.Add(-time.Duration(len(values)-i)*time.Minute), value)
	}
}
