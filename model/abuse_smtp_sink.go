package model

// §21's case flow as SMTP's report intake and halt ladder reach it
// (ABUSE-M5-09, ABUSE-M9-01; D-ABUSE-1, D-ABUSE-5, D-SMTP-8, D-SMTP-19,
// D-MAIL-10, FS-39): the real smtpedgeservice.SmtpAbuseSink of mxedge's
// platform block and smtpedge's ladder. The sink is handed only an attributed
// report's fields: the network SMTP's join named, the reported From domain, the
// complaint's type, the join's confidence and the reporter's class. No
// envelope, header text, body or reporter address reaches this file, by the
// type it takes (mx-4, R-ABUSE-2), so none can reach a row.
//
// An attributed role mail opens a case against the network's sending domain,
// joined by headers, on §21.8's report clock, and a repeat inside the
// coalescing window counts on the case already open. A feedback loop's
// complaint takes §14.8's own signal by its join's confidence, high and medium
// on the ≤24 h clock and a low one on §21.8's last row, reaching review only.
// No machine action follows from a report here: a stranger's report reaches
// only review (§21.17), and §14.8's ladder is smtpedge's, each halt reaching
// §21 as its own action.
//
// The M9 half is §21's side of that ladder, which controller's mail pass runs
// (controller.RunAbuseMailPass) in a process holding §21's effects, notices
// and record mailer, since smtpedge and mxedge link none of them:
//
//	complaints   ListSmtpComplaintReports reads the loops' complaints by a
//	             cursor, and each goes through OpenSmtpCase, so every ARF join
//	             is a case, the intake handing the sink role mail alone
//	halts        ListUnrecordedSmtpHalts reads SMTP's halts no abuse_action
//	             records yet: a halt is recorded under its own id, so the pass
//	             finds the rest by the action's key, and a halt stays SMTP's
//	             Redis flag and smtp_halt row written before §21 hears of it,
//	             so it never waits on §21 (I-12)
//	suspension   RequestSmtpSuspension, the ladder's last rung, kicks the pass,
//	             which judges §21.4's rung on §21's own records
//	effects      EnsureAbuseSmtpHalt writes a §21 halt's row back where a
//	             restore lost it, and its lift removes the row (§21.10);
//	             AbuseSmtpStatusHint holds or releases the network's queues
//	             on topic mail, as SMTP's ladder does at its own halts
//
// A report the sink cannot place is an error, which the intake answers 451, so
// the sender retries and no report is lost unread; a binary that does not link
// MG5 answers ErrAbuseUnlinked the same way. Every value the store raises
// reaches the caller as an error (QD-1090).

import (
	"context"
	"fmt"
	"math"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/task"
)

// §14.8's attribution confidences, as smtp_complaint's join_result holds them
// (D-SMTP-8).
const (
	abuseSmtpJoinHigh   = "high"
	abuseSmtpJoinMedium = "medium"
)

// The sink's seams: the clock, the settings and the pass's kick, which a test
// replaces.
type AbuseSmtpSinkDeps struct {
	// The time a case opens at. nil reads the report's arrival time, and the
	// process's clock for a report with none.
	Now func() time.Time
	// §21's settings for each report. nil reads the defaults with abuse.yml's
	// protected names laid over them.
	Settings func() *AbuseSettings
	// Pulls §21's mail pass to now, what a suspension request does. nil kicks
	// the process's task queue (KickAbuseMailPass).
	Kick func(ctx context.Context) error
}

// The real sink. Safe for concurrent use: it holds no state between calls.
type abuseSmtpSink struct {
	deps *AbuseSmtpSinkDeps
}

var _ smtpedgeservice.SmtpAbuseSink = (*abuseSmtpSink)(nil)

// The sink mxedge's platform block gives its report intake
// (smtpedgeservice.SmtpReportIntakeDeps.Sink). nil deps reads the process's
// clock and config.
func NewAbuseSmtpSink(deps *AbuseSmtpSinkDeps) smtpedgeservice.SmtpAbuseSink {
	if deps == nil {
		deps = &AbuseSmtpSinkDeps{}
	}
	return &abuseSmtpSink{deps: deps}
}

