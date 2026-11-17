package model

// Phase 14b (connect/EXTENDER.md P6; connect/EXTENDER-UPDATE2.md §3): the
// camouflage key an activation posts is stored only by a passing activation of
// the network that holds the identity key, and every reader a record is signed
// from returns it only while that network still holds the key.

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// A fresh X25519 public key, as an extender with camouflage on posts.
func testRealityPublicKey(t testing.TB) []byte {
	t.Helper()
	privateKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return privateKey.PublicKey().Bytes()
}

// A signer that records the camouflage key each record it signs would carry.
func testRealityKeySigner() (sign NetworkExtenderRecordSigner, signedKeys func() [][]byte) {
	keys := [][]byte{}
	sign = func(
		extender *NetworkExtender,
		addresses []*NetworkExtenderAddress,
		issueTime time.Time,
	) ([]byte, error) {
		keys = append(keys, extender.RecordRealityPublicKey())
		return []byte("record"), nil
	}
	return sign, func() [][]byte { return keys }
}

// The camouflage key each signing reader returns for one extender: the drip
// and dns reader, the uptime probe targets, and the activation bootstrap
// sample. A reader that does not list the extender is reported as nil.
func testReaderRealityPublicKeys(ctx context.Context, extenderId server.Id) map[string][]byte {
	keys := map[string][]byte{
		"record": nil,
		"probe":  nil,
		"sample": nil,
	}
	if extender, _ := GetActiveNetworkExtenderForRecord(ctx, extenderId); extender != nil {
		keys["record"] = extender.RecordRealityPublicKey()
	}
	for _, target := range GetActiveNetworkExtenderProbeTargets(ctx) {
		if target.ExtenderId == extenderId {
			keys["probe"] = target.RealityPublicKey
		}
	}
	for _, sampled := range GetRandomActiveNetworkExtenders(ctx, 1024, server.Id{}) {
		if sampled.Extender.ExtenderId == extenderId {
			keys["sample"] = sampled.Extender.RecordRealityPublicKey()
		}
	}
	return keys
}

func requireReaderRealityPublicKeys(t testing.TB, ctx context.Context, extenderId server.Id, want []byte) {
	t.Helper()
	for reader, key := range testReaderRealityPublicKeys(ctx, extenderId) {
		if !bytes.Equal(key, want) {
			t.Fatalf("the %s reader returned camouflage key %x, expected %x", reader, key, want)
		}
	}
}

// A passing activation stores its key for the network that posted it: the
// record it signs, the drip's record and every other signing reader carry it.
// A later activation without a key clears it everywhere.
func TestActivateNetworkExtenderStoresTheCamouflageKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		publicKey := []byte("extender-public-key-reality-0001")
		realityPublicKey := testRealityPublicKey(t)
		sign, signedKeys := testRealityKeySigner()

		activation := testExtenderActivation(publicKey, 4, "192.0.2.71")
		activation.RealityPublicKey = realityPublicKey
		activated := ActivateNetworkExtender(ctx, activation, sign)
		extenderId := activated.Extender.ExtenderId
		if !bytes.Equal(signedKeys()[0], realityPublicKey) {
			t.Fatalf("the activation's record carries camouflage key %x, expected %x", signedKeys()[0], realityPublicKey)
		}
		stored, postingNetworkId := Testing_GetNetworkExtenderStoredRealityPublicKey(ctx, extenderId)
		if !bytes.Equal(stored, realityPublicKey) {
			t.Fatalf("stored camouflage key %x, expected %x", stored, realityPublicKey)
		}
		connect.AssertEqual(t, *postingNetworkId, activation.NetworkId)
		requireReaderRealityPublicKeys(t, ctx, extenderId, realityPublicKey)

		connect.AssertEqual(t, PublishNetworkExtenderRecord(ctx, extenderId, sign), true)
		if dripKey := signedKeys()[len(signedKeys())-1]; !bytes.Equal(dripKey, realityPublicKey) {
			t.Fatalf("the drip's record carries camouflage key %x, expected %x", dripKey, realityPublicKey)
		}

		// the other family of the same extender keeps the one key
		other := testExtenderActivation(publicKey, 6, "2001:db8::71")
		other.RealityPublicKey = realityPublicKey
		ActivateNetworkExtender(ctx, other, sign)
		requireReaderRealityPublicKeys(t, ctx, extenderId, realityPublicKey)

		// camouflage turned off: the next passing activation clears the key
		activation.RealityPublicKey = nil
		ActivateNetworkExtender(ctx, activation, sign)
		if key := signedKeys()[len(signedKeys())-1]; len(key) != 0 {
			t.Fatalf("an activation without a camouflage key signed %x", key)
		}
		stored, _ = Testing_GetNetworkExtenderStoredRealityPublicKey(ctx, extenderId)
		connect.AssertEqual(t, len(stored), 0)
		requireReaderRealityPublicKeys(t, ctx, extenderId, nil)
	})
}

