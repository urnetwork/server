package model

// The scrub of the network-create audit rows written before the allowlist
// (audit_network_create_scrub.go). The rows here are written the way the
// audit wrote them then, by marshaling the whole request; every credential and
// address is synthetic.

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The event details the audit wrote before the allowlist: the whole request,
// as auditNetworkCreate marshaled it.
func networkCreateAuditDetailsBeforeAllowlist(t testing.TB, networkCreate NetworkCreateArgs) string {
	t.Helper()
	type Details struct {
		NetworkCreate     NetworkCreateArgs `json:"network_create"`
		ClientAddressHash string            `json:"client_address_hash,omitempty"`
		ClientPort        int               `json:"client_port,omitempty"`
	}
	detailsJson, err := json.Marshal(Details{
		NetworkCreate:     networkCreate,
		ClientAddressHash: "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff",
		ClientPort:        40100,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(detailsJson)
}

// Ids in creation order.
func orderedTestIds(count int) []server.Id {
	ids := []server.Id{}
	for range count {
		ids = append(ids, server.NewId())
	}
	slices.SortFunc(ids, func(a server.Id, b server.Id) int { return a.Cmp(b) })
	return ids
}

func insertNetworkCreateAuditRow(ctx context.Context, eventId server.Id, eventType AuditEventType, eventDetails *string) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO audit_network_event (event_id, event_time, network_id, event_type, event_details)
				VALUES ($1, $2, $3, $4, $5)
			`,
			eventId,
			server.NowUtc(),
			server.NewId(),
			eventType,
			eventDetails,
		))
	})
}

// The details of every row, by event id.
func networkCreateAuditRows(ctx context.Context) map[server.Id]*string {
	rows := map[server.Id]*string{}
	server.Tx(ctx, func(tx server.PgTx) {
		result, err := tx.Query(ctx, `SELECT event_id, event_details FROM audit_network_event`)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var eventId server.Id
				var eventDetails *string
				server.Raise(result.Scan(&eventId, &eventDetails))
				rows[eventId] = eventDetails
			}
		})
	})
	return rows
}

func testNetworkCreateAuditScrubSettings(batchSize int) *NetworkCreateAuditScrubSettings {
	settings := DefaultNetworkCreateAuditScrubSettings()
	settings.BatchSize = batchSize
	settings.BatchPause = 0
	return settings
}

// Pure: each shape the audit wrote loses exactly its credential keys, keeps
// every other member in place and byte for byte, and a second scrub changes
// nothing.
func TestScrubNetworkCreateAuditDetailsRemovesOnlyTheCredentials(t *testing.T) {
	for _, c := range []struct {
		name            string
		eventDetails    string
		want            string
		removedKeyPaths []string
		readable        bool
	}{
		{
			name:            "password",
			eventDetails:    `{"network_create":{"user_name":"u","user_auth":"scrub@example.com","password":"synthetic-pw","network_name":"scrub-net","terms":true,"verify_use_numeric":true},"client_address_hash":"ab","client_port":4321}`,
			want:            `{"network_create":{"user_name":"u","user_auth":"scrub@example.com","network_name":"scrub-net","terms":true,"verify_use_numeric":true},"client_address_hash":"ab","client_port":4321}`,
			removedKeyPaths: []string{"network_create.password"},
			readable:        true,
		},
		{
			name:            "sign-in with a balance code",
			eventDetails:    `{"network_create":{"user_name":"","auth_jwt":"synthetic.jwt.token","auth_jwt_type":"apple","network_name":"scrub-net","terms":true,"guest_mode":false,"verify_use_numeric":false,"referral_code":"R1","balance_code":"synthetic-code","product_updates":false},"client_address":"192.0.2.1:1"}`,
			want:            `{"network_create":{"user_name":"","auth_jwt_type":"apple","network_name":"scrub-net","terms":true,"guest_mode":false,"verify_use_numeric":false,"referral_code":"R1","product_updates":false},"client_address":"192.0.2.1:1"}`,
			removedKeyPaths: []string{"network_create.auth_jwt", "network_create.balance_code"},
			readable:        true,
		},
		{
			name:            "wallet",
			eventDetails:    `{"network_create":{"user_name":"","network_name":"scrub-net","terms":true,"verify_use_numeric":false,"wallet_auth":{"wallet_address":"synthetic-address","wallet_signature":"synthetic-signature","wallet_message":"synthetic <message>","blockchain":"SOL","wallet_nonce":"n"}},"client_port":7}`,
			want:            `{"network_create":{"user_name":"","network_name":"scrub-net","terms":true,"verify_use_numeric":false,"wallet_auth":{"wallet_address":"synthetic-address","blockchain":"SOL","wallet_nonce":"n"}},"client_port":7}`,
			removedKeyPaths: []string{"network_create.wallet_auth.wallet_signature", "network_create.wallet_auth.wallet_message"},
			readable:        true,
		},
		{
			name:            "every key, a duplicate, and escapes kept as written",
			eventDetails:    `{"network_create":{"password":"a","user_name":"<u> &","password":"b","auth_jwt":"j","balance_code":"c","wallet_auth":{"wallet_signature":"s","wallet_message":"m"}}}`,
			want:            `{"network_create":{"user_name":"<u> &","wallet_auth":{}}}`,
			removedKeyPaths: NetworkCreateAuditScrubKeyPaths(),
			readable:        true,
		},
		{
			name:         "seedphrase, nothing to remove",
			eventDetails: `{"network_create":{"user_name":"","network_name":"","terms":true,"verify_use_numeric":false},"client_address_hash":"ab","client_port":1}`,
			want:         `{"network_create":{"user_name":"","network_name":"","terms":true,"verify_use_numeric":false},"client_address_hash":"ab","client_port":1}`,
			readable:     true,
		},
		{
			name:         "a key's name as a value",
			eventDetails: `{"network_create":{"user_name":"password","network_name":"auth_jwt","wallet_auth":null}}`,
			want:         `{"network_create":{"user_name":"password","network_name":"auth_jwt","wallet_auth":null}}`,
			readable:     true,
		},
		{
			name:            "a null wallet",
			eventDetails:    `{"network_create":{"wallet_auth":null,"password":"x"}}`,
			want:            `{"network_create":{"wallet_auth":null}}`,
			removedKeyPaths: []string{"network_create.password"},
			readable:        true,
		},
		{
			name:         "credential keys outside the request are not the request's",
			eventDetails: `{"password":"x","network_create":null}`,
			want:         `{"password":"x","network_create":null}`,
			readable:     true,
		},
		{
			name:         "not json",
			eventDetails: `"password" here`,
			want:         `"password" here`,
		},
		{
			name:         "not an object",
			eventDetails: `["network_create",{"password":"x"}]`,
			want:         `["network_create",{"password":"x"}]`,
		},
		{
			name:         "a request that is not an object",
			eventDetails: `{"network_create":"password"}`,
			want:         `{"network_create":"password"}`,
		},
		{
			name:         "a wallet that is not an object",
			eventDetails: `{"network_create":{"password":"x","wallet_auth":["wallet_signature"]}}`,
			want:         `{"network_create":{"password":"x","wallet_auth":["wallet_signature"]}}`,
		},
		{
			name:         "text after the object",
			eventDetails: `{"network_create":{"password":"x"}} {}`,
			want:         `{"network_create":{"password":"x"}} {}`,
		},
	} {
		scrubbed, removedKeyPaths, readable := scrubNetworkCreateAuditDetails(c.eventDetails)
		if scrubbed != c.want || !slices.Equal(removedKeyPaths, c.removedKeyPaths) || readable != c.readable {
			t.Errorf("%s: scrubbed %s, removed %v, readable %t; want %s, %v, %t", c.name, scrubbed, removedKeyPaths, readable, c.want, c.removedKeyPaths, c.readable)
			continue
		}
		again, removedAgain, readableAgain := scrubNetworkCreateAuditDetails(scrubbed)
		if again != scrubbed || len(removedAgain) != 0 || readableAgain != c.readable {
			t.Errorf("%s: a second scrub gave %s, removed %v, readable %t", c.name, again, removedAgain, readableAgain)
		}
	}
}

// Pure: the rows the old writer wrote for every sign-up path lose their
// credentials, and keep what the allowlist keeps.
func TestScrubNetworkCreateAuditDetailsOfTheOldWriter(t *testing.T) {
	userAuth := "scrub-writer@example.com"
	password := "synthetic-password-scrub-writer"
	authJwt := "synthetic-header.synthetic-claims.synthetic-signature"
	authJwtType := string(AuthTypeGoogle)
	balanceCode := "synthetic-balance-code-scrub-writer"
	referralCode := "SCRUB1"
	productUpdates := true
	networkCreate := NetworkCreateArgs{
		UserName:         "scrub-writer",
		UserAuth:         &userAuth,
		AuthJwt:          &authJwt,
		AuthJwtType:      &authJwtType,
		Password:         &password,
		NetworkName:      "scrub-writer-net",
		Terms:            true,
		VerifyUseNumeric: true,
		ReferralCode:     &referralCode,
		BalanceCode:      &balanceCode,
		WalletAuth: &WalletAuthArgs{
			PublicKey:  "synthetic-wallet-address",
			Signature:  "synthetic-wallet-signature",
			Message:    "synthetic signed wallet message",
			Blockchain: SOL.String(),
		},
		ProductUpdates: &productUpdates,
	}
	scrubbed, removedKeyPaths, readable := scrubNetworkCreateAuditDetails(networkCreateAuditDetailsBeforeAllowlist(t, networkCreate))
	if !readable || !slices.Equal(removedKeyPaths, NetworkCreateAuditScrubKeyPaths()) {
		t.Fatalf("removed %v, readable %t", removedKeyPaths, readable)
	}
	for _, credential := range []string{password, authJwt, balanceCode, "synthetic-wallet-signature", "synthetic signed wallet message"} {
		if strings.Contains(scrubbed, credential) {
			t.Errorf("the scrubbed row holds a credential: %q", credential)
		}
	}
	details := map[string]any{}
	if err := json.Unmarshal([]byte(scrubbed), &details); err != nil {
		t.Fatal(err)
	}
	wantRequest := map[string]any{
		"user_name":          "scrub-writer",
		"user_auth":          userAuth,
		"auth_jwt_type":      authJwtType,
		"network_name":       "scrub-writer-net",
		"terms":              true,
		"verify_use_numeric": true,
		"referral_code":      referralCode,
		"product_updates":    true,
		"wallet_auth": map[string]any{
			"wallet_address": "synthetic-wallet-address",
			"blockchain":     SOL.String(),
		},
	}
	request, _ := json.Marshal(details["network_create"])
	want, _ := json.Marshal(wantRequest)
	if string(request) != string(want) {
		t.Errorf("the scrubbed request is %s, want %s", request, want)
	}
	if details["client_address_hash"] != "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff" || details["client_port"] != float64(40100) {
		t.Errorf("the scrubbed row lost its client address: %v", details)
	}
}

// The rows of the scrub's database tests, in id order, and which the census
// should count.
type networkCreateAuditScrubFixture struct {
	eventIds []server.Id
	// the original details of each row, by event id
	details map[server.Id]*string
}

// Writes, in id order: a password row; a deleted event; a sign-in row with a
// balance code; a seedphrase row with nothing to remove; a wallet row; an
// unreadable row; a row with a key's name as a value; a second password row.
func writeNetworkCreateAuditScrubFixture(t testing.TB, ctx context.Context) *networkCreateAuditScrubFixture {
	t.Helper()
	password := "synthetic-password-scrub-fixture"
	userAuth := "scrub-fixture@example.com"
	authJwt := "synthetic-header.synthetic-claims.synthetic-signature"
	authJwtType := string(AuthTypeApple)
	balanceCode := "synthetic-balance-code-scrub-fixture"
	unreadable := `{"network_create":"password"}`
	keyAsValue := `{"network_create":{"user_name":"password","network_name":"wallet_message"}}`
	rows := []struct {
		eventType    AuditEventType
		eventDetails *string
	}{
		{eventType: AuditEventTypeNetworkCreated, eventDetails: new(networkCreateAuditDetailsBeforeAllowlist(t, NetworkCreateArgs{UserAuth: &userAuth, Password: &password, NetworkName: "scrub-a", Terms: true}))},
		{eventType: AuditEventTypeNetworkDeleted},
		{eventType: AuditEventTypeNetworkCreated, eventDetails: new(networkCreateAuditDetailsBeforeAllowlist(t, NetworkCreateArgs{AuthJwt: &authJwt, AuthJwtType: &authJwtType, BalanceCode: &balanceCode, NetworkName: "scrub-b", Terms: true}))},
		{eventType: AuditEventTypeNetworkCreated, eventDetails: new(networkCreateAuditDetailsBeforeAllowlist(t, NetworkCreateArgs{Terms: true}))},
		{eventType: AuditEventTypeNetworkCreated, eventDetails: new(networkCreateAuditDetailsBeforeAllowlist(t, NetworkCreateArgs{NetworkName: "scrub-c", Terms: true, WalletAuth: &WalletAuthArgs{PublicKey: "synthetic-address", Signature: "synthetic-signature", Message: "synthetic message", Blockchain: SOL.String()}}))},
		{eventType: AuditEventTypeNetworkCreated, eventDetails: &unreadable},
		{eventType: AuditEventTypeNetworkCreated, eventDetails: &keyAsValue},
		{eventType: AuditEventTypeNetworkCreated, eventDetails: new(networkCreateAuditDetailsBeforeAllowlist(t, NetworkCreateArgs{UserAuth: &userAuth, Password: &password, NetworkName: "scrub-d", Terms: true}))},
	}
	fixture := &networkCreateAuditScrubFixture{
		eventIds: orderedTestIds(len(rows)),
		details:  map[server.Id]*string{},
	}
	for i, row := range rows {
		insertNetworkCreateAuditRow(ctx, fixture.eventIds[i], row.eventType, row.eventDetails)
		fixture.details[fixture.eventIds[i]] = row.eventDetails
	}
	return fixture
}

// The dry run's census: the rows holding each key in the scope, the rows to
// rewrite and the unreadable rows, over several ranges, with a digest that
// stays the same for the same rows and scope.
func TestNetworkCreateAuditScrubCensusCountsTheCredentials(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := writeNetworkCreateAuditScrubFixture(t, ctx)
		ids := fixture.eventIds
		// ranges of 3 rows of any type: the scope through row 7 is 3 ranges
		settings := testNetworkCreateAuditScrubSettings(3)

		census, err := CensusNetworkCreateAuditSecrets(ctx, nil, ids[6], settings, nil)
		if err != nil {
			t.Fatal(err)
		}
		wantKeyCounts := map[string]int{
			"network_create.password":                     1,
			"network_create.auth_jwt":                     1,
			"network_create.balance_code":                 1,
			"network_create.wallet_auth.wallet_signature": 1,
			"network_create.wallet_auth.wallet_message":   1,
		}
		if len(census.KeyCounts) != len(wantKeyCounts) {
			t.Errorf("the census counts the keys %v", census.KeyCounts)
		}
		for keyPath, count := range wantKeyCounts {
			if census.KeyCounts[keyPath] != count {
				t.Errorf("%s: %d rows, want %d", keyPath, census.KeyCounts[keyPath], count)
			}
		}
		// rows 1, 3, 5, 6 and 7 hold a key's name as text; row 8 is past the scope
		if census.CandidateCount != 5 {
			t.Errorf("the census decoded %d rows, want 5", census.CandidateCount)
		}
		if !slices.Equal(census.EventIds, []server.Id{ids[0], ids[2], ids[4]}) {
			t.Errorf("the census rewrites %v, want rows 1, 3 and 5", census.EventIds)
		}
		if !slices.Equal(census.UnreadableEventIds, []server.Id{ids[5]}) {
			t.Errorf("the census found the unreadable rows %v, want row 6", census.UnreadableEventIds)
		}

		// the same rows and scope, in other ranges, give the same digest
		again, err := CensusNetworkCreateAuditSecrets(ctx, nil, ids[6], testNetworkCreateAuditScrubSettings(5000), nil)
		if err != nil {
			t.Fatal(err)
		}
		if again.Digest != census.Digest {
			t.Errorf("the same rows and scope gave another digest")
		}

		// a narrower scope counts its own rows, with its own digest
		after, err := CensusNetworkCreateAuditSecrets(ctx, &ids[2], ids[7], settings, nil)
		if err != nil {
			t.Fatal(err)
		}
		if after.KeyCounts["network_create.password"] != 1 || after.KeyCounts["network_create.auth_jwt"] != 0 || after.KeyCounts["network_create.wallet_auth.wallet_signature"] != 1 {
			t.Errorf("the scope after row 3 counts %v", after.KeyCounts)
		}
		if !slices.Equal(after.EventIds, []server.Id{ids[4], ids[7]}) || after.Digest == census.Digest {
			t.Errorf("the scope after row 3 rewrites %v (digest changed: %t)", after.EventIds, after.Digest != census.Digest)
		}

		// a row that changes in the scope changes the digest
		insertNetworkCreateAuditRow(ctx, server.NewId(), AuditEventTypeNetworkDeleted, nil)
		password := "synthetic-password-scrub-late"
		late := networkCreateAuditDetailsBeforeAllowlist(t, NetworkCreateArgs{Password: &password})
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE audit_network_event SET event_details = $2 WHERE event_id = $1`, ids[3], late))
		})
		changed, err := CensusNetworkCreateAuditSecrets(ctx, nil, ids[6], settings, nil)
		if err != nil {
			t.Fatal(err)
		}
		if changed.Digest == census.Digest || changed.KeyCounts["network_create.password"] != 2 {
			t.Errorf("a changed row in the scope left the digest (changed: %t) or counts %v", changed.Digest != census.Digest, changed.KeyCounts)
		}
	})
}

