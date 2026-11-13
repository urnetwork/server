package server

// The handoff key under a wireguard.yml whose handoff_encryption_key is blank.
// Each test pushes a synthetic key; nothing reads a deployment vault.

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"testing"
)

// Pushes wireguard.yml for the rest of the test and reloads the handoff key.
func setWgHandoffKeyForTest(t *testing.T, resource string) {
	t.Helper()
	pop := Vault.PushSimpleResource("wireguard.yml", []byte(resource))
	restore := Testing_ReloadWgHandoffKey()
	t.Cleanup(func() {
		restore()
		pop()
	})
}

// A handoff blob sealed under the key material stretched through sha256, as
// WgHandoffSeal forms one.
func wgHandoffSealedUnder(t *testing.T, material string, plain []byte) string {
	t.Helper()
	key := sha256.Sum256([]byte(material))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, aead.NonceSize())
	return wgHandoffSealedPrefix + base64.StdEncoding.EncodeToString(aead.Seal(nonce, nonce, plain, nil))
}

// A blank handoff_encryption_key stretched to sha256(""), so anyone could read
// the persisted peer endpoints, raw client addresses joinable to their client
// ids, and forge handoffs a replacement proxy would follow.
func TestWgHandoffRefusesBlankKey(t *testing.T) {
	plain := []byte("synthetic handoff for 192.0.2.7:51820")
	for _, blank := range []string{"", "   "} {
		setWgHandoffKeyForTest(t, "handoff_encryption_key: '"+blank+"'\n")
		if sealedValue, err := WgHandoffSeal(plain); err == nil {
			t.Fatalf("sealed under the blank key %q: %q", blank, sealedValue)
		}
		opened, sealed, err := WgHandoffOpen(wgHandoffSealedUnder(t, blank, plain))
		if !sealed || err == nil {
			t.Fatalf("a blob sealed under the blank key %q opened: %q", blank, opened)
		}
	}
}

func TestWgHandoffSealsUnderConfiguredKey(t *testing.T) {
	setWgHandoffKeyForTest(t, "handoff_encryption_key: synthetic-handoff-key\n")
	plain := []byte("synthetic handoff for 192.0.2.7:51820")
	sealedValue, err := WgHandoffSeal(plain)
	if err != nil {
		t.Fatal(err)
	}
	opened, sealed, err := WgHandoffOpen(sealedValue)
	if !sealed || err != nil || !bytes.Equal(opened, plain) {
		t.Fatalf("a sealed handoff did not round trip: %v %q", err, opened)
	}
	if opened, sealed, err := WgHandoffOpen(wgHandoffSealedUnder(t, "", plain)); !sealed || err == nil {
		t.Fatalf("a blob sealed under the blank key opened: %q", opened)
	}
}
