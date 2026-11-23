//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// The world's team networks (§7.5, §7.9; the control-plane case's, e2e-control-plane-concurrency, and reusable by later
// cases such as e2e-churn-load): a network with several members, each a person with a personal network of their own,
// their own devices, and tagged sidecars beside them, where every world network before was one owner's personal one.
//
//	team      POST /network/team-create through api from its first Owner's personal network, which makes the team's
//	          network, its owner of record and the first Owner's membership; then the team tier, by a team grant of
//	          QUOTA-M6-01's as invoice.paid writes it (worldGrantTeamTier), since the world runs no Stripe checkout;
//	          then the first Owner's devices, enrolled under the Owner's team credential (its member_id in it), the
//	          first bootstrapping the team as a Team's first owner does (its draft names the Owner's member and runs
//	          the team's first ACL files), the rest admitted by it
//	members   each joins as §7.5 says: an Owner's or Admin's POST /network/member-invite answers the bearer, the
//	          inviting device appends the link's fragment (the genesis, its pin, its own id and key, a join secret),
//	          the person accepts with their own sign-in (POST /network/member-invite-accept from their personal
//	          network's credential), which makes their membership; an admin device signs MemberAdmit at the role
//	          the invite names; the member's first device enrolls under the member's team credential and is admitted
//	          by the inviting device once its join's MAC verifies under the fragment's secret, with
//	          SIDECAR_CAPABILITY_INVITED (§8.5's invited clause); the member's later devices are admitted by that
//	          consented first device (§7.9's scoped row)
//	tags      the team's ACL document names each tag, owned by autogroup:admin; a tagged sidecar enrolls from an auth
//	          key carrying the tag's id, made under an Owner's or Admin's credential, its sponsor, whose live
//	          membership the enrollment runs on while the client it mints is the team's, its token naming the team's
//	          owner of record with no member_id (QD-1194), so it outlives its sponsor's membership; and an Owner's
//	          device signs its SidecarAdmit with that tag id
//	removal   POST /network/member-remove denies a member at api at once (its row, its untagged clients, its rev/
//	          object), and an admin device signs its MemberRevoke (§7.10); a device revoked, or whose member leaves, is
//	          retired from the network's syncs first, before api's denial, and a sync already waiting on it stops
//
// What a person's credential is, the world signs with session's fixture signer, as its owners' credentials are
// (world_owner_sign_test.go): a network credential naming the person's user, and in the team their member_id, as POST
// /auth/network-switch mints one. Every read and write of server state the team needs outside api's routes is in
// world_team_state_test.go. Safe for concurrent use once made.

package sidecare2e

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/sidecart3"
)

// What a team network is made with (QD-702).
type WorldTeamSettings struct {
	// What the team's sidecars' names start with, and its network name's label.
	Name string
	// The first Owner's devices, the first bootstrapping the team: at least one.
	OwnerDeviceCount int
	// The tags the team's ACL document names, each owned by autogroup:admin, without their tag: prefix.
	Tags []string
	// An invite's expire_days (§7.5: 1 to 30).
	InviteExpireDays int
	// How long the team grant's period runs from now (QUOTA-M6-01's balances run a period plus its grace).
	GrantPeriod time.Duration
	// The team's first ACL document beside its tags: its grants, as worldAclPolicy writes them. None leaves the
	// grants out, so §8.5's implicit rules hold for the rest.
	Grants []string
	// pro.yml's team: sidecar: block for the world's life, which AddTeamNetwork installs (§20.4's Team column by
	// default); nil leaves the process's, where every Team limit reads zero and refuses every creation.
	SidecarLimits map[string]int64
	// The data a team grant writes, pro.yml's Pro data, for the world's life where the process's pro.yml sets none:
	// §20.9's 10 TiB.
	GrantDataByteCount int64
	// How long a role the log set may take to reach network_member through the effects task, and how often it is
	// read meanwhile: the task worker polls its queue, and the effects run after it claims the task.
	RoleMirrorTimeout      time.Duration
	RoleMirrorPollInterval time.Duration
	// How long a tagged admission lives, its not_after from its filing: 30 days, inside §7.8's 90.
	TaggedAdmissionLife time.Duration
}

func DefaultWorldTeamSettings() *WorldTeamSettings {
	return &WorldTeamSettings{
		Name:                   "team",
		OwnerDeviceCount:       2,
		Tags:                   []string{"ci"},
		InviteExpireDays:       7,
		GrantPeriod:            30 * 24 * time.Hour,
		Grants:                 []string{},
		SidecarLimits:          DefaultWorldTeamSidecarLimits(),
		GrantDataByteCount:     10 * 1024 * 1024 * 1024 * 1024,
		RoleMirrorTimeout:      60 * time.Second,
		RoleMirrorPollInterval: 100 * time.Millisecond,
		TaggedAdmissionLife:    30 * 24 * time.Hour,
	}
}

