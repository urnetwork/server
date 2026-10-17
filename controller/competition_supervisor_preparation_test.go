// Exact preparation acknowledgment and private source-setting projection do
// not depend on live services, timing, or a self-asserted adapter exit code.
package controller

import (
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Missing or different-role/source/round receipts leave the adapter retryable;
// they can never be cached as completed preparation for the sixteen-hour wait.
func TestSourcePreparationRequiresBothExactRuntimeReceipts(t *testing.T) {
	intent := &model.CompetitionSourceActivation{Environment: "synthetic", CompetitionId: "competition", SeasonId: "season", Generation: 3, TargetRoundId: server.NewId(), ManifestCommit: strings.Repeat("a", 40), ManifestSha256: strings.Repeat("b", 64), SourceLockSha256: strings.Repeat("c", 64), HostQualificationSha256: strings.Repeat("d", 64)}
	api := model.CompetitionSourceReadiness{Environment: intent.Environment, CompetitionId: intent.CompetitionId, SeasonId: intent.SeasonId, Generation: intent.Generation, TargetRoundId: intent.TargetRoundId, Role: "api", ActorId: "api-runtime", ManifestCommit: intent.ManifestCommit, ManifestSha256: intent.ManifestSha256, SourceLockSha256: intent.SourceLockSha256, QualificationSha256: intent.HostQualificationSha256, RuntimeImageDigest: "sha256:" + strings.Repeat("e", 64), Ready: true}
	worker := api
	worker.Role, worker.ActorId = "worker", "worker-runtime"
	if err := sourcePreparationReceiptsMatch(intent, []model.CompetitionSourceReadiness{api, worker}); err != nil {
		t.Fatal(err)
	}
	for _, receipts := range [][]model.CompetitionSourceReadiness{nil, {api}, {api, api}, {worker, worker}} {
		if sourcePreparationReceiptsMatch(intent, receipts) == nil {
			t.Fatalf("incomplete/duplicate receipts accepted: %+v", receipts)
		}
	}
	for _, mutate := range []func(*model.CompetitionSourceReadiness){
		func(value *model.CompetitionSourceReadiness) { value.TargetRoundId = server.NewId() },
		func(value *model.CompetitionSourceReadiness) { value.Generation++ },
		func(value *model.CompetitionSourceReadiness) { value.ManifestSha256 = strings.Repeat("f", 64) },
		func(value *model.CompetitionSourceReadiness) { value.SourceLockSha256 = strings.Repeat("f", 64) },
		func(value *model.CompetitionSourceReadiness) { value.Ready = false },
	} {
		changed := worker
		mutate(&changed)
		if sourcePreparationReceiptsMatch(intent, []model.CompetitionSourceReadiness{api, changed}) == nil {
			t.Fatal("different runtime acknowledgment accepted")
		}
	}
}

// Main control-plane configuration remains static while only authenticated
// source fields move. A prior-round host marker is never enough for readiness.
func TestSourcePreparationDerivesOnlySourcePolicyAndRequiresReservedRoundHost(t *testing.T) {
	settings := &Settings{CompetitionId: "competition", BaseSha: strings.Repeat("0", 40), EvaluatorCommand: "/trusted/frozen-evaluator", SelfCheckCommand: "/trusted/host-check", AutomationPolicy: model.CompetitionAutomationPolicy{Enabled: true, Environment: "synthetic", SeasonId: "season", PolicyApprovalSha256: strings.Repeat("f", 64)}}
	settings.EvaluationPolicy.HardwareId = "synthetic-hardware"
	commits := map[string]string{}
	for _, name := range []string{"server", "connect", "sdk", "proxy", "glog", "goidenticons", "userwireguard", "sn", "operator-proxy", "warp", "gvisor"} {
		commits[name] = strings.Repeat("a", 40)
	}
	lock, err := model.CompetitionSourceLockDigest(commits)
	if err != nil {
		t.Fatal(err)
	}
	intent := &model.CompetitionSourceActivation{Environment: "synthetic", CompetitionId: "competition", SeasonId: "season", Generation: 3, SourceGeneration: 1, TargetRoundId: server.NewId(), Commits: commits, SourceLockSha256: lock, EvaluatorImageDigest: "sha256:" + strings.Repeat("b", 64), HostQualificationSha256: strings.Repeat("c", 64), Threshold: 0.2}
	derived, err := sourceSettingsFromIntent(settings, intent)
	if err != nil {
		t.Fatal(err)
	}
	if derived.BaseSha != commits["server"] || derived.AutomationPolicy.SourceActivationGeneration != 3 || derived.EvaluationPolicy.TakeoverMargin != 0.2 || derived.EvaluatorCommand != settings.EvaluatorCommand || derived.SelfCheckCommand != settings.SelfCheckCommand || derived.AutomationPolicy.PolicyApprovalSha256 != settings.AutomationPolicy.PolicyApprovalSha256 || settings.BaseSha != strings.Repeat("0", 40) {
		t.Fatal("source projection changed control policy or mutated static configuration")
	}
	host := passingHostCheck(derived)
	if sourceWorkerHostReady(host, derived, intent) {
		t.Fatal("different-round host marker accepted")
	}
	host.RebaselineRoundId = &intent.TargetRoundId
	if !sourceWorkerHostReady(host, derived, intent) {
		t.Fatal("genuine exact reserved-round host rejected")
	}
	host.NoProductionSecrets = false
	if sourceWorkerHostReady(host, derived, intent) {
		t.Fatal("round marker bypassed containment")
	}
	intent.SeasonId = "different-season"
	if _, err := sourceSettingsFromIntent(settings, intent); err == nil {
		t.Fatal("different-season source settings accepted")
	}
}
