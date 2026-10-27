// SIGNALS.md §22.1--§22.5 reviewed subnet correctness enrollment. This block
// names the read scope and the reviewed expectations the family readers
// compare with finalized chain state. Deployment identity itself comes from
// the public config sn.yml, never from this block, and nothing here attests
// chain state. Missing, malformed or expired enrollment remains unknown.
package monitor

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/urfoundation/sn/v2026/protocol"
)

const subnetCorrectnessEnrollmentSchema = "urnetwork-monitor-subnet-correctness-v1"
const maximumSubnetCorrectnessSettingsBytes = 64 * 1024
const maximumSubnetCorrectnessYamlNodes = 2048
const maximumSubnetReviewedRuntimes = 8
const maximumSubnetReviewedOperators = 16
const maximumSubnetReviewedValidators = 16
const maximumSubnetScheduledPolicies = 8
const maximumSubnetPolicyDocumentBytes = 32 * 1024
const maximumSubnetTreasuryHotkeys = 8

// Review timestamps govern the local expectation, never the age of chain
// evidence. Endpoints are read separately; two names for one node are not
// independent observations, and that cannot be detected from here.
type SubnetCorrectnessSettings struct {
	Schema                string                      `yaml:"schema"`
	ReviewFrom            string                      `yaml:"review_from"`
	ReviewUntil           string                      `yaml:"review_until"`
	NativeRpc             []string                    `yaml:"native_rpc"`
	EvmRpc                []string                    `yaml:"evm_rpc"`
	ApiOrigins            []string                    `yaml:"api_origins"`
	DeployBlock           uint64                      `yaml:"deploy_block"`
	ReviewedRuntimes      []SubnetReviewedRuntime     `yaml:"reviewed_runtimes"`
	Governance            SubnetReviewedGovernance    `yaml:"governance"`
	NativeProfile         SubnetReviewedNativeProfile `yaml:"native_profile"`
	PolicyDocument        string                      `yaml:"policy_document"`
	ScheduledPolicyHashes []string                    `yaml:"scheduled_policy_hashes"`
	Operators             []SubnetReviewedOperator    `yaml:"operators"`
	Validators            []SubnetReviewedValidator   `yaml:"validators"`
	TreasuryHotkeys       []string                    `yaml:"treasury_hotkeys"`
	Limits                SubnetCorrectnessLimits     `yaml:"limits"`
	// Public deployment identity loaded from config sn.yml.
	Deployment SubnetDeploymentIdentity `yaml:"-"`
	invalid    bool
}

// A reviewed runtime successor beyond the code-owned sn catalog. Every field
// is required: a matching spec number alone never admits a runtime.
type SubnetReviewedRuntime struct {
	SpecName           string `yaml:"spec_name"`
	SpecVersion        uint32 `yaml:"spec_version"`
	TransactionVersion uint32 `yaml:"transaction_version"`
	StateVersion       uint8  `yaml:"state_version"`
	CodeHash           string `yaml:"code_hash"`
	MetadataHash       string `yaml:"metadata_hash"`
}

// The approved contract deployment. Addresses are lowercase 20-byte hex and
// code hashes are lowercase keccak256 of the deployed runtime bytecode.
// OwnerThreshold zero means the owner is a dedicated account; a positive value
// requires a Safe-compatible multisig with exactly OwnerSigners owners.
type SubnetReviewedGovernance struct {
	CoordinatorImplementation         string `yaml:"coordinator_implementation"`
	CoordinatorProxyCodeHash          string `yaml:"coordinator_proxy_code_hash"`
	CoordinatorImplementationCodeHash string `yaml:"coordinator_implementation_code_hash"`
	SettlementVaultCodeHash           string `yaml:"settlement_vault_code_hash"`
	ReserveSink                       string `yaml:"reserve_sink"`
	ReserveSinkCodeHash               string `yaml:"reserve_sink_code_hash"`
	Owner                             string `yaml:"owner"`
	OwnerThreshold                    uint64 `yaml:"owner_threshold"`
	OwnerSigners                      uint64 `yaml:"owner_signers"`
	Guardian                          string `yaml:"guardian"`
	CommitmentOracle                  string `yaml:"commitment_oracle"`
}

