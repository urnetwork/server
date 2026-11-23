package model

// §7.8's auth keys (D-TEAM-5, R-TEAM-6; §2.8 api-11): the credential `ur
// connect --token` enrolls a device with, in place of an unscoped, unexpiring
// urn_ API key. A key lives 5 minutes to 90 days, allows a maximum number of
// uses, and may be ephemeral, pre-approved and tagged.
//
// The string a person holds is ur-authkey-<id>-<secret>[#<bundle>]. <id> is the
// key's id and <secret> 32 random bytes, each lowercase RFC 4648 base32 with no
// padding (26 and 52 characters), so neither holds a "-". The bearer
// ur-authkey-<id>-<secret> appears once, in the create answer, and only the
// SHA-256 of its secret is stored. The #<bundle> of a pre-approved key, the
// genesis hash, the authorization head and a Delegation's seeds, is built on
// the creating device and never reaches the server (D-TEAM-22): a bearer
// holding "#" is refused without its value being logged or echoed, and so is
// a urn_ API key, which never enrolls (D-CORE-7).
//
// A key's tags are the opaque 16-byte ids of §7.9's tags, never their names,
// written as 32 lowercase hex characters.
//
// Revoking a key at the server stops its bearer only. A pre-approved key's
// bundle carries a Delegation's signing seed, and every copy of it keeps
// admitting devices until that Delegation ends by a customer-signed
// DelegationRevoke, so the revoke answer says so (D-TEAM-22, D-TEAM-31).
//
// Who makes which key (§7.5's roles, authKeyReachOf): a personal network's
// owner and a team member whose role, cut by its credential's scopes, holds
// the keys permission make any key; a member whose role holds keys for owned
// tags (a Network admin) a key for its own devices or a tagged key, pre-approved
// or not, whose every tag the network's tagOwners name its member an owner of at
// the newest verified state of the network's log (authKeyTagsOwnedBy), which
// enrollment asks again (network_client_owner_model.go); every other member, a
// Member above all, only a key for its own devices, untagged and not
// pre-approved, so the device it enrolls is one that member owns
// (network_client_owner_model.go's ownerRow).
//
// Who lists and revokes which key: a personal network's owner and a team
// member whose role, cut by its scopes, holds the keys permission the
// network's every key; every other member only the keys its own user created,
// another's key answering as no key at all. The three routes' gate asks own
// devices of every caller first (api/route_permission.go).
//
// Creating and revoking a key owe the auth key security notice
// (security_notice_auth_key.go) and an admin event (§7.11), in the change's
// transaction. A row is deleted when its key is revoked, or within the hour
// after it expires (RemoveExpiredNetworkAuthKeys), so none outlives the 90 days
// §2.8 publishes.
//
// Nothing here reads or writes network_auth_key unless MG1 is linked
// (server.SidecarMg1Linked); until then each route answers that auth keys are
// not available, and nothing is consumed.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Tunables of auth keys and of API keys' new expiry.
type NetworkAuthKeySettings struct {
	// The shortest lifetime a key may have: 5 minutes (§7.8).
	MinLifetime time.Duration
	// The longest lifetime a key may have: 90 days (§7.8), which §2.8 api-11
	// publishes, so a longer value breaks that promise.
	MaxLifetime time.Duration
	// The most uses one key may allow.
	MaxUses int
	// The most tags one key may carry.
	MaxTagCount int
	// The longest lifetime of a new API key, and the lifetime of one created
	// without an expiry: 365 days (§7.8; §2.8 api-11 publishes it).
	ApiKeyMaxLifetime time.Duration
	// The longest lifetime of a key a network OAuth client's token creates: an
	// hour (§7.8's "auth_keys' ≤1 h keys", D-TEAM-6), so CI's keys pair with its
	// Delegation and never outlive a run.
	OauthClientMaxLifetime time.Duration
}

func DefaultNetworkAuthKeySettings() *NetworkAuthKeySettings {
	return &NetworkAuthKeySettings{
		MinLifetime:            5 * time.Minute,
		MaxLifetime:            90 * 24 * time.Hour,
		MaxUses:                1000000,
		MaxTagCount:            64,
		ApiKeyMaxLifetime:      365 * 24 * time.Hour,
		OauthClientMaxLifetime: time.Hour,
	}
}

