package model

import (
	"fmt"
	"net/url"

	// "strings"
	"crypto/rand"
	"encoding/hex"
	"image/color"
	"time"

	// "errors"

	// FIXME remove
	qrcode "github.com/skip2/go-qrcode"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/session"
)

type CodeType = string

const (
	CodeTypeAdopt CodeType = "adopt"
	CodeTypeShare CodeType = "share"
)

const NetworkAddLimitTimeout = 30 * time.Minute
const NetworkAddLimit = 10
const AdoptCodeExpireTimeout = 15 * time.Minute

type DeviceAddArgs struct {
	Code string `json:"code"`
}

type DeviceAddResult struct {
	CodeType              CodeType        `json:"code_type,omitempty"`
	Code                  string          `json:"code,omitempty"`
	DeviceName            string          `json:"device_name,omitempty"`
	AssociatedNetworkName string          `json:"associated_network_name,omitempty"`
	ClientId              server.Id       `json:"client_id,omitempty"`
	DurationMinutes       float64         `json:"duration_minutes,omitempty"`
	Error                 *DeviceAddError `json:"error,omitempty"`
}

type DeviceAddError struct {
	Message string `json:"message"`
}

func DeviceAdd(
	add *DeviceAddArgs,
	clientSession *session.ClientSession,
) (addResult *DeviceAddResult, returnErr error) {
	server.Tx(clientSession.Ctx, func(tx server.PgTx) {
		result, err := tx.Query(
			clientSession.Ctx,
			`
                SELECT
                    device_association_id,
                    code_type
                FROM device_association_code
                WHERE
                    code = $1
            `,
			add.Code,
		)

		var deviceAssociationId *server.Id
		var codeType CodeType

		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(
					&deviceAssociationId,
					&codeType,
				))
			}
		})

		addTime := server.NowUtc()

		server.RaisePgResult(tx.Exec(
			clientSession.Ctx,
			`
                INSERT INTO device_add_history (
                    network_id,
                    add_time,
                    device_add_id,
                    code,
                    device_association_id
                ) VALUES ($1, $2, $3, $4, $5)
            `,
			clientSession.ByJwt.NetworkId,
			addTime,
			server.NewId(),
			add.Code,
			deviceAssociationId,
		))

		result, err = tx.Query(
			clientSession.Ctx,
			`
                SELECT
                    COUNT(*) AS add_count
                FROM device_add_history
                WHERE
                    network_id = $1 AND
                    $2 <= add_time
            `,
			clientSession.ByJwt.NetworkId,
			addTime.Add(-NetworkAddLimitTimeout),
		)

		var limitExceeded bool

		server.WithPgResult(result, err, func() {
			result.Next()
			var addCount int
			server.Raise(result.Scan(&addCount))
			limitExceeded = (NetworkAddLimit <= addCount)
		})

		if deviceAssociationId == nil || limitExceeded {
			// returnErr = errors.New("TEST limit exceeded")
			return
		}

		switch codeType {
		case CodeTypeAdopt:
			tag := server.RaisePgResult(tx.Exec(
				clientSession.Ctx,
				`
                    UPDATE device_adopt
                    SET
                        owner_network_id = $2,
                        owner_user_id = $3
                    WHERE
                        device_association_id = $1 AND
                        owner_network_id IS NULL AND
                        $4 < expire_time
                `,
				deviceAssociationId,
				clientSession.ByJwt.NetworkId,
				clientSession.ByJwt.UserId,
				addTime,
			))
			if tag.RowsAffected() == 0 {
				// returnErr = errors.New("TEST adopt update")
				return
			}

			result, err := tx.Query(
				clientSession.Ctx,
				`
                    SELECT

                        COALESCE(device_association_name.device_name, device_adopt.device_name) AS device_name,
                        device_adopt.expire_time

                    FROM device_adopt
                    LEFT JOIN device_association_name ON
                        device_association_name.device_association_id = device_adopt.device_association_id AND
                        device_association_name.network_id = $2
                    WHERE
                        device_adopt.device_association_id = $1 AND
                        confirmed = false AND
                        $3 < device_adopt.expire_time
                `,
				deviceAssociationId,
				clientSession.ByJwt.NetworkId,
				addTime,
			)

			addResult = &DeviceAddResult{
				Code:     add.Code,
				CodeType: codeType,
			}

			server.WithPgResult(result, err, func() {
				result.Next()
				var endTime time.Time
				server.Raise(result.Scan(
					&addResult.DeviceName,
					&endTime,
				))
				addResult.DurationMinutes = float64(endTime.Sub(addTime)) / float64(time.Minute)
			})
			return

		case CodeTypeShare:
			tag := server.RaisePgResult(tx.Exec(
				clientSession.Ctx,
				`
                    UPDATE device_share
                    SET
                        guest_network_id = $2
                    WHERE
                        device_association_id = $1 AND
                        guest_network_id IS NULL AND
                        source_network_id != $2
                `,
				deviceAssociationId,
				clientSession.ByJwt.NetworkId,
			))
			if tag.RowsAffected() == 0 {
				// returnErr = errors.New("TEST share update")
				return
			}

			result, err := tx.Query(
				clientSession.Ctx,
				`
                    SELECT

                        COALESCE(device_association_name.device_name, device_share.device_name) AS device_name,
                        device_share.client_id,
                        network.network_name

                    FROM device_share
                    INNER JOIN device_association_code ON
                        device_association_code.device_association_id = device_share.device_association_id
                    LEFT JOIN device_association_name ON
                        device_association_name.device_association_id = device_share.device_association_id AND
                        device_association_name.network_id = $2
                    INNER JOIN network ON
                        network.network_id = device_share.source_network_id
                    WHERE
                        device_share.device_association_id = $1
                `,
				deviceAssociationId,
				clientSession.ByJwt.NetworkId,
			)

			addResult = &DeviceAddResult{
				Code:     add.Code,
				CodeType: codeType,
			}

			server.WithPgResult(result, err, func() {
				result.Next()
				server.Raise(result.Scan(
					&addResult.DeviceName,
					&addResult.ClientId,
					&addResult.AssociatedNetworkName,
				))
			})
			return

		default:
			return
		}
	})

	if addResult == nil && returnErr == nil {
		// returnErr = errors.New("TEST default")
		addResult = &DeviceAddResult{
			Error: &DeviceAddError{
				Message: "Invalid code.",
			},
		}
		return
	}

	return
}

