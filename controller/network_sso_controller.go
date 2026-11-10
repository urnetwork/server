package controller

// §7.6's sign-in through a team's IdP (R-TEAM-4, D-TEAM-4, D-TEAM-23, Q-TEAM-4;
// TEAM-M6-05): OpenID Connect's code flow with PKCE S256, state and nonce,
// api the relying party.
//
//	begin     BeginNetworkSso keeps a flow, its state, nonce and PKCE verifier
//	          and the user whose own sign-in began it, if any, in the flow
//	          store, and answers the IdP's authorization request. POST
//	          /auth/sso-discover (TEAM-M6-06) is its route
//	callback  GET /auth/sso/callback takes the flow by its state once, so a
//	          replayed state finds nothing; redeems the code with the verifier
//	          and verifies the ID token, iss, aud, exp and the flow's nonce
//	          under the IdP's JWKS; then resolves the identity
//	          (model.ResolveNetworkSsoLoginInTx): an unlinked one links only to
//	          the user that began the flow, and a first sign-in is a pending
//	          member, which gets no credential until an admin's device admits it
//	mint      a live member's credential for the IdP's network alone, through
//	          model.MintNetworkSignIn with the IdP's mark, which an SSO-required
//	          team asks of every sign-in
//
// The flow store's §2.8 row is open (questions/team-m2-16-sso-flow-state-row.md),
// so production installs none and every flow is refused meanwhile, failing
// closed. api fetches only https issuers, never through a redirect, and never
// dials a non-public address (model.NetworkIdpDialControl).

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-jose/go-jose/v4"
	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// What api asks of an IdP.
type NetworkOidcClient interface {
	// The IdP's authorization request for the flow: response_type code, scope
	// openid email, its state, nonce and redirect, and the S256 challenge of
	// its verifier.
	AuthorizationUrl(ctx context.Context, idp *model.NetworkIdp, flow *model.NetworkSsoFlow) (string, error)
	// Redeems code at the IdP's token endpoint with the flow's verifier, and
	// answers the ID token's identity once its signature under the IdP's JWKS,
	// its iss, aud, exp and the flow's nonce verify.
	Exchange(ctx context.Context, idp *model.NetworkIdp, flow *model.NetworkSsoFlow, code string) (*model.NetworkOidcIdentity, error)
}

var networkOidcClient atomic.Pointer[NetworkOidcClient]

// Installs the OIDC client; nil restores the HTTP one.
func SetNetworkOidcClient(client NetworkOidcClient) {
	if client == nil {
		networkOidcClient.Store(nil)
		return
	}
	networkOidcClient.Store(&client)
}

func currentNetworkOidcClient() NetworkOidcClient {
	if client := networkOidcClient.Load(); client != nil {
		return *client
	}
	return defaultNetworkOidcClient()
}

// api's HTTP OIDC client, made on first use.
var defaultNetworkOidcClient = sync.OnceValue(func() NetworkOidcClient {
	return NewHttpNetworkOidcClient(DefaultHttpNetworkOidcClientSettings())
})

// The origin the IdP redirects back to, the api's public one. A test replaces
// it.
var networkSsoRedirectOrigin = session.DpopApiOrigin

