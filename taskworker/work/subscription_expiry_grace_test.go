// Expiry must follow protocol retirement instead of converting a still-live
// receive owner into a synthetic missing report.
package work

import (
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// Walk the ordinary idle sequence using virtual timestamps, including one
// application interval before the final payload, without sleeping.
func TestCloseExpiredContractsQuietGraceKeepsReceiverReport(t *testing.T) {
	created := time.Unix(10000, 0).UTC()
	lastPayload := created.Add(30 * time.Second)
	senderFinished := lastPayload.Add(connect.DefaultSendBufferSettings().IdleTimeout)
	receiverReported := lastPayload.Add(connect.DefaultReceiveBufferSettings().IdleTimeout)
	if !senderFinished.Before(receiverReported) {
		t.Fatal("fixture requires the receive owner to outlive its sender")
	}
	for _, sweep := range []time.Time{
		created.Add(5*time.Minute + time.Second),
		senderFinished.Add(time.Second),
		receiverReported.Add(time.Second),
	} {
		if !created.After(closeExpiredContractsCutoff(sweep)) {
			t.Fatalf("expiry at %s retired the contract before receiver reporting grace through %s", sweep.Sub(created), receiverReported.Sub(created))
		}
	}
}

// Couple the server default to both protocol owners; a future transport
// lifetime increase must not silently overtake settlement's waiting period.
func TestCloseExpiredContractsQuietGraceCoversProtocolOwners(t *testing.T) {
	now := time.Unix(10000, 0).UTC()
	quietGrace := now.Sub(closeExpiredContractsCutoff(now))
	required := 2 * max(connect.DefaultSendBufferSettings().IdleTimeout, connect.DefaultReceiveBufferSettings().IdleTimeout)
	if quietGrace < required {
		t.Fatalf("settlement quiet grace %s is shorter than two protocol owner lifetimes %s", quietGrace, required)
	}
	if quietGrace > 12*time.Minute {
		t.Fatalf("settlement grace escaped its reviewed finite bound: %s", quietGrace)
	}
}

// A silent owner becomes eligible at the fixed boundary, while the model's
// newer report timestamp can retain it without changing this global clock.
func TestCloseExpiredContractsQuietGraceHasExactFiniteBoundary(t *testing.T) {
	lastReport := time.Unix(10000, 0).UTC()
	deadline := lastReport.Add(12 * time.Minute)
	if !lastReport.After(closeExpiredContractsCutoff(deadline.Add(-time.Nanosecond))) {
		t.Fatal("contract became eligible before its quiet grace elapsed")
	}
	if !lastReport.Equal(closeExpiredContractsCutoff(deadline)) {
		t.Fatal("silent contract did not become eligible at the exact quiet deadline")
	}
	if !lastReport.Before(closeExpiredContractsCutoff(deadline.Add(time.Nanosecond))) {
		t.Fatal("silent contract remained retained after its finite quiet deadline")
	}
}
