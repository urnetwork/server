package model

// §7.6's team IdPs (R-TEAM-4, R-TEAM-8, R-TEAM-12, D-TEAM-4, D-TEAM-23, Q-TEAM-4;
// TEAM-M6-05): OpenID Connect only, one or more network_idp rows a team's Owner
// adds with POST /network/idp, each authenticating only its own network.
//
//	client     the code flow with PKCE S256, state and nonce
//	           (network_sso_controller.go); the IdP authenticates api by
//	           private_key_jwt, or by a client secret that api keeps sealed
//	           under a key only vault holds, never in the clear. Without that
//	           vault key, an IdP asking that way is refused: it fails closed
//	identity   (idp_id, sub), never an email (OIDC Core §5.7): a sub of another
//	           IdP is another identity
//	link       an identity links to an existing user only once that user has
//	           signed in with its own credential in the same flow, the flow
//	           begun from its own session (D-TEAM-23); an email alone, verified
//	           or not, links nothing, so a password account registered with a
//	           team's email before its domain was verified is never handed to
//	           the IdP
//	JIT        a linked user's first sign-in to the team makes it a pending
//	           member (§7.6): unsigned IdP data adds no reader, so it reaches
//	           nothing until an admin's device admits it and invites the
//	           IdP-verified email
//	reauth     each IdP sign-in moves the member's reauth_deadline 7 days on,
//	           which bounds access after the IdP disables the user where no
//	           SCIM runs
//
// A new IdP owes Owners and Admins the network authority notice (§7.8).
//
// Not here, and listed by TEAM-M6-05's draft: a person with no URnetwork account
// signing in first through an IdP, which needs an account the flow can make;
// and the vault key's provisioning.

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Tunables of team IdPs.
type NetworkIdpSettings struct {
	// How long a member's access lasts after its newest IdP sign-in where no
	// SCIM runs: 7 days (§7.6).
	ReauthWindow time.Duration
	// How long one sign-in flow lives between its start and its callback:
	// 10 min, a sign-in at the IdP with a second factor.
	FlowTtl time.Duration
}

func DefaultNetworkIdpSettings() *NetworkIdpSettings {
	return &NetworkIdpSettings{
		ReauthWindow: 7 * 24 * time.Hour,
		FlowTtl:      10 * time.Minute,
	}
}

// How an IdP authenticates api at its token endpoint.
const (
	NetworkIdpClientAuthPrivateKeyJwt = "private_key_jwt"
	NetworkIdpClientAuthClientSecret  = "client_secret"
)

// The widths of network_idp's text columns.
const (
	networkIdpIssuerMaxLength   = 1024
	networkIdpClientIdMaxLength = 1024
	// an IdP's sub is at most 255 ASCII characters (OIDC Core §2)
	networkIdpSubMaxLength = 255
)

// The vault resource holding the 32-byte key that seals IdP client secrets.
// Only vault holds it (§7.6: secrets live only in vault).
const NetworkIdpSecretKeyVaultResource = "network_idp_secret_key"

// An IdP as network_idp holds it, its secret still sealed.
type NetworkIdp struct {
	IdpId      server.Id
	NetworkId  server.Id
	Issuer     string
	ClientId   string
	ClientAuth string
	// the client secret sealed under the vault key, nil for private_key_jwt
	clientSecretCiphertext []byte
}

// The key that seals client secrets, nil while vault holds none. A test
// replaces it.
var networkIdpSecretKey = func() []byte {
	resource, err := server.Vault.SimpleResource(NetworkIdpSecretKeyVaultResource)
	if err != nil {
		return nil
	}
	key, err := resource.BytesE()
	if err != nil || len(key) != 32 {
		return nil
	}
	return key
}

// Replaces the vault key for a test, returning a restore function.
func Testing_SetNetworkIdpSecretKey(key []byte) func() {
	previous := networkIdpSecretKey
	networkIdpSecretKey = func() []byte { return key }
	return func() {
		networkIdpSecretKey = previous
	}
}

// Whether api can sign private_key_jwt client assertions: its assertion key is
// TEAM-M6-05's review phase's, so none is, and such an IdP is refused.
var networkIdpPrivateKeyJwtAvailable = func() bool {
	return false
}

