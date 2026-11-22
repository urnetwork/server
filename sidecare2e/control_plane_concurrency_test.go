//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// The control plane under concurrency (the end-to-end plan's e2e-control-plane-concurrency; the owner's end-to-end
// tests of 2026-11-21): enrollment, config and ACL saves racing at one head, team churn, and the limits on them, across
// PersonalNetworkCount personal networks and one team network (world_team_test.go) on the world's real keys.
//
// Setup: each personal network is its owner's first device, which bootstraps it; the team is its first Owner's device,
// which bootstraps it and saves its first document, naming TeamTag owned by autogroup:admin, an Admin who joins by
// invite (MemberAdmit, then an AdminSet naming them Admin, mirrored into network_member by the log's effects) and
// PlainMemberCount members who join by invite as members. Network b saves a document naming tag:ci, owned by
// autogroup:admin.
//
// The phases, each bounded by PhaseTimeout and dumping the world's progress when it stalls:
//
//   - enrollment: EnrollCount devices enroll at once (T3WorldSettings.EnrollConcurrency), through api and the exchange:
//     PersonalEnrollCount owner's devices in each personal network, a tagged sidecar in b; in the team,
//     TeamOwnerEnrollCount Owner's devices, TeamAdminEnrollCount Admin's devices and TeamMemberEnrollCount devices of
//     each setup member, each from an auth key its own member made (§7.5's Member: own devices), and
//     TeamTaggedEnrollCount tagged sidecars of TeamTag, their keys the Admin's; then each is admitted (an owner's
//     devices by its first, a member's first by the inviting device once its invite join's MAC verifies, with the
//     invited mark, the rest by that first device, a tagged one by its tag owner) and follows, every network's
//     admissions, the team's members' and tagged sidecars' among them, at once. What must hold: every enrollment lands
//     once (api holds exactly the devices' clients, each id once); each device gets exactly the scopes §7.8 and §8 give
//     it at evs and gkm (controlPlaneExpectedScopes); each refusal names the design's reason: a spent one-use key
//     (auth_key_invalid), a tagged key before base 1 (no_genesis), a network outside preview_networks (not_preview), an
//     enrolled device no statement admitted (invalid_grant at the exchange), and a Member's key for a tag
//     autogroup:admin owns (403: a Member makes keys for its own devices alone); and each of the team's tagged sidecars
//     works at api on its own credential, which names the team's owner of record with no member (QD-1194;
//     control_plane_concurrency_team_test.go): the state check, a refresh, a window client and the exchange
//   - saves racing: in a personal network and in the team at once, RacerCount config writers race exact-head ACL saves
//     in rounds (every racer previews at one verified head, all save at once, each signing EXACT_HEAD on what it
//     previewed). What must hold: one winner per round, every other save refused as the head having moved, every
//     racer's document landed once; every device of each network on one pin; and the right the first winning document
//     took away (§8.5's implicit document gave every member device admin on every general repo, and every racer's
//     document holds grant 1 alone) refused to the device that held it once evs's gate holds the base, within §8.9's
//     bound, and at the token exchange for gkm (invalid_scope). The config document's other saves (ServiceSettings,
//     which the config writer re-reads and retries on a race) are not raced: the world's devices run no config writer,
//     which ur daemon composes, only the confirmed ACL ref, so SaveServiceSettings refuses on every one of them
//   - team churn, in bursts at once: ChurnInviteCount new members invited by the Owner and the Admin, accepted and
//     admitted; a setup member with devices of its own removed (member-remove, then MemberRevoke) and re-invited,
//     rejoining under a new member_id; the Admin's last device revoked; the Owner's vault shared with the Admin's first
//     device by EvsShare and the share revoked; and in b, TaggedChurnCount tagged sidecars admitted and one revoked.
//     Then the Admin is moved out of the AdminSet and removed. What must hold: every live device's state, api's member
//     rows and the exchange agree on who is a member and who is admitted; a revoked or removed device is refused at the
//     exchange's projection (controlPlaneExchange, since a device that learned of its revocation asks for nothing), and
//     a removed member's devices are revoked in every live device's state (§7.10: a MemberRevoke revokes the member's
//     untagged devices) and their clients deactivated at api, and api holds every rev/ object its removal owes within
//     RevocationSettleTimeout; the share's subject is granted the vault's read and takes
//     its key while the share stands, is refused the read once it is revoked, and takes no later generation; at the
//     next rotation of the config repo every live device, the team's tagged sidecars among them, takes the new
//     generation while no revoked or removed one does; and the team's tagged sidecars, the Admin's keys, outlive the
//     Admin's removal (owner decision 83, QD-1194): admitted in every live device's state, their clients active at api,
//     and at api on their own credentials as at the enrollment. The share is judged at the exchange and at gkm, not by
//     a read: a reader of an owner-bound repo that does not write it deposits a read receipt first (§16.8, D-MCP-18),
//     and the world's devices wire no receipt gate, so evs refuses that read on each of them (ErrEvsReceiptGate)
//   - limits: config_admissions in b set to its admissions plus AdmissionHeadroom, then AdmissionHeadroom plus
//     AdmissionOverflow tagged admissions filed at once: exactly the headroom lands, the rest refused by that limit
//   - ledgers: api's write budget reconciled exactly with the objects api wrote for every statement, base and
//     removal of the case (control_plane_concurrency_harness_test.go)
//
// A failure from a product bug stays failing, and the case runs on so one finding does not hide the next. As the case
// landed (e2e-control-plane-concurrency, 2026-11-22) it failed on two findings, which kept every team device but the
// Owner's and the Admin's out of it: a Member's own auth key refused 403 (finding A) and a tagged enrollment in a team
// answered 500 (finding B). Both fixes have landed,
// team-member-own-auth-key (server c5579f826 to 927a07e0d) and team-tagged-enroll (server c730a0441 to e9b9598d1,
// QD-1194), and the case now runs what they blocked; e2e-control-plane-verify (2026-11-22) widened it so. Three parts
// are cases of their own: the chaos, api and a member device restarted mid-save, mid-invite, mid-removal and
// mid-statement on their stores (TestE2eControlPlaneConcurrencyChaos), a reader starting cold at a base a later config
// writer signed (TestE2eControlPlaneConcurrencyColdGate, finding C), and the other quotas §20 names under concurrent
// use, evs storage, sidecar_clients and dns_entries (TestE2eControlPlaneConcurrencyQuota). Run through the build's
// wrapper: -tags 'sidecar_e2e sidecar_pending sidecar_localstack'.

package sidecare2e

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/oauth"
	"github.com/urnetwork/server/sidecart3"
)