// The parts of the auth key string (§7.8).
const (
	authKeyPrefix = "ur-authkey-"
	// the client-only bundle's separator, which no bearer may hold
	authKeyBundleSeparator = "#"
	// an API key's prefix (session's apiKeyTokenPrefix), which never enrolls
	authKeyApiKeyPrefix    = "urn_"
	authKeySecretByteCount = 32
	authKeyTagIdByteCount  = 16
)

// Lowercase RFC 4648 base32 without padding: no "-", so the bearer splits once.
var authKeyEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// Why a bearer was refused. Each wraps ErrAuthKeyRefused, and none holds any of
// the bearer's text.
var (
	ErrAuthKeyRefused = errors.New("auth key refused")
	// the bearer holds a "#": a bundle that must never leave the client
	ErrAuthKeyHoldsBundle = fmt.Errorf("%w: the key's bundle never leaves the device", ErrAuthKeyRefused)
	// a urn_ API key, which never enrolls
	ErrAuthKeyIsApiKey  = fmt.Errorf("%w: an API key does not enroll", ErrAuthKeyRefused)
	ErrAuthKeyMalformed = fmt.Errorf("%w: not an auth key", ErrAuthKeyRefused)
	// no such key, or the secret does not match
	ErrAuthKeyUnknown = fmt.Errorf("%w: unknown", ErrAuthKeyRefused)
	ErrAuthKeyExpired = fmt.Errorf("%w: expired", ErrAuthKeyRefused)
	// its uses are spent
	ErrAuthKeySpent = fmt.Errorf("%w: its uses are spent", ErrAuthKeyRefused)
)

const authKeysUnavailableMessage = "Auth keys are not available yet."

// The id and secret of a bearer, ur-authkey-<id>-<secret> or <id>-<secret>.
// The refusal never holds the bearer's text.
func ParseAuthKeyBearer(bearer string) (authKeyId server.Id, secret []byte, err error) {
	if strings.Contains(bearer, authKeyBundleSeparator) {
		err = ErrAuthKeyHoldsBundle
		return
	}
	if strings.HasPrefix(bearer, authKeyApiKeyPrefix) {
		err = ErrAuthKeyIsApiKey
		return
	}
	idText, secretText, found := strings.Cut(strings.TrimPrefix(bearer, authKeyPrefix), "-")
	if !found {
		err = ErrAuthKeyMalformed
		return
	}
	idBytes, idErr := authKeyEncoding.DecodeString(idText)
	secret, secretErr := authKeyEncoding.DecodeString(secretText)
	if idErr != nil || secretErr != nil || len(idBytes) != 16 || len(secret) != authKeySecretByteCount ||
		authKeyEncoding.EncodeToString(idBytes) != idText || authKeyEncoding.EncodeToString(secret) != secretText {
		err = ErrAuthKeyMalformed
		secret = nil
		return
	}
	authKeyId, err = server.IdFromBytes(idBytes)
	if err != nil {
		err = ErrAuthKeyMalformed
		secret = nil
	}
	return
}

// The bearer a person holds for the key.
func authKeyBearer(authKeyId server.Id, secret []byte) string {
	return authKeyPrefix + authKeyEncoding.EncodeToString(authKeyId.Bytes()) + "-" + authKeyEncoding.EncodeToString(secret)
}

// The tag ids of a request: each 32 lowercase hex characters, 16 bytes that are
// not an autogroup's form (fifteen zero bytes, then the autogroup), distinct, at
// most maxTagCount.
func parseAuthKeyTagIds(tags []string, maxTagCount int) ([][]byte, error) {
	if maxTagCount < len(tags) {
		return nil, fmt.Errorf("An auth key carries at most %d tags.", maxTagCount)
	}
	tagIds := [][]byte{}
	for _, tag := range tags {
		tagId, err := hex.DecodeString(tag)
		if err != nil || len(tagId) != authKeyTagIdByteCount || hex.EncodeToString(tagId) != tag {
			return nil, errors.New("A tag is its id, 32 lowercase hex characters, never its name.")
		}
		if bytes.Equal(tagId[:authKeyTagIdByteCount-1], make([]byte, authKeyTagIdByteCount-1)) {
			return nil, errors.New("A tag id cannot name an autogroup.")
		}
		for _, other := range tagIds {
			if bytes.Equal(other, tagId) {
				return nil, errors.New("A tag is listed twice.")
			}
		}
		tagIds = append(tagIds, tagId)
	}
	return tagIds, nil
}

// An auth key as the server holds it, without its secret.
type NetworkAuthKey struct {
	AuthKeyId     server.Id
	NetworkId     server.Id
	CreatorUserId server.Id
	TagIds        [][]byte
	CreateTime    time.Time
	ExpireTime    time.Time
	MaxUses       int
	UseCount      int
	Ephemeral     bool
	PreApproved   bool
}

