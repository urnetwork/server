package model

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/gagliardetto/solana-go"
	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

func TestGetUserAuth(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		networkName := "test"

		testingUserAuth := Testing_CreateNetwork(ctx, networkId, networkName, userId)

		userAuth, err := GetUserAuth(ctx, networkId)
		assert.Equal(t, err, nil)
		assert.Equal(t, userAuth, testingUserAuth)
	})
}

func TestResetPassword(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		networkName := "test"
		guestMode := false
		isPro := false

		testingUserAuth := Testing_CreateNetwork(ctx, networkId, networkName, userId)

		byJwt := jwt.NewByJwt(
			networkId,
			userId,
			networkName,
			guestMode,
			isPro,
		)
		clientSession := session.Testing_CreateClientSession(ctx, byJwt)

		// add a phone user auth to the network user
		phone := "16097370000"
		password := "password"
		passwordSalt := createPasswordSalt()
		passwordHash := computePasswordHashV1([]byte(password), passwordSalt)

		addUserAuth(
			&AddUserAuthArgs{
				UserId:       userId,
				UserAuth:     &phone,
				PasswordHash: passwordHash,
				PasswordSalt: passwordSalt,
				Verified:     true,
			},
			ctx,
		)

		networkUser := GetNetworkUser(ctx, userId)
		assert.NotEqual(t, networkUser, nil)
		assert.Equal(t, len(networkUser.UserAuths), 2)

		passwordResetCreateCodeResult, err := AuthPasswordResetCreateCode(
			AuthPasswordResetCreateCodeArgs{
				UserAuth: testingUserAuth,
			},
			clientSession,
		)
		assert.Equal(t, err, nil)

		newPassword := "testagain"

		result, err := AuthPasswordSet(
			AuthPasswordSetArgs{
				ResetCode: *passwordResetCreateCodeResult.ResetCode,
				Password:  newPassword,
			},
			clientSession,
		)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, result.NetworkId, nil)

		userAuths, err := getUserAuths(userId, ctx)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(userAuths), 2)

		for _, userAuth := range userAuths {
			loginPasswordHash := computePasswordHashV1([]byte(newPassword), userAuth.PasswordSalt)
			assert.Equal(t, bytes.Equal(userAuth.PasswordHash, loginPasswordHash), true)
		}

	})
}

func TestAuthCode(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		networkName := "test"
		guestMode := false
		isPro := false

		Testing_CreateNetwork(ctx, networkId, networkName, userId)

		byJwt := jwt.NewByJwt(
			networkId,
			userId,
			networkName,
			guestMode,
			isPro,
		)
		clientSession := session.Testing_CreateClientSession(ctx, byJwt)

		authCodeCreate := &AuthCodeCreateArgs{}

		authCodeCreateResult, err := AuthCodeCreate(authCodeCreate, clientSession)
		assert.Equal(t, err, nil)

		assert.NotEqual(t, authCodeCreateResult.AuthCode, "")

		// now try to redeem the code

		authCodeLogin := &AuthCodeLoginArgs{
			AuthCode: authCodeCreateResult.AuthCode,
		}

		authCodeLoginResult, err := AuthCodeLogin(authCodeLogin, clientSession)
		assert.Equal(t, err, nil)

		assert.NotEqual(t, authCodeLoginResult.ByJwt, "")

		// the second redeem should fail

		authCodeLogin2 := &AuthCodeLoginArgs{
			AuthCode: authCodeCreateResult.AuthCode,
		}

		authCodeLoginResult2, err := AuthCodeLogin(authCodeLogin2, clientSession)
		assert.Equal(t, err, nil)

		assert.Equal(t, authCodeLoginResult2.ByJwt, "")
		assert.NotEqual(t, authCodeLoginResult2.Error, nil)

		RemoveExpiredAuthCodes(ctx, server.NowUtc())
	})
}

