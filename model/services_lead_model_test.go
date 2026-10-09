package model

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// DB-backed tests (postgres + redis). Under the owner rule these run only
// after the branch, with its migrations, is merged to main.

func countServicesLeads(ctx context.Context, leadId server.Id) (count int) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT COUNT(*) FROM services_lead WHERE lead_id = $1`, leadId)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&count))
			}
		})
	})
	return
}

func TestServicesContactSales(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		// never reach a real webhook from a test
		defer server.Vault.PushSimpleResource(servicesSalesVaultResource, []byte("slack_webhook_url: \"\"\n"))()
		servicesSalesConfigCache.Store(nil)

		// a public request: no token, an address the rate limit counts
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		clientSession.ClientAddress = "203.0.113.7:40000"

		// a valid lead is stored, trimmed
		result, err := ServicesContactSales(validServicesContactSalesArgs(), clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, (*ServicesContactSalesError)(nil))
		connect.AssertNotEqual(t, result.RequestId, (*server.Id)(nil))
		connect.AssertEqual(t, countServicesLeads(ctx, *result.RequestId), 1)
		var name, email string
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `SELECT name, email FROM services_lead WHERE lead_id = $1`, *result.RequestId)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&name, &email))
				}
			})
		})
		connect.AssertEqual(t, name, "Ada Lovelace")
		connect.AssertEqual(t, email, "ada@example.com")

		// the honeypot answers like a success and stores nothing
		honeypot := validServicesContactSalesArgs()
		honeypot.Website = "https://spam.example"
		result, err = ServicesContactSales(honeypot, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, (*ServicesContactSalesError)(nil))
		connect.AssertNotEqual(t, result.RequestId, (*server.Id)(nil))
		connect.AssertEqual(t, countServicesLeads(ctx, *result.RequestId), 0)

		// a validation failure is 200 with error.message
		invalid := validServicesContactSalesArgs()
		invalid.Email = "not-an-email"
		result, err = ServicesContactSales(invalid, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.RequestId, (*server.Id)(nil))
		connect.AssertEqual(t, result.Error.Message, "Please enter a valid email address.")

		// every request counts (the three above too): past the per-address
		// budget the answer is 429
		for range servicesLeadAddressAttemptsPerHour - 3 {
			_, err = ServicesContactSales(validServicesContactSalesArgs(), clientSession)
			connect.AssertEqual(t, err, nil)
		}
		_, err = ServicesContactSales(validServicesContactSalesArgs(), clientSession)
		var rateLimit *rateLimitError
		if !errors.As(err, &rateLimit) || !strings.HasPrefix(rateLimit.Error(), "429 ") {
			t.Fatalf("expected a 429 refusal, got %v", err)
		}

		// another address still has its budget
		otherSession := session.Testing_CreateClientSession(ctx, nil)
		otherSession.ClientAddress = "198.51.100.9:40000"
		_, err = ServicesContactSales(validServicesContactSalesArgs(), otherSession)
		connect.AssertEqual(t, err, nil)
	})
}
