package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	// "github.com/golang/glog"

	goaway "github.com/TwiN/go-away"
	"github.com/golang/glog"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"

	// "github.com/urnetwork/server/ulid"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/search"
)

var networkNameSearch = search.NewSearch("network_name", search.SearchTypeFull)

const MinPasswordLength = 6

type NetworkCheckArgs struct {
	NetworkName string `json:"network_name"`
}

type NetworkCheckResult struct {
	Available bool `json:"available"`
}

type NetworkCreateError = string

const (
	AgreeToTerms NetworkCreateError = "The terms of service and privacy policy must be accepted."
)

func NetworkCheck(check *NetworkCheckArgs, session *session.ClientSession) (*NetworkCheckResult, error) {
	taken := networkNameSearch.AnyAround(session.Ctx, check.NetworkName, 1)

	result := &NetworkCheckResult{
		Available: !taken,
	}
	return result, nil
}

type NetworkCreateArgs struct {
	UserName         string          `json:"user_name"`
	UserAuth         *string         `json:"user_auth,omitempty"`
	AuthJwt          *string         `json:"auth_jwt,omitempty"`
	AuthJwtType      *string         `json:"auth_jwt_type,omitempty"`
	Password         *string         `json:"password,omitempty"`
	NetworkName      string          `json:"network_name"`
	Terms            bool            `json:"terms"`
	GuestMode        bool            `json:"guest_mode"`
	VerifyUseNumeric bool            `json:"verify_use_numeric"`
	ReferralCode     *string         `json:"referral_code,omitempty"`
	WalletAuth       *WalletAuthArgs `json:"wallet_auth,omitempty"`
}

type UpgradeGuestArgs struct {
	NetworkName string          `json:"network_name"`
	UserAuth    *string         `json:"user_auth,omitempty"`
	AuthJwt     *string         `json:"auth_jwt,omitempty"`
	AuthJwtType *string         `json:"auth_jwt_type,omitempty"`
	Password    *string         `json:"password,omitempty"`
	WalletAuth  *WalletAuthArgs `json:"wallet_auth,omitempty"`
}

type NetworkCreateResult struct {
	Network              *NetworkCreateResultNetwork      `json:"network,omitempty"`
	UserAuth             *string                          `json:"user_auth,omitempty"`
	VerificationRequired *NetworkCreateResultVerification `json:"verification_required,omitempty"`
	Error                *NetworkCreateResultError        `json:"error,omitempty"`
}

type NetworkCreateResultNetwork struct {
	ByJwt       *string   `json:"by_jwt,omitempty"`
	NetworkId   server.Id `json:"network_id,omitempty"`
	NetworkName string    `json:"network_name,omitempty"`
}

type NetworkCreateResultVerification struct {
	UserAuth string `json:"user_auth"`
}

type NetworkCreateResultError struct {
	Message string `json:"message"`
}

