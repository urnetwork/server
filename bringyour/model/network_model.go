package model


import (
	"errors"
	"context"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/ulid"
	"bringyour.com/bringyour/search"
	"bringyour.com/bringyour/jwt"
)



var networkNameSearch = search.NewSearch("network_name", search.SearchTypeFull)



type NetworkCheckArgs struct {
	NetworkName string  `json:"networkName"`
}

type NetworkCheckResult struct {
	Available bool  `json:"available"`
}

func NetworkCheck(check *NetworkCheckArgs) (*NetworkCheckResult, error) {
	taken := networkNameSearch.AnyAround(check.NetworkName, 3)

	result := &NetworkCheckResult{
		Available: !taken,
	}
	return result, nil
}


type NetworkCreateArgs struct {
	UserName *string `json:"userName"`
	UserAuth *string `json:"userAuth"`
	AuthJwt *string `json:"authJwt"`
	AuthJwtType *string `json:"authJwtType"`
	Password string `json:"password"`
	NetworkName string `json:"networkName"`
	Terms bool `json:"terms"`
}

type NetworkCreateResult struct {
	Network *NetworkCreateResultNetwork `json:"network,omitempty"`
	ValidationRequired *NetworkCreateResultValidation `json:"validationRequired,omitempty"`
	Error *NetworkCreateResultError `json:"error,omitempty"`
}

type NetworkCreateResultNetwork struct {
	ByJwt *string `json:"byJwt,omitempty"`
	NetworkName *string `json:"networkName,omitempty"`
}

type NetworkCreateResultValidation struct {
	UserAuth string `json:"userAuth"`
}

type NetworkCreateResultError struct {
	Message string `json:"message"`
}

func NetworkCreate(networkCreate NetworkCreateArgs, session *bringyour.ClientSession) (*NetworkCreateResult, error) {
	userAuth, _ := NormalUserAuthV1(networkCreate.UserAuth)

	var userAuthAttemptId *ulid.ULID
	if session != nil {
		var allow bool
		userAuthAttemptId, allow = UserAuthAttempt(userAuth, session)
		if !allow {
			return nil, maxUserAuthAttemptsError()
		}
	}

	taken := networkNameSearch.AnyAround(networkCreate.NetworkName, 3)

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



	if userAuth != nil {
		// validate the user does not exist

		created := false
		var createdNetworkId ulid.ULID
		
		bringyour.Tx(func(context context.Context, tx bringyour.PgTx) {
			var result bringyour.PgResult
			var err error

			var userId bringyour.PgUUID

			result, err = tx.Query(
				context,
				`
					SELECT user_id FROM network_user WHERE user_auth = $1
				`,
				userAuth,
			)
			bringyour.With(result, err, func() {
				if result.Next() {
					bringyour.Raise(
						result.Scan(&userId),
					)
				}
			})

			if !userId.Valid {
				created = true
				createdUserId := ulid.Make()
				createdNetworkId = ulid.Make()

				passwordSalt := createPasswordSalt()
				passwordHash := computePasswordHashV1([]byte(networkCreate.Password), passwordSalt)

				_, err = tx.Exec(
					context,
					`
						INSERT INTO network_user
						(user_id, user_name, auth_type, user_auth, password_hash, password_salt)
						VALUES ($1, $2, $3, $4, $5, $6)
					`,
					ulid.ToPg(&createdUserId),
					networkCreate.UserName,
					AuthTypePassword,
					userAuth,
					passwordHash,
					passwordSalt,
				)
				bringyour.Raise(err)

				_, err = tx.Exec(
					context,
					`
						INSERT INTO network
						(network_id, network_name, admin_user_id)
						VALUES ($1, $2, $3)
					`,
					ulid.ToPg(&createdNetworkId),
					networkCreate.NetworkName,
					ulid.ToPg(&createdUserId),
				)
				bringyour.Raise(err)
			}
		})
		if created {
			networkNameSearch.Add(networkCreate.NetworkName, createdNetworkId)
			// fixme log an audit event that account created and terms were accepted with a client ip with the auth type

			result := &NetworkCreateResult{
				ValidationRequired: &NetworkCreateResultValidation{
					UserAuth: *userAuth,
				},
				Network: &NetworkCreateResultNetwork{
					NetworkName: &networkCreate.NetworkName,
				},
			}
			return result, nil
		}
	} else if networkCreate.AuthJwt != nil && networkCreate.AuthJwtType != nil {
		authJwt := ParseAuthJwt(*networkCreate.AuthJwt, AuthType(*networkCreate.AuthJwtType))
		if authJwt != nil {
			// validate the user does not exist

			created := false
			var createdNetworkId ulid.ULID
			var createdUserId ulid.ULID

			bringyour.Tx(func(context context.Context, tx bringyour.PgTx) {
				var result bringyour.PgResult
				var err error

				var userId bringyour.PgUUID

				result, err = tx.Query(
					context,
					`
						SELECT user_id FROM network_user WHERE user_auth = $1
					`,
					authJwt.UserAuth,
				)
				bringyour.With(result, err, func() {
					if result.Next() {
						bringyour.Raise(
							result.Scan(&userId),
						)
					}
				})

				if !userId.Valid {
					bringyour.Logger().Printf("JWT Creating a new network\n")

					created = true
					createdUserId = ulid.Make()
					createdNetworkId = ulid.Make()

					var err error

					_, err = tx.Exec(
						context,
						`
							INSERT INTO network_user
							(user_id, user_name, auth_type, user_auth, auth_jwt)
							VALUES ($1, $2, $3, $4, $5)
						`,
						ulid.ToPg(&createdUserId),
						networkCreate.UserName,
						authJwt.AuthType,
						authJwt.UserAuth,
						networkCreate.AuthJwt,
					)
					if err != nil {
						panic(err)
					}

					_, err = tx.Exec(
						context,
						`
							INSERT INTO network
							(network_id, network_name, admin_user_id)
							VALUES ($1, $2, $3)
						`,
						ulid.ToPg(&createdNetworkId),
						networkCreate.NetworkName,
						ulid.ToPg(&createdUserId),
					)
					if err != nil {
						panic(err)
					}
				}
			})
			if created {
				networkNameSearch.Add(networkCreate.NetworkName, createdNetworkId)
				// fixme log an audit event that account created and terms were accepted with a client ip with the auth jwt type

				if userAuthAttemptId != nil {
					SetUserAuthAttemptSuccess(*userAuthAttemptId, true)
				}

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
			}
		}
	}
	
	return nil, errors.New("Invalid login.")
}