type CreateAuthKeyArgs struct {
	// 300 to 7,776,000: 5 minutes to 90 days
	DurationSeconds int64    `json:"duration_seconds"`
	MaxUses         int      `json:"max_uses"`
	Tags            []string `json:"tags,omitempty"`
	Ephemeral       bool     `json:"ephemeral,omitempty"`
	PreApproved     bool     `json:"pre_approved,omitempty"`
}

type CreateAuthKeyError struct {
	Message string `json:"message"`
}

type CreateAuthKeyResult struct {
	AuthKeyId *server.Id `json:"auth_key_id,omitempty"`
	// the bearer ur-authkey-<id>-<secret>, which this answer alone holds
	AuthKey    string              `json:"auth_key,omitempty"`
	ExpireTime *time.Time          `json:"expire_time,omitempty"`
	Error      *CreateAuthKeyError `json:"error,omitempty"`
}

// ur-cli's one admin command in v0 (§7.8): an admin token reaches this route,
// and its use is spent in the key's transaction.
func init() {
	session.RegisterAdminCommandRoute(http.MethodPost, "/network/auth-keys")
}

// The scope an admin token needs to create a key (§7.8's OAuth client scopes).
const authKeysAdminScope = "auth_keys"

// What a caller's credential may create at POST /network/auth-keys, and list
// and revoke at GET /network/auth-keys and POST /network/auth-key-revoke
// (§7.5).
type authKeyReach int

const (
	// no key: a member whose role cannot be read, or a role and scopes that
	// reach neither permission
	authKeyReachNone authKeyReach = iota
	// a key for the caller's own devices alone: untagged and not pre-approved;
	// and to list and revoke, the keys the caller created
	authKeyReachOwnDevices
	// beside a key for the caller's own devices, a tagged key, pre-approved or
	// not, for tags the caller's member owns (authKeyTagsOwnedBy); and to list
	// and revoke, the keys the caller created
	authKeyReachOwnedTags
	// any key the route takes, and the network's every key to list and revoke
	authKeyReachAny
)

// The refusals of a caller's role, each a 403 as the route's role gate
// answers, and the 503 of a log that cannot say who owns a tag.
const (
	authKeyRoleRefusedMessage          = "Your role in this network does not permit auth keys."
	authKeyOwnDevicesRefusedMessage    = "Your role in this network makes auth keys for your own devices only: untagged and not pre-approved."
	authKeyOwnedTagsRefusedMessage     = "Your role in this network makes auth keys for your own devices, untagged and not pre-approved, or tagged keys only for tags you own under the network's tagOwners."
	authKeyTagOwnersUnavailableMessage = "The network's log cannot say who owns its tags now."
)

// What the session's credential may create (§7.5's roles): a personal
// network's owner, who carries no member_id, whatever its credential's scopes
// reach; a team member whose role, cut by those scopes, holds
// RolePermissionKeys, any key; one whose role holds RolePermissionOwnedTagKeys
// (a Network admin's keys for owned tags), beside a key for its own devices, a
// tagged key for tags it owns, which createAuthKeyAt asks the log; one whose
// role holds only RolePermissionOwnDevices, a key for its own devices. Such a
// key is untagged, so the device it enrolls is owned by the member (ownerRow),
// never tagged; and not pre-approved, since a pre-approved key's enrollment
// lands whatever admission its bundle's Delegation signed, which may name
// another member or a tag, and a Delegation is signed only by an owner, an
// admin or a tag-owning network admin (§7.9's Delegation row), so only the
// first two reach one. A member whose role cannot be read creates none. Each
// reach but none also lists and revokes keys: the network's every key for the
// first, only the caller's own for the others (authKeyCreatorFilterOf).
func authKeyReachOf(clientSession *session.ClientSession) authKeyReach {
	scopes := clientSession.CredentialScopes()
	byJwt := clientSession.ByJwt
	permits := func(permission RolePermission) bool {
		return ScopesPermit(scopes, permission)
	}
	if byJwt.MemberId != nil {
		role, ok := MemberRole(clientSession.Ctx, byJwt.NetworkId, *byJwt.MemberId)
		if !ok {
			return authKeyReachNone
		}
		permits = func(permission RolePermission) bool {
			return RoleAndScopesPermit(role, scopes, permission)
		}
	}
	switch {
	case permits(RolePermissionKeys):
		return authKeyReachAny
	case byJwt.MemberId != nil && permits(RolePermissionOwnedTagKeys):
		return authKeyReachOwnedTags
	case permits(RolePermissionOwnDevices):
		return authKeyReachOwnDevices
	}
	return authKeyReachNone
}

