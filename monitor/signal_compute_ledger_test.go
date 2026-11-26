package monitor

// SIGNALS.md §32.1 (CSRV-M12-06): each ledger page at its line and not below it, on recorded series.

import (
	"testing"
)

// Settlement lag pages at 61 s and not at 59 s.
func TestComputeSettlementLagPagesPastSixtySeconds(t *testing.T) {
	settings := computeTestSettings("ledger")
	for _, c := range []struct {
		lag   float64
		pages bool
	}{{lag: 61, pages: true}, {lag: 59, pages: false}} {
		findings := computeFamilyJudge(computeLedgerFamily(), settings, []*sidecarStorageSample{
			computeTestLive(),
			computeTestSample("compute-settlement-lag", "lag", c.lag, nil),
		})
		lag := computeTestFindings(findings, "compute-settlement-lag")["compute"]
		if lag.healthy == c.pages || lag.unobservable || (c.pages && (lag.tier != tierPage || lag.sustain != settings.PageSustain)) {
			t.Errorf("a lag of %.0f s: %+v", c.lag, lag)
		}
	}
}

// An operation row open past 24 hours, or settled and unbooked past 60 s, pages; one under both does not.
func TestComputeOpStalePagesPastEitherLine(t *testing.T) {
	settings := computeTestSettings("ledger")
	for _, c := range []struct {
		open, settled float64
		pages         bool
	}{
		{open: 24*60*60 + 1, settled: 0, pages: true},
		{open: 0, settled: 61, pages: true},
		{open: 24*60*60 - 1, settled: 59, pages: false},
	} {
		findings := computeFamilyJudge(computeLedgerFamily(), settings, []*sidecarStorageSample{
			computeTestLive(),
			computeTestSample("compute-op-stale", "open", c.open, nil),
			computeTestSample("compute-op-stale", "settled", c.settled, nil),
		})
		if stale := computeTestFindings(findings, "compute-op-stale")["compute"]; stale.healthy == c.pages || stale.unobservable {
			t.Errorf("open %.0f s, settled %.0f s: %+v", c.open, c.settled, stale)
		}
	}
}

// compute_rollup_day's alarm pages within the window; absorbed charges past $50 a day page, and $49 does not.
func TestComputeLedgerAlarmsAndAbsorbedCharges(t *testing.T) {
	settings := computeTestSettings("ledger")
	findings := computeFamilyJudge(computeLedgerFamily(), settings, []*sidecarStorageSample{
		computeTestLive(),
		computeTestSample("compute-rollup-mismatch", "alarms", 1, nil),
		computeTestSample("compute-direct-absorbed-high", "absorbed", 51*1000*1000*1000, nil),
	})
	if rollup := computeTestFindings(findings, "compute-rollup-mismatch")["compute"]; rollup.healthy {
		t.Errorf("a rollup alarm: %+v", rollup)
	}
	if absorbed := computeTestFindings(findings, "compute-direct-absorbed-high")["compute"]; absorbed.healthy {
		t.Errorf("$51 absorbed: %+v", absorbed)
	}
	findings = computeFamilyJudge(computeLedgerFamily(), settings, []*sidecarStorageSample{
		computeTestLive(),
		computeTestSample("compute-direct-absorbed-high", "absorbed", 49*1000*1000*1000, nil),
	})
	if absorbed := computeTestFindings(findings, "compute-direct-absorbed-high")["compute"]; !absorbed.healthy {
		t.Errorf("$49 absorbed: %+v", absorbed)
	}
}
