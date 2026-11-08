//go:build sidecar_pending

package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/apikey"
	"github.com/urnetwork/server/session"
)

// §7.8's auth keys and API-key scopes and expiry, which need MG0 and MG1.

const testAuthKeyNoticeTemplate = "security_auth_key_changed"

// Creates an auth key with the session at now, failing the test on a refusal.
func createTestAuthKey(t testing.TB, clientSession *session.ClientSession, args *CreateAuthKeyArgs, now time.Time) *CreateAuthKeyResult {
	t.Helper()
	result, err := createAuthKeyAt(args, clientSession, DefaultNetworkAuthKeySettings(), now)
	if err != nil || result.Error != nil || result.AuthKey == "" || result.AuthKeyId == nil {
		t.Fatalf("create auth key: %v %+v", err, result)
	}
	return result
}

// Spends one use of the bearer in a transaction of its own at now.
func consumeTestAuthKey(ctx context.Context, bearer string, now time.Time) (authKey *NetworkAuthKey, err error) {
	server.Tx(ctx, func(tx server.PgTx) {
		authKey, err = consumeAuthKeyInTxAt(ctx, tx, bearer, now)
	})
	return
}

// The bearer has the format §7.8 gives, parses back to the key's id, and
// consumes once with every field the key was made with.
func TestAuthKeyBearerFormat(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, _, clientSession := authParityAccount(t, ctx, "authkey-format")
		now := server.NowUtc()
		tagId := strings.Repeat("ab", 16)
		created := createTestAuthKey(t, clientSession, &CreateAuthKeyArgs{
			DurationSeconds: 3600,
			MaxUses:         2,
			Tags:            []string{tagId},
			Ephemeral:       true,
			PreApproved:     true,
		}, now)

		if !strings.HasPrefix(created.AuthKey, "ur-authkey-") || len(created.AuthKey) != len("ur-authkey-")+26+1+52 {
			t.Fatalf("bearer has the wrong shape (%d characters)", len(created.AuthKey))
		}
		authKeyId, secret, err := ParseAuthKeyBearer(created.AuthKey)
		if err != nil || authKeyId != *created.AuthKeyId || len(secret) != 32 {
			t.Fatalf("parse: %s %v", authKeyId, err)
		}
		// enroll sends <id>-<secret>, without the prefix
		if parsedId, _, err := ParseAuthKeyBearer(strings.TrimPrefix(created.AuthKey, "ur-authkey-")); err != nil || parsedId != authKeyId {
			t.Fatalf("parse without the prefix: %s %v", parsedId, err)
		}

		authKey, err := consumeTestAuthKey(ctx, created.AuthKey, now)
		if err != nil {
			t.Fatal(err)
		}
		if authKey.NetworkId != clientSession.ByJwt.NetworkId || authKey.CreatorUserId != clientSession.ByJwt.UserId ||
			!authKey.Ephemeral || !authKey.PreApproved || authKey.MaxUses != 2 || authKey.UseCount != 1 ||
			len(authKey.TagIds) != 1 || hex.EncodeToString(authKey.TagIds[0]) != tagId {
			t.Fatalf("consumed %+v", authKey)
		}

		// only the secret's hash is stored
		server.Db(ctx, func(conn server.PgConn) {
			var row string
			server.Raise(conn.QueryRow(ctx, `SELECT row_to_json(network_auth_key)::text FROM network_auth_key WHERE auth_key_id = $1`, authKeyId).Scan(&row))
			secretSha256 := sha256.Sum256(secret)
			_, secretText, _ := strings.Cut(strings.TrimPrefix(created.AuthKey, "ur-authkey-"), "-")
			if strings.Contains(row, secretText) || strings.Contains(row, hex.EncodeToString(secret)) {
				t.Fatalf("the row holds the secret: %s", row)
			}
			if !strings.Contains(row, hex.EncodeToString(secretSha256[:])) {
				t.Fatalf("the row does not hold the secret's hash: %s", row)
			}
		})
	})
}

