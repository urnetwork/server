//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// The control-plane case's checks of a team's members and tagged sidecars (control_plane_concurrency_test.go's
// enrollment and churn phases), which findings A and B kept out of the case as it landed:
//
//   - a Member's keys: a Member makes auth keys for its own devices alone (§7.5's Member: own devices;
//     team-member-own-auth-key), so its key for a tag autogroup:admin owns is refused 403. What a Member may not do
//     yet, list or revoke its own keys or make keys for a tag it owns, waits on team-key-reach-followups and is not
//     asserted
//   - a tagged agent at api: a team's tagged sidecar is the network's, never its sponsor's (owner decision 83), so its
//     credential names the team's owner of record with no member_id (QD-1194; team-tagged-enroll) and keeps working at
//     api past its sponsor's removal: the state check, its refresh, a window client of its own, and the exchange
//   - a removed member's devices: its MemberRevoke revokes its untagged devices in every live device's state (§7.10),
//     POST /network/member-remove deactivates their clients at api, the exchange refuses them (invalid_grant), and
//     api's first metadata cluster holds every rev/ object the removal owes: the member's, written by the denial, and
//     each device's client's, written as the denial deactivates it, and sidecar's, written by the MemberRevoke's
//     effects at the entry's time
//
// A failure from a product bug stays failing.

package sidecare2e

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/sidecart3"
)

// A Member's auth key for the team's tag, which autogroup:admin owns, is refused 403, and the Member's own untagged
// key, which its devices enrolled from at the enrollment phase, stands beside it.
func (self *controlPlaneCase) checkMemberTaggedKeyRefused(ctx context.Context) {
	t := self.t
	member := self.plain[0]
	tagId, ok := self.team.TagId(self.settings.TeamTag)
	if !ok {
		t.Errorf("the team's document holds no tag %s", self.settings.TeamTag)
		return
	}
	_, err := controlPlaneAuthKey(ctx, self.world, member.Root, 1, []string{controlPlaneTagHex(tagId)}, false)
	var routeErr *controlPlaneRouteError
	switch {
	case err == nil:
		t.Errorf("%s, a Member, made an auth key for %s, a tag autogroup:admin owns (§7.5: a Member makes keys for its own devices)", member.Name, self.settings.TeamTag)
	case !errors.As(err, &routeErr) || routeErr.status != http.StatusForbidden:
		t.Errorf("%s's auth key for %s was refused other than 403: %v", member.Name, self.settings.TeamTag, err)
	}
	if len(member.Devices()) != self.settings.TeamMemberEnrollCount {
		t.Errorf("%s holds %d devices of its own, enrolled from its own keys, not %d", member.Name, len(member.Devices()), self.settings.TeamMemberEnrollCount)
	}
}

