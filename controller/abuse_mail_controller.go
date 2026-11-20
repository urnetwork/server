package controller

// §21's half of §14.8's ladder (ABUSE-M9-01; R-ABUSE-1, D-ABUSE-1, D-ABUSE-5, D-SMTP-8, D-SMTP-14, D-SMTP-23, I-4,
// I-12): what SMTP's reports and halts become in §21, §21.4's spam and mail rows.
//
//	ARF joins   each feedback loop's complaint smtp_complaint holds opens a case, or counts on the open case of its
//	            network, sending domain and category, through the real SmtpAbuseSink (model.NewAbuseSmtpSink), on
//	            §21.8's clock for its join's confidence: ≤24 h for a high or medium join, §21.8's last row for a low
//	            one, which reaches review only and never a halt (D-SMTP-8). No machine action follows from a report,
//	            since §14.8's ladder is smtpedge's (model.AbuseMachineAction's mail_ladder)
//	halts       each smtp_halt row SMTP's ladder wrote becomes an action under a case, the halt's own id its action
//	            id: mail_halt for the network, mail_label_halt for one burner label, through ApplyAbuseAction, so it
//	            is reviewed within ≤24 h of the halt's start, noticed to Owners and Admins and mailed off site
//	            (§21.10), and a network halt restricts its network for 30 days, internet mail resuming after the halt
//	            at Community caps (§21.7, read by smtpedge's gate through ServiceHost.Abuse). A burner label's halt
//	            restricts nothing (reading 4). The halt stays SMTP's Redis flag and row, written before §21 hears of
//	            it, so a halt never waits on §21 (I-12): §21 records it, at most a pass later
//	suspension  on each network halt it records, §21.4's rung over the network's mail_halts not lifted as wrong, of
//	            any category: the third in 30 days suspends the network until reviewed, through ABUSE-M5-06's
//	            effects, once, and only a newer halt suspends again after a lift (model.NextAbuseStep). The rung is
//	            judged before the halt's own action is recorded, so a suspension that fails to commit leaves the halt
//	            for the next pass to judge again
//	appeals     POST /smtp/appeal's recheck (SmtpHaltLadder.Lift) lifts what it can, removing the halt's row; each
//	            halt it lifted is lifted here as wrong, so it leaves every count and the restricted window
//	            (LiftAbuseSmtpHaltOnRecheck, from the route, and the pass for any the route could not record); a halt
//	            the recheck keeps stays pending on its case for §21's review (§14.8), and its network may appeal the
//	            case through POST /abuse/appeal
//	RUA         SMTP-M9-17's reconcile opens its own case with no halt (NewSmtpReconcileCaseSink, D-SMTP-23)
//
// RunAbuseMailPass does the first four a minute at a time in taskworker (work.AbuseMailPass), and the ladder's
// suspension request kicks it at once (model.KickAbuseMailPass). It runs where §21's effects, record mailer and
// notices are installed: smtpedge and mxedge link none of them, which is why a halt reaches §21 through its row and
// not inside the ladder's call. Each halt's action is ApplyAbuseAction's, so its effect (the halt's row, written back
// only where a restore lost it, abuse_effects.go) and its mail run on §21's one path, and the action pass finishes
// either when it fails.
//
// No sealed report reaches a reviewer unless the network volunteers it (§21.8): a case keeps §21's fields alone,
// and nothing here reads a sealed copy. Logs and metrics carry no network id, domain or label (I-13).

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Tunables of the mail pass.
type AbuseMailSettings struct {
	// How often the pass runs: a minute, so a halt is §21's, noticed and mailed, within a minute or so of SMTP's
	// ladder writing it, and a suspension request kicks it sooner.
	PassInterval time.Duration
	// A complaint that arrived more recently than this is left for the next pass: 5 minutes, past the intake's own
	// store bound, so a complaint still being stored when the cursor passes its arrival is not passed over.
	ComplaintGrace time.Duration
	// Where a chain with no cursor starts: a day back, CaseCoalesceWindow's span, so a chain started again counts
	// what it may have taken on the cases still open.
	ComplaintHorizon time.Duration
	// How far back an unrecorded halt is still recorded: §14.8's 30 days, the suspension rung's window, so every
	// halt the rung counts is §21's.
	HaltHorizon time.Duration
	// The most rows one read takes, by a cursor.
	ReadBatchSize int
	// One pass's bound; a pass that runs out leaves the rest to the next, sooner.
	PassMaxTime time.Duration
	// The task runner's bound on one pass, past PassMaxTime.
	PassTaskMaxTime time.Duration
	// How soon a pass that left work behind, or failed, runs again.
	RetryInterval time.Duration
}