// §21.4's category for a complaint's type: a role mail and a loop's abuse
// complaint are about mail the network sent, fraud is phishing, a virus is
// malware, and anything else is for a person to read.
func abuseSmtpCategory(complaintType string) AbuseCategory {
	switch complaintType {
	case smtpedgeservice.SmtpComplaintTypeRole, smtpedgeservice.SmtpComplaintTypeAbuse:
		return AbuseCategorySpam
	case smtpedgeservice.SmtpComplaintTypeFraud:
		return AbuseCategoryPhishing
	case smtpedgeservice.SmtpComplaintTypeVirus:
		return AbuseCategoryMalware
	}
	return AbuseCategoryOther
}

// See smtpedgeservice.SmtpAbuseSink.
func (self *abuseSmtpSink) OpenSmtpCase(ctx context.Context, report *smtpedgeservice.SmtpAbuseReport) (returnErr error) {
	if !server.SidecarMg5Linked() {
		return ErrAbuseUnlinked
	}
	if report == nil || report.NetworkId == (sidecar.Id{}) {
		return fmt.Errorf("%w: no network", ErrAbuseReportInvalid)
	}
	domain := abuseName(report.Domain)
	if domain == "" {
		return fmt.Errorf("%w: no domain", ErrAbuseReportInvalid)
	}
	defer func() {
		// the store raises a failed read or write, an ended context's included,
		// and each reaches the intake or the ladder as an error, so its sender
		// retries (QD-1090)
		if r := recover(); r != nil {
			returnErr = abuseSmtpSinkRecovered(r)
		}
	}()

	abuseReport := &AbuseReport{
		Category: abuseSmtpCategory(report.Category),
	}
	var signal AbuseSignal
	switch report.ReporterClass {
	case smtpedgeservice.SmtpReporterClassLoop:
		abuseReport.ReporterClass = AbuseReporterClassMailReport
		// §14.8's joins take their signals directly, by confidence
		switch report.JoinResult {
		case abuseSmtpJoinHigh:
			signal = AbuseSignalMailJoinHigh
		case abuseSmtpJoinMedium:
			signal = AbuseSignalMailJoinMedium
		default:
			signal = AbuseSignalMailJoinLow
		}
	case smtpedgeservice.SmtpReporterClassRole:
		abuseReport.ReporterClass = AbuseReporterClassRoleMail
		// anyone may mail a role address, so it sits on a report's row
		signal = AbuseSignalOfReport(abuseReport.ReporterClass, abuseReport.Category, false)
	default:
		return fmt.Errorf("%w: an unknown reporter class", ErrAbuseReportInvalid)
	}

	settings := self.settings()
	join := &AbuseReportJoin{
		NetworkId: server.Id(report.NetworkId),
		JoinKind:  AbuseJoinKindHeaders,
		JoinKey:   domain,
		Target:    AbuseTarget{Kind: AbuseTargetKindSendingDomain, Name: domain},
	}
	_, err := OpenAbuseReportCase(ctx, abuseReport, join, signal, self.now(report), settings)
	return err
}

// What the sink answers for a value the store raised: an error, every value
// alike, an ended context's included, wrapping a raised error so errors.Is
// still reads it (QD-1090). The sink's callers are engine goroutines with no
// server handler above them, smtpedge's halt ladder under its StoreTimeout and
// mxedge's report intake, so a raise that left here would end the process,
// where an error is a failure each of them already retries.
func abuseSmtpSinkRecovered(r any) error {
	if err, ok := r.(error); ok {
		return fmt.Errorf("abuse smtp sink: %w", err)
	}
	return fmt.Errorf("abuse smtp sink: %v", r)
}

// When the report's case opens.
func (self *abuseSmtpSink) now(report *smtpedgeservice.SmtpAbuseReport) time.Time {
	if self.deps.Now != nil {
		return self.deps.Now()
	}
	if !report.ArrivalTime.IsZero() {
		return report.ArrivalTime.UTC()
	}
	return server.NowUtc()
}

