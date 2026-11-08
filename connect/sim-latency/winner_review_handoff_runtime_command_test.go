// Runtime bootstrap has exactly two synthetic non-inference calls. Permanent
// root intents survive every failure; no partial result admits a normal review.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
)

type winnerHandoffRuntimeSettings struct {
	MaximumDuration     time.Duration `json:"maximum_duration"`      // Positive total owner bound; default two five-minute probes.
	MaximumDurableBytes int64         `json:"maximum_durable_bytes"` // Positive full-local bound; default 512 MiB includes originals and staging.
	MaximumMemoryBytes  int64         `json:"maximum_memory_bytes"`  // Positive native working allowance; default 512 MiB, independently reserved.
	ReceiptTimeout      time.Duration `json:"receipt_timeout"`       // Positive protected-custody bound after invocation joins; default native two minutes.
}

func defaultWinnerHandoffRuntimeSettings() winnerHandoffRuntimeSettings {
	return winnerHandoffRuntimeSettings{MaximumDuration: 2 * defaultWinnerMultipassProviderLimits().TokenizerTimeout, MaximumDurableBytes: 512 * 1024 * 1024, MaximumMemoryBytes: 512 * 1024 * 1024, ReceiptTimeout: defaultWinnerHandoffCaptureSettings().ReceiptTimeout}
}

type winnerHandoffRuntimeConfig struct {
	Schema             int                                 `json:"schema"`
	Kind               string                              `json:"kind"`
	OutputDirectory    string                              `json:"output_directory"`
	ActorId            string                              `json:"actor_id"`
	UserApprovalSha256 string                              `json:"user_approval_sha256"`
	ArchiveRoundId     server.Id                           `json:"archive_round_id"`
	ArchiveJobId       server.Id                           `json:"archive_job_id"`
	Release            winnerMultipassQualificationRelease `json:"release"`
	Settings           winnerHandoffRuntimeSettings        `json:"settings"`
	StoragePaths       []string                            `json:"storage_paths"`
	Permission         currentQgQualificationPin           `json:"permission"`
}

type winnerHandoffRuntimePermission struct {
	Schema                int       `json:"schema"`
	Kind                  string    `json:"kind"`
	ActorId               string    `json:"actor_id"`
	UserApprovalSha256    string    `json:"user_approval_sha256"`
	ConfigurationSha256   string    `json:"configuration_sha256"`
	OwnerId               string    `json:"owner_id"`
	AuthorizedAt          time.Time `json:"authorized_at"`
	SingleAttempt         bool      `json:"single_attempt"`
	AllowNonInferenceLoad bool      `json:"allow_non_inference_load"`
	Inference             bool      `json:"inference"`
	WinnerApproval        bool      `json:"winner_approval"`
	SourcePromotion       bool      `json:"source_promotion"`
	RewardRelease         bool      `json:"reward_release"`
}

type winnerHandoffRuntimeOwnerClaim struct {
	Schema              int                  `json:"schema"`
	Kind                string               `json:"kind"`
	ConfigurationSha256 string               `json:"configuration_sha256"`
	PermissionSha256    string               `json:"permission_sha256"`
	UserApprovalSha256  string               `json:"user_approval_sha256"`
	Owner               winnerMultipassOwner `json:"owner"`
	ReleaseSha256       string               `json:"release_sha256"`
}

type winnerHandoffRuntimeCallClaim struct {
	Schema      int                  `json:"schema"`
	Kind        string               `json:"kind"`
	Owner       winnerMultipassOwner `json:"owner"`
	Member      winnerSnowMember     `json:"member"`
	InputSha256 string               `json:"input_sha256"`
	StartedAt   time.Time            `json:"started_at"`
}

// Neither a new permission timestamp nor another output path changes this
// one-shot release owner. Observation hashes are empty until both calls finish.
func winnerHandoffRuntimeOwnerId(config winnerHandoffRuntimeConfig) string {
	release, _ := json.Marshal(config.Release)
	content, _ := json.Marshal(struct{ Approval, Release string }{Approval: config.UserApprovalSha256, Release: winnerMultipassSha256(release)})
	return winnerMultipassSha256(content)
}

