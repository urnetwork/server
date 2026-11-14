package controller

// §21's one path for every action and lift (ABUSE-M5-05). In order:
//
//  1. One transaction records the row under its caller's action_id, where a
//     second insert of that id adds nothing (which is what makes a retry and a
//     restore idempotent), sets the case's review clock for a machine's action,
//     and owes each Owner and Admin AbuseActionNoticeTemplate through the account
//     message outbox, plus an admin event, unless the case is barred.
//  2. After commit, AbuseEffects runs, and applied_time is set.
//  3. Then §21.10's record is mailed (abuse_record_mail.go), and mailed_time is
//     set.
//
// A crash between the commit and either step leaves the row owed, and
// AbuseActionPass, each minute, finishes it (RetryOwedAbuseActions). So effects
// are idempotent: one may run twice. An effect or mail that fails is left to the
// pass too, and the record goes even when the effect failed, since the record is
// what a restore needs. No step holds a Postgres transaction across the effect
// or the mail (CODESTYLE's Locking).
//
// A barred case (a sealing order or CSAM, §21.8) writes no notice, no admin
// event and no hint, and an action on it reveals nothing beyond the deny itself:
// AbuseEffect.Barred carries that to the effects. No statement is signed: a
// server originates only denials (I-4). Logs and metrics carry no network id
// and no target.
//
// The process installs its effects (InstallAbuseEffects, abuse_effects.go) and
// its record mailer at startup. Until then every effect and mail refuses, each
// action stays owed with applied_time or mailed_time NULL, and the pass keeps
// retrying: nothing reads as applied or mailed that was not.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The steps around an action's linearization points, which a test hook sees in
// this order.
const (
	AbuseActionStepCommitted    = "abuse.commit.after"
	AbuseActionStepEffectBefore = "abuse.effect.before"
	AbuseActionStepEffectAfter  = "abuse.effect.after"
	AbuseActionStepMailBefore   = "abuse.mail.before"
	AbuseActionStepMailAfter    = "abuse.mail.after"
)

// The minimum time between two default-level reports of a failed effect or mail.
const abuseActionReportInterval = 1 * time.Minute

// One action's effect, as the effects take it.
type AbuseEffect struct {
	Action *model.AbuseAction
	// the action a lift undoes; nil for any other kind
	Lifted *model.AbuseAction
	// the case is barred: send no hint and reveal nothing beyond the deny
	Barred bool
}

// What an action does outside Postgres: TLS's kill switch, DNS's suspensions,
// §8's hard deny, SMTP's halts and their hints (ABUSE-M5-06, ABUSE-M9-01). Safe
// for concurrent use. ApplyAbuseEffect is idempotent, since a crash between
// commit and effect runs it again, from the request or the action pass, and it
// holds no Postgres transaction across Redis or object I/O. A kind with nothing
// to do outside Postgres, such as a forwarded notice, answers nil.
type AbuseEffects interface {
	ApplyAbuseEffect(ctx context.Context, effect *AbuseEffect) error
}

// No effects are installed, so the action stays owed.
var ErrAbuseEffectsUnavailable = errors.New("abuse effects unavailable")

// What a process has before it installs the real effects
// (InstallAbuseEffects): it refuses every effect, so no action reads as
// applied.
type unavailableAbuseEffects struct{}

// Refuses: see the type.
func (self *unavailableAbuseEffects) ApplyAbuseEffect(ctx context.Context, effect *AbuseEffect) error {
	return ErrAbuseEffectsUnavailable
}

// The process's effects, record mailer and test hook, set at startup.
var abuseActionDeps = struct {
	stateLock sync.Mutex
	effects   AbuseEffects
	mailer    AbuseRecordMailer
	hook      func(step string, actionId server.Id)
}{
	effects: &unavailableAbuseEffects{},
	mailer:  &sesAbuseRecordMailer{},
}

// The effects, mailer and hook in force now.
func currentAbuseActionDeps() (AbuseEffects, AbuseRecordMailer, func(step string, actionId server.Id)) {
	abuseActionDeps.stateLock.Lock()
	defer abuseActionDeps.stateLock.Unlock()
	return abuseActionDeps.effects, abuseActionDeps.mailer, abuseActionDeps.hook
}

