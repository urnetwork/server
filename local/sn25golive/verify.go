package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/urfoundation/sn/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

type verifyFileKey struct {
	ServerKeyId int    `yaml:"server_key_id"`
	Seed        string `yaml:"seed"`
}

// The server reads profile, policy_hash, egress_hash_key, keys and settings
// and ignores other keys, such as the policy's trail_depth.
type verifyFile struct {
	Profile       string                `yaml:"profile"`
	PolicyHash    string                `yaml:"policy_hash"`
	EgressHashKey string                `yaml:"egress_hash_key"`
	Keys          []verifyFileKey       `yaml:"keys"`
	Settings      protocol.VerifyPolicy `yaml:"settings"`
}

func runVerifyConfig(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("verify-config", flag.ContinueOnError)
	policyPath := flags.String("policy", "", "approved protocol policy (sn/deploy/mainnet/policy-v1.yml)")
	expectHash := flags.String("expect-policy-hash", "", "the approved 0x policy hash; a different policy file is refused")
	out := flags.String("out", "", "new verify.yml (created 0600, never overwritten)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *policyPath == "" || *expectHash == "" || *out == "" {
		return errors.New("--policy, --expect-policy-hash and --out are required")
	}
	policy, err := protocol.LoadPolicy(*policyPath)
	if err != nil {
		return err
	}
	policyHash, err := policy.HashHex()
	if err != nil {
		return err
	}
	if policyHash != strings.ToLower(*expectHash) {
		return fmt.Errorf("policy hash %s differs from --expect-policy-hash", policyHash)
	}
	egressHashKey := make([]byte, 32)
	seed := make([]byte, ed25519.SeedSize)
	for _, secret := range [][]byte{egressHashKey, seed} {
		if _, err := rand.Read(secret); err != nil {
			return err
		}
	}
	document := verifyFile{Profile: policy.NetworkProfile, PolicyHash: policyHash, EgressHashKey: base64.StdEncoding.EncodeToString(egressHashKey),
		Keys: []verifyFileKey{{ServerKeyId: 0, Seed: base64.StdEncoding.EncodeToString(seed)}}, Settings: policy.Verify}
	body, err := yamlEncode(document)
	if err != nil {
		return err
	}
	encoded := append([]byte(fmt.Sprintf("# verify.yml for the %s vault. Secret: egress_hash_key and keys[].seed.\n# settings: the verify section of policy %s.\n", policy.NetworkProfile, policyHash)), body...)
	publicKey, err := checkVerifyWithServer(encoded, policy, policyHash, egressHashKey, seed)
	if err != nil {
		return fmt.Errorf("the server's verify.yml loader refused the candidate: %w", err)
	}
	if err := writeNewFile(*out, encoded, 0o600); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "profile: %s\npolicy_hash: %s\negress_hash_key_id: %s\nserver_key_id: 0\nverify_public_key_hex: %s\nverify_public_key_base64: %s\nout: %s (mode 0600)\n",
		policy.NetworkProfile, policyHash, policy.Verify.EgressHashKeyID, hex.EncodeToString(publicKey), base64.StdEncoding.EncodeToString(publicKey), *out)
	return nil
}

// Runs the server's own verify.yml loaders (keys and settings) on the
// candidate bytes, injected as the vault resource, with an st.yml identity
// carrying the same profile and policy hash for the loader's cross-check.
func checkVerifyWithServer(encoded []byte, policy *protocol.Policy, policyHash string, egressHashKey, seed []byte) (publicKey ed25519.PublicKey, err error) {
	hash, err := hex.DecodeString(strings.TrimPrefix(policyHash, "0x"))
	if err != nil || len(hash) != 32 {
		return nil, errors.New("policy hash is not 32 bytes")
	}
	stIdentity := &controller.StConfig{Profile: policy.NetworkProfile, PolicyHash: [32]byte(hash)}
	pop := server.Vault.PushSimpleResource("verify.yml", encoded)
	defer pop()
	controller.SetStConfig(stIdentity)
	defer controller.SetStConfig(nil)
	var keys *controller.GetVerifyKeysResult
	var settings *model.VerifySettings
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				err = fmt.Errorf("%v", recovered)
			}
		}()
		if keys, err = controller.GetVerifyKeys(nil); err == nil {
			settings = controller.VerifySettings()
		}
	}()
	if err != nil {
		return nil, err
	}
	publicKey = ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	if len(keys.Keys) != 1 || keys.Keys[0].ServerKeyId != 0 || !bytes.Equal(keys.Keys[0].PublicKey, publicKey) {
		return nil, errors.New("loaded signing keys differ from the generated server_key_id 0 seed")
	}
	v := policy.Verify
	second := func(value int) time.Duration { return time.Duration(value) * time.Second }
	if settings.StepTimeout != second(v.StepTimeoutSeconds) || settings.StepTimeoutGrace != second(v.StepTimeoutGraceSeconds) ||
		settings.TrailTtlGrace != second(v.TrailTTLGraceSeconds) || settings.EgressTtl != second(v.EgressTTLSeconds) ||
		settings.EgressRefreshInterval != second(v.EgressRefreshSeconds) || settings.StatsPeriod != second(v.StatsPeriodSeconds) ||
		settings.ReliabilityAMin != int64(v.ReliabilityAMin) || settings.EgressHashV4Prefix != v.EgressIPv4Prefix || settings.EgressHashV6Prefix != v.EgressIPv6Prefix ||
		settings.EgressHashKeyId != v.EgressHashKeyID || settings.SoftLimitsEnabled != v.SoftGuardrailsEnabled ||
		settings.SeedRateHardLimit != int64(v.HardSeedPerMinutePerSource) || settings.ExtendRateHardLimit != int64(v.HardExtendPerMinutePerSource) ||
		settings.ActiveTrailsHardLimit != int64(v.HardActiveTrailsPerSource) || !bytes.Equal(settings.EgressHashKey, egressHashKey) {
		return nil, errors.New("loaded settings differ from the policy's verify section")
	}
	return publicKey, nil
}