// The client secret sealed under key for idpId: a 12-byte nonce, then
// AES-256-GCM over the secret, the idp_id its additional data, so a sealed
// secret moved to another row does not open.
func sealNetworkIdpSecret(key []byte, idpId server.Id, secret string) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, []byte(secret), idpId.Bytes()), nil
}

// Seals secret for idpId under the current vault key, for a test that writes
// an IdP row itself, such as one whose issuer is a loopback test server.
func Testing_SealNetworkIdpSecret(idpId server.Id, secret string) ([]byte, error) {
	key := networkIdpSecretKey()
	if key == nil {
		return nil, errors.New("vault holds no IdP secret key")
	}
	return sealNetworkIdpSecret(key, idpId, secret)
}

// The IdP's client secret, opened under the vault key.
func (self *NetworkIdp) ClientSecret() (string, error) {
	if self.ClientAuth != NetworkIdpClientAuthClientSecret || len(self.clientSecretCiphertext) == 0 {
		return "", errors.New("the IdP has no client secret")
	}
	key := networkIdpSecretKey()
	if key == nil {
		return "", errors.New("vault holds no IdP secret key")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(self.clientSecretCiphertext) < aead.NonceSize() {
		return "", errors.New("a sealed secret too short")
	}
	nonce, sealed := self.clientSecretCiphertext[:aead.NonceSize()], self.clientSecretCiphertext[aead.NonceSize():]
	secret, err := aead.Open(nil, nonce, sealed, self.IdpId.Bytes())
	if err != nil {
		return "", err
	}
	return string(secret), nil
}

// An issuer api may fetch from: an https URL with no user, query or fragment,
// whose host is a name or a public address. A host that resolves to a private
// address is refused where api dials it (the OIDC client's dialer).
func ValidNetworkIdpIssuer(issuer string) error {
	if issuer == "" || networkIdpIssuerMaxLength < len(issuer) {
		return errors.New("The issuer is missing or too long.")
	}
	issuerUrl, err := url.Parse(issuer)
	if err != nil || issuerUrl.Scheme != "https" || issuerUrl.Host == "" || issuerUrl.User != nil ||
		issuerUrl.RawQuery != "" || issuerUrl.Fragment != "" {
		return errors.New("The issuer must be an https URL with no query or fragment.")
	}
	if address, err := netip.ParseAddr(strings.Trim(issuerUrl.Hostname(), "[]")); err == nil && !NetworkIdpPublicAddress(address) {
		return errors.New("The issuer's host must be public.")
	}
	return nil
}

// Whether api may dial address for an IdP: a public unicast address, never a
// loopback, private, link-local, shared, multicast or unspecified one.
func NetworkIdpPublicAddress(address netip.Addr) bool {
	address = address.Unmap()
	if !address.IsValid() || address.IsLoopback() || address.IsPrivate() || address.IsLinkLocalUnicast() ||
		address.IsLinkLocalMulticast() || address.IsInterfaceLocalMulticast() || address.IsMulticast() || address.IsUnspecified() {
		return false
	}
	// 100.64.0.0/10, carrier-grade NAT, which reaches inside networks too
	sharedPrefix := netip.MustParsePrefix("100.64.0.0/10")
	return !sharedPrefix.Contains(address)
}

// A dialer's Control that refuses every non-public address, so a name that
// resolves inside api's network is never fetched (NetworkIdpPublicAddress).
func NetworkIdpDialControl(network string, address string, conn interface{}) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return err
	}
	if !NetworkIdpPublicAddress(ip) {
		return fmt.Errorf("an IdP at a non-public address")
	}
	return nil
}

type CreateNetworkIdpArgs struct {
	Issuer   string `json:"issuer"`
	ClientId string `json:"client_id"`
	// private_key_jwt or client_secret
	ClientAuth string `json:"client_auth"`
	// with client_secret only; sealed at once and never answered back
	ClientSecret string `json:"client_secret,omitempty"`
}

// Never the secret.
func (self *CreateNetworkIdpArgs) String() string {
	return "CreateNetworkIdpArgs{issuer " + self.Issuer + "}"
}

// As String, for %#v.
func (self *CreateNetworkIdpArgs) GoString() string {
	return self.String()
}

