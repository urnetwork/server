//go:build sidecar_pending

package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
)

// The sink's M9 half (abuse_smtp_sink.go; ABUSE-M9-01) on MG5's and MG9's tables, in a build with sidecar_pending.
// Names are .example.

// QD-1090: a report on a context that ended, as a store call past smtpedge's StoreTimeout has, answers
// context.Canceled as an error and raises nothing into the engine's goroutine, which no server handler would catch.
func TestAbuseSmtpSinkAnswersAnEndedContextWithAnError(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		sink := newTestAbuseSmtpSink()
		ended, cancel := context.WithCancel(context.Background())
		cancel()
		raised, err := func() (raised any, err error) {
			defer func() {
				raised = recover()
			}()
			return nil, sink.OpenSmtpCase(ended, &smtpedgeservice.SmtpAbuseReport{
				ComplaintId:   sidecar.NewId(),
				NetworkId:     sidecar.NewId(),
				Domain:        "quietnet.root.example",
				Category:      smtpedgeservice.SmtpComplaintTypeAbuse,
				JoinResult:    "high",
				ReporterClass: smtpedgeservice.SmtpReporterClassLoop,
				ArrivalTime:   server.NowUtc(),
			})
		}()
		if raised != nil {
			t.Fatalf("the sink raised %v", raised)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the sink answered %v", err)
		}
		if got := testAbuseCaseCount(t, context.Background()); got != 0 {
			t.Fatalf("an ended report left %d cases", got)
		}
	})
}

// §14.8's last rung: a suspension request names a network and its halts, and the sink takes it by kicking §21's mail
// pass, which judges §21.4's rung on §21's own records (controller.RunAbuseMailPass); a request with no network or no
// halt is refused and kicks nothing, and a kick that fails is the answer's error.
func TestAbuseSmtpSinkSuspensionRequestKicksThePass(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		kicks := 0
		var kickErr error
		sink := NewAbuseSmtpSink(&AbuseSmtpSinkDeps{
			Settings: testAbuseJoinSettings,
			Kick: func(ctx context.Context) error {
				kicks += 1
				return kickErr
			},
		})
		request := &smtpedgeservice.SmtpSuspensionRequest{
			NetworkId:   sidecar.NewId(),
			HaltIds:     []sidecar.Id{sidecar.NewId(), sidecar.NewId(), sidecar.NewId()},
			RequestTime: server.NowUtc(),
		}
		if err := sink.RequestSmtpSuspension(ctx, request); err != nil || kicks != 1 {
			t.Fatalf("a request: %v, %d kicks", err, kicks)
		}
		for name, refused := range map[string]*smtpedgeservice.SmtpSuspensionRequest{
			"no request": nil,
			"no network": {HaltIds: request.HaltIds},
			"no halt":    {NetworkId: request.NetworkId},
		} {
			if err := sink.RequestSmtpSuspension(ctx, refused); !errors.Is(err, ErrAbuseReportInvalid) {
				t.Fatalf("%s: %v", name, err)
			}
		}
		if kicks != 1 {
			t.Fatalf("a refused request kicked: %d kicks", kicks)
		}
		kickErr = errors.New("the task queue did not answer")
		if err := sink.RequestSmtpSuspension(ctx, request); !errors.Is(err, kickErr) {
			t.Fatalf("a failed kick answered %v", err)
		}
		// the process's own kick, on its task queue, with no chain pending
		if err := NewAbuseSmtpSink(nil).RequestSmtpSuspension(ctx, request); err != nil {
			t.Fatal(err)
		}
	})
}

