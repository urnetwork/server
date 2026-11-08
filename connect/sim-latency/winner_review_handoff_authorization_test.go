// A prospective run permission binds the exact already-approved bounded review.
// It is never a winner verdict, source authorization or reward-release request.
package main

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Root prepares/reviews this only after actual release and resource evidence
// exists. The prior user-choice record is referenced, never relabeled as runtime
// qualification. AuthorizationSha256 is empty inside Args to avoid a hash cycle.
type winnerHandoffRunAuthorization struct {
	Schema                    int                                `json:"schema"`
	Kind                      string                             `json:"kind"`
	UserApprovalSha256        string                             `json:"user_approval_sha256"`
	AuthorizedAt              time.Time                          `json:"authorized_at"`
	ActorId                   string                             `json:"actor_id"`
	Args                      model.CompetitionReviewHandoffArgs `json:"args"`
	ReleaseSha256             string                             `json:"release_sha256"`
	QualificationReportSha256 string                             `json:"qualification_report_sha256"`
	ScanGroupsSha256          string                             `json:"scan_groups_sha256"`
	CaptureSettingsSha256     string                             `json:"capture_settings_sha256"`
	StoragePathsSha256        string                             `json:"storage_paths_sha256"`
	SingleAttempt             bool                               `json:"single_attempt"`
	WinnerApproval            bool                               `json:"winner_approval"`
	SourcePromotion           bool                               `json:"source_promotion"`
	RewardRelease             bool                               `json:"reward_release"`
}

// This second record can exist only after the DB stamps the immutable owner.
// Root approves the fixed plan and automatic final-token admission mechanism;
// actual token receipts are then retained before any generation claim.
type winnerHandoffCaptureAdmission struct {
	Schema                        int                         `json:"schema"`
	Kind                          string                      `json:"kind"`
	HandoffId                     server.Id                   `json:"handoff_id"`
	OwnerSha256                   string                      `json:"owner_sha256"`
	PlanSha256                    string                      `json:"plan_sha256"`
	BundleSha256                  string                      `json:"bundle_sha256"`
	Provenance                    winnerHandoffProvenance     `json:"provenance"` // CaptureAdmissionSha256 stays empty inside this self-hashed record.
	CapacitySha256                string                      `json:"capacity_sha256"`
	ResourceProofs                []currentQgQualificationPin `json:"resource_proofs"`
	AuthorizedAt                  time.Time                   `json:"authorized_at"`
	CompleteLeafPreflightRequired bool                        `json:"complete_leaf_preflight_required"`
	NoRepackOrRenewal             bool                        `json:"no_repack_or_renewal"`
	WinnerApproval                bool                        `json:"winner_approval"`
	SourcePromotion               bool                        `json:"source_promotion"`
	RewardRelease                 bool                        `json:"reward_release"`
}

