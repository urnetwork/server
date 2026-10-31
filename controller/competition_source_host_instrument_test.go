// Synthetic instrument handoffs reproduce the broker/host identity mismatch.
// No test installs root configuration, contacts services or runs an evaluator.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Existing host and typed-approval fixtures supply independent synthetic
// boundaries; only their image/receipt links are composed here.
func newCompetitionInstrumentHostFixture(t *testing.T, promoted bool) competitionReviewedHostFixture {
	t.Helper()
	f := newCompetitionReviewedHostFixture(t)
	settings, _, intent := trustedInstrumentAuthorizationFixture(t)
	approval := settings.FutureEpochPolicy.TrustedInstrument
	approval.InstrumentBaseImageDigest = f.Profile.BaseImageId
	approval.ApprovalSha256 = ""
	var err error
	approval.ApprovalSha256, err = model.CompetitionAutomationDigest(approval)
	if err != nil {
		t.Fatal(err)
	}
	proof := intent.TrustedInstrument
	proof.Approval = *approval
	f.Settings.FutureEpochPolicy = settings.FutureEpochPolicy
	f.Settings.EvaluationPolicy.G2VolumePolicy = model.CompetitionG2VolumePolicyMinimumWork
	intent.BaseSourceLockSha256 = approval.InstrumentBaseSourceLockSha256
	intent.EvaluatorImageDigest, intent.HostQualificationSha256 = f.Settings.EvaluatorImageDigest, f.Settings.EvaluationPolicy.HostQualificationSha256
	if promoted {
		reviewId := server.RequireIdFromBytes(bytes.Repeat([]byte{1}, 16))
		intent.Kind, intent.ReviewId = "reviewed_source_promotion", &reviewId
		intent.ReviewDecisionDigest, intent.ApprovedPatchSha256 = strings.Repeat("4", 64), strings.Repeat("5", 64)
		intent.EvaluatedImageDigest = approval.InstrumentBaseImageDigest
		proof.SelectedProductKind = intent.Kind
		proof.SelectedProductCommits = maps.Clone(approval.InstrumentBaseCommits)
		proof.SelectedProductCommits["server"] = strings.Repeat("7", 40)
		proof.SelectedProductSourceLockSha256, err = model.CompetitionSourceLockDigest(proof.SelectedProductCommits)
		if err != nil {
			t.Fatal(err)
		}
		proof.SelectedProductReviewId, proof.SelectedProductReviewDigest, proof.SelectedProductPatchSha256 = &reviewId, intent.ReviewDecisionDigest, intent.ApprovedPatchSha256
	}
	receipt := f.Receipt
	receipt.Schema, receipt.Operation, receipt.ReviewId = 2, "trusted_instrument_build", server.Id{}
	receipt.InstrumentApprovalSha256, receipt.PatchSha256 = approval.ApprovalSha256, approval.PatchSha256
	receipt.SourceLockSha256, receipt.SimulatorSha256, receipt.G2VolumePolicy = intent.SourceLockSha256, intent.SimulatorSha256, intent.G2VolumePolicy
	proof.BuildReceipt = &receipt
	f.Intent, f.Receipt = intent, receipt
	f.Profile.SourceLockSha256 = intent.SourceLockSha256
	f.Profile.ReviewId = "instrument-" + approval.ApprovalSha256 + "-" + intent.SourceLockSha256
	return f
}

// The real carry has no product review or evaluated-image claim. Both carry
// and promoted-product overlays must install only image/fingerprint fields.
func TestCompetitionInstrumentHostInstallBindsCarryAndPromotedProduct(t *testing.T) {
	for _, promoted := range []bool{false, true} {
		f := newCompetitionInstrumentHostFixture(t, promoted)
		before := bytes.Clone(f.Active)
		content, err := competitionReviewedHostInstallContent(f.Settings, f.Intent, f.Active, f.Prospective, f.Receipt, f.Profile, f.Profile.BootId)
		if err != nil {
			t.Fatalf("instrument handoff refused promoted=%t: %v", promoted, err)
		}
		var original, installed map[string]any
		if json.Unmarshal(f.Active, &original) != nil || json.Unmarshal(content, &installed) != nil || !bytes.Equal(before, f.Active) ||
			installed["image_digest"] != f.Intent.EvaluatorImageDigest || installed["qualification_sha256"] != f.Intent.HostQualificationSha256 {
			t.Fatal("instrument host installation lost its exact immutable image/fingerprint boundary")
		}
		for _, key := range []string{"image_digest", "qualification_sha256"} {
			delete(original, key)
			delete(installed, key)
		}
		if !reflect.DeepEqual(original, installed) || f.Profile.RuntimeQualified || f.Profile.Prospective.RebaselinePassed || f.Profile.Prospective.CleanupVerified ||
			f.Receipt.ReviewId != (server.Id{}) || (!promoted && (f.Intent.ReviewId != nil || f.Intent.EvaluatedImageDigest != "")) {
			t.Fatal("instrument installation changed unrelated policy or manufactured review/runtime evidence")
		}
	}
}