// Installs the process's effects, returning what puts the previous ones back.
func SetAbuseEffects(effects AbuseEffects) (restore func()) {
	abuseActionDeps.stateLock.Lock()
	defer abuseActionDeps.stateLock.Unlock()
	previous := abuseActionDeps.effects
	abuseActionDeps.effects = effects
	return func() {
		abuseActionDeps.stateLock.Lock()
		defer abuseActionDeps.stateLock.Unlock()
		abuseActionDeps.effects = previous
	}
}

// Installs the process's record mailer, returning what puts the previous one
// back.
func SetAbuseRecordMailer(mailer AbuseRecordMailer) (restore func()) {
	abuseActionDeps.stateLock.Lock()
	defer abuseActionDeps.stateLock.Unlock()
	previous := abuseActionDeps.mailer
	abuseActionDeps.mailer = mailer
	return func() {
		abuseActionDeps.stateLock.Lock()
		defer abuseActionDeps.stateLock.Unlock()
		abuseActionDeps.mailer = previous
	}
}

// Calls hook at each AbuseActionStep until the returned restore: how a test
// holds an action at a step, or ends its goroutine there as a crash would.
func Testing_SetAbuseActionHook(hook func(step string, actionId server.Id)) (restore func()) {
	abuseActionDeps.stateLock.Lock()
	defer abuseActionDeps.stateLock.Unlock()
	previous := abuseActionDeps.hook
	abuseActionDeps.hook = hook
	return func() {
		abuseActionDeps.stateLock.Lock()
		defer abuseActionDeps.stateLock.Unlock()
		abuseActionDeps.hook = previous
	}
}

// Counts effects and mails by step and result (ok, failed, unavailable).
var abuseActionStepCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "abuse",
		Name:      "action_steps_total",
		Help:      "Abuse action effects and record mails after commit, by step and result",
	},
	[]string{"step", "result"},
)

// Registers the step counter with the default registry.
func init() {
	prometheus.MustRegister(abuseActionStepCounter)
}

// The default-level reports of failed effects and mails.
var (
	abuseEffectFailures = newBoundedReport(abuseActionReportInterval)
	abuseMailFailures   = newBoundedReport(abuseActionReportInterval)
)

// What became of an action.
type AbuseActionResult struct {
	Action *model.AbuseAction
	// false when the action id had its row already: a retry or a restore
	Inserted bool
	// whether its effect has run, now or before
	Applied bool
	// whether its record has been mailed, now or before
	Mailed bool
	// for a lift of a revocation: the revocation stands, since nothing returns
	// a revoked certificate, so the network's devices order new ones (§21.6).
	// What the reviewer is told.
	RevocationStands bool
}

// Records and applies one action under its case, the path in the file header.
// The error is the commit's alone: an effect or mail that fails leaves the
// action owed, and the result says so. A binary that does not link MG5 records
// nothing and answers model.ErrAbuseUnlinked.
func ApplyAbuseAction(ctx context.Context, args *model.AbuseActionArgs) (*AbuseActionResult, error) {
	if !server.SidecarMg5Linked() {
		return nil, model.ErrAbuseUnlinked
	}
	settings := model.DefaultAbuseSettings()
	var action *model.AbuseAction
	var abuseCase *model.AbuseCase
	var inserted bool
	var err error
	server.Tx(ctx, func(tx server.PgTx) {
		action, abuseCase, inserted, err = model.RecordAbuseActionInTx(ctx, tx, args, server.NowUtc(), settings)
		if err == nil && inserted {
			oweAbuseActionNoticeInTx(ctx, tx, abuseCase, action, nil)
		}
	})
	if err != nil {
		return nil, err
	}
	abuseActionStep(AbuseActionStepCommitted, action.ActionId)
	result := &AbuseActionResult{Action: action, Inserted: inserted}
	result.Applied, result.Mailed, _, _ = completeAbuseAction(ctx, abuseCase.Barred, action, nil, true, true)
	return result, nil
}

