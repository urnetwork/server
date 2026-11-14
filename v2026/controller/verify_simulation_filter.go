package controller

// verify_simulation_filter.go contains the filesystem-only control used by
// sim-testnet to exercise an equivocation-shaped operator fault: one testnet
// operator can make a bounded provider set unavailable to one validator while
// every other validator continues through the production /verify path. The
// control is inert unless an explicitly simulator-scoped environment points at
// a private file. Mainnet and ordinary testnet processes reject the control.

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/urnetwork/server/v2026"
)

const VerifySimulationAssignmentFilterFileEnv = "URNETWORK_SIM_VERIFY_ASSIGNMENT_FILTER_FILE"

const VerifySimulationModeEnv = "URNETWORK_SIM_TESTNET"

const verifySimulationAssignmentFilterSchema = "urnetwork-sim-verify-assignment-filter-v1"

// VerifySimulationAssignmentFilterSchemaV2 is the identity-bound, composable
// simulator filter schema. It is exported so the simulator writer and server
// reader cannot silently drift to different wire identities.
const VerifySimulationAssignmentFilterSchemaV2 = "urnetwork-sim-verify-assignment-filter-v2"

// VerifySimulationAssignmentFilterPlanHashEnv binds a simulator process to the
// one approved setup plan whose rules it may consume. The remaining v2 identity
// fields are checked against the server's enabled testnet ST configuration.
const VerifySimulationAssignmentFilterPlanHashEnv = "URNETWORK_SIM_VERIFY_ASSIGNMENT_FILTER_PLAN_HASH"

const verifySimulationAssignmentFilterMaximumBytes = 4 * 1024 * 1024

const verifySimulationAssignmentFilterMaximumRules = 16

const verifySimulationAssignmentFilterMaximumValidatorVPKs = 8

const verifySimulationAssignmentFilterMaximumClientIDs = 4096

type verifySimulationAssignmentFilter struct {
	Schema            string   `json:"schema"`
	ValidatorVPK      string   `json:"validator_vpk"`
	ExcludedClientIDs []string `json:"excluded_client_ids"`
}

type verifySimulationAssignmentFilterV2 struct {
	Schema       string                                   `json:"schema"`
	DeploymentID string                                   `json:"deployment_id"`
	PlanHash     string                                   `json:"plan_hash"`
	ChainID      uint64                                   `json:"chain_id"`
	GenesisHash  string                                   `json:"genesis_hash"`
	Netuid       uint64                                   `json:"netuid"`
	Coordinator  string                                   `json:"coordinator"`
	OperatorNo   uint64                                   `json:"operator_no"`
	Rules        []verifySimulationAssignmentFilterRuleV2 `json:"rules"`
}

type verifySimulationAssignmentFilterRuleV2 struct {
	RuleID            string   `json:"rule_id"`
	ValidatorVPKs     []string `json:"validator_vpks"`
	ExcludedClientIDs []string `json:"excluded_client_ids"`
}

// verifySimulationAssignmentExclusions loads one atomic simulator control
// snapshot. Missing files mean the fault is inactive. A malformed or
// incorrectly scoped file fails closed instead of silently turning a required
// release vector into the happy path.
func verifySimulationAssignmentExclusions(vpk []byte) ([]server.Id, error) {
	path := strings.TrimSpace(os.Getenv(VerifySimulationAssignmentFilterFileEnv))
	if path == "" {
		return nil, nil
	}
	if !filepath.IsAbs(path) {
		return nil, errors.New("verify simulation assignment filter path is not absolute")
	}
	if os.Getenv("URNETWORK_ST_PROFILE") != "testnet" || os.Getenv(VerifySimulationModeEnv) != "1" {
		return nil, errors.New("verify simulation assignment filter is permitted only in sim-testnet")
	}
	file, absent, err := verifySimulationOpenAssignmentFilterNoFollow(path)
	if absent {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat opened verify simulation assignment filter: %w", err)
	}
	if !openedInfo.Mode().IsRegular() || openedInfo.Mode().Perm() != 0o600 || openedInfo.Size() <= 0 || openedInfo.Size() > verifySimulationAssignmentFilterMaximumBytes {
		return nil, errors.New("opened verify simulation assignment filter is not a private bounded regular file")
	}
	encoded, err := io.ReadAll(io.LimitReader(file, verifySimulationAssignmentFilterMaximumBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read verify simulation assignment filter: %w", err)
	}
	if int64(len(encoded)) != openedInfo.Size() || len(encoded) > verifySimulationAssignmentFilterMaximumBytes {
		return nil, errors.New("verify simulation assignment filter changed while it was read")
	}
	if err := verifySimulationAssignmentFilterRejectDuplicateJSONFields(encoded); err != nil {
		return nil, err
	}
	var envelope struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		return nil, fmt.Errorf("decode verify simulation assignment filter identity: %w", err)
	}
	switch envelope.Schema {
	case verifySimulationAssignmentFilterSchema:
		// A v2-bound process must never interpret a stale v1 file that omits the
		// deployment and plan identity. V1 remains available only for the
		// explicitly unbound local compatibility path.
		if os.Getenv(VerifySimulationAssignmentFilterPlanHashEnv) != "" {
			return nil, errors.New("legacy verify simulation assignment filter is ambiguous in a v2-bound process")
		}
		return verifySimulationAssignmentExclusionsV1(encoded, vpk)
	case VerifySimulationAssignmentFilterSchemaV2:
		return verifySimulationAssignmentExclusionsV2(encoded, vpk)
	default:
		return nil, errors.New("verify simulation assignment filter schema is invalid")
	}
}

