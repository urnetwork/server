// Inactive administrative recovery for one pinned, unmeasured setup failure.
// This command never launches an evaluator, changes inputs, or removes evidence.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// These values are the frozen incident contract, not evaluation tunables.
const (
	frozenEvaluationBytes        = int64(96 * 1024 * 1024 * 1024)
	frozenEvidenceBytes          = int64(5 * 1024 * 1024 * 1024)
	frozenManagementReserveBytes = int64(24 * 1024 * 1024 * 1024)
	minimumNonEvidenceBytes      = int64(3 * 1024 * 1024 * 1024)
	freshClockLimitSeconds       = 3 * 60 * 60
)

// Bounds keep metadata inspection independent of the failed evaluator's memory.
type recoverySettings struct {
	// Administrative metadata read cap in bytes; non-positive is invalid.
	MaximumMetadataBytes int64
	// Individual opaque forensic/source cap in bytes; non-positive is invalid.
	MaximumFileBytes int64
	// Failed-tree entry census cap; non-positive is invalid.
	MaximumTreeEntries int
	// Process census cap; non-positive is invalid.
	MaximumProcessCount int
	// Total inspected process descriptors; non-positive is invalid.
	MaximumDescriptorCount int
}

// Non-positive bounds are invalid; they never disable a safety check.
func defaultRecoverySettings() recoverySettings {
	return recoverySettings{
		MaximumMetadataBytes:   8 * 1024 * 1024,
		MaximumFileBytes:       256 * 1024 * 1024,
		MaximumTreeEntries:     128,
		MaximumProcessCount:    32768,
		MaximumDescriptorCount: 131072,
	}
}

// Production authority is compiled in, rather than selected by a config file.
type recoveryPolicy struct {
	CampaignRoot     string
	SelectionRoot    string
	PublicPlanSha256 string
	Paths            pathPolicy
	ScoringLock      legacyLockPolicy
}

// Only the exact first-committed campaign is eligible for this one-use tool.
func defaultRecoveryPolicy() recoveryPolicy {
	return recoveryPolicy{
		CampaignRoot:     "/var/lib/urnetwork/competition-held-out-two-b-c2-a9e8e1a7-20261027-v2/campaign",
		SelectionRoot:    "/var/lib/urnetwork/competition-held-out-first-committed-c2-a9e8e1a7-20261027-v1",
		PublicPlanSha256: "a8a57ccfe3d5f050ec76bf475dc1e35fb25011c9b781ee1c90dfa7cca720bb9f",
		Paths:            pathPolicy{OwnerUid: 0, AnchorPath: "/"},
		ScoringLock: legacyLockPolicy{
			Path:     "/home/by/urnetwork/temp/production-frontier-20261024.63AtMXS5/scoring.lock",
			Identity: lockIdentity{OwnerUid: 1000, Device: 64512, Inode: 35309334, Mode: 0600},
			Ancestors: []lockAncestor{
				{Path: "/", OwnerUid: 0, Mode: 0755},
				{Path: "/home", OwnerUid: 0, Mode: 0755},
				{Path: "/home/by", OwnerUid: 1000, Mode: 0750},
				{Path: "/home/by/urnetwork", OwnerUid: 1000, Mode: 0775},
				{Path: "/home/by/urnetwork/temp", OwnerUid: 1000, Mode: 0775},
				{Path: "/home/by/urnetwork/temp/production-frontier-20261024.63AtMXS5", OwnerUid: 1000, Mode: 0700},
			},
		},
	}
}

// A pin authenticates bytes without interpreting private plans or requests.
type filePin struct {
	Path   string `json:"path"`
	Sha256 string `json:"sha256"`
}

// Every one of the original six jobs retains its three opaque input bytes.
type jobPins struct {
	Root   string    `json:"root"`
	Inputs []filePin `json:"inputs"`
}

// The config is independently hash-pinned on the command line and by approval.
// ProtectedPins must include both private plans/configurations, evaluation policy,
// original selection claim, executor, and all other source/authority pins in use.
type recoveryConfig struct {
	Schema                int          `json:"schema"`
	Kind                  string       `json:"kind"`
	CampaignRoot          string       `json:"campaign_root"`
	SelectionRoot         string       `json:"selection_root"`
	AuditRoot             string       `json:"audit_root"`
	PublicPlan            filePin      `json:"public_plan"`
	FailureProof          filePin      `json:"failure_proof"`
	OldGlobalStart        filePin      `json:"old_global_start"`
	OldFirstStart         filePin      `json:"old_first_start"`
	FailedTreeSha256      string       `json:"failed_tree_sha256"`
	OldDeadline           time.Time    `json:"old_deadline"`
	FirstJobIds           []string     `json:"first_job_ids"`
	Jobs                  []jobPins    `json:"jobs"`
	ProtectedPins         []filePin    `json:"protected_pins"`
	Executor              filePin      `json:"executor"`
	ScoringLock           filePin      `json:"scoring_lock"`
	ScoringLockIdentity   lockIdentity `json:"scoring_lock_identity"`
	RuntimeProcessMarkers []string     `json:"runtime_process_markers"`
	Memory                memoryBudget `json:"memory"`
}

