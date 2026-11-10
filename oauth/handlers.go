package oauth

// The http surface of the authorization server.
//
// These are plain handlers rather than the api's Wrap helpers: oauth is
// form-encoded, mostly unauthenticated, and has its own error envelope, none of
// which the json-in/json-out wrappers model.
//
// The authorization endpoint itself is NOT here -- it is the consent page on
// the ur.io origin, because that is where the logged in session lives (IDP.md
// §1). ur.io calls `AuthorizeHandler` to mint the code once the user approves.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// The authorization server routes. Served on the issuer host.
func Routes() []*router.Route {
	return []*router.Route{
		router.NewRoute("GET", "/\\.well-known/oauth-authorization-server", ServerMetadataHandler),
		router.NewRoute("GET", "/\\.well-known/openid-configuration", ServerMetadataHandler),
		router.NewRoute("GET", "/\\.well-known/jwks\\.json", JwksHandler),
		router.NewRoute("POST", "/oauth/token", TokenHandler),
		router.NewRoute("POST", "/oauth/register", RegisterHandler),
		router.NewRoute("POST", "/oauth/revoke", RevokeHandler),
		router.NewRoute("GET", "/oauth/userinfo", UserinfoHandler),
		// called by the consent page on the ur.io origin
		router.NewRoute("POST", "/oauth/authorize", AuthorizeHandler),
		router.NewRoute("POST", "/oauth/consent", ConsentHandler),
		// §7.8's edge login: a front sends its visitor here, on to the consent
		// page; and the id- set's keys its ID tokens verify by (edge_login.go)
		router.NewRoute("GET", "/oauth/authorize", AuthorizeRedirectHandler),
		router.NewRoute("GET", "/\\.well-known/ur/id-jwks\\.json", EdgeJwksHandler),
	}
}

// rfc 6749 section 5.2 error envelope.
type oauthError struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description,omitempty"`
}

func writeJson(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	// discovery documents and token responses must never be cached by a shared
	// cache; the token response carries credentials
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}

func writeOauthError(w http.ResponseWriter, status int, code string, description string) {
	writeJson(w, status, &oauthError{
		Error:            code,
		ErrorDescription: description,
	})
}

// Maps a grant error to its rfc 6749 code. Descriptions are deliberately terse:
// the detail is logged, not returned, so an error cannot be used to probe
// which of several checks failed.
func writeGrantError(w http.ResponseWriter, err error) {
	glog.Infof("[oauth]grant error = %s\n", err)

	switch {
	case errors.Is(err, ErrInvalidRequest):
		writeOauthError(w, http.StatusBadRequest, "invalid_request", "The request is malformed.")
	case errors.Is(err, ErrInvalidRedirect):
		writeOauthError(w, http.StatusBadRequest, "invalid_request", "The redirect_uri is not registered for this client.")
	case errors.Is(err, ErrUnknownClient), errors.Is(err, ErrInvalidClientMeta):
		writeOauthError(w, http.StatusBadRequest, "invalid_client", "The client could not be resolved.")
	case errors.Is(err, ErrPkceFailed), errors.Is(err, ErrInvalidCode),
		errors.Is(err, ErrInvalidRefreshToken), errors.Is(err, ErrInvalidGrant):
		writeOauthError(w, http.StatusBadRequest, "invalid_grant", "The grant is invalid or expired.")
	case errors.Is(err, ErrUrcliReauthenticationRequired):
		writeOauthError(w, http.StatusUnauthorized, "login_required", "Sign in again to approve this command.")
	case errors.Is(err, ErrConsentRequired):
		writeOauthError(w, http.StatusForbidden, "consent_required", "No consent showed this command and its code.")
	case errors.Is(err, ErrInvalidDpopProof):
		writeOauthError(w, http.StatusBadRequest, "invalid_dpop_proof", "The DPoP proof is invalid.")
	case errors.Is(err, ErrUnauthorizedUser):
		// a declared reason tells the user what to do; any other detail is logged
		description := ErrUnauthorizedUser.Error()
		for _, reason := range []error{errUnauthorizedGuestNetwork, errUnauthorizedNoNetwork} {
			if errors.Is(err, reason) {
				description = reason.Error()
			}
		}
		writeOauthError(w, http.StatusForbidden, "access_denied", description)
	default:
		writeOauthError(w, http.StatusInternalServerError, "server_error", "Please try again.")
	}
}

// The authorization server metadata, which doubles as the openid connect
// discovery document.
func ServerMetadataHandler(w http.ResponseWriter, r *http.Request) {
	writeJson(w, http.StatusOK, ServerMetadata())
}

