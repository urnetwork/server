// Selection observations retain the bounded reason distribution independently of alerts.
package monitor

import (
	"encoding/json"
	"io"
	"sort"
	"time"
)

// Only validated finite labels leave the private process join. Requests are
// counter increases over the window, not distinct requests or users.
type ProviderSelectionOutcomeObservation struct {
	TargetKind   string  `json:"target_kind"`
	RequestClass string  `json:"request_class"`
	IpFamily     string  `json:"ip_family"`
	RankMode     string  `json:"rank_mode"`
	Outcome      string  `json:"outcome"`
	Reason       string  `json:"reason"`
	Requests     float64 `json:"requests"`
}

// A complete source window is not proof that any particular target was healthy.
// Lazy first events can be absent, and omitted partitions never mean zero.
type ProviderSelectionObservation struct {
	SchemaVersion          int                                   `json:"schema_version"`
	Environment            string                                `json:"environment"`
	ObservedAt             time.Time                             `json:"observed_at"`
	WindowStart            time.Time                             `json:"window_start"`
	WindowSeconds          int                                   `json:"window_seconds"`
	Reason                 string                                `json:"reason"`
	SourceCoverageComplete bool                                  `json:"source_coverage_complete"`
	ShapeCoverageComplete  bool                                  `json:"shape_coverage_complete"`
	PairedProcesses        int                                   `json:"paired_processes"`
	ShapePairedProcesses   int                                   `json:"shape_paired_processes"`
	ExpectedSlots          int                                   `json:"expected_slots"`
	CountsScope            string                                `json:"counts_scope"`
	Outcomes               []ProviderSelectionOutcomeObservation `json:"outcomes"`
}

// A failed or partial local append is visible without dropping actual alerts.
func (self ProviderSelectionObservation) WriteJsonl(output io.Writer) error {
	return json.NewEncoder(shortWriteErrorWriter{Writer: output}).Encode(self)
}

// Do not manufacture absent reason rows, including cache_missing=0. Valid
// positive evidence survives a partial fleet; rejected source data does not.
func newProviderSelectionObservation(environment string, now time.Time, evidence providerSelectionEvidence) ProviderSelectionObservation {
	observation := ProviderSelectionObservation{
		SchemaVersion: 1, Environment: environment, ObservedAt: now,
		WindowStart: now.Add(-5 * time.Minute), WindowSeconds: 300,
		Reason: evidence.reason, SourceCoverageComplete: evidence.complete,
		ShapeCoverageComplete: evidence.shapeComplete, PairedProcesses: evidence.paired,
		ShapePairedProcesses: evidence.shapePaired, ExpectedSlots: evidence.expected,
		CountsScope: "observed-process-subset", Outcomes: []ProviderSelectionOutcomeObservation{},
	}
	for key, count := range evidence.counts {
		if count <= 0 {
			continue
		}
		observation.Outcomes = append(observation.Outcomes, ProviderSelectionOutcomeObservation{
			TargetKind: key.targetKind, RequestClass: key.requestClass, IpFamily: key.family,
			RankMode: key.rankMode, Outcome: key.outcome, Reason: key.reason, Requests: count,
		})
	}
	sort.Slice(observation.Outcomes, func(i, j int) bool {
		a, b := observation.Outcomes[i], observation.Outcomes[j]
		return a.TargetKind+"/"+a.RequestClass+"/"+a.IpFamily+"/"+a.RankMode+"/"+a.Outcome+"/"+a.Reason < b.TargetKind+"/"+b.RequestClass+"/"+b.IpFamily+"/"+b.RankMode+"/"+b.Outcome+"/"+b.Reason
	})
	return observation
}
