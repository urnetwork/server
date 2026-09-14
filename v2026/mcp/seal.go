package mcp

// Sealed opaque blobs for state threaded back through the caller.
//
// The transport is stateless, so state that must survive from one tool call to
// the next (the cookie jar, a work continuation) rides in the tool result and
// comes back as an argument. That state is credential-bearing -- a jar holds
// session cookies -- and it lands in model context, host logs, and context
// summaries, so it never travels in the clear: each blob is encrypted and
// authenticated under a key derived from the proxy vault secret, is bound to a
// label so a jar cannot be replayed as a continuation, and carries an expiry so
// a leaked blob stops working on its own.
//
// The key is derived from the same vault secret that signs proxy ids, with a
// distinct label, so no new vault entry is needed and the two uses cannot
// collide.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/urnetwork/server/v2026"
)

const sealVersion = 2

// domain separation from the proxy id signature that shares the vault secret
const sealKeyLabel = "urnetwork-mcp-seal-v2"

const (
	sealLabelCookies      = "cookies"
	sealLabelContinuation = "continuation"
	sealLabelProxy        = "proxy"
)

var errSealExpired = errors.New("sealed state expired")

var sealKey = sync.OnceValue(func() []byte {
	proxy := server.Vault.RequireSimpleResource("proxy.yml")
	secrets := proxy.RequireStringList("secrets")

	h := sha256.New()
	h.Write([]byte(sealKeyLabel))
	h.Write([]byte(secrets[0]))
	return h.Sum(nil)
})

var sealAead = sync.OnceValue(func() cipher.AEAD {
	block, err := aes.NewCipher(sealKey())
	if err != nil {
		panic(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	return aead
})

type sealedEnvelope struct {
	Version   int             `json:"v"`
	Label     string          `json:"l"`
	ExpiresAt int64           `json:"e"`
	Payload   json.RawMessage `json:"p"`
}

// Encrypts v into an opaque string bound to label and valid for ttl.
func seal(label string, binding string, v any, ttl time.Duration) (string, error) {
	if binding == "" {
		return "", errors.New("sealed state identity binding is required")
	}
	payload, err := json.Marshal(v)
	if err != nil {
		return "", err
	}

	envelopeJson, err := json.Marshal(&sealedEnvelope{
		Version:   sealVersion,
		Label:     label,
		ExpiresAt: server.NowUtc().Add(ttl).Unix(),
		Payload:   payload,
	})
	if err != nil {
		return "", err
	}

	aead := sealAead()
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}

	// the nonce prefixes the ciphertext so unseal can split it back out
	sealed := aead.Seal(nonce, nonce, envelopeJson, sealAdditionalData(label, binding))
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

// Decrypts a blob produced by seal into v. The label must match the one it was
// sealed under, and an expired blob is rejected with errSealExpired so callers
// can distinguish "stale, start over" from "corrupt or forged".
func unseal(label string, binding string, sealedStr string, v any) error {
	if binding == "" {
		return errors.New("sealed state identity binding is required")
	}
	sealed, err := base64.RawURLEncoding.DecodeString(sealedStr)
	if err != nil {
		return err
	}

	aead := sealAead()
	if len(sealed) < aead.NonceSize() {
		return fmt.Errorf("sealed state too short")
	}

	nonce := sealed[:aead.NonceSize()]
	envelopeJson, err := aead.Open(nil, nonce, sealed[aead.NonceSize():], sealAdditionalData(label, binding))
	if err != nil {
		return err
	}

	var envelope sealedEnvelope
	if err := json.Unmarshal(envelopeJson, &envelope); err != nil {
		return err
	}

	if envelope.Version != sealVersion {
		return fmt.Errorf("unsupported sealed state version %d", envelope.Version)
	}
	// the label is additional authenticated data above, so a mismatch cannot
	// reach here; check anyway so the invariant is stated where it is relied on
	if envelope.Label != label {
		return fmt.Errorf("sealed state label mismatch")
	}
	if server.NowUtc().After(time.Unix(envelope.ExpiresAt, 0)) {
		return errSealExpired
	}

	return json.Unmarshal(envelope.Payload, v)
}

func sealAdditionalData(label string, binding string) []byte {
	return []byte(fmt.Sprintf("%d\x00%s\x00%s", sealVersion, label, binding))
}