// A team's tagged agent at api on its own credential, as the file's header says, when: its token, as api minted it at
// its enrollment, names the team's owner of record and its own client, no member, strict_exp; the state check passes it
// at a sidecar route (GET /sidecar/identity/<its sidecar> on the Sidecar's own api); GET /auth/refresh with its proof
// answers a token naming the same and passing the state check; POST /network/auth-client mints it a window client, a
// child of its client, admin_refused and strict_exp, whose token names the owner of record with no member and passes
// the state check; and the exchange grants it what §7.8 gives a tagged sidecar at evs and gkm, through api's exchange,
// and the config's read at the exchange's projection. Its admission stands in every live device of the team's state,
// carrying the team's tag and no member, and api holds its client active.
func (self *controlPlaneCase) checkTaggedAgent(ctx context.Context, device *sidecart3.T3Device, when string) {
	t := self.t
	world := self.world
	identity := device.Sidecar().Identity()
	clientId := server.Id(identity.ClientId)
	claimsOf := func(what string, token string, wantClientId server.Id) {
		claims, err := controlPlaneTokenClaims(token)
		switch {
		case err != nil:
			t.Errorf("%s's %s %s: %v", device.Name, what, when, err)
		case claims.UserId != self.ownerOfRecord || claims.MemberId != nil || claims.ClientId == nil || *claims.ClientId != wantClientId || !claims.StrictExp:
			t.Errorf("%s's %s %s names user %s, member %v and client %v, strict_exp %t, not the team's owner of record %s, no member and client %s, strict_exp", device.Name, what, when, claims.UserId, claims.MemberId, claims.ClientId, claims.StrictExp, self.ownerOfRecord, wantClientId)
		}
	}
	stateCheck := func(what string, bearer string) {
		status, answer, err := world.T3().Api().Call(ctx, http.MethodGet, "/sidecar/identity/"+identity.SidecarId.String(), bearer, nil)
		if err != nil || status != http.StatusOK {
			t.Errorf("%s's %s %s was refused at GET /sidecar/identity: %d %s (%v)", device.Name, what, when, status, strings.TrimSpace(string(answer)), err)
		}
	}
	api, err := controlPlaneDeviceApi(ctx, device)
	if err != nil {
		t.Errorf("%s's api %s: %v", device.Name, when, err)
		return
	}

	// its token and the state check
	claimsOf("token", strings.TrimPrefix(device.Bearer(), "Bearer "), clientId)
	if _, err := api.GetIdentity(ctx, identity.SidecarId, 0); err != nil {
		t.Errorf("%s's state check %s, GET /sidecar/identity on its own token: %v", device.Name, when, err)
	}
	// its refresh
	if renewed, err := api.Refresh(ctx); err != nil {
		t.Errorf("%s's refresh %s: %v", device.Name, when, err)
	} else {
		claimsOf("refreshed token", renewed, clientId)
		stateCheck("refreshed token", "Bearer "+renewed)
	}
	// a window client of its own
	sourceClientId := identity.ClientId
	window, err := api.AuthClient(ctx, &sidecar.SidecarAuthClientArgs{
		SourceClientId:   &sourceClientId,
		Description:      "control-plane window",
		DeviceSpec:       "control-plane",
		DeclaresNoEgress: true,
	})
	switch {
	case err != nil:
		t.Errorf("%s's window client %s: %v", device.Name, when, err)
	case !window.AdminRefused || !window.StrictExp:
		t.Errorf("%s's window client %s is admin_refused %t and strict_exp %t, not both", device.Name, when, window.AdminRefused, window.StrictExp)
	default:
		claimsOf("window client's token", window.ByClientJwt, server.Id(window.ClientId))
		stateCheck("window client's token", "Bearer "+window.ByClientJwt)
	}
	// the exchange, at api's and at its projection
	configRepoId, directoryRepoId, err := controlPlaneNetworkRepos(self.team.Owner.Devices()[0])
	if err != nil {
		t.Errorf("the team's repos: %v", err)
		return
	}
	self.checkDeviceScopes(ctx, device, configRepoId, directoryRepoId, when)
	if grant, err := self.exchange.grant(ctx, device, WorldBlockEvs, []string{controlPlaneRepoScope(configRepoId, "r")}); err != nil {
		t.Errorf("%s %s was refused the config's read at the exchange's projection: %v", device.Name, when, err)
	} else if len(grant.Scopes) != 1 {
		t.Errorf("%s %s was granted %v of the config's read at the exchange's projection", device.Name, when, grant.Scopes)
	}

	// its admission, in every live device's state, and its client at api
	tagId, _ := self.team.TagId(self.settings.TeamTag)
	for _, other := range self.team.LiveDevices() {
		row := other.Follower().VerifiedState().SidecarRow(identity.SidecarId)
		if row == nil {
			t.Errorf("%s's state %s never admitted the tagged %s", other.Name, when, device.Name)
			continue
		}
		if row.GetRevokedAt() != 0 {
			t.Errorf("%s's state %s holds the tagged %s revoked at %d", other.Name, when, device.Name, row.GetRevokedAt())
		}
		heldTagId := sidecar.Id{}
		if len(row.GetTagIds()) == 1 {
			heldTagId, _ = sidecar.IdFromBytes(row.GetTagIds()[0])
		}
		if len(row.GetMemberId()) != 0 || len(row.GetTagIds()) != 1 || heldTagId != tagId {
			t.Errorf("%s's state %s admits the tagged %s with member %x and tags %x, not %s alone", other.Name, when, device.Name, row.GetMemberId(), row.GetTagIds(), self.settings.TeamTag)
		}
	}
	if active, err := worldNetworkActiveClientIds(ctx, self.team.NetworkId); err != nil {
		t.Errorf("the team's clients: %v", err)
	} else if !active[clientId] {
		t.Errorf("api holds the tagged %s's client %s inactive %s", device.Name, clientId, when)
	}
	windowClientId := sidecar.Id{}
	if window != nil {
		windowClientId = window.ClientId
	}
	t.Logf("the tagged %s %s, on the owner of record %s: state check, refresh, window client %s and exchange checked", device.Name, when, self.ownerOfRecord, windowClientId)
}

