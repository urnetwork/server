// Custody time budgets are prospective native input, never permission to renew
// an already spent owner or to omit a committed failure from the audit trail.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

func TestWinnerHandoffGroupCustodyBudgetsArePinnedBeforeOwner(t *testing.T) {
	plan := winnerMultipassQualificationFixture(t)
	directory := t.TempDir()
	config := winnerHandoffGroupConfig{Schema: 1, Kind: "winner-review-multipass-native-qualification-config/v1", ActorId: "synthetic-custody-operator", UserApprovalSha256: winnerMultipassSha256([]byte("synthetic prospective custody permission")),
		ArchiveRoundId: server.Id{1}, ArchiveJobId: server.Id{2}, OutputDirectory: filepath.Join(directory, "group"), Release: plan.Release, Settings: plan.Settings, TimeoutSeconds: 3600, StoragePaths: []string{directory}, Spool: winnerMultipassSpoolSettings{LifecycleVersion: winnerMultipassSpoolLifecycle}}
	for _, item := range plan.Cases {
		config.ScanGroups = append(config.ScanGroups, item.Plan.ScanGroups)
	}
	if err := validateWinnerHandoffGroupConfig(config); err != nil {
		t.Fatal("synthetic native group configuration must be admitted before mutations", err)
	}
	permission := winnerHandoffGroupPermission{Schema: 1, Kind: "winner-review-multipass-qualification-permission/v1", Stage: "freeze", ActorId: config.ActorId, UserApprovalSha256: config.UserApprovalSha256,
		ConfigurationSha256: winnerHandoffGroupConfigurationSha256(config), OwnerId: winnerHandoffGroupOwnerId(config), AuthorizedAt: plan.Owner.StartedAt, SingleAttempt: true, CompleteLeafPreflightRequired: true}
	content, _ := json.Marshal(permission)
	if err := validateWinnerHandoffGroupPermission(content, "freeze", config, nil, plan.Owner.StartedAt); err != nil {
		t.Fatal("synthetic original custody permission is invalid", err)
	}
	for index, mutate := range []func(*winnerHandoffGroupConfig){
		func(value *winnerHandoffGroupConfig) { value.Settings.Custody.ProtectionTimeout = time.Minute },
		func(value *winnerHandoffGroupConfig) { value.Settings.Custody.PostCommitReplayTimeout = time.Minute },
	} {
		changed := config
		mutate(&changed)
		if winnerHandoffGroupConfigurationSha256(changed) == permission.ConfigurationSha256 || winnerHandoffGroupOwnerId(changed) != permission.OwnerId {
			t.Fatalf("custody mutation %d escaped its configuration pin or minted a replacement owner", index)
		}
		if err := validateWinnerHandoffGroupPermission(content, "freeze", changed, nil, plan.Owner.StartedAt); err == nil {
			t.Fatalf("custody mutation %d reused the original prospective permission", index)
		}
	}
	encoded, _ := json.Marshal(config)
	var original map[string]json.RawMessage
	var settings map[string]json.RawMessage
	if json.Unmarshal(encoded, &original) != nil || json.Unmarshal(original["settings"], &settings) != nil {
		t.Fatal("synthetic original configuration cannot be decoded")
	}
	delete(settings, "custody")
	original["settings"], _ = json.Marshal(settings)
	encoded, _ = json.Marshal(original)
	var missing winnerHandoffGroupConfig
	if decodeStrictJSONBytes(encoded, &missing, "synthetic absent custody policy") != nil {
		t.Fatal("synthetic missing-field mutation is not valid JSON")
	}
	if err := validateWinnerHandoffGroupConfig(missing); err == nil {
		t.Fatal("historical configuration silently inherited new custody budgets")
	}
}

func TestWinnerHandoffGroupCommittedFailureKeepsExactAuditPin(t *testing.T) {
	content := []byte(`{"kind":"synthetic-protected-reference"}`)
	pin := currentQgQualificationPin{Path: filepath.Join(t.TempDir(), "invocation-000001.retention.json"), Sha256: winnerMultipassSha256(content)}
	if err := writePrivateReviewFile(pin.Path, content); err != nil {
		t.Fatal(err)
	}
	err := winnerHandoffGroupSpoolFailure(pin, context.DeadlineExceeded)
	var failure *winnerMultipassQualificationSpoolRetentionError
	if !errors.As(err, &failure) || failure.Failure != pin || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("post-commit failure discarded its exact durable reference or underlying deadline")
	}
	if _, err := readCurrentQgQualificationPin(failure.Failure, winnerMultipassSpoolManifestBytes, true); err != nil {
		t.Fatal("preserved failure reference cannot be independently read", err)
	}
	var absent *winnerMultipassQualificationSpoolRetentionError
	err = winnerHandoffGroupSpoolFailure(currentQgQualificationPin{}, context.DeadlineExceeded)
	if errors.As(err, &absent) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("pre-commit failure invented a protected reference or lost its cause")
	}
}