func winnerHandoffRuntimeConfigurationSha256(config winnerHandoffRuntimeConfig) string {
	config.Permission = currentQgQualificationPin{}
	content, _ := json.Marshal(config)
	return winnerMultipassSha256(content)
}

func validateWinnerHandoffRuntimeConfig(config winnerHandoffRuntimeConfig) error {
	defaults := defaultWinnerHandoffRuntimeSettings()
	if config.Schema != 1 || config.Kind != "winner-review-multipass-runtime-bootstrap-config/v1" || config.ActorId == "" || len(config.ActorId) > 128 || !validSha256(config.UserApprovalSha256) ||
		config.ArchiveRoundId == (server.Id{}) || config.ArchiveJobId == (server.Id{}) || !filepath.IsAbs(config.OutputDirectory) || filepath.Clean(config.OutputDirectory) != config.OutputDirectory || len(config.StoragePaths) == 0 ||
		config.Settings.MaximumDuration <= 0 || config.Settings.MaximumDuration > defaults.MaximumDuration || config.Settings.MaximumDurableBytes != defaults.MaximumDurableBytes || config.Settings.MaximumMemoryBytes != defaults.MaximumMemoryBytes ||
		config.Settings.ReceiptTimeout <= 0 || config.Settings.ReceiptTimeout > defaults.ReceiptTimeout {
		return errors.New("runtime bootstrap lacks its exact two-probe bounded release and resource scope")
	}
	return validateWinnerMultipassRuntimeProbeReleaseShape(config.Release)
}

func validateWinnerHandoffRuntimePermission(content []byte, config winnerHandoffRuntimeConfig, now time.Time) error {
	var permission winnerHandoffRuntimePermission
	var fields map[string]json.RawMessage
	if rejectWinnerReviewCaseAliases(content) != nil || decodeStrictJSONBytes(content, &permission, "runtime bootstrap permission") != nil || json.Unmarshal(content, &fields) != nil {
		return errors.New("runtime permission is malformed")
	}
	for _, name := range []string{"single_attempt", "allow_non_inference_load", "inference", "winner_approval", "source_promotion", "reward_release"} {
		if value, found := fields[name]; !found || string(value) == "null" {
			return errors.New("runtime permission omitted an explicit authority boundary")
		}
	}
	if permission.Schema != 1 || permission.Kind != "winner-review-multipass-runtime-bootstrap-permission/v1" || permission.ActorId != config.ActorId || permission.UserApprovalSha256 != config.UserApprovalSha256 ||
		permission.ConfigurationSha256 != winnerHandoffRuntimeConfigurationSha256(config) || permission.OwnerId != winnerHandoffRuntimeOwnerId(config) || permission.AuthorizedAt.IsZero() || permission.AuthorizedAt.After(now) ||
		!permission.SingleAttempt || !permission.AllowNonInferenceLoad || permission.Inference || permission.WinnerApproval || permission.SourcePromotion || permission.RewardRelease {
		return errors.New("runtime permission changed fixed non-inference scope or forbidden authority")
	}
	return nil
}

// This tiny sequential owner is shared by the actual root command and the
// deterministic offline orchestration controls. Hooks do not qualify a provider.
type winnerHandoffRuntimeHooks struct {
	Now     func() time.Time
	Claim   func(winnerMultipassBrokerEnvelope, []byte, time.Time) (currentQgQualificationPin, error)
	Invoke  func(context.Context, []byte) ([]byte, []byte, error)
	Capture func(winnerMultipassBrokerEnvelope, currentQgQualificationPin, []byte, []byte, []byte, error) (currentQgQualificationPin, error)
}

