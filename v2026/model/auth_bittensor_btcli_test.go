package model

// What btcli really prints when it signs a network payout consent, recorded
// from the real tools so the manual signing steps on ur.io stay true:
//
//	btcli wallet sign --message '<the two lines of the consent>'
//
// pasted into an interactive shell under a pty, with a throwaway sr25519
// coldkey in a throwaway container (bittensor 11.3.0 and bittensor-cli 9.23.2
// from PyPI, 2026-11-02). Only public values are kept: the address, the
// synthetic consent and the signature. btcli signs the message's UTF-8 bytes in
// the substrate context with no <Bytes> wrapper, and prints bare hex, which
// btcli 9 hard-wraps at the terminal's width.

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/urfoundation/sn/v2026/protocol"
)

type bittensorBtcliSignature struct {
	version string
	address string
	// the statement names the address's coldkey: synthetic identities, nonce
	// and operator key (the sn protocol tests' fixture)
	message string
	// exactly as the terminal showed it, at 80 columns
	printed string
}

var bittensorBtcliSignatures = []bittensorBtcliSignature{
	{
		version: "bittensor 11.3.0",
		address: "5FuqvKqpskxSHv3vbVmxTwFnxsk1JdjXC1F4GzERkC6zmhXZ",
		message: `Approve URnetwork network wallet mapping
{"schema":"urnetwork-network-wallet-mapping-consent-v1","scope":"network","domain":{"chain_id":964,"genesis_hash":[1,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0],"netuid":25,"coordinator":"0x0200000000000000000000000000000000000000","settlement_vault":"0x0300000000000000000000000000000000000000","deployment_id_hash":[4,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0],"policy_hash":[5,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0],"no_id":6},"user_id":[7,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0],"network_id":[9,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0],"coldkey":[170,46,222,41,113,105,216,15,18,49,161,242,127,179,201,200,126,8,22,238,141,162,167,189,151,198,30,98,8,244,252,65],"generation":1,"previous_hash":[0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0],"nonce":[10,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0],"issued_at":1000,"expires_at":1300,"from_epoch":51,"through_epoch":65586,"prospective":{"boundary":{"epoch":50,"block":500,"hash":[12,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0]},"signer":"0x17c5185167401ed00cf5f5b2fc97d9bbfdb7d025","signature":[102,207,194,47,127,38,215,206,93,219,15,67,102,151,146,3,182,157,139,186,15,9,187,52,202,145,80,78,153,120,173,221,3,138,141,176,161,147,25,109,82,228,180,74,49,242,211,88,106,82,0,217,233,44,191,255,114,81,230,238,41,194,209,110,1]}}`,
		// "  signed_message  <hex>" on one line, however wide the terminal is
		printed: "3e447a2103656826c66f0c99f8e5198da77c50ceab7b051064b4166e8b4744341072b11ab910d0dc0fb8e44c8aa7f4cbcc328f1a4521be8ea799e87172624f87",
	},
	{
		version: "bittensor-cli 9.23.2",
		address: "5DHqLacxpVxpygYXLZmqrSuhmHv1NnutYxsL4xjrMzXm6i5U",
		message: `Approve URnetwork network wallet mapping
{"schema":"urnetwork-network-wallet-mapping-consent-v1","scope":"network","domain":{"chain_id":964,"genesis_hash":[1,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0],"netuid":25,"coordinator":"0x0200000000000000000000000000000000000000","settlement_vault":"0x0300000000000000000000000000000000000000","deployment_id_hash":[4,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0],"policy_hash":[5,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0],"no_id":6},"user_id":[7,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0],"network_id":[9,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0],"coldkey":[54,63,155,152,147,213,31,235,213,103,226,19,117,20,210,56,163,69,146,151,24,188,0,36,235,211,85,239,4,55,207,109],"generation":1,"previous_hash":[0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0],"nonce":[10,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0],"issued_at":1000,"expires_at":1300,"from_epoch":51,"through_epoch":65586,"prospective":{"boundary":{"epoch":50,"block":500,"hash":[12,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0]},"signer":"0x17c5185167401ed00cf5f5b2fc97d9bbfdb7d025","signature":[231,174,201,133,66,225,227,170,178,1,184,205,113,49,248,131,35,213,105,223,190,167,82,235,150,173,168,149,74,202,147,145,74,120,11,170,151,67,235,86,109,45,195,12,29,70,91,91,205,192,185,227,211,219,92,83,222,124,148,80,175,194,185,14,1]}}`,
		// under "Signature:", broken at 80 columns
		printed: "489aadc4884bfecd0c0a9a359db8a633d8218b70c3e2cbe9074b59c48390f65b3cc1851165a19aed\n3b7630fff4b06932fc1327d96bbfc7bc561bc8661a907e8b",
	},
}

