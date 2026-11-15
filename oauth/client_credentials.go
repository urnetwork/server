package oauth

// §7.8's client_credentials grant for a team's network OAuth clients (TEAM-M6-08; R-TEAM-6, D-TEAM-6): CI exchanges
// its ur-oauth-<id> and ur_sk_<secret> (model/network_oauth_client_model.go) for an access token of an hour that acts
// as the client's creating member within the client's scopes, which the role gate cuts to that member's role on every
// request (api/route_permission.go: scopes ∩ creator).
//
//	request  POST /oauth/token grant_type=client_credentials, the client's id and secret in the form
//	         (client_secret_post) or HTTP Basic (client_secret_basic), an optional scope within the client's, and an
//	         optional resource, which is the api's origin
//	token    an ES256 JWT under the adm- key set with a type no other token has, typ ur-client+jwt:
//	         {iss, sub = the client_id, aud "api", network_id, member_id = the creator, oauth_client_id, scope, jti,
//	         iat, exp <= 1 h}; never stored (§2.8 api-11)
//	api      takes it as "Bearer <token>" on the routes session.RegisterOauthClientRoute names alone, and checks at
//	         each request that the client still stands and its creator is still a member, so a revocation or a
//	         removal stops its live tokens at once
//
// The server cannot admit (D-TEAM-6): the token creates auth keys of at most an hour, and CI's Delegation, which only
// a person's device signs, admits its sidecars.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

const (
	// RFC 6749 §4.4's grant type
	ClientCredentialsGrantType = "client_credentials"
	// the access token's type, which keeps it apart from an admin token, an id token or a consent receipt of the
	// same key set
	oauthClientTokenType = "ur-client+jwt"
)

// Tunables of the client_credentials grant.
type ClientCredentialsSettings struct {
	// An access token's lifetime: an hour, an admin token's (§7.8), past which CI asks again.
	TokenDuration time.Duration
}

// The defaults, a fresh value each call.
func DefaultClientCredentialsSettings() *ClientCredentialsSettings {
	return &ClientCredentialsSettings{
		TokenDuration: time.Hour,
	}
}

var (
	// The client's id and secret authenticate no client: unknown, revoked or another secret, told apart by nothing.
	ErrInvalidClient = errors.New("invalid_client")
	// A token that is no network OAuth client's, or whose client or creator no longer stands.
	ErrInvalidOauthClientToken = errors.New("invalid OAuth client token")
)

// The claims of a network OAuth client's access token.
type OauthClientTokenClaims struct {
	// space delimited
	Scope         string    `json:"scope"`
	NetworkId     server.Id `json:"network_id"`
	MemberId      server.Id `json:"member_id"`
	OauthClientId server.Id `json:"oauth_client_id"`

	gojwt.RegisteredClaims
}

// The team tier check the grant makes, which a test replaces: QUOTA-M6-01's tier
// is not in transfer_balance yet.
var clientCredentialsRequireTier = model.RequireTeamTier

// What the grant was asked.
type ClientCredentialsArgs struct {
	ClientId     string
	ClientSecret string
	// the scopes asked, within the client's; empty for all of them
	Scopes []string
	// the api's origin, or empty
	Resource string
}

// Exchanges a network OAuth client's credentials for its access token (the file header).
func ExchangeClientCredentials(ctx context.Context, args *ClientCredentialsArgs) (*TokenResponse, error) {
	return exchangeClientCredentialsAt(ctx, args, DefaultClientCredentialsSettings(), server.NowUtc())
}

func exchangeClientCredentialsAt(ctx context.Context, args *ClientCredentialsArgs, settings *ClientCredentialsSettings, now time.Time) (*TokenResponse, error) {
	if args.Resource != "" {
		origin, err := session.DpopApiOrigin()
		resource, resourceErr := CanonicalResource(args.Resource)
		if err != nil || resourceErr != nil || resource != origin {
			return nil, fmt.Errorf("%w: a client's resource is the api", ErrInvalidRequest)
		}
	}
	client := model.AuthenticateNetworkOauthClient(ctx, args.ClientId, args.ClientSecret)
	if client == nil {
		return nil, ErrInvalidClient
	}
	creator := model.GetNetworkMember(ctx, client.CreatorMemberId)
	if creator == nil || creator.Pending || creator.NetworkId != client.NetworkId {
		return nil, fmt.Errorf("%w: the client's creator is no member of its network", ErrInvalidGrant)
	}
	if err := clientCredentialsRequireTier(ctx, client.NetworkId); err != nil {
		return nil, fmt.Errorf("%w: the network is outside the team tier", ErrInvalidGrant)
	}
	scopes := client.Scopes
	if 0 < len(args.Scopes) {
		for _, scope := range args.Scopes {
			if !slices.Contains(client.Scopes, scope) {
				return nil, fmt.Errorf("%w: a scope the client does not carry", ErrInvalidScope)
			}
		}
		scopes = args.Scopes
	}
	token, expiry, err := mintOauthClientToken(client, scopes, settings, now)
	if err != nil {
		return nil, err
	}
	return &TokenResponse{
		AccessToken: token,
		TokenType:   "Bearer",
		ExpiresIn:   int(expiry.Sub(now).Seconds()),
		Scope:       FormatScope(scopes),
	}, nil
}

