// The SN25 validator rate class token. The api mints it for a client whose
// validator activation the reserved upload admission admits now (POST
// /sn/validator-class), and the lb front charges a request that carries it,
// beside the same client's verified bearer, to the validator rate class.
//
// The token is base64url(payload) "." base64url(HMAC-SHA256), keyed by
// DerivedKeys(validatorClassKeyPurpose), so only a holder of the jwt keys can
// mint one. It names its client, lives at most ValidatorClassMaximumLifetime,
// and never past the admission's own expiry, so a validator that loses its
// permit loses its class within that window.
//
// The front checks a token cheaply before any HMAC: its size and shape, its
// audience, an expiry inside the lifetime, and the bearer's client id. Only a
// token that passes is verified, once: verified tokens are kept until they
// expire, and refused ones for a minute, both in bounded caches, so junk
// headers cost the front neither CPU nor memory.
package session

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/server/v2026"
)

const (
	// the header that carries a class token on a validator's api requests;
	// sn's protocol.ValidatorClassHeader
	ValidatorClassHeader   = "X-UR-Validator-Class"
	ValidatorClassAudience = "urnetwork-validator-class"
	// a class token lives at most this long, and never past the admission's
	// own expiry
	ValidatorClassMaximumLifetime = 5 * time.Minute

	validatorClassKeyPurpose        = "urnetwork:sn25-validator-class:v1"
	maximumValidatorClassTokenBytes = 1024
	validatorClassClockLeeway       = 30 * time.Second
	// how long a refused token is remembered
	validatorClassRefusedTimeout = time.Minute
	// how often the verifier derives its keys again
	validatorClassKeysTimeout = time.Minute
)

// The claims of a class token. Hotkey and Activation are lowercase hex.
type ValidatorClassClaims struct {
	Audience   string    `json:"aud"`
	ClientId   server.Id `json:"client_id"`
	NetworkId  server.Id `json:"network_id"`
	Hotkey     string    `json:"hotkey"`
	Activation string    `json:"activation"`
	IssuedAt   int64     `json:"iat"`
	ExpiresAt  int64     `json:"exp"`
}

// The token's expiry.
func (self ValidatorClassClaims) Expiry() time.Time {
	return time.Unix(self.ExpiresAt, 0)
}

// Mints a class token for claims with the deployment's current key. The
// caller sets the expiry within ValidatorClassMaximumLifetime.
func MintValidatorClass(claims ValidatorClassClaims) (string, error) {
	keys := DerivedKeys(validatorClassKeyPurpose)
	if len(keys) == 0 {
		return "", errors.New("no signing key")
	}
	return signValidatorClass(keys[0], claims)
}

func signValidatorClass(key []byte, claims ValidatorClassClaims) (string, error) {
	claims.Audience = ValidatorClassAudience
	if claims.ClientId == (server.Id{}) || claims.ExpiresAt <= claims.IssuedAt {
		return "", errors.New("validator class claims are incomplete")
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	body := base64.RawURLEncoding.EncodeToString(payload)
	token := body + "." + base64.RawURLEncoding.EncodeToString(validatorClassMac(key, body))
	if maximumValidatorClassTokenBytes < len(token) {
		return "", errors.New("validator class token is too large")
	}
	return token, nil
}

func validatorClassMac(key []byte, body string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(body))
	return mac.Sum(nil)
}

