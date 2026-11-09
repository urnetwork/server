package oauth

// ur-cli, the public client of `ur login` and `ur admin` (§7.8, D-TEAM-7):
// loopback PKCE (RFC 8252) with code-matched consent, a fresh sign-in and
// one-command DPoP tokens. It is preregistered here, in code, and never stored.
//
//	request   client_id=ur-cli, a loopback redirect, PKCE S256, resource=the
//	          api's origin, scope=auth_keys, dpop_jkt=<the CLI's DPoP key's
//	          thumbprint> (RFC 9449 §10), and authorization_details naming one
//	          command beside the consent code the terminal shows (RFC 9396;
//	          sidecar.AdminCommandDetails)
//	consent   the page asks /oauth/consent, which validates all of it and
//	          answers the command and the code to show side by side, and a
//	          receipt, an adm- signed JWT of this request for this user, valid
//	          10 minutes; consent is never remembered, so every command shows
//	          again; a sign-in older than 10 minutes is answered
//	          reauthentication_required, never a consent
//	authorize /oauth/authorize mints a code only with that receipt, so no
//	          authorization completes without a consent that showed its code
//	token     the code grant with the PKCE verifier and a DPoP proof by the
//	          dpop_jkt key, its ath over the code, mints the admin token
//	          (admin_token.go) with that cmd and cnf.jkt; ur-cli gets no refresh
//	          or id token
//
// v0's one command is POST /network/auth-keys for a single-use, user-owned key
// of at most 5 minutes (§5.6), which `ur login` hands the daemon. Full `ur admin`
// and the device grant are TEAM-M6-08's.
//
// The code carries the consented cmd and the dpop_jkt as two scope entries,
// admin_cmd:<cmd> and admin_jkt:<jkt>, beside auth_keys, so
// oauth_authorization_code needs no new column; neither reaches a token's scope.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

const (
	// ur-cli's client_id: preregistered, public, native
	UrcliClientId = "ur-cli"
	// the one scope an ur-cli token carries in v0 (§7.8's OAuth client scopes)
	UrcliScopeAuthKeys = "auth_keys"
	// the code's scope entries that carry the consented command and DPoP key
	urcliCmdScopePrefix = "admin_cmd:"
	urcliJktScopePrefix = "admin_jkt:"
	// the consent receipt's type and audience
	urcliConsentReceiptType     = "ur-consent+jwt"
	urcliConsentReceiptAudience = "/oauth/authorize"
)

// Tunables of ur-cli's grant.
type UrcliSettings struct {
	// How recent the signed-in session's login must be for the page to approve
	// a command: 10 minutes, so a browser an agent drives cannot approve on a
	// session the person opened days ago (§7.8's fresh auth_time).
	MaxAuthAge time.Duration
	// How long a consent receipt stands: 10 minutes, long enough to read the
	// command and compare the code.
	ConsentReceiptDuration time.Duration
	// An admin token's lifetime: an hour (§7.8). A longer one breaks §2.8
	// api-11's published 1 h.
	AdminTokenDuration time.Duration
	// The longest auth key v0's one command may create: 5 minutes (§5.6).
	MaxAuthKeyDuration time.Duration
}

func DefaultUrcliSettings() *UrcliSettings {
	return &UrcliSettings{
		MaxAuthAge:             10 * time.Minute,
		ConsentReceiptDuration: 10 * time.Minute,
		AdminTokenDuration:     time.Hour,
		MaxAuthKeyDuration:     5 * time.Minute,
	}
}

var (
	// an ur-cli request whose command v0 does not allow, or that lacks its
	// details, code or DPoP key
	ErrUrcliCommandRefused = fmt.Errorf("%w: the command is not one ur-cli may run", ErrInvalidRequest)
	// the signed-in session's login is older than MaxAuthAge
	ErrUrcliReauthenticationRequired = errors.New("reauthentication_required")
	// an authorization with no receipt of a consent that showed this request
	ErrUrcliConsentRequired = fmt.Errorf("%w: no consent showed this command and its code", ErrConsentRequired)
)

// ur-cli's registration: a native client with loopback redirects on any port
// (RFC 8252 §7.3).
func urcliClient() *Client {
	return &Client{
		ClientId:        UrcliClientId,
		ClientType:      ClientTypePreregistered,
		ClientName:      "ur (URnetwork command line)",
		ApplicationType: ApplicationTypeNative,
		RedirectUris: []string{
			"http://127.0.0.1/callback",
			"http://[::1]/callback",
		},
		Scopes: []string{UrcliScopeAuthKeys},
	}
}