// The apply rewrites exactly the census's rows: their credential keys go, every
// other member stays as written, and every other row is untouched. A second
// census finds nothing, and its apply rewrites nothing.
func TestNetworkCreateAuditScrubApplyRemovesTheCredentialsOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := writeNetworkCreateAuditScrubFixture(t, ctx)
		ids := fixture.eventIds
		settings := testNetworkCreateAuditScrubSettings(2)

		census, err := CensusNetworkCreateAuditSecrets(ctx, nil, ids[6], settings, nil)
		if err != nil {
			t.Fatal(err)
		}
		progress := []NetworkCreateAuditScrubProgress{}
		result, err := ApplyNetworkCreateAuditScrub(ctx, nil, ids[6], census.Digest, settings, func(p NetworkCreateAuditScrubProgress) {
			if p.Stage == "rewrite" {
				progress = append(progress, p)
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.RewrittenCount != 3 || result.LastEventId == nil || *result.LastEventId != ids[4] {
			t.Fatalf("the apply rewrote %d rows through %v, want 3 through row 5", result.RewrittenCount, result.LastEventId)
		}
		// batches of 2 rows: rows 1 and 3, then row 5
		if len(progress) != 2 || progress[0].EventId != ids[2] || progress[1].Count != 3 {
			t.Errorf("the apply reported %v", progress)
		}

		rows := networkCreateAuditRows(ctx)
		for i, eventId := range ids {
			original := fixture.details[eventId]
			got := rows[eventId]
			switch i {
			case 0, 2, 4:
				want, removedKeyPaths, readable := scrubNetworkCreateAuditDetails(*original)
				if got == nil || *got != want || len(removedKeyPaths) == 0 || !readable {
					t.Errorf("row %d is %v, want its scrub %s", i+1, got, want)
					continue
				}
				for _, key := range networkCreateAuditCredentialKeys {
					if strings.Contains(*got, `"`+key+`"`) {
						t.Errorf("row %d still holds %q", i+1, key)
					}
				}
			default:
				// the deleted event, the rows with nothing to remove, the
				// unreadable row and the row past the scope
				if (got == nil) != (original == nil) || (got != nil && *got != *original) {
					t.Errorf("row %d changed: %v, was %v", i+1, got, original)
				}
			}
		}
		// the row the deleted event's network had is not the subject; the kept
		// members of a rewritten row read as before
		details := map[string]any{}
		if err := json.Unmarshal([]byte(*rows[ids[0]]), &details); err != nil {
			t.Fatal(err)
		}
		request, _ := details["network_create"].(map[string]any)
		if request["user_auth"] != "scrub-fixture@example.com" || request["network_name"] != "scrub-a" || request["terms"] != true || details["client_port"] != float64(40100) {
			t.Errorf("row 1 lost a kept member: %v", details)
		}

		// idempotent
		again, err := CensusNetworkCreateAuditSecrets(ctx, nil, ids[6], settings, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(again.EventIds) != 0 || again.KeyCounts["network_create.password"] != 0 || !slices.Equal(again.UnreadableEventIds, []server.Id{ids[5]}) {
			t.Fatalf("a census after the apply rewrites %v, counts %v", again.EventIds, again.KeyCounts)
		}
		before := networkCreateAuditRows(ctx)
		repeat, err := ApplyNetworkCreateAuditScrub(ctx, nil, ids[6], again.Digest, settings, nil)
		if err != nil {
			t.Fatal(err)
		}
		if repeat.RewrittenCount != 0 || repeat.LastEventId != nil {
			t.Errorf("a second apply rewrote %d rows", repeat.RewrittenCount)
		}
		after := networkCreateAuditRows(ctx)
		for eventId, eventDetails := range before {
			if (eventDetails == nil) != (after[eventId] == nil) || (eventDetails != nil && *eventDetails != *after[eventId]) {
				t.Errorf("a second apply changed row %s", eventId)
			}
		}
	})
}

// An apply whose census does not have the digest it was given changes nothing.
func TestNetworkCreateAuditScrubApplyRefusesAnotherDigest(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := writeNetworkCreateAuditScrubFixture(t, ctx)
		ids := fixture.eventIds
		settings := testNetworkCreateAuditScrubSettings(5000)

		// the dry run's scope was through row 5; the apply names row 7
		dryRun, err := CensusNetworkCreateAuditSecrets(ctx, nil, ids[4], settings, nil)
		if err != nil {
			t.Fatal(err)
		}
		before := networkCreateAuditRows(ctx)
		for _, expect := range []string{"", "not-a-digest", dryRun.Digest} {
			result, err := ApplyNetworkCreateAuditScrub(ctx, nil, ids[6], expect, settings, nil)
			if !errors.Is(err, ErrNetworkCreateAuditScrubDigestMismatch) || result.RewrittenCount != 0 || result.Census == nil {
				t.Errorf("an apply expecting %q ended with %v, rewrote %d", expect, err, result.RewrittenCount)
			}
		}
		after := networkCreateAuditRows(ctx)
		for eventId, eventDetails := range before {
			if (eventDetails == nil) != (after[eventId] == nil) || (eventDetails != nil && *eventDetails != *after[eventId]) {
				t.Errorf("a refused apply changed row %s", eventId)
			}
		}
	})
}

