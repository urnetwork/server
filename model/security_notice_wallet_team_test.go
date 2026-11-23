//go:build sidecar_pending

package model

// A team's payout wallet and subnet wallet security notices reach the members whose role holds billing, its Owners and
// Billing admins (QD-1217; owner decision 2026-10-29, §7.5's role table, D-TEAM-2), on Postgres with MG0, MG1 and MG6.
// Every wallet notice used to go to the sign-in email addresses of the network's admin user, which in a team is the
// synthetic owner of record that team-create writes with no sign-in, so every payout and subnet wallet change of a team
// was owed to nobody, counted as no_email: no one in a team heard that where its payouts go had changed, the shape of
// the May 2026 incident in which an attacker's wallet replaced the payout wallet of 1,459 networks unnoticed. The
// members whose role holds billing are the ones who can change where a team's payouts go and the only ones the role
// table lets read it, so they are the ones told; an Admin, which holds no billing, is not. Each team here is made by
// team-create, with the people and teams of the key notices' tests (security_notice_team_recipients_test.go).

import (
	"context"
	"slices"
	"testing"

	"github.com/urnetwork/server"
)

// A team made by team-create with a person in every role, and a pending Billing admin, each signing in at an email
// address of its own, its first Owner at two.
type walletNoticeTeam struct {
	*keyNoticeTeam
	owner        *keyNoticePerson
	billingAdmin *keyNoticePerson
	// the sign-in email addresses of its two Owners and its Billing admin, sorted: the people whose role holds billing
	recipients []string
}

func newWalletNoticeTeam(t testing.TB, ctx context.Context) *walletNoticeTeam {
	t.Helper()
	owner := newKeyNoticePerson(t, ctx, newKeyNoticeEmail("owner"))
	owner.addGoogleSignIn(t, ctx, newKeyNoticeEmail("owner-google"))
	team := newKeyNoticeTeam(t, ctx, owner)
	secondOwner := newKeyNoticePerson(t, ctx, newKeyNoticeEmail("second-owner"))
	team.join(t, secondOwner, NetworkMemberRoleOwner)
	billingAdmin := newKeyNoticePerson(t, ctx, newKeyNoticeEmail("billing-admin"))
	team.join(t, billingAdmin, NetworkMemberRoleBillingAdmin)
	for _, c := range []struct {
		label string
		role  NetworkMemberRole
	}{
		{label: "admin", role: NetworkMemberRoleAdmin},
		{label: "network-admin", role: NetworkMemberRoleNetworkAdmin},
		{label: "it-admin", role: NetworkMemberRoleItAdmin},
		{label: "auditor", role: NetworkMemberRoleAuditor},
		{label: "member", role: NetworkMemberRoleMember},
	} {
		team.join(t, newKeyNoticePerson(t, ctx, newKeyNoticeEmail(c.label)), c.role)
	}
	pendingBillingAdmin := newKeyNoticePerson(t, ctx, newKeyNoticeEmail("pending-billing-admin"))
	addTestSignInMember(t, ctx, team.networkId, pendingBillingAdmin.userId, NetworkMemberRoleBillingAdmin, true)
	return &walletNoticeTeam{
		keyNoticeTeam: team,
		owner:         owner,
		billingAdmin:  billingAdmin,
		recipients:    keyNoticeEmailsOf(owner, secondOwner, billingAdmin),
	}
}

// One change a wallet notice lists, as a test expects it.
type walletNoticeChange struct {
	// one of SecurityWalletDestination*
	destination string
	// the masked wallet the change set, "" for none
	newWallet string
}

