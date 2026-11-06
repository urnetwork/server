package session

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/server"
)

// A strict_exp bearer proves it holds its ClientKey to renew, exchange or mint
// (§7.4, Q-CORE-4), so a token copied alone does none of them. The proof is
// DPoP, RFC 9449, in the shape §7.4 fixes:
//
//	header  {typ dpop+jwt, alg EdDSA, jwk}
//	claims  {jti, htm, htu, iat, ath = SHA-256(token)}
//	replay  SET {dpop_<client_id>}<jti> 1 NX EX 120; iat within ±60 s
//
// The caller supplies the key the client has bound and this checks everything
// else. A proof that fails any check spends no jti, so nobody without the key
// can burn one.

// Settings of the proof check.
type DpopProofSettings struct {
	// How far a proof's iat may sit from now, either way. §7.4 fixes 60 s.
	IatWindow time.Duration
	// How long a spent jti is kept. §7.4 fixes 120 s. A proof is usable for at
	// most twice IatWindow, so a shorter ttl would let one replay.
	ReplayTtl time.Duration
	// The longest jti accepted, in bytes: it becomes part of a Redis key.
	MaxJtiLength int
	// Whether htu may name http. Production's api serves only https; a local
	// stack may not.
	AllowHttp bool
}

// §7.4's window and ttl, with http on a local stack alone.
func DefaultDpopProofSettings() *DpopProofSettings {
	env, _ := server.Env()
	return &DpopProofSettings{
		IatWindow:    60 * time.Second,
		ReplayTtl:    120 * time.Second,
		MaxJtiLength: 256,
		AllowHttp:    env == "local",
	}
}

const (
	// the request header that carries the proof (RFC 9449 §4.1)
	DpopHeader = "DPoP"
	dpopType   = "dpop+jwt"
	dpopAlg    = "EdDSA"
)

// A proof that did not verify. Its message is for logs at V(1) only: a refusal
// is driven by client input, and the caller answers it with a fixed message.
var ErrDpopProofRefused = errors.New("DPoP proof refused")

// A refusal with its detail for the logs.
func dpopRefused(detail string) error {
	return fmt.Errorf("%w: %s", ErrDpopProofRefused, detail)
}

// A proof's claims. jti and iat are registered claims.
type dpopProofClaims struct {
	Htm string `json:"htm"`
	Htu string `json:"htu"`
	Ath string `json:"ath"`
	gojwt.RegisteredClaims
}

// The Ed25519 public key a proof's jwk header holds, which must be exactly that:
// an OKP key on Ed25519 with no private part.
func dpopPublicKey(token *gojwt.Token) (ed25519.PublicKey, error) {
	jwk, ok := token.Header["jwk"].(map[string]any)
	if !ok {
		return nil, dpopRefused("no jwk")
	}
	if _, private := jwk["d"]; private {
		return nil, dpopRefused("jwk holds a private key")
	}
	if jwk["kty"] != "OKP" || jwk["crv"] != "Ed25519" {
		return nil, dpopRefused("jwk is not an Ed25519 key")
	}
	x, ok := jwk["x"].(string)
	if !ok {
		return nil, dpopRefused("jwk has no x")
	}
	publicKey, err := base64.RawURLEncoding.DecodeString(x)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return nil, dpopRefused("jwk x is not a public key")
	}
	return publicKey, nil
}

// host and port in the form URIs compare equal in (RFC 3986 §6.2.3): lower case,
// and without the scheme's default port.
func dpopCanonicalHost(scheme string, host string) string {
	host = strings.ToLower(host)
	switch scheme {
	case "https":
		return strings.TrimSuffix(host, ":443")
	case "http":
		return strings.TrimSuffix(host, ":80")
	default:
		return host
	}
}

// Whether htu names the request's URI, ignoring query and fragment (RFC 9449
// §4.3). What reached this process past the load balancer is http whatever the
// client dialed, so the scheme is checked against what the api serves and only
// the host and path against the request. The host is the one the ingress passed
// on, which the sender chose, and that is enough here: htu keeps a proof from
// serving another route, and a proof that is not spent yet needs the bearer token
// it hashes to be of any use.
func dpopHtuMatches(htu string, r *http.Request, settings *DpopProofSettings) bool {
	target, err := url.Parse(htu)
	if err != nil {
		return false
	}
	if target.Scheme != "https" && !(settings.AllowHttp && target.Scheme == "http") {
		return false
	}
	return dpopCanonicalHost(target.Scheme, target.Host) == dpopCanonicalHost(target.Scheme, r.Host) &&
		target.Path == r.URL.Path
}

