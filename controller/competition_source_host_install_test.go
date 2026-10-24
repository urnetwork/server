// Synthetic source/host records exercise promotion handoff and exact authority
// boundaries. No test probes a host, accesses main services or installs config.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

type competitionReviewedHostFixture struct {
	Settings    *Settings
	Intent      *model.CompetitionSourceActivation
	Receipt     CompetitionReviewedSourceBuildReceipt
	Profile     CompetitionProspectiveHostProfile
	Active      []byte
	Prospective []byte
}

// Complete positive evidence includes a qualified incumbent but explicitly
// unqualified new-image runtime markers. The hardware reports are synthetic.
func newCompetitionReviewedHostFixture(t *testing.T) competitionReviewedHostFixture {
	t.Helper()
	settings := validSettings()
	settings.ConfigLocalDirectory, settings.VaultLocalDirectory = "/synthetic/config/local", "/synthetic/vault/local"
	settings.EvaluationPolicy.HardwareId = "synthetic-hardware"
	settings.EvaluationPolicy.ConfigLocalSha256, settings.EvaluationPolicy.VaultLocalSha256 = strings.Repeat("e", 64), strings.Repeat("f", 64)
	settings.EvaluatorImageDigest, settings.EvaluationPolicy.HostQualificationSha256 = "sha256:"+strings.Repeat("b", 64), strings.Repeat("d", 64)
	incumbentSettings := *settings
	incumbentSettings.EvaluatorImageDigest, incumbentSettings.EvaluationPolicy.HostQualificationSha256 = "sha256:"+strings.Repeat("a", 64), strings.Repeat("c", 64)
	parent := passingHostCheck(&incumbentSettings)
	parent.HostId, parent.KernelRelease, parent.MicrocodeRevision = "synthetic-host", "synthetic-kernel", "synthetic-microcode"
	parent.Checks = map[string]bool{"config_secure": true, "qualification_match": true, "cpu_count_exact": true, "worker_affinity_pinned": true,
		"kernel_microcode_pinned": true, "docker_runtime": true, "docker_user_namespace": true}
	prospective := parent
	prospective.Checks = maps.Clone(parent.Checks)
	prospective.Checks["qualification_match"] = false
	prospective.ImageDigest, prospective.QualificationSha256 = settings.EvaluatorImageDigest, settings.EvaluationPolicy.HostQualificationSha256
	prospective.RebaselinePassed, prospective.RebaselineRoundId = false, nil
	prospective.CleanupVerified, prospective.ResourceLimitsVerified, prospective.ResourceBombCleanupVerified = false, false, false
	prospective.ServicesInJobCgroup, prospective.DefaultDenyNetwork, prospective.TemplateDatabase, prospective.RedisReset = false, false, false, false
	prospective.ArtifactStorage, prospective.ImmutableReports, prospective.NoProductionSecrets = false, false, false
	reviewId := server.NewId()
	intent := &model.CompetitionSourceActivation{Kind: "reviewed_source_promotion", ReviewId: &reviewId,
		ApprovedPatchSha256: strings.Repeat("1", 64), SourceLockSha256: strings.Repeat("2", 64), EvaluatedImageDigest: parent.ImageDigest,
		EvaluatorImageDigest: settings.EvaluatorImageDigest, HostQualificationSha256: settings.EvaluationPolicy.HostQualificationSha256}
	receipt := CompetitionReviewedSourceBuildReceipt{Schema: 1, ReviewId: reviewId, PatchSha256: intent.ApprovedPatchSha256, SourceLockSha256: intent.SourceLockSha256,
		EvaluatorImageDigest: intent.EvaluatorImageDigest, HostQualificationSha256: intent.HostQualificationSha256, ContainmentSha256: strings.Repeat("3", 64),
		TestEvidenceSha256: strings.Repeat("4", 64), SandboxId: strings.Repeat("5", 64), Offline: true, NoMainCredentials: true, ReadOnlySource: true, Passed: true,
		TestModel: "gpt-6.1-sol", TestReasoning: "low"}
	fields := map[string]any{"schema": 1, "hardware_id": settings.EvaluationPolicy.HardwareId, "host_id": parent.HostId,
		"image_digest": parent.ImageDigest, "qualification_sha256": parent.QualificationSha256,
		"config_local_directory": settings.ConfigLocalDirectory, "vault_local_directory": settings.VaultLocalDirectory,
		"config_local_sha256": settings.EvaluationPolicy.ConfigLocalSha256, "vault_local_sha256": settings.EvaluationPolicy.VaultLocalSha256,
		"rebaseline_manifest": "/synthetic/old-marker.json", "rebaseline_manifest_sha256": strings.Repeat("6", 64),
		"management_cpu_list": "10,11", "retained_metadata": map[string]bool{"synthetic": true}}
	active, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	fields["image_digest"], fields["qualification_sha256"] = settings.EvaluatorImageDigest, ""
	candidate, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(candidate)
	profile := CompetitionProspectiveHostProfile{Schema: 1, Kind: "prospective-source-hardware-fingerprint/v1", RuntimeQualified: false,
		ImageId: intent.EvaluatorImageDigest, BaseImageId: intent.EvaluatedImageDigest, SourceLockSha256: intent.SourceLockSha256, ReviewId: reviewId.String(),
		QualificationSha256: intent.HostQualificationSha256, ProspectiveConfigSha256: hex.EncodeToString(digest[:]),
		ConfigLocalSha256: settings.EvaluationPolicy.ConfigLocalSha256, VaultLocalSha256: settings.EvaluationPolicy.VaultLocalSha256, BootId: "synthetic-boot",
		Incumbent: parent, Prospective: prospective}
	return competitionReviewedHostFixture{Settings: settings, Intent: intent, Receipt: receipt, Profile: profile, Active: active, Prospective: candidate}
}

