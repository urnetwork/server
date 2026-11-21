package oauth

// §7.8's edge login (R-TEAM-14, D-TEAM-15, I-13): a published name's own front
// (§13.7's members audience) signs its visitors in through this issuer, which
// makes the issuer an authority for those origins (§2.8 EX8).
//
//	client     the origin itself, client_id=https://<host>.<net>.<root>, with
//	           :<port> for tlsedge's 8443, 8883 and 5671 and 443 written bare,
//	           never :443 (QD-723), a client of no registration and no metadata
//	           document: accepted only while the network <net> names holds, in
//	           its newest verified projection, an active publish grant of
//	           tls/<port>/<host> whose audience is members (§8), and refused
//	           once the grant ends, at authorization and again at the code's
//	           redemption. Each port's endpoint is an origin and a client of its
//	           own (D-TEAM-15), so a grant, a code and a token of one port
//	           count for nothing on another
//	redirect   exactly that origin's /.well-known/ur/callback, so no open
//	           redirect: no other uri, port, path or query
//	visitor    signed in on ur.io (a team IdP where a team requires one, with
//	           TEAM-M6-05), and only an active member of the network: in a
//	           personal network its owner's own sign-in, in a Team the current
//	           membership of the signed-in visitor
//	token      the code redeems, with its PKCE verifier, for an ID token alone:
//	           {iss, aud = origin, its port as written, network_id, nonce, exp
//	           = 60 s (EdgeLoginSettings.IdTokenDuration), sub =
//	           member_id, the owner's user_id in a personal network}, signed by
//	           the id- key set, whose public keys EdgeJwksUri serves apart from
//	           the mcp set's
//	log        each login goes to api's 7-day service log alone (api-11):
//	           member, network, origin, time and nonce
//
// The issuer reads no projection itself: EdgeLoginSource answers which network
// an origin's host names and whether its port's grant is live. The API installs
// that source with its verified §8.9 gate cache.
// With none installed every edge origin is refused, never fetched as a client
// metadata document.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

const (
	// the path an edge origin's front receives its code at (§7.8)
	EdgeCallbackPath = "/.well-known/ur/callback"
	// the client type of an edge origin
	ClientTypeEdge = "edge"
	// the one scope an edge login asks
	EdgeScopeOpenid = "openid"
	// https's own port, which an origin never writes (QD-723)
	edgeOriginBarePort = "443"
)

// Tunables of edge login.
type EdgeLoginSettings struct {
	// How long an edge login's ID token lives: §7.8's 60 s, which a front
	// takes no further than its own IdTokenMaxLifetime.
	IdTokenDuration time.Duration
}

func DefaultEdgeLoginSettings() *EdgeLoginSettings {
	return &EdgeLoginSettings{
		IdTokenDuration: 60 * time.Second,
	}
}

var edgeLoginSettings atomic.Pointer[EdgeLoginSettings]

// Installs the issuer's edge login settings; nil restores the defaults.
func SetEdgeLoginSettings(settings *EdgeLoginSettings) {
	edgeLoginSettings.Store(settings)
}

func currentEdgeLoginSettings() *EdgeLoginSettings {
	if settings := edgeLoginSettings.Load(); settings != nil {
		return settings
	}
	return DefaultEdgeLoginSettings()
}

var (
	// the origin is no edge origin this issuer serves, or its grant is not live
	ErrEdgeOriginRefused = fmt.Errorf("%w: the origin holds no members publish grant", ErrUnknownClient)
	// the visitor is not an active member of the origin's network
	ErrEdgeVisitorRefused = fmt.Errorf("%w: not a member of the origin's network", ErrUnauthorizedUser)
)

// What edge login asks of the projection. Safe for concurrent use.
type EdgeLoginSource interface {
	// The network an edge origin's host names and the published host under it,
	// <host> of <host>.<net>.<root> for a root this server serves; ok false for
	// a host under no such root, which is then no edge origin.
	ResolveOrigin(ctx context.Context, originHost string) (networkId server.Id, host string, ok bool, err error)
	// Whether the network's newest verified projection holds, now, an active
	// publish grant of host on port, tls/<port>/<host>, whose audience is
	// members (§8.4): AclMembersPublishGranted over the network's AclState. A
	// grant of the host's other ports answers nothing for this one.
	MembersPublishGranted(ctx context.Context, networkId server.Id, host string, port int, now time.Time) (bool, error)
}

var edgeLoginSource atomic.Pointer[EdgeLoginSource]

