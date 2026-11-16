// Unknown executions interrupt only their owning signal's recovery streak.
package monitor

import (
	"testing"
	"time"
)

// A complete absence on either side of unknown evidence is not consecutive.
func TestSlackAlertNotifierUnknownRestartsCompleteRecovery(t *testing.T) {
	unknown := slackTestAlert("unknown.example", "", SeverityWarn)
	unknown.unobservable = true
	for _, interruption := range []SignalExecution{
		{Failed: true},
		{Alerts: Alerts{unknown}},
		{HeldAlerts: Alerts{unknown}},
	} {
		notifier := &SlackAlertNotifier{
			settings:              SlackAlertNotifierSettings{ClearAfterCompleteRuns: 2},
			keyIncidents:          map[string]*slackIncident{},
			signalKeyCompleteRuns: map[string]int{},
		}
		now := time.Date(2037, 1, 2, 3, 4, 0, 0, time.UTC)
		alert := slackTestAlert("database.example", "", SeverityPage)
		key := slackIncidentKey(alert)
		raised := notifier.observeWithLock(now, "owner", SignalExecution{Alerts: Alerts{alert}})
		if len(raised) != 1 || raised[0].kind != slackRaised {
			t.Fatal("incident was not admitted exactly once")
		}
		notifier.observeWithLock(now, "owner", SignalExecution{})
		if notifier.keyIncidents[key].signalKeyAbsentRuns["owner"] != 1 {
			t.Fatal("first complete absence did not accrue exactly one run")
		}
		notifier.observeWithLock(now, "owner", interruption)
		incident, present := notifier.keyIncidents[key]
		if !present || incident.signalKeyAbsentRuns["owner"] != 0 || incident.severity != SeverityPage {
			t.Fatal("unknown evidence removed the PAGE or retained recovery credit")
		}
		notifier.observeWithLock(now, "owner", SignalExecution{})
		if _, present := notifier.keyIncidents[key]; !present {
			t.Fatal("first complete run after unknown cleared incident")
		}
		cleared := notifier.observeWithLock(now, "owner", SignalExecution{})
		if _, present := notifier.keyIncidents[key]; present {
			t.Fatal("two consecutive complete absences did not clear incident")
		}
		found := 0
		for _, transition := range cleared {
			if slackIncidentKey(transition.alert) == key {
				if transition.kind != slackCleared || transition.alert.Severity != SeverityPage {
					t.Fatal("recovery changed transition or retained severity")
				}
				found++
			}
		}
		if found != 1 {
			t.Fatal("recovery did not clear the incident exactly once")
		}
	}
}

// Unknown without an incident creates no ownership; sibling recovery stays local.
func TestSlackAlertNotifierUnknownRecoveryPreservesOtherOwners(t *testing.T) {
	notifier := &SlackAlertNotifier{
		settings:              SlackAlertNotifierSettings{ClearAfterCompleteRuns: 2},
		keyIncidents:          map[string]*slackIncident{},
		signalKeyCompleteRuns: map[string]int{},
	}
	now := time.Date(2037, 1, 2, 3, 4, 0, 0, time.UTC)
	if transitions := notifier.observeWithLock(now, "absent-owner", SignalExecution{Failed: true}); len(transitions) != 0 || len(notifier.keyIncidents) != 0 {
		t.Fatal("unobserved source invented an incident or ownership")
	}
	alert := slackTestAlert("database.example", "", SeverityWarn)
	key := slackIncidentKey(alert)
	notifier.observeWithLock(now, "owner", SignalExecution{Alerts: Alerts{alert}})
	alert.Severity = SeverityPage
	escalated := notifier.observeWithLock(now, "sibling", SignalExecution{Alerts: Alerts{alert}})
	if len(escalated) != 1 || escalated[0].kind != slackEscalated {
		t.Fatal("mixed-severity reporter did not escalate exactly once")
	}
	notifier.observeWithLock(now, "owner", SignalExecution{})
	notifier.observeWithLock(now, "sibling", SignalExecution{})
	notifier.observeWithLock(now, "owner", SignalExecution{Failed: true})
	incident := notifier.keyIncidents[key]
	if incident.signalKeyAbsentRuns["owner"] != 0 || incident.signalKeyAbsentRuns["sibling"] != 1 {
		t.Fatal("unknown reset another owner's progress or retained its own")
	}
	notifier.observeWithLock(now, "sibling", SignalExecution{})
	if _, owned := notifier.keyIncidents[key].signalKeyAbsentRuns["sibling"]; owned {
		t.Fatal("sibling did not release after its own consecutive complete runs")
	}
	notifier.observeWithLock(now, "owner", SignalExecution{})
	if incident.signalKeyAbsentRuns["owner"] != 1 {
		t.Fatal("complete owner absence did not accrue before positive interrupt")
	}
	notifier.observeWithLock(now, "owner", SignalExecution{HeldAlerts: Alerts{alert}})
	if incident.signalKeyAbsentRuns["owner"] != 0 {
		t.Fatal("held affirmative observation counted as recovery")
	}
	notifier.observeWithLock(now, "owner", SignalExecution{})
	if _, present := notifier.keyIncidents[key]; !present {
		t.Fatal("single remaining-owner absence cleared shared PAGE")
	}
	transitions := notifier.observeWithLock(now, "owner", SignalExecution{})
	if len(transitions) != 1 || transitions[0].kind != slackCleared || transitions[0].alert.Severity != SeverityPage || len(notifier.keyIncidents) != 0 {
		t.Fatal("last owner's complete recovery did not clear retained PAGE once")
	}
}
