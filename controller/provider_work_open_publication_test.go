//go:build linux || darwin || freebsd

// Actual SDK admissions remain unclosed while the publisher obtains its live
// boundary observation from the separately configured original source owner.
package controller

import (
	"bytes"
	"context"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/urfoundation/sn/payoutartifact"
	snprotocol "github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The same real ingress, SDK capture, signed roster, payout and public reader
// used for settled work can prove an explicit open contract without earnings.
func TestProviderWorkActualOpenSdkPublishesOriginalBoundaryObservation(t *testing.T) {
	providerWorkActualSdkPublication(t, false, true, false, func(t testing.TB, ctx context.Context, f *providerWorkWindowFixture, artifact *payoutartifact.Artifact) {
		window := artifact.ClosedWork.WholeInventory.Window
		if len(window.Records) != 1 || window.Records[0].Disposition != "open" || len(artifact.ClosedWork.Records) != 0 || len(artifact.Leaves) != 0 {
			t.Fatal("still-open admission acquired a terminal original or earning leaf")
		}
		id, err := server.ParseId(window.Records[0].ContractId)
		if err != nil {
			t.Fatal(err)
		}
		var outcome *string
		var retained []byte
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT outcome FROM transfer_contract WHERE contract_id=$1`, id).Scan(&outcome))
			server.Raise(conn.QueryRow(ctx, `SELECT original FROM provider_work_open_original WHERE contract_id=$1 AND epoch=$2::numeric AND block_hash=$3`, id, artifact.Epoch, common.HexToHash(artifact.End.Hash).Bytes()).Scan(&retained))
		})
		if outcome != nil {
			t.Fatal("fixture closed the live contract before its boundary observation")
		}
		found := false
		for _, raw := range artifact.ClosedWork.WholeInventory.AttributionOriginals {
			if bytes.Equal(raw, retained) {
				found = true
			}
		}
		original, err := snprotocol.DecodeProviderWorkReceipt(ctx, retained)
		if err != nil || original.Open == nil || !found || original.Open.ContractId != id.String() || original.Open.Epoch != artifact.Epoch || original.Open.Block != artifact.End.Number || original.Open.BlockHash != [32]byte(common.HexToHash(artifact.End.Hash)) || original.Open.BoundaryUnixMicro != f.epoch.EndTime.UnixMicro() || original.Open.ObservedAtUnixMicro < f.epoch.EndTime.UnixMicro() {
			t.Fatal("public inventory changed or omitted the exact live observation", err)
		}
		// A retired signer can still read the first exact observation; obtaining
		// the public companion must not require a fresh signature or overwrite.
		keyless := model.WithProviderWorkSessionSource(ctx, nil)
		replayed, err := model.RetainProviderWorkOpenObservations(keyless, []server.Id{id}, artifact.Epoch, artifact.End.Number, [32]byte(common.HexToHash(artifact.End.Hash)), f.epoch.EndTime)
		if err != nil || len(replayed) != 1 || !bytes.Equal(replayed[0], retained) {
			t.Fatal("keyless retry lost the first original observation", err)
		}
	})
}

// Ordinary admission and actual SDK cuts still succeed without current source
// signing authority. Publication waits; it cannot relabel an SQL open row.
func TestProviderWorkActualOpenSdkWaitsForOriginalObservationAuthority(t *testing.T) {
	providerWorkActualSdkPublication(t, false, true, true)
}
