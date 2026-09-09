package model

import (
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/ChainSafe/go-schnorrkel"
	"github.com/urnetwork/connect"
)

// The challenge message is the contract every wallet client signs. These
// tests pin the exact text and the parser's deny rules without a database.

func TestWalletAuthChallengeMessageRoundTrip(t *testing.T) {
	challenge := "abc123_-XYZ="
	timestamp := int64(1757340000)

	message := FormatWalletAuthChallengeMessage(challenge, timestamp)
	connect.AssertEqual(t, message, "Sign in to URnetwork\nChallenge: abc123_-XYZ=\nTimestamp: 1757340000")

	parsedChallenge, parsedTimestamp, err := parseWalletAuthChallengeMessage(message)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, parsedChallenge, challenge)
	connect.AssertEqual(t, parsedTimestamp, timestamp)
}

func TestWalletAuthChallengeMessageDenyRules(t *testing.T) {
	// a random string, which is what a generic WalletConnect sign-message
	// flow produces, carries neither a challenge nor a timestamp
	cases := map[string]string{
		"random string":          "3f9c2a7b1d",
		"missing challenge line": "Sign in to URnetwork\nTimestamp: 1757340000",
		"missing timestamp line": "Sign in to URnetwork\nChallenge: abc",
		"wrong header":           "Welcome to URnetwork\nChallenge: abc\nTimestamp: 1757340000",
		"non-numeric timestamp":  "Sign in to URnetwork\nChallenge: abc\nTimestamp: soon",
		"crlf line endings":      "Sign in to URnetwork\r\nChallenge: abc\r\nTimestamp: 1757340000",
		"bytes-wrapped text":     "<Bytes>Sign in to URnetwork\nChallenge: abc\nTimestamp: 1757340000</Bytes>",
		"empty":                  "",
	}
	for name, message := range cases {
		_, _, err := parseWalletAuthChallengeMessage(message)
		if err == nil {
			t.Fatalf("%s: expected the message to be rejected", name)
		}
	}

	// extra trailing lines ride along in the timestamp field and are rejected
	_, _, err := parseWalletAuthChallengeMessage("Sign in to URnetwork\nChallenge: abc\nTimestamp: 1757340000\nextra")
	connect.AssertNotEqual(t, err, nil)
}

// A Bittensor wallet signs the issued message text; polkadot-js style signers
// wrap it in <Bytes>…</Bytes> before signing, and the server accepts both the
// wrapped and the raw form while the client always submits the unwrapped text.
func TestWalletAuthChallengeMessageBittensorSignatureForms(t *testing.T) {
	message := FormatWalletAuthChallengeMessage("c2hhcmVkLWNoYWxsZW5nZQ==", 1757340000)

	secretKey, publicKey, err := schnorrkel.GenerateKeypair()
	connect.AssertEqual(t, err, nil)
	publicKeyBytes := publicKey.Encode()
	address := testingSS58Encode(42, publicKeyBytes)

	sign := func(payload string) string {
		transcript := schnorrkel.NewSigningContext([]byte("substrate"), []byte(payload))
		signature, err := secretKey.Sign(transcript)
		connect.AssertEqual(t, err, nil)
		signatureBytes := signature.Encode()
		return "0x" + hex.EncodeToString(signatureBytes[:])
	}

	// wrapped form, as WalletConnect / polkadot signRaw produce it
	valid, err := VerifyBittensorSignature(address, message, sign("<Bytes>"+message+"</Bytes>"))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, valid, true)

	// raw form
	valid, err = VerifyBittensorSignature(address, message, sign(message))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, valid, true)

	// a signature over a different challenge text does not verify
	other := FormatWalletAuthChallengeMessage("b3RoZXI=", 1757340000)
	valid, err = VerifyBittensorSignature(address, message, sign("<Bytes>"+other+"</Bytes>"))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, valid, false)

	// the dispatcher accepts both blockchain spellings the clients may send
	for _, blockchain := range []string{"TAO", "tao", "bittensor"} {
		valid, err = VerifySignature(blockchain, address, message, sign("<Bytes>"+message+"</Bytes>"))
		connect.AssertEqual(t, err, nil)
		if !valid {
			t.Fatalf("%s: expected the signature to verify", blockchain)
		}
	}

	// the parser sees the unwrapped text the client submits, so the wrapped
	// text must never be sent as wallet_message
	_, _, err = parseWalletAuthChallengeMessage(fmt.Sprintf("<Bytes>%s</Bytes>", message))
	connect.AssertNotEqual(t, err, nil)
	connect.AssertEqual(t, strings.HasPrefix(message, "Sign in to URnetwork\n"), true)
}
