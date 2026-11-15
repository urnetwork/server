//go:build sidecar_pending

package scim

// §7.6's SCIM (scim.go; TEAM-M6-07), which needs MG6, through the router as an IdP calls it: the revocation suite's
// SCIM active=false (denied at once at the server, queued for a signature), in Okta's and Entra's PATCH forms and as a
// DELETE; provisioning queued for an invite and withdrawn; groups queued and read back; the queue's 7 days; every
// refusal of the bearer and the tier; and RFC 7644's message shapes by fixture. The team tier is stubbed on, since
// QUOTA-M6-01's tier is not in transfer_balance yet, and the bearer source is a test's, since where the bearer lives is
// open (questions/team-m6-07-scim-bearer-row.md).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// The test's SCIM bearer.
const scimTestBearer = "ur-scim-test-bearer-for-the-scim-tests"

// A bearer source a test fills.
type scimTestBearers struct {
	stateLock sync.Mutex
	hashes    map[server.Id][]byte
}

func (self *scimTestBearers) BearerSha256(ctx context.Context, networkId server.Id) ([]byte, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	hash, ok := self.hashes[networkId]
	return hash, ok
}

// A team with an Owner and members, its bearer installed, the tier on, and the SCIM routes behind a router.
type scimFixture struct {
	ctx           context.Context
	teamNetworkId server.Id
	owner         *model.NetworkMember
	ownerSession  *session.ClientSession
	router        *router.Router
	now           time.Time
	nowLock       sync.Mutex
	tierOn        bool
	settings      *ScimSettings
}

func newScimFixture(t testing.TB, label string) *scimFixture {
	t.Helper()
	ctx := context.Background()
	_, restoreSearch := model.Testing_InMemoryNetworkNameSearch(ctx)
	t.Cleanup(restoreSearch)
	personalNetworkId := server.NewId()
	ownerUserId := server.NewId()
	suffix := strings.ReplaceAll(server.NewId().String(), "-", "")[24:32]
	personalName := fmt.Sprintf("%s-owner-%s", label, suffix)
	model.Testing_CreateNetwork(ctx, personalNetworkId, personalName, ownerUserId)
	personal := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: personalNetworkId, UserId: ownerUserId, NetworkName: personalName})
	created, err := model.CreateNetworkTeam(&model.CreateNetworkTeamArgs{NetworkName: fmt.Sprintf("%s-team-%s", label, suffix)}, personal)
	if err != nil || created.Error != nil || created.NetworkId == nil {
		t.Fatalf("team-create: %v %+v", err, created)
	}
	self := &scimFixture{
		ctx:           ctx,
		teamNetworkId: *created.NetworkId,
		now:           server.NowUtc(),
		tierOn:        true,
		settings:      DefaultScimSettings(),
	}
	self.owner = model.GetNetworkMemberOfUser(ctx, self.teamNetworkId, ownerUserId)
	ownerMemberId := self.owner.MemberId
	self.ownerSession = session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: self.teamNetworkId, UserId: ownerUserId, MemberId: &ownerMemberId})
	hash := sha256.Sum256([]byte(scimTestBearer))
	SetBearerSource(&scimTestBearers{hashes: map[server.Id][]byte{self.teamNetworkId: hash[:]}})
	t.Cleanup(func() { SetBearerSource(nil) })
	t.Cleanup(testingSetService(self.settings, self.clock, func(ctx context.Context, networkId server.Id) error {
		if self.tierOn {
			return nil
		}
		return model.RequireTeamTier(ctx, server.NewId())
	}))
	self.router = router.NewRouter(ctx, Routes())
	return self
}

func (self *scimFixture) clock() time.Time {
	self.nowLock.Lock()
	defer self.nowLock.Unlock()
	return self.now
}

func (self *scimFixture) advance(timeout time.Duration) {
	self.nowLock.Lock()
	defer self.nowLock.Unlock()
	self.now = self.now.Add(timeout)
}