func DefaultAbuseMailSettings() *AbuseMailSettings {
	return &AbuseMailSettings{
		PassInterval:     time.Minute,
		ComplaintGrace:   5 * time.Minute,
		ComplaintHorizon: 24 * time.Hour,
		HaltHorizon:      30 * 24 * time.Hour,
		ReadBatchSize:    100,
		PassMaxTime:      5 * time.Minute,
		PassTaskMaxTime:  10 * time.Minute,
		RetryInterval:    10 * time.Second,
	}
}

// The review reason a recheck's lift records: fixed words, never the appellant's.
const abuseMailRecheckLiftReason = "lifted by POST /smtp/appeal's recheck (§14.8): a duplicate's or a cancelled complaint's halt"

// The pass's seams, which a test replaces.
var abuseMailDeps = struct {
	settings func() *model.AbuseSettings
}{
	settings: func() *model.AbuseSettings {
		settings := model.DefaultAbuseSettings()
		model.ApplyAbuseConfig(settings, server.GetAbuseConfig())
		return settings
	},
}

// Counts what the pass did with each item, by kind (complaint, halt, recheck) and result. No label carries a name.
var abuseMailItemCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "abuse",
		Name:      "mail_items_total",
		Help:      "Complaints, halts and recheck lifts §21's mail pass took, by kind and result",
	},
	[]string{"kind", "result"},
)

// Registers the item counter with the default registry.
func init() {
	prometheus.MustRegister(abuseMailItemCounter)
}

// The default-level reports of items the pass could not take.
var abuseMailPassFailures = newBoundedReport(abuseActionReportInterval)

// What one pass did.
type AbuseMailPassResult struct {
	// complaints that opened or joined a case
	ComplaintCaseCount int
	// complaints no case can hold, passed over: no network or no domain
	ComplaintRefusedCount int
	// halts recorded as actions
	HaltRecordedCount int
	// networks suspended by §21.4's rung
	SuspendedCount int
	// halts a recheck lifted, lifted here as wrong
	RecheckLiftCount int
	// items that failed and wait for the next pass
	FailedCount int
	// where the complaint cursor stands after the pass
	Next model.AbuseSmtpComplaintCursor
	// the pass ran out of time or failed with work left
	More bool
}

