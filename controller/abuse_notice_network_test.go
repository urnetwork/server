// The abuse action notice names the network it is about (owner decision 725,
// QD-1225; §21.8's notice and appeal, §7.8's notices row). It reaches the
// network's admin of record and, from M6, each standing Owner and Admin of a
// Team at their own addresses (TEAM-M6-02, ABUSE-M6-01), each with a network of
// its own and perhaps in other Teams, and its Target line, a host, a label or a
// sending domain, does not say which network to look at or appeal for. As the
// security notices do (QD-1216), it says "your URnetwork network <name>" in its
// subject, its preheader, its first line and its footer, the name read at
// delivery from the outbox row's network and never stored with the notice. The
// render cases run in every build; the delivery cases need MG5's abuse tables,
// and the Team case MG6's, so they run with -tags sidecar_pending.
package controller

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The network the samples' notices are about.
const abuseNoticeSampleNetworkName = "synthetic-abuse-network"

// An unnamed notice of each shape: a host kill with its end and appeal link, a
// forwarded copyright notice, and the lift of a host kill.
func abuseActionNoticeSample(kind model.AbuseActionKind) *AbuseActionNoticeTemplate {
	caseId := server.NewId()
	switch kind {
	case model.AbuseActionKindNoticeForward:
		return &AbuseActionNoticeTemplate{
			CaseId:   caseId.String(),
			Category: string(model.AbuseCategoryCopyright),
			Kind:     string(model.AbuseActionKindNoticeForward),
			Target:   "files.root.example",
		}
	case model.AbuseActionKindLift:
		return &AbuseActionNoticeTemplate{
			CaseId:     caseId.String(),
			Category:   string(model.AbuseCategoryPhishing),
			Kind:       string(model.AbuseActionKindLift),
			LiftedKind: string(model.AbuseActionKindHostKill),
			Target:     "shop.root.example",
		}
	default:
		until := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
		return &AbuseActionNoticeTemplate{
			CaseId:    caseId.String(),
			Category:  string(model.AbuseCategoryPhishing),
			Kind:      string(kind),
			Target:    "shop.root.example",
			UntilTime: &until,
			AppealUrl: "https://ur.io/abuse/appeal?case_id=" + caseId.String(),
		}
	}
}

// A rendered part with each run of space, line breaks included, read as one
// space, so a sentence the text part wraps reads as one line.
func abuseNoticeOneLine(part string) string {
	return strings.Join(strings.Fields(part), " ")
}

