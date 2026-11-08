package controller

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// §21's action path (abuse_action_controller.go), with a recording AbuseEffects
// and mailer and a hook at each step. The store cases need MG5's tables, so
// they run in a build with sidecar_pending; the template's render runs in both.

// Every effect it was asked for, in order. err, when set, fails each.
type testAbuseEffects struct {
	stateLock sync.Mutex
	effects   []*AbuseEffect
	err       error
}

func (self *testAbuseEffects) ApplyAbuseEffect(ctx context.Context, effect *AbuseEffect) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.err != nil {
		return self.err
	}
	self.effects = append(self.effects, effect)
	return nil
}

func (self *testAbuseEffects) applied() []*AbuseEffect {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]*AbuseEffect{}, self.effects...)
}

// Every record it mailed, as the bytes a mailbox would hold before the seal.
type testAbuseRecordMailer struct {
	stateLock sync.Mutex
	bodies    [][]byte
}

func (self *testAbuseRecordMailer) MailAbuseRecord(ctx context.Context, record *AbuseRecord) error {
	body, err := EncodeAbuseRecord(record, testAbuseRecordMacKey)
	if err != nil {
		return err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.bodies = append(self.bodies, body)
	return nil
}

func (self *testAbuseRecordMailer) mailed() [][]byte {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([][]byte{}, self.bodies...)
}

// Installs a recording effects and mailer for the rest of the test.
func newTestAbuseDeps(t testing.TB) (*testAbuseEffects, *testAbuseRecordMailer) {
	effects := &testAbuseEffects{}
	mailer := &testAbuseRecordMailer{}
	restoreEffects := SetAbuseEffects(effects)
	restoreMailer := SetAbuseRecordMailer(mailer)
	t.Cleanup(func() {
		restoreEffects()
		restoreMailer()
	})
	return effects, mailer
}

// Skips a store case in a build that does not link MG5.
func requireAbuseTables(t *testing.T) {
	t.Helper()
	if !server.SidecarMg5Linked() {
		t.Skip("MG5 is pending: these cases run with -tags sidecar_pending")
	}
}

// A network with an admin who signs in by email, and a phishing case against
// one of its hosts.
func newTestAbuseCase(t testing.TB, ctx context.Context, barred bool) (networkId server.Id, adminUserAuth string, caseId server.Id) {
	t.Helper()
	networkId = server.NewId()
	adminUserAuth = model.Testing_CreateNetwork(ctx, networkId, "abuse-"+networkId.String()[:8], server.NewId())
	var err error
	server.Tx(ctx, func(tx server.PgTx) {
		caseId, _, err = model.OpenAbuseCaseInTx(ctx, tx, &model.AbuseCaseArgs{
			NetworkId:     &networkId,
			Category:      model.AbuseCategoryPhishing,
			Target:        model.AbuseTarget{Kind: model.AbuseTargetKindHost, Name: "shop.root.example"},
			ReporterClass: model.AbuseReporterClassWebRisk,
			JoinKind:      model.AbuseJoinKindHost,
			JoinKey:       "shop.root.example",
			Barred:        barred,
		}, server.NowUtc(), model.DefaultAbuseSettings())
	})
	if err != nil {
		t.Fatal(err)
	}
	return
}

// A machine's host kill on the case.
func testAbuseKillArgs(caseId server.Id) *model.AbuseActionArgs {
	return &model.AbuseActionArgs{
		ActionId: server.NewId(),
		CaseId:   caseId,
		Kind:     model.AbuseActionKindHostKill,
		Target:   "shop.root.example",
	}
}

// The notices owed to a network, by recipient and fields.
func testAbuseNotices(t testing.TB, ctx context.Context, networkId server.Id) (userAuths []string, templates []*AbuseActionNoticeTemplate) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `
			SELECT user_auth, template_json::text
			FROM account_message_outbox
			WHERE network_id = $1 AND template_name = 'abuse_action_notice'
			ORDER BY create_time, message_id
		`, networkId)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var userAuth, templateJson string
				server.Raise(result.Scan(&userAuth, &templateJson))
				template := &AbuseActionNoticeTemplate{}
				server.Raise(json.Unmarshal([]byte(templateJson), template))
				userAuths = append(userAuths, userAuth)
				templates = append(templates, template)
			}
		})
	})
	return
}