func executeWinnerHandoffRuntimeProbes(ctx context.Context, owner winnerMultipassOwner, hooks winnerHandoffRuntimeHooks) ([]currentQgQualificationPin, error) {
	captures := []currentQgQualificationPin{}
	if hooks.Now == nil || hooks.Claim == nil || hooks.Invoke == nil || hooks.Capture == nil {
		return captures, errors.New("runtime bootstrap omitted a required ownership/capture boundary")
	}
	for _, member := range defaultWinnerSnowProviderSettings().Members {
		now := hooks.Now()
		deadline := now.Add(defaultWinnerMultipassProviderLimits().TokenizerTimeout)
		if owner.Deadline.Before(deadline) {
			deadline = owner.Deadline
		}
		envelope, err := buildWinnerMultipassRuntimeProbe(owner, member, deadline)
		if err != nil || validateWinnerMultipassRuntimeProbeEnvelope(envelope, now, defaultWinnerMultipassProviderLimits()) != nil || ctx.Err() != nil {
			return captures, errors.New("runtime bootstrap owner expired or lost its exact synthetic call")
		}
		input, _ := json.Marshal(envelope)
		claim, err := hooks.Claim(envelope, input, now)
		if err != nil {
			return captures, err
		}
		bounded, cancel := context.WithDeadline(ctx, deadline)
		output, diagnostics, invokeErr := hooks.Invoke(bounded, input)
		cancel()
		pin, captureErr := hooks.Capture(envelope, claim, input, output, diagnostics, invokeErr)
		if pin.Path != "" {
			captures = append(captures, pin)
		}
		if err := errors.Join(invokeErr, captureErr); err != nil {
			return captures, err
		}
		if !hooks.Now().Before(owner.Deadline) {
			return captures, errors.New("runtime bootstrap completed outside its fixed original owner")
		}
	}
	return captures, nil
}

// Every write is exclusive and fsynced before input release. No cleanup code
// targets this permanent owner/claim namespace, even after an uncertain call.
func writeWinnerHandoffRuntimeIntent(path string, value any) (currentQgQualificationPin, error) {
	content, err := json.Marshal(value)
	if err == nil {
		err = writePrivateReviewFile(path, content)
	}
	if err == nil {
		err = syncWinnerHandoffSpoolDirectory(filepath.Dir(path))
	}
	return currentQgQualificationPin{Path: path, Sha256: winnerMultipassSha256(content)}, err
}