// What an ur-cli request asks, once it validates.
type UrcliAuthorization struct {
	Details *sidecar.AdminCommandDetails
	// the command's digest, the token's cmd
	Cmd string
	// the DPoP key's thumbprint, the token's cnf.jkt
	Jkt string
}

// The checks an ur-cli request meets past ValidateAuthorizationRequest's: the
// api's resource, exactly the auth_keys scope, a dpop_jkt, and
// authorization_details naming one command v0 allows with a consent code.
func ValidateUrcliRequest(request *AuthorizationRequest) (*UrcliAuthorization, error) {
	if request.ClientId != UrcliClientId {
		return nil, fmt.Errorf("%w: not ur-cli", ErrInvalidRequest)
	}
	origin, err := session.DpopApiOrigin()
	if err != nil {
		return nil, fmt.Errorf("%w: the api has no public origin", ErrInvalidRequest)
	}
	resource, err := CanonicalResource(request.Resource)
	if err != nil || resource != origin {
		return nil, fmt.Errorf("%w: ur-cli's resource is the api", ErrInvalidRequest)
	}
	if len(request.Scopes) != 1 || request.Scopes[0] != UrcliScopeAuthKeys {
		return nil, fmt.Errorf("%w: ur-cli asks for %s alone", ErrUrcliCommandRefused, UrcliScopeAuthKeys)
	}
	if !urcliJktWellFormed(request.DpopJkt) {
		return nil, fmt.Errorf("%w: no dpop_jkt", ErrUrcliCommandRefused)
	}
	details, err := sidecar.ParseAdminCommandDetails(request.AuthorizationDetails)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUrcliCommandRefused, err)
	}
	command := details.Command()
	if err := validateUrcliCommand(command, DefaultUrcliSettings()); err != nil {
		return nil, err
	}
	cmd, err := command.Digest()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUrcliCommandRefused, err)
	}
	return &UrcliAuthorization{Details: details, Cmd: cmd, Jkt: request.DpopJkt}, nil
}

// An RFC 7638 thumbprint's form: 43 unpadded base64url characters.
func urcliJktWellFormed(jkt string) bool {
	thumbprint, err := base64.RawURLEncoding.DecodeString(jkt)
	return err == nil && len(thumbprint) == sha256.Size
}

