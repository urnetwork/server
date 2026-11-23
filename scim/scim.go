// Package scim serves §7.6's SCIM 2.0 (RFC 7643, RFC 7644) for a team's IdP at /scim/v2/<network_id>/ (TEAM-M6-07;
// R-TEAM-4, D-TEAM-25, Q-TEAM-3, I-4, I-15).
//
// The IdP authenticates with the team's SCIM bearer, which the server keeps only hashed; this package compares the
// SHA-256 of the presented bearer with the network's in constant time. The hash is network_team.scim_token_sha256,
// which the team's Owner sets and clears with POST /network/scim-token (QD-828; model.SetNetworkScimToken), and
// NetworkTeamBearerSource reads it, which api's route set installs: a request whose network holds no hash, or another
// one, answers 401. Only a process with no source installed answers 503, and tests install their own.
//
//	users     a SCIM User is a member of the team, id its member_id, userName its email. Unsigned SCIM data adds no
//	          reader (§7.6): POST /Users of a person who is no member queues a user_create for an admin device to
//	          invite, its id the change's, and a member is never made here. active=false, by PATCH or PUT, or a
//	          DELETE, denies the member at once (model.DenyNetworkMemberInTx: its row, its untagged clients, the
//	          auth keys its user made in the team, its rev/ object; QD-1211) and queues its MemberRevoke, which is
//	          the removal only once an admin device, or for a member outside the AdminSet the indexer
//	          ServiceSettings.scim_signer names, signs it (D-TEAM-25); until then members still encrypt to its
//	          devices, which the console lists. The deactivated user reads active=false while its queued change
//	          lasts. Of a queued create, active=false or a DELETE withdraws it
//	groups    groups live in the config repo (owner decision 46), so every group request queues a group change for
//	          an admin device, and reads answer from the queue: a group the queue no longer holds is unknown here
//	roles     never sync (§7.6): no attribute here sets one
//	queue     every change lasts at most 7 days (model.NetworkScimSettings.ChangeRetention)
//
// A response never carries the bearer, and errors carry fixed text. The network must be a team on the team tier.
package scim

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// RFC 7643's and RFC 7644's schema URNs, and SCIM's media type.
const (
	schemaUser                  = "urn:ietf:params:scim:schemas:core:2.0:User"
	schemaGroup                 = "urn:ietf:params:scim:schemas:core:2.0:Group"
	schemaListResponse          = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
	schemaPatchOp               = "urn:ietf:params:scim:api:messages:2.0:PatchOp"
	schemaError                 = "urn:ietf:params:scim:api:messages:2.0:Error"
	schemaServiceProviderConfig = "urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig"
	schemaResourceType          = "urn:ietf:params:scim:schemas:core:2.0:ResourceType"
	schemaSchema                = "urn:ietf:params:scim:schemas:core:2.0:Schema"
	scimContentType             = "application/scim+json"
	// RFC 7644 §3.12's scimType values this package answers
	scimTypeInvalidFilter = "invalidFilter"
	scimTypeInvalidSyntax = "invalidSyntax"
	scimTypeInvalidValue  = "invalidValue"
	scimTypeUniqueness    = "uniqueness"
	scimTypeTooMany       = "tooMany"
)

// Tunables of the SCIM surface.
type ScimSettings struct {
	// The most resources one list answers, and the count a list without one asks: 100, RFC 7644 §3.4.2.4's count
	// capped where a page stays one response.
	ListMaxCount int
	// The longest request body read: 64 KiB, past a PUT of a group of ListMaxCount members.
	RequestMaxByteCount int64
	// The most operations one PATCH carries: 64.
	PatchMaxOperationCount int
	// The most members one group request names: 1,000.
	GroupMaxMemberCount int
	// The queue's tunables, its 7-day retention among them.
	Queue *model.NetworkScimSettings
}

func DefaultScimSettings() *ScimSettings {
	return &ScimSettings{
		ListMaxCount:           100,
		RequestMaxByteCount:    64 * 1024,
		PatchMaxOperationCount: 64,
		GroupMaxMemberCount:    1000,
		Queue:                  model.DefaultNetworkScimSettings(),
	}
}

// Where the team's SCIM bearer's hash comes from.
type BearerSource interface {
	// SHA-256 of the network's SCIM bearer, false when the network has none, so SCIM is off for it.
	BearerSha256(ctx context.Context, networkId server.Id) ([]byte, bool)
}

var bearerSource atomic.Pointer[BearerSource]

// Installs the process's bearer source; nil removes it, and every request answers 503.
func SetBearerSource(source BearerSource) {
	if source == nil {
		bearerSource.Store(nil)
		return
	}
	bearerSource.Store(&source)
}

func currentBearerSource() BearerSource {
	source := bearerSource.Load()
	if source == nil {
		return nil
	}
	return *source
}

// The source over network_team.scim_token_sha256 (model.GetNetworkTeamScimTokenSha256), which POST
// /network/scim-token sets and clears (QD-828): what api installs. It reads the primary at each request, so a new
// bearer or a removal holds from its commit on, and holds no state, so installing it again changes nothing.
func NetworkTeamBearerSource() BearerSource {
	return networkTeamBearerSource{}
}

type networkTeamBearerSource struct{}

func (self networkTeamBearerSource) BearerSha256(ctx context.Context, networkId server.Id) ([]byte, bool) {
	return model.GetNetworkTeamScimTokenSha256(ctx, networkId)
}

// What the handlers depend on apart from the database, so tests can replace it.
type service struct {
	settings    *ScimSettings
	now         func() time.Time
	requireTier func(ctx context.Context, networkId server.Id) error
}

var defaultService atomic.Pointer[service]

func init() {
	defaultService.Store(&service{
		settings:    DefaultScimSettings(),
		now:         server.NowUtc,
		requireTier: model.RequireTeamTier,
	})
}

// Replaces the handlers' service for a test, until the returned restore.
func testingSetService(settings *ScimSettings, now func() time.Time, requireTier func(ctx context.Context, networkId server.Id) error) func() {
	previous := defaultService.Swap(&service{settings: settings, now: now, requireTier: requireTier})
	return func() {
		defaultService.Store(previous)
	}
}

