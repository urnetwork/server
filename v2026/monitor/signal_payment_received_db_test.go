package monitor

// Database-backed root-cause test of the received-but-uncredited payment
// alerts (SIGNALS.md §2.22): a crypto payment the scanner recorded and never
// credited, and one held for review that no other class names, alert. Before
// these kinds the payment-failures signal read only four review reasons of
// receipts tied to an invoice, so a payment stuck below its credit, one whose
// credit kept failing, and one to an address no invoice was given paged
// nothing, as the lost Solana payments of support inbox 6584, 6692 and 6729
// paged nothing. The signal's own query runs against the test database
// (server.DefaultTestEnv); the test names only the signal, so it runs
// unchanged against the code before the kinds.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Answers the signal's PostgreSQL query from the test database, each value as
// text.
func paymentReceivedTestSource(t testing.TB, ctx context.Context) *syntheticSource {
	return &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		rows := []Row{}
		var queryErr error
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, query)
			if err != nil {
				queryErr = err
				return
			}
			defer result.Close()
			for result.Next() {
				values, err := result.Values()
				if err != nil {
					queryErr = err
					return
				}
				row := Row{}
				for _, value := range values {
					row = append(row, fmt.Sprint(value))
				}
				rows = append(rows, row)
			}
			queryErr = result.Err()
		})
		return rows, queryErr
	}}
}

// A receipt stuck in a block for three hours and one whose credit failed page
// for xmr, a zec payment to an address no invoice was given warns, and a
// credited or young receipt alerts nothing; no identifier reaches the alerts.
func TestPaymentFailuresAlertsReceivedPaymentsNeverCredited(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		receipt := func(asset string, txId string, state string, reason *string, firstSeen time.Time) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
						INSERT INTO crypto_payment_receipt
						(asset, tx_id, output_key, intent_id, address, address_index, amount_atomic,
						 confirmations, first_seen_at, state, review_reason)
						VALUES ($1, $2, '0', NULL, 'synthetic-address', 7, 1000, 1, $3, $4, $5)
					`,
					asset,
					txId,
					firstSeen,
					state,
					reason,
				))
			})
		}
		unknownAddress := "unknown_address"
		creditFailed := "credit_failed"
		// stuck in a block for three hours: the scanner never credited it
		receipt("xmr", "stuck", "confirmed", nil, now.Add(-3*time.Hour))
		// its credit failed five times
		receipt("xmr", "failing", "review", &creditFailed, now.Add(-time.Hour))
		// paid to an address no invoice was given, an hour ago
		receipt("zec", "stray", "review", &unknownAddress, now.Add(-time.Hour))
		// credited, and young: neither alerts
		receipt("xmr", "credited", "credited", nil, now.Add(-3*time.Hour))
		receipt("xmr", "young", "confirmed", nil, now.Add(-10*time.Minute))

		alerts, err := NewPaymentFailuresSignal().Run(ctx, syntheticSettings(paymentReceivedTestSource(t, ctx)))
		if err != nil {
			t.Fatal(err)
		}
		uncredited := requireAlertClass(t, alerts, "payment-received-uncredited")
		if uncredited.Severity != SeverityPage {
			t.Fatalf("uncredited severity = %s, want page", uncredited.Severity)
		}
		held := requireAlertClass(t, alerts, "payment-received-held")
		if held.Severity != SeverityWarn {
			t.Fatalf("held severity = %s, want warn", held.Severity)
		}
		for _, alert := range []Alert{uncredited, held} {
			for _, forbidden := range []string{"stuck", "failing", "stray", "synthetic-address"} {
				requireAlertOmits(t, alert, forbidden)
			}
		}
		markdown := uncredited.Markdown()
		for _, want := range []string{"target=xmr", "count=2"} {
			if !strings.Contains(markdown, want) {
				t.Fatalf("uncredited alert lacks %q:\n%s", want, markdown)
			}
		}
		if markdown := held.Markdown(); !strings.Contains(markdown, "target=zec") || !strings.Contains(markdown, "count=1") {
			t.Fatalf("held alert does not count the stray zec payment:\n%s", markdown)
		}
	})
}
