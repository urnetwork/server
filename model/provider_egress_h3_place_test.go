package model

import (
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

// The verdict turns on the other established networks counted: three make the
// place, and the silence then fails the outcome; fewer leave the outcome to
// its HTTPS leg. Neither verdict changes the caller's record.
func TestJudgedProviderEgressH3ThresholdAndCounts(t *testing.T) {
	site := egresshealth.Destination{Name: "h3-site", Class: egresshealth.ClassSite, Url: "https://h3.example/"}
	silent := testingH3UrlResult(server.NewId(), site, time.Date(2026, time.November, 11, 12, 0, 0, 0, time.UTC), egresshealth.UrlProbeH3UdpSilent)
	for others := 0; others <= 4; others++ {
		judged := judgedProviderEgressH3(silent, others)
		place := others < ProviderEgressH3PlaceMinNetworks
		wantVerdict, wantOk, wantFailed := egresshealth.UrlProbeH3VerdictProvider, 0, site.Name
		if place {
			wantVerdict, wantOk, wantFailed = egresshealth.UrlProbeH3VerdictPlace, 1, ""
		}
		if judged.UrlProbeEvidence.H3.Verdict != wantVerdict || judged.OKCount != wantOk || judged.FailedNames != wantFailed ||
			judged.ClassResults["site"] != (ProviderEgressHealthClassResult{OK: wantOk, Total: 1}) {
			t.Fatalf("%d other networks: verdict=%q ok=%d failed=%q classes=%v", others, judged.UrlProbeEvidence.H3.Verdict, judged.OKCount, judged.FailedNames, judged.ClassResults)
		}
		if err := judged.UrlProbeEvidence.ValidateOutcome(judged.OKCount, judged.Total, false); err != nil {
			t.Fatalf("%d other networks: the judged outcome fails validation: %v", others, err)
		}
	}
	if silent.OKCount != 0 || silent.UrlProbeEvidence.H3.Verdict != "" || silent.FailedNames != site.Name || silent.ClassResults["site"].OK != 0 {
		t.Fatal("judging changed the caller's record")
	}
}
