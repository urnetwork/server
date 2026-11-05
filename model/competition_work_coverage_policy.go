// Prospective offered-work coverage is separate from the latency objective and
// G2 volume floors. This standalone dependency can accompany a reviewed trusted
// instrument overlay without copying newer control-plane implementations.
package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"math/big"
	"strconv"
)

const CompetitionWorkCoverageScorerVersion = "sim-latency-score/3"
const CompetitionWorkCoverageKind = "seeded-crawl-coverage/v1"

// The v1 wire object has three small scalar fields; it is not a request body
// size knob or a source of configurable work-inventory allocation budgets.
const competitionWorkCoveragePolicyBytes = 512

// Schema-three run manifests include the bounded completion ledger. This is
// an artifact-format ceiling, distinct from the control RPC document budget.
const CompetitionWorkCoverageMaximumManifestBytes = 8 << 20

// Nil/omission preserves historical scoring exactly. An explicit v1 contract
// freezes both tolerances, never deriving them from candidate measurements.
type CompetitionWorkCoveragePolicy struct {
	Kind                    string  `json:"kind" yaml:"kind"`
	MaximumPageCoverageLoss float64 `json:"maximum_page_coverage_loss" yaml:"maximum_page_coverage_loss"`
	MaximumByteCoverageLoss float64 `json:"maximum_byte_coverage_loss" yaml:"maximum_byte_coverage_loss"`
}

// A missing historical leaf stays missing; explicit policies are copied by
// value at independent authority and lifecycle boundaries.
func (self *CompetitionWorkCoveragePolicy) Clone() *CompetitionWorkCoveragePolicy {
	if self == nil {
		return nil
	}
	copy := *self
	return &copy
}

// One percentage point is an absolute loss in completion fractions. There is
// no absolute 97% work floor, no zero-loss rule, and no upper-volume penalty.
func DefaultCompetitionWorkCoveragePolicy() CompetitionWorkCoveragePolicy {
	return CompetitionWorkCoveragePolicy{Kind: CompetitionWorkCoverageKind, MaximumPageCoverageLoss: 0.01, MaximumByteCoverageLoss: 0.01}
}

// Values different from the versioned v1 contract need another
// policy version; an omitted tolerance must not become an implicit zero.
func ValidateCompetitionWorkCoveragePolicy(policy *CompetitionWorkCoveragePolicy) error {
	if policy == nil {
		return nil
	}
	expected := DefaultCompetitionWorkCoveragePolicy()
	if *policy != expected {
		return errors.New("unsupported or incomplete seeded crawl coverage policy")
	}
	return nil
}

// Owned policy keys are exact, unique and complete before typed decoding.
// Parent contracts still decide whether omission is historically permitted.
func (self *CompetitionWorkCoveragePolicy) UnmarshalJSON(content []byte) error {
	if len(content) == 0 || len(content) > competitionWorkCoveragePolicyBytes {
		return errors.New("work coverage policy exceeds its wire shape")
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return errors.New("explicit work coverage policy must be an object")
	}
	seen := map[string]bool{}
	for decoder.More() {
		keyToken, err := decoder.Token()
		key, ok := keyToken.(string)
		if err != nil || !ok || seen[key] || key != "kind" && key != "maximum_page_coverage_loss" && key != "maximum_byte_coverage_loss" {
			return errors.New("ambiguous or unknown work coverage policy field")
		}
		seen[key] = true
		if err := decoder.Decode(new(json.RawMessage)); err != nil {
			return err
		}
	}
	if last, err := decoder.Token(); err != nil || last != json.Delim('}') || len(seen) != 3 {
		return errors.New("incomplete work coverage policy")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("work coverage policy has trailing data")
	}
	type plainPolicy CompetitionWorkCoveragePolicy
	var value plainPolicy
	if err := json.Unmarshal(content, &value); err != nil {
		return err
	}
	policy := CompetitionWorkCoveragePolicy(value)
	if err := ValidateCompetitionWorkCoveragePolicy(&policy); err != nil {
		return err
	}
	*self = policy
	return nil
}

// Historical policy/version mappings do not change. Coverage is supported
// only together with the floors-only volume contract. Native qualification
// and public calibration approval remain separate from source validation.
func CompetitionScorerVersionForWorkCoverage(g2 CompetitionG2VolumePolicy, coverage *CompetitionWorkCoveragePolicy) (string, error) {
	version, err := CompetitionScorerVersionForG2Policy(g2)
	if err != nil || coverage == nil {
		return version, err
	}
	if g2 != CompetitionG2VolumePolicyMinimumWork || ValidateCompetitionWorkCoveragePolicy(coverage) != nil {
		return "", errors.New("seeded crawl coverage requires the explicit minimum-work contract")
	}
	return CompetitionWorkCoverageScorerVersion, nil
}

// Counts are integer response representations, not only payload sizes. Each
// independent run remains the statistical unit; pages are not R9 replicates.
type CompetitionWorkCoverageSummary struct {
	PlanSha256     string  `json:"plan_sha256"`
	OfferedRoots   int64   `json:"offered_roots"`
	ExpectedPages  int64   `json:"expected_pages"`
	ExpectedBytes  int64   `json:"expected_bytes"`
	CompletedPages int64   `json:"completed_pages"`
	CompletedBytes int64   `json:"completed_bytes"`
	PageCoverage   float64 `json:"page_coverage"`
	ByteCoverage   float64 `json:"byte_coverage"`
}

