// The by jwt keyring: the private keys under the vault jwt.yml `tls_key_paths`,
// as this process has read them.
//
// Verification accepts every key the keyring holds. The vault is read at first
// use, and read again when a token names a kid the keyring does not hold, or
// names none and no held key verifies it, at most once per reread interval. A
// key version the vault gains while the process runs (a certificate rotation
// adds a version under a tls key path) therefore verifies without a restart.
// Before rereads, a process verified with only the keys it read at first use:
// on 2026-10-12 a certificate rotation reached the vault, the api restarted and
// signed with the new version, and every connect process started before the
// rotation refused those tokens until it was restarted. A token whose kid the
// keyring holds causes no reread: a kid names the exact key, so a signature
// that fails it is a forgery, not a rotation.
//
// Keys are only added while the process runs. A key the vault no longer lists
// keeps verifying until restart and no longer signs.
//
// Signing uses the newest key version that has settled: its key file was
// modified at least the signing delay ago. A new version directory keeps the
// modification time it had in the vault checkout through the vault push, so
// every host dates the version alike, by when it was written to the vault
// (a push that rewrites a file in place dates it by the push). The delay lets
// the push reach every host, whose verifiers then reread, before any host signs
// with the version. When no held key has settled, the newest signs, as it
// always did. Ecdsa keys sign in preference to rsa keys.
//
// Safe for concurrent use.
package session

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// spaces rereads, bounding the vault reads that tokens naming unknown kids
// can cause
const byJwtKeyRereadInterval = 10 * time.Second

// a new key version signs only this long after it was written to the vault,
// long enough for a vault push to reach every host
const byJwtKeySigningDelay = 2 * time.Hour

type byJwtKeyringSettings struct {
	// the vault key paths (jwt.yml `tls_key_paths`)
	keyPaths func() ([]string, error)
	// the vault files of a key path, newest version first
	resourcePaths  func(keyPath string) ([]string, error)
	now            func() time.Time
	rereadInterval time.Duration
	signingDelay   time.Duration
}

func defaultByJwtKeyringSettings() *byJwtKeyringSettings {
	return &byJwtKeyringSettings{
		keyPaths: func() ([]string, error) {
			resource, err := server.Vault.SimpleResource("jwt.yml")
			if err != nil {
				return nil, err
			}
			keyPaths := resource.StringList("tls_key_paths")
			if len(keyPaths) == 0 {
				return nil, errors.New("jwt.yml lists no tls_key_paths")
			}
			return keyPaths, nil
		},
		resourcePaths:  server.Vault.ResourcePaths,
		now:            time.Now,
		rereadInterval: byJwtKeyRereadInterval,
		signingDelay:   byJwtKeySigningDelay,
	}
}

// the process keyring: verifies and signs every by jwt
var defaultByJwtKeyring = newByJwtKeyring(defaultByJwtKeyringSettings())

type byJwtKeyring struct {
	settings *byJwtKeyringSettings

	// serializes vault reads. Readers of the held keys never take it.
	readLock sync.Mutex
	// the last reread, zero before the first. Guarded by readLock.
	rereadTime time.Time

	stateLock sync.Mutex
	// nil until the first read
	keys *byJwtKeys
}

func newByJwtKeyring(settings *byJwtKeyringSettings) *byJwtKeyring {
	return &byJwtKeyring{
		settings: settings,
	}
}

// The held keys, read from the vault at first use. A process that finds no key
// can neither sign nor verify, so the first read panics without one.
func (self *byJwtKeyring) heldKeys() *byJwtKeys {
	if keys := self.loadedKeys(); keys != nil {
		return keys
	}
	self.readLock.Lock()
	defer self.readLock.Unlock()
	if keys := self.loadedKeys(); keys != nil {
		return keys
	}
	keys, err := readByJwtKeys(self.settings, nil)
	if err != nil {
		panic(err)
	}
	self.setKeys(keys)
	return keys
}

func (self *byJwtKeyring) loadedKeys() *byJwtKeys {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.keys
}

func (self *byJwtKeyring) setKeys(keys *byJwtKeys) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.keys = keys
}

