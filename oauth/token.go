package oauth

// Access tokens and openid connect id tokens.
//
// An access token is audience bound: `aud` is the canonical uri of the
// resource it was requested for (rfc 8707), and a resource server MUST reject
// a token minted for anything else. `VerifyAccessToken` takes the expected
// audience for exactly that reason -- there is no "verify without audience"
// entry point, so a caller cannot accidentally accept another resource's
// token.
//
// The subject is the user; the network the token acts on is a separate claim
// bound at authorization time, so a token cannot follow the user to a
// different network later.

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/server"
)

var ErrInvalidToken = errors.New("invalid token")

// The claims of an issued access token.
type AccessTokenClaims struct {
	// space delimited, per rfc 8693
	Scope string `json:"scope,omitempty"`
	// the network this token acts on, bound at authorization time
	NetworkId server.Id `json:"network_id,omitempty"`
	// the client the token was issued to
	ClientId string `json:"client_id,omitempty"`
	// identity carried through from the authorizing session
	Principal string   `json:"principal,omitempty"`
	Roles     []string `json:"roles,omitempty"`

	gojwt.RegisteredClaims
}

type MintAccessTokenArgs struct {
	UserId    server.Id
	NetworkId server.Id
	ClientId  string
	// the canonical uri of the resource the token is for
	Audience  string
	Scopes    []string
	Principal string
	Roles     []string
}

// Mints a signed access token. The caller has already authorized the request;
// this only encodes the decision.
func MintAccessToken(args *MintAccessTokenArgs) (string, time.Time, error) {
	if args.UserId == (server.Id{}) {
		return "", time.Time{}, fmt.Errorf("user_id is required")
	}
	if args.NetworkId == (server.Id{}) {
		return "", time.Time{}, fmt.Errorf("network_id is required")
	}
	if args.Audience == "" {
		return "", time.Time{}, fmt.Errorf("audience is required")
	}

	now := server.NowUtc()
	expiry := now.Add(AccessTokenDuration)

	claims := &AccessTokenClaims{
		Scope:     FormatScope(args.Scopes),
		NetworkId: args.NetworkId,
		ClientId:  args.ClientId,
		Principal: args.Principal,
		Roles:     args.Roles,
		RegisteredClaims: gojwt.RegisteredClaims{
			Issuer:    Issuer(),
			Subject:   args.UserId.String(),
			Audience:  gojwt.ClaimStrings{args.Audience},
			IssuedAt:  gojwt.NewNumericDate(now),
			NotBefore: gojwt.NewNumericDate(now),
			ExpiresAt: gojwt.NewNumericDate(expiry),
			ID:        server.NewId().String(),
		},
	}

	signed, err := sign(claims)
	if err != nil {
		return "", time.Time{}, err
	}
	return signed, expiry, nil
}

// Verifies a signed access token for a specific resource. The audience is
// required: a resource server must only accept tokens minted for itself.
func VerifyAccessToken(tokenStr string, expectedAudience string) (*AccessTokenClaims, error) {
	if expectedAudience == "" {
		return nil, fmt.Errorf("an expected audience is required")
	}

	claims := &AccessTokenClaims{}

	// unlike the ByJwt parser, every registered claim is validated here
	_, err := gojwt.ParseWithClaims(
		tokenStr,
		claims,
		verificationKeyFunc,
		gojwt.WithValidMethods([]string{signerAlgEs256}),
		gojwt.WithIssuer(Issuer()),
		gojwt.WithAudience(expectedAudience),
		gojwt.WithExpirationRequired(),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidToken, err)
	}

	if claims.NetworkId == (server.Id{}) {
		return nil, fmt.Errorf("%w: no network", ErrInvalidToken)
	}

	return claims, nil
}

// Verifies an access token this issuer minted, WITHOUT checking its audience.
//
// This exists for the issuer's own endpoints -- userinfo -- where the caller
// presents whatever access token it already holds, whose audience is the
// resource it was minted for (e.g. the mcp server) rather than the issuer.
// Requiring the audience there would make userinfo unreachable for every real
// client.
//
// A RESOURCE SERVER MUST NOT USE THIS. Skipping the audience check is exactly
// what lets a token minted for one resource be replayed at another; use
// `VerifyAccessToken` with an explicit audience instead.
func VerifyAccessTokenAnyResource(tokenStr string) (*AccessTokenClaims, error) {
	claims := &AccessTokenClaims{}

	_, err := gojwt.ParseWithClaims(
		tokenStr,
		claims,
		verificationKeyFunc,
		gojwt.WithValidMethods([]string{signerAlgEs256}),
		gojwt.WithIssuer(Issuer()),
		gojwt.WithExpirationRequired(),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidToken, err)
	}

	// an audience is still required to be present: a token with none was not
	// minted by the code above and should never verify
	if len(claims.Audience) == 0 {
		return nil, fmt.Errorf("%w: no audience", ErrInvalidToken)
	}
	if claims.NetworkId == (server.Id{}) {
		return nil, fmt.Errorf("%w: no network", ErrInvalidToken)
	}

	return claims, nil
}

