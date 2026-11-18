// Native qualification has its own one-shot owner and no candidate DB review
// authority. Prepare/freeze never invoke a provider. Capture remains gated by
// actual offline release qualification and a separate exact frozen permission.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

type winnerHandoffGroupConfig struct {
	Schema             int                                 `json:"schema"`
	Kind               string                              `json:"kind"`
	OutputDirectory    string                              `json:"output_directory"`
	ActorId            string                              `json:"actor_id"`
	UserApprovalSha256 string                              `json:"user_approval_sha256"`
	ArchiveRoundId     server.Id                           `json:"archive_round_id"`
	ArchiveJobId       server.Id                           `json:"archive_job_id"`
	Seed               currentQgQualificationPin           `json:"seed"`
	RepositoriesRoot   string                              `json:"repositories_root"`
	Release            winnerMultipassQualificationRelease `json:"release"`
	Settings           winnerMultipassSettings             `json:"settings"`
	ScanGroups         [][]winnerMultipassScanGroup        `json:"scan_groups"`
	TimeoutSeconds     int                                 `json:"timeout_seconds"`
	Capacity           winnerMultipassCapacitySettings     `json:"capacity"`
	Spool              winnerMultipassSpoolSettings        `json:"spool"`
	StoragePaths       []string                            `json:"storage_paths"`
	FreezePermission   currentQgQualificationPin           `json:"freeze_permission"`
	CapturePermission  currentQgQualificationPin           `json:"capture_permission"`
	Frozen             currentQgQualificationPin           `json:"frozen"`
	Report             currentQgQualificationPin           `json:"report"`
}

// All serialized authority booleans must be present and false except the
// explicit one-shot/complete-preflight requirements. No winner verdict exists.
type winnerHandoffGroupPermission struct {
	Schema                        int       `json:"schema"`
	Kind                          string    `json:"kind"`
	Stage                         string    `json:"stage"`
	ActorId                       string    `json:"actor_id"`
	UserApprovalSha256            string    `json:"user_approval_sha256"`
	ConfigurationSha256           string    `json:"configuration_sha256"`
	OwnerId                       string    `json:"owner_id"`
	PlanSha256                    string    `json:"plan_sha256"`
	AggregateCapacitySha256       string    `json:"aggregate_capacity_sha256"`
	SpoolCapacitySha256           string    `json:"spool_capacity_sha256"`
	AuthorizedAt                  time.Time `json:"authorized_at"`
	SingleAttempt                 bool      `json:"single_attempt"`
	CompleteLeafPreflightRequired bool      `json:"complete_leaf_preflight_required"`
	WinnerApproval                bool      `json:"winner_approval"`
	SourcePromotion               bool      `json:"source_promotion"`
	RewardRelease                 bool      `json:"reward_release"`
}

type winnerHandoffGroupFrozen struct {
	Schema                 int                                   `json:"schema"`
	Kind                   string                                `json:"kind"`
	ConfigurationSha256    string                                `json:"configuration_sha256"`
	FreezePermissionSha256 string                                `json:"freeze_permission_sha256"`
	Plan                   currentQgQualificationPin             `json:"plan"`
	Owner                  winnerMultipassOwner                  `json:"owner"`
	Capacity               *winnerMultipassQualificationCapacity `json:"full_local_aggregate_capacity"`
	Spool                  *winnerMultipassSpoolCapacity         `json:"protected_spool_capacity"`
	OwnerClaim             currentQgQualificationPin             `json:"owner_claim"`
}

// Permission/report paths do not enter their own authority hash. All actual
// source, release, layout, timeout, archive and resource choices do enter it.
func winnerHandoffGroupConfigurationSha256(config winnerHandoffGroupConfig) string {
	config.FreezePermission, config.CapturePermission, config.Frozen, config.Report = currentQgQualificationPin{}, currentQgQualificationPin{}, currentQgQualificationPin{}, currentQgQualificationPin{}
	content, _ := json.Marshal(config)
	return winnerMultipassSha256(content)
}

// A new permission timestamp/output path cannot create a second owner for the
// same approved source preparation and exact release. This namespace is durable.
func winnerHandoffGroupOwnerId(config winnerHandoffGroupConfig) string {
	release, _ := json.Marshal(config.Release)
	content, _ := json.Marshal(struct{ Approval, Preparation, Release string }{Approval: config.UserApprovalSha256, Preparation: config.Release.Preparation.Sha256, Release: winnerMultipassSha256(release)})
	return winnerMultipassSha256(content)
}