// This command never executes candidate text, requests generation or issues a
// review verdict. Failed wrapper attempts remain local and protected when possible.
func captureWinnerHandoffRuntime(ctx context.Context, config winnerHandoffRuntimeConfig) (resultErr error) {
	if err := validateWinnerHandoffRuntimeConfig(config); err != nil {
		return err
	}
	if err := authenticateWinnerMultipassRuntimeProbeArtifacts(config.Release); err != nil {
		return err
	}
	permission, err := readCurrentQgQualificationPin(config.Permission, winnerMultipassSpoolManifestBytes, true)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if err := validateWinnerHandoffRuntimePermission(permission, config, now); err != nil {
		return err
	}
	root := "/var/lib/urnetwork/competition-review-multipass/qualification-owners"
	profile := winnerMultipassContainmentProfile{ImageId: config.Release.ImageId, ProviderSettings: config.Release.ProviderSettings, StateRoot: "/var/lib/urnetwork/competition-review-multipass/attempts"}
	for _, directory := range []string{root, profile.StateRoot, filepath.Dir(config.OutputDirectory)} {
		if err := currentQgQualificationPrivateRoot(directory); err != nil {
			return err
		}
	}
	for _, required := range []string{config.OutputDirectory, os.TempDir(), profile.StateRoot, filepath.Dir(profile.StateRoot)} {
		if !slices.Contains(config.StoragePaths, required) {
			return errors.New("runtime bootstrap omitted an actual storage filesystem")
		}
	}
	control, _, err := loadCompetitionControl()
	if err != nil {
		return err
	}
	retain := func(ctx context.Context, content []byte, digest string) (*controller.CompetitionWinnerReviewEvidence, error) {
		return controller.RetainCompetitionReviewHandoffEvidence(ctx, control, config.ArchiveRoundId, config.ArchiveJobId, content, digest)
	}
	owner := winnerMultipassOwner{Id: winnerHandoffRuntimeOwnerId(config), StartedAt: now, Deadline: now.Add(config.Settings.MaximumDuration)}
	releaseBytes, _ := json.Marshal(config.Release)
	ownerClaim, err := writeWinnerHandoffRuntimeIntent(filepath.Join(root, owner.Id+".runtime.json"), winnerHandoffRuntimeOwnerClaim{Schema: 1, Kind: "winner-review-multipass-runtime-owner/v1", ConfigurationSha256: winnerHandoffRuntimeConfigurationSha256(config), PermissionSha256: config.Permission.Sha256, UserApprovalSha256: config.UserApprovalSha256, Owner: owner, ReleaseSha256: winnerMultipassSha256(releaseBytes)})
	if err != nil {
		return errors.New("runtime owner already exists or cannot be persisted; no retry or renewal permitted")
	}
	if err := os.Mkdir(config.OutputDirectory, 0700); err != nil {
		return err
	}
	memoryCgroup, memoryLimit, memoryCurrent, memoryAvailable, err := readWinnerHandoffMemory()
	if err != nil || memoryLimit > defaultWinnerMultipassCapacitySettings().MaximumMemoryBytes || memoryCurrent < 0 || memoryLimit-memoryCurrent < config.Settings.MaximumMemoryBytes || memoryAvailable < config.Settings.MaximumMemoryBytes {
		return errors.New("runtime bootstrap lacks bounded native memory headroom")
	}
	storage, err := allocateWinnerHandoffStorage(config.OutputDirectory, config.StoragePaths, config.Settings.MaximumDurableBytes)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, storage.Close()) }()
	// The entire two-call allowance is released to this exclusive bounded owner.
	// Originals are not evicted; the native capture and wrapper enforce every leaf cap.
	if err := storage.Consume(config.Settings.MaximumDurableBytes); err != nil {
		return err
	}
	claimBytes, err := readCurrentQgQualificationPin(ownerClaim, winnerMultipassSpoolManifestBytes, true)
	if err != nil {
		return err
	}
	capacity := struct {
		ReservedBytes   int64  `json:"reserved_bytes"`
		Device          uint64 `json:"device"`
		Inode           uint64 `json:"inode"`
		MemoryCgroup    string `json:"memory_cgroup"`
		MemoryLimit     int64  `json:"memory_limit_bytes"`
		MemoryCurrent   int64  `json:"memory_current_bytes"`
		MemoryAvailable int64  `json:"memory_available_bytes"`
	}{ReservedBytes: storage.allocated, Device: storage.device, Inode: storage.inode, MemoryCgroup: memoryCgroup, MemoryLimit: memoryLimit, MemoryCurrent: memoryCurrent, MemoryAvailable: memoryAvailable}
	for _, document := range []struct {
		name  string
		value any
	}{{name: "bootstrap-config.json", value: config}, {name: "bootstrap-permission.json", value: json.RawMessage(permission)}, {name: "runtime-owner.json", value: json.RawMessage(claimBytes)}, {name: "runtime-capacity.json", value: capacity}} {
		content, _ := json.Marshal(document.value)
		if err := writePrivateReviewFile(filepath.Join(config.OutputDirectory, document.name), content); err != nil {
			return err
		}
		if _, err := retainWinnerHandoffDocument(ctx, document.name, content, winnerMultipassSpoolCaptureBytes, owner.Deadline, retain); err != nil {
			return err
		}
	}
	hooks := winnerHandoffRuntimeHooks{Now: func() time.Time { return time.Now().UTC() },
		Claim: func(envelope winnerMultipassBrokerEnvelope, input []byte, startedAt time.Time) (currentQgQualificationPin, error) {
			claim := winnerHandoffRuntimeCallClaim{Schema: 1, Kind: "winner-review-multipass-runtime-call/v1", Owner: owner, Member: envelope.Call.Member, InputSha256: winnerMultipassSha256(input), StartedAt: startedAt}
			pin, err := writeWinnerHandoffRuntimeIntent(filepath.Join(root, owner.Id+"."+envelope.Call.Member.Name+".runtime.json"), claim)
			if err != nil {
				return pin, err
			}
			content, _ := json.Marshal(claim)
			_, err = retainWinnerHandoffDocument(ctx, "runtime-call-intent.json", content, winnerMultipassSpoolManifestBytes, owner.Deadline, retain)
			return pin, err
		},
		Invoke: func(callCtx context.Context, input []byte) ([]byte, []byte, error) {
			return invokeWinnerMultipassBroker(callCtx, config.Release.Wrapper, input)
		},
		Capture: func(envelope winnerMultipassBrokerEnvelope, claim currentQgQualificationPin, input, output, diagnostics []byte, invokeErr error) (currentQgQualificationPin, error) {
			retentionCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), config.Settings.ReceiptTimeout)
			defer cancel()
			physical, receipt, independent, authenticationErr := createWinnerMultipassPhysicalAuthentication(input, output, diagnostics, false, profile, config.Release)
			capture := winnerHandoffRuntimeCapture{Schema: 1, Kind: "winner-review-multipass-runtime-capture/v1", BootstrapRelease: config.Release, Owner: owner, OwnerClaim: ownerClaim, Claim: claim, Input: input, Output: output, Diagnostics: diagnostics, Containment: receipt, PhysicalAuthentication: physical}
			captureErr := errors.Join(invokeErr, authenticationErr)
			if authenticationErr == nil {
				if receipt == nil || receipt.Result == nil || receipt.Result.RuntimeObservation == nil || receipt.Result.Admission != nil || receipt.Result.Error != "" || len(independent) != 0 {
					captureErr = errors.Join(captureErr, errors.New("runtime result lacks a complete non-inference observation"))
				} else {
					var retainErr error
					capture.Custody, retainErr = retainWinnerHandoffContainment(retentionCtx, "runtime_probe", envelope, receipt, retain)
					captureErr = errors.Join(captureErr, retainErr)
				}
			}
			if captureErr != nil {
				capture.Error = captureErr.Error()
			}
			content, marshalErr := json.Marshal(capture)
			if marshalErr != nil || len(content) > winnerMultipassSpoolCaptureBytes {
				return currentQgQualificationPin{}, errors.Join(captureErr, errors.New("runtime capture exceeds its admitted host-record bound"), marshalErr)
			}
			path := filepath.Join(config.OutputDirectory, envelope.Call.Member.Name+"-runtime-capture.json")
			writeErr := writePrivateReviewFile(path, content)
			_, retainErr := retainWinnerHandoffDocument(retentionCtx, "runtime-capture.json", content, winnerMultipassSpoolCaptureBytes, owner.Deadline, retain)
			return currentQgQualificationPin{Path: path, Sha256: winnerMultipassSha256(content)}, errors.Join(captureErr, writeErr, retainErr)
		},
	}
	captures, resultErr := executeWinnerHandoffRuntimeProbes(ctx, owner, hooks)
	profiles := map[string]winnerMultipassTokenizationProfile{}
	if resultErr == nil && len(captures) == len(defaultWinnerSnowProviderSettings().Members) {
		for index, member := range defaultWinnerSnowProviderSettings().Members {
			value := config.Release.TokenizationProfiles[member.Name]
			value.RuntimeEvidenceSha256 = captures[index].Sha256
			profiles[member.Name] = value
		}
	}
	report := struct {
		Schema   int                                           `json:"schema"`
		Kind     string                                        `json:"kind"`
		Owner    winnerMultipassOwner                          `json:"owner"`
		Captures []currentQgQualificationPin                   `json:"captures"`
		Profiles map[string]winnerMultipassTokenizationProfile `json:"tokenization_profiles"`
		Complete bool                                          `json:"complete"`
		Error    string                                        `json:"error"`
	}{Schema: 1, Kind: "winner-review-multipass-runtime-bootstrap-report/v1", Owner: owner, Captures: captures, Profiles: profiles, Complete: resultErr == nil && len(profiles) == 2}
	if resultErr != nil {
		report.Error = resultErr.Error()
	}
	content, _ := json.Marshal(report)
	writeErr := writePrivateReviewFile(filepath.Join(config.OutputDirectory, "runtime-report.json"), content)
	retentionCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), config.Settings.ReceiptTimeout)
	_, retainErr := retainWinnerHandoffDocument(retentionCtx, "runtime-report.json", content, winnerMultipassSpoolManifestBytes, owner.Deadline, retain)
	cancel()
	return errors.Join(resultErr, writeErr, retainErr, syncWinnerHandoffSpoolDirectory(config.OutputDirectory))
}