func NetworkCreate(
	networkCreate NetworkCreateArgs,
	session *session.ClientSession,
) (*NetworkCreateResult, error) {
	userAuth, _ := NormalUserAuthV1(networkCreate.UserAuth)

	userAuthAttemptId, allow := UserAuthAttempt(userAuth, session)
	if !allow {
		return nil, maxUserAuthAttemptsError()
	}

	if !networkCreate.Terms {
		result := &NetworkCreateResult{
			Error: &NetworkCreateResultError{
				Message: AgreeToTerms,
			},
		}
		return result, nil
	}

	// create a guest network
	if networkCreate.GuestMode {

		created := false
		var createdNetworkId server.Id
		var networkName string
		var createdUserId server.Id

		server.Tx(session.Ctx, func(tx server.PgTx) {
			var err error

			created = true
			createdUserId = server.NewId()
			createdNetworkId = server.NewId()

			// remove dashes from the network name
			networkName = fmt.Sprintf(
				"g%s",
				strings.ReplaceAll(server.NewId().String(), "-", ""),
			)

			_, err = tx.Exec(
				session.Ctx,
				`
					INSERT INTO network_user
					(user_id, user_name)
					VALUES ($1, $2)
				`,
				createdUserId,
				"guest",
			)
			server.Raise(err)

			_, err = tx.Exec(
				session.Ctx,
				`
					INSERT INTO network
					(network_id, network_name, admin_user_id)
					VALUES ($1, $2, $3)
				`,
				createdNetworkId,
				networkName,
				createdUserId,
			)
			server.Raise(err)
		})
		if created {
			auditNetworkCreate(networkCreate, createdNetworkId, session)

			// we should disable adding the guest network to name search?
			// networkNameSearch.Add(session.Ctx, networkCreate.NetworkName, createdNetworkId, 0)

			byJwt := jwt.NewByJwt(
				createdNetworkId,
				createdUserId,
				networkCreate.NetworkName,
				true,
			)
			byJwtSigned := byJwt.Sign()
			result := &NetworkCreateResult{
				Network: &NetworkCreateResultNetwork{
					ByJwt:       &byJwtSigned,
					NetworkName: networkName,
					NetworkId:   createdNetworkId,
				},
			}

			return result, nil
		} else {
			result := &NetworkCreateResult{
				Error: &NetworkCreateResultError{
					Message: "An error occurred creating a guest network",
				},
			}
			return result, nil
		}
	}

	// user is create an authenticated network

	// check if the network name is already taken
	err := checkNetworkNameAvailability(networkCreate.NetworkName, session)
	if err != nil {
		result := &NetworkCreateResult{
			Error: &NetworkCreateResultError{
				Message: err.Error(),
			},
		}
		return result, nil
	}

	containsProfanity := goaway.IsProfane(networkCreate.NetworkName)

	if networkCreate.UserAuth != nil {
		// user is creating a network via email/phone + pass
		// validate the user does not exist

		if userAuth == nil {
			result := &NetworkCreateResult{
				Error: &NetworkCreateResultError{
					Message: "Invalid email or phone number.",
				},
			}
			return result, nil
		}

		created := false
		var createdNetworkId server.Id

		server.Tx(session.Ctx, func(tx server.PgTx) {
			var result server.PgResult
			var err error

			/**
			 * make sure user auth is not already in use
			 */
			exists := userAuthExistsInTx(session.Ctx, tx, *networkCreate.UserAuth)
			if exists {
				return
			}

			var existingNetworkId *server.Id

			result, err = tx.Query(
				session.Ctx,
				`
					SELECT network_id FROM network WHERE network_name = $1
				`,
				networkCreate.NetworkName,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&existingNetworkId))
				}
			})

			if existingNetworkId != nil {
				return
			}

			created = true
			createdUserId := server.NewId()
			createdNetworkId = server.NewId()

			passwordSalt := createPasswordSalt()
			passwordHash := computePasswordHashV1([]byte(*networkCreate.Password), passwordSalt)

			// todo: remove user_auth once UIs are updated to use user_auths
			// specifically profile -> reset password uses network_user.user_auth
			_, err = tx.Exec(
				session.Ctx,
				`
					INSERT INTO network_user
					(user_id, user_name, user_auth)
					VALUES ($1, $2, $3)
				`,
				createdUserId,
				networkCreate.UserName,
				userAuth,
			)
			server.Raise(err)

			// insert into network_user_auth_password
			addUserAuth(
				&AddUserAuthArgs{
					UserId:       createdUserId,
					UserAuth:     userAuth,
					PasswordHash: passwordHash,
					PasswordSalt: passwordSalt,
				},
				session.Ctx,
			)

			_, err = tx.Exec(
				session.Ctx,
				`
					INSERT INTO network
					(network_id, network_name, admin_user_id, contains_profanity)
					VALUES ($1, $2, $3, $4)
				`,
				createdNetworkId,
				networkCreate.NetworkName,
				createdUserId,
				containsProfanity,
			)
			server.Raise(err)
		})
		if created {
			auditNetworkCreate(networkCreate, createdNetworkId, session)

			networkNameSearch.Add(session.Ctx, networkCreate.NetworkName, createdNetworkId, 0)

			result := &NetworkCreateResult{
				VerificationRequired: &NetworkCreateResultVerification{
					UserAuth: *userAuth,
				},
				Network: &NetworkCreateResultNetwork{
					NetworkName: networkCreate.NetworkName,
					NetworkId:   createdNetworkId,
				},
			}
			return result, nil
		} else {
			result := &NetworkCreateResult{
				Error: &NetworkCreateResultError{
					Message: "Account might already exist. Please start over.",
				},
			}
			return result, nil
		}
	} else if networkCreate.AuthJwt != nil && networkCreate.AuthJwtType != nil {
		// user is creating a network via social login

		authJwt, _ := ParseAuthJwt(*networkCreate.AuthJwt, AuthType(*networkCreate.AuthJwtType))
		// server.Logger().Printf("Parse JWT result: %s, %s\n", authJwt, err)
		if authJwt != nil {

			normalJwtUserAuth, _ := NormalUserAuth(authJwt.UserAuth)

			created := false
			var createdNetworkId server.Id
			var createdUserId server.Id

			server.Tx(session.Ctx, func(tx server.PgTx) {

				/**
				 * make sure user auth is not already in use
				 */
				exists := userAuthExistsInTx(session.Ctx, tx, normalJwtUserAuth)
				if exists {
					return
				}

				created = true
				createdUserId = server.NewId()
				createdNetworkId = server.NewId()

				// todo: remove user_auth once UIs are updated to use user_auths
				// specifically profile -> reset password uses network_user.user_auth
				_, err = tx.Exec(
					session.Ctx,
					`
						INSERT INTO network_user
						(user_id, user_name, user_auth)
						VALUES ($1, $2, $3)
					`,
					createdUserId,
					networkCreate.UserName,
				)
				if err != nil {
					panic(err)
				}

				// insert into network_user_auth_sso
				addSsoAuth(
					&AddSsoAuthArgs{
						UserId:        createdUserId,
						AuthJwt:       *networkCreate.AuthJwt,
						ParsedAuthJwt: *authJwt,
						AuthJwtType:   SsoAuthType(*networkCreate.AuthJwtType),
					},
					session.Ctx,
				)

				_, err = tx.Exec(
					session.Ctx,
					`
						INSERT INTO network
						(network_id, network_name, admin_user_id, contains_profanity)
						VALUES ($1, $2, $3, $4)
					`,
					createdNetworkId,
					networkCreate.NetworkName,
					createdUserId,
					containsProfanity,
				)
				if err != nil {
					panic(err)
				}
			})
			if created {
				auditNetworkCreate(networkCreate, createdNetworkId, session)

				networkNameSearch.Add(session.Ctx, networkCreate.NetworkName, createdNetworkId, 0)

				SetUserAuthAttemptSuccess(session.Ctx, userAuthAttemptId, true)

				// successful login
				byJwt := jwt.NewByJwt(
					createdNetworkId,
					createdUserId,
					networkCreate.NetworkName,
					false,
				)
				byJwtSigned := byJwt.Sign()
				result := &NetworkCreateResult{
					Network: &NetworkCreateResultNetwork{
						ByJwt:       &byJwtSigned,
						NetworkName: networkCreate.NetworkName,
						NetworkId:   createdNetworkId,
					},
					UserAuth: &authJwt.UserAuth,
				}
				return result, nil
			} else {
				result := &NetworkCreateResult{
					Error: &NetworkCreateResultError{
						Message: "Account might already exist. Please log in again.",
					},
				}
				return result, nil
			}
		}
	} else if networkCreate.WalletAuth != nil {

		/**
		 * User is authenticating with a crypto wallet
		 */

		isValid, err := VerifySolanaSignature(
			networkCreate.WalletAuth.PublicKey,
			networkCreate.WalletAuth.Message,
			networkCreate.WalletAuth.Signature,
		)

		if err != nil {
			return nil, err
		}

		if !isValid {
			return nil, errors.New("invalid signature")
		}

		created := false
		var createdNetworkId server.Id
		var createdUserId server.Id

		server.Tx(session.Ctx, func(tx server.PgTx) {

			walletAuths, err := getWalletAuthsByAddressInTx(session.Ctx, tx, networkCreate.WalletAuth.PublicKey)
			if err != nil {
				glog.Errorf("Error getting wallet auths by address: %v", err)
				return
			}

			if len(walletAuths) > 0 {
				glog.Infof("Network user already exists with this wallet address")
				return
			}

			created = true
			createdUserId = server.NewId()
			createdNetworkId = server.NewId()

			/**
			 * hard set the blockchain to solana for now
			 */
			networkCreate.WalletAuth.Blockchain = "solana"

			_, err = tx.Exec(
				session.Ctx,
				`
					INSERT INTO network_user
					(user_id, user_name)
					VALUES ($1, $2)
				`,
				createdUserId,
				networkCreate.UserName,
			)
			if err != nil {
				panic(err)
			}

			// insert into network_user_auth_wallet
			addWalletAuth(
				&AddWalletAuthArgs{
					WalletAuth: &WalletAuthArgs{
						PublicKey:  networkCreate.WalletAuth.PublicKey,
						Blockchain: networkCreate.WalletAuth.Blockchain,
						Message:    networkCreate.WalletAuth.Message,
						Signature:  networkCreate.WalletAuth.Signature,
					},
					UserId: createdUserId,
				},
				session.Ctx,
			)

			_, err = tx.Exec(
				session.Ctx,
				`
					INSERT INTO network
					(network_id, network_name, admin_user_id, contains_profanity)
					VALUES ($1, $2, $3, $4)
				`,
				createdNetworkId,
				networkCreate.NetworkName,
				createdUserId,
				containsProfanity,
			)
			if err != nil {
				panic(err)
			}
		})
		if created {

			auditNetworkCreate(networkCreate, createdNetworkId, session)

			networkNameSearch.Add(session.Ctx, networkCreate.NetworkName, createdNetworkId, 0)

			SetUserAuthAttemptSuccess(session.Ctx, userAuthAttemptId, true)

			/**
			 * Create new payout wallet
			 */

			walletId := CreateAccountWalletExternal(
				session,
				&CreateAccountWalletExternalArgs{
					NetworkId:        createdNetworkId,
					Blockchain:       "SOL",
					WalletAddress:    networkCreate.WalletAuth.PublicKey,
					DefaultTokenType: "USDC",
				},
			)

			/**
			 * Set the payout wallet for the network
			 */
			SetPayoutWallet(
				session.Ctx,
				createdNetworkId,
				*walletId,
			)

			// successful login
			byJwt := jwt.NewByJwt(
				createdNetworkId,
				createdUserId,
				networkCreate.NetworkName,
				false,
			)
			byJwtSigned := byJwt.Sign()
			result := &NetworkCreateResult{
				Network: &NetworkCreateResultNetwork{
					ByJwt:       &byJwtSigned,
					NetworkName: networkCreate.NetworkName,
					NetworkId:   createdNetworkId,
				},
				// UserAuth: &authJwt.UserAuth,
			}
			return result, nil
		} else {
			result := &NetworkCreateResult{
				Error: &NetworkCreateResultError{
					Message: "Account might already exist. Please log in again.",
				},
			}
			return result, nil
		}

	}

	return nil, errors.New("invalid login")
}

