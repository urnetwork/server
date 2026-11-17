// The same protected per-invocation lifecycle serves candidate capture and the
// fixed qualification corpus. Archive credentials never enter the model child.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"golang.org/x/sys/unix"
)

// Adapt a separately authenticated exact-version archive reader, never a local
// path or mutable latest object, to the core's opaque prior-physical-proof gate.
func winnerHandoffSpoolPortableVerifier(ctx context.Context, release winnerMultipassQualificationRelease, profile winnerMultipassContainmentProfile, read winnerHandoffSpoolReader) winnerHandoffSpoolVerifier {
	return func(commit winnerHandoffSpoolCommit, originals map[string][]byte) error {
		releaseBytes, _ := json.Marshal(release)
		if read == nil || commit.Binding.ReleaseSha256 != winnerMultipassSha256(releaseBytes) || commit.Binding.NativeDriverSha256 != release.NativeDriver.Sha256 || commit.AttemptRoot != profile.StateRoot {
			return errors.New("protected spool belongs to another trusted driver/release")
		}
		value := commit.Physical
		ref := winnerMultipassProtectedReference{Key: value.Retention.Key, VersionId: value.Retention.VersionId, Sha256: value.Retention.Sha256, Bytes: value.Bytes, RetainUntil: value.Retention.RetainUntil}
		proof, err := loadWinnerMultipassPhysicalAuthentication(ctx, ref, release, commit.MinimumUntil, func(ctx context.Context, reference winnerMultipassProtectedReference) ([]byte, error) {
			return read(ctx, winnerHandoffCustodyOriginal{Name: "physical-authentication.json", Bytes: reference.Bytes, Retention: model.CompetitionReviewHandoffRetention{Key: reference.Key, VersionId: reference.VersionId, Sha256: reference.Sha256, RetainUntil: reference.RetainUntil}})
		})
		if err != nil {
			return err
		}
		leaves := map[string][]byte{}
		for _, original := range commit.Invocation.Originals {
			leaves[original.Name] = originals[original.Name]
		}
		receipt, err := replayWinnerMultipassPortableContainment(leaves, originals["independent-generation-claim.json"], proof, profile)
		if err != nil || receipt.AttemptDirectory != commit.Attempt || receipt.ManifestSha256 != commit.Invocation.ManifestSha256 || receipt.WrapperReceiptSha256 != commit.Invocation.WrapperReceiptSha256 {
			return errors.New("protected spool original containment did not replay exactly")
		}
		return nil
	}
}

