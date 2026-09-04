package controller

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/urnetwork/server/v2026"
)

type verifySimulationAssignmentFilterV2TestFixture struct {
	Path       string
	PlanHash   string
	Config     *StConfig
	Validators [][]byte
	ClientIDs  []server.Id
	Filter     verifySimulationAssignmentFilterV2
}

// verifySimulationAssignmentFilterTestTempDir returns an absolute test
// directory whose existing components are not symbolic links. On Darwin,
// testing.T.TempDir normally starts with /var, which aliases /private/var;
// feeding that spelling to the production no-follow reader would test the
// platform alias rather than the filter file. Keep the reader strict and give
// positive fixtures the canonical path. Symlink rejection tests add their own
// deliberate link beneath this directory.
func verifySimulationAssignmentFilterTestTempDir(t *testing.T) string {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve verify simulation assignment filter test directory: %v", err)
	}
	return directory
}

func newVerifySimulationAssignmentFilterV2TestFixture(t *testing.T) *verifySimulationAssignmentFilterV2TestFixture {
	t.Helper()
	t.Setenv("URNETWORK_ST_PROFILE", "testnet")
	t.Setenv(VerifySimulationModeEnv, "1")
	path := filepath.Join(verifySimulationAssignmentFilterTestTempDir(t), "assignment-filter.json")
	t.Setenv(VerifySimulationAssignmentFilterFileEnv, path)
	planHash := "0x" + strings.Repeat("33", 32)
	t.Setenv(VerifySimulationAssignmentFilterPlanHashEnv, planHash)
	config := &StConfig{
		Profile:         "testnet",
		Enabled:         true,
		ChainId:         945,
		DeploymentId:    "sim-filter-v2-deployment",
		Netuid:          7,
		NoId:            2,
		ContractAddress: common.HexToAddress("0x1111111111111111111111111111111111111111"),
	}
	for index := range config.GenesisHash {
		config.GenesisHash[index] = 0x22
	}
	previousConfig := stConfigInstance
	SetStConfig(config)
	t.Cleanup(func() { SetStConfig(previousConfig) })
	validators := [][]byte{
		bytes.Repeat([]byte{0x11}, 32),
		bytes.Repeat([]byte{0x22}, 32),
		bytes.Repeat([]byte{0x33}, 32),
	}
	clientIDs := []server.Id{
		verifySimulationAssignmentFilterTestClientID(0x11),
		verifySimulationAssignmentFilterTestClientID(0x22),
		verifySimulationAssignmentFilterTestClientID(0x33),
		verifySimulationAssignmentFilterTestClientID(0x44),
	}
	return &verifySimulationAssignmentFilterV2TestFixture{
		Path: path, PlanHash: planHash, Config: config, Validators: validators, ClientIDs: clientIDs,
		Filter: verifySimulationAssignmentFilterV2{
			Schema: VerifySimulationAssignmentFilterSchemaV2, DeploymentID: config.DeploymentId,
			PlanHash: planHash, ChainID: config.ChainId, GenesisHash: fmt.Sprintf("0x%x", config.GenesisHash),
			Netuid: config.Netuid, Coordinator: strings.ToLower(config.ContractAddress.Hex()), OperatorNo: config.NoId,
		},
	}
}

func verifySimulationAssignmentFilterTestClientID(fill byte) server.Id {
	var clientID server.Id
	for index := range clientID {
		clientID[index] = fill
	}
	return clientID
}

func verifySimulationAssignmentFilterTestEncodedVPKs(validators ...[]byte) []string {
	encoded := make([]string, len(validators))
	for index, validator := range validators {
		encoded[index] = hex.EncodeToString(validator)
	}
	sort.Strings(encoded)
	return encoded
}

func verifySimulationAssignmentFilterTestEncodedClientIDs(clientIDs ...server.Id) []string {
	encoded := make([]string, len(clientIDs))
	for index, clientID := range clientIDs {
		encoded[index] = clientID.String()
	}
	sort.Strings(encoded)
	return encoded
}

