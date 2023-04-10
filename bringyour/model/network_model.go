package model


import (
	"errors"
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/pgtype"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/ulid"
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

func NetworkCheck(check *NetworkCheckArgs) (*NetworkCheckResult, error) {
	taken := networkNameSearch.AnyAround(check.NetworkName, 3)

	result := &NetworkCheckResult{
		Available: !taken,
	}
	return result, nil
}


type NetworkCreateArgs struct {
	UserName *string `json:"user_name"`
	UserAuth *string `json:"user_auth"`
	AuthJwt *string `json:"auth_jwt"`
	AuthJwtType *string `json:"auth_jwt_type"`
	Password string `json:"password"`
	NetworkName string `json:"network_name"`
	Terms bool `json:"terms"`
}

type NetworkCreateResult struct {
	Network *NetworkCreateResultNetwork `json:"network"`
	ValidationRequired *NetworkCreateResultValidation `json:"validation_required"`
	Error *NetworkCreateResultError `json:"error"`
}

type NetworkCreateResultNetwork struct {
	ByJwt *string `json:"by_jwt"`
	NetworkName *string `json:"network_name"`
}

type NetworkCreateResultValidation struct {
	UserAuth string `json:"user_auth"`
}

type NetworkCreateResultError struct {
	Message string `json:"message"`
}

func NetworkCreate(networkCreate NetworkCreateArgs, session *bringyour.ClientSession) (*NetworkCreateResult, error) {
	userAuth, _ := NormalUserAuthV1(networkCreate.UserAuth)

	var userAuthAttemptId *ulid.ULID
	if session != nil {
		var allow bool
		*userAuthAttemptId, allow = UserAuthAttempt(userAuth, session)
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

		var createdNetworkId *ulid.ULID
		
		bringyour.Tx(func(context context.Context, conn *pgxpool.Conn) {
			var result pgx.Rows
			var err error

			var userId pgtype.UUID

			result, err = conn.Query(
				context,
				`
					SELECT user_id FROM network_user WHERE user_auth = $1
				`,
				userAuth,
			)
			if err != nil {
				panic(err)
			}
			if result.Next() {
				result.Scan(&userId)
			}

			if !userId.Valid {
				createdUserId := ulid.Make()
				*createdNetworkId = ulid.Make()

				passwordSalt := createPasswordSalt()
				passwordHash := computePasswordHashV1([]byte(networkCreate.Password), passwordSalt)

				conn.Exec(
					context,
					`
						INSERT INTO TABLE network_user
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

				conn.Exec(
					context,
					`
						INSERT INTO TABLE network
						(network_id, network_name, admin_user_id)
						VALUES ($1, $2, $3)
					`,
					ulid.ToPg(createdNetworkId),
					networkCreate.NetworkName,
					ulid.ToPg(&createdUserId),
				)
			}
		})
		if createdNetworkId != nil {
			networkNameSearch.Add(networkCreate.NetworkName, *createdNetworkId)
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
		authJwt := ParseAuthJwt(*networkCreate.AuthJwt, *networkCreate.AuthJwtType)
		if authJwt != nil {
			// validate the user does not exist

			var createdNetworkId *ulid.ULID
			var createdUserId *ulid.ULID

			bringyour.Tx(func(context context.Context, conn *pgxpool.Conn) {
				var result pgx.Rows
				var err error

				var userId pgtype.UUID

				result, err = conn.Query(
					context,
					`
						SELECT user_id FROM network_user WHERE user_auth = $1
					`,
					authJwt.UserAuth,
				)
				if err != nil {
					panic(err)
				}
				if result.Next() {
					result.Scan(&userId)
				}

				if !userId.Valid {
					*createdUserId = ulid.Make()
					*createdNetworkId = ulid.Make()

					conn.Exec(
						context,
						`
							INSERT INTO TABLE network_user
							(user_id, user_name, auth_type, user_auth, auth_jwt)
							VALUES ($1, $2, $3, $4, $5, $6)
						`,
						ulid.ToPg(createdUserId),
						networkCreate.UserName,
						authJwt.AuthType,
						userAuth,
						networkCreate.AuthJwt,
					)

					conn.Exec(
						context,
						`
							INSERT INTO TABLE network
							(network_id, network_name, admin_user_id)
							VALUES ($1, $2, $3)
						`,
						ulid.ToPg(createdNetworkId),
						networkCreate.NetworkName,
						ulid.ToPg(createdUserId),
					)
				}
			})
			if createdNetworkId != nil {
				networkNameSearch.Add(networkCreate.NetworkName, *createdNetworkId)
				// fixme log an audit event that account created and terms were accepted with a client ip with the auth jwt type

				if userAuthAttemptId != nil {
					SetUserAuthAttemptSuccess(*userAuthAttemptId, true)
				}

				// successful login
				byJwt := jwt.NewByJwt(
					*createdNetworkId,
					*createdUserId,
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