// What the case runs with (QD-702): every count, document and bound it uses.
type controlPlaneSettings struct {
	// The personal networks, each its owner's first device at setup.
	PersonalNetworkCount int
	// The plain members who join the team by invite at setup.
	PlainMemberCount int
	// EnrollCount, the devices the first phase enrolls at once: PersonalEnrollCount in each personal network, one
	// tagged sidecar in b, and in the team TeamOwnerEnrollCount of the Owner's, TeamAdminEnrollCount of the Admin's,
	// TeamMemberEnrollCount of each setup member's and TeamTaggedEnrollCount tagged sidecars of TeamTag.
	EnrollCount           int
	PersonalEnrollCount   int
	TeamOwnerEnrollCount  int
	TeamAdminEnrollCount  int
	TeamMemberEnrollCount int
	TeamTaggedEnrollCount int
	// The tag network b's document names, owned by autogroup:admin.
	Tag string
	// The tag every document of the team names, owned by autogroup:admin (Team.Tags), whose tagged sidecars' keys the
	// Admin makes, so they outlive their sponsor's removal at the churn's end.
	TeamTag string
	// The config writers racing exact-head saves in each raced network.
	RacerCount int
	// The new members the churn invites, and the tagged sidecars it admits in b, one of them then revoked.
	ChurnInviteCount int
	TaggedChurnCount int
	// The limits phase's headroom under config_admissions, and the admissions past it filed at once.
	AdmissionHeadroom int
	AdmissionOverflow int
	// §7.9's config_bases_per_min for the case: every save publishes a base, which the Community column's 1 a minute
	// would hold a minute each.
	ConfigBasesPerMinute int64
	// grant 1 (§8.5, owner decision 344), which every document of the case holds, so no rule gives a member device evs
	// access.
	GrantOne string
	// Each racer's document past grant 1: a dns rule naming the racer's own name, so each document differs.
	RaceGrant func(index int) string
	// §8.9's bound on a gate's following the log: a right a base took away is refused within it.
	GateBound time.Duration
	// How long a device may take to read a repo's file it was given, its key delivered by a holder after gkm's hint
	// (DESIGN-DETAILS 6.2.4), and how often it asks meanwhile.
	ReadSettleTimeout  time.Duration
	ReadSettleInterval time.Duration
	// How long a device's key client may take to hold a rotated generation, and how often it is asked meanwhile.
	RotationSettleTimeout  time.Duration
	RotationSettleInterval time.Duration
	// How long a member's removal may take to land every rev/ object it owes (§7.10), its MemberRevoke's effects
	// writing theirs as a task does, with no hook a test reads, and how often the store is asked meanwhile.
	RevocationSettleTimeout  time.Duration
	RevocationSettleInterval time.Duration
	// How a preview is asked again while evs answers unavailable.
	Retry *controlPlaneRetrySettings
	// The team's settings.
	Team *WorldTeamSettings
	// How the ledgers at the end are read.
	Ledgers *controlPlaneLedgerSettings
	// The bursts' pauses and each phase's bound.
	Bursts       *WorldBurstSettings
	PhaseTimeout time.Duration
}

func defaultControlPlaneSettings() *controlPlaneSettings {
	teamTag := "team-ci"
	team := DefaultWorldTeamSettings()
	team.OwnerDeviceCount = 1
	team.Tags = []string{teamTag}
	team.SidecarLimits["config_bases_per_min"] = 6000
	return &controlPlaneSettings{
		PersonalNetworkCount:  2,
		PlainMemberCount:      2,
		EnrollCount:           22,
		PersonalEnrollCount:   4,
		TeamOwnerEnrollCount:  1,
		TeamAdminEnrollCount:  6,
		TeamMemberEnrollCount: 2,
		TeamTaggedEnrollCount: 2,
		Tag:                   "ci",
		TeamTag:               teamTag,
		RacerCount:            4,
		ChurnInviteCount:      3,
		TaggedChurnCount:      2,
		AdmissionHeadroom:     2,
		AdmissionOverflow:     2,
		ConfigBasesPerMinute:  6000,
		GrantOne:              `{"src": ["autogroup:member", "autogroup:untagged"], "dst": ["autogroup:self"], "via": ["autogroup:self"], "allow": ["dial", "route"]}`,
		RaceGrant: func(index int) string {
			return fmt.Sprintf(`{"src": ["autogroup:member"], "dst": ["dns:race-%d"], "allow": ["admin"]}`, index)
		},
		GateBound:                5 * time.Second,
		ReadSettleTimeout:        90 * time.Second,
		ReadSettleInterval:       500 * time.Millisecond,
		RotationSettleTimeout:    90 * time.Second,
		RotationSettleInterval:   250 * time.Millisecond,
		RevocationSettleTimeout:  60 * time.Second,
		RevocationSettleInterval: 250 * time.Millisecond,
		Retry:                    &controlPlaneRetrySettings{Timeout: 2 * time.Minute, Interval: 500 * time.Millisecond},
		Team:                     team,
		Ledgers:                  defaultControlPlaneLedgerSettings(),
		Bursts:                   DefaultWorldBurstSettings(),
		PhaseTimeout:             4 * time.Minute,
	}
}

// One run of the case: the world, its networks and what each phase recorded.
type controlPlaneCase struct {
	t        testing.TB
	settings *controlPlaneSettings
	world    *World
	// the personal networks, a and b, and the team with its Admin and setup members
	personal []*WorldNetwork
	team     *WorldTeam
	admin    *WorldTeamMember
	// the invites each setup member joined by, which their first device proves, plainInvites by plain's order
	adminInvite  *WorldTeamInvite
	plain        []*WorldTeamMember
	plainInvites []*WorldTeamInvite
	// b's tag
	tagId sidecar.Id
	// the team's owner of record, which its tagged sidecars' tokens name, and its tagged sidecars, the Admin's keys
	ownerOfRecord server.Id
	teamTagged    []*sidecart3.T3Device

	// the exchange's own judgment, over a gate cache of the case's
	exchange *controlPlaneExchange
	// the rev/ writer's writes in this process as the world began, which api's write budget is read against
	revWritesAtStart *controlPlaneRevWrites

	stateLock sync.Mutex
	// each device's kind, which its scopes follow
	kinds map[*sidecart3.T3Device]controlPlaneDeviceKind
}

// A device's kind, as the case recorded it.
func (self *controlPlaneCase) kind(device *sidecart3.T3Device) controlPlaneDeviceKind {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.kinds[device]
}

// Records devices' kind.
func (self *controlPlaneCase) setKind(kind controlPlaneDeviceKind, devices ...*sidecart3.T3Device) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, device := range devices {
		self.kinds[device] = kind
	}
}

func TestE2eControlPlaneConcurrency(t *testing.T) {
	settings := defaultControlPlaneSettings()
	e2eTestEnv().Run(t, func(t testing.TB) {
		self := newControlPlaneCase(t, settings)
		defer self.world.Close()
		self.setup()
		self.enrollment()
		self.saves()
		self.churn()
		self.limits()
		self.ledgers()
	})
}

// The ledgers phase (the file's header).
func (self *controlPlaneCase) ledgers() {
	ctx, cancel := self.world.StepContext(self.t.Context(), self.settings.PhaseTimeout)
	defer cancel()
	controlPlaneCheckApiBudget(self.t, ctx, self.world, self.settings.Ledgers, self.revWritesAtStart)
}

// The world, with no network yet, its Community limits taking the case's base rate and its enrollments all at once.
func newControlPlaneCase(t testing.TB, settings *controlPlaneSettings) *controlPlaneCase {
	if settings.PersonalNetworkCount < 2 {
		t.Fatalf("the case needs two personal networks, not %d", settings.PersonalNetworkCount)
	}
	if settings.PlainMemberCount < 1 {
		t.Fatalf("the churn removes a setup member, and %d setup members leave none to remove", settings.PlainMemberCount)
	}
	total := settings.PersonalNetworkCount*settings.PersonalEnrollCount + 1 + settings.TeamOwnerEnrollCount + settings.TeamAdminEnrollCount +
		settings.PlainMemberCount*settings.TeamMemberEnrollCount + settings.TeamTaggedEnrollCount
	if total != settings.EnrollCount {
		t.Fatalf("the enrollment phase's counts sum to %d devices, not EnrollCount %d", total, settings.EnrollCount)
	}
	if !slices.Contains(settings.Team.Tags, settings.TeamTag) {
		t.Fatalf("the team's documents name tags %v, not TeamTag %s", settings.Team.Tags, settings.TeamTag)
	}
	if settings.PersonalEnrollCount+1 < settings.RacerCount || settings.TeamOwnerEnrollCount+1+settings.TeamAdminEnrollCount < settings.RacerCount {
		t.Fatalf("%d racers outnumber a network's config writers", settings.RacerCount)
	}
	revWritesAtStart, err := controlPlaneRevWriteCounts()
	if err != nil {
		t.Fatalf("the rev/ writer's writes: %v", err)
	}
	worldSettings := DefaultWorldSettings()
	worldSettings.NetworkCount = 0
	t3Settings := sidecart3.DefaultT3WorldSettings()
	t3Settings.EnrollConcurrency = settings.EnrollCount
	limits := sidecart3.DefaultT3WorldSidecarLimits()
	limits.LimitKeyValues["config_bases_per_min"] = settings.ConfigBasesPerMinute
	t3Settings.SidecarLimits = limits
	worldSettings.T3 = t3Settings
	world := NewWorld(t, worldSettings)
	exchange, err := newControlPlaneExchange(world, settings.Retry)
	if err != nil {
		t.Fatalf("the exchange's projection: %v", err)
	}
	return &controlPlaneCase{
		t:                t,
		settings:         settings,
		world:            world,
		exchange:         exchange,
		revWritesAtStart: revWritesAtStart,
		kinds:            map[*sidecart3.T3Device]controlPlaneDeviceKind{},
	}
}