// Installs api's source; nil removes it, refusing every edge origin.
func SetEdgeLoginSource(source EdgeLoginSource) {
	if source == nil {
		edgeLoginSource.Store(nil)
		return
	}
	edgeLoginSource.Store(&source)
}

func currentEdgeLoginSource() EdgeLoginSource {
	if source := edgeLoginSource.Load(); source != nil {
		return *source
	}
	return nil
}

// An edge origin as a client_id names it.
type EdgeOrigin struct {
	// https://<host>.<net>.<root>, with :<port> unless the port is 443: the
	// client_id and the ID token's aud
	Origin    string
	NetworkId server.Id
	// the published host the grant names
	Host string
	// the tlsedge port its endpoint listens on: 443, 8443, 8883 or 5671
	Port int
}

// The origin's callback, the one redirect_uri it may use.
func (self *EdgeOrigin) RedirectUri() string {
	return self.Origin + EdgeCallbackPath
}

// The host of an https origin with nothing but a lowercase host and, at most,
// a port; false for any other url, which is then no edge origin. Which ports an
// edge origin may write is resolveEdgeOrigin's to say.
func edgeOriginHost(clientId string) (string, bool) {
	host, _, ok := edgeOriginHostPort(clientId)
	return host, ok
}

// edgeOriginHost with the port as the client id writes it, "" for an origin
// that writes none.
func edgeOriginHostPort(clientId string) (host string, portText string, ok bool) {
	originUrl, err := url.Parse(clientId)
	if err != nil || originUrl.Scheme != "https" || originUrl.Host == "" ||
		originUrl.User != nil || originUrl.Path != "" || originUrl.RawQuery != "" || originUrl.Fragment != "" {
		return "", "", false
	}
	host = originUrl.Hostname()
	if host == "" || host != strings.ToLower(host) {
		return "", "", false
	}
	if "https://"+host == clientId {
		return host, "", true
	}
	portText, written := strings.CutPrefix(clientId, "https://"+host+":")
	if !written {
		return "", "", false
	}
	return host, portText, true
}

// The tlsedge port an origin under a served root names, by what it writes
// after its host: none is 443, and a written port is 8443, 8883 or 5671 in its
// one spelling. 443 written out, any other port and any other spelling are
// refused, so each endpoint has one origin and one client_id (QD-723). The four
// ports are the tls/ namespace's own (sidecar.ParseSocketName), which keeps
// the issuer's list the one a listener registers under.
func edgeOriginPort(host string, portText string, written bool) (int, bool) {
	switch {
	case !written:
		portText = edgeOriginBarePort
	case portText == edgeOriginBarePort:
		return 0, false
	}
	socketName := "tls/" + portText + "/" + host
	name, namespace, err := sidecar.ParseSocketName(socketName)
	if err != nil || namespace != sidecar.SocketNamespaceTls || name != socketName {
		return 0, false
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return 0, false
	}
	return port, true
}

// The edge origin clientId names, ok false for a client id that is no edge
// origin of a root this server serves. An origin under such a root that no
// installed source can judge, or whose port is not one an edge origin writes,
// is refused, not passed on.
func resolveEdgeOrigin(ctx context.Context, clientId string) (origin *EdgeOrigin, ok bool, err error) {
	originHost, portText, isOrigin := edgeOriginHostPort(clientId)
	if !isOrigin {
		return nil, false, nil
	}
	source := currentEdgeLoginSource()
	if source == nil {
		root, err := edgeLoginRoot(ctx, originHost)
		if err != nil || root != "" {
			return nil, true, ErrEdgeOriginRefused
		}
		return nil, false, nil
	}
	networkId, host, under, err := source.ResolveOrigin(ctx, originHost)
	if err != nil {
		return nil, true, fmt.Errorf("%w: %w", ErrEdgeOriginRefused, err)
	}
	if !under {
		return nil, false, nil
	}
	port, served := edgeOriginPort(host, portText, clientId != "https://"+originHost)
	if !served {
		return nil, true, ErrEdgeOriginRefused
	}
	return &EdgeOrigin{Origin: clientId, NetworkId: networkId, Host: host, Port: port}, true, nil
}

// Whether the grant of the origin's host on its port is live now, refusing when
// it is not or cannot be told.
func checkEdgeOriginGrant(ctx context.Context, origin *EdgeOrigin, now time.Time) error {
	source := currentEdgeLoginSource()
	if source == nil {
		return ErrEdgeOriginRefused
	}
	granted, err := source.MembersPublishGranted(ctx, origin.NetworkId, origin.Host, origin.Port, now)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrEdgeOriginRefused, err)
	}
	if !granted {
		return ErrEdgeOriginRefused
	}
	return nil
}

