// Wallet removal must share one transaction with payout-selection cleanup so
// it needs only one pool slot and both changes retain the same commit outcome.
package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

// Use generated identities and the real wallet creators without an external
// wallet, payment processor, or network request.
func walletRemovalPoolFixture(t testing.TB, ctx context.Context) (server.Id, server.Id) {
	t.Helper()
	networkId := server.NewId()
	owner := session.NewLocalClientSession(ctx, "192.0.2.1:443", &jwt.ByJwt{NetworkId: networkId})
	defer owner.Cancel()
	walletId := CreateAccountWalletExternal(owner, &CreateAccountWalletExternalArgs{
		NetworkId: networkId, Blockchain: "MATIC", WalletAddress: "synthetic-pool-wallet", DefaultTokenType: "USDC",
	})
	if walletId == nil {
		t.Fatal("wallet fixture was not created")
	}
	server.Raise(SetPayoutWallet(ctx, networkId, *walletId))
	return networkId, *walletId
}

// Inspect the raw selection as well as active state: the public selection
// reader intentionally hides a stale row and cannot prove its cleanup.
func walletRemovalPoolState(ctx context.Context, networkId, walletId server.Id) (active, selected bool) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT active,
			EXISTS(SELECT 1 FROM payout_wallet WHERE network_id=$1 AND wallet_id=$2)
			FROM account_wallet WHERE network_id=$1 AND wallet_id=$2`, networkId, walletId).Scan(&active, &selected))
	})
	return
}

// Eight real callers share one reusable slot. Retaining the outer transaction
// while opening an independent cleanup transaction cannot finish at any load.
func TestRemoveWalletConcurrentSinglePoolConnection(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		type fixture struct{ networkId, walletId server.Id }
		fixtures := make([]fixture, 8)
		for i := range fixtures {
			fixtures[i].networkId, fixtures[i].walletId = walletRemovalPoolFixture(t, ctx)
		}
		withNetworkUserSingleConnection(t, func() {
			bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			type outcome struct {
				result *RemoveWalletResult
				err    error
			}
			start := make(chan struct{})
			finished := make(chan outcome, len(fixtures))
			for _, f := range fixtures {
				go func() {
					<-start
					owner := session.NewLocalClientSession(bounded, "192.0.2.1:443", &jwt.ByJwt{NetworkId: f.networkId})
					defer owner.Cancel()
					var out outcome
					server.HandleError(func() { out.result = RemoveWallet(f.walletId, owner) }, func(err error) { out.err = err })
					finished <- out
				}()
			}
			close(start)
			failed := 0
			for range fixtures {
				out := <-finished
				if out.err != nil || out.result == nil || !out.result.Success || out.result.Error != nil {
					failed++
				}
			}
			if failed != 0 {
				t.Errorf("single-slot removals failed=%d/%d; each must finish using one transaction", failed, len(fixtures))
			}
		})
		for _, f := range fixtures {
			if active, selected := walletRemovalPoolState(ctx, f.networkId, f.walletId); active || selected {
				t.Errorf("completed removal retained active=%t selected=%t", active, selected)
			}
		}
	})
}

// A deferred failure occurs after both statements execute. Neither wallet
// deactivation nor payout-selection cleanup may escape the failed commit.
func TestRemoveWalletCommitFailurePreservesSelection(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId, walletId := walletRemovalPoolFixture(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION synthetic_wallet_removal_commit_failure()
				RETURNS trigger LANGUAGE plpgsql AS $function$
				BEGIN
					IF OLD.active AND NOT NEW.active THEN
						RAISE EXCEPTION 'synthetic wallet removal commit failure' USING ERRCODE='P0001';
					END IF;
					RETURN NEW;
				END $function$;
				CREATE CONSTRAINT TRIGGER synthetic_wallet_removal_commit_failure
				AFTER UPDATE ON account_wallet DEFERRABLE INITIALLY DEFERRED
				FOR EACH ROW EXECUTE FUNCTION synthetic_wallet_removal_commit_failure()`))
		})
		owner := session.NewLocalClientSession(ctx, "192.0.2.1:443", &jwt.ByJwt{NetworkId: networkId})
		defer owner.Cancel()
		var err error
		server.HandleError(func() { RemoveWallet(walletId, owner) }, func(cause error) { err = cause })
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "P0001" {
			t.Fatalf("owning commit failure was not exercised: %v", err)
		}
		if active, selected := walletRemovalPoolState(ctx, networkId, walletId); !active || !selected {
			t.Errorf("failed commit escaped transaction: active=%t selected=%t, want both true", active, selected)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER synthetic_wallet_removal_commit_failure ON account_wallet;
				DROP FUNCTION synthetic_wallet_removal_commit_failure()`))
		})
		if result := RemoveWallet(walletId, owner); !result.Success || result.Error != nil {
			t.Fatal("removal could not recover after failed commit")
		}
		if active, selected := walletRemovalPoolState(ctx, networkId, walletId); active || selected {
			t.Fatal("successful retry did not commit both removal effects")
		}
	})
}

// Refused and canceled callers keep ownership intact; cancellation releases
// the only pool slot so an independent authorized owner can complete afterward.
func TestRemoveWalletSinglePoolOwnershipAndCancellation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId, walletId := walletRemovalPoolFixture(t, ctx)
		withNetworkUserSingleConnection(t, func() {
			bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			stranger := session.NewLocalClientSession(bounded, "192.0.2.2:443", &jwt.ByJwt{NetworkId: server.NewId()})
			defer stranger.Cancel()
			if result := RemoveWallet(walletId, stranger); result.Success {
				t.Fatal("foreign network removed wallet")
			}
			owner := session.NewLocalClientSession(bounded, "192.0.2.1:443", &jwt.ByJwt{NetworkId: networkId})
			defer owner.Cancel()
			if result := RemoveWallet(server.NewId(), owner); result.Success {
				t.Fatal("missing wallet removal succeeded")
			}
			canceled := session.NewLocalClientSession(bounded, "192.0.2.1:443", &jwt.ByJwt{NetworkId: networkId})
			canceled.Cancel()
			if err := server.HandleError(func() { RemoveWallet(walletId, canceled) }); err == nil {
				t.Fatal("canceled owner removed wallet")
			}
			if active, selected := walletRemovalPoolState(bounded, networkId, walletId); !active || !selected {
				t.Fatal("refused or canceled removal changed wallet ownership")
			}
			if result := RemoveWallet(walletId, owner); !result.Success || result.Error != nil {
				t.Fatal("authorized caller could not reuse the slot after cancellation")
			}
		})
		if active, selected := walletRemovalPoolState(ctx, networkId, walletId); active || selected {
			t.Fatal("authorized removal did not commit both effects")
		}
	})
}
