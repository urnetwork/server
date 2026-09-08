package model

import (
	"encoding/hex"
	"testing"

	"github.com/ChainSafe/go-schnorrkel"
	"github.com/mr-tron/base58"
	"github.com/urnetwork/connect/v2026"
	"golang.org/x/crypto/blake2b"
)

// ss58 encode for tests (single byte network prefix)
func testingSS58Encode(prefix byte, publicKey [32]byte) string {
	data := append([]byte{prefix}, publicKey[:]...)
	hasher, _ := blake2b.New512(nil)
	hasher.Write([]byte(ss58Prefix))
	hasher.Write(data)
	checksum := hasher.Sum(nil)
	return base58.Encode(append(data, checksum[:2]...))
}

// well known substrate dev addresses: the same public key rendered with the
// generic substrate prefix (42, used by bittensor) and the polkadot prefix (0)
func TestDecodeSS58Address(t *testing.T) {
	alicePublicKey, err := hex.DecodeString("d43593c715fdd31c61141abd04a99fd6822c8558854ccde39a5684e7a56da27d")
	connect.AssertEqual(t, err, nil)

	// alice, substrate generic prefix 42
	publicKey, err := DecodeSS58Address("5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY")
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, publicKey[:], alicePublicKey)

	// alice, polkadot prefix 0 (prefix-agnostic decode)
	publicKey, err = DecodeSS58Address("15oF4uVJwmo4TdGW7VfQxNLavjCXviqxT9S1MgbjMNHr6Sp5")
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, publicKey[:], alicePublicKey)

	// corrupt the checksum
	_, err = DecodeSS58Address("5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQZ")
	connect.AssertNotEqual(t, err, nil)

	// malformed inputs
	_, err = DecodeSS58Address("")
	connect.AssertNotEqual(t, err, nil)
	_, err = DecodeSS58Address("not-an-address")
	connect.AssertNotEqual(t, err, nil)
	_, err = DecodeSS58Address("0x00")
	connect.AssertNotEqual(t, err, nil)

	connect.AssertEqual(t, IsValidBittensorAddress("5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY"), true)
	connect.AssertEqual(t, IsValidBittensorAddress("5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQZ"), false)
}

func TestVerifyBittensorSignature(t *testing.T) {
	message := "Welcome to URnetwork"

	secretKey, publicKey, err := schnorrkel.GenerateKeypair()
	connect.AssertEqual(t, err, nil)
	publicKeyBytes := publicKey.Encode()
	address := testingSS58Encode(42, publicKeyBytes)

	// signers wrap the payload in <Bytes>…</Bytes> (polkadot signRaw)
	wrapped := "<Bytes>" + message + "</Bytes>"
	transcript := schnorrkel.NewSigningContext([]byte("substrate"), []byte(wrapped))
	signature, err := secretKey.Sign(transcript)
	connect.AssertEqual(t, err, nil)
	signatureBytes := signature.Encode()
	signatureHex := hex.EncodeToString(signatureBytes[:])

	valid, err := VerifyBittensorSignature(address, message, signatureHex)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, valid, true)

	// 0x prefixed signatures also verify (the walletconnect return format)
	valid, err = VerifyBittensorSignature(address, message, "0x"+signatureHex)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, valid, true)

	// signers that do not wrap the payload also verify
	rawTranscript := schnorrkel.NewSigningContext([]byte("substrate"), []byte(message))
	rawSignature, err := secretKey.Sign(rawTranscript)
	connect.AssertEqual(t, err, nil)
	rawSignatureBytes := rawSignature.Encode()
	valid, err = VerifyBittensorSignature(address, message, hex.EncodeToString(rawSignatureBytes[:]))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, valid, true)

	// a different message does not verify
	valid, _ = VerifyBittensorSignature(address, "another message", signatureHex)
	connect.AssertEqual(t, valid, false)

	// a different key does not verify
	_, otherPublicKey, err := schnorrkel.GenerateKeypair()
	connect.AssertEqual(t, err, nil)
	otherPublicKeyBytes := otherPublicKey.Encode()
	otherAddress := testingSS58Encode(42, otherPublicKeyBytes)
	valid, _ = VerifyBittensorSignature(otherAddress, message, signatureHex)
	connect.AssertEqual(t, valid, false)

	// malformed signature encodings error
	_, err = VerifyBittensorSignature(address, message, "zz")
	connect.AssertNotEqual(t, err, nil)
	_, err = VerifyBittensorSignature(address, message, "abcd")
	connect.AssertNotEqual(t, err, nil)

	// dispatch through VerifySignature for both blockchain spellings
	valid, err = VerifySignature("TAO", address, message, signatureHex)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, valid, true)
	valid, err = VerifySignature("bittensor", address, message, signatureHex)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, valid, true)
}

func TestParseBlockchainTao(t *testing.T) {
	blockchain, err := ParseBlockchain("tao")
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, blockchain, TAO)
	connect.AssertEqual(t, blockchain.String(), "TAO")

	blockchain, err = ParseBlockchain("BITTENSOR")
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, blockchain, TAO)
}
