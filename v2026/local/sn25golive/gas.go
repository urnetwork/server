package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
)

const approverKeySchema = "urnetwork-operator-gas-approver-key-v1"

// The server signs at most this many attempts per transaction intent
// (controller stTxMaxAttempts); a larger intent allowance is never usable.
const serverMaximumIntentAttempts = 3

// The census digest of an account with no retained transaction intent: the
// framing header alone (model stGasHistoryHashStart). Admission recomputes the
// census from the database and refuses a pin that differs.
var emptyAccountHistorySha256 = func() string {
	digest := sha256.Sum256([]byte("urnetwork-operator-gas-history-v1\n"))
	return hex.EncodeToString(digest[:])
}()

type approverKeyFile struct {
	Schema    string `json:"schema"`
	PublicKey string `json:"public_key"`
	Seed      string `json:"seed"`
}

func runGasApproverKeygen(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("gas-approver-keygen", flag.ContinueOnError)
	out := flags.String("out", "", "new approver key file (created 0600, never overwritten)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *out == "" {
		return errors.New("--out is required and no positional arguments are accepted")
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(approverKeyFile{Schema: approverKeySchema, PublicKey: hex.EncodeToString(public), Seed: hex.EncodeToString(private.Seed())}, "", "  ")
	if err != nil {
		return err
	}
	if err := writeNewFile(*out, append(encoded, '\n'), 0o600); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "approver_public_key: %s\nkey_file: %s (mode 0600)\n", hex.EncodeToString(public), *out)
	fmt.Fprintln(stdout, "The first admitted policy makes this key the permanent trust root of its scope st-operator-gas-v1:<chain>:<genesis>:<no_id>.")
	return nil
}

func loadApproverKey(path string) (ed25519.PrivateKey, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("approver key %s must be a regular file readable only by its owner (mode %04o)", path, info.Mode().Perm())
	}
	data, err := readBounded(path, 4096)
	if err != nil {
		return nil, err
	}
	var file approverKeyFile
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		return nil, errors.New("approver key file is not an approver key document")
	}
	seed, err := hex.DecodeString(file.Seed)
	if file.Schema != approverKeySchema || err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("approver key file has the wrong schema or seed size")
	}
	key := ed25519.NewKeyFromSeed(seed)
	if hex.EncodeToString(key.Public().(ed25519.PublicKey)) != file.PublicKey {
		return nil, errors.New("approver key file public key does not match its seed")
	}
	return key, nil
}

// PARAMS.json is the unsigned policy itself, decoded strictly into the
// server's type. The server reads the policy through Clone, which turns an
// empty historical census into nil, so the signed JSON carries null there.
func loadGasParams(path string) (*server.StOperatorGasPolicy, error) {
	data, err := readBounded(path, 64*1024)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var params server.StOperatorGasPolicy
	if err := decoder.Decode(&params); err != nil {
		return nil, fmt.Errorf("params: %w", err)
	}
	if decoder.Decode(new(json.RawMessage)) != io.EOF {
		return nil, errors.New("params must hold exactly one JSON object")
	}
	if params.Signature != "" {
		return nil, errors.New("params must be unsigned (signature \"\")")
	}
	policy := params.Clone()
	if _, err := controller.StOperatorGasPolicyApprovalRequest(policy); err != nil {
		return nil, fmt.Errorf("params: %w", err)
	}
	return policy, nil
}

// Go-live refusals beyond the server's Validate, and notes for the reviewer.
func reviewGasPolicy(policy *server.StOperatorGasPolicy, now time.Time) (notes []string, err error) {
	if policy.MaximumIntentAttempts > serverMaximumIntentAttempts {
		return nil, fmt.Errorf("maximum_intent_attempts %d exceeds the server's %d attempts per intent", policy.MaximumIntentAttempts, serverMaximumIntentAttempts)
	}
	if now.Unix() >= policy.ValidUntil {
		return nil, fmt.Errorf("the validity window ended at %s", unixUtc(policy.ValidUntil))
	}
	if now.Unix() < policy.ValidFrom {
		notes = append(notes, fmt.Sprintf("signing is refused until valid_from %s", unixUtc(policy.ValidFrom)))
	}
	for _, account := range policy.AllAccounts() {
		if account.InitialHistorySha256 == emptyAccountHistorySha256 {
			notes = append(notes, fmt.Sprintf("%s %s pins an empty original history (no retained intent in the database)", account.Role, account.Address))
		} else {
			notes = append(notes, fmt.Sprintf("%s %s pins a nonempty original history; admission recomputes it from the database", account.Role, account.Address))
		}
	}
	return notes, nil
}