// A member of the team with an email and a role.
func (self *scimFixture) addMember(t testing.TB, email string, role model.NetworkMemberRole) *model.NetworkMember {
	t.Helper()
	var member *model.NetworkMember
	server.Tx(self.ctx, func(tx server.PgTx) {
		var err error
		member, err = model.AddNetworkMemberInTx(self.ctx, tx, &model.AddNetworkMemberArgs{NetworkId: self.teamNetworkId, UserId: server.NewId(), Email: &email, Role: role})
		server.Raise(err)
	})
	return member
}

// An untagged client of the member, as an enrollment's owner row records it.
func (self *scimFixture) addClient(t testing.TB, member *model.NetworkMember) server.Id {
	t.Helper()
	clientId := server.NewId()
	model.Testing_CreateDevice(self.ctx, self.teamNetworkId, server.NewId(), clientId, "scim-device", "test")
	server.Tx(self.ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(self.ctx, `INSERT INTO network_client_owner (client_id, owner_user_id, member_id) VALUES ($1, $2, $3)`, clientId, member.UserId, member.MemberId))
	})
	return clientId
}

func (self *scimFixture) clientActive(t testing.TB, clientId server.Id) (active bool) {
	t.Helper()
	server.Db(self.ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(self.ctx, `SELECT active FROM network_client WHERE client_id = $1`, clientId).Scan(&active))
	})
	return
}

// One request through the router, with the bearer unless bearer is set to another or "" for none.
func (self *scimFixture) do(t testing.TB, method string, path string, body any, bearer ...string) (int, map[string]any, http.Header) {
	t.Helper()
	var reader *bytes.Reader
	switch value := body.(type) {
	case nil:
		reader = bytes.NewReader(nil)
	case string:
		reader = bytes.NewReader([]byte(value))
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, path, reader)
	request.RemoteAddr = "192.0.2.10:443"
	token := scimTestBearer
	if 0 < len(bearer) {
		token = bearer[0]
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	request.Header.Set("Content-Type", scimContentType)
	recorder := httptest.NewRecorder()
	self.router.ServeHTTP(recorder, request)
	answer := map[string]any{}
	if 0 < recorder.Body.Len() {
		if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
			t.Fatalf("%s %s answered no JSON: %q", method, path, recorder.Body.String())
		}
	}
	return recorder.Code, answer, recorder.Header()
}

func (self *scimFixture) path(rest string) string {
	return "/scim/v2/" + self.teamNetworkId.String() + rest
}

// The team's queued changes of a kind, unexpired at the fixture's clock.
func (self *scimFixture) queued(t testing.TB, kind model.NetworkScimChangeKind) []*model.NetworkScimChange {
	t.Helper()
	var changes []*model.NetworkScimChange
	server.Tx(self.ctx, func(tx server.PgTx) {
		changes = model.GetNetworkScimChangesInTx(self.ctx, tx, self.teamNetworkId, self.clock().Add(-self.settings.Queue.ChangeRetention), kind)
	})
	return changes
}

// The team's admin event actions, in order.
func (self *scimFixture) eventActions() []string {
	actions := []string{}
	for _, event := range model.GetNetworkAdminEvents(self.ctx, self.teamNetworkId, 0, 200) {
		actions = append(actions, event.Action)
	}
	return actions
}