// The exchange refuses device whole, as a revoked or removed client's (§7.8: invalid_grant), at its projection.
func (self *controlPlaneCase) checkExchangeRefused(ctx context.Context, device *sidecart3.T3Device, why string) {
	configRepoId, _, err := controlPlaneNetworkRepos(device.Network.Owner())
	if err != nil {
		self.t.Errorf("%s's network's repos: %v", device.Name, err)
		return
	}
	grant, err := self.exchange.grant(ctx, device, WorldBlockEvs, []string{controlPlaneRepoScope(configRepoId, "r")})
	if err == nil {
		self.t.Errorf("%s, %s, is still granted %v at the exchange", device.Name, why, grant.Scopes)
	} else if !errors.Is(err, oauth.ErrInvalidGrant) {
		self.t.Errorf("%s, %s, was refused at the exchange other than as invalid_grant: %v", device.Name, why, err)
	}
}

// The setup (the file's header).
func (self *controlPlaneCase) setup() {
	t := self.t
	settings := self.settings
	ctx, cancel := self.world.StepContext(t.Context(), settings.PhaseTimeout)
	defer cancel()
	for index := range settings.PersonalNetworkCount {
		network, err := self.world.AddNetwork(ctx, worldNetworkName(index), 1, nil)
		if err != nil {
			self.world.FailProgress(t, "the personal networks", err, nil)
		}
		self.personal = append(self.personal, network)
		self.setKind(controlPlaneDeviceKind{configWriter: true, member: true}, network.Clients...)
	}
	b := self.personal[1]
	tagged := `{"tagOwners": {"tag:` + settings.Tag + `": ["autogroup:admin"]}, "grants": [` + settings.GrantOne + `]}`
	if _, err := self.world.SaveAclPolicy(ctx, b, b.Owner(), tagged); err != nil {
		self.world.FailProgress(t, "b's tagged document", err, nil)
	}
	tagId, err := controlPlaneTagId(ctx, b.Owner(), tagged, settings.Tag)
	if err != nil {
		t.Fatalf("b's tag: %v", err)
	}
	self.tagId = tagId

	team, err := self.world.AddTeamNetwork(ctx, settings.Team)
	if err != nil {
		self.world.FailProgress(t, "the team", err, nil)
	}
	self.team = team
	if _, ok := team.TagId(settings.TeamTag); !ok {
		t.Fatalf("the team's first document holds no tag %s", settings.TeamTag)
	}
	if self.ownerOfRecord, err = worldNetworkOwnerOfRecord(ctx, team.NetworkId); err != nil {
		t.Fatal(err)
	}
	self.setKind(controlPlaneDeviceKind{configWriter: true, member: true}, team.Owner.Devices()...)
	ownerDevice := team.Owner.Devices()[0]
	join := func(name string, role bridge.MemberRole) (*WorldTeamMember, *WorldTeamInvite) {
		person, err := self.world.NewPerson(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		invite, err := team.Invite(ctx, team.Owner, ownerDevice, person.Email, role)
		if err != nil {
			t.Fatalf("%s's invite: %v", name, err)
		}
		member, err := team.Accept(ctx, person, invite)
		if err != nil {
			t.Fatalf("%s's accept: %v", name, err)
		}
		if _, err := team.AdmitMember(ctx, ownerDevice, member); err != nil {
			self.world.FailProgress(t, name+"'s MemberAdmit", err, nil)
		}
		return member, invite
	}
	self.admin, self.adminInvite = join("admin", bridge.MemberRole_MEMBER_ROLE_ADMIN)
	if _, err := team.SetAdmins(ctx, ownerDevice, []*WorldTeamMember{self.admin}); err != nil {
		self.world.FailProgress(t, "the AdminSet", err, nil)
	}
	if err := team.AwaitServerRole(ctx, self.admin, model.NetworkMemberRoleAdmin); err != nil {
		self.world.FailProgress(t, "the Admin's mirrored role", err, nil)
	}
	for index := range settings.PlainMemberCount {
		member, invite := join(fmt.Sprintf("member-%d", index), bridge.MemberRole_MEMBER_ROLE_MEMBER)
		self.plain = append(self.plain, member)
		self.plainInvites = append(self.plainInvites, invite)
	}
}

// The enrollment phase (the file's header).
func (self *controlPlaneCase) enrollment() {
	t := self.t
	settings := self.settings
	world := self.world
	ctx, cancel := world.StepContext(t.Context(), settings.PhaseTimeout)
	defer cancel()
	team := self.team
	b := self.personal[1]
	ownerDevice := team.Owner.Devices()[0]

	// every enrollment at once, one goroutine a network or member
	type enrolled struct {
		name    string
		devices []*sidecart3.T3Device
		err     error
	}
	results := []*enrolled{}
	var group sync.WaitGroup
	start := func(name string, enroll func() ([]*sidecart3.T3Device, error)) {
		result := &enrolled{name: name}
		results = append(results, result)
		group.Go(func() {
			result.devices, result.err = enroll()
		})
	}
	for _, network := range self.personal {
		start(network.Name, func() ([]*sidecart3.T3Device, error) {
			return network.AddSidecarsWith(ctx, &sidecart3.T3EnrollArgs{Count: settings.PersonalEnrollCount, Name: network.Name + "-dev"})
		})
	}
	start("b's tagged sidecar", func() ([]*sidecart3.T3Device, error) {
		return b.AddSidecarsWith(ctx, &sidecart3.T3EnrollArgs{Count: 1, Tags: []string{controlPlaneTagHex(self.tagId)}, Name: b.Name + "-tagged"})
	})
	start("the team's Owner", func() ([]*sidecart3.T3Device, error) {
		return team.EnrollMemberDevices(ctx, team.Owner, settings.TeamOwnerEnrollCount, team.Name+"-owner")
	})
	start("the team's Admin", func() ([]*sidecart3.T3Device, error) {
		return team.EnrollMemberDevices(ctx, self.admin, settings.TeamAdminEnrollCount, team.Name+"-admin")
	})
	for index, member := range self.plain {
		start(member.Name+"'s devices", func() ([]*sidecart3.T3Device, error) {
			return team.EnrollMemberDevices(ctx, member, settings.TeamMemberEnrollCount, fmt.Sprintf("%s-member-%d", team.Name, index))
		})
	}
	start("the team's tagged sidecars", func() ([]*sidecart3.T3Device, error) {
		return team.EnrollTagged(ctx, self.admin, settings.TeamTag, settings.TeamTaggedEnrollCount, team.Name+"-tagged")
	})
	group.Wait()
	enrolledByName := map[string][]*sidecart3.T3Device{}
	for _, result := range results {
		if result.err != nil {
			world.FailProgress(t, "the enrollment of "+result.name, result.err, nil)
		}
		enrolledByName[result.name] = result.devices
	}

	// every enrollment lands once: each id once, and api holds exactly each network's devices' clients
	clientIds := map[sidecar.Id]string{}
	sidecarIds := map[sidecar.Id]string{}
	for _, network := range append(slices.Clone(self.personal), team.WorldNetwork) {
		for _, device := range network.Devices() {
			identity := device.Sidecar().Identity()
			if other, ok := clientIds[identity.ClientId]; ok {
				t.Errorf("%s and %s enrolled as one client %s", device.Name, other, identity.ClientId)
			}
			if other, ok := sidecarIds[identity.SidecarId]; ok {
				t.Errorf("%s and %s enrolled as one sidecar %s", device.Name, other, identity.SidecarId)
			}
			clientIds[identity.ClientId], sidecarIds[identity.SidecarId] = device.Name, device.Name
		}
		self.checkClientsLandedOnce(ctx, network.T3Network)
	}

	// an enrolled device no statement admitted takes no token (§7.8: refuses unadmitted clients, invalid_grant)
	configRepoId, _, err := controlPlaneNetworkRepos(self.personal[0].Owner())
	if err != nil {
		t.Fatal(err)
	}
	unadmitted := enrolledByName[self.personal[0].Name][0]
	if _, err := controlPlaneGrantedScopes(ctx, world, unadmitted, WorldBlockEvs, []string{controlPlaneRepoScope(configRepoId, "r")}); err == nil {
		t.Errorf("%s, enrolled and not admitted, took an evs token", unadmitted.Name)
	} else if _, code := controlPlaneApiRefusal(err); code != "invalid_grant" {
		t.Errorf("%s's exchange before its admission was refused %v, not invalid_grant", unadmitted.Name, err)
	}

	// the admissions, every network at once
	admissions := []*WorldBurst{}
	for _, network := range self.personal {
		devices := enrolledByName[network.Name]
		self.setKind(controlPlaneDeviceKind{configWriter: true, member: true}, devices...)
		admissions = append(admissions, &WorldBurst{Name: network.Name + "'s admissions", StepCount: 1, Step: func(ctx context.Context, index int, random *rand.Rand) error {
			return network.Admit(ctx, devices)
		}})
	}
	taggedDevices := enrolledByName["b's tagged sidecar"]
	self.setKind(controlPlaneDeviceKind{}, taggedDevices...)
	admissions = append(admissions, &WorldBurst{Name: "b's tagged admission", StepCount: 1, Step: func(ctx context.Context, index int, random *rand.Rand) error {
		for _, device := range taggedDevices {
			if err := self.admitTagged(ctx, b, device); err != nil {
				return err
			}
		}
		return b.Follow(ctx, taggedDevices)
	}})
	ownerDevices := enrolledByName["the team's Owner"]
	adminDevices := enrolledByName["the team's Admin"]
	self.setKind(controlPlaneDeviceKind{configWriter: true, member: true}, ownerDevices...)
	self.setKind(controlPlaneDeviceKind{configWriter: true, member: true}, adminDevices...)
	admissions = append(admissions, &WorldBurst{Name: "the team's admissions", StepCount: 1, Step: func(ctx context.Context, index int, random *rand.Rand) error {
		if err := team.AdmitMemberDevices(ctx, team.Owner, nil, ownerDevices); err != nil {
			return err
		}
		return team.AdmitMemberDevices(ctx, self.admin, self.adminInvite, adminDevices)
	}})
	for memberIndex, member := range self.plain {
		devices := enrolledByName[member.Name+"'s devices"]
		invite := self.plainInvites[memberIndex]
		self.setKind(controlPlaneDeviceKind{member: true}, devices...)
		admissions = append(admissions, &WorldBurst{Name: member.Name + "'s admissions", StepCount: 1, Step: func(ctx context.Context, index int, random *rand.Rand) error {
			return team.AdmitMemberDevices(ctx, member, invite, devices)
		}})
	}
	self.teamTagged = enrolledByName["the team's tagged sidecars"]
	self.setKind(controlPlaneDeviceKind{}, self.teamTagged...)
	admissions = append(admissions, &WorldBurst{Name: "the team's tagged admissions", StepCount: 1, Step: func(ctx context.Context, index int, random *rand.Rand) error {
		return team.AdmitTaggedDevices(ctx, ownerDevice, settings.TeamTag, self.teamTagged)
	}})
	if err := RunBursts(ctx, t, "the admissions", admissions, settings.Bursts); err != nil {
		world.FailProgress(t, "the admissions", err, admissions)
	}
	FailBurstErrors(t, admissions)

	// each device's scopes, at evs and gkm
	checked := 0
	for _, network := range append(slices.Clone(self.personal), team.WorldNetwork) {
		checked += self.checkScopes(ctx, network.T3Network)
	}
	t.Logf("the enrollment: %d devices enrolled at once, %d clients each landed once, %d devices' scopes checked at evs and gkm", settings.EnrollCount, len(clientIds), checked)

	// the design's refusals, a Member's key for a tag it does not own among them
	self.checkEnrollRefusals(ctx)
	self.checkMemberTaggedKeyRefused(ctx)
	// the team's tagged sidecars at api on their own credentials, before their sponsor's removal
	for _, device := range self.teamTagged {
		self.checkTaggedAgent(ctx, device, "at its enrollment")
	}
}

// Every client api holds active for network is a device of the case's there, and every device's client is one of them,
// each once.
func (self *controlPlaneCase) checkClientsLandedOnce(ctx context.Context, network *sidecart3.T3Network) {
	t := self.t
	active, err := worldNetworkActiveClientIds(ctx, network.NetworkId)
	if err != nil {
		t.Errorf("%s's clients: %v", network.Name, err)
		return
	}
	devices := network.Devices()
	held := map[server.Id]bool{}
	for _, device := range devices {
		clientId := server.Id(device.Sidecar().Identity().ClientId)
		held[clientId] = true
		if !active[clientId] {
			t.Errorf("%s's client %s is not active at api", device.Name, clientId)
		}
	}
	for clientId := range active {
		if !held[clientId] {
			t.Errorf("api holds an active client %s of %s that no device enrolled", clientId, network.Name)
		}
	}
	if len(active) != len(devices) {
		t.Errorf("api holds %d active clients of %s for its %d devices", len(active), network.Name, len(devices))
	}
}

// Each live device of network takes exactly the scopes its kind gives it, at evs and at gkm. How many devices it
// checked.
func (self *controlPlaneCase) checkScopes(ctx context.Context, network *sidecart3.T3Network) int {
	t := self.t
	configRepoId, directoryRepoId, err := controlPlaneNetworkRepos(network.Owner())
	if err != nil {
		t.Errorf("%s's repos: %v", network.Name, err)
		return 0
	}
	team := network == self.team.T3Network
	if team && directoryRepoId == (sidecar.Id{}) {
		// §7.5: the bootstrapping Owner's device files the team directory's RepoCreate after base 1
		t.Errorf("%s's state holds no team directory", network.Name)
	}
	devices := network.LiveDevices()
	defer func() {
		t.Logf("%s: %d devices' scopes checked, its directory %s", network.Name, len(devices), directoryRepoId)
	}()
	for _, device := range devices {
		self.checkDeviceScopes(ctx, device, configRepoId, directoryRepoId, "")
	}
	return len(devices)
}

// device takes exactly the scopes its kind gives it of the config repo's and directory's candidates, at evs and at
// gkm, through api's exchange on its own token; when, where set, says at what point of the case.
func (self *controlPlaneCase) checkDeviceScopes(ctx context.Context, device *sidecart3.T3Device, configRepoId sidecar.Id, directoryRepoId sidecar.Id, when string) {
	t := self.t
	candidates := controlPlaneScopeCandidates(configRepoId, directoryRepoId)
	kind := self.kind(device)
	if when != "" {
		when = " " + when
	}
	for _, service := range []string{WorldBlockEvs, WorldBlockGkm} {
		granted, err := controlPlaneGrantedScopes(ctx, self.world, device, service, candidates)
		if err != nil {
			t.Errorf("%s's %s exchange%s: %v", device.Name, service, when, err)
			continue
		}
		expected := controlPlaneExpectedScopes(kind, service, configRepoId, directoryRepoId)
		for _, scope := range candidates {
			if granted[scope] != expected[scope] {
				t.Errorf("%s (%+v) was granted %s at %s%s: %t, where §7.8 and §8 give %t", device.Name, kind, scope, service, when, granted[scope], expected[scope])
			}
		}
	}
}

// b's owner signs device's tagged SidecarAdmit with b's tag, filed through api.
func (self *controlPlaneCase) admitTagged(ctx context.Context, network *WorldNetwork, device *sidecart3.T3Device) error {
	return WorldAdmitTagged(ctx, network.T3Network, network.Owner(), device, self.tagId, self.settings.Team.TaggedAdmissionLife)
}

// The enrollment refusals the design names (§7.8's enroll answer, owner decision 326): a spent one-use key, a tagged key
// before the network's base 1, and a network outside preview_networks; each enrolls nothing.
func (self *controlPlaneCase) checkEnrollRefusals(ctx context.Context) {
	t := self.t
	world := self.world
	// a spent one-use key: the first enrollment takes it, a second device presenting it is refused
	owner, err := world.NewPerson(ctx, "spent-key-owner")
	if err != nil {
		t.Fatal(err)
	}
	spentNetwork, err := world.T3().AddNetworkOf(ctx, &sidecart3.T3NetworkOf{
		Name:          "spent",
		NetworkId:     owner.PersonalNetworkId,
		OwnerUserId:   owner.UserId,
		OwnerMemberId: sidecar.Id(owner.UserId),
		Root:          owner.PersonalRoot,
	})
	if err != nil {
		t.Fatalf("the spent key's network: %v", err)
	}
	key, err := controlPlaneAuthKey(ctx, world, owner.PersonalRoot, 1, nil, false)
	if err != nil {
		t.Fatalf("the one-use key: %v", err)
	}
	if err := controlPlaneEnroll(ctx, world, "spent-first", key); err != nil {
		t.Errorf("the one-use key's first enrollment: %v", err)
	}
	err = controlPlaneEnroll(ctx, world, "spent-second", key)
	if status, code := controlPlaneApiRefusal(err); err == nil || code != model.NetworkEnrollCodeAuthKeyInvalid {
		t.Errorf("a spent one-use key's second enrollment answered %v (%d %q), not %s", err, status, code, model.NetworkEnrollCodeAuthKeyInvalid)
	}
	if active, err := worldNetworkActiveClientIds(ctx, spentNetwork.NetworkId); err != nil || len(active) != 1 {
		t.Errorf("the spent key's network holds %d active clients after one enrollment and one refusal (%v)", len(active), err)
	}

	// a tagged key before base 1: no_genesis (§7.9's enroll row)
	tagged, err := controlPlaneAuthKey(ctx, world, owner.PersonalRoot, 1, []string{controlPlaneTagHex(self.tagId)}, false)
	if err != nil {
		t.Fatalf("the tagged key: %v", err)
	}
	err = controlPlaneEnroll(ctx, world, "no-genesis", tagged)
	if status, code := controlPlaneApiRefusal(err); err == nil || status != http.StatusForbidden || code != model.NetworkEnrollCodeNoGenesis {
		t.Errorf("a tagged enrollment before base 1 answered %v (%d %q), not 403 %s", err, status, code, model.NetworkEnrollCodeNoGenesis)
	}

	// a network outside preview_networks: not_preview (§7.4, D-TRUST-3)
	outsider, err := world.NewPerson(ctx, "outsider")
	if err != nil {
		t.Fatal(err)
	}
	outsiderKey, err := controlPlaneAuthKey(ctx, world, outsider.PersonalRoot, 1, nil, false)
	if err != nil {
		t.Fatalf("the outsider's key: %v", err)
	}
	err = controlPlaneEnroll(ctx, world, "not-preview", outsiderKey)
	if status, code := controlPlaneApiRefusal(err); err == nil || status != http.StatusForbidden || code != model.NetworkEnrollCodeNotPreview {
		t.Errorf("an enrollment outside preview_networks answered %v (%d %q), not 403 %s", err, status, code, model.NetworkEnrollCodeNotPreview)
	}
	if active, err := worldNetworkActiveClientIds(ctx, outsider.PersonalNetworkId); err != nil || len(active) != 0 {
		t.Errorf("the outsider's network holds %d active clients after a refused enrollment (%v)", len(active), err)
	}
}

// The saves phase (the file's header).
func (self *controlPlaneCase) saves() {
	t := self.t
	settings := self.settings
	world := self.world
	ctx, cancel := world.StepContext(t.Context(), settings.PhaseTimeout)
	defer cancel()
	a := self.personal[0]
	team := self.team

	// a right §8.5's implicit document gives: a's owner device creates a general repo and writes a file, and another
	// device of the owner's, no grant naming it, reads it as every member device does with no document
	repo, err := a.CreateRepo(ctx, a.Owner(), protocol.RepoKind_REPO_KIND_GENERAL)
	if err != nil {
		world.FailProgress(t, "a's general repo", err, nil)
	}
	repoId := repo.RepoId()
	content := []byte("implicit-reader")
	if err := worldKeysWrite(ctx, a.Owner(), repoId, "implicit.txt", content); err != nil {
		world.FailProgress(t, "a's write under the implicit document", err, nil)
	}
	reader := a.Devices()[len(a.Devices())-1]
	if err := self.awaitRead(ctx, reader, repoId, "implicit.txt", content); err != nil {
		t.Errorf("%s never read a's general repo under the implicit document: %v", reader.Name, err)
	}

	// the races, both networks at once
	racersOf := func(devices []*sidecart3.T3Device) []*sidecart3.T3Device {
		racers := []*sidecart3.T3Device{}
		for _, device := range devices {
			if self.kind(device).configWriter && len(racers) < settings.RacerCount {
				racers = append(racers, device)
			}
		}
		return racers
	}
	type raced struct {
		network *sidecart3.T3Network
		rounds  []*controlPlaneRaceRound
		err     error
	}
	aRacers := racersOf(a.LiveDevices())
	teamRacers := racersOf(append(team.Owner.Devices(), self.admin.Devices()...))
	races := []*raced{{network: a.T3Network}, {network: team.T3Network}}
	var group sync.WaitGroup
	raceStart := time.Now()
	group.Go(func() {
		races[0].rounds, races[0].err = controlPlaneRace(ctx, world, a.T3Network, aRacers, func(index int) string {
			return worldAclPolicy(settings.GrantOne, settings.RaceGrant(index))
		}, settings.Retry)
	})
	group.Go(func() {
		races[1].rounds, races[1].err = controlPlaneRace(ctx, world, team.T3Network, teamRacers, func(index int) string {
			return team.AclPolicy(settings.GrantOne, settings.RaceGrant(index))
		}, settings.Retry)
	})
	group.Wait()
	for _, race := range races {
		if race.err != nil {
			t.Errorf("the races on %s: %v", race.network.Name, race.err)
		}
		landed := 0
		for _, round := range race.rounds {
			landed += len(round.winners)
			for name, refusal := range round.refusals {
				if !controlPlaneHeadMoved(refusal) {
					t.Errorf("%s at config version %d: %s's save was refused other than as the head having moved: %v", race.network.Name, round.configVersion, name, refusal)
				}
			}
			if round.wonVersion <= round.configVersion {
				t.Errorf("%s: a round won at config version %d left it at %d", race.network.Name, round.configVersion, round.wonVersion)
			}
		}
		racerCount := len(aRacers)
		if race.network == team.T3Network {
			racerCount = len(teamRacers)
		}
		if landed != racerCount {
			t.Errorf("%s's races landed %d documents for %d racers", race.network.Name, landed, racerCount)
		}
		t.Logf("%s's races: %d rounds in %s", race.network.Name, len(race.rounds), time.Since(raceStart))
	}

	// the right the first winning document took away: refused to its holder once evs's gate holds the base, and at the
	// token exchange for gkm, whose token for the repo's keys the holder no longer takes
	if err := self.awaitReadRefused(ctx, reader, repoId, "implicit.txt"); err != nil {
		t.Errorf("%s still reads a's general repo after a document holding grant 1 alone landed: %v", reader.Name, err)
	} else {
		t.Logf("the saves: %s's implicit read of a's general repo is refused once the races' bases landed", reader.Name)
	}
	repoRead := []string{controlPlaneRepoScope(repoId, "r")}
	if granted, err := controlPlaneGrantedScopes(ctx, world, reader, WorldBlockGkm, repoRead); err == nil {
		t.Errorf("%s still takes a gkm token holding %v of a's general repo after a document holding grant 1 alone landed", reader.Name, granted)
	} else if _, code := controlPlaneApiRefusal(err); code != "invalid_scope" {
		t.Errorf("%s's gkm exchange for a's general repo after the races was refused %v, not invalid_scope", reader.Name, err)
	}

	// every device of each network on one pin
	for _, network := range []*sidecart3.T3Network{a.T3Network, team.T3Network} {
		if divergent := controlPlaneDivergent(network.LiveDevices()); 0 < len(divergent) {
			t.Errorf("%s's devices did not converge: %s", network.Name, strings.Join(divergent, "; "))
		}
	}
}

// Waits until device reads repoId's path as content, its key delivered by a holder, within ReadSettleTimeout.
func (self *controlPlaneCase) awaitRead(ctx context.Context, device *sidecart3.T3Device, repoId sidecar.Id, path string, content []byte) error {
	ctx, cancel := context.WithTimeout(ctx, self.settings.ReadSettleTimeout)
	defer cancel()
	for {
		err := worldKeysRead(ctx, device, repoId, path, content)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (%w)", err, ctx.Err())
		case <-time.After(self.settings.ReadSettleInterval):
		}
	}
}