// §21's settings for one report.
func (self *abuseSmtpSink) settings() *AbuseSettings {
	if self.deps.Settings != nil {
		return self.deps.Settings()
	}
	settings := DefaultAbuseSettings()
	ApplyAbuseConfig(settings, server.GetAbuseConfig())
	return settings
}

// See smtpedgeservice.SmtpAbuseSink: §14.8's last rung, the ladder asking
// once, when the network's halts reach its count. §21 takes the request from
// its own records, since the suspension is §21.4's rung over §21's mail_halts
// and only a process holding §21's effects can apply it: the mail pass records
// each of the network's halts as a case and an action, and judges the rung on
// every network halt it records, suspending through ABUSE-M5-06's effects
// once the network's halts not lifted as wrong reach SuspendHalts within
// HaltWindow. The request kicks the pass so it runs now rather than at its
// next interval. A kick that fails is this answer's error, which the ladder
// counts; the pass still runs at its interval, so a request lost here loses
// no suspension.
func (self *abuseSmtpSink) RequestSmtpSuspension(ctx context.Context, request *smtpedgeservice.SmtpSuspensionRequest) error {
	if !server.SidecarMg5Linked() {
		return ErrAbuseUnlinked
	}
	if request == nil || request.NetworkId == (sidecar.Id{}) {
		return fmt.Errorf("%w: no network", ErrAbuseReportInvalid)
	}
	if len(request.HaltIds) == 0 {
		return fmt.Errorf("%w: no halt", ErrAbuseReportInvalid)
	}
	if self.deps.Kick != nil {
		return self.deps.Kick(ctx)
	}
	return KickAbuseMailPass(ctx)
}

// The run-once key of §21's mail pass (controller.RunAbuseMailPass), which a
// suspension request kicks.
const AbuseMailPassRunOnceKey = "abuse_mail_pass"

// Pulls the pending mail pass to now. What raised is the error: a kick that
// fails changes nothing the pass's own interval does not bound.
func KickAbuseMailPass(ctx context.Context) (returnErr error) {
	defer func() {
		if r := recover(); r != nil {
			returnErr = abuseSmtpSinkRecovered(r)
		}
	}()
	task.KickTasks(ctx, AbuseMailPassRunOnceKey)
	return nil
}

// Where §21's mail pass stands in smtp_complaint: the last complaint it took,
// by arrival then id. The zero value starts at the chain's horizon.
type AbuseSmtpComplaintCursor struct {
	ArrivalTime time.Time `json:"arrival_time"`
	ComplaintId server.Id `json:"complaint_id"`
}

// The complaint types a feedback loop's report is a §21 report of: those
// §14.8's ladder counts (smtpedgeservice's smtpComplaintHalts). A not-spam
// cancels, an aggregate's home row and an authentication failure report
// nothing, and role mail reaches the sink from the intake itself.
func abuseSmtpReportTypes() []string {
	return []string{
		smtpedgeservice.SmtpComplaintTypeAbuse,
		smtpedgeservice.SmtpComplaintTypeFraud,
		smtpedgeservice.SmtpComplaintTypeVirus,
		smtpedgeservice.SmtpComplaintTypeOther,
	}
}