type DeviceCreateShareCodeArgs struct {
	ClientId   server.Id `json:"client_id"`
	DeviceName string    `json:"device_name,omitempty"`
}

type DeviceCreateShareCodeResult struct {
	ShareCode string                      `json:"share_code,omitempty"`
	Error     *DeviceCreateShareCodeError `json:"error,omitempty"`
}

type DeviceCreateShareCodeError struct {
	Message string `json:"message"`
}

func DeviceCreateShareCode(
	createShareCode *DeviceCreateShareCodeArgs,
	clientSession *session.ClientSession,
) (createShareCodeResult *DeviceCreateShareCodeResult, returnErr error) {
	server.Tx(clientSession.Ctx, func(tx server.PgTx) {
		// verify that the client id is owned by this network
		result, err := tx.Query(
			clientSession.Ctx,
			`
                SELECT
                    active
                FROM network_client
                WHERE
                    network_id = $1 AND
                    client_id = $2
            `,
			clientSession.ByJwt.NetworkId,
			createShareCode.ClientId,
		)

		active := false

		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&active))
			}
		})

		if !active {
			createShareCodeResult = &DeviceCreateShareCodeResult{
				Error: &DeviceCreateShareCodeError{
					Message: "Invalid client.",
				},
			}
			return
		}

		// TODO: use fewer words from a larger dictionary
		// use 6 words from bip39 as the code (64 bits)
		shareCode, err := newCode()
		if err != nil {
			returnErr = err
			return
		}

		deviceAssociationId := server.NewId()

		server.RaisePgResult(tx.Exec(
			clientSession.Ctx,
			`
                INSERT INTO device_association_code (
                    device_association_id,
                    code,
                    code_type
                ) VALUES ($1, $2, $3)
            `,
			deviceAssociationId,
			shareCode,
			CodeTypeShare,
		))

		server.RaisePgResult(tx.Exec(
			clientSession.Ctx,
			`
                INSERT INTO device_share (
                    device_association_id,
                    device_name,
                    source_network_id,
                    client_id
                ) VALUES ($1, $2, $3, $4)
            `,
			deviceAssociationId,
			createShareCode.DeviceName,
			clientSession.ByJwt.NetworkId,
			createShareCode.ClientId,
		))

		createShareCodeResult = &DeviceCreateShareCodeResult{
			ShareCode: shareCode,
		}
		return
	})

	return
}

type DeviceShareCodeQRArgs struct {
	ShareCode string `json:"share_code"`
}

type DeviceShareCodeQRResult struct {
	PngBytes []byte `json:"png_bytes"`
}

