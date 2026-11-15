package model

// §7.8's network OAuth clients (TEAM-M6-08; R-TEAM-6, R-TEAM-12, D-TEAM-6; §2.8 api-11): a team's CI credential,
// ur-oauth-<id> with ur_sk_<secret>, for the client_credentials grant (server/oauth/client_credentials.go), which
// mints an hour's access token acting as the client's creating member within the client's scopes. It lasts until
// revoked.
//
//	create  POST /network/oauth-clients {scopes}: an Owner's or Admin's network credential (the keys permission) of a
//	        team on the team tier, or a one-command admin token with the all scope whose use the transaction spends.
//	        Scopes are §7.8's devices:core, auth_keys, policy:read, logs:read and all. network_oauth_client keeps the
//	        scopes, the creating member and only the SHA-256 of the secret; the answer alone holds the secret. A new
//	        client owes the network authority notice and an admin event
//	list    GET /network/oauth-clients: the team's clients, never a secret
//	revoke  POST /network/oauth-client-revoke {oauth_client_id}: the row goes, and its live tokens stop at their next
//	        request, since each request checks the client still stands
//
// The server cannot admit (D-TEAM-6): a client mints auth keys of at most an hour (NetworkAuthKeySettings'
// OauthClientMaxLifetime), and CI's own Delegation, which only a person's device signs, admits its sidecars. A
// creator who leaves the team leaves its clients with no role, so they reach nothing.
//
// Nothing here reads or writes network_oauth_client unless the binary links MG6.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// The credential's format values: ur-oauth-<id> and ur_sk_<secret>, each part lowercase RFC 4648 base32 with no
// padding, as an auth key's bearer is written.
const (
	NetworkOauthClientIdPrefix     = "ur-oauth-"
	NetworkOauthClientSecretPrefix = "ur_sk_"
	// the secret's random bytes
	networkOauthClientSecretByteCount = 32
)

var networkOauthClientEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// The admin event words of OAuth clients.
const (
	AdminEventTargetOauthClient       = "oauth_client"
	AdminEventActionOauthClientCreate = "oauth_client.create"
	AdminEventActionOauthClientRevoke = "oauth_client.revoke"
)

// The scope an admin token needs to manage OAuth clients: only all, as for members.
const networkOauthClientAdminScope = "all"

// Refusals, fixed text that names no secret.
const (
	networkOauthClientRefusedMessage = "Only the team's Owners and Admins may manage its OAuth clients."
	networkOauthClientScopesMessage  = "An OAuth client carries one or more of devices:core, auth_keys, policy:read, logs:read and all."
	networkOauthClientUnknownMessage = "No such OAuth client."
)

func init() {
	// each a one-command admin token reaches, its use spent in the route's transaction
	session.RegisterAdminCommandRoute(http.MethodPost, "/network/oauth-clients")
	session.RegisterAdminCommandRoute(http.MethodGet, "/network/oauth-clients")
	session.RegisterAdminCommandRoute(http.MethodPost, "/network/oauth-client-revoke")
}

// §7.8's OAuth client scopes, the only ones a client carries.
var networkOauthClientScopes = []string{ApiKeyScopeDevicesCore, ApiKeyScopeAuthKeys, ApiKeyScopePolicyRead, ApiKeyScopeLogsRead, ApiKeyScopeAll}

// A client as network_oauth_client holds it, without its secret.
type NetworkOauthClient struct {
	OauthClientId   server.Id
	NetworkId       server.Id
	Scopes          []string
	CreatorMemberId server.Id
}

// The client_id a client authenticates with.
func NetworkOauthClientIdText(oauthClientId server.Id) string {
	return NetworkOauthClientIdPrefix + networkOauthClientEncoding.EncodeToString(oauthClientId.Bytes())
}

// The id a client_id names, false for anything else. A decoding that does not encode back is refused.
func ParseNetworkOauthClientId(clientId string) (server.Id, bool) {
	text, found := strings.CutPrefix(clientId, NetworkOauthClientIdPrefix)
	if !found {
		return server.Id{}, false
	}
	idBytes, err := networkOauthClientEncoding.DecodeString(text)
	if err != nil || len(idBytes) != len(server.Id{}) || networkOauthClientEncoding.EncodeToString(idBytes) != text {
		return server.Id{}, false
	}
	oauthClientId, err := server.IdFromBytes(idBytes)
	return oauthClientId, err == nil
}

// The secret's bytes, false for a client_secret that is not one.
func parseNetworkOauthClientSecret(clientSecret string) ([]byte, bool) {
	text, found := strings.CutPrefix(clientSecret, NetworkOauthClientSecretPrefix)
	if !found {
		return nil, false
	}
	secret, err := networkOauthClientEncoding.DecodeString(text)
	if err != nil || len(secret) != networkOauthClientSecretByteCount || networkOauthClientEncoding.EncodeToString(secret) != text {
		return nil, false
	}
	return secret, true
}