// Each shape of the notice, named, says "your URnetwork network <name>" in its
// subject, its preheader, its first line and its footer, and says "your
// network", or "your URnetwork network" without the name, nowhere: the headline
// is a few words in capitals that name no network.
func TestAbuseActionNoticeNamesItsNetwork(t *testing.T) {
	const network = "your URnetwork network " + abuseNoticeSampleNetworkName
	const footer = "Sent to the Owners and Admins of " + network + " when an abuse action is taken on it or lifted."
	const taken = "URnetwork took an abuse action on " + network + "."
	const holds = taken + " It holds until a person reviews it, and you can appeal it."
	for _, c := range []struct {
		notice    *AbuseActionNoticeTemplate
		subject   string
		preheader string
		firstLine string
		headline  string
	}{
		{
			notice:    abuseActionNoticeSample(model.AbuseActionKindHostKill),
			subject:   "An abuse action on " + network,
			preheader: taken,
			firstLine: holds,
			headline:  "Abuse action taken",
		},
		{
			notice:    abuseActionNoticeSample(model.AbuseActionKindNoticeForward),
			subject:   "A copyright notice about " + network,
			preheader: taken,
			firstLine: holds,
			headline:  "Abuse action taken",
		},
		{
			notice:    abuseActionNoticeSample(model.AbuseActionKindLift),
			subject:   "An abuse action on " + network + " was lifted",
			preheader: "An abuse action on " + network + " was lifted.",
			firstLine: "An abuse action on " + network + " has been lifted.",
			headline:  "Abuse action lifted",
		},
	} {
		var template Template = c.notice
		if _, ok := template.(accountMessageNetworkNamer); !ok {
			t.Fatal("the abuse action notice does not name its network")
		}
		c.notice.nameNetwork(abuseNoticeSampleNetworkName)
		subject, bodyHtml, bodyText, err := RenderEmailTemplate(c.notice)
		if err != nil {
			t.Fatalf("%s: render: %v", c.notice.Kind, err)
		}
		if subject != c.subject {
			t.Errorf("%s: subject = %q, want %q", c.notice.Kind, subject, c.subject)
		}
		html := abuseNoticeOneLine(bodyHtml)
		text := abuseNoticeOneLine(bodyText)
		for _, want := range []string{c.preheader, c.firstLine, footer, c.headline} {
			if !strings.Contains(html, want) {
				t.Errorf("%s: the html part does not say %q:\n%s", c.notice.Kind, want, bodyHtml)
			}
		}
		for _, want := range []string{c.firstLine, footer, strings.ToUpper(c.headline)} {
			if !strings.Contains(text, want) {
				t.Errorf("%s: the text part does not say %q:\n%s", c.notice.Kind, want, bodyText)
			}
		}
		for part, body := range map[string]string{"subject": subject, "html": html, "text": text} {
			if strings.Contains(strings.ToLower(body), "your network") {
				t.Errorf("%s: the %s says your network without naming it: %q", c.notice.Kind, part, body)
			}
			if mentionCount, namedCount := strings.Count(body, "your URnetwork network"), strings.Count(body, network); mentionCount != namedCount {
				t.Errorf("%s: the %s says your URnetwork network %d times and names it %d times", c.notice.Kind, part, mentionCount, namedCount)
			}
		}
		// the case, category, target and action still show, as before the name
		for _, want := range []string{c.notice.CaseId, c.notice.Target, c.notice.CategoryText(), c.notice.ActionText()} {
			if !strings.Contains(html, want) || !strings.Contains(text, want) {
				t.Errorf("%s: a part lacks %q", c.notice.Kind, want)
			}
		}
	}
}

// A notice with no name to show, as one decoded from the outbox is until its
// delivery names it, and as the delivery leaves one whose network is gone,
// reads "your URnetwork network", with no stray space.
func TestAbuseActionNoticeWithoutANameSaysYourNetwork(t *testing.T) {
	const footer = "Sent to the Owners and Admins of your URnetwork network when an abuse action is taken on it or lifted."
	for _, c := range []struct {
		kind      model.AbuseActionKind
		subject   string
		firstLine string
	}{
		{model.AbuseActionKindHostKill, "An abuse action on your URnetwork network", "URnetwork took an abuse action on your URnetwork network. It holds"},
		{model.AbuseActionKindNoticeForward, "A copyright notice about your URnetwork network", "URnetwork took an abuse action on your URnetwork network. It holds"},
		{model.AbuseActionKindLift, "An abuse action on your URnetwork network was lifted", "An abuse action on your URnetwork network has been lifted."},
	} {
		unnamed := abuseActionNoticeSample(c.kind)
		gone := abuseActionNoticeSample(c.kind)
		gone.nameNetwork("")
		for _, notice := range []*AbuseActionNoticeTemplate{unnamed, gone} {
			subject, bodyHtml, bodyText, err := RenderEmailTemplate(notice)
			if err != nil {
				t.Fatalf("%s: render: %v", c.kind, err)
			}
			if subject != c.subject {
				t.Errorf("%s: subject = %q, want %q", c.kind, subject, c.subject)
			}
			for _, part := range []string{abuseNoticeOneLine(bodyHtml), abuseNoticeOneLine(bodyText)} {
				if !strings.Contains(part, c.firstLine) || !strings.Contains(part, footer) {
					t.Errorf("%s: a part lacks the unnamed first line or footer:\n%s", c.kind, part)
				}
			}
			for _, part := range []string{subject, bodyHtml, bodyText} {
				if strings.Contains(part, "network  ") || strings.Contains(part, "network .") || strings.Contains(part, "network \n") {
					t.Errorf("%s: the unnamed network leaves a stray space: %q", c.kind, part)
				}
			}
		}
	}
}