// The network's verified state at the log's tip, nil before base 1: what a
// tagged key for owned tags is judged by, at its creation and at each
// enrollment. A test replaces it.
var authKeyTagOwnerState = configTipState

// Whether the network's tagOwners name memberId an owner of every tag in
// tagIds at the newest verified state of the network's log (§7.5's keys for
// owned tags), read without a Postgres connection held; an error when the log
// cannot be read, which every caller refuses on (configStateOwnsTags).
func authKeyTagsOwnedBy(ctx context.Context, networkId server.Id, memberId server.Id, tagIds [][]byte) (bool, error) {
	state, err := authKeyTagOwnerState(ctx, networkId)
	if err != nil {
		return false, err
	}
	return configStateOwnsTags(state, memberId, tagIds), nil
}

// Whether state's tagOwners name memberId an owner of every tag in tagIds:
// each tag's atom row in the state's AclRows lists the member among its
// tag_owner_member_ids, the document's tagOwners expanded to member_ids at
// compile (§8.8's atoms), and the state holds the member. A tag no row holds
// has no owner. A nil state, before base 1, and an empty tagIds own nothing,
// so no caller passes an untagged key through it.
func configStateOwnsTags(state *sidecar.ConfigState, memberId server.Id, tagIds [][]byte) bool {
	if state == nil || len(tagIds) == 0 || state.Member(sidecar.Id(memberId)) == nil {
		return false
	}
	acl, err := state.Acl()
	if err != nil {
		return false
	}
	memberIdBytes := sidecar.Id(memberId).Bytes()
	ownedTagIds := map[string]bool{}
	for _, atom := range acl.Rows().GetAtoms() {
		ownedBy := slices.ContainsFunc(atom.GetTagOwnerMemberIds(), func(ownerId []byte) bool {
			return bytes.Equal(ownerId, memberIdBytes)
		})
		if ownedBy {
			ownedTagIds[string(atom.GetAtom())] = true
		}
	}
	for _, tagId := range tagIds {
		if !ownedTagIds[string(tagId)] {
			return false
		}
	}
	return true
}

// The creator a listing or a revoke is held to (§7.5): nil, every key, for a
// reach of any key; the session's own user for one that reaches only its own
// devices or owned tags, so a member lists and revokes the keys it created and
// no one else's; false for a reach of none, which is refused.
func authKeyCreatorFilterOf(clientSession *session.ClientSession) (creatorUserId *server.Id, ok bool) {
	switch authKeyReachOf(clientSession) {
	case authKeyReachAny:
		return nil, true
	case authKeyReachOwnedTags, authKeyReachOwnDevices:
		userId := clientSession.ByJwt.UserId
		return &userId, true
	}
	return nil, false
}

// Creates an auth key for the session's network, its creator the session's
// user, and owes the notice and the admin event in the same transaction. A
// caller whose role reaches only its own devices creates only an untagged key
// that is not pre-approved; one whose role reaches owned tags that, or a
// tagged key whose every tag the network's tagOwners name its member an owner
// of at the log's newest verified state, read before the transaction, the
// read failing closed with a 503; and one whose role reaches no key none, each
// refusal a 403 (authKeyReachOf). A session an admin token authenticated
// spends the token's use in that same transaction, and a replay of the token
// answers the first use's result and creates nothing
// (admin_token_use_model.go).
//
// The owners are read once, so a save that takes the caller off a tag's
// tagOwners just after the read leaves the key made; it never enrolls, since
// each enrollment asks the owners again (network_client_owner_model.go).
func CreateAuthKey(createAuthKey *CreateAuthKeyArgs, clientSession *session.ClientSession) (*CreateAuthKeyResult, error) {
	return createAuthKeyAt(createAuthKey, clientSession, DefaultNetworkAuthKeySettings(), server.NowUtc())
}