// The SCIM routes, which api serves (route_authz.go classifies each as public: the SCIM bearer is a credential of
// its own kind).
func Routes() []*router.Route {
	return []*router.Route{
		router.NewRoute("GET", "/scim/v2/([^/]+)/ServiceProviderConfig", ServiceProviderConfigHandler),
		router.NewRoute("GET", "/scim/v2/([^/]+)/ResourceTypes", ResourceTypesHandler),
		router.NewRoute("GET", "/scim/v2/([^/]+)/Schemas", SchemasHandler),
		router.NewRoute("GET", "/scim/v2/([^/]+)/Users", UsersHandler),
		router.NewRoute("POST", "/scim/v2/([^/]+)/Users", UsersHandler),
		router.NewRoute("GET", "/scim/v2/([^/]+)/Users/([^/]+)", UserHandler),
		router.NewRoute("PUT", "/scim/v2/([^/]+)/Users/([^/]+)", UserHandler),
		router.NewRoute("PATCH", "/scim/v2/([^/]+)/Users/([^/]+)", UserHandler),
		router.NewRoute("DELETE", "/scim/v2/([^/]+)/Users/([^/]+)", UserHandler),
		router.NewRoute("GET", "/scim/v2/([^/]+)/Groups", GroupsHandler),
		router.NewRoute("POST", "/scim/v2/([^/]+)/Groups", GroupsHandler),
		router.NewRoute("GET", "/scim/v2/([^/]+)/Groups/([^/]+)", GroupHandler),
		router.NewRoute("PUT", "/scim/v2/([^/]+)/Groups/([^/]+)", GroupHandler),
		router.NewRoute("PATCH", "/scim/v2/([^/]+)/Groups/([^/]+)", GroupHandler),
		router.NewRoute("DELETE", "/scim/v2/([^/]+)/Groups/([^/]+)", GroupHandler),
	}
}

// RFC 7644 §3.12's error.
type scimError struct {
	Schemas  []string `json:"schemas"`
	Status   string   `json:"status"`
	ScimType string   `json:"scimType,omitempty"`
	Detail   string   `json:"detail,omitempty"`
}

func writeScim(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", scimContentType)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if value != nil {
		json.NewEncoder(w).Encode(value)
	}
}

func writeScimError(w http.ResponseWriter, status int, scimType string, detail string) {
	writeScim(w, status, &scimError{Schemas: []string{schemaError}, Status: strconv.Itoa(status), ScimType: scimType, Detail: detail})
}

// One authorized request: its network and session.
type scimRequest struct {
	service       *service
	networkId     server.Id
	clientSession *session.ClientSession
	r             *http.Request
}

// Authorizes the request: a network id in the path, a bearer source installed, the network's bearer presented, and a
// team on the team tier. False after writing the refusal. The caller cancels the session.
func authorize(w http.ResponseWriter, r *http.Request) (*scimRequest, bool) {
	self := defaultService.Load()
	pathValues := router.GetPathValues(r)
	if len(pathValues) < 1 {
		writeScimError(w, http.StatusNotFound, "", "Not found.")
		return nil, false
	}
	networkId, err := server.ParseId(pathValues[0])
	if err != nil {
		writeScimError(w, http.StatusNotFound, "", "Not found.")
		return nil, false
	}
	if !server.SidecarMg6Linked() {
		writeScimError(w, http.StatusServiceUnavailable, "", "SCIM is not available.")
		return nil, false
	}
	source := currentBearerSource()
	if source == nil {
		writeScimError(w, http.StatusServiceUnavailable, "", "SCIM is not available.")
		return nil, false
	}
	clientSession, err := session.NewClientSessionFromRequest(r)
	if err != nil {
		writeScimError(w, http.StatusInternalServerError, "", "Please try again.")
		return nil, false
	}
	refuse := func(status int, detail string) (*scimRequest, bool) {
		clientSession.Cancel()
		if status == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", `Bearer realm="scim"`)
		}
		writeScimError(w, status, "", detail)
		return nil, false
	}
	authorization := r.Header.Get("Authorization")
	if len(authorization) <= len("bearer ") || !strings.EqualFold(authorization[:len("bearer ")], "bearer ") {
		return refuse(http.StatusUnauthorized, "A SCIM bearer is required.")
	}
	presented := sha256.Sum256([]byte(authorization[len("bearer "):]))
	stored, ok := source.BearerSha256(clientSession.Ctx, networkId)
	if !ok || len(stored) != sha256.Size || subtle.ConstantTimeCompare(stored, presented[:]) != 1 {
		return refuse(http.StatusUnauthorized, "The SCIM bearer is not valid.")
	}
	if !model.NetworkIsTeam(clientSession.Ctx, networkId) || self.requireTier(clientSession.Ctx, networkId) != nil {
		return refuse(http.StatusForbidden, "The network is not a team on the team tier.")
	}
	return &scimRequest{service: self, networkId: networkId, clientSession: clientSession, r: r}, true
}

// The request's body, decoded strictly enough to refuse what is not JSON, within the bound. False after writing the
// refusal.
func (self *scimRequest) decode(w http.ResponseWriter, value any) bool {
	body, err := io.ReadAll(io.LimitReader(self.r.Body, self.service.settings.RequestMaxByteCount+1))
	if err != nil || self.service.settings.RequestMaxByteCount < int64(len(body)) {
		writeScimError(w, http.StatusRequestEntityTooLarge, scimTypeTooMany, "The request is too large.")
		return false
	}
	if err := json.Unmarshal(body, value); err != nil {
		writeScimError(w, http.StatusBadRequest, scimTypeInvalidSyntax, "The request is not valid JSON.")
		return false
	}
	return true
}

// The resource's location under the api's origin, or its path where the origin is unknown.
func (self *scimRequest) location(resourceType string, id string) string {
	path := fmt.Sprintf("/scim/v2/%s/%s/%s", self.networkId, resourceType, id)
	if origin, err := session.DpopApiOrigin(); err == nil {
		return origin + path
	}
	return path
}