// One pass at now from cursor, the steps in the file header: complaints first, by the cursor, which stops at the first
// that fails for a reason a retry could cure; then the unrecorded halts, each on its own, so one that fails waits for
// the next pass and holds back no other; then the recheck lifts the route did not record. A binary that does not link
// MG5 does nothing, and one that does not link MG9 takes complaints alone.
func RunAbuseMailPass(ctx context.Context, cursor model.AbuseSmtpComplaintCursor, now time.Time, settings *AbuseMailSettings) *AbuseMailPassResult {
	result := &AbuseMailPassResult{Next: cursor}
	if !server.SidecarMg5Linked() {
		return result
	}
	deadline := time.Now().Add(settings.PassMaxTime)
	expired := func() bool {
		return ctx.Err() != nil || !time.Now().Before(deadline)
	}
	abuseSettings := abuseMailDeps.settings()
	sink := model.NewAbuseSmtpSink(&model.AbuseSmtpSinkDeps{
		Settings: func() *model.AbuseSettings {
			return abuseSettings
		},
	})

	if result.Next.ArrivalTime.IsZero() {
		result.Next = model.AbuseSmtpComplaintCursor{ArrivalTime: now.Add(-settings.ComplaintHorizon).UTC()}
	}
	before := now.Add(-settings.ComplaintGrace)
complaints:
	for !expired() {
		reports, err := model.ListSmtpComplaintReports(ctx, result.Next, before, settings.ReadBatchSize)
		if err != nil {
			result.FailedCount += 1
			result.More = true
			abuseMailFailure("complaint", err)
			break
		}
		for _, report := range reports {
			if expired() {
				break complaints
			}
			err := sink.OpenSmtpCase(ctx, report)
			switch {
			case err == nil:
				result.ComplaintCaseCount += 1
				abuseMailItemCounter.WithLabelValues("complaint", "case").Inc()
			case errors.Is(err, model.ErrAbuseReportInvalid):
				// no case can hold it, now or later
				result.ComplaintRefusedCount += 1
				abuseMailItemCounter.WithLabelValues("complaint", "refused").Inc()
			default:
				// the cursor stays before it, so the next pass takes it again
				result.FailedCount += 1
				result.More = true
				abuseMailItemCounter.WithLabelValues("complaint", "failed").Inc()
				abuseMailFailure("complaint", err)
				break complaints
			}
			result.Next = model.AbuseSmtpComplaintCursor{ArrivalTime: report.ArrivalTime.UTC(), ComplaintId: server.Id(report.ComplaintId)}
		}
		if len(reports) < settings.ReadBatchSize {
			break
		}
	}

	if !server.SidecarMg9Linked() {
		return result
	}
	since := now.Add(-settings.HaltHorizon)
	haltCursor := model.AbuseSmtpHaltCursor{}
	for !expired() {
		halts, err := model.ListUnrecordedSmtpHalts(ctx, since, haltCursor, settings.ReadBatchSize)
		if err != nil {
			result.FailedCount += 1
			result.More = true
			abuseMailFailure("halt", err)
			break
		}
		for _, halt := range halts {
			if expired() {
				result.More = true
				break
			}
			haltCursor = model.AbuseSmtpHaltCursor{StartTime: halt.StartTime, HaltId: server.Id(halt.HaltId)}
			record, err := RecordAbuseSmtpHalt(ctx, halt, now, abuseSettings)
			if err != nil {
				// the halt stays unrecorded, and the next pass takes it again
				result.FailedCount += 1
				abuseMailItemCounter.WithLabelValues("halt", "failed").Inc()
				abuseMailFailure("halt", err)
				continue
			}
			result.HaltRecordedCount += 1
			abuseMailItemCounter.WithLabelValues("halt", "recorded").Inc()
			if record.Suspension != nil {
				result.SuspendedCount += 1
				abuseMailItemCounter.WithLabelValues("halt", "suspended").Inc()
			}
		}
		if len(halts) < settings.ReadBatchSize {
			break
		}
	}

	if !expired() {
		actions, err := model.ListAbuseSmtpHaltsLiftedBySmtp(ctx, now, settings.ReadBatchSize)
		if err != nil {
			result.FailedCount += 1
			abuseMailFailure("recheck", err)
		}
		for _, action := range actions {
			if expired() {
				result.More = true
				break
			}
			lift, err := liftAbuseSmtpHaltAction(ctx, action)
			switch {
			case err != nil:
				result.FailedCount += 1
				abuseMailItemCounter.WithLabelValues("recheck", "failed").Inc()
				abuseMailFailure("recheck", err)
			case lift != nil:
				result.RecheckLiftCount += 1
				abuseMailItemCounter.WithLabelValues("recheck", "lifted").Inc()
			}
		}
	}
	if expired() {
		result.More = true
	}
	return result
}