// The admin events of a network, as actions.
func testAbuseAdminEvents(t testing.TB, ctx context.Context, networkId server.Id) (actions []string) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `
			SELECT action || ' ' || target_kind
			FROM network_admin_event
			WHERE network_id = $1
			ORDER BY seq
		`, networkId)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var action string
				server.Raise(result.Scan(&action))
				actions = append(actions, action)
			}
		})
	})
	return
}

// The pass with no grace, as a minute later would run it.
func testRunAbuseActionPass(ctx context.Context) *AbuseActionPassResult {
	settings := model.DefaultAbuseSettings()
	settings.ActionPassGrace = 0
	return RetryOwedAbuseActions(ctx, server.NowUtc().Add(time.Second), settings)
}

// Runs ApplyAbuseAction on its own goroutine, ending that goroutine as a crash
// would at step, and waits for it to end.
func testApplyAbuseActionCrashingAt(t testing.TB, ctx context.Context, args *model.AbuseActionArgs, step string) {
	t.Helper()
	restoreHook := Testing_SetAbuseActionHook(func(hookStep string, actionId server.Id) {
		if hookStep == step && actionId == args.ActionId {
			runtime.Goexit()
		}
	})
	defer restoreHook()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := ApplyAbuseAction(ctx, args); err != nil {
			t.Error(err)
		}
	}()
	<-done
}

// The path in order: commit, effect, mail, each marked once it is done, the
// steps seen at their barriers in that order.
func TestApplyAbuseActionCommitsThenAppliesThenMails(t *testing.T) {
	requireAbuseTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		effects, mailer := newTestAbuseDeps(t)
		_, _, caseId := newTestAbuseCase(t, ctx, false)
		args := testAbuseKillArgs(caseId)

		steps := []string{}
		var stepsLock sync.Mutex
		defer Testing_SetAbuseActionHook(func(step string, actionId server.Id) {
			stepsLock.Lock()
			defer stepsLock.Unlock()
			steps = append(steps, step)
		})()

		result, err := ApplyAbuseAction(ctx, args)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Inserted || !result.Applied || !result.Mailed {
			t.Fatalf("result %+v", result)
		}
		want := []string{
			AbuseActionStepCommitted, AbuseActionStepEffectBefore, AbuseActionStepEffectAfter,
			AbuseActionStepMailBefore, AbuseActionStepMailAfter,
		}
		if strings.Join(steps, ",") != strings.Join(want, ",") {
			t.Fatalf("steps %v, want %v", steps, want)
		}
		action := model.GetAbuseAction(ctx, args.ActionId)
		if action.AppliedTime == nil || action.MailedTime == nil {
			t.Fatalf("the row reads %+v", action)
		}
		if got := effects.applied(); len(got) != 1 || got[0].Action.ActionId != args.ActionId || got[0].Lifted != nil || got[0].Barred {
			t.Fatalf("effects %+v", got)
		}
		bodies := mailer.mailed()
		if len(bodies) != 1 {
			t.Fatalf("%d records mailed", len(bodies))
		}
		record, err := ParseAbuseRecord(bodies[0], testAbuseRecordMacKey)
		if err != nil {
			t.Fatal(err)
		}
		if record.ActionId != args.ActionId || record.CaseId != caseId || record.Kind != model.AbuseActionKindHostKill ||
			record.Target != "shop.root.example" || record.Category != model.AbuseCategoryPhishing {
			t.Fatalf("record %+v", record)
		}
	})
}

// A crash after commit and before the effect leaves the row owed, and the pass
// runs the effect once and mails once; a pass after that does nothing.
func TestAbuseActionCrashBeforeTheEffectRerunsOnThePass(t *testing.T) {
	requireAbuseTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		effects, mailer := newTestAbuseDeps(t)
		_, _, caseId := newTestAbuseCase(t, ctx, false)
		args := testAbuseKillArgs(caseId)

		testApplyAbuseActionCrashingAt(t, ctx, args, AbuseActionStepEffectBefore)
		action := model.GetAbuseAction(ctx, args.ActionId)
		if action == nil || action.AppliedTime != nil || action.MailedTime != nil {
			t.Fatalf("after the crash the row reads %+v", action)
		}
		if len(effects.applied()) != 0 || len(mailer.mailed()) != 0 {
			t.Fatal("the crash ran a step")
		}

		result := testRunAbuseActionPass(ctx)
		if result.AppliedCount != 1 || result.MailedCount != 1 || result.OwedCount != 0 {
			t.Fatalf("the pass %+v", result)
		}
		if got := effects.applied(); len(got) != 1 || got[0].Action.ActionId != args.ActionId {
			t.Fatalf("effects %+v", got)
		}
		if len(mailer.mailed()) != 1 {
			t.Fatalf("%d records mailed", len(mailer.mailed()))
		}
		if result := testRunAbuseActionPass(ctx); result.AppliedCount != 0 || result.MailedCount != 0 {
			t.Fatalf("a second pass %+v", result)
		}
		if len(effects.applied()) != 1 || len(mailer.mailed()) != 1 {
			t.Fatal("a second pass ran a step again")
		}
	})
}