// The default test path performs no operation. Actual execution requires the
// qualified root driver, exact config/permission pins and an explicit Snow opt-in.
func TestWinnerHandoffRuntimeBootstrapCommand(t *testing.T) {
	if os.Getenv("SIM_LATENCY_MULTIPASS_RUNTIME_BOOTSTRAP") != "1" {
		return
	}
	if err := validateCurrentQgQualificationRuntime(); err != nil {
		t.Fatal(err)
	}
	content, err := readCurrentQgQualificationPin(currentQgQualificationPin{Path: os.Getenv("SIM_LATENCY_MULTIPASS_RUNTIME_CONFIG"), Sha256: os.Getenv("SIM_LATENCY_MULTIPASS_RUNTIME_CONFIG_SHA256")}, winnerMultipassSpoolCaptureBytes, true)
	if err != nil {
		t.Fatal(err)
	}
	var config winnerHandoffRuntimeConfig
	if decodeStrictJSONBytes(content, &config, "native runtime bootstrap config") != nil || validateWinnerHandoffRuntimeConfig(config) != nil {
		t.Fatal("runtime bootstrap configuration is invalid")
	}
	if os.Getenv("SIM_LATENCY_MULTIPASS_RUNTIME_DESCRIBE") == "1" {
		t.Logf("non-authorizing runtime scope: configuration_sha256=%s owner_id=%s", winnerHandoffRuntimeConfigurationSha256(config), winnerHandoffRuntimeOwnerId(config))
		return
	}
	if os.Getenv("SIM_LATENCY_MULTIPASS_RUNTIME_SNOW_OPT_IN") != "1" {
		t.Fatal("runtime bootstrap needs separate non-inference Snow opt-in")
	}
	if err := captureWinnerHandoffRuntime(t.Context(), config); err != nil {
		t.Fatal("runtime bootstrap stopped without retry or generation authority: ", err)
	}
}

