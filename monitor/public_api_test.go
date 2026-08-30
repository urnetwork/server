package monitor_test

import (
	"context"
	"strings"
	"testing"

	servermonitor "github.com/urnetwork/server/monitor"
)

type externalSyntheticSource struct{}

func (externalSyntheticSource) PostgreSQL(context.Context, string) ([]servermonitor.Row, error) {
	return []servermonitor.Row{{"700"}}, nil
}

func (externalSyntheticSource) Redis(context.Context, servermonitor.HostSettings, int, ...string) (string, error) {
	return "", nil
}

func (externalSyntheticSource) Host(context.Context, servermonitor.HostSettings, string) (string, error) {
	return "", nil
}

func TestPublicSignalAPIIsEmbeddable(t *testing.T) {
	settings := servermonitor.SignalSettings{
		Environment: "external-test",
		Source:      externalSyntheticSource{},
		Hosts:       []servermonitor.HostSettings{{Name: "pg-1", Roles: []string{"pg-primary"}}},
	}
	alerts, err := servermonitor.NewContractRateSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 || alerts[0].SignalKey != "contract-rate" || !strings.Contains(alerts.ToMarkdown(), "contracts-collapse") {
		t.Fatalf("unexpected public result: %+v", alerts)
	}
}
