// Distributed execution is a prospectively approved, immutable epoch-two policy.
// Transport secrets and operational bounds stay in the coordinator's local role.
package controller

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"regexp"
	"time"

	"github.com/urnetwork/server/competitionagent"
)

// Every host has exactly one physical slot, its independent response key, and
// an independently qualified containment fingerprint. Kernel/Docker minor
// versions need not match; the frozen measured source and local fixtures must.
type CompetitionExecutorHostPolicy struct {
	HostId               string `json:"host_id" yaml:"host_id"`
	SlotId               int    `json:"slot_id" yaml:"slot_id"`
	Transport            string `json:"transport" yaml:"transport"`
	QualificationSha256  string `json:"qualification_sha256" yaml:"qualification_sha256"`
	AgentPublicKeyBase64 string `json:"agent_public_key_base64" yaml:"agent_public_key_base64"`
}

// This is an operational admission screen, not a statistical equivalence proof.
// The approved five-percent raw-score band and rejection of significant shifts
// are independent checks. Full per-metric uncertainty remains retained evidence.
type CompetitionHostEquivalencePolicy struct {
	ScreenKind                        string  `json:"screen_kind" yaml:"screen_kind"`
	MaximumRawScoreRelativeDifference float64 `json:"maximum_raw_score_relative_difference" yaml:"maximum_raw_score_relative_difference"`
	ConfidenceZ                       float64 `json:"confidence_z" yaml:"confidence_z"`
}

// Approval includes the host list, response keys and equivalence policy. It
// cannot be added to epoch one or substituted after a round freezes its seed.
type CompetitionDistributedEvaluationPolicy struct {
	Schema            int                              `json:"schema" yaml:"schema"`
	FromEpoch         int                              `json:"from_epoch" yaml:"from_epoch"`
	ApprovalSha256    string                           `json:"approval_sha256" yaml:"approval_sha256"`
	CoordinatorId     string                           `json:"coordinator_id" yaml:"coordinator_id"`
	Hosts             []CompetitionExecutorHostPolicy  `json:"hosts" yaml:"hosts"`
	EquivalencePolicy CompetitionHostEquivalencePolicy `json:"equivalence_policy" yaml:"equivalence_policy"`
}

// Empty retains the exact singleton policy and wire representation.
func validateCompetitionDistributedPolicy(policy *CompetitionDistributedEvaluationPolicy) error {
	if policy == nil {
		return nil
	}
	if policy.Schema != 1 || policy.FromEpoch != 2 || !sha256Pattern.MatchString(policy.ApprovalSha256) || !workerIdPattern.MatchString(policy.CoordinatorId) || len(policy.Hosts) < 2 || len(policy.Hosts) > 16 {
		return errors.New("distributed evaluation requires an approved epoch-two host policy")
	}
	copy := *policy
	copy.ApprovalSha256 = ""
	content, err := json.Marshal(copy)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != policy.ApprovalSha256 {
		return errors.New("distributed evaluation approval digest does not authenticate its policy")
	}
	hostIds, slotIds := map[string]bool{}, map[int]bool{}
	localCount := 0
	for _, host := range policy.Hosts {
		key, err := base64.StdEncoding.DecodeString(host.AgentPublicKeyBase64)
		if !workerIdPattern.MatchString(host.HostId) || host.SlotId < 1 || hostIds[host.HostId] || slotIds[host.SlotId] ||
			(host.Transport != "local" && host.Transport != "ssh") || !sha256Pattern.MatchString(host.QualificationSha256) || err != nil || len(key) != ed25519.PublicKeySize {
			return errors.New("distributed executor host identity is invalid or duplicated")
		}
		hostIds[host.HostId], slotIds[host.SlotId] = true, true
		if host.Transport == "local" {
			localCount++
		}
	}
	if localCount != 1 {
		return errors.New("distributed evaluation requires exactly one local executor")
	}
	p := policy.EquivalencePolicy
	if p.ScreenKind != "operational-admission/v1" || p.MaximumRawScoreRelativeDifference != 0.05 || p.ConfidenceZ != 1.96 {
		return errors.New("host admission requires the explicitly approved five-percent, 95-percent-confidence screen")
	}
	return nil
}

// Exact canonical comparison preserves host order as part of approved policy.
func sameCompetitionDistributedPolicy(left, right *CompetitionDistributedEvaluationPolicy) bool {
	leftBytes, leftErr := json.Marshal(left)
	rightBytes, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftBytes, rightBytes)
}