func auditNetworkCreate(
	networkCreate NetworkCreateArgs,
	networkId server.Id,
	session *session.ClientSession,
) {
	type Details struct {
		NetworkCreate NetworkCreateArgs `json:"network_create"`
		ClientAddress string            `json:"client_address"`
	}

	details := Details{
		NetworkCreate: networkCreate,
		ClientAddress: session.ClientAddress,
	}

	detailsJson, err := json.Marshal(details)
	if err != nil {
		panic(err)
	}
	detailsJsonString := string(detailsJson)

	auditNetworkEvent := NewAuditNetworkEvent(AuditEventTypeNetworkCreated)
	auditNetworkEvent.NetworkId = networkId
	auditNetworkEvent.EventDetails = &detailsJsonString
	AddAuditEvent(session.Ctx, auditNetworkEvent)
}

type NetworkUpdateArgs struct {
	NetworkName string
}

type NetworkUpdateError struct {
	Message string `json:"message"`
}

type NetworkUpdateResult struct {
	Error *NetworkUpdateError `json:"error,omitempty"`
}

func checkNetworkNameAvailability(
	networkName string,
	session *session.ClientSession,
) (err error) {

	var existingNetworkId *server.Id

	if len(networkName) < 5 {
		err = errors.New("Network name must have at least 5 characters")
		return
	}

	taken := networkNameSearch.AnyAround(session.Ctx, networkName, 1)

	if taken {
		err = errors.New("Network name not available")
		return
	}

	server.Tx(session.Ctx, func(tx server.PgTx) {

		result, queryErr := tx.Query(
			session.Ctx,
			`
				SELECT network_id FROM network WHERE network_name = $1
			`,
			networkName,
		)
		server.WithPgResult(result, queryErr, func() {
			if result.Next() {
				server.Raise(result.Scan(&existingNetworkId))
			}
		})

		if existingNetworkId != nil {

			err = errors.New("Network name not available")
			return

		}
	})

	return err
}