// A stored name that ValidateNetworkName refuses but an older network may hold
// is shown on one line in reading order and escaped in the html part: a line
// break, a tab or a direction override neither breaks the subject's one line
// nor reorders the words around it, and markup in it stays text.
func TestAbuseActionNoticeShowsTheNetworkNameSafely(t *testing.T) {
	notice := abuseActionNoticeSample(model.AbuseActionKindHostKill)
	notice.nameNetwork("  Ops <b>net</b> &\r\nSubject: x\t\u202egro.example\u200b ")
	subject, bodyHtml, bodyText, err := RenderEmailTemplate(notice)
	if err != nil {
		t.Fatal(err)
	}
	const shown = "Ops <b>net</b> & Subject: x gro.example"
	if subject != "An abuse action on your URnetwork network "+shown {
		t.Errorf("subject = %q", subject)
	}
	for _, line := range []string{
		"\nURnetwork took an abuse action on your URnetwork network " + shown + ".\n",
		"\nSent to the Owners and Admins of your URnetwork network " + shown + "\n",
	} {
		if !strings.Contains(bodyText, line) {
			t.Errorf("the text part does not show the name on its line %q:\n%s", line, bodyText)
		}
	}
	for _, part := range []string{subject, bodyHtml, bodyText} {
		if strings.ContainsAny(part, "\u202e\u200b\t") {
			t.Errorf("the name kept a format character or a tab: %q", part)
		}
	}
	if strings.Count(bodyHtml, "your URnetwork network Ops &lt;b&gt;net&lt;/b&gt; &amp; Subject: x gro.example") != 3 || strings.Contains(bodyHtml, "<b>net</b>") {
		t.Errorf("the html part does not show the name escaped in its preheader, first line and footer:\n%s", bodyHtml)
	}
}

// The name is read at delivery and never stored: a named notice's fields, as
// the outbox stores them, hold no name, and a notice decoded from fields that
// carry one is unnamed until its delivery names it.
func TestAbuseActionNoticeNetworkNameIsNeverStored(t *testing.T) {
	notice := abuseActionNoticeSample(model.AbuseActionKindHostKill)
	notice.nameNetwork(abuseNoticeSampleNetworkName)
	templateJson, err := json.Marshal(notice)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(templateJson), abuseNoticeSampleNetworkName) || strings.Contains(strings.ToLower(string(templateJson)), "networkname") {
		t.Errorf("the stored fields hold the network's name: %s", templateJson)
	}

	decoded, err := decodeAccountMessageTemplate(
		notice.Name(),
		`{"CaseId":"synthetic-case","Kind":"host_kill","Target":"shop.root.example","networkName":"stored-name","network_name":"stored-name","NetworkName":"stored-name"}`,
	)
	if err != nil {
		t.Fatal(err)
	}
	subject, bodyHtml, bodyText, err := RenderEmailTemplate(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if subject != "An abuse action on your URnetwork network" {
		t.Errorf("a decoded notice's subject = %q, want it unnamed", subject)
	}
	for _, part := range []string{subject, bodyHtml, bodyText} {
		if strings.Contains(part, "stored-name") {
			t.Errorf("a name in the stored fields reached the notice: %q", part)
		}
	}
}

// A phishing case against a host of the network.
func openTestAbuseNoticeCase(t testing.TB, ctx context.Context, networkId server.Id) server.Id {
	t.Helper()
	var caseId server.Id
	var err error
	server.Tx(ctx, func(tx server.PgTx) {
		caseId, _, err = model.OpenAbuseCaseInTx(ctx, tx, &model.AbuseCaseArgs{
			NetworkId:     &networkId,
			Category:      model.AbuseCategoryPhishing,
			Target:        model.AbuseTarget{Kind: model.AbuseTargetKindHost, Name: "shop.root.example"},
			ReporterClass: model.AbuseReporterClassWebRisk,
			JoinKind:      model.AbuseJoinKindHost,
			JoinKey:       "shop.root.example",
		}, server.NowUtc(), model.DefaultAbuseSettings())
	})
	if err != nil {
		t.Fatal(err)
	}
	return caseId
}

