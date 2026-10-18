package mcp

// The seal key under a proxy.yml whose secrets include blank entries. Each
// test pushes synthetic secrets; nothing reads a deployment vault.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Pushes proxy.yml for the rest of the test and makes the memoized seal cipher
// load it again.
func setSealSecretsForTest(t *testing.T, resource string) {
	t.Helper()
	pop := server.Vault.PushSimpleResource("proxy.yml", []byte(resource))
	previous := sealAead
	sealAead = sync.OnceValues(loadSealAead)
	t.Cleanup(func() {
		sealAead = previous
		pop()
	})
}

// A blob sealed under the key a blank secret gave: a hash of public constants.
func sealUnderBlankSecret(t *testing.T, blank string) string {
	t.Helper()
	h := sha256.New()
	h.Write([]byte(sealKeyLabel))
	h.Write([]byte(blank))
	block, err := aes.NewCipher(h.Sum(nil))
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	previous := sealAead
	sealAead = func() (cipher.AEAD, error) { return aead, nil }
	defer func() { sealAead = previous }()
	sealed, err := seal(sealLabelCookies, "synthetic-binding", map[string]string{"cookie": "synthetic"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

// A blank first secret keyed the seal from public constants alone, so anyone
// could seal a cookie jar or continuation the server would accept. Without a
// non-blank secret, nothing is sealed or unsealed.
func TestSealRefusesBlankProxySecret(t *testing.T) {
	for _, blank := range []string{"", "   "} {
		forged := sealUnderBlankSecret(t, blank)
		setSealSecretsForTest(t, "secrets:\n  - '"+blank+"'\n")
		if sealed, err := seal(sealLabelCookies, "synthetic-binding", map[string]string{"cookie": "synthetic"}, time.Hour); err == nil {
			t.Fatalf("sealed under the blank secret %q: %q", blank, sealed)
		}
		var opened map[string]string
		if err := unseal(sealLabelCookies, "synthetic-binding", forged, &opened); err == nil {
			t.Fatalf("a blob sealed under the blank secret %q was unsealed: %v", blank, opened)
		}
	}
}

// With a non-blank secret after a blank one, the seal is keyed from that
// secret, the one that signs proxy ids, and a blob keyed from the blank one is
// refused.
func TestSealUsesFirstNonblankProxySecret(t *testing.T) {
	forged := sealUnderBlankSecret(t, "")
	setSealSecretsForTest(t, "secrets:\n  - ''\n  - synthetic-proxy-signing-secret\n")
	sealed, err := seal(sealLabelCookies, "synthetic-binding", map[string]string{"cookie": "synthetic"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var opened map[string]string
	if err := unseal(sealLabelCookies, "synthetic-binding", sealed, &opened); err != nil || opened["cookie"] != "synthetic" {
		t.Fatalf("a sealed blob did not round trip: %v %v", err, opened)
	}
	opened = nil
	if err := unseal(sealLabelCookies, "synthetic-binding", forged, &opened); err == nil {
		t.Fatalf("a blob sealed under the blank secret was unsealed: %v", opened)
	}
}