type CreateNetworkIdpError struct {
	Message string `json:"message"`
}

type CreateNetworkIdpResult struct {
	IdpId *server.Id             `json:"idp_id,omitempty"`
	Error *CreateNetworkIdpError `json:"error,omitempty"`
}

// §7.5's team routes check the tier. A test replaces it.
var networkIdpRequireTeamTier = RequireTeamTier

// Replaces the tier check for a test, returning a restore function.
func Testing_SetNetworkIdpRequireTeamTier(requireTeamTier func(ctx context.Context, networkId server.Id) error) func() {
	previous := networkIdpRequireTeamTier
	networkIdpRequireTeamTier = requireTeamTier
	return func() {
		networkIdpRequireTeamTier = previous
	}
}

// The action and target of an IdP's admin event.
const (
	AdminEventActionIdpAdd = "idp.add"
	AdminEventTargetIdp    = "idp"
)

// POST /network/idp (§7.6): adds an OIDC IdP to the caller's team, its secret
// sealed under the vault key. Team only (§7.5's team routes), and the
// Owner's: the route's role gate asks the Owner's permission.
func CreateNetworkIdp(createNetworkIdp *CreateNetworkIdpArgs, clientSession *session.ClientSession) (*CreateNetworkIdpResult, error) {
	refuse := func(message string) (*CreateNetworkIdpResult, error) {
		return &CreateNetworkIdpResult{Error: &CreateNetworkIdpError{Message: message}}, nil
	}
	if !server.SidecarMg6Linked() {
		return nil, fmt.Errorf("%d %s", http.StatusServiceUnavailable, networkTeamsUnavailableMessage)
	}
	networkId := clientSession.ByJwt.NetworkId
	if err := networkIdpRequireTeamTier(clientSession.Ctx, networkId); err != nil {
		return nil, err
	}
	if !NetworkIsTeam(clientSession.Ctx, networkId) {
		return refuse("Only a team network has an IdP.")
	}
	if err := ValidNetworkIdpIssuer(createNetworkIdp.Issuer); err != nil {
		return refuse(err.Error())
	}
	if createNetworkIdp.ClientId == "" || networkIdpClientIdMaxLength < len(createNetworkIdp.ClientId) {
		return refuse("The client id is missing or too long.")
	}
	idpId := server.NewId()
	var sealed []byte
	switch createNetworkIdp.ClientAuth {
	case NetworkIdpClientAuthPrivateKeyJwt:
		if createNetworkIdp.ClientSecret != "" {
			return refuse("A private_key_jwt IdP takes no client secret.")
		}
		if !networkIdpPrivateKeyJwtAvailable() {
			return refuse("private_key_jwt is not available yet.")
		}
	case NetworkIdpClientAuthClientSecret:
		if createNetworkIdp.ClientSecret == "" {
			return refuse("A client_secret IdP needs its secret.")
		}
		key := networkIdpSecretKey()
		if key == nil {
			return refuse("Client secrets are not available yet.")
		}
		var err error
		sealed, err = sealNetworkIdpSecret(key, idpId, createNetworkIdp.ClientSecret)
		if err != nil {
			return nil, err
		}
	default:
		return refuse("The client auth is private_key_jwt or client_secret.")
	}
	server.Tx(clientSession.Ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			clientSession.Ctx,
			`
				INSERT INTO network_idp (idp_id, network_id, issuer, client_id, client_auth, client_secret_ciphertext)
				VALUES ($1, $2, $3, $4, $5, $6)
			`,
			idpId,
			networkId,
			createNetworkIdp.Issuer,
			createNetworkIdp.ClientId,
			createNetworkIdp.ClientAuth,
			sealed,
		))
		AddAdminEventInTx(clientSession.Ctx, tx, NewSessionAdminEvent(clientSession, AdminEventActionIdpAdd, AdminEventTargetIdp, &idpId))
		// a new way to sign people in owes Owners and Admins a notice (§7.8)
		oweSecurityNetworkAuthorityNoticeInTx(clientSession.Ctx, tx, networkId, &SecurityNetworkAuthorityChange{
			ChangeTime: server.NowUtc(),
			Action:     SecurityNetworkAuthorityIdp,
			Via:        securityNoticeVia(clientSession.Ctx),
		})
	})
	return &CreateNetworkIdpResult{IdpId: &idpId}, nil
}

