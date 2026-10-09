package model

import (
	"crypto/ed25519"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/ChainSafe/go-schnorrkel"
	"github.com/urnetwork/connect"
)

// A substrate account can be ed25519 rather than sr25519. Its ss58 address
// looks the same, so the verifier must accept an ed25519 signature on it,
// raw or <Bytes>-wrapped, without loosening anything else.
func TestVerifyBittensorSignatureEd25519(t *testing.T) {
	message := "Sign in to URnetwork\nChallenge: q1w2e3r4t5y6u7i8o9p0\nTimestamp: 1757340000"

	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	connect.AssertEqual(t, err, nil)
	var publicKeyBytes [32]byte
	copy(publicKeyBytes[:], publicKey)
	address := testingSS58Encode(42, publicKeyBytes)

	for _, signed := range []string{"<Bytes>" + message + "</Bytes>", message} {
		signature := ed25519.Sign(privateKey, []byte(signed))
		// an ed25519 signature never carries schnorrkel's marker bit
		connect.AssertEqual(t, signature[63]&0x80, byte(0))
		for _, encoded := range []string{hex.EncodeToString(signature), "0x" + hex.EncodeToString(signature)} {
			valid, err := VerifyBittensorSignature(address, message, encoded)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, valid, true)
			valid, err = VerifySignature("TAO", address, message, encoded)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, valid, true)
		}
	}

	signature := hex.EncodeToString(ed25519.Sign(privateKey, []byte(message)))

	// another message, or another ed25519 key, does not verify
	valid, err := VerifyBittensorSignature(address, "another message", signature)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, valid, false)
	otherPublicKey, _, err := ed25519.GenerateKey(nil)
	connect.AssertEqual(t, err, nil)
	var otherBytes [32]byte
	copy(otherBytes[:], otherPublicKey)
	valid, err = VerifyBittensorSignature(testingSS58Encode(42, otherBytes), message, signature)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, valid, false)

	// a signature over some other text the wallet was tricked into signing
	// (the challenge is still mandatory) does not verify
	valid, err = VerifyBittensorSignature(address, message, hex.EncodeToString(ed25519.Sign(privateKey, []byte("Sign in to URnetwork"))))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, valid, false)
}

// Accepting both curves must not let one key's signature pass for the
// other's address: the curves never verify each other's signatures.
func TestVerifyBittensorSignatureCurvesDoNotCross(t *testing.T) {
	message := "Sign in to URnetwork\nChallenge: crosscheck\nTimestamp: 1757340000"

	// an sr25519 signature presented for an ed25519 address
	srSecret, _, err := schnorrkel.GenerateKeypair()
	connect.AssertEqual(t, err, nil)
	srSignature, err := srSecret.Sign(schnorrkel.NewSigningContext([]byte("substrate"), []byte(message)))
	connect.AssertEqual(t, err, nil)
	srBytes := srSignature.Encode()
	edPublic, edPrivate, err := ed25519.GenerateKey(nil)
	connect.AssertEqual(t, err, nil)
	var edKey [32]byte
	copy(edKey[:], edPublic)
	valid, _ := VerifyBittensorSignature(testingSS58Encode(42, edKey), message, hex.EncodeToString(srBytes[:]))
	connect.AssertEqual(t, valid, false)

	// an ed25519 signature presented for an sr25519 address
	_, srPublic, err := schnorrkel.GenerateKeypair()
	connect.AssertEqual(t, err, nil)
	valid, err = VerifyBittensorSignature(testingSS58Encode(42, srPublic.Encode()), message, hex.EncodeToString(ed25519.Sign(edPrivate, []byte(message))))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, valid, false)
}

// The marker bit decides the curve. Without it the bytes are an ed25519
// signature that simply does not verify (401); with it they must be a well
// formed schnorrkel signature, or they are an encoding error (400).
func TestVerifyBittensorSignatureMarkerDispatch(t *testing.T) {
	_, publicKey, err := schnorrkel.GenerateKeypair()
	connect.AssertEqual(t, err, nil)
	address := testingSS58Encode(42, publicKey.Encode())

	valid, err := VerifyBittensorSignature(address, "message", strings.Repeat("00", 64))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, valid, false)

	// marker set, but the scalar is not canonical: not a schnorrkel signature
	malformed := strings.Repeat("ff", 64)
	_, err = VerifyBittensorSignature(address, "message", malformed)
	connect.AssertNotEqual(t, err, nil)
}