// This is the broker's exact .attempts key, not the zero review id, selected
// product lock or genuine promoted product's independent review reservation.
func TestCompetitionInstrumentHostBuildAttemptUsesBrokerOperationIdentity(t *testing.T) {
	for _, promoted := range []bool{false, true} {
		f := newCompetitionInstrumentHostFixture(t, promoted)
		build, err := competitionHostInstallBuildIdentity(f.Settings, f.Intent)
		identity := "instrument-" + f.Intent.TrustedInstrument.Approval.ApprovalSha256 + "-" + f.Intent.SourceLockSha256
		key := sha256.Sum256([]byte(identity))
		if err != nil || build.AuthorizationId != identity || build.BaseImageDigest != f.Profile.BaseImageId || build.AttemptKey != hex.EncodeToString(key[:]) {
			t.Fatal("host lookup does not reproduce the instrument broker journal/profile identity", err)
		}
		root := t.TempDir()
		actual := filepath.Join(root, ".attempts", build.AttemptKey)
		for _, alias := range []string{(server.Id{}).String(), f.Intent.TrustedInstrument.SelectedProductSourceLockSha256, f.Intent.TrustedInstrument.Approval.ApprovalSha256} {
			wrong := sha256.Sum256([]byte(alias))
			if actual == filepath.Join(root, ".attempts", hex.EncodeToString(wrong[:])) {
				t.Fatal("instrument build reused an unrelated attempt alias")
			}
		}
		if promoted {
			wrong := sha256.Sum256([]byte(f.Intent.ReviewId.String()))
			if build.AttemptKey == hex.EncodeToString(wrong[:]) {
				t.Fatal("instrument overlay borrowed the selected winner's reviewed build attempt")
			}
		}
	}
}

// A later ordinary reviewed minimum-work image still uses schema one and its
// real review id; the presence of a new scorer does not imply an instrument op.
func TestCompetitionReviewedMinimumWorkHostInstallKeepsReviewIdentity(t *testing.T) {
	f := newCompetitionReviewedHostFixture(t)
	f.Intent.G2VolumePolicy, f.Receipt.G2VolumePolicy = model.CompetitionG2VolumePolicyMinimumWork, model.CompetitionG2VolumePolicyMinimumWork
	f.Intent.SimulatorSha256, f.Intent.ScorerSha256, f.Receipt.SimulatorSha256 = strings.Repeat("7", 64), strings.Repeat("7", 64), strings.Repeat("7", 64)
	build, err := competitionHostInstallBuildIdentity(f.Settings, f.Intent)
	key := sha256.Sum256([]byte(f.Intent.ReviewId.String()))
	if err != nil || build.AuthorizationId != f.Intent.ReviewId.String() || build.AttemptKey != hex.EncodeToString(key[:]) {
		t.Fatal("ordinary reviewed source lost its review-indexed attempt", err)
	}
	if _, err := competitionReviewedHostInstallContent(f.Settings, f.Intent, f.Active, f.Prospective, f.Receipt, f.Profile, f.Profile.BootId); err != nil {
		t.Fatal("ordinary reviewed minimum-work receipt was relabeled as an instrument operation", err)
	}
}