// The oldest create_time a queued change may have and still be read.
func (self *scimRequest) minCreateTime() time.Time {
	return self.service.now().Add(-self.service.settings.Queue.ChangeRetention)
}

// The event of an action the IdP made through SCIM.
func (self *scimRequest) adminEvent(action string, targetKind string, targetId *server.Id) *model.AdminEvent {
	event := &model.AdminEvent{
		NetworkId:  self.networkId,
		ActorKind:  model.AdminEventActorScim,
		Action:     action,
		TargetKind: targetKind,
		TargetId:   targetId,
		Result:     model.AdminEventResultOk,
	}
	if requestId, ok := session.RequestIdFromContext(self.clientSession.Ctx); ok {
		event.RequestId = &requestId
	}
	return event
}

// RFC 7643 §8.7.2's service provider configuration.
func ServiceProviderConfigHandler(w http.ResponseWriter, r *http.Request) {
	request, ok := authorize(w, r)
	if !ok {
		return
	}
	defer request.clientSession.Cancel()
	supported := func(value bool) map[string]bool {
		return map[string]bool{"supported": value}
	}
	writeScim(w, http.StatusOK, map[string]any{
		"schemas":        []string{schemaServiceProviderConfig},
		"patch":          supported(true),
		"bulk":           map[string]any{"supported": false, "maxOperations": 0, "maxPayloadSize": 0},
		"filter":         map[string]any{"supported": true, "maxResults": request.service.settings.ListMaxCount},
		"changePassword": supported(false),
		"sort":           supported(false),
		"etag":           supported(false),
		"authenticationSchemes": []map[string]any{{
			"type":        "oauthbearertoken",
			"name":        "Bearer",
			"description": "The team's SCIM bearer.",
			"primary":     true,
		}},
		"meta": map[string]string{"resourceType": "ServiceProviderConfig", "location": request.location("ServiceProviderConfig", "")},
	})
}

// RFC 7643 §6's resource types: User and Group.
func ResourceTypesHandler(w http.ResponseWriter, r *http.Request) {
	request, ok := authorize(w, r)
	if !ok {
		return
	}
	defer request.clientSession.Cancel()
	resourceTypes := []map[string]any{
		{"schemas": []string{schemaResourceType}, "id": "User", "name": "User", "endpoint": "/Users", "schema": schemaUser},
		{"schemas": []string{schemaResourceType}, "id": "Group", "name": "Group", "endpoint": "/Groups", "schema": schemaGroup},
	}
	writeScim(w, http.StatusOK, map[string]any{
		"schemas":      []string{schemaListResponse},
		"totalResults": len(resourceTypes),
		"itemsPerPage": len(resourceTypes),
		"startIndex":   1,
		"Resources":    resourceTypes,
	})
}

// RFC 7643 §7's schemas, the attributes this server keeps.
func SchemasHandler(w http.ResponseWriter, r *http.Request) {
	request, ok := authorize(w, r)
	if !ok {
		return
	}
	defer request.clientSession.Cancel()
	attribute := func(name string, kind string, required bool, mutability string) map[string]any {
		return map[string]any{"name": name, "type": kind, "multiValued": false, "required": required, "mutability": mutability, "returned": "default", "uniqueness": "none"}
	}
	schemas := []map[string]any{
		{
			"schemas": []string{schemaSchema}, "id": schemaUser, "name": "User",
			"attributes": []map[string]any{
				attribute("userName", "string", true, "readWrite"),
				attribute("displayName", "string", false, "readWrite"),
				attribute("externalId", "string", false, "readWrite"),
				attribute("active", "boolean", false, "readWrite"),
				{"name": "emails", "type": "complex", "multiValued": true, "required": false, "mutability": "readWrite", "returned": "default"},
			},
		},
		{
			"schemas": []string{schemaSchema}, "id": schemaGroup, "name": "Group",
			"attributes": []map[string]any{
				attribute("displayName", "string", true, "readWrite"),
				{"name": "members", "type": "complex", "multiValued": true, "required": false, "mutability": "readWrite", "returned": "default"},
			},
		},
	}
	writeScim(w, http.StatusOK, map[string]any{
		"schemas":      []string{schemaListResponse},
		"totalResults": len(schemas),
		"itemsPerPage": len(schemas),
		"startIndex":   1,
		"Resources":    schemas,
	})
}

// An email in a User.
type scimEmail struct {
	Value   string `json:"value"`
	Primary bool   `json:"primary,omitempty"`
	Type    string `json:"type,omitempty"`
}

type scimMeta struct {
	ResourceType string     `json:"resourceType"`
	Location     string     `json:"location,omitempty"`
	Created      *time.Time `json:"created,omitempty"`
}

// A User as this server answers it.
type scimUser struct {
	Schemas     []string    `json:"schemas"`
	Id          string      `json:"id"`
	ExternalId  string      `json:"externalId,omitempty"`
	UserName    string      `json:"userName"`
	DisplayName string      `json:"displayName,omitempty"`
	Emails      []scimEmail `json:"emails,omitempty"`
	Active      bool        `json:"active"`
	Meta        scimMeta    `json:"meta"`
}

// A User as an IdP sends it. active is raw: some IdPs send it as a string.
type scimUserInput struct {
	UserName    string          `json:"userName"`
	ExternalId  string          `json:"externalId"`
	DisplayName string          `json:"displayName"`
	Emails      []scimEmail     `json:"emails"`
	Active      json.RawMessage `json:"active"`
}

// A boolean as IdPs write one: true or false, or a string of either in any case. False for anything else.
func parseScimBool(raw json.RawMessage) (value bool, ok bool) {
	if len(raw) == 0 {
		return false, false
	}
	if json.Unmarshal(raw, &value) == nil {
		return value, true
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		switch strings.ToLower(strings.TrimSpace(text)) {
		case "true":
			return true, true
		case "false":
			return false, true
		}
	}
	return false, false
}

// An email as members store and match one: trimmed and lowercase.
func normalizeScimEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// The User of a member.
func (self *scimRequest) memberUser(member *model.NetworkMember) *scimUser {
	user := &scimUser{
		Schemas:  []string{schemaUser},
		Id:       member.MemberId.String(),
		UserName: member.MemberId.String(),
		Active:   true,
		Meta:     scimMeta{ResourceType: "User", Location: self.location("Users", member.MemberId.String())},
	}
	if member.Email != nil {
		user.UserName = *member.Email
		user.Emails = []scimEmail{{Value: *member.Email, Primary: true}}
	}
	return user
}