// The feedback loops' complaints past cursor that arrived before before,
// oldest first, at most limit, each as the report the sink takes: a complaint
// of a type abuseSmtpReportTypes lists, from an allowlisted reporter. Its
// sealed copy is never read, nor its reporter or header fields beyond the
// domain. Needs MG5.
func ListSmtpComplaintReports(ctx context.Context, cursor AbuseSmtpComplaintCursor, before time.Time, limit int) (reports []*smtpedgeservice.SmtpAbuseReport, returnErr error) {
	reports = []*smtpedgeservice.SmtpAbuseReport{}
	if !server.SidecarMg5Linked() {
		return nil, ErrAbuseUnlinked
	}
	if limit <= 0 {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			reports = nil
			returnErr = abuseSmtpSinkRecovered(r)
		}
	}()
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT complaint_id, network_id, complaint_type, arrival_time, domain, join_result
				FROM smtp_complaint
				WHERE
					$1 <= arrival_time AND
					arrival_time < $2 AND
					(arrival_time, complaint_id) > ($1, $3) AND
					complaint_type = ANY($4::varchar[]) AND
					reporter IS NOT NULL
				ORDER BY arrival_time, complaint_id
				LIMIT $5
			`,
			cursor.ArrivalTime.UTC(),
			before.UTC(),
			cursor.ComplaintId,
			abuseSmtpReportTypes(),
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var complaintId, networkId server.Id
				var domain, joinResult *string
				report := &smtpedgeservice.SmtpAbuseReport{ReporterClass: smtpedgeservice.SmtpReporterClassLoop}
				server.Raise(result.Scan(&complaintId, &networkId, &report.Category, &report.ArrivalTime, &domain, &joinResult))
				report.ComplaintId = sidecar.Id(complaintId)
				report.NetworkId = sidecar.Id(networkId)
				report.Domain = smtpTextOf(domain)
				report.JoinResult = smtpTextOf(joinResult)
				reports = append(reports, report)
			}
		})
	})
	return
}

// Where one walk over the unrecorded halts stands: the last it read, by start
// then id.
type AbuseSmtpHaltCursor struct {
	StartTime time.Time
	HaltId    server.Id
}

// SMTP's halts started at or after since that §21 has not recorded, no
// abuse_action carrying the halt's id, oldest first, past cursor, at most
// limit. A halt a recheck lifted is gone from smtp_halt and is never listed.
// Needs MG5 and MG9.
func ListUnrecordedSmtpHalts(ctx context.Context, since time.Time, cursor AbuseSmtpHaltCursor, limit int) (halts []*smtpedgeservice.SmtpHalt, returnErr error) {
	halts = []*smtpedgeservice.SmtpHalt{}
	if !server.SidecarMg5Linked() {
		return nil, ErrAbuseUnlinked
	}
	if !server.SidecarMg9Linked() {
		return nil, ErrSmtpEdgeUnlinked
	}
	if limit <= 0 {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			halts = nil
			returnErr = abuseSmtpSinkRecovered(r)
		}
	}()
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT halt.halt_id, halt.network_id, halt.label, halt.trigger, halt.start_time
				FROM smtp_halt halt
				WHERE
					$1 <= halt.start_time AND
					(halt.start_time, halt.halt_id) > ($2, $3) AND
					NOT EXISTS (
						SELECT 1
						FROM abuse_action
						WHERE abuse_action.action_id = halt.halt_id
					)
				ORDER BY halt.start_time, halt.halt_id
				LIMIT $4
			`,
			since.UTC(),
			cursor.StartTime.UTC(),
			cursor.HaltId,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var haltId, networkId server.Id
				var label *string
				halt := &smtpedgeservice.SmtpHalt{}
				server.Raise(result.Scan(&haltId, &networkId, &label, &halt.Trigger, &halt.StartTime))
				halt.HaltId = sidecar.Id(haltId)
				halt.NetworkId = sidecar.Id(networkId)
				halt.Label = smtpTextOf(label)
				halts = append(halts, halt)
			}
		})
	})
	return
}