func runGasPolicy(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("gas-policy", flag.ContinueOnError)
	paramsPath := flags.String("params", "", "unsigned policy JSON (the approval request)")
	keyPath := flags.String("approver-key", "", "approver key file from gas-approver-keygen")
	outPolicy := flags.String("out-policy", "", "new st.yml fragment holding the signed operator_gas_policy")
	outAuthority := flags.String("out-authority", "", "new config/<env>/operator-gas-authority.yml")
	dryRun := flags.Bool("dry-run", false, "validate the params and print the digest the approval covers; write nothing")
	nowValue := flags.String("now", "", "RFC3339 instant for the validity checks (default: now)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("no positional arguments are accepted")
	}
	now, err := parseNow(*nowValue)
	if err != nil {
		return err
	}
	policy, err := loadGasParams(*paramsPath)
	if err != nil {
		return err
	}
	notes, err := reviewGasPolicy(policy, now)
	if err != nil {
		return err
	}
	digest, err := policy.Digest()
	if err != nil {
		return err
	}
	if *dryRun {
		if *keyPath != "" || *outPolicy != "" || *outAuthority != "" {
			return errors.New("--dry-run takes no key or outputs")
		}
		printGasSummary(stdout, policy, digest, "", notes)
		return nil
	}
	if *keyPath == "" || *outPolicy == "" || *outAuthority == "" {
		return errors.New("--approver-key, --out-policy and --out-authority are required")
	}
	key, err := loadApproverKey(*keyPath)
	if err != nil {
		return err
	}
	signing, err := policy.SigningBytes()
	if err != nil {
		return err
	}
	policy.Signature = hex.EncodeToString(ed25519.Sign(key, signing))
	authority := &server.StOperatorGasAuthority{Schema: server.StOperatorGasAuthoritySchema, Profile: policy.Profile, ChainId: policy.ChainId,
		GenesisHash: policy.GenesisHash, NoId: policy.NoId, ApproverPublicKey: hex.EncodeToString(key.Public().(ed25519.PublicKey)), PolicySha256: digest}
	policyBytes, err := encodeGasPolicyFragment(policy, authority)
	if err != nil {
		return err
	}
	authorityBytes, err := encodeGasAuthority(authority)
	if err != nil {
		return err
	}
	if err := verifyGasOutputs(policyBytes, authorityBytes, digest, now); err != nil {
		return fmt.Errorf("signed outputs failed the server's verification: %w", err)
	}
	if err := writeNewFile(*outPolicy, policyBytes, 0o644); err != nil {
		return err
	}
	if err := writeNewFile(*outAuthority, authorityBytes, 0o644); err != nil {
		os.Remove(*outPolicy)
		return err
	}
	printGasSummary(stdout, policy, digest, authority.ApproverPublicKey, notes)
	fmt.Fprintf(stdout, "out_policy: %s\nout_authority: %s\n", *outPolicy, *outAuthority)
	return nil
}

