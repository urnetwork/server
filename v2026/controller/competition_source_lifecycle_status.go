// Source-index consumers receive authority facts, not private workload bytes
// or an activation timestamp converted into fictitious grading readiness.
package controller

import (
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

const CompetitionSourceLifecycleCapability = "competition.source.lifecycle.v1"

const (
	CompetitionSourceLifecyclePrepared = "prepared"
	CompetitionSourceLifecycleIntake   = "intake"
	CompetitionSourceLifecycleGrading  = "grading"
)

// This deliberately omits the round view's seed and providers download URL.
type CompetitionSourceLifecycleRound struct {
	RoundId  server.Id `json:"round_id"`
	Epoch    int       `json:"epoch"`
	OpensAt  time.Time `json:"opens_at"`
	ClosesAt time.Time `json:"closes_at"`
	Canceled bool      `json:"canceled"`
}

// The concrete read path authenticates each fact before assigning Purpose.
// An empty/unsupported capability must fail closed in offline renderers.
type CompetitionSourceLifecycleStatus struct {
	Schema                  int                                      `json:"schema"`
	Capability              string                                   `json:"capability"`
	Purpose                 string                                   `json:"purpose"`
	Intent                  *model.CompetitionSourceActivation       `json:"intent"`
	Intake                  *model.CompetitionSourceIntakeActivation `json:"intake,omitempty"`
	Round                   *CompetitionSourceLifecycleRound         `json:"round,omitempty"`
	ApiAcknowledged         bool                                     `json:"api_acknowledged"`
	WorkerAcknowledged      bool                                     `json:"worker_acknowledged"`
	GradingRuntimeActivated bool                                     `json:"grading_runtime_activated"`
	BaselineSha256          string                                   `json:"baseline_sha256,omitempty"`
	HostAdmissionReady      bool                                     `json:"host_admission_ready"`
	GradingReady            bool                                     `json:"grading_ready"`
	CompilerEvidenceSha256  string                                   `json:"compiler_evidence_sha256,omitempty"`
}