func TestAuthCodeIdentity(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		networkName := "test"
		guestMode := false
		isPro := false

		Testing_CreateNetwork(ctx, networkId, networkName, userId)

		byJwt := jwt.NewByJwt(
			networkId,
			userId,
			networkName,
			guestMode,
			isPro,
		)
		clientSession := session.Testing_CreateClientSession(ctx, byJwt)

		// an auth code with roles and a principal
		authCodeCreate := &AuthCodeCreateArgs{
			Roles:     []string{"role2", "role1"},
			Principal: "svc-a",
		}

		authCodeCreateResult, err := AuthCodeCreate(authCodeCreate, clientSession)
		assert.Equal(t, err, nil)
		assert.Equal(t, authCodeCreateResult.Error, nil)
		assert.NotEqual(t, authCodeCreateResult.AuthCode, "")

		// the login mints the roles and principal into the jwt
		authCodeLogin := &AuthCodeLoginArgs{
			AuthCode: authCodeCreateResult.AuthCode,
		}

		authCodeLoginResult, err := AuthCodeLogin(authCodeLogin, clientSession)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, authCodeLoginResult.ByJwt, "")

		loginByJwt, err := jwt.ParseByJwt(ctx, authCodeLoginResult.ByJwt)
		assert.Equal(t, err, nil)
		assert.Equal(t, loginByJwt.Roles, []string{"role1", "role2"})
		assert.Equal(t, loginByJwt.Principal, "svc-a")

		// a client created by the service session inherits the identity
		// into the client jwt
		serviceSession := session.Testing_CreateClientSession(ctx, loginByJwt)
		authClientResult, err := AuthNetworkClient(
			&AuthNetworkClientArgs{
				Description: "service device",
			},
			serviceSession,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, authClientResult.Error, nil)

		clientByJwt, err := jwt.ParseByJwt(ctx, *authClientResult.ByClientJwt)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, clientByJwt.ClientId, nil)
		assert.Equal(t, clientByJwt.Roles, []string{"role1", "role2"})
		assert.Equal(t, clientByJwt.Principal, "svc-a")

		// LoadByJwtFromClientId rebuilds the identity from the db
		loadedByJwt, err := jwt.LoadByJwtFromClientId(ctx, *clientByJwt.ClientId)
		assert.Equal(t, err, nil)
		assert.Equal(t, loadedByJwt.Roles, []string{"role1", "role2"})
		assert.Equal(t, loadedByJwt.Principal, "svc-a")

		// a guest session cannot create an auth code with roles or principal
		guestSession := session.Testing_CreateClientSession(ctx, jwt.NewByJwt(
			networkId,
			userId,
			networkName,
			true,
			isPro,
		))
		authCodeCreateResult, err = AuthCodeCreate(
			&AuthCodeCreateArgs{
				Principal: "svc-b",
			},
			guestSession,
		)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, authCodeCreateResult.Error, nil)
	})
}

func TestVerifySolanaSignature(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		pk := "6UJtwDRMv2CCfVCKm6hgMDAGrFzv7z8WKEHut2u8dV8s"
		signature := "KEpagxVwv1FmPt3KIMdVZz4YsDxgD7J23+f6aafejwdnBy3WJgkE4qteYMwucNoH+9RaPU70YV2Bf+xI+Nd7Cw=="
		message := "Welcome to URnetwork"

		isValid, err := VerifySolanaSignature(pk, message, signature)
		assert.Equal(t, err, nil)
		assert.Equal(t, isValid, true)

		// now test with an invalid signature
		invalidSignature := "KEpagxVwv1FmPt3KIMdVZz4YsDxgD7J23+f6aafejwdnBy3WJgkE4qteYMwucNoH+9RaPU70YV2Bf+xI+Nd7Cw"

		isValid, err = VerifySolanaSignature(pk, message, invalidSignature)
		assert.NotEqual(t, err, nil)
		assert.Equal(t, isValid, false)

	})
}

func TestVerifyEthereumSignature(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		// Create a test wallet
		privateKey, err := crypto.GenerateKey()
		if err != nil {
			log.Fatal(err)
		}

		address := crypto.PubkeyToAddress(privateKey.PublicKey)

		messageStr := "Hello signature test"
		message := []byte(messageStr)

		// EIP-191 compliant message hash
		hash := accounts.TextHash(message)

		signature, err := crypto.Sign(hash, privateKey)
		if err != nil {
			log.Fatal(err)
		}
		sigHex := hex.EncodeToString(signature)

		isValid, err := VerifyEthereumSignature(address.String(), messageStr, sigHex)
		assert.Equal(t, err, nil)
		assert.Equal(t, isValid, true)

		// Test with an invalid signature (modified signature)
		invalidSigBytes, _ := hex.DecodeString(sigHex)
		invalidSigBytes[10] ^= 0xFF // Flip some bits in the R or S part
		invalidSigHex := hex.EncodeToString(invalidSigBytes)

		isValid, err = VerifyEthereumSignature(address.String(), messageStr, invalidSigHex)
		assert.Equal(t, isValid, false)

		// Malformed signature (wrong length)
		malformedSig := "wrongsig"
		isValid, err = VerifyEthereumSignature(address.String(), messageStr, malformedSig)
		assert.NotEqual(t, err, nil) // Error expected
		assert.Equal(t, isValid, false)
	})
}