func NetworkUpdate(
	networkUpdate NetworkUpdateArgs,
	session *session.ClientSession,
) (*NetworkUpdateResult, error) {
	var networkCreateResult = &NetworkUpdateResult{}
	networkName := strings.TrimSpace(networkUpdate.NetworkName)

	err := checkNetworkNameAvailability(networkName, session)
	if err != nil {
		networkCreateResult = &NetworkUpdateResult{
			Error: &NetworkUpdateError{
				Message: err.Error(),
			},
		}
		return networkCreateResult, nil
	}

	server.Tx(session.Ctx, func(tx server.PgTx) {

		server.RaisePgResult(tx.Exec(
			session.Ctx,
			`
							UPDATE network
							SET
									network_name = $2
							WHERE
									network_id = $1
					`,
			session.ByJwt.NetworkId,
			networkName,
		))

	})

	return networkCreateResult, nil
}

type UpgradeGuestResult struct {
	Error                *UpgradeGuestError              `json:"error,omitempty"`
	VerificationRequired *UpgradeGuestResultVerification `json:"verification_required,omitempty"`
	Network              *UpgradeGuestNetwork            `json:"network,omitempty"`
	UserAuth             *string                         `json:"user_auth,omitempty"`
}

type UpgradeGuestNetwork struct {
	ByJwt *string `json:"by_jwt,omitempty"`
}

