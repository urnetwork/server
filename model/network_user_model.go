package model

import (
	"context"
	"errors"
	"fmt"

	"github.com/golang/glog"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

type NetworkUser struct {
	UserId        server.Id               `json:"user_id"`
	UserAuth      *string                 `json:"user_auth,omitempty"`
	Verified      bool                    `json:"verified"`
	AuthType      string                  `json:"auth_type"`
	NetworkName   string                  `json:"network_name"`
	WalletAddress *string                 `json:"wallet_address,omitempty"`
	UserAuths     []NetworkUserUserAuth   `json:"user_auths,omitempty"`
	SsoAuths      []NetworkUserSsoAuth    `json:"sso_auths,omitempty"`
	WalletAuths   []NetworkUserWalletAuth `json:"wallet_auths,omitempty"`
}

type NetworkUserUserAuth struct {
	UserAuth string       `json:"user_auth,omitempty"`
	AuthType UserAuthType `json:"auth_type"`
}

type NetworkUserSsoAuth struct {
	AuthType UserAuthType `json:"auth_type"`
	AuthJwt  string       `json:"auth_jwt"`
	UserAuth *string      `json:"user_auth,omitempty"`
}

type NetworkUserWalletAuth struct {
	WalletAddress *string `json:"wallet_address,omitempty"`
	Blockchain    string  `json:"blockchain"`
}

func GetNetworkUser(
	ctx context.Context,
	userId server.Id,
) *NetworkUser {

	var networkUser *NetworkUser

	server.Tx(ctx, func(tx server.PgTx) {

		result, err := tx.Query(
			ctx,
			`
			SELECT
				network_user.user_id,
				network_user.auth_type,
				network_user.user_auth,
				network_user.verified,
				network_user.wallet_address,
				network.network_name
			FROM network_user
			LEFT JOIN network ON
				network.admin_user_id = network_user.user_id
			WHERE network_user.user_id = $1
		`,
			userId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {

				networkUser = &NetworkUser{}

				server.Raise(result.Scan(
					&networkUser.UserId,
					&networkUser.AuthType,
					&networkUser.UserAuth,
					&networkUser.Verified,
					&networkUser.WalletAddress,
					&networkUser.NetworkName,
				))
			}
		})

		if networkUser == nil {
			glog.Infof("No network user found for user ID: %s", userId)
			// No user found with this ID
			return
		}

		/**
		 * Get SSO auths for the user
		 */
		ssoAuths, err := getSsoAuths(ctx, userId)
		server.Raise(err)
		networkUser.SsoAuths = ssoAuths

		/**
		 * Get email/phone + password auths for the user
		 */
		userAuths, err := getUserAuths(userId, ctx)
		server.Raise(err)
		networkUser.UserAuths = userAuths

		/**
		 * Get wallet auths for the user
		 */
		walletAuths, err := getWalletAuths(ctx, userId)
		server.Raise(err)
		networkUser.WalletAuths = walletAuths

	})

	return networkUser
}

/**
 * Add an authentication method to a network user
 * Allows user to add an email, phone, password, sso, or wallet authentication method
 */
type AddAuthMethod struct {
	UserAuth    *string         `json:"user_auth,omitempty"`
	AuthJwt     *string         `json:"auth_jwt,omitempty"`
	AuthJwtType *string         `json:"auth_jwt_type,omitempty"`
	Password    *string         `json:"password,omitempty"`
	WalletAuth  *WalletAuthArgs `json:"wallet_auth,omitempty"`
}

type AddAuthMethodResult struct {
	Error *AddAuthMethodError `json:"error,omitempty"`
}

type AddAuthMethodError struct {
	Message string `json:"message"`
}