// The User of a deactivated member while its queued member_revoke lasts: active false.
func (self *scimRequest) deactivatedUser(change *model.NetworkScimChange) *scimUser {
	user := &scimUser{
		Schemas:  []string{schemaUser},
		Id:       change.MemberId.String(),
		UserName: change.MemberId.String(),
		Active:   false,
		Meta:     scimMeta{ResourceType: "User", Location: self.location("Users", change.MemberId.String())},
	}
	detail := &model.NetworkScimMemberRevokeDetail{}
	if json.Unmarshal(change.Detail, detail) == nil && detail.Email != nil {
		user.UserName = *detail.Email
		user.Emails = []scimEmail{{Value: *detail.Email, Primary: true}}
	}
	return user
}

// The User of a queued create: id the change's, active true.
func (self *scimRequest) createdUser(change *model.NetworkScimChange) *scimUser {
	detail := &model.NetworkScimUserCreateDetail{}
	json.Unmarshal(change.Detail, detail)
	created := change.CreateTime
	user := &scimUser{
		Schemas:     []string{schemaUser},
		Id:          change.ChangeId.String(),
		ExternalId:  detail.ExternalId,
		UserName:    detail.UserName,
		DisplayName: detail.DisplayName,
		Active:      true,
		Meta:        scimMeta{ResourceType: "User", Location: self.location("Users", change.ChangeId.String()), Created: &created},
	}
	for i, email := range detail.Emails {
		user.Emails = append(user.Emails, scimEmail{Value: email, Primary: i == 0})
	}
	return user
}

// What a User id names: a member, a deactivated member's queued revoke, a queued create, or nothing.
type scimUserTarget struct {
	member      *model.NetworkMember
	deactivated *model.NetworkScimChange
	created     *model.NetworkScimChange
}

func (self *scimRequest) userTargetInTx(ctx context.Context, tx server.PgTx, id server.Id) *scimUserTarget {
	if member := model.GetNetworkMemberInTx(ctx, tx, id); member != nil && member.NetworkId == self.networkId {
		return &scimUserTarget{member: member}
	}
	if change := model.GetNetworkScimChangeInTx(ctx, tx, self.networkId, id, self.minCreateTime()); change != nil && change.Kind == model.NetworkScimChangeKindUserCreate {
		return &scimUserTarget{created: change}
	}
	for _, change := range model.GetNetworkScimChangesInTx(ctx, tx, self.networkId, self.minCreateTime(), model.NetworkScimChangeKindMemberRevoke) {
		if change.MemberId != nil && *change.MemberId == id {
			return &scimUserTarget{deactivated: change}
		}
	}
	return nil
}

func (self *scimRequest) targetUser(target *scimUserTarget) *scimUser {
	switch {
	case target.member != nil:
		return self.memberUser(target.member)
	case target.deactivated != nil:
		return self.deactivatedUser(target.deactivated)
	default:
		return self.createdUser(target.created)
	}
}

// Denies a member at once and queues its MemberRevoke, in tx (the package header). The denial records the
// deactivation's admin event, after the auth_key.revoke event of each key it ends, which name SCIM as their actor.
func (self *scimRequest) deactivateInTx(ctx context.Context, tx server.PgTx, member *model.NetworkMember) (*model.NetworkScimChange, error) {
	now := self.service.now().UTC().Truncate(time.Microsecond)
	memberId := member.MemberId
	denied, err := model.DenyNetworkMemberInTx(ctx, tx, &model.NetworkMemberDenial{
		NetworkId: self.networkId,
		MemberId:  memberId,
		Time:      now,
		Event:     self.adminEvent(model.AdminEventActionScimDeactivate, model.AdminEventTargetMember, &memberId),
	})
	if err != nil {
		return nil, err
	}
	change, err := model.AddNetworkScimChangeInTx(ctx, tx, self.networkId, &memberId, model.NetworkScimChangeKindMemberRevoke, &model.NetworkScimMemberRevokeDetail{
		Email: denied.Member.Email,
		Role:  denied.Member.Role,
	}, now)
	if err != nil {
		return nil, err
	}
	if denied.Member.Role.AdminSet() {
		model.OweNetworkMemberDeniedNoticeInTx(ctx, tx, self.networkId, now)
	}
	return change, nil
}

// Withdraws a queued create, in tx.
func (self *scimRequest) withdrawInTx(ctx context.Context, tx server.PgTx, change *model.NetworkScimChange) {
	if model.RemoveNetworkScimChangeInTx(ctx, tx, self.networkId, change.ChangeId) {
		changeId := change.ChangeId
		model.AddAdminEventInTx(ctx, tx, self.adminEvent(model.AdminEventActionScimUserWithdraw, model.AdminEventTargetScimChange, &changeId))
	}
}

// RFC 7644 §3.4.2.2's filters this server answers: attribute eq "value" on userName, emails (emails.value or
// emails[value eq "..."]), externalId, id and displayName, case-insensitive on the attribute and the operator.
var scimFilterPattern = regexp.MustCompile(`(?i)^\s*(userName|externalId|id|displayName|emails\.value|emails\[\s*value\s+eq\s+"([^"]*)"\s*\])\s*(eq\s+"([^"]*)")?\s*$`)

// A group PATCH's path naming one member to remove: members[value eq "<id>"].
var scimGroupMemberPathPattern = regexp.MustCompile(`(?i)^members\[\s*value\s+eq\s+"([^"]+)"\s*\]$`)

type scimFilter struct {
	attribute string
	value     string
}