// The client oauthClientId names, nil for none or a binary that does not link MG6.
func GetNetworkOauthClient(ctx context.Context, oauthClientId server.Id) (client *NetworkOauthClient) {
	if !server.SidecarMg6Linked() {
		return nil
	}
	server.Db(ctx, func(conn server.PgConn) {
		client, _ = getNetworkOauthClientWithQuery(ctx, conn, oauthClientId)
	})
	return
}

func getNetworkOauthClientWithQuery(ctx context.Context, query server.PgCanQuery, oauthClientId server.Id) (client *NetworkOauthClient, secretSha256 []byte) {
	result, err := query.Query(
		ctx,
		`
			SELECT network_id, scopes, creator_member_id, secret_sha256
			FROM network_oauth_client
			WHERE oauth_client_id = $1
		`,
		oauthClientId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			client = &NetworkOauthClient{OauthClientId: oauthClientId}
			var scopesJson []byte
			server.Raise(result.Scan(&client.NetworkId, &scopesJson, &client.CreatorMemberId, &secretSha256))
			server.Raise(json.Unmarshal(scopesJson, &client.Scopes))
		}
	})
	return
}

// The client a client_id and client_secret authenticate, nil for none: an unknown id, a revoked client or another
// secret, told apart by nothing. The secret is compared by its SHA-256 in constant time.
func AuthenticateNetworkOauthClient(ctx context.Context, clientId string, clientSecret string) *NetworkOauthClient {
	if !server.SidecarMg6Linked() {
		return nil
	}
	oauthClientId, ok := ParseNetworkOauthClientId(clientId)
	secret, secretOk := parseNetworkOauthClientSecret(clientSecret)
	if !ok || !secretOk {
		return nil
	}
	presented := sha256.Sum256(secret)
	var client *NetworkOauthClient
	var stored []byte
	server.Db(ctx, func(conn server.PgConn) {
		client, stored = getNetworkOauthClientWithQuery(ctx, conn, oauthClientId)
	})
	if client == nil || subtle.ConstantTimeCompare(stored, presented[:]) != 1 {
		return nil
	}
	return client
}

type CreateNetworkOauthClientArgs struct {
	Scopes []string `json:"scopes"`
}

type NetworkOauthClientError struct {
	Message string `json:"message"`
}

// Prints without the secret.
type CreateNetworkOauthClientResult struct {
	OauthClientId *server.Id `json:"oauth_client_id,omitempty"`
	// ur-oauth-<id>
	ClientId string `json:"client_id,omitempty"`
	// ur_sk_<secret>, which this answer alone holds
	ClientSecret string                   `json:"client_secret,omitempty"`
	Scopes       []string                 `json:"scopes,omitempty"`
	Error        *NetworkOauthClientError `json:"error,omitempty"`
}

// Never the secret.
func (self *CreateNetworkOauthClientResult) String() string {
	return "CreateNetworkOauthClientResult{}"
}

// As String, for %#v.
func (self *CreateNetworkOauthClientResult) GoString() string {
	return self.String()
}

// What OAuth clients depend on apart from the database, so tests can replace it.
type networkOauthClients struct {
	now    func() time.Time
	tierOf func(ctx context.Context, networkId server.Id) NetworkTier
}

func newNetworkOauthClients() *networkOauthClients {
	return &networkOauthClients{
		now:    server.NowUtc,
		tierOf: NetworkTierOf,
	}
}

// The caller checks every OAuth client route makes before its transaction: MG6, no client token or API key, an admin
// token's all scope, and the team tier.
func (self *networkOauthClients) checkCaller(clientSession *session.ClientSession) error {
	if !server.SidecarMg6Linked() {
		return fmt.Errorf("%d %s", http.StatusServiceUnavailable, networkTeamsUnavailableMessage)
	}
	byJwt := clientSession.ByJwt
	if byJwt == nil || byJwt.ClientId != nil || clientSession.ApiKeyAuthenticated || clientSession.OauthClient != nil {
		return fmt.Errorf("%d %s", http.StatusForbidden, networkOauthClientRefusedMessage)
	}
	adminCommand := clientSession.AdminCommand
	if adminCommand != nil && !slices.Contains(adminCommand.Scopes, networkOauthClientAdminScope) {
		return fmt.Errorf("%d %s", http.StatusForbidden, "The admin token does not carry the all scope.")
	}
	if clientSession.UnsignedIdentity && adminCommand == nil {
		return fmt.Errorf("%d %s", http.StatusForbidden, networkOauthClientRefusedMessage)
	}
	return requireTeamTierOf(self.tierOf(clientSession.Ctx, byJwt.NetworkId))
}