// Plausible hashes or receipt booleans cannot replace configured approval,
// exact graphs/child/binary, original host identity or an immutable proof link.
func TestCompetitionInstrumentHostInstallRejectsSubstitutedTypedAuthority(t *testing.T) {
	for index, mutate := range []func(*competitionReviewedHostFixture){
		func(f *competitionReviewedHostFixture) { f.Settings.FutureEpochPolicy = nil },
		func(f *competitionReviewedHostFixture) {
			f.Settings.FutureEpochPolicy.TrustedInstrument.ApprovalSha256 = strings.Repeat("9", 64)
		},
		func(f *competitionReviewedHostFixture) {
			f.Intent.TrustedInstrument.Approval.ProtectedPaths = []string{"connect/synthetic.go"}
		},
		func(f *competitionReviewedHostFixture) { f.Intent.TargetEpoch = 1 },
		func(f *competitionReviewedHostFixture) { f.Intent.Staging = true },
		func(f *competitionReviewedHostFixture) { f.Intent.TrustedInstrument.Schema = 2 },
		func(f *competitionReviewedHostFixture) {
			f.Intent.G2VolumePolicy = model.CompetitionG2VolumePolicyLegacy
		},
		func(f *competitionReviewedHostFixture) { f.Intent.BaseSourceLockSha256 = strings.Repeat("9", 64) },
		func(f *competitionReviewedHostFixture) { f.Intent.SourceLockSha256 = strings.Repeat("9", 64) },
		func(f *competitionReviewedHostFixture) { f.Intent.Commits["foreign"] = strings.Repeat("9", 40) },
		func(f *competitionReviewedHostFixture) {
			f.Intent.TrustedInstrument.SelectedProductSourceLockSha256 = strings.Repeat("9", 64)
		},
		func(f *competitionReviewedHostFixture) {
			f.Intent.TrustedInstrument.InstrumentedServerCommit = strings.Repeat("9", 40)
		},
		func(f *competitionReviewedHostFixture) { f.Intent.ScorerSha256 = strings.Repeat("9", 64) },
		func(f *competitionReviewedHostFixture) {
			f.Intent.EvaluatedImageDigest = "sha256:" + strings.Repeat("9", 64)
		},
		func(f *competitionReviewedHostFixture) { f.Profile.BaseImageId = "sha256:" + strings.Repeat("9", 64) },
		func(f *competitionReviewedHostFixture) { f.Profile.ReviewId = (server.Id{}).String() },
		func(f *competitionReviewedHostFixture) {
			f.Profile.ReviewId = "instrument-" + f.Intent.TrustedInstrument.Approval.ApprovalSha256 + "-" + f.Intent.TrustedInstrument.SelectedProductSourceLockSha256
		},
		func(f *competitionReviewedHostFixture) { f.Profile.SourceLockSha256 = strings.Repeat("9", 64) },
		func(f *competitionReviewedHostFixture) { f.Receipt.Schema, f.Receipt.Operation = 1, "" },
		func(f *competitionReviewedHostFixture) { f.Receipt.TestEvidenceSha256 = strings.Repeat("9", 64) },
		func(f *competitionReviewedHostFixture) { f.Receipt.SandboxId = strings.Repeat("9", 64) },
		func(f *competitionReviewedHostFixture) { f.Intent.TrustedInstrument.BuildReceipt.Schema = 1 },
		func(f *competitionReviewedHostFixture) {
			f.Intent.TrustedInstrument.BuildReceipt.SimulatorSha256 = strings.Repeat("9", 64)
		},
		func(f *competitionReviewedHostFixture) {
			f.Intent.TrustedInstrument.BuildReceipt.EvaluatorImageDigest = f.Profile.BaseImageId
		},
		func(f *competitionReviewedHostFixture) { f.Intent.TrustedInstrument = nil },
	} {
		f := newCompetitionInstrumentHostFixture(t, false)
		mutate(&f)
		if content, err := competitionReviewedHostInstallContent(f.Settings, f.Intent, f.Active, f.Prospective, f.Receipt, f.Profile, f.Profile.BootId); err == nil || content != nil {
			t.Fatalf("substituted instrument authority %d produced installable host bytes", index)
		}
	}
}

// Even a fully rehashed configured approval and matching profile cannot turn
// a different image into the actual original root-provisioned host image.
func TestCompetitionInstrumentHostInstallRequiresActualOriginalImage(t *testing.T) {
	f := newCompetitionInstrumentHostFixture(t, false)
	approval := f.Settings.FutureEpochPolicy.TrustedInstrument
	approval.InstrumentBaseImageDigest = "sha256:" + strings.Repeat("9", 64)
	approval.ApprovalSha256 = ""
	var err error
	approval.ApprovalSha256, err = model.CompetitionAutomationDigest(approval)
	if err != nil {
		t.Fatal(err)
	}
	proof := f.Intent.TrustedInstrument
	proof.Approval = *approval
	proof.BuildReceipt.InstrumentApprovalSha256 = approval.ApprovalSha256
	f.Receipt = *proof.BuildReceipt
	f.Profile.ReviewId = "instrument-" + approval.ApprovalSha256 + "-" + f.Intent.SourceLockSha256
	f.Profile.BaseImageId, f.Profile.Incumbent.ImageDigest = approval.InstrumentBaseImageDigest, approval.InstrumentBaseImageDigest
	if content, err := competitionReviewedHostInstallContent(f.Settings, f.Intent, f.Active, f.Prospective, f.Receipt, f.Profile, f.Profile.BootId); err == nil || content != nil {
		t.Fatal("rehashed approval/profile replaced the actual original host image boundary")
	}
}

