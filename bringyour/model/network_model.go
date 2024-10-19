package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	// "github.com/golang/glog"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/session"

	// "bringyour.com/bringyour/ulid"
	"bringyour.com/bringyour/jwt"
	"bringyour.com/bringyour/search"
)

var networkNameSearch = search.NewSearch("network_name", search.SearchTypeFull)

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
	UserName    string  `json:"user_name"`
	UserAuth    *string `json:"user_auth,omitempty"`
	AuthJwt     *string `json:"auth_jwt,omitempty"`
	AuthJwtType *string `json:"auth_jwt_type,omitempty"`
	Password    *string `json:"password,omitempty"`
	NetworkName string  `json:"network_name"`
	Terms       bool    `json:"terms"`
	GuestMode   bool    `json:"guest_mode"`
}

type NetworkCreateResult struct {
	Network              *NetworkCreateResultNetwork      `json:"network,omitempty"`
	UserAuth             *string                          `json:"user_auth,omitempty"`
	VerificationRequired *NetworkCreateResultVerification `json:"verification_required,omitempty"`
	Error                *NetworkCreateResultError        `json:"error,omitempty"`
}

type NetworkCreateResultNetwork struct {
	ByJwt       *string      `json:"by_jwt,omitempty"`
	NetworkId   bringyour.Id `json:"network_id,omitempty"`
	NetworkName string       `json:"network_name,omitempty"`
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
		var createdNetworkId bringyour.Id
		var networkName string
		var createdUserId bringyour.Id

		bringyour.Tx(session.Ctx, func(tx bringyour.PgTx) {
			var err error

			created = true
			createdUserId = bringyour.NewId()
			createdNetworkId = bringyour.NewId()

			networkName = fmt.Sprintf("guest_%s", bringyour.NewId().String())

			_, err = tx.Exec(
				session.Ctx,
				`
					INSERT INTO network_user
					(user_id, user_name, auth_type)
					VALUES ($1, $2, $3)
				`,
				createdUserId,
				"guest",
				AuthTypeGuest,
			)
			bringyour.Raise(err)

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
			bringyour.Raise(err)
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
	taken := networkNameSearch.AnyAround(session.Ctx, networkCreate.NetworkName, 1)

	if taken {
		result := &NetworkCreateResult{
			Error: &NetworkCreateResultError{
				Message: "Network name not available.",
			},
		}
		return result, nil
	}

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
		var createdNetworkId bringyour.Id

		bringyour.Tx(session.Ctx, func(tx bringyour.PgTx) {
			var result bringyour.PgResult
			var err error

			var userId *bringyour.Id

			result, err = tx.Query(
				session.Ctx,
				`
					SELECT user_id FROM network_user WHERE user_auth = $1
				`,
				userAuth,
			)
			bringyour.WithPgResult(result, err, func() {
				if result.Next() {
					bringyour.Raise(result.Scan(&userId))
				}
			})

			if userId != nil {
				return
			}

			var existingNetworkId *bringyour.Id

			result, err = tx.Query(
				session.Ctx,
				`
					SELECT network_id FROM network WHERE network_name = $1
				`,
				networkCreate.NetworkName,
			)
			bringyour.WithPgResult(result, err, func() {
				if result.Next() {
					bringyour.Raise(result.Scan(&existingNetworkId))
				}
			})

			if existingNetworkId != nil {
				return
			}

			created = true
			createdUserId := bringyour.NewId()
			createdNetworkId = bringyour.NewId()

			passwordSalt := createPasswordSalt()
			passwordHash := computePasswordHashV1([]byte(*networkCreate.Password), passwordSalt)

			_, err = tx.Exec(
				session.Ctx,
				`
					INSERT INTO network_user
					(user_id, user_name, auth_type, user_auth, password_hash, password_salt)
					VALUES ($1, $2, $3, $4, $5, $6)
				`,
				createdUserId,
				networkCreate.UserName,
				AuthTypePassword,
				userAuth,
				passwordHash,
				passwordSalt,
			)
			bringyour.Raise(err)

			_, err = tx.Exec(
				session.Ctx,
				`
					INSERT INTO network
					(network_id, network_name, admin_user_id)
					VALUES ($1, $2, $3)
				`,
				createdNetworkId,
				networkCreate.NetworkName,
				createdUserId,
			)
			bringyour.Raise(err)
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

		// bringyour.Logger().Printf("Parsing JWT\n")
		authJwt, _ := ParseAuthJwt(*networkCreate.AuthJwt, AuthType(*networkCreate.AuthJwtType))
		// bringyour.Logger().Printf("Parse JWT result: %s, %s\n", authJwt, err)
		if authJwt != nil {
			// validate the user does not exist

			normalJwtUserAuth, _ := NormalUserAuth(authJwt.UserAuth)

			// bringyour.Logger().Printf("Parsed JWT as %s\n", authJwt.AuthType)

			created := false
			var createdNetworkId bringyour.Id
			var createdUserId bringyour.Id

			bringyour.Tx(session.Ctx, func(tx bringyour.PgTx) {
				var userId *bringyour.Id

				result, err := tx.Query(
					session.Ctx,
					`
						SELECT user_id FROM network_user WHERE user_auth = $1
					`,
					normalJwtUserAuth,
				)
				bringyour.WithPgResult(result, err, func() {
					if result.Next() {
						bringyour.Raise(result.Scan(&userId))
					}
				})

				if userId != nil {
					// bringyour.Logger().Printf("User already exists\n")
					return
				}

				// bringyour.Logger().Printf("JWT Creating a new network\n")

				created = true
				createdUserId = bringyour.NewId()
				createdNetworkId = bringyour.NewId()

				_, err = tx.Exec(
					session.Ctx,
					`
						INSERT INTO network_user
						(user_id, user_name, auth_type, user_auth, auth_jwt)
						VALUES ($1, $2, $3, $4, $5)
					`,
					createdUserId,
					networkCreate.UserName,
					authJwt.AuthType,
					normalJwtUserAuth,
					networkCreate.AuthJwt,
				)
				if err != nil {
					panic(err)
				}

				_, err = tx.Exec(
					session.Ctx,
					`
						INSERT INTO network
						(network_id, network_name, admin_user_id)
						VALUES ($1, $2, $3)
					`,
					createdNetworkId,
					networkCreate.NetworkName,
					createdUserId,
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
	}

	return nil, errors.New("Invalid login.")
}

func auditNetworkCreate(
	networkCreate NetworkCreateArgs,
	networkId bringyour.Id,
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

func NetworkUpdate(
	networkUpdate NetworkUpdateArgs,
	session *session.ClientSession,
) (*NetworkUpdateResult, error) {

	var existingNetworkId *bringyour.Id
	var networkCreateResult = &NetworkUpdateResult{}
	networkName := strings.TrimSpace(networkUpdate.NetworkName)

	if len(networkName) < 5 {
		networkCreateResult = &NetworkUpdateResult{
			Error: &NetworkUpdateError{
				Message: "Network name must have at least 5 characters",
			},
		}
		return networkCreateResult, nil
	}

	taken := networkNameSearch.AnyAround(session.Ctx, networkName, 1)

	if taken {
		networkCreateResult = &NetworkUpdateResult{
			Error: &NetworkUpdateError{
				Message: "Network name not available.",
			},
		}
		return networkCreateResult, nil
	}

	bringyour.Tx(session.Ctx, func(tx bringyour.PgTx) {

		result, err := tx.Query(
			session.Ctx,
			`
				SELECT network_id FROM network WHERE network_name = $1
			`,
			networkName,
		)
		bringyour.WithPgResult(result, err, func() {
			if result.Next() {
				bringyour.Raise(result.Scan(&existingNetworkId))
			}
		})

		if existingNetworkId != nil {

			networkCreateResult = &NetworkUpdateResult{
				Error: &NetworkUpdateError{
					Message: "Network name not available.",
				},
			}
			return
		}

		bringyour.RaisePgResult(tx.Exec(
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

type Network struct {
	NetworkId   *bringyour.Id `json:"network_id"`
	NetworkName string        `json:"network_name"`
	AdminUserId *bringyour.Id `json:"admin_user_id"`
}

func GetNetwork(
	session *session.ClientSession,
) *Network {
	var network *Network

	bringyour.Tx(session.Ctx, func(tx bringyour.PgTx) {

		result, err := tx.Query(
			session.Ctx,
			`
			SELECT
				network_id,
				network_name,
				admin_user_id
			FROM network 
			WHERE network_id = $1
		`,
			session.ByJwt.NetworkId,
		)
		bringyour.WithPgResult(result, err, func() {
			if result.Next() {

				network = &Network{}

				bringyour.Raise(result.Scan(
					&network.NetworkId,
					&network.NetworkName,
					&network.AdminUserId,
				))
			}
		})

	})

	return network
}

func Testing_CreateNetwork(
	ctx context.Context,
	networkId bringyour.Id,
	networkName string,
	adminUserId bringyour.Id,
) (userAuth string) {
	userAuth = fmt.Sprintf("%s@bringyour.com", networkId)

	bringyour.Tx(ctx, func(tx bringyour.PgTx) {
		bringyour.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network (network_id, network_name, admin_user_id)
				VALUES ($1, $2, $3)
			`,
			networkId,
			networkName,
			adminUserId,
		))

		bringyour.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_user (user_id, user_name, auth_type, user_auth, verified)
				VALUES ($1, $2, $3, $4, $5)
			`,
			adminUserId,
			"test",
			AuthTypePassword,
			userAuth,
			true,
		))
	})

	return
}

func Testing_CreateGuestNetwork(
	ctx context.Context,
	networkId bringyour.Id,
	networkName string,
	adminUserId bringyour.Id,
) {

	bringyour.Tx(ctx, func(tx bringyour.PgTx) {
		bringyour.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network (network_id, network_name, admin_user_id)
				VALUES ($1, $2, $3)
			`,
			networkId,
			networkName,
			adminUserId,
		))

		bringyour.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_user (user_id, user_name, auth_type, verified)
				VALUES ($1, $2, $3, $4)
			`,
			adminUserId,
			"test",
			AuthTypeGuest,
			false,
		))
	})

}