func TestWinnerHandoffRuntimeProbeIntentsRemainOneShotAfterUncertainty(t *testing.T) {
	plan := winnerMultipassQualificationFixture(t)
	owner := winnerMultipassOwner{Id: "synthetic-runtime-owner", StartedAt: plan.Owner.StartedAt, Deadline: plan.Owner.StartedAt.Add(defaultWinnerHandoffRuntimeSettings().MaximumDuration)}
	root, calls, captures := t.TempDir(), 0, 0
	hooks := winnerHandoffRuntimeHooks{Now: func() time.Time { return owner.StartedAt },
		Claim: func(envelope winnerMultipassBrokerEnvelope, input []byte, now time.Time) (currentQgQualificationPin, error) {
			return writeWinnerHandoffRuntimeIntent(filepath.Join(root, envelope.Call.Member.Name+".json"), winnerHandoffRuntimeCallClaim{Schema: 1, Kind: "winner-review-multipass-runtime-call/v1", Owner: owner, Member: envelope.Call.Member, InputSha256: winnerMultipassSha256(input), StartedAt: now})
		},
		Invoke: func(context.Context, []byte) ([]byte, []byte, error) {
			calls++
			return nil, nil, errors.New("synthetic uncertain transport")
		},
		Capture: func(winnerMultipassBrokerEnvelope, currentQgQualificationPin, []byte, []byte, []byte, error) (currentQgQualificationPin, error) {
			captures++
			return currentQgQualificationPin{}, nil
		},
	}
	if _, err := executeWinnerHandoffRuntimeProbes(t.Context(), owner, hooks); err == nil || calls != 1 || captures != 1 {
		t.Fatal("uncertain runtime probe continued or discarded capture", err, calls, captures)
	}
	if _, err := executeWinnerHandoffRuntimeProbes(t.Context(), owner, hooks); err == nil || calls != 1 || captures != 1 {
		t.Fatal("uncertain runtime probe was silently repeated", err, calls, captures)
	}
}

