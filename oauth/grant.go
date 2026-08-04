package oauth

// The grant flows: minting an authorization code once the user has consented,
// and exchanging a code or a refresh token for tokens.
//
// These are the decision points, kept out of the http handlers so they can be
// tested without a request. The handlers translate to and from the wire; the
// rules live here.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/urnetwork/server"
)

var (
	ErrInvalidRequest   = errors.New("invalid_request")
	ErrInvalidGrant     = errors.New("invalid_grant")
	ErrUnauthorizedUser = errors.New("unauthorized")
	ErrConsentRequired  = errors.New("consent_required")
)

// What the authorization endpoint received from the client.
type AuthorizationRequest struct {
	ClientId            string
	RedirectUri         string
	ResponseType        string
	Scopes              []string
	State               string
	CodeChallenge       string
	CodeChallengeMethod string
	Resource            string
	Nonce               string
}

// Validates an authorization request before any user interaction, so a
// malformed or hostile request is refused before a consent screen is shown.
//
// A request that fails here must NOT be redirected back to the client:
// the redirect uri is only trustworthy once it has been matched against the
// registered set, so errors found here are shown to the user instead.
func ValidateAuthorizationRequest(
	ctx context.Context,
	request *AuthorizationRequest,
) (*Client, error) {
	if request.ClientId == "" {
		return nil, fmt.Errorf("%w: client_id is required", ErrInvalidRequest)
	}
	if request.ResponseType != "code" {
		return nil, fmt.Errorf("%w: only the code response_type is supported", ErrInvalidRequest)
	}
	// oauth 2.1 requires pkce for every client
	if request.CodeChallenge == "" {
		return nil, fmt.Errorf("%w: code_challenge is required", ErrInvalidRequest)
	}
	if request.CodeChallengeMethod != PkceMethodS256 {
		return nil, fmt.Errorf("%w: code_challenge_method must be S256", ErrInvalidRequest)
	}
	if request.Resource == "" {
		return nil, fmt.Errorf("%w: resource is required", ErrInvalidRequest)
	}
	if _, err := CanonicalResource(request.Resource); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidRequest, err)
	}

	client, err := GetClient(ctx, request.ClientId)
	if err != nil {
		return nil, err
	}

	if request.RedirectUri == "" {
		return nil, fmt.Errorf("%w: redirect_uri is required", ErrInvalidRequest)
	}
	if !client.ValidRedirectUri(request.RedirectUri) {
		return nil, ErrInvalidRedirect
	}

	return client, nil
}

// The authenticated user approving a request.
type AuthorizingUser struct {
	UserId      server.Id
	NetworkId   server.Id
	NetworkName string
	GuestMode   bool
	Principal   string
	Roles       []string
	AuthTime    time.Time
}

// Mints an authorization code for a consented request. The caller has already
// authenticated the user and shown consent when required.
func Authorize(
	ctx context.Context,
	request *AuthorizationRequest,
	user *AuthorizingUser,
) (string, error) {
	client, err := ValidateAuthorizationRequest(ctx, request)
	if err != nil {
		return "", err
	}

	// a guest network cannot be billed, and every scope that matters bills
	if user.GuestMode {
		return "", fmt.Errorf("%w: upgrade the guest network before authorizing", ErrUnauthorizedUser)
	}
	if user.NetworkId == (server.Id{}) {
		return "", fmt.Errorf("%w: no network", ErrUnauthorizedUser)
	}

	scopes := FilterSupportedScopes(request.Scopes)
	if len(scopes) == 0 {
		return "", fmt.Errorf("%w: no supported scope requested", ErrInvalidRequest)
	}

	resource, err := CanonicalResource(request.Resource)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrInvalidRequest, err)
	}

	// consent is recorded here rather than at the consent screen, so a screen
	// that is shown but abandoned grants nothing
	SaveConsent(ctx, &Consent{
		UserId:    user.UserId,
		ClientId:  client.ClientId,
		NetworkId: user.NetworkId,
		Scopes:    scopes,
	})

	authTime := user.AuthTime
	if authTime.IsZero() {
		authTime = server.NowUtc()
	}

	return CreateAuthorizationCode(ctx, &AuthorizationCode{
		ClientId:            client.ClientId,
		UserId:              user.UserId,
		NetworkId:           user.NetworkId,
		RedirectUri:         request.RedirectUri,
		CodeChallenge:       request.CodeChallenge,
		CodeChallengeMethod: request.CodeChallengeMethod,
		Resource:            resource,
		Scopes:              scopes,
		Nonce:               request.Nonce,
		Principal:           user.Principal,
		Roles:               user.Roles,
		AuthTime:            authTime,
	})
}

// The token endpoint response.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
	IdToken      string `json:"id_token,omitempty"`
	Scope        string `json:"scope,omitempty"`
}

