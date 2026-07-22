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

// the polkadot-js signRaw payload wrapper
var bittensorBytesWrapPrefix = []byte("<Bytes>")
var bittensorBytesWrapSuffix = []byte("</Bytes>")

// DecodeSS58Address decodes an ss58 address to its 32 byte public key,
// verifying the blake2b checksum. The network prefix (42 for bittensor and
// generic substrate) is validated structurally but not pinned, matching how
// substrate tooling treats addresses.
func DecodeSS58Address(address string) ([32]byte, error) {
	var publicKey [32]byte

	raw, err := base58.Decode(address)
	if err != nil {
		return publicKey, fmt.Errorf("invalid ss58 address: %v", err)
	}

	// [prefix (1 or 2 bytes)][32 byte public key][2 byte checksum]
	var prefixLen int
	switch len(raw) {
	case 1 + 32 + 2:
		prefixLen = 1
		if 64 <= raw[0] {
			return publicKey, fmt.Errorf("invalid ss58 address: malformed network prefix")
		}
	case 2 + 32 + 2:
		prefixLen = 2
		if raw[0] < 64 {
			return publicKey, fmt.Errorf("invalid ss58 address: malformed network prefix")
		}
	default:
		return publicKey, fmt.Errorf("invalid ss58 address: unexpected length %d", len(raw))
	}

	checksumStart := len(raw) - 2
	hasher, err := blake2b.New512(nil)
	if err != nil {
		return publicKey, err
	}
	hasher.Write([]byte(ss58Prefix))
	hasher.Write(raw[:checksumStart])
	checksum := hasher.Sum(nil)
	if !bytes.Equal(checksum[:2], raw[checksumStart:]) {
		return publicKey, fmt.Errorf("invalid ss58 address: checksum mismatch")
	}

	copy(publicKey[:], raw[prefixLen:checksumStart])
	return publicKey, nil
}

// IsValidBittensorAddress reports whether the address is a well formed ss58
// address (base58, structure, and checksum)
func IsValidBittensorAddress(address string) bool {
	_, err := DecodeSS58Address(address)
	return err == nil
}

/**
 * Verify a Bittensor (sr25519) wallet signature
 * publicKey: the ss58 wallet address
 * message: the signed message text
 * signature: the 64 byte sr25519 signature in hex (with or without 0x)
 */
func VerifyBittensorSignature(publicKey string, message string, signature string) (bool, error) {
	publicKeyBytes, err := DecodeSS58Address(publicKey)
	if err != nil {
		return false, err
	}

	signatureHex := strings.TrimPrefix(strings.TrimSpace(signature), "0x")
	signatureBytes, err := hex.DecodeString(signatureHex)
	if err != nil {
		return false, fmt.Errorf("invalid signature encoding: %v", err)
	}
	if len(signatureBytes) != 64 {
		return false, fmt.Errorf("signature must be 64 bytes")
	}
	var signatureFixed [64]byte
	copy(signatureFixed[:], signatureBytes)

	pub := &schnorrkel.PublicKey{}
	if err := pub.Decode(publicKeyBytes); err != nil {
		return false, fmt.Errorf("invalid public key: %v", err)
	}
	sig := &schnorrkel.Signature{}
	if err := sig.Decode(signatureFixed); err != nil {
		return false, fmt.Errorf("invalid signature: %v", err)
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