// The client an edge origin is while its grant is live: its one redirect uri,
// openid alone, no registration and nothing fetched.
func edgeOriginClient(ctx context.Context, origin *EdgeOrigin) (*Client, error) {
	if err := checkEdgeOriginGrant(ctx, origin, server.NowUtc()); err != nil {
		return nil, err
	}
	return &Client{
		ClientId:        origin.Origin,
		ClientType:      ClientTypeEdge,
		ClientName:      strings.TrimPrefix(origin.Origin, "https://"),
		ClientUri:       origin.Origin,
		ApplicationType: ApplicationTypeWeb,
		RedirectUris:    []string{origin.RedirectUri()},
		Scopes:          []string{EdgeScopeOpenid},
	}, nil
}

// The resource-less OIDC request is distinct from every third-party or command grant.
func isEdgeSignIn(request *AuthorizationRequest) bool {
	_, isOrigin := edgeOriginHost(request.ClientId)
	return isOrigin && request.Resource == "" && request.ResponseType == "code" &&
		request.RedirectUri == request.ClientId+EdgeCallbackPath && request.CodeChallengeMethod == PkceMethodS256 &&
		request.CodeChallenge != "" && request.Nonce != "" && slices.Equal(request.Scopes, []string{EdgeScopeOpenid})
}

// IsEdgeSignInRequest lets the role middleware admit a person who lacks key-management
// permission to the edge handler. The handler authenticates the visitor, verifies the origin
// and its grant and checks destination membership. No other grant accepts this request.
func IsEdgeSignInRequest(r *http.Request) bool {
	if r.Body == nil {
		return false
	}
	limit := DefaultOauthHandlerSettings().ConsentRequestMaxByteCount
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil || int64(len(body)) > limit {
		return false
	}
	var request ConsentRequest
	return json.Unmarshal(body, &request) == nil && isEdgeSignIn(request.authorizationRequest())
}

// A personal sign-in is portable to a Team with an active membership for its user.
// A personal origin still requires its owner's own network. A Team requiring its IdP
// accepts only a session established there through that IdP.
func edgeVisitorSignIn(ctx context.Context, networkId server.Id, user *AuthorizingUser) (*model.NetworkSignIn, error) {
	if user == nil || user.GuestMode || user.NetworkId == (server.Id{}) || user.UserId == (server.Id{}) {
		return nil, ErrEdgeVisitorRefused
	}
	signIn := model.ResolveNetworkSignIn(ctx, user.UserId, networkId, server.NowUtc())
	if signIn == nil || (signIn.MemberId == nil && user.NetworkId != networkId) {
		return nil, ErrEdgeVisitorRefused
	}
	if signIn.SsoRequired {
		if user.NetworkId != networkId {
			return nil, ErrEdgeVisitorRefused
		}
		sso, err := user.edgeCredential.AuthenticatedWithTeamIdp(ctx)
		if err != nil {
			return nil, err
		}
		if !sso {
			return nil, ErrEdgeVisitorRefused
		}
	}
	return signIn, nil
}

func validateEdgeSignIn(ctx context.Context, origin *EdgeOrigin, request *AuthorizationRequest, user *AuthorizingUser) (*model.NetworkSignIn, error) {
	if request.RedirectUri != origin.RedirectUri() {
		return nil, ErrInvalidRedirect
	}
	if !isEdgeSignIn(request) {
		return nil, fmt.Errorf("%w: an edge login asks openid with PKCE S256 and a nonce", ErrInvalidRequest)
	}
	if err := checkEdgeOriginGrant(ctx, origin, server.NowUtc()); err != nil {
		return nil, err
	}
	return edgeVisitorSignIn(ctx, origin.NetworkId, user)
}

// Mints the code of an edge login (§7.8's issuer row): the origin's grant live,
// its callback exact, PKCE S256, a nonce, scope openid, and the visitor an
// active member of the origin's network. No consent is recorded: the network's
// own published name asks its own members to sign in.
func AuthorizeEdgeOrigin(ctx context.Context, request *AuthorizationRequest, user *AuthorizingUser) (string, error) {
	origin, isOrigin, err := resolveEdgeOrigin(ctx, request.ClientId)
	if err != nil {
		return "", err
	}
	if !isOrigin {
		return "", ErrUnknownClient
	}
	return authorizeEdgeVisitor(ctx, origin, request, user)
}

