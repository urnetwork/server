package model


import (
	"errors"
	// "context"
	"encoding/json"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/session"
	// "bringyour.com/bringyour/ulid"
	"bringyour.com/bringyour/search"
	"bringyour.com/bringyour/jwt"
)


var networkNameSearch = search.NewSearch("network_name", search.SearchTypeFull)


type NetworkCheckArgs struct {
	NetworkName string  `json:"network_name"`
}

type NetworkCheckResult struct {
	Available bool  `json:"available"`
}

func NetworkCheck(check *NetworkCheckArgs, session *session.ClientSession) (*NetworkCheckResult, error) {
	taken := networkNameSearch.AnyAround(session.Ctx, check.NetworkName, 3)

	result := &NetworkCheckResult{
		Available: !taken,
	}
	return result, nil
}


type NetworkCreateArgs struct {
	UserName string `json:"user_name"`
	UserAuth *string `json:"user_auth,omitempty"`
	AuthJwt *string `json:"auth_jwt,omitempty"`
	AuthJwtType *string `json:"auth_jwt_type,omitempty"`
	Password *string `json:"password,omitempty"`
	NetworkName string `json:"network_name"`
	Terms bool `json:"terms"`
}

type NetworkCreateResult struct {
	Network *NetworkCreateResultNetwork `json:"network,omitempty"`
	VerificationRequired *NetworkCreateResultVerification `json:"verification_required,omitempty"`
	Error *NetworkCreateResultError `json:"error,omitempty"`
}

type NetworkCreateResultNetwork struct {
	ByJwt *string `json:"by_jwt,omitempty"`
	NetworkName *string `json:"network_name,omitempty"`
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

	taken := networkNameSearch.AnyAround(session.Ctx, networkCreate.NetworkName, 3)

	if taken {
		result := &NetworkCreateResult{
			Error: &NetworkCreateResultError{
				Message: "Network name not available.",
			},
		}
		return result, nil
	}

	if !networkCreate.Terms {
		result := &NetworkCreateResult{
			Error: &NetworkCreateResultError{
				Message: "The terms of service and privacy policy must be accepted.",
			},
		}
		return result, nil
	}

	if networkCreate.UserAuth != nil {
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

			if userId == nil {
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
					NetworkName: &networkCreate.NetworkName,
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
		bringyour.Logger().Printf("Parsing JWT\n")
		authJwt := ParseAuthJwt(*networkCreate.AuthJwt, AuthType(*networkCreate.AuthJwtType))
		if authJwt != nil {
			// validate the user does not exist

			bringyour.Logger().Printf("Parsed JWT as %s\n", authJwt.AuthType)

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
					authJwt.UserAuth,
				)
				bringyour.WithPgResult(result, err, func() {
					if result.Next() {
						bringyour.Raise(result.Scan(&userId))
					}
				})

				if userId != nil {
					bringyour.Logger().Printf("User already exists\n")
					return
				}

				bringyour.Logger().Printf("JWT Creating a new network\n")

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
					authJwt.UserAuth,
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
				)
				byJwtSigned := byJwt.Sign()
				result := &NetworkCreateResult{
					Network: &NetworkCreateResultNetwork{
						ByJwt: &byJwtSigned,
					},
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
		ClientAddress string `json:"client_address"`
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
