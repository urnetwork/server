package main

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"gopkg.in/yaml.v3"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	stconn "github.com/urnetwork/server/st"
)

type stCheckReport struct {
	out      io.Writer
	failures int
}

func (self *stCheckReport) add(kind string, format string, args ...any) {
	fmt.Fprintf(self.out, "%s: %s\n", kind, fmt.Sprintf(format, args...))
	if kind == "FAIL" {
		self.failures++
	}
}

func runStCheck(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("st-check", flag.ContinueOnError)
	stPath := flags.String("st", "", "candidate st.yml (holds private keys: read, never printed)")
	profile := flags.String("profile", stconn.ProfileMainnet, "profile to select: mainnet or testnet")
	authorityPath := flags.String("authority", "", "operator-gas-authority.yml to verify the embedded policy against")
	snPath := flags.String("sn", "", "sn.yml to check the payout identity and readiness digest against")
	nowValue := flags.String("now", "", "RFC3339 instant for policy validity (default: now)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *stPath == "" {
		return errors.New("--st is required and no positional arguments are accepted")
	}
	if *profile != stconn.ProfileMainnet && *profile != stconn.ProfileTestnet {
		return errors.New("--profile must be mainnet or testnet")
	}
	now, err := parseNow(*nowValue)
	if err != nil {
		return err
	}
	raw, err := readBounded(*stPath, 1<<20)
	if err != nil {
		return err
	}
	report := &stCheckReport{out: stdout}
	var top map[string]any
	if err := yaml.Unmarshal(raw, &top); err != nil || top == nil {
		return errors.New("st.yml is not one YAML mapping")
	}
	rpcUrls, err := stConnection(top, *profile, report)
	if err != nil {
		report.add("FAIL", "%v", err)
		return errors.New("st.yml has no subtensor connection")
	}
	check, err := controller.CheckStConfigBytes(raw, *profile, rpcUrls)
	if err != nil {
		report.add("FAIL", "st.yml loader validation (as enabled): %v", err)
		return errors.New("st.yml failed the server's validation")
	}
	cfg := check.Config
	report.add("ok", "st.yml passed the server loader's validation for profile %s, as enabled", *profile)
	if check.EnabledInFile {
		report.add("note", "enabled is true in the file")
	} else {
		report.add("note", "enabled is false in the file, so the st subsystem stays off until it is set; all fields were checked as if enabled")
	}
	reportStKeys(report, top, check.LoaderKeys, *profile)
	reportStSettings(report, cfg, *profile)
	reportStGasPolicy(report, raw, check, *authorityPath, now)
	if *snPath != "" {
		reportStSchedule(report, check, *snPath, now)
	}
	reportStIdentity(report, cfg)
	if projection, err := receiptProjectionSha256(raw, *profile, rpcUrls); err != nil {
		report.add("FAIL", "receipt projection: %v", err)
	} else {
		report.add("receipt_public_config_sha256", "%s (server projection urnetwork.st.public_config.v1 with enabled true and launch_readiness_sha256 empty)", hex.EncodeToString(projection[:]))
	}
	if report.failures > 0 {
		return fmt.Errorf("%d check(s) failed", report.failures)
	}
	fmt.Fprintln(stdout, "result: ok")
	return nil
}

func profilePrefix(profile string) string {
	if profile == stconn.ProfileTestnet {
		return "testnet-"
	}
	return ""
}

// server/st resolves the connection from explicit rpc_urls, else from the
// authority (with {{ env: }} interpolation at runtime). Values are never
// printed, because an explicit URL may carry credentials.
func stConnection(top map[string]any, profile string, report *stCheckReport) ([]string, error) {
	prefix := profilePrefix(profile)
	var urls []string
	switch value := top[prefix+"rpc_urls"].(type) {
	case string:
		if strings.TrimSpace(value) != "" {
			urls = append(urls, value)
		}
	case []any:
		for _, item := range value {
			if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
				urls = append(urls, text)
			}
		}
	}
	if len(urls) > 0 {
		report.add("note", "connection: %d explicit %srpc_urls", len(urls), prefix)
		return urls, nil
	}
	if authority, ok := top[prefix+"authority"].(string); ok && strings.TrimSpace(authority) != "" {
		report.add("note", "connection: %sauthority is set and resolves at runtime", prefix)
		return []string{"http://" + authority}, nil
	}
	return nil, fmt.Errorf("st.yml has neither %sauthority nor %srpc_urls", prefix, prefix)
}

