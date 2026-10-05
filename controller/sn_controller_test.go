package controller

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ChainSafe/go-schnorrkel"
	"github.com/urnetwork/connect"

	"github.com/urfoundation/sn/merkle"
	"github.com/urfoundation/sn/ss58"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// testSnPayoutLeaves builds a synthetic committed leaf set in leaf_index
// order, one distinct coldkey per leaf.
func testSnPayoutLeaves(n int) []*model.StPayoutLeaf {
	leaves := make([]*model.StPayoutLeaf, n)
	for i := 0; i < n; i += 1 {
		var coldkey [32]byte
		coldkey[0] = byte(i + 1)
		coldkey[31] = 0xa0
		leaves[i] = &model.StPayoutLeaf{
			Epoch:     7,
			NoId:      1,
			Coldkey:   coldkey,
			ShareBps:  (i + 1) * 100,
			LeafIndex: i,
		}
	}
	return leaves
}

func TestSnPoolClaimProof(t *testing.T) {
	leaves := testSnPayoutLeaves(5)

	// every leaf's proof must verify against the one shared root
	var root [32]byte
	for i, leaf := range leaves {
		leafRoot, proof, err := snPoolClaimProof(leaves, leaf.Coldkey, leaf.ShareBps)
		connect.AssertEqual(t, err, nil)
		if i == 0 {
			root = leafRoot
		} else {
			connect.AssertEqual(t, leafRoot, root)
		}

		claimLeaf := merkle.PayoutLeaf(leaf.Coldkey, big.NewInt(int64(leaf.ShareBps)))
		connect.AssertEqual(t, merkle.Verify(root, claimLeaf, proof), true)
	}

	// a proof must not verify for a different leaf
	otherLeaf := merkle.PayoutLeaf(leaves[1].Coldkey, big.NewInt(int64(leaves[1].ShareBps)))
	_, proof, err := snPoolClaimProof(leaves, leaves[0].Coldkey, leaves[0].ShareBps)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, merkle.Verify(root, otherLeaf, proof), false)
}

func TestSnPoolClaimProofColdkeyNotInSet(t *testing.T) {
	leaves := testSnPayoutLeaves(5)

	// a coldkey with no leaf gets no proof
	var unknown [32]byte
	unknown[0] = 0xff
	_, _, err := snPoolClaimProof(leaves, unknown, 100)
	connect.AssertNotEqual(t, err, nil)

	// a known coldkey with the wrong share is a different leaf: no proof
	_, _, err = snPoolClaimProof(leaves, leaves[0].Coldkey, leaves[0].ShareBps+1)
	connect.AssertNotEqual(t, err, nil)
}

func TestSnPoolClaimProofSingleLeaf(t *testing.T) {
	leaves := testSnPayoutLeaves(1)

	// a single-leaf tree has an empty (non-nil) proof and root == leaf hash
	root, proof, err := snPoolClaimProof(leaves, leaves[0].Coldkey, leaves[0].ShareBps)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, len(proof), 0)

	claimLeaf := merkle.PayoutLeaf(leaves[0].Coldkey, big.NewInt(int64(leaves[0].ShareBps)))
	connect.AssertEqual(t, root, [32]byte(claimLeaf))
	connect.AssertEqual(t, merkle.Verify(root, claimLeaf, proof), true)

	wireProof := snProofBytes(proof)
	connect.AssertNotEqual(t, wireProof, nil)
	connect.AssertEqual(t, len(wireProof), 0)
}

func TestSnNoIdBytes(t *testing.T) {
	// the noId word is the 32-byte big-endian uint256 the contract keys by
	word := snNoIdBytes(1)
	connect.AssertEqual(t, len(word), 32)
	connect.AssertEqual(t, word[31], byte(1))
	connect.AssertEqual(t, new(big.Int).SetBytes(word).Uint64(), uint64(1))

	word = snNoIdBytes(0x0102030405060708)
	connect.AssertEqual(t, len(word), 32)
	connect.AssertEqual(t, new(big.Int).SetBytes(word).Uint64(), uint64(0x0102030405060708))
	for _, b := range word[:24] {
		connect.AssertEqual(t, b, byte(0))
	}
}

// A fresh sr25519 coldkey: its ss58 address (Bittensor prefix) and a signer
// that signs a message in the <Bytes> wrapped form polkadot-js style wallets
// produce. Keys are generated per call, never fixed.
func testSnColdkey(t testing.TB) (string, func(message string) string) {
	t.Helper()
	secretKey, publicKey, err := schnorrkel.GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	address, err := ss58.Encode(publicKey.Encode(), ss58.BittensorPrefix)
	if err != nil {
		t.Fatal(err)
	}
	return address, func(message string) string {
		transcript := schnorrkel.NewSigningContext([]byte("substrate"), []byte("<Bytes>"+message+"</Bytes>"))
		signature, err := secretKey.Sign(transcript)
		if err != nil {
			t.Fatal(err)
		}
		signatureBytes := signature.Encode()
		return "0x" + hex.EncodeToString(signatureBytes[:])
	}
}