func DeviceShareCodeQR(
	shareCodeQR *DeviceShareCodeQRArgs,
	clientSession *session.ClientSession,
) (*DeviceShareCodeQRResult, error) {
	addUrl := fmt.Sprintf(
		"https://app.bringyour.com/?add=%s",
		url.QueryEscape(shareCodeQR.ShareCode),
	)

	pngBytes, err := qrPngBytes(addUrl)
	if err != nil {
		return nil, err
	}

	return &DeviceShareCodeQRResult{
		PngBytes: pngBytes,
	}, err
}

type DeviceShareStatusArgs struct {
	ShareCode string `json:"share_code"`
}

type DeviceShareStatusResult struct {
	Pending               bool                    `json:"pending,omitempty"`
	AssociatedNetworkName string                  `json:"associated_network_name,omitempty"`
	Error                 *DeviceShareStatusError `json:"error,omitempty"`
}

type DeviceShareStatusError struct {
	Message string `json:"message"`
}

func DeviceShareStatus(
	shareStatus *DeviceShareStatusArgs,
	clientSession *session.ClientSession,
) (shareStatusResult *DeviceShareStatusResult, returnErr error) {
	server.Db(clientSession.Ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			clientSession.Ctx,
			`
                SELECT
                    device_share.confirmed,
                    network.network_name
                FROM device_share
                LEFT JOIN network ON
                    network.network_id = device_share.guest_network_id
                INNER JOIN device_association_code ON
                    device_association_code.device_association_id = device_share.device_association_id AND
                    device_association_code.code = $1 AND
                    device_association_code.code_type = $2
            `,
			shareStatus.ShareCode,
			CodeTypeShare,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				shareStatusResult = &DeviceShareStatusResult{}
				var confirmed bool
				var networkName *string
				server.Raise(result.Scan(
					&confirmed,
					&networkName,
				))
				shareStatusResult.Pending = !confirmed
				if networkName != nil {
					shareStatusResult.AssociatedNetworkName = *networkName
				}
			} else {
				shareStatusResult = &DeviceShareStatusResult{
					Error: &DeviceShareStatusError{
						Message: "Invalid share code.",
					},
				}
			}
		})
	})

	return
}

type DeviceConfirmShareArgs struct {
	ShareCode             string `json:"share_code"`
	AssociatedNetworkName string `json:"associated_network_name,omitempty"`
}

type DeviceConfirmShareResult struct {
	AssociatedNetworkName string                   `json:"associated_network_name,omitempty"`
	Error                 *DeviceConfirmShareError `json:"error,omitempty"`
}

type DeviceConfirmShareError struct {
	Message string `json:"message"`
}

func DeviceConfirmShare(
	confirmShare *DeviceConfirmShareArgs,
	clientSession *session.ClientSession,
) (confirmShareResult *DeviceConfirmShareResult, returnErr error) {
	server.Tx(clientSession.Ctx, func(tx server.PgTx) {
		result, err := tx.Query(
			clientSession.Ctx,
			`
                SELECT
                    device_association_id
                FROM device_association_code
                WHERE
                    code = $1 AND
                    code_type = $2
            `,
			confirmShare.ShareCode,
			CodeTypeShare,
		)

		var deviceAssociationId *server.Id

		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&deviceAssociationId))
			}
		})

		if deviceAssociationId == nil {
			return
		}

		tag := server.RaisePgResult(tx.Exec(
			clientSession.Ctx,
			`
                UPDATE device_share
                SET
                    confirmed = true
                FROM network
                WHERE
                    device_share.device_association_id = $1 AND
                    network.network_name = $2 AND
                    device_share.guest_network_id = network.network_id AND
                    device_share.confirmed = false
            `,
			deviceAssociationId,
			confirmShare.AssociatedNetworkName,
		))
		if tag.RowsAffected() == 0 {
			return
		}

		confirmShareResult = &DeviceConfirmShareResult{
			AssociatedNetworkName: confirmShare.AssociatedNetworkName,
		}
		return
	})

	if confirmShareResult == nil && returnErr == nil {
		confirmShareResult = &DeviceConfirmShareResult{
			Error: &DeviceConfirmShareError{
				Message: "Invalid code.",
			},
		}
		return
	}

	return
}

type DeviceCreateAdoptCodeArgs struct {
	DeviceName string `json:"device_name"`
	DeviceSpec string `json:"device_spec"`
}

type DeviceCreateAdoptCodeResult struct {
	AdoptCode       string                       `json:"adopt_code,omitempty"`
	AdoptSecret     string                       `json:"share_code,omitempty"`
	DurationMinutes float64                      `json:"duration_minutes,omitempty"`
	Error           *DeviceCreateAdoptCodeResult `json:"error,omitempty"`
}

type DeviceCreateAdoptCodeError struct {
	Message string `json:"message"`
}

