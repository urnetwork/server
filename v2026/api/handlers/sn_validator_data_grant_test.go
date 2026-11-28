package handlers

// The SN25 validator data grant at the rate class mint (POST
// /sn/validator-class) over the reserved fixture's real admission and the
// local database: an admitted mint grants the validator's network 350 GiB for
// 30 days once while that grant is active; a refused mint grants nothing.

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// One balance of a network, as the database holds it.
type snValidatorDataGrantTestBalance struct {
	startTime             time.Time
	endTime               time.Time
	startBalanceByteCount model.ByteCount
	netRevenue            model.NanoCents
	pro                   bool
	paid                  bool
}

// Every balance of the network, oldest first.
func snValidatorDataGrantTestBalances(tb testing.TB, networkId server.Id) []snValidatorDataGrantTestBalance {
	tb.Helper()
	balances := []snValidatorDataGrantTestBalance{}
	server.Db(tb.Context(), func(conn server.PgConn) {
		result, err := conn.Query(
			tb.Context(),
			`
				SELECT start_time, end_time, start_balance_byte_count, net_revenue_nano_cents, pro, paid
				FROM transfer_balance
				WHERE network_id = $1
				ORDER BY start_time
			`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var balance snValidatorDataGrantTestBalance
				server.Raise(result.Scan(
					&balance.startTime,
					&balance.endTime,
					&balance.startBalanceByteCount,
					&balance.netRevenue,
					&balance.pro,
					&balance.paid,
				))
				balances = append(balances, balance)
			}
		})
	})
	return balances
}

// The production route: an admitted validator's first mint grants its
// network 350 GiB for 30 days, data only at no cost, and its next mint while
// that grant is active grants nothing. With pro.yml carrying no
// validator_grant keys, these are the defaults.
func TestSnValidatorClassAdmittedMintGrantsDataOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		if model.Pro().MaxConcurrentClients(true) == 0 {
			tb.Skip("pro.yml is not present in this environment; no grant runs without it")
		}
		authority := newSnReservedAttemptAuthority(tb)
		endpoint := httptest.NewServer(SnValidatorClassWithReserved(authority.start(tb, 1)))
		tb.Cleanup(endpoint.Close)
		_, client := snAttemptUploadTestIdentity(tb)
		credential := client.Testing_Sign()

		before := time.Now()
		if status, body := snValidatorClassTestRequest(tb, endpoint.URL, authority, credential, credential, authority.key, 1); status != http.StatusOK {
			tb.Fatalf("an admitted validator's mint = %d %q", status, body)
		}
		after := time.Now()
		balances := snValidatorDataGrantTestBalances(tb, client.NetworkId)
		if len(balances) != 1 {
			tb.Fatalf("the admitted mint left %d balances on the validator's network, want 1 grant", len(balances))
		}
		grant := balances[0]
		if grant.startTime.Before(before.Add(-time.Second)) || grant.startTime.After(after.Add(time.Second)) {
			tb.Errorf("the grant starts at %s, not at the mint (%s to %s)", grant.startTime, before, after)
		}
		if grant.startBalanceByteCount != 350*model.Gib || grant.endTime.Sub(grant.startTime) != 30*24*time.Hour {
			tb.Errorf("the grant is %d bytes for %s, want 350 GiB for 30 days", grant.startBalanceByteCount, grant.endTime.Sub(grant.startTime))
		}
		if grant.pro || grant.paid || grant.netRevenue != 0 {
			tb.Errorf("the grant has pro %t, paid %t and revenue %d; want data only at no cost", grant.pro, grant.paid, grant.netRevenue)
		}

		if status, body := snValidatorClassTestRequest(tb, endpoint.URL, authority, credential, credential, authority.key, 1); status != http.StatusOK {
			tb.Fatalf("the validator's next mint = %d %q", status, body)
		}
		if got := len(snValidatorDataGrantTestBalances(tb, client.NetworkId)); got != 1 {
			tb.Errorf("the next mint while the grant is active left %d balances, want the 1 grant", got)
		}
	})
}