// Called only after actual physical authentication under the installed driver.
// All raw originals, the independent claim, proof and full capture are retained
// and reread before a compact commit permits any local payload retirement.
func protectWinnerHandoffSpoolInvocation(ctx context.Context, directory, phase string, binding winnerHandoffSpoolBinding, envelope winnerMultipassBrokerEnvelope, receipt *winnerMultipassContainmentReceipt, physical *winnerMultipassPhysicalAuthentication, independentClaim, capture []byte, release winnerMultipassQualificationRelease,
	retain func(context.Context, []byte, string) (*controller.CompetitionWinnerReviewEvidence, error), read winnerHandoffSpoolReader) (winnerHandoffInvocationCustody, error) {
	limits := defaultWinnerHandoffSpoolLimits()
	value := winnerHandoffInvocationCustody{}
	if physical == nil || receipt == nil || physical.ManifestSha256 != receipt.ManifestSha256 || physical.WrapperReceiptSha256 != receipt.WrapperReceiptSha256 || physical.NativeDriver != release.NativeDriver ||
		len(capture) == 0 || int64(len(capture)) > limits.MaximumCaptureBytes || currentQgQualificationPrivateRoot(directory) != nil {
		return value, errors.New("spool lacks its actual physical proof, bounded full capture or private journal")
	}
	entries, err := snapshotWinnerHandoffSpool(physical.Profile.StateRoot, receipt, limits)
	if err != nil {
		return value, err
	}
	value, err = retainWinnerHandoffContainment(ctx, phase, envelope, receipt, retain)
	if err != nil {
		return value, err
	}
	content, err := json.Marshal(physical)
	if err != nil {
		return value, err
	}
	proof, err := retainWinnerHandoffDocument(ctx, "physical-authentication.json", content, int(limits.MaximumCommitBytes), envelope.Owner.Deadline, retain)
	if err != nil {
		return value, err
	}
	commit := winnerHandoffSpoolCommit{Schema: 1, Kind: "winner-review-protected-spool-commit/v1", Lifecycle: winnerMultipassSpoolLifecycle, Binding: binding, AttemptRoot: physical.Profile.StateRoot, Attempt: receipt.AttemptDirectory,
		Invocation: value, Physical: proof, Extra: []winnerHandoffCustodyOriginal{}, Entries: entries, MinimumUntil: envelope.Owner.Deadline, Limits: limits}
	if len(independentClaim) != 0 {
		claim, err := retainWinnerHandoffDocument(ctx, "independent-generation-claim.json", independentClaim, int(limits.MaximumCommitBytes), envelope.Owner.Deadline, retain)
		if err != nil {
			return value, err
		}
		commit.Extra = append(commit.Extra, claim)
	}
	original, err := retainWinnerHandoffDocument(ctx, "invocation-capture.json", capture, int(limits.MaximumCaptureBytes), envelope.Owner.Deadline, retain)
	if err != nil {
		return value, err
	}
	commit.Extra = append(commit.Extra, original)
	verify := winnerHandoffSpoolPortableVerifier(ctx, release, physical.Profile, read)
	protected, err := commitWinnerHandoffSpool(ctx, directory, commit, retain, read, verify)
	if err != nil {
		return value, err
	}
	// The small references survive a cleanup failure as well, so recovery can
	// finish exact custody without issuing another provider request.
	value.SpoolCommit = &protected.reference.Commit
	value.SpoolReferencePath = protected.referencePath
	referenceBytes, _ := json.Marshal(protected.reference)
	value.SpoolReferenceSha256 = winnerMultipassSha256(referenceBytes)
	if err := evictWinnerHandoffSpool(ctx, protected, read, verify, unix.Unlinkat); err != nil {
		return value, err
	}
	return value, nil
}

// The actual reader includes exact candidate namespace, protected version,
// retention, length, digest and independently retrievable original bytes.
func winnerHandoffProtectedReader(control *controller.Settings, owner *model.CompetitionReviewHandoff) winnerHandoffSpoolReader {
	return func(ctx context.Context, original winnerHandoffCustodyOriginal) ([]byte, error) {
		return controller.LoadCompetitionReviewHandoffEvidence(ctx, control, owner.Args.RoundId, owner.Args.JobId, original.Retention, original.Bytes)
	}
}

// Compact local journals are an additional durable index, not a substitute for
// the remotely protected original commit. Both identities must agree on replay.
func reloadWinnerHandoffSpoolInvocation(ctx context.Context, value winnerHandoffInvocationCustody, binding winnerHandoffSpoolBinding, release winnerMultipassQualificationRelease, profile winnerMultipassContainmentProfile, read winnerHandoffSpoolReader) error {
	if value.SpoolCommit == nil || !validSha256(value.SpoolReferenceSha256) || filepath.Base(value.SpoolReferencePath) == "." {
		return errors.New("protected invocation lacks its compact spool index")
	}
	if _, err := readCurrentQgQualificationPin(currentQgQualificationPin{Path: value.SpoolReferencePath, Sha256: value.SpoolReferenceSha256}, winnerMultipassSpoolManifestBytes, true); err != nil {
		return err
	}
	loaded, err := loadWinnerHandoffSpool(ctx, value.SpoolReferencePath, binding, profile.StateRoot, read, winnerHandoffSpoolPortableVerifier(ctx, release, profile, read))
	if err != nil || loaded.reference.Commit != *value.SpoolCommit {
		return errors.New("protected invocation changed its exact original spool commit")
	}
	want := value
	want.SpoolCommit, want.SpoolReferencePath, want.SpoolReferenceSha256 = nil, "", ""
	actualBytes, _ := json.Marshal(loaded.commit.Invocation)
	wantBytes, _ := json.Marshal(want)
	if string(actualBytes) != string(wantBytes) {
		return errors.New("protected invocation changed its complete original census")
	}
	// Independent claims and native custody indexes are deliberately still here.
	// This check never rehydrates an old directory to invent physical ownership.
	if _, err := os.Lstat(loaded.commit.Attempt); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