// Reports one failed item at most once per interval, naming its kind and no id.
func abuseMailFailure(kind string, err error) {
	if suppressedCount, ok := abuseMailPassFailures.Allow(time.Now()); ok {
		glog.Infof("[abuse]the mail pass could not take a %s, and the next pass takes it again; %d more since the last report\n", kind, suppressedCount)
	}
	if glog.V(1) {
		glog.Infof("[abuse]the mail pass's %s failed: %s\n", kind, err)
	}
}

// What recording one halt did.
type AbuseSmtpHaltRecord struct {
	CaseId server.Id
	// the halt's action, under the halt's id
	Action *AbuseActionResult
	// the suspension §21.4's rung took on this halt, or nil
	Suspension *AbuseActionResult
}

// The network's sending domain, <label>.<root>, in the first of roots that holds its label, or "" for none.
func abuseMailSendingDomain(ctx context.Context, networkId server.Id, roots []string) string {
	for _, root := range roots {
		root = model.AbuseName(root)
		if root == "" {
			continue
		}
		if label := model.GetNetworkLabel(ctx, networkId, root); label != "" {
			return model.AbuseName(label + "." + root)
		}
	}
	return ""
}

// §21.4's category of a halt's case. A fraud halt takes the type of the complaint that set it, which arrived at the
// halt's start: a virus is malware, and anything else phishing (RFC 5965's fraud); every other halt is spam's.
func abuseMailHaltCategory(ctx context.Context, halt *smtpedgeservice.SmtpHalt, settings *model.AbuseSettings) model.AbuseCategory {
	if halt.Trigger != smtpedgeservice.SmtpHaltTriggerFraud {
		return model.AbuseCategorySpam
	}
	complaints, err := model.FindSmtpComplaints(ctx, &smtpedgeservice.SmtpComplaintKey{NetworkId: halt.NetworkId, Since: halt.StartTime}, settings.LookupRowLimit)
	if err == nil {
		for _, complaint := range complaints {
			if complaint.ArrivalTime.Equal(halt.StartTime) && complaint.Type == smtpedgeservice.SmtpComplaintTypeVirus {
				return model.AbuseCategoryMalware
			}
		}
	}
	return model.AbuseCategoryPhishing
}

// Whether a halt's trigger is a complaint's, so its case joins by headers (§14.8's joins); a record cut or the
// network's own bounces is the edge's observation of the network itself.
func abuseMailHaltJoinedByHeaders(trigger string) bool {
	switch trigger {
	case smtpedgeservice.SmtpHaltTriggerLabelComplaint, smtpedgeservice.SmtpHaltTriggerCommunityComplaint,
		smtpedgeservice.SmtpHaltTriggerSecondLabel, smtpedgeservice.SmtpHaltTriggerProRate,
		smtpedgeservice.SmtpHaltTriggerTeamRate, smtpedgeservice.SmtpHaltTriggerFraud:
		return true
	}
	return false
}

