package controller

import (
	"bytes"
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The ingest also stores the keyed egress a unique-exit allocation compares
// (model/proxy_unique_exit_model.go): the key of the exact address the echo
// saw, counted stable per probe, and still never the address itself.
func TestSubmitProviderEgressLocationStoresTheKeyedEgress(t *testing.T) {
	requireGeoLite2Database(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer model.Testing_SetUniqueExitKeySecret(bytes.Repeat([]byte{4}, 32))()
		ctx := context.Background()
		clientId := testEgressProvider(ctx)

		read := func() (key []byte, stable int) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(
					ctx,
					`SELECT egress_key, egress_key_stable_count FROM provider_egress_location WHERE client_id = $1`,
					clientId,
				).Scan(&key, &stable))
			})
			return
		}

		observedAt := server.NowUtc()
		for i := range 2 {
			_, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
				ClientId:   clientId,
				ExitIp:     testEgressCityExitIp,
				ObservedAt: observedAt.Add(time.Duration(i-1) * time.Minute),
			})
			connect.AssertEqual(t, err, nil)
		}
		key, stable := read()
		connect.AssertEqual(t, bytes.Equal(key, model.UniqueExitEgressKey(netip.MustParseAddr(testEgressCityExitIp))), true)
		connect.AssertEqual(t, stable, 2)

		if text := testEgressLocationRowText(ctx, clientId); strings.Contains(text, testEgressCityExitIp) {
			t.Fatalf("the stored row holds the exit address: %s", text)
		}
	})
}