func TestUserAuthLogin(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		networkName := "test"

		Testing_CreateNetwork(ctx, networkId, networkName, userId)

		userAuth := fmt.Sprintf("%s@bringyour.com", networkId)

		result, err := loginUserAuth(&userAuth, ctx)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, result, nil)
		assert.Equal(t, result.UserAuth, userAuth)
		assert.NotEqual(t, result.AuthAllowed, nil)

		for _, authAllowed := range *result.AuthAllowed {
			assert.NotEqual(t, authAllowed, "")
			glog.Infof("Auth allowed: %s", authAllowed)
		}

		assert.Equal(t, len(*result.AuthAllowed), 2)
		authAllowed := (*result.AuthAllowed)[0]
		assert.Equal(t, UserAuthType(authAllowed), UserAuthTypeEmail)
		authAllowed = (*result.AuthAllowed)[1]
		assert.Equal(t, authAllowed, "password")

		networkUser := GetNetworkUser(ctx, userId)
		assert.NotEqual(t, networkUser, nil)
		assert.Equal(t, len(networkUser.UserAuths), 1)
		assert.Equal(t, len(networkUser.SsoAuths), 0)

		/**
		 * Login with SSO with same userAuth should work
		 */
		parsedAuthJwt := AuthJwt{
			AuthType: SsoAuthTypeGoogle,
			UserAuth: userAuth,
			UserName: "",
		}
		useAuthAttemptId := server.NewId()

		result, err = handleLoginParsedAuthJwt(
			&HandleLoginParsedAuthJwtArgs{
				AuthJwt: parsedAuthJwt,
				// AuthJwtType:       SsoAuthTypeGoogle,
				AuthJwtStr:        "",
				UserAuthAttemptId: useAuthAttemptId,
			},
			ctx,
		)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, result, nil)
		assert.NotEqual(t, result.Network.ByJwt, nil)

		// the login should have created a SSO auth
		networkUser = GetNetworkUser(ctx, userId)
		assert.NotEqual(t, networkUser, nil)
		assert.Equal(t, len(networkUser.UserAuths), 1)
		assert.Equal(t, len(networkUser.SsoAuths), 1)

	})
}

func TestLoginWithWallet(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		networkName := "test"

		pk := "6UJtwDRMv2CCfVCKm6hgMDAGrFzv7z8WKEHut2u8dV8s"
		signature := "KEpagxVwv1FmPt3KIMdVZz4YsDxgD7J23+f6aafejwdnBy3WJgkE4qteYMwucNoH+9RaPU70YV2Bf+xI+Nd7Cw=="
		message := "Welcome to URnetwork"

		Testing_CreateNetworkByWallet(
			ctx,
			networkId,
			networkName,
			userId,
			pk,
			signature,
			message,
		)

		result, err := handleLoginWallet(&WalletAuthArgs{
			PublicKey:  pk,
			Signature:  signature,
			Message:    message,
			Blockchain: AuthTypeSolana,
		}, ctx)

		assert.Equal(t, err, nil)
		assert.NotEqual(t, result, nil)
		assert.NotEqual(t, result.Network.ByJwt, nil)

	})
}