// The IdP, nil for none. A binary that does not link MG6 has none.
func GetNetworkIdp(ctx context.Context, idpId server.Id) (idp *NetworkIdp) {
	if !server.SidecarMg6Linked() {
		return nil
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT idp_id, network_id, issuer, client_id, client_auth, client_secret_ciphertext
				FROM network_idp
				WHERE idp_id = $1
			`,
			idpId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				idp = &NetworkIdp{}
				server.Raise(result.Scan(&idp.IdpId, &idp.NetworkId, &idp.Issuer, &idp.ClientId, &idp.ClientAuth, &idp.clientSecretCiphertext))
			}
		})
	})
	return
}

// Refused: the identity is linked to another user already.
var ErrIdpIdentityLinkedElsewhere = errors.New("The IdP identity belongs to another user.")

// The user an IdP identity is linked to, false for none.
func IdpIdentityUserInTx(ctx context.Context, tx server.PgTx, idpId server.Id, sub string) (userId server.Id, ok bool) {
	result, err := tx.Query(
		ctx,
		`SELECT user_id FROM network_idp_identity WHERE idp_id = $1 AND sub = $2`,
		idpId,
		sub,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&userId))
			ok = true
		}
	})
	return
}

// Links the identity (idpId, sub) to userId in tx (D-TEAM-4, D-TEAM-23): an
// identity links once, and a second link to the same user changes nothing. The
// caller has shown that userId signed in with its own credential in this flow;
// nothing here reads an email.
func LinkIdpIdentityInTx(ctx context.Context, tx server.PgTx, idpId server.Id, sub string, userId server.Id) error {
	if sub == "" || networkIdpSubMaxLength < len(sub) {
		return errors.New("an IdP sub is 1 to 255 characters")
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO network_idp_identity (idp_id, sub, user_id)
			VALUES ($1, $2, $3)
			ON CONFLICT (idp_id, sub) DO NOTHING
		`,
		idpId,
		sub,
		userId,
	))
	linkedUserId, ok := IdpIdentityUserInTx(ctx, tx, idpId, sub)
	if !ok || linkedUserId != userId {
		return ErrIdpIdentityLinkedElsewhere
	}
	return nil
}

// LinkIdpIdentityInTx in its own transaction.
func LinkIdpIdentity(ctx context.Context, idpId server.Id, sub string, userId server.Id) (returnErr error) {
	server.Tx(ctx, func(tx server.PgTx) {
		returnErr = LinkIdpIdentityInTx(ctx, tx, idpId, sub, userId)
	})
	return
}

// What an IdP asserts of one sign-in, its ID token verified.
type NetworkOidcIdentity struct {
	Sub           string
	Email         string
	EmailVerified bool
}

// Refused: the IdP identity is linked to no user, and the flow carried no
// user's own sign-in to link it to (D-TEAM-23).
var ErrNetworkSsoLinkRequired = errors.New("Sign in with your URnetwork account first, then sign in with your team's IdP to link them.")

// One IdP sign-in's result.
type NetworkSsoLogin struct {
	NetworkId server.Id
	UserId    server.Id
	MemberId  server.Id
	// the membership awaits an admin's admission (§7.6's JIT)
	Pending bool
	// this sign-in linked the identity
	Linked bool
}