// No model receipts are invented at this point. The native allocator/tokenizer
// independently enforces this prospective resource/admission decision at capture.
func validateWinnerHandoffCaptureAdmission(content []byte, owner *model.CompetitionReviewHandoff, plan *winnerMultipassPlan, bundle winnerMultipassBundle, provenance winnerHandoffProvenance, settings winnerHandoffCaptureSettings, now time.Time) (*winnerHandoffCaptureAdmission, error) {
	if owner == nil || plan == nil || rejectWinnerReviewCaseAliases(content) != nil {
		return nil, errors.New("frozen capture admission is absent or malformed")
	}
	var admission winnerHandoffCaptureAdmission
	if decodeStrictJSONBytes(content, &admission, "frozen capture admission") != nil {
		return nil, errors.New("frozen capture admission cannot be decoded")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(content, &fields) != nil {
		return nil, errors.New("frozen capture admission is malformed")
	}
	for _, name := range []string{"complete_leaf_preflight_required", "no_repack_or_renewal", "winner_approval", "source_promotion", "reward_release"} {
		if value, found := fields[name]; !found || string(value) == "null" {
			return nil, errors.New("frozen capture admission omitted an explicit authority boundary")
		}
	}
	capacity, err := estimateWinnerMultipassCapacity(plan, bundle, winnerHandoffTokenizerCensus(plan), settings.Capacity)
	if err != nil {
		return nil, err
	}
	capacityBytes, _ := json.Marshal(capacity)
	if settings.Spool != nil {
		spool, _, err := estimateWinnerMultipassSpoolCapacity(plan, bundle, winnerHandoffTokenizerCensus(plan), settings.Capacity, *settings.Spool)
		if err != nil {
			return nil, err
		}
		capacityBytes, _ = json.Marshal(struct {
			Aggregate *winnerMultipassCapacity      `json:"aggregate"`
			Spool     *winnerMultipassSpoolCapacity `json:"spool"`
		}{Aggregate: capacity, Spool: spool})
	}
	ownerBytes, _ := json.Marshal(owner)
	if admission.Schema != 1 || admission.Kind != "elected-winner-frozen-capture-admission/v1" || admission.HandoffId != owner.HandoffId || admission.OwnerSha256 != winnerMultipassSha256(ownerBytes) ||
		admission.PlanSha256 != owner.Plan.PlanSha256 || admission.BundleSha256 != plan.BundleSha256 || admission.Provenance != provenance || admission.CapacitySha256 != winnerMultipassSha256(capacityBytes) || len(admission.ResourceProofs) == 0 || len(admission.ResourceProofs) > 16 ||
		admission.AuthorizedAt.Before(owner.StartedAt) || !admission.AuthorizedAt.Before(owner.Deadline) || admission.AuthorizedAt.After(now) || !admission.CompleteLeafPreflightRequired || !admission.NoRepackOrRenewal || admission.WinnerApproval || admission.SourcePromotion || admission.RewardRelease {
		return nil, errors.New("capture admission changed exact owner/plan/resources or conferred forbidden winner authority")
	}
	for _, pin := range admission.ResourceProofs {
		if !validSha256(pin.Sha256) {
			return nil, errors.New("capture admission lacks an original resource evidence identity")
		}
	}
	return &admission, nil
}

// Pure comparison makes the same binding usable before and after DB ownership.
func validateWinnerHandoffRunAuthorization(content []byte, pin currentQgQualificationPin, actor string, args model.CompetitionReviewHandoffArgs, release winnerMultipassQualificationRelease, qualification currentQgQualificationPin, groups []winnerMultipassScanGroup, settings winnerHandoffCaptureSettings, storagePaths []string, now time.Time) (winnerHandoffProvenance, error) {
	provenance := winnerHandoffProvenance{RunAuthorizationSha256: pin.Sha256, QualificationReportSha256: qualification.Sha256}
	releaseBytes, _ := json.Marshal(release)
	provenance.ReleaseSha256 = winnerMultipassSha256(releaseBytes)
	var authorization winnerHandoffRunAuthorization
	if winnerMultipassSha256(content) != pin.Sha256 || rejectWinnerReviewCaseAliases(content) != nil || decodeStrictJSONBytes(content, &authorization, "prospective one-shot review run authority") != nil {
		return provenance, errors.New("prospective run authority is not the exact independently reviewed record")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(content, &fields) != nil {
		return provenance, errors.New("prospective run authority is malformed")
	}
	for _, name := range []string{"single_attempt", "winner_approval", "source_promotion", "reward_release"} {
		if value, found := fields[name]; !found || string(value) == "null" {
			return provenance, errors.New("prospective run authority omitted an explicit authority boundary")
		}
	}
	groupsBytes, _ := json.Marshal(groups)
	settingsBytes, _ := json.Marshal(settings)
	pathsBytes, _ := json.Marshal(storagePaths)
	if args.AuthorizationSha256 != pin.Sha256 {
		return provenance, errors.New("DB owner is not bound to this exact prospective run authority")
	}
	args.AuthorizationSha256 = ""
	if authorization.Schema != 1 || authorization.Kind != "elected-winner-multipass-run-authorization/v1" || !validSha256(authorization.UserApprovalSha256) || authorization.AuthorizedAt.IsZero() || authorization.AuthorizedAt.After(now) ||
		authorization.ActorId != actor || authorization.Args != args || authorization.ReleaseSha256 != provenance.ReleaseSha256 || !validSha256(qualification.Sha256) || authorization.QualificationReportSha256 != qualification.Sha256 ||
		authorization.ScanGroupsSha256 != winnerMultipassSha256(groupsBytes) || authorization.CaptureSettingsSha256 != winnerMultipassSha256(settingsBytes) || authorization.StoragePathsSha256 != winnerMultipassSha256(pathsBytes) ||
		!authorization.SingleAttempt || authorization.WinnerApproval || authorization.SourcePromotion || authorization.RewardRelease {
		return provenance, errors.New("prospective run authority changed candidate, actor, qualified release, bounded layout/resources or winner-approval boundary")
	}
	return provenance, nil
}

// Even a newly hashed root record cannot change the exact one-shot candidate,
// selected layout/resources or explicit absence of winner/source/reward power.
func TestWinnerHandoffProspectiveAuthorityBindsExactScopeWithoutWinnerApproval(t *testing.T) {
	fixture := newWinnerHandoffFixture(t, false)
	release := winnerMultipassQualificationFixture(t).Release
	qualification := currentQgQualificationPin{Path: "/synthetic/qualification", Sha256: winnerMultipassSha256([]byte("synthetic-report"))}
	settings, paths := defaultWinnerHandoffCaptureSettings(), []string{"/synthetic/review"}
	args := fixture.owner.Args
	args.AuthorizationSha256 = ""
	releaseBytes, _ := json.Marshal(release)
	groupsBytes, _ := json.Marshal(fixture.plan.ScanGroups)
	settingsBytes, _ := json.Marshal(settings)
	pathsBytes, _ := json.Marshal(paths)
	authority := winnerHandoffRunAuthorization{Schema: 1, Kind: "elected-winner-multipass-run-authorization/v1", UserApprovalSha256: winnerMultipassSha256([]byte("synthetic-user-choice")), AuthorizedAt: fixture.now,
		ActorId: fixture.owner.ActorId, Args: args, ReleaseSha256: winnerMultipassSha256(releaseBytes), QualificationReportSha256: qualification.Sha256, ScanGroupsSha256: winnerMultipassSha256(groupsBytes),
		CaptureSettingsSha256: winnerMultipassSha256(settingsBytes), StoragePathsSha256: winnerMultipassSha256(pathsBytes), SingleAttempt: true}
	check := func(value any, want bool) {
		t.Helper()
		content, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		pin := currentQgQualificationPin{Path: "/synthetic/run-authority", Sha256: winnerMultipassSha256(content)}
		ownerArgs := args
		ownerArgs.AuthorizationSha256 = pin.Sha256
		_, err = validateWinnerHandoffRunAuthorization(content, pin, fixture.owner.ActorId, ownerArgs, release, qualification, fixture.plan.ScanGroups, settings, paths, fixture.now)
		if (err == nil) != want {
			t.Fatal("prospective authority boundary differs", want, err)
		}
	}
	check(authority, true)
	for _, mutate := range []func(*winnerHandoffRunAuthorization){
		func(value *winnerHandoffRunAuthorization) { value.WinnerApproval = true },
		func(value *winnerHandoffRunAuthorization) { value.SourcePromotion = true },
		func(value *winnerHandoffRunAuthorization) { value.RewardRelease = true },
		func(value *winnerHandoffRunAuthorization) { value.SingleAttempt = false },
		func(value *winnerHandoffRunAuthorization) { value.Args.TimeoutSeconds++ },
		func(value *winnerHandoffRunAuthorization) {
			value.ReleaseSha256 = winnerMultipassSha256([]byte("other synthetic release"))
		},
		func(value *winnerHandoffRunAuthorization) {
			value.StoragePathsSha256 = winnerMultipassSha256([]byte("other synthetic storage"))
		},
	} {
		changed := authority
		mutate(&changed)
		check(changed, false)
	}
	content, _ := json.Marshal(authority)
	for _, name := range []string{"single_attempt", "winner_approval", "source_promotion", "reward_release"} {
		var changed map[string]any
		if err := json.Unmarshal(content, &changed); err != nil {
			t.Fatal(err)
		}
		delete(changed, name)
		check(changed, false)
		changed[name] = nil
		check(changed, false)
	}
}

// The second permission cannot predate freeze, select another clock/plan or
// silently turn automatic token admission into a winner decision.
func TestWinnerHandoffCaptureAdmissionBindsFrozenClockAndResourceCensus(t *testing.T) {
	fixture := newWinnerHandoffFixture(t, false)
	settings := defaultWinnerHandoffCaptureSettings()
	capacity, err := estimateWinnerMultipassCapacity(fixture.plan, fixture.bundle, winnerHandoffTokenizerCensus(fixture.plan), settings.Capacity)
	if err != nil {
		t.Fatal(err)
	}
	ownerBytes, _ := json.Marshal(fixture.owner)
	capacityBytes, _ := json.Marshal(capacity)
	provenance := fixture.hooks(t).Provenance
	provenance.CaptureAdmissionSha256 = ""
	admission := winnerHandoffCaptureAdmission{Schema: 1, Kind: "elected-winner-frozen-capture-admission/v1", HandoffId: fixture.owner.HandoffId, OwnerSha256: winnerMultipassSha256(ownerBytes), PlanSha256: fixture.owner.Plan.PlanSha256,
		BundleSha256: fixture.plan.BundleSha256, Provenance: provenance, CapacitySha256: winnerMultipassSha256(capacityBytes), ResourceProofs: []currentQgQualificationPin{{Path: "/synthetic/resource-proof", Sha256: winnerMultipassSha256([]byte("synthetic-capacity-observation"))}},
		AuthorizedAt: fixture.owner.StartedAt, CompleteLeafPreflightRequired: true, NoRepackOrRenewal: true}
	check := func(value any, want bool) {
		t.Helper()
		content, _ := json.Marshal(value)
		_, err := validateWinnerHandoffCaptureAdmission(content, &fixture.owner, fixture.plan, fixture.bundle, provenance, settings, fixture.now)
		if (err == nil) != want {
			t.Fatal("frozen admission boundary differs", want, err)
		}
	}
	check(admission, true)
	for _, mutate := range []func(*winnerHandoffCaptureAdmission){
		func(value *winnerHandoffCaptureAdmission) {
			value.AuthorizedAt = fixture.owner.StartedAt.Add(-time.Second)
		},
		func(value *winnerHandoffCaptureAdmission) {
			value.OwnerSha256 = winnerMultipassSha256([]byte("other-owner"))
		},
		func(value *winnerHandoffCaptureAdmission) {
			value.CapacitySha256 = winnerMultipassSha256([]byte("smaller-budget"))
		},
		func(value *winnerHandoffCaptureAdmission) { value.WinnerApproval = true },
		func(value *winnerHandoffCaptureAdmission) { value.CompleteLeafPreflightRequired = false },
		func(value *winnerHandoffCaptureAdmission) { value.NoRepackOrRenewal = false },
	} {
		changed := admission
		mutate(&changed)
		check(changed, false)
	}
	content, _ := json.Marshal(admission)
	for _, name := range []string{"winner_approval", "source_promotion", "reward_release", "complete_leaf_preflight_required", "no_repack_or_renewal"} {
		var fields map[string]any
		if err := json.Unmarshal(content, &fields); err != nil {
			t.Fatal(err)
		}
		delete(fields, name)
		check(fields, false)
		fields[name] = nil
		check(fields, false)
	}
}
