package server

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
)

// Symmetric encryption for the wg handoff blobs persisted in redis.
//
// The handoff (network_client_proxy_wg_handoff_model.go) carries each wg
// peer's learned public ip:port endpoint so a replacement proxy instance can
// re-initiate toward its peers. The endpoint is a raw client address, and the
// blob pairs it with the tunnel-internal ClientIpv4, which joins to
// proxy_client.client_id — so at rest the blob is de-anonymizing while it
// lives (ttl ~10 minutes). It cannot be hashed like the other address stores:
// the consumer needs the real endpoint back. So it is sealed here instead —
// anywhere a raw ip must be persisted AND recovered, it is persisted
// encrypted.
//
// The key lives in vault/<env>/wireguard.yml under `handoff_encryption_key`
// (any string; it is stretched through sha256 to the aes key), loaded the
// same way as the client ip hash pepper (client.yml). AES-256-GCM with a
// random nonce; the sealed form is "enc1:" + base64(nonce || ciphertext) so
// a reader can distinguish sealed blobs from the legacy plain-json ones
// still draining out of redis during a deploy.

const wgHandoffSealedPrefix = "enc1:"

var wgHandoffKey = sync.OnceValue(func() []byte {
	wireguardKeys := Vault.RequireSimpleResource("wireguard.yml")
	keyMaterial := wireguardKeys.RequireString("handoff_encryption_key")
	key := sha256.Sum256([]byte(keyMaterial))
	return key[:]
})

func wgHandoffAead() cipher.AEAD {
	block, err := aes.NewCipher(wgHandoffKey())
	if err != nil {
		panic(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	return aead
}

// WgHandoffSeal encrypts a handoff blob for persistence.
func WgHandoffSeal(plain []byte) string {
	aead := wgHandoffAead()
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	sealed := aead.Seal(nonce, nonce, plain, nil)
	return wgHandoffSealedPrefix + base64.StdEncoding.EncodeToString(sealed)
}

// WgHandoffOpen decrypts a persisted handoff blob. `sealed` reports whether
// the value was in the sealed format at all — a legacy plain-json blob
// (written before encryption, still inside its ttl during a deploy) returns
// sealed=false and the caller may fall back to reading it directly.
func WgHandoffOpen(value string) (plain []byte, sealed bool, err error) {
	if !strings.HasPrefix(value, wgHandoffSealedPrefix) {
		return nil, false, nil
	}
	raw, err := base64.StdEncoding.DecodeString(value[len(wgHandoffSealedPrefix):])
	if err != nil {
		return nil, true, err
	}
	aead := wgHandoffAead()
	if len(raw) < aead.NonceSize() {
		return nil, true, fmt.Errorf("sealed wg handoff too short")
	}
	plain, err = aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], nil)
	if err != nil {
		return nil, true, err
	}
	return plain, true, nil
}