// Asks device to read repoId's path until a read is refused, within GateBound from the first ask: what a right a base
// took away must come to once the gates hold the base (§8.9; D-ACL-21: evs honors r only for current readers, never a
// token's 15 minutes). The refusal, logged as the evidence; an error when every read in the bound answered.
func (self *controlPlaneCase) awaitReadRefused(ctx context.Context, device *sidecart3.T3Device, repoId sidecar.Id, path string) error {
	deadline := time.Now().Add(self.settings.GateBound)
	for {
		client, err := device.EvsClient()
		if err != nil {
			return err
		}
		var sink strings.Builder
		if _, err := client.Repo(repoId).ReadHead(ctx, &sidecar.EvsReadArgs{Path: path, Fresh: true}, &sink); err != nil {
			// evs's gate refusing the read for want of the right, never another failure that only looks like one
			if !sidecar.IsEvsRefusal(err, protocol.EvsRefusalCode_EVS_REFUSAL_SCOPE) {
				return fmt.Errorf("the read failed other than by evs's scope refusal: %w", err)
			}
			self.t.Logf("%s's read of %s in %s is refused: %v", device.Name, path, repoId, err)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the read still answered %d bytes past §8.9's %s", sink.Len(), self.settings.GateBound)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(self.settings.ReadSettleInterval):
		}
	}
}

