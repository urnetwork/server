//go:build sidecar_pending

package model

// The Team routes `ur admin` runs by one-command admin tokens (TEAM-M6-08; §7.8's admin_token_use, D-TEAM-7), on
// Postgres with MG1 and MG6: POST /network/idp, POST /network/domain-add|domain-verify|domain-remove, POST
// /network/audit-webhook and GET /network/admin-events. Each runs a token's command once, a replay answering the
// first and running nothing; a refused caller spends no use; a token without the all scope runs nothing; no stored
// answer holds a lasting credential; and no admin command carries an IdP's client secret.

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// A one-command admin token's session for the member memberSession names: the token's jti, scopes and a cmd of its
// own, as the authenticator leaves them (session.authenticate).
func testAdminCommandSession(t testing.TB, ctx context.Context, memberSession *session.ClientSession, scopes []string, jti string) *session.ClientSession {
	t.Helper()
	byJwt := memberSession.ByJwt
	memberId := *byJwt.MemberId
	tokenSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: byJwt.NetworkId, UserId: byJwt.UserId, MemberId: &memberId})
	t.Cleanup(tokenSession.Cancel)
	tokenSession.UnsignedIdentity = true
	tokenSession.AdminCommand = &session.AdminCommandUse{
		NetworkId:  byJwt.NetworkId,
		MemberId:   memberId,
		Jti:        jti,
		Cmd:        "cmd-" + jti,
		Scopes:     scopes,
		ExpireTime: server.NowUtc().Add(time.Hour),
	}
	return tokenSession
}