// Another network's activation of a stored key, with a camouflage key of its
// own, stores nothing (fix/extender-key-ownership) and signs nothing: the
// owner's key stays, posted by the owner, and every record carries it.
func TestActivateNetworkExtenderKeepsTheCamouflageKeyFromAnotherNetwork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		publicKey := []byte("extender-public-key-reality-0002")
		ownerRealityPublicKey := testRealityPublicKey(t)
		sign, signedKeys := testRealityKeySigner()

		owner := testExtenderActivation(publicKey, 4, "192.0.2.72")
		owner.RealityPublicKey = ownerRealityPublicKey
		activated := ActivateNetworkExtender(ctx, owner, sign)
		extenderId := activated.Extender.ExtenderId
		signedCount := len(signedKeys())

		for _, ipVersion := range []int{4, 6} {
			ip := "192.0.2.73"
			if ipVersion == 6 {
				ip = "2001:db8::73"
			}
			other := testExtenderActivation(publicKey, ipVersion, ip)
			other.NetworkId = server.NewId()
			other.RealityPublicKey = testRealityPublicKey(t)
			if moved := ActivateNetworkExtender(ctx, other, sign); moved != nil {
				t.Fatalf("another network's activation of the key was stored (ipv%d)", ipVersion)
			}
		}
		connect.AssertEqual(t, len(signedKeys()), signedCount)

		stored, postingNetworkId := Testing_GetNetworkExtenderStoredRealityPublicKey(ctx, extenderId)
		if !bytes.Equal(stored, ownerRealityPublicKey) {
			t.Fatalf("stored camouflage key %x after another network's post, expected the owner's %x", stored, ownerRealityPublicKey)
		}
		connect.AssertEqual(t, *postingNetworkId, owner.NetworkId)
		requireReaderRealityPublicKeys(t, ctx, extenderId, ownerRealityPublicKey)
	})
}

// A key that reaches another network without an activation, as a support
// correction moves one, carries no camouflage key: the stored key was posted
// by the network the identity left. The new network's own passing activation
// stores its key, and the records carry that.
func TestNetworkExtenderCamouflageKeyStaysWithItsNetwork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		publicKey := []byte("extender-public-key-reality-0003")
		sign, signedKeys := testRealityKeySigner()

		owner := testExtenderActivation(publicKey, 4, "192.0.2.74")
		owner.RealityPublicKey = testRealityPublicKey(t)
		activated := ActivateNetworkExtender(ctx, owner, sign)
		extenderId := activated.Extender.ExtenderId

		newNetworkId := server.NewId()
		Testing_MoveNetworkExtenderToNetwork(ctx, extenderId, newNetworkId)
		requireReaderRealityPublicKeys(t, ctx, extenderId, nil)
		connect.AssertEqual(t, PublishNetworkExtenderRecord(ctx, extenderId, sign), true)
		if key := signedKeys()[len(signedKeys())-1]; len(key) != 0 {
			t.Fatalf("a record signed after the key moved networks carries camouflage key %x", key)
		}

		next := testExtenderActivation(publicKey, 4, "192.0.2.74")
		next.NetworkId = newNetworkId
		next.RealityPublicKey = testRealityPublicKey(t)
		if reactivated := ActivateNetworkExtender(ctx, next, sign); reactivated == nil {
			t.Fatal("the key's new network could not activate it")
		}
		requireReaderRealityPublicKeys(t, ctx, extenderId, next.RealityPublicKey)
	})
}

// The gated release reader returns the key with the same guard: a gated
// extender's records carry the key its owner posted, and none once the key has
// moved to another network.
func TestGatedNetworkExtenderCamouflageKeyStaysWithItsNetwork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		publicKey := []byte("extender-public-key-reality-0004")
		realityPublicKey := testRealityPublicKey(t)
		sign, _ := testRealityKeySigner()

		activation := testExtenderActivation(publicKey, 4, "192.0.2.75")
		activation.DirectoryTier = connect.ExtenderDirectoryTierGated
		activation.RealityPublicKey = realityPublicKey
		activated := ActivateNetworkExtender(ctx, activation, sign)
		extenderId := activated.Extender.ExtenderId

		gatedKey := func() ([]byte, bool) {
			for _, gated := range GetActiveGatedNetworkExtenders(ctx) {
				if gated.Extender.ExtenderId == extenderId {
					return gated.Extender.RecordRealityPublicKey(), true
				}
			}
			return nil, false
		}
		key, listed := gatedKey()
		connect.AssertEqual(t, listed, true)
		if !bytes.Equal(key, realityPublicKey) {
			t.Fatalf("the gated reader returned camouflage key %x, expected %x", key, realityPublicKey)
		}

		Testing_MoveNetworkExtenderToNetwork(ctx, extenderId, server.NewId())
		key, listed = gatedKey()
		connect.AssertEqual(t, listed, true)
		if len(key) != 0 {
			t.Fatalf("the gated reader returned camouflage key %x after the key moved networks", key)
		}
	})
}

// A stored value that is not an X25519 key's size is never signed: a record
// carries a camouflage key a client can use, or none.
func TestNetworkExtenderRecordRealityPublicKeyRefusesAMalformedKey(t *testing.T) {
	for _, key := range [][]byte{nil, {}, make([]byte, 31), make([]byte, 33)} {
		extender := &NetworkExtender{RealityPublicKey: key}
		if recordKey := extender.RecordRealityPublicKey(); recordKey != nil {
			t.Fatalf("a %d-byte stored key was signed as %x", len(key), recordKey)
		}
	}
	key := make([]byte, 32)
	key[0] = 9
	extender := &NetworkExtender{RealityPublicKey: key}
	recordKey := extender.RecordRealityPublicKey()
	if !bytes.Equal(recordKey, key) {
		t.Fatalf("a 32-byte key was signed as %x", recordKey)
	}
	// the caller's copy, never the stored slice
	recordKey[0] = 1
	connect.AssertEqual(t, extender.RealityPublicKey[0], byte(9))
}