func DeviceCreateAdoptCode(
	createAdoptCode *DeviceCreateAdoptCodeArgs,
	clientSession *session.ClientSession,
) (createAdoptCodeResult *DeviceCreateAdoptCodeResult, returnErr error) {
	server.Tx(clientSession.Ctx, func(tx server.PgTx) {
		createTime := server.NowUtc()
		expireTime := createTime.Add(AdoptCodeExpireTimeout)

		deviceAssociationId := server.NewId()

		// TODO: use fewer words from a larger dictionary
		// use 6 words from bip39 as the code (64 bits)
		adoptCode, err := newCode()
		if err != nil {
			returnErr = err
			return
		}

		// 128 bits
		adoptSecretBytes := make([]byte, 128)
		if _, err := rand.Read(adoptSecretBytes); err != nil {
			returnErr = err
			return
		}
		adoptSecret := hex.EncodeToString(adoptSecretBytes)

		server.RaisePgResult(tx.Exec(
			clientSession.Ctx,
			`
                INSERT INTO device_association_code (
                    device_association_id,
                    code,
                    code_type
                ) VALUES ($1, $2, $3)
            `,
			deviceAssociationId,
			adoptCode,
			CodeTypeAdopt,
		))

		server.RaisePgResult(tx.Exec(
			clientSession.Ctx,
			`
                INSERT INTO device_adopt (
                    device_association_id,
                    adopt_secret,
                    device_name,
                    device_spec,
                    create_time,
                    expire_time
                ) VALUES ($1, $2, $3, $4, $5, $6)
            `,
			deviceAssociationId,
			adoptSecret,
			createAdoptCode.DeviceName,
			createAdoptCode.DeviceSpec,
			createTime,
			expireTime,
		))

		createAdoptCodeResult = &DeviceCreateAdoptCodeResult{
			AdoptCode:       adoptCode,
			AdoptSecret:     adoptSecret,
			DurationMinutes: float64(expireTime.Sub(createTime)) / float64(time.Minute),
		}
	})

	return
}

type DeviceAdoptCodeQRArgs struct {
	AdoptCode string `json:"share_code"`
}

type DeviceAdoptCodeQRResult struct {
	PngBytes []byte `json:"png_bytes"`
}

func DeviceAdoptCodeQR(
	adoptCodeQR *DeviceAdoptCodeQRArgs,
	clientSession *session.ClientSession,
) (*DeviceAdoptCodeQRResult, error) {
	addUrl := fmt.Sprintf(
		"https://app.bringyour.com/?add=%s",
		url.QueryEscape(adoptCodeQR.AdoptCode),
	)

	pngBytes, err := qrPngBytes(addUrl)
	if err != nil {
		return nil, err
	}

	return &DeviceAdoptCodeQRResult{
		PngBytes: pngBytes,
	}, err
}

type DeviceAdoptStatusArgs struct {
	AdoptCode string `json:"adopt_code"`
}

type DeviceAdoptStatusResult struct {
	Pending               bool                    `json:"pending,omitempty"`
	AssociatedNetworkName string                  `json:"associated_network_name,omitempty"`
	Error                 *DeviceAdoptStatusError `json:"error,omitempty"`
}

type DeviceAdoptStatusError struct {
	Message string `json:"message"`
}

func DeviceAdoptStatus(
	adoptStatus *DeviceAdoptStatusArgs,
	clientSession *session.ClientSession,
) (adoptStatusResult *DeviceAdoptStatusResult, returnErr error) {
	server.Db(clientSession.Ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			clientSession.Ctx,
			`
                SELECT
                    device_adopt.confirmed,
                    network.network_name
                FROM device_adopt
                LEFT JOIN network ON
                    network.network_id = device_adopt.owner_network_id
                INNER JOIN device_association_code ON
                    device_association_code.device_association_id = device_adopt.device_association_id AND
                    device_association_code.code = $1 AND
                    device_association_code.code_type = $2
            `,
			adoptStatus.AdoptCode,
			CodeTypeAdopt,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var confirmed bool
				var networkName *string
				adoptStatusResult = &DeviceAdoptStatusResult{}
				server.Raise(result.Scan(
					&confirmed,
					&networkName,
				))
				adoptStatusResult.Pending = !confirmed
				if networkName != nil {
					adoptStatusResult.AssociatedNetworkName = *networkName
				}
			} else {
				adoptStatusResult = &DeviceAdoptStatusResult{
					Error: &DeviceAdoptStatusError{
						Message: "Invalid adopt code.",
					},
				}
			}
		})
	})

	return
}

type DeviceConfirmAdoptArgs struct {
	AdoptCode             string `json:"adopt_code"`
	AdoptSecret           string `json:"adopt_secret"`
	AssociatedNetworkName string `json:"associated_network_name,omitempty"`
}