// A crash after the effect and before the mail mails once on the pass, and runs
// no second effect.
func TestAbuseActionCrashBeforeTheMailMailsOnce(t *testing.T) {
	requireAbuseTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		effects, mailer := newTestAbuseDeps(t)
		_, _, caseId := newTestAbuseCase(t, ctx, false)
		args := testAbuseKillArgs(caseId)

		testApplyAbuseActionCrashingAt(t, ctx, args, AbuseActionStepMailBefore)
		action := model.GetAbuseAction(ctx, args.ActionId)
		if action.AppliedTime == nil || action.MailedTime != nil {
			t.Fatalf("after the crash the row reads %+v", action)
		}
		result := testRunAbuseActionPass(ctx)
		if result.AppliedCount != 0 || result.MailedCount != 1 {
			t.Fatalf("the pass %+v", result)
		}
		if len(effects.applied()) != 1 || len(mailer.mailed()) != 1 {
			t.Fatalf("%d effects, %d mails", len(effects.applied()), len(mailer.mailed()))
		}
	})
}

// The same action id twice is one row and one effect: the second call, a retry
// or a restore, finds the row done and does nothing.
func TestApplyAbuseActionTwiceIsOneEffect(t *testing.T) {
	requireAbuseTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		effects, mailer := newTestAbuseDeps(t)
		networkId, _, caseId := newTestAbuseCase(t, ctx, false)
		args := testAbuseKillArgs(caseId)
		if _, err := ApplyAbuseAction(ctx, args); err != nil {
			t.Fatal(err)
		}
		result, err := ApplyAbuseAction(ctx, args)
		if err != nil {
			t.Fatal(err)
		}
		if result.Inserted || !result.Applied || !result.Mailed {
			t.Fatalf("the second call %+v", result)
		}
		if len(model.GetAbuseCaseActions(ctx, caseId)) != 1 || len(effects.applied()) != 1 || len(mailer.mailed()) != 1 {
			t.Fatalf("rows, effects and mails are not one each")
		}
		if userAuths, _ := testAbuseNotices(t, ctx, networkId); len(userAuths) != 1 {
			t.Fatalf("%d notices", len(userAuths))
		}
	})
}

// Each Owner and Admin is owed one notice with the category, target, case id and
// appeal link, and the network one admin event. Until TEAM-M6-02 that is the
// network's one admin.
func TestApplyAbuseActionNoticesEachOwnerAndAdmin(t *testing.T) {
	requireAbuseTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		newTestAbuseDeps(t)
		defer server.Testing_PushAbuseConfig("appeal_url: \"https://ur.io/abuse/appeal\"\n")()
		networkId, adminUserAuth, caseId := newTestAbuseCase(t, ctx, false)
		args := testAbuseKillArgs(caseId)
		if _, err := ApplyAbuseAction(ctx, args); err != nil {
			t.Fatal(err)
		}
		userAuths, templates := testAbuseNotices(t, ctx, networkId)
		if len(userAuths) != 1 || userAuths[0] != adminUserAuth {
			t.Fatalf("notices to %v, want one to the admin", userAuths)
		}
		template := templates[0]
		if template.CaseId != caseId.String() || template.Category != string(model.AbuseCategoryPhishing) ||
			template.Kind != string(model.AbuseActionKindHostKill) || template.Target != "shop.root.example" ||
			template.AppealUrl != "https://ur.io/abuse/appeal?case_id="+caseId.String() {
			t.Fatalf("the notice holds %+v", template)
		}
		if events := testAbuseAdminEvents(t, ctx, networkId); len(events) != 1 || events[0] != "abuse.action abuse_case" {
			t.Fatalf("admin events %v", events)
		}
	})
}