func verifySimulationAssignmentExclusionsV1(encoded []byte, vpk []byte) ([]server.Id, error) {
	var filter verifySimulationAssignmentFilter
	if err := verifySimulationAssignmentFilterDecodeExact(encoded, &filter); err != nil {
		return nil, err
	}
	if filter.Schema != verifySimulationAssignmentFilterSchema || len(filter.ExcludedClientIDs) == 0 || len(filter.ExcludedClientIDs) > 256 {
		return nil, errors.New("verify simulation assignment filter identity is invalid")
	}
	if len(vpk) != 32 {
		return nil, errors.New("verify simulation assignment filter request validator key is invalid")
	}
	wantVPK := hex.EncodeToString(vpk)
	if !verifySimulationCanonicalValidatorVPK(filter.ValidatorVPK) {
		return nil, errors.New("verify simulation assignment filter validator key is not canonical")
	}
	if filter.ValidatorVPK != wantVPK {
		return nil, nil
	}
	return verifySimulationCanonicalClientIDs(filter.ExcludedClientIDs)
}

func verifySimulationAssignmentExclusionsV2(encoded []byte, vpk []byte) ([]server.Id, error) {
	if len(vpk) != 32 {
		return nil, errors.New("verify simulation assignment filter request validator key is invalid")
	}
	var filter verifySimulationAssignmentFilterV2
	if err := verifySimulationAssignmentFilterDecodeExact(encoded, &filter); err != nil {
		return nil, err
	}
	if filter.Schema != VerifySimulationAssignmentFilterSchemaV2 || filter.DeploymentID == "" || len(filter.DeploymentID) > 256 || filter.DeploymentID != strings.TrimSpace(filter.DeploymentID) || len(filter.Rules) == 0 || len(filter.Rules) > verifySimulationAssignmentFilterMaximumRules {
		return nil, errors.New("verify simulation assignment filter v2 shape is invalid")
	}
	if err := verifySimulationAssignmentFilterValidateIdentityV2(&filter); err != nil {
		return nil, err
	}
	wantVPK := hex.EncodeToString(vpk)
	matchedClientIDs := map[server.Id]struct{}{}
	seenAssignments := map[string]struct{}{}
	priorRuleID := ""
	for ruleIndex, rule := range filter.Rules {
		if !verifySimulationCanonicalRuleID(rule.RuleID) || ruleIndex > 0 && rule.RuleID <= priorRuleID || len(rule.ValidatorVPKs) == 0 || len(rule.ValidatorVPKs) > verifySimulationAssignmentFilterMaximumValidatorVPKs || len(rule.ExcludedClientIDs) == 0 || len(rule.ExcludedClientIDs) > verifySimulationAssignmentFilterMaximumClientIDs {
			return nil, errors.New("verify simulation assignment filter rules are not canonical")
		}
		priorRuleID = rule.RuleID
		priorVPK := ""
		matchesValidator := false
		for vpkIndex, validatorVPK := range rule.ValidatorVPKs {
			if !verifySimulationCanonicalValidatorVPK(validatorVPK) || vpkIndex > 0 && validatorVPK <= priorVPK {
				return nil, errors.New("verify simulation assignment filter validator keys are not unique canonical order")
			}
			matchesValidator = matchesValidator || validatorVPK == wantVPK
			priorVPK = validatorVPK
		}
		clientIDs, err := verifySimulationCanonicalClientIDs(rule.ExcludedClientIDs)
		if err != nil {
			return nil, err
		}
		for _, validatorVPK := range rule.ValidatorVPKs {
			for clientIndex, encodedClientID := range rule.ExcludedClientIDs {
				assignment := validatorVPK + "\x00" + encodedClientID
				if _, duplicate := seenAssignments[assignment]; duplicate {
					return nil, errors.New("verify simulation assignment filter rules contain a duplicate validator/client assignment")
				}
				seenAssignments[assignment] = struct{}{}
				if matchesValidator && validatorVPK == wantVPK {
					matchedClientIDs[clientIDs[clientIndex]] = struct{}{}
				}
			}
		}
	}
	exclusions := make([]server.Id, 0, len(matchedClientIDs))
	for clientID := range matchedClientIDs {
		exclusions = append(exclusions, clientID)
	}
	sort.Slice(exclusions, func(i, j int) bool { return exclusions[i].String() < exclusions[j].String() })
	return exclusions, nil
}