func JwksHandler(w http.ResponseWriter, r *http.Request) {
	writeJson(w, http.StatusOK, Jwks())
}

// Exchanges a code or refresh token for tokens, or a sidecar's client token for
// a service token. The token exchange names no client_id, so the grant type is
// read first.
func TokenHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeOauthError(w, http.StatusBadRequest, "invalid_request", "The form could not be parsed.")
		return
	}

	if r.PostFormValue("grant_type") == ExchangeGrantType {
		ExchangeHandler(w, r)
		return
	}

	clientId := r.PostFormValue("client_id")
	if clientId == "" {
		writeOauthError(w, http.StatusBadRequest, "invalid_client", "client_id is required.")
		return
	}

	switch r.PostFormValue("grant_type") {
	case "authorization_code":
		exchangeCodeArgs := &ExchangeCodeArgs{
			Code:         r.PostFormValue("code"),
			ClientId:     clientId,
			RedirectUri:  r.PostFormValue("redirect_uri"),
			CodeVerifier: r.PostFormValue("code_verifier"),
			Resource:     r.PostFormValue("resource"),
		}
		// an edge origin's code redeems for its ID token alone (edge_login.go)
		if _, isOrigin := edgeOriginHost(clientId); isOrigin {
			if _, isEdge, err := resolveEdgeOrigin(r.Context(), clientId); isEdge || err != nil {
				var edgeResponse *EdgeTokenResponse
				if err == nil {
					edgeResponse, err = ExchangeEdgeCode(r.Context(), exchangeCodeArgs)
				}
				if err != nil {
					writeGrantError(w, err)
					return
				}
				writeJson(w, http.StatusOK, edgeResponse)
				return
			}
		}
		var response *TokenResponse
		var err error
		if clientId == UrcliClientId {
			// ur-cli's admin token, bound to the DPoP key its request named
			response, err = ExchangeUrcliCode(r.Context(), exchangeCodeArgs, r.Header.Values(session.DpopHeader))
		} else {
			response, err = ExchangeCode(r.Context(), exchangeCodeArgs)
		}
		if err != nil {
			writeGrantError(w, err)
			return
		}
		writeJson(w, http.StatusOK, response)

	case "refresh_token":
		response, err := Refresh(r.Context(), &RefreshArgs{
			RefreshToken: r.PostFormValue("refresh_token"),
			ClientId:     clientId,
			Scopes:       ParseScope(r.PostFormValue("scope")),
		})
		if err != nil {
			writeGrantError(w, err)
			return
		}
		writeJson(w, http.StatusOK, response)

	default:
		writeOauthError(w, http.StatusBadRequest, "unsupported_grant_type", "Supported: authorization_code, refresh_token, "+ExchangeGrantType+".")
	}
}

// The token exchange (exchange.go), from a parsed form and the request's DPoP
// headers.
func ExchangeHandler(w http.ResponseWriter, r *http.Request) {
	result, err := Exchange(r.Context(), &ExchangeArgs{
		SubjectToken:     r.PostFormValue("subject_token"),
		SubjectTokenType: r.PostFormValue("subject_token_type"),
		Resources:        r.PostForm["resource"],
		Scope:            r.PostFormValue("scope"),
		Proofs:           r.Header.Values(session.DpopHeader),
	})
	if err != nil {
		writeExchangeError(w, err)
		return
	}
	writeJson(w, http.StatusOK, result)
}

// Maps an exchange error to its code. Descriptions are terse and fixed: the
// detail goes to the service log, never a token, as the errors name none.
func writeExchangeError(w http.ResponseWriter, err error) {
	exchangeLogf("[oauth][exchange]refused: %s\n", err)

	switch {
	case errors.Is(err, ErrInvalidRequest):
		writeOauthError(w, http.StatusBadRequest, "invalid_request", "The request is malformed.")
	case errors.Is(err, ErrInvalidTarget):
		writeOauthError(w, http.StatusBadRequest, "invalid_target", "The resource is not a service this server issues tokens for.")
	case errors.Is(err, ErrInvalidScope):
		writeOauthError(w, http.StatusBadRequest, "invalid_scope", "The scope is not granted.")
	case errors.Is(err, ErrInvalidDpopProof):
		writeOauthError(w, http.StatusBadRequest, "invalid_dpop_proof", "The DPoP proof is invalid.")
	case errors.Is(err, ErrInvalidGrant):
		writeOauthError(w, http.StatusBadRequest, "invalid_grant", "The grant is invalid or expired.")
	case errors.Is(err, ErrRestoreRequired):
		writeOauthError(w, http.StatusBadRequest, "restore_required", "The network is being restored. Try again later.")
	default:
		writeOauthError(w, http.StatusInternalServerError, "server_error", "Please try again.")
	}
}