func authorizeEdgeVisitor(ctx context.Context, origin *EdgeOrigin, request *AuthorizationRequest, user *AuthorizingUser) (string, error) {
	signIn, err := validateEdgeSignIn(ctx, origin, request, user)
	if err != nil {
		return "", err
	}
	memberId := signIn.UserId
	if signIn.MemberId != nil {
		memberId = *signIn.MemberId
	}
	authTime := user.AuthTime
	if authTime.IsZero() {
		authTime = server.NowUtc()
	}
	return CreateAuthorizationCode(ctx, &AuthorizationCode{
		ClientId:            origin.Origin,
		UserId:              memberId,
		NetworkId:           origin.NetworkId,
		RedirectUri:         request.RedirectUri,
		CodeChallenge:       request.CodeChallenge,
		CodeChallengeMethod: request.CodeChallengeMethod,
		// the one audience its ID token names
		Resource: origin.Origin,
		Scopes:   []string{EdgeScopeOpenid},
		Nonce:    request.Nonce,
		AuthTime: authTime,
	})
}

// What an edge login's code redeems for: the ID token alone.
type EdgeTokenResponse struct {
	IdToken   string `json:"id_token"`
	TokenType string `json:"token_type"`
	ExpiresIn int    `json:"expires_in"`
}

// The claims of an edge login's ID token (§7.8).
type EdgeIdTokenClaims struct {
	Nonce     string    `json:"nonce"`
	NetworkId server.Id `json:"network_id"`
	gojwt.RegisteredClaims
}

// Redeems an edge login's code: the origin's grant still live, the code its
// own, its callback and PKCE verifier exact, once. Logs the login to api's
// service log alone.
func ExchangeEdgeCode(ctx context.Context, args *ExchangeCodeArgs) (*EdgeTokenResponse, error) {
	origin, isOrigin, err := resolveEdgeOrigin(ctx, args.ClientId)
	if err != nil {
		return nil, err
	}
	if !isOrigin {
		return nil, ErrUnknownClient
	}
	if err := checkEdgeOriginGrant(ctx, origin, server.NowUtc()); err != nil {
		return nil, err
	}
	if args.RedirectUri != origin.RedirectUri() {
		return nil, ErrInvalidRedirect
	}
	code, err := RedeemAuthorizationCode(ctx, args.Code, origin.Origin, args.RedirectUri, args.CodeVerifier, origin.Origin)
	if err != nil {
		return nil, err
	}
	if code.NetworkId != origin.NetworkId {
		return nil, ErrInvalidCode
	}
	// A code names the membership that authorized it. Removal or a rejoin between
	// authorization and redemption must not turn that code into a fresh session.
	userId := code.UserId
	if member := model.GetNetworkMember(ctx, code.UserId); member != nil {
		userId = member.UserId
	}
	signIn := model.ResolveNetworkSignIn(ctx, userId, origin.NetworkId, server.NowUtc())
	if signIn == nil || (signIn.MemberId != nil && *signIn.MemberId != code.UserId) ||
		(signIn.MemberId == nil && signIn.UserId != code.UserId) {
		return nil, ErrEdgeVisitorRefused
	}
	settings := currentEdgeLoginSettings()
	idToken, err := mintEdgeIdToken(origin.Origin, code.NetworkId, code.UserId, code.Nonce, server.NowUtc(), settings)
	if err != nil {
		return nil, err
	}
	// api's 7-day service log (api-11): the published origin is public, every id opaque
	glog.Infof("[oauth]edge login member=%s network=%s origin=%s nonce=%s\n", code.UserId, code.NetworkId, origin.Origin, code.Nonce)
	return &EdgeTokenResponse{
		IdToken:   idToken,
		TokenType: "Bearer",
		ExpiresIn: int(settings.IdTokenDuration / time.Second),
	}, nil
}

// An edge login's ID token for origin, signed by the id- key set at now.
func MintEdgeIdToken(origin string, networkId server.Id, memberId server.Id, nonce string, now time.Time) (string, error) {
	return mintEdgeIdToken(origin, networkId, memberId, nonce, now, currentEdgeLoginSettings())
}

func mintEdgeIdToken(origin string, networkId server.Id, memberId server.Id, nonce string, now time.Time, settings *EdgeLoginSettings) (string, error) {
	signingKey, err := SigningKeyOf(SignerKeySetId)
	if err != nil {
		return "", err
	}
	claims := &EdgeIdTokenClaims{
		Nonce:     nonce,
		NetworkId: networkId,
		RegisteredClaims: gojwt.RegisteredClaims{
			Issuer:    Issuer(),
			Subject:   memberId.String(),
			Audience:  gojwt.ClaimStrings{origin},
			IssuedAt:  gojwt.NewNumericDate(now),
			ExpiresAt: gojwt.NewNumericDate(now.Add(settings.IdTokenDuration)),
			ID:        server.NewId().String(),
		},
	}
	token := gojwt.NewWithClaims(gojwt.SigningMethodES256, claims)
	token.Header["kid"] = signingKey.Kid
	return token.SignedString(signingKey.PrivateKey)
}