type DeviceConfirmAdoptResult struct {
	ByClientJwt string                   `json:"by_client_jwt,omitempty"`
	Error       *DeviceConfirmAdoptError `json:"error,omitempty"`
}

type DeviceConfirmAdoptError struct {
	Message string `json:"message"`
}

func DeviceConfirmAdopt(
	confirmAdopt *DeviceConfirmAdoptArgs,
	clientSession *session.ClientSession,
) (confirmAdoptResult *DeviceConfirmAdoptResult, returnErr error) {
	server.Tx(clientSession.Ctx, func(tx server.PgTx) {
		result, err := tx.Query(
			clientSession.Ctx,
			`
                SELECT
                    device_association_id
                FROM device_association_code
                WHERE
                    code = $1 AND
                    code_type = $2
            `,
			confirmAdopt.AdoptCode,
			CodeTypeAdopt,
		)

		var deviceAssociationId *server.Id

		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&deviceAssociationId))
			}
		})

		if deviceAssociationId == nil {
			return
		}

		adoptTime := server.NowUtc()

		tag := server.RaisePgResult(tx.Exec(
			clientSession.Ctx,
			`
                UPDATE device_adopt
                SET
                    confirmed = true
                FROM network
                WHERE
                    device_association_id = $1 AND
                    network.network_name = $2 AND
                    device_adopt.owner_network_id = network.network_id AND
                    device_adopt.confirmed = false AND
                    $3 < device_adopt.expire_time
            `,
			deviceAssociationId,
			confirmAdopt.AssociatedNetworkName,
			adoptTime,
		))

		if tag.RowsAffected() == 0 {
			return
		}

		result, err = tx.Query(
			clientSession.Ctx,
			`
                SELECT
                    device_adopt.device_name,
                    device_adopt.device_spec,
                    network.network_id,
                    network.network_name,
										network_user.auth_type as admin_auth_type,
                    device_adopt.owner_user_id
                FROM device_adopt
                INNER JOIN network ON
                    network.network_id = device_adopt.owner_network_id
								LEFT JOIN network_user ON
                    network_user.user_id = network.admin_user_id
                WHERE
                    device_adopt.device_association_id = $1
            `,
			deviceAssociationId,
		)

		var deviceName string
		var deviceSpec string
		var networkId server.Id
		var networkName string
		var userId server.Id
		var authType AuthType

		server.WithPgResult(result, err, func() {
			result.Next()
			server.Raise(result.Scan(
				&deviceName,
				&deviceSpec,
				&networkId,
				&networkName,
				&authType,
				&userId,
			))
		})

		result, err = tx.Query(
			clientSession.Ctx,
			`
                SELECT
                    auth_session_id
                FROM device_adopt_auth_session
                WHERE
                    device_association_id = $1
            `,
			deviceAssociationId,
		)

		authSessionIds := []server.Id{}
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var authSessionId server.Id
				server.Raise(result.Scan(&authSessionId))
				authSessionIds = append(authSessionIds, authSessionId)
			}
		})

		authSessionId := server.NewId()
		authSessionIds = append(authSessionIds, authSessionId)

		server.RaisePgResult(tx.Exec(
			clientSession.Ctx,
			`
                INSERT INTO auth_session (
                    auth_session_id,
                    network_id,
                    user_id
                ) VALUES ($1, $2, $3)
            `,
			authSessionId,
			networkId,
			userId,
		))

		deviceId := server.NewId()
		clientId := server.NewId()

		server.RaisePgResult(tx.Exec(
			clientSession.Ctx,
			`
                INSERT INTO device (
                    device_id,
                    network_id,
                    device_name,
                    device_spec,
                    create_time
                )
                VALUES ($1, $2, $3, $4, $5)
            `,
			deviceId,
			networkId,
			deviceName,
			deviceSpec,
			adoptTime,
		))

		server.RaisePgResult(tx.Exec(
			clientSession.Ctx,
			`
                INSERT INTO network_client (
                    client_id,
                    network_id,
                    device_id,
                    description,
                    create_time,
                    auth_time
                )
                VALUES ($1, $2, $3, $4, $5, $5)
            `,
			clientId,
			networkId,
			deviceId,
			deviceName,
			adoptTime,
		))

		isGuestMode := (authType == AuthTypeGuest)

		isPro := IsPro(
			clientSession.Ctx,
			&networkId,
		)

		byJwtWithClientId := jwt.NewByJwt(
			networkId,
			userId,
			networkName,
			isGuestMode,
			isPro,
			authSessionIds...,
		).Client(deviceId, clientId).Sign()

		confirmAdoptResult = &DeviceConfirmAdoptResult{
			// AssociatedNetworkName: confirmAdopt.AssociatedNetworkName,
			ByClientJwt: byJwtWithClientId,
		}
	})

	if confirmAdoptResult == nil && returnErr == nil {
		confirmAdoptResult = &DeviceConfirmAdoptResult{
			Error: &DeviceConfirmAdoptError{
				Message: "Invalid code.",
			},
		}
		return
	}
	return
}