// Holds that the network owes the payout wallet notice to each of recipients once and to nobody else, each notice due
// and listing the changes in order; then settles them, so the next change owes its own.
func requireWalletNoticeRecipients(
	t testing.TB,
	ctx context.Context,
	networkId server.Id,
	recipients []string,
	changes ...walletNoticeChange,
) {
	t.Helper()
	got := []string{}
	for _, notice := range unsentTestNotices(t, ctx, networkId, testPayoutWalletNoticeTemplate) {
		got = append(got, notice.userAuth)
		if notice.deliverTime == nil {
			t.Fatalf("the wallet notice to %s is held, want due", notice.userAuth)
		}
		listed := notice.changes(t)
		if len(listed) != len(changes) {
			t.Fatalf("the wallet notice to %s lists %d changes, want %d: %s", notice.userAuth, len(listed), len(changes), notice.templateJson)
		}
		for i, change := range changes {
			newWallet, _ := listed[i]["new_wallet"].(string)
			if listed[i]["destination"] != change.destination || newWallet != change.newWallet {
				t.Fatalf("the wallet notice to %s lists %v set to %q, want %s set to %q: %s", notice.userAuth, listed[i]["destination"], newWallet, change.destination, change.newWallet, notice.templateJson)
			}
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, recipients) {
		t.Fatalf("the wallet notice is owed to %v, want one each to %v", got, recipients)
	}
	settleTestNotices(ctx, networkId)
}

// Holds that the network owes no payout wallet notice.
func requireNoWalletNotice(t testing.TB, ctx context.Context, networkId server.Id, what string) {
	t.Helper()
	if notices := unsentTestNotices(t, ctx, networkId, testPayoutWalletNoticeTemplate); len(notices) != 0 {
		t.Fatalf("%s owed %d wallet notices, want none: %s", what, len(notices), notices[0].templateJson)
	}
}

// A team's payout wallet notices reach each of its Owners and Billing admins, at each of their sign-in email addresses,
// and nobody else: a wallet its Billing admin adds and selects as the payout wallet, another selected in its place, and
// the removal of the payout wallet, which promotes the first again, each owe one notice to each, listing the change, and
// none to its Admin, Network admin, IT admin, Auditor, Member or pending Billing admin, or to the owner of record, the
// one recipient a team's wallet changes used to owe, which has no address. Once the Billing admin is removed, the next
// change, the last wallet's removal, reaches the Owners alone. None is counted as no_email.
func TestTeamPayoutWalletNoticesReachItsOwnersAndBillingAdmins(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		team := newWalletNoticeTeam(t, ctx)
		noEmailBefore := securityNoticeCount(SecurityNoticePayoutWallet, securityNoticeNoEmail)

		createWallet := func(address string) server.Id {
			walletId := CreateAccountWalletExternal(team.billingAdmin.teamSession, &CreateAccountWalletExternalArgs{
				NetworkId:        team.networkId,
				Blockchain:       SOL.String(),
				WalletAddress:    address,
				DefaultTokenType: "USDC",
			})
			if walletId == nil {
				t.Fatal("the team's wallet was not created")
			}
			return *walletId
		}
		addressA, addressB := testSolanaAddress(t), testSolanaAddress(t)
		walletA, walletB := createWallet(addressA), createWallet(addressB)
		requireNoWalletNotice(t, ctx, team.networkId, "adding wallets that are not the payout wallet")

		if err := SetPayoutWallet(team.billingAdmin.teamSession.Ctx, team.networkId, walletA); err != nil {
			t.Fatal(err)
		}
		requireWalletNoticeRecipients(t, ctx, team.networkId, team.recipients, walletNoticeChange{
			destination: SecurityWalletDestinationUsdc,
			newWallet:   testMasked(addressA),
		})
		if err := SetPayoutWallet(team.billingAdmin.teamSession.Ctx, team.networkId, walletB); err != nil {
			t.Fatal(err)
		}
		requireWalletNoticeRecipients(t, ctx, team.networkId, team.recipients, walletNoticeChange{
			destination: SecurityWalletDestinationUsdc,
			newWallet:   testMasked(addressB),
		})
		if removed := RemoveWallet(walletB, team.billingAdmin.teamSession); !removed.Success || removed.PayoutWalletId == nil || *removed.PayoutWalletId != walletA {
			t.Fatalf("the payout wallet's removal: %+v", removed)
		}
		requireWalletNoticeRecipients(t, ctx, team.networkId, team.recipients, walletNoticeChange{
			destination: SecurityWalletDestinationUsdc,
			newWallet:   testMasked(addressA),
		})

		// a removed member holds no role, and hears of no later change
		denyTestMember(t, ctx, team.networkId, team.billingAdmin.member.MemberId, newTestMemberRemovalEvent(team.networkId, team.billingAdmin.member.MemberId))
		if removed := RemoveWallet(walletA, team.owner.teamSession); !removed.Success || removed.PayoutWalletId != nil {
			t.Fatalf("the last wallet's removal: %+v", removed)
		}
		ownerRecipients := slices.DeleteFunc(slices.Clone(team.recipients), func(email string) bool {
			return slices.Contains(team.billingAdmin.emails, email)
		})
		requireWalletNoticeRecipients(t, ctx, team.networkId, ownerRecipients, walletNoticeChange{
			destination: SecurityWalletDestinationUsdc,
			newWallet:   "",
		})

		if delta := securityNoticeCount(SecurityNoticePayoutWallet, securityNoticeNoEmail) - noEmailBefore; delta != 0 {
			t.Fatalf("counted %v no_email payout wallet notices, want none: every change had its Owners and Billing admins to tell", delta)
		}
	})
}

