package model

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ChainSafe/go-schnorrkel"
	"github.com/gagliardetto/solana-go"
	"github.com/mr-tron/base58"
	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"golang.org/x/crypto/blake2b"
)

// These cover the wallet-auth hardening that has no database dependency.
// UseWalletAuthChallenge answers the address, message-format, timestamp and
// signature gates before it opens a transaction, so every case here runs
// without the local stack.

// testingSS58EncodeWithPrefix renders a public key under any ss58 network
// prefix type, including the two byte form (64..16383). testingSS58Encode
// only covers the single byte prefixes.
func testingSS58EncodeWithPrefix(prefix uint16, publicKey [32]byte) string {
	var prefixBytes []byte
	if prefix <= 63 {
		prefixBytes = []byte{byte(prefix)}
	} else {
		prefixBytes = []byte{
			byte(prefix&0b0000_0000_1111_1100)>>2 | 0b0100_0000,
			byte(prefix>>8) | byte(prefix&0b0000_0000_0000_0011)<<6,
		}
	}
	data := append(prefixBytes, publicKey[:]...)
	hasher, _ := blake2b.New512(nil)
	hasher.Write([]byte(ss58Prefix))
	hasher.Write(data)
	checksum := hasher.Sum(nil)
	return base58.Encode(append(data, checksum[:2]...))
}

type testingBittensorWallet struct {
	address   string
	publicKey [32]byte
	sign      func(message string) string
}

func newTestingBittensorWallet(t testing.TB) testingBittensorWallet {
	t.Helper()
	secretKey, publicKey, err := schnorrkel.GenerateKeypair()
	connect.AssertEqual(t, err, nil)
	publicKeyBytes := publicKey.Encode()
	return testingBittensorWallet{
		address:   testingSS58EncodeWithPrefix(BittensorSS58Prefix, publicKeyBytes),
		publicKey: publicKeyBytes,
		sign: func(message string) string {
			// the wrapped form every polkadot-js / WalletConnect signer produces
			transcript := schnorrkel.NewSigningContext([]byte("substrate"), []byte("<Bytes>"+message+"</Bytes>"))
			signature, err := secretKey.Sign(transcript)
			connect.AssertEqual(t, err, nil)
			signatureBytes := signature.Encode()
			return "0x" + hex.EncodeToString(signatureBytes[:])
		},
	}
}

// One sr25519 key must map to exactly one accepted address string. Re-encoding
// it under another network prefix yields a different string that every
// uniqueness check in the account model treats as a different wallet, so
// accepting those prefixes let one key mint unlimited distinct identities.
func TestBittensorAddressNetworkPrefixIsPinned(t *testing.T) {
	wallet := newTestingBittensorWallet(t)
	message := FormatWalletAuthChallengeMessage("cHJlZml4LXBpbm5pbmc=", 1757340000)
	signature := wallet.sign(message)

	// prefix 42 is the one Bittensor uses, and the only one accepted
	connect.AssertEqual(t, IsValidBittensorAddress(wallet.address), true)
	valid, err := VerifyBittensorSignature(wallet.address, message, signature)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, valid, true)

	// 0 polkadot, 2 kusama, 5 astar, 16383 the top of the two byte range
	distinct := map[string]bool{wallet.address: true}
	for _, prefix := range []uint16{0, 2, 5, 16383} {
		address := testingSS58EncodeWithPrefix(prefix, wallet.publicKey)
		distinct[address] = true

		if IsValidBittensorAddress(address) {
			t.Fatalf("prefix %d: address %s was accepted as a bittensor address", prefix, address)
		}
		// the checksum is still well formed -- it is the prefix that is
		// rejected, and the prefix-agnostic decoder still reads it
		publicKey, err := DecodeSS58Address(address)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, publicKey, wallet.publicKey)
		_, err = DecodeBittensorAddress(address)
		connect.AssertNotEqual(t, err, nil)

		// the same signature must not verify under the alternate encoding
		valid, _ := VerifyBittensorSignature(address, message, signature)
		if valid {
			t.Fatalf("prefix %d: one signature verified under a second address string", prefix)
		}
	}
	// the point of the pinning: these really are distinct identity strings
	connect.AssertEqual(t, len(distinct), 5)
}

// An address that only the pinned decoder rejects must fail at the wallet
// address gate, with a 400 and no transaction.
func TestUseWalletAuthChallengeBittensorAddressGate(t *testing.T) {
	wallet := newTestingBittensorWallet(t)
	message := FormatWalletAuthChallengeMessage("YWRkcmVzcy1nYXRl", server.NowUtc().Unix())
	signature := wallet.sign(message)

	cases := map[string]string{
		"malformed ss58":      "not-a-valid-ss58-address",
		"empty":               "",
		"bad checksum":        "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQZ",
		"polkadot prefix 0":   testingSS58EncodeWithPrefix(0, wallet.publicKey),
		"two byte prefix":     testingSS58EncodeWithPrefix(16383, wallet.publicKey),
		"solana address form": solana.NewWallet().PublicKey().String(),
	}
	for name, address := range cases {
		result, err := UseWalletAuthChallenge(&UseWalletAuthChallengeArgs{
			Blockchain: "tao",
			PublicKey:  address,
			Message:    message,
			Signature:  signature,
		}, context.Background())
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Valid, false)
		if !strings.HasPrefix(result.Error.Message, "400 invalid wallet address") {
			t.Fatalf("%s: error = %q, want 400 invalid wallet address", name, result.Error.Message)
		}
	}
}

