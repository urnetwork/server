package model

import (
	"context"
	"testing"

	"github.com/urnetwork/server"
)

// A processor submit failure is recorded through the generic audit entry
// point, which used to drop account payment events.
func TestAddAuditEventPersistsAccountPaymentEvent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		paymentId := server.NewId()
		details := `{"error":"synthetic processor rejection"}`
		event := NewAuditAccountPaymentEvent(AuditEventTypeCirclePayoutFailed)
		event.AccountPaymentId = paymentId
		event.EventDetails = &details
		AddAuditEvent(ctx, event)

		server.Db(ctx, func(conn server.PgConn) {
			var count int
			var stored *string
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*), MAX(event_details) FROM audit_account_payment
				WHERE payment_id=$1 AND event_type=$2 AND event_id=$3`, paymentId, AuditEventTypeCirclePayoutFailed, event.EventId).Scan(&count, &stored))
			if count != 1 || stored == nil || *stored != details {
				t.Fatalf("account payment audit event was not recorded: count=%d details=%v", count, stored)
			}
		})
	})
}