// §20.4's Team column, pro.yml's team: sidecar: keys: what a team network runs under, which the test stack's pro.yml
// does not carry. Every key the Community and Pro columns set.
func DefaultWorldTeamSidecarLimits() map[string]int64 {
	kib := int64(1024)
	mib := 1024 * kib
	gib := 1024 * mib
	return map[string]int64{
		"storage":                     200 * gib,
		"sidecar_clients":             2000,
		"top_level_client_limit":      100,
		"dns_entries":                 2000,
		"tls_endpoints":               200,
		"config_admissions":           5000,
		"config_statements_per_min":   60,
		"config_statements_burst":     600,
		"config_statements_per_day":   20000,
		"config_bases_per_min":        1,
		"config_bases_per_day":        200,
		"store_weight":                16,
		"evs_drops_per_day":           1000,
		"acl_rules":                   5000,
		"acl_groups":                  1,
		"acl_requests":                1,
		"acl_approvers_two":           1,
		"acl_network_shares":          1,
		"smtp_connections_per_day":    20000,
		"smtp_bytes_per_day":          20 * gib,
		"smtp_recipients_per_message": 100,
		"smtp_recipients_per_day":     100000,
		"smtp_message_bytes":          35 * mib,
		"smtp_burst":                  2000,
		"smtp_burst_window":           int64(time.Hour / time.Millisecond),
		"smtp_concurrent_connections": 32,
		"mail_inbound_message_bytes":  50 * mib,
		"mail_burners_active":         -1,
		"mail_burner_creates_per_day": -1,
		"tls_endpoint_concurrent":     8192,
		"tls_endpoint_new_per_s":      1000,
		"tls_endpoint_mbit_per_s":     1000,
	}
}

// A person the world made: their user, their personal network, whose verified email accepts an invite to it, and
// that network's credential.
type WorldPerson struct {
	Name              string
	UserId            server.Id
	PersonalNetworkId server.Id
	Email             string
	// "Bearer <jwt>" for the person's personal network, as a signed-in person holds it
	PersonalRoot string
}

// One member of a team: a person, their membership and its credential, and their devices.
type WorldTeamMember struct {
	*WorldPerson
	// network_member.member_id, new per join (§7.5)
	MemberId sidecar.Id
	// The role the invite named, which MemberAdmit or the AdminSet signs.
	Role bridge.MemberRole
	// "Bearer <jwt>" for the team, naming the member, as POST /auth/network-switch mints one
	Root string

	stateLock sync.Mutex
	devices   []*sidecart3.T3Device
	removed   bool
}

// The member's devices, in enrollment order.
func (self *WorldTeamMember) Devices() []*sidecart3.T3Device {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.devices)
}

// Whether the member was removed (RemoveMember).
func (self *WorldTeamMember) Removed() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.removed
}

// An invite as its inviting device holds it: the bearer api answered and the fragment the device appended, with its
// join secret (sidecar's config_invite.go).
type WorldTeamInvite struct {
	Email    string
	Role     bridge.MemberRole
	InviteId sidecar.Id
	// ur-invite-<invite_id>-<secret>#<fragment>
	Link string
	// The device that made the fragment, which admits the invitee's first device.
	Inviter *sidecart3.T3Device

	invite *sidecar.MemberInvite
}

// A team network of the world (the file's header).
type WorldTeam struct {
	*WorldNetwork
	world    *World
	settings *WorldTeamSettings
	// The first Owner, whose devices are the network's first.
	Owner *WorldTeamMember
	// the team's network name, which its credentials carry
	networkName string

	stateLock sync.Mutex
	members   []*WorldTeamMember
	// each tag's atom id, by its name without the prefix, from the team's saved ACL document
	tagIds map[string]sidecar.Id
	tagged []*sidecart3.T3Device
	// the team directory (fileDirectory)
	directoryId sidecar.Id
}

// The team's members, its first Owner first, removed ones included, in join order.
func (self *WorldTeam) Members() []*WorldTeamMember {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.members)
}

// The tagged sidecars admitted so far.
func (self *WorldTeam) Tagged() []*sidecart3.T3Device {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.tagged)
}

// The tag's atom id, as the team's ACL document drew it.
func (self *WorldTeam) TagId(tag string) (sidecar.Id, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	id, ok := self.tagIds[tag]
	return id, ok
}

// The team's ACL document: its tags owned by autogroup:admin, and settings' grants.
func (self *WorldTeam) AclPolicy(grants ...string) string {
	owners := []string{}
	for _, tag := range self.settings.Tags {
		owners = append(owners, fmt.Sprintf(`"tag:%s": ["autogroup:admin"]`, tag))
	}
	return `{"tagOwners": {` + strings.Join(owners, ", ") + `}, "grants": [` + strings.Join(grants, ", ") + `]}`
}

// A person with a personal network of their own, made as a sign-up makes one (worldNewPerson), and its credential.
func (self *World) NewPerson(ctx context.Context, name string) (*WorldPerson, error) {
	return worldNewPerson(ctx, name)
}

