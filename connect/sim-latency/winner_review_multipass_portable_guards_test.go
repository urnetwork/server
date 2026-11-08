// Portable guards use synthetic protected-readback callbacks and reserved-IP
// containment fixtures. They are never real driver or provider attestations.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func winnerMultipassPortableFixture(t *testing.T) (map[string][]byte, *winnerMultipassPhysicalAuthentication, winnerMultipassQualificationRelease, winnerMultipassContainmentProfile, currentQgQualificationProviderProfile, *winnerMultipassContainmentReceipt, string) {
	t.Helper()
	input, output, diagnostics, image, settings, attempt, provider, _ := winnerMultipassCanceledContainmentFixture(t)
	if _, err := authenticateWinnerMultipassContainmentInternal(input, output, diagnostics, true, image, settings, provider); err != nil {
		t.Fatal(err)
	}
	originals, claim, wrapper, err := readWinnerMultipassPhysicalOriginals(input, diagnostics, settings)
	if err != nil || len(claim) != 0 {
		t.Fatal("synthetic canceled original census failed", err)
	}
	var manifest struct {
		Schema    int                             `json:"schema"`
		Kind      string                          `json:"kind"`
		Artifacts []currentQgQualificationBinding `json:"artifacts"`
	}
	if err := json.Unmarshal(originals["evidence-manifest.json"], &manifest); err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(diagnostics), []byte("\n"))
	rawReceipt := lines[len(lines)-1]
	receipt := &winnerMultipassContainmentReceipt{AttemptDirectory: attempt, ManifestSha256: wrapper.ManifestSha256, WrapperReceiptSha256: winnerMultipassSha256(rawReceipt), WrapperReceipt: rawReceipt, Artifacts: manifest.Artifacts, Canceled: true}
	receipt.Artifacts = append(receipt.Artifacts, currentQgQualificationBinding{Path: "evidence-manifest.json", Sha256: wrapper.ManifestSha256, Bytes: int64(len(originals["evidence-manifest.json"]))})
	originals["wrapper-receipt.json"] = rawReceipt
	release := winnerMultipassQualificationFixture(t).Release
	release.ImageId, release.ProviderSettings = image, provider.Settings
	profile := winnerMultipassContainmentProfile{ImageId: image, ProviderSettings: provider.Settings, StateRoot: settings.StateRoot}
	releaseBytes, _ := json.Marshal(release)
	receiptBytes, _ := json.Marshal(receipt)
	proof := &winnerMultipassPhysicalAuthentication{Schema: 1, Kind: "winner-review-physical-containment-authentication/v1", Protocol: winnerMultipassVersion, ReleaseSha256: winnerMultipassSha256(releaseBytes), NativeDriver: release.NativeDriver, Profile: profile, ObservedAt: time.Now().UTC(),
		InputSha256: winnerMultipassSha256(input), OutputSha256: winnerMultipassSha256(output), ManifestSha256: wrapper.ManifestSha256, WrapperReceiptSha256: receipt.WrapperReceiptSha256, AuthenticatedReceiptSha256: winnerMultipassSha256(receiptBytes), Canceled: true}
	return originals, proof, release, profile, provider, receipt, attempt
}

