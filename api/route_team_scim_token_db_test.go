//go:build sidecar_pending

package api

// §7.6's SCIM bearer through api's route set (QD-828), on Postgres with MG6: the team's Owner issues it with POST
// /network/scim-token and SCIM, behind the source api's route set installs, accepts it; a second issue retires the
// first; remove turns SCIM off; the route's class refuses a client token and the role gate every other role; and no
// answer or log line holds a bearer.

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/scim"
	"github.com/urnetwork/server/session"
)

// One request through handler with a JSON body and an Authorization header.
func serveScimTokenRoute(handler http.Handler, method string, path string, authorization string, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "198.51.100.43:4000"
	r.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

// A team on the team tier, as QUOTA-M6-01's period writes its grant, issues its SCIM bearer as its Owner through api's
// routes; SCIM accepts it at its base URL from a router built after the source was removed, since the route set
// installs it, and answers 401, never 503, to a bearer the team does not hold; the Owner's client token is refused by
// the route's class, issue and removal alike, and an Admin by the role gate, neither changing the bearer; a second
// issue retires the first; remove turns SCIM off. At the router's most verbose level no answer and no log line holds a
// bearer.
func TestScimTokenThroughTheApiRoutes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		teamId, ownerOfRecordId := server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, teamId, "team-"+teamId.String(), ownerOfRecordId)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_team(network_id) VALUES($1)`, teamId))
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO transfer_balance (
						balance_id,
						network_id,
						start_time,
						end_time,
						start_balance_byte_count,
						net_revenue_nano_cents,
						subsidy_net_revenue_nano_cents,
						balance_byte_count,
						pro,
						grant_kind,
						tier
					)
					VALUES ($1, $2, $3, $4, 0, 0, 0, 0, true, $5, $6)
				`,
				server.NewId(),
				teamId,
				now.Add(-time.Hour),
				now.Add(24*time.Hour),
				model.GrantKindTeam,
				string(model.NetworkTierTeam),
			))
		})
		memberByJwt := func(role model.NetworkMemberRole) *session.ByJwt {
			userId := server.NewId()
			model.Testing_CreateNetwork(ctx, server.NewId(), "member-"+userId.String(), userId)
			server.Tx(ctx, func(tx server.PgTx) {
				_, err := model.AddNetworkMemberInTx(ctx, tx, &model.AddNetworkMemberArgs{NetworkId: teamId, UserId: userId, Role: role})
				server.Raise(err)
			})
			signIn := model.ResolveNetworkSignIn(ctx, userId, teamId, server.NowUtc())
			if signIn == nil || signIn.MemberId == nil {
				t.Fatalf("no sign-in for the %s", role)
			}
			return model.NewNetworkSignInByJwt(signIn, false)
		}
		ownerByJwt := memberByJwt(model.NetworkMemberRoleOwner)
		ownerToken := "Bearer " + ownerByJwt.Testing_Sign()
		// a client of the Owner's sign-in, which carries the Owner's role but names a client
		ownerClientToken := "Bearer " + ownerByJwt.Client(server.NewId(), server.NewId()).Testing_Sign()
		adminToken := "Bearer " + memberByJwt(model.NetworkMemberRoleAdmin).Testing_Sign()

		previousVerbosity := flag.Lookup("v").Value.String()
		flag.Set("v", "2")
		defer flag.Set("v", previousVerbosity)
		read, write, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		realStderr := os.Stderr
		os.Stderr = write
		captured := make(chan []byte, 1)
		go func() {
			logged, _ := io.ReadAll(read)
			captured <- logged
		}()
		restoreStderr := func() string {
			if os.Stderr == realStderr {
				return ""
			}
			glog.Infof("[test]scim token capture canary\n")
			glog.Flush()
			os.Stderr = realStderr
			write.Close()
			return string(<-captured)
		}
		defer restoreStderr()

		// a process whose source was removed gets it back with the route set
		scim.SetBearerSource(nil)
		routerCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		apiRouter := router.NewRouter(routerCtx, Routes())
		scimPath := "/scim/v2/" + teamId.String()
		answers := []string{}
		serve := func(method string, path string, authorization string, body string) *httptest.ResponseRecorder {
			w := serveScimTokenRoute(apiRouter, method, path, authorization, body)
			answers = append(answers, w.Body.String())
			return w
		}
		if w := serve(http.MethodGet, scimPath+"/ServiceProviderConfig", "Bearer ur-scim-held-by-no-team", ""); w.Code != http.StatusUnauthorized {
			t.Fatalf("SCIM before an issue: %d %s", w.Code, w.Body.String())
		}

		// each issued bearer's own answer, the one answer that may hold it
		issuedAnswers := map[string]int{}
		issue := func(authorization string) (*model.NetworkScimTokenResult, *httptest.ResponseRecorder) {
			w := serve(http.MethodPost, "/network/scim-token", authorization, `{}`)
			result := &model.NetworkScimTokenResult{}
			if w.Code == http.StatusOK {
				if err := json.Unmarshal(w.Body.Bytes(), result); err != nil {
					t.Fatalf("the issue's answer %q: %v", w.Body.String(), err)
				}
				issuedAnswers[result.ScimBearer] = len(answers) - 1
			}
			return result, w
		}
		first, w := issue(ownerToken)
		if w.Code != http.StatusOK || !first.ScimOn || !strings.HasPrefix(first.ScimBearer, "ur-scim-") {
			t.Fatalf("the Owner's issue: %d %s", w.Code, w.Body.String())
		}
		scimUrl, err := url.Parse(first.ScimUrl)
		if err != nil || scimUrl.Path != scimPath {
			t.Fatalf("the answered base URL %q: %v", first.ScimUrl, err)
		}
		if w := serve(http.MethodGet, scimUrl.Path+"/ServiceProviderConfig", "Bearer "+first.ScimBearer, ""); w.Code != http.StatusOK {
			t.Fatalf("SCIM with the issued bearer: %d %s", w.Code, w.Body.String())
		}
		if w := serve(http.MethodGet, scimPath+"/Users", "Bearer "+first.ScimBearer+"x", ""); w.Code != http.StatusUnauthorized {
			t.Fatalf("SCIM with another bearer: %d %s", w.Code, w.Body.String())
		}

		for _, body := range []string{`{}`, `{"remove":true}`} {
			w := serve(http.MethodPost, "/network/scim-token", ownerClientToken, body)
			if w.Code != http.StatusForbidden || strings.TrimSpace(w.Body.String()) != router.ClientCredentialRefusedMessage {
				t.Fatalf("the Owner's client token with %s: %d %s", body, w.Code, w.Body.String())
			}
		}
		if _, w := issue(adminToken); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), router.RolePermissionRefusedMessage) {
			t.Fatalf("an Admin's issue: %d %s", w.Code, w.Body.String())
		}
		if w := serve(http.MethodGet, scimPath+"/Users", "Bearer "+first.ScimBearer, ""); w.Code != http.StatusOK {
			t.Fatalf("a refusal changed the bearer: %d %s", w.Code, w.Body.String())
		}

		second, w := issue(ownerToken)
		if w.Code != http.StatusOK || second.ScimBearer == "" || second.ScimBearer == first.ScimBearer {
			t.Fatalf("the second issue: %d %s", w.Code, w.Body.String())
		}
		if w := serve(http.MethodGet, scimPath+"/Users", "Bearer "+first.ScimBearer, ""); w.Code != http.StatusUnauthorized {
			t.Fatalf("the retired bearer: %d %s", w.Code, w.Body.String())
		}
		if w := serve(http.MethodGet, scimPath+"/Users", "Bearer "+second.ScimBearer, ""); w.Code != http.StatusOK {
			t.Fatalf("the second bearer: %d %s", w.Code, w.Body.String())
		}

		w = serve(http.MethodPost, "/network/scim-token", ownerToken, `{"remove":true}`)
		removed := &model.NetworkScimTokenResult{}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), removed) != nil || !removed.Removed || removed.ScimOn || removed.ScimBearer != "" {
			t.Fatalf("the removal: %d %s", w.Code, w.Body.String())
		}
		if w := serve(http.MethodGet, scimPath+"/Users", "Bearer "+second.ScimBearer, ""); w.Code != http.StatusUnauthorized {
			t.Fatalf("SCIM after the removal: %d %s", w.Code, w.Body.String())
		}

		logged := restoreStderr()
		if !strings.Contains(logged, "scim token capture canary") {
			t.Fatal("the capture read nothing glog wrote")
		}
		for _, bearer := range []string{first.ScimBearer, second.ScimBearer} {
			secret := strings.TrimPrefix(bearer, "ur-scim-")
			if strings.Contains(logged, secret) {
				t.Fatal("a bearer reached the log")
			}
			for index, answer := range answers {
				if strings.Contains(answer, secret) && index != issuedAnswers[bearer] {
					t.Fatalf("answer %d holds a bearer it did not issue", index)
				}
			}
		}
	})
}