// TestWalletLoginNonceSingleUse verifies the wallet-login replay fix: a server-issued
// nonce embedded in the signed message is single-use, so replaying a captured
// (message, signature, nonce) triple is rejected the second time.
func TestWalletLoginNonceSingleUse(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.Testing_CreateClientSession(ctx, nil)

		wallet := solana.NewWallet()

		nonceResult, err := AuthWalletNonceCreate(clientSession)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, nonceResult.Nonce, "")

		// the client signs a message that embeds the server-issued nonce
		message := "Welcome to URnetwork " + nonceResult.Nonce
		sig, err := wallet.PrivateKey.Sign([]byte(message))
		assert.Equal(t, err, nil)

		walletAuth := &WalletAuthArgs{
			PublicKey:  wallet.PublicKey().String(),
			Signature:  base64.StdEncoding.EncodeToString(sig[:]),
			Message:    message,
			Blockchain: AuthTypeSolana,
			Nonce:      nonceResult.Nonce,
		}

		// first use: valid signature + valid nonce -> accepted (nonce consumed)
		_, err = handleLoginWallet(walletAuth, ctx)
		assert.Equal(t, err, nil)

		// replay the identical triple: the nonce is already consumed -> rejected
		_, err = handleLoginWallet(walletAuth, ctx)
		assert.NotEqual(t, err, nil)
	})
}

// TestWalletLoginNonceMustBindMessage verifies that a supplied nonce must actually be
// embedded in the signed message, so an attacker cannot pair a fresh nonce with an old
// signature over a message that never committed to it.
func TestWalletLoginNonceMustBindMessage(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.Testing_CreateClientSession(ctx, nil)

		wallet := solana.NewWallet()

		nonceResult, err := AuthWalletNonceCreate(clientSession)
		assert.Equal(t, err, nil)

		// the signed message does NOT contain the nonce
		message := "Welcome to URnetwork"
		sig, err := wallet.PrivateKey.Sign([]byte(message))
		assert.Equal(t, err, nil)

		_, err = handleLoginWallet(&WalletAuthArgs{
			PublicKey:  wallet.PublicKey().String(),
			Signature:  base64.StdEncoding.EncodeToString(sig[:]),
			Message:    message,
			Blockchain: AuthTypeSolana,
			Nonce:      nonceResult.Nonce,
		}, ctx)
		assert.NotEqual(t, err, nil)
	})
}

// test social logins
func TestSocialLogin(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		useAuthAttemptId := server.NewId()

		email := "hello@bringyour.com"

		parsedAuthJwt := AuthJwt{
			AuthType: SsoAuthTypeGoogle,
			UserAuth: email,
			UserName: "",
		}

		Testing_CreateNetworkSso(
			networkId,
			userId,
			parsedAuthJwt,
			ctx,
		)

		networkUser := GetNetworkUser(ctx, userId)
		assert.NotEqual(t, networkUser, nil)
		assert.Equal(t, len(networkUser.UserAuths), 0)
		assert.Equal(t, len(networkUser.SsoAuths), 1)

		// login
		result, err := handleLoginParsedAuthJwt(
			&HandleLoginParsedAuthJwtArgs{
				AuthJwt:           parsedAuthJwt,
				AuthJwtStr:        "",
				UserAuthAttemptId: useAuthAttemptId,
			},
			ctx,
		)

		assert.Equal(t, err, nil)
		assert.Equal(t, result.Error, nil)
		assert.NotEqual(t, result.Network.ByJwt, nil)

		networkUser = GetNetworkUser(ctx, userId)
		assert.NotEqual(t, networkUser, nil)
		assert.Equal(t, len(networkUser.UserAuths), 0)
		assert.Equal(t, len(networkUser.SsoAuths), 1)

		// logging in with an Apple SSO auth should work too
		parsedAuthJwt = AuthJwt{
			AuthType: SsoAuthTypeApple,
			UserAuth: email,
			UserName: "",
		}
		result, err = handleLoginParsedAuthJwt(
			&HandleLoginParsedAuthJwtArgs{
				AuthJwt:           parsedAuthJwt,
				AuthJwtStr:        "",
				UserAuthAttemptId: useAuthAttemptId,
			},
			ctx,
		)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, result, nil)

		/**
		 * Should now have 2 SSO auths
		 */
		networkUser = GetNetworkUser(ctx, userId)
		assert.NotEqual(t, networkUser, nil)
		assert.Equal(t, len(networkUser.UserAuths), 0)
		assert.Equal(t, len(networkUser.SsoAuths), 2)

	})
}