// Lifts one action through its case, on the same path as ApplyAbuseAction with
// the lift's own id and kind lift (plan-ABUSE-small-readings #11). A lift of an
// action the case does not hold is refused with model.ErrAbuseActionNotFound.
func LiftAbuseAction(ctx context.Context, args *model.AbuseLiftArgs) (*AbuseActionResult, error) {
	if !server.SidecarMg5Linked() {
		return nil, model.ErrAbuseUnlinked
	}
	var lift *model.AbuseAction
	var lifted *model.AbuseAction
	var abuseCase *model.AbuseCase
	var inserted bool
	var err error
	server.Tx(ctx, func(tx server.PgTx) {
		lift, lifted, abuseCase, inserted, err = model.RecordAbuseLiftInTx(ctx, tx, args, server.NowUtc())
		if err == nil && inserted {
			oweAbuseActionNoticeInTx(ctx, tx, abuseCase, lift, lifted)
		}
	})
	if err != nil {
		return nil, err
	}
	abuseActionStep(AbuseActionStepCommitted, lift.ActionId)
	result := &AbuseActionResult{
		Action:           lift,
		Inserted:         inserted,
		RevocationStands: lifted.Kind == model.AbuseActionKindCertRevoke,
	}
	result.Applied, result.Mailed, _, _ = completeAbuseAction(ctx, abuseCase.Barred, lift, lifted, true, true)
	return result, nil
}

// Calls the test hook, if any, with no lock held.
func abuseActionStep(step string, actionId server.Id) {
	_, _, hook := currentAbuseActionDeps()
	if hook != nil {
		hook(step, actionId)
	}
}

// Runs the steps after commit that the row still owes and the caller tries:
// its effect, then its record. Each is marked done only once it succeeded. The
// errors are each step's failure, nil when it succeeded, was done already or
// was not tried.
func completeAbuseAction(
	ctx context.Context,
	barred bool,
	action *model.AbuseAction,
	lifted *model.AbuseAction,
	tryEffect bool,
	tryMail bool,
) (applied bool, mailed bool, effectErr error, mailErr error) {
	effects, mailer, _ := currentAbuseActionDeps()

	applied = action.AppliedTime != nil
	if !applied && tryEffect {
		abuseActionStep(AbuseActionStepEffectBefore, action.ActionId)
		effectErr = effects.ApplyAbuseEffect(ctx, &AbuseEffect{Action: action, Lifted: lifted, Barred: barred})
		if effectErr == nil {
			model.SetAbuseActionApplied(ctx, action.ActionId, server.NowUtc())
			applied = true
		}
		countAbuseActionStep("effect", effectErr, ErrAbuseEffectsUnavailable, abuseEffectFailures)
		abuseActionStep(AbuseActionStepEffectAfter, action.ActionId)
	}

	mailed = action.MailedTime != nil
	if !mailed && tryMail {
		abuseActionStep(AbuseActionStepMailBefore, action.ActionId)
		mailErr = mailer.MailAbuseRecord(ctx, AbuseRecordOf(action))
		if mailErr == nil {
			model.SetAbuseActionMailed(ctx, action.ActionId, server.NowUtc())
			mailed = true
		}
		countAbuseActionStep("mail", mailErr, ErrAbuseRecordMailerUnavailable, abuseMailFailures)
		abuseActionStep(AbuseActionStepMailAfter, action.ActionId)
	}
	return
}

// Counts one step's result and reports a failure at most once per interval,
// naming the step and no id.
func countAbuseActionStep(step string, err error, unavailable error, report *boundedReport) {
	result := "ok"
	switch {
	case err == nil:
	case errors.Is(err, unavailable):
		result = "unavailable"
	default:
		result = "failed"
	}
	abuseActionStepCounter.WithLabelValues(step, result).Inc()
	if err == nil {
		return
	}
	if suppressedCount, ok := report.Allow(time.Now()); ok {
		glog.Infof("[abuse]an action's %s is owed (%s), and the action pass retries it; %d more since the last report\n", step, result, suppressedCount)
	}
	if glog.V(1) {
		glog.Infof("[abuse]an action's %s failed: %s\n", step, err)
	}
}

// The admin event words of an action and a lift: fixed words, never input.
const (
	adminEventActionAbuseAction = "abuse.action"
	adminEventActionAbuseLift   = "abuse.lift"
	adminEventTargetAbuseCase   = "abuse_case"
)