// The revocation suite's SCIM active=false, in Okta's PATCH form, Entra's (a string value under a path) and as a
// DELETE: each member is denied at once, its row gone and its untagged client deactivated, and its MemberRevoke queued
// with its email and role, cryptographic only once signed; it reads active=false while its queued change lasts; and
// each is an admin event by scim.
func TestScimDeactivationDeniesAtOnceAndQueuesTheMemberRevoke(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newScimFixture(t, "scim-deny")
		okta := self.addMember(t, "okta.person@example.com", model.NetworkMemberRoleMember)
		entra := self.addMember(t, "entra.person@example.com", model.NetworkMemberRoleAuditor)
		deleted := self.addMember(t, "deleted.person@example.com", model.NetworkMemberRoleItAdmin)
		oktaClient := self.addClient(t, okta)

		status, answer, header := self.do(t, "PATCH", self.path("/Users/"+okta.MemberId.String()), map[string]any{
			"schemas":    []string{schemaPatchOp},
			"Operations": []map[string]any{{"op": "replace", "value": map[string]any{"active": false}}},
		})
		connect.AssertEqual(t, status, http.StatusOK)
		connect.AssertEqual(t, answer["active"], false)
		connect.AssertEqual(t, answer["id"], okta.MemberId.String())
		connect.AssertEqual(t, header.Get("Content-Type"), scimContentType)
		connect.AssertEqual(t, model.GetNetworkMember(self.ctx, okta.MemberId) == nil, true)
		connect.AssertEqual(t, self.clientActive(t, oktaClient), false)

		status, answer, _ = self.do(t, "PATCH", self.path("/Users/"+entra.MemberId.String()), `{"schemas":["`+schemaPatchOp+`"],"Operations":[{"op":"Replace","path":"active","value":"False"}]}`)
		connect.AssertEqual(t, status, http.StatusOK)
		connect.AssertEqual(t, answer["active"], false)
		connect.AssertEqual(t, model.GetNetworkMember(self.ctx, entra.MemberId) == nil, true)

		status, _, _ = self.do(t, "DELETE", self.path("/Users/"+deleted.MemberId.String()), nil)
		connect.AssertEqual(t, status, http.StatusNoContent)
		connect.AssertEqual(t, model.GetNetworkMember(self.ctx, deleted.MemberId) == nil, true)

		revokes := self.queued(t, model.NetworkScimChangeKindMemberRevoke)
		connect.AssertEqual(t, len(revokes), 3)
		roles := map[server.Id]model.NetworkMemberRole{}
		for _, change := range revokes {
			detail := &model.NetworkScimMemberRevokeDetail{}
			connect.AssertEqual(t, json.Unmarshal(change.Detail, detail), nil)
			roles[*change.MemberId] = detail.Role
		}
		connect.AssertEqual(t, roles[okta.MemberId], model.NetworkMemberRoleMember)
		// an AdminSet member's removal needs k of n, which the console and admin devices read from the queue
		connect.AssertEqual(t, roles[deleted.MemberId], model.NetworkMemberRoleItAdmin)

		// a deactivated user reads active false while its queued change lasts, and a second deactivation is idempotent
		status, answer, _ = self.do(t, "GET", self.path("/Users/"+okta.MemberId.String()), nil)
		connect.AssertEqual(t, status, http.StatusOK)
		connect.AssertEqual(t, answer["active"], false)
		connect.AssertEqual(t, answer["userName"], "okta.person@example.com")
		status, _, _ = self.do(t, "DELETE", self.path("/Users/"+okta.MemberId.String()), nil)
		connect.AssertEqual(t, status, http.StatusNoContent)
		connect.AssertEqual(t, len(self.queued(t, model.NetworkScimChangeKindMemberRevoke)), 3)
		// a membership that ended is not made again by SCIM: only an invite makes one
		status, answer, _ = self.do(t, "PATCH", self.path("/Users/"+okta.MemberId.String()), map[string]any{
			"schemas":    []string{schemaPatchOp},
			"Operations": []map[string]any{{"op": "replace", "path": "active", "value": true}},
		})
		connect.AssertEqual(t, status, http.StatusBadRequest)
		connect.AssertEqual(t, answer["scimType"], scimTypeInvalidValue)

		deactivations := 0
		for _, action := range self.eventActions() {
			if action == model.AdminEventActionScimDeactivate {
				deactivations += 1
			}
		}
		connect.AssertEqual(t, deactivations, 3)

		// the console's and admin devices' read of the queue
		changes, err := model.Testing_GetNetworkScimChanges(&model.GetNetworkScimChangesArgs{}, self.ownerSession, self.clock, func(ctx context.Context, networkId server.Id) model.NetworkTier {
			return model.NetworkTierTeam
		})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(changes.Changes), 3)
	})
}