// Delivers what is due now and returns the sends of the abuse action notice.
func deliverAbuseActionNotices(t testing.TB, ctx context.Context) []outboxTestSend {
	t.Helper()
	sender := newOutboxTestSender()
	deliverAccountMessagesAt(ctx, sender, server.NowUtc())
	templateName := (&AbuseActionNoticeTemplate{}).Name()
	sends := []outboxTestSend{}
	for _, send := range sender.sent() {
		if send.template.Name() == templateName {
			sends = append(sends, send)
		}
	}
	return sends
}

// Holds that the send's subject is the one given.
func requireAbuseNoticeSubject(t testing.TB, send outboxTestSend, wantSubject string) {
	t.Helper()
	subject, _, _, err := RenderEmailTemplate(send.template)
	if err != nil {
		t.Fatal(err)
	}
	if subject != wantSubject {
		t.Fatalf("the notice to %s: subject %q, want %q", send.userAuth, subject, wantSubject)
	}
}

// An action on a personal network owes its owner a notice that names the
// network as it is called when the notice is sent: renamed after the action
// and before the delivery, the notice shows the new name and not the old, and
// the outbox holds neither. The lift's notice names the network too.
func TestAbuseActionNoticeNamesAPersonalNetwork(t *testing.T) {
	requireAbuseTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := model.Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		newTestAbuseDeps(t)
		networkId := server.NewId()
		userId := server.NewId()
		oldName := securityNoticeTestNetworkName("acted-before")
		newName := securityNoticeTestNetworkName("acted-after")
		address, _ := model.NormalUserAuth(model.Testing_CreateNetwork(ctx, networkId, oldName, userId))
		clientSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{
			NetworkId:   networkId,
			UserId:      userId,
			NetworkName: oldName,
		})
		caseId := openTestAbuseNoticeCase(t, ctx, networkId)
		args := testAbuseKillArgs(caseId)
		if _, err := ApplyAbuseAction(ctx, args); err != nil {
			t.Fatal(err)
		}

		// the notice is owed; the network is renamed before it goes out
		userAuths, _ := testAbuseNotices(t, ctx, networkId)
		if len(userAuths) != 1 || userAuths[0] != address {
			t.Fatalf("the action owed notices to %v, want one to %s", userAuths, address)
		}
		renamed, err := ChangeNetworkName(ChangeNetworkNameArgs{NetworkName: newName}, clientSession)
		if err != nil || renamed.Error != nil || renamed.NetworkName != newName {
			t.Fatalf("rename: %v %+v", err, renamed)
		}
		for _, templateJson := range unsentSecurityNoticeJsons(t, ctx, networkId, (&AbuseActionNoticeTemplate{}).Name()) {
			if strings.Contains(templateJson, oldName) || strings.Contains(templateJson, newName) {
				t.Fatalf("the outbox holds the network's name: %s", templateJson)
			}
		}

		sends := deliverAbuseActionNotices(t, ctx)
		if len(sends) != 1 || sends[0].userAuth != address {
			t.Fatalf("delivered %d abuse notices (%+v), want one to %s", len(sends), sends, address)
		}
		requireSecurityNoticeNamesNetwork(t, sends[0], newName, oldName)
		requireAbuseNoticeSubject(t, sends[0], "An abuse action on your URnetwork network "+newName)

		reviewerId := server.NewId()
		if _, err := LiftAbuseAction(ctx, &model.AbuseLiftArgs{
			ActionId:       server.NewId(),
			CaseId:         caseId,
			LiftedActionId: args.ActionId,
			ReviewState:    model.AbuseReviewStateLiftedWrong,
			ReviewerId:     &reviewerId,
		}); err != nil {
			t.Fatal(err)
		}
		sends = deliverAbuseActionNotices(t, ctx)
		if len(sends) != 1 || sends[0].userAuth != address {
			t.Fatalf("delivered %d lift notices (%+v), want one to %s", len(sends), sends, address)
		}
		requireSecurityNoticeNamesNetwork(t, sends[0], newName, oldName)
		requireAbuseNoticeSubject(t, sends[0], "An abuse action on your URnetwork network "+newName+" was lifted")
	})
}

