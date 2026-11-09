package session

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
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
// A DpopRequest says what one proof must name and bind and whose key must sign
// it. Its caller fixes the uri from the origin it is configured with, never from
// a header the request carries, and looks the key up: api's gates the ClientKey
// (model.ClientProofPublicKey), the token exchange the same (TEAM-M1-06), and an
// admin token's check the key its cnf.jkt names (TEAM-M1-08). A proof that fails
// any check spends no jti, so nobody without the key can burn one.

// Settings of the proof check.
type DpopProofSettings struct {
	// How far a proof's iat may sit from now, either way. §7.4 fixes 60 s.
	IatWindow time.Duration
	// How long a spent jti is kept. §7.4 fixes 120 s. A proof is usable for at
	// most twice IatWindow, so a shorter ttl would let one replay.
	ReplayTtl time.Duration
	// The longest jti accepted, in bytes: it becomes part of a Redis key.
	MaxJtiLength int
}

// §7.4's window and ttl.
func DefaultDpopProofSettings() *DpopProofSettings {
	return &DpopProofSettings{
		IatWindow:    60 * time.Second,
		ReplayTtl:    120 * time.Second,
		MaxJtiLength: 256,
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

// One proof's check: what it must name and bind, and the key that must sign it.
type DpopRequest struct {
	// The proof: the request's one DPoP header.
	Proof string
	// htm: the request's method.
	Method string
	// htu: the request's uri at the origin the caller is configured with, with no
	// query or fragment. A proof whose htu names another scheme, host or path is
	// refused, so a proof a client sent elsewhere proves nothing here.
	Uri string
	// The token the proof is presented with, whose SHA-256 is ath: the bearer, or
	// the token exchange's subject token.
	Token string
	// The Ed25519 key the proof must be signed by, for a key the caller looked
	// up. Set exactly one of Key and KeyThumbprint.
	Key ed25519.PublicKey
	// The RFC 7638 thumbprint the proof's own key must have, for a key a token
	// names by cnf.jkt (RFC 9449 §6.1).
	KeyThumbprint string
	// Whose jti space the proof spends: {dpop_<ReplayId>}<jti>, the client's for
	// a client's proof.
	ReplayId server.Id
}

// The api's public origin, https://api.<WARP_DOMAIN>: the origin every caller of
// the api names, which x402's terms name too. Without WARP_DOMAIN there is none,
// and every api proof is refused.
func DpopApiOrigin() (string, error) {
	domain, err := server.Domain()
	if err != nil {
		return "", err
	}
	return "https://api." + domain, nil
}

// The check of an api request's proof: its one DPoP header, its method, and its
// path at the api's public origin (DpopApiOrigin), by key, spending in clientId's
// jti space. A request that does not carry exactly one proof, or an api with no
// origin, is refused here.
func NewApiDpopRequest(r *http.Request, token string, clientId server.Id, key ed25519.PublicKey) (*DpopRequest, error) {
	proofs := r.Header.Values(DpopHeader)
	if len(proofs) != 1 {
		return nil, dpopRefused("the request does not carry exactly one proof")
	}
	origin, err := DpopApiOrigin()
	if err != nil {
		return nil, dpopRefused("the api has no public origin")
	}
	return &DpopRequest{
		Proof:    proofs[0],
		Method:   r.Method,
		Uri:      origin + r.URL.Path,
		Token:    token,
		Key:      key,
		ReplayId: clientId,
	}, nil
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

// The RFC 7638 thumbprint of an Ed25519 key as a proof's jwk names it (RFC 8037
// §2): base64url(SHA-256({"crv":"Ed25519","kty":"OKP","x":<x>})), the members in
// that order and no whitespace. It is what an admin token's cnf.jkt holds, and
// what api's service log records for a proof's key (§2.8 api-11).
func DpopKeyThumbprint(publicKey ed25519.PublicKey) string {
	canonical := `{"crv":"Ed25519","kty":"OKP","x":"` + base64.RawURLEncoding.EncodeToString(publicKey) + `"}`
	hash := sha256.Sum256([]byte(canonical))
	return base64.RawURLEncoding.EncodeToString(hash[:])
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

// Whether htu names uri, ignoring htu's query and fragment (RFC 9449 §4.3).
// uri is what the caller is configured to serve, so a client's choice of Host
// header or forwarded headers decides nothing here. Only https is ever named.
func dpopHtuMatches(htu string, uri string) bool {
	target, err := url.Parse(htu)
	if err != nil || target.Scheme != "https" || target.User != nil {
		return false
	}
	expected, err := url.Parse(uri)
	if err != nil || expected.Scheme != "https" {
		return false
	}
	return dpopCanonicalHost(target.Scheme, target.Host) == dpopCanonicalHost(expected.Scheme, expected.Host) &&
		target.Path == expected.Path
}

// Checks one proof against its request and returns its jti, unspent. Reads no
// store.
func verifyDpopProof(dpopRequest *DpopRequest, now time.Time, settings *DpopProofSettings) (string, error) {
	keyByValue := len(dpopRequest.Key) != 0
	keyByThumbprint := dpopRequest.KeyThumbprint != ""
	if keyByValue == keyByThumbprint {
		return "", dpopRefused("the check names no one key")
	}
	if keyByValue && len(dpopRequest.Key) != ed25519.PublicKeySize {
		return "", dpopRefused("the bound key is not an Ed25519 key")
	}
	claims := &dpopProofClaims{}
	keyFunc := func(token *gojwt.Token) (any, error) {
		if token.Header["typ"] != dpopType {
			return nil, dpopRefused("typ is not dpop+jwt")
		}
		publicKey, err := dpopPublicKey(token)
		if err != nil {
			return nil, err
		}
		// the proof names the key it was signed by, and only the key the check
		// names counts: an older generation's key is refused here
		if keyByValue && !publicKey.Equal(dpopRequest.Key) {
			return nil, dpopRefused("jwk is not the bound key")
		}
		if keyByThumbprint && subtle.ConstantTimeCompare([]byte(DpopKeyThumbprint(publicKey)), []byte(dpopRequest.KeyThumbprint)) != 1 {
			return nil, dpopRefused("jwk is not the key the token names")
		}
		return publicKey, nil
	}
	parser := gojwt.NewParser(gojwt.WithValidMethods([]string{dpopAlg}), gojwt.WithoutClaimsValidation())
	if _, err := parser.ParseWithClaims(dpopRequest.Proof, claims, keyFunc); err != nil {
		return "", fmt.Errorf("%w: %w", ErrDpopProofRefused, err)
	}
	if claims.ID == "" || settings.MaxJtiLength < len(claims.ID) {
		return "", dpopRefused("jti is missing or too long")
	}
	if claims.Htm != dpopRequest.Method {
		return "", dpopRefused("htm is not the request's method")
	}
	if !dpopHtuMatches(claims.Htu, dpopRequest.Uri) {
		return "", dpopRefused("htu is not the request's uri")
	}
	if claims.IssuedAt == nil {
		return "", dpopRefused("no iat")
	}
	if age := now.Sub(claims.IssuedAt.Time); age < -settings.IatWindow || settings.IatWindow < age {
		return "", dpopRefused("iat is outside its window")
	}
	hash := sha256.Sum256([]byte(dpopRequest.Token))
	if subtle.ConstantTimeCompare([]byte(claims.Ath), []byte(base64.RawURLEncoding.EncodeToString(hash[:]))) != 1 {
		return "", dpopRefused("ath is not the token's hash")
	}
	return claims.ID, nil
}

// The replay key §7.4 names, one hash slot per client.
func dpopJtiKey(replayId server.Id, jti string) string {
	return fmt.Sprintf("{dpop_%s}%s", replayId, jti)
}

// Spends the jti once. An unavailable store refuses: nothing can tell a replay
// from a first use without it.
func spendDpopJti(ctx context.Context, replayId server.Id, jti string, settings *DpopProofSettings) error {
	spent := false
	err := server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
		var err error
		spent, err = r.SetNX(ctx, dpopJtiKey(replayId, jti), 1, settings.ReplayTtl).Result()
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

// Verifies the proof and spends its jti, or returns an error wrapping
// ErrDpopProofRefused. A check that names no key, as for a client with none
// bound, is refused.
func VerifyDpopProof(ctx context.Context, dpopRequest *DpopRequest) error {
	if dpopRequest == nil {
		return dpopRefused("no proof")
	}
	settings := DefaultDpopProofSettings()
	jti, err := verifyDpopProof(dpopRequest, server.NowUtc(), settings)
	if err != nil {
		return err
	}
	return spendDpopJti(ctx, dpopRequest.ReplayId, jti, settings)
}

// The proof a sidecar would send, by the key given: what sdk's Api.SetDpopSigner
// signs from the request's method and url and the token it proves.
func Testing_SignDpopProof(privateKey ed25519.PrivateKey, method string, url string, token string, jti string, iat time.Time) string {
	hash := sha256.Sum256([]byte(token))
	claims := &dpopProofClaims{
		Htm: method,
		Htu: url,
		Ath: base64.RawURLEncoding.EncodeToString(hash[:]),
		RegisteredClaims: gojwt.RegisteredClaims{
			ID:       jti,
			IssuedAt: gojwt.NewNumericDate(iat),
		},
	}
	proof := gojwt.NewWithClaims(gojwt.SigningMethodEdDSA, claims)
	proof.Header["typ"] = dpopType
	proof.Header["jwk"] = map[string]any{
		"kty": "OKP",
		"crv": "Ed25519",
		"x":   base64.RawURLEncoding.EncodeToString(privateKey.Public().(ed25519.PublicKey)),
	}
	signed, err := proof.SignedString(privateKey)
	if err != nil {
		panic(err)
	}
	return signed
}

// The url an api request's proof names for path: the api's public origin and
// the path, as a sidecar configured for this api signs it.
func Testing_DpopApiUrl(path string) string {
	origin, err := DpopApiOrigin()
	if err != nil {
		panic(err)
	}
	return origin + path
}
