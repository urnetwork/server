package controller

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// §21's half of §14.8's ladder (abuse_mail_controller.go; ABUSE-M9-01) on MG5's and MG9's tables, with a recording
// AbuseEffects and mailer, over a fake ladder (halt rows written as SMTP's ladder writes them) and then SMTP's own
// SmtpHaltLadder on model's store. The store cases run in a build with sidecar_pending. Names are .example and
// addresses RFC 5737's.

const testAbuseMailRoot = "root.example"

// Skips a store case in a build that links neither MG5 nor MG9.
func requireAbuseMailTables(t *testing.T) {
	t.Helper()
	if !server.SidecarMg5Linked() || !server.SidecarMg9Linked() {
		t.Skip("MG5 and MG9 are pending: these cases run with -tags sidecar_pending")
	}
}

// §21's settings with the test root and a platform name, as abuse.yml would give them, for the rest of the test.
func newTestAbuseMail(t testing.TB) *model.AbuseSettings {
	settings := model.DefaultAbuseSettings()
	settings.RootNames = []string{testAbuseMailRoot}
	settings.PlatformNames = []string{"platform.example"}
	previous := abuseMailDeps.settings
	abuseMailDeps.settings = func() *model.AbuseSettings {
		return settings
	}
	t.Cleanup(func() {
		abuseMailDeps.settings = previous
	})
	return settings
}

// A network with an admin and a label in the test root, created long enough ago to be off probation, and its
// sending domain.
func newTestAbuseMailNetwork(t testing.TB, ctx context.Context) (networkId server.Id, domain string) {
	t.Helper()
	networkId = server.NewId()
	// the id's random tail, since its head is its time and two networks of one test share it
	model.Testing_CreateNetwork(ctx, networkId, "mail-"+networkId.String()[24:], server.NewId())
	label, err := model.EnsureNetworkLabel(ctx, networkId, testAbuseMailRoot)
	if err != nil {
		t.Fatal(err)
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE network SET create_time = $2 WHERE network_id = $1`, networkId, server.NowUtc().AddDate(0, 0, -60)))
	})
	return networkId, label + "." + testAbuseMailRoot
}

// A halt as SMTP's ladder writes it: its row, before §21 hears of it.
func testAbuseMailHalt(t testing.TB, ctx context.Context, networkId server.Id, label string, trigger string, startTime time.Time) *smtpedgeservice.SmtpHalt {
	t.Helper()
	halt := &smtpedgeservice.SmtpHalt{
		HaltId:    sidecar.NewId(),
		NetworkId: sidecar.Id(networkId),
		Label:     label,
		Trigger:   trigger,
		StartTime: startTime.UTC().Truncate(time.Microsecond),
	}
	if err := model.SetSmtpHalt(ctx, halt); err != nil {
		t.Fatal(err)
	}
	return halt
}

// One pass at now from the zero cursor, which must leave nothing failed.
func testRunAbuseMailPass(t testing.TB, ctx context.Context, now time.Time) *AbuseMailPassResult {
	t.Helper()
	result := RunAbuseMailPass(ctx, model.AbuseSmtpComplaintCursor{}, now, DefaultAbuseMailSettings())
	if result.FailedCount != 0 {
		t.Fatalf("the pass failed %d items: %+v", result.FailedCount, result)
	}
	return result
}

// The cases of one network, oldest first.
func testAbuseMailCases(t testing.TB, ctx context.Context, networkId server.Id) (cases []*model.AbuseCase) {
	t.Helper()
	caseIds := []server.Id{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT case_id FROM abuse_case WHERE network_id = $1 ORDER BY create_time, case_id`, networkId)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var caseId server.Id
				server.Raise(result.Scan(&caseId))
				caseIds = append(caseIds, caseId)
			}
		})
	})
	for _, caseId := range caseIds {
		cases = append(cases, model.GetAbuseCase(ctx, caseId))
	}
	return
}

// The network's actions of kind, oldest first.
func testAbuseMailActions(t testing.TB, ctx context.Context, networkId server.Id, kind model.AbuseActionKind) (actions []*model.AbuseAction) {
	t.Helper()
	for _, abuseCase := range testAbuseMailCases(t, ctx, networkId) {
		for _, action := range model.GetAbuseCaseActions(ctx, abuseCase.CaseId) {
			if action.Kind == kind {
				actions = append(actions, action)
			}
		}
	}
	return
}

