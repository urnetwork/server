// Synthetic actor/kernel/archive seams exercise the entire semantic reader.
// They are not deployment observations, actual Docker or provider qualification.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/server/model"
)

type winnerHandoffRuntimeReplayFixture struct {
	Capture   winnerHandoffRuntimeCapture
	Release   winnerMultipassQualificationRelease
	Member    winnerSnowMember
	Claims    map[string][]byte
	Originals map[string][]byte
	Hooks     winnerHandoffRuntimeReplayHooks
}

func newWinnerHandoffRuntimeReplayFixture(t *testing.T, memberIndex int) *winnerHandoffRuntimeReplayFixture {
	t.Helper()
	actor := newWinnerMultipassSpoolActor(t)
	bootstrap := actor.Plan.Release
	for name, profile := range bootstrap.TokenizationProfiles {
		profile.RuntimeEvidenceSha256 = ""
		bootstrap.TokenizationProfiles[name] = profile
	}
	releaseBytes, _ := json.Marshal(bootstrap)
	approval := winnerMultipassSha256([]byte("synthetic operator runtime permission"))
	identity, _ := json.Marshal(struct {
		Approval string
		Release  string
	}{Approval: approval, Release: winnerMultipassSha256(releaseBytes)})
	now := time.Now().UTC()
	owner := winnerMultipassOwner{Id: winnerMultipassSha256(identity), StartedAt: now.Add(-time.Minute), Deadline: now.Add(9 * time.Minute)}
	member := defaultWinnerSnowProviderSettings().Members[memberIndex]
	envelope, err := buildWinnerMultipassRuntimeProbe(owner, member, now.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	actor.Plan.Cases[1].Plan.Owner = owner
	input, _ := json.Marshal(envelope)
	output, diagnostics, err := actor.invoke(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	pending := actor.Pending[winnerMultipassSha256(input)]
	root := "/var/lib/urnetwork/competition-review-multipass/qualification-owners"
	ownerBytes, _ := json.Marshal(map[string]any{"schema": 1, "kind": "winner-review-multipass-runtime-owner/v1", "configuration_sha256": winnerMultipassSha256([]byte("synthetic configuration")), "permission_sha256": winnerMultipassSha256([]byte("synthetic native permission")), "user_approval_sha256": approval, "owner": owner, "release_sha256": winnerMultipassSha256(releaseBytes)})
	claimBytes, _ := json.Marshal(map[string]any{"schema": 1, "kind": "winner-review-multipass-runtime-call/v1", "owner": owner, "member": member, "input_sha256": winnerMultipassSha256(input), "started_at": now.Add(-time.Millisecond)})
	ownerPin := currentQgQualificationPin{Path: filepath.Join(root, owner.Id+".runtime.json"), Sha256: winnerMultipassSha256(ownerBytes)}
	claimPin := currentQgQualificationPin{Path: filepath.Join(root, owner.Id+"."+member.Name+".runtime.json"), Sha256: winnerMultipassSha256(claimBytes)}
	receiptBytes, _ := json.Marshal(pending.Receipt)
	proof := &winnerMultipassPhysicalAuthentication{Schema: 1, Kind: "winner-review-physical-containment-authentication/v1", Protocol: winnerMultipassVersion, ReleaseSha256: winnerMultipassSha256(releaseBytes), NativeDriver: bootstrap.NativeDriver, Profile: actor.Profile, ObservedAt: time.Now().UTC(), InputSha256: winnerMultipassSha256(input), OutputSha256: winnerMultipassSha256(output), ManifestSha256: pending.Receipt.ManifestSha256, WrapperReceiptSha256: pending.Receipt.WrapperReceiptSha256, AuthenticatedReceiptSha256: winnerMultipassSha256(receiptBytes)}
	custody := winnerHandoffInvocationCustody{Phase: "runtime_probe", Operation: "tokenize", CallId: envelope.Call.Id, RequestSha256: winnerMultipassSha256(envelope.Payload), ManifestSha256: pending.Receipt.ManifestSha256, WrapperReceiptSha256: pending.Receipt.WrapperReceiptSha256, Originals: []winnerHandoffCustodyOriginal{}}
	for _, binding := range append(append([]currentQgQualificationBinding{}, pending.Receipt.Artifacts...), currentQgQualificationBinding{Path: "wrapper-receipt.json", Sha256: pending.Receipt.WrapperReceiptSha256, Bytes: int64(len(pending.Receipt.WrapperReceipt))}) {
		custody.Originals = append(custody.Originals, winnerHandoffCustodyOriginal{Name: binding.Path, Bytes: binding.Bytes, Retention: model.CompetitionReviewHandoffRetention{Key: "synthetic/runtime/" + binding.Path, VersionId: "synthetic-original-version", Sha256: binding.Sha256, RetainUntil: owner.Deadline.Add(time.Hour)}})
	}
	fixture := &winnerHandoffRuntimeReplayFixture{Capture: winnerHandoffRuntimeCapture{Schema: 1, Kind: "winner-review-multipass-runtime-capture/v1", BootstrapRelease: bootstrap, Owner: owner, OwnerClaim: ownerPin, Claim: claimPin, Input: input, Output: output, Diagnostics: diagnostics, Containment: pending.Receipt, PhysicalAuthentication: proof, Custody: custody}, Release: bootstrap, Member: member, Claims: map[string][]byte{ownerPin.Path: ownerBytes, claimPin.Path: claimBytes}, Originals: pending.Originals}
	fixture.Release.TokenizationProfiles = map[string]winnerMultipassTokenizationProfile{}
	for name, profile := range bootstrap.TokenizationProfiles {
		profile.RuntimeEvidenceSha256 = winnerMultipassSha256([]byte("synthetic other member capture"))
		fixture.Release.TokenizationProfiles[name] = profile
	}
	fixture.Release.DeploymentDeclaration = currentQgQualificationPin{Path: "/synthetic/observed-deployment.json", Sha256: winnerMultipassSha256([]byte("synthetic observed declaration"))}
	fixture.Hooks = winnerHandoffRuntimeReplayHooks{
		ReadClaim: func(pin currentQgQualificationPin) ([]byte, error) {
			content, found := fixture.Claims[pin.Path]
			if !found || winnerMultipassSha256(content) != pin.Sha256 {
				return nil, errors.New("synthetic original claim missing or changed")
			}
			return content, nil
		},
		Authenticate: func(input, output, diagnostics []byte, profile winnerMultipassContainmentProfile) (*winnerMultipassContainmentReceipt, error) {
			bound := map[string][]byte{}
			for name, content := range fixture.Originals {
				if name != "wrapper-receipt.json" {
					bound[name] = content
				}
			}
			settings := defaultCurrentQgQualificationSettings()
			settings.StateRoot = profile.StateRoot
			if _, err := validateWinnerMultipassContainmentBytes(input, output, diagnostics, false, profile.ImageId, settings, currentQgSnowQualificationProfile(profile.ProviderSettings), bound, nil); err != nil {
				return nil, err
			}
			return pending.Receipt, nil
		},
		ReadOriginal: func(_ *winnerMultipassContainmentReceipt, binding currentQgQualificationBinding) ([]byte, error) {
			content, found := fixture.Originals[binding.Path]
			if !found {
				return nil, errors.New("synthetic retained original missing")
			}
			return content, nil
		},
	}
	return fixture
}

func (self *winnerHandoffRuntimeReplayFixture) content() []byte {
	content, _ := json.Marshal(self.Capture)
	profile := self.Release.TokenizationProfiles[self.Member.Name]
	profile.RuntimeEvidenceSha256 = winnerMultipassSha256(content)
	self.Release.TokenizationProfiles[self.Member.Name] = profile
	return content
}

func TestWinnerHandoffRuntimeEvidenceReplaysOriginalTypedProbeAndClaims(t *testing.T) {
	for index, member := range defaultWinnerSnowProviderSettings().Members {
		t.Run(member.Name, func(t *testing.T) {
			fixture := newWinnerHandoffRuntimeReplayFixture(t, index)
			if err := validateWinnerHandoffRuntimeEvidence(fixture.content(), fixture.Release, fixture.Member, fixture.Hooks); err != nil {
				t.Fatal("complete synthetic original runtime receipt failed", err)
			}
		})
	}
}

func TestWinnerHandoffRuntimeEvidenceRejectsPinnedButUnprovedOrMissingOriginals(t *testing.T) {
	for _, item := range []struct {
		name   string
		mutate func(*winnerHandoffRuntimeReplayFixture)
	}{
		{name: "changed_binary", mutate: func(f *winnerHandoffRuntimeReplayFixture) {
			f.Capture.BootstrapRelease.TrustedCli.Sha256 = winnerMultipassSha256([]byte("different source binary"))
		}},
		{name: "failed_capture", mutate: func(f *winnerHandoffRuntimeReplayFixture) { f.Capture.Error = "synthetic failed original" }},
		{name: "missing_intent", mutate: func(f *winnerHandoffRuntimeReplayFixture) { delete(f.Claims, f.Capture.Claim.Path) }},
		{name: "missing_owner", mutate: func(f *winnerHandoffRuntimeReplayFixture) { delete(f.Claims, f.Capture.OwnerClaim.Path) }},
		{name: "missing_physical_original", mutate: func(f *winnerHandoffRuntimeReplayFixture) { delete(f.Originals, "uid-map.txt") }},
		{name: "missing_protected_version", mutate: func(f *winnerHandoffRuntimeReplayFixture) { f.Capture.Custody.Originals[0].Retention.VersionId = "" }},
		{name: "wrong_physical_driver", mutate: func(f *winnerHandoffRuntimeReplayFixture) {
			f.Capture.PhysicalAuthentication.NativeDriver.Sha256 = winnerMultipassSha256([]byte("different driver"))
		}},
		{name: "different_member", mutate: func(f *winnerHandoffRuntimeReplayFixture) { f.Member = defaultWinnerSnowProviderSettings().Members[1] }},
	} {
		t.Run(item.name, func(t *testing.T) {
			fixture := newWinnerHandoffRuntimeReplayFixture(t, 0)
			item.mutate(fixture)
			if err := validateWinnerHandoffRuntimeEvidence(fixture.content(), fixture.Release, fixture.Member, fixture.Hooks); err == nil {
				t.Fatal("pinned runtime capture bypassed exact original replay")
			}
		})
	}
	fixture := newWinnerHandoffRuntimeReplayFixture(t, 0)
	arbitrary := []byte(`{"max_model_len":98304,"model":"synthetic alias alone"}`)
	profile := fixture.Release.TokenizationProfiles[fixture.Member.Name]
	profile.RuntimeEvidenceSha256 = winnerMultipassSha256(arbitrary)
	fixture.Release.TokenizationProfiles[fixture.Member.Name] = profile
	if err := validateWinnerHandoffRuntimeEvidence(arbitrary, fixture.Release, fixture.Member, fixture.Hooks); err == nil {
		t.Fatal("arbitrary pinned runtime bytes were accepted as observed evidence")
	}
}
