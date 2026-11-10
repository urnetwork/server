package api

// §7.5's roles at the routes (TEAM-M6-03; D-TEAM-3).

import (
	"strings"
	"testing"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

// Every Network and AppAdmin route names its permission, and every entry names
// such a route: a new admin route fails here until it is mapped, and meanwhile
// asks the Owner's permission.
func TestEveryAdminRouteHasAPermission(t *testing.T) {
	served := map[string]bool{}
	for _, route := range Routes() {
		key := routeAccessKey(route)
		served[key] = true
		access, ok := routeAccessByRoute[key]
		if !ok || access != routeAccessNetwork && access != routeAccessAppAdmin {
			continue
		}
		if _, mapped := routePermissionByRoute[key]; !mapped {
			t.Errorf("%s administers the network and names no permission", key)
		}
	}
	for key, permission := range routePermissionByRoute {
		if !served[key] {
			t.Errorf("%s names permission %s and is no route", key, permission)
			continue
		}
		if access := routeAccessByRoute[key]; access != routeAccessNetwork && access != routeAccessAppAdmin {
			t.Errorf("%s names permission %s and is not an admin route (access %d)", key, permission, access)
		}
	}
	if routePermissionFor("POST /network/no-such-route") != model.RolePermissionOwner {
		t.Fatal("an unmapped route asks less than the Owner's permission")
	}
}

// Every admin route carries the role gate, and no other route does.
func TestEveryAdminRouteHasTheRoleGate(t *testing.T) {
	for _, route := range applyRouteAccess(Routes()) {
		access := routeAccessFor(route)
		admin := access == routeAccessNetwork || access == routeAccessAppAdmin
		if route.RequiresRolePermission() != admin {
			t.Errorf("%s (access %d): role gate %t", routeAccessKey(route), access, route.RequiresRolePermission())
		}
	}
}

// Each role against each permission, as §7.5's table gives them; no role holds
// the owner of record's account.
func TestEachRoleAgainstItsPermissions(t *testing.T) {
	want := map[model.NetworkMemberRole][]model.RolePermission{
		model.NetworkMemberRoleOwner: {
			model.RolePermissionOwner, model.RolePermissionMembers, model.RolePermissionAdmin, model.RolePermissionDevices,
			model.RolePermissionPolicy, model.RolePermissionKeys, model.RolePermissionBilling, model.RolePermissionRead, model.RolePermissionSelf,
		},
		model.NetworkMemberRoleAdmin: {
			model.RolePermissionMembers, model.RolePermissionAdmin, model.RolePermissionDevices, model.RolePermissionPolicy,
			model.RolePermissionKeys, model.RolePermissionRead, model.RolePermissionSelf,
		},
		model.NetworkMemberRoleNetworkAdmin: {model.RolePermissionPolicy, model.RolePermissionRead, model.RolePermissionSelf},
		model.NetworkMemberRoleItAdmin:      {model.RolePermissionDevices, model.RolePermissionRead, model.RolePermissionSelf},
		model.NetworkMemberRoleBillingAdmin: {model.RolePermissionBilling, model.RolePermissionRead, model.RolePermissionSelf},
		model.NetworkMemberRoleAuditor:      {model.RolePermissionRead, model.RolePermissionSelf},
		model.NetworkMemberRoleMember:       {model.RolePermissionSelf},
	}
	for role, permissions := range want {
		held := map[model.RolePermission]bool{}
		for _, permission := range permissions {
			held[permission] = true
		}
		for _, permission := range model.RolePermissions {
			if role.Permits(permission) != held[permission] {
				t.Errorf("%s permits %s: %t, want %t", role, permission, role.Permits(permission), held[permission])
			}
		}
		if role.Permits(model.RolePermissionAccount) {
			t.Errorf("%s holds the owner of record's account", role)
		}
	}
	if model.NetworkMemberRole("superuser").Permits(model.RolePermissionRead) {
		t.Fatal("a role this build does not know permits something")
	}
}

// An unmapped route asks the Owner's permission, which an Admin lacks.
func TestUnmappedRouteRefusedForAnAdmin(t *testing.T) {
	permission := routePermissionFor("POST /network/a-route-nobody-mapped")
	if model.NetworkMemberRoleAdmin.Permits(permission) {
		t.Fatalf("an Admin reaches an unmapped route (%s)", permission)
	}
	if !model.NetworkMemberRoleOwner.Permits(permission) {
		t.Fatalf("an Owner cannot reach an unmapped route (%s)", permission)
	}
}

// A scope never reaches past its creator's role: for every role, scope set and
// permission, the scoped credential permits only what the role does.
func TestScopeNeverExceedsTheCreatorsRole(t *testing.T) {
	roles := []model.NetworkMemberRole{
		model.NetworkMemberRoleOwner, model.NetworkMemberRoleAdmin, model.NetworkMemberRoleNetworkAdmin,
		model.NetworkMemberRoleItAdmin, model.NetworkMemberRoleBillingAdmin, model.NetworkMemberRoleAuditor, model.NetworkMemberRoleMember,
	}
	scopeSets := [][]string{
		nil,
		{},
		{model.ApiKeyScopeAll},
		{model.ApiKeyScopeDevicesCore},
		{model.ApiKeyScopeAuthKeys},
		{model.ApiKeyScopePolicyRead},
		{model.ApiKeyScopeLogsRead},
		{model.ApiKeyScopeAuthKeys, model.ApiKeyScopePolicyRead},
		{"not-a-scope"},
	}
	for _, role := range roles {
		for _, scopes := range scopeSets {
			for _, permission := range model.RolePermissions {
				if model.RoleAndScopesPermit(role, scopes, permission) && !role.Permits(permission) {
					t.Errorf("%s with scopes %v reaches %s, past the role", role, scopes, permission)
				}
			}
		}
	}
	// a narrow scope cuts the role
	if model.RoleAndScopesPermit(model.NetworkMemberRoleOwner, []string{model.ApiKeyScopePolicyRead}, model.RolePermissionKeys) {
		t.Fatal("policy:read reaches the keys")
	}
	if model.ScopesPermit([]string{}, model.RolePermissionRead) {
		t.Fatal("an empty scope list reaches something")
	}
	if model.ScopesPermit([]string{"not-a-scope"}, model.RolePermissionSelf) {
		t.Fatal("an unknown scope reaches something")
	}
}

// The spec lists the role gate's refusal, with the gate's own message.
func TestSpecListsRolePermissionRefused(t *testing.T) {
	spec := loadSpec(t)
	responses := asMap(asMap(spec.root["components"])["responses"])
	example, _ := asMap(asMap(asMap(responses["RolePermissionRefused"])["content"])["text/plain"])["example"].(string)
	if example != router.RolePermissionRefusedMessage {
		t.Fatalf("RolePermissionRefused example = %q, want the gate's message %q", example, router.RolePermissionRefusedMessage)
	}
	description, _ := asMap(spec.root["info"])["description"].(string)
	if !strings.Contains(description, "RolePermissionRefused") {
		t.Fatal("the spec's description does not say when RolePermissionRefused answers")
	}
}