type UpgradeGuestResultVerification struct {
	UserAuth string `json:"user_auth"`
}

type UpgradeGuestError struct {
	Message string `json:"message"`
}

func UpgradeGuest(
	upgradeGuest UpgradeGuestArgs,
	session *session.ClientSession,
) (*UpgradeGuestResult, error) {

	userAuth, _ := NormalUserAuthV1(upgradeGuest.UserAuth)

	userAuthAttemptId, allow := UserAuthAttempt(userAuth, session)
	if !allow {
		return nil, maxUserAuthAttemptsError()
	}

	var result = &UpgradeGuestResult{}
	networkName := strings.TrimSpace(upgradeGuest.NetworkName)

	err := checkNetworkNameAvailability(networkName, session)
	if err != nil {
		result = &UpgradeGuestResult{
			Error: &UpgradeGuestError{
				Message: err.Error(),
			},
		}
		return result, nil
	}

	server.Tx(session.Ctx, func(tx server.PgTx) {

		if upgradeGuest.UserAuth != nil {

			/**
			 * Upgrade from guest from email + password
			 */

			if userAuth == nil {
				result = &UpgradeGuestResult{
					Error: &UpgradeGuestError{
						Message: "Invalid email or phone number.",
					},
				}
				return
			}

			/**
			 * Validate the user does not exist
			 */

			exists := userAuthExistsInTx(session.Ctx, tx, *userAuth)

			if exists {
				result = &UpgradeGuestResult{
					Error: &UpgradeGuestError{
						Message: "User already exists",
					},
				}
				return
			}

			/**
			 * Handle password
			 */

			passwordSalt := createPasswordSalt()
			passwordHash := computePasswordHashV1([]byte(*upgradeGuest.Password), passwordSalt)

			/**
			 * Update network user from guest
			 */

			addUserAuth(
				&AddUserAuthArgs{
					UserId:       session.ByJwt.UserId,
					UserAuth:     userAuth,
					PasswordHash: passwordHash,
					PasswordSalt: passwordSalt,
					Verified:     false,
				},
				session.Ctx,
			)

			// do we need to run auditNetworkCreate on the upgrade from guest -> normal account?

			networkNameSearch.Add(session.Ctx, networkName, session.ByJwt.NetworkId, 0)

			result = &UpgradeGuestResult{
				VerificationRequired: &UpgradeGuestResultVerification{
					UserAuth: *userAuth,
				},
			}

		} else if upgradeGuest.AuthJwt != nil && upgradeGuest.AuthJwtType != nil {

			/**
			 * Upgrade from guest from social login
			 */

			authJwt, _ := ParseAuthJwt(*upgradeGuest.AuthJwt, AuthType(*upgradeGuest.AuthJwtType))

			if authJwt != nil {

				/**
				 * Validate the user does not exist
				 */
				normalJwtUserAuth, _ := NormalUserAuth(authJwt.UserAuth)

				exists := userAuthExistsInTx(session.Ctx, tx, *userAuth)

				if exists {
					result = &UpgradeGuestResult{
						Error: &UpgradeGuestError{
							Message: "User already exists",
						},
					}
					return
				}

				/**
				 * Update network user from guest
				 */

				addSsoAuth(
					&AddSsoAuthArgs{
						UserId:        session.ByJwt.UserId,
						AuthJwt:       *upgradeGuest.AuthJwt,
						ParsedAuthJwt: *authJwt,
						AuthJwtType:   SsoAuthType(*upgradeGuest.AuthJwtType),
					},
					session.Ctx,
				)

				SetUserAuthAttemptSuccess(session.Ctx, userAuthAttemptId, true)

				byJwt := jwt.NewByJwt(
					session.ByJwt.NetworkId,
					session.ByJwt.UserId,
					networkName,
					false,
				)
				byJwtSigned := byJwt.Sign()

				result = &UpgradeGuestResult{
					Network: &UpgradeGuestNetwork{
						ByJwt: &byJwtSigned,
					},
					UserAuth: &normalJwtUserAuth,
				}

			}

		} else if upgradeGuest.WalletAuth != nil {
			/**
			 * Upgrade from guest from wallet
			 */
			walletAuths, err := getWalletAuthsByAddressInTx(
				session.Ctx,
				tx,
				upgradeGuest.WalletAuth.PublicKey,
			)
			if err != nil {
				glog.Errorf("Error getting wallet auths by address: %v", err)
				return
			}

			if len(walletAuths) > 0 {
				glog.Infof("Network user already exists with this wallet address")

				result = &UpgradeGuestResult{
					Error: &UpgradeGuestError{
						Message: "User already exists",
					},
				}
				return
			}

			/**
			 * Update network user from guest
			 */

			addWalletAuth(
				&AddWalletAuthArgs{
					WalletAuth: &WalletAuthArgs{
						PublicKey:  upgradeGuest.WalletAuth.PublicKey,
						Blockchain: upgradeGuest.WalletAuth.Blockchain,
						Message:    upgradeGuest.WalletAuth.Message,
						Signature:  upgradeGuest.WalletAuth.Signature,
					},
					UserId: session.ByJwt.UserId,
				},
				session.Ctx,
			)

			SetUserAuthAttemptSuccess(session.Ctx, userAuthAttemptId, true)

			byJwt := jwt.NewByJwt(
				session.ByJwt.NetworkId,
				session.ByJwt.UserId,
				networkName,
				false,
			)
			byJwtSigned := byJwt.Sign()

			result = &UpgradeGuestResult{
				Network: &UpgradeGuestNetwork{
					ByJwt: &byJwtSigned,
				},
				// UserAuth: &normalJwtUserAuth,
			}

		}

		/**
		 * Update the network name
		 */
		server.RaisePgResult(tx.Exec(
			session.Ctx,
			`
				UPDATE network
				SET
					network_name = $2

				WHERE
					network_id = $1
			`,
			session.ByJwt.NetworkId,
			networkName,
		))

	})

	return result, nil
}

