// The API key security notice against the database (owner decision
// 2026-10-29, "Yes, API keys too"): creating or deleting an API key owes one
// email notice to the admin's sign-in email addresses, in the change's own
// transaction, naming the key and never holding it; changes inside the window
// join one notice that keeps its due time; a notice that cannot be owed never
// fails the change; and the notice goes by email only.
//
// Like security_notice_db_test.go, these reach the changes through the entry
// points the routes call and read the outbox by template name, so they hold the
// behavior apart from the notice code that implements it.
package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// The API key notice's outbox template and its metric label.
const (
	testApiKeyNoticeTemplate = "security_api_key_changed"
	testApiKeyNoticeLabel    = "api_key"
)

// Creates an API key with the session, failing the test on a refusal.
func createTestApiKey(t testing.TB, clientSession *session.ClientSession, name string) *CreateApiKeyResult {
	t.Helper()
	result, err := CreateApiKey(&CreateApiKeyArgs{Name: name}, clientSession)
	if err != nil || result == nil || result.Error != nil || result.ApiKey == "" {
		t.Fatalf("create api key %q: %v %+v", name, err, result)
	}
	return result
}

// The session network's API keys, by id.
func testApiKeysOf(t testing.TB, clientSession *session.ClientSession) map[server.Id]*PublicAccountApiKey {
	t.Helper()
	apiKeys, err := GetAccountApiKeys(clientSession)
	if err != nil {
		t.Fatal(err)
	}
	byId := map[server.Id]*PublicAccountApiKey{}
	for _, apiKey := range apiKeys {
		byId[apiKey.Id] = apiKey
	}
	return byId
}

// Asserts the network owes no unsent API key notice.
func requireNoTestApiKeyNotice(t testing.TB, ctx context.Context, networkId server.Id, what string) {
	t.Helper()
	if notices := unsentTestNotices(t, ctx, networkId, testApiKeyNoticeTemplate); len(notices) != 0 {
		t.Fatalf("%s owed %d API key notices, want none: %s", what, len(notices), notices[0].templateJson)
	}
}

