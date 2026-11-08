// A prospective run permission binds the exact already-approved bounded review.
// It is never a winner verdict, source authorization or reward-release request.
package main

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

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