// A team's subnet wallet notices reach its Owners and Billing admins alike and nobody else: its subnet claim wallet set
// and then replaced, and a provider wallet mapping consent of one of its clients, which moves the provider's wallet and
// the team's claim wallet, each owe one notice to each, listing what changed, and none to its other members, its
// pending Billing admin or its owner of record. None is counted as no_email.
func TestTeamSubnetWalletNoticesReachItsOwnersAndBillingAdmins(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		team := newWalletNoticeTeam(t, ctx)
		noEmailBefore := securityNoticeCount(SecurityNoticeSubnetWallet, securityNoticeNoEmail)

		_, coldkeyX, pubkeyX := testBittensorKey(t, 221)
		_, coldkeyY, pubkeyY := testBittensorKey(t, 222)
		SetStWalletOriginal(ctx, team.networkId, nil, coldkeyX, pubkeyX, nil, nil)
		requireWalletNoticeRecipients(t, ctx, team.networkId, team.recipients, walletNoticeChange{
			destination: SecurityWalletDestinationSubnetNetwork,
			newWallet:   testMasked(coldkeyX),
		})
		SetStWalletOriginal(ctx, team.networkId, nil, coldkeyY, pubkeyY, nil, nil)
		requireWalletNoticeRecipients(t, ctx, team.networkId, team.recipients, walletNoticeChange{
			destination: SecurityWalletDestinationSubnetNetwork,
			newWallet:   testMasked(coldkeyY),
		})

		// the consent fixture's provider is a client of the team, its consent the Billing admin's
		f := walletMappingProspectiveFixture(t)
		f.owner.ClientId = server.NewId()
		f.owner.NetworkId = team.networkId
		f.owner.UserId = team.billingAdmin.userId
		addContractPayoutTestClients(ctx, map[server.Id]server.Id{f.owner.ClientId: team.networkId})
		accepted, err := AcceptWalletMappingConsent(ctx, f.owner, f.challenge(t, 51), f.address)
		if err != nil || accepted == nil || !accepted.Applied {
			t.Fatal("the team provider's consent", accepted, err)
		}
		requireWalletNoticeRecipients(
			t,
			ctx,
			team.networkId,
			team.recipients,
			walletNoticeChange{destination: SecurityWalletDestinationSubnetProvider, newWallet: testMasked(f.address)},
			walletNoticeChange{destination: SecurityWalletDestinationSubnetNetwork, newWallet: testMasked(f.address)},
		)

		if delta := securityNoticeCount(SecurityNoticeSubnetWallet, securityNoticeNoEmail) - noEmailBefore; delta != 0 {
			t.Fatalf("counted %v no_email subnet wallet notices, want none", delta)
		}
	})
}

// A personal network's wallet notices are as they were: its owner's payout wallet and subnet wallet changes reach its
// owner's sign-in email addresses alone, though the owner is an Owner of a team that has a Billing admin and an Admin,
// and owe the team nothing.
func TestPersonalNetworkWalletNoticesStillReachItsOwner(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		owner := newKeyNoticePerson(t, ctx, newKeyNoticeEmail("owner"))
		owner.addGoogleSignIn(t, ctx, newKeyNoticeEmail("owner-google"))
		team := newKeyNoticeTeam(t, ctx, owner)
		team.join(t, newKeyNoticePerson(t, ctx, newKeyNoticeEmail("billing-admin")), NetworkMemberRoleBillingAdmin)
		team.join(t, newKeyNoticePerson(t, ctx, newKeyNoticeEmail("admin")), NetworkMemberRoleAdmin)
		personalNetworkId := owner.personalSession.ByJwt.NetworkId
		recipients := keyNoticeEmailsOf(owner)

		address := testSolanaAddress(t)
		walletId := CreateAccountWalletExternal(owner.personalSession, &CreateAccountWalletExternalArgs{
			NetworkId:        personalNetworkId,
			Blockchain:       SOL.String(),
			WalletAddress:    address,
			DefaultTokenType: "USDC",
		})
		if walletId == nil {
			t.Fatal("the wallet was not created")
		}
		if err := SetPayoutWallet(ctx, personalNetworkId, *walletId); err != nil {
			t.Fatal(err)
		}
		requireWalletNoticeRecipients(t, ctx, personalNetworkId, recipients, walletNoticeChange{
			destination: SecurityWalletDestinationUsdc,
			newWallet:   testMasked(address),
		})
		if removed := RemoveWallet(*walletId, owner.personalSession); !removed.Success || removed.PayoutWalletId != nil {
			t.Fatalf("the payout wallet's removal: %+v", removed)
		}
		requireWalletNoticeRecipients(t, ctx, personalNetworkId, recipients, walletNoticeChange{
			destination: SecurityWalletDestinationUsdc,
			newWallet:   "",
		})
		_, coldkey, pubkey := testBittensorKey(t, 223)
		SetStWalletOriginal(ctx, personalNetworkId, nil, coldkey, pubkey, nil, nil)
		requireWalletNoticeRecipients(t, ctx, personalNetworkId, recipients, walletNoticeChange{
			destination: SecurityWalletDestinationSubnetNetwork,
			newWallet:   testMasked(coldkey),
		})

		// the team, the owner an Owner of it, owes nothing for its Owner's own network's wallets
		requireNoWalletNotice(t, ctx, team.networkId, "its Owner's own network's wallet changes")
	})
}