// Runs a route's work in one transaction as an Owner or Admin: the caller's role first, so a refused caller writes
// nothing; then an admin token's use, a replay answering the first use's stored result into replayed; then work,
// whose answer the token's use stores.
func (self *networkOauthClients) inTx(clientSession *session.ClientSession, now time.Time, replayed any, work func(tx server.PgTx) any) (answer any, err error) {
	ctx := clientSession.Ctx
	byJwt := clientSession.ByJwt
	adminCommand := clientSession.AdminCommand
	server.Tx(ctx, func(tx server.PgTx) {
		answer, err = nil, nil
		caller := GetNetworkMemberOfUserInTx(ctx, tx, byJwt.NetworkId, byJwt.UserId)
		if caller == nil || caller.Pending || !caller.Role.Permits(RolePermissionKeys) {
			err = fmt.Errorf("%d %s", http.StatusForbidden, networkOauthClientRefusedMessage)
			return
		}
		if adminCommand != nil {
			storedResult, first, claimErr := ClaimAdminTokenUseInTx(ctx, tx, adminCommand, now)
			server.Raise(claimErr)
			if !first {
				server.Raise(json.Unmarshal(storedResult, replayed))
				answer = replayed
				return
			}
		}
		answer = work(tx)
		if adminCommand != nil {
			resultJson, marshalErr := json.Marshal(answer)
			server.Raise(marshalErr)
			server.Raise(StoreAdminTokenUseResultInTx(ctx, tx, adminCommand.Jti, resultJson))
		}
	})
	return
}

// POST /network/oauth-clients (see the file header).
func CreateNetworkOauthClient(args *CreateNetworkOauthClientArgs, clientSession *session.ClientSession) (*CreateNetworkOauthClientResult, error) {
	return newNetworkOauthClients().create(args, clientSession)
}

