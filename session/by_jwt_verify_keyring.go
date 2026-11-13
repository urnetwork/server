// The by jwt verify-only keyring (plan-SOCK-client-auth, QD-040; §19.4): the
// public keys of the by jwt keyring, as a block that mounts no vault reads them
// from a jwk set in its secret_files. sockets verifies Client-class tokens with
// it, offline, and holds no key that signs (D-OPS-1).
//
// The document is RFC 7517's jwk set, ByJwtPublicJwksResourceName, written from
// the issuer's keys by ExportByJwtPublicJwks: each key public, its kid the kid
// by jwts carry (jwtKid, the base64url SHA-256 of its PKIX encoding), its alg
// the one the issuer signs with it. A private key, a key whose kid is not its
// own, an alg that does not match the key, or a key no by jwt is signed with
// refuses the whole document, so a mistaken mount verifies nothing rather than
// the wrong tokens.
//
// It rereads as byJwtKeyring does: the document is read at first use, and read
// again when a token names a kid the keyring does not hold, or names none and no
// held key verifies it, at most once per reread interval, so a key version the
// vault gains verifies here once its export reaches the mount, without a
// restart. A reread that fails keeps the held keys, and a key the document drops
// keeps verifying until restart. Unlike the issuer's keyring, a process that
// cannot read the document is not broken: it holds no key, which a verifier
// answers as unavailable, and it tries again at most once per reread interval.
//
// Safe for concurrent use.
package session

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// The jwk set a block mounts in its secret_files, beside redis.yml.
const ByJwtPublicJwksResourceName = "jwt-jwks.json"

// The use every exported key has: signatures.
const byJwtJwkUseSignature = "sig"

// A keyring that holds no key: nothing it could verify is known.
var ErrByJwtVerifyKeysUnavailable = errors.New("by jwt verify keys unavailable")

// What the verify-only keyring reads and how often.
type ByJwtVerifyKeyringSettings struct {
	// Reads the jwk set: the block's mounted ByJwtPublicJwksResourceName.
	Document func() ([]byte, error)
	// Spaces reads, as the issuer's keyring spaces its vault reads: a token
	// naming a kid the keyring does not hold, and a document that could not be
	// read, cause at most one read each interval. 10 s, the issuer keyring's.
	RereadInterval time.Duration
	// The smallest rsa key a document may hold: 2,048 bits, the least the issuer
	// signs with (signWithKeyring).
	RsaMinBitCount int
	Now            func() time.Time
}

// The block's mounted document, read each ten seconds at most.
func DefaultByJwtVerifyKeyringSettings() *ByJwtVerifyKeyringSettings {
	return &ByJwtVerifyKeyringSettings{
		Document: func() ([]byte, error) {
			resource, err := server.Vault.SimpleResource(ByJwtPublicJwksResourceName)
			if err != nil {
				return nil, err
			}
			return resource.BytesE()
		},
		RereadInterval: 10 * time.Second,
		RsaMinBitCount: 2048,
		Now:            server.NowUtc,
	}
}

// Verifies by jwt signatures with public keys alone. NewByJwtVerifyKeyring makes
// one.
type ByJwtVerifyKeyring struct {
	settings *ByJwtVerifyKeyringSettings

	// serializes document reads. Readers of the held keys never take it.
	readLock sync.Mutex
	// the last reread, zero before the first, as the issuer's keyring keeps it:
	// the first read at first use sets none, so the first reread is admitted at
	// once. Guarded by readLock.
	rereadTime time.Time
	// the last first read that failed, zero while none has. Guarded by readLock.
	failedReadTime time.Time

	stateLock sync.Mutex
	// nil until a read succeeds
	keys *byJwtKeys
	// why the last read failed, while keys is nil
	readErr error
}

// A keyring that reads the document at first use.
func NewByJwtVerifyKeyring(settings *ByJwtVerifyKeyringSettings) *ByJwtVerifyKeyring {
	if settings == nil {
		settings = DefaultByJwtVerifyKeyringSettings()
	}
	return &ByJwtVerifyKeyring{settings: settings}
}

func (self *ByJwtVerifyKeyring) loadedKeys() (*byJwtKeys, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.keys, self.readErr
}