// The signature as ur.io and the SDK send it after normalizing the paste.
func bittensorBtcliSignatureHex(printed string) string {
	return strings.ReplaceAll(printed, "\n", "")
}

// The login and mapping checks accept btcli's bare hex over the raw message,
// and refuse it for any other address or any other text.
func TestVerifyBittensorSignatureAcceptsBtcliOutput(t *testing.T) {
	for _, signed := range bittensorBtcliSignatures {
		signature := bittensorBtcliSignatureHex(signed.printed)
		for _, form := range []string{signature, "0x" + signature, "  " + strings.ToUpper(signature) + "\n"} {
			valid, err := VerifyBittensorSignature(signed.address, signed.message, form)
			if err != nil || !valid {
				t.Fatalf("%s: %q did not verify: %v, %v", signed.version, form, valid, err)
			}
		}
		other := bittensorBtcliSignatures[0].address
		if other == signed.address {
			other = bittensorBtcliSignatures[1].address
		}
		if valid, err := VerifyBittensorSignature(other, signed.message, signature); err != nil || valid {
			t.Fatalf("%s: verified for another address: %v, %v", signed.version, valid, err)
		}
		// what a changed copy of the message gives: a line break added at the
		// end, changed to CRLF or replaced by a space, or only the first line
		// (what btcli signs when the text is pasted at its own prompt)
		firstLine, _, _ := strings.Cut(signed.message, "\n")
		for _, changed := range []string{signed.message + "\n", strings.ReplaceAll(signed.message, "\n", "\r\n"), strings.ReplaceAll(signed.message, "\n", " "), firstLine} {
			if valid, err := VerifyBittensorSignature(signed.address, changed, signature); err != nil || valid {
				t.Fatalf("%s: verified over changed text %q: %v, %v", signed.version, changed[:48], valid, err)
			}
		}
	}
}

// The consent's own check, the one POST /sn/wallet and the validators run on
// the retained original, accepts what btcli signed.
func TestNetworkWalletMappingConsentAcceptsBtcliSignature(t *testing.T) {
	for _, signed := range bittensorBtcliSignatures {
		raw, err := hexDecodeBittensorSignature(bittensorBtcliSignatureHex(signed.printed))
		if err != nil {
			t.Fatal(signed.version, err)
		}
		original := protocol.WalletMappingConsent{Message: signed.message, Signature: raw}
		statement, _, err := protocol.VerifyNetworkWalletMappingConsent(context.Background(), original)
		if err != nil {
			t.Fatalf("%s: refused: %v", signed.version, err)
		}
		address, err := DecodeBittensorAddress(signed.address)
		if err != nil || statement.Coldkey != address {
			t.Fatalf("%s: the statement names another coldkey: %v", signed.version, err)
		}
		// a changed message is not this consent
		changed := protocol.WalletMappingConsent{Message: signed.message + "\n", Signature: raw}
		if _, _, err := protocol.VerifyNetworkWalletMappingConsent(context.Background(), changed); !errors.Is(err, protocol.ErrWalletMappingIntegrity) {
			t.Fatalf("%s: a changed message: %v", signed.version, err)
		}
	}
}

func hexDecodeBittensorSignature(signature string) ([64]byte, error) {
	var raw [64]byte
	decoded, err := hex.DecodeString(signature)
	if err != nil {
		return raw, err
	}
	if len(decoded) != len(raw) {
		return raw, errors.New("not 64 bytes")
	}
	copy(raw[:], decoded)
	return raw, nil
}