// The churn phase (the file's header).
func (self *controlPlaneCase) churn() {
	t := self.t
	settings := self.settings
	world := self.world
	ctx, cancel := world.StepContext(t.Context(), settings.PhaseTimeout)
	defer cancel()
	team := self.team
	b := self.personal[1]
	ownerDevice := team.Owner.Devices()[0]
	adminDevices := self.admin.Devices()

	// the Owner's vault, owner-bound to the Owner's member, which an EvsShare opens to the Admin's first device
	vaultId, err := self.createVault(ctx, ownerDevice, team.Owner.MemberId)
	if err != nil {
		world.FailProgress(t, "the Owner's vault", err, nil)
	}
	vaultContent := []byte("owner-vault")
	if err := worldKeysWrite(ctx, ownerDevice, vaultId, "vault.txt", vaultContent); err != nil {
		world.FailProgress(t, "the vault's write", err, nil)
	}

	var membersLock sync.Mutex
	joined := []*WorldTeamMember{}
	removed := []*WorldTeamMember{}
	revoked := []*sidecart3.T3Device{}
	keepJoined := func(member *WorldTeamMember) {
		membersLock.Lock()
		defer membersLock.Unlock()
		joined = append(joined, member)
	}
	inviters := []*WorldTeamMember{team.Owner, self.admin}
	inviterDevices := []*sidecart3.T3Device{ownerDevice, adminDevices[0]}
	bursts := []*WorldBurst{}
	for index := range settings.ChurnInviteCount {
		var person *WorldPerson
		var invite *WorldTeamInvite
		var member *WorldTeamMember
		inviter, inviterDevice := inviters[index%len(inviters)], inviterDevices[index%len(inviters)]
		steps := []func(ctx context.Context) error{
			func(ctx context.Context) error {
				var err error
				person, err = world.NewPerson(ctx, fmt.Sprintf("churn-%d", index))
				return err
			},
			func(ctx context.Context) error {
				var err error
				invite, err = team.Invite(ctx, inviter, inviterDevice, person.Email, bridge.MemberRole_MEMBER_ROLE_MEMBER)
				return err
			},
			func(ctx context.Context) error {
				var err error
				member, err = team.Accept(ctx, person, invite)
				return err
			},
			func(ctx context.Context) error {
				if _, err := team.AdmitMember(ctx, inviterDevice, member); err != nil {
					return err
				}
				keepJoined(member)
				return nil
			},
		}
		bursts = append(bursts, controlPlaneBurst(fmt.Sprintf("invite %d", index), steps))
	}
	// a setup member removed, its devices with it, then re-invited, rejoining under a new member_id
	leaving := self.plain[len(self.plain)-1]
	leavingDevices := leaving.Devices()
	var rejoined *WorldTeamMember
	var reinvite *WorldTeamInvite
	bursts = append(bursts, controlPlaneBurst("remove and re-invite", []func(ctx context.Context) error{
		func(ctx context.Context) error {
			if _, err := team.RemoveMember(ctx, team.Owner, ownerDevice, leaving); err != nil {
				return err
			}
			membersLock.Lock()
			defer membersLock.Unlock()
			removed = append(removed, leaving)
			return nil
		},
		func(ctx context.Context) error {
			var err error
			reinvite, err = team.Invite(ctx, self.admin, adminDevices[0], leaving.Email, bridge.MemberRole_MEMBER_ROLE_MEMBER)
			return err
		},
		func(ctx context.Context) error {
			var err error
			rejoined, err = team.Accept(ctx, leaving.WorldPerson, reinvite)
			return err
		},
		func(ctx context.Context) error {
			if rejoined.MemberId == leaving.MemberId {
				return fmt.Errorf("%s rejoined under its old member_id %s", leaving.Name, leaving.MemberId)
			}
			if _, err := team.AdmitMember(ctx, adminDevices[0], rejoined); err != nil {
				return err
			}
			keepJoined(rejoined)
			return nil
		},
	}))
	// the Admin's last device revoked
	lastAdminDevice := adminDevices[len(adminDevices)-1]
	bursts = append(bursts, controlPlaneBurst("revoke the Admin's last device", []func(ctx context.Context) error{
		func(ctx context.Context) error {
			if _, err := team.RevokeDevice(ctx, ownerDevice, lastAdminDevice, bridge.SidecarRevokeReason_SIDECAR_REVOKE_REASON_REMOVED); err != nil {
				return err
			}
			membersLock.Lock()
			defer membersLock.Unlock()
			revoked = append(revoked, lastAdminDevice)
			return nil
		},
	}))
	// the vault's share to the Admin's first device, then its revocation: the exchange grants the subject the vault's
	// read while the share stands and refuses it once revoked, and the subject takes the vault's key while it holds it
	sharedTo := adminDevices[0]
	vaultRead := []string{controlPlaneRepoScope(vaultId, "r")}
	bursts = append(bursts, controlPlaneBurst("share and revoke", []func(ctx context.Context) error{
		func(ctx context.Context) error {
			_, err := team.FileStatement(ctx, ownerDevice, bridge.SignedType_SIGNED_EVS_SHARE, &protocol.EvsShare{
				RepoId:  vaultId.Bytes(),
				Subject: sharedTo.Sidecar().Identity().SidecarId.Bytes(),
			})
			return err
		},
		func(ctx context.Context) error {
			if _, err := self.exchange.grant(ctx, sharedTo, WorldBlockEvs, vaultRead); err != nil {
				return fmt.Errorf("the exchange refused %s the shared vault's read: %w", sharedTo.Name, err)
			}
			return self.awaitGeneration(ctx, sharedTo, vaultId, 1)
		},
		func(ctx context.Context) error {
			_, err := team.FileStatement(ctx, ownerDevice, bridge.SignedType_SIGNED_REPO_REVOKE, &protocol.RepoRevoke{
				RepoId:  vaultId.Bytes(),
				Subject: sharedTo.Sidecar().Identity().SidecarId.Bytes(),
			})
			return err
		},
		func(ctx context.Context) error {
			if grant, err := self.exchange.grant(ctx, sharedTo, WorldBlockEvs, vaultRead); err == nil {
				return fmt.Errorf("the exchange still grants %s %v of the vault after the share's revocation", sharedTo.Name, grant.Scopes)
			}
			return nil
		},
	}))
	// b's tagged sidecars admitted, one revoked
	var taggedChurn []*sidecart3.T3Device
	bursts = append(bursts, controlPlaneBurst("b's tagged churn", []func(ctx context.Context) error{
		func(ctx context.Context) error {
			var err error
			taggedChurn, err = b.AddSidecarsWith(ctx, &sidecart3.T3EnrollArgs{Count: settings.TaggedChurnCount, Tags: []string{controlPlaneTagHex(self.tagId)}, Name: b.Name + "-churn"})
			if err != nil {
				return err
			}
			self.setKind(controlPlaneDeviceKind{}, taggedChurn...)
			for _, device := range taggedChurn {
				if err := self.admitTagged(ctx, b, device); err != nil {
					return err
				}
			}
			return b.Follow(ctx, taggedChurn)
		},
		func(ctx context.Context) error {
			device := taggedChurn[0]
			b.Retire(device)
			if _, err := b.FileStatement(ctx, b.Owner(), bridge.SignedType_SIGNED_SIDECAR_REVOKE, &bridge.SidecarRevoke{
				SidecarId: device.Sidecar().Identity().SidecarId.Bytes(),
				Reason:    bridge.SidecarRevokeReason_SIDECAR_REVOKE_REASON_REMOVED,
			}); err != nil {
				return err
			}
			membersLock.Lock()
			defer membersLock.Unlock()
			revoked = append(revoked, device)
			return nil
		},
	}))
	if err := RunBursts(ctx, t, "the churn", bursts, settings.Bursts); err != nil {
		world.FailProgress(t, "the churn", err, bursts)
	}
	FailBurstErrors(t, bursts)

	// every live device, api's rows and the exchange agree
	if err := self.syncTip(ctx, team.T3Network, ownerDevice); err != nil {
		world.FailProgress(t, "the team's convergence after the churn", err, nil)
	}
	expected := map[sidecar.Id]string{team.Owner.MemberId: team.Owner.Name, self.admin.MemberId: self.admin.Name}
	for _, member := range self.plain {
		if !member.Removed() {
			expected[member.MemberId] = member.Name
		}
	}
	for _, member := range joined {
		expected[member.MemberId] = member.Name
	}
	t.Logf("the churn: %d members joined, %d removed, %d devices revoked; %d members expected at n %d", len(joined), len(removed), len(revoked), len(expected), ownerDevice.Follower().Client().Pin().GetVersion())
	self.checkMembers(ctx, expected, removed)
	for _, device := range revoked {
		self.checkExchangeRefused(ctx, device, "revoked")
	}
	// the removed member's devices: revoked in every live device's state by its MemberRevoke, deactivated at api by its
	// removal there, and refused at the exchange
	if leaving.Removed() {
		self.checkMemberDevicesRemoved(ctx, leaving, leavingDevices)
	}
	// the vault's next generation: the owner takes it, the share's former subject does not
	self.checkRotationOf(ctx, team.T3Network, ownerDevice, vaultId, []*sidecart3.T3Device{ownerDevice}, []*sidecart3.T3Device{sharedTo})
	// the next rotation of the config repo: every live device takes it, the team's tagged sidecars among them, and the
	// revoked Admin device and the removed member's devices do not
	self.checkRotation(ctx, team.T3Network, ownerDevice, slices.Concat([]*sidecart3.T3Device{lastAdminDevice}, leavingDevices))

	// the Admin moved out of the AdminSet and removed: its devices lose the exchange, their clients and the next
	// generation, and the tagged sidecars it sponsored outlive it (owner decision 83, QD-1194)
	if _, err := team.SetAdmins(ctx, ownerDevice, nil); err != nil {
		t.Errorf("the AdminSet without the Admin: %v", err)
		return
	}
	if err := team.AwaitServerRole(ctx, self.admin, model.NetworkMemberRoleMember); err != nil {
		t.Errorf("the Admin's mirrored role after the AdminSet: %v", err)
	}
	if _, err := team.RemoveMember(ctx, team.Owner, ownerDevice, self.admin); err != nil {
		t.Errorf("the Admin's removal: %v", err)
		return
	}
	adminLive := slices.DeleteFunc(slices.Clone(adminDevices), func(device *sidecart3.T3Device) bool { return device == lastAdminDevice })
	self.checkMemberDevicesRemoved(ctx, self.admin, adminLive)
	for _, device := range self.teamTagged {
		self.checkTaggedAgent(ctx, device, "past its sponsor's removal")
	}
	self.checkRotation(ctx, team.T3Network, ownerDevice, adminLive)
}