func (self *ByJwtVerifyKeyring) setKeys(keys *byJwtKeys, readErr error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if keys != nil {
		self.keys = keys
	}
	self.readErr = readErr
}

// The held keys, read at first use. With none held, the document is read again
// at most once per reread interval, and until a read succeeds the answer wraps
// ErrByJwtVerifyKeysUnavailable with why the last read failed.
func (self *ByJwtVerifyKeyring) heldKeys() (*byJwtKeys, error) {
	if keys, _ := self.loadedKeys(); keys != nil {
		return keys, nil
	}
	self.readLock.Lock()
	defer self.readLock.Unlock()
	keys, readErr := self.loadedKeys()
	if keys != nil {
		return keys, nil
	}
	now := self.settings.Now()
	if self.failedReadTime.IsZero() || !now.Before(self.failedReadTime.Add(self.settings.RereadInterval)) {
		keys, readErr = readByJwtVerifyKeys(self.settings, nil)
		self.setKeys(keys, readErr)
		if keys != nil {
			self.failedReadTime = time.Time{}
			return keys, nil
		}
		self.failedReadTime = now
		glog.Errorf("[jwt]no by jwt verify keys: %s\n", readErr)
	}
	if readErr == nil {
		readErr = errors.New("not read yet")
	}
	return nil, fmt.Errorf("%w: %w", ErrByJwtVerifyKeysUnavailable, readErr)
}

// Reads the document again unless the last read was less than the reread
// interval ago, and answers the held keys when they now hold a kid seenKeys
// lacks, else nil. Reads are serialized, so a caller refused by the interval
// still sees the keys an earlier or concurrent read gained. A read that fails
// keeps the held keys.
func (self *ByJwtVerifyKeyring) reread(seenKeys *byJwtKeys) *byJwtKeys {
	self.readLock.Lock()
	defer self.readLock.Unlock()

	keys, _ := self.loadedKeys()
	now := self.settings.Now()
	if self.rereadTime.IsZero() || !now.Before(self.rereadTime.Add(self.settings.RereadInterval)) {
		self.rereadTime = now
		readKeys, err := readByJwtVerifyKeys(self.settings, keys)
		if err != nil {
			glog.Infof("[jwt]verify keys reread kept the held keys. err = %s\n", err)
		} else {
			if keys != nil && len(keys.kidPublicKeys) < len(readKeys.kidPublicKeys) {
				glog.Infof("[jwt]verify keys reread gained %d key(s)\n", len(readKeys.kidPublicKeys)-len(keys.kidPublicKeys))
			}
			self.setKeys(readKeys, nil)
			keys = readKeys
		}
	}
	if keys == nil || seenKeys == nil {
		return keys
	}
	for kid := range keys.kidPublicKeys {
		if _, ok := seenKeys.kidPublicKeys[kid]; !ok {
			return keys
		}
	}
	return nil
}

// The signature of jwtSigned, verified as parseByJwtSigned verifies it, without
// validating claims. A keyring that holds no key answers
// ErrByJwtVerifyKeysUnavailable. unknownKid reports a token that named a kid
// this keyring does not hold even after a reread: a key it was not given yet,
// rather than a forgery under one it holds.
func (self *ByJwtVerifyKeyring) parse(jwtSigned string) (byJwt *ByJwt, unknownKid bool, err error) {
	keys, err := self.heldKeys()
	if err != nil {
		return nil, false, err
	}
	return parseByJwtSignedByKeys(keys, self.reread, jwtSigned)
}

// The keys a document holds, each held key previous holds kept beside them, as
// the issuer's keyring keeps a key its vault no longer lists.
func readByJwtVerifyKeys(settings *ByJwtVerifyKeyringSettings, previous *byJwtKeys) (*byJwtKeys, error) {
	document, err := settings.Document()
	if err != nil {
		return nil, err
	}
	kidPublicKeys, err := parseByJwtPublicJwks(document, settings.RsaMinBitCount)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ByJwtPublicJwksResourceName, err)
	}
	keys := &byJwtKeys{kidPublicKeys: kidPublicKeys}
	if previous != nil {
		for kid, publicKey := range previous.kidPublicKeys {
			if _, ok := keys.kidPublicKeys[kid]; !ok {
				keys.kidPublicKeys[kid] = publicKey
			}
		}
	}
	for _, publicKey := range keys.kidPublicKeys {
		keys.verificationKeys = append(keys.verificationKeys, publicKey)
	}
	return keys, nil
}