// Checks one proof of the request against the bearer token and the client's
// bound key and returns its jti, unspent. Reads no store.
func verifyDpopProof(proof string, r *http.Request, bearerToken string, boundKey ed25519.PublicKey, now time.Time, settings *DpopProofSettings) (string, error) {
	claims := &dpopProofClaims{}
	keyFunc := func(token *gojwt.Token) (any, error) {
		if token.Header["typ"] != dpopType {
			return nil, dpopRefused("typ is not dpop+jwt")
		}
		publicKey, err := dpopPublicKey(token)
		if err != nil {
			return nil, err
		}
		// the proof names the key it was signed by, and only the newest bound
		// key counts: an older generation's key is refused here
		if !publicKey.Equal(boundKey) {
			return nil, dpopRefused("jwk is not the bound key")
		}
		return boundKey, nil
	}
	parser := gojwt.NewParser(gojwt.WithValidMethods([]string{dpopAlg}), gojwt.WithoutClaimsValidation())
	if _, err := parser.ParseWithClaims(proof, claims, keyFunc); err != nil {
		return "", fmt.Errorf("%w: %w", ErrDpopProofRefused, err)
	}
	if claims.ID == "" || settings.MaxJtiLength < len(claims.ID) {
		return "", dpopRefused("jti is missing or too long")
	}
	if claims.Htm != r.Method {
		return "", dpopRefused("htm is not the request's method")
	}
	if !dpopHtuMatches(claims.Htu, r, settings) {
		return "", dpopRefused("htu is not the request's uri")
	}
	if claims.IssuedAt == nil {
		return "", dpopRefused("no iat")
	}
	if age := now.Sub(claims.IssuedAt.Time); age < -settings.IatWindow || settings.IatWindow < age {
		return "", dpopRefused("iat is outside its window")
	}
	hash := sha256.Sum256([]byte(bearerToken))
	if claims.Ath != base64.RawURLEncoding.EncodeToString(hash[:]) {
		return "", dpopRefused("ath is not the bearer token's hash")
	}
	return claims.ID, nil
}

// The replay key §7.4 names, one hash slot per client.
func dpopJtiKey(clientId server.Id, jti string) string {
	return fmt.Sprintf("{dpop_%s}%s", clientId, jti)
}

// Spends the jti once. An unavailable store refuses: nothing can tell a replay
// from a first use without it.
func spendDpopJti(ctx context.Context, clientId server.Id, jti string, settings *DpopProofSettings) error {
	spent := false
	err := server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
		var err error
		spent, err = r.SetNX(ctx, dpopJtiKey(clientId, jti), 1, settings.ReplayTtl).Result()
		return err
	})
	if err != nil {
		return fmt.Errorf("%w: %w", ErrDpopProofRefused, errors.Join(ErrAuthUnavailable, err))
	}
	if !spent {
		return dpopRefused("jti was already used")
	}
	return nil
}

// Verifies the request's DPoP proof for the client and spends its jti, or
// returns an error wrapping ErrDpopProofRefused. boundKey is the client's newest
// bound key; with none, the proof is refused.
func VerifyDpopProof(ctx context.Context, r *http.Request, bearerToken string, clientId server.Id, boundKey ed25519.PublicKey) error {
	if len(boundKey) != ed25519.PublicKeySize {
		return dpopRefused("the client has no bound key")
	}
	proofs := r.Header.Values(DpopHeader)
	if len(proofs) != 1 {
		return dpopRefused("the request does not carry exactly one proof")
	}
	settings := DefaultDpopProofSettings()
	jti, err := verifyDpopProof(proofs[0], r, bearerToken, boundKey, server.NowUtc(), settings)
	if err != nil {
		return err
	}
	return spendDpopJti(ctx, clientId, jti, settings)
}

// The proof a sidecar would send, by the key given: what sdk's Api.SetDpopSigner
// signs from the request's method and url and the bearer it proves.
func Testing_SignDpopProof(privateKey ed25519.PrivateKey, method string, url string, bearerToken string, jti string, iat time.Time) string {
	hash := sha256.Sum256([]byte(bearerToken))
	claims := &dpopProofClaims{
		Htm: method,
		Htu: url,
		Ath: base64.RawURLEncoding.EncodeToString(hash[:]),
		RegisteredClaims: gojwt.RegisteredClaims{
			ID:       jti,
			IssuedAt: gojwt.NewNumericDate(iat),
		},
	}
	token := gojwt.NewWithClaims(gojwt.SigningMethodEdDSA, claims)
	token.Header["typ"] = dpopType
	token.Header["jwk"] = map[string]any{
		"kty": "OKP",
		"crv": "Ed25519",
		"x":   base64.RawURLEncoding.EncodeToString(privateKey.Public().(ed25519.PublicKey)),
	}
	signed, err := token.SignedString(privateKey)
	if err != nil {
		panic(err)
	}
	return signed
}