type ExchangeCodeArgs struct {
	Code         string
	ClientId     string
	RedirectUri  string
	CodeVerifier string
	Resource     string
}

// Exchanges an authorization code for tokens.
func ExchangeCode(ctx context.Context, args *ExchangeCodeArgs) (*TokenResponse, error) {
	resource, err := CanonicalResource(args.Resource)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidRequest, err)
	}

	code, err := RedeemAuthorizationCode(
		ctx,
		args.Code,
		args.ClientId,
		args.RedirectUri,
		args.CodeVerifier,
		resource,
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidGrant, err)
	}

	return issueTokens(ctx, &issueTokensArgs{
		ClientId:   code.ClientId,
		UserId:     code.UserId,
		NetworkId:  code.NetworkId,
		Resource:   code.Resource,
		Scopes:     code.Scopes,
		Principal:  code.Principal,
		Roles:      code.Roles,
		AuthTime:   code.AuthTime,
		Nonce:      code.Nonce,
		NewRefresh: true,
	})
}

type RefreshArgs struct {
	RefreshToken string
	ClientId     string
	// optional narrowing, per rfc 6749: a refresh may request a subset of the
	// originally granted scope, never a superset
	Scopes []string
}

// Exchanges a refresh token for tokens, rotating the refresh token.
func Refresh(ctx context.Context, args *RefreshArgs) (*TokenResponse, error) {
	// the narrowing is applied and validated inside the rotation, so a
	// rejected scope does not consume the token
	grant, nextRefreshToken, err := RotateRefreshToken(
		ctx,
		args.RefreshToken,
		args.ClientId,
		args.Scopes,
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidGrant, err)
	}

	response, err := issueTokens(ctx, &issueTokensArgs{
		ClientId:  grant.ClientId,
		UserId:    grant.UserId,
		NetworkId: grant.NetworkId,
		Resource:  grant.Resource,
		Scopes:    grant.Scopes,
		Principal: grant.Principal,
		Roles:     grant.Roles,
		AuthTime:  grant.AuthTime,
	})
	if err != nil {
		return nil, err
	}

	response.RefreshToken = nextRefreshToken
	return response, nil
}

type issueTokensArgs struct {
	ClientId   string
	UserId     server.Id
	NetworkId  server.Id
	Resource   string
	Scopes     []string
	Principal  string
	Roles      []string
	AuthTime   time.Time
	Nonce      string
	NewRefresh bool
}

func issueTokens(ctx context.Context, args *issueTokensArgs) (*TokenResponse, error) {
	accessToken, expiry, err := MintAccessToken(&MintAccessTokenArgs{
		UserId:    args.UserId,
		NetworkId: args.NetworkId,
		ClientId:  args.ClientId,
		Audience:  args.Resource,
		Scopes:    args.Scopes,
		Principal: args.Principal,
		Roles:     args.Roles,
	})
	if err != nil {
		return nil, err
	}

	response := &TokenResponse{
		AccessToken: accessToken,
		TokenType:   "Bearer",
		ExpiresIn:   int(time.Until(expiry).Seconds()),
		Scope:       FormatScope(args.Scopes),
	}

	if HasScope(args.Scopes, ScopeOpenid) {
		networkName := networkNameForNetwork(ctx, args.NetworkId)
		idToken, err := MintIdToken(&MintIdTokenArgs{
			UserId:      args.UserId,
			NetworkId:   args.NetworkId,
			NetworkName: networkName,
			ClientId:    args.ClientId,
			Nonce:       args.Nonce,
			AuthTime:    args.AuthTime,
			AccessToken: accessToken,
		})
		if err != nil {
			return nil, err
		}
		response.IdToken = idToken
	}

	if args.NewRefresh && HasScope(args.Scopes, ScopeOfflineAccess) {
		refreshToken, err := CreateRefreshToken(ctx, &RefreshToken{
			ClientId:  args.ClientId,
			UserId:    args.UserId,
			NetworkId: args.NetworkId,
			Resource:  args.Resource,
			Scopes:    args.Scopes,
			Principal: args.Principal,
			Roles:     args.Roles,
			AuthTime:  args.AuthTime,
		})
		if err != nil {
			return nil, err
		}
		response.RefreshToken = refreshToken
	}

	return response, nil
}

// The openid connect userinfo claims.
type UserInfo struct {
	Sub         string `json:"sub"`
	NetworkId   string `json:"network_id,omitempty"`
	NetworkName string `json:"network_name,omitempty"`
	Principal   string `json:"principal,omitempty"`
}

func networkNameForNetwork(ctx context.Context, networkId server.Id) string {
	networkName := ""

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT network_name
			FROM network
			WHERE network_id = $1
			`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&networkName))
			}
		})
	})

	return networkName
}
