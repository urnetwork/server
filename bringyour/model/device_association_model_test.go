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

	// clientSessionB := session.Testing_CreateClientSession(
	// 	ctx,
	// 	jwt.NewByJwt(networkIdB, userIdB, "b"),
	// )

	

	Testing_CreateNetwork(ctx, networkIdA, "a", userIdA)
	Testing_CreateDevice(ctx, networkIdA, deviceIdA, clientIdA, "devicea", "speca")

	Testing_CreateNetwork(ctx, networkIdB, "b", userIdB)
	Testing_CreateDevice(ctx, networkIdB, deviceIdB, clientIdB, "deviceb", "specb")


	result1, err := DeviceCreateAdoptCode(
		&DeviceCreateAdoptCodeArgs{
			DeviceName: "devicec",
			DeviceSpec: "specc",
		},
		clientSessionA,
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
		clientSessionA,
	)
	assert.Equal(t, err, nil)
	assert.Equal(t, result3.Error, nil)

	result4, err := DeviceConfirmAdopt(
		&DeviceConfirmAdoptArgs{
			AdoptCode: result1.AdoptCode,
			AdoptSecret: result1.AdoptSecret,
			AssociatedNetworkName: result3.AssociatedNetworkName,
		},
		clientSessionA,
	)
	assert.Equal(t, err, nil)
	assert.Equal(t, result4.Error, nil)
	assert.Equal(t, result4.AssociatedNetworkName, "a")
	assert.NotEqual(t, result4.ByClientJwt, "")
	assert.Equal(t, result4.Complete, true)
})}


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
	assert.Equal(t, result4.Complete, true)
})}


/*
func TestDeviceShareCodeQR(t *testing.T) { bringyour.DefaultTestEnv().Run(func() {


})}


func TestDeviceAdoptCodeQR(t *testing.T) { bringyour.DefaultTestEnv().Run(func() {


})}


func TestDeviceRemoveAdoptCode(t *testing.T) { bringyour.DefaultTestEnv().Run(func() {


})}


func TestDeviceAssociations(t *testing.T) { bringyour.DefaultTestEnv().Run(func() {


})}


func TestDeviceRemoveAssociation(t *testing.T) { bringyour.DefaultTestEnv().Run(func() {


})}


func TestDeviceSetAssociationName(t *testing.T) { bringyour.DefaultTestEnv().Run(func() {


})}
*/