// A signature the server cannot decode is client input, not a server fault.
// Both chains must answer 4xx in the result body and never return a transport
// error, which the router turns into a 500 carrying the raw text.
func TestUseWalletAuthChallengeSignatureErrorsStay4xx(t *testing.T) {
	ctx := context.Background()
	timestamp := server.NowUtc().Unix()
	message := FormatWalletAuthChallengeMessage("c2lnbmF0dXJlLXNoYXBl", timestamp)

	wallet := newTestingBittensorWallet(t)
	solanaWallet := solana.NewWallet()

	cases := []struct {
		name       string
		blockchain string
		publicKey  string
		signature  string
		wantPrefix string
	}{
		{
			name:       "tao 63 bytes",
			blockchain: "tao",
			publicKey:  wallet.address,
			signature:  strings.Repeat("ab", 63),
			wantPrefix: "400 invalid signature encoding",
		},
		{
			name:       "tao 64 zero bytes",
			blockchain: "tao",
			publicKey:  wallet.address,
			signature:  strings.Repeat("00", 64),
			wantPrefix: "400 invalid signature encoding",
		},
		{
			name:       "tao non hex",
			blockchain: "bittensor",
			publicKey:  wallet.address,
			signature:  "zzzz",
			wantPrefix: "400 invalid signature encoding",
		},
		{
			name:       "sol 63 bytes",
			blockchain: "sol",
			publicKey:  solanaWallet.PublicKey().String(),
			signature:  base64.StdEncoding.EncodeToString(make([]byte, 63)),
			wantPrefix: "400 invalid signature encoding",
		},
		{
			name:       "sol 65 bytes",
			blockchain: "solana",
			publicKey:  solanaWallet.PublicKey().String(),
			signature:  base64.StdEncoding.EncodeToString(make([]byte, 65)),
			wantPrefix: "400 invalid signature encoding",
		},
		{
			name:       "sol not base64",
			blockchain: "SOL",
			publicKey:  solanaWallet.PublicKey().String(),
			signature:  "!!!not base64!!!",
			wantPrefix: "400 invalid signature encoding",
		},
		{
			name:       "tao well formed but wrong key",
			blockchain: "TAO",
			publicKey:  newTestingBittensorWallet(t).address,
			signature:  wallet.sign(message),
			wantPrefix: "401 invalid signature",
		},
	}

	for _, test := range cases {
		result, err := UseWalletAuthChallenge(&UseWalletAuthChallengeArgs{
			Blockchain: test.blockchain,
			PublicKey:  test.publicKey,
			Message:    message,
			Signature:  test.signature,
		}, ctx)
		// the load bearing assertion: no transport error, so no 500
		if err != nil {
			t.Fatalf("%s: returned a transport error %v (this becomes a 500)", test.name, err)
		}
		connect.AssertEqual(t, result.Valid, false)
		if !strings.HasPrefix(result.Error.Message, test.wantPrefix) {
			t.Fatalf("%s: error = %q, want prefix %q", test.name, result.Error.Message, test.wantPrefix)
		}
	}
}

// A fixed size solana.Signature silently zero pads a short signature and
// truncates a long one. Reject instead, matching the Bittensor verifier.
func TestVerifySolanaSignatureLength(t *testing.T) {
	address := solana.NewWallet().PublicKey().String()
	message := "Sign in to URnetwork"

	for _, length := range []int{0, 63, 65, 128} {
		valid, err := VerifySolanaSignature(
			address,
			message,
			base64.StdEncoding.EncodeToString(make([]byte, length)),
		)
		connect.AssertEqual(t, valid, false)
		if err == nil {
			t.Fatalf("%d byte signature: expected a length error", length)
		}
		if !errors.Is(err, ErrWalletSignatureEncoding) {
			t.Fatalf("%d byte signature: err = %v, want an encoding error", length, err)
		}
	}
}