/**
 * Upgrade guest with existing account
 */
type UpgradeGuestExistingArgs struct {
	UserAuth    *string         `json:"user_auth,omitempty"`
	Password    *string         `json:"password,omitempty"`
	AuthJwt     *string         `json:"auth_jwt,omitempty"`
	AuthJwtType *string         `json:"auth_jwt_type,omitempty"`
	WalletAuth  *WalletAuthArgs `json:"wallet_auth,omitempty"`
}

type UpgradeGuestExistingError struct {
	Message string `json:"message"`
}

type UpgradeGuestExistingResult struct {
	Error                *UpgradeGuestExistingError                `json:"error,omitempty"`
	VerificationRequired *UpgradeGuestExistingVerificationRequired `json:"verification_required,omitempty"`
	Network              *UpgradeGuestExistingResultNetwork        `json:"network,omitempty"`
	// Error                *AuthLoginWithPasswordResultError        `json:"error,omitempty"`
}

type UpgradeGuestExistingVerificationRequired struct {
	UserAuth string `json:"user_auth"`
}

type UpgradeGuestExistingResultNetwork struct {
	ByJwt *string `json:"by_jwt,omitempty"`
	// NetworkName *string `json:"name,omitempty"`
}

func UpgradeFromGuestExisting(
	upgradeGuestExisting UpgradeGuestExistingArgs,
	session *session.ClientSession,
) (*UpgradeGuestExistingResult, error) {

	if upgradeGuestExisting.UserAuth != nil && upgradeGuestExisting.Password != nil {
		/**
		 * Upgrade from guest from email + password
		 */

		args := AuthLoginWithPasswordArgs{
			UserAuth: *upgradeGuestExisting.UserAuth,
			Password: *upgradeGuestExisting.Password,
		}

		loginResult, err := AuthLoginWithPassword(args, session)
		if err != nil {
			return &UpgradeGuestExistingResult{
				Error: &UpgradeGuestExistingError{
					Message: "Invalid login",
				},
			}, nil
		}

		if loginResult.Error != nil {
			return &UpgradeGuestExistingResult{
				Error: &UpgradeGuestExistingError{
					Message: loginResult.Error.Message,
				},
			}, nil
		}

		if loginResult.Network.ByJwt == nil {

			return &UpgradeGuestExistingResult{
				Error: &UpgradeGuestExistingError{
					Message: "Invalid network token",
				},
			}, nil
		}

		network, err := jwt.ParseByJwt(*loginResult.Network.ByJwt)
		if err != nil {
			return &UpgradeGuestExistingResult{
				Error: &UpgradeGuestExistingError{
					Message: "Error parsing network token",
				},
			}, nil
		}

		err = markUpgradedNetworkId(network.NetworkId, session)
		if err != nil {
			return &UpgradeGuestExistingResult{
				Error: &UpgradeGuestExistingError{
					Message: "Error marking upgraded network id",
				},
			}, nil
		}

		result := &UpgradeGuestExistingResult{
			Network: &UpgradeGuestExistingResultNetwork{
				ByJwt: loginResult.Network.ByJwt,
			},
		}

		if loginResult.VerificationRequired != nil {
			result.VerificationRequired = &UpgradeGuestExistingVerificationRequired{
				UserAuth: loginResult.VerificationRequired.UserAuth,
			}
		}

		return result, nil

	}

	if upgradeGuestExisting.AuthJwt != nil && upgradeGuestExisting.AuthJwtType != nil {
		/**
		 * Upgrade from guest from social login
		 */

		args := AuthLoginArgs{
			AuthJwt:     upgradeGuestExisting.AuthJwt,
			AuthJwtType: upgradeGuestExisting.AuthJwtType,
		}

		return handleAuthLoginUpgrade(args, session)

	}

	if upgradeGuestExisting.WalletAuth != nil {

		args := AuthLoginArgs{
			WalletAuth: upgradeGuestExisting.WalletAuth,
		}

		return handleAuthLoginUpgrade(args, session)

	}

	return &UpgradeGuestExistingResult{
		Error: &UpgradeGuestExistingError{
			Message: "Invalid args",
		},
	}, nil

}