// Dynamic client registration (rfc 7591). Deprecated by the mcp spec in favor
// of client id metadata documents, and supported for clients that predate it.
func RegisterHandler(w http.ResponseWriter, r *http.Request) {
	// this endpoint is openly writable, so it is metered per caller ip before
	// anything is parsed or stored
	clientSession, err := session.NewClientSessionFromRequest(r)
	if err != nil {
		writeOauthError(w, http.StatusInternalServerError, "server_error", "Please try again.")
		return
	}
	defer clientSession.Cancel()

	if !AllowRegistration(clientSession.Ctx, clientSession.ClientAddress) {
		w.Header().Set("Retry-After", registrationRetryAfterSeconds())
		writeOauthError(w, http.StatusTooManyRequests, "temporarily_unavailable", "Too many registrations. Try again later.")
		return
	}

	var document clientMetadataDocument
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, cimdMaxBodyBytes)).Decode(&document); err != nil {
		writeOauthError(w, http.StatusBadRequest, "invalid_client_metadata", "The metadata could not be parsed.")
		return
	}

	client, err := RegisterClient(clientSession.Ctx, &document)
	if err != nil {
		glog.Infof("[oauth]register error = %s\n", err)
		writeOauthError(w, http.StatusBadRequest, "invalid_client_metadata", "The metadata was rejected.")
		return
	}

	writeJson(w, http.StatusCreated, map[string]any{
		"client_id":        client.ClientId,
		"client_name":      client.ClientName,
		"application_type": client.ApplicationType,
		"redirect_uris":    client.RedirectUris,
		"scope":            FormatScope(client.Scopes),
		// public clients only: there is no secret to return
		"token_endpoint_auth_method": "none",
	})
}

// Token revocation (rfc 7009). Always answers 200: per the rfc, a client must
// not be able to distinguish an unknown token from a revoked one.
func RevokeHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeOauthError(w, http.StatusBadRequest, "invalid_request", "The form could not be parsed.")
		return
	}

	if token := r.PostFormValue("token"); token != "" {
		RevokeRefreshToken(r.Context(), token)
	}

	w.WriteHeader(http.StatusOK)
}

// The openid connect userinfo endpoint. Authenticated by an access token
// carrying `openid`, whose audience is this issuer.
func UserinfoHandler(w http.ResponseWriter, r *http.Request) {
	accessToken := bearerToken(r)
	if accessToken == "" {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		writeOauthError(w, http.StatusUnauthorized, "invalid_token", "A bearer token is required.")
		return
	}

	// the caller presents the access token it already holds, whose audience is
	// the resource it was minted for rather than this issuer, so the audience
	// is deliberately not constrained here (see VerifyAccessTokenAnyResource)
	claims, err := VerifyAccessTokenAnyResource(accessToken)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		writeOauthError(w, http.StatusUnauthorized, "invalid_token", "The token is invalid or expired.")
		return
	}
	if !HasScope(ParseScope(claims.Scope), ScopeOpenid) {
		w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope", scope="openid"`)
		writeOauthError(w, http.StatusForbidden, "insufficient_scope", "The openid scope is required.")
		return
	}

	writeJson(w, http.StatusOK, &UserInfo{
		Sub:         claims.Subject,
		NetworkId:   claims.NetworkId.String(),
		NetworkName: networkNameForNetwork(r.Context(), claims.NetworkId),
		Principal:   claims.Principal,
	})
}

// What the consent page needs to render: who is asking, and for what.
type ConsentRequest struct {
	ClientId            string `json:"client_id"`
	RedirectUri         string `json:"redirect_uri"`
	ResponseType        string `json:"response_type"`
	Scope               string `json:"scope"`
	State               string `json:"state"`
	CodeChallenge       string `json:"code_challenge"`
	CodeChallengeMethod string `json:"code_challenge_method"`
	Resource            string `json:"resource"`
	Nonce               string `json:"nonce,omitempty"`
	Prompt              string `json:"prompt,omitempty"`
	// ur-cli's alone (urcli.go): its DPoP key's thumbprint, its one command
	// with the consent code, and on approval the receipt /oauth/consent gave
	DpopJkt              string `json:"dpop_jkt,omitempty"`
	AuthorizationDetails string `json:"authorization_details,omitempty"`
	ConsentReceipt       string `json:"consent_receipt,omitempty"`
}