// The answer admin_token_use keeps for jti, false for no row.
func testAdminTokenUseResult(ctx context.Context, jti string) (result []byte, found bool) {
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT result FROM admin_token_use WHERE jti = $1`, jti)
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				server.Raise(rows.Scan(&result))
				found = true
			}
		})
	})
	return
}

// How many of the network's admin events are action.
func testAdminEventCount(ctx context.Context, networkId server.Id, action string) int {
	count := 0
	for _, eventAction := range testAdminEventActions(ctx, networkId) {
		if eventAction == action {
			count += 1
		}
	}
	return count
}

// POST /network/idp: an Owner's token adds a private_key_jwt IdP once; a client secret never rides in a command, at
// the consent's body check or at the route, and its refusal spends nothing; an Admin's token is refused by the
// Owner's route and spends nothing; a token without all adds nothing.
func TestIdpByAdminTokenOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId, ownerSession := newTestIdpTeam(t, ctx)
		previous := networkIdpPrivateKeyJwtAvailable
		networkIdpPrivateKeyJwtAvailable = func() bool { return true }
		defer func() { networkIdpPrivateKeyJwtAvailable = previous }()
		defer Testing_SetNetworkIdpSecretKey(bytes.Repeat([]byte{7}, 32))()
		args := &CreateNetworkIdpArgs{Issuer: "https://idp.example", ClientId: "synthetic-client", ClientAuth: NetworkIdpClientAuthPrivateKeyJwt}
		idpCount := func() int {
			return testTeamCount(ctx, `SELECT COUNT(*) FROM network_idp WHERE network_id = $1`, networkId)
		}

		jti := server.NewId().String()
		first, err := CreateNetworkIdp(args, testAdminCommandSession(t, ctx, ownerSession, []string{"all"}, jti))
		if err != nil || first.Error != nil || first.IdpId == nil {
			t.Fatalf("the first use: %v %+v", err, first)
		}
		replay, err := CreateNetworkIdp(args, testAdminCommandSession(t, ctx, ownerSession, []string{"all"}, jti))
		if err != nil || replay.IdpId == nil || *replay.IdpId != *first.IdpId {
			t.Fatalf("the replay: %v %+v", err, replay)
		}
		connect.AssertEqual(t, idpCount(), 1)
		connect.AssertEqual(t, testAdminEventCount(ctx, networkId, AdminEventActionIdpAdd), 1)

		secretJti := server.NewId().String()
		secret := "synthetic-idp-secret-4f1e"
		refused, err := CreateNetworkIdp(&CreateNetworkIdpArgs{
			Issuer: "https://idp.example", ClientId: "synthetic-client", ClientAuth: NetworkIdpClientAuthClientSecret, ClientSecret: secret,
		}, testAdminCommandSession(t, ctx, ownerSession, []string{"all"}, secretJti))
		if err != nil || refused.Error == nil || refused.Error.Message != networkIdpAdminCommandSecretMessage {
			t.Fatalf("a client secret by an admin token: %v %+v", err, refused)
		}
		if _, found := testAdminTokenUseResult(ctx, secretJti); found {
			t.Fatal("a refused client secret spent the token's use")
		}
		connect.AssertEqual(t, idpCount(), 1)

		if _, err := CreateNetworkIdp(args, testAdminCommandSession(t, ctx, ownerSession, []string{"auth_keys"}, server.NewId().String())); err == nil {
			t.Fatal("a token without all added an IdP")
		}
		adminSession := newTestTeamMemberSession(t, ctx, networkId, NetworkMemberRoleAdmin, false)
		adminJti := server.NewId().String()
		if _, err := CreateNetworkIdp(args, testAdminCommandSession(t, ctx, adminSession, []string{"all"}, adminJti)); err == nil {
			t.Fatal("an Admin's token added an IdP")
		}
		if _, found := testAdminTokenUseResult(ctx, adminJti); found {
			t.Fatal("a refused caller spent the token's use")
		}
		connect.AssertEqual(t, idpCount(), 1)

		// the check the consent page's request meets, as the route reads the body
		for _, body := range []string{
			`{"issuer":"https://idp.example","client_id":"c","client_auth":"client_secret","client_secret":"` + secret + `"}`,
			`{"issuer":"https://idp.example","client_id":"c","client_auth":"client_secret"}`,
			`{"issuer":"https://idp.example","client_id":"c","client_auth":"private_key_jwt","Client_Secret":"` + secret + `"}`,
			`not json`,
		} {
			err := session.CheckAdminCommandBody("POST", "/network/idp", []byte(body))
			if err == nil {
				t.Errorf("the body check passed %s", body)
			} else if strings.Contains(err.Error(), secret) {
				t.Errorf("the body check's refusal names the secret: %s", err)
			}
		}
		connect.AssertEqual(t, session.CheckAdminCommandBody("POST", "/network/idp", []byte(`{"issuer":"https://idp.example","client_id":"c","client_auth":"private_key_jwt"}`)), nil)
	})
}

// The three domain routes: each runs a token's command once and a replay answers the first, domain-verify's replay
// looking nothing up whether the first found the record or not; a replay that meets a first use with no answer yet,
// as domain-verify's lookup leaves one between its transactions, runs nothing and is told so; an Admin's token is
// refused by the Owner's routes and spends nothing.
func TestDomainRoutesByAdminTokenOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		domains := newTestDomains()
		networkId, ownerSession := newTestDomainTeam(t, ctx)
		token := func(jti string) *session.ClientSession {
			return testAdminCommandSession(t, ctx, ownerSession, []string{"all"}, jti)
		}
		args := &NetworkDomainArgs{Domain: "corp.example"}

		addJti := server.NewId().String()
		added, err := domains.add(args, token(addJti))
		if err != nil || added.Error != nil || added.Domain == nil {
			t.Fatalf("add: %v %+v", err, added)
		}
		replayAdded, err := domains.add(args, token(addJti))
		if err != nil || replayAdded.Domain == nil || replayAdded.Domain.TxtValue != added.Domain.TxtValue {
			t.Fatalf("add's replay: %v %+v", err, replayAdded)
		}
		connect.AssertEqual(t, testAdminEventCount(ctx, networkId, AdminEventActionDomainAdd), 1)

		notFoundJti := server.NewId().String()
		notFound, err := domains.verify(args, token(notFoundJti))
		if err != nil || notFound.Error == nil || notFound.Domain == nil || notFound.Domain.Verified {
			t.Fatalf("verify, not found: %v %+v", err, notFound)
		}
		lookups := domains.resolver.lookupCount()
		connect.AssertEqual(t, lookups, 1)
		replayNotFound, err := domains.verify(args, token(notFoundJti))
		if err != nil || replayNotFound.Error == nil || replayNotFound.Error.Message != notFound.Error.Message {
			t.Fatalf("verify's replay, not found: %v %+v", err, replayNotFound)
		}
		connect.AssertEqual(t, domains.resolver.lookupCount(), lookups)

		domains.advance(time.Minute)
		domains.resolver.set(NetworkDomainChallengeName("corp.example"), added.Domain.TxtValue)
		verifyJti := server.NewId().String()
		verified, err := domains.verify(args, token(verifyJti))
		if err != nil || verified.Error != nil || verified.Domain == nil || !verified.Domain.Verified {
			t.Fatalf("verify: %v %+v", err, verified)
		}
		lookups = domains.resolver.lookupCount()
		domains.advance(time.Minute)
		replayVerified, err := domains.verify(args, token(verifyJti))
		if err != nil || replayVerified.Domain == nil || !replayVerified.Domain.Verified {
			t.Fatalf("verify's replay: %v %+v", err, replayVerified)
		}
		connect.AssertEqual(t, domains.resolver.lookupCount(), lookups)
		connect.AssertEqual(t, testAdminEventCount(ctx, networkId, AdminEventActionDomainVerify), 1)

		// a first use that claimed and has not answered: its replay runs nothing
		unansweredJti := server.NewId().String()
		server.Tx(ctx, func(tx server.PgTx) {
			_, first, err := ClaimAdminTokenUseInTx(ctx, tx, token(unansweredJti).AdminCommand, domains.nowTime())
			server.Raise(err)
			connect.AssertEqual(t, first, true)
		})
		lookups = domains.resolver.lookupCount()
		if _, err := domains.verify(args, token(unansweredJti)); err == nil || !strings.HasPrefix(err.Error(), "409 ") {
			t.Fatalf("a replay of an unanswered use: %v", err)
		}
		connect.AssertEqual(t, domains.resolver.lookupCount(), lookups)

		removeJti := server.NewId().String()
		removed, err := domains.remove(args, token(removeJti))
		if err != nil || !removed.Removed {
			t.Fatalf("remove: %v %+v", err, removed)
		}
		replayRemoved, err := domains.remove(args, token(removeJti))
		if err != nil || !replayRemoved.Removed {
			t.Fatalf("remove's replay answers the first: %v %+v", err, replayRemoved)
		}
		connect.AssertEqual(t, testAdminEventCount(ctx, networkId, AdminEventActionDomainRemove), 1)
		connect.AssertEqual(t, GetNetworkDomain(ctx, networkId, "corp.example") == nil, true)

		if _, err := domains.add(args, testAdminCommandSession(t, ctx, ownerSession, []string{"auth_keys"}, server.NewId().String())); err == nil {
			t.Fatal("a token without all added a domain")
		}
		adminSession := newTestTeamMemberSession(t, ctx, networkId, NetworkMemberRoleAdmin, false)
		adminJti := server.NewId().String()
		if _, err := domains.add(args, testAdminCommandSession(t, ctx, adminSession, []string{"all"}, adminJti)); err == nil {
			t.Fatal("an Admin's token added a domain")
		}
		if _, found := testAdminTokenUseResult(ctx, adminJti); found {
			t.Fatal("a refused caller spent the token's use")
		}
		connect.AssertEqual(t, GetNetworkDomain(ctx, networkId, "corp.example") == nil, true)
	})
}

// POST /network/audit-webhook and GET /network/admin-events: an added webhook's signing key is answered to the first
// use alone and never rests in admin_token_use, a replay answering the webhook without it; a list runs once; an
// Auditor's token reads the first page of at most AdminCommandReadLimit events once, a replay answering the same page
// after more events; a Member's token is refused and spends nothing; a token without all reads nothing.
func TestAuditRoutesByAdminTokenOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		audits := newTestAudits()
		audits.eventSettings.AdminCommandReadLimit = 3
		networkId, _ := newTestSignInTeam(t, ctx, false)
		ownerSession := newTestTeamMemberSession(t, ctx, networkId, NetworkMemberRoleOwner, false)
		defer Testing_SetNetworkAuditWebhookRootKey(bytes.Repeat([]byte{9}, 32))()
		webhookArgs := &NetworkAuditWebhookArgs{Url: "https://siem.example/hook"}

		addJti := server.NewId().String()
		added, err := audits.setWebhook(webhookArgs, testAdminCommandSession(t, ctx, ownerSession, []string{"all"}, addJti))
		if err != nil || added.Error != nil || added.WebhookId == nil || added.SigningKey == "" {
			t.Fatalf("add: %v %+v", err, added)
		}
		stored, found := testAdminTokenUseResult(ctx, addJti)
		if !found || !strings.Contains(string(stored), added.WebhookId.String()) || strings.Contains(string(stored), added.SigningKey) {
			t.Fatalf("the stored answer: %s", stored)
		}
		replay, err := audits.setWebhook(webhookArgs, testAdminCommandSession(t, ctx, ownerSession, []string{"all"}, addJti))
		if err != nil || replay.WebhookId == nil || *replay.WebhookId != *added.WebhookId || replay.SigningKey != "" || len(replay.Webhooks) != 1 {
			t.Fatalf("add's replay: %v %+v", err, replay)
		}
		connect.AssertEqual(t, testTeamCount(ctx, `SELECT COUNT(*) FROM network_audit_webhook WHERE network_id = $1`, networkId), 1)
		connect.AssertEqual(t, testAdminEventCount(ctx, networkId, AdminEventActionAuditWebhookAdd), 1)

		listed, err := audits.setWebhook(&NetworkAuditWebhookArgs{}, testAdminCommandSession(t, ctx, ownerSession, []string{"all"}, server.NewId().String()))
		if err != nil || listed.Error != nil || len(listed.Webhooks) != 1 || listed.WebhookId != nil || listed.SigningKey != "" {
			t.Fatalf("list: %v %+v", err, listed)
		}

		addTestAdminEvents(ctx, networkId, 5)
		auditorSession := newTestTeamMemberSession(t, ctx, networkId, NetworkMemberRoleAuditor, false)
		eventsJti := server.NewId().String()
		page, err := audits.getEvents(&GetNetworkAdminEventsArgs{}, testAdminCommandSession(t, ctx, auditorSession, []string{"all"}, eventsJti))
		if err != nil || len(page.Events) != 3 || !page.More || page.Events[0].Seq != 1 {
			t.Fatalf("the first page: %v %+v", err, page)
		}
		connect.AssertEqual(t, VerifyNetworkAdminEventResults(networkId, page.Events), nil)
		addTestAdminEvents(ctx, networkId, 1)
		replayPage, err := audits.getEvents(&GetNetworkAdminEventsArgs{}, testAdminCommandSession(t, ctx, auditorSession, []string{"all"}, eventsJti))
		if err != nil || len(replayPage.Events) != 3 || replayPage.Events[2].ChainSha256 != page.Events[2].ChainSha256 {
			t.Fatalf("the page's replay: %v %+v", err, replayPage)
		}

		memberSession := newTestTeamMemberSession(t, ctx, networkId, NetworkMemberRoleMember, false)
		memberJti := server.NewId().String()
		if _, err := audits.getEvents(&GetNetworkAdminEventsArgs{}, testAdminCommandSession(t, ctx, memberSession, []string{"all"}, memberJti)); err == nil {
			t.Fatal("a Member's token read the events")
		}
		if _, found := testAdminTokenUseResult(ctx, memberJti); found {
			t.Fatal("a refused caller spent the token's use")
		}
		if _, err := audits.getEvents(&GetNetworkAdminEventsArgs{}, testAdminCommandSession(t, ctx, auditorSession, []string{"logs:read"}, server.NewId().String())); err == nil {
			t.Fatal("a token without all read the events")
		}
		adminJti := server.NewId().String()
		adminSession := newTestTeamMemberSession(t, ctx, networkId, NetworkMemberRoleAdmin, false)
		if _, err := audits.setWebhook(webhookArgs, testAdminCommandSession(t, ctx, adminSession, []string{"all"}, adminJti)); err == nil {
			t.Fatal("an Admin's token added a webhook")
		}
		if _, found := testAdminTokenUseResult(ctx, adminJti); found {
			t.Fatal("a refused caller spent the token's use")
		}
	})
}

// A page of AdminCommandReadLimit events of the longest form, every id present and every word at its column's width,
// fits ur-cli's 64 KiB answer bound (cli/urcli_token.go's ResponseMaxByteCount).
func TestAdminCommandEventPageFitsUrcliAnswer(t *testing.T) {
	settings := DefaultNetworkAdminEventSettings()
	id := server.NewId()
	result := &GetNetworkAdminEventsResult{More: true}
	for i := 0; i < settings.AdminCommandReadLimit; i += 1 {
		result.Events = append(result.Events, &NetworkAdminEventResult{
			Seq:         1<<63 - 1,
			EventTime:   time.Date(2026, time.November, 15, 23, 59, 59, 999999000, time.UTC),
			ActorKind:   strings.Repeat("k", adminEventKindMaxLength),
			ActorId:     &id,
			Action:      strings.Repeat("a", adminEventActionMaxLength),
			TargetKind:  strings.Repeat("t", adminEventKindMaxLength),
			TargetId:    &id,
			Result:      strings.Repeat("r", adminEventResultMaxLength),
			RequestId:   &id,
			PrevSha256:  strings.Repeat("0", 64),
			ChainSha256: strings.Repeat("f", 64),
		})
	}
	encoded, err := json.Marshal(result)
	connect.AssertEqual(t, err, nil)
	if 64*1024 <= len(encoded) {
		t.Fatalf("a page of %d events is %d bytes", settings.AdminCommandReadLimit, len(encoded))
	}
}