func createAuthKeyAt(
	createAuthKey *CreateAuthKeyArgs,
	clientSession *session.ClientSession,
	settings *NetworkAuthKeySettings,
	now time.Time,
) (*CreateAuthKeyResult, error) {
	if !server.SidecarMg1Linked() {
		return nil, fmt.Errorf("%d %s", http.StatusServiceUnavailable, authKeysUnavailableMessage)
	}
	adminCommand := clientSession.AdminCommand
	if adminCommand != nil && !slices.Contains(adminCommand.Scopes, authKeysAdminScope) {
		return nil, fmt.Errorf("%d %s", http.StatusForbidden, "The admin token does not carry the auth_keys scope.")
	}
	reach := authKeyReachOf(clientSession)
	switch reach {
	case authKeyReachNone:
		return nil, fmt.Errorf("%d %s", http.StatusForbidden, authKeyRoleRefusedMessage)
	case authKeyReachOwnDevices:
		if 0 < len(createAuthKey.Tags) || createAuthKey.PreApproved {
			return nil, fmt.Errorf("%d %s", http.StatusForbidden, authKeyOwnDevicesRefusedMessage)
		}
	case authKeyReachOwnedTags:
		// an untagged key is for the caller's own devices, which is never
		// pre-approved; its tags are asked of the log below
		if len(createAuthKey.Tags) == 0 && createAuthKey.PreApproved {
			return nil, fmt.Errorf("%d %s", http.StatusForbidden, authKeyOwnedTagsRefusedMessage)
		}
	}
	refuse := func(message string) (*CreateAuthKeyResult, error) {
		return &CreateAuthKeyResult{
			Error: &CreateAuthKeyError{
				Message: message,
			},
		}, nil
	}

	lifetime := time.Duration(createAuthKey.DurationSeconds) * time.Second
	if createAuthKey.DurationSeconds <= 0 || lifetime < settings.MinLifetime || settings.MaxLifetime < lifetime ||
		int64(settings.MaxLifetime/time.Second) < createAuthKey.DurationSeconds {
		return refuse(fmt.Sprintf(
			"An auth key lives %d to %d seconds.",
			int64(settings.MinLifetime/time.Second),
			int64(settings.MaxLifetime/time.Second),
		))
	}
	if clientSession.OauthClient != nil && settings.OauthClientMaxLifetime < lifetime {
		// a CI client's keys live at most an hour (D-TEAM-6)
		return refuse(fmt.Sprintf("An OAuth client's auth key lives at most %d seconds.", int64(settings.OauthClientMaxLifetime/time.Second)))
	}
	if createAuthKey.MaxUses < 1 || settings.MaxUses < createAuthKey.MaxUses {
		return refuse(fmt.Sprintf("An auth key allows 1 to %d uses.", settings.MaxUses))
	}
	tagIds, err := parseAuthKeyTagIds(createAuthKey.Tags, settings.MaxTagCount)
	if err != nil {
		return refuse(err.Error())
	}
	if reach == authKeyReachOwnedTags && 0 < len(tagIds) {
		memberId := clientSession.ByJwt.MemberId
		if memberId == nil {
			return nil, fmt.Errorf("%d %s", http.StatusForbidden, authKeyOwnedTagsRefusedMessage)
		}
		owned, err := authKeyTagsOwnedBy(clientSession.Ctx, clientSession.ByJwt.NetworkId, *memberId, tagIds)
		if err != nil {
			return nil, fmt.Errorf("%d %s", http.StatusServiceUnavailable, authKeyTagOwnersUnavailableMessage)
		}
		if !owned {
			return nil, fmt.Errorf("%d %s", http.StatusForbidden, authKeyOwnedTagsRefusedMessage)
		}
	}

	secret := make([]byte, authKeySecretByteCount)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	secretSha256 := sha256.Sum256(secret)
	now = now.UTC().Truncate(time.Microsecond)
	authKey := &NetworkAuthKey{
		AuthKeyId:     server.NewId(),
		NetworkId:     clientSession.ByJwt.NetworkId,
		CreatorUserId: clientSession.ByJwt.UserId,
		TagIds:        tagIds,
		CreateTime:    now,
		ExpireTime:    now.Add(lifetime),
		MaxUses:       createAuthKey.MaxUses,
		Ephemeral:     createAuthKey.Ephemeral,
		PreApproved:   createAuthKey.PreApproved,
	}

	authKeyId := authKey.AuthKeyId
	expireTime := authKey.ExpireTime
	result := &CreateAuthKeyResult{
		AuthKeyId:  &authKeyId,
		AuthKey:    authKeyBearer(authKey.AuthKeyId, secret),
		ExpireTime: &expireTime,
	}
	var replayed *CreateAuthKeyResult
	server.Tx(clientSession.Ctx, func(tx server.PgTx) {
		replayed = nil
		if adminCommand != nil {
			storedResult, first, err := ClaimAdminTokenUseInTx(clientSession.Ctx, tx, adminCommand, now)
			server.Raise(err)
			if !first {
				// a replay of the token: the first use's answer, and nothing runs
				replayed = &CreateAuthKeyResult{}
				server.Raise(json.Unmarshal(storedResult, replayed))
				return
			}
		}
		server.RaisePgResult(tx.Exec(
			clientSession.Ctx,
			`
				INSERT INTO network_auth_key (
					auth_key_id,
					network_id,
					secret_sha256,
					creator_user_id,
					tag_ids,
					create_time,
					expire_time,
					max_uses,
					ephemeral,
					pre_approved
				)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			`,
			authKey.AuthKeyId,
			authKey.NetworkId,
			secretSha256[:],
			authKey.CreatorUserId,
			authKey.TagIds,
			authKey.CreateTime,
			authKey.ExpireTime,
			authKey.MaxUses,
			authKey.Ephemeral,
			authKey.PreApproved,
		))
		oweSecurityAuthKeyNoticeInTx(
			clientSession.Ctx,
			tx,
			authKey.NetworkId,
			newSecurityAuthKeyChange(clientSession.Ctx, SecurityAuthKeyCreated, authKey, now),
		)
		addAdminEventInTxAt(
			clientSession.Ctx,
			tx,
			NewSessionAdminEvent(clientSession, AdminEventActionAuthKeyCreate, AdminEventTargetAuthKey, &authKey.AuthKeyId),
			now,
		)
		if adminCommand != nil {
			resultJson, err := json.Marshal(result)
			server.Raise(err)
			server.Raise(StoreAdminTokenUseResultInTx(clientSession.Ctx, tx, adminCommand.Jti, resultJson))
		}
	})
	if replayed != nil {
		return replayed, nil
	}
	return result, nil
}