// An IdP sign-in at now (§7.6): the identity's user, linking it to
// linkingUserId when it has none and the flow carried that user's own sign-in;
// that user's membership of the IdP's network, made pending at its first
// sign-in; and its reauth_deadline moved a window on. ErrNetworkSsoLinkRequired
// for an unlinked identity with no linking user, whatever email it asserts.
func ResolveNetworkSsoLoginInTx(
	ctx context.Context,
	tx server.PgTx,
	idp *NetworkIdp,
	identity *NetworkOidcIdentity,
	linkingUserId *server.Id,
	now time.Time,
	settings *NetworkIdpSettings,
) (*NetworkSsoLogin, error) {
	login := &NetworkSsoLogin{NetworkId: idp.NetworkId}
	userId, linked := IdpIdentityUserInTx(ctx, tx, idp.IdpId, identity.Sub)
	switch {
	case linked:
		login.UserId = userId
	case linkingUserId != nil:
		if err := LinkIdpIdentityInTx(ctx, tx, idp.IdpId, identity.Sub, *linkingUserId); err != nil {
			return nil, err
		}
		login.UserId, login.Linked = *linkingUserId, true
	default:
		return nil, ErrNetworkSsoLinkRequired
	}
	member := GetNetworkMemberOfUserInTx(ctx, tx, idp.NetworkId, login.UserId)
	if member == nil {
		var email *string
		if identity.EmailVerified && identity.Email != "" {
			// the IdP-verified email the admitting device invites (§7.6),
			// recorded for that and never read as an identity
			email = &identity.Email
		}
		var err error
		member, err = AddNetworkMemberInTx(ctx, tx, &AddNetworkMemberArgs{
			NetworkId: idp.NetworkId,
			UserId:    login.UserId,
			Email:     email,
			Role:      NetworkMemberRoleMember,
			Pending:   true,
		})
		if err != nil {
			return nil, err
		}
		// a membership the server made, pending, is an admin action in the
		// team's log (§7.11): by the user that signed in, of its own membership
		actorId, memberId := login.UserId, member.MemberId
		event := &AdminEvent{
			NetworkId:  idp.NetworkId,
			ActorKind:  AdminEventActorUser,
			ActorId:    &actorId,
			Action:     AdminEventActionMemberAdd,
			TargetKind: AdminEventTargetMember,
			TargetId:   &memberId,
			Result:     AdminEventResultOk,
		}
		if requestId, ok := session.RequestIdFromContext(ctx); ok {
			event.RequestId = &requestId
		}
		AddAdminEventInTx(ctx, tx, event)
	}
	login.MemberId, login.Pending = member.MemberId, member.Pending
	server.RaisePgResult(tx.Exec(
		ctx,
		`UPDATE network_member SET reauth_deadline = $3 WHERE network_id = $1 AND member_id = $2`,
		idp.NetworkId,
		member.MemberId,
		now.Add(settings.ReauthWindow),
	))
	return login, nil
}

// One sign-in flow between its start and its callback: what the IdP's answer
// is checked against.
type NetworkSsoFlow struct {
	// the state parameter, 128 random bits, the flow's key
	State     string    `json:"state"`
	IdpId     server.Id `json:"idp_id"`
	NetworkId server.Id `json:"network_id"`
	// the ID token's nonce, 128 random bits
	Nonce string `json:"nonce"`
	// PKCE's code_verifier, whose S256 the authorization request carried
	CodeVerifier string `json:"code_verifier"`
	RedirectUri  string `json:"redirect_uri"`
	// the user whose own sign-in began the flow, which an unlinked identity
	// links to (D-TEAM-23); nil for a flow begun with no sign-in
	LinkingUserId *server.Id `json:"linking_user_id,omitempty"`
	CreateTime    time.Time  `json:"create_time"`
}

// Where flows wait between their start and their callback. Take answers a flow
// once, so a replayed state finds nothing.
type NetworkSsoFlowStore interface {
	Put(ctx context.Context, flow *NetworkSsoFlow, ttl time.Duration) error
	// The flow, removed; nil for none: unknown, expired or taken.
	Take(ctx context.Context, state string) (*NetworkSsoFlow, error)
}

// This process's flow store. None is installed in production until the
// store's §2.8 row lands (questions/team-m2-16-sso-flow-state-row.md), so every
// sign-in flow is refused meanwhile: it fails closed.
var networkSsoFlowStore atomic.Pointer[NetworkSsoFlowStore]

// Installs the flow store; nil removes it.
func SetNetworkSsoFlowStore(store NetworkSsoFlowStore) {
	if store == nil {
		networkSsoFlowStore.Store(nil)
		return
	}
	networkSsoFlowStore.Store(&store)
}

// This process's flow store, nil for none.
func CurrentNetworkSsoFlowStore() NetworkSsoFlowStore {
	store := networkSsoFlowStore.Load()
	if store == nil {
		return nil
	}
	return *store
}