type DeviceRemoveAdoptCodeArgs struct {
	AdoptCode   string `json:"adopt_code"`
	AdoptSecret string `json:"adopt_secret"`
}

type DeviceRemoveAdoptCodeResult struct {
	Error *DeviceRemoveAdoptCodeError `json:"error,omitempty"`
}

type DeviceRemoveAdoptCodeError struct {
	Message string `json:"message"`
}

func DeviceRemoveAdoptCode(
	removeAdoptCode *DeviceRemoveAdoptCodeArgs,
	clientSession *session.ClientSession,
) (removeAdoptCodeResult *DeviceRemoveAdoptCodeResult, returnErr error) {
	server.Tx(clientSession.Ctx, func(tx server.PgTx) {
		result, err := tx.Query(
			clientSession.Ctx,
			`
                SELECT
                    device_association_id
                FROM device_association_code
                WHERE
                    code = $1 AND
                    code_type = $2
            `,
			removeAdoptCode.AdoptCode,
			CodeTypeAdopt,
		)

		var deviceAssociationId *server.Id

		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&deviceAssociationId))
			}
		})

		if deviceAssociationId == nil {
			return
		}

		tag := server.RaisePgResult(tx.Exec(
			clientSession.Ctx,
			`
                DELETE FROM device_adopt
                WHERE
                    device_association_id = $1 AND
                    adopt_secret = $2 AND
                    confirmed = false
            `,
			deviceAssociationId,
			removeAdoptCode.AdoptSecret,
		))
		if tag.RowsAffected() == 0 {
			return
		}

		removeAdoptCodeResult = &DeviceRemoveAdoptCodeResult{}
		return
	})

	if removeAdoptCodeResult == nil && returnErr == nil {
		removeAdoptCodeResult = &DeviceRemoveAdoptCodeResult{
			Error: &DeviceRemoveAdoptCodeError{
				Message: "Invalid code.",
			},
		}
		return
	}

	return
}

type DeviceAssociationsResult struct {
	PendingAdoptionDevices []*DeviceAssociation `json:"pending_adoption_devices"`
	IncomingSharedDevices  []*DeviceAssociation `json:"incoming_shared_devices"`
	OutgoingSharedDevices  []*DeviceAssociation `json:"outgoing_shared_devices"`
}

type DeviceAssociation struct {
	Pending         bool    `json:"pending,omitempty"`
	Code            string  `json:"code,omitempty"`
	DeviceName      string  `json:"device_name,omitempty"`
	ClientId        string  `json:"client_id,omitempty"`
	NetworkName     string  `json:"network_name,omitempty"`
	DurationMinutes float64 `json:"duration_minutes,omitempty"`
}

