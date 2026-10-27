// Read budgets and the release owner's optional enrollment for the §22.6
// through §22.9 subnet family readers. Every tunable is a field with one
// default; enrollment only names what to read and never attests that the
// chain, an artifact or a process is healthy. A present malformed block stays
// visible as an unknown configuration instead of becoming an empty scope.
package monitor

import (
	"bytes"
	"crypto/sha256"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/urfoundation/sn/v2026/ss58"
	"gopkg.in/yaml.v3"
)

const subnetReplayEnrollmentSchema = "urnetwork-monitor-subnet-replay-v1"
const maximumSubnetReplaySettingsBytes = 16 * 1024
const maximumSubnetReplayValidators = 16

// SubnetReplaySettings is shared by the weight, settlement, evidence and
// process readers. Durations are wall-clock bounds; counts and byte sizes are
// per family run unless named otherwise. Zero enrollment values mean "not
// enrolled", which keeps the dependent check unknown rather than healthy.
type SubnetReplaySettings struct {
	// Interval between two runs of one family reader.
	Cadence time.Duration
	// One family run, including shared ssh admission and every relayed read.
	RunBudget time.Duration
	// One relayed json rpc batch end to end, including the ssh session.
	RpcExchangeTimeout time.Duration
	// The node read inside one relayed batch; shorter than the exchange so
	// the remote reader always ends before its ssh owner.
	RpcRemoteReadTimeout time.Duration
	// Response bytes of one relayed batch before decoding; one more byte fails.
	RpcMaximumResponseBytes int
	// Members of one json rpc batch.
	RpcMaximumBatchRequests int
	// Relayed batches one family run may spend: the shared endpoint budget.
	RpcMaximumExchanges int
	// Inclusive eth_getLogs window, matching the server's settlement indexer.
	LogRangeBlocks uint64
	// Log windows one family run may request while its frontier catches up.
	LogMaximumRangesPerRun int
	// Logs one window may return before the window is treated as truncated.
	LogMaximumEntriesPerRange int
	// Coordinator operator census bound; a larger census is unknown.
	MaximumOperators int
	// Epoch-by-operator entitlement census bound; a larger census leaves the
	// whole-history identities unknown while per-epoch checks still run.
	MaximumCensusEntries int
	// Blocks after the finalize offset before an unfinalized epoch pages.
	FinalizeOverdueBlocks uint64
	// Remaining root window blocks below which a missing root is at risk.
	RootAtRiskBlocks uint64
	// Remaining close grace blocks below which a missing close is at risk.
	CloseAtRiskBlocks uint64
	// Consecutive dust-deferred epochs of one operator before a warning.
	DustDeferralWarnEpochs int
	// Credit replay bound: distinct coldkeys whose current credit is compared.
	MaximumCreditChecks int
	// One public api read, headers included.
	ApiReadTimeout time.Duration
	// Body bytes of one public api read before decoding; one more byte fails.
	ApiMaximumBodyBytes int
	// Header bytes of one public api read.
	ApiMaximumHeaderBytes int
	// Concurrent connections to one enrolled origin; reads are serial anyway.
	ApiMaximumConnectionsPerOrigin int
	// Paged history reads per origin per run.
	HistoryMaximumPages int
	// Objects requested per history page.
	HistoryPageObjects int
	// Chain-committed artifacts fetched per origin per run.
	ArtifactMaximumReads int
	// Anchored payloads fetched per origin per run.
	PayloadMaximumReads int
	// New write-once anchors read back per run.
	AnchorMaximumChecks int
	// Remaining anchor deadline blocks below which a missing anchor is delayed.
	AnchorAtRiskBlocks uint64
	// Blocks after an epoch end before its audit replay must be complete.
	AuditReplaySloBlocks uint64
	// Runtime metadata bytes accepted for native storage decoding.
	MetadataMaximumBytes int
	// Permitted validator rows read per run.
	MaximumValidatorRows int
	// Native storage keys requested per page while enumerating a map.
	NativeKeysPerPage int
	// Native key pages per enumerated map per run.
	NativeMaximumKeyPages int
	// Native epochs a finalized commit may remain unrevealed past its reveal epoch.
	RevealOverdueEpochs uint64
	// Tempo multiples without a weight update before a permitted validator is stale.
	WeightStaleTempos uint64
	// Validator trust in u16 units below which an active validator has collapsed.
	ValidatorTrustCollapse uint16
	// Producer chain mirror lag in blocks before liveness warns.
	MirrorLagWarnBlocks uint64
	// Producer chain mirror update age before liveness warns.
	MirrorStaleAge time.Duration
	// Age of a broadcast or signed intent before it is unresolved.
	IntentUnresolvedAge time.Duration
	// Producer intents examined per run.
	MaximumIntents int
	// Producer mirror and task rows examined per run.
	ProducerMaximumRows int
	// Reschedule errors on one subnet task before liveness warns.
	TaskRescheduleWarnCount int64

	// Release owner's enrollment; see SIGNALS.md §22.0 and the family sections.
	Schema string
	// Exactly two bare https operator api origins for public evidence reads.
	ApiOrigins []string
	// First evm block of the immutable settlement journal replay; zero leaves
	// event replay unknown while state-only identities still run.
	ReplayFromBlock uint64
	// First evm block of the evidence journal replay; zero leaves anchors unknown.
	EvidenceFromBlock uint64
	// Enrolled validator hotkeys in ss58 form; never rendered.
	Validators []string
	// Signed steering policy weight cap; zero leaves the cap check unknown.
	MaximumWeightLimitU16 uint16
	// Blocks after an epoch end within which its census anchor is required;
	// zero leaves anchor deadlines unknown.
	AnchorDeadlineBlocks uint64
	// Fundable actions per scoped writer below which funding warns; zero
	// leaves the runway check unknown.
	WriterMinimumActions uint64
	// Per-action gas liability ceiling in wei, decimal; empty leaves runway unknown.
	WriterActionLiabilityWei string

	invalid      bool
	desiredInput [32]byte
}