// Owes the notice of an action or lift to each Owner and Admin, and its admin
// event, in the action's own transaction, unless the case is barred or names
// no network. The notice key is the action and the recipient, so a rerun owes
// one notice each.
func oweAbuseActionNoticeInTx(
	ctx context.Context,
	tx server.PgTx,
	abuseCase *model.AbuseCase,
	action *model.AbuseAction,
	lifted *model.AbuseAction,
) {
	if abuseCase.Barred || abuseCase.NetworkId == nil {
		return
	}
	networkId := *abuseCase.NetworkId
	template := &AbuseActionNoticeTemplate{
		CaseId:    abuseCase.CaseId.String(),
		Category:  string(action.Category),
		Kind:      string(action.Kind),
		Target:    action.Target,
		UntilTime: action.UntilTime,
		AppealUrl: abuseAppealUrl(server.GetAbuseConfig().AppealUrl(), abuseCase.CaseId),
	}
	eventAction := adminEventActionAbuseAction
	if lifted != nil {
		template.LiftedKind = string(lifted.Kind)
		template.Target = lifted.Target
		template.UntilTime = nil
		eventAction = adminEventActionAbuseLift
	}
	messages := []*accountMessage{}
	for _, recipient := range model.AbuseNoticeRecipientsInTx(ctx, tx, networkId) {
		messages = append(messages, &accountMessage{
			key:       action.ActionId.String() + "/" + recipient,
			networkId: &networkId,
			userAuth:  recipient,
			template:  template,
		})
	}
	addAccountMessagesInTx(ctx, tx, messages)

	caseId := abuseCase.CaseId
	model.AddAdminEventInTx(ctx, tx, &model.AdminEvent{
		NetworkId:  networkId,
		ActorKind:  model.AdminEventActorSystem,
		Action:     eventAction,
		TargetKind: adminEventTargetAbuseCase,
		TargetId:   &caseId,
		Result:     model.AdminEventResultOk,
	})
}

// The appeal link of a case: abuse.yml's appeal_url with the case id, an opaque
// id (I-13), or "" when the file names none.
func abuseAppealUrl(appealUrl string, caseId server.Id) string {
	if appealUrl == "" {
		return ""
	}
	separator := "?"
	if strings.Contains(appealUrl, "?") {
		separator = "&"
	}
	return appealUrl + separator + "case_id=" + caseId.String()
}

// What one pass of AbuseActionPass did.
type AbuseActionPassResult struct {
	AppliedCount int
	MailedCount  int
	// owed actions the pass reached and could not finish
	OwedCount int
}

// One pass over actions whose effect or record is owed: those older than
// ActionPassGrace, oldest first, by a cursor in batches of ActionPassBatchSize,
// until none is left, ActionPassMaxTime has passed or ctx ends. An effect or
// mail that answers unavailable stops that step for the rest of the pass, since
// each further try would fail the same way. A binary that does not link MG5
// does nothing.
func RetryOwedAbuseActions(ctx context.Context, now time.Time, settings *model.AbuseSettings) *AbuseActionPassResult {
	result := &AbuseActionPassResult{}
	if !server.SidecarMg5Linked() {
		return result
	}
	startTime := time.Now()
	createdBefore := now.Add(-settings.ActionPassGrace)
	effectsUnavailable := false
	mailerUnavailable := false
	cursor := model.AbuseActionCursor{}
	for {
		if ctx.Err() != nil || settings.ActionPassMaxTime <= time.Since(startTime) {
			return result
		}
		actions, next := model.ListOwedAbuseActions(ctx, createdBefore, cursor, settings.ActionPassBatchSize)
		if len(actions) == 0 {
			return result
		}
		cursor = next
		for _, action := range actions {
			if ctx.Err() != nil || (effectsUnavailable && mailerUnavailable) {
				return result
			}
			abuseCase := model.GetAbuseCase(ctx, action.CaseId)
			var lifted *model.AbuseAction
			if action.Kind == model.AbuseActionKindLift {
				if liftedId, err := server.ParseId(action.Target); err == nil {
					lifted = model.GetAbuseAction(ctx, liftedId)
				}
			}
			if abuseCase == nil || (action.Kind == model.AbuseActionKindLift && lifted == nil) {
				// a row whose case or lifted action is gone cannot be finished
				result.OwedCount += 1
				continue
			}
			wasApplied := action.AppliedTime != nil
			wasMailed := action.MailedTime != nil
			applied, mailed, effectErr, mailErr := completeAbuseAction(
				ctx, abuseCase.Barred, action, lifted, !effectsUnavailable, !mailerUnavailable,
			)
			if applied && !wasApplied {
				result.AppliedCount += 1
			}
			if mailed && !wasMailed {
				result.MailedCount += 1
			}
			if !applied || !mailed {
				result.OwedCount += 1
			}
			if errors.Is(effectErr, ErrAbuseEffectsUnavailable) {
				effectsUnavailable = true
			}
			if errors.Is(mailErr, ErrAbuseRecordMailerUnavailable) {
				mailerUnavailable = true
			}
		}
	}
}

