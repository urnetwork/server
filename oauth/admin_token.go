package oauth

// One-command admin tokens (§7.8, D-TEAM-7): what ur-cli's code grant mints, an
// ES256 JWT under the adm- key set, never the mcp, svc- or id- sets or the
// ByJwt keys, so it is no credential anywhere else:
//
//	header  {alg ES256, kid adm-..., typ at+jwt}
//	claims  {iss, sub, aud "api", network_id, member_id, scope, cnf {jkt}, jti,
//	         iat, exp <= 1 h, cmd}
//
// cmd is sidecar.AdminCommandDigest of the one command the person consented to,
// method, path and JCS body, and cnf.jkt the thumbprint of the DPoP key ur-cli
// holds in its own process (RFC 9449 §6.1). api takes the token only as
// "Authorization: DPoP <token>" with a proof by that key naming the request's
// method and path at the api's own origin, recomputes cmd from the request and
// refuses a mismatch, and its route's model spends the jti in the command's own
// transaction (model.ClaimAdminTokenUseInTx), so a replay returns the stored
// result and runs nothing. The token enters the session as an unsigned identity,
// so no route signs it back into a by_jwt (session.AdminCommandUse).
//
// network_id is not among §7.8's claims; api needs it to act for the member's
// network, as service tokens carry it. Tokens are never stored.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

const (
	// §7.8: an admin token's audience is the api
	AdminTokenAudience = "api"
	// RFC 9068's access token type, which keeps an admin token apart from an id
	// token or a consent receipt of the same key set
	adminTokenType = "at+jwt"
	// RFC 9449 §7.1's token type in a token response
	adminTokenResponseType = "DPoP"
)

// A token that is no admin token, or whose proof or command does not match its
// request.
var ErrInvalidAdminToken = errors.New("invalid admin token")

// RFC 7800's confirmation: the DPoP key's RFC 7638 thumbprint.
type AdminTokenConfirmation struct {
	Jkt string `json:"jkt"`
}

// The claims of an admin token.
type AdminTokenClaims struct {
	// space delimited
	Scope        string                  `json:"scope"`
	NetworkId    server.Id               `json:"network_id"`
	MemberId     server.Id               `json:"member_id"`
	Confirmation *AdminTokenConfirmation `json:"cnf"`
	Cmd          string                  `json:"cmd"`

	gojwt.RegisteredClaims
}

// What an admin token says.
type MintAdminTokenArgs struct {
	NetworkId server.Id
	// the user, in a personal network
	MemberId server.Id
	Scopes   []string
	// the DPoP key's thumbprint
	Jkt string
	// sidecar.AdminCommandDigest of the consented command
	Cmd      string
	Duration time.Duration
}

// The checks every admin token meets, minted or verified.
func validateAdminTokenClaims(claims *AdminTokenClaims) error {
	if claims.NetworkId == (server.Id{}) || claims.MemberId == (server.Id{}) {
		return errors.New("no network or member")
	}
	if claims.Confirmation == nil || claims.Confirmation.Jkt == "" {
		return errors.New("no cnf.jkt")
	}
	if claims.Cmd == "" {
		return errors.New("no cmd")
	}
	if len(claims.RegisteredClaims.Audience) != 1 || claims.RegisteredClaims.Audience[0] != AdminTokenAudience {
		return errors.New("not the api's audience")
	}
	if claims.ID == "" || 64 < len(claims.ID) {
		return errors.New("no jti")
	}
	if len(ParseScope(claims.Scope)) == 0 {
		return errors.New("no scope")
	}
	return nil
}

// Mints an admin token under the adm- set's newest key. The caller has already
// authorized every claim: ur-cli's code grant after a consent that showed the
// command and its code.
func MintAdminToken(args *MintAdminTokenArgs) (string, time.Time, error) {
	signingKey, err := SigningKeyOf(SignerKeySetAdmin)
	if err != nil {
		return "", time.Time{}, err
	}
	now := server.NowUtc()
	expiry := now.Add(args.Duration)
	claims := &AdminTokenClaims{
		Scope:        FormatScope(args.Scopes),
		NetworkId:    args.NetworkId,
		MemberId:     args.MemberId,
		Confirmation: &AdminTokenConfirmation{Jkt: args.Jkt},
		Cmd:          args.Cmd,
		RegisteredClaims: gojwt.RegisteredClaims{
			Issuer:    Issuer(),
			Subject:   args.MemberId.String(),
			Audience:  gojwt.ClaimStrings{AdminTokenAudience},
			IssuedAt:  gojwt.NewNumericDate(now),
			NotBefore: gojwt.NewNumericDate(now),
			ExpiresAt: gojwt.NewNumericDate(expiry),
			ID:        server.NewId().String(),
		},
	}
	if args.Duration <= 0 || DefaultUrcliSettings().AdminTokenDuration < args.Duration {
		return "", time.Time{}, fmt.Errorf("admin token: a lifetime of %s", args.Duration)
	}
	if err := validateAdminTokenClaims(claims); err != nil {
		return "", time.Time{}, fmt.Errorf("admin token: %w", err)
	}
	token := gojwt.NewWithClaims(gojwt.SigningMethodES256, claims)
	token.Header["kid"] = signingKey.Kid
	token.Header["typ"] = adminTokenType
	signed, err := token.SignedString(signingKey.PrivateKey)
	if err != nil {
		return "", time.Time{}, err
	}
	return signed, expiry, nil
}