// Asserts no message the network owes holds the key, its stored hash, or its
// id.
func requireTestNoticesHoldNoKey(t testing.TB, ctx context.Context, networkId server.Id, key *CreateApiKeyResult) {
	t.Helper()
	keyHash := sha256.Sum256([]byte(key.ApiKey))
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT template_json::text FROM account_message_outbox WHERE network_id = $1`, networkId)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var templateJson string
				server.Raise(result.Scan(&templateJson))
				for _, secret := range []string{key.ApiKey, "urn_", hex.EncodeToString(keyHash[:]), key.Id.String()} {
					if strings.Contains(templateJson, secret) {
						t.Fatalf("an owed message holds %q: %s", secret, templateJson)
					}
				}
			}
		})
	})
}

// The key creation time a notice's change names, or the zero time.
func testKeyCreateTime(t testing.TB, change map[string]any) time.Time {
	t.Helper()
	value, ok := change["key_create_time"].(string)
	if !ok {
		return time.Time{}
	}
	keyCreateTime, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatalf("key_create_time %q: %v", value, err)
	}
	return keyCreateTime
}

// Creating an API key owes exactly one notice to the admin's email, naming the
// key and holding neither the key nor its hash nor its id; deleting it owes
// exactly one, with the name and the creation time the key listing shows. A
// delete that deletes nothing (the key again, an unknown id, no id, another
// network's key) and a refused name owe none. A name is shown on one line.
func TestSecurityNoticeOwedOnceForEachApiKeyChange(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, userAuth, clientSession := authParityAccount(t, ctx, "security-api-key")
		networkId := clientSession.ByJwt.NetworkId
		recipients := []string{userAuth}

		key := createTestApiKey(t, clientSession, "deploy-bot")
		requireTestNoticesHoldNoKey(t, ctx, networkId, key)
		requireOneTestNotice(t, ctx, networkId, testApiKeyNoticeTemplate, recipients, map[string]any{
			"action": "created", "name": "deploy-bot", "key_create_time": nil,
		})

		listed, ok := testApiKeysOf(t, clientSession)[key.Id]
		if !ok {
			t.Fatal("the created key is not listed")
		}
		if err := DeleteApiKey(&key.Id, clientSession); err != nil {
			t.Fatal(err)
		}
		if _, ok := testApiKeysOf(t, clientSession)[key.Id]; ok {
			t.Fatal("the deleted key is still listed")
		}
		requireTestNoticesHoldNoKey(t, ctx, networkId, key)
		if notices := unsentTestNotices(t, ctx, networkId, testApiKeyNoticeTemplate); len(notices) == 1 {
			if changes := notices[0].changes(t); len(changes) == 1 && !testKeyCreateTime(t, changes[0]).Equal(listed.CreateTime) {
				t.Fatalf("the delete notice names the key created %v, want %v as listed: %s", changes[0]["key_create_time"], listed.CreateTime, notices[0].templateJson)
			}
		}
		requireOneTestNotice(t, ctx, networkId, testApiKeyNoticeTemplate, recipients, map[string]any{
			"action": "deleted", "name": "deploy-bot",
		})

		// a delete that deletes nothing owes nothing
		unknownId := server.NewId()
		for _, apiKeyId := range []*server.Id{&key.Id, &unknownId, nil} {
			if err := DeleteApiKey(apiKeyId, clientSession); err != nil {
				t.Fatal(err)
			}
		}
		requireNoTestApiKeyNotice(t, ctx, networkId, "a delete of no key")

		// another network's key is not deleted, and neither network owes a notice
		_, _, otherSession := authParityAccount(t, ctx, "security-api-key-other")
		otherNetworkId := otherSession.ByJwt.NetworkId
		otherKey := createTestApiKey(t, otherSession, "other")
		settleTestNotices(ctx, otherNetworkId)
		if err := DeleteApiKey(&otherKey.Id, clientSession); err != nil {
			t.Fatal(err)
		}
		if _, ok := testApiKeysOf(t, otherSession)[otherKey.Id]; !ok {
			t.Fatal("a network deleted another network's key")
		}
		requireNoTestApiKeyNotice(t, ctx, networkId, "a delete of another network's key")
		requireNoTestApiKeyNotice(t, ctx, otherNetworkId, "another network's delete of the key")

		// a refused name creates nothing and owes nothing
		refused, err := CreateApiKey(&CreateApiKeyArgs{Name: strings.Repeat("n", MaxApiKeyNameLength+1)}, clientSession)
		if err != nil || refused == nil || refused.Error == nil {
			t.Fatalf("a name the column cannot store was not refused: %v %+v", err, refused)
		}
		requireNoTestApiKeyNotice(t, ctx, networkId, "a refused name")

		// a name is free text: the notice keeps it to one line in reading order
		createTestApiKey(t, clientSession, " ops\nbot\u202e\tkey ")
		requireOneTestNotice(t, ctx, networkId, testApiKeyNoticeTemplate, recipients, map[string]any{
			"action": "created", "name": "ops bot key",
		})
	})
}

// Changes inside the window join one notice that lists them in order and keeps
// the due time the first change set, about a minute after it; a sign-in change in
// the same minute owes its own notice. Once the notice is due, a change owes a
// new one.
func TestSecurityNoticeApiKeyBurstJoinsOneNotice(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		userId, userAuth, clientSession := authParityAccount(t, ctx, "security-api-key-burst")
		networkId := clientSession.ByJwt.NetworkId

		first := createTestApiKey(t, clientSession, "first")
		notices := unsentTestNotices(t, ctx, networkId, testApiKeyNoticeTemplate)
		if len(notices) != 1 || notices[0].deliverTime == nil {
			t.Fatalf("the first key owed %d API key notices, want 1, due", len(notices))
		}
		dueTime := *notices[0].deliverTime
		if window := dueTime.Sub(notices[0].createTime); window < 30*time.Second || 2*time.Minute < window {
			t.Fatalf("the notice is due %s after it was owed, want about a minute", window)
		}

		second := createTestApiKey(t, clientSession, "second")
		if err := DeleteApiKey(&first.Id, clientSession); err != nil {
			t.Fatal(err)
		}
		// a sign-in change in the same minute is another kind of notice
		if _, err := GenerateSeedphrase(ctx, userId); err != nil {
			t.Fatal(err)
		}

		notices = unsentTestNotices(t, ctx, networkId, testApiKeyNoticeTemplate)
		if len(notices) != 1 {
			t.Fatalf("three API key changes in a burst owed %d notices, want 1", len(notices))
		}
		changes := notices[0].changes(t)
		if len(changes) != 3 ||
			changes[0]["action"] != "created" || changes[0]["name"] != "first" ||
			changes[1]["action"] != "created" || changes[1]["name"] != "second" ||
			changes[2]["action"] != "deleted" || changes[2]["name"] != "first" {
			t.Fatalf("the burst's notice lists %s, want the two keys created and the first deleted, in order", notices[0].templateJson)
		}
		if notices[0].deliverTime == nil || !notices[0].deliverTime.Equal(dueTime) {
			t.Fatalf("the changes that joined moved the notice's due time from %s to %v", dueTime, notices[0].deliverTime)
		}
		if notices[0].userAuth != userAuth {
			t.Fatalf("the notice went to %s, want %s", notices[0].userAuth, userAuth)
		}
		if signIns := unsentTestNotices(t, ctx, networkId, testSignInNoticeTemplate); len(signIns) != 1 || len(signIns[0].changes(t)) != 1 {
			t.Fatalf("the sign-in change owed %d sign-in notices, want one of its own", len(signIns))
		}

		// once the notice is due, a change owes a new one
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE account_message_outbox SET deliver_time = $2 WHERE message_id = $1`,
				notices[0].messageId,
				server.NowUtc().Add(-time.Second),
			))
		})
		if err := DeleteApiKey(&second.Id, clientSession); err != nil {
			t.Fatal(err)
		}
		notices = unsentTestNotices(t, ctx, networkId, testApiKeyNoticeTemplate)
		if len(notices) != 2 || len(notices[0].changes(t)) != 3 || len(notices[1].changes(t)) != 1 {
			t.Fatalf("a change after the notice was due owed %d notices, want a second one", len(notices))
		}
	})
}