// A barred case owes no notice and no admin event, and its effect is told so,
// so it sends no hint; the deny and the off-site record still happen.
func TestApplyAbuseActionOnABarredCaseRevealsNothing(t *testing.T) {
	requireAbuseTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		effects, mailer := newTestAbuseDeps(t)
		networkId, _, caseId := newTestAbuseCase(t, ctx, true)
		result, err := ApplyAbuseAction(ctx, testAbuseKillArgs(caseId))
		if err != nil {
			t.Fatal(err)
		}
		if !result.Applied || !result.Mailed {
			t.Fatalf("result %+v", result)
		}
		if userAuths, _ := testAbuseNotices(t, ctx, networkId); len(userAuths) != 0 {
			t.Fatalf("a barred case owed %d notices", len(userAuths))
		}
		if events := testAbuseAdminEvents(t, ctx, networkId); len(events) != 0 {
			t.Fatalf("a barred case wrote admin events %v", events)
		}
		if got := effects.applied(); len(got) != 1 || !got[0].Barred {
			t.Fatalf("effects %+v", got)
		}
		if len(mailer.mailed()) != 1 {
			t.Fatal("a barred case's action was not recorded off site")
		}
	})
}

// A lift goes through the case: its own row, effect, notice, admin event and
// record, the effect told what it undoes. A lift of an action the case does not
// hold is refused and writes nothing.
func TestLiftAbuseActionThroughTheCase(t *testing.T) {
	requireAbuseTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		effects, mailer := newTestAbuseDeps(t)
		networkId, _, caseId := newTestAbuseCase(t, ctx, false)
		args := testAbuseKillArgs(caseId)
		if _, err := ApplyAbuseAction(ctx, args); err != nil {
			t.Fatal(err)
		}

		reviewerId := server.NewId()
		if _, err := LiftAbuseAction(ctx, &model.AbuseLiftArgs{
			ActionId:       server.NewId(),
			CaseId:         caseId,
			LiftedActionId: server.NewId(),
			ReviewState:    model.AbuseReviewStateLifted,
			ReviewerId:     &reviewerId,
		}); !errors.Is(err, model.ErrAbuseActionNotFound) {
			t.Fatalf("a lift of an unknown action: %v", err)
		}

		liftId := server.NewId()
		result, err := LiftAbuseAction(ctx, &model.AbuseLiftArgs{
			ActionId:       liftId,
			CaseId:         caseId,
			LiftedActionId: args.ActionId,
			ReviewState:    model.AbuseReviewStateLiftedWrong,
			ReviewerId:     &reviewerId,
			ReviewReason:   "not phishing",
		})
		if err != nil {
			t.Fatal(err)
		}
		if !result.Inserted || !result.Applied || !result.Mailed || result.Action.Kind != model.AbuseActionKindLift {
			t.Fatalf("result %+v", result)
		}
		got := effects.applied()
		if len(got) != 2 || got[1].Action.ActionId != liftId || got[1].Lifted == nil || got[1].Lifted.ActionId != args.ActionId {
			t.Fatalf("effects %+v", got)
		}
		bodies := mailer.mailed()
		if len(bodies) != 2 {
			t.Fatalf("%d records", len(bodies))
		}
		record, err := ParseAbuseRecord(bodies[1], testAbuseRecordMacKey)
		if err != nil {
			t.Fatal(err)
		}
		if record.ActionId != liftId || record.Kind != model.AbuseActionKindLift || record.Target != args.ActionId.String() {
			t.Fatalf("the lift's record %+v", record)
		}
		_, templates := testAbuseNotices(t, ctx, networkId)
		if len(templates) != 2 || !templates[1].Lift() || templates[1].LiftedKind != string(model.AbuseActionKindHostKill) ||
			templates[1].Target != "shop.root.example" {
			t.Fatalf("notices %+v", templates)
		}
		if events := testAbuseAdminEvents(t, ctx, networkId); len(events) != 2 || events[1] != "abuse.lift abuse_case" {
			t.Fatalf("admin events %v", events)
		}
		if action := model.GetAbuseAction(ctx, args.ActionId); action.ReviewState != model.AbuseReviewStateLiftedWrong {
			t.Fatalf("the lifted action reads %+v", action)
		}
	})
}