// v0's one command (§5.6, §7.8): POST /network/auth-keys for a single-use,
// user-owned key of at most MaxAuthKeyDuration, a body of exactly
// {duration_seconds, max_uses: 1} and nothing else, so no tag, ephemeral or
// pre-approved key.
func validateUrcliCommand(command *sidecar.AdminCommand, settings *UrcliSettings) error {
	if command.Method != "POST" || command.Path != "/network/auth-keys" {
		return fmt.Errorf("%w: v0 runs POST /network/auth-keys alone", ErrUrcliCommandRefused)
	}
	type authKeyBody struct {
		DurationSeconds *int64 `json:"duration_seconds"`
		MaxUses         *int   `json:"max_uses"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(command.Body)))
	decoder.DisallowUnknownFields()
	body := &authKeyBody{}
	if err := decoder.Decode(body); err != nil {
		return fmt.Errorf("%w: the key's body", ErrUrcliCommandRefused)
	}
	if body.DurationSeconds == nil || body.MaxUses == nil {
		return fmt.Errorf("%w: the key's body names its duration and uses", ErrUrcliCommandRefused)
	}
	if *body.MaxUses != 1 {
		return fmt.Errorf("%w: the key is single-use", ErrUrcliCommandRefused)
	}
	if *body.DurationSeconds <= 0 || int64(settings.MaxAuthKeyDuration/time.Second) < *body.DurationSeconds {
		return fmt.Errorf("%w: the key lives at most %s", ErrUrcliCommandRefused, settings.MaxAuthKeyDuration)
	}
	return nil
}

// The digest a consent receipt binds: every parameter of the request, so a
// receipt for one command, code, key or redirect admits no other.
func urcliRequestDigest(request *AuthorizationRequest) string {
	canonical, _ := json.Marshal([]string{
		request.ClientId,
		request.RedirectUri,
		request.ResponseType,
		FormatScope(request.Scopes),
		request.State,
		request.CodeChallenge,
		request.CodeChallengeMethod,
		request.Resource,
		request.DpopJkt,
		request.AuthorizationDetails,
	})
	hash := sha256.Sum256(canonical)
	return base64.RawURLEncoding.EncodeToString(hash[:])
}

// A consent receipt's claims.
type urcliConsentReceiptClaims struct {
	// urcliRequestDigest of the request the consent described
	Request string `json:"req"`
	gojwt.RegisteredClaims
}

// A receipt that the consent page was given this request's command and code to
// show, for userId, signed by the adm- set with a type no admin token has.
func mintUrcliConsentReceipt(userId server.Id, request *AuthorizationRequest, now time.Time, settings *UrcliSettings) (string, error) {
	signingKey, err := SigningKeyOf(SignerKeySetAdmin)
	if err != nil {
		return "", err
	}
	claims := &urcliConsentReceiptClaims{
		Request: urcliRequestDigest(request),
		RegisteredClaims: gojwt.RegisteredClaims{
			Issuer:    Issuer(),
			Subject:   userId.String(),
			Audience:  gojwt.ClaimStrings{Issuer() + urcliConsentReceiptAudience},
			IssuedAt:  gojwt.NewNumericDate(now),
			ExpiresAt: gojwt.NewNumericDate(now.Add(settings.ConsentReceiptDuration)),
			ID:        server.NewId().String(),
		},
	}
	token := gojwt.NewWithClaims(gojwt.SigningMethodES256, claims)
	token.Header["kid"] = signingKey.Kid
	token.Header["typ"] = urcliConsentReceiptType
	return token.SignedString(signingKey.PrivateKey)
}

// Whether receipt is this issuer's receipt of request for userId, unexpired.
func verifyUrcliConsentReceipt(receipt string, userId server.Id, request *AuthorizationRequest, now func() time.Time) error {
	if receipt == "" {
		return ErrUrcliConsentRequired
	}
	claims := &urcliConsentReceiptClaims{}
	keyFunc := func(token *gojwt.Token) (any, error) {
		if token.Header["typ"] != urcliConsentReceiptType {
			return nil, errors.New("not a consent receipt's type")
		}
		kid, _ := token.Header["kid"].(string)
		key := verificationKeyOf(SignerKeySetAdmin, kid)
		if key == nil {
			return nil, errors.New("no admin key of that kid")
		}
		return &key.PrivateKey.PublicKey, nil
	}
	_, err := gojwt.ParseWithClaims(
		receipt,
		claims,
		keyFunc,
		gojwt.WithValidMethods([]string{signerAlgEs256}),
		gojwt.WithIssuer(Issuer()),
		gojwt.WithAudience(Issuer()+urcliConsentReceiptAudience),
		gojwt.WithSubject(userId.String()),
		gojwt.WithExpirationRequired(),
		gojwt.WithTimeFunc(now),
	)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrUrcliConsentRequired, err)
	}
	if claims.Request != urcliRequestDigest(request) {
		return fmt.Errorf("%w: the receipt is of another request", ErrUrcliConsentRequired)
	}
	return nil
}

// What the consent page shows for an ur-cli request: the one command and the
// code to match against the terminal, and the receipt it returns on approval.
type UrcliConsent struct {
	Method      string          `json:"method"`
	Path        string          `json:"path"`
	Body        json.RawMessage `json:"body,omitempty"`
	ConsentCode string          `json:"consent_code"`
	Receipt     string          `json:"consent_receipt"`
}

// Describes an ur-cli request to the consent page for user, signed in at
// authTime: the command and its code with a receipt, or
// ErrUrcliReauthenticationRequired for a sign-in past MaxAuthAge.
func DescribeUrcliConsent(request *AuthorizationRequest, userId server.Id, authTime time.Time, now time.Time) (*UrcliConsent, error) {
	settings := DefaultUrcliSettings()
	authorization, err := ValidateUrcliRequest(request)
	if err != nil {
		return nil, err
	}
	if authTime.IsZero() || settings.MaxAuthAge < now.Sub(authTime) {
		return nil, ErrUrcliReauthenticationRequired
	}
	receipt, err := mintUrcliConsentReceipt(userId, request, now, settings)
	if err != nil {
		return nil, err
	}
	return &UrcliConsent{
		Method:      authorization.Details.Method,
		Path:        authorization.Details.Path,
		Body:        authorization.Details.Body,
		ConsentCode: authorization.Details.ConsentCode,
		Receipt:     receipt,
	}, nil
}

// Mints ur-cli's authorization code once the page returns a receipt of this
// request's consent: a fresh sign-in, a full network, and no remembered
// consent, so every command shows again.
func authorizeUrcli(ctx context.Context, request *AuthorizationRequest, user *AuthorizingUser, receipt string) (string, error) {
	settings := DefaultUrcliSettings()
	client, err := ValidateAuthorizationRequest(ctx, request)
	if err != nil {
		return "", err
	}
	authorization, err := ValidateUrcliRequest(request)
	if err != nil {
		return "", err
	}
	if user.GuestMode {
		return "", errUnauthorizedGuestNetwork
	}
	if user.NetworkId == (server.Id{}) {
		return "", errUnauthorizedNoNetwork
	}
	now := server.NowUtc()
	if user.AuthTime.IsZero() || settings.MaxAuthAge < now.Sub(user.AuthTime) {
		return "", ErrUrcliReauthenticationRequired
	}
	if err := verifyUrcliConsentReceipt(receipt, user.UserId, request, server.NowUtc); err != nil {
		return "", err
	}
	resource, err := CanonicalResource(request.Resource)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrInvalidRequest, err)
	}
	return CreateAuthorizationCode(ctx, &AuthorizationCode{
		ClientId:            client.ClientId,
		UserId:              user.UserId,
		NetworkId:           user.NetworkId,
		RedirectUri:         request.RedirectUri,
		CodeChallenge:       request.CodeChallenge,
		CodeChallengeMethod: request.CodeChallengeMethod,
		Resource:            resource,
		Scopes: []string{
			UrcliScopeAuthKeys,
			urcliCmdScopePrefix + authorization.Cmd,
			urcliJktScopePrefix + authorization.Jkt,
		},
		Principal: user.Principal,
		Roles:     user.Roles,
		AuthTime:  user.AuthTime,
	})
}

// Exchanges ur-cli's code for its admin token: the code, PKCE verifier and
// redirect as every code grant checks them, then one DPoP proof by the key the
// authorization named, at the token endpoint, its ath over the code.
func ExchangeUrcliCode(ctx context.Context, args *ExchangeCodeArgs, proofs []string) (*TokenResponse, error) {
	if args.ClientId != UrcliClientId {
		return nil, fmt.Errorf("%w: not ur-cli", ErrInvalidRequest)
	}
	resource, err := CanonicalResource(args.Resource)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidRequest, err)
	}
	if len(proofs) != 1 {
		return nil, fmt.Errorf("%w: the request does not carry exactly one proof", ErrInvalidDpopProof)
	}
	code, err := RedeemAuthorizationCode(ctx, args.Code, args.ClientId, args.RedirectUri, args.CodeVerifier, resource)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidGrant, err)
	}
	cmd := ""
	jkt := ""
	scopes := []string{}
	for _, scope := range code.Scopes {
		switch {
		case strings.HasPrefix(scope, urcliCmdScopePrefix):
			cmd = strings.TrimPrefix(scope, urcliCmdScopePrefix)
		case strings.HasPrefix(scope, urcliJktScopePrefix):
			jkt = strings.TrimPrefix(scope, urcliJktScopePrefix)
		default:
			scopes = append(scopes, scope)
		}
	}
	if cmd == "" || jkt == "" || len(scopes) != 1 || scopes[0] != UrcliScopeAuthKeys {
		return nil, fmt.Errorf("%w: the code names no command", ErrInvalidGrant)
	}
	if err := session.VerifyDpopProof(ctx, &session.DpopRequest{
		Proof:         proofs[0],
		Method:        "POST",
		Uri:           TokenEndpoint(),
		Token:         args.Code,
		KeyThumbprint: jkt,
		ReplayId:      code.UserId,
	}); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidDpopProof, err)
	}
	settings := DefaultUrcliSettings()
	token, expiry, err := MintAdminToken(&MintAdminTokenArgs{
		NetworkId: code.NetworkId,
		MemberId:  code.UserId,
		Scopes:    scopes,
		Jkt:       jkt,
		Cmd:       cmd,
		Duration:  settings.AdminTokenDuration,
	})
	if err != nil {
		return nil, err
	}
	return &TokenResponse{
		AccessToken: token,
		TokenType:   adminTokenResponseType,
		ExpiresIn:   int(time.Until(expiry).Seconds()),
		Scope:       FormatScope(scopes),
	}, nil
}
