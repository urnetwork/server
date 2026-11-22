//go:build sidecar_pending

package model

// SmtpApiGateOf (smtp_api_gate_model.go) over hand-made verified states (sidecartest's FakeAclContractStates) on a
// private database: mail_send only for the sidecar the rows grant it, refused once §21 suspends the network, and
// unavailable for a network the cache has not loaded, which asks for its load, or one Postgres does not hold; the
// reports' read only for the readers of the network mail key's repo, a Team's admin inbox or a personal network's
// Owner's mailbox, for no one where the rows name no such repo, unavailable while not loaded, and kept through a
// suspension. It needs MG5's network_abuse_state (sidecar_pending), so it runs through .stack/server-test.sh.

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar"
	sidecarprotocol "github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

// sidecartest's FakeAclView with the Genesis §7.9's bootstrap leaves: its owner member and that member's first device.
type testSmtpGateView struct {
	*sidecartest.FakeAclView
	genesis *bridge.Genesis
}

func (self *testSmtpGateView) Genesis() *bridge.Genesis {
	return self.genesis
}

// A network Postgres holds, created now.
func testSmtpGateNetwork(t testing.TB, ctx context.Context) server.Id {
	t.Helper()
	networkId := server.NewId()
	Testing_CreateNetwork(ctx, networkId, "smtpgate"+strings.ReplaceAll(networkId.String(), "-", "")[:12], server.NewId())
	return networkId
}

