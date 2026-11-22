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

	"github.com/urnetwork/server/v2026/model"
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
	return newWinnerHandoffRuntimeReplayFixtureForOwner(t, memberIndex, nil)
}

func newWinnerHandoffRuntimeReplayFixtureForOwner(t *testing.T, memberIndex int, originalOwner *winnerMultipassOwner) *winnerHandoffRuntimeReplayFixture {
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
	if originalOwner != nil {
		owner = *originalOwner
	}
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
	claimBytes, _ := json.Marshal(map[string]any{"schema": 1, "kind": "winner-review-multipass-runtime-call/v1", "owner": owner, "member": member, "input_sha256": winnerMultipassSha256(input), "started_at": now})
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

func TestWinnerHandoffRuntimePairRequiresOneCompleteOrderedOwnerReport(t *testing.T) {
	qwen := newWinnerHandoffRuntimeReplayFixture(t, 0)
	gemma := newWinnerHandoffRuntimeReplayFixtureForOwner(t, 1, &qwen.Capture.Owner)
	qwenBytes, gemmaBytes := qwen.content(), gemma.content()
	release := qwen.Release
	release.TokenizationProfiles["gemma"] = gemma.Release.TokenizationProfiles["gemma"]
	for _, fixture := range []*winnerHandoffRuntimeReplayFixture{qwen, gemma} {
		content, _ := json.Marshal(fixture.Capture)
		if err := validateWinnerHandoffRuntimeEvidence(content, release, fixture.Member, fixture.Hooks); err != nil {
			t.Fatal("paired synthetic member lost independent complete proof", err)
		}
	}
	claims := map[string][]byte{}
	for _, fixture := range []*winnerHandoffRuntimeReplayFixture{qwen, gemma} {
		for path, content := range fixture.Claims {
			claims[path] = content
		}
	}
	reader := func(pin currentQgQualificationPin) ([]byte, error) {
		value, found := claims[pin.Path]
		if !found || winnerMultipassSha256(value) != pin.Sha256 {
			return nil, errors.New("synthetic pair original intent missing")
		}
		return value, nil
	}
	report := winnerHandoffRuntimeReport{Schema: 1, Kind: "winner-review-multipass-runtime-bootstrap-report/v1", Owner: qwen.Capture.Owner, Captures: []currentQgQualificationPin{{Path: "/synthetic/runtime/qwen-capture.json", Sha256: winnerMultipassSha256(qwenBytes)}, {Path: "/synthetic/runtime/gemma-capture.json", Sha256: winnerMultipassSha256(gemmaBytes)}}, Profiles: release.TokenizationProfiles, Complete: true}
	evidence := func(value winnerHandoffRuntimeReport) map[string][]byte {
		content, _ := json.Marshal(value)
		return map[string][]byte{winnerMultipassSha256(qwenBytes): qwenBytes, winnerMultipassSha256(gemmaBytes): gemmaBytes, winnerMultipassSha256(content): content}
	}
	if err := validateWinnerHandoffRuntimePairWithClaims(evidence(report), release, reader); err != nil {
		t.Fatal("complete single-owner runtime pair refused", err)
	}
	for _, mutate := range []func(*winnerHandoffRuntimeReport){
		func(value *winnerHandoffRuntimeReport) {
			value.Complete = false
			value.Error = "synthetic final custody completed late"
		},
		func(value *winnerHandoffRuntimeReport) { value.Owner.Deadline = value.Owner.Deadline.Add(time.Second) },
		func(value *winnerHandoffRuntimeReport) {
			value.Captures = []currentQgQualificationPin{report.Captures[1], report.Captures[0]}
		},
	} {
		changed := report
		mutate(&changed)
		if err := validateWinnerHandoffRuntimePairWithClaims(evidence(changed), release, reader); err == nil {
			t.Fatal("runtime pair accepted failed, reset or reordered original terminal")
		}
	}
	withoutReport := map[string][]byte{winnerMultipassSha256(qwenBytes): qwenBytes, winnerMultipassSha256(gemmaBytes): gemmaBytes}
	if err := validateWinnerHandoffRuntimePairWithClaims(withoutReport, release, reader); err == nil {
		t.Fatal("two individually complete observations bypassed missing original paired terminal")
	}
	for _, mutate := range []func(*winnerHandoffRuntimeCapture){
		func(value *winnerHandoffRuntimeCapture) {
			value.Owner.StartedAt = value.Owner.StartedAt.Add(time.Millisecond)
		},
		func(value *winnerHandoffRuntimeCapture) {
			value.OwnerClaim.Sha256 = winnerMultipassSha256([]byte("synthetic another owner claim"))
		},
		func(value *winnerHandoffRuntimeCapture) {
			value.PhysicalAuthentication.ObservedAt = value.Owner.Deadline
		},
	} {
		content, _ := json.Marshal(gemma.Capture)
		var changed winnerHandoffRuntimeCapture
		if err := json.Unmarshal(content, &changed); err != nil {
			t.Fatal(err)
		}
		mutate(&changed)
		changedBytes, _ := json.Marshal(changed)
		changedRelease := release
		changedRelease.TokenizationProfiles = map[string]winnerMultipassTokenizationProfile{}
		for name, profile := range release.TokenizationProfiles {
			changedRelease.TokenizationProfiles[name] = profile
		}
		profile := changedRelease.TokenizationProfiles["gemma"]
		profile.RuntimeEvidenceSha256 = winnerMultipassSha256(changedBytes)
		changedRelease.TokenizationProfiles["gemma"] = profile
		changedReport := report
		changedReport.Profiles = changedRelease.TokenizationProfiles
		changedReport.Captures = append([]currentQgQualificationPin{}, report.Captures...)
		changedReport.Captures[1].Sha256 = profile.RuntimeEvidenceSha256
		reportBytes, _ := json.Marshal(changedReport)
		changedEvidence := map[string][]byte{winnerMultipassSha256(qwenBytes): qwenBytes, winnerMultipassSha256(changedBytes): changedBytes, winnerMultipassSha256(reportBytes): reportBytes}
		if err := validateWinnerHandoffRuntimePairWithClaims(changedEvidence, changedRelease, reader); err == nil {
			t.Fatal("runtime pair mixed original owners or admitted half-open-deadline proof")
		}
	}
}

func (self *winnerHandoffRuntimeReplayFixture) content() []byte {
	content, _ := json.Marshal(self.Capture)
	profile := self.Release.TokenizationProfiles[self.Member.Name]
	profile.RuntimeEvidenceSha256 = winnerMultipassSha256(content)
	self.Release.TokenizationProfiles[self.Member.Name] = profile
	return content
}

func TestWinnerHandoffRuntimeEvidenceReplaysOriginalTypedProbeAndClaims(t *testing.T) {
	for index := range defaultWinnerSnowProviderSettings().Members {
		{
			fixture := newWinnerHandoffRuntimeReplayFixture(t, index)
			if err := validateWinnerHandoffRuntimeEvidence(fixture.content(), fixture.Release, fixture.Member, fixture.Hooks); err != nil {
				t.Fatal("complete synthetic original runtime receipt failed", err)
			}
		}
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
		{
			fixture := newWinnerHandoffRuntimeReplayFixture(t, 0)
			item.mutate(fixture)
			if err := validateWinnerHandoffRuntimeEvidence(fixture.content(), fixture.Release, fixture.Member, fixture.Hooks); err == nil {
				t.Fatal("pinned runtime capture bypassed exact original replay", item.name)
			}
		}
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
