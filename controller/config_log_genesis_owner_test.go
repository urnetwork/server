//go:build sidecar_pending

package controller

// base 1's owner check (configGenesisOwner, §7.9's who) against the credential
// QD-1194 lets a team's tagged lineage hold: one naming the team's owner of
// record with no member_id, which passes the state check as such a client's.
// A genesis naming the owner of record as its owner member must not make that
// credential the genesis's owner, while a personal network's owner, whose
// credential has the same shape, still is.

import (
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

func TestConfigGenesisOwnerRefusesATeamsOwnerOfRecord(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		personalNetworkId, ownerUserId := server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, personalNetworkId, "personal-"+personalNetworkId.String(), ownerUserId)
		teamNetworkId, ownerOfRecordId := server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, teamNetworkId, "team-"+teamNetworkId.String(), ownerOfRecordId)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_team (network_id, sso_required) VALUES ($1, false)`, teamNetworkId))
		})

		// a sidecar's client credential naming userId with no member_id, and a
		// genesis naming userId as its owner member
		genesisOwner := func(networkId server.Id, userId server.Id) bool {
			byJwt := session.NewByJwt(networkId, userId, "genesis-owner", false, false).Client(server.NewId(), server.NewId())
			byJwt.SetStrictExp(true)
			clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", byJwt)
			defer clientSession.Cancel()
			return configGenesisOwner(clientSession, &bridge.Genesis{OwnerMemberId: sidecar.Id(userId).Bytes()})
		}
		connect.AssertEqual(t, genesisOwner(personalNetworkId, ownerUserId), true)
		connect.AssertEqual(t, genesisOwner(teamNetworkId, ownerOfRecordId), false)
	})
}