// The pass's reads: the loops' complaints past a cursor and before a bound, a report each, role mail, a not-spam and
// an aggregate's row left out and no sealed copy read; and SMTP's halts no action records, by start, past a cursor.
func TestAbuseSmtpSinkListsWhatThePassTakes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg9Linked() {
			t.Skip("smtp_halt is MG9's: run with -tags sidecar_pending")
		}
		ctx := context.Background()
		networkId := sidecar.NewId()
		arrivalTime := server.NowUtc().Add(-time.Hour).Truncate(time.Microsecond)
		insert := func(complaintType string, reporter string, at time.Time) sidecar.Id {
			complaintId := sidecar.NewId()
			if err := InsertSmtpComplaint(ctx, &smtpedgeservice.SmtpComplaint{
				ComplaintId:  complaintId,
				NetworkId:    networkId,
				Reporter:     reporter,
				Type:         complaintType,
				ArrivalTime:  at,
				Domain:       "quietnet.root.example",
				JoinResult:   "medium",
				SealedReport: []byte("sealed bytes the pass never reads"),
			}); err != nil {
				t.Fatal(err)
			}
			return complaintId
		}
		first := insert(smtpedgeservice.SmtpComplaintTypeAbuse, "fbl.example", arrivalTime)
		insert(smtpedgeservice.SmtpComplaintTypeRole, "", arrivalTime.Add(time.Minute))
		insert(smtpedgeservice.SmtpComplaintTypeNotSpam, "fbl.example", arrivalTime.Add(2*time.Minute))
		insert(smtpedgeservice.SmtpComplaintTypeRua, "agg.example", arrivalTime.Add(3*time.Minute))
		second := insert(smtpedgeservice.SmtpComplaintTypeVirus, "fbl.example", arrivalTime.Add(4*time.Minute))
		late := insert(smtpedgeservice.SmtpComplaintTypeFraud, "fbl.example", arrivalTime.Add(50*time.Minute))

		cursor := AbuseSmtpComplaintCursor{ArrivalTime: arrivalTime.Add(-time.Minute)}
		reports, err := ListSmtpComplaintReports(ctx, cursor, arrivalTime.Add(10*time.Minute), 10)
		if err != nil || len(reports) != 2 || reports[0].ComplaintId != first || reports[1].ComplaintId != second {
			t.Fatalf("the reports %+v, %v", reports, err)
		}
		if reports[0].ReporterClass != smtpedgeservice.SmtpReporterClassLoop || reports[0].JoinResult != "medium" ||
			reports[0].Domain != "quietnet.root.example" || reports[1].Category != smtpedgeservice.SmtpComplaintTypeVirus {
			t.Fatalf("the first report reads %+v", reports[0])
		}
		next := AbuseSmtpComplaintCursor{ArrivalTime: reports[0].ArrivalTime, ComplaintId: server.Id(reports[0].ComplaintId)}
		if reports, err := ListSmtpComplaintReports(ctx, next, server.NowUtc(), 10); err != nil || len(reports) != 2 || reports[1].ComplaintId != late {
			t.Fatalf("past the first %+v, %v", reports, err)
		}

		haltIds := []sidecar.Id{}
		for _, offset := range []time.Duration{0, time.Minute, 2 * time.Minute} {
			halt := &smtpedgeservice.SmtpHalt{HaltId: sidecar.NewId(), NetworkId: networkId, Trigger: smtpedgeservice.SmtpHaltTriggerRecordCuts, StartTime: arrivalTime.Add(offset)}
			if err := SetSmtpHalt(ctx, halt); err != nil {
				t.Fatal(err)
			}
			haltIds = append(haltIds, halt.HaltId)
		}
		halts, err := ListUnrecordedSmtpHalts(ctx, arrivalTime, AbuseSmtpHaltCursor{}, 2)
		if err != nil || len(halts) != 2 || halts[0].HaltId != haltIds[0] || halts[1].HaltId != haltIds[1] {
			t.Fatalf("the halts %+v, %v", halts, err)
		}
		halts, err = ListUnrecordedSmtpHalts(ctx, arrivalTime, AbuseSmtpHaltCursor{StartTime: halts[1].StartTime, HaltId: server.Id(halts[1].HaltId)}, 2)
		if err != nil || len(halts) != 1 || halts[0].HaltId != haltIds[2] {
			t.Fatalf("past the second %+v, %v", halts, err)
		}
		if halts, err := ListUnrecordedSmtpHalts(ctx, arrivalTime.Add(90*time.Second), AbuseSmtpHaltCursor{}, 10); err != nil || len(halts) != 1 {
			t.Fatalf("since the third's start %+v, %v", halts, err)
		}
	})
}