func handleAuthLoginUpgrade(
	args AuthLoginArgs,
	session *session.ClientSession,
) (*UpgradeGuestExistingResult, error) {
	loginResult, err := AuthLogin(args, session)
	if err != nil {
		return &UpgradeGuestExistingResult{
			Error: &UpgradeGuestExistingError{
				Message: "Invalid login",
			},
		}, nil
	}

	if loginResult.Error != nil {
		return &UpgradeGuestExistingResult{
			Error: &UpgradeGuestExistingError{
				Message: loginResult.Error.Message,
			},
		}, nil
	}

	// in this case, we should navigate the user to the network creation view
	if loginResult.Network == nil {
		return &UpgradeGuestExistingResult{}, nil
	}

	network, err := jwt.ParseByJwt(loginResult.Network.ByJwt)
	if err != nil {
		return &UpgradeGuestExistingResult{
			Error: &UpgradeGuestExistingError{
				Message: "Error parsing network token",
			},
		}, nil
	}

	err = markUpgradedNetworkId(network.NetworkId, session)
	if err != nil {
		return &UpgradeGuestExistingResult{
			Error: &UpgradeGuestExistingError{
				Message: "Error marking upgraded network id",
			},
		}, nil
	}

	return &UpgradeGuestExistingResult{
		Network: &UpgradeGuestExistingResultNetwork{
			ByJwt: &loginResult.Network.ByJwt,
		},
	}, nil
}

func markUpgradedNetworkId(
	upgradedNetworkId server.Id,
	session *session.ClientSession,
) error {

	server.Tx(session.Ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			session.Ctx,
			`
				UPDATE network
				SET
						guest_upgrade_network_id = $1
				WHERE
						network_id = $2
			`,
			upgradedNetworkId,
			session.ByJwt.NetworkId,
		))
	})

	return nil
}