func DeviceAssociations(
	clientSession *session.ClientSession,
) (associationResult *DeviceAssociationsResult, returnErr error) {
	server.Db(clientSession.Ctx, func(conn server.PgConn) {
		pendingAdoptionDevices := []*DeviceAssociation{}
		incomingSharedDevices := []*DeviceAssociation{}
		outgoingSharedDevices := []*DeviceAssociation{}

		checkTime := server.NowUtc()

		// pending adoption
		result, err := conn.Query(
			clientSession.Ctx,
			`
                SELECT

                    device_association_code.code,
                    COALESCE(device_association_name.device_name, device_adopt.device_name) AS device_name,
                    device_adopt.expire_time

                FROM device_adopt
                INNER JOIN device_association_code ON
                    device_association_code.device_association_id = device_adopt.device_association_id
                LEFT JOIN device_association_name ON
                    device_association_name.device_association_id = device_adopt.device_association_id AND
                    device_association_name.network_id = device_adopt.owner_network_id
                WHERE
                    device_adopt.owner_network_id = $1 AND
                    confirmed = false AND
                    $2 < device_adopt.expire_time
            `,
			clientSession.ByJwt.NetworkId,
			checkTime,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var endTime time.Time
				association := &DeviceAssociation{
					Pending: true,
				}
				server.Raise(result.Scan(
					&association.Code,
					&association.DeviceName,
					&endTime,
				))
				association.DurationMinutes = float64(endTime.Sub(checkTime)) / float64(time.Minute)
				pendingAdoptionDevices = append(pendingAdoptionDevices, association)
			}
		})

		// incoming shared
		result, err = conn.Query(
			clientSession.Ctx,
			`
                SELECT

                    device_share.confirmed,
                    device_association_code.code,
                    COALESCE(device_association_name.device_name, device_share.device_name) AS device_name,
                    device_share.client_id,
                    network.network_name

                FROM device_share
                INNER JOIN device_association_code ON
                    device_association_code.device_association_id = device_share.device_association_id
                LEFT JOIN device_association_name ON
                    device_association_name.device_association_id = device_share.device_association_id AND
                    device_association_name.network_id = device_share.guest_network_id
                LEFT JOIN network ON
                    network.network_id = device_share.source_network_id
                WHERE
                    device_share.guest_network_id = $1
            `,
			clientSession.ByJwt.NetworkId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var confirmed bool
				var networkName *string
				association := &DeviceAssociation{}
				server.Raise(result.Scan(
					&confirmed,
					&association.Code,
					&association.DeviceName,
					&association.ClientId,
					&networkName,
				))
				association.Pending = !confirmed
				if networkName != nil {
					association.NetworkName = *networkName
				}
				incomingSharedDevices = append(incomingSharedDevices, association)
			}
		})

		// outgoing shared
		result, err = conn.Query(
			clientSession.Ctx,
			`
                SELECT

                    device_share.confirmed,
                    device_association_code.code,
                    COALESCE(device_association_name.device_name, device_share.device_name) AS device_name,
                    device_share.client_id,
                    network.network_name

                FROM device_share
                INNER JOIN device_association_code ON
                    device_association_code.device_association_id = device_share.device_association_id
                LEFT JOIN device_association_name ON
                    device_association_name.device_association_id = device_share.device_association_id AND
                    device_association_name.network_id = device_share.source_network_id
                LEFT JOIN network ON
                    network.network_id = device_share.guest_network_id
                WHERE
                    device_share.source_network_id = $1
            `,
			clientSession.ByJwt.NetworkId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var confirmed bool
				var networkName *string
				association := &DeviceAssociation{}
				server.Raise(result.Scan(
					&confirmed,
					&association.Code,
					&association.DeviceName,
					&association.ClientId,
					&networkName,
				))
				association.Pending = !confirmed
				if networkName != nil {
					association.NetworkName = *networkName
				}
				outgoingSharedDevices = append(outgoingSharedDevices, association)
			}
		})

		associationResult = &DeviceAssociationsResult{
			PendingAdoptionDevices: pendingAdoptionDevices,
			IncomingSharedDevices:  incomingSharedDevices,
			OutgoingSharedDevices:  outgoingSharedDevices,
		}
		return
	})

	return
}

type DeviceRemoveAssociationArgs struct {
	Code string `json:"code"`
}

type DeviceRemoveAssociationResult struct {
	Error *DeviceRemoveAssociationError `json:"error,omitempty"`
}

type DeviceRemoveAssociationError struct {
	Message string `json:"message"`
}

func DeviceRemoveAssociation(
	removeAssociation *DeviceRemoveAssociationArgs,
	clientSession *session.ClientSession,
) (removeAssociationResult *DeviceRemoveAssociationResult, returnErr error) {
	server.Tx(clientSession.Ctx, func(tx server.PgTx) {
		result, err := tx.Query(
			clientSession.Ctx,
			`
                SELECT
                    device_association_id,
                    code_type
                FROM device_association_code
                WHERE
                    code = $1
            `,
			removeAssociation.Code,
		)

		var deviceAssociationId *server.Id
		var codeType CodeType

		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(
					&deviceAssociationId,
					&codeType,
				))
			}
		})

		if deviceAssociationId == nil {
			return
		}

		switch codeType {
		case CodeTypeAdopt:
			tag := server.RaisePgResult(tx.Exec(
				clientSession.Ctx,
				`
                    DELETE FROM device_adopt
                    WHERE
                        device_association_id = $1 AND
                        owner_network_id = $2
                `,
				deviceAssociationId,
				clientSession.ByJwt.NetworkId,
			))
			if tag.RowsAffected() == 0 {
				return
			}

			removeAssociationResult = &DeviceRemoveAssociationResult{}
			return
		case CodeTypeShare:
			tag := server.RaisePgResult(tx.Exec(
				clientSession.Ctx,
				`
                    DELETE FROM device_share
                    WHERE
                        device_association_id = $1 AND
                        (source_network_id = $2 OR guest_network_id = $2)
                `,
				deviceAssociationId,
				clientSession.ByJwt.NetworkId,
			))
			if tag.RowsAffected() == 0 {
				return
			}

			removeAssociationResult = &DeviceRemoveAssociationResult{}
			return

		default:
			return
		}
	})

	if removeAssociationResult == nil && returnErr == nil {
		removeAssociationResult = &DeviceRemoveAssociationResult{
			Error: &DeviceRemoveAssociationError{
				Message: "Invalid code.",
			},
		}
	}

	return
}