// A notice that cannot be owed never fails or rolls back the API key change it
// reports: with every outbox write failing, a key is created and another
// deleted, and nothing is owed; a notice that fails after its writes is rolled
// back alone. Each failure is counted. Without a fault, the next change owes its
// notice.
func TestSecurityNoticeApiKeyFailureNeverBlocksTheChange(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, userAuth, clientSession := authParityAccount(t, ctx, "security-api-key-fault")
		networkId := clientSession.ByJwt.NetworkId
		recipients := []string{userAuth}

		// the control: without a fault, a key owes its notice
		kept := createTestApiKey(t, clientSession, "kept")
		requireOneTestNotice(t, ctx, networkId, testApiKeyNoticeTemplate, recipients, map[string]any{
			"action": "created", "name": "kept",
		})

		change := func(what string, call func()) {
			t.Helper()
			var failure any
			func() {
				defer func() {
					failure = recover()
				}()
				call()
			}()
			if failure != nil {
				t.Fatalf("%s failed with its notice: %v", what, failure)
			}
		}

		failedBefore := securityNoticeCount(testApiKeyNoticeLabel, securityNoticeFailed)
		removeFault := failTestOutboxStatements(ctx, networkId)
		var created *CreateApiKeyResult
		change("the key created", func() {
			created = createTestApiKey(t, clientSession, "created while the outbox fails")
		})
		change("the key deleted", func() {
			if err := DeleteApiKey(&kept.Id, clientSession); err != nil {
				panic(err)
			}
		})
		removeFault()
		keys := testApiKeysOf(t, clientSession)
		if _, ok := keys[created.Id]; !ok {
			t.Fatal("the key created while its notice failed is not listed")
		}
		if _, ok := keys[kept.Id]; ok {
			t.Fatal("the key deleted while its notice failed is still listed")
		}
		requireNoTestApiKeyNotice(t, ctx, networkId, "every outbox write failing")
		if delta := securityNoticeCount(testApiKeyNoticeLabel, securityNoticeFailed) - failedBefore; delta != 2 {
			t.Fatalf("counted %v failed API key notices, want 2", delta)
		}

		// a failure after the notice's writes rolls back the notice alone
		faultCtx := context.WithValue(ctx, securityNoticeFaultKey{}, func(notice string, result string) {
			if notice == testApiKeyNoticeLabel && result == securityNoticeQueued {
				panic(errors.New("synthetic notice failure after its writes"))
			}
		})
		var late *CreateApiKeyResult
		change("the key created with a late notice failure", func() {
			late = createTestApiKey(t, session.Testing_CreateClientSession(faultCtx, clientSession.ByJwt), "late")
		})
		if _, ok := testApiKeysOf(t, clientSession)[late.Id]; !ok {
			t.Fatal("the key whose notice failed after its writes is not listed")
		}
		requireNoTestApiKeyNotice(t, ctx, networkId, "a notice failing after its writes")
		if delta := securityNoticeCount(testApiKeyNoticeLabel, securityNoticeFailed) - failedBefore; delta != 3 {
			t.Fatalf("counted %v failed API key notices, want 3", delta)
		}

		// without a fault, the next change owes its notice
		if err := DeleteApiKey(&late.Id, clientSession); err != nil {
			t.Fatal(err)
		}
		requireOneTestNotice(t, ctx, networkId, testApiKeyNoticeTemplate, recipients, map[string]any{
			"action": "deleted", "name": "late",
		})
	})
}

