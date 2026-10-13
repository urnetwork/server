package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"gopkg.in/yaml.v3"

	"github.com/urnetwork/server"
)

const proposedParams = "sn25-mainnet-gas-params.json"

// The approval digest of the proposed params. A change to any value changes
// what the approver signs, so it must be reviewed together with this pin.
const proposedParamsSha256 = "e2de5acf1ef30de0b5c00f0e9cb5291db1b56b5fe6d0a17a524bb65806b5d64d"

var goLiveNow = time.Date(2026, 10, 8, 15, 0, 0, 0, time.UTC)

// Every resolver home is an empty directory, as main arranges.
func isolateHomes(t *testing.T) {
	t.Helper()
	empty := t.TempDir()
	for _, name := range []string{"WARP_HOME", "WARP_VAULT_HOME", "WARP_CONFIG_HOME", "WARP_SITE_HOME"} {
		t.Setenv(name, empty)
	}
}

func TestEmptyAccountHistoryIsTheServerFramingHeader(t *testing.T) {
	if emptyAccountHistorySha256 != "cf8559f53ee3434bb255a876dc55606cb97a3f91a36bfa8f57407cf0f8e0927c" {
		t.Fatalf("empty account census digest changed: %s", emptyAccountHistorySha256)
	}
}

// The proposed mainnet params validate, sign null for the empty historical
// census, and keep the sizing the go-live review relies on.
func TestProposedMainnetParams(t *testing.T) {
	policy, err := loadGasParams(proposedParams)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reviewGasPolicy(policy, goLiveNow); err != nil {
		t.Fatal(err)
	}
	digest, err := policy.Digest()
	if err != nil || digest != proposedParamsSha256 {
		t.Fatalf("approval digest = %s, %v", digest, err)
	}
	signing, err := policy.SigningBytes()
	if err != nil || !bytes.Contains(signing, []byte(`"historical_accounts":null`)) {
		t.Fatalf("signing bytes must carry a null historical census: %s, %v", signing, err)
	}
	if policy.Profile != "mainnet" || policy.ChainId != 964 || policy.NoId != 1 || policy.Scope() != "st-operator-gas-v1:964:0x2f0555cc76fc2840a25a6ea3b9637146806f1f44b090c175ffde2a7e5ab36c03:1" ||
		policy.Accounts[0].Address != "0x672b0dd503c5066ab2e3fb41b84fc818e646c21b" || policy.Accounts[1].Address != "0xe963314caf8002f23afbf92e67daf9edad37d2a3" ||
		policy.Accounts[0].InitialHistorySha256 != emptyAccountHistorySha256 || policy.Accounts[1].InitialHistorySha256 != emptyAccountHistorySha256 ||
		policy.ValidUntil-policy.ValidFrom != 365*24*60*60 || policy.MaximumIntentAttempts != serverMaximumIntentAttempts {
		t.Fatalf("proposed identity or window changed: %+v", policy)
	}
	quantity := func(value string) *big.Int {
		n, err := server.StOperatorGasQuantity(value)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	// The server's fee cap is 2*base+tip, raised by 9/8 per replacement: the
	// third attempt at the pre-2026-09-18 10 gwei base fee needs 25.3125 gwei.
	thirdAttemptAtTenGwei := big.NewInt(20_000_000_000)
	for range 2 {
		thirdAttemptAtTenGwei.Mul(thirdAttemptAtTenGwei, big.NewInt(9))
		thirdAttemptAtTenGwei.Add(thirdAttemptAtTenGwei, big.NewInt(7))
		thirdAttemptAtTenGwei.Div(thirdAttemptAtTenGwei, big.NewInt(8))
	}
	fee := quantity(policy.MaximumFeePerGasWei)
	if fee.Cmp(thirdAttemptAtTenGwei) < 0 {
		t.Fatalf("fee cap %s is below the third attempt at a 10 gwei base fee (%s)", fee, thirdAttemptAtTenGwei)
	}
	// One attempt may use the whole gas and fee envelope.
	envelope := new(big.Int).Mul(new(big.Int).SetUint64(policy.MaximumGas), fee)
	if quantity(policy.MaximumIntentLiabilityWei).Cmp(envelope) < 0 {
		t.Fatalf("intent liability is below one full envelope %s", envelope)
	}
	// The largest server-signed testnet envelope was the 333,688 gas deposit.
	if policy.MaximumGas < 333_688 {
		t.Fatalf("maximum gas %d is below the measured deposit envelope", policy.MaximumGas)
	}
}

func writeTestParams(t *testing.T, dir string, edit func(map[string]any)) string {
	t.Helper()
	data, err := os.ReadFile(proposedParams)
	if err != nil {
		t.Fatal(err)
	}
	var params map[string]any
	if err := json.Unmarshal(data, &params); err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(params)
	}
	encoded, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "params.json")
	os.Remove(path)
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func newApproverKey(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "approver.json")
	var stdout bytes.Buffer
	if err := runGasApproverKeygen([]string{"--out", path}, &stdout); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("approver key mode: %v, %v", info, err)
	}
	return path
}