// A batch that cannot take its rows' locks stops the run after the batches it
// committed and reports the last; a new dry run after that id and its apply
// finish the rest. The lock is held by another transaction for the whole
// apply, so the stop does not depend on timing.
func TestNetworkCreateAuditScrubResumesAfterAStoppedBatch(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		ids := orderedTestIds(6)
		for i, eventId := range ids {
			password := "synthetic-password-scrub-resume-" + string(rune('a'+i))
			details := networkCreateAuditDetailsBeforeAllowlist(t, NetworkCreateArgs{Password: &password, Terms: true})
			insertNetworkCreateAuditRow(ctx, eventId, AuditEventTypeNetworkCreated, &details)
		}
		settings := testNetworkCreateAuditScrubSettings(2)
		settings.LockTimeout = 200 * time.Millisecond
		settings.MaxBatchAttempts = 1

		census, err := CensusNetworkCreateAuditSecrets(ctx, nil, ids[5], settings, nil)
		if err != nil {
			t.Fatal(err)
		}

		// another transaction holds row 3, of the second batch
		locked := make(chan struct{})
		lockedOnce := sync.Once{}
		release := make(chan struct{})
		holderDone := make(chan struct{})
		go func() {
			defer close(holderDone)
			server.HandleError(func() {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `SELECT event_id FROM audit_network_event WHERE event_id = $1 FOR UPDATE`, ids[2]))
					lockedOnce.Do(func() {
						close(locked)
					})
					<-release
				})
			})
		}()
		select {
		case <-locked:
		case <-holderDone:
			t.Fatal("the lock holder ended before it locked row 3")
		}
		result, err := ApplyNetworkCreateAuditScrub(ctx, nil, ids[5], census.Digest, settings, nil)
		close(release)
		<-holderDone
		if err == nil {
			t.Fatal("the apply went past a batch it could not lock")
		}
		if result.RewrittenCount != 2 || result.LastEventId == nil || *result.LastEventId != ids[1] {
			t.Fatalf("the stopped apply rewrote %d rows through %v, want 2 through row 2", result.RewrittenCount, result.LastEventId)
		}
		if !strings.Contains(err.Error(), ids[1].String()) {
			t.Errorf("the stop does not name the last committed id: %s", err)
		}

		resume, err := CensusNetworkCreateAuditSecrets(ctx, result.LastEventId, ids[5], settings, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(resume.EventIds, ids[2:]) {
			t.Fatalf("the census after row 2 rewrites %v, want rows 3 to 6", resume.EventIds)
		}
		finished, err := ApplyNetworkCreateAuditScrub(ctx, result.LastEventId, ids[5], resume.Digest, settings, nil)
		if err != nil {
			t.Fatal(err)
		}
		if finished.RewrittenCount != 4 {
			t.Errorf("the resumed apply rewrote %d rows, want 4", finished.RewrittenCount)
		}
		for eventId, eventDetails := range networkCreateAuditRows(ctx) {
			if eventDetails == nil || strings.Contains(*eventDetails, `"password"`) || strings.Contains(*eventDetails, "synthetic-password-scrub-resume") {
				t.Errorf("row %s still holds its password", eventId)
			}
		}
	})
}