// The §21 mail actions that still stand at now, their effect applied, whose
// smtp_halt row is gone before their until: halts a recheck lifted
// (SmtpHaltLadder.Lift removes the row), whose §21 record the appeal did not
// lift. An action whose effect is still owed is left out, since a restore
// re-applies one before its effect writes the row back. At most limit, oldest
// first. Needs MG5 and MG9.
func ListAbuseSmtpHaltsLiftedBySmtp(ctx context.Context, now time.Time, limit int) (actions []*AbuseAction, returnErr error) {
	actions = []*AbuseAction{}
	if !server.SidecarMg5Linked() {
		return nil, ErrAbuseUnlinked
	}
	if !server.SidecarMg9Linked() {
		return nil, ErrSmtpEdgeUnlinked
	}
	if limit <= 0 {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			actions = nil
			returnErr = abuseSmtpSinkRecovered(r)
		}
	}()
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT `+abuseActionColumns+`
				FROM abuse_action
				WHERE
					abuse_action.kind = ANY($1::varchar[]) AND
					abuse_action.review_state IN ($2, $3) AND
					abuse_action.applied_time IS NOT NULL AND
					$4 < abuse_action.until_time AND
					NOT EXISTS (
						SELECT 1
						FROM smtp_halt
						WHERE smtp_halt.halt_id = abuse_action.action_id
					)
				ORDER BY abuse_action.create_time, abuse_action.action_id
				LIMIT $5
			`,
			[]string{string(AbuseActionKindMailHalt), string(AbuseActionKindMailLabelHalt)},
			string(AbuseReviewStatePending),
			string(AbuseReviewStateUpheld),
			now.UTC(),
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				actions = append(actions, scanAbuseAction(result))
			}
		})
	})
	return
}

// The trigger of a halt §21's effect writes back (EnsureAbuseSmtpHalt): the
// record §21.10 mails carries no trigger of SMTP's, so a halt a restore
// re-applies holds as §21's, which SmtpHaltLadder.Lift's recheck never lifts,
// since the complaints that set it may be gone with the restore. §21's review
// decides it.
const AbuseSmtpHaltTrigger = "abuse"

// Writes halt's smtp_halt row when no row of its id is there and the halt still
// holds at now, and leaves a row that is there as it is, SMTP's trigger kept:
// a §21 mail action's effect, which writes something only where §19.10's
// restore lost a halt that §21.10's mail re-applies. written is whether it
// wrote. Needs MG9.
func EnsureAbuseSmtpHalt(ctx context.Context, halt *smtpedgeservice.SmtpHalt, now time.Time) (written bool, returnErr error) {
	if !server.SidecarMg9Linked() {
		return false, ErrSmtpEdgeUnlinked
	}
	if err := smtpHaltRefusal(halt); err != nil {
		return false, err
	}
	if !halt.ActiveAt(now) {
		// an ended halt needs no row
		return false, nil
	}
	defer func() {
		if r := recover(); r != nil {
			written = false
			returnErr = abuseSmtpSinkRecovered(r)
		}
	}()
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO smtp_halt (halt_id, network_id, label, trigger, start_time)
				VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (halt_id) DO NOTHING
			`,
			server.Id(halt.HaltId),
			server.Id(halt.NetworkId),
			smtpNullableText(halt.Label),
			halt.Trigger,
			halt.StartTime.UTC(),
		))
		written = tag.RowsAffected() == 1
	})
	return
}

// Hints the network's sending clients on topic mail with §14.8's SmtpStatus
// (QD-479: api and taskworker may write the topic, its version the writer's
// clock in ms): HALTED holding their queues for retryAfter, at least a
// millisecond, or CLEAR releasing them. They are the clients with a send
// started since since (SmtpSendingClients), as SMTP's ladder hints them. A
// client that misses the hint meets the halt or its lift at its next
// preamble, so an error is the caller's to count. Needs MG9.
func AbuseSmtpStatusHint(ctx context.Context, networkId server.Id, kind protocol.SmtpStatusKind, retryAfter time.Duration, since time.Time, now time.Time) (returnErr error) {
	defer func() {
		if r := recover(); r != nil {
			returnErr = abuseSmtpSinkRecovered(r)
		}
	}()
	clientIds, err := SmtpSendingClients(ctx, networkId, since)
	if err != nil {
		return err
	}
	if len(clientIds) == 0 {
		return nil
	}
	status := &protocol.SmtpStatus{Kind: uint32(kind)}
	if kind != protocol.SmtpStatusKind_SMTP_STATUS_CLEAR {
		status.RetryAfterMs = uint32(min(max(retryAfter.Milliseconds(), 1), math.MaxUint32))
	}
	body, err := proto.Marshal(status)
	if err != nil {
		return err
	}
	writer, err := sidecarHintWriter()
	if err != nil {
		return err
	}
	connectClientIds := make([]connect.Id, 0, len(clientIds))
	for _, clientId := range clientIds {
		connectClientIds = append(connectClientIds, connect.Id(clientId))
	}
	return writer.Hint(ctx, connectClientIds, &sidecar.ClientHint{
		Topic:   sidecar.PushTopicMail,
		Version: uint64(now.UnixMilli()),
		Kind:    uint32(protocol.MessageType_SMTP_STATUS),
		Body:    body,
	})
}