// A product winner's review remains mandatory, but never becomes authority
// for a schema-two instrument build or a different prospective host profile.
func TestCompetitionInstrumentHostInstallRejectsCrossKindAndReviewAliases(t *testing.T) {
	for index, mutate := range []func(*competitionReviewedHostFixture){
		func(f *competitionReviewedHostFixture) { f.Intent.ReviewId = nil },
		func(f *competitionReviewedHostFixture) {
			id := server.Id{}
			f.Intent.ReviewId = &id
		},
		func(f *competitionReviewedHostFixture) { f.Intent.Kind = "trusted_instrument_transition" },
		func(f *competitionReviewedHostFixture) {
			f.Intent.TrustedInstrument.SelectedProductReviewDigest = strings.Repeat("9", 64)
		},
		func(f *competitionReviewedHostFixture) { f.Intent.EvaluatedImageDigest = "" },
		func(f *competitionReviewedHostFixture) { f.Profile.ReviewId = f.Intent.ReviewId.String() },
		func(f *competitionReviewedHostFixture) { f.Receipt.ReviewId = *f.Intent.ReviewId },
		func(f *competitionReviewedHostFixture) {
			f.Intent.TrustedInstrument = nil
			f.Profile.ReviewId = f.Intent.ReviewId.String()
		},
	} {
		f := newCompetitionInstrumentHostFixture(t, true)
		mutate(&f)
		if content, err := competitionReviewedHostInstallContent(f.Settings, f.Intent, f.Active, f.Prospective, f.Receipt, f.Profile, f.Profile.BootId); err == nil || content != nil {
			t.Fatalf("cross-kind or product-review alias %d authorized instrument host installation", index)
		}
	}
}

// The shared pure proof check does not replace any predecessor, threshold or
// generation check at the existing database authorization boundary.
func TestCompetitionInstrumentHostProofRetainsPredecessorAuthorization(t *testing.T) {
	for index, mutate := range []func(*model.CompetitionSourceActivation, *model.CompetitionSourceActivation){
		func(current, intent *model.CompetitionSourceActivation) { current.TargetEpoch = 2 },
		func(current, intent *model.CompetitionSourceActivation) {
			current.G2VolumePolicy = model.CompetitionG2VolumePolicyMinimumWork
		},
		func(current, intent *model.CompetitionSourceActivation) { current.SourceGeneration++ },
		func(current, intent *model.CompetitionSourceActivation) {
			current.SourceLockSha256 = strings.Repeat("9", 64)
		},
		func(current, intent *model.CompetitionSourceActivation) {
			current.EvaluatorImageDigest = "sha256:" + strings.Repeat("9", 64)
		},
		func(current, intent *model.CompetitionSourceActivation) { intent.Threshold += 0.01 },
		func(current, intent *model.CompetitionSourceActivation) {
			intent.ThresholdEvidenceSha256 = strings.Repeat("9", 64)
		},
	} {
		settings, current, intent := trustedInstrumentAuthorizationFixture(t)
		mutate(current, intent)
		if err := validateTrustedInstrumentSource(settings, intent, current); err == nil {
			t.Fatalf("instrument proof bypassed predecessor/threshold boundary %d", index)
		}
	}
}

// The new admission reaches the unchanged fenced probe/compare path; failed
// authority, live-host evidence, cancellation or changed root bytes refuse it.
func TestCompetitionInstrumentHostInstallPreservesFencedCommitOrdering(t *testing.T) {
	for _, failure := range []string{"", "fence", "probe", "changed_file", "canceled"} {
		f := newCompetitionInstrumentHostFixture(t, false)
		content, err := competitionReviewedHostInstallContent(f.Settings, f.Intent, f.Active, f.Prospective, f.Receipt, f.Profile, f.Profile.BootId)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		events := []string{}
		replaced := false
		fence := func(context.Context) error {
			events = append(events, "fence")
			if failure == "fence" {
				return ErrLeaseLost
			}
			return nil
		}
		probe := func(context.Context, []byte) (HostSelfCheck, error) {
			events = append(events, "probe")
			host := f.Profile.Prospective
			host.Checks = maps.Clone(host.Checks)
			host.Checks["qualification_match"] = true
			if failure == "probe" {
				return HostSelfCheck{}, errors.New("synthetic failed host probe")
			}
			if failure == "canceled" {
				cancel()
			}
			return host, nil
		}
		current := func() ([]byte, error) {
			events = append(events, "current")
			if failure == "changed_file" {
				return []byte("changed synthetic root bytes"), nil
			}
			return f.Active, nil
		}
		replace := func(actual []byte) error {
			events = append(events, "replace")
			replaced = true
			if !bytes.Equal(actual, content) {
				return errors.New("changed synthetic install content")
			}
			return nil
		}
		err = executeCompetitionReviewedHostInstall(ctx, f.Active, content, f.Settings, fence, probe, current, replace)
		cancel()
		if (err != nil) != (failure != "") || replaced != (failure == "") {
			t.Fatal("instrument handoff crossed a failed fenced commit boundary", failure, events, err)
		}
		if failure == "" && !reflect.DeepEqual(events, []string{"fence", "probe", "fence", "current", "replace"}) {
			t.Fatal("instrument host installation reordered the existing fenced commit", events)
		}
	}
}