// An action on a Team owes each of its Owners and Admins a notice at their own
// address that names the Team's network and neither person's own; an action on
// the Owner's own network then names that network, and not the Team, to the
// Owner alone.
func TestAbuseActionNoticeOfATeamNamesTheTeam(t *testing.T) {
	requireAbuseTables(t)
	if !server.SidecarMg6Linked() {
		t.Skip("a Team is MG6's: run with -tags sidecar_pending")
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := model.Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		newTestAbuseDeps(t)
		// a person with a network of its own, at the address that network's creation gives it
		newPerson := func(label string) (networkId server.Id, networkName string, userId server.Id, address string, personal *session.ClientSession) {
			networkId = server.NewId()
			userId = server.NewId()
			networkName = securityNoticeTestNetworkName(label)
			address, _ = model.NormalUserAuth(model.Testing_CreateNetwork(ctx, networkId, networkName, userId))
			personal = session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: networkId, UserId: userId, NetworkName: networkName})
			return
		}
		ownerNetworkId, ownerNetworkName, _, ownerAddress, ownerPersonal := newPerson("acted-owner")
		_, adminNetworkName, adminUserId, adminAddress, _ := newPerson("acted-admin")

		teamName := securityNoticeTestNetworkName("acted-team")
		created, err := model.CreateNetworkTeam(&model.CreateNetworkTeamArgs{NetworkName: teamName}, ownerPersonal)
		if err != nil || created.Error != nil || created.NetworkId == nil {
			t.Fatalf("team-create: %v %+v", err, created)
		}
		teamNetworkId := *created.NetworkId
		server.Tx(ctx, func(tx server.PgTx) {
			_, err := model.AddNetworkMemberInTx(ctx, tx, &model.AddNetworkMemberArgs{
				NetworkId: teamNetworkId,
				UserId:    adminUserId,
				Role:      model.NetworkMemberRoleAdmin,
			})
			server.Raise(err)
		})

		teamCaseId := openTestAbuseNoticeCase(t, ctx, teamNetworkId)
		if _, err := ApplyAbuseAction(ctx, testAbuseKillArgs(teamCaseId)); err != nil {
			t.Fatal(err)
		}
		sends := deliverAbuseActionNotices(t, ctx)
		recipients := []string{}
		for _, send := range sends {
			recipients = append(recipients, send.userAuth)
			requireSecurityNoticeNamesNetwork(t, send, teamName, ownerNetworkName, adminNetworkName)
			requireAbuseNoticeSubject(t, send, "An abuse action on your URnetwork network "+teamName)
		}
		slices.Sort(recipients)
		want := []string{ownerAddress, adminAddress}
		slices.Sort(want)
		if !slices.Equal(recipients, want) {
			t.Fatalf("the Team's abuse notice went to %v, want the Owner and the Admin, %v", recipients, want)
		}

		// the Owner's own network is named for itself
		ownerCaseId := openTestAbuseNoticeCase(t, ctx, ownerNetworkId)
		if _, err := ApplyAbuseAction(ctx, testAbuseKillArgs(ownerCaseId)); err != nil {
			t.Fatal(err)
		}
		sends = deliverAbuseActionNotices(t, ctx)
		if len(sends) != 1 || sends[0].userAuth != ownerAddress {
			t.Fatalf("the personal network's abuse notice went to %+v, want its owner alone", sends)
		}
		requireSecurityNoticeNamesNetwork(t, sends[0], ownerNetworkName, teamName)
		requireAbuseNoticeSubject(t, sends[0], "An abuse action on your URnetwork network "+ownerNetworkName)
	})
}
