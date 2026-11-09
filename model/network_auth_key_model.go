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
}

func DefaultNetworkAuthKeySettings() *NetworkAuthKeySettings {
	return &NetworkAuthKeySettings{
		MinLifetime:       5 * time.Minute,
		MaxLifetime:       90 * 24 * time.Hour,
		MaxUses:           1000000,
		MaxTagCount:       64,
		ApiKeyMaxLifetime: 365 * 24 * time.Hour,
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

// Creates an auth key for the session's network, its creator the session's
// user, and owes the notice and the admin event in the same transaction. A
// session an admin token authenticated spends the token's use in that same
// transaction, and a replay of the token answers the first use's result and
// creates nothing (admin_token_use_model.go).
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
	if createAuthKey.MaxUses < 1 || settings.MaxUses < createAuthKey.MaxUses {
		return refuse(fmt.Sprintf("An auth key allows 1 to %d uses.", settings.MaxUses))
	}
	tagIds, err := parseAuthKeyTagIds(createAuthKey.Tags, settings.MaxTagCount)
	if err != nil {
		return refuse(err.Error())
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
// their secrets.
func GetNetworkAuthKeys(clientSession *session.ClientSession) (*GetNetworkAuthKeysResult, error) {
	if !server.SidecarMg1Linked() {
		return nil, fmt.Errorf("%d %s", http.StatusServiceUnavailable, authKeysUnavailableMessage)
	}
	authKeys := []*PublicNetworkAuthKey{}
	now := server.NowUtc()
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
				WHERE network_id = $1 AND $2 < expire_time
				ORDER BY auth_key_id
			`,
			clientSession.ByJwt.NetworkId,
			now,
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
// transaction.
func RevokeAuthKey(revokeAuthKey *RevokeAuthKeyArgs, clientSession *session.ClientSession) (*RevokeAuthKeyResult, error) {
	if !server.SidecarMg1Linked() {
		return nil, fmt.Errorf("%d %s", http.StatusServiceUnavailable, authKeysUnavailableMessage)
	}
	var revoked *NetworkAuthKey
	server.Tx(clientSession.Ctx, func(tx server.PgTx) {
		revoked = nil
		now := server.NowUtc()
		result, err := tx.Query(
			clientSession.Ctx,
			`
				DELETE FROM network_auth_key
				WHERE auth_key_id = $1 AND network_id = $2
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
			revokeAuthKey.AuthKeyId,
			clientSession.ByJwt.NetworkId,
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
	authKeyId, secret, err := ParseAuthKeyBearer(bearer)
	if err != nil {
		return nil, err
	}
	if !server.SidecarMg1Linked() {
		// no key exists in a binary without MG1, so none admits
		return nil, ErrAuthKeyUnknown
	}
	var authKey *NetworkAuthKey
	var secretSha256 []byte
	result, err := tx.Query(
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
			FOR UPDATE
		`,
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
	server.RaisePgResult(tx.Exec(
		ctx,
		`UPDATE network_auth_key SET use_count = use_count + 1 WHERE auth_key_id = $1`,
		authKey.AuthKeyId,
	))
	authKey.UseCount += 1
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