// An auth key as the listing shows it.
type PublicNetworkAuthKey struct {
	AuthKeyId server.Id `json:"auth_key_id"`
	Creator   server.Id `json:"creator"`
	// opaque tag ids, 32 lowercase hex characters each
	Tags        []string  `json:"tags"`
	CreateTime  time.Time `json:"create_time"`
	ExpireTime  time.Time `json:"expire_time"`
	Uses        int       `json:"uses"`
	MaxUses     int       `json:"max_uses"`
	Ephemeral   bool      `json:"ephemeral"`
	PreApproved bool      `json:"pre_approved"`
}

type GetNetworkAuthKeysResult struct {
	AuthKeys []*PublicNetworkAuthKey `json:"auth_keys"`
}

// The session's network's keys that have not expired, oldest first, without
// their secrets: every key for a credential whose reach is any key, and only
// the keys the session's user created for every other member (§7.5;
// authKeyCreatorFilterOf), a member whose role cannot be read refused 403.
func GetNetworkAuthKeys(clientSession *session.ClientSession) (*GetNetworkAuthKeysResult, error) {
	if !server.SidecarMg1Linked() {
		return nil, fmt.Errorf("%d %s", http.StatusServiceUnavailable, authKeysUnavailableMessage)
	}
	creatorUserId, ok := authKeyCreatorFilterOf(clientSession)
	if !ok {
		return nil, fmt.Errorf("%d %s", http.StatusForbidden, authKeyRoleRefusedMessage)
	}
	creatorClause := ""
	queryArgs := []any{clientSession.ByJwt.NetworkId, server.NowUtc()}
	if creatorUserId != nil {
		creatorClause = "AND creator_user_id = $3"
		queryArgs = append(queryArgs, *creatorUserId)
	}
	authKeys := []*PublicNetworkAuthKey{}
	server.Db(clientSession.Ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			clientSession.Ctx,
			`
				SELECT
					auth_key_id,
					creator_user_id,
					tag_ids,
					create_time,
					expire_time,
					use_count,
					max_uses,
					ephemeral,
					pre_approved
				FROM network_auth_key
				WHERE network_id = $1 AND $2 < expire_time `+creatorClause+`
				ORDER BY auth_key_id
			`,
			queryArgs...,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				authKey := &PublicNetworkAuthKey{}
				var tagIds [][]byte
				server.Raise(result.Scan(
					&authKey.AuthKeyId,
					&authKey.Creator,
					&tagIds,
					&authKey.CreateTime,
					&authKey.ExpireTime,
					&authKey.Uses,
					&authKey.MaxUses,
					&authKey.Ephemeral,
					&authKey.PreApproved,
				))
				authKey.Tags = []string{}
				for _, tagId := range tagIds {
					authKey.Tags = append(authKey.Tags, hex.EncodeToString(tagId))
				}
				authKeys = append(authKeys, authKey)
			}
		})
	})
	return &GetNetworkAuthKeysResult{
		AuthKeys: authKeys,
	}, nil
}