// The pre-fix handoff left the incumbent image pin in place. The installed
// document must now bind the approved winner and preserve every other field.
func TestCompetitionReviewedHostInstallChangesOnlyApprovedImageAndFingerprint(t *testing.T) {
	f := newCompetitionReviewedHostFixture(t)
	before := bytes.Clone(f.Active)
	content, err := competitionReviewedHostInstallContent(f.Settings, f.Intent, f.Active, f.Prospective, f.Receipt, f.Profile, f.Profile.BootId)
	if err != nil {
		t.Fatal(err)
	}
	var old, installed map[string]any
	if json.Unmarshal(f.Active, &old) != nil || json.Unmarshal(content, &installed) != nil || !bytes.Equal(before, f.Active) ||
		installed["image_digest"] != f.Intent.EvaluatorImageDigest || installed["qualification_sha256"] != f.Intent.HostQualificationSha256 {
		t.Fatal("reviewed winner did not replace exactly the expected host identity")
	}
	for _, key := range []string{"image_digest", "qualification_sha256"} {
		delete(old, key)
		delete(installed, key)
	}
	if !reflect.DeepEqual(old, installed) || f.Profile.RuntimeQualified || f.Profile.Prospective.RebaselinePassed || f.Profile.Prospective.CleanupVerified {
		t.Fatal("host installation changed unrelated policy or manufactured new-image runtime readiness")
	}
}

// A newer incumbent reset marker is not part of the physical fingerprint;
// installation keeps it, rather than restoring an old snapshot's evidence.
func TestCompetitionReviewedHostInstallPreservesCurrentRebaselineLinks(t *testing.T) {
	f := newCompetitionReviewedHostFixture(t)
	var current map[string]any
	if err := json.Unmarshal(f.Active, &current); err != nil {
		t.Fatal(err)
	}
	current["rebaseline_manifest"], current["rebaseline_manifest_sha256"] = "/synthetic/newer-marker.json", strings.Repeat("7", 64)
	active, err := json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	content, err := competitionReviewedHostInstallContent(f.Settings, f.Intent, active, f.Prospective, f.Receipt, f.Profile, f.Profile.BootId)
	if err != nil {
		t.Fatal(err)
	}
	var installed map[string]any
	if json.Unmarshal(content, &installed) != nil || installed["rebaseline_manifest"] != current["rebaseline_manifest"] || installed["rebaseline_manifest_sha256"] != current["rebaseline_manifest_sha256"] {
		t.Fatal("reviewed host installation revived stale reset evidence")
	}
}