// member's removal took devices with it: every live device of the team's state holds each one revoked (§7.10: its
// MemberRevoke revokes its untagged devices), api holds none of their clients active (POST /network/member-remove
// deactivates them), the exchange refuses each whole (invalid_grant), and api holds every rev/ object the removal owes
// (awaitRemovalRevocations).
func (self *controlPlaneCase) checkMemberDevicesRemoved(ctx context.Context, member *WorldTeamMember, devices []*sidecart3.T3Device) {
	t := self.t
	self.awaitRemovalRevocations(ctx, member, devices)
	active, err := worldNetworkActiveClientIds(ctx, self.team.NetworkId)
	if err != nil {
		t.Errorf("the team's clients: %v", err)
	}
	for _, device := range devices {
		sidecarId := device.Sidecar().Identity().SidecarId
		for _, other := range self.team.LiveDevices() {
			row := other.Follower().VerifiedState().SidecarRow(sidecarId)
			if row == nil || row.GetRevokedAt() == 0 {
				t.Errorf("%s's state holds %s, %s's device, unrevoked after %s's removal", other.Name, device.Name, member.Name, member.Name)
			}
		}
		if clientId := server.Id(device.Sidecar().Identity().ClientId); active[clientId] {
			t.Errorf("api holds %s's client %s active after %s's removal", device.Name, clientId, member.Name)
		}
		self.checkExchangeRefused(ctx, device, "its member removed")
	}
	t.Logf("%s's removal: its %d devices revoked, deactivated at api and refused at the exchange", member.Name, len(devices))
}

// Waits until api's first metadata cluster holds every rev/ object member's removal owes (§7.10's revocation objects):
// the member's, which api's denial writes and its MemberRevoke's effects raise, and each of devices' client's, which
// the denial writes as it deactivates the client, and sidecar's, which the MemberRevoke's effects write at the entry's
// time; within RevocationSettleTimeout, asking every RevocationSettleInterval, since the effects run as a task with no
// hook a test reads. How long they took to stand is logged; the objects still missing at the bound are an error.
func (self *controlPlaneCase) awaitRemovalRevocations(ctx context.Context, member *WorldTeamMember, devices []*sidecart3.T3Device) {
	t := self.t
	cluster, err := self.world.T3().Host().Blobs.FirstCluster()
	if err != nil {
		t.Errorf("the first metadata cluster: %v", err)
		return
	}
	networkId := sidecar.Id(self.team.NetworkId)
	owed := map[string]string{sidecar.RevocationKey(networkId, sidecar.RevocationKindMember, member.MemberId): member.Name + "'s member"}
	for _, device := range devices {
		identity := device.Sidecar().Identity()
		owed[sidecar.RevocationKey(networkId, sidecar.RevocationKindClient, identity.ClientId)] = device.Name + "'s client"
		owed[sidecar.RevocationKey(networkId, sidecar.RevocationKindSidecar, identity.SidecarId)] = device.Name + "'s sidecar"
	}
	startedAt := time.Now()
	ctx, cancel := context.WithTimeout(ctx, self.settings.RevocationSettleTimeout)
	defer cancel()
	for {
		missing := []string{}
		for key, what := range owed {
			if _, err := cluster.Head(ctx, key); errors.Is(err, sidecar.ErrBlobNotFound) {
				missing = append(missing, what)
			} else if err != nil && ctx.Err() == nil {
				t.Errorf("%s's removal: the rev/ object of %s: %v", member.Name, what, err)
				return
			}
		}
		if len(missing) == 0 {
			t.Logf("%s's removal: its %d rev/ objects stand, %s after its check began", member.Name, len(owed), time.Since(startedAt).Round(time.Millisecond))
			return
		}
		select {
		case <-ctx.Done():
			slices.Sort(missing)
			t.Errorf("%s's removal: api holds no rev/ object of %s past %s (§7.10)", member.Name, strings.Join(missing, ", "), self.settings.RevocationSettleTimeout)
			return
		case <-time.After(self.settings.RevocationSettleInterval):
		}
	}
}
