// Synthetic checker reports distinguish a new hardware fingerprint from actual
// round readiness. No fixture claims live hardware or independent model proof.
package main

import (
	"bytes"
	"encoding/json"
	"maps"
	"reflect"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
)

// Build a complete incumbent report and a prospective report whose physical
// controls pass but whose old-image runtime markers explicitly do not.
func sourceHostProfileFixture(t *testing.T) (reviewedSourceBuildRequest, sourceBuildArtifacts, []byte, controller.HostSelfCheck, controller.HostSelfCheck) {
	t.Helper()
	request := sourceBuildContextFixture(t)
	artifacts := sourceBuildArtifacts{ImageId: "sha256:" + strings.Repeat("b", 64)}
	rebaseline := server.NewId()
	parent := controller.HostSelfCheck{Schema: 1, HostId: "synthetic-host", HardwareId: "synthetic-hardware",
		ImageDigest: request.Proposal.EvaluatedImageDigest, QualificationSha256: strings.Repeat("a", 64),
		KernelRelease: "synthetic-kernel", MicrocodeRevision: "synthetic-microcode", IrqAffinitySha256: strings.Repeat("e", 64), IrqPolicySha256: strings.Repeat("f", 64),
		LogicalCpuCount: 12, SMTDisabled: true, GovernorPinned: true, TurboPinned: true, NumaPinned: true, IrqPinned: true, CgroupV2: true,
		ServicesInJobCgroup: true, DefaultDenyNetwork: true, OfflineBuildCache: true, TemplateDatabase: true, RedisReset: true,
		ArtifactStorage: true, ImmutableReports: true, NoProductionSecrets: true, CleanupVerified: true, ResourceLimitsVerified: true,
		ManagementCpuReserved: true, ManagementMemoryReserved: true, ResourceBombCleanupVerified: true, RebaselinePassed: true, RebaselineRoundId: &rebaseline,
		Checks: map[string]bool{"config_secure": true, "qualification_match": true, "cpu_count_exact": true, "worker_affinity_pinned": true,
			"kernel_microcode_pinned": true, "docker_runtime": true, "docker_user_namespace": true}}
	prospective := parent
	prospective.ImageDigest, prospective.QualificationSha256 = artifacts.ImageId, strings.Repeat("c", 64)
	prospective.Checks = maps.Clone(parent.Checks)
	prospective.Checks["qualification_match"] = false
	prospective.ServicesInJobCgroup, prospective.DefaultDenyNetwork = false, false
	prospective.TemplateDatabase, prospective.RedisReset = false, false
	prospective.ArtifactStorage, prospective.ImmutableReports = false, false
	prospective.NoProductionSecrets, prospective.CleanupVerified, prospective.ResourceLimitsVerified = false, false, false
	prospective.ResourceBombCleanupVerified, prospective.RebaselinePassed, prospective.RebaselineRoundId = false, false, nil
	config, err := json.Marshal(map[string]any{"schema": 1, "hardware_id": parent.HardwareId, "image_digest": parent.ImageDigest,
		"qualification_sha256": parent.QualificationSha256, "config_local_directory": request.ConfigLocalDirectory, "vault_local_directory": request.VaultLocalDirectory,
		"config_local_sha256": strings.Repeat("d", 64), "vault_local_sha256": strings.Repeat("e", 64),
		"retained_unknown_metadata": map[string]any{"preserve": true}})
	if err != nil {
		t.Fatal(err)
	}
	return request, artifacts, config, parent, prospective
}

// An alternate host document must not modify its incumbent or carry the old
// image's fingerprint forward as if it described the newly assembled image.
func TestSourceProspectiveHostConfigPreservesFieldsWithoutChangingIncumbent(t *testing.T) {
	request, artifacts, content, _, _ := sourceHostProfileFixture(t)
	before := bytes.Clone(content)
	config, err := prospectiveSourceHostConfig(content, request, artifacts)
	if err != nil {
		t.Fatal(err)
	}
	var old, changed map[string]any
	if json.Unmarshal(content, &old) != nil || json.Unmarshal(config.Content, &changed) != nil || !bytes.Equal(before, content) ||
		changed["image_digest"] != artifacts.ImageId || changed["qualification_sha256"] != "" {
		t.Fatal("prospective configuration mutated incumbent or retained a false image fingerprint")
	}
	delete(old, "image_digest")
	delete(old, "qualification_sha256")
	delete(changed, "image_digest")
	delete(changed, "qualification_sha256")
	if !reflect.DeepEqual(old, changed) {
		t.Fatal("prospective configuration dropped or modified unrelated root facts")
	}
}