// The defaults follow the proposed §22.0 collection profile: a bounded
// incremental pass per cadence with a retained frontier, never a full scan.
func DefaultSubnetReplaySettings() *SubnetReplaySettings {
	return &SubnetReplaySettings{
		Cadence:                        5 * time.Minute,
		RunBudget:                      3 * time.Minute,
		RpcExchangeTimeout:             25 * time.Second,
		RpcRemoteReadTimeout:           15 * time.Second,
		RpcMaximumResponseBytes:        8 * 1024 * 1024,
		RpcMaximumBatchRequests:        64,
		RpcMaximumExchanges:            24,
		LogRangeBlocks:                 1000,
		LogMaximumRangesPerRun:         256,
		LogMaximumEntriesPerRange:      2048,
		MaximumOperators:               16,
		MaximumCensusEntries:           512,
		FinalizeOverdueBlocks:          300,
		RootAtRiskBlocks:               300,
		CloseAtRiskBlocks:              60,
		DustDeferralWarnEpochs:         3,
		MaximumCreditChecks:            256,
		ApiReadTimeout:                 10 * time.Second,
		ApiMaximumBodyBytes:            16 * 1024 * 1024,
		ApiMaximumHeaderBytes:          16 * 1024,
		ApiMaximumConnectionsPerOrigin: 1,
		HistoryMaximumPages:            16,
		HistoryPageObjects:             256,
		ArtifactMaximumReads:           8,
		PayloadMaximumReads:            8,
		AnchorMaximumChecks:            256,
		AnchorAtRiskBlocks:             300,
		AuditReplaySloBlocks:           7200,
		MetadataMaximumBytes:           8 * 1024 * 1024,
		MaximumValidatorRows:           64,
		NativeKeysPerPage:              64,
		NativeMaximumKeyPages:          8,
		RevealOverdueEpochs:            1,
		WeightStaleTempos:              3,
		ValidatorTrustCollapse:         6554,
		MirrorLagWarnBlocks:            150,
		MirrorStaleAge:                 10 * time.Minute,
		IntentUnresolvedAge:            30 * time.Minute,
		MaximumIntents:                 256,
		ProducerMaximumRows:            64,
		TaskRescheduleWarnCount:        3,
	}
}

// Nested values are copied before a caller can reuse its settings snapshot.
func cloneSubnetReplaySettings(value *SubnetReplaySettings) *SubnetReplaySettings {
	if value == nil {
		return nil
	}
	copy := *value
	copy.ApiOrigins = append([]string(nil), value.ApiOrigins...)
	copy.Validators = append([]string(nil), value.Validators...)
	return &copy
}

// The effective value always carries every default; enrollment is optional.
func effectiveSubnetReplaySettings(value *SubnetReplaySettings) *SubnetReplaySettings {
	if value == nil {
		return DefaultSubnetReplaySettings()
	}
	return cloneSubnetReplaySettings(value)
}

// Only the enrollment fields are read from monitor.yml; budgets keep their
// single default so a configuration typo cannot silently widen a bound.
type subnetReplayEnrollmentYaml struct {
	Schema                   string   `yaml:"schema"`
	ApiOrigins               []string `yaml:"api_origins"`
	ReplayFromBlock          uint64   `yaml:"replay_from_block"`
	EvidenceFromBlock        uint64   `yaml:"evidence_from_block"`
	Validators               []string `yaml:"validators"`
	MaximumWeightLimitU16    uint16   `yaml:"maximum_weight_limit_u16"`
	AnchorDeadlineBlocks     uint64   `yaml:"anchor_deadline_blocks"`
	WriterMinimumActions     uint64   `yaml:"writer_minimum_actions"`
	WriterActionLiabilityWei string   `yaml:"writer_action_liability_wei"`
}

