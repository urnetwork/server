package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// URL evidence is taken from the history's ingest age back to a minute ahead,
// both bounds included, at a fixed clock; a report without evidence is timed
// at arrival and never refused for its time (PARTITION-MAINTENANCE-PLAN.md
// C2). The refusal comes before any store.
func TestProviderEgressHealthIngestTimeBounds(t *testing.T) {
	settings := model.DefaultProviderEgressHealthHistorySettings()
	now := time.Date(2026, 10, 24, 15, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		measuredAt time.Time
		refusal    string
	}{
		{now, ""},
		{now.Add(-settings.MaxIngestAge), ""},
		{now.Add(-settings.MaxIngestAge - time.Microsecond), "too old"},
		{now.AddDate(0, 0, -8), "too old"},
		{now.Add(time.Minute), ""},
		{now.Add(time.Minute + time.Microsecond), "in the future"},
	} {
		args := &SubmitProviderEgressHealthArgs{UrlProbeEvidence: statsEgressTestUrlEvidence(test.measuredAt, true)}
		err := refuseProviderEgressHealthTime(args, now, settings)
		if test.refusal == "" {
			if err != nil {
				t.Fatalf("%s refused: %v", test.measuredAt, err)
			}
			continue
		}
		refusal := ProviderEgressIngestRefusalOf(err)
		if refusal == nil || refusal.Status != 400 || !strings.Contains(refusal.Message, test.refusal) {
			t.Fatalf("%s: refusal %+v, want %q", test.measuredAt, refusal, test.refusal)
		}
	}
	if err := refuseProviderEgressHealthTime(&SubmitProviderEgressHealthArgs{}, now, settings); err != nil {
		t.Fatalf("a report without evidence was refused: %v", err)
	}
	// through the ingest: an old receipt is refused before anything is read
	args := &SubmitProviderEgressHealthArgs{
		ClientId:         server.NewId(),
		RunId:            server.NewId(),
		UrlProbeEvidence: statsEgressTestUrlEvidence(time.Now().UTC().Add(-settings.MaxIngestAge-time.Hour), true),
		OKCount:          1,
		TotalCount:       1,
		ClassResults:     map[string]ProviderEgressHealthClassResult{"site": {OK: 1, Total: 1}},
	}
	if refusal := ProviderEgressIngestRefusalOf(SubmitProviderEgressHealth(t.Context(), args)); refusal == nil || !strings.Contains(refusal.Message, "too old") {
		t.Fatalf("the ingest took an old receipt: %+v", refusal)
	}
}
