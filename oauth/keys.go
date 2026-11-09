package oauth

// Signer key loading and the published jwks.
//
// Keys are listed newest first in vault auth.yml. The first of a set signs that
// set's new tokens; every listed key of the set verifies, so tokens signed before
// a rotation keep working until they expire. Because access tokens live an hour,
// a rotated key can be dropped from the list the day after it is replaced.
//
// §7.8 keeps four disjoint ES256 sets, named by the kid's prefix: today's mcp
// access and id tokens (no prefix), admin tokens (adm-), service tokens and the
// rev/ objects (svc-), and edge login's id tokens (id-). A key belongs to the
// set its kid names, and a token verifies only under the set its type expects,
// so a key of one set verifies nothing another set signs.
//
// The kid is the set's prefix and the rfc 7638 thumbprint of the public key, so
// it is derived from the key rather than assigned. `warpctl oauth keygen` writes
// the key file under that name and `warpctl oauth promote` inserts the matching
// auth.yml entry, which keeps the three in agreement; `--set` names the set.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/urnetwork/server"
)

const signerAlgEs256 = "ES256"

// One of §7.8's disjoint key sets, named by its kids' prefix.
type SignerKeySet string

const (
	// today's mcp access tokens and their openid connect id tokens: kids with no
	// set prefix
	SignerKeySetMcp SignerKeySet = ""
	// admin tokens, aud api, for ur admin and the console (§7.8)
	SignerKeySetAdmin SignerKeySet = "adm"
	// service tokens, aud evs, gkm, dns, certs or mail, and the rev/ objects
	SignerKeySetService SignerKeySet = "svc"
	// edge login's id tokens, the members audience (§7.8's edge login)
	SignerKeySetId SignerKeySet = "id"
)

// The sets with a prefix, which `warpctl oauth --set` names.
var prefixedSignerKeySets = []SignerKeySet{SignerKeySetAdmin, SignerKeySetService, SignerKeySetId}

// The prefix a kid of this set starts with, "" for the mcp set.
func (self SignerKeySet) KidPrefix() string {
	if self == SignerKeySetMcp {
		return ""
	}
	return string(self) + "-"
}

// The set a kid names: the one whose prefix it starts with, else the mcp set.
func SignerKeySetOfKid(kid string) SignerKeySet {
	for _, set := range prefixedSignerKeySets {
		if strings.HasPrefix(kid, set.KidPrefix()) {
			return set
		}
	}
	return SignerKeySetMcp
}

type SignerKey struct {
	Kid        string
	Alg        string
	Set        SignerKeySet
	PrivateKey *ecdsa.PrivateKey
}

// The keys of every set, newest first. Tests install their own
// (Testing_SetSignerKeys).
var signerKeysOverride atomic.Pointer[[]*SignerKey]

// Newest first, every set. The first of a set signs that set.
func signerKeys() []*SignerKey {
	if keys := signerKeysOverride.Load(); keys != nil {
		return *keys
	}
	return configSignerKeys()
}

// The keys auth.yml lists, read once.
var configSignerKeys = sync.OnceValue(func() []*SignerKey {
	keys := []*SignerKey{}

	for _, keyConfig := range vaultConfig().SignerKeys {
		if keyConfig.Alg != signerAlgEs256 {
			panic(fmt.Errorf("oauth signer key %s: unsupported alg %s", keyConfig.Kid, keyConfig.Alg))
		}

		path, err := server.Vault.ResourcePath(keyConfig.Path)
		if err != nil {
			panic(fmt.Errorf("oauth signer key %s: %w", keyConfig.Kid, err))
		}

		keyPem, err := os.ReadFile(path)
		if err != nil {
			panic(fmt.Errorf("oauth signer key %s: %w", keyConfig.Kid, err))
		}

		block, _ := pem.Decode(keyPem)
		if block == nil {
			panic(fmt.Errorf("oauth signer key %s: not pem", keyConfig.Kid))
		}

		var privateKey *ecdsa.PrivateKey
		if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
			ecKey, ok := key.(*ecdsa.PrivateKey)
			if !ok {
				panic(fmt.Errorf("oauth signer key %s: not an ec key", keyConfig.Kid))
			}
			privateKey = ecKey
		} else if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
			privateKey = key
		} else {
			panic(fmt.Errorf("oauth signer key %s: could not parse", keyConfig.Kid))
		}

		// the kid must be the set's prefix and the thumbprint of this key. A
		// mismatch means the config and the file have drifted, which would
		// publish a jwks that cannot verify the tokens actually being signed
		set := SignerKeySetOfKid(keyConfig.Kid)
		thumbprint, err := SignerKid(&privateKey.PublicKey)
		if err != nil {
			panic(fmt.Errorf("oauth signer key %s: %w", keyConfig.Kid, err))
		}
		if kid := set.KidPrefix() + thumbprint; kid != keyConfig.Kid {
			panic(fmt.Errorf(
				"oauth signer key %s: the key at %s is %s",
				keyConfig.Kid,
				keyConfig.Path,
				kid,
			))
		}

		keys = append(keys, &SignerKey{
			Kid:        keyConfig.Kid,
			Alg:        keyConfig.Alg,
			Set:        set,
			PrivateKey: privateKey,
		})
	}

	return keys
})