// An absent block is nil, so every enrollment-dependent check stays unknown.
// A malformed block keeps its defaults and an invalid marker. Raw yaml and
// decoder errors never reach a finding or diagnostic.
func subnetReplaySettingsFromNode(node yaml.Node) *SubnetReplaySettings {
	if node.Kind == 0 {
		return nil
	}
	invalid := DefaultSubnetReplaySettings()
	invalid.invalid = true
	remaining := 256
	remainingBytes := maximumSubnetReplaySettingsBytes
	if node.Kind != yaml.MappingNode || !boundedSubnetArtifactYaml(&node, 0, &remaining, &remainingBytes) {
		return invalid
	}
	raw, err := yaml.Marshal(&node)
	if err != nil || len(raw) > maximumSubnetReplaySettingsBytes {
		return invalid
	}
	invalid.desiredInput = sha256.Sum256(raw)
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var enrollment subnetReplayEnrollmentYaml
	if decoder.Decode(&enrollment) != nil || decoder.Decode(new(any)) != io.EOF {
		return invalid
	}
	value := DefaultSubnetReplaySettings()
	value.Schema = enrollment.Schema
	value.ApiOrigins = append([]string(nil), enrollment.ApiOrigins...)
	value.ReplayFromBlock = enrollment.ReplayFromBlock
	value.EvidenceFromBlock = enrollment.EvidenceFromBlock
	value.Validators = append([]string(nil), enrollment.Validators...)
	value.MaximumWeightLimitU16 = enrollment.MaximumWeightLimitU16
	value.AnchorDeadlineBlocks = enrollment.AnchorDeadlineBlocks
	value.WriterMinimumActions = enrollment.WriterMinimumActions
	value.WriterActionLiabilityWei = enrollment.WriterActionLiabilityWei
	value.desiredInput = invalid.desiredInput
	if value.enrollmentReason() != "" {
		value.invalid = true
	}
	return value
}

// Decodes an enrolled Bittensor hotkey; the value is compared, never rendered.
func subnetReplayAccountId(address string) ([32]byte, error) {
	return ss58.DecodeWithPrefix(address, ss58.BittensorPrefix)
}

var subnetReplaySs58 = regexp.MustCompile(`^[1-9A-HJ-NP-Za-km-z]{46,48}$`)
var subnetReplayDecimal = regexp.MustCompile(`^(?:0|[1-9][0-9]{0,77})$`)

// Return a fixed reason, never a desired value. Enrollment is optional, so an
// all-zero block is valid; a present field must still be well formed.
func (self *SubnetReplaySettings) enrollmentReason() string {
	if self == nil {
		return ""
	}
	if self.invalid {
		return "enrollment-invalid"
	}
	if self.Schema != "" && self.Schema != subnetReplayEnrollmentSchema {
		return "enrollment-invalid"
	}
	if len(self.ApiOrigins) != 0 {
		if len(self.ApiOrigins) != 2 || !subnetArtifactOrigin(self.ApiOrigins[0]) || !subnetArtifactOrigin(self.ApiOrigins[1]) || self.ApiOrigins[0] == self.ApiOrigins[1] {
			return "enrollment-invalid"
		}
	}
	if len(self.Validators) > maximumSubnetReplayValidators {
		return "enrollment-invalid"
	}
	seen := map[string]bool{}
	for _, validator := range self.Validators {
		if !subnetReplaySs58.MatchString(validator) || seen[validator] {
			return "enrollment-invalid"
		}
		if _, err := subnetReplayAccountId(validator); err != nil {
			return "enrollment-invalid"
		}
		seen[validator] = true
	}
	if self.WriterActionLiabilityWei != "" && (!subnetReplayDecimal.MatchString(self.WriterActionLiabilityWei) || strings.TrimLeft(self.WriterActionLiabilityWei, "0") == "") {
		return "enrollment-invalid"
	}
	return ""
}

// Budgets must be positive for a run to be bounded; an embedder that zeroes
// one receives an explicit unknown instead of an unbounded read.
func (self *SubnetReplaySettings) budgetReason() string {
	if self.Cadence <= 0 || self.RunBudget <= 0 || self.RpcExchangeTimeout <= 0 || self.RpcRemoteReadTimeout < time.Second ||
		self.RpcRemoteReadTimeout >= self.RpcExchangeTimeout || self.RpcMaximumResponseBytes <= 0 ||
		self.RpcMaximumBatchRequests <= 0 || self.RpcMaximumExchanges <= 0 || self.LogRangeBlocks == 0 ||
		self.LogMaximumRangesPerRun <= 0 || self.LogMaximumEntriesPerRange <= 0 || self.MaximumOperators <= 0 ||
		self.MaximumCensusEntries <= 0 || self.ApiReadTimeout <= 0 || self.ApiMaximumBodyBytes <= 0 ||
		self.ApiMaximumHeaderBytes <= 0 || self.ApiMaximumConnectionsPerOrigin <= 0 || self.HistoryMaximumPages <= 0 || self.HistoryPageObjects <= 0 ||
		self.ArtifactMaximumReads <= 0 || self.PayloadMaximumReads <= 0 || self.AnchorMaximumChecks <= 0 || self.MetadataMaximumBytes <= 0 || self.MaximumValidatorRows <= 0 || self.NativeKeysPerPage <= 0 || self.NativeMaximumKeyPages <= 0 ||
		self.MaximumCreditChecks <= 0 || self.MaximumIntents <= 0 || self.ProducerMaximumRows <= 0 || self.TaskRescheduleWarnCount <= 0 || self.WeightStaleTempos == 0 {
		return "budget-invalid"
	}
	return ""
}
