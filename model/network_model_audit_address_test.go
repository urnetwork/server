package model

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// audit_network_event rows are permanent (no reaper), so the network-create
// audit blob must carry the peppered address hash, never the raw ip:port.
func TestAuditNetworkCreateStoresAddressHash(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		clientAddress := "9.8.7.6:4321"
		expectedHash, err := server.ClientIpHash("9.8.7.6")
		assert.Equal(t, err, nil)

		clientSession := session.NewLocalClientSession(ctx, clientAddress, nil)
		defer clientSession.Cancel()

		networkId := server.NewId()
		auditNetworkCreate(
			NetworkCreateArgs{
				NetworkName: "audithashtest",
			},
			networkId,
			clientSession,
		)

		var eventDetails string
		server.Tx(ctx, func(tx server.PgTx) {
			result, err := tx.Query(
				ctx,
				`
					SELECT event_details
					FROM audit_network_event
					WHERE network_id = $1 AND event_type = $2
				`,
				networkId,
				AuditEventTypeNetworkCreated,
			)
			server.WithPgResult(result, err, func() {
				assert.Equal(t, result.Next(), true)
				server.Raise(result.Scan(&eventDetails))
			})
		})

		// the raw address must not appear anywhere in the stored blob
		if strings.Contains(eventDetails, "9.8.7.6") {
			t.Fatalf("audit event details contain the raw client ip: %s", eventDetails)
		}

		details := map[string]any{}
		assert.Equal(t, json.Unmarshal([]byte(eventDetails), &details), nil)
		_, hasRaw := details["client_address"]
		assert.Equal(t, hasRaw, false)
		assert.Equal(t, details["client_address_hash"], hex.EncodeToString(expectedHash[:]))
		assert.Equal(t, details["client_port"], float64(4321))
	})
}

// A session with no parseable address audits without address fields rather
// than failing or storing a zero hash.
func TestAuditNetworkCreateNoAddress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		clientSession := session.NewLocalClientSession(ctx, "", nil)
		defer clientSession.Cancel()

		networkId := server.NewId()
		auditNetworkCreate(
			NetworkCreateArgs{
				NetworkName: "auditnoaddrtest",
			},
			networkId,
			clientSession,
		)

		var eventDetails string
		server.Tx(ctx, func(tx server.PgTx) {
			result, err := tx.Query(
				ctx,
				`
					SELECT event_details
					FROM audit_network_event
					WHERE network_id = $1 AND event_type = $2
				`,
				networkId,
				AuditEventTypeNetworkCreated,
			)
			server.WithPgResult(result, err, func() {
				assert.Equal(t, result.Next(), true)
				server.Raise(result.Scan(&eventDetails))
			})
		})

		details := map[string]any{}
		assert.Equal(t, json.Unmarshal([]byte(eventDetails), &details), nil)
		_, hasRaw := details["client_address"]
		assert.Equal(t, hasRaw, false)
		_, hasHash := details["client_address_hash"]
		assert.Equal(t, hasHash, false)
	})
}
