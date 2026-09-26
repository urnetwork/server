package model

import (
	"encoding/hex"
	"testing"

	"github.com/ChainSafe/go-schnorrkel"
	"github.com/mr-tron/base58"
	"github.com/urnetwork/connect"
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

// The miner CLI (`provider wallet set --coldkey_seed_file`, sn/miner/sn.go)
// derives the coldkey from a 32-byte mini secret the way subkey, polkadot-js
// and btcli do (ExpandEd25519) and signs the wallet challenge in the
// "substrate" signing context, hex encoded with a 0x prefix, over the
// <Bytes>…</Bytes>-wrapped message (the same bytes a polkadot-js signRaw of
// type "bytes" signs). A signature made elsewhere (`--message --signature`,
// e.g. btcli) may cover the raw text instead. Both must verify here.
//
// Known keypair: the substrate dev account Alice (sr25519), whose mini secret
// and public key are published vectors. The two fixed signatures were
// produced by ur.io's test-only signer (mmm/ur.io/react/tests/sr25519.mjs,
// vectors in sr25519.test.mjs) and pin that signer to this verifier.
func TestVerifyBittensorSignatureKnownKeypairCliFormat(t *testing.T) {
	seedBytes, err := hex.DecodeString("e5be9a5092b81bca64be81d212e7f2f9eba183bb7a90954f7b76361f6edb5c0a")
	connect.AssertEqual(t, err, nil)
	var seed [32]byte
	copy(seed[:], seedBytes)
	miniSecret, err := schnorrkel.NewMiniSecretKeyFromRaw(seed)
	connect.AssertEqual(t, err, nil)
	secretKey := miniSecret.ExpandEd25519()
	publicKeyBytes := miniSecret.Public().Encode()
	connect.AssertEqual(t, hex.EncodeToString(publicKeyBytes[:]), "d43593c715fdd31c61141abd04a99fd6822c8558854ccde39a5684e7a56da27d")
	address := testingSS58Encode(42, publicKeyBytes)
	connect.AssertEqual(t, address, "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY")

	message := FormatWalletAuthChallengeMessage("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", 1700000000)
	connect.AssertEqual(t, message, "Sign in to URnetwork\nChallenge: AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\nTimestamp: 1700000000")

	// the CLI's seed-file path: wrapped, 0x-prefixed hex
	wrapped := "<Bytes>" + message + "</Bytes>"
	signature, err := secretKey.Sign(schnorrkel.NewSigningContext([]byte("substrate"), []byte(wrapped)))
	connect.AssertEqual(t, err, nil)
	signatureBytes := signature.Encode()
	valid, err := VerifyBittensorSignature(address, message, "0x"+hex.EncodeToString(signatureBytes[:]))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, valid, true)

	// a signature made elsewhere over the raw text (btcli style)
	rawSignature, err := secretKey.Sign(schnorrkel.NewSigningContext([]byte("substrate"), []byte(message)))
	connect.AssertEqual(t, err, nil)
	rawSignatureBytes := rawSignature.Encode()
	valid, err = VerifyBittensorSignature(address, message, hex.EncodeToString(rawSignatureBytes[:]))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, valid, true)

	// fixed vectors from the ur.io test signer: wrapped and raw
	for _, fixed := range []string{
		"0xb642fa47781f14d9185311b6fb29d654975b2f8d1b383ae0dc7ece352d377a4fc409bbaf53cc81293c103ee9a6113dd09e59c7db83e540eee85ccc7420cc538d",
		"1c5a4bf2e43145dff05cd141d56d9d7bf61d4b512a071327a6a8e41783e8825766b5fc10f4e6a17bdf33297f0ececacbeeeb95fc23a7a59fdf33811dd3900388",
	} {
		valid, err = VerifyBittensorSignature(address, message, fixed)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, valid, true)
		// the same signature does not verify for a different challenge
		valid, _ = VerifyBittensorSignature(address, FormatWalletAuthChallengeMessage("BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB=", 1700000000), fixed)
		connect.AssertEqual(t, valid, false)
	}

	// the same key under a different derivation (ExpandUniform) is a different
	// key: a signer that does not follow subkey/polkadot-js derivation would
	// produce an address the CLI refuses to pair with the seed
	uniformPublic, err := miniSecret.ExpandUniform().Public()
	connect.AssertEqual(t, err, nil)
	uniformPublicKey := uniformPublic.Encode()
	connect.AssertNotEqual(t, hex.EncodeToString(uniformPublicKey[:]), hex.EncodeToString(publicKeyBytes[:]))
}