type Network struct {
	NetworkId             *server.Id `json:"network_id"`
	NetworkName           string     `json:"network_name"`
	ContainsProfanity     bool       `json:"contains_profanity"`
	AdminUserId           *server.Id `json:"admin_user_id"`
	GuestUpgradeNetworkId *server.Id `json:"guest_upgrade_network_id"`
}

func GetNetwork(
	session *session.ClientSession,
) *Network {
	var network *Network

	server.Tx(session.Ctx, func(tx server.PgTx) {

		result, err := tx.Query(
			session.Ctx,
			`
			SELECT
				network_id,
				network_name,
				admin_user_id,
				guest_upgrade_network_id,
				contains_profanity
			FROM network
			WHERE network_id = $1
		`,
			session.ByJwt.NetworkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {

				network = &Network{}

				server.Raise(result.Scan(
					&network.NetworkId,
					&network.NetworkName,
					&network.AdminUserId,
					&network.GuestUpgradeNetworkId,
					&network.ContainsProfanity,
				))
			}
		})

	})

	return network
}

/**
 * todo - better password validation
 */
func passwordValid(password string) bool {
	if len(password) < MinPasswordLength {
		return false
	}
	return true
}

/**
 * ===
 * Testing util functions
 * ===
 */
func Testing_CreateNetwork(
	ctx context.Context,
	networkId server.Id,
	networkName string,
	adminUserId server.Id,
) (userAuth string) {
	userAuth = fmt.Sprintf("%s@bringyour.com", networkId)
	password := "password"

	passwordSalt := createPasswordSalt()
	passwordHash := computePasswordHashV1([]byte(password), passwordSalt)

	containsProfanity := goaway.IsProfane(networkName)

	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network (network_id, network_name, admin_user_id, contains_profanity)
				VALUES ($1, $2, $3, $4)
			`,
			networkId,
			networkName,
			adminUserId,
			containsProfanity,
		))

		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_user (user_id, user_name, user_auth, verified, password_hash, password_salt)
				VALUES ($1, $2, $3, $4, $5, $6)
			`,
			adminUserId,
			"test",
			userAuth,
			true,
			passwordHash,
			passwordSalt,
		))

		addUserAuth(
			&AddUserAuthArgs{
				UserId:       adminUserId,
				UserAuth:     &userAuth,
				PasswordHash: passwordHash,
				PasswordSalt: passwordSalt,
				Verified:     true,
			}, ctx,
		)
	})

	return
}

func Testing_CreateNetworkByWallet(
	ctx context.Context,
	networkId server.Id,
	networkName string,
	adminUserId server.Id,
	publicKey string,
	signature string,
	message string,
) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network (network_id, network_name, admin_user_id)
				VALUES ($1, $2, $3)
			`,
			networkId,
			networkName,
			adminUserId,
		))

		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_user (user_id, user_name, verified)
				VALUES ($1, $2, $3)
			`,
			adminUserId,
			"test",
			true,
		))

		addWalletAuth(
			&AddWalletAuthArgs{
				WalletAuth: &WalletAuthArgs{
					PublicKey:  publicKey,
					Signature:  signature,
					Message:    message,
					Blockchain: AuthTypeSolana,
				},
				UserId: adminUserId,
			},
			ctx,
		)
	})

}

func Testing_CreateGuestNetwork(
	ctx context.Context,
	networkId server.Id,
	networkName string,
	adminUserId server.Id,
) {

	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network (network_id, network_name, admin_user_id)
				VALUES ($1, $2, $3)
			`,
			networkId,
			networkName,
			adminUserId,
		))

		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_user (user_id, user_name, verified)
				VALUES ($1, $2, $3)
			`,
			adminUserId,
			"test",
			false,
		))

	})

}

func Testing_CreateNetworkSso(
	networkId server.Id,
	userId server.Id,
	authJwt AuthJwt,
	// authJwtType SsoAuthType,
	ctx context.Context,
) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network (network_id, network_name, admin_user_id)
				VALUES ($1, $2, $3)
			`,
			networkId,
			"network_name",
			userId,
		))

		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_user (user_id, user_name, verified)
				VALUES ($1, $2, $3)
			`,
			userId,
			"user_name",
			true,
		))

		addSsoAuth(
			&AddSsoAuthArgs{
				ParsedAuthJwt: authJwt,
				AuthJwt:       "",
				AuthJwtType:   authJwt.AuthType,
				UserId:        userId,
			},
			ctx,
		)
	})

}