// The filter, nil for none; an error for one this server does not answer.
func parseScimFilter(text string) (*scimFilter, error) {
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	match := scimFilterPattern.FindStringSubmatch(text)
	if match == nil {
		return nil, errors.New("unsupported filter")
	}
	attribute := strings.ToLower(match[1])
	if strings.HasPrefix(attribute, "emails[") {
		if match[3] != "" {
			return nil, errors.New("unsupported filter")
		}
		return &scimFilter{attribute: "emails", value: match[2]}, nil
	}
	if match[3] == "" {
		return nil, errors.New("unsupported filter")
	}
	if attribute == "emails.value" {
		attribute = "emails"
	}
	return &scimFilter{attribute: attribute, value: match[4]}, nil
}

// RFC 7644 §3.4.2.4's startIndex and count, within the settings' bound.
func (self *scimRequest) page() (startIndex int, count int) {
	query := self.r.URL.Query()
	startIndex, _ = strconv.Atoi(query.Get("startIndex"))
	startIndex = max(startIndex, 1)
	count = self.service.settings.ListMaxCount
	if value, err := strconv.Atoi(query.Get("count")); err == nil {
		count = min(max(value, 0), self.service.settings.ListMaxCount)
	}
	return
}

func writeScimList[T any](w http.ResponseWriter, totalResults int, startIndex int, resources []T) {
	writeScim(w, http.StatusOK, map[string]any{
		"schemas":      []string{schemaListResponse},
		"totalResults": totalResults,
		"itemsPerPage": len(resources),
		"startIndex":   startIndex,
		"Resources":    resources,
	})
}

// /Users: GET lists, POST provisions.
func UsersHandler(w http.ResponseWriter, r *http.Request) {
	request, ok := authorize(w, r)
	if !ok {
		return
	}
	defer request.clientSession.Cancel()
	switch r.Method {
	case http.MethodGet:
		request.listUsers(w)
	case http.MethodPost:
		request.createUser(w)
	default:
		writeScimError(w, http.StatusMethodNotAllowed, "", "Not allowed.")
	}
}

// GET /Users: members, then deactivated members while their queued revoke lasts, then queued creates, a page at a
// time, or those a filter names.
func (self *scimRequest) listUsers(w http.ResponseWriter) {
	filter, err := parseScimFilter(self.r.URL.Query().Get("filter"))
	if err != nil {
		writeScimError(w, http.StatusBadRequest, scimTypeInvalidFilter, "The filter is not one this server answers.")
		return
	}
	startIndex, count := self.page()
	ctx := self.clientSession.Ctx
	users := []*scimUser{}
	totalResults := 0
	server.Tx(ctx, func(tx server.PgTx) {
		users, totalResults = []*scimUser{}, 0
		deactivated := model.GetNetworkScimChangesInTx(ctx, tx, self.networkId, self.minCreateTime(), model.NetworkScimChangeKindMemberRevoke)
		created := model.GetNetworkScimChangesInTx(ctx, tx, self.networkId, self.minCreateTime(), model.NetworkScimChangeKindUserCreate)
		if filter != nil {
			matches := []*scimUser{}
			candidates := []*scimUser{}
			switch filter.attribute {
			case "username", "emails":
				for _, member := range model.GetNetworkMembersByEmailInTx(ctx, tx, self.networkId, normalizeScimEmail(filter.value)) {
					candidates = append(candidates, self.memberUser(member))
				}
			case "id":
				if id, err := server.ParseId(filter.value); err == nil {
					if target := self.userTargetInTx(ctx, tx, id); target != nil {
						candidates = append(candidates, self.targetUser(target))
					}
				}
			}
			for _, change := range deactivated {
				candidates = append(candidates, self.deactivatedUser(change))
			}
			for _, change := range created {
				candidates = append(candidates, self.createdUser(change))
			}
			seen := map[string]bool{}
			for _, user := range candidates {
				if seen[user.Id] || !scimUserMatches(user, filter) {
					continue
				}
				seen[user.Id] = true
				matches = append(matches, user)
			}
			totalResults = len(matches)
			if startIndex-1 < len(matches) {
				users = matches[startIndex-1 : min(len(matches), startIndex-1+count)]
			}
			return
		}
		memberCount := model.CountNetworkMembersInTx(ctx, tx, self.networkId)
		totalResults = memberCount + len(deactivated) + len(created)
		offset := startIndex - 1
		if offset < memberCount {
			for _, member := range model.GetNetworkMembersPageInTx(ctx, tx, self.networkId, offset, count) {
				users = append(users, self.memberUser(member))
			}
		}
		rest := []*scimUser{}
		for _, change := range deactivated {
			rest = append(rest, self.deactivatedUser(change))
		}
		for _, change := range created {
			rest = append(rest, self.createdUser(change))
		}
		restOffset := max(offset-memberCount, 0)
		for i := restOffset; i < len(rest) && len(users) < count; i += 1 {
			users = append(users, rest[i])
		}
	})
	writeScimList(w, totalResults, startIndex, users)
}

// Whether a User matches an eq filter: userName and emails case-insensitively, the rest exactly.
func scimUserMatches(user *scimUser, filter *scimFilter) bool {
	switch filter.attribute {
	case "username":
		return strings.EqualFold(user.UserName, filter.value)
	case "emails":
		return slices.ContainsFunc(user.Emails, func(email scimEmail) bool { return strings.EqualFold(email.Value, filter.value) })
	case "externalid":
		return user.ExternalId != "" && user.ExternalId == filter.value
	case "id":
		return user.Id == filter.value
	case "displayname":
		return user.DisplayName != "" && user.DisplayName == filter.value
	}
	return false
}