// A structurally valid zero-completion run is observable, never silently
// omitted. Baseline qualification and the prospective gate decide admission.
func ValidateCompetitionWorkCoverageSummary(value *CompetitionWorkCoverageSummary) error {
	if value == nil || len(value.PlanSha256) != 64 || value.OfferedRoots <= 0 || value.ExpectedPages < value.OfferedRoots ||
		value.ExpectedBytes < value.ExpectedPages || value.CompletedPages < 0 || value.ExpectedPages < value.CompletedPages ||
		value.CompletedBytes < 0 || value.ExpectedBytes < value.CompletedBytes ||
		(value.CompletedPages == 0) != (value.CompletedBytes == 0) ||
		math.IsNaN(value.PageCoverage) || math.IsNaN(value.ByteCoverage) ||
		value.PageCoverage != float64(value.CompletedPages)/float64(value.ExpectedPages) ||
		value.ByteCoverage != float64(value.CompletedBytes)/float64(value.ExpectedBytes) {
		return errors.New("invalid independent work coverage summary")
	}
	for _, character := range value.PlanSha256 {
		if !(character >= '0' && character <= '9') && !(character >= 'a' && character <= 'f') {
			return errors.New("invalid work plan digest")
		}
	}
	return nil
}

// Eligibility uses integer fractions and the policy's canonical decimal, not
// an epsilon or binary64 subtraction at the inclusive publication boundary.
func CompetitionWorkCoveragePassed(policy *CompetitionWorkCoveragePolicy, baseline, candidate *CompetitionWorkCoverageSummary) bool {
	if policy == nil || ValidateCompetitionWorkCoveragePolicy(policy) != nil ||
		ValidateCompetitionWorkCoverageSummary(baseline) != nil || ValidateCompetitionWorkCoverageSummary(candidate) != nil ||
		baseline.PlanSha256 != candidate.PlanSha256 || baseline.OfferedRoots != candidate.OfferedRoots ||
		baseline.ExpectedPages != candidate.ExpectedPages || baseline.ExpectedBytes != candidate.ExpectedBytes ||
		candidate.CompletedPages <= 0 || candidate.CompletedBytes <= 0 {
		return false
	}
	for _, dimension := range []struct {
		baseline, candidate, expected int64
		loss                          float64
	}{
		{baseline.CompletedPages, candidate.CompletedPages, baseline.ExpectedPages, policy.MaximumPageCoverageLoss},
		{baseline.CompletedBytes, candidate.CompletedBytes, baseline.ExpectedBytes, policy.MaximumByteCoverageLoss},
	} {
		loss, ok := new(big.Rat).SetString(strconv.FormatFloat(dimension.loss, 'g', -1, 64))
		control := new(big.Rat).SetFrac64(dimension.baseline, dimension.expected)
		if !ok || control.Cmp(loss) <= 0 {
			return false
		}
		floor := new(big.Rat).Sub(control, loss)
		observed := new(big.Rat).SetFrac64(dimension.candidate, dimension.expected)
		if observed.Cmp(floor) < 0 {
			return false
		}
	}
	return true
}

// An explicit coverage object cannot be erased by an older score decoder.
// Marshaling also accepts the typed local producer and durable JSON maps.
func CompetitionScoreWorkCoveragePolicy(diagnostics map[string]any) (*CompetitionWorkCoveragePolicy, error) {
	value, present := diagnostics["work_coverage_policy"]
	if !present {
		return nil, nil
	}
	content, err := json.Marshal(value)
	if err != nil {
		return nil, errors.New("invalid score coverage provenance")
	}
	var policy CompetitionWorkCoveragePolicy
	if err := json.Unmarshal(content, &policy); err != nil {
		return nil, err
	}
	return &policy, nil
}

// All consumers derive the mandatory surface from versioned provenance, not
// from whichever gates a candidate happened to include in its document.
func CompetitionScoreRequiredGateNames(score *CompetitionScoreResult) ([]string, error) {
	if score == nil {
		return nil, errors.New("score is missing")
	}
	coverage, err := CompetitionScoreWorkCoveragePolicy(score.Diagnostics)
	if err != nil {
		return nil, err
	}
	policyValue, policyPresent := score.Diagnostics["g2_volume_policy"]
	versionValue, versionPresent := score.Diagnostics["scorer_version"]
	if policyPresent || versionPresent || coverage != nil {
		policy, policyOk := policyValue.(string)
		version, versionOk := versionValue.(string)
		expected, err := CompetitionScorerVersionForWorkCoverage(CompetitionG2VolumePolicy(policy), coverage)
		if !policyOk || !versionOk || policy != string(CompetitionG2VolumePolicyMinimumWork) || err != nil || version != expected {
			return nil, errors.New("score instrument policy provenance is malformed")
		}
	}
	names := []string{"G1_success", "G2_volume", "G3_path_integrity", "G4_matchmaking", "G5_stability", "G6_resources"}
	if coverage != nil {
		names = append(names, "G7_work_coverage")
	}
	return names, nil
}