type RevokeAuthKeyArgs struct {
	AuthKeyId server.Id `json:"auth_key_id"`
}

type RevokeAuthKeyError struct {
	Message string `json:"message"`
}

type RevokeAuthKeyResult struct {
	// a pre-approved key's bundle keeps admitting devices until its Delegation
	// ends: revoking the key here stops only its bearer
	BundleStillAdmits bool `json:"bundle_still_admits"`
	// what to do about the bundle, for a pre-approved key
	Note  string              `json:"note,omitempty"`
	Error *RevokeAuthKeyError `json:"error,omitempty"`
}

// What a revoke of a pre-approved key answers (D-TEAM-31).
const revokeAuthKeyBundleNote = "Revoking stops this key's bearer only. A copy of its bundle still admits devices until its Delegation ends: run `ur admin delegations revoke` on an admin device to end it."

// Revokes one of the session's network's keys: its row goes, so its bearer
// enrolls nothing more. Owes the notice and the admin event in the same
// transaction. A credential whose reach is any key revokes any of the
// network's keys; every other member only one its user created, another's
// answering as no such key, so the answer tells nothing of whose keys exist
// (§7.5; authKeyCreatorFilterOf). A member whose role cannot be read is
// refused 403.
func RevokeAuthKey(revokeAuthKey *RevokeAuthKeyArgs, clientSession *session.ClientSession) (*RevokeAuthKeyResult, error) {
	if !server.SidecarMg1Linked() {
		return nil, fmt.Errorf("%d %s", http.StatusServiceUnavailable, authKeysUnavailableMessage)
	}
	creatorUserId, ok := authKeyCreatorFilterOf(clientSession)
	if !ok {
		return nil, fmt.Errorf("%d %s", http.StatusForbidden, authKeyRoleRefusedMessage)
	}
	creatorClause := ""
	queryArgs := []any{revokeAuthKey.AuthKeyId, clientSession.ByJwt.NetworkId}
	if creatorUserId != nil {
		creatorClause = "AND creator_user_id = $3"
		queryArgs = append(queryArgs, *creatorUserId)
	}
	var revoked *NetworkAuthKey
	server.Tx(clientSession.Ctx, func(tx server.PgTx) {
		revoked = nil
		now := server.NowUtc()
		result, err := tx.Query(
			clientSession.Ctx,
			`
				DELETE FROM network_auth_key
				WHERE auth_key_id = $1 AND network_id = $2 `+creatorClause+`
				RETURNING
					auth_key_id,
					network_id,
					creator_user_id,
					tag_ids,
					create_time,
					expire_time,
					max_uses,
					use_count,
					ephemeral,
					pre_approved
			`,
			queryArgs...,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				revoked = &NetworkAuthKey{}
				server.Raise(result.Scan(
					&revoked.AuthKeyId,
					&revoked.NetworkId,
					&revoked.CreatorUserId,
					&revoked.TagIds,
					&revoked.CreateTime,
					&revoked.ExpireTime,
					&revoked.MaxUses,
					&revoked.UseCount,
					&revoked.Ephemeral,
					&revoked.PreApproved,
				))
			}
		})
		if revoked == nil {
			return
		}
		oweSecurityAuthKeyNoticeInTx(
			clientSession.Ctx,
			tx,
			revoked.NetworkId,
			newSecurityAuthKeyChange(clientSession.Ctx, SecurityAuthKeyRevoked, revoked, now),
		)
		addAdminEventInTxAt(
			clientSession.Ctx,
			tx,
			NewSessionAdminEvent(clientSession, AdminEventActionAuthKeyRevoke, AdminEventTargetAuthKey, &revoked.AuthKeyId),
			now,
		)
	})
	if revoked == nil {
		return &RevokeAuthKeyResult{
			Error: &RevokeAuthKeyError{
				Message: "No such auth key.",
			},
		}, nil
	}
	result := &RevokeAuthKeyResult{
		BundleStillAdmits: revoked.PreApproved,
	}
	if revoked.PreApproved {
		result.Note = revokeAuthKeyBundleNote
	}
	return result, nil
}