// A coldkey signature made with another account than the typed address (the
// TAO.com manual entry, where the user picks the signing account in the
// wallet) is refused with a stable code next to the message, so an app can
// say what went wrong in the user's language. Malformed input and a wrong
// challenge keep their messages and carry no code. None of these cases
// reaches the database: the signature and timestamp gates come first.
func TestSnSetWalletSignatureMismatchCode(t *testing.T) {
	typedAddress, typedSign := testSnColdkey(t)
	_, otherSign := testSnColdkey(t)
	message := model.FormatWalletAuthChallengeMessage("c24td2FsbGV0LW1pc21hdGNo", server.NowUtc().Unix())
	expiredMessage := model.FormatWalletAuthChallengeMessage("c24td2FsbGV0LWV4cGlyZWQ=", server.NowUtc().Add(-time.Hour).Unix())

	clientSession := session.Testing_CreateClientSession(context.Background(), &jwt.ByJwt{
		NetworkId: server.NewId(),
		UserId:    server.NewId(),
	})
	setWallet := func(signature string, message string) *SnSetWalletResult {
		result, err := SnSetWallet(&SnSetWalletArgs{
			ColdkeySs58: typedAddress,
			Signature:   signature,
			Message:     message,
		}, clientSession)
		connect.AssertEqual(t, err, nil)
		if result.Error == nil {
			t.Fatalf("the wallet set was not refused: %+v", result)
		}
		return result
	}

	mismatch := setWallet(otherSign(message), message)
	connect.AssertEqual(t, mismatch.Error.Code, SnSetWalletErrorCodeSignatureMismatch)
	connect.AssertEqual(t, mismatch.Error.Code, "signature_mismatch")
	if mismatch.Error.Message == "" {
		t.Fatalf("the coded refusal needs a message for older clients")
	}
	raw, err := json.Marshal(mismatch)
	connect.AssertEqual(t, err, nil)
	if !strings.Contains(string(raw), `"error":{"code":"signature_mismatch","message":`) {
		t.Fatalf("wire form: %s", raw)
	}

	// the typed account over other text: sr25519 cannot tell this from
	// another account, and signing the shown challenge again fixes both
	otherText := setWallet(typedSign(expiredMessage), message)
	connect.AssertEqual(t, otherText.Error.Code, SnSetWalletErrorCodeSignatureMismatch)

	uncoded := []struct {
		name      string
		signature string
		message   string
		want      string
	}{
		{name: "not hex", signature: "zzzz", message: message, want: "400 invalid signature encoding"},
		{name: "63 bytes", signature: "0x" + strings.Repeat("ab", 63), message: message, want: "400 invalid signature encoding"},
		{name: "not a challenge", signature: typedSign("Sign in to URnetwork"), message: "Sign in to URnetwork", want: "400 invalid message format"},
		{name: "an expired challenge", signature: typedSign(expiredMessage), message: expiredMessage, want: "400 challenge timestamp too old"},
	}
	for _, test := range uncoded {
		result := setWallet(test.signature, test.message)
		if result.Error.Code != "" {
			t.Fatalf("%s: code = %q, want none", test.name, result.Error.Code)
		}
		connect.AssertEqual(t, result.Error.Message, test.want)
		raw, err := json.Marshal(result)
		connect.AssertEqual(t, err, nil)
		if strings.Contains(string(raw), `"code"`) {
			t.Fatalf("%s: the wire form gained a code: %s", test.name, raw)
		}
	}
}

// The same refusal against a real challenge issued for the typed address: a
// signature from another account is coded and leaves the challenge unused, so
// the typed account's own signature over it still connects. A replay of that
// signature is a wrong challenge now, refused as before with no code.
func TestSnSetWalletSignatureMismatchOnIssuedChallenge(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		typedAddress, typedSign := testSnColdkey(t)
		_, otherSign := testSnColdkey(t)

		blockchain := model.TAO.String()
		challenge := model.CreateWalletAuthChallenge(model.WalletAuthChallengeArgs{
			WalletAddress: &typedAddress,
			Blockchain:    &blockchain,
		}, ctx)
		if challenge.Error != nil {
			t.Fatalf("challenge: %s", challenge.Error.Message)
		}
		message := challenge.MessageTemplate

		networkId := server.NewId()
		clientSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			UserId:    server.NewId(),
		})
		setWallet := func(signature string) *SnSetWalletResult {
			result, err := SnSetWallet(&SnSetWalletArgs{
				ColdkeySs58: typedAddress,
				Signature:   signature,
				Message:     message,
			}, clientSession)
			connect.AssertEqual(t, err, nil)
			return result
		}

		mismatch := setWallet(otherSign(message))
		if mismatch.Error == nil || mismatch.Error.Code != SnSetWalletErrorCodeSignatureMismatch {
			t.Fatalf("another account's signature: %+v", mismatch.Error)
		}
		if model.GetStWallet(ctx, networkId) != nil {
			t.Fatal("a refused signature attached a wallet")
		}

		signature := typedSign(message)
		connected := setWallet(signature)
		if connected.Error != nil {
			t.Fatalf("the typed account's signature over the same challenge: %+v", connected.Error)
		}
		wallet := model.GetStWallet(ctx, networkId)
		if wallet == nil || wallet.ColdkeySs58 != typedAddress {
			t.Fatalf("wallet: %+v", wallet)
		}

		replay := setWallet(signature)
		if replay.Error == nil || replay.Error.Code != "" {
			t.Fatalf("replay: %+v", replay.Error)
		}
		connect.AssertEqual(t, replay.Error.Message, "403 challenge already used")
	})
}
