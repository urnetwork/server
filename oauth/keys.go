package oauth

// Signer key loading and the published jwks.
//
// Keys are listed newest first in vault auth.yml. The first signs new tokens;
// every listed key verifies, so tokens signed before a rotation keep working
// until they expire. Because access tokens live an hour, a rotated key can be
// dropped from the list the day after it is replaced.
//
// The kid is the rfc 7638 thumbprint of the public key, so it is derived from
// the key rather than assigned. `warpctl oauth keygen` writes the key file
// under that name and `warpctl oauth promote` inserts the matching auth.yml
// entry, which keeps the three in agreement.

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"sync"

	"github.com/urnetwork/server"
)

const signerAlgEs256 = "ES256"

type SignerKey struct {
	Kid        string
	Alg        string
	PrivateKey *ecdsa.PrivateKey
}

// Newest first. The first entry signs.
var signerKeys = sync.OnceValue(func() []*SignerKey {
	keys := []*SignerKey{}

	for _, keyConfig := range Config().SignerKeys {
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

		// the kid must be the thumbprint of this key. A mismatch means the
		// config and the file have drifted, which would publish a jwks that
		// cannot verify the tokens actually being signed
		kid, err := SignerKid(&privateKey.PublicKey)
		if err != nil {
			panic(fmt.Errorf("oauth signer key %s: %w", keyConfig.Kid, err))
		}
		if kid != keyConfig.Kid {
			panic(fmt.Errorf(
				"oauth signer key %s: the key at %s has thumbprint %s",
				keyConfig.Kid,
				keyConfig.Path,
				kid,
			))
		}

		keys = append(keys, &SignerKey{
			Kid:        keyConfig.Kid,
			Alg:        keyConfig.Alg,
			PrivateKey: privateKey,
		})
	}

	return keys
})

// The key new tokens are signed with.
func SigningKey() *SignerKey {
	return signerKeys()[0]
}

// Every key a token may have been signed with, for verification and the jwks.
func VerificationKeys() []*SignerKey {
	return signerKeys()
}

func VerificationKey(kid string) *SignerKey {
	for _, key := range signerKeys() {
		if key.Kid == kid {
			return key
		}
	}
	return nil
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

// The public half of every signer key, for the jwks endpoint.
func Jwks() *JwkSet {
	jwks := &JwkSet{Keys: []*Jwk{}}

	for _, key := range signerKeys() {
		x, y := ecPublicCoordinates(&key.PrivateKey.PublicKey)
		jwks.Keys = append(jwks.Keys, &Jwk{
			Kty: "EC",
			Crv: key.PrivateKey.Curve.Params().Name,
			Kid: key.Kid,
			Alg: key.Alg,
			Use: "sig",
			X:   x,
			Y:   y,
		})
	}

	return jwks
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