func writeVerifySimulationAssignmentFilterV2(path string, filter verifySimulationAssignmentFilterV2) error {
	encoded, err := json.Marshal(filter)
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(encoded, '\n'), 0o600)
}

func atomicWriteVerifySimulationAssignmentFilterV2(path string, filter verifySimulationAssignmentFilterV2) error {
	encoded, err := json.Marshal(filter)
	if err != nil {
		return err
	}
	temporaryPath := path + ".next"
	if err := os.WriteFile(temporaryPath, append(encoded, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func cloneVerifySimulationAssignmentFilterV2(filter verifySimulationAssignmentFilterV2) verifySimulationAssignmentFilterV2 {
	encoded, err := json.Marshal(filter)
	if err != nil {
		panic(err)
	}
	var clone verifySimulationAssignmentFilterV2
	if err := json.Unmarshal(encoded, &clone); err != nil {
		panic(err)
	}
	return clone
}

func verifySimulationAssignmentFilterTestExclusionsMatch(actual []server.Id, expected ...server.Id) bool {
	want := append([]server.Id(nil), expected...)
	sort.Slice(want, func(i, j int) bool { return want[i].String() < want[j].String() })
	return slices.Equal(actual, want)
}

func TestVerifySimulationAssignmentFilterV2UnionsOverlappingRulesForBothValidators(t *testing.T) {
	fixture := newVerifySimulationAssignmentFilterV2TestFixture(t)
	fixture.Filter.Rules = []verifySimulationAssignmentFilterRuleV2{
		{
			RuleID: "fleet-lifecycle-target-prune", ValidatorVPKs: verifySimulationAssignmentFilterTestEncodedVPKs(fixture.Validators[0], fixture.Validators[1]),
			ExcludedClientIDs: verifySimulationAssignmentFilterTestEncodedClientIDs(fixture.ClientIDs[0], fixture.ClientIDs[1]),
		},
		{
			RuleID: "validator-local-head-boundary", ValidatorVPKs: verifySimulationAssignmentFilterTestEncodedVPKs(fixture.Validators[0]),
			ExcludedClientIDs: verifySimulationAssignmentFilterTestEncodedClientIDs(fixture.ClientIDs[2], fixture.ClientIDs[3]),
		},
	}
	if err := writeVerifySimulationAssignmentFilterV2(fixture.Path, fixture.Filter); err != nil {
		t.Fatal(err)
	}
	first, err := verifySimulationAssignmentExclusions(fixture.Validators[0])
	if err != nil || !verifySimulationAssignmentFilterTestExclusionsMatch(first, fixture.ClientIDs[0], fixture.ClientIDs[1], fixture.ClientIDs[2], fixture.ClientIDs[3]) {
		t.Fatalf("first validator exclusions=%v error=%v", first, err)
	}
	second, err := verifySimulationAssignmentExclusions(fixture.Validators[1])
	if err != nil || !verifySimulationAssignmentFilterTestExclusionsMatch(second, fixture.ClientIDs[0], fixture.ClientIDs[1]) {
		t.Fatalf("second validator exclusions=%v error=%v", second, err)
	}
	unknown, err := verifySimulationAssignmentExclusions(fixture.Validators[2])
	if err != nil || len(unknown) != 0 {
		t.Fatalf("unknown validator exclusions=%v error=%v", unknown, err)
	}
}

func TestVerifySimulationAssignmentFilterV1SingleValidatorCompatibilityIsUnambiguous(t *testing.T) {
	t.Setenv("URNETWORK_ST_PROFILE", "testnet")
	t.Setenv(VerifySimulationModeEnv, "1")
	t.Setenv(VerifySimulationAssignmentFilterPlanHashEnv, "")
	path := filepath.Join(verifySimulationAssignmentFilterTestTempDir(t), "assignment-filter.json")
	t.Setenv(VerifySimulationAssignmentFilterFileEnv, path)
	validator := bytes.Repeat([]byte{0x51}, 32)
	other := bytes.Repeat([]byte{0x52}, 32)
	clientID := verifySimulationAssignmentFilterTestClientID(0x53)
	writeVerifySimulationAssignmentFilter(t, path, validator, []server.Id{clientID})
	exclusions, err := verifySimulationAssignmentExclusions(validator)
	if err != nil || !verifySimulationAssignmentFilterTestExclusionsMatch(exclusions, clientID) {
		t.Fatalf("legacy matching exclusions=%v error=%v", exclusions, err)
	}
	if exclusions, err := verifySimulationAssignmentExclusions(other); err != nil || len(exclusions) != 0 {
		t.Fatalf("legacy unrelated exclusions=%v error=%v", exclusions, err)
	}
	t.Setenv(VerifySimulationAssignmentFilterPlanHashEnv, "0x"+strings.Repeat("61", 32))
	if _, err := verifySimulationAssignmentExclusions(validator); err == nil {
		t.Fatal("identity-less v1 filter was accepted in a v2-bound process")
	}
}

func TestVerifySimulationAssignmentFilterV2RejectsNonCanonicalOrderHashesAndDuplicates(t *testing.T) {
	fixture := newVerifySimulationAssignmentFilterV2TestFixture(t)
	base := fixture.Filter
	base.Rules = []verifySimulationAssignmentFilterRuleV2{
		{RuleID: "a-rule", ValidatorVPKs: verifySimulationAssignmentFilterTestEncodedVPKs(fixture.Validators[0], fixture.Validators[1]), ExcludedClientIDs: verifySimulationAssignmentFilterTestEncodedClientIDs(fixture.ClientIDs[0], fixture.ClientIDs[1])},
		{RuleID: "b-rule", ValidatorVPKs: verifySimulationAssignmentFilterTestEncodedVPKs(fixture.Validators[0]), ExcludedClientIDs: verifySimulationAssignmentFilterTestEncodedClientIDs(fixture.ClientIDs[2])},
	}
	if err := writeVerifySimulationAssignmentFilterV2(fixture.Path, base); err != nil {
		t.Fatal(err)
	}
	if _, err := verifySimulationAssignmentExclusions(fixture.Validators[0]); err != nil {
		t.Fatalf("canonical filter was rejected: %v", err)
	}
	mutations := []func(*verifySimulationAssignmentFilterV2){
		func(filter *verifySimulationAssignmentFilterV2) { filter.PlanHash = strings.ToUpper(filter.PlanHash) },
		func(filter *verifySimulationAssignmentFilterV2) {
			filter.GenesisHash = strings.ToUpper(filter.GenesisHash)
		},
		func(filter *verifySimulationAssignmentFilterV2) {
			filter.Coordinator = strings.ToUpper(filter.Coordinator)
		},
		func(filter *verifySimulationAssignmentFilterV2) {
			filter.Rules[0], filter.Rules[1] = filter.Rules[1], filter.Rules[0]
		},
		func(filter *verifySimulationAssignmentFilterV2) { slices.Reverse(filter.Rules[0].ValidatorVPKs) },
		func(filter *verifySimulationAssignmentFilterV2) { slices.Reverse(filter.Rules[0].ExcludedClientIDs) },
		func(filter *verifySimulationAssignmentFilterV2) { filter.Rules[1].RuleID = filter.Rules[0].RuleID },
		func(filter *verifySimulationAssignmentFilterV2) {
			filter.Rules[0].ValidatorVPKs[1] = filter.Rules[0].ValidatorVPKs[0]
		},
		func(filter *verifySimulationAssignmentFilterV2) {
			filter.Rules[0].ExcludedClientIDs[1] = filter.Rules[0].ExcludedClientIDs[0]
		},
		func(filter *verifySimulationAssignmentFilterV2) {
			filter.Rules[1].ExcludedClientIDs[0] = filter.Rules[0].ExcludedClientIDs[0]
		},
	}
	for index, mutate := range mutations {
		candidate := cloneVerifySimulationAssignmentFilterV2(base)
		mutate(&candidate)
		if err := writeVerifySimulationAssignmentFilterV2(fixture.Path, candidate); err != nil {
			t.Fatal(err)
		}
		if _, err := verifySimulationAssignmentExclusions(fixture.Validators[0]); err == nil {
			t.Fatalf("non-canonical filter mutation %d was accepted", index)
		}
	}
}

func TestVerifySimulationAssignmentFilterV2RejectsMalformedUnknownAndOversizeRules(t *testing.T) {
	fixture := newVerifySimulationAssignmentFilterV2TestFixture(t)
	base := fixture.Filter
	base.Rules = []verifySimulationAssignmentFilterRuleV2{{
		RuleID: "valid-rule", ValidatorVPKs: verifySimulationAssignmentFilterTestEncodedVPKs(fixture.Validators[0]),
		ExcludedClientIDs: verifySimulationAssignmentFilterTestEncodedClientIDs(fixture.ClientIDs[0]),
	}}
	valid, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	rawCases := [][]byte{
		[]byte(`{"schema":"` + VerifySimulationAssignmentFilterSchemaV2 + `","schema":"` + VerifySimulationAssignmentFilterSchemaV2 + `"}`),
		bytes.Replace(valid, []byte(`"rule_id":"valid-rule"`), []byte(`"rule_id":"valid-rule","rule_id":"valid-rule"`), 1),
		append(append([]byte(nil), valid...), []byte(` {}`)...),
		bytes.Replace(valid, []byte(`"rules":`), []byte(`"unknown":true,"rules":`), 1),
		[]byte(`{"schema":`),
	}
	for index, encoded := range rawCases {
		if err := os.WriteFile(fixture.Path, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := verifySimulationAssignmentExclusions(fixture.Validators[0]); err == nil {
			t.Fatalf("malformed raw filter %d was accepted", index)
		}
	}
	mutations := []func(*verifySimulationAssignmentFilterV2){
		func(filter *verifySimulationAssignmentFilterV2) { filter.Rules = nil },
		func(filter *verifySimulationAssignmentFilterV2) { filter.DeploymentID = " padded" },
		func(filter *verifySimulationAssignmentFilterV2) { filter.PlanHash = "0x" + strings.Repeat("00", 32) },
		func(filter *verifySimulationAssignmentFilterV2) { filter.Rules[0].RuleID = "-invalid" },
		func(filter *verifySimulationAssignmentFilterV2) { filter.Rules[0].RuleID = strings.Repeat("a", 65) },
		func(filter *verifySimulationAssignmentFilterV2) { filter.Rules[0].ValidatorVPKs = nil },
		func(filter *verifySimulationAssignmentFilterV2) {
			filter.Rules[0].ValidatorVPKs[0] = strings.Repeat("0", 64)
		},
		func(filter *verifySimulationAssignmentFilterV2) {
			filter.Rules[0].ValidatorVPKs[0] = strings.Repeat("z", 64)
		},
		func(filter *verifySimulationAssignmentFilterV2) { filter.Rules[0].ExcludedClientIDs = nil },
		func(filter *verifySimulationAssignmentFilterV2) {
			filter.Rules[0].ExcludedClientIDs[0] = "not-a-client-id"
		},
	}
	for index, mutate := range mutations {
		candidate := cloneVerifySimulationAssignmentFilterV2(base)
		mutate(&candidate)
		if err := writeVerifySimulationAssignmentFilterV2(fixture.Path, candidate); err != nil {
			t.Fatal(err)
		}
		if _, err := verifySimulationAssignmentExclusions(fixture.Validators[0]); err == nil {
			t.Fatalf("malformed typed filter %d was accepted", index)
		}
	}
	tooManyRules := cloneVerifySimulationAssignmentFilterV2(base)
	tooManyRules.Rules = make([]verifySimulationAssignmentFilterRuleV2, verifySimulationAssignmentFilterMaximumRules+1)
	for index := range tooManyRules.Rules {
		tooManyRules.Rules[index] = verifySimulationAssignmentFilterRuleV2{
			RuleID: fmt.Sprintf("rule-%02d", index), ValidatorVPKs: verifySimulationAssignmentFilterTestEncodedVPKs(fixture.Validators[0]),
			ExcludedClientIDs: verifySimulationAssignmentFilterTestEncodedClientIDs(verifySimulationAssignmentFilterTestClientID(byte(index + 1))),
		}
	}
	if err := writeVerifySimulationAssignmentFilterV2(fixture.Path, tooManyRules); err != nil {
		t.Fatal(err)
	}
	if _, err := verifySimulationAssignmentExclusions(fixture.Validators[0]); err == nil {
		t.Fatal("filter with too many rules was accepted")
	}
	tooManyValidatorVPKs := cloneVerifySimulationAssignmentFilterV2(base)
	tooManyValidatorVPKs.Rules[0].ValidatorVPKs = make([]string, verifySimulationAssignmentFilterMaximumValidatorVPKs+1)
	for index := range tooManyValidatorVPKs.Rules[0].ValidatorVPKs {
		tooManyValidatorVPKs.Rules[0].ValidatorVPKs[index] = hex.EncodeToString(bytes.Repeat([]byte{byte(index + 1)}, 32))
	}
	if err := writeVerifySimulationAssignmentFilterV2(fixture.Path, tooManyValidatorVPKs); err != nil {
		t.Fatal(err)
	}
	if _, err := verifySimulationAssignmentExclusions(fixture.Validators[0]); err == nil {
		t.Fatal("filter rule with too many validator keys was accepted")
	}
	tooManyClientIDs := cloneVerifySimulationAssignmentFilterV2(base)
	tooManyClientIDs.Rules[0].ExcludedClientIDs = make([]string, verifySimulationAssignmentFilterMaximumClientIDs+1)
	for index := range tooManyClientIDs.Rules[0].ExcludedClientIDs {
		var clientID server.Id
		clientID[0] = 0x70
		clientID[12] = byte(uint32(index+1) >> 24)
		clientID[13] = byte(uint32(index+1) >> 16)
		clientID[14] = byte(uint32(index+1) >> 8)
		clientID[15] = byte(index + 1)
		tooManyClientIDs.Rules[0].ExcludedClientIDs[index] = clientID.String()
	}
	if err := writeVerifySimulationAssignmentFilterV2(fixture.Path, tooManyClientIDs); err != nil {
		t.Fatal(err)
	}
	if _, err := verifySimulationAssignmentExclusions(fixture.Validators[0]); err == nil {
		t.Fatal("filter rule with too many client ids was accepted")
	}
}

func TestVerifySimulationAssignmentFilterV2RequiresExactCanonicalPlanBinding(t *testing.T) {
	fixture := newVerifySimulationAssignmentFilterV2TestFixture(t)
	fixture.Filter.Rules = []verifySimulationAssignmentFilterRuleV2{{
		RuleID: "valid-rule", ValidatorVPKs: verifySimulationAssignmentFilterTestEncodedVPKs(fixture.Validators[0]),
		ExcludedClientIDs: verifySimulationAssignmentFilterTestEncodedClientIDs(fixture.ClientIDs[0]),
	}}
	if err := writeVerifySimulationAssignmentFilterV2(fixture.Path, fixture.Filter); err != nil {
		t.Fatal(err)
	}
	t.Setenv(VerifySimulationAssignmentFilterPlanHashEnv, "")
	if _, err := verifySimulationAssignmentExclusions(fixture.Validators[0]); err == nil {
		t.Fatal("v2 filter was accepted without an authenticated plan binding")
	}
	t.Setenv(VerifySimulationAssignmentFilterPlanHashEnv, " "+fixture.PlanHash)
	if _, err := verifySimulationAssignmentExclusions(fixture.Validators[0]); err == nil {
		t.Fatal("v2 filter accepted a non-canonical plan binding")
	}
	t.Setenv(VerifySimulationAssignmentFilterPlanHashEnv, fixture.PlanHash)
	if _, err := verifySimulationAssignmentExclusions(fixture.Validators[0]); err != nil {
		t.Fatalf("v2 filter rejected its exact plan binding: %v", err)
	}
}

func TestVerifySimulationAssignmentFilterV2RejectsEveryForeignIdentityField(t *testing.T) {
	fixture := newVerifySimulationAssignmentFilterV2TestFixture(t)
	base := fixture.Filter
	base.Rules = []verifySimulationAssignmentFilterRuleV2{{
		RuleID: "valid-rule", ValidatorVPKs: verifySimulationAssignmentFilterTestEncodedVPKs(fixture.Validators[0]),
		ExcludedClientIDs: verifySimulationAssignmentFilterTestEncodedClientIDs(fixture.ClientIDs[0]),
	}}
	mutations := []func(*verifySimulationAssignmentFilterV2){
		func(filter *verifySimulationAssignmentFilterV2) { filter.DeploymentID += "-foreign" },
		func(filter *verifySimulationAssignmentFilterV2) { filter.PlanHash = "0x" + strings.Repeat("44", 32) },
		func(filter *verifySimulationAssignmentFilterV2) { filter.ChainID++ },
		func(filter *verifySimulationAssignmentFilterV2) { filter.GenesisHash = "0x" + strings.Repeat("55", 32) },
		func(filter *verifySimulationAssignmentFilterV2) { filter.Netuid++ },
		func(filter *verifySimulationAssignmentFilterV2) {
			filter.Coordinator = "0x2222222222222222222222222222222222222222"
		},
		func(filter *verifySimulationAssignmentFilterV2) { filter.OperatorNo++ },
	}
	for index, mutate := range mutations {
		candidate := cloneVerifySimulationAssignmentFilterV2(base)
		mutate(&candidate)
		if err := writeVerifySimulationAssignmentFilterV2(fixture.Path, candidate); err != nil {
			t.Fatal(err)
		}
		if _, err := verifySimulationAssignmentExclusions(fixture.Validators[0]); err == nil {
			t.Fatalf("foreign identity mutation %d was accepted", index)
		}
	}
}

func TestVerifySimulationAssignmentFilterV2IsolatedAcrossDeployments(t *testing.T) {
	fixture := newVerifySimulationAssignmentFilterV2TestFixture(t)
	fixture.Filter.Rules = []verifySimulationAssignmentFilterRuleV2{{
		RuleID: "valid-rule", ValidatorVPKs: verifySimulationAssignmentFilterTestEncodedVPKs(fixture.Validators[0]),
		ExcludedClientIDs: verifySimulationAssignmentFilterTestEncodedClientIDs(fixture.ClientIDs[0]),
	}}
	if err := writeVerifySimulationAssignmentFilterV2(fixture.Path, fixture.Filter); err != nil {
		t.Fatal(err)
	}
	if _, err := verifySimulationAssignmentExclusions(fixture.Validators[0]); err != nil {
		t.Fatalf("source deployment rejected its filter: %v", err)
	}
	foreignConfig := *fixture.Config
	foreignConfig.DeploymentId = "foreign-deployment"
	foreignConfig.ContractAddress = common.HexToAddress("0x3333333333333333333333333333333333333333")
	SetStConfig(&foreignConfig)
	if _, err := verifySimulationAssignmentExclusions(fixture.Validators[0]); err == nil {
		t.Fatal("foreign deployment consumed another deployment's filter")
	}
}

func TestVerifySimulationAssignmentFilterRejectsLeafSymlink(t *testing.T) {
	fixture := newVerifySimulationAssignmentFilterV2TestFixture(t)
	fixture.Filter.Rules = []verifySimulationAssignmentFilterRuleV2{{
		RuleID: "valid-rule", ValidatorVPKs: verifySimulationAssignmentFilterTestEncodedVPKs(fixture.Validators[0]),
		ExcludedClientIDs: verifySimulationAssignmentFilterTestEncodedClientIDs(fixture.ClientIDs[0]),
	}}
	targetPath := filepath.Join(filepath.Dir(fixture.Path), "target-filter.json")
	if err := writeVerifySimulationAssignmentFilterV2(targetPath, fixture.Filter); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(targetPath, fixture.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := verifySimulationAssignmentExclusions(fixture.Validators[0]); err == nil {
		t.Fatal("symbolic-link filter leaf was followed")
	}
}

func TestVerifySimulationAssignmentFilterRejectsParentSymlink(t *testing.T) {
	fixture := newVerifySimulationAssignmentFilterV2TestFixture(t)
	fixture.Filter.Rules = []verifySimulationAssignmentFilterRuleV2{{
		RuleID: "valid-rule", ValidatorVPKs: verifySimulationAssignmentFilterTestEncodedVPKs(fixture.Validators[0]),
		ExcludedClientIDs: verifySimulationAssignmentFilterTestEncodedClientIDs(fixture.ClientIDs[0]),
	}}
	root := verifySimulationAssignmentFilterTestTempDir(t)
	realDirectory := filepath.Join(root, "real")
	if err := os.Mkdir(realDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeVerifySimulationAssignmentFilterV2(filepath.Join(realDirectory, "filter.json"), fixture.Filter); err != nil {
		t.Fatal(err)
	}
	symlinkDirectory := filepath.Join(root, "linked")
	if err := os.Symlink(realDirectory, symlinkDirectory); err != nil {
		t.Fatal(err)
	}
	t.Setenv(VerifySimulationAssignmentFilterFileEnv, filepath.Join(symlinkDirectory, "filter.json"))
	if _, err := verifySimulationAssignmentExclusions(fixture.Validators[0]); err == nil {
		t.Fatal("symbolic-link filter parent was followed")
	}
}

func TestVerifySimulationAssignmentFilterAtomicReplacementPinsOpenedDescriptor(t *testing.T) {
	fixture := newVerifySimulationAssignmentFilterV2TestFixture(t)
	first := fixture.Filter
	first.Rules = []verifySimulationAssignmentFilterRuleV2{{
		RuleID: "valid-rule", ValidatorVPKs: verifySimulationAssignmentFilterTestEncodedVPKs(fixture.Validators[0]),
		ExcludedClientIDs: verifySimulationAssignmentFilterTestEncodedClientIDs(fixture.ClientIDs[0]),
	}}
	second := fixture.Filter
	second.Rules = []verifySimulationAssignmentFilterRuleV2{{
		RuleID: "valid-rule", ValidatorVPKs: verifySimulationAssignmentFilterTestEncodedVPKs(fixture.Validators[0]),
		ExcludedClientIDs: verifySimulationAssignmentFilterTestEncodedClientIDs(fixture.ClientIDs[1]),
	}}
	if err := writeVerifySimulationAssignmentFilterV2(fixture.Path, first); err != nil {
		t.Fatal(err)
	}
	opened, absent, err := verifySimulationOpenAssignmentFilterNoFollow(fixture.Path)
	if err != nil || absent {
		t.Fatalf("secure open absent=%t error=%v", absent, err)
	}
	defer opened.Close()
	if err := atomicWriteVerifySimulationAssignmentFilterV2(fixture.Path, second); err != nil {
		t.Fatal(err)
	}
	openedBytes, err := io.ReadAll(opened)
	if err != nil {
		t.Fatal(err)
	}
	openedExclusions, err := verifySimulationAssignmentExclusionsV2(openedBytes, fixture.Validators[0])
	if err != nil || !verifySimulationAssignmentFilterTestExclusionsMatch(openedExclusions, fixture.ClientIDs[0]) {
		t.Fatalf("opened descriptor exclusions=%v error=%v", openedExclusions, err)
	}
	currentExclusions, err := verifySimulationAssignmentExclusions(fixture.Validators[0])
	if err != nil || !verifySimulationAssignmentFilterTestExclusionsMatch(currentExclusions, fixture.ClientIDs[1]) {
		t.Fatalf("replacement path exclusions=%v error=%v", currentExclusions, err)
	}
}

func TestVerifySimulationAssignmentFilterV2ConcurrentReadsObserveWholeAtomicReloads(t *testing.T) {
	fixture := newVerifySimulationAssignmentFilterV2TestFixture(t)
	first := fixture.Filter
	first.Rules = []verifySimulationAssignmentFilterRuleV2{
		{RuleID: "fleet-lifecycle-target-prune", ValidatorVPKs: verifySimulationAssignmentFilterTestEncodedVPKs(fixture.Validators[0], fixture.Validators[1]), ExcludedClientIDs: verifySimulationAssignmentFilterTestEncodedClientIDs(fixture.ClientIDs[0])},
		{RuleID: "validator-local-head-boundary", ValidatorVPKs: verifySimulationAssignmentFilterTestEncodedVPKs(fixture.Validators[0]), ExcludedClientIDs: verifySimulationAssignmentFilterTestEncodedClientIDs(fixture.ClientIDs[1])},
	}
	second := fixture.Filter
	second.Rules = []verifySimulationAssignmentFilterRuleV2{
		{RuleID: "fleet-lifecycle-companion-prune", ValidatorVPKs: verifySimulationAssignmentFilterTestEncodedVPKs(fixture.Validators[0], fixture.Validators[1]), ExcludedClientIDs: verifySimulationAssignmentFilterTestEncodedClientIDs(fixture.ClientIDs[2])},
		{RuleID: "validator-local-head-boundary", ValidatorVPKs: verifySimulationAssignmentFilterTestEncodedVPKs(fixture.Validators[0]), ExcludedClientIDs: verifySimulationAssignmentFilterTestEncodedClientIDs(fixture.ClientIDs[3])},
	}
	if err := atomicWriteVerifySimulationAssignmentFilterV2(fixture.Path, first); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errors := make(chan error, 1)
	var waitGroup sync.WaitGroup
	report := func(err error) {
		select {
		case errors <- err:
		default:
		}
	}
	waitGroup.Add(1)
	go func() {
		defer waitGroup.Done()
		<-start
		for iteration := 0; iteration < 300; iteration++ {
			if iteration%3 == 2 {
				if err := os.Remove(fixture.Path); err != nil {
					report(fmt.Errorf("atomic removal %d: %w", iteration, err))
					return
				}
				continue
			}
			filter := first
			if iteration%3 == 1 {
				filter = second
			}
			if err := atomicWriteVerifySimulationAssignmentFilterV2(fixture.Path, filter); err != nil {
				report(fmt.Errorf("atomic reload %d: %w", iteration, err))
				return
			}
		}
	}()
	for reader := 0; reader < 8; reader++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			<-start
			for iteration := 0; iteration < 200; iteration++ {
				firstValidator, err := verifySimulationAssignmentExclusions(fixture.Validators[0])
				if err != nil {
					report(err)
					return
				}
				firstSnapshot := verifySimulationAssignmentFilterTestExclusionsMatch(firstValidator, fixture.ClientIDs[0], fixture.ClientIDs[1])
				secondSnapshot := verifySimulationAssignmentFilterTestExclusionsMatch(firstValidator, fixture.ClientIDs[2], fixture.ClientIDs[3])
				if len(firstValidator) != 0 && !firstSnapshot && !secondSnapshot {
					report(fmt.Errorf("reader observed mixed first-validator snapshot %v", firstValidator))
					return
				}
				secondValidator, err := verifySimulationAssignmentExclusions(fixture.Validators[1])
				if err != nil {
					report(err)
					return
				}
				if len(secondValidator) != 0 && !verifySimulationAssignmentFilterTestExclusionsMatch(secondValidator, fixture.ClientIDs[0]) && !verifySimulationAssignmentFilterTestExclusionsMatch(secondValidator, fixture.ClientIDs[2]) {
					report(fmt.Errorf("reader observed mixed second-validator snapshot %v", secondValidator))
					return
				}
			}
		}()
	}
	close(start)
	waitGroup.Wait()
	select {
	case err := <-errors:
		t.Fatal(err)
	default:
	}
}
