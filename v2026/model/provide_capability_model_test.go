package model

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// The declaration a client's latest provide with a Public or Network key made
// is the one discovery reads: a repeated provide writes nothing, a provide
// that stops declaring removes it, and a provide with neither key, as a
// window client's return-traffic provide, never writes one.
func TestSetProvideWithCapabilitiesKeepsTheLatestDeclaration(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		newClient := func() server.Id {
			clientId := server.NewId()
			Testing_CreateDevice(ctx, server.NewId(), server.NewId(), clientId, "", "")
			return clientId
		}
		declared := func(clientId server.Id) bool {
			capableClientIds, err := readProvidePostQuantumEncryption(ctx, []server.Id{clientId})
			connect.AssertEqual(t, err, nil)
			return capableClientIds[clientId]
		}
		// the stored row's change time, and whether there is a row
		row := func(clientId server.Id) (changeTime time.Time, ok bool) {
			server.Db(ctx, func(conn server.PgConn) {
				err := conn.QueryRow(
					ctx,
					`SELECT change_time FROM provide_capability WHERE client_id = $1`,
					clientId,
				).Scan(&changeTime)
				if errors.Is(err, pgx.ErrNoRows) {
					return
				}
				server.Raise(err)
				ok = true
			})
			return
		}
		postQuantum := ProvideCapabilities{PostQuantumEncryption: true}
		public := map[ProvideMode][]byte{ProvideModePublic: []byte("public-secret")}

		clientId := newClient()
		SetProvideWithCapabilities(ctx, clientId, public, postQuantum)
		connect.AssertEqual(t, declared(clientId), true)
		firstChangeTime, ok := row(clientId)
		connect.AssertEqual(t, ok, true)

		// every connection announces the same provide again
		SetProvideWithCapabilities(ctx, clientId, public, postQuantum)
		secondChangeTime, ok := row(clientId)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, secondChangeTime, firstChangeTime)

		// the provide of a release before the declaration declares nothing
		SetProvide(ctx, clientId, public)
		connect.AssertEqual(t, declared(clientId), false)
		_, ok = row(clientId)
		connect.AssertEqual(t, ok, false)

		// a Network key alone is offered to the provider's own network
		SetProvideWithCapabilities(ctx, clientId, map[ProvideMode][]byte{ProvideModeNetwork: []byte("network-secret")}, postQuantum)
		connect.AssertEqual(t, declared(clientId), true)

		// a return-traffic provide is never offered by discovery
		streamClientId := newClient()
		SetProvideWithCapabilities(ctx, streamClientId, map[ProvideMode][]byte{ProvideModeStream: []byte("stream-secret")}, postQuantum)
		connect.AssertEqual(t, declared(streamClientId), false)
		_, ok = row(streamClientId)
		connect.AssertEqual(t, ok, false)

		// a client that never provided has declared nothing, and an empty
		// read reads nothing
		connect.AssertEqual(t, declared(newClient()), false)
		capableClientIds, err := readProvidePostQuantumEncryption(ctx, nil)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(capableClientIds), 0)
	})
}