// Reads the vault again unless the last reread was less than the reread
// interval ago, and reports whether the keyring now holds a kid that seenKeys,
// the caller's view, lacks. Rereads are serialized, so a caller refused by the
// interval still sees the keys an earlier or concurrent reread gained. A reread
// that cannot resolve a key path, or finds no key, keeps the held keys.
func (self *byJwtKeyring) reread(seenKeys *byJwtKeys) bool {
	self.readLock.Lock()
	defer self.readLock.Unlock()

	keys := self.loadedKeys()
	now := self.settings.now()
	if self.rereadTime.IsZero() || !now.Before(self.rereadTime.Add(self.settings.rereadInterval)) {
		self.rereadTime = now
		readKeys, err := readByJwtKeys(self.settings, keys)
		if err != nil {
			glog.Infof("[jwt]reread kept the held keys. err = %s\n", err)
		} else {
			gainedCount := 0
			for kid := range readKeys.kidPublicKeys {
				if _, ok := keys.kidPublicKeys[kid]; !ok {
					gainedCount += 1
				}
			}
			if 0 < gainedCount {
				glog.Infof("[jwt]reread gained %d key(s)\n", gainedCount)
			}
			self.setKeys(readKeys)
			keys = readKeys
		}
	}
	for kid := range keys.kidPublicKeys {
		if _, ok := seenKeys.kidPublicKeys[kid]; !ok {
			return true
		}
	}
	return false
}

// The held key that signs now (see byJwtKeys.signingKey).
func (self *byJwtKeyring) signingKey() *byJwtKey {
	key := self.heldKeys().signingKey(self.settings.now(), self.settings.signingDelay)
	if key == nil {
		panic(fmt.Errorf("No signing key found"))
	}
	return key
}

// One key file as read.
type byJwtKey struct {
	path       string
	modifyTime time.Time
	privateKey crypto.PrivateKey
	publicKey  crypto.PublicKey
	// empty when the public key cannot be encoded
	kid string
	// the vault no longer lists it: it verifies and does not sign
	retired bool
}

// An immutable view of the keyring.
type byJwtKeys struct {
	// the listed keys in vault order (jwt.yml path order, each path's versions
	// newest first), then the retired keys
	keys             []*byJwtKey
	kidPublicKeys    map[string]crypto.PublicKey
	verificationKeys []gojwt.VerificationKey
	// the listed key of each file, and the modification time of each file that
	// did not parse, so a reread reads only new and rewritten files
	pathKeys              map[string]*byJwtKey
	pathFailedModifyTimes map[string]time.Time
}

// The key that signs at now: the newest settled ecdsa key, else the newest
// settled rsa key; with none settled, the newest ecdsa key, else the newest rsa
// key. Retired keys never sign. Nil when no listed key can sign.
func (self *byJwtKeys) signingKey(now time.Time, signingDelay time.Duration) *byJwtKey {
	settleTime := now.Add(-signingDelay)
	var settledEcdsaKey, settledRsaKey, newestEcdsaKey, newestRsaKey *byJwtKey
	for _, key := range self.keys {
		if key.retired {
			continue
		}
		settled := !key.modifyTime.After(settleTime)
		switch key.privateKey.(type) {
		case *ecdsa.PrivateKey:
			if newestEcdsaKey == nil {
				newestEcdsaKey = key
			}
			if settled && settledEcdsaKey == nil {
				settledEcdsaKey = key
			}
		case *rsa.PrivateKey:
			if newestRsaKey == nil {
				newestRsaKey = key
			}
			if settled && settledRsaKey == nil {
				settledRsaKey = key
			}
		}
	}
	for _, key := range []*byJwtKey{settledEcdsaKey, settledRsaKey, newestEcdsaKey, newestRsaKey} {
		if key != nil {
			return key
		}
	}
	return nil
}

// The private keys in order, the signing key first. With no signing key, in
// order.
func (self *byJwtKeys) privateKeysSigningFirst(signingKey *byJwtKey) []crypto.PrivateKey {
	privateKeys := []crypto.PrivateKey{}
	if signingKey != nil {
		privateKeys = append(privateKeys, signingKey.privateKey)
	}
	for _, key := range self.keys {
		if key != signingKey {
			privateKeys = append(privateKeys, key.privateKey)
		}
	}
	return privateKeys
}