// The claims of an issued id token. The id token proves an authentication
// event to the client and is never presented to a resource server.
type IdTokenClaims struct {
	Nonce string `json:"nonce,omitempty"`
	// seconds since epoch of the authentication event, per openid connect
	AuthTime int64 `json:"auth_time,omitempty"`
	// hash of the access token issued alongside, per openid connect core 3.1.3.6
	AccessTokenHash string `json:"at_hash,omitempty"`

	NetworkId   server.Id `json:"network_id,omitempty"`
	NetworkName string    `json:"network_name,omitempty"`

	gojwt.RegisteredClaims
}

type MintIdTokenArgs struct {
	UserId      server.Id
	NetworkId   server.Id
	NetworkName string
	// the id token is addressed to the client, not to a resource
	ClientId    string
	Nonce       string
	AuthTime    time.Time
	AccessToken string
}

func MintIdToken(args *MintIdTokenArgs) (string, error) {
	if args.UserId == (server.Id{}) {
		return "", fmt.Errorf("user_id is required")
	}
	if args.ClientId == "" {
		return "", fmt.Errorf("client_id is required")
	}

	now := server.NowUtc()

	claims := &IdTokenClaims{
		Nonce:       args.Nonce,
		NetworkId:   args.NetworkId,
		NetworkName: args.NetworkName,
		RegisteredClaims: gojwt.RegisteredClaims{
			Issuer:    Issuer(),
			Subject:   args.UserId.String(),
			Audience:  gojwt.ClaimStrings{args.ClientId},
			IssuedAt:  gojwt.NewNumericDate(now),
			ExpiresAt: gojwt.NewNumericDate(now.Add(IdTokenDuration)),
			ID:        server.NewId().String(),
		},
	}
	if !args.AuthTime.IsZero() {
		claims.AuthTime = args.AuthTime.Unix()
	}
	if args.AccessToken != "" {
		claims.AccessTokenHash = accessTokenHash(args.AccessToken)
	}

	return sign(claims)
}

// Verifies an id token addressed to a client. Used by our own clients and by
// the conformance tests; a resource server never sees one.
func VerifyIdToken(tokenStr string, clientId string) (*IdTokenClaims, error) {
	claims := &IdTokenClaims{}

	_, err := gojwt.ParseWithClaims(
		tokenStr,
		claims,
		verificationKeyFunc,
		gojwt.WithValidMethods([]string{signerAlgEs256}),
		gojwt.WithIssuer(Issuer()),
		gojwt.WithAudience(clientId),
		gojwt.WithExpirationRequired(),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidToken, err)
	}

	return claims, nil
}

func sign(claims gojwt.Claims) (string, error) {
	signingKey := SigningKey()

	token := gojwt.NewWithClaims(gojwt.SigningMethodES256, claims)
	// the kid lets a verifier select the key directly, and lets a rotated key
	// be identified in a token that is still in flight
	token.Header["kid"] = signingKey.Kid

	return token.SignedString(signingKey.PrivateKey)
}

// Selects the verification key by kid. A token with no kid, or an unknown one,
// is rejected rather than tried against every key: these tokens are always
// minted by us with a kid, so a missing one is a malformed token.
func verificationKeyFunc(token *gojwt.Token) (any, error) {
	kid, ok := token.Header["kid"].(string)
	if !ok {
		return nil, fmt.Errorf("no kid")
	}
	signerKey := VerificationKey(kid)
	if signerKey == nil {
		return nil, fmt.Errorf("unknown kid %s", kid)
	}
	return &signerKey.PrivateKey.PublicKey, nil
}

// openid connect core 3.1.3.6: the base64url of the left-most half of the
// sha256 of the access token octets.
func accessTokenHash(accessToken string) string {
	sum := sha256.Sum256([]byte(accessToken))
	return base64.RawURLEncoding.EncodeToString(sum[:len(sum)/2])
}

func base64RawUrlSha256(s string) string {
	sum := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
