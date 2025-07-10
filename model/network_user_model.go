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
	UserAuth     string       `json:"user_auth,omitempty"`
	AuthType     UserAuthType `json:"auth_type"`
	PasswordHash []byte       `json:"-"`
	PasswordSalt []byte       `json:"-"`
}

type NetworkUserSsoAuth struct {
	UserId   *server.Id  `json:"user_id,omitempty"`
	AuthType SsoAuthType `json:"auth_type"`
	AuthJwt  string      `json:"auth_jwt"`
	UserAuth *string     `json:"user_auth,omitempty"`
}

type NetworkUserWalletAuth struct {
	UserId        *server.Id `json:"user_id,omitempty"`
	WalletAddress *string    `json:"wallet_address,omitempty"`
	Blockchain    string     `json:"blockchain"`
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
			session.Ctx,
		)

		return &AddAuthMethodResult{}, nil
	} else if authArgs.AuthJwt != nil && authArgs.AuthJwtType != nil {
		// user is adding a social login auth method

		addSsoAuth(
			&AddSsoAuthArgs{
				AuthJwt:     *authArgs.AuthJwt,
				AuthJwtType: SsoAuthType(*authArgs.AuthJwtType),
				UserId:      session.ByJwt.UserId,
			},
			session.Ctx,
		)

		return &AddAuthMethodResult{}, nil
	} else if authArgs.WalletAuth != nil {
		// user is adding a wallet auth method
		addWalletAuth(
			&AddWalletAuthArgs{
				WalletAuth: authArgs.WalletAuth,
				UserId:     session.ByJwt.UserId,
			},
			session.Ctx,
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
	Verified     bool
}

func addUserAuth(
	args *AddUserAuthArgs,
	ctx context.Context,
) (returnErr *error) {

	userAuth, userAuthType := NormalUserAuthV1(args.UserAuth)

	// TODO - if they have authed through SSO, mark them as verified

	server.Tx(ctx, func(tx server.PgTx) {

		/**
		 * Check if the userauth already exists
		 */
		result, queryErr := tx.Query(
			ctx,
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
			ctx,
			`
				INSERT INTO network_user_auth_password
				(user_id, user_auth, auth_type, password_salt, password_hash, verified)
				VALUES ($1, $2, $3, $4, $5, $6)
			`,
			args.UserId,
			userAuth,
			userAuthType,
			args.PasswordSalt,
			args.PasswordHash,
			args.Verified,
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
				auth_type,
				password_hash,
				password_salt
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
					&userAuth.PasswordHash,
					&userAuth.PasswordSalt,
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

type AddSsoAuthArgs struct {
	AuthJwt     string      `json:"auth_jwt"`
	AuthJwtType SsoAuthType `json:"auth_jwt_type"`
	UserId      server.Id   `json:"user_id"`
}

func addSsoAuth(
	args *AddSsoAuthArgs,
	ctx context.Context,
) error {

	parsedAuthJwt, err := ParseAuthJwt(args.AuthJwt, AuthType(args.AuthJwtType))

	if err != nil {
		return fmt.Errorf("error parsing auth jwt: %s", err.Error())
	}

	if parsedAuthJwt == nil {
		return errors.New("parsed auth jwt is nil")
	}

	normalJwtUserAuth, _ := NormalUserAuth(parsedAuthJwt.UserAuth)

	server.Tx(ctx, func(tx server.PgTx) {
		_, err = tx.Exec(
			ctx,
			`
			INSERT INTO network_user_auth_sso
			(user_id, auth_type, user_auth, auth_jwt)
			VALUES ($1, $2, $3, $4)
		`,
			args.UserId,
			parsedAuthJwt.AuthType,
			normalJwtUserAuth,
			args.AuthJwt,
		)
		server.Raise(err)
	})

	return nil

}

/**
 * Get all SSO auths for a user by user ID
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
 * Get SSO auths by user auth
 */
func getSsoAuthsByUserAuth(
	ctx context.Context,
	userAuth string,
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
				WHERE user_auth = $1
			`,
			userAuth,
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
type AddWalletAuthArgs struct {
	UserId     server.Id       `json:"user_id"`
	WalletAuth *WalletAuthArgs `json:"wallet_auth"`
}

func addWalletAuth(
	addWalletAuth *AddWalletAuthArgs,
	ctx context.Context,
) error {

	walletAuth := addWalletAuth.WalletAuth

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

	server.Tx(ctx, func(tx server.PgTx) {

		_, dbErr := tx.Exec(
			ctx,
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
			addWalletAuth.UserId,
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

func getWalletAuthsByAddress(
	ctx context.Context,
	walletAddress string,
) ([]NetworkUserWalletAuth, error) {

	var walletAuths []NetworkUserWalletAuth

	server.Tx(ctx, func(tx server.PgTx) {

		result, err := tx.Query(
			ctx,
			`
				SELECT
					user_id,
					wallet_address,
					blockchain
				FROM network_user_auth_wallet
				WHERE wallet_address = $1
			`,
			walletAddress,
		)
		if err != nil {
			server.Raise(err)
		}

		server.WithPgResult(result, err, func() {
			for result.Next() {
				walletAuth := NetworkUserWalletAuth{}
				server.Raise(result.Scan(
					&walletAuth.UserId,
					&walletAuth.WalletAddress,
					&walletAuth.Blockchain,
				))
				walletAuths = append(walletAuths, walletAuth)
			}
		})

	})

	return walletAuths, nil
}