// POST /Users of a person who is no member queues a user_create for an admin device's invite, its id the change's;
// a filter finds it; a second provisioning of the same person, or of a member's email, is a conflict; and active=false
// withdraws it, after which it is not found.
func TestScimProvisioningQueuesAnInviteAndWithdraws(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newScimFixture(t, "scim-create")
		member := self.addMember(t, "already.member@example.com", model.NetworkMemberRoleMember)

		status, answer, _ := self.do(t, "POST", self.path("/Users"), map[string]any{
			"schemas":    []string{schemaUser},
			"userName":   "New.Person@Example.com",
			"externalId": "idp-00u1",
			"emails":     []map[string]any{{"value": "new.person@example.com", "primary": true}},
			"active":     true,
		})
		connect.AssertEqual(t, status, http.StatusCreated)
		connect.AssertEqual(t, answer["userName"], "new.person@example.com")
		createdId, _ := answer["id"].(string)
		creates := self.queued(t, model.NetworkScimChangeKindUserCreate)
		connect.AssertEqual(t, len(creates), 1)
		connect.AssertEqual(t, creates[0].ChangeId.String(), createdId)
		connect.AssertEqual(t, creates[0].MemberId == nil, true)

		status, answer, _ = self.do(t, "GET", self.path(`/Users?filter=userName+eq+%22new.person%40example.com%22`), nil)
		connect.AssertEqual(t, status, http.StatusOK)
		connect.AssertEqual(t, answer["totalResults"], float64(1))
		status, answer, _ = self.do(t, "GET", self.path(`/Users?filter=externalId+eq+%22idp-00u1%22`), nil)
		connect.AssertEqual(t, status, http.StatusOK)
		connect.AssertEqual(t, answer["totalResults"], float64(1))
		status, answer, _ = self.do(t, "GET", self.path(`/Users?filter=userName+eq+%22already.member%40example.com%22`), nil)
		connect.AssertEqual(t, status, http.StatusOK)
		resources, _ := answer["Resources"].([]any)
		connect.AssertEqual(t, len(resources), 1)
		connect.AssertEqual(t, resources[0].(map[string]any)["id"], member.MemberId.String())

		for _, userName := range []string{"new.person@example.com", "already.member@example.com"} {
			status, answer, _ = self.do(t, "POST", self.path("/Users"), map[string]any{"schemas": []string{schemaUser}, "userName": userName})
			connect.AssertEqual(t, status, http.StatusConflict)
			connect.AssertEqual(t, answer["scimType"], scimTypeUniqueness)
		}
		// no member is made from SCIM
		server.Tx(self.ctx, func(tx server.PgTx) {
			connect.AssertEqual(t, len(model.GetNetworkMembersByEmailInTx(self.ctx, tx, self.teamNetworkId, "new.person@example.com")), 0)
		})

		status, answer, _ = self.do(t, "PATCH", self.path("/Users/"+createdId), map[string]any{
			"schemas":    []string{schemaPatchOp},
			"Operations": []map[string]any{{"op": "replace", "path": "active", "value": false}},
		})
		connect.AssertEqual(t, status, http.StatusOK)
		connect.AssertEqual(t, answer["active"], false)
		connect.AssertEqual(t, len(self.queued(t, model.NetworkScimChangeKindUserCreate)), 0)
		status, _, _ = self.do(t, "GET", self.path("/Users/"+createdId), nil)
		connect.AssertEqual(t, status, http.StatusNotFound)
		// the member itself was never touched
		connect.AssertEqual(t, model.GetNetworkMember(self.ctx, member.MemberId) != nil, true)
	})
}