// The claims of token, an admin token of this issuer: alg ES256, typ at+jwt, an
// adm- kid the issuer lists, iss, the api audience, exp and iat present and exp
// not passed, and the claims every admin token carries. A failure wraps
// ErrInvalidAdminToken.
func VerifyAdminToken(token string) (*AdminTokenClaims, error) {
	return verifyAdminTokenAt(token, server.NowUtc)
}

func verifyAdminTokenAt(token string, now func() time.Time) (*AdminTokenClaims, error) {
	claims := &AdminTokenClaims{}
	keyFunc := func(token *gojwt.Token) (any, error) {
		if token.Header["typ"] != adminTokenType {
			return nil, errors.New("not an access token's type")
		}
		kid, ok := token.Header["kid"].(string)
		if !ok {
			return nil, errors.New("no kid")
		}
		key := verificationKeyOf(SignerKeySetAdmin, kid)
		if key == nil {
			return nil, fmt.Errorf("kid %s is no admin key", kid)
		}
		return &key.PrivateKey.PublicKey, nil
	}
	_, err := gojwt.ParseWithClaims(
		token,
		claims,
		keyFunc,
		gojwt.WithValidMethods([]string{signerAlgEs256}),
		gojwt.WithIssuer(Issuer()),
		gojwt.WithAudience(AdminTokenAudience),
		gojwt.WithExpirationRequired(),
		gojwt.WithIssuedAt(),
		gojwt.WithTimeFunc(now),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidAdminToken, err)
	}
	if claims.IssuedAt == nil || claims.ExpiresAt == nil {
		return nil, fmt.Errorf("%w: no iat or exp", ErrInvalidAdminToken)
	}
	if DefaultUrcliSettings().AdminTokenDuration < claims.ExpiresAt.Sub(claims.IssuedAt.Time) {
		return nil, fmt.Errorf("%w: a lifetime past an hour", ErrInvalidAdminToken)
	}
	if err := validateAdminTokenClaims(claims); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidAdminToken, err)
	}
	return claims, nil
}

// The session's check of an admin token sent as "DPoP <token>": the token,
// its one proof by the cnf.jkt key naming the request's method and path at the
// api's public origin with ath over the token (its jti spent in the member's
// jti space), no query, and cmd recomputed from the request's method, path and
// body. The body is read whole, within the command's bound, and left readable.
func authenticateAdminToken(ctx context.Context, req *http.Request, token string) (*session.AdminCommandUse, error) {
	claims, err := VerifyAdminToken(token)
	if err != nil {
		return nil, err
	}
	if req.URL.RawQuery != "" || req.URL.Fragment != "" {
		return nil, fmt.Errorf("%w: a query the command does not cover", ErrInvalidAdminToken)
	}
	proofs := req.Header.Values(session.DpopHeader)
	if len(proofs) != 1 {
		return nil, fmt.Errorf("%w: the request does not carry exactly one proof", ErrInvalidAdminToken)
	}
	origin, err := session.DpopApiOrigin()
	if err != nil {
		return nil, fmt.Errorf("%w: the api has no public origin", ErrInvalidAdminToken)
	}
	if err := session.VerifyDpopProof(ctx, &session.DpopRequest{
		Proof:         proofs[0],
		Method:        req.Method,
		Uri:           origin + req.URL.Path,
		Token:         token,
		KeyThumbprint: claims.Confirmation.Jkt,
		ReplayId:      claims.MemberId,
	}); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidAdminToken, err)
	}

	body := []byte{}
	if req.Body != nil {
		body, err = io.ReadAll(io.LimitReader(req.Body, sidecar.AdminCommandMaxBodyByteCount+1))
		req.Body.Close()
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	if sidecar.AdminCommandMaxBodyByteCount < len(body) {
		return nil, fmt.Errorf("%w: a body past the command's bound", ErrInvalidAdminToken)
	}
	cmd, err := sidecar.AdminCommandDigest(req.Method, req.URL.Path, body)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidAdminToken, err)
	}
	if cmd != claims.Cmd {
		return nil, fmt.Errorf("%w: the request is not the command its token names", ErrInvalidAdminToken)
	}
	return &session.AdminCommandUse{
		NetworkId:  claims.NetworkId,
		MemberId:   claims.MemberId,
		Jti:        claims.ID,
		Cmd:        claims.Cmd,
		Scopes:     ParseScope(claims.Scope),
		ExpireTime: claims.ExpiresAt.Time,
	}, nil
}

func init() {
	session.SetAdminTokenAuthenticator(authenticateAdminToken)
}