// Every reviewed native hyperparameter is required so a partial profile cannot
// silently leave a defense unobserved.
type SubnetReviewedNativeProfile struct {
	MaxAllowedUids             uint16 `yaml:"max_allowed_uids"`
	MaxAllowedValidators       uint16 `yaml:"max_allowed_validators"`
	ImmunityPeriod             uint16 `yaml:"immunity_period"`
	CommitRevealWeightsEnabled bool   `yaml:"commit_reveal_weights_enabled"`
	LiquidAlphaEnabled         bool   `yaml:"liquid_alpha_enabled"`
	WeightsVersionKey          uint64 `yaml:"weights_version_key"`
	MechanismCount             uint8  `yaml:"mechanism_count"`
	Tempo                      uint16 `yaml:"tempo"`
}

// One expected network operator. The artifact signer is the reviewed Evm
// signer of its payout artifacts, which the chain does not record.
type SubnetReviewedOperator struct {
	NoId           uint64 `yaml:"no_id"`
	ArtifactSigner string `yaml:"artifact_signer"`
}

// One expected validator. Keys are public 32-byte values; the monitor holds
// no secret material for them.
type SubnetReviewedValidator struct {
	Hotkey  string `yaml:"hotkey"`
	Coldkey string `yaml:"coldkey"`
	Vpk     string `yaml:"vpk"`
}

// Every tunable bound of the readers. Zero selects the code-owned default;
// any other value must fall inside the documented range.
type SubnetCorrectnessLimits struct {
	RequestTimeoutSeconds      uint64 `yaml:"request_timeout_seconds"`
	EndpointConcurrency        uint64 `yaml:"endpoint_concurrency"`
	MaxRequestsPerEndpoint     uint64 `yaml:"max_requests_per_endpoint"`
	MaxResponseBytes           uint64 `yaml:"max_response_bytes"`
	MaxMetadataBytes           uint64 `yaml:"max_metadata_bytes"`
	MaxBatchCalls              uint64 `yaml:"max_batch_calls"`
	MaxFinalityLagBlocks       uint64 `yaml:"max_finality_lag_blocks"`
	LogChunkBlocks             uint64 `yaml:"log_chunk_blocks"`
	MaxLogRequests             uint64 `yaml:"max_log_requests"`
	MaxLogsPerRequest          uint64 `yaml:"max_logs_per_request"`
	InitialLogWindowBlocks     uint64 `yaml:"initial_log_window_blocks"`
	MaxCommitments             uint64 `yaml:"max_commitments"`
	MaxProofRows               uint64 `yaml:"max_proof_rows"`
	ProofWindowMinutes         uint64 `yaml:"proof_window_minutes"`
	StatsMaxAgeMinutes         uint64 `yaml:"stats_max_age_minutes"`
	CoverageMinEligiblePercent uint64 `yaml:"coverage_min_eligible_percent"`
	BiasMinTrails              uint64 `yaml:"bias_min_trails"`
	BiasZScoreTenths           uint64 `yaml:"bias_z_score_tenths"`
	MaxArtifactProviders       uint64 `yaml:"max_artifact_providers"`
	MaxHeadChurnPerRun         uint64 `yaml:"max_head_churn_per_run"`
}

// The resolved bound for every reader limit.
type subnetLimits struct {
	requestTimeout             time.Duration
	endpointConcurrency        int
	maxRequestsPerEndpoint     int
	maxResponseBytes           int64
	maxMetadataBytes           int64
	maxBatchCalls              int
	maxFinalityLagBlocks       uint64
	logChunkBlocks             uint64
	maxLogRequests             int
	maxLogsPerRequest          int
	initialLogWindowBlocks     uint64
	maxCommitments             int
	maxProofRows               int
	proofWindow                time.Duration
	statsMaxAge                time.Duration
	coverageMinEligiblePercent uint64
	biasMinTrails              uint64
	biasZScoreTenths           uint64
	maxArtifactProviders       int
	maxHeadChurnPerRun         uint64
}

// One configured limit with its default and inclusive range.
type subnetLimitRule struct {
	value    uint64
	fallback uint64
	minimum  uint64
	maximum  uint64
}

// Zero selects the default; an out-of-range value invalidates the enrollment.
func (self subnetLimitRule) resolve() (uint64, bool) {
	if self.value == 0 {
		return self.fallback, true
	}
	return self.value, self.minimum <= self.value && self.value <= self.maximum
}