// Spends one use of the bearer's key in the caller's transaction, which is the
// enrollment's (TEAM-M1-04), and returns the key. The key's row is locked, so of
// two enrollments racing for a key's last use one wins and the other is refused
// with ErrAuthKeySpent, under either isolation level. Every refusal wraps
// ErrAuthKeyRefused and holds none of the bearer's text; the bearer must not
// be logged by the caller either.
func ConsumeAuthKeyInTx(ctx context.Context, tx server.PgTx, bearer string) (*NetworkAuthKey, error) {
	return consumeAuthKeyInTxAt(ctx, tx, bearer, server.NowUtc())
}

func consumeAuthKeyInTxAt(ctx context.Context, tx server.PgTx, bearer string, now time.Time) (*NetworkAuthKey, error) {
	authKey, err := readAuthKeyBearer(ctx, tx, bearer, now, true)
	if err != nil {
		return nil, err
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`UPDATE network_auth_key SET use_count = use_count + 1 WHERE auth_key_id = $1`,
		authKey.AuthKeyId,
	))
	authKey.UseCount += 1
	return authKey, nil
}

// Checks the bearer's key as ConsumeAuthKeyInTx does, spending nothing: what
// an enrollment's admission checks before it appends (TEAM-M2-16), since a
// statement in the log cannot roll back with the mint. A key that passes here
// may still be refused at the mint, by a racing enrollment that spent its last
// use. Every refusal wraps ErrAuthKeyRefused and holds none of the bearer's
// text.
func PeekAuthKey(ctx context.Context, bearer string) (authKey *NetworkAuthKey, returnErr error) {
	server.Db(ctx, func(conn server.PgConn) {
		authKey, returnErr = readAuthKeyBearer(ctx, conn, bearer, server.NowUtc(), false)
	})
	return
}

// The bearer's key when its secret matches, it is unexpired at now and it has
// a use left; with forUpdate, its row locked in the caller's transaction.
func readAuthKeyBearer(ctx context.Context, conn server.PgCanQuery, bearer string, now time.Time, forUpdate bool) (*NetworkAuthKey, error) {
	authKeyId, secret, err := ParseAuthKeyBearer(bearer)
	if err != nil {
		return nil, err
	}
	if !server.SidecarMg1Linked() {
		// no key exists in a binary without MG1, so none admits
		return nil, ErrAuthKeyUnknown
	}
	lock := ""
	if forUpdate {
		lock = "FOR UPDATE"
	}
	var authKey *NetworkAuthKey
	var secretSha256 []byte
	result, err := conn.Query(
		ctx,
		`
			SELECT
				auth_key_id,
				network_id,
				secret_sha256,
				creator_user_id,
				tag_ids,
				create_time,
				expire_time,
				max_uses,
				use_count,
				ephemeral,
				pre_approved
			FROM network_auth_key
			WHERE auth_key_id = $1
		`+lock,
		authKeyId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			authKey = &NetworkAuthKey{}
			server.Raise(result.Scan(
				&authKey.AuthKeyId,
				&authKey.NetworkId,
				&secretSha256,
				&authKey.CreatorUserId,
				&authKey.TagIds,
				&authKey.CreateTime,
				&authKey.ExpireTime,
				&authKey.MaxUses,
				&authKey.UseCount,
				&authKey.Ephemeral,
				&authKey.PreApproved,
			))
		}
	})
	if authKey == nil {
		return nil, ErrAuthKeyUnknown
	}
	presentedSha256 := sha256.Sum256(secret)
	if subtle.ConstantTimeCompare(presentedSha256[:], secretSha256) != 1 {
		return nil, ErrAuthKeyUnknown
	}
	if !now.Before(authKey.ExpireTime) {
		return nil, ErrAuthKeyExpired
	}
	if authKey.MaxUses <= authKey.UseCount {
		return nil, ErrAuthKeySpent
	}
	return authKey, nil
}

// Removes the keys that expired before minTime, oldest first, for at most the
// retention run budget.
func RemoveExpiredNetworkAuthKeys(ctx context.Context, minTime time.Time) (removedCount int64) {
	if !server.SidecarMg1Linked() {
		return 0
	}
	removedCount, _ = networkAuthKeyRetention.RemoveBefore(ctx, minTime, retentionBatchSize, time.Now().Add(retentionRunBudget))
	return
}

// network_auth_key is keyed by its id, its expiry indexed.
var networkAuthKeyRetention = newIndexedRetention(RetentionTableNetworkAuthKey, "auth_key_id", "expire_time")
