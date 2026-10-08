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
	tx := &providerWorkReadyTx{}
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