// Resolves every bound, or reports that one was outside its range.
func (self SubnetCorrectnessLimits) resolve() (subnetLimits, bool) {
	rules := []subnetLimitRule{
		{self.RequestTimeoutSeconds, 10, 1, 60},
		{self.EndpointConcurrency, 2, 1, 4},
		{self.MaxRequestsPerEndpoint, 384, 16, 4096},
		{self.MaxResponseBytes, 4 << 20, 64 << 10, 16 << 20},
		{self.MaxMetadataBytes, 20 << 20, 1 << 20, 32 << 20},
		{self.MaxBatchCalls, 64, 1, 256},
		{self.MaxFinalityLagBlocks, 64, 1, 100000},
		{self.LogChunkBlocks, 1000, 1, 10000},
		{self.MaxLogRequests, 16, 1, 256},
		{self.MaxLogsPerRequest, 1000, 1, 10000},
		{self.InitialLogWindowBlocks, 7200, 1, 1000000},
		{self.MaxCommitments, 1024, 1, 16384},
		{self.MaxProofRows, 500, 1, 1000},
		{self.ProofWindowMinutes, 60, 1, 1440},
		{self.StatsMaxAgeMinutes, 120, 1, 10080},
		{self.CoverageMinEligiblePercent, 50, 1, 100},
		{self.BiasMinTrails, 50, 1, 1000000},
		{self.BiasZScoreTenths, 40, 10, 200},
		{self.MaxArtifactProviders, 256, 1, 16384},
		{self.MaxHeadChurnPerRun, 16, 1, 4096},
	}
	values := make([]uint64, len(rules))
	for i, rule := range rules {
		value, ok := rule.resolve()
		if !ok {
			return subnetLimits{}, false
		}
		values[i] = value
	}
	return subnetLimits{
		requestTimeout:             time.Duration(values[0]) * time.Second,
		endpointConcurrency:        int(values[1]),
		maxRequestsPerEndpoint:     int(values[2]),
		maxResponseBytes:           int64(values[3]),
		maxMetadataBytes:           int64(values[4]),
		maxBatchCalls:              int(values[5]),
		maxFinalityLagBlocks:       values[6],
		logChunkBlocks:             values[7],
		maxLogRequests:             int(values[8]),
		maxLogsPerRequest:          int(values[9]),
		initialLogWindowBlocks:     values[10],
		maxCommitments:             int(values[11]),
		maxProofRows:               int(values[12]),
		proofWindow:                time.Duration(values[13]) * time.Minute,
		statsMaxAge:                time.Duration(values[14]) * time.Minute,
		coverageMinEligiblePercent: values[15],
		biasMinTrails:              values[16],
		biasZScoreTenths:           values[17],
		maxArtifactProviders:       int(values[18]),
		maxHeadChurnPerRun:         values[19],
	}, true
}

var subnetCorrectnessRuntimeName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

// Nested values are copied before a caller can reuse its settings snapshot.
func cloneSubnetCorrectnessSettings(value *SubnetCorrectnessSettings) *SubnetCorrectnessSettings {
	if value == nil {
		return nil
	}
	copy := *value
	copy.NativeRpc = append([]string(nil), value.NativeRpc...)
	copy.EvmRpc = append([]string(nil), value.EvmRpc...)
	copy.ApiOrigins = append([]string(nil), value.ApiOrigins...)
	copy.ReviewedRuntimes = append([]SubnetReviewedRuntime(nil), value.ReviewedRuntimes...)
	copy.ScheduledPolicyHashes = append([]string(nil), value.ScheduledPolicyHashes...)
	copy.Operators = append([]SubnetReviewedOperator(nil), value.Operators...)
	copy.Validators = append([]SubnetReviewedValidator(nil), value.Validators...)
	copy.TreasuryHotkeys = append([]string(nil), value.TreasuryHotkeys...)
	return &copy
}