// A team none of whose Owners and Billing admins has a sign-in email address owes its wallet notices to nobody, counted
// as no_email, as a network whose owner signs in with a phone number alone does: its Owner signs in with a phone number;
// its Admin and its Member, which hold no billing, and its pending Billing admin, which holds no role's reach yet, have
// email addresses; and its owner of record has no sign-in. Each change still goes through, and no message is owed to
// anyone, by email or SMS.
func TestTeamWalletNoticeWithNoBillingEmailIsOwedToNobody(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		owner := newKeyNoticePerson(t, ctx, "+1 415-555-0169")
		team := newKeyNoticeTeam(t, ctx, owner)
		team.join(t, newKeyNoticePerson(t, ctx, newKeyNoticeEmail("admin")), NetworkMemberRoleAdmin)
		team.join(t, newKeyNoticePerson(t, ctx, newKeyNoticeEmail("member")), NetworkMemberRoleMember)
		pendingBillingAdmin := newKeyNoticePerson(t, ctx, newKeyNoticeEmail("pending-billing-admin"))
		addTestSignInMember(t, ctx, team.networkId, pendingBillingAdmin.userId, NetworkMemberRoleBillingAdmin, true)
		payoutNoEmailBefore := securityNoticeCount(SecurityNoticePayoutWallet, securityNoticeNoEmail)
		payoutQueuedBefore := securityNoticeCount(SecurityNoticePayoutWallet, securityNoticeQueued)
		subnetNoEmailBefore := securityNoticeCount(SecurityNoticeSubnetWallet, securityNoticeNoEmail)
		subnetQueuedBefore := securityNoticeCount(SecurityNoticeSubnetWallet, securityNoticeQueued)

		walletId := CreateAccountWalletExternal(owner.teamSession, &CreateAccountWalletExternalArgs{
			NetworkId:        team.networkId,
			Blockchain:       SOL.String(),
			WalletAddress:    testSolanaAddress(t),
			DefaultTokenType: "USDC",
		})
		if walletId == nil {
			t.Fatal("the team's wallet was not created")
		}
		if err := SetPayoutWallet(owner.teamSession.Ctx, team.networkId, *walletId); err != nil {
			t.Fatal(err)
		}
		if payoutWalletId := GetPayoutWalletId(ctx, team.networkId); payoutWalletId == nil || *payoutWalletId != *walletId {
			t.Fatalf("the team's payout wallet is %v, want %s", payoutWalletId, *walletId)
		}
		_, coldkey, pubkey := testBittensorKey(t, 224)
		SetStWalletOriginal(ctx, team.networkId, nil, coldkey, pubkey, nil, nil)
		if stWallet := GetStWallet(ctx, team.networkId); stWallet == nil || stWallet.ColdkeySs58 != coldkey {
			t.Fatalf("the team's subnet claim wallet is %+v, want %s", stWallet, coldkey)
		}

		if count := countRows(ctx, `SELECT COUNT(*) FROM account_message_outbox WHERE network_id = $1`, team.networkId); count != 0 {
			t.Fatalf("the team owes %d messages, want none", count)
		}
		for _, c := range []struct {
			notice string
			result string
			before float64
			want   float64
		}{
			{notice: SecurityNoticePayoutWallet, result: securityNoticeNoEmail, before: payoutNoEmailBefore, want: 1},
			{notice: SecurityNoticePayoutWallet, result: securityNoticeQueued, before: payoutQueuedBefore, want: 0},
			{notice: SecurityNoticeSubnetWallet, result: securityNoticeNoEmail, before: subnetNoEmailBefore, want: 1},
			{notice: SecurityNoticeSubnetWallet, result: securityNoticeQueued, before: subnetQueuedBefore, want: 0},
		} {
			if delta := securityNoticeCount(c.notice, c.result) - c.before; delta != c.want {
				t.Errorf("counted %v %s %s notices, want %v", delta, c.result, c.notice, c.want)
			}
		}
	})
}