// Before ABUSE-M5-06's effects and the sealed mailer exist, an action commits
// and reads as neither applied nor mailed, and the pass leaves it owed.
func TestAbuseActionStaysOwedWithoutEffectsOrMailer(t *testing.T) {
	requireAbuseTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer server.Testing_PushAbuseConfig("abuse_mailbox: records@mailbox.example\n")()
		_, _, caseId := newTestAbuseCase(t, ctx, false)
		args := testAbuseKillArgs(caseId)
		result, err := ApplyAbuseAction(ctx, args)
		if err != nil {
			t.Fatal(err)
		}
		if result.Applied || result.Mailed {
			t.Fatalf("result %+v", result)
		}
		pass := testRunAbuseActionPass(ctx)
		if pass.AppliedCount != 0 || pass.MailedCount != 0 || pass.OwedCount != 1 {
			t.Fatalf("the pass %+v", pass)
		}
		if action := model.GetAbuseAction(ctx, args.ActionId); action.AppliedTime != nil || action.MailedTime != nil {
			t.Fatalf("the row reads %+v", action)
		}
	})
}

// An effect that fails still leaves the record mailed, since a restore needs
// the record most, and the pass applies the effect once it works.
func TestAbuseActionFailedEffectStillMails(t *testing.T) {
	requireAbuseTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		effects, mailer := newTestAbuseDeps(t)
		effects.err = errors.New("synthetic effect failure")
		_, _, caseId := newTestAbuseCase(t, ctx, false)
		result, err := ApplyAbuseAction(ctx, testAbuseKillArgs(caseId))
		if err != nil {
			t.Fatal(err)
		}
		if result.Applied || !result.Mailed || len(mailer.mailed()) != 1 {
			t.Fatalf("result %+v", result)
		}
		effects.stateLock.Lock()
		effects.err = nil
		effects.stateLock.Unlock()
		if pass := testRunAbuseActionPass(ctx); pass.AppliedCount != 1 || pass.MailedCount != 0 {
			t.Fatalf("the pass %+v", pass)
		}
	})
}

// The notice renders through the shared layout for an action and a lift: one
// subject line, every link on ur.io, no markup in the text part, and the case,
// category and target in both parts.
func TestAbuseActionNoticeTemplateRenders(t *testing.T) {
	caseId := server.NewId()
	until := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	for _, template := range []*AbuseActionNoticeTemplate{
		{
			CaseId:    caseId.String(),
			Category:  string(model.AbuseCategoryPhishing),
			Kind:      string(model.AbuseActionKindHostKill),
			Target:    "shop.root.example",
			UntilTime: &until,
			AppealUrl: "https://ur.io/abuse/appeal?case_id=" + caseId.String(),
		},
		{
			CaseId:   caseId.String(),
			Category: string(model.AbuseCategoryCopyright),
			Kind:     string(model.AbuseActionKindNoticeForward),
			Target:   "files.root.example",
		},
		{
			CaseId:     caseId.String(),
			Category:   string(model.AbuseCategoryPhishing),
			Kind:       string(model.AbuseActionKindLift),
			LiftedKind: string(model.AbuseActionKindHostKill),
			Target:     "shop.root.example",
		},
	} {
		subject, bodyHtml, bodyText, err := RenderEmailTemplate(template)
		if err != nil {
			t.Fatal(err)
		}
		if subject == "" || strings.ContainsAny(subject, "\r\n") || subject != template.Subject() {
			t.Errorf("subject %q", subject)
		}
		for _, part := range []string{bodyHtml, bodyText} {
			if strings.Contains(part, "{{") {
				t.Errorf("an unresolved action in %q", part)
			}
			for _, want := range []string{caseId.String(), template.Target, template.CategoryText(), template.ActionText()} {
				if !strings.Contains(part, want) {
					t.Errorf("%s: a part lacks %q", template.Kind, want)
				}
			}
		}
		if strings.Contains(bodyText, "<") || strings.Contains(bodyText, "&amp;") {
			t.Errorf("the text part holds markup")
		}
		for _, href := range strings.Split(bodyHtml, `href="`)[1:] {
			link := href[:strings.Index(href, `"`)]
			onUr := link == "https://ur.io" || strings.HasPrefix(link, "https://ur.io/")
			if !onUr && !(strings.HasPrefix(link, "mailto:") && strings.HasSuffix(link, "@ur.io")) {
				t.Errorf("a link leaves ur.io: %q", link)
			}
		}
		if template.Lift() && strings.Contains(bodyText, "appeal") {
			t.Errorf("a lift's notice offers an appeal")
		}
		if sms, err := RenderSmsTemplate(template); err != nil || sms != bodyText {
			t.Errorf("the notice has an sms body of its own: %v", err)
		}
	}
}