// The keys of one set, newest first.
func signerKeysOf(set SignerKeySet) []*SignerKey {
	keys := []*SignerKey{}
	for _, key := range signerKeys() {
		if key.Set == set {
			keys = append(keys, key)
		}
	}
	return keys
}

// The key new mcp access and id tokens are signed with.
func SigningKey() *SignerKey {
	return signerKeysOf(SignerKeySetMcp)[0]
}

// The key new tokens of the set are signed with, or an error when auth.yml
// lists none, so nothing of that set is minted.
func SigningKeyOf(set SignerKeySet) (*SignerKey, error) {
	keys := signerKeysOf(set)
	if len(keys) == 0 {
		return nil, fmt.Errorf("auth.yml lists no %q signer key; run `warpctl oauth keygen --set=%s`", set, set)
	}
	return keys[0], nil
}

// Every key an mcp access or id token may have been signed with, for
// verification and the jwks.
func VerificationKeys() []*SignerKey {
	return signerKeysOf(SignerKeySetMcp)
}

// The mcp set's key of kid, or nil: a kid of another set is none of its.
func VerificationKey(kid string) *SignerKey {
	return verificationKeyOf(SignerKeySetMcp, kid)
}

// The set's key of kid, or nil.
func verificationKeyOf(set SignerKeySet, kid string) *SignerKey {
	if SignerKeySetOfKid(kid) != set {
		return nil
	}
	for _, key := range signerKeysOf(set) {
		if key.Kid == kid {
			return key
		}
	}
	return nil
}

// Installs keys, newest first, in place of auth.yml's until the returned pop.
func Testing_SetSignerKeys(keys ...*SignerKey) func() {
	previous := signerKeysOverride.Load()
	signerKeysOverride.Store(&keys)
	return func() {
		signerKeysOverride.Store(previous)
	}
}

type Jwk struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

type JwkSet struct {
	Keys []*Jwk `json:"keys"`
}

// The public half of every mcp signer key, for the jwks endpoint.
func Jwks() *JwkSet {
	return PublicJwks(SignerKeySetMcp)
}

// The public half of every key of the set, newest first: what a verifier of
// that set's tokens holds.
func PublicJwks(set SignerKeySet) *JwkSet {
	jwks := &JwkSet{Keys: []*Jwk{}}

	for _, key := range signerKeysOf(set) {
		jwks.Keys = append(jwks.Keys, NewSignerJwk(key.Kid, key.Alg, &key.PrivateKey.PublicKey))
	}

	return jwks
}

// The public jwk of a signer key.
func NewSignerJwk(kid string, alg string, publicKey *ecdsa.PublicKey) *Jwk {
	x, y := ecPublicCoordinates(publicKey)
	return &Jwk{
		Kty: "EC",
		Crv: publicKey.Curve.Params().Name,
		Kid: kid,
		Alg: alg,
		Use: "sig",
		X:   x,
		Y:   y,
	}
}

// A jwk's public key: P-256 only, its point on the curve.
func (self *Jwk) PublicKey() (*ecdsa.PublicKey, error) {
	if self.Kty != "EC" || self.Crv != "P-256" {
		return nil, fmt.Errorf("jwk %s is not a P-256 key", self.Kid)
	}
	x, err := base64.RawURLEncoding.DecodeString(self.X)
	if err != nil || len(x) != 32 {
		return nil, fmt.Errorf("jwk %s has no x", self.Kid)
	}
	y, err := base64.RawURLEncoding.DecodeString(self.Y)
	if err != nil || len(y) != 32 {
		return nil, fmt.Errorf("jwk %s has no y", self.Kid)
	}
	// the uncompressed point, which ParseUncompressedPublicKey checks is on the
	// curve
	point := append(append([]byte{4}, x...), y...)
	publicKey, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), point)
	if err != nil {
		return nil, fmt.Errorf("jwk %s is not a point on P-256", self.Kid)
	}
	return publicKey, nil
}

// The rfc 7638 jwk thumbprint: base64url(sha256) over canonical json with
// lexicographically ordered members and no whitespace. Mirrors the derivation
// in `warpctl oauth keygen`, which is what names the key file.
func SignerKid(publicKey *ecdsa.PublicKey) (string, error) {
	if publicKey.Curve.Params().Name != "P-256" {
		return "", fmt.Errorf("unsupported curve %s", publicKey.Curve.Params().Name)
	}

	x, y := ecPublicCoordinates(publicKey)

	// the member order here is normative, not stylistic
	canonical := fmt.Sprintf(`{"crv":"P-256","kty":"EC","x":"%s","y":"%s"}`, x, y)

	return base64RawUrlSha256(canonical), nil
}

// Fixed width base64url coordinates, as jwk requires: the leading zero bytes
// are significant, so the big.Int bytes must be left padded to the curve size.
func ecPublicCoordinates(publicKey *ecdsa.PublicKey) (string, string) {
	byteLen := (publicKey.Curve.Params().BitSize + 7) / 8

	pad := func(i *big.Int) string {
		b := i.Bytes()
		if len(b) < byteLen {
			padded := make([]byte, byteLen)
			copy(padded[byteLen-len(b):], b)
			b = padded
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}

	return pad(publicKey.X), pad(publicKey.Y)
}