func validateWinnerHandoffGroupPermission(content []byte, stage string, config winnerHandoffGroupConfig, frozen *winnerHandoffGroupFrozen, now time.Time) error {
	var permission winnerHandoffGroupPermission
	if rejectWinnerReviewCaseAliases(content) != nil || decodeStrictJSONBytes(content, &permission, "bounded qualification permission") != nil {
		return errors.New("qualification permission is malformed")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(content, &fields) != nil {
		return errors.New("qualification permission fields are malformed")
	}
	for _, name := range []string{"single_attempt", "complete_leaf_preflight_required", "winner_approval", "source_promotion", "reward_release"} {
		if value, found := fields[name]; !found || string(value) == "null" {
			return errors.New("qualification permission omitted an explicit authority boundary")
		}
	}
	if permission.Schema != 1 || permission.Kind != "winner-review-multipass-qualification-permission/v1" || permission.Stage != stage || (stage != "freeze" && stage != "capture") || permission.ActorId != config.ActorId || permission.UserApprovalSha256 != config.UserApprovalSha256 ||
		permission.ConfigurationSha256 != winnerHandoffGroupConfigurationSha256(config) || permission.OwnerId != winnerHandoffGroupOwnerId(config) || permission.AuthorizedAt.IsZero() || permission.AuthorizedAt.After(now) || !permission.SingleAttempt || !permission.CompleteLeafPreflightRequired || permission.WinnerApproval || permission.SourcePromotion || permission.RewardRelease {
		return errors.New("qualification permission changed fixed scope, one-shot ownership or forbidden authority")
	}
	if stage == "freeze" {
		if frozen != nil || permission.PlanSha256 != "" || permission.AggregateCapacitySha256 != "" || permission.SpoolCapacitySha256 != "" {
			return errors.New("prospective freeze permission invents completed frozen evidence")
		}
		return nil
	}
	if frozen == nil {
		return errors.New("capture permission has no existing frozen owner")
	}
	aggregate, _ := json.Marshal(frozen.Capacity)
	spool, _ := json.Marshal(frozen.Spool)
	if permission.PlanSha256 != frozen.Plan.Sha256 || permission.AggregateCapacitySha256 != winnerMultipassSha256(aggregate) || permission.SpoolCapacitySha256 != winnerMultipassSha256(spool) || permission.AuthorizedAt.Before(frozen.Owner.StartedAt) || !permission.AuthorizedAt.Before(frozen.Owner.Deadline) {
		return errors.New("qualification capture permission differs from exact frozen plan/capacity/clock")
	}
	return nil
}

func validateWinnerHandoffGroupConfig(config winnerHandoffGroupConfig) error {
	if config.Schema != 1 || config.Kind != "winner-review-multipass-native-qualification-config/v1" || config.ActorId == "" || len(config.ActorId) > 128 || !validSha256(config.UserApprovalSha256) ||
		config.ArchiveRoundId == (server.Id{}) || config.ArchiveJobId == (server.Id{}) || !filepath.IsAbs(config.OutputDirectory) || filepath.Clean(config.OutputDirectory) != config.OutputDirectory ||
		config.TimeoutSeconds < 60 || config.TimeoutSeconds > int(config.Settings.MaximumOwnerDuration/time.Second) || validateWinnerMultipassSettings(config.Settings) != nil ||
		!reflect.DeepEqual(config.Settings.TokenizationProfiles, config.Release.TokenizationProfiles) || len(config.ScanGroups) != 19 || len(config.StoragePaths) == 0 || config.Spool.LifecycleVersion != winnerMultipassSpoolLifecycle {
		return errors.New("qualification configuration lacks a complete bounded exact source/release/layout/resource scope")
	}
	return validateWinnerMultipassQualificationReleaseShape(config.Release)
}

// Exclusive root claim is committed before materializing any usable plan. If a
// later write fails, that owner remains spent/inspectable rather than renewed.
func freezeWinnerHandoffGroup(ctx context.Context, config winnerHandoffGroupConfig) (*winnerHandoffGroupFrozen, error) {
	if err := validateWinnerHandoffGroupConfig(config); err != nil {
		return nil, err
	}
	if err := authenticateWinnerMultipassReleaseArtifacts(config.Release); err != nil {
		return nil, err
	}
	permission, err := readCurrentQgQualificationPin(config.FreezePermission, winnerMultipassSpoolManifestBytes, true)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if err := validateWinnerHandoffGroupPermission(permission, "freeze", config, nil, now); err != nil {
		return nil, err
	}
	inputs, err := authenticateWinnerMultipassQualificationInputs(ctx, config.Release)
	if err != nil {
		return nil, err
	}
	owner := winnerMultipassOwner{Id: winnerHandoffGroupOwnerId(config), StartedAt: now, Deadline: now.Add(time.Duration(config.TimeoutSeconds) * time.Second)}
	plan, err := prepareWinnerMultipassQualification(inputs.Requests, owner, config.Settings, config.Release, config.ScanGroups)
	if err != nil {
		return nil, err
	}
	if err := validateWinnerMultipassQualificationOriginalInputs(plan, inputs); err != nil {
		return nil, err
	}
	spool, aggregate, err := estimateWinnerMultipassQualificationSpoolCapacity(plan, config.Capacity, config.Spool)
	if err != nil {
		return nil, err
	}
	root := "/var/lib/urnetwork/competition-review-multipass/qualification-owners"
	if err := currentQgQualificationPrivateRoot(root); err != nil {
		return nil, err
	}
	if err := currentQgQualificationPrivateRoot(filepath.Dir(config.OutputDirectory)); err != nil {
		return nil, err
	}
	planBytes, _ := json.Marshal(plan)
	claim := struct {
		Schema              int                  `json:"schema"`
		Kind                string               `json:"kind"`
		ConfigurationSha256 string               `json:"configuration_sha256"`
		PermissionSha256    string               `json:"permission_sha256"`
		Owner               winnerMultipassOwner `json:"owner"`
		PlanSha256          string               `json:"plan_sha256"`
	}{Schema: 1, Kind: "winner-review-qualification-owner/v1", ConfigurationSha256: winnerHandoffGroupConfigurationSha256(config), PermissionSha256: config.FreezePermission.Sha256, Owner: owner, PlanSha256: winnerMultipassSha256(planBytes)}
	claimBytes, _ := json.Marshal(claim)
	claimPath := filepath.Join(root, owner.Id+".json")
	if err := writePrivateReviewFile(claimPath, claimBytes); err != nil {
		return nil, errors.New("qualification owner already exists or cannot be exclusively persisted; no renewal permitted")
	}
	if err := syncWinnerHandoffSpoolDirectory(root); err != nil {
		return nil, err
	}
	if err := os.Mkdir(config.OutputDirectory, 0700); err != nil {
		return nil, err
	}
	planPath := filepath.Join(config.OutputDirectory, "group-plan.json")
	if err := writePrivateReviewFile(planPath, planBytes); err != nil {
		return nil, err
	}
	frozen := &winnerHandoffGroupFrozen{Schema: 1, Kind: "winner-review-frozen-qualification-owner/v1", ConfigurationSha256: claim.ConfigurationSha256, FreezePermissionSha256: config.FreezePermission.Sha256, Plan: currentQgQualificationPin{Path: planPath, Sha256: claim.PlanSha256}, Owner: owner, Capacity: aggregate, Spool: spool, OwnerClaim: currentQgQualificationPin{Path: claimPath, Sha256: winnerMultipassSha256(claimBytes)}}
	content, _ := json.Marshal(frozen)
	if err := writePrivateReviewFile(filepath.Join(config.OutputDirectory, "frozen-group.json"), content); err != nil {
		return nil, err
	}
	if err := syncWinnerHandoffSpoolDirectory(config.OutputDirectory); err != nil {
		return nil, err
	}
	return frozen, nil
}

// Opt-in command never imports a winner review or changes a source/reward hold.
// A default ordinary test return is not counted as positive workflow execution.
func TestWinnerMultipassNativeQualificationCommand(t *testing.T) {
	operation := os.Getenv("SIM_LATENCY_MULTIPASS_QUALIFICATION_COMMAND")
	if operation == "" {
		return
	}
	if operation != "prepare" && operation != "freeze" && operation != "capture" && operation != "validate" {
		t.Fatal("qualification command accepts prepare/freeze/capture/validate only")
	}
	if err := validateCurrentQgQualificationRuntime(); err != nil {
		t.Fatal(err)
	}
	content, err := readCurrentQgQualificationPin(currentQgQualificationPin{Path: os.Getenv("SIM_LATENCY_MULTIPASS_QUALIFICATION_CONFIG"), Sha256: os.Getenv("SIM_LATENCY_MULTIPASS_QUALIFICATION_CONFIG_SHA256")}, 8*int64(defaultWinnerMultipassSettings().MaximumBundleBytes), true)
	if err != nil {
		t.Fatal(err)
	}
	var config winnerHandoffGroupConfig
	if err := decodeStrictJSONBytes(content, &config, "native multi-pass qualification configuration"); err != nil {
		t.Fatal(err)
	}
	// Read-only scope inspection emits digest metadata, never an approval record.
	// It helps the operator construct a separately reviewed typed permission with
	// actual immutable identities rather than hand-reimplement canonical hashing.
	if os.Getenv("SIM_LATENCY_MULTIPASS_QUALIFICATION_DESCRIBE") == "1" {
		if err := validateWinnerHandoffGroupConfig(config); err != nil {
			t.Fatal(err)
		}
		if _, err := authenticateWinnerMultipassQualificationInputs(t.Context(), config.Release); err != nil {
			t.Fatal(err)
		}
		t.Logf("scope only; configuration_sha256=%s owner_id=%s; no authority, owner or provider call", winnerHandoffGroupConfigurationSha256(config), winnerHandoffGroupOwnerId(config))
		if config.Frozen.Path != "" {
			frozen, _, err := loadWinnerHandoffFrozenGroup(t.Context(), config)
			if err != nil {
				t.Fatal(err)
			}
			aggregate, _ := json.Marshal(frozen.Capacity)
			spool, _ := json.Marshal(frozen.Spool)
			t.Logf("existing frozen scope; plan_sha256=%s aggregate_capacity_sha256=%s spool_capacity_sha256=%s", frozen.Plan.Sha256, winnerMultipassSha256(aggregate), winnerMultipassSha256(spool))
		}
		return
	}
	if operation == "prepare" {
		_, pin, err := prepareWinnerMultipassQualificationInputFiles(t.Context(), config.Seed, config.RepositoriesRoot, config.Release.NativeFixtureEvidence, config.OutputDirectory)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("authentic nineteen-control preparation retained sha256=%s; no provider call or owner created", pin.Sha256)
		return
	}
	if err := validateWinnerHandoffGroupConfig(config); err != nil {
		t.Fatal(err)
	}
	if operation == "freeze" {
		if os.Getenv("SIM_LATENCY_MULTIPASS_QUALIFICATION_MUTATION_OPT_IN") != "1" {
			t.Fatal("qualification owner requires explicit freeze opt-in")
		}
		frozen, err := freezeWinnerHandoffGroup(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("one-shot group owner %s frozen at %s; no provider calls; separate frozen capture permission required", frozen.Owner.Id, frozen.Plan.Sha256)
		return
	}
	if operation == "capture" {
		if os.Getenv("SIM_LATENCY_MULTIPASS_QUALIFICATION_MODEL_OPT_IN") != "1" || os.Getenv("SIM_LATENCY_MULTIPASS_QUALIFICATION_SNOW_OPT_IN") != "1" {
			t.Fatal("genuine qualification requires separate model and Snow opt-ins")
		}
		if err := captureWinnerHandoffGroup(t.Context(), config); err != nil {
			t.Fatal("qualification stopped with originals preserved; no retry/renewal permission: ", err)
		}
		return
	}
	control, _, err := loadCompetitionControl()
	if err != nil {
		t.Fatal(err)
	}
	if err := authenticateWinnerMultipassReleaseArtifacts(config.Release); err != nil {
		t.Fatal(err)
	}
	if err := authenticateWinnerHandoffQualification(t.Context(), config.Report, config.Release, true, control, winnerHandoffGroupArchiveArgs(config)); err != nil {
		t.Fatal(err)
	}
	t.Log("genuine nineteen-control protected qualification replayed; no winner approval or candidate inference performed")
}

func TestWinnerHandoffGroupPermissionCannotRenewOrConferWinnerAuthority(t *testing.T) {
	plan := winnerMultipassQualificationFixture(t)
	config := winnerHandoffGroupConfig{ActorId: "synthetic-qualification", UserApprovalSha256: winnerMultipassSha256([]byte("synthetic choice")), Release: plan.Release, Settings: plan.Settings, TimeoutSeconds: 3600, StoragePaths: []string{"/synthetic/state"}}
	permission := winnerHandoffGroupPermission{Schema: 1, Kind: "winner-review-multipass-qualification-permission/v1", Stage: "freeze", ActorId: config.ActorId, UserApprovalSha256: config.UserApprovalSha256, ConfigurationSha256: winnerHandoffGroupConfigurationSha256(config), OwnerId: winnerHandoffGroupOwnerId(config), AuthorizedAt: plan.Owner.StartedAt, SingleAttempt: true, CompleteLeafPreflightRequired: true}
	content, _ := json.Marshal(permission)
	if err := validateWinnerHandoffGroupPermission(content, "freeze", config, nil, plan.Owner.StartedAt); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*winnerHandoffGroupPermission){func(p *winnerHandoffGroupPermission) { p.WinnerApproval = true }, func(p *winnerHandoffGroupPermission) { p.SourcePromotion = true }, func(p *winnerHandoffGroupPermission) { p.RewardRelease = true }, func(p *winnerHandoffGroupPermission) { p.SingleAttempt = false }, func(p *winnerHandoffGroupPermission) { p.PlanSha256 = fmt.Sprintf("%064d", 1) }} {
		changed := permission
		mutate(&changed)
		content, _ := json.Marshal(changed)
		if err := validateWinnerHandoffGroupPermission(content, "freeze", config, nil, plan.Owner.StartedAt); err == nil {
			t.Fatal("qualification permission crossed authority/frozen-evidence boundary")
		}
	}
	oldOwner := winnerHandoffGroupOwnerId(config)
	config.OutputDirectory = "/synthetic/another-output"
	if winnerHandoffGroupOwnerId(config) != oldOwner {
		t.Fatal("new output path minted another owner for the same approved preparation/release")
	}
}
