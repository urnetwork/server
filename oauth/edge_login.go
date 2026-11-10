package oauth

// §7.8's edge login (R-TEAM-14, D-TEAM-15, I-13): a published name's own front
// (§13.7's members audience) signs its visitors in through this issuer, which
// makes the issuer an authority for those origins (§2.8 EX8).
//
//	client     the origin itself, client_id=https://<host>.<net>.<root>, a
//	           client of no registration and no metadata document: accepted
//	           only while the network <net> names holds, in its newest verified
//	           projection, an active publish grant of <host> whose audience is
//	           members (§8), and refused once the grant ends, at authorization
//	           and again at the code's redemption
//	redirect   exactly that origin's /.well-known/ur/callback, so no open
//	           redirect: no other uri, port, path or query
//	visitor    signed in on ur.io (a team IdP where a team requires one, with
//	           TEAM-M6-05), and only an active member of the network: in a
//	           personal network its owner's own sign-in; a team's members wait
//	           on TEAM-M6's member rows, so a team network's origin refuses
//	           every visitor until then
//	token      the code redeems, with its PKCE verifier, for an ID token alone:
//	           {iss, aud = origin, network_id, nonce, exp = 60 s, sub =
//	           member_id, the owner's user_id in a personal network}, signed by
//	           the id- key set, whose public keys EdgeJwksUri serves apart from
//	           the mcp set's
//	log        each login goes to api's 7-day service log alone (api-11):
//	           member, network, origin, time and nonce
//
// The issuer reads no projection itself: EdgeLoginSource answers which network
// an origin names and whether its grant is live, and api installs one
// (SetEdgeLoginSource) once it hosts §8.9's gate cache (ACL's AclGateCache).
// With none installed every edge origin is refused, never fetched as a client
// metadata document.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"

	"github.com/urnetwork/server"
)

const (
	// the path an edge origin's front receives its code at (§7.8)
	EdgeCallbackPath = "/.well-known/ur/callback"
	// an edge login's ID token lives this long: a format value of §7.8's flow
	EdgeIdTokenDuration = 60 * time.Second
	// the client type of an edge origin
	ClientTypeEdge = "edge"
	// the one scope an edge login asks
	EdgeScopeOpenid = "openid"
)

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
	// publish grant of host whose audience is members (§8.4).
	MembersPublishGranted(ctx context.Context, networkId server.Id, host string, now time.Time) (bool, error)
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
	// https://<host>.<net>.<root>, the client_id and the ID token's aud
	Origin    string
	NetworkId server.Id
	// the published host the grant names
	Host string
}

// The origin's callback, the one redirect_uri it may use.
func (self *EdgeOrigin) RedirectUri() string {
	return self.Origin + EdgeCallbackPath
}

// The host of an https origin with nothing but a host, lowercase; false for any
// other url, which is then no edge origin.
func edgeOriginHost(clientId string) (string, bool) {
	originUrl, err := url.Parse(clientId)
	if err != nil || originUrl.Scheme != "https" || originUrl.Host == "" || originUrl.Port() != "" ||
		originUrl.User != nil || (originUrl.Path != "" && originUrl.Path != "/") || originUrl.RawQuery != "" || originUrl.Fragment != "" {
		return "", false
	}
	host := originUrl.Hostname()
	if host != strings.ToLower(host) || "https://"+host != clientId {
		return "", false
	}
	return host, true
}

// The edge origin clientId names, ok false for a client id that is no edge
// origin of a root this server serves. An origin under such a root that no
// installed source can judge is refused, not passed on.
func resolveEdgeOrigin(ctx context.Context, clientId string) (origin *EdgeOrigin, ok bool, err error) {
	originHost, isOrigin := edgeOriginHost(clientId)
	if !isOrigin {
		return nil, false, nil
	}
	source := currentEdgeLoginSource()
	if source == nil {
		if edgeLoginRootHolds(ctx, originHost) {
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
	return &EdgeOrigin{Origin: "https://" + originHost, NetworkId: networkId, Host: host}, true, nil
}

// Whether a dns root this server serves holds host, read from §12's dns_root
// (MG5), so an edge origin is never fetched as a client metadata document even
// while no source is installed. False in a binary that does not link MG5.
func edgeLoginRootHolds(ctx context.Context, host string) bool {
	if !server.SidecarMg5Linked() {
		return false
	}
	holds := false
	server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`SELECT EXISTS (SELECT 1 FROM dns_root WHERE $1 LIKE '%.' || root)`,
				host,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&holds))
				}
			})
		})
	})
	return holds
}