// The notice goes by email only: an account with an email and a phone number
// owes it to the email address alone, and an account that signs in only with a
// phone number owes no message for an API key change, by email or SMS, and
// counts it as no_email.
func TestSecurityNoticeApiKeyIsEmailOnly(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		_, userAuth, clientSession := authParityAccount(t, ctx, "security-api-key-email")
		networkId := clientSession.ByJwt.NetworkId
		phone := "+1 415-555-0166"
		password := "synthetic-password-1"
		if result, err := AddAuth(AddAuthMethod{UserAuth: &phone, Password: &password}, clientSession); err != nil || result.Error != nil {
			t.Fatalf("add phone: %v %+v", err, result)
		}
		settleTestNotices(ctx, networkId)
		key := createTestApiKey(t, clientSession, "email only")
		if err := DeleteApiKey(&key.Id, clientSession); err != nil {
			t.Fatal(err)
		}
		notices := unsentTestNotices(t, ctx, networkId, testApiKeyNoticeTemplate)
		if len(notices) != 1 || !strings.EqualFold(notices[0].userAuth, userAuth) {
			got := []string{}
			for _, notice := range notices {
				got = append(got, notice.userAuth)
			}
			t.Fatalf("owed %d API key notices (to %v), want one, to %s", len(notices), got, userAuth)
		}
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `SELECT user_auth FROM account_message_outbox WHERE network_id = $1 AND sent_time IS NULL`, networkId)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var recipient string
					server.Raise(result.Scan(&recipient))
					if _, userAuthType := NormalUserAuth(recipient); userAuthType != UserAuthTypeEmail {
						t.Fatalf("a message went to %s, which is no email address", recipient)
					}
				}
			})
		})

		// an account that signs in only with a phone number
		phoneNetworkId, phoneUserId := server.NewId(), server.NewId()
		phoneOnly := "+1 415-555-0167"
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network (network_id, network_name, admin_user_id) VALUES ($1, $2, $3)`, phoneNetworkId, "phone-"+phoneNetworkId.String()[:8], phoneUserId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_user (user_id, user_name, auth_type, user_auth, verified) VALUES ($1, 'phone', 'password', $2, true)`, phoneUserId, phoneOnly))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_user_auth_password (user_id, user_auth, auth_type, verified) VALUES ($1, $2, 'phone', true)`, phoneUserId, phoneOnly))
		})
		phoneSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: phoneNetworkId, UserId: phoneUserId, NetworkName: "phone"})
		noEmailBefore := securityNoticeCount(testApiKeyNoticeLabel, securityNoticeNoEmail)
		phoneKey := createTestApiKey(t, phoneSession, "phone only")
		if err := DeleteApiKey(&phoneKey.Id, phoneSession); err != nil {
			t.Fatal(err)
		}
		if count := countRows(ctx, `SELECT COUNT(*) FROM account_message_outbox WHERE network_id = $1`, phoneNetworkId); count != 0 {
			t.Fatalf("a phone account owed %d messages, want none", count)
		}
		if delta := securityNoticeCount(testApiKeyNoticeLabel, securityNoticeNoEmail) - noEmailBefore; delta != 2 {
			t.Fatalf("counted %v no_email API key notices, want 2", delta)
		}
	})
}