// The signing alg the issuer uses with publicKey (signWithKeyring), or an error
// for a key no by jwt is signed with.
func byJwtKeyAlg(publicKey crypto.PublicKey, rsaMinBitCount int) (string, error) {
	switch key := publicKey.(type) {
	case *ecdsa.PublicKey:
		switch key.Curve {
		case elliptic.P256():
			return gojwt.SigningMethodES256.Alg(), nil
		case elliptic.P384():
			return gojwt.SigningMethodES384.Alg(), nil
		case elliptic.P521():
			return gojwt.SigningMethodES512.Alg(), nil
		}
		return "", errors.New("an ecdsa key on another curve")
	case *rsa.PublicKey:
		if key.N.BitLen() < rsaMinBitCount {
			return "", fmt.Errorf("an rsa key of %d bits", key.N.BitLen())
		}
		return gojwt.SigningMethodRS512.Alg(), nil
	}
	return "", fmt.Errorf("a %T key", publicKey)
}

// A document's keys by kid. Any key that is not public, not a by jwt key, not
// its kid's, or not its alg's refuses the whole document.
func parseByJwtPublicJwks(document []byte, rsaMinBitCount int) (map[string]crypto.PublicKey, error) {
	set := &jose.JSONWebKeySet{}
	if err := json.Unmarshal(document, set); err != nil {
		return nil, err
	}
	kidPublicKeys := map[string]crypto.PublicKey{}
	for _, jwk := range set.Keys {
		if !jwk.IsPublic() || !jwk.Valid() {
			return nil, fmt.Errorf("jwk %q is not a public key", jwk.KeyID)
		}
		alg, err := byJwtKeyAlg(jwk.Key, rsaMinBitCount)
		if err != nil {
			return nil, fmt.Errorf("jwk %q: %w", jwk.KeyID, err)
		}
		if jwk.Algorithm != alg {
			return nil, fmt.Errorf("jwk %q is %q, not its key's %s", jwk.KeyID, jwk.Algorithm, alg)
		}
		kid, err := jwtKid(jwk.Key)
		if err != nil {
			return nil, err
		}
		if jwk.KeyID != kid {
			return nil, fmt.Errorf("jwk %q is not its key's kid", jwk.KeyID)
		}
		kidPublicKeys[kid] = jwk.Key
	}
	if len(kidPublicKeys) == 0 {
		return nil, errors.New("the jwk set holds no key")
	}
	return kidPublicKeys, nil
}

// The jwk set of every key the vault's jwt.yml lists, public parts only, in the
// vault's order, for ByJwtPublicJwksResourceName: what an operations command
// writes for a block's secret_files each time jwt.yml gains a key version, which
// must reach the blocks before the version signs (byJwtKeySigningDelay). It
// reads the vault as the issuer's keyring does, so it runs where jwt.yml is
// mounted.
func ExportByJwtPublicJwks() ([]byte, error) {
	keys, err := readByJwtKeys(defaultByJwtKeyringSettings(), nil)
	if err != nil {
		return nil, err
	}
	return exportByJwtPublicJwks(keys)
}

// keys' public parts as a jwk set, each key once, in keys' order.
func exportByJwtPublicJwks(keys *byJwtKeys) ([]byte, error) {
	set := &jose.JSONWebKeySet{}
	exported := map[string]bool{}
	for _, key := range keys.keys {
		if key.kid == "" || exported[key.kid] {
			continue
		}
		alg, err := byJwtKeyAlg(key.publicKey, 0)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key.path, err)
		}
		set.Keys = append(set.Keys, jose.JSONWebKey{
			Key:       key.publicKey,
			KeyID:     key.kid,
			Algorithm: alg,
			Use:       byJwtJwkUseSignature,
		})
		exported[key.kid] = true
	}
	if len(set.Keys) == 0 {
		return nil, errors.New("no jwt key to export")
	}
	return json.Marshal(set)
}

// The jwk set of the process keyring's keys, for a test that verifies tokens
// this process signs.
func Testing_ByJwtPublicJwks() ([]byte, error) {
	return exportByJwtPublicJwks(defaultByJwtKeyring.heldKeys())
}