// A burst of steps run in order, each its own error.
func controlPlaneBurst(name string, steps []func(ctx context.Context) error) *WorldBurst {
	return &WorldBurst{Name: name, StepCount: len(steps), Step: func(ctx context.Context, index int, random *rand.Rand) error {
		return steps[index](ctx)
	}}
}

// A vault repo owner-bound to memberId, created through evs by device, that member's consented device (§8.4's kinds).
func (self *controlPlaneCase) createVault(ctx context.Context, device *sidecart3.T3Device, memberId sidecar.Id) (sidecar.Id, error) {
	client, err := device.EvsClient()
	if err != nil {
		return sidecar.Id{}, err
	}
	owner := memberId
	repo, err := client.CreateRepo(ctx, &sidecar.EvsCreateRepoArgs{Kind: protocol.RepoKind_REPO_KIND_VAULT, OwnerMemberId: &owner, Confirmed: true})
	if err != nil {
		return sidecar.Id{}, fmt.Errorf("%s's vault: %w", device.Name, err)
	}
	return repo.RepoId(), nil
}

// Every live team device's state names exactly expected's members, api's rows hold exactly them, and no removed member
// is named by either.
func (self *controlPlaneCase) checkMembers(ctx context.Context, expected map[sidecar.Id]string, removed []*WorldTeamMember) {
	t := self.t
	for _, device := range self.team.LiveDevices() {
		state := device.Follower().VerifiedState()
		for memberId, name := range expected {
			if state.Member(memberId) == nil {
				t.Errorf("%s's state names no member %s (%s)", device.Name, memberId, name)
			}
		}
		for _, memberId := range state.MemberIds() {
			if _, ok := expected[memberId]; !ok {
				t.Errorf("%s's state names a member %s the case did not keep", device.Name, memberId)
			}
		}
	}
	roles, err := worldNetworkMemberRoles(ctx, self.team.NetworkId)
	if err != nil {
		t.Errorf("api's member rows: %v", err)
		return
	}
	for memberId, name := range expected {
		if _, ok := roles[server.Id(memberId)]; !ok {
			t.Errorf("api holds no member row of %s (%s)", memberId, name)
		}
	}
	for memberId := range roles {
		if _, ok := expected[sidecar.Id(memberId)]; !ok {
			t.Errorf("api holds a member row %s the case did not keep", memberId)
		}
	}
	for _, member := range removed {
		if _, ok := roles[server.Id(member.MemberId)]; ok {
			t.Errorf("api still holds %s's removed membership %s", member.Name, member.MemberId)
		}
	}
}

