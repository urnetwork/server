package model

// Exercise the real profile/auth readers with one client slot. A callback
// retaining that slot while requesting another cannot finish at any load.
import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Only disposable fixture configuration changes; callers still use the normal
// public model entry point and pool acquisition/retry implementation.
func withNetworkUserSingleConnection(t testing.TB, do func()) {
	t.Helper()
	pop := server.Config.PushSimpleResource("db.yml", []byte("min_connections: 0\nmax_connections: 1\n"))
	server.PgReset()
	defer func() { pop(); server.PgReset() }()
	do()
}

// Eight concurrent profile requests must all preserve the four independent
// authentication results while sharing one reusable pool connection.
func TestGetNetworkUserConcurrentSinglePoolConnection(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		userId := server.NewId()
		userAuth := Testing_CreateNetwork(ctx, server.NewId(), "synthetic-profile", userId)
		created := server.NowUtc().Truncate(time.Second)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_user_auth_sso
				(user_id,auth_type,auth_jwt,user_auth) VALUES ($1,'google','synthetic-not-a-token','synthetic@example.invalid')`, userId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_user_auth_wallet
				(user_id,wallet_address,blockchain) VALUES ($1,'synthetic-not-a-wallet','solana')`, userId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_user_auth_seedphrase
				(user_id,seedphrase_lookup,seedphrase_hash,seedphrase_salt,create_time)
				VALUES ($1,$2,$2,$2,$3)`, userId, []byte("synthetic-not-a-secret"), created))
		})
		withNetworkUserSingleConnection(t, func() {
			ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			type outcome struct {
				user *NetworkUser
				err  any
			}
			start := make(chan struct{})
			finished := make(chan outcome, 8)
			for range 8 {
				go func() {
					<-start
					var result outcome
					result.err = server.HandleError(func() { result.user = GetNetworkUser(ctx, userId) })
					finished <- result
				}()
			}
			close(start)
			failed, mismatched := 0, 0
			for range 8 {
				result := <-finished
				if result.err != nil || result.user == nil {
					failed++
					continue
				}
				user := result.user
				if user.UserId != userId || user.NetworkName != "synthetic-profile" || !user.Verified ||
					len(user.UserAuths) != 1 || user.UserAuths[0].UserAuth != userAuth ||
					len(user.SsoAuths) != 1 || user.SsoAuths[0].AuthType != SsoAuthTypeGoogle ||
					len(user.WalletAuths) != 1 || user.WalletAuths[0].Blockchain != "solana" ||
					len(user.SeedphraseAuths) != 1 || !user.SeedphraseAuths[0].CreateTime.Equal(created) ||
					len(user.AuthTypes) != 4 || !slices.Contains(user.AuthTypes, "seedphrase") {
					mismatched++
				}
			}
			if failed != 0 || mismatched != 0 {
				t.Fatalf("single-slot profiles failed=%d mismatched=%d; all eight must finish with all four auth families", failed, mismatched)
			}
		})
	})
}

// A canceled profile cannot retain the only slot or cancel an independent
// request, and the missing-user path must still return nil without auth reads.
func TestGetNetworkUserCanceledAndMissingSinglePoolConnection(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		userId := server.NewId()
		Testing_CreateNetwork(context.Background(), server.NewId(), "synthetic-profile-cancel", userId)
		withNetworkUserSingleConnection(t, func() {
			canceled, cancel := context.WithCancel(context.Background())
			cancel()
			if server.HandleError(func() { GetNetworkUser(canceled, userId) }) == nil {
				t.Fatal("canceled profile unexpectedly succeeded")
			}
			ctx, finish := context.WithTimeout(context.Background(), 3*time.Second)
			defer finish()
			var user *NetworkUser
			if err := server.HandleError(func() { user = GetNetworkUser(ctx, userId) }); err != nil || user == nil {
				t.Fatal("independent profile could not reuse the slot after cancellation")
			}
			if missing := GetNetworkUser(ctx, server.NewId()); missing != nil {
				t.Fatal("missing profile was manufactured")
			}
		})
	})
}