// Two enrollers that both read the key before either spends it, on a barrier:
// one wins its single use and the other is refused as spent.
func TestAuthKeySingleUse(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, _, clientSession := authParityAccount(t, ctx, "authkey-single")
		now := server.NowUtc()
		created := createTestAuthKey(t, clientSession, &CreateAuthKeyArgs{DurationSeconds: 300, MaxUses: 1}, now)

		enrollerCount := 2
		arrived := sync.WaitGroup{}
		arrived.Add(enrollerCount)
		release := make(chan struct{})
		done := sync.WaitGroup{}
		done.Add(enrollerCount)
		errs := make([]error, enrollerCount)
		panics := make([]any, enrollerCount)
		for i := range enrollerCount {
			go func() {
				defer done.Done()
				defer func() {
					panics[i] = recover()
				}()
				options := []any{}
				if i == 1 {
					options = append(options, server.TxReadCommitted)
				}
				attempt := 0
				server.Tx(ctx, func(tx server.PgTx) {
					attempt += 1
					// both see the unspent key before either spends it
					var useCount int
					server.Raise(tx.QueryRow(ctx, `SELECT use_count FROM network_auth_key WHERE auth_key_id = $1`, *created.AuthKeyId).Scan(&useCount))
					if attempt == 1 {
						if useCount != 0 {
							panic(errors.New("the key was spent before the barrier"))
						}
						arrived.Done()
						<-release
					}
					_, errs[i] = consumeAuthKeyInTxAt(ctx, tx, created.AuthKey, now)
				}, options...)
			}()
		}
		arrived.Wait()
		close(release)
		done.Wait()
		for i := range enrollerCount {
			if panics[i] != nil {
				t.Fatalf("enroller %d failed: %v", i, panics[i])
			}
		}
		wins, spent := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				wins += 1
			case errors.Is(err, ErrAuthKeySpent):
				spent += 1
			default:
				t.Fatalf("an enroller was refused for another reason: %v", err)
			}
		}
		if wins != 1 || spent != 1 {
			t.Fatalf("%d won and %d were refused as spent, want one each", wins, spent)
		}
		var useCount int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT use_count FROM network_auth_key WHERE auth_key_id = $1`, *created.AuthKeyId).Scan(&useCount))
		})
		if useCount != 1 {
			t.Fatalf("use_count = %d", useCount)
		}
	})
}

// A key's lifetime bounds, 5 minutes and 90 days, each accepted at the bound
// and refused a second past it; a key consumes a moment before it expires and
// not at its expiry.
func TestAuthKeyLifetimeBounds(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, _, clientSession := authParityAccount(t, ctx, "authkey-bounds")
		now := server.NowUtc()
		settings := DefaultNetworkAuthKeySettings()
		for _, c := range []struct {
			durationSeconds int64
			accepted        bool
		}{
			{299, false},
			{300, true},
			{7776000, true},
			{7776001, false},
			{0, false},
			{-1, false},
			{1 << 62, false},
		} {
			result, err := createAuthKeyAt(&CreateAuthKeyArgs{DurationSeconds: c.durationSeconds, MaxUses: 1}, clientSession, settings, now)
			if err != nil {
				t.Fatal(err)
			}
			if (result.Error == nil) != c.accepted {
				t.Fatalf("%d seconds: accepted %t, want %t (%+v)", c.durationSeconds, result.Error == nil, c.accepted, result.Error)
			}
			if c.accepted && !result.ExpireTime.Equal(now.Add(time.Duration(c.durationSeconds)*time.Second)) {
				t.Fatalf("%d seconds: expires %s", c.durationSeconds, result.ExpireTime)
			}
		}
		for _, maxUses := range []int{0, -1, settings.MaxUses + 1} {
			result, err := createAuthKeyAt(&CreateAuthKeyArgs{DurationSeconds: 300, MaxUses: maxUses}, clientSession, settings, now)
			if err != nil || result.Error == nil {
				t.Fatalf("%d uses: accepted %+v %v", maxUses, result, err)
			}
		}

		created := createTestAuthKey(t, clientSession, &CreateAuthKeyArgs{DurationSeconds: 600, MaxUses: 5}, now)
		expireTime := now.Add(600 * time.Second)
		if _, err := consumeTestAuthKey(ctx, created.AuthKey, expireTime.Add(-time.Microsecond)); err != nil {
			t.Fatalf("a moment before expiry: %v", err)
		}
		if _, err := consumeTestAuthKey(ctx, created.AuthKey, expireTime); !errors.Is(err, ErrAuthKeyExpired) {
			t.Fatalf("at expiry: %v", err)
		}

		// the hourly expiry removes a key once it has expired, and not before:
		// at its expiry only the bound loop's 300-second key goes
		if removed := RemoveExpiredNetworkAuthKeys(ctx, expireTime); removed != 1 {
			t.Fatalf("removed %d keys at the 600-second key's expiry, want the 300-second key", removed)
		}
		if _, err := consumeTestAuthKey(ctx, created.AuthKey, now); err != nil {
			t.Fatalf("a key at its expiry was removed: %v", err)
		}
		if removed := RemoveExpiredNetworkAuthKeys(ctx, expireTime.Add(time.Microsecond)); removed != 1 {
			t.Fatalf("removed %d keys past their expiry", removed)
		}
		if _, err := consumeTestAuthKey(ctx, created.AuthKey, now); !errors.Is(err, ErrAuthKeyUnknown) {
			t.Fatalf("a removed key: %v", err)
		}
	})
}

// A bearer holding a bundle, an API key, a wrong secret, a malformed string and
// an unknown key are each refused, and no refusal holds the bearer's text.
func TestAuthKeyRefusals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, _, clientSession := authParityAccount(t, ctx, "authkey-refusals")
		now := server.NowUtc()
		created := createTestAuthKey(t, clientSession, &CreateAuthKeyArgs{DurationSeconds: 300, MaxUses: 5}, now)
		idText, secretText, _ := strings.Cut(strings.TrimPrefix(created.AuthKey, "ur-authkey-"), "-")
		otherSecret := strings.Repeat("a", 52)
		apiKey, err := apikey.Testing_CreateApiKey(clientSession.ByJwt.NetworkId, ctx)
		if err != nil {
			t.Fatal(err)
		}

		for _, c := range []struct {
			bearer string
			want   error
		}{
			{created.AuthKey + "#" + strings.Repeat("Q", 40), ErrAuthKeyHoldsBundle},
			{"#" + created.AuthKey, ErrAuthKeyHoldsBundle},
			{apiKey.ApiKey, ErrAuthKeyIsApiKey},
			{"ur-authkey-" + idText + "-" + otherSecret, ErrAuthKeyUnknown},
			{"ur-authkey-" + strings.Repeat("a", 26) + "-" + secretText, ErrAuthKeyUnknown},
			{"ur-authkey-" + idText, ErrAuthKeyMalformed},
			{"ur-authkey-" + strings.ToUpper(idText) + "-" + secretText, ErrAuthKeyMalformed},
			{"ur-authkey-" + idText + "-" + secretText + "-", ErrAuthKeyMalformed},
			{"", ErrAuthKeyMalformed},
		} {
			_, err := consumeTestAuthKey(ctx, c.bearer, now)
			if !errors.Is(err, c.want) || !errors.Is(err, ErrAuthKeyRefused) {
				t.Fatalf("bearer refused with %v, want %v", err, c.want)
			}
			for _, part := range []string{idText, secretText, apiKey.ApiKey} {
				if strings.Contains(err.Error(), part) {
					t.Fatalf("a refusal holds the bearer: %v", err)
				}
			}
		}
		// none of those spent a use
		authKey, err := consumeTestAuthKey(ctx, created.AuthKey, now)
		if err != nil || authKey.UseCount != 1 {
			t.Fatalf("after the refusals: %+v %v", authKey, err)
		}

		// tags are ids, never names
		for _, tags := range [][]string{
			{"tag:ci"},
			{strings.Repeat("AB", 16)},
			{strings.Repeat("ab", 15)},
			{strings.Repeat("00", 15) + "0e"},
			{strings.Repeat("ab", 16), strings.Repeat("ab", 16)},
		} {
			result, err := createAuthKeyAt(&CreateAuthKeyArgs{DurationSeconds: 300, MaxUses: 1, Tags: tags}, clientSession, DefaultNetworkAuthKeySettings(), now)
			if err != nil || result.Error == nil {
				t.Fatalf("tags %v accepted: %+v %v", tags, result, err)
			}
		}
	})
}

// Revoking stops the bearer; a pre-approved key's answer says its bundle still
// admits and names ur admin delegations revoke, and a plain key's does not. A
// key of another network, or one already revoked, is not revoked.
func TestAuthKeyRevoke(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, _, clientSession := authParityAccount(t, ctx, "authkey-revoke")
		_, _, otherSession := authParityAccount(t, ctx, "authkey-revoke-other")
		now := server.NowUtc()
		plain := createTestAuthKey(t, clientSession, &CreateAuthKeyArgs{DurationSeconds: 300, MaxUses: 5}, now)
		preApproved := createTestAuthKey(t, clientSession, &CreateAuthKeyArgs{DurationSeconds: 300, MaxUses: 5, PreApproved: true}, now)

		if result, err := RevokeAuthKey(&RevokeAuthKeyArgs{AuthKeyId: *plain.AuthKeyId}, otherSession); err != nil || result.Error == nil {
			t.Fatalf("another network revoked the key: %+v %v", result, err)
		}

		result, err := RevokeAuthKey(&RevokeAuthKeyArgs{AuthKeyId: *plain.AuthKeyId}, clientSession)
		if err != nil || result.Error != nil || result.BundleStillAdmits || result.Note != "" {
			t.Fatalf("revoke plain: %+v %v", result, err)
		}
		if _, err := consumeTestAuthKey(ctx, plain.AuthKey, now); !errors.Is(err, ErrAuthKeyUnknown) {
			t.Fatalf("a revoked bearer: %v", err)
		}
		if again, err := RevokeAuthKey(&RevokeAuthKeyArgs{AuthKeyId: *plain.AuthKeyId}, clientSession); err != nil || again.Error == nil {
			t.Fatalf("a second revoke: %+v %v", again, err)
		}

		result, err = RevokeAuthKey(&RevokeAuthKeyArgs{AuthKeyId: *preApproved.AuthKeyId}, clientSession)
		if err != nil || result.Error != nil || !result.BundleStillAdmits || !strings.Contains(result.Note, "ur admin delegations revoke") {
			t.Fatalf("revoke pre-approved: %+v %v", result, err)
		}

		listed, err := GetNetworkAuthKeys(clientSession)
		if err != nil || len(listed.AuthKeys) != 0 {
			t.Fatalf("listed after revoking both: %+v %v", listed, err)
		}

		// each change recorded an admin event, chained
		actions := []string{}
		for _, event := range GetNetworkAdminEvents(ctx, clientSession.ByJwt.NetworkId, 0, 0) {
			actions = append(actions, event.Action)
		}
		want := []string{AdminEventActionAuthKeyCreate, AdminEventActionAuthKeyCreate, AdminEventActionAuthKeyRevoke, AdminEventActionAuthKeyRevoke}
		if strings.Join(actions, ",") != strings.Join(want, ",") {
			t.Fatalf("admin events %v, want %v", actions, want)
		}
		if err := VerifyNetworkAdminEventChain(ctx, clientSession.ByJwt.NetworkId); err != nil {
			t.Fatal(err)
		}
	})
}

// The listing shows a network's live keys without their secrets, and only its
// own.
func TestAuthKeyListing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, _, clientSession := authParityAccount(t, ctx, "authkey-list")
		_, _, otherSession := authParityAccount(t, ctx, "authkey-list-other")
		now := server.NowUtc()
		created := createTestAuthKey(t, clientSession, &CreateAuthKeyArgs{DurationSeconds: 3600, MaxUses: 3, Tags: []string{strings.Repeat("cd", 16)}}, now)
		createTestAuthKey(t, otherSession, &CreateAuthKeyArgs{DurationSeconds: 3600, MaxUses: 3}, now)

		listed, err := GetNetworkAuthKeys(clientSession)
		if err != nil || len(listed.AuthKeys) != 1 {
			t.Fatalf("listed %+v %v", listed, err)
		}
		key := listed.AuthKeys[0]
		if key.AuthKeyId != *created.AuthKeyId || key.Creator != clientSession.ByJwt.UserId || key.MaxUses != 3 || key.Uses != 0 ||
			len(key.Tags) != 1 || key.Tags[0] != strings.Repeat("cd", 16) || !key.ExpireTime.Equal(now.Add(time.Hour)) {
			t.Fatalf("listed %+v", key)
		}
	})
}

// Creating keys owes the auth key notice in the change's transaction, two
// creations inside the window join one email, and no notice holds the key, its
// id or its tags.
func TestAuthKeyNoticeOwedAndJoined(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, userAuth, clientSession := authParityAccount(t, ctx, "authkey-notice")
		networkId := clientSession.ByJwt.NetworkId
		now := server.NowUtc()
		tagId := strings.Repeat("ef", 16)
		first := createTestAuthKey(t, clientSession, &CreateAuthKeyArgs{DurationSeconds: 300, MaxUses: 1, Tags: []string{tagId}}, now)
		notices := unsentTestNotices(t, ctx, networkId, testAuthKeyNoticeTemplate)
		if len(notices) != 1 || !strings.EqualFold(notices[0].userAuth, userAuth) || notices[0].deliverTime == nil {
			t.Fatalf("%d notices after one creation, want one due to %s", len(notices), userAuth)
		}
		second := createTestAuthKey(t, clientSession, &CreateAuthKeyArgs{DurationSeconds: 600, MaxUses: 2}, now)

		notices = unsentTestNotices(t, ctx, networkId, testAuthKeyNoticeTemplate)
		if len(notices) != 1 {
			t.Fatalf("%d notices, want the second change joined to the first", len(notices))
		}
		changes := notices[0].changes(t)
		if len(changes) != 2 {
			t.Fatalf("the notice lists %d changes: %s", len(changes), notices[0].templateJson)
		}
		for i, want := range []map[string]string{
			{"action": "created", "max_uses": "1", "tag_count": "1"},
			{"action": "created", "max_uses": "2", "tag_count": "<nil>"},
		} {
			for key, value := range want {
				if got := fmt.Sprint(changes[i][key]); got != value {
					t.Fatalf("change %d %s = %s, want %s: %s", i, key, got, value, notices[0].templateJson)
				}
			}
		}
		for _, created := range []*CreateAuthKeyResult{first, second} {
			idText, secretText, _ := strings.Cut(strings.TrimPrefix(created.AuthKey, "ur-authkey-"), "-")
			for _, secret := range []string{created.AuthKey, idText, secretText, created.AuthKeyId.String(), tagId} {
				if strings.Contains(notices[0].templateJson, secret) {
					t.Fatalf("the notice holds %q: %s", secret, notices[0].templateJson)
				}
			}
		}
	})
}

// An API key gains scopes, expiry and creator: a new key is all-scoped and
// expires within 365 days, a narrower scope is refused rather than granted as
// all, an expired key authenticates nothing, and a legacy key, with no scopes
// and no expiry, keeps working.
func TestApiKeyScopesAndExpiry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, _, clientSession := authParityAccount(t, ctx, "apikey-scopes")
		networkId := clientSession.ByJwt.NetworkId

		before := server.NowUtc()
		created, err := CreateApiKey(&CreateApiKeyArgs{Name: "deploy-bot"}, clientSession)
		if err != nil || created.Error != nil {
			t.Fatalf("create: %+v %v", created, err)
		}
		if strings.Join(created.Scopes, ",") != ApiKeyScopeAll || created.ExpireTime == nil ||
			created.ExpireTime.Before(before.Add(365*24*time.Hour)) || server.NowUtc().Add(365*24*time.Hour).Before(*created.ExpireTime) {
			t.Fatalf("a new key has scopes %v and expiry %v", created.Scopes, created.ExpireTime)
		}
		if network := apikey.GetNetworkByApiKey(created.ApiKey, ctx); network == nil || network.NetworkId != networkId {
			t.Fatalf("a new key does not authenticate: %+v", network)
		}
		listed := testApiKeysOf(t, clientSession)[created.Id]
		if listed == nil || listed.Creator == nil || *listed.Creator != clientSession.ByJwt.UserId || listed.ExpireTime == nil {
			t.Fatalf("listed %+v", listed)
		}

		for _, args := range []*CreateApiKeyArgs{
			{Name: "narrow", Scopes: []string{ApiKeyScopeAuthKeys}},
			{Name: "unknown", Scopes: []string{"everything"}},
			{Name: "too-long", DurationSeconds: func() *int64 { d := int64(365*24*3600 + 1); return &d }()},
			{Name: "zero", DurationSeconds: func() *int64 { d := int64(0); return &d }()},
		} {
			result, err := CreateApiKey(args, clientSession)
			if err != nil || result.Error == nil {
				t.Fatalf("%s accepted: %+v %v", args.Name, result, err)
			}
		}

		// an expired key is no key, and the hourly expiry removes it with its codes
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE account_api_key SET expire_time = $2 WHERE api_key_id = $1`, created.Id, server.NowUtc().Add(-time.Second)))
		})
		if network := apikey.GetNetworkByApiKey(created.ApiKey, ctx); network != nil {
			t.Fatalf("an expired key authenticates: %+v", network)
		}
		if _, listed := testApiKeysOf(t, clientSession)[created.Id]; listed {
			t.Fatal("an expired key is listed")
		}
		if removed := RemoveExpiredApiKeys(ctx, server.NowUtc()); removed != 1 {
			t.Fatalf("removed %d expired keys", removed)
		}

		// a legacy key, as the release before MG0 writes it, keeps its reach
		legacy, err := apikey.Testing_CreateApiKey(networkId, ctx)
		if err != nil {
			t.Fatal(err)
		}
		if network := apikey.GetNetworkByApiKey(legacy.ApiKey, ctx); network == nil || network.NetworkId != networkId {
			t.Fatalf("a legacy key does not authenticate: %+v", network)
		}
		if removed := RemoveExpiredApiKeys(ctx, server.NowUtc().Add(1000*24*time.Hour)); removed != 0 {
			t.Fatalf("the expiry removed %d legacy keys", removed)
		}
		if listed := testApiKeysOf(t, clientSession)[legacy.Id]; listed == nil || listed.Scopes != nil || listed.ExpireTime != nil || listed.Creator != nil {
			t.Fatalf("a legacy key lists as %+v", listed)
		}
	})
}