func AddAuth(
	authArgs AddAuthMethod,
	session *session.ClientSession,
) (*AddAuthMethodResult, error) {

	if authArgs.UserAuth != nil && authArgs.Password != nil {
		/**
		 * user is adding an email/phone + password auth method
		 */

		// todo - check if userAuth is email or phone
		//
		if !passwordValid(*authArgs.Password) {
			return &AddAuthMethodResult{
				Error: &AddAuthMethodError{
					Message: fmt.Sprintf("Password must have at least %d characters", MinPasswordLength),
				},
			}, nil
		}

		passwordSalt := createPasswordSalt()
		passwordHash := computePasswordHashV1([]byte(*authArgs.Password), passwordSalt)

		addUserAuth(
			&AddUserAuthArgs{
				UserId:       session.ByJwt.UserId,
				UserAuth:     authArgs.UserAuth,
				PasswordHash: passwordHash,
				PasswordSalt: passwordSalt,
			},
			session,
		)

		return &AddAuthMethodResult{}, nil
	} else if authArgs.AuthJwt != nil && authArgs.AuthJwtType != nil {
		// user is adding a social login auth method
		addSsoAuth(
			*authArgs.AuthJwt,
			*authArgs.AuthJwtType,
			session,
		)
		return &AddAuthMethodResult{}, nil
	} else if authArgs.WalletAuth != nil {
		// user is adding a wallet auth method
		addWalletAuth(
			*authArgs.WalletAuth,
			session,
		)
		return &AddAuthMethodResult{}, nil
	}

	return nil, nil
}

type AddUserAuthArgs struct {
	UserId   server.Id
	UserAuth *string
	// password string,
	PasswordHash []byte
	PasswordSalt []byte
}

func addUserAuth(
	args *AddUserAuthArgs,
	session *session.ClientSession,
) (returnErr *error) {

	userAuth, userAuthType := NormalUserAuthV1(args.UserAuth)

	// TODO - if they have authed through SSO, mark them as verified

	server.Tx(session.Ctx, func(tx server.PgTx) {

		/**
		 * Check if the userauth already exists
		 */
		result, queryErr := tx.Query(
			session.Ctx,
			`
			SELECT
				auth_type
			FROM network_user_auth_password
			WHERE user_id = $1 AND auth_type = $2
		`,
			args.UserId,
			userAuthType,
		)
		if queryErr != nil {
			returnErr = &queryErr
			return
		}

		exists := false

		server.WithPgResult(result, queryErr, func() {
			if result.Next() {
				exists = true
			}
		})

		if exists {
			err := fmt.Errorf("User exists with auth type %s", userAuthType)
			returnErr = &err
			return
		}

		/**
		 * No record exists with this auth type, create a new one
		 */
		// if !passwordValid(password) {
		// 	passwordErr := errors.New("Network name must have at least 5 characters")
		// 	returnErr = &passwordErr
		// 	return
		// }

		// passwordSalt := createPasswordSalt()
		// passwordHash := computePasswordHashV1([]byte(password), passwordSalt)

		_, dbErr := tx.Exec(
			session.Ctx,
			`
				INSERT INTO network_user_auth_password
				(user_id, user_auth, auth_type, password_salt, password_hash)
				VALUES ($1, $2, $3, $4, $5)
			`,
			args.UserId,
			userAuth,
			userAuthType,
			args.PasswordSalt,
			args.PasswordHash,
		)
		server.Raise(dbErr)
	})

	return
}

func getUserAuths(
	userId server.Id,
	ctx context.Context,
) ([]NetworkUserUserAuth, error) {

	var userAuths []NetworkUserUserAuth

	server.Tx(ctx, func(tx server.PgTx) {

		result, err := tx.Query(
			ctx,
			`
			SELECT
				user_auth,
				auth_type
			FROM network_user_auth_password
			WHERE user_id = $1
		`,
			userId,
		)
		if err != nil {
			server.Raise(err)
		}

		server.WithPgResult(result, err, func() {
			for result.Next() {
				userAuth := NetworkUserUserAuth{}
				server.Raise(result.Scan(
					&userAuth.UserAuth,
					&userAuth.AuthType,
				))
				userAuths = append(userAuths, userAuth)
			}
		})

	})

	return userAuths, nil

}