var ErrEdgeIdTokenInvalid = errors.New("edge id token invalid")

// Verifies an edge login's ID token as an origin's front does (§7.8's front
// row): the id- set's signature, the issuer, aud the origin, the nonce its
// cookie holds and exp at now. The member check against its config is the
// front's.
func VerifyEdgeIdToken(tokenStr string, origin string, nonce string, now time.Time) (*EdgeIdTokenClaims, error) {
	claims := &EdgeIdTokenClaims{}
	_, err := gojwt.ParseWithClaims(
		tokenStr,
		claims,
		func(token *gojwt.Token) (any, error) {
			kid, ok := token.Header["kid"].(string)
			if !ok {
				return nil, errors.New("no kid")
			}
			key := verificationKeyOf(SignerKeySetId, kid)
			if key == nil {
				return nil, fmt.Errorf("no id- key %s", kid)
			}
			return &key.PrivateKey.PublicKey, nil
		},
		gojwt.WithValidMethods([]string{signerAlgEs256}),
		gojwt.WithIssuer(Issuer()),
		gojwt.WithAudience(origin),
		gojwt.WithExpirationRequired(),
		gojwt.WithTimeFunc(func() time.Time { return now }),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrEdgeIdTokenInvalid, err)
	}
	if claims.Nonce == "" || claims.Nonce != nonce {
		return nil, fmt.Errorf("%w: nonce", ErrEdgeIdTokenInvalid)
	}
	return claims, nil
}

// Where the id- set's public keys are served.
func EdgeJwksUri() string {
	return fmt.Sprintf("%s/.well-known/ur/id-jwks.json", Issuer())
}

// GET /.well-known/ur/id-jwks.json: the id- set's public keys, which an edge
// origin's front verifies its ID tokens with.
func EdgeJwksHandler(w http.ResponseWriter, r *http.Request) {
	writeJson(w, http.StatusOK, PublicJwks(SignerKeySetId))
}

// GET /oauth/authorize: §7.8 sends a visitor here, while the authorization
// endpoint this issuer advertises is ur.io's consent page, where the session
// lives. Forwards there with the query unchanged.
func AuthorizeRedirectHandler(w http.ResponseWriter, r *http.Request) {
	target, err := url.Parse(AuthorizeEndpoint())
	if err != nil || target.Scheme != "https" && target.Scheme != "http" {
		writeOauthError(w, http.StatusInternalServerError, "server_error", "Please try again.")
		return
	}
	target.RawQuery = r.URL.RawQuery
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, target.String(), http.StatusFound)
}

// Whether a projection's AclState holds, now, a members publish grant of host on
// port: what an EdgeLoginSource over §8.9's gate cache answers for a network
// (QD-723). It is ACL-M5-01's AclState.PublishGrant for tls/<port>/<host> in
// http mode, asked of each sidecar the state admits, so it carries every rule
// of §8.10's: a rule binding another port's socket gives nothing here, publish
// needs listen on the bound socket, rules that disagree give none, and a parked
// or suspended sidecar holds no grant. The issuer names no listener, so one
// sidecar's grant whose audience is members or members-device is enough: the
// network then publishes that endpoint to its members.
func AclMembersPublishGranted(acl *sidecar.AclState, host string, port int, now time.Time) bool {
	if acl == nil || host == "" || port <= 0 {
		return false
	}
	socketName := "tls/" + strconv.Itoa(port) + "/" + host
	if name, namespace, err := sidecar.ParseSocketName(socketName); err != nil || namespace != sidecar.SocketNamespaceTls || name != socketName {
		return false
	}
	for _, sidecarId := range acl.View().SidecarIds() {
		grant, decision := acl.PublishGrant(sidecar.AclSidecarSubject(sidecarId), socketName, protocol.AclAppMode_ACL_APP_MODE_HTTP, now)
		if !decision.Allowed || grant == nil {
			continue
		}
		switch grant.Audience {
		case protocol.AclAudience_ACL_AUDIENCE_MEMBERS, protocol.AclAudience_ACL_AUDIENCE_MEMBERS_DEVICE:
			return true
		}
	}
	return false
}