func TestAddingSsoToDifferentNetworksShouldFail(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		walletNetworkId := server.NewId()
		walletNetworkUserId := server.NewId()

		Testing_CreateNetwork(ctx, networkId, "network_a", userId)

		email := fmt.Sprintf("%s@bringyour.com", networkId)

		pk := "6UJtwDRMv2CCfVCKm6hgMDAGrFzv7z8WKEHut2u8dV8s"
		signature := "KEpagxVwv1FmPt3KIMdVZz4YsDxgD7J23+f6aafejwdnBy3WJgkE4qteYMwucNoH+9RaPU70YV2Bf+xI+Nd7Cw=="
		message := "Welcome to URnetwork"

		Testing_CreateNetworkByWallet(ctx, walletNetworkId, "wallet_network", walletNetworkUserId, pk, signature, message)

		/**
		 * adding SSO to wallet_network with email associated with network_a should fail
		 */
		parsedAuthJwt := AuthJwt{
			AuthType: SsoAuthTypeApple,
			UserAuth: email,
			UserName: "",
		}

		err := addSsoAuth(&AddSsoAuthArgs{
			ParsedAuthJwt: parsedAuthJwt,
			AuthJwt:       "",
			AuthJwtType:   SsoAuthTypeGoogle,
			UserId:        walletNetworkUserId,
		}, ctx)
		assert.NotEqual(t, err, nil)

		networkUser := GetNetworkUser(ctx, userId)
		assert.NotEqual(t, networkUser, nil)
		assert.Equal(t, len(networkUser.UserAuths), 1)
		assert.Equal(t, len(networkUser.SsoAuths), 0)
		assert.Equal(t, len(networkUser.WalletAuths), 0)

		walletNetworkUser := GetNetworkUser(ctx, walletNetworkUserId)
		assert.NotEqual(t, walletNetworkUser, nil)
		assert.Equal(t, len(walletNetworkUser.UserAuths), 0)
		assert.Equal(t, len(walletNetworkUser.SsoAuths), 0)
		assert.Equal(t, len(walletNetworkUser.WalletAuths), 1)

		/**
		 * add a SSO to the email network should work
		 */
		err = addSsoAuth(&AddSsoAuthArgs{
			ParsedAuthJwt: parsedAuthJwt,
			AuthJwt:       "",
			AuthJwtType:   SsoAuthTypeGoogle,
			UserId:        userId,
		}, ctx)
		assert.Equal(t, err, nil)

		networkUser = GetNetworkUser(ctx, userId)
		assert.NotEqual(t, networkUser, nil)
		assert.Equal(t, len(networkUser.UserAuths), 1)
		assert.Equal(t, len(networkUser.SsoAuths), 1)
		assert.Equal(t, len(networkUser.WalletAuths), 0)

	})
}

func TestAddingSameSsoToNetworkShouldFail(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()

		Testing_CreateNetwork(ctx, networkId, "network_a", userId)

		email := fmt.Sprintf("%s@bringyour.com", networkId)

		parsedAuthJwt := AuthJwt{
			AuthType: SsoAuthTypeApple,
			UserAuth: email,
			UserName: "",
		}

		addSsoAuthArgs := &AddSsoAuthArgs{
			ParsedAuthJwt: parsedAuthJwt,
			AuthJwt:       "",
			AuthJwtType:   SsoAuthTypeGoogle,
			UserId:        userId,
		}

		err := addSsoAuth(addSsoAuthArgs, ctx)
		assert.Equal(t, err, nil)

		networkUser := GetNetworkUser(ctx, userId)
		assert.NotEqual(t, networkUser, nil)
		assert.Equal(t, len(networkUser.UserAuths), 1)
		assert.Equal(t, len(networkUser.SsoAuths), 1)
		assert.Equal(t, len(networkUser.WalletAuths), 0)

		/**
		 * Trying to add the same SSO auth again should fail
		 */
		err = addSsoAuth(addSsoAuthArgs, ctx)
		assert.NotEqual(t, err, nil)

		networkUser = GetNetworkUser(ctx, userId)
		assert.Equal(t, len(networkUser.SsoAuths), 1)

	})
}