// The account message an action or lift owes each Owner and Admin (§21.8's
// notice): category, target, case id and appeal link, never the reporter, the
// report or a reviewer. Email only: a phone number gets no SMS, as only account
// security goes by text.
type AbuseActionNoticeTemplate struct {
	CaseId   string
	Category string
	Kind     string
	// for a lift, the lifted action's kind; empty otherwise
	LiftedKind string
	// the host, label, sending domain or network acted on; for a lift, the
	// lifted action's
	Target    string
	UntilTime *time.Time
	// the case's appeal link, empty when abuse.yml names none
	AppealUrl string
	BaseTemplate
}

// The template's files under email_templates.
func (self *AbuseActionNoticeTemplate) Name() string {
	return "abuse_action_notice"
}

// Whether the notice is of a lift.
func (self *AbuseActionNoticeTemplate) Lift() bool {
	return self.Kind == string(model.AbuseActionKindLift)
}

// The subject: what happened, with no target.
func (self *AbuseActionNoticeTemplate) Subject() string {
	if self.Lift() {
		return "An abuse action on your URnetwork network was lifted"
	}
	if self.Kind == string(model.AbuseActionKindNoticeForward) {
		return "A copyright notice about your URnetwork network"
	}
	return "An abuse action on your URnetwork network"
}

// The category in words.
func (self *AbuseActionNoticeTemplate) CategoryText() string {
	switch model.AbuseCategory(self.Category) {
	case model.AbuseCategorySpam:
		return "Spam"
	case model.AbuseCategoryPhishing:
		return "Phishing"
	case model.AbuseCategoryMalware:
		return "Malware"
	case model.AbuseCategoryCommandAndControl:
		return "Command and control"
	case model.AbuseCategoryCopyright:
		return "Copyright"
	case model.AbuseCategoryExposedPanel:
		return "An exposed agent or admin panel"
	case model.AbuseCategoryCompromise:
		return "A compromised device"
	case model.AbuseCategoryThreatToLife:
		return "A threat to life"
	case model.AbuseCategoryBrand:
		return "A brand name held for review"
	}
	return "Other"
}

// An action kind in words.
func abuseActionKindText(kind string) string {
	switch model.AbuseActionKind(kind) {
	case model.AbuseActionKindNoticeForward:
		return "Copyright notice forwarded"
	case model.AbuseActionKindHostKill:
		return "Host blocked"
	case model.AbuseActionKindLabelKill:
		return "Name blocked"
	case model.AbuseActionKindCertRevoke:
		return "Certificate revoked"
	case model.AbuseActionKindZoneSuspend:
		return "DNS zone suspended"
	case model.AbuseActionKindHostSuspend:
		return "Host suspended in DNS"
	case model.AbuseActionKindNetworkSuspend:
		return "Network suspended"
	case model.AbuseActionKindMailHalt:
		return "Sending halted"
	case model.AbuseActionKindMailLabelHalt:
		return "Sending halted for one address"
	}
	return "Action taken"
}

// The action in words; for a lift, what was lifted.
func (self *AbuseActionNoticeTemplate) ActionText() string {
	if self.Lift() {
		return "Lifted: " + abuseActionKindText(self.LiftedKind)
	}
	return abuseActionKindText(self.Kind)
}

// When the action ends by itself, in UTC, or "" when only a lift ends it.
func (self *AbuseActionNoticeTemplate) UntilText() string {
	if self.UntilTime == nil {
		return ""
	}
	return self.UntilTime.UTC().Format("January 2, 2006, 15:04 UTC")
}

// The headline, in the html part's case.
func (self *AbuseActionNoticeTemplate) Headline() string {
	if self.Lift() {
		return "Abuse action lifted"
	}
	return "Abuse action on your network"
}

// The headline in the text part's upper case.
func (self *AbuseActionNoticeTemplate) TextHeadline() string {
	return strings.ToUpper(self.Headline())
}
