package model

// basic test for each function

import (
	"context"
	"testing"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/server/v2025"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

func TestDeviceAdopt(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		networkIdA := server.NewId()

		userIdA := server.NewId()

		deviceIdA := server.NewId()
		clientIdA := server.NewId()
		guestMode := false

		clientSessionA := session.Testing_CreateClientSession(
			ctx,
			jwt.NewByJwt(networkIdA, userIdA, "a", guestMode),
		)

		clientSessionNoAuth := session.Testing_CreateClientSession(
			ctx,
			nil,
		)

		Testing_CreateNetwork(ctx, networkIdA, "a", userIdA)
		Testing_CreateDevice(ctx, networkIdA, deviceIdA, clientIdA, "devicea", "speca")

		clientsResult0, err := GetNetworkClients(clientSessionA)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(clientsResult0.Clients), 1)

		result1, err := DeviceCreateAdoptCode(
			&DeviceCreateAdoptCodeArgs{
				DeviceName: "devicec",
				DeviceSpec: "specc",
			},
			clientSessionNoAuth,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, result1.Error, nil)
		assert.NotEqual(t, result1.AdoptCode, "")
		assert.NotEqual(t, result1.AdoptSecret, "")

		qrResult0, err := DeviceAdoptCodeQR(
			&DeviceAdoptCodeQRArgs{
				AdoptCode: result1.AdoptCode,
			},
			clientSessionNoAuth,
		)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, qrResult0.PngBytes, nil)

		result2, err := DeviceAdd(
			&DeviceAddArgs{
				Code: result1.AdoptCode,
			},
			clientSessionA,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, result2.Error, nil)

		result3, err := DeviceAdoptStatus(
			&DeviceAdoptStatusArgs{
				AdoptCode: result1.AdoptCode,
			},
			clientSessionNoAuth,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, result3.Error, nil)

		// at this point there should be adopt association
		associationResult0, err := DeviceAssociations(clientSessionA)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(associationResult0.PendingAdoptionDevices), 1)
		assert.Equal(t, len(associationResult0.IncomingSharedDevices), 0)
		assert.Equal(t, len(associationResult0.OutgoingSharedDevices), 0)

		result4, err := DeviceConfirmAdopt(
			&DeviceConfirmAdoptArgs{
				AdoptCode:             result1.AdoptCode,
				AdoptSecret:           result1.AdoptSecret,
				AssociatedNetworkName: result3.AssociatedNetworkName,
			},
			clientSessionNoAuth,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, result4.Error, nil)
		// assert.Equal(t, result4.AssociatedNetworkName, "a")
		assert.NotEqual(t, result4.ByClientJwt, "")
		byJwt, err := jwt.ParseByJwt(result4.ByClientJwt)
		assert.Equal(t, err, nil)
		assert.Equal(t, byJwt.NetworkId, networkIdA)
		assert.Equal(t, byJwt.NetworkName, "a")
		assert.Equal(t, byJwt.UserId, userIdA)
		assert.Equal(t, byJwt.GuestMode, false)

		// at this point there should be no adopt association
		associationResult1, err := DeviceAssociations(clientSessionA)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(associationResult1.PendingAdoptionDevices), 0)
		assert.Equal(t, len(associationResult1.IncomingSharedDevices), 0)
		assert.Equal(t, len(associationResult1.OutgoingSharedDevices), 0)

		clientResult1, err := GetNetworkClients(clientSessionA)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(clientResult1.Clients), 2)
	})
}