// A team network (the file's header): made through api by a new person, its first Owner, put on the team tier, with
// settings' OwnerDeviceCount devices of the Owner's, the first bootstrapping it and admitting the rest, and its ACL
// document saved with settings' tags, whose ids the team then holds. settings nil takes the defaults.
func (self *World) AddTeamNetwork(ctx context.Context, settings *WorldTeamSettings) (*WorldTeam, error) {
	if settings == nil {
		settings = DefaultWorldTeamSettings()
	}
	if settings.OwnerDeviceCount < 1 {
		return nil, fmt.Errorf("a team of %d Owner devices has no bootstrapping device", settings.OwnerDeviceCount)
	}
	{
		popLimits := func() {}
		if settings.SidecarLimits != nil {
			popLimits = worldInstallTeamLimits(settings.SidecarLimits)
		}
		popData := worldInstallTeamData(settings.GrantDataByteCount)
		stopEffects := worldRunConfigLogEffects(self.t3.Context())
		pop := func() {
			stopEffects()
			popData()
			popLimits()
		}
		installed := func() bool {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if self.closed {
				return false
			}
			self.closes = append(self.closes, pop)
			return true
		}()
		if !installed {
			pop()
			return nil, errWorldClosed
		}
	}
	ownerPerson, err := worldNewPerson(ctx, settings.Name+"-owner")
	if err != nil {
		return nil, err
	}
	created := &model.CreateNetworkTeamResult{}
	status, answer, err := self.t3.Api().Call(ctx, http.MethodPost, "/network/team-create", ownerPerson.PersonalRoot, &model.CreateNetworkTeamArgs{
		NetworkName: worldTeamNetworkName(settings.Name),
	})
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK || json.Unmarshal(answer, created) != nil || created.Error != nil || created.NetworkId == nil || created.MemberId == nil {
		return nil, fmt.Errorf("POST /network/team-create: %d %s", status, strings.TrimSpace(string(answer)))
	}
	teamNetworkId := *created.NetworkId
	if err := worldGrantTeamTier(ctx, teamNetworkId, settings.GrantPeriod); err != nil {
		return nil, fmt.Errorf("the team tier: %w", err)
	}
	owner := &WorldTeamMember{
		WorldPerson: ownerPerson,
		MemberId:    sidecar.Id(*created.MemberId),
		Role:        bridge.MemberRole_MEMBER_ROLE_OWNER,
		Root:        worldTeamCredential(teamNetworkId, created.NetworkName, ownerPerson.UserId, *created.MemberId),
	}
	network, err := self.t3.AddNetworkOf(ctx, &sidecart3.T3NetworkOf{
		Name:          settings.Name,
		NetworkId:     teamNetworkId,
		OwnerUserId:   ownerPerson.UserId,
		OwnerMemberId: owner.MemberId,
		Root:          owner.Root,
	})
	if err != nil {
		return nil, err
	}
	clients, err := network.AddSidecars(ctx, settings.OwnerDeviceCount, nil)
	if err != nil {
		return nil, fmt.Errorf("%s's Owner's devices: %w", settings.Name, err)
	}
	if err := network.Bootstrap(ctx, clients[0]); err != nil {
		return nil, err
	}
	if 1 < len(clients) {
		if err := network.Admit(ctx, clients[1:]); err != nil {
			return nil, err
		}
	}
	owner.devices = slices.Clone(clients)
	team := &WorldTeam{
		WorldNetwork: &WorldNetwork{T3Network: network, Clients: clients},
		world:        self,
		settings:     settings,
		Owner:        owner,
		networkName:  created.NetworkName,
		members:      []*WorldTeamMember{owner},
		tagIds:       map[string]sidecar.Id{},
	}
	kept := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return false
		}
		self.networks = append(self.networks, team.WorldNetwork)
		return true
	}()
	if !kept {
		return nil, errWorldClosed
	}
	if 0 < len(settings.Tags) || 0 < len(settings.Grants) {
		if _, err := team.SaveAclPolicy(ctx, clients[0], team.AclPolicy(settings.Grants...)); err != nil {
			return nil, fmt.Errorf("%s's first ACL document: %w", settings.Name, err)
		}
	}
	if err := team.fileDirectory(ctx, clients[0]); err != nil {
		return nil, err
	}
	return team, nil
}

// The bootstrapping Owner's device files the team directory's RepoCreate past base 1 and creates the repo, as ur
// daemon's team start does (§7.5, §16.11; sidecar.FileTeamDirectory), synced at the log's tip.
func (self *WorldTeam) fileDirectory(ctx context.Context, device *sidecart3.T3Device) error {
	client := device.Follower().Client()
	evs, err := device.EvsClient()
	if err != nil {
		return err
	}
	result, err := sidecar.FileTeamDirectory(ctx, &sidecar.TeamDirectoryArgs{
		SidecarId: device.Sidecar().Identity().SidecarId,
		State:     client.State,
		CatchUp:   client.CatchUp,
		FileStatement: func(ctx context.Context, signedType bridge.SignedType, body proto.Message) error {
			_, err := self.FileStatement(ctx, device, signedType, body)
			return err
		},
		CreateRepo: evs.CreateRepo,
	})
	if err != nil {
		return fmt.Errorf("%s's team directory: %w", self.Name, err)
	}
	self.stateLock.Lock()
	self.directoryId = result.RepoId
	self.stateLock.Unlock()
	if err := client.CatchUp(ctx); err != nil {
		return err
	}
	return self.Sync(ctx, client.Pin().GetVersion())
}

// The team directory's repo, which the bootstrapping Owner's device filed.
func (self *WorldTeam) DirectoryId() sidecar.Id {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.directoryId
}

// A team network's name: the label and 8 random hex characters, so worlds never share one.
func worldTeamNetworkName(label string) string {
	return fmt.Sprintf("e2e%s%s", strings.ToLower(label), strings.ReplaceAll(server.NewId().String(), "-", "")[24:32])
}