// Each approval/image/physical/local-leaf substitution must refuse the handoff,
// even if all other booleans and digest strings remain plausible.
func TestCompetitionReviewedHostInstallRejectsSubstitutedAuthority(t *testing.T) {
	for _, mutate := range []func(*competitionReviewedHostFixture){
		func(f *competitionReviewedHostFixture) { f.Intent.Kind = "reviewed_source_carry_forward" },
		func(f *competitionReviewedHostFixture) { f.Intent.ReviewId = nil },
		func(f *competitionReviewedHostFixture) { f.Receipt.Passed = false },
		func(f *competitionReviewedHostFixture) { f.Receipt.NoMainCredentials = false },
		func(f *competitionReviewedHostFixture) { f.Receipt.TestReasoning = "max" },
		func(f *competitionReviewedHostFixture) { f.Receipt.EvaluatorImageDigest = f.Profile.BaseImageId },
		func(f *competitionReviewedHostFixture) { f.Receipt.SandboxId = "../redirected" },
		func(f *competitionReviewedHostFixture) { f.Profile.RuntimeQualified = true },
		func(f *competitionReviewedHostFixture) { f.Profile.BootId = "other-synthetic-boot" },
		func(f *competitionReviewedHostFixture) { f.Profile.SourceLockSha256 = strings.Repeat("8", 64) },
		func(f *competitionReviewedHostFixture) { f.Profile.ProspectiveConfigSha256 = strings.Repeat("8", 64) },
		func(f *competitionReviewedHostFixture) { f.Profile.Incumbent.ResourceBombCleanupVerified = false },
		func(f *competitionReviewedHostFixture) { f.Profile.Incumbent.RebaselineRoundId = nil },
		func(f *competitionReviewedHostFixture) { f.Profile.Prospective.ManagementCpuReserved = false },
		func(f *competitionReviewedHostFixture) { f.Profile.Prospective.Checks["unexpected_control"] = false },
		func(f *competitionReviewedHostFixture) {
			f.Profile.Prospective.KernelRelease = "different-synthetic-kernel"
		},
		func(f *competitionReviewedHostFixture) { f.Profile.ConfigLocalSha256 = strings.Repeat("8", 64) },
		func(f *competitionReviewedHostFixture) { f.Settings.VaultLocalDirectory = "/synthetic/vault/main" },
		func(f *competitionReviewedHostFixture) {
			f.Active = bytes.Replace(f.Active, []byte(`"management_cpu_list":"10,11"`), []byte(`"management_cpu_list":"0-23"`), 1)
		},
	} {
		f := newCompetitionReviewedHostFixture(t)
		mutate(&f)
		if content, err := competitionReviewedHostInstallContent(f.Settings, f.Intent, f.Active, f.Prospective, f.Receipt, f.Profile, "synthetic-boot"); err == nil || content != nil {
			t.Fatal("substituted host/source authority produced an installable configuration")
		}
	}
}

// Duplicate keys must not be accepted just because encoding/json resolves
// them to the same value. This covers the sibling root-reader parser too.
func TestCompetitionReviewedHostInstallRejectsAmbiguousRootDocuments(t *testing.T) {
	f := newCompetitionReviewedHostFixture(t)
	for _, prefix := range []string{`{"schema":1,`, `{"Schema":1,`} {
		changed := append([]byte(prefix), f.Active[1:]...)
		if content, err := competitionReviewedHostInstallContent(f.Settings, f.Intent, changed, f.Prospective, f.Receipt, f.Profile, f.Profile.BootId); err == nil || content != nil {
			t.Fatal("ambiguous root host fields were accepted")
		}
	}
}

