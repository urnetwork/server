// Expected evidence refusal is exact and cannot mask a different failure.
package model

import (
	"errors"
	"testing"

	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/server"
)

// A refusal marker cannot hide an independently attached integrity failure.
func TestProviderWorkOptionalRefusalDoesNotSwallowOtherCauses(t *testing.T) {
	ctx := WithProviderWorkSessionSource(t.Context(), &ProviderWorkSessionSource{})
	tx := &providerWorkSingleOwnerTx{}
	var raised error
	server.HandleError(func() {
		providerWorkOptionalInTx(ctx, tx, func(server.PgTx) error {
			return errors.Join(errProviderWorkEvidenceUnavailable, protocol.ErrProviderWorkIntegrity)
		})
	}, func(err error) { raised = err })
	if !errors.Is(raised, protocol.ErrProviderWorkIntegrity) {
		t.Fatal("evidence marker swallowed an integrity cause", raised)
	}
}

// Ordinary contract accounting accepts a self-pair, but the original-work
// protocol has no proof for it. Refuse that proof before protocol signing.
func TestProviderWorkSelfPairRefusesOnlyOriginal(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		id, err := CreateContractNoEscrow(f.ctx, f.sourceNetworkId, f.sourceId, f.sourceNetworkId, f.sourceId, 121)
		server.Raise(err)
		server.Db(f.ctx, func(conn server.PgConn) {
			var contract, original bool
			server.Raise(conn.QueryRow(f.ctx, `SELECT
 EXISTS(SELECT 1 FROM transfer_contract WHERE contract_id=$1),
 EXISTS(SELECT 1 FROM provider_work_reservation_original WHERE contract_id=$1)`, id).Scan(&contract, &original))
			if !contract || original {
				t.Fatal("unsupported optional proof changed self-pair accounting", contract, original)
			}
		})
	})
}