func TestWinnerHandoffRuntimeProbesCompleteBothWithoutGenerationAdmission(t *testing.T) {
	plan := winnerMultipassQualificationFixture(t)
	owner := winnerMultipassOwner{Id: "synthetic-runtime-owner", StartedAt: plan.Owner.StartedAt, Deadline: plan.Owner.StartedAt.Add(defaultWinnerHandoffRuntimeSettings().MaximumDuration)}
	steps := []string{}
	hooks := winnerHandoffRuntimeHooks{Now: func() time.Time { return owner.StartedAt },
		Claim: func(envelope winnerMultipassBrokerEnvelope, input []byte, now time.Time) (currentQgQualificationPin, error) {
			steps = append(steps, "claim:"+envelope.Call.Member.Name)
			return currentQgQualificationPin{Path: "/synthetic/claim", Sha256: winnerMultipassSha256(input)}, nil
		},
		Invoke: func(_ context.Context, input []byte) ([]byte, []byte, error) {
			var envelope winnerMultipassBrokerEnvelope
			if json.Unmarshal(input, &envelope) != nil || envelope.Operation != "tokenize" || envelope.Kind != winnerMultipassRuntimeProbeKind || envelope.Admission != nil {
				t.Fatal("bootstrap admitted inference")
			}
			steps = append(steps, "invoke:"+envelope.Call.Member.Name)
			return nil, nil, nil
		},
		Capture: func(envelope winnerMultipassBrokerEnvelope, claim currentQgQualificationPin, input, output, diagnostics []byte, err error) (currentQgQualificationPin, error) {
			steps = append(steps, "retain:"+envelope.Call.Member.Name)
			return currentQgQualificationPin{Path: "/synthetic/" + envelope.Call.Member.Name, Sha256: winnerMultipassSha256(input)}, nil
		},
	}
	captures, err := executeWinnerHandoffRuntimeProbes(t.Context(), owner, hooks)
	expected := []string{}
	for _, member := range defaultWinnerSnowProviderSettings().Members {
		expected = append(expected, "claim:"+member.Name, "invoke:"+member.Name, "retain:"+member.Name)
	}
	if err != nil || len(captures) != 2 || !reflect.DeepEqual(steps, expected) {
		t.Fatal("two-probe claim/capture ordering changed", err, steps)
	}
}

func TestWinnerHandoffRuntimePermissionCannotMintReviewAuthority(t *testing.T) {
	plan := winnerMultipassQualificationFixture(t)
	config := winnerHandoffRuntimeConfig{ActorId: "synthetic-operator", UserApprovalSha256: winnerMultipassSha256([]byte("synthetic permission")), Release: plan.Release}
	permission := winnerHandoffRuntimePermission{Schema: 1, Kind: "winner-review-multipass-runtime-bootstrap-permission/v1", ActorId: config.ActorId, UserApprovalSha256: config.UserApprovalSha256, ConfigurationSha256: winnerHandoffRuntimeConfigurationSha256(config), OwnerId: winnerHandoffRuntimeOwnerId(config), AuthorizedAt: plan.Owner.StartedAt, SingleAttempt: true, AllowNonInferenceLoad: true}
	content, _ := json.Marshal(permission)
	if err := validateWinnerHandoffRuntimePermission(content, config, plan.Owner.StartedAt); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*winnerHandoffRuntimePermission){func(p *winnerHandoffRuntimePermission) { p.Inference = true }, func(p *winnerHandoffRuntimePermission) { p.WinnerApproval = true }, func(p *winnerHandoffRuntimePermission) { p.SingleAttempt = false }} {
		changed := permission
		mutate(&changed)
		content, _ := json.Marshal(changed)
		if err := validateWinnerHandoffRuntimePermission(content, config, plan.Owner.StartedAt); err == nil {
			t.Fatal("bootstrap permission crossed non-inference boundary")
		}
	}
	old := winnerHandoffRuntimeOwnerId(config)
	config.OutputDirectory = "/synthetic/another-output"
	if winnerHandoffRuntimeOwnerId(config) != old {
		t.Fatal("another output directory renewed one-shot bootstrap owner")
	}
}
