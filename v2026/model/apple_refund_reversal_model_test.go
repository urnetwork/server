package model

import (
	"testing"

	"github.com/urnetwork/connect/v2026"
)

// Only a refund's own end of the window counts for a REFUND_REVERSED: the
// REFUND notification's revoked event that ended something, or the reconcile's
// end when the store reported the transaction revoked. A family-sharing REVOKE,
// a REFUND that found nothing left to end, and an expiry the reconcile ended
// are not the refund's to give back.
func TestAppleRefundEndedEventCountsOnlyARefundsEnd(t *testing.T) {
	cases := []struct {
		action  string
		details string
		ended   bool
	}{
		{PaymentReconcileActionRevoked, `{"notification_type":"REFUND","subtype":"","ended":true}`, true},
		{PaymentReconcileActionRevoked, `{"notification_type":"REFUND","subtype":"","ended":false}`, false},
		{PaymentReconcileActionRevoked, `{"notification_type":"REVOKE","subtype":"","ended":true}`, false},
		{PaymentReconcileActionEnded, `{"status":5}`, true},
		{PaymentReconcileActionEnded, `{"status":2}`, false},
		{PaymentReconcileActionEnded, `{}`, false},
		{PaymentReconcileActionDisputed, `{"ended":true}`, false},
		{PaymentReconcileActionRevoked, `not json`, false},
	}
	for _, c := range cases {
		connect.AssertEqual(t, appleRefundEndedEvent(c.action, c.details), c.ended)
	}
}