// Groups queue for an admin device and read back from the queue: create with a member, add and remove by PATCH in
// Okta's and Entra's forms, replace the name, delete; roles never sync, so a group grants nothing here.
func TestScimGroupsQueueAndReadBack(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newScimFixture(t, "scim-groups")
		alice := self.addMember(t, "alice@example.com", model.NetworkMemberRoleMember)
		bob := self.addMember(t, "bob@example.com", model.NetworkMemberRoleMember)

		status, answer, _ := self.do(t, "POST", self.path("/Groups"), map[string]any{
			"schemas":     []string{schemaGroup},
			"displayName": "engineering",
			"members":     []map[string]any{{"value": alice.MemberId.String()}},
		})
		connect.AssertEqual(t, status, http.StatusCreated)
		groupId, _ := answer["id"].(string)
		connect.AssertEqual(t, answer["displayName"], "engineering")

		status, _, _ = self.do(t, "PATCH", self.path("/Groups/"+groupId), map[string]any{
			"schemas": []string{schemaPatchOp},
			"Operations": []map[string]any{
				{"op": "add", "path": "members", "value": []map[string]any{{"value": bob.MemberId.String()}}},
				{"op": "replace", "path": "displayName", "value": "platform"},
			},
		})
		connect.AssertEqual(t, status, http.StatusNoContent)
		status, _, _ = self.do(t, "PATCH", self.path("/Groups/"+groupId), `{"schemas":["`+schemaPatchOp+`"],"Operations":[{"op":"Remove","path":"members[value eq \"`+alice.MemberId.String()+`\"]"}]}`)
		connect.AssertEqual(t, status, http.StatusNoContent)

		status, answer, _ = self.do(t, "GET", self.path("/Groups/"+groupId), nil)
		connect.AssertEqual(t, status, http.StatusOK)
		connect.AssertEqual(t, answer["displayName"], "platform")
		members, _ := answer["members"].([]any)
		connect.AssertEqual(t, len(members), 1)
		connect.AssertEqual(t, members[0].(map[string]any)["value"], bob.MemberId.String())
		status, answer, _ = self.do(t, "GET", self.path(`/Groups?filter=displayName+eq+%22platform%22`), nil)
		connect.AssertEqual(t, status, http.StatusOK)
		connect.AssertEqual(t, answer["totalResults"], float64(1))

		status, _, _ = self.do(t, "DELETE", self.path("/Groups/"+groupId), nil)
		connect.AssertEqual(t, status, http.StatusNoContent)
		status, _, _ = self.do(t, "GET", self.path("/Groups/"+groupId), nil)
		connect.AssertEqual(t, status, http.StatusNotFound)
		connect.AssertEqual(t, len(self.queued(t, model.NetworkScimChangeKindGroup)), 4)

		// a member that is no user id is refused, and nothing is queued for it
		status, _, _ = self.do(t, "POST", self.path("/Groups"), map[string]any{"displayName": "bad", "members": []map[string]any{{"value": "not-an-id"}}})
		connect.AssertEqual(t, status, http.StatusBadRequest)
		connect.AssertEqual(t, len(self.queued(t, model.NetworkScimChangeKindGroup)), 4)
		// the members' roles are the config repo's, untouched by any group
		connect.AssertEqual(t, model.GetNetworkMember(self.ctx, bob.MemberId).Role, model.NetworkMemberRoleMember)
	})
}

// A queued change lasts 7 days: past them reads leave it out, a deactivated user is not found, and the sweep deletes
// the rows.
func TestScimQueueExpiresAtSevenDays(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newScimFixture(t, "scim-expiry")
		member := self.addMember(t, "expiring@example.com", model.NetworkMemberRoleMember)
		status, _, _ := self.do(t, "DELETE", self.path("/Users/"+member.MemberId.String()), nil)
		connect.AssertEqual(t, status, http.StatusNoContent)
		connect.AssertEqual(t, len(self.queued(t, model.NetworkScimChangeKindMemberRevoke)), 1)

		self.advance(self.settings.Queue.ChangeRetention - time.Second)
		connect.AssertEqual(t, len(self.queued(t, model.NetworkScimChangeKindMemberRevoke)), 1)
		self.advance(2 * time.Second)
		connect.AssertEqual(t, len(self.queued(t, model.NetworkScimChangeKindMemberRevoke)), 0)
		status, _, _ = self.do(t, "GET", self.path("/Users/"+member.MemberId.String()), nil)
		connect.AssertEqual(t, status, http.StatusNotFound)
		changes, err := model.Testing_GetNetworkScimChanges(&model.GetNetworkScimChangesArgs{}, self.ownerSession, self.clock, func(ctx context.Context, networkId server.Id) model.NetworkTier {
			return model.NetworkTierTeam
		})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(changes.Changes), 0)

		removed := model.RemoveExpiredNetworkScimChanges(self.ctx, self.clock().Add(-self.settings.Queue.ChangeRetention))
		connect.AssertEqual(t, 1 <= removed, true)
	})
}