// The claims a token names, if it could be clientId's class now, read without
// verifying it: its size and shape, its audience, an expiry within the
// lifetime and the client.
func validatorClassCandidate(token string, clientId server.Id, now time.Time) (body string, mac []byte, claims ValidatorClassClaims, ok bool) {
	if clientId == (server.Id{}) || len(token) == 0 || maximumValidatorClassTokenBytes < len(token) {
		return
	}
	body, encodedMac, found := strings.Cut(token, ".")
	if !found || strings.Contains(encodedMac, ".") || len(encodedMac) != base64.RawURLEncoding.EncodedLen(sha256.Size) {
		return
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(body)
	if err != nil {
		return
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Audience != ValidatorClassAudience || claims.ClientId != clientId {
		return
	}
	expiry := claims.Expiry()
	if !now.Before(expiry) || now.Add(ValidatorClassMaximumLifetime+validatorClassClockLeeway).Before(expiry) {
		return
	}
	mac, err = base64.RawURLEncoding.Strict().DecodeString(encodedMac)
	if err != nil {
		return
	}
	return body, mac, claims, true
}

// The counts of a verifier's decisions, for its metrics and tests.
type ValidatorClassVerifierStats struct {
	// tokens that failed the cheap checks
	Refused uint64
	// tokens answered from the verified or refused caches
	Cached uint64
	// HMAC verifications made, and how many verified
	Verifications uint64
	Verified      uint64
}

// Verifies class tokens for the lb front. Safe for concurrent use.
type ValidatorClassVerifier struct {
	// the derived keys, signing key first; DerivedKeys, or a test's
	derive func() [][]byte

	lock     sync.Mutex
	keys     [][]byte
	keysTime time.Time
	verified *validatorClassCache
	refused  *validatorClassCache

	refusedCount      atomic.Uint64
	cachedCount       atomic.Uint64
	verificationCount atomic.Uint64
	verifiedCount     atomic.Uint64
}

// A verifier over the session keyring, holding at most verifiedCapacity
// verified and refusedCapacity refused tokens.
func NewValidatorClassVerifier(verifiedCapacity int, refusedCapacity int) *ValidatorClassVerifier {
	return newValidatorClassVerifier(func() [][]byte { return DerivedKeys(validatorClassKeyPurpose) }, verifiedCapacity, refusedCapacity)
}

func newValidatorClassVerifier(derive func() [][]byte, verifiedCapacity int, refusedCapacity int) *ValidatorClassVerifier {
	return &ValidatorClassVerifier{
		derive:   derive,
		verified: newValidatorClassCache(verifiedCapacity),
		refused:  newValidatorClassCache(refusedCapacity),
	}
}

// The claims of token if it verifies as clientId's class at now.
func (self *ValidatorClassVerifier) Verify(token string, clientId server.Id, now time.Time) (ValidatorClassClaims, bool) {
	body, mac, claims, ok := validatorClassCandidate(token, clientId, now)
	if !ok {
		self.refusedCount.Add(1)
		return ValidatorClassClaims{}, false
	}
	self.lock.Lock()
	defer self.lock.Unlock()
	if self.verified.contains(token, now) {
		self.cachedCount.Add(1)
		return claims, true
	}
	if self.refused.contains(token, now) {
		self.cachedCount.Add(1)
		return ValidatorClassClaims{}, false
	}
	if self.keys == nil || !now.Before(self.keysTime.Add(validatorClassKeysTimeout)) {
		self.keys, self.keysTime = self.derive(), now
	}
	self.verificationCount.Add(1)
	for _, key := range self.keys {
		if hmac.Equal(validatorClassMac(key, body), mac) {
			self.verifiedCount.Add(1)
			self.verified.put(token, claims.Expiry(), now)
			return claims, true
		}
	}
	refusedUntil := now.Add(validatorClassRefusedTimeout)
	if claims.Expiry().Before(refusedUntil) {
		refusedUntil = claims.Expiry()
	}
	self.refused.put(token, refusedUntil, now)
	return ValidatorClassClaims{}, false
}

// A copy of the verifier's counts.
func (self *ValidatorClassVerifier) Stats() ValidatorClassVerifierStats {
	return ValidatorClassVerifierStats{
		Refused:       self.refusedCount.Load(),
		Cached:        self.cachedCount.Load(),
		Verifications: self.verificationCount.Load(),
		Verified:      self.verifiedCount.Load(),
	}
}

// Tokens and their expiry, at most capacity of them: a full cache drops its
// oldest entry, and an expired entry is dropped when met. Guarded by the
// verifier's lock.
type validatorClassCache struct {
	capacity int
	expiries map[string]time.Time
	order    []string
}

func newValidatorClassCache(capacity int) *validatorClassCache {
	return &validatorClassCache{capacity: max(capacity, 1), expiries: map[string]time.Time{}}
}

func (self *validatorClassCache) contains(token string, now time.Time) bool {
	expiry, found := self.expiries[token]
	if !found {
		return false
	}
	if !now.Before(expiry) {
		delete(self.expiries, token)
		return false
	}
	return true
}

func (self *validatorClassCache) put(token string, expiry time.Time, now time.Time) {
	if _, found := self.expiries[token]; !found {
		for self.capacity <= len(self.expiries) && 0 < len(self.order) {
			oldest := self.order[0]
			self.order = self.order[1:]
			delete(self.expiries, oldest)
		}
		self.order = append(self.order, token)
		// keep the order from outgrowing the map once its entries expire
		if 2*self.capacity < len(self.order) {
			kept := self.order[:0]
			for _, ordered := range self.order {
				if expiry, found := self.expiries[ordered]; found && now.Before(expiry) {
					kept = append(kept, ordered)
				} else {
					delete(self.expiries, ordered)
				}
			}
			self.order = kept
		}
	}
	self.expiries[token] = expiry
}

// A verifier that knows key alone, for tests outside this package.
func Testing_NewValidatorClassVerifier(key []byte) *ValidatorClassVerifier {
	return newValidatorClassVerifier(func() [][]byte { return [][]byte{key} }, 16, 16)
}

// Signs claims with key, for tests outside this package.
func Testing_SignValidatorClass(key []byte, claims ValidatorClassClaims) (string, error) {
	return signValidatorClass(key, claims)
}