// A malformed present block stays visible instead of becoming an empty scope.
// Raw Yaml and decoder errors never reach a finding or diagnostic. The
// deployment identity is loaded only for a present block and is attached even
// when the block is invalid.
func subnetCorrectnessSettingsFromNode(node yaml.Node, loadDeployment func() SubnetDeploymentIdentity) *SubnetCorrectnessSettings {
	if node.Kind == 0 {
		return nil
	}
	deployment := loadDeployment()
	invalid := &SubnetCorrectnessSettings{invalid: true, Deployment: deployment}
	remaining := maximumSubnetCorrectnessYamlNodes
	remainingBytes := maximumSubnetCorrectnessSettingsBytes
	if node.Kind != yaml.MappingNode || !boundedSubnetCorrectnessYaml(&node, 0, &remaining, &remainingBytes) {
		return invalid
	}
	raw, err := yaml.Marshal(&node)
	if err != nil || len(raw) > maximumSubnetCorrectnessSettingsBytes {
		return invalid
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var value SubnetCorrectnessSettings
	if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF {
		return invalid
	}
	value.Deployment = deployment
	return &value
}

// Reject aliases, custom tags and excessive nesting before the narrow decoder.
func boundedSubnetCorrectnessYaml(node *yaml.Node, depth int, remaining, remainingBytes *int) bool {
	*remaining -= 1
	*remainingBytes -= len(node.Value) + len(node.Tag)
	if *remaining < 0 || *remainingBytes < 0 || depth > 5 || node.Alias != nil || node.Anchor != "" {
		return false
	}
	if node.Kind != yaml.MappingNode && node.Kind != yaml.SequenceNode && node.Kind != yaml.ScalarNode {
		return false
	}
	if node.Tag != "!!map" && node.Tag != "!!seq" && node.Tag != "!!str" && node.Tag != "!!int" && node.Tag != "!!bool" && node.Tag != "!!timestamp" {
		return false
	}
	for _, child := range node.Content {
		if !boundedSubnetCorrectnessYaml(child, depth+1, remaining, remainingBytes) {
			return false
		}
	}
	return true
}

// A chain endpoint may carry a capability-bearing path, so it is never
// rendered. Userinfo, queries, fragments and non-Http schemes are refused.
func subnetCorrectnessRpcEndpoint(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || len(value) > 512 || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil || parsed.Opaque != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" || parsed.String() != value {
		return false
	}
	return !strings.ContainsAny(value, " \t\r\n")
}

// Lowercase 0x-prefixed hex of an exact byte width.
func subnetCorrectnessHex(value string, size int) bool {
	if len(value) != 2+2*size || !strings.HasPrefix(value, "0x") || strings.ToLower(value) != value {
		return false
	}
	raw, err := hex.DecodeString(value[2:])
	return err == nil && len(raw) == size && !bytes.Equal(raw, make([]byte, size))
}