func (self *networkOauthClients) create(args *CreateNetworkOauthClientArgs, clientSession *session.ClientSession) (*CreateNetworkOauthClientResult, error) {
	if err := self.checkCaller(clientSession); err != nil {
		return nil, err
	}
	scopes := []string{}
	for _, scope := range args.Scopes {
		if !slices.Contains(networkOauthClientScopes, scope) {
			return &CreateNetworkOauthClientResult{Error: &NetworkOauthClientError{Message: networkOauthClientScopesMessage}}, nil
		}
		if !slices.Contains(scopes, scope) {
			scopes = append(scopes, scope)
		}
	}
	if len(scopes) == 0 {
		return &CreateNetworkOauthClientResult{Error: &NetworkOauthClientError{Message: networkOauthClientScopesMessage}}, nil
	}
	slices.Sort(scopes)
	scopesJson, err := json.Marshal(scopes)
	if err != nil {
		return nil, err
	}
	secret := make([]byte, networkOauthClientSecretByteCount)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	secretSha256 := sha256.Sum256(secret)
	oauthClientId := server.NewId()
	created := &CreateNetworkOauthClientResult{
		OauthClientId: &oauthClientId,
		ClientId:      NetworkOauthClientIdText(oauthClientId),
		ClientSecret:  NetworkOauthClientSecretPrefix + networkOauthClientEncoding.EncodeToString(secret),
		Scopes:        scopes,
	}
	clear(secret)
	now := self.now().UTC().Truncate(time.Microsecond)
	ctx := clientSession.Ctx
	byJwt := clientSession.ByJwt
	answer, err := self.inTx(clientSession, now, &CreateNetworkOauthClientResult{}, func(tx server.PgTx) any {
		caller := GetNetworkMemberOfUserInTx(ctx, tx, byJwt.NetworkId, byJwt.UserId)
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_oauth_client (
					oauth_client_id,
					network_id,
					secret_sha256,
					scopes,
					creator_member_id
				)
				VALUES ($1, $2, $3, $4, $5)
			`,
			oauthClientId,
			byJwt.NetworkId,
			secretSha256[:],
			scopesJson,
			caller.MemberId,
		))
		AddAdminEventInTx(ctx, tx, NewSessionAdminEvent(clientSession, AdminEventActionOauthClientCreate, AdminEventTargetOauthClient, &oauthClientId))
		oweSecurityNetworkAuthorityNoticeInTx(ctx, tx, byJwt.NetworkId, &SecurityNetworkAuthorityChange{
			ChangeTime: now,
			Action:     SecurityNetworkAuthorityOauthClient,
			Via:        securityNoticeVia(ctx),
		})
		return created
	})
	if err != nil {
		return nil, err
	}
	return answer.(*CreateNetworkOauthClientResult), nil
}

// A client as GET /network/oauth-clients shows it.
type NetworkOauthClientResult struct {
	OauthClientId   server.Id `json:"oauth_client_id"`
	ClientId        string    `json:"client_id"`
	Scopes          []string  `json:"scopes"`
	CreatorMemberId server.Id `json:"creator_member_id"`
}

type GetNetworkOauthClientsResult struct {
	OauthClients []*NetworkOauthClientResult `json:"oauth_clients"`
}

// GET /network/oauth-clients (see the file header).
func GetNetworkOauthClients(clientSession *session.ClientSession) (*GetNetworkOauthClientsResult, error) {
	return newNetworkOauthClients().list(clientSession)
}

func (self *networkOauthClients) list(clientSession *session.ClientSession) (*GetNetworkOauthClientsResult, error) {
	if err := self.checkCaller(clientSession); err != nil {
		return nil, err
	}
	ctx := clientSession.Ctx
	now := self.now().UTC().Truncate(time.Microsecond)
	answer, err := self.inTx(clientSession, now, &GetNetworkOauthClientsResult{}, func(tx server.PgTx) any {
		listed := &GetNetworkOauthClientsResult{OauthClients: []*NetworkOauthClientResult{}}
		result, err := tx.Query(
			ctx,
			`
				SELECT oauth_client_id, scopes, creator_member_id
				FROM network_oauth_client
				WHERE network_id = $1
				ORDER BY oauth_client_id
			`,
			clientSession.ByJwt.NetworkId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				client := &NetworkOauthClientResult{}
				var scopesJson []byte
				server.Raise(result.Scan(&client.OauthClientId, &scopesJson, &client.CreatorMemberId))
				server.Raise(json.Unmarshal(scopesJson, &client.Scopes))
				client.ClientId = NetworkOauthClientIdText(client.OauthClientId)
				listed.OauthClients = append(listed.OauthClients, client)
			}
		})
		return listed
	})
	if err != nil {
		return nil, err
	}
	return answer.(*GetNetworkOauthClientsResult), nil
}

type RevokeNetworkOauthClientArgs struct {
	OauthClientId server.Id `json:"oauth_client_id"`
}

type RevokeNetworkOauthClientResult struct {
	OauthClientId *server.Id               `json:"oauth_client_id,omitempty"`
	Error         *NetworkOauthClientError `json:"error,omitempty"`
}

// POST /network/oauth-client-revoke (see the file header).
func RevokeNetworkOauthClient(args *RevokeNetworkOauthClientArgs, clientSession *session.ClientSession) (*RevokeNetworkOauthClientResult, error) {
	return newNetworkOauthClients().revoke(args, clientSession)
}

func (self *networkOauthClients) revoke(args *RevokeNetworkOauthClientArgs, clientSession *session.ClientSession) (*RevokeNetworkOauthClientResult, error) {
	if err := self.checkCaller(clientSession); err != nil {
		return nil, err
	}
	ctx := clientSession.Ctx
	now := self.now().UTC().Truncate(time.Microsecond)
	answer, err := self.inTx(clientSession, now, &RevokeNetworkOauthClientResult{}, func(tx server.PgTx) any {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM network_oauth_client WHERE oauth_client_id = $1 AND network_id = $2`,
			args.OauthClientId,
			clientSession.ByJwt.NetworkId,
		))
		if tag.RowsAffected() != 1 {
			return &RevokeNetworkOauthClientResult{Error: &NetworkOauthClientError{Message: networkOauthClientUnknownMessage}}
		}
		oauthClientId := args.OauthClientId
		AddAdminEventInTx(ctx, tx, NewSessionAdminEvent(clientSession, AdminEventActionOauthClientRevoke, AdminEventTargetOauthClient, &oauthClientId))
		return &RevokeNetworkOauthClientResult{OauthClientId: &oauthClientId}
	})
	if err != nil {
		return nil, err
	}
	return answer.(*RevokeNetworkOauthClientResult), nil
}

// POST /network/oauth-clients at a tier a test gives.
func Testing_CreateNetworkOauthClient(args *CreateNetworkOauthClientArgs, clientSession *session.ClientSession, tierOf func(ctx context.Context, networkId server.Id) NetworkTier) (*CreateNetworkOauthClientResult, error) {
	return (&networkOauthClients{now: server.NowUtc, tierOf: tierOf}).create(args, clientSession)
}

// POST /network/oauth-client-revoke at a tier a test gives.
func Testing_RevokeNetworkOauthClient(args *RevokeNetworkOauthClientArgs, clientSession *session.ClientSession, tierOf func(ctx context.Context, networkId server.Id) NetworkTier) (*RevokeNetworkOauthClientResult, error) {
	return (&networkOauthClients{now: server.NowUtc, tierOf: tierOf}).revoke(args, clientSession)
}