// Records one of SMTP's halts in §21 at now (the file header's halts and suspension): its case, opened or joined at
// the halt's start on the ≤24 h clock, then for a network halt §21.4's rung, then the halt's action under the halt's
// own id, which ApplyAbuseAction records, applies, notices and mails. The case names the network's sending domain,
// or for a label halt the burner's, <blabel>.<label>.<root>, or the network itself when it has no label in a root. A
// raise from the store is an error, so one halt never ends the pass.
func RecordAbuseSmtpHalt(ctx context.Context, halt *smtpedgeservice.SmtpHalt, now time.Time, settings *model.AbuseSettings) (record *AbuseSmtpHaltRecord, returnErr error) {
	if !server.SidecarMg5Linked() {
		return nil, model.ErrAbuseUnlinked
	}
	if halt == nil || halt.HaltId == (sidecar.Id{}) || halt.NetworkId == (sidecar.Id{}) || halt.StartTime.IsZero() {
		return nil, fmt.Errorf("%w: a halt with no id, network or start", model.ErrAbuseActionInvalid)
	}
	defer func() {
		if r := recover(); r != nil {
			record = nil
			returnErr = abuseMailRecovered(r)
		}
	}()
	networkId := server.Id(halt.NetworkId)
	label := model.AbuseName(halt.Label)
	kind := model.AbuseActionKindMailHalt
	if label != "" {
		kind = model.AbuseActionKindMailLabelHalt
	}

	sendingDomain := abuseMailSendingDomain(ctx, networkId, settings.RootNames)
	target := model.AbuseTarget{Kind: model.AbuseTargetKindNetwork, Name: networkId.String()}
	actionTarget := networkId.String()
	switch {
	case sendingDomain != "" && label != "":
		target = model.AbuseTarget{Kind: model.AbuseTargetKindSendingDomain, Name: label + "." + sendingDomain}
		actionTarget = target.Name
	case sendingDomain != "":
		target = model.AbuseTarget{Kind: model.AbuseTargetKindSendingDomain, Name: sendingDomain}
		actionTarget = target.Name
	case label != "":
		// the effect reads the label back from its target's first name
		actionTarget = label
	}
	joinKind := model.AbuseJoinKindNetwork
	if abuseMailHaltJoinedByHeaders(halt.Trigger) {
		joinKind = model.AbuseJoinKindHeaders
	}
	category := abuseMailHaltCategory(ctx, halt, settings)

	reportCase, err := model.OpenAbuseReportCase(
		ctx,
		&model.AbuseReport{
			Category:      category,
			ReporterClass: model.AbuseReporterClassMailReport,
		},
		&model.AbuseReportJoin{
			NetworkId: networkId,
			JoinKind:  joinKind,
			JoinKey:   target.Name,
			Target:    target,
		},
		// a halt is §14.8's ladder acting on a high or medium join, or on the edge's own count: the machine
		// action's ≤24 h clock, from the report's arrival
		model.AbuseSignalMailJoinHigh,
		halt.StartTime,
		settings,
	)
	if err != nil {
		return nil, err
	}
	record = &AbuseSmtpHaltRecord{CaseId: reportCase.CaseId}

	if kind == model.AbuseActionKindMailHalt {
		// the rung counts this halt as the network's newest, before its own row is written (the file header)
		history := model.GetNetworkAbuseHistory(ctx, networkId)
		history = append(history, model.AbuseHistoryAction{
			CaseId:      reportCase.CaseId,
			Category:    category,
			Kind:        model.AbuseActionKindMailHalt,
			Target:      actionTarget,
			ReviewState: model.AbuseReviewStatePending,
			CreateTime:  now,
		})
		// spam's row, whatever the halt's category: 3 network halts in 30 days, of any category (§21.4)
		step, err := model.NextAbuseStep(model.AbuseCategorySpam, target, history, now, &settings.AbusePolicySettings)
		if err == nil && slices.Contains(step.ActionKinds, model.AbuseActionKindNetworkSuspend) {
			suspension, err := ApplyAbuseAction(ctx, &model.AbuseActionArgs{
				ActionId:      server.NewId(),
				CaseId:        reportCase.CaseId,
				Kind:          model.AbuseActionKindNetworkSuspend,
				Target:        actionTarget,
				ReviewDueTime: reportCase.ReviewDueTime,
			})
			if err != nil {
				return nil, err
			}
			record.Suspension = suspension
		}
	}

	untilTime := halt.StartTime.Add(smtpedgeservice.SmtpHaltDuration).UTC()
	action, err := ApplyAbuseAction(ctx, &model.AbuseActionArgs{
		ActionId:      server.Id(halt.HaltId),
		CaseId:        reportCase.CaseId,
		Kind:          kind,
		Target:        actionTarget,
		UntilTime:     &untilTime,
		ReviewDueTime: reportCase.ReviewDueTime,
	})
	if err != nil {
		return nil, err
	}
	record.Action = action
	return record, nil
}

