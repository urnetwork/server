package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/maxmind/mmdbwriter/mmdbtype"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Most controls focus on reducer behavior and supply the same immutable
// source facts as a synthetic current census. Join-mismatch controls below
// call Snapshot directly with independent facts.
func shadowTestSnapshot(r *ArinShadowRecorder, census []ArinShadowProvider, required []string, complete bool, maxAge time.Duration) (ArinShadowReport, error) {
	for i := range census {
		census[i].Lookups = map[string]ArinShadowActiveFacts{}
		for _, connection := range census[i].Connections {
			if observed, ok := r.connections[connection]; ok {
				census[i].Lookups[connection] = observed.actual
			}
		}
	}
	return r.Snapshot(census, required, complete, maxAge)
}

func shadowFixture(t *testing.T, risk bool) (*ArinShadowRecorder, ArinShadowActiveFacts) {
	t.Helper()
	record := mmdbtype.Map{"classifier_version": mmdbtype.Uint32(1), "quality_policy_version": mmdbtype.Uint32(2), "quality_state": mmdbtype.String("subscriber"), "non_quality": mmdbtype.Bool(false), "risk": mmdbtype.Bool(risk)}
	if risk {
		record["network_risk_evidence"] = mmdbtype.Slice{mmdbtype.Map{"category": mmdbtype.String("proxy")}}
	}
	bytes := testExceptionDatabase(t, string(schemaTypeArinDb), map[string]mmdbtype.Map{"192.0.2.0/24": record})
	path := filepath.Join(t.TempDir(), "fixture.mmdb")
	if err := os.WriteFile(path, bytes, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(bytes)
	pin := hex.EncodeToString(sum[:])
	now := time.Now().UTC()
	r, err := OpenArinShadowRecorder(path, pin, path, pin, now.Add(-time.Second), 10)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	// Match the recorder clock's microsecond resolution so a fixture cannot
	// appear to come from the future within the same clock tick.
	return r, ArinShadowActiveFacts{Epoch: r.active.Metadata.BuildTime().Unix(), At: NowUtc(), Risk: risk, Verified: !risk}
}
func TestArinShadowRiskStaysIndependentAndOutputPrivate(t *testing.T) {
	r, facts := shadowFixture(t, true)
	if !r.Observe("private-connection", "192.0.2.1", facts) {
		t.Fatal("lookup failed")
	}
	report, err := shadowTestSnapshot(r, []ArinShadowProvider{{Token: "private-provider", Connections: []string{"private-connection"}, Buckets: []string{"us"}, BaseQuality: true, BaseSpeed: true, ActiveQuality: true}}, []string{"us", "ca"}, true, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	row := report.Buckets[1]
	if row.VerifiedSubscriber != 1 || row.Risk != 1 || row.ProxyRisk != 1 || row.CandidateQuality != 0 || row.CandidateSpeed != 0 || row.QualityRemoved != 1 || !report.ObservationComplete || report.ActualMainCoverage {
		t.Fatalf("incorrect independent gates: %+v", report)
	}
	b, _ := json.Marshal(report)
	for _, secret := range []string{"192.0.2.1", "private-provider", "private-connection"} {
		if strings.Contains(string(b), secret) {
			t.Fatal("private identity leaked")
		}
	}
	if report.Buckets[0].Providers != 0 {
		t.Fatal("empty required bucket omitted")
	}
}
func TestArinShadowAllConnectionsFreshAndFailuresFence(t *testing.T) {
	r, f := shadowFixture(t, false)
	if !r.Observe("a", "192.0.2.1", f) {
		t.Fatal("lookup failed")
	}
	census := []ArinShadowProvider{{Token: "p", Connections: []string{"a", "b"}, Buckets: []string{"us"}, BaseQuality: true}}
	report, err := shadowTestSnapshot(r, census, []string{"us"}, true, time.Minute)
	if err != nil || report.ObservationComplete || report.Buckets[0].CandidateQuality != 0 || report.Buckets[0].MissingOrStale != 1 {
		t.Fatal("partial provider promoted")
	}
	if !r.Observe("b", "192.0.2.2", f) {
		t.Fatal("lookup failed")
	}
	report, _ = shadowTestSnapshot(r, census, []string{"us"}, true, time.Minute)
	if !report.ObservationComplete || report.Buckets[0].CandidateQuality != 1 {
		t.Fatal("complete provider not counted")
	}
	f.Epoch++
	if r.Observe("a", "192.0.2.1", f) {
		t.Fatal("mismatched active epoch accepted")
	}
	report, _ = shadowTestSnapshot(r, census, []string{"us"}, true, time.Minute)
	if report.ObservationComplete || report.Buckets[0].CandidateQuality != 0 {
		t.Fatal("failed lookup retained old eligibility")
	}
	r.clock = func() time.Time { return time.Now().UTC().Add(2 * time.Minute) }
	report, _ = shadowTestSnapshot(r, census, []string{"us"}, true, time.Minute)
	if report.ObservationComplete {
		t.Fatal("stale lookup accepted")
	}
}
func TestArinShadowCapacityAndCloseRace(t *testing.T) {
	r, f := shadowFixture(t, false)
	r.capacity = 1
	if !r.Observe("a", "192.0.2.1", f) || r.Observe("b", "192.0.2.2", f) {
		t.Fatal("capacity bound ignored")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); r.Observe("a", "192.0.2.1", f) }()
	}
	r.Close()
	wg.Wait()
	if r.Observe("a", "192.0.2.1", f) {
		t.Fatal("closed recorder accepted observation")
	}
}

func TestArinShadowCensusAndCutoverFailClosed(t *testing.T) {
	r, f := shadowFixture(t, false)
	if !r.Observe("a", "192.0.2.1", f) {
		t.Fatal("lookup failed")
	}
	provider := ArinShadowProvider{Token: "p", Connections: []string{"a"}, Buckets: []string{"us"}, BaseQuality: true, BaseSpeed: true}
	for _, c := range [][]ArinShadowProvider{{provider, provider}, {{Token: "p", Connections: []string{"a", "a"}, Buckets: []string{"us"}}}, {{Token: "p", Connections: []string{"a"}, Buckets: []string{"unlisted"}}}} {
		if _, err := shadowTestSnapshot(r, c, []string{"us"}, true, time.Minute); err == nil {
			t.Fatal("inconsistent census accepted")
		}
	}
	report, err := shadowTestSnapshot(r, []ArinShadowProvider{provider}, []string{"us"}, false, time.Minute)
	if err != nil || report.ObservationComplete || report.ActualMainCoverage {
		t.Fatal("partial source census qualified")
	}
	f.At = r.start.Add(-time.Second)
	if r.Observe("a", "192.0.2.1", f) {
		t.Fatal("source lookup before cutover accepted")
	}
	report, _ = shadowTestSnapshot(r, []ArinShadowProvider{provider}, []string{"us"}, true, time.Minute)
	if report.Buckets[0].CandidateQuality != 0 || report.ObservationComplete {
		t.Fatal("pre-cutover input retained old qualification")
	}
}