func TestWinnerMultipassPortableReplayRequiresProtectedProofNotRecreatedFilesystem(t *testing.T) {
	originals, proof, release, profile, provider, want, attempt := winnerMultipassPortableFixture(t)
	content, _ := json.Marshal(proof)
	minimum := time.Now().UTC().Add(time.Hour)
	reference := winnerMultipassProtectedReference{Key: "synthetic/protected/physical-proof", VersionId: "synthetic-exact-version", Sha256: winnerMultipassSha256(content), Bytes: int64(len(content)), RetainUntil: minimum.Add(time.Hour)}
	authenticated, err := loadWinnerMultipassPhysicalAuthentication(context.Background(), reference, release, minimum, func(_ context.Context, actual winnerMultipassProtectedReference) ([]byte, error) {
		if actual != reference {
			return nil, errors.New("synthetic exact-version mismatch")
		}
		return content, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Move only this test-owned temporary fixture. Its original path ceases to
	// exist; replay must not recreate it or claim the new inode was the original.
	if err := os.Rename(attempt, attempt+".retired-fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(attempt); !os.IsNotExist(err) {
		t.Fatal("synthetic original path still exists")
	}
	actual, err := replayWinnerMultipassPortableContainmentForProfile(originals, nil, authenticated, profile, provider)
	if err != nil || !reflect.DeepEqual(actual, want) {
		t.Fatal("protected original replay depended on reconstructed files", err)
	}
	if _, err := os.Lstat(attempt); !os.IsNotExist(err) {
		t.Fatal("portable replay synthesized an original filesystem path")
	}
	if _, err := replayWinnerMultipassPortableContainmentForProfile(originals, nil, nil, profile, provider); err == nil {
		t.Fatal("unattested archived bytes authorized portable physical claims")
	}
	for _, mutate := range []func(map[string][]byte){
		func(value map[string][]byte) {
			value["cgroup.json"] = []byte(`{"synthetic":"substituted kernel observation"}`)
		},
		func(value map[string][]byte) { delete(value, "uid-map.txt") },
		func(value map[string][]byte) { value["unplanned.json"] = []byte(`{}`) },
		func(value map[string][]byte) {
			value["wrapper-receipt.json"] = append(append([]byte{}, value["wrapper-receipt.json"]...), '\n')
		},
	} {
		changed := map[string][]byte{}
		for name, value := range originals {
			changed[name] = value
		}
		mutate(changed)
		if _, err := replayWinnerMultipassPortableContainmentForProfile(changed, nil, authenticated, profile, provider); err == nil {
			t.Fatal("changed protected original census/bytes accepted")
		}
	}
	if _, err := replayWinnerMultipassPortableContainmentForProfile(originals, []byte("synthetic substituted claim"), authenticated, profile, provider); err == nil {
		t.Fatal("cancellation gained a generation claim")
	}
}

func TestWinnerMultipassPortableProofRejectsMissingVersionChangedDriverAndAlteredReadback(t *testing.T) {
	_, proof, release, _, _, _, _ := winnerMultipassPortableFixture(t)
	content, _ := json.Marshal(proof)
	minimum := time.Now().UTC().Add(time.Hour)
	reference := winnerMultipassProtectedReference{Key: "synthetic/protected/proof", VersionId: "synthetic-version", Sha256: winnerMultipassSha256(content), Bytes: int64(len(content)), RetainUntil: minimum.Add(time.Hour)}
	load := func(context.Context, winnerMultipassProtectedReference) ([]byte, error) { return content, nil }
	for _, mutate := range []func(*winnerMultipassProtectedReference){
		func(value *winnerMultipassProtectedReference) { value.VersionId = "" },
		func(value *winnerMultipassProtectedReference) { value.RetainUntil = minimum.Add(-time.Second) },
		func(value *winnerMultipassProtectedReference) { value.Bytes++ },
	} {
		changed := reference
		mutate(&changed)
		if _, err := loadWinnerMultipassPhysicalAuthentication(context.Background(), changed, release, minimum, load); err == nil {
			t.Fatal("unprotected or changed physical-proof version accepted")
		}
	}
	changedRelease := release
	changedRelease.NativeDriver.Path = filepath.Join("/synthetic", "different-driver")
	if _, err := loadWinnerMultipassPhysicalAuthentication(context.Background(), reference, changedRelease, minimum, load); err == nil {
		t.Fatal("another native driver inherited physical authentication")
	}
	if _, err := loadWinnerMultipassPhysicalAuthentication(context.Background(), reference, release, minimum, nil); err == nil {
		t.Fatal("missing exact-version archive reader accepted")
	}
	if _, err := loadWinnerMultipassPhysicalAuthentication(context.Background(), reference, release, minimum, func(context.Context, winnerMultipassProtectedReference) ([]byte, error) {
		return append(append([]byte{}, content...), '\n'), nil
	}); err == nil {
		t.Fatal("changed protected proof bytes accepted")
	}
}