// The wall-clock timestamp gate must bound the advertised lifetime, not the
// one minute skew. It runs before signature verification, so a challenge that
// is merely 61 seconds old used to be rejected as "too old" and never reached
// the signature or the row at all -- which is exactly what a WalletConnect
// round trip to a phone costs.
func TestUseWalletAuthChallengeTimestampWindow(t *testing.T) {
	ctx := context.Background()
	wallet := newTestingBittensorWallet(t)
	now := server.NowUtc().Unix()

	// a deliberately unverifiable signature, so a message that gets past the
	// timestamp gate stops at the signature gate rather than needing the
	// database. The distinction between the two errors is the whole test.
	badSignature := strings.Repeat("ab", 64)

	insideWindow := []int64{0, 30, 61, 90, 240, 299}
	for _, age := range insideWindow {
		message := FormatWalletAuthChallengeMessage("dGltZXN0YW1wLXdpbmRvdw==", now-age)
		result, err := UseWalletAuthChallenge(&UseWalletAuthChallengeArgs{
			Blockchain: "tao",
			PublicKey:  wallet.address,
			Message:    message,
			Signature:  badSignature,
		}, ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Valid, false)
		if strings.Contains(result.Error.Message, "timestamp too old") {
			t.Fatalf("age %ds: rejected as too old, inside the %ds lifetime", age, int64(WalletAuthChallengeLifetime/time.Second))
		}
	}

	// past the lifetime plus the skew the wall-clock gate takes over again
	for _, age := range []int64{400, 3600} {
		message := FormatWalletAuthChallengeMessage("dGltZXN0YW1wLXdpbmRvdw==", now-age)
		result, err := UseWalletAuthChallenge(&UseWalletAuthChallengeArgs{
			Blockchain: "tao",
			PublicKey:  wallet.address,
			Message:    message,
			Signature:  badSignature,
		}, ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Valid, false)
		connect.AssertEqual(t, result.Error.Message, "400 challenge timestamp too old")
	}

	// the future bound is unchanged: a client cannot mint a timestamp ahead
	for _, ahead := range []int64{90, 3600} {
		message := FormatWalletAuthChallengeMessage("dGltZXN0YW1wLXdpbmRvdw==", now+ahead)
		result, err := UseWalletAuthChallenge(&UseWalletAuthChallengeArgs{
			Blockchain: "tao",
			PublicKey:  wallet.address,
			Message:    message,
			Signature:  badSignature,
		}, ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Valid, false)
		connect.AssertEqual(t, result.Error.Message, "400 challenge timestamp too far in the future")
	}
}

// expires_in is on the wire and documented as 300; the gates must be derived
// from the same constant so they cannot drift apart again.
func TestWalletAuthChallengeLifetimeMatchesPublishedContract(t *testing.T) {
	connect.AssertEqual(t, int64(WalletAuthChallengeLifetime/time.Second), int64(300))
}

// The blockchain identifier is case insensitive and accepts both spellings on
// every wallet-auth gate, not just the verifier dispatcher.
func TestUseWalletAuthChallengeBlockchainSpellings(t *testing.T) {
	ctx := context.Background()
	wallet := newTestingBittensorWallet(t)
	message := FormatWalletAuthChallengeMessage("c3BlbGxpbmdz", server.NowUtc().Unix())
	badSignature := strings.Repeat("ab", 64)

	for _, blockchain := range []string{"tao", "TAO", "Tao", "bittensor", "BITTENSOR", "Bittensor"} {
		result, err := UseWalletAuthChallenge(&UseWalletAuthChallengeArgs{
			Blockchain: blockchain,
			PublicKey:  wallet.address,
			Message:    message,
			Signature:  badSignature,
		}, ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Valid, false)
		// it reached the signature gate, so the chain and the address parsed
		if !strings.HasPrefix(result.Error.Message, "400 invalid signature encoding") {
			t.Fatalf("%s: error = %q, want the signature gate", blockchain, result.Error.Message)
		}
	}

	// a solana address never resolves as a bittensor one and vice versa
	result, err := UseWalletAuthChallenge(&UseWalletAuthChallengeArgs{
		Blockchain: "tao",
		PublicKey:  solana.NewWallet().PublicKey().String(),
		Message:    message,
		Signature:  badSignature,
	}, ctx)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Error.Message, "400 invalid wallet address")
}

// filterWalletAuthsByBlockchain must resolve a legacy lowercase 'solana' row
// for a caller presenting the canonical 'SOL', and must not let a row for
// another chain decide the account.
func TestFilterWalletAuthsByBlockchain(t *testing.T) {
	userId := server.NewId()
	address := "wallet-address"
	rows := []NetworkUserWalletAuth{
		{UserId: &userId, WalletAddress: &address, Blockchain: "solana"},
		{UserId: &userId, WalletAddress: &address, Blockchain: "SOL"},
		{UserId: &userId, WalletAddress: &address, Blockchain: "TAO"},
		{UserId: &userId, WalletAddress: &address, Blockchain: "bittensor"},
		{UserId: &userId, WalletAddress: &address, Blockchain: "nonsense"},
	}

	connect.AssertEqual(t, len(filterWalletAuthsByBlockchain(rows, "SOL")), 2)
	connect.AssertEqual(t, len(filterWalletAuthsByBlockchain(rows, "solana")), 2)
	// an empty blockchain defaults to solana, as every other wallet-auth gate does
	connect.AssertEqual(t, len(filterWalletAuthsByBlockchain(rows, "")), 2)
	connect.AssertEqual(t, len(filterWalletAuthsByBlockchain(rows, "TAO")), 2)
	connect.AssertEqual(t, len(filterWalletAuthsByBlockchain(rows, "bittensor")), 2)
	connect.AssertEqual(t, len(filterWalletAuthsByBlockchain(rows, "ethereum")), 0)
	connect.AssertEqual(t, len(filterWalletAuthsByBlockchain(rows, "nonsense")), 0)
}