// Exact callback order proves that loss of either lease, physical drift,
// cancellation, or a concurrent root edit prevents replacement without sleeps.
func TestCompetitionReviewedHostInstallRechecksFenceAndFileBeforeReplacement(t *testing.T) {
	for _, failure := range []string{"", "admission_fence", "commit_fence", "probe", "image", "fingerprint", "physical", "changed_file", "cancellation", "cancel_admission", "cancel_commit", "cancel_current"} {
		f := newCompetitionReviewedHostFixture(t)
		content, err := competitionReviewedHostInstallContent(f.Settings, f.Intent, f.Active, f.Prospective, f.Receipt, f.Profile, f.Profile.BootId)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		events := []string{}
		fences, replaced := 0, false
		fence := func(context.Context) error {
			events = append(events, "fence")
			fences++
			if failure == "admission_fence" && fences == 1 || failure == "commit_fence" && fences == 2 {
				return ErrLeaseLost
			}
			if failure == "cancel_admission" && fences == 1 || failure == "cancel_commit" && fences == 2 {
				cancel()
			}
			return nil
		}
		probe := func(context.Context, []byte) (HostSelfCheck, error) {
			events = append(events, "probe")
			host := f.Profile.Prospective
			host.Checks = maps.Clone(host.Checks)
			host.Checks["qualification_match"] = true
			switch failure {
			case "probe":
				return HostSelfCheck{}, errors.New("synthetic failed probe")
			case "image":
				host.ImageDigest = f.Profile.BaseImageId
			case "fingerprint":
				host.QualificationSha256 = strings.Repeat("9", 64)
			case "physical":
				host.ManagementMemoryReserved = false
			case "cancellation":
				cancel()
			}
			return host, nil
		}
		current := func() ([]byte, error) {
			events = append(events, "current")
			if failure == "cancel_current" {
				cancel()
			}
			if failure == "changed_file" {
				return []byte("changed synthetic root config"), nil
			}
			return f.Active, nil
		}
		replace := func(actual []byte) error {
			events = append(events, "replace")
			replaced = true
			if !bytes.Equal(content, actual) {
				return errors.New("changed install content")
			}
			return nil
		}
		err = executeCompetitionReviewedHostInstall(ctx, f.Active, content, f.Settings, fence, probe, current, replace)
		cancel()
		if (err != nil) != (failure != "") || replaced != (failure == "") {
			t.Fatal("host install crossed a failed authority boundary", failure, events, err)
		}
		if failure == "" && !reflect.DeepEqual(events, []string{"fence", "probe", "fence", "current", "replace"}) {
			t.Fatal("host install reordered authority/probe/commit operations", events)
		}
		if failure == "cancel_admission" && !reflect.DeepEqual(events, []string{"fence"}) {
			t.Fatal("host probe ran after canceled admission", events)
		}
		if failure == "cancel_commit" && !reflect.DeepEqual(events, []string{"fence", "probe", "fence"}) {
			t.Fatal("host configuration was read after canceled commit admission", events)
		}
	}
}

// Reproduce the publisher's exact crash shape: the final name exists beside
// its temporary hard link. Candidate-artifact policy still rejects it, while
// the separately root-gated private-record reader can authenticate its bytes.
func TestCompetitionReviewedBuildRecordSurvivesInterruptedLinkPublication(t *testing.T) {
	root := t.TempDir()
	path, alias := filepath.Join(root, "record.json"), filepath.Join(root, ".interrupted-publication")
	content := []byte(`{"schema":1,"synthetic":true}`)
	if err := os.WriteFile(path, content, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := readCompetitionPreparationFile(path, 1024, true); err == nil || !strings.Contains(err.Error(), "singly linked regular file") {
		t.Fatal("interrupted-publication fixture did not reproduce the candidate-artifact link-count rejection", err)
	}
	var record struct {
		Schema    int  `json:"schema"`
		Synthetic bool `json:"synthetic"`
	}
	got, err := readCompetitionReviewedBuildRecordContent(path, 1024, &record)
	if err != nil || !bytes.Equal(content, got) || record.Schema != 1 || !record.Synthetic {
		t.Fatal("sealed private record was lost after interrupted exclusive publication", err)
	}
}

// Trusted ancestry is not permission to follow leaf aliases, accept changed
// permissions, exceed a budget or decode duplicate authority fields.
func TestCompetitionReviewedBuildRecordRejectsUnsafeLeafAndAmbiguousContent(t *testing.T) {
	for _, invalid := range []string{"symlink", "writable", "oversized", "duplicate"} {
		root := t.TempDir()
		path := filepath.Join(root, "record.json")
		content := []byte(`{"schema":1}`)
		if invalid == "duplicate" {
			content = []byte(`{"schema":1,"schema":1}`)
		}
		if err := os.WriteFile(path, content, 0o400); err != nil {
			t.Fatal(err)
		}
		limit := int64(1024)
		switch invalid {
		case "symlink":
			alias := filepath.Join(root, "alias.json")
			if err := os.Symlink(path, alias); err != nil {
				t.Fatal(err)
			}
			path = alias
		case "writable":
			if err := os.Chmod(path, 0o600); err != nil {
				t.Fatal(err)
			}
		case "oversized":
			limit = 1
		}
		var record struct {
			Schema int `json:"schema"`
		}
		if _, err := readCompetitionReviewedBuildRecordContent(path, limit, &record); err == nil {
			t.Fatal("unsafe private record accepted", invalid)
		}
	}
}
