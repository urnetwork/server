// Synthetic retained evidence proves the exception cannot waive another metric,
// substitute another document or change the original failed judgement.
package controller

import (
	"encoding/json"
	"strings"
	"testing"
)

// Construct the failed screen from run-level samples, not a mocked verdict.
func admissionExceptionTestReceipt(t *testing.T) (*competitionNoopScreenFixture, *competitionExecutorEquivalenceReceipt) {
	t.Helper()
	f := newCompetitionNoopScreenFixture(t)
	r, err := screenCompetitionExecutorNoop(f.settings, f.job, f.host, f.directory, f.response, f.outcome)
	if err != nil {
		t.Fatal(err)
	}
	for index, metric := range r.Metrics {
		if metric.Metric != "throughput_p50_bytes_per_s" {
			continue
		}
		candidate := append([]float64(nil), metric.CandidateValues...)
		for i := range candidate {
			candidate[i] *= 0.5
		}
		changed, err := compareCompetitionHostMetric(metric.Metric, metric.BaselineValues, candidate)
		if err != nil || !changed.SignificantShift {
			t.Fatal("synthetic shift was not reproduced", err)
		}
		changed.BaselineWithinRunSe, changed.CandidateWithinRunSe = metric.BaselineWithinRunSe, metric.CandidateWithinRunSe
		r.Metrics[index] = *changed
	}
	r.Passed = false
	sealCompetitionNoopScreenReceipt(t, f.settings, r)
	return f, r
}

func TestExecutorAdmissionExceptionAcceptsExactFailedMetricWithoutRelabeling(t *testing.T) {
	f, r := admissionExceptionTestReceipt(t)
	if validateCompetitionExecutorReceipt(f.settings, &f.job.Round, f.job.RoundBaseline, f.host, r) == nil {
		t.Fatal("strict screen accepted failed sample")
	}
	before, _ := json.Marshal(r)
	if err := validateCompetitionExecutorReceiptWithException(f.settings, &f.job.Round, f.job.RoundBaseline, f.host, r, r.Retention.Sha256); err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(r)
	if string(before) != string(after) || r.Passed {
		t.Fatal("exception rewrote failed judgement")
	}
}

func TestExecutorAdmissionExceptionRejectsChangedEvidenceAndOtherFailures(t *testing.T) {
	for _, change := range []string{"document", "host", "baseline", "statistics", "other-metric", "passed", "missing-metric", "raw-bound"} {
		f, r := admissionExceptionTestReceipt(t)
		expected := r.Retention.Sha256
		switch change {
		case "document":
			expected = strings.Repeat("0", 64)
		case "host":
			r.HostId = "synthetic-other-host"
		case "baseline":
			r.BaselineSha256 = strings.Repeat("0", 64)
		case "statistics":
			r.Metrics[0].TwoSidedPValue = 0.12345
		case "other-metric":
			metricIndex := -1
			for i, metric := range r.Metrics {
				if metric.Metric == "ttfb_p95_ms" {
					metricIndex = i
				}
			}
			if metricIndex < 0 {
				t.Fatal("synthetic TTFB metric absent")
			}
			m := r.Metrics[metricIndex]
			values := append([]float64(nil), m.CandidateValues...)
			for i := range values {
				values[i] *= 0.5
			}
			changed, err := compareCompetitionHostMetric(m.Metric, m.BaselineValues, values)
			if err != nil {
				t.Fatal(err)
			}
			changed.BaselineWithinRunSe, changed.CandidateWithinRunSe = m.BaselineWithinRunSe, m.CandidateWithinRunSe
			r.Metrics[metricIndex] = *changed
		case "passed":
			r.Passed = true
		case "missing-metric":
			r.Metrics = r.Metrics[:len(r.Metrics)-1]
		case "raw-bound":
			r.RawScoreRelativeDifference = 0.06
		}
		if change != "document" {
			sealCompetitionNoopScreenReceipt(t, f.settings, r)
			expected = r.Retention.Sha256
		}
		if validateCompetitionExecutorReceiptWithException(f.settings, &f.job.Round, f.job.RoundBaseline, f.host, r, expected) == nil {
			t.Fatal("exception accepted altered scope/evidence", change)
		}
	}
}

func TestExecutorAdmissionExceptionApprovalBindsOneDocumentAndMetric(t *testing.T) {
	f, r := admissionExceptionTestReceipt(t)
	value := CompetitionExecutorAdmissionException{Schema: 1, Kind: "executor-retained-admission-exception/v1", RoundId: f.job.RoundId, HostId: f.host.HostId, JobId: f.job.JobId, BaselineSha256: r.BaselineSha256, ReceiptDocumentSha256: r.Retention.Sha256, ExceptedMetric: "throughput_p50_bytes_per_s", EvidenceSha256: strings.Repeat("b", 64)}
	if err := validateCompetitionAdmissionException(value, r); err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"metric", "evidence", "job", "round", "digest"} {
		v := value
		switch change {
		case "metric":
			v.ExceptedMetric = "ttfb_p95_ms"
		case "evidence":
			v.EvidenceSha256 = ""
		case "job":
			v.JobId[0]++
		case "round":
			v.RoundId[0]++
		case "digest":
			v.ReceiptDocumentSha256 = strings.Repeat("0", 64)
		}
		if validateCompetitionAdmissionException(v, r) == nil {
			t.Fatal("approval scope was not exact", change)
		}
	}
}