func printGasSummary(stdout io.Writer, policy *server.StOperatorGasPolicy, digest, approver string, notes []string) {
	fmt.Fprintf(stdout, "policy_sha256: %s\n", digest)
	if approver != "" {
		fmt.Fprintf(stdout, "approver_public_key: %s\n", approver)
	}
	fmt.Fprintf(stdout, "scope: %s\n", policy.Scope())
	fmt.Fprintf(stdout, "valid: %s .. %s\n", unixUtc(policy.ValidFrom), unixUtc(policy.ValidUntil))
	fmt.Fprintf(stdout, "limits: gas %d, fee %s wei/gas, tip %s wei/gas, intent %s wei, lifetime %s wei, attempts %d/intent %d/lifetime\n",
		policy.MaximumGas, policy.MaximumFeePerGasWei, policy.MaximumTipPerGasWei, policy.MaximumIntentLiabilityWei, policy.MaximumLifetimeLiabilityWei,
		policy.MaximumIntentAttempts, policy.MaximumLifetimeAttempts)
	for _, note := range notes {
		fmt.Fprintf(stdout, "note: %s\n", note)
	}
}

// Every string is double-quoted: a YAML 1.1 reader (PyYAML, for one) would
// otherwise read a plain 0x-prefixed hash or address as an integer.
func yamlEncode(value any) ([]byte, error) {
	var node yaml.Node
	if err := node.Encode(value); err != nil {
		return nil, err
	}
	var quote func(*yaml.Node)
	quote = func(node *yaml.Node) {
		if node.Kind == yaml.ScalarNode && node.Tag == "!!str" {
			node.Style = yaml.DoubleQuotedStyle
		}
		for index, child := range node.Content {
			if node.Kind == yaml.MappingNode && index%2 == 0 {
				continue
			}
			quote(child)
		}
	}
	quote(&node)
	var buffer bytes.Buffer
	encoder := yaml.NewEncoder(&buffer)
	encoder.SetIndent(2)
	if err := encoder.Encode(&node); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

type gasPolicyFragment struct {
	OperatorGasPolicy *server.StOperatorGasPolicy `yaml:"operator_gas_policy"`
}

func encodeGasPolicyFragment(policy *server.StOperatorGasPolicy, authority *server.StOperatorGasAuthority) ([]byte, error) {
	body, err := yamlEncode(gasPolicyFragment{OperatorGasPolicy: policy})
	if err != nil {
		return nil, err
	}
	header := strings.Join([]string{
		"# Signed operator gas policy: merge this top-level key into st.yml.",
		"# policy_sha256: " + authority.PolicySha256,
		"# approver_public_key: " + authority.ApproverPublicKey,
		"# scope: " + policy.Scope(),
		"# The signature covers JSON with historical_accounts null; the server reads",
		"# this policy through StOperatorGasPolicy.Clone, which maps [] to null.",
	}, "\n")
	return append([]byte(header+"\n"), body...), nil
}

func encodeGasAuthority(authority *server.StOperatorGasAuthority) ([]byte, error) {
	body, err := yamlEncode(authority)
	if err != nil {
		return nil, err
	}
	header := "# Independent pin of the approved operator gas policy (config/<env>/operator-gas-authority.yml).\n"
	return append([]byte(header), body...), nil
}

// The exact output bytes must pass the server's authority parser and the
// policy's Verify after the same decoding and Clone the st.yml loader applies.
func verifyGasOutputs(policyBytes, authorityBytes []byte, digest string, now time.Time) error {
	authority, err := server.ParseStOperatorGasAuthority(authorityBytes)
	if err != nil {
		return err
	}
	strict := yaml.NewDecoder(bytes.NewReader(policyBytes))
	strict.KnownFields(true)
	var fragment gasPolicyFragment
	if err := strict.Decode(&fragment); err != nil {
		return err
	}
	var loaded gasPolicyFragment
	if err := yaml.Unmarshal(policyBytes, &loaded); err != nil {
		return err
	}
	policy := loaded.OperatorGasPolicy.Clone()
	if policy == nil {
		return errors.New("policy fragment has no operator_gas_policy")
	}
	at := now
	if at.Unix() < policy.ValidFrom {
		at = time.Unix(policy.ValidFrom, 0).UTC()
	}
	if err := policy.Verify(authority, at); err != nil {
		return err
	}
	if loadedDigest, err := policy.Digest(); err != nil || loadedDigest != digest || authority.PolicySha256 != digest {
		return errors.New("decoded policy digest differs from the signed digest")
	}
	return nil
}