func reportStKeys(report *stCheckReport, top map[string]any, loaderKeys []string, profile string) {
	known := map[string]bool{}
	for _, key := range loaderKeys {
		known[key] = true
	}
	// The connection keys server/st reads, for both profiles.
	for _, key := range []string{"authority", "rpc_urls", "ws_url", "tls", "lightnode-authority", "lightnode-rpc_urls", "lightnode-ws_url"} {
		known[key] = true
		known["testnet-"+key] = true
	}
	var unknown []string
	for key := range top {
		if !known[key] {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(unknown)
	for _, key := range unknown {
		report.add("warn", "top-level key %q is read by neither the st loader nor server/st; the server ignores it (check the spelling)", key)
	}
	prefix := profilePrefix(profile)
	for _, key := range []string{prefix + "lightnode-authority", prefix + "lightnode-rpc_urls", prefix + "lightnode-ws_url"} {
		if _, ok := top[key]; ok && profile == stconn.ProfileMainnet {
			report.add("warn", "%s is set: substrate storage reads fail over to it (controller/sn_substrate.go); the mainnet plan removes it so reads use only the approved archive", key)
		}
	}
}

func lowerHex(value string, size int) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == size && value == strings.ToLower(value)
}

func reportStSettings(report *stCheckReport, cfg *controller.StConfig, profile string) {
	if profile == stconn.ProfileMainnet {
		if cfg.Netuid != 25 {
			report.add("FAIL", "netuid is %d; mainnet payout admission requires 25", cfg.Netuid)
		}
		switch readiness := cfg.LaunchReadinessSha256; {
		case readiness == "":
			report.add("warn", "launch_readiness_sha256 is empty; payout admission refuses until it equals sn.yml readiness_sha256")
		case !lowerHex(readiness, 32):
			report.add("FAIL", "launch_readiness_sha256 must be 64 lowercase hex characters without 0x; sn.yml compares it byte for byte")
		default:
			report.add("ok", "launch_readiness_sha256 is 64 lowercase hex characters")
		}
	}
	if err := cfg.AttemptUploadBudget.Validate(); err != nil {
		report.add("warn", "attempt_upload: %v; typed attempt uploads answer 503 until a budget is set", err)
	}
	if cfg.ReservedAttemptUpload == nil {
		report.add("note", "reserved_attempt_upload is absent (reserved staging is off)")
	}
	if cfg.WalletAllowUnsigned {
		report.add("warn", "wallet_allow_unsigned is true")
	}
}

func reportStGasPolicy(report *stCheckReport, raw []byte, check *controller.StGoLiveConfigCheck, authorityPath string, now time.Time) {
	if check.OperatorGasPolicyErr != nil {
		report.add("FAIL", "operator_gas_policy: %v; signing admission refuses every new EVM transaction", check.OperatorGasPolicyErr)
		return
	}
	policy := check.Config.OperatorGasPolicy.Clone()
	report.add("ok", "operator_gas_policy is valid and names this deployment and the deposit and root signers")
	if err := strictGasPolicyKeys(raw, profilePrefix(check.Config.Profile)); err != nil {
		report.add("FAIL", "operator_gas_policy: %v", err)
	}
	digest, err := policy.Digest()
	if err != nil {
		report.add("FAIL", "operator_gas_policy digest: %v", err)
		return
	}
	printGasSummary(report.out, policy, digest, "", nil)
	notes, err := reviewGasPolicy(policy, now)
	if err != nil {
		report.add("FAIL", "operator_gas_policy: %v", err)
	}
	for _, note := range notes {
		report.add("note", "%s", note)
	}
	if policy.Signature == "" {
		report.add("FAIL", "operator_gas_policy is unsigned")
		return
	}
	if authorityPath == "" {
		report.add("warn", "no --authority given: the policy signature and pin were not verified")
		return
	}
	encoded, err := readBounded(authorityPath, 16*1024)
	if err != nil {
		report.add("FAIL", "authority: %v", err)
		return
	}
	authority, err := server.ParseStOperatorGasAuthority(encoded)
	if err != nil {
		report.add("FAIL", "authority: %v", err)
		return
	}
	at := now
	if now.Unix() < policy.ValidFrom {
		at = time.Unix(policy.ValidFrom, 0).UTC()
	}
	if err := policy.Verify(authority, at); err != nil {
		report.add("FAIL", "operator_gas_policy against the authority: %v", err)
		return
	}
	report.add("ok", "operator_gas_policy signature and digest match the authority (approver %s) at %s", authority.ApproverPublicKey, at.Format(time.RFC3339))
}

// The live loader ignores unknown keys inside the policy too. A misspelled
// signed field would change the digest, so it is refused here explicitly.
func strictGasPolicyKeys(raw []byte, prefix string) error {
	var document map[string]yaml.Node
	if err := yaml.Unmarshal(raw, &document); err != nil {
		return err
	}
	key := "operator_gas_policy"
	if prefix != "" {
		key = "testnet-operator-gas-policy"
	}
	node, ok := document[key]
	if !ok {
		return nil
	}
	encoded, err := yaml.Marshal(&node)
	if err != nil {
		return err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(encoded))
	decoder.KnownFields(true)
	var policy server.StOperatorGasPolicy
	if err := decoder.Decode(&policy); err != nil {
		return fmt.Errorf("unknown or malformed field: %w", err)
	}
	return nil
}

func reportStSchedule(report *stCheckReport, check *controller.StGoLiveConfigCheck, snPath string, now time.Time) {
	data, err := readBounded(snPath, 32*1024)
	if err != nil {
		report.add("FAIL", "sn.yml: %v", err)
		return
	}
	transition, err := server.ParseProviderPayoutTransition(data)
	if err != nil {
		report.add("FAIL", "%v", err)
		return
	}
	earning, _ := transition.EarningIdentitySha256()
	report.add("note", "sn.yml config_sha256 %s, earning identity sha256 %s, activation %s", transition.ConfigSha256, earning, transition.Mainnet.Activation)
	at := now
	if at.Before(transition.Cutoff) {
		at = transition.Cutoff
	}
	if err := transition.MainnetAdmission(at, check.PayoutIdentity); err != nil {
		report.add("FAIL", "sn.yml payout admission against this st.yml: %v", err)
		return
	}
	report.add("ok", "sn.yml reviewed identity and readiness digest equal this st.yml's")
}

func reportStIdentity(report *stCheckReport, cfg *controller.StConfig) {
	address := func(label string, key *ecdsa.PrivateKey) {
		if key != nil {
			report.add("identity", "%s %s", label, strings.ToLower(crypto.PubkeyToAddress(key.PublicKey).Hex()))
		}
	}
	report.add("identity", "profile %s, chain_id %d, genesis_hash 0x%x, netuid %d, no_id %d", cfg.Profile, cfg.ChainId, cfg.GenesisHash, cfg.Netuid, cfg.NoId)
	report.add("identity", "deployment_id %s, policy_hash 0x%x, deploy_block %d", cfg.DeploymentId, cfg.PolicyHash, cfg.DeployBlock)
	report.add("identity", "coordinator %s, settlement_vault %s, reserve_sink %s", cfg.ContractAddress.Hex(), cfg.SettlementVault.Hex(), cfg.ReserveSink.Hex())
	report.add("identity", "deposit_hotkey 0x%x", cfg.DepositHotkey)
	if cfg.TreasuryHotkey != ([32]byte{}) {
		report.add("identity", "treasury_hotkey 0x%x", cfg.TreasuryHotkey)
	}
	address("deposit signer", cfg.DepositKey)
	address("root signer (also ops)", cfg.RootKey)
	address("artifact signer", cfg.ArtifactKey)
	tiers := make([]string, 0, len(cfg.DepositTiers))
	for _, tier := range cfg.DepositTiers {
		tiers = append(tiers, fmt.Sprintf("{min %d, %d/%d per GiB, %d per user}", tier.MinConvictionRao, tier.RateNumerator, tier.RateDenominator, tier.UserRateNumerator))
	}
	report.add("identity", "deposit tiers %s, zero_rate_action %q, epoch cap %d rao", strings.Join(tiers, " "), cfg.DepositZeroRateAction, cfg.DepositEpochCapRao)
	if cfg.PublicRpcUrl != "" {
		report.add("note", "public_rpc_url is set (published through GET /sn/epoch)")
	}
}

// The readiness receipt may bind this projection without a cycle: the
// server's own public projection of the configuration as it will run, with
// the readiness digest field emptied.
func receiptProjectionSha256(raw []byte, profile string, rpcUrls []string) ([32]byte, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(raw, &document); err != nil {
		return [32]byte{}, err
	}
	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return [32]byte{}, errors.New("st.yml is not one YAML mapping")
	}
	mapping := document.Content[0]
	setYamlScalar(mapping, profilePrefix(profile)+"enabled", "!!bool", "true")
	if profile == stconn.ProfileMainnet {
		setYamlScalar(mapping, "launch_readiness_sha256", "!!str", "")
	}
	encoded, err := yaml.Marshal(&document)
	if err != nil {
		return [32]byte{}, err
	}
	inspection, err := controller.InspectStConfigBytes(encoded, profile, rpcUrls)
	if err != nil {
		return [32]byte{}, err
	}
	return inspection.PublicConfigSha256, nil
}

func setYamlScalar(mapping *yaml.Node, key, tag, value string) {
	scalar := &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content[i+1] = scalar
			return
		}
	}
	mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, scalar)
}