// A selected physical executor may differ from the coordinator baseline host.
func competitionExecutorQualificationSha256(settings *Settings) string {
	if settings.DistributedEvaluationPolicy != nil && settings.executorHostId != "" {
		for _, host := range settings.DistributedEvaluationPolicy.Hosts {
			if host.HostId == settings.executorHostId && host.SlotId == settings.executorSlotId {
				return competitionExecutorControlHost(settings, host).QualificationSha256
			}
		}
		return ""
	}
	return settings.EvaluationPolicy.HostQualificationSha256
}

// A policy alone cannot route database work; the exact frozen round and one
// approved selected physical executor are both required.
func competitionDistributedEnabled(settings *Settings) bool {
	if settings == nil || settings.DistributedEvaluationPolicy == nil || settings.distributedRoundEpoch < settings.DistributedEvaluationPolicy.FromEpoch || settings.distributedRoundEpoch < 2 {
		return false
	}
	for _, host := range settings.DistributedEvaluationPolicy.Hosts {
		if host.HostId == settings.executorHostId && host.SlotId == settings.executorSlotId {
			return true
		}
	}
	return false
}

// Contains no production vault, database, blob-store or seed-key settings.
// SSH identity is a dedicated forced-command role, never an operator agent.
type competitionExecutorConnection struct {
	HostId                  string `json:"host_id"`
	EvaluatorCommandSha256  string `json:"evaluator_command_sha256,omitempty"`
	AgentConfigSnapshotPath string `json:"agent_config_snapshot_path,omitempty"`
	AgentCommand            string `json:"agent_command"`
	AgentCommandSha256      string `json:"agent_command_sha256,omitempty"`
	AgentConfigPath         string `json:"agent_config_path"`
	AgentConfigSha256       string `json:"agent_config_sha256,omitempty"`
	SshEndpoint             string `json:"ssh_endpoint,omitempty"`
	SshIdentityFile         string `json:"ssh_identity_file,omitempty"`
	SshKnownHostsFile       string `json:"ssh_known_hosts_file,omitempty"`
}

// Operational bounds belong to the coordinator owner. No nonpositive bound
// disables authentication, transfer limits or reconciliation.
type competitionDistributedCoordinatorSettings struct {
	SshCommand             string                  `json:"ssh_command"`
	ConnectionTimeout      time.Duration           `json:"connection_timeout"`
	RequestTimeout         time.Duration           `json:"request_timeout"`
	PollInterval           time.Duration           `json:"poll_interval"`
	CancelTimeout          time.Duration           `json:"cancel_timeout"`
	MaximumDiagnosticBytes int64                   `json:"maximum_diagnostic_bytes"`
	Limits                 competitionagent.Limits `json:"limits"`
}

// Defaults are transport bounds, never calibration or scoring policy.
func defaultCompetitionDistributedCoordinatorSettings() competitionDistributedCoordinatorSettings {
	return competitionDistributedCoordinatorSettings{SshCommand: "/usr/bin/ssh", ConnectionTimeout: 15 * time.Second,
		RequestTimeout: 2 * time.Minute, PollInterval: 5 * time.Second, CancelTimeout: 30 * time.Second,
		MaximumDiagnosticBytes: 1024 * 1024, Limits: competitionagent.DefaultLimits()}
}

// Private, root-pinned deployment role. The approved public policy separately
// freezes response keys and hosts; this file only says how to reach those hosts.
type competitionDistributedCoordinatorConfig struct {
	Schema                      int                                       `json:"schema"`
	Kind                        string                                    `json:"kind"`
	CoordinatorId               string                                    `json:"coordinator_id"`
	CoordinatorPrivateKeyPath   string                                    `json:"coordinator_private_key_path"`
	CoordinatorPrivateKeySha256 string                                    `json:"coordinator_private_key_sha256"`
	Hosts                       []competitionExecutorConnection           `json:"hosts"`
	Settings                    competitionDistributedCoordinatorSettings `json:"settings"`
}

// Only the coordinator process retains the private signing key. Copies of
// frozen Settings share immutable key bytes; no agent request serializes them.
type competitionDistributedCoordinatorProfile struct {
	Config     competitionDistributedCoordinatorConfig
	PrivateKey ed25519.PrivateKey
}