// The exact reviewed policy bytes, or nil when the document is absent.
func (self *SubnetCorrectnessSettings) policyDocumentBytes() ([]byte, bool) {
	if self == nil || self.PolicyDocument == "" {
		return nil, false
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(self.PolicyDocument)
	if err != nil || len(raw) == 0 || len(raw) > maximumSubnetPolicyDocumentBytes {
		return nil, false
	}
	return raw, true
}

// The enrollment digest identifies an exact reviewed input in memory only.
func (self *SubnetCorrectnessSettings) policyDocumentDigest() string {
	raw, ok := self.policyDocumentBytes()
	if !ok {
		return ""
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

// Return a fixed reason, never a secret-bearing parse error or desired value.
func (self *SubnetCorrectnessSettings) validate(now time.Time) string {
	if self == nil {
		return "expectations-missing"
	}
	if self.invalid || self.Schema != subnetCorrectnessEnrollmentSchema {
		return "expectations-invalid"
	}
	from, fromErr := time.Parse(time.RFC3339Nano, self.ReviewFrom)
	until, untilErr := time.Parse(time.RFC3339Nano, self.ReviewUntil)
	if fromErr != nil || untilErr != nil || from.IsZero() || until.IsZero() || from.UTC().Format(time.RFC3339Nano) != self.ReviewFrom || until.UTC().Format(time.RFC3339Nano) != self.ReviewUntil || !from.Before(until) {
		return "expectations-invalid"
	}
	if now.IsZero() || now.Before(from) || !now.Before(until) {
		return "expectations-outside-review-window"
	}
	if len(self.NativeRpc) != 2 || len(self.EvmRpc) != 2 || len(self.ApiOrigins) != 2 || self.DeployBlock == 0 {
		return "expectations-invalid"
	}
	for _, endpoints := range [][]string{self.NativeRpc, self.EvmRpc} {
		if !subnetCorrectnessRpcEndpoint(endpoints[0]) || !subnetCorrectnessRpcEndpoint(endpoints[1]) || endpoints[0] == endpoints[1] {
			return "expectations-invalid"
		}
	}
	if !subnetArtifactOrigin(self.ApiOrigins[0]) || !subnetArtifactOrigin(self.ApiOrigins[1]) || self.ApiOrigins[0] == self.ApiOrigins[1] {
		return "expectations-invalid"
	}
	if len(self.ReviewedRuntimes) > maximumSubnetReviewedRuntimes {
		return "expectations-invalid"
	}
	for _, runtime := range self.ReviewedRuntimes {
		if !subnetCorrectnessRuntimeName.MatchString(runtime.SpecName) || runtime.SpecVersion == 0 || runtime.TransactionVersion == 0 || !subnetCorrectnessHex(runtime.CodeHash, 32) || !subnetCorrectnessHex(runtime.MetadataHash, 32) {
			return "expectations-invalid"
		}
	}
	governance := self.Governance
	for _, address := range []string{governance.CoordinatorImplementation, governance.ReserveSink, governance.Owner, governance.Guardian, governance.CommitmentOracle} {
		if !subnetArtifactAddress(address) {
			return "expectations-invalid"
		}
	}
	for _, digest := range []string{governance.CoordinatorProxyCodeHash, governance.CoordinatorImplementationCodeHash, governance.SettlementVaultCodeHash, governance.ReserveSinkCodeHash} {
		if !subnetCorrectnessHex(digest, 32) {
			return "expectations-invalid"
		}
	}
	if governance.OwnerThreshold > governance.OwnerSigners || (governance.OwnerThreshold == 0) != (governance.OwnerSigners == 0) || governance.OwnerSigners > 16 {
		return "expectations-invalid"
	}
	profile := self.NativeProfile
	if profile.MaxAllowedUids == 0 || profile.MaxAllowedValidators == 0 || profile.MaxAllowedValidators > profile.MaxAllowedUids || profile.MechanismCount == 0 || profile.Tempo == 0 {
		return "expectations-invalid"
	}
	if self.PolicyDocument != "" {
		if _, ok := self.policyDocumentBytes(); !ok {
			return "expectations-invalid"
		}
	}
	if len(self.ScheduledPolicyHashes) > maximumSubnetScheduledPolicies {
		return "expectations-invalid"
	}
	for _, digest := range self.ScheduledPolicyHashes {
		if !subnetCorrectnessHex(digest, 32) {
			return "expectations-invalid"
		}
	}
	if len(self.Operators) == 0 || len(self.Operators) > maximumSubnetReviewedOperators {
		return "expectations-invalid"
	}
	seenOperators := map[uint64]bool{}
	for _, operator := range self.Operators {
		if operator.NoId == 0 || seenOperators[operator.NoId] || !subnetArtifactAddress(operator.ArtifactSigner) {
			return "expectations-invalid"
		}
		seenOperators[operator.NoId] = true
	}
	if len(self.Validators) == 0 || len(self.Validators) > maximumSubnetReviewedValidators {
		return "expectations-invalid"
	}
	seenValidators := map[string]bool{}
	for _, validator := range self.Validators {
		if !subnetCorrectnessHex(validator.Hotkey, 32) || !subnetCorrectnessHex(validator.Coldkey, 32) || !subnetCorrectnessHex(validator.Vpk, 32) || seenValidators[validator.Hotkey] || seenValidators[validator.Vpk] {
			return "expectations-invalid"
		}
		seenValidators[validator.Hotkey] = true
		seenValidators[validator.Vpk] = true
	}
	if len(self.TreasuryHotkeys) > maximumSubnetTreasuryHotkeys {
		return "expectations-invalid"
	}
	seenTreasury := map[string]bool{}
	for _, hotkey := range self.TreasuryHotkeys {
		if !subnetCorrectnessHex(hotkey, 32) || seenTreasury[hotkey] {
			return "expectations-invalid"
		}
		seenTreasury[hotkey] = true
	}
	if _, ok := self.Limits.resolve(); !ok {
		return "expectations-invalid"
	}
	if reason := self.Deployment.validate(); reason != "" {
		return reason
	}
	return ""
}

// Only valid enrollment resolves limits; callers validate first.
func (self *SubnetCorrectnessSettings) limits() subnetLimits {
	limits, _ := self.Limits.resolve()
	return limits
}

// The reviewed policy when its exact pinned document is enrolled.
func subnetEnrolledPolicy(settings *SubnetCorrectnessSettings) *protocol.Policy {
	document, ok := settings.policyDocumentBytes()
	if !ok {
		return nil
	}
	policy, err := protocol.ParsePolicy(document)
	if err != nil {
		return nil
	}
	digest, err := policy.Hash()
	if err != nil || subnetHex32(digest) != settings.Deployment.PolicyHash {
		return nil
	}
	return policy
}