// Mints a client's access token under the adm- set's newest key.
func mintOauthClientToken(client *model.NetworkOauthClient, scopes []string, settings *ClientCredentialsSettings, now time.Time) (string, time.Time, error) {
	signingKey, err := SigningKeyOf(SignerKeySetAdmin)
	if err != nil {
		return "", time.Time{}, err
	}
	if settings.TokenDuration <= 0 || DefaultClientCredentialsSettings().TokenDuration < settings.TokenDuration {
		return "", time.Time{}, fmt.Errorf("OAuth client token: a lifetime of %s", settings.TokenDuration)
	}
	expiry := now.Add(settings.TokenDuration)
	claims := &OauthClientTokenClaims{
		Scope:         FormatScope(scopes),
		NetworkId:     client.NetworkId,
		MemberId:      client.CreatorMemberId,
		OauthClientId: client.OauthClientId,
		RegisteredClaims: gojwt.RegisteredClaims{
			Issuer:    Issuer(),
			Subject:   model.NetworkOauthClientIdText(client.OauthClientId),
			Audience:  gojwt.ClaimStrings{AdminTokenAudience},
			IssuedAt:  gojwt.NewNumericDate(now),
			NotBefore: gojwt.NewNumericDate(now),
			ExpiresAt: gojwt.NewNumericDate(expiry),
			ID:        server.NewId().String(),
		},
	}
	token := gojwt.NewWithClaims(gojwt.SigningMethodES256, claims)
	token.Header["kid"] = signingKey.Kid
	token.Header["typ"] = oauthClientTokenType
	signed, err := token.SignedString(signingKey.PrivateKey)
	if err != nil {
		return "", time.Time{}, err
	}
	return signed, expiry, nil
}

// Whether token's JOSE header names a network OAuth client's access token, read without verifying anything: what
// lets the session route a bearer to this check rather than a by_jwt's.
func IsOauthClientTokenType(token string) bool {
	header, _, found := strings.Cut(token, ".")
	if !found || 1024 < len(header) {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(header)
	if err != nil {
		return false
	}
	fields := &struct {
		Typ string `json:"typ"`
	}{}
	return json.Unmarshal(decoded, fields) == nil && fields.Typ == oauthClientTokenType
}

// The claims of token, a network OAuth client's access token of this issuer: alg ES256, its type, an adm- kid the
// issuer lists, iss, the api audience, iat and exp present and exp not passed, a lifetime of at most the grant's, and
// every claim set. A failure wraps ErrInvalidOauthClientToken.
func VerifyOauthClientToken(token string) (*OauthClientTokenClaims, error) {
	return verifyOauthClientTokenAt(token, server.NowUtc)
}

func verifyOauthClientTokenAt(token string, now func() time.Time) (*OauthClientTokenClaims, error) {
	claims := &OauthClientTokenClaims{}
	keyFunc := func(token *gojwt.Token) (any, error) {
		if token.Header["typ"] != oauthClientTokenType {
			return nil, errors.New("not an OAuth client token's type")
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
		return nil, fmt.Errorf("%w: %s", ErrInvalidOauthClientToken, err)
	}
	if claims.IssuedAt == nil || claims.ExpiresAt == nil || DefaultClientCredentialsSettings().TokenDuration < claims.ExpiresAt.Sub(claims.IssuedAt.Time) {
		return nil, fmt.Errorf("%w: no iat or exp, or a lifetime past the grant's", ErrInvalidOauthClientToken)
	}
	if claims.NetworkId == (server.Id{}) || claims.MemberId == (server.Id{}) || claims.OauthClientId == (server.Id{}) || len(ParseScope(claims.Scope)) == 0 {
		return nil, fmt.Errorf("%w: a claim is missing", ErrInvalidOauthClientToken)
	}
	return claims, nil
}

// The session's check of a network OAuth client's token: the token, then the client still standing in its network
// with its creator, and its scopes still the client's.
func authenticateOauthClientToken(ctx context.Context, token string) (*session.OauthClientUse, error) {
	claims, err := VerifyOauthClientToken(token)
	if err != nil {
		return nil, err
	}
	client := model.GetNetworkOauthClient(ctx, claims.OauthClientId)
	if client == nil || client.NetworkId != claims.NetworkId || client.CreatorMemberId != claims.MemberId {
		return nil, fmt.Errorf("%w: the client is revoked", ErrInvalidOauthClientToken)
	}
	scopes := ParseScope(claims.Scope)
	for _, scope := range scopes {
		if !slices.Contains(client.Scopes, scope) {
			return nil, fmt.Errorf("%w: a scope the client does not carry", ErrInvalidOauthClientToken)
		}
	}
	return &session.OauthClientUse{
		NetworkId:     claims.NetworkId,
		MemberId:      claims.MemberId,
		OauthClientId: claims.OauthClientId,
		Scopes:        scopes,
		ExpireTime:    claims.ExpiresAt.Time,
	}, nil
}

func init() {
	session.SetOauthClientTokenAuthenticator(IsOauthClientTokenType, authenticateOauthClientToken)
}

// A client's access token with scopes, minted now, for a test that holds no client secret.
func Testing_MintOauthClientToken(client *model.NetworkOauthClient, scopes []string) (string, error) {
	token, _, err := mintOauthClientToken(client, scopes, DefaultClientCredentialsSettings(), server.NowUtc())
	return token, err
}