var competitionAgentCommandPathPattern = regexp.MustCompile(`^/[A-Za-z0-9/._-]+$`)
var competitionAgentEndpointPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*@[A-Za-z0-9][A-Za-z0-9.:-]*$`)

// Explicit CLI configuration cannot activate or rewrite the current epoch's
// public policy. Every executable/key/transport path is pinned outside inputs.
func ConfigureCompetitionDistributedCoordinator(settings *Settings, path, expected string) error {
	if settings == nil || settings.FutureEpochPolicy == nil || settings.FutureEpochPolicy.DistributedEvaluationPolicy == nil || !sha256Pattern.MatchString(expected) {
		return errors.New("distributed coordinator requires a prospectively approved future policy")
	}
	if err := competitionQualificationRootPath(path, false); err != nil {
		return err
	}
	content, err := readCompetitionPreparationFile(path, 1024*1024, false)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != expected {
		return errors.New("distributed coordinator configuration digest mismatch")
	}
	config := competitionDistributedCoordinatorConfig{Settings: defaultCompetitionDistributedCoordinatorSettings()}
	if err := decodeCompetitionPreparationJson(bytes.NewReader(content), 1024*1024, &config); err != nil {
		return err
	}
	policy := settings.FutureEpochPolicy.DistributedEvaluationPolicy
	if err := validateCompetitionDistributedPolicy(policy); err != nil {
		return err
	}
	if config.Schema != 1 || config.Kind != "competition-distributed-coordinator/v1" || config.CoordinatorId != policy.CoordinatorId || len(config.Hosts) != len(policy.Hosts) ||
		config.Settings.ConnectionTimeout <= 0 || config.Settings.RequestTimeout <= 0 || config.Settings.PollInterval <= 0 || config.Settings.CancelTimeout <= 0 || config.Settings.MaximumDiagnosticBytes <= 0 ||
		!filepath.IsAbs(config.Settings.SshCommand) || config.Settings.Limits.Validate() != nil || config.Settings.Limits.MaximumJobDuration != 3*time.Hour {
		return errors.New("invalid distributed coordinator role or operational bounds")
	}
	seen := map[string]bool{}
	for _, connection := range config.Hosts {
		var host *CompetitionExecutorHostPolicy
		for i := range policy.Hosts {
			if policy.Hosts[i].HostId == connection.HostId {
				host = &policy.Hosts[i]
			}
		}
		if host == nil || seen[connection.HostId] || !competitionAgentCommandPathPattern.MatchString(connection.AgentCommand) || !competitionAgentCommandPathPattern.MatchString(connection.AgentConfigPath) ||
			filepath.Clean(connection.AgentCommand) != connection.AgentCommand || filepath.Clean(connection.AgentConfigPath) != connection.AgentConfigPath {
			return errors.New("coordinator connection is not an approved fixed agent command")
		}
		seen[connection.HostId] = true
		if err := validateCompetitionExecutorEvaluatorConnection(connection); err != nil {
			return err
		}
		if host.Transport == "ssh" {
			if !competitionAgentEndpointPattern.MatchString(connection.SshEndpoint) {
				return errors.New("invalid dedicated executor SSH endpoint")
			}
			for _, pin := range []string{connection.SshIdentityFile, connection.SshKnownHostsFile, config.Settings.SshCommand} {
				if err := competitionQualificationRootPath(pin, false); err != nil {
					return err
				}
			}
		} else {
			if connection.SshEndpoint != "" || connection.SshIdentityFile != "" || connection.SshKnownHostsFile != "" {
				return errors.New("local executor cannot inherit an SSH role")
			}
			if err := competitionQualificationRootPath(connection.AgentCommand, false); err != nil {
				return err
			}
			if err := competitionQualificationRootPath(connection.AgentConfigPath, false); err != nil {
				return err
			}
			if err := verifyPinnedExecutable(connection.AgentCommand, connection.AgentCommandSha256); err != nil {
				return err
			}
			digest, _, err := hashRegularFile(connection.AgentConfigPath)
			if err != nil || digest != connection.AgentConfigSha256 {
				return errors.New("local executor configuration differs from its pinned role")
			}
		}
	}
	if err := competitionQualificationRootPath(config.CoordinatorPrivateKeyPath, false); err != nil {
		return err
	}
	keyBytes, err := readCompetitionPreparationFile(config.CoordinatorPrivateKeyPath, 4096, false)
	if err != nil {
		return err
	}
	defer clear(keyBytes)
	keyDigest := sha256.Sum256(keyBytes)
	privateKey, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(keyBytes)))
	if err != nil || len(privateKey) != ed25519.PrivateKeySize || hex.EncodeToString(keyDigest[:]) != config.CoordinatorPrivateKeySha256 {
		clear(privateKey)
		return errors.New("distributed coordinator signing key failed authentication")
	}
	settings.distributedCoordinator = &competitionDistributedCoordinatorProfile{Config: config, PrivateKey: privateKey}
	return nil
}