// The network's network_abuse_state row's state, "" for none.
func testAbuseMailStateRow(t testing.TB, ctx context.Context, networkId server.Id) (state string) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT state FROM network_abuse_state WHERE network_id = $1`, networkId)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&state))
			}
		})
	})
	return
}

// The network's standing now.
func testAbuseMailStanding(t testing.TB, ctx context.Context, networkId server.Id) model.AbuseStanding {
	t.Helper()
	standing, found := model.GetNetworkAbuseStanding(ctx, networkId, server.NowUtc())
	if !found {
		t.Fatal("no network")
	}
	return standing.Standing
}

// When a case opened at at on signal's row falls due, on the calendar abuse.yml gives.
func testAbuseMailDue(t testing.TB, settings *model.AbuseSettings, signal model.AbuseSignal, at time.Time) time.Time {
	t.Helper()
	calendar, err := model.NewAbuseCalendar(model.AbuseReviewHoursOf(server.GetAbuseConfig()), &settings.AbusePolicySettings)
	if err != nil {
		t.Fatal(err)
	}
	return model.AbuseReviewDue(signal, "", "", at, calendar)
}

// A network halt becomes a case, opened at the halt's start on the ≤24 h clock, and an action under the halt's own
// id, applied, mailed and noticed, which restricts the network so its internet mail resumes at Community caps; a
// second pass records nothing again.
func TestAbuseMailHaltBecomesACaseAndAnAction(t *testing.T) {
	requireAbuseMailTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := newTestAbuseMail(t)
		effects, mailer := newTestAbuseDeps(t)
		networkId, domain := newTestAbuseMailNetwork(t, ctx)
		adminUserAuth := networkId.String() + "@bringyour.com"
		startTime := server.NowUtc().Add(-10 * time.Minute)
		halt := testAbuseMailHalt(t, ctx, networkId, "", smtpedgeservice.SmtpHaltTriggerCommunityComplaint, startTime)
		if standing := testAbuseMailStanding(t, ctx, networkId); standing != model.AbuseStandingStandard {
			t.Fatalf("before §21 hears of the halt the network is %s", standing)
		}

		result := testRunAbuseMailPass(t, ctx, server.NowUtc())
		if result.HaltRecordedCount != 1 || result.SuspendedCount != 0 {
			t.Fatalf("the pass %+v", result)
		}
		cases := testAbuseMailCases(t, ctx, networkId)
		if len(cases) != 1 {
			t.Fatalf("%d cases", len(cases))
		}
		abuseCase := cases[0]
		if abuseCase.Category != model.AbuseCategorySpam || abuseCase.TargetKind != model.AbuseTargetKindSendingDomain ||
			abuseCase.Target != domain || abuseCase.ReporterClass != model.AbuseReporterClassMailReport ||
			abuseCase.JoinKind != model.AbuseJoinKindHeaders || abuseCase.State != model.AbuseCaseStateOpen || abuseCase.Barred {
			t.Fatalf("the case reads %+v", abuseCase)
		}
		if !abuseCase.CreateTime.Equal(halt.StartTime) {
			t.Fatalf("the case opened at %s, not the halt's start %s", abuseCase.CreateTime, halt.StartTime)
		}
		if due := testAbuseMailDue(t, settings, model.AbuseSignalMailJoinHigh, halt.StartTime); abuseCase.ReviewDueTime == nil || !abuseCase.ReviewDueTime.Equal(due) {
			t.Fatalf("the case is due %v, want %s", abuseCase.ReviewDueTime, due)
		}

		actions := model.GetAbuseCaseActions(ctx, abuseCase.CaseId)
		if len(actions) != 1 {
			t.Fatalf("%d actions", len(actions))
		}
		action := actions[0]
		until := halt.StartTime.Add(smtpedgeservice.SmtpHaltDuration)
		if action.ActionId != server.Id(halt.HaltId) || action.Kind != model.AbuseActionKindMailHalt || action.Target != domain ||
			!action.ByMachine || action.ReviewState != model.AbuseReviewStatePending || action.UntilTime == nil || !action.UntilTime.Equal(until) ||
			action.AppliedTime == nil || action.MailedTime == nil {
			t.Fatalf("the action reads %+v", action)
		}
		if applied := effects.applied(); len(applied) != 1 || applied[0].Action.ActionId != action.ActionId {
			t.Fatalf("the effects %v", applied)
		}
		bodies := mailer.mailed()
		if len(bodies) != 1 {
			t.Fatalf("%d records mailed", len(bodies))
		}
		record, err := ParseAbuseRecord(bodies[0], testAbuseRecordMacKey)
		if err != nil {
			t.Fatal(err)
		}
		// a record's times are RFC 3339 to the millisecond
		if record.ActionId != action.ActionId || record.Kind != model.AbuseActionKindMailHalt || record.Until == nil || !record.Until.Equal(until.Truncate(time.Millisecond)) {
			t.Fatalf("the record reads %+v", record)
		}
		userAuths, templates := testAbuseNotices(t, ctx, networkId)
		if len(templates) != 1 || userAuths[0] != adminUserAuth || templates[0].Kind != string(model.AbuseActionKindMailHalt) ||
			templates[0].CaseId != abuseCase.CaseId.String() || templates[0].Target != domain || templates[0].UntilTime == nil {
			t.Fatalf("the notices %v %+v", userAuths, templates)
		}

		// §21.7: restricted, so the send gate gives the network Community's caps once the halt ends
		if state := testAbuseMailStateRow(t, ctx, networkId); state != "restricted" {
			t.Fatalf("the state row reads %q", state)
		}
		standing := testAbuseMailStanding(t, ctx, networkId)
		if standing != model.AbuseStandingRestricted || !model.AbuseStandingLimits(standing, settings).CommunityMailCaps {
			t.Fatalf("the network stands %s", standing)
		}

		if again := testRunAbuseMailPass(t, ctx, server.NowUtc()); again.HaltRecordedCount != 0 {
			t.Fatalf("a second pass %+v", again)
		}
		if len(model.GetAbuseCaseActions(ctx, abuseCase.CaseId)) != 1 || len(mailer.mailed()) != 1 {
			t.Fatal("a second pass recorded the halt again")
		}
	})
}

// A burner label's halt is recorded as mail_label_halt on the burner's own sending domain, and restricts nothing
// (reading 4).
func TestAbuseMailLabelHaltRestrictsNothing(t *testing.T) {
	requireAbuseMailTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		newTestAbuseMail(t)
		newTestAbuseDeps(t)
		networkId, domain := newTestAbuseMailNetwork(t, ctx)
		halt := testAbuseMailHalt(t, ctx, networkId, "otter-1234", smtpedgeservice.SmtpHaltTriggerLabelComplaint, server.NowUtc().Add(-10*time.Minute))

		if result := testRunAbuseMailPass(t, ctx, server.NowUtc()); result.HaltRecordedCount != 1 {
			t.Fatalf("the pass %+v", result)
		}
		actions := testAbuseMailActions(t, ctx, networkId, model.AbuseActionKindMailLabelHalt)
		if len(actions) != 1 || actions[0].ActionId != server.Id(halt.HaltId) || actions[0].Target != "otter-1234."+domain {
			t.Fatalf("the label's actions %+v", actions)
		}
		if len(testAbuseMailActions(t, ctx, networkId, model.AbuseActionKindMailHalt)) != 0 {
			t.Fatal("a label's halt took the network's")
		}
		if state := testAbuseMailStateRow(t, ctx, networkId); state != "" {
			t.Fatalf("a label's halt left a %q row", state)
		}
		if standing := testAbuseMailStanding(t, ctx, networkId); standing != model.AbuseStandingStandard {
			t.Fatalf("the network stands %s", standing)
		}
	})
}

// §21.4's rung: the third network halt in 30 days suspends the network until reviewed, once, through the action
// path; a fourth while the suspension stands suspends nothing again, and a burner label's halt never counts.
func TestAbuseMailThirdHaltSuspendsOnce(t *testing.T) {
	requireAbuseMailTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		newTestAbuseMail(t)
		effects, _ := newTestAbuseDeps(t)
		networkId, _ := newTestAbuseMailNetwork(t, ctx)
		now := server.NowUtc()
		testAbuseMailHalt(t, ctx, networkId, "", smtpedgeservice.SmtpHaltTriggerCommunityComplaint, now.Add(-72*time.Hour))
		testAbuseMailHalt(t, ctx, networkId, "otter-1234", smtpedgeservice.SmtpHaltTriggerLabelComplaint, now.Add(-60*time.Hour))
		testAbuseMailHalt(t, ctx, networkId, "", smtpedgeservice.SmtpHaltTriggerRecordCuts, now.Add(-48*time.Hour))
		if result := testRunAbuseMailPass(t, ctx, now); result.HaltRecordedCount != 3 || result.SuspendedCount != 0 {
			t.Fatalf("two network halts and a label's: %+v", result)
		}
		third := testAbuseMailHalt(t, ctx, networkId, "", smtpedgeservice.SmtpHaltTriggerFraud, now.Add(-10*time.Minute))
		if result := testRunAbuseMailPass(t, ctx, server.NowUtc()); result.HaltRecordedCount != 1 || result.SuspendedCount != 1 {
			t.Fatalf("the third network halt: %+v", result)
		}
		suspensions := testAbuseMailActions(t, ctx, networkId, model.AbuseActionKindNetworkSuspend)
		thirdAction := model.GetAbuseAction(ctx, server.Id(third.HaltId))
		if len(suspensions) != 1 || thirdAction == nil || suspensions[0].CaseId != thirdAction.CaseId || !suspensions[0].ByMachine {
			t.Fatalf("the suspensions %+v under the third halt %+v", suspensions, thirdAction)
		}
		// a fraud halt's case is a threat category's, which still counts toward spam's rung
		if abuseCase := model.GetAbuseCase(ctx, thirdAction.CaseId); abuseCase.Category != model.AbuseCategoryPhishing {
			t.Fatalf("the fraud halt's case is %s", abuseCase.Category)
		}
		applied := []model.AbuseActionKind{}
		for _, effect := range effects.applied() {
			applied = append(applied, effect.Action.Kind)
		}
		if !slices.Contains(applied, model.AbuseActionKindNetworkSuspend) {
			t.Fatalf("the effects %v", applied)
		}

		testAbuseMailHalt(t, ctx, networkId, "", smtpedgeservice.SmtpHaltTriggerHardBounces, now.Add(-5*time.Minute))
		if result := testRunAbuseMailPass(t, ctx, server.NowUtc()); result.HaltRecordedCount != 1 || result.SuspendedCount != 0 {
			t.Fatalf("a fourth halt under the suspension: %+v", result)
		}
		if got := testAbuseMailActions(t, ctx, networkId, model.AbuseActionKindNetworkSuspend); len(got) != 1 {
			t.Fatalf("%d suspensions", len(got))
		}
	})
}

// A halt review lifted as wrong counts toward no rung: two network halts, one lifted as wrong, and a third suspends
// nothing; a fourth, the third that counts, suspends.
func TestAbuseMailHaltLiftedAsWrongNotCounted(t *testing.T) {
	requireAbuseMailTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		newTestAbuseMail(t)
		newTestAbuseDeps(t)
		networkId, _ := newTestAbuseMailNetwork(t, ctx)
		now := server.NowUtc()
		wrong := testAbuseMailHalt(t, ctx, networkId, "", smtpedgeservice.SmtpHaltTriggerCommunityComplaint, now.Add(-96*time.Hour))
		testAbuseMailHalt(t, ctx, networkId, "", smtpedgeservice.SmtpHaltTriggerCommunityComplaint, now.Add(-72*time.Hour))
		testRunAbuseMailPass(t, ctx, now)
		wrongAction := model.GetAbuseAction(ctx, server.Id(wrong.HaltId))
		reviewerId := server.NewId()
		if _, err := LiftAbuseAction(ctx, &model.AbuseLiftArgs{
			ActionId:       server.NewId(),
			CaseId:         wrongAction.CaseId,
			LiftedActionId: wrongAction.ActionId,
			ReviewState:    model.AbuseReviewStateLiftedWrong,
			ReviewerId:     &reviewerId,
			ReviewReason:   "the complaints were a list operator's mistake",
		}); err != nil {
			t.Fatal(err)
		}
		testAbuseMailHalt(t, ctx, networkId, "", smtpedgeservice.SmtpHaltTriggerCommunityComplaint, now.Add(-48*time.Hour))
		if result := testRunAbuseMailPass(t, ctx, server.NowUtc()); result.SuspendedCount != 0 {
			t.Fatalf("two halts that count suspended: %+v", result)
		}
		testAbuseMailHalt(t, ctx, networkId, "", smtpedgeservice.SmtpHaltTriggerCommunityComplaint, now.Add(-10*time.Minute))
		if result := testRunAbuseMailPass(t, ctx, server.NowUtc()); result.SuspendedCount != 1 {
			t.Fatalf("the third that counts: %+v", result)
		}
	})
}

// The ARF joins: a low join opens a case on §21.8's last row and reaches review only, with no halt and no action; a
// high join opens one on the ≤24 h clock; role mail, which the intake hands the sink itself, and a not-spam open
// none from the pass; and the cursor moves past what it took, so a second pass opens nothing.
func TestAbuseMailComplaintsReachReview(t *testing.T) {
	requireAbuseMailTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := newTestAbuseMail(t)
		newTestAbuseDeps(t)
		lowNetworkId, lowDomain := newTestAbuseMailNetwork(t, ctx)
		highNetworkId, highDomain := newTestAbuseMailNetwork(t, ctx)
		arrivalTime := server.NowUtc().Add(-30 * time.Minute).Truncate(time.Microsecond)
		complaint := func(networkId server.Id, complaintType string, reporter string, domain string, joinResult string) {
			if err := model.InsertSmtpComplaint(ctx, &smtpedgeservice.SmtpComplaint{
				ComplaintId: sidecar.NewId(),
				NetworkId:   sidecar.Id(networkId),
				Reporter:    reporter,
				Type:        complaintType,
				ArrivalTime: arrivalTime,
				Domain:      domain,
				JoinResult:  joinResult,
			}); err != nil {
				t.Fatal(err)
			}
		}
		complaint(lowNetworkId, smtpedgeservice.SmtpComplaintTypeAbuse, "fbl.example", lowDomain, "low")
		complaint(highNetworkId, smtpedgeservice.SmtpComplaintTypeAbuse, "fbl.example", highDomain, "high")
		complaint(highNetworkId, smtpedgeservice.SmtpComplaintTypeRole, "", "role."+highDomain, "medium")
		complaint(highNetworkId, smtpedgeservice.SmtpComplaintTypeNotSpam, "fbl.example", "not."+highDomain, "high")

		result := RunAbuseMailPass(ctx, model.AbuseSmtpComplaintCursor{}, server.NowUtc(), DefaultAbuseMailSettings())
		if result.ComplaintCaseCount != 2 || result.FailedCount != 0 || result.HaltRecordedCount != 0 {
			t.Fatalf("the pass %+v", result)
		}
		lowCases := testAbuseMailCases(t, ctx, lowNetworkId)
		if len(lowCases) != 1 || lowCases[0].Target != lowDomain || lowCases[0].Category != model.AbuseCategorySpam ||
			lowCases[0].ReporterClass != model.AbuseReporterClassMailReport {
			t.Fatalf("the low join's cases %+v", lowCases)
		}
		if due := testAbuseMailDue(t, settings, model.AbuseSignalMailJoinLow, arrivalTime); lowCases[0].ReviewDueTime == nil || !lowCases[0].ReviewDueTime.Equal(due) {
			t.Fatalf("the low join is due %v, want %s", lowCases[0].ReviewDueTime, due)
		}
		if actions := model.GetAbuseCaseActions(ctx, lowCases[0].CaseId); len(actions) != 0 {
			t.Fatalf("a low join took %d actions", len(actions))
		}
		if halts, err := model.FindSmtpHalts(ctx, lowNetworkId, arrivalTime.Add(-time.Hour)); err != nil || len(halts) != 0 {
			t.Fatalf("a low join halted: %v, %v", halts, err)
		}
		if standing := testAbuseMailStanding(t, ctx, lowNetworkId); standing != model.AbuseStandingStandard {
			t.Fatalf("a low join left the network %s", standing)
		}
		highCases := testAbuseMailCases(t, ctx, highNetworkId)
		if len(highCases) != 1 || highCases[0].Target != highDomain {
			t.Fatalf("the high join's cases %+v", highCases)
		}
		if due := testAbuseMailDue(t, settings, model.AbuseSignalMailJoinHigh, arrivalTime); highCases[0].ReviewDueTime == nil || !highCases[0].ReviewDueTime.Equal(due) {
			t.Fatalf("the high join is due %v, want %s", highCases[0].ReviewDueTime, due)
		}

		again := RunAbuseMailPass(ctx, result.Next, server.NowUtc(), DefaultAbuseMailSettings())
		if again.ComplaintCaseCount != 0 || again.Next != result.Next {
			t.Fatalf("a second pass from the cursor %+v", again)
		}
	})
}

// D-SMTP-23's RUA mismatch opens a case through SMTP-M9-17's sink, and no halt or action follows from it.
func TestAbuseMailRuaOpensACaseWithNoHalt(t *testing.T) {
	requireAbuseMailTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		newTestAbuseMail(t)
		newTestAbuseDeps(t)
		networkId, _ := newTestAbuseMailNetwork(t, ctx)
		if err := NewSmtpReconcileCaseSink(nil).OpenSmtpReconcileCase(ctx, &smtpedgeservice.SmtpReconcileDay{NetworkId: sidecar.Id(networkId)}); err != nil {
			t.Fatal(err)
		}
		cases := testAbuseMailCases(t, ctx, networkId)
		if len(cases) != 1 || cases[0].Category != model.AbuseCategorySpam || cases[0].ReviewDueTime == nil {
			t.Fatalf("the RUA's cases %+v", cases)
		}
		if actions := model.GetAbuseCaseActions(ctx, cases[0].CaseId); len(actions) != 0 {
			t.Fatalf("the RUA took %d actions", len(actions))
		}
		if result := testRunAbuseMailPass(t, ctx, server.NowUtc()); result.HaltRecordedCount != 0 {
			t.Fatalf("the pass found a halt %+v", result)
		}
		if standing := testAbuseMailStanding(t, ctx, networkId); standing != model.AbuseStandingStandard {
			t.Fatalf("the RUA left the network %s", standing)
		}
	})
}

// A ladder for a test, on model's store, the network's tier the fake quota's Community, at the fake clock.
func newTestAbuseMailLadder(t testing.TB, ctx context.Context, clock *sidecartest.FakeClock, kicks *int) *smtpedgeservice.SmtpHaltLadder {
	t.Helper()
	host := sidecartest.NewFakeHost(ctx, clock, nil, nil, nil)
	settings := smtpedgeservice.DefaultSmtpEdgeSettings()
	settings.Root = testAbuseMailRoot
	settings.ReportHostName = "rpt.report.example"
	settings.SendingAddresses = []smtpedgeservice.SmtpSendingAddress{
		{Address: netip.MustParseAddr("192.0.2.10"), HeloName: "o1.out.report.example"},
	}
	store := model.NewSmtpEdgeStore(nil)
	ladder, err := smtpedgeservice.NewSmtpHaltLadder(host.ServiceHost(), &smtpedgeservice.SmtpHaltLadderDeps{
		Store:   store,
		Lookups: store,
		Halts:   store,
		Sink: model.NewAbuseSmtpSink(&model.AbuseSmtpSinkDeps{
			Kick: func(ctx context.Context) error {
				*kicks += 1
				return nil
			},
		}),
	}, settings)
	if err != nil {
		t.Fatal(err)
	}
	return ladder
}

// The same on SMTP's own ladder: a Community network's first complaint halts it (§14.8), the complaint and the halt
// reach one case, the halt its action, and the network is restricted; a not-spam for the message then lets the
// recheck lift the halt, removing its row, and the pass lifts the action as wrong, so the network is restricted no
// more; the route's own lift finds it lifted already.
func TestAbuseMailOnSmtpsLadder(t *testing.T) {
	requireAbuseMailTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		newTestAbuseMail(t)
		newTestAbuseDeps(t)
		networkId, domain := newTestAbuseMailNetwork(t, ctx)
		arrivalTime := server.NowUtc().Add(-30 * time.Minute).Truncate(time.Millisecond)
		clock := sidecartest.NewFakeClock(arrivalTime)
		kicks := 0
		ladder := newTestAbuseMailLadder(t, ctx, clock, &kicks)

		messageIdHash := smtpedgeservice.SmtpMessageIdHash("<m1@" + domain + ">")
		complaint := &smtpedgeservice.SmtpComplaint{
			ComplaintId:   sidecar.NewId(),
			NetworkId:     sidecar.Id(networkId),
			Reporter:      "fbl.example",
			Type:          smtpedgeservice.SmtpComplaintTypeAbuse,
			ArrivalTime:   arrivalTime,
			Domain:        domain,
			MessageIdHash: messageIdHash,
			JoinResult:    "high",
		}
		if err := model.InsertSmtpComplaint(ctx, complaint); err != nil {
			t.Fatal(err)
		}
		halts, err := ladder.Apply(ctx, &smtpedgeservice.SmtpHaltEvent{
			Kind:      smtpedgeservice.SmtpHaltEventComplaint,
			NetworkId: sidecar.Id(networkId),
			Time:      arrivalTime,
			Complaint: complaint,
		})
		if err != nil || len(halts) != 1 || halts[0].Label != "" || halts[0].Trigger != smtpedgeservice.SmtpHaltTriggerCommunityComplaint {
			t.Fatalf("the ladder set %+v, %v", halts, err)
		}
		halt := halts[0]

		result := testRunAbuseMailPass(t, ctx, server.NowUtc())
		if result.ComplaintCaseCount != 1 || result.HaltRecordedCount != 1 {
			t.Fatalf("the pass %+v", result)
		}
		cases := testAbuseMailCases(t, ctx, networkId)
		if len(cases) != 1 || cases[0].Target != domain {
			t.Fatalf("the complaint and its halt made %d cases: %+v", len(cases), cases)
		}
		action := model.GetAbuseAction(ctx, server.Id(halt.HaltId))
		if action == nil || action.CaseId != cases[0].CaseId || action.Kind != model.AbuseActionKindMailHalt {
			t.Fatalf("the halt's action %+v", action)
		}
		if standing := testAbuseMailStanding(t, ctx, networkId); standing != model.AbuseStandingRestricted {
			t.Fatalf("the network stands %s", standing)
		}
		if kicks != 0 {
			t.Fatalf("one halt asked for a suspension %d times", kicks)
		}

		// RFC 6430: the recipient takes it back, and the recheck lifts the halt
		if err := model.InsertSmtpComplaint(ctx, &smtpedgeservice.SmtpComplaint{
			ComplaintId:   sidecar.NewId(),
			NetworkId:     sidecar.Id(networkId),
			Reporter:      "fbl.example",
			Type:          smtpedgeservice.SmtpComplaintTypeNotSpam,
			ArrivalTime:   arrivalTime.Add(time.Minute),
			Domain:        domain,
			MessageIdHash: messageIdHash,
			JoinResult:    "high",
		}); err != nil {
			t.Fatal(err)
		}
		clock.Advance(2 * time.Minute)
		lifted, err := ladder.Lift(ctx, sidecar.Id(networkId), halt.HaltId)
		if err != nil || !lifted {
			t.Fatalf("the recheck: %t, %v", lifted, err)
		}
		result = testRunAbuseMailPass(t, ctx, server.NowUtc())
		if result.RecheckLiftCount != 1 {
			t.Fatalf("the pass after the recheck %+v", result)
		}
		action = model.GetAbuseAction(ctx, server.Id(halt.HaltId))
		if action.ReviewState != model.AbuseReviewStateLiftedWrong {
			t.Fatalf("the halt's action reads %s", action.ReviewState)
		}
		if standing := testAbuseMailStanding(t, ctx, networkId); standing != model.AbuseStandingStandard {
			t.Fatalf("after the recheck the network stands %s", standing)
		}
		if lift, err := LiftAbuseSmtpHaltOnRecheck(ctx, networkId, server.Id(halt.HaltId)); err != nil || lift != nil {
			t.Fatalf("the route's lift of a lifted halt: %+v, %v", lift, err)
		}
	})
}

// The route's own lift: a recheck's lifted halt is lifted as wrong at once, by machine, noticed, and out of the
// restricted window; another network's halt and an id that is no halt lift nothing.
func TestAbuseMailRecheckLiftFromTheRoute(t *testing.T) {
	requireAbuseMailTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		newTestAbuseMail(t)
		newTestAbuseDeps(t)
		networkId, _ := newTestAbuseMailNetwork(t, ctx)
		otherNetworkId, _ := newTestAbuseMailNetwork(t, ctx)
		halt := testAbuseMailHalt(t, ctx, networkId, "", smtpedgeservice.SmtpHaltTriggerProRate, server.NowUtc().Add(-10*time.Minute))
		testRunAbuseMailPass(t, ctx, server.NowUtc())
		if standing := testAbuseMailStanding(t, ctx, networkId); standing != model.AbuseStandingRestricted {
			t.Fatalf("the network stands %s", standing)
		}
		if err := model.DeleteSmtpHalt(ctx, server.Id(halt.HaltId)); err != nil {
			t.Fatal(err)
		}

		for name, other := range map[string][2]server.Id{
			"another network's": {otherNetworkId, server.Id(halt.HaltId)},
			"no halt":           {networkId, server.NewId()},
		} {
			if lift, err := LiftAbuseSmtpHaltOnRecheck(ctx, other[0], other[1]); err != nil || lift != nil {
				t.Fatalf("%s: %+v, %v", name, lift, err)
			}
		}
		lift, err := LiftAbuseSmtpHaltOnRecheck(ctx, networkId, server.Id(halt.HaltId))
		if err != nil || lift == nil || !lift.Inserted || !lift.Action.ByMachine || lift.Action.Kind != model.AbuseActionKindLift {
			t.Fatalf("the lift %+v, %v", lift, err)
		}
		if action := model.GetAbuseAction(ctx, server.Id(halt.HaltId)); action.ReviewState != model.AbuseReviewStateLiftedWrong {
			t.Fatalf("the halt's action reads %s", action.ReviewState)
		}
		if standing := testAbuseMailStanding(t, ctx, networkId); standing != model.AbuseStandingStandard {
			t.Fatalf("after the lift the network stands %s", standing)
		}
		_, templates := testAbuseNotices(t, ctx, networkId)
		if len(templates) != 2 || templates[1].LiftedKind != string(model.AbuseActionKindMailHalt) {
			t.Fatalf("the notices %+v", templates)
		}
		if result := testRunAbuseMailPass(t, ctx, server.NowUtc()); result.RecheckLiftCount != 0 {
			t.Fatalf("the pass lifted it again %+v", result)
		}
	})
}

// The real effects of a mail halt (abuse_effects.go): a halt whose row a restore lost is written back under §21's
// trigger, its start one halt before its until; a row SMTP's ladder wrote is left with its trigger; an action lifted
// before its effect ran, or a halt that has ended, writes nothing; and a lift removes the row.
func TestAbuseMailHaltEffectWritesBackALostRow(t *testing.T) {
	requireAbuseMailTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		deps := DefaultAbuseEffectsDeps()
		hints := 0
		deps.HintClientIds = func(ctx context.Context, networkId server.Id, limit int) []server.Id {
			return []server.Id{server.NewId()}
		}
		deps.Hint = func(ctx context.Context, clientIds []server.Id, hint *sidecar.ClientHint) error {
			hints += 1
			return nil
		}
		mailHints := []testAbuseMailHint{}
		deps.SmtpStatusHint = func(ctx context.Context, networkId server.Id, kind protocol.SmtpStatusKind, retryAfter time.Duration, since time.Time, now time.Time) error {
			mailHints = append(mailHints, testAbuseMailHint{kind: kind, retryAfter: retryAfter, window: now.Sub(since)})
			return nil
		}
		effects := NewAbuseEffects(deps, nil)
		networkId := server.NewId()
		now := server.NowUtc().Truncate(time.Microsecond)
		action := func(kind model.AbuseActionKind, target string, until time.Time) *model.AbuseAction {
			return &model.AbuseAction{
				ActionId:    server.NewId(),
				CaseId:      server.NewId(),
				NetworkId:   &networkId,
				Category:    model.AbuseCategorySpam,
				Kind:        kind,
				Target:      target,
				UntilTime:   &until,
				ByMachine:   true,
				ReviewState: model.AbuseReviewStatePending,
				CreateTime:  now,
			}
		}
		row := func(haltId server.Id) *smtpedgeservice.SmtpHalt {
			halts, err := model.FindSmtpHalts(ctx, networkId, now.Add(-48*time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			for _, halt := range halts {
				if server.Id(halt.HaltId) == haltId {
					return halt
				}
			}
			return nil
		}

		lost := action(model.AbuseActionKindMailLabelHalt, "otter-1234.quietnet.root.example", now.Add(20*time.Hour))
		if err := effects.ApplyAbuseEffect(ctx, &AbuseEffect{Action: lost}); err != nil {
			t.Fatal(err)
		}
		written := row(lost.ActionId)
		if written == nil || written.Label != "otter-1234" || written.Trigger != model.AbuseSmtpHaltTrigger ||
			!written.StartTime.Equal(lost.UntilTime.Add(-smtpedgeservice.SmtpHaltDuration)) {
			t.Fatalf("the restored halt %+v", written)
		}
		// no one hinted the halt a restore lost, so the effect holds the queues until it ends
		if len(mailHints) != 1 || mailHints[0].kind != protocol.SmtpStatusKind_SMTP_STATUS_HALTED ||
			mailHints[0].retryAfter <= 19*time.Hour || 20*time.Hour < mailHints[0].retryAfter || mailHints[0].window != 24*time.Hour {
			t.Fatalf("the mail hints %+v", mailHints)
		}

		kept := action(model.AbuseActionKindMailHalt, "quietnet.root.example", now.Add(23*time.Hour))
		if err := model.SetSmtpHalt(ctx, &smtpedgeservice.SmtpHalt{
			HaltId:    sidecar.Id(kept.ActionId),
			NetworkId: sidecar.Id(networkId),
			Trigger:   smtpedgeservice.SmtpHaltTriggerCommunityComplaint,
			StartTime: kept.UntilTime.Add(-smtpedgeservice.SmtpHaltDuration),
		}); err != nil {
			t.Fatal(err)
		}
		if err := effects.ApplyAbuseEffect(ctx, &AbuseEffect{Action: kept}); err != nil {
			t.Fatal(err)
		}
		if halt := row(kept.ActionId); halt == nil || halt.Trigger != smtpedgeservice.SmtpHaltTriggerCommunityComplaint {
			t.Fatalf("SMTP's row %+v", halt)
		}
		if len(mailHints) != 1 {
			t.Fatalf("a halt SMTP's ladder hinted was hinted again: %+v", mailHints)
		}

		liftedFirst := action(model.AbuseActionKindMailHalt, "quietnet.root.example", now.Add(22*time.Hour))
		liftedFirst.ReviewState = model.AbuseReviewStateLiftedWrong
		ended := action(model.AbuseActionKindMailHalt, "quietnet.root.example", now.Add(-time.Minute))
		for _, nothing := range []*model.AbuseAction{liftedFirst, ended} {
			if err := effects.ApplyAbuseEffect(ctx, &AbuseEffect{Action: nothing}); err != nil {
				t.Fatal(err)
			}
			if halt := row(nothing.ActionId); halt != nil {
				t.Fatalf("a halt written for %+v", nothing)
			}
		}

		lifted := *lost
		lifted.ReviewState = model.AbuseReviewStateLiftedWrong
		lift := &model.AbuseAction{
			ActionId:  server.NewId(),
			CaseId:    lost.CaseId,
			NetworkId: &networkId,
			Category:  model.AbuseCategorySpam,
			Kind:      model.AbuseActionKindLift,
			Target:    lost.ActionId.String(),
		}
		if err := effects.ApplyAbuseEffect(ctx, &AbuseEffect{Action: lift, Lifted: &lifted}); err != nil {
			t.Fatal(err)
		}
		if halt := row(lost.ActionId); halt != nil {
			t.Fatalf("the lifted halt's row stands %+v", halt)
		}
		if len(mailHints) != 2 || mailHints[1].kind != protocol.SmtpStatusKind_SMTP_STATUS_CLEAR {
			t.Fatalf("the lift's mail hints %+v", mailHints)
		}
		if halt := row(kept.ActionId); halt == nil {
			t.Fatal("the lift took another halt's row")
		}
		if hints == 0 {
			t.Fatal("no core hint for a standing change")
		}

		// a set of hooks with no SMTP halt hook refuses the mail halts, which stay owed
		withoutSmtp := DefaultAbuseEffectsDeps()
		withoutSmtp.EnsureSmtpHalt = nil
		if err := NewAbuseEffects(withoutSmtp, nil).ApplyAbuseEffect(ctx, &AbuseEffect{Action: action(model.AbuseActionKindMailHalt, "quietnet.root.example", now.Add(time.Hour))}); !errors.Is(err, ErrAbuseEffectsUnavailable) {
			t.Fatalf("with no SMTP hook: %v", err)
		}
	})
}

// One mail hint the effects wrote.
type testAbuseMailHint struct {
	kind       protocol.SmtpStatusKind
	retryAfter time.Duration
	window     time.Duration
}

// A suspension's mail halt (§21.7): its effect holds the network's queues on topic mail for
// SuspensionMailHoldTimeout, and its lift, the last standing suspension's, releases them; a barred case's hints
// nothing, and a hook that fails fails no effect.
func TestAbuseMailSuspensionHoldsTheQueues(t *testing.T) {
	ctx := context.Background()
	owners := newTestAbuseOwners()
	deps := owners.deps()
	mailHints := []testAbuseMailHint{}
	var mailErr error
	deps.SmtpStatusHint = func(ctx context.Context, networkId server.Id, kind protocol.SmtpStatusKind, retryAfter time.Duration, since time.Time, now time.Time) error {
		mailHints = append(mailHints, testAbuseMailHint{kind: kind, retryAfter: retryAfter, window: now.Sub(since)})
		return mailErr
	}
	effects := NewAbuseEffects(deps, nil)
	suspension := owners.action(model.AbuseActionKindNetworkSuspend, owners.label+"."+testAbuseEffectRoot)
	if err := effects.ApplyAbuseEffect(ctx, &AbuseEffect{Action: suspension}); err != nil {
		t.Fatal(err)
	}
	settings := DefaultAbuseEffectsSettings()
	if len(mailHints) != 1 || mailHints[0].kind != protocol.SmtpStatusKind_SMTP_STATUS_HALTED ||
		mailHints[0].retryAfter != settings.SuspensionMailHoldTimeout || mailHints[0].window != settings.MailHintWindow {
		t.Fatalf("the suspension's mail hints %+v", mailHints)
	}
	if err := effects.ApplyAbuseEffect(ctx, testAbuseLiftEffect(suspension, model.AbuseReviewStateLifted)); err != nil {
		t.Fatal(err)
	}
	if len(mailHints) != 2 || mailHints[1].kind != protocol.SmtpStatusKind_SMTP_STATUS_CLEAR {
		t.Fatalf("the lift's mail hints %+v", mailHints)
	}

	barred := owners.action(model.AbuseActionKindNetworkSuspend, owners.label+"."+testAbuseEffectRoot)
	if err := effects.ApplyAbuseEffect(ctx, &AbuseEffect{Action: barred, Barred: true}); err != nil {
		t.Fatal(err)
	}
	if len(mailHints) != 2 {
		t.Fatalf("a barred suspension hinted %+v", mailHints)
	}
	mailErr = errors.New("the hint's script failed")
	failing := owners.action(model.AbuseActionKindNetworkSuspend, owners.label+"."+testAbuseEffectRoot)
	if err := effects.ApplyAbuseEffect(ctx, &AbuseEffect{Action: failing}); err != nil {
		t.Fatalf("a failed mail hint failed the effect: %v", err)
	}
}
