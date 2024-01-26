package model

// basic test for each function

import (
	"context"
    "testing"

    "github.com/go-playground/assert/v2"

    "bringyour.com/bringyour/jwt"
    "bringyour.com/bringyour/session"
    "bringyour.com/bringyour"
)


func TestDeviceAdopt(t *testing.T) { bringyour.DefaultTestEnv().Run(func() {
	ctx := context.Background()

	networkIdA := bringyour.NewId()

	userIdA := bringyour.NewId()

	deviceIdA := bringyour.NewId()
	clientIdA := bringyour.NewId()

	clientSessionA := session.Testing_CreateClientSession(
		ctx,
		jwt.NewByJwt(networkIdA, userIdA, "a"),
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

	result4, err := DeviceConfirmAdopt(
		&DeviceConfirmAdoptArgs{
			AdoptCode: result1.AdoptCode,
			AdoptSecret: result1.AdoptSecret,
			AssociatedNetworkName: result3.AssociatedNetworkName,
		},
		clientSessionNoAuth,
	)
	assert.Equal(t, err, nil)
	assert.Equal(t, result4.Error, nil)
	assert.Equal(t, result4.AssociatedNetworkName, "a")
	assert.NotEqual(t, result4.ByClientJwt, "")
	byJwt, err := jwt.ParseByJwt(result4.ByClientJwt)
	assert.Equal(t, err, nil)
	assert.Equal(t, byJwt.NetworkId, networkIdA)
	assert.Equal(t, byJwt.NetworkName, "a")
	assert.Equal(t, byJwt.UserId, userIdA)


	// at this point there should be no adopt association
	associationResult1, err := DeviceAssociations(clientSessionA)
	assert.Equal(t, err, nil)
	assert.Equal(t, len(associationResult1.PendingAdoptionDevices), 0)


	clientResult1, err := GetNetworkClients(clientSessionA)
	assert.Equal(t, err, nil)
	assert.Equal(t, len(clientResult1.Clients), 2)
})}


// FIXME TestDeviceAdoptPartial
// first, remove the adopt code before confirm
// remove adopt code after confirm should have no impact
// second, add adopt new code
// remove association from the added account should remove the adopt code



func TestDeviceShare(t *testing.T) { bringyour.DefaultTestEnv().Run(func() {
	ctx := context.Background()

	networkIdA := bringyour.NewId()
	networkIdB := bringyour.NewId()

	userIdA := bringyour.NewId()
	userIdB := bringyour.NewId()

	deviceIdA := bringyour.NewId()
	clientIdA := bringyour.NewId()

	deviceIdB := bringyour.NewId()
	clientIdB := bringyour.NewId()

	clientSessionA := session.Testing_CreateClientSession(
		ctx,
		jwt.NewByJwt(networkIdA, userIdA, "a"),
	)

	clientSessionB := session.Testing_CreateClientSession(
		ctx,
		jwt.NewByJwt(networkIdB, userIdB, "b"),
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
			ClientId: clientIdA,
			DeviceName: "devicea",
		},
		clientSessionA,
	)
	assert.Equal(t, err, nil)
	assert.Equal(t, result1.Error, nil)
	assert.NotEqual(t, result1.ShareCode, "")


	qrResult0, err := DeviceShareCodeQR(
		&DeviceAdoptCodeQRArgs{
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

	// FIXME at this point there should be a share association pending

	result4, err := DeviceConfirmShare(
		&DeviceConfirmShareArgs{
			ShareCode: result1.ShareCode,
			AssociatedNetworkName: result3.AssociatedNetworkName,
		},
		clientSessionA,
	)
	assert.Equal(t, err, nil)
	assert.Equal(t, result4.Error, nil)
	assert.Equal(t, result4.AssociatedNetworkName, "b")

	// FIXME at this point there should be a share association not pending, as outgoing for A, incoming for B

	// FIXME set association name

	// FIXME remove association




	clientsResult2, err := GetNetworkClients(clientSessionA)
	assert.Equal(t, err, nil)
	assert.Equal(t, len(clientsResult2.Clients), 1)

	clientsResult3, err := GetNetworkClients(clientSessionB)
	assert.Equal(t, err, nil)
	assert.Equal(t, len(clientsResult3.Clients), 1)
})}


/*

func TestDeviceRemoveAdoptCode(t *testing.T) { bringyour.DefaultTestEnv().Run(func() {


})}


func TestDeviceRemoveAssociation(t *testing.T) { bringyour.DefaultTestEnv().Run(func() {


})}


func TestDeviceSetAssociationName(t *testing.T) { bringyour.DefaultTestEnv().Run(func() {


})}
*/
