package oauth

// The signature on §7.8's rev/ objects: a compact JWS (RFC 7515) under the svc-
// key set, the set evs and gkm already mount to verify service tokens. The
// payload is sidecar.EncodeRevocationPayload's bytes, signed as they are, and
// the header names the type sidecar.RevocationJwsType:
//
//	header   {"alg":"ES256","kid":"svc-...","typ":"ur-rev+jwt"}
//	payload  {"subject":"<network_id>/<kind>/<subject_id>","not_before":..,"reason":..,"time":..}
//
// The typ keeps a rev/ object from ever verifying as a service token, which
// carries a JWT's claims and no such type, and keeps a service token from
// reading as a rev/ object. api signs (model's rev/ writer); evs and gkm verify
// through sidecar.RevocationChecker, which takes VerifyRevocation as its
// function, so the engines never import this package.

import (
	"crypto/ecdsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/sidecar"
)

// An object that is no rev/ object of this issuer: its form, its header, its
// key or its signature.
var ErrInvalidRevocationObject = errors.New("invalid revocation object")

// The header a rev/ object carries, keys in RFC 8785's order.
type revocationObjectHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Typ string `json:"typ"`
}

// Signs payload under the svc- set's newest key, as a compact JWS: what api
// writes to rev/. An issuer with no svc- key signs nothing.
func SignRevocation(payload []byte) ([]byte, error) {
	signingKey, err := SigningKeyOf(SignerKeySetService)
	if err != nil {
		return nil, err
	}
	return signRevocationWith(signingKey, payload)
}

func signRevocationWith(signingKey *SignerKey, payload []byte) ([]byte, error) {
	header, err := json.Marshal(&revocationObjectHeader{
		Alg: signerAlgEs256,
		Kid: signingKey.Kid,
		Typ: sidecar.RevocationJwsType,
	})
	if err != nil {
		return nil, err
	}
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	signature, err := gojwt.SigningMethodES256.Sign(signingInput, signingKey.PrivateKey)
	if err != nil {
		return nil, err
	}
	return []byte(signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)), nil
}

// The payload of object, a rev/ object this verifier's issuer signed: three
// base64url parts, a header naming ES256, an svc- kid this verifier holds and
// the rev/ type, and a signature that verifies under that key. A failure wraps
// ErrInvalidRevocationObject.
func (self *ServiceTokenVerifier) VerifyRevocation(object []byte) ([]byte, error) {
	return verifyRevocationWith(object, func(kid string) *ecdsa.PublicKey {
		if SignerKeySetOfKid(kid) != SignerKeySetService {
			return nil
		}
		return self.keys[kid]
	})
}

func verifyRevocationWith(object []byte, publicKeyOf func(kid string) *ecdsa.PublicKey) ([]byte, error) {
	refuse := func(detail string) ([]byte, error) {
		return nil, fmt.Errorf("%w: %s", ErrInvalidRevocationObject, detail)
	}
	if sidecar.RevocationObjectMaxByteCount < len(object) {
		return refuse("too large")
	}
	parts := strings.Split(string(object), ".")
	if len(parts) != 3 {
		return refuse("not a compact JWS")
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return refuse("the header is not base64url")
	}
	header := &revocationObjectHeader{}
	decoder := json.NewDecoder(strings.NewReader(string(headerBytes)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(header); err != nil {
		return refuse("the header does not parse")
	}
	if header.Alg != signerAlgEs256 || header.Typ != sidecar.RevocationJwsType {
		return refuse("the header's alg or typ")
	}
	publicKey := publicKeyOf(header.Kid)
	if publicKey == nil {
		return refuse("no svc- key of that kid")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return refuse("the signature is not base64url")
	}
	if err := gojwt.SigningMethodES256.Verify(parts[0]+"."+parts[1], signature, publicKey); err != nil {
		return refuse("the signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return refuse("the payload is not base64url")
	}
	return payload, nil
}

// The payload of a rev/ object by the process's verifier: a non-issuer block's
// mounted svc- jwks, else the issuer's own (VerifyServiceToken's). What evs and
// gkm pass to sidecar.NewRevocationChecker. A process with no usable verifier
// refuses every object with an error that is not ErrInvalidRevocationObject.
func VerifyRevocation(object []byte) ([]byte, error) {
	verifier, err := currentServiceTokenVerifier()
	if err != nil {
		return nil, fmt.Errorf("no service token verifier: %w", err)
	}
	return verifier.VerifyRevocation(object)
}

var _ sidecar.RevocationVerifyFunction = VerifyRevocation
