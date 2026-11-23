//go:build sidecar_pending

package api

// §7.5's "Member: own devices" through api's routes (R-TEAM-2, D-TEAM-3), on Postgres with MG1 and MG6. A team Member's
// credential was refused 403 RolePermissionRefused at POST /network/auth-keys, which asked the keys permission, so a
// Member's device could never enroll (e2e-control-plane-concurrency's checkTeamMemberOwnKey).

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

// A team Member's network credential creates an untagged one-use key at POST /network/auth-keys, answered 200, and the
// key enrolls at POST /network/enroll a device whose owner row names that Member: its user and its member_id, untagged,
// with no sponsor. The Member is refused 403 a tagged key and a pre-approved one, which would enroll a tagged sidecar or
// land whatever admission a Delegation signed, none of it creating a key. An Admin's tagged, pre-approved key is
// unchanged, and the Member's one-use key enrolls once. Through GET /network/auth-keys and POST /network/auth-key-revoke
// the Member lists its own key alone and revokes it, while the Admin's key answers it as no such key and stays, and
// the Admin lists both.
func TestTeamMemberAuthKeyEnrollsItsOwnDevice(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		teamId, ownerOfRecordId := server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, teamId, "team-"+teamId.String(), ownerOfRecordId)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_team(network_id) VALUES($1)`, teamId))
		})
		t.Cleanup(server.Testing_PushSidecarConfig(fmt.Sprintf("sidecar:\n  preview_networks: [%q]\n", teamId)))
		memberOf := func(role model.NetworkMemberRole) (*model.NetworkMember, string) {
			userId := server.NewId()
			model.Testing_CreateNetwork(ctx, server.NewId(), "member-"+userId.String(), userId)
			var member *model.NetworkMember
			server.Tx(ctx, func(tx server.PgTx) {
				var err error
				member, err = model.AddNetworkMemberInTx(ctx, tx, &model.AddNetworkMemberArgs{NetworkId: teamId, UserId: userId, Role: role})
				server.Raise(err)
			})
			signIn := model.ResolveNetworkSignIn(ctx, userId, teamId, server.NowUtc())
			if signIn == nil || signIn.MemberId == nil || *signIn.MemberId != member.MemberId {
				t.Fatalf("no sign-in for the %s", role)
			}
			return member, "Bearer " + model.NewNetworkSignInByJwt(signIn, false).Testing_Sign()
		}
		member, memberToken := memberOf(model.NetworkMemberRoleMember)
		admin, adminToken := memberOf(model.NetworkMemberRoleAdmin)

		routerCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		apiRouter := router.NewRouter(routerCtx, Routes())
		serve := func(method string, path string, authorization string, body string) *httptest.ResponseRecorder {
			r := httptest.NewRequest(method, path, strings.NewReader(body))
			r.RemoteAddr = "198.51.100.44:4000"
			r.Header.Set("Content-Type", "application/json")
			if authorization != "" {
				r.Header.Set("Authorization", authorization)
			}
			w := httptest.NewRecorder()
			apiRouter.ServeHTTP(w, r)
			return w
		}
		createKey := func(authorization string, args *model.CreateAuthKeyArgs) (*model.CreateAuthKeyResult, *httptest.ResponseRecorder) {
			body, err := json.Marshal(args)
			if err != nil {
				t.Fatal(err)
			}
			w := serve(http.MethodPost, "/network/auth-keys", authorization, string(body))
			result := &model.CreateAuthKeyResult{}
			if w.Code == http.StatusOK {
				if err := json.Unmarshal(w.Body.Bytes(), result); err != nil {
					t.Fatalf("the key's answer %q: %v", w.Body.String(), err)
				}
			}
			return result, w
		}
		keyCountOf := func(creatorUserId server.Id) (count int) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(
					ctx,
					`SELECT COUNT(*) FROM network_auth_key WHERE network_id = $1 AND creator_user_id = $2`,
					teamId,
					creatorUserId,
				).Scan(&count))
			})
			return
		}

		own, w := createKey(memberToken, &model.CreateAuthKeyArgs{DurationSeconds: 3600, MaxUses: 1})
		if w.Code != http.StatusOK || own.Error != nil || own.AuthKey == "" || own.AuthKeyId == nil {
			t.Fatalf("the Member's key for its own device: %d %s", w.Code, w.Body.String())
		}

		tagId := strings.Repeat("ef", 16)
		for _, args := range []*model.CreateAuthKeyArgs{
			{DurationSeconds: 3600, MaxUses: 1, Tags: []string{tagId}},
			{DurationSeconds: 3600, MaxUses: 1, PreApproved: true},
		} {
			if _, w := createKey(memberToken, args); w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), "ur-authkey-") {
				t.Errorf("the Member's key %+v: %d %s", args, w.Code, w.Body.String())
			}
		}
		if count := keyCountOf(member.UserId); count != 1 {
			t.Fatalf("the Member holds %d keys, want its one own", count)
		}

		adminKey, w := createKey(adminToken, &model.CreateAuthKeyArgs{DurationSeconds: 3600, MaxUses: 1, Tags: []string{tagId}, PreApproved: true})
		if w.Code != http.StatusOK || adminKey.Error != nil || adminKey.AuthKey == "" {
			t.Fatalf("the Admin's tagged, pre-approved key: %d %s", w.Code, w.Body.String())
		}
		if count := keyCountOf(admin.UserId); count != 1 {
			t.Fatalf("the Admin holds %d keys", count)
		}

		listed := func(authorization string) []server.Id {
			t.Helper()
			w := serve(http.MethodGet, "/network/auth-keys", authorization, "")
			result := &model.GetNetworkAuthKeysResult{}
			if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), result) != nil {
				t.Fatalf("a listing: %d %s", w.Code, w.Body.String())
			}
			authKeyIds := []server.Id{}
			for _, authKey := range result.AuthKeys {
				authKeyIds = append(authKeyIds, authKey.AuthKeyId)
			}
			return authKeyIds
		}
		revoke := func(authorization string, authKeyId server.Id) *model.RevokeAuthKeyResult {
			t.Helper()
			w := serve(http.MethodPost, "/network/auth-key-revoke", authorization, fmt.Sprintf(`{"auth_key_id":%q}`, authKeyId.String()))
			result := &model.RevokeAuthKeyResult{}
			if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), result) != nil {
				t.Fatalf("a revoke: %d %s", w.Code, w.Body.String())
			}
			return result
		}
		if authKeyIds := listed(memberToken); len(authKeyIds) != 1 || authKeyIds[0] != *own.AuthKeyId {
			t.Fatalf("the Member lists %v, want its own key %s alone", authKeyIds, own.AuthKeyId)
		}
		if result := revoke(memberToken, *adminKey.AuthKeyId); result.Error == nil || result.BundleStillAdmits {
			t.Fatalf("the Member's revoke of the Admin's key answered %+v", result)
		}
		if count := keyCountOf(admin.UserId); count != 1 {
			t.Fatalf("the Admin holds %d keys after the Member's revoke", count)
		}
		if authKeyIds := listed(adminToken); len(authKeyIds) != 2 {
			t.Fatalf("the Admin lists %v, want both keys", authKeyIds)
		}

		enroll := func(authKey string) *httptest.ResponseRecorder {
			clientPublicKey, _, err := ed25519.GenerateKey(nil)
			if err != nil {
				t.Fatal(err)
			}
			body, err := json.Marshal(&model.NetworkEnrollArgs{
				AuthKey:         authKey,
				ClientPublicKey: clientPublicKey,
				DeviceSpec:      "member-own-device",
				AdminRefused:    true,
				StrictExp:       true,
			})
			if err != nil {
				t.Fatal(err)
			}
			return serve(http.MethodPost, "/network/enroll", "", string(body))
		}
		w = enroll(own.AuthKey)
		enrolled := &model.NetworkEnrollResult{}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), enrolled) != nil || enrolled.ClientId == (server.Id{}) {
			t.Fatalf("the Member's key's enrollment: %d %s", w.Code, w.Body.String())
		}
		var ownerUserId, memberId, sponsorUserId, authKeyId *server.Id
		var tagged, ephemeral bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`
					SELECT owner_user_id, member_id, sponsor_user_id, auth_key_id, tagged, ephemeral
					FROM network_client_owner
					WHERE client_id = $1
				`,
				enrolled.ClientId,
			).Scan(&ownerUserId, &memberId, &sponsorUserId, &authKeyId, &tagged, &ephemeral))
		})
		if ownerUserId == nil || *ownerUserId != member.UserId || memberId == nil || *memberId != member.MemberId ||
			sponsorUserId != nil || authKeyId == nil || *authKeyId != *own.AuthKeyId || tagged || ephemeral {
			t.Fatalf("the Member's device's owner row: owner %v member %v sponsor %v key %v tagged %t ephemeral %t",
				ownerUserId, memberId, sponsorUserId, authKeyId, tagged, ephemeral)
		}

		// the one-use key is spent: a second device is refused and mints nothing
		if w := enroll(own.AuthKey); w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), model.NetworkEnrollCodeAuthKeyInvalid) {
			t.Fatalf("a second enrollment from the Member's one-use key: %d %s", w.Code, w.Body.String())
		}

		// and the Member revokes its own key
		if result := revoke(memberToken, *own.AuthKeyId); result.Error != nil || result.BundleStillAdmits {
			t.Fatalf("the Member's revoke of its own key answered %+v", result)
		}
		if count := keyCountOf(member.UserId); count != 0 {
			t.Fatalf("the Member holds %d keys after revoking its own", count)
		}
		if authKeyIds := listed(memberToken); len(authKeyIds) != 0 {
			t.Fatalf("the Member lists %v after revoking its own key", authKeyIds)
		}
	})
}