func TestDeviceAdoptPartialOfferRemove(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		// add adopt new code
		// remove association from the added account should remove the adopt code

		ctx := context.Background()

		networkIdA := server.NewId()

		userIdA := server.NewId()

		deviceIdA := server.NewId()
		clientIdA := server.NewId()
		guestMode := false

		clientSessionA := session.Testing_CreateClientSession(
			ctx,
			jwt.NewByJwt(networkIdA, userIdA, "a", guestMode),
		)

		clientSessionNoAuth := session.Testing_CreateClientSession(
			ctx,
			nil,
		)

		Testing_CreateNetwork(ctx, networkIdA, "a", userIdA)
		Testing_CreateDevice(ctx, networkIdA, deviceIdA, clientIdA, "devicea", "speca")

		clientsResult0, err := GetNetworkClients(clientSessionA)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(clientsResult0.Clients), 1)

		result1, err := DeviceCreateAdoptCode(
			&DeviceCreateAdoptCodeArgs{
				DeviceName: "devicec",
				DeviceSpec: "specc",
			},
			clientSessionNoAuth,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, result1.Error, nil)
		assert.NotEqual(t, result1.AdoptCode, "")
		assert.NotEqual(t, result1.AdoptSecret, "")

		result2, err := DeviceAdd(
			&DeviceAddArgs{
				Code: result1.AdoptCode,
			},
			clientSessionA,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, result2.Error, nil)

		result3, err := DeviceAdoptStatus(
			&DeviceAdoptStatusArgs{
				AdoptCode: result1.AdoptCode,
			},
			clientSessionNoAuth,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, result3.Error, nil)

		// at this point there should be adopt association
		associationResult0, err := DeviceAssociations(clientSessionA)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(associationResult0.PendingAdoptionDevices), 1)
		assert.Equal(t, len(associationResult0.IncomingSharedDevices), 0)
		assert.Equal(t, len(associationResult0.OutgoingSharedDevices), 0)

		removeResult0, err := DeviceRemoveAdoptCode(
			&DeviceRemoveAdoptCodeArgs{
				AdoptCode:   result1.AdoptCode,
				AdoptSecret: result1.AdoptSecret,
			},
			clientSessionNoAuth,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, removeResult0.Error, nil)

		associationResult1, err := DeviceAssociations(clientSessionA)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(associationResult1.PendingAdoptionDevices), 0)
		assert.Equal(t, len(associationResult1.IncomingSharedDevices), 0)
		assert.Equal(t, len(associationResult1.OutgoingSharedDevices), 0)

	})
}

func TestDeviceAdoptPartialOwnerRemove(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		// remove the adopt code before confirm
		// TODO test remove adopt code after confirm should have no impact

		ctx := context.Background()

		networkIdA := server.NewId()

		userIdA := server.NewId()

		deviceIdA := server.NewId()
		clientIdA := server.NewId()
		guestMode := false

		clientSessionA := session.Testing_CreateClientSession(
			ctx,
			jwt.NewByJwt(networkIdA, userIdA, "a", guestMode),
		)

		clientSessionNoAuth := session.Testing_CreateClientSession(
			ctx,
			nil,
		)

		Testing_CreateNetwork(ctx, networkIdA, "a", userIdA)
		Testing_CreateDevice(ctx, networkIdA, deviceIdA, clientIdA, "devicea", "speca")

		clientsResult0, err := GetNetworkClients(clientSessionA)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(clientsResult0.Clients), 1)

		result1, err := DeviceCreateAdoptCode(
			&DeviceCreateAdoptCodeArgs{
				DeviceName: "devicec",
				DeviceSpec: "specc",
			},
			clientSessionNoAuth,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, result1.Error, nil)
		assert.NotEqual(t, result1.AdoptCode, "")
		assert.NotEqual(t, result1.AdoptSecret, "")

		result2, err := DeviceAdd(
			&DeviceAddArgs{
				Code: result1.AdoptCode,
			},
			clientSessionA,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, result2.Error, nil)

		result3, err := DeviceAdoptStatus(
			&DeviceAdoptStatusArgs{
				AdoptCode: result1.AdoptCode,
			},
			clientSessionNoAuth,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, result3.Error, nil)

		// at this point there should be adopt association
		associationResult0, err := DeviceAssociations(clientSessionA)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(associationResult0.PendingAdoptionDevices), 1)
		assert.Equal(t, len(associationResult0.IncomingSharedDevices), 0)
		assert.Equal(t, len(associationResult0.OutgoingSharedDevices), 0)

		removeResult0, err := DeviceRemoveAssociation(
			&DeviceRemoveAssociationArgs{
				Code: result1.AdoptCode,
			},
			clientSessionA,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, removeResult0.Error, nil)

		associationResult1, err := DeviceAssociations(clientSessionA)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(associationResult1.PendingAdoptionDevices), 0)
		assert.Equal(t, len(associationResult1.IncomingSharedDevices), 0)
		assert.Equal(t, len(associationResult1.OutgoingSharedDevices), 0)

	})
}