// device saves source as the team's ACL document (World.SaveAclPolicy), then reads the tag ids the saved head holds.
func (self *WorldTeam) SaveAclPolicy(ctx context.Context, device *sidecart3.T3Device, source string) (*bridge.AuthPin, error) {
	pin, err := self.world.SaveAclPolicy(ctx, self.WorldNetwork, device, source)
	if err != nil {
		return nil, err
	}
	// the saved head is the next plan's base, its tags table every tag's id, kept for the name's life (§7.9's Writes)
	plan, err := device.Sidecar().PlanAclPolicy(ctx, []byte(source))
	if err != nil {
		return nil, fmt.Errorf("the saved head's plan: %w", err)
	}
	table := &protocol.ConfigAtomTable{}
	if tags := plan.BaseHead.File(sidecar.ConfigHeadTagsPath); len(tags) != 0 {
		if err := proto.Unmarshal(tags, table); err != nil {
			return nil, fmt.Errorf("the saved head's tags: %w", err)
		}
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, atom := range table.GetAtoms() {
		id, err := sidecar.IdFromBytes(atom.GetAtomId())
		if err != nil {
			return nil, fmt.Errorf("tag %s's id: %w", atom.GetName(), err)
		}
		self.tagIds[atom.GetName()] = id
	}
	return pin, nil
}

// inviter, an Owner's or Admin's member, invites email as role through POST /network/member-invite on its team
// credential, and its device appends the link's fragment at that device's verified pin (§7.5).
func (self *WorldTeam) Invite(ctx context.Context, inviter *WorldTeamMember, device *sidecart3.T3Device, email string, role bridge.MemberRole) (*WorldTeamInvite, error) {
	serverRole, ok := worldTeamServerRole(role)
	if !ok {
		return nil, fmt.Errorf("%s is no role an invite names", role)
	}
	status, answer, err := self.world.t3.Api().Call(ctx, http.MethodPost, "/network/member-invite", inviter.Root, &model.CreateNetworkMemberInviteArgs{
		Email:      email,
		Role:       serverRole,
		ExpireDays: self.settings.InviteExpireDays,
	})
	if err != nil {
		return nil, err
	}
	result := &model.CreateNetworkMemberInviteResult{}
	if status != http.StatusOK || json.Unmarshal(answer, result) != nil || result.Error != nil || result.InviteBearer == "" {
		message := ""
		if result.Error != nil {
			message = result.Error.Message
		}
		return nil, fmt.Errorf("POST /network/member-invite: %d %s", status, message)
	}
	follower := device.Follower()
	if follower == nil || follower.Client() == nil {
		return nil, fmt.Errorf("%s invites by a follower that stands", device.Name)
	}
	anchor, err := device.Sidecar().Anchor()
	if err != nil {
		return nil, fmt.Errorf("%s's anchor: %w", device.Name, err)
	}
	identity := device.Sidecar().Identity()
	fragment, err := sidecar.NewMemberInviteFragment(anchor.GenesisSha256, follower.Client().Pin(), identity.SidecarId, identity.IdentityPublicKey)
	if err != nil {
		return nil, err
	}
	link, err := sidecar.FormatMemberInvite(result.InviteBearer, fragment)
	if err != nil {
		return nil, err
	}
	invite, err := sidecar.ParseMemberInvite(link, nil)
	if err != nil {
		return nil, err
	}
	return &WorldTeamInvite{
		Email:    email,
		Role:     role,
		InviteId: invite.InviteId(),
		Link:     link,
		Inviter:  device,
		invite:   invite,
	}, nil
}

// person accepts invite with their own sign-in, POST /network/member-invite-accept from their personal network's
// credential, the bearer alone (never the fragment): their membership, whose team credential the world signs.
func (self *WorldTeam) Accept(ctx context.Context, person *WorldPerson, invite *WorldTeamInvite) (*WorldTeamMember, error) {
	status, answer, err := self.world.t3.Api().Call(ctx, http.MethodPost, "/network/member-invite-accept", person.PersonalRoot, &model.AcceptNetworkMemberInviteArgs{
		InviteBearer: invite.invite.Bearer(),
	})
	if err != nil {
		return nil, err
	}
	result := &model.AcceptNetworkMemberInviteResult{}
	if status != http.StatusOK || json.Unmarshal(answer, result) != nil || result.Error != nil || result.MemberId == nil {
		message := ""
		if result.Error != nil {
			message = result.Error.Message
		}
		return nil, fmt.Errorf("POST /network/member-invite-accept: %d %s", status, message)
	}
	if result.NetworkId == nil || *result.NetworkId != self.NetworkId {
		return nil, fmt.Errorf("the accept joined another network than %s", self.Name)
	}
	member := &WorldTeamMember{
		WorldPerson: person,
		MemberId:    sidecar.Id(*result.MemberId),
		Role:        invite.Role,
		Root:        worldTeamCredential(self.NetworkId, self.networkName, person.UserId, *result.MemberId),
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.members = append(self.members, member)
	return member, nil
}

// signer, an Owner's device, signs the AdminSet: the first Owner, admins as Admins, threshold 1, which §7.9 allows
// while the set holds at most two members (k = 1 when n <= 2), synced at its entry. Each admin must be a member of
// the config already (AdmitMember). api's effects mirror each role
// into network_member (worldRunConfigLogEffects), which AwaitServerRole waits on.
func (self *WorldTeam) SetAdmins(ctx context.Context, signer *sidecart3.T3Device, admins []*WorldTeamMember) (*bridge.AuthPin, error) {
	adminIds := [][]byte{}
	for _, admin := range admins {
		adminIds = append(adminIds, admin.MemberId.Bytes())
	}
	return self.FileStatement(ctx, signer, bridge.SignedType_SIGNED_ADMIN_SET, &protocol.AdminSet{
		Owners:    [][]byte{self.Owner.MemberId.Bytes()},
		Admins:    adminIds,
		Threshold: 1,
	})
}

// Waits until api's network_member row holds role for member, as the log's effects mirror it, within the team's
// RoleMirrorTimeout, asking every RoleMirrorPollInterval: the effects run as a task, with no hook to wait on.
func (self *WorldTeam) AwaitServerRole(ctx context.Context, member *WorldTeamMember, role model.NetworkMemberRole) error {
	ctx, cancel := context.WithTimeout(ctx, self.settings.RoleMirrorTimeout)
	defer cancel()
	last := ""
	for {
		roles, err := worldNetworkMemberRoles(ctx, self.NetworkId)
		if err == nil {
			last = roles[server.Id(member.MemberId)]
			if last == string(role) {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s's server role stayed %q, not %q: %w", member.Name, last, role, ctx.Err())
		case <-time.After(self.settings.RoleMirrorPollInterval):
		}
	}
}

// admin, an Owner's or Admin's device, signs member's MemberAdmit (§7.9's MemberAdmit row), synced at its entry on
// every live device: at the role the invite named where it is billing_admin, auditor or member, else at member, since
// only an AdminSet signs the first four roles and it names only members (SetAdmins).
func (self *WorldTeam) AdmitMember(ctx context.Context, admin *sidecart3.T3Device, member *WorldTeamMember) (*bridge.AuthPin, error) {
	role := member.Role
	switch role {
	case bridge.MemberRole_MEMBER_ROLE_BILLING_ADMIN, bridge.MemberRole_MEMBER_ROLE_AUDITOR, bridge.MemberRole_MEMBER_ROLE_MEMBER:
	default:
		role = bridge.MemberRole_MEMBER_ROLE_MEMBER
	}
	return self.FileStatement(ctx, admin, bridge.SignedType_SIGNED_MEMBER_ADMIT, &protocol.MemberAdmit{
		MemberId: member.MemberId.Bytes(),
		Role:     role,
	})
}

// The first device of a member who joined by invite, then count-1 more, enrolled (EnrollMemberDevices) and admitted
// (AdmitMemberDevices).
func (self *WorldTeam) AddMemberDevices(ctx context.Context, member *WorldTeamMember, invite *WorldTeamInvite, count int) ([]*sidecart3.T3Device, error) {
	if count < 1 {
		return nil, nil
	}
	devices, err := self.EnrollMemberDevices(ctx, member, count, "")
	if err != nil {
		return nil, err
	}
	if err := self.AdmitMemberDevices(ctx, member, invite, devices); err != nil {
		return nil, err
	}
	return devices, nil
}

// count devices of member, each enrolled under the member's team credential, Online and not yet admitted; name, where
// set, starts their names (T3EnrollArgs.Name), so enrollments of several members may run at once.
func (self *WorldTeam) EnrollMemberDevices(ctx context.Context, member *WorldTeamMember, count int, name string) ([]*sidecart3.T3Device, error) {
	devices, err := self.AddSidecarsWith(ctx, &sidecart3.T3EnrollArgs{Root: member.Root, MemberId: member.MemberId, Count: count, Name: name})
	if err != nil {
		return nil, fmt.Errorf("%s's devices: %w", member.Name, err)
	}
	return devices, nil
}

// Admits member's enrolled devices, then has each follow: the first, where invite is the member's, by the inviting
// device with SIDECAR_CAPABILITY_INVITED once its join's MAC verifies under the invite's fragment (§7.5), else by the
// first Owner's first device; the rest by that consented first device (§7.9's scoped row). A member who already has
// a device admitted has each new one admitted by that device.
func (self *WorldTeam) AdmitMemberDevices(ctx context.Context, member *WorldTeamMember, invite *WorldTeamInvite, devices []*sidecart3.T3Device) error {
	if len(devices) == 0 {
		return nil
	}
	var admitter *sidecart3.T3Device
	if held := member.Devices(); 0 < len(held) {
		admitter = held[0]
	}
	pending := devices
	if admitter == nil {
		first := devices[0]
		pending = devices[1:]
		if invite != nil {
			identity := first.Sidecar().Identity()
			join := &sidecar.MemberInviteJoin{
				NetworkId:         sidecar.Id(self.NetworkId),
				InviteId:          invite.InviteId,
				SidecarId:         identity.SidecarId,
				IdentityPublicKey: identity.IdentityPublicKey,
				MemberId:          member.MemberId,
			}
			// the joining device MACs its request with the fragment's secret, and the inviting device checks it
			mac, err := invite.invite.Fragment().JoinMac(join)
			if err != nil {
				return fmt.Errorf("%s's join: %w", first.Name, err)
			}
			if err := sidecar.VerifyMemberInviteJoin(invite.invite.Fragment(), join, mac); err != nil {
				return fmt.Errorf("%s's join did not verify: %w", first.Name, err)
			}
			if err := self.admitBy(ctx, invite.Inviter, first, member.MemberId, uint32(bridge.SidecarCapability_SIDECAR_CAPABILITY_INVITED)); err != nil {
				return err
			}
		} else if err := self.admitBy(ctx, self.Owner.Devices()[0], first, member.MemberId, 0); err != nil {
			return err
		}
		if err := self.Follow(ctx, devices[:1]); err != nil {
			return err
		}
		admitter = first
		func() {
			member.stateLock.Lock()
			defer member.stateLock.Unlock()
			member.devices = append(member.devices, first)
		}()
	}
	for _, device := range pending {
		if err := self.admitBy(ctx, admitter, device, member.MemberId, 0); err != nil {
			return err
		}
	}
	if 0 < len(pending) {
		if err := self.Follow(ctx, pending); err != nil {
			return err
		}
	}
	member.stateLock.Lock()
	defer member.stateLock.Unlock()
	member.devices = append(member.devices, pending...)
	return nil
}

// admitter admits device under memberId with capabilities: a device of the same member as `ur approve` does on its
// approval item (§5.6, §7.9's scoped row), any other, an Owner's or Admin's, by signing the SidecarAdmit and filing it
// through api, as an admin device admits another member's device (§7.9's SidecarAdmit row).
func (self *WorldTeam) admitBy(ctx context.Context, admitter *sidecart3.T3Device, device *sidecart3.T3Device, memberId sidecar.Id, capabilities uint32) error {
	identity := device.Sidecar().Identity()
	if admitter.MemberId != memberId {
		_, err := self.FileStatement(ctx, admitter, bridge.SignedType_SIGNED_SIDECAR_ADMIT, &protocol.SidecarAdmit{
			NetworkId:         sidecar.Id(self.NetworkId).Bytes(),
			SidecarId:         identity.SidecarId.Bytes(),
			IdentityPublicKey: identity.IdentityPublicKey,
			IdentityAlg:       "ed25519",
			MemberId:          memberId.Bytes(),
			Capabilities:      capabilities,
		})
		if err != nil {
			return fmt.Errorf("%s's admission by %s: %w", device.Name, admitter.Name, err)
		}
		return nil
	}
	approvalId, err := admitter.Sidecar().OpenAdmission(&sidecar.SidecarAdmissionRequest{
		SidecarId:         identity.SidecarId,
		MemberId:          memberId,
		IdentityPublicKey: identity.IdentityPublicKey,
		Capabilities:      capabilities,
	})
	if err != nil {
		return fmt.Errorf("%s's admission item on %s: %w", device.Name, admitter.Name, err)
	}
	if err := admitter.Sidecar().DecideApproval(ctx, approvalId, sidecar.SidecarDecisionApprove, sidecar.SidecarActConfirmed, nil); err != nil {
		return fmt.Errorf("%s's admission by %s: %w", device.Name, admitter.Name, err)
	}
	return nil
}

// count tagged sidecars of tag, their keys the first Owner's (EnrollTagged), then admitted by admin, an Owner's device,
// signing each SidecarAdmit with that tag id (§7.7's tags, set at admission within tagOwners), and following
// (AdmitTaggedDevices).
func (self *WorldTeam) AddTagged(ctx context.Context, admin *sidecart3.T3Device, tag string, count int) ([]*sidecart3.T3Device, error) {
	devices, err := self.EnrollTagged(ctx, self.Owner, tag, count, "")
	if err != nil {
		return nil, err
	}
	if err := self.AdmitTaggedDevices(ctx, admin, tag, devices); err != nil {
		return nil, err
	}
	return devices, nil
}

// count tagged sidecars of tag, Online and not yet admitted: each enrolled from a one-use auth key carrying the tag's
// id, made under sponsor's team credential, an Owner's or an Admin's (a Member makes keys for its own devices alone,
// §7.5). The enrollment runs on the sponsor's live membership, and the client it mints is the team's: its token names
// the team's owner of record with no member_id (QD-1194). name, where set, starts their names (T3EnrollArgs.Name), so
// enrollments may run at once.
func (self *WorldTeam) EnrollTagged(ctx context.Context, sponsor *WorldTeamMember, tag string, count int, name string) ([]*sidecart3.T3Device, error) {
	tagId, ok := self.TagId(tag)
	if !ok {
		return nil, fmt.Errorf("the team's document names no tag %s", tag)
	}
	devices, err := self.AddSidecarsWith(ctx, &sidecart3.T3EnrollArgs{Root: sponsor.Root, Tags: []string{hex.EncodeToString(tagId.Bytes())}, Count: count, Name: name})
	if err != nil {
		return nil, fmt.Errorf("%s's tagged sidecars, %s's keys: %w", tag, sponsor.Name, err)
	}
	return devices, nil
}

// admin, an Owner's device, admits each of devices carrying tag alone (AdmitTagged), and each then follows; the team
// keeps them among its tagged sidecars.
func (self *WorldTeam) AdmitTaggedDevices(ctx context.Context, admin *sidecart3.T3Device, tag string, devices []*sidecart3.T3Device) error {
	tagId, ok := self.TagId(tag)
	if !ok {
		return fmt.Errorf("the team's document names no tag %s", tag)
	}
	for _, device := range devices {
		if err := self.AdmitTagged(ctx, admin, device, tagId); err != nil {
			return err
		}
	}
	if err := self.Follow(ctx, devices); err != nil {
		return err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.tagged = append(self.tagged, devices...)
	return nil
}

// admin signs device's SidecarAdmit carrying tagId alone, filed through api.
func (self *WorldTeam) AdmitTagged(ctx context.Context, admin *sidecart3.T3Device, device *sidecart3.T3Device, tagId sidecar.Id) error {
	return WorldAdmitTagged(ctx, self.T3Network, admin, device, tagId, self.settings.TaggedAdmissionLife)
}

// admin signs device's SidecarAdmit in network carrying tagId alone and a not_after life from now, which a tagged
// admission carries (§7.9's type rules: tombstoned until its not_after), filed through api.
func WorldAdmitTagged(ctx context.Context, network *sidecart3.T3Network, admin *sidecart3.T3Device, device *sidecart3.T3Device, tagId sidecar.Id, life time.Duration) error {
	identity := device.Sidecar().Identity()
	_, err := network.FileStatement(ctx, admin, bridge.SignedType_SIGNED_SIDECAR_ADMIT, &protocol.SidecarAdmit{
		NetworkId:         sidecar.Id(network.NetworkId).Bytes(),
		SidecarId:         identity.SidecarId.Bytes(),
		IdentityPublicKey: identity.IdentityPublicKey,
		IdentityAlg:       "ed25519",
		TagIds:            [][]byte{tagId.Bytes()},
		NotAfter:          time.Now().Add(life).UnixMilli(),
	})
	if err != nil {
		return fmt.Errorf("%s's tagged admission: %w", device.Name, err)
	}
	return nil
}

// remover, an Owner or Admin, removes member at api (DenyMember), then admin, an admin's device, signs its MemberRevoke
// (RevokeMember). The member's devices are retired from the syncs before the denial, which deactivates their clients
// at api at once, so that a statement another burst files meanwhile never waits on a device that follows nothing more.
// The MemberRevoke's pin.
func (self *WorldTeam) RemoveMember(ctx context.Context, remover *WorldTeamMember, admin *sidecart3.T3Device, member *WorldTeamMember) (*bridge.AuthPin, error) {
	for _, device := range member.Devices() {
		self.Retire(device)
	}
	if err := self.DenyMember(ctx, remover, member); err != nil {
		return nil, err
	}
	return self.RevokeMember(ctx, admin, member)
}

// remover, an Owner or Admin, removes member at api: POST /network/member-remove, which denies it at once (§7.10). A
// refusal, a member api no longer holds among them, answers its status and message.
func (self *WorldTeam) DenyMember(ctx context.Context, remover *WorldTeamMember, member *WorldTeamMember) error {
	status, answer, err := self.world.t3.Api().Call(ctx, http.MethodPost, "/network/member-remove", remover.Root, &controller.RemoveNetworkMemberArgs{MemberId: server.Id(member.MemberId)})
	if err != nil {
		return err
	}
	result := &controller.RemoveNetworkMemberResult{}
	if status != http.StatusOK || json.Unmarshal(answer, result) != nil || result.Error != nil || result.MemberId == nil {
		message := ""
		if result.Error != nil {
			message = result.Error.Message
		}
		return fmt.Errorf("POST /network/member-remove: %d %s", status, message)
	}
	return nil
}

// admin, an admin's device, signs member's MemberRevoke (§7.10), the removal's cryptographic half, after the member's
// devices are retired from the syncs. The MemberRevoke's pin.
func (self *WorldTeam) RevokeMember(ctx context.Context, admin *sidecart3.T3Device, member *WorldTeamMember) (*bridge.AuthPin, error) {
	func() {
		member.stateLock.Lock()
		defer member.stateLock.Unlock()
		member.removed = true
	}()
	for _, device := range member.Devices() {
		self.Retire(device)
	}
	return self.FileStatement(ctx, admin, bridge.SignedType_SIGNED_MEMBER_REVOKE, &bridge.MemberRevoke{MemberId: member.MemberId.Bytes()})
}

// admin revokes device (SidecarRevoke, reason), which is retired from the syncs first.
func (self *WorldTeam) RevokeDevice(ctx context.Context, admin *sidecart3.T3Device, device *sidecart3.T3Device, reason bridge.SidecarRevokeReason) (*bridge.AuthPin, error) {
	self.Retire(device)
	return self.FileStatement(ctx, admin, bridge.SignedType_SIGNED_SIDECAR_REVOKE, &bridge.SidecarRevoke{
		SidecarId: device.Sidecar().Identity().SidecarId.Bytes(),
		Reason:    reason,
	})
}

// Every member's live devices and the live tagged sidecars, in the network's order: what a convergence check reads.
func (self *WorldTeam) LiveDevices() []*sidecart3.T3Device {
	return self.T3Network.LiveDevices()
}

// The server's role for a signed member role, as an invite names it.
func worldTeamServerRole(role bridge.MemberRole) (model.NetworkMemberRole, bool) {
	switch role {
	case bridge.MemberRole_MEMBER_ROLE_OWNER:
		return model.NetworkMemberRoleOwner, true
	case bridge.MemberRole_MEMBER_ROLE_ADMIN:
		return model.NetworkMemberRoleAdmin, true
	case bridge.MemberRole_MEMBER_ROLE_NETWORK_ADMIN:
		return model.NetworkMemberRoleNetworkAdmin, true
	case bridge.MemberRole_MEMBER_ROLE_IT_ADMIN:
		return model.NetworkMemberRoleItAdmin, true
	case bridge.MemberRole_MEMBER_ROLE_BILLING_ADMIN:
		return model.NetworkMemberRoleBillingAdmin, true
	case bridge.MemberRole_MEMBER_ROLE_AUDITOR:
		return model.NetworkMemberRoleAuditor, true
	case bridge.MemberRole_MEMBER_ROLE_MEMBER:
		return model.NetworkMemberRoleMember, true
	}
	return "", false
}

// The team world's own check: a team of its first Owner's two devices, an Admin who joins by invite with two devices, a
// Member who joins by invite with one, its auth key its own (§7.5's Member: own devices; team-member-own-auth-key), and
// a tagged sidecar, its key the first Owner's (team-tagged-enroll), every device following one log, which names every
// member and every sidecar as admitted, each member's under that member and the tagged one under its one tag.
func TestE2eWorldTeamNetwork(t *testing.T) {
	e2eTestEnv().Run(t, func(t testing.TB) {
		worldSettings := DefaultWorldSettings()
		worldSettings.NetworkCount = 0
		world := NewWorld(t, worldSettings)
		defer world.Close()
		ctx, cancel := world.StepContext(t.Context(), 8*time.Minute)
		defer cancel()
		teamSettings := DefaultWorldTeamSettings()
		teamSettings.SidecarLimits["config_bases_per_min"] = 6000
		team, err := world.AddTeamNetwork(ctx, teamSettings)
		if err != nil {
			world.FailProgress(t, "the team", err, nil)
		}
		owner := team.Owner.Devices()[0]
		// an Admin, whose devices enroll under its own credential, and a Member, whose device enrolls from an auth key
		// its own credential makes
		joins := []struct {
			role        bridge.MemberRole
			deviceCount int
		}{
			{role: bridge.MemberRole_MEMBER_ROLE_ADMIN, deviceCount: 2},
			{role: bridge.MemberRole_MEMBER_ROLE_MEMBER, deviceCount: 1},
		}
		for index, join := range joins {
			person, err := world.NewPerson(ctx, fmt.Sprintf("member-%d", index))
			if err != nil {
				t.Fatal(err)
			}
			invite, err := team.Invite(ctx, team.Owner, owner, person.Email, join.role)
			if err != nil {
				t.Fatalf("%s's invite: %v", person.Name, err)
			}
			member, err := team.Accept(ctx, person, invite)
			if err != nil {
				t.Fatalf("%s's accept: %v", person.Name, err)
			}
			if _, err := team.AdmitMember(ctx, owner, member); err != nil {
				t.Fatalf("%s's MemberAdmit: %v", person.Name, err)
			}
			if join.role == bridge.MemberRole_MEMBER_ROLE_ADMIN {
				if _, err := team.SetAdmins(ctx, owner, []*WorldTeamMember{member}); err != nil {
					t.Fatalf("%s's AdminSet: %v", person.Name, err)
				}
				if err := team.AwaitServerRole(ctx, member, model.NetworkMemberRoleAdmin); err != nil {
					world.FailProgress(t, person.Name+"'s role", err, nil)
				}
			}
			if _, err := team.AddMemberDevices(ctx, member, invite, join.deviceCount); err != nil {
				t.Errorf("%s's devices: %v", person.Name, err)
			}
		}
		// a tagged sidecar of the team, its key the first Owner's
		tagged, err := team.AddTagged(ctx, owner, teamSettings.Tags[0], 1)
		if err != nil {
			t.Errorf("the tagged sidecar: %v", err)
		}
		tip := owner.Follower().Client().Pin().GetVersion()
		if err := team.Sync(ctx, tip); err != nil {
			world.FailProgress(t, "the team's convergence", err, nil)
		}
		for _, device := range team.LiveDevices() {
			state := device.Follower().VerifiedState()
			for _, member := range team.Members() {
				if state.Member(member.MemberId) == nil {
					t.Errorf("%s's state names no member %s (%s)", device.Name, member.MemberId, member.Name)
				}
				for _, memberDevice := range member.Devices() {
					admitted := state.Sidecar(memberDevice.Sidecar().Identity().SidecarId)
					if admitted == nil || admitted.MemberId != member.MemberId {
						t.Errorf("%s's state does not admit %s under %s", device.Name, memberDevice.Name, member.Name)
					}
				}
			}
			for _, taggedDevice := range tagged {
				admitted := state.Sidecar(taggedDevice.Sidecar().Identity().SidecarId)
				if admitted == nil || len(admitted.TagIds) != 1 {
					t.Errorf("%s's state does not admit %s with its one tag", device.Name, taggedDevice.Name)
				}
			}
		}
	})
}