// rotator rotates network's config repo, and every live device's key client holds the new generation within
// RotationSettleTimeout, while none of excluded does.
func (self *controlPlaneCase) checkRotation(ctx context.Context, network *sidecart3.T3Network, rotator *sidecart3.T3Device, excluded []*sidecart3.T3Device) {
	configRepoId, _, err := controlPlaneNetworkRepos(rotator)
	if err != nil {
		self.t.Errorf("%s's config repo: %v", network.Name, err)
		return
	}
	self.checkRotationOf(ctx, network, rotator, configRepoId, network.LiveDevices(), excluded)
}

// rotator rotates repoId, and each of holders holds the new generation within RotationSettleTimeout, while none of
// excluded does.
func (self *controlPlaneCase) checkRotationOf(ctx context.Context, network *sidecart3.T3Network, rotator *sidecart3.T3Device, repoId sidecar.Id, holders []*sidecart3.T3Device, excluded []*sidecart3.T3Device) {
	t := self.t
	key, err := rotator.Gkm.RotateRepo(ctx, repoId)
	if err != nil {
		t.Errorf("%s's rotation of %s's repo %s: %v", rotator.Name, network.Name, repoId, err)
		return
	}
	generation := key.Generation()
	for _, device := range holders {
		if err := self.awaitGeneration(ctx, device, repoId, generation); err != nil {
			t.Errorf("%s never held %s's repo %s at generation %d after the rotation: %v", device.Name, network.Name, repoId, generation, err)
		}
	}
	for _, device := range excluded {
		held, err := device.Gkm.CurrentRepoKey(ctx, repoId)
		if err == nil && generation <= held.Generation() {
			t.Errorf("%s, revoked or removed, holds %s's repo %s at generation %d, the rotation's after its removal", device.Name, network.Name, repoId, held.Generation())
		}
	}
}

