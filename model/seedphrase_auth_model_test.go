package model

import (
	"context"
	"testing"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
)

func TestSeedphraseCreateAndLogin(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		userId := server.NewId()
		networkId := server.NewId()
		networkName := "seedphrase-test"

		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx,
				`INSERT INTO network_user (user_id, user_name, auth_type)
				 VALUES ($1, $2, $3)`,
				userId, networkName, AuthTypeSeedphrase,
			))
			server.RaisePgResult(tx.Exec(ctx,
				`INSERT INTO network (network_id, network_name, admin_user_id)
				 VALUES ($1, $2, $3)`,
				networkId, networkName, userId,
			))
		})

		// Test 1: Generate seedphrase
		seedphrase, err := GenerateSeedphrase(ctx, userId)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, len(seedphrase), 0)

		// Test 2: Has seedphrase
		hasSeedphrase, err := HasSeedphraseAuth(ctx, userId)
		assert.Equal(t, err, nil)
		assert.Equal(t, hasSeedphrase, true)

		// Test 3: Login with seedphrase
		loginResult, err := LoginWithSeedphrase(ctx, seedphrase)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, loginResult.ByJwt, "")

		// Test 4: Parse the JWT
		parsed, err := jwt.ParseByJwt(ctx, loginResult.ByJwt)
		assert.Equal(t, err, nil)
		assert.Equal(t, parsed.NetworkId, networkId)
		assert.Equal(t, parsed.UserId, userId)

		// Test 5: Regenerate
		newSeedphrase, err := RegenerateSeedphrase(ctx, userId)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, newSeedphrase, seedphrase)

		// Test 6: Login with new seedphrase
		loginResult2, err := LoginWithSeedphrase(ctx, newSeedphrase)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, loginResult2.ByJwt, "")

		// Test 7: Login with old seedphrase should fail
		_, err = LoginWithSeedphrase(ctx, seedphrase)
		assert.NotEqual(t, err, nil)
	})
}

func TestSeedphraseBadLogin(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		_, err := LoginWithSeedphrase(ctx, "this is not a valid bip39 mnemonic at all")
		assert.NotEqual(t, err, nil)
	})
}

func TestSeedphraseAlreadyExists(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		userId := server.NewId()
		networkId := server.NewId()
		networkName := "seedphrase-exists"

		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx,
				`INSERT INTO network_user (user_id, user_name, auth_type)
				 VALUES ($1, $2, $3)`,
				userId, networkName, AuthTypeSeedphrase,
			))
			server.RaisePgResult(tx.Exec(ctx,
				`INSERT INTO network (network_id, network_name, admin_user_id)
				 VALUES ($1, $2, $3)`,
				networkId, networkName, userId,
			))
		})

		_, err := GenerateSeedphrase(ctx, userId)
		assert.Equal(t, err, nil)

		// Second generate should fail
		_, err = GenerateSeedphrase(ctx, userId)
		assert.NotEqual(t, err, nil)
	})
}

func TestRemoveAuth(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		userId := server.NewId()
		networkId := server.NewId()
		networkName := "remove-auth-test"

		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx,
				`INSERT INTO network_user (user_id, user_name, auth_type)
				 VALUES ($1, $2, $3)`,
				userId, networkName, AuthTypeSeedphrase,
			))
			server.RaisePgResult(tx.Exec(ctx,
				`INSERT INTO network (network_id, network_name, admin_user_id)
				 VALUES ($1, $2, $3)`,
				networkId, networkName, userId,
			))
		})

		// Add seedphrase auth
		_, err := GenerateSeedphrase(ctx, userId)
		assert.Equal(t, err, nil)

		// Can't remove last auth method
		err = RemoveAuth(ctx, userId, "seedphrase")
		assert.NotEqual(t, err, nil)

		// Add a second auth method (email)
		email := "test@removeauth.com"
		passwordSalt := createPasswordSalt()
		passwordHash := computePasswordHashV1([]byte("password"), passwordSalt)
		addUserAuth(&AddUserAuthArgs{
			UserId:       userId,
			UserAuth:     &email,
			PasswordHash: passwordHash,
			PasswordSalt: passwordSalt,
			Verified:     true,
		}, ctx)

		// Now can remove seedphrase
		err = RemoveAuth(ctx, userId, "seedphrase")
		assert.Equal(t, err, nil)

		// Can't remove last auth method (only email left)
		err = RemoveAuth(ctx, userId, "email")
		assert.NotEqual(t, err, nil)
	})
}