/**
 * Allow different SSO auth methods
 */
func addSsoAuth(
	authJwt string,
	authJwtType string,
	session *session.ClientSession,
) error {

	parsedAuthJwt, err := ParseAuthJwt(authJwt, AuthType(authJwtType))

	if err != nil {
		return fmt.Errorf("error parsing auth jwt: %s", err.Error())
	}

	if parsedAuthJwt == nil {
		return errors.New("parsed auth jwt is nil")
	}

	normalJwtUserAuth, _ := NormalUserAuth(parsedAuthJwt.UserAuth)

	server.Tx(session.Ctx, func(tx server.PgTx) {
		_, err = tx.Exec(
			session.Ctx,
			`
			INSERT INTO network_user_auth_sso
			(user_id, auth_type, user_auth, auth_jwt)
			VALUES ($1, $2, $3, $4)
		`,
			session.ByJwt.UserId,
			parsedAuthJwt.AuthType,
			normalJwtUserAuth,
			authJwt,
		)
		server.Raise(err)
	})

	return nil

}

/**
 * Get all SSO auths for a user
 */
func getSsoAuths(
	ctx context.Context,
	userId server.Id,
) ([]NetworkUserSsoAuth, error) {

	var ssoAuths []NetworkUserSsoAuth

	server.Tx(ctx, func(tx server.PgTx) {

		result, err := tx.Query(
			ctx,
			`
			SELECT
				auth_type,
				auth_jwt,
				user_auth
			FROM network_user_auth_sso
			WHERE user_id = $1
		`,
			userId,
		)
		if err != nil {
			server.Raise(err)
		}

		server.WithPgResult(result, err, func() {
			for result.Next() {
				ssoAuth := NetworkUserSsoAuth{}
				server.Raise(result.Scan(
					&ssoAuth.AuthType,
					&ssoAuth.AuthJwt,
					&ssoAuth.UserAuth,
				))
				ssoAuths = append(ssoAuths, ssoAuth)
			}
		})

	})

	return ssoAuths, nil
}

/**
 * Currently only allowing 1 wallet auth per user
 * We can expand on this if needed
 */
func addWalletAuth(
	walletAuth WalletAuthArgs,
	session *session.ClientSession,
) error {

	isValid, err := VerifySolanaSignature(
		walletAuth.PublicKey,
		walletAuth.Message,
		walletAuth.Signature,
	)
	if err != nil {
		return err
	}
	if !isValid {
		return errors.New("invalid signature")
	}

	server.Tx(session.Ctx, func(tx server.PgTx) {

		_, dbErr := tx.Exec(
			session.Ctx,
			`
				INSERT INTO network_user_auth_wallet
				(user_id, wallet_address, blockchain)
				VALUES ($1, $2, $3)
				ON CONFLICT (user_id)
				DO UPDATE SET
					wallet_address = $2,
					blockchain = $3,
					create_time = now();
			`,
			session.ByJwt.UserId,
			walletAuth.PublicKey,
			walletAuth.Blockchain,
		)
		server.Raise(dbErr)

	})

	return nil
}

func getWalletAuths(
	ctx context.Context,
	userId server.Id,
) ([]NetworkUserWalletAuth, error) {

	var walletAuths []NetworkUserWalletAuth

	server.Tx(ctx, func(tx server.PgTx) {

		result, err := tx.Query(
			ctx,
			`
			SELECT
				wallet_address,
				blockchain
			FROM network_user_auth_wallet
			WHERE user_id = $1
		`,
			userId,
		)
		if err != nil {
			server.Raise(err)
		}

		server.WithPgResult(result, err, func() {
			for result.Next() {
				walletAuth := NetworkUserWalletAuth{}
				server.Raise(result.Scan(
					&walletAuth.WalletAddress,
					&walletAuth.Blockchain,
				))
				walletAuths = append(walletAuths, walletAuth)
			}
		})

	})

	return walletAuths, nil
}