// The role gate at POST /network/scim-token: the team's Owner reaches the handler and every other role, an Admin
// included, is refused 403 RolePermissionRefused before it.
func TestScimTokenRouteAtTheRoleGate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		const routeKey = "POST /network/scim-token"
		teamId, ownerOfRecordId := server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, teamId, "team-"+teamId.String(), ownerOfRecordId)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_team(network_id) VALUES($1)`, teamId))
		})
		for _, role := range []model.NetworkMemberRole{
			model.NetworkMemberRoleOwner,
			model.NetworkMemberRoleAdmin,
			model.NetworkMemberRoleNetworkAdmin,
			model.NetworkMemberRoleItAdmin,
			model.NetworkMemberRoleBillingAdmin,
			model.NetworkMemberRoleAuditor,
			model.NetworkMemberRoleMember,
		} {
			userId := server.NewId()
			model.Testing_CreateNetwork(ctx, server.NewId(), "member-"+userId.String(), userId)
			server.Tx(ctx, func(tx server.PgTx) {
				_, err := model.AddNetworkMemberInTx(ctx, tx, &model.AddNetworkMemberArgs{NetworkId: teamId, UserId: userId, Role: role})
				server.Raise(err)
			})
			signIn := model.ResolveNetworkSignIn(ctx, userId, teamId, server.NowUtc())
			if signIn == nil || signIn.MemberId == nil {
				t.Fatalf("no sign-in for the %s", role)
			}
			reached := false
			route := testRoleGatedRoute(t, routeKey, &reached)
			w := testRoleServe(route, http.MethodPost, "/network/scim-token", model.NewNetworkSignInByJwt(signIn, false).Testing_Sign())
			want := role == model.NetworkMemberRoleOwner
			if reached != want {
				t.Errorf("the %s reaches the SCIM bearer: %t, want %t", role, reached, want)
			}
			if !reached && (w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), router.RolePermissionRefusedMessage)) {
				t.Errorf("the SCIM bearer for the %s: %d %s", role, w.Code, w.Body.String())
			}
		}
	})
}