func TestAddingSameUserAuthToNetworkShouldFail(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()

		Testing_CreateNetwork(ctx, networkId, "network_a", userId)

		email := fmt.Sprintf("%s@bringyour.com", networkId)
		password := "password123"
		passwordSalt := createPasswordSalt()
		passwordHash := computePasswordHashV1([]byte(password), passwordSalt)

		networkUser := GetNetworkUser(ctx, userId)
		assert.NotEqual(t, networkUser, nil)
		assert.Equal(t, len(networkUser.UserAuths), 1)

		/**
		 * Trying to add the same user auth again should fail
		 */
		args := &AddUserAuthArgs{
			UserId:       userId,
			UserAuth:     &email,
			PasswordHash: passwordHash,
			PasswordSalt: passwordSalt,
			Verified:     true,
		}

		err := addUserAuth(args, ctx)
		assert.NotEqual(t, err, nil)

		networkUser = GetNetworkUser(ctx, userId)
		assert.NotEqual(t, networkUser, nil)
		assert.Equal(t, len(networkUser.UserAuths), 1)

		/**
		 * But adding a phone user auth should work
		 */
		phoneNumber := "16097370000"
		args = &AddUserAuthArgs{
			UserId:       userId,
			UserAuth:     &phoneNumber,
			PasswordHash: passwordHash,
			PasswordSalt: passwordSalt,
			Verified:     true,
		}

		err = addUserAuth(args, ctx)
		assert.Equal(t, err, nil)

		networkUser = GetNetworkUser(ctx, userId)
		assert.NotEqual(t, networkUser, nil)
		assert.Equal(t, len(networkUser.UserAuths), 2)

		/**
		 * Adding an existing user auth to a different network should fail
		 */
		userId2 := server.NewId()
		args = &AddUserAuthArgs{
			UserId:       userId2,
			UserAuth:     &phoneNumber,
			PasswordHash: passwordHash,
			PasswordSalt: passwordSalt,
			Verified:     true,
		}

		err = addUserAuth(args, ctx)
		assert.NotEqual(t, err, nil)

	})
}

// FIXME test concurrent redeem
// FIXME test expire all auth

// Creating a new verify code invalidates the user's previous codes (only the
// latest code verifies), and the invalidation update touches only live
// (used = false) rows. Expired rows are reaped by RemoveExpiredVerifyCodes.
func TestAuthVerifyCodeInvalidation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		userAuth := Testing_CreateNetwork(ctx, networkId, "test", userId)

		byJwt := jwt.NewByJwt(networkId, userId, "test", false, false)
		clientSession := session.Testing_CreateClientSession(ctx, byJwt)

		createCode := func() string {
			result, err := AuthVerifyCreateCode(
				AuthVerifyCreateCodeArgs{
					UserAuth: userAuth,
				},
				clientSession,
			)
			assert.Equal(t, err, nil)
			assert.Equal(t, result.Error, nil)
			assert.NotEqual(t, result.VerifyCode, nil)
			return *result.VerifyCode
		}

		// stay within AttemptFailedCountThreshold: each create/verify consumes
		// auth attempt budget
		code1 := createCode()
		code2 := createCode()
		assert.NotEqual(t, code1, code2)

		// only the latest code is live
		countUnused := func() int {
			c := 0
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(
					ctx,
					`
					SELECT COUNT(*) FROM user_auth_verify
					WHERE user_id = $1 AND used = false
					`,
					userId,
				)
				server.WithPgResult(result, err, func() {
					if result.Next() {
						server.Raise(result.Scan(&c))
					}
				})
			})
			return c
		}
		assert.Equal(t, countUnused(), 1)

		// an invalidated code does not verify
		verifyResult, err := AuthVerify(
			AuthVerifyArgs{
				UserAuth:   userAuth,
				VerifyCode: code1,
			},
			clientSession,
		)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, verifyResult.Error, nil)

		// the latest code verifies
		verifyResult, err = AuthVerify(
			AuthVerifyArgs{
				UserAuth:   userAuth,
				VerifyCode: code2,
			},
			clientSession,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, verifyResult.Error, nil)
		assert.Equal(t, countUnused(), 0)

		// age out the rows and reap them
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				UPDATE user_auth_verify
				SET verify_time = now() - INTERVAL '30 days'
				WHERE user_id = $1
				`,
				userId,
			))
		})
		RemoveExpiredVerifyCodes(ctx, server.NowUtc().Add(-24*time.Hour))

		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`SELECT COUNT(*) FROM user_auth_verify WHERE user_id = $1`,
				userId,
			)
			server.WithPgResult(result, err, func() {
				assert.Equal(t, result.Next(), true)
				var c int
				server.Raise(result.Scan(&c))
				assert.Equal(t, c, 0)
			})
		})
	})
}