// Waits until device's key client holds repoId at generation or past it, within RotationSettleTimeout.
func (self *controlPlaneCase) awaitGeneration(ctx context.Context, device *sidecart3.T3Device, repoId sidecar.Id, generation uint64) error {
	ctx, cancel := context.WithTimeout(ctx, self.settings.RotationSettleTimeout)
	defer cancel()
	for {
		key, err := device.Gkm.CurrentRepoKey(ctx, repoId)
		if err == nil && generation <= key.Generation() {
			return nil
		}
		select {
		case <-ctx.Done():
			held := uint64(0)
			if key != nil {
				held = key.Generation()
			}
			return fmt.Errorf("holds generation %d: %v (%w)", held, err, ctx.Err())
		case <-time.After(self.settings.RotationSettleInterval):
		}
	}
}

// Catches device's follower up to the log's tip, then syncs network there: every gate and live device at the tip.
func (self *controlPlaneCase) syncTip(ctx context.Context, network *sidecart3.T3Network, device *sidecart3.T3Device) error {
	client := device.Follower().Client()
	if client == nil {
		return fmt.Errorf("%s's follower no longer stands", device.Name)
	}
	if err := client.CatchUp(ctx); err != nil {
		return err
	}
	return network.Sync(ctx, client.Pin().GetVersion())
}

// The limits phase (the file's header).
func (self *controlPlaneCase) limits() {
	t := self.t
	settings := self.settings
	world := self.world
	ctx, cancel := world.StepContext(t.Context(), settings.PhaseTimeout)
	defer cancel()
	b := self.personal[1]
	if err := self.syncTip(ctx, b.T3Network, b.Owner()); err != nil {
		world.FailProgress(t, "b at its tip", err, nil)
	}
	admitted := worldConfigAdmissionCount(b.Owner().Follower().VerifiedState(), time.Now())
	limit := admitted + int64(settings.AdmissionHeadroom)
	pop := worldSetFreeLimit("config_admissions", limit)
	defer pop()
	devices, err := b.AddSidecarsWith(ctx, &sidecart3.T3EnrollArgs{Count: settings.AdmissionHeadroom + settings.AdmissionOverflow, Tags: []string{controlPlaneTagHex(self.tagId)}, Name: b.Name + "-cap"})
	if err != nil {
		world.FailProgress(t, "b's devices for the admission cap", err, nil)
	}
	errs := make([]error, len(devices))
	var group sync.WaitGroup
	for index, device := range devices {
		group.Go(func() {
			errs[index] = self.admitTagged(ctx, b, device)
		})
	}
	group.Wait()
	landed, capped := 0, 0
	for index, err := range errs {
		switch {
		case err == nil:
			landed += 1
		case strings.Contains(err.Error(), "config_admissions"):
			capped += 1
		default:
			t.Errorf("%s's admission was refused other than by config_admissions: %v", devices[index].Name, err)
		}
	}
	t.Logf("the limits: config_admissions %d with %d admitted, %d admissions landed and %d were refused by the limit", limit, admitted, landed, capped)
	if landed != settings.AdmissionHeadroom || capped != settings.AdmissionOverflow {
		t.Errorf("config_admissions %d with %d admitted: %d of %d admissions landed and %d were refused by the limit, not %d and %d", limit, admitted, landed, len(devices), capped, settings.AdmissionHeadroom, settings.AdmissionOverflow)
	}
	for _, device := range devices {
		b.Retire(device)
	}
	if err := self.syncTip(ctx, b.T3Network, b.Owner()); err != nil {
		t.Errorf("b at its tip after the admissions: %v", err)
		return
	}
	if now := worldConfigAdmissionCount(b.Owner().Follower().VerifiedState(), time.Now()); now != limit {
		t.Errorf("b's config holds %d admissions, not its config_admissions %d", now, limit)
	}
}
