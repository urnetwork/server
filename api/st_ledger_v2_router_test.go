package api

import (
	"context"
	"errors"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/startifact"
)

// §8.2 (server start), SV-3 (wiring): the API router, which the api and alt
// services both serve, installs the server's settlement v2 ledger builder
// when v2 is configured, so the ledger routes read committed trees through
// it: a read of a ledger missing from the store reaches the store. Without
// v2 it installs none.
func TestRouterInstallsTheLedgerBuilder(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		controller.SetStLedgerV2ObjectStore(server.NewLocalBlobStore(t.TempDir(), "test"))
		defer controller.SetStLedgerV2ObjectStore(nil)
		defer controller.SetStLedgerBuilder(nil)
		defer controller.SetStConfig(nil)
		for _, v2 := range []bool{false, true} {
			cfg := &controller.StConfig{Enabled: true}
			if v2 {
				cfg.SettlementV2, cfg.VaultV2 = common.Address{1}, common.Address{2}
			}
			controller.SetStConfig(cfg)
			controller.SetStLedgerBuilder(nil)
			ctx, cancel := context.WithCancel(t.Context())
			_, closeRouter, err := NewRouter(ctx, ctx)
			if err != nil {
				t.Fatal(err)
			}
			closeRouter()
			cancel()
			_, _, err = controller.StLedgerV2Tree(t.Context(), [32]byte{7}, [32]byte{}, new(uint256.Int), 1)
			if v2 && !errors.Is(err, startifact.ErrLedgerObjectNotFound) || !v2 && (err == nil || err.Error() != "st ledger v2: no ledger builder is installed") {
				t.Fatalf("v2 configured %t: %v", v2, err)
			}
		}
	})
}