func TestDeviceShare(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		networkIdA := server.NewId()
		networkIdB := server.NewId()

		userIdA := server.NewId()
		userIdB := server.NewId()

		deviceIdA := server.NewId()
		clientIdA := server.NewId()

		deviceIdB := server.NewId()
		clientIdB := server.NewId()
		guestMode := false

		clientSessionA := session.Testing_CreateClientSession(
			ctx,
			jwt.NewByJwt(networkIdA, userIdA, "a", guestMode),
		)

		clientSessionB := session.Testing_CreateClientSession(
			ctx,
			jwt.NewByJwt(networkIdB, userIdB, "b", guestMode),
		)

		Testing_CreateNetwork(ctx, networkIdA, "a", userIdA)
		Testing_CreateDevice(ctx, networkIdA, deviceIdA, clientIdA, "devicea", "speca")

		Testing_CreateNetwork(ctx, networkIdB, "b", userIdB)
		Testing_CreateDevice(ctx, networkIdB, deviceIdB, clientIdB, "deviceb", "specb")

		clientsResult0, err := GetNetworkClients(clientSessionA)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(clientsResult0.Clients), 1)

		clientsResult1, err := GetNetworkClients(clientSessionB)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(clientsResult1.Clients), 1)

		result1, err := DeviceCreateShareCode(
			&DeviceCreateShareCodeArgs{
				ClientId:   clientIdA,
				DeviceName: "devicea",
			},
			clientSessionA,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, result1.Error, nil)
		assert.NotEqual(t, result1.ShareCode, "")

		associationResult0, err := DeviceAssociations(clientSessionA)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(associationResult0.PendingAdoptionDevices), 0)
		assert.Equal(t, len(associationResult0.IncomingSharedDevices), 0)
		assert.Equal(t, len(associationResult0.OutgoingSharedDevices), 1)
		assert.Equal(t, associationResult0.OutgoingSharedDevices[0].Pending, true)
		assert.Equal(t, associationResult0.OutgoingSharedDevices[0].NetworkName, "")
		assert.Equal(t, associationResult0.OutgoingSharedDevices[0].DeviceName, "devicea")

		shareStatus1, err := DeviceShareStatus(
			&DeviceShareStatusArgs{
				ShareCode: result1.ShareCode,
			},
			clientSessionA,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, shareStatus1.Error, nil)

		qrResult0, err := DeviceShareCodeQR(
			&DeviceShareCodeQRArgs{
				ShareCode: result1.ShareCode,
			},
			clientSessionA,
		)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, qrResult0.PngBytes, nil)

		result2, err := DeviceAdd(
			&DeviceAddArgs{
				Code: result1.ShareCode,
			},
			clientSessionB,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, result2.Error, nil)

		result3, err := DeviceShareStatus(
			&DeviceShareStatusArgs{
				ShareCode: result1.ShareCode,
			},
			clientSessionA,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, result3.Error, nil)

		// at this point there should be share association pending

		associationResult1, err := DeviceAssociations(clientSessionA)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(associationResult1.PendingAdoptionDevices), 0)
		assert.Equal(t, len(associationResult1.IncomingSharedDevices), 0)
		assert.Equal(t, len(associationResult1.OutgoingSharedDevices), 1)
		assert.Equal(t, associationResult1.OutgoingSharedDevices[0].Pending, true)
		assert.Equal(t, associationResult1.OutgoingSharedDevices[0].NetworkName, "b")
		assert.Equal(t, associationResult1.OutgoingSharedDevices[0].DeviceName, "devicea")

		associationResult2, err := DeviceAssociations(clientSessionB)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(associationResult2.PendingAdoptionDevices), 0)
		assert.Equal(t, len(associationResult2.IncomingSharedDevices), 1)
		assert.Equal(t, len(associationResult2.OutgoingSharedDevices), 0)
		assert.Equal(t, associationResult2.IncomingSharedDevices[0].Pending, true)
		assert.Equal(t, associationResult2.IncomingSharedDevices[0].NetworkName, "a")
		assert.Equal(t, associationResult2.IncomingSharedDevices[0].DeviceName, "devicea")

		result4, err := DeviceConfirmShare(
			&DeviceConfirmShareArgs{
				ShareCode:             result1.ShareCode,
				AssociatedNetworkName: result3.AssociatedNetworkName,
			},
			clientSessionA,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, result4.Error, nil)
		assert.Equal(t, result4.AssociatedNetworkName, "b")

		// at this point there should be a share association not pending, as outgoing for A, incoming for B
		associationResult3, err := DeviceAssociations(clientSessionA)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(associationResult3.PendingAdoptionDevices), 0)
		assert.Equal(t, len(associationResult3.IncomingSharedDevices), 0)
		assert.Equal(t, len(associationResult3.OutgoingSharedDevices), 1)
		assert.Equal(t, associationResult3.OutgoingSharedDevices[0].Pending, false)
		assert.Equal(t, associationResult3.OutgoingSharedDevices[0].NetworkName, "b")
		assert.Equal(t, associationResult3.OutgoingSharedDevices[0].DeviceName, "devicea")

		associationResult4, err := DeviceAssociations(clientSessionB)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(associationResult4.PendingAdoptionDevices), 0)
		assert.Equal(t, len(associationResult4.IncomingSharedDevices), 1)
		assert.Equal(t, len(associationResult4.OutgoingSharedDevices), 0)
		assert.Equal(t, associationResult4.IncomingSharedDevices[0].Pending, false)
		assert.Equal(t, associationResult4.IncomingSharedDevices[0].NetworkName, "a")
		assert.Equal(t, associationResult4.IncomingSharedDevices[0].DeviceName, "devicea")

		setNameResult1, err := DeviceSetAssociationName(
			&DeviceSetAssociationNameArgs{
				Code:       result1.ShareCode,
				DeviceName: "That device I shared",
			},
			clientSessionA,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, setNameResult1.Error, nil)

		setNameResult2, err := DeviceSetAssociationName(
			&DeviceSetAssociationNameArgs{
				Code:       result1.ShareCode,
				DeviceName: "My new device from friend",
			},
			clientSessionB,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, setNameResult2.Error, nil)

		associationResult5, err := DeviceAssociations(clientSessionA)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(associationResult5.PendingAdoptionDevices), 0)
		assert.Equal(t, len(associationResult5.IncomingSharedDevices), 0)
		assert.Equal(t, len(associationResult5.OutgoingSharedDevices), 1)
		assert.Equal(t, associationResult5.OutgoingSharedDevices[0].Pending, false)
		assert.Equal(t, associationResult5.OutgoingSharedDevices[0].NetworkName, "b")
		assert.Equal(t, associationResult5.OutgoingSharedDevices[0].DeviceName, "That device I shared")

		associationResult6, err := DeviceAssociations(clientSessionB)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(associationResult6.PendingAdoptionDevices), 0)
		assert.Equal(t, len(associationResult6.IncomingSharedDevices), 1)
		assert.Equal(t, len(associationResult6.OutgoingSharedDevices), 0)
		assert.Equal(t, associationResult6.IncomingSharedDevices[0].Pending, false)
		assert.Equal(t, associationResult6.IncomingSharedDevices[0].NetworkName, "a")
		assert.Equal(t, associationResult6.IncomingSharedDevices[0].DeviceName, "My new device from friend")

		clientsResult2, err := GetNetworkClients(clientSessionA)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(clientsResult2.Clients), 1)

		clientsResult3, err := GetNetworkClients(clientSessionB)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(clientsResult3.Clients), 1)
	})
}