func verifySimulationAssignmentFilterValidateIdentityV2(filter *verifySimulationAssignmentFilterV2) error {
	cfg := stConfig()
	rawPlanHash := os.Getenv(VerifySimulationAssignmentFilterPlanHashEnv)
	planHash := strings.TrimSpace(rawPlanHash)
	if cfg == nil || !cfg.Enabled || cfg.Profile != "testnet" || cfg.DeploymentId == "" || cfg.ChainId == 0 || cfg.GenesisHash == ([32]byte{}) || cfg.Netuid == 0 || cfg.Netuid > uint64(^uint16(0)) || cfg.ContractAddress == ([20]byte{}) || cfg.NoId == 0 {
		return errors.New("verify simulation assignment filter server identity is unavailable")
	}
	if rawPlanHash != planHash || !verifySimulationCanonicalHex(planHash, 32) || !verifySimulationCanonicalHex(filter.PlanHash, 32) || !verifySimulationCanonicalHex(filter.GenesisHash, 32) || !verifySimulationCanonicalHex(filter.Coordinator, 20) {
		return errors.New("verify simulation assignment filter v2 hashes or coordinator are not canonical")
	}
	wantGenesisHash := fmt.Sprintf("0x%x", cfg.GenesisHash)
	wantCoordinator := strings.ToLower(cfg.ContractAddress.Hex())
	if filter.DeploymentID != cfg.DeploymentId || filter.PlanHash != planHash || filter.ChainID != cfg.ChainId || filter.GenesisHash != wantGenesisHash || filter.Netuid != cfg.Netuid || filter.Coordinator != wantCoordinator || filter.OperatorNo != cfg.NoId {
		return errors.New("verify simulation assignment filter v2 identity differs from this operator deployment")
	}
	return nil
}

func verifySimulationAssignmentFilterDecodeExact(encoded []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode verify simulation assignment filter: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("verify simulation assignment filter has trailing JSON")
	}
	return nil
}

// verifySimulationAssignmentFilterRejectDuplicateJSONFields rejects duplicate
// object members at every nesting depth. encoding/json otherwise accepts the
// last occurrence, which would make an operator and peer reviewer hash or
// interpret the same authenticated control differently.
func verifySimulationAssignmentFilterRejectDuplicateJSONFields(encoded []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var consumeValue func() error
	consumeValue = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, composite := token.(json.Delim)
		if !composite {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]struct{}{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("verify simulation assignment filter object key is invalid")
				}
				if _, duplicate := seen[key]; duplicate {
					return fmt.Errorf("verify simulation assignment filter contains duplicate JSON field %q", key)
				}
				seen[key] = struct{}{}
				if err := consumeValue(); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim('}') {
				return errors.New("verify simulation assignment filter object is malformed")
			}
		case '[':
			for decoder.More() {
				if err := consumeValue(); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim(']') {
				return errors.New("verify simulation assignment filter array is malformed")
			}
		default:
			return errors.New("verify simulation assignment filter JSON is malformed")
		}
		return nil
	}
	if err := consumeValue(); err != nil {
		return fmt.Errorf("decode verify simulation assignment filter: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("verify simulation assignment filter has trailing JSON")
	}
	return nil
}

func verifySimulationCanonicalValidatorVPK(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && verifySimulationNonzeroBytes(decoded)
}

func verifySimulationCanonicalHex(value string, byteCount int) bool {
	if len(value) != 2+2*byteCount || !strings.HasPrefix(value, "0x") || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value[2:])
	return err == nil && len(decoded) == byteCount && verifySimulationNonzeroBytes(decoded)
}

func verifySimulationNonzeroBytes(value []byte) bool {
	for _, item := range value {
		if item != 0 {
			return true
		}
	}
	return false
}

func verifySimulationCanonicalRuleID(value string) bool {
	if len(value) == 0 || len(value) > 64 || !verifySimulationRuleIDAlphaNumeric(value[0]) || !verifySimulationRuleIDAlphaNumeric(value[len(value)-1]) {
		return false
	}
	for index := 1; index+1 < len(value); index++ {
		if value[index] != '-' && !verifySimulationRuleIDAlphaNumeric(value[index]) {
			return false
		}
	}
	return true
}

func verifySimulationRuleIDAlphaNumeric(value byte) bool {
	return 'a' <= value && value <= 'z' || '0' <= value && value <= '9'
}

func verifySimulationCanonicalClientIDs(encodedIDs []string) ([]server.Id, error) {
	clientIDs := make([]server.Id, len(encodedIDs))
	prior := ""
	for index, encodedID := range encodedIDs {
		clientID, err := server.ParseId(encodedID)
		if err != nil || clientID == (server.Id{}) || clientID.String() != encodedID || index > 0 && encodedID <= prior {
			return nil, errors.New("verify simulation assignment filter client ids are not unique canonical order")
		}
		clientIDs[index] = clientID
		prior = encodedID
	}
	return clientIDs, nil
}

func verifySimulationAssignmentExcluded(exclusions []server.Id, clientID server.Id) bool {
	index := sort.Search(len(exclusions), func(index int) bool {
		return exclusions[index].String() >= clientID.String()
	})
	return index < len(exclusions) && exclusions[index] == clientID
}
