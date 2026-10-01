package perfvar

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

const (
	perfvarShapedLinkProtocol = "shaped-link-v1"
	perfvarShapedLinkSchema   = 15
	perfvarHeadroomLimited    = "headroom-limited"
	perfvarHeadroomReason     = "calibration is not at least 10% faster than the tunneled result"
)

// Capacity is attested twice by the same live measurement process. A run
// references the immutable plan and executable; the parser requires matching
// pre/post receipts, host/source metadata, and complete correctness evidence.
type perfvarShapedProcess struct {
	BinarySHA256 string
	PlanSHA256   string
}

func newPerfvarShapedProcess() (perfvarShapedProcess, error) {
	path, err := os.Executable()
	if err != nil {
		return perfvarShapedProcess{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return perfvarShapedProcess{}, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return perfvarShapedProcess{}, err
	}
	return perfvarShapedProcess{hex.EncodeToString(h.Sum(nil)), perfvarCapacityPlanHash()}, nil
}

func perfvarAnnotateShapedRun(record *perfvarRunRecord, process perfvarShapedProcess) {
	record.SchemaVersion = perfvarShapedLinkSchema
	record.GateProtocol = perfvarShapedLinkProtocol
	record.BinarySHA256 = process.BinarySHA256
	record.CapacityPlanSHA256 = process.PlanSHA256
	record.InvalidKind = ""
	switch {
	case !record.Correct:
		record.InvalidKind = "incorrect"
	case record.InvalidReason == perfvarHeadroomReason:
		record.InvalidKind = perfvarHeadroomLimited
	case record.InvalidReason != "":
		record.InvalidKind = "harness-invalid"
	}
}

// This additional aggregate never changes the legacy filtered statistics.
// Every run remains in its correctness/ownership denominator. Numeric shaped
// metrics require every run to be correct and free of non-headroom failures;
// the consumer independently verifies this and both capacity receipts.
type perfvarShapedAggregate struct {
	RunCount         int                `json:"run_count"`
	CorrectRunCount  int                `json:"correct_run_count"`
	EligibleRunCount int                `json:"eligible_run_count"`
	HeadroomRunCount int                `json:"headroom_run_count"`
	Metrics          map[string]float64 `json:"metrics"`
}

func aggregatePerfvarShapedRuns(records []perfvarRunRecord) *perfvarShapedAggregate {
	result := &perfvarShapedAggregate{RunCount: len(records), Metrics: map[string]float64{}}
	copyRecords := append([]perfvarRunRecord(nil), records...)
	for i, record := range records {
		if record.Correct {
			result.CorrectRunCount++
		}
		if record.Correct && record.InvalidReason == "" {
			result.HeadroomRunCount++
		}
		if record.Correct && (record.InvalidReason == "" || record.InvalidKind == perfvarHeadroomLimited && record.InvalidReason == perfvarHeadroomReason) {
			result.EligibleRunCount++
		}
		copyRecords[i].SchemaVersion = perfvarSchemaVersion
		copyRecords[i].InvalidReason = ""
	}
	if len(records) == 0 || result.EligibleRunCount != len(records) {
		return result
	}
	agg := aggregatePerfvarRuns(copyRecords)
	encoded, _ := json.Marshal(agg)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(encoded, &fields)
	for _, name := range []string{
		"goodput_median_gigabits_per_second", "goodput_p95_gigabits_per_second", "goodput_worst_gigabits_per_second",
		"duration_median_nanoseconds", "duration_p95_nanoseconds", "duration_worst_nanoseconds", "setup_median_nanoseconds",
		"latency_p95_median_nanoseconds", "loaded_latency_p95_median_nanoseconds", "efficiency_median", "wire_efficiency_median",
		"dead_window_count", "dead_window_run_count", "window_count", "worst_window_megabits_per_second",
		"memory_heap_and_stack_p50_median_bytes", "memory_heap_and_stack_p95_median_bytes", "memory_heap_and_stack_max_bytes", "memory_samples_above_ceiling",
	} {
		var value float64
		if err := json.Unmarshal(fields[name], &value); err != nil {
			panic(fmt.Sprintf("unknown shaped aggregate metric %s", name))
		}
		result.Metrics[name] = value
	}
	return result
}