type ConsentResult struct {
	ClientName  string   `json:"client_name,omitempty"`
	ClientUri   string   `json:"client_uri,omitempty"`
	LogoUri     string   `json:"logo_uri,omitempty"`
	Scopes      []string `json:"scopes"`
	NetworkName string   `json:"network_name,omitempty"`
	// the consent page echoes this as `iss` when the user declines: rfc 9207
	// requires the parameter on error responses too, and the page has no other
	// trustworthy source for it
	Issuer          string `json:"issuer"`
	ConsentRequired bool   `json:"consent_required"`
	Error           string `json:"error,omitempty"`
	// ur-cli's one command and the code to show beside it, with the receipt the
	// page returns on approval
	AdminCommand *UrcliConsent `json:"admin_command,omitempty"`
	// ur-cli's request on a sign-in older than UrcliSettings.MaxAuthAge: the
	// page signs the person in again before it shows the command
	ReauthenticationRequired bool `json:"reauthentication_required,omitempty"`
}

// Describes an authorization request to the consent page.
//
// The request is validated here, before anything is rendered, so a hostile
// request never reaches a screen that could be used to phish approval. An
// invalid request is reported to the USER rather than redirected: a redirect
// uri is only trustworthy once it has matched the registered set.
func ConsentHandler(w http.ResponseWriter, r *http.Request) {
	clientSession, err := session.NewClientSessionFromRequest(r)
	if err != nil {
		writeOauthError(w, http.StatusInternalServerError, "server_error", "Please try again.")
		return
	}
	defer clientSession.Cancel()

	if err := clientSession.Auth(r); err != nil {
		writeOauthError(w, http.StatusUnauthorized, "login_required", "Sign in to continue.")
		return
	}

	var consentRequest ConsentRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&consentRequest); err != nil {
		writeOauthError(w, http.StatusBadRequest, "invalid_request", "The request could not be parsed.")
		return
	}

	authorizationRequest := consentRequest.authorizationRequest()

	client, err := ValidateAuthorizationRequest(clientSession.Ctx, authorizationRequest)
	if err != nil {
		writeGrantError(w, err)
		return
	}

	if client.ClientId == UrcliClientId {
		// never remembered: every command shows, with its code, on a fresh
		// sign-in
		result := &ConsentResult{
			ClientName:      client.ClientName,
			Scopes:          authorizationRequest.Scopes,
			NetworkName:     clientSession.ByJwt.NetworkName,
			Issuer:          Issuer(),
			ConsentRequired: true,
		}
		adminCommand, err := DescribeUrcliConsent(authorizationRequest, clientSession.ByJwt.UserId, clientSession.ByJwt.CreateTime, server.NowUtc())
		switch {
		case errors.Is(err, ErrUrcliReauthenticationRequired):
			result.ReauthenticationRequired = true
		case err != nil:
			writeGrantError(w, err)
			return
		default:
			result.AdminCommand = adminCommand
		}
		writeJson(w, http.StatusOK, result)
		return
	}

	if client.ClientType == ClientTypeEdge {
		// a network's own published name signs in its members: nothing to
		// consent to, only a sign-in (edge_login.go)
		writeJson(w, http.StatusOK, &ConsentResult{
			ClientName:      client.ClientName,
			ClientUri:       client.ClientUri,
			Scopes:          []string{EdgeScopeOpenid},
			NetworkName:     clientSession.ByJwt.NetworkName,
			Issuer:          Issuer(),
			ConsentRequired: false,
		})
		return
	}

	scopes := FilterSupportedScopes(authorizationRequest.Scopes)

	// `prompt=consent` forces the screen even when the scopes are already
	// approved, per openid connect
	consentRequired := consentRequest.Prompt == "consent" ||
		!ConsentSatisfies(clientSession.Ctx, clientSession.ByJwt.UserId, client.ClientId, scopes)

	writeJson(w, http.StatusOK, &ConsentResult{
		ClientName:      client.ClientName,
		ClientUri:       client.ClientUri,
		LogoUri:         client.LogoUri,
		Scopes:          scopes,
		NetworkName:     clientSession.ByJwt.NetworkName,
		Issuer:          Issuer(),
		ConsentRequired: consentRequired,
	})
}

type AuthorizeResult struct {
	// where the consent page should send the user agent
	RedirectUri string `json:"redirect_uri"`
}