// Whether the origin's grant is live now, refusing when it is not or cannot be
// told.
func checkEdgeOriginGrant(ctx context.Context, origin *EdgeOrigin, now time.Time) error {
	source := currentEdgeLoginSource()
	if source == nil {
		return ErrEdgeOriginRefused
	}
	granted, err := source.MembersPublishGranted(ctx, origin.NetworkId, origin.Host, now)
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

// Whether the user may sign in to the network's origins: an active member of
// it. In a personal network that is its owner, signed in to that network; a team
// network's members are TEAM-M6's, so its origins refuse every visitor until
// they land.
func edgeVisitorIsMember(ctx context.Context, networkId server.Id, user *AuthorizingUser) (memberId server.Id, err error) {
	if user.GuestMode || user.NetworkId != networkId {
		return server.Id{}, ErrEdgeVisitorRefused
	}
	var adminUserId *server.Id
	found := false
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT admin_user_id FROM network WHERE network_id = $1`, networkId)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&adminUserId))
				found = true
			}
		})
	})
	if !found || adminUserId == nil || *adminUserId != user.UserId {
		return server.Id{}, ErrEdgeVisitorRefused
	}
	if server.SidecarMg6Linked() {
		team := false
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `SELECT EXISTS (SELECT 1 FROM network_team WHERE network_id = $1)`, networkId)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&team))
				}
			})
		})
		if team {
			return server.Id{}, ErrEdgeVisitorRefused
		}
	}
	return user.UserId, nil
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
	client, err := edgeOriginClient(ctx, origin)
	if err != nil {
		return "", err
	}
	if !client.ValidRedirectUri(request.RedirectUri) {
		return "", ErrInvalidRedirect
	}
	if request.ResponseType != "code" || request.CodeChallengeMethod != PkceMethodS256 || request.CodeChallenge == "" ||
		request.Nonce == "" || !slices.Equal(request.Scopes, []string{EdgeScopeOpenid}) || request.Resource != "" {
		return "", fmt.Errorf("%w: an edge login asks openid with PKCE S256 and a nonce", ErrInvalidRequest)
	}
	memberId, err := edgeVisitorIsMember(ctx, origin.NetworkId, user)
	if err != nil {
		return "", err
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
	idToken, err := MintEdgeIdToken(origin.Origin, code.NetworkId, code.UserId, code.Nonce, server.NowUtc())
	if err != nil {
		return nil, err
	}
	// api's 7-day service log (api-11): the published origin is public, every id opaque
	glog.Infof("[oauth]edge login member=%s network=%s origin=%s nonce=%s\n", code.UserId, code.NetworkId, origin.Origin, code.Nonce)
	return &EdgeTokenResponse{
		IdToken:   idToken,
		TokenType: "Bearer",
		ExpiresIn: int(EdgeIdTokenDuration / time.Second),
	}, nil
}

// An edge login's ID token for origin, signed by the id- key set at now.
func MintEdgeIdToken(origin string, networkId server.Id, memberId server.Id, nonce string, now time.Time) (string, error) {
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
			ExpiresAt: gojwt.NewNumericDate(now.Add(EdgeIdTokenDuration)),
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

// Whether the rules of an AclState's rows grant an active publish of host whose
// audience is members: what an EdgeLoginSource over §8.9's gate cache answers
// for a network. A rule's public names take a host as sidecar's MayPublish
// does: a bare * every host, else label by label, * within a label any run of
// characters; an expired rule, one with another audience and a name with a path
// take nothing. Whose rule it is does not matter here: the issuer asks whether
// the network publishes the host to its members at all.
func AclMembersPublishGranted(acl *sidecar.AclState, host string, now time.Time) bool {
	if acl == nil || host == "" || strings.Contains(host, "*") {
		return false
	}
	for _, rule := range acl.Rows().GetRules() {
		if expires := rule.GetExpires(); expires != 0 && expires <= now.UnixMilli() {
			continue
		}
		if !slices.Contains(rule.GetActions(), uint32(protocol.AclAction_ACL_ACTION_PUBLISH)) {
			continue
		}
		switch rule.GetAudience() {
		case protocol.AclAudience_ACL_AUDIENCE_UNSPECIFIED, protocol.AclAudience_ACL_AUDIENCE_MEMBERS, protocol.AclAudience_ACL_AUDIENCE_MEMBERS_DEVICE:
		default:
			continue
		}
		for _, target := range rule.GetTargets() {
			if name := target.GetPublicName(); name != "" && !strings.Contains(name, "/") && edgeHostCovers(name, host) {
				return true
			}
		}
	}
	return false
}

// Whether a dns pattern takes a host: a bare * every host, else label by label,
// as sidecar's aclDnsHostCovers.
func edgeHostCovers(pattern string, host string) bool {
	if pattern == "*" {
		return true
	}
	patternLabels := strings.Split(pattern, ".")
	hostLabels := strings.Split(host, ".")
	if len(patternLabels) != len(hostLabels) {
		return false
	}
	for i, label := range patternLabels {
		if !edgeGlobMatches(label, hostLabels[i]) {
			return false
		}
	}
	return true
}

// Whether a label glob takes text: * any run of characters, every other
// character itself (sidecar's aclGlobMatches).
func edgeGlobMatches(pattern string, text string) bool {
	patternIndex := 0
	textIndex := 0
	starIndex := -1
	starTextIndex := 0
	for textIndex < len(text) {
		switch {
		case patternIndex < len(pattern) && pattern[patternIndex] == '*':
			starIndex = patternIndex
			starTextIndex = textIndex
			patternIndex += 1
		case patternIndex < len(pattern) && pattern[patternIndex] == text[textIndex]:
			patternIndex += 1
			textIndex += 1
		case 0 <= starIndex:
			starTextIndex += 1
			textIndex = starTextIndex
			patternIndex = starIndex + 1
		default:
			return false
		}
	}
	for patternIndex < len(pattern) && pattern[patternIndex] == '*' {
		patternIndex += 1
	}
	return patternIndex == len(pattern)
}