func TestGasPolicySignsOutputsTheServerVerifies(t *testing.T) {
	isolateHomes(t)
	dir := t.TempDir()
	key := newApproverKey(t, dir)
	params := writeTestParams(t, dir, nil)
	policyPath, authorityPath := filepath.Join(dir, "policy.yml"), filepath.Join(dir, "authority.yml")
	args := []string{"--params", params, "--approver-key", key, "--out-policy", policyPath, "--out-authority", authorityPath, "--now", goLiveNow.Format(time.RFC3339)}
	var stdout bytes.Buffer
	if err := runGasPolicy(args, &stdout); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "policy_sha256: "+proposedParamsSha256) {
		t.Fatalf("signed digest differs from the dry-run approval digest:\n%s", stdout.String())
	}
	policyBytes, err := os.ReadFile(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	authorityBytes, err := os.ReadFile(authorityPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyGasOutputs(policyBytes, authorityBytes, proposedParamsSha256, goLiveNow); err != nil {
		t.Fatal(err)
	}
	authority, err := server.ParseStOperatorGasAuthority(authorityBytes)
	if err != nil || authority.PolicySha256 != proposedParamsSha256 {
		t.Fatalf("authority pin: %+v, %v", authority, err)
	}
	if err := runGasPolicy(args, &stdout); err == nil {
		t.Fatal("existing outputs were overwritten")
	}
	tampered := bytes.Replace(policyBytes, []byte("maximum_gas: 1000000"), []byte("maximum_gas: 2000000"), 1)
	if bytes.Equal(tampered, policyBytes) || verifyGasOutputs(tampered, authorityBytes, proposedParamsSha256, goLiveNow) == nil {
		t.Fatal("a changed signed limit still verified")
	}
	if err := os.Chmod(key, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadApproverKey(key); err == nil {
		t.Fatal("a group-readable approver key was accepted")
	}
}

func TestGasParamsRefusals(t *testing.T) {
	dir := t.TempDir()
	emptyList := writeTestParams(t, dir, func(params map[string]any) { params["historical_accounts"] = []any{} })
	policy, err := loadGasParams(emptyList)
	if err != nil {
		t.Fatal(err)
	}
	if digest, _ := policy.Digest(); digest != proposedParamsSha256 {
		t.Fatalf("an empty historical list must sign as null: %s", digest)
	}
	for name, edit := range map[string]func(map[string]any){
		"signed":   func(params map[string]any) { params["signature"] = strings.Repeat("00", 64) },
		"unknown":  func(params map[string]any) { params["maximum_gas_wei"] = "1" },
		"checksum": func(params map[string]any) { params["coordinator"] = "0xC18925925E2B7bb9059b7d696b8c92762AE86406" },
		"order": func(params map[string]any) {
			accounts := params["accounts"].([]any)
			accounts[0], accounts[1] = accounts[1], accounts[0]
		},
	} {
		if _, err := loadGasParams(writeTestParams(t, dir, edit)); err == nil {
			t.Fatalf("%s params were accepted", name)
		}
	}
	attempts := writeTestParams(t, dir, func(params map[string]any) {
		params["maximum_intent_attempts"] = 4
	})
	policy, err = loadGasParams(attempts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reviewGasPolicy(policy, goLiveNow); err == nil {
		t.Fatal("more than the server's three attempts per intent was accepted")
	}
	if _, err := reviewGasPolicy(policy, time.Unix(policy.ValidUntil, 0)); err == nil {
		t.Fatal("an expired window was accepted")
	}
}

// The server's own verify.yml loaders accept the generated file. They run
// once per process, so this is the only successful verify-config here.
func TestVerifyConfigPassesTheServerLoaders(t *testing.T) {
	isolateHomes(t)
	policyPath := filepath.Join("..", "..", "..", "sn", "deploy", "mainnet", "policy-v1.yml")
	if _, err := os.Stat(policyPath); err != nil {
		t.Skipf("sibling sn checkout policy is unavailable: %v", err)
	}
	const policyHash = "0x6b188830b47e3b7dbfafc2839d7e1f460125115c9fbded053fa46293c79130a2"
	dir := t.TempDir()
	out := filepath.Join(dir, "verify.yml")
	var stdout bytes.Buffer
	if err := runVerifyConfig([]string{"--policy", policyPath, "--expect-policy-hash", "0x" + strings.Repeat("00", 32), "--out", out}, &stdout); err == nil {
		t.Fatal("a different expected policy hash was accepted")
	}
	if err := runVerifyConfig([]string{"--policy", policyPath, "--expect-policy-hash", policyHash, "--out", out}, &stdout); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(out)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("verify.yml mode: %v, %v", info, err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Profile       string          `yaml:"profile"`
		PolicyHash    string          `yaml:"policy_hash"`
		EgressHashKey string          `yaml:"egress_hash_key"`
		Keys          []verifyFileKey `yaml:"keys"`
		Settings      map[string]any  `yaml:"settings"`
	}
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	egress, egressErr := base64.StdEncoding.DecodeString(document.EgressHashKey)
	if document.Profile != "mainnet" || document.PolicyHash != policyHash || egressErr != nil || len(egress) != 32 || len(document.Keys) != 1 || document.Keys[0].ServerKeyId != 0 ||
		document.Settings["egress_hash_key_id"] != "mainnet-v1" || document.Settings["trail_depth"] != 8 || len(document.Settings) != 15 {
		t.Fatalf("verify.yml content: %+v", document)
	}
	seed, err := base64.StdEncoding.DecodeString(document.Keys[0].Seed)
	if err != nil || len(seed) != ed25519.SeedSize {
		t.Fatal("verify seed is not a base64 Ed25519 seed")
	}
	public := hex.EncodeToString(ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey))
	if !strings.Contains(stdout.String(), "verify_public_key_hex: "+public) || strings.Contains(stdout.String(), document.Keys[0].Seed) || strings.Contains(stdout.String(), document.EgressHashKey) {
		t.Fatalf("stdout must name the public key and no secret:\n%s", stdout.String())
	}
}

func goLiveTestKey(t *testing.T) (*ecdsa.PrivateKey, string) {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return key, strings.ToLower(crypto.PubkeyToAddress(key.PublicKey).Hex())
}

const goLiveStYml = `authority: "{{ env: BRINGYOUR_SUBTENSOR_HOSTNAME }}"
enabled: false
chain_id: 964
genesis_hash: "0x2f0555cc76fc2840a25a6ea3b9637146806f1f44b090c175ffde2a7e5ab36c03"
deployment_id: "sn25-mainnet-20261007-01"
policy_hash: "0x6b188830b47e3b7dbfafc2839d7e1f460125115c9fbded053fa46293c79130a2"
coordinator_address: "0xC18925925E2B7bb9059b7d696b8c92762AE86406"
settlement_vault_address: "0x98BF47ba01828676855B5ED10F2f22a867F0cB59"
reserve_sink_address: "0x134a70Cbe45eA14324536feca4d6F08FDDee9245"
netuid: 25
no_id: 1
deposit_hotkey: "0xc04a955735900f4b981952f5a985ad765892b58bd399717a337d8800fb1da907"
deposit_tiers:
  - {min_conviction_rao: 0, rate_numerator_rao_per_gib: 0, rate_denominator: 1}
  - {min_conviction_rao: 1000000000000, rate_numerator_rao_per_gib: 0, rate_denominator: 1}
deposit_zero_rate_action: "equal_demand"
deposit_epoch_cap_rao: 10000000000
deploy_block: 9239500
launch_readiness_sha256: "%s"
deposit_key: "%s"
root_key: "%s"
artifact_key: "%s"
`

const goLiveSnYml = `schema: urnetwork-provider-payout-transition-v1
cutoff_utc: "2026-10-06T00:00:00Z"
attribution: settled_contract_close_time
legacy_usdc: finish_pre_cutoff_obligations
mainnet:
  profile: mainnet
  chain_id: 964
  genesis_hash: "0x2f0555cc76fc2840a25a6ea3b9637146806f1f44b090c175ffde2a7e5ab36c03"
  netuid: 25
  activation: reviewed
  deployment_id: "sn25-mainnet-20261007-01"
  coordinator: "0xC18925925E2B7bb9059b7d696b8c92762AE86406"
  settlement_vault: "0x98BF47ba01828676855B5ED10F2f22a867F0cB59"
  policy_hash: "0x6b188830b47e3b7dbfafc2839d7e1f460125115c9fbded053fa46293c79130a2"
  readiness_sha256: "%s"
`

// The readiness digest goLiveStCheck writes into st.yml.
var goLiveReadiness = strings.Repeat("7e", 32)

// goLiveStCheck writes the go-live st.yml with fresh role keys, the top-level
// lines stExtra and an operator gas policy signed for those keys. It returns a
// run of st-check, with any further arguments, against an sn.yml with a
// readiness digest and the mainnet lines snExtra, and the deposit key's hex,
// which must never be printed.
func goLiveStCheck(t *testing.T, stExtra string, snExtra string) (func(readiness string, args ...string) (string, error), string) {
	t.Helper()
	isolateHomes(t)
	dir := t.TempDir()
	depositKey, deposit := goLiveTestKey(t)
	rootKey, root := goLiveTestKey(t)
	artifactKey, _ := goLiveTestKey(t)
	params := writeTestParams(t, dir, func(params map[string]any) {
		accounts := params["accounts"].([]any)
		accounts[0].(map[string]any)["address"] = deposit
		accounts[1].(map[string]any)["address"] = root
	})
	key := newApproverKey(t, dir)
	policyPath, authorityPath := filepath.Join(dir, "policy.yml"), filepath.Join(dir, "authority.yml")
	var stdout bytes.Buffer
	if err := runGasPolicy([]string{"--params", params, "--approver-key", key, "--out-policy", policyPath, "--out-authority", authorityPath, "--now", goLiveNow.Format(time.RFC3339)}, &stdout); err != nil {
		t.Fatal(err)
	}
	fragment, err := os.ReadFile(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	keyHex := func(key *ecdsa.PrivateKey) string { return hex.EncodeToString(crypto.FromECDSA(key)) }
	stPath, snPath := filepath.Join(dir, "st.yml"), filepath.Join(dir, "sn.yml")
	stYml := []byte(fmt.Sprintf(goLiveStYml, goLiveReadiness, keyHex(depositKey), keyHex(rootKey), keyHex(artifactKey)) + stExtra)
	if err := os.WriteFile(stPath, append(stYml, fragment...), 0o600); err != nil {
		t.Fatal(err)
	}
	check := func(readiness string, args ...string) (string, error) {
		os.Remove(snPath)
		if err := os.WriteFile(snPath, []byte(fmt.Sprintf(goLiveSnYml, readiness)+snExtra), 0o644); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		err := runStCheck(append([]string{"--st", stPath, "--authority", authorityPath, "--sn", snPath, "--now", goLiveNow.Format(time.RFC3339)}, args...), &out)
		return out.String(), err
	}
	return check, keyHex(depositKey)
}

// The go-live st.yml values pass the server's validation, the signed policy
// verifies against its authority and the reviewed sn.yml identity matches.
func TestStCheckAcceptsTheGoLiveValues(t *testing.T) {
	check, depositKey := goLiveStCheck(t, "", "")
	out, err := check(goLiveReadiness)
	if err != nil || !strings.HasSuffix(out, "result: ok\n") || strings.Contains(out, depositKey) {
		t.Fatalf("go-live values were refused: %v\n%s", err, out)
	}
	for _, want := range []string{"enabled is false in the file", "signature and digest match the authority", "sn.yml reviewed identity and readiness digest equal"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
	if out, err := check(strings.Repeat("7f", 32)); err == nil || !strings.Contains(out, "FAIL: sn.yml payout admission") {
		t.Fatalf("a different sn.yml readiness digest was accepted: %v\n%s", err, out)
	}
}

// st-check prints the v2 pair st.yml declares as one identity line, in the
// lowercase form st.yml and sn.yml declare, and no v2 line without it.
func TestStCheckPrintsTheV2Addresses(t *testing.T) {
	const settlement, vault = "0x00000000000000000000000000000000000000a1", "0x00000000000000000000000000000000000000a2"
	check, _ := goLiveStCheck(t,
		fmt.Sprintf("settlement_v2_address: %q\nvault_v2_address: %q\n", settlement, vault),
		fmt.Sprintf("  settlement_v2_address: %q\n  vault_v2_address: %q\n", settlement, vault))
	out, err := check(goLiveReadiness)
	if err != nil || !strings.HasSuffix(out, "result: ok\n") || !strings.Contains(out, "\nidentity: settlement_v2_address "+settlement+", vault_v2_address "+vault+"\n") {
		t.Fatalf("the v2 pair was not printed: %v\n%s", err, out)
	}
	check, _ = goLiveStCheck(t, "", "")
	if out, err := check(goLiveReadiness); err != nil || strings.Contains(out, "v2_address") {
		t.Fatalf("a v2 line without the v2 pair: %v\n%s", err, out)
	}
}

// st-check shows the server's activation latch from its `bringyourctl st
// v2-activation` output and checks the candidate st.yml against it (SV-9): an
// armed latch passes an st.yml naming its recorded pair and fails any other
// with the refusal the server would give; a latch that is not armed is noted;
// without the output, or with one of another deployment or of another shape,
// nothing is taken as checked.
func TestStCheckChecksTheV2Activation(t *testing.T) {
	const settlement, vault = "0x00000000000000000000000000000000000000a1", "0x00000000000000000000000000000000000000a2"
	const deployment = "964:0xc18925925e2b7bb9059b7d696b8c92762ae86406"
	dir := t.TempDir()
	status := func(name string, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	armed := status("armed.json", fmt.Sprintf(`{"deployment_key":%q,"armed":true,"no_id":1,"first_epoch":7,"vault_v2_address":%q,"settlement_v2_address":%q,"recorded_at":"2026-10-13T20:00:00Z"}`+"\n", deployment, vault, settlement))
	unarmed := status("unarmed.json", fmt.Sprintf(`{"deployment_key":%q,"armed":false}`+"\n", deployment))
	other := status("other.json", `{"deployment_key":"945:0x2000000000000000000000000000000000000002","armed":false}`)
	shaped := status("shaped.json", `{"ledger_v2_activation":{"armed":false}}`)
	const refusal = "FAIL: v2 activation: st: settlement v2 is active from epoch 7 (pool 1), but st.yml is missing the recorded settlement_v2_address " +
		settlement + " and vault_v2_address " + vault + " (st subsystem disabled)\n"

	withPair, _ := goLiveStCheck(t,
		fmt.Sprintf("settlement_v2_address: %q\nvault_v2_address: %q\n", settlement, vault),
		fmt.Sprintf("  settlement_v2_address: %q\n  vault_v2_address: %q\n", settlement, vault))
	if out, err := withPair(goLiveReadiness, "--v2-activation", armed); err != nil || !strings.HasSuffix(out, "result: ok\n") ||
		!strings.Contains(out, "\nok: v2 activation: the latch is armed (pool 1 from epoch 7) and st.yml names its recorded v2 pair\n") {
		t.Fatalf("the recorded pair against an armed latch: %v\n%s", err, out)
	}

	withoutPair, _ := goLiveStCheck(t, "", "")
	if out, err := withoutPair(goLiveReadiness, "--v2-activation", armed); err == nil || err.Error() != "1 check(s) failed" || !strings.Contains(out, "\n"+refusal) {
		t.Fatalf("no pair against an armed latch: %v\n%s", err, out)
	}
	if out, err := withoutPair(goLiveReadiness, "--v2-activation", unarmed); err != nil || !strings.HasSuffix(out, "result: ok\n") ||
		!strings.Contains(out, "\nnote: v2 activation: the latch is not armed: the server has recorded no activation and would not refuse an st.yml without the v2 pair\n") {
		t.Fatalf("no pair against a latch that is not armed: %v\n%s", err, out)
	}
	if out, err := withoutPair(goLiveReadiness); err != nil || !strings.HasSuffix(out, "result: ok\n") ||
		!strings.Contains(out, "\nnote: no --v2-activation given: st.yml was not checked against the server's recorded settlement v2 activation\n") {
		t.Fatalf("without the server's output: %v\n%s", err, out)
	}
	for name, test := range map[string]struct{ path, want string }{
		"another deployment": {other, `FAIL: v2 activation: st: the activation status is of deployment "945:0x2000000000000000000000000000000000000002", not st.yml's "` + deployment + `"` + "\n"},
		"another shape":      {shaped, "FAIL: v2 activation: not the output of bringyourctl st v2-activation: "},
		"a missing file":     {filepath.Join(dir, "missing.json"), "FAIL: v2 activation: open "},
	} {
		if out, err := withoutPair(goLiveReadiness, "--v2-activation", test.path); err == nil || !strings.Contains(out, "\n"+test.want) {
			t.Fatalf("%s: %v\n%s", name, err, out)
		}
	}
}