// Reads the keys the vault lists, reusing every file that previous holds and
// that was not modified since. Every listed file is read, and one that cannot
// be read or parsed is skipped. The first read (previous nil) skips a key
// path that cannot be resolved; a reread fails instead, so a path that failed
// to resolve does not retire its keys. Fails when no key is listed.
func readByJwtKeys(settings *byJwtKeyringSettings, previous *byJwtKeys) (*byJwtKeys, error) {
	keyPaths, err := settings.keyPaths()
	if err != nil {
		return nil, err
	}
	if previous == nil {
		glog.Infof("[jwt]paths: %s", keyPaths)
	}

	keys := &byJwtKeys{
		kidPublicKeys:         map[string]crypto.PublicKey{},
		pathKeys:              map[string]*byJwtKey{},
		pathFailedModifyTimes: map[string]time.Time{},
	}
	listedKeys := map[*byJwtKey]bool{}
	errs := []error{}
	for _, keyPath := range keyPaths {
		paths, err := settings.resourcePaths(keyPath)
		if err != nil {
			if previous != nil {
				return nil, err
			}
			errs = append(errs, err)
			continue
		}
		for _, path := range paths {
			info, err := os.Stat(path)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			modifyTime := info.ModTime()
			if previous != nil {
				if key, ok := previous.pathKeys[path]; ok && key.modifyTime.Equal(modifyTime) {
					keys.keys = append(keys.keys, key)
					keys.pathKeys[path] = key
					listedKeys[key] = true
					continue
				}
				if failedModifyTime, ok := previous.pathFailedModifyTimes[path]; ok && failedModifyTime.Equal(modifyTime) {
					keys.pathFailedModifyTimes[path] = failedModifyTime
					continue
				}
			}
			// a file that cannot be read is retried by the next read
			bytes, err := os.ReadFile(path)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			privateKey, keyType, err := parseByJwtPrivateKey(bytes)
			if err != nil {
				glog.Errorf("[jwt]could not load key \"%s\". err = %s\n", path, err)
				keys.pathFailedModifyTimes[path] = modifyTime
				errs = append(errs, err)
				continue
			}
			logLoadedPrivateKey(keyType, path)
			key := &byJwtKey{
				path:       path,
				modifyTime: modifyTime,
				privateKey: privateKey,
				publicKey:  publicKey(privateKey),
			}
			if kid, err := jwtKid(key.publicKey); err == nil {
				key.kid = kid
			} else {
				glog.Errorf("[jwt]could not compute kid: %s\n", err)
			}
			keys.keys = append(keys.keys, key)
			keys.pathKeys[path] = key
			listedKeys[key] = true
		}
	}
	if len(keys.keys) == 0 {
		errs = append(errs, errors.New("no jwt key found"))
		return nil, errors.Join(errs...)
	}

	for _, key := range keys.keys {
		if key.kid != "" {
			if _, ok := keys.kidPublicKeys[key.kid]; !ok {
				keys.kidPublicKeys[key.kid] = key.publicKey
			}
		}
	}
	if previous != nil {
		// a held key the vault no longer lists retires, unless a listed file
		// holds the same key
		for _, key := range previous.keys {
			if listedKeys[key] {
				continue
			}
			if _, ok := keys.kidPublicKeys[key.kid]; ok && key.kid != "" {
				continue
			}
			if !key.retired {
				retiredKey := *key
				retiredKey.retired = true
				key = &retiredKey
			}
			keys.keys = append(keys.keys, key)
			if key.kid != "" {
				keys.kidPublicKeys[key.kid] = key.publicKey
			}
		}
	}
	for _, key := range keys.keys {
		keys.verificationKeys = append(keys.verificationKeys, key.publicKey)
	}
	return keys, nil
}

// A pem private key in sec 1 (ec), pkcs 8 or pkcs 1 form, and the form's name.
func parseByJwtPrivateKey(bytes []byte) (crypto.PrivateKey, string, error) {
	block, _ := pem.Decode(bytes)
	if block == nil {
		return nil, "", errors.New("no pem block")
	}
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return key, "ec", nil
	}
	keyErrs := []error{}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		return key, "pkcs8", nil
	} else {
		keyErrs = append(keyErrs, err)
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, "pkcs1", nil
	} else {
		keyErrs = append(keyErrs, err)
	}
	return nil, "", errors.Join(keyErrs...)
}
