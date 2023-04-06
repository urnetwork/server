package model


import (
	"encoding/json"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/ulid"
)



networkNameSearch := bringyour.NewSearch("network_name", bringyour.SearchTypeFull)



type NetworkCheckArgs struct {
	NetworkName string
}

type NetworkCheckResult struct {
	Available bool
}

func NetworkCheck(check *NetworkCheckArgs) (*NetworkCheckResult, error) {
	taken := networkNameSearch.AnyAround(check.NetworkName, 3)

	result := &NetworkCheckResult{
		Available: !taken
	}
	return result, nil
}



type NetworkCreateArgs struct {
	userName *string
	userAuth *string
	authJwt *string
	password string
	networkName string
	terms bool
}

type NetworkCreateResult struct {
	network *NetworkCreateResultNetwork
	validatonRequired *NetworkCreateResultValidation
	error NetworkCreateResultError
}

type NetworkCreateResultNetwork struct {
	byJwt *string
	networkName *string
}

type NetworkCreateResultValidation struct {
	userAuth string
}

type NetworkCreateResultError struct {
	message string
}


func NetworkCreate(create *NetworkCreate, session *bringyour.ClientSession) (*NetworkCreateResult, error) {
	userAuth, _ := normalUserAuthV1(create.userAuth)

	var userAuthAttemptId *ulid.ULID
	if session != nil {
		var allow bool
		userAuthAttemptId, allow = UserAuthAttempt(userAuth, *session)
		if !allow {
			return nil, maxUserAuthAttemptsError()
		}
	}

	taken := networkNameSearch.AnyAround(create.NetworkName, 3)

	if taken {
		result := &NetworkCreateResult{
			Error: &NetworkCreateResultError{
				Message: "Network name not available."
			}
		}
		return result, nil
	}

	if !terms {
		result := &NetworkCreateResult{
			Error: &NetworkCreateResultError{
				Message: "The terms of service and privacy policy must be accepted."
			}
		}
		return result, nil
	}



	if userAuth != nil {
		// validate the user does not exist

		var userId pgtype.UUID
		bringyour.Db(func(context context.Context, conn *pgxpool.Conn) {
			conn.Exec(
				`
					SELECT user_id FROM network_user WHERE user_auth = $1
				`,
				userAuth,
			)
			if conn.Next() {
				conn.Scan(&userId)
			}
		})

		if !userId.Valid {
			userId := ulid.Make()
			networkId := ulid.Make()

			passwordSalt := createPasswordSalt()
			passwordHash := computePasswordHashV1([]byte(create.Password), passwordSalt)

			bringyour.Tx(func(context context.Context, conn *pgxpool.Conn) {
				conn.Exec(
					`
						INSERT INTO TABLE network_user
						(user_id, user_name, auth_type, user_auth, password_hash, password_salt)
						VALUES ($1, $2, $3, $4, $5, $6)
					`,
					ulid.ToPg(userId),
					create.UserName,
					AuthTypePassword,
					userAuth,
					passwordHash,
					passwordSalt,
				)

				conn.Exec(
					`
						INSERT INTO TABLE network
						(network_id, network_name, admin_user_id)
						VALUES ($1, $2, $3)
					`,
					ulid.ToPg(networkId),
					create.NetworkName,
					ulid.ToPg(userId),
				)
			})

			networkNameSearch.Add(create.NetworkName, networkId)
			// fixme log an audit event that account created and terms were accepted with a client ip with the auth type

			result := &NetworkCreateResultNetwork{
				ValidatonRequired: &NetworkCreateResultValidation{
					UserAuth: userAuth,
				},
				Network: &NetworkCreateResultNetwork{
					NetworkName: create.NetworkName
				}
			}
			return result, nil
		}
	} else if create.authJwt != nil {
		authJwt := ParseAuthJwt(create.AuthJwt, create.AuthJwtType)
		if authJwt != nil {
			// validate the user does not exist

			var userId pgtype.UUID
			bringyour.Db(func(context context.Context, conn *pgxpool.Conn) {
				conn.Exec(
					`
						SELECT user_id FROM network_user WHERE user_auth = $1
					`,
					authJwt.userAuth,
				)
				if conn.Next() {
					conn.Scan(&userId)
				}
			})

			if !userId.Valid {
				userId := ulid.Make()
				networkId := ulid.Make()

				bringyour.Tx(func(context context.Context, conn *pgxpool.Conn) {
					conn.Exec(
						`
							INSERT INTO TABLE network_user
							(user_id, user_name, auth_type, user_auth, auth_jwt)
							VALUES ($1, $2, $3, $4, $5, $6)
						`,
						ulid.ToPg(userId),
						create.UserName,
						authJwt.authType,
						userAuth,
						create.AuthJwt,
					)

					conn.Exec(
						`
							INSERT INTO TABLE network
							(network_id, network_name, admin_user_id)
							VALUES ($1, $2, $3)
						`,
						ulid.ToPg(networkId),
						create.NetworkName,
						ulid.ToPg(userId),
					)
				})

				networkNameSearch.Add(create.NetworkName, networkId)
				// fixme log an audit event that account created and terms were accepted with a client ip with the auth jwt type

				SetUserAuthAttemptSuccess(userAuthAttemptId, true)

				// successful login
				byJwt := CreateByJwt(
					networkId,
					userId,
					create.NetworkName,
				)

				result := &NetworkCreateResultNetwork{
					Network: &NetworkCreateResultNetwork{
						ByJwt: byJwt.String()
					}
				}
				return result, nil
			}
		}
	}
	
	return nil, error("Invalid login.")
}




// bringyour
// lawgiver-insole-truck-splutter

// nlen, dim, dlen, network_id

/*
SELECT network_id, SUM(sim) FROM
(
    SELECT network_id, 3 - ABS(3 - dlen) AS sim
    FROM Test
    WHERE 5 <= nlen AND nlen <= 10 AND dim = 'a' AND 0 <= dlen AND dlen <= 4
    UNION ALL
    ; next dim
) TestSim
GROUP BY network_id
HAVING 7 <= SUM(sim) AND SUM(sim) <= 10
;
*/