// This is a separate prospective owner decision, not a generated receipt.
// The caller supplies its path and sha256; the tool never creates approval.
type ownerApproval struct {
	Schema                          int       `json:"schema"`
	Kind                            string    `json:"kind"`
	Approved                        bool      `json:"approved"`
	FreshClockException             bool      `json:"fresh_clock_exception"`
	AcknowledgeOldDeadlineException bool      `json:"acknowledge_old_deadline_exception"`
	ConfigSha256                    string    `json:"config_sha256"`
	FailureProofSha256              string    `json:"failure_proof_sha256"`
	PublicPlanSha256                string    `json:"public_plan_sha256"`
	OldGlobalStartSha256            string    `json:"old_global_start_sha256"`
	OldFirstStartSha256             string    `json:"old_first_start_sha256"`
	FailedTreeSha256                string    `json:"failed_tree_sha256"`
	OldDeadline                     time.Time `json:"old_deadline"`
	FirstJobIds                     []string  `json:"first_job_ids"`
	FreshClockLimitSeconds          int       `json:"fresh_clock_limit_seconds"`
	NoNewEntropy                    bool      `json:"no_new_entropy"`
	NoInputChanges                  bool      `json:"no_input_changes"`
	SecondSeedUnstarted             bool      `json:"second_seed_unstarted"`
	AllFailedArtifactsRetained      bool      `json:"all_failed_artifacts_retained"`
}

// The incident investigator creates this summary over separately retained raw
// evidence. Zero-valued counters alone are insufficient: all claims are explicit.
type failureProof struct {
	Schema                   int       `json:"schema"`
	Kind                     string    `json:"kind"`
	CampaignRoot             string    `json:"campaign_root"`
	FirstJobRoot             string    `json:"first_job_root"`
	PublicPlanSha256         string    `json:"public_plan_sha256"`
	OldGlobalStartSha256     string    `json:"old_global_start_sha256"`
	OldFirstStartSha256      string    `json:"old_first_start_sha256"`
	FailedTreeSha256         string    `json:"failed_tree_sha256"`
	ZeroMeasurementsProved   bool      `json:"zero_measurements_proved"`
	SetupOomBeforeSimulation bool      `json:"setup_oom_before_simulation"`
	NoSimulationProgress     bool      `json:"no_simulation_progress"`
	NoResultsOrBaseline      bool      `json:"no_results_or_baseline"`
	NoCompletionOrOutcomes   bool      `json:"no_completion_or_outcomes"`
	RunsAndScoreOutputEmpty  bool      `json:"runs_and_score_output_empty"`
	NoActiveProcesses        bool      `json:"no_active_processes"`
	NoResidualMounts         bool      `json:"no_residual_mounts"`
	SecondSeedUnstarted      bool      `json:"second_seed_unstarted"`
	RawEvidence              []filePin `json:"raw_evidence"`
}

// Minimal public-only projection; no private input or seed is decoded.
type publicPlan struct {
	Selected struct {
		PrivatePlanSha256 string        `json:"private_plan_sha256"`
		Rounds            []publicRound `json:"rounds"`
	} `json:"selected"`
	OriginalPrivatePlanSha256 string      `json:"original_private_plan_sha256"`
	UnstartedSecondRound      publicRound `json:"unstarted_second_round"`
}

// Public identities and positions constrain all six filesystem job locations.
type publicRound struct {
	Position        int         `json:"position"`
	RoundId         string      `json:"round_id"`
	ProvidersSha256 string      `json:"providers_sha256"`
	Jobs            []publicJob `json:"jobs"`
}

// Only public identity, ordering, and input hashes are projected.
type publicJob struct {
	Position      int    `json:"position"`
	Role          string `json:"role"`
	JobId         string `json:"job_id"`
	RequestSha256 string `json:"precommitted_request_sha256"`
}

// A read-only result contains paths, hashes, counts, and explicit old-clock data.
type recoveryReport struct {
	Mode                    string    `json:"mode"`
	ConfigSha256            string    `json:"config_sha256"`
	ApprovalSha256          string    `json:"approval_sha256,omitempty"`
	FailureProofSha256      string    `json:"failure_proof_sha256"`
	FailedTreeSha256        string    `json:"failed_tree_sha256"`
	AuditRoot               string    `json:"audit_root"`
	JobCount                int       `json:"job_count"`
	RetainedEntryCount      int       `json:"retained_entry_count"`
	OldDeadline             time.Time `json:"old_deadline"`
	FreshClockException     bool      `json:"fresh_clock_exception"`
	FreshClockNotStarted    bool      `json:"fresh_clock_not_started"`
	RequiredManagementBytes int64     `json:"required_management_bytes"`
}

// Lowercase digests have one canonical spelling for all records and comparisons.
func validDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == value
}

// Hashes are only over opaque bytes or public metadata, never seed decoding.
func digestBytes(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}