// Suspends networkId as §21.4's suspend leaves network_abuse_state.
func testSmtpGateSuspend(t testing.TB, ctx context.Context, networkId server.Id) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_abuse_state (network_id, state, case_id, until_time, update_time)
				VALUES ($1, $2, $3, NULL, $4)
				ON CONFLICT (network_id) DO UPDATE SET state = EXCLUDED.state, case_id = EXCLUDED.case_id, until_time = NULL
			`,
			networkId,
			networkAbuseStateSuspended,
			server.NewId(),
			server.NowUtc(),
		))
	})
}

// A rule of the rows: src's derived sidecar atom given actions on targets.
func testSmtpGateRule(src sidecar.Id, targets []*sidecarprotocol.AclTarget, actions ...sidecarprotocol.AclAction) *sidecarprotocol.AclRule {
	rule := &sidecarprotocol.AclRule{
		RuleId:   sidecar.NewId().Bytes(),
		SrcAtoms: [][]byte{sidecartest.FakeAclSidecarAtom(src).GetAtom()},
		Targets:  targets,
	}
	for _, action := range actions {
		rule.Actions = append(rule.Actions, uint32(action))
	}
	return rule
}

func TestSmtpApiGateMailSendIsTheRowsGrantUnderTheStanding(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		networkId := testSmtpGateNetwork(t, ctx)
		view := sidecartest.NewFakeAclView()
		member := view.AddMember(bridge.MemberRole_MEMBER_ROLE_OWNER)
		sender, other := view.AddDevice(member), view.AddDevice(member)
		rows := &sidecarprotocol.AclRows{
			HasDocument: true,
			Atoms:       []*sidecarprotocol.AclAtom{sidecartest.FakeAclSidecarAtom(sender), sidecartest.FakeAclSidecarAtom(other)},
			Rules: []*sidecarprotocol.AclRule{
				testSmtpGateRule(sender, []*sidecarprotocol.AclTarget{{MailSend: true}}, sidecarprotocol.AclAction_ACL_ACTION_SEND),
				// a send grant on anything but mail_send sends no mail
				testSmtpGateRule(other, []*sidecarprotocol.AclTarget{{PublicName: "smtp/25/elsewhere"}}, sidecarprotocol.AclAction_ACL_ACTION_SEND),
			},
		}
		states := sidecartest.NewFakeAclContractStates()
		if err := states.Install(sidecar.Id(networkId), rows, view); err != nil {
			t.Fatal(err)
		}
		gate := SmtpApiGateOf(states)

		if allowed, err := gate.MaySendMail(ctx, networkId, sender, now); !allowed || err != nil {
			t.Fatalf("the sender: %t, %v", allowed, err)
		}
		if allowed, err := gate.MaySendMail(ctx, networkId, other, now); allowed || err != nil {
			t.Fatalf("a sidecar with no mail_send: %t, %v", allowed, err)
		}
		if allowed, err := gate.MaySendMail(ctx, networkId, server.NewId(), now); allowed || err != nil {
			t.Fatalf("a sidecar the state does not hold: %t, %v", allowed, err)
		}

		// a network the cache has not loaded answers unavailable and asks for its load
		unloaded := testSmtpGateNetwork(t, ctx)
		if allowed, err := gate.MaySendMail(ctx, unloaded, sender, now); allowed || !errors.Is(err, ErrSmtpApiUnavailable) {
			t.Fatalf("an unloaded network: %t, %v", allowed, err)
		}
		if !slices.Contains(states.ChangedNetworks(), sidecar.Id(unloaded)) {
			t.Fatal("an unloaded network was not asked for")
		}
		// a loaded state for a network Postgres does not hold has no standing to read
		stranger := server.NewId()
		if err := states.Install(sidecar.Id(stranger), rows, view); err != nil {
			t.Fatal(err)
		}
		if allowed, err := gate.MaySendMail(ctx, stranger, sender, now); allowed || !errors.Is(err, ErrSmtpApiUnavailable) {
			t.Fatalf("a network with no standing: %t, %v", allowed, err)
		}

		// §8.6's hard deny: a §21 suspension holds mail_send, the grant notwithstanding
		testSmtpGateSuspend(t, ctx, networkId)
		if allowed, err := gate.MaySendMail(ctx, networkId, sender, now); allowed || err != nil {
			t.Fatalf("the sender of a suspended network: %t, %v", allowed, err)
		}

		if allowed, err := SmtpApiGateOf(nil).MaySendMail(ctx, networkId, sender, now); allowed || !errors.Is(err, ErrSmtpApiUnavailable) {
			t.Fatalf("no gate cache: %t, %v", allowed, err)
		}
	})
}

func TestSmtpApiGateReportsReadIsTheMailKeyReposReaders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		read := sidecarprotocol.AclAction_ACL_ACTION_READ
		states := sidecartest.NewFakeAclContractStates()
		gate := SmtpApiGateOf(states)
		repoRow := func(kind sidecarprotocol.RepoKind, ownerId sidecar.Id, inbox sidecarprotocol.RepoInbox, at uint64, consented ...sidecar.Id) (sidecar.Id, *sidecarprotocol.AclRepo) {
			repoId := sidecar.NewId()
			creatorId := sidecar.NewId().Bytes()
			row := &sidecarprotocol.AclRepo{
				RepoId: repoId.Bytes(),
				Kind:   kind,
				Inbox:  inbox,
				Admins: []*sidecarprotocol.AclRepoAdminRow{{SidecarId: creatorId, Signer: creatorId, At: at}},
			}
			if ownerId != (sidecar.Id{}) {
				row.OwnerMemberId = ownerId.Bytes()
			}
			for _, sidecarId := range consented {
				row.ConsentedSidecarIds = append(row.ConsentedSidecarIds, sidecarId.Bytes())
			}
			return repoId, row
		}

		// a Team: a directory, and the admin inbox its readers read by a rule
		team := testSmtpGateNetwork(t, ctx)
		teamView := sidecartest.NewFakeAclView()
		admin := teamView.AddMember(bridge.MemberRole_MEMBER_ROLE_ADMIN)
		reader, outsider := teamView.AddDevice(admin), teamView.AddDevice(admin)
		_, directory := repoRow(sidecarprotocol.RepoKind_REPO_KIND_DIRECTORY, sidecar.Id{}, sidecarprotocol.RepoInbox_REPO_INBOX_NONE, 1)
		adminInbox, adminInboxRow := repoRow(sidecarprotocol.RepoKind_REPO_KIND_GENERAL, sidecar.Id{}, sidecarprotocol.RepoInbox_REPO_INBOX_ADMIN, 2)
		groupInbox, groupInboxRow := repoRow(sidecarprotocol.RepoKind_REPO_KIND_GENERAL, sidecar.Id{}, sidecarprotocol.RepoInbox_REPO_INBOX_GROUP, 3)
		teamRows := &sidecarprotocol.AclRows{
			HasDocument: true,
			Atoms:       []*sidecarprotocol.AclAtom{sidecartest.FakeAclSidecarAtom(reader), sidecartest.FakeAclSidecarAtom(outsider)},
			Rules: []*sidecarprotocol.AclRule{
				testSmtpGateRule(reader, []*sidecarprotocol.AclTarget{{RepoId: adminInbox.Bytes()}}, read),
				// a reader of a group inbox reads no report of the network's
				testSmtpGateRule(outsider, []*sidecarprotocol.AclTarget{{RepoId: groupInbox.Bytes()}}, read),
			},
			Repos: []*sidecarprotocol.AclRepo{directory, adminInboxRow, groupInboxRow},
		}
		if err := states.Install(sidecar.Id(team), teamRows, teamView); err != nil {
			t.Fatal(err)
		}
		if allowed, err := gate.MayReadReports(ctx, team, reader, now); !allowed || err != nil {
			t.Fatalf("the admin inbox's reader: %t, %v", allowed, err)
		}
		if allowed, err := gate.MayReadReports(ctx, team, outsider, now); allowed || err != nil {
			t.Fatalf("a group inbox's reader: %t, %v", allowed, err)
		}
		// a suspension never reaches the network's own reports
		testSmtpGateSuspend(t, ctx, team)
		if allowed, err := gate.MayReadReports(ctx, team, reader, now); !allowed || err != nil {
			t.Fatalf("the reader of a suspended network: %t, %v", allowed, err)
		}
		// a Team whose rows name no admin inbox gives no one the copies
		teamRows.Repos = []*sidecarprotocol.AclRepo{directory, groupInboxRow}
		teamRows.Rules = teamRows.Rules[1:]
		if err := states.Install(sidecar.Id(team), teamRows, teamView); err != nil {
			t.Fatal(err)
		}
		if allowed, err := gate.MayReadReports(ctx, team, outsider, now); allowed || err != nil {
			t.Fatalf("a Team with no admin inbox: %t, %v", allowed, err)
		}

		// a personal network: its Owner's mailbox, which the Owner's consented device reads
		personal := testSmtpGateNetwork(t, ctx)
		fakeView := sidecartest.NewFakeAclView()
		owner := fakeView.AddMember(bridge.MemberRole_MEMBER_ROLE_OWNER)
		ownerDevice, unconsented := fakeView.AddDevice(owner), fakeView.AddDevice(owner)
		personalView := &testSmtpGateView{
			FakeAclView: fakeView,
			genesis:     &bridge.Genesis{OwnerMemberId: owner.Bytes(), OwnerSidecarId: ownerDevice.Bytes()},
		}
		_, mailbox := repoRow(sidecarprotocol.RepoKind_REPO_KIND_MAILBOX, owner, sidecarprotocol.RepoInbox_REPO_INBOX_NONE, 2, ownerDevice)
		personalRows := &sidecarprotocol.AclRows{HasDocument: true, Repos: []*sidecarprotocol.AclRepo{mailbox}}
		if err := states.Install(sidecar.Id(personal), personalRows, personalView); err != nil {
			t.Fatal(err)
		}
		if allowed, err := gate.MayReadReports(ctx, personal, ownerDevice, now); !allowed || err != nil {
			t.Fatalf("the Owner's consented device: %t, %v", allowed, err)
		}
		// the Owner's device with no consent deposits there and reads nothing
		if allowed, err := gate.MayReadReports(ctx, personal, unconsented, now); allowed || err != nil {
			t.Fatalf("the Owner's unconsented device: %t, %v", allowed, err)
		}

		// a network the cache has not loaded answers unavailable and asks for its load
		unloaded := testSmtpGateNetwork(t, ctx)
		if allowed, err := gate.MayReadReports(ctx, unloaded, reader, now); allowed || !errors.Is(err, ErrSmtpApiUnavailable) {
			t.Fatalf("an unloaded network: %t, %v", allowed, err)
		}
		if !slices.Contains(states.ChangedNetworks(), sidecar.Id(unloaded)) {
			t.Fatal("an unloaded network was not asked for")
		}
		if allowed, err := SmtpApiGateOf(nil).MayReadReports(ctx, team, reader, now); allowed || !errors.Is(err, ErrSmtpApiUnavailable) {
			t.Fatalf("no gate cache: %t, %v", allowed, err)
		}
	})
}