// POST /Users: a person who is a member already is a conflict; anyone else is queued for an admin device's invite,
// since unsigned SCIM data makes no member (the package header).
func (self *scimRequest) createUser(w http.ResponseWriter) {
	input := &scimUserInput{}
	if !self.decode(w, input) {
		return
	}
	userName := normalizeScimEmail(input.UserName)
	if userName == "" {
		writeScimError(w, http.StatusBadRequest, scimTypeInvalidValue, "userName is required.")
		return
	}
	if active, ok := parseScimBool(input.Active); ok && !active {
		writeScimError(w, http.StatusBadRequest, scimTypeInvalidValue, "A user is provisioned active.")
		return
	}
	emails := []string{}
	for _, email := range input.Emails {
		if normalized := normalizeScimEmail(email.Value); normalized != "" && !slices.Contains(emails, normalized) {
			emails = append(emails, normalized)
		}
	}
	ctx := self.clientSession.Ctx
	var created *model.NetworkScimChange
	conflict := false
	server.Tx(ctx, func(tx server.PgTx) {
		created, conflict = nil, false
		for _, email := range append([]string{userName}, emails...) {
			if 0 < len(model.GetNetworkMembersByEmailInTx(ctx, tx, self.networkId, email)) {
				conflict = true
				return
			}
		}
		for _, change := range model.GetNetworkScimChangesInTx(ctx, tx, self.networkId, self.minCreateTime(), model.NetworkScimChangeKindUserCreate) {
			detail := &model.NetworkScimUserCreateDetail{}
			if json.Unmarshal(change.Detail, detail) == nil && strings.EqualFold(detail.UserName, userName) {
				conflict = true
				return
			}
		}
		change, err := model.AddNetworkScimChangeInTx(ctx, tx, self.networkId, nil, model.NetworkScimChangeKindUserCreate, &model.NetworkScimUserCreateDetail{
			UserName:    userName,
			Emails:      emails,
			ExternalId:  input.ExternalId,
			DisplayName: input.DisplayName,
		}, self.service.now())
		server.Raise(err)
		changeId := change.ChangeId
		model.AddAdminEventInTx(ctx, tx, self.adminEvent(model.AdminEventActionScimUserCreate, model.AdminEventTargetScimChange, &changeId))
		created = change
	})
	if conflict {
		writeScimError(w, http.StatusConflict, scimTypeUniqueness, "The user exists.")
		return
	}
	writeScim(w, http.StatusCreated, self.createdUser(created))
}

// RFC 7644 §3.5.2's PATCH request.
type scimPatchRequest struct {
	Schemas    []string             `json:"schemas"`
	Operations []scimPatchOperation `json:"Operations"`
}

type scimPatchOperation struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value"`
}

// The active value a User PATCH sets, nil for none: a replace or add of path active, or of no path with an object
// holding active. Other attributes are not kept here and are ignored. An error for a malformed request.
func scimPatchActive(request *scimPatchRequest, maxOperationCount int) (*bool, error) {
	if len(request.Operations) == 0 || maxOperationCount < len(request.Operations) {
		return nil, errors.New("a PATCH of 1 to the bound's operations")
	}
	var active *bool
	for _, operation := range request.Operations {
		op := strings.ToLower(operation.Op)
		if op != "replace" && op != "add" && op != "remove" {
			return nil, errors.New("an unknown operation")
		}
		if op == "remove" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(operation.Path)) {
		case "active":
			value, ok := parseScimBool(operation.Value)
			if !ok {
				return nil, errors.New("active is not a boolean")
			}
			active = &value
		case "":
			fields := map[string]json.RawMessage{}
			if json.Unmarshal(operation.Value, &fields) != nil {
				return nil, errors.New("a value with no path is not an object")
			}
			for name, raw := range fields {
				if strings.EqualFold(name, "active") {
					value, ok := parseScimBool(raw)
					if !ok {
						return nil, errors.New("active is not a boolean")
					}
					active = &value
				}
			}
		}
	}
	return active, nil
}

// /Users/<id>: GET reads, PUT and PATCH set active, DELETE deactivates.
func UserHandler(w http.ResponseWriter, r *http.Request) {
	request, ok := authorize(w, r)
	if !ok {
		return
	}
	defer request.clientSession.Cancel()
	id, err := server.ParseId(router.GetPathValues(r)[1])
	if err != nil {
		writeScimError(w, http.StatusNotFound, "", "Not found.")
		return
	}
	var active *bool
	switch r.Method {
	case http.MethodGet, http.MethodDelete:
	case http.MethodPut:
		input := &scimUserInput{}
		if !request.decode(w, input) {
			return
		}
		if len(input.Active) != 0 {
			value, ok := parseScimBool(input.Active)
			if !ok {
				writeScimError(w, http.StatusBadRequest, scimTypeInvalidValue, "active is not a boolean.")
				return
			}
			active = &value
		}
	case http.MethodPatch:
		patch := &scimPatchRequest{}
		if !request.decode(w, patch) {
			return
		}
		active, err = scimPatchActive(patch, request.service.settings.PatchMaxOperationCount)
		if err != nil {
			writeScimError(w, http.StatusBadRequest, scimTypeInvalidSyntax, "The PATCH is not one this server answers.")
			return
		}
	default:
		writeScimError(w, http.StatusMethodNotAllowed, "", "Not allowed.")
		return
	}
	if r.Method == http.MethodDelete {
		deactivate := false
		active = &deactivate
	}
	request.actOnUser(w, id, active)
}

// Reads a User, or sets its active: false denies a member and queues its MemberRevoke, or withdraws a queued create;
// true changes nothing, and is refused for a membership that ended, which only a new invite makes again.
func (self *scimRequest) actOnUser(w http.ResponseWriter, id server.Id, active *bool) {
	ctx := self.clientSession.Ctx
	var user *scimUser
	found := true
	ended := false
	server.Tx(ctx, func(tx server.PgTx) {
		user, found, ended = nil, true, false
		target := self.userTargetInTx(ctx, tx, id)
		if target == nil {
			found = false
			return
		}
		switch {
		case active == nil || *active && target.deactivated == nil:
			user = self.targetUser(target)
		case *active:
			ended = true
		case target.member != nil:
			change, err := self.deactivateInTx(ctx, tx, target.member)
			if errors.Is(err, model.ErrNetworkMemberNotFound) {
				found = false
				return
			}
			server.Raise(err)
			user = self.deactivatedUser(change)
		case target.created != nil:
			self.withdrawInTx(ctx, tx, target.created)
			user = self.createdUser(target.created)
			user.Active = false
		default:
			user = self.targetUser(target)
		}
	})
	switch {
	case !found:
		writeScimError(w, http.StatusNotFound, "", "Not found.")
	case ended:
		writeScimError(w, http.StatusBadRequest, scimTypeInvalidValue, "The membership ended: the person joins again by invite.")
	case self.r.Method == http.MethodDelete:
		writeScim(w, http.StatusNoContent, nil)
	default:
		writeScim(w, http.StatusOK, user)
	}
	if found && bool(glog.V(2)) {
		glog.Infof("[scim][%s]%s user %s\n", self.networkId, self.r.Method, id)
	}
}

