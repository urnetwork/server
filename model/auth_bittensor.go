package model

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/ChainSafe/go-schnorrkel"
	"github.com/mr-tron/base58"
	"golang.org/x/crypto/blake2b"
)

/**
 * Bittensor (substrate sr25519 / ss58) wallet signature verification
 * ===================================================================
 * Bittensor accounts are substrate sr25519 keys addressed with ss58.
 * The standard mobile/browser signing path (polkadot signRaw /
 * polkadot_signMessage over WalletConnect) signs with the "substrate"
 * signing context, and most signers wrap the payload in <Bytes>…</Bytes>
 * before signing — so verification accepts both the raw and wrapped forms.
 * sr25519 signatures are non-deterministic; only verification is possible.
 */

// the ss58 checksum preimage prefix
const ss58Prefix = "SS58PRE"

// BittensorSS58Prefix is the ss58 network prefix type Bittensor uses (the
// generic substrate one). Wallet auth PINS it, and that pinning is load
// bearing: the same 32 byte public key re-encoded under any of the 16384
// network prefixes is a different address string, while every account
// uniqueness check keys on that string (network_user_auth_wallet
// UNIQUE (wallet_address, blockchain), the add-auth pre-check, and the
// network-create pre-check). Accepting unpinned prefixes therefore let one
// key mint unlimited distinct identities, and let a wallet that handed back
// a different encoding of an already-bound key present as a brand new
// signup. Mirrors ss58.BittensorPrefix in the sn module (used by
// controller/sn_controller.go) and the published api contract.
const BittensorSS58Prefix = 42

// the polkadot-js signRaw payload wrapper
var bittensorBytesWrapPrefix = []byte("<Bytes>")
var bittensorBytesWrapSuffix = []byte("</Bytes>")

// DecodeSS58Address decodes an ss58 address to its 32 byte public key,
// verifying the blake2b checksum. The network prefix is validated
// structurally but not pinned, matching how substrate tooling treats
// addresses. Wallet auth must use DecodeBittensorAddress instead -- see
// BittensorSS58Prefix for why an unpinned prefix is unsafe as an identity.
func DecodeSS58Address(address string) ([32]byte, error) {
	publicKey, _, err := decodeSS58AddressWithPrefix(address)
	return publicKey, err
}

// DecodeBittensorAddress decodes an ss58 address and additionally requires
// the Bittensor network prefix, so one public key has exactly one accepted
// address string. This is the decoder every wallet-auth path uses.
func DecodeBittensorAddress(address string) ([32]byte, error) {
	publicKey, prefix, err := decodeSS58AddressWithPrefix(address)
	if err != nil {
		return publicKey, err
	}
	if prefix != BittensorSS58Prefix {
		return publicKey, fmt.Errorf(
			"invalid ss58 address: network prefix %d is not bittensor (%d)",
			prefix,
			BittensorSS58Prefix,
		)
	}
	return publicKey, nil
}

// decodeSS58AddressWithPrefix returns the public key and the decoded network
// prefix type. Prefix types 0..63 use one prefix byte and 64..16383 the
// two byte form, per the ss58 registry.
func decodeSS58AddressWithPrefix(address string) ([32]byte, uint16, error) {
	var publicKey [32]byte

	raw, err := base58.Decode(address)
	if err != nil {
		return publicKey, 0, fmt.Errorf("invalid ss58 address: %v", err)
	}

	// [prefix (1 or 2 bytes)][32 byte public key][2 byte checksum]
	var prefixLen int
	var prefix uint16
	switch len(raw) {
	case 1 + 32 + 2:
		prefixLen = 1
		if 64 <= raw[0] {
			return publicKey, 0, fmt.Errorf("invalid ss58 address: malformed network prefix")
		}
		prefix = uint16(raw[0])
	case 2 + 32 + 2:
		prefixLen = 2
		if raw[0] < 64 {
			return publicKey, 0, fmt.Errorf("invalid ss58 address: malformed network prefix")
		}
		prefix = uint16(raw[0]&0b0011_1111)<<2 |
			uint16(raw[1])>>6 |
			uint16(raw[1]&0b0011_1111)<<8
	default:
		return publicKey, 0, fmt.Errorf("invalid ss58 address: unexpected length %d", len(raw))
	}

	checksumStart := len(raw) - 2
	hasher, err := blake2b.New512(nil)
	if err != nil {
		return publicKey, 0, err
	}
	hasher.Write([]byte(ss58Prefix))
	hasher.Write(raw[:checksumStart])
	checksum := hasher.Sum(nil)
	if !bytes.Equal(checksum[:2], raw[checksumStart:]) {
		return publicKey, 0, fmt.Errorf("invalid ss58 address: checksum mismatch")
	}

	copy(publicKey[:], raw[prefixLen:checksumStart])
	return publicKey, prefix, nil
}

// IsValidBittensorAddress reports whether the address is a well formed
// Bittensor ss58 address (base58, structure, checksum, and network prefix 42)
func IsValidBittensorAddress(address string) bool {
	_, err := DecodeBittensorAddress(address)
	return err == nil
}

/**
 * Verify a Bittensor (sr25519) wallet signature
 * publicKey: the ss58 wallet address
 * message: the signed message text
 * signature: the 64 byte sr25519 signature in hex (with or without 0x)
 */
func VerifyBittensorSignature(publicKey string, message string, signature string) (bool, error) {
	publicKeyBytes, err := DecodeBittensorAddress(publicKey)
	if err != nil {
		return false, err
	}

	signatureHex := strings.TrimPrefix(strings.TrimSpace(signature), "0x")
	signatureBytes, err := hex.DecodeString(signatureHex)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrWalletSignatureEncoding, err)
	}
	if len(signatureBytes) != 64 {
		return false, fmt.Errorf("%w: signature must be 64 bytes", ErrWalletSignatureEncoding)
	}
	var signatureFixed [64]byte
	copy(signatureFixed[:], signatureBytes)

	pub := &schnorrkel.PublicKey{}
	if err := pub.Decode(publicKeyBytes); err != nil {
		return false, fmt.Errorf("invalid public key: %v", err)
	}
	sig := &schnorrkel.Signature{}
	// 64 bytes that are not a well formed schnorrkel signature are client
	// input, not a server fault -- classify them as an encoding problem so
	// the caller answers 4xx instead of leaking this text through a 500.
	if err := sig.Decode(signatureFixed); err != nil {
		return false, fmt.Errorf("%w: %v", ErrWalletSignatureEncoding, err)
	}

	// signers (polkadot-js signRaw and compatible wallets) usually wrap the
	// payload in <Bytes>…</Bytes>; accept the raw form too for signers that
	// do not
	messageBytes := []byte(message)
	wrappedBytes := append(append([]byte{}, bittensorBytesWrapPrefix...), append(messageBytes, bittensorBytesWrapSuffix...)...)
	for _, candidate := range [][]byte{wrappedBytes, messageBytes} {
		transcript := schnorrkel.NewSigningContext([]byte("substrate"), candidate)
		ok, err := pub.Verify(sig, transcript)
		if err == nil && ok {
			return true, nil
		}
	}
	return false, nil
}