// The refusals: no bearer source (503), no bearer or another bearer (401, with WWW-Authenticate), a network outside
// the team tier (403), a path naming no network (404). Each is a SCIM error, and none changes the team.
func TestScimRefusals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newScimFixture(t, "scim-refuse")
		member := self.addMember(t, "refused@example.com", model.NetworkMemberRoleMember)
		target := self.path("/Users/" + member.MemberId.String())

		status, answer, header := self.do(t, "DELETE", target, nil, "")
		connect.AssertEqual(t, status, http.StatusUnauthorized)
		connect.AssertEqual(t, header.Get("WWW-Authenticate") != "", true)
		connect.AssertEqual(t, answer["status"], "401")
		status, _, _ = self.do(t, "DELETE", target, nil, "ur-scim-another-bearer")
		connect.AssertEqual(t, status, http.StatusUnauthorized)

		self.tierOn = false
		status, _, _ = self.do(t, "DELETE", target, nil)
		connect.AssertEqual(t, status, http.StatusForbidden)
		self.tierOn = true

		status, _, _ = self.do(t, "GET", "/scim/v2/not-a-network/Users", nil)
		connect.AssertEqual(t, status, http.StatusNotFound)

		SetBearerSource(nil)
		status, answer, _ = self.do(t, "DELETE", target, nil)
		connect.AssertEqual(t, status, http.StatusServiceUnavailable)
		connect.AssertEqual(t, answer["schemas"], []any{schemaError})

		connect.AssertEqual(t, model.GetNetworkMember(self.ctx, member.MemberId) != nil, true)
		connect.AssertEqual(t, len(self.queued(t, model.NetworkScimChangeKindMemberRevoke)), 0)
	})
}

// RFC 7644 conformance by fixture: the discovery documents' schemas and fields, a ListResponse's shape and paging, a
// filter this server does not answer (400 invalidFilter), and a malformed body (400 invalidSyntax).
func TestScimMessagesConformToRfc7644(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newScimFixture(t, "scim-rfc")
		for i := range 3 {
			self.addMember(t, fmt.Sprintf("listed-%d@example.com", i), model.NetworkMemberRoleMember)
		}
		status, answer, _ := self.do(t, "GET", self.path("/ServiceProviderConfig"), nil)
		connect.AssertEqual(t, status, http.StatusOK)
		connect.AssertEqual(t, answer["schemas"], []any{schemaServiceProviderConfig})
		connect.AssertEqual(t, answer["patch"].(map[string]any)["supported"], true)
		connect.AssertEqual(t, answer["bulk"].(map[string]any)["supported"], false)
		status, answer, _ = self.do(t, "GET", self.path("/ResourceTypes"), nil)
		connect.AssertEqual(t, status, http.StatusOK)
		connect.AssertEqual(t, answer["totalResults"], float64(2))
		status, answer, _ = self.do(t, "GET", self.path("/Schemas"), nil)
		connect.AssertEqual(t, status, http.StatusOK)
		connect.AssertEqual(t, answer["schemas"], []any{schemaListResponse})

		// four users (the Owner and three members), two to a page from the second
		status, answer, _ = self.do(t, "GET", self.path("/Users?startIndex=2&count=2"), nil)
		connect.AssertEqual(t, status, http.StatusOK)
		connect.AssertEqual(t, answer["schemas"], []any{schemaListResponse})
		connect.AssertEqual(t, answer["totalResults"], float64(4))
		connect.AssertEqual(t, answer["itemsPerPage"], float64(2))
		connect.AssertEqual(t, answer["startIndex"], float64(2))
		resources, _ := answer["Resources"].([]any)
		connect.AssertEqual(t, len(resources), 2)
		connect.AssertEqual(t, resources[0].(map[string]any)["schemas"], []any{schemaUser})

		status, answer, _ = self.do(t, "GET", self.path(`/Users?filter=name.familyName+co+%22x%22`), nil)
		connect.AssertEqual(t, status, http.StatusBadRequest)
		connect.AssertEqual(t, answer["scimType"], scimTypeInvalidFilter)
		status, answer, _ = self.do(t, "POST", self.path("/Users"), "{not json")
		connect.AssertEqual(t, status, http.StatusBadRequest)
		connect.AssertEqual(t, answer["scimType"], scimTypeInvalidSyntax)
		connect.AssertEqual(t, answer["schemas"], []any{schemaError})
	})
}