// A complete physical fingerprint still cannot attest another image's reset,
// cleanup, storage or readiness facts before the fenced runtime probe runs.
func TestSourceProspectiveHostFingerprintCannotClaimRuntimeQualification(t *testing.T) {
	request, artifacts, content, parent, prospective := sourceHostProfileFixture(t)
	config, err := prospectiveSourceHostConfig(content, request, artifacts)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := prospectiveSourceHostProfile(request, artifacts, config, parent, prospective, "synthetic-boot", "synthetic-boot")
	if err != nil || profile == nil || profile.RuntimeQualified || profile.ImageId != artifacts.ImageId || profile.BaseImageId != parent.ImageDigest ||
		profile.QualificationSha256 != prospective.QualificationSha256 || profile.Prospective.ArtifactStorage || profile.Prospective.RebaselinePassed ||
		!validSha256(profile.ProspectiveConfigSha256) || profile.ConfigLocalSha256 != config.ConfigLocalSha256 || profile.VaultLocalSha256 != config.VaultLocalSha256 ||
		!reflect.DeepEqual(profile.Prospective, prospective) {
		t.Fatal("hardware fingerprint fabricated runtime readiness or changed the checker observations", err)
	}
}

// An unrelated incumbent image or changed local-leaf identity cannot seed the
// new image's expected hardware profile, even when the remaining fields exist.
func TestSourceProspectiveHostConfigRejectsWrongImageAndLocalLeafIdentity(t *testing.T) {
	request, artifacts, content, _, _ := sourceHostProfileFixture(t)
	var original map[string]any
	if err := json.Unmarshal(content, &original); err != nil {
		t.Fatal(err)
	}
	for key, wrong := range map[string]any{
		"image_digest":           "sha256:" + strings.Repeat("f", 64),
		"qualification_sha256":   "",
		"hardware_id":            "",
		"config_local_directory": "/synthetic/config/main",
		"vault_local_directory":  "/synthetic/vault/main",
		"config_local_sha256":    "",
		"vault_local_sha256":     "",
	} {
		changed := maps.Clone(original)
		changed[key] = wrong
		encoded, err := json.Marshal(changed)
		if err != nil {
			t.Fatal(err)
		}
		if profile, err := prospectiveSourceHostConfig(encoded, request, artifacts); err == nil || profile != nil {
			t.Fatal("unrelated image or changed direct-local identity accepted", key)
		}
	}
}

// Each changed control, missing evidence field or substituted image must fail
// without manufacturing a new profile from the remaining true booleans.
func TestSourceProspectiveHostFingerprintRejectsChangedOrIncompleteAuthority(t *testing.T) {
	request, artifacts, content, parent, prospective := sourceHostProfileFixture(t)
	config, err := prospectiveSourceHostConfig(content, request, artifacts)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*controller.HostSelfCheck, *controller.HostSelfCheck){
		func(p, _ *controller.HostSelfCheck) { p.ArtifactStorage = false },
		func(p, _ *controller.HostSelfCheck) { p.RebaselinePassed = false },
		func(p, _ *controller.HostSelfCheck) { p.RebaselineRoundId = nil },
		func(p, _ *controller.HostSelfCheck) { p.QualificationSha256 = strings.Repeat("f", 64) },
		func(_, p *controller.HostSelfCheck) { p.ImageDigest = parent.ImageDigest },
		func(_, p *controller.HostSelfCheck) { p.QualificationSha256 = parent.QualificationSha256 },
		func(_, p *controller.HostSelfCheck) { p.QualificationSha256 = "" },
		func(_, p *controller.HostSelfCheck) { p.HostId = "different-synthetic-host" },
		func(_, p *controller.HostSelfCheck) { p.HardwareId = "different-synthetic-hardware" },
		func(_, p *controller.HostSelfCheck) { p.KernelRelease = "different-synthetic-kernel" },
		func(_, p *controller.HostSelfCheck) { p.IrqAffinitySha256 = strings.Repeat("a", 64) },
		func(_, p *controller.HostSelfCheck) { p.LogicalCpuCount = 24 },
		func(_, p *controller.HostSelfCheck) { p.SMTDisabled = false },
		func(_, p *controller.HostSelfCheck) { p.ManagementCpuReserved = false },
		func(_, p *controller.HostSelfCheck) { delete(p.Checks, "config_secure") },
		func(_, p *controller.HostSelfCheck) { p.Checks["unknown_future_check"] = false },
	} {
		base, next := parent, prospective
		base.Checks, next.Checks = maps.Clone(parent.Checks), maps.Clone(prospective.Checks)
		mutate(&base, &next)
		if profile, err := prospectiveSourceHostProfile(request, artifacts, config, base, next, "synthetic-boot", "synthetic-boot"); err == nil || profile != nil {
			t.Fatal("changed or incomplete host authority produced a prospective fingerprint")
		}
	}
	if profile, err := prospectiveSourceHostProfile(request, artifacts, config, parent, prospective, "synthetic-boot", "different-synthetic-boot"); err == nil || profile != nil {
		t.Fatal("reboot during image qualification was ignored")
	}
}