type DeviceSetAssociationNameArgs struct {
	Code       string `json:"code"`
	DeviceName string `json:"device_name"`
}

type DeviceSetAssociationNameResult struct {
	Error *DeviceSetAssociationNameError `json:"error,omitempty"`
}

type DeviceSetAssociationNameError struct {
	Message string `json:"message"`
}

func DeviceSetAssociationName(
	setAssociationName *DeviceSetAssociationNameArgs,
	clientSession *session.ClientSession,
) (setAssociationNameResult *DeviceSetAssociationNameResult, returnErr error) {
	server.Tx(clientSession.Ctx, func(tx server.PgTx) {
		result, err := tx.Query(
			clientSession.Ctx,
			`
                SELECT
                    device_association_id,
                    code_type
                FROM device_association_code
                WHERE
                    code = $1
            `,
			setAssociationName.Code,
		)

		var deviceAssociationId *server.Id
		var codeType CodeType

		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(
					&deviceAssociationId,
					&codeType,
				))
			}
		})

		if deviceAssociationId == nil {
			return
		}

		switch codeType {
		case CodeTypeAdopt:
			result, err := tx.Query(
				clientSession.Ctx,
				`
                    SELECT
                        owner_network_id
                    FROM device_adopt
                    WHERE
                        device_association_id = $1
                `,
				deviceAssociationId,
			)

			var ownerNetworkId *server.Id

			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&ownerNetworkId))
				}
			})

			if ownerNetworkId != nil && *ownerNetworkId == clientSession.ByJwt.NetworkId {
				server.RaisePgResult(tx.Exec(
					clientSession.Ctx,
					`
                        INSERT INTO device_association_name (
                            device_association_id,
                            network_id,
                            device_name
                        ) VALUES ($1, $2, $3)
                        ON CONFLICT (device_association_id, network_id) DO UPDATE
                        SET
                            device_name = $3
                    `,
					deviceAssociationId,
					clientSession.ByJwt.NetworkId,
					setAssociationName.DeviceName,
				))

				setAssociationNameResult = &DeviceSetAssociationNameResult{}
				return
			} else {
				return
			}

		case CodeTypeShare:
			result, err := tx.Query(
				clientSession.Ctx,
				`
                    SELECT
                        source_network_id,
                        guest_network_id
                    FROM device_share
                    WHERE
                        device_association_id = $1
                `,
				deviceAssociationId,
			)

			var sourceNetworkId *server.Id
			var guestNetworkId *server.Id

			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(
						&sourceNetworkId,
						&guestNetworkId,
					))
				}
			})

			if sourceNetworkId != nil && *sourceNetworkId == clientSession.ByJwt.NetworkId ||
				guestNetworkId != nil && *guestNetworkId == clientSession.ByJwt.NetworkId {
				server.RaisePgResult(tx.Exec(
					clientSession.Ctx,
					`
                        INSERT INTO device_association_name (
                            device_association_id,
                            network_id,
                            device_name
                        ) VALUES ($1, $2, $3)
                        ON CONFLICT (device_association_id, network_id) DO UPDATE
                        SET
                            device_name = $3
                    `,
					deviceAssociationId,
					clientSession.ByJwt.NetworkId,
					setAssociationName.DeviceName,
				))

				setAssociationNameResult = &DeviceSetAssociationNameResult{}
				return
			} else {
				return
			}

		default:
			return
		}
	})

	if setAssociationNameResult == nil && returnErr == nil {
		setAssociationNameResult = &DeviceSetAssociationNameResult{
			Error: &DeviceSetAssociationNameError{
				Message: "Invalid code.",
			},
		}
	}

	return
}

func qrPngBytes(url string) ([]byte, error) {
	q, err := qrcode.New(url, qrcode.Medium)
	if err != nil {
		return nil, err
	}
	q.ForegroundColor = color.RGBA{
		R: 29,
		G: 49,
		B: 80,
		A: 255,
	}
	q.BackgroundColor = color.RGBA{
		R: 255,
		G: 255,
		B: 255,
		A: 255,
	}

	pngBytes, err := q.PNG(256)
	if err != nil {
		return nil, err
	}

	return pngBytes, err
}
