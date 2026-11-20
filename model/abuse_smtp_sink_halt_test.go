//go:build sidecar_pending

package model

import (
	"context"
	"errors"
	"testing"

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