// POST /smtp/appeal's §21 half (§14.8): the halt networkId's recheck lifted (SmtpHaltLadder.Lift answered true) was
// a duplicate's or a cancelled complaint's, so its §21 action is lifted as wrong, which takes it out of the restricted
// window and every count, and its network is noticed. A halt §21 has not recorded has nothing to lift, and the pass
// never records it, its row gone. nil for a halt that is not the network's, not a mail halt, or lifted already. The
// route calls it after each lift, and the pass lifts any it could not (ListAbuseSmtpHaltsLiftedBySmtp).
func LiftAbuseSmtpHaltOnRecheck(ctx context.Context, networkId server.Id, haltId server.Id) (lift *AbuseActionResult, returnErr error) {
	if !server.SidecarMg5Linked() {
		return nil, model.ErrAbuseUnlinked
	}
	defer func() {
		if r := recover(); r != nil {
			lift = nil
			returnErr = abuseMailRecovered(r)
		}
	}()
	action := model.GetAbuseAction(ctx, haltId)
	if action == nil || action.NetworkId == nil || *action.NetworkId != networkId {
		return nil, nil
	}
	return liftAbuseSmtpHaltAction(ctx, action)
}

// What a raise from the store becomes in the mail pass and the route: an error, every value alike, so one item
// never ends the pass or the request.
func abuseMailRecovered(r any) error {
	if err, ok := r.(error); ok {
		return fmt.Errorf("abuse mail: %w", err)
	}
	return fmt.Errorf("abuse mail: %v", r)
}

// Lifts a mail halt's action as wrong, by machine, unless it is no mail halt or is lifted already.
func liftAbuseSmtpHaltAction(ctx context.Context, action *model.AbuseAction) (lift *AbuseActionResult, returnErr error) {
	defer func() {
		if r := recover(); r != nil {
			lift = nil
			returnErr = abuseMailRecovered(r)
		}
	}()
	if action.Kind != model.AbuseActionKindMailHalt && action.Kind != model.AbuseActionKindMailLabelHalt {
		return nil, nil
	}
	if action.ReviewState == model.AbuseReviewStateLifted || action.ReviewState == model.AbuseReviewStateLiftedWrong {
		return nil, nil
	}
	lifted, err := LiftAbuseAction(ctx, &model.AbuseLiftArgs{
		ActionId:       server.NewId(),
		CaseId:         action.CaseId,
		LiftedActionId: action.ActionId,
		ReviewState:    model.AbuseReviewStateLiftedWrong,
		ReviewReason:   abuseMailRecheckLiftReason,
	})
	if errors.Is(err, model.ErrAbuseActionLifted) {
		return nil, nil
	}
	return lifted, err
}

// The SMTP halt a §21 mail action stands for, as its effect writes it back (model.EnsureAbuseSmtpHalt): its id the
// action's, its network the action's, the burner label the first name of a mail_label_halt's target, and its start
// one SmtpHaltDuration before the action's until, under §21's trigger, since the record carries none of SMTP's.
func abuseMailHaltOf(action *model.AbuseAction) (*smtpedgeservice.SmtpHalt, error) {
	if action.NetworkId == nil {
		return nil, fmt.Errorf("%w: a halt with no network", ErrAbuseEffectTarget)
	}
	if action.UntilTime == nil {
		return nil, fmt.Errorf("%w: a halt with no until", ErrAbuseEffectTarget)
	}
	halt := &smtpedgeservice.SmtpHalt{
		HaltId:    sidecar.Id(action.ActionId),
		NetworkId: sidecar.Id(*action.NetworkId),
		Trigger:   model.AbuseSmtpHaltTrigger,
		StartTime: action.UntilTime.Add(-smtpedgeservice.SmtpHaltDuration).UTC(),
	}
	if action.Kind == model.AbuseActionKindMailLabelHalt {
		label, _, _ := strings.Cut(model.AbuseName(action.Target), ".")
		if label == "" {
			return nil, fmt.Errorf("%w: a label halt with no label", ErrAbuseEffectTarget)
		}
		halt.Label = label
	}
	return halt, nil
}
