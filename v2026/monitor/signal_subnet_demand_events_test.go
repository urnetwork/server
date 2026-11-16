package monitor

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A synthetic producer database answering the reader's two views.
type subnetDemandEventsTestDatabase struct {
	tableAbsent bool
	tableRows   []Row
	events      []Row
	failure     error
	queries     []string
}

func (self *subnetDemandEventsTestDatabase) query(query string) ([]Row, error) {
	self.queries = append(self.queries, query)
	if self.failure != nil {
		return nil, self.failure
	}
	switch {
	case strings.Contains(query, "to_regclass"):
		if self.tableRows != nil {
			return self.tableRows, nil
		}
		if self.tableAbsent {
			return []Row{{"f"}}, nil
		}
		return []Row{{"t"}}, nil
	case strings.Contains(query, "FROM st_demand_event"):
		return self.events, nil
	}
	return nil, errors.New("unexpected producer query")
}

func newSubnetDemandEventsFixture(t *testing.T) (*subnetDemandEventsTestDatabase, SignalSettings) {
	t.Helper()
	settings, source := subnetReplayTestSettings(t, nil)
	database := &subnetDemandEventsTestDatabase{}
	source.postgresFn = database.query
	return database, settings
}

func runSubnetDemandEvents(t *testing.T, settings SignalSettings) Alerts {
	t.Helper()
	alerts, err := NewSubnetDemandEventsSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatalf("demand events run failed: %v", err)
	}
	return alerts
}

func subnetDemandEventRow(kind string, age time.Duration) Row {
	return Row{subnetProcessTestDeploymentKey(), kind, "7", strconv.FormatInt(int64(age/time.Second), 10)}
}

// Nothing recorded and nothing to read are both silence, including before the
// demand deposit's migration is applied.
func TestSubnetDemandEventsQuiet(t *testing.T) {
	for name, edit := range map[string]func(*subnetDemandEventsTestDatabase){
		"no table":  func(database *subnetDemandEventsTestDatabase) { database.tableAbsent = true },
		"no events": func(*subnetDemandEventsTestDatabase) {},
		"another deployment": func(database *subnetDemandEventsTestDatabase) {
			database.events = []Row{{"945:0x0000000000000000000000000000000000000001", "swap_needs_person", "7", "5"}}
		},
		"a kind that does not page": func(database *subnetDemandEventsTestDatabase) {
			database.events = []Row{subnetDemandEventRow("swapped", time.Minute)}
		},
	} {
		t.Run(name, func(t *testing.T) {
			database, settings := newSubnetDemandEventsFixture(t)
			edit(database)
			if alerts := runSubnetDemandEvents(t, settings); len(alerts) != 0 {
				t.Fatalf("%+v", alerts)
			}
		})
	}
}

// Each kind pages from its newest row to the end of its window. The query never
// names the deployment, and the event's data is never read.
func TestSubnetDemandEventsPage(t *testing.T) {
	for _, test := range []struct {
		kind   string
		window time.Duration
	}{
		{"swap_needs_person", 7 * 24 * time.Hour},
		{"funder_alert", time.Hour},
		{"hotkey_not_ready", time.Hour},
	} {
		t.Run(test.kind, func(t *testing.T) {
			database, settings := newSubnetDemandEventsFixture(t)
			database.events = []Row{subnetDemandEventRow(test.kind, test.window)}
			alerts := runSubnetDemandEvents(t, settings)
			if len(alerts) != 1 {
				t.Fatalf("%+v", alerts)
			}
			alert := requireSubnetReplayAlert(t, alerts, "subnet-demand-event", "subnet-demand-events/"+test.kind, "")
			if alert.Severity != SeverityPage || alert.unobservable {
				t.Fatalf("%+v", alert)
			}
			for _, query := range database.queries {
				if strings.Contains(query, strings.ToLower(subnetReplayTestCoordinator.Hex())) || strings.Contains(query, "data") {
					t.Fatalf("the reader named an identity or read the data: %s", query)
				}
			}
			database.events = []Row{subnetDemandEventRow(test.kind, test.window+time.Second)}
			if alerts := runSubnetDemandEvents(t, settings); len(alerts) != 0 {
				t.Fatalf("past its window: %+v", alerts)
			}
		})
	}
}

// The newest row of every kind is read: repeats of one kind cannot hide another.
func TestSubnetDemandEventsReadTheNewestOfEachKind(t *testing.T) {
	database, settings := newSubnetDemandEventsFixture(t)
	database.events = []Row{subnetDemandEventRow("hotkey_not_ready", time.Minute), subnetDemandEventRow("swap_needs_person", 3*24*time.Hour)}
	alerts := runSubnetDemandEvents(t, settings)
	requireSubnetReplayAlert(t, alerts, "subnet-demand-event", "subnet-demand-events/hotkey_not_ready", "")
	requireSubnetReplayAlert(t, alerts, "subnet-demand-event", "subnet-demand-events/swap_needs_person", "")
	if len(database.queries) != 2 || !strings.Contains(database.queries[1], "DISTINCT ON (deployment_key, kind)") {
		t.Fatalf("%v", database.queries)
	}
}

// A source that fails says nothing about the events, and never pages by itself.
func TestSubnetDemandEventsUnknown(t *testing.T) {
	for name, edit := range map[string]func(*subnetDemandEventsTestDatabase){
		"database unavailable": func(database *subnetDemandEventsTestDatabase) { database.failure = errors.New("down") },
		"malformed table read": func(database *subnetDemandEventsTestDatabase) { database.tableRows = []Row{{"maybe"}} },
		"malformed row": func(database *subnetDemandEventsTestDatabase) {
			database.events = []Row{{subnetProcessTestDeploymentKey(), "funder_alert", "seven", "5"}}
		},
		"too many rows": func(database *subnetDemandEventsTestDatabase) {
			for range subnetDemandEventsMaximumRows + 1 {
				database.events = append(database.events, subnetDemandEventRow("funder_alert", time.Minute))
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			database, settings := newSubnetDemandEventsFixture(t)
			edit(database)
			alerts := runSubnetDemandEvents(t, settings)
			if len(alerts) != 1 || alerts[0].Class != "subnet-monitor-coverage" || alerts[0].Severity == SeverityPage || !strings.Contains(alerts[0].Observed, "subnet-demand-event") {
				t.Fatalf("%+v", alerts)
			}
		})
	}
}

// An explicitly disabled producer records nothing to read.
func TestSubnetDemandEventsDisabledProducer(t *testing.T) {
	database, settings := newSubnetDemandEventsFixture(t)
	settings.STConfigStatus = STConfigurationExplicitDisabled
	settings.VerificationEnabled = false
	settings.STDeploymentKey = ""
	if alerts := runSubnetDemandEvents(t, settings); len(alerts) != 0 || len(database.queries) != 0 {
		t.Fatalf("%+v after %d queries", alerts, len(database.queries))
	}
}

func TestSubnetDemandEventsRegistration(t *testing.T) {
	selected, err := IncludeSignals(NewSignals(), "subnet-demand-events")
	if err != nil || len(selected) != 1 {
		t.Fatal("the demand events reader is absent or duplicated")
	}
	if selected[0].Number() != "22.3a" || selected[0].ID() != subnetDemandEventsProbeId || isVisibilityAlertClass("subnet-demand-event") {
		t.Fatal("registration changed")
	}
}