// A group member as SCIM names it.
type scimGroupMember struct {
	Value   string `json:"value"`
	Display string `json:"display,omitempty"`
}

// A Group as this server answers it, from its queued changes.
type scimGroup struct {
	Schemas     []string          `json:"schemas"`
	Id          string            `json:"id"`
	DisplayName string            `json:"displayName"`
	Members     []scimGroupMember `json:"members"`
	Meta        scimMeta          `json:"meta"`
}

// A Group as an IdP sends it.
type scimGroupInput struct {
	DisplayName string            `json:"displayName"`
	Members     []scimGroupMember `json:"members"`
}

// The member ids a request names, each a SCIM user id; an error for one that is not, or past the bound.
func (self *scimRequest) groupMemberIds(members []scimGroupMember) ([]server.Id, error) {
	if self.service.settings.GroupMaxMemberCount < len(members) {
		return nil, errors.New("too many members")
	}
	memberIds := []server.Id{}
	for _, member := range members {
		memberId, err := server.ParseId(strings.TrimSpace(member.Value))
		if err != nil {
			return nil, err
		}
		if !slices.Contains(memberIds, memberId) {
			memberIds = append(memberIds, memberId)
		}
	}
	return memberIds, nil
}

// The groups the queue holds, folded from their changes in order, deleted ones left out, by group id.
func (self *scimRequest) groupsInTx(ctx context.Context, tx server.PgTx) (map[server.Id]*scimGroup, []server.Id) {
	groups := map[server.Id]*scimGroup{}
	order := []server.Id{}
	for _, change := range model.GetNetworkScimChangesInTx(ctx, tx, self.networkId, self.minCreateTime(), model.NetworkScimChangeKindGroup) {
		detail := &model.NetworkScimGroupDetail{}
		if json.Unmarshal(change.Detail, detail) != nil {
			continue
		}
		group := groups[detail.GroupId]
		if group == nil && detail.Op != model.NetworkScimGroupOpDelete {
			group = &scimGroup{
				Schemas: []string{schemaGroup},
				Id:      detail.GroupId.String(),
				Members: []scimGroupMember{},
				Meta:    scimMeta{ResourceType: "Group", Location: self.location("Groups", detail.GroupId.String())},
			}
			groups[detail.GroupId] = group
			order = append(order, detail.GroupId)
		}
		if detail.DisplayName != nil && group != nil {
			group.DisplayName = *detail.DisplayName
		}
		switch detail.Op {
		case model.NetworkScimGroupOpCreate, model.NetworkScimGroupOpReplace:
			group.Members = []scimGroupMember{}
			for _, memberId := range detail.MemberIds {
				group.Members = append(group.Members, scimGroupMember{Value: memberId.String()})
			}
		case model.NetworkScimGroupOpPatch:
			for _, memberId := range detail.RemoveMemberIds {
				group.Members = slices.DeleteFunc(group.Members, func(member scimGroupMember) bool { return member.Value == memberId.String() })
			}
			for _, memberId := range detail.AddMemberIds {
				if !slices.ContainsFunc(group.Members, func(member scimGroupMember) bool { return member.Value == memberId.String() }) {
					group.Members = append(group.Members, scimGroupMember{Value: memberId.String()})
				}
			}
		case model.NetworkScimGroupOpDelete:
			delete(groups, detail.GroupId)
			order = slices.DeleteFunc(order, func(groupId server.Id) bool { return groupId == detail.GroupId })
		}
	}
	return groups, order
}

// Queues a group change in tx, with its admin event.
func (self *scimRequest) queueGroupInTx(ctx context.Context, tx server.PgTx, detail *model.NetworkScimGroupDetail) {
	change, err := model.AddNetworkScimChangeInTx(ctx, tx, self.networkId, nil, model.NetworkScimChangeKindGroup, detail, self.service.now())
	server.Raise(err)
	changeId := change.ChangeId
	model.AddAdminEventInTx(ctx, tx, self.adminEvent(model.AdminEventActionScimGroup, model.AdminEventTargetScimChange, &changeId))
}

// /Groups: GET lists the queue's groups, POST queues a new group.
func GroupsHandler(w http.ResponseWriter, r *http.Request) {
	request, ok := authorize(w, r)
	if !ok {
		return
	}
	defer request.clientSession.Cancel()
	ctx := request.clientSession.Ctx
	switch r.Method {
	case http.MethodGet:
		filter, err := parseScimFilter(r.URL.Query().Get("filter"))
		if err != nil || filter != nil && filter.attribute != "displayname" && filter.attribute != "id" {
			writeScimError(w, http.StatusBadRequest, scimTypeInvalidFilter, "The filter is not one this server answers.")
			return
		}
		startIndex, count := request.page()
		matches := []*scimGroup{}
		server.Tx(ctx, func(tx server.PgTx) {
			matches = []*scimGroup{}
			groups, order := request.groupsInTx(ctx, tx)
			for _, groupId := range order {
				group := groups[groupId]
				if filter != nil && (filter.attribute == "displayname" && group.DisplayName != filter.value || filter.attribute == "id" && group.Id != filter.value) {
					continue
				}
				matches = append(matches, group)
			}
		})
		page := []*scimGroup{}
		if startIndex-1 < len(matches) {
			page = matches[startIndex-1 : min(len(matches), startIndex-1+count)]
		}
		writeScimList(w, len(matches), startIndex, page)
	case http.MethodPost:
		input := &scimGroupInput{}
		if !request.decode(w, input) {
			return
		}
		if strings.TrimSpace(input.DisplayName) == "" {
			writeScimError(w, http.StatusBadRequest, scimTypeInvalidValue, "displayName is required.")
			return
		}
		memberIds, err := request.groupMemberIds(input.Members)
		if err != nil {
			writeScimError(w, http.StatusBadRequest, scimTypeInvalidValue, "A member is not a user of this team.")
			return
		}
		groupId := server.NewId()
		displayName := strings.TrimSpace(input.DisplayName)
		var group *scimGroup
		server.Tx(ctx, func(tx server.PgTx) {
			request.queueGroupInTx(ctx, tx, &model.NetworkScimGroupDetail{Op: model.NetworkScimGroupOpCreate, GroupId: groupId, DisplayName: &displayName, MemberIds: memberIds})
			groups, _ := request.groupsInTx(ctx, tx)
			group = groups[groupId]
		})
		writeScim(w, http.StatusCreated, group)
	default:
		writeScimError(w, http.StatusMethodNotAllowed, "", "Not allowed.")
	}
}