// Mints an authorization code for an approved request and returns the redirect
// the consent page should follow.
//
// The caller must be an authenticated user session -- the ur.io consent page
// passes the signed-in user's jwt through. Consent is recorded here rather than
// when the screen is shown, so an abandoned screen grants nothing.
func AuthorizeHandler(w http.ResponseWriter, r *http.Request) {
	clientSession, err := session.NewClientSessionFromRequest(r)
	if err != nil {
		writeOauthError(w, http.StatusInternalServerError, "server_error", "Please try again.")
		return
	}
	defer clientSession.Cancel()

	if err := clientSession.Auth(r); err != nil {
		writeOauthError(w, http.StatusUnauthorized, "login_required", "Sign in to continue.")
		return
	}

	var consentRequest ConsentRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&consentRequest); err != nil {
		writeOauthError(w, http.StatusBadRequest, "invalid_request", "The request could not be parsed.")
		return
	}

	authorizationRequest := consentRequest.authorizationRequest()

	byJwt := clientSession.ByJwt
	var code string
	if authorizationRequest.ClientId == UrcliClientId {
		// ur-cli: the sign-in's own time, which must be fresh, and the receipt of
		// the consent that showed this command and its code
		code, err = authorizeUrcli(clientSession.Ctx, authorizationRequest, &AuthorizingUser{
			UserId:      byJwt.UserId,
			NetworkId:   byJwt.NetworkId,
			NetworkName: byJwt.NetworkName,
			GuestMode:   byJwt.GuestMode,
			Principal:   byJwt.Principal,
			Roles:       byJwt.Roles,
			AuthTime:    byJwt.CreateTime,
		}, consentRequest.ConsentReceipt)
	} else {
		code, err = Authorize(clientSession.Ctx, authorizationRequest, &AuthorizingUser{
			UserId:      byJwt.UserId,
			NetworkId:   byJwt.NetworkId,
			NetworkName: byJwt.NetworkName,
			GuestMode:   byJwt.GuestMode,
			// the principal identifies the user; roles carry through from the
			// authorizing session, which for an ordinary login is empty
			Principal: byJwt.Principal,
			Roles:     byJwt.Roles,
			AuthTime:  server.NowUtc(),
		})
	}
	if err != nil {
		writeGrantError(w, err)
		return
	}

	redirectUri, err := authorizationRedirect(authorizationRequest.RedirectUri, code, authorizationRequest.State)
	if err != nil {
		writeOauthError(w, http.StatusBadRequest, "invalid_request", "The redirect_uri could not be built.")
		return
	}

	writeJson(w, http.StatusOK, &AuthorizeResult{RedirectUri: redirectUri})
}

// Builds the redirect back to the client. `iss` is included per rfc 9207 so
// the client can detect a mix-up attack; the metadata advertises that we send
// it.
func authorizationRedirect(redirectUri string, code string, state string) (string, error) {
	redirectUrl, err := url.Parse(redirectUri)
	if err != nil {
		return "", err
	}

	query := redirectUrl.Query()
	query.Set("code", code)
	query.Set("iss", Issuer())
	if state != "" {
		query.Set("state", state)
	}
	redirectUrl.RawQuery = query.Encode()

	return redirectUrl.String(), nil
}

func (self *ConsentRequest) authorizationRequest() *AuthorizationRequest {
	responseType := self.ResponseType
	if responseType == "" {
		responseType = "code"
	}
	codeChallengeMethod := self.CodeChallengeMethod
	if codeChallengeMethod == "" {
		codeChallengeMethod = PkceMethodS256
	}

	return &AuthorizationRequest{
		ClientId:             self.ClientId,
		RedirectUri:          self.RedirectUri,
		ResponseType:         responseType,
		Scopes:               ParseScope(self.Scope),
		State:                self.State,
		CodeChallenge:        self.CodeChallenge,
		CodeChallengeMethod:  codeChallengeMethod,
		Resource:             self.Resource,
		Nonce:                self.Nonce,
		DpopJkt:              self.DpopJkt,
		AuthorizationDetails: self.AuthorizationDetails,
	}
}

func bearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	const prefix = "bearer "
	if len(prefix) < len(auth) && strings.ToLower(auth[:len(prefix)]) == prefix {
		return auth[len(prefix):]
	}
	return ""
}

// The window a rate limited caller must wait out, for the Retry-After header.
func registrationRetryAfterSeconds() string {
	return strconv.Itoa(int(rateLimitSettings.RegistrationBurstDuration / time.Second))
}