// 128 random bits, base64url with no padding.
func networkSsoRandom() (string, error) {
	b := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// PKCE's S256 challenge of a verifier (RFC 7636 §4.2).
func NetworkSsoCodeChallenge(codeVerifier string) string {
	digest := sha256.Sum256([]byte(codeVerifier))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

// The refusal of a sign-in flow, which names nothing of the request.
type NetworkSsoError struct {
	Message string `json:"message"`
}

type NetworkSsoBeginResult struct {
	AuthorizationUrl string `json:"authorization_url"`
}

// Begins a sign-in through the IdP: the flow kept until FlowTtl, and the
// IdP's authorization request. linkingSession is the caller's own sign-in, a
// network credential of its user, which an unlinked identity may link to; nil
// begins with none.
func BeginNetworkSso(ctx context.Context, idpId server.Id, linkingSession *session.ClientSession) (*NetworkSsoBeginResult, error) {
	store := model.CurrentNetworkSsoFlowStore()
	if store == nil {
		return nil, fmt.Errorf("%d Sign-in through an IdP is not available yet.", http.StatusServiceUnavailable)
	}
	idp := model.GetNetworkIdp(ctx, idpId)
	if idp == nil {
		return nil, fmt.Errorf("%d No such IdP.", http.StatusNotFound)
	}
	origin, err := networkSsoRedirectOrigin()
	if err != nil {
		return nil, fmt.Errorf("%d The api has no public origin.", http.StatusServiceUnavailable)
	}
	flow := &model.NetworkSsoFlow{
		IdpId:       idp.IdpId,
		NetworkId:   idp.NetworkId,
		RedirectUri: origin + "/auth/sso/callback",
		CreateTime:  server.NowUtc(),
	}
	for _, value := range []*string{&flow.State, &flow.Nonce, &flow.CodeVerifier} {
		if *value, err = networkSsoRandom(); err != nil {
			return nil, err
		}
	}
	if linkingSession != nil && linkingSession.ByJwt != nil && linkingSession.ByJwt.ClientId == nil &&
		!linkingSession.ApiKeyAuthenticated && !linkingSession.UnsignedIdentity {
		// a user's own sign-in, never a client's token or a key acting for it
		userId := linkingSession.ByJwt.UserId
		flow.LinkingUserId = &userId
	}
	if err := store.Put(ctx, flow, model.DefaultNetworkIdpSettings().FlowTtl); err != nil {
		return nil, err
	}
	authorizationUrl, err := currentNetworkOidcClient().AuthorizationUrl(ctx, idp, flow)
	if err != nil {
		return nil, fmt.Errorf("%d The IdP is unavailable.", http.StatusBadGateway)
	}
	return &NetworkSsoBeginResult{AuthorizationUrl: authorizationUrl}, nil
}

// GET /auth/sso/callback's query.
type NetworkSsoCallbackArgs struct {
	State string `json:"state"`
	Code  string `json:"code"`
	// the IdP's own refusal
	Error string `json:"error,omitempty"`
}

// Never the code.
func (self *NetworkSsoCallbackArgs) String() string {
	return "NetworkSsoCallbackArgs{}"
}

// As String, for %#v.
func (self *NetworkSsoCallbackArgs) GoString() string {
	return self.String()
}

type NetworkSsoCallbackResult struct {
	// the member's network credential; empty for a pending member
	ByJwt     string           `json:"by_jwt,omitempty"`
	NetworkId *server.Id       `json:"network_id,omitempty"`
	Pending   bool             `json:"pending,omitempty"`
	Linked    bool             `json:"linked,omitempty"`
	Error     *NetworkSsoError `json:"error,omitempty"`
}

// Never the credential.
func (self *NetworkSsoCallbackResult) String() string {
	return "NetworkSsoCallbackResult{}"
}

// As String, for %#v.
func (self *NetworkSsoCallbackResult) GoString() string {
	return self.String()
}

// GET /auth/sso/callback (§7.6): the flow by its state, once; the code
// redeemed and the ID token verified; the identity resolved, linked or JIT;
// and a live member's credential for the IdP's network alone.
func NetworkSsoCallback(callback *NetworkSsoCallbackArgs, clientSession *session.ClientSession) (*NetworkSsoCallbackResult, error) {
	refuse := func(message string) (*NetworkSsoCallbackResult, error) {
		return &NetworkSsoCallbackResult{Error: &NetworkSsoError{Message: message}}, nil
	}
	store := model.CurrentNetworkSsoFlowStore()
	if store == nil {
		return nil, fmt.Errorf("%d Sign-in through an IdP is not available yet.", http.StatusServiceUnavailable)
	}
	if callback.State == "" {
		return refuse("The sign-in has no state.")
	}
	flow, err := store.Take(clientSession.Ctx, callback.State)
	if err != nil {
		return nil, err
	}
	if flow == nil {
		// unknown, expired, or a replay of a state already taken
		return refuse("The sign-in expired or was already used. Start again.")
	}
	if callback.Error != "" || callback.Code == "" {
		return refuse("The IdP did not sign you in.")
	}
	idp := model.GetNetworkIdp(clientSession.Ctx, flow.IdpId)
	if idp == nil || idp.NetworkId != flow.NetworkId {
		return refuse("The IdP is gone.")
	}
	identity, err := currentNetworkOidcClient().Exchange(clientSession.Ctx, idp, flow, callback.Code)
	if err != nil {
		if glog.V(1) {
			glog.Infof("[sso][%s]idp %s refused: %s\n", idp.NetworkId, idp.IdpId, err)
		}
		return refuse("The IdP's answer did not verify.")
	}
	settings := model.DefaultNetworkIdpSettings()
	var login *model.NetworkSsoLogin
	var loginErr error
	server.Tx(clientSession.Ctx, func(tx server.PgTx) {
		login, loginErr = model.ResolveNetworkSsoLoginInTx(clientSession.Ctx, tx, idp, identity, flow.LinkingUserId, server.NowUtc(), settings)
	})
	switch {
	case errors.Is(loginErr, model.ErrNetworkSsoLinkRequired), errors.Is(loginErr, model.ErrIdpIdentityLinkedElsewhere):
		return refuse(loginErr.Error())
	case loginErr != nil:
		return nil, loginErr
	}
	networkId := login.NetworkId
	result := &NetworkSsoCallbackResult{NetworkId: &networkId, Linked: login.Linked}
	if login.Pending {
		result.Pending = true
		return result, nil
	}
	signed, signIn, err := model.MintNetworkSignIn(clientSession.Ctx, login.UserId, idp.NetworkId, "sso", true)
	if err != nil {
		return nil, err
	}
	if signIn == nil {
		return refuse("You have no access to this team.")
	}
	result.ByJwt = signed
	return result, nil
}

// Tunables of api's HTTP OIDC client.
type HttpNetworkOidcClientSettings struct {
	// One request to an IdP: 10 s.
	RequestTimeout time.Duration
	// The most bytes api reads of one IdP answer: 1 MiB.
	MaxBodyByteCount int64
	// Refuses a dial: by default model.NetworkIdpDialControl's, every
	// non-public address. A test's IdP on loopback sets its own.
	DialControl func(network string, address string, conn syscall.RawConn) error
	// The transport's TLS roots, nil for the system's. A test's IdP sets its
	// own.
	Transport *http.Transport
}

func DefaultHttpNetworkOidcClientSettings() *HttpNetworkOidcClientSettings {
	return &HttpNetworkOidcClientSettings{
		RequestTimeout:   10 * time.Second,
		MaxBodyByteCount: 1024 * 1024,
		DialControl: func(network string, address string, conn syscall.RawConn) error {
			return model.NetworkIdpDialControl(network, address, conn)
		},
	}
}

// api's OIDC client over HTTP: discovery, the token endpoint with
// client_secret_post, and the ID token under the IdP's JWKS.
type httpNetworkOidcClient struct {
	settings   *HttpNetworkOidcClientSettings
	httpClient *http.Client
}

func NewHttpNetworkOidcClient(settings *HttpNetworkOidcClientSettings) NetworkOidcClient {
	transport := settings.Transport
	if transport == nil {
		transport = http.DefaultTransport.(*http.Transport).Clone()
	} else {
		transport = transport.Clone()
	}
	dialer := &net.Dialer{Timeout: settings.RequestTimeout, Control: settings.DialControl}
	transport.DialContext = dialer.DialContext
	transport.Proxy = nil
	return &httpNetworkOidcClient{
		settings: settings,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   settings.RequestTimeout,
			// an IdP's endpoints answer where they are; a redirect could lead
			// anywhere
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// The parts of an IdP's discovery document api reads.
type networkOidcDiscovery struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JwksUri               string `json:"jwks_uri"`
}

// GETs or POSTs one IdP endpoint and decodes its JSON answer into out.
func (self *httpNetworkOidcClient) do(req *http.Request, out any) error {
	if req.URL.Scheme != "https" {
		return errors.New("an IdP endpoint that is not https")
	}
	res, err := self.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("the IdP answered %d", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, self.settings.MaxBodyByteCount))
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}

// The IdP's discovery document, its issuer the one configured.
func (self *httpNetworkOidcClient) discover(ctx context.Context, idp *model.NetworkIdp) (*networkOidcDiscovery, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(idp.Issuer, "/")+"/.well-known/openid-configuration", nil)
	if err != nil {
		return nil, err
	}
	discovery := &networkOidcDiscovery{}
	if err := self.do(req, discovery); err != nil {
		return nil, err
	}
	if discovery.Issuer != idp.Issuer {
		return nil, errors.New("the discovery document names another issuer")
	}
	for _, endpoint := range []string{discovery.AuthorizationEndpoint, discovery.TokenEndpoint, discovery.JwksUri} {
		if !strings.HasPrefix(endpoint, "https://") {
			return nil, errors.New("an IdP endpoint that is not https")
		}
	}
	return discovery, nil
}

func (self *httpNetworkOidcClient) AuthorizationUrl(ctx context.Context, idp *model.NetworkIdp, flow *model.NetworkSsoFlow) (string, error) {
	discovery, err := self.discover(ctx, idp)
	if err != nil {
		return "", err
	}
	authorizationUrl, err := url.Parse(discovery.AuthorizationEndpoint)
	if err != nil {
		return "", err
	}
	query := authorizationUrl.Query()
	query.Set("response_type", "code")
	query.Set("client_id", idp.ClientId)
	query.Set("redirect_uri", flow.RedirectUri)
	query.Set("scope", "openid email")
	query.Set("state", flow.State)
	query.Set("nonce", flow.Nonce)
	query.Set("code_challenge", NetworkSsoCodeChallenge(flow.CodeVerifier))
	query.Set("code_challenge_method", "S256")
	authorizationUrl.RawQuery = query.Encode()
	return authorizationUrl.String(), nil
}

// The claims of an IdP's ID token api reads.
type networkOidcIdTokenClaims struct {
	Nonce         string `json:"nonce"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	gojwt.RegisteredClaims
}

func (self *httpNetworkOidcClient) Exchange(ctx context.Context, idp *model.NetworkIdp, flow *model.NetworkSsoFlow, code string) (*model.NetworkOidcIdentity, error) {
	discovery, err := self.discover(ctx, idp)
	if err != nil {
		return nil, err
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", flow.RedirectUri)
	form.Set("code_verifier", flow.CodeVerifier)
	form.Set("client_id", idp.ClientId)
	switch idp.ClientAuth {
	case model.NetworkIdpClientAuthClientSecret:
		secret, err := idp.ClientSecret()
		if err != nil {
			return nil, err
		}
		form.Set("client_secret", secret)
	default:
		// private_key_jwt's assertion key is not built: no assertion, no
		// exchange
		return nil, errors.New("private_key_jwt is not available yet")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, discovery.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokens := &struct {
		IdToken string `json:"id_token"`
	}{}
	if err := self.do(req, tokens); err != nil {
		return nil, err
	}
	jwksReq, err := http.NewRequestWithContext(ctx, http.MethodGet, discovery.JwksUri, nil)
	if err != nil {
		return nil, err
	}
	keySet := &jose.JSONWebKeySet{}
	if err := self.do(jwksReq, keySet); err != nil {
		return nil, err
	}
	return VerifyNetworkOidcIdToken(tokens.IdToken, keySet, idp, flow, server.NowUtc())
}

// Verifies an IdP's ID token for the flow at now: RS256 or ES256 under a key of
// keySet named by its kid, iss the IdP's issuer, aud its client id, exp and
// iat, the flow's nonce, and a sub.
func VerifyNetworkOidcIdToken(idToken string, keySet *jose.JSONWebKeySet, idp *model.NetworkIdp, flow *model.NetworkSsoFlow, now time.Time) (*model.NetworkOidcIdentity, error) {
	claims := &networkOidcIdTokenClaims{}
	parser := gojwt.NewParser(
		gojwt.WithValidMethods([]string{"RS256", "ES256"}),
		gojwt.WithIssuer(idp.Issuer),
		gojwt.WithAudience(idp.ClientId),
		gojwt.WithExpirationRequired(),
		gojwt.WithIssuedAt(),
		gojwt.WithTimeFunc(func() time.Time { return now }),
	)
	_, err := parser.ParseWithClaims(idToken, claims, func(token *gojwt.Token) (any, error) {
		kid, _ := token.Header["kid"].(string)
		for _, key := range keySet.Key(kid) {
			if key.Use == "" || key.Use == "sig" {
				return key.Key, nil
			}
		}
		return nil, errors.New("no key of the IdP's set signed it")
	})
	if err != nil {
		return nil, err
	}
	if claims.Nonce == "" || claims.Nonce != flow.Nonce {
		return nil, errors.New("the ID token's nonce is not the flow's")
	}
	if claims.Subject == "" || 255 < len(claims.Subject) {
		return nil, errors.New("the ID token names no sub")
	}
	return &model.NetworkOidcIdentity{Sub: claims.Subject, Email: claims.Email, EmailVerified: claims.EmailVerified}, nil
}