// Strict metadata rejects unknown fields, trailing objects, and duplicate keys.
// Duplicate detection applies recursively before typed decoding.
func decodeMetadata(content []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(content))
	var visit func() error
	visit = func() error {
		token, err := decoder.Token()
		if err != nil {
			return errors.New("invalid administrative metadata")
		}
		delimiter, isDelimiter := token.(json.Delim)
		if !isDelimiter {
			return nil
		}
		switch delimiter {
		case '{':
			seenKeys := map[string]bool{}
			for decoder.More() {
				key, err := decoder.Token()
				name, ok := key.(string)
				if err != nil || !ok || seenKeys[name] {
					return errors.New("duplicate or invalid metadata field")
				}
				// encoding/json matches struct fields case-insensitively. Reject
				// aliases before typed decoding, including escaped uppercase keys.
				if len(name) == 0 || name[0] < 'a' || name[0] > 'z' {
					return errors.New("metadata field must use canonical lowercase ascii")
				}
				for _, character := range name {
					if !((character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '_') {
						return errors.New("metadata field must use canonical lowercase ascii")
					}
				}
				seenKeys[name] = true
				if err := visit(); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := visit(); err != nil {
					return err
				}
			}
		default:
			return errors.New("invalid metadata delimiter")
		}
		_, err = decoder.Token()
		return err
	}
	if err := visit(); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("trailing administrative metadata")
	}
	decoder = json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("administrative metadata schema mismatch")
	}
	return nil
}

// Missing flags and apply=false have no persistent side effects.
func runCommand(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("unmeasured-recovery", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "protected recovery config path")
	configSha256 := flags.String("config-sha256", "", "exact recovery config digest")
	approvalPath := flags.String("owner-approval", "", "prospective owner approval path")
	approvalSha256 := flags.String("owner-approval-sha256", "", "exact owner approval digest")
	apply := flags.Bool("apply", false, "one-use archive and byte-identical input restoration")
	memoryOnly := flags.Bool("memory-preflight-only", false, "validate current cgroup before any start or copy")
	inventoryOnly := flags.Bool("inventory-only", false, "read-only fixed first-job inventory for independent forensic pinning")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		return errors.New("invalid command arguments")
	}
	if os.Geteuid() != 0 {
		return errors.New("root uid required")
	}
	policy, settings := defaultRecoveryPolicy(), defaultRecoverySettings()
	if *inventoryOnly {
		if *apply || *memoryOnly || *configPath != "" || *configSha256 != "" || *approvalPath != "" || *approvalSha256 != "" {
			return errors.New("inventory mode accepts no recovery or approval flags")
		}
		if err := checkRuntimeQuiescent(recoveryConfig{CampaignRoot: policy.CampaignRoot}, settings); err != nil {
			return err
		}
		entries, treeSha256, err := failedTreeInventory(firstJobRoot(policy), policy.Paths, settings)
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(map[string]any{"mode": "inventory-only", "failed_tree_sha256": treeSha256, "entries": entries})
	}
	content, err := readPinned(filePin{Path: *configPath, Sha256: *configSha256}, policy.Paths, settings.MaximumMetadataBytes)
	if err != nil {
		return err
	}
	var config recoveryConfig
	if err := decodeMetadata(content, &config); err != nil {
		return err
	}
	limits, err := currentCgroupLimits(os.ReadFile)
	if err != nil {
		return err
	}
	requiredBytes, err := validateMemoryBudget(config.Memory, limits)
	if err != nil {
		return err
	}
	if *memoryOnly {
		if *apply || *approvalPath != "" || *approvalSha256 != "" {
			return errors.New("memory-only mode cannot apply recovery")
		}
		return json.NewEncoder(output).Encode(map[string]any{"mode": "memory-preflight-only", "required_management_bytes": requiredBytes, "evaluation_limit_bytes": config.Memory.EvaluationLimitBytes, "evidence_limit_bytes": config.Memory.EvidenceLimitBytes, "management_reserve_bytes": config.Memory.ManagementReserveBytes})
	}
	request := recoveryRequest{Config: config, ConfigPin: filePin{Path: *configPath, Sha256: *configSha256}, ApprovalPin: filePin{Path: *approvalPath, Sha256: *approvalSha256}, Apply: *apply}
	hooks := recoveryHooks{Now: time.Now, CheckRuntime: checkRuntimeQuiescent, MemoryLimits: func() ([]cgroupLimit, error) { return currentCgroupLimits(os.ReadFile) }, AfterStep: func(string) error { return nil }}
	report, err := recoverUnmeasured(request, policy, settings, hooks)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(report)
}

// Errors expose no input content, private seed, public identity, or request.
func main() {
	if err := runCommand(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "unmeasured recovery refused:", err)
		os.Exit(1)
	}
}

// Canonical locations are derived from fixed roots, never caller-selected claims.
func firstJobRoot(policy recoveryPolicy) string {
	return filepath.Join(policy.CampaignRoot, "round-01", "job-01-better")
}
