package monitor

import (
	"context"
	"strings"
	"testing"
)

func testEgressPublicationAlert(t *testing.T, missing bool) Alerts {
	t.Helper()
	snapshot := egressCoverageSnapshot{
		shardIndex: 0, eligible: 12, fullAgeSeconds: 600, blackholeAgeSeconds: 600,
		fullCurrent: 12, blackholeCurrent: 12, fullAttemptsLastHour: 1, blackholeLastHour: 12,
		staleLocationOldestAgeSeconds: -1, staleHealthOldestAgeSeconds: -1,
	}
	if missing {
		snapshot.blackholeCurrent = 0
		snapshot.blackholeLastHour = 0
		snapshot.blackholeDue = 12
		snapshot.blackholeVerdictDue = 12
		snapshot.blackholeAgeSeconds = 6600
	}
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "pg_attribute"):
			return []Row{{"t", "t", "t"}}, nil
		case strings.Contains(query, "FROM pending_task"):
			return []Row{syntheticEgressCoverageTask(t, 0, 1)}, nil
		case strings.Contains(query, "WITH lifecycle_clock AS"):
			return []Row{syntheticEgressCoverageActivity(snapshot)}, nil
		default:
			t.Fatal("unexpected coverage query")
			return nil, nil
		}
	}}
	alerts, err := syntheticEgressCoverageSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	requireEgressConfigPrivacy(t, alerts)
	return alerts
}

func TestEgressCoveragePublicationUnknownKeepsMeasuredExposure(t *testing.T) {
	alert := requireAlertClass(t, testEgressPublicationAlert(t, true), "egress-blackhole-stalled")
	markdown := alert.Markdown()
	for _, want := range []string{
		"publication_progress=unobserved", "batch barrier", "completed_buffered",
		"not a durable verdict", "claim_time", "not an execution-start clock",
		"process/start", "source-time", "NotMeasured", "returned acknowledgement",
		"not a submission counter", "Do not delete provider evidence",
	} {
		if !strings.Contains(markdown, want) {
			t.Errorf("publication qualifier missing %q", want)
		}
	}
	if alert.Class != "egress-blackhole-stalled" {
		t.Fatal("publication qualifier changed alert identity")
	}
}

func TestEgressCoveragePublicationHealthyMeasuredRemainsHealthy(t *testing.T) {
	if alerts := testEgressPublicationAlert(t, false); len(alerts) != 0 {
		t.Fatalf("publication unknown invented an alert over healthy measured evidence: %d", len(alerts))
	}
}