// The members a group PATCH adds and removes, the displayName it sets, and the members it replaces, from its
// operations; an error for a malformed one.
func (self *scimRequest) scimGroupPatch(request *scimPatchRequest, groupId server.Id) (*model.NetworkScimGroupDetail, error) {
	if len(request.Operations) == 0 || self.service.settings.PatchMaxOperationCount < len(request.Operations) {
		return nil, errors.New("a PATCH of 1 to the bound's operations")
	}
	detail := &model.NetworkScimGroupDetail{Op: model.NetworkScimGroupOpPatch, GroupId: groupId}
	var replaced []server.Id
	readMembers := func(raw json.RawMessage) ([]server.Id, error) {
		members := []scimGroupMember{}
		if err := json.Unmarshal(raw, &members); err != nil {
			return nil, err
		}
		return self.groupMemberIds(members)
	}
	for _, operation := range request.Operations {
		op := strings.ToLower(operation.Op)
		path := strings.TrimSpace(operation.Path)
		switch {
		case (op == "add" || op == "replace") && strings.EqualFold(path, "members"):
			memberIds, err := readMembers(operation.Value)
			if err != nil {
				return nil, err
			}
			if op == "replace" {
				replaced = memberIds
			} else {
				detail.AddMemberIds = append(detail.AddMemberIds, memberIds...)
			}
		case op == "remove" && strings.EqualFold(path, "members"):
			if len(operation.Value) == 0 {
				replaced = []server.Id{}
				continue
			}
			memberIds, err := readMembers(operation.Value)
			if err != nil {
				return nil, err
			}
			detail.RemoveMemberIds = append(detail.RemoveMemberIds, memberIds...)
		case op == "remove" && scimGroupMemberPathPattern.MatchString(path):
			memberId, err := server.ParseId(scimGroupMemberPathPattern.FindStringSubmatch(path)[1])
			if err != nil {
				return nil, err
			}
			detail.RemoveMemberIds = append(detail.RemoveMemberIds, memberId)
		case op == "replace" && strings.EqualFold(path, "displayName"):
			var displayName string
			if err := json.Unmarshal(operation.Value, &displayName); err != nil || strings.TrimSpace(displayName) == "" {
				return nil, errors.New("displayName is not a name")
			}
			displayName = strings.TrimSpace(displayName)
			detail.DisplayName = &displayName
		case op == "replace" && path == "":
			fields := &scimGroupInput{}
			if err := json.Unmarshal(operation.Value, fields); err != nil {
				return nil, err
			}
			if displayName := strings.TrimSpace(fields.DisplayName); displayName != "" {
				detail.DisplayName = &displayName
			}
		default:
			return nil, errors.New("an operation this server does not answer")
		}
	}
	if replaced != nil {
		// a replace of the members makes the change a replace, with the adds after it
		detail.Op = model.NetworkScimGroupOpReplace
		detail.MemberIds = append(replaced, detail.AddMemberIds...)
		detail.AddMemberIds, detail.RemoveMemberIds = nil, nil
	}
	return detail, nil
}

// /Groups/<id>: GET reads the queue's group; PUT, PATCH and DELETE queue a change for an admin device. A group the
// queue no longer holds may still be the config's, so a change of an unknown group id is queued all the same.
func GroupHandler(w http.ResponseWriter, r *http.Request) {
	request, ok := authorize(w, r)
	if !ok {
		return
	}
	defer request.clientSession.Cancel()
	ctx := request.clientSession.Ctx
	groupId, err := server.ParseId(router.GetPathValues(r)[1])
	if err != nil {
		writeScimError(w, http.StatusNotFound, "", "Not found.")
		return
	}
	var detail *model.NetworkScimGroupDetail
	switch r.Method {
	case http.MethodGet:
	case http.MethodPut:
		input := &scimGroupInput{}
		if !request.decode(w, input) {
			return
		}
		memberIds, err := request.groupMemberIds(input.Members)
		if err != nil || strings.TrimSpace(input.DisplayName) == "" {
			writeScimError(w, http.StatusBadRequest, scimTypeInvalidValue, "The group is not valid.")
			return
		}
		displayName := strings.TrimSpace(input.DisplayName)
		detail = &model.NetworkScimGroupDetail{Op: model.NetworkScimGroupOpReplace, GroupId: groupId, DisplayName: &displayName, MemberIds: memberIds}
	case http.MethodPatch:
		patch := &scimPatchRequest{}
		if !request.decode(w, patch) {
			return
		}
		detail, err = request.scimGroupPatch(patch, groupId)
		if err != nil {
			writeScimError(w, http.StatusBadRequest, scimTypeInvalidSyntax, "The PATCH is not one this server answers.")
			return
		}
	case http.MethodDelete:
		detail = &model.NetworkScimGroupDetail{Op: model.NetworkScimGroupOpDelete, GroupId: groupId}
	default:
		writeScimError(w, http.StatusMethodNotAllowed, "", "Not allowed.")
		return
	}
	var group *scimGroup
	server.Tx(ctx, func(tx server.PgTx) {
		if detail != nil {
			request.queueGroupInTx(ctx, tx, detail)
		}
		groups, _ := request.groupsInTx(ctx, tx)
		group = groups[groupId]
	})
	switch {
	case r.Method == http.MethodDelete, r.Method == http.MethodPatch:
		writeScim(w, http.StatusNoContent, nil)
	case group == nil:
		writeScimError(w, http.StatusNotFound, "", "Not found.")
	default:
		writeScim(w, http.StatusOK, group)
	}
}